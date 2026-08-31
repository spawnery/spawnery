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

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func volumeNamed(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

func mountNamed(pod *corev1.Pod, name string) *corev1.VolumeMount {
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == name {
			return &pod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	return nil
}

func TestAClaimMountIsReadOnlyUnlessItSaysOtherwise(t *testing.T) {
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Mounts = []spawneryv1alpha1.Mount{{
			Name:                  "worlds",
			MountPath:             "/data/worlds",
			PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "map-pool"},
		}}
	})

	vol := volumeNamed(pod, "worlds")
	if vol == nil {
		t.Fatal("no volume was rendered for the claim mount")
	}
	if vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != "map-pool" {
		t.Fatalf("volume source = %+v, want the named claim", vol.VolumeSource)
	}
	// Read-only at the volume as well as at the mount. A volume marked
	// writable and mounted read-only still attaches read-write to the node,
	// and the cost of that shows up as a claim that will not attach elsewhere
	// rather than as anything about this pod.
	if !vol.PersistentVolumeClaim.ReadOnly {
		t.Error("the claim is attached read-write for a mount that did not ask")
	}

	m := mountNamed(pod, "worlds")
	if m == nil {
		t.Fatal("the claim volume is not mounted")
	}
	if !m.ReadOnly {
		t.Error("the mount is writable for a mount that did not ask")
	}
	if m.MountPath != "/data/worlds" {
		t.Errorf("mountPath = %q, want /data/worlds", m.MountPath)
	}
}

func TestAWritableClaimMountIsWritableAtBothEnds(t *testing.T) {
	// The case the field exists for: one group fills a pool of generated
	// worlds that other groups read. Writable at the mount but read-only at
	// the volume would fail at runtime with a read-only filesystem error,
	// which is the wrong half to get right.
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Mounts = []spawneryv1alpha1.Mount{{
			Name:      "pool",
			MountPath: "/world-pool",
			PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{
				ClaimName: "world-pool",
				Writable:  true,
			},
		}}
	})

	vol := volumeNamed(pod, "pool")
	if vol == nil || vol.PersistentVolumeClaim == nil {
		t.Fatal("no claim volume was rendered")
	}
	if vol.PersistentVolumeClaim.ReadOnly {
		t.Error("the claim is attached read-only for a writable mount")
	}
	if m := mountNamed(pod, "pool"); m == nil || m.ReadOnly {
		t.Errorf("mount = %+v, want it writable", m)
	}
}

func TestAConfigMapMountStaysReadOnly(t *testing.T) {
	// Writable is a property of a claim and nothing else. The kubelet mounts a
	// ConfigMap read-only whatever anybody writes, so the pod has to say so
	// too rather than claiming a thing that is not true.
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Mounts = []spawneryv1alpha1.Mount{{
			Name:      "motd",
			MountPath: "/data/motd",
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "motd"},
			},
		}}
	})

	vol := volumeNamed(pod, "motd")
	if vol == nil || vol.ConfigMap == nil {
		t.Fatalf("volume = %+v, want a ConfigMap source", vol)
	}
	if vol.PersistentVolumeClaim != nil {
		t.Error("a ConfigMap mount rendered a claim source as well")
	}
	if m := mountNamed(pod, "motd"); m == nil || !m.ReadOnly {
		t.Errorf("mount = %+v, want it read-only", m)
	}
}

func TestAClaimMountObeysTheReservedPaths(t *testing.T) {
	// The refusals are a property of the path, not of the source. A claim
	// arriving at /var/run/spawnery would shadow the agent's own credentials,
	// and the server would fail to authenticate with nothing naming the mount.
	_, err := BuildServerPod(testNetwork(), func() *spawneryv1alpha1.ServerGroup {
		g := testGroup()
		g.Spec.Mounts = []spawneryv1alpha1.Mount{{
			Name:                  "sneaky",
			MountPath:             AgentMountPath,
			PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "any"},
		}}
		return g
	}(), testServer(), testEndpoint)

	if err == nil {
		t.Fatal("a claim mount at the agent's credential path was accepted")
	}
	if !strings.Contains(err.Error(), AgentMountPath) {
		t.Errorf("error = %v, want it to name the reserved path", err)
	}
}

func TestAProxyGroupCarriesItsOwnMounts(t *testing.T) {
	// A ProxyGroup had no spec.mounts at all until this change, so this is the
	// test that the field reaches a pod rather than sitting in the CRD being
	// accepted and ignored -- which is what an unrendered spec field does, and
	// it looks exactly like a working one from `kubectl get`.
	group := testProxyGroup()
	group.Spec.Mounts = []spawneryv1alpha1.Mount{{
		Name:                  "assets",
		MountPath:             "/data/resources",
		PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "assets"},
	}}

	pod, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint, nil)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	vol := volumeNamed(pod, "assets")
	if vol == nil || vol.PersistentVolumeClaim == nil ||
		vol.PersistentVolumeClaim.ClaimName != "assets" {
		t.Fatalf("volume = %+v, want the named claim", vol)
	}
	if m := mountNamed(pod, "assets"); m == nil || m.MountPath != "/data/resources" {
		t.Fatalf("mount = %+v, want it at /data/resources", m)
	}
}

