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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

// A request the operator does not recognise is left alone and reported -- but
// not obeyed and not halted on; one it does recognise is removed once acted
// on.
//
// Clearing an annotation you did not understand hides the typo that produced
// it, and leaving one you did act on would freeze the sequence: the phase is
// only ever driven on a tick with no request pending.
func TestAnUnknownRequestIsLeftInPlaceAndAKnownOneIsConsumed(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute

	// "Reported" is half the claim, so it is captured rather than assumed.
	var logged []string
	ctx = log.IntoContext(ctx, funcr.New(func(prefix, args string) {
		logged = append(logged, args)
	}, funcr.Options{}))

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	createNetwork(t, ctx, s.Client, ns, "net")

	requestRotation(t, ctx, s, "strat")
	if _, _, err := s.AdvanceRotation(ctx, b); err != nil {
		t.Fatalf("an unreadable request was treated as a reason to stop: %v", err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "strat") {
		t.Errorf("the value it could not read was not reported; logged: %v", logged)
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

	// The same typo, now while a rotation is mid-window. Reporting it must not
	// halt the sequence. Continuing costs at most one unwanted automatic step
	// -- the switch -- and that step is fully reversible: rollback re-signs
	// with ca-previous.*, which every agent has trusted throughout. Halting
	// costs an indefinite freeze with nothing on the object saying so, since
	// the phase still reads `distributing` and `since` is still stamped.
	requestRotation(t, ctx, s, "dropp-old")
	clock.Advance(12*time.Minute + time.Second)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation with an unreadable request mid-window: %v", err)
	}
	if len(b.NextCACertPEM) != 0 {
		t.Fatal("an unreadable request froze the rotation mid-window. The value is " +
			"reported and left alone; it is not a reason to stop a procedure that is " +
			"already past its gate and whose next step can be undone")
	}
	if got := secretOf(t, ctx, s, ns).Annotations[certs.AnnotationRotateRequest]; got != "dropp-old" {
		t.Errorf("rotate-ca = %q after the switch, want the unreadable value still in place", got)
	}
}

// A human's instruction survives a conflict on the operator's own write.
//
// applyStep wraps its update in retry.RetryOnConflict with the Get inside the
// retried function, which is the whole concurrency story for a secret two
// parties write. The part worth protecting is the consume closure: it deletes
// rotate-ca only when the value is still the one this call decided on, so a
// request nobody has acted on yet cannot be swallowed by somebody else's
// retry.
//
// The scenario: the operator decides to act on start; between its read and its
// write a human replaces the annotation with rollback; the update conflicts;
// the retry re-reads, finds rollback, and must leave it alone -- while start's
// own work still lands, because it succeeded.
func TestAConflictDoesNotSwallowAnInstructionNobodyActedOn(t *testing.T) {
	// interceptor.NewClient needs a client.WithWatch, and newStore's client
	// (testenv.Client's plain client.Client) is not one: client.New's
	// concrete type carries no Watch method, only client.NewWithWatch's does.
	// Built against the same shared control plane testenv.Config/Scheme
	// memoize, so this reaches the identical apiserver every other test in
	// this package talks to.
	plain, err := client.NewWithWatch(testenv.Config(t), client.Options{Scheme: testenv.Scheme(t)})
	if err != nil {
		t.Fatalf("new watch client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ns := testenv.Namespace(t, ctx, plain)
	clock := &testClock{now: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
	s := &certs.Store{
		Client:    plain,
		Namespace: ns,
		Name:      certs.SecretName,
		DNSNames:  certs.ServingDNSNames("spawnery-operator", ns),
		Clock:     clock.Now,
	}

	before, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Written through the plain client, before the interceptor goes on: this
	// is the request applyStep's retry has to protect, not the write under test.
	requestRotation(t, ctx, s, certs.RequestStart)

	// From here on, s.Client intercepts Update: the first call is applyStep's
	// own write losing a race to a human's kubectl edit, and it conflicts;
	// the second is the retry's own write and goes through untouched.
	conflicted := false
	s.Client = interceptor.NewClient(plain, interceptor.Funcs{
		Update: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if conflicted {
				return inner.Update(ctx, obj, opts...)
			}
			conflicted = true
			// The competing write, through the underlying client so it is not
			// itself intercepted: a human's kubectl edit landing between
			// applyStep's Get and its Update.
			secret := &corev1.Secret{}
			key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
			if err := inner.Get(ctx, key, secret); err != nil {
				return err
			}
			if secret.Annotations == nil {
				secret.Annotations = map[string]string{}
			}
			secret.Annotations[certs.AnnotationRotateRequest] = certs.RequestRollback
			if err := inner.Update(ctx, secret); err != nil {
				return err
			}
			// A real Conflict, not a stand-in: retry.RetryOnConflict gates on
			// apierrors.IsConflict, which only a StatusReasonConflict --
			// exactly what NewConflict produces -- satisfies. Anything else
			// would exercise applyStep's plain error return, not the retry.
			return apierrors.NewConflict(corev1.Resource("secrets"), obj.GetName(),
				errors.New("a human edited rotate-ca first"))
		},
	})

	b, inFlight, err := s.AdvanceRotation(ctx, before)
	if err != nil {
		t.Fatalf("AdvanceRotation (start) over a conflicting write: %v", err)
	}
	if !inFlight {
		t.Error("start did not report a rotation in flight after surviving the conflict")
	}
	if len(b.NextCACertPEM) == 0 {
		t.Fatal("start's own work did not land: no incoming CA in the bundle the retry returned")
	}

	secret := secretOf(t, ctx, s, ns)
	// Both halves, not just one: an implementation that gave up on the
	// conflict entirely -- consuming nothing and minting no incoming CA --
	// would also leave rollback sitting on the secret.
	if got := secret.Annotations[certs.AnnotationRotateRequest]; got != certs.RequestRollback {
		t.Errorf("rotate-ca = %q, want %q: the retry must not swallow an instruction "+
			"nobody had acted on when it fired", got, certs.RequestRollback)
	}
	if got := secret.Annotations[certs.AnnotationRotationPhase]; got != certs.PhaseDistributing {
		t.Errorf("phase = %q, want %q: start's own work has to land even though the "+
			"write that carried it conflicted once", got, certs.PhaseDistributing)
	}
	if len(secret.Data["ca-next.crt"]) == 0 {
		t.Error("ca-next.crt is empty; start's incoming CA did not reach the secret")
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

// An incoming CA no agent could parse abandons the rotation.
//
// It never distributed anything usable, so the end state is the one a
// rollback out of `distributing` already produces: no slot, no phase, the
// signing CA published alone -- and every agent trusted that one throughout.
func TestAnUnparseableIncomingCAAbandonsTheRotation(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute

	b := startedAndDistributed(t, s, clock, ctx, ns)
	signing := b.CACertPEM
	// Wired in after the fixture, so the only event on the channel is the one
	// the call under test records -- the fixture's own start is not evidence
	// about anything here.
	rec := events.NewFakeRecorder(8)
	s.Recorder = rec
	// The bundle handed to AdvanceRotation deliberately still carries the
	// good bytes: the hand-edit lands after Ensure read the secret, which is
	// the ordering AdvanceRotation's own fresh Get exists for.
	breakSlot(t, ctx, s, ns, "ca-next.crt", []byte("-- not a certificate --\n"))

	after, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation over a broken incoming CA: %v", err)
	}
	if inFlight {
		t.Error("an abandoned rotation still reported itself in flight; the provider " +
			"would keep polling every 30s for a rotation that has nothing left to distribute")
	}
	if len(after.NextCACertPEM) != 0 || len(after.NextCAKeyPEM) != 0 {
		t.Error("the bundle still carries the incoming CA nobody can parse")
	}
	if !bytes.Equal(after.CACertPEM, signing) {
		t.Error("the signing CA changed; the cleanup discards the slot that broke, not the one that works")
	}

	secret := secretOf(t, ctx, s, ns)
	for _, k := range []string{"ca-next.crt", "ca-next.key"} {
		if len(secret.Data[k]) != 0 {
			t.Errorf("%s survived the cleanup", k)
		}
	}
	if got := secret.Annotations[certs.AnnotationRotationPhase]; got != "" {
		t.Errorf("phase = %q after the incoming CA was discarded, want it cleared — "+
			"a phase that says `distributing` with no incoming CA left is a rotation "+
			"the next tick cannot advance and nobody asked to abandon", got)
	}
	got := secret.Annotations[certs.AnnotationRotationDiscarded]
	if !strings.Contains(got, "ca-next.crt") {
		t.Errorf("the discarded record = %q, want it to name the slot", got)
	}
	if !strings.Contains(got, "not PEM") {
		t.Errorf("the discarded record = %q, want it to carry the parse error: the bytes "+
			"are gone, so this is the whole of what a diagnosis has left", got)
	}
	// The exact stamp, not merely some stamp: it comes from the store's clock,
	// which is what makes "when did this happen" answerable against the rest
	// of the rotation's timeline rather than against wall-clock time in a
	// process that may have been restarted since.
	if want := clock.Now().UTC().Format(time.RFC3339); !strings.Contains(got, want) {
		t.Errorf("the discarded record = %q, want it to carry the time %s: a record with no "+
			"time cannot be told apart from one left by a rotation two months ago", got, want)
	}
	note := expectEvent(t, rec, corev1.EventTypeWarning, certs.ReasonRotationSlotDiscarded)
	if !strings.Contains(note, "ca-next.crt") || !strings.Contains(note, "abandoned") {
		t.Errorf("event = %q, want it to name the slot and say the rotation was abandoned: "+
			"a human reading this has to know both what was thrown away and what is left", note)
	}
}

// A broken ca-previous while switched completes the drop.
//
// This looks like the operator performing drop-old unasked, and it is not.
// The hold at `switched` exists so a rollback stays possible; a rollback signs
// with the previous CA, through RestorePrevious -> Reissue -> parseCA, on
// exactly these bytes. They stopped parsing, so the rollback was already
// impossible. Clearing the slot takes away no ability -- it records that the
// ability is gone. Nobody is stranded: the serving certificate chains to the
// new CA, which every agent trusts.
func TestAnUnparseableOutgoingCACompletesTheDrop(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute

	b := switchedAndHolding(t, s, clock, ctx, ns)
	// After the fixture: the start and the switch it performed are not
	// evidence about this call.
	rec := events.NewFakeRecorder(8)
	s.Recorder = rec

	// Break the outgoing CA in place, the way a hand-edit would.
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: certs.SecretName, Namespace: ns}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		t.Fatalf("get the secret: %v", err)
	}
	secret.Data["ca-previous.crt"] = []byte("-- not a certificate --\n")
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("break the outgoing CA: %v", err)
	}

	after, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation over a broken outgoing CA: %v", err)
	}
	if inFlight {
		t.Error("the rotation still reported itself in flight after the drop it was holding for was completed")
	}

	if err := s.Client.Get(ctx, key, secret); err != nil {
		t.Fatalf("read the secret back: %v", err)
	}
	for _, k := range []string{"ca-previous.crt", "ca-previous.key"} {
		if len(secret.Data[k]) != 0 {
			t.Errorf("%s survived the cleanup", k)
		}
	}
	// The drop is complete, not merely the slot emptied: the hold at
	// `switched` exists so a rollback stays possible, and a rollback signs
	// with these very bytes through RestorePrevious -> Reissue -> parseCA.
	// They stopped parsing, so it was already impossible; leaving the phase
	// at `switched` would advertise a choice nobody can make.
	if got := secret.Annotations[certs.AnnotationRotationPhase]; got != "" {
		t.Errorf("phase = %q after the outgoing CA was discarded, want it cleared — "+
			"the hold's only purpose is a rollback, and the bytes it would sign with are gone", got)
	}
	if got := secret.Annotations[certs.AnnotationRotationDiscarded]; !strings.Contains(got, "ca-previous.crt") {
		t.Errorf("the discarded record = %q, want it to name the slot", got)
	}
	// Nobody is stranded by the narrowing: the serving certificate chains to
	// the CA that is still published, which is the one every agent came to
	// trust during the overlap.
	serving := parseCert(t, after.ServingCertPEM)
	if err := serving.CheckSignatureFrom(parseCert(t, after.CACertPEM)); err != nil {
		t.Errorf("the serving certificate no longer chains to the published CA: %v", err)
	}
	if bytes.Contains(after.PublishedCA(), []byte("-- not a certificate --")) {
		t.Error("the discarded bytes are still published; every agent's trust store throws on the whole stream")
	}
	note := expectEvent(t, rec, corev1.EventTypeWarning, certs.ReasonRotationSlotDiscarded)
	if !strings.Contains(note, "ca-previous.crt") || !strings.Contains(note, "rollback") {
		t.Errorf("event = %q, want it to name the slot and why completing the drop took "+
			"nothing away: the reader's first thought is that the operator dropped the "+
			"outgoing CA unasked", note)
	}
}

