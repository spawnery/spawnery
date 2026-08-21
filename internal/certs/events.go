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
// (docs/superpowers/specs/2026-08-21-ca-rotation-design.md §4); the last two
// name the two ways a request ends without a transition.
//
// ReasonRotationRequestUnrecognised covers a case §4 deliberately does not
// route through ReasonRotationBlocked: a spawnery.cloud/rotate-ca value the
// operator does not recognise -- a typo -- is left in place and reported, but
// does not stop the sequence, which carries on and performs its next
// transition on schedule (rotation.go's AdvanceRotation, the default case).
// Before this event that outcome had exactly one signal, a log line, and the
// human who mistyped the annotation is not the one reading the operator's
// logs. RotationBlocked already means something specific and observable --
// the gate is holding because a namespace has not caught up -- and firing it
// for an unreadable request would tell that human the wrong story: there is
// no gate involved, and the rotation is not waiting on anything but a second,
// correctly spelled annotation.
//
// ReasonRotationRequestRefused is the other one, and it is the likelier of
// the two to be hit under pressure: a request the operator understands
// perfectly and will not carry out from the phase it is in -- a drop-old sent
// a minute early, most of all. §4 has that request consumed like an accepted
// one, so within one tick the annotation is gone and the secret says nothing;
// without this event the whole trace is a log line, and the procedure looks
// to whoever ran it as though it swallowed the instruction. It is a reason of
// its own rather than either of the two above for the same triage argument
// §4 makes about RotationBlocked: nothing is gated on a namespace here, and
// the value was not misspelled -- it was right and its timing was wrong. The
// note carries the refusal's own wording, which names the phase in two of the
// three cases and, in the start-while-rotating case, the requests that would
// end the rotation it is already running. The drop-old-too-early case -- the
// likeliest of all -- says which phase refused it and why, and leaves the
// remedy to the entry in docs/known-issues.md, which is where somebody
// running the procedure is already reading.
const (
	ReasonRotationStarted             = "RotationStarted"
	ReasonRotationBlocked             = "RotationBlocked"
	ReasonRotationSwitched            = "RotationSwitched"
	ReasonRotationCompleted           = "RotationCompleted"
	ReasonRotationRequestUnrecognised = "RotationRequestUnrecognised"
	ReasonRotationRequestRefused      = "RotationRequestRefused"
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
