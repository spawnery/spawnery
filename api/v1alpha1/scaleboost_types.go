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

// ScaleBoostSpec is extra capacity for one group, for a while.
type ScaleBoostSpec struct {
	// GroupRef names the ServerGroup this adds capacity to.
	GroupRef ObjectRef `json:"groupRef"`

	// Replicas is how many servers to add to the group's own floor. Boosts on
	// one group add up.
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas"`

	// ExpiresAt is when this boost stops counting. A boost without one never
	// expires; the tools that create boosts supply a default.
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
// It is its own object rather than a field on the group because the operator
// cannot write a ServerGroup's spec, and under GitOps that spec belongs to a
// file anyway.
//
// It adds to the group's floor and never to its ceiling: spec.scaling.
// maxReplicas still binds. Its effect shows on the group as
// status.boostedReplicas.
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
