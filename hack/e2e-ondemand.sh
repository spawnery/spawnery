#!/usr/bin/env bash
# The on-demand run: one player's private server, started and stopped over a
# real agent session, with a real world on a real claim.
#
# It is its own script for one reason, and the reason is a decision hack/e2e.sh
# took deliberately: that run's manifest names images that never resolve, so no
# game process ever starts there. A world that survives its server can only be
# shown by a server that wrote one -- the phase the test waits for needs both
# halves of the ready gate, the server-list ping and the agent's own report,
# and the marker file needs a container to write it. So this run builds and
# loads the real Purpur image, the same one config/samples/ondemand.yaml names.
#
# Not folded into `make e2e` for the reason that decision gives: an image of
# this size on every push is milestone 6a's declared non-goal (spec 1.4, 7.4),
# and a real game server would not fit that run's 20-second startup deadline
# either. It is `make e2e-ondemand` instead, and .github/workflows/nightly.yml
# runs it beside the tutorial's own path.
#
# Everything else here is hack/e2e.sh's plumbing, deliberately: the same
# operator namespace, so test/e2e's own helpers keep reading the operator they
# already know how to find, and the same per-namespace forwarding-secret grant,
# for the same reason.
#
# # What this run needs
#
# A container runtime kind will take, and on paul-desktop that is not the one
# the name suggests: `docker` there is podman's shim, so kind detects podman,
# takes its rootless path and refuses without a delegated systemd scope. The
# first run of this script died on exactly that. The invocation is hack/e2e.sh's:
#
#   systemd-run --scope --user --property=Delegate=yes -- \
#     nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e-ondemand
#
# It needs more of the machine than hack/e2e.sh does, and knowing where that
# goes is worth a line: a real JVM with a 2 GiB limit, from
# test/e2e/manifests/ondemand.yaml's Network defaults. That limit is not
# decoration -- the image sizes its heap with MaxRAMPercentage, so a server with
# no limit takes three quarters of whatever machine it lands on, which on a
# GitHub runner is more than the runner has.
#
# # What a green run does not prove
#
# **Nothing about the consumer.** The plugin that starts and stops private
# servers lives in another repository; this run mints a proxy token and speaks
# the agent protocol itself. What it establishes is that the operator's side of
# those two requests works against a real cluster, not that anybody's plugin
# calls it correctly.
#
# **Nothing about a join.** No proxy process runs here -- the ProxyGroup's image
# never resolves on purpose, because a proxy pod is needed only as something to
# bind a token to. A player actually reaching a private server through a proxy
# is out of reach, and hack/e2e-tutorial.sh is the run that drives a join.
#
# **Nothing about a second player.** One key, started twice. Two members at once
# and spec.maxInstances are the envtest suites' (internal/agentserver), which can
# make a hundred without waiting for a JVM.
set -euo pipefail

CLUSTER="${CLUSTER:-spawnery-e2e-ondemand}"
E2E_KEEP="${E2E_KEEP:-0}"
DEADLINE="${DEADLINE:-300}"

# Not the chart's default, and not a choice this run gets to make freely:
# test/e2e's operatorNamespace is this string, and every helper in the package
# -- the log read, the denial hint a timed-out wait appends, the TLS secret
# this run's session verifies against -- looks the operator up there.
OPERATOR_NAMESPACE=platform-system

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# The cluster comes from docs/tutorial/kind-config.yaml, as both other runs do,
# and that config binds a fixed host port. Two clusters cannot hold it, and the
# collision would otherwise surface deep into the run, after the images are
# already built.
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
	# The one thing hack/e2e.sh never has to dump, because nothing there runs:
	# a failure here is usually Purpur's own, and its log is the only place
	# that says so.
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

# The manifest names the game image as a literal, the way every sample and
# every tutorial page in this repository does. That literal and the tag built
# above move together only as long as somebody moves both, and when they part
# the symptom is a pod in ErrImagePull twenty minutes in -- named here instead,
# in one line, before anything is built into a cluster.
manifest=test/e2e/manifests/ondemand.yaml
if ! grep -q "image: ${purpur_repo}:${purpur_tag}$" "$manifest"; then
	echo "$manifest does not name ${purpur_repo}:${purpur_tag}, which is the image this run" >&2
	echo "builds and loads. Nothing else puts a game image on the node, so the private server" >&2
	echo "would sit in ErrImagePull and never reach Ready. imageVersion in flake.nix has" >&2
	echo "probably moved; the manifest's tag has to move with it." >&2
	exit 1
fi

# dockerTools.buildLayeredImage emits a gzipped archive; `kind load
# image-archive` wants a plain tar.
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

# image.digest is cleared for the reason hack/e2e.sh gives at length: the
# chart's helper prefers a non-empty digest over the tag, and overriding only
# the tag leaves the Deployment naming an image nothing loaded.
#
# operator.startupDeadline is deliberately left at the chart's own 5m. hack/e2e.sh
# drops it to 20s to make a server fail on purpose; a real Purpur server
# generating a world does not reach Ready in twenty seconds, so this run would
# fail every start it made.
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

# The namespace and the grant the operator needs to read a Network's forwarding
# secret, before the operator ever looks -- hack/e2e.sh's own steps, including
# the read-back, which catches a sed that matched nothing (kubectl accepts a
# RoleBinding naming a ServiceAccount that exists nowhere).
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

# 30m rather than the 20m the other two runs allow: this one waits for a real
# world to be generated twice, and a cold kind node is the slowest place it
# will ever happen.
SPAWNERY_E2E_ONDEMAND=1 go test -tags e2e -count=1 -v -timeout 30m \
	-run TestAPrivateServersWorldOutlivesItsServer ./test/e2e/...
