#!/usr/bin/env bash
# Refuses a Purpur/Velocity image tag whose imageVersion, or whose upstream
# version, has drifted from what flake.nix currently builds.
#
# Images are tagged "${upstreamVersion}-${imageVersion}" and the manifests
# below pin that tag by hand. A stale tag still pulls cleanly, so nothing else
# notices.
#
# Not listed: docs/archive/ and docs/superpowers/, where an old tag records
# what was true then. A new guide that pins a tag belongs in the list.
#
# Compares against what flake.nix builds, not against what is published.
#
# Usage:
#   hack/image-tag-pins-agree.sh [--flake FILE] [--image-version VERSION]
#                                 [--purpur-version V] [--velocity-version V]
#                                 [--manifest FILE]...
#
# With no --manifest, checks the list below.
#
# Exit status: 0 every tag agrees, 1 one does not or something could not be
# read.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
flake="$root/flake.nix"
image_version=""
purpur_version=""
velocity_version=""
manifests=()

while [ $# -gt 0 ]; do
  case "$1" in
    --flake) flake="$2"; shift 2 ;;
    --image-version) image_version="$2"; shift 2 ;;
    --purpur-version) purpur_version="$2"; shift 2 ;;
    --velocity-version) velocity_version="$2"; shift 2 ;;
    --manifest) manifests+=("$2"); shift 2 ;;
    *) echo "image-tag-pins-agree: unknown argument $1" >&2; exit 1 ;;
  esac
done

if [ "${#manifests[@]}" -eq 0 ]; then
  manifests=(
    "$root/docs/tutorial/network.yaml"
    "$root/docs/tutorial/index.md"
    "$root/config/samples/network.yaml"
    "$root/config/samples/ondemand.yaml"
    "$root/docs/guides/expose-strategies.md"
    "$root/docs/guides/on-demand-servers.md"
    "$root/docs/guides/persistent-worlds.md"
    "$root/docs/guides/object-store-worlds.md"
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
if [ -z "$purpur_version" ]; then
  purpur_version="$(sed -n 's/^[[:space:]]*purpurVersion = "\([^"]*\)";/\1/p' "$root/nix/purpur.nix")"
  [ -n "$purpur_version" ] || fail "cannot read purpurVersion out of nix/purpur.nix"
fi
if [ -z "$velocity_version" ]; then
  velocity_version="$(sed -n 's/^[[:space:]]*velocityVersion = "\([^"]*\)";/\1/p' "$root/nix/velocity.nix")"
  [ -n "$velocity_version" ] || fail "cannot read velocityVersion out of nix/velocity.nix"
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
    case "$ref" in
      */purpur:*) want_upstream="$purpur_version" ;;
      *) want_upstream="$velocity_version" ;;
    esac
    if [ "${tag%-*}" != "$want_upstream" ]; then
      echo "image-tag-pins-agree: $manifest pins $ref, upstream version ${tag%-*};" \
        "flake.nix currently builds $want_upstream" >&2
      bad=1
    fi
  done < <(grep -oE 'ghcr\.io/spawnery/(purpur|velocity):[^[:space:]]+' "$manifest" || true)

  # The Maven coordinate is imageVersion undivided (nix/agents.nix -PagentVersion).
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

[ "$found_any" -eq 1 ] ||
  fail "none of the given manifests name a ghcr.io/spawnery/{purpur,velocity} image; this check would pass vacuously"

if [ "$bad" -ne 0 ]; then
  echo "image-tag-pins-agree: move the tags above to purpur:$purpur_version-$image_version" \
    "and velocity:$velocity_version-$image_version." >&2
  exit 1
fi
