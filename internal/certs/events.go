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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Event reasons recorded on the TLS secret. The first four are the design's
// vocabulary (docs/superpowers/specs/2026-08-21-ca-rotation-design.md §4).
//
// ReasonRotationRequestUnrecognised: a misspelled rotate-ca value; left in
// place, and the sequence carries on.
//
// ReasonRotationRequestRefused: a request understood but not allowed from the
// current phase. It is consumed like an accepted one, so without this event
// the secret would say nothing.
//
// ReasonRotationSlotDiscarded: a slot whose certificate does not parse is
// cleared, because PublishedCA ships every byte of it to the agents, whose
// CertificateFactory.generateCertificates throws for the whole stream on one
// bad block. The durable copy is AnnotationRotationDiscarded; events expire.
//
// ReasonRotationSlotTruncated: a slot holding more than one PEM block is cut
// to the first, the one parseCA signs with, so nothing usable is lost.
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

// Event actions: events.k8s.io/v1 rejects an event with an empty action.
// internal/controller/events_test.go does not scan this package;
// TestNoCertsActionConstantIsEmpty is its stand-in here.
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

// event is a no-op without a Recorder, which only main.go's wiring sets.
// The recorder resolves Kind and APIVersion itself and needs no UID, so a
// Secret carrying only Namespace and Name is enough.
func (s *Store) event(eventtype, reason, action, note string, args ...any) {
	if s.Recorder == nil {
		return
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace}}
	s.Recorder.Eventf(secret, nil, eventtype, reason, action, note, args...)
}
