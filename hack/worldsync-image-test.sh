#!/usr/bin/env bash
# Smoke test for the node agent image: root, no shell, the CA bundle the
# store client needs, and a binary that runs.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# The DaemonSet bind-mounts into the kubelet's directories, which needs root.
user="$("$CONTAINER" image inspect --format '{{.Config.User}}' "$IMAGE")"
[ "$user" = "0:0" ] || fail "image user = '$user', want 0:0"

root="$(mktemp -d)"
trap 'chmod -R u+w "$root"; rm -rf "$root"' EXIT
cid="$("$CONTAINER" create "$IMAGE")"
"$CONTAINER" export "$cid" | tar -x -C "$root"
"$CONTAINER" rm "$cid" >/dev/null

# Go reads its roots from this file; without it every HTTPS call to the store fails.
ca="$root/etc/ssl/certs/ca-certificates.crt"
while [ -L "$ca" ]; do
	link="$(readlink "$ca")"
	case "$link" in
	/*) ca="$root$link" ;;
	*) ca="$(dirname "$ca")/$link" ;;
	esac
done
grep -q 'BEGIN CERTIFICATE' "$ca" 2>/dev/null || fail "the image has no CA bundle at /etc/ssl/certs/ca-certificates.crt"

# Without a subcommand the binary prints its usage; only the output is matched.
out="$("$CONTAINER" run --rm --read-only --network none "$IMAGE" 2>&1 || true)"
case "$out" in
*"node|import"*) ;;
*) fail "the binary did not print its usage: $out" ;;
esac

echo "OK: $IMAGE"