// A broken slot with no phase set is cleared, and nothing else changes.
//
// There is nothing to abandon and nothing to complete; the slot simply must
// not be published, and the record is what tells whoever left it there.
func TestAnUnparseableSlotWithNoRotationIsJustCleared(t *testing.T) {
	s, _, ctx, ns := newStore(t)
	rec := events.NewFakeRecorder(8)
	s.Recorder = rec

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// A PEM envelope around something that is not a certificate: pem.Decode
	// is happy with it and the agent's CertificateFactory is not, so it is
	// the corruption a check built on pem.Decode alone would wave through.
	breakSlot(t, ctx, s, ns, "ca-previous.crt",
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")}))

	after, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation over a broken slot on an idle secret: %v", err)
	}
	if inFlight {
		t.Error("a secret with no phase set reported a rotation in flight")
	}

	secret := secretOf(t, ctx, s, ns)
	if len(secret.Data["ca-previous.crt"]) != 0 {
		t.Error("ca-previous.crt survived the cleanup")
	}
	if got, ok := secret.Annotations[certs.AnnotationRotationPhase]; ok {
		t.Errorf("phase = %q; clearing a leftover slot started or ended something", got)
	}
	got := secret.Annotations[certs.AnnotationRotationDiscarded]
	if !strings.Contains(got, "ca-previous.crt") {
		t.Errorf("the discarded record = %q, want it to name the slot", got)
	}
	if !strings.Contains(got, "parse certificate") {
		t.Errorf("the discarded record = %q, want it to carry the parse error", got)
	}
	// "Nothing else changes" is the other half of the claim: the certificate
	// the operator is serving right now has nothing to do with the slot.
	if !bytes.Equal(secret.Data["ca.crt"], b.CACertPEM) || !bytes.Equal(secret.Data["tls.crt"], b.ServingCertPEM) {
		t.Error("the cleanup rewrote the signing CA or the serving certificate")
	}
	if !bytes.Equal(after.CACertPEM, b.CACertPEM) {
		t.Error("the bundle that came back is not the one that went in, apart from the slot")
	}
	note := expectEvent(t, rec, corev1.EventTypeWarning, certs.ReasonRotationSlotDiscarded)
	if !strings.Contains(note, "ca-previous.crt") {
		t.Errorf("event = %q, want it to name the slot", note)
	}
}

