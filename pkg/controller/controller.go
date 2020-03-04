package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	api "eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/api/generated/v1"
	accrd "eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/api/v1/availablecapacitycrd"
	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/api/v1/volumecrd"
	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/pkg/base"

	"github.com/sirupsen/logrus"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	coreV1 "k8s.io/api/core/v1"
	k8sError "k8s.io/apimachinery/pkg/api/errors"
	apisV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sClient "sigs.k8s.io/controller-runtime/pkg/client"
)

type NodeID string

// CtxKey variable type uses for keys in context WithValue
type CtxKey string

const (
	NodeSvcPodsMask           = "baremetal-csi-node"
	NodeIDTopologyKey         = "baremetal-csi/nodeid"
	VolumeStatusAnnotationKey = "dell.emc.csi/volume-status"

	RequestUUID                     CtxKey = "RequestUUID"
	DefaultVolumeID                        = "Undefined ID"
	CreateLocalVolumeRequestTimeout        = 300 * time.Second
)

// interface implementation for ControllerServer
type CSIControllerService struct {
	namespace     string
	communicators map[NodeID]api.VolumeManagerClient
	//mutex for csi request
	reqMu sync.Mutex
	log   *logrus.Entry
	//mutex for request to CR
	crMu sync.Mutex

	k8sClient.Client
}

func NewControllerService(k8sClient k8sClient.Client, logger *logrus.Logger, namespace string) *CSIControllerService {
	c := &CSIControllerService{
		namespace:     namespace,
		Client:        k8sClient,
		communicators: make(map[NodeID]api.VolumeManagerClient),
	}
	c.log = logger.WithField("component", "CSIControllerService")
	return c
}

func (c *CSIControllerService) InitController() error {
	ll := c.log.WithField("method", "InitController")

	ll.Info("Initialize communicators ...")
	if err := c.updateCommunicators(); err != nil {
		return fmt.Errorf("unable to initialize communicators for node services: %v", err)
	}

	timeout := 240 * time.Second
	ll.Infof("Initialize available capacity with timeout in %s", timeout)
	ctx, cancelFn := context.WithTimeout(context.Background(), timeout)
	defer cancelFn()

	if err := c.updateAvailableCapacityCRs(ctx); err != nil {
		return fmt.Errorf("unable to initialize available capacity: %v", err)
	}

	return nil
}

func (c *CSIControllerService) updateAvailableCapacityCRs(ctx context.Context) error {
	ll := c.log.WithFields(logrus.Fields{
		"method": "updateAvailableCapacityCRs",
	})
	wasError := false
	for nodeID, mgr := range c.communicators {
		response, err := mgr.GetAvailableCapacity(ctx, &api.AvailableCapacityRequest{NodeId: string(nodeID)})
		if err != nil {
			ll.Errorf("Error during GetAvailableCapacity request to node %s: %v", nodeID, err)
			wasError = true
		}
		availableCapacity := response.GetAvailableCapacity()
		ll.Info("Current available capacity is: ", availableCapacity)
		for _, ac := range availableCapacity {
			//name of available capacity cr is node id + drive location
			name := ac.NodeId + "-" + strings.ToLower(ac.Location)
			if err := c.ReadCR(context.WithValue(ctx, RequestUUID, name), name, &accrd.AvailableCapacity{}); err != nil {
				if k8sError.IsNotFound(err) {
					newAC := c.constructAvailableCapacityCR(name, ac)
					if err := c.CreateCR(context.WithValue(ctx, RequestUUID, name), newAC, name); err != nil {
						ll.Errorf("Error during CreateAvailableCapacity request to k8s: %v, error: %v", ac, err)
						wasError = true
					}
				} else {
					ll.Errorf("Unable to read Available Capacity %s, error: %v", name, err)
					wasError = true
				}
			} else {
				ll.Infof("Available Capacity %s already exist", name)
			}
		}
	}

	if wasError {
		return errors.New("not all available capacity were created")
	}
	return nil
}

