# The documentation site in the Zen style — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `docs.spawnery.cloud` takes on paul.wtf's Zen visual system — Catppuccin Mocha and Latte, Peach as its accent, the mantle frame, the bar as a workspace switcher — without changing a word of content.

**Architecture:** Material for MkDocs stays and is restyled. One stylesheet, `docs/assets/stylesheets/zen.css`, holds two token blocks (Mocha, Latte), maps them onto Material's `--md-*` variables, and styles the components. One template override, `overrides/main.html`, adds the frame and `theme-color`. Fonts are fetched by Nix from Fontsource and served by the site. A Go test reads the token blocks and fails when a text colour drops below AA.

**Tech Stack:** mkdocs-material 9.7.6 (from nixpkgs), Nix, Go (contrast test), bash, headless Chromium for screenshots.

**Spec:** `docs/superpowers/specs/2026-09-17-docs-zen-theme-design.md` — read it before starting a task; it is the authority this plan argues from.

## Global Constraints

- Every command runs in the dev shell with the full prefix: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c <command>`. Never `cd` before it; pass the flake path as the argument.
- Everything in git is English. Comment policy (`/home/paul/.claude/CLAUDE.md` via `/home/paul/git/spawnery/CLAUDE.md` conventions): no comment unless a reader cannot derive it from the code beside it. The history of a change belongs in the commit message, never in the file.
- Conventional Commits with a scope; body wrapped at 72 columns saying why; every message ends with exactly:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01RhLpABDQDo2jeWRbfNEdtT
  ```
- Commits are gpg-signed. On the host `dev` a failed signing attempt kills the user's session: before the first commit of a task run
  `gpg-connect-agent 'keyinfo --list' /bye | awk '$3=="BEC5947C38AD8CD63056A018FD306300930B0421"{print $7}'` — `1` means cached; anything else: stop and report, never disable signing.
- **No content changes.** No page text, no nav order, no heading text.
- **No Google Fonts, no CDN.** `theme.font: false`; fonts come from `nix/fonts.nix` only.
- Fonts, pinned exactly:
  - `@fontsource-variable/jetbrains-mono` 5.3.0, `sha256-mW/mNopIDJzhXU3iKiaCt8QLQDcY/uKxXnJy8kT9mT8=`
  - `@fontsource/ibm-plex-sans` 5.3.0, `sha256-IyWSryCuQTAmzHBfycEEE1+3YEMv1xDwagLDngYtMhM=`
- Colour tokens, exactly (the contrast test enforces AA; these values pass it):

  | token | mocha | latte |
  |---|---|---|
  | crust | `#11111b` | `#dce0e8` |
  | mantle | `#181825` | `#e6e9ef` |
  | base | `#1e1e2e` | `#eff1f5` |
  | surface0 | `#313244` | `#ccd0da` |
  | surface1 | `#45475a` | `#bcc0cc` |
  | text | `#cdd6f4` | `#4c4f69` |
  | muted | `#9399b2` | `#63667a` |
  | accent | `#fab387` | `#fe640b` |
  | accent-text | `#fab387` | `#b44201` |
  | accent-contrast | `#11111b` | `#eff1f5` |
  | green | `#a6e3a1` | `#2f7620` |
  | yellow | `#f9e2af` | `#905c13` |
  | red | `#f38ba8` | `#cd0f38` |
  | syn-keyword | `#cba6f7` | `#8534ef` |
  | syn-string | `#a6e3a1` | `#2f7620` |
  | syn-number | `#fab387` | `#b44201` |
  | syn-comment | `#9399b2` | `#636679` |
  | syn-function | `#89b4fa` | `#0b59f4` |
  | syn-name | `#f9e2af` | `#905c13` |
  | syn-variable | `#eba0ac` | `#cb1b2b` |
  | syn-operator | `#89dceb` | `#036f9a` |
  | syn-special | `#f5c2e7` | `#bc1d91` |
  | syn-generic | `#f38ba8` | `#cd0f38` |
- `--zen-accent` (raw) is used for exactly one thing: the `──` glyph before an `h2`. Links, buttons, active workspace, selection and focus ring use `--zen-accent-text`.
- Frame and bar sizes from the homepage: frame 10px, bar 44px, viewport corner radius 16px. Radii: 16 panels, 10 nested elements and buttons, 8 chips.
- Material's desktop breakpoint is `76.25em` (1220px); its tabs are hidden below `76.234375em`.
- A task that changes what the site looks like ends with screenshots (`hack/docs-screenshots.sh`, created in Task 2) that the implementer **reads** before reporting done.

## Rulings made while writing this plan

- **No partials are copied for the bar.** `navigation.tabs.sticky` is Material's own way of rendering the tabs inside `<header>` (`partials/header.html`, the block guarded by `"navigation.tabs.sticky" in features`); the workspace numbers are a CSS counter. The only template override is `overrides/main.html`.
- **The `h2` trailing rule is not flex.** 53 `h2` headings in `docs/` contain inline code; a flex container drops the whitespace between a code span and the text beside it. The rule is an inline-block `::after` with `width: 100vw; margin-right: -100vw` inside `overflow: hidden`.
- **`─` (U+2500) is in neither Fontsource subset** (latin, latin-ext). The glyph falls back to the system monospace font, exactly as it does on the homepage. Accepted, not worked around.
- **All text-carrying content sits on base or mantle.** Crust is canvas only (behind the page window, under the wallpaper). This is what lets the contrast test check two surfaces and be complete.

---

### Task 1: Fonts the site serves itself

**Files:**
- Create: `nix/fonts.nix`
- Create: `hack/vendor-fonts.sh`
- Modify: `flake.nix` (the `let` bindings near `mermaid-js = pkgs.callPackage ./nix/mermaid.nix { };`, the `docs-site = ...` line, and the `inherit ... docs-site;` line in the outputs)
- Modify: `nix/docs-site.nix` (arguments and `postPatch`)
- Modify: `.gitignore`, `Makefile` (`docs-assets` target and its comment)

**Interfaces:**
- Produces: flake package `docs-fonts`, a store path containing exactly 12 `.woff2` files at its root, named as Fontsource names them (listed in Step 1). At build time and after `hack/vendor-fonts.sh`, they live at `docs/assets/fonts/<name>.woff2`. Task 2's `@font-face` rules reference `../fonts/<name>.woff2` from `docs/assets/stylesheets/zen.css`.

- [ ] **Step 1: Write `nix/fonts.nix`**

