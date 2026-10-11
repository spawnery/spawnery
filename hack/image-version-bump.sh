#!/usr/bin/env bash
# Moves imageVersion in flake.nix to the next release's number and the image
# tags and spawnery-api coordinates in the files hack/image-tag-pins-agree.sh
# checks along with it, plus the e2e manifests that hack/e2e-{ondemand,worldsync}.sh
# check themselves (the lint cannot: they pin a Velocity tag that must not
# exist). pin-bump.yml runs it after a Paper or Purpur pin moved.
#
# An imageVersion no v* tag names yet already belongs to the coming release
# and stays. Otherwise it joins a release another bump has already numbered
# (an operatorVersion or chart version above every tag), or takes the patch
# after the highest tag: a new upstream build is a fix in SemVer terms.
#
# Usage:
#   hack/image-version-bump.sh
#
# Environment, for hack/image-version-bump-test.sh:
#   ROOT  the tree to rewrite (default: this repository)
#   TAGS  the released tags, one per line (default: git tag --list 'v*')
#
# Exit status: 0 imageVersion names an unreleased number, 1 a refusal.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
root="${ROOT:-$here}"
flake="$root/flake.nix"
chart="$root/charts/spawnery/Chart.yaml"

fail() { echo "image-version-bump: $*" >&2; exit 1; }

read_flake() {
	sed -n "s/^[[:space:]]*$1 = \"\([^\"]*\)\";/\1/p" "$flake"
}

image="$(read_flake imageVersion)"
operator="$(read_flake operatorVersion)"
chart_version="$(sed -n 's/^version: *//p' "$chart" | head -1)"
[ -n "$image" ] && [ -n "$operator" ] && [ -n "$chart_version" ] ||
	fail "could not read imageVersion, operatorVersion or the chart version"

if [ -z "${TAGS+x}" ]; then
	TAGS="$(git -C "$root" tag --list 'v*')"
fi
released="$(sed -n 's/^v//p' <<<"$TAGS" | sort -V)"
[ -n "$released" ] || fail "no v* tags to number the next release from"
highest="$(tail -1 <<<"$released")"

is_released() { grep -qxF "$1" <<<"$released"; }

if ! is_released "$image"; then
	echo "imageVersion $image is not released yet; it stays"
	exit 0
fi

next=""
for v in "$operator" "$chart_version"; do
	if ! is_released "$v" && [ "$(printf '%s\n%s\n' "$highest" "$v" | sort -V | tail -1)" = "$v" ]; then
		next="$(printf '%s\n%s\n' "${next:-0}" "$v" | sort -V | tail -1)"
	fi
done
if [ -z "$next" ]; then
	IFS=. read -r major minor patch <<<"$highest"
	next="$major.$minor.$((patch + 1))"
fi

old="${image//./\\.}"
sed -i "s/^\([[:space:]]*imageVersion = \"\)$old\";/\1$next\";/" "$flake"
[ "$(read_flake imageVersion)" = "$next" ] || fail "imageVersion in $flake did not take $next"

while IFS= read -r manifest; do
	file="$root/${manifest#"$here"/}"
	[ -w "$file" ] || fail "cannot write $file"
	sed -i -E \
		-e "s#((purpur|velocity):[^[:space:]]*-)$old([^0-9.]|\$)#\1$next\3#g" \
		-e "s#(cloud\.spawnery:spawnery-api:)$old([^0-9.]|\$)#\1$next\2#g" \
		"$file"
done < <("$here/hack/image-tag-pins-agree.sh" --list
	printf '%s\n' "$here/test/e2e/manifests/ondemand.yaml" "$here/test/e2e/manifests/worldsync.yaml")

echo "imageVersion $image -> $next"
