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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ExposeType selects how the proxies are reachable from outside the cluster.
// +kubebuilder:validation:Enum=LoadBalancer;NodePort;HostPort;ClusterIP
type ExposeType string

const (
	// ExposeLoadBalancer needs MetalLB or kube-vip on bare metal; RKE2 ships no
	// active LoadBalancer controller.
	ExposeLoadBalancer ExposeType = "LoadBalancer"
	// ExposeNodePort uses the API server's service-node-port-range.
	ExposeNodePort ExposeType = "NodePort"
	// ExposeHostPort binds a fixed port on the nodes. CNI dependent, and
	// forbidden by Pod Security restricted.
	ExposeHostPort ExposeType = "HostPort"
	// ExposeClusterIP is for a network something else publishes: an ingress
	// controller, a gateway, a tunnel. The operator creates the Service that
	// thing routes to, and nothing else. spec.expose.clusterIP.address says
	// where players connect, because the operator cannot learn it.
	ExposeClusterIP ExposeType = "ClusterIP"
)

// LoadBalancerSpec configures the LoadBalancer strategy.
type LoadBalancerSpec struct {
	// Annotations are copied onto the Service, e.g. for MetalLB pool selection.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// ExternalTrafficPolicy defaults to Local so the client IP survives — bans
	// and rate limits depend on it.
	// +kubebuilder:default=Local
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`
}

// NodePortSpec configures the NodePort strategy.
type NodePortSpec struct {
	// Port must lie inside the API server's service-node-port-range.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// HostPortSpec configures the HostPort strategy.
type HostPortSpec struct {
	// Port is bound on every node running a proxy pod. The kube-scheduler
	// keeps at most one such pod per node, so replicas are capped by nodes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// ClusterIPSpec configures the ClusterIP strategy.
//
// +kubebuilder:validation:XValidation:rule="!self.address.contains(' ') && !self.address.contains('://')",message="expose.clusterIP.address is what a player types, not a URL: no scheme and no spaces"
type ClusterIPSpec struct {
	// Address is what a player types.
	//
	// Required, because the operator cannot learn it: it lives in an
	// IngressRouteTCP, an HTTPRoute, a tunnel's configuration or a DNS
	// record — objects under APIs this operator does not read and cannot
	// know are installed. Optional would make "empty" and "forgotten" the
	// same state, which is the gap this strategy exists to close.
	//
	// No port is required and none should usually be given: Minecraft
	// clients default to 25565, so "mc.paul.wtf" is the whole of what a
	// player types. Give "host:port" only when the entry point really is on
	// another port.
	//
	// Nothing checks that it resolves, that anything listens, or that it
	// leads to this group's Service. It is a sign on a door, not a test of
	// the door.
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`
}

// ExposeSpec selects exactly one strategy and its matching sub-block.
// +kubebuilder:validation:XValidation:rule="self.type != 'NodePort' || has(self.nodePort)",message="expose.nodePort is required for type NodePort"
// +kubebuilder:validation:XValidation:rule="self.type != 'HostPort' || has(self.hostPort)",message="expose.hostPort is required for type HostPort"
// +kubebuilder:validation:XValidation:rule="self.type == 'LoadBalancer' || !has(self.loadBalancer)",message="expose.loadBalancer is only allowed for type LoadBalancer"
// +kubebuilder:validation:XValidation:rule="self.type == 'NodePort' || !has(self.nodePort)",message="expose.nodePort is only allowed for type NodePort"
// +kubebuilder:validation:XValidation:rule="self.type == 'HostPort' || !has(self.hostPort)",message="expose.hostPort is only allowed for type HostPort"
// +kubebuilder:validation:XValidation:rule="self.type != 'ClusterIP' || has(self.clusterIP)",message="expose.clusterIP is required for type ClusterIP"
// +kubebuilder:validation:XValidation:rule="self.type == 'ClusterIP' || !has(self.clusterIP)",message="expose.clusterIP is only allowed for type ClusterIP"
type ExposeSpec struct {
	// Type selects the strategy.
	Type ExposeType `json:"type"`

	// LoadBalancer configures type LoadBalancer.
	// +optional
	LoadBalancer *LoadBalancerSpec `json:"loadBalancer,omitempty"`

	// NodePort configures type NodePort.
	// +optional
	NodePort *NodePortSpec `json:"nodePort,omitempty"`

	// HostPort configures type HostPort.
	// +optional
	HostPort *HostPortSpec `json:"hostPort,omitempty"`

	// ClusterIP configures type ClusterIP.
	// +optional
	ClusterIP *ClusterIPSpec `json:"clusterIP,omitempty"`
}

// RoutingSpec configures where players land.
type RoutingSpec struct {
	// FallbackGroups is the ordered try-list on join and on drain.
	// +kubebuilder:validation:MinItems=1
	FallbackGroups []string `json:"fallbackGroups"`
}

