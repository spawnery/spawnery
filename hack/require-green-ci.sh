#!/usr/bin/env bash
# Refuses a commit whose ci.yml run did not conclude success.
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
#   *  whatever a tool it runs exited with (jq, gh), passed through.
#
# Reads the run's conclusion, not a job's, so a job added to ci.yml later is
# inside the gate. Filters on event=push: the pull_request run tested a
# speculative merge, the push run tests the tree a tag publishes.
#
# Environment:
#   GH_TOKEN         read by `gh` itself.
#   CI_WAIT_LIMIT    seconds to keep polling a run that has not completed.
#                    Default 1200, about three times the longest ci.yml run.
#   CI_RUNS_CMD      test seam for hack/require-green-ci-test.sh: replaces the
#                    `gh api` call, its stdout read as the run list. Nothing
#                    else should set it.
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: hack/require-green-ci.sh <owner/repo> <sha>" >&2
	exit 1
fi
repo="$1"
sha="$2"

CI_WAIT_LIMIT="${CI_WAIT_LIMIT:-1200}"

# ::error:: on stdout puts the line on the run's summary page; GitHub reads
# workflow commands from stdout and takes one line each.
refuse() {
	echo "::error::$*"
	exit 1
}

waited=0
while true; do
	# Word-split rather than eval'd, so a fixture's content cannot inject shell.
	gh_status=0
	runs="$(${CI_RUNS_CMD:-gh api "/repos/${repo}/actions/workflows/ci.yml/runs?head_sha=${sha}&event=push&branch=master&per_page=1"})" || gh_status=$?
	if [ "${gh_status}" -ne 0 ]; then
		echo "::error::could not ask GitHub for ci.yml's runs on ${sha}; gh exited ${gh_status} and its own error is above. Check GH_TOKEN is set and that the token carries actions: read on ${repo} -- release.yml grants exactly that, so a 404 here is usually the token rather than a missing run."
		exit "${gh_status}"
	fi
	count="$(printf '%s' "${runs}" | jq '.workflow_runs | length')"
	# jq on empty stdin prints nothing and exits 0.
	case "${count}" in
	'' | *[!0-9]*)
		refuse "asked GitHub how many ci.yml runs ${sha} has and got \"${count}\" back, which is not a count -- an empty or unparseable run list, not a verdict. Nothing here says CI passed, so this refuses. Re-run this job; if it says the same thing twice, check gh and the api.github.com status page."
		;;
	esac
	if [ "${count}" -eq 0 ]; then
		refuse "no ci.yml run for ${sha} on master (event=push): CI never ran on this commit, so nothing has checked it. Push the commit to master, let ci.yml finish, and tag the commit whose run went green."
	fi

	status="$(printf '%s' "${runs}" | jq -r '.workflow_runs[0].status')"
	conclusion="$(printf '%s' "${runs}" | jq -r '.workflow_runs[0].conclusion')"
	url="$(printf '%s' "${runs}" | jq -r '.workflow_runs[0].html_url')"

	if [ "${status}" != "completed" ]; then
		if [ "${waited}" -ge "${CI_WAIT_LIMIT}" ]; then
			refuse "ci.yml on ${sha} did not complete within ${CI_WAIT_LIMIT}s (still ${status}): ${url} -- CI has not answered yet, which is not the same as CI being red. Watch that run, and re-run this job once it is green."
		fi
		sleep 15
		waited=$((waited + 15))
		continue
	fi

	# Positive match: `!= failure` would pass cancelled, timed_out and "null".
	if [ "${conclusion}" = "success" ]; then
		exit 0
	fi
	refuse "ci.yml on ${sha} concluded ${conclusion}, not success: ${url}. Fix what that run found, push the fix to master, and tag the commit whose run is green."
done
