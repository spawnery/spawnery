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
#
# Phase six goes back to the Paper image for one more obligation neither
# language's unit tests can reach on their own: that the agent's trust survives
# a CA rotation's overlap. OperatorChannel.trustManager parses every
# certificate a mounted bundle holds, not only the first, and its own doc
# comment says that a single-PEM parse "would make the agent the one thing
# that cannot survive such a rotation". That is a claim about a real JVM's TLS
# stack meeting a real ServerHello, so a Kotlin unit test can build the same
# X509TrustManager and call it satisfied without ever proving it against a
# handshake, and a Go test can build the same two-CA bundle with
# internal/certs and never hand it to a JVM at all. So the stub, told
# --rotate-ca, builds that bundle with internal/certs itself - the same
# IssueCA and SwitchToNext the operator's own rotation calls - and serves a
# certificate signed by the second entry. Success here is the agent greeting
# that server; failure is the handshake error a single-PEM trustManager would
# have produced instead.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
VELOCITY_IMAGE="${VELOCITY_IMAGE:?VELOCITY_IMAGE must be set}"
STUBOP="${STUBOP:?STUBOP must be set}"
DEADLINE="${DEADLINE:-240}"

# The renewal interval every phase but the proxy-sync one configures its stub
# with, and the window the two stream-rate phases count over.
#
# Declared here rather than beside the phase that first used them, which is
# what docs/known-issues.md recorded: phase 5 read WINDOW, RENEWALS, LIMIT and
# FLOOR out of phase 2, five hundred lines above it, and RENEWALS divided by a
# literal 5 that had to match a --renew-after somewhere else again. Changing
# the interval meant finding three unrelated places and hoping.
#
# The bounds are the same for both phases because they measure the same
# quantity from opposite sides -- the rate at which an agent opens streams --
# and a phase that wanted its own would say so by declaring its own, the way
# phase 3 does with MUTE_LIMIT.
RENEW_AFTER=5
WINDOW=30
RENEWALS=$((WINDOW / RENEW_AFTER))
# At most two streams per renewal plus slack: one per renewal is correct, and
# twice that still rules out a reconnect storm.
LIMIT=$((RENEWALS * 2 + 2))
# And at least half, so a dead agent cannot pass a bound that only fails
# upwards.
FLOOR=$((RENEWALS / 2))

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
NAME6="spawnery-agent-test-rotate-$$"
VOLUME6="spawnery-agent-test-rotate-$$"
NAME7="spawnery-agent-test-deaf-$$"
VOLUME7="spawnery-agent-test-deaf-$$"
WORK="$(mktemp -d)"
EVENTS="$WORK/events.jsonl"
EVENTS2="$WORK/events-supersede.jsonl"
EVENTS3="$WORK/events-mute.jsonl"
EVENTS4="$WORK/events-proxy.jsonl"
EVENTS5="$WORK/events-proxy-supersede.jsonl"
EVENTS6="$WORK/events-rotate.jsonl"
EVENTS7="$WORK/events-deaf.jsonl"
STUB_PID=""
STUB2_PID=""
STUB3_PID=""
STUB4_PID=""
STUB5_PID=""
STUB6_PID=""
STUB7_PID=""