// TODO: update communicators and available capacity in background AK8S-174
func (c *CSIControllerService) updateCommunicators() error {
	ll := c.log.WithField("method", "updateCommunicators")
	pods, err := c.getPods(context.Background(), NodeSvcPodsMask)
	if err != nil {
		return err
	}

	ll.Infof("Found %d pods with node service", len(pods))

	for _, pod := range pods {
		endpoint := fmt.Sprintf("tcp://%s:%d", pod.Status.PodIP, base.DefaultVolumeManagerPort)
		client, err := base.NewClient(nil, endpoint, c.log.Logger)
		if err != nil {
			c.log.Errorf("Unable to initialize gRPC client for communicating with pod %s, error: %v",
				pod.Name, err)
			continue
		}
		c.communicators[NodeID(pod.Spec.NodeName)] = api.NewVolumeManagerClient(client.GRPCClient)
		ll.Infof("Add communicator for node %s on endpoint %s", pod.Spec.NodeName, endpoint)
	}

	if len(c.communicators) == 0 {
		return errors.New("unable to initialize communicators")
	}

	return nil
}

func (c *CSIControllerService) CreateVolume(ctx context.Context,
	req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	ll := c.log.WithFields(logrus.Fields{
		"method":   "CreateVolume",
		"volumeID": req.GetName(),
	})
	ll.Infof("Processing request: %v", req)

	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume name missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	var (
		reqName   = req.GetName()
		ctxWithID = context.WithValue(ctx, RequestUUID, req.GetName())
		volumeCR  = &volumecrd.Volume{}
		err       error
	)
	// check whether volume CR exist or no
	err = c.ReadCR(ctx, reqName, volumeCR)
	switch {
	case err == nil:
		ll.Infof("Volume exists, current status: %s", api.OperationalStatus_name[int32(volumeCR.Spec.Status)])
	case !k8sError.IsNotFound(err):
		ll.Errorf("unable to read volume CR: %v", err)
		return nil, status.Error(codes.Aborted, "unable to check volume existence")
	default:
		// create volume
		c.reqMu.Lock()
		var (
			ac            *accrd.AvailableCapacity
			requiredBytes = req.GetCapacityRange().GetRequiredBytes()
			preferredNode = ""
		)
		if req.GetAccessibilityRequirements() != nil {
			preferredNode = req.GetAccessibilityRequirements().Preferred[0].Segments[NodeIDTopologyKey]
			ll.Infof("Preferred node was provided: %s", preferredNode)
		}

		if ac = c.searchAvailableCapacity(preferredNode, requiredBytes); ac == nil {
			c.reqMu.Unlock()
			ll.Info("There is no suitable drive for volume")
			return nil, status.Errorf(codes.ResourceExhausted, "there is no suitable drive for request %s", req.GetName())
		}
		ll.Infof("Disk with S/N %s on node %s was selected.", ac.Spec.Location, ac.Spec.NodeId)

		// create volume CR
		volumeCR = &volumecrd.Volume{
			TypeMeta: apisV1.TypeMeta{
				Kind:       "Volume",
				APIVersion: "volume.dell.com/v1",
			},
			ObjectMeta: apisV1.ObjectMeta{
				Name:      reqName,
				Namespace: "default",
				Annotations: map[string]string{
					VolumeStatusAnnotationKey: api.OperationalStatus_name[int32(api.OperationalStatus_Creating)],
				},
			},
			Spec: api.Volume{
				Id:       reqName,
				Owner:    ac.Spec.NodeId,
				Size:     ac.Spec.Size,
				Location: ac.Spec.Location,
				Status:   api.OperationalStatus_Creating,
			},
		}

		if err = c.CreateCR(ctxWithID, volumeCR, reqName); err != nil {
			ll.Errorf("Unable to create CR, error: %v", err)
			c.reqMu.Unlock()
			return nil, status.Errorf(codes.Internal, "unable to create volume CR")
		}

		// delete Available Capacity CR
		if err = c.DeleteCR(ctxWithID, ac); err != nil {
			ll.Errorf("Unable to delete Available Capacity CR, error: %v", err)
		}
		c.reqMu.Unlock()

		// create volume on the remove node
		go c.createLocalVolume(req, ac)
	}

	ll.Info("Waiting unit volume will reach Created status")
	reached, st := c.waitVCRStatus(ctx, req.GetName(),
		api.OperationalStatus_Created, api.OperationalStatus_FailedToCreate)

	if reached {
		if st == api.OperationalStatus_FailedToCreate {
			// this is should be a non-retryable error
			return nil, status.Error(codes.Internal, "Unable to create volume on local node.")
		}

		ll.Infof("Construct response with owner: %s, size: %d", volumeCR.Spec.Owner, volumeCR.Spec.Size)

		topologyList := []*csi.Topology{
			{Segments: map[string]string{NodeIDTopologyKey: volumeCR.Spec.Owner}},
		}

		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:           req.GetName(),
				CapacityBytes:      volumeCR.Spec.Size,
				VolumeContext:      req.GetParameters(),
				AccessibleTopology: topologyList,
			},
		}, nil
	}

	return nil, status.Errorf(codes.Aborted, "CreateVolume is in progress")
}

