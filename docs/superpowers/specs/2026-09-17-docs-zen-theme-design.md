# The documentation site in the Zen style

`docs.spawnery.cloud` takes on the visual system of `paul.wtf`, the way
`tidalwave.paul.wtf` and `linkhop.paul.wtf` already do, with an accent of its
own.

Decided with Paul on 2026-09-17. It follows the content work of
`2026-09-15-documentation-show-before-explain-design.md` (PR #54) and comes
before the shortening that spec left open. Nothing here changes a word of
content, a page's place in the nav, or the build's gate.

## Where the style comes from

The source is `~/git/homepage`, not a description of it:
`src/styles/tokens.css` for the tokens and `src/layouts/Layout.astro` for the
frame. The homepage calls the system the "zen" variant of Paul's
quickshell/Hyprland desktop theme: opaque Catppuccin surfaces, no borders, one
accent, JetBrains Mono, a mantle frame around the viewport with concave
corners, and the navigation as a numbered workspace switcher.

tidalwave and linkhop each carry a port of the same tokens
(`frontend/src/lib/theme/tokens.css`). The site follows the homepage's names
and values where they differ; it does not introduce a fourth variant.

## Decisions

**The accent is Peach.** Every project except the homepage gets its own
Catppuccin accent so the family stays recognisable and the projects stay
distinguishable: Spawnery Peach, tidalwave Mauve, linkhop Blue; the homepage
stays Teal and Sky. Green and Red are reserved for status and for critical
things, Sky and Sapphire sit too close to Teal, Maroon and Flamingo too close to
Red, and Yellow reads as a warning. Chosen on a comparison page that previewed
each candidate on both schemes with its measured contrast.

**Mocha and Latte, with a toggle** that also follows the system preference.
tidalwave and linkhop already offer both; a documentation site with pages of
several thousand words is read in daylight too. This replaces today's
Material default/slate pair.

**Running text is proportional.** JetBrains Mono carries the bar, the
navigation, headings, code and labels; **IBM Plex Sans** carries running
text. The homepage sets everything in mono and can, because its texts are
short; a page of 4,500 words in mono reads measurably slower, which works
against the complaint that started the content work.

**Material for MkDocs stays**, overridden rather than replaced. A theme of our
own would rebuild search, navigation, code copy, the table of contents and the
scheme toggle; a move to Astro Starlight would migrate a toolchain the Nix
build, `--strict`, the mermaid plugin and the generated reference are all built
around. The cost of staying is that the frame and the bar live inside
Material's DOM, and some rules target its class names.

## Colour

Two custom schemes, `mocha` and `latte`, map the homepage tokens onto
Material's `--md-*` variables: `--md-default-bg-color` from base,
`--md-default-fg-color` from text, and so on. The mapping is one table in one
stylesheet; nothing else in the site sets a colour literal.

### Two accent tokens, because Latte fails otherwise

Measured as link text against each scheme's base (`#1e1e2e`, `#eff1f5`), with
AA requiring 4.5:1:

| | Mocha | Latte, raw |
|---|---:|---:|
| Peach | 9.27 | **2.64** |
| Mauve | 8.07 | 4.79 |
| Blue | 7.79 | **4.34** |
| Teal, as tidalwave and linkhop use it today | 11.01 | **3.31** |

Catppuccin's Latte accents are drawn for surfaces, not for text. So the accent
is two tokens:

- `--accent` for decoration that carries no text: the `──` before a heading,
  the selection, the focus ring, the dashed rule after a section title.
- `--accent-text` for everything read: links, buttons, the active workspace.
  Text on a surface filled with it is `--accent-contrast`.

| | `--accent` | `--accent-text` | `--accent-contrast` |
|---|---|---|---|
| Mocha | `#fab387` | `#fab387` | crust `#11111b` |
| Latte | `#fe640b` | `#bc4501` (4.65:1) | base `#eff1f5` |

`#bc4501` is Latte Peach's own hue with its lightness lowered until the ratio
reaches 4.6:1, not a different colour.

### Everything else that has a colour

- **Code.** Material's twelve `--md-code-hl-*` variables take Catppuccin's
  syntax colours for each scheme. Code blocks sit on mantle.
- **Admonitions** stay semantic and quiet: `tip` Green, `warning` Yellow,
  `danger` Red, `note` the accent, its title in `--accent-text`. A
  low-contrast fill and the coloured title; no border and no stripe down the
  left edge.
- **The diagram** on the home page must read on both schemes. It is the only
  mermaid diagram on the site; the preview below is where that is checked.
- `theme-color` is mantle, and the favicon is Mocha Peach `#fab387` on a
  rounded square, as the homepage's is Teal.

## Frame and bar

Template overrides live in `overrides/`, set as `theme.custom_dir`.

- **The frame** is the homepage's: a fixed mantle border around the viewport
  with four concave corners drawn as radial gradients, taken from
  `Layout.astro`. It ignores pointer events and never covers content.
- **The bar** is Material's header, restyled to the homepage's 44 px of
  mantle with the brand on the left. It replaces the frame's top edge, as on
  the desktop.
- **The nine top-level sections become numbered workspaces 1-9** in the bar:
  `navigation.tabs`, moved into the header by overriding `partials/header.html`
  and `partials/tabs.html`. The active one is filled with `--accent-text`. Nine
  is what the nav has today: Home, Getting started, Tutorial, Guides,
  Reference, Plugin API, Explanation, Contributing, Archive. On narrow screens
  only the numbers remain, as on the homepage.
- **The left sidebar** keeps the pages within the active section, restyled to
  the tokens.
- **The wallpaper**, the homepage's ridgelines and moon, shows only in the
  margins beside the reading surface, from Material's 1220 px breakpoint up;
  below that it is absent, because behind long text it is noise. Latte gets a
  light variant of the same drawing.
- **No reveal animation.** The homepage fades sections in; a documentation
  page is read from the first frame and stays visible at rest. Colour
  transitions of 200 ms remain.

The existing features stay: `navigation.sections`, `navigation.top`,
`content.code.copy`, `search.suggest`.

## Type

- JetBrains Mono (variable) and IBM Plex Sans at 400, 500, 600 and 400 italic.
- `h2` takes the homepage's section-title form: `──` in `--accent` before it
  and a dashed surface rule after it. The heading's own capitalisation is left
  as written; only the workspace labels in the bar are lowercase, as on the
  homepage.
- Radii 16 for panels, 10 for nested elements and buttons, 8 for chips.
- Running text is held near 70 characters.

**Fonts are served by the site itself.** `theme.font: false` stops Material
loading Google Fonts. `nix/fonts.nix` fetches the two Fontsource npm tarballs
by pinned hash and extracts the latin and latin-ext `woff2` files, exactly as
`nix/mermaid.nix` fetches mermaid, for the reason that file gives: a site served
from one's own cluster should not depend on somebody else's. The homepage
loads JetBrains Mono from the same Fontsource package.

## Build

- `nix/docs-site.nix` takes `overrides/` into its source set, which today is
  `docs/` and `mkdocs.yml` only, and installs the fonts in `postPatch` beside
  mermaid.js.
- `docs/assets/fonts/` is gitignored like `docs/assets/mermaid.min.js`, and
  `hack/vendor-fonts.sh` joins `make docs-assets` so `mkdocs serve` finds them.
- The stylesheet is `docs/assets/stylesheets/zen.css`, listed in `extra_css`.

## Verification

- **`mkdocs build --strict` stays the gate**, unchanged.
- **A contrast test guards the tokens.** A Go test reads the custom properties
  of both schemes out of `zen.css` and fails when a named text/surface pair
  falls below 4.5:1: text on base, muted text on base, text on mantle,
  `--accent-text` on base, `--accent-contrast` on `--accent-text`. It is a
  source-reading test in the repository's existing habit, and it runs in
  `make test`. Without it, the Latte finding above returns the first time
  somebody adjusts a colour by eye.
- **Paul sees it before it goes live.** He cannot open a local preview from
  where he works, so the built site's home page, the tutorial, one guide and one
  reference page are published as a private artifact, with their stylesheet and
  fonts, before the branch is offered for merge. The diagram on the home page is
  checked there on both schemes.

## What this does not do

- **It does not change content.** Not a word, not the nav order.
- **It does not change tidalwave or linkhop.** Their accents move to Mauve and
  Blue in their own repositories, afterwards: two `--accent` lines and a
  favicon each, plus the same two-token split, which also lifts their Latte
  links above AA.
- **It does not extract a shared token package.** With this there are four
  copies of the Zen tokens, and tidalwave's and linkhop's already differ in
  shape from the homepage's. Worth doing once a fifth appears; not needed to
  ship this.

## Risk

The overrides depend on Material's template partials and class names, and
Material moves with `nix flake update`, not with a pin of its own. A Material
release that renames a partial breaks the build loudly; one that renames a
class breaks the look silently, and `--strict` will not notice. The contrast
test catches colour regressions only. The mitigation is that the overrides stay
small, name the partials they replace in one place, and that a flake update
touching `mkdocs-material` gets the same preview step as this spec.
