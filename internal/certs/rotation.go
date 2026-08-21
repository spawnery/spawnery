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

package certs

import (
	"context"
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// The annotations are the whole operator interface to a rotation: a human
// asks for a step by setting AnnotationRotateRequest, and reads where the
// procedure has got to from the other three. They live on the TLS secret
// rather than on a CRD because the secret is already the one object that
// holds the rotation's state, and a restart has to be able to pick the
// sequence up from it alone.
const (
	AnnotationRotateRequest     = "spawnery.cloud/rotate-ca"
	AnnotationRotationPhase     = "spawnery.cloud/ca-rotation-phase"
	AnnotationRotationSince     = "spawnery.cloud/ca-rotation-since"
	AnnotationRotationBlockedOn = "spawnery.cloud/ca-rotation-blocked-on"

	// AnnotationRotationDiscarded is what is left of a rotation slot whose
	// certificate would not parse: the slot's name, the reason, whether the
	// slot was cleared or merely truncated to its first PEM block, and when.
	// Discarded bytes are not kept -- they are the one thing that must stop
	// being published -- so this is the whole of what a diagnosis has, and it
	// outlives the Warning event, which expires after about an hour. One key
	// for both outcomes, because a reader asking what became of their slots
	// should not have to know two annotations to find out, and both are
	// cleared by the same next accepted start, so neither ever narrates an
	// old failure beside a running rotation.
	AnnotationRotationDiscarded = "spawnery.cloud/ca-rotation-discarded"

	RequestStart    = "start"
	RequestDropOld  = "drop-old"
	RequestRollback = "rollback"

	PhaseDistributing = "distributing"
	PhaseSwitched     = "switched"
)

// RotationCheckInterval is how often the provider looks while a rotation is in
// flight. RenewCheckInterval's hour is right for a certificate that lives 90
// days and wrong for a procedure that takes a quarter of an hour.
const RotationCheckInterval = 30 * time.Second

// projectionMargin covers the gap between the operator writing the CA
// ConfigMap and the kubelet projecting it into the pods that mount it. The
// kubelet's --sync-frequency defaults to one minute and the watch-based
// projection is faster than that in practice, so two minutes is the margin
// rather than the estimate. It is arithmetic on a documented period, not a
// measurement: the thing that would make it wrong is a cluster configured with
// a much longer sync frequency, which this operator cannot see.
const projectionMargin = 2 * time.Minute

// maxBlockedNamesInAnnotation bounds the blocked-on list. Ten namespace names
// at the 63-character maximum, with separators and the prefix, stay well below
// the 1024 bytes an event note allows (internal/controller/events.go documents
// that limit as probed against a real API server).
const maxBlockedNamesInAnnotation = 10

// Cluster-wide and unqualified, unlike the secrets marker in store.go: the
// gate has to see every namespace an agent could be running in, and it cannot
// know which those are in advance. It duplicates the right
// internal/controller/orphan.go already asks for -- controller-gen merges the
// two into one rule -- and is repeated here so the permission is documented
// where it is used, the way every other pods marker in this repository is.
// It sits in a comment block of its own with a blank line under it:
// docs/known-issues.md records that controller-gen silently ignores a marker
// buried inside a declaration's doc comment.
// +kubebuilder:rbac:groups="",resources=pods,verbs=list

// AdvanceRotation performs at most one step of a rotation and returns the
// bundle to publish and whether a rotation is still in flight -- which is
// what decides the provider's cadence.
//
// One step per call, never two. A call that walked several transitions would
// be untestable at the boundaries -- the overlap window only means anything
// if the switch is a separate call from the gate that opened it -- and it
// would make a partial write ambiguous: the phase in the secret would no
// longer say which transition the operator was in the middle of.
//
// Any error comes back with a nil bundle and inFlight false. The caller keeps
// publishing whatever Ensure gave it; a rotation that cannot advance is a
// stalled procedure, not an unusable certificate.
func (s *Store) AdvanceRotation(ctx context.Context, current *Bundle) (*Bundle, bool, error) {
	// Read fresh rather than trusting what Ensure saw: the secret has two
	// writers now, and the annotation a human set a second ago is the entire
	// input to this function.
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: s.Name, Namespace: s.Namespace}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		return nil, false, fmt.Errorf("get %s: %w", s.Name, err)
	}

	phase := secret.Annotations[AnnotationRotationPhase]

	// Before any request, because a slot nobody can parse is the one thing
	// here that is already doing damage -- every byte of it reaches every
	// agent's trust store -- and because a request acted on in the same call
	// would be acted on against a state that is about to change under it.
	// This is the step this call takes when it takes it: a request stays
	// where it is and is picked up on the next tick, by which time the slots
	// it would work from are the repaired or cleared ones.
	cleaned, inFlight, err := s.repairOrDiscardSlots(ctx, current, phase, secret)
	if err != nil {
		return nil, false, err
	}
	if cleaned != nil {
		return cleaned, inFlight, nil
	}

	switch request := secret.Annotations[AnnotationRotateRequest]; request {
	case "":
	case RequestStart, RequestDropOld, RequestRollback:
		return s.applyRequest(ctx, current, request, phase)
	default:
		// Left in place, because clearing an annotation you did not
		// understand hides the typo that produced it -- but reported and then
		// stepped over, not treated as a stop. Halting here would freeze a
		// rotation that may be mid-window, with `phase` and `since` still
		// reading as if it were progressing and only a log line saying
		// otherwise. Continuing costs at most one unwanted automatic step,
		// the switch, and that step is undone by a rollback that re-signs
		// with a CA every agent has trusted the whole time.
		log.FromContext(ctx).Info("ignoring an unrecognised CA rotation request",
			"annotation", AnnotationRotateRequest, "value", request,
			"expected", strings.Join([]string{RequestStart, RequestDropOld, RequestRollback}, ", "))
		// The log line above is the only signal a human who is not tailing
		// this operator's logs will ever see. %.150s: request is arbitrary
		// text a human typed, fmt counts a string precision in runes, so this
		// cuts on a rune boundary by construction and adds at most 600 bytes
		// -- nowhere near the note's 1024-byte limit even before the fixed
		// text around it.
		s.event(corev1.EventTypeWarning, ReasonRotationRequestUnrecognised, actionReportUnrecognisedRequest,
			"%s=%.150s is not %s, %s or %s; left in place and stepped over rather than halting the rotation",
			AnnotationRotateRequest, request, RequestStart, RequestDropOld, RequestRollback)
	}
	return s.drivePhase(ctx, current, phase, secret.Annotations[AnnotationRotationSince],
		secret.Annotations[AnnotationRotationBlockedOn])
}

