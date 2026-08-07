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

package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func networkReconciler(f *fixture) *NetworkReconciler {
	return &NetworkReconciler{
		Client:   f.c,
		Scheme:   f.reconc.Scheme,
		Recorder: record.NewFakeRecorder(100),
		Clock:    f.clock.Now,
	}
}

func (f *fixture) reconcileNetwork(t *testing.T, r *NetworkReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("reconcile network %s: %v", name, err)
	}
}

// getNetwork re-reads a Network. Named getNetwork, not network, because the
// fixture already carries a field of that name (its own bootstrap Network) —
// a field and a method cannot share an identifier on the same type.
func (f *fixture) getNetwork(t *testing.T, name string) *spawneryv1alpha1.Network {
	t.Helper()
	net := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, net); err != nil {
		t.Fatalf("get network %s: %v", name, err)
	}
	return net
}

// rejectNetwork sets a Network's own Accepted condition to False/
// DuplicateNetwork directly through the status client, standing in for
// whatever the Network controller's real one-per-namespace verdict would
// have produced. Reproducing that verdict for real needs a competitor whose
// creationTimestamp beats this Network's, which the name tie-break can only
// guarantee when the two are created back to back — exactly what
// TestSecondNetworkInTheSameNamespaceIsRejected and
// TestGroupPointingAtARejectedNetworkCreatesNoServers do. A test that first
// does real work (bringing a server up, several reconciles) before creating
// the competitor can no longer rely on that tie: by then the two
// creationTimestamps typically fall in different seconds, and the Network
// created first wins on real elapsed time regardless of name — the failure
// mode this helper sidesteps. Tests using it are exercising
// ServerGroupReconciler's reaction to a rejected Network, not the Network
// controller's own tie-break, which is covered elsewhere.
func rejectNetwork(t *testing.T, f *fixture, name string) {
	t.Helper()
	net := f.getNetwork(t, name)
	meta.SetStatusCondition(&net.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  metav1.ConditionFalse,
		Reason:  spawneryv1alpha1.ReasonDuplicateNetwork,
		Message: "rejected for the test, standing in for the Network controller's verdict",
	})
	if err := f.c.Status().Update(f.ctx, net); err != nil {
		t.Fatalf("reject network %s: %v", name, err)
	}
}

func TestFirstNetworkIsAccepted(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	f.reconcileNetwork(t, r, "production")

	got := f.getNetwork(t, "production")
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Errorf("conditions = %+v, want Accepted=True", got.Status.Conditions)
	}
}

func TestSecondNetworkInTheSameNamespaceIsRejected(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	// The fixture's network already exists. Create a younger one.
	f.clock.Advance(time.Minute)
	second := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "other-secret"},
		},
	}
	if err := f.c.Create(f.ctx, second); err != nil {
		t.Fatalf("create second network: %v", err)
	}

	f.reconcileNetwork(t, r, "production")
	f.reconcileNetwork(t, r, "staging")

	if !hasCondition(f.getNetwork(t, "production").Status.Conditions,
		spawneryv1alpha1.ConditionAccepted, metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Error("the older network must stay accepted")
	}
	if !hasCondition(f.getNetwork(t, "staging").Status.Conditions,
		spawneryv1alpha1.ConditionAccepted, metav1.ConditionFalse, spawneryv1alpha1.ReasonDuplicateNetwork) {
		t.Errorf("conditions = %+v, want Accepted=False/DuplicateNetwork",
			f.getNetwork(t, "staging").Status.Conditions)
	}
}

// networkAt builds a Network with an explicit creation timestamp, which is the
// only way to test the age rule at all. A real API server stamps
// creationTimestamp itself, at one-second resolution, and the fixture's clock
// does not reach it — two Networks created back to back land in the same second
// and the name tiebreak alone decides, which gives the same verdict whichever
// way the age comparison points.
func networkAt(name string, offsetSeconds int, deleting bool) spawneryv1alpha1.Network {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	n := spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "minecraft",
			CreationTimestamp: metav1.NewTime(base.Add(time.Duration(offsetSeconds) * time.Second)),
		},
	}
	if deleting {
		gone := metav1.NewTime(base.Add(time.Hour))
		n.DeletionTimestamp = &gone
		n.Finalizers = []string{"spawnery.cloud/test"}
	}
	return n
}

