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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

func networkReconciler(f *fixture) *NetworkReconciler {
	r, _ := networkReconcilerWithEvents(f)
	return r
}

// networkReconcilerWithEvents hands back the recorder too, which the forwarding
// secret tests need: the events are emitted on entering a state, so proving
// "exactly once" means reading the channel rather than the object.
func networkReconcilerWithEvents(f *fixture) (*NetworkReconciler, *record.FakeRecorder) {
	events := record.NewFakeRecorder(100)
	return &NetworkReconciler{
		Client:       f.c,
		Scheme:       f.reconc.Scheme,
		Recorder:     events,
		Clock:        f.clock.Now,
		SecretReader: f.c,
	}, events
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

func putForwardingSecret(t *testing.T, f *fixture, value string) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "velocity-forwarding-secret", Namespace: f.ns},
		Data:       map[string][]byte{podspec.ForwardingSecretKey: []byte(value)},
	}
	if err := f.c.Create(f.ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create secret: %v", err)
		}
		existing := &corev1.Secret{}
		if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(secret), existing); err != nil {
			t.Fatalf("get secret: %v", err)
		}
		existing.Data = secret.Data
		if err := f.c.Update(f.ctx, existing); err != nil {
			t.Fatalf("update secret: %v", err)
		}
	}
}

func countEvents(events []string, reason string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, reason) {
			n++
		}
	}
	return n
}

// The first sight of a secret is adoption, not rotation. Emitting an event
// there would mean every operator start announces a rotation that never
// happened, on every network at once.
func TestFirstSightOfTheForwardingSecretIsAdoption(t *testing.T) {
	f := newFixture(t)
	r, events := networkReconcilerWithEvents(f)
	putForwardingSecret(t, f, "first")

	f.reconcileNetwork(t, r, "production")

	got := f.getNetwork(t, "production")
	if got.Status.ForwardingSecretHash == "" {
		t.Error("status.forwardingSecretHash is empty after a successful read")
	}
	for _, e := range drainEvents(events) {
		if strings.Contains(e, spawneryv1alpha1.EventForwardingSecretRotated) {
			t.Errorf("the first read emitted %q; an empty recorded hash is adoption", e)
		}
	}
}

// The event fires on the transition and not once per resync: at a five-second
// requeue, an event per pass would be seven hundred an hour for one unremedied
// rotation.
func TestARotationIsAnnouncedExactlyOnce(t *testing.T) {
	f := newFixture(t)
	r, events := networkReconcilerWithEvents(f)
	putForwardingSecret(t, f, "first")
	f.reconcileNetwork(t, r, "production")
	drainEvents(events)

	putForwardingSecret(t, f, "second")
	f.reconcileNetwork(t, r, "production")
	first := drainEvents(events)
	f.reconcileNetwork(t, r, "production")
	second := drainEvents(events)

	if n := countEvents(first, spawneryv1alpha1.EventForwardingSecretRotated); n != 1 {
		t.Errorf("the rotation emitted %d events, want exactly 1: %v", n, first)
	}
	if n := countEvents(second, spawneryv1alpha1.EventForwardingSecretRotated); n != 0 {
		t.Errorf("the next reconcile emitted %d more events, want 0: %v", n, second)
	}
}

// A stale pod is the whole signal. It is created by hand here rather than by a
// group controller, because what is under test is the comparison and not how
// pods come to exist.
func TestAStalePodRaisesRotationPending(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	putForwardingSecret(t, f, "first")
	f.reconcileNetwork(t, r, "production")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lobby-0",
			Namespace: f.ns,
			Labels: map[string]string{
				podspec.LabelManagedBy:      podspec.ManagedByValue,
				podspec.LabelNetwork:        "production",
				podspec.LabelGroup:          "lobby",
				podspec.LabelRole:           podspec.RoleServer,
				podspec.LabelForwardingHash: "0000000000000000",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "paper", Image: "img:1"}}},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	f.reconcileNetwork(t, r, "production")

	got := f.getNetwork(t, "production")
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionForwardingSecretRotationPending,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonRotationPending) {
		t.Errorf("conditions = %+v, want RotationPending=True/RotationPending", got.Status.Conditions)
	}
}

// Accepted is what servergroup_controller.go derives networkUsable from, and
// since 5b mayResize equals networkUsable. A missing secret must not reach it,
// or a typo in one field stops the network from sizing at all.
func TestAMissingSecretLeavesAcceptedAlone(t *testing.T) {
	f := newFixture(t)
	r, events := networkReconcilerWithEvents(f)

	// newFixture's own bootstrap reconcile already read this (nonexistent)
	// secret once, to its own throwaway recorder, so
	// ForwardingSecretResolved already carries SecretNotFound by this point.
	// Clear it so the reconcile below is a genuine entry into that state —
	// which is what "exactly one event" below is actually testing.
	net := f.getNetwork(t, "production")
	meta.RemoveStatusCondition(&net.Status.Conditions, spawneryv1alpha1.ConditionForwardingSecretResolved)
	if err := f.c.Status().Update(f.ctx, net); err != nil {
		t.Fatalf("reset forwarding secret condition: %v", err)
	}

	f.reconcileNetwork(t, r, "production")

	got := f.getNetwork(t, "production")
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Errorf("conditions = %+v, want Accepted=True despite the missing secret", got.Status.Conditions)
	}
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionForwardingSecretResolved,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonSecretNotFound) {
		t.Errorf("conditions = %+v, want ForwardingSecretResolved=False/SecretNotFound", got.Status.Conditions)
	}
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionForwardingSecretRotationPending,
		metav1.ConditionUnknown, spawneryv1alpha1.ReasonSecretUnresolved) {
		t.Errorf("conditions = %+v, want RotationPending=Unknown/SecretUnresolved", got.Status.Conditions)
	}
	if n := countEvents(drainEvents(events), spawneryv1alpha1.EventForwardingSecretNotFound); n != 1 {
		t.Errorf("the missing secret emitted %d events, want exactly 1", n)
	}

	// The next reconcile is still in SecretNotFound, not entering it — the
	// hasConditionReason guard must suppress this one, or an unremedied
	// missing secret announces itself roughly seven hundred times an hour.
	f.reconcileNetwork(t, r, "production")
	if n := countEvents(drainEvents(events), spawneryv1alpha1.EventForwardingSecretNotFound); n != 0 {
		t.Errorf("the next reconcile emitted %d more events, want 0", n)
	}
}
