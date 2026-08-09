#!/bin/sh
# Entrypoint of the Spawnery Paper base image.
#
# It does the least that lets Paper start at all, plus the three fields the
# operator has to be able to rely on. Everything else stays Paper's default:
# there is no configuration rendering yet. /data/config is not an override
# slot — it is Paper's own writable directory, where Paper itself writes
# paper-global.yml and paper-world-defaults.yml at startup; see
# docs/known-issues.md for the collision this creates with a read-only mount
# there. See section 6 of
# docs/superpowers/specs/2026-08-08-paper-base-image-design.md.
set -eu

# max-players is not cosmetic: from milestone 2c the agent reports it to the
# operator as slots, and the operator scales on that number. Starting with
# Paper's default of 20 while the group says 100 would make the operator plan
# against capacity the server can never honour, so refusing is better than
# guessing.
if [ -z "${SPAWNERY_MAX_PLAYERS:-}" ]; then
	echo "spawnery-entrypoint: SPAWNERY_MAX_PLAYERS is not set" >&2
	exit 1
fi
case "$SPAWNERY_MAX_PLAYERS" in
*[!0-9]*)
	echo "spawnery-entrypoint: SPAWNERY_MAX_PLAYERS is not a number: $SPAWNERY_MAX_PLAYERS" >&2
	exit 1
	;;
esac

PAPER_HOME="${PAPER_HOME:-/opt/paper}"

# Mojang's EULA. Running this image is accepting it, and the README says so
# rather than leaving it buried here.
printf 'eula=true\n' >eula.txt

[ -f server.properties ] || : >server.properties

# Rewrite one key, keeping every other line exactly as it was found. Dropping
# the old occurrence first is what keeps the file from growing a second
# max-players on every restart.
set_property() {
	grep -v "^$1=" server.properties >server.properties.tmp || true
	printf '%s=%s\n' "$1" "$2" >>server.properties.tmp
	mv server.properties.tmp server.properties
}

# The three the operator relies on. The port is obvious. max-players is
# explained above. enable-status would be the quietest failure of the three:
# switched off, the server answers no server list ping, the readiness probe
# stays red forever, and nothing in the log says why.
set_property server-port 25565
set_property max-players "$SPAWNERY_MAX_PLAYERS"
set_property enable-status true

# The agent plugin. It ships in the read-only part of the image and is copied
# out on every start, unconditionally: the image is the truth, not whatever a
# previous start left in the volume.
#
# It cannot simply be loaded from where it ships. Paper writes its plugins'
# data folders inside the plugins directory - measured in milestone 2b, which
# saw plugins/spark/config.json and plugins/bStats/config.yml appear on a plain
# run - so pointing --plugins at a read-only directory takes Paper's own
# bundled plugins down with it.
#
# A read-only mount at /data/plugins therefore breaks the start here, with a
# bare cp error. Mounts below /data are allowed by internal/podspec, so this is
# reachable; see docs/known-issues.md.
if [ -f "$PAPER_HOME/agent/spawnery-agent.jar" ]; then
	mkdir -p plugins
	cp -f "$PAPER_HOME/agent/spawnery-agent.jar" plugins/spawnery-agent.jar
	chmod u+w plugins/spawnery-agent.jar
fi

# exec, so the JVM becomes PID 1 and receives SIGTERM directly. With a shell in
# between, the group's termination grace period would run out empty and every
# server would lose its last world state on every stop.
#
# MaxRAMPercentage rather than a fixed -Xmx: the memory bound comes from the
# group's resources, and the image does not know it. The remaining flags are
# the ones Paper itself recommends.
exec java \
	-XX:MaxRAMPercentage=75 \
	-XX:+UseG1GC \
	-XX:+AlwaysPreTouch \
	-XX:+ParallelRefProcEnabled \
	-XX:+UnlockExperimentalVMOptions \
	-XX:+DisableExplicitGC \
	-XX:+PerfDisableSharedMem \
	-XX:MaxGCPauseMillis=200 \
	-XX:G1NewSizePercent=30 \
	-XX:G1MaxNewSizePercent=40 \
	-XX:G1HeapRegionSize=8M \
	-XX:G1ReservePercent=20 \
	-XX:G1HeapWastePercent=5 \
	-XX:G1MixedGCCountTarget=4 \
	-XX:G1MixedGCLiveThresholdPercent=90 \
	-XX:G1RSetUpdatingPauseTimePercent=5 \
	-XX:InitiatingHeapOccupancyPercent=15 \
	-DbundlerRepoDir="$PAPER_HOME/repo" \
	-jar "$PAPER_HOME/paper.jar" --nogui
