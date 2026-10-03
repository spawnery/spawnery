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

// The annotations are the whole operator interface to a rotation. They live
// on the TLS secret, which already holds the rotation's state, so a restart
// can resume from it alone.
const (
	AnnotationRotateRequest     = "spawnery.cloud/rotate-ca"
	AnnotationRotationPhase     = "spawnery.cloud/ca-rotation-phase"
	AnnotationRotationSince     = "spawnery.cloud/ca-rotation-since"
	AnnotationRotationBlockedOn = "spawnery.cloud/ca-rotation-blocked-on"

	// AnnotationRotationDiscarded records which slot failed to parse, why,
	// whether it was cleared or truncated, and when. The discarded bytes are
	// not kept, and the Warning event expires, so this is all a diagnosis has.
	AnnotationRotationDiscarded = "spawnery.cloud/ca-rotation-discarded"

	RequestStart    = "start"
	RequestDropOld  = "drop-old"
	RequestRollback = "rollback"

	PhaseDistributing = "distributing"
	PhaseSwitched     = "switched"
)

// RotationCheckInterval applies while a rotation is in flight; the window is
// a quarter of an hour.
const RotationCheckInterval = 30 * time.Second

// projectionMargin covers the kubelet projecting the CA ConfigMap into pods;
// its --sync-frequency defaults to one minute. A cluster with a much longer
// sync frequency would make it too short.
const projectionMargin = 2 * time.Minute

// maxBlockedNamesInAnnotation: ten 63-character namespace names stay well
// below an event note's 1024 bytes.
const maxBlockedNamesInAnnotation = 10

// Cluster-wide: the gate must see every namespace an agent could run in.
// Kept out of any doc comment, where controller-gen ignores markers.
// +kubebuilder:rbac:groups="",resources=pods,verbs=list

// AdvanceRotation performs at most one step of a rotation and returns the
// bundle to publish and whether a rotation is still in flight.
//
// One step per call, so the switch is never in the same call as the gate that
// opened its window, and a partial write leaves a phase that names the
// transition in progress.
//
// On error the caller keeps publishing what Ensure gave it.
func (s *Store) AdvanceRotation(ctx context.Context, current *Bundle) (*Bundle, bool, error) {
	// Read fresh: a human may have set the request a second ago.
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: s.Name, Namespace: s.Namespace}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		return nil, false, fmt.Errorf("get %s: %w", s.Name, err)
	}

	phase := secret.Annotations[AnnotationRotationPhase]

	// Before any request: an unparsable slot is already reaching every agent's
	// trust store. A pending request waits for the next tick.
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
		// Left in place so the typo stays visible, but stepped over: halting
		// would freeze a rotation mid-window, while continuing costs at most
		// the switch, which a rollback undoes.
		log.FromContext(ctx).Info("ignoring an unrecognised CA rotation request",
			"annotation", AnnotationRotateRequest, "value", request,
			"expected", strings.Join([]string{RequestStart, RequestDropOld, RequestRollback}, ", "))
		// %.150s counts runes, so the cut lands on a rune boundary and the note
		// stays below 1024 bytes.
		s.event(corev1.EventTypeWarning, ReasonRotationRequestUnrecognised, actionReportUnrecognisedRequest,
			"%s=%.150s is not %s, %s or %s; left in place and stepped over rather than halting the rotation",
			AnnotationRotateRequest, request, RequestStart, RequestDropOld, RequestRollback)
	}
	return s.drivePhase(ctx, current, phase, secret.Annotations[AnnotationRotationSince],
		secret.Annotations[AnnotationRotationBlockedOn])
}

