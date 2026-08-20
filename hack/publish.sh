#!/usr/bin/env bash
# Publish Spawnery images to ghcr.io.
#
# Every image is built by Nix and copied from its archive straight to the
# registry: no local container store in between, so what lands there is what
# the flake describes and not what a previous `podman load` left behind.
#
# Usage:
#   hack/publish.sh                     publish all three images
#   hack/publish.sh operator-image      publish only the operator's
#
# The argument list exists because the versions move independently. flake.nix
# keeps `imageVersion` (the agent's, and so Paper's and Velocity's) apart from
# `operatorVersion` on purpose, so the ordinary case after a reconciler fix is
# that exactly one of the three tags is new. Publishing all three then refuses
# at the first image whose tag is already there -- correctly -- and never
# reaches the one that changed. FORCE=1 would get past it only by re-pushing
# ~1.4 GB over tags that are already correct, which is the very overwrite the
# refusal exists to prevent. Naming the image instead keeps the refusal
# absolute and makes a partial publish a deliberate act rather than an
# accident: a run either publishes what it was asked for or stops.
#
# Environment:
#   DRY_RUN=1       build the images and print what would be copied where.
#                   Nothing is sent to the registry and no credential is
#                   needed -- but the Nix builds still run, which on a machine
#                   without them cached is the expensive part.
#   FORCE=1         overwrite a tag that already exists.
#   WRITE_DIGEST=1  write the digest this run pushed for the operator image
#                   into charts/spawnery/values.yaml's image.digest.
#
# Exit status:
#   0  everything asked for was published (or, under DRY_RUN, described).
#   2  an argument named an image this script does not know.
#   3  the refusal below: a tag asked for is already on the registry, and
#      nothing was overwritten. Separate from 1 on purpose. A person reads the
#      message and does not need the number, but .github/workflows/release.yml
#      publishes one image at a time and has to tell "already there, nothing to
#      do" apart from "I could not tell whether it is there" -- and those two
#      are the same message to a caller that only sees a non-zero exit. Making
#      the distinction here keeps the guard in one place instead of a second
#      copy of it in YAML.
#   1  anything else, including "cannot tell whether it already exists".
set -euo pipefail

DRY_RUN="${DRY_RUN:-0}"
FORCE="${FORCE:-0}"
WRITE_DIGEST="${WRITE_DIGEST:-0}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# attr:out-link, in the order they are published. The operator is last so that
# a WRITE_DIGEST run does not rewrite the manifest before the two large images
# have succeeded.
all_images=(
	"paper-image:result-paper"
	"velocity-image:result-velocity"
	"operator-image:result-operator"
)

# Select the requested images, keeping the order above rather than the order
# they were named on the command line: WRITE_DIGEST's reason for publishing the
# operator last does not stop applying because somebody typed it first.
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
			echo "version in flake.nix, or publish only the image that changed (e.g." >&2
			echo "\`hack/publish.sh operator-image\`), or re-run with FORCE=1 if you mean it." >&2
			# 3, not 1: see "Exit status" in the header.
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

	# --digestfile, and not a second `skopeo inspect` of the tag afterwards.
	# The guard above exists precisely because a write:packages token may carry
	# no read scope, and a read-back would need exactly the scope that guard
	# offers FORCE=1 as a way to survive without: under `set -e` a push-only
	# `FORCE=1 hack/publish.sh` would push paper, then die reading its digest,
	# leaving one image published, no digest and no manifest rewrite. It is
	# also a re-read of a mutable tag, so a push landing between the copy and
	# the inspect would have this run record somebody else's digest as its own.
	# --digestfile is written by the copy itself: no read scope, the digest of
	# this push, and nothing in between to race.
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
	# The one line that carries the operator's digest, set rather than the
	# `image:` line the old config/deploy/deployment.yaml rewrite touched.
	# charts/spawnery/templates/_helpers.tpl's spawnery.image helper already
	# prefers image.digest over image.tag whenever it is non-empty, so
	# repository and tag are left exactly as they are -- only this key
	# changes, for the reason the master design's §8 asks for a digest in
	# shipped manifests: a tag can move under a running cluster.
	#
	# Matched with grep before it is substituted, because `sed -i` exits 0
	# whether or not its pattern matched anything and would leave the run
	# printing "wrote ... into ..." over a file it had not touched. This line
	# runs once, by hand, on the run that closes acceptance criterion 7, and
	# the person reading that sentence has no other signal.
	pattern='(^[[:space:]]*digest:[[:space:]]*)".*"[[:space:]]*$'
	if ! grep -qE "$pattern" "$manifest"; then
		echo "no digest: line in ${manifest}; the chart's shape has moved and this" >&2
		echo "substitution would have reported success over an unchanged file. Fix" >&2
		echo "the pattern here, or set the digest by hand:" >&2
		echo "  ${operator_digest}" >&2
		exit 1
	fi
	sed -i -E "s|${pattern}|\1\"${operator_digest}\"|" "$manifest"
	# The grep above only proved the anchor existed before the edit -- sed -i
	# exits 0 regardless of whether its replacement text matches the pattern
	# it was asked to substitute, so a typo in the replacement would pass that
	# check and still leave the file unchanged or wrong. Reading the value
	# back is the only way to know the write actually took.
	if ! grep -qF "digest: \"${operator_digest}\"" "$manifest"; then
		echo "sed reported success but ${manifest} does not carry" >&2
		echo "  digest: \"${operator_digest}\"" >&2
		echo "after the substitution; refusing to claim the write succeeded." >&2
		exit 1
	fi
	echo "wrote digest ${operator_digest} into ${manifest}"
fi
