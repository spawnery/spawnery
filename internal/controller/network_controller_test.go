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
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

func networkReconciler(f *fixture) *NetworkReconciler {
	r, _ := networkReconcilerWithEvents(f)
	return r
}

// networkReconcilerWithEvents hands back the recorder too, which the forwarding
// secret tests need: the events are emitted on entering a state, so proving
// "exactly once" means reading the channel rather than the object.
func networkReconcilerWithEvents(f *fixture) (*NetworkReconciler, *nonBlockingRecorder) {
	rec := newRecorder()
	return &NetworkReconciler{
		Client:   f.rc,
		Scheme:   f.reconc.Scheme,
		Recorder: rec,
		// Matches what config/deploy/ installs; the NetworkPolicy tests need
		// this non-empty, and no other test cares what it is.
		OperatorNamespace: "spawnery-system",
		SecretReader:      f.c,
		// Every test that reconciles a Network now bootstraps its namespace.
		// A test that cares about the bundle replaces this; the rest need it
		// only to be non-nil and to return something.
		Bootstrap: &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return []byte("PEM-FIXTURE") }},
	}, rec
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

// countEvents counts the events carrying a given reason. It matched the reason
// as a substring of the whole rendered line until milestone 6e's final review;
// eventHasReason says why that was wrong.
func countEvents(events []string, reason string) int {
	n := 0
	for _, e := range events {
		if eventHasReason(e, reason) {
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

// policyKey is where a network's policy lives, so no test has to restate it.
func policyKey(f *fixture, network string) types.NamespacedName {
	return types.NamespacedName{
		Namespace: f.ns,
		Name:      podspec.NetworkPolicyName(network),
	}
}

// TestAnAcceptedNetworkGetsItsPolicy is the milestone's central object claim.
func TestAnAcceptedNetworkGetsItsPolicy(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	f.reconcileNetwork(t, r, "production")

	var policy networkingv1.NetworkPolicy
	if err := f.c.Get(f.ctx, policyKey(f, "production"), &policy); err != nil {
		t.Fatalf("get the network policy: %v", err)
	}
	if got := policy.Spec.PodSelector.MatchLabels[podspec.LabelRole]; got != podspec.RoleServer {
		t.Errorf("policy selects role %q, want %q", got, podspec.RoleServer)
	}
	network := f.getNetwork(t, "production")
	if len(policy.OwnerReferences) != 1 || policy.OwnerReferences[0].UID != network.UID {
		t.Errorf("owner references = %v, want one naming the Network's UID %s",
			policy.OwnerReferences, network.UID)
	}
}

// TestARejectedNetworkWritesNoPolicy: pickNamespaceOwner already decides which
// Network owns a namespace when several exist. If the loser wrote one too, two
// Network objects would overwrite each other's policy on every pass, and which
// object survived would depend on reconcile ordering.
func TestARejectedNetworkWritesNoPolicy(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	// The fixture's "production" already exists; a younger one loses, because
	// age decides before the name does.
	f.clock.Advance(time.Minute)
	loser := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "other-secret"},
		},
	}
	if err := f.c.Create(f.ctx, loser); err != nil {
		t.Fatalf("create the second network: %v", err)
	}

	f.reconcileNetwork(t, r, "staging")

	var policy networkingv1.NetworkPolicy
	err := f.c.Get(f.ctx, policyKey(f, "staging"), &policy)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a rejected Network wrote a policy (err = %v); two Networks in "+
			"one namespace would then fight over the namespace's traffic rules", err)
	}
}

// TestADeletedPolicyComesBack: the policy is a security control, so removing it
// by hand must not be a durable way to switch it off.
func TestADeletedPolicyComesBack(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)
	f.reconcileNetwork(t, r, "production")

	var policy networkingv1.NetworkPolicy
	if err := f.c.Get(f.ctx, policyKey(f, "production"), &policy); err != nil {
		t.Fatalf("get the network policy: %v", err)
	}
	before := policy.UID
	if err := f.c.Delete(f.ctx, &policy); err != nil {
		t.Fatalf("delete the network policy: %v", err)
	}

	f.reconcileNetwork(t, r, "production")

	if err := f.c.Get(f.ctx, policyKey(f, "production"), &policy); err != nil {
		t.Fatalf("the policy did not come back: %v", err)
	}
	// The UID and not merely the presence. envtest's delete is synchronous and
	// this fixture records the mutation, so the object here cannot be the one
	// that was deleted -- but nothing in this test said so, and "it is still
	// there" is exactly what a delete that silently did not happen looks like.
	// A different identity is the only thing that distinguishes recreated from
	// never removed.
	if policy.UID == before {
		t.Errorf("the policy carries its original UID %s, so it was never actually deleted "+
			"and this test proves nothing about recreation", before)
	}
}

