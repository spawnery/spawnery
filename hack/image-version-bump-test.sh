#!/usr/bin/env bash
# Drives hack/image-version-bump.sh over copies of this tree's files.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bump="$here/hack/image-version-bump.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

failures=0
ok() { echo "ok   $1"; }
bad() {
	echo "FAIL $1"
	[ -n "${2:-}" ] && printf '%s\n' "$2" | sed 's/^/    /'
	failures=$((failures + 1))
}

# A tree whose flake carries image $1 and operator $2 and whose chart is $3.
tree() {
	rm -rf "$tmp/tree"
	mkdir -p "$tmp/tree/charts/spawnery"
	sed -e "s/^\([[:space:]]*imageVersion = \"\)[^\"]*\";/\1$1\";/" \
		-e "s/^\([[:space:]]*operatorVersion = \"\)[^\"]*\";/\1$2\";/" \
		"$here/flake.nix" >"$tmp/tree/flake.nix"
	printf 'apiVersion: v2\nname: spawnery\nversion: %s\nappVersion: "%s"\n' "$3" "$2" \
		>"$tmp/tree/charts/spawnery/Chart.yaml"
	while IFS= read -r manifest; do
		rel="${manifest#"$here"/}"
		mkdir -p "$tmp/tree/$(dirname "$rel")"
		sed -E -e "s#((purpur|canvas|velocity):[^[:space:]]*-)[0-9]+\.[0-9]+\.[0-9]+#\1$1#g" \
			-e "s#(cloud\.spawnery:spawnery-api:)[0-9]+\.[0-9]+\.[0-9]+#\1$1#g" \
			"$manifest" >"$tmp/tree/$rel"
	done < <("$here/hack/image-tag-pins-agree.sh" --list
		printf '%s\n' "$here/test/e2e/manifests/ondemand.yaml" "$here/test/e2e/manifests/worldsync.yaml")
	printf 'image: ghcr.io/spawnery/canvas:26.3-%s\n' "$1" >>"$tmp/tree/docs/tutorial/network.yaml"
}

image_of() { sed -n 's/^[[:space:]]*imageVersion = "\([^"]*\)";/\1/p' "$tmp/tree/flake.nix"; }

expect() {
	local name="$1" want="$2" tags="$3"
	local out
	if ! out="$(ROOT="$tmp/tree" TAGS="$tags" "$bump" 2>&1)"; then
		bad "$name: the bump refused" "$out"
		return
	fi
	if [ "$(image_of)" = "$want" ]; then ok "$name"; else bad "$name: imageVersion $(image_of), want $want" "$out"; fi
}

tags=$'v0.25.1\nv0.26.0\nv0.26.1\nv0.9.0'

tree 0.26.1 0.26.0 0.26.0
expect "a released imageVersion takes the patch after the highest tag" 0.26.2 "$tags"

if grep -rqE 'purpur:26\.3-0\.26\.2' "$tmp/tree/docs/tutorial/network.yaml" &&
	grep -qF 'spawnery-api:0.26.2' "$tmp/tree/agent/api/README.md" &&
	grep -qF 'purpur:26.3-0.26.2' "$tmp/tree/test/e2e/manifests/worldsync.yaml" &&
	grep -qF 'velocity:e2e-no-such-tag' "$tmp/tree/test/e2e/manifests/worldsync.yaml" &&
	grep -qF 'canvas:26.3-0.26.2' "$tmp/tree/docs/tutorial/network.yaml" &&
	! grep -rqE '(purpur|canvas|velocity):[^[:space:]]*-0\.26\.1|spawnery-api:0\.26\.1' "$tmp/tree/docs" "$tmp/tree/config" "$tmp/tree/agent" "$tmp/tree/test"; then
	ok "the image tags and the api coordinate move with it"
else
	bad "the image tags and the api coordinate move with it" "$(grep -rnE '0\.26\.[12]' "$tmp/tree/docs" "$tmp/tree/config" "$tmp/tree/agent" | head)"
fi

tree 0.26.1 0.27.0 0.26.0
expect "it joins an operator release already numbered" 0.27.0 "$tags"

tree 0.26.1 0.26.0 0.28.0
expect "it joins a chart release already numbered" 0.28.0 "$tags"

tree 0.26.2 0.26.0 0.26.0
expect "an unreleased imageVersion stays" 0.26.2 "$tags"

tree 0.26.1 0.24.9 0.24.9
expect "an operator version below the highest tag is no release to join" 0.26.2 $'v0.24.8\nv0.26.1'

if [ "$failures" -ne 0 ]; then
	echo "$failures failed"
	exit 1
fi
