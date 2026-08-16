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
	"encoding/hex"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/render"
)

func TestPodHashIsStableAcrossBuilds(t *testing.T) {
	net, group := testNetwork(), testProxyGroup()
	a, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	b, err := BuildProxyPod(net, group, "gateway-bbbb", testEndpoint, nil)
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
	before, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	group.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	after, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint, nil)
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
	before, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	group.Spec.Replicas = 5
	after, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint, nil)
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
	pod, err := BuildProxyPod(net, group, "gateway-aaaa", testEndpoint, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want, err := DesiredProxyHash(net, group, testEndpoint, nil)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if want != pod.Labels[LabelPodHash] {
		t.Errorf("DesiredProxyHash = %q, want the stamped %q", want, pod.Labels[LabelPodHash])
	}
}

// motd reaches only the ConfigMap, never the rendered pod, so a pod-only digest
// left it invisible: the ConfigMap updated, no proxy went stale, DecideRollout
// ordered nothing, and there is no reload path. Shipped since 4c-2.
//
// playerLimit is here beside it precisely because it is *not* broken -- it
// rides in the pod as SPAWNERY_PLAYER_LIMIT -- so that a later refactor moving
// that env var out of the pod cannot break it in silence.
func TestDesiredProxyHashSeesTheConfigValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*spawneryv1alpha1.ProxyGroup)
	}{
		{
			name: "motd",
			mutate: func(g *spawneryv1alpha1.ProxyGroup) {
				g.Spec.Config.Motd = "a different motd"
			},
		},
		{
			name: "playerLimit",
			mutate: func(g *spawneryv1alpha1.ProxyGroup) {
				g.Spec.Config.PlayerLimit = 999
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net, group := proxyHashFixtures(t)
			group.Spec.Config = &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 20, Motd: "before"}

			before, err := DesiredProxyHash(net, group, testEndpoint, proxyValuesBytes(t, group))
			if err != nil {
				t.Fatalf("baseline: %v", err)
			}

			tc.mutate(group)
			after, err := DesiredProxyHash(net, group, testEndpoint, proxyValuesBytes(t, group))
			if err != nil {
				t.Fatalf("mutated: %v", err)
			}

			if before == after {
				t.Fatalf("changing %s left the hash at %q, so no proxy would ever go stale for it", tc.name, before)
			}
		})
	}
}

func proxyHashFixtures(t *testing.T) (*spawneryv1alpha1.Network, *spawneryv1alpha1.ProxyGroup) {
	t.Helper()
	return testNetwork(), testProxyGroup()
}

func proxyValuesBytes(t *testing.T, group *spawneryv1alpha1.ProxyGroup) []byte {
	t.Helper()
	// The production path marshals controller.proxyConfigValues(group); this
	// mirrors only the two fields under test, which is enough to prove the
	// bytes reach the digest.
	limit := group.Spec.Config.PlayerLimit
	motd := group.Spec.Config.Motd
	data, err := yaml.Marshal(render.Values{PlayerLimit: &limit, Motd: &motd})
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	return data
}

func serverHashFixtures(t *testing.T) (*spawneryv1alpha1.Network, *spawneryv1alpha1.ServerGroup) {
	t.Helper()
	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "fwd"},
		},
	}
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "n"},
			Type:       spawneryv1alpha1.ServerGroupPersistent,
			Image:      "img:1",
			MaxPlayers: 20,
			Replicas:   ptr.To(int32(1)),
			Storage:    &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
		},
	}
	return net, group
}

// The hash must not flap between passes: a PodSpec carries maps, and Go's map
// iteration order is unspecified. An unstable digest would restart every world
// on every operator restart, which is worse than the problem 5b solves.
func TestDesiredServerHashIsStableAcrossRuns(t *testing.T) {
	net, group := serverHashFixtures(t)
	values := []byte("maxPlayers: 20\n")

	first, err := DesiredServerHash(net, group, values)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := DesiredServerHash(net, group, values)
		if err != nil {
			t.Fatalf("DesiredServerHash run %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("run %d gave %q, first run gave %q", i, again, first)
		}
	}
}

