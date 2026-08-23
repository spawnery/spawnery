#!/usr/bin/env bash
# Decides whether the Paper and Velocity image derivations need building for a
# given range of commits.
#
# ci.yml builds .#operator-image and no other image, so the two fixed-output
# hashes in nix/paper.nix and the one in nix/velocity.nix are fetched against
# by nothing on the commit loop. Building all three images on every push was
# weighed and rejected -- minutes of runner time for derivations that change a
# handful of times a year. This is the middle: build them exactly when
# something that defines them moved.
#
# Usage:
#   hack/image-derivations-changed.sh <base-sha> <head-sha>
#
# It writes `build=true` or `build=false` to $GITHUB_OUTPUT when that variable
# is set, and prints the same line to stdout regardless, so it can be run by
# hand against any two commits:
#
#   hack/image-derivations-changed.sh v0.1.2 v0.2.0
#
# Exit status is 0 for both answers. A refusal to answer is not one of the
# outcomes: every path that cannot determine the diff answers `true`, which is
# the direction that costs runner minutes rather than coverage.
#
# **Every uncertainty builds.** A base of all zeros (a branch's first push), an
# empty base, a base the runner's clone does not contain, a `git diff` that
# fails for any reason -- each of those is a case where the honest answer is "I
# cannot tell what moved", and the cheap wrong answer would be to skip. Skipping
# is the failure this script exists to prevent, so it is never the fallback.
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: hack/image-derivations-changed.sh <base-sha> <head-sha>" >&2
	exit 1
fi
base="$1"
head="$2"

# Everything the two derivations are built from. nix/ entire rather than the
# four files that name the hashes: oci-common.nix is shared machinery both
# images go through, agents.nix produces the jars both embed, and a list that
# names files individually is a list somebody has to remember to extend when
# they add a fifth. flake.nix carries the derivation definitions themselves and
# flake.lock the nixpkgs they resolve against, either of which can change what
# these builds do without a line under nix/ moving.
#
# ci.yml is here so that editing this job's own definition exercises it once.
paths=(
	'nix/'
	'flake.nix'
	'flake.lock'
	'.github/workflows/ci.yml'
	'hack/image-derivations-changed.sh'
)

answer() {
	echo "build=$1"
	if [ -n "${GITHUB_OUTPUT:-}" ]; then
		echo "build=$1" >>"${GITHUB_OUTPUT}"
	fi
	exit 0
}

# All zeros is what GitHub sends for `before` on a branch's first push: there
# is no earlier commit to diff against, so nothing can be ruled out.
if [ -z "${base}" ] || [ "${base}" = "0000000000000000000000000000000000000000" ]; then
	echo "::notice::no usable base commit (${base:-empty}), so building rather than guessing"
	answer true
fi

if ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
	echo "::notice::base ${base} is not in this clone, so building rather than guessing"
	answer true
fi

changed=""
if ! changed="$(git diff --name-only "${base}" "${head}" -- "${paths[@]}")"; then
	echo "::notice::could not diff ${base}..${head}, so building rather than guessing"
	answer true
fi

if [ -n "${changed}" ]; then
	echo "::notice::building the Paper and Velocity images; these moved:"
	echo "${changed}"
	answer true
fi

# The only path that skips, and it is the one where the question has a definite
# negative answer: nothing these derivations are built from changed between the
# two commits, so their fixed-output hashes cannot have moved either.
echo "::notice::nothing under nix/, flake.nix or flake.lock moved between ${base} and ${head}; not rebuilding the Paper and Velocity images. nightly.yml still builds them every night, which is what covers a hash breaking because bytes at a URL changed rather than because this repository did."
answer false
