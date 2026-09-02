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

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServerGroupType selects the operating mode of a group.
// +kubebuilder:validation:Enum=Ephemeral;Persistent
type ServerGroupType string

const (
	// ServerGroupEphemeral loses its state on stop: minigames and lobbies.
	ServerGroupEphemeral ServerGroupType = "Ephemeral"
	// ServerGroupPersistent keeps its world on a PVC: survival and creative.
	ServerGroupPersistent ServerGroupType = "Persistent"
)

// ScalingSpec drives slot-based scaling of ephemeral groups.
type ScalingSpec struct {
	// MinReplicas is the number of servers kept running at all times.
	// +kubebuilder:validation:Minimum=0
	MinReplicas int32 `json:"minReplicas"`

	// MaxReplicas caps the number of servers.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// SpareSlots is the number of free player slots kept available.
	// +kubebuilder:validation:Minimum=0
	SpareSlots int32 `json:"spareSlots"`

	// ScaleDownStabilizationSeconds is how long a server must be empty before
	// it is eligible for scale-down.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	// +optional
	ScaleDownStabilizationSeconds int32 `json:"scaleDownStabilizationSeconds,omitempty"`
}

// UpdateSpec controls the rolling update of ephemeral groups.
type UpdateSpec struct {
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
}

// DrainSpec bounds how long players may be moved off a server.
type DrainSpec struct {
	// TimeoutSeconds is the upper bound for the drain.
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds"`
}

