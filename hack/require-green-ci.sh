#!/usr/bin/env bash
# Refuses a commit whose ci.yml run did not conclude success.
#
# Written after a release was tagged on a commit whose CI had already failed
# three times on the same Nix hash mismatch. The failure was visible in three
# places before the tag was pushed; the tag was the fourth place the
# repository said so, and it was the first place anybody read it. This script
# is the read that should have happened automatically.
#
# Usage:
#   hack/require-green-ci.sh <owner/repo> <sha>
#
# Exit status:
#   0  the ci.yml run for <sha> on master, triggered by the push event,
#      concluded success.
#   1  anything else: no such run exists, it concluded something other than
#      success, or it never finished within CI_WAIT_LIMIT.
#
# It queries /actions/workflows/ci.yml/runs and reads the *run's* conclusion,
# never a job's. ci.yml has four jobs -- test, lint, deps and e2e -- and a run
# is red if any one of them is. Keying this on a single job's name would leave
# a job added later silently outside the gate, and the incident this script
# exists for failed in e2e, the last of the four; a rule that only knew about
# "test" would have waved it through.
#
# It filters on event=push and branch=master, not just head_sha. A commit
# reachable from master can carry two runs of ci.yml against the identical
# tree -- one triggered by the pull request that introduced it, one by the
# push that merged it -- and they are not the same evidence. The pull_request
# run tests a speculative merge of the PR branch into master as it stood at
# the time; the push run tests the tree master actually carries after the
# merge landed. A v* tag publishes the second tree, so it is the second run's
# verdict that answers the question this script asks. Filtering only by
# head_sha would let the two answers stand in for each other.
#
# Environment:
#   GH_TOKEN        read by `gh` itself, the same as every other script here
#                    that shells out to it.
#   CI_WAIT_LIMIT    seconds to keep polling a run that has not completed.
#                    Default 1200 (20 minutes), because that is roughly what
#                    the slowest job here (e2e, timeout-minutes: 45) takes on
#                    a warm runner; see ci.yml's own timeout-minutes comments
#                    for where the individual job budgets come from.
#   CI_RUNS_CMD      **the seam, not a configuration knob.** When set, it
#                    replaces the `gh api` call below -- its own stdout is
#                    read as the run list, and no network request is made.
#                    This exists so hack/require-green-ci-test.sh can hand
#                    this script a fixture: an in-progress run cannot
#                    otherwise be produced against live history, since a
#                    passing CI run finishes before a test could observe it
#                    still polling. It follows hack/e2e.sh, which for the
#                    same reason takes its cluster provider from the
#                    environment rather than hard-coding one: "the script
#                    deliberately hard-codes neither the scope nor the
#                    provider; it reads them from the environment." A caller
#                    outside the test has no reason to ever set this.
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: hack/require-green-ci.sh <owner/repo> <sha>" >&2
	exit 1
fi
repo="$1"
sha="$2"

CI_WAIT_LIMIT="${CI_WAIT_LIMIT:-1200}"

waited=0
while true; do
	# ${CI_RUNS_CMD:-gh api ...}: unquoted and unset by default, so bash
	# expands it to nothing and the words that follow (the real `gh api`
	# invocation) run as normal. When the test sets it, it is a command name
	# -- e.g. `cat /path/to/fixture.json` -- split on whitespace the same way
	# hack/e2e.sh's own environment-provided commands are; not `eval`, which
	# would let a fixture's content reinterpret shell metacharacters.
	runs="$(${CI_RUNS_CMD:-gh api "/repos/${repo}/actions/workflows/ci.yml/runs?head_sha=${sha}&event=push&branch=master&per_page=1"})"
	count="$(printf '%s' "${runs}" | jq '.workflow_runs | length')"
	if [ "${count}" -eq 0 ]; then
		# Absence is not permission. A tag on a commit CI never saw is exactly
		# as unverified as a tag on a red one -- the empty-list trap this
		# script exists not to fall into is treating this case as green
		# because jq's .conclusion on an empty list is the *string* "null",
		# which a careless `!= failure` would pass straight through.
		echo "no ci.yml run for ${sha} on master (event=push): CI never ran on this commit" >&2
		exit 1
	fi

	status="$(printf '%s' "${runs}" | jq -r '.workflow_runs[0].status')"
	conclusion="$(printf '%s' "${runs}" | jq -r '.workflow_runs[0].conclusion')"
	url="$(printf '%s' "${runs}" | jq -r '.workflow_runs[0].html_url')"

	if [ "${status}" != "completed" ]; then
		if [ "${waited}" -ge "${CI_WAIT_LIMIT}" ]; then
			# Distinct from every failure message below on purpose: hitting
			# the wait limit says nothing about whether CI is red, only that
			# it has not answered yet. A release owner acts on those two
			# facts differently, and folding this into "ci.yml ... concluded
			# ..." below would report a conclusion that was never reached.
			echo "ci.yml on ${sha} did not complete within ${CI_WAIT_LIMIT}s (still ${status}): ${url}" >&2
			exit 1
		fi
		sleep 15
		waited=$((waited + 15))
		continue
	fi

	# Positive, never negative. `[ "${conclusion}" != failure ]` would also
	# pass on cancelled, timed_out, action_required and the literal string
	# "null" that jq prints for a conclusion that is JSON null -- which is
	# exactly what an in-progress run's conclusion field is before it
	# finishes. Only an explicit match on success is read as permission.
	if [ "${conclusion}" = "success" ]; then
		exit 0
	fi
	echo "ci.yml on ${sha} concluded ${conclusion}, not success: ${url}" >&2
	exit 1
done