// TestThePolicyCarriesTheLabelsAHumanReadsIt guards metadata nothing selects
// on, which is why nothing else would catch a wrong value. Both labels exist
// for somebody reading kubectl output in a namespace with more than one
// Network in its history, and a policy carrying the wrong network name there
// is worse than one carrying none: it answers the question wrongly instead of
// not answering it.
func TestThePolicyCarriesTheLabelsAHumanReadsIt(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)
	f.reconcileNetwork(t, r, "production")

	var policy networkingv1.NetworkPolicy
	if err := f.c.Get(f.ctx, policyKey(f, "production"), &policy); err != nil {
		t.Fatalf("get the network policy: %v", err)
	}
	for key, want := range map[string]string{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelNetwork:   "production",
	} {
		if got := policy.Labels[key]; got != want {
			t.Errorf("policy label %s = %q, want %q", key, got, want)
		}
	}
}

// TestTheOperatorNamespaceReachesTheEgressRule guards the one value the policy
// cannot derive from the Network it protects. The agent endpoint is assembled
// from the operator's own namespace, which is a flag (--operator-namespace), so
// a policy hard-coding "spawnery-system" would be correct only by coincidence
// in any installation that moved it.
func TestTheOperatorNamespaceReachesTheEgressRule(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)
	r.OperatorNamespace = "spawnery-elsewhere"

	f.reconcileNetwork(t, r, "production")

	var policy networkingv1.NetworkPolicy
	if err := f.c.Get(f.ctx, policyKey(f, "production"), &policy); err != nil {
		t.Fatalf("get the network policy: %v", err)
	}
	last := policy.Spec.Egress[len(policy.Spec.Egress)-1]
	if last.To[0].NamespaceSelector == nil {
		t.Fatalf("the operator egress rule has no namespace selector: %+v", last.To[0])
	}
	got := last.To[0].NamespaceSelector.MatchLabels[podspec.NamespaceNameLabel]
	if got != "spawnery-elsewhere" {
		t.Errorf("egress names namespace %q, want spawnery-elsewhere", got)
	}
}

// A namespace where nothing starts still tracks the operator's CA.
//
// Before this test's subject existed, Bootstrapper.Ensure ran only from
// ServerReconciler, on the path that creates a pod
// (server_controller.go:304). A namespace whose pods were all already
// running -- or which had none at all -- kept whatever ca.crt it was given
// the last time a pod happened to be created there, however long ago. That
// is the second half of docs/known-issues.md's "The CA has no rotation
// procedure", and it is what makes a rotation's overlap window impossible to
// close: the operator cannot tell whether a quiet namespace has the new
// bundle yet.
//
// No pod is created anywhere in this test. That is the whole point of it.
func TestAQuietNamespaceFollowsTheCABundle(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)

	ca := []byte("PEM-FIRST")
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return ca }}

	f.reconcileNetwork(t, r, f.network.Name)

	read := func() string {
		t.Helper()
		var cm corev1.ConfigMap
		key := client.ObjectKey{Namespace: f.ns, Name: podspec.CAConfigMapName}
		if err := f.c.Get(f.ctx, key, &cm); err != nil {
			t.Fatalf("read the CA ConfigMap back: %v", err)
		}
		return cm.Data[podspec.CAConfigMapKey]
	}

	if got := read(); got != string(ca) {
		t.Fatalf("ca.crt = %q after the first reconcile, want %q", got, ca)
	}

	ca = []byte("PEM-SECOND")
	f.reconcileNetwork(t, r, f.network.Name)

	if got := read(); got != string(ca) {
		t.Errorf("ca.crt = %q after the bundle changed, want %q. The namespace is quiet -- "+
			"no pod was created in it -- so nothing but the Network's own reconcile can "+
			"bring the new bundle here", got, ca)
	}
}

