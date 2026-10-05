#!/usr/bin/env bash
# hack/aot-train.sh must fail, and say why, when the server never reaches
# Done: a release must not publish a cache from a run that did not boot.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"

out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT

status=0
msg="$(CONTAINER="$CONTAINER" IMAGE="$IMAGE" OUT="$out" DEADLINE=1 hack/aot-train.sh 2>&1)" || status=$?
if [ "$status" -eq 0 ]; then
	echo "aot-train.sh succeeded with a 1s deadline:" >&2
	echo "$msg" >&2
	exit 1
fi
if ! grep -q 'never reached Done' <<<"$msg"; then
	echo "aot-train.sh failed without saying the server never reached Done:" >&2
	echo "$msg" >&2
	exit 1
fi
if [ -e "$out/server.aot" ]; then
	echo "aot-train.sh left a server.aot behind after a failed run" >&2
	exit 1
fi
echo "aot-train-test: ok"
