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

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
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

	"github.com/spawnery/spawnery/internal/agent"
)

func networkReconciler(f *fixture) *NetworkReconciler {
	r, _ := networkReconcilerWithEvents(f)
	return r
}

// Returns the recorder too: these events fire on entering a state, so "exactly once"
// is read from the channel, not the object.
func networkReconcilerWithEvents(f *fixture) (*NetworkReconciler, *nonBlockingRecorder) {
	rec := newRecorder()
	return &NetworkReconciler{
		Client:   f.rc,
		Scheme:   f.reconc.Scheme,
		Recorder: rec,
		// Matches config/deploy/; only the NetworkPolicy tests care what it is.
		OperatorNamespace: "spawnery-system",
		SecretReader:      f.c,
		// Tests that care about the bundle replace this.
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

// Not network: the fixture already has a field of that name.
func (f *fixture) getNetwork(t *testing.T, name string) *spawneryv1alpha1.Network {
	t.Helper()
	net := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, net); err != nil {
		t.Fatalf("get network %s: %v", name, err)
	}
	return net
}

// rejectNetwork stands in for the one-per-namespace verdict. A competitor created after
// real work lands in a later second and loses on age whatever its name.
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

// The API server stamps creationTimestamp itself at one-second resolution, so
// back-to-back Networks tie and only an explicit timestamp exercises the age rule.
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

// Names are chosen so going by name alone, or by the newest, picks a different winner.
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
			// The answer must not depend on the order the API server returns them in.
			name: "the oldest network wins whatever order it is listed in",
			networks: []spawneryv1alpha1.Network{
				networkAt("alpha", 600, false),
				networkAt("zulu", 0, false),
			},
			want: "zulu",
		},
		{
			// With equal age the name decides, or the winner would flip between reconciles.
			name: "equal timestamps fall through to the name",
			networks: []spawneryv1alpha1.Network{
				networkAt("zulu", 300, false),
				networkAt("alpha", 300, false),
			},
			want: "alpha",
		},
		{
			// The owner is being deleted: the next oldest wins, not the smallest name.
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

// The API server guarantees no list order, so an order-dependent winner would hand
// the namespace back and forth.
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

func TestNetworkCountsChangeoversInFlightAndWaiting(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	begun := f.group
	begun.Status.Changeover = spawneryv1alpha1.ChangeoverBegun
	if err := f.c.Status().Update(f.ctx, begun); err != nil {
		t.Fatalf("set lobby's changeover to Begun: %v", err)
	}
	waiting := f.createEphemeralGroupLike(t, "arena")
	waiting.Status.Changeover = spawneryv1alpha1.ChangeoverWaiting
	if err := f.c.Status().Update(f.ctx, waiting); err != nil {
		t.Fatalf("set arena's changeover to Waiting: %v", err)
	}
	persistent := f.createPersistentGroup(t, "world", 1)
	persistent.Status.Changeover = spawneryv1alpha1.ChangeoverBegun
	if err := f.c.Status().Update(f.ctx, persistent); err != nil {
		t.Fatalf("set world's changeover to Begun: %v", err)
	}

	f.reconcileNetwork(t, r, "production")

	if got := testutil.ToFloat64(ChangeoversInFlight.WithLabelValues(f.ns, "production")); got != 1 {
		t.Errorf("ChangeoversInFlight = %v, want 1: a persistent group holds no place", got)
	}
	if got := testutil.ToFloat64(ChangeoversWaiting.WithLabelValues(f.ns, "production")); got != 1 {
		t.Errorf("ChangeoversWaiting = %v, want 1", got)
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
		if eventHasReason(e, reason) {
			n++
		}
	}
	return n
}

// Otherwise every operator start would announce a rotation on every network.
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

// Created by hand: the comparison is under test, not how pods come to exist.
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

// Groups derive networkUsable from Accepted, so a missing secret must not stop sizing.
func TestAMissingSecretLeavesAcceptedAlone(t *testing.T) {
	f := newFixture(t)
	r, events := networkReconcilerWithEvents(f)

	// newFixture's own reconcile already entered SecretNotFound; clear it so this one is
	// a genuine entry.
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

	// Still in SecretNotFound rather than entering it, so no second event.
	f.reconcileNetwork(t, r, "production")
	if n := countEvents(drainEvents(events), spawneryv1alpha1.EventForwardingSecretNotFound); n != 0 {
		t.Errorf("the next reconcile emitted %d more events, want 0", n)
	}
}

func policyKey(f *fixture, network string) types.NamespacedName {
	return types.NamespacedName{
		Namespace: f.ns,
		Name:      podspec.NetworkPolicyName(network),
	}
}

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

// If the loser wrote one too, two Networks would overwrite each other's policy.
func TestARejectedNetworkWritesNoPolicy(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	// A younger Network loses: age decides before the name.
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
	// A different UID is the only thing that tells recreated from never removed.
	if policy.UID == before {
		t.Errorf("the policy carries its original UID %s, so it was never actually deleted "+
			"and this test proves nothing about recreation", before)
	}
}

// Nothing selects on these labels, so nothing else would catch a wrong value.
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

// The operator namespace is a flag, so the policy must not hard-code spawnery-system.
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

// Ensure runs from the Network, not only on pod creation, or a quiet namespace could
// never close a CA rotation's overlap window.
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

// No OwnerReference on purpose: deleting a Network must not take a running fleet's
// CA and ServiceAccount with it. All three objects, since a pod keeping its CA but
// losing its ServiceAccount cannot mint a token.
func TestTheCAConfigMapIsOwnedByNothing(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return []byte("PEM-A") }}

	// Cleared so the objects read back are this reconcile's, not newFixture's.
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

