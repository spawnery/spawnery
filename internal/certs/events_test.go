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
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

// certsActions mirrors internal/controller/events_test.go's knownActions,
// whose scan does not reach this package.
var certsActions = map[string]string{
	"actionStartRotation":             actionStartRotation,
	"actionBlockRotation":             actionBlockRotation,
	"actionSwitchRotation":            actionSwitchRotation,
	"actionCompleteRotation":          actionCompleteRotation,
	"actionReportUnrecognisedRequest": actionReportUnrecognisedRequest,
	"actionRefuseRotationRequest":     actionRefuseRotationRequest,
	"actionDiscardRotationSlot":       actionDiscardRotationSlot,
	"actionTruncateRotationSlot":      actionTruncateRotationSlot,
}

// events.k8s.io/v1 rejects an event whose action is empty.
func TestNoCertsActionConstantIsEmpty(t *testing.T) {
	for name, value := range certsActions {
		if value == "" {
			t.Errorf("%s is empty; events.k8s.io/v1 refuses an event with no action", name)
		}
	}
}

func TestStoreEventIsANoOpWithNoRecorder(t *testing.T) {
	s := &Store{Name: SecretName, Namespace: "spawnery-system"}
	s.event("Normal", ReasonRotationStarted, actionStartRotation, "no recorder is wired in")
}

// Store.Recorder is optional and every test wires its own, so only a source
// scan notices it missing from cmd/spawnery-operator's certs.Store literal.
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
			if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "nil" {
				continue
			}
			wired++
		}
		return true
	})

	if literals == 0 {
		t.Fatalf("no certs.Store literal found in %s; the wiring moved and this pin "+
			"has to follow it, or every rotation event goes unrecorded with nothing noticing", path)
	}
	if wired != literals {
		t.Errorf("%d of %d certs.Store literals in %s set Recorder; without it every "+
			"rotation event is silently dropped, because (*Store).event is a no-op on a "+
			"nil Recorder and every test that asserts an event supplies its own",
			wired, literals, path)
	}
}
