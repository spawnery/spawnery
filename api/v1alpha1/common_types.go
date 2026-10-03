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
	// to cover its spareSlots and maxReplicas is stopping it. Not Degraded: a
	// group at its ceiling works as configured.
	ConditionScalingLimited = "ScalingLimited"
	// ConditionReadinessDiverged is true while a proxy pod that was told to
	// stop taking connections has stayed Ready for longer than the grace
	// period. The reverse is not reported: a pod is meant to be Ready from
	// creation, so every slow start would count.
	ConditionReadinessDiverged = "ReadinessDiverged"
	// ConditionBackingOff reports that the group is waiting before it creates
	// another server, because one or more failed to start. Not Degraded,
	// which would set the group's phase after a single failed start.
	ConditionBackingOff = "BackingOff"
	// ConditionOrdinalBlocked is true while a persistent ServerGroup cannot
	// act on one of its ordinals: another object holds the name
	// `<group>-<ordinal>` (OrdinalNameTaken), or two Servers of the group
	// carry the same spec.ordinal (OrdinalCarriedByTwoServers).
	ConditionOrdinalBlocked = "OrdinalBlocked"
	// ConditionChangingOver is true while a ProxyGroup holds pods whose
	// rendered shape this operator no longer produces, and says how many.
	// True on every group at once means an operator upgrade changed the pod
	// render. Pods replaced for a draining node are ConditionNodeDraining's.
	ConditionChangingOver = "ChangingOver"
	// ConditionProgressing is true while a ServerGroup has not arrived where it
	// decided to be: a server of the current generation is still coming up, or
	// a server of an earlier one is still there. status.phase Ready only says
	// the group is serving. Unlike a Deployment's, it never turns False for
	// "stuck"; Degraded/GaveUp says that.
	ConditionProgressing = "Progressing"
	// ConditionNodeDraining is true while this group has pods on nodes that
	// are on their way out of service, and names them. It reports; the
	// removals it describes are decided elsewhere.
	ConditionNodeDraining = "NodeDraining"
	// ConditionStorageResize reports on resizes of a group's claims, in
	// persistent and on-demand groups alike: a patch of this operator's own
	// that the API server refused, or a resize from any requester that the
	// storage driver failed. Separate from Degraded because the remedy differs.
	ConditionStorageResize = "StorageResize"
	// ConditionForwardingSecretResolved reports whether this network's
	// forwarding secret can be read and carries a usable value. Not folded
	// into Accepted: every group gates its sizing on Accepted, so a transient
	// read error would stop the whole network.
	ConditionForwardingSecretResolved = "ForwardingSecretResolved"
	// ConditionForwardingSecretRotationPending is true while pods of this
	// network run on a forwarding secret that is no longer the current one.
	// The operator reports; it recreates nothing. Neither Velocity nor Paper
	// accepts two forwarding secrets at once, so the order is left to a
	// runbook: all server groups first, then all proxy groups. For Unknown see
	// ReasonPodsPredateTracking.
	ConditionForwardingSecretRotationPending = "ForwardingSecretRotationPending"
	// ConditionRescueWindowShort is true when the proxies serving this network
	// give up on a silent backend (Velocity's read timeout, reported on Hello)
	// before phase.RescueWindow lets the operator move its players. Not folded
	// into Accepted, which every group gates on. Unknown while no proxy has
	// reported.
	ConditionRescueWindowShort = "RescueWindowShort"
)

