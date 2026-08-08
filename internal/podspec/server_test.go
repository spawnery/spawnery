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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// testEndpoint is what the operator would pass in; the tests below assert it
// arrives in the container unchanged.
const testEndpoint = "spawnery-operator.spawnery-system.svc:9443"

func testNetwork() *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "minecraft"},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
			Defaults: &spawneryv1alpha1.Defaults{
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry-credentials"}},
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
				Scheduling: &spawneryv1alpha1.Scheduling{
					NodeSelector: map[string]string{"node-role/minecraft": "true"},
				},
			},
		},
	}
}

func testGroup() *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:                          spawneryv1alpha1.ServerGroupEphemeral,
			Image:                         "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:                    100,
			TerminationGracePeriodSeconds: 60,
			Scaling: &spawneryv1alpha1.ScalingSpec{
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40,
			},
		},
	}
}

func testServer() *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name: "lobby-x7k2", Namespace: "minecraft", UID: "server-uid",
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:        spawneryv1alpha1.ObjectRef{Name: "lobby"},
			GroupGeneration: 7,
		},
	}
}

func build(t *testing.T, mutate func(*spawneryv1alpha1.Network, *spawneryv1alpha1.ServerGroup)) *corev1.Pod {
	t.Helper()
	net, group := testNetwork(), testGroup()
	if mutate != nil {
		mutate(net, group)
	}
	pod, err := BuildServerPod(net, group, testServer(), testEndpoint)
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}
	return pod
}

func TestPodIdentity(t *testing.T) {
	pod := build(t, nil)

	if pod.Name != "lobby-x7k2" || pod.Namespace != "minecraft" {
		t.Errorf("pod identity = %s/%s, want minecraft/lobby-x7k2", pod.Namespace, pod.Name)
	}
	want := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   "production",
		LabelGroup:     "lobby",
		LabelServer:    "lobby-x7k2",
		LabelRole:      RoleServer,
	}
	for k, v := range want {
		if pod.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, pod.Labels[k], v)
		}
	}
	if pod.Labels[LabelOccupied] != "" {
		t.Error("the occupied label is maintained by the controller, not by the builder")
	}
	if len(pod.OwnerReferences) != 1 ||
		pod.OwnerReferences[0].Kind != "Server" ||
		pod.OwnerReferences[0].Name != "lobby-x7k2" ||
		pod.OwnerReferences[0].Controller == nil || !*pod.OwnerReferences[0].Controller {
		t.Errorf("owner reference = %+v, want a controller ref to Server/lobby-x7k2", pod.OwnerReferences)
	}
	if pod.Annotations[AnnotationSafeToEvict] != "false" {
		t.Errorf("%s = %q, want false", AnnotationSafeToEvict, pod.Annotations[AnnotationSafeToEvict])
	}
}

func TestPodCarriesNoKubernetesCredentials(t *testing.T) {
	pod := build(t, nil)
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be false")
	}
}

func TestPodIsRestrictedCompliant(t *testing.T) {
	pod := build(t, nil)

	if pod.Spec.SecurityContext == nil ||
		pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("runAsNonRoot must be true")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil ||
		pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccompProfile must be RuntimeDefault")
	}

	c := pod.Spec.Containers[0]
	if c.SecurityContext == nil {
		t.Fatal("container security context missing")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must be true")
	}
	if c.SecurityContext.Capabilities == nil ||
		len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities = %+v, want drop ALL", c.SecurityContext.Capabilities)
	}
}

func TestPodWritableDirectories(t *testing.T) {
	pod := build(t, nil)

	mounts := map[string]bool{}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		mounts[m.MountPath] = true
	}
	for _, path := range []string{"/data", "/tmp"} {
		if !mounts[path] {
			t.Errorf("%s is not mounted; with a read-only root the server cannot write", path)
		}
	}
}

