#!/usr/bin/env bash
# Refuses a release while the nightly reproducibility build is known-red.
#
# nightly.yml is the only job that builds .#paper-image and .#velocity-image;
# it opens an issue labelled ${NIGHTLY_LABEL} when it fails and closes it when
# it passes. This script asks whether that issue is open.
#
# Usage:
#   hack/require-no-red-nightly.sh <owner/repo>
#
# Exit status:
#   0  no open issue carries the label. Nothing else exits 0.
#   1  every refusal this script decides for itself: wrong argument count, an
#      issue count that is not a number, or an open issue.
#   *  whatever a tool it runs exited with, passed through.
#
# Unlike hack/require-green-ci.sh, absence is permission here: no issue is the
# ordinary state. A failed query still refuses.
#
# The only override is closing the issue; there is deliberately no variable
# that skips this.
#
# Environment:
#   GH_TOKEN             read by `gh` itself.
#   NIGHTLY_LABEL        the label nightly.yml applies. Default nightly-red.
#                        The workflow does not read this; change both.
#   NIGHTLY_ISSUES_CMD   test seam for hack/require-no-red-nightly-test.sh:
#                        replaces the `gh issue list` call, its stdout read as
#                        the issue list. Nothing else should set it.
set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: hack/require-no-red-nightly.sh <owner/repo>" >&2
	exit 1
fi
repo="$1"

NIGHTLY_LABEL="${NIGHTLY_LABEL:-nightly-red}"

# Same as hack/require-green-ci.sh's refuse().
refuse() {
	echo "::error::$*"
	exit 1
}

# Word-split rather than eval'd, so a fixture's content cannot inject shell.
gh_status=0
issues="$(${NIGHTLY_ISSUES_CMD:-gh issue list --repo "${repo}" --label "${NIGHTLY_LABEL}" --state open --json number,url,title --limit 1})" || gh_status=$?
if [ "${gh_status}" -ne 0 ]; then
	echo "::error::could not ask GitHub whether a ${NIGHTLY_LABEL} issue is open on ${repo}; gh exited ${gh_status} and its own error is above. Check GH_TOKEN is set and that the token carries issues: read on ${repo} -- release.yml grants exactly that, so a 404 here is usually the token rather than the repository. This refuses rather than passing: not being able to look is not the same as there being nothing to find."
	exit "${gh_status}"
fi

count="$(printf '%s' "${issues}" | jq 'length')"
# jq on empty stdin prints nothing and exits 0; `[ "" -eq 0 ]` would pass.
case "${count}" in
'' | *[!0-9]*)
	refuse "asked GitHub how many open ${NIGHTLY_LABEL} issues ${repo} has and got \"${count}\" back, which is not a count -- an empty or unparseable response, not an answer. Nothing here says the nightly is green, so this refuses. Re-run this job; if it says the same thing twice, check gh and the api.github.com status page."
	;;
esac

if [ "${count}" -eq 0 ]; then
	exit 0
fi

number="$(printf '%s' "${issues}" | jq -r '.[0].number')"
url="$(printf '%s' "${issues}" | jq -r '.[0].url')"
title="$(printf '%s' "${issues}" | jq -r '.[0].title')"
refuse "the nightly reproducibility build is red: ${repo}#${number} \"${title}\" is open (${url}). nightly.yml is the only job that builds .#paper-image and .#velocity-image, so this is the one signal that a Paper or Velocity hash has gone stale, and ci.yml cannot see it. Read that issue and fix what it names; then close it -- closing it is how you say the cause is fixed, and the next nightly reopens one if you were wrong."
