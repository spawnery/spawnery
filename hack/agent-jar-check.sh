#!/usr/bin/env bash
# Checks that the agent jar is the jar the plugin needs: everything relocated,
# the stubs present, and the descriptor expanded.
#
# Paper carries its own protobuf-java 4.29.0, guava 33.6.0, gson 2.14.0 and
# netty 4.2.15 (see <paper-repo>/libraries). A plugin that ships any of those at
# their original coordinates meets Paper's copy at class load, and the symptom
# is a NoSuchMethodError deep inside gRPC that names neither the plugin nor the
# conflict. This check is what keeps that from being discovered in a pod.
#
# The relocation is checked as an invariant and not as a list. A list of
# packages has to be revisited every time a dependency changes, and the version
# of this file that had one fell six packages behind without anything noticing,
# three of them Paper's. So the first check below fails on *any* class outside
# cloud/spawnery/agent/, named or not; the enumeration that follows exists only
# to say which of them Paper would actually collide with.
set -euo pipefail

JAR="${1:?usage: agent-jar-check.sh <jar> [source-dir [flavour]]}"
# Optional: the Gradle root of agent/, not one subproject. Given one, the
# no-Java constraint below is checked too. nix/agents.nix passes it; a hand
# invocation on a store path has no source to point at and says so rather than
# passing silently.
SRC="${2:-}"
# Which agent this jar is. Defaults to paper so the two-argument invocation
# keeps working; every flavour-specific fact is looked up from here rather than
# spelled inline, so a second agent adds a case and changes nothing else.
FLAVOUR="${3:-paper}"

case "$FLAVOUR" in
paper)
	# The source directories this jar is built from. Listed rather than
	# discovered: the whole value of the no-Java check below is that a
	# directory which has moved or vanished is a failure, and a `find` over
	# whatever happens to be present cannot tell "moved" from "empty".
	# :common is here for every flavour -- both agents compile its sources
	# into their jar. :paper has no test sources yet (its first arrives with
	# ServerRole), so paper/src/test is deliberately absent from this list
	# rather than probed for; add the line when the directory appears.
	SRC_DIRS="common/src/main common/src/test paper/src/main"
	DESCRIPTOR="paper-plugin.yml"
	# The line the descriptor states its version on. Read rather than merely
	# counted, see below.
	DESCRIPTOR_VERSION='^version:'
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

# Every class in the jar is the plugin's own or one of its relocated
# dependencies. Nothing else may be in it at all.
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
	fail "every class the plugin ships must be under cloud/spawnery/agent/ -- add the package to the relocate list in agent/$FLAVOUR/build.gradle.kts"
fi

# Named separately because for these the consequence is a linkage error rather
# than a broken invariant: Paper has its own copy on the classpath that loads
# the plugin. Guava contributes two top-level packages, and missing the second
# is exactly how the list above went stale.
for pkg in com/google/protobuf com/google/common com/google/thirdparty com/google/gson \
	com/google/errorprone com/google/j2objc io/netty; do
	if grep -q "^$pkg/" <<<"$entries"; then
		fail "$pkg is present unrelocated; it would meet Paper's own copy"
	fi
done

# Relocated packages must also be present under the prefix -- the checks above
# pass just as well for a jar that lost the dependency altogether.
grep -q '^cloud/spawnery/agent/shaded/com/google/protobuf/' <<<"$entries" ||
	fail "protobuf was not relocated under cloud/spawnery/agent/shaded/"
grep -q '^cloud/spawnery/agent/shaded/io/grpc/' <<<"$entries" ||
	fail "grpc was not relocated under cloud/spawnery/agent/shaded/"
grep -q '^cloud/spawnery/agent/shaded/kotlin/' <<<"$entries" ||
	fail "the Kotlin standard library was not relocated under cloud/spawnery/agent/shaded/"

# The generated stubs are compiled in :common and reach this jar because
# shadowJar bundles the project dependency on it. A :paper that stopped
# depending on :common, or a :common whose src/proto/java srcDir was dropped,
# produces a jar that installs, passes every check above, and cannot construct
# a single message.
grep -q '^cloud/spawnery/agent/pb/AgentServiceGrpc.class$' <<<"$entries" ||
	fail "the generated gRPC stubs are missing from the jar"

# gRPC resolves its transport through ServiceLoader. Relocation renames the
# provider classes, so the service files have to be merged and rewritten with
# them; without that the channel fails at runtime with "no functional channel
# service provider found" and nothing points at the shading as the cause.
grep -q '^META-INF/services/cloud.spawnery.agent.shaded.io.grpc.ManagedChannelProvider$' <<<"$entries" ||
	fail "the relocated ManagedChannelProvider service file is missing"

# The plugin descriptor is what makes this a plugin at all -- and its version is
# what the agent reports as Hello.version. processResources expands it from
# -PagentVersion; losing that expansion ships a literal ${version} and the
# operator records a server running "${version}". Presence alone does not see
# that, so the contents are read.
grep -q "^$DESCRIPTOR\$" <<<"$entries" ||
	fail "$DESCRIPTOR is missing from the jar"
descriptor="$(unzip -p "$JAR" "$DESCRIPTOR")"
grep -q "$DESCRIPTOR_VERSION" <<<"$descriptor" ||
	fail "$DESCRIPTOR carries no version"
if grep -q '\${' <<<"$descriptor"; then
	fail "$DESCRIPTOR still holds an unexpanded placeholder; processResources did not expand it"
fi

# The agents are Kotlin, and the generated Java is confined to
# common/src/proto/java, which is the one directory that compiles with javac and
# with no platform jar anywhere near it (see agent/common/build.gradle.kts). A
# .java under any src/main or src/test would break that in a way that is silent
# both ways: under src/main/java it compiles against the class-file-major-69
# Paper jars and only fails if it resolves a class out of one, and under
# src/main/kotlin kotlinc reads it for resolution and never emits it.
if [ -n "$SRC" ]; then
	# Without this the check passes silently when the directories are not
	# there at all - a moved sourceRoot, a changed `src` in nix/agents.nix,
	# or an invocation from the wrong directory would each read as "no stray
	# Java" rather than "nothing was looked at". SRC is now the Gradle root,
	# so every path is <project>/src/<set>.
	dirs=()
	for dir in $SRC_DIRS; do
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
