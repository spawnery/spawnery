#!/usr/bin/env bash
# Smoke test for the Velocity base image, under the same constraints as hack/image-test.sh.
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

# The renderer refuses to start without both files, and without onlineMode.
# World-readable because the container reads them as uid 10001.
#
# 137 and an unusual motd so the status ping below shows what Velocity parsed;
# 500 is both Velocity's default and podspec.DefaultPlayerLimit.
PLAYER_LIMIT=137
MOTD="spawnery image test motd"
printf 'playerLimit: %s\nonlineMode: true\nmotd: "%s"\n' "$PLAYER_LIMIT" "$MOTD" >"$CONFDIR/config.yaml"
printf 'test-forwarding-secret\n' >"$CONFDIR/forwarding.secret"
chmod 755 "$CONFDIR"
chmod 644 "$CONFDIR/config.yaml" "$CONFDIR/forwarding.secret"

# A named volume: files written as uid 10001 are hard to clean up from the host.
"$CONTAINER" volume create "$VOLUME" >/dev/null

# No --user: the image's User decides, as in hack/image-test.sh.
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

# Only the port: with no backends registered, a real join is disconnected by design.
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

# What spawnery-config wrote, not what Velocity read: Velocity never rewrites
# velocity.toml here. The status ping and the secret check below cover parsing.
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
# Backends run online-mode=false; only modern forwarding lets them verify a forwarded player.
if ! grep -qE "player-info-forwarding-mode = .modern." <<<"$rendered"; then
	echo "velocity.toml is not on modern forwarding:" >&2
	echo "$rendered" >&2
	exit 1
fi
echo "velocity.toml renders online-mode = true, modern forwarding, and the mounted secret path"

# show-max-players and motd come back in the status ping, so a misspelled key,
# which Velocity silently replaces with its default, shows up here.
#
# Hand-written status request (the image has no ping client): a handshake for
# protocol 0, "localhost", port 25565 (0x63dd), next state 1, then the status
# request. Velocity keeps the connection open, so timeout always exits 124.
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

# A forwarding-secret-file Velocity cannot resolve makes it create a random
# forwarding.secret in /data and start normally, refusing every forwarded join.
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

# A bad velocity-plugin.json starts a healthy proxy with no agent. The message
# names SPAWNERY_OPERATOR_ENDPOINT because ProxyEnvironment checks it first.
container_logs="$("$CONTAINER" logs "$NAME" 2>&1)"
if ! grep -q 'spawnery agent dormant.*SPAWNERY_OPERATOR_ENDPOINT' <<<"$container_logs"; then
	echo "the agent plugin did not load, or did not report why it stayed dormant:" >&2
	grep -iE 'spawnery|plugin' <<<"$container_logs" >&2 || true
	exit 1
fi
echo "the agent plugin loaded and stayed dormant without an operator"

# Without an operator the gRPC classes are never loaded, so this misses a shading
# regression on the connection path.
if grep -qE 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs"; then
	echo "a linkage error appeared while loading the plugin:" >&2
	grep -B2 -A10 -E 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs" >&2
	exit 1
fi
echo "the plugin's own classes load without a linkage error"

# A dormant agent must leave the ready gate (podspec.ProxyReadyPort) closed. No race:
# ProxyInitializeEvent fires before Velocity starts listening on 25565.
if "$CONTAINER" exec "$NAME" bash -c 'exec 3<>/dev/tcp/127.0.0.1/8081' 2>/dev/null; then
	echo "the ready gate is open on 8081 with no operator; a dormant agent must not bind it:" >&2
	grep -iE 'spawnery' <<<"$container_logs" >&2 || true
	exit 1
fi
echo "the ready gate stayed closed, so the pod would not turn ready"

"$CONTAINER" stop -t 60 "$NAME" >/dev/null
container_logs="$("$CONTAINER" logs "$NAME" 2>&1)"
if ! grep -q 'Shutting down the proxy' <<<"$container_logs"; then
	echo "SIGTERM did not produce a clean shutdown:" >&2
	tail -30 <<<"$container_logs" >&2
	exit 1
fi
echo "clean shutdown on SIGTERM"

echo "image-test: ok"
