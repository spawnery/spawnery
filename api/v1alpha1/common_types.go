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
	// ConditionOrdinalBlocked is true while a persistent ServerGroup cannot
	// create one of its ordinals because something else already holds that
	// exact name without being a member of the group.
	//
	// It exists because that state was permanent and completely silent. The
	// name of a persistent ordinal is derived, `<group>-<ordinal>`, so anything
	// created by hand under that name — or left behind by something else —
	// takes it. DecidePersistentSize reads spec.ordinal rather than parsing
	// names, so the squatter never enters its held map and the group believes
	// the ordinal is still missing; the create then returns AlreadyExists,
	// which the reconciler treats as success because that is the right answer
	// for the *other* cause of the same error, a cache that has not caught up
	// yet. So the group retried every five seconds, forever, with nothing on
	// its conditions, events or logs to say so.
	//
	// It carries a second, mirrored occasion, distinguished by the reason:
	// more than one Server of the group carrying the same spec.ordinal. There
	// an object holds the *ordinal* rather than the name, and it was silent in
	// the other direction -- DecidePersistentSize's held map is keyed by
	// ordinal, so the second server overwrote the first and the loser was
	// never surplus, never recreated and never named anywhere, while its pod
	// went on mounting a claim of its own. Reason tells the two apart:
	// OrdinalNameTaken and OrdinalCarriedByTwoServers.
	ConditionOrdinalBlocked = "OrdinalBlocked"
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
	// ConditionProgressing is true while a ServerGroup has not arrived where it
	// decided to be: a server of the current generation is still coming up, or
	// a server of an earlier one is still there.
	//
	// It exists because status.phase does not answer that and is deliberately
	// not being made to. Ready there means the group is serving, which is true
	// as soon as one server is up, and that is the useful thing for a printed
	// column to say. Whether the group has *arrived* is a second question, and
	// two states make the difference visible: an ephemeral group runs above
	// spec.scaling.minReplicas to cover spareSlots, so one ready server out of
	// five decided-upon satisfies the phase; and readyReplicas counts servers
	// of every generation, so a group mid-changeover whose new servers are all
	// still starting is Ready on the strength of the ones being replaced.
	//
	// This is the split a Deployment makes between Available and Progressing,
	// for the same reason, with one difference taken on purpose: True here
	// means "has not arrived" and nothing else. A group that has given up has
	// not arrived, so this stays True and Degraded/GaveUp beside it says it has
	// stopped trying. A Progressing that also went False for "stuck" -- the
	// reading a Deployment's ProgressDeadlineExceeded produces -- is the one
	// nobody can act on without reading a second field anyway.
	ConditionProgressing = "Progressing"
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
	// ConditionRescueWindowShort is true when the proxies serving this network
	// give up on a silent backend sooner than the operator can react to one.
	//
	// When a backend's node dies the operator has phase.RescueWindow to
	// deregister it and move its players; Velocity has its own read timeout,
	// and whichever fires first decides whether those players are moved or
	// disconnected outright. The window is that timeout less twice the agent
	// report interval, and both numbers are configurable by different people
	// in different places -- the interval is the operator's flag, the timeout
	// is a velocity.toml the operator never reads and a proxy reports on its
	// Hello.
	//
	// It is on the Network because that is where the two meet: proxies and
	// backends of one namespace. It is not folded into Accepted -- every group
	// gates on that, and a short rescue window must not stop a namespace from
	// running. Unknown is a real answer, for a namespace whose proxies have
	// not said.
	ConditionRescueWindowShort = "RescueWindowShort"
)

