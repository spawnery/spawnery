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

// Package theme holds no code: it is the documentation site's colour tokens,
// checked where the site's own build cannot check them.
package theme

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"testing"
)

const stylesheet = "../../../docs/assets/stylesheets/zen.css"

// WCAG 2.x AA for body text.
const minRatio = 4.5

var (
	schemeBlock = regexp.MustCompile(`(?ms)^\[data-md-color-scheme="([a-z]+)"\] \{\n(.*?)^\}`)
	tokenDecl   = regexp.MustCompile(`--zen-([a-z0-9-]+):\s*(#[0-9a-fA-F]{6});`)
)

// Every token that text is drawn in. All of them can sit on either surface.
var textTokens = []string{
	"text", "muted", "accent-text", "green", "yellow", "red",
	"syn-keyword", "syn-string", "syn-number", "syn-comment", "syn-function",
	"syn-name", "syn-variable", "syn-operator", "syn-special", "syn-generic",
}

var surfaces = []string{"base", "mantle"}

func channel(hex string, offset int) float64 {
	v, err := strconv.ParseUint(hex[1+offset:3+offset], 16, 8)
	if err != nil {
		panic(err)
	}
	c := float64(v) / 255
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func luminance(hex string) float64 {
	return 0.2126*channel(hex, 0) + 0.7152*channel(hex, 2) + 0.0722*channel(hex, 4)
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func TestContrastOfKnownPairs(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"#000000", "#ffffff", 21},
		{"#ffffff", "#ffffff", 1},
		{"#777777", "#ffffff", 4.48},
	}
	for _, c := range cases {
		if got := contrast(c.a, c.b); math.Abs(got-c.want) > 0.01 {
			t.Errorf("contrast(%s, %s) = %.3f, want %.2f", c.a, c.b, got, c.want)
		}
	}
}

func TestZenTokensMeetAA(t *testing.T) {
	css, err := os.ReadFile(stylesheet)
	if err != nil {
		t.Fatalf("read %s: %v", stylesheet, err)
	}

	schemes := map[string]map[string]string{}
	inBlocks := 0
	for _, block := range schemeBlock.FindAllStringSubmatch(string(css), -1) {
		if _, seen := schemes[block[1]]; seen {
			t.Fatalf("zen.css declares the %q token block twice", block[1])
		}
		tokens := map[string]string{}
		decls := tokenDecl.FindAllStringSubmatch(block[2], -1)
		inBlocks += len(decls)
		for _, decl := range decls {
			tokens[decl[1]] = decl[2]
		}
		schemes[block[1]] = tokens
	}

	total := len(tokenDecl.FindAllStringSubmatch(string(css), -1))
	if total != inBlocks {
		t.Errorf("zen.css has %d --zen-*: #rrggbb declarations but only %d are inside the two scheme token blocks; %d stray outside them", total, inBlocks, total-inBlocks)
	}

	for _, scheme := range []string{"mocha", "latte"} {
		tokens, ok := schemes[scheme]
		if !ok {
			t.Errorf("zen.css has no [data-md-color-scheme=%q] token block", scheme)
			continue
		}
		lookup := func(name string) (string, bool) {
			v, ok := tokens[name]
			if !ok {
				t.Errorf("%s: --zen-%s is not declared as a #rrggbb literal", scheme, name)
			}
			return v, ok
		}
		for _, fg := range textTokens {
			for _, bg := range surfaces {
				f, okF := lookup(fg)
				b, okB := lookup(bg)
				if !okF || !okB {
					continue
				}
				if r := contrast(f, b); r < minRatio {
					t.Errorf("%s: --zen-%s %s on --zen-%s %s is %.2f:1, below %.1f:1", scheme, fg, f, bg, b, r, minRatio)
				}
			}
		}
		fill, okFill := lookup("accent-text")
		on, okOn := lookup("accent-contrast")
		if okFill && okOn {
			if r := contrast(on, fill); r < minRatio {
				t.Errorf("%s: --zen-accent-contrast %s on --zen-accent-text %s is %.2f:1, below %.1f:1", scheme, on, fill, r, minRatio)
			}
		}

		// The workspace hover state and the diagram's cluster label put text
		// on surface0, which the general text-on-surface loop above does not
		// cover.
		text, okText := lookup("text")
		surface0, okSurface0 := lookup("surface0")
		if okText && okSurface0 {
			if r := contrast(text, surface0); r < minRatio {
				t.Errorf("%s: --zen-text %s on --zen-surface0 %s is %.2f:1, below %.1f:1", scheme, text, surface0, r, minRatio)
			}
		}
	}
}