// repairOrDiscardSlots puts every rotation slot an agent could not parse back
// into a state it can, records what it did, and ends the rotation when a slot
// it had to throw away is the one the current phase depends on.
//
// It returns a nil bundle when there was nothing wrong, which is every
// ordinary call; anything else is a step taken, and AdvanceRotation returns
// on it.
//
// **One question decides repair or discard, and it decides it the same way
// for either slot: is the slot's first PEM block a certificate?**
//
// If it is, the slot is repairable, because that block is precisely the one
// parseCA was already using -- a certificate with a chain pasted after it, or
// a header pasted in front of it, still signs perfectly well. Truncating to
// it makes publication and signing agree again, which is the whole of the
// defect; nothing usable is lost, no phase moves, and the rotation carries
// on. (If the slot already *is* that block, parsableCert returns nil and this
// function never sees it.)
//
// If it is not -- not PEM at all, or a PEM envelope around something that is
// not a certificate -- then parseCA fails on that same first block, so the
// slot is unusable in fact and not only in publication, and it is cleared.
//
// **For a cleared slot the phase decides the end state.** The rotation is
// abandoned (`distributing`, whose next step is to promote ca-next) or the
// drop completed (`switched`, whose only remaining step is a rollback signed
// with ca-previous) only when the cleared slot is the one that step needs.
// Anything else -- a ca-previous occupying a `distributing` secret, which no
// transition produces -- is cleared and reported without disturbing a
// sequence that does not depend on it.
//
// Completing the drop is not the operator performing drop-old unasked, and
// the repair above is what makes that true. The hold at `switched` exists so
// a rollback stays possible, and a rollback signs with these very bytes
// (RestorePrevious -> Reissue -> parseCA). A slot only reaches this branch
// when parseCA fails on the same first block parsableCert rejected, so the
// rollback was already impossible and clearing the slot records that rather
// than causing it. Every slot on which the rollback would still have worked
// is repaired instead -- which matters because clearing kills a rollback
// whatever the reason, and "clear it but hold the phase" would advertise a
// hold nobody could act on. Nobody is stranded either: the
// serving certificate chains to the new CA, which every agent came to trust
// during the overlap.
//
// A repaired slot takes **both** its halves from the secret, not from
// current: AdvanceRotation re-reads the secret precisely because it has two
// writers, and current may predate the hand-edit. Pairing the secret's
// certificate with current's older key would produce a slot that cannot sign,
// and applyStep rewrites the whole of Data from the bundle, so the newer key
// would be gone -- SwitchToNext would then fail on every tick, with parseCA's
// "CA key does not match" reaching nobody but the log.
func (s *Store) repairOrDiscardSlots(ctx context.Context, current *Bundle, phase string, stored *corev1.Secret) (*Bundle, bool, error) {
	slots := []struct {
		certKey        string
		keyKey         string
		dependentPhase string
		clear          func(*Bundle)
		setPair        func(*Bundle, []byte, []byte)
	}{
		// Only the certificate halves are judged, because they are what gets
		// published. A malformed key reaches no agent and already fails
		// loudly at the moment it matters, in parseCA. The key is dropped
		// with its certificate all the same: secretFor writes a slot's two
		// keys only while its certificate is occupied.
		{keyNextCACert, keyNextCAKey, PhaseDistributing,
			func(b *Bundle) { b.NextCACertPEM, b.NextCAKeyPEM = nil, nil },
			func(b *Bundle, cert, key []byte) { b.NextCACertPEM, b.NextCAKeyPEM = cert, key }},
		{keyPreviousCACert, keyPreviousCAKey, PhaseSwitched,
			func(b *Bundle) { b.PreviousCACertPEM, b.PreviousCAKeyPEM = nil, nil },
			func(b *Bundle, cert, key []byte) { b.PreviousCACertPEM, b.PreviousCAKeyPEM = cert, key }},
	}

	// One entry per slot this call touched: what it was called, why, and
	// whether the slot survived. record is the annotation's wording and
	// outcome the event's closing clause, which is per slot rather than per
	// call -- two broken slots on one secret produce two records, and a
	// sentence about the drop belongs only to the slot the drop was about.
	type change struct {
		certKey  string
		record   string
		reason   string
		outcome  string
		repaired bool
	}
	var changes []change
	fresh := *current
	endsRotation := false
	for _, slot := range slots {
		certPEM := stored.Data[slot.certKey]
		if len(certPEM) == 0 {
			continue
		}
		reason := parsableCert(certPEM)
		if reason == nil {
			continue
		}
		if errors.Is(reason, errNotOnlyTheFirstBlock) {
			slot.setPair(&fresh, firstPEMBlock(certPEM), stored.Data[slot.keyKey])
			changes = append(changes, change{
				certKey:  slot.certKey,
				record:   fmt.Sprintf("%s: %.150s; truncated to that block", slot.certKey, reason),
				repaired: true,
			})
			continue
		}
		slot.clear(&fresh)
		dependent := phase == slot.dependentPhase
		endsRotation = endsRotation || dependent
		changes = append(changes, change{
			certKey: slot.certKey,
			record:  fmt.Sprintf("%s: %.150s", slot.certKey, reason),
			reason:  reason.Error(),
			outcome: discardOutcome(phase, dependent),
		})
	}
	if len(changes) == 0 {
		return nil, false, nil
	}

	records := make([]string, 0, len(changes))
	for _, c := range changes {
		records = append(records, c.record)
	}
	// The slot, the reason and the time, which is what a diagnosis needs; the
	// bytes are the one thing that must not survive a discard. One annotation
	// for both outcomes, because it is the single durable answer to "what
	// happened to my slots", it is cleared by the same start, and its own
	// wording says which of the two happened. Written in the same applyStep
	// as the repair or the cleanup, so the record cannot land without the
	// change or the change without the record.
	record := fmt.Sprintf("%s (%s)", strings.Join(records, "; "),
		s.Clock().UTC().Format(time.RFC3339))
	err := s.applyStep(ctx, &fresh, func(secret *corev1.Secret) {
		setAnnotation(secret, AnnotationRotationDiscarded, record)
		if endsRotation {
			clearRotationAnnotations(secret)
		}
	})
	if err != nil {
		return nil, false, err
	}

	remaining := phase
	if endsRotation {
		remaining = ""
		// Both gauges, as drop-old and rollback do: with no rotation left,
		// nothing is blocked on anything.
		RotationBlockedNamespaces.Set(0)
	}
	// drivePhase's hoisted Set is not reached on this path, and the phase
	// gauge is only self-healing across a restart if every tick that reads
	// the annotation writes it.
	setRotationPhase(remaining)

	// After the write, unlike refuse's event: a refusal is a fact the moment
	// it is decided, but these are claims about the secret, and an applyStep
	// that failed leaves every slot exactly where it was.
	for _, c := range changes {
		if c.repaired {
			// Its own reason rather than a RotationSlotDiscarded whose note
			// says otherwise: the reason is the field a human triages on, and
			// nothing was discarded here -- the slot is still in the secret,
			// still in the rotation, and now usable. No bound is needed: the
			// note is 178 bytes of literal ASCII plus a slot name of at most
			// 15, so 193 against the same 1024-byte limit.
			s.event(corev1.EventTypeWarning, ReasonRotationSlotTruncated, actionTruncateRotationSlot,
				"%s held more than its first PEM block and has been truncated to that block, "+
					"which is the one the operator already signs with: nothing usable was "+
					"lost and the slot stays where it is", c.certKey)
			continue
		}
		// %.150s: reason carries x509's own wording, which embeds the
		// structure it choked on, so the bound has to hold for text this
		// package did not write. fmt counts a string precision in runes, so
		// the cut lands on a rune boundary by construction and 150 runes are
		// at most 600 bytes; the fixed text is 61, the longer slot name 15
		// and the longest outcome 127, for 803 bytes worst case -- inside the
		// 1024 internal/controller/events.go documents, having probed that
		// limit against a real API server and found it counted in bytes even
		// though the API server's own message says characters.
		s.event(corev1.EventTypeWarning, ReasonRotationSlotDiscarded, actionDiscardRotationSlot,
			"%s could not be parsed and has been cleared from the secret: %.150s; %s",
			c.certKey, c.reason, c.outcome)
	}
	return &fresh, remaining != "", nil
}

