# Milestone 6b Implementation Plan — NetworkPolicies and the channel's availability half

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give backends a NetworkPolicy that admits only their own network's
proxies, give the operator's agent endpoint one that admits only managed pods,
and bound the agent channel so a single compromised pod cannot load the API
server.

**Architecture:** A pure builder in `internal/podspec` renders the per-`Network`
policy; `NetworkReconciler` writes it into the `Network`'s namespace with an
owner reference to the `Network`, so the garbage collector removes it. The
operator's own policy is a static manifest in `config/deploy/`. On the channel
side, a `TokenReview` result cache and a per-peer token bucket sit inside
`Authenticator.Authenticate`, and the gRPC server gains its missing bounds.

**Tech Stack:** Go, controller-runtime, `networking.k8s.io/v1`, grpc-go,
envtest, `kind`.

**Spec:** `docs/superpowers/specs/2026-08-17-network-policies-design.md`. Read it
before Task 1. Section references below (§3.2, §5.2, …) are to that document.

## Global Constraints

- **Commit messages use Conventional Commits**, deliberately overriding this
  repository's own sentence-style history: `feat(6b):`, `fix(6b):`,
  `test(6b):`, `docs(6b):`. The subject says what changed; the body says *why*,
  wrapped at **72 columns**. Every commit ends with
  `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- **Go builds and tests:** `nix develop -c make test`. `internal/controller`
  takes about 85 seconds because envtest boots a real API server — normal, not
  a hang. `make test` runs `controller-gen` first, so a new
  `+kubebuilder:rbac` marker lands in `config/rbac/role.yaml` in the same
  invocation that then audits it.
- **The end-to-end run**, in the **foreground**, one cluster at a time:

      systemd-run --scope --user --property=Delegate=yes -- \
        nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e

  `systemd-run --user` needs `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS`,
  which a non-interactive shell does not inherit. The host has 8 GB and no
  swap; `free -g` before assuming anything else.
- **No assertion may be claimed to pass without its output**, and every
  mutation must be *performed*, not reasoned about. Milestone 5 shipped six
  assertions that could not discriminate; mutation found all six and reading
  found none. Milestone 6a found five more, one of them in its own final review.
- **Do not touch `agent/` or `proto/`**, and do not run `make agent`.

---

### Task 1: The per-`Network` policy builder

**Files:**
- Create: `internal/podspec/netpol.go`
- Create: `internal/podspec/netpol_test.go`

**Interfaces:**
- Produces, for Task 2: `podspec.BuildNetworkPolicy(network *spawneryv1alpha1.Network, operatorNamespace string) *networkingv1.NetworkPolicy`, `podspec.NetworkPolicyName(network string) string`, `podspec.OperatorPodLabels() map[string]string`, and the constants `podspec.NamespaceNameLabel`, `podspec.KubeSystemNamespace`, `podspec.DNSPort`, `podspec.AgentPort`.

- [ ] **Step 1: Write the failing test**

Create `internal/podspec/netpol_test.go`. Copy the Apache header from
`internal/podspec/claim.go` verbatim.

```go
package podspec_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
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
	if ref.Controller == nil || !*ref.Controller {
		t.Error("the owner reference must be a controller reference")
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
```

Add `networkingv1 "k8s.io/api/networking/v1"` to the imports.

- [ ] **Step 2: Run it to verify it fails**

Run:

```bash
nix develop -c go test ./internal/podspec/ -run TestBuildNetworkPolicy -v
```

Expected: FAIL to compile — `undefined: podspec.BuildNetworkPolicy`.

- [ ] **Step 3: Write the builder**

Create `internal/podspec/netpol.go` with the Apache header, then:

```go
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

// BuildNetworkPolicy renders the policy that closes the invariant Spawnery has
// carried open since milestone 3b: a Paper server runs online-mode=false, so it
// authenticates nobody and trusts whatever completes the modern-forwarding
// handshake with the right secret. This is what restricts who may attempt it.
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
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
nix develop -c go test ./internal/podspec/ -run TestBuildNetworkPolicy -v
```

Expected: five subtests PASS.

- [ ] **Step 5: Verify each assertion can fail**

Perform each mutation, run the command, record the output, revert:

1. Delete `servers[LabelRole] = RoleServer`. Expected: the selector test fails
   reporting two labels where three were wanted. **This is the mutation that
   matters most** — without the role term the policy also selects proxy pods,
   which is the failure design §3.3 avoids.
2. Add `NamespaceSelector: &metav1.LabelSelector{}` to the ingress peer.
   Expected: `the ingress peer carries a namespaceSelector`.
3. Split the operator egress peer into two peers, one with each selector.
   Expected: `got 2 operator peers, want exactly one`.
4. Drop `networkingv1.PolicyTypeEgress` from `PolicyTypes`. Expected:
   `policyTypes = [Ingress], want both`. Note that the object would still be
   accepted by a real API server — that is why this test exists.
5. Set `Controller: ptr.To(false)`. Expected: `the owner reference must be a
   controller reference`.

- [ ] **Step 6: Commit**

```bash
git add internal/podspec/netpol.go internal/podspec/netpol_test.go
git commit
```

Subject: `feat(6b): render the policy that admits only a network's own proxies`.
The body says why it selects servers and not proxies, and why it carries an
owner reference where `BuildDataClaim` refuses one.

---

### Task 2: The `Network` controller writes it

**Files:**
- Modify: `internal/controller/network_controller.go`
- Modify: `internal/controller/setup.go` (a new `Options` field, threaded to the reconciler)
- Modify: `cmd/spawnery-operator/main.go` (pass the operator namespace)
- Modify: `internal/rbacaudit/required.go` (five entries)
- Modify: `internal/controller/network_controller_test.go` (envtest)

**Interfaces:**
- Consumes: `podspec.BuildNetworkPolicy`, `podspec.NetworkPolicyName` (Task 1).
- Produces: `Options.OperatorNamespace string`, and `NetworkReconciler.OperatorNamespace string`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/controller/network_controller_test.go`. The fixture is already
there and needs no extension: `newFixture(t)` creates a namespace with a
`production` Network in it, `networkReconciler(f)` builds the reconciler,
`f.reconcileNetwork(t, r, name)` drives one pass, and `f.c`, `f.ctx`, `f.ns` and
`f.clock` are the cluster handles. The one new thing any of these tests needs is
a reconciler whose `OperatorNamespace` differs, and that is a field assignment
rather than a helper.

```go
// policyKey is where a network's policy lives, so no test has to restate it.
func policyKey(f *fixture, network string) types.NamespacedName {
	return types.NamespacedName{
		Namespace: f.ns,
		Name:      podspec.NetworkPolicyName(network),
	}
}

// TestAnAcceptedNetworkGetsItsPolicy is the milestone's central object claim.
func TestAnAcceptedNetworkGetsItsPolicy(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	f.reconcileNetwork(t, r, "production")

	var policy networkingv1.NetworkPolicy
	if err := f.c.Get(f.ctx, policyKey(f, "production"), &policy); err != nil {
		t.Fatalf("get the network policy: %v", err)
	}
	if got := policy.Spec.PodSelector.MatchLabels[podspec.LabelRole]; got != podspec.RoleServer {
		t.Errorf("policy selects role %q, want %q", got, podspec.RoleServer)
	}
	network := f.getNetwork(t, "production")
	if len(policy.OwnerReferences) != 1 || policy.OwnerReferences[0].UID != network.UID {
		t.Errorf("owner references = %v, want one naming the Network's UID %s",
			policy.OwnerReferences, network.UID)
	}
}

// TestARejectedNetworkWritesNoPolicy: pickNamespaceOwner already decides which
// Network owns a namespace when several exist. If the loser wrote one too, two
// Network objects would overwrite each other's policy on every pass, and which
// object survived would depend on reconcile ordering.
func TestARejectedNetworkWritesNoPolicy(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)

	// The fixture's "production" already exists; a younger one loses, because
	// age decides before the name does.
	f.clock.Advance(time.Minute)
	loser := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "other-secret"},
		},
	}
	if err := f.c.Create(f.ctx, loser); err != nil {
		t.Fatalf("create the second network: %v", err)
	}

	f.reconcileNetwork(t, r, "staging")

	var policy networkingv1.NetworkPolicy
	err := f.c.Get(f.ctx, policyKey(f, "staging"), &policy)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a rejected Network wrote a policy (err = %v); two Networks in "+
			"one namespace would then fight over the namespace's traffic rules", err)
	}
}