// repairOrDiscardSlots puts every rotation slot an agent could not parse back
// into a state it can, records what it did, and ends the rotation when a slot
// it had to throw away is the one the current phase depends on. A nil bundle
// means nothing was wrong.
//
// A slot whose first PEM block is a certificate is truncated to it: parseCA
// already signs with that block, so nothing is lost. Otherwise parseCA fails
// too, and the slot is cleared.
//
// A cleared ca-next while distributing abandons the rotation; a cleared
// ca-previous while switched completes the drop. The rollback that hold
// existed for was already impossible, because it signs with these bytes. A
// slot the current phase does not depend on is cleared without moving it.
//
// A repaired slot takes both halves from the secret, not from current, which
// may predate the hand-edit: pairing the new certificate with an old key
// would store a slot that cannot sign, since applyStep rewrites all of Data.
func (s *Store) repairOrDiscardSlots(ctx context.Context, current *Bundle, phase string, stored *corev1.Secret) (*Bundle, bool, error) {
	slots := []struct {
		certKey        string
		keyKey         string
		dependentPhase string
		clear          func(*Bundle)
		setPair        func(*Bundle, []byte, []byte)
	}{
		// Only the certificates are judged, as only they are published. A bad
		// key fails loudly in parseCA.
		{keyNextCACert, keyNextCAKey, PhaseDistributing,
			func(b *Bundle) { b.NextCACertPEM, b.NextCAKeyPEM = nil, nil },
			func(b *Bundle, cert, key []byte) { b.NextCACertPEM, b.NextCAKeyPEM = cert, key }},
		{keyPreviousCACert, keyPreviousCAKey, PhaseSwitched,
			func(b *Bundle) { b.PreviousCACertPEM, b.PreviousCAKeyPEM = nil, nil },
			func(b *Bundle, cert, key []byte) { b.PreviousCACertPEM, b.PreviousCAKeyPEM = cert, key }},
	}

	// One entry per slot touched: two broken slots give two records, and the
	// outcome clause belongs only to the slot it is about.
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
	restartWindow := false
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
			// Accepted edge: a hand edit that also deletes the key stores an empty
			// key, which parseCA refuses loudly on the next transition.
			slot.setPair(&fresh, firstPEMBlock(certPEM), stored.Data[slot.keyKey])
			// The gate ran against other bytes; if the kept block is a different
			// CA than the one distributed, switching would strand every agent.
			// So the window restarts and the gate re-runs.
			restartWindow = restartWindow ||
				(slot.certKey == keyNextCACert && phase == PhaseDistributing)
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
	// Written in the same applyStep as the change, so neither lands alone.
	record := fmt.Sprintf("%s (%s)", strings.Join(records, "; "),
		s.Clock().UTC().Format(time.RFC3339))
	err := s.applyStep(ctx, &fresh, func(secret *corev1.Secret) {
		setAnnotation(secret, AnnotationRotationDiscarded, record)
		if restartWindow {
			delete(secret.Annotations, AnnotationRotationSince)
		}
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
		RotationBlockedNamespaces.Set(0)
	}
	setRotationPhase(remaining)

	// After the write: a failed applyStep leaves every slot where it was.
	for _, c := range changes {
		if c.repaired {
			s.event(corev1.EventTypeWarning, ReasonRotationSlotTruncated, actionTruncateRotationSlot,
				"%s held more than its first PEM block and has been truncated to that block, "+
					"which is the one the operator already signs with: nothing usable was "+
					"lost and the slot stays where it is", c.certKey)
			continue
		}
		// %.150s bounds x509's wording, which embeds the bytes it choked on;
		// at most 803 bytes against the note's 1024.
		s.event(corev1.EventTypeWarning, ReasonRotationSlotDiscarded, actionDiscardRotationSlot,
			"%s could not be parsed and has been cleared from the secret: %.150s; %s",
			c.certKey, c.reason, c.outcome)
	}
	return &fresh, remaining != "", nil
}

// discardOutcome is what the event says became of the rotation, for one
// cleared slot.
func discardOutcome(phase string, dependent bool) string {
	switch {
	case !dependent:
		// Silent on the sequence: the other slot may have ended it in this call.
		return "no rotation depended on this slot, so nothing about the sequence turns on it"
	case phase == PhaseSwitched:
		return "the drop was completed: a rollback would have signed with these bytes, " +
			"so the hold at switched was already impossible to act on"
	default:
		return "the rotation was abandoned, and ca.crt is published alone -- " +
			"nothing usable was ever distributed"
	}
}

// applyRequest performs the step a human asked for, or refuses it. A refused
// request is consumed like an accepted one: left in place, it would keep
// drivePhase from ever running and freeze the sequence.
func (s *Store) applyRequest(ctx context.Context, current *Bundle, request, phase string) (*Bundle, bool, error) {
	now := s.Clock()
	consume := func(secret *corev1.Secret) {
		// A human may have replaced it since the read.
		if secret.Annotations[AnnotationRotateRequest] == request {
			delete(secret.Annotations, AnnotationRotateRequest)
		}
	}

	switch request {
	case RequestStart:
		if phase != "" {
			// The bundle has one spare slot, and while switched it holds the
			// CA a rollback signs with.
			return s.refuse(ctx, consume, fmt.Errorf(
				"%s=%s refused: a rotation is already in the %q phase; finish it with %s=%s or %s=%s first",
				AnnotationRotateRequest, RequestStart, phase,
				AnnotationRotateRequest, RequestDropOld, AnnotationRotateRequest, RequestRollback))
		}
		certPEM, keyPEM, err := IssueCA(now)
		if err != nil {
			return nil, false, err
		}
		fresh := current.WithNextCA(certPEM, keyPEM)
		err = s.applyStep(ctx, fresh, func(secret *corev1.Secret) {
			setAnnotation(secret, AnnotationRotationPhase, PhaseDistributing)
			delete(secret.Annotations, AnnotationRotationSince)
			delete(secret.Annotations, AnnotationRotationBlockedOn)
			// A discard record beside a running phase would read as about it.
			delete(secret.Annotations, AnnotationRotationDiscarded)
			consume(secret)
		})
		if err != nil {
			return nil, false, err
		}
		setRotationPhase(PhaseDistributing)
		// RotationBlockedNamespaces already reads 0 with phase == "".
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
			// Nothing was signed with the incoming CA yet.
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
		// No event: the design's vocabulary (§4) has no RotationRolledBack.
		setRotationPhase(phaseNone)
		RotationBlockedNamespaces.Set(0)
		return fresh, false, nil
	}

	return nil, false, fmt.Errorf("applyRequest called with an unrecognised %s=%q",
		AnnotationRotateRequest, request)
}

// refuse consumes the request and reports why it was not carried out. The
// event is the only trace on the secret once the annotation is gone.
func (s *Store) refuse(ctx context.Context, consume func(*corev1.Secret), reason error) (*Bundle, bool, error) {
	// Before the consume: a failed consume is when the event matters most.
	// %.230s: reason embeds the hand-editable phase annotation; 230 runes plus
	// the 74-byte suffix stay within the note's 1024 bytes.
	s.event(corev1.EventTypeWarning, ReasonRotationRequestRefused, actionRefuseRotationRequest,
		"%.230s; the request was consumed, so setting it again is what asks a second time",
		reason.Error())
	if err := s.applyStep(ctx, nil, consume); err != nil {
		return nil, false, fmt.Errorf("%w (and clearing it failed: %v)", reason, err)
	}
	return nil, false, reason
}

// drivePhase is what happens on a tick with no request pending: the gate, the
// window, and then the switch. `switched` has no transition out of it; the
// operator holds there until a human asks.
func (s *Store) drivePhase(ctx context.Context, current *Bundle, phase, since, blockedOn string) (*Bundle, bool, error) {
	// Above the guards, so a restart into a broken `distributing` state still
	// exports the phase.
	setRotationPhase(phase)
	if phase != PhaseDistributing {
		return current, phase == PhaseSwitched, nil
	}
	if len(current.NextCACertPEM) == 0 {
		return nil, false, fmt.Errorf("the secret says %s=%s but holds no incoming CA",
			AnnotationRotationPhase, PhaseDistributing)
	}
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
		RotationBlockedNamespaces.Set(float64(len(missing)))
		if len(missing) > 0 {
			note := blockedOnNote(missing)
			if note != blockedOn {
				// Only on change, for both the write and the event.
				if err := s.applyStep(ctx, nil, func(secret *corev1.Secret) {
					setAnnotation(secret, AnnotationRotationBlockedOn, note)
				}); err != nil {
					return nil, false, err
				}
				s.event(corev1.EventTypeWarning, ReasonRotationBlocked, actionBlockRotation, "%s", note)
			}
			return current, true, nil
		}
		// The window counts from the last namespace receiving the CA.
		err = s.applyStep(ctx, nil, func(secret *corev1.Secret) {
			setAnnotation(secret, AnnotationRotationSince, now.UTC().Format(time.RFC3339))
			delete(secret.Annotations, AnnotationRotationBlockedOn)
		})
		if err != nil {
			return nil, false, err
		}
		return current, true, nil
	}

	// No second run of the gate: a Network created after `since` gets both
	// CAs from its first reconcile, and re-running it would let regular
	// Network creation postpone the switch forever.
	stamped, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s=%q: %w", AnnotationRotationSince, since, err)
	}
	// The current flag suffices: the agent endpoint is leader-bound on this
	// manager, so a restart already closed every stream and forced the
	// re-read the window waits for.
	if now.Before(stamped.Add(projectionMargin + s.AgentSessionDeadline)) {
		return current, true, nil
	}

	fresh, err := current.SwitchToNext(now, s.DNSNames)
	if err != nil {
		return nil, false, err
	}
	err = s.applyStep(ctx, fresh, func(secret *corev1.Secret) {
		setAnnotation(secret, AnnotationRotationPhase, PhaseSwitched)
		// Restamped: now it says how long the outgoing CA has waited for a
		// human.
		setAnnotation(secret, AnnotationRotationSince, now.UTC().Format(time.RFC3339))
		delete(secret.Annotations, AnnotationRotationBlockedOn)
	})
	if err != nil {
		return nil, false, err
	}
	setRotationPhase(PhaseSwitched)
	// The gate never runs again for this rotation.
	RotationBlockedNamespaces.Set(0)
	s.event(corev1.EventTypeNormal, ReasonRotationSwitched, actionSwitchRotation,
		"signed the serving certificate with the incoming CA; the outgoing CA stays published until drop-old")
	return fresh, true, nil
}

