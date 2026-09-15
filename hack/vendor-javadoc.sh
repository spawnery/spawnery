#!/usr/bin/env bash
# Puts the plugin API's Javadoc where mkdocs.yml's nav expects it, so
# `mkdocs serve` has something for that link to resolve to locally.
#
# Deliberately not committed: it is a build product, and
# nix/agent-api-javadoc.nix is where it is pinned. nix/docs-site.nix takes the
# same derivation as an argument, so a built site needs this script for
# nothing.
set -euo pipefail

cd "$(dirname "$0")/.."

out="$(nix build --no-link --print-out-paths .#agent-api-javadoc)"

rm -rf docs/plugin-api/javadoc
mkdir -p docs/plugin-api/javadoc
# The store path is read-only; without --no-preserve=mode the copy would be
# too, and the next run's `rm -rf` above would fail on its own output.
# GNU-only, but this script only ever runs through `nix develop -c`, which
# always supplies GNU cp.
cp -r --no-preserve=mode "$out"/. docs/plugin-api/javadoc/
