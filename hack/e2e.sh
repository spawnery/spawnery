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
# # What a green run does not prove
#
# Two decisions here put whole classes of behaviour out of reach, and both are
# worth knowing before this is read as coverage.
#
# **One node.** kind's default topology, so nothing in this run can reach node
# drain and its taint handling, a PodDisruptionBudget's effect on a real
# eviction, HostPort and its CNI dependency, a LoadBalancer address, or CIS
# `restricted` pod security. Those are the RKE2 rollout's (design §12), and
# docs/runbook-milestone-6-rollout.md is where each was driven.
#
# **No image resolves**, by the decision test/e2e/manifests/e2e.yaml takes and
# explains, so no game or proxy process ever starts and every pod sits in
# ErrImagePull for the whole run. Out of reach in consequence: the second stage
# of the ready gate, which needs a connected agent; syncOccupiedLabel and the
# PDB upkeep, which need a server that has been Ready once; growing a claim;
# and a join. There is no cheap stand-in -- the readiness probe execs
# spawnery-slp against 127.0.0.1:25565, and both that binary and something
# answering a server-list ping exist only in the real Paper image.
#
# On this machine kind runs under rootless podman, which needs both an
# environment variable and a systemd scope. The script deliberately hard-codes
# neither:
#
#   systemd-run --scope --user --property=Delegate=yes -- \
#     nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
#
# It costs less memory than it looks like it should. Measured 2026-08-25 on a
# 7.9 GiB machine with no swap: 962 MiB over baseline for a full run with a
# warm nix store, of which a bare single-node kind cluster is 675 MiB. No image
# in the run resolves, by decision, so no game or proxy process ever starts --
# what runs is a control plane and one operator pod with a 256 MiB limit. The
# `nix build` at line 95 costs about 1 GiB on a cold store and finishes before
# `kind create` at line 112, so the peak is the larger of the two rather than
# their sum.
set -euo pipefail

CLUSTER="${CLUSTER:-spawnery-e2e}"
E2E_KEEP="${E2E_KEEP:-0}"
DEADLINE="${DEADLINE:-300}"

# Deliberately not spawnery-system, the chart's own default. What that buys
# splits in two, and only one half is something this run's scenarios can see.
#
# A spawnery-system literal surviving in one of the chart's *own-namespace*
# RBAC fields -- a RoleBinding's own metadata.namespace, or the generated
# Role's namespace: line -- is caught by the `helm install` below, because
# Kubernetes validates those for existence at admission. Measured, not
# reasoned: the install refused with `namespaces "spawnery-system" not
# found`, this script stopped there under set -e, and `go test` never ran --
# so no scenario in test/e2e caught that mutation or could have
# (docs/known-issues.md, "From milestone 6d").
#
# A literal surviving in a *subject* namespace -- subjects[].namespace, which
# the chart templates as {{ .Release.Namespace }} -- is not validated by the
# API server at all: it applies cleanly and binds a ServiceAccount that exists
# nowhere. That is by design the path theOperatorWasNeverDenied catches, once
# the resulting denial lands on a write verb; this milestone never mutated it,
# so that half is reasoning rather than measurement and nothing here has
# proven it.
#
# The name shares nothing with the default on purpose: a near-miss like
# spawnery-operators reads as a variant and invites somebody to tidy it back.
OPERATOR_NAMESPACE=platform-system

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
	kubectl -n "$OPERATOR_NAMESPACE" logs deployment/spawnery-operator --tail=-1 2>&1 || true
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
image_repo="$(nix eval --raw '.#operator-image.imageName')"
image_tag="$(nix eval --raw '.#operator-image.imageTag')"

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

# A first run of this script hit "namespaces \"spawnery-system\" not found":
# kubectl apply -f config/deploy/ walks that directory alphabetically, so the
# Deployment landed before the Namespace. Helm has its own answer to install
# ordering; use it, rather than porting the script's sequence. It also creates
# the CRDs in charts/spawnery/templates/, so there is no separate
# `kubectl apply -f config/crd/bases/` here either.
#
# The image is set to what was just built, not whatever a registry holds, and
# imagePullPolicy Never makes that a guarantee rather than a hope: a missing
# local image then fails loudly instead of being fetched.
#
# image.digest is cleared, and that is not belt-and-braces. _helpers.tpl's
# spawnery.image prefers digest over tag whenever digest is non-empty, so
# overriding only the tag leaves the Deployment naming
# ghcr.io/spawnery/spawnery-operator@sha256:... -- an image no `kind load`
# put on the node -- and the run dies five minutes later in a rollout
# timeout whose only clue is ErrImageNeverPull. That is exactly what CI run
# 32352012323 did, on the commit that first wrote a real digest into
# values.yaml. Until then digest was "" and the tag happened to win, so this
# override was correct by accident for as long as the field was empty.
helm install spawnery charts/spawnery \
	--namespace "$OPERATOR_NAMESPACE" \
	--create-namespace \
	--set image.repository="$image_repo" \
	--set image.tag="$image_tag" \
	--set image.digest="" \
	--set image.pullPolicy=Never \
	--set operator.startupDeadline=20s

