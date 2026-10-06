#!/usr/bin/env bash
# Smoke test for the Paper base image, under internal/podspec's constraints and
# with no network, so a runtime download from Mojang fails here.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
DEADLINE="${DEADLINE:-180}"

NAME="spawnery-image-test-$$"
VOLUME="spawnery-image-test-$$"
CONFDIR="$(mktemp -d)"

SUBNAME="$NAME-substitute"
SUBVOLUME="$VOLUME-substitute"
SUBDIR=""
AOTNAME="$NAME-aot"
AOTVOLUME="$VOLUME-aot"
AOTDIR=""
cleanup() {
	"$CONTAINER" rm -f "$NAME" "$SUBNAME" "$AOTNAME" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" "$SUBVOLUME" "$AOTVOLUME" >/dev/null 2>&1 || true
	rm -rf "$CONFDIR" ${SUBDIR:+"$SUBDIR"} ${AOTDIR:+"$AOTDIR"}
}
trap cleanup EXIT

# The renderer refuses to start without both files. World-readable because the
# container reads them as uid 10001.
printf 'maxPlayers: 100\n' >"$CONFDIR/config.yaml"
printf 'test-forwarding-secret\n' >"$CONFDIR/forwarding.secret"
chmod 755 "$CONFDIR"
chmod 644 "$CONFDIR/config.yaml" "$CONFDIR/forwarding.secret"

# A named volume: files written as uid 10001 are hard to clean up from the host.
"$CONTAINER" volume create "$VOLUME" >/dev/null

# No --user: podspec sets RunAsNonRoot without RunAsUser, so the image's User decides.
# No seccomp option: the runtime default is what the podspec's RuntimeDefault means.
"$CONTAINER" run -d --name "$NAME" \
	--network none \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME:/data" \
	-v "$CONFDIR:/etc/spawnery:ro" \
	"$IMAGE" >/dev/null

identity="$("$CONTAINER" exec "$NAME" id -u)"
if [ "$identity" != "10001" ]; then
	echo "container runs as uid $identity, want 10001 from the image's own config.User" >&2
	exit 1
fi
echo "runs as uid 10001, from the image's own config.User"

echo "waiting up to ${DEADLINE}s for a server list ping..."
start=$SECONDS
until "$CONTAINER" exec "$NAME" /usr/local/bin/spawnery-slp --host 127.0.0.1 --port 25565 >/dev/null 2>&1; do
	if [ -z "$("$CONTAINER" ps -q --filter "name=^${NAME}$")" ]; then
		echo "the container exited before answering:" >&2
		"$CONTAINER" logs "$NAME" >&2
		exit 1
	fi
	if [ $((SECONDS - start)) -gt "$DEADLINE" ]; then
		echo "no server list ping within ${DEADLINE}s:" >&2
		"$CONTAINER" logs "$NAME" >&2
		exit 1
	fi
	sleep 2
done
echo "the server answered after $((SECONDS - start))s"

# Matches only an artifact fetch: Paper's other outbound calls (Yggdrasil key,
# update checker) fail harmlessly offline. Here-string rather than a pipe into
# grep -q, whose early exit SIGPIPEs the writer and pipefail turns into a miss.
check_no_download() {
	if grep -qiE 'piston-data|Downloading mojang_|Failed to download' <<<"$1"; then
		echo "the image tried to download the Paper/Mojang artifact at runtime:" >&2
		echo "$1" >&2
		exit 1
	fi
}

container_logs="$("$CONTAINER" logs "$NAME" 2>&1)"
check_no_download "$container_logs"
echo "no download attempted"

# Paper rewrites paper-global.yml on load and ignores, but keeps, keys it does
# not know, so reading it back is the only proof Paper consumed the override.
# A presence check on the value; an absent-error grep survives upstream rewording.
effective_global="$("$CONTAINER" exec "$NAME" cat /data/config/paper-global.yml)"

# The renderer writes the same velocity keys Paper writes back; it writes
# neither _version nor bungee-cord, so their presence proves Paper rewrote proxies.
for want in '_version:' '  bungee-cord:'; do
	if ! grep -qF "$want" <<<"$effective_global"; then
		echo "/data/config/paper-global.yml carries no \"$want\", so Paper never rewrote it; what follows is the renderer's own output and proves nothing about what Paper read:" >&2
		echo "$effective_global" >&2
		exit 1
	fi
done

# Narrowed first: Paper's spark and update-checker sections also say "enabled: true".
velocity_block="$(awk '
	/^  velocity:/ { inblock = 1; next }
	inblock && /^    / { print; next }
	inblock { exit }
' <<<"$effective_global")"
if [ -z "$velocity_block" ]; then
	echo "Paper wrote no proxies.velocity block at all:" >&2
	echo "$effective_global" >&2
	exit 1
fi
for want in 'enabled: true' 'secret: test-forwarding-secret'; do
	if ! grep -qF "$want" <<<"$velocity_block"; then
		echo "Paper's proxies.velocity does not say \"$want\"; forwarding is off and every join through a proxy is refused with \"Your server did not send a forwarding request to the proxy\":" >&2
		echo "$velocity_block" >&2
		exit 1
	fi
done
echo "Paper read the forwarding secret and enabled Velocity forwarding"

# A bad paper-plugin.yml starts a healthy server with no agent.
if ! grep -q 'spawnery agent dormant' <<<"$container_logs"; then
	echo "the agent plugin did not load, or did not report why it stayed dormant:" >&2
	grep -iE 'spawnery|plugin' <<<"$container_logs" >&2 || true
	exit 1
fi
echo "the agent plugin loaded and stayed dormant without an operator"

