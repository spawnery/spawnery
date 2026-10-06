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
	if envValue(pod, EnvWorldSync) != "1" {
		t.Error("SPAWNERY_WORLD_SYNC is not set")
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