// The next accepted start clears the record, so it never narrates an old
// failure beside a live rotation.
func TestAStartClearsTheDiscardedRecord(t *testing.T) {
	s, _, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	createNetwork(t, ctx, s.Client, ns, "net")
	breakSlot(t, ctx, s, ns, "ca-next.crt", []byte("-- not a certificate --\n"))
	requestRotation(t, ctx, s, certs.RequestStart)

	// The cleanup is this call's one step. The request is not acted on in the
	// same call and is not consumed either: it is picked up on the next tick,
	// against the state the cleanup left.
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation over a broken slot with a start pending: %v", err)
	}
	secret := secretOf(t, ctx, s, ns)
	if got := secret.Annotations[certs.AnnotationRotationDiscarded]; got == "" {
		t.Fatal("nothing was recorded, so there is no record for the start to clear")
	}
	if got := secret.Annotations[certs.AnnotationRotateRequest]; got != certs.RequestStart {
		t.Errorf("rotate-ca = %q after the cleanup, want %q: the cleanup is one step and "+
			"the request belongs to the next tick, which sees the cleaned state", got, certs.RequestStart)
	}

	b, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation (start) on the tick after the cleanup: %v", err)
	}
	if !inFlight || len(b.NextCACertPEM) == 0 {
		t.Fatalf("start was not acted on on the next tick (inFlight=%v)", inFlight)
	}
	secret = secretOf(t, ctx, s, ns)
	if got, ok := secret.Annotations[certs.AnnotationRotationDiscarded]; ok {
		t.Errorf("the discarded record = %q survived a start, want it removed: read beside "+
			"a phase of `distributing` it describes the running rotation, which it does not", got)
	}
	if got := secret.Annotations[certs.AnnotationRotationPhase]; got != certs.PhaseDistributing {
		t.Errorf("phase = %q, want %q", got, certs.PhaseDistributing)
	}
}