// createLocalVolume sends request to the local node and based on response sets volume status (update CR)
func (c *CSIControllerService) createLocalVolume(req *csi.CreateVolumeRequest, ac *accrd.AvailableCapacity) {
	ll := c.log.WithFields(logrus.Fields{
		"method":   "createLocalVolume",
		"volumeID": req.GetName(),
	})

	var (
		clvReq = &api.CreateLocalVolumeRequest{
			PvcUUID:  req.GetName(),
			Capacity: req.GetCapacityRange().GetRequiredBytes(),
			Sc:       "hdd",
			Location: ac.Spec.Location,
		}
		node = ac.Spec.NodeId
	)

	ll.Infof("RPC on node %s with timeout in %.2f seconds. Request: %v", node,
		CreateLocalVolumeRequestTimeout.Seconds(), clvReq)

	ctxT, cancelFn := context.WithTimeout(context.Background(), CreateLocalVolumeRequestTimeout)
	resp, err := c.communicators[NodeID(node)].CreateLocalVolume(ctxT, clvReq)
	cancelFn()
	ll.Infof("Got response: %v", resp)

	var newStatus api.OperationalStatus
	if err != nil {
		ll.Errorf("Unable to create volume size of %d bytes. Error: %v. Context Error: %v. Set volume status to FailedToCreate",
			clvReq.Capacity, err, ctxT.Err())
		newStatus = api.OperationalStatus_FailedToCreate
	} else {
		ll.Infof("CreateLocalVolume returned response: %v. Set status to Created", resp)
		newStatus = api.OperationalStatus_Created
	}

	if err = c.changeVolumeStatus(clvReq.PvcUUID, newStatus); err != nil {
		ll.Error(err.Error())
	}
}

// waitVCRStatus check volume status until it will be reached one of the statuses
// return true if one of the status had reached, or return false instead
// also return status that had reached or -1
func (c *CSIControllerService) waitVCRStatus(ctx context.Context,
	volumeID string,
	statuses ...api.OperationalStatus) (bool, api.OperationalStatus) {
	ll := c.log.WithFields(logrus.Fields{
		"method":   "waitVCRStatus",
		"volumeID": volumeID,
	})
	ll.Infof("Pulling volume status")

	var (
		v   = &volumecrd.Volume{}
		err error
	)

	for {
		select {
		case <-ctx.Done():
			ll.Warnf("Context is done but volume still not become in expected state")
			return false, -1
		case <-time.After(time.Second):
			if err = c.ReadCR(context.WithValue(ctx, RequestUUID, volumeID), volumeID, v); err != nil {
				ll.Errorf("Unable to read volume CR and check status: %v", err)
				continue
			}
			for _, s := range statuses {
				if v.Spec.Status == s {
					ll.Infof("Volume has reached %s state.", api.OperationalStatus_name[int32(s)])
					return true, s
				}
			}
		}
	}
}

