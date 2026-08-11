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
#
# onlineMode is written explicitly because render.Velocity refuses to guess it
# - it is the property that authenticates players, and the operator always
# writes the key. true here: nothing in this test joins the proxy, so the
# offline-mode case has no bearing on what it measures.
#
# playerLimit is 137 and not 500 on purpose, and the motd is a string nothing
# else in this repository writes. Both come back out of the status ping below,
# which is the only way this script can see what Velocity parsed rather than
# what the renderer wrote - and 500 would be invisible there, because it is
# both Velocity's own default for show-max-players and
# internal/podspec.DefaultPlayerLimit.
PLAYER_LIMIT=137
MOTD="spawnery image test motd"
printf 'playerLimit: %s\nonlineMode: true\nmotd: "%s"\n' "$PLAYER_LIMIT" "$MOTD" >"$CONFDIR/config.yaml"
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

# What is on disk at the path Velocity opens - and no more than that.
#
# This block used to claim it checked "the file Velocity actually parsed". It
# does not, and cannot: Velocity never rewrites velocity.toml here.
# VelocityConfiguration.read loads it through night-config's .autosave(), which
# writes only when something in the config changes, and nothing changes it.
# Disassembled out of the pinned jar, 2026-08-11: five of its six migrations
# are version-gated with `configVersion < threshold` for thresholds 2.0, 2.0,
# 2.6, 2.7 and 2.8, and the renderer writes exactly "2.8", so none of them
# fires; the sixth, MiniMessageTranslationsMigration, always says it should run
# but only ever rewrites files under lang/ and never touches the config.
# Measured the same day against the pinned image: /data/velocity.toml after a
# clean start is byte for byte spawnery-config's own 255-byte output,
# comment-free and in go-toml's key order, where a file Velocity had written
# would carry its own comments and its [advanced], [query] and [packet-limiter]
# tables.
#
# So what the three greps below establish is that spawnery-config ran, wrote
# all three critical strings, and put them where Velocity opens them. That is
# worth asserting - a broken entrypoint that skipped the renderer would leave
# Velocity to generate its own defaults here, which is a different file
# entirely, and would otherwise be caught only at the port probe above with a
# message naming the wrong cause. What it does not establish is that Velocity
# read any of it. Everything after this block is what does.
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

# The same file, as Velocity parsed it.
#
# A server list ping is the whole of what a proxy with no operator and no
# backends will tell an outsider, and two of the renderer's keys come back out
# of it: show-max-players as players.max and motd as description. That is the
# measurement of the receiving program this script was missing - the failure it
# closes is a misspelled key, which velocity.toml carries as happily as a
# correct one and which Velocity answers by silently keeping its own default.
# internal/render's TestVelocityWritesTheKeysVelocityItselfReads is the
# fixture-based half of the same guard; this half needs no fixture refreshed
# and would also catch a spawnery-config that wrote a correct file to the wrong
# place.
#
# The handshake is written by hand because the image carries no ping client
# (spawnery-slp is in the Paper image, not this one): a 15-byte handshake -
# packet id 0x00, protocol version 0, host "localhost", port 25565 (0x63dd),
# next state 1 - followed by the 1-byte status request. `timeout 5 cat` rather
# than `head -c N` because the response length is not known in advance and
# Velocity holds the connection open afterwards, waiting for a ping packet this
# probe never sends. `|| true` because that is exactly why timeout exits 124
# every time, and this script runs under `set -o pipefail`. tr strips the
# packet framing around the JSON.
echo "asking the proxy for its status, to see what Velocity made of velocity.toml..."
ping_json="$("$CONTAINER" exec "$NAME" bash -c '
	exec 3<>/dev/tcp/127.0.0.1/25565
	printf "\x0f\x00\x00\x09localhost\x63\xdd\x01\x01\x00" >&3
	timeout 5 cat <&3 || true
' 2>/dev/null | LC_ALL=C tr -cd '[:print:]')"
if ! grep -q "\"max\":$PLAYER_LIMIT" <<<"$ping_json"; then
	echo "the proxy reports a maximum other than the rendered show-max-players = $PLAYER_LIMIT; Velocity did not read the key the renderer wrote:" >&2
	echo "$ping_json" >&2
	exit 1
fi
if ! grep -qF "$MOTD" <<<"$ping_json"; then
	echo "the proxy's status does not carry the rendered motd; Velocity did not read the key the renderer wrote:" >&2
	echo "$ping_json" >&2
	exit 1
fi
echo "Velocity answers with the rendered show-max-players and motd, so it parsed the file"

# The third critical key, checked by what Velocity did not do.
#
# forwarding-secret-file cannot be read back out of anything: no log line names
# the secret, and only a forwarded join would exercise it. What can be observed
# is the branch a misspelled key takes. VelocityConfiguration.read looks the key
# up, falls back to the relative path "forwarding.secret" when it is absent,
# and - finding no such file - creates it in the working directory, fills it
# with twelve random characters, logs "The forwarding-secret-file does not
# exist. A new file has been created at {}", and starts perfectly. /data is the
# working directory and is the one writable place in this container, so the file
# really would appear. Every other check in this script stays green in that
# state, the port probe passes, and every forwarded join is refused with a
# secret neither backend shares - the same shape of silent failure milestone 3c
# found on the Paper side.
if "$CONTAINER" exec "$NAME" test -e /data/forwarding.secret; then
	echo "Velocity generated its own /data/forwarding.secret, so it did not resolve forwarding-secret-file to the mounted one; every forwarded join would be refused:" >&2
	"$CONTAINER" exec "$NAME" cat /data/forwarding.secret >&2 || true
	exit 1
fi
if grep -q 'forwarding-secret-file does not exist' <<<"$("$CONTAINER" logs "$NAME" 2>&1)"; then
	echo "Velocity says the forwarding-secret-file it looked for does not exist:" >&2
	"$CONTAINER" logs "$NAME" 2>&1 | grep -i 'forwarding' >&2 || true
	exit 1
fi
echo "Velocity resolved forwarding-secret-file to the mount and generated no secret of its own"

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
