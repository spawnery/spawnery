#!/bin/sh
# Entrypoint of the Spawnery Paper base image: renders configuration and
# starts the server.
set -eu

# Every variable read from the environment is SPAWNERY_-prefixed, a prefix
# spec.env cannot set; image/reserved_env_test.go fails on an unprefixed one.
PAPER_HOME="${SPAWNERY_PAPER_HOME:-/opt/paper}"

# Set by the Purpur image, which shares this script.
SERVER_JAR="${SPAWNERY_SERVER_JAR:-$PAPER_HOME/paper.jar}"

MOUNTINFO="${SPAWNERY_MOUNTINFO:-/proc/self/mountinfo}"
FILE_SOURCE="${SPAWNERY_FILE_SOURCE:-/var/run/spawnery/files}"
PLUGIN_SOURCE="${SPAWNERY_PLUGIN_SOURCE:-/var/run/spawnery/plugins}"

# mountinfo writes a space in a path as \040, which printf %b turns back.
readonly_mounts_below_here() {
	[ -r "$MOUNTINFO" ] || return 0
	here=$(pwd -P)
	while read -r _ _ _ _ point options _; do
		case "$options" in
		ro | ro,*) ;;
		*) continue ;;
		esac
		point=$(printf '%b' "$point")
		case "$point" in
		"$here"/*) printf '%s\n' "${point#"$here"/}" ;;
		esac
	done <"$MOUNTINFO"
}

# refuse_mounted SOURCE DEST FIELD refuses the start when SOURCE carries a path
# that a read-only mount already holds under DEST.
refuse_mounted() {
	readonly_mounts_below_here | while IFS= read -r mounted; do
		case "$2" in
		.) carried=$mounted ;;
		*)
			case "$mounted" in
			"$2"/*) carried=${mounted#"$2"/} ;;
			*) continue ;;
			esac
			;;
		esac
		if [ -e "$1/$carried" ] || [ -L "$1/$carried" ]; then
			echo "spawnery: spec.$3 carries $carried, and a spec.mounts entry holds $(pwd -P)/$mounted read-only." >&2
			echo "spawnery: point the mount and the claim at different paths. Refusing to start." >&2
			exit 1
		fi
	done
}

# The scans run before the prune and before any writer. Refusing a source
# that carries a path another writer owns keeps the writers' paths disjoint,
# so the order between them cannot decide the result.
if [ -d "$FILE_SOURCE" ]; then
	# -e and not -d: a regular file named plugins would still break the
	# `mkdir -p plugins` below, with a message naming neither claim nor field.
	if [ -e "$FILE_SOURCE/plugins" ]; then
		echo "spawnery: spec.extraFiles carries plugins/, which spec.extraPlugins owns." >&2
		echo "spawnery: move those files to the extraPlugins claim. Refusing to start." >&2
		exit 1
	fi
	# Outside the loop below, which image/refusal_lists_test.go holds to
	# render.PaperFiles: eula.txt is the image's, not the renderer's.
	if [ -e "$FILE_SOURCE/eula.txt" ]; then
		echo "spawnery: spec.extraFiles carries eula.txt, which this image writes itself." >&2
		echo "spawnery: running the image is accepting the EULA; drop the file. Refusing to start." >&2
		exit 1
	fi
	for owned in server.properties config/paper-global.yml config/paper-world-defaults.yml; do
		if [ -e "$FILE_SOURCE/$owned" ]; then
			echo "spawnery: spec.extraFiles carries $owned, which the operator writes itself." >&2
			echo "spawnery: use spec.configOverlay for it. Refusing to start." >&2
			exit 1
		fi
	done

	refuse_mounted "$FILE_SOURCE" . extraFiles || exit 1
fi
if [ -d "$PLUGIN_SOURCE" ]; then
	refuse_mounted "$PLUGIN_SOURCE" plugins extraPlugins || exit 1
fi

# spec.storage.keep. It cannot run at stop: the JVM is PID 1 and a hard kill
# runs no hook.
if [ -n "${SPAWNERY_KEEP:-}" ]; then
	spawnery-config --prune "$SPAWNERY_KEEP" --mountinfo "$MOUNTINFO" \
		--pair "$FILE_SOURCE=." --pair "$PLUGIN_SOURCE=plugins" || exit 1
fi

printf 'eula=true\n' >eula.txt

spawnery-config --flavor paper

# lost+found: see the plugin copy below.
if [ -d "$FILE_SOURCE" ]; then
	for entry in "$FILE_SOURCE"/* "$FILE_SOURCE"/.[!.]*; do
		[ -e "$entry" ] || continue
		name="${entry##*/}"
		case "$name" in
		lost+found) continue ;;
		esac
		cp -R "$entry" ./
		# The source mount is read-only, so the copies arrive read-only, and
		# servers rewrite these files. Not `chmod -R u+w .`: a read-only claim
		# mount elsewhere under /data would kill the start under `set -eu`.
		find "./$name" -xdev -exec chmod u+w {} +
	done
fi

# The whole tree, not just *.jar: plugin configuration lives in
# plugins/<Name>/. This runs before the agent copy, which overwrites any
# spawnery-agent.jar the volume carries.
if [ -d "$PLUGIN_SOURCE" ]; then
	mkdir -p plugins
	# cp -R and not cp -a: a non-root container cannot preserve ownership, and
	# -a fails on it. lost+found (root, 0700, on every ext4 claim) is
	# unreadable to this user and never a plugin.
	for entry in "$PLUGIN_SOURCE"/* "$PLUGIN_SOURCE"/.[!.]*; do
		[ -e "$entry" ] || continue
		case "${entry##*/}" in
		lost+found) continue ;;
		esac
		cp -R "$entry" plugins/
	done
	# Read-only copies of a read-only mount; plugins rewrite their own configs.
	find plugins -xdev -exec chmod u+w {} +
fi

# Copied rather than loaded where it ships: Paper writes its plugins' data
# folders inside the plugins directory, so it cannot be read-only.
if [ -f "$PAPER_HOME/agent/spawnery-agent.jar" ]; then
	mkdir -p plugins
	cp -f "$PAPER_HOME/agent/spawnery-agent.jar" plugins/spawnery-agent.jar
fi

# After the agent copy, so the agent jar is never touched; before the JVM, so
# a missing secret stops this start instead of a plugin later.
if [ -n "${SPAWNERY_SUBSTITUTION_PREFIX:-}" ]; then
	spawnery-config --substitute "$SPAWNERY_SUBSTITUTION_PREFIX" \
		--pair "$PLUGIN_SOURCE=plugins" --pair "$FILE_SOURCE=." || exit 1
fi

# MaxRAMPercentage rather than -Xmx: the image does not know the group's
# memory limit. The remaining flags are the ones Paper recommends.
#
# AlwaysPreTouch claims the whole heap at start. Without a memory limit that
# is three quarters of the node, so the flag is dropped then. cgroup v1's
# "unbounded" sentinel differs by kernel and page size, hence anything at or
# above 2^60. An unreadable cgroup counts as limited, which changes nothing.
# SPAWNERY_CGROUP_ROOT exists only for the tests.
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
	-jar "$SERVER_JAR" --nogui