cleanup() {
	[ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
	[ -n "$STUB2_PID" ] && kill "$STUB2_PID" 2>/dev/null || true
	[ -n "$STUB3_PID" ] && kill "$STUB3_PID" 2>/dev/null || true
	[ -n "$STUB4_PID" ] && kill "$STUB4_PID" 2>/dev/null || true
	[ -n "$STUB5_PID" ] && kill "$STUB5_PID" 2>/dev/null || true
	[ -n "$STUB6_PID" ] && kill "$STUB6_PID" 2>/dev/null || true
	[ -n "$STUB7_PID" ] && kill "$STUB7_PID" 2>/dev/null || true
	"$CONTAINER" rm -f "$NAME" "$NAME2" "$NAME3" "$NAME4" "$NAME5" "$NAME6" "$NAME7" \
		>/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" "$VOLUME2" "$VOLUME3" "$VOLUME4" "$VOLUME5" "$VOLUME6" \
		"$VOLUME7" >/dev/null 2>&1 || true
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

# start_stub <events-file> <log-file> <what> [stubop flags...]
#
# Starts a stub operator in the background, waits a moment, and stops the run
# if it did not stay up -- printing its log rather than leaving the next wait
# to time out against a stub that is not there.
#
# Every phase used to write these ten lines out again. What differed between
# them was the flags and nothing else, so the flags are what this takes: the
# thing each phase varies is now on one line at the call site instead of being
# found by reading two phases side by side. The scaffolding is shared; no
# assertion is.
#
# It echoes the PID rather than setting a named variable, so a caller keeps its
# own STUBn_PID and the trap at the top of this file keeps working unchanged.
start_stub() {
	local events="$1" log="$2" what="$3"
	shift 3
	"$STUBOP" "$@" >"$events" 2>"$log" &
	local pid=$!
	sleep 1
	if ! kill -0 "$pid" 2>/dev/null; then
		echo "the $what stub operator did not stay up:" >&2
		cat "$log" >&2
		exit 1
	fi
	printf '%s\n' "$pid"
}

# start_agent <name> <volume> <agent-dir> <config-dir> <endpoint> <image> [-e VAR=value...]
#
# Creates the volume and starts the container, with the sandbox every phase
# applies: a read-only root, an exec tmpfs for /tmp, no capabilities, no new
# privileges, and a memory limit.
#
# The limit is not decoration. Both entrypoints read the cgroup and drop
# AlwaysPreTouch when nothing bounds the container, so a phase that forgot
# --memory would silently exercise a different JVM configuration than a pod
# does -- and would say so only in a log line nobody reads.
#
# host-gateway is understood by both Docker and Podman, so the container
# reaches the stub the same way under either runtime, and the SAN each stub is
# given is the name the endpoint below dials.
start_agent() {
	local name="$1" volume="$2" agent_dir="$3" config_dir="$4" endpoint="$5" image="$6"
	shift 6
	"$CONTAINER" volume create "$volume" >/dev/null
	"$CONTAINER" run -d --name "$name" \
		--add-host stubop:host-gateway \
		--read-only --tmpfs /tmp:rw,exec,size=256m \
		--cap-drop ALL \
		--security-opt no-new-privileges \
		--memory 2g \
		-v "$volume:/data" \
		-v "$agent_dir:/var/run/spawnery:ro" \
		-v "$config_dir:/etc/spawnery:ro" \
		-e SPAWNERY_OPERATOR_ENDPOINT="$endpoint" \
		"$@" \
		"$image" >/dev/null
}

STUB_PID="$(start_stub "$EVENTS" "$WORK/stub.log" "passive" \
	--dir "$WORK/agent" \
	--san stubop \
	--listen ":19443" \
	--report-interval 1 \
	--renew-after "$RENEW_AFTER" \
	--hard-deadline 20)"

start_agent "$NAME" "$VOLUME" "$WORK/agent" "$WORK/config" "stubop:19443" "$IMAGE"

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
# The report arrives *after* the stub has already sent a NetworkState this jar
# has no branch for, and that is the assertion rather than a side effect.
#
# Every additive change to this proto rests on one property: a shipped agent
# receives a message it does not recognise and keeps its session. A jar that
# ended its stream on one would fail against the first operator that sent it --
# a fleet-wide outage on an operator upgrade, looking like a network problem
# the whole way through -- and no JUnit or Go test on either side can see it,
# because both sides are built from the same generated code.
#
# So the stub sends one to both agent kinds on connect, and every phase that
# follows is the measurement. This is the first of them.
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
# alive <container> - stops the run if the container has exited, naming what it
# was waiting for. Every wait in this script is a wait on something a live
# container is expected to do, so a container that died turns each of them into
# a wait for its own timeout and then a diagnosis pointing at whatever the wait
# was about rather than at the crash. await_event has always checked this; the
# two loops below did not, which is what docs/known-issues.md recorded.
alive() {
	local name="$1" waiting_for="$2"
	if [ -z "$("$CONTAINER" ps -q --filter "name=^${name}$")" ]; then
		echo "the container exited while waiting for $waiting_for" >&2
		"$CONTAINER" logs "$name" >&2
		cat "$WORK"/stub*.log >&2 || true
		exit 1
	fi
}

echo "waiting for a renewal..."
start=$SECONDS
until [ "$(jq -rs '[.[] | select(.kind == "stream_opened")] | length' <"$EVENTS" 2>/dev/null || echo 0)" -ge 2 ]; do
	alive "$NAME" "the renewal"
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
	# Checked here too, although this loop breaks rather than failing: a
	# container that died mid-handover would otherwise reach the verdict below
	# as "the first stream was not retired ... the agent is leaking a stream
	# per renewal", which is an accusation about an agent that is not running.
	alive "$NAME" "the outgoing stream's retirement"
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
#
# What seq orders is the stub's own record calls, not arrival on the wire. The
# two are taken under one lock (see recorder.record), so the sequence is a real
# total order over what this process observed -- it is just an order over
# observations rather than over packets, and two events arriving within a
# scheduling quantum of each other could be recorded either way round.
#
# That is enough for this verdict and not more than it, which is why the
# message below says "recorded". The failure it names is not a close and a
# greeting a microsecond apart: it is the ordering a break-before-make renewal
# produces, where the close travels an established connection and the greeting
# waits on a fresh TCP handshake and a TLS handshake behind it. Milestones of
# runs put that gap in the tens of milliseconds and it was measured losing
# every time, so a verdict that fired on a race would be a coin flip rather
# than the consistent result this has.
verdict="$(jq -rs --argjson within "$RETIRED_WITHIN" '
	(map(select(.kind == "hello" and .stream == 1)) | first | .seq) as $second_greeted |
	(map(select(.kind == "stream_closed" and .stream == 0)) | first | .seq) as $first_closed |
	if $second_greeted == null then "the second stream never greeted"
	elif $first_closed == null then "the first stream was not retired within \($within)s of the replacement opening: the agent is leaking a stream per renewal"
	elif $first_closed < $second_greeted then "the operator recorded the first stream closing before the second greeted: break before make"
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

# The connection peak, which is a different quantity from the stream count
# above and the reason internal/agentserver has a bound at all. SessionLoop
# builds a fresh ManagedChannel per attempt, so a make-before-break renewal
# holds two connections while it holds two streams -- and until the stub
# counted connections, an agent leaking one per reconnect moved no number this
# script reads. MaxConnectionsPerPeer is derived from this peak, so asserting
# it here is what keeps the derivation true: an agent change that needs a third
# concurrent connection fails on this line rather than by being refused in a
# cluster.
#
# The bound is the constant's value, restated. Reading it out of the Go source
# would couple a shell script to a parse; restating it means the two can drift,
# and the comment above internal/agentserver.MaxConnectionsPerPeer names this
# assertion so whoever moves the constant is told where the other copy lives.
CONNECTION_PEAK_BOUND=8
peak="$(jq -rs '[.[] | select(.kind == "connection") | .peak] | max // 0' <"$EVENTS")"
case "$peak" in
'' | *[!0-9]*)
	echo "could not read the connection peak: jq answered '$peak'" >&2
	exit 1
	;;
esac
if [ "$peak" -gt "$CONNECTION_PEAK_BOUND" ]; then
	jq -c 'select(.kind == "connection")' <"$EVENTS" >&2
	echo "the agent held $peak connections at once, over the $CONNECTION_PEAK_BOUND that internal/agentserver admits per peer" >&2
	exit 1
fi
if [ "$peak" -lt 2 ]; then
	echo "the agent never held two connections at once, so this trace saw no renewal overlap on the wire and the peak proves nothing" >&2
	exit 1
fi
echo "the connection peak was $peak, within the $CONNECTION_PEAK_BOUND per peer the operator admits"

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
STUB2_PID="$(start_stub "$EVENTS2" "$WORK/stub2.log" "superseding" \
	--dir "$WORK/agent-supersede" \
	--san stubop \
	--listen ":19444" \
	--report-interval 1 \
	--renew-after "$RENEW_AFTER" \
	--hard-deadline 20 \
	--supersede)"

start_agent "$NAME2" "$VOLUME2" "$WORK/agent-supersede" "$WORK/config" "stubop:19444" "$IMAGE"

echo "waiting up to ${DEADLINE}s for the agent to greet the superseding operator..."
await_event hello "$EVENTS2" "$NAME2"

# WINDOW, RENEWALS, LIMIT and FLOOR are declared at the top of this script,
# beside RENEW_AFTER they derive from, because phase 5 measures the same
# quantity with the same bounds. What each one is for is written there; what
# follows is why this phase needs both directions.
#
# Twice the renewals due plus two is the ceiling: a correct agent is nowhere
# near it and the 1 Hz churn this phase exists to catch is several times past
# it, so the number is not a timing guess.
#
# Half the renewals due is the floor, and it is not decoration. Everything this
# phase measures is bounded from above, so every way the agent can stop - a
# container that died, a wedged session, a stub that stopped renewing - produces
# a low count and would print "no reconnect storm" on the strength of it. That
# is the milestone's own assertion passing for the exact failure it exists to
# catch.
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
STUB3_PID="$(start_stub "$EVENTS3" "$WORK/stub3.log" "muting" \
	--dir "$WORK/agent-mute" \
	--san stubop \
	--listen ":19445" \
	--report-interval 1 \
	--renew-after "$RENEW_AFTER" \
	--hard-deadline "$MUTE_HARD_DEADLINE" \
	--supersede \
	--mute-after 1)"

start_agent "$NAME3" "$VOLUME3" "$WORK/agent-mute" "$WORK/config" "stubop:19445" "$IMAGE"

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
# An overlay lowering read-timeout, which is the exact shape of the thing the
# operator could not see: `spec.configOverlay` names the user's own ConfigMap,
# it is mounted into the pod by name, and spawnery-config folds it into
# velocity.toml inside the container. The rendered file sets no read-timeout of
# its own -- internal/render writes config-version, motd, show-max-players and
# the forwarding keys, and Velocity supplies the rest from its own defaults --
# so an overlay is the only way the value ever differs, and it is the case
# worth proving reaches the wire.
#
# Under [advanced], which is where Velocity declares it. The first draft of
# this put it at the top level and spawnery-config refused the whole render --
# "a key at the wrong depth is not read and not refused" -- which is
# internal/render's overlay check doing exactly what it exists for, on the way
# to this assertion rather than in somebody's cluster.
PROXY_READ_TIMEOUT=12000
mkdir -p "$WORK/velocity-config/overlay"
printf '[advanced]\nread-timeout = %s\n' "$PROXY_READ_TIMEOUT" \
	>"$WORK/velocity-config/overlay/velocity.toml"
chmod 0755 "$WORK/velocity-config" "$WORK/velocity-config/overlay"
chmod 0644 "$WORK/velocity-config/config.yaml" "$WORK/velocity-config/forwarding.secret" \
	"$WORK/velocity-config/overlay/velocity.toml"

# How long the stub holds the FullSync back, counted from the stream opening.
# Everything between the agent's Hello and the sync has to fit inside it: the
# poll that notices the greeting, the control probe below, and the closed-gate
# probe itself. Generous rather than tight, because the cost of overrunning it
# is a failed run and the cost of a long hold is 45 seconds.
FULL_SYNC_AFTER=45

# And how long it then holds the SetReady back, counted from that FullSync -
# cmd/spawnery-stubop/main.go's `delayed` chain counts each `after` from the
# previous send, and records the event once that Send has returned.
#
# What has to fit inside it is the poll that notices the sync and the whole of
# the open-gate probe below, its deadline included: a withdrawal that lands
# before that probe has given up is a run that measured this script's flags
# rather than the agent, and the two arms of the guard below say so by name.
# That is an arithmetic claim, so here is the arithmetic, in the constants as
# they stand:
#
#   start   <= T_fullsync + 2       await_event polls on `sleep 2` (:169) and
#                                   `start` is taken the line after it returns
#   timeout <= start + GATE_WITHIN + 1 + one port_open exec
#            = T_fullsync + 2 + 30 + 1 + exec
#   SetReady at T_fullsync + 60
#
# So the probe's deadline is reached about 33s after the FullSync and the
# withdrawal is still some 27 seconds away, which is the margin a single
# `container exec` would have to overrun for the two arms to swap. At the 20s
# this held until 2026-08-14 the same arithmetic ran the other way - 33 > 20 -
# and the withdrawal was always already out by the time the deadline was
# reached, so an agent that never opened its gate was announced as a
# misconfigured harness: the guard's own message, inverted. Measured that day
# by driving the loop below against a trace whose port never opens: at 20s it
# takes the first arm, at 60s the second, and at 5s the first again.
#
# The player count assertion that follows may overrun it freely - the only
# difference is that closed_after reads 0. Generous for the same reason
# FULL_SYNC_AFTER is: what raising it costs is 40 further seconds of run time,
# and what it has to stay under is the renewal and the session deadline the
# stub is started with below, 180s and 240s from the stream opening against a
# withdrawal now due at 45 + 60 = 105.
SET_READY_AFTER=60s

# renew-after is 180 rather than the 5 the phases above use, so exactly one
# stream exists for the whole of this phase. Under a 5s renewal each new stream
# would start a schedule of its own and the trace would carry a full_sync_sent
# and a set_ready_sent per stream; nothing about the gate would change, but the
# three events this phase orders its probes against would stop being one
# sequence. It has to outlast the SetReady as well as the sync: a renewal
# landing between them would retire the stream the withdrawal was scheduled on,
# and the stub would record set_ready_failed on a stream that no longer exists.
# Counted from the stream opening that is FULL_SYNC_AFTER + SET_READY_AFTER =
# 45 + 60 = 105 seconds against a renewal at 180, so the margin is 75s; raising
# either hold past 180 between them is what would break this.
mkdir -p "$WORK/agent-proxy"
chmod 0755 "$WORK/agent-proxy"
STUB4_PID="$(start_stub "$EVENTS4" "$WORK/stub4.log" "proxy" \
	--dir "$WORK/agent-proxy" \
	--san stubop \
	--listen ":19446" \
	--report-interval 1 \
	--renew-after 180 \
	--hard-deadline 240 \
	--proxy \
	--require-token \
	--full-sync-after "$FULL_SYNC_AFTER" \
	--set-ready-after "$SET_READY_AFTER")"

# The two proxy-only variables are not decoration: without either one the agent
# goes dormant and never connects at all, naming the variable it did not get.
start_agent "$NAME4" "$VOLUME4" "$WORK/agent-proxy" "$WORK/velocity-config" "stubop:19446" "$VELOCITY_IMAGE" \
	-e SPAWNERY_PLAYER_LIMIT="$PROXY_LIMIT" \
	-e SPAWNERY_FALLBACK_GROUPS=lobby

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

# The read timeout the proxy actually parsed, on its Hello. It is the one thing
# on that message the operator cannot find out any other way: the value lives in
# a velocity.toml the operator never reads, and the operator races that deadline
# every time a backend's node dies.
#
# The assertion is against the *overlay*, not against Velocity's default, and
# that is what makes it worth having. A proxy reporting 30000 would prove
# nothing this repository did not already assume -- the number would be right
# by coincidence, since nothing renders read-timeout and Velocity's own default
# is 30000. Reporting the overlay's number proves the value came from
# ProxyServer.getConfiguration(), through a real Velocity that had really read
# a really mounted overlay, which is the only path a person's lowered timeout
# can take.
reported_timeout="$(jq -rs '[.[] | select(.kind == "hello") | .readTimeoutMillis] | first // 0' <"$EVENTS4")"
if [ "$reported_timeout" != "$PROXY_READ_TIMEOUT" ]; then
	echo "the proxy reported a read timeout of ${reported_timeout}ms; its overlay sets "\
		"${PROXY_READ_TIMEOUT}ms" >&2
	echo "the operator races this deadline when a backend's node dies, and a value it cannot "\
		"see is one it assumes" >&2
	jq -rs '[.[] | select(.kind == "hello")]' <"$EVENTS4" >&2
	"$CONTAINER" exec "$NAME4" cat /data/velocity.toml >&2 || true
	exit 1
fi
echo "the proxy reported the ${reported_timeout}ms read timeout its mounted overlay set"

# The other half of the control, and the half that was missing:
# docs/known-issues.md recorded that the probe had been shown able to answer
# true and never shown able to answer false for the ordinary reason. The 25565
# probe above rules out a prober that always says no; this rules out one that
# always says yes -- a /dev/tcp redirect that succeeded on a refused
# connection, a bash that treats the failure as success, a runtime whose exec
# swallows the exit code.
#
# 1 is chosen because nothing in either image can be listening on it: it is
# privileged, the containers drop every capability, and both entrypoints exec a
# JVM that binds 25565 and 8081 and nothing else. Any port nothing binds would
# do; a port that could conceivably be bound would make a failure here
# ambiguous.
UNBOUND_PORT=1
if port_open "$NAME4" "$UNBOUND_PORT"; then
	echo "port_open answered true for port $UNBOUND_PORT, which nothing in this image binds" >&2
	echo "the closed-gate assertion below cannot fail, so it proves nothing" >&2
	exit 1
fi
echo "$UNBOUND_PORT reads closed, so the probe tells the two apart"

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
		# first.
		#
		# Which of the two arms a failing run takes is decided by the constants
		# rather than by the agent, so they are evaluated where SET_READY_AFTER
		# is set: at GATE_WITHIN=30 this branch is reached about 33s after the
		# FullSync, against a withdrawal due 60s after it. So an agent that
		# never opens its gate reaches the arm below this one, and this arm is
		# what a SET_READY_AFTER of about 33s or less produces - the
		# misconfiguration it names. Both are reachable; at the 20s
		# SET_READY_AFTER held until 2026-08-14, only this one was, and every
		# agent that failed to open its gate was reported as a harness fault.
		#
		# Assigned rather than tested inline, for the reason count_events
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
# having established only the first half: the close it names would be one it
# never saw happen. Both arms name SET_READY_AFTER because raising it is the
# answer to either.
withdrawn4="$(count_events set_ready_sent "$EVENTS4")"
if [ "$withdrawn4" -ne 0 ]; then
	echo "the SetReady went out before the gate could be probed open; raise SET_READY_AFTER" >&2
	echo "the close below would then be asserted against a gate that had already shut, and this phase would pass having never observed the close it reports" >&2
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

# The message the operator's drain rests on, and the one whose absence looks
# exactly like a working agent.
#
# BackendPlayers is what tells the operator a player is *arriving* at a
# backend. Neither the backend nor the proxy's own getPlayersConnected() can
# see one -- velocity 3.5.1 build 615 calls VelocityRegisteredServer.addPlayer
# from BackendPlaySessionHandler.activated() and from nowhere else, so a player
# still in the configuration phase is counted by neither. An agent that stopped
# sending this would leave every other assertion in this phase green while a
# drain deleted a pod under somebody, which is why it is checked here rather
# than left to the unit tests that cannot reach a real proxy.
#
# The map is empty in this phase -- no player has ever joined this proxy -- and
# that is the assertion: the message arrives on the reporting tick whether or
# not anybody is on a backend, because it is a state and not an event. A
# version that only sent it when it had something to say would be silent
# exactly when the operator most needs to hear "nobody".
await_event backend_players "$EVENTS4" "$NAME4"
backends4="$(jq -rs '[.[] | select(.kind == "backend_players")] | length' <"$EVENTS4")"
case "$backends4" in
'' | *[!0-9]*)
	echo "could not count backend_players events: jq answered '$backends4'" >&2
	exit 1
	;;
