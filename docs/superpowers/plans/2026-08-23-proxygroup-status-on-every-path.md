# ProxyGroup Status On Every Observed Path Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `ProxyGroup.status.address` names only an address the current expose
strategy has observably realised, and is recomputed on every path of
`Reconcile` that read the pods and the Service.

**Architecture:** Two changes, in this order. First `proxyAddress` stops
mixing spec with observation, so an address can no longer be fabricated from
pods of a previous strategy. Only then does `Reconcile` split into an outer
function that keeps the early return paths and an inner one that returns what
it observed, with the outer finalising the status once, however the inner
returned.

**Tech Stack:** Go, controller-runtime, envtest.

**Spec:** `docs/superpowers/specs/2026-08-23-proxygroup-status-on-every-path-design.md`

## Global Constraints

- Where this plan and the spec disagree, **stop and ask** — do not pick one.
- Every command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`.
- **Do not run `make e2e`.** This machine has 8 GB of RAM.
- **Never run `git config` in any form.** A worktree shares `.git/config` with
  the main repository; a previous agent set an identity there and rewrote the
  author name on real commits.
- **Never push, never merge, and never create a tag.**
- Conventional Commits, English subjects. Every commit ends with exactly:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- Comments explain **why**, not what. `internal/controller/proxygroup_controller.go`
  sets the voice: they explain the failure the code is standing in front of.
- **A test that passes the moment it is written has proven nothing.** Task 1's
  fabrication test and Task 2's scenario test must both be seen to fail first,
  and their verbatim failure output goes in the report.
- **Do not claim a verification that did not run.**
- `internal/testenv` shares **one** envtest control plane per package and
  registers no cleanup, and envtest runs no kube-controller-manager, so a
  namespace deletion collects nothing. Anything a test creates cluster-wide
  outlives it. Use the fixture's namespace and nothing else.

---

## File Structure

| File | Change |
|---|---|
| `internal/controller/proxygroup_controller.go` | `proxyAddress` rewritten; three small helpers beside it; `Reconcile` split; the `IsForbidden` branch loses its own status write |
| `internal/controller/proxyaddress_test.go` | new — table test over the four strategies and the fabrication case |
| `internal/controller/proxygroup_controller_test.go:318` | the NodePort expectation is read from the Service, not the spec |
| `internal/controller/expose_test.go` | new envtest: the scenario from the known-issues entry, plus the early-path guard |
| `docs/known-issues.md` | the 6c entry records what was done |

---

## Task 1: `proxyAddress` is grounded in observation

**Files:**
- Create: `internal/controller/proxyaddress_test.go`
- Modify: `internal/controller/proxygroup_controller.go:1745-1796`
- Modify: `internal/controller/proxygroup_controller_test.go:316-320`

**Interfaces:**
- Produces, for Task 2: `proxyAddress(group *spawneryv1alpha1.ProxyGroup, pods
  []corev1.Pod, svc *corev1.Service) string` — unchanged signature, changed
  rules. Task 2 does not call it directly; `setStatus` does, as today.
- Produces three unexported helpers in the same file:
  `readyHostIP(pods []corev1.Pod) string`,
  `readyHostIPBindingPort(pods []corev1.Pod, hostPort int32) string`,
  `allocatedNodePort(svc *corev1.Service) int32`.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/proxyaddress_test.go`. This is a pure-function
test — no fixture, no envtest, no client.

