#!/usr/bin/env bash
# Five cases for hack/require-no-red-nightly.sh.
#
# One is driven against the live repository and four go through the seam. The
# split is not the same as hack/require-green-ci-test.sh's, and the reason is
# worth stating: that script's red and no-run cases could use real commits
# because a commit with red CI is a thing this repository's history already
# contains and nothing has to be created to observe it. The refusing case here
# would need an *open issue in the live repository*, which is an object
# somebody would have to look at and close again -- a test that leaves litter
# in the tracker it is testing. So the refusal is a fixture, and the only live
# call is the passing one, which asserts the ordinary state and needs nothing
# to exist.
#
# That leaves the live half thin on purpose, and it is thin in the safe
# direction: the case that can be wrong without anyone noticing is a gate that
# passes when it should refuse, and that case is the one driven four ways
# below.
#
# Requires: GH_TOKEN or an already-authenticated `gh`, network access for the
# live case, and `jq`. Run inside the dev shell:
# `make require-no-red-nightly-test`. That target is in no other target, for
# the same reason require-green-ci-test is not: it talks to api.github.com,
# and a commit-loop target that needs the network is a commit loop that breaks
# on a train.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

sut="./hack/require-no-red-nightly.sh"
REPO="${REPO:-spawnery/spawnery}"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

failures=0
pass() { echo "ok   - $1"; }
fail() {
	echo "FAIL - $1" >&2
	failures=$((failures + 1))
}

# ---------------------------------------------------------------------------
# live: no open nightly-red issue on the real repository.
#
# Confirmed before relying on it:
#   $ gh api /repos/spawnery/spawnery --jq .open_issues_count
#   0
# The repository has never had an issue. This case therefore asserts the
# ordinary state through the real `gh issue list` and the real label, and it
# is the only case here that would notice the query itself being malformed --
# a wrong flag or a label name gh rejects fails here and nowhere else, because
# every case below replaces the command outright.
# ---------------------------------------------------------------------------
out="$workdir/live"
if "$sut" "$REPO" >"$out" 2>&1; then
	pass "live: exits 0 while no ${NIGHTLY_LABEL:-nightly-red} issue is open on ${REPO}"
else
	fail "live: expected exit 0 for ${REPO}; output: $(cat "$out")"
fi

# ---------------------------------------------------------------------------
# empty: the seam returns an empty list. The same verdict as the live case,
# reached without the network, so a failure of the live case can be told apart
# from a failure of the decision.
# ---------------------------------------------------------------------------
echo '[]' >"$workdir/empty.json"
out="$workdir/empty"
if NIGHTLY_ISSUES_CMD="cat $workdir/empty.json" "$sut" "$REPO" >"$out" 2>&1; then
	pass "empty: an empty list exits 0"
else
	fail "empty: expected exit 0; output: $(cat "$out")"
fi

# ---------------------------------------------------------------------------
# open: one open issue. The case the gate exists for.
#
# Asserts three things, not one: that it refuses, that the refusal names the
# issue number and its URL -- a refusal that does not say which issue leaves
# the reader to find it, at whatever hour a release goes wrong -- and that the
# message tells them closing it is the way through.
# ---------------------------------------------------------------------------
cat >"$workdir/open.json" <<'JSON'
[{"number":7,"url":"https://github.com/spawnery/spawnery/issues/7","title":"Nightly: make image-repro failed"}]
JSON
out="$workdir/open"
if NIGHTLY_ISSUES_CMD="cat $workdir/open.json" "$sut" "$REPO" >"$out" 2>&1; then
	fail "open: expected a refusal, got exit 0; output: $(cat "$out")"
else
	if grep -q "#7" "$out" && grep -q "issues/7" "$out" && grep -qi "close it" "$out"; then
		pass "open: refuses, naming the issue, its URL and how to get past it"
	else
		fail "open: refused without naming the issue, its URL or the remedy; output: $(cat "$out")"
	fi
fi

# ---------------------------------------------------------------------------
# unreadable: the query itself fails.
#
# `false` exits 1 with no output, which is what a `gh` refused by a token
# missing `issues: read` looks like to this script once gh's own message has
# gone to stderr. The assertion that matters is the direction: not being able
# to look must not read as nothing to find.
# ---------------------------------------------------------------------------
out="$workdir/unreadable"
if NIGHTLY_ISSUES_CMD="false" "$sut" "$REPO" >"$out" 2>&1; then
	fail "unreadable: a failed query exited 0 -- this is the gate failing open; output: $(cat "$out")"
else
	if grep -qi "could not ask GitHub" "$out"; then
		pass "unreadable: a failed query refuses, and says it could not look"
	else
		fail "unreadable: refused, but not with the message that says why; output: $(cat "$out")"
	fi
fi

# ---------------------------------------------------------------------------
# silent: the query succeeds and produces nothing.
#
# `true` exits 0 with empty stdout, which is the shape hack/require-green-ci.sh
# spent twenty wasted minutes on before its own guard existed: jq over empty
# stdin prints nothing and exits 0, so the count is the empty string, and
# `[ "" -eq 0 ]` is a test *error* rather than false -- invisible to set -e
# inside an `if`. Here falling through that would mean passing. This is the
# case that would fail silently in production and never in a fixture anyone
# thought to write.
# ---------------------------------------------------------------------------
out="$workdir/silent"
if NIGHTLY_ISSUES_CMD="true" "$sut" "$REPO" >"$out" 2>&1; then
	fail "silent: an empty response exited 0 -- this is the gate failing open; output: $(cat "$out")"
else
	if grep -qi "not a count" "$out"; then
		pass "silent: an empty response refuses, and says what it got instead"
	else
		fail "silent: refused, but not for the stated reason; output: $(cat "$out")"
	fi
fi

echo
if [ "${failures}" -ne 0 ]; then
	echo "${failures} case(s) failed" >&2
	exit 1
fi
echo "all cases passed"
