#!/usr/bin/env bash
# The agents, proven against a real operator-shaped counterpart.
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
# The first three phases run against the Paper image, one for each of the three
# things an operator does to an agent. Against the passive stub, everything
# closed in the trace was closed by the agent, which is what makes the overlap
# assertion a statement about the agent at all. Against --supersede, the stub
# retires the displaced stream itself, where and when internal/agentserver
# does. Against --mute-after it does that and then says nothing, which is an
# operator blocked between the cancel and its first Send.
#
# The last two assert the same quantity from opposite sides: the rate at which
# the agent opens streams. Mistaking the operator's retirement for a breakage
# it owes a reconnect makes that rate too high; having no bound on a stream the
# operator accepted and never answered makes it zero. Both bounds are on both
# phases, because a phase that only fails upwards reads a dead agent as a
# healthy one.
#
# Phases four and five run against the Velocity image, and add the one
# obligation only a proxy has: the pod's readiness gate opens on the first
# FullSync and not before, and closes again when the operator says so. Those
# three probes - 8081 closed while the agent is connected and has deliberately
# not been given a server list, open once one has arrived, closed again on a
# SetReady - are the whole reason this script grew a proxy half. A unit test can
# see that bind() was called; only a container can see a port.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
VELOCITY_IMAGE="${VELOCITY_IMAGE:?VELOCITY_IMAGE must be set}"
STUBOP="${STUBOP:?STUBOP must be set}"
DEADLINE="${DEADLINE:-240}"

NAME="spawnery-agent-test-$$"
VOLUME="spawnery-agent-test-$$"
NAME2="spawnery-agent-test-supersede-$$"
VOLUME2="spawnery-agent-test-supersede-$$"
NAME3="spawnery-agent-test-mute-$$"
VOLUME3="spawnery-agent-test-mute-$$"
NAME4="spawnery-agent-test-proxy-$$"
VOLUME4="spawnery-agent-test-proxy-$$"
NAME5="spawnery-agent-test-proxy-supersede-$$"
VOLUME5="spawnery-agent-test-proxy-supersede-$$"
WORK="$(mktemp -d)"
EVENTS="$WORK/events.jsonl"
EVENTS2="$WORK/events-supersede.jsonl"
EVENTS3="$WORK/events-mute.jsonl"
EVENTS4="$WORK/events-proxy.jsonl"
EVENTS5="$WORK/events-proxy-supersede.jsonl"
STUB_PID=""
STUB2_PID=""
STUB3_PID=""
STUB4_PID=""
STUB5_PID=""

cleanup() {
	[ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
	[ -n "$STUB2_PID" ] && kill "$STUB2_PID" 2>/dev/null || true
	[ -n "$STUB3_PID" ] && kill "$STUB3_PID" 2>/dev/null || true
	[ -n "$STUB4_PID" ] && kill "$STUB4_PID" 2>/dev/null || true
	[ -n "$STUB5_PID" ] && kill "$STUB5_PID" 2>/dev/null || true
	"$CONTAINER" rm -f "$NAME" "$NAME2" "$NAME3" "$NAME4" "$NAME5" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" "$VOLUME2" "$VOLUME3" "$VOLUME4" "$VOLUME5" \
		>/dev/null 2>&1 || true
	rm -rf "$WORK"
}
# INT and TERM as well as EXIT: an untrapped SIGINT kills the shell without
# running the handler, and a run that takes several minutes gets interrupted.
# That used to leave every container, volume and stub process this script has
# started, plus a temp directory, behind on a machine where the next run would
# then hit them.
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

# count_events <kind> <events file> - how many events of one kind the trace
# holds. No `|| echo 0` fallback: the stub appends whole lines and fsyncs each
# one, so a parse failure here is a real one, and answering 0 for it would turn
# an unreadable trace into a wrong number rather than a loud stop.
#
# The `case` is what makes that true rather than intended. jq answering nothing
# is the shape a failed read takes here, and `[ "" -ne 0 ]` does not fail an
# assertion: it prints "integer expression expected", returns 2, and every `if`
# built on it reads that as false. A guard written that way passes exactly when
# the trace could not be read. Refusing to return a non-number is what stops
# that, and every caller assigns the result first so `set -e` sees this exit
# rather than a comparison swallowing it.
count_events() {
	local count
	count="$(jq -rs --arg kind "$1" '[.[] | select(.kind == $kind)] | length' <"$2")"
	case "$count" in
	'' | *[!0-9]*)
		echo "could not count $1 events in $2: jq answered '$count'" >&2
		exit 1
		;;
	esac
	printf '%s\n' "$count"
}

