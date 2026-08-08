#!/usr/bin/env bash
# Smoke test for the Paper base image.
#
# It runs the image under exactly the constraints internal/podspec imposes,
# rather than more comfortable ones, and with no network at all. The offline
# part is the point: the day somebody unpins the pre-patched repo, this test
# fails instead of a pod quietly downloading from Mojang in production.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
DEADLINE="${DEADLINE:-180}"

NAME="spawnery-image-test-$$"
VOLUME="spawnery-image-test-$$"

cleanup() {
	"$CONTAINER" rm -f "$NAME" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A named volume rather than a host directory: the container writes as uid
# 10001, and cleaning those files up from the host afterwards is a fight that
# has nothing to do with what is being tested.
"$CONTAINER" volume create "$VOLUME" >/dev/null

"$CONTAINER" run -d --name "$NAME" \
	--network none \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--user 10001:10001 \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME:/data" \
	-e SPAWNERY_MAX_PLAYERS=100 \
	"$IMAGE" >/dev/null

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

# The promise being guarded is that the Paper/Mojang artifact is provisioned
# at build time, not fetched at runtime - not that the JVM never opens a
# socket for any reason. Paper itself makes a couple of unrelated outbound
# calls on every startup regardless of provisioning (an online-mode
# Yggdrasil key fetch, its own update checker); those fail harmlessly under
# --network none and are not what this check is for. So the pattern matches
# only signs of an artifact fetch, not any network attempt: piston-data is
# Mojang's asset CDN, "Downloading mojang_" is the bundler's own log line for
# fetching the server jar, and "Failed to download" is what it logs when
# that fetch fails.
#
# The logs are captured to a variable before grepping rather than piped
# straight into grep. Piping matters here: under `set -o pipefail`, `grep -q`
# exits as soon as it finds a match and can close its end of the pipe while
# `logs` is still writing, which sends `logs` a SIGPIPE. `grep` itself still
# reports the match (exit 0), but pipefail then reports the *pipeline's*
# status as the `logs` process's non-zero SIGPIPE exit instead - so `if
# logs | grep -q pattern` can silently fail to flag a real match on a log
# long enough to trigger the race. Capturing to a variable first removes the
# pipe (and the race) entirely.
container_logs="$("$CONTAINER" logs "$NAME" 2>&1)"
if grep -qiE 'piston-data|Downloading mojang_|Failed to download' <<<"$container_logs"; then
	echo "the image tried to download the Paper/Mojang artifact at runtime:" >&2
	echo "$container_logs" >&2
	exit 1
fi
echo "no download attempted"

# SIGTERM reaches PID 1 and saves the world. Without exec in the entrypoint the
# grace period would run out empty and every stop would lose world state.
"$CONTAINER" stop -t 60 "$NAME" >/dev/null
container_logs="$("$CONTAINER" logs "$NAME" 2>&1)"
if ! grep -q 'All dimensions are saved' <<<"$container_logs"; then
	echo "SIGTERM did not produce a clean shutdown:" >&2
	tail -30 <<<"$container_logs" >&2
	exit 1
fi
echo "clean shutdown on SIGTERM"

echo "image-test: ok"
