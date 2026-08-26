#!/bin/sh
# Entrypoint of the Spawnery Velocity base image.
#
# It renders configuration and starts the proxy. Everything it used to be
# tempted to validate lives in spawnery-config, which has the values and can
# name the key that is missing.
set -eu

VELOCITY_HOME="${VELOCITY_HOME:-/opt/velocity}"

# The configuration Velocity actually reads, written from the operator's
# rendered ConfigMap, the user's overlay and the fields neither may move.
# Invoked unqualified, the same way java is below and the same way
# image/entrypoint.sh already invokes it — /usr/local/bin is already ahead on
# this image's PATH, and going through PATH rather than a hardcoded path is
# what lets a test double stand in for it in image/entrypoint_test.go.
spawnery-config --flavor velocity

# The agent plugin. It ships in the read-only part of the image and is copied
# out on every start, unconditionally: the image is the truth, not whatever a
# previous start left in the volume. The copy predates the jar -- it was written
# in 3b so that the image contract would not change under 3c, which is what put
# a jar here. The guard therefore stays: it is what lets the entrypoint be
# tested, and run, against an image built without an agent.
if [ -f "$VELOCITY_HOME/agent/spawnery-agent.jar" ]; then
	mkdir -p plugins
	cp -f "$VELOCITY_HOME/agent/spawnery-agent.jar" plugins/spawnery-agent.jar
fi

# exec, so the JVM becomes PID 1 and receives SIGTERM directly. With a shell in
# between, a proxy would never get its signal and would drop every player on it
# instead of draining.
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
	-XX:+ParallelRefProcEnabled \
	-XX:MaxGCPauseMillis=200 \
	-XX:+UnlockExperimentalVMOptions \
	-XX:+DisableExplicitGC \
	${PRETOUCH:+$PRETOUCH} \
	-jar "$VELOCITY_HOME/velocity.jar"
