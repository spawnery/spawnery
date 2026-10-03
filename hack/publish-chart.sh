#!/usr/bin/env bash
# Publish the Helm chart to ghcr.io as an OCI artefact,
# `oci://ghcr.io/spawnery/charts/spawnery` at Chart.yaml's version.
#
# Usage:
#   hack/publish-chart.sh
#
# Packaged from `git archive HEAD`, not the working tree, which
# `hack/publish.sh WRITE_DIGEST=1` may already have rewritten.
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
#                       Defaults to `skopeo inspect --raw`; the test seam for
#                       hack/publish-chart-test.sh. --raw because skopeo does
#                       not interpret a chart's config media type.
#
# Exit status:
#   0  the chart was pushed (or, under DRY_RUN, described).
#   3  this version is already on the registry and nothing was overwritten.
#      release.yml tells this apart from 1.
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

git archive HEAD charts/spawnery | tar -x -C "$workdir"
chart_dir="$workdir/charts/spawnery"

version="$(grep -E '^version:' "$chart_dir/Chart.yaml" | head -1 | awk '{print $2}')"
if [ -z "$version" ]; then
	echo "no version: line in charts/spawnery/Chart.yaml at HEAD; there is no name to" >&2
	echo "publish this chart under." >&2
	exit 1
fi

# Nothing else catches a committed image.digest; FORCE=1 deliberately does not
# skip this.
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
	# Only "manifest unknown" means no such tag: a write:packages token
	# without read can 403 here and still succeed on the push.
	inspect_status=0
	inspect_err="$($CHART_INSPECT_CMD "docker://${ref}" 2>&1 >/dev/null)" || inspect_status=$?

	if [ "$inspect_status" -eq 0 ]; then
		echo "refusing to overwrite the chart ${ref}, which already exists. Bump version" >&2
		echo "in charts/spawnery/Chart.yaml, or re-run with FORCE=1 if you mean it." >&2
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
