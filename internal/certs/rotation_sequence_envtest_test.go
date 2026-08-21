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

// The sequence AdvanceRotation drives, one step per call.
//
// package certs_test, not the white-box package certs that
// rotation_envtest_test.go had to be: every name these tests touch is
// exported, and they share newStore/testClock with store_envtest_test.go,
// which is where the fixture already lives. Reaching for the white-box
// package would mean a second copy of that fixture in this directory for no
// access it does not already have.
package certs_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

// start publishes the incoming CA and signs nothing with it.
func TestStartPublishesTheIncomingCAWithoutSigningWithIt(t *testing.T) {
	s, _, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute

	before, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	requestRotation(t, ctx, s, certs.RequestStart)

	b, inFlight, err := s.AdvanceRotation(ctx, before)
	if err != nil {
		t.Fatalf("AdvanceRotation (start): %v", err)
	}
	if !inFlight {
		t.Error("start did not report a rotation in flight; the provider would go back to checking hourly")
	}
	if len(b.NextCACertPEM) == 0 || len(b.NextCAKeyPEM) == 0 {
		t.Fatal("start did not mint an incoming CA")
	}
	if !bytes.Equal(b.CACertPEM, before.CACertPEM) {
		t.Fatal("start replaced the signing CA. Agents pin what they were given at " +
			"bootstrap; the incoming CA has to be published for a whole window before " +
			"anything is signed with it")
	}
	if !bytes.Equal(b.ServingCertPEM, before.ServingCertPEM) {
		t.Error("start reissued the serving certificate; nothing about it changed")
	}

	// Published beside the old one, so agents come to trust both.
	published := b.PublishedCA()
	if !bytes.Contains(published, before.CACertPEM) || !bytes.Contains(published, b.NextCACertPEM) {
		t.Error("PublishedCA does not carry both CAs during the distributing phase")
	}

	// "Signs nothing with it" is the assertion that a bundle-shaped check
	// would miss: a switch performed early leaves NextCACertPEM populated
	// too, and only the serving certificate's issuer tells the difference.
	serving := parseCert(t, b.ServingCertPEM)
	if err := serving.CheckSignatureFrom(parseCert(t, before.CACertPEM)); err != nil {
		t.Errorf("the serving certificate no longer chains to the outgoing CA: %v", err)
	}
	if err := serving.CheckSignatureFrom(parseCert(t, b.NextCACertPEM)); err == nil {
		t.Error("the serving certificate was signed by the incoming CA already; the " +
			"overlap window exists precisely to postpone that")
	}

	secret := secretOf(t, ctx, s, ns)
	if !bytes.Equal(secret.Data["ca-next.crt"], b.NextCACertPEM) {
		t.Error("the incoming CA was not written to the secret; a restart would lose the rotation")
	}
	if !bytes.Equal(secret.Data["ca.crt"], before.CACertPEM) {
		t.Error("ca.crt changed; it always means the CA signing right now")
	}
	if got := secret.Annotations[certs.AnnotationRotationPhase]; got != certs.PhaseDistributing {
		t.Errorf("phase = %q, want %q", got, certs.PhaseDistributing)
	}
	if _, ok := secret.Annotations[certs.AnnotationRotationSince]; ok {
		t.Error("start stamped the window's start; the window runs from the moment the " +
			"gate passes, not from the moment the CA is minted")
	}
}

