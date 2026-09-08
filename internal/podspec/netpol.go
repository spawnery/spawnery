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

package podspec

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	// NamespaceNameLabel is the label Kubernetes stamps on every namespace
	// itself, holding that namespace's own name. On by default since 1.21 and
	// GA in 1.22. It is what lets a policy name a namespace without anybody
	// having to label one by hand, which matters because the game namespaces
	// are discovered at runtime and nobody is there to label them.
	NamespaceNameLabel = "kubernetes.io/metadata.name"

	// KubeSystemNamespace is where the cluster's DNS lives.
	KubeSystemNamespace = "kube-system"
)

const (
	// DNSPort is the port cluster DNS answers on. Both protocols are needed:
	// a response larger than 512 bytes falls back to TCP, and an agent that
	// could not resolve the operator would never connect at all.
	DNSPort int32 = 53

	// AgentPort is the operator's gRPC endpoint, the port every managed pod
	// dials. It is the literal config/deploy/deployment.yaml names as the
	// container port "agent" and config/deploy/service.yaml exposes.
	AgentPort int32 = 9443
)

// OperatorPodLabels selects the operator's own pod. It deliberately does NOT
// include LabelManagedBy: the operator pod does not carry it, which is a
// feature -- the two ends of the agent channel need different rules -- and a
// trap for anyone writing a peer selector by copying ManagedSelector. These
// are the two labels config/deploy/deployment.yaml puts on the pod template
// and config/deploy/service.yaml already selects on.
func OperatorPodLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "spawnery",
		"app.kubernetes.io/component": "operator",
	}
}

// NetworkPolicyName is the policy a Network owns in its own namespace.
func NetworkPolicyName(network string) string { return network + "-backends" }

// BuildNetworkPolicy renders the policy that restricts who may attempt modern
// forwarding: a Paper server runs online-mode=false, so it authenticates
// nobody and trusts whatever completes the handshake with the right secret.
//
// It restricts them only as far as the namespace boundary, and that limit is
// worth stating because the shape of the rule invites the opposite reading.
// The ingress peer is a podSelector over spawnery's own labels, and a pod's
// labels are chosen by whoever creates it -- so anyone who may create a pod in
// a game namespace can wear this policy's colours and reach the backends,
// while the same pod without them is refused.
//
// That is not a hole this operator can close. Vanilla NetworkPolicy offers
// podSelector, namespaceSelector and ipBlock as peers, and within one namespace
// the first is forgeable and the second says nothing -- no policy expressible
// here distinguishes a real proxy from an invented one. Nor would closing it
// buy much: the same privilege grants the forwarding secret outright, since
// any pod may mount any Secret in its own namespace.
//
// So the boundary is the namespace, not this policy. What the policy does
// defend against, and does defend well, is the co-tenant that cannot create
// pods: a compromised workload cannot relabel itself.
//
// It selects server pods and not proxy pods, and that asymmetry is deliberate
// rather than partial. A server's readiness probe is an exec of spawnery-slp
// against 127.0.0.1 (server.go), which runs inside the container and no
// NetworkPolicy governs; a proxy's is a TCPSocket from the kubelet
// (proxy.go), which one might. Selecting proxies would put the whole fleet's
// readiness at the mercy of whether this cluster's CNI subjects kubelet
// traffic to policy. Since the invariant is entirely about backends, this
// selects backends. See the design's §3.3.
//
// The owner reference is the one place this departs from BuildDataClaim, which
// carries none on purpose. A stale claim is inert and may still hold a world
// somebody wants; a stale NetworkPolicy silently drops traffic in a namespace
// nobody associates with Spawnery any more. It is namespace-local and
// therefore legal, because a Network owns its namespace.
func BuildNetworkPolicy(
	network *spawneryv1alpha1.Network,
	operatorNamespace string,
) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP

	// ManagedSelector builds a fresh map per call, so adding the role term
	// here cannot reach any other caller's copy.
	servers := ManagedSelector(network.Name)
	servers[LabelRole] = RoleServer
	proxies := ManagedSelector(network.Name)
	proxies[LabelRole] = RoleProxy

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NetworkPolicyName(network.Name),
			Namespace: network.Namespace,
			// Both labels are metadata for a human reading kubectl output,
			// and neither is load-bearing. In particular LabelManagedBy is
			// NOT what any restricted cache selects on: cmd/spawnery-operator
			// restricts the cache for ConfigMaps, ServiceAccounts and
			// PersistentVolumeClaims, which are high-cardinality kinds that
			// would otherwise pull every object in the cluster, and does not
			// restrict NetworkPolicies, of which there are a handful.
			//
			// Restricting them would also be a regression rather than a
			// tightening: reconcileNetworkPolicy's CreateOrUpdate reads
			// through that cache, so a pre-existing UNLABELLED object at this
			// name would be invisible to the Get, and every pass would Create
			// and take AlreadyExists, forever.
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelNetwork:   network.Name,
			},
			// Set literally rather than through
			// controllerutil.SetControllerReference, which needs a scheme only
			// to look up a GroupVersionKind this package knows statically.
			// Keeping it here is what lets the builder stay a pure function
			// and the owner reference stay unit-tested.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spawneryv1alpha1.GroupVersion.String(),
				Kind:               "Network",
				Name:               network.Name,
				UID:                network.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: servers},
			// Both types are declared explicitly. A policy carrying egress
			// rules without PolicyTypeEgress applies none of them, and the API
			// server accepts it without complaint.
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				// No namespaceSelector: a peer without one means the policy's
				// own namespace, which is exactly right, because a Network
				// owns its namespace. An empty selector here would admit a
				// proxy carrying the same network name from anywhere in the
				// cluster.
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{MatchLabels: proxies},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: &tcp,
					Port:     ptr.To(intstr.FromInt32(MinecraftPort)),
				}},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								NamespaceNameLabel: KubeSystemNamespace,
							},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: ptr.To(intstr.FromInt32(DNSPort))},
						{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(DNSPort))},
					},
				},
				{
					// Both selectors in ONE peer: that means "pods matching
					// the pod selector, in namespaces matching the namespace
					// selector". Splitting them into two peers would mean OR,
					// and would open every pod in the operator's namespace.
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								NamespaceNameLabel: operatorNamespace,
							},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: OperatorPodLabels(),
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(AgentPort))},
					},
				},
			},
		},
	}
}

