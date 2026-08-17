#!/usr/bin/env bash
# The driven end-to-end run: the operator inside a real cluster, under its own
# ServiceAccount.
#
# This script is plumbing only. It creates the cluster, gets the operator
# running in it, and hands over; every claim is made by the Go package in
# test/e2e. The split is the 2026-08-07 E2E design's §4, kept, and the reason is
# that a shell script asserting on cluster state produces failure messages
# nobody can act on.
#
# On this machine kind runs under rootless podman, which needs both an
# environment variable and a systemd scope. The script deliberately hard-codes
# neither:
#
#   systemd-run --scope --user --property=Delegate=yes -- \
#     nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
set -euo pipefail

CLUSTER="${CLUSTER:-spawnery-e2e}"
E2E_KEEP="${E2E_KEEP:-0}"
DEADLINE="${DEADLINE:-300}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

workdir="$(mktemp -d)"
KUBECONFIG="$workdir/kubeconfig"
export KUBECONFIG

# Set just before `kind create cluster` runs, and read by cleanup. Without
# it, any failure before that line -- a nix build that does not build, a
# gunzip that does not gunzip -- still ran `kind delete cluster --name
# spawnery-e2e` on the way out. That is destructive in exactly the situation a
# person is most likely to be in: they kept a cluster with E2E_KEEP=1, re-ran,
# hit an early error, and lost the cluster they were keeping to a run that
# never created one. The dump is empty in that window too, because $KUBECONFIG
# has not been written yet, so the run destroys the evidence and produces none.
created_cluster=0

dump() {
	echo "================ operator log ================"
	kubectl -n spawnery-system logs deployment/spawnery-operator --tail=-1 2>&1 || true
	echo "================ objects ================"
	kubectl get networks,servergroups,proxygroups,servers,pods,pvc -A 2>&1 || true
	echo "================ events ================"
	kubectl get events -A --sort-by=.lastTimestamp 2>&1 || true
}

cleanup() {
	local status=$?
	# Dumped only when there is something to dump from. Before the cluster
	# exists $KUBECONFIG has not been written, and every command in dump would
	# print its own connection error over whatever the real failure said.
	if [ "$status" -ne 0 ] && [ -s "$KUBECONFIG" ]; then
		dump
	fi
	if [ "$created_cluster" != "1" ]; then
		# Nothing of ours to tear down, and nothing of anyone else's to take
		# with us.
		rm -rf "$workdir"
	elif [ "$E2E_KEEP" = "1" ]; then
		echo "E2E_KEEP=1: cluster '$CLUSTER' left standing; KUBECONFIG=$KUBECONFIG"
	else
		kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
		rm -rf "$workdir"
	fi
	exit "$status"
}
trap cleanup EXIT

nix build .#operator-image --out-link result-operator
image="$(nix eval --raw '.#operator-image.imageName'):$(nix eval --raw '.#operator-image.imageTag')"

# dockerTools.buildLayeredImage emits a gzipped archive; `kind load
# image-archive` wants a plain tar. Decompress if it is compressed and copy if
# it is not, so this keeps working either way.
archive="$workdir/operator.tar"
if gunzip -t result-operator 2>/dev/null; then
	gunzip -c result-operator >"$archive"
else
	cp -L result-operator "$archive"
fi

# Set before the create, not after: a create that fails halfway leaves a
# partial cluster of this run's own making, and that one is ours to remove.
created_cluster=1
kind create cluster --name "$CLUSTER" --wait 120s
kind load image-archive "$archive" --name "$CLUSTER"

# config/rbac/role.yaml carries both a cluster-scoped ClusterRole and a
# namespace-scoped Role in spawnery-system (the operator's own Secret and
# Lease rights). Applying it before the namespace exists fails with
# "namespaces \"spawnery-system\" not found" -- a real failure a first run of
# this script hit, not a hypothetical. config/deploy/namespace.yaml is applied
# on its own first so the namespace is there for both role.yaml and the rest
# of config/deploy/, which kubectl would otherwise apply in the alphabetical
# file order of that directory (clusterrolebinding, deployment, namespace,
# ...) -- deployment.yaml before namespace.yaml, the same failure again.
kubectl apply -f config/crd/bases/
kubectl apply -f config/deploy/namespace.yaml
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/deploy/

# The per-namespace grant milestone 5c deliberately kept out of config/deploy/.
# The ClusterRole grants no access to secrets outside the operator's own
# namespace, so an administrator opens exactly the namespaces holding a
# Network -- and this run is the first thing in the repository that has to
# actually be that administrator. Without it the operator's read of the
# forwarding secret is refused and the Network reports
# Unknown/SecretReadForbidden for the whole run -- milestone 5c's own path,
# left permanently unresolved, on a denial that would be nobody's bug but this
# script's.
#
# Note what does NOT happen without it, because an earlier version of this
# comment claimed it did: test/e2e's denial check does not fire.
# readForwardingSecret (internal/controller/forwardingsecret.go) folds the 403
# into a condition whose message says "the operator may not read secret ..."
# and carries no `is forbidden:` substring, the read sits after the Accepted
# branch has already returned so no scenario fails either, and
# network_controller.go makes no logger call at all. That is a second and
# quite different way a denied read escapes the check -- not the cache, just
# an error the code handles instead of surfacing.
#
# The namespace is created here rather than by the test manifest so the grant
# can exist before the operator ever looks; applyManifest tolerates the
# namespace already being there.
kubectl create namespace minecraft
kubectl apply -n minecraft -f config/rbac/forwarding-secret-reader.yaml

# Three edits, none of which belongs in the manifest itself.
#
# The image: the run tests the bits just built, not whatever the registry
# happens to hold. imagePullPolicy Never makes that a guarantee rather than a
# hope -- with it, a missing local image fails loudly instead of being fetched.
#
# The deadline: config/deploy/deployment.yaml carries the production five
# minutes, which is longer than this run. Appending a second occurrence rather
# than rewriting the list means the manifest stays the single place the flags
# are written; Go's flag package resolves a repeated flag to the last one, and
# scenario 6 is what proves it did.
kubectl -n spawnery-system patch deployment spawnery-operator --type=json -p "$(
	cat <<EOF
[
  {"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "${image}"},
  {"op": "add", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"},
  {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--startup-deadline=20s"}
]
EOF
)"

kubectl -n spawnery-system rollout status deployment/spawnery-operator --timeout="${DEADLINE}s"

go test -tags e2e -count=1 -v -timeout 20m ./test/e2e/...
