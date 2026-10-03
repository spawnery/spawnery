#!/usr/bin/env bash
# The agents against a real operator-shaped stub (cmd/spawnery-stubop): what
# only a real JVM, TLS handshake and HTTP/2 stream can show -- the shaded gRPC
# stack inside Paper's classloader, trust in the mounted CA bundle and nothing
# else, renewals that really overlap, the proxy's readiness port, and a CA
# rotation's two-certificate bundle. Each phase is described where it starts.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
VELOCITY_IMAGE="${VELOCITY_IMAGE:?VELOCITY_IMAGE must be set}"
STUBOP="${STUBOP:?STUBOP must be set}"
DEADLINE="${DEADLINE:-240}"

# Shared by every phase's stub but the proxy-sync one, so the stream-rate
# bounds below stay in step with --renew-after.
RENEW_AFTER=5

# Must match syncedGroup in cmd/spawnery-stubop/main.go.
SYNCED_GROUP=lobby
if ! grep -q "syncedGroup   = \"$SYNCED_GROUP\"" "$(dirname "$0")/../cmd/spawnery-stubop/main.go"; then
	echo "SYNCED_GROUP no longer matches syncedGroup in cmd/spawnery-stubop/main.go" >&2
	echo "the console check below would look for a string nothing sends and pass by never matching" >&2
	exit 1
fi
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
# INT and TERM too: an untrapped SIGINT exits without running the handler.
trap cleanup EXIT INT TERM

# The container runs as uid 10001 and must traverse it; mktemp -d gives 0700.
mkdir -p "$WORK/agent"
chmod 0755 "$WORK/agent"

# The renderer refuses to start without both files (see hack/image-test.sh);
# maxPlayers must be 100 to match the slots assertion below.
mkdir -p "$WORK/config"
printf 'maxPlayers: 100\n' >"$WORK/config/config.yaml"
printf 'test-forwarding-secret\n' >"$WORK/config/forwarding.secret"
chmod 0755 "$WORK/config"
chmod 0644 "$WORK/config/config.yaml" "$WORK/config/forwarding.secret"

# start_stub <events-file> <log-file> <what> [stubop flags...] - prints the PID.
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
# --memory matters: both entrypoints drop AlwaysPreTouch when the cgroup is
# unbounded. host-gateway works under both Docker and Podman.
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

# -i on this container only: a pod runs with stdin closed, and this is the one
# phase that drives the server console (Paper runs `java ... --nogui`).
#
# $WORK/agent/plugins stands in for the plugin claim at
# /var/run/spawnery/plugins. The jar is deliberately not a jar: only the copy
# is under test, not Paper's plugin loader.
mkdir -p "$WORK/agent/plugins/ProbePlugin"
printf 'not a jar\n' >"$WORK/agent/plugins/probe-plugin.jar"
printf 'probe: true\n' >"$WORK/agent/plugins/ProbePlugin/config.yml"

start_agent "$NAME" "$VOLUME" "$WORK/agent" "$WORK/config" "stubop:19443" "$IMAGE" -i

# Dormant and hung look the same on the wire; only the container log tells them apart.
explain_silence() {
	local name="$1"
	if "$CONTAINER" logs "$name" 2>&1 | grep -q 'spawnery agent dormant'; then
		echo "the agent went dormant rather than hanging - the endpoint, the CA or the token did not reach it:" >&2
		"$CONTAINER" logs "$name" 2>&1 | grep -i 'spawnery' >&2 || true
	fi
}

require_api_installed() {
	local name="$1" start=$SECONDS
	# Waited for: the platform buffers its stdout, so `logs` lags the process.
	until "$CONTAINER" logs "$name" 2>&1 | grep -q 'spawnery API installed'; do
		if [ -z "$("$CONTAINER" ps -q --filter "name=^${name}$")" ]; then
			echo "the container exited before installing its plugin API" >&2
			"$CONTAINER" logs "$name" >&2
			exit 1
		fi
		if [ $((SECONDS - start)) -gt "$DEADLINE" ]; then
			echo "the agent never installed its plugin API within ${DEADLINE}s" >&2
			echo "every other test of that API is JUnit against a fake; this line is the only thing that says the install path runs inside the shipped jar" >&2
			"$CONTAINER" logs "$name" 2>&1 | grep -i spawnery >&2 || true
			exit 1
		fi
		sleep 1
	done
	# Presence only: this harness sets no SPAWNERY_NETWORK or SPAWNERY_GROUP.
	echo "the plugin API is installed and available to other plugins"
}