esac
if [ "$backends4" -lt 1 ]; then
	echo "the proxy never reported which backends its players are on" >&2
	exit 1
fi
empty4="$(jq -rs '[.[] | select(.kind == "backend_players") | .backends | length] | max' <"$EVENTS4")"
if [ "$empty4" != "0" ]; then
	echo "the proxy reported players on a backend nobody has ever joined: $empty4" >&2
	jq -c 'select(.kind == "backend_players")' <"$EVENTS4" >&2
	exit 1
fi
echo "the proxy reports its backend attachments, empty and on every tick"

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
#
# **Observed failing, 2026-08-27, which docs/known-issues.md recorded that it
# never had been.** This arm could only be seen working if an agent shut its
# listener on a SetReady, which no correct one does and no fault this harness
# can inject produces -- so it was a control that had never controlled
# anything. The answer was a mutation rather than a fault injector kept in the
# repository: AgentPlugin's `onSetReady` was built once as
# `if (ready) gate.open() else { gate.close(); proxy.shutdown() }`, the images
# rebuilt, and this phase run. The 8081 loop above passed, this check failed
# with its own message, and Velocity's own log carried
# `Closing endpoint /0.0.0.0:25565` at the moment the gate closed. Reverted and
# re-run green.
#
# What that mutation is not is a listener closing while the JVM lives, which
# nothing can produce -- Velocity exposes no way to stop listening without
# stopping. It does not need to be. The claim this guards is that the proxy
# goes on *serving*, and shutting it down violates that claim in the most
# direct way there is: every player on it is dropped. `port_open` cannot tell
# the two apart and does not have to.
if ! port_open "$NAME4" 25565; then
	echo "the proxy's own listener on 25565 went down with the ready gate" >&2
	echo "withdrawing readiness must take this pod out of the Service's endpoints, not out of service: the players already on it have to keep playing" >&2
	"$CONTAINER" logs "$NAME4" 2>&1 | tail -40 >&2
	exit 1
