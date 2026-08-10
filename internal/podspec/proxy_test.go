/*
Copyright The Spawnery Authors.

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

package podspec

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func testProxyGroup() *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "minecraft", UID: "pg-uid"},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Replicas:   2,
			Image:      "ghcr.io/spawnery/velocity:3.4.0-0.2.0",
			Expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			Routing: spawneryv1alpha1.RoutingSpec{FallbackGroups: []string{"lobby"}},
		},
	}
}

func buildProxy(t *testing.T) *corev1.Pod {
	t.Helper()
	pod, err := BuildProxyPod(testNetwork(), testProxyGroup(), "gateway-abcd", testEndpoint)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	return pod
}

func proxyEnv(pod *corev1.Pod, name string) string {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// The probe is the ready gate of design 6.6. It has to be a tcpSocket on a
// port of its own: Velocity speaks no HTTP, and an exec probe would need
// another binary in the image and another thing to keep reproducible. The
// agent binds this port only after it has processed its first FullSync, so a
// proxy cannot go green without a server list.
func TestProxyPodReadinessProbeIsTheAgentsOwnPort(t *testing.T) {
	pod := buildProxy(t)
	probe := pod.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.TCPSocket == nil {
		t.Fatalf("readiness probe = %+v, want a tcpSocket", probe)
	}
	if probe.TCPSocket.Port.IntVal != ProxyReadyPort {
		t.Errorf("probe port = %v, want %d", probe.TCPSocket.Port, ProxyReadyPort)
	}
	if pod.Spec.Containers[0].LivenessProbe != nil {
		t.Error("a liveness probe would restart the container and kick every player on it")
	}
}

// The player limit is not cosmetic: the agent reports it as slots, and the
// registry discards any report above it. A group that sets none must still
// produce a workable number.
func TestProxyPodCarriesAPlayerLimit(t *testing.T) {
	pod := buildProxy(t)
	if got := proxyEnv(pod, EnvPlayerLimit); got != "500" {
		t.Errorf("%s = %q, want the default %d", EnvPlayerLimit, got, DefaultPlayerLimit)
	}

	group := testProxyGroup()
	group.Spec.Config = &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 120}
	configured, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	if got := proxyEnv(configured, EnvPlayerLimit); got != "120" {
		t.Errorf("%s = %q, want the group's 120", EnvPlayerLimit, got)
	}
}

func TestProxyPodHasNoServerLabel(t *testing.T) {
	pod := buildProxy(t)
	if _, ok := pod.Labels[LabelServer]; ok {
		// The orphan sweep keys on its absence to tell the two kinds of pod
		// apart once it lists by managed-by alone.
		t.Error("a proxy pod must carry no server label")
	}
	if pod.Labels[LabelRole] != RoleProxy {
		t.Errorf("role label = %q, want %q", pod.Labels[LabelRole], RoleProxy)
	}
	if pod.Labels[LabelGroup] != "gateway" {
		t.Errorf("group label = %q, want gateway", pod.Labels[LabelGroup])
	}
}

// The pod has no CR of its own, so the group is what deletion cascades from.
func TestProxyPodIsOwnedByItsGroup(t *testing.T) {
	pod := buildProxy(t)
	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("owner references = %+v, want exactly the ProxyGroup", pod.OwnerReferences)
	}
	owner := pod.OwnerReferences[0]
	if owner.Kind != "ProxyGroup" || owner.Name != "gateway" || owner.UID != "pg-uid" {
		t.Errorf("owner = %+v, want the ProxyGroup gateway", owner)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Error("the ProxyGroup must be the controlling owner")
	}
}

// The network's defaults reach the proxy layer too — a nodeSelector that keeps
// game servers off the control plane has to keep proxies off it as well.
func TestProxyPodInheritsTheNetworkDefaults(t *testing.T) {
	pod := buildProxy(t)
	if pod.Spec.NodeSelector["node-role/minecraft"] != "true" {
		t.Errorf("NodeSelector = %v, want the network default", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "registry-credentials" {
		t.Errorf("ImagePullSecrets = %v, want the network default", pod.Spec.ImagePullSecrets)
	}
	if pod.Spec.Containers[0].Resources.Requests.Cpu().String() != "1" {
		t.Errorf("cpu request = %v, want the network default", pod.Spec.Containers[0].Resources.Requests.Cpu())
	}
}

func TestProxyPodRefusesAGroupWithoutAnImage(t *testing.T) {
	group := testProxyGroup()
	group.Spec.Image = ""
	if _, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint); err == nil {
		t.Fatal("a ProxyGroup with no image was accepted")
	}
}

func TestProxyPodMountsTheAgentCredentialsAndNothingElse(t *testing.T) {
	pod := buildProxy(t)
	if pod.Spec.ServiceAccountName != ProxyServiceAccountName {
		t.Errorf("ServiceAccountName = %q, want %q", pod.Spec.ServiceAccountName, ProxyServiceAccountName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("AutomountServiceAccountToken must be false: the pod carries no Kubernetes credentials")
	}

	var projected *corev1.Volume
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		// No PVC: proxies hold no state worth keeping, and the cross-proxy
		// player state that would need one is deferred by the main design.
		if v.PersistentVolumeClaim != nil {
			t.Errorf("volume %q is a PVC", v.Name)
		}
		if v.Name == AgentVolumeName {
			projected = v
		}
	}
	if projected == nil || projected.Projected == nil {
		t.Fatalf("no projected agent volume among %+v", pod.Spec.Volumes)
	}
	if len(projected.Projected.Sources) != 2 {
		t.Fatalf("projected sources = %+v, want the token and the CA", projected.Projected.Sources)
	}
	token := projected.Projected.Sources[0].ServiceAccountToken
	if token == nil || token.Audience != AgentTokenAudience {
		t.Errorf("token projection = %+v, want audience %q", token, AgentTokenAudience)
	}
}

func TestProxyPodExposesBothPorts(t *testing.T) {
	pod := buildProxy(t)
	ports := map[string]int32{}
	for _, p := range pod.Spec.Containers[0].Ports {
		ports[p.Name] = p.ContainerPort
	}
	if ports[MinecraftPortName] != MinecraftPort {
		t.Errorf("minecraft port = %d, want %d", ports[MinecraftPortName], MinecraftPort)
	}
	if ports[ProxyReadyPortName] != ProxyReadyPort {
		t.Errorf("ready port = %d, want %d", ports[ProxyReadyPortName], ProxyReadyPort)
	}
}

// The drain window is how long existing sessions may run out when a proxy is
// replaced. A grace period shorter than it would have the kubelet kill the
// process mid-drain.
func TestProxyPodGracePeriodComesFromTheDrainWindow(t *testing.T) {
	group := testProxyGroup()
	group.Spec.Drain = &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 120}
	pod, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	if pod.Spec.TerminationGracePeriodSeconds == nil || *pod.Spec.TerminationGracePeriodSeconds != 120 {
		t.Errorf("grace period = %v, want 120", pod.Spec.TerminationGracePeriodSeconds)
	}

	unset := buildProxy(t) // testProxyGroup sets no drain block
	if unset.Spec.TerminationGracePeriodSeconds == nil ||
		*unset.Spec.TerminationGracePeriodSeconds != int64(DefaultDrainTimeoutSeconds) {
		t.Errorf("grace period without a drain block = %v, want the default %d",
			unset.Spec.TerminationGracePeriodSeconds, DefaultDrainTimeoutSeconds)
	}
}