// The Network that does not own its namespace bootstraps nothing.
//
// pickNamespaceOwner gives the namespace to the oldest Network, and the
// loser's reconcile returns before it writes anything. That already governed
// the NetworkPolicy; it governs the CA ConfigMap for the same reason, and
// this test exists because the new call sits close enough to the acceptance
// branch that moving it above one line would silently change that.
func TestALosingNetworkDoesNotBootstrapTheNamespace(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return []byte("PEM-A") }}

	younger := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	if err := f.c.Create(f.ctx, younger); err != nil {
		t.Fatalf("create the younger Network: %v", err)
	}

	key := client.ObjectKey{Namespace: f.ns, Name: podspec.CAConfigMapName}
	var cm corev1.ConfigMap
	if err := f.c.Get(f.ctx, key, &cm); err == nil {
		if err := f.c.Delete(f.ctx, &cm); err != nil {
			t.Fatalf("clear the ConfigMap before the loser reconciles: %v", err)
		}
	}

	f.reconcileNetwork(t, r, younger.Name)

	if err := f.c.Get(f.ctx, key, &cm); err == nil {
		t.Error("the losing Network created the CA ConfigMap; only the namespace's owner writes here")
	}
}

// The objects Ensure writes carry no OwnerReference, and that is deliberate:
// they are meant to outlive the operator so a pod restarting during an
// outage still finds a CA to trust and a ServiceAccount to authenticate
// with. Making the Network own them is the tidy-looking change this design
// refused, and it would delete a running fleet's trust anchor and its
// identity the moment somebody deleted a Network. Asserted here because
// "tidy up the ownership" is exactly the kind of edit that arrives later
// with a green suite.
//
// All three objects, not just the ConfigMap: a pod that keeps its CA but
// loses its ServiceAccount cannot mint a token, and its projected volume
// never fills.
func TestTheCAConfigMapIsOwnedByNothing(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return []byte("PEM-A") }}

	// Cleared first so the objects read back below are the ones this
	// reconcile creates, not the ones newFixture's own reconcile left here.
	key := client.ObjectKey{Namespace: f.ns, Name: podspec.CAConfigMapName}
	if err := f.c.Delete(f.ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: podspec.CAConfigMapName, Namespace: f.ns},
	}); err != nil {
		t.Fatalf("clear the fixture's CA ConfigMap: %v", err)
	}
	serviceAccounts := []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName}
	for _, name := range serviceAccounts {
		if err := f.c.Delete(f.ctx, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		}); err != nil {
			t.Fatalf("clear the fixture's %s ServiceAccount: %v", name, err)
		}
	}

	f.reconcileNetwork(t, r, f.network.Name)

	var cm corev1.ConfigMap
	if err := f.c.Get(f.ctx, key, &cm); err != nil {
		t.Fatalf("read the CA ConfigMap back: %v", err)
	}
	if len(cm.OwnerReferences) != 0 {
		t.Errorf("the CA ConfigMap has %d owner reference(s): %v. It must have none — "+
			"deleting a Network would otherwise take the trust anchor of every pod still "+
			"running in the namespace with it", len(cm.OwnerReferences), cm.OwnerReferences)
	}
	for _, name := range serviceAccounts {
		var sa corev1.ServiceAccount
		if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: name}, &sa); err != nil {
			t.Fatalf("read the %s ServiceAccount back: %v", name, err)
		}
		if len(sa.OwnerReferences) != 0 {
			t.Errorf("the %s ServiceAccount has %d owner reference(s): %v. It must have "+
				"none — deleting a Network would otherwise strip the identity every pod "+
				"still running in the namespace authenticates with", name,
				len(sa.OwnerReferences), sa.OwnerReferences)
		}
	}
}