func TestAProxyGroupsMountsObeyTheReservedPaths(t *testing.T) {
	group := testProxyGroup()
	group.Spec.Mounts = []spawneryv1alpha1.Mount{{
		Name:      "sneaky",
		MountPath: ConfigMountPath,
		ConfigMap: &corev1.ConfigMapVolumeSource{},
	}}

	if _, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint, nil); err == nil {
		t.Fatal("a proxy mount at the operator's own config path was accepted")
	}
}

func TestAClaimMountReachesTheHash(t *testing.T) {
	net, group := testNetwork(), testGroup()
	before, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}

	group.Spec.Mounts = []spawneryv1alpha1.Mount{{
		Name:                  "worlds",
		MountPath:             "/data/worlds",
		PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{ClaimName: "map-pool"},
	}}
	mounted, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	if mounted == before {
		t.Fatal("adding a claim mount did not move the digest")
	}

	// Flipping writable changes what the pod actually gets, so it has to move
	// the digest too. It reaches the pod through two fields at once, and a
	// digest that only saw one of them would leave a fleet mounted read-only
	// while the spec said otherwise.
	group.Spec.Mounts[0].PersistentVolumeClaim.Writable = true
	writable, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	if writable == mounted {
		t.Fatal("flipping writable did not move the digest")
	}
}

func TestASubPathMountLandsOneFile(t *testing.T) {
	// The case it exists for: a single configuration file beside the ones the
	// server writes itself. Without subPath the pod gets a *directory* named
	// bukkit.yml, and what the server reports is a parse error rather than
	// anything about a mount.
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Mounts = []spawneryv1alpha1.Mount{{
			Name:      "bukkit",
			MountPath: "/data/bukkit.yml",
			SubPath:   "bukkit.yml",
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "server-files"},
			},
		}}
	})

	m := mountNamed(pod, "bukkit")
	if m == nil {
		t.Fatal("no mount was rendered")
	}
	if m.SubPath != "bukkit.yml" {
		t.Errorf("subPath = %q, want bukkit.yml", m.SubPath)
	}
	if m.MountPath != "/data/bukkit.yml" {
		t.Errorf("mountPath = %q, want /data/bukkit.yml", m.MountPath)
	}
}

func TestAMountWithNoSubPathLeavesItEmpty(t *testing.T) {
	// An empty subPath is the whole volume, which is what every mount written
	// before this field existed meant. A default that was anything else would
	// change what those mounts do.
	pod := build(t, func(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		g.Spec.Mounts = []spawneryv1alpha1.Mount{{
			Name:      "assets",
			MountPath: "/data/resources",
			ConfigMap: &corev1.ConfigMapVolumeSource{},
		}}
	})
	if m := mountNamed(pod, "assets"); m == nil || m.SubPath != "" {
		t.Errorf("mount = %+v, want an empty subPath", m)
	}
}

func TestSubPathReachesTheHash(t *testing.T) {
	// Two mounts identical but for the subPath put different files in the
	// container. A digest that could not tell them apart would leave a fleet
	// mounting the whole directory while the spec named one file.
	net, group := testNetwork(), testGroup()
	group.Spec.Mounts = []spawneryv1alpha1.Mount{{
		Name:      "files",
		MountPath: "/data/bukkit.yml",
		ConfigMap: &corev1.ConfigMapVolumeSource{},
	}}
	whole, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}

	group.Spec.Mounts[0].SubPath = "bukkit.yml"
	one, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	if whole == one {
		t.Fatal("adding a subPath did not move the digest")
	}
}

func TestAMountInsideTheServersConfigDirectoryIsRefused(t *testing.T) {
	// Measured in a kind cluster on 2026-08-31: the kubelet creates a mount's
	// parent directory root-owned and group-read-only (drwxr-sr-x 0 10001),
	// while fsGroup with OnRootMismatch only ever touches the volume root
	// (drwxrwsrwx). So a mount here leaves the container unable to write into
	// the directory, and the first write it attempts is spawnery-config's own
	// paper-global.yml. The server never starts, and the error names a file
	// rather than a mount.
	//
	// Design spec 4.3 used exactly this path as its example of a legitimate
	// mount, which is why this test names the measurement rather than
	// pointing at the design.
	for _, mountPath := range []string{
		ServerConfigDirPath,
		ServerConfigDirPath + "/paper-world-defaults.yml",
		ServerConfigDirPath + "/sponge/sponge.conf",
	} {
		group := testGroup()
		group.Spec.Mounts = []spawneryv1alpha1.Mount{{
			Name:      "conf",
			MountPath: mountPath,
			ConfigMap: &corev1.ConfigMapVolumeSource{},
		}}
		_, err := BuildServerPod(testNetwork(), group, testServer(), testEndpoint)
		if err == nil {
			t.Errorf("a mount at %q was accepted; every server of the group would fail to start", mountPath)
			continue
		}
		// The remedy, not just the refusal: somebody who wanted
		// paper-world-defaults.yml has a field that does work.
		if !strings.Contains(err.Error(), "configOverlay") {
			t.Errorf("the refusal for %q does not name what to use instead: %v", mountPath, err)
		}
	}

	// And a sibling that merely shares the prefix is still fine.
	group := testGroup()
	group.Spec.Mounts = []spawneryv1alpha1.Mount{{
		Name:      "conf",
		MountPath: ServerConfigDirPath + "-extra",
		ConfigMap: &corev1.ConfigMapVolumeSource{},
	}}
	if _, err := BuildServerPod(testNetwork(), group, testServer(), testEndpoint); err != nil {
		t.Errorf("a sibling of the config directory was refused: %v", err)
	}
}