# console <container> <line> - type one line into the server's console.
# The timeout guards against an attach that does not return when its stdin closes.
console() {
	local name="$1" line="$2"
	printf '%s\n' "$line" | timeout 10 "$CONTAINER" attach --sig-proxy=false "$name" >/dev/null 2>&1 || true
}

# Retried: the platform buffers its stdout, and the mirror is empty until the
# first NetworkState arrives.
require_cloud_list() {
	local name="$1" start=$SECONDS
	until "$CONTAINER" logs "$name" 2>&1 | grep -q "$SYNCED_GROUP ("; do
		if [ -z "$("$CONTAINER" ps -q --filter "name=^${name}$")" ]; then
			echo "the container exited before answering a console command" >&2
			"$CONTAINER" logs "$name" >&2
			exit 1
		fi
		if [ $((SECONDS - start)) -gt "$DEADLINE" ]; then
			echo "/cloud list never named the group the stub synced, within ${DEADLINE}s" >&2
			echo "the tree is covered by JUnit against a fake source; this is the only check that runs it inside the shipped jar" >&2
			"$CONTAINER" logs "$name" 2>&1 | tail -30 >&2
			exit 1
		fi
		console "$name" "cloud list"
		sleep 2
	done
	echo "/cloud list answered from inside the shipped jar and named the synced group"
}

# await_event <kind> [events file] [container]
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

# count_events <kind> <events file> - refuses a non-number, because
# `[ "" -ne 0 ]` reads as false in an `if` instead of failing. Callers assign
# the result first so `set -e` sees the exit.
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

streams_opened() {
	count_events stream_opened "$1"
}

# port_open <container> <port> - probed from inside, never through a published
# port: rootless podman's forwarder accepts on the host with nothing listening
# inside.
port_open() {
	"$CONTAINER" exec "$1" bash -c "exec 3<>/dev/tcp/127.0.0.1/$2" >/dev/null 2>&1
}

echo "waiting up to ${DEADLINE}s for the agent to greet..."
await_event hello
require_api_installed "$NAME"
echo "the agent connected"

require_cloud_list "$NAME"

# Delivery of the cloud-event feed to chat is not testable here: there is no
# player, and the console is not one (PaperAudience lists players). What is
# testable is that an agent with nobody watching reports no interest.
echo "waiting for the agent to report that nobody is watching for cloud events..."
await_event event_interest
if ! jq -e 'select(.kind == "event_interest") | select(.wanted == false)' <"$EVENTS" >/dev/null; then
	echo "the agent asked for cloud events on a server with nobody online" >&2
	jq -c 'select(.kind == "event_interest")' <"$EVENTS" >&2
	exit 1
fi
echo "the agent reports no interest in events while nobody is online"

# Read from the filesystem: Paper ignores an unloadable file in plugins/
# silently. Waited for, because `exec` can reach the container before the
# entrypoint's copy has run.
echo "waiting for the plugin volume's contents to reach the shipped image..."
start=$SECONDS
until "$CONTAINER" exec "$NAME" sh -c 'cat /data/plugins/probe-plugin.jar' 2>/dev/null | grep -q 'not a jar'; do
	if [ -z "$("$CONTAINER" ps -q --filter "name=^${NAME}$")" ]; then
		echo "the container exited before the plugin volume was copied" >&2
		"$CONTAINER" logs "$NAME" >&2
		exit 1
	fi
	if [ $((SECONDS - start)) -gt "$DEADLINE" ]; then
		echo "the plugin from the group's volume never reached /data/plugins within ${DEADLINE}s" >&2
		echo "the entrypoint's copy is covered by go test ./image; this is the only check that runs it inside the shipped image" >&2
		"$CONTAINER" exec "$NAME" sh -c 'ls -la /var/run/spawnery/plugins /data/plugins 2>&1' >&2 || true
		exit 1
	fi
	sleep 2
