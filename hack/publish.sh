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

	if [ "$FORCE" != "1" ]; then
		# Refuse rather than replace, but only once we can actually tell the
		# tag is there. skopeo inspect's exit status alone conflates "this tag
		# does not exist" with "I could not read it for some other reason" --
		# a token with write:packages is not guaranteed to carry read, so a
		# push credential shaped exactly like that can 403 on inspect and
		# still succeed on copy. Falling through to publish on any inspect
		# failure would make that 403 indistinguishable from an empty
		# registry and silently defeat the refusal below. Only the registry's
		# own "manifest unknown" -- the unambiguous "no such tag" answer -- is
		# read as permission to proceed; every other failure stops the run,
		# because not knowing whether the tag exists is not the same as it
		# not existing.
		inspect_status=0
		inspect_err="$(skopeo inspect "$ref" 2>&1 >/dev/null)" || inspect_status=$?

		if [ "$inspect_status" -eq 0 ]; then
			echo "refusing to overwrite ${name}:${tag}, which already exists. Bump the" >&2
			echo "version in flake.nix, or re-run with FORCE=1 if you mean it." >&2
			exit 1
		elif ! grep -qi 'manifest unknown' <<<"$inspect_err"; then
			echo "cannot tell whether ${name}:${tag} already exists -- skopeo inspect" >&2
			echo "failed for a reason other than a missing tag:" >&2
			echo "  ${inspect_err}" >&2
			echo "Publishing now would be blind to whatever is already there. Check" >&2
			echo "the token's read scope (write:packages does not imply read) and" >&2
			echo "network access, then re-run; or re-run with FORCE=1 if you already" >&2
			echo "know it is safe to overwrite whatever is there." >&2
			exit 1
		fi
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
