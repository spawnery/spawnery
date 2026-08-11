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
# It runs in three phases, against the three things an operator does to an
# agent. Against the passive stub, everything closed in the trace was closed by
# the agent, which is what makes the overlap assertion a statement about the
# agent at all. Against --supersede, the stub retires the displaced stream
# itself, where and when internal/agentserver does. Against --mute-after it
# does that and then says nothing, which is an operator blocked between the
# cancel and its first Send.
#
# The last two assert the same quantity from opposite sides: the rate at which
# the agent opens streams. Mistaking the operator's retirement for a breakage
# it owes a reconnect makes that rate too high; having no bound on a stream the
# operator accepted and never answered makes it zero. Both bounds are on both
# phases, because a phase that only fails upwards reads a dead agent as a
# healthy one.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
STUBOP="${STUBOP:?STUBOP must be set}"
DEADLINE="${DEADLINE:-240}"

NAME="spawnery-agent-test-$$"
VOLUME="spawnery-agent-test-$$"
NAME2="spawnery-agent-test-supersede-$$"
VOLUME2="spawnery-agent-test-supersede-$$"
NAME3="spawnery-agent-test-mute-$$"
VOLUME3="spawnery-agent-test-mute-$$"
WORK="$(mktemp -d)"
EVENTS="$WORK/events.jsonl"
EVENTS2="$WORK/events-supersede.jsonl"
EVENTS3="$WORK/events-mute.jsonl"
STUB_PID=""
STUB2_PID=""
STUB3_PID=""

