#!/usr/bin/env bash
# Puts mermaid.min.js where mkdocs.yml expects it, for `mkdocs serve`.
# Not committed: nix/mermaid.nix pins it, and a built site does not need this.
set -euo pipefail

cd "$(dirname "$0")/.."

out="$(nix build --no-link --print-out-paths .#mermaid-js)"

mkdir -p docs/assets
install -m 644 "$out/mermaid.min.js" docs/assets/mermaid.min.js