# streams_opened <events file> - how many streams the agent has opened so far.
# The two rate assertions below are this measurement taken at both ends of a
# window; the difference is what they bound, from above and from below.
streams_opened() {
	count_events stream_opened "$1"
}

# port_open <container> <port> - whether something inside the container accepts
# a connection on that port.
#
# From inside, never through a published port. A rootless podman port forwarder
# accepts on the host and only then dials the container, so it can complete the
# host-side handshake with nothing listening inside - which would make the
# closed-gate assertion below pass for a reason that has nothing to do with the
# agent. /dev/tcp is a bash builtin rather than a tool added to the image for
# this probe's convenience; bash already ships because the entrypoint's shebang
# points at it.
port_open() {
	"$CONTAINER" exec "$1" bash -c "exec 3<>/dev/tcp/127.0.0.1/$2" >/dev/null 2>&1
}

echo "waiting up to ${DEADLINE}s for the agent to greet..."
await_event hello
echo "the agent connected"

# Reaching this line at all is the relocation proof: the agent cannot have
# greeted without SessionLoop, OperatorChannel and BearerCredentials - the only
# classes that import io.grpc - having been constructed and run inside Paper's
# classloader, which is what make image-test explicitly cannot show.

# The header the operator's interceptor matches character for character.
#
# The same guard phase 4 carries, and it matters more here: this phase's stub
# runs without --require-token, so nothing refuses an uncredentialed stream and
# this string comparison is the phase's only credential check. With an empty
# token file both sides would read "Bearer " and it would pass for an agent
# that sent no token at all.
token1="$(cat "$WORK/agent/token")"
if [ -z "$token1" ]; then
	echo "the stub wrote an empty token file, so the header comparison below would prove nothing" >&2
	exit 1
fi
expected="Bearer $token1"
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

# ---------------------------------------------------------------------------
# Phase four: the proxy, and the gate that must not open early - or stay open.
#
# internal/podspec gives the proxy container a tcpSocket readiness probe on 8081
# and nothing else, so "this pod is ready" means exactly "the agent bound that
# port". The agent binds it on the first FullSync and never before, because a
# proxy that turned ready without a server list would take traffic it can only
# answer with "no available server".
#
# Both halves of that have to be measured, and only here can either be: the
# port is closed while the agent is connected and unsynced, and open once a
# server list has arrived. The closed half is what makes the open half mean
# anything, so the stub holds its FullSync back rather than sending it with the
# opening messages, and the guards below fail the phase if it went out before
# the probe ran anyway.
#
# Then the third probe, and the one milestone 4c-1 exists for. Opening is a
# decision the agent reaches on its own from a server list; closing is one the
# operator makes and the agent obeys, over the wire, as a SetReady{ready:false}.
# Every test written for that so far has faked one side of that wire: the Go
# tests send the message to a recorder, the JUnit tests hand a hand-built
# message to a ProxyRole holding a lambda. Nothing had ever put the real message
# in front of the real jar, and the port it is supposed to release is not a
# thing either level can see. So the stub sends it, and this phase reads 8081.
echo
echo "starting the proxy against an operator that holds its server list back..."
"$CONTAINER" rm -f "$NAME3" >/dev/null 2>&1 || true
kill "$STUB3_PID" 2>/dev/null || true
STUB3_PID=""

