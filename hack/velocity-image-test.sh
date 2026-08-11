#!/usr/bin/env bash
# Smoke test for the Velocity base image.
#
# It runs under the same constraints hack/image-test.sh runs the Paper image
# under, because internal/podspec imposes them on both: no network, a
# read-only root, every capability dropped, no --user override. What differs
# is what the container needs to start at all (spawnery-config refuses
# without a rendered /etc/spawnery) and what a proxy with an empty server
# list can be made to prove.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
DEADLINE="${DEADLINE:-120}"

NAME="spawnery-velocity-image-test-$$"
VOLUME="spawnery-velocity-image-test-$$"
CONFDIR="$(mktemp -d)"

cleanup() {
	"$CONTAINER" rm -f "$NAME" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" >/dev/null 2>&1 || true
	rm -rf "$CONFDIR"
}
trap cleanup EXIT

# The renderer refuses to start without both files - a proxy that came up
# with no forwarding secret would be a security hole, not a convenience. Built
# on the host and mounted read-only, so the container never needs to write to
# /etc/spawnery at all.
#
# mktemp -d makes the directory 0700, readable only by the host user; the
# container reads it as uid 10001, a different identity with no --user
# override to bridge the two. World-readable permissions are what make the
# bind mount legible from inside, the same way a projected ConfigMap volume
# would be in the cluster.
printf 'playerLimit: 500\n' >"$CONFDIR/config.yaml"
# No trailing newline: internal/render.Load now refuses a secret whose raw
# bytes are not already its own trimmed form, rather than trimming it away —
# see hack/image-test.sh for why, and internal/render/load.go for the check.
printf 'test-forwarding-secret' >"$CONFDIR/forwarding.secret"
chmod 755 "$CONFDIR"
chmod 644 "$CONFDIR/config.yaml" "$CONFDIR/forwarding.secret"

# A named volume rather than a host directory, for the same reason as the
# Paper test: the container writes /data as uid 10001, and cleaning those
# files up from the host afterwards has nothing to do with what is tested
# here.
"$CONTAINER" volume create "$VOLUME" >/dev/null

# No --user: see hack/image-test.sh for why, and the assertion below is what
# that absence relies on.
"$CONTAINER" run -d --name "$NAME" \
	--network none \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 1g \
	-v "$VOLUME:/data" \
	-v "$CONFDIR:/etc/spawnery:ro" \
	"$IMAGE" >/dev/null

identity="$("$CONTAINER" exec "$NAME" id -u)"
if [ "$identity" != "10001" ]; then
	echo "container runs as uid $identity, want 10001 from the image's own config.User" >&2
	exit 1
fi
echo "runs as uid 10001, from the image's own config.User"

# The port answering is the only claim 3b can make. Velocity's [servers] table
# is empty until the agent in 3c registers backends over the operator
# channel, so a player who actually tries to join is disconnected with "no
# available server" - correct behaviour, not a failure of this test. /dev/tcp
# is a bash builtin, not a tool added to the image for this probe's
# convenience; bash already ships because the entrypoint's shebang points at
# it.
echo "waiting up to ${DEADLINE}s for 25565 to accept a connection..."
start=$SECONDS
until "$CONTAINER" exec "$NAME" bash -c 'exec 3<>/dev/tcp/127.0.0.1/25565' 2>/dev/null; do
	if [ -z "$("$CONTAINER" ps -q --filter "name=^${NAME}$")" ]; then
		echo "the container exited before the port answered:" >&2
		"$CONTAINER" logs "$NAME" >&2
		exit 1
	fi
	if [ $((SECONDS - start)) -gt "$DEADLINE" ]; then
		echo "no connection accepted on 25565 within ${DEADLINE}s:" >&2
		"$CONTAINER" logs "$NAME" >&2
		exit 1
	fi
	sleep 2
done
echo "the port answered after $((SECONDS - start))s"

# online-mode = true alone is not a renderer-unique marker: Velocity's own
# generated default-velocity.toml carries the identical line, so a broken
# entrypoint that skipped spawnery-config entirely and let Velocity write its
# own defaults would still pass this one check - and would then be caught
# only at the port probe above, 120 seconds later, with a message that names
# the wrong cause. forwarding-secret-file and forced modern forwarding are
# both strings only the renderer writes; checking all three in the file
# Velocity actually parsed is what makes this the one place the paired
# invariant - the proxy authenticates players, the backends trust what it
# forwards - is checked against reality rather than a unit test's output.
rendered="$("$CONTAINER" exec "$NAME" cat /data/velocity.toml)"
if ! grep -qE '^online-mode = true$' <<<"$rendered"; then
	echo "velocity.toml does not set online-mode = true:" >&2
	echo "$rendered" >&2
	exit 1
fi
if ! grep -q 'forwarding-secret-file = .*/etc/spawnery/forwarding.secret' <<<"$rendered"; then
	echo "velocity.toml does not point forwarding-secret-file at the mounted secret:" >&2
	echo "$rendered" >&2
	exit 1
fi
# Backends run online-mode=false and trust whatever the proxy forwards; any
# mode but modern leaves them unable to verify a forwarded player at all.
if ! grep -qE "player-info-forwarding-mode = .modern." <<<"$rendered"; then
	echo "velocity.toml is not on modern forwarding:" >&2
	echo "$rendered" >&2
	exit 1
fi
echo "velocity.toml renders online-mode = true, modern forwarding, and the mounted secret path"

# exec in the entrypoint puts the JVM at PID 1 so it receives SIGTERM
# directly; without it, a proxy would never see the signal and would drop
# every player on it instead of draining. "Shutting down the proxy..." is
# Velocity's own log line for a clean stop, the same signal
# hack/image-test.sh checks for Paper's "All dimensions are saved".
"$CONTAINER" stop -t 60 "$NAME" >/dev/null
container_logs="$("$CONTAINER" logs "$NAME" 2>&1)"
if ! grep -q 'Shutting down the proxy' <<<"$container_logs"; then
	echo "SIGTERM did not produce a clean shutdown:" >&2
	tail -30 <<<"$container_logs" >&2
	exit 1
fi
echo "clean shutdown on SIGTERM"

echo "image-test: ok"