// TestPickNamespaceOwnerLetsAgeDecide is the test the one-network-per-namespace
// rule did not have. Every name below is chosen so that a rule going by the
// name alone, or by the newest timestamp, picks a different winner than the
// right one — otherwise flipping the comparison in pickNamespaceOwner would
// leave the suite green, and one stray kubectl apply of a Network would then
// reject the running one and stop every ServerGroup in the namespace from
// sizing.
func TestPickNamespaceOwnerLetsAgeDecide(t *testing.T) {
	cases := []struct {
		name     string
		networks []spawneryv1alpha1.Network
		want     string
	}{
		{
			// The oldest has the largest name, so the name cannot be what wins.
			name: "the oldest network wins",
			networks: []spawneryv1alpha1.Network{
				networkAt("zulu", 0, false),
				networkAt("alpha", 600, false),
			},
			want: "zulu",
		},
		{
			// Same list, the other way round: the answer must not depend on the
			// order the API server happened to return them in.
			name: "the oldest network wins whatever order it is listed in",
			networks: []spawneryv1alpha1.Network{
				networkAt("alpha", 600, false),
				networkAt("zulu", 0, false),
			},
			want: "zulu",
		},
		{
			// Only now, with nothing to choose on age, does the name decide —
			// and it has to, or the winner would flip between reconciles.
			name: "equal timestamps fall through to the name",
			networks: []spawneryv1alpha1.Network{
				networkAt("zulu", 300, false),
				networkAt("alpha", 300, false),
			},
			want: "alpha",
		},
		{
			// The owner is being deleted. The namespace goes to the next oldest,
			// not to the smallest name — "alpha" is younger and must lose.
			name: "deleting the owner hands over to the next oldest, not the smallest name",
			networks: []spawneryv1alpha1.Network{
				networkAt("zulu", 0, true),
				networkAt("middle", 300, false),
				networkAt("alpha", 600, false),
			},
			want: "middle",
		},
		{
			name:     "an empty namespace has no owner",
			networks: nil,
			want:     "",
		},
		{
			name: "a namespace whose only network is going away has no owner",
			networks: []spawneryv1alpha1.Network{
				networkAt("zulu", 0, true),
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickNamespaceOwner(tc.networks); got != tc.want {
				t.Errorf("pickNamespaceOwner() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPickNamespaceOwnerNeverFlips is the property the tiebreak exists for. The
// verdict is recomputed from scratch on every reconcile of every Network in the
// namespace, and the API server gives no order guarantee across those calls. A
// winner that depends on the order would hand the namespace back and forth,
// and every ServerGroup in it would be accepted and rejected in turn.
func TestPickNamespaceOwnerNeverFlips(t *testing.T) {
	// Two of these share a timestamp, so both halves of the rule are in play.
	networks := []spawneryv1alpha1.Network{
		networkAt("zulu", 0, false),
		networkAt("alpha", 300, false),
		networkAt("beta", 300, false),
		networkAt("gone", -600, true),
	}

	const want = "zulu"
	for _, order := range permutations(len(networks)) {
		shuffled := make([]spawneryv1alpha1.Network, 0, len(networks))
		for _, i := range order {
			shuffled = append(shuffled, networks[i])
		}
		for pass := 0; pass < 2; pass++ {
			if got := pickNamespaceOwner(shuffled); got != want {
				t.Fatalf("order %v pass %d: pickNamespaceOwner() = %q, want %q", order, pass, got, want)
			}
		}
	}
}

// permutations returns every ordering of the indices 0..n-1.
func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for _, rest := range permutations(n - 1) {
		for pos := 0; pos <= len(rest); pos++ {
			p := make([]int, 0, n)
			p = append(p, rest[:pos]...)
			p = append(p, n-1)
			p = append(p, rest[pos:]...)
			out = append(out, p)
		}
	}
	return out
}

func TestNetworkCountsItsGroups(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)
	gr := groupReconciler(f)

	f.reconcileGroup(t, gr)
	srv := f.listServers(t)[0]
	uid := bringUpNamed(t, f, srv.Name)
	if err := f.agents.ReportPlayers(uid, 9, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile(srv.Name)
	f.reconcileGroup(t, gr)
	f.reconcileNetwork(t, r, "production")

	got := f.getNetwork(t, "production")
	if got.Status.ServerGroups != 1 {
		t.Errorf("serverGroups = %d, want 1", got.Status.ServerGroups)
	}
	if got.Status.ProxyGroups != 0 {
		t.Errorf("proxyGroups = %d, want 0", got.Status.ProxyGroups)
	}
	if got.Status.OnlinePlayers != 9 {
		t.Errorf("onlinePlayers = %d, want 9", got.Status.OnlinePlayers)
	}
}