done
if ! "$CONTAINER" exec "$NAME" sh -c 'cat /data/plugins/ProbePlugin/config.yml' 2>/dev/null | grep -q 'probe: true'; then
	echo "the jar arrived and its configuration did not, so the copy is not carrying the tree" >&2
	"$CONTAINER" exec "$NAME" sh -c 'ls -la /data/plugins /data/plugins/* 2>&1' >&2 || true
	exit 1
fi
echo "a plugin and its configuration reached the shipped image from the group's volume"

# Having greeted at all proves the relocated gRPC classes ran inside Paper's
# classloader.

# This stub runs without --require-token, so this comparison is the phase's
# only credential check.
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

# The stub has already sent a NetworkState this jar has no branch for: every
# phase from here proves an agent keeps its session on an unknown message.
await_event player_count
# Retried until it parses: the stub's last line may be partial.
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

# alive <container> <what> - fail at once rather than time out on a dead container.
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

# Bounded by a deadline, not a sleep: a correct but slow handover must not
# read as a leak.
RETIRED_WITHIN=30
start=$SECONDS
until [ "$(jq -rs '[.[] | select(.kind == "stream_closed" and .stream == 0)] | length' <"$EVENTS" 2>/dev/null || echo 0)" -ge 1 ]; do
	# Otherwise a container that died mid-handover would be reported as a leak.
	alive "$NAME" "the outgoing stream's retirement"
	if [ $((SECONDS - start)) -gt "$RETIRED_WITHIN" ]; then
		break
	fi
	sleep 1
done

# Captured, not piped into grep: under pipefail a SIGPIPE'd jq fails `jq | grep -q`.
#
# The stub never closes a stream, so a first stream that never closed is the
# agent leaking one. seq orders the stub's observations, not packets; a
# break-before-make renewal loses by a whole TCP and TLS handshake, so the
# ordering is not a race.
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

jq -rs '
	(map(select(.kind == "stream_closed" and .stream == 0)) | first | .seq) as $retired |
	map(select(.seq <= $retired and (.stream == 0 or .stream == 1)))
	| map("  seq \(.seq)  stream \(.stream)  \(.kind)")
	| .[-6:] | .[]
' <"$EVENTS"

# SessionLoop builds a fresh ManagedChannel per attempt, so a renewal holds two
# connections. The bound restates internal/agentserver.MaxConnectionsPerPeer,
# whose comment points back here.
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
# internal/agentserver cancels the displaced stream before the replacement's
# first Send; an agent that reads that as a breakage reconnects about once a
# second. Nothing about the order is wrong then, so this asserts the rate.
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

# The floor matters as much as the ceiling: every way the agent can stop gives
# a low count that an upper bound alone would pass.
before="$(streams_opened "$EVENTS2")"
echo "counting the streams the agent opens over ${WINDOW}s of renewals..."
sleep "$WINDOW"
after="$(streams_opened "$EVENTS2")"
opened=$((after - before))

# A stub that never superseded would also give a low count.
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
	"$CONTAINER" logs "$NAME2" 2>&1 | tail -20 >&2 || true
	exit 1
fi
echo "the agent opened $opened streams in ${WINDOW}s across $superseded supersessions: one per renewal, no reconnect storm"

# ---------------------------------------------------------------------------
# Phase three: the operator that accepts a stream and then says nothing.
#
# Between the cancel and its first Send, internal/agentserver has armed no
# deadline, and the agent's channel has no keepalive, idle timeout or call
# deadline of its own. With no bound the agent opens no further stream, which
# only the floor can see.
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

# Stream 1 is the first muted one; the window starts when it opens.
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

