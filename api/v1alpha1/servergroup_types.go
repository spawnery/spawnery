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

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServerGroupType selects the operating mode of a group.
// +kubebuilder:validation:Enum=Ephemeral;Persistent;OnDemand
type ServerGroupType string

const (
	// ServerGroupEphemeral loses its state on stop: minigames and lobbies.
	ServerGroupEphemeral ServerGroupType = "Ephemeral"
	// ServerGroupPersistent keeps its world on a PVC: survival and creative.
	ServerGroupPersistent ServerGroupType = "Persistent"
	// ServerGroupOnDemand is a template whose members are asked for by name
	// rather than counted: one world per key, started when somebody asks and
	// gone when they are done. The group itself never creates one.
	ServerGroupOnDemand ServerGroupType = "OnDemand"
)

// +kubebuilder:validation:Enum=Required;DenyOnly
type JoinPermissionMode string

const (
	JoinPermissionRequired JoinPermissionMode = "Required"
	JoinPermissionDenyOnly JoinPermissionMode = "DenyOnly"
)

// JoinPermission is the permission that decides who may join a group's
// servers. It is read at runtime over the agent channel; changing it
// restarts no server.
type JoinPermission struct {
	// Node is the permission node. Empty means spawnery.join.<group name>.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-z0-9_.-]+$`
	// +optional
	Node string `json:"node,omitempty"`

	// Mode Required admits only players who hold the node. DenyOnly admits
	// everyone except players for whom the node is explicitly set to false.
	// +kubebuilder:default=Required
	// +optional
	Mode JoinPermissionMode `json:"mode,omitempty"`
}

func (j *JoinPermission) ResolvedNode(group string) string {
	if j == nil {
		return ""
	}
	if j.Node != "" {
		return j.Node
	}
	return "spawnery.join." + group
}

func (j *JoinPermission) DenyOnly() bool {
	return j != nil && j.Mode == JoinPermissionDenyOnly
}

// AnnotationRetry on a ServerGroup resets its failure streak whenever its
// value changes. It is the way to retry after fixing a cause outside the
// group, such as a Secret or a registry, which moves nothing the operator
// reads.
const AnnotationRetry = "spawnery.cloud/retry"

// ScalingSpec drives slot-based scaling of ephemeral groups.
type ScalingSpec struct {
	// MinReplicas is the number of servers kept running at all times.
	// +kubebuilder:validation:Minimum=0
	MinReplicas int32 `json:"minReplicas"`

	// MaxReplicas caps the number of servers.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// SpareSlots is the number of free player slots kept available, counted
	// in playable seats when the group sets playableSlots.
	// +kubebuilder:validation:Minimum=0
	SpareSlots int32 `json:"spareSlots"`

	// ScaleDownStabilizationSeconds is how long a server must be empty before
	// it is eligible for scale-down.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	// +optional
	ScaleDownStabilizationSeconds int32 `json:"scaleDownStabilizationSeconds,omitempty"`
}

// UpdateStrategy is how a changeover replaces stale servers.
// +kubebuilder:validation:Enum=RollingUpdate;WhenEmpty
type UpdateStrategy string

const (
	// UpdateRollingUpdate retires stale servers whether or not they have
	// players; the players stay until they leave.
	UpdateRollingUpdate UpdateStrategy = "RollingUpdate"
	// UpdateWhenEmpty retires only stale servers known to be empty. An
	// occupied stale server stays Ready and joinable until it empties.
	UpdateWhenEmpty UpdateStrategy = "WhenEmpty"
)

// UpdateSpec controls the rolling update of ephemeral groups.
// +kubebuilder:validation:XValidation:rule="!has(self.strategy) || self.strategy != 'WhenEmpty' || !has(self.maxStaleSeconds) || self.maxStaleSeconds == 0",message="spec.update.maxStaleSeconds must be 0 with strategy WhenEmpty: it drains the occupied servers WhenEmpty leaves alone"
type UpdateSpec struct {
	// Strategy is how stale servers are replaced.
	// +kubebuilder:default=RollingUpdate
	// +optional
	Strategy UpdateStrategy `json:"strategy,omitempty"`

	// MaxUnavailable is how many servers may be draining or terminating at the
	// same time because of a generation change.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxUnavailable int32 `json:"maxUnavailable,omitempty"`

	// MaxStaleSeconds forces an active drain of stale servers after this many
	// seconds. 0 means stale servers are never actively emptied.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxStaleSeconds int32 `json:"maxStaleSeconds,omitempty"`

	// MinAvailable is how many servers must stay joinable while the group
	// changes over: Ready, registered, door open, and not on their way out,
	// of either generation. The group builds one extra server at a time to
	// keep it. Unset keeps no floor beyond one Ready server of the current
	// generation.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinAvailable *int32 `json:"minAvailable,omitempty"`
}