// Condition reasons.
const (
	ReasonDuplicateNetwork   = "DuplicateNetwork"
	ReasonNetworkNotFound    = "NetworkNotFound"
	ReasonNetworkNotAccepted = "NetworkNotAccepted"
	// ReasonNetworkPolicyNotWritten: the Network's NetworkPolicy could not be
	// written, so it is Accepted=False and the namespace stays closed rather
	// than unprotected.
	ReasonNetworkPolicyNotWritten = "NetworkPolicyNotWritten"
	// Until a proxy has reported, ConditionRescueWindowShort is Unknown with
	// ReasonNoProxyReported, which does not mean sufficient.
	ReasonRescueWindowTooShort   = "RescueWindowTooShort"
	ReasonRescueWindowSufficient = "RescueWindowSufficient"
	ReasonNoProxyReported        = "NoProxyReported"
	ReasonGroupNotFound          = "GroupNotFound"
	ReasonAccepted               = "Accepted"
	ReasonCrashLoopBackoff       = "CrashLoopBackoff"
	ReasonNoFallback             = "NoFallbackAvailable"
	ReasonNotImplemented         = "NotImplementedInThisVersion"
	ReasonReconciling            = "Reconciling"
	ReasonExposeNotImplemented   = "ExposeStrategyNotImplemented"
	ReasonMaxReplicasReached     = "MaxReplicasReached"
	ReasonWithinLimits           = "WithinLimits"
	ReasonNoRecentFailures       = "NoRecentFailures"
	ReasonReadinessDiverged      = "ReadinessDiverged"
	ReasonReadinessAgrees        = "ReadinessAgrees"
	ReasonNodeDraining           = "NodeDraining"
	ReasonNoNodesDraining        = "NoNodesDraining"
	ReasonPodShapeChanged        = "PodShapeChanged"
	ReasonPodShapeCurrent        = "PodShapeCurrent"
	ReasonOrdinalNameTaken       = "OrdinalNameTaken"
	// ReasonOrdinalDuplicated: the operator will not choose between two
	// worlds, so this needs a person.
	ReasonOrdinalDuplicated = "OrdinalCarriedByTwoServers"
	ReasonOrdinalsAvailable = "OrdinalsAvailable"
	// ReasonWaitingForChangeoverBudget: the network's changeover budget is
	// spent by other groups.
	ReasonWaitingForChangeoverBudget = "WaitingForChangeoverBudget"
	// ReasonWaitingForEarlierStage: a group of a lower changeoverStage is
	// still changing over.
	ReasonWaitingForEarlierStage = "WaitingForEarlierStage"
	// ReasonWaitingForMinAvailable: the next stale server would leave fewer
	// than spec.update.minAvailable joinable servers, and the extra server
	// that would keep the floor is not being built.
	ReasonWaitingForMinAvailable = "WaitingForMinAvailable"
	ReasonServersStarting        = "ServersStarting"
	ReasonReplacingServers       = "ReplacingServers"
	ReasonAtDesiredState         = "AtDesiredState"
	// ReasonRetireeStuck says a server carrying spec.retire has failed, and is
	// therefore holding an update slot until its retention window ends.
	ReasonRetireeStuck         = "RetireeFailedAndHoldsTheBudget"
	ReasonStorageResized       = "StorageResized"
	ReasonStorageResizeRefused = "StorageResizeRefused"
	// ReasonConfigMapNotOurs says something else occupies the name this group
	// renders its ConfigMap at. Degraded, since the group can start no pod.
	ReasonConfigMapNotOurs = "ConfigMapNotOwnedByGroup"

	// The ProxyPods reasons on a ProxyGroup's Degraded condition.
	ReasonProxyPodRejected      = "ProxyPodRejected"
	ReasonProxyPodUnschedulable = "ProxyPodUnschedulable"
	ReasonProxyPodsAdmitted     = "ProxyPodsAdmitted"

	// The ForwardingSecretResolved reasons.
	ReasonSecretResolved      = "SecretResolved"
	ReasonSecretNotFound      = "SecretNotFound"
	ReasonSecretKeyMissing    = "SecretKeyMissing"
	ReasonSecretReadForbidden = "SecretReadForbidden"
	ReasonSecretReadFailed    = "SecretReadFailed"

	// ReasonPluginVolumeUnusable says spec.extraPlugins names a claim that is
	// missing, or that cannot be mounted by every server of the group.
	ReasonPluginVolumeUnusable = "PluginVolumeUnusable"
	// ReasonPluginVolumesDisabled says spec.extraPlugins is set on an
	// installation whose operator was not started with
	// --allow-plugin-volumes.
	ReasonPluginVolumesDisabled = "PluginVolumesDisabled"

	// ReasonFileVolumeUnusable says spec.extraFiles names a claim that is
	// missing or not ReadWriteMany.
	ReasonFileVolumeUnusable = "FileVolumeUnusable"
	// ReasonFileVolumesDisabled says spec.extraFiles is set on an
	// installation started without --allow-file-volumes.
	ReasonFileVolumesDisabled = "FileVolumesDisabled"

	// ReasonMountVolumeUnusable says a spec.mounts entry names a claim that
	// is missing, or that cannot be mounted by every pod of the group.
	ReasonMountVolumeUnusable = "MountVolumeUnusable"
	// ReasonMountVolumesDisabled says a spec.mounts entry names a claim on an
	// installation whose operator was not started with
	// --allow-mount-volumes.
	ReasonMountVolumesDisabled = "MountVolumesDisabled"

	// ReasonSchedulingNotAllowed says the group's effective spec.scheduling
	// asks something of the scheduler its Network's spec.scheduling does not
	// allow; the message names the key or namespace.
	ReasonSchedulingNotAllowed = "SchedulingNotAllowed"
	// ReasonHostPortNotAllowed says a HostPort proxy group's port lies
	// outside its Network's spec.scheduling.hostPortRange, or the Network
	// has none.
	ReasonHostPortNotAllowed = "HostPortNotAllowed"

	// The ForwardingSecretRotationPending reasons. ReasonPodsPredateTracking
	// is Unknown: after an operator upgrade no running pod carries a stamp yet,
	// which is not a rotation.
	ReasonRotationPending        = "RotationPending"
	ReasonForwardingSecretInSync = "ForwardingSecretInSync"
	ReasonPodsPredateTracking    = "PodsPredateTracking"
	ReasonSecretUnresolved       = "SecretUnresolved"
)