// TestADeletedPolicyComesBack: the policy is a security control, so removing it
// by hand must not be a durable way to switch it off.
func TestADeletedPolicyComesBack(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)
	f.reconcileNetwork(t, r, "production")

	var policy networkingv1.NetworkPolicy
	if err := f.c.Get(f.ctx, policyKey(f, "production"), &policy); err != nil {
		t.Fatalf("get the network policy: %v", err)
	}
	if err := f.c.Delete(f.ctx, &policy); err != nil {
		t.Fatalf("delete the network policy: %v", err)
	}

	f.reconcileNetwork(t, r, "production")

	if err := f.c.Get(f.ctx, policyKey(f, "production"), &policy); err != nil {
		t.Fatalf("the policy did not come back: %v", err)
	}
}

// TestTheOperatorNamespaceReachesTheEgressRule guards the one value the policy
// cannot derive from the Network it protects. The agent endpoint is assembled
// from the operator's own namespace, which is a flag (--operator-namespace), so
// a policy hard-coding "spawnery-system" would be correct only by coincidence
// in any installation that moved it.
func TestTheOperatorNamespaceReachesTheEgressRule(t *testing.T) {
	f := newFixture(t)
	r := networkReconciler(f)
	r.OperatorNamespace = "spawnery-elsewhere"

	f.reconcileNetwork(t, r, "production")

	var policy networkingv1.NetworkPolicy
	if err := f.c.Get(f.ctx, policyKey(f, "production"), &policy); err != nil {
		t.Fatalf("get the network policy: %v", err)
	}
	last := policy.Spec.Egress[len(policy.Spec.Egress)-1]
	if last.To[0].NamespaceSelector == nil {
		t.Fatalf("the operator egress rule has no namespace selector: %+v", last.To[0])
	}
	got := last.To[0].NamespaceSelector.MatchLabels[podspec.NamespaceNameLabel]
	if got != "spawnery-elsewhere" {
		t.Errorf("egress names namespace %q, want spawnery-elsewhere", got)
	}
}
```

`networkReconciler(f)` must set `OperatorNamespace` to something non-empty for
the first three tests; use `"spawnery-system"` there, matching what
`config/deploy/` installs.

- [ ] **Step 2: Run them to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestAnAcceptedNetworkGetsItsPolicy|TestARejectedNetworkWritesNoPolicy|TestADeletedPolicyComesBack|TestTheOperatorNamespaceReachesTheEgressRule' -v
```

Expected: FAIL — the policy is never created, so the `Get` calls report
`NotFound`.

- [ ] **Step 3: Add the field and thread it**

In `internal/controller/setup.go`, in `Options`:

```go
	// OperatorNamespace is where the operator itself runs. The per-Network
	// NetworkPolicy needs it for its egress rule, which has to name the
	// namespace the agents dial into; AgentEndpoint above is built from the
	// same flag, and the two must not be allowed to disagree.
	OperatorNamespace string
```

Pass it through to the `NetworkReconciler` where `SetupAll` constructs it, and
add the matching field to the reconciler struct in
`internal/controller/network_controller.go`:

```go
	// OperatorNamespace is where this operator runs, and it is the one value
	// the policy cannot derive from the Network it protects.
	OperatorNamespace string
```

In `cmd/spawnery-operator/main.go`, where `Options` is built, add
`OperatorNamespace: operatorNamespace` beside the existing
`AgentEndpoint: agentEndpoint(operatorNamespace)`.

- [ ] **Step 4: Write the reconcile step**

In `internal/controller/network_controller.go`, add the marker beside the
existing ones on `Reconcile`:

```go
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update
```

Add the method:

```go
// reconcileNetworkPolicy keeps the policy that admits only this network's own
// proxies to its own backends. It carries no delete: the owner reference on
// the object means the garbage collector removes it when the Network goes,
// which is why internal/rbacaudit's table has none either.
func (r *NetworkReconciler) reconcileNetworkPolicy(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
) error {
	desired := podspec.BuildNetworkPolicy(network, r.OperatorNamespace)
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		policy.Labels = desired.Labels
		policy.OwnerReferences = desired.OwnerReferences
		policy.Spec = desired.Spec
		return nil
	})
	return err
}
```

Call it immediately after the ownership branch in `Reconcile` — the earliest
point at which this `Network` is known to be the namespace's owner — and return
the error:

```go
	// The policy, before anything else this reconcile does. A Forbidden here
	// is a security control failing to land, and it must not pass silently:
	// returning the error logs it and requeues. It deliberately does not
	// become a condition on the Network — the design's §2.4 argues that this
	// shape needs no report, and an error that appears only under an RBAC
	// misconfiguration is a fact about the installation rather than about
	// this object.
	if err := r.reconcileNetworkPolicy(ctx, network); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile the network policy: %w", err)
	}
```

And in `SetupWithManager`:

```go
		Owns(&networkingv1.NetworkPolicy{}).
```

with a sentence added to that function's existing doc comment saying why: a
hand-deleted policy comes back on the next event rather than waiting out
`resyncInterval`.

- [ ] **Step 5: Add the RBAC table entries**

In `internal/rbacaudit/required.go`, in `RequiredCluster`:

```go
	// NetworkPolicies — one per accepted Network, written into that Network's
	// own namespace, which is why this is cluster-wide: game namespaces are
	// discovered at runtime and no install-time list of them exists.
	//
	// No delete and no patch, deliberately, and the omission is enforced
	// rather than merely documented: the policy carries an owner reference to
	// its Network, so the garbage collector removes it. A delete marker added
	// later turns this suite red in both directions before it can ship, the
	// same way the persistentvolumeclaims grant above works.
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "get", Why: "NetworkReconciler.reconcileNetworkPolicy reads before it writes"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "list", Why: "NetworkReconciler Owns(&networkingv1.NetworkPolicy{})"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "watch", Why: "NetworkReconciler Owns(&networkingv1.NetworkPolicy{})"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "create", Why: "NetworkReconciler.reconcileNetworkPolicy creates the per-network policy"},
	{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "update", Why: "NetworkReconciler.reconcileNetworkPolicy keeps it in step with the Network"},
```

- [ ] **Step 6: Run the whole suite**

```bash
nix develop -c make test
```

Expected: PASS. `make test` regenerates `config/rbac/role.yaml` from the new
marker before auditing it, so the table and the role are compared in the same
invocation. If `internal/rbacaudit` fails, read the message: it names the exact
triple and which side is missing.

- [ ] **Step 7: Verify the assertions can fail**

Perform, run, record, revert:

1. Return `nil` from `reconcileNetworkPolicy` without creating anything.
   Expected: `TestAnAcceptedNetworkGetsItsPolicy` and `TestADeletedPolicyComesBack`
   fail with `NotFound`; the rejected-network test still passes, which is what
   shows it is not riding on its neighbours.
2. Move the `reconcileNetworkPolicy` call *above* the ownership branch.
   Expected: `TestARejectedNetworkWritesNoPolicy` fails — a loser wrote a policy.
3. Hard-code `"spawnery-system"` in place of `r.OperatorNamespace`. Expected:
   `TestTheOperatorNamespaceReachesTheEgressRule` fails naming both namespaces.
4. Remove `create` from the new marker and run `nix develop -c make test`.
   Expected: `internal/rbacaudit` fails naming
   `networking.k8s.io/networkpolicies:create` as listed by the table and never
   granted by the role. Revert and re-run `make manifests`, confirming
   `git diff` on `config/rbac/role.yaml` is clean.

- [ ] **Step 8: Commit**

```bash
git add internal/ cmd/ config/rbac/role.yaml
git commit
```

Subject: `feat(6b): the accepted network writes its own traffic rules`.

---

### Task 3: The operator's own policy