```nix
# The two typefaces of the documentation site, served by the site itself.
#
# From Fontsource's npm tarballs for the reason nix/mermaid.nix gives: a site
# served from one's own cluster should not depend on somebody else's. Only the
# latin and latin-ext subsets, which is what the site's text uses.
{ fetchurl
, runCommand
}:

let
  jetbrainsMono = fetchurl {
    url = "https://registry.npmjs.org/@fontsource-variable/jetbrains-mono/-/jetbrains-mono-5.3.0.tgz";
    hash = "sha256-mW/mNopIDJzhXU3iKiaCt8QLQDcY/uKxXnJy8kT9mT8=";
  };

  ibmPlexSans = fetchurl {
    url = "https://registry.npmjs.org/@fontsource/ibm-plex-sans/-/ibm-plex-sans-5.3.0.tgz";
    hash = "sha256-IyWSryCuQTAmzHBfycEEE1+3YEMv1xDwagLDngYtMhM=";
  };
in
runCommand "docs-fonts" { } ''
  mkdir -p $out
  tar -xzf ${jetbrainsMono} -C $out --strip-components=2 \
    package/files/jetbrains-mono-latin-wght-normal.woff2 \
    package/files/jetbrains-mono-latin-wght-italic.woff2 \
    package/files/jetbrains-mono-latin-ext-wght-normal.woff2 \
    package/files/jetbrains-mono-latin-ext-wght-italic.woff2
  tar -xzf ${ibmPlexSans} -C $out --strip-components=2 \
    package/files/ibm-plex-sans-latin-400-normal.woff2 \
    package/files/ibm-plex-sans-latin-400-italic.woff2 \
    package/files/ibm-plex-sans-latin-500-normal.woff2 \
    package/files/ibm-plex-sans-latin-600-normal.woff2 \
    package/files/ibm-plex-sans-latin-ext-400-normal.woff2 \
    package/files/ibm-plex-sans-latin-ext-400-italic.woff2 \
    package/files/ibm-plex-sans-latin-ext-500-normal.woff2 \
    package/files/ibm-plex-sans-latin-ext-600-normal.woff2
''
```

- [ ] **Step 2: Wire it into `flake.nix`**

Directly below `mermaid-js = pkgs.callPackage ./nix/mermaid.nix { };` add:

```nix
          docs-fonts = pkgs.callPackage ./nix/fonts.nix { };
```

Change the comment and the `docs-site` line from

```nix
          # mermaid-js and agent-api-javadoc are local let bindings, not pkgs
          # attributes, so callPackage cannot fill them and both are passed
          # explicitly.
          docs-site = pkgs.callPackage ./nix/docs-site.nix { inherit mermaid-js agent-api-javadoc; };
```

to

```nix
          # mermaid-js, docs-fonts and agent-api-javadoc are local let
          # bindings, not pkgs attributes, so callPackage cannot fill them and
          # all three are passed explicitly.
          docs-site = pkgs.callPackage ./nix/docs-site.nix { inherit mermaid-js docs-fonts agent-api-javadoc; };
```

In the outputs line `inherit spawnery-slp spawnery-stubop spawnery-join spawnery-config agents spawnery-operator mermaid-js agent-api-javadoc docs-site;` add `docs-fonts` after `mermaid-js`.

- [ ] **Step 3: Build it and check the file list**

`git add nix/fonts.nix flake.nix` first — Nix builds read the git index, not the working tree.

Run: `nix --extra-experimental-features 'nix-command flakes' build /home/paul/git/spawnery#docs-fonts --no-link --print-out-paths | xargs ls`
Expected: exactly these 12 names:
```
ibm-plex-sans-latin-400-italic.woff2      ibm-plex-sans-latin-ext-500-normal.woff2
ibm-plex-sans-latin-400-normal.woff2      ibm-plex-sans-latin-ext-600-normal.woff2
ibm-plex-sans-latin-500-normal.woff2      jetbrains-mono-latin-ext-wght-italic.woff2
ibm-plex-sans-latin-600-normal.woff2      jetbrains-mono-latin-ext-wght-normal.woff2
ibm-plex-sans-latin-ext-400-italic.woff2  jetbrains-mono-latin-wght-italic.woff2
ibm-plex-sans-latin-ext-400-normal.woff2  jetbrains-mono-latin-wght-normal.woff2
```

- [ ] **Step 4: Install them into the site build**

In `nix/docs-site.nix`, add `docs-fonts` to the argument set after `mermaid-js`, and in `postPatch` directly after the `install -m 644 ${mermaid-js}/mermaid.min.js docs/assets/mermaid.min.js` line add:

```nix
    mkdir -p docs/assets/fonts
    install -m 644 ${docs-fonts}/*.woff2 docs/assets/fonts/
```

Extend the comment above `postPatch` so its first paragraph names both gitignored files: replace `docs/assets/mermaid.min.js is gitignored -- hack/vendor-mermaid.sh writes` with `docs/assets/mermaid.min.js and docs/assets/fonts/ are gitignored -- hack/vendor-mermaid.sh and hack/vendor-fonts.sh write`, and `mkdocs.yml points the mermaid2 plugin at exactly that path.` with `mkdocs.yml and the stylesheet point at exactly those paths.`

- [ ] **Step 5: `hack/vendor-fonts.sh`, `.gitignore`, `Makefile`**

`hack/vendor-fonts.sh` (mode 755):

```bash
#!/usr/bin/env bash
# Puts the site's fonts where docs/assets/stylesheets/zen.css expects them, so
# `mkdocs serve` renders the real typefaces locally.
#
# Deliberately not committed: nix/fonts.nix is where they are pinned, and
# nix/docs-site.nix installs the same derivation into a built site.
set -euo pipefail

cd "$(dirname "$0")/.."

out="$(nix build --no-link --print-out-paths .#docs-fonts)"

mkdir -p docs/assets/fonts
install -m 644 "$out"/*.woff2 docs/assets/fonts/
```

`.gitignore`, directly below the `docs/assets/mermaid.min.js` line and its comment, add:

```
# Vendored by hack/vendor-fonts.sh from the pins in nix/fonts.nix.
docs/assets/fonts/
```

`Makefile`: in the `docs-assets` recipe add `hack/vendor-fonts.sh` as the second line (after `hack/vendor-mermaid.sh`). In the comment above it, change `Two build products` to `Three build products`, and add a third entry after the mermaid one:

```
#   docs/assets/fonts/               -- pinned in nix/fonts.nix, without them
#                                        the site falls back to system fonts
#                                        silently.
```