# The renderer refuses to start without both files, the same shape
# hack/velocity-image-test.sh builds -- a proxy's own config, which is not the
# file the Paper phases mount. playerLimit is the proxy's capacity and
# SPAWNERY_PLAYER_LIMIT below is what the agent reports as slots; in a cluster
# internal/podspec derives both from the one ProxyGroup field, so they are one
# number here too rather than two that happen to agree.
PROXY_LIMIT=100
mkdir -p "$WORK/velocity-config"
#
# onlineMode is written explicitly because render.Velocity refuses to guess it.
# true here: no phase of this script joins the proxy as a player, so an
# authenticating proxy costs nothing. The phase that does join needs it off,
# because a Go client cannot authenticate against Microsoft.
printf 'playerLimit: %s\nonlineMode: true\n' "$PROXY_LIMIT" >"$WORK/velocity-config/config.yaml"
printf 'test-forwarding-secret\n' >"$WORK/velocity-config/forwarding.secret"
chmod 0755 "$WORK/velocity-config"
chmod 0644 "$WORK/velocity-config/config.yaml" "$WORK/velocity-config/forwarding.secret"

# How long the stub holds the FullSync back, counted from the stream opening.
# Everything between the agent's Hello and the sync has to fit inside it: the
# poll that notices the greeting, the control probe below, and the closed-gate
# probe itself. Generous rather than tight, because the cost of overrunning it
# is a failed run and the cost of a long hold is 45 seconds.
FULL_SYNC_AFTER=45

# And how long it then holds the SetReady back, counted from that FullSync.
#
# What has to fit inside it is the poll that notices the sync and the open-gate
# probe, and nothing after them: a withdrawal that lands before the gate has
# been seen open is a run that measured this script's flags rather than the
# agent, and the two arms of the guard below say so by name. The player count
# assertion that follows may overrun it freely - the only difference is that
# closed_after reads 0. Generous for the same reason FULL_SYNC_AFTER is.
SET_READY_AFTER=20s

# renew-after is 180 rather than the 5 the phases above use, so exactly one
# stream exists for the whole of this phase. Under a 5s renewal each new stream
# would start a schedule of its own and the trace would carry a full_sync_sent
# and a set_ready_sent per stream; nothing about the gate would change, but the
# three events this phase orders its probes against would stop being one
# sequence. It has to outlast the SetReady as well as the sync now: a renewal
# landing between them would retire the stream the withdrawal was scheduled on,
# and the stub would record set_ready_failed on a stream that no longer exists.
mkdir -p "$WORK/agent-proxy"
chmod 0755 "$WORK/agent-proxy"
"$STUBOP" \
	--dir "$WORK/agent-proxy" \
	--san stubop \
	--listen ":19446" \
	--report-interval 1 \
	--renew-after 180 \
	--hard-deadline 240 \
	--proxy \
	--require-token \
	--full-sync-after "$FULL_SYNC_AFTER" \
	--set-ready-after "$SET_READY_AFTER" \
	>"$EVENTS4" 2>"$WORK/stub4.log" &
STUB4_PID=$!

sleep 1
if ! kill -0 "$STUB4_PID" 2>/dev/null; then
	echo "the proxy stub operator did not stay up:" >&2
	cat "$WORK/stub4.log" >&2
	exit 1
fi

# The two proxy-only variables are not decoration: without either one the agent
# goes dormant and never connects at all, naming the variable it did not get.
"$CONTAINER" volume create "$VOLUME4" >/dev/null
"$CONTAINER" run -d --name "$NAME4" \
	--add-host stubop:host-gateway \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME4:/data" \
	-v "$WORK/agent-proxy:/var/run/spawnery:ro" \
	-v "$WORK/velocity-config:/etc/spawnery:ro" \
	-e SPAWNERY_OPERATOR_ENDPOINT=stubop:19446 \
	-e SPAWNERY_PLAYER_LIMIT="$PROXY_LIMIT" \
	-e SPAWNERY_FALLBACK_GROUPS=lobby \
	"$VELOCITY_IMAGE" >/dev/null

echo "waiting up to ${DEADLINE}s for the proxy agent to greet..."
await_event hello "$EVENTS4" "$NAME4"
echo "the proxy agent connected"

