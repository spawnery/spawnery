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

// White-box (package certs, like bundle_test.go and rotation_envtest_test.go):
// the action constants this file checks are unexported.
package certs

import "testing"

// certsActions is every action constant events.go declares, keyed by the
// identifier the call sites in rotation.go spell -- the same shape
// internal/controller/events_test.go's knownActions uses, kept here because
// that test's AST scan does not reach this package (it walks
// filepath.WalkDir(".", ...) from internal/controller, its own directory, so
// its corpus never includes internal/certs).
var certsActions = map[string]string{
	"actionStartRotation":             actionStartRotation,
	"actionBlockRotation":             actionBlockRotation,
	"actionSwitchRotation":            actionSwitchRotation,
	"actionCompleteRotation":          actionCompleteRotation,
	"actionReportUnrecognisedRequest": actionReportUnrecognisedRequest,
}

// TestNoCertsActionConstantIsEmpty is this package's copy of the one check
// internal/controller/events_test.go opens with: events.k8s.io/v1 rejects an
// event whose action is empty, and a constant that lost its value would
// compile just as well as one that kept it -- nothing else here would notice.
func TestNoCertsActionConstantIsEmpty(t *testing.T) {
	for name, value := range certsActions {
		if value == "" {
			t.Errorf("%s is empty; events.k8s.io/v1 refuses an event with no action", name)
		}
	}
}

// TestStoreEventIsANoOpWithNoRecorder pins the safety property that makes it
// fine for the rest of this package's tests to build a Store with no
// Recorder at all: every event call site in rotation.go goes through
// (*Store).event, and this is the one place that guarantees it never panics
// on a nil Recorder rather than trusting every call site to remember a nil
// check.
func TestStoreEventIsANoOpWithNoRecorder(t *testing.T) {
	s := &Store{Name: SecretName, Namespace: "spawnery-system"}
	s.event("Normal", ReasonRotationStarted, actionStartRotation, "no recorder is wired in")
}