func TestPodReadinessProbeIsAnSLPCheck(t *testing.T) {
	pod := build(t, nil)

	probe := pod.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.Exec == nil {
		t.Fatalf("readiness probe = %+v, want an exec probe", probe)
	}
	if probe.Exec.Command[0] != SLPHealthBinary {
		t.Errorf("probe command = %v, want %s first — a tcpSocket check turns green before the world is loaded",
			probe.Exec.Command, SLPHealthBinary)
	}
	if pod.Spec.Containers[0].LivenessProbe != nil {
		t.Error("no liveness probe: a restart would kick every player on the server")
	}
}

func TestPodPort(t *testing.T) {
	pod := build(t, nil)

	ports := pod.Spec.Containers[0].Ports
	if len(ports) != 1 || ports[0].ContainerPort != MinecraftPort || ports[0].Name != MinecraftPortName {
		t.Errorf("ports = %+v, want a single %s port %d", ports, MinecraftPortName, MinecraftPort)
	}
	if ports[0].HostPort != 0 {
		t.Error("server pods never bind a host port; only proxies are exposed")
	}
}

func TestNetworkDefaultsAreInherited(t *testing.T) {
	pod := build(t, nil)

	if pod.Spec.NodeSelector["node-role/minecraft"] != "true" {
		t.Errorf("nodeSelector = %v, want the network default", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "registry-credentials" {
		t.Errorf("imagePullSecrets = %v, want the network default", pod.Spec.ImagePullSecrets)
	}
	if got := pod.Spec.Containers[0].Resources.Requests.Cpu(); got.String() != "1" {
		t.Errorf("cpu request = %s, want the network default 1", got)
	}
}

func TestGroupOverridesNetworkDefaults(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Scheduling = &spawneryv1alpha1.Scheduling{
			NodeSelector: map[string]string{"node-role/minigames": "true"},
		}
		g.Spec.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
		}
	})

	if _, ok := pod.Spec.NodeSelector["node-role/minecraft"]; ok {
		t.Error("group scheduling must replace the network default, not merge with it")
	}
	if pod.Spec.NodeSelector["node-role/minigames"] != "true" {
		t.Errorf("nodeSelector = %v, want the group override", pod.Spec.NodeSelector)
	}
	if got := pod.Spec.Containers[0].Resources.Requests.Cpu(); got.String() != "4" {
		t.Errorf("cpu request = %s, want the group override 4", got)
	}
}

func TestUserMounts(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Mounts = []spawneryv1alpha1.Mount{{
			Name:      "lobby-config",
			MountPath: "/data/config",
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "lobby-config"},
			},
		}}
	})

	var found bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "lobby-config" && m.MountPath == "/data/config" && m.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Errorf("volumeMounts = %+v, want a read-only lobby-config at /data/config",
			pod.Spec.Containers[0].VolumeMounts)
	}

	for _, v := range pod.Spec.Volumes {
		if v.Name == "lobby-config" {
			if v.ConfigMap == nil || v.ConfigMap.Name != "lobby-config" {
				t.Errorf("volume lobby-config = %+v, want the configMap source", v)
			}
			return
		}
	}
	t.Errorf("volumes = %+v, want a lobby-config volume", pod.Spec.Volumes)
}

func TestEnvironment(t *testing.T) {
	pod := build(t, nil)

	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["SPAWNERY_NETWORK"] != "production" ||
		env["SPAWNERY_GROUP"] != "lobby" ||
		env["SPAWNERY_SERVER"] != "lobby-x7k2" ||
		env["SPAWNERY_MAX_PLAYERS"] != "100" {
		t.Errorf("env = %v, want network, group, server and max players", env)
	}
}

func TestTerminationGracePeriod(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.TerminationGracePeriodSeconds = 300
	})
	if pod.Spec.TerminationGracePeriodSeconds == nil || *pod.Spec.TerminationGracePeriodSeconds != 300 {
		t.Errorf("terminationGracePeriodSeconds = %v, want 300", pod.Spec.TerminationGracePeriodSeconds)
	}
}

