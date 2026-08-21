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
	"fmt"
	"unicode/utf8"
)

// Event actions.
//
// The events API (k8s.io/client-go/tools/events, which replaced the deprecated
// tools/record) takes an `action` alongside `reason`, and events.k8s.io/v1
// validation rejects an event that leaves it empty. The two fields answer
// different questions, and Kubernetes' convention is that `reason` says why an
// event happened while `action` says what the controller was doing when it did.
//
// This repository's reasons are already machine-readable and outcome-shaped --
// ProxyPodBlocked, ForwardingSecretRotated, ReadinessDiverged. Restating one of
// those as the action would add a second copy of the same fact and no new
// information. So `action` here is derived from the operation instead, by one
// rule applied at every call site:
//
//  1. If the event reports on the controller's attempt to mutate a subordinate
//     object -- create it, delete it, adopt it, ask it to retire -- the action
//     is <Verb><Kind> naming that attempt. Success and failure of the same
//     attempt share one action and are told apart by `reason`, which is the
//     whole point of having both fields. The r.Create three lines above the
//     event is what names it.
//  2. Otherwise the controller mutated nothing and was computing and writing
//     the reconciled object's own status. The action is actionSyncStatus. Every
//     condition flip and every phase transition below is this case.
//
// Adding an event means choosing from this list, not inventing a new string. If
// nothing here fits, the operation is new and belongs here first.
//
// Note what rule 1 buys, at the two sites that both report reason
// ProxyPodBlocked: ProxyGroupReconciler.Reconcile fires it when the API server
// refuses a pod it tried to create (actionCreateProxyPod), and
// reportBlockedProxies fires it when an already-created pod cannot be scheduled
// (actionSyncStatus). Same reason, different operation -- and now the event
// says which.
const (
	// actionAdoptPod is ServerReconciler deciding whether a pod that already
	// holds the name it wants is one it may take charge of.
	actionAdoptPod = "AdoptPod"
	// actionCreatePod is ServerReconciler creating a game server's pod, from
	// the namespace bootstrap that gates it through to the create itself.
	actionCreatePod = "CreatePod"
	// actionDeletePod is ServerReconciler deleting a game server's pod.
	actionDeletePod = "DeletePod"

	// actionCreateProxyPod is ProxyGroupReconciler creating a proxy pod.
	actionCreateProxyPod = "CreateProxyPod"
	// actionDrainProxy is ProxyGroupReconciler taking a proxy pod out of
	// service, whether because its node is going away or because the drain ran
	// out of time.
	actionDrainProxy = "DrainProxy"

	// actionCreateServer is ServerGroupReconciler creating a member Server,
	// ephemeral or persistent.
	actionCreateServer = "CreateServer"
	// actionDeleteServer is ServerGroupReconciler deleting a member Server.
	actionDeleteServer = "DeleteServer"
	// actionRetireServer is ServerGroupReconciler asking a member Server into
	// soft drain for a rolling update.
	actionRetireServer = "RetireServer"

	// actionBootstrapNamespace is NetworkReconciler putting the CA bundle and
	// the agent ServiceAccounts into its namespace. Rule 1: the objects
	// Bootstrapper.Ensure writes are subordinate ones, and this names the
	// attempt to write them. ServerReconciler reports the same failure under
	// actionCreatePod because there the bootstrap only gates the create it is
	// named after; here nothing follows it, so the bootstrap is the operation.
	actionBootstrapNamespace = "BootstrapNamespace"

	// actionSyncStatus is rule 2: the controller changed nothing but its own
	// object's status, and the event reports what it observed while doing so.
	actionSyncStatus = "SyncStatus"
)

// A note on the `related` argument, which is nil at every call site in this
// package.
//
// The events API's `related` names a secondary object for events about two of
// them at once. Every event this operator emits regards exactly one object, and
// the tools/record call sites this package migrated from had no way to carry a
// second one. Passing the created Server or the deleted Pod there would be new
// information in the operator's output that no test covers and no design asked
// for, so the migration leaves it nil throughout and says why once, here,
// rather than at twenty-three call sites.

// The event note's size limit, which is not the same limit the old API had.
//
// events.k8s.io/v1 validation rejects an event whose note exceeds 1024 bytes;
// the core v1 Event the deprecated tools/record wrote had no such cap. Nothing
// between a call site and the API server enforces it: the recorder does
// `fmt.Sprintf(note, args...)` and assigns the result straight to Note
// (k8s.io/client-go/tools/events/event_recorder.go), and when the API server
// then refuses the create, the broadcaster classifies a *errors.StatusError as
// non-retryable and abandons the event with a klog line. The operator loses the
// event silently -- and loses it precisely when the text was long, which for an
// API server's own error message means precisely when it had the most to say.
//
// A note built entirely from compile-time literals cannot reach the limit and
// needs nothing. A note built from text this operator did not write -- an
// admission webhook's refusal, a scheduler's explanation, a list that grows
// with the group -- must go through eventNote below.
//
// The limit is counted in bytes even though the API server's own message says
// "can have at most 1024 characters". Probed against envtest's real API server:
// 1024 ASCII bytes is accepted, 1025 is refused, and 512 em-dashes -- 512
// characters, 1536 bytes -- is refused too.
const maxEventNote = 1024

// eventNoteTruncated marks a cut note and says where the whole one is.
//
// The marker is not decoration. A reader who sees an API server's explanation
// stop mid-sentence cannot otherwise tell whether the API server phrased it that
// way or whether this operator cut it, and the difference matters when the
// remedy is in the wording. So the cut announces itself, and it points at the
// copy that is not cut: the same text is on the object's status conditions,
// which allow 32 KB and are written from the same values.
const eventNoteTruncated = " ... (truncated; the full text is on the object's status conditions)"

// eventNote formats an event note and guarantees the result fits maxEventNote.
//
// Call sites pass the result as the whole note -- `Eventf(..., "%s",
// eventNote(...))` -- rather than passing a format and letting the recorder
// expand it. That is the only shape that can promise anything: the limit
// applies to the formatted string, so a helper that saw only the format and the
// arguments would be guessing at the length of what it had not yet built.
//
// A note already within the limit is returned byte for byte unchanged.
func eventNote(format string, args ...any) string {
	note := fmt.Sprintf(format, args...)
	if len(note) <= maxEventNote {
		return note
	}
	keep := maxEventNote - len(eventNoteTruncated)
	// Back off to a rune boundary. Cutting a multi-byte rune in half would
	// put invalid UTF-8 in the note, which the API server rejects for its own
	// reasons -- trading a too-long event for an invalid one is no fix.
	for keep > 0 && !utf8.RuneStart(note[keep]) {
		keep--
	}
	return note[:keep] + eventNoteTruncated
}
