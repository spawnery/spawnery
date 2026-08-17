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

package podspec_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

func testNetwork() *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production",
			Namespace: "minecraft",
			UID:       "5f3a9c1e-0000-4000-8000-000000000001",
		},
	}
}

// TestBuildNetworkPolicySelectsServersOfOneNetwork pins the three terms of the
// pod selector. Dropping any one of them widens the policy to pods it was
// never meant to govern -- without the role term it would select the proxies
// too, and a proxy's readiness is a kubelet dial (internal/podspec/proxy.go),
// which is exactly the failure design §3.3 exists to avoid.
func TestBuildNetworkPolicySelectsServersOfOneNetwork(t *testing.T) {
	p := podspec.BuildNetworkPolicy(testNetwork(), "spawnery-system")

	want := map[string]string{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelNetwork:   "production",
		podspec.LabelRole:      podspec.RoleServer,
	}
	got := p.Spec.PodSelector.MatchLabels
	if len(got) != len(want) {
		t.Fatalf("podSelector = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("podSelector[%q] = %q, want %q", k, got[k], v)
		}
	}
	if p.Namespace != "minecraft" {
		t.Errorf("namespace = %q, want minecraft", p.Namespace)
	}
	if p.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Errorf("the policy itself must carry %s=%s, or the operator's own "+
			"restricted cache cannot see the object it wrote",
			podspec.LabelManagedBy, podspec.ManagedByValue)
	}
}

// TestBuildNetworkPolicyAdmitsOnlyItsOwnProxies checks the rule the whole
// milestone exists for, and the one thing about it that is easy to get wrong:
// the ingress peer carries a podSelector and NO namespaceSelector, which
// restricts it to the policy's own namespace. Adding an empty namespaceSelector
// there would admit a proxy of the same network name from any namespace in the
// cluster.
func TestBuildNetworkPolicyAdmitsOnlyItsOwnProxies(t *testing.T) {
	p := podspec.BuildNetworkPolicy(testNetwork(), "spawnery-system")

	if len(p.Spec.Ingress) != 1 {
		t.Fatalf("got %d ingress rules, want exactly one", len(p.Spec.Ingress))
	}
	rule := p.Spec.Ingress[0]
	if len(rule.From) != 1 {
		t.Fatalf("got %d peers, want exactly one", len(rule.From))
	}
	peer := rule.From[0]
	if peer.NamespaceSelector != nil {
		t.Errorf("the ingress peer carries a namespaceSelector (%v); without one "+
			"it means the policy's own namespace, which is what a Network owns",
			peer.NamespaceSelector)
	}
	if peer.PodSelector == nil {
		t.Fatal("the ingress peer has no podSelector, so it admits every pod")
	}
	if got := peer.PodSelector.MatchLabels[podspec.LabelRole]; got != podspec.RoleProxy {
		t.Errorf("ingress peer role = %q, want %q", got, podspec.RoleProxy)
	}
	if got := peer.PodSelector.MatchLabels[podspec.LabelNetwork]; got != "production" {
		t.Errorf("ingress peer network = %q, want production", got)
	}
	if len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != int(podspec.MinecraftPort) {
		t.Errorf("ingress ports = %v, want only %d", rule.Ports, podspec.MinecraftPort)
	}
}

