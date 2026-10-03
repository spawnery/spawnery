#!/usr/bin/env bash
# Five cases for hack/require-no-red-nightly.sh. Only the passing case runs
# live; a live refusal would need an open issue in the real tracker.
#
# Requires GH_TOKEN or an authenticated `gh`, network access for the live
# case, and `jq`.
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
# live: no open nightly-red issue on the real repository. The only case that
# would notice the real `gh issue list` query being malformed.
# ---------------------------------------------------------------------------
out="$workdir/live"
if "$sut" "$REPO" >"$out" 2>&1; then
	pass "live: exits 0 while no ${NIGHTLY_LABEL:-nightly-red} issue is open on ${REPO}"
else
	fail "live: expected exit 0 for ${REPO}; output: $(cat "$out")"
fi

# ---------------------------------------------------------------------------
# empty: the live case's verdict without the network.
# ---------------------------------------------------------------------------
echo '[]' >"$workdir/empty.json"
out="$workdir/empty"
if NIGHTLY_ISSUES_CMD="cat $workdir/empty.json" "$sut" "$REPO" >"$out" 2>&1; then
	pass "empty: an empty list exits 0"
else
	fail "empty: expected exit 0; output: $(cat "$out")"
fi

# ---------------------------------------------------------------------------
# open: one open issue; the refusal names it, its URL and the remedy.
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
# unreadable: the query itself fails, as gh does without `issues: read`.
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
# silent: the query succeeds and produces nothing; falling through the
# count check would pass.
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