// The gate holds the phase while any namespace with a Network lacks it, and
// the missing namespaces are named on the secret.
func TestTheSwitchWaitsForEveryNamespaceThatHoldsANetwork(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute
	b := startedAndDistributed(t, s, clock, ctx, ns)

	// A second namespace that holds a Network and has not received the new CA.
	laggard := testenv.Namespace(t, ctx, s.Client)
	createNetwork(t, ctx, s.Client, laggard, "slow")

	b, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation with a namespace behind: %v", err)
	}
	if !inFlight {
		t.Error("a blocked gate reported the rotation finished")
	}
	if len(b.NextCACertPEM) == 0 {
		t.Fatal("the switch happened while a namespace with a Network had not received " +
			"the new CA; every agent in it would be locked out")
	}
	secret := secretOf(t, ctx, s, ns)
	if got := secret.Annotations[certs.AnnotationRotationBlockedOn]; !strings.Contains(got, laggard) {
		t.Errorf("blocked-on = %q, want it to name %q: a gate that holds without saying "+
			"what it is waiting for is indistinguishable from one that is stuck", got, laggard)
	}
	if _, ok := secret.Annotations[certs.AnnotationRotationSince]; ok {
		t.Error("the window was stamped while the gate was still blocked")
	}

	// Time alone does not open the gate.
	clock.Advance(time.Hour)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation an hour later: %v", err)
	}
	if len(b.NextCACertPEM) == 0 {
		t.Fatal("waiting long enough was treated as the gate passing")
	}

	// The laggard catches up. Now the gate passes and stamps the window.
	writeCA(t, ctx, s.Client, laggard, b.PublishedCA())
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation after the laggard caught up: %v", err)
	}
	if len(b.NextCACertPEM) == 0 {
		t.Fatal("the switch happened on the same call that passed the gate")
	}
	secret = secretOf(t, ctx, s, ns)
	if _, ok := secret.Annotations[certs.AnnotationRotationSince]; !ok {
		t.Fatal("the gate passed without stamping the window's start")
	}
	if got, ok := secret.Annotations[certs.AnnotationRotationBlockedOn]; ok {
		t.Errorf("blocked-on = %q after the gate passed, want it removed", got)
	}

	clock.Advance(12*time.Minute + time.Second)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation after the window: %v", err)
	}
	if len(b.NextCACertPEM) != 0 {
		t.Error("the switch did not happen once every namespace had the CA and the window had run")
	}
}

// The window is projectionMargin + the operator's own session deadline,
// measured from the moment the gate passed.
func TestTheSwitchWaitsOutTheWindowAfterTheGatePasses(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute
	b := startedAndDistributed(t, s, clock, ctx, ns)

	// The gate passes on this call, which stamps `since`. Nothing switches yet:
	// the window has not begun to run until it is stamped.
	b, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation at the gate: %v", err)
	}
	if !inFlight {
		t.Fatal("the rotation reported itself finished at the moment the gate passed")
	}
	if len(b.NextCACertPEM) == 0 {
		t.Fatal("the switch happened on the same call that passed the gate; the window never ran")
	}

	// One second short of projectionMargin + AgentSessionDeadline.
	clock.Advance(12*time.Minute - time.Second)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation just before the window: %v", err)
	}
	if len(b.NextCACertPEM) == 0 {
		t.Error("the switch happened one second early. The window is the kubelet's " +
			"projection margin plus this operator's own --agent-session-deadline, and " +
			"an agent that has not re-read the file yet is locked out by the switch")
	}

	clock.Advance(2 * time.Second)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation after the window: %v", err)
	}
	if len(b.NextCACertPEM) != 0 {
		t.Fatal("the switch did not happen after the window elapsed")
	}
	if got := phaseOf(t, s, ctx, ns); got != certs.PhaseSwitched {
		t.Errorf("phase = %q after the window, want %q", got, certs.PhaseSwitched)
	}
}