// Swallowing it would leave the stale ConfigMap Ensure exists to prevent.
func TestAReconcileWithoutACABundleFails(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return nil }}

	// Cleared so "no ConfigMap after" tests this reconcile, not newFixture's.
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
	// The whole wrapper: Ensure's own message already starts "bootstrap namespace".
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

// Ensure runs after the status update: a ConfigMap write refused by a webhook or quota
// can stand indefinitely, and both group controllers gate on Accepted.
func TestABootstrapFailureStillWritesTheStatus(t *testing.T) {
	f := newFixture(t)
	r, rec := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return nil }}

	// newFixture's ServerGroup postdates the stored status, so a non-zero count below can
	// only come from this reconcile.
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

	// Once per transition: this branch runs for as long as the duplicate stands.
	f.reconcileNetwork(t, r, "staging")
	if ev := drainEvents(rec); len(ev) != 0 {
		t.Errorf("events = %v on a second pass with nothing changed, want none", ev)
	}
}

// For() already enqueues the subject, so the mapper must return only the others.
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

// A Network refused on its first pass must still count what points at it; the count
// shows what is stranded behind the refusal.
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
	// Two groups behind the loser, and one behind the winner that must not be credited to it.
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

// An event must not be emitted when the status write recording it did not land.
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

// Whether a pass is an entry is decided by the condition in etcd, so an event emitted
// before a failed write would be announced again on the retry.
func TestARotationIsAnnouncedOnlyOnceTheStatusWriteLands(t *testing.T) {
	f := newFixture(t)
	r, rec := networkReconcilerWithEvents(f)

	// A rotation to announce: a status hash the secret no longer matches.
	putForwardingSecret(t, f, "first-value")
	f.reconcileNetwork(t, r, f.network.Name)
	drainEvents(rec)
	putForwardingSecret(t, f, "second-value")

	// The write cannot land: nothing is announced, and the retry may announce it then.
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

// Only the pod List fails; the Network controller's other Lists are served.
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

// Groups derive networkUsable from Accepted, so a pod List failure must not block it.
// The rotation condition is left alone rather than reported InSync with no pod examined.
func TestAPodListFailureStillRecordsAcceptance(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)

	// Its own Network in its own namespace, so Accepted has really never been written.
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

type refusingSecretReader struct {
	client.Reader
	err error
}

func (r refusingSecretReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

// The condition's message is written for a person and does not quote the API server;
// test/e2e greps the operator's log for "is forbidden:", so the log must carry it.
func TestARefusedSecretReadIsSaidOutLoud(t *testing.T) {
	f := newFixture(t)
	r, rec := networkReconcilerWithEvents(f)
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Resource: "secrets"}, "velocity-forwarding-secret",
		errors.New(`User "system:serviceaccount:spawnery-system:spawnery-operator" cannot get resource "secrets"`))
	r.SecretReader = refusingSecretReader{Reader: f.c, err: forbidden}

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

	// Not again on the next pass.
	f.reconcileNetwork(t, r, f.network.Name)
	if n := countEvents(drainEvents(rec), spawneryv1alpha1.ReasonSecretReadForbidden); n != 0 {
		t.Errorf("the refusal was announced again on an unchanged pass, %d time(s)", n)
	}

	// Accepted untouched: a namespace whose grant is missing must keep scheduling.
	got := f.getNetwork(t, f.network.Name)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
		t.Errorf("Accepted = %+v with the secret read refused, want True",
			meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted))
	}
}

// Refuses only NetworkPolicy writes; taking the grant from the real ServiceAccount
// would also fail the reads this test needs.
type refusingPolicyWrites struct {
	client.Client
}

func (c refusingPolicyWrites) forbidden(obj client.Object) error {
	if _, ok := obj.(*networkingv1.NetworkPolicy); !ok {
		return nil
	}
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"},
		obj.GetName(),
		fmt.Errorf("user cannot create resource \"networkpolicies\" in API group \"networking.k8s.io\""))
}

