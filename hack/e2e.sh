#!/usr/bin/env bash
# The driven end-to-end run: the operator inside a real cluster, under its own
# ServiceAccount. Plumbing only; every claim is made by the Go package in test/e2e.
#
# Not covered: anything needing a second node, or a running game or proxy
# process -- no image in test/e2e/manifests/e2e.yaml resolves, by decision.
#
# Under rootless podman:
#
#   systemd-run --scope --user --property=Delegate=yes -- \
#     nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
set -euo pipefail

CLUSTER="${CLUSTER:-spawnery-e2e}"
E2E_KEEP="${E2E_KEEP:-0}"
DEADLINE="${DEADLINE:-300}"

# Deliberately not the chart's default, so a hard-coded spawnery-system in the
# chart fails here; a near-miss name would invite someone to tidy it back.
OPERATOR_NAMESPACE=platform-system

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# docs/tutorial/kind-config.yaml binds a fixed host port, so a second cluster would fail late.
tutorial_join_port=30001
if ss -H -ltn "sport = :${tutorial_join_port}" 2>/dev/null | grep -q .; then
	echo "port ${tutorial_join_port} is already bound -- an e2e or tutorial cluster is probably still up." >&2
	echo "kind clusters: $(kind get clusters 2>/dev/null | tr '\n' ' ')" >&2
	echo "only one can run at a time: delete the other first (kind delete cluster --name <name>)." >&2
	exit 1
fi

workdir="$(mktemp -d)"
KUBECONFIG="$workdir/kubeconfig"
export KUBECONFIG

# Guards cleanup from deleting a kept cluster when this run failed before creating one.
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
image_repo="$(nix eval --raw '.#operator-image.imageName')"
image_tag="$(nix eval --raw '.#operator-image.imageTag')"

# dockerTools emits a gzipped archive; `kind load image-archive` wants a plain tar.
archive="$workdir/operator.tar"
if gunzip -t result-operator 2>/dev/null; then
	gunzip -c result-operator >"$archive"
else
	cp -L result-operator "$archive"
fi

# Before the create: a half-failed create leaves a partial cluster that is ours.
created_cluster=1
# The tutorial's own kind config, so a port mapping that breaks for readers breaks here too.
kind create cluster --name "$CLUSTER" --config docs/tutorial/kind-config.yaml --wait 120s
kind load image-archive "$archive" --name "$CLUSTER"

# image.digest is cleared because spawnery.image prefers a non-empty digest over the tag.
helm install spawnery charts/spawnery \
	--namespace "$OPERATOR_NAMESPACE" \
	--create-namespace \
	--set image.repository="$image_repo" \
	--set image.tag="$image_tag" \
	--set image.digest="" \
	--set image.pullPolicy=Never \
	--set operator.startupDeadline=20s

installed_image="$(kubectl -n "$OPERATOR_NAMESPACE" get deployment spawnery-operator \
	-o jsonpath='{.spec.template.spec.containers[0].image}')"
if [ "$installed_image" != "${image_repo}:${image_tag}" ]; then
	echo "the installed Deployment names ${installed_image}, not the image this run" >&2
	echo "just built and loaded (${image_repo}:${image_tag}). Nothing put that image on" >&2
	echo "the kind node, so every pod would fail ErrImageNeverPull. Check what in" >&2
	echo "charts/spawnery outranks image.tag now." >&2
	exit 1
fi

# The ClusterRole grants no secret reads outside the operator's namespace, so
# this run grants the per-namespace read an administrator would, before the
# operator ever looks.
kubectl create namespace minecraft

# forwarding-secret-reader.yaml hard-codes spawnery-system as its subject's namespace.
check_forwarding_secret_reader_subject() {
	local ns="$1" got
	got="$(kubectl -n "$ns" get rolebinding spawnery-forwarding-secret-reader -o jsonpath='{.subjects[0].namespace}')"
	if [ "$got" != "$OPERATOR_NAMESPACE" ]; then
		echo "hack/e2e.sh: spawnery-forwarding-secret-reader in $ns names a ServiceAccount in namespace '$got', want '$OPERATOR_NAMESPACE'. kubectl apply does not reject this, so it would otherwise fail silently. Likely cause: the sed rewrite above did not match -- config/rbac/forwarding-secret-reader.yaml's 'namespace: spawnery-system' anchor (line 65) may have moved." >&2
		exit 1
	fi
}

sed "s/namespace: spawnery-system/namespace: ${OPERATOR_NAMESPACE}/" config/rbac/forwarding-secret-reader.yaml |
	kubectl apply -n minecraft -f -
check_forwarding_secret_reader_subject minecraft

# Pod Security baseline disallows host ports, so the HostPort group here never gets a pod.
kubectl create namespace minecraft-baseline
kubectl label namespace minecraft-baseline pod-security.kubernetes.io/enforce=baseline
sed "s/namespace: spawnery-system/namespace: ${OPERATOR_NAMESPACE}/" config/rbac/forwarding-secret-reader.yaml |
	kubectl apply -n minecraft-baseline -f -
check_forwarding_secret_reader_subject minecraft-baseline

kubectl -n "$OPERATOR_NAMESPACE" rollout status deployment/spawnery-operator --timeout="${DEADLINE}s"

go test -tags e2e -count=1 -v -timeout 20m ./test/e2e/...