- [ ] **Step 6: Verify the whole build still passes and carries the fonts**

Run: `bash -n hack/vendor-fonts.sh && nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs`
Expected: exit 0.

Run: `nix --extra-experimental-features 'nix-command flakes' build /home/paul/git/spawnery#docs-site --no-link --print-out-paths | xargs -I{} sh -c 'ls {}/assets/fonts | wc -l'`
Expected: `12`

Run: `hack/vendor-fonts.sh && git status --short docs/assets/`
Expected: no output (the vendored directory is ignored).

- [ ] **Step 7: Commit**

```bash
git add nix/fonts.nix hack/vendor-fonts.sh flake.nix nix/docs-site.nix .gitignore Makefile
git commit
```

Message subject: `feat(docs): the site's fonts come from a pinned derivation`. Body: why — no third-party font host, same pattern and reason as nix/mermaid.nix, only latin and latin-ext.

---

### Task 2: Colour tokens, the two schemes, and the test that guards them

**Files:**
- Create: `internal/docsgen/theme/contrast_test.go`
- Create: `docs/assets/stylesheets/zen.css`
- Create: `hack/docs-screenshots.sh`
- Modify: `mkdocs.yml` (`theme.palette`, `theme.font`, add `extra_css`)

**Interfaces:**
- Consumes: `docs/assets/fonts/*.woff2` from Task 1 (names in Task 1 Step 3).
- Produces:
  - Every `--zen-<token>` custom property in the Global Constraints table, declared in exactly two blocks of `zen.css`, each beginning at column 0 with `[data-md-color-scheme="mocha"] {` or `[data-md-color-scheme="latte"] {` and ending with `}` at column 0. Later tasks use these names and add **no** new `--zen-*` colour tokens outside those blocks.
  - `hack/docs-screenshots.sh <out-dir> [page ...]` — writes `<page>-<scheme>-<width>.png` for schemes `latte`,`mocha` and widths `1440`,`390`; page `""` is saved as `home`.
  - `zen.css` sections are marked by a single-line comment `/* ── <name> ── */`; later tasks append sections at the end of the file.

- [ ] **Step 1: Write the failing test**

`internal/docsgen/theme/contrast_test.go`:

```go
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
	for _, block := range schemeBlock.FindAllStringSubmatch(string(css), -1) {
		if _, seen := schemes[block[1]]; seen {
			t.Fatalf("zen.css declares the %q token block twice", block[1])
		}
		tokens := map[string]string{}
		for _, decl := range tokenDecl.FindAllStringSubmatch(block[2], -1) {
			tokens[decl[1]] = decl[2]
		}
		schemes[block[1]] = tokens
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
	}
}
```

- [ ] **Step 2: Run it and watch it fail for the right reason**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/docsgen/theme/ -count=1`
Expected: `TestContrastOfKnownPairs` passes; `TestZenTokensMeetAA` FAILS with `read ../../../docs/assets/stylesheets/zen.css: open ...: no such file or directory`.

- [ ] **Step 3: Write `zen.css` — tokens, Material mapping, fonts, base rules**

`docs/assets/stylesheets/zen.css`:

```css
/* ── tokens ── */

[data-md-color-scheme="mocha"] {
  color-scheme: dark;
  --zen-crust: #11111b;
  --zen-mantle: #181825;
  --zen-base: #1e1e2e;
  --zen-surface0: #313244;
  --zen-surface1: #45475a;
  --zen-text: #cdd6f4;
  --zen-muted: #9399b2;
  --zen-accent: #fab387;
  --zen-accent-text: #fab387;
  --zen-accent-contrast: #11111b;
  --zen-green: #a6e3a1;
  --zen-yellow: #f9e2af;
  --zen-red: #f38ba8;
  --zen-syn-keyword: #cba6f7;
  --zen-syn-string: #a6e3a1;
  --zen-syn-number: #fab387;
  --zen-syn-comment: #9399b2;
  --zen-syn-function: #89b4fa;
  --zen-syn-name: #f9e2af;
  --zen-syn-variable: #eba0ac;
  --zen-syn-operator: #89dceb;
  --zen-syn-special: #f5c2e7;
  --zen-syn-generic: #f38ba8;
  --zen-shadow: 0 10px 30px -12px rgba(0, 0, 0, 0.55);
}

/* Latte's text colours are Catppuccin's hues darkened until each reaches
   4.6:1 on mantle; the raw Latte palette is drawn for surfaces and fails AA
   as text. */
[data-md-color-scheme="latte"] {
  color-scheme: light;
  --zen-crust: #dce0e8;
  --zen-mantle: #e6e9ef;
  --zen-base: #eff1f5;
  --zen-surface0: #ccd0da;
  --zen-surface1: #bcc0cc;
  --zen-text: #4c4f69;
  --zen-muted: #63667a;
  --zen-accent: #fe640b;
  --zen-accent-text: #b44201;
  --zen-accent-contrast: #eff1f5;
  --zen-green: #2f7620;
  --zen-yellow: #905c13;
  --zen-red: #cd0f38;
  --zen-syn-keyword: #8534ef;
  --zen-syn-string: #2f7620;
  --zen-syn-number: #b44201;
  --zen-syn-comment: #636679;
  --zen-syn-function: #0b59f4;
  --zen-syn-name: #905c13;
  --zen-syn-variable: #cb1b2b;
  --zen-syn-operator: #036f9a;
  --zen-syn-special: #bc1d91;
  --zen-syn-generic: #cd0f38;
  --zen-shadow: 0 10px 30px -14px rgba(76, 79, 105, 0.35);
}

/* ── material ── */

