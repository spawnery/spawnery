#!/usr/bin/env bash
# Publish the Helm chart to ghcr.io as an OCI artefact, so that installing it
# needs no checkout of this repository.
#
# Usage:
#   hack/publish-chart.sh
#
# The result is `oci://ghcr.io/spawnery/charts/spawnery`, at the version
# charts/spawnery/Chart.yaml names -- beside the three images rather than in a
# second place, which is why this is an OCI artefact and not a gh-pages
# index.yaml: the release job is already authenticated to this registry, and a
# classic Helm repository would need a branch, a merged index and a repository
# setting nobody can grant from inside a workflow.
#
# Packaged from `git archive HEAD`, and deliberately not from the working
# tree. `hack/publish.sh WRITE_DIGEST=1` rewrites charts/spawnery/values.yaml's
# image.digest in place during a release, so on the runner the working tree
# stops being the chart the tag describes the moment the operator image is
# pushed. Archiving HEAD makes what lands on the registry the chart at that
# commit no matter when this runs, and takes the ordering constraint out of
# .github/workflows/release.yml, where it would have been a comment somebody
# has to keep obeying.
#
# Environment:
#   DRY_RUN=1           package the chart and print what would be pushed
#                       where. Nothing reaches the registry and no credential
#                       is needed.
#   FORCE=1             overwrite a version that is already there.
#   CHART_REPO=...      the OCI repository to push into. Defaults to
#                       ghcr.io/spawnery/charts; `helm push` appends the
#                       chart's own name, so the artefact is
#                       <CHART_REPO>/spawnery:<version>.
#   CHART_INSPECT_CMD=... how to ask the registry whether a version exists.
#                       Defaults to `skopeo inspect --raw`, which is the seam
#                       hack/publish-chart-test.sh drives instead of a
#                       registry. --raw and not a plain inspect: a packaged
#                       chart is an artefact whose config media type skopeo
#                       does not interpret, and only the raw manifest fetch
#                       answers the question actually being asked.
#
# Exit status:
#   0  the chart was pushed (or, under DRY_RUN, described).
#   3  the refusal below: this version is already on the registry and nothing
#      was overwritten. Separate from 1 for the same reason hack/publish.sh
#      separates it -- .github/workflows/release.yml has to tell "already
#      there, nothing to do" apart from "I could not tell whether it is
#      there", and those are the same message to a caller that sees only a
#      non-zero exit.
#   1  anything else, including "cannot tell whether it already exists" and
#      the digest refusal below.
set -euo pipefail

DRY_RUN="${DRY_RUN:-0}"
FORCE="${FORCE:-0}"
CHART_REPO="${CHART_REPO:-ghcr.io/spawnery/charts}"
CHART_INSPECT_CMD="${CHART_INSPECT_CMD:-skopeo inspect --raw}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# The chart as HEAD has it. `git archive` fails on a path HEAD does not carry,
# which under set -e is the right answer: there is nothing to publish.
git archive HEAD charts/spawnery | tar -x -C "$workdir"
chart_dir="$workdir/charts/spawnery"

version="$(grep -E '^version:' "$chart_dir/Chart.yaml" | head -1 | awk '{print $2}')"
if [ -z "$version" ]; then
	echo "no version: line in charts/spawnery/Chart.yaml at HEAD; there is no name to" >&2
	echo "publish this chart under." >&2
	exit 1
fi

# A released chart must not carry image.digest, and this is the only place that
# can still say so. charts/spawnery/values.yaml's own comment gives the reason
# -- the digest exists only after the tag is pushed, so a committed one is
# always the previous release's -- and internal/rbacaudit's
# TestTheOperatorImageIsNotAMutableTag *returns early* when a digest is set,
# which means committing one does not fail that test, it silences it. Nothing
# else in this repository would notice. FORCE=1 does not get past this: it is
# about overwriting a version, not about shipping a reference this project has
# decided not to ship.
if grep -qE '^[[:space:]]*digest:[[:space:]]*"[^"]+"' "$chart_dir/values.yaml"; then
	echo "charts/spawnery/values.yaml at HEAD carries a non-empty image.digest:" >&2
	grep -nE '^[[:space:]]*digest:' "$chart_dir/values.yaml" >&2
	echo "A chart published with one pins every installation to whatever digest was" >&2
	echo "current when it was committed -- necessarily an earlier release's, because" >&2
	echo "the digest does not exist until after the push -- and it silences" >&2
	echo "internal/rbacaudit's TestTheOperatorImageIsNotAMutableTag, which returns" >&2
	echo "early rather than failing when a digest is present. Empty it and commit," >&2
	echo "then tag again." >&2
	exit 1
fi

ref="${CHART_REPO}/spawnery:${version}"

helm package "$chart_dir" --destination "$workdir" >/dev/null
package="$workdir/spawnery-${version}.tgz"
if [ ! -f "$package" ]; then
	echo "helm package reported success but ${package} is not there; refusing to" >&2
	echo "claim a chart was built." >&2
	exit 1
fi

if [ "$DRY_RUN" = "1" ]; then
	echo "would push ${package##*/} -> oci://${ref}"
	exit 0
fi

if [ "$FORCE" != "1" ]; then
	# The same shape as hack/publish.sh's guard, and for the same reason: a
	# token with write:packages is not guaranteed to carry read, so a push
	# credential can 403 on the existence check and still succeed on the push.
	# Only the registry's unambiguous "no such tag" is read as permission to
	# proceed; not knowing is not the same as it not being there.
	#
	# Unlike an image, the version guard in .github/workflows/release.yml
	# already makes this the ordinary case rather than an error: charts/
	# unchanged since the previous tag means Chart.yaml's version stands, this
	# exact chart is already on the registry, and there is nothing to do.
	inspect_status=0
	inspect_err="$($CHART_INSPECT_CMD "docker://${ref}" 2>&1 >/dev/null)" || inspect_status=$?

	if [ "$inspect_status" -eq 0 ]; then
		echo "refusing to overwrite the chart ${ref}, which already exists. Bump version" >&2
		echo "in charts/spawnery/Chart.yaml, or re-run with FORCE=1 if you mean it." >&2
		# 3, not 1: see "Exit status" in the header.
		exit 3
	elif ! grep -qi 'manifest unknown' <<<"$inspect_err"; then
		echo "cannot tell whether the chart ${ref} already exists -- the existence check" >&2
		echo "failed for a reason other than a missing tag:" >&2
		echo "  ${inspect_err}" >&2
		echo "Pushing now would be blind to whatever is already there. Check the token's" >&2
		echo "read scope (write:packages does not imply read) and network access, then" >&2
		echo "re-run; or re-run with FORCE=1 if you already know it is safe to overwrite" >&2
		echo "whatever is there." >&2
		exit 1
	fi
fi

helm push "$package" "oci://${CHART_REPO}"
echo "published oci://${ref}"
