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

// NetworkSpec describes one Minecraft network. Exactly one Network may exist
// per namespace; further ones are rejected with an Accepted=False condition.
type NetworkSpec struct {
	// ForwardingSecretRef names the Secret holding the Velocity modern
	// forwarding secret under the key "secret".
	ForwardingSecretRef ObjectRef `json:"forwardingSecretRef"`

	// Defaults are inherited by all groups of this network.
	// +optional
	Defaults *Defaults `json:"defaults,omitempty"`
}

// NetworkStatus is the observed state of a Network.
type NetworkStatus struct {
	// Conditions follow the standard Kubernetes condition contract.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ProxyGroups is the number of ProxyGroups referencing this network.
	// +optional
	ProxyGroups int32 `json:"proxyGroups"`

	// ServerGroups is the number of ServerGroups referencing this network.
	// +optional
	ServerGroups int32 `json:"serverGroups"`

	// OnlinePlayers is the sum of players across all server groups.
	// +optional
	OnlinePlayers int32 `json:"onlinePlayers"`

	// ForwardingSecretHash is podspec.ForwardingHash over this network's
	// forwarding secret as the operator last read it. The pod builders stamp
	// it onto every pod they create (podspec.LabelForwardingHash), which is
	// how a rotation becomes visible: a pod whose stamp differs is running on
	// the previous secret.
	//
	// Written only after a successful read. A read failure leaves the previous
	// value in place, because clearing it would leave every pod created during
	// the failure unstamped, and an unstamped pod is one the operator can say
	// nothing about afterwards.
	// +optional
	ForwardingSecretHash string `json:"forwardingSecretHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcnet
// +kubebuilder:printcolumn:name="Server Groups",type=integer,JSONPath=`.status.serverGroups`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.onlinePlayers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Network is the root resource of a Minecraft network.
type Network struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetworkSpec   `json:"spec,omitempty"`
	Status NetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NetworkList contains a list of Network.
type NetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Network `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Network{}, &NetworkList{})
}
