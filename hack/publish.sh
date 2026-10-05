#!/usr/bin/env bash
# Publish Spawnery images to ghcr.io.
#
# Images are copied from their Nix archives straight to the registry, with
# no local container store in between.
#
# Usage:
#   hack/publish.sh                     publish every image
#   hack/publish.sh operator-image      publish only the operator's
#
# Naming images exists because imageVersion and operatorVersion move
# independently: publishing all would refuse at the first unchanged tag.
#
# Environment:
#   DRY_RUN=1       build the images and print what would be copied where.
#                   Nothing is sent to the registry; the Nix builds still run.
#   FORCE=1         overwrite a tag that already exists.
#   WRITE_DIGEST=1  write the digest this run pushed for the operator image
#                   into charts/spawnery/values.yaml's image.digest.
#
# Exit status:
#   0  everything asked for was published (or, under DRY_RUN, described).
#   2  an argument named an image this script does not know.
#   3  a tag asked for is already on the registry, and nothing was
#      overwritten. release.yml tells this apart from 1.
#   1  anything else, including "cannot tell whether it already exists".
set -euo pipefail

DRY_RUN="${DRY_RUN:-0}"
FORCE="${FORCE:-0}"
WRITE_DIGEST="${WRITE_DIGEST:-0}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# attr:out-link, in publish order. The operator is last so WRITE_DIGEST does
# not rewrite the manifest before the game images have succeeded.
all_images=(
	"purpur-image:result-purpur"
	"purpur-image-26-2:result-purpur-26-2"
	"velocity-image:result-velocity"
	"operator-image:result-operator"
)

# Keeps the order above, not the command line's.
images=()
if [ "$#" -eq 0 ]; then
	images=("${all_images[@]}")
else
	for want in "$@"; do
		found=""
		for entry in "${all_images[@]}"; do
			if [ "${entry%%:*}" = "$want" ]; then
				found="$entry"
			fi
		done
		if [ -z "$found" ]; then
			echo "unknown image '${want}'. Known images:" >&2
			for entry in "${all_images[@]}"; do
				echo "  ${entry%%:*}" >&2
			done
			exit 2
		fi
	done
	for entry in "${all_images[@]}"; do
		for want in "$@"; do
			if [ "${entry%%:*}" = "$want" ]; then
				images+=("$entry")
				break
			fi
		done
	done
fi

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
		# Only "manifest unknown" means no such tag: a write:packages token
		# without read can 403 on inspect and still succeed on copy.
		inspect_status=0
		inspect_err="$(skopeo inspect "$ref" 2>&1 >/dev/null)" || inspect_status=$?

		if [ "$inspect_status" -eq 0 ]; then
			echo "refusing to overwrite ${name}:${tag}, which already exists. Bump the" >&2
			echo "version in flake.nix, or publish only the image that changed (e.g." >&2
			echo "\`hack/publish.sh operator-image\`), or re-run with FORCE=1 if you mean it." >&2
			exit 3
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

	# --digestfile rather than inspecting afterwards: needs no read scope and
	# cannot race another push to the same tag.
	digestfile="$(mktemp)"
	skopeo copy --digestfile "$digestfile" "docker-archive:${link}" "$ref"
	digest="$(cat "$digestfile")"
	rm -f "$digestfile"
	if [ -z "$digest" ]; then
		echo "skopeo copy wrote no digest for ${name}:${tag}; refusing to carry on with" >&2
		echo "an empty one." >&2
		exit 1
	fi
	echo "published ${name}:${tag} @ ${digest}"

	if [ "$attr" = "operator-image" ]; then
		operator_digest="$digest"
	fi
done

if [ "$WRITE_DIGEST" = "1" ] && [ -n "$operator_digest" ]; then
	manifest="charts/spawnery/values.yaml"
	# The spawnery.image helper prefers image.digest over image.tag, so only
	# this key changes. `sed -i` exits 0 whether or not anything matched.
	pattern='(^[[:space:]]*digest:[[:space:]]*)".*"[[:space:]]*$'
	if ! grep -qE "$pattern" "$manifest"; then
		echo "no digest: line in ${manifest}; the chart's shape has moved and this" >&2
		echo "substitution would have reported success over an unchanged file. Fix" >&2
		echo "the pattern here, or set the digest by hand:" >&2
		echo "  ${operator_digest}" >&2
		exit 1
	fi
	sed -i -E "s|${pattern}|\1\"${operator_digest}\"|" "$manifest"
	if ! grep -qF "digest: \"${operator_digest}\"" "$manifest"; then
		echo "sed reported success but ${manifest} does not carry" >&2
		echo "  digest: \"${operator_digest}\"" >&2
		echo "after the substitution; refusing to claim the write succeeded." >&2
		exit 1
	fi
	echo "wrote digest ${operator_digest} into ${manifest}"
fi
