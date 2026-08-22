#!/usr/bin/env bash
# Five cases for hack/require-green-ci.sh, exercised against this
# repository's own live history wherever that history already contains the
# state being tested, and against a fixture only where it cannot: neither a
# live in-progress run nor a live empty response can be summoned to order, so
# those two go through CI_RUNS_CMD -- the seam hack/require-green-ci.sh's own
# header documents.
#
# The green, red and no-run cases hit api.github.com through `gh` for real.
# Each one names the commit it asserts about and the `gh run list` output
# that was read, by hand, before this file was written, to confirm the claim
# is still true of the live repository -- this plan was written from one
# snapshot of it and commits move.
#
# Requires: GH_TOKEN or an already-authenticated `gh`, network access, and
# `jq`. Run inside the dev shell, same as every other script here.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

sut="./hack/require-green-ci.sh"
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
# green: a commit whose ci.yml push run concluded success.
#
# Confirmed live before relying on it:
#   $ gh run list --workflow=ci.yml --json headSha,event,headBranch,conclusion \
#       -q '.[] | select(.headSha | startswith("1a9293b"))'
#   {"conclusion":"success","event":"push","headBranch":"master", ...}
# run 32580999843, on 2026-08-22. The plan named this commit as green when it
# was written; this is this run's own re-check of that claim, not a repeat of
# the plan's.
# ---------------------------------------------------------------------------
GREEN_SHA="1a9293bd2b36ba6d694d1ca98413aafecc78da1f"
out="$workdir/green"
if "$sut" "$REPO" "$GREEN_SHA" >"$out" 2>&1; then
	pass "green: exits 0 for ${GREEN_SHA}"
else
	fail "green: expected exit 0 for ${GREEN_SHA}; output: $(cat "$out")"
fi

# ---------------------------------------------------------------------------
# red: a commit whose ci.yml push run concluded failure.
#
# Confirmed live the same way:
#   $ gh run list --workflow=ci.yml --json headSha,event,headBranch,conclusion \
#       -q '.[] | select(.headSha | startswith("2ee9ce7"))'
#   {"conclusion":"failure","event":"push","headBranch":"master", ...}
# run 32580737899, on 2026-08-22.
# ---------------------------------------------------------------------------
RED_SHA="2ee9ce77954c3c49dc3d4d3b1a61c04a93ced7ec"
out="$workdir/red"
if "$sut" "$REPO" "$RED_SHA" >"$out" 2>&1; then
	fail "red: expected non-zero exit for ${RED_SHA}; output: $(cat "$out")"
elif grep -q "concluded failure" "$out"; then
	pass "red: refuses ${RED_SHA} and names the conclusion"
else
	fail "red: exited non-zero but did not name the conclusion; output: $(cat "$out")"
fi

# ---------------------------------------------------------------------------
# no run: a commit that was never pushed to master, so ci.yml never ran on
# it at all -- not "ran and is pending", the absence-of-evidence case the
# constraints call out by name.
#
# git commit-tree builds a real commit object -- a real sha `gh api` can be
# asked about -- without moving any ref or touching the working tree, so it
# is never on master and this holds no matter how many times the test runs.
# ---------------------------------------------------------------------------
NO_RUN_SHA="$(git commit-tree "$(git rev-parse HEAD^{tree})" -p "$(git rev-parse HEAD)" \
	-m "hack/require-green-ci-test.sh: unreachable fixture commit, never pushed to master")"
out="$workdir/norun"
if "$sut" "$REPO" "$NO_RUN_SHA" >"$out" 2>&1; then
	fail "no run: expected non-zero exit for ${NO_RUN_SHA} (unreachable from any branch); output: $(cat "$out")"
elif grep -qi "never ran" "$out"; then
	pass "no run: refuses ${NO_RUN_SHA} and says CI never ran"
else
	fail "no run: exited non-zero but did not say CI never ran; output: $(cat "$out")"
fi

# ---------------------------------------------------------------------------
# in progress: the one case live history cannot supply on demand, because a
# passing run finishes before a test could observe it still running. The
# fixture never reports status=completed, so the script must poll at least
# once and then, because CI_WAIT_LIMIT is short, give up on the *limit* --
# and its message must say so, not claim CI failed. Two assertions, not one:
# that it waited at all, and that it stopped for the right reason.
# ---------------------------------------------------------------------------
cat >"$workdir/in_progress.json" <<'EOF'
{"total_count":1,"workflow_runs":[{"status":"in_progress","conclusion":null,"html_url":"https://example.invalid/runs/0"}]}
EOF

out="$workdir/inprogress"
start=$SECONDS
if CI_RUNS_CMD="cat ${workdir}/in_progress.json" CI_WAIT_LIMIT=10 \
	"$sut" "$REPO" "0000000000000000000000000000000000dead" >"$out" 2>&1; then
	fail "in progress: expected non-zero exit once CI_WAIT_LIMIT was exceeded; output: $(cat "$out")"
else
	elapsed=$((SECONDS - start))
	if [ "$elapsed" -lt 14 ]; then
		fail "in progress: returned after ${elapsed}s without ever polling; the script polls every 15s so this should have taken at least one cycle"
	elif grep -q "did not complete within" "$out" && ! grep -qi "concluded" "$out"; then
		pass "in progress: waited (${elapsed}s), then reported the wait limit rather than a conclusion"
	else
		fail "in progress: message did not report the wait limit correctly; output: $(cat "$out")"
	fi
fi

# ---------------------------------------------------------------------------
# not a count: the fetch succeeds and produces nothing. jq prints nothing and
# exits 0 for empty stdin, so the run count comes back as the empty string --
# and `[ "" -eq 0 ]` is a test *error* rather than false, which as an `if`
# condition does not trip set -e. Before the guard this case fell through to
# the status check, found that empty too, and polled for the whole of
# CI_WAIT_LIMIT before refusing with a message naming an empty status: safe,
# but twenty minutes and a nonsense reason.
#
# So the assertion is on the speed as much as on the exit code. CI_WAIT_LIMIT
# is left at its 1200s default on purpose: a short limit here would pass
# whether the guard exists or not, and the point is that the script must not
# reach the wait loop at all.
# ---------------------------------------------------------------------------
: >"$workdir/empty.txt"

out="$workdir/notacount"
start=$SECONDS
if CI_RUNS_CMD="cat ${workdir}/empty.txt" \
	"$sut" "$REPO" "0000000000000000000000000000000000dead" >"$out" 2>&1; then
	fail "not a count: expected non-zero exit for an empty run list; output: $(cat "$out")"
else
	elapsed=$((SECONDS - start))
	if [ "$elapsed" -ge 14 ]; then
		fail "not a count: took ${elapsed}s, so it entered the poll loop; an uncountable run list must be refused before any waiting"
	elif grep -q "integer expected" "$out"; then
		fail "not a count: still reaches [ \"\" -eq 0 ]; output: $(cat "$out")"
	elif grep -q "not a count" "$out"; then
		pass "not a count: refuses an empty run list at once (${elapsed}s) and says why"
	else
		fail "not a count: exited non-zero but did not say the run list was uncountable; output: $(cat "$out")"
	fi
fi

echo
if [ "$failures" -eq 0 ]; then
	echo "require-green-ci-test: ok (5/5)"
	exit 0
fi
echo "require-green-ci-test: ${failures} case(s) failed" >&2
exit 1
