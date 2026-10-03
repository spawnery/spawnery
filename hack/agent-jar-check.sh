#!/usr/bin/env bash
# Checks that the agent jar is the jar the plugin needs: everything relocated,
# the stubs present, and the descriptor expanded.
#
# Paper and Velocity carry their own copies of several of the agent's
# dependencies; an unrelocated one meets the platform's at class load as a
# NoSuchMethodError inside gRPC. Relocation is checked as an invariant (any
# class outside cloud/spawnery/agent/ fails), not a list. The per-flavour
# COLLIDES list only explains that failure.
set -euo pipefail

JAR="${1:?usage: agent-jar-check.sh <jar> [source-dir [flavour]]}"
# Optional: the Gradle root of agent/, not one subproject. Given one, the
# no-Java constraint below is checked too.
SRC="${2:-}"
FLAVOUR="${3:-paper}"

case "$FLAVOUR" in
paper)
	# Every hand-written source set, shipped or not. Listed rather than
	# discovered, so a moved or vanished directory fails.
	SRC_DIRS=(common/src/main common/src/test paper/src/main paper/src/test)
	DESCRIPTOR="paper-plugin.yml"
	DESCRIPTOR_VERSION='^version:'
	PLATFORM="Paper"
	# Out of <paper-repo>/libraries: protobuf-java, guava (two top-level
	# packages), gson, netty and guava's annotation-only artifacts.
	COLLIDES=(
		com/google/protobuf
		com/google/common
		com/google/thirdparty
		com/google/gson
		com/google/errorprone
		com/google/j2objc
		io/netty
	)
	;;
velocity)
	SRC_DIRS=(common/src/main common/src/test velocity/src/main velocity/src/test)
	DESCRIPTOR="velocity-plugin.json"
	DESCRIPTOR_VERSION='"version"[[:space:]]*:'
	PLATFORM="Velocity"
	# Read out of Velocity's fat jar. It carries no protobuf, gRPC,
	# okhttp/okio or Kotlin.
	COLLIDES=(
		com/google/common
		com/google/thirdparty
		com/google/gson
		com/google/errorprone
		com/google/j2objc
		com/google/inject
		io/netty
		org/apache/logging
		net/kyori
		com/mojang/brigadier
	)
	;;
*)
	echo "agent-jar-check: unknown flavour '$FLAVOUR'" >&2
	exit 1
	;;
esac

entries="$(unzip -Z1 "$JAR")"

fail() {
	echo "agent-jar-check: $1" >&2
	exit 1
}

stray="$(
	{
		grep '\.class$' <<<"$entries" |
			grep -v '^cloud/spawnery/agent/' |
			sed -e 's|/[^/]*\.class$||' |
			sort -u
	} || true
)"
if [ -n "$stray" ]; then
	echo "agent-jar-check: these packages ship unrelocated:" >&2
	sed -e 's|^|  |' <<<"$stray" >&2

	collides=()
	for pkg in "${COLLIDES[@]}"; do
		if grep -q "^$pkg/" <<<"$entries"; then
			collides+=("$pkg")
		fi
	done
	if [ "${#collides[@]}" -gt 0 ]; then
		echo "agent-jar-check: and $PLATFORM ships its own copy of these, so they fail at class load rather than merely bloating the jar:" >&2
		printf '  %s\n' "${collides[@]}" >&2
	fi

	fail "every class the plugin ships must be under cloud/spawnery/agent/ -- add the package to the relocate list in agent/$FLAVOUR/build.gradle.kts"
fi

# The check above passes just as well for a jar that lost a dependency.
grep -q '^cloud/spawnery/agent/shaded/com/google/protobuf/' <<<"$entries" ||
	fail "protobuf was not relocated under cloud/spawnery/agent/shaded/"
grep -q '^cloud/spawnery/agent/shaded/io/grpc/' <<<"$entries" ||
	fail "grpc was not relocated under cloud/spawnery/agent/shaded/"
grep -q '^cloud/spawnery/agent/shaded/kotlin/' <<<"$entries" ||
	fail "the Kotlin standard library was not relocated under cloud/spawnery/agent/shaded/"

# Without :common's stubs the jar installs and passes everything above.
grep -q '^cloud/spawnery/agent/pb/AgentServiceGrpc.class$' <<<"$entries" ||
	fail "the generated gRPC stubs are missing from the jar"

# SessionLoop.close(cancel = true) casts to this inside a runCatching, which
# would swallow its NoClassDefFoundError; no runtime test can see it missing.
grep -q '^cloud/spawnery/agent/shaded/io/grpc/stub/ClientCallStreamObserver.class$' <<<"$entries" ||
	fail "the relocated ClientCallStreamObserver is missing; the give-up path's cancel would fail silently"

# gRPC finds its transport through ServiceLoader, so the service files must be
# rewritten with the relocated provider names.
grep -q '^META-INF/services/cloud.spawnery.agent.shaded.io.grpc.ManagedChannelProvider$' <<<"$entries" ||
	fail "the relocated ManagedChannelProvider service file is missing"

# The descriptor's version is what the agent reports as Hello.version, so an
# unexpanded ${version} must fail here.
grep -q "^$DESCRIPTOR\$" <<<"$entries" ||
	fail "$DESCRIPTOR is missing from the jar"
descriptor="$(unzip -p "$JAR" "$DESCRIPTOR")"
grep -q "$DESCRIPTOR_VERSION" <<<"$descriptor" ||
	fail "$DESCRIPTOR carries no version"
if grep -q '\${' <<<"$descriptor"; then
	fail "$DESCRIPTOR still holds an unexpanded placeholder; processResources did not expand it"
fi

# Generated Java is confined to common/src/proto/java. A stray .java elsewhere
# fails silently: under src/main/java it compiles against the class-file-major-69
# Paper jars, under src/main/kotlin kotlinc reads it and never emits it.
if [ -n "$SRC" ]; then
	# With no paths at all, `find` would fall back to `.`.
	[ "${#SRC_DIRS[@]}" -gt 0 ] ||
		fail "flavour '$FLAVOUR' names no source directories, so the no-Java constraint would have checked nothing"
	dirs=()
	for dir in "${SRC_DIRS[@]}"; do
		[ -d "$SRC/$dir" ] || fail "$SRC/$dir does not exist, so the no-Java constraint checked nothing"
		dirs+=("$SRC/$dir")
	done
	strayjava="$(find "${dirs[@]}" -name '*.java' -print 2>/dev/null || true)"
	if [ -n "$strayjava" ]; then
		echo "agent-jar-check: these Java sources are outside the generated source directory:" >&2
		sed -e 's|^|  |' <<<"$strayjava" >&2
		fail "src/main and src/test hold Kotlin only; generated Java belongs in common/src/proto/java"
	fi
else
	echo "agent-jar-check: no source directory given, so the no-Java constraint was not checked"
fi

echo "agent-jar-check: ok"