func TestPersistentGroupMountsItsPVC(t *testing.T) {
	net := testNetwork()
	group := testGroup()
	group.Name = "survival"
	group.Spec.Type = spawneryv1alpha1.ServerGroupPersistent
	group.Spec.Scaling = nil
	group.Spec.Replicas = ptr.To[int32](1)
	group.Spec.Storage = &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("20Gi")}

	srv := testServer()
	srv.Name = "survival-0"
	srv.Spec.GroupRef.Name = "survival"
	srv.Spec.Ordinal = ptr.To[int32](0)

	pod, err := BuildServerPod(net, group, srv, testEndpoint)
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}

	for _, v := range pod.Spec.Volumes {
		if v.Name == DataVolumeName {
			if v.PersistentVolumeClaim == nil {
				t.Fatalf("data volume = %+v, want a PVC source for a persistent group", v)
			}
			if v.PersistentVolumeClaim.ClaimName != "survival-0-data" {
				t.Errorf("claimName = %q, want survival-0-data", v.PersistentVolumeClaim.ClaimName)
			}
			return
		}
	}
	t.Errorf("volumes = %+v, want a %s volume", pod.Spec.Volumes, DataVolumeName)
}

func TestBuildRejectsAnEmptyImage(t *testing.T) {
	net, group := testNetwork(), testGroup()
	group.Spec.Image = ""
	if _, err := BuildServerPod(net, group, testServer(), testEndpoint); err == nil {
		t.Fatal("empty image accepted, want an error")
	}
}

func TestBuildRejectsAnEmptyAgentEndpoint(t *testing.T) {
	net, group := testNetwork(), testGroup()
	if _, err := BuildServerPod(net, group, testServer(), ""); err == nil {
		t.Fatal("empty agent endpoint accepted, want an error")
	}
}

func TestPodCarriesTheProjectedAgentToken(t *testing.T) {
	pod := build(t, nil)

	var projected *corev1.ProjectedVolumeSource
	for _, v := range pod.Spec.Volumes {
		if v.Name == AgentVolumeName {
			projected = v.Projected
		}
	}
	if projected == nil {
		t.Fatal("the pod has no projected agent volume")
	}

	var sawToken, sawCA bool
	for _, src := range projected.Sources {
		if sa := src.ServiceAccountToken; sa != nil {
			sawToken = true
			if sa.Audience != AgentTokenAudience {
				t.Errorf("audience = %q, want %q", sa.Audience, AgentTokenAudience)
			}
			if sa.ExpirationSeconds == nil || *sa.ExpirationSeconds != TokenExpirationSeconds {
				t.Errorf("expirationSeconds = %v, want %d", sa.ExpirationSeconds, TokenExpirationSeconds)
			}
			if sa.Path != AgentTokenPath {
				t.Errorf("token path = %q, want %q", sa.Path, AgentTokenPath)
			}
		}
		if cm := src.ConfigMap; cm != nil {
			sawCA = true
			if cm.Name != CAConfigMapName {
				t.Errorf("configmap = %q, want %q", cm.Name, CAConfigMapName)
			}
		}
	}
	if !sawToken || !sawCA {
		t.Errorf("token=%v ca=%v, want both in one volume", sawToken, sawCA)
	}
}

func TestPodUsesTheServerServiceAccountButNoAutomount(t *testing.T) {
	pod := build(t, nil)

	if pod.Spec.ServiceAccountName != ServerServiceAccountName {
		t.Errorf("serviceAccountName = %q, want %q",
			pod.Spec.ServiceAccountName, ServerServiceAccountName)
	}
	// The claim "these pods carry no Kubernetes credentials" only holds with
	// automount off; the projected, audience-bound token is the exception.
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken is not off")
	}
}

