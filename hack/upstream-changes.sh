#!/usr/bin/env bash
# Prints, as Markdown, the upstream commits between two Paper or Purpur
# builds of one Minecraft version: every build after FROM up to and including
# TO. pin-bump.yml puts it into the pull request it opens.
#
# Usage:
#   hack/upstream-changes.sh paper 26.3 135 174
#   hack/upstream-changes.sh purpur 26.3 2642 2647
set -euo pipefail

fail() { echo "upstream-changes: $*" >&2; exit 1; }

[ $# -eq 4 ] || fail "usage: $0 paper|purpur VERSION FROM TO"
project="$1" version="$2" from="$3" to="$4"

case "$project" in
paper)
	curl -fsS "https://fill.papermc.io/v3/projects/paper/versions/$version/builds" |
		jq -r --argjson from "$from" --argjson to "$to" '
			map(select(.id > $from and .id <= $to)) | sort_by(.id) | .[]
			| "- build \(.id) (\(.channel)): " + ([.commits[]? | .message | split("\n")[0]] | join("; "))'
	;;
purpur)
	builds="$(curl -fsS "https://api.purpurmc.org/v2/purpur/$version" |
		jq -r --argjson from "$from" --argjson to "$to" \
			'.builds.all[] | tonumber | select(. > $from and . <= $to)')"
	for build in $builds; do
		curl -fsS "https://api.purpurmc.org/v2/purpur/$version/$build" |
			jq -r '"- build \(.build): " + ([.commits[]? | .description | split("\n")
				| map(select(length > 0)) | .[0] + (if length > 1 then "\n" + (.[1:] | map("  - " + .) | join("\n")) else "" end)]
				| join("\n"))'
	done
	;;
*) fail "unknown project $project; paper or purpur" ;;
esac