// Event reasons. These name a transition rather than a state and are emitted
// on entering a condition, never once per resync. Entry is judged by the
// condition in etcd, so NetworkReconciler emits only after the status update
// lands.
const (
	// EventForwardingSecretRotated fires when status.forwardingSecretHash
	// moves from a non-empty value to a different one. Empty to a value is
	// adoption, not rotation, and emits nothing.
	EventForwardingSecretRotated = "ForwardingSecretRotated"
	// EventForwardingSecretNotFound fires on entering SecretNotFound. Without
	// it the pods hang in ContainerCreating and only show up later as
	// startup failures, like a bad image would.
	EventForwardingSecretNotFound = "ForwardingSecretNotFound"
)

// ChangeoverState is a group's changeover as the network's changeover budget
// sees it.
type ChangeoverState string

const (
	// ChangeoverNone means the group is not changing over.
	ChangeoverNone ChangeoverState = ""
	// ChangeoverWaiting means the group must change over but has not begun:
	// it holds no place in the network's budget.
	ChangeoverWaiting ChangeoverState = "Waiting"
	// ChangeoverBegun means the group has a server or pod of the current
	// generation beside stale ones: it holds a place until the changeover ends.
	ChangeoverBegun ChangeoverState = "Begun"
	// ChangeoverDeferred means the group's new generation stands and its
	// remaining stale servers or pods wait only for their players: it holds
	// no place in the network's budget and gates no later stage.
	ChangeoverDeferred ChangeoverState = "Deferred"
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

	// FeedFormat is the line the agent's chat output wears: announcements
	// about the cloud and replies to a `/cloud` command alike.
	//
	// MiniMessage. `$EVENT_MESSAGE` is replaced by what the line has to say;
	// everything around it is yours.
	//
	// Changing it rolls no pod and takes effect within a resync interval. A
	// value that MiniMessage cannot parse makes the agent send the message
	// alone.
	// +kubebuilder:default="<gray>»</gray> <gradient:aqua:green>Spawnery</gradient> <dark_gray>|</dark_gray> <gray>$EVENT_MESSAGE"
	// +optional
	FeedFormat string `json:"feedFormat,omitempty"`
}