fi
echo "the ready gate closed ${closed_after}s after the operator withdrew readiness, and 25565 still answers"

# The roster, from the real jar.
#
# **What this proves and what it does not.** It proves the shipped Velocity
# plugin builds and sends a PlayerRoster that the operator's own parser reads
# -- which catches a proto mismatch, a missing import, and a shading problem in
# the generated Java, none of which any JUnit or Go test can see. It does not
# prove a UUID is correct, because no player is connected: this harness has
# never driven a join, and doing so needs a live backend for the proxy to route
# to, which is its own project rather than a step of this one. The empty roster
# is exactly what a proxy with nobody on it should send, and sending it at all
# is the thing this milestone added.
#
# docs/runbook-milestone-3-evidence.md is where a real client's join is driven,
# and is where a real UUID would be observed.
await_event player_roster "$EVENTS4" "$NAME4"
roster_players="$(jq -rs '[.[] | select(.kind == "player_roster")] | last | .players | length' <"$EVENTS4")"
if [ "$roster_players" != "0" ]; then
	echo "the proxy reported $roster_players players with nobody connected" >&2
	jq -rs '[.[] | select(.kind == "player_roster")] | last' <"$EVENTS4" >&2
	exit 1
fi
echo "the proxy sends a PlayerRoster the operator can parse, empty with nobody connected"

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
STUB5_PID="$(start_stub "$EVENTS5" "$WORK/stub5.log" "superseding proxy" \
	--dir "$WORK/agent-proxy-supersede" \
	--san stubop \
	--listen ":19447" \
	--report-interval 1 \
	--renew-after "$RENEW_AFTER" \
	--hard-deadline 20 \
	--supersede \
	--proxy \
	--require-token)"

