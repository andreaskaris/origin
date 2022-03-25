package networking

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	exutil "github.com/openshift/origin/test/extended/util"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/kubernetes/test/e2e/framework"
	frameworkpod "k8s.io/kubernetes/test/e2e/framework/pod"
)

var (
	egressIPYamlTemplate1 = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: %s
spec:
    egressIPs: %s
    podSelector:
        matchLabels:
            %s
    namespaceSelector:
        matchLabels:
            %s`

	egressIPYamlTemplate2 = `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: %s
spec:
    egressIPs: %s
    namespaceSelector:
        matchLabels:
            %s`
)

const (
	nodeEgressIPConfigAnnotationKey = "cloud.network.openshift.io/egress-ipconfig"
	egressIPAgnImage                = "k8s.gcr.io/e2e-test-images/agnhost:2.36"
)

// TODO: make port allocator a singleton, shared among all test processes for egressip
// TODO: add an egressip allocator similar to the port allocator

// PortAllocator is a simple class to allocate ports serially.
type PortAllocator struct {
	minPort, maxPort int
	reservedPorts    map[int]struct{}
	nextPort         int
	numAllocated     int
	mu               sync.Mutex
}

// NewPortAllocator initialized a new object of type PortAllocator and returns
// a pointer to that object.
func NewPortAllocator(minPort, maxPort int) *PortAllocator {
	pa := PortAllocator{
		minPort:       minPort,
		maxPort:       maxPort,
		reservedPorts: make(map[int]struct{}),
		nextPort:      minPort,
		numAllocated:  0,
	}
	return &pa
}

// AllocateNextPort will allocate a new port, serially. If the end of the range is
// reached, start over again at the start of the range and look for gaps.
func (p *PortAllocator) AllocateNextPort() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	rangeSize := p.maxPort - p.minPort
	if p.numAllocated > rangeSize {
		return -1, fmt.Errorf("No more free ports to allocate")
	}

	for i := 0; i < rangeSize; i++ {
		if p.nextPort > p.maxPort || p.nextPort < p.minPort {
			p.nextPort = p.minPort
		}
		if p.allocatePort(p.nextPort) == nil {
			return p.nextPort, nil
		}
		p.nextPort++
	}

	return -1, fmt.Errorf("Cannot allocate new port after %d tries.", rangeSize)
}

// ReleasePort will release the allocatoin for port <port>.
func (p *PortAllocator) ReleasePort(port int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.reservedPorts[port]; !ok {
		return fmt.Errorf("Chosen port %d is not allocated", port)
	}

	delete(p.reservedPorts, port)
	p.numAllocated--

	return nil
}

// isPortFree is a helper method. Not to be used by its own.
func (p *PortAllocator) isPortFree(port int) bool {
	_, ok := p.reservedPorts[port]
	return !ok
}

// allocatemPort is a helper method. Not to be used by its own.
func (p *PortAllocator) allocatePort(port int) error {
	if port < p.minPort || port > p.maxPort {
		return fmt.Errorf("Chosen port %d is not part of valid range %d - %d",
			port, p.minPort, p.maxPort)
	}

	if _, ok := p.reservedPorts[port]; ok {
		return fmt.Errorf("Chosen port %d is already reserved", port)
	}

	p.reservedPorts[port] = struct{}{}
	p.numAllocated++

	return nil
}

type byName []corev1.Node

func (n byName) Len() int           { return len(n) }
func (n byName) Swap(i, j int)      { n[i], n[j] = n[j], n[i] }
func (n byName) Less(i, j int) bool { return n[i].Name < n[j].Name }

// GetNodesOrdered returns a sorted slice (by node.Name) of nodes or error.
func GetWorkerNodesOrdered(clientset kubernetes.Interface) ([]corev1.Node, error) {
	if clientset == nil {
		return nil, fmt.Errorf("Nil pointer clientset provided")
	}

	nodes, err := clientset.CoreV1().Nodes().List(
		context.TODO(),
		metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/worker=",
		})
	if err != nil {
		return nil, err
	}
	items := nodes.Items
	sort.Sort(byName(items))

	return items, nil
}

// DeleteDaemonSet deletes the Daemonset <namespace>/<dsName>.
func DeleteDaemonSet(clientset kubernetes.Interface, namespace, dsName string) error {
	deleteOptions := metav1.DeleteOptions{}
	if err := clientset.AppsV1().DaemonSets(namespace).Delete(context.TODO(), dsName, deleteOptions); err != nil {
		return fmt.Errorf("Failed to delete DaemonSet %s/%s: %v", namespace, dsName, err)
	}
	return nil
}

// CreateHostNetworkedDaemonSetAndProbe creates a host networked pod in namespace <namespace> on
// node <nodeName>. It will allocate a port to listen on and it will return
// the DaemonSet or an error.
func CreateHostNetworkedDaemonSet(clientset kubernetes.Interface, namespace, nodeName, daemonsetName string, containerPort int) (*appsv1.DaemonSet, error) {
	podCommand := []string{
		"/agnhost",
		"netexec",
		"--udp-port",
		"-1",
		"--http-port",
	}
	podCommand = append(podCommand, fmt.Sprintf("%d", containerPort))

	podLabels := map[string]string{
		"app": daemonsetName,
	}
	nodeSelector := map[string]string{"kubernetes.io/hostname": nodeName}
	containerPorts := []v1.ContainerPort{
		{
			Name:          fmt.Sprintf("port%d", containerPort),
			HostPort:      int32(containerPort),
			ContainerPort: int32(containerPort),
			Protocol:      v1.ProtocolTCP,
		},
	}
	readinessProbe := &v1.Probe{
		ProbeHandler: v1.ProbeHandler{
			HTTPGet: &v1.HTTPGetAction{
				Port: intstr.FromInt(int(containerPort)),
				Path: "/clientip",
			},
		},
	}

	dsDefinition := &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "DaemonSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      daemonsetName,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: podLabels,
				},
				Spec: corev1.PodSpec{
					NodeSelector: nodeSelector,
					HostNetwork:  true,
					Containers: []v1.Container{
						{
							Name:           daemonsetName,
							Image:          egressIPAgnImage,
							Command:        podCommand,
							Ports:          containerPorts,
							ReadinessProbe: readinessProbe,
						},
					},
				},
			},
		},
	}
	ds, err := clientset.AppsV1().DaemonSets(namespace).Create(context.TODO(), dsDefinition, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	return ds, nil
}

// CreateHostNetworkedDaemonSetAndProbe creates a host networked pod in namespace <namespace> on
// node <nodeName>. It will allocate a port to listen on and it will return
// the DaemonSet or an error. It will probe the container and return an error such as the custom
// "Port conflict when creating pod" error message when the pod failed due to port binding issues.
func CreateHostNetworkedDaemonSetAndProbe(clientset kubernetes.Interface, namespace, nodeName, daemonsetName string, port, pollInterval, retries int) (*appsv1.DaemonSet, error) {
	targetDaemonset, err := CreateHostNetworkedDaemonSet(
		clientset,
		namespace,
		nodeName,
		daemonsetName,
		port,
	)
	if err != nil {
		return targetDaemonset, err
	}

	var ds *appsv1.DaemonSet
	for i := 0; i < retries; i++ {
		// Get the DS
		ds, err = clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), daemonsetName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		// Check if NumberReady == DesiredNumberScheduled.
		// In that case, simply return as all went well.
		if ds.Status.NumberReady == ds.Status.DesiredNumberScheduled {
			return ds, nil
		}

		// Iterate over the pods (should only be one) and check if we couldn't spawn
		// because of duplicate assigned ports.
		// In case of a duplicate port conflict, we return an error message starting with
		// 'Port conflict when creating pod' so that other parts of the code can react to this.
		pods, err := clientset.CoreV1().Pods(namespace).List(
			context.TODO(),
			metav1.ListOptions{LabelSelector: labels.Set(ds.Spec.Selector.MatchLabels).String()})
		if err != nil {
			return nil, err
		}
		for _, pod := range pods.Items {
			hasPortConflict, err := podHasPortConflict(clientset, pod)
			if err != nil {
				return ds, err
			}
			if hasPortConflict {
				return ds, fmt.Errorf("Port conflict when creating pod %s/%s", namespace, pod.Name)
			}
		}

		// If no port conflict error was found, simply sleep for pollInterval and then
		// check again.
		time.Sleep(time.Duration(pollInterval) * time.Second)
	}

	// The DaemonSet is not ready, but this is not because of a port conflict.
	// This shouldn't happen and other parts of the code will likely report this error
	// as a CI failure.
	return ds, fmt.Errorf("Daemonset still not ready after %d tries", retries)
}

// podHasPortConflict scans the pod for a port conflict message and also scans the
// pod's logs for error messages that might indicate such a conflict.
func podHasPortConflict(clientset kubernetes.Interface, pod v1.Pod) (bool, error) {
	msg := "have free ports for the requested pod ports"
	if pod.Status.Phase == v1.PodPending {
		conditions := pod.Status.Conditions
		for _, condition := range conditions {
			if strings.Contains(condition.Message, msg) {
				return true, nil
			}

		}
	} else if pod.Status.Phase == v1.PodRunning {
		logOptions := corev1.PodLogOptions{}
		req := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &logOptions)
		logs, err := req.Stream(context.TODO())
		if err != nil {
			return false, fmt.Errorf("Error in opening log stream")
		}
		defer logs.Close()

		buf := new(bytes.Buffer)
		_, err = io.Copy(buf, logs)
		if err != nil {
			return false, fmt.Errorf("Error in copying info from pod logs to buffer")
		}
		logStr := buf.String()
		if strings.Contains(logStr, "address already in use") {
			return true, nil
		}
	}
	return false, nil
}

// GetDaemonSetPodIPs returns the IPs of all pods in the DaemonSet.
func GetDaemonSetPodIPs(clientset kubernetes.Interface, namespace, daemonsetName string) ([]string, error) {
	var ds *appsv1.DaemonSet
	var podIPs []string
	// Get the DS
	ds, err := clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), daemonsetName, metav1.GetOptions{})
	if err != nil {
		return []string{}, err
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(
		context.TODO(),
		metav1.ListOptions{LabelSelector: labels.Set(ds.Spec.Selector.MatchLabels).String()})
	if err != nil {
		return []string{}, err
	}
	for _, pod := range pods.Items {
		podIPs = append(podIPs, pod.Status.PodIP)
	}

	return podIPs, nil
}

func GetIngressDomain(oc *exutil.CLI) (string, error) {
	ic, err := oc.AdminOperatorClient().OperatorV1().IngressControllers("openshift-ingress-operator").Get(context.Background(), "default", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return ic.Status.Domain, nil
}

// CreateEgressIPTestSource creates the route, service and deployment that will be used as
// a source for EgressIP tests. Returns the route name that can be queried to run queries against the source pods.
func CreateEgressIPTestSource(oc *exutil.CLI, namespace, ingressDomain string, replicas int, scheduleOnHosts []string) (string, string, error) {
	f := oc.KubeFramework()
	clientset := f.ClientSet

	targetPort := 8000
	routeName := fmt.Sprintf("%s-route", namespace)
	routeHost := fmt.Sprintf("%s.%s", namespace, ingressDomain)
	serviceName := fmt.Sprintf("%s-service", namespace)
	deploymentName := fmt.Sprintf("%s-deployment", namespace)
	weight := int32(100)
	podLabels := map[string]string{
		"app": deploymentName,
	}
	podCommand := []string{
		"/agnhost",
		"netexec",
		"--udp-port",
		"-1",
		"--http-port",
		fmt.Sprintf("%d", targetPort),
	}
	replicaCount := int32(replicas)

	// create route
	routeDefinition := routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeName,
			Namespace: namespace,
		},
		Spec: routev1.RouteSpec{
			Host: routeHost,
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromInt(targetPort),
			},
			To: routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   serviceName,
				Weight: &weight,
			},
		},
	}
	_, err := oc.RouteClient().RouteV1().Routes(namespace).Create(context.TODO(), &routeDefinition, metav1.CreateOptions{})
	if err != nil {
		return "", "", err
	}

	// create service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceName,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": deploymentName,
			},
			Ports: []corev1.ServicePort{
				{
					Protocol: corev1.ProtocolTCP,
					Port:     int32(targetPort),
				},
			},
		},
	}
	_, err = clientset.CoreV1().Services(namespace).Create(
		context.Background(),
		service,
		metav1.CreateOptions{})
	if err != nil {
		return "", "", err
	}

	// create deployment
	nodeAffinity := v1.Affinity{
		NodeAffinity: &v1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchExpressions: []v1.NodeSelectorRequirement{
							{
								Key:      "kubernetes.io/hostname",
								Operator: v1.NodeSelectorOpIn,
								Values:   scheduleOnHosts,
							},
						},
					},
				},
			},
		},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: deploymentName,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Replicas: &replicaCount,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": deploymentName,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:    "agnhost",
							Image:   egressIPAgnImage,
							Command: podCommand,
						},
					},
					Affinity: &nodeAffinity,
				},
			},
		},
	}
	_, err = clientset.AppsV1().Deployments(namespace).Create(
		context.Background(),
		deployment,
		metav1.CreateOptions{})
	if err != nil {
		return "", "", err
	}

	// block until the deployment's pods are ready
	wait.PollImmediate(10*time.Second, 5*time.Minute, func() (bool, error) {
		framework.Logf("Verifying if deployment %s is ready ...", deploymentName)
		d, err := clientset.AppsV1().Deployments(namespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return d.Status.AvailableReplicas == *d.Spec.Replicas, nil
	})

	return deploymentName, routeHost, nil
}

// UpdateDeploymentAffinity updates the deployment's Affinity to match the scheduleOnHosts parameter and
// scales down and back up the replica count of the deployment.
func UpdateDeploymentAffinity(oc *exutil.CLI, namespace, deploymentName string, scheduleOnHosts []string) error {
	f := oc.KubeFramework()
	clientset := f.ClientSet

	// update deployment affinity
	nodeAffinity := v1.Affinity{
		NodeAffinity: &v1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchExpressions: []v1.NodeSelectorRequirement{
							{
								Key:      "kubernetes.io/hostname",
								Operator: v1.NodeSelectorOpIn,
								Values:   scheduleOnHosts,
							},
						},
					},
				},
			},
		},
	}

	var currentReplicaNumber int32
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get deployment
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(
			context.Background(),
			deploymentName,
			metav1.GetOptions{})
		if err != nil {
			return err
		}

		// update the affinity and lower the replica number to 0
		deployment.Spec.Template.Spec.Affinity = &nodeAffinity
		currentReplicaNumber = *deployment.Spec.Replicas
		deployment.Spec.Replicas = &currentReplicaNumber

		_, err = clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		return nil
	})
	if retryErr != nil {
		return fmt.Errorf("Update failed: %v", retryErr)
	}

	retryErr = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get deployment
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(
			context.Background(),
			deploymentName,
			metav1.GetOptions{})
		if err != nil {
			return err
		}

		// update the replica count back to what it used to be
		deployment.Spec.Replicas = &currentReplicaNumber

		_, err = clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		return nil
	})
	if retryErr != nil {
		return fmt.Errorf("Update failed: %v", retryErr)
	}

	// block until the deployment's pods are ready
	wait.PollImmediate(10*time.Second, 5*time.Minute, func() (bool, error) {
		framework.Logf("Verifying if deployment %s is ready ...", deploymentName)
		d, err := clientset.AppsV1().Deployments(namespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return d.Status.AvailableReplicas == *d.Spec.Replicas, nil
	})

	return nil
}

// from github.com/openshift/cloud-network-config-controller/pkg/cloudprovider/cloudprovider.go
type NodeEgressIPConfiguration struct {
	Interface string   `json:"interface"`
	IFAddr    ifAddr   `json:"ifaddr"`
	Capacity  capacity `json:"capacity"`
}

type ifAddr struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

type capacity struct {
	IPv4 int `json:"ipv4,omitempty"`
	IPv6 int `json:"ipv6,omitempty"`
	IP   int `json:"ip,omitempty"`
}

// FindNodeEgressIPs will return a list of available EgressIPs in a map <nodeName>:<egressIP>.
// The returned EgressIPs are chosen from the nodes' cloud.network.openshift.io/egress-ipconfig annotation and they
// depend on the current cloud type, on the currenctly used cloudprivateipconfigs, and on an internal reservation
// manager.
func FindNodeEgressIPs(oc *exutil.CLI, nodeNames []string, cloudType configv1.PlatformType) (map[string]string, error) {
	f := oc.KubeFramework()
	clientset := f.ClientSet

	// Get the node API objects corresponding to the node names.
	var nodeList []*v1.Node
	for _, nodeName := range nodeNames {
		node, err := clientset.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		nodeList = append(nodeList, node)
	}

	// Build the list of reserved IPs. To do so, look at the currently used cloudprivateipconfigs
	// and egressips.
	var reservedIPs []string
	reservedIPs, err := BuildReservedEgressIPList(oc)
	if err != nil {
		return nil, err
	}

	// For each node, get the node's Egress IP range (annotation cloud.network.openshift.io/egress-ipconfig).
	// Then, get the first free suitable IP address for this node and add the mapping <node name>:<ip address>
	// to the map.
	nodeEgressIPs := make(map[string]string)
	for _, node := range nodeList {
		nodeEgressIPConfigs, err := GetNodeEgressIPConfiguration(node)
		if err != nil {
			return nil, err
		}
		if l := len(nodeEgressIPConfigs); l != 1 {
			return nil, fmt.Errorf("Unexpect length of slice for node egress IP configuration: %d", l)
		}
		// todo - not ready for dualstack
		ipnetStr := nodeEgressIPConfigs[0].IFAddr.IPv4
		if ipnetStr == "" {
			ipnetStr = nodeEgressIPConfigs[0].IFAddr.IPv6
		}
		freeIP, err := GetFirstFreeIP(ipnetStr, reservedIPs, cloudType)
		if err != nil {
			return nil, err
		}
		nodeEgressIPs[node.Name] = freeIP
	}

	return nodeEgressIPs, nil
}

// BuildReservedEgressIPList builds the list of reserved IPs. To do so, look at the currently used cloudprivateipconfigs
// and egressips.
// TODO: add an internal reservation system based on a singleton to avoid race conditions during
// concurrent tests.
// TODO: replace with actual library to access cloudprivateipconfigs and egressips - possibly
// add to the networkclient
func BuildReservedEgressIPList(oc *exutil.CLI) ([]string, error) {
	var reservedIPs []string

	out, err := oc.AsAdmin().Run("get").Args("-o", "name", "cloudprivateipconfigs").Output()
	if err != nil {
		return nil, err
	}
	outReader := bufio.NewScanner(strings.NewReader(out))
	re := regexp.MustCompile("^cloudprivateipconfig.cloud.network.openshift.io/(.*)")
	for outReader.Scan() {
		match := re.FindSubmatch([]byte(outReader.Text()))
		if len(match) != 2 {
			continue
		}
		reservedIPs = append(reservedIPs, string(match[1]))
	}
	var existingEgressIPs []string
	out, err = oc.AsAdmin().Run("get").Args("-o", "name", "egressip").Output()
	if err != nil {
		return nil, err
	}
	outReader = bufio.NewScanner(strings.NewReader(out))
	re = regexp.MustCompile("^egressip.k8s.ovn.org/(.*)")
	for outReader.Scan() {
		match := re.FindSubmatch([]byte(outReader.Text()))
		if len(match) != 2 {
			continue
		}
		existingEgressIPs = append(existingEgressIPs, string(match[1]))
	}
	for _, existingEgressIP := range existingEgressIPs {
		out, err = oc.AsAdmin().Run("get").Args("egressip", existingEgressIP, "-o", "jsonpath={.spec.egressIPs}").Output()
		if err != nil {
			return nil, err
		}
		var existingEgressIPList []string
		err = json.Unmarshal([]byte(out), &existingEgressIPList)
		if err != nil {
			return nil, err
		}
		for _, ip := range existingEgressIPList {
			reservedIPs = append(reservedIPs, ip)
		}
	}

	return reservedIPs, nil
}

// GetFirstFreeIP returns the first available IP address from the IP network (CIDR notation). reservedIPs are
// eliminated from the choice and the cloudType is taken into account.
func GetFirstFreeIP(ipnetStr string, reservedIPs []string, cloudType configv1.PlatformType) (string, error) {
	// Parse the CIDR notation and enumerate all IPs inside the subnet.
	_, ipnet, err := net.ParseCIDR(ipnetStr)
	if err != nil {
		return "", err
	}
	ipList, err := SubnetIPs(*ipnet)
	if err != nil {
		return "", err
	}

	// For AWS, skip the first 5 addresses
	// https://stackoverflow.com/questions/64212709/how-do-i-assign-an-ec2-instance-to-a-fixed-ip-address-within-a-subnet
	if cloudType == configv1.AWSPlatformType {
		if len(ipList) < 6 {
			return "", fmt.Errorf("Cloud type is AWS, but there are less than 6 IPs available in the IP network %s", ipnetStr)
		}
		ipList = ipList[5:]
	}

	// Eliminate reserved IPs and return the first hit
outer:
	for _, ip := range ipList {
		for _, rip := range reservedIPs {
			if ip.String() == rip {
				continue outer
			}
		}
		return ip.String(), nil
	}

	return "", fmt.Errorf("No suitable IP found in ipnet: %s, reserved IPs %v", ipnetStr, reservedIPs)
}

// GetNodeEgressIPConfig returns the parsed NodeEgressIPConfiguration for the node.
func GetNodeEgressIPConfiguration(node *corev1.Node) ([]*NodeEgressIPConfiguration, error) {
	annotation, ok := node.Annotations[nodeEgressIPConfigAnnotationKey]
	if !ok {
		return nil, fmt.Errorf("Cannot find annotation %s on node %s", nodeEgressIPConfigAnnotationKey, node)
	}

	var nodeEgressIPConfigs []*NodeEgressIPConfiguration
	err := json.Unmarshal([]byte(annotation), &nodeEgressIPConfigs)
	if err != nil {
		return nil, err
	}

	return nodeEgressIPConfigs, nil
}

// ProbeForClientIPs spawns a prober pod inside the prober namespace. It then runs curl against http://%s/dial?host=%s&port=%d&request=/clientip
// for the specified number of iterations and returns a set of the clientIP addresses that were returned.
// At the end of the test, the prober pod is deleted again.
func ProbeForClientIPs(oc *exutil.CLI, proberPodNamespace, proberPodName, url, targetIP string, targetPort, iterations int) (map[string]struct{}, error) {
	f := oc.KubeFramework()
	clientset := f.ClientSet

	clientIpSet := make(map[string]struct{})

	proberPod := frameworkpod.CreateExecPodOrFail(clientset, proberPodNamespace, probePodName, func(pod *corev1.Pod) {
		// pod.ObjectMeta.Annotations = annotation
	})
	request := fmt.Sprintf("http://%s/dial?host=%s&port=%d&request=/clientip", url, targetIP, targetPort)
	for i := 0; i < iterations; i++ {
		output, err := oc.AsAdmin().Run("exec").Args(proberPod.Name, "--", "curl", "-s", request).Output()
		if err != nil {
			continue
		}
		dialResponse := &struct {
			Responses []string
		}{}
		err = json.Unmarshal([]byte(output), dialResponse)
		if err != nil {
			continue
		}
		if len(dialResponse.Responses) != 1 {
			continue
		}
		clientIpPort := strings.Split(dialResponse.Responses[0], ":")
		if len(clientIpPort) != 2 {
			continue
		}
		clientIp := clientIpPort[0]
		clientIpSet[clientIp] = struct{}{}
	}

	// delete the exec pod again - in foreground, so that it blocks
	deletePolicy := metav1.DeletePropagationForeground
	if err := clientset.CoreV1().Pods(proberPod.Namespace).Delete(context.TODO(), proberPod.Name, metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}); err != nil {
		return nil, err
	}

	return clientIpSet, nil
}
