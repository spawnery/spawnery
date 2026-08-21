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

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

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
	"actionRefuseRotationRequest":     actionRefuseRotationRequest,
	"actionDiscardRotationSlot":       actionDiscardRotationSlot,
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

// TestMainWiresTheRecorderIntoTheStore pins the one line that turns all six
// events on.
//
// Store.Recorder is optional by design -- (*Store).event is a no-op without
// it, which is what lets every fixture in this package build a Store without
// one -- and that is exactly why nothing else can catch the field going
// missing from production. Deleting `Recorder:` from cmd/spawnery-operator's
// certs.Store literal silences every event on the secret, and before this
// test the whole suite stayed green, because every test that asserts an event
// wires its own FakeRecorder in.
//
// A source scan rather than a runtime assertion, because the property is
// about the one construction of a Store that is not a test fixture, and no
// runtime seam distinguishes it: the alternative considered was Provider.Start
// logging once when Recorder is nil, which is a signal in production but goes
// green the moment somebody deletes the field and never runs the operator.
// This goes red on the deletion itself. The AST-scan shape is the one
// internal/controller/events_test.go already uses for its own event
// invariants.
func TestMainWiresTheRecorderIntoTheStore(t *testing.T) {
	path := testenv.RepoPath(t, "cmd/spawnery-operator/main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	literals, wired := 0, 0
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Store" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "certs" {
			return true
		}
		literals++
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Recorder" {
				continue
			}
			// The value, not just the key. `Recorder: nil` satisfies a
			// key-only check and disables every event exactly as deleting
			// the line would -- and nil is the one value a reader might
			// plausibly write here while wiring something else up.
			if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "nil" {
				continue
			}
			wired++
		}
		return true
	})

	// Nought would pass the loop above vacuously, so it is its own failure:
	// the wiring having moved somewhere this scan cannot see is a reason to
	// update this test, not a reason to stop checking.
	if literals == 0 {
		t.Fatalf("no certs.Store literal found in %s; the wiring moved and this pin "+
			"has to follow it, or the six rotation events go unrecorded with nothing noticing", path)
	}
	if wired != literals {
		t.Errorf("%d of %d certs.Store literals in %s set Recorder; without it every "+
			"rotation event is silently dropped, because (*Store).event is a no-op on a "+
			"nil Recorder and every test that asserts an event supplies its own",
			wired, literals, path)
	}
}