```go
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// readyProxyPod builds a pod the way the kubelet leaves one: Running, on a
// node, Ready. hostPort is what podspec.BuildProxyPod puts on the container
// under the HostPort strategy and leaves at zero under every other one
// (internal/podspec/proxy.go:227-229), which is the fact the fabrication case
// below turns on.
func readyProxyPod(hostIP string, hostPort int32) corev1.Pod {
	return corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "minecraft",
			Ports: []corev1.ContainerPort{{
				Name:          podspec.MinecraftPortName,
				ContainerPort: podspec.MinecraftPort,
				HostPort:      hostPort,
			}},
		}},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			HostIP: hostIP,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
		},
	}
}

func notReadyProxyPod(hostIP string, hostPort int32) corev1.Pod {
	pod := readyProxyPod(hostIP, hostPort)
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	return pod
}

// nodePortService is what reconcileService leaves behind for a NodePort
// group: one port, named, with the node port the API server allocated.
func nodePortService(nodePort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{
				Name:       podspec.MinecraftPortName,
				Port:       podspec.MinecraftPort,
				TargetPort: intstr.FromString(podspec.MinecraftPortName),
				NodePort:   nodePort,
			}},
		},
	}
}

func clusterIPService() *corev1.Service {
	svc := nodePortService(0)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	return svc
}

func loadBalancerService(ingressIP, ingressHost string) *corev1.Service {
	svc := nodePortService(0)
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{
		IP: ingressIP, Hostname: ingressHost,
	}}
	return svc
}

func groupExposing(spec spawneryv1alpha1.ExposeSpec) *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{Spec: spawneryv1alpha1.ProxyGroupSpec{Expose: spec}}
}

func TestProxyAddressPublishesOnlyWhatIsObservablyRealised(t *testing.T) {
	nodePort := spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30765},
	}
	hostPort := spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
	}
	clusterIP := spawneryv1alpha1.ExposeSpec{
		Type:      spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
	}
	loadBalancer := spawneryv1alpha1.ExposeSpec{
		Type: spawneryv1alpha1.ExposeLoadBalancer,
	}

	cases := []struct {
		name string
		spec spawneryv1alpha1.ExposeSpec
		pods []corev1.Pod
		svc  *corev1.Service
		want string
		why  string
	}{
		{
			name: "NodePort publishes the port the API server allocated",
			spec: nodePort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  nodePortService(30765),
			want: "192.168.1.10:30765",
			why:  "a ready pod on a node, and a Service carrying the allocation",
		},
		{
			name: "NodePort reads the Service and not the spec",
			spec: nodePort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			// The spec asks for 30765; the API server allocated 31000. The
			// allocation is what a client can dial.
			svc:  nodePortService(31000),
			want: "192.168.1.10:31000",
			why:  "the allocation wins over the request",
		},
		{
			name: "NodePort publishes nothing once the Service is gone",
			spec: nodePort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  nil,
			want: "",
			why:  "no Service means no node port, whatever the pods say",
		},
		{
			name: "HostPort publishes a port a ready pod actually binds",
			spec: hostPort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 25565)},
			svc:  nil,
			want: "192.168.1.10:25565",
			why:  "HostPort creates no Service, so the pod is the whole evidence",
		},
		{
			// THE FABRICATION CASE. This is the one that fails before the
			// change. The spec has been switched to HostPort; the pods still
			// running are the NodePort generation, whose containers carry
			// HostPort == 0. Before this change proxyAddress took their HostIP
			// and appended the spec's 25565, publishing an address whose host
			// is real, whose port is real, and which no process on that node
			// is listening on.
			name: "HostPort publishes nothing while only the old strategy's pods are ready",
			spec: hostPort,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  nil,
			want: "",
			why:  "no pod in existence binds 25565 on that node",
		},
		{
			name: "HostPort ignores a pod that binds the port but is not ready",
			spec: hostPort,
			pods: []corev1.Pod{notReadyProxyPod("192.168.1.10", 25565)},
			svc:  nil,
			want: "",
			why:  "the readiness gate is unchanged by this work",
		},
		{
			name: "ClusterIP echoes the address once the Service exists",
			spec: clusterIP,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  clusterIPService(),
			want: "mc.example.test",
			why:  "no port is appended; a client types the name and nothing else",
		},
		{
			name: "ClusterIP publishes nothing without the Service to route to",
			spec: clusterIP,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  nil,
			want: "",
			why:  "the fronting proxy routes to the Service; without it the name goes nowhere",
		},
		{
			name: "LoadBalancer publishes the ingress IP",
			spec: loadBalancer,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  loadBalancerService("203.0.113.7", ""),
			want: "203.0.113.7:25565",
			why:  "unchanged behaviour, pinned so this work cannot move it",
		},
		{
			name: "LoadBalancer falls back to the ingress hostname",
			spec: loadBalancer,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  loadBalancerService("", "lb.example.test"),
			want: "lb.example.test:25565",
			why:  "unchanged behaviour, pinned so this work cannot move it",
		},
		{
			name: "LoadBalancer publishes nothing before the address is assigned",
			spec: loadBalancer,
			pods: []corev1.Pod{readyProxyPod("192.168.1.10", 0)},
			svc:  loadBalancerService("", ""),
			want: "",
			why:  "unchanged behaviour, pinned so this work cannot move it",
		},
		{
			name: "no ready pod publishes nothing, whatever the strategy",
			spec: nodePort,
			pods: []corev1.Pod{notReadyProxyPod("192.168.1.10", 0)},
			svc:  nodePortService(30765),
			want: "",
			why:  "test/e2e/expose_test.go rests on exactly this",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := proxyAddress(groupExposing(tc.spec), tc.pods, tc.svc)
			if got != tc.want {
				t.Errorf("proxyAddress = %q, want %q — %s", got, tc.want, tc.why)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test and record which cases fail**

```
nix --extra-experimental-features 'nix-command flakes' develop -c \
  go test ./internal/controller/ -run TestProxyAddressPublishesOnlyWhatIsObservablyRealised -v