// Ensure does not fail because of a corrupt slot, and the operator starts.
//
// This is the guard on the obvious implementation. Provider.Start returns
// Ensure's error from a Runnable, so an error here is fatal at startup:
// validating in Ensure would mean a hand-edited annotation takes the operator
// down, which is a worse outage than the one it describes. Asserted directly
// because "just validate it where you read it" is what a later reader will
// reach for.
func TestACorruptSlotDoesNotFailEnsure(t *testing.T) {
	s, _, ctx, ns := newStore(t)

	first, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	breakSlot(t, ctx, s, ns, "ca-next.crt", []byte("-- not a certificate --\n"))

	again, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure failed over a corrupt rotation slot: %v. Provider.Start returns "+
			"this from a Runnable, so a hand-edited annotation would take the whole "+
			"operator down rather than be reported and cleaned up", err)
	}
	if !bytes.Equal(again.CACertPEM, first.CACertPEM) || !bytes.Equal(again.ServingCertPEM, first.ServingCertPEM) {
		t.Error("a corrupt slot made Ensure reissue the bundle; the slot is not part of what Validate judges")
	}
	if _, err := again.TLSCertificate(); err != nil {
		t.Errorf("the bundle Ensure returned cannot serve TLS: %v", err)
	}
	// And what reaches the agents omits it, which is the safety net the
	// report above complements rather than replaces.
	if bytes.Contains(again.PublishedCA(), []byte("-- not a certificate --")) {
		t.Error("the corrupt slot is in what PublishedCA returns")
	}
}

