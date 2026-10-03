#!/usr/bin/env bash
# Puts the site's fonts where docs/assets/stylesheets/zen.css expects them, for
# `mkdocs serve`. Not committed: nix/fonts.nix pins them.
set -euo pipefail

cd "$(dirname "$0")/.."

out="$(nix build --no-link --print-out-paths .#docs-fonts)"

mkdir -p docs/assets/fonts
install -m 644 "$out"/*.woff2 docs/assets/fonts/
