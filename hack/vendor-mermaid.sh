#!/usr/bin/env bash
# Puts mermaid.min.js where mkdocs.yml expects it, so `mkdocs serve` renders a
# diagram locally.
#
# Deliberately not committed: it is a dependency, and nix/mermaid.nix is where
# it is pinned. nix/docs-site.nix takes the same derivation as an argument, so
# a built site needs this script for nothing.
set -euo pipefail

cd "$(dirname "$0")/.."

out="$(nix build --no-link --print-out-paths .#mermaid-js)"

mkdir -p docs/assets
install -m 644 "$out/mermaid.min.js" docs/assets/mermaid.min.js