// StorageSpec describes the PVC of a persistent group.
type StorageSpec struct {
	// Size of the volume. May grow, never shrink; actual expansion requires
	// allowVolumeExpansion on the StorageClass.
	Size resource.Quantity `json:"size"`

	// StorageClassName is immutable once set.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// AccessModes are immutable once set.
	// +kubebuilder:default={ReadWriteOnce}
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="!has(self.scaling) || self.scaling.minReplicas <= self.scaling.maxReplicas",message="scaling.minReplicas must not exceed scaling.maxReplicas"
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || !has(oldSelf.storage) || (has(self.storage.storageClassName) == has(oldSelf.storage.storageClassName) && (!has(self.storage.storageClassName) || self.storage.storageClassName == oldSelf.storage.storageClassName))",message="storage.storageClassName is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || !has(oldSelf.storage) || self.storage.accessModes == oldSelf.storage.accessModes",message="storage.accessModes is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.storage) || !has(oldSelf.storage) || quantity(self.storage.size).compareTo(quantity(oldSelf.storage.size)) >= 0",message="storage.size must not shrink"
type ServerGroupSpec struct {
	// NetworkRef names the Network this group belongs to.
	NetworkRef ObjectRef `json:"networkRef"`

	// Type selects ephemeral or persistent operation. Immutable.
	Type ServerGroupType `json:"type"`

	// Image is the Paper base image. A digest reference is recommended.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// MaxPlayers is the player capacity of a single server of this group.
	// +kubebuilder:validation:Minimum=1
	MaxPlayers int32 `json:"maxPlayers"`

	// Replicas is the fixed number of persistent servers. Ephemeral groups are
	// sized by scaling instead.
	//
	// Lowering it takes the top ordinal whoever is on it. A persistent server
	// is an identity and no other server can take its place, so unlike an
	// ephemeral group -- which shrinks around its players by picking an empty
	// server instead -- this one has no alternative to offer. The players on
	// that ordinal are protected by the ordinary drain and by nothing else:
	// they are moved through the proxies, and anyone still connected when
	// spec.drain.timeoutSeconds passes is disconnected with the pod.
	//
	// So if they must not be: empty the ordinal first, or raise
	// spec.drain.timeoutSeconds beforehand so the drain has room to finish.
	// docs/persistent-storage.md carries why refusing to shrink while anyone
	// is online would not be the better rule.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

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
	// to the ones the operator sets. A name may not begin with
	// ReservedEnvPrefix; see that constant for why the whole prefix is taken
	// rather than the individual names.
	//
	// This is also the only way to reach the JVM. The entrypoint execs java
	// with a fixed flag list and takes no arguments from any spec, so a
	// process-level setting travels in JAVA_TOOL_OPTIONS, which is the seam
	// the JVM itself offers. Measured on OpenJDK 21.0.12: an option on the
	// command line beats the same option in that variable, for -D and for
	// -Xmx alike, so a group can add the system property its plugins read
	// without being able to displace the heap and GC flags the entrypoint
	// sets. The case this exists for is a network whose game variants differ
	// by nothing else -- the same jars and the same configuration, one group
	// running them solo and another in teams, told apart by a single -D.
	//
	// It shapes the pod, so it is in podspec.DesiredServerHash: editing it
	// makes every server of the group stale and replaces them exactly the way
	// an image bump does, through maxUnavailable and the cold start. That is
	// the opposite of ExtraPlugins, whose contents deliberately reach no
	// hash, and the difference is that a filesystem the operator only names
	// cannot be digested while an env list it renders can.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.all(e, !e.name.startsWith('SPAWNERY_'))",message="the SPAWNERY_ prefix is reserved for the environment variables the operator sets itself"
	Env []corev1.EnvVar `json:"env,omitempty"`

	// ExtraPlugins names a volume whose plugins and their configuration are
	// copied into this group's servers on every start. See ExtraPlugins.
	// +optional
	ExtraPlugins *ExtraPlugins `json:"extraPlugins,omitempty"`

	// ExtraFiles names a volume whose tree is copied into this group's
	// servers on every start. See ExtraFiles.
	// +optional
	ExtraFiles *ExtraFiles `json:"extraFiles,omitempty"`

	// ConfigOverlay names a ConfigMap whose keys are configuration files to
	// merge over the rendered defaults — "server.properties",
	// "paper-global.yml", "paper-world-defaults.yml" or "velocity.toml", in
	// the target's own dialect.
	//
	// paper-world-defaults.yml is the one of those the operator writes only
	// when an overlay names it. Nothing in it is operationally critical, so
	// there is nothing to assert into it, and a file written empty on every
	// start would overwrite whatever the server had filled in for itself. It
	// is also the only route that file has: a mount cannot deliver it, because
	// a mount anywhere under /data/config stops the server writing its own
	// configuration and it never starts. See internal/podspec.ServerConfigDirPath.
	//
	// It is a field of its own rather than a reserved name inside mounts,
	// because mounts is documented as raw files for plugins and worlds and a
	// name-based convention is invisible until someone picks that name by
	// accident. It outranks the rendered defaults and is outranked by the
	// operationally critical fields, which nothing can reach.
	//
	// A key the receiving program does not declare is refused rather than
	// written. Paper and Velocity both keep their own default for a key they
	// do not read and write the stray one straight back out, so the rendered
	// file goes on looking like the override took while the setting never
	// applies -- which is the failure this refusal exists to prevent, and
	// which has cost this project two outages. The declared keys are measured
	// from each program's own default configuration, so a Paper or Velocity
	// bump can refuse a legitimately new key until that measurement is
	// retaken. server.properties is the exception: it has no such
	// measurement, so a mistyped key there is still only an unused one.
	// +optional
	ConfigOverlay *ObjectRef `json:"configOverlay,omitempty"`

	// Scaling configures slot-based scaling. Ephemeral only.
	//
	// Editing any of it replaces the group's servers. metadata.generation
	// moves on every spec change and a server of an older generation is
	// stale, so tuning a scaling knob marks every running server for
	// replacement even though nothing here shapes a pod. Nobody is kicked --
	// maxUnavailable and the cold start govern the changeover exactly as they
	// would for an image bump -- but it costs churn, and the likeliest moment
	// anyone reaches for these is a player spike, which is the worst time to
	// be replacing servers one at a time. Narrowing staleness to the fields
	// that actually shape a pod is a design change that has not been made.
	// +optional
	Scaling *ScalingSpec `json:"scaling,omitempty"`

	// Update configures the rolling update. Ephemeral only.
	// +optional
	Update *UpdateSpec `json:"update,omitempty"`

	// Storage configures the PVC. Persistent only.
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
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=0
	// +optional
	FailedRetentionSeconds int32 `json:"failedRetentionSeconds,omitempty"`
}

