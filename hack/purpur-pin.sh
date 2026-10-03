#!/usr/bin/env bash
# Computes what nix/purpur.nix has to be told about a Purpur build, and writes
# it in. Mojang's jar comes from nix/paper.nix, so only one artifact is pinned.
#
# Purpur's API publishes only an MD5, so the SHA-256 is computed from the
# downloaded bytes. The MD5 check catches corruption and a mistyped build
# number, not an adversary; review the diff this writes.
#
# Usage:
#   hack/purpur-pin.sh                # the latest build of the pinned version
#   hack/purpur-pin.sh 26.3           # the latest build of 26.3
#   hack/purpur-pin.sh 26.3 2650      # exactly that build
#   CHECK=1 hack/purpur-pin.sh        # print, compare, change nothing
#
# Exit status:
#   0  nix/purpur.nix now names that build (or, under CHECK, already did)
#   1  a refusal this script decides for itself
#   2  under CHECK, the file does not match
set -euo pipefail

NIX_FILE="${NIX_FILE:-nix/purpur.nix}"
API="https://api.purpurmc.org/v2/purpur"

fail() { echo "purpur-pin: $*" >&2; exit 1; }

for tool in curl jq nix md5sum sha256sum; do
	command -v "$tool" >/dev/null || fail "$tool is not on PATH; run this inside nix develop"
done
[ -r "$NIX_FILE" ] || fail "cannot read $NIX_FILE (run from the repository root)"

# Without an argument, stay on the pinned Minecraft version.
current_version="$(sed -n 's/^  purpurVersion = "\(.*\)";$/\1/p' "$NIX_FILE")"
current_build="$(sed -n 's/^  purpurBuild = "\(.*\)";$/\1/p' "$NIX_FILE")"
[ -n "$current_version" ] && [ -n "$current_build" ] ||
	fail "could not read purpurVersion/purpurBuild out of $NIX_FILE"

version="${1:-$current_version}"
build="${2:-}"
if [ -z "$build" ]; then
	build="$(curl -fsS "$API/$version" | jq -r '.builds.latest')"
	[ -n "$build" ] && [ "$build" != "null" ] ||
		fail "no builds listed for Purpur $version"
fi

meta="$(curl -fsS "$API/$version/$build")" || fail "no such build: Purpur $version build $build"
result="$(jq -r '.result' <<<"$meta")"
[ "$result" = "SUCCESS" ] || fail "Purpur $version build $build is $result, not SUCCESS"
want_md5="$(jq -r '.md5' <<<"$meta")"
[ -n "$want_md5" ] && [ "$want_md5" != "null" ] || fail "the API stated no md5 for that build"

jar_url="$API/$version/$build/download"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL "$jar_url" -o "$work/purpur.jar" || fail "could not download $jar_url"
got_md5="$(md5sum "$work/purpur.jar" | cut -d' ' -f1)"
[ "$got_md5" = "$want_md5" ] ||
	fail "the downloaded jar has md5 $got_md5, but the API said $want_md5"

got_sha="$(sha256sum "$work/purpur.jar" | cut -d' ' -f1)"
jar_hash="$(nix --extra-experimental-features 'nix-command flakes' \
	hash convert --hash-algo sha256 --to sri "$got_sha")"

# Mojang's jar comes from nix/paper.nix, so a Purpur build wanting another
# Minecraft version is refused here rather than by paperclip in the sandbox.
if command -v jar >/dev/null; then
	(cd "$work" && jar xf purpur.jar META-INF/download-context) ||
		fail "no META-INF/download-context in the launcher; Purpur's packaging has changed"
	mojang_url="$(awk '{print $2}' "$work/META-INF/download-context")"
	paper_mojang="$(sed -n 's|^    url = "\(https://piston-data.mojang.com/[^"]*\)";$|\1|p' nix/paper.nix)"
	if [ -n "$paper_mojang" ] && [ "$mojang_url" != "$paper_mojang" ]; then
		echo "purpur-pin: this build wants a different Mojang jar than nix/paper.nix pins:" >&2
		echo "  purpur: $mojang_url" >&2
		echo "  paper:  $paper_mojang" >&2
		fail "move the Paper pin first, or give nix/purpur.nix a mojangJar of its own"
	fi
fi

cat <<REPORT
Purpur $version build $build
  purpurJar url   $jar_url
  purpurJar md5   $want_md5   (stated by the API, checked)
  purpurJar hash  $jar_hash   (computed here from the bytes received)
REPORT

# The URL is not rewritten: it is templated over the two values above.
rewrite() {
	local file="$1"
	local before after
	before="$(cat "$file")"
	sed -i \
		-e "s|^  purpurVersion = \".*\";$|  purpurVersion = \"$version\";|" \
		-e "s|^  purpurBuild = \".*\";$|  purpurBuild = \"$build\";|" \
		-e "s|^    hash = \"sha256-.*\";$|    hash = \"$jar_hash\";|" \
		"$file"
	after="$(cat "$file")"
	if [ "$before" = "$after" ] && [ "$version" != "$current_version" ]; then
		fail "nothing in $file matched the anchors; its shape has drifted"
	fi
}

if [ -n "${CHECK:-}" ]; then
	copy="$work/purpur.nix"
	cp "$NIX_FILE" "$copy"
	rewrite "$copy"
	if diff -u "$NIX_FILE" "$copy" >/dev/null; then
		echo "$NIX_FILE already names Purpur $version build $build"
		exit 0
	fi
	diff -u "$NIX_FILE" "$copy" || true
	echo "purpur-pin: $NIX_FILE does not name Purpur $version build $build" >&2
	exit 2
fi

rewrite "$NIX_FILE"
echo "wrote $NIX_FILE"
echo "Now run: nix build .#purpur-image && make purpur-image-test"
