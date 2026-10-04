#!/usr/bin/env bash
# The tutorial's own path with the real Purpur and Velocity images and a real join.
#
# Not part of `make e2e`: the real images would put 724 MB into a fresh kind
# node on every push. Runs nightly instead (.github/workflows/nightly.yml).
set -euo pipefail

CLUSTER="${CLUSTER:-spawnery-e2e-tutorial}"
E2E_KEEP="${E2E_KEEP:-0}"
DEADLINE="${DEADLINE:-300}"
# E2E_RUN narrows -run, for proving one test bites.
E2E_RUN="${E2E_RUN:-TestTutorialPath|TestTutorialPlayableSlots|TestTutorialChangeoverStages|TestTutorialTransferOnDrain|TestTutorialJoinPermission|TestTutorialCloudCommands}"

# The chart's default, so this run installs exactly what README.md documents.
OPERATOR_NAMESPACE=spawnery-system

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# The tutorial's kind config binds a fixed host port, so a second cluster would fail late.
tutorial_join_port=30001
if ss -H -ltn "sport = :${tutorial_join_port}" 2>/dev/null | grep -q .; then
	echo "port ${tutorial_join_port} is already bound -- a tutorial cluster is probably still up." >&2
	echo "kind clusters: $(kind get clusters 2>/dev/null | tr '\n' ' ')" >&2
	echo "only one can run at a time: delete the other first (kind delete cluster --name <name>)." >&2
	exit 1
fi

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

# dockerTools emits a gzipped archive; `kind load image-archive` wants a plain tar.
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

# README's Install command, unmodified: image.tag already tracks operatorVersion.
helm install spawnery charts/spawnery --namespace "$OPERATOR_NAMESPACE" --create-namespace

kubectl -n "$OPERATOR_NAMESPACE" rollout status deployment/spawnery-operator --timeout="${DEADLINE}s"

# The reader's single-stream apply, which the Go test's per-document create does not exercise.
kubectl apply -f docs/tutorial/network.yaml

SPAWNERY_E2E_TUTORIAL=1 go test -tags e2e -count=1 -v -timeout 28m -run "$E2E_RUN" ./test/e2e/...