# The agent's bound on an unanswered stream is the operator's
# hardDeadlineSeconds, so a correct agent gives up at 20s and 41s: two streams.
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
# The pod's only readiness probe is a tcpSocket on 8081 (internal/podspec). The
# agent binds it on the first FullSync and releases it on SetReady{ready:false}.
# The stub holds the FullSync back so the closed half can be probed first.
echo
echo "starting the proxy against an operator that holds its server list back..."
"$CONTAINER" rm -f "$NAME3" >/dev/null 2>&1 || true
kill "$STUB3_PID" 2>/dev/null || true
STUB3_PID=""

# A proxy's own config, as in hack/velocity-image-test.sh. playerLimit and
# SPAWNERY_PLAYER_LIMIT come from one ProxyGroup field in a cluster.
PROXY_LIMIT=100
mkdir -p "$WORK/velocity-config"
# render.Velocity refuses to guess onlineMode; true costs nothing, since no
# phase here joins as a player.
printf 'playerLimit: %s\nonlineMode: true\n' "$PROXY_LIMIT" >"$WORK/velocity-config/config.yaml"
printf 'test-forwarding-secret\n' >"$WORK/velocity-config/forwarding.secret"
# Nothing renders read-timeout, so only an overlay makes the reported value
# differ from Velocity's default. It belongs under [advanced]; spawnery-config
# refuses a key at the wrong depth.
PROXY_READ_TIMEOUT=12000
mkdir -p "$WORK/velocity-config/overlay"
printf '[advanced]\nread-timeout = %s\n' "$PROXY_READ_TIMEOUT" \
	>"$WORK/velocity-config/overlay/velocity.toml"
chmod 0755 "$WORK/velocity-config" "$WORK/velocity-config/overlay"
chmod 0644 "$WORK/velocity-config/config.yaml" "$WORK/velocity-config/forwarding.secret" \
	"$WORK/velocity-config/overlay/velocity.toml"

# Counted from the stream opening; the closed-gate probes must fit inside it.
FULL_SYNC_AFTER=45

# Counted from the FullSync (the stub's `delayed` chain counts from the
# previous send). The open-gate probe gives up about 33s after the FullSync
# (2s poll + GATE_WITHIN + an exec), so 60 keeps its two failure arms apart.
# FULL_SYNC_AFTER + SET_READY_AFTER must stay below the 180s renewal.
SET_READY_AFTER=60s

# renew-after 180 keeps one stream for the whole phase, so the FullSync and the
# SetReady are one schedule on it.
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

# Without either proxy-only variable the agent goes dormant.
start_agent "$NAME4" "$VOLUME4" "$WORK/agent-proxy" "$WORK/velocity-config" "stubop:19446" "$VELOCITY_IMAGE" \
	-e SPAWNERY_PLAYER_LIMIT="$PROXY_LIMIT" \
	-e SPAWNERY_FALLBACK_GROUPS=lobby

echo "waiting up to ${DEADLINE}s for the proxy agent to greet..."
await_event hello "$EVENTS4" "$NAME4"
require_api_installed "$NAME4"
echo "the proxy agent connected"

# --require-token proves some token arrived; this proves it was the mounted
# one. Dropping ProxyRole.open()'s credentials passes every JUnit test.
token4="$(cat "$WORK/agent-proxy/token")"
if [ -z "$token4" ]; then
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

# Control: port_open reads every exec failure as closed, so first show it can
# see the proxy's own 25565.
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

# Asserted against the overlay, not Velocity's default of 30000, which would
# match by coincidence.
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

# The opposite control, against a probe that always answers true. Port 1 is
# privileged, the containers drop every capability, and the JVM binds only
# 25565 and 8081.
UNBOUND_PORT=1
if port_open "$NAME4" "$UNBOUND_PORT"; then
	echo "port_open answered true for port $UNBOUND_PORT, which nothing in this image binds" >&2
	echo "the closed-gate assertion below cannot fail, so it proves nothing" >&2
	exit 1
fi
echo "$UNBOUND_PORT reads closed, so the probe tells the two apart"

