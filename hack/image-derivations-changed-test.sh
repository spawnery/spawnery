#!/usr/bin/env bash
# Cases for hack/image-derivations-changed.sh against this repository's own
# history, so it needs a full clone.
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

# Inherited from a runner, the script would write into that job's outputs.
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

# v0.1.2..v0.2.0 moves flake.nix; 022a421..a6f766c touches only
# docs/reference/known-issues.md.
expect true 'v0.1.2' 'v0.2.0' 'a range where flake.nix moved'
expect false '022a421' 'a6f766c' 'a range that touched only documentation'

expect true '0000000000000000000000000000000000000000' 'HEAD' "GitHub's all-zeros base on a branch's first push"
expect true '' 'HEAD' 'an empty base'
expect true 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef' 'HEAD' 'a base this clone does not contain'

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
