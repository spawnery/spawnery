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

// NetworkSpec describes one Minecraft network. Exactly one Network may exist
// per namespace; further ones are rejected with an Accepted=False condition.
type NetworkSpec struct {
	// ForwardingSecretRef names the Secret holding the Velocity modern
	// forwarding secret under the key "secret".
	//
	// Generate the value at random. Every pod carries a salted eight-byte
	// digest of it in the spawnery.cloud/forwarding-hash label, readable by
	// anyone who can read pods, so a memorable secret can be guessed offline.
	// It is the whole of the authentication between the proxy and the
	// backends.
	ForwardingSecretRef ObjectRef `json:"forwardingSecretRef"`

	// Defaults are inherited by all groups of this network.
	// +optional
	Defaults *Defaults `json:"defaults,omitempty"`

	// Scheduling is what this network's groups may ask of the scheduler.
	//
	// Absent, nothing is allowed: a group that sets spec.scheduling, or a
	// proxy group exposed by HostPort, is not accepted.
	// +optional
	Scheduling *SchedulingPolicy `json:"scheduling,omitempty"`

	// Update is how the network's groups change over to a new spec.
	// +optional
	Update *NetworkUpdateSpec `json:"update,omitempty"`
}

// NetworkUpdateSpec bounds changeovers across the network's groups.
type NetworkUpdateSpec struct {
	// MaxConcurrentChangeovers is how many server and proxy groups may change
	// over at the same time. A group changing over runs one server more than
	// its size until its last stale server is gone; a change that reaches
	// every group at once needs that room for every group at once. Unset
	// means no cap.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConcurrentChangeovers *int32 `json:"maxConcurrentChangeovers,omitempty"`
}

// ChangeoverBudget is spec.update.maxConcurrentChangeovers, 0 when unset.
func (n *Network) ChangeoverBudget() int32 {
	if n.Spec.Update == nil || n.Spec.Update.MaxConcurrentChangeovers == nil {
		return 0
	}
	return *n.Spec.Update.MaxConcurrentChangeovers
}

// SchedulingPolicy names what a group's spec.scheduling may contain. Each
// list is a plain allowlist; an empty or absent list allows nothing.
type SchedulingPolicy struct {
	// AllowedTolerationKeys are the taint keys a group may tolerate. A
	// toleration with no key matches every taint and is never allowed.
	// +optional
	AllowedTolerationKeys []string `json:"allowedTolerationKeys,omitempty"`

	// AllowedNodeSelectorKeys are the node label keys a group may select on,
	// in nodeSelector and in node-affinity terms alike.
	// +optional
	AllowedNodeSelectorKeys []string `json:"allowedNodeSelectorKeys,omitempty"`

	// AllowedAffinityNamespaces are the namespaces a pod-affinity or
	// pod-anti-affinity term may name besides the group's own. A
	// namespaceSelector can name any namespace and is never allowed.
	// +optional
	AllowedAffinityNamespaces []string `json:"allowedAffinityNamespaces,omitempty"`

	// HostPortRange bounds expose.hostPort.port on this network's proxy
	// groups. Without it no HostPort group is accepted.
	// +optional
	HostPortRange *PortRange `json:"hostPortRange,omitempty"`
}

// PortRange is an inclusive port interval.
type PortRange struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Min int32 `json:"min"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Max int32 `json:"max"`
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
	// it onto every pod they create (podspec.LabelForwardingHash); a pod whose
	// stamp differs is running on the previous secret. A read failure leaves
	// the previous value in place.
	//
	// The pattern guards the pod label it is copied into unchecked.
	// +optional
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{16})?$`
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
