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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func objectStoreGroup(_ *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
	g.Spec.Type = spawneryv1alpha1.ServerGroupOnDemand
	g.Spec.Scaling = nil
	g.Spec.Storage = &spawneryv1alpha1.StorageSpec{
		Size: resource.MustParse("6Gi"), Backend: spawneryv1alpha1.StorageBackendObjectStore,
		Keep: []string{"worlds/world", "plugins/Example/data"},
	}
}

func envValue(pod *corev1.Pod, name string) string {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func TestAnObjectStoreMemberGetsTheCSIVolume(t *testing.T) {
	net, group := testNetwork(), testGroup()
	objectStoreGroup(net, group)
	srv := testServer()
	srv.Spec.Key = "c0ffee"
	pod, err := BuildServerPod(net, group, srv, testEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	v := volumeNamed(pod, DataVolumeName)
	if v == nil || v.CSI == nil {
		t.Fatalf("data volume = %+v, want a CSI inline volume", v)
	}
	if v.CSI.Driver != WorldSyncDriver {
		t.Errorf("driver = %q", v.CSI.Driver)
	}
	if got, want := v.CSI.VolumeAttributes["world"], srv.Namespace+"/"+group.Name+"/c0ffee"; got != want {
		t.Errorf("world = %q, want %q", got, want)
	}
	if got := v.CSI.VolumeAttributes["keep"]; got != "worlds/world\nplugins/Example/data" {
		t.Errorf("keep = %q", got)
	}
	if _, ok := v.CSI.VolumeAttributes["replace"]; ok {
		t.Error("a group without replace renders a replace attribute")
	}
	if envValue(pod, EnvWorldSync) != "1" {
		t.Error("SPAWNERY_WORLD_SYNC is not set")
	}
}

func TestTheNodeAgentLearnsReplace(t *testing.T) {
	pod := build(t, func(n *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		objectStoreGroup(n, g)
		g.Spec.Storage.Replace = []string{"worlds/world/datapacks", "lobby"}
	})
	v := volumeNamed(pod, DataVolumeName)
	if got := v.CSI.VolumeAttributes["replace"]; got != "worlds/world/datapacks\nlobby" {
		t.Errorf("replace = %q", got)
	}
}

func TestAClaimMemberIsUnchanged(t *testing.T) {
	pod := build(t, func(n *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) {
		objectStoreGroup(n, g)
		g.Spec.Storage.Backend = ""
	})
	if v := volumeNamed(pod, DataVolumeName); v == nil || v.PersistentVolumeClaim == nil {
		t.Fatalf("data volume = %+v, want the claim", v)
	}
	if envValue(pod, EnvWorldSync) != "" {
		t.Error("a claim member carries SPAWNERY_WORLD_SYNC")
	}
}

func TestTheIntervalIsAddedAfterTheHash(t *testing.T) {
	pod := build(t, objectStoreGroup)
	WithWorldSyncInterval(pod, 5*time.Minute)
	if got := envValue(pod, EnvWorldSyncInterval); got != "5m0s" {
		t.Fatalf("interval = %q", got)
	}
}

func TestAClaimPodGetsNoInterval(t *testing.T) {
	pod := build(t, nil)
	WithWorldSyncInterval(pod, time.Minute)
	if envValue(pod, EnvWorldSyncInterval) != "" {
		t.Error("a claim pod carries the interval")
	}
}

func persistentObjectStoreGroup(g *spawneryv1alpha1.ServerGroup) {
	g.Spec.Type = spawneryv1alpha1.ServerGroupPersistent
	g.Spec.Scaling = nil
	g.Spec.Storage = &spawneryv1alpha1.StorageSpec{
		Size: resource.MustParse("6Gi"), Backend: spawneryv1alpha1.StorageBackendObjectStore,
		Keep: []string{"world"},
	}
}

func TestAPersistentObjectStoreServerNamesItsWorldByOrdinal(t *testing.T) {
	net, group := testNetwork(), testGroup()
	persistentObjectStoreGroup(group)
	srv := testServer()
	srv.Spec.Ordinal = ptr.To[int32](3)
	pod, err := BuildServerPod(net, group, srv, testEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	v := volumeNamed(pod, DataVolumeName)
	if v == nil || v.CSI == nil {
		t.Fatalf("data volume = %+v, want a CSI inline volume", v)
	}
	if got, want := v.CSI.VolumeAttributes["world"], srv.Namespace+"/"+group.Name+"/3"; got != want {
		t.Errorf("world = %q, want %q", got, want)
	}
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			t.Errorf("volume %s is a claim", vol.Name)
		}
	}
	if envValue(pod, EnvWorldSync) != "1" {
		t.Error("SPAWNERY_WORLD_SYNC is not set")
	}
}

func TestWorldKey(t *testing.T) {
	tests := []struct {
		name string
		spec spawneryv1alpha1.ServerSpec
		want string
	}{
		{"key", spawneryv1alpha1.ServerSpec{Key: "c0ffee"}, "c0ffee"},
		{"ordinal zero", spawneryv1alpha1.ServerSpec{Ordinal: ptr.To[int32](0)}, "0"},
		{"ordinal twelve", spawneryv1alpha1.ServerSpec{Ordinal: ptr.To[int32](12)}, "12"},
		{"neither", spawneryv1alpha1.ServerSpec{}, ""},
		{"both", spawneryv1alpha1.ServerSpec{Key: "c0ffee", Ordinal: ptr.To[int32](1)}, "c0ffee"},
		{"world key", spawneryv1alpha1.ServerSpec{WorldKey: "round-7"}, "round-7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WorldKey(&spawneryv1alpha1.Server{Spec: tt.spec}); got != tt.want {
				t.Errorf("WorldKey = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAPersistentObjectStoreHashIgnoresTheOrdinal(t *testing.T) {
	net, group := testNetwork(), testGroup()
	persistentObjectStoreGroup(group)
	a, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("hash differs between calls: %s, %s", a, b)
	}
	claim := testGroup()
	claim.Spec.Type = spawneryv1alpha1.ServerGroupPersistent
	claim.Spec.Scaling = nil
	claim.Spec.Storage = &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("6Gi")}
	c, err := DesiredServerHash(net, claim, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Error("ObjectStore and Claim groups hash alike")
	}
}

func keptRoundsGroup(g *spawneryv1alpha1.ServerGroup) {
	g.Spec.MaxInstances = ptr.To[int32](20)
	g.Spec.Storage = &spawneryv1alpha1.StorageSpec{
		Size: resource.MustParse("2Gi"), Backend: spawneryv1alpha1.StorageBackendObjectStore,
		Keep: []string{"world"},
	}
}

// A group that gains storage rolls: its members' pods are built once, so
// the ones already running keep their volume until the changed hash replaces
// them.
func TestAnEphemeralGroupGainingStorageMovesItsHash(t *testing.T) {
	net, group := testNetwork(), testGroup()
	before, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatal(err)
	}
	keptRoundsGroup(group)
	after, err := DesiredServerHash(net, group, nil)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("an Ephemeral group that gained storage hashes as before, so its running members would never get the standby volume")
	}
}

