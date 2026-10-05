#!/usr/bin/env bash
# Trains the JVM's AOT cache for a Purpur image and leaves it in
# OUT/server.aot. The image's own entrypoint runs the training, so the cache
# carries exactly the flags every server starts with.
#
# The first boot only creates the world; the training boot loads it, as a
# server with a world does. Training on world creation pushes the JDK's
# MethodType table past the 256 KiB the cache can hold for one object, and the
# cache step fails. 4 GiB: the training JVM and the child that writes the
# cache run side by side.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
OUT="${OUT:?OUT must be set}"
DEADLINE="${DEADLINE:-300}"

NAME="spawnery-aot-train-$$"
VOLUME="spawnery-aot-train-$$"
WORK="$(mktemp -d)"
cleanup() {
	"$CONTAINER" rm -f "$NAME" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

mkdir -p "$WORK/conf/overlay" "$WORK/out"
printf 'maxPlayers: 1\n' >"$WORK/conf/config.yaml"
printf 'aot-training\n' >"$WORK/conf/forwarding.secret"
printf 'level-type=minecraft\\:flat\ngenerate-structures=false\nview-distance=4\n' \
	>"$WORK/conf/overlay/server.properties"
chmod -R a+rX "$WORK/conf"
# The container writes as uid 10001.
chmod 777 "$WORK/out"
"$CONTAINER" volume create "$VOLUME" >/dev/null

# boot LABEL [ENV...] starts the server, sends "stop" once it is Done, removes
# the container at the deadline, and returns once it exited.
boot() {
	local label="$1"
	shift
	local log="$WORK/$label.log"
	: >"$log"
	local env=()
	for e in "$@"; do env+=(-e "$e"); done
	(
		start=$SECONDS
		seen=0
		while :; do
			if grep -q 'Done (' "$log"; then
				: >"$WORK/$label.done"
				break
			fi
			if "$CONTAINER" container inspect "$NAME" >/dev/null 2>&1; then
				seen=1
			elif [ "$seen" = 1 ]; then
				break
			fi
			[ $((SECONDS - start)) -gt "$DEADLINE" ] && break
			sleep 1
		done
		# A server still starting drops a console "stop".
		if [ -e "$WORK/$label.done" ]; then
			echo stop
		else
			"$CONTAINER" rm -f "$NAME" >/dev/null 2>&1 || true
		fi
	) | "$CONTAINER" run -i --rm --name "$NAME" \
		--network none \
		--read-only --tmpfs /tmp:rw,exec,size=256m \
		--cap-drop ALL \
		--security-opt no-new-privileges \
		--memory 4g \
		-v "$VOLUME:/data" \
		-v "$WORK/conf:/etc/spawnery:ro" \
		-v "$WORK/out:/var/run/spawnery/aot-out" \
		"${env[@]}" \
		"$IMAGE" >"$log" 2>&1 || true
	if [ ! -e "$WORK/$label.done" ]; then
		echo "the $label boot never reached Done within ${DEADLINE}s:" >&2
		tail -40 "$log" >&2
		exit 1
	fi
}

boot world
boot training SPAWNERY_AOT_OUTPUT=/var/run/spawnery/aot-out/server.aot

if [ ! -s "$WORK/out/server.aot" ]; then
	echo "the training boot reached Done but wrote no cache:" >&2
	grep -E '\[error|Child process' "$WORK/training.log" | tail -20 >&2 || tail -40 "$WORK/training.log" >&2
	exit 1
fi
mkdir -p "$OUT"
cp "$WORK/out/server.aot" "$OUT/server.aot"
chmod 0444 "$OUT/server.aot"
echo "trained $(du -h "$OUT/server.aot" | cut -f1) for $IMAGE"
