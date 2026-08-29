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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ScaleBoostSpec is extra capacity for one group, for a while.
type ScaleBoostSpec struct {
	// GroupRef names the ServerGroup this adds capacity to. Immutable in
	// practice: nothing edits a boost, and moving one between groups would be
	// two decisions wearing one object.
	GroupRef ObjectRef `json:"groupRef"`

	// Replicas is how many servers to add to the group's own floor.
	//
	// Added, not set: two boosts on one group are two boosts, and a second
	// does not replace a first. That is what makes "somebody else already
	// boosted this" a non-event rather than a race.
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas"`

	// ExpiresAt is when this boost stops counting.
	//
	// Optional in the type and supplied by everything that creates one. A
	// boost with none never expires, which is a real need -- a maintenance
	// window somebody is watching -- and a real hazard: the boost from last
	// weekend still running in March, with nobody left who remembers why the
	// lobby has four servers. Whatever creates these gives them a default, and
	// the type does not, because a type that invented a time would make an
	// explicit "forever" impossible to write.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=boost
// +kubebuilder:printcolumn:name="Group",type=string,JSONPath=`.spec.groupRef.name`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ScaleBoost is extra capacity for a group, for a while.
//
// It exists as its own object rather than as a field on the group, and the
// reason is not tidiness. The operator has no write access to a ServerGroup's
// spec -- its ClusterRole grants get, list and watch -- and on a
// GitOps-managed cluster that spec is owned by a file, so a floor the operator
// raised there would be reverted at the next reconciliation. An admin would
// type a command, watch the count rise, and find it back where the file has
// it. This object is the operator's own, and nothing outside the cluster
// claims it.
//
// It adds to the group's floor and never to its ceiling. spec.scaling.
// maxReplicas still binds, because a ceiling is an instruction -- milestone 4a
// established that -- and a boost is the one thing here that a person might
// create in a hurry.
//
// It has no status. There is nothing about a boost that the operator observes
// and the object does not already say: whether it is live is its expiry
// against the clock, and what it did is on the group it names, as
// status.boostedReplicas. A status here would be a second place to read one
// fact.
type ScaleBoost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ScaleBoostSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ScaleBoostList contains a list of ScaleBoost.
type ScaleBoostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScaleBoost `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScaleBoost{}, &ScaleBoostList{})
}
