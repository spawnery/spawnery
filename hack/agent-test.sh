#!/usr/bin/env bash
# The Paper agent, proven against a real operator-shaped counterpart.
#
# This is the level that the unit tests structurally cannot reach. Three of the
# agent's obligations only exist inside a real JVM with Paper's classloader, a
# real TLS handshake against the pinned CA, and a real HTTP/2 stream:
#
#   - that the shaded gRPC stack does not meet Paper's own protobuf and Netty,
#   - that the channel trusts the mounted bundle and nothing else,
#   - that a renewal really overlaps rather than merely being scheduled.
#
# The last one is what docs/known-issues.md calls non-optional, and a unit test
# can only claim it.
#
# Everything asserted below is the agent's own behaviour: cmd/spawnery-stubop
# accepts streams, records them and never closes one, precisely so that the
# ordering assertion at the end is not measuring the stub.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
STUBOP="${STUBOP:?STUBOP must be set}"
DEADLINE="${DEADLINE:-240}"

NAME="spawnery-agent-test-$$"
VOLUME="spawnery-agent-test-$$"
WORK="$(mktemp -d)"
EVENTS="$WORK/events.jsonl"
STUB_PID=""

cleanup() {
	[ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
	"$CONTAINER" rm -f "$NAME" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

mkdir -p "$WORK/agent"

# host-gateway is understood by both Docker and Podman, so the container
# reaches the stub the same way under either runtime, and the SAN below is the
# name it dials.
"$STUBOP" \
	--dir "$WORK/agent" \
	--san stubop \
	--listen ":19443" \
	--report-interval 1 \
	--renew-after 5 \
	--hard-deadline 20 \
	>"$EVENTS" 2>"$WORK/stub.log" &
STUB_PID=$!

# The container runs as uid 10001 and reads these through a bind mount.
sleep 1
if ! kill -0 "$STUB_PID" 2>/dev/null; then
	echo "the stub operator did not stay up:" >&2
	cat "$WORK/stub.log" >&2
	exit 1
fi
chmod 0755 "$WORK/agent"
chmod 0644 "$WORK/agent/ca.crt" "$WORK/agent/token"

"$CONTAINER" volume create "$VOLUME" >/dev/null
"$CONTAINER" run -d --name "$NAME" \
	--add-host stubop:host-gateway \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME:/data" \
	-v "$WORK/agent:/var/run/spawnery:ro" \
	-e SPAWNERY_MAX_PLAYERS=100 \
	-e SPAWNERY_OPERATOR_ENDPOINT=stubop:19443 \
	"$IMAGE" >/dev/null

# A dormant agent is silent by design, and silence is also what a hung one
# looks like from here. The agent logs its reason on the way to dormancy, so
# the distinction is in the container's log rather than in the event stream -
# and getting it wrong costs an hour of looking at the wrong side.
explain_silence() {
	if "$CONTAINER" logs "$NAME" 2>&1 | grep -q 'spawnery agent dormant'; then
		echo "the agent went dormant rather than hanging - the endpoint, the CA or the token did not reach it:" >&2
		"$CONTAINER" logs "$NAME" 2>&1 | grep -i 'spawnery' >&2 || true
	fi
}

await_event() {
	local what="$1" start=$SECONDS
	until jq -e "select(.kind == \"$what\")" <"$EVENTS" >/dev/null 2>&1; do
		if [ -z "$("$CONTAINER" ps -q --filter "name=^${NAME}$")" ]; then
			echo "the container exited before sending $what" >&2
			"$CONTAINER" logs "$NAME" >&2
			cat "$WORK/stub.log" >&2
			exit 1
		fi
		if [ $((SECONDS - start)) -gt "$DEADLINE" ]; then
			echo "no $what within ${DEADLINE}s" >&2
			explain_silence
			cat "$EVENTS" >&2
			"$CONTAINER" logs "$NAME" | tail -40 >&2
			exit 1
		fi
		sleep 2
	done
}

echo "waiting up to ${DEADLINE}s for the agent to greet..."
await_event hello
echo "the agent connected"

# Reaching this line at all is the relocation proof: the agent cannot have
# greeted without SessionLoop, OperatorChannel and BearerCredentials - the only
# classes that import io.grpc - having been constructed and run inside Paper's
# classloader, which is what make image-test explicitly cannot show.

# The header the operator's interceptor matches character for character.
expected="Bearer $(cat "$WORK/agent/token")"
actual="$(jq -r 'select(.kind == "hello") | .authorization' <"$EVENTS" | head -1)"
if [ "$actual" != "$expected" ]; then
	echo "authorization header is $(printf '%q' "$actual"), want $(printf '%q' "$expected")" >&2
	exit 1
fi
echo "authorization header is exact"

await_event ready
echo "the agent reported readiness"

# The enforced maximum comes from the Bukkit main thread, which samples it on a
# timer; the operator's ReportInterval can reach the agent before that timer's
# first run, so the very first report legitimately carries the zero the counter
# was constructed with. What has to be true is that the number the agent
# reports is the server's own max-players, not that it never reports before it
# knows one - so this waits for the sampled value rather than reading the first
# line and calling a startup ordering a defect.
await_event player_count
echo "waiting for a sampled player count..."
start=$SECONDS
until [ "$(jq -rs '[.[] | select(.kind == "player_count") | .slots] | max // 0' <"$EVENTS" 2>/dev/null || echo 0)" = "100" ]; do
	if [ $((SECONDS - start)) -gt 60 ]; then
		echo "no player count carried slots = 100 from the server's own max-players within 60s" >&2
		jq -rs '[.[] | select(.kind == "player_count")]' <"$EVENTS" >&2
		exit 1
	fi
	sleep 2
done
echo "the agent reports player counts with the enforced maximum"

# The overlap. Two streams must exist, and the second must have greeted before
# the first was closed - which is exactly what a make-before-break renewal
# looks like from the operator's side, and what a break-before-make one does
# not.
echo "waiting for a renewal..."
start=$SECONDS
until [ "$(jq -rs '[.[] | select(.kind == "stream_opened")] | length' <"$EVENTS" 2>/dev/null || echo 0)" -ge 2 ]; do
	if [ $((SECONDS - start)) -gt 60 ]; then
		echo "no second stream within 60s of a 5s renewal deadline" >&2
		cat "$EVENTS" >&2
		exit 1
	fi
	sleep 2
done

# Give the handover a moment to finish. The second stream opening is what the
# loop above waited for; the first stream's close follows it, but only after a
# round trip, and an assertion made in between would see a handover that has
# not happened yet.
sleep 5

# Captured rather than piped into grep. hack/image-test.sh carries the long
# version of why: under pipefail, `jq | grep -q` can report the pipeline as
# failed because jq took a SIGPIPE, and here that would turn a real
# break-before-make verdict into a silent pass.
#
# A first stream that never closed at all is a failure here rather than a pass.
# The stub is passive - it never closes a stream - so every close in this trace
# is the agent retiring one, and an agent that has opened its replacement and
# retired nothing is leaking a stream per renewal, forever.
verdict="$(jq -rs '
	(map(select(.kind == "hello" and .stream == 1)) | first | .seq) as $second_greeted |
	(map(select(.kind == "stream_closed" and .stream == 0)) | first | .seq) as $first_closed |
	if $second_greeted == null then "the second stream never greeted"
	elif $first_closed == null then "the first stream was never retired: the agent is leaking a stream per renewal"
	elif $first_closed < $second_greeted then "the first stream closed before the second greeted: break before make"
	else empty end
' <"$EVENTS")"
if [ -n "$verdict" ]; then
	jq -rs '.' <"$EVENTS" >&2
	echo "$verdict" >&2
	exit 1
fi
echo "the renewal overlapped: the new stream greeted before the old one closed"

# A proof that only says "ok" is a proof nobody can check. This is the handover
# itself, in the order the operator saw it.
jq -rs '
	(map(select(.kind == "stream_closed" and .stream == 0)) | first | .seq) as $retired |
	map(select(.seq <= $retired and (.stream == 0 or .stream == 1)))
	| map("  seq \(.seq)  stream \(.stream)  \(.kind)")
	| .[-6:] | .[]
' <"$EVENTS"

echo "agent-test: ok"