// DrainSpec bounds how long players may be moved off a server.
type DrainSpec struct {
	// TimeoutSeconds is the upper bound for the drain.
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds"`
}

// StorageSpec describes the PVC of a persistent or on-demand group.
type StorageSpec struct {
	// Size of each new claim. Raising it grows existing claims up to the new
	// size (needs allowVolumeExpansion on the StorageClass); lowering it
	// changes only claims created afterwards and never shrinks one. A claim
	// larger than this, grown by hand or by an autoresizer, is left alone.
	Size resource.Quantity `json:"size"`

	// Annotations are copied onto each data claim when it is created, e.g. for
	// a volume autoresizer's per-claim ceiling. Existing claims are not changed.
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?(\\\\.[A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$') && k.split('/')[size(k.split('/')) - 1].size() <= 63 && (!k.contains('/') || k.split('/')[0].size() <= 253))",message="every key must be a valid Kubernetes annotation key: an optional DNS subdomain prefix and '/', then a name of at most 63 characters"
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// StorageClassName is immutable once set.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// AccessModes are immutable once set.
	// +kubebuilder:default={ReadWriteOnce}
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// Keep lists the paths on the data claim, relative to /data, that survive
	// a start. When set, each start first deletes everything on the claim that
	// no entry matches, then renders and copies as before. Unset, everything
	// is kept.
	//
	// An entry is a path whose segments may use * and ? (path.Match per
	// segment). A matched directory is kept whole. Mount points, their parent
	// directories and the root lost+found are never deleted. The start is
	// refused when a path no entry keeps is, or holds, a level.dat* file, a
	// region directory or an .mca file, unless extraFiles and extraPlugins
	// ship every file and directory at and below it at the same path, which
	// the copy then writes back.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:XValidation:rule="!self.startsWith('/') && !self.contains('[') && !self.contains(']') && !self.contains('\\\\') && !self.contains('\\n') && !self.contains('\\r') && self.split('/').all(s, s != '' && s != '.' && s != '..')",message="a keep entry is a relative path without [ ] \\, line breaks or empty, . and .. segments"
	// +optional
	Keep []string `json:"keep,omitempty"`
}

// ServerGroupSpec describes a group of Minecraft servers.
// +kubebuilder:validation:XValidation:rule="self.type == oldSelf.type",message="spec.type is immutable"
// +kubebuilder:validation:XValidation:rule="self.type != 'Ephemeral' || !has(self.storage)",message="spec.storage is not allowed for type Ephemeral"
// +kubebuilder:validation:XValidation:rule="self.type != 'Ephemeral' || !has(self.replicas)",message="spec.replicas is not allowed for type Ephemeral"
// +kubebuilder:validation:XValidation:rule="self.type != 'Ephemeral' || has(self.scaling)",message="spec.scaling is required for type Ephemeral"
// +kubebuilder:validation:XValidation:rule="self.type != 'Persistent' || !has(self.scaling)",message="spec.scaling is not allowed for type Persistent"
// +kubebuilder:validation:XValidation:rule="self.type != 'Persistent' || !has(self.update)",message="spec.update is not allowed for type Persistent"
// +kubebuilder:validation:XValidation:rule="self.type != 'Persistent' || has(self.storage)",message="spec.storage is required for type Persistent"
// +kubebuilder:validation:XValidation:rule="self.type != 'Persistent' || has(self.replicas)",message="spec.replicas is required for type Persistent"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || !has(self.scaling)",message="spec.scaling is not allowed for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || !has(self.replicas)",message="spec.replicas is not allowed for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || !has(self.update)",message="spec.update is not allowed for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || has(self.storage)",message="spec.storage is required for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || has(self.maxInstances)",message="spec.maxInstances is required for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type == 'OnDemand' || !has(self.maxInstances)",message="spec.maxInstances is only allowed for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || !has(self.changeoverStage) || self.changeoverStage == 0",message="spec.changeoverStage is not allowed for type OnDemand"
// +kubebuilder:validation:XValidation:rule="!has(self.scaling) || self.scaling.minReplicas <= self.scaling.maxReplicas",message="scaling.minReplicas must not exceed scaling.maxReplicas"
// +kubebuilder:validation:XValidation:rule="!has(self.update) || !has(self.update.minAvailable) || !has(self.scaling) || self.update.minAvailable < self.scaling.maxReplicas",message="spec.update.minAvailable must be less than spec.scaling.maxReplicas: keeping the floor needs room for one extra server"
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || !has(oldSelf.storage) || (has(self.storage.storageClassName) == has(oldSelf.storage.storageClassName) && (!has(self.storage.storageClassName) || self.storage.storageClassName == oldSelf.storage.storageClassName))",message="storage.storageClassName is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || !has(oldSelf.storage) || self.storage.accessModes == oldSelf.storage.accessModes",message="storage.accessModes is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.playableSlots) || (self.playableSlots >= 1 && self.playableSlots <= self.maxPlayers)",message="spec.playableSlots must be between 1 and spec.maxPlayers"
type ServerGroupSpec struct {
	// NetworkRef names the Network this group belongs to.
	NetworkRef ObjectRef `json:"networkRef"`

	// Type selects ephemeral, persistent or on-demand operation. Immutable.
	Type ServerGroupType `json:"type"`

	// Image is the Paper base image. A digest reference is recommended.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// MaxPlayers is the player capacity of a single server of this group.
	// +kubebuilder:validation:Minimum=1
	MaxPlayers int32 `json:"maxPlayers"`

	// PlayableSlots is how many seats of each server count as capacity: what
	// spareSlots, status.freeSlots and a connect to the group measure. Unset,
	// every seat up to maxPlayers counts. A plugin can set its own server's
	// figure at runtime, which wins over this one.
	//
	// maxPlayers stays the limit a server enforces, so the seats between the
	// two are room for players the group is not sized by, such as spectators --
	// unless enforcePlayableSlots makes this number the limit too.
	// +optional
	PlayableSlots *int32 `json:"playableSlots,omitempty"`

	// EnforcePlayableSlots refuses a login once a server holds as many players
	// as its playable slots, except for players with the permission
	// spawnery.join.full.<group>, who never take a seat. Read at runtime over
	// the agent channel; changing it restarts no server.
	// +optional
	EnforcePlayableSlots bool `json:"enforcePlayableSlots,omitempty"`

	// JoinPermission limits who may join this group's servers. A proxy routes
	// a player around a group they may not join; the server refuses the login.
	// On an OnDemand group only the proxy checks it. Agents older than 0.18.0
	// ignore the rule, so the game and proxy images must carry 0.18.0 or later.
	// +optional
	JoinPermission *JoinPermission `json:"joinPermission,omitempty"`

	// Replicas is the fixed number of persistent servers. Ephemeral groups are
	// sized by scaling instead.
	//
	// Lowering it takes the top ordinal whoever is on it. Its players are
	// moved by the ordinary drain, and anyone still connected when
	// spec.drain.timeoutSeconds passes is disconnected with the pod. Empty the
	// ordinal first or raise spec.drain.timeoutSeconds beforehand.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// MaxInstances is how many members this group may have at once.
	//
	// A fleet ceiling, not a per-player quota. Zero closes the group: new
	// starts are refused and every world stays where it is. Required, with no
	// default.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxInstances *int32 `json:"maxInstances,omitempty"`

	// Resources overrides Network.spec.defaults.resources.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling overrides Network.spec.defaults.scheduling.
	// +optional
	Scheduling *Scheduling `json:"scheduling,omitempty"`

	// Mounts are extra ConfigMap and Secret mounts.
	// +optional
	// +listType=map
	// +listMapKey=name
	Mounts []Mount `json:"mounts,omitempty"`

	// Env are extra environment variables for the server container, appended
	// to the ones the operator sets. A name may not begin with SPAWNERY_.
	//
	// JVM options go in JAVA_TOOL_OPTIONS. The entrypoint's own command-line
	// flags win over the same option there, so a group can add a -D but not
	// displace the heap and GC flags.
	//
	// Editing it replaces every server of the group, like an image bump. A
	// valueFrom reference is digested, not its value: rotating the Secret or
	// ConfigMap reaches only new pods.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.all(e, !e.name.startsWith('SPAWNERY_'))",message="the SPAWNERY_ prefix is reserved for the environment variables the operator sets itself"
	Env []corev1.EnvVar `json:"env,omitempty"`

	// DisplayName is what this group is called where a person reads it:
	// a scoreboard, a chat message, a playtime key.
	//
	// The operator carries it and reads none of it. Empty, a plugin shows the
	// group's own name.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	DisplayName string `json:"displayName,omitempty"`

	// Attributes is what plugins are told about this group. See
	// GroupAttributes. Editing it replaces and restarts nothing.
	// +optional
	Attributes GroupAttributes `json:"attributes,omitempty"`

	// ExtraPlugins names a volume whose plugins and their configuration are
	// copied into this group's servers on every start. See ExtraPlugins.
	// +optional
	ExtraPlugins *ExtraPlugins `json:"extraPlugins,omitempty"`

	// ExtraFiles names a volume whose tree is copied into this group's
	// servers on every start, replacing what the server wrote there. A world
	// does not belong in it. See ExtraFiles.
	// +optional
	ExtraFiles *ExtraFiles `json:"extraFiles,omitempty"`

	// Substitution fills placeholders in the files the entrypoint copies from
	// extraPlugins and extraFiles. See Substitution.
	// +optional
	Substitution *Substitution `json:"substitution,omitempty"`

	// ConfigOverlay names a ConfigMap whose keys are configuration files to
	// merge over the rendered defaults — "server.properties",
	// "paper-global.yml", "paper-world-defaults.yml" or "velocity.toml", in
	// the target's own dialect.
	//
	// paper-world-defaults.yml is written only when an overlay names it, and
	// this is its only route: a mount under /data/config keeps the server
	// from starting.
	//
	// It outranks the rendered defaults and is outranked by the operationally
	// critical fields.
	//
	// A key the receiving program does not declare is refused, since Paper and
	// Velocity silently ignore it. The declared keys come from each program's
	// captured default configuration, so a Paper or Velocity bump can refuse a
	// new key until that capture is updated. server.properties keys are not
	// checked.
	// +optional
	ConfigOverlay *ObjectRef `json:"configOverlay,omitempty"`

	// Scaling configures slot-based scaling. Ephemeral only. Editing it
	// replaces no server.
	// +optional
	Scaling *ScalingSpec `json:"scaling,omitempty"`

	// ChangeoverStage orders this group's changeover against the network's
	// other groups: a group waits while any group of a lower stage is still
	// changing over. Groups of one stage change over together, within
	// Network.spec.update.maxConcurrentChangeovers. Not for type OnDemand.
	// +optional
	ChangeoverStage int32 `json:"changeoverStage,omitempty"`

	// Update configures the rolling update. Ephemeral only.
	// +optional
	Update *UpdateSpec `json:"update,omitempty"`

	// Storage configures the PVC. Persistent and OnDemand only.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`

	// Drain bounds how long players may be moved off a server.
	// +kubebuilder:default={timeoutSeconds:60}
	// +optional
	Drain *DrainSpec `json:"drain,omitempty"`

	// TerminationGracePeriodSeconds is the time the pod gets to save its world.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	// +optional
	TerminationGracePeriodSeconds int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// FailedRetentionSeconds is how long a Failed server is kept for diagnosis.
	// A pod that comes up after its server failed is stopped as soon as another
	// server of the group is Ready; the Server object stays for the retention.
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=0
	// +optional
	FailedRetentionSeconds int32 `json:"failedRetentionSeconds,omitempty"`

	// FinishedRetentionSeconds is how long a Finished server is kept.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	// +optional
	FinishedRetentionSeconds int32 `json:"finishedRetentionSeconds,omitempty"`
}

// ServerGroupStatus is the observed state of a ServerGroup.
type ServerGroupStatus struct {
	// Phase says whether players can join: Ready means the group's *floor* is
	// met -- spec.replicas for a persistent group, spec.scaling.minReplicas
	// for an ephemeral one -- not that every server the scaler decided to run
	// is up. Compare readyReplicas against replicas for that, or see the
	// Progressing condition.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Replicas is the number of Server objects owned by this group.
	// +optional
	Replicas int32 `json:"replicas"`

	// ReadyReplicas is the number of servers in phase Ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// OnlinePlayers is the sum of players across all ready servers.
	// +optional
	OnlinePlayers int32 `json:"onlinePlayers"`

	// FreeSlots is the number of seats a proxy can send a player to right
	// now: the free playable seats of servers that are Ready, in the proxies'
	// routing tables, accepting joins, and rendered under the group's current
	// spec. It is the scaler's input.
	// +optional
	FreeSlots int32 `json:"freeSlots"`

	// BoostedReplicas is how much of this group's current floor comes from
	// ScaleBoost objects rather than from spec.scaling.minReplicas.
	// +optional
	BoostedReplicas int32 `json:"boostedReplicas"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ConsecutiveFailures counts *rounds* in which at least one server failed
	// to start, with no success since. One pass adds one however many servers
	// it saw fail.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// LastFailureAt is the newest status.failedAt this group has counted. It
	// is what makes the count idempotent across resyncs, and the instant the
	// backoff window runs from.
	// +optional
	LastFailureAt *metav1.Time `json:"lastFailureAt,omitempty"`

	// FailureStreakKey identifies what the servers counted in
	// consecutiveFailures started with: the desired pod hash, the
	// configOverlay ConfigMap's resourceVersion and the spawnery.cloud/retry
	// annotation. The streak is reset when any of them moves, and by nothing
	// else. Opaque; compare it, never parse it.
	// +optional
	FailureStreakKey string `json:"failureStreakKey,omitempty"`

	// Changeover is this group's changeover as the network's budget sees it;
	// written by its own reconcile and read by its siblings'.
	// +optional
	// +kubebuilder:validation:Enum="";Waiting;Begun;Deferred
	Changeover ChangeoverState `json:"changeover,omitempty"`

	// Conditions follow the standard Kubernetes condition contract.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcgroup
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.onlinePlayers`
// +kubebuilder:printcolumn:name="Free Slots",type=integer,JSONPath=`.status.freeSlots`
// +kubebuilder:printcolumn:name="Boosted",type=integer,JSONPath=`.status.boostedReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ServerGroup is a group of interchangeable Minecraft servers.
type ServerGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServerGroupSpec   `json:"spec,omitempty"`
	Status ServerGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServerGroupList contains a list of ServerGroup.
type ServerGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServerGroup `json:"items"`
}

func (g *ServerGroup) IsEphemeral() bool {
	return g.Spec.Type == ServerGroupEphemeral
}

// IsOnDemand reports whether this group's members are asked for by name.
func (g *ServerGroup) IsOnDemand() bool {
	return g.Spec.Type == ServerGroupOnDemand
}

// DesiredReplicas is the number of servers the group must have at minimum. For
// an ephemeral group it is the floor only; DecideSize decides the actual size.
func (g *ServerGroup) DesiredReplicas() int32 {
	if g.IsEphemeral() {
		if g.Spec.Scaling == nil {
			return 0
		}
		return g.Spec.Scaling.MinReplicas
	}
	if g.IsOnDemand() {
		return 0
	}
	if g.Spec.Replicas == nil {
		return 0
	}
	return *g.Spec.Replicas
}

func (g *ServerGroup) DrainTimeout() time.Duration {
	if g.Spec.Drain == nil {
		return 60 * time.Second
	}
	return time.Duration(g.Spec.Drain.TimeoutSeconds) * time.Second
}

func (g *ServerGroup) FailedRetention() time.Duration {
	return time.Duration(g.Spec.FailedRetentionSeconds) * time.Second
}

func (g *ServerGroup) FinishedRetention() time.Duration {
	return time.Duration(g.Spec.FinishedRetentionSeconds) * time.Second
}

// UpdateMaxUnavailable is how many servers a rolling update may have
// unavailable at once. With spec.update unset its +kubebuilder:default=1 never
// applies, so the 0 is floored here; a 0 would block every roll.
// selectRetirement floors its own copy again, for callers that bypass this.
func (g *ServerGroup) UpdateMaxUnavailable() int32 {
	if g.Spec.Update == nil || g.Spec.Update.MaxUnavailable < 1 {
		return 1
	}
	return g.Spec.Update.MaxUnavailable
}

// UpdateMaxStale is how long a server may wait in soft drain before its
// players are moved off. Zero means never.
func (g *ServerGroup) UpdateMaxStale() time.Duration {
	if g.Spec.Update == nil {
		return 0
	}
	return time.Duration(g.Spec.Update.MaxStaleSeconds) * time.Second
}

func (g *ServerGroup) UpdateWhenEmpty() bool {
	return g.Spec.Update != nil && g.Spec.Update.Strategy == UpdateWhenEmpty
}

// UpdateMinAvailable is spec.update.minAvailable, 0 when unset.
func (g *ServerGroup) UpdateMinAvailable() int32 {
	if g.Spec.Update == nil || g.Spec.Update.MinAvailable == nil {
		return 0
	}
	return *g.Spec.Update.MinAvailable
}

func init() {
	SchemeBuilder.Register(&ServerGroup{}, &ServerGroupList{})
}