// A reconcile that runs before certs.Provider has published fails and
// requeues rather than passing quietly. Swallowing it would leave the
// silently stale ConfigMap this whole change exists to prevent, and
// ServerReconciler already treats the same call the same way.
func TestAReconcileWithoutACABundleFails(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return nil }}

	// newFixture already accepted this Network once, with a real CA, so the
	// ConfigMap exists before this test's own reconcile runs. Clear it first
	// so "no ConfigMap after" below tests this reconcile, not the fixture's.
	key := client.ObjectKey{Namespace: f.ns, Name: podspec.CAConfigMapName}
	var existing corev1.ConfigMap
	if err := f.c.Get(f.ctx, key, &existing); err == nil {
		if err := f.c.Delete(f.ctx, &existing); err != nil {
			t.Fatalf("clear the ConfigMap the fixture wrote: %v", err)
		}
	}

	_, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: f.ns, Name: f.network.Name},
	})
	if err == nil {
		t.Fatal("the reconcile succeeded with no CA bundle available")
	}
	// The whole wrapper, not the word "bootstrap": Ensure's own message
	// already starts "bootstrap namespace ...", so a substring match on that
	// word would pass with the reconcile's wrapper removed and would say
	// nothing about which step failed.
	const wrapper = "bootstrap the namespace: "
	if !strings.HasPrefix(err.Error(), wrapper) {
		t.Errorf("error = %v, want it to start with %q so the failing step is named "+
			"by this reconcile and not only by Ensure", err, wrapper)
	}

	var cm corev1.ConfigMap
	if err := f.c.Get(f.ctx, key, &cm); err == nil {
		t.Error("a ConfigMap was written despite there being no bundle to write")
	}
}

// A namespace whose CA cannot be written must still get a current status and
// an event saying why.
//
// Bootstrap.Ensure runs last, after r.Status().Update, because the state it
// reports on is not always the transient one. An empty bundle clears itself
// within seconds of process start; a ConfigMap write refused by an admission
// webhook, a ResourceQuota or a namespace policy stands until somebody removes
// it. With the call ahead of the status update, a reconcile in such a
// namespace records nothing at all for as long as the refusal lasts: a new
// Network never persists Accepted, and servergroup_controller.go and
// proxygroup_controller.go both gate on that condition, so every group in the
// namespace stops; an existing one keeps a stale Accepted=True while its
// player counts, its group counts and its rotation condition freeze. The
// error is still returned, so the reconcile still fails and requeues.
func TestABootstrapFailureStillWritesTheStatus(t *testing.T) {
	f := newFixture(t)
	r, rec := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return nil }}

	// newFixture creates its ServerGroup after the reconcile that accepted the
	// Network, so the stored status still counts none. A non-zero count below
	// can only have come from this reconcile's own status update.
	if got := f.getNetwork(t, f.network.Name).Status.ServerGroups; got != 0 {
		t.Fatalf("serverGroups = %d before the reconcile, want 0", got)
	}

	_, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: f.ns, Name: f.network.Name},
	})
	if err == nil {
		t.Fatal("the reconcile succeeded with no CA bundle available")
	}

	got := f.getNetwork(t, f.network.Name)
	if got.Status.ServerGroups != 1 {
		t.Errorf("serverGroups = %d, want 1 — a namespace the operator cannot bootstrap "+
			"must not stop the Network's status being written", got.Status.ServerGroups)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
		t.Errorf("conditions = %+v, want Accepted=True persisted; the groups in this "+
			"namespace are gated on it", got.Status.Conditions)
	}
	recorded := drainEvents(rec)
	if !containsEvent(recorded, ReasonNamespaceNotBootstrapped) {
		t.Errorf("events = %q, want one naming %s — a log line is the only other trace "+
			"a refused ConfigMap write leaves", recorded, ReasonNamespaceNotBootstrapped)
	}
}

