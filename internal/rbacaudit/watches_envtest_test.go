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

package rbacaudit_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/spawnery/spawnery/internal/rbacaudit"
	"github.com/spawnery/spawnery/internal/testenv"
)

// An informer without list and watch on its kind retries silently forever.
var watchBuilders = map[string]bool{"For": true, "Owns": true, "Watches": true}

type watchedKind struct {
	pkgPath string
	name    string
	where   string
}

// Other verbs are covered: controller tests run under the operator's
// restricted ClusterRole, which the audit holds against the table. No test
// starts an informer under that identity, and a forbidden watch is silent,
// so this scans For/Owns/Watches literals. It does not see a watch from a
// raw source or built through a variable; none exists today.
func TestEveryWatchedKindIsInTheRequiredTable(t *testing.T) {
	watched := watchedKindsIn(t, testenv.RepoPath(t, filepath.Join("internal", "controller")))
	if len(watched) == 0 {
		t.Fatal("no For/Owns/Watches call was found at all; this test would pass without checking anything")
	}

	c, _ := testenv.Client(t)
	scheme := testenv.Scheme(t)

	byType := map[watchedKind]schema.GroupVersionKind{}
	for gvk, rt := range scheme.AllKnownTypes() {
		byType[watchedKind{pkgPath: rt.PkgPath(), name: rt.Name()}] = gvk
	}

	granted := map[string]bool{}
	for _, p := range append(append([]rbacaudit.Permission{},
		rbacaudit.RequiredCluster...), rbacaudit.RequiredNamespaced...) {
		granted[p.Group+"/"+p.Resource+":"+p.Verb] = true
	}

	for _, w := range watched {
		key := watchedKind{pkgPath: w.pkgPath, name: w.name}
		gvk, known := byType[key]
		if !known {
			t.Errorf("%s watches %s.%s, which the scheme does not know. Either the scheme "+
				"is missing an AddToScheme or this test resolved the import wrongly; both "+
				"are real, so do not silence it", w.where, w.pkgPath, w.name)
			continue
		}
		mapping, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			t.Errorf("%s watches %s, which the cluster cannot map to a resource: %v",
				w.where, gvk, err)
			continue
		}
		resource := mapping.Resource.Resource
		for _, verb := range []string{"list", "watch"} {
			if !granted[gvk.Group+"/"+resource+":"+verb] {
				t.Errorf("%s starts an informer on %s, and the required table asks for no "+
					"%s on %s/%s. An informer without it retries forever and says nothing: "+
					"the controller would reconcile nothing on that kind and report healthy "+
					"throughout",
					w.where, gvk.Kind, verb, gvk.Group, resource)
			}
		}
	}
}

func watchedKindsIn(t *testing.T, dir string) []watchedKind {
	t.Helper()

	set := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	// Not parser.ParseDir, which is deprecated for ignoring build tags; no
	// package grouping is needed here.
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(set, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = file
	}
	if len(files) == 0 {
		t.Fatalf("no non-test Go file in %s", dir)
	}

	var found []watchedKind
	for path, file := range files {
		imports := importsOf(file)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !watchBuilders[sel.Sel.Name] {
				return true
			}
			alias, name, ok := literalType(call.Args[0])
			if !ok {
				// Reported rather than skipped: this test claims to see every informer.
				t.Errorf("%s: %s(...) is not called with a &pkg.Type{} literal, so this "+
					"test cannot tell which kind it watches. Widen it rather than "+
					"leaving the kind unchecked",
					set.Position(sel.Sel.Pos()), sel.Sel.Name)
				return true
			}
			pkgPath, known := imports[alias]
			if !known {
				t.Errorf("%s: cannot resolve the import %q", set.Position(sel.Sel.Pos()), alias)
				return true
			}
			found = append(found, watchedKind{
				pkgPath: pkgPath, name: name,
				// The method's position, not the call's: in a chain call.Pos() is the For()
				// line for every Owns() and Watches() under it.
				where: filepath.Base(path) + ":" + strconv.Itoa(set.Position(sel.Sel.Pos()).Line),
			})
			return true
		})
	}
	return found
}

// literalType reads &pkg.Type{} and answers ("pkg", "Type").
func literalType(arg ast.Expr) (string, string, bool) {
	unary, ok := arg.(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		return "", "", false
	}
	composite, ok := unary.X.(*ast.CompositeLit)
	if !ok {
		return "", "", false
	}
	sel, ok := composite.Type.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	return pkg.Name, sel.Sel.Name, true
}

func importsOf(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		alias := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		out[alias] = path
	}
	return out
}