// After the switch the serving certificate chains to the new CA, the old one
// is still published, and nothing advances without a second annotation.
func TestTheSwitchHoldsUntilDropOldIsAsked(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute
	b := startedAndDistributed(t, s, clock, ctx, ns)
	outgoing := b.CACertPEM
	incoming := b.NextCACertPEM

	b = switchNow(t, ctx, s, clock, b)

	if !bytes.Equal(b.CACertPEM, incoming) {
		t.Fatal("the incoming CA did not become the signing CA")
	}
	if !bytes.Equal(b.PreviousCACertPEM, outgoing) {
		t.Error("the outgoing CA was not kept; a rollback would have nothing to sign with")
	}
	serving := parseCert(t, b.ServingCertPEM)
	if err := serving.CheckSignatureFrom(parseCert(t, incoming)); err != nil {
		t.Errorf("the serving certificate does not chain to the new CA: %v", err)
	}
	published := b.PublishedCA()
	if !bytes.Contains(published, outgoing) {
		t.Error("the outgoing CA stopped being published at the switch. Agents that have " +
			"not restarted still pin it, and dropping it is a separate, human-asked step")
	}

	// Hold. Far more than any window, and nothing moves.
	clock.Advance(30 * 24 * time.Hour)
	held, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation a month after the switch: %v", err)
	}
	if !inFlight {
		t.Error("the rotation reported itself finished while the outgoing CA was still published")
	}
	if len(held.PreviousCACertPEM) == 0 {
		t.Fatal("the outgoing CA was dropped without anyone asking. The hold is the point: " +
			"only a human knows whether every agent has been restarted")
	}
	if got := phaseOf(t, s, ctx, ns); got != certs.PhaseSwitched {
		t.Errorf("phase = %q a month after the switch, want %q", got, certs.PhaseSwitched)
	}

	// And a start now is refused rather than overwriting the slot the outgoing
	// CA is sitting in.
	requestRotation(t, ctx, s, certs.RequestStart)
	if _, _, err := s.AdvanceRotation(ctx, held); err == nil {
		t.Error("a second rotation was started while the first was holding; there is no " +
			"slot for a third CA")
	}
}

// drop-old narrows the bundle and removes the outgoing key.
func TestDropOldNarrowsTheBundleAndRemovesTheOutgoingKey(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute
	b := startedAndDistributed(t, s, clock, ctx, ns)
	outgoing := b.CACertPEM
	b = switchNow(t, ctx, s, clock, b)

	requestRotation(t, ctx, s, certs.RequestDropOld)
	b, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation (drop-old): %v", err)
	}
	if inFlight {
		t.Error("drop-old left the rotation in flight; the provider would keep polling every 30s forever")
	}
	if len(b.PreviousCACertPEM) != 0 || len(b.PreviousCAKeyPEM) != 0 {
		t.Fatal("drop-old kept the outgoing CA in the bundle")
	}
	if bytes.Contains(b.PublishedCA(), outgoing) {
		t.Error("drop-old still publishes the outgoing CA")
	}

	secret := secretOf(t, ctx, s, ns)
	for _, key := range []string{"ca-previous.crt", "ca-previous.key"} {
		if _, ok := secret.Data[key]; ok {
			t.Errorf("secret still holds %q. The key especially: it is a CA private key "+
				"that can mint a certificate this operator would serve, and the whole "+
				"point of drop-old is that it stops existing", key)
		}
	}
	for _, key := range []string{certs.AnnotationRotationPhase, certs.AnnotationRotationSince, certs.AnnotationRotationBlockedOn, certs.AnnotationRotateRequest} {
		if got, ok := secret.Annotations[key]; ok {
			t.Errorf("annotation %q = %q survived drop-old, want it removed", key, got)
		}
	}
}

