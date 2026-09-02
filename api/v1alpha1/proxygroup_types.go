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
	//
	// It cannot coexist with Pod Security `baseline` or `restricted` in the
	// same namespace: both disallow a container hostPort outright, so the API
	// server refuses every pod of such a group and this operator reports the
	// refusal on Degraded rather than ever admitting one. Measured against
	// `restricted` on a real cluster, quoted verbatim:
	//
	//	violates PodSecurity "restricted:latest": hostPort
	//	(container "velocity" uses hostPort 25577)
	//
	// The remedy is a namespace of its own for the HostPort group, with a
	// relaxed Pod Security label, separate from the restricted namespaces the
	// rest of the network runs in.
	//
	// That namespace is necessary and not sufficient. A hostPort that is
	// admitted, ready and published in status.address can still be
	// unreachable, because whether the port is open to the world is a
	// host-firewall question rather than a Kubernetes one -- measured on a
	// Cilium cluster whose CiliumClusterwideNetworkPolicy admitted a fixed
	// list of ports from `world` and dropped this one, while the pod was
	// serving perfectly. docs/network-boundaries.md carries that measurement.
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
	//
	// Editing it does not replace the group's proxies. It reaches only the
	// group's ConfigMap, which the pod names in a volume, so the new value
	// applies to the next proxy pod and to no existing one -- cosmetic here,
	// and not cosmetic on OnlineMode below, which behaves the same way.
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
	//
	// Editing it does not replace the group's proxies, and nothing on the
	// object says so. Like Motd it reaches only the group's ConfigMap, which
	// spawnery-config reads at container start, so a running proxy goes on
	// authenticating players the old way while observedGeneration advances
	// and the phase stays Ready -- every signal the API offers says the
	// change is applied. PlayerLimit, its sibling under the same stanza,
	// *does* roll the group, because it reaches the pod as an environment
	// variable; nothing about the stanza distinguishes them and only
	// internal/podspec/proxy.go does.
	//
	// After changing this, delete the group's proxy pods or edit a field that
	// does roll it -- PlayerLimit is the cheapest. Leaving it is worse than it
	// looks: a pod that restarts later for an unrelated reason picks the new
	// value up on its own, so the group drifts into running both settings at
	// once.
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
	//
	// Editing it rolls the whole group, and that is worth knowing before an
	// incident rather than during one. The value reaches the pod as
	// terminationGracePeriodSeconds, so it is part of the rendered pod the
	// group's hash covers -- which means raising a drain timeout, the thing an
	// operator does in the middle of an incident, adds a surge pod and a full
	// replacement cycle on top of whatever prompted it.
	//
	// Raising it while a drain is already in flight otherwise behaves: the
	// marked pod keeps its mark, being now stale as well as draining, and the
	// deadline is read from the current spec on every pass. So the edit is
	// safe, just not free.
	// +kubebuilder:default={timeoutSeconds:300}
	// +optional
	Drain *DrainSpec `json:"drain,omitempty"`

	// Config are the rendered Velocity settings.
	// +optional
	Config *ProxyConfigSpec `json:"config,omitempty"`

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

	// Mounts are extra ConfigMap, Secret and PersistentVolumeClaim mounts.
	//
	// It is ServerGroupSpec.Mounts for a proxy and it arrived later, which is
	// worth knowing when reading a manifest written against an older chart: a
	// ProxyGroup simply had no way to be handed a file until then, and the
	// asymmetry was never a decision anybody took. What forced it was one
	// network's own shape -- its proxies read the same shared asset directory
	// its backends do, out of a template that targets both.
	// +optional
	// +listType=map
	// +listMapKey=name
	Mounts []Mount `json:"mounts,omitempty"`

	// Env are extra environment variables for the proxy container, appended
	// to the ones the operator sets. A name may not begin with
	// ReservedEnvPrefix.
	//
	// It is ServerGroupSpec.Env for a proxy, rule and reasoning unchanged,
	// and JAVA_TOOL_OPTIONS is the same seam: the Velocity entrypoint execs
	// java with its own flag list too. It is in podspec.DesiredProxyHash for
	// the same reason -- editing it rolls the group through the ordinary
	// surge-1 path.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.all(e, !e.name.startsWith('SPAWNERY_'))",message="the SPAWNERY_ prefix is reserved for the environment variables the operator sets itself"
	Env []corev1.EnvVar `json:"env,omitempty"`

	// DisplayName is what this group is called where a person reads it:
	// a scoreboard, a chat message, a playtime key. A metadata.name is a DNS
	// label -- lowercase, no spaces -- and a name people say out loud rarely
	// is, so a plugin that shows "Bingo-Team" reads it from here and not from
	// the object's name.
	//
	// The operator carries it and reads none of it, like Attributes below.
	// Empty is not an error: a plugin then shows the group's own name, and it
	// is the plugin that makes that substitution rather than the operator,
	// so that a picture with the field unset and a picture whose operator
	// predates the field read the same.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	DisplayName string `json:"displayName,omitempty"`

	// Attributes is what plugins are told about this group. See
	// GroupAttributes: the operator carries it and acts on none of it.
	//
	// It shapes no pod and is therefore not in the spec hash a server is
	// replaced on -- the opposite of Env above. Editing it replaces nothing
	// and restarts nothing; the next network picture simply carries the new
	// value, which is what a description of a group should cost.
	// +optional
	Attributes GroupAttributes `json:"attributes,omitempty"`

	// ExtraPlugins names a volume whose plugins and their configuration are
	// copied into this group's servers on every start. See ExtraPlugins.
	// +optional
	ExtraPlugins *ExtraPlugins `json:"extraPlugins,omitempty"`
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
