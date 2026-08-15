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

import "testing"

func TestPodHashIsStableAcrossBuilds(t *testing.T) {
	net, group := testNetwork(), testProxyGroup()
	a, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	b, err := BuildProxyPod(net, group, "gateway-bbbb", testEndpoint)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a.Labels[LabelPodHash] != b.Labels[LabelPodHash] {
		t.Errorf("hash differs between two builds of one spec: %q vs %q — the pod name must not reach it",
			a.Labels[LabelPodHash], b.Labels[LabelPodHash])
	}
}

func TestPodHashMovesWithTheImage(t *testing.T) {
	net, group := testNetwork(), testProxyGroup()
	before, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	group.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	after, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if before.Labels[LabelPodHash] == after.Labels[LabelPodHash] {
		t.Error("hash unchanged after the image changed; a new image would never roll out")
	}
}

func TestPodHashIgnoresReplicas(t *testing.T) {
	net, group := testNetwork(), testProxyGroup()
	group.Spec.Replicas = 2
	before, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	group.Spec.Replicas = 5
	after, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if before.Labels[LabelPodHash] != after.Labels[LabelPodHash] {
		t.Error("hash moved when only replicas changed; scaling would trigger a full replacement")
	}
}

// TestPodHashMatchesWhatTheOperatorStamped is the property Task 4's rollout
// decision depends on: recomputing the desired hash for a group has to equal
// the hash BuildProxyPod already stamped on a pod it built for the identical
// inputs, or every comparison the rollout makes would read a fresh pod as
// stale against itself.
func TestPodHashMatchesWhatTheOperatorStamped(t *testing.T) {
	net, group := testNetwork(), testProxyGroup()
	pod, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want, err := DesiredProxyHash(net, group, testEndpoint)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if want != pod.Labels[LabelPodHash] {
		t.Errorf("DesiredProxyHash = %q, want the stamped %q", want, pod.Labels[LabelPodHash])
	}
}
