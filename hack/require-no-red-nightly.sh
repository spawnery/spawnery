#!/usr/bin/env bash
# Refuses a release while the nightly reproducibility build is known-red.
#
# nightly.yml is the only thing in this repository that builds .#paper-image
# and .#velocity-image. ci.yml builds .#operator-image and nothing else, so a
# stale fixed-output hash in nix/paper.nix or nix/velocity.nix is green all
# the way to a tag and is first noticed by hack/publish.sh on a runner, after
# the tag has been pushed and the release has therefore already been
# announced.
#
# On 2026-08-22 the nightly went red at 03:56Z and nobody read it. Eleven
# hours later a release died on the same cause. The signal existed; the reader
# did not. nightly.yml now opens an issue labelled ${NIGHTLY_LABEL} when it
# fails and closes it when it passes, and this script is the release asking
# whether that issue is open.
#
# Usage:
#   hack/require-no-red-nightly.sh <owner/repo>
#
# Exit status:
#   0  no open issue carries the label. Nothing else exits 0.
#   1  every refusal this script decides for itself: wrong argument count, an
#      issue count that is not a number, or an open issue.
#   *  whatever a tool it runs exited with, rather than translated into 1 --
#      the same contract hack/require-green-ci.sh's header sets out, for the
#      same reasons.
#
# **Absence is permission here, and that is the opposite of the rule in
# hack/require-green-ci.sh.** That script refuses a commit with no ci.yml run,
# because a run is something a releasable commit must *have*: absence there
# means nothing checked it. Here the issue is something a releasable
# repository must *not* have, and no issue is the ordinary state -- the label
# has never been applied to anything at the time of writing. Inverting one of
# these to match the other would break whichever one was inverted, so the
# asymmetry is deliberate and is written down rather than left to be
# rediscovered.
#
# What is *not* inverted is the treatment of a failed query. A `gh` that
# cannot answer is a refusal, not a pass. The distinction is the one the
# empty-list trap in require-green-ci.sh is about: "I did not find an open
# issue" and "I could not look" produce the same empty output and mean
# opposite things, so they are separated before the output is read rather
# than after.
#
# **How to get past this.** Close the issue. That is the whole override, and
# it is deliberately an act rather than a flag: the thing this gate exists to
# fix is that a red nightly reached nobody, and a human closing the issue is
# the reader arriving. Closing it is a claim that the cause is fixed; if the
# claim is wrong, the next nightly opens a new one within a day and the
# following release stops again. There is no environment variable that skips
# this, because a variable would be read by the same person, on the same
# night, for the same reason, and would leave no trace that they did.
#
# Environment:
#   GH_TOKEN             read by `gh` itself, as everywhere else in hack/.
#   NIGHTLY_LABEL        the label nightly.yml applies. Default nightly-red.
#                        A knob only so that this script and nightly.yml can
#                        be read against one name; changing it means changing
#                        both, and the workflow does not read this variable.
#   NIGHTLY_ISSUES_CMD   **the seam, not a configuration knob**, exactly as
#                        CI_RUNS_CMD is in hack/require-green-ci.sh. When set
#                        it replaces the `gh issue list` call below and its
#                        stdout is read as the issue list, with no network
#                        request. It exists because the refusing case cannot
#                        be driven against the live repository without
#                        creating an issue in it, and a test that leaves
#                        objects behind in a real repository is a worse test
#                        than one that reads a fixture. A caller outside
#                        hack/require-no-red-nightly-test.sh has no reason to
#                        set this.
set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: hack/require-no-red-nightly.sh <owner/repo>" >&2
	exit 1
fi
repo="$1"

NIGHTLY_LABEL="${NIGHTLY_LABEL:-nightly-red}"

# ::error:: on stdout, one line, ending in an instruction -- the reasoning is
# in hack/require-green-ci.sh's refuse(), and this is deliberately the same
# function so that the two gates in release.yml's publish job speak with one
# voice on the run summary.
refuse() {
	echo "::error::$*"
	exit 1
}

# Unquoted and unset by default, so bash expands it to nothing and the real
# `gh issue list` that follows runs as normal. When the test sets it, it is a
# command name split on whitespace by the same unquoted expansion -- not
# `eval`, which would let a fixture's content reinterpret shell metacharacters.
#
# --limit 1 because the question is "is there one", not "how many". If the
# label somehow carries several, the first is as good a thing to name as any
# and the refusal's instruction is the same for all of them.
gh_status=0
issues="$(${NIGHTLY_ISSUES_CMD:-gh issue list --repo "${repo}" --label "${NIGHTLY_LABEL}" --state open --json number,url,title --limit 1})" || gh_status=$?
if [ "${gh_status}" -ne 0 ]; then
	echo "::error::could not ask GitHub whether a ${NIGHTLY_LABEL} issue is open on ${repo}; gh exited ${gh_status} and its own error is above. Check GH_TOKEN is set and that the token carries issues: read on ${repo} -- release.yml grants exactly that, so a 404 here is usually the token rather than the repository. This refuses rather than passing: not being able to look is not the same as there being nothing to find."
	exit "${gh_status}"
fi

count="$(printf '%s' "${issues}" | jq 'length')"
# Checked before it is used as a number, for the reason spelled out at length
# in hack/require-green-ci.sh: fed empty stdin, jq prints nothing and exits 0,
# and `[ "" -eq 0 ]` is a test error rather than false. As an `if` condition
# that error is invisible to set -e, and the script would fall through it --
# here that would mean passing, which is the direction that must not happen by
# accident.
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
