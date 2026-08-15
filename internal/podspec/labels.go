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
	// proxyOccupied (proxygroup_controller.go).
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
// failed early and it never got its Service or its pods, in an error loop
// naming neither the group nor the cause. The role suffix also keeps a
// user's own ConfigMap named after their group — their configOverlay
// ConfigMap being the likeliest way to do that — from ever coinciding with
// the one the operator owns and deletes when the group goes: without it, the
// operator would silently adopt the user's ConfigMap, inject config.yaml
// into it, and delete it on the group's own deletion.
//
// BuildServerPod and BuildProxyPod call this to know what to project into
// ConfigVolumeName; the group controllers call it to know what to write, so
// the two sides can only agree on a running pod if both go through this one
// function.
func GroupConfigMapName(group, role string) string {
	return group + "-" + role + "-config"
}

// ManagedSelector matches every pod Spawnery manages for one network.
func ManagedSelector(network string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
	}
}
