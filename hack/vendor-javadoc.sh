#!/usr/bin/env bash
# Puts the plugin API's Javadoc where mkdocs.yml's nav expects it, for
# `mkdocs serve`. Not committed: nix/agent-api-javadoc.nix builds it.
set -euo pipefail

cd "$(dirname "$0")/.."

out="$(nix build --no-link --print-out-paths .#agent-api-javadoc)"

rm -rf docs/plugin-api/javadoc
mkdir -p docs/plugin-api/javadoc
# Without --no-preserve=mode the copy inherits the store's read-only mode and
# the next run's `rm -rf` fails.
cp -r --no-preserve=mode "$out"/. docs/plugin-api/javadoc/
