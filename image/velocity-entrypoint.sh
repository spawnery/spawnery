#!/bin/sh
# Entrypoint of the Spawnery Velocity base image: renders configuration and
# starts the proxy.
set -eu

# SPAWNERY_-prefixed like every variable this script reads; see
# image/entrypoint.sh.
VELOCITY_HOME="${SPAWNERY_VELOCITY_HOME:-/opt/velocity}"

spawnery-config --flavor velocity

# mountinfo writes a space in a path as \040, which printf %b turns back.
MOUNTINFO="${SPAWNERY_MOUNTINFO:-/proc/self/mountinfo}"
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

# The scan runs before the copy. Refusing a source that carries a path another
# writer owns keeps the writers' paths disjoint, so the order between them
# cannot decide the result. lost+found: see the plugin copy below.
FILE_SOURCE="${SPAWNERY_FILE_SOURCE:-/var/run/spawnery/files}"
if [ -d "$FILE_SOURCE" ]; then
	# -e and not -d: a regular file named plugins would still break the
	# `mkdir -p plugins` below, with a message naming neither claim nor field.
	if [ -e "$FILE_SOURCE/plugins" ]; then
		echo "spawnery: spec.extraFiles carries plugins/, which spec.extraPlugins owns." >&2
		echo "spawnery: move those files to the extraPlugins claim. Refusing to start." >&2
		exit 1
	fi
	# Velocity overwrites lang/ on every start, so a copy there silently does
	# nothing; that is why it is refused.
	if [ -e "$FILE_SOURCE/lang" ]; then
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

	refuse_mounted "$FILE_SOURCE" . extraFiles || exit 1

	for entry in "$FILE_SOURCE"/* "$FILE_SOURCE"/.[!.]*; do
		[ -e "$entry" ] || continue
		name="${entry##*/}"
		case "$name" in
		lost+found) continue ;;
		esac
		cp -R "$entry" ./
		# The source mount is read-only, so the copies arrive read-only. Not
		# `chmod -R u+w .`: a read-only claim mount elsewhere under /data would
		# kill the start under `set -eu`.
		find "./$name" -xdev -exec chmod u+w {} +
	done
fi

# The whole tree, not just *.jar: plugin configuration lives in
# plugins/<Name>/. This runs before the agent copy, which overwrites any
# spawnery-agent.jar the volume carries.
PLUGIN_SOURCE="${SPAWNERY_PLUGIN_SOURCE:-/var/run/spawnery/plugins}"
if [ -d "$PLUGIN_SOURCE" ]; then
	refuse_mounted "$PLUGIN_SOURCE" plugins extraPlugins || exit 1
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

if [ -f "$VELOCITY_HOME/agent/spawnery-agent.jar" ]; then
	mkdir -p plugins
	cp -f "$VELOCITY_HOME/agent/spawnery-agent.jar" plugins/spawnery-agent.jar
fi

# After the agent copy, so the agent jar is never touched; before the JVM, so
# a missing secret stops this start instead of a plugin later.
if [ -n "${SPAWNERY_SUBSTITUTION_PREFIX:-}" ]; then
	spawnery-config --substitute "$SPAWNERY_SUBSTITUTION_PREFIX" \
		--pair "$PLUGIN_SOURCE=plugins" --pair "$FILE_SOURCE=." || exit 1
fi

# AlwaysPreTouch is dropped without a memory limit; image/entrypoint.sh says
# why and how the limit is read.
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