start_agent "$NAME5" "$VOLUME5" "$WORK/agent-proxy-supersede" "$WORK/velocity-config" \
	"stubop:19447" "$VELOCITY_IMAGE" \
	-e SPAWNERY_PLAYER_LIMIT="$PROXY_LIMIT" \
	-e SPAWNERY_FALLBACK_GROUPS=lobby

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

# ---------------------------------------------------------------------------
# Phase six: the CA rotation's overlap, from the agent's own trust store.
#
# Every phase above authenticates the stub's server certificate against a
# ca.crt holding exactly one CA, which is also what every JUnit test that
# builds an OperatorChannel does - so none of them can tell a trustManager
# that reads the first certificate of a bundle from one that reads all of
# them. --rotate-ca is the one flag that changes what ca.crt holds: two CAs,
# built with internal/certs the same way a real rotation builds them, with the
# serving certificate signed by the second. See cmd/spawnery-stubop/main.go's
# materialiseRotated for why it is built from that package rather than by
# hand.
echo
echo "restarting the agent against a stub mid CA rotation..."
"$CONTAINER" rm -f "$NAME5" >/dev/null 2>&1 || true
kill "$STUB5_PID" 2>/dev/null || true
STUB5_PID=""

# renew-after well past this phase's own patience, for the same reason phase
# four's proxy stub uses 180: this phase asserts on the handshake alone, and a
# renewal mid-run would open a second stream signed by the same bundle without
# adding anything this phase is checking.
mkdir -p "$WORK/agent-rotate"
chmod 0755 "$WORK/agent-rotate"
STUB6_PID="$(start_stub "$EVENTS6" "$WORK/stub6.log" "rotating" \
	--dir "$WORK/agent-rotate" \
	--san stubop \
	--listen ":19448" \
	--report-interval 1 \
	--renew-after 180 \
	--hard-deadline 240 \
	--rotate-ca)"