[data-md-color-scheme="mocha"], [data-md-color-scheme="latte"] {
  --md-default-bg-color: var(--zen-base);
  --md-default-bg-color--light: var(--zen-mantle);
  --md-default-bg-color--lighter: var(--zen-surface0);
  --md-default-bg-color--lightest: var(--zen-surface1);
  --md-default-fg-color: var(--zen-text);
  --md-default-fg-color--light: var(--zen-muted);
  --md-default-fg-color--lighter: var(--zen-surface1);
  --md-default-fg-color--lightest: var(--zen-surface0);
  --md-primary-fg-color: var(--zen-mantle);
  --md-primary-fg-color--light: var(--zen-mantle);
  --md-primary-fg-color--dark: var(--zen-mantle);
  --md-primary-bg-color: var(--zen-text);
  --md-primary-bg-color--light: var(--zen-muted);
  --md-accent-fg-color: var(--zen-accent-text);
  --md-accent-fg-color--transparent: color-mix(in srgb, var(--zen-accent-text) 12%, transparent);
  --md-accent-bg-color: var(--zen-accent-contrast);
  --md-accent-bg-color--light: var(--zen-accent-contrast);
  --md-typeset-color: var(--zen-text);
  --md-typeset-a-color: var(--zen-accent-text);
  --md-typeset-mark-color: color-mix(in srgb, var(--zen-accent-text) 22%, transparent);
  --md-typeset-table-color: var(--zen-surface0);
  --md-typeset-table-color--light: var(--zen-mantle);
  --md-code-fg-color: var(--zen-text);
  --md-code-bg-color: var(--zen-mantle);
  --md-code-bg-color--light: var(--zen-mantle);
  --md-code-bg-color--lighter: var(--zen-surface0);
  --md-code-hl-color: color-mix(in srgb, var(--zen-accent-text) 18%, transparent);
  --md-code-hl-color--light: color-mix(in srgb, var(--zen-accent-text) 10%, transparent);
  --md-code-hl-number-color: var(--zen-syn-number);
  --md-code-hl-special-color: var(--zen-syn-special);
  --md-code-hl-function-color: var(--zen-syn-function);
  --md-code-hl-constant-color: var(--zen-syn-number);
  --md-code-hl-keyword-color: var(--zen-syn-keyword);
  --md-code-hl-string-color: var(--zen-syn-string);
  --md-code-hl-name-color: var(--zen-syn-name);
  --md-code-hl-operator-color: var(--zen-syn-operator);
  --md-code-hl-punctuation-color: var(--zen-syn-comment);
  --md-code-hl-comment-color: var(--zen-syn-comment);
  --md-code-hl-generic-color: var(--zen-syn-generic);
  --md-code-hl-variable-color: var(--zen-syn-variable);
  --md-footer-fg-color: var(--zen-text);
  --md-footer-fg-color--light: var(--zen-muted);
  --md-footer-fg-color--lighter: var(--zen-muted);
  --md-footer-bg-color: var(--zen-mantle);
  --md-footer-bg-color--dark: var(--zen-mantle);
  --md-admonition-fg-color: var(--zen-text);
  --md-admonition-bg-color: var(--zen-base);
  --md-shadow-z1: none;
  --md-shadow-z2: var(--zen-shadow);
  --md-shadow-z3: var(--zen-shadow);
}

:root {
  --md-text-font: "IBM Plex Sans";
  --md-code-font: "JetBrains Mono";
}

::selection {
  background: var(--zen-accent-text);
  color: var(--zen-accent-contrast);
}

:focus-visible {
  outline: 2px solid var(--zen-accent-text);
  outline-offset: 2px;
}

/* ── fonts ── */

@font-face {
  font-family: "JetBrains Mono";
  font-style: normal;
  font-display: swap;
  font-weight: 100 800;
  src: url("../fonts/jetbrains-mono-latin-wght-normal.woff2") format("woff2");
  unicode-range: U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD;
}
@font-face {
  font-family: "JetBrains Mono";
  font-style: normal;
  font-display: swap;
  font-weight: 100 800;
  src: url("../fonts/jetbrains-mono-latin-ext-wght-normal.woff2") format("woff2");
  unicode-range: U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF;
}
@font-face {
  font-family: "JetBrains Mono";
  font-style: italic;
  font-display: swap;
  font-weight: 100 800;
  src: url("../fonts/jetbrains-mono-latin-wght-italic.woff2") format("woff2");
  unicode-range: U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD;
}
@font-face {
  font-family: "JetBrains Mono";
  font-style: italic;
  font-display: swap;
  font-weight: 100 800;
  src: url("../fonts/jetbrains-mono-latin-ext-wght-italic.woff2") format("woff2");
  unicode-range: U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF;
}
@font-face {
  font-family: "IBM Plex Sans";
  font-style: normal;
  font-display: swap;
  font-weight: 400;
  src: url("../fonts/ibm-plex-sans-latin-400-normal.woff2") format("woff2");
  unicode-range: U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD;
}
@font-face {
  font-family: "IBM Plex Sans";
  font-style: normal;
  font-display: swap;
  font-weight: 400;
  src: url("../fonts/ibm-plex-sans-latin-ext-400-normal.woff2") format("woff2");
  unicode-range: U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF;
}
@font-face {
  font-family: "IBM Plex Sans";
  font-style: italic;
  font-display: swap;
  font-weight: 400;
  src: url("../fonts/ibm-plex-sans-latin-400-italic.woff2") format("woff2");
  unicode-range: U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD;
}
@font-face {
  font-family: "IBM Plex Sans";
  font-style: italic;
  font-display: swap;
  font-weight: 400;
  src: url("../fonts/ibm-plex-sans-latin-ext-400-italic.woff2") format("woff2");
  unicode-range: U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF;
}
@font-face {
  font-family: "IBM Plex Sans";
  font-style: normal;
  font-display: swap;
  font-weight: 500;
  src: url("../fonts/ibm-plex-sans-latin-500-normal.woff2") format("woff2");
  unicode-range: U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD;
}
@font-face {
  font-family: "IBM Plex Sans";
  font-style: normal;
  font-display: swap;
  font-weight: 500;
  src: url("../fonts/ibm-plex-sans-latin-ext-500-normal.woff2") format("woff2");
  unicode-range: U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF;
}
@font-face {
  font-family: "IBM Plex Sans";
  font-style: normal;
  font-display: swap;
  font-weight: 600;
  src: url("../fonts/ibm-plex-sans-latin-600-normal.woff2") format("woff2");
  unicode-range: U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD;
}
@font-face {
  font-family: "IBM Plex Sans";
  font-style: normal;
  font-display: swap;
  font-weight: 600;
  src: url("../fonts/ibm-plex-sans-latin-ext-600-normal.woff2") format("woff2");
  unicode-range: U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF;
}
```

- [ ] **Step 4: Point `mkdocs.yml` at the schemes and the stylesheet**

Replace the whole `palette:` list under `theme:` with:

```yaml
  palette:
    - media: "(prefers-color-scheme: dark)"
      scheme: mocha
      primary: custom
      accent: custom
      toggle:
        icon: material/weather-sunny
        name: Switch to Latte
    - media: "(prefers-color-scheme: light)"
      scheme: latte
      primary: custom
      accent: custom
      toggle:
        icon: material/weather-night
        name: Switch to Mocha
  font: false
```

Add a top-level key directly after the `theme:` block:

```yaml
extra_css:
  - assets/stylesheets/zen.css
