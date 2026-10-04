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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ServerSpec describes one running Minecraft server instance. It is created
// and owned by a ServerGroup and is not meant to be edited by hand.
type ServerSpec struct {
	// GroupRef names the owning ServerGroup.
	GroupRef ObjectRef `json:"groupRef"`

	// Ordinal is the stable index of a persistent server. Unset for ephemeral
	// servers, whose names carry a random suffix instead.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Ordinal *int32 `json:"ordinal,omitempty"`

	// Key is the caller's name for the world this member carries, and it is
	// set for a member of an OnDemand group and for no other server. Like
	// Ordinal, it lets a Server say what kind of member it is once its group
	// is gone.
	// +optional
	Key string `json:"key,omitempty"`

	// Number is which of its group's servers this is, counted the way a
	// person counts: the second hub is 2. The group assigns it once, at
	// creation, and it is given out again only after this server is gone.
	//
	// A persistent server's Number equals its Ordinal. Zero means unnumbered:
	// a server older than this field, or ordinal zero of a persistent group.
	// A reader then falls back to the server's name.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Number int32 `json:"number,omitempty"`

	// GroupGeneration is the metadata.generation of the group at creation
	// time. A server whose value is behind the group's is stale.
	// +optional
	GroupGeneration int64 `json:"groupGeneration,omitempty"`

	// PodHash is podspec.DesiredServerHash at the moment this server was
	// created: a digest of everything the operator would render for it. The
	// group compares it against a freshly computed one to decide whether this
	// ordinal is running the current spec.
	//
	// Empty means adopt, never stale: the group stamps the current hash onto
	// such a server and orders no takedown.
	// +optional
	PodHash string `json:"podHash,omitempty"`

	// Retire asks this server to stop taking joins and empty out, without its
	// players being moved. The ServerGroup controller sets it during a rolling
	// update, and an administrator through the agent endpoint's retire
	// request. A server counts against spec.update.maxUnavailable while this
	// is true.
	// +optional
	Retire bool `json:"retire,omitempty"`

	// Hold keeps this server against every automatic removal: a rolling
	// update does not retire it, and neither the demand rule nor a lowered
	// maxReplicas deletes it. It stays until it ends by itself. Set by the
	// agent endpoint's unretire request; a node drain still moves it, and a
	// retire request still retires it.
	// +optional
	Hold bool `json:"hold,omitempty"`

	// ForceStop kills this server's pod at once, with no drain and a grace
	// period of zero: its players lose their connection and the world loses
	// whatever it had not saved. Set by the agent endpoint's force-stop
	// request, which only a proxy may send.
	// +optional
	ForceStop bool `json:"forceStop,omitempty"`
}

// ServerStatus is the observed state of a Server.
type ServerStatus struct {
	// Phase is the state machine position: Pending, Starting, Ready, Draining,
	// Terminating or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// PodName is the pod backing this server. Set once the pod was created and
	// never reused for a different pod.
	// +optional
	PodName string `json:"podName,omitempty"`

	// PodUID identifies the pod behind PodName, and so one run of this server
	// apart from the next one under the same name, which a persistent server
	// keeps across restarts. Not cleared when the pod goes.
	// +optional
	PodUID string `json:"podUID,omitempty"`

	// Address is the pod IP and port the proxies connect to.
	// +optional
	Address string `json:"address,omitempty"`

	// Players is the last reported player count, throttled to protect etcd.
	// +optional
	Players int32 `json:"players"`

	// Slots is the player capacity reported by the agent.
	// +optional
	Slots int32 `json:"slots"`

	// PlayableSlots is how many of Slots count as capacity: the plugin's
	// figure, else the group's spec.playableSlots, else Slots.
	// +optional
	PlayableSlots int32 `json:"playableSlots"`

	// PlayersUpdatedAt is when Players was last reported by the agent. Counts
	// older than twice the report interval are treated as occupied.
	// +optional
	PlayersUpdatedAt *metav1.Time `json:"playersUpdatedAt,omitempty"`

	// Registered reports whether the proxies currently know this server.
	// +optional
	Registered bool `json:"registered"`

	// WasRegistered is true once this server has been registered with the
	// proxies during the life of its current pod. A server that fell back to
	// Starting may still have players connected.
	// +optional
	WasRegistered bool `json:"wasRegistered"`

	// StartedAt is when this server last began trying to become playable: the
	// first pass that accepted it, then the pod creation, and then every entry
	// into phase Starting. It drives the startup deadline, which therefore
	// bounds the current attempt rather than the age of the pod, and also
	// fails a Server whose pod is never created.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// ReadySince is when the server last entered phase Ready. Drives the reset
	// of the readiness-loss counter.
	// +optional
	ReadySince *metav1.Time `json:"readySince,omitempty"`

	// DrainStartedAt is when the server entered phase Draining. Drives the
	// drain deadline.
	// +optional
	DrainStartedAt *metav1.Time `json:"drainStartedAt,omitempty"`

	// RetiringSince is when the server entered phase Retiring. It drives
	// spec.update.maxStaleSeconds and nothing else — what marks a server as
	// one this update made unavailable is spec.retire.
	// +optional
	RetiringSince *metav1.Time `json:"retiringSince,omitempty"`

	// FailedAt is when the server entered phase Failed. Drives the retention.
	// +optional
	FailedAt *metav1.Time `json:"failedAt,omitempty"`

	// RoundEndedAt is when the server said its round was over. Nil for one
	// that never did. Stamped while the server still runs, so it survives an
	// operator restart.
	// +optional
	RoundEndedAt *metav1.Time `json:"roundEndedAt,omitempty"`

	// ReadinessLosses counts how often this server fell out of Ready. Past the
	// threshold the server is considered broken rather than flapping.
	// +optional
	ReadinessLosses int32 `json:"readinessLosses"`

	// StorageResizePending is true while this server's claim carries the
	// FileSystemResizePending condition: the CSI driver has grown the volume
	// and needs the pod restarted before the filesystem follows. Most drivers
	// expand online and never set it.
	// +optional
	StorageResizePending bool `json:"storageResizePending,omitempty"`

	// StorageResizeError names why the last resize of this server's claim
	// failed, or is empty when none did: this operator's patch refused by the
	// API server (usually allowVolumeExpansion: false), or a driver failure
	// from the claim's ControllerResizeError or NodeResizeError condition.
	// +optional
	StorageResizeError string `json:"storageResizeError,omitempty"`

	// Conditions follow the standard Kubernetes condition contract.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcsrv
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.groupRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.players`
// +kubebuilder:printcolumn:name="Slots",type=integer,JSONPath=`.status.slots`
// +kubebuilder:printcolumn:name="Registered",type=boolean,JSONPath=`.status.registered`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Server is a single running Minecraft server instance.
type Server struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServerSpec   `json:"spec,omitempty"`
	Status ServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServerList contains a list of Server.
type ServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Server `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Server{}, &ServerList{})
}
