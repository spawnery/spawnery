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

package v1alpha1

import corev1 "k8s.io/api/core/v1"

// Condition types used across all Spawnery resources.
const (
	// ConditionAccepted reports whether the operator manages this object at all.
	ConditionAccepted = "Accepted"
	// ConditionReady reports whether the object serves its purpose.
	ConditionReady = "Ready"
	// ConditionDegraded reports a persistent problem that needs attention.
	ConditionDegraded = "Degraded"
	// ConditionScalingLimited reports that a group would create more servers
	// to cover its spareSlots and maxReplicas is stopping it. Deliberately not
	// Degraded: a popular group sitting on its ceiling works exactly as
	// configured, and folding the two together would move the group's phase and
	// make a real fault during peak load indistinguishable from peak load.
	ConditionScalingLimited = "ScalingLimited"
	// ConditionReadinessDiverged is true while a proxy pod that was told to
	// stop taking connections has stayed Ready for longer than the grace
	// period. The operator does not try to repair it: a divergence from a
	// lost message is already corrected by the next resync, so what remains
	// is an agent that heard the withdrawal and did not act on it. The
	// reverse -- a pod that is supposed to be Ready and never gets there --
	// is deliberately not reported here: every pod is asserted ready from the
	// moment it exists, before it has had any chance to pass its first
	// probe, and treating that as a divergence would misname an ordinary
	// slow start as a proxy that disobeyed an instruction.
	ConditionReadinessDiverged = "ReadinessDiverged"
	// ConditionBackingOff reports that the group is waiting before it creates
	// another server, because one or more failed to start. It is deliberately
	// not folded into Degraded: derivePhase turns a true Degraded into the
	// group's phase, and a group waiting ten seconds after a single hiccup
	// would then be indistinguishable from one with a real fault.
	ConditionBackingOff = "BackingOff"
	// ConditionChangingOver is true while a ProxyGroup holds pods whose
	// rendered shape this operator no longer produces, and says how many.
	//
	// It exists because that state was invisible on the object. An operator
	// upgrade whose pod render changed moves the digest for every group in
	// every namespace at once, so every one of them begins replacing its pods
	// with nobody having edited a spec -- and until this condition, the only
	// outward sign was pods churning and readyReplicas dipping, which is what
	// a dozen unrelated faults also look like. Seeing it True on every group
	// simultaneously is the fingerprint of that upgrade; seeing it on one is
	// somebody having edited that group.
	//
	// It reports the hash half of staleness only. A pod being replaced because
	// its node is going away is ConditionNodeDraining's subject, and folding
	// the two together would lose the distinction the reader needs most: one
	// is local and expected, the other is fleet-wide and arrived with a
	// release.
	ConditionChangingOver = "ChangingOver"
	// ConditionNodeDraining is true while this group has pods on nodes that
	// are on their way out of service, and names them. It reports; the
	// removals it describes are decided elsewhere.
	ConditionNodeDraining = "NodeDraining"
	// ConditionStorageResize reports on a persistent group's attempt to grow
	// its claims. It is separate from Degraded on purpose: a storage class that
	// refuses expansion and a group whose servers will not start are different
	// problems with different remedies, and one field cannot carry both.
	ConditionStorageResize = "StorageResize"
	// ConditionForwardingSecretResolved reports whether this network's
	// forwarding secret can be read and carries a usable value. It is
	// deliberately not folded into Accepted: servergroup_controller.go derives
	// networkUsable from Accepted, proxygroup_controller.go gates on it, and
	// since 5b mayResize equals networkUsable — so reporting an unreadable
	// secret there would stop all sizing for the network, turning a
	// five-second API hiccup into a self-inflicted outage. Accepted keeps its
	// meaning: this Network owns its namespace.
	ConditionForwardingSecretResolved = "ForwardingSecretResolved"
	// ConditionForwardingSecretRotationPending is true while pods of this
	// network run on a forwarding secret that is no longer the current one.
	// The operator reports; it recreates nothing. Neither Velocity nor Paper
	// accepts two forwarding secrets at once, so any rollout has a window in
	// which joins fail, and the master design (section 6.5) leaves the order
	// to a runbook: all server groups first, then all proxy groups.
	//
	// Unknown is a real answer here rather than an omission — see
	// ReasonPodsPredateTracking.
	ConditionForwardingSecretRotationPending = "ForwardingSecretRotationPending"
)