# The same character-for-character check the Paper phase makes. ProxyRole.open()
# attaches the call credentials, and deleting that call compiles and passes
# every JUnit test, so this level is the only cover it has.
#
# The stub is started with --require-token above, so the greeting this phase
# already waited for is itself the first half of that cover: an uncredentialed
# stream is refused before it opens and the agent never greets at all. This
# check is the second half, and the two are not redundant -- the refusal proves
# some acceptable token arrived, this proves it was the mounted one, byte for
# byte, in the spelling internal/grpcauth's interceptor matches.
token4="$(cat "$WORK/agent-proxy/token")"
if [ -z "$token4" ]; then
	# Otherwise both sides of the comparison below are "Bearer " and it passes
	# for an agent that sent no token at all - the assertion this whole phase
	# exists to keep honest, defeated by an empty file.
	echo "the stub wrote an empty token file, so the header comparison below would prove nothing" >&2
	exit 1
fi
expected4="Bearer $token4"
actual4="$(jq -r 'select(.kind == "hello") | .authorization' <"$EVENTS4" | head -1)"
if [ "$actual4" != "$expected4" ]; then
	echo "proxy authorization header is $(printf '%q' "$actual4"), want $(printf '%q' "$expected4")" >&2
	echo "a proxy stream that presents no credentials is accepted by this stub and refused by the operator" >&2
	exit 1
fi
echo "the proxy's authorization header is exact"

# A control probe, and the reason the closed-gate assertion below is an
# assertion rather than a decoration. `port_open` answers false for every way an
# exec can fail - a container that is gone, a bash that is not there, a runtime
# that refuses - and all of those would read as "the gate is closed". 25565 is
# the port the proxy binds on its own, with no help from the agent, so a
# connection accepted there proves the probe mechanism works inside this
# container at this moment.
echo "waiting for the proxy's own listener, to prove the probe works at all..."
start=$SECONDS
until port_open "$NAME4" 25565; do
	if [ -z "$("$CONTAINER" ps -q --filter "name=^${NAME4}$")" ]; then
		echo "the proxy container exited before binding 25565:" >&2
		"$CONTAINER" logs "$NAME4" >&2
		exit 1
	fi
	if [ $((SECONDS - start)) -gt 30 ]; then
		echo "the proxy did not accept a connection on 25565 within 30s, so the gate probe below would prove nothing" >&2
		"$CONTAINER" logs "$NAME4" | tail -40 >&2
		exit 1
	fi
	sleep 1
done
echo "25565 answers from inside the container"

# Nothing has been synced yet, checked before and after the probe. Either check
# failing means the hold above expired while this phase was still getting ready,
# which makes the result below meaningless rather than merely late - and a
# closed-gate assertion that is allowed to run after the sync is the exact shape
# of assertion this milestone has twice found unable to fail.
synced4="$(count_events full_sync_sent "$EVENTS4")"
if [ "$synced4" -ne 0 ]; then
	echo "the FullSync went out before the gate could be probed closed; raise FULL_SYNC_AFTER" >&2
	jq -rs '.' <"$EVENTS4" >&2
	exit 1
fi
if port_open "$NAME4" 8081; then
	echo "the ready gate is open on 8081 before any server list arrived" >&2
	echo "a proxy that turns ready without one takes traffic it can only answer with 'no available server'" >&2
	"$CONTAINER" logs "$NAME4" 2>&1 | grep -i spawnery >&2 || true
	exit 1
fi
synced4="$(count_events full_sync_sent "$EVENTS4")"
if [ "$synced4" -ne 0 ]; then
	echo "the FullSync went out while the gate was being probed closed; raise FULL_SYNC_AFTER" >&2
	jq -rs '.' <"$EVENTS4" >&2
	exit 1
fi
echo "the ready gate is closed while the agent is connected and unsynced"

echo "waiting for the operator to release the server list..."
await_event full_sync_sent "$EVENTS4" "$NAME4"