// An outgoing CA with a second PEM block after it is repaired, not discarded
// -- and the rollback it was holding for still works.
//
// This is the case the "a rollback was already impossible" argument does not
// cover. parseCA decodes the first block and ignores whatever follows, so
// RestorePrevious -> Reissue -> parseCA succeeds on these bytes: the rollback
// is alive, and clearing the slot would have killed it while the warning said
// it was already dead. Only trustManager, which reads the whole stream,
// objects -- so the repair is to publish what the operator already signs
// with.
func TestAMultiBlockOutgoingCAIsTruncatedAndTheRollbackStillWorks(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute

	b := switchedAndHolding(t, s, clock, ctx, ns)
	original := b.PreviousCACertPEM
	switched := b.CACertPEM
	rec := events.NewFakeRecorder(8)
	s.Recorder = rec

	// A chain pasted in: the CA, then a second certificate after it. Exactly
	// what a restore that carried an intermediate along produces.
	extra, _, err := certs.IssueCA(clock.Now())
	if err != nil {
		t.Fatalf("IssueCA for the second block: %v", err)
	}
	breakSlot(t, ctx, s, ns, "ca-previous.crt", append(append([]byte{}, original...), extra...))

	after, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation over a multi-block outgoing CA: %v", err)
	}
	if !inFlight {
		t.Error("the rotation reported itself finished; a repaired slot ends nothing")
	}

	secret := secretOf(t, ctx, s, ns)
	if !bytes.Equal(secret.Data["ca-previous.crt"], original) {
		t.Fatalf("ca-previous.crt = %d bytes, want the %d of its first block alone: the "+
			"repair keeps the certificate parseCA already signs with and drops what "+
			"followed it", len(secret.Data["ca-previous.crt"]), len(original))
	}
	if len(secret.Data["ca-previous.key"]) == 0 {
		t.Error("the outgoing key was dropped with the extra block; a rollback needs the pair")
	}
	if got := secret.Annotations[certs.AnnotationRotationPhase]; got != certs.PhaseSwitched {
		t.Errorf("phase = %q, want %q left alone: the rollback this hold exists for is "+
			"still possible, and completing the drop here would destroy it", got, certs.PhaseSwitched)
	}
	got := secret.Annotations[certs.AnnotationRotationDiscarded]
	if !strings.Contains(got, "ca-previous.crt") || !strings.Contains(got, "truncated to the first") {
		t.Errorf("the record = %q, want it to name the slot and say it was truncated rather "+
			"than discarded -- nothing was thrown away that anything could have used", got)
	}
	note := expectEvent(t, rec, corev1.EventTypeWarning, certs.ReasonRotationSlotTruncated)
	if !strings.Contains(note, "ca-previous.crt") {
		t.Errorf("event = %q, want it to name the slot", note)
	}

	// The proof that the slot is still worth something: the rollback runs.
	requestRotation(t, ctx, s, certs.RequestRollback)
	rolled, _, err := s.AdvanceRotation(ctx, after)
	if err != nil {
		t.Fatalf("rollback after the repair: %v. This is the ability the discard would "+
			"have destroyed while reporting that it was already gone", err)
	}
	if !bytes.Equal(rolled.CACertPEM, original) {
		t.Fatal("the rollback did not put the outgoing CA back in charge of signing")
	}
	if bytes.Equal(rolled.CACertPEM, switched) {
		t.Error("the rollback left the switched-to CA signing")
	}
	serving := parseCert(t, rolled.ServingCertPEM)
	if err := serving.CheckSignatureFrom(parseCert(t, original)); err != nil {
		t.Errorf("the serving certificate does not chain to the restored CA: %v", err)
	}
}

