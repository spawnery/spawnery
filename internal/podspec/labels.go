/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package podspec turns the Spawnery API objects into pod specs. It is pure:
// no client, no cluster access, so every inheritance and override rule is
// covered by table tests.
package podspec

// Labels the operator puts on every managed pod.
const (
	// LabelManagedBy marks a pod as belonging to Spawnery.
	LabelManagedBy = "spawnery.cloud/managed-by"
	// LabelNetwork carries the Network name. NetworkPolicies select on it.
	LabelNetwork = "spawnery.cloud/network"
	// LabelGroup carries the ProxyGroup or ServerGroup name.
	LabelGroup = "spawnery.cloud/group"
	// LabelServer carries the Server name.
	LabelServer = "spawnery.cloud/server"
	// LabelRole is "server" or "proxy".
	LabelRole = "spawnery.cloud/role"
	// LabelOccupied is set to "true" while players are online. The group's
	// PodDisruptionBudget selects on it, which is what stops the eviction API
	// from removing an occupied pod. Maintained by the Server controller on
	// server pods and by the ProxyGroup controller on proxy pods, each from
	// its own occupancy rule — see isOccupied (candidates.go) and
	// proxyOccupiedForBudget (proxygroup_controller.go).
	//
	// Two writers over two populations, so a budget selecting on this key has
	// to pin LabelRole as well or it matches the other one's pods: see
	// ServerGroupReconciler.reconcilePDB, which shipped without that term and
	// let the eviction API take an occupied server pod.
	LabelOccupied = "spawnery.cloud/occupied"
	// LabelPodHash is a digest of the pod this operator would render for its
	// group right now, with the pod's own name and this label excluded. A pod
	// whose label differs from the current digest is stale and gets replaced.
	//
	// It covers the rendered pod rather than a chosen list of spec fields, so a
	// spec field added later cannot be forgotten here and silently never roll
	// out. The cost is recorded in docs/known-issues.md: a change to the
	// rendering code moves the digest for every group, so an operator upgrade
	// that touches podspec rolls the fleet.
	LabelPodHash = "spawnery.cloud/pod-hash"
	// LabelForwardingHash is podspec.ForwardingHash over the Network's
	// forwarding secret as it stood when this pod was created. It says which
	// secret this process read at startup, which is the only thing that
	// matters: the kubelet refreshes the projected file underneath a running
	// pod, and neither Velocity nor Paper reads it a second time.
	//
	// A pod without it is unknown, not stale. The pod builders omit it while
	// Network.status.forwardingSecretHash is empty, and the Network
	// controller reports that case as Unknown rather than raising a rotation
	// nobody can confirm.
	//
	// Deliberately not part of LabelPodHash: DesiredServerHash and
	// DesiredProxyHash delete it before digesting. Were it included, rotating
	// the secret would make every pod of the network stale at once and the
	// operator would recreate all of them, proxies and backends interleaved --
	// the uncoordinated version of the rollout the master design (section 6.5)
	// defers. Rotation is detected and reported; the restarts follow
	// docs/runbook-milestone-5c-secret-rotation.md.
	LabelForwardingHash = "spawnery.cloud/forwarding-hash"
)

// Label values.
const (
	// ManagedByValue is the value of LabelManagedBy.
	ManagedByValue = "spawnery-operator"
	// RoleServer is the value of LabelRole for Paper pods.
	RoleServer = "server"
	// RoleProxy is the value of LabelRole for Velocity pods.
	RoleProxy = "proxy"
)

// AnnotationSafeToEvict tells the cluster autoscaler to leave the pod alone.
// It is only a hint to the autoscaler and no protection against kubectl drain
// — that is what the PodDisruptionBudget is for.
const AnnotationSafeToEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// AnnotationExposeAnnotations records which annotation keys on a proxy
// group's Service the operator put there, comma-separated and sorted.
//
// It exists because the Service is shared ground: spec.expose.loadBalancer
// .annotations is written by the user, and the load balancer controller that
// acts on it -- MetalLB, kube-vip -- annotates the same object with its own
// keys. Without this record the operator could only choose between never
// removing a key the user dropped, and removing keys that were never its to
// touch. Sorted so the value is stable across passes; an unsorted join would
// make CreateOrUpdate write on every reconcile.
const AnnotationExposeAnnotations = "spawnery.cloud/expose-annotations"