// changeVolumeStatus sets volume status with reqMu.Lock(): read Volume, change status, update volume
func (c *CSIControllerService) changeVolumeStatus(volumeID string, newStatus api.OperationalStatus) error {
	ll := c.log.WithFields(logrus.Fields{
		"method":   "createVolumeOnNode",
		"volumeID": volumeID,
	})

	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	var (
		err          error
		newStatusStr = api.OperationalStatus_name[int32(newStatus)]
		v            = &volumecrd.Volume{}
		attempts     = 10
		timeout      = 500 * time.Millisecond
		ctxV         = context.WithValue(context.Background(), RequestUUID, volumeID)
	)
	ll.Infof("Try to set status to %s", newStatusStr)

	// read volume into v
	for i := 0; i < attempts; i++ {
		if err = c.ReadCR(ctxV, volumeID, v); err == nil {
			break
		}
		ll.Warnf("Unable to read CR: %v. Attempt %d out of %d.", err, i, attempts)
		time.Sleep(timeout)
	}

	// change status
	v.Spec.Status = newStatus
	if v.ObjectMeta.Annotations == nil {
		v.ObjectMeta.Annotations = make(map[string]string, 1)
	}
	v.ObjectMeta.Annotations[VolumeStatusAnnotationKey] = newStatusStr

	for i := 0; i < attempts; i++ {
		// update volume with new status
		if err = c.UpdateCR(ctxV, v); err == nil {
			return nil
		}
		ll.Warnf("Unable to update volume CR (set status to %s). Attempt %d out of %d",
			api.OperationalStatus_name[int32(newStatus)], i, attempts)
		time.Sleep(timeout)
	}

	return fmt.Errorf("unable to persist status to %s for volume %s", newStatusStr, volumeID)
}

// searchAvailableCapacity search appropriate available capacity and remove it from cache
func (c *CSIControllerService) searchAvailableCapacity(preferredNode string, requiredBytes int64) *accrd.AvailableCapacity {
	ll := c.log.WithFields(logrus.Fields{
		"method":        "searchAvailableCapacity",
		"requiredBytes": fmt.Sprintf("%.3fG", float64(requiredBytes)/float64(base.GBYTE)),
	})

	ll.Info("Search appropriate available ac")

	var (
		allocatedCapacity int64 = math.MaxInt64
		foundAC           *accrd.AvailableCapacity
		acList            = &accrd.AvailableCapacityList{}
		acNodeMap         map[string][]*accrd.AvailableCapacity
		maxLen            = 0
	)

	err := c.ReadList(context.Background(), acList)
	if err != nil {
		ll.Errorf("Unable to read Available Capacity list, error: %v", err)
		return nil // it does mean a non-retryable error
	}
	acNodeMap = c.acNodeMapping(acList.Items)

	if preferredNode == "" {
		for nodeID, acs := range acNodeMap {
			if len(acs) > maxLen {
				// TODO: what if node doesn't have AC size of requiredBytes
				preferredNode = nodeID
				maxLen = len(acs)
			}
		}
	}

	ll.Infof("Node %s was selected, search drive size of %d on it", preferredNode, requiredBytes)

	for _, ac := range acNodeMap[preferredNode] {
		if ac.Spec.Size < allocatedCapacity && ac.Spec.Size >= requiredBytes {
			foundAC = ac
			allocatedCapacity = ac.Spec.Size
		}
	}
	return foundAC
}

// acNodeMapping constructs map with key - nodeID(hostname), value - AC instance
func (c *CSIControllerService) acNodeMapping(acs []accrd.AvailableCapacity) map[string][]*accrd.AvailableCapacity {
	var (
		acNodeMap = make(map[string][]*accrd.AvailableCapacity)
		node      string
	)

	for _, ac := range acs {
		node = ac.Spec.NodeId
		if _, ok := acNodeMap[node]; !ok {
			acNodeMap[node] = make([]*accrd.AvailableCapacity, 0)
		}
		acTmp := ac
		acNodeMap[node] = append(acNodeMap[node], &acTmp)
	}
	return acNodeMap
}