// discardOutcome is what the event says became of the rotation, for one
// cleared slot. Per slot and not per call: with two slots broken at once, a
// clause about the drop belongs to the slot the drop was about and would read
// as a claim about the other one.
func discardOutcome(phase string, dependent bool) string {
	switch {
	case !dependent:
		// Deliberately says nothing about where the sequence ended up: the
		// other slot may have ended it in this same call, and this clause has
		// to stay true either way.
		return "no rotation depended on this slot, so nothing about the sequence turns on it"
	case phase == PhaseSwitched:
		return "the drop was completed: a rollback would have signed with these bytes, " +
			"so the hold at switched was already impossible to act on"
	default:
		// dependent is only true for the two phases above, so this is
		// `distributing`: the incoming CA was the whole of what it had left
		// to do.
		return "the rotation was abandoned, and ca.crt is published alone -- " +
			"nothing usable was ever distributed"
	}
}

// applyRequest performs the step a human asked for, or refuses it.
//
// The refusals are as much of the behaviour as the transitions, and a refused
// request is consumed like an accepted one. A recognised request left in
// place is worse than a complaint repeated every tick: AdvanceRotation
// reaches drivePhase only when rotate-ca is empty, so it freezes the sequence
// where it stands. A drop-old set during `distributing` would stall the
// rotation mid-window rather than -- as one might fear -- lie in wait and
// fire the moment the phase became `switched`; it can never become
// `switched`, which is the whole trouble. Consuming it is the same judgement
// that leaves an unrecognised value reported but stepped over: nothing about
// this procedure is improved by stopping it in place.
func (s *Store) applyRequest(ctx context.Context, current *Bundle, request, phase string) (*Bundle, bool, error) {
	now := s.Clock()
	consume := func(secret *corev1.Secret) {
		// Only if it is still the request this call decided on: between the
		// read above and this write a human may have replaced it, and
		// deleting an instruction nobody has acted on would lose it silently.
		if secret.Annotations[AnnotationRotateRequest] == request {
			delete(secret.Annotations, AnnotationRotateRequest)
		}
	}

	switch request {
	case RequestStart:
		if phase != "" {
			// A third CA has nowhere to go: the bundle has one spare slot,
			// and while `switched` it is holding the outgoing CA that a
			// rollback still needs to be able to sign with.
			return s.refuse(ctx, consume, fmt.Errorf(
				"%s=%s refused: a rotation is already in the %q phase; finish it with %s=%s or %s=%s first",
				AnnotationRotateRequest, RequestStart, phase,
				AnnotationRotateRequest, RequestDropOld, AnnotationRotateRequest, RequestRollback))
		}
		certPEM, keyPEM, err := IssueCA(now)
		if err != nil {
			return nil, false, err
		}
		// The incoming CA signs nothing yet. It is published beside the one
		// that does, and that is the whole of this step.
		fresh := current.WithNextCA(certPEM, keyPEM)
		err = s.applyStep(ctx, fresh, func(secret *corev1.Secret) {
			setAnnotation(secret, AnnotationRotationPhase, PhaseDistributing)
			delete(secret.Annotations, AnnotationRotationSince)
			delete(secret.Annotations, AnnotationRotationBlockedOn)
			// In the same mutate as the phase, because the two are read
			// together: a discarded record left beside a phase of
			// `distributing` describes the rotation that is running, and it
			// does not -- it is about a slot that was thrown away before this
			// one started.
			delete(secret.Annotations, AnnotationRotationDiscarded)
			consume(secret)
		})
		if err != nil {
			return nil, false, err
		}
		setRotationPhase(PhaseDistributing)
		// RotationBlockedNamespaces is left alone here rather than written to
		// 0: this branch is only reachable with phase == "", which means the
		// gauge already reads 0 -- drop-old and rollback both set it to 0 on
		// their way to phase == "", and a process that has never rotated
		// exports 0 from its very first scrape, since RotationBlockedNamespaces
		// is a plain prometheus.NewGauge registered in init(). Writing 0 here
		// would be the same instruction repeated, not a different one. The gate
		// runs on the very next tick (drivePhase, since == "") and sets the
		// real count within RotationCheckInterval regardless.
		s.event(corev1.EventTypeNormal, ReasonRotationStarted, actionStartRotation,
			"published a second CA (ca-next.crt); switching once every namespace holding a "+
				"Network confirms it and the overlap window has elapsed")
		return fresh, true, nil

	case RequestDropOld:
		if phase != PhaseSwitched {
			return s.refuse(ctx, consume, fmt.Errorf(
				"%s=%s refused in the %q phase: the CA it would drop is the one signing the serving certificate",
				AnnotationRotateRequest, RequestDropOld, phase))
		}
		fresh := current.WithoutRotation()
		err := s.applyStep(ctx, fresh, func(secret *corev1.Secret) {
			clearRotationAnnotations(secret)
			consume(secret)
		})
		if err != nil {
			return nil, false, err
		}
		// This is the path the design calls out by name: a rotation that just
		// ended and a gauge left at its last value are indistinguishable
		// without this. Both gauges, not just the phase -- nothing is blocked
		// on anything once there is no incoming CA left to distribute.
		setRotationPhase(phaseNone)
		RotationBlockedNamespaces.Set(0)
		s.event(corev1.EventTypeNormal, ReasonRotationCompleted, actionCompleteRotation,
			"dropped the outgoing CA; ca.crt is the only CA published now")
		return fresh, false, nil

	case RequestRollback:
		var fresh *Bundle
		var err error
		switch phase {
		case PhaseDistributing:
			// Nothing was signed with the incoming CA, so discarding it
			// strands nobody.
			fresh = current.WithoutRotation()
		case PhaseSwitched:
			fresh, err = current.RestorePrevious(now, s.DNSNames)
		default:
			return s.refuse(ctx, consume, fmt.Errorf(
				"%s=%s refused: no rotation is in progress", AnnotationRotateRequest, RequestRollback))
		}
		if err != nil {
			return nil, false, err
		}
		err = s.applyStep(ctx, fresh, func(secret *corev1.Secret) {
			clearRotationAnnotations(secret)
			consume(secret)
		})
		if err != nil {
			return nil, false, err
		}
		// A rollback ends a rotation exactly as drop-old does, so it gets the
		// same gauge treatment -- but no event of its own: the design's
		// vocabulary (§4) has no RotationRolledBack, and the request that
		// caused this is already visible as the rotate-ca=rollback a human
		// wrote, acted on and consumed.
		setRotationPhase(phaseNone)
		RotationBlockedNamespaces.Set(0)
		return fresh, false, nil
	}

	// Unreachable: AdvanceRotation dispatches only the three values above
	// here. Kept because Go needs a terminating statement, and a caller added
	// later should hear about it rather than get a silent no-op.
	return nil, false, fmt.Errorf("applyRequest called with an unrecognised %s=%q",
		AnnotationRotateRequest, request)
}

