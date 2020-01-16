/*test package includes baremetal test storage class definition for e2e tests
  and definition of e2e test suites with ginkgo library
  main file for e2e tests is in cmd/tests directory
  we can run defined test suites with following command:
  go run cmd/tests/baremetal_e2e.go -ginkgo.v -ginkgo.progress --kubeconfig=<kubeconfig>
*/
package test

import (
	"path"

	"github.com/onsi/ginkgo"
	"github.com/sirupsen/logrus"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/testfiles"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"
	"k8s.io/kubernetes/test/e2e/storage/utils"
)

var CSITestSuites = []func() testsuites.TestSuite{
	testsuites.InitProvisioningTestSuite,
}

var _ = utils.SIGDescribe("CSI Volumes", func() {
	logrus.Info(framework.TestContext.RepoRoot)
	testfiles.AddFileSource(testfiles.RootFileSource{Root: path.Join(framework.TestContext.RepoRoot, "/tmp/baremetal-csi-plugin/templates/")})

	curDriver := BaremetalDriver()
	ginkgo.Context(testsuites.GetDriverNameWithFeatureTags(curDriver), func() {
		testsuites.DefineTestSuite(curDriver, CSITestSuites)
	})
})
