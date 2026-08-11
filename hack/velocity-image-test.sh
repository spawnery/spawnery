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
printf 'test-forwarding-secret\n' >"$CONFDIR/forwarding.secret"
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

# The plugin is in the image but has no operator to reach here, exactly as in
# hack/image-test.sh. Two things have to be true, and neither is visible from a
# green port probe alone.
#
# That it loaded at all: a velocity-plugin.json naming a class that is not
# there, a malformed descriptor, or a main class Guice cannot construct each
# produce a proxy that starts perfectly and silently has no agent. The
# dormancy line names SPAWNERY_OPERATOR_ENDPOINT because none of the three
# proxy variables is set in this run and ProxyEnvironment checks the endpoint
# first -- so this assertion also pins that ordering, which is the only reason
# the message here is about an operator rather than about a player limit
# nobody set.
container_logs="$("$CONTAINER" logs "$NAME" 2>&1)"
if ! grep -q 'spawnery agent dormant.*SPAWNERY_OPERATOR_ENDPOINT' <<<"$container_logs"; then
	echo "the agent plugin did not load, or did not report why it stayed dormant:" >&2
	grep -iE 'spawnery|plugin' <<<"$container_logs" >&2 || true
	exit 1
fi
echo "the agent plugin loaded and stayed dormant without an operator"

# And that nothing broke at class load. The dormancy line is itself most of
# that proof: it is printed from the plugin's own code, after Velocity read the
# descriptor, loaded the class and Guice constructed it with an injected
# ProxyServer and Logger.
#
# Note what this does NOT prove, the same caveat hack/image-test.sh carries:
# with no operator endpoint the plugin goes dormant before SessionLoop,
# OperatorChannel or BearerCredentials are ever constructed, and those are the
# classes that import io.grpc. Class loading is lazy, so the shaded gRPC tree
# is never touched in this run, and a shading regression confined to the
# operator-connection path would pass here.
if grep -qE 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs"; then
	echo "a linkage error appeared while loading the plugin:" >&2
	grep -B2 -A10 -E 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs" >&2
	exit 1
fi
echo "the plugin's own classes load without a linkage error"

# And that it did nothing else. The ready gate is internal/podspec.ProxyReadyPort
# and the only thing that binds it; a dormant agent must leave it closed, or a
# proxy with no operator, no server list and nothing to route to would still
# turn its pod ready and then disconnect every player with "no available
# server".
#
# This is the assertion task 7 has to keep true when it opens the gate for
# real: the gate belongs behind the first FullSync, not behind plugin load.
# There is no race to wait out -- ProxyInitializeEvent fires before Velocity
# logs "Listening on ...:25565", which is what the wait loop above already
# blocked on, so by here the plugin has run to completion.
if "$CONTAINER" exec "$NAME" bash -c 'exec 3<>/dev/tcp/127.0.0.1/8081' 2>/dev/null; then
	echo "the ready gate is open on 8081 with no operator; a dormant agent must not bind it:" >&2
	grep -iE 'spawnery' <<<"$container_logs" >&2 || true
	exit 1
fi
echo "the ready gate stayed closed, so the pod would not turn ready"

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
