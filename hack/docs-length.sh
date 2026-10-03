#!/usr/bin/env bash
# Refuses a documentation page that has grown past its word ceiling.
#
# Each ceiling is roughly 12% above the page's length after the 2026-09-17
# rewrite, so a paragraph with something to say still fits. Generated pages
# are absent: their length is their sources' business.
#
# Usage: hack/docs-length.sh [--page FILE:CEILING]...
#
# With no --page, checks the table below.
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
