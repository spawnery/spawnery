#!/bin/sh
# Entrypoint of the Spawnery Paper base image.
#
# It renders configuration and starts the server. Everything it used to
# validate or rewrite server.properties by hand — max-players, the port, the
# status flag — lives in spawnery-config now, which has the values and can
# name the key that is missing; see docs/known-issues.md for why the shell
# version it replaces did not generalise. /data/config is not an override
# slot — it is Paper's own writable directory, where Paper itself writes
# paper-global.yml and paper-world-defaults.yml at startup, which is why the
# operator's own target was moved to /etc/spawnery rather than fought over.
# (The plugins directory below could not be moved the same way, so
# internal/podspec refuses a user mount at it instead.) See section 6 of
# docs/superpowers/specs/2026-08-08-paper-base-image-design.md.
set -eu

PAPER_HOME="${PAPER_HOME:-/opt/paper}"

# Mojang's EULA. Running this image is accepting it, and the README says so
# rather than leaving it buried here.
printf 'eula=true\n' >eula.txt

# The configuration Paper actually reads, written from the operator's
# rendered ConfigMap, the user's overlay and the fields neither may move. It
# replaces the three set_property calls this script used to make: a
# .properties helper in shell could not reach paper-global.yml, which is
# YAML, and it failed on a read-only file with a bare mv message that said
# nothing about why. Invoked unqualified, the same way java is below —
# /usr/local/bin is already ahead on this image's PATH, and going through
# PATH rather than a hardcoded path is what lets a test double stand in for
# it below.
spawnery-config --flavor paper

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
fi

# exec, so the JVM becomes PID 1 and receives SIGTERM directly. With a shell in
# between, the group's termination grace period would run out empty and every
# server would lose its last world state on every stop.
#
# MaxRAMPercentage rather than a fixed -Xmx: the memory bound comes from the
# group's resources, and the image does not know it. The remaining flags are
# the ones Paper itself recommends.
# AlwaysPreTouch is dropped when nothing bounds this container's memory.
#
# The flag makes the JVM claim its whole heap from the operating system at
# start rather than growing into it. Paired with MaxRAMPercentage that is a
# good trade *inside a limit*: the share is the container's, and touching it
# up front costs a slower start and buys stable latency afterwards. With no
# limit the share is the node's, so a single server with no
# resources.limits.memory takes three quarters of the machine the instant it
# starts -- from every other pod on it, before it has served anybody.
#
# Read from the kernel rather than from the pod spec, because this script has
# no access to the pod spec and the kernel is where the answer actually is.
# cgroup v2 writes the literal "max" when unbounded; v1 writes a sentinel so
# large it is indistinguishable from unbounded in practice, and the comparison
# below treats anything at or above 2^60 as unbounded rather than trying to
# name the exact sentinel, which differs by kernel and by page size.
#
# An unreadable cgroup is treated as *limited*, which is the direction that
# changes nothing: the flags stay exactly as they were before this check
# existed, so a kernel layout nobody here anticipated cannot turn a working
# start into a different one. It is only the case this can positively identify
# -- an unbounded limit, read from a file that exists -- that drops the flag.
# That is also why the host running this image's tests needs no cgroup of any
# particular shape: it lands on the unreadable branch and nothing changes.
#
# SPAWNERY_CGROUP_ROOT exists for those tests and for nothing else. Both
# branches below are unreachable from a test host otherwise -- the root cgroup
# has no memory.max, and no test may write under /sys -- so without it the one
# case this check exists for would ship unexercised.
CGROUP_ROOT="${SPAWNERY_CGROUP_ROOT:-/sys/fs/cgroup}"
PRETOUCH="-XX:+AlwaysPreTouch"
memory_unbounded() {
	if [ -r "$CGROUP_ROOT/memory.max" ]; then
		[ "$(cat "$CGROUP_ROOT/memory.max")" = "max" ]
		return
	fi
	if [ -r "$CGROUP_ROOT/memory/memory.limit_in_bytes" ]; then
		limit="$(cat "$CGROUP_ROOT/memory/memory.limit_in_bytes")"
		case "$limit" in
		'' | *[!0-9]*) return 1 ;;
		esac
		[ "$limit" -ge 1152921504606846976 ]
		return
	fi
	return 1
}
if memory_unbounded; then
	PRETOUCH=""
	echo "spawnery: no memory limit on this container, so the JVM would size itself against the whole node." >&2
	echo "spawnery: starting without AlwaysPreTouch. Set resources.limits.memory on the group." >&2
fi

exec java \
	-XX:MaxRAMPercentage=75 \
	-XX:+UseG1GC \
	${PRETOUCH:+$PRETOUCH} \
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
