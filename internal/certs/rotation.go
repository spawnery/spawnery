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
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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
			consume(secret)
		})
		if err != nil {
			return nil, false, err
		}
		setRotationPhase(PhaseDistributing)
		// RotationBlockedNamespaces is left alone here rather than zeroed:
		// every namespace with a Network is missing this brand new CA at the
		// instant it is minted, so 0 would claim the opposite of the truth.
		// The gate runs on the very next tick (drivePhase, since == "") and
		// sets it to the real count within RotationCheckInterval.
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
func (s *Store) refuse(ctx context.Context, consume func(*corev1.Secret), reason error) (*Bundle, bool, error) {
	if err := s.applyStep(ctx, nil, consume); err != nil {
		return nil, false, fmt.Errorf("%w (and clearing it failed: %v)", reason, err)
	}
	return nil, false, reason
}

// drivePhase is what happens on a tick with no request pending: the gate, the
// window, and then the switch. The `switched` phase deliberately has no
// transition out of it -- the operator holds there until a human asks.
func (s *Store) drivePhase(ctx context.Context, current *Bundle, phase, since, blockedOn string) (*Bundle, bool, error) {
	if phase != PhaseDistributing {
		// Set on every tick, not only on the ones that change it: this
		// package's Provider.Start calls in here at most once an hour while
		// idle (RenewCheckInterval), and that tick is what re-populates the
		// gauge after a restart -- the Prometheus client library starts a
		// GaugeVec's series unset until the first Set.
		setRotationPhase(phase)
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
		RotationBlockedNamespaces.Set(float64(len(missing)))
		setRotationPhase(PhaseDistributing)
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
		setRotationPhase(PhaseDistributing)
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

// namespacesMissingCA returns the namespaces that hold a Network but whose
// spawnery-ca ConfigMap does not yet carry the given certificate, sorted.
//
// The namespaces to check come from the Network objects, not from the CA
// ConfigMaps. Those are different sets: the ConfigMap deliberately carries no
// owner reference (see internal/controller/bootstrap.go) so that it outlives
// the operator, and a Network's ownership of its namespace is the
// one-per-namespace convention (pickNamespaceOwner in
// internal/controller/network_controller.go), never a Kubernetes
// OwnerReference -- the operator never creates or owns a namespace. So a
// namespace whose Network was deleted keeps whatever spawnery-ca it last
// received, forever. Listing ConfigMaps instead of Networks would make the
// gate wait on that dead namespace until a human cleaned it up by hand, and a
// rotation would never complete on any cluster where a Network had ever been
// deleted.
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
