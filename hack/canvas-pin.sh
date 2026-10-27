#!/usr/bin/env bash
# Computes what nix/canvas.nix has to be told about a Canvas build, and writes
# it in. Mojang's jar comes from nix/paper.nix, so only one artifact is pinned.
#
# Canvas's API publishes no checksum, so the SHA-256 is computed from the
# downloaded bytes and nothing vouches for them: review the diff this writes.
# The API ignores a minecraft_version filter, so builds are filtered here.
#
# Usage:
#   hack/canvas-pin.sh                # the latest stable build of the pinned version
#   hack/canvas-pin.sh 26.3           # the latest stable build of 26.3
#   hack/canvas-pin.sh 26.3 991       # exactly that build
#   CHECK=1 hack/canvas-pin.sh        # print, compare, change nothing
#
# Environment:
#   API       the builds listing (default: canvasmc.io); file:// works
#   NIX_FILE  the file to read and rewrite (default: nix/canvas.nix)
#
# Exit status:
#   0  nix/canvas.nix now names that build (or, under CHECK, already did)
#   1  a refusal this script decides for itself
#   2  under CHECK, the file does not match
set -euo pipefail

NIX_FILE="${NIX_FILE:-nix/canvas.nix}"
API="${API:-https://canvasmc.io/api/v2/builds?project=canvas}"

fail() { echo "canvas-pin: $*" >&2; exit 1; }

for tool in curl jq nix jar sha256sum; do
	command -v "$tool" >/dev/null || fail "$tool is not on PATH; run this inside nix develop"
done
[ -r "$NIX_FILE" ] || fail "cannot read $NIX_FILE (run from the repository root)"

current_version="$(sed -n 's/^  canvasVersion = "\(.*\)";$/\1/p' "$NIX_FILE")"
current_build="$(sed -n 's/^  canvasBuild = "\(.*\)";$/\1/p' "$NIX_FILE")"
[ -n "$current_version" ] && [ -n "$current_build" ] ||
	fail "could not read canvasVersion/canvasBuild out of $NIX_FILE"

version="${1:-$current_version}"
build="${2:-}"

listing="$(curl -fsSL "$API")" || fail "could not fetch $API"
jq -e '.builds | type == "array"' <<<"$listing" >/dev/null ||
	fail "the API answered with no builds array"

if [ -z "$build" ]; then
	meta="$(jq -c --arg v "$version" '
		[.builds[]
		 | select(.channelVersion == $v and .result == "SUCCESS"
		          and .isExperimental == false and .channelName == "stable")]
		| sort_by(.buildNumber) | last // empty' <<<"$listing")"
	[ -n "$meta" ] || fail "no successful stable build of Canvas $version in the API's listing"
	build="$(jq -r '.buildNumber' <<<"$meta")"
else
	meta="$(jq -c --argjson b "$build" '.builds[] | select(.buildNumber == $b)' <<<"$listing")"
	[ -n "$meta" ] || fail "no such build in the API's listing: Canvas build $build"
	got_version="$(jq -r '.channelVersion' <<<"$meta")"
	[ "$got_version" = "$version" ] || fail "Canvas build $build is for $got_version, not $version"
	result="$(jq -r '.result' <<<"$meta")"
	[ "$result" = "SUCCESS" ] || fail "Canvas $version build $build is $result, not SUCCESS"
	[ "$(jq -r '.isExperimental' <<<"$meta")" = "false" ] ||
		fail "Canvas $version build $build is experimental"
	channel="$(jq -r '.channelName' <<<"$meta")"
	[ "$channel" = "stable" ] || fail "Canvas $version build $build is on the $channel channel, not stable"
fi

jar_url="$(jq -r '.downloadUrl' <<<"$meta")"
[ -n "$jar_url" ] && [ "$jar_url" != "null" ] || fail "the API gave no download URL for build $build"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL "$jar_url" -o "$work/canvas.jar" || fail "could not download $jar_url"

got_sha="$(sha256sum "$work/canvas.jar" | cut -d' ' -f1)"
jar_hash="$(nix --extra-experimental-features 'nix-command flakes' \
	hash convert --hash-algo sha256 --to sri "$got_sha")"

# Mojang's jar comes from nix/paper.nix, so a Canvas build wanting another
# Minecraft version is refused here rather than by paperclip in the sandbox.
(cd "$work" && jar xf canvas.jar META-INF/download-context) 2>/dev/null ||
	fail "no META-INF/download-context in the launcher; Canvas's packaging has changed"
mojang_url="$(awk '{print $2}' "$work/META-INF/download-context")"
paper_mojang="$(sed -n 's|^    url = "\(https://piston-data.mojang.com/[^"]*\)";$|\1|p' nix/paper.nix)"
[ -n "$paper_mojang" ] || fail "could not read the Mojang jar URL out of nix/paper.nix"
if [ "$mojang_url" != "$paper_mojang" ]; then
	echo "canvas-pin: this build wants a different Mojang jar than nix/paper.nix pins:" >&2
	echo "  canvas: $mojang_url" >&2
	echo "  paper:  $paper_mojang" >&2
	fail "move the Paper pin first, or give nix/canvas.nix a mojangJar of its own"
fi

cat <<REPORT
Canvas $version build $build
  canvasJar url   $jar_url
  canvasJar hash  $jar_hash   (computed here from the bytes received)
The API publishes no checksum for Canvas builds, so nothing confirms these
bytes; the diff wants review.
REPORT

# The URL is not rewritten: it is templated over canvasBuild.
rewrite() {
	local file="$1"
	local before after
	before="$(cat "$file")"
	sed -i \
		-e "s|^  canvasVersion = \".*\";$|  canvasVersion = \"$version\";|" \
		-e "s|^  canvasBuild = \".*\";$|  canvasBuild = \"$build\";|" \
		-e "s|^    hash = \"sha256-.*\";$|    hash = \"$jar_hash\";|" \
		"$file"
	after="$(cat "$file")"
	if [ "$before" = "$after" ] && [ "$version" != "$current_version" ]; then
		fail "nothing in $file matched the anchors; its shape has drifted"
	fi
}

if [ -n "${CHECK:-}" ]; then
	copy="$work/canvas.nix"
	cp "$NIX_FILE" "$copy"
	rewrite "$copy"
	if diff -u "$NIX_FILE" "$copy" >/dev/null; then
		echo "$NIX_FILE already names Canvas $version build $build"
		exit 0
	fi
	diff -u "$NIX_FILE" "$copy" || true
	echo "canvas-pin: $NIX_FILE does not name Canvas $version build $build" >&2
	exit 2
fi

rewrite "$NIX_FILE"
echo "wrote $NIX_FILE"
echo "Now run: nix build .#canvas-image && make canvas-image-test"