// Condition reasons.
const (
	ReasonDuplicateNetwork     = "DuplicateNetwork"
	ReasonNetworkNotFound      = "NetworkNotFound"
	ReasonNetworkNotAccepted   = "NetworkNotAccepted"
	ReasonGroupNotFound        = "GroupNotFound"
	ReasonAccepted             = "Accepted"
	ReasonCrashLoopBackoff     = "CrashLoopBackoff"
	ReasonNoFallback           = "NoFallbackAvailable"
	ReasonNotImplemented       = "NotImplementedInThisVersion"
	ReasonReconciling          = "Reconciling"
	ReasonExposeNotImplemented = "ExposeStrategyNotImplemented"
	ReasonMaxReplicasReached   = "MaxReplicasReached"
	ReasonWithinLimits         = "WithinLimits"
	ReasonNoRecentFailures     = "NoRecentFailures"
	ReasonReadinessDiverged    = "ReadinessDiverged"
	ReasonReadinessAgrees      = "ReadinessAgrees"
	ReasonNodeDraining         = "NodeDraining"
	ReasonNoNodesDraining      = "NoNodesDraining"
	ReasonPodShapeChanged      = "PodShapeChanged"
	ReasonPodShapeCurrent      = "PodShapeCurrent"
	ReasonStorageResized       = "StorageResized"
	ReasonStorageResizeRefused = "StorageResizeRefused"

	// The three ProxyPods reasons on a ProxyGroup's Degraded condition. Two
	// failures rather than one because the remedies differ: a create the API
	// server refused is fixed at the namespace's policy, and a pod the
	// scheduler cannot place is fixed at the node count or at replicas.
	ReasonProxyPodRejected      = "ProxyPodRejected"
	ReasonProxyPodUnschedulable = "ProxyPodUnschedulable"
	ReasonProxyPodsAdmitted     = "ProxyPodsAdmitted"

	// The five ForwardingSecretResolved reasons. Three failures rather than
	// one because each has a different remedy: a name the user can fix, an
	// install step that was skipped, and neither of those.
	ReasonSecretResolved      = "SecretResolved"
	ReasonSecretNotFound      = "SecretNotFound"
	ReasonSecretKeyMissing    = "SecretKeyMissing"
	ReasonSecretReadForbidden = "SecretReadForbidden"
	ReasonSecretReadFailed    = "SecretReadFailed"

	// The four ForwardingSecretRotationPending reasons.
	// ReasonPodsPredateTracking is the Unknown that keeps an operator upgrade
	// from reading as a rotation: after an upgrade no running pod carries a
	// stamp, and calling that True would instruct every user to perform a
	// runbook they do not need.
	ReasonRotationPending        = "RotationPending"
	ReasonForwardingSecretInSync = "ForwardingSecretInSync"
	ReasonPodsPredateTracking    = "PodsPredateTracking"
	ReasonSecretUnresolved       = "SecretUnresolved"
)

// Event reasons. Separate from the condition reasons above because these name
// a transition rather than a state: both are emitted on entering a condition,
// never once per resync.
const (
	// EventForwardingSecretRotated fires when status.forwardingSecretHash
	// moves from a non-empty value to a different one. Empty to a value is
	// adoption, not rotation, and emits nothing.
	EventForwardingSecretRotated = "ForwardingSecretRotated"
	// EventForwardingSecretNotFound fires on entering SecretNotFound. It is
	// the loud channel for a misconfiguration that is otherwise reported under
	// the wrong name: the pods hang in ContainerCreating and the only
	// operator-side account arrives after --startup-deadline as a counted
	// startup failure, which is what a bad image looks like too.
	EventForwardingSecretNotFound = "ForwardingSecretNotFound"
)

// ObjectRef names another object in the same namespace.
type ObjectRef struct {
	// Name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// Scheduling controls where pods are placed.
type Scheduling struct {
	// NodeSelector restricts pods to nodes carrying all these labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow pods onto tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity expresses scheduling preferences and constraints.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// Defaults are inherited by every ProxyGroup and ServerGroup of a Network.
// Each field can be overridden on the group.
type Defaults struct {
	// MinecraftVersion documents the version the images of this network carry.
	// +optional
	MinecraftVersion string `json:"minecraftVersion,omitempty"`

	// ImagePullSecrets are attached to every managed pod.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// Resources are the default container resources.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling is the default pod placement.
	// +optional
	Scheduling *Scheduling `json:"scheduling,omitempty"`
}

// Mount is a single file mount into a managed pod. V1 supports ConfigMaps and
// Secrets only; the layered template system is a later project.
// +kubebuilder:validation:XValidation:rule="has(self.configMap) != has(self.secret)",message="exactly one of configMap or secret must be set"
type Mount struct {
	// Name of the volume inside the pod.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// MountPath is the absolute path inside the container.
	// +kubebuilder:validation:Pattern=`^/.*`
	MountPath string `json:"mountPath"`

	// ConfigMap source.
	// +optional
	ConfigMap *corev1.ConfigMapVolumeSource `json:"configMap,omitempty"`

	// Secret source.
	// +optional
	Secret *corev1.SecretVolumeSource `json:"secret,omitempty"`
}