cleanup() {
	[ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
	[ -n "$STUB2_PID" ] && kill "$STUB2_PID" 2>/dev/null || true
	[ -n "$STUB3_PID" ] && kill "$STUB3_PID" 2>/dev/null || true
	"$CONTAINER" rm -f "$NAME" "$NAME2" "$NAME3" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" "$VOLUME2" "$VOLUME3" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
# INT and TERM as well as EXIT: an untrapped SIGINT kills the shell without
# running the handler, and a run that takes several minutes gets interrupted.
# That used to leave three containers, three volumes, up to three stub processes
# and a temp directory behind, on a machine where the next run would then hit
# them.
trap cleanup EXIT INT TERM

# 0755 on the directory rather than on what the stub writes into it: the
# container runs as uid 10001 and has to traverse it, and mktemp -d gives 0700
# under any umask. The two files need nothing from here -- the stub writes them
# world-readable and chmods them itself (cmd/spawnery-stubop/main.go's `write`)
# -- and doing it before the stub starts is what keeps this off the far side of
# a liveness check that could not prove the files existed anyway.
mkdir -p "$WORK/agent"
chmod 0755 "$WORK/agent"

# The renderer refuses to start without both files, for the same reason and
# in the same shape as hack/image-test.sh's fixture: see there for why
# maxPlayers has to be a real positive number rather than an arbitrary one -
# it also has to be exactly 100 here, to match the slots assertion below.
# Built once and mounted read-only into all three phases below: nothing ever
# writes to it, and none of the three needs a value the others don't.
mkdir -p "$WORK/config"
printf 'maxPlayers: 100\n' >"$WORK/config/config.yaml"
printf 'test-forwarding-secret\n' >"$WORK/config/forwarding.secret"
chmod 0755 "$WORK/config"
chmod 0644 "$WORK/config/config.yaml" "$WORK/config/forwarding.secret"

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

sleep 1
if ! kill -0 "$STUB_PID" 2>/dev/null; then
	echo "the stub operator did not stay up:" >&2
	cat "$WORK/stub.log" >&2
	exit 1
fi

"$CONTAINER" volume create "$VOLUME" >/dev/null
"$CONTAINER" run -d --name "$NAME" \
	--add-host stubop:host-gateway \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME:/data" \
	-v "$WORK/agent:/var/run/spawnery:ro" \
	-v "$WORK/config:/etc/spawnery:ro" \
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

# streams_opened <events file> - how many streams the agent has opened so far.
# The two rate assertions below are this measurement taken at both ends of a
# window; the difference is what they bound, from above and from below.
streams_opened() {
	jq -rs '[.[] | select(.kind == "stream_opened")] | length' <"$1"
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

# The enforced maximum, and on the *first* report rather than eventually.
#
# This used to wait for max(slots) to reach 100, because the sampler is a Bukkit
# timer whose first run is the tick after onEnable returns while the operator's
# ReportInterval schedules the first report at delay zero - so the first
# PlayerCount of the process carried the zero the counter was constructed with.
# That is not a startup ordering to be waited out: internal/controller computes
# free := Slots - Players, so a server that has just gone Ready announced itself
# as having no free slots. The plugin now samples once in onEnable, before the
# session starts, and this asserts the number that crosses the seam rather than
# working around it.
await_event player_count
# The stub appends a line at a time, so a read can catch a partial one. Retried
# until it parses rather than guarded with `|| echo 0`, which would turn an
# unparseable file into a wrong number instead of a wait.
start=$SECONDS
until first_slots="$(jq -rs '[.[] | select(.kind == "player_count")] | first | .slots' <"$EVENTS" 2>/dev/null)" &&
	[ -n "$first_slots" ] && [ "$first_slots" != "null" ]; do
	if [ $((SECONDS - start)) -gt 30 ]; then
		echo "no complete player count event within 30s" >&2
		cat "$EVENTS" >&2
		exit 1
	fi
	sleep 1
done
if [ "$first_slots" != "100" ]; then
	echo "the first player count carried slots = $first_slots, want the server's own max-players of 100" >&2
	echo "the agent reported before it had sampled, so the operator saw a Ready server with no free slots" >&2
	jq -rs '[.[] | select(.kind == "player_count")]' <"$EVENTS" >&2
	exit 1
fi
echo "the first player count already carries the enforced maximum"

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
chmod 0755 "$WORK/agent-supersede"
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

"$CONTAINER" volume create "$VOLUME2" >/dev/null
"$CONTAINER" run -d --name "$NAME2" \
	--add-host stubop:host-gateway \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME2:/data" \
	-v "$WORK/agent-supersede:/var/run/spawnery:ro" \
	-v "$WORK/config:/etc/spawnery:ro" \
	-e SPAWNERY_OPERATOR_ENDPOINT=stubop:19444 \
	"$IMAGE" >/dev/null

echo "waiting up to ${DEADLINE}s for the agent to greet the superseding operator..."
await_event hello "$EVENTS2" "$NAME2"

# One renewal interval is 5s, so the window holds about six of them. Twice that
# plus two is the threshold: a correct agent is nowhere near it and the 1 Hz
# churn is several times past it, so the number below is not a timing guess.
#
# Half the renewals due is the floor, and it is not decoration. Everything this
# phase measures is bounded from above, so every way the agent can stop - a
# container that died, a wedged session, a stub that stopped renewing - produces
# a low count and would print "no reconnect storm" on the strength of it. That
# is the milestone's own assertion passing for the exact failure it exists to
# catch.
WINDOW=30
RENEWALS=$((WINDOW / 5))
LIMIT=$((RENEWALS * 2 + 2))
FLOOR=$((RENEWALS / 2))
before="$(streams_opened "$EVENTS2")"
echo "counting the streams the agent opens over ${WINDOW}s of renewals..."
sleep "$WINDOW"
after="$(streams_opened "$EVENTS2")"
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
if [ "$opened" -lt "$FLOOR" ]; then
	echo "the agent opened $opened streams in ${WINDOW}s, at least $FLOOR expected from a ${RENEWALS}-renewal window" >&2
	echo "the agent stopped renewing during the window, so the count above measured a stopped agent rather than a quiet one" >&2
	jq -rs '[.[] | select(.kind == "stream_opened" or (.kind == "stream_closed"))]' <"$EVENTS2" >&2
	# One of the things this catches is a container that is no longer there, so
	# the log read has to be allowed to fail without replacing the verdict above
	# with its own exit status.
	"$CONTAINER" logs "$NAME2" 2>&1 | tail -20 >&2 || true
	exit 1
fi
echo "the agent opened $opened streams in ${WINDOW}s across $superseded supersessions: one per renewal, no reconnect storm"

# ---------------------------------------------------------------------------
# Phase three: the operator that accepts a stream and then says nothing.
#
# The phase above proves the agent does not mistake the operator's retirement
# for a breakage. It does that by handing the reconnect obligation forward to
# the replacement - which leaves the replacement as the only thing that can ever
# discharge it. internal/agentserver cancels the displaced stream inside
# sessions.enter (server.go:179), calls Agents.Supersede (server.go:193) and
# only then sends (server.go:200); its own hard-deadline rescue is armed after
# both Sends (server.go:218). An operator that blocks in between has therefore
# retired the agent's outgoing stream, accepted its replacement, and armed
# nothing that ends either wait - and the agent's channel has no keepalive, no
# idle timeout and no call deadline of its own.
#
# So this is the same measurement as above with the bound that matters
# reversed: with no floor of its own the agent opens no further stream, ever,
# and silence is not something an upper bound can see.
echo
echo "restarting the agent against an operator that accepts a stream and says nothing..."
"$CONTAINER" rm -f "$NAME2" >/dev/null 2>&1 || true
kill "$STUB2_PID" 2>/dev/null || true
STUB2_PID=""

MUTE_HARD_DEADLINE=20
mkdir -p "$WORK/agent-mute"
chmod 0755 "$WORK/agent-mute"
"$STUBOP" \
	--dir "$WORK/agent-mute" \
	--san stubop \
	--listen ":19445" \
	--report-interval 1 \
	--renew-after 5 \
	--hard-deadline "$MUTE_HARD_DEADLINE" \
	--supersede \
	--mute-after 1 \
	>"$EVENTS3" 2>"$WORK/stub3.log" &
STUB3_PID=$!

sleep 1
if ! kill -0 "$STUB3_PID" 2>/dev/null; then
	echo "the muting stub operator did not stay up:" >&2
	cat "$WORK/stub3.log" >&2
	exit 1
fi

"$CONTAINER" volume create "$VOLUME3" >/dev/null
"$CONTAINER" run -d --name "$NAME3" \
	--add-host stubop:host-gateway \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME3:/data" \
	-v "$WORK/agent-mute:/var/run/spawnery:ro" \
	-v "$WORK/config:/etc/spawnery:ro" \
	-e SPAWNERY_OPERATOR_ENDPOINT=stubop:19445 \
	"$IMAGE" >/dev/null

echo "waiting up to ${DEADLINE}s for the agent to greet the muting operator..."
await_event hello "$EVENTS3" "$NAME3"

# Stream 0 is answered, so the agent learns the interval and both deadlines from
# it and renews on schedule. Stream 1 is the first muted one: the moment it
# opens is when the agent is holding an accepted, unanswered stream and owes
# itself everything that follows. The window starts there.
echo "waiting for the renewal the operator will not answer..."
start=$SECONDS
until [ "$(streams_opened "$EVENTS3")" -ge 2 ]; do
	if [ $((SECONDS - start)) -gt 60 ]; then
		echo "no second stream within 60s of a 5s renewal deadline" >&2
		cat "$EVENTS3" >&2
		exit 1
	fi
	sleep 1
done

# Two hard deadlines plus the backoff between them. The agent's bound on an
# unanswered stream is the operator's own hardDeadlineSeconds, so a correct
# agent gives up at 20s, reconnects a second later, gives up again at 41s and so
# on: two further streams in the window, against a floor of one. An agent
# without the bound opens none at all, and one that gave up on some invented
# short constant would run past the ceiling.
WINDOW3=45
MUTE_FLOOR=1
MUTE_LIMIT=$((WINDOW3 / MUTE_HARD_DEADLINE + 2))
before3="$(streams_opened "$EVENTS3")"
echo "counting the streams the agent opens over ${WINDOW3}s of silence..."
sleep "$WINDOW3"
after3="$(streams_opened "$EVENTS3")"
opened3=$((after3 - before3))

if [ "$opened3" -lt "$MUTE_FLOOR" ]; then
	echo "the agent opened $opened3 streams in ${WINDOW3}s of an unanswered session, at least $MUTE_FLOOR expected" >&2
	echo "an operator that accepts a stream and never answers it leaves the agent with no renewal, no reports and no reconnect: the wait has no bound of its own" >&2
	jq -rs '.' <"$EVENTS3" >&2
	"$CONTAINER" logs "$NAME3" 2>&1 | tail -20 >&2 || true
	exit 1
fi
if [ "$opened3" -gt "$MUTE_LIMIT" ]; then
	echo "the agent opened $opened3 streams in ${WINDOW3}s of an unanswered session, at most $MUTE_LIMIT expected" >&2
	echo "the bound on an unanswered stream is meant to be the operator's own ${MUTE_HARD_DEADLINE}s hard deadline, not something shorter" >&2
	jq -rs '[.[] | select(.kind == "stream_opened" or (.kind == "stream_closed"))]' <"$EVENTS3" >&2
	exit 1
fi
echo "the agent opened $opened3 streams in ${WINDOW3}s while the operator said nothing: an unanswered session is bounded by the operator's hard deadline"

echo "agent-test: ok"
