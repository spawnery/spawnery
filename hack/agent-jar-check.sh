#!/usr/bin/env bash
# Checks that the agent jar's dependencies were relocated.
#
# Paper carries protobuf-java 4.29.0 and netty 4.2.15 on its own classpath (see
# <paper-repo>/libraries). A plugin that ships an unrelocated com/google/protobuf
# meets Paper's copy at class load, and the symptom is a NoSuchMethodError deep
# inside gRPC that names neither the plugin nor the conflict. This check is what
# keeps that from being discovered in a pod.
set -euo pipefail

JAR="${1:?usage: agent-jar-check.sh <jar>}"

entries="$(unzip -Z1 "$JAR")"

fail() {
	echo "agent-jar-check: $1" >&2
	exit 1
}

# Relocated packages must be present under the prefix...
grep -q '^cloud/spawnery/agent/shaded/com/google/protobuf/' <<<"$entries" ||
	fail "protobuf was not relocated under cloud/spawnery/agent/shaded/"
grep -q '^cloud/spawnery/agent/shaded/io/grpc/' <<<"$entries" ||
	fail "grpc was not relocated under cloud/spawnery/agent/shaded/"

# ...and absent at their original coordinates.
for pkg in com/google/protobuf io/grpc com/google/common okio com/squareup/okhttp3 io/perfmark; do
	if grep -q "^$pkg/" <<<"$entries"; then
		fail "$pkg is present unrelocated; it would meet Paper's own copy"
	fi
done

# gRPC resolves its transport through ServiceLoader. Relocation renames the
# provider classes, so the service files have to be merged and rewritten with
# them; without that the channel fails at runtime with "no functional channel
# service provider found" and nothing points at the shading as the cause.
grep -q '^META-INF/services/cloud.spawnery.agent.shaded.io.grpc.ManagedChannelProvider$' <<<"$entries" ||
	fail "the relocated ManagedChannelProvider service file is missing"

# The plugin descriptor is what makes this a plugin at all.
grep -q '^paper-plugin.yml$' <<<"$entries" ||
	fail "paper-plugin.yml is missing from the jar"

echo "agent-jar-check: ok"