// Condition reasons.
const (
	ReasonDuplicateNetwork   = "DuplicateNetwork"
	ReasonNetworkNotFound    = "NetworkNotFound"
	ReasonNetworkNotAccepted = "NetworkNotAccepted"
	// ReasonNetworkPolicyNotWritten says the Network is otherwise acceptable
	// and its NetworkPolicy could not be written, so it is refused rather than
	// left unprotected. It is Accepted=False because every group gates on that
	// and the namespace must stay closed -- not because anything is wrong with
	// the Network itself.
	ReasonNetworkPolicyNotWritten = "NetworkPolicyNotWritten"
	// ReasonRescueWindowTooShort and ReasonRescueWindowSufficient are the two
	// answers ConditionRescueWindowShort has once a proxy has reported; before
	// that it is Unknown with ReasonNoProxyReported, which is not the same as
	// "sufficient" and must not be read as it.
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
	// ReasonOrdinalDuplicated says more than one Server of the group carries
	// the same spec.ordinal. The operator refuses to act on such an ordinal
	// rather than choose between two worlds, so this needs a person.
	ReasonOrdinalDuplicated = "OrdinalCarriedByTwoServers"
	ReasonOrdinalsAvailable = "OrdinalsAvailable"
	ReasonServersStarting   = "ServersStarting"
	ReasonReplacingServers  = "ReplacingServers"
	ReasonAtDesiredState    = "AtDesiredState"
	// ReasonRetireeStuck says a server carrying spec.retire has failed, and is
	// therefore holding an update slot until its retention window ends. The
	// changeover is stopped, not finished.
	ReasonRetireeStuck         = "RetireeFailedAndHoldsTheBudget"
	ReasonStorageResized       = "StorageResized"
	ReasonStorageResizeRefused = "StorageResizeRefused"
	// ReasonConfigMapNotOurs says something else occupies the name this group
	// renders its ConfigMap at. The operator refuses to write into an object
	// it does not own, and a group whose configuration cannot be written can
	// start no pod, so this is Degraded rather than a warning.
	ReasonConfigMapNotOurs = "ConfigMapNotOwnedByGroup"

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

	// The two spec.mounts claim reasons. They are separate from the
	// extraPlugins pair above even though one rule judges both claims,
	// because the remedy differs by which field somebody wrote: a person
	// reading MountVolumeUnusable goes and looks at spec.mounts, and a
	// shared reason would have sent them to a field their group may not even
	// set.
	//
	// ReasonMountVolumeUnusable says a spec.mounts entry names a claim that
	// is missing, or that cannot be mounted by every pod of the group.
	ReasonMountVolumeUnusable = "MountVolumeUnusable"
	// ReasonMountVolumesDisabled says a spec.mounts entry names a claim on an
	// installation whose operator was not started with
	// --allow-mount-volumes.
	ReasonMountVolumesDisabled = "MountVolumesDisabled"

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
//
// "On entering" is a property of the write, not only of the emit, and until
// 2026-08-24 it held only usually. Whether a pass is an entry is decided by the
// condition still in etcd, so emitting before the status update meant that
// anything failing in between — a pod List, a conflict, a refused write — left
// the old status behind and the retry announced the same transition again.
// NetworkReconciler now holds both events until the update lands, so a pass
// whose write fails announces nothing and the retry is entitled to announce it
// then. That is what makes this comment true rather than usual.
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

	// FeedFormat is the line the agent's chat output wears.
	//
	// It covers both halves of what the plugin says: an announcement about the
	// cloud and a reply to a `/cloud` command. One field and not two, because
	// they come from the same plugin and a network that styles one should not
	// have to style the other to match.
	//
	// MiniMessage, which both platforms parse -- see the agent's own Style for
	// why that rather than the deprecated section-sign codes. `$EVENT_MESSAGE`
	// is replaced by what the line has to say; everything around it is yours.
	// The token keeps its name because an installation has already written it
	// down.
	//
	// **Nothing about this reaches podspec.DesiredServerHash.** It travels in
	// the NetworkState the operator already sends, so changing it rolls no
	// pod and takes effect within a resync interval. A format carried in an
	// environment variable would have been in the pod spec, and re-wording a
	// chat line would have replaced every server on the network.
	//
	// A value that MiniMessage cannot parse costs the line, not the agent: the
	// agent falls back to the message alone rather than throwing inside a
	// network callback.
	// +kubebuilder:default="<gray>»</gray> <gradient:aqua:green>Spawnery</gradient> <dark_gray>|</dark_gray> <gray>$EVENT_MESSAGE"
	// +optional
	FeedFormat string `json:"feedFormat,omitempty"`
}

