#!/usr/bin/env bash
# Smoke test for the operator image, under the Deployment's constraints:
# non-root, read-only root filesystem, no network, and no tmpfs or volume.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# The Deployment sets runAsNonRoot with no runAsUser, so the image's User decides.
user="$("$CONTAINER" image inspect --format '{{.Config.User}}' "$IMAGE")"
[ "$user" = "10001:10001" ] || fail "image user = '$user', want 10001:10001"

workdir="$("$CONTAINER" image inspect --format '{{.Config.WorkingDir}}' "$IMAGE")"
[ "$workdir" = "/" ] || fail "image workingDir = '$workdir', want /"

# Exported rather than checked with `test -d`: the image has no shell.
cid="$("$CONTAINER" create "$IMAGE")"
# Not piped into grep -q: its early exit SIGPIPEs tar, and pipefail turns a match into a miss.
entries="$("$CONTAINER" export "$cid" | tar -t)"
if grep -qE '^(\./)?data/?$' <<<"$entries"; then
	"$CONTAINER" rm "$cid" >/dev/null
	fail "the image has a /data directory; it should carry no writable directory of its own"
fi
# The store client verifies TLS against the roots Go reads from this file.
root="$(mktemp -d)"
trap 'chmod -R u+w "$root"; rm -rf "$root"' EXIT
"$CONTAINER" export "$cid" | tar -x -C "$root"
"$CONTAINER" rm "$cid" >/dev/null
ca="$root/etc/ssl/certs/ca-certificates.crt"
while [ -L "$ca" ]; do
	link="$(readlink "$ca")"
	case "$link" in
	/*) ca="$root$link" ;;
	*) ca="$(dirname "$ca")/$link" ;;
	esac
done
grep -q 'BEGIN CERTIFICATE' "$ca" 2>/dev/null || fail "the image has no CA bundle at /etc/ssl/certs/ca-certificates.crt"

# Go's flag package prints usage for -h; only the output is matched, not the exit code.
out="$("$CONTAINER" run --rm --read-only --network none "$IMAGE" -h 2>&1 || true)"
for flag in startup-deadline leader-elect metrics-bind-address health-probe-bind-address; do
	case "$out" in
	*"-$flag"*) ;;
	*) fail "the operator's usage does not mention -$flag; the Deployment passes it" ;;
	esac
done

echo "OK: $IMAGE"
