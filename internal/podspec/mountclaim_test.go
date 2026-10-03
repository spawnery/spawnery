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
	// A volume marked writable but mounted read-only still attaches read-write
	// to the node.
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
	// The kubelet mounts a ConfigMap read-only regardless.
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

	// Writable reaches the pod through two fields, and the digest must see both.
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
	// Without subPath the pod gets a directory named bukkit.yml.
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
	// An empty subPath is the whole volume, as before the field existed.
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
	// The kubelet creates a mount's parent directory root-owned and
	// group-read-only, which fsGroup with OnRootMismatch does not fix, so the
	// server cannot write paper-global.yml and never starts.
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
		if !strings.Contains(err.Error(), "configOverlay") {
			t.Errorf("the refusal for %q does not name what to use instead: %v", mountPath, err)
		}
	}

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

func TestAProxyPodCanWriteAClaimItMountsWritable(t *testing.T) {
	group := testProxyGroup()
	group.Spec.Mounts = []spawneryv1alpha1.Mount{{
		Name:      "pool",
		MountPath: "/data/pool",
		PersistentVolumeClaim: &spawneryv1alpha1.MountClaim{
			ClaimName: "shared",
			Writable:  true,
		},
	}}
	pod, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint, nil)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	if m := mountNamed(pod, "pool"); m == nil || m.ReadOnly {
		t.Errorf("mount = %+v, want it writable", m)
	}
	assertFSGroup(t, pod)
}