# Retried rather than probed once: the gate binds from a gRPC callback thread,
# and the stub records full_sync_sent the moment its own Send returned, which is
# before the bytes have crossed the loopback and been applied.
GATE_WITHIN=30
start=$SECONDS
until port_open "$NAME4" 8081; do
	if [ $((SECONDS - start)) -gt "$GATE_WITHIN" ]; then
		# Which failure this is depends on whether the withdrawal has already
		# gone out, and the two have nothing to do with each other. A gate that
		# never opened because SET_READY_AFTER was too small for this phase to
		# get here in time is a harness that measured its own flags; the message
		# below would blame the agent for it, and blame is what a person reads
		# first. Assigned rather than tested inline, for the reason count_events
		# gives about itself: a failed read has to reach `set -e` as an exit
		# rather than as a comparison that reads false.
		withdrawn4="$(count_events set_ready_sent "$EVENTS4")"
		if [ "$withdrawn4" -ne 0 ]; then
			echo "the SetReady went out before the gate ever opened; raise SET_READY_AFTER" >&2
			echo "this proxy was told to stop being ready before it had started, so the gate below is shut for the harness's reasons rather than the agent's" >&2
			jq -rs '.' <"$EVENTS4" >&2
			exit 1
		fi
		echo "8081 was still closed ${GATE_WITHIN}s after the operator sent a server list" >&2
		echo "this pod would never turn ready, and the proxy would never receive a player" >&2
		jq -rs '.' <"$EVENTS4" >&2
		"$CONTAINER" logs "$NAME4" 2>&1 | grep -i spawnery >&2 || true
		exit 1
	fi
	sleep 1
done
echo "the ready gate opened $((SECONDS - start))s after the first FullSync"

# And it opened on the sync, not in spite of a withdrawal that had already gone
# out. This is the second arm of the same guard, and it covers the case the one
# inside the timeout cannot reach: the port answered, so that loop never
# returned to its deadline, but the withdrawal had already been sent by the time
# it did. What that produces is worse than a failure - it is a pass. The gate
# would have opened and closed inside this phase's own probe window, the
# closed-gate assertion below would then succeed against a gate that was already
# shut, and the phase would report "open, then closed on the operator's word"
# having established neither. Both arms name SET_READY_AFTER because raising it
# is the answer to either.
withdrawn4="$(count_events set_ready_sent "$EVENTS4")"
if [ "$withdrawn4" -ne 0 ]; then
	echo "the SetReady went out before the gate could be probed open; raise SET_READY_AFTER" >&2
	echo "the close below would then be asserted against a gate that had already shut, and this phase would pass having measured nothing" >&2
	jq -rs '.' <"$EVENTS4" >&2
	exit 1
fi

# The proxy's own capacity, on the wire. internal/agent/registry.go discards any
# report where players exceed slots, so a proxy that reported zero slots would
# have every count with a player online silently thrown away - and this is the
# only level where the number that crosses the seam comes from a real
# SPAWNERY_PLAYER_LIMIT rather than from a test's constructor argument.
await_event player_count "$EVENTS4" "$NAME4"
start=$SECONDS
until proxy_slots="$(jq -rs '[.[] | select(.kind == "player_count")] | first | .slots' <"$EVENTS4" 2>/dev/null)" &&
	[ -n "$proxy_slots" ] && [ "$proxy_slots" != "null" ]; do
	if [ $((SECONDS - start)) -gt 30 ]; then
		echo "no complete player count event within 30s" >&2
		cat "$EVENTS4" >&2
		exit 1
	fi
	sleep 1
done
if [ "$proxy_slots" != "$PROXY_LIMIT" ]; then
	echo "the proxy reported slots = $proxy_slots, want the $PROXY_LIMIT of SPAWNERY_PLAYER_LIMIT" >&2
	echo "the registry discards any report where players exceed slots, so this proxy's counts would be dropped" >&2
	jq -rs '[.[] | select(.kind == "player_count")]' <"$EVENTS4" >&2
	exit 1
fi
echo "the proxy reports its configured player limit as slots"

# The gate closes on the operator's word, which is the contract milestone 4c-1
# hangs the whole drain on: the operator tells a surplus proxy to stop being
# ready, the kubelet's probe fails, the Service drops the endpoint, and no new
# player is sent to a proxy that is on its way out.
echo "waiting for the operator to withdraw the proxy's readiness..."
await_event set_ready_sent "$EVENTS4" "$NAME4"

