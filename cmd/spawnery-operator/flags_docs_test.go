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

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/rbacaudit"
	"github.com/spawnery/spawnery/internal/testenv"
)

// flagsInMain returns every flag name main.go registers with the standard
// library's flag package, found by walking the syntax tree; a regex would
// miss flag.Var. A *Var call carries the name as its second argument, any
// other as its first. controller-runtime's zap flags are registered elsewhere
// and are out of scope.
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

// flagHeading matches "### `--name`". Only headings count, so a flag's prose
// may mention a sibling flag.
var flagHeading = regexp.MustCompile(`(?m)^### ` + "`" + `--([a-z][a-z0-9-]*)` + "`" + `\s*$`)

// flagsInDocs returns every flag documented on the reference page. A missing
// page documents nothing, so the failure names every missing flag.
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

// TestFlagsAreDocumented fails in either direction: an undocumented flag, or a
// documented flag main.go no longer defines.
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

// flagDefault is one flag.*Var call's name, its kind ("string", "bool" or
// "duration", from the call's name), and the unevaluated default argument.
type flagDefault struct {
	name string
	kind string
	expr ast.Expr
}

// flagDefaultsInMain returns every flag.*Var call's default. A bare flag.Var
// (drain-taint) has no default argument and is skipped.
func flagDefaultsInMain(t *testing.T) []flagDefault {
	t.Helper()
	path := testenv.RepoPath(t, "cmd/spawnery-operator/main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var defaults []flagDefault
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
		kind := strings.TrimSuffix(sel.Sel.Name, "Var")
		if kind == "" || kind == sel.Sel.Name {
			// "Var" itself (bare flag.Value registration, no default arg) or
			// a non-Var function such as flag.Parse.
			return true
		}
		if len(call.Args) < 3 {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		defaults = append(defaults, flagDefault{name: name, kind: strings.ToLower(kind), expr: call.Args[2]})
		return true
	})
	return defaults
}

