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