# Without an operator the gRPC classes are never loaded, so this misses a shading
# regression on the connection path; make agent-test covers that.
if grep -qE 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs"; then
	echo "a linkage error appeared while loading the plugin:" >&2
	grep -B2 -A10 -E 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs" >&2
	exit 1
fi
echo "the plugin's own classes load without a linkage error"

"$CONTAINER" stop -t 60 "$NAME" >/dev/null
container_logs="$("$CONTAINER" logs "$NAME" 2>&1)"
check_no_download "$container_logs"
# Vanilla's "All dimensions are saved" is no longer printed by 26.3.
if ! grep -q 'All RegionFile I/O tasks to complete' <<<"$container_logs"; then
	echo "SIGTERM did not produce a clean shutdown:" >&2
	tail -30 <<<"$container_logs" >&2
	exit 1
fi
echo "clean shutdown on SIGTERM"

# spec.substitution end to end. Waits for the content, not the file: the copy
# lands a moment before the substitution fills it.
SUBDIR="$(mktemp -d)"
mkdir -p "$SUBDIR/Demo"
printf 'password: {{ SECRET_DEMO }}\n' >"$SUBDIR/Demo/config.yml"
chmod -R a+rX "$SUBDIR"
"$CONTAINER" volume create "$SUBVOLUME" >/dev/null
"$CONTAINER" run -d --name "$SUBNAME" \
	--network none \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$SUBVOLUME:/data" \
	-v "$CONFDIR:/etc/spawnery:ro" \
	-v "$SUBDIR:/var/run/spawnery/plugins:ro" \
	-e SPAWNERY_SUBSTITUTION_PREFIX=SECRET_ -e 'SECRET_DEMO=a$b&c' \
	"$IMAGE" >/dev/null
got=""
for _ in $(seq 1 60); do
	got="$("$CONTAINER" exec "$SUBNAME" cat /data/plugins/Demo/config.yml 2>/dev/null || true)"
	[ "$got" = 'password: a$b&c' ] && break
	sleep 1
done
"$CONTAINER" rm -f "$SUBNAME" >/dev/null
"$CONTAINER" volume rm -f "$SUBVOLUME" >/dev/null
rm -rf "$SUBDIR"
if [ "$got" != 'password: a$b&c' ]; then
	echo "substitution: got '$got', want 'password: a\$b&c'" >&2
	exit 1
fi
echo "substitution: a placeholder in a mounted plugin source was filled from the environment"

# The startup cache: trained under one memory limit, mapped under another, and
# dropped with a warning, not a failed start, when a group's JAVA_TOOL_OPTIONS
# changes a flag it depends on.
AOTDIR="$(mktemp -d)"
# mktemp -d is 0700, and the container reads the cache as uid 10001.
chmod 755 "$AOTDIR"
CONTAINER="$CONTAINER" IMAGE="$IMAGE" OUT="$AOTDIR" hack/aot-train.sh
aot_boot() {
	"$CONTAINER" volume create "$AOTVOLUME" >/dev/null
	"$CONTAINER" run -d --name "$AOTNAME" \
		--network none \
		--read-only --tmpfs /tmp:rw,exec,size=256m \
		--cap-drop ALL \
		--security-opt no-new-privileges \
		--memory 3g \
		-v "$AOTVOLUME:/data" \
		-v "$CONFDIR:/etc/spawnery:ro" \
		-v "$AOTDIR:/var/run/spawnery/aot:ro" \
		-e "JAVA_TOOL_OPTIONS=$1" \
		"$IMAGE" >/dev/null
	start=$SECONDS
	until "$CONTAINER" exec "$AOTNAME" /usr/local/bin/spawnery-slp --host 127.0.0.1 --port 25565 >/dev/null 2>&1; do
		if [ $((SECONDS - start)) -gt "$DEADLINE" ] || [ -z "$("$CONTAINER" ps -q --filter "name=^${AOTNAME}$")" ]; then
			echo "no server list ping with the cache mounted (JAVA_TOOL_OPTIONS=$1):" >&2
			"$CONTAINER" logs "$AOTNAME" >&2
			exit 1
		fi
		sleep 1
	done
	echo "answered after $((SECONDS - start))s with the cache mounted (JAVA_TOOL_OPTIONS=$1)"
	aot_logs="$("$CONTAINER" logs "$AOTNAME" 2>&1)"
	"$CONTAINER" rm -f "$AOTNAME" >/dev/null
	"$CONTAINER" volume rm -f "$AOTVOLUME" >/dev/null
}
aot_boot "-Xlog:aot"
if ! grep -q 'Opened AOT cache' <<<"$aot_logs" || grep -qiE 'unable to (use|map)|mismatch' <<<"$aot_logs"; then
	echo "the shipped cache was not used:" >&2
	grep -iE '\[aot' <<<"$aot_logs" | head -30 >&2
	exit 1
fi
echo "the startup cache was mapped"
aot_boot "-Xlog:aot -XX:-UseCompressedOops"
if grep -q 'Opened AOT cache' <<<"$aot_logs" && ! grep -qiE 'unable to (use|map)|mismatch|disabled' <<<"$aot_logs"; then
	echo "the JVM claims to have used a cache trained with compressed oops without them:" >&2
	grep -iE '\[aot' <<<"$aot_logs" | head -30 >&2
	exit 1
fi
echo "a cache that does not fit is dropped and the server starts anyway"
aot_boot "-Xshare:off"
if ! grep -q 'starting without the startup cache' <<<"$aot_logs"; then
	echo "-Xshare:off next to the cache started without the entrypoint leaving the cache out:" >&2
	echo "$aot_logs" | head -20 >&2
	exit 1
fi
echo "a group's -Xshare:off starts without the cache instead of failing"
rm -rf "$AOTDIR"

echo "image-test: ok"