**Files:**
- Create: `config/deploy/networkpolicy.yaml`
- Modify: `internal/rbacaudit/deploy_envtest_test.go`

**Interfaces:**
- Consumes: `podspec.OperatorPodLabels()` and `podspec.AgentPort` (Task 1).

- [ ] **Step 1: Write the failing test**

Add to `internal/rbacaudit/deploy_envtest_test.go`, beside the other manifest
tests. `readManifest` there is already strict, so a mistyped key fails loudly.

```go
// TestTheAgentPolicySelectsTheOperatorAndAdmitsManagedPods checks the one
// shipped NetworkPolicy, and every hop of it lives in a different file.
//
// Two mistakes it exists to catch. A podSelector copied from a managed pod's
// labels selects nothing here — the operator pod deliberately does not carry
// spawnery.cloud/managed-by — and a policy that selects nothing fails open,
// which looks exactly like one that works. And a peer without an empty
// namespaceSelector would admit only pods in spawnery-system, while every
// managed pod in the cluster dials in from its own game namespace.
func TestTheAgentPolicySelectsTheOperatorAndAdmitsManagedPods(t *testing.T) {
	var policy networkingv1.NetworkPolicy
	var deploy appsv1.Deployment
	readManifest(t, "config/deploy/networkpolicy.yaml", &policy)
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

	if policy.Namespace != deploy.Namespace {
		t.Errorf("policy namespace = %q, deployment namespace = %q — a "+
			"NetworkPolicy only governs pods in its own namespace",
			policy.Namespace, deploy.Namespace)
	}

	podLabels := deploy.Spec.Template.Labels
	for k, v := range policy.Spec.PodSelector.MatchLabels {
		if podLabels[k] != v {
			t.Errorf("the policy selects %s=%q but the operator pod carries "+
				"%s=%q — the policy would select nothing, and a policy that "+
				"selects nothing fails open", k, v, k, podLabels[k])
		}
	}
	if len(policy.Spec.PodSelector.MatchLabels) == 0 {
		t.Error("an empty podSelector selects every pod in the namespace")
	}

	var agentRule, probeRule *networkingv1.NetworkPolicyIngressRule
	for i := range policy.Spec.Ingress {
		rule := &policy.Spec.Ingress[i]
		if len(rule.From) == 0 {
			probeRule = rule
			continue
		}
		agentRule = rule
	}

	if agentRule == nil {
		t.Fatal("no ingress rule with a peer: nothing admits the agents")
	}
	if len(agentRule.From) != 1 {
		t.Fatalf("the agent rule has %d peers, want exactly one", len(agentRule.From))
	}
	peer := agentRule.From[0]
	if peer.NamespaceSelector == nil || len(peer.NamespaceSelector.MatchLabels) != 0 {
		t.Errorf("the agent peer's namespaceSelector = %v, want an empty one — "+
			"every managed pod dials in from its own game namespace, and the "+
			"operator's chart cannot know those names", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil ||
		peer.PodSelector.MatchLabels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Errorf("the agent peer must select %s=%s; got %v",
			podspec.LabelManagedBy, podspec.ManagedByValue, peer.PodSelector)
	}
	if len(agentRule.Ports) != 1 || agentRule.Ports[0].Port.IntValue() != int(podspec.AgentPort) {
		t.Errorf("the agent rule admits %v, want only %d", agentRule.Ports, podspec.AgentPort)
	}

	// Selecting the pod at all makes it default-deny for ingress, which covers
	// the kubelet's probes and any metrics scrape. Both have to be admitted
	// explicitly or the operator goes NotReady the moment this policy lands.
	if probeRule == nil {
		t.Fatal("no peerless ingress rule: the kubelet's probe to the health " +
			"port is denied, and the operator goes NotReady")
	}
	admitted := map[int]bool{}
	for _, p := range probeRule.Ports {
		admitted[p.Port.IntValue()] = true
	}
	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want exactly one", len(deploy.Spec.Template.Spec.Containers))
	}
	for _, p := range deploy.Spec.Template.Spec.Containers[0].Ports {
		if p.Name == "agent" {
			continue
		}
		if !admitted[int(p.ContainerPort)] {
			t.Errorf("the container declares port %q (%d) and the policy does "+
				"not admit it", p.Name, p.ContainerPort)
		}
	}
}
```

Add `networkingv1 "k8s.io/api/networking/v1"` and the `podspec` import if the
file lacks them.

- [ ] **Step 2: Run it to verify it fails**

```bash
nix develop -c go test ./internal/rbacaudit/ -run TestTheAgentPolicySelectsTheOperator -v
```

Expected: FAIL — `read config/deploy/networkpolicy.yaml: ... no such file`.

- [ ] **Step 3: Write the manifest**

Create `config/deploy/networkpolicy.yaml`:

```yaml
# The agent endpoint's ingress rule, and the network-independent half of
# milestone 6b. The per-Network policy that protects the backends is written by
# the operator itself, into each game namespace, because those namespaces are
# discovered at runtime and no install-time list of them exists.
#
# Selecting the operator pod at all makes it default-deny for ingress. That is
# the point for 9443, and it is why the second rule below exists: the kubelet's
# probes and any metrics scrape would otherwise be denied along with everything
# else, and the operator would go NotReady the moment this lands.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: spawnery-operator-agent
  namespace: spawnery-system
  labels:
    app.kubernetes.io/name: spawnery
    app.kubernetes.io/component: operator
spec:
  # The operator pod does NOT carry spawnery.cloud/managed-by. The two ends of
  # the agent channel need different rules, so this selects the operator by its
  # own two labels -- the same pair config/deploy/service.yaml selects on.
  podSelector:
    matchLabels:
      app.kubernetes.io/name: spawnery
      app.kubernetes.io/component: operator
  policyTypes:
    - Ingress
  ingress:
    # The agents. An empty namespaceSelector means every namespace, which is
    # required rather than lax: agentEndpoint builds the dial name from the
    # operator's own namespace, so every managed pod in every game namespace
    # dials into this one, and the chart cannot know those names.
    - from:
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              spawnery.cloud/managed-by: spawnery-operator
      ports:
        - protocol: TCP
          port: 9443
    # The kubelet's liveness and readiness probes, and a metrics scrape. No
    # `from`: the kubelet's source is the node rather than a pod, so there is
    # no selector that would name it, and whether kubelet traffic is subject to
    # policy at all is CNI-dependent. Admitting these two ports from anywhere
    # is the only formulation that is correct on every CNI.
    - ports:
        - protocol: TCP
          port: 8081
        - protocol: TCP
          port: 8080
```

- [ ] **Step 4: Run the suite**

```bash
nix develop -c make test
```

Expected: PASS.

- [ ] **Step 5: Run the end-to-end suite, because this is where it first bites**

`hack/e2e.sh` applies `config/deploy/` wholesale, so from this commit onward
every run stands the operator up behind its own policy.

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

Expected: PASS, twelve subtests. **If the rollout times out here, the policy is
denying the kubelet's probe** — that is acceptance criterion 6 failing, and it
is a finding about the manifest rather than about the harness. Report it with
the pod's conditions rather than working around it.

- [ ] **Step 6: Verify the assertions can fail**

Perform, run, record, revert:

1. Change the policy's `podSelector` to `spawnery.cloud/managed-by:
   spawnery-operator`. Expected: the selector comparison fails naming the label
   the operator pod does not carry. This is the mutation that matters: the
   mutated policy is valid YAML, the API server accepts it, and it protects
   nothing.
2. Delete the second (peerless) ingress rule. Expected: `no peerless ingress
   rule: the kubelet's probe ... is denied`. Then run `make e2e` with it still
   deleted and record what happens — if the run stays green, that tells you
   kindnet is not enforcing, which is the fact §8 of the design says must be
   established rather than assumed.
3. Remove `namespaceSelector: {}` from the agent peer. Expected: `the agent
   peer's namespaceSelector = <nil>, want an empty one`.

- [ ] **Step 7: Commit**

```bash
git add config/deploy/networkpolicy.yaml internal/rbacaudit/deploy_envtest_test.go
git commit
```

