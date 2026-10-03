/*
Copyright paul_wtf.

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

// Package podspec turns the Spawnery API objects into pod specs, without a
// client.
package podspec

const (
	LabelManagedBy = "spawnery.cloud/managed-by"
	// LabelNetwork is what NetworkPolicies select on.
	LabelNetwork = "spawnery.cloud/network"
	LabelGroup   = "spawnery.cloud/group"
	LabelServer  = "spawnery.cloud/server"
	// LabelKey carries an on-demand member's key, on its world claim only. The
	// chart's admission policy lets the operator delete a claim only if it
	// carries this label and the name composed from group and key.
	LabelKey  = "spawnery.cloud/key"
	LabelRole = "spawnery.cloud/role"
	// LabelOccupied is what the group's PodDisruptionBudget selects on. Server
	// and proxy pods have different writers (isOccupied, proxyOccupiedForBudget),
	// so a budget on this key must also pin LabelRole.
	LabelOccupied = "spawnery.cloud/occupied"
	// LabelPodHash digests the whole rendered pod rather than chosen spec fields,
	// so a new field cannot be forgotten. The cost: changing the rendering code
	// rolls the fleet on operator upgrade (docs/reference/known-issues.md).
	LabelPodHash = "spawnery.cloud/pod-hash"
	// LabelForwardingHash is the ForwardingHash of the secret as it stood when the
	// pod was created; Velocity and Paper read the file only at startup. A pod
	// without it is unknown, not stale. It is kept out of LabelPodHash so that a
	// rotation is reported instead of recreating every pod of the network at
	// once (docs/guides/rotating-the-forwarding-secret.md).
	LabelForwardingHash = "spawnery.cloud/forwarding-hash"
)

const (
	ManagedByValue = "spawnery-operator"
	RoleServer     = "server"
	RoleProxy      = "proxy"
)

// AnnotationSafeToEvict is only a hint to the cluster autoscaler; the
// PodDisruptionBudget is what protects against kubectl drain.
const AnnotationSafeToEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// AnnotationExposeAnnotations records, sorted, which annotation keys on a proxy
// group's Service the operator wrote. The load balancer controller annotates
// the same object, so without it the operator could not tell a key the user
// dropped from one that was never its own.
const AnnotationExposeAnnotations = "spawnery.cloud/expose-annotations"

// AnnotationProxyDrainingSince is RFC 3339.
const AnnotationProxyDrainingSince = "spawnery.cloud/draining-since"

// AnnotationRetireRequested marks a proxy an admin asked to retire, as RFC 3339.
const AnnotationRetireRequested = "spawnery.cloud/retire-requested"

// AnnotationProxyReadySince dates a proxy's first pass of the ready gate, as RFC 3339.
const AnnotationProxyReadySince = "spawnery.cloud/ready-since"

func ServerLabels(network, group, server string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
		LabelGroup:     group,
		LabelServer:    server,
		LabelRole:      RoleServer,
	}
}

// ProxyLabels carries no LabelServer: a proxy has no Server object.
func ProxyLabels(network, group string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
		LabelGroup:     group,
		LabelRole:      RoleProxy,
	}
}

// ServerGroupSelector is ServerLabels without LabelServer; a drifted selector
// would match no pods and look healthy, so
// TestTheServerGroupSelectorIsASubsetOfServerLabels ties them.
func ServerGroupSelector(network, group string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
		LabelGroup:     group,
		LabelRole:      RoleServer,
	}
}

// GroupConfigMapName carries the role because a ServerGroup and a ProxyGroup
// may share a name, and the suffix keeps a user's ConfigMap named after the
// group from coinciding with the operator's. Pod builders and group
// controllers must both go through it.
func GroupConfigMapName(group, role string) string {
	return group + "-" + role + "-config"
}

// GroupPDBName carries the role for the same reason as GroupConfigMapName.
func GroupPDBName(group, role string) string {
	return group + "-" + role + "-pdb"
}

func ManagedSelector(network string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
	}
}