start_agent "$NAME6" "$VOLUME6" "$WORK/agent-rotate" "$WORK/config" "stubop:19448" "$IMAGE"

# Reaching this line is the whole proof: the obvious mutation is mounting only
# ca.crt's first PEM, which is exactly the bundle a pod carries before a
# rotation ever starts, against a server whose certificate is signed by the
# second - a handshake a single-PEM trustManager cannot complete, so the agent
# would never reach hello and this would time out instead.
echo "waiting up to ${DEADLINE}s for the agent to greet a server signed by the second CA..."
await_event hello "$EVENTS6" "$NAME6"
echo "the agent completed a handshake against a certificate signed by the CA that was second in its mounted bundle"

await_event ready "$EVENTS6" "$NAME6"
echo "the agent completed a session through it"

# ---------------------------------------------------------------------------
# Phase seven: a connection that is up and going nowhere.
#
# Every phase above ends a stream in a way the agent can see: the stub closes
# it, supersedes it, or accepts it and never answers -- and SessionLoop has a
# clock for each. This one takes the case with no clock on either side. The
# stub goes deaf mid-session: it stops reading and stops writing on every
# connection and closes none of them, so no FIN, no RST, and no answer ever
# again. See `deafness` in cmd/spawnery-stubop/deafen.go for what that
# reproduces and what it does not.
#
# Two things are proved here and they pull in opposite directions.
#
# Before the deafening, that the keepalive is *allowed*. OperatorChannel pings
# every 45 seconds and the operator's KeepaliveEnforcementPolicy sends a GOAWAY
# to a client that pings oftener than MinKeepaliveInterval. The stub carries
# that same policy now, so a ping refused would break the stream and the agent
# would reconnect -- which is why the connection count at the moment of
# deafening is taken and asserted, rather than merely noted. It is the whole
# GOAWAY check, and it is why this phase runs long enough for a ping to cross.
#
# After it, that the keepalive is *enough*. Nothing else can end this wait: the
# renewal is set past the phase, no timer in the agent is armed by anything the
# operator said, and its own sends go on succeeding into a kernel buffer. A new
# connection reaching the stub is therefore the ping having given up and
# SessionLoop having started again -- and the connection event is recorded at
# Accept, before a byte is read, so a stub that is deaf still records it.
#
# The phase costs about two minutes, which is what testing a 45-second timer
# costs. The alternative is a knob on the agent that exists only for this test,
# and a number nothing in production uses is a number this would not be
# measuring.
echo
echo "deafening the operator mid-session, to prove the agent has a clock of its own..."
"$CONTAINER" rm -f "$NAME6" >/dev/null 2>&1 || true
kill "$STUB6_PID" 2>/dev/null || true
STUB6_PID=""