// The discrimination table is the whole point of the hash: it says, as a list a
// person can read, which edits restart a world and which do not.
func TestDesiredServerHashDiscriminates(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*spawneryv1alpha1.ServerGroup)
		values  []byte
		changed bool
	}{
		{
			name:    "image changes it",
			mutate:  func(g *spawneryv1alpha1.ServerGroup) { g.Spec.Image = "img:2" },
			changed: true,
		},
		{
			name:    "maxPlayers changes it, through the config values",
			values:  []byte("maxPlayers: 40\n"),
			changed: true,
		},
		{
			// BuildServerPod never reads Replicas, so this row cannot fail
			// today on that account. What it guards against is
			// DesiredServerHash's own marshalled struct (hash.go) growing a
			// Replicas field directly — the mutation this row exists to
			// catch, not any path already reachable through the pod.
			name:    "replicas does not change it",
			mutate:  func(g *spawneryv1alpha1.ServerGroup) { g.Spec.Replicas = ptr.To(int32(5)) },
			changed: false,
		},
		{
			// Same guard as above, for Drain.TimeoutSeconds: BuildServerPod
			// does not read it either, so this row watches for
			// DesiredServerHash's marshalled struct being widened to
			// include it directly, not for a path through the pod that
			// exists today.
			name: "drain.timeoutSeconds does not change it",
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Drain = &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 999}
			},
			changed: false,
		},
	}

	base := []byte("maxPlayers: 20\n")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net, group := serverHashFixtures(t)
			before, err := DesiredServerHash(net, group, base)
			if err != nil {
				t.Fatalf("baseline: %v", err)
			}

			if tc.mutate != nil {
				tc.mutate(group)
			}
			values := base
			if tc.values != nil {
				values = tc.values
			}
			after, err := DesiredServerHash(net, group, values)
			if err != nil {
				t.Fatalf("mutated: %v", err)
			}

			if tc.changed && before == after {
				t.Fatalf("expected the hash to change, both are %q", before)
			}
			if !tc.changed && before != after {
				t.Fatalf("expected the hash to hold, got %q then %q", before, after)
			}
		})
	}
}

// TestDesiredServerHashHasNoPerServerInput does not call DesiredServerHash
// twice to compare outputs: its signature — DesiredServerHash(net, group,
// configValues) — admits no *Server at all, so there is no per-server value
// (name, ordinal, claim) that could reach the digest, and the compiler
// enforces that, not this test.
//
// What this test actually guards is the fixture's premise: that two ordinals
// of this group really would render two different pods if DesiredServerHash
// were, hypothetically, called per-server. BuildServerPod is exercised
// directly here for that reason — proving pod0.Spec != pod1.Spec is what
// makes "the digest can't distinguish them" a meaningful property of the API
// shape rather than a coincidence of a fixture where they'd have been
// identical anyway. The non-empty digest check is a basic sanity check on
// top, not a proof of the identity-independence claim.
func TestDesiredServerHashHasNoPerServerInput(t *testing.T) {
	net, group := serverHashFixtures(t)
	values := []byte("maxPlayers: 20\n")

	got, err := DesiredServerHash(net, group, values)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	pod0, err := BuildServerPod(net, group, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "g-0", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{Ordinal: ptr.To(int32(0))},
	}, "agent:9443")
	if err != nil {
		t.Fatalf("BuildServerPod g-0: %v", err)
	}
	pod1, err := BuildServerPod(net, group, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "g-1", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{Ordinal: ptr.To(int32(1))},
	}, "agent:9443")
	if err != nil {
		t.Fatalf("BuildServerPod g-1: %v", err)
	}
	if reflect.DeepEqual(pod0.Spec, pod1.Spec) {
		t.Fatal("fixture is not exercising the property: two ordinals rendered identical pods")
	}
	if got == "" {
		t.Fatal("empty hash")
	}
}

// ForwardingHash is what tells a rotation from a steady state, and it becomes
// a pod label, so five separate properties have to hold at once. They are one
// test because each is a single comparison and splitting them would repeat the
// fixture five times.
func TestForwardingHashIsStableSaltedAndUntrimmed(t *testing.T) {
	const uidA = types.UID("11111111-2222-3333-4444-555555555555")
	const uidB = types.UID("99999999-8888-7777-6666-555555555555")
	value := []byte("s3cret")

	got := ForwardingHash(uidA, value)

	if again := ForwardingHash(uidA, value); again != got {
		t.Errorf("ForwardingHash is not stable: %q then %q", got, again)
	}
	if len(got) != 16 {
		t.Errorf("ForwardingHash = %q, want 16 hex characters so it fits a label value", got)
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Errorf("ForwardingHash returned %q, which is not hex: %v", got, err)
	}
	if other := ForwardingHash(uidB, value); other == got {
		t.Errorf("the same secret in two networks hashed alike (%q); the UID salt is not reaching the digest", got)
	}
	if trailing := ForwardingHash(uidA, []byte("s3cret\n")); trailing == got {
		t.Error("a trailing newline hashed alike; the value is being trimmed somewhere it must not be")
	}
}

// Without a separator between the salt and the value, ("ab", "c") and
// ("a", "bc") are the same byte sequence and hash alike. Real UIDs are all the
// same length so this could never bite in production -- which is exactly why it
// needs a test rather than a reader noticing it.
func TestForwardingHashSeparatesTheSaltFromTheValue(t *testing.T) {
	if ForwardingHash("ab", []byte("c")) == ForwardingHash("a", []byte("bc")) {
		t.Error("the UID and the value run together in the digest; the separator byte is missing")
	}
}