// TestARefusedNetworkSaysSoAsAnEventToo closes the "a rejected Network
// produces no Kubernetes event, only a condition" item in
// docs/known-issues.md. A duplicate is the refusal a user is most likely to
// cause by hand — two Networks in one namespace — and it was the only one this
// reconciler made silently.
func TestARefusedNetworkSaysSoAsAnEventToo(t *testing.T) {
	f := newFixture(t)
	r, rec := networkReconcilerWithEvents(f)

	// f.network already owns the namespace; this one arrives second.
	loser := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "fwd"},
		},
	}
	if err := f.c.Create(f.ctx, loser); err != nil {
		t.Fatalf("create the second Network: %v", err)
	}
	f.reconcileNetwork(t, r, "staging")

	ev := drainEvents(rec)
	if !containsEvent(ev, spawneryv1alpha1.ReasonDuplicateNetwork) {
		t.Fatalf("events = %v, want one naming %s: a Network refused into an occupied "+
			"namespace did nothing observable unless somebody described that object",
			ev, spawneryv1alpha1.ReasonDuplicateNetwork)
	}
	if !containsEventType(ev, "Warning") {
		t.Errorf("events = %v, want it recorded as a Warning", ev)
	}

	// Once per transition, not once per pass. This branch runs for as long as
	// the duplicate stands, and a Warning every minute forever buries the one
	// that mattered instead of reporting it.
	f.reconcileNetwork(t, r, "staging")
	if ev := drainEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v on a second pass with nothing changed, want none", ev)
	}
}

// TestSiblingNetworksWakesTheLosersAndNotTheWinner covers the mapper behind the
// second watch on Network. docs/known-issues.md measures recovery after
// deleting the winning Network at roughly ninety seconds and names the cause:
// two requeues stacked, the loser's minute and the group's thirty seconds.
// This mapper removes the first.
//
// For() already enqueues the object an event is about, so the mapper must
// return the *others* — returning the subject too would be a second, pointless
// pass on every Network event in the cluster.
func TestSiblingNetworksWakesTheLosersAndNotTheWinner(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	for _, name := range []string{"staging", "canary"} {
		loser := &spawneryv1alpha1.Network{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
			Spec: spawneryv1alpha1.NetworkSpec{
				ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "fwd"},
			},
		}
		if err := f.c.Create(f.ctx, loser); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	got := r.siblingNetworks(f.ctx, f.network)
	names := make([]string, 0, len(got))
	for _, req := range got {
		if req.Namespace != f.ns {
			t.Errorf("request %v is outside the fixture's namespace", req)
		}
		names = append(names, req.Name)
	}
	sort.Strings(names)
	if !slices.Equal(names, []string{"canary", "staging"}) {
		t.Errorf("siblingNetworks(%s) = %v, want [canary staging]: the winner's own "+
			"deletion is what changes the verdict for every loser, and none of them is "+
			"the object that event names", f.network.Name, names)
	}
}

// TestARefusedNetworkStillCountsWhatPointsAtIt closes "the status of a rejected
// Network freezes and keeps reporting old numbers" in docs/known-issues.md.
//
// The ordinary case is worse than freezing. A Network created second is refused
// on its very first pass, before it has counted anything, so it reported zero
// however many groups were later pointed at it — and the count is precisely how
// somebody sees what is stranded behind the refusal.
func TestARefusedNetworkStillCountsWhatPointsAtIt(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	loser := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "fwd"},
		},
	}
	if err := f.c.Create(f.ctx, loser); err != nil {
		t.Fatalf("create the second Network: %v", err)
	}
	// Two groups behind the loser, and one behind the winner that must not be
	// credited to it: a namespace holding two Networks is what the duplicate
	// rule refuses, not what it prevents.
	f.createProxyGroup("stranded-proxy", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.NetworkRef = spawneryv1alpha1.ObjectRef{Name: "staging"}
	})
	f.createProxyGroup("the-winners-proxy")

	f.reconcileNetwork(t, r, "staging")

	got := f.getNetwork(t, "staging")
	if !meta.IsStatusConditionFalse(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
		t.Fatalf("Accepted = %+v, want False; this test is about a refused Network",
			meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted))
	}
	if got.Status.ProxyGroups != 1 {
		t.Errorf("status.proxyGroups = %d on a refused Network, want 1. The refusal is not a "+
			"reason to stop saying how much is waiting behind it", got.Status.ProxyGroups)
	}
}

// failingStatusWriter is a client whose status writes always fail. It exists
// for one property that cannot be observed any other way: an event must not be
// emitted when the write recording what it announces did not land.
type failingStatusWriter struct {
	client.Client
	err error
}

func (f failingStatusWriter) Status() client.SubResourceWriter {
	return failingSubResource{SubResourceWriter: f.Client.Status(), err: f.err}
}