// refuse consumes the request and reports why it was not carried out.
//
// The event is not decoration. A refusal deletes the annotation within one
// tick and writes nothing else, so without it the whole trace is one line in
// the operator's log -- and the human who set the annotation is not the one
// tailing that log. The failure it produces is a human under pressure sending
// drop-old a little early: the annotation vanishes inside 30 seconds, the
// secret says nothing, and the procedure looks as though it swallowed the
// instruction. That path was the only one of the two request dead ends with
// no event at all, while the less likely and less consequential typo path had
// ReasonRotationRequestUnrecognised.
func (s *Store) refuse(ctx context.Context, consume func(*corev1.Secret), reason error) (*Bundle, bool, error) {
	// Recorded before the consume rather than after it: the refusal is a fact
	// as soon as this function is entered, and a consume that fails is
	// exactly the case that most needs the event -- the annotation is then
	// still sitting there with nothing saying why it was not acted on.
	//
	// %.230s: reason embeds the phase annotation, which a human can hand-edit
	// to arbitrary text, so the bound has to hold for any input rather than
	// for the inputs this code happens to produce. fmt counts a string
	// precision in runes, so the cut lands on a rune boundary by construction;
	// 230 runes are at most 920 bytes, and the 74-byte suffix below brings the
	// worst case to 994 -- inside the 1024 bytes internal/controller/events.go
	// documents, having probed it against a real API server. 250 would not
	// have been: 1000 + 74 overruns it, and only the fixed ASCII prefix every
	// reason.Error() happens to start with kept it safe.
	s.event(corev1.EventTypeWarning, ReasonRotationRequestRefused, actionRefuseRotationRequest,
		"%.230s; the request was consumed, so setting it again is what asks a second time",
		reason.Error())
	if err := s.applyStep(ctx, nil, consume); err != nil {
		return nil, false, fmt.Errorf("%w (and clearing it failed: %v)", reason, err)
	}
	return nil, false, reason
}

