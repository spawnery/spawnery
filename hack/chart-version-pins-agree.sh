#!/usr/bin/env bash
# Refuses a `helm install`/`helm upgrade` of the spawnery chart whose
# --version differs from charts/spawnery/Chart.yaml.
#
# The install lines in the READMEs and the docs name the chart version by
# hand, and a stale one still installs cleanly: an old chart, with no error.
# hack/image-tag-pins-agree.sh does the same for image tags.
#
# Not checked: docs/archive/ and docs/superpowers/, where an old version
# records what was true then. Only commands naming charts/spawnery count, so
# another chart's --version is left alone; a command continued over several
# lines with a trailing backslash is read as one.
#
# Usage:
#   hack/chart-version-pins-agree.sh [--chart FILE] [--chart-version VERSION]
#                                    [--manifest FILE]...
#
# With no --manifest, checks README.md, charts/spawnery/README.md and every
# Markdown file under docs/.
#
# Exit status: 0 every --version agrees, 1 one does not or something could
# not be read.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="$root/charts/spawnery/Chart.yaml"
chart_version=""
manifests=()

while [ $# -gt 0 ]; do
  case "$1" in
    --chart) chart="$2"; shift 2 ;;
    --chart-version) chart_version="$2"; shift 2 ;;
    --manifest) manifests+=("$2"); shift 2 ;;
    *) echo "chart-version-pins-agree: unknown argument $1" >&2; exit 1 ;;
  esac
done

fail() { echo "chart-version-pins-agree: $*" >&2; exit 1; }

if [ "${#manifests[@]}" -eq 0 ]; then
  manifests=("$root/README.md" "$root/charts/spawnery/README.md")
  while IFS= read -r f; do
    manifests+=("$f")
  done < <(find "$root/docs" \( -path "$root/docs/archive" -o -path "$root/docs/superpowers" \) -prune \
    -o -name '*.md' -print | sort)
fi

if [ -z "$chart_version" ]; then
  [ -r "$chart" ] || fail "cannot read $chart"
  chart_version="$(sed -n 's/^version:[[:space:]]*"\{0,1\}\([^"[:space:]]*\)"\{0,1\}[[:space:]]*$/\1/p' "$chart" | head -1)"
  [ -n "$chart_version" ] || fail "cannot read version out of $chart"
fi

bad=0
found_any=0
for manifest in "${manifests[@]}"; do
  [ -r "$manifest" ] || fail "cannot read $manifest"
  while IFS=: read -r line got; do
    found_any=1
    if [ "$got" != "$chart_version" ]; then
      echo "chart-version-pins-agree: $manifest:$line installs the chart with --version $got;" \
        "Chart.yaml is $chart_version" >&2
      bad=1
    fi
  done < <(awk '
    function flush() {
      if (cmd ~ /helm[[:space:]]+(install|upgrade)/ && cmd ~ /charts\/spawnery/) {
        rest = cmd
        while (match(rest, /--version[[:space:]=]+[^[:space:]\\]+/)) {
          v = substr(rest, RSTART, RLENGTH)
          sub(/--version[[:space:]=]+/, "", v)
          print start ":" v
          rest = substr(rest, RSTART + RLENGTH)
        }
      }
      cmd = ""
    }
    {
      if (cmd == "") start = FNR
      cmd = cmd " " $0
      if ($0 !~ /\\[[:space:]]*$/) flush()
    }
    END { flush() }
  ' "$manifest")
done

[ "$found_any" -eq 1 ] ||
  fail "none of the given files installs charts/spawnery with --version; this check would pass vacuously"

if [ "$bad" -ne 0 ]; then
  echo "chart-version-pins-agree: move the lines above to --version $chart_version." >&2
  exit 1
fi
