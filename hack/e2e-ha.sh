#!/usr/bin/env bash
# The standby-replica run: the chart's two operator replicas on three kind
# workers, with real Purpur and Velocity images, through an upgrade from the
# latest released chart, a planned handover, a crashed leader, a rollout and
# a paused leader node. Not part of `make e2e`: it needs four kind nodes, real
# game images and the released chart from ghcr.io.
#
# Under rootless podman:
#
#   systemd-run --scope --user --property=Delegate=yes -- \
#     nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e-ha
#
# HA_RUN narrows the cases to a -test.run pattern; HA_COUNT repeats them.
#
# Not covered: a node that is powered off rather than paused. For new
# connections the two look alike: a dial to the paused leader's pod got no
# SYN-ACK (conntrack showed SYN_SENT, UNREPLIED; dev, 2026-11-11). Unlike a
# dead node, a paused one comes back.
set -euo pipefail

CLUSTER="${CLUSTER:-spawnery-e2e-ha}"
E2E_KEEP="${E2E_KEEP:-0}"
DEADLINE="${DEADLINE:-300}"
GAME_NAMESPACE=minecraft-ha
RELEASED_CHART=oci://ghcr.io/spawnery/charts/spawnery

# test/e2e's helpers look the operator up in this namespace.
OPERATOR_NAMESPACE=platform-system

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

workdir="$(mktemp -d)"
KUBECONFIG="$workdir/kubeconfig"
export KUBECONFIG

created_cluster=0

dump() {
	echo "================ operator logs ================"
	kubectl -n "$OPERATOR_NAMESPACE" logs -l app.kubernetes.io/name=spawnery,app.kubernetes.io/component=operator --prefix --tail=-1 2>&1 || true
	echo "================ the lease and the agent Service's slices ================"
	kubectl -n "$OPERATOR_NAMESPACE" get lease spawnery-operator.spawnery.cloud -o yaml 2>&1 || true
	kubectl -n "$OPERATOR_NAMESPACE" get endpointslices,endpoints -o wide 2>&1 || true
	echo "================ objects ================"
	kubectl get nodes -o wide 2>&1 || true
	kubectl get networks,servergroups,proxygroups,servers,pods -A -o wide 2>&1 || true
	echo "================ the network's pods ================"
	kubectl -n "$GAME_NAMESPACE" logs -l spawnery.cloud/managed-by=spawnery-operator --all-containers --prefix --tail=300 2>&1 || true
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

retry() {
	local attempt
	for attempt in 1 2 3 4 5; do
		"$@" && return 0
		echo "attempt $attempt of '$*' failed" >&2
		sleep 5
	done
	return 1
}

# Without --version, the highest released version.
retry helm pull "$RELEASED_CHART" --destination "$workdir"
released_chart="$(echo "$workdir"/spawnery-*.tgz)"
released_version="$(helm show chart "$released_chart" | sed -n 's/^version: //p')"
released_image="$(helm template spawnery "$released_chart" --show-only templates/deployment.yaml |
	sed -n 's/^ *image: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p')"
if [ -z "$released_version" ] || [ -z "$released_image" ]; then
	echo "could not read the version or the operator image of $released_chart" >&2
	exit 1
fi
# Pulled here and loaded below: on the dev VM, pulls from inside a kind node
# under rootless podman timed out (2026-11-11), while the host's own went through.
skopeo copy --insecure-policy --quiet --retry-times 5 "docker://$released_image" \
	"docker-archive:$workdir/released-operator.tar:$released_image"

nix build .#operator-image --out-link result-operator
nix build .#purpur-image --out-link result-purpur
nix build .#velocity-image --out-link result-velocity

operator_repo="$(nix eval --raw '.#operator-image.imageName')"
operator_tag="$(nix eval --raw '.#operator-image.imageTag')"
game_repo="$(nix eval --raw '.#purpur-image.imageName')"
game_tag="$(nix eval --raw '.#purpur-image.imageTag')"
proxy_repo="$(nix eval --raw '.#velocity-image.imageName')"
proxy_tag="$(nix eval --raw '.#velocity-image.imageTag')"

# Local builds carry the released tags until the next release. Under their
# own names the local operator would stand in for the released one the
# upgrade starts from, and a node missing a local game image would pull the
# released one, whose agent may keep the old keepalive.
local_tag="${operator_tag}-e2e-ha"
game_image="${game_repo}:${game_tag}-e2e-ha"
proxy_image="${proxy_repo}:${proxy_tag}-e2e-ha"

# dockerTools emits a gzipped archive; `kind load image-archive` wants a plain tar.
archive() {
	local result="$1" out="$2"
	if gunzip -t "$result" 2>/dev/null; then
		gunzip -c "$result" >"$out"
	else
		cp -L "$result" "$out"
	fi
}
archive result-operator "$workdir/operator-built.tar"
skopeo copy --insecure-policy --quiet "docker-archive:$workdir/operator-built.tar" \
	"docker-archive:$workdir/operator.tar:${operator_repo}:${local_tag}"
