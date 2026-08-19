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
	pod, err := BuildProxyPod(testNetwork(), testProxyGroup(), "gateway-abcd", testEndpoint, nil)
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
	configured, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint, nil)
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
	if _, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint, nil); err == nil {
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

// TestProxyPodConfigVolumeCarriesTheGroupConfigMapAndForwardingSecret mirrors
// the server builder's own config-volume test: the two builders share one
// helper for exactly this volume, and design intent (docs section 4.6) is
// that they cannot drift into different answers about where configuration
// lives.
func TestProxyPodConfigVolumeCarriesTheGroupConfigMapAndForwardingSecret(t *testing.T) {
	pod := buildProxy(t)

	var mounted bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == ConfigVolumeName {
			mounted = true
			if m.MountPath != ConfigMountPath {
				t.Errorf("mountPath = %q, want %q", m.MountPath, ConfigMountPath)
			}
			if !m.ReadOnly {
				t.Error("the config volume is writable")
			}
		}
	}
	if !mounted {
		t.Fatal("the config volume is not mounted into the container")
	}

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == ConfigVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Projected == nil {
		t.Fatalf("volume %q = %+v, want a projected source", ConfigVolumeName, vol)
	}
	if len(vol.Projected.Sources) != 2 {
		t.Fatalf("projected sources = %+v, want exactly 2 with no overlay declared", vol.Projected.Sources)
	}

	var sawGroupConfigMap, sawSecret bool
	for _, s := range vol.Projected.Sources {
		if s.ConfigMap != nil && s.ConfigMap.Name == GroupConfigMapName("gateway", RoleProxy) {
			sawGroupConfigMap = true
			if len(s.ConfigMap.Items) != 1 || s.ConfigMap.Items[0].Key != ConfigValuesKey || s.ConfigMap.Items[0].Path != "config.yaml" {
				t.Errorf("configMap items = %+v, want %s mapped to config.yaml", s.ConfigMap.Items, ConfigValuesKey)
			}
		}
		if s.Secret != nil && s.Secret.Name == "velocity-forwarding-secret" {
			sawSecret = true
			if len(s.Secret.Items) != 1 || s.Secret.Items[0].Key != ForwardingSecretKey || s.Secret.Items[0].Path != "forwarding.secret" {
				t.Errorf("secret items = %+v, want %s mapped to forwarding.secret", s.Secret.Items, ForwardingSecretKey)
			}
		}
	}
	if !sawGroupConfigMap {
		t.Errorf("sources = %+v, want a configMap source named %q", vol.Projected.Sources, GroupConfigMapName("gateway", RoleProxy))
	}
	if !sawSecret {
		t.Errorf("sources = %+v, want a secret source named velocity-forwarding-secret", vol.Projected.Sources)
	}
}

// TestProxyPodConfigOverlayIsAnUnfilteredVolumeNestedUnderTheConfigMount
// mirrors the server builder's own version of this test (see the long
// comment there for why): the overlay must be a separate, plain ConfigMap
// volume with no Items, not a third Projected source enumerating a closed
// set of known names — an enumerated list would drop an unrecognised key at
// the kubelet, before internal/render.checkOverlayFiles ever got a chance to
// refuse it by name.
func TestProxyPodConfigOverlayIsAnUnfilteredVolumeNestedUnderTheConfigMount(t *testing.T) {
	group := testProxyGroup()
	group.Spec.ConfigOverlay = &spawneryv1alpha1.ObjectRef{Name: "gateway-overlay"}
	pod, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint, nil)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}

	var base, overlay *corev1.Volume
	for i := range pod.Spec.Volumes {
		switch pod.Spec.Volumes[i].Name {
		case ConfigVolumeName:
			base = &pod.Spec.Volumes[i]
		case ConfigOverlayVolumeName:
			overlay = &pod.Spec.Volumes[i]
		}
	}
	if base == nil || base.Projected == nil || len(base.Projected.Sources) != 2 {
		t.Fatalf("volume %q = %+v, want exactly the 2 base sources — the overlay must not be folded in here", ConfigVolumeName, base)
	}
	if overlay == nil {
		t.Fatalf("volumes = %+v, want a %s volume", pod.Spec.Volumes, ConfigOverlayVolumeName)
	}
	if overlay.ConfigMap == nil {
		t.Fatalf("volume %q = %+v, want a plain configMap source, not Projected", ConfigOverlayVolumeName, overlay)
	}
	if overlay.ConfigMap.Name != "gateway-overlay" {
		t.Errorf("configMap name = %q, want gateway-overlay", overlay.ConfigMap.Name)
	}
	if len(overlay.ConfigMap.Items) != 0 {
		t.Errorf("items = %+v, want none: an enumerated list would filter out an unrecognised overlay key before internal/render ever sees it", overlay.ConfigMap.Items)
	}

	var mount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == ConfigOverlayVolumeName {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatal("the overlay volume is not mounted into the container")
	}
	if mount.MountPath != ConfigMountPath+"/overlay" {
		t.Errorf("mountPath = %q, want %q", mount.MountPath, ConfigMountPath+"/overlay")
	}
	if !mount.ReadOnly {
		t.Error("the overlay volume is writable")
	}
}

