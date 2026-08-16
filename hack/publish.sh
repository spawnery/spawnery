#!/usr/bin/env bash
# Publish the three Spawnery images to ghcr.io.
#
# Every image is built by Nix and copied from its archive straight to the
# registry: no local container store in between, so what lands there is what
# the flake describes and not what a previous `podman load` left behind.
#
# Environment:
#   DRY_RUN=1       print what would be copied where; contact nothing.
#   FORCE=1         overwrite a tag that already exists.
#   WRITE_DIGEST=1  rewrite config/deploy/deployment.yaml's operator image to
#                   the digest the registry returned.
set -euo pipefail

DRY_RUN="${DRY_RUN:-0}"
FORCE="${FORCE:-0}"
WRITE_DIGEST="${WRITE_DIGEST:-0}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# attr:out-link, in the order they are published. The operator is last so that
# a WRITE_DIGEST run does not rewrite the manifest before the two large images
# have succeeded.
images=(
	"paper-image:result-paper"
	"velocity-image:result-velocity"
	"operator-image:result-operator"
)

operator_digest=""

for entry in "${images[@]}"; do
	attr="${entry%%:*}"
	link="${entry##*:}"

	nix build ".#${attr}" --out-link "$link"
	name="$(nix eval --raw ".#${attr}.imageName")"
	tag="$(nix eval --raw ".#${attr}.imageTag")"
	ref="docker://${name}:${tag}"

	if [ "$DRY_RUN" = "1" ]; then
		echo "would copy docker-archive:${link} -> ${ref}"
		continue
	fi

	# Refuse rather than replace. A tag that already exists and is not the
	# archive in hand means somebody else published it, or this version was
	# never bumped -- both are worth stopping for, and neither is worth
	# discovering from a cluster that pulled something unexpected.
	if [ "$FORCE" != "1" ] && skopeo inspect "$ref" >/dev/null 2>&1; then
		echo "refusing to overwrite ${name}:${tag}, which already exists. Bump the" >&2
		echo "version in flake.nix, or re-run with FORCE=1 if you mean it." >&2
		exit 1
	fi

	skopeo copy "docker-archive:${link}" "$ref"
	digest="$(skopeo inspect --format '{{.Digest}}' "$ref")"
	echo "published ${name}:${tag} @ ${digest}"

	if [ "$attr" = "operator-image" ]; then
		operator_digest="$digest"
	fi
done

if [ "$WRITE_DIGEST" = "1" ] && [ -n "$operator_digest" ]; then
	manifest="config/deploy/deployment.yaml"
	name="$(nix eval --raw '.#operator-image.imageName')"
	# The one line that names the operator image, replaced with a digest
	# reference. The master design's §8 asks for this in shipped manifests
	# because a tag can move under a running cluster.
	sed -i -E "s|(^[[:space:]]*image:[[:space:]]*)${name}[:@].*$|\1${name}@${operator_digest}|" "$manifest"
	echo "wrote ${name}@${operator_digest} into ${manifest}"
fi