// rollback abandons the rotation from either phase and leaves nothing that
// would advance on its own.
func TestRollbackAbandonsTheRotationFromEitherPhase(t *testing.T) {
	t.Run("while distributing", func(t *testing.T) {
		s, clock, ctx, ns := newStore(t)
		s.AgentSessionDeadline = 10 * time.Minute
		b := startedAndDistributed(t, s, clock, ctx, ns)
		signing := b.CACertPEM

		requestRotation(t, ctx, s, certs.RequestRollback)
		b, inFlight, err := s.AdvanceRotation(ctx, b)
		if err != nil {
			t.Fatalf("AdvanceRotation (rollback while distributing): %v", err)
		}
		if inFlight {
			t.Error("an abandoned rotation still reported itself in flight")
		}
		if len(b.NextCACertPEM) != 0 {
			t.Fatal("rollback kept the incoming CA")
		}
		if !bytes.Equal(b.CACertPEM, signing) {
			t.Error("rollback changed the signing CA; nothing had been signed with the incoming one")
		}
		assertNothingAdvances(t, ctx, s, clock, b, ns)
	})

	t.Run("after the switch", func(t *testing.T) {
		s, clock, ctx, ns := newStore(t)
		s.AgentSessionDeadline = 10 * time.Minute
		b := startedAndDistributed(t, s, clock, ctx, ns)
		original := b.CACertPEM
		b = switchNow(t, ctx, s, clock, b)

		requestRotation(t, ctx, s, certs.RequestRollback)
		b, inFlight, err := s.AdvanceRotation(ctx, b)
		if err != nil {
			t.Fatalf("AdvanceRotation (rollback after the switch): %v", err)
		}
		if inFlight {
			t.Error("an abandoned rotation still reported itself in flight")
		}
		if !bytes.Equal(b.CACertPEM, original) {
			t.Fatal("rollback did not put the original CA back in charge of signing")
		}
		serving := parseCert(t, b.ServingCertPEM)
		if err := serving.CheckSignatureFrom(parseCert(t, original)); err != nil {
			t.Errorf("the serving certificate does not chain to the restored CA: %v", err)
		}
		if len(b.NextCACertPEM) != 0 || len(b.PreviousCACertPEM) != 0 {
			t.Error("rollback left a rotation slot occupied")
		}
		assertNothingAdvances(t, ctx, s, clock, b, ns)
	})
}

// A Network created during the window does not postpone the switch.
//
// Its namespace's ConfigMap receives the current bundle -- already two PEMs --
// on its first reconcile, and its pods have never held anything else.
// Re-checking the gate is the obvious implementation and would let a cluster
// where networks are created regularly push the switch out forever.
func TestANetworkCreatedDuringTheWindowDoesNotPostponeTheSwitch(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute
	b := startedAndDistributed(t, s, clock, ctx, ns)

	// Pass the gate and stamp `since`.
	b, _, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation at the gate: %v", err)
	}

	// A Network appears halfway through the window, in a namespace with no CA
	// ConfigMap at all -- the state a re-run of the gate would call "missing".
	clock.Advance(6 * time.Minute)
	latecomer := testenv.Namespace(t, ctx, s.Client)
	createNetwork(t, ctx, s.Client, latecomer, "arrived-late")

	clock.Advance(6*time.Minute + time.Second)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation after the window: %v", err)
	}
	if len(b.NextCACertPEM) != 0 {
		t.Fatal("a Network created during the window postponed the switch. Its " +
			"namespace's ConfigMap receives the current bundle -- already two PEMs -- " +
			"on its first reconcile, and its pods have never held anything else, so " +
			"there is nothing for the window to protect. Re-checking the gate lets a " +
			"cluster where networks are created regularly push the switch out forever")
	}
}

// A request the operator does not recognise is left alone and reported; one it
// does recognise is removed once acted on.
//
// Clearing an annotation you did not understand hides the typo that produced
// it, and leaving one you did act on would fire it again on the next tick.
func TestAnUnknownRequestIsLeftInPlaceAndAKnownOneIsConsumed(t *testing.T) {
	s, _, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	createNetwork(t, ctx, s.Client, ns, "net")

	requestRotation(t, ctx, s, "strat")
	if _, _, err := s.AdvanceRotation(ctx, b); err == nil {
		t.Error("a misspelled request was accepted silently")
	} else if !strings.Contains(err.Error(), "strat") {
		t.Errorf("the error does not quote the request it could not read: %v", err)
	}
	secret := secretOf(t, ctx, s, ns)
	if got := secret.Annotations[certs.AnnotationRotateRequest]; got != "strat" {
		t.Errorf("rotate-ca = %q after an unreadable request, want it left in place: "+
			"clearing it hides the typo from whoever made it", got)
	}
	if got, ok := secret.Annotations[certs.AnnotationRotationPhase]; ok {
		t.Errorf("phase = %q; an unreadable request started something", got)
	}

	requestRotation(t, ctx, s, certs.RequestStart)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation (start): %v", err)
	}
	incoming := b.NextCACertPEM
	if got, ok := secretOf(t, ctx, s, ns).Annotations[certs.AnnotationRotateRequest]; ok {
		t.Errorf("rotate-ca = %q after being acted on, want it removed", got)
	}

	// The next tick. A request still sitting there fires again: at best it
	// mints a second incoming CA over the one agents are already picking up,
	// at worst it is refused and the rotation makes no progress at all.
	writeCA(t, ctx, s.Client, ns, b.PublishedCA())
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("the tick after a consumed start: %v", err)
	}
	if !bytes.Equal(b.NextCACertPEM, incoming) {
		t.Error("the incoming CA changed on the next tick; start was acted on twice")
	}
}