// ServerGroupStatus is the observed state of a ServerGroup.
type ServerGroupStatus struct {
	// Phase says whether players can join, and that is narrower than it
	// sounds: Ready means the group's *floor* is met -- spec.replicas for a
	// persistent group, spec.scaling.minReplicas for an ephemeral one -- not
	// that every server the scaler decided to run is up.
	//
	// The distinction is real and used to be invisible. An ephemeral group
	// scaled above its floor to cover spareSlots reports Ready as soon as the
	// floor is covered, with the rest still starting. Compare readyReplicas
	// against replicas to see that; both are printed columns for exactly this
	// reason.
	//
	// It is deliberately not "every decided server is up". A group scaling up
	// under load would then flip to Pending while thousands of players were
	// connected, and a group whose cluster has no capacity left for its
	// scaled target would sit at Pending forever while serving perfectly.
	// Ready answering "can somebody join" is the more useful of the two, and
	// this comment exists because the meaning of DesiredReplicas changed
	// underneath this field once already, in milestone 4a, without anything
	// saying so.
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

	// FreeSlots is the sum of free slots across ready servers rendered under
	// the group's current spec. Servers of an older one do not count.
	// +optional
	FreeSlots int32 `json:"freeSlots"`

	// BoostedReplicas is how much of this group's current floor comes from
	// ScaleBoost objects rather than from spec.scaling.minReplicas.
	//
	// It exists because the likeliest failure of a boost is not a wrong number
	// but an unexplained one: a group running four servers with a declared
	// floor of one and nothing anywhere saying why. A person meeting that will
	// edit the spec, which is the single thing that would not help -- the
	// boost is a separate object and the spec is not where it lives.
	//
	// Zero and present rather than absent, so that comparing two groups does
	// not mean telling "no boost" apart from "this operator is too old to
	// say".
	// +optional
	BoostedReplicas int32 `json:"boostedReplicas"`

	// ObservedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ConsecutiveFailures counts *rounds* in which at least one server failed
	// to start, with no success since. One pass adds one however many servers
	// it saw fail: counting servers meant a group with a floor of six spent
	// its whole budget in a single round, because the scaler creates the
	// shortfall in one pass. It lives on the CR rather than in the operator's memory because a
	// restart must not reset it: that would restart the create loop it exists
	// to bound. This is the opposite of the choice made for the empty-since
	// clock in milestone 4a, where a reset delays a scale-down and so errs
	// safely.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// LastFailureAt is the newest status.failedAt this group has counted. It
	// is what makes the count idempotent across resyncs, and the instant the
	// backoff window runs from.
	// +optional
	LastFailureAt *metav1.Time `json:"lastFailureAt,omitempty"`

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
// Replicas beside Ready, so the printed row carries a denominator. Without it
// `kubectl get servergroup` showed "Phase Ready, Ready 1" for a group running
// five, and nothing in the row said the other four were still starting.
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

// IsEphemeral reports whether this group is ephemeral.
func (g *ServerGroup) IsEphemeral() bool {
	return g.Spec.Type == ServerGroupEphemeral
}

// DesiredReplicas is the number of servers the group must have at minimum. For
// an ephemeral group it is the floor only: the size it actually runs at is
// DecideSize's, which reads this as one input among several.
func (g *ServerGroup) DesiredReplicas() int32 {
	if g.IsEphemeral() {
		if g.Spec.Scaling == nil {
			return 0
		}
		return g.Spec.Scaling.MinReplicas
	}
	if g.Spec.Replicas == nil {
		return 0
	}
	return *g.Spec.Replicas
}

// DrainTimeout is the configured drain timeout.
func (g *ServerGroup) DrainTimeout() time.Duration {
	if g.Spec.Drain == nil {
		return 60 * time.Second
	}
	return time.Duration(g.Spec.Drain.TimeoutSeconds) * time.Second
}

// FailedRetention is how long a Failed server is kept.
func (g *ServerGroup) FailedRetention() time.Duration {
	return time.Duration(g.Spec.FailedRetentionSeconds) * time.Second
}

// UpdateMaxUnavailable is how many servers a rolling update may have
// unavailable at once. A group with no spec.update gets the CRD's own default,
// so the rule is the same whether the field was written out or left off.
//
// spec.update is +optional with no CEL rule requiring it, so Spec.Update is
// nil for any Ephemeral group whose operator never wrote an update policy —
// and a nil parent means the field's own +kubebuilder:default=1 never
// applies. The CRD forbids 0 whenever spec.update is present (minimum 1), so
// a 0 reaching this method can only be that unset case, never a real operator
// choice. Floor it at 1 here: selectRetirement (internal/controller/scaling.go)
// treats unavailable >= budget as "no room to retire", and an unfloored 0
// would make that comparison true forever, so the group would silently never
// roll — no error, no condition, no event, just stale servers standing
// forever.
//
// selectRetirement floors the same value a second time, on its own copy in
// ScalingInputs.MaxUnavailable. That is deliberate, not redundant, and
// neither floor should be removed: this accessor is where a reader learns
// what an unset policy means, and selectRetirement's floor is what protects
// the pure rule from a future call site that builds ScalingInputs without
// going through this accessor.
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

func init() {
	SchemeBuilder.Register(&ServerGroup{}, &ServerGroupList{})
}
