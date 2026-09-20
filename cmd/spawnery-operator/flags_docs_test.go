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

// flagDefault is one flag.*Var call's name, the Go type its value argument
// implies ("string", "bool" or "duration", from the call's own name --
// StringVar, BoolVar, DurationVar), and the default-value argument itself,
// unevaluated.
type flagDefault struct {
	name string
	kind string
	expr ast.Expr
}

// flagDefaultsInMain walks the same syntax tree flagsInMain does and returns
// every flag.*Var call's default-value argument alongside its name and kind.
// flag.Var(value, name, usage) -- drain-taint's, the only one in main.go --
// carries no default argument at all; its zero value comes from the
// flag.Value it is given, not from a call argument, so there is nothing here
// to compare against the page's "Default: none" and this walk skips it.
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

// durationUnits covers what main.go's own duration flags actually multiply
// by; extending it costs one line if a future flag needs time.Hour or finer.
var durationUnits = map[string]time.Duration{
	"Second": time.Second,
	"Minute": time.Minute,
	"Hour":   time.Hour,
}

// resolveDurationDefault reads an `N * time.Unit` default, the shape every
// duration flag in main.go uses, without needing a full constant evaluator --
// there is no arithmetic here beyond that one multiplication.
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
// into a comparable Go value. Fourteen of main.go's seventeen flags pass a
// literal (or, for --operator-namespace, an os.Getenv call naming a literal
// env var) straight to the flag.*Var call, and the AST walk above already
// has that argument in hand. Three more -- --orphan-interval,
// --permission-check-interval and --agent-bind-address -- default to an
// exported constant in another package, which reading main.go's syntax tree
// alone cannot resolve; this test imports those three packages, the same
// ones main.go itself imports for the same constants, and reads the real
// value directly rather than trying to teach the AST walk cross-package
// lookup. --drain-taint, the seventeenth, never reaches this function: see
// flagDefaultsInMain.
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

// TestFlagDefaultsAreDocumented checks the one thing TestFlagsAreDocumented
// above does not: that the default stated on the page is the default
// main.go actually registers, not just that the flag is mentioned at all.
// Values are compared as parsed Go values rather than as text, because the
// two sides format the same default differently -- time.Duration prints
// 5*time.Minute as "5m0s" where the page says "5m".
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
