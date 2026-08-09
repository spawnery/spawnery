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
# It runs in two phases, against the two halves of what an operator does.
# Against the passive stub, everything closed in the trace was closed by the
# agent, which is what makes the overlap assertion a statement about the agent
# at all. Against --supersede, the stub retires the displaced stream itself,
# where and when internal/agentserver does - and what is asserted there is the
# rate at which the agent opens streams, because that is what an agent gets
# wrong when it mistakes the operator's retirement for a breakage of its own.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
STUBOP="${STUBOP:?STUBOP must be set}"
DEADLINE="${DEADLINE:-240}"

NAME="spawnery-agent-test-$$"
VOLUME="spawnery-agent-test-$$"
NAME2="spawnery-agent-test-supersede-$$"
VOLUME2="spawnery-agent-test-supersede-$$"
WORK="$(mktemp -d)"
EVENTS="$WORK/events.jsonl"
EVENTS2="$WORK/events-supersede.jsonl"
STUB_PID=""
STUB2_PID=""

cleanup() {
	[ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
	[ -n "$STUB2_PID" ] && kill "$STUB2_PID" 2>/dev/null || true
	"$CONTAINER" rm -f "$NAME" "$NAME2" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" "$VOLUME2" >/dev/null 2>&1 || true
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
	local name="$1"
	if "$CONTAINER" logs "$name" 2>&1 | grep -q 'spawnery agent dormant'; then
		echo "the agent went dormant rather than hanging - the endpoint, the CA or the token did not reach it:" >&2
		"$CONTAINER" logs "$name" 2>&1 | grep -i 'spawnery' >&2 || true
	fi
}

# await_event <kind> [events file] [container] - the second phase runs its own
# container against its own stub, so neither is assumed here.
await_event() {
	local what="$1" events="${2:-$EVENTS}" name="${3:-$NAME}" start=$SECONDS
	until jq -e "select(.kind == \"$what\")" <"$events" >/dev/null 2>&1; do
		if [ -z "$("$CONTAINER" ps -q --filter "name=^${name}$")" ]; then
			echo "the container exited before sending $what" >&2
			"$CONTAINER" logs "$name" >&2
			cat "$WORK"/stub*.log >&2 || true
			exit 1
		fi
		if [ $((SECONDS - start)) -gt "$DEADLINE" ]; then
			echo "no $what within ${DEADLINE}s" >&2
			explain_silence "$name"
			cat "$events" >&2
			"$CONTAINER" logs "$name" | tail -40 >&2
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

# The second stream opening is what the loop above waited for; the first
# stream's close follows it, but only after a round trip. That wait has to be
# bounded by a deadline rather than by a fixed sleep: the verdict below accuses
# the agent of leaking a stream per renewal, and a handover that is correct but
# slower than the guess would earn that accusation for a loaded CI box. Only
# once RETIRED_WITHIN has actually elapsed is "never retired" a true statement.
RETIRED_WITHIN=30
start=$SECONDS
until [ "$(jq -rs '[.[] | select(.kind == "stream_closed" and .stream == 0)] | length' <"$EVENTS" 2>/dev/null || echo 0)" -ge 1 ]; do
	if [ $((SECONDS - start)) -gt "$RETIRED_WITHIN" ]; then
		break
	fi
	sleep 1
done

# Captured rather than piped into grep. hack/image-test.sh carries the long
# version of why: under pipefail, `jq | grep -q` can report the pipeline as
# failed because jq took a SIGPIPE, and here that would turn a real
# break-before-make verdict into a silent pass.
#
# A first stream that never closed at all is a failure here rather than a pass.
# The stub is passive - it never closes a stream - so every close in this trace
# is the agent retiring one, and an agent that has opened its replacement and
# retired nothing is leaking a stream per renewal, forever.
verdict="$(jq -rs --argjson within "$RETIRED_WITHIN" '
	(map(select(.kind == "hello" and .stream == 1)) | first | .seq) as $second_greeted |
	(map(select(.kind == "stream_closed" and .stream == 0)) | first | .seq) as $first_closed |
	if $second_greeted == null then "the second stream never greeted"
	elif $first_closed == null then "the first stream was not retired within \($within)s of the replacement opening: the agent is leaking a stream per renewal"
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

# ---------------------------------------------------------------------------
# Phase two: the operator's own retirement order.
#
# The stub above never cancels a stream, which is what the overlap assertion
# needs and is also the one thing the real operator does that the assertion can
# therefore not see. internal/agentserver cancels the displaced stream's
# context inside sessions.enter(), at the handler entry of the replacement -
# before Supersede and before either Send - and the cancelled handler answers
# Unavailable. So the agent always learns that the outgoing stream failed while
# its replacement is still unanswered, and an agent that reads that as a
# breakage it owes a reconnect books one on every renewal. The extra stream
# supersedes the replacement a second later (the replacement's first message
# having reset the backoff to its floor) and the sequence repeats: roughly one
# stream a second, per server, for as long as the fleet runs.
#
# Nothing about the order is wrong in that failure, so this phase asserts on the
# rate instead: over a window, about one stream per renewal interval and not one
# per second.
echo
echo "restarting the agent against a superseding operator..."
"$CONTAINER" rm -f "$NAME" >/dev/null 2>&1 || true
kill "$STUB_PID" 2>/dev/null || true
STUB_PID=""

mkdir -p "$WORK/agent-supersede"
"$STUBOP" \
	--dir "$WORK/agent-supersede" \
	--san stubop \
	--listen ":19444" \
	--report-interval 1 \
	--renew-after 5 \
	--hard-deadline 20 \
	--supersede \
	>"$EVENTS2" 2>"$WORK/stub2.log" &
STUB2_PID=$!

sleep 1
if ! kill -0 "$STUB2_PID" 2>/dev/null; then
	echo "the superseding stub operator did not stay up:" >&2
	cat "$WORK/stub2.log" >&2
	exit 1
fi
chmod 0755 "$WORK/agent-supersede"
chmod 0644 "$WORK/agent-supersede/ca.crt" "$WORK/agent-supersede/token"

"$CONTAINER" volume create "$VOLUME2" >/dev/null
"$CONTAINER" run -d --name "$NAME2" \
	--add-host stubop:host-gateway \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME2:/data" \
	-v "$WORK/agent-supersede:/var/run/spawnery:ro" \
	-e SPAWNERY_MAX_PLAYERS=100 \
	-e SPAWNERY_OPERATOR_ENDPOINT=stubop:19444 \
	"$IMAGE" >/dev/null

echo "waiting up to ${DEADLINE}s for the agent to greet the superseding operator..."
await_event hello "$EVENTS2" "$NAME2"

# One renewal interval is 5s, so the window holds about six of them. Twice that
# plus two is the threshold: a correct agent is nowhere near it and the 1 Hz
# churn is several times past it, so the number below is not a timing guess.
WINDOW=30
RENEWALS=$((WINDOW / 5))
LIMIT=$((RENEWALS * 2 + 2))
before="$(jq -rs '[.[] | select(.kind == "stream_opened")] | length' <"$EVENTS2")"
echo "counting the streams the agent opens over ${WINDOW}s of renewals..."
sleep "$WINDOW"
after="$(jq -rs '[.[] | select(.kind == "stream_opened")] | length' <"$EVENTS2")"
opened=$((after - before))

# Without this the phase would pass for the wrong reason: a stub that never
# actually superseded would produce a perfectly low stream count.
superseded="$(jq -rs '[.[] | select(.kind == "stream_closed" and .error == "superseded")] | length' <"$EVENTS2")"
if [ "$superseded" -lt 1 ]; then
	echo "the stub never retired a displaced stream, so this phase measured nothing" >&2
	jq -rs '.' <"$EVENTS2" >&2
	exit 1
fi

if [ "$opened" -gt "$LIMIT" ]; then
	echo "the agent opened $opened streams in ${WINDOW}s, at most $LIMIT expected from a ${RENEWALS}-renewal window" >&2
	echo "the operator retiring the displaced stream is being mistaken for a breakage the agent owes a reconnect" >&2
	jq -rs '[.[] | select(.kind == "stream_opened" or (.kind == "stream_closed"))]' <"$EVENTS2" >&2
	exit 1
fi
echo "the agent opened $opened streams in ${WINDOW}s across $superseded supersessions: one per renewal, no reconnect storm"

echo "agent-test: ok"