type failingSubResource struct {
	client.SubResourceWriter
	err error
}

func (f failingSubResource) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return f.err
}

// TestARotationIsAnnouncedOnlyOnceTheStatusWriteLands closes the entry in
// docs/known-issues.md: `"exactly one event per transition" holds only if the
// status write lands`.
//
// Both forwarding-secret events fire on *entering* a state, and whether a pass
// is an entry is decided by the condition still in etcd. Emitting before the
// write meant anything failing in between — the pod List that returns on error,
// a conflict, a refused write — left the old status behind, so the retry found
// the same transition and announced it again. A rotation could be reported
// twice, or without end under a persistently failing update, while three places
// stated the property unconditionally: the runbook's §5 step 3, the event-reason
// comments, and design §4.4.
func TestARotationIsAnnouncedOnlyOnceTheStatusWriteLands(t *testing.T) {
	f := newFixture(t)
	r, rec := networkReconcilerWithEvents(f)

	// A rotation to announce: a hash on the status that the secret no longer
	// matches.
	putForwardingSecret(t, f, "first-value")
	f.reconcileNetwork(t, r, f.network.Name)
	drainEvents(rec)
	putForwardingSecret(t, f, "second-value")

	// The write cannot land. The reconcile fails, and nothing may be announced:
	// the state the event describes was not recorded, so the retry will find
	// the same transition and is entitled to announce it then.
	broken := *r
	broken.Client = failingStatusWriter{Client: f.c, err: errors.New("no status writes today")}
	if _, err := broken.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: f.network.Name, Namespace: f.ns},
	}); err == nil {
		t.Fatal("the reconcile succeeded with a client that refuses status writes")
	}
	if ev := drainEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v after a failed status write, want none. Announcing a rotation "+
			"the status does not record is how the same rotation gets announced again on "+
			"every retry", ev)
	}

	// The write lands: announced, once.
	f.reconcileNetwork(t, r, f.network.Name)
	ev := drainEvents(rec)
	if !containsEvent(ev, spawneryv1alpha1.EventForwardingSecretRotated) {
		t.Fatalf("events = %v, want the rotation announced once the write landed", ev)
	}

	// And not again on the next pass, which is the property this is all for.
	f.reconcileNetwork(t, r, f.network.Name)
	if ev := drainEvents(rec); containsEvent(ev, spawneryv1alpha1.EventForwardingSecretRotated) {
		t.Errorf("events = %v on a pass with nothing changed, want the rotation announced "+
			"exactly once", ev)
	}
}

// failingPodList is a client whose pod List always fails and whose every other
// List is served normally. The Network controller lists three kinds — Networks,
// to find the namespace owner; the two group kinds, to count them; and pods, to
// compare forwarding-secret stamps — and only the last is the concern under
// test here.
type failingPodList struct {
	client.Client
	err error
}

func (f failingPodList) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		return f.err
	}
	return f.Client.List(ctx, list, opts...)
}

