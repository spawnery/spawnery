#!/usr/bin/env bash
# The on-demand run: one player's private server, started and stopped over a
# real agent session, with a real world on a real claim, using the real Purpur
# image. Not part of `make e2e`: too large for every push, and a real server
# misses that run's 20-second startup deadline. Runs nightly.
#
# Under rootless podman:
#
#   systemd-run --scope --user --property=Delegate=yes -- \
#     nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e-ondemand
#
# Not covered: the consumer's plugin (this run speaks the agent protocol
# itself), a join (no proxy process runs), or a second player.
set -euo pipefail

CLUSTER="${CLUSTER:-spawnery-e2e-ondemand}"
E2E_KEEP="${E2E_KEEP:-0}"
DEADLINE="${DEADLINE:-300}"

# test/e2e's helpers look the operator up in this namespace.
OPERATOR_NAMESPACE=platform-system

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# docs/tutorial/kind-config.yaml binds a fixed host port, so a second cluster would fail late.
tutorial_join_port=30001
if ss -H -ltn "sport = :${tutorial_join_port}" 2>/dev/null | grep -q .; then
	echo "port ${tutorial_join_port} is already bound -- an e2e, tutorial or on-demand cluster is probably still up." >&2
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
	echo "================ the private server's own log ================"
	kubectl -n minecraft logs private-servers-c0ffee --tail=-1 2>&1 || true
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

operator_repo="$(nix eval --raw '.#operator-image.imageName')"
operator_tag="$(nix eval --raw '.#operator-image.imageTag')"
purpur_repo="$(nix eval --raw '.#purpur-image.imageName')"
purpur_tag="$(nix eval --raw '.#purpur-image.imageTag')"

manifest=test/e2e/manifests/ondemand.yaml
if ! grep -q "image: ${purpur_repo}:${purpur_tag}$" "$manifest"; then
	echo "$manifest does not name ${purpur_repo}:${purpur_tag}, which is the image this run" >&2
	echo "builds and loads. Nothing else puts a game image on the node, so the private server" >&2
	echo "would sit in ErrImagePull and never reach Ready. imageVersion in flake.nix has" >&2
	echo "probably moved; the manifest's tag has to move with it." >&2
	exit 1
fi

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

created_cluster=1
kind create cluster --name "$CLUSTER" --config docs/tutorial/kind-config.yaml --wait 120s
kind load image-archive "$workdir/operator.tar" --name "$CLUSTER"
kind load image-archive "$workdir/purpur.tar" --name "$CLUSTER"

# image.digest is cleared because the chart prefers a non-empty digest over the tag.
# startupDeadline stays at 5m: a real server generating a world needs more than e2e.sh's 20s.
helm install spawnery charts/spawnery \
	--namespace "$OPERATOR_NAMESPACE" \
	--create-namespace \
	--set image.repository="$operator_repo" \
	--set image.tag="$operator_tag" \
	--set image.digest="" \
	--set image.pullPolicy=Never

installed_image="$(kubectl -n "$OPERATOR_NAMESPACE" get deployment spawnery-operator \
	-o jsonpath='{.spec.template.spec.containers[0].image}')"
if [ "$installed_image" != "${operator_repo}:${operator_tag}" ]; then
	echo "the installed Deployment names ${installed_image}, not the image this run" >&2
	echo "just built and loaded (${operator_repo}:${operator_tag}). Check what in" >&2
	echo "charts/spawnery outranks image.tag now." >&2
	exit 1
fi

# forwarding-secret-reader.yaml hard-codes spawnery-system as its subject's namespace.
check_forwarding_secret_reader_subject() {
	local ns="$1" got
	got="$(kubectl -n "$ns" get rolebinding spawnery-forwarding-secret-reader -o jsonpath='{.subjects[0].namespace}')"
	if [ "$got" != "$OPERATOR_NAMESPACE" ]; then
		echo "hack/e2e-ondemand.sh: spawnery-forwarding-secret-reader in $ns names a ServiceAccount in namespace '$got', want '$OPERATOR_NAMESPACE'. kubectl apply does not reject this, so it would otherwise fail silently. Likely cause: the sed rewrite above did not match -- config/rbac/forwarding-secret-reader.yaml's 'namespace: spawnery-system' anchor may have moved." >&2
		exit 1
	fi
}

kubectl create namespace minecraft
sed "s/namespace: spawnery-system/namespace: ${OPERATOR_NAMESPACE}/" config/rbac/forwarding-secret-reader.yaml |
	kubectl apply -n minecraft -f -
check_forwarding_secret_reader_subject minecraft

kubectl -n "$OPERATOR_NAMESPACE" rollout status deployment/spawnery-operator --timeout="${DEADLINE}s"

# 30m: a real world is generated twice on a cold kind node.
SPAWNERY_E2E_ONDEMAND=1 go test -tags e2e -count=1 -v -timeout 30m \
	-run TestAPrivateServersWorldOutlivesItsServer ./test/e2e/...