// resolveStringDefault reads a string-typed default straight out of the
// syntax tree: either a plain literal, or -- --operator-namespace's case --
// os.Getenv("NAME"), rendered as "$NAME" to match how the page states it.
func resolveStringDefault(expr ast.Expr) (string, bool) {
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		v, err := strconv.Unquote(lit.Value)
		return v, err == nil
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "os" || sel.Sel.Name != "Getenv" {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	envVar, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return "$" + envVar, true
}

func resolveBoolDefault(expr ast.Expr) (bool, bool) {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false, false
	}
	switch id.Name {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// durationUnits covers what main.go's duration flags multiply by.
var durationUnits = map[string]time.Duration{
	"Second": time.Second,
	"Minute": time.Minute,
	"Hour":   time.Hour,
}

// resolveDurationDefault reads an `N * time.Unit` default, the shape every
// duration flag in main.go uses.
func resolveDurationDefault(expr ast.Expr) (time.Duration, bool) {
	bin, ok := expr.(*ast.BinaryExpr)
	if !ok || bin.Op != token.MUL {
		return 0, false
	}
	lit, ok := bin.X.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.ParseInt(lit.Value, 10, 64)
	if err != nil {
		return 0, false
	}
	sel, ok := bin.Y.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "time" {
		return 0, false
	}
	unit, ok := durationUnits[sel.Sel.Name]
	if !ok {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// flagExpectation is one flag's default, in whichever of the three shapes
// the page renders it as.
type flagExpectation struct {
	name string
	kind string
	str  string
	b    bool
	dur  time.Duration
}

// expectedFlagDefaults resolves every flag.*Var default main.go registers
// into a comparable Go value. Defaults that are constants of another package
// are read from that package directly.
func expectedFlagDefaults(t *testing.T) []flagExpectation {
	t.Helper()

	overrides := map[string]flagExpectation{
		"orphan-interval":           {kind: "duration", dur: controller.DefaultOrphanInterval},
		"permission-check-interval": {kind: "duration", dur: rbacaudit.DefaultCheckInterval},
		"agent-bind-address":        {kind: "string", str: fmt.Sprintf(":%d", agentserver.DefaultPort)},
	}

	var out []flagExpectation
	for _, d := range flagDefaultsInMain(t) {
		if ov, ok := overrides[d.name]; ok {
			ov.name = d.name
			out = append(out, ov)
			continue
		}
		switch d.kind {
		case "string":
			if v, ok := resolveStringDefault(d.expr); ok {
				out = append(out, flagExpectation{name: d.name, kind: "string", str: v})
				continue
			}
		case "bool":
			if v, ok := resolveBoolDefault(d.expr); ok {
				out = append(out, flagExpectation{name: d.name, kind: "bool", b: v})
				continue
			}
		case "duration":
			if v, ok := resolveDurationDefault(d.expr); ok {
				out = append(out, flagExpectation{name: d.name, kind: "duration", dur: v})
				continue
			}
		}
		t.Errorf("--%s: default argument in main.go is not a literal this test can read and is not "+
			"one of the known cross-package overrides in expectedFlagDefaults -- its default has "+
			"gone unchecked; add a case or an override", d.name)
	}
	return out
}

// stripBackticks removes one wrapping pair, if present, and nothing else --
// the page never nests code spans inside a "Default:" line.
func stripBackticks(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, "`") && strings.HasSuffix(s, "`") {
		return s[1 : len(s)-1]
	}
	return s
}

// docDefaultText returns the text of the "Default: ..." line in the named
// flag's own section -- between its "### `--name`" heading and the next
// level-3 heading or end of page -- with the "Default:" prefix removed.
func docDefaultText(page, name string) (string, bool) {
	heading := "### `--" + name + "`"
	start := strings.Index(page, heading)
	if start == -1 {
		return "", false
	}
	section := page[start+len(heading):]
	if next := strings.Index(section, "\n### "); next != -1 {
		section = section[:next]
	}
	for _, line := range strings.Split(section, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "Default:"); ok {
			return strings.TrimSpace(after), true
		}
	}
	return "", false
}

// TestFlagDefaultsAreDocumented compares the page's defaults with main.go's as
// parsed values: time.Duration prints "5m0s" where the page says "5m".
func TestFlagDefaultsAreDocumented(t *testing.T) {
	raw, err := os.ReadFile(testenv.RepoPath(t, "docs/reference/operator-flags.md"))
	if err != nil {
		t.Fatalf("read docs/reference/operator-flags.md: %v", err)
	}
	page := string(raw)

	expected := expectedFlagDefaults(t)
	if len(expected) == 0 {
		t.Fatal("resolved no flag defaults at all -- the parser is broken, not the flags")
	}

	for _, exp := range expected {
		docText, ok := docDefaultText(page, exp.name)
		if !ok {
			t.Errorf("--%s: no \"Default:\" line found in its section of "+
				"docs/reference/operator-flags.md", exp.name)
			continue
		}
		switch exp.kind {
		case "bool":
			got, err := strconv.ParseBool(stripBackticks(docText))
			if err != nil {
				t.Errorf("--%s: docs default %q does not parse as a bool: %v", exp.name, docText, err)
				continue
			}
			if got != exp.b {
				t.Errorf("--%s: main.go defaults to %v, docs say %q", exp.name, exp.b, docText)
			}
		case "duration":
			got, err := time.ParseDuration(stripBackticks(docText))
			if err != nil {
				t.Errorf("--%s: docs default %q does not parse as a duration: %v", exp.name, docText, err)
				continue
			}
			if got != exp.dur {
				t.Errorf("--%s: main.go defaults to %s, docs say %q", exp.name, exp.dur, docText)
			}
		case "string":
			if exp.str == "" {
				if !strings.HasPrefix(strings.ToLower(docText), "empty") {
					t.Errorf("--%s: main.go defaults to the empty string, docs say %q "+
						"(expected it to start with \"empty\")", exp.name, docText)
				}
				continue
			}
			if stripBackticks(docText) != exp.str {
				t.Errorf("--%s: main.go defaults to %q, docs say %q", exp.name, exp.str, docText)
			}
		}
	}
}