// ExtraPlugins names a volume whose contents are copied into the server's
// plugins directory on every start.
//
// **The claim's contents are the truth, on every start.** A plugin that
// rewrites its own configuration at runtime loses that change when the pod is
// replaced. For an ephemeral group it would lose it anyway -- spec.type
// Ephemeral gives /data an emptyDir -- so this costs nothing there and makes
// the persistent case predictable rather than accumulating.
//
// Nothing about the contents reaches podspec.DesiredServerHash: the operator
// holds a claim name, not a filesystem. So changing a plugin does not roll a
// fleet, which is the point of this field existing -- and a change therefore
// takes effect when the group next restarts, which somebody triggers.
type ExtraPlugins struct {
	// ClaimName is a PersistentVolumeClaim in this object's own namespace.
	//
	// It must be ReadWriteMany. A ReadWriteOnce claim mounts on one node, so
	// the second server of a group would sit Pending with a scheduling error
	// naming volume affinity rather than the actual cause; the operator
	// refuses it instead. That refusal also catches a single-replica group,
	// for which ReadWriteOnce would in fact work -- the simpler rule was
	// chosen because maxReplicas can be raised by an edit that has nothing to
	// do with storage, and a group that worked until somebody scaled it is a
	// worse failure than one that never started.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`
}

// ExtraFiles names a volume whose tree is copied into a server's working
// directory on every start.
//
// **The claim's contents are the truth, on every start.** Word for word the
// ExtraPlugins rule, and for the same reason: a server that rewrote one of
// these files finds the administrator's version back in place next start,
// which is what makes the claim the truth rather than a first-boot seed.
//
// **A world in this claim is therefore overwritten on every start**, and a
// world does not belong in it. spec.storage and spec.mounts are what carry
// one. That consequence follows from the rule above rather than qualifying
// it, and it is the one worth reading twice before pointing this field at a
// claim somebody was already using for something else.
//
// It is ExtraPlugins one directory up. ExtraPlugins reaches /data/plugins and
// nothing else, so a plugin whose configuration lives elsewhere -- Sponge
// reads config/sponge/sponge.conf -- could not be configured without an image.
// A mount cannot deliver there either: see ServerConfigDirPath, whose comment
// carries the kubelet-ownership measurement that rules it out for good.
//
// The entrypoint refuses a tree carrying a path another owner writes, so this
// volume, the renderer and ExtraPlugins never write the same file and the
// order between them cannot decide the result.
//
// Nothing about the contents reaches podspec.DesiredServerHash: the operator
// holds a claim name, not a filesystem, and a filesystem it only names cannot
// be digested. Changing what the claim holds therefore replaces no running
// server; a new file reaches one on its next start, which somebody triggers.
type ExtraFiles struct {
	// ClaimName is a PersistentVolumeClaim in this object's own namespace.
	//
	// It must be ReadWriteMany, for the reason ExtraPlugins.ClaimName gives:
	// every pod of a group mounts it, and a ReadWriteOnce claim would leave
	// the second server Pending with a scheduling error naming volume
	// affinity rather than the cause.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`
}

// GroupAttributes is what whoever runs a network wants every plugin in it to
// know about one group.
//
// **The operator carries it and reads none of it.** No decision it makes looks
// at a key: not scheduling, not routing, not scaling. It reaches the agents as
// part of the network picture they already receive and goes no further, which
// is what makes free-form text safe here -- a field the operator acted on
// would need a schema, a validation error path and a version story, and a
// field it only carries needs a length bound.
//
// It is the counterpart of what a server announces about itself, and the
// difference is who writes it. A server describes what it is doing right now
// and changes its mind every round; this is written by a person in the group's
// own definition, reviewed like anything else there, and changes when somebody
// edits it. A plugin that needs to know something about a group that nobody
// could derive from its servers -- which permission it is behind, which of
// several games it runs, whose it is -- reads it here rather than asking every
// server of the group and hoping they agree.
//
// The bounds match the announcement's, and matching is the point: two ways of
// carrying a handful of strings that stopped at different sizes would be two
// rules for a reader to remember. Sixteen keys of at most 64 characters, with
// values of at most 256. This is copied into every agent's picture on every
// resync, so what it costs is paid by every pod for as long as the network
// runs.
//
// +kubebuilder:validation:MaxProperties=16
// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 64 && size(self[k]) <= 256)",message="an attribute name may be 64 characters and a value 256"
type GroupAttributes map[string]string

