#!/usr/bin/env bash
# Refuses a Paper/Velocity image tag whose imageVersion has drifted from the
# one flake.nix currently builds.
#
# nix/{purpur,velocity}-image.nix tag their images
# "${upstreamVersion}-${imageVersion}", and the manifests below pin that tag
# by hand: the two a reader actually runs or copies, plus every guide that
# opens with a pastable manifest. Nothing else compares any of them to
# imageVersion.
#
# Deliberately not listed: docs/archive/ and docs/superpowers/, where an old
# tag records what was true then and freezing it is the point -- which covers
# docs/archive/release-notes.md, whose version notes name old tags on purpose.
# A new guide that pins a tag belongs in the list; one that quotes history
# does not. A `docker pull` only catches a
# tag that has been deleted; a stale tag that still exists in the registry
# pulls cleanly forever, which is the actual failure -- config/samples/
# network.yaml sat at 0.2.15, nineteen releases behind flake.nix's 0.2.34,
# with nothing noticing. This is the standing check that gap asked for.
#
# What it does not cover: this compares the tag string against what flake.nix
# currently builds, not against what is published, so a release that has not
# published yet leaves the tutorial naming an image that does not exist.
#
# The tag's two halves are not both the same shape: purpurVersion has one
# dot ("26.2"), velocityVersion has two ("3.5.1"). So the split point is the
# *last* dash, not a regex assuming a fixed number of dots on either side --
# a fixed-shape regex reads one of the two tags correctly and mis-reads the
# other.
#
# Usage:
#   hack/image-tag-pins-agree.sh [--flake FILE] [--image-version VERSION]
#                                 [--manifest FILE]...
#
# With no --manifest, checks the tree's own two manifests. --image-version
# overrides reading flake.nix, the way toolchain-pins-agree.sh's --protoc
# overrides invoking the real protoc binary; it exists for
# hack/image-tag-pins-agree-test.sh, which has to drive a version this tree
# does not carry.
#
# Exit status: 0 every tag agrees, 1 one does not or something could not be
# read.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
flake="$root/flake.nix"
image_version=""
manifests=()

while [ $# -gt 0 ]; do
  case "$1" in
    --flake) flake="$2"; shift 2 ;;
    --image-version) image_version="$2"; shift 2 ;;
    --manifest) manifests+=("$2"); shift 2 ;;
    *) echo "image-tag-pins-agree: unknown argument $1" >&2; exit 1 ;;
  esac
done

if [ "${#manifests[@]}" -eq 0 ]; then
  manifests=(
    "$root/docs/tutorial/network.yaml"
    "$root/docs/tutorial/index.md"
    "$root/config/samples/network.yaml"
    "$root/docs/guides/expose-strategies.md"
    "$root/docs/guides/persistent-worlds.md"
    "$root/docs/guides/scaling-and-boosts.md"
    "$root/docs/guides/scheduling.md"
    "$root/docs/plugin-api/index.md"
    "$root/agent/api/README.md"
  )
fi

fail() { echo "image-tag-pins-agree: $*" >&2; exit 1; }

if [ -z "$image_version" ]; then
  [ -r "$flake" ] || fail "cannot read $flake"
  image_version="$(sed -n 's/^[[:space:]]*imageVersion = "\([^"]*\)";/\1/p' "$flake")"
  [ -n "$image_version" ] || fail "cannot read imageVersion out of $flake"
fi

bad=0
found_any=0
for manifest in "${manifests[@]}"; do
  [ -r "$manifest" ] || fail "cannot read $manifest"
  while IFS= read -r ref; do
    found_any=1
    tag="${ref#*:}"
    got="${tag##*-}"
    if [ "$got" != "$image_version" ]; then
      echo "image-tag-pins-agree: $manifest pins $ref, imageVersion $got;" \
        "flake.nix currently builds $image_version" >&2
      bad=1
    fi
  done < <(grep -oE 'ghcr\.io/spawnery/(purpur|velocity):[^[:space:]]+' "$manifest" || true)

  # The Maven coordinate carries imageVersion undivided: nix/agents.nix sets
  # the agents' version to imageVersion and passes it as -PagentVersion, which
  # is what agent/api/build.gradle.kts publishes under. So there is no
  # upstream half to strip here, unlike the image tags above.
  while IFS= read -r ref; do
    found_any=1
    got="${ref##*:}"
    if [ "$got" != "$image_version" ]; then
      echo "image-tag-pins-agree: $manifest pins $ref, agent version $got;" \
        "flake.nix currently builds $image_version" >&2
      bad=1
    fi
  done < <(grep -oE 'cloud\.spawnery:spawnery-api:[0-9][^"[:space:]]*' "$manifest" || true)
done

# A manifest naming neither image would pass the loop above by having
# nothing to disagree with.
[ "$found_any" -eq 1 ] ||
  fail "none of the given manifests name a ghcr.io/spawnery/{purpur,velocity} image; this check would pass vacuously"

if [ "$bad" -ne 0 ]; then
  echo "image-tag-pins-agree: after a release, move the tags above to" \
    "imageVersion $image_version." >&2
  exit 1
fi