rm "$workdir/operator-built.tar"
archive result-purpur "$workdir/game-built.tar"
skopeo copy --insecure-policy --quiet "docker-archive:$workdir/game-built.tar" \
	"docker-archive:$workdir/game.tar:${game_image}"
rm "$workdir/game-built.tar"
archive result-velocity "$workdir/velocity-built.tar"
skopeo copy --insecure-policy --quiet "docker-archive:$workdir/velocity-built.tar" \
	"docker-archive:$workdir/velocity.tar:${proxy_image}"
rm "$workdir/velocity-built.tar"

# Built before the upgrade, so the upgrade case starts inside the 40 s it measures.
go test -tags e2e -c -o "$workdir/e2e.test" ./test/e2e

created_cluster=1
kind create cluster --name "$CLUSTER" --config test/e2e/ha-kind.yaml --wait 120s
kind load image-archive "$workdir/operator.tar" --name "$CLUSTER"
kind load image-archive "$workdir/game.tar" --name "$CLUSTER"
kind load image-archive "$workdir/velocity.tar" --name "$CLUSTER"
kind load image-archive "$workdir/released-operator.tar" --name "$CLUSTER"

# Off the workers, so that pausing the leader's node does not take half of
# the cluster's DNS with it: that would measure CoreDNS failover, not the
# operator's.
kubectl -n kube-system patch deployment coredns --type merge \
	-p '{"spec":{"template":{"spec":{"nodeSelector":{"node-role.kubernetes.io/control-plane":""}}}}}'
kubectl -n kube-system rollout status deployment/coredns --timeout="${DEADLINE}s"

helm install spawnery "$released_chart" \
	--namespace "$OPERATOR_NAMESPACE" --create-namespace --wait --timeout "${DEADLINE}s"

stale_slices="$(kubectl -n "$OPERATOR_NAMESPACE" get endpointslices \
	-l kubernetes.io/service-name=spawnery-operator -o jsonpath='{.items[*].metadata.name}')"
if [ -z "$stale_slices" ] || ! kubectl -n "$OPERATOR_NAMESPACE" get endpoints spawnery-operator >/dev/null 2>&1; then
	echo "the released chart $released_version left no EndpointSlice or no Endpoints object for" >&2
	echo "spawnery-operator, so the upgrade has nothing stale to clean up and proves nothing." >&2
	exit 1
fi
echo "released chart $released_version installed; its selector produced: $stale_slices"

# image.digest is cleared because spawnery.image prefers a non-empty digest over the tag.
helm upgrade spawnery charts/spawnery \
	--namespace "$OPERATOR_NAMESPACE" \
	--set image.repository="$operator_repo" \
	--set image.tag="$local_tag" \
	--set image.digest="" \
	--set image.pullPolicy=Never \
	--wait --timeout "${DEADLINE}s"
upgraded_at="$(date +%s)"

installed_image="$(kubectl -n "$OPERATOR_NAMESPACE" get deployment spawnery-operator \
	-o jsonpath='{.spec.template.spec.containers[0].image}')"
if [ "$installed_image" != "${operator_repo}:${local_tag}" ]; then
	echo "the upgraded Deployment names ${installed_image}, not the image this run" >&2
	echo "just built and loaded (${operator_repo}:${local_tag}). Check what in" >&2
	echo "charts/spawnery outranks image.tag now." >&2
	exit 1
fi

# forwarding-secret-reader.yaml hard-codes spawnery-system as its subject's namespace.
kubectl create namespace "$GAME_NAMESPACE"
sed "s/namespace: spawnery-system/namespace: ${OPERATOR_NAMESPACE}/" config/rbac/forwarding-secret-reader.yaml |
	kubectl apply -n "$GAME_NAMESPACE" -f -
got="$(kubectl -n "$GAME_NAMESPACE" get rolebinding spawnery-forwarding-secret-reader -o jsonpath='{.subjects[0].namespace}')"
if [ "$got" != "$OPERATOR_NAMESPACE" ]; then
	echo "hack/e2e-ha.sh: spawnery-forwarding-secret-reader in $GAME_NAMESPACE names a ServiceAccount in namespace '$got', want '$OPERATOR_NAMESPACE'. The sed rewrite above did not match." >&2
	exit 1
fi

echo "operator ${operator_repo}:${local_tag}, game ${game_image}, proxy ${proxy_image}, all built from this tree"

# go test runs a test binary in its package's directory; test/e2e's paths are relative to it.
cd test/e2e
SPAWNERY_E2E_HA=1 \
	SPAWNERY_E2E_HA_UPGRADED_AT="$upgraded_at" \
	SPAWNERY_E2E_HA_STALE_SLICES="$stale_slices" \
	SPAWNERY_E2E_GAME_IMAGE="$game_image" \
	SPAWNERY_E2E_PROXY_IMAGE="$proxy_image" \
	"$workdir/e2e.test" -test.count="${HA_COUNT:-1}" -test.v -test.timeout 60m -test.run "${HA_RUN:-TestHA}"