// Mount is a single file mount into a managed pod: a ConfigMap, a Secret, or
// a PersistentVolumeClaim.
//
// The claim is what carries the things an image cannot and `extraPlugins` will
// not: a world tree, a directory of assets every server reads, a pool one
// group writes and another reads. `extraPlugins` is deliberately narrow -- one
// claim, read-only, copied into `plugins/` by the entrypoint -- and everything
// outside `plugins/` needed somewhere to go. docs/mounts.md carries what each
// of those routes is and the one that is refused.
//
// It is still not a layered template system. There is no composition, no
// priority, no per-server rendering: a mount is one volume at one path, and
// what assembles the volume's contents is somebody else's job.
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
	// Without it, a mount whose path names a file gets a *directory* there --
	// the source's keys as separate files inside it. A server looking for
	// /data/bukkit.yml then finds a directory called bukkit.yml, and what it
	// reports is a parse error rather than anything about a mount. Landing a
	// single file beside the ones the server writes itself is the case this
	// exists for; there is no other way to do it.
	//
	// **A ConfigMap or Secret mounted through subPath does not update.**
	// Kubernetes refreshes a projected volume in place, and a subPath mount is
	// a bind of one file out of it that the kubelet does not re-point. Editing
	// the ConfigMap changes nothing in a running pod, and nothing reports
	// that. Without subPath the file does update, eventually and without a
	// restart -- which is a real difference and the reason this is a field
	// somebody opts into rather than something inferred from the path looking
	// like a file. Neither behaviour reaches the pod digest either way, so no
	// edit to a ConfigMap's contents rolls anything.
	// +optional
	SubPath string `json:"subPath,omitempty"`
}

// MountClaim is a PersistentVolumeClaim mounted into a group's pods.
//
// Like ExtraPlugins it must be ReadWriteMany, for the reason that field's own
// comment gives at length: every pod of the group mounts it, they are spread
// across nodes, and a ReadWriteOnce claim would leave the second one Pending
// on a scheduling error about volume affinity with nothing naming the claim.
// The rule does not soften for a group that happens to run one replica today,
// because maxReplicas is raised by edits that have nothing to do with storage.
//
// It is gated by its own --allow-mount-volumes rather than
// --allow-plugin-volumes: until this field existed, the plugin flag governed
// spec.mounts too, and its refusal said so -- which the flag's name never
// promised. The flag is still not a security boundary -- a claim is a
// namespaced object in the same trust domain as the group naming it -- and
// docs/plugins.md says so at more length.
type MountClaim struct {
	// ClaimName is a PersistentVolumeClaim in this object's own namespace.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`

	// Writable mounts the claim read-write. It defaults to false, and the
	// field is spelled this way round -- Writable rather than ReadOnly --
	// so that the zero value is the safe one. An omitted field, a field
	// somebody has not heard of, and a field lost in a hand-written manifest
	// all land on read-only.
	//
	// One group writing a volume that others read is the case this exists
	// for: a generator filling a pool of worlds that game servers consume.
	// Nothing here coordinates that. Two groups that both write the same
	// claim get exactly what two processes writing one filesystem get, and
	// the operator has no way to know which of them meant to.
	//
	// It reaches the pod, so flipping it replaces the group's servers.
	// +optional
	Writable bool `json:"writable,omitempty"`
}

// ReservedEnvPrefix is the prefix of every container environment variable the
// operator sets itself, and the one prefix a group's own spec.env may not use.
//
// The operator writes SPAWNERY_NETWORK, SPAWNERY_GROUP and either
// SPAWNERY_SERVER or SPAWNERY_PROXY into every pod it renders, along with the
// agent's endpoint and, for a proxy, its player limit and fallback groups. The
// agent reads them to know what it is and whom to call. Without them it never
// connects, and what an installation sees is a server that starts, stays
// NotReady, and says nothing about why.
//
// Kubernetes does not refuse a duplicate name in a container's env list. It
// keeps both entries and the last one wins, so a group setting SPAWNERY_GROUP
// would leave `kubectl describe pod` printing both values with nothing on the
// pod saying which one the process actually got. Refusing the prefix at
// admission turns that into an error on the object somebody just wrote.
//
// The prefix is reserved whole rather than the six names being denied one by
// one, so a variable added to a pod in a later release needs no change here
// and cannot collide with something an installation already set. Reserving it
// whole also covers the two seams the entrypoints read: SPAWNERY_PLUGIN_SOURCE
// and SPAWNERY_CGROUP_ROOT exist so the image tests can point them at a
// temporary directory, and setting either from a group spec would break a
// start in a way nothing reports.
//
// The rule is a CEL expression on both spec.env fields, so the API server
// refuses the object rather than the operator finding it afterwards. The
// literal is repeated in those markers because a kubebuilder marker cannot
// interpolate a constant; TestTheReservedEnvPrefixMarkersMatchTheConstant
// reads the generated CRDs and checks that all of them still agree with this.
const ReservedEnvPrefix = "SPAWNERY_"