func (c refusingPolicyWrites) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := c.forbidden(obj); err != nil {
		return err
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c refusingPolicyWrites) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if err := c.forbidden(obj); err != nil {
		return err
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c refusingPolicyWrites) Patch(ctx context.Context, obj client.Object,
	patch client.Patch, opts ...client.PatchOption) error {
	if err := c.forbidden(obj); err != nil {
		return err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// Without a status write a fresh Network shows nothing, and every group misleadingly
// reports it as not accepted yet.
func TestAPolicyThatCannotBeWrittenIsNamedOnTheNetwork(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)
	r.Client = refusingPolicyWrites{Client: r.Client}

	_, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "production", Namespace: f.ns},
	})
	if err == nil {
		t.Fatal("the reconcile succeeded with the policy write refused")
	}

	network := &spawneryv1alpha1.Network{}
	if getErr := f.c.Get(f.ctx,
		types.NamespacedName{Name: "production", Namespace: f.ns}, network); getErr != nil {
		t.Fatalf("get network: %v", getErr)
	}
	accepted := meta.FindStatusCondition(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil {
		t.Fatal("Accepted was never persisted, so every group in the namespace reports " +
			"\"has not been accepted yet\" and nothing says why")
	}
	// Fail-closed: True beside a missing policy would release every group to create the
	// pods it was meant to fence.
	if accepted.Status != metav1.ConditionFalse {
		t.Errorf("Accepted = %s, want False: the namespace must stay closed", accepted.Status)
	}
	if accepted.Reason != spawneryv1alpha1.ReasonNetworkPolicyNotWritten {
		t.Errorf("reason = %q, want %q", accepted.Reason, spawneryv1alpha1.ReasonNetworkPolicyNotWritten)
	}
	for _, want := range []string{"NetworkPolicy", "networkpolicies"} {
		if !strings.Contains(accepted.Message, want) {
			t.Errorf("message %q does not mention %q, so it does not name the cause",
				accepted.Message, want)
		}
	}
}

func TestAWritablePolicyStillAcceptsTheNetwork(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)
	f.reconcileNetwork(t, r, "production")

	network := &spawneryv1alpha1.Network{}
	if err := f.c.Get(f.ctx,
		types.NamespacedName{Name: "production", Namespace: f.ns}, network); err != nil {
		t.Fatalf("get network: %v", err)
	}
	accepted := meta.FindStatusCondition(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if accepted == nil || accepted.Status != metav1.ConditionTrue {
		t.Fatalf("Accepted = %+v, want True", accepted)
	}
}

// A velocity.toml overlay may lower Velocity's read timeout, so the window comes from
// what the proxies report, not the shipped value.
func TestTheRescueWindowConditionReportsWhatTheProxiesSaid(t *testing.T) {
	for _, tc := range []struct {
		what       string
		report     func(*agent.Registry, string)
		wantStatus metav1.ConditionStatus
		wantReason string
		wantPhrase string
	}{
		{
			what:       "no proxy has said, which is not the same as sufficient",
			report:     func(*agent.Registry, string) {},
			wantStatus: metav1.ConditionUnknown,
			wantReason: spawneryv1alpha1.ReasonNoProxyReported,
		},
		{
			what: "the shipped timeout leaves the whole window",
			report: func(r *agent.Registry, ns string) {
				r.Connect("proxy-ok", agent.RoleProxy)
				r.ReportReadTimeout("proxy-ok", ns, 30*time.Second)
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonRescueWindowSufficient,
			wantPhrase: "20s",
		},
		{
			what: "a lowered timeout leaves less than a resync",
			report: func(r *agent.Registry, ns string) {
				r.Connect("proxy-impatient", agent.RoleProxy)
				r.ReportReadTimeout("proxy-impatient", ns, 12*time.Second)
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: spawneryv1alpha1.ReasonRescueWindowTooShort,
			wantPhrase: "12s",
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			f := newFixture(t)
			registry := agent.New(f.clock.Now, 5*time.Second, f.clock.now)
			tc.report(registry, f.ns)
			r, _ := networkReconcilerWithEvents(f)
			r.Agents = registry
			r.ReportInterval = 5 * time.Second

			f.reconcileNetwork(t, r, "production")

			c := meta.FindStatusCondition(f.getNetwork(t, "production").Status.Conditions,
				spawneryv1alpha1.ConditionRescueWindowShort)
			if c == nil {
				t.Fatal("no RescueWindowShort condition at all")
			}
			if c.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s (message: %q)", c.Status, tc.wantStatus, c.Message)
			}
			if c.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", c.Reason, tc.wantReason)
			}
			if tc.wantPhrase != "" && !strings.Contains(c.Message, tc.wantPhrase) {
				t.Errorf("message = %q, which does not name %q — the number somebody has to "+
					"change is the point of the message", c.Message, tc.wantPhrase)
			}
		})
	}
}