func TestAnUnkeyedEphemeralMemberGetsAStandbyVolume(t *testing.T) {
	net, group := testNetwork(), testGroup()
	keptRoundsGroup(group)
	srv := testServer()
	pod, err := BuildServerPod(net, group, srv, testEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	v := volumeNamed(pod, DataVolumeName)
	if v == nil || v.CSI == nil || v.CSI.Driver != WorldSyncDriver {
		t.Fatalf("data volume = %+v, want the world sync CSI volume", v)
	}
	if got, want := v.CSI.VolumeAttributes[WorldSyncAttrStandby], srv.Namespace+"/"+group.Name+"/"+srv.Name; got != want {
		t.Errorf("standby = %q, want %q", got, want)
	}
	if _, ok := v.CSI.VolumeAttributes[WorldSyncAttrWorld]; ok {
		t.Error("a standby volume names a world")
	}
	if got := v.CSI.VolumeAttributes[WorldSyncAttrKeep]; got != "world" {
		t.Errorf("keep = %q", got)
	}
	if got := envValue(pod, EnvWorldSync); got != WorldSyncStandby {
		t.Errorf("SPAWNERY_WORLD_SYNC = %q, want %q", got, WorldSyncStandby)
	}
}

func TestAKeyedEphemeralMemberRendersTheOnDemandWorld(t *testing.T) {
	net, group := testNetwork(), testGroup()
	keptRoundsGroup(group)
	srv := testServer()
	srv.Name = group.Name + "-round-7"
	srv.Spec.WorldKey = "round-7"
	srv.Spec.Resumed = true
	pod, err := BuildServerPod(net, group, srv, testEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	v := volumeNamed(pod, DataVolumeName)
	if v == nil || v.CSI == nil {
		t.Fatalf("data volume = %+v, want a CSI inline volume", v)
	}
	if got, want := v.CSI.VolumeAttributes[WorldSyncAttrWorld], srv.Namespace+"/"+group.Name+"/round-7"; got != want {
		t.Errorf("world = %q, want %q", got, want)
	}
	if _, ok := v.CSI.VolumeAttributes[WorldSyncAttrStandby]; ok {
		t.Error("a keyed member carries the standby attribute")
	}
	if envValue(pod, EnvWorldSync) != "1" {
		t.Error("SPAWNERY_WORLD_SYNC is not 1")
	}
}

func TestAnEphemeralGroupWithoutStorageKeepsItsHash(t *testing.T) {
	group := goldenEphemeralServerGroup()
	group.Spec.Storage = nil
	got, err := DesiredServerHash(goldenNetwork(), group, goldenConfigValues)
	if err != nil {
		t.Fatal(err)
	}
	if got != goldenEphemeralServerDigest {
		t.Fatalf("hash = %q, want the pinned %q", got, goldenEphemeralServerDigest)
	}
	pod, err := BuildServerPod(goldenNetwork(), group, &spawneryv1alpha1.Server{}, goldenAgentEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if v := volumeNamed(pod, DataVolumeName); v == nil || v.EmptyDir == nil {
		t.Errorf("data volume = %+v, want an emptyDir", v)
	}
	if envValue(pod, EnvWorldSync) != "" {
		t.Error("an ephemeral member without storage carries SPAWNERY_WORLD_SYNC")
	}

	keptRoundsGroup(group)
	withStorage, err := DesiredServerHash(goldenNetwork(), group, goldenConfigValues)
	if err != nil {
		t.Fatal(err)
	}
	if withStorage == got {
		t.Error("adding storage to an ephemeral group leaves its hash alone; its members would keep their emptyDir")
	}
}