func (c *CSIControllerService) DeleteVolume(ctx context.Context,
	req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	ll := c.log.WithFields(logrus.Fields{
		"method":   "DeleteVolume",
		"volumeID": req.GetVolumeId(),
	})

	ll.Infof("Processing request: %v", req)

	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID must be provided")
	}

	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	var (
		volume         = &volumecrd.Volume{}
		ctxT, cancelFn = context.WithTimeout(context.WithValue(ctx, RequestUUID, req.GetVolumeId()),
			CreateLocalVolumeRequestTimeout)
	)
	defer cancelFn()

	if err := c.ReadCR(ctxT, req.VolumeId, volume); err != nil {
		if k8sError.IsNotFound(err) {
			ll.Infof("Volume CR doesn't exist, volume had removed")
			return &csi.DeleteVolumeResponse{}, nil
		}
		ll.Errorf("Unable to read volume: %v", err)
		return nil, fmt.Errorf("unable to find volume with ID %s", req.VolumeId)
	}

	node := volume.Spec.Owner //volume.NodeID

	ll.Infof("RPC on node %s with", node)
	resp, err := c.communicators[NodeID(node)].DeleteLocalVolume(ctxT, &api.DeleteLocalVolumeRequest{
		PvcUUID: req.VolumeId,
	})

	if err != nil {
		ll.Errorf("failed to delete local volume with %s", err)
		return nil, status.Errorf(codes.Internal, "unable to delete volume on node %s", node)
	}

	if !resp.Ok {
		return nil, status.Error(codes.Internal, "response for delete local volume is not ok")
	}

	ll.Info("DeleteLocalVolume return Ok")
	localVolume := resp.GetVolume()
	if localVolume == nil {
		return nil, status.Error(codes.Internal, "Unable to delete volume from node")
	}

	// remove volume CR
	if err = c.DeleteCR(ctxT, volume); err != nil {
		ll.Errorf("Delete volume CR with name %s failed, error: %v", req.VolumeId, err)
		return nil, status.Errorf(codes.Internal, "can't delete volume CR: %s", err.Error())
	}

	ac := &api.AvailableCapacity{
		Size:     localVolume.Size,
		Type:     api.StorageClass_ANY,
		Location: localVolume.Location,
		NodeId:   node,
	}

	location := strings.ToLower(localVolume.Location)
	name := node + "-" + location

	cr := c.constructAvailableCapacityCR(name, ac)
	if err := c.CreateCR(ctxT, cr, name); err != nil {
		ll.Errorf("Can't create AvailableCapacity CR %v error: %v", cr, err)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (c *CSIControllerService) ControllerPublishVolume(ctx context.Context,
	req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	c.log.WithFields(logrus.Fields{
		"method":   "ControllerPublishVolume",
		"volumeID": req.GetVolumeId(),
	}).Info("Return empty response, ok.")

	return &csi.ControllerPublishVolumeResponse{}, nil
}

func (c *CSIControllerService) ControllerUnpublishVolume(ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	c.log.WithFields(logrus.Fields{
		"method":   "ControllerUnpublishVolume",
		"volumeID": req.GetVolumeId(),
	}).Info("Return empty response, ok")

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (c *CSIControllerService) ValidateVolumeCapabilities(context.Context, *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented yet")
}

func (c *CSIControllerService) ListVolumes(context.Context, *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented yet")
}

func (c *CSIControllerService) GetCapacity(context.Context, *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented yet")
}

func (c *CSIControllerService) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	ll := c.log.WithFields(logrus.Fields{
		"method": "ControllerGetCapabilities",
	})

	newCap := func(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: cap,
				},
			},
		}
	}

	caps := make([]*csi.ControllerServiceCapability, 0)
	for _, c := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	} {
		caps = append(caps, newCap(c))
	}

	resp := &csi.ControllerGetCapabilitiesResponse{
		Capabilities: caps,
	}

	ll.Infof("ControllerGetCapabilities returns response: %v", resp)

	return resp, nil
}

func (c *CSIControllerService) CreateSnapshot(context.Context, *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented yet")
}