```

- [ ] **Step 5: Run the test and the build**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/docsgen/theme/ -count=1 -v`
Expected: both tests PASS.

Run: `git add docs/assets/stylesheets/zen.css mkdocs.yml && nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs`
Expected: exit 0.

- [ ] **Step 6: Prove the test bites, in a throwaway worktree**

```bash
wt=$(mktemp -d)
git worktree add --detach "$wt" HEAD
cp docs/assets/stylesheets/zen.css "$wt/docs/assets/stylesheets/zen.css"
mkdir -p "$wt/internal/docsgen/theme" && cp internal/docsgen/theme/contrast_test.go "$wt/internal/docsgen/theme/"
sed -i 's/--zen-accent-text: #b44201;/--zen-accent-text: #bc4501;/' "$wt/docs/assets/stylesheets/zen.css"
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c sh -c "cd $wt && go test ./internal/docsgen/theme/ -count=1"
git worktree remove --force "$wt"
```

Expected: FAIL containing `latte: --zen-accent-text #bc4501 on --zen-mantle #e6e9ef is 4.33:1, below 4.5:1`. Paste that line into the report. Then confirm `git worktree list` shows only the main tree.

- [ ] **Step 7: The screenshot script**

`hack/docs-screenshots.sh` (mode 755):

```bash
#!/usr/bin/env bash
# Screenshots of the built documentation site at desktop and phone width in
# both colour schemes, for judging the theme without a browser at hand.
#
# Usage: hack/docs-screenshots.sh <out-dir> [page ...]
# A page is a site path such as "tutorial/"; "" is the home page.
set -euo pipefail

cd "$(dirname "$0")/.."

out="${1:?usage: hack/docs-screenshots.sh <out-dir> [page ...]}"
shift
pages=("$@")
[ "${#pages[@]}" -gt 0 ] || pages=("" "tutorial/" "guides/scaling-and-boosts/" "reference/crds/")

site="$(nix build --no-link --print-out-paths .#docs-site)"
chromium="$(nix build --no-link --print-out-paths --inputs-from . nixpkgs#chromium)/bin/chromium"

port=8317
python3 -m http.server --bind 127.0.0.1 --directory "$site" "$port" >/dev/null 2>&1 &
server=$!
trap 'kill "$server"' EXIT
until curl -fsS "http://127.0.0.1:$port/" >/dev/null 2>&1; do sleep 0.2; done

mkdir -p "$out"
for page in "${pages[@]}"; do
  name="${page%/}"
  name="${name//\//_}"
  name="${name:-home}"
  for scheme in latte mocha; do
    # Blink's PreferredColorScheme enum: 0 is dark, 1 is light.
    if [ "$scheme" = mocha ]; then pref=0; else pref=1; fi
    for size in 1440,900 390,844; do
      "$chromium" --headless --no-sandbox --disable-gpu --hide-scrollbars \
        --blink-settings=preferredColorScheme="$pref" \
        --window-size="$size" --virtual-time-budget=5000 \
        --screenshot="$out/$name-$scheme-${size%%,*}.png" \
        "http://127.0.0.1:$port/$page" >/dev/null 2>&1
    done
  done
done
ls "$out"
```

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c hack/docs-screenshots.sh /tmp/zen-task2 ""`
Expected: four files `home-latte-1440.png home-latte-390.png home-mocha-1440.png home-mocha-390.png`.

**Read `home-latte-1440.png` and `home-mocha-1440.png` with the Read tool.** They must differ: Latte light, Mocha dark, links in Peach, body text in a sans serif, code in mono. If both come out in the same scheme, first swap `pref=0`/`pref=1` and rerun. If they are still identical, this Chromium ignores `--blink-settings=preferredColorScheme`: replace that flag with `--force-dark-mode` for mocha and no flag for latte, rerun, and say in the report which variant worked.

- [ ] **Step 8: Commit**

```bash
git add internal/docsgen/theme/contrast_test.go docs/assets/stylesheets/zen.css mkdocs.yml hack/docs-screenshots.sh
git commit
```

Subject: `feat(docs): Catppuccin Mocha and Latte, guarded by a contrast test`. Body: the Latte measurement (raw Peach 2.64:1 on base, #bc4501 4.33:1 on mantle), why every text token is checked on both surfaces, the mutation result from Step 6.

---

### Task 3: Type and surfaces

**Files:**
- Modify: `docs/assets/stylesheets/zen.css` (append a section)

**Interfaces:**
- Consumes: the `--zen-*` tokens and `--md-code-font-family` / `--md-text-font-family` (Material derives both from Task 2's `:root` font variables).
- Produces: `.md-main__inner` is the page window (base, radius 16, shadow) on a crust canvas. Task 4's frame and Task 5's wallpaper rely on the canvas being crust and the window being opaque.

- [ ] **Step 1: Append the section**

```css
/* ── surfaces and type ── */

body {
  background: var(--zen-crust);
}

.md-container,
.md-main {
  background: transparent;
}

.md-main__inner {
  background: var(--zen-base);
  border-radius: 16px;
  box-shadow: var(--zen-shadow);
  margin-block: 1.2rem;
}

.md-header,
.md-tabs,
.md-nav,
.md-footer,
.md-search__form,
.md-typeset h1,
.md-typeset h2,
.md-typeset h3,
.md-typeset h4,
.md-typeset .admonition-title,
.md-typeset summary,
.md-typeset table:not([class]) th {
  font-family: var(--md-code-font-family);
}

.md-typeset h1,
.md-typeset h2,
.md-typeset h3,
.md-typeset h4 {
  color: var(--zen-text);
  font-weight: 700;
  letter-spacing: -0.03em;
  line-height: 1.2;
}

.md-typeset > p,
.md-typeset > ul,
.md-typeset > ol,
.md-typeset > blockquote {
  max-width: 70ch;
}

/* Inline rather than flex: a flex heading drops the spaces around inline code,
   which 53 headings here contain. The rule is 100vw wide with an equal
   negative margin, so it takes no room on the line and is clipped at the
   heading's edge. */
.md-typeset h2 {
  overflow: hidden;
}
.md-typeset h2::before {
  content: "──";
  color: var(--zen-accent);
  font-weight: 400;
  margin-right: 0.6rem;
}
.md-typeset h2::after {
  content: "";
  display: inline-block;
  vertical-align: middle;
  width: 100vw;
  height: 2px;
  margin-left: 0.6rem;
  margin-right: -100vw;
  background: repeating-linear-gradient(90deg, var(--zen-surface0) 0 14px, transparent 14px 22px);
}

