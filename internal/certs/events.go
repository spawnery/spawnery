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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Event reasons recorded on the TLS secret, so that a rotation is visible
// without reading logs. The first four are the design's own vocabulary
// (docs/superpowers/specs/2026-08-21-ca-rotation-design.md §4); the next two
// name the two ways a request ends without a transition, and the last two the
// two things the operator does to a slot nobody asked it to touch.
//
// The four that are not the design's vocabulary are each a separate reason
// rather than a note on one of the others, because the reason is the field a
// human triages on:
//
// ReasonRotationRequestUnrecognised is a misspelled rotate-ca value. It is
// left in place and reported, and the sequence carries on to its next
// scheduled transition (AdvanceRotation's default case). Not RotationBlocked,
// which means a gate is holding because a namespace has not caught up: there
// is no gate here, and nothing is waited on but a correctly spelled
// annotation.
//
// ReasonRotationRequestRefused is a request the operator understands and will
// not carry out from the phase it is in -- a drop-old sent a minute early,
// most of all. Such a request is consumed like an accepted one, so within a
// tick the annotation is gone and the secret would otherwise say nothing,
// leaving the procedure looking as though it swallowed the instruction. The
// note carries the refusal's own wording; the remedy is in
// docs/ca-rotation.md, which whoever ran the procedure is already reading.
//
// ReasonRotationSlotDiscarded is the only one that reports the operator
// undoing part of a rotation on its own: a slot whose certificate does not
// parse is cleared, because every byte of it reaches every agent's trust
// store through PublishedCA, where a five-hyphen run that opens no valid
// block makes CertificateFactory.generateCertificates throw for the whole
// stream -- the signing CA included. A hand-edited slot is not a request, so
// neither of the two above fits, and no gate is holding. The durable copy is
// AnnotationRotationDiscarded, because an event expires after about an hour.
//
// ReasonRotationSlotTruncated is the one parse failure the operator repairs
// instead of discarding: a slot holding more than one PEM block is truncated
// to the first, which is the block parseCA already signs with, so nothing
// usable is lost and no phase moves. A discard reason that had to be read to
// the end before one could tell nothing was discarded would train a reader to
// distrust it on the ones that mean what they say. The durable record shares
// AnnotationRotationDiscarded, whose own wording distinguishes the two.
const (
	ReasonRotationStarted             = "RotationStarted"
	ReasonRotationBlocked             = "RotationBlocked"
	ReasonRotationSwitched            = "RotationSwitched"
	ReasonRotationCompleted           = "RotationCompleted"
	ReasonRotationRequestUnrecognised = "RotationRequestUnrecognised"
	ReasonRotationRequestRefused      = "RotationRequestRefused"
	ReasonRotationSlotDiscarded       = "RotationSlotDiscarded"
	ReasonRotationSlotTruncated       = "RotationSlotTruncated"
)

// Event actions, in the shape internal/controller/events.go established:
// events.k8s.io/v1 rejects an event whose action is empty, and this package
// keeps its own vocabulary rather than reusing that file's -- these regard
// the TLS secret itself, not a subordinate object a controller created.
//
// internal/controller/events_test.go's AST scan does not check these: it
// walks filepath.WalkDir(".", ...) from its own package directory, so its
// corpus is internal/controller's sources and nothing under internal/certs.
// TestNoCertsActionConstantIsEmpty in events_test.go is this package's own
// copy of the one check that scan opens with.
const (
	actionStartRotation             = "StartRotation"
	actionBlockRotation             = "BlockRotation"
	actionSwitchRotation            = "SwitchRotation"
	actionCompleteRotation          = "CompleteRotation"
	actionReportUnrecognisedRequest = "ReportUnrecognisedRequest"
	actionRefuseRotationRequest     = "RefuseRotationRequest"
	actionDiscardRotationSlot       = "DiscardRotationSlot"
	actionTruncateRotationSlot      = "TruncateRotationSlot"
)

// event records an event on the TLS secret, or does nothing if no recorder is
// wired in.
//
// Store is built directly with no Recorder throughout this package's own
// tests (newStore in store_envtest_test.go, and every white-box test in this
// package) -- only main.go's wiring, mgr.GetEventRecorder("certs"), ever sets
// one. Guarding here, once, is what keeps every other construction of a Store
// safe without requiring each of them to carry a no-op recorder just to avoid
// a nil pointer dereference on Eventf.
//
// The object passed as "regarding" needs only its Namespace and Name:
// client-go's tools/events recorder resolves Kind and APIVersion itself,
// through the manager's scheme (reference.GetReference), and does not require
// a UID. Building it fresh here rather than threading the *corev1.Secret each
// caller already read avoids passing a stale one across a retry inside
// applyStep -- the identity is all this needs, and the identity does not
// change between retries.
func (s *Store) event(eventtype, reason, action, note string, args ...any) {
	if s.Recorder == nil {
		return
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace}}
	s.Recorder.Eventf(secret, nil, eventtype, reason, action, note, args...)
}