// The same repair for ca-next, because the rule keys on the failure mode and
// not on the slot: the rotation carries on and switches to the first block.
func TestAMultiBlockIncomingCAIsTruncatedAndTheRotationCarriesOn(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute

	b := startedAndDistributed(t, s, clock, ctx, ns)
	incoming := b.NextCACertPEM
	rec := events.NewFakeRecorder(8)
	s.Recorder = rec

	extra, _, err := certs.IssueCA(clock.Now())
	if err != nil {
		t.Fatalf("IssueCA for the second block: %v", err)
	}
	breakSlot(t, ctx, s, ns, "ca-next.crt", append(append([]byte{}, incoming...), extra...))

	after, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation over a multi-block incoming CA: %v", err)
	}
	if !inFlight {
		t.Error("a repaired incoming CA ended the rotation")
	}
	if !bytes.Equal(after.NextCACertPEM, incoming) {
		t.Fatal("ca-next.crt was not truncated to its first block")
	}
	if got := phaseOf(t, s, ctx, ns); got != certs.PhaseDistributing {
		t.Errorf("phase = %q, want %q: nothing about the rotation had to change", got, certs.PhaseDistributing)
	}
	expectEvent(t, rec, corev1.EventTypeWarning, certs.ReasonRotationSlotTruncated)

	// And the rotation still finishes, on the repaired bytes.
	writeCA(t, ctx, s.Client, ns, after.PublishedCA())
	after = switchNow(t, ctx, s, clock, after)
	if !bytes.Equal(after.CACertPEM, incoming) {
		t.Error("the switch did not promote the repaired incoming CA")
	}
}