// drivePhase is what happens on a tick with no request pending: the gate, the
// window, and then the switch. The `switched` phase deliberately has no
// transition out of it -- the operator holds there until a human asks.
func (s *Store) drivePhase(ctx context.Context, current *Bundle, phase, since, blockedOn string) (*Bundle, bool, error) {
	// Set on every tick, not only on the ones that change it, and hoisted
	// above every return in this function -- including the two guards just
	// below, which is the point: a freshly restarted process with a secret
	// that says `distributing` but has no incoming CA, or no
	// AgentSessionDeadline configured, is exactly the state somebody would
	// want to query, and those two guards used to return before this ever
	// ran, leaving the GaugeVec with no series for that phase at all --
	// Provider.Start calls in here at most once an hour while idle
	// (RenewCheckInterval) or every RotationCheckInterval while distributing,
	// and each of those ticks is what re-populates the gauge after a restart,
	// since the Prometheus client library starts a GaugeVec's series unset
	// until the first Set.
	setRotationPhase(phase)
	if phase != PhaseDistributing {
		return current, phase == PhaseSwitched, nil
	}
	if len(current.NextCACertPEM) == 0 {
		return nil, false, fmt.Errorf("the secret says %s=%s but holds no incoming CA",
			AnnotationRotationPhase, PhaseDistributing)
	}
	// Refused rather than computed against a window that is short by exactly
	// the deadline: the bound this phase rests on is that every agent stream
	// opened before the CA ConfigMap changed has since been closed, and only
	// the running operator's own --agent-session-deadline says when that is.
	if s.AgentSessionDeadline <= 0 {
		return nil, false, fmt.Errorf(
			"refusing to time the overlap window: Store.AgentSessionDeadline is unset, "+
				"so --agent-session-deadline never reached %s", SecretName)
	}
	now := s.Clock()

	if since == "" {
		missing, err := s.namespacesMissingCA(ctx, current.NextCACertPEM)
		if err != nil {
			return nil, false, err
		}
		// A ground-truth read just succeeded, so this is always accurate --
		// unlike the annotation below, there is no reason to write it only on
		// change: RotationBlockedNamespaces is a gauge, not a Kubernetes
		// object, and setting it to the value it already holds costs nothing.
		// (RotationPhase itself needs no second Set here: the hoisted call at
		// the top of this function already set it to PhaseDistributing, and
		// nothing between there and here changes phase.)
		RotationBlockedNamespaces.Set(float64(len(missing)))
		if len(missing) > 0 {
			note := blockedOnNote(missing)
			if note != blockedOn {
				// Written only when it changed, so a gate that stays blocked
				// for an hour is an hour of reads and not an hour of writes.
				if err := s.applyStep(ctx, nil, func(secret *corev1.Secret) {
					setAnnotation(secret, AnnotationRotationBlockedOn, note)
				}); err != nil {
					return nil, false, err
				}
				// Same gate as the annotation write, and for the same reason:
				// an event fired every 30 seconds for as long as the gate
				// holds would bury the one that said something changed.
				s.event(corev1.EventTypeWarning, ReasonRotationBlocked, actionBlockRotation, "%s", note)
			}
			return current, true, nil
		}
		// The window starts here and not at `start`: it measures the time
		// since the last namespace received the new CA, which is the only
		// moment from which "every agent has re-read the bundle" can be
		// counted.
		err = s.applyStep(ctx, nil, func(secret *corev1.Secret) {
			setAnnotation(secret, AnnotationRotationSince, now.UTC().Format(time.RFC3339))
			delete(secret.Annotations, AnnotationRotationBlockedOn)
		})
		if err != nil {
			return nil, false, err
		}
		return current, true, nil
	}

	// Deliberately no second run of the gate. A Network created after `since`
	// was stamped gets the current bundle -- already two CAs -- in its
	// namespace's ConfigMap on its first reconcile, and its pods have never
	// held anything else, so there is nothing for the window to protect. A
	// gate re-run every tick would let a cluster where networks are created
	// regularly push the switch out forever.
	stamped, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s=%q: %w", AnnotationRotationSince, since, err)
	}
	// The flag this operator is running with is enough; nothing has to be
	// persisted from the moment the rotation started. The agent endpoint is
	// leader-bound on the same manager as this provider, so the process that
	// computes the window is the process that terminates every agent stream
	// when it restarts or loses the lease, and every reconnect re-reads the
	// bundle from disk. A restart therefore forces the re-read this window is
	// waiting for, which is strictly stronger than waiting it out -- so a
	// shorter flag afterwards only shortens a wait the restart already
	// satisfied.
	if now.Before(stamped.Add(projectionMargin + s.AgentSessionDeadline)) {
		// No second setRotationPhase(PhaseDistributing) needed here either,
		// for the same reason as the gate-check branch above: the hoisted
		// call at the top of this function already covers it.
		return current, true, nil
	}

	fresh, err := current.SwitchToNext(now, s.DNSNames)
	if err != nil {
		return nil, false, err
	}
	err = s.applyStep(ctx, fresh, func(secret *corev1.Secret) {
		setAnnotation(secret, AnnotationRotationPhase, PhaseSwitched)
		// Restamped, not cleared: in this phase it is how long the outgoing
		// CA has been hanging around waiting for a human, which is the one
		// thing worth reading off a rotation that has stopped.
		setAnnotation(secret, AnnotationRotationSince, now.UTC().Format(time.RFC3339))
		delete(secret.Annotations, AnnotationRotationBlockedOn)
	})
	if err != nil {
		return nil, false, err
	}
	setRotationPhase(PhaseSwitched)
	// Re-asserted rather than left from the gate-pass tick: this is the
	// point a blocked gate becomes structurally impossible to return to (the
	// gate never runs again for this rotation), so 0 here is not a guess.
	RotationBlockedNamespaces.Set(0)
	s.event(corev1.EventTypeNormal, ReasonRotationSwitched, actionSwitchRotation,
		"signed the serving certificate with the incoming CA; the outgoing CA stays published until drop-old")
	return fresh, true, nil
}