// TestProxyPodConfigOverlayVolumeIsAbsentWhenNoneIsDeclared guards the nil
// case: a ProxyGroup that sets no configOverlay must get neither the
// overlay volume nor its mount, and the base config volume keeps exactly
// its 2 sources.
func TestProxyPodConfigOverlayVolumeIsAbsentWhenNoneIsDeclared(t *testing.T) {
	pod := buildProxy(t)
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if v.Name == ConfigOverlayVolumeName {
			t.Errorf("volume %+v present with no configOverlay declared", v)
		}
		if v.Name == ConfigVolumeName && (v.Projected == nil || len(v.Projected.Sources) != 2) {
			t.Errorf("volume %q = %+v, want exactly 2 sources with no overlay declared", ConfigVolumeName, v)
		}
	}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == ConfigOverlayVolumeName {
			t.Errorf("mount %+v present with no configOverlay declared", m)
		}
	}
}

// The drain window is how long existing sessions may run out when a proxy is
// replaced. A grace period shorter than it would have the kubelet kill the
// process mid-drain.
func TestProxyPodGracePeriodComesFromTheDrainWindow(t *testing.T) {
	group := testProxyGroup()
	group.Spec.Drain = &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 120}
	pod, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint, nil)
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

func TestProxyPodCarriesTheFallbackGroups(t *testing.T) {
	net := testNetwork()
	group := testProxyGroup()
	group.Spec.Routing.FallbackGroups = []string{"lobby", "hub"}

	pod, err := BuildProxyPod(net, group, "edge-0", "operator:9443", nil)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}

	got := proxyEnv(pod, EnvFallbackGroups)
	if got != "lobby,hub" {
		t.Fatalf("%s = %q, want %q", EnvFallbackGroups, got, "lobby,hub")
	}
}

// A hostPort is the whole of what makes the HostPort strategy work without a
// Service, and it is also what makes the kube-scheduler refuse a second pod
// of the same group on one node. Setting it under any other strategy would
// impose that cap on groups that never asked for it.
func TestBuildProxyPodBindsAHostPortOnlyForThatStrategy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expose spawneryv1alpha1.ExposeSpec
		want   int32
	}{
		{
			name: "NodePort",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			want: 0,
		},
		{
			name: "LoadBalancer",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			want: 0,
		},
		{
			name: "HostPort",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			want: 25565,
		},
		{
			// The CEL rules on ExposeSpec forbid a NodePort object from also
			// carrying a hostPort sub-block, but a unit test builds the Go
			// struct directly and never goes through the API server's
			// validation. If the type check were ever dropped from the
			// condition below, this is the case that would catch it: every
			// other subtest here has HostPort == nil, so a mutation that
			// keys off presence alone would sail through them unnoticed.
			name: "NodePort with a stray HostPort sub-block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net := testNetwork()
			group := testProxyGroup()
			group.Spec.Expose = tc.expose

			pod, err := BuildProxyPod(net, group, "gateway-abcd", testEndpoint, nil)
			if err != nil {
				t.Fatalf("BuildProxyPod: %v", err)
			}

			var minecraft *corev1.ContainerPort
			for i := range pod.Spec.Containers[0].Ports {
				if pod.Spec.Containers[0].Ports[i].Name == MinecraftPortName {
					minecraft = &pod.Spec.Containers[0].Ports[i]
				}
			}
			if minecraft == nil {
				t.Fatalf("no port named %q on the container: %+v",
					MinecraftPortName, pod.Spec.Containers[0].Ports)
			}
			if minecraft.HostPort != tc.want {
				t.Errorf("hostPort = %d, want %d", minecraft.HostPort, tc.want)
			}
			// The ready port is the kubelet's probe target and is never
			// published on a node: a second host port would cap the group by
			// node count for a port no player dials.
			for _, p := range pod.Spec.Containers[0].Ports {
				if p.Name == ProxyReadyPortName && p.HostPort != 0 {
					t.Errorf("the ready port carries hostPort %d; it must never be published",
						p.HostPort)
				}
			}
		})
	}
}

// The hash is what makes a strategy switch roll the pods, and what makes a
// switch that changes nothing about the pods roll nothing. Both halves are
// asserted here because only the pair says what the field is for: without
// the equality, adding the strategy to the hash by any other means would
// pass while replacing every pod of a group that switched NodePort to
// LoadBalancer for no reason at all.
func TestDesiredProxyHashSeparatesHostPortFromTheServiceStrategies(t *testing.T) {
	hashFor := func(t *testing.T, expose spawneryv1alpha1.ExposeSpec) string {
		t.Helper()
		group := testProxyGroup()
		group.Spec.Expose = expose
		h, err := DesiredProxyHash(testNetwork(), group, testEndpoint, nil)
		if err != nil {
			t.Fatalf("DesiredProxyHash: %v", err)
		}
		return h
	}

	nodePort := hashFor(t, spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
	})
	loadBalancer := hashFor(t, spawneryv1alpha1.ExposeSpec{
		Type:         spawneryv1alpha1.ExposeLoadBalancer,
		LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
	})
	hostPort := hashFor(t, spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
	})

	if nodePort != loadBalancer {
		t.Errorf("NodePort and LoadBalancer hash differently (%s vs %s). Those two "+
			"differ only in the Service; rolling every pod for that switch would "+
			"disconnect players for nothing", nodePort, loadBalancer)
	}
	if hostPort == nodePort {
		t.Errorf("HostPort hashes the same as NodePort (%s). A group switched into "+
			"or out of HostPort would keep pods whose container ports no longer "+
			"match the strategy", hostPort)
	}
}
