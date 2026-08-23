package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// readyProxyPod builds a pod the way the kubelet leaves one: Running, on a
// node, Ready. hostPort is what podspec.BuildProxyPod puts on the container
// under the HostPort strategy and leaves at zero under every other one
// (internal/podspec/proxy.go:227-229), which is the fact the fabrication case
// below turns on.
func readyProxyPod(hostIP string, hostPort int32) corev1.Pod {
	return corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "minecraft",
			Ports: []corev1.ContainerPort{{
				Name:          podspec.MinecraftPortName,
				ContainerPort: podspec.MinecraftPort,
				HostPort:      hostPort,
			}},
		}}},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			HostIP: hostIP,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
		},
	}
}

func notReadyProxyPod(hostIP string, hostPort int32) corev1.Pod {
	pod := readyProxyPod(hostIP, hostPort)
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	return pod
}

// nodePortService is what reconcileService leaves behind for a NodePort
// group: one port, named, with the node port the API server allocated.
func nodePortService(nodePort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{
				Name:       podspec.MinecraftPortName,
				Port:       podspec.MinecraftPort,
				TargetPort: intstr.FromString(podspec.MinecraftPortName),
				NodePort:   nodePort,
			}},
		},
	}
}

func clusterIPService() *corev1.Service {
	svc := nodePortService(0)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	return svc
}

func loadBalancerService(ingressIP, ingressHost string) *corev1.Service {
	svc := nodePortService(0)
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{
		IP: ingressIP, Hostname: ingressHost,
	}}
	return svc
}

func groupExposing(spec spawneryv1alpha1.ExposeSpec) *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{Spec: spawneryv1alpha1.ProxyGroupSpec{Expose: spec}}
}

func TestProxyAddressPublishesOnlyWhatIsObservablyRealised(t *testing.T) {
	nodePort := spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30765},
	}
	hostPort := spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
	}
	clusterIP := spawneryv1alpha1.ExposeSpec{
		Type:      spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
	}
	loadBalancer := spawneryv1alpha1.ExposeSpec{
		Type: spawneryv1alpha1.ExposeLoadBalancer,
	}

	cases := []struct {
		name string
		spec spawneryv1alpha1.ExposeSpec
		pods []corev1.Pod
		svc  *corev1.Service
		want string
		why  string
	}{
		{
			name: "NodePort publishes the port the API server allocated",
			spec: nodePort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  nodePortService(30765),
			want: "192.168.1.10:30765",
			why:  "a ready pod on a node, and a Service carrying the allocation",
		},
		{
			name: "NodePort reads the Service and not the spec",
			spec: nodePort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			// The spec asks for 30765; the API server allocated 31000. The
			// allocation is what a client can dial.
			svc:  nodePortService(31000),
			want: "192.168.1.10:31000",
			why:  "the allocation wins over the request",
		},
		{
			name: "NodePort publishes nothing once the Service is gone",
			spec: nodePort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  nil,
			want: "",
			why:  "no Service means no node port, whatever the pods say",
		},
		{
			name: "HostPort publishes a port a ready pod actually binds",
			spec: hostPort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 25565)},
			svc:  nil,
			want: "192.168.1.10:25565",
			why:  "HostPort creates no Service, so the pod is the whole evidence",
		},
		{
			// THE FABRICATION CASE. This is the one that fails before the
			// change. The spec has been switched to HostPort; the pods still
			// running are the NodePort generation, whose containers carry
			// HostPort == 0. Before this change proxyAddress took their HostIP
			// and appended the spec's 25565, publishing an address whose host
			// is real, whose port is real, and which no process on that node
			// is listening on.
			name: "HostPort publishes nothing while only the old strategy's pods are ready",
			spec: hostPort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  nil,
			want: "",
			why:  "no pod in existence binds 25565 on that node",
		},
		{
			// The CRD makes this unreachable -- HostPortSpec.Port is required
			// with Minimum=1 -- so the only caller that can produce it is a
			// test like this one. Without the guard, zero matches every pod
			// declaring no host port, which is every pod of every other
			// strategy, and the helper would hand back a node address for
			// `host:0`: the exact inversion of its purpose.
			name: "HostPort with a zero port publishes nothing rather than host:0",
			spec: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 0},
			},
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  nil,
			want: "",
			why:  "zero is not a port anything binds",
		},
		{
			name: "HostPort ignores a pod that binds the port but is not ready",
			spec: hostPort,
			pods: []corev1.Pod{notReadyProxyPod("192.168.1.10", 25565)},
			svc:  nil,
			want: "",
			why:  "the readiness gate is unchanged by this work",
		},
		{
			name: "ClusterIP echoes the address once the Service exists",
			spec: clusterIP,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  clusterIPService(),
			want: "mc.example.test",
			why:  "no port is appended; a client types the name and nothing else",
		},
		{
			name: "ClusterIP publishes nothing without the Service to route to",
			spec: clusterIP,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  nil,
			want: "",
			why:  "the fronting proxy routes to the Service; without it the name goes nowhere",
		},
		{
			name: "LoadBalancer publishes the ingress IP",
			spec: loadBalancer,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  loadBalancerService("203.0.113.7", ""),
			want: "203.0.113.7:25565",
			why:  "unchanged behaviour, pinned so this work cannot move it",
		},
		{
			name: "LoadBalancer falls back to the ingress hostname",
			spec: loadBalancer,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  loadBalancerService("", "lb.example.test"),
			want: "lb.example.test:25565",
			why:  "unchanged behaviour, pinned so this work cannot move it",
		},
		{
			name: "LoadBalancer publishes nothing before the address is assigned",
			spec: loadBalancer,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  loadBalancerService("", ""),
			want: "",
			why:  "unchanged behaviour, pinned so this work cannot move it",
		},
		{
			name: "no ready pod publishes nothing, whatever the strategy",
			spec: nodePort,
			pods: []corev1.Pod{notReadyProxyPod("192.168.1.10", 0)},
			svc:  nodePortService(30765),
			want: "",
			why:  "test/e2e/expose_test.go rests on exactly this",
		},
		{
			// The readiness gate has to be stated for this strategy rather
			// than inherited: a LoadBalancer's address comes from the Service,
			// which knows nothing about readiness, so without the gate
			// status.address would point somewhere the moment a load balancer
			// answered -- including for a group whose every pod is in
			// ImagePullBackOff.
			name: "LoadBalancer with an assigned address but no ready proxy",
			spec: loadBalancer,
			pods: []corev1.Pod{notReadyProxyPod("192.168.1.10", 0)},
			svc:  loadBalancerService("192.0.2.10", ""),
			want: "",
			why:  "an assigned address is not a serving proxy",
		},
		{
			// Same shape one strategy over, and the one test/e2e/expose_test.go
			// names as its backing: it asserts nothing about the ClusterIP
			// group's address, because no image resolves there and asserting an
			// empty string would be asserting the image tag rather than the
			// strategy.
			name: "ClusterIP publishes nothing until a proxy is ready",
			spec: clusterIP,
			pods: []corev1.Pod{notReadyProxyPod("192.168.1.10", 0)},
			svc:  clusterIPService(),
			want: "",
			why:  "the echoed name goes nowhere until something serves behind it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := proxyAddress(groupExposing(tc.spec), tc.pods, tc.svc)
			if got != tc.want {
				t.Errorf("proxyAddress = %q, want %q — %s", got, tc.want, tc.why)
			}
		})
	}
}