// Two broken slots in one call are two records and two events, and neither
// record borrows the other's outcome.
func TestTwoBrokenSlotsAreOneStepAndTwoRecords(t *testing.T) {
	// A ca-next occupying a `switched` secret is a state no transition
	// produces, which is the point: both of these arrive by hand, and the
	// rule has to hold for a combination the sequence never builds.
	t.Run("one cleared and one truncated", func(t *testing.T) {
		s, clock, ctx, ns := newStore(t)
		s.AgentSessionDeadline = 10 * time.Minute

		b := switchedAndHolding(t, s, clock, ctx, ns)
		spare, _, err := certs.IssueCA(clock.Now())
		if err != nil {
			t.Fatalf("IssueCA: %v", err)
		}
		rec := events.NewFakeRecorder(8)
		s.Recorder = rec

		breakSlot(t, ctx, s, ns, "ca-previous.crt", []byte("-- not a certificate --\n"))
		breakSlot(t, ctx, s, ns, "ca-next.crt", append(append([]byte{}, spare...), spare...))

		if _, _, err := s.AdvanceRotation(ctx, b); err != nil {
			t.Fatalf("AdvanceRotation over two broken slots: %v", err)
		}

		// One read-back, after one call: both changes and both records are
		// there. (One applyStep carries them; a second update would satisfy
		// this assertion too, so it shows the record is not forgotten rather
		// than that the write is atomic.)
		secret := secretOf(t, ctx, s, ns)
		if len(secret.Data["ca-previous.crt"]) != 0 {
			t.Error("the unparseable outgoing CA survived")
		}
		if !bytes.Equal(secret.Data["ca-next.crt"], spare) {
			t.Error("the multi-block incoming CA was not truncated to its first block")
		}
		if got := secret.Annotations[certs.AnnotationRotationPhase]; got != "" {
			t.Errorf("phase = %q, want it cleared: the slot the hold existed for is gone", got)
		}
		record := secret.Annotations[certs.AnnotationRotationDiscarded]
		for _, want := range []string{"ca-next.crt", "ca-previous.crt", "truncated to the first"} {
			if !strings.Contains(record, want) {
				t.Errorf("the record = %q, want it to contain %q: one call touched two slots "+
					"and the record is the only account of both", record, want)
			}
		}

		notes := drainEvents(t, rec, 2)
		truncated := noteNaming(t, notes, "ca-next.crt")
		if !strings.HasPrefix(truncated, corev1.EventTypeWarning+" "+certs.ReasonRotationSlotTruncated+" ") {
			t.Errorf("the ca-next event = %q, want a %s: it was repaired, not thrown away",
				truncated, certs.ReasonRotationSlotTruncated)
		}
		discarded := noteNaming(t, notes, "ca-previous.crt")
		if !strings.HasPrefix(discarded, corev1.EventTypeWarning+" "+certs.ReasonRotationSlotDiscarded+" ") {
			t.Errorf("the ca-previous event = %q, want a %s", discarded, certs.ReasonRotationSlotDiscarded)
		}
	})

	// The oddity a test would have caught: with both slots cleared at
	// `switched`, an outcome clause written per call would put "the drop was
	// completed" on the ca-next event too, where it describes nothing.
	t.Run("both cleared, and only one names the drop", func(t *testing.T) {
		s, clock, ctx, ns := newStore(t)
		s.AgentSessionDeadline = 10 * time.Minute

		b := switchedAndHolding(t, s, clock, ctx, ns)
		rec := events.NewFakeRecorder(8)
		s.Recorder = rec

		breakSlot(t, ctx, s, ns, "ca-previous.crt", []byte("-- not a certificate --\n"))
		breakSlot(t, ctx, s, ns, "ca-next.crt",
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")}))

		if _, _, err := s.AdvanceRotation(ctx, b); err != nil {
			t.Fatalf("AdvanceRotation over two unparseable slots: %v", err)
		}

		secret := secretOf(t, ctx, s, ns)
		for _, k := range []string{"ca-previous.crt", "ca-next.crt"} {
			if len(secret.Data[k]) != 0 {
				t.Errorf("%s survived the cleanup", k)
			}
		}

		notes := drainEvents(t, rec, 2)
		previous := noteNaming(t, notes, "ca-previous.crt")
		if !strings.Contains(previous, "the drop was completed") {
			t.Errorf("the ca-previous event = %q, want it to say the drop was completed: "+
				"that is the slot the hold at switched existed for", previous)
		}
		next := noteNaming(t, notes, "ca-next.crt")
		if strings.Contains(next, "the drop was completed") {
			t.Errorf("the ca-next event = %q, want the drop clause left off it: the drop was "+
				"about ca-previous, and a reader triaging this one would go looking for a "+
				"rollback that had nothing to do with it", next)
		}
		if !strings.Contains(next, "no rotation depended on this slot") {
			t.Errorf("the ca-next event = %q, want it to say what was true of this slot: a "+
				"ca-next in a switched secret is a state no transition produces and "+
				"nothing was waiting on it", next)
		}
	})
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

// switchedAndHolding leaves the rotation where it stops on its own: switched,
// with the outgoing CA still in ca-previous.* and nothing that will advance
// until a human asks. startedAndDistributed followed by switchNow, named for
// the state rather than the path because that state is what its callers are
// about.
func switchedAndHolding(t *testing.T, s *certs.Store, clock *testClock, ctx context.Context, ns string) *certs.Bundle {
	t.Helper()
	b := switchNow(t, ctx, s, clock, startedAndDistributed(t, s, clock, ctx, ns))
	if len(b.PreviousCACertPEM) == 0 {
		t.Fatal("the switch kept no outgoing CA, so there is no slot to break")
	}
	if got := phaseOf(t, s, ctx, ns); got != certs.PhaseSwitched {
		t.Fatalf("phase = %q after the switch, want %q", got, certs.PhaseSwitched)
	}
	return b
}

// breakSlot puts bytes into one of the secret's rotation slots, the way a
// person with kubectl and a paste buffer would. The bundle the caller is
// holding is deliberately not updated: AdvanceRotation re-reads the secret,
// and this is the edit it re-reads for.
func breakSlot(t *testing.T, ctx context.Context, s *certs.Store, ns, key string, certPEM []byte) {
	t.Helper()
	secret := secretOf(t, ctx, s, ns)
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[key] = certPEM
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("break %s on the secret: %v", key, err)
	}
}

// expectEvent takes the one event the call under test recorded and checks its
// type and reason, failing rather than hanging when nothing was recorded --
// a missing event being the whole failure these assertions exist to catch.
//
// The package certs counterpart in rotation_envtest_test.go is the original;
// this copy exists because an external test package cannot call an unexported
// helper, not because the two differ.
func expectEvent(t *testing.T, rec *events.FakeRecorder, eventtype, reason string) string {
	t.Helper()
	select {
	case got := <-rec.Events:
		if want := eventtype + " " + reason + " "; !strings.HasPrefix(got, want) {
			t.Fatalf("event = %q, want it to start %q", got, want)
		}
		return got
	default:
		t.Fatalf("no %s %s event was recorded", eventtype, reason)
		return ""
	}
}

// drainEvents takes exactly n events off the recorder, failing if fewer were
// recorded or if an n+1th is waiting. Order is not asserted: it is the order
// of a slice inside the function under test, which is not a property worth
// pinning, so the caller picks its event out with noteNaming.
func drainEvents(t *testing.T, rec *events.FakeRecorder, n int) []string {
	t.Helper()
	var got []string
	for i := 0; i < n; i++ {
		select {
		case e := <-rec.Events:
			got = append(got, e)
		default:
			t.Fatalf("only %d of %d events were recorded: %q", i, n, got)
		}
	}
	select {
	case extra := <-rec.Events:
		t.Fatalf("a %dth event was recorded: %q", n+1, extra)
	default:
	}
	return got
}

// noteNaming returns the one drained event that mentions slot.
func noteNaming(t *testing.T, notes []string, slot string) string {
	t.Helper()
	var found string
	for _, n := range notes {
		if strings.Contains(n, slot) {
			if found != "" {
				t.Fatalf("two events name %s: %q and %q", slot, found, n)
			}
			found = n
		}
	}
	if found == "" {
		t.Fatalf("no event names %s; recorded: %q", slot, notes)
	}
	return found
}