# The keepalive fires 45s after the last thing the operator said and waits 20
# for the answer, so deafening at 50 lands after the first ping has been
# answered and before the second is due.
DEAFEN_AFTER=50
# Worst case is a ping sent the instant before the deafening: 45 to the next
# one and 20 more to give up on it. Half as much again for a loaded machine.
DEAF_LIMIT=100

mkdir -p "$WORK/agent-deaf"
chmod 0755 "$WORK/agent-deaf"
STUB7_PID="$(start_stub "$EVENTS7" "$WORK/stub7.log" "deafening" \
	--dir "$WORK/agent-deaf" \
	--san stubop \
	--listen ":19450" \
	--report-interval 1 \
	--renew-after 600 \
	--hard-deadline 1200 \
	--deafen-after "${DEAFEN_AFTER}s")"

start_agent "$NAME7" "$VOLUME7" "$WORK/agent-deaf" "$WORK/config" "stubop:19450" "$IMAGE"

await_event hello "$EVENTS7" "$NAME7"
echo "the agent connected; waiting ${DEAFEN_AFTER}s for the stub to go deaf, past the first keepalive..."
await_event deafened "$EVENTS7" "$NAME7"

before="$(count_events connection "$EVENTS7")"
reports="$(count_events player_count "$EVENTS7")"
# A ping the enforcement policy refused would have cost the stream and shown up
# as a reconnect; the reports are the same statement from the other side.
if [ "$before" -gt 2 ]; then
	echo "the agent opened $before connections before the stub went deaf, want at most 2 (the "\
		"session and a renewal overlap). A GOAWAY for pinging oftener than "\
		"MinKeepaliveInterval looks exactly like this" >&2
	jq -rs '.' <"$EVENTS7" >&2
	exit 1
fi
if [ "$reports" -lt $((DEAFEN_AFTER / 2)) ]; then
	echo "only $reports player counts arrived in ${DEAFEN_AFTER}s at a 1s interval; the stream "\
		"did not survive the first keepalive ping" >&2
	jq -rs '.' <"$EVENTS7" >&2
	exit 1
fi
echo "$reports player counts crossed and the connection was not remade, so the ping was answered rather than refused"

echo "waiting up to ${DEAF_LIMIT}s for the agent to give up on a connection nothing will ever answer..."
deaf_start="$SECONDS"
until [ "$(count_events connection "$EVENTS7")" -gt "$before" ]; do
	if [ $((SECONDS - deaf_start)) -gt "$DEAF_LIMIT" ]; then
		echo "the agent held a dead connection for ${DEAF_LIMIT}s without reconnecting. Nothing "\
			"else can end this wait: OperatorChannel's keepalive is the only clock on it" >&2
		jq -rs '.' <"$EVENTS7" >&2
		"$CONTAINER" logs "$NAME7" | tail -40 >&2
		exit 1
	fi
	sleep 2
done
echo "the agent reconnected $((SECONDS - deaf_start))s after the operator went silent, on its keepalive alone"

echo "agent-test: ok"