// ProxyNetworkPolicyName is the egress policy a ProxyGroup owns.
func ProxyNetworkPolicyName(group string) string { return group + "-proxies" }

// HTTPSPort is what Mojang's session and API servers answer on, and the one
// port an online-mode proxy needs the internet for.
const HTTPSPort int32 = 443

// privateRanges are what an online-mode proxy's internet rule leaves out: the
// cluster's own address space, which the backend and operator rules already
// cover as far as they should, and link-local, where a cloud's metadata
// endpoint hands out node credentials.
var privateRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"}

// BuildProxyNetworkPolicy renders the egress policy for one ProxyGroup's
// pods. Egress only, on purpose: a proxy's readiness probe is a TCPSocket
// from the kubelet, and an ingress rule would put the fleet's readiness at
// the mercy of whether the CNI subjects kubelet traffic to policy --
// BuildNetworkPolicy's comment carries the argument. Egress has no such
// caller.
//
// What a proxy needs: cluster DNS, the operator's agent port, and the
// backends of its own network on the Minecraft port. An online-mode proxy
// also needs Mojang's session servers, whose addresses nobody can pin, so
// it gets the internet on 443 minus the private and link-local ranges. An
// offline-mode proxy authenticates nobody and gets nothing beyond the three.
//
// Per group and not per network, because online-mode is a group's setting.
// The owner reference is set by the reconciler, which has the scheme.
func BuildProxyNetworkPolicy(
	networkName string,
	group *spawneryv1alpha1.ProxyGroup,
	operatorNamespace string,
	onlineMode bool,
) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP

	servers := ManagedSelector(networkName)
	servers[LabelRole] = RoleServer

	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{NamespaceNameLabel: KubeSystemNamespace},
				},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: ptr.To(intstr.FromInt32(DNSPort))},
				{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(DNSPort))},
			},
		},
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{NamespaceNameLabel: operatorNamespace},
				},
				PodSelector: &metav1.LabelSelector{MatchLabels: OperatorPodLabels()},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(AgentPort))},
			},
		},
		{
			// No namespaceSelector: the policy's own namespace, which a
			// Network owns.
			To: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: servers},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(MinecraftPort))},
			},
		},
	}
	if onlineMode {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: privateRanges},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(HTTPSPort))},
			},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ProxyNetworkPolicyName(group.Name),
			Namespace: group.Namespace,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelNetwork:   networkName,
				LabelGroup:     group.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: ProxyLabels(networkName, group.Name)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}