.md-typeset code {
  border-radius: 6px;
}
.md-typeset pre > code,
.md-typeset .highlight {
  border-radius: 10px;
}

.md-typeset table:not([class]) {
  border: 0;
  border-radius: 10px;
  box-shadow: none;
  overflow: hidden;
}
.md-typeset table:not([class]) th {
  background: var(--zen-mantle);
  color: var(--zen-text);
}

.md-typeset .admonition,
.md-typeset details {
  --zen-adm: var(--zen-accent-text);
  border: 0;
  border-radius: 10px;
  box-shadow: none;
  background: color-mix(in srgb, var(--zen-adm) 6%, var(--zen-base));
}
.md-typeset .tip, .md-typeset .hint, .md-typeset .success, .md-typeset .check, .md-typeset .done {
  --zen-adm: var(--zen-green);
}
.md-typeset .warning, .md-typeset .caution, .md-typeset .attention {
  --zen-adm: var(--zen-yellow);
}
.md-typeset .danger, .md-typeset .error, .md-typeset .failure, .md-typeset .fail, .md-typeset .missing, .md-typeset .bug {
  --zen-adm: var(--zen-red);
}
.md-typeset .admonition > .admonition-title,
.md-typeset details > summary {
  background: transparent;
  color: var(--zen-adm);
  border: 0;
}
.md-typeset .admonition > .admonition-title::before,
.md-typeset details > summary::before {
  background-color: var(--zen-adm);
}

.md-nav__link--active,
.md-nav__link:hover {
  color: var(--zen-accent-text);
}

.md-footer {
  border-radius: 0;
}
```

- [ ] **Step 2: Build, test, and look**

Run: `git add docs/assets/stylesheets/zen.css && nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c sh -c 'go test ./internal/docsgen/theme/ -count=1 && make docs && hack/docs-screenshots.sh /tmp/zen-task3 "" "guides/scaling-and-boosts/" "explanation/network-boundaries/"'`
Expected: tests pass, build exit 0, twelve PNGs.

Read `guides_scaling-and-boosts-mocha-1440.png`, `guides_scaling-and-boosts-latte-1440.png` and `explanation_network-boundaries-latte-390.png`. Check and report on each:
- the page content sits in a rounded base window on a darker canvas;
- `h2` headings show `──` in Peach before the text and a dashed rule after it on the same line, and a heading containing inline code keeps its spaces (network-boundaries has `## \`HostPort\`, Pod Security, and the host firewall`);
- code blocks on mantle with Catppuccin syntax colours;
- body text in IBM Plex Sans, headings and navigation in JetBrains Mono.

- [ ] **Step 3: Commit**

Subject: `feat(docs): the page is a window on a canvas, headings take the zen rule`. Body: why the heading rule is inline and not flex (53 headings with inline code).

---

### Task 4: The bar, the frame, the favicon

**Files:**
- Create: `overrides/main.html`
- Create: `docs/assets/favicon.svg`
- Modify: `mkdocs.yml` (`theme.custom_dir`, `theme.favicon`, `theme.features`)
- Modify: `nix/docs-site.nix` (source set)
- Modify: `docs/assets/stylesheets/zen.css` (append a section)

**Interfaces:**
- Consumes: Task 3's canvas/window arrangement; `--zen-*` tokens.
- Produces: `.zen-frame` element (fixed, pointer-events none) from `overrides/main.html`; header 44px tall containing the workspace tabs. Task 5 layers the wallpaper under `.md-container` and relies on `.zen-frame` staying above it.

- [ ] **Step 1: `overrides/main.html`**

```html
{% extends "base.html" %}

{% block extrahead %}
  <meta name="theme-color" content="#181825" media="(prefers-color-scheme: dark)">
  <meta name="theme-color" content="#e6e9ef" media="(prefers-color-scheme: light)">
{% endblock %}

{% block footer %}
  {{ super() }}
  <div class="zen-frame" aria-hidden="true"><i></i><i></i><i></i><i></i></div>
{% endblock %}
```

- [ ] **Step 2: `docs/assets/favicon.svg`**

```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">
  <rect width="64" height="64" rx="16" fill="#fab387"/>
  <text x="50%" y="54%" text-anchor="middle" dominant-baseline="middle"
        font-family="'JetBrains Mono', 'DejaVu Sans Mono', monospace"
        font-weight="700" font-size="30" fill="#11111b">SP</text>
</svg>
```

- [ ] **Step 3: `mkdocs.yml`**

Under `theme:` directly after `name: material` add:

```yaml
  custom_dir: overrides
  favicon: assets/favicon.svg
```

In `features:` add `navigation.tabs` and `navigation.tabs.sticky` as the first two entries; keep the four existing ones.

- [ ] **Step 4: Take `overrides/` into the site's source set**

In `nix/docs-site.nix` change

```nix
      (lib.fileset.unions [ ../docs ../mkdocs.yml ])
```

to

```nix
      (lib.fileset.unions [ ../docs ../mkdocs.yml ../overrides ])
```

and in the comment at the top of the file change `this one reads docs/ and mkdocs.yml alone` to `this one reads docs/, mkdocs.yml and overrides/ alone`.

- [ ] **Step 5: Append the bar and frame section to `zen.css`**