```

Expected: at least the fabrication case and the two "Service is gone /
without the Service" cases FAIL, because today's `proxyAddress` never looks at
`svc` for `HostPort` or `ClusterIP` and never looks at the container's ports
at all. **Paste the verbatim failure lines into the report.** If the
fabrication case passes here, stop — the premise of this task is wrong and
that is worth knowing before anything is changed.

- [ ] **Step 3: Rewrite `proxyAddress` and add its helpers**

Replace the body of `proxyAddress` at
`internal/controller/proxygroup_controller.go:1745`. Keep the existing doc
comment above it and extend it; do not delete the paragraph explaining that
for NodePort and HostPort the address is a node's.

```go
// readyHostIP is the node address of the first ready pod that has one. It is
// the readiness gate every strategy shares: nothing is published for a group
// whose pods cannot answer, which is what test/e2e/expose_test.go rests on --
// no image resolves there, so no pod is ready, so no address appears.
func readyHostIP(pods []corev1.Pod) string {
	for i := range pods {
		if isPodReady(&pods[i]) && pods[i].Status.HostIP != "" {
			return pods[i].Status.HostIP
		}
	}
	return ""
}

// readyHostIPBindingPort is the node address of the first ready pod whose
// container actually declares hostPort.
//
// This exists because a group's pods outlive the spec that created them. A
// group switched from NodePort to HostPort keeps its NodePort pods running and
// Ready until they are replaced -- and if the replacement is refused, forever.
// Asking only "is some pod ready" and then appending the spec's port published
// an address whose host was real, whose port was real, and which no process on
// that node was listening on. podspec.BuildProxyPod sets the container's
// HostPort only under the HostPort strategy (internal/podspec/proxy.go), so a
// pod from any other generation carries zero here and is skipped, which makes
// the distinction a fact about the pod rather than a rule to remember.
func readyHostIPBindingPort(pods []corev1.Pod, hostPort int32) string {
	for i := range pods {
		if !isPodReady(&pods[i]) || pods[i].Status.HostIP == "" {
			continue
		}
		for _, c := range pods[i].Spec.Containers {
			for _, p := range c.Ports {
				if p.HostPort == hostPort {
					return pods[i].Status.HostIP
				}
			}
		}
	}
	return ""
}