Subject: `feat(6b): the agent endpoint stops accepting every pod in the cluster`.
The body records what mutation 2's `make e2e` run showed about kindnet.

---

### Task 4: The gRPC server's missing bounds

**Files:**
- Modify: `internal/agentserver/server.go`
- Modify: `internal/agentserver/server_test.go` (or the file that holds this package's tests)

**Interfaces:**
- Produces: the exported constants `agentserver.MaxConcurrentStreams`, `agentserver.ConnectionTimeout`, `agentserver.MaxConnectionIdle`, `agentserver.MinKeepaliveInterval`.

- [ ] **Step 1: Write the failing test**

Add this to `internal/agentserver/server_envtest_test.go`, which already has
everything it needs: `newServerFixture(t)` gives `f.ctx`, `f.addr` and `f.ca`,
`f.pod(name)` creates a server pod, and `f.token(sa, audiences, boundTo)` mints
a token the authenticator accepts. `dialAgent` in the same file opens one
`ServerSession` per connection, so this test dials once itself rather than
reusing it.

```go
// TestTheServerBoundsStreamsPerConnection is the one new bound that can be
// observed from outside. An agent opens exactly one stream -- the proto has two
// RPCs and a session uses one of them -- so the limit is generous by design;
// what it stops is a single connection multiplexing an unbounded number.
//
// Be precise about what this does NOT bound, because the convenient reading is
// that it closes the availability gap and it does not: MaxConcurrentStreams is
// per connection, so a pod that opens many connections is untouched by it.
// That is what grpcauth's per-peer rate limit is for.
func TestTheServerBoundsStreamsPerConnection(t *testing.T) {
	f := newServerFixture(t)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(f.ca) {
		t.Fatal("CA bundle unusable")
	}
	creds := credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: "spawnery-operator.spawnery-system.svc",
		MinVersion: tls.VersionTLS13,
	})
	// One connection, many streams: that is the shape the bound governs.
	conn, err := grpc.NewClient(f.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	client := agentpb.NewAgentServiceClient(conn)

	open := func(ctx context.Context, i int) (
		grpc.BidiStreamingClient[agentpb.ServerMessage, agentpb.OperatorToServer], error) {
		pod := f.pod(fmt.Sprintf("lobby-stream-%d", i))
		token := f.token(podspec.ServerServiceAccountName,
			[]string{podspec.AgentTokenAudience}, pod)
		streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		return client.ServerSession(streamCtx)
	}

	for i := 0; i < int(agentserver.MaxConcurrentStreams); i++ {
		stream, err := open(f.ctx, i)
		if err != nil {
			t.Fatalf("stream %d of the permitted %d was refused: %v",
				i, agentserver.MaxConcurrentStreams, err)
		}
		// Hold it open: a stream is only concurrent while it lives.
		if err := stream.Send(&agentpb.ServerMessage{
			Message: &agentpb.ServerMessage_Hello{
				Hello: &agentpb.Hello{Version: "0.1.0", Ready: false},
			},
		}); err != nil {
			t.Fatalf("send Hello on stream %d: %v", i, err)
		}
	}

	// One past the limit. HTTP/2 queues an over-limit stream rather than
	// refusing it, so the observable is a bounded wait: with the bound in
	// force the first Recv does not return, without it the server answers.
	// ServerSession itself returns immediately either way, because grpc-go
	// creates the stream lazily.
	over, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	extra, err := open(over, int(agentserver.MaxConcurrentStreams))
	if err != nil {
		return // refused outright, which also satisfies the bound
	}
	if _, err := extra.Recv(); err == nil {
		t.Errorf("stream %d was served; MaxConcurrentStreams is not in force",
			agentserver.MaxConcurrentStreams+1)
	} else if status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("stream %d failed with %v, want the deadline — a different "+
			"error means it was refused for some other reason and this test "+
			"proves nothing about the bound", agentserver.MaxConcurrentStreams+1, err)
	}
}
```

The imports it needs (`crypto/tls`, `crypto/x509`, `fmt`,
`google.golang.org/grpc`, `.../credentials`, `.../metadata`, `.../status`,
`.../codes`) are all already used elsewhere in this file except `fmt` and
`codes`; add those two.

- [ ] **Step 2: Run it to verify it fails**

```bash
nix develop -c go test ./internal/agentserver/ -run TestTheServerBoundsStreams -v
```

Expected: FAIL — `undefined: agentserver.MaxConcurrentStreams`.

- [ ] **Step 3: Add the bounds**

In `internal/agentserver/server.go`, above the constructor:

```go
const (
	// MaxConcurrentStreams bounds streams on ONE connection. An agent opens
	// exactly one -- proto/spawnery/agent/v1alpha1 has two RPCs and a session
	// uses one of them -- so this is generous by an order of magnitude. What it
	// does not bound is how many connections a pod may open, which is the
	// documented attack and is grpcauth's rate limit's job.
	MaxConcurrentStreams uint32 = 8

	// ConnectionTimeout bounds how long a half-finished handshake holds
	// resources. grpc-go's default is two minutes.
	ConnectionTimeout = 30 * time.Second

	// MaxConnectionIdle reaps a connection carrying no stream. An agent's
	// session stream is long-lived, so a connection that has been idle this
	// long has lost its agent.
	MaxConnectionIdle = 5 * time.Minute

	// MinKeepaliveInterval is how often a client may ping. The agents send no
	// keepalive at all -- agent/common's SessionLoop says so in its own
	// comment: "the channel underneath has no keepalive, no idle timeout" --
	// so this cannot throttle a legitimate agent. It bounds a client that
	// decides to ping in a loop.
	MinKeepaliveInterval = 30 * time.Second
)
```

and extend the constructor:

```go
	grpcServer := grpc.NewServer(
		grpc.Creds(creds),
		grpc.StreamInterceptor(s.opts.Auth.StreamInterceptor()),
		grpc.MaxConcurrentStreams(MaxConcurrentStreams),
		grpc.ConnectionTimeout(ConnectionTimeout),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: MaxConnectionIdle,
		}),
		// PermitWithoutStream is false: a client with no active stream has no
		// reason to ping, and the agents never do.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             MinKeepaliveInterval,
			PermitWithoutStream: false,
		}),
	)
```

Import `"google.golang.org/grpc/keepalive"`.

- [ ] **Step 4: Run it to verify it passes**

```bash
nix develop -c go test ./internal/agentserver/ -v
```

Expected: PASS, including the package's existing tests — a keepalive policy
that is stricter than a test client's settings would show up here as
`ENHANCE_YOUR_CALM`.

- [ ] **Step 5: Verify the assertion can fail**

Remove `grpc.MaxConcurrentStreams(MaxConcurrentStreams)` from the constructor
and re-run. Expected: the over-limit stream is served and the test reports
`MaxConcurrentStreams is not in force`. Record the output and revert.

- [ ] **Step 6: Run `make agent-test`, and say whether you did**

The agents are the real clients of this endpoint, and `hack/agent-test.sh` runs
both JUnit suites against a stub operator. **This task changes the server's
connection policy, so it is the one task in this milestone where the agent side
can regress.** Do not run `make agent` (the Gradle build exhausted this host at
its previous size); `make agent-test` builds the images and runs the harness.
If it is too slow or fails for reasons unrelated to this change, say so
explicitly in the report rather than skipping it silently.

- [ ] **Step 7: Commit**

```bash
git add internal/agentserver/
git commit
```

Subject: `feat(6b): bound the agent endpoint's streams and connections`.
The body states what each bound actually bounds, and what none of them does.

---

### Task 5: The `TokenReview` result cache

**Files:**
- Create: `internal/grpcauth/cache.go`
- Create: `internal/grpcauth/cache_test.go`
- Modify: `internal/grpcauth/identity.go` (split `Authenticate`, wire the cache)
- Modify: `internal/grpcauth/metrics.go`
- Modify: `cmd/spawnery-operator/main.go` (construct the cache)

**Interfaces:**
- Produces, for Task 6: `Authenticator.Cache *ReviewCache`, `NewReviewCache(now func() time.Time) *ReviewCache`, and the split-out method `(*Authenticator).reviewToken(ctx, token) (reviewResult, error)`.

- [ ] **Step 1: Write the failing test**

Create `internal/grpcauth/cache_test.go` with the Apache header.

```go
package grpcauth

import (
	"errors"
	"testing"
	"time"
)

func TestReviewCacheServesAPositiveAnswerUntilItExpires(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	want := reviewResult{Namespace: "minecraft", PodName: "lobby-abc"}
	c.store("token", want, nil)

	got, err, ok := c.lookup("token")
	if !ok {
		t.Fatal("a freshly stored entry was not served")
	}
	if err != nil || got.PodName != want.PodName {
		t.Fatalf("lookup = (%+v, %v), want (%+v, nil)", got, err, want)
	}

	now = now.Add(PositiveTTL - time.Second)
	if _, _, ok := c.lookup("token"); !ok {
		t.Error("the entry expired before its TTL")
	}

	now = now.Add(2 * time.Second)
	if _, _, ok := c.lookup("token"); ok {
		t.Error("the entry outlived its TTL")
	}
}

// A cached "no" heals faster than a cached "yes" on purpose: a rejection that
// was wrong -- clock skew, a token checked before its ServiceAccount existed --
// should not stick, while a cached "yes" is what removes the API server load.
func TestReviewCacheForgetsANegativeAnswerSooner(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	c.store("bad", reviewResult{}, errors.New("token not authenticated"))

	if _, err, ok := c.lookup("bad"); !ok || err == nil {
		t.Fatal("a stored rejection was not served back as a rejection")
	}

	now = now.Add(NegativeTTL + time.Second)
	if _, _, ok := c.lookup("bad"); ok {
		t.Error("the rejection outlived NegativeTTL")
	}
	if NegativeTTL >= PositiveTTL {
		t.Errorf("NegativeTTL (%s) must be shorter than PositiveTTL (%s)",
			NegativeTTL, PositiveTTL)
	}
}

// The whole point of the split in Authenticate: an outage must not be cached,
// or it outlives itself. internal/grpcauth already distinguishes the two, and
// the interceptor already maps this one to codes.Unavailable so an agent backs
// off rather than concluding its credentials are wrong.
func TestReviewCacheRefusesToStoreAnOutage(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	c.store("token", reviewResult{}, wrapUnavailable(errors.New("apiserver down")))

	if _, _, ok := c.lookup("token"); ok {
		t.Error("an API server outage was cached; it would outlive the outage")
	}
}

// The operator must not hold bearer tokens in a map.
func TestReviewCacheDoesNotKeepTheTokenItself(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	c.store("super-secret-token", reviewResult{Namespace: "minecraft"}, nil)

	for key := range c.entries {
		if key == "super-secret-token" {
			t.Fatal("the cache is keyed on the raw token")
		}
	}
}

// Without eviction the map grows for as long as distinct tokens arrive. The
// rate limit in the interceptor bounds how fast that can happen, and this
// bounds how large it gets in between.
func TestReviewCacheEvictsExpiredEntriesAsItGrows(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	c := NewReviewCache(func() time.Time { return now })

	for i := 0; i < maxCacheEntries+1; i++ {
		c.store(fmt.Sprintf("token-%d", i), reviewResult{}, nil)
	}
	now = now.Add(PositiveTTL + time.Second)
	c.store("one-more", reviewResult{}, nil)

	if len(c.entries) > maxCacheEntries {
		t.Errorf("the cache holds %d entries past its bound of %d",
			len(c.entries), maxCacheEntries)
	}
}
```

Add `"fmt"` to the imports.

- [ ] **Step 2: Run it to verify it fails**

```bash
nix develop -c go test ./internal/grpcauth/ -run TestReviewCache -v
```

Expected: FAIL to compile — `undefined: NewReviewCache`.

- [ ] **Step 3: Write the cache**

Create `internal/grpcauth/cache.go` with the Apache header.

```go
package grpcauth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	// PositiveTTL is how long an accepted token's review is reused. Projected
	// agent tokens live 600 seconds (podspec.TokenExpirationSeconds) and the
	// kubelet rotates them, so this sits well inside one token's life.
	//
	// What it can delay is narrow, and the narrowing is the design: only the
	// TokenReview is cached, never the pod lookup, so deleting a pod -- the
	// revocation an operator actually performs -- takes effect on the very
	// next connection attempt whatever this cache holds.
	PositiveTTL = 60 * time.Second

	// NegativeTTL is how long a refusal is reused, deliberately shorter. A
	// cached "no" that was wrong should heal quickly; a cached "yes" is what
	// removes the load.
	NegativeTTL = 10 * time.Second

	// maxCacheEntries bounds the map between evictions.
	maxCacheEntries = 1024
)

// reviewResult is what a TokenReview establishes about a token on its own,
// independent of which role the caller asked for. The role check is
// deliberately not in here: it varies per call, so caching it would let one
// agent's rejection answer another agent's question.
type reviewResult struct {
	Namespace      string
	ServiceAccount string
	PodName        string
	PodUID         string
}

type cacheEntry struct {
	result  reviewResult
	reason  string // empty when the review succeeded
	expires time.Time
}

// ReviewCache remembers what the API server said about a token.
type ReviewCache struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
}

// NewReviewCache returns a cache reading time from now.
func NewReviewCache(now func() time.Time) *ReviewCache {
	return &ReviewCache{now: now, entries: map[string]cacheEntry{}}
}

func cacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// lookup returns a remembered answer, if one has not expired.
func (c *ReviewCache) lookup(token string) (reviewResult, error, bool) {
	if c == nil {
		return reviewResult{}, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[cacheKey(token)]
	if !ok || !c.now().Before(entry.expires) {
		return reviewResult{}, nil, false
	}
	if entry.reason != "" {
		return reviewResult{}, errors.New(entry.reason), true
	}
	return entry.result, nil, true
}

// store remembers an answer. An API server outage is never remembered: it says
// nothing about the token, and caching it would extend the outage past its end.
//
// A refusal is stored as its message rather than as the error value, which
// loses the error's type. That is safe precisely because the one type that
// matters -- unavailableErr -- is the one case this never stores.
func (c *ReviewCache) store(token string, result reviewResult, err error) {
	if c == nil || isUnavailable(err) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= maxCacheEntries {
		c.evictExpiredLocked()
	}

	entry := cacheEntry{result: result, expires: c.now().Add(PositiveTTL)}
	if err != nil {
		entry = cacheEntry{reason: err.Error(), expires: c.now().Add(NegativeTTL)}
	}
	c.entries[cacheKey(token)] = entry
}

func (c *ReviewCache) evictExpiredLocked() {
	now := c.now()
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, key)
		}
	}
}
```

- [ ] **Step 4: Split `Authenticate` and wire the cache**

In `internal/grpcauth/identity.go`, add the field:

```go
type Authenticator struct {
	Reviews  TokenReviewer
	Pods     PodChecker
	Audience string

	// Cache remembers what the API server said about a token. Optional: a nil
	// cache reviews every time, which is what the tests that predate it do.
	Cache *ReviewCache
}
```

Split the token-only half out of `Authenticate` into a method, moving the
existing body verbatim as far as the pod lookup:

```go
// reviewToken is everything a TokenReview establishes about a token by itself:
// that the API server authenticated it for our audience, that the subject is a
// ServiceAccount, and which pod it is bound to. It stops short of the role
// check and the pod lookup, which is what makes its answer cacheable -- the
// role varies per call, and the pod lookup is the half that must stay live.
func (a *Authenticator) reviewToken(ctx context.Context, token string) (reviewResult, error) {
	review, err := a.Reviews.Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{a.Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return reviewResult{}, wrapUnavailable(fmt.Errorf("token review unavailable: %w", err))
	}
	if !review.Status.Authenticated {
		return reviewResult{}, fmt.Errorf("token not authenticated: %s", review.Status.Error)
	}
	if !containsString(review.Status.Audiences, a.Audience) {
		return reviewResult{}, fmt.Errorf("token not authenticated for audience %q", a.Audience)
	}
	namespace, name, ok := splitServiceAccount(review.Status.User.Username)
	if !ok {
		return reviewResult{}, fmt.Errorf("not a service account: %q", review.Status.User.Username)
	}
	podName := firstExtra(review.Status.User.Extra, claimPodName)
	podUID := firstExtra(review.Status.User.Extra, claimPodUID)
	if podName == "" || podUID == "" {
		return reviewResult{}, fmt.Errorf("token is not bound to a pod")
	}
	return reviewResult{
		Namespace:      namespace,
		ServiceAccount: name,
		PodName:        podName,
		PodUID:         podUID,
	}, nil
}
```

and rewrite `Authenticate` around it:

```go
func (a *Authenticator) Authenticate(ctx context.Context, token string, want agent.Role) (Identity, error) {
	if token == "" {
		return Identity{}, fmt.Errorf("no token presented")
	}

	res, err, cached := a.Cache.lookup(token)
	if cached {
		ReviewCacheHits.Inc()
	} else {
		ReviewCacheMisses.Inc()
		res, err = a.reviewToken(ctx, token)
		a.Cache.store(token, res, err)
	}
	if err != nil {
		return Identity{}, err
	}

	// The role check is after the cache on purpose: it depends on which
	// session the caller asked for, not on the token.
	if wantSA := serviceAccountFor(want); res.ServiceAccount != wantSA {
		return Identity{}, fmt.Errorf("service account %q may not open a %s session, %q may",
			res.ServiceAccount, want, wantSA)
	}

	// Never cached. This is the half that ties an identity to a live pod, and
	// keeping it live is what makes deleting a pod an immediate revocation.
	group, exists, err := a.Pods.LookupPod(ctx, res.Namespace, res.PodName, res.PodUID, want)
	if err != nil {
		return Identity{}, wrapUnavailable(fmt.Errorf("look up pod %s/%s: %w", res.Namespace, res.PodName, err))
	}
	if !exists {
		return Identity{}, fmt.Errorf("pod %s/%s is not a Spawnery pod", res.Namespace, res.PodName)
	}

	return Identity{
		Namespace:      res.Namespace,
		PodName:        res.PodName,
		PodUID:         res.PodUID,
		ServiceAccount: res.ServiceAccount,
		Role:           want,
		Group:          group,
	}, nil
}
```

In `internal/grpcauth/metrics.go`, beside `AuthFailures`:

```go
// ReviewCacheHits and ReviewCacheMisses make the cache visible. Milestone 6a
// established that a mechanism reporting nothing is indistinguishable from an
// absent one, and a cache nobody can see cannot be shown to be working.
var (
	ReviewCacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_agent_token_review_cache_hits_total",
		Help: "Token checks answered without asking the API server.",
	})
	ReviewCacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_agent_token_review_cache_misses_total",
		Help: "Token checks that required a TokenReview.",
	})
)
```

and register both in the existing `init()`.

In `cmd/spawnery-operator/main.go`, where the `Authenticator` is constructed,
add `Cache: grpcauth.NewReviewCache(time.Now)`.

- [ ] **Step 5: Run the suite**

```bash
nix develop -c make test
```

Expected: PASS, including `internal/grpcauth`'s existing envtest suite, which
builds real tokens against a real API server and is the check that the split
did not change what `Authenticate` accepts.

- [ ] **Step 6: Verify the assertions can fail**

Perform, run, record, revert:

1. In `store`, drop the `isUnavailable(err)` guard. Expected:
   `TestReviewCacheRefusesToStoreAnOutage` fails.
2. Set `NegativeTTL = PositiveTTL`. Expected: the negative test fails on the
   ordering assertion, naming both durations.
3. Key the cache on `token` instead of `cacheKey(token)`. Expected:
   `TestReviewCacheDoesNotKeepTheTokenItself` fails.
4. In `Authenticate`, move the `LookupPod` call above the cache branch and
   cache its result too. Expected: the existing envtest for a deleted pod —
   find it in `internal/grpcauth/auth_envtest_test.go` — must fail. **If it
   does not, say so**: that would mean nothing covers the property the cache's
   whole line is drawn around, and it needs a test before this task closes.

- [ ] **Step 7: Commit**

```bash
git add internal/grpcauth/ cmd/spawnery-operator/main.go
git commit
```

Subject: `feat(6b): cache the token review, never the pod lookup`.

---

### Task 6: The per-peer rate limit

**Files:**
- Create: `internal/grpcauth/limiter.go`
- Create: `internal/grpcauth/limiter_test.go`
- Modify: `internal/grpcauth/identity.go` (consult it on a cache miss)
- Modify: `internal/grpcauth/interceptor.go` (map the new error)
- Modify: `internal/grpcauth/metrics.go`
- Modify: `cmd/spawnery-operator/main.go`

**Interfaces:**
- Consumes: `Authenticator.Cache` and the cache-miss branch from Task 5.
- Produces: `NewPeerLimiter(now func() time.Time) *PeerLimiter`, `Authenticator.Limiter *PeerLimiter`, and `isExhausted(err) bool`.

- [ ] **Step 1: Write the failing test**

Create `internal/grpcauth/limiter_test.go` with the Apache header.

```go
package grpcauth

import (
	"testing"
	"time"
)

func TestPeerLimiterAllowsABurstThenRefills(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := NewPeerLimiter(func() time.Time { return now })

	for i := 0; i < PeerBurst; i++ {
		if !l.allow("10.244.0.7") {
			t.Fatalf("attempt %d of the permitted burst %d was refused", i, PeerBurst)
		}
	}
	if l.allow("10.244.0.7") {
		t.Fatalf("attempt %d was allowed; the burst is %d", PeerBurst+1, PeerBurst)
	}

	now = now.Add(PeerRefill)
	if !l.allow("10.244.0.7") {
		t.Errorf("one token did not refill after %s", PeerRefill)
	}
	if l.allow("10.244.0.7") {
		t.Errorf("two tokens refilled after %s; the rate is one per interval", PeerRefill)
	}
}

// The key is what makes a rollout safe. Every agent reconnects from its own pod
// IP, so a fleet coming back after an operator restart spends one bucket each
// rather than sharing one. A limiter keyed on anything the whole fleet has in
// common would throttle exactly the case that must not be throttled.
func TestPeerLimiterBucketsArePerPeer(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := NewPeerLimiter(func() time.Time { return now })

	for i := 0; i < PeerBurst; i++ {
		l.allow("10.244.0.7")
	}
	if !l.allow("10.244.0.8") {
		t.Error("a second peer was refused because the first had spent its burst")
	}
}

// A bucket never fills past its burst, or a peer that was quiet for an hour
// could spend an hour's worth of budget at once.
func TestPeerLimiterDoesNotAccumulatePastItsBurst(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	l := NewPeerLimiter(func() time.Time { return now })

	l.allow("10.244.0.7")
	now = now.Add(100 * PeerRefill)

	for i := 0; i < PeerBurst; i++ {
		if !l.allow("10.244.0.7") {
			t.Fatalf("attempt %d was refused after a long quiet period", i)
		}
	}
	if l.allow("10.244.0.7") {
		t.Error("the bucket accumulated past its burst")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
nix develop -c go test ./internal/grpcauth/ -run TestPeerLimiter -v
```

Expected: FAIL to compile — `undefined: NewPeerLimiter`.

- [ ] **Step 3: Write the limiter**

Create `internal/grpcauth/limiter.go` with the Apache header.

```go
package grpcauth

import (
	"sync"
	"time"
)

const (
	// PeerBurst is how many token checks one peer may cause back to back.
	// A legitimate agent misses the cache at most once per token rotation --
	// projected tokens live 600 seconds -- and once per reconnect, so this is
	// generous by an order of magnitude.
	PeerBurst = 5

	// PeerRefill is how long one token takes to come back.
	PeerRefill = 10 * time.Second

	// maxBuckets bounds the map. Pod IPs are recycled and a peer that has
	// refilled to full is indistinguishable from one that never appeared, so
	// full buckets are dropped rather than kept.
	maxBuckets = 4096
)

type bucket struct {
	tokens float64
	last   time.Time
}

// PeerLimiter is a token bucket per peer address.
//
// It is consulted only when the review cache misses, and that is what makes it
// harmless to legitimate traffic and effective against the documented attack.
// A pod in a connection loop replays one token, hits the cache, and never
// reaches this. Feeling it at all requires presenting tokens the cache has not
// seen -- and those cannot be manufactured, because TokenReview is
// audience-bound and the token is signed by the cluster.
type PeerLimiter struct {
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]bucket
}

// NewPeerLimiter returns a limiter reading time from now.
func NewPeerLimiter(now func() time.Time) *PeerLimiter {
	return &PeerLimiter{now: now, buckets: map[string]bucket{}}
}

// allow spends one token for peer, reporting whether there was one.
func (l *PeerLimiter) allow(peer string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, seen := l.buckets[peer]
	if !seen {
		b = bucket{tokens: PeerBurst, last: now}
	} else {
		refilled := now.Sub(b.last).Seconds() / PeerRefill.Seconds()
		b.tokens += refilled
		if b.tokens > PeerBurst {
			b.tokens = PeerBurst
		}
		b.last = now
	}

	if b.tokens < 1 {
		l.buckets[peer] = b
		return false
	}
	b.tokens--

	if len(l.buckets) >= maxBuckets {
		l.evictFullLocked()
	}
	l.buckets[peer] = b
	return true
}

func (l *PeerLimiter) evictFullLocked() {
	for key, b := range l.buckets {
		if b.tokens >= PeerBurst {
			delete(l.buckets, key)
		}
	}
}
```

- [ ] **Step 4: Wire it into the cache-miss branch**

In `internal/grpcauth/identity.go`, add the error type beside `unavailableErr`:

```go
// exhaustedErr marks a refusal caused by the rate limit rather than by the
// credentials. The interceptor maps it to codes.ResourceExhausted, which is
// distinct from both Unauthenticated and Unavailable, so an agent's log says
// which of the three happened.
type exhaustedErr struct{ err error }

func (e *exhaustedErr) Error() string { return e.err.Error() }
func (e *exhaustedErr) Unwrap() error { return e.err }

func wrapExhausted(err error) error { return &exhaustedErr{err} }

func isExhausted(err error) bool {
	var e *exhaustedErr
	return errors.As(err, &e)
}

// peerAddr is who is asking, as far as the transport knows. It is the only
// identity available before the TokenReview, which is exactly why the limit
// keys on it.
func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}
```

Add the field and the check inside the cache-miss branch of `Authenticate`:

```go
	Limiter *PeerLimiter
```

```go
	} else {
		ReviewCacheMisses.Inc()
		if !a.Limiter.allow(peerAddr(ctx)) {
			RateLimited.Inc()
			return Identity{}, wrapExhausted(
				fmt.Errorf("too many token checks from %s", peerAddr(ctx)))
		}
		res, err = a.reviewToken(ctx, token)
		a.Cache.store(token, res, err)
	}
```

Import `"google.golang.org/grpc/peer"`.

In `internal/grpcauth/interceptor.go`, extend the code mapping:

```go
			code := codes.Unauthenticated
			switch {
			case isUnavailable(err):
				code = codes.Unavailable
			case isExhausted(err):
				code = codes.ResourceExhausted
			}
```

In `internal/grpcauth/metrics.go`, add and register:

```go
	RateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "spawnery_agent_rate_limited_total",
		Help: "Token checks refused by the per-peer rate limit.",
	})
```

In `cmd/spawnery-operator/main.go`, add
`Limiter: grpcauth.NewPeerLimiter(time.Now)` beside the cache.

- [ ] **Step 5: Write the failing test for the wiring**

Add to `internal/grpcauth/limiter_test.go`:

```go
// The limit sits behind the cache, and that ordering is the design rather than
// an implementation detail: a pod replaying one token must never reach it.
func TestARepeatedTokenNeverReachesTheLimiter(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	reviews := &countingReviewer{} // stub: counts Create calls, returns an accepted review
	a := &Authenticator{
		Reviews:  reviews,
		Pods:     alwaysFoundPods{}, // stub: LookupPod returns ("lobby", true, nil)
		Audience: "spawnery-operator",
		Cache:    NewReviewCache(clock),
		Limiter:  NewPeerLimiter(clock),
	}

	for i := 0; i < PeerBurst*3; i++ {
		if _, err := a.Authenticate(context.Background(), "one-token", agent.RoleServer); err != nil {
			t.Fatalf("attempt %d was refused: %v", i, err)
		}
	}
	if reviews.calls != 1 {
		t.Errorf("the API server was asked %d times for one token, want 1", reviews.calls)
	}
}
```

The two stubs, in the same file:

```go
// countingReviewer answers every review the same way and counts the asking.
// The count is the whole point: it is what shows the API server was spared.
type countingReviewer struct{ calls int }

func (c *countingReviewer) Create(
	_ context.Context, tr *authnv1.TokenReview, _ metav1.CreateOptions,
) (*authnv1.TokenReview, error) {
	c.calls++
	return &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     tr.Spec.Audiences,
		User: authnv1.UserInfo{
			Username: "system:serviceaccount:minecraft:" + podspec.ServerServiceAccountName,
			Extra: map[string]authnv1.ExtraValue{
				claimPodName: {"lobby-abcd"},
				claimPodUID:  {"5f3a9c1e-0000-4000-8000-000000000002"},
			},
		},
	}}, nil
}

// alwaysFoundPods is the half Authenticate must NOT cache, stubbed to succeed
// so this test measures only the half it must.
type alwaysFoundPods struct{}

func (alwaysFoundPods) LookupPod(
	_ context.Context, _, _, _ string, _ agent.Role,
) (string, bool, error) {
	return "lobby", true, nil
}
```

`internal/grpcauth`'s existing tests may already carry equivalents — read them
first and reuse rather than shipping a second copy under a new name.

- [ ] **Step 6: Run the suite**

```bash
nix develop -c make test
```

Expected: PASS.

- [ ] **Step 7: Verify the assertions can fail**

Perform, run, record, revert:

1. Move the limiter check *above* the cache lookup. Expected:
   `TestARepeatedTokenNeverReachesTheLimiter` fails on the fourth attempt —
   which is the whole design of the ordering, shown rather than asserted.
2. In `allow`, remove the `b.tokens > PeerBurst` clamp. Expected:
   `TestPeerLimiterDoesNotAccumulatePastItsBurst` fails.
3. Key the limiter on a constant instead of `peer`. Expected:
   `TestPeerLimiterBucketsArePerPeer` fails — a second peer refused because the
   first spent its budget, which is the rollout-breaking behaviour.

- [ ] **Step 8: Commit**

```bash
git add internal/grpcauth/ cmd/spawnery-operator/main.go
git commit
```

Subject: `feat(6b): limit token checks per peer, behind the cache`.
The body says why the ordering is load-bearing and why no global ceiling was
added (design §5.4).

---

### Task 7: The end-to-end scenarios

**Files:**
- Create: `test/e2e/netpol_test.go`
- Modify: `test/e2e/e2e_test.go` (two `t.Run` lines)

**Interfaces:**
- Consumes: `eventually`, `applyManifest`, `operatorPod`, `k8s`, `ctx`, `testNamespace`, `operatorNamespace` from `test/e2e`.

- [ ] **Step 1: Write the failing tests**

Create `test/e2e/netpol_test.go` with the `//go:build e2e` tag.

```go
//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/podspec"
)

// theNetworkGetsItsPolicy asserts the object, and only the object.
//
// It cannot assert that anything is blocked, and saying so here rather than in
// a document is deliberate. Two reasons compound. No image in this harness
// resolves, by decision, so no process listens on 25565 and there is nothing to
// connect from or to. And enforcement is a property of the CNI rather than of
// the object: hack/e2e.sh runs a bare `kind create cluster` with the default
// kindnet, and if that CNI drops nothing then "the connection was blocked" and
// "the policy was never applied" produce the same green. The enforcement claim
// belongs to the RKE2 rollout at the end of milestone 6.
func theNetworkGetsItsPolicy(t *testing.T) {
	eventually(t, 2*time.Minute, "the production network's policy", func() (bool, string) {
		var policy networkingv1.NetworkPolicy
		key := client.ObjectKey{
			Namespace: testNamespace,
			Name:      podspec.NetworkPolicyName("production"),
		}
		if err := k8s.Get(ctx, key, &policy); err != nil {
			return false, err.Error()
		}
		if got := policy.Spec.PodSelector.MatchLabels[podspec.LabelRole]; got != podspec.RoleServer {
			return false, fmt.Sprintf("selects role %q", got)
		}
		if len(policy.OwnerReferences) != 1 {
			return false, fmt.Sprintf("%d owner references", len(policy.OwnerReferences))
		}
		return true, ""
	})
}

// theOperatorStaysReadyBehindItsOwnPolicy is acceptance criterion 6, and it is
// the one place milestone 6b touches probe traffic at all.
//
// config/deploy/networkpolicy.yaml selects the operator pod, which makes it
// default-deny for ingress — including the kubelet's probe on the health port.
// If the peerless rule admitting it were wrong, the pod would go NotReady and
// the Deployment would stop being Available. Unlike the block assertions above,
// this one is meaningful even on a CNI that enforces nothing: an unenforced
// policy leaves the pod ready, and so does a correct one, but an enforced and
// wrong one is caught here rather than in a cluster.
func theOperatorStaysReadyBehindItsOwnPolicy(t *testing.T) {
	var policy networkingv1.NetworkPolicy
	key := client.ObjectKey{Namespace: operatorNamespace, Name: "spawnery-operator-agent"}
	if err := k8s.Get(ctx, key, &policy); err != nil {
		t.Fatalf("the operator's own policy was never applied: %v", err)
	}

	// Held rather than sampled: a probe failure takes three periods to move
	// the pod out of Ready, and hack/e2e.sh's rollout wait returned before
	// that could have happened.
	eventuallyStable(t, time.Minute, 20*time.Second,
		"the operator ready behind its own policy", func() (bool, string) {
			pod := operatorPod(t)
			for _, c := range pod.Status.ContainerStatuses {
				if !c.Ready {
					return false, fmt.Sprintf("container not ready, restarts %d", c.RestartCount)
				}
			}
			return true, ""
		})
}
```

Insert both into the ordered driver in `test/e2e/e2e_test.go`, above the final
`t.Run("the operator was never denied", …)`:

```go
	t.Run("the network gets its policy", theNetworkGetsItsPolicy)
	t.Run("the operator stays ready behind its own policy", theOperatorStaysReadyBehindItsOwnPolicy)
```

- [ ] **Step 2: Run to verify they fail**

Temporarily rename the policy the controller writes — change
`NetworkPolicyName` to return `network + "-elsewhere"` — and run:

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

Expected: `timed out ... waiting for the production network's policy`. Revert.

- [ ] **Step 3: Run to verify they pass**

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

Expected: fourteen subtests pass.

- [ ] **Step 4: Establish what kindnet actually enforces, and write it down**

This is the measurement the design's §8 says must be made rather than assumed,
and it decides how every sentence about 6b's proof has to be phrased.

With `E2E_KEEP=1`, keep the cluster and check whether the CNI enforces at all —
for example by applying a deny-all policy to a namespace with a pod that can be
`exec`'d into, or by any means that does not need a game image. Record the
method and the result. **Both answers are useful and neither is a failure:** if
kindnet enforces, say so and note that a future task could assert a block; if
it does not, that is the fact that makes "assert objects only" correct rather
than merely convenient.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/
git commit
```

Subject: `test(6b): the policy exists, and the operator survives its own`.
The body carries Step 4's finding about kindnet.

---

### Task 8: The documentation

**Files:**
- Modify: `README.md`
- Modify: `docs/known-issues.md`
- Create: `docs/handover-milestone-6b.md` — **or** extend `docs/handover-milestone-6.md`; decide and say why

**Interfaces:**
- Consumes: everything. Runs last and reports what happened, not what was planned.

- [ ] **Step 1: `docs/known-issues.md`**

Close or amend what 6b closes. At minimum, the entry that has been carried
since milestone 3b — the one its own text calls "the entry in this file most
likely to be read as a formality" — which says a backend accepts a connection
attempt from any pod in the cluster. State precisely what is now true and what
is not: the object exists, enforcement is the CNI's, and no run in this
repository has yet observed a connection being refused.

Then add a "From milestone 6b" section carrying at least: what Task 7 Step 4
established about kindnet; that proxy pods are unselected and why; that
unlabelled pods in a game namespace are unrestricted; that proxy egress is
unrestricted because vanilla NetworkPolicy cannot name a destination by DNS;
the Service-ClusterIP/DNAT question from design §6, which the RKE2 rollout has
to settle; and anything the implementation turned up that this plan did not
predict.

- [ ] **Step 2: `README.md`**

A milestone 6b paragraph in the shape the others use — what it does, the one
thing worth naming, what it leaves open. The one thing worth naming is the
asymmetry: the policy selects backends and not proxies, because a backend's
readiness is an `exec` and a proxy's is a kubelet dial, so the fleet's readiness
was never put at the CNI's mercy.

Update the development section if any command changed.

- [ ] **Step 3: The handover**

Written to be picked up cold. It must say where 6b stopped; what 6c
(`LoadBalancer` and `HostPort`) finds in place, checked against the code rather
than against this plan; what the RKE2 rollout now owes, which has grown by
everything 6b could not prove; and the environment.

`docs/handover-milestone-6.md` was written for 6b and its §5 already addresses
6c and 6d. Decide whether to extend it or start a new document, and say which
and why — milestone 5 started fresh from 4's for a stated reason, and 4b got its
own once it stopped being the thing to read 4c against.

- [ ] **Step 4: Run the absolute-word sweep over the whole diff**

```bash
git diff master...HEAD -- '*.md' | grep -n -i -E "\b(never|only|nothing|exactly one|cannot|always|every|no|none|any|all|both)\b"
```

Read every hit against the state of the tree. Milestone 5 recorded nine
instances of a sentence that reads plausibly while describing a mechanism the
code does not have, and 6a found several more — including a true statement
about *removing* an owner reference applied to *adding* one. The instance the
sweep cannot catch is a claim about wiring that does not exist yet: it reads as
ordinary prose and trips none of these words. Check the tense of every claim
about what "now" happens.

Be especially careful with any sentence saying the policy *blocks* anything.
Nothing in this repository has observed that.

- [ ] **Step 5: Full verification**

```bash
nix develop -c make test
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
git diff master...HEAD --name-only
```

The last command is the evidence for any claim about what was and was not
touched. Task 4 changed the gRPC server's connection policy, so if `agent/`
appears here something went wrong; if `make agent-test` was run in Task 4,
quote its result here rather than re-running it.

- [ ] **Step 6: Commit**

```bash
git add README.md docs/
git commit
```

Subject: `docs(6b): what the policy protects, and what nothing has yet proven`.

---

## Self-review notes for the executing session

**Spec coverage.** §2.1–2.2 → Task 2's placement and the RBAC table. §2.3 →
Task 1's owner reference. §2.4 → no task, deliberately: the absence of a
condition is the deliverable, and Task 2's comment records it. §3.1 → Task 3.
§3.2 → Tasks 1 and 2. §3.3 → Task 1's doc comment and its first mutation. §4 →
Task 2 Step 5. §5.1 → Task 4. §5.2 → Task 5. §5.3 → Task 6. §5.4 → Task 6's
commit body. §5.5 → the metrics in Tasks 5 and 6. §6 → Task 2 (`Owns`, the
error path) and Task 8 (the DNAT question). §7 → every task's mutation block.
§8 → Task 7. §9 → distributed, re-verified in Task 8 Step 5. §10 → Task 8.

**What this plan cannot deliver.** Acceptance criterion 8's second half — that
the enforcement limitation is stated in the test's own comment — is written
into Task 7's code. But no task can show a connection being refused, and Task 7
Step 4 exists to establish whether that is because the harness cannot or because
the CNI does not. Whichever it turns out to be, the milestone closes with the
invariant's *enforcement* unproven, and Task 8 must say so in the README rather
than let the object's existence read as protection.