```css
/* ── bar and frame ── */

:root {
  --zen-frame: 10px;
  --zen-bar-h: 44px;
  --zen-vr: 16px;
}

.md-header {
  display: flex;
  align-items: center;
  height: var(--zen-bar-h);
  background: var(--zen-mantle);
  color: var(--zen-text);
  box-shadow: none;
}

/* Material renders the tabs as the header's second child. Dissolving the
   inner nav's box puts its buttons and the tabs into one row. */
.md-header__inner {
  display: contents;
}
.md-header__inner > * {
  order: 3;
}
.md-header .md-logo,
.md-header__button[for="__drawer"],
.md-header__title {
  order: 1;
}
.md-header .md-tabs {
  order: 2;
}

.md-header__title {
  flex-grow: 0;
  margin-inline: 0.6rem 1rem;
  font-weight: 700;
}
.md-header__title .md-header__topic + .md-header__topic {
  display: none;
}
.md-header__title--active .md-header__topic {
  opacity: 1;
  transform: none;
  pointer-events: auto;
  z-index: auto;
}

@media screen and (min-width: 76.25em) {
  .md-header .md-logo {
    display: none;
  }
}

.md-tabs {
  flex: 1;
  min-width: 0;
  background: transparent;
  color: var(--zen-muted);
}
.md-tabs .md-grid {
  max-width: none;
  margin: 0;
}
.md-tabs__list {
  counter-reset: ws;
  display: flex;
  gap: 4px;
  margin: 0;
  padding: 0;
  overflow-x: auto;
  white-space: nowrap;
  contain: none;
}
.md-tabs__item {
  counter-increment: ws;
  display: block;
  height: auto;
  padding: 0;
}
.md-tabs__link {
  display: inline-flex;
  align-items: center;
  gap: 0.35rem;
  margin: 0;
  padding: 0.15rem 0.45rem;
  border-radius: 8px;
  font-size: 0.64rem;
  color: var(--zen-muted);
  opacity: 1;
  transition: background-color 200ms cubic-bezier(0.45, 0, 0.55, 1);
}
.md-tabs__link::before {
  content: counter(ws);
}
.md-tabs__link:hover,
.md-tabs__link:focus-visible {
  background: var(--zen-surface0);
  color: var(--zen-text);
}
.md-tabs__item--active .md-tabs__link,
.md-tabs__item--active .md-tabs__link:hover {
  background: var(--zen-accent-text);
  color: var(--zen-accent-contrast);
}

@media screen and (max-width: 76.234375em) {
  .md-tabs {
    display: block;
  }
  .md-tabs__link {
    font-size: 0;
    gap: 0;
  }
  .md-tabs__link::before {
    font-size: 0.64rem;
  }
}

.md-container {
  padding-inline: var(--zen-frame);
  padding-bottom: var(--zen-frame);
}

.zen-frame {
  position: fixed;
  inset: 0;
  z-index: 3;
  pointer-events: none;
  border: solid var(--zen-mantle);
  border-width: var(--zen-bar-h) var(--zen-frame) var(--zen-frame);
}
.zen-frame i {
  position: absolute;
  width: var(--zen-vr);
  height: var(--zen-vr);
}
.zen-frame i:nth-child(1) {
  top: 0;
  left: 0;
  background: radial-gradient(circle at 100% 100%, transparent calc(var(--zen-vr) - 0.5px), var(--zen-mantle) var(--zen-vr));
}
.zen-frame i:nth-child(2) {
  top: 0;
  right: 0;
  background: radial-gradient(circle at 0% 100%, transparent calc(var(--zen-vr) - 0.5px), var(--zen-mantle) var(--zen-vr));
}
.zen-frame i:nth-child(3) {
  bottom: 0;
  left: 0;
  background: radial-gradient(circle at 100% 0%, transparent calc(var(--zen-vr) - 0.5px), var(--zen-mantle) var(--zen-vr));
}
.zen-frame i:nth-child(4) {
  bottom: 0;
  right: 0;
  background: radial-gradient(circle at 0% 0%, transparent calc(var(--zen-vr) - 0.5px), var(--zen-mantle) var(--zen-vr));
}
```

- [ ] **Step 6: Verify the structure before the look**

Run: `git add overrides/main.html docs/assets/favicon.svg mkdocs.yml nix/docs-site.nix docs/assets/stylesheets/zen.css && nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs`
Expected: exit 0.

Run: `site=$(nix --extra-experimental-features 'nix-command flakes' build /home/paul/git/spawnery#docs-site --no-link --print-out-paths); python3 -c "import re,sys; h=open('$site/index.html').read(); hd=re.search(r'<header class=\"md-header.*?</header>', h, re.S).group(0); print('tabs in header:', 'md-tabs__list' in hd); print('workspaces:', hd.count('md-tabs__item')); print('frame:', 'class=\"zen-frame\"' in h); print('theme-color:', h.count('name=\"theme-color\"')); print('favicon:', 'assets/favicon.svg' in h)"`
Expected:
```
tabs in header: True
workspaces: 9
frame: True
theme-color: 2
favicon: True
```

- [ ] **Step 7: Look, at both widths**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c hack/docs-screenshots.sh /tmp/zen-task4 "" "guides/scaling-and-boosts/"`

Read `home-mocha-1440.png`, `home-latte-1440.png`, `guides_scaling-and-boosts-mocha-390.png`. Check and report:
- a single 44px mantle bar: `Spawnery`, then workspaces `1 Home … 9 Archive`, then search and the scheme toggle on the right;
- on the guide page, workspace `4 Guides` filled with Peach; on the home page `1 Home`;
- a 10px mantle frame at the left, right and bottom edges with concave inner corners, meeting the bar with no seam;
- at 390px only the numbers `1`–`9` remain in the bar, and the drawer button still shows;
- no content hidden under the frame at 390px.

If the bar is two rows or its buttons overlap, report what the screenshot shows rather than adjusting blindly; Material's header markup is the likeliest cause.

- [ ] **Step 8: Commit**

Subject: `feat(docs): the bar is a workspace switcher, the viewport has the zen frame`. Body: why `navigation.tabs.sticky` rather than a copied header partial; the structure check from Step 6.

---

### Task 5: The wallpaper and the diagram

**Files:**
- Modify: `docs/assets/stylesheets/zen.css` (append a section)

**Interfaces:**
- Consumes: canvas (`body` crust), window (`.md-main__inner`), frame z-index 3 (Task 4).
- Produces: nothing later tasks rely on.

- [ ] **Step 1: Append the section**

```css
/* ── wallpaper and diagram ── */

/* Only beside the page window, where there is room for it; behind long text
   it is noise. The drawing is the homepage's, with the moon in this site's
   accent. */