// applyStep writes one transition. A nil bundle leaves the data alone and
// edits only the annotations.
//
// The Get is inside the retried function, not outside it: on a conflict,
// re-sending the object read before the conflict would re-send the state that
// lost, which here means discarding the annotation the human wrote in the
// meantime -- the exact write this retry exists to preserve.
func (s *Store) applyStep(ctx context.Context, b *Bundle, mutate func(*corev1.Secret)) error {
	key := types.NamespacedName{Name: s.Name, Namespace: s.Namespace}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret := &corev1.Secret{}
		if err := s.Client.Get(ctx, key, secret); err != nil {
			return err
		}
		if b != nil {
			secret.Type = corev1.SecretTypeTLS
			secret.Data = s.secretFor(b).Data
		}
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		mutate(secret)
		return s.Client.Update(ctx, secret)
	})
	if err != nil {
		return fmt.Errorf("update %s: %w", s.Name, err)
	}
	return nil
}

func setAnnotation(secret *corev1.Secret, key, value string) {
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[key] = value
}

func clearRotationAnnotations(secret *corev1.Secret) {
	delete(secret.Annotations, AnnotationRotationPhase)
	delete(secret.Annotations, AnnotationRotationSince)
	delete(secret.Annotations, AnnotationRotationBlockedOn)
}

