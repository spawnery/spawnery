#!/usr/bin/env bash
# Computes everything nix/paper.nix has to be told about a Paper build, and
# writes it in.
#
# Following Paper upstream used to be four values read by hand out of two
# places -- the launcher's URL and hash from PaperMC's API, and Mojang's URL
# and hash out of META-INF/download-context inside the launcher itself -- and
# then converted from hex to the base64 SRI form Nix wants. Every one of those
# steps is mechanical and every one of them is a place to mistype a hash into
# a build that then fails somewhere else entirely.
#
# This does not replace the automated image pipeline the main design calls
# project 3. That would watch for new builds and open a change; this answers
# the narrower question of what the values *are* once somebody has decided to
# move. The decision stays a person's.
#
# Usage:
#   hack/paper-pin.sh                 # the latest build of the pinned version
#   hack/paper-pin.sh 26.3            # the latest build of 26.3
#   hack/paper-pin.sh 26.3 118        # exactly that build
#   CHECK=1 hack/paper-pin.sh         # print, compare, change nothing
#
# Exit status:
#   0  nix/paper.nix now names that build (or, under CHECK, already did)
#   1  a refusal this script decides for itself
#   2  under CHECK, the file does not match
set -euo pipefail

NIX_FILE="${NIX_FILE:-nix/paper.nix}"
API="https://fill.papermc.io/v3/projects/paper"

fail() { echo "paper-pin: $*" >&2; exit 1; }

# jar rather than unzip: the dev shell has a JDK for the agent build and no
# unzip, and `jar xf` reads the same archives. Checked here rather than met as
# a bare "command not found" from inside the extraction below.
for tool in curl jq nix jar python3; do
	command -v "$tool" >/dev/null || fail "$tool is not on PATH; run this inside nix develop"
done
[ -r "$NIX_FILE" ] || fail "cannot read $NIX_FILE (run from the repository root)"

# The version currently pinned, so the no-argument form means "the newest
# build of the version we are already on" rather than "whatever is newest",
# which would be a Minecraft upgrade wearing the clothes of a patch bump.
current_version="$(sed -n 's/^  paperVersion = "\(.*\)";$/\1/p' "$NIX_FILE")"
current_build="$(sed -n 's/^  paperBuild = "\(.*\)";$/\1/p' "$NIX_FILE")"
[ -n "$current_version" ] && [ -n "$current_build" ] ||
	fail "could not read paperVersion/paperBuild out of $NIX_FILE"

version="${1:-$current_version}"
build="${2:-}"
if [ -z "$build" ]; then
	build="$(curl -fsS "$API/versions/$version/builds" |
		jq -r '[.[] | select(.channel == "STABLE") | .id] | max')"
	[ -n "$build" ] && [ "$build" != "null" ] ||
		fail "no STABLE build found for Paper $version"
fi

meta="$(curl -fsS "$API/versions/$version/builds/$build")" ||
	fail "no such build: Paper $version build $build"
jar_url="$(jq -r '.downloads["server:default"].url' <<<"$meta")"
jar_sha="$(jq -r '.downloads["server:default"].checksums.sha256' <<<"$meta")"
[ -n "$jar_url" ] && [ "$jar_url" != "null" ] || fail "the API gave no server:default download"

# hex to SRI. nix hash convert does it without fetching anything, which keeps
# this step honest: the hash is the one the API stated, not one recomputed
# from bytes this script happened to receive.
sri() { nix --extra-experimental-features 'nix-command flakes' hash convert --hash-algo sha256 --to sri "$1"; }
jar_hash="$(sri "$jar_sha")"

# Mojang's jar, out of the launcher rather than out of Mojang's own API. That
# is the whole point of this file's existing comment: the checksum does not
# come from the host that serves the artifact.
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL "$jar_url" -o "$work/paper.jar" || fail "could not download $jar_url"
got_sha="$(sha256sum "$work/paper.jar" | cut -d' ' -f1)"
[ "$got_sha" = "$jar_sha" ] ||
	fail "the downloaded jar hashes to $got_sha, but the API said $jar_sha"
(cd "$work" && jar xf paper.jar META-INF/download-context) ||
	fail "no META-INF/download-context in the launcher; Paper's packaging has changed"

context="$(cat "$work/META-INF/download-context")"
mojang_sha="$(awk '{print $1}' <<<"$context")"
mojang_url="$(awk '{print $2}' <<<"$context")"
[ -n "$mojang_sha" ] && [ -n "$mojang_url" ] ||
	fail "could not parse download-context: $context"
mojang_hash="$(sri "$mojang_sha")"

cat <<REPORT
Paper $version build $build
  paperJar url    $jar_url
  paperJar hash   $jar_hash
  mojangJar url   $mojang_url
  mojangJar hash  $mojang_hash
REPORT

# The rewrite is line-oriented and anchored, so a file whose shape has drifted
# is a refusal rather than a silent partial edit: four substitutions that all
# have to bite.
rewrite() {
	local file="$1"
	sed -i \
		-e "s|^  paperVersion = \".*\";$|  paperVersion = \"$version\";|" \
		-e "s|^  paperBuild = \".*\";$|  paperBuild = \"$build\";|" \
		-e "s|^    url = \"https://fill-data.papermc.io/.*\";$|    url = \"${jar_url//$version-$build/\$\{paperVersion\}-\$\{paperBuild\}}\";|" \
		-e "s|^    url = \"https://piston-data.mojang.com/.*\";$|    url = \"$mojang_url\";|" \
		"$file"
	# The two hashes are keyed off the URL above them rather than matched
	# blind: both lines read `hash = "sha256-...";` and a blind substitution
	# would put the launcher's hash on Mojang's jar.
	python3 - "$file" "$jar_hash" "$mojang_hash" <<'PY'
import re, sys
path, jar, mojang = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(path).read()
def after(url_fragment, new_hash, s):
    pat = re.compile(r'(url = "[^"]*' + re.escape(url_fragment) + r'[^"]*";\n(?:\s*)hash = ")[^"]*(")')
    out, n = pat.subn(lambda m: m.group(1) + new_hash + m.group(2), s, count=1)
    if n != 1:
        sys.exit("paper-pin: could not place the hash after " + url_fragment)
    return out
s = after("fill-data.papermc.io", jar, s)
s = after("piston-data.mojang.com", mojang, s)
open(path, "w").write(s)
PY
}

if [ -n "${CHECK:-}" ]; then
	copy="$work/paper.nix"
	cp "$NIX_FILE" "$copy"
	rewrite "$copy"
	if diff -u "$NIX_FILE" "$copy" >/dev/null; then
		echo "$NIX_FILE already names Paper $version build $build"
		exit 0
	fi
	diff -u "$NIX_FILE" "$copy" || true
	echo "paper-pin: $NIX_FILE does not name Paper $version build $build" >&2
	exit 2
fi

rewrite "$NIX_FILE"
echo "wrote $NIX_FILE"
echo "Now run: nix build .#paper-jar && make image-test"
