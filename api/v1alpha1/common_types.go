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
