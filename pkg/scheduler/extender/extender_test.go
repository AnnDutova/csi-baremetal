/*
Copyright © 2020 Dell Inc. or its subsidiaries. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package extender

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	coreV1 "k8s.io/api/core/v1"
	storageV1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sCl "sigs.k8s.io/controller-runtime/pkg/client"

	genV1 "github.com/dell/csi-baremetal/api/generated/v1"
	v1 "github.com/dell/csi-baremetal/api/v1"
	acrcrd "github.com/dell/csi-baremetal/api/v1/acreservationcrd"
	accrd "github.com/dell/csi-baremetal/api/v1/availablecapacitycrd"
	volcrd "github.com/dell/csi-baremetal/api/v1/volumecrd"
	"github.com/dell/csi-baremetal/pkg/base"
	"github.com/dell/csi-baremetal/pkg/base/capacityplanner"
	baseerr "github.com/dell/csi-baremetal/pkg/base/error"
	fc "github.com/dell/csi-baremetal/pkg/base/featureconfig"
	"github.com/dell/csi-baremetal/pkg/base/k8s"
	"github.com/dell/csi-baremetal/pkg/base/logger/objects"
	"github.com/dell/csi-baremetal/pkg/base/util"
	annotations "github.com/dell/csi-baremetal/pkg/crcontrollers/node/common"
)

// todo review all tests. some might not be relevant
var (
	testLogger = logrus.New()
	testUUID   = uuid.New().String()
	testCtx    = context.WithValue(context.Background(), base.RequestUUID, testUUID)

	testNs          = "default"
	testProvisioner = "baremetal-csi"
	testSCName1     = "baremetal-sc-qwe"
	testSCName2     = "another-one-sc"

	testSizeGb      int64 = 10
	testSizeStr           = fmt.Sprintf("%dG", testSizeGb)
	testStorageType       = v1.StorageClassHDD

	testCSIVolumeSrc = coreV1.CSIVolumeSource{
		Driver:           testProvisioner,
		VolumeAttributes: map[string]string{base.SizeKey: testSizeStr, base.StorageTypeKey: testStorageType},
	}

	testEphemeralVolumeSrc = coreV1.EphemeralVolumeSource{
		VolumeClaimTemplate: &coreV1.PersistentVolumeClaimTemplate{
			Spec: coreV1.PersistentVolumeClaimSpec{
				StorageClassName: &testSCName1,
				Resources: coreV1.ResourceRequirements{
					Requests: coreV1.ResourceList{
						coreV1.ResourceStorage: *resource.NewQuantity(testSizeGb*1024, resource.DecimalSI),
					},
				},
			},
		},
	}

	testEphemeralVolumeSrcNilSC = coreV1.EphemeralVolumeSource{
		VolumeClaimTemplate: &coreV1.PersistentVolumeClaimTemplate{
			Spec: coreV1.PersistentVolumeClaimSpec{},
		},
	}

	testEphemeralVolumeSrcAnotherDriver = coreV1.EphemeralVolumeSource{
		VolumeClaimTemplate: &coreV1.PersistentVolumeClaimTemplate{
			Spec: coreV1.PersistentVolumeClaimSpec{
				StorageClassName: &testSCName2,
				Resources: coreV1.ResourceRequirements{
					Requests: coreV1.ResourceList{
						coreV1.ResourceStorage: *resource.NewQuantity(testSizeGb*1024, resource.DecimalSI),
					},
				},
			},
		},
	}

	testSC1 = storageV1.StorageClass{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      testSCName1,
			Namespace: testNs,
		},
		Provisioner: testProvisioner,
		Parameters:  map[string]string{base.StorageTypeKey: testStorageType},
	}

	testSC2 = storageV1.StorageClass{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      testSCName2,
			Namespace: testNs,
		},
		Provisioner: "another-provisioner",
		Parameters:  map[string]string{base.StorageTypeKey: "another-storage"},
	}

	testPVCTypeMeta = metaV1.TypeMeta{
		Kind:       "PersistentVolumeClaim",
		APIVersion: "v1",
	}

	testPVC1Name = "pvc-with-plugin"
	testPVC1     = coreV1.PersistentVolumeClaim{
		TypeMeta: testPVCTypeMeta,
		ObjectMeta: metaV1.ObjectMeta{
			Name:      testPVC1Name,
			Namespace: testNs,
		},
		Spec: coreV1.PersistentVolumeClaimSpec{
			StorageClassName: &testSCName1,
			Resources: coreV1.ResourceRequirements{
				Requests: coreV1.ResourceList{
					coreV1.ResourceStorage: *resource.NewQuantity(testSizeGb*1024, resource.DecimalSI),
				},
			},
		},
	}

	testPVC2Name = "not-a-plugin-pvc"
	testPVC2     = coreV1.PersistentVolumeClaim{
		TypeMeta: testPVCTypeMeta,
		ObjectMeta: metaV1.ObjectMeta{
			Name:      testPVC2Name,
			Namespace: testNs,
		},
		Spec: coreV1.PersistentVolumeClaimSpec{
			StorageClassName: &testSCName2,
			Resources: coreV1.ResourceRequirements{
				Requests: coreV1.ResourceList{
					coreV1.ResourceStorage: *resource.NewQuantity(testSizeGb*1024, resource.DecimalSI),
				},
			},
		},
	}

	testPVC3Name = "corrupted-pvc"
	testPVC3     = coreV1.PersistentVolumeClaim{
		TypeMeta: testPVCTypeMeta,
		ObjectMeta: metaV1.ObjectMeta{
			Name:      testPVC3Name,
			Namespace: testNs,
		},
		Spec: coreV1.PersistentVolumeClaimSpec{},
	}

	testPodName = "pod1"
	testPod     = coreV1.Pod{
		TypeMeta:   metaV1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metaV1.ObjectMeta{Name: testPodName, Namespace: testNs},
		Spec:       coreV1.PodSpec{},
	}
)

func TestExtender_gatherVolumesByProvisioner_Success(t *testing.T) {
	e := setup(t)
	pod := testPod.DeepCopy()
	// append ephemeralVolume
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{CSI: &testCSIVolumeSrc},
	})
	// append generic ephemeralVolume with CSI Baremetal SC
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{Ephemeral: &testEphemeralVolumeSrc},
	})
	// append generic ephemeralVolume with Nil SC
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{Ephemeral: &testEphemeralVolumeSrcNilSC},
	})
	// append generic ephemeralVolume with another CSI SC
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{Ephemeral: &testEphemeralVolumeSrcAnotherDriver},
	})
	// append testPVC1
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{
			PersistentVolumeClaim: &coreV1.PersistentVolumeClaimVolumeSource{
				ClaimName: testPVC1Name,
			},
		},
	})
	// append testPVC2
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{
			PersistentVolumeClaim: &coreV1.PersistentVolumeClaimVolumeSource{
				ClaimName: testPVC2Name,
			},
		},
	})
	// append testPVC3
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{
			PersistentVolumeClaim: &coreV1.PersistentVolumeClaimVolumeSource{
				ClaimName: testPVC3Name,
			},
		},
	})
	// create PVCs and SC
	applyObjs(t, e.k8sClient, testPVC1.DeepCopy(), testPVC2.DeepCopy(), testPVC3.DeepCopy(), testSC1.DeepCopy(), testSC2.DeepCopy())

	volumes, err := e.gatherCapacityRequestsByProvisioner(testCtx, pod)
	assert.Nil(t, err)
	assert.Equal(t, 3, len(volumes))
}

func TestExtender_gatherVolumesByProvisioner_Fail(t *testing.T) {
	e := setup(t)

	// sc mapping empty
	pod := testPod.DeepCopy()
	volumes, err := e.gatherCapacityRequestsByProvisioner(testCtx, pod)
	assert.Nil(t, volumes)
	assert.NotNil(t, err)

	// createCapacityRequest failed
	pod = testPod.DeepCopy()
	badCSIVolumeSrc := testCSIVolumeSrc
	badCSIVolumeSrc.VolumeAttributes = map[string]string{}
	// append inlineVolume
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{CSI: &badCSIVolumeSrc},
	})
	// create SC
	applyObjs(t, e.k8sClient, testSC1.DeepCopy())

	volumes, err = e.gatherCapacityRequestsByProvisioner(testCtx, pod)
	assert.Nil(t, err)
	assert.Equal(t, 1, len(volumes))
	//assert.True(t, volumes[0].Ephemeral)
	// any because storageType key missing in csi volume sourc
	assert.Equal(t, v1.StorageClassAny, volumes[0].StorageClass)

	// unable to read PVCs (bad namespace)
	pod.Namespace = "unexisted-namespace"
	// append testPVC1
	pod.Spec.Volumes = append(pod.Spec.Volumes, coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{
			PersistentVolumeClaim: &coreV1.PersistentVolumeClaimVolumeSource{
				ClaimName: testPVC1Name,
			},
		},
	})
	volumes, err = e.gatherCapacityRequestsByProvisioner(testCtx, pod)
	assert.Nil(t, volumes)
	assert.Equal(t, err, baseerr.ErrorNotFound) // PVC can be created later

	// PVC doesn't contain information about size
	pod.Namespace = testNs
	pvcWithoutSize := testPVC1
	delete(pvcWithoutSize.Spec.Resources.Requests, coreV1.ResourceStorage)
	applyObjs(t, e.k8sClient, &pvcWithoutSize)

	pod.Spec.Volumes = []coreV1.Volume{{
		VolumeSource: coreV1.VolumeSource{
			PersistentVolumeClaim: &coreV1.PersistentVolumeClaimVolumeSource{
				ClaimName: testPVC1Name,
			},
		},
	}}

	volumes, err = e.gatherCapacityRequestsByProvisioner(testCtx, pod)
	assert.Nil(t, err)
	assert.NotNil(t, volumes)
	assert.Equal(t, 1, len(volumes))
	assert.Equal(t, int64(0), volumes[0].Size)
}

func TestExtender_constructVolumeFromCSISource_Success(t *testing.T) {
	e := setup(t)
	expectedSize, err := util.StrToBytes(testSizeStr)
	assert.Nil(t, err)
	request := &genV1.CapacityRequest{
		Name:         "-",
		StorageClass: util.ConvertStorageClass(testStorageType),
		Size:         expectedSize,
		//Ephemeral:    true,
	}

	volume := coreV1.Volume{
		VolumeSource: coreV1.VolumeSource{CSI: &testCSIVolumeSrc},
	}
	curr, err := e.createCapacityRequest(testCtx, "", volume)
	assert.Nil(t, err)
	assert.Equal(t, request, curr)

}

func TestExtender_constructVolumeFromCSISource_Fail(t *testing.T) {
	var (
		e = setup(t)
		v = coreV1.Volume{
			VolumeSource: coreV1.VolumeSource{CSI: &testCSIVolumeSrc},
		}
	)

	// missing storage type
	expected := &genV1.CapacityRequest{Name: "-", StorageClass: v1.StorageClassAny}

	vol := v.DeepCopy()
	vol.VolumeSource.CSI.VolumeAttributes = map[string]string{}
	curr, err := e.createCapacityRequest(testCtx, "", *vol)
	assert.NotNil(t, curr)
	assert.Equal(t, expected, curr)
	assert.NotNil(t, err)
	assert.Contains(t, err.Error(), "unable to detect storage class from attributes")

	// unable to convert size
	sizeStr := "12S12"
	vol = v.DeepCopy()
	vol.VolumeSource.CSI.VolumeAttributes[base.SizeKey] = sizeStr
	expected = &genV1.CapacityRequest{Name: "-", StorageClass: util.ConvertStorageClass(testStorageType)}
	curr, err = e.createCapacityRequest(testCtx, "", *vol)
	assert.NotNil(t, curr)
	assert.Equal(t, expected, curr)
	assert.NotNil(t, err)
	assert.Contains(t, err.Error(), sizeStr)
}

func TestExtender_filterCases(t *testing.T) {
	var (
		podName   = "mypod"
		node1Name = "NODE-1"
		node1UID  = "node-1111-uuid"

		e *Extender
	)

	nodes := []coreV1.Node{
		{ObjectMeta: metaV1.ObjectMeta{UID: types.UID(node1UID), Name: node1Name}},
	}

	pod := &coreV1.Pod{
		ObjectMeta: metaV1.ObjectMeta{Name: podName},
	}

	// empty volumes
	e = setup(t)
	matched, failed, err := e.filter(testCtx, pod, nodes, nil)
	assert.Nil(t, err)
	assert.Nil(t, failed)
	assert.Equal(t, len(nodes), len(matched))

	// different scenarios
	e = setup(t)
	capacities := make([]*genV1.CapacityRequest, 1)

	for _, tt := range []struct {
		Status            string
		ExpectedNodeNames []string
		Err               error
	}{
		{Status: v1.ReservationConfirmed, Err: nil},
		{Status: v1.ReservationRejected, Err: nil},
		{Status: v1.ReservationCancelled, Err: errors.New("unsupported reservation status: CANCELLED")},
	} {
		reservation := *e.k8sClient.ConstructACRCR(
			getReservationName(pod),
			genV1.AvailableCapacityReservation{
				Status:       tt.Status,
				NodeRequests: &genV1.NodeRequests{Requested: []string{node1Name}},
			},
		)
		assert.Nil(t, e.k8sClient.Create(testCtx, &reservation))

		matched, failed, err = e.filter(testCtx, pod, nodes, capacities)
		assert.Equal(t, tt.Err, err)
		assert.Nil(t, matched)
		assert.Nil(t, failed)
		assert.Nil(t, e.k8sClient.DeleteCR(testCtx, &reservation))
	}
}

func TestExtender_filterSuccess(t *testing.T) {
	var (
		node1Name = "NODE-1"
		node2Name = "NODE-2"
		node3Name = "NODE-3"
		node1UID  = "node-1111-uuid"
		node2UID  = "node-2222-uuid"
		node3UID  = "node-3333-uuid"
	)

	nodes := []coreV1.Node{
		{ObjectMeta: metaV1.ObjectMeta{UID: types.UID(node1UID), Name: node1Name}},
		{ObjectMeta: metaV1.ObjectMeta{UID: types.UID(node2UID), Name: node2Name}},
		{ObjectMeta: metaV1.ObjectMeta{UID: types.UID(node3UID), Name: node3Name}},
	}
	// empty volumes
	e := setup(t)
	matched, failed, err := e.filter(testCtx, &coreV1.Pod{ObjectMeta: metaV1.ObjectMeta{Name: "pod-1"}}, nodes, nil)
	assert.Nil(t, err)
	assert.Nil(t, failed)
	assert.Equal(t, len(nodes), len(matched))

	// create all AC
	for _, ac := range []*accrd.AvailableCapacity{
		// NODE-1 ACs, HDD[50Gb, 100Gb]
		e.k8sClient.ConstructACCR(uuid.New().String(),
			genV1.AvailableCapacity{NodeId: node1UID, StorageClass: v1.StorageClassHDD, Size: 50 * int64(util.GBYTE)}),
		e.k8sClient.ConstructACCR(uuid.New().String(),
			genV1.AvailableCapacity{NodeId: node1UID, StorageClass: v1.StorageClassHDD, Size: 100 * int64(util.GBYTE)}),
		// NODE-2 ACs, HDD[100Gb], SSD[50Gb]
		e.k8sClient.ConstructACCR(uuid.New().String(),
			genV1.AvailableCapacity{NodeId: node2UID, StorageClass: v1.StorageClassHDD, Size: 100 * int64(util.GBYTE)}),
		e.k8sClient.ConstructACCR(uuid.New().String(),
			genV1.AvailableCapacity{NodeId: node2UID, StorageClass: v1.StorageClassSSD, Size: 50 * int64(util.GBYTE)}),
		// NODE-3 ACs, HDDLVG[150Gb], SSDLVG[100Gb]
		e.k8sClient.ConstructACCR(uuid.New().String(),
			genV1.AvailableCapacity{NodeId: node3UID, StorageClass: v1.StorageClassHDDLVG, Size: 150 * int64(util.GBYTE)}),
		e.k8sClient.ConstructACCR(uuid.New().String(),
			genV1.AvailableCapacity{NodeId: node3UID, StorageClass: v1.StorageClassSSDLVG, Size: 100 * int64(util.GBYTE)}),
	} {
		assert.Nil(t, e.k8sClient.Create(testCtx, ac))
	}

	testCases := []struct {
		Pod               *coreV1.Pod
		CR                []*genV1.CapacityRequest
		ACR               []*acrcrd.AvailableCapacityReservation
		ExpectedNodeNames []string
		Msg               string
	}{
		{
			Pod: &coreV1.Pod{
				ObjectMeta: metaV1.ObjectMeta{Name: "mypod-hdd-1"},
			},
			CR: []*genV1.CapacityRequest{
				{StorageClass: v1.StorageClassHDD, Size: 50 * int64(util.GBYTE)},
				{StorageClass: v1.StorageClassHDD, Size: 100 * int64(util.GBYTE)},
			},
			ACR: []*acrcrd.AvailableCapacityReservation{
				e.k8sClient.ConstructACRCR(
					"default-mypod-hdd-1",
					genV1.AvailableCapacityReservation{
						Status:       v1.ReservationConfirmed,
						NodeRequests: &genV1.NodeRequests{Reserved: []string{node1UID}},
						ReservationRequests: []*genV1.ReservationRequest{
							{CapacityRequest: &genV1.CapacityRequest{StorageClass: v1.StorageClassHDD, Size: 50 * int64(util.GBYTE)}},
						},
					},
				),
				e.k8sClient.ConstructACRCR(
					"default-mypod-hdd-2",
					genV1.AvailableCapacityReservation{
						Status:       v1.ReservationConfirmed,
						NodeRequests: &genV1.NodeRequests{Reserved: []string{node1UID}},
						ReservationRequests: []*genV1.ReservationRequest{
							{CapacityRequest: &genV1.CapacityRequest{StorageClass: v1.StorageClassHDD, Size: 100 * int64(util.GBYTE)}},
						},
					},
				),
			},
			ExpectedNodeNames: []string{node1Name},
			Msg:               "Volumes: HDD[50Gb, 100Gb]; Expected nodes: [NODE-1]",
		},
		{
			Pod: &coreV1.Pod{
				ObjectMeta: metaV1.ObjectMeta{Name: "mypod-ssd-1"},
			},
			CR: []*genV1.CapacityRequest{
				{StorageClass: v1.StorageClassSSD, Size: 50 * int64(util.GBYTE)},
			},
			ACR: []*acrcrd.AvailableCapacityReservation{
				e.k8sClient.ConstructACRCR(
					"default-mypod-ssd-1",
					genV1.AvailableCapacityReservation{
						Status:       v1.ReservationConfirmed,
						NodeRequests: &genV1.NodeRequests{Reserved: []string{node2UID}},
						ReservationRequests: []*genV1.ReservationRequest{
							{CapacityRequest: &genV1.CapacityRequest{StorageClass: v1.StorageClassSSD, Size: 50 * int64(util.GBYTE)}},
						},
					},
				),
			},
			ExpectedNodeNames: []string{node2Name},
			Msg:               "Volumes: SSD[50Gb]; Expected nodes: [NODE-2]",
		},
		{
			Pod: &coreV1.Pod{
				ObjectMeta: metaV1.ObjectMeta{Name: "mypod-hdd-1"},
			},
			CR: []*genV1.CapacityRequest{
				{StorageClass: v1.StorageClassHDD, Size: 80 * int64(util.GBYTE)},
			},
			ACR: []*acrcrd.AvailableCapacityReservation{
				e.k8sClient.ConstructACRCR(
					"default-mypod-hdd-1",
					genV1.AvailableCapacityReservation{
						Status:       v1.ReservationConfirmed,
						NodeRequests: &genV1.NodeRequests{Reserved: []string{node1UID}},
						ReservationRequests: []*genV1.ReservationRequest{
							{CapacityRequest: &genV1.CapacityRequest{StorageClass: v1.StorageClassHDD, Size: 80 * int64(util.GBYTE)}},
						},
					},
				),
			},
			ExpectedNodeNames: []string{node1Name},
			Msg:               "Volumes: HDD[80Gb]; Expected nodes: [NODE-1]",
		},
		{
			Pod: &coreV1.Pod{
				ObjectMeta: metaV1.ObjectMeta{Name: "mypod-lvg-2"},
			},
			ACR: []*acrcrd.AvailableCapacityReservation{
				e.k8sClient.ConstructACRCR(
					"default-mypod-lvg-2",
					genV1.AvailableCapacityReservation{
						Status:       v1.ReservationConfirmed,
						NodeRequests: &genV1.NodeRequests{Reserved: []string{node3UID}},
						ReservationRequests: []*genV1.ReservationRequest{
							{CapacityRequest: &genV1.CapacityRequest{StorageClass: v1.StorageClassHDDLVG, Size: 50 * int64(util.GBYTE)}},
						},
					},
				),
				e.k8sClient.ConstructACRCR(
					"default-mypod-lvg-3",
					genV1.AvailableCapacityReservation{
						Status:       v1.ReservationConfirmed,
						NodeRequests: &genV1.NodeRequests{Reserved: []string{node3UID}},
						ReservationRequests: []*genV1.ReservationRequest{
							{CapacityRequest: &genV1.CapacityRequest{StorageClass: v1.StorageClassHDDLVG, Size: 50 * int64(util.GBYTE)}},
						},
					},
				),
				e.k8sClient.ConstructACRCR(
					"default-mypod-lvg-4",
					genV1.AvailableCapacityReservation{
						Status:       v1.ReservationConfirmed,
						NodeRequests: &genV1.NodeRequests{Reserved: []string{node3UID}},
						ReservationRequests: []*genV1.ReservationRequest{
							{CapacityRequest: &genV1.CapacityRequest{StorageClass: v1.StorageClassHDDLVG, Size: 50 * int64(util.GBYTE)}},
						},
					},
				),
			},
			CR: []*genV1.CapacityRequest{
				{StorageClass: v1.StorageClassHDDLVG, Size: 50 * int64(util.GBYTE)},
				{StorageClass: v1.StorageClassHDDLVG, Size: 50 * int64(util.GBYTE)},
				{StorageClass: v1.StorageClassHDDLVG, Size: 50 * int64(util.GBYTE)},
			},
			ExpectedNodeNames: []string{node3Name},
			Msg:               "Volumes: HDDLVG[50Gb, 50Gb, 50Gb]; Expected nodes: [NODE-3]",
		},
		{
			Pod: &coreV1.Pod{
				ObjectMeta: metaV1.ObjectMeta{Name: "mypod-lvg-1"},
			},
			ACR: []*acrcrd.AvailableCapacityReservation{
				e.k8sClient.ConstructACRCR(
					"default-mypod-lvg-1",
					genV1.AvailableCapacityReservation{
						Status:       v1.ReservationConfirmed,
						NodeRequests: &genV1.NodeRequests{Reserved: []string{node3UID}},
						ReservationRequests: []*genV1.ReservationRequest{
							{CapacityRequest: &genV1.CapacityRequest{StorageClass: v1.StorageClassHDDLVG, Size: 150 * int64(util.GBYTE)}},
						},
					},
				),
				e.k8sClient.ConstructACRCR(
					"default-mypod-lvg-2",
					genV1.AvailableCapacityReservation{
						Status:       v1.ReservationConfirmed,
						NodeRequests: &genV1.NodeRequests{Reserved: []string{node3UID}},
						ReservationRequests: []*genV1.ReservationRequest{
							{CapacityRequest: &genV1.CapacityRequest{StorageClass: v1.StorageClassSSDLVG, Size: 100 * int64(util.GBYTE)}},
						},
					},
				),
			},
			CR: []*genV1.CapacityRequest{
				{StorageClass: v1.StorageClassHDDLVG, Size: 150 * int64(util.GBYTE)},
				{StorageClass: v1.StorageClassSSDLVG, Size: 100 * int64(util.GBYTE)},
			},
			ExpectedNodeNames: []string{node3Name},
			Msg:               "Volumes: HDDLVG[150Gb], SSDLVG[100Gb]; Expected nodes: [NODE-3]",
		},
	}

	for _, testCase := range testCases {
		for _, acr := range testCase.ACR {
			assert.Nil(t, e.k8sClient.Create(testCtx, acr))
		}
		matchedNodes, failedNode, _ := e.filter(testCtx, testCase.Pod, nodes, testCase.CR)
		assert.Equal(t, len(nodes)-len(matchedNodes), len(failedNode), testCase.Msg)
		matchedNodeNames := getNodeNames(matchedNodes)
		assert.Equal(t, len(testCase.ExpectedNodeNames), len(matchedNodes),
			fmt.Sprintf("Matched nodes %v. Test case: %v", matchedNodeNames, testCase.Msg))
		assert.Nil(t, err, testCase.Msg)

		// check ACRs
		acrList := &acrcrd.AvailableCapacityReservationList{}
		assert.Nil(t, e.k8sClient.ReadList(testCtx, acrList), testCase.Msg)
		if len(testCase.ExpectedNodeNames) > 0 {
			assert.Equal(t, len(testCase.CR), len(acrList.Items), testCase.Msg)
		}

		reservedACCount := 0
		for _, acr := range acrList.Items {
			reservedACCount += len(acr.Spec.ReservationRequests)
		}
		assert.Equal(t, len(testCase.ExpectedNodeNames)*len(testCase.CR), reservedACCount, testCase.Msg)

		for _, n := range testCase.ExpectedNodeNames {
			assert.True(t, util.ContainsString(matchedNodeNames, n),
				fmt.Sprintf("Matched nodes: %v, msg - %s", matchedNodeNames, testCase.Msg))
		}
		removeAllACRs(e.k8sClient, t)
	}
}

func TestExtender_getSCNameStorageType_Success(t *testing.T) {
	e := setup(t)
	// create 2 storage classes
	applyObjs(t, e.k8sClient, testSC1.DeepCopy(), testSC2.DeepCopy())

	m, err := e.buildSCChecker(testCtx, testLogger.WithField("test", "buildSCChecker"))
	assert.Nil(t, err)
	assert.Equal(t, 1, len(m.managedSCs))
	assert.Equal(t, 1, len(m.unmanagedSCs))
	assert.Equal(t, m.managedSCs[testSCName1], testStorageType)
}

func TestExtender_buildSCChecker_Fail(t *testing.T) {
	e := setup(t)

	m, err := e.buildSCChecker(testCtx, testLogger.WithField("test", "buildSCChecker"))
	assert.Nil(t, m)
	assert.NotNil(t, err)
}

func TestExtender_scChecker_check(t *testing.T) {
	ch := &scChecker{
		managedSCs:   map[string]string{testSCName1: testStorageType},
		unmanagedSCs: map[string]bool{testSCName2: true},
	}

	storageType, scType := ch.check(testSCName1)
	assert.Equal(t, scType, managedSC)
	assert.Equal(t, storageType, testStorageType)

	_, scType = ch.check(testSCName2)
	assert.Equal(t, scType, unmanagedSC)

	_, scType = ch.check("non-cached-sc")
	assert.Equal(t, scType, unknown)
}

func setup(t *testing.T) *Extender {
	k, err := k8s.GetFakeKubeClient(testNs, testLogger)
	assert.Nil(t, err)

	featureConf := fc.NewFeatureConfig()
	kubeClient := k8s.NewKubeClient(k, testLogger, objects.NewObjectLogger(), testNs)
	kubeCache := k8s.NewKubeCache(k, testLogger)
	return &Extender{
		k8sClient:              kubeClient,
		k8sCache:               kubeCache,
		featureChecker:         featureConf,
		namespace:              testNs,
		provisioner:            testProvisioner,
		logger:                 testLogger.WithField("component", "Extender"),
		capacityManagerBuilder: &capacityplanner.DefaultCapacityManagerBuilder{},
	}
}

func Test_prepareListOfNodes(t *testing.T) {
	e := setup(t)
	testCases := []struct {
		Message       string
		ExpectedNodes []string
		Nodes         []coreV1.Node
	}{
		{
			Message: "exclude nodes without uuid fails",
			Nodes: []coreV1.Node{
				{
					ObjectMeta: metaV1.ObjectMeta{
						UID:         types.UID("1111-2222"),
						Name:        "node-1",
						Annotations: map[string]string{annotations.DeafultNodeIDAnnotationKey: "aaaa-bbbb"},
					},
				},
				{
					ObjectMeta: metaV1.ObjectMeta{
						UID:         types.UID(""),
						Name:        "node-1",
						Annotations: map[string]string{annotations.DeafultNodeIDAnnotationKey: "aaaa-bbbb"},
					},
				},
			},
			ExpectedNodes: []string{"1111-2222"},
		},
		{
			Message: "expected nodes count not equal actual",
			Nodes: []coreV1.Node{
				{
					ObjectMeta: metaV1.ObjectMeta{
						UID:         types.UID("1111-2222"),
						Name:        "node-1",
						Annotations: map[string]string{annotations.DeafultNodeIDAnnotationKey: "aaaa-bbbb"},
					},
				},
				{
					ObjectMeta: metaV1.ObjectMeta{
						UID:         types.UID("1111-3333"),
						Name:        "node-1",
						Annotations: map[string]string{annotations.DeafultNodeIDAnnotationKey: "aaaa-bbbb"},
					},
				},
			},
			ExpectedNodes: []string{"1111-2222", "1111-3333"},
		},
		{
			Message:       "empty nodes leads to empty nodeids",
			Nodes:         []coreV1.Node{},
			ExpectedNodes: []string{},
		},
	}
	for _, tt := range testCases {
		assert.Equalf(t, tt.ExpectedNodes, e.prepareListOfRequestedNodes(tt.Nodes), tt.Message)
	}
}

func Test_Score(t *testing.T) {
	e := setup(t)
	uid := "1111-2222"
	val := "aaaa-bbbb"
	nodes := []coreV1.Node{
		{
			ObjectMeta: metaV1.ObjectMeta{
				UID:         types.UID(uid),
				Name:        "node-1",
				Annotations: map[string]string{annotations.DeafultNodeIDAnnotationKey: val},
				Labels:      map[string]string{"app": "baremetal-csi"},
			},
		},
	}

	_, err := e.score(nodes)
	assert.Nil(t, err)
}

func Test_getNodeId(t *testing.T) {
	var (
		e    = setup(t)
		uid  = "1111-2222"
		val  = "aaaa-bbbb"
		node = coreV1.Node{
			ObjectMeta: metaV1.ObjectMeta{
				UID:         types.UID(uid),
				Name:        "node-1",
				Annotations: map[string]string{annotations.DeafultNodeIDAnnotationKey: val},
				Labels:      map[string]string{"app": "baremetal-csi"},
			},
		}
		res           string
		annotationKey string
	)

	featureConf := fc.NewFeatureConfig()

	res, err := annotations.GetNodeID(&node, annotationKey, "", featureConf)
	assert.Equal(t, uid, res)
	assert.Nil(t, err)

	featureConf.Update(fc.FeatureNodeIDFromAnnotation, true)
	e.featureChecker = featureConf

	res, err = annotations.GetNodeID(&node, annotationKey, "app=baremetal-csi", featureConf)
	assert.Equal(t, val, res)
	assert.Nil(t, err)

	node.Annotations = nil
	res, err = annotations.GetNodeID(&node, annotationKey, "app=baremetal-csi", featureConf)
	assert.Equal(t, "", res)
	assert.NotNil(t, err)
}

func applyObjs(t *testing.T, k8sClient *k8s.KubeClient, objs ...k8sCl.Object) {
	for _, obj := range objs {
		assert.Nil(t, k8sClient.Create(testCtx, obj))
	}
}

func Test_nodePrioritize(t *testing.T) {
	type args struct {
		nodeMapping map[string][]volcrd.Volume
	}
	tests := []struct {
		name  string
		args  args
		want  map[string]int64
		want1 int64
	}{{
		name: "Zero volumes",
		args: args{
			nodeMapping: map[string][]volcrd.Volume{"node1": nil, "node2": nil},
		},
		want:  map[string]int64{"node1": 0, "node2": 0},
		want1: 0,
	},
		{
			name: "2 volumes with equal number of volumes",
			args: args{
				nodeMapping: map[string][]volcrd.Volume{"node1": {volcrd.Volume{}}, "node2": {volcrd.Volume{}}},
			},
			want:  map[string]int64{"node1": 0, "node2": 0},
			want1: 1,
		},
		{
			name: "Node2 must have higher priority",
			args: args{
				nodeMapping: map[string][]volcrd.Volume{"node1": {volcrd.Volume{}, volcrd.Volume{}}, "node2": {volcrd.Volume{}}},
			},
			want:  map[string]int64{"node1": 0, "node2": 1},
			want1: 2,
		},
		{
			name: "Node3 must have higher priority",
			args: args{
				nodeMapping: map[string][]volcrd.Volume{"node1": {volcrd.Volume{}, volcrd.Volume{}}, "node2": {volcrd.Volume{}, volcrd.Volume{}}, "node3": {volcrd.Volume{}}},
			},
			want:  map[string]int64{"node1": 0, "node2": 0, "node3": 1},
			want1: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1 := nodePrioritize(tt.args.nodeMapping)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("nodePrioritize() got = %v, want %v", got, tt.want)
			}
			if got1 != tt.want1 {
				t.Errorf("nodePrioritize() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}

func Test_reservationName(t *testing.T) {
	podName := "mypod-0"
	namespace := "mynamespace"
	pod := &coreV1.Pod{ObjectMeta: metaV1.ObjectMeta{Name: podName, Namespace: namespace}}
	name := getReservationName(pod)
	assert.Equal(t, namespace+"-"+podName, name)

	pod = &coreV1.Pod{ObjectMeta: metaV1.ObjectMeta{Name: podName, Namespace: ""}}
	name = getReservationName(pod)
	assert.Equal(t, "default-"+podName, name)

}

func Test_createReservation(t *testing.T) {
	// names
	namespace := "test"
	podName := "mypod-0"
	pod := &coreV1.Pod{ObjectMeta: metaV1.ObjectMeta{Name: podName, Namespace: namespace}}
	name := getReservationName(pod)
	// volumes
	capacityRequests := []*genV1.CapacityRequest{{Name: "pvc-1", Size: 100, StorageClass: "HDD"}}
	// nodes
	nodes := []coreV1.Node{{ObjectMeta: metaV1.ObjectMeta{Name: "node-1", UID: "uuid-1"}}}

	e := setup(t)
	assert.Nil(t, e.createReservation(testCtx, namespace, name, nodes, capacityRequests))

	// empty node returns nil
	assert.Nil(t, e.createReservation(testCtx, namespace, name, []coreV1.Node{}, capacityRequests))

	// read back and check fields
	reservationResource := &acrcrd.AvailableCapacityReservation{}
	err := e.k8sClient.ReadCR(testCtx, name, "", reservationResource)
	assert.Nil(t, err)
	assert.Equal(t, name, reservationResource.Name)
	assert.Equal(t, namespace, reservationResource.Spec.Namespace)
	assert.Equal(t, len(nodes), len(reservationResource.Spec.NodeRequests.Requested))
	assert.Equal(t, len(capacityRequests), len(reservationResource.Spec.ReservationRequests))

	// empty namespace
	namespace = ""
	pod = &coreV1.Pod{ObjectMeta: metaV1.ObjectMeta{Name: podName, Namespace: namespace}}
	name = getReservationName(pod)
	err = e.createReservation(testCtx, namespace, name, nodes, capacityRequests)
	assert.Nil(t, err)

	reservationResource = &acrcrd.AvailableCapacityReservation{}
	err = e.k8sClient.ReadCR(testCtx, name, "", reservationResource)
	assert.Equal(t, name, reservationResource.Name)
	assert.Equal(t, namespace, reservationResource.Spec.Namespace)
	assert.Equal(t, len(nodes), len(reservationResource.Spec.NodeRequests.Requested))
	assert.Equal(t, len(capacityRequests), len(reservationResource.Spec.ReservationRequests))
}

func removeAllACRs(k *k8s.KubeClient, t *testing.T) {
	acrList := acrcrd.AvailableCapacityReservationList{}
	assert.Nil(t, k.ReadList(testCtx, &acrList))
	for _, acr := range acrList.Items {
		assert.Nil(t, k.DeleteCR(testCtx, &acr))
	}
}

func getNodeNames(nodes []coreV1.Node) []string {
	nodeNames := make([]string, 0)
	for _, n := range nodes {
		nodeNames = append(nodeNames, n.Name)
	}
	return nodeNames
}
