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
	// NamespaceNameLabel is stamped by Kubernetes on every namespace (GA in 1.22),
	// so a policy can name a game namespace nobody labelled by hand.
	NamespaceNameLabel = "kubernetes.io/metadata.name"

	KubeSystemNamespace = "kube-system"
)

const (
	// DNSPort needs both protocols: responses over 512 bytes fall back to TCP.
	DNSPort int32 = 53

	// AgentPort must match the "agent" port in config/deploy/deployment.yaml and
	// config/deploy/service.yaml.
	AgentPort int32 = 9443
)

// OperatorPodLabels deliberately omits LabelManagedBy, which the operator pod
// does not carry; a selector copied from ManagedSelector would match nothing.
// These are the labels config/deploy/deployment.yaml puts on the pod template.
func OperatorPodLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "spawnery",
		"app.kubernetes.io/component": "operator",
	}
}

func NetworkPolicyName(network string) string { return network + "-backends" }

// BuildNetworkPolicy restricts who may reach the backends, which run
// online-mode=false and trust whoever knows the forwarding secret. The
// boundary is really the namespace: pod labels are forgeable by anyone who
// can create pods there, and that privilege grants the secret anyway. It
// selects servers and not proxies, whose kubelet TCPSocket probe some CNIs
// subject to policy. Unlike BuildDataClaim it sets an owner reference: a
// stale policy silently drops traffic.
func BuildNetworkPolicy(
	network *spawneryv1alpha1.Network,
	operatorNamespace string,
) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP

	servers := ManagedSelector(network.Name)
	servers[LabelRole] = RoleServer
	proxies := ManagedSelector(network.Name)
	proxies[LabelRole] = RoleProxy

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NetworkPolicyName(network.Name),
			Namespace: network.Namespace,
			// Neither label is load-bearing: NetworkPolicies are deliberately not in the
			// manager's restricted cache, which would hide a pre-existing unlabelled
			// object from CreateOrUpdate's Get and make every pass take AlreadyExists.
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelNetwork:   network.Name,
			},
			// Set literally rather than through SetControllerReference, which needs a
			// scheme, so the builder stays pure.
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
			// A policy with egress rules but without PolicyTypeEgress applies none of
			// them, and the API server accepts it silently.
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				// No namespaceSelector means the policy's own namespace; an empty one would
				// admit a same-named proxy from anywhere in the cluster.
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
					// Both selectors in ONE peer means AND; two peers would mean OR and open
					// every pod in the operator's namespace.
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

func ProxyNetworkPolicyName(group string) string { return group + "-proxies" }

// HTTPSPort is what Mojang's session and API servers answer on.
const HTTPSPort int32 = 443

// privateRanges are excluded from an online-mode proxy's internet rule: the
// cluster's own address space, and link-local, where a cloud's metadata
// endpoint hands out node credentials.
var privateRanges = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"}

// BuildProxyNetworkPolicy is egress only; an ingress rule would gate on the
// kubelet's TCPSocket probe (see BuildNetworkPolicy). An online-mode proxy
// also gets the internet on 443, since Mojang's addresses cannot be pinned.
// Per group, because online-mode is a group setting. The reconciler sets
// the owner reference.
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
