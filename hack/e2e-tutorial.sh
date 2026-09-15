#!/usr/bin/env bash
# The tutorial's own path, once, with a real join.
#
# hack/e2e.sh's own manifest (test/e2e/manifests/e2e.yaml) deliberately names
# images that never resolve, so no game or proxy process ever starts there and
# a join is out of reach -- see that script's header. This one builds and
# loads the real Purpur and Velocity images, the ones docs/tutorial/network.yaml
# actually names, and drives TestTutorialPath, which hack/e2e.sh's own run
# skips (SPAWNERY_E2E_TUTORIAL is unset there).
#
# Not part of `make e2e`: a real image tag would make the kubelet pull 724 MB
# into a fresh kind node on every push, which milestone 6a's design declares a
# non-goal. This runs nightly instead (.github/workflows/nightly.yml), on a
# runner that already builds every image for `make image-repro`.
set -euo pipefail

CLUSTER="${CLUSTER:-spawnery-e2e-tutorial}"
E2E_KEEP="${E2E_KEEP:-0}"
DEADLINE="${DEADLINE:-300}"

# The chart's own default, unlike hack/e2e.sh's platform-system: this run
# installs exactly the command README.md documents, so what it exercises is
# what a reader running the tutorial actually gets.
OPERATOR_NAMESPACE=spawnery-system

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

workdir="$(mktemp -d)"
KUBECONFIG="$workdir/kubeconfig"
export KUBECONFIG

created_cluster=0

dump() {
	echo "================ operator log ================"
	kubectl -n "$OPERATOR_NAMESPACE" logs deployment/spawnery-operator --tail=-1 2>&1 || true
	echo "================ objects ================"
	kubectl get networks,servergroups,proxygroups,servers,pods,pvc -A 2>&1 || true
	echo "================ events ================"
	kubectl get events -A --sort-by=.lastTimestamp 2>&1 || true
}

cleanup() {
	local status=$?
	if [ "$status" -ne 0 ] && [ -s "$KUBECONFIG" ]; then
		dump
	fi
	if [ "$created_cluster" != "1" ]; then
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
nix build .#purpur-image --out-link result-purpur
nix build .#velocity-image --out-link result-velocity

# dockerTools.buildLayeredImage emits a gzipped archive; `kind load
# image-archive` wants a plain tar. Decompress if it is compressed and copy if
# it is not, so this keeps working either way -- the same test hack/e2e.sh
# uses for the same reason.
archive() {
	local result="$1" out="$2"
	if gunzip -t "$result" 2>/dev/null; then
		gunzip -c "$result" >"$out"
	else
		cp -L "$result" "$out"
	fi
}
archive result-operator "$workdir/operator.tar"
archive result-purpur "$workdir/purpur.tar"
archive result-velocity "$workdir/velocity.tar"

created_cluster=1
kind create cluster --name "$CLUSTER" --config docs/tutorial/kind-config.yaml --wait 120s
kind load image-archive "$workdir/operator.tar" --name "$CLUSTER"
kind load image-archive "$workdir/purpur.tar" --name "$CLUSTER"
kind load image-archive "$workdir/velocity.tar" --name "$CLUSTER"

# README's own Install command, unmodified. charts/spawnery/values.yaml's
# image.tag already tracks operatorVersion and pullPolicy is IfNotPresent, so
# the Deployment names exactly the image this run just built and loaded --
# nothing here has to override it the way hack/e2e.sh does, because this run
# does not move the operator to a non-default namespace.
helm install spawnery charts/spawnery --namespace "$OPERATOR_NAMESPACE" --create-namespace

kubectl -n "$OPERATOR_NAMESPACE" rollout status deployment/spawnery-operator --timeout="${DEADLINE}s"

SPAWNERY_E2E_TUTORIAL=1 go test -tags e2e -count=1 -v -timeout 20m -run TestTutorialPath ./test/e2e/...
