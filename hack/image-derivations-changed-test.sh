#!/usr/bin/env bash
# Five cases for hack/image-derivations-changed.sh, every one against this
# repository's own history.
#
# There is no seam here and none is wanted. The others in this directory need
# one because they ask api.github.com a question whose answer cannot be
# summoned to order; this one asks git a question about commits that already
# exist and will keep existing. Unlike hack/require-green-ci-test.sh, which
# expires when GitHub drops its workflow runs after about 90 days, nothing here
# has a shelf life: git does not garbage-collect reachable history.
#
# Requires: nothing but a full clone. Run it inside the dev shell for
# consistency with its neighbours: `make image-derivations-changed-test`.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

sut="./hack/image-derivations-changed.sh"

failures=0
pass() { echo "ok   - $1"; }
fail() {
	echo "FAIL - $1" >&2
	failures=$((failures + 1))
}

# Deliberately unset for every case below: the script writes to $GITHUB_OUTPUT
# when it is set, and a test that let it inherit a real one from a runner would
# be writing into that job's outputs.
unset GITHUB_OUTPUT

expect() {
	local want="$1" base="$2" head="$3" why="$4"
	local out
	if ! out="$("$sut" "$base" "$head" 2>&1)"; then
		fail "${why}: the script exited non-zero — it answers, it does not refuse"
		return
	fi
	if printf '%s' "$out" | grep -qx "build=${want}"; then
		pass "${why}: build=${want}"
	else
		fail "${why}: wanted build=${want}; output was: ${out}"
	fi
}

# ---------------------------------------------------------------------------
# Two fixed commits, chosen because git will still hold them in a year.
#
# v0.1.2..v0.2.0 is the release that carried the vendorHash fix: `git diff
# --name-only v0.1.2 v0.2.0 -- nix/ flake.nix flake.lock` names flake.nix. So
# this is not an invented example — it is the real range in which a hash moved,
# and the job this script gates would have built the images across it.
#
# 022a421..a6f766c touches docs/known-issues.md and nothing else, which is the
# ordinary shape of a commit here and the case the whole script exists to make
# cheap.
# ---------------------------------------------------------------------------
expect true 'v0.1.2' 'v0.2.0' 'a range where flake.nix moved'
expect false '022a421' 'a6f766c' 'a range that touched only documentation'

# ---------------------------------------------------------------------------
# The three ways of not knowing. Each must build: the answer that costs runner
# minutes is always preferred to the answer that costs coverage, and this is
# the property most worth pinning, because every one of these is reachable on a
# real runner and none of them is reachable in the ordinary case anyone would
# think to check by hand.
# ---------------------------------------------------------------------------
expect true '0000000000000000000000000000000000000000' 'HEAD' "GitHub's all-zeros base on a branch's first push"
expect true '' 'HEAD' 'an empty base'
expect true 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef' 'HEAD' 'a base this clone does not contain'

# ---------------------------------------------------------------------------
# The wrong argument count is the one case that is not an answer.
# ---------------------------------------------------------------------------
if "$sut" 'only-one-argument' >/dev/null 2>&1; then
	fail 'one argument: expected a non-zero exit'
else
	pass 'one argument: refuses rather than answering'
fi

echo
if [ "${failures}" -ne 0 ]; then
	echo "${failures} case(s) failed" >&2
	exit 1
fi
echo "all cases passed"