// allocatedNodePort is the node port the API server assigned, read back off
// the Service rather than taken from the spec.
//
// reconcileService writes spec.expose.nodePort.port into the Service and the
// API server allocates one when the spec names none, so the Service is both
// the honest value and the only one that exists in the second case. Matched by
// name, because that is what reconcileService sets and a group's Service
// carries exactly one port.
func allocatedNodePort(svc *corev1.Service) int32 {
	if svc == nil {
		return 0
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == podspec.MinecraftPortName {
			return p.NodePort
		}
	}
	return 0
}
```

And the function itself:

```go
func proxyAddress(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, svc *corev1.Service) string {
	port := func(p int32) string { return strconv.Itoa(int(p)) }

	switch group.Spec.Expose.Type {
	case spawneryv1alpha1.ExposeLoadBalancer:
		if svc == nil || readyHostIP(pods) == "" {
			return ""
		}
		for _, ing := range svc.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				return net.JoinHostPort(ing.IP, port(podspec.MinecraftPort))
			}
			if ing.Hostname != "" {
				return net.JoinHostPort(ing.Hostname, port(podspec.MinecraftPort))
			}
		}
		return ""
	case spawneryv1alpha1.ExposeHostPort:
		if group.Spec.Expose.HostPort == nil {
			return ""
		}
		hostIP := readyHostIPBindingPort(pods, group.Spec.Expose.HostPort.Port)
		if hostIP == "" {
			return ""
		}
		return net.JoinHostPort(hostIP, port(group.Spec.Expose.HostPort.Port))
	case spawneryv1alpha1.ExposeClusterIP:
		// Echoed, not composed: no port is appended, because a Minecraft
		// client defaults to 25565 and "mc.example.test" is the whole of what
		// a player types. An operator who needs another port writes it in.
		//
		// The Service is required even though the address does not come from
		// it: this strategy publishes a name that something outside the
		// cluster routes to the Service, so without the Service the name goes
		// nowhere.
		if svc == nil || group.Spec.Expose.ClusterIP == nil || readyHostIP(pods) == "" {
			return ""
		}
		return group.Spec.Expose.ClusterIP.Address
	case spawneryv1alpha1.ExposeNodePort:
		nodePort := allocatedNodePort(svc)
		if nodePort == 0 {
			return ""
		}
		hostIP := readyHostIP(pods)
		if hostIP == "" {
			return ""
		}
		return net.JoinHostPort(hostIP, port(nodePort))
	default:
		// See reconcileService's default: for why this is written out rather
		// than folded into the NodePort arm.
		return ""
	}
}
```

Note that `group.Spec.Expose.NodePort` is no longer read. That is the point:
the request is not evidence, the allocation is.

- [ ] **Step 4: Run the test to verify it passes**

```
nix --extra-experimental-features 'nix-command flakes' develop -c \
  go test ./internal/controller/ -run TestProxyAddressPublishesOnlyWhatIsObservablyRealised -v
```

Expected: PASS, all twelve subtests.

- [ ] **Step 5: Move the existing address expectation onto the Service**

`internal/controller/proxygroup_controller_test.go:316-320` currently reads:

```go
	want := fmt.Sprintf("%s:%d", proxyPodHostIP, f.proxyGroup("gateway").Spec.Expose.NodePort.Port)
```

The comment above it says both sides are "read back from where they were
written". After Step 3 the port is written to, and read from, the Service — so
replace it, keeping the comment true:

```go
	// The host half is the hostIP markProxyPodReady puts on the pod; the port
	// half is the node port the API server allocated, read off the Service
	// because that is now where proxyAddress reads it. Both sides are read
	// back from where they were written, not hardcoded twice.
	svc := &corev1.Service{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway", Namespace: f.ns}, svc); err != nil {
		t.Fatalf("get the group's Service: %v", err)
	}
	want := fmt.Sprintf("%s:%d", proxyPodHostIP, allocatedNodePort(svc))
```

- [ ] **Step 6: Run the whole package**

```
nix --extra-experimental-features 'nix-command flakes' develop -c \
  go test -race ./internal/controller/ -count=1