// blockedOnNote names the namespaces the gate is waiting on, truncated so the
// value stays quotable in an event.
func blockedOnNote(missing []string) string {
	if len(missing) <= maxBlockedNamesInAnnotation {
		return strings.Join(missing, ",")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(missing[:maxBlockedNamesInAnnotation], ","),
		len(missing)-maxBlockedNamesInAnnotation)
}

// namespacesMissingCA returns the namespaces where an agent could be running
// but whose spawnery-ca ConfigMap does not yet carry the given certificate,
// sorted.
//
// The namespaces to check are the union of two sets: those holding a Network,
// and those holding a managed pod that still runs a process.
//
// The Networks are the ordinary answer, and the ConfigMaps are deliberately
// not: the CA ConfigMap carries no owner reference (see
// internal/controller/bootstrap.go) so that it outlives the operator, and a
// Network's ownership of its namespace is the one-per-namespace convention
// (pickNamespaceOwner in internal/controller/network_controller.go), never a
// Kubernetes OwnerReference -- the operator never creates or owns a
// namespace. So a namespace whose Network was deleted keeps whatever
// spawnery-ca it last received, forever, and a gate phrased as "every managed
// CA ConfigMap" would wait on that dead namespace until a human cleaned it up
// by hand: a rotation would never complete on any cluster where a Network had
// ever been deleted.
//
// The pods are the answer to what the Networks alone miss. ServerGroup and
// ProxyGroup carry no OwnerReference to the Network either -- ServerGroupReconciler
// sets group -> Server and nothing sets Network -> group -- so deleting a
// Network leaves the groups and their pods running, and in that namespace
// nothing refreshes the CA ConfigMap any more: NetworkReconciler needs the
// Network, ProxyGroupReconciler returns through refuse before its
// Bootstrap.Ensure, and ServerReconciler reaches Ensure only under its
// createPod guard. Driven from the Networks alone the gate skips such a
// namespace entirely, the window elapses, the switch runs, and every agent
// there fails its next handshake.
//
// Adding the pods is not a retreat from the decision above. A ConfigMap
// outlives everything, which is why one can block a rotation forever; a pod
// does not -- it is deleted, or it finishes, and either way it goes away. And
// a namespace with running agent pods and no Network *should* block the
// rotation and be named in ca-rotation-blocked-on, because that is precisely
// a namespace where the switch would strand somebody: blocking loudly is this
// design's chosen behaviour for "I cannot yet prove this is safe". The gate's
// question was always "where could an agent be running", and the Networks
// alone were an incomplete answer to it.
func (s *Store) namespacesMissingCA(ctx context.Context, caCertPEM []byte) ([]string, error) {
	target, err := fingerprintFirst(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("target certificate: %w", err)
	}

	list := &spawneryv1alpha1.NetworkList{}
	if err := s.Client.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}

	// A namespace can hold at most one Network (the one-per-namespace rule),
	// but nothing here depends on that; distinct namespaces is all this
	// needs, and it is cheap insurance against a namespace briefly holding
	// two while one is on its way out.
	namespaces := make(map[string]struct{}, len(list.Items))
	for i := range list.Items {
		namespaces[list.Items[i].Namespace] = struct{}{}
	}

	// The same label the orphan sweep lists on, and the same cluster-wide
	// pods:list right it already needs -- this adds a call site, not a
	// permission.
	pods := &corev1.PodList{}
	if err := s.Client.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
	}); err != nil {
		return nil, fmt.Errorf("list managed pods: %w", err)
	}
	for i := range pods.Items {
		if podRunsAnAgent(&pods.Items[i]) {
			namespaces[pods.Items[i].Namespace] = struct{}{}
		}
	}

	var missing []string
	for ns := range namespaces {
		ok, err := s.namespaceHasCA(ctx, ns, target)
		if err != nil {
			// A read that failed is not "everything is fine": surface it
			// rather than silently treating the namespace as caught up, or a
			// switch could go ahead while an agent in this namespace never
			// saw the new CA at all.
			return nil, err
		}
		if !ok {
			missing = append(missing, ns)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// podRunsAnAgent reports whether this pod is one the gate has to wait for.
//
// Terminal is the only exclusion, and it is the narrow one on purpose. A
// Succeeded or Failed pod runs no process, holds no agent stream and will
// never open another, so its namespace is not somewhere the switch can strand
// anybody. Everything else counts, including the two cases it would be
// tempting to skip: a Pending pod is about to start and will read the bundle
// it finds, and a Terminating pod is still running its agent until the kubelet
// is done with it. Neither can outlive the rotation the way a ConfigMap can --
// the gate re-reads this every RotationCheckInterval, and both states resolve
// on their own.
//
// Deliberately not internal/controller's podTerminal, which also counts a
// crash-looping pod as finished. That is the right question for a drain and
// the wrong one here: a crash-looping pod has RestartPolicy Always, so it
// comes back, re-reads the CA bundle and needs to find the new CA in it.
func podRunsAnAgent(pod *corev1.Pod) bool {
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// namespaceHasCA reports whether the namespace's spawnery-ca ConfigMap
// already carries target among its (possibly two, while a rotation
// overlaps) certificates.
func (s *Store) namespaceHasCA(ctx context.Context, namespace string, target [sha256.Size]byte) (bool, error) {
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: namespace}
	err := s.Client.Get(ctx, key, cm)
	switch {
	case apierrors.IsNotFound(err):
		// The bootstrapper has not written the ConfigMap in this namespace
		// yet (or it raced with this check). Missing, not an error: the
		// caller's job is to say which namespaces still need the CA, and an
		// absent ConfigMap needs it same as a present-but-stale one.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get configmap %s/%s: %w", namespace, podspec.CAConfigMapName, err)
	}

	rest := []byte(cm.Data[podspec.CAConfigMapKey])
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return false, nil
		}
		// SHA-256 of the DER, not the PEM bytes: a re-encoded PEM with
		// different line wrapping is the same certificate, and comparing PEM
		// text would be a subtler way of getting that wrong.
		if sha256.Sum256(block.Bytes) == target {
			return true, nil
		}
	}
}

// fingerprintFirst decodes the first PEM block and returns the SHA-256 of its
// DER bytes.
func fingerprintFirst(certPEM []byte) ([sha256.Size]byte, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return [sha256.Size]byte{}, fmt.Errorf("certificate is not PEM")
	}
	return sha256.Sum256(block.Bytes), nil
}