// TestBuildNetworkPolicyEgressIsDNSAndTheOperatorOnly pins the egress half. A
// backend runs online-mode=false and never authenticates a player, so it never
// needs Mojang; its only other measured outbound call is Paper's update check,
// which docs/known-issues.md records as failing harmlessly with no network.
func TestBuildNetworkPolicyEgressIsDNSAndTheOperatorOnly(t *testing.T) {
	p := podspec.BuildNetworkPolicy(testNetwork(), "spawnery-system")

	if len(p.Spec.Egress) != 2 {
		t.Fatalf("got %d egress rules, want exactly two (DNS, operator)", len(p.Spec.Egress))
	}

	dns := p.Spec.Egress[0]
	if len(dns.To) != 1 || dns.To[0].NamespaceSelector == nil {
		t.Fatalf("the DNS rule must select a namespace; got %v", dns.To)
	}
	if got := dns.To[0].NamespaceSelector.MatchLabels[podspec.NamespaceNameLabel]; got != podspec.KubeSystemNamespace {
		t.Errorf("DNS namespace = %q, want %q", got, podspec.KubeSystemNamespace)
	}
	if dns.To[0].PodSelector != nil {
		t.Errorf("the DNS rule carries a podSelector (%v); CoreDNS's labels are "+
			"conventional rather than guaranteed and narrowing to them buys nothing",
			dns.To[0].PodSelector)
	}
	protocols := map[corev1.Protocol]bool{}
	for _, port := range dns.Ports {
		if port.Port.IntValue() != int(podspec.DNSPort) {
			t.Errorf("DNS port = %v, want %d", port.Port, podspec.DNSPort)
		}
		if port.Protocol != nil {
			protocols[*port.Protocol] = true
		}
	}
	if !protocols[corev1.ProtocolUDP] || !protocols[corev1.ProtocolTCP] {
		t.Errorf("DNS must be allowed over both UDP and TCP; got %v", protocols)
	}

	op := p.Spec.Egress[1]
	if len(op.To) != 1 {
		t.Fatalf("got %d operator peers, want exactly one", len(op.To))
	}
	// One peer carrying both selectors means "pods matching podSelector in
	// namespaces matching namespaceSelector". Two peers would mean OR, which
	// would admit every pod in the operator's namespace and the operator's own
	// labels in every namespace.
	if op.To[0].NamespaceSelector == nil || op.To[0].PodSelector == nil {
		t.Fatalf("the operator peer needs both selectors in one peer; got %+v", op.To[0])
	}
	if got := op.To[0].NamespaceSelector.MatchLabels[podspec.NamespaceNameLabel]; got != "spawnery-system" {
		t.Errorf("operator namespace = %q, want spawnery-system", got)
	}
	// The podSelector's content, not just its presence: OperatorPodLabels'
	// own doc comment calls this a trap for anyone writing a peer selector by
	// copying ManagedSelector, since the operator pod carries neither
	// LabelManagedBy nor LabelNetwork. A selector with the wrong labels would
	// admit nothing -- or the wrong thing -- while every other assertion here
	// stayed green.
	wantOperator := podspec.OperatorPodLabels()
	gotOperator := op.To[0].PodSelector.MatchLabels
	if len(gotOperator) != len(wantOperator) {
		t.Errorf("operator podSelector = %v, want %v", gotOperator, wantOperator)
	}
	for k, v := range wantOperator {
		if gotOperator[k] != v {
			t.Errorf("operator podSelector[%q] = %q, want %q", k, gotOperator[k], v)
		}
	}
	if len(op.Ports) != 1 || op.Ports[0].Port.IntValue() != int(podspec.AgentPort) {
		t.Errorf("operator ports = %v, want only %d", op.Ports, podspec.AgentPort)
	}
}

// TestBuildNetworkPolicyIsOwnedByItsNetwork is the property that keeps a stale
// policy from outliving the Network it protects. It is the one place this
// builder departs from BuildDataClaim, which carries no owner reference on
// purpose: a stale claim is a world somebody may still want, a stale
// NetworkPolicy silently drops traffic.
func TestBuildNetworkPolicyIsOwnedByItsNetwork(t *testing.T) {
	network := testNetwork()
	p := podspec.BuildNetworkPolicy(network, "spawnery-system")

	if len(p.OwnerReferences) != 1 {
		t.Fatalf("got %d owner references, want exactly one", len(p.OwnerReferences))
	}
	ref := p.OwnerReferences[0]
	if ref.Kind != "Network" || ref.Name != network.Name || ref.UID != network.UID {
		t.Errorf("owner = %s/%s uid %s, want Network/%s uid %s",
			ref.Kind, ref.Name, ref.UID, network.Name, network.UID)
	}
	// APIVersion matters as much as Kind: the garbage collector resolves the
	// owner by GroupVersionKind, and a wrong or missing APIVersion means it
	// never finds the Network, so the policy outlives it -- the exact failure
	// this owner reference exists to prevent.
	if ref.APIVersion != spawneryv1alpha1.GroupVersion.String() {
		t.Errorf("owner apiVersion = %q, want %q", ref.APIVersion, spawneryv1alpha1.GroupVersion.String())
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Error("the owner reference must be a controller reference")
	}
	// BlockOwnerDeletion defaults to false, which permits a foreground-deletion
	// race: the policy could be deleted out from under a Network still being
	// foreground-deleted, instead of blocking until the policy is gone.
	if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Error("the owner reference must block owner deletion")
	}
}

// TestBuildNetworkPolicyDeclaresBothPolicyTypes guards a silent widening.
// PolicyTypes is not decoration: a policy with egress rules but no
// PolicyTypeEgress applies none of them, and the object is still accepted.
func TestBuildNetworkPolicyDeclaresBothPolicyTypes(t *testing.T) {
	p := podspec.BuildNetworkPolicy(testNetwork(), "spawnery-system")

	var ingress, egress bool
	for _, t := range p.Spec.PolicyTypes {
		switch t {
		case networkingv1.PolicyTypeIngress:
			ingress = true
		case networkingv1.PolicyTypeEgress:
			egress = true
		}
	}
	if !ingress || !egress {
		t.Errorf("policyTypes = %v, want both Ingress and Egress", p.Spec.PolicyTypes)
	}
}