```

Expected: PASS. If anything else in the package fails, **read it before
fixing it** — a test that was asserting the fabricated address is evidence,
not an obstacle, and the report must say which test it was and what it
asserted.

- [ ] **Step 7: Prove the fabrication test can fail**

Mutate `readyHostIPBindingPort` to ignore the port — replace its inner loops
with `return pods[i].Status.HostIP` after the readiness check. Run only the
fabrication subtest:

```
nix --extra-experimental-features 'nix-command flakes' develop -c \
  go test ./internal/controller/ -run 'TestProxyAddressPublishesOnlyWhatIsObservablyRealised/HostPort_publishes_nothing_while' -v
```

Expected: FAIL, naming `192.168.1.10:25565` as what it got. Record the
verbatim output, revert, and confirm with `git diff --stat internal/`.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/proxyaddress_test.go internal/controller/proxygroup_controller.go internal/controller/proxygroup_controller_test.go
git commit
```

Subject: `fix(proxygroup): status.address named a port no pod was binding`.
The body must carry the verbatim before-failure from Step 2 and the mutation
output from Step 7.

---

## Task 2: `Reconcile` finalises the status on every path that observed

**Files:**
- Modify: `internal/controller/proxygroup_controller.go:178-327`
- Modify: `internal/controller/expose_test.go` (append two tests)
- Modify: `docs/known-issues.md` (the 6c entry)

**Interfaces:**
- Consumes: `proxyAddress` from Task 1, with its new rules. Task 2 does not
  change it.
- Produces: `type proxyObservation struct { observed bool; pods []corev1.Pod;
  svc *corev1.Service }` and
  `func (r *ProxyGroupReconciler) reconcileObserved(ctx context.Context,
  network *spawneryv1alpha1.Network, group *spawneryv1alpha1.ProxyGroup)
  (proxyObservation, ctrl.Result, error)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/expose_test.go`, beside
`TestARejectedProxyPodIsReportedOnTheGroup` which it is the sequel to.

```go
// TestAGroupSwitchedIntoARefusedStrategyStopsAdvertisingTheOldAddress is the
// scenario docs/known-issues.md's milestone 6c entry describes, driven rather
// than reasoned: a NodePort group publishing an address is switched to
// HostPort in a namespace that forbids host ports, reconcileService deletes
// the Service, the replacement pods are refused, and before this test existed
// Reconcile returned before setStatus and left status.address naming the node
// port of a Service that no longer existed -- for as long as the namespace
// label stood, which is forever.
func TestAGroupSwitchedIntoARefusedStrategyStopsAdvertisingTheOldAddress(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 1
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeNodePort,
			NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30765},
		}
	})

	// Bring the group up and get an address on it.
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	if len(pods) != 1 {
		t.Fatalf("proxy pods = %d, want 1", len(pods))
	}
	f.markProxyPodReady(t, &pods[0])
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyGroup("gateway").Status.Address
	if before == "" {
		t.Fatal("the group published no address before the switch, so this test " +
			"cannot show one being withdrawn")
	}

	// Now forbid host ports and ask for them.
	f.enforcePodSecurity(t, "baseline")
	group := f.proxyGroup("gateway")
	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
	}
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("switch the group to HostPort: %v", err)
	}

	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gateway", Namespace: f.ns},
	}); err == nil {
		t.Fatal("the reconcile succeeded in a namespace that forbids host ports")
	}

	after := f.proxyGroup("gateway")
	if after.Status.Address != "" {
		t.Errorf("status.address = %q, want it empty. It was %q before the switch, "+
			"and the Service that node port belonged to has been deleted -- a player "+
			"dialing it reaches nothing", after.Status.Address, before)
	}
	// The empty address on its own would be its own defect: a group with no
	// address and no reason is indistinguishable from one that has not come up
	// yet.
	cond := meta.FindStatusCondition(after.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True beside the empty address", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonProxyPodRejected {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonProxyPodRejected)
	}
	if after.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", after.Status.Phase)
	}
}
```