// ServerLabels are the labels of a Paper pod.
func ServerLabels(network, group, server string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
		LabelGroup:     group,
		LabelServer:    server,
		LabelRole:      RoleServer,
	}
}

// ProxyLabels are the labels of a Velocity pod. There is deliberately no
// LabelServer: a proxy has no Server object, so there is nothing to put in
// it. The orphan sweep does not depend on the absence — it tells server and
// proxy pods apart by LabelRole — so this is a fact about proxy pods, not a
// mechanism anything is built on.
func ProxyLabels(network, group string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
		LabelGroup:     group,
		LabelRole:      RoleProxy,
	}
}

// GroupConfigMapName is the ConfigMap a group's controller renders its
// configuration into, per design section 4.6 — owned by the group, so
// deletion cascades without a name derived by convention.
//
// role must be RoleServer or RoleProxy. A ServerGroup and a ProxyGroup are
// different Kinds, so Kubernetes lets them share a name in the same
// namespace; a name that was only the group's own name collided the moment
// that happened — the second controller's SetControllerReference returned
// AlreadyOwnedError inside its mutate function, so that group's reconcile
// failed early and it never got its Service or its pods. AlreadyOwnedError's
// own message names both the object and the owning controller, so that part
// is not silent; what actually goes unnoticed is the group's own status: no
// condition is written for this failure, so whatever was last there (often a
// stale Accepted=True) stays, and `kubectl get` on the group alone shows
// nothing wrong. The role suffix also keeps a
// user's own ConfigMap named after their group — their configOverlay
// ConfigMap being the likeliest way to do that — from ever coinciding with
// the one the operator owns and deletes when the group goes.
//
// What that collision costs depends on one thing, and this comment used to
// state the worse half as though it were universal. If the user's ConfigMap
// carries podspec.LabelManagedBy, the operator adopts it silently: injects
// config.yaml into it and deletes it with the group. If it does not — which is
// the ordinary case, since nothing asks a user to apply that label —
// cmd/spawnery-operator/main.go narrows the manager's ConfigMap cache to that
// label, so the object is invisible to CreateOrUpdate's Get. The reconciler
// attempts a plain Create, gets AlreadyExists, and loops on that error instead
// of adopting anything. Nothing is destroyed; the group simply never comes up,
// and the CR does not say why.
//
// BuildServerPod and BuildProxyPod call this to know what to project into
// ConfigVolumeName; the group controllers call it to know what to write, so
// the two sides can only agree on a running pod if both go through this one
// function.
func GroupConfigMapName(group, role string) string {
	return group + "-" + role + "-config"
}

// GroupPDBName is the PodDisruptionBudget a group's controller keeps sized to
// its occupied pods, following GroupConfigMapName's scheme for the same
// reason that function's own doc comment narrates: a ServerGroup and a
// ProxyGroup are different Kinds and Kubernetes lets them share a name in one
// namespace, so a PodDisruptionBudget named only after the group collides the
// moment a user does that — the losing controller's SetControllerReference
// returns AlreadyOwnedError from inside CreateOrUpdate's mutate function on
// every pass, and that group's Reconcile fails before it ever reaches
// setStatus. As with the ConfigMap collision GroupConfigMapName documents,
// AlreadyOwnedError's own message names both the object and the owning
// controller; what stays silent is the group's own status, which carries no
// condition for this failure and so goes on showing whatever it last did.
//
// role must be RoleServer or RoleProxy, matching GroupConfigMapName.
func GroupPDBName(group, role string) string {
	return group + "-" + role + "-pdb"
}

// ManagedSelector matches every pod Spawnery manages for one network.
func ManagedSelector(network string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
	}
}
