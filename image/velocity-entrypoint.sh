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
# previous start left in the volume. Milestone 3c is what puts a jar here; the
# copy is written now so the image contract does not change under 3c.
if [ -f "$VELOCITY_HOME/agent/spawnery-agent.jar" ]; then
	mkdir -p plugins
	cp -f "$VELOCITY_HOME/agent/spawnery-agent.jar" plugins/spawnery-agent.jar
fi

# exec, so the JVM becomes PID 1 and receives SIGTERM directly. With a shell in
# between, a proxy would never get its signal and would drop every player on it
# instead of draining.
exec java \
	-XX:MaxRAMPercentage=75 \
	-XX:+UseG1GC \
	-XX:+ParallelRefProcEnabled \
	-XX:MaxGCPauseMillis=200 \
	-XX:+UnlockExperimentalVMOptions \
	-XX:+DisableExplicitGC \
	-XX:+AlwaysPreTouch \
	-jar "$VELOCITY_HOME/velocity.jar"