**One thing this test must not do vacuously.** If `reconcileReplicas` happens
to delete the old NodePort pod before attempting the refused create, the
address would go empty because there is no ready pod at all, and the test
would pass without exercising Task 1's guard. So assert the premise: after
the failing reconcile, before checking the address, add

```go
	// The premise: an old ready pod is still there. Without this, an empty
	// address might only mean "no pod is ready", which is a different and much
	// weaker statement than the one this test is making.
	stillReady := 0
	for _, p := range f.proxyPods("gateway") {
		if isPodReady(&p) {
			stillReady++
		}
	}
	if stillReady == 0 {
		t.Skip("no ready pod survived the switch, so this run cannot distinguish " +
			"the address guard from the readiness gate; see the plan's note")
	}
```

Place it immediately after the failing `Reconcile` call and before `after :=`.
A `t.Skip` rather than a `t.Fatal` because the ordering inside
`reconcileReplicas` is not this test's subject — but a silent pass would be.
**If the skip fires, say so in the report**; it means this scenario needs a
different construction and the plan was wrong about the ordering.

- [ ] **Step 2: Run it and watch it fail**

```
nix --extra-experimental-features 'nix-command flakes' develop -c \
  go test ./internal/controller/ -run TestAGroupSwitchedIntoARefusedStrategyStopsAdvertisingTheOldAddress -v
```

Expected: FAIL on `status.address = "192.168.1.10:30765", want it empty`.
Record the verbatim line. If instead it reports the skip, stop and report.

- [ ] **Step 3: Split `Reconcile`**

In `internal/controller/proxygroup_controller.go`, keep everything from the
top of `Reconcile` through the `writeStatus` that persists `Accepted=True`
(currently `:178-256`) in `Reconcile`. Then:

```go
	obs, res, err := r.reconcileObserved(ctx, network, group)
	// The status is written wherever the pods and the Service were actually
	// read, and nowhere else. Both halves of that are deliberate.
	//
	// Wherever: before this, setStatus was the last call before the successful
	// return and eleven error returns stood in front of it, so a pass that
	// failed partway through kept whatever the last successful pass had
	// published. A group switched into a strategy the API server refuses went
	// on advertising the node port of a Service that reconcileService had
	// already deleted, and no later pass corrected it because no later pass
	// could get any further.
	//
	// And nowhere else: Reconcile also returns before any of this, on a
	// missing or unaccepted Network and on an unimplemented expose type. The
	// address is untouched there on purpose. Nothing about the serving world
	// has changed on those paths -- the pods are running, the Service is
	// there, people are connected through it -- and blanking a working address
	// because a different object went missing would be a regression caused by
	// this fix rather than a part of it.
	if obs.observed {
		r.setStatus(group, obs.pods, obs.svc)
		if werr := r.writeStatus(ctx, group); werr != nil {
			if err == nil {
				return res, werr
			}
			// The reconcile's own error is the cause and the one worth backing
			// off on; the failed write is reported rather than substituted.
			log.FromContext(ctx).Error(werr, "recording the group's status")
		}
	}
	return res, err
}

// proxyObservation is what one pass of reconcileObserved saw. observed is a
// flag and not a nil check on svc: HostPort creates no Service at all, so a
// nil svc is that strategy's normal state and cannot stand in for "this pass
// never looked".
type proxyObservation struct {
	observed bool
	pods     []corev1.Pod
	svc      *corev1.Service
}

// reconcileObserved does everything from the namespace bootstrap onward and
// returns what it saw, so that its caller can record the status however this
// returns. Its result carries the RequeueAfter the successful path has always
// returned; the caller adds nothing to it.
func (r *ProxyGroupReconciler) reconcileObserved(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
) (proxyObservation, ctrl.Result, error) {
	var obs proxyObservation

	if err := r.Bootstrap.Ensure(ctx, group.Namespace); err != nil {
		return obs, ctrl.Result{}, err
	}
	if err := r.reconcileConfigMap(ctx, group); err != nil {
		return obs, ctrl.Result{}, err
	}
	svc, err := r.reconcileService(ctx, group)
	if err != nil {
		return obs, ctrl.Result{}, err
	}
	pods, err := r.pods(ctx, group)
	if err != nil {
		return obs, ctrl.Result{}, err
	}
	// From here the pass has seen both, so every return below carries an
	// observation the caller will record.
	obs = proxyObservation{observed: true, pods: pods, svc: svc}

	if err := r.reconcileReplicas(ctx, network, group, pods); err != nil {
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			if setProxyPodsBlocked(group, spawneryv1alpha1.ReasonProxyPodRejected, err.Error()) {
				r.Recorder.Eventf(group, nil, corev1.EventTypeWarning, "ProxyPodBlocked",
					actionCreateProxyPod, "%s",
					eventNote("the API server refused a proxy pod: %s", err.Error()))
			}
			// The phase is not set here and the status is not written here.
			// setStatus derives Degraded from the condition this branch just
			// set, and the caller writes on every path that got this far.
		}
		return obs, ctrl.Result{}, err
	}

	// Re-read after the changes, so the status describes what is there rather
	// than what was there when the reconcile started. A failure here leaves
	// the earlier snapshot in place: a status describing the last observation
	// that succeeded is a weaker statement than this pass meant to make and a
	// far stronger one than none.
	if pods, err = r.pods(ctx, group); err != nil {
		return obs, ctrl.Result{}, err
	}
	obs.pods = pods

	r.reportBlockedProxies(group, pods)
	if err := r.protectOccupiedProxies(ctx, group, pods); err != nil {
		return obs, ctrl.Result{}, err
	}
	return obs, ctrl.Result{RequeueAfter: resyncInterval}, nil
}
```