# Checked before and after the probe: a FullSync already out would make the
# closed-gate result meaningless.
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

# Retried: the stub records full_sync_sent when its Send returns, before the
# agent has applied it.
GATE_WITHIN=30
start=$SECONDS
until port_open "$NAME4" 8081; do
	if [ $((SECONDS - start)) -gt "$GATE_WITHIN" ]; then
		# A SetReady already out means SET_READY_AFTER was too short for the
		# harness, not that the agent failed (see the arithmetic there).
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

# The other arm: a withdrawal already out when the gate opened would let the
# check below pass without observing the close.
withdrawn4="$(count_events set_ready_sent "$EVENTS4")"
if [ "$withdrawn4" -ne 0 ]; then
	echo "the SetReady went out before the gate could be probed open; raise SET_READY_AFTER" >&2
	echo "the close below would then be asserted against a gate that had already shut, and this phase would pass having never observed the close it reports" >&2
	jq -rs '.' <"$EVENTS4" >&2
	exit 1
fi

# internal/agent/registry.go discards reports where players exceed slots, so
# zero slots would drop every count.
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

# BackendPlayers is the only signal of a player still in the configuration
# phase (Velocity calls addPlayer only from BackendPlaySessionHandler.activated()),
# and the drain rests on it. It is sent every tick, so an empty map must arrive.
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

echo "waiting for the operator to withdraw the proxy's readiness..."
await_event set_ready_sent "$EVENTS4" "$NAME4"

# Retried for the same reason as the open probe.
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

# 25565 must still answer: port_open reads a dead container as closed, and a
# withdrawn proxy must keep serving the players already on it.
if ! port_open "$NAME4" 25565; then
	echo "the proxy's own listener on 25565 went down with the ready gate" >&2
	echo "withdrawing readiness must take this pod out of the Service's endpoints, not out of service: the players already on it have to keep playing" >&2
	"$CONTAINER" logs "$NAME4" 2>&1 | tail -40 >&2
	exit 1
fi
echo "the ready gate closed ${closed_after}s after the operator withdrew readiness, and 25565 still answers"

# No player joins in this harness, so this proves only that the real jar sends
# a PlayerRoster the operator parses.
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
# The same measurement as phase two, on a different rpc and classloader, with a
# server list reapplied on every reconnect.
echo
echo "restarting the proxy against a superseding operator..."
"$CONTAINER" rm -f "$NAME4" >/dev/null 2>&1 || true
kill "$STUB4_PID" 2>/dev/null || true
STUB4_PID=""

# --proxy with no hold, so every stream is opened, answered and synced.
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

# The same two-sided bound as phase two.
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

# Without this, deleting --proxy would pass identically.
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

# Nothing here may close the gate (no SetReady, no shutdown), so a closed gate
# means renewals flap the pod's readiness.
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
# --rotate-ca mounts a two-CA bundle built with internal/certs and serves a
# certificate signed by the second.
echo
echo "restarting the agent against a stub mid CA rotation..."
"$CONTAINER" rm -f "$NAME5" >/dev/null 2>&1 || true
kill "$STUB5_PID" 2>/dev/null || true
STUB5_PID=""

# renew-after past the phase: it asserts on the handshake alone.
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

# A trustManager reading only the first PEM never completes this handshake.
echo "waiting up to ${DEADLINE}s for the agent to greet a server signed by the second CA..."
await_event hello "$EVENTS6" "$NAME6"
echo "the agent completed a handshake against a certificate signed by the CA that was second in its mounted bundle"

await_event ready "$EVENTS6" "$NAME6"
echo "the agent completed a session through it"

# ---------------------------------------------------------------------------
# Phase seven: a connection that is up and going nowhere.
#
# The stub stops reading and writing without closing anything (see `deafness`
# in cmd/spawnery-stubop/deafen.go). Before that, the connection count proves
# the stub's KeepaliveEnforcementPolicy (the operator's) accepts the agent's 45s
# pings; after it, only that keepalive can end the wait. About two minutes,
# because it tests the production timer rather than a test-only knob.
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
