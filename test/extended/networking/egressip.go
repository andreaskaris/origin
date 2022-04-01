package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"strings"
	"time"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	exutil "github.com/openshift/origin/test/extended/util"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	namespacePrefix     = "egressip"
	targetDaemonSetName = "egressip-target-daemonset"
	targetHostPortMin   = 32667
	targetHostPortMax   = 32767
	egressIPYaml        = "egressip.yaml"
	probePodName        = "prober-pod"
)

var _ = g.Describe("[sig-network][Feature:EgressIP]", func() {
	oc := exutil.NewCLI(namespacePrefix)
	portAllocator := NewPortAllocator(targetHostPortMin, targetHostPortMax)

	var (
		clientset      kubernetes.Interface
		tmpDirEgressIP string

		targetNode     corev1.Node
		sourceNodes    []corev1.Node
		sourceNodesStr []string

		sourceNamespace string
		targetNamespace string

		targetDaemonset *v1.DaemonSet
		targetPort      int
		targetIP        string
		ingressDomain   string

		cloudType configv1.PlatformType
	)

	g.BeforeEach(func() {
		g.By("Creating a temp directory")
		var err error
		tmpDirEgressIP, err = ioutil.TempDir("", "egressip-e2e")
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Getting the clientset")
		f := oc.KubeFramework()
		clientset = f.ClientSet

		g.By("Determining the cloud infrastructure type")
		infra, err := oc.AdminConfigClient().ConfigV1().Infrastructures().Get(context.Background(), "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		cloudType = infra.Spec.PlatformSpec.Type

		g.By("Creating a target namespace")
		// Create a target namespace and assign source and target namespace
		// to variables for later use.
		sourceNamespace = f.Namespace.Name
		targetNamespace = oc.SetupNamespace()

		g.By("Getting all worker nodes in alphabetical order")
		// Get all worker nodes, order them alphabetically and populate
		// targetNode and sourceNodes.
		nodes, err := GetWorkerNodesOrdered(clientset)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Selecting a target node and the source nodes")
		targetNode = nodes[0]
		sourceNodes = nodes[1:]
		for _, s := range sourceNodes {
			sourceNodesStr = append(sourceNodesStr, s.Name)
		}

		g.By("Adding SCC hostnetwork to the target namespace")
		_, err = oc.AsAdmin().Run("adm").Args("policy", "add-scc-to-user", "hostnetwork", fmt.Sprintf("system:serviceaccount:%s:default", targetNamespace)).Output()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Creating the target DaemonSet with a single hostnetworked pod on the target node")
		// Try the entire port range to create the DaemonSet.
		for i := 0; i < targetHostPortMax-targetHostPortMin; i++ {
			containerPort, err := portAllocator.AllocateNextPort()
			o.Expect(err).NotTo(o.HaveOccurred())

			// use the port that we got from the port allocator for this
			// new DS / pod. Store the created daemonset for later.
			targetDaemonset, err = CreateHostNetworkedDaemonSetAndProbe(
				clientset,
				targetNamespace,
				targetNode.Name,
				targetDaemonSetName,
				containerPort,
				10, // every 10 seconds
				6,  // for 6 retries
			)

			// If this is a port conflict, then keep the port allocation and
			// simply continue (but delete the current DS first).
			// The current port is hence marked as unavailable for
			// further tries.
			if err != nil {
				if strings.Contains(err.Error(), "Port conflict when creating pod") {
					err := DeleteDaemonSet(clientset, targetNamespace, targetDaemonSetName)
					o.Expect(err).NotTo(o.HaveOccurred())
					continue
				}
			}

			// Any other error shoud not have occurred.
			o.Expect(err).NotTo(o.HaveOccurred())

			// Break if no error was found.
			targetPort = containerPort
			break
		}

		g.By("Getting the targetIP for the test from the DaemonSet pod")
		podIPs, err := GetDaemonSetPodIPs(clientset, targetNamespace, targetDaemonSetName)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(podIPs)).Should(o.BeNumerically(">", 0))
		targetIP = podIPs[0]

		g.By("Setting the ingressdomain")
		ingressDomain, err = GetIngressDomain(oc)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Setting the source nodes as EgressIP assignable")
		for _, node := range sourceNodesStr {
			_, err = oc.AsAdmin().Run("label").Args("node", node, "k8s.ovn.org/egress-assignable=").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
		}
	})

	g.AfterEach(func() {
		g.By("Deleting the EgressIP object if it exists")
		egressIPYamlPath := tmpDirEgressIP + "/" + egressIPYaml
		if _, err := os.Stat(egressIPYamlPath); err == nil {
			_, err = oc.AsAdmin().Run("delete").Args("-f", tmpDirEgressIP+"/"+egressIPYaml).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
		}

		g.By("Removing the EgressIP assignable annotation")
		for _, node := range sourceNodesStr {
			_, err := oc.AsAdmin().Run("label").Args("node", node, "k8s.ovn.org/egress-assignable-").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
		}

		g.By("Removing the temp directory")
		o.Expect(os.RemoveAll(tmpDirEgressIP)).To(o.Succeed())
	})

	InOVNKubernetesContext(func() {
		g.When("assigning egress IPs to namespaces", func() {
			g.It("pods should have the assigned EgressIPs", func() {
				g.By("Creating the EgressIP test source deployment")
				_, routeName, err := CreateEgressIPTestSource(oc, sourceNamespace, ingressDomain, 2, sourceNodesStr)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Getting a map of source nodes and potential Egress IPs for these nodes")
				nodeEgressIPMap, err := FindNodeEgressIPs(oc, sourceNodesStr, cloudType)
				framework.Logf("%v", nodeEgressIPMap)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Choosing the EgressIPs to be assigned")
				var egressIPs []string
				egressIPSet := make(map[string]struct{})
				for _, eip := range nodeEgressIPMap {
					_, ok := egressIPSet[eip]
					if !ok {
						egressIPs = append(egressIPs, eip)
						egressIPSet[eip] = struct{}{}
					}
				}

				g.By("Marshalling the desired EgressIPs into a string")
				egressIPsString, err := json.Marshal(egressIPs)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Creating the EgressIP object and writing it to disk")
				var egressIPConfig = fmt.Sprintf(
					egressIPYamlTemplate2, // template yaml
					sourceNamespace,       // name of EgressIP
					egressIPsString,       // compact yaml of egressIPs
					fmt.Sprintf("kubernetes.io/metadata.name: %s", sourceNamespace), // namespace selector
				)
				err = ioutil.WriteFile(tmpDirEgressIP+"/"+egressIPYaml, []byte(egressIPConfig), 0644)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Applying the EgressIP object")
				_, err = oc.AsAdmin().Run("create").Args("-f", tmpDirEgressIP+"/"+egressIPYaml).Output()
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Launching a new prober pod and probing for EgressIPs")
				clientIPSet, err := ProbeForClientIPs(oc, targetNamespace, probePodName, routeName, targetIP, targetPort, 10)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Making sure that all EgressIPs and only the EgressIPs were part of the response")
				framework.Logf("egressIPSet is: %v", egressIPSet)
				framework.Logf("clientIPSet is: %v", clientIPSet)
				o.Expect(reflect.DeepEqual(egressIPSet, clientIPSet)).To(o.BeTrue())
				time.Sleep(300 * time.Second)
			})

			g.It("pods should have the assigned EgressIP when the EgressIP is on another node than the pod", func() {
				g.By("Picking the start source node")
				podNode := sourceNodesStr[:1]
				egressIPNode := sourceNodesStr[1:2]

				g.By("Creating the EgressIP test source deployment")
				_, routeName, err := CreateEgressIPTestSource(oc, sourceNamespace, ingressDomain, 1, podNode)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Getting a map of source nodes and potential Egress IPs for these nodes")
				nodeEgressIPMap, err := FindNodeEgressIPs(oc, egressIPNode, cloudType)
				framework.Logf("%v", nodeEgressIPMap)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Choosing the EgressIPs to be assigned")
				var egressIPs []string
				egressIPSet := make(map[string]struct{})
				for _, eip := range nodeEgressIPMap {
					_, ok := egressIPSet[eip]
					if !ok {
						egressIPs = append(egressIPs, eip)
						egressIPSet[eip] = struct{}{}
					}
				}

				g.By("Marshalling the desired EgressIPs into a string")
				egressIPsString, err := json.Marshal(egressIPs)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Creating the EgressIP object and writing it to disk")
				var egressIPConfig = fmt.Sprintf(
					egressIPYamlTemplate2, // template yaml
					sourceNamespace,       // name of EgressIP
					egressIPsString,       // compact yaml of egressIPs
					fmt.Sprintf("kubernetes.io/metadata.name: %s", sourceNamespace), // namespace selector
				)
				err = ioutil.WriteFile(tmpDirEgressIP+"/"+egressIPYaml, []byte(egressIPConfig), 0644)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Applying the EgressIP object")
				_, err = oc.AsAdmin().Run("create").Args("-f", tmpDirEgressIP+"/"+egressIPYaml).Output()
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Launching a new prober pod and probing for EgressIPs")
				clientIPSet, err := ProbeForClientIPs(oc, targetNamespace, probePodName, routeName, targetIP, targetPort, 10)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Making sure that all EgressIPs and only the EgressIPs were part of the response")
				framework.Logf("egressIPSet is: %v", egressIPSet)
				framework.Logf("clientIPSet is: %v", clientIPSet)
				time.Sleep(5 * time.Minute)
				o.Expect(reflect.DeepEqual(egressIPSet, clientIPSet)).To(o.BeTrue())
			})

			g.It("pods should keep the assigned EgressIPs when being rescheduled to another node", func() {
				g.By("Picking the start source node")
				startSourceNode := sourceNodesStr[:1]
				endSourceNodes := sourceNodesStr[1:]

				g.By("Creating the EgressIP test source deployment")
				sourceDeploymentName, routeName, err := CreateEgressIPTestSource(oc, sourceNamespace, ingressDomain, 1, startSourceNode)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Getting a map of source nodes and potential Egress IPs for these nodes")
				nodeEgressIPMap, err := FindNodeEgressIPs(oc, startSourceNode, cloudType)
				framework.Logf("%v", nodeEgressIPMap)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Choosing the EgressIPs to be assigned")
				var egressIPs []string
				egressIPSet := make(map[string]struct{})
				for _, eip := range nodeEgressIPMap {
					_, ok := egressIPSet[eip]
					if !ok {
						egressIPs = append(egressIPs, eip)
						egressIPSet[eip] = struct{}{}
					}
				}

				g.By("Marshalling the desired EgressIPs into a string")
				egressIPsString, err := json.Marshal(egressIPs)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Creating the EgressIP object and writing it to disk")
				var egressIPConfig = fmt.Sprintf(
					egressIPYamlTemplate2, // template yaml
					sourceNamespace,       // name of EgressIP
					egressIPsString,       // compact yaml of egressIPs
					fmt.Sprintf("kubernetes.io/metadata.name: %s", sourceNamespace), // namespace selector
				)
				err = ioutil.WriteFile(tmpDirEgressIP+"/"+egressIPYaml, []byte(egressIPConfig), 0644)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Applying the EgressIP object")
				_, err = oc.AsAdmin().Run("create").Args("-f", tmpDirEgressIP+"/"+egressIPYaml).Output()
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Launching a new prober pod and probing for EgressIPs")
				clientIPSet, err := ProbeForClientIPs(oc, targetNamespace, probePodName, routeName, targetIP, targetPort, 10)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Making sure that all EgressIPs and only the EgressIPs were part of the response")
				framework.Logf("egressIPSet is: %v", egressIPSet)
				framework.Logf("clientIPSet is: %v", clientIPSet)
				o.Expect(reflect.DeepEqual(egressIPSet, clientIPSet)).To(o.BeTrue())

				g.By("Updating the source deployment's Affinity and moving it to the other source node")
				UpdateDeploymentAffinity(oc, sourceNamespace, sourceDeploymentName, endSourceNodes)

				g.By("Launching a new prober pod and probing for EgressIPs")
				clientIPSet, err = ProbeForClientIPs(oc, targetNamespace, probePodName, routeName, targetIP, targetPort, 10)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Making sure that all EgressIPs and only the EgressIPs were part of the response")
				framework.Logf("egressIPSet is: %v", egressIPSet)
				framework.Logf("clientIPSet is: %v", clientIPSet)
				o.Expect(reflect.DeepEqual(egressIPSet, clientIPSet)).To(o.BeTrue())
			})

			g.It("should be able to tolerate recreation of the same EgressIP", func() {
				g.By("Creating the EgressIP test source deployment")
				_, routeName, err := CreateEgressIPTestSource(oc, sourceNamespace, ingressDomain, 2, sourceNodesStr)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Getting a map of source nodes and potential Egress IPs for these nodes")
				nodeEgressIPMap, err := FindNodeEgressIPs(oc, sourceNodesStr, cloudType)
				framework.Logf("%v", nodeEgressIPMap)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Choosing the EgressIPs to be assigned")
				var egressIPs []string
				egressIPSet := make(map[string]struct{})
				for _, eip := range nodeEgressIPMap {
					_, ok := egressIPSet[eip]
					if !ok {
						egressIPs = append(egressIPs, eip)
						egressIPSet[eip] = struct{}{}
					}
				}

				g.By("Marshalling the desired EgressIPs into a string")
				egressIPsString, err := json.Marshal(egressIPs)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Creating the EgressIP object and writing it to disk")
				var egressIPConfig = fmt.Sprintf(
					egressIPYamlTemplate2, // template yaml
					sourceNamespace,       // name of EgressIP
					egressIPsString,       // compact yaml of egressIPs
					fmt.Sprintf("kubernetes.io/metadata.name: %s", sourceNamespace), // namespace selector
				)
				err = ioutil.WriteFile(tmpDirEgressIP+"/"+egressIPYaml, []byte(egressIPConfig), 0644)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Applying the EgressIP object")
				_, err = oc.AsAdmin().Run("create").Args("-f", tmpDirEgressIP+"/"+egressIPYaml).Output()
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Launching a new prober pod and probing for EgressIPs")
				clientIPSet, err := ProbeForClientIPs(oc, targetNamespace, probePodName, routeName, targetIP, targetPort, 10)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Making sure that all EgressIPs and only the EgressIPs were part of the response")
				framework.Logf("egressIPSet is: %v", egressIPSet)
				framework.Logf("clientIPSet is: %v", clientIPSet)
				o.Expect(reflect.DeepEqual(egressIPSet, clientIPSet)).To(o.BeTrue())

				g.By("Deleting the EgressIP object")
				_, err = oc.AsAdmin().Run("delete").Args("-f", tmpDirEgressIP+"/"+egressIPYaml).Output()
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Launching a new prober pod and probing for EgressIPs")
				clientIPSet, err = ProbeForClientIPs(oc, targetNamespace, probePodName, routeName, targetIP, targetPort, 10)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Making sure that the EgressIPs were not part of the response")
				framework.Logf("egressIPSet is: %v", egressIPSet)
				framework.Logf("clientIPSet is: %v", clientIPSet)
				o.Expect(AntiDeepEqual(egressIPSet, clientIPSet)).To(o.BeTrue())

				g.By("Applying the EgressIP object again")
				_, err = oc.AsAdmin().Run("create").Args("-f", tmpDirEgressIP+"/"+egressIPYaml).Output()
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Launching a new prober pod and probing for EgressIPs")
				clientIPSet, err = ProbeForClientIPs(oc, targetNamespace, probePodName, routeName, targetIP, targetPort, 10)
				o.Expect(err).NotTo(o.HaveOccurred())

				g.By("Making sure that all EgressIPs and only the EgressIPs were part of the response")
				framework.Logf("egressIPSet is: %v", egressIPSet)
				framework.Logf("clientIPSet is: %v", clientIPSet)
				o.Expect(reflect.DeepEqual(egressIPSet, clientIPSet)).To(o.BeTrue())
			})
		})
	})
})
