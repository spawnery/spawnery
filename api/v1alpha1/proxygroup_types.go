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
	// Address is what a player types. Required, because the operator cannot
	// learn it from whatever routes to the Service.
	//
	// Give "host:port" only when the entry point is not on 25565, the
	// client's default.
	//
	// Nothing checks that it resolves, that anything listens, or that it
	// leads to this group's Service.
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
	// Pod Security `baseline` and `restricted` both refuse a hostPort, so the
	// group needs a namespace of its own with a relaxed label; the refusal is
	// reported on Degraded. Whether the port is reachable from outside is
	// still up to the host firewall or cluster network policy.
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

	// Motd is shown in the server list. Editing it rolls the group's proxies.
	// +optional
	Motd string `json:"motd,omitempty"`

	// OnlineMode is whether the proxy authenticates players with Mojang.
	//
	// Turning it off lets any client connect under any name; the backends
	// run online-mode=false and do not catch it. It is not the backends'
	// proxies.velocity.online-mode, which stays true either way.
	//
	// Editing it rolls the group's proxies.
	// +kubebuilder:default=true
	// +optional
	OnlineMode *bool `json:"onlineMode,omitempty"`
}

// ProxyUpdateSpec controls how a proxy group lets draining proxies go.
type ProxyUpdateSpec struct {
	// MaxStaleSeconds disconnects the players left on a draining proxy after
	// this many seconds. 0, the default, means a drain waits for its players:
	// the proxy takes no new connections and stops once empty.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxStaleSeconds int32 `json:"maxStaleSeconds,omitempty"`

	// Transfer moves players to another proxy instead of waiting for them.
	// +optional
	Transfer *ProxyTransferSpec `json:"transfer,omitempty"`
}

// ProxyTransferSpec moves players off a leaving proxy with Minecraft's
// transfer packet (clients 1.20.5 and newer): at once when they change
// server, the rest after ForceAfterSeconds unless their server has closed
// its door. Setting it rolls the group once.
type ProxyTransferSpec struct {
	// ForceAfterSeconds is how long a leaving proxy waits before it
	// transfers players who have not changed server, counted from when its
	// agent first sees it leaving. Default 120; keep it below
	// drain.timeoutSeconds and any maxStaleSeconds.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ForceAfterSeconds *int32 `json:"forceAfterSeconds,omitempty"`

	// ForceGroups limits the forced transfer to players on servers of these
	// server groups; everyone else moves only when they change server. Empty
	// forces in every group.
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +optional
	ForceGroups []string `json:"forceGroups,omitempty"`
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
	// It reaches the pod as terminationGracePeriodSeconds, so editing it
	// replaces every proxy of the group. A drain already in flight picks up
	// the new deadline.
	// +kubebuilder:default={timeoutSeconds:300}
	// +optional
	Drain *DrainSpec `json:"drain,omitempty"`

	// ChangeoverStage orders this group's changeover against the network's
	// other groups: a group waits while any group of a lower stage is still
	// changing over. Groups of one stage change over together, within
	// Network.spec.update.maxConcurrentChangeovers.
	// +optional
	ChangeoverStage int32 `json:"changeoverStage,omitempty"`

	// Update bounds how long a draining proxy may wait for its players.
	// +optional
	Update *ProxyUpdateSpec `json:"update,omitempty"`

	// Config are the rendered Velocity settings.
	// +optional
	Config *ProxyConfigSpec `json:"config,omitempty"`

	// ConfigOverlay names a ConfigMap whose keys are configuration files to
	// merge over the rendered defaults — "server.properties",
	// "paper-global.yml", "paper-world-defaults.yml" or "velocity.toml", in
	// the target's own dialect.
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

	// Mounts are extra ConfigMap, Secret and PersistentVolumeClaim mounts.
	// +optional
	// +listType=map
	// +listMapKey=name
	Mounts []Mount `json:"mounts,omitempty"`

	// Env are extra environment variables for the proxy container, appended
	// to the ones the operator sets. A name may not begin with SPAWNERY_.
	// JVM options go in JAVA_TOOL_OPTIONS.
	//
	// Editing it replaces every proxy of the group. A valueFrom reference is
	// digested, not its value: rotating the Secret or ConfigMap reaches only
	// new pods.
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
	// servers on every start, replacing what the server wrote there. See
	// ExtraFiles.
	// +optional
	ExtraFiles *ExtraFiles `json:"extraFiles,omitempty"`

	// Substitution fills placeholders in the files the entrypoint copies from
	// extraPlugins and extraFiles. See Substitution.
	// +optional
	Substitution *Substitution `json:"substitution,omitempty"`
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
	// It advances on a refused pass too; check Degraded beside it.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

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

// defaultProxyDrainTimeout is longer than a ServerGroup's 60 s because a proxy
// drain cannot move its players; it waits for them to leave. Keep it in step
// with ProxyGroupSpec.Drain's +kubebuilder:default by hand.
const defaultProxyDrainTimeout = 300 * time.Second

func (g *ProxyGroup) DrainTimeout() time.Duration {
	if g.Spec.Drain == nil || g.Spec.Drain.TimeoutSeconds < 1 {
		return defaultProxyDrainTimeout
	}
	return time.Duration(g.Spec.Drain.TimeoutSeconds) * time.Second
}

// MaxStale is spec.update.maxStaleSeconds, 0 when unset.
func (g *ProxyGroup) MaxStale() time.Duration {
	if g.Spec.Update == nil {
		return 0
	}
	return time.Duration(g.Spec.Update.MaxStaleSeconds) * time.Second
}

// defaultTransferForceAfter stays well below the 300 s drain default because
// the agent learns it is leaving up to one resync after the drain clock starts.
const defaultTransferForceAfter = 120 * time.Second

// TransferForceGroups is spec.update.transfer.forceGroups; nil when transfer
// is unset or names none.
func (g *ProxyGroup) TransferForceGroups() []string {
	if g.Spec.Update == nil || g.Spec.Update.Transfer == nil {
		return nil
	}
	return g.Spec.Update.Transfer.ForceGroups
}

// TransferForceAfter is spec.update.transfer.forceAfterSeconds and whether
// transfer is on at all. (0, false) when spec.update.transfer is unset.
func (g *ProxyGroup) TransferForceAfter() (time.Duration, bool) {
	if g.Spec.Update == nil || g.Spec.Update.Transfer == nil {
		return 0, false
	}
	if g.Spec.Update.Transfer.ForceAfterSeconds == nil {
		return defaultTransferForceAfter, true
	}
	return time.Duration(*g.Spec.Update.Transfer.ForceAfterSeconds) * time.Second, true
}