# And read back what the install actually produced, rather than trusting the
# flags above to have been the ones that win. The flag list is the input; the
# container's image is the outcome, and this repository has now shipped three
# guards that checked the first and missed the second. If a later values.yaml
# key comes to outrank both tag and digest, this says so in one line at the
# point of installation instead of as a pull failure two hundred lines down.
installed_image="$(kubectl -n "$OPERATOR_NAMESPACE" get deployment spawnery-operator \
	-o jsonpath='{.spec.template.spec.containers[0].image}')"
if [ "$installed_image" != "${image_repo}:${image_tag}" ]; then
	echo "the installed Deployment names ${installed_image}, not the image this run" >&2
	echo "just built and loaded (${image_repo}:${image_tag}). Nothing put that image on" >&2
	echo "the kind node, so every pod would fail ErrImageNeverPull. Check what in" >&2
	echo "charts/spawnery outranks image.tag now." >&2
	exit 1
fi

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
# What happens without it, as of 2026-08-24: test/e2e's denial check DOES
# fire. It did not until then, and why is worth keeping, because it is a
# second and quite different way a denied read escapes that check -- not the
# cache, just an error the code handled instead of surfacing.
# readForwardingSecret (internal/controller/forwardingsecret.go) folds the 403
# into a condition whose message says "the operator may not read secret ...",
# written for a person and quoting no API server, so it carries no
# `is forbidden:` substring; the read sits after the Accepted branch has
# already returned, so no scenario fails either; and network_controller.go
# made no logger call at all. The read now carries the API server's own error
# out beside the message and the controller logs it, once per entry into the
# state, which is the line theOperatorWasNeverDenied greps for.
#
# The namespace is created here rather than by the test manifest so the grant
# can exist before the operator ever looks; applyManifest tolerates the
# namespace already being there.
kubectl create namespace minecraft

# config/rbac/forwarding-secret-reader.yaml hard-codes its RoleBinding
# subject's namespace to spawnery-system (line 65) because that is the
# operator's own default -- this file stays outside the chart on purpose, per
# its own header comment, and nothing in this milestone templates it. With the
# operator running in $OPERATOR_NAMESPACE instead, applying it unedited would
# name a ServiceAccount that does not exist where the operator actually runs,
# and every Network here would report SecretReadForbidden for a reason that
# has nothing to do with the templating this run exists to check. A real
# administrator installing the chart into a non-default namespace hits this by
# hand and has to edit the file before applying it -- that manual step is real
# and this script does not remove it from the world, only from this one run:
# it performs the identical edit programmatically so the run matches
# $OPERATOR_NAMESPACE instead of stopping to ask a person to do it.
#
# sed exits 0 whether or not its pattern matched. If the anchor above ever
# goes missing -- the file reformatted, the string changed -- the unedited
# YAML applies without complaint: a RoleBinding subject naming a namespace
# that does not exist is not validated by the API server the way the
# RoleBinding's own metadata.namespace is, so `kubectl apply` would succeed
# having granted nothing. The only symptom would be a Network's
# ForwardingSecretResolved condition going SecretReadForbidden at runtime --
# and test/e2e cannot see that: readForwardingSecret folds the 403 into a
# condition message with no `is forbidden:` substring and nothing on that
# path logs (see theOperatorWasNeverDenied's doc comment,
# test/e2e/e2e_test.go:187-193), so a broken rewrite here would still produce
# a fully green 18/18 run. Reading the applied object back and checking what
# it actually says is the only check that catches that; checking the input
# file's shape before the sed runs would not, for the same reason
# hack/chart-templates.sh's input-shape guards did not catch a broken
# transform there -- that script now checks the files it wrote as well, and
# this is the same shape of check on the object this one wrote.
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

# The second namespace exists to be hostile. Pod Security baseline disallows
# host ports, so the HostPort group the manifest puts here can never get a
# pod -- which is the one refusal this whole run can observe being enforced,
# and it is the API server enforcing it, not a CNI. The label goes on at
# creation rather than later so no pod can slip in before it.
kubectl create namespace minecraft-baseline
kubectl label namespace minecraft-baseline pod-security.kubernetes.io/enforce=baseline
sed "s/namespace: spawnery-system/namespace: ${OPERATOR_NAMESPACE}/" config/rbac/forwarding-secret-reader.yaml |
	kubectl apply -n minecraft-baseline -f -
check_forwarding_secret_reader_subject minecraft-baseline

kubectl -n "$OPERATOR_NAMESPACE" rollout status deployment/spawnery-operator --timeout="${DEADLINE}s"

go test -tags e2e -count=1 -v -timeout 20m ./test/e2e/...
