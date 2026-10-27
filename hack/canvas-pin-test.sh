#!/usr/bin/env bash
# Drives hack/canvas-pin.sh against a recorded listing and two small jars, so
# no run downloads Canvas.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin="$root/hack/canvas-pin.sh"
fixture="$root/hack/testdata/canvas-builds.json"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cd "$root"

paper_mojang="$(sed -n 's|^    url = "\(https://piston-data.mojang.com/[^"]*\)";$|\1|p' nix/paper.nix)"
[ -n "$paper_mojang" ] || { echo "cannot read the Mojang URL out of nix/paper.nix"; exit 1; }

mkjar() {
	local name="$1" url="$2"
	mkdir -p "$tmp/$name/META-INF"
	printf '%s\t%s\tmojang_26.3.jar\n' "$(printf x | sha256sum | cut -d' ' -f1)" "$url" \
		>"$tmp/$name/META-INF/download-context"
	(cd "$tmp/$name" && jar cf "$tmp/$name.jar" META-INF)
}
mkjar good "$paper_mojang"
mkjar other "https://piston-data.mojang.com/v1/objects/0000000000000000000000000000000000000000/server.jar"

listing() {
	sed -E "s#https://jenkins\.canvasmc\.io/job/Canvas/[0-9]+/artifact/[^\"]*#file://$tmp/$1.jar#" \
		"$fixture" >"$tmp/$1.json"
	echo "file://$tmp/$1.json"
}
good_api="$(listing good)"
other_api="$(listing other)"

nix_copy() {
	sed -e 's/^  canvasBuild = ".*";$/  canvasBuild = "990";/' \
		-e 's/^    hash = "sha256-.*";$/    hash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";/' \
		nix/canvas.nix >"$tmp/canvas.nix"
	echo "$tmp/canvas.nix"
}

failures=0
# expect NAME STATUS NEEDLE -- ENV... COMMAND...
expect() {
	local name="$1" want="$2" needle="$3"
	shift 4
	local out status
	out="$(env "$@" 2>&1)" && status=0 || status=$?
	if [ "$status" -ne "$want" ]; then
		echo "FAIL $name: exit $status, want $want"
		printf '%s\n' "$out" | sed 's/^/    /'
		failures=$((failures + 1))
	elif [[ "$out" != *"$needle"* ]]; then
		echo "FAIL $name: output does not mention '$needle'"
		printf '%s\n' "$out" | sed 's/^/    /'
		failures=$((failures + 1))
	else
		echo "ok   $name"
	fi
}

nix_file="$(nix_copy)"
expect "the latest stable build of 26.3 is 991" 2 "does not name Canvas 26.3 build 991" -- \
	CHECK=1 API="$good_api" NIX_FILE="$nix_file" "$pin" 26.3
expect "without arguments it stays on the pinned version" 2 "Canvas 26.3 build 991" -- \
	CHECK=1 API="$good_api" NIX_FILE="$nix_file" "$pin"
expect "a build the API lists on another Minecraft version" 1 "is for 26.4, not 26.3" -- \
	API="$good_api" NIX_FILE="$nix_file" "$pin" 26.3 992
expect "an experimental build" 1 "experimental" -- \
	API="$good_api" NIX_FILE="$nix_file" "$pin" 26.3 995
expect "a build on another channel" 1 "beta channel, not stable" -- \
	API="$good_api" NIX_FILE="$nix_file" "$pin" 26.3 994
expect "a failed build" 1 "is FAILURE, not SUCCESS" -- \
	API="$good_api" NIX_FILE="$nix_file" "$pin" 26.3 993
expect "a build the API does not list" 1 "no such build" -- \
	API="$good_api" NIX_FILE="$nix_file" "$pin" 26.3 100
expect "a Minecraft version with no stable build" 1 "no successful stable build of Canvas 26.9" -- \
	API="$good_api" NIX_FILE="$nix_file" "$pin" 26.9
expect "a jar wanting another Mojang jar" 1 "different Mojang jar" -- \
	API="$other_api" NIX_FILE="$nix_file" "$pin" 26.3
expect "the pin is written" 0 "wrote $nix_file" -- \
	API="$good_api" NIX_FILE="$nix_file" "$pin" 26.3
expect "and then the check agrees" 0 "already names Canvas 26.3 build 991" -- \
	CHECK=1 API="$good_api" NIX_FILE="$nix_file" "$pin" 26.3

if grep -q '^  canvasBuild = "991";$' "$nix_file" &&
	grep -q "^    hash = \"$(nix --extra-experimental-features 'nix-command flakes' hash convert --hash-algo sha256 --to sri "$(sha256sum "$tmp/good.jar" | cut -d' ' -f1)")\";$" "$nix_file"; then
	echo "ok   the file carries the build and the hash of the bytes received"
else
	echo "FAIL the file carries the build and the hash of the bytes received"
	diff -u nix/canvas.nix "$nix_file" | sed 's/^/    /' || true
	failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
	echo "$failures failed"
	exit 1
fi