// A switch with no session deadline configured is refused rather than
// performed against a window that is short by ten minutes.
func TestAMissingSessionDeadlineRefusesTheSwitch(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	// Deliberately not set: this is the operator whose --agent-session-deadline
	// never reached the store.
	b := startedAndDistributed(t, s, clock, ctx, ns)

	clock.Advance(24 * time.Hour)
	after, _, err := s.AdvanceRotation(ctx, b)
	if err == nil {
		t.Fatal("the rotation advanced with no session deadline configured. Without it " +
			"the window is only the kubelet's projection margin, which is short by the " +
			"whole life of an agent stream")
	}
	if !strings.Contains(err.Error(), "agent-session-deadline") {
		t.Errorf("the error does not name the missing wiring: %v", err)
	}
	if after != nil {
		t.Errorf("a bundle came back alongside the error: %+v", after)
	}
	if got := phaseOf(t, s, ctx, ns); got != certs.PhaseDistributing {
		t.Errorf("phase = %q, want the rotation left where it was (%q)", got, certs.PhaseDistributing)
	}
	if !bytes.Equal(secretOf(t, ctx, s, ns).Data["ca.crt"], b.CACertPEM) {
		t.Error("the signing CA changed despite the refusal")
	}
}

// --- fixture plumbing ---------------------------------------------------

// startedAndDistributed leaves the rotation one call short of the gate
// passing: started, with the incoming CA already in the only namespace that
// holds a Network. The clock is not advanced here -- every caller needs the
// gate to pass on a call it makes itself, since that call is what stamps the
// window -- but it is taken so the call sites read like the others.
func startedAndDistributed(t *testing.T, s *certs.Store, clock *testClock, ctx context.Context, ns string) *certs.Bundle {
	t.Helper()
	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	createNetwork(t, ctx, s.Client, ns, "net")

	requestRotation(t, ctx, s, certs.RequestStart)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation (start): %v", err)
	}
	if len(b.NextCACertPEM) == 0 {
		t.Fatal("start did not mint an incoming CA")
	}

	// What the bootstrapper does on the next reconcile, done by hand: the
	// namespace's ConfigMap now carries both CAs.
	writeCA(t, ctx, s.Client, ns, b.PublishedCA())
	return b
}

// switchNow runs the gate, waits the window out and performs the switch.
func switchNow(t *testing.T, ctx context.Context, s *certs.Store, clock *testClock, b *certs.Bundle) *certs.Bundle {
	t.Helper()
	b, _, err := s.AdvanceRotation(ctx, b) // the gate; stamps the window
	if err != nil {
		t.Fatalf("AdvanceRotation at the gate: %v", err)
	}
	clock.Advance(projectionMarginPlus(s.AgentSessionDeadline) + time.Second)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation after the window: %v", err)
	}
	if len(b.NextCACertPEM) != 0 {
		t.Fatal("the switch did not happen after the window elapsed")
	}
	return b
}

