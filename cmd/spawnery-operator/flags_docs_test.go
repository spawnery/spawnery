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

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spawnery/spawnery/internal/testenv"
)

// flagsInMain returns every flag name main.go registers with the standard
// library's flag package, found by walking the syntax tree rather than
// matching text against it. A regex tuned for the StringVar/BoolVar/
// DurationVar calls this file happens to use today would silently miss
// flag.Var(&drainTaints, "drain-taint", ...), which takes a pointer to a
// custom type instead of a builtin -- and a parser that quietly skips a flag
// is worse than no parser, because the page would look checked while being
// wrong.
//
// Every flag.XxxVar(ptr, name, ...) call and flag.Var(value, name, ...) call
// carries the flag's name as its second argument; every flag.Xxx(name, ...)
// call that returns a pointer instead of taking one -- unused in main.go
// today -- carries it as its first. Distinguishing the two by whether the
// function name ends in "Var" covers both without one branch per flag type.
//
// zap.Options.BindFlags(flag.CommandLine) registers controller-runtime's own
// logging flags (--zap-log-level and the rest) from inside the zap package,
// not through a flag.XxxVar call written in this file. They never reach this
// walk, which is correct rather than a gap this test happens to have: they
// belong to controller-runtime, are documented there, and are out of scope
// for a page about spawnery's own flags.
func flagsInMain(t *testing.T) []string {
	t.Helper()
	path := testenv.RepoPath(t, "cmd/spawnery-operator/main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "flag" {
			return true
		}
		idx := 0
		if strings.HasSuffix(sel.Sel.Name, "Var") {
			idx = 1
		}
		if idx >= len(call.Args) {
			return true
		}
		lit, ok := call.Args[idx].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		names = append(names, name)
		return true
	})
	sort.Strings(names)
	return names
}

// flagHeading matches the one heading shape flagsInDocs looks for: a level-3
// heading whose entire text is the flag in a code span, "### `--name`". Only
// headings are scanned, and not every code span on the page, so a flag's own
// prose can mention a sibling flag -- --allow-plugin-volumes' section names
// --allow-mount-volumes, the switch it used to be part of -- without that
// mention being misread as a section of its own.
var flagHeading = regexp.MustCompile(`(?m)^### ` + "`" + `--([a-z][a-z0-9-]*)` + "`" + `\s*$`)

// flagsInDocs returns every flag documented on the reference page. A missing
// page reads as documenting nothing rather than failing the test outright, so
// that the failure this test exists to produce -- the page does not exist yet
// -- comes back as "every flag is undocumented" instead of a file-not-found
// error that says nothing about which flags are missing.
func flagsInDocs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(testenv.RepoPath(t, "docs/reference/operator-flags.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read docs/reference/operator-flags.md: %v", err)
	}

	var names []string
	for _, m := range flagHeading.FindAllStringSubmatch(string(raw), -1) {
		names = append(names, m[1])
	}
	sort.Strings(names)
	return names
}

// TestFlagsAreDocumented fails in either direction: a flag main.go defines
// and the page does not carry a heading for, and a heading on the page
// naming a flag main.go no longer defines. The second is the same defect as
// the first wearing the other face -- a reference that still describes a
// removed flag is as wrong as one that omits a new one.
func TestFlagsAreDocumented(t *testing.T) {
	inMain := flagsInMain(t)
	if len(inMain) == 0 {
		t.Fatal("found no flags in main.go at all -- the parser is broken, not the flags")
	}
	inDocs := flagsInDocs(t)

	documented := make(map[string]bool, len(inDocs))
	for _, name := range inDocs {
		documented[name] = true
	}
	defined := make(map[string]bool, len(inMain))
	for _, name := range inMain {
		defined[name] = true
	}

	for _, name := range inMain {
		if !documented[name] {
			t.Errorf("--%s is defined in main.go but has no ### `--%s` section in "+
				"docs/reference/operator-flags.md", name, name)
		}
	}
	for _, name := range inDocs {
		if !defined[name] {
			t.Errorf("docs/reference/operator-flags.md documents --%s, which main.go "+
				"no longer defines", name)
		}
	}
}