Keep every comment that currently sits on the moved lines — the snapshot
comment on the first `pods` read, the ordering comments before
`reportBlockedProxies` and `protectOccupiedProxies`, and the
`reconcileConfigMap` ordering comment. They explain constraints that did not
change.

- [ ] **Step 4: Run the test to verify it passes**

```
nix --extra-experimental-features 'nix-command flakes' develop -c \
  go test ./internal/controller/ -run TestAGroupSwitchedIntoARefusedStrategyStopsAdvertisingTheOldAddress -v
```

Expected: PASS.

- [ ] **Step 5: Write the guard test for the other half of the rule**

It exists so that the fix does not become the regression the known-issues
entry warned about. Append to `internal/controller/expose_test.go`.

```go
// TestABrokenNetworkLeavesAWorkingAddressAlone pins the other half of the
// rule. Reconcile returns on a missing Network before it reads a single pod or
// Service, and the address must survive that: the proxies are still running,
// the Service is still there, and people are still connected through it. A
// deleted Network does not make an address wrong, and clearing it here would
// be a regression caused by the fix rather than a part of it.
func TestABrokenNetworkLeavesAWorkingAddressAlone(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 1
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeNodePort,
			NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30766},
		}
	})
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	if len(pods) != 1 {
		t.Fatalf("proxy pods = %d, want 1", len(pods))
	}
	f.markProxyPodReady(t, &pods[0])
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyGroup("gateway").Status.Address
	if before == "" {
		t.Fatal("no address to preserve, so this test cannot show it being preserved")
	}

	f.deleteNetwork(t)

	// This path returns cleanly with a requeue, so reconcileProxyGroup is the
	// right helper.
	f.reconcileProxyGroup(r, "gateway")

	after := f.proxyGroup("gateway")
	if after.Status.Address != before {
		t.Errorf("status.address = %q, want it left at %q — the pods and the Service "+
			"are untouched by a missing Network, so the address still works",
			after.Status.Address, before)
	}
	cond := meta.FindStatusCondition(after.Status.Conditions, spawneryv1alpha1.ConditionAccepted)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Accepted = %+v, want False — the refusal still has to be legible", cond)
	}
}
```