@media screen and (min-width: 76.25em) {
  body::before {
    content: "";
    position: fixed;
    inset: 0;
    z-index: -1;
    pointer-events: none;
    background-repeat: no-repeat;
    background-position: bottom center;
    background-size: cover;
  }
  body[data-md-color-scheme="mocha"]::before {
    background-image: url("data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 1600 900' preserveAspectRatio='xMidYMax slice'><circle cx='1190' cy='300' r='110' fill='%23fab387' opacity='0.05'/><circle cx='1190' cy='300' r='72' fill='%23fab387' opacity='0.07'/><path d='M0 560 L140 480 L300 545 L470 430 L640 530 L820 455 L990 540 L1160 470 L1330 550 L1470 500 L1600 555 L1600 900 L0 900 Z' fill='%231c2030' opacity='0.55'/><path d='M0 640 L180 560 L340 625 L520 540 L720 630 L900 560 L1090 645 L1280 575 L1440 640 L1600 590 L1600 900 L0 900 Z' fill='%23181d29' opacity='0.8'/><path d='M0 730 L200 655 L380 715 L580 640 L790 725 L1000 660 L1210 730 L1420 670 L1600 720 L1600 900 L0 900 Z' fill='%23131622'/><path d='M0 810 L240 750 L460 800 L700 740 L950 805 L1200 750 L1430 800 L1600 765 L1600 900 L0 900 Z' fill='%230d0d15'/></svg>");
  }
  body[data-md-color-scheme="latte"]::before {
    background-image: url("data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 1600 900' preserveAspectRatio='xMidYMax slice'><circle cx='1190' cy='300' r='110' fill='%23fe640b' opacity='0.06'/><circle cx='1190' cy='300' r='72' fill='%23fe640b' opacity='0.08'/><path d='M0 560 L140 480 L300 545 L470 430 L640 530 L820 455 L990 540 L1160 470 L1330 550 L1470 500 L1600 555 L1600 900 L0 900 Z' fill='%23d4d8e1' opacity='0.7'/><path d='M0 640 L180 560 L340 625 L520 540 L720 630 L900 560 L1090 645 L1280 575 L1440 640 L1600 590 L1600 900 L0 900 Z' fill='%23cbd0da' opacity='0.85'/><path d='M0 730 L200 655 L380 715 L580 640 L790 725 L1000 660 L1210 730 L1420 670 L1600 720 L1600 900 L0 900 Z' fill='%23c2c7d2'/><path d='M0 810 L240 750 L460 800 L700 740 L950 805 L1200 750 L1430 800 L1600 765 L1600 900 L0 900 Z' fill='%23b9bfcb'/></svg>");
  }
}

/* mermaid inlines its own theme into each SVG, under an id selector and in
   style attributes; only !important reaches past both, and it is what lets
   one rendered diagram follow the scheme toggle. */
.mermaid svg .node rect,
.mermaid svg .node circle,
.mermaid svg .node polygon,
.mermaid svg .node path {
  fill: var(--zen-mantle) !important;
  stroke: var(--zen-accent-text) !important;
}
.mermaid svg .cluster rect {
  fill: transparent !important;
  stroke: var(--zen-surface1) !important;
  stroke-dasharray: 6 4;
}
.mermaid svg text,
.mermaid svg .nodeLabel,
.mermaid svg .cluster-label,
.mermaid svg .edgeLabel,
.mermaid svg .label {
  color: var(--zen-text) !important;
  fill: var(--zen-text) !important;
  font-family: var(--md-code-font-family) !important;
}
.mermaid svg .flowchart-link,
.mermaid svg .edgePath path {
  stroke: var(--zen-muted) !important;
}
.mermaid svg marker path {
  fill: var(--zen-muted) !important;
  stroke: var(--zen-muted) !important;
}
.mermaid svg .edgeLabel,
.mermaid svg .labelBkg,
.mermaid svg .edgeLabel rect {
  background-color: var(--zen-base) !important;
  fill: var(--zen-base) !important;
}
```

- [ ] **Step 2: Build, test, look**

Run: `git add docs/assets/stylesheets/zen.css && nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c sh -c 'go test ./internal/docsgen/theme/ -count=1 && make docs && hack/docs-screenshots.sh /tmp/zen-task5 ""'`

Read `home-mocha-1440.png` and `home-latte-1440.png`. Check and report:
- ridgelines visible in the side margins beside the page window, a faint Peach moon at upper right; nothing of the wallpaper behind the text;
- the home page diagram: nodes outlined in Peach on mantle, labels legible, edges and arrowheads in the muted colour, the dashed cluster outline — in **both** schemes;
- `home-mocha-390.png`: no wallpaper.

If the diagram is not rendered in the screenshot at all (mermaid runs after load), rerun with `--virtual-time-budget=10000` by editing that single value in `hack/docs-screenshots.sh`, and say so in the report.

- [ ] **Step 3: Commit**

Subject: `feat(docs): the wallpaper beside the page, and a diagram that follows the scheme`. Body: why `!important` in the mermaid rules; why the wallpaper stops below 1220px.

---

### Task 6: Preview for Paul (controller)

Not a subagent task: publishing an artifact is only available to the main session.

**Files:** none committed.

- [ ] **Step 1: Full verification**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c sh -c 'make docs && go test ./internal/docsgen/theme/ -count=1 && hack/image-tag-pins-agree.sh'`
Expected: all exit 0.

- [ ] **Step 2: Assemble the preview subset**

From the built site copy into a scratch directory, preserving paths: `index.html`, `tutorial/index.html`, `guides/scaling-and-boosts/index.html`, `reference/crds/index.html`, `assets/stylesheets/` (all), `assets/javascripts/bundle.*.min.js`, `assets/javascripts/workers/search.*.min.js`, `assets/fonts/` (all 12), `assets/favicon.svg`, `assets/mermaid.min.js`, `search/search_index.json`. Count the files; it must stay under 255.

- [ ] **Step 3: Publish and hand over**

Publish `index.html` as a multi-file artifact with the rest mapped at their relative paths (`files` map, `root` at the scratch directory). Tell Paul the link, that links to pages outside the four published ones will 404 in the preview, and what to look at: both schemes via the toggle, the bar at phone width, the diagram on the home page.

---

## Self-review

- **Spec coverage.** Two schemes with toggle and system preference: Task 2. Peach, two-token accent, selection and focus ring on accent-text: Global Constraints + Task 2. Syntax colours: Task 2. Admonitions semantic, no border: Task 3. Diagram on both schemes: Task 5. `theme-color` and favicon: Task 4. Frame: Task 4. Bar with nine workspaces, numbers only on narrow screens: Task 4. Sidebar restyled: Tasks 2 (variables) and 3 (active link). Wallpaper from 1220px with a Latte variant: Task 5. No reveal animation: nothing adds one. Fonts self-hosted, `font: false`: Tasks 1, 2. `h2` form, radii, reading width: Task 3. Build: `overrides/` in source set (Task 4), fonts gitignored and vendored (Task 1), `zen.css` in `extra_css` (Task 2). Contrast test on both surfaces including syntax and semantic tokens and the fill pair: Task 2. Preview before merge: Task 6.
- **Placeholders.** None; every step carries its code or command and its expected output.
- **Names.** Token names are defined once in Global Constraints and Task 2 and used unchanged in Tasks 3–5. Screenshot file names follow Task 2 Step 7's `<page>-<scheme>-<width>.png` with `/` replaced by `_` and the trailing slash dropped, which is what Tasks 3–5 read.