// projectionMarginPlus is the window as an operator would compute it from
// outside the package. Spelled out rather than reaching for the unexported
// projectionMargin, so a change to that constant shows up here as a failure
// to explain rather than as a silently adjusted test.
func projectionMarginPlus(deadline time.Duration) time.Duration {
	return 2*time.Minute + deadline
}

// assertNothingAdvances is what "abandoned" means: no later tick picks the
// rotation back up.
func assertNothingAdvances(t *testing.T, ctx context.Context, s *certs.Store, clock *testClock, b *certs.Bundle, ns string) {
	t.Helper()
	if got, ok := secretOf(t, ctx, s, ns).Annotations[certs.AnnotationRotationPhase]; ok {
		t.Errorf("phase = %q after a rollback, want it removed", got)
	}
	clock.Advance(24 * time.Hour)
	after, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation a day after the rollback: %v", err)
	}
	if inFlight {
		t.Error("a rotation reappeared a day after it was abandoned")
	}
	if !bytes.Equal(after.CACertPEM, b.CACertPEM) {
		t.Error("the signing CA changed a day after the rollback")
	}
	if len(after.NextCACertPEM) != 0 || len(after.PreviousCACertPEM) != 0 {
		t.Error("a rotation slot filled itself back in after the rollback")
	}
}

func requestRotation(t *testing.T, ctx context.Context, s *certs.Store, request string) {
	t.Helper()
	secret := secretOf(t, ctx, s, s.Namespace)
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[certs.AnnotationRotateRequest] = request
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("annotate the secret with %s=%s: %v", certs.AnnotationRotateRequest, request, err)
	}
}

func secretOf(t *testing.T, ctx context.Context, s *certs.Store, ns string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: certs.SecretName, Namespace: ns}, secret); err != nil {
		t.Fatalf("get the secret: %v", err)
	}
	return secret
}

func phaseOf(t *testing.T, s *certs.Store, ctx context.Context, ns string) string {
	t.Helper()
	return secretOf(t, ctx, s, ns).Annotations[certs.AnnotationRotationPhase]
}

// createNetwork makes the namespace one the gate looks at, and deletes the
// Network again afterwards.
//
// The deletion is not tidiness. testenv runs one apiserver per test binary
// with no kube-controller-manager, so a Namespace never goes away and neither
// do the objects in it, while namespacesMissingCA lists Networks
// cluster-wide: a Network left behind would block every later test's gate on
// a namespace that will never receive that test's freshly minted CA. Deleting
// the object works against a bare apiserver and Network carries no finalizer,
// so it completes at once. Registered after testenv.Client's own
// t.Cleanup(cancel), which runs cleanups LIFO, so this one fires while ctx is
// still live.
func createNetwork(t *testing.T, ctx context.Context, c client.Client, ns, name string) {
	t.Helper()
	n := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	if err := c.Create(ctx, n); err != nil {
		t.Fatalf("create Network in %s: %v", ns, err)
	}
	t.Cleanup(func() {
		if err := c.Delete(ctx, n); err != nil {
			t.Errorf("cleanup: delete Network in %s: %v", ns, err)
		}
	})
}

// writeCA plays the bootstrapper: it puts the published bundle into the
// namespace's spawnery-ca ConfigMap, creating or replacing it.
func writeCA(t *testing.T, ctx context.Context, c client.Client, ns string, caPEM []byte) {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podspec.CAConfigMapName,
			Namespace: ns,
			Labels:    map[string]string{podspec.LabelManagedBy: podspec.ManagedByValue},
		},
		Data: map[string]string{podspec.CAConfigMapKey: string(caPEM)},
	}
	err := c.Create(ctx, cm)
	if apierrors.IsAlreadyExists(err) {
		existing := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: ns}, existing); err != nil {
			t.Fatalf("get the CA ConfigMap in %s: %v", ns, err)
		}
		existing.Data = cm.Data
		err = c.Update(ctx, existing)
	}
	if err != nil {
		t.Fatalf("write the CA ConfigMap in %s: %v", ns, err)
	}
}

func parseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("not PEM: %q", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}
