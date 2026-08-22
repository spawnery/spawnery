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
#      concluded success. Nothing else exits 0.
#   1  every refusal this script decides for itself: wrong argument count, no
#      such run, a run count that is not a number, a conclusion other than
#      success, or a run still unfinished at CI_WAIT_LIMIT.
#   *  whatever a tool it runs exited with, propagated by `set -e -o pipefail`
#      rather than translated into 1. Measured: `jq` exits 5 on stdout that is
#      not JSON, and a missing `jq` or `gh` exits 127; a failing `gh api` exits
#      with gh's own status. These are not enumerated because the list belongs
#      to those tools rather than to this script -- what this script promises
#      is only the first line, that no path out of here except an explicit
#      success returns 0.
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
#                    Default 1200 (20 minutes): roughly three times the
#                    longest ci.yml run observed, which the design measured at
#                    four to seven minutes. That leaves room for a queued
#                    runner without letting a wedged run hold a release job
#                    open indefinitely. Deliberately not sized against
#                    ci.yml's timeout-minutes (e2e budgets 45 for a run that
#                    takes single-digit minutes): those are numbers chosen to
#                    stop a wedged job, not durations, and waiting a released
#                    tag out on one would be waiting on the wrong quantity.
#   CI_RUNS_CMD      **the seam, not a configuration knob.** When set, it
#                    replaces the `gh api` call below -- its own stdout is
#                    read as the run list, and no network request is made.
#                    This exists so hack/require-green-ci-test.sh can hand
#                    this script a fixture: an in-progress run cannot
#                    otherwise be produced against live history, since a
#                    passing CI run finishes before a test could observe it
#                    still polling. hack/e2e.sh is the nearest precedent --
#                    "kind runs under rootless podman, which needs both an
#                    environment variable and a systemd scope. The script
#                    deliberately hard-codes neither" -- but it is a weaker
#                    one than it looks: everything e2e.sh takes from the
#                    environment is a *value* (CLUSTER, E2E_KEEP, DEADLINE,
#                    KIND_EXPERIMENTAL_PROVIDER), never a command to run.
#                    This variable is a command, which is a step further than
#                    anything else in hack/ goes, and the reason it is
#                    defensible here is the paragraph above and not the
#                    precedent. A caller outside the test has no reason to
#                    ever set this.
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
	# -- e.g. `cat /path/to/fixture.json` -- split on whitespace by the same
	# unquoted expansion; not `eval`, which would let a fixture's content
	# reinterpret shell metacharacters.
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
