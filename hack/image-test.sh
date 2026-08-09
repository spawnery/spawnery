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

# No --user: internal/podspec sets RunAsNonRoot with no RunAsUser, so the
# kubelet (and, here, the container runtime) resolves the identity entirely
# from the image's own config.User. Passing --user would override the one
# field this test exists to validate; the assertion below checks it instead.
#
# No --security-opt seccomp=... either: the podspec's PodSecurityContext sets
# SeccompProfile: RuntimeDefault, which is exactly what a container runtime
# applies when no seccomp option is given at all. Passing
# --security-opt seccomp=unconfined here would be the one thing that broke
# that parity, so the absence of the flag is the replication, not an
# oversight.
"$CONTAINER" run -d --name "$NAME" \
	--network none \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME:/data" \
	-e SPAWNERY_MAX_PLAYERS=100 \
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

# The plugin is in the image but has no operator to reach here. Two things have
# to be true, and neither is visible from a green ping alone.
#
# That it loaded at all: a paper-plugin.yml naming a class that is not there,
# or an api-version Paper rejects, produces a server that starts perfectly and
# silently has no agent.
if ! grep -q 'spawnery agent dormant' <<<"$container_logs"; then
	echo "the agent plugin did not load, or did not report why it stayed dormant:" >&2
	grep -iE 'spawnery|plugin' <<<"$container_logs" >&2 || true
	exit 1
fi
echo "the agent plugin loaded and stayed dormant without an operator"

# And that nothing broke at class load. Note what this does NOT prove: with no
# operator endpoint the plugin goes dormant before SessionLoop, OperatorChannel
# or BearerCredentials are ever constructed, and those are the classes that
# import io.grpc. Class loading is lazy, so the shaded gRPC tree is never
# touched in this run. A shading regression confined to the operator-connection
# path would pass here. That proof is make agent-test's, in the next task.
if grep -qE 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs"; then
	echo "a linkage error appeared while loading the plugin:" >&2
	grep -B2 -A10 -E 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs" >&2
	exit 1
fi
echo "the plugin's own classes load without a linkage error"

# SIGTERM reaches PID 1 and saves the world. Without exec in the entrypoint the
# grace period would run out empty and every stop would lose world state.
"$CONTAINER" stop -t 60 "$NAME" >/dev/null
container_logs="$("$CONTAINER" logs "$NAME" 2>&1)"
# A download attempted only during shutdown would go unseen by the check
# above, which only ever saw the pre-shutdown log; re-run it against the log
# that already exists here for the SIGTERM assertion below.
check_no_download "$container_logs"
if ! grep -q 'All dimensions are saved' <<<"$container_logs"; then
	echo "SIGTERM did not produce a clean shutdown:" >&2
	tail -30 <<<"$container_logs" >&2
	exit 1
fi
echo "clean shutdown on SIGTERM"

echo "image-test: ok"
