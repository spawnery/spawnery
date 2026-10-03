#!/usr/bin/env bash
# Five cases for hack/require-green-ci.sh: three against this repository's
# live history, the two that history cannot supply through CI_RUNS_CMD.
#
# Requires GH_TOKEN or an authenticated `gh`, network access, and `jq`.
# GitHub keeps workflow runs for about 90 days, so GREEN_SHA and RED_SHA stop
# resolving around 2026-11-20.
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
# green: a commit whose ci.yml push run concluded success (run 32580999843).
# ---------------------------------------------------------------------------
GREEN_SHA="1a9293bd2b36ba6d694d1ca98413aafecc78da1f"
out="$workdir/green"
if "$sut" "$REPO" "$GREEN_SHA" >"$out" 2>&1; then
	pass "green: exits 0 for ${GREEN_SHA}"
else
	fail "green: expected exit 0 for ${GREEN_SHA}; output: $(cat "$out")"
fi

# ---------------------------------------------------------------------------
# red: a commit whose ci.yml push run concluded failure (run 32580737899).
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
# no run: a commit ci.yml never saw. commit-tree moves no ref, so it is
# never on master.
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
# in progress: the script must poll at least once, then give up on the
# limit without claiming a conclusion.
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
# not a count: an empty fetch must be refused before any polling. A
# regression here hangs for the default CI_WAIT_LIMIT; a shorter one would
# catch it just as well.
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
