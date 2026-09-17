#!/usr/bin/env bash
# Refuses a documentation page that has grown past the size its rewrite
# landed on.
#
# The six pages below were 25,829 words on 2026-09-17 and 10,740 after
# docs/superpowers/specs/2026-09-17-docs-shortening-design.md. They had grown
# there once already, which is why a check exists at all rather than a note
# saying to keep them short.
#
# A ceiling is not a judgement about a page: it is roughly 12% above what its
# rewrite measured, so a paragraph with something to say fits without a
# fight. Three pages in this same rewrite finished within seven words of an
# earlier, tighter ceiling, and one of them dropped a sentence worth keeping
# to get there -- that is the failure this headroom exists to prevent.
# Raising a ceiling is a line here and a sentence in the commit saying what
# the page gained. Generated pages are deliberately absent -- their length is
# their sources' business.
#
# Usage: hack/docs-length.sh [--page FILE:CEILING]...
#
# With no --page, checks the table below. --page is for
# hack/docs-length-test.sh, which has to drive a failure this tree does not
# carry.
#
# Exit status: 0 every page is at or under its ceiling, 1 one is over or a
# named file does not exist.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PAGES=(
  "docs/guides/upgrading.md:950"
  "docs/getting-started/index.md:1000"
  "docs/guides/rotating-the-forwarding-secret.md:1800"
  "docs/guides/rotating-the-ca.md:1400"
  "docs/explanation/network-boundaries.md:3900"
  "docs/contributing/development.md:3000"
)

if [ $# -gt 0 ]; then
  PAGES=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --page) PAGES+=("$2"); shift 2 ;;
      *) echo "docs-length: unknown argument $1" >&2; exit 1 ;;
    esac
  done
fi

bad=0
for entry in "${PAGES[@]}"; do
  file="${entry%:*}"
  ceiling="${entry##*:}"
  path="$file"
  [ "${file#/}" = "$file" ] && path="$root/$file"
  if [ ! -r "$path" ]; then
    echo "docs-length: $file does not exist" >&2
    bad=1
    continue
  fi
  count="$(wc -w < "$path")"
  if [ "$count" -gt "$ceiling" ]; then
    echo "$file: $count words, ceiling $ceiling" >&2
    bad=1
  fi
done

exit "$bad"