// ExtraPlugins names a volume whose contents are copied into the server's
// plugins directory on every start.
//
// **The claim's contents are the truth, on every start.** A plugin that
// rewrites its own configuration at runtime loses that change when the pod is
// replaced.
//
// Changing a claim's contents rolls nothing; a change takes effect when a
// server next starts. An image source's reference is part of the pod, so a
// new digest rolls the group.
// +kubebuilder:validation:XValidation:rule="has(self.claimName) != has(self.image)",message="extraPlugins: exactly one of claimName or image must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.pullPolicy) || has(self.image)",message="extraPlugins: pullPolicy applies to an image source only"
type ExtraPlugins struct {
	// ClaimName is a PersistentVolumeClaim in this object's own namespace.
	//
	// It must be ReadWriteMany, even for a single-replica group: a
	// ReadWriteOnce claim would leave the second server Pending on a volume
	// affinity error.
	// +kubebuilder:validation:MinLength=1
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// Image names an OCI image whose filesystem root is what the claim's root
	// would be. It is mounted read-only as an image volume and copied as a
	// claim is, pulled with the pod's imagePullSecrets. A digest reference
	// rolls the group whenever it changes; a tag does not.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Image string `json:"image,omitempty"`

	// PullPolicy for Image. Empty means IfNotPresent.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// ExtraFiles names a volume whose tree is copied into a server's working
// directory on every start.
//
// **The claim's contents are the truth, on every start**, as for
// ExtraPlugins. **A world in this claim is therefore overwritten on every
// start**; spec.storage and spec.mounts are what carry one.
//
// The entrypoint refuses a tree carrying `plugins/`, `eula.txt` or a file the
// renderer writes, so no two sources write the same file.
//
// Changing what the claim holds replaces no running server; a new file
// reaches one on its next start.
// +kubebuilder:validation:XValidation:rule="has(self.claimName) != has(self.image)",message="extraFiles: exactly one of claimName or image must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.pullPolicy) || has(self.image)",message="extraFiles: pullPolicy applies to an image source only"
type ExtraFiles struct {
	// ClaimName is a PersistentVolumeClaim in this object's own namespace.
	//
	// It must be ReadWriteMany: every pod of a group mounts it.
	// +kubebuilder:validation:MinLength=1
	// +optional
	ClaimName string `json:"claimName,omitempty"`

	// Image names an OCI image whose filesystem root is what the claim's root
	// would be. It is mounted read-only as an image volume and copied as a
	// claim is, pulled with the pod's imagePullSecrets. A digest reference
	// rolls the group whenever it changes; a tag does not.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Image string `json:"image,omitempty"`

	// PullPolicy for Image. Empty means IfNotPresent.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// GroupAttributes is what whoever runs a network wants every plugin in it to
// know about one group: which permission it is behind, which game it runs,
// whose it is.
//
// **The operator carries it and reads none of it.** It reaches the agents as
// part of the network picture and goes no further. Unlike what a server
// announces about itself, it is written by a person and changes only when
// somebody edits the group.
//
// Sixteen keys of at most 64 characters, with values of at most 256, the same
// bounds as a server's announcement.
//
// +kubebuilder:validation:MaxProperties=16
// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 64 && size(self[k]) <= 256)",message="an attribute name may be 64 characters and a value 256"
type GroupAttributes map[string]string

// Mount is a single file mount into a managed pod: a ConfigMap, a Secret, or
// a PersistentVolumeClaim.
//
// The claim carries what an image cannot: a world tree, a directory of assets
// every server reads, a pool one group writes and another reads.
//
// A mount is one volume at one path. There is no composition, priority or
// per-server rendering.
// +kubebuilder:validation:XValidation:rule="[has(self.configMap), has(self.secret), has(self.persistentVolumeClaim)].exists_one(x, x)",message="exactly one of configMap, secret or persistentVolumeClaim must be set"
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

	// PersistentVolumeClaim source. See MountClaim.
	// +optional
	PersistentVolumeClaim *MountClaim `json:"persistentVolumeClaim,omitempty"`

	// SubPath mounts one file or one subdirectory of the source instead of
	// the whole of it, so that MountPath can be a file.
	//
	// Without it, a mount whose path names a file gets a *directory* there
	// holding the source's keys as separate files.
	//
	// **A ConfigMap or Secret mounted through subPath does not update** in a
	// running pod; without subPath it does, eventually. No edit to a
	// ConfigMap's contents rolls anything.
	// +optional
	SubPath string `json:"subPath,omitempty"`
}

// MountClaim is a PersistentVolumeClaim mounted into a group's pods.
//
// Like ExtraPlugins it must be ReadWriteMany, even for a single-replica group.
// It needs the operator flag --allow-mount-volumes.
type MountClaim struct {
	// ClaimName is a PersistentVolumeClaim in this object's own namespace.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`

	// Writable mounts the claim read-write. It defaults to false.
	//
	// Nothing coordinates writers: two groups that both write the same claim
	// get what two processes writing one filesystem get.
	//
	// Flipping it replaces the group's servers.
	// +optional
	Writable bool `json:"writable,omitempty"`
}

// ReservedEnvPrefix is the prefix of every container environment variable the
// operator sets itself, and the one prefix a group's own spec.env may not use.
//
// Kubernetes keeps a duplicate name in a container's env list and the last
// entry silently wins, so the prefix is refused at admission instead.
//
// A kubebuilder marker cannot interpolate a constant, so the CEL rules on both
// spec.env fields repeat the literal;
// TestTheReservedEnvPrefixMarkersMatchTheConstant checks they agree.
const ReservedEnvPrefix = "SPAWNERY_"

// Substitution fills {{ NAME }} placeholders in the files copied from
// extraPlugins and extraFiles, at every start, from the container's
// environment. Only names beginning with Prefix are replaced, and one whose
// variable is missing stops the start.
type Substitution struct {
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Z0-9_]*$`
	Prefix string `json:"prefix"`
}
