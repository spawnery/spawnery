#!/usr/bin/env bash
# Prints, as Markdown, the upstream commits between two Paper, Purpur or
# Canvas builds of one Minecraft version: every build after FROM up to and
# including TO. pin-bump.yml puts it into the pull request it opens.
#
# Usage:
#   hack/upstream-changes.sh paper 26.3 135 174
#   hack/upstream-changes.sh purpur 26.3 2642 2647
#   hack/upstream-changes.sh canvas 26.3 990 993
set -euo pipefail

fail() { echo "upstream-changes: $*" >&2; exit 1; }

[ $# -eq 4 ] || fail "usage: $0 paper|purpur|canvas VERSION FROM TO"
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
canvas)
	curl -fsSL "${API:-https://canvasmc.io/api/v2/builds?project=canvas}" |
		jq -r --arg v "$version" --argjson from "$from" --argjson to "$to" '
			.builds | map(select(.channelVersion == $v and .buildNumber > $from and .buildNumber <= $to))
			| sort_by(.buildNumber) | .[]
			| "- build \(.buildNumber): " + ([.commits[]? | .message | split("\n")[0]] | join("; "))'
	;;
*) fail "unknown project $project; paper, purpur or canvas" ;;
esac