// TestAPodListFailureStillRecordsAcceptance closes the entry in
// docs/known-issues.md: `a pod List failure blocks the Accepted=True status
// write`.
//
// The List that gathers forwarding-secret stamps sat between the Accepted
// condition being set and the status update that would have persisted it, so
// its failure discarded everything the pass had decided. Design §4.3 keeps
// Accepted deliberately clear of secret problems because both group
// controllers derive networkUsable from it — and this put a secret-detection
// concern on the path that publishes it.
//
// The entry recorded a rejected alternative: carrying on with an empty stamp
// set, which makes rotationCondition report ForwardingSecretInSync with no pod
// examined. The second assertion below is what distinguishes the fix from that
// alternative — the rotation condition is left alone, not computed from
// nothing.
func TestAPodListFailureStillRecordsAcceptance(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)

	// Its own Network in its own namespace, so that "Accepted has never been
	// written" is the real starting state rather than one arranged by hand.
	ns := testenv.Namespace(t, f.ctx, f.c)
	if err := f.c.Create(f.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "velocity-forwarding-secret", Namespace: ns},
		Data:       map[string][]byte{podspec.ForwardingSecretKey: []byte("first")},
	}); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	network := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	if err := f.c.Create(f.ctx, network); err != nil {
		t.Fatalf("create Network: %v", err)
	}

	broken := *r
	broken.Client = failingPodList{Client: f.c, err: errors.New("no pod lists today")}
	if _, err := broken.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: network.Name, Namespace: ns},
	}); err == nil {
		t.Fatal("the reconcile succeeded with a client that refuses pod lists")
	}

	got := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(network), got); err != nil {
		t.Fatalf("get network: %v", err)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
		t.Errorf("Accepted = %+v after a failed pod List, want True. Both group controllers "+
			"derive networkUsable from this condition, so a secret-detection concern that "+
			"takes it down with it stops every group in the namespace, with a log line as "+
			"the only trace",
			meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted))
	}
	if c := meta.FindStatusCondition(got.Status.Conditions,
		spawneryv1alpha1.ConditionForwardingSecretRotationPending); c != nil {
		t.Errorf("RotationPending = %+v after a failed pod List, want it unset. Deriving it "+
			"from an empty stamp set trades a rare blocked status write for a confident "+
			"wrong report: ForwardingSecretInSync with no pod examined", c)
	}
}

// refusingSecretReader answers every Get with the API server's own 403.
type refusingSecretReader struct {
	client.Reader
	err error
}

func (r refusingSecretReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

// A forwarding-secret read the API server refuses has to reach the operator's
// log carrying the API server's own words, and has to say so on the Network.
//
// The condition's message is written for a person — "the operator may not read
// secret X; grant it with kubectl apply …" — so it deliberately does not quote
// the API server, and therefore carries no `is forbidden:` substring. That is
// the exact string test/e2e's theOperatorWasNeverDenied greps the operator's
// log for, and network_controller.go made no logger call at all, so a broken
// config/rbac/forwarding-secret-reader.yaml grant was invisible to the one
// check in this repository written to catch a denial the RBAC audit cannot.
func TestARefusedSecretReadIsSaidOutLoud(t *testing.T) {
	f := newFixture(t)
	r, rec := networkReconcilerWithEvents(f)
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Resource: "secrets"}, "velocity-forwarding-secret",
		errors.New(`User "system:serviceaccount:spawnery-system:spawnery-operator" cannot get resource "secrets"`))
	r.SecretReader = refusingSecretReader{Reader: f.c, err: forbidden}

	// The error the read carries out is what the log line is built from, and
	// it is the half the condition cannot carry.
	read := readForwardingSecret(f.ctx, r.SecretReader, f.network)
	if read.Err == nil {
		t.Fatal("a refused read carried no error out; the log line has nothing to say")
	}
	if !strings.Contains(read.Err.Error(), "is forbidden:") {
		t.Errorf("read.Err = %q, want the API server's own text. `is forbidden:` is what "+
			"theOperatorWasNeverDenied greps for", read.Err)
	}
	if strings.Contains(read.Message, "is forbidden:") {
		t.Errorf("the condition message quotes the API server: %q. It is written for a person "+
			"and says what to do; the error belongs in the log", read.Message)
	}

	f.reconcileNetwork(t, r, f.network.Name)
	ev := drainEvents(rec)
	if n := countEvents(ev, spawneryv1alpha1.ReasonSecretReadForbidden); n != 1 {
		t.Errorf("events = %v, want exactly one naming %s",
			ev, spawneryv1alpha1.ReasonSecretReadForbidden)
	}

	// And not again on the next pass: at resyncInterval an ungated report is
	// twelve a minute per Network, forever.
	f.reconcileNetwork(t, r, f.network.Name)
	if n := countEvents(drainEvents(rec), spawneryv1alpha1.ReasonSecretReadForbidden); n != 0 {
		t.Errorf("the refusal was announced again on an unchanged pass, %d time(s)", n)
	}

	// Accepted is untouched by any of it, which is the property design §4.3
	// keeps deliberately: both group controllers derive networkUsable from it,
	// and a namespace whose grant is missing must keep scheduling.
	got := f.getNetwork(t, f.network.Name)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
		t.Errorf("Accepted = %+v with the secret read refused, want True",
			meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted))
	}
}