// ProxyConfigSpec are the Velocity settings the operator renders.
type ProxyConfigSpec struct {
	// PlayerLimit is the network-wide player limit of one proxy.
	// +kubebuilder:validation:Minimum=1
	// +optional
	PlayerLimit int32 `json:"playerLimit,omitempty"`

	// Motd is shown in the server list.
	// +optional
	Motd string `json:"motd,omitempty"`

	// OnlineMode is whether the proxy authenticates players with Mojang.
	//
	// Turning it off means the proxy stops authenticating anyone: any client
	// may connect under any name, including the name of a player who has
	// paid for the game and owns things on this network, and the network is
	// only as protected as whatever sits in front of it. There is nothing
	// further down that catches it — the backends run online-mode=false by
	// design, because the proxy is the layer that was supposed to do this.
	//
	// It is a field on the custom resource rather than something reachable
	// through configOverlay so that turning it off is a visible edit to the
	// object an operator reviews, not a line in a ConfigMap nobody reads.
	//
	// This is the proxy's own online-mode, not the backends'
	// proxies.velocity.online-mode in paper-global.yml, which means "trust
	// what the proxy forwards" and stays true either way: modern forwarding
	// works the same whether the proxy authenticated the player or not.
	// +kubebuilder:default=true
	// +optional
	OnlineMode *bool `json:"onlineMode,omitempty"`
}

// ProxyGroupSpec describes the Velocity layer of a network.
// +kubebuilder:validation:XValidation:rule="self.networkRef == oldSelf.networkRef",message="spec.networkRef is immutable"
type ProxyGroupSpec struct {
	// NetworkRef names the Network this group belongs to. Immutable.
	NetworkRef ObjectRef `json:"networkRef"`

	// Replicas is the number of proxy pods.
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas"`

	// Image is the Velocity base image. A digest reference is recommended.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Resources overrides Network.spec.defaults.resources.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Scheduling overrides Network.spec.defaults.scheduling.
	// +optional
	Scheduling *Scheduling `json:"scheduling,omitempty"`

	// Expose makes the proxies reachable from outside the cluster.
	Expose ExposeSpec `json:"expose"`

	// Routing configures the fallback groups.
	Routing RoutingSpec `json:"routing"`

	// Drain bounds how long existing sessions may run out on proxy replacement.
	// +kubebuilder:default={timeoutSeconds:300}
	// +optional
	Drain *DrainSpec `json:"drain,omitempty"`

	// Config are the rendered Velocity settings.
	// +optional
	Config *ProxyConfigSpec `json:"config,omitempty"`

	// ConfigOverlay names a ConfigMap whose keys are configuration files to
	// merge over the rendered defaults — "server.properties",
	// "paper-global.yml" or "velocity.toml", in the target's own dialect.
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
}

// ProxyGroupStatus is the observed state of a ProxyGroup.
type ProxyGroupStatus struct {
	// Phase is derived from the proxy pods and conditions.
	// +optional
	Phase string `json:"phase,omitempty"`

	// ReadyReplicas is the number of proxies that passed the ready gate.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// Address is where players connect.
	// +optional
	Address string `json:"address,omitempty"`

	// ConnectedPlayers is the sum of players across all proxies.
	// +optional
	ConnectedPlayers int32 `json:"connectedPlayers"`

	// ObservedGeneration is the spec generation this status was computed from.
	//
	// Literally that, and not the looser convention it is often read as. It
	// advances on a pass that failed as well as one that succeeded, because
	// setStatus writes it on every path that observed the pods and the
	// Service -- so a group permanently refused by Pod Security reports
	// observedGeneration == generation for as long as the refusal stands. A
	// reader taking that to mean "the controller has caught up and all is
	// well" is misled; Degraded=True beside the same generation is the
	// correction, and the two together say the controller did catch up and
	// what it found was a refusal.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard Kubernetes condition contract.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mcproxy
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.connectedPlayers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ProxyGroup is the Velocity layer of a network.
type ProxyGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProxyGroupSpec   `json:"spec,omitempty"`
	Status ProxyGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProxyGroupList contains a list of ProxyGroup.
type ProxyGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProxyGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProxyGroup{}, &ProxyGroupList{})
}

// defaultProxyDrainTimeout is how long a proxy may take to empty before it is
// removed anyway.
//
// Five minutes rather than the sixty seconds a ServerGroup uses, because the
// two waits are not the same wait. A server drain moves its players to another
// backend, which is quick; a proxy drain has nowhere to move them — the
// client's connection terminates at the proxy being removed — so this waits
// for people to leave on their own. There is no honest default: a play session
// runs to tens of minutes, so every number short of that disconnects somebody.
// Five minutes lets a scale-down in a quiet period finish without kicks while
// still bounding a deploy. An operator who cares about this should set
// spec.drain.timeoutSeconds.
//
// This only fires for an in-memory zero value: ProxyGroupSpec.Drain's
// +kubebuilder:default={timeoutSeconds:300} marker means a ProxyGroup that
// came from the API server already carries 300 in Drain.TimeoutSeconds. The
// two 300s are independent and must be kept in step by hand.
const defaultProxyDrainTimeout = 300 * time.Second

// DrainTimeout is how long existing sessions may run out before a proxy being
// removed is deleted anyway.
func (g *ProxyGroup) DrainTimeout() time.Duration {
	if g.Spec.Drain == nil || g.Spec.Drain.TimeoutSeconds < 1 {
		return defaultProxyDrainTimeout
	}
	return time.Duration(g.Spec.Drain.TimeoutSeconds) * time.Second
}