`f.deleteNetwork` may not exist. If it does not, delete the `Network` inline
with `f.c.Delete`, using whatever name `f.createProxyGroup`'s default
`NetworkRef` points at — read the fixture rather than guessing, and if adding
a helper is cleaner, add it beside the other `f.*` helpers in
`proxygroup_controller_test.go` with a comment saying which test needs it.

**The counter-property needs no second envtest, and this is why.** The
known-issues entry's worst-case worry was a fix that blanks the address of a
group whose Service is live and whose pods are ready, because the pass hit an
unrelated error. After Step 3 that cannot happen, and not as a matter of care:
`setStatus` and `proxyAddress` are never handed the error. They take the group,
the pods and the Service, and nothing else. An address computed from a live
Service and a ready pod is the same address whether the pass that computed it
went on to fail or not, so "an unrelated failure clears the address" has no
path through the code to travel.

What that leaves to test is the two halves that *are* contingent, and both are
covered: an address whose strategy is no longer realisable goes (the scenario
test above), and a path that never looked writes nothing
(`TestABrokenNetworkLeavesAWorkingAddressAlone`). Task 1's table covers the
third: with a live Service and a ready pod, `proxyAddress` returns the address.

Do not add an envtest that injects a failure after the observation point. There
is no injection point in the fixture today, and adding one to production code
so that a test can reach it would be a change to the operator made for the
test's benefit — which is what the counter-test exists to guard against in the
first place.

- [ ] **Step 6: Run the package**

```
nix --extra-experimental-features 'nix-command flakes' develop -c \
  go test -race ./internal/controller/ -count=1
```

Expected: PASS.

- [ ] **Step 7: Prove the scenario test can fail**

Mutate the finaliser: change `if obs.observed {` to `if obs.observed && err == nil {`,
which is exactly the defect being fixed — status written only when the pass
succeeded. Run the scenario test alone.

Expected: FAIL, with the stale address named. Record the verbatim output,
revert, confirm with `git diff --stat internal/`.

- [ ] **Step 8: Record it in `docs/known-issues.md`**

The 6c entry `**`status.address` can go on advertising a Service that has been
deleted...**` describes an open defect and defers the remedy to "6d's or
later". Rewrite it to record what happened, keeping: the scenario (it is the
test's own subject now), the two candidate fixes it rejected and why — the
reasoning is what stopped a worse fix being made — and add what was actually
done, including the second defect Task 1 found, which the entry had described
as a hypothetical cost of a fix rather than as current behaviour.

State the limit plainly: `HostPort` is exercised in envtest and unit tests
only. `paulwtf` runs one `ProxyGroup` on `ClusterIP`, so no cluster has driven
the rows this work changed most.

- [ ] **Step 9: Run the whole suite and commit**

```
nix --extra-experimental-features 'nix-command flakes' develop -c make test
```

Expected: PASS. Then:

```bash
git add internal/controller/ docs/known-issues.md
git commit
```

Subject: `fix(proxygroup): the status is recorded on every path that observed one`.
The body must carry the Step 2 failure, the Step 7 mutation output, and
whether the Step 1 skip fired.

---

## What this plan does not cover

- **No cluster is driven.** `paulwtf` runs one `ProxyGroup`,
  `expose.type: ClusterIP`, whose row gains only a `svc != nil` guard. The
  `HostPort` and `NodePort` rows are envtest and unit tests only. Do not write
  a report implying otherwise.
- **`ServerGroupReconciler` is untouched**, though it has the same
  success-path-only status shape. The spec's §3 says why, and that reasoning
  is not to be relitigated inside this plan's tasks.
- **`writeStatus` keeps its lack of conflict retry.** If the envtest suite
  starts flaking on conflicts after this change, that is a finding to report,
  not a thing to fix here.
