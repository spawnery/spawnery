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
	"fmt"
	"unicode/utf8"
)

// Event actions. `reason` says what happened; `action` names the operation:
// <Verb><Kind> when the controller tried to mutate a subordinate object (success
// and failure share one action), otherwise actionSyncStatus.
const (
	actionAdoptPod  = "AdoptPod"
	actionCreatePod = "CreatePod"
	actionDeletePod = "DeletePod"

	actionCreateProxyPod = "CreateProxyPod"
	actionDrainProxy     = "DrainProxy"

	actionCreateServer = "CreateServer"
	actionDeleteServer = "DeleteServer"
	// actionRetireServer asks a member Server into soft drain for a rolling update.
	actionRetireServer = "RetireServer"

	// ServerReconciler reports a failed bootstrap under actionCreatePod, since
	// there the bootstrap only gates the create.
	actionBootstrapNamespace = "BootstrapNamespace"

	actionSyncStatus = "SyncStatus"
)

// events.k8s.io/v1 rejects a note over 1024 bytes (bytes, not characters as
// its message claims), and the recorder then drops the event silently. Notes
// carrying text this operator did not write must go through eventNote.
const maxEventNote = 1024

const eventNoteTruncated = " ... (truncated; the full text is on the object's status conditions)"

// eventNote is passed as the whole note (`Eventf(..., "%s", eventNote(...))`)
// because the limit applies to the formatted string.
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
