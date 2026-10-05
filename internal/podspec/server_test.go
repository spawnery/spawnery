/*
Copyright paul_wtf.

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
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/render"
)

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
	assertFSGroup(t, pod)

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

// assertFSGroup can only observe what the pod spec asks for; envtest runs no
// kubelet, so the chown itself is not tested.
func assertFSGroup(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	sc := pod.Spec.SecurityContext
	if sc == nil {
		t.Fatal("pod security context missing")
	}
	if sc.FSGroup == nil || *sc.FSGroup != FSGroupID {
		t.Errorf("fsGroup = %v, want %d", sc.FSGroup, FSGroupID)
	}
	if sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroupChangePolicy = %v, want %s", sc.FSGroupChangePolicy, corev1.FSGroupChangeOnRootMismatch)
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
			MountPath: "/data/resources",
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "lobby-config"},
			},
		}}
	})

	var found bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "lobby-config" && m.MountPath == "/data/resources" && m.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Errorf("volumeMounts = %+v, want a read-only lobby-config at /data/resources",
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
		env["SPAWNERY_SERVER"] != "lobby-x7k2" {
		t.Errorf("env = %v, want network, group and server", env)
	}
}

func TestServerPodNoLongerCarriesMaxPlayersAsAnEnvVar(t *testing.T) {
	pod := build(t, nil)
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "SPAWNERY_MAX_PLAYERS" {
			t.Error("SPAWNERY_MAX_PLAYERS is still on the pod; the value travels through the ConfigMap now")
		}
	}
}

func findVolume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// findConfigMapSource matches by content so the test does not depend on
// source order.
func findConfigMapSource(sources []corev1.VolumeProjection, configMap string) *corev1.ConfigMapProjection {
	for _, s := range sources {
		if s.ConfigMap != nil && s.ConfigMap.Name == configMap {
			return s.ConfigMap
		}
	}
	return nil
}

func findSecretSource(sources []corev1.VolumeProjection, secret string) *corev1.SecretProjection {
	for _, s := range sources {
		if s.Secret != nil && s.Secret.Name == secret {
			return s.Secret
		}
	}
	return nil
}

func TestConfigVolumeCarriesTheGroupConfigMapAndForwardingSecret(t *testing.T) {
	pod := build(t, nil)

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

	vol := findVolume(pod, ConfigVolumeName)
	if vol == nil || vol.Projected == nil {
		t.Fatalf("volume %q = %+v, want a projected source", ConfigVolumeName, vol)
	}
	if len(vol.Projected.Sources) != 2 {
		t.Fatalf("projected sources = %+v, want exactly 2 with no overlay declared", vol.Projected.Sources)
	}

	cm := findConfigMapSource(vol.Projected.Sources, GroupConfigMapName("lobby", RoleServer))
	if cm == nil {
		t.Fatalf("sources = %+v, want a configMap source named %q", vol.Projected.Sources, GroupConfigMapName("lobby", RoleServer))
	}
	if len(cm.Items) != 1 || cm.Items[0].Key != ConfigValuesKey || cm.Items[0].Path != "config.yaml" {
		t.Errorf("configMap items = %+v, want %s mapped to config.yaml", cm.Items, ConfigValuesKey)
	}

	sec := findSecretSource(vol.Projected.Sources, "velocity-forwarding-secret")
	if sec == nil {
		t.Fatalf("sources = %+v, want a secret source named velocity-forwarding-secret", vol.Projected.Sources)
	}
	if len(sec.Items) != 1 || sec.Items[0].Key != ForwardingSecretKey || sec.Items[0].Path != "forwarding.secret" {
		t.Errorf("secret items = %+v, want %s mapped to forwarding.secret", sec.Items, ForwardingSecretKey)
	}
}

// An Items-based projected source would drop unrecognised overlay keys at the
// kubelet, so the overlay must be a separate plain ConfigMap volume with nil
// Items.
func TestConfigOverlayIsAnUnfilteredVolumeNestedUnderTheConfigMount(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.ConfigOverlay = &spawneryv1alpha1.ObjectRef{Name: "lobby-overlay"}
	})

	base := findVolume(pod, ConfigVolumeName)
	if base == nil || base.Projected == nil || len(base.Projected.Sources) != 2 {
		t.Fatalf("volume %q = %+v, want exactly the 2 base sources — the overlay must not be folded in here", ConfigVolumeName, base)
	}

	overlay := findVolume(pod, ConfigOverlayVolumeName)
	if overlay == nil {
		t.Fatalf("volumes = %+v, want a %s volume", pod.Spec.Volumes, ConfigOverlayVolumeName)
	}
	if overlay.ConfigMap == nil {
		t.Fatalf("volume %q = %+v, want a plain configMap source, not Projected", ConfigOverlayVolumeName, overlay)
	}
	if overlay.ConfigMap.Name != "lobby-overlay" {
		t.Errorf("configMap name = %q, want lobby-overlay", overlay.ConfigMap.Name)
	}
	if len(overlay.ConfigMap.Items) != 0 {
		t.Errorf("items = %+v, want none: an enumerated list is exactly what would filter out an unrecognised overlay key before internal/render ever sees it", overlay.ConfigMap.Items)
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
		t.Errorf("mountPath = %q, want %q, nested inside ConfigMountPath so internal/render finds it at the OverlayDir it already reads",
			mount.MountPath, ConfigMountPath+"/overlay")
	}
	if !mount.ReadOnly {
		t.Error("the overlay volume is writable")
	}
}

func TestConfigOverlayVolumeIsAbsentWhenNoneIsDeclared(t *testing.T) {
	pod := build(t, nil)
	if v := findVolume(pod, ConfigOverlayVolumeName); v != nil {
		t.Errorf("volume %q = %+v, want it absent with no configOverlay declared", ConfigOverlayVolumeName, v)
	}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == ConfigOverlayVolumeName {
			t.Errorf("mount %+v present with no configOverlay declared", m)
		}
	}
}

// Builds the on-disk shape an unfiltered overlay mount produces and checks
// that internal/render refuses an unknown key by name.
func TestConfigOverlayReachesTheRendererEvenWithAnUnrecognisedKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, render.ValuesFile), []byte("maxPlayers: 100\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, render.SecretFile), []byte("s3cr3t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlayDir := filepath.Join(dir, render.OverlayDir)
	if err := os.Mkdir(overlayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A real Paper file that internal/render.Paper does not write.
	const badKey = "paper-world-defaults.yml"
	if err := os.WriteFile(filepath.Join(overlayDir, badKey), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	values, secret, overlay, err := render.Load(dir)
	if err != nil {
		t.Fatalf("render.Load: %v", err)
	}
	if _, ok := overlay[badKey]; !ok {
		t.Fatalf("overlay = %+v, want the mount to have surfaced %q unfiltered", overlay, badKey)
	}

	_, err = render.Paper(values, secret, overlay)
	if err == nil {
		t.Fatal("render.Paper accepted an overlay naming a file it does not write")
	}
	if !strings.Contains(err.Error(), badKey) {
		t.Errorf("error = %q, want it to name %q", err, badKey)
	}
}

// podspec names these paths independently of internal/render (it must not
// import it). A diverged overlay dir would drop the user's override silently.
func TestConfigPathsAgreeWithRender(t *testing.T) {
	if configOverlayDir != render.OverlayDir {
		t.Errorf("podspec.configOverlayDir = %q, render.OverlayDir = %q, want them equal", configOverlayDir, render.OverlayDir)
	}
	if configSecretFile != render.SecretFile {
		t.Errorf("podspec.configSecretFile = %q, render.SecretFile = %q, want them equal", configSecretFile, render.SecretFile)
	}
	if ConfigMountPath != render.ConfigDir {
		t.Errorf("podspec.ConfigMountPath = %q, render.ConfigDir = %q, want them equal", ConfigMountPath, render.ConfigDir)
	}
	if ConfigValuesKey != render.ValuesFile {
		t.Errorf("podspec.ConfigValuesKey = %q, render.ValuesFile = %q, want them equal", ConfigValuesKey, render.ValuesFile)
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
	// A PVC arrives owned by root, unlike an emptyDir.
	assertFSGroup(t, pod)

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

func TestOnDemandServerMountsItsOwnClaim(t *testing.T) {
	network, group, srv := testNetwork(), testGroup(), testServer()
	group.Spec.Type = spawneryv1alpha1.ServerGroupOnDemand
	group.Spec.Storage = &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("2Gi")}
	srv.Name = "private-servers-c0ffee"
	srv.Spec.Key = "c0ffee"

	pod, err := BuildServerPod(network, group, srv, testEndpoint)
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}

	data := findVolume(pod, DataVolumeName)
	if data == nil {
		t.Fatal("the pod has no data volume")
	}
	if data.PersistentVolumeClaim == nil {
		t.Fatal("an on-demand member got an emptyDir, so its world would not survive a stop")
	}
	if got, want := data.PersistentVolumeClaim.ClaimName, DataClaimName(srv.Name); got != want {
		t.Errorf("claim = %q, want %q", got, want)
	}
	// Always cannot tell a player's /stop from a crash.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
}

func TestAnEphemeralPodIsNotLiftedAgainByKubelet(t *testing.T) {
	// Always would restart over the same emptyDir and bring back the same world.
	ephemeral := build(t, nil)
	if got, want := ephemeral.Spec.RestartPolicy, corev1.RestartPolicyNever; got != want {
		t.Errorf("ephemeral restartPolicy = %q, want %q", got, want)
	}

	persistent := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Type = spawneryv1alpha1.ServerGroupPersistent
	})
	if got, want := persistent.Spec.RestartPolicy, corev1.RestartPolicyAlways; got != want {
		t.Errorf("persistent restartPolicy = %q, want %q", got, want)
	}
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
		{
			name: "same volume name as the config volume",
			mount: spawneryv1alpha1.Mount{
				Name:      ConfigVolumeName,
				MountPath: "/irgendwo",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: ConfigVolumeName,
		},
		{
			name: "same volume name as the config overlay volume",
			mount: spawneryv1alpha1.Mount{
				Name:      ConfigOverlayVolumeName,
				MountPath: "/irgendwo",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: ConfigOverlayVolumeName,
		},
		{
			name: "mounted over the config path",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: ConfigMountPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: ConfigMountPath,
		},
		{
			name: "nested inside the config mount, shadowing the forwarding secret",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: ConfigMountPath + "/forwarding.secret",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: ConfigMountPath,
		},
		{
			name: "mounted over where the overlay volume nests",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: ConfigMountPath + "/overlay",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: ConfigMountPath,
		},
		{
			name: "mounted over a parent of the config mount",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: "/etc",
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: ConfigMountPath,
		},
		{
			// checkMountCollision has no entry for FileSourceMountPath: this is refused
			// only because it nests under AgentMountPath.
			name: "mounted over the extraFiles claim",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: FileSourceMountPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			},
			want: AgentMountPath,
		},
		{
			name: "nested inside the extraFiles claim",
			mount: spawneryv1alpha1.Mount{
				Name:      "eigenes",
				MountPath: FileSourceMountPath + "/nested",
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

func TestNonCollidingUserMountsAreAccepted(t *testing.T) {
	cases := []struct {
		name      string
		mountPath string
	}{
		{
			name:      "a world tree nested inside /data, the documented pattern",
			mountPath: DataMountPath + "/worlds",
		},
		{
			name:      "a sibling directory that only shares a prefix with /data",
			mountPath: DataMountPath + "-extra",
		},
		{
			name:      "a plugin nested inside the plugins directory",
			mountPath: PluginsMountPath + "/my-plugin",
		},
		{
			name:      "a sibling directory that only shares a prefix with the plugins directory",
			mountPath: PluginsMountPath + "-extra",
		},
		{
			name:      "a sibling directory that only shares a prefix with the config mount",
			mountPath: ConfigMountPath + "-extra",
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

func TestGroupConfigMapNameIsScopedByRoleAsWellAsGroup(t *testing.T) {
	server := GroupConfigMapName("lobby", RoleServer)
	proxy := GroupConfigMapName("lobby", RoleProxy)

	if server == proxy {
		t.Fatalf("GroupConfigMapName(%q, RoleServer) = GroupConfigMapName(%q, RoleProxy) = %q, want them to differ: "+
			"a ServerGroup and a ProxyGroup of the same name would otherwise fight over one ConfigMap", "lobby", "lobby", server)
	}
	if server == "lobby" || proxy == "lobby" {
		t.Errorf("server=%q proxy=%q, want neither to be the bare group name: "+
			"that identity is what a user's own ConfigMap named after their group could collide with", server, proxy)
	}
	if !strings.Contains(server, "lobby") || !strings.Contains(proxy, "lobby") {
		t.Errorf("server=%q proxy=%q, want both to still carry the group name", server, proxy)
	}
}

// checkMountCollision sees one mount at a time; collisions between two user
// mounts are caught in the loop instead of by the API server's index-based
// message.
func TestTwoUserMountsCannotCollideWithEachOther(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mounts []spawneryv1alpha1.Mount
		says   string
	}{
		{
			name: "two mounts sharing a name",
			mounts: []spawneryv1alpha1.Mount{
				{Name: "extra", MountPath: "/data/plugins/a", ConfigMap: &corev1.ConfigMapVolumeSource{}},
				{Name: "extra", MountPath: "/data/plugins/b", ConfigMap: &corev1.ConfigMapVolumeSource{}},
			},
			says: "declared twice",
		},
		{
			name: "two mounts sharing a path",
			mounts: []spawneryv1alpha1.Mount{
				{Name: "one", MountPath: "/data/plugins/x", ConfigMap: &corev1.ConfigMapVolumeSource{}},
				{Name: "two", MountPath: "/data/plugins/x", ConfigMap: &corev1.ConfigMapVolumeSource{}},
			},
			says: "already targets",
		},
		{
			name: "the same path spelled differently",
			mounts: []spawneryv1alpha1.Mount{
				{Name: "one", MountPath: "/data/plugins/x", ConfigMap: &corev1.ConfigMapVolumeSource{}},
				{Name: "two", MountPath: "/data/plugins/x/", ConfigMap: &corev1.ConfigMapVolumeSource{}},
			},
			says: "already targets",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net, group := serverHashFixtures(t)
			group.Spec.Mounts = tc.mounts
			_, err := BuildServerPod(net, group, &spawneryv1alpha1.Server{
				ObjectMeta: metav1.ObjectMeta{Name: "lobby-aaaa", Namespace: group.Namespace},
			}, "spawnery.invalid:0")
			if err == nil {
				t.Fatal("the pod was built; the API server would refuse it later with a " +
					"message about an index in an array")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error = %q, want it to say %q", err, tc.says)
			}
		})
	}
}

// A selector naming a label that is gone would match no pods, and a group
// with no pods reads as healthy.
func TestTheServerGroupSelectorIsASubsetOfServerLabels(t *testing.T) {
	full := ServerLabels("production", "lobby", "lobby-x7k2")
	selector := ServerGroupSelector("production", "lobby")

	for key, want := range selector {
		got, ok := full[key]
		if !ok {
			t.Errorf("the selector names %q, which ServerLabels does not write", key)
			continue
		}
		if got != want {
			t.Errorf("the selector wants %s=%q, ServerLabels writes %q", key, want, got)
		}
	}
	if _, ok := selector[LabelServer]; ok {
		t.Errorf("the selector carries %s, which differs per pod", LabelServer)
	}
	if len(selector) != len(full)-1 {
		t.Errorf("the selector has %d labels and ServerLabels %d; they differ by more than %s",
			len(selector), len(full), LabelServer)
	}
}

func TestAMountAtThePluginsDirectoryIsRefused(t *testing.T) {
	for _, mountPath := range []string{PluginsMountPath, PluginsMountPath + "/"} {
		t.Run(mountPath, func(t *testing.T) {
			net, group := testNetwork(), testGroup()
			group.Spec.Mounts = []spawneryv1alpha1.Mount{{
				Name:      "plugins",
				MountPath: mountPath,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
				},
			}}

			_, err := BuildServerPod(net, group, testServer(), testEndpoint)
			if err == nil {
				t.Fatalf("BuildServerPod accepted a mount at %q; the server would not have come up", mountPath)
			}
			for _, want := range []string{PluginsMountPath, "agent plugin", "Mount inside it instead"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestTheServerContainerKeepsStdinOpenForTheConsole(t *testing.T) {
	// Without stdin the console reader sees EOF and `kubectl attach` keystrokes
	// go nowhere.
	pod := build(t, nil)
	c := pod.Spec.Containers[0]

	if !c.Stdin {
		t.Error("the container closes stdin, so the console cannot be reached at all")
	}
	// StdinOnce would close stdin when the first attached client disconnects.
	if c.StdinOnce {
		t.Error("StdinOnce is set: the console would work once and then never again")
	}
	// A TTY would switch Paper to its terminal console output.
	if c.TTY {
		t.Error("a TTY was allocated; the console needs stdin, not a terminal")
	}
}

func TestExtraPluginsMountsTheClaimReadOnlyOutsideData(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.ExtraPlugins = &spawneryv1alpha1.ExtraPlugins{ClaimName: "plugins"}
	})

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == PluginSourceVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatal("no plugin source volume was rendered")
	}
	if vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != "plugins" {
		t.Fatalf("volume source = %+v, want the named claim", vol.VolumeSource)
	}
	// One claim may serve several groups.
	if !vol.PersistentVolumeClaim.ReadOnly {
		t.Error("the claim is mounted writable")
	}

	var mount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == PluginSourceVolumeName {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatal("the plugin source volume is not mounted")
	}
	if !mount.ReadOnly {
		t.Error("the plugin source is mounted writable")
	}
	if mount.MountPath != PluginSourceMountPath {
		t.Errorf("mountPath = %q, want %q", mount.MountPath, PluginSourceMountPath)
	}
	// A read-only mount under /data/plugins fails the entrypoint's own copy.
	if isPathUnder(path.Clean(mount.MountPath), path.Clean(DataMountPath)) {
		t.Errorf("mountPath %q is under %s, where a read-only mount breaks the start",
			mount.MountPath, DataMountPath)
	}
}

func TestNoExtraPluginsRendersNoVolume(t *testing.T) {
	// Keeps the golden digests still for installations that do not use this.
	pod := build(t, nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == PluginSourceVolumeName {
			t.Fatal("a plugin source volume was rendered for a group that named none")
		}
	}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == PluginSourceVolumeName {
			t.Fatal("a plugin source mount was rendered for a group that named none")
		}
	}
}

func TestExtraFilesIsMountedReadOnlyOutsideData(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.ExtraFiles = &spawneryv1alpha1.ExtraFiles{ClaimName: "files"}
	})

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == FileSourceVolumeName {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatal("no extra-files volume was rendered")
	}
	if vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != "files" {
		t.Fatalf("volume source = %+v, want the named claim", vol.VolumeSource)
	}
	// One claim may serve several groups.
	if !vol.PersistentVolumeClaim.ReadOnly {
		t.Error("the claim is mounted writable")
	}

	var mount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == FileSourceVolumeName {
			mount = &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatal("no extra-files mount on a group that names a claim")
	}
	if mount.MountPath != FileSourceMountPath {
		t.Errorf("mounted at %q, want %q", mount.MountPath, FileSourceMountPath)
	}
	if !mount.ReadOnly {
		t.Error("the source is writable; every user volume this package renders is read-only")
	}
	if strings.HasPrefix(mount.MountPath, DataMountPath) {
		t.Errorf("mounted inside %s, which a read-only mount may not be", DataMountPath)
	}
}

func TestNoExtraFilesVolumeWithoutTheField(t *testing.T) {
	pod := build(t, nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == FileSourceVolumeName {
			t.Error("a group that names no claim got the volume anyway")
		}
	}
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == FileSourceVolumeName {
			t.Error("a group that names no claim got the mount anyway")
		}
	}
}

// Reads the slice the check uses rather than a hand-written list.
func TestEveryReservedVolumeNameIsRefusedAsAUserMount(t *testing.T) {
	for _, name := range reservedVolumeNames {
		err := checkMountCollision(spawneryv1alpha1.Mount{
			Name:      name,
			MountPath: "/somewhere/else",
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "meine-cm"},
			},
		})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a user mount named %q: err = %v, want a refusal naming it", name, err)
		}
	}
	if !slices.Contains(reservedVolumeNames, PluginSourceVolumeName) {
		t.Error("extra-plugins is not among the reserved names")
	}
	if !slices.Contains(reservedVolumeNames, AOTCacheVolumeName) {
		t.Error("aot-cache is not among the reserved names")
	}
}