# Retried rather than probed once, for the same reason the open probe is: the
# stub records set_ready_sent the moment its own Send returned, which is before
# the bytes have crossed the loopback, reached a gRPC callback thread and closed
# a socket.
CLOSED_WITHIN=30
start=$SECONDS
while port_open "$NAME4" 8081; do
	if [ $((SECONDS - start)) -gt "$CLOSED_WITHIN" ]; then
		echo "8081 was still open ${CLOSED_WITHIN}s after the operator withdrew this proxy's readiness" >&2
		echo "this pod would stay in the Service's endpoints, and a proxy the operator is trying to drain would go on being handed new players" >&2
		jq -rs '.' <"$EVENTS4" >&2
		"$CONTAINER" logs "$NAME4" 2>&1 | grep -i spawnery >&2 || true
		exit 1
	fi
	sleep 1
done
closed_after=$((SECONDS - start))

# The control probe again, and it carries more here than the one before the
# open probe did. `port_open` answers false for every way an exec can fail, so
# without this the loop above would have reported success for a proxy that had
# died - the assertion this half of the phase exists for, passing for the
# failure it exists to catch.
#
# And 25565 is not merely the control this time, it is half the claim. The
# contract is that a withdrawn proxy stops being *ready*, not that it stops
# serving: the whole point of closing 8081 rather than shutting down is that
# established sessions survive while the endpoint drains. A proxy whose own
# listener went with the gate would have dropped every player still on it.
if ! port_open "$NAME4" 25565; then
	echo "the proxy's own listener on 25565 went down with the ready gate" >&2
	echo "withdrawing readiness must take this pod out of the Service's endpoints, not out of service: the players already on it have to keep playing" >&2
	"$CONTAINER" logs "$NAME4" 2>&1 | tail -40 >&2
	exit 1
fi
echo "the ready gate closed ${closed_after}s after the operator withdrew readiness, and 25565 still answers"

# ---------------------------------------------------------------------------
# Phase five: the operator's retirement order, against the proxy.
#
# The same measurement as phase two, on the other agent. The renewal loop is
# shared code from task 3 and the Paper phase already drives it, which is the
# argument for not repeating this - and it is the same argument that would have
# excused every one of milestone 2c's five defects, all of which lived in an
# assumption about the harness rather than in the loop. The proxy dials a
# different rpc, on a different classloader, holding a server list it reapplies
# on every reconnect; none of that is what phase two measured.
echo
echo "restarting the proxy against a superseding operator..."
"$CONTAINER" rm -f "$NAME4" >/dev/null 2>&1 || true
kill "$STUB4_PID" 2>/dev/null || true
STUB4_PID=""

# --proxy with no hold, so every stream is a complete proxy session: opened,
# answered, synced. A supersession that retired a stream the proxy had never
# been synced on would be a shape the operator does not produce.
mkdir -p "$WORK/agent-proxy-supersede"
chmod 0755 "$WORK/agent-proxy-supersede"
"$STUBOP" \
	--dir "$WORK/agent-proxy-supersede" \
	--san stubop \
	--listen ":19447" \
	--report-interval 1 \
	--renew-after 5 \
	--hard-deadline 20 \
	--supersede \
	--proxy \
	--require-token \
	>"$EVENTS5" 2>"$WORK/stub5.log" &
STUB5_PID=$!

sleep 1
if ! kill -0 "$STUB5_PID" 2>/dev/null; then
	echo "the superseding proxy stub operator did not stay up:" >&2
	cat "$WORK/stub5.log" >&2
	exit 1
fi

"$CONTAINER" volume create "$VOLUME5" >/dev/null
"$CONTAINER" run -d --name "$NAME5" \
	--add-host stubop:host-gateway \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME5:/data" \
	-v "$WORK/agent-proxy-supersede:/var/run/spawnery:ro" \
	-v "$WORK/velocity-config:/etc/spawnery:ro" \
	-e SPAWNERY_OPERATOR_ENDPOINT=stubop:19447 \
	-e SPAWNERY_PLAYER_LIMIT="$PROXY_LIMIT" \
	-e SPAWNERY_FALLBACK_GROUPS=lobby \
	"$VELOCITY_IMAGE" >/dev/null

