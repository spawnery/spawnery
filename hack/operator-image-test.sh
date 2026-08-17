#!/usr/bin/env bash
# Smoke test for the operator image.
#
# It runs the image under exactly the constraints config/deploy/deployment.yaml
# imposes -- non-root, read-only root filesystem, no network -- rather than more
# comfortable ones. The operator writes nothing to disk, so unlike the two game
# images this one gets no tmpfs and no volume: if it ever needs one, this test
# is where that shows up, instead of in a CrashLoopBackOff on a real cluster.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# The image config, not the pod spec, is what resolves the identity: the
# Deployment sets runAsNonRoot with no runAsUser, so the kubelet refuses to
# start an image whose User is empty or names root.
user="$("$CONTAINER" image inspect --format '{{.Config.User}}' "$IMAGE")"
[ "$user" = "10001:10001" ] || fail "image user = '$user', want 10001:10001"

# The working directory is the root, not a game server's /data.
workdir="$("$CONTAINER" image inspect --format '{{.Config.WorkingDir}}' "$IMAGE")"
[ "$workdir" = "/" ] || fail "image workingDir = '$workdir', want /"

# /data belongs to a game server. An operator that acquired one would be
# carrying state nothing reads, and oci-common.layeredImage -- which creates it
# -- is deliberately not used here.
#
# Checked by exporting the filesystem rather than by running `test -d` inside
# the image: this image has no shell at all, so an in-container check would
# fail to start and its non-zero exit would read as "no /data" whether the
# directory was there or not. That assertion could never have failed.
cid="$("$CONTAINER" create "$IMAGE")"
# Captured into a variable, and matched against with a here-string rather than
# with `export | tar -t | grep -q`: grep -q exits the instant it finds a
# match, which sends SIGPIPE back up a live pipe from tar and podman export.
# Under `set -o pipefail` that SIGPIPE can itself become the pipeline's exit
# status, which reads as "no match" to the `if` below regardless of what grep
# found -- so the assertion would silently never fire. Capturing first with a
# command substitution (which always reads to EOF, no early exit) and then
# testing the already-complete string sidesteps that race entirely. Tar list
# entries come back as e.g. "data/", not "./data/", hence no leading "./" is
# required by the pattern.
entries="$("$CONTAINER" export "$cid" | tar -t)"
if grep -qE '^(\./)?data/?$' <<<"$entries"; then
	"$CONTAINER" rm "$cid" >/dev/null
	fail "the image has a /data directory; it should carry no writable directory of its own"
fi
"$CONTAINER" rm "$cid" >/dev/null

# The binary runs, statically, as uid 10001, with nothing writable and no
# network. Go's flag package prints usage and exits 0 for -h -- exit 2 is what
# it uses for a parse error -- which is the cheapest proof that the ELF loads
# and the flags are the ones the Deployment passes. The exit code is discarded
# either way; what is matched is the output.
out="$("$CONTAINER" run --rm --read-only --network none "$IMAGE" -h 2>&1 || true)"
for flag in startup-deadline leader-elect metrics-bind-address health-probe-bind-address; do
	case "$out" in
	*"-$flag"*) ;;
	*) fail "the operator's usage does not mention -$flag; the Deployment passes it" ;;
	esac
done

echo "OK: $IMAGE"
