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

# Plugins from the group's own volume, if it has one.
#
# The default is internal/podspec.PluginSourceMountPath. The operator mounts
# exactly there and passes nothing -- the variable is overridable only so the
# tests can point it at a temporary directory, which is the same seam PAPER_HOME
# already is. Creating /var/run/spawnery/plugins needs root, so without it this
# copy would have no test at all.
#
# The whole tree, not just *.jar. A plugin's configuration lives at
# plugins/<Name>/config.yml, and copying jars without it would leave every
# plugin at its defaults on an ephemeral group, whose /data is an emptyDir.
#
# The source wins on every start. A plugin that rewrote its own config at
# runtime loses that change here: on an ephemeral group it was going anyway,
# and this makes the persistent case predictable rather than accumulating.
#
# The trailing dot copies the directory's *contents*. Without it the tree lands
# at plugins/plugins.
#
# **This runs before the agent jar below, and the order is the bound.** That
# copy overwrites whatever landed here, so a spawnery-agent.jar on the volume
# cannot displace the one the operator shipped -- otherwise somebody pinning an
# older agent would leave the operator talking to a version it never published,
# with every object in the cluster saying the right thing.
# Files an administrator put on a volume, copied into the working directory.
#
# **The scan runs before the copy, and that is the whole safety property.**
# Three things write into /data on a start: spawnery-config above, this, and
# the plugin copy below. Refusing a source that carries a path one of the
# others owns makes their paths disjoint, so the order between them cannot
# decide the result -- rather than a rule about which runs first, which would
# make these line numbers load-bearing.
#
# lost+found and the two globs are the plugin copy's reasoning exactly; see
# the comment on PLUGIN_SOURCE below for the measurements behind both.
FILE_SOURCE="${SPAWNERY_FILE_SOURCE:-/var/run/spawnery/files}"
if [ -d "$FILE_SOURCE" ]; then
	# The directory extraPlugins owns, the directory Velocity itself owns,
	# and the renderer's own file. A proxy does not refuse the Paper files:
	# nothing writes them on a proxy, and refusing a path no owner claims
	# would be a rule with no reason.
	if [ -d "$FILE_SOURCE/plugins" ]; then
		echo "spawnery: spec.extraFiles carries plugins/, which spec.extraPlugins owns." >&2
		echo "spawnery: move those files to the extraPlugins claim. Refusing to start." >&2
		exit 1
	fi
	# lang/ belongs to Velocity itself: it migrates lang/messages.properties
	# to MiniMessage on every start and writes the result back, so a file
	# placed there is overwritten before anybody reads it. Nothing breaks,
	# which is exactly why it is refused -- a copy that silently does nothing
	# is the failure this scan exists to prevent.
	if [ -d "$FILE_SOURCE/lang" ]; then
		echo "spawnery: spec.extraFiles carries lang/, which Velocity owns -- it migrates" >&2
		echo "spawnery: lang/messages.properties on every start and writes it back, so a" >&2
		echo "spawnery: file placed there is overwritten unread. Refusing to start." >&2
		exit 1
	fi
	for owned in velocity.toml; do
		if [ -e "$FILE_SOURCE/$owned" ]; then
			echo "spawnery: spec.extraFiles carries $owned, which the operator writes itself." >&2
			echo "spawnery: use spec.configOverlay for it. Refusing to start." >&2
			exit 1
		fi
	done

	for entry in "$FILE_SOURCE"/* "$FILE_SOURCE"/.[!.]*; do
		[ -e "$entry" ] || continue
		name="${entry##*/}"
		case "$name" in
		lost+found) continue ;;
		esac
		cp -R "$entry" ./
		# The whole of "./$name", not only what this loop just placed there --
		# if $name is a directory that already existed, that recurses over
		# whatever was already there too. That is fine rather than merely
		# tolerated: anything already there arrived as this same non-root
		# user, so it is already writable and the recursion is a no-op on it.
		#
		# Not `chmod -R u+w .`, though: this script runs under `set -eu`,
		# every user mount is read-only, and a group with a claim mount
		# somewhere else under /data would die on that wider chmod with a
		# bare `chmod:` naming no cause. The mount this copies from is
		# read-only too, so the copies arrive read-only and the files it
		# carries are exactly the kind a server rewrites on its own.
		chmod -R u+w "./$name"
	done
fi

PLUGIN_SOURCE="${SPAWNERY_PLUGIN_SOURCE:-/var/run/spawnery/plugins}"
if [ -d "$PLUGIN_SOURCE" ]; then
	mkdir -p plugins
	# cp -R and not cp -a, and lost+found skipped by name. Both were measured
	# on a live Longhorn claim on 2026-08-29, and either one alone kills the
	# start under `set -eu`:
	#
	#   cp: can't preserve ownership of '.../lost+found': Operation not permitted
	#   cp: can't preserve ownership of '.../.': Operation not permitted
	#
	# `-a` implies --preserve=all, and this container is not root, so it cannot
	# set an owner on anything -- not even on the destination directory. `-R`
	# copies the tree and leaves ownership to the process, which is what a
	# non-root container can actually do. chmod below then makes it writable.
	#
	# lost+found is created by mkfs on every ext4 filesystem and is mode 0700
	# owned by root, so a non-root copy cannot read it at all. Longhorn formats
	# ext4 by default, which makes this the ordinary case rather than an exotic
	# one. It is never a plugin, so skipping it by name loses nothing.
	for entry in "$PLUGIN_SOURCE"/* "$PLUGIN_SOURCE"/.[!.]*; do
		[ -e "$entry" ] || continue
		case "${entry##*/}" in
		lost+found) continue ;;
		esac
		cp -R "$entry" plugins/
	done
	# The mount is read-only, so the copies arrive read-only too. Paper writes
	# its plugins' data folders inside this directory, and a plugin that cannot
	# rewrite its own config file fails in its own way rather than in one the
	# server reports.
	chmod -R u+w plugins
fi

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
