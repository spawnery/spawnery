#!/usr/bin/env bash
# Decides whether the Paper and Velocity image derivations need building for a
# given range of commits.
#
# Building them on every push costs minutes for derivations that change a few
# times a year; ci.yml otherwise builds only the operator image.
#
# Usage:
#   hack/image-derivations-changed.sh <base-sha> <head-sha>
#
# Prints `build=true` or `build=false`, and appends it to $GITHUB_OUTPUT when set.
#
# Exit status is 0 for both answers. Every uncertainty builds: skipping costs
# coverage, building only runner minutes.
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: hack/image-derivations-changed.sh <base-sha> <head-sha>" >&2
	exit 1
fi
base="$1"
head="$2"

# nix/ whole, so a new shared file there needs no entry. ci.yml is here so that
# editing this job's own definition exercises it once.
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

# All zeros is GitHub's `before` on a branch's first push.
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

echo "::notice::nothing under nix/, flake.nix or flake.lock moved between ${base} and ${head}; not rebuilding the Paper and Velocity images. nightly.yml still builds them every night, which is what covers a hash breaking because bytes at a URL changed rather than because this repository did."
answer false