func TestContainerKnowsWhereToReachTheOperator(t *testing.T) {
	pod := build(t, nil)
	c := pod.Spec.Containers[0]

	var endpoint string
	for _, e := range c.Env {
		if e.Name == EnvOperatorEndpoint {
			endpoint = e.Value
		}
	}
	if endpoint != testEndpoint {
		t.Errorf("%s = %q", EnvOperatorEndpoint, endpoint)
	}

	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.Name == AgentVolumeName {
			mounted = true
			if m.MountPath != AgentMountPath {
				t.Errorf("mountPath = %q, want %q", m.MountPath, AgentMountPath)
			}
			if !m.ReadOnly {
				t.Error("the agent volume is writable")
			}
		}
	}
	if !mounted {
		t.Error("the agent volume is not mounted into the container")
	}
}

// A user mount must never shadow the token, and the API server's generic
// rejection is no substitute for the operator saying what is wrong.
func TestCollidingUserMountsAreRefused(t *testing.T) {
	cases := []struct {
		name  string
		mount spawneryv1alpha1.Mount
		want  string
	}{
		{
			name: "same volume name as the agent volume",
			mount: spawneryv1alpha1.Mount{
				Name:      AgentVolumeName,
				MountPath: "/irgendwo",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: AgentVolumeName,
		},
		{
			name: "mounted over the agent path",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: AgentMountPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: AgentMountPath,
		},
		{
			name: "mounted over /data",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: DataMountPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: DataMountPath,
		},
		{
			// The case the check exists for: a mount nested inside the agent
			// volume can shadow the exact file the agent reads its token
			// from, and Kubernetes permits nested mounts without complaint.
			name: "nested inside the agent mount, shadowing the token file",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: AgentMountPath + "/" + AgentTokenPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: AgentMountPath,
		},
		{
			name: "a trailing slash on a reserved path is still the same path",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: DataMountPath + "/",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: DataMountPath,
		},
		{
			name: "mounted over /tmp",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: TmpMountPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: TmpMountPath,
		},
		{
			name: "a trailing slash on /tmp is still the same path",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: TmpMountPath + "/",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: TmpMountPath,
		},
		{
			// The reverse nesting: a mount over a parent directory of one of
			// ours sits above it, not beside it.
			name: "mounted over a parent of the agent mount",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: "/var/run",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: AgentMountPath,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net, group := testNetwork(), testGroup()
			group.Spec.Mounts = []spawneryv1alpha1.Mount{tc.mount}

			_, err := BuildServerPod(net, group, testServer(), testEndpoint)
			if err == nil {
				t.Fatal("BuildServerPod accepted a colliding mount")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestNonCollidingUserMountsAreAccepted guards the two ways the collision
// check must stay permissive: a path nested under DataMountPath or
// TmpMountPath, which is a feature and not a collision (see the comment on
// checkMountCollision), and a sibling path that merely shares a textual
// prefix with a reserved one, which a naive strings.HasPrefix check would
// wrongly reject.
func TestNonCollidingUserMountsAreAccepted(t *testing.T) {
	cases := []struct {
		name      string
		mountPath string
	}{
		{
			// Design spec 4.3's own ServerGroup example: a ConfigMap mounted
			// at DataMountPath+"/config" to add server config files. If this
			// case is ever removed as "redundant with TestUserMounts", it is
			// not — this one specifically exercises checkMountCollision, the
			// other exercises the resulting volume and mount.
			name:      "a config file nested inside /data, the documented pattern",
			mountPath: DataMountPath + "/config",
		},
		{
			name:      "a sibling directory that only shares a prefix with /data",
			mountPath: DataMountPath + "-extra",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net, group := testNetwork(), testGroup()
			group.Spec.Mounts = []spawneryv1alpha1.Mount{{
				Name:      "eigenes",
				MountPath: tc.mountPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			}}

			if _, err := BuildServerPod(net, group, testServer(), testEndpoint); err != nil {
				t.Fatalf("BuildServerPod rejected mount path %q: %v", tc.mountPath, err)
			}
		})
	}
}