func (c *CSIControllerService) DeleteSnapshot(context.Context, *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented yet")
}

func (c *CSIControllerService) ListSnapshots(context.Context, *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented yet")
}

func (c *CSIControllerService) ControllerExpandVolume(context.Context, *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented yet")
}

func (c *CSIControllerService) constructAvailableCapacityCR(name string, ac *api.AvailableCapacity) *accrd.AvailableCapacity {
	return &accrd.AvailableCapacity{
		TypeMeta: apisV1.TypeMeta{
			Kind:       "AvailableCapacity",
			APIVersion: "availablecapacity.dell.com/v1",
		},
		ObjectMeta: apisV1.ObjectMeta{
			Name:      name,
			Namespace: c.namespace,
		},
		Spec: *ac,
	}
}

func (c *CSIControllerService) CreateCR(ctx context.Context, obj runtime.Object, name string) error {
	c.crMu.Lock()
	defer c.crMu.Unlock()

	volumeID := ctx.Value(RequestUUID)
	if volumeID == nil {
		volumeID = DefaultVolumeID
	}

	ll := c.log.WithFields(logrus.Fields{
		"method":      "CreateCR",
		"requestUUID": volumeID.(string),
	})
	ll.Infof("Creating CR %s with name %s", obj.GetObjectKind().GroupVersionKind().Kind, name)

	err := c.Get(ctx, k8sClient.ObjectKey{Name: name, Namespace: c.namespace}, obj)
	if err != nil {
		if k8sError.IsNotFound(err) {
			e := c.Create(ctx, obj)
			if e != nil {
				return e
			}
		} else {
			return err
		}
	}
	ll.Infof("CR with name %s was created successfully", name)
	return nil
}

func (c *CSIControllerService) ReadCR(ctx context.Context, name string, obj runtime.Object) error {
	c.crMu.Lock()
	defer c.crMu.Unlock()

	return c.Get(ctx, k8sClient.ObjectKey{Name: name, Namespace: c.namespace}, obj)
}

func (c *CSIControllerService) ReadList(ctx context.Context, object runtime.Object) error {
	c.crMu.Lock()
	defer c.crMu.Unlock()
	c.log.WithField("method", "ReadList").Info("Reading list")

	return c.List(ctx, object, k8sClient.InNamespace(c.namespace))
}

func (c *CSIControllerService) UpdateCR(ctx context.Context, obj runtime.Object) error {
	c.crMu.Lock()
	defer c.crMu.Unlock()

	volumeID := ctx.Value(RequestUUID)
	if volumeID == nil {
		volumeID = DefaultVolumeID
	}

	c.log.WithFields(logrus.Fields{
		"method":   "UpdateCR",
		"volumeID": volumeID.(string),
	}).Infof("Updating CR %s", obj.GetObjectKind().GroupVersionKind().Kind)

	return c.Update(ctx, obj)
}

func (c *CSIControllerService) DeleteCR(ctx context.Context, obj runtime.Object) error {
	c.crMu.Lock()
	defer c.crMu.Unlock()

	volumeID := ctx.Value(RequestUUID)
	if volumeID == nil {
		volumeID = DefaultVolumeID
	}

	c.log.WithFields(logrus.Fields{
		"method":   "DeleteCR",
		"volumeID": volumeID.(string),
	}).Infof("Deleting CR %s", obj.GetObjectKind().GroupVersionKind().Kind)

	return c.Delete(ctx, obj)
}

func (c *CSIControllerService) getPods(ctx context.Context, mask string) ([]*coreV1.Pod, error) {
	pods := coreV1.PodList{}
	err := c.List(ctx, &pods, k8sClient.InNamespace(c.namespace))
	// TODO: how does simulate error here?
	if err != nil {
		return nil, err
	}
	p := make([]*coreV1.Pod, 0)
	for i := range pods.Items {
		podName := pods.Items[i].ObjectMeta.Name
		if strings.Contains(podName, mask) {
			p = append(p, &pods.Items[i])
		}
	}
	return p, nil
}