// applyStep writes one transition. A nil bundle leaves the data alone and
// edits only the annotations.
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

func blockedOnNote(missing []string) string {
	if len(missing) <= maxBlockedNamesInAnnotation {
		return strings.Join(missing, ",")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(missing[:maxBlockedNamesInAnnotation], ","),
		len(missing)-maxBlockedNamesInAnnotation)
}

// namespacesMissingCA returns, sorted, the namespaces where an agent could be
// running whose spawnery-ca ConfigMap lacks the certificate: those holding a
// Network, plus those holding a managed pod that still runs.
//
// Not the CA ConfigMaps themselves: they have no owner and outlive a deleted
// Network forever, so they would block every rotation. Not the Networks
// alone: nothing owns groups from a Network, so its pods survive its
// deletion with nobody refreshing their ConfigMap, and the switch would
// strand them. Such a namespace should block, loudly, until its pods go.
func (s *Store) namespacesMissingCA(ctx context.Context, caCertPEM []byte) ([]string, error) {
	target, err := fingerprintFirst(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("target certificate: %w", err)
	}

	list := &spawneryv1alpha1.NetworkList{}
	if err := s.Client.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}

	namespaces := make(map[string]struct{}, len(list.Items))
	for i := range list.Items {
		namespaces[list.Items[i].Namespace] = struct{}{}
	}

	// Uses the pods:list right the orphan sweep already holds.
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
			return nil, err
		}
		if !ok {
			missing = append(missing, ns)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// podRunsAnAgent excludes only terminal pods. Pending and Terminating pods
// still read or hold the bundle.
//
// Not internal/controller's podTerminal, which counts a crash-looping pod as
// finished: it restarts and must find the new CA.
func podRunsAnAgent(pod *corev1.Pod) bool {
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// namespaceHasCA reports whether the namespace's spawnery-ca ConfigMap
// carries target among its certificates.
func (s *Store) namespaceHasCA(ctx context.Context, namespace string, target [sha256.Size]byte) (bool, error) {
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: podspec.CAConfigMapName, Namespace: namespace}
	err := s.Client.Get(ctx, key, cm)
	switch {
	case apierrors.IsNotFound(err):
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
		// DER, not PEM: re-wrapped PEM is the same certificate.
		if sha256.Sum256(block.Bytes) == target {
			return true, nil
		}
	}
}

func fingerprintFirst(certPEM []byte) ([sha256.Size]byte, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return [sha256.Size]byte{}, fmt.Errorf("certificate is not PEM")
	}
	return sha256.Sum256(block.Bytes), nil
}