echo "waiting up to ${DEADLINE}s for the proxy agent to greet the superseding operator..."
await_event hello "$EVENTS5" "$NAME5"

# The same two-sided bound, for the same reasons, as phase two: above it is a
# reconnect storm, below it is an agent that stopped renewing and would have
# printed the upper bound's success message on the strength of having died.
before5="$(streams_opened "$EVENTS5")"
echo "counting the streams the proxy opens over ${WINDOW}s of renewals..."
sleep "$WINDOW"
after5="$(streams_opened "$EVENTS5")"
opened5=$((after5 - before5))

superseded5="$(jq -rs '[.[] | select(.kind == "stream_closed" and .error == "superseded")] | length' <"$EVENTS5")"
if [ "$superseded5" -lt 1 ]; then
	echo "the stub never retired a displaced proxy stream, so this phase measured nothing" >&2
	jq -rs '.' <"$EVENTS5" >&2
	exit 1
fi

# And the same for --proxy, for exactly the reason above. The comment on this
# phase's stub claims every stream is a complete proxy session - opened,
# answered, synced - and without this the flag could be deleted and the phase
# would pass identically, because everything else here reads only stream_opened
# and stream_closed.
synced5="$(count_events full_sync_sent "$EVENTS5")"
if [ "$synced5" -lt 1 ]; then
	echo "no stream in this phase was ever sent a server list, so --proxy measured nothing" >&2
	jq -rs '.' <"$EVENTS5" >&2
	exit 1
fi

if [ "$opened5" -gt "$LIMIT" ]; then
	echo "the proxy opened $opened5 streams in ${WINDOW}s, at most $LIMIT expected from a ${RENEWALS}-renewal window" >&2
	echo "the operator retiring the displaced stream is being mistaken for a breakage the agent owes a reconnect" >&2
	jq -rs '[.[] | select(.kind == "stream_opened" or (.kind == "stream_closed"))]' <"$EVENTS5" >&2
	exit 1
fi
if [ "$opened5" -lt "$FLOOR" ]; then
	echo "the proxy opened $opened5 streams in ${WINDOW}s, at least $FLOOR expected from a ${RENEWALS}-renewal window" >&2
	echo "the agent stopped renewing during the window, so the count above measured a stopped agent rather than a quiet one" >&2
	jq -rs '[.[] | select(.kind == "stream_opened" or (.kind == "stream_closed"))]' <"$EVENTS5" >&2
	"$CONTAINER" logs "$NAME5" 2>&1 | tail -20 >&2 || true
	exit 1
fi
echo "the proxy opened $opened5 streams in ${WINDOW}s across $superseded5 supersessions: one per renewal, no reconnect storm"

# The gate is still open, after $superseded5 retirements and $opened5 handovers.
#
# Phase four proves the gate closes when the operator says so; what no level can
# see is whether anything *else* closes it. ReadyGate.close() is reachable from
# a SetReady and from onShutdown, and this phase's stub sends no SetReady and is
# killed rather than asked to shut the proxy down - so across every retirement
# and handover above there is nothing the gate may legitimately react to, and
# any movement in it is the renewal machinery touching a port it does not own.
# That is the failure this whole design exists to prevent: a proxy that dropped
# out of Ready on every renewal would flap its pod's readiness on the operator's
# own schedule, taking itself out of the Service endpoints while nothing was
# wrong with it.
if ! port_open "$NAME5" 8081; then
	echo "the ready gate is closed after ${WINDOW}s of supersessions; a renewal took this pod out of Ready" >&2
	jq -rs '[.[] | select(.kind == "stream_opened" or .kind == "stream_closed" or .kind == "full_sync_sent")]' <"$EVENTS5" >&2
	"$CONTAINER" logs "$NAME5" 2>&1 | grep -i spawnery >&2 || true
	exit 1
fi
echo "the ready gate stayed open across $synced5 syncs and $superseded5 supersessions"

echo "agent-test: ok"
