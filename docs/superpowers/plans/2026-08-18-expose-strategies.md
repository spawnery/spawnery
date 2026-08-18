# Milestone 6c — Expose Strategies Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the `LoadBalancer` and `HostPort` expose strategies that
`ProxyGroupReconciler` has refused since milestone 1, and make a proxy pod
that cannot come into existence say so on its group.

**Architecture:** No API change. The three strategies differ in exactly two
places — `reconcileService` and `proxyAddress` — plus one container field in
`internal/podspec`. The blanket refusal in `Reconcile` becomes a guard for an
enum value the controller does not know. A new `Degraded` writer on
`ProxyGroup` reports the two ways a proxy pod fails to exist: an admission
refusal of the create, and a scheduler that cannot place it.

**Tech Stack:** Go 1.24, controller-runtime, envtest, kind under rootless
podman, Nix flakes.

**Spec:** `docs/superpowers/specs/2026-08-18-expose-strategies-design.md`

## Global Constraints

- **Every command runs inside the dev shell.** This machine needs the
  experimental flags: prefix with
  `nix --extra-experimental-features 'nix-command flakes' develop -c`. The
  short form used throughout this plan is `nix develop -c <cmd>`; expand it.
  See `docs/known-issues.md`.
- **`make test` runs with `-race`.** A test that is green without it and red
  with it is red.
- **Commit messages use Conventional Commits** — `feat(6c):`, `fix(6c):`,
  `test(6c):`, `docs(6c):` — deliberately overriding the repository's older
  sentence-style history. Every commit ends with the two trailers used
  throughout this repository:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **No claim of reachability.** No test name, doc comment, commit message or
  document produced by this milestone may say that a client reached a proxy,
  that a `LoadBalancer` address works, or that a `hostPort` accepted a
  connection. Nothing here can observe any of that. The honest verbs are that
  the operator *publishes* an address and that an object *exists*.
- **A test is not done until a mutation proves it can fail.** Every task
  below names the specific mutation to apply to production code, the command
  to run, and the failure to expect. Report the mutation's actual output.
  This project's record is that findings come from mutation and not from
  reading: milestone 6b produced seven, every one in test code the plan
  specified verbatim.
- **`internal/rbacaudit` fails when a `+kubebuilder:rbac` marker and
  `internal/rbacaudit/required.go` disagree in either direction.** Any marker
  change is a table change in the same commit.
- **Ports used by the E2E manifest, all distinct on purpose:** `gateway`
  keeps nodePort `30765`; `gateway-switch` uses nodePort `30766` and, after
  its switch, hostPort `25566`; `gateway-host` uses hostPort `25565`;
  `gateway-lb` names no nodePort at all.

---

## File Structure

**Modified:**

- `internal/podspec/proxy.go` — the Minecraft container port gains a
  `HostPort` for one strategy. Because `DesiredProxyHash` renders through the
  same `renderProxyPod`, this is also what makes a strategy switch roll the
  pods.
- `internal/controller/proxygroup_controller.go` — `reconcileService` becomes
  strategy-aware and returns the Service; `proxyAddress` and `setStatus` take
  the strategy and that Service; the refusal becomes an unknown-type guard; a
  new `Degraded` reporter.
- `internal/controller/readinessdivergence.go` — one doc comment counts three
  early returns and must count two.
- `internal/rbacaudit/required.go` — one row for `services: delete`.
- `api/v1alpha1/common_types.go` — three condition reasons.
- `internal/podspec/labels.go` — one annotation-key constant.
- `config/rbac/role.yaml` — regenerated, never hand-edited.
- `test/e2e/e2e_test.go` — four lines in the ordered scenario list, and one
  narrowed substring in `theOperatorWasNeverDenied`.
- `test/e2e/manifests/e2e.yaml` — four `ProxyGroup`s and a second namespace.
- `hack/e2e.sh` — the second namespace and its grant.
- `config/samples/network.yaml`, `README.md`, `docs/known-issues.md`.

**Created:**

- `internal/controller/expose_test.go` — every controller test this milestone
  adds. The existing `proxygroup_controller_test.go` is 1400+ lines; the new
  material is one coherent subject and belongs in its own file, alongside the
  fixture helpers it shares.
- `test/e2e/expose_test.go` — the four E2E scenarios.
- `docs/handover-milestone-6c.md` — the cold-start entry point for 6d.

---

### Task 1: The container's host port

**Files:**
- Modify: `internal/podspec/proxy.go:212-226` (the container's `Ports` block)
- Test: `internal/podspec/proxy_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: nothing callable. The observable effect later tasks rely on is
  that `podspec.BuildProxyPod` sets
  `pod.Spec.Containers[0].Ports[0].HostPort` when
  `group.Spec.Expose.Type == spawneryv1alpha1.ExposeHostPort`, and that
  `podspec.DesiredProxyHash` therefore differs between the `HostPort`
  strategy and the other two while being *equal* between `NodePort` and
  `LoadBalancer`.

- [ ] **Step 1: Read the surrounding code**

Read `internal/podspec/proxy.go` from the `renderProxyPod` doc comment down
past the `container := corev1.Container{...}` literal. Note two things you
must not break: `renderProxyPod` is called by both `BuildProxyPod` and
`DesiredProxyHash` (the latter with the pod name held empty), and the ports
block currently has two entries, `MinecraftPortName` and `ProxyReadyPortName`.

- [ ] **Step 2: Write the failing tests**

Append to `internal/podspec/proxy_test.go`. That file is **in** package
`podspec`, not `podspec_test`, so everything is referenced unqualified —
`BuildProxyPod`, `MinecraftPortName`, `MinecraftPort`. It already has the
helpers used below: `testNetwork()`, `testProxyGroup()` (a `NodePort` group on
port 30001), `testEndpoint`, and `buildProxy(t)`.

`TestProxyPodExposesBothPorts` (`:187`) already asserts the two container
ports' names and numbers. Do not restate that; the tests below are about the
host port only.

```go
// A hostPort is the whole of what makes the HostPort strategy work without a
// Service, and it is also what makes the kube-scheduler refuse a second pod
// of the same group on one node. Setting it under any other strategy would
// impose that cap on groups that never asked for it.
func TestBuildProxyPodBindsAHostPortOnlyForThatStrategy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expose spawneryv1alpha1.ExposeSpec
		want   int32
	}{
		{
			name: "NodePort",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			want: 0,
		},
		{
			name: "LoadBalancer",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			want: 0,
		},
		{
			name: "HostPort",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			want: 25565,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			net := testNetwork()
			group := testProxyGroup()
			group.Spec.Expose = tc.expose

			pod, err := BuildProxyPod(net, group, "gateway-abcd", testEndpoint, nil)
			if err != nil {
				t.Fatalf("BuildProxyPod: %v", err)
			}

			var minecraft *corev1.ContainerPort
			for i := range pod.Spec.Containers[0].Ports {
				if pod.Spec.Containers[0].Ports[i].Name == MinecraftPortName {
					minecraft = &pod.Spec.Containers[0].Ports[i]
				}
			}
			if minecraft == nil {
				t.Fatalf("no port named %q on the container: %+v",
					MinecraftPortName, pod.Spec.Containers[0].Ports)
			}
			if minecraft.HostPort != tc.want {
				t.Errorf("hostPort = %d, want %d", minecraft.HostPort, tc.want)
			}
			// The ready port is the kubelet's probe target and is never
			// published on a node: a second host port would cap the group by
			// node count for a port no player dials.
			for _, p := range pod.Spec.Containers[0].Ports {
				if p.Name == ProxyReadyPortName && p.HostPort != 0 {
					t.Errorf("the ready port carries hostPort %d; it must never be published",
						p.HostPort)
				}
			}
		})
	}
}

// The hash is what makes a strategy switch roll the pods, and what makes a
// switch that changes nothing about the pods roll nothing. Both halves are
// asserted here because only the pair says what the field is for: without
// the equality, adding the strategy to the hash by any other means would
// pass while replacing every pod of a group that switched NodePort to
// LoadBalancer for no reason at all.
func TestDesiredProxyHashSeparatesHostPortFromTheServiceStrategies(t *testing.T) {
	hashFor := func(t *testing.T, expose spawneryv1alpha1.ExposeSpec) string {
		t.Helper()
		group := testProxyGroup()
		group.Spec.Expose = expose
		h, err := DesiredProxyHash(testNetwork(), group, testEndpoint, nil)
		if err != nil {
			t.Fatalf("DesiredProxyHash: %v", err)
		}
		return h
	}

	nodePort := hashFor(t, spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
	})
	loadBalancer := hashFor(t, spawneryv1alpha1.ExposeSpec{
		Type:         spawneryv1alpha1.ExposeLoadBalancer,
		LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
	})
	hostPort := hashFor(t, spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
	})

	if nodePort != loadBalancer {
		t.Errorf("NodePort and LoadBalancer hash differently (%s vs %s). Those two "+
			"differ only in the Service; rolling every pod for that switch would "+
			"disconnect players for nothing", nodePort, loadBalancer)
	}
	if hostPort == nodePort {
		t.Errorf("HostPort hashes the same as NodePort (%s). A group switched into "+
			"or out of HostPort would keep pods whose container ports no longer "+
			"match the strategy", hostPort)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```
nix develop -c go test ./internal/podspec/ -run 'HostPort|DesiredProxyHashSeparates' -v
```

Expected: `TestBuildProxyPodBindsAHostPortOnlyForThatStrategy/HostPort` fails
with `hostPort = 0, want 25565`, and
`TestDesiredProxyHashSeparatesHostPortFromTheServiceStrategies` fails with
`HostPort hashes the same as NodePort`. The `NodePort` and `LoadBalancer`
subtests pass already — that is expected and is the point of including them.

- [ ] **Step 4: Implement**

In `internal/podspec/proxy.go`, replace the inline `Ports:` slice of the
`container := corev1.Container{...}` literal. Build the Minecraft port
separately just above the literal:

```go
	minecraft := corev1.ContainerPort{
		Name:          MinecraftPortName,
		ContainerPort: MinecraftPort,
		Protocol:      corev1.ProtocolTCP,
	}
	// The HostPort strategy publishes this port on whatever node the pod
	// lands on, which is what lets it work with no Service at all -- and what
	// makes the kube-scheduler decline to place a second pod of this group on
	// the same node, capping replicas at the node count.
	//
	// Set here, inside renderProxyPod, rather than by any caller:
	// DesiredProxyHash renders through this same function, so a hostPort
	// applied anywhere else would not reach the hash, and a group switched
	// into or out of HostPort would keep pods the rollout still called
	// current. The nil check is not defensive noise -- the CRD's CEL rules
	// guarantee the sub-block only for objects that went through the API
	// server, and a ProxyGroup built in a unit test never does. This is the
	// same hazard DefaultDrainTimeoutSeconds exists for.
	if group.Spec.Expose.Type == spawneryv1alpha1.ExposeHostPort &&
		group.Spec.Expose.HostPort != nil {
		minecraft.HostPort = group.Spec.Expose.HostPort.Port
	}
```

and in the literal use:

```go
		Ports: []corev1.ContainerPort{
			minecraft,
			{
				Name:          ProxyReadyPortName,
				ContainerPort: ProxyReadyPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
```

- [ ] **Step 5: Run the tests to verify they pass**

```
nix develop -c go test ./internal/podspec/ -run 'HostPort|DesiredProxyHashSeparates' -v
```

Expected: PASS, all four subtests plus the hash test.

- [ ] **Step 6: Mutate, and report what happened**

Apply each mutation to `internal/podspec/proxy.go`, run
`nix develop -c go test ./internal/podspec/`, record the failure, revert.

1. Drop the `Type == ExposeHostPort` half of the condition, so every strategy
   gets a host port when `HostPort != nil`. Expected: the hash test fails on
   `NodePort and LoadBalancer hash differently`? No — it must fail on nothing
   there, because those groups have `HostPort == nil`. **If neither test
   fails, that is a real gap**: add a subtest where a `NodePort` group also
   carries a stray `HostPort` sub-block (an object the CEL rules forbid but a
   unit test can build) and assert `hostPort == 0`.
2. Set `minecraft.HostPort = MinecraftPort` unconditionally. Expected: the
   `NodePort` and `LoadBalancer` subtests fail with `hostPort = 25565, want 0`.
3. Also set `HostPort` on the ready port. Expected: the ready-port assertion
   fails in the `HostPort` subtest.

- [ ] **Step 7: Full suite and commit**

```
nix develop -c make test
git add internal/podspec/proxy.go internal/podspec/proxy_test.go
git commit
```

Message subject: `feat(6c): bind the node port on the pod, for one strategy`.
The body should say that `DesiredProxyHash` renders through the same
function, so the strategy switch rolls the pods with no further code, and
that `NodePort` to `LoadBalancer` deliberately rolls nothing.

---

### Task 2: The Service, and the address it implies

**Files:**
- Modify: `internal/controller/proxygroup_controller.go` — `reconcileService`
  (`:1200`), its call site in `Reconcile` (`:254`), `setStatus` (`:1320`),
  `proxyAddress` (`:1376`), the RBAC marker (`:162`)
- Modify: `internal/rbacaudit/required.go:142-146`
- Create: `internal/controller/expose_test.go`

**Interfaces:**
- Consumes: `podspec.MinecraftPort`, `podspec.MinecraftPortName`,
  `podspec.ProxyLabels(network, group string) map[string]string`,
  `podspec.LabelManagedBy`, `podspec.ManagedByValue`.
- Produces:
  - `func (r *ProxyGroupReconciler) reconcileService(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) (*corev1.Service, error)`
    — returns `nil, nil` for the `HostPort` strategy.
  - `func (r *ProxyGroupReconciler) setStatus(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, svc *corev1.Service)`
  - `func proxyAddress(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, svc *corev1.Service) string`
  - `func loadBalancerTrafficPolicy(group *spawneryv1alpha1.ProxyGroup) corev1.ServiceExternalTrafficPolicy`

**Note on reachability from `Reconcile`:** until Task 4 removes the refusal,
`Reconcile` still rejects any non-`NodePort` group before it gets here. The
tests in this task therefore call `r.reconcileService(...)`, `proxyAddress(...)`
and `r.setStatus(...)` directly, which is legitimate — they are package-local
and this is a package-internal test file. Task 4 adds the end-to-end
assertions through `Reconcile`.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/expose_test.go` with the standard Apache header
copied verbatim from the top of `internal/controller/proxygroup_controller.go`,
`package controller`, and:

```go
// A LoadBalancer Service names no node port. One is allocated regardless, by
// the API server, and naming one here would add a second way for two groups
// in different namespaces to collide over a number no player ever dials.
func TestLoadBalancerServiceShape(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type: spawneryv1alpha1.ExposeLoadBalancer,
			LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{
				ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyCluster,
			},
		}
	})

	svc, err := r.reconcileService(f.ctx, group)
	if err != nil {
		t.Fatalf("reconcileService: %v", err)
	}
	if svc == nil {
		t.Fatal("reconcileService returned no Service for a LoadBalancer group")
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Service type = %q, want LoadBalancer", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster {
		t.Errorf("externalTrafficPolicy = %q, want the Cluster the spec asked for",
			svc.Spec.ExternalTrafficPolicy)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("ports = %+v, want exactly the Minecraft port", svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].Port != podspec.MinecraftPort {
		t.Errorf("port = %d, want %d", svc.Spec.Ports[0].Port, podspec.MinecraftPort)
	}
	if svc.Spec.Selector[podspec.LabelRole] != podspec.RoleProxy {
		t.Error("the selector must pin the proxy role, or it would also select server pods")
	}
}

// The CRD defaults externalTrafficPolicy to Local because bans and rate
// limits are built on the client's real IP, and Cluster SNATs it away. A
// ProxyGroup built in a unit test never passes through the API server's
// defaulting, so the default has to exist in the code as well as in the
// marker -- the same hazard podspec.DefaultDrainTimeoutSeconds exists for.
func TestLoadBalancerDefaultsToLocalWithoutTheAPIServer(t *testing.T) {
	group := &spawneryv1alpha1.ProxyGroup{
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			Expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
		},
	}
	if got := loadBalancerTrafficPolicy(group); got != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("externalTrafficPolicy = %q, want Local", got)
	}

	group.Spec.Expose.LoadBalancer = nil
	if got := loadBalancerTrafficPolicy(group); got != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("with no loadBalancer block at all, externalTrafficPolicy = %q, want Local", got)
	}
}

// Nothing inside the cluster dials a proxy: players arrive from outside,
// agents dial the operator, and Velocity dials backends. A Service left
// behind after a switch to HostPort would still hold its node port and still
// select the same pods, so the group would stay reachable by exactly the
// route the switch was meant to end.
func TestSwitchingToHostPortDeletesTheService(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway")

	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService as NodePort: %v", err)
	}
	var before corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &before); err != nil {
		t.Fatalf("the NodePort Service was not created: %v", err)
	}

	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
	}
	svc, err := r.reconcileService(f.ctx, group)
	if err != nil {
		t.Fatalf("reconcileService as HostPort: %v", err)
	}
	if svc != nil {
		t.Errorf("a HostPort group got a Service: %+v", svc.Spec)
	}

	var after corev1.Service
	err = f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &after)
	if !apierrors.IsNotFound(err) {
		t.Errorf("the Service survived the switch to HostPort (err = %v); it still holds "+
			"node port %d and still selects this group's pods",
			err, before.Spec.Ports[0].NodePort)
	}
}

// The operator deletes a Service because it owns it, not because it knows
// the name. A Service somebody else put at the group's name -- an ingress
// shim, a hand-written override -- is not this operator's to remove, and
// removing it would be an unrecoverable action taken on somebody else's
// object.
func TestSwitchingToHostPortLeavesAForeignServiceAlone(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})

	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: f.ns},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Ports:    []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP}},
			Selector: map[string]string{"app": "somebody-elses"},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create the foreign Service: %v", err)
	}

	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService: %v", err)
	}

	var after corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &after); err != nil {
		t.Fatalf("the operator deleted a Service it does not own: %v", err)
	}
}

// proxyAddress publishes an address only for a proxy that is demonstrably
// serving. The LoadBalancer branch is the one where that gate has to be
// stated rather than inherited: its address comes from the Service, which
// knows nothing about readiness, so without the gate status.address would
// point somewhere the moment a load balancer answered -- including for a
// group whose every pod is in ImagePullBackOff.
func TestProxyAddressPerStrategy(t *testing.T) {
	readyPod := func() corev1.Pod {
		return corev1.Pod{
			Status: corev1.PodStatus{
				HostIP: "10.0.0.7",
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			},
		}
	}
	notReadyPod := func() corev1.Pod {
		return corev1.Pod{
			Status: corev1.PodStatus{
				HostIP: "10.0.0.7",
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionFalse},
				},
			},
		}
	}
	withIngress := func(ing ...corev1.LoadBalancerIngress) *corev1.Service {
		return &corev1.Service{Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{Ingress: ing},
		}}
	}

	for _, tc := range []struct {
		name   string
		expose spawneryv1alpha1.ExposeSpec
		pods   []corev1.Pod
		svc    *corev1.Service
		want   string
	}{
		{
			name: "NodePort publishes the node's address",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  &corev1.Service{},
			want: "10.0.0.7:30001",
		},
		{
			name: "HostPort publishes the same node with the host port",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  nil,
			want: "10.0.0.7:25565",
		},
		{
			name: "LoadBalancer publishes the assigned ingress IP",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  withIngress(corev1.LoadBalancerIngress{IP: "192.0.2.10"}),
			want: "192.0.2.10:25565",
		},
		{
			name: "LoadBalancer falls back to the hostname",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  withIngress(corev1.LoadBalancerIngress{Hostname: "lb.example.net"}),
			want: "lb.example.net:25565",
		},
		{
			name: "LoadBalancer with an assigned address but no ready proxy",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			pods: []corev1.Pod{notReadyPod()},
			svc:  withIngress(corev1.LoadBalancerIngress{IP: "192.0.2.10"}),
			want: "",
		},
		{
			name: "LoadBalancer with a ready proxy and nothing assigned",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			pods: []corev1.Pod{readyPod()},
			svc:  withIngress(),
			want: "",
		},
		{
			name: "HostPort with no ready proxy",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			pods: []corev1.Pod{notReadyPod()},
			svc:  nil,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &spawneryv1alpha1.ProxyGroup{
				Spec: spawneryv1alpha1.ProxyGroupSpec{Expose: tc.expose},
			}
			if got := proxyAddress(group, tc.pods, tc.svc); got != tc.want {
				t.Errorf("proxyAddress = %q, want %q", got, tc.want)
			}
		})
	}
}
```

Add whatever imports the file needs: `testing`, `corev1 "k8s.io/api/core/v1"`,
`metav1`, `apierrors "k8s.io/apimachinery/pkg/api/errors"`,
`"sigs.k8s.io/controller-runtime/pkg/client"`,
`spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"`,
`"github.com/spawnery/spawnery/internal/podspec"`.

- [ ] **Step 2: Run the tests to verify they fail**

```
nix develop -c go test ./internal/controller/ -run 'LoadBalancer|HostPort|ProxyAddressPerStrategy' -v
```

Expected: compilation fails — `reconcileService` returns one value, not two;
`loadBalancerTrafficPolicy` is undefined; `proxyAddress` takes
`([]corev1.Pod, int32)`. A compile failure is a legitimate red here.

- [ ] **Step 3: Widen the RBAC marker and the audit table**

In `internal/controller/proxygroup_controller.go`, change the marker at `:162`:

```go
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;delete
```

In `internal/rbacaudit/required.go`, immediately after the existing
`services` rows (`:142-146`), add:

```go
	{Group: "", Resource: "services", Verb: "delete", Why: "reconcileService removes the Service of a group switched to HostPort"},
```

Then regenerate and confirm the audit agrees:

```
nix develop -c make manifests
nix develop -c go test ./internal/rbacaudit/
```

Never hand-edit `config/rbac/role.yaml`.

- [ ] **Step 4: Implement `reconcileService`**

Replace the whole of `reconcileService` in
`internal/controller/proxygroup_controller.go`. Keep the existing doc comment
about `ExternalTrafficPolicy: Local` and the selector — both still apply —
and fold them into the new text:

```go
// reconcileService keeps the group's Service in step with its expose
// strategy, and returns the Service it settled on so the caller can read
// status.loadBalancer off it without a second Get.
//
// HostPort gets no Service and returns nil: nothing inside the cluster dials
// a proxy. Players arrive from outside, agents dial the operator, and
// Velocity dials backends.
func (r *ProxyGroupReconciler) reconcileService(
	ctx context.Context,
	group *spawneryv1alpha1.ProxyGroup,
) (*corev1.Service, error) {
	if group.Spec.Expose.Type == spawneryv1alpha1.ExposeHostPort {
		return nil, r.deleteServiceIfOurs(ctx, group)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: group.Name, Namespace: group.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue

		// The selector must pin the role as well as the group: without it the
		// Service would also select any server pod that happened to share the
		// group name, and players would land on a backend directly.
		svc.Spec.Selector = podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)

		port := corev1.ServicePort{
			Name:       podspec.MinecraftPortName,
			Port:       podspec.MinecraftPort,
			TargetPort: intstr.FromString(podspec.MinecraftPortName),
			Protocol:   corev1.ProtocolTCP,
		}
		switch group.Spec.Expose.Type {
		case spawneryv1alpha1.ExposeLoadBalancer:
			svc.Spec.Type = corev1.ServiceTypeLoadBalancer
			svc.Spec.ExternalTrafficPolicy = loadBalancerTrafficPolicy(group)
			// No node port is named. A LoadBalancer Service gets one anyway,
			// allocated by the API server, and naming one here would add a
			// second way for two groups in different namespaces to collide
			// over a number no player ever dials.
		default:
			svc.Spec.Type = corev1.ServiceTypeNodePort
			// Local, not the Cluster default, for the same reason
			// LoadBalancerSpec.ExternalTrafficPolicy defaults to Local: the
			// default SNATs, so Velocity would never see a player's real IP,
			// and bans and rate limits depend on it. The consequence is the
			// trade-off this makes: a client that reaches a node running no
			// proxy pod for this group gets no answer at all, rather than
			// being routed to one that does. That is consistent with
			// proxyAddress only ever publishing the address of a node that
			// demonstrably runs a ready proxy -- a client dialing the
			// published address never hits the empty case.
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
			port.NodePort = group.Spec.Expose.NodePort.Port
		}
		svc.Spec.Ports = []corev1.ServicePort{port}

		return controllerutil.SetControllerReference(group, svc, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	return svc, nil
}

// loadBalancerTrafficPolicy is the CRD's Local default, restated in code.
// A ProxyGroup built in a unit test never passes through the API server's
// defaulting, and an empty policy on a Service is not a valid value -- the
// same hazard podspec.DefaultDrainTimeoutSeconds exists for.
func loadBalancerTrafficPolicy(group *spawneryv1alpha1.ProxyGroup) corev1.ServiceExternalTrafficPolicy {
	if lb := group.Spec.Expose.LoadBalancer; lb != nil && lb.ExternalTrafficPolicy != "" {
		return lb.ExternalTrafficPolicy
	}
	return corev1.ServiceExternalTrafficPolicyLocal
}

// deleteServiceIfOurs removes the Service a group had before it switched to
// HostPort, and only that one.
//
// The ownership check is the whole of the function's care: a Service
// somebody else put at the group's name is not this operator's to remove,
// and a delete is the one action here that cannot be undone. The
// preconditions pin the object the decision was made about -- between the
// Get and the Delete the name could have come to hold a different object
// entirely.
func (r *ProxyGroupReconciler) deleteServiceIfOurs(
	ctx context.Context,
	group *spawneryv1alpha1.ProxyGroup,
) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKey{Namespace: group.Namespace, Name: group.Name}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner := metav1.GetControllerOf(svc); owner == nil || owner.UID != group.UID {
		return nil
	}
	uid := svc.UID
	rv := svc.ResourceVersion
	return client.IgnoreNotFound(r.Delete(ctx, svc, &client.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
	}))
}
```

- [ ] **Step 5: Implement `proxyAddress` and thread the Service**

Replace `proxyAddress` and `setStatus`'s use of it:

```go
// proxyAddress is where players connect.
//
// Every branch publishes an address only while a proxy is demonstrably
// serving, and that is the property the whole function is built around.
// Empty is the truthful answer otherwise: there is nowhere to connect yet,
// and printing an address for a proxy that is not serving would send players
// at a closed port.
//
// For NodePort and HostPort the address is a node's, and the operator has no
// right to read Node objects -- nor does it need one: hostIP on a ready
// proxy pod is the address of a node that demonstrably has a proxy on it,
// and the pod is already watched. Granting a cluster-wide node read for a
// status string would be the same trade the bootstrapper refused when it
// declined the update verb on ServiceAccounts to restore a cosmetic label.
//
// For LoadBalancer the address comes from the Service instead, and the
// readiness gate has to be stated rather than inherited -- the Service knows
// nothing about whether anything is serving, so without the gate this would
// publish an address the moment a load balancer answered, including for a
// group whose every pod is in ImagePullBackOff.
//
// net.JoinHostPort rather than a format string: a node with an IPv6 hostIP
// needs brackets, and the old formatting produced an address no client could
// use. For an IPv4 address the two are identical.
func proxyAddress(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, svc *corev1.Service) string {
	hostIP := ""
	for i := range pods {
		if isPodReady(&pods[i]) && pods[i].Status.HostIP != "" {
			hostIP = pods[i].Status.HostIP
			break
		}
	}
	if hostIP == "" {
		return ""
	}

	port := func(p int32) string { return strconv.Itoa(int(p)) }

	switch group.Spec.Expose.Type {
	case spawneryv1alpha1.ExposeLoadBalancer:
		if svc == nil {
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
		return net.JoinHostPort(hostIP, port(group.Spec.Expose.HostPort.Port))
	default:
		if group.Spec.Expose.NodePort == nil {
			return ""
		}
		return net.JoinHostPort(hostIP, port(group.Spec.Expose.NodePort.Port))
	}
}
```

Change `setStatus`'s signature to
`func (r *ProxyGroupReconciler) setStatus(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod, svc *corev1.Service)`
and its address line to `group.Status.Address = proxyAddress(group, pods, svc)`.

In `Reconcile`, capture the Service and pass it on:

```go
	svc, err := r.reconcileService(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}
```

and at the bottom, `r.setStatus(group, pods, svc)`.

Add `"net"` and `"strconv"` to the file's imports if they are not there.

- [ ] **Step 6: Run the tests to verify they pass**

```
nix develop -c go test ./internal/controller/ -run 'LoadBalancer|HostPort|ProxyAddressPerStrategy' -v
nix develop -c go test ./internal/controller/ ./internal/rbacaudit/
```

Expected: PASS. The pre-existing `TestProxyGroupCreatesItsPodsAndService` and
`TestProxyGroupAddressComesFromAReadyPodsHostIP` must still pass unchanged —
if either needed editing, say exactly what changed and why.

- [ ] **Step 7: Mutate, and report what happened**

For each, run `nix develop -c go test ./internal/controller/ ./internal/rbacaudit/`,
record the failure, revert.

1. In `deleteServiceIfOurs`, remove the `GetControllerOf` guard entirely.
   Expected: `TestSwitchingToHostPortLeavesAForeignServiceAlone` fails with
   `the operator deleted a Service it does not own`.
2. In `reconcileService`, return the Service instead of `nil` for `HostPort`
   (skip the delete). Expected: `TestSwitchingToHostPortDeletesTheService`
   fails on both the non-nil return and the surviving Service.
3. In `proxyAddress`, move the `hostIP == ""` gate below the `switch` so the
   `LoadBalancer` branch no longer waits for a ready proxy. Expected:
   `TestProxyAddressPerStrategy/LoadBalancer_with_an_assigned_address_but_no_ready_proxy`
   fails with `proxyAddress = "192.0.2.10:25565", want ""`.
4. In `loadBalancerTrafficPolicy`, return `corev1.ServiceExternalTrafficPolicyCluster`
   as the fallback. Expected:
   `TestLoadBalancerDefaultsToLocalWithoutTheAPIServer` fails twice.
5. Delete the `services: delete` row from `internal/rbacaudit/required.go`,
   keeping the marker. Expected: `internal/rbacaudit` goes red naming the
   verb. Then restore the row and remove `delete` from the marker instead,
   run `make manifests`, and confirm it goes red the other way too. **Report
   both directions.**

- [ ] **Step 8: Full suite and commit**

```
nix develop -c make test
git add -A
git commit
```

Message subject:
`feat(6c): the Service and the address follow the strategy`.
The body should name the one new permission (`services: delete`), the
ownership guard that makes it safe, and the readiness gate on the
LoadBalancer address with the reason it is there.

---

### Task 3: The annotations the operator owns

**Files:**
- Modify: `internal/podspec/labels.go` (one constant)
- Modify: `internal/controller/proxygroup_controller.go` (`reconcileService`)
- Test: `internal/controller/expose_test.go`

**Interfaces:**
- Consumes: `reconcileService` from Task 2.
- Produces:
  - `podspec.AnnotationExposeAnnotations = "spawnery.cloud/expose-annotations"`
  - `func applyExposeAnnotations(svc *corev1.Service, want map[string]string)`

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/expose_test.go`:

```go
// spec.expose.loadBalancer.annotations is the only place a user writes into
// an object a third-party controller also writes into -- MetalLB and kube-vip
// both annotate the Service they act on. So the operator cannot treat the
// spec's map as the whole truth and delete whatever is not in it, and it
// cannot simply never delete either: a user who removes a pool annotation
// would see nothing happen, permanently, with no message anywhere. It
// records the keys it set and removes only those.
func TestLoadBalancerAnnotationsAreOwnedAndReleased(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type: spawneryv1alpha1.ExposeLoadBalancer,
			LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{
				Annotations: map[string]string{
					"metallb.universe.tf/address-pool": "minecraft",
					"metallb.universe.tf/allow-shared-ip": "spawnery",
				},
			},
		}
	})

	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService: %v", err)
	}

	// A third party annotates the Service the way a real load balancer
	// controller does. Nothing the operator does afterwards may remove it.
	var svc corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	if svc.Annotations["metallb.universe.tf/address-pool"] != "minecraft" {
		t.Fatalf("the spec's annotation did not reach the Service: %+v", svc.Annotations)
	}
	svc.Annotations["metallb.universe.tf/ip-allocated-from-pool"] = "minecraft"
	if err := f.c.Update(f.ctx, &svc); err != nil {
		t.Fatalf("annotate the Service as a third party would: %v", err)
	}

	// The user drops one of the two keys they had set.
	group.Spec.Expose.LoadBalancer.Annotations = map[string]string{
		"metallb.universe.tf/address-pool": "minecraft",
	}
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService after the spec changed: %v", err)
	}

	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	if _, still := svc.Annotations["metallb.universe.tf/allow-shared-ip"]; still {
		t.Error("an annotation removed from the spec survived on the Service; a user " +
			"who removes one sees nothing happen, permanently")
	}
	if svc.Annotations["metallb.universe.tf/address-pool"] != "minecraft" {
		t.Error("the annotation still in the spec was removed too")
	}
	if svc.Annotations["metallb.universe.tf/ip-allocated-from-pool"] != "minecraft" {
		t.Error("the operator removed an annotation it never set. That key belongs to " +
			"the load balancer controller, and taking it away is how a working " +
			"allocation gets torn down")
	}
}

// A group that leaves LoadBalancer behind takes its annotations with it, and
// the bookkeeping key goes too -- otherwise the Service carries a record of
// keys nobody owns, and the next LoadBalancer group at that name inherits it.
func TestLeavingLoadBalancerReleasesTheAnnotations(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type: spawneryv1alpha1.ExposeLoadBalancer,
			LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{
				Annotations: map[string]string{"metallb.universe.tf/address-pool": "minecraft"},
			},
		}
	})
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService: %v", err)
	}

	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
	}
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("reconcileService as NodePort: %v", err)
	}

	var svc corev1.Service
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc); err != nil {
		t.Fatalf("get the Service: %v", err)
	}
	if _, still := svc.Annotations["metallb.universe.tf/address-pool"]; still {
		t.Error("a LoadBalancer annotation survived the switch to NodePort")
	}
	if _, still := svc.Annotations[podspec.AnnotationExposeAnnotations]; still {
		t.Error("the bookkeeping key survived with nothing left to account for")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```
nix develop -c go test ./internal/controller/ -run 'Annotations' -v
```

Expected: compile failure on `podspec.AnnotationExposeAnnotations`, then —
once the constant exists — `an annotation removed from the spec survived`.

- [ ] **Step 3: Add the constant**

In `internal/podspec/labels.go`, beside the other keys:

```go
// AnnotationExposeAnnotations records which annotation keys on a proxy
// group's Service the operator put there, comma-separated and sorted.
//
// It exists because the Service is shared ground: spec.expose.loadBalancer
// .annotations is written by the user, and the load balancer controller that
// acts on it -- MetalLB, kube-vip -- annotates the same object with its own
// keys. Without this record the operator could only choose between never
// removing a key the user dropped, and removing keys that were never its to
// touch. Sorted so the value is stable across passes; an unsorted join would
// make CreateOrUpdate write on every reconcile.
const AnnotationExposeAnnotations = "spawnery.cloud/expose-annotations"
```

- [ ] **Step 4: Implement**

In `internal/controller/proxygroup_controller.go`, add:

```go
// applyExposeAnnotations reconciles the annotations the operator owns on a
// Service, leaving every other key untouched. See
// podspec.AnnotationExposeAnnotations for why the record is necessary.
func applyExposeAnnotations(svc *corev1.Service, want map[string]string) {
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	if owned := svc.Annotations[podspec.AnnotationExposeAnnotations]; owned != "" {
		for _, k := range strings.Split(owned, ",") {
			if _, still := want[k]; !still {
				delete(svc.Annotations, k)
			}
		}
	}
	keys := make([]string, 0, len(want))
	for k, v := range want {
		svc.Annotations[k] = v
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		delete(svc.Annotations, podspec.AnnotationExposeAnnotations)
		return
	}
	sort.Strings(keys)
	svc.Annotations[podspec.AnnotationExposeAnnotations] = strings.Join(keys, ",")
}
```

Call it inside `reconcileService`'s mutate function, once, before the
`switch`, so both Service-backed strategies pass through it:

```go
		// Unconditional rather than inside the LoadBalancer arm: a group that
		// leaves LoadBalancer has to release the keys it set, and nil is how
		// that is said.
		var lbAnnotations map[string]string
		if group.Spec.Expose.Type == spawneryv1alpha1.ExposeLoadBalancer &&
			group.Spec.Expose.LoadBalancer != nil {
			lbAnnotations = group.Spec.Expose.LoadBalancer.Annotations
		}
		applyExposeAnnotations(svc, lbAnnotations)
```

Add `"sort"` and `"strings"` to the imports if absent.

- [ ] **Step 5: Run to verify it passes**

```
nix develop -c go test ./internal/controller/ -v -run 'Annotations'
```

- [ ] **Step 6: Mutate, and report what happened**

1. Make `applyExposeAnnotations` skip the removal loop entirely. Expected:
   `an annotation removed from the spec survived on the Service`.
2. Make it delete every key not in `want` (ignoring the record). Expected:
   `the operator removed an annotation it never set`.
3. Remove the `sort.Strings(keys)` call. Expected: **nothing fails.** That is
   the correct outcome to report — the sort is about write churn, not
   correctness, and no test here can see it. Say so; do not invent a test
   that asserts a map iteration order.

- [ ] **Step 7: Full suite and commit**

```
nix develop -c make test
git add -A
git commit
```

Subject: `feat(6c): own the annotations we set, and only those`.

---

### Task 4: The dispatch, and the refusal that leaves

**Files:**
- Modify: `internal/controller/proxygroup_controller.go:210-227` (the
  refusal), `:296-300` (the doc comment on `refuse`)
- Modify: `internal/controller/readinessdivergence.go:41-47`
- Modify: `internal/controller/proxygroup_controller_test.go:288-307` (delete
  `TestProxyGroupRefusesLoadBalancer`)
- Test: `internal/controller/expose_test.go`

**Interfaces:**
- Consumes: `reconcileService` and `proxyAddress` from Task 2.
- Produces: `func exposeImplemented(t spawneryv1alpha1.ExposeType) bool`

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/expose_test.go`:

```go
// The API server's enum makes the false branch unreachable for any object
// that exists. The guard is here for the day a fourth value is added to the
// enum without a branch in reconcileService: a refusal on the object is a
// message a user can read, and a nil dereference is a crash loop.
//
// A pure function rather than an inline default arm because the enum is
// closed: no ProxyGroup carrying an unknown type can be created through
// envtest, so the branch is reachable from a test only here.
func TestExposeImplementedCoversTheEnumAndNothingElse(t *testing.T) {
	for _, known := range []spawneryv1alpha1.ExposeType{
		spawneryv1alpha1.ExposeNodePort,
		spawneryv1alpha1.ExposeLoadBalancer,
		spawneryv1alpha1.ExposeHostPort,
	} {
		if !exposeImplemented(known) {
			t.Errorf("%s is in the CRD's enum, so a user can create a group asking for "+
				"it, and this operator refuses it", known)
		}
	}
	for _, unknown := range []spawneryv1alpha1.ExposeType{"", "Anycast", "nodeport"} {
		if exposeImplemented(unknown) {
			t.Errorf("%q is accepted as implemented; reconcileService has no branch for "+
				"it and would dereference a nil sub-block", unknown)
		}
	}
}

// The three strategies end to end, through Reconcile rather than through the
// pieces, because the refusal that stood in front of two of them is what this
// task removes.
func TestReconcileAcceptsEveryStrategy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expose     spawneryv1alpha1.ExposeSpec
		wantSvc    bool
		wantType   corev1.ServiceType
		wantHostPort int32
	}{
		{
			name: "NodePort",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			wantSvc: true, wantType: corev1.ServiceTypeNodePort,
		},
		{
			name: "LoadBalancer",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:         spawneryv1alpha1.ExposeLoadBalancer,
				LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{},
			},
			wantSvc: true, wantType: corev1.ServiceTypeLoadBalancer,
		},
		{
			name: "HostPort",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeHostPort,
				HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
			},
			wantSvc: false, wantHostPort: 25565,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := proxyGroupReconciler(f)
			f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
				g.Spec.Expose = tc.expose
			})

			f.reconcileProxyGroup(r, "gateway")

			group := f.proxyGroup("gateway")
			if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
				metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
				t.Fatalf("conditions = %+v, want Accepted=True", group.Status.Conditions)
			}

			pods := f.proxyPods("gateway")
			if len(pods) == 0 {
				t.Fatal("an accepted group created no proxy pods")
			}
			var hostPort int32
			for _, p := range pods[0].Spec.Containers[0].Ports {
				if p.Name == podspec.MinecraftPortName {
					hostPort = p.HostPort
				}
			}
			if hostPort != tc.wantHostPort {
				t.Errorf("the pod's minecraft hostPort = %d, want %d", hostPort, tc.wantHostPort)
			}

			var svc corev1.Service
			err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "gateway"}, &svc)
			switch {
			case tc.wantSvc && err != nil:
				t.Fatalf("no Service for a %s group: %v", tc.name, err)
			case tc.wantSvc && svc.Spec.Type != tc.wantType:
				t.Errorf("Service type = %q, want %q", svc.Spec.Type, tc.wantType)
			case !tc.wantSvc && !apierrors.IsNotFound(err):
				t.Errorf("a HostPort group got a Service (err = %v)", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```
nix develop -c go test ./internal/controller/ -run 'ExposeImplemented|ReconcileAcceptsEveryStrategy' -v
```

Expected: `exposeImplemented` undefined; and once it exists, the
`LoadBalancer` and `HostPort` subtests fail with
`conditions = ... want Accepted=True` because the refusal is still there.

- [ ] **Step 3: Implement the dispatch**

In `internal/controller/proxygroup_controller.go`, replace the whole refusal
block at `:210-227` — comment included — with:

```go
	// The strategies differ in reconcileService and proxyAddress and nowhere
	// else. This guard is not about any of them: the CRD's enum is closed, so
	// no object carrying an unrecognised type can be created, and the branch
	// below is reachable only if a fourth value is added to the enum without
	// a branch to serve it. A refusal on the object is a message a user can
	// read; the alternative is a nil dereference on a sub-block that was
	// never validated.
	if !exposeImplemented(group.Spec.Expose.Type) {
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonExposeNotImplemented,
			fmt.Sprintf("expose.type %s is not implemented by this operator",
				group.Spec.Expose.Type))
		// refuse rather than a bare return, for the reason it documents: the
		// player-safety pass has to run again, and whether a proxy is
		// occupied is a fact about the agent registry that nothing watches.
		// A user whose group is stuck here has real pods with real players on
		// them for as long as the refusal stands.
		return r.refuse(ctx, group)
	}
```

and add, near `setProxyGroupAccepted`:

```go
// exposeImplemented reports whether this operator has a branch for the
// strategy. See the call site in Reconcile for why it exists at all, and
// TestExposeImplementedCoversTheEnumAndNothingElse for why it is a function
// rather than an inline default arm.
func exposeImplemented(t spawneryv1alpha1.ExposeType) bool {
	switch t {
	case spawneryv1alpha1.ExposeNodePort,
		spawneryv1alpha1.ExposeLoadBalancer,
		spawneryv1alpha1.ExposeHostPort:
		return true
	default:
		return false
	}
}
```

- [ ] **Step 4: Correct the two comments the change falsifies**

In `internal/controller/proxygroup_controller.go`, `refuse`'s doc comment
opens: *"refuse is the shared tail of the three paths that give up before
reconcileReplicas: a missing Network, one that is not Accepted, and an
expose.type this milestone does not implement."* Rewrite the list to name a
missing Network, one that is not Accepted, and an `expose.type` this operator
has no branch for — and say that the third is unreachable while the CRD's
enum and `exposeImplemented` agree.

In `internal/controller/readinessdivergence.go:41-47`, the type's doc comment
says *"Three of its steady-state early returns handle that themselves --
NetworkNotFound, NetworkNotAccepted and ExposeNotImplemented all return before
reconcileReplicas runs"*. Two are now reachable in practice. Rewrite it to
name `NetworkNotFound` and `NetworkNotAccepted` as the steady-state pair, and
`ExposeNotImplemented` as a third that shares the path but cannot be reached
while the enum is closed. Do not delete the mention — a reader who greps for
the reason must still land here.

- [ ] **Step 5: Delete the obsolete test**

Delete `TestProxyGroupRefusesLoadBalancer` from
`internal/controller/proxygroup_controller_test.go` (`:288-307`, including its
doc comment, which begins *"Milestone 6 owns the other two strategies"*).
`TestReconcileAcceptsEveryStrategy` and
`TestExposeImplementedCoversTheEnumAndNothingElse` are what replace it, and
between them they assert both directions.

- [ ] **Step 6: Run to verify it passes**

```
nix develop -c go test ./internal/controller/ -v
```

Expected: PASS, including every pre-existing test.

- [ ] **Step 7: Mutate, and report what happened**

1. Make `exposeImplemented` return `true` unconditionally. Expected:
   `TestExposeImplementedCoversTheEnumAndNothingElse` fails on all three
   unknown values.
2. Remove `spawneryv1alpha1.ExposeHostPort` from `exposeImplemented`'s case
   list. Expected: `TestExposeImplementedCoversTheEnumAndNothingElse` fails
   naming HostPort, and `TestReconcileAcceptsEveryStrategy/HostPort` fails on
   the Accepted condition.
3. Restore the old blanket refusal (`!= ExposeNodePort`). Expected: two
   subtests of `TestReconcileAcceptsEveryStrategy` fail.

- [ ] **Step 8: Full suite and commit**

```
nix develop -c make test
git add -A
git commit
```

Subject: `feat(6c): accept the two strategies the API has always described`.
The body should note that the refusal is not deleted but narrowed to an enum
value the controller does not know, and name the two doc comments corrected
because they counted the old paths.

---

### Task 5: A proxy pod that cannot exist says so

**Files:**
- Modify: `api/v1alpha1/common_types.go` (condition reasons)
- Modify: `internal/controller/proxygroup_controller.go` (`Reconcile`, and a
  new `reportBlockedProxies`)
- Test: `internal/controller/expose_test.go`

**Interfaces:**
- Consumes: the dispatch from Task 4.
- Produces:
  - `spawneryv1alpha1.ReasonProxyPodRejected = "ProxyPodRejected"`
  - `spawneryv1alpha1.ReasonProxyPodUnschedulable = "ProxyPodUnschedulable"`
  - `spawneryv1alpha1.ReasonProxyPodsAdmitted = "ProxyPodsAdmitted"`
  - `func (r *ProxyGroupReconciler) reportBlockedProxies(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod)`
  - `func setProxyPodsBlocked(group *spawneryv1alpha1.ProxyGroup, reason, message string) bool`
    — returns whether the condition transitioned to True on this call, so the
    caller can fire an event on the flank only.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/expose_test.go`:

```go
// A HostPort group in a namespace enforcing Pod Security baseline never gets
// a pod: the API server refuses the create outright. Before this, the error
// went to the log and the group reported Pending with no reason at all --
// for as long as the namespace's policy stood, which is forever.
//
// envtest runs the PodSecurity admission plugin, so the label below is
// enforced here exactly as it is in a cluster.
func TestARejectedProxyPodIsReportedOnTheGroup(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.enforcePodSecurity(t, "baseline")
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})

	// The reconcile returns the API server's error, so reconcileProxyGroup --
	// which fails the test on any error -- is the wrong helper here.
	_, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gateway", Namespace: f.ns},
	})
	if err == nil {
		t.Fatal("the reconcile succeeded in a namespace that forbids host ports")
	}

	group := f.proxyGroup("gateway")
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True. Without it the group reports Pending and "+
			"only the operator's log says why", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonProxyPodRejected {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonProxyPodRejected)
	}
	if !strings.Contains(cond.Message, "PodSecurity") {
		t.Errorf("message = %q; it must carry the API server's own words, because the "+
			"remedy is in them and nothing else knows it", cond.Message)
	}
	if group.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded", group.Status.Phase)
	}
}

// With hostPort the kube-scheduler places at most one pod of a group per
// node, so replicas is capped by the node count -- the likeliest HostPort
// mistake there is. The surplus pod exists and stays Pending, and the
// scheduler's own message on it is the only thing that explains why.
//
// envtest runs no scheduler, so the condition is written here the way one
// would write it. That is the honest shape of this test: it asserts the
// operator's reading of PodScheduled=False, not the scheduler's decision to
// set it.
func TestAnUnschedulableProxyPodIsReportedOnTheGroup(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("no proxy pods to make unschedulable")
	}
	const schedulerSays = "0/1 nodes are available: 1 node(s) didn't have free ports " +
		"for the requested pod ports."
	pods[0].Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  "Unschedulable",
		Message: schedulerSays,
	}}
	if err := f.c.Status().Update(f.ctx, &pods[0]); err != nil {
		t.Fatalf("mark the pod unschedulable: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %+v, want True", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonProxyPodUnschedulable {
		t.Errorf("reason = %q, want %q", cond.Reason,
			spawneryv1alpha1.ReasonProxyPodUnschedulable)
	}
	if !strings.Contains(cond.Message, "free ports") {
		t.Errorf("message = %q, want the scheduler's own text", cond.Message)
	}
	if !strings.Contains(cond.Message, pods[0].Name) {
		t.Errorf("message = %q, want the name of the pod that cannot be placed -- with "+
			"several pods the group's condition is otherwise unattributable",
			cond.Message)
	}
}

// A group whose pods all exist says so, or the condition would latch True
// after any transient refusal and never come back.
func TestAGroupWithItsPodsIsNotDegraded(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if cond == nil {
		t.Fatal("no Degraded condition at all; False is a verdict and absent is not")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Degraded = %+v, want False", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonProxyPodsAdmitted {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonProxyPodsAdmitted)
	}
}
```

Add the fixture helper this needs, in `internal/controller/expose_test.go`:

```go
// enforcePodSecurity labels the fixture's namespace so the API server's
// PodSecurity admission plugin enforces a profile on it. envtest runs that
// plugin, so this is the real control, not a stand-in for one.
func (f *fixture) enforcePodSecurity(t *testing.T, profile string) {
	t.Helper()
	var ns corev1.Namespace
	if err := f.c.Get(f.ctx, client.ObjectKey{Name: f.ns}, &ns); err != nil {
		t.Fatalf("get namespace %s: %v", f.ns, err)
	}
	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}
	ns.Labels["pod-security.kubernetes.io/enforce"] = profile
	if err := f.c.Update(f.ctx, &ns); err != nil {
		t.Fatalf("label namespace %s: %v", f.ns, err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

```
nix develop -c go test ./internal/controller/ -run 'RejectedProxyPod|UnschedulableProxyPod|NotDegraded' -v
```

Expected: compile failure on the three reason constants, then all three tests
failing on a missing `Degraded` condition.

**If `TestARejectedProxyPodIsReportedOnTheGroup` does not even get an error
from the reconcile**, the envtest API server is not running the PodSecurity
plugin. Do not paper over it: report it as a blocker with the exact
behaviour observed, because the E2E scenario in Task 6 rests on the same
mechanism and the design's §7 claims it.

- [ ] **Step 3: Add the reasons**

In `api/v1alpha1/common_types.go`, inside the `// Condition reasons.` block:

```go
	// The three ProxyPods reasons on a ProxyGroup's Degraded condition. Two
	// failures rather than one because the remedies differ: a create the API
	// server refused is fixed at the namespace's policy, and a pod the
	// scheduler cannot place is fixed at the node count or at replicas.
	ReasonProxyPodRejected      = "ProxyPodRejected"
	ReasonProxyPodUnschedulable = "ProxyPodUnschedulable"
	ReasonProxyPodsAdmitted     = "ProxyPodsAdmitted"
```

- [ ] **Step 4: Implement**

In `internal/controller/proxygroup_controller.go`:

```go
// setProxyPodsBlocked records why a proxy pod this group asked for does not
// exist. It reports whether the condition transitioned to True on this call,
// so the caller can put an event on the flank rather than on every resync.
//
// This is the first writer of Degraded on a ProxyGroup. setStatus has always
// read it and routed it to phase Degraded; until now only ServerGroup ever
// set one.
func setProxyPodsBlocked(group *spawneryv1alpha1.ProxyGroup, reason, message string) bool {
	was := meta.IsStatusConditionTrue(group.Status.Conditions,
		spawneryv1alpha1.ConditionDegraded)
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionDegraded,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})
	return !was
}

// reportBlockedProxies is the second of the two ways a proxy pod fails to
// exist: the scheduler has nowhere to put it. With hostPort at most one pod
// of a group fits per node, so replicas is silently capped by the node count
// -- the likeliest HostPort mistake there is, and one that produces a pod
// that exists, never runs, and explains itself only on its own object.
//
// The pod's name is in the message because a group has several, and a
// condition that says only "a pod cannot be placed" cannot be acted on.
//
// It does not count nodes and predict. Doing that would mean reimplementing
// the scheduler's view of node selectors, taints and foreign hostPort
// holders in order to guess ahead of it, and being wrong the moment a node
// joins. Both halves of this condition report what the cluster said.
func (r *ProxyGroupReconciler) reportBlockedProxies(
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
) {
	for i := range pods {
		for _, c := range pods[i].Status.Conditions {
			if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse {
				continue
			}
			if setProxyPodsBlocked(group, spawneryv1alpha1.ReasonProxyPodUnschedulable,
				fmt.Sprintf("%s cannot be scheduled: %s", pods[i].Name, c.Message)) {
				r.Recorder.Eventf(group, corev1.EventTypeWarning, "ProxyPodBlocked",
					"proxy pod %s cannot be scheduled: %s", pods[i].Name, c.Message)
			}
			return
		}
	}
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionDegraded,
		Status:  metav1.ConditionFalse,
		Reason:  spawneryv1alpha1.ReasonProxyPodsAdmitted,
		Message: "every proxy pod this group asked for exists",
	})
}
```

In `Reconcile`, wrap the `reconcileReplicas` call:

```go
	if err := r.reconcileReplicas(ctx, network, group, pods); err != nil {
		// A create the API server refused is the group's business and not
		// only the log's. IsForbidden covers both ways it happens: a Pod
		// Security profile that forbids the pod's shape -- which is how every
		// HostPort group in a baseline or restricted namespace ends -- and an
		// RBAC grant the operator does not have. IsInvalid covers a pod the
		// API server rejects outright, a quota or a webhook among them.
		//
		// The status write is on the error path deliberately: without it the
		// reconcile returns having recorded nothing, and the group sits at
		// Pending with no conditions, indistinguishable from one no reconcile
		// has ever touched. A failure to write the status is reported by
		// returning the original error regardless -- the create failure is
		// the cause and the one worth backing off on.
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			if setProxyPodsBlocked(group, spawneryv1alpha1.ReasonProxyPodRejected, err.Error()) {
				r.Recorder.Eventf(group, corev1.EventTypeWarning, "ProxyPodBlocked",
					"the API server refused a proxy pod: %s", err.Error())
			}
			group.Status.Phase = "Degraded"
			if werr := r.writeStatus(ctx, group); werr != nil {
				log.FromContext(ctx).Error(werr, "recording a refused proxy pod on the group")
			}
		}
		return ctrl.Result{}, err
	}
```

and, after the pods are re-read and before `setStatus`:

```go
	r.reportBlockedProxies(group, pods)
```

Check what logging import the file already uses for `log.FromContext`; if it
uses a different handle, follow it.

- [ ] **Step 5: Run to verify they pass**

```
nix develop -c go test ./internal/controller/ -v
```

- [ ] **Step 6: Mutate, and report what happened**

1. Change the `Reconcile` guard from `IsForbidden(err) || IsInvalid(err)` to
   `false`. Expected: `TestARejectedProxyPodIsReportedOnTheGroup` fails on the
   missing Degraded condition.
2. Drop the `group.Status.Phase = "Degraded"` line. Expected: that same test
   fails on `phase = "Pending", want Degraded`. **If it does not fail**, the
   phase is being derived somewhere else on this path — find out where, say
   so, and remove the redundant line rather than keeping both.
3. In `reportBlockedProxies`, drop the pod name from the message. Expected:
   `TestAnUnschedulableProxyPodIsReportedOnTheGroup` fails on
   `want the name of the pod that cannot be placed`.
4. In `reportBlockedProxies`, delete the trailing `SetStatusCondition` that
   writes False. Expected: `TestAGroupWithItsPodsIsNotDegraded` fails with
   `no Degraded condition at all`.
5. Replace `c.Message` with a fixed string like `"unschedulable"`. Expected:
   the unschedulable test fails on `want the scheduler's own text`.

- [ ] **Step 7: Full suite and commit**

```
nix develop -c make test
git add -A
git commit
```

Subject: `feat(6c): say why a proxy pod does not exist`.
The body should state that this is the first writer of `Degraded` on a
`ProxyGroup`, name the two reasons and why they are two, and say plainly that
neither half predicts — both report what the cluster said.

---

### Task 6: The driven run

**Files:**
- Modify: `test/e2e/manifests/e2e.yaml`
- Modify: `hack/e2e.sh` (around `:127-128`)
- Modify: `test/e2e/e2e_test.go` (the `t.Run` list at `:104-117`, and
  `theOperatorWasNeverDenied` at `:207`)
- Create: `test/e2e/expose_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-5.
- Produces: four `func theXxx(t *testing.T)` scenarios, listed below by their
  exact names.

**Before you start:** read `test/e2e/e2e_test.go` end to end. It documents the
harness's rules — the ordered `t.Run` list, why nothing sleeps and then
asserts, what `eventually` and `eventuallyStable` are for, and that no image
in the manifest resolves so no container process ever runs. Every scenario
below is written to hold under that last constraint.

Running the suite takes several minutes and creates a kind cluster:

```
nix develop -c make e2e
```

`E2E_KEEP=1 nix develop -c make e2e` leaves the cluster standing for
inspection. Use it while iterating.

- [ ] **Step 1: Extend the manifest**

Append to `test/e2e/manifests/e2e.yaml`. Note the header comment at the top
of that file explaining why every image is unresolvable — keep that true.

```yaml
---
# gateway-lb exercises the LoadBalancer branch. kind runs no load balancer
# controller, so status.loadBalancer stays empty until the test writes an
# ingress entry into it by hand. That is a test of the operator's read-back,
# and nothing here claims a load balancer was involved.
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway-lb
  namespace: minecraft
spec:
  networkRef:
    name: production
  replicas: 1
  image: ghcr.io/spawnery/velocity:e2e-no-such-tag
  expose:
    type: LoadBalancer
    loadBalancer:
      externalTrafficPolicy: Local
      annotations:
        metallb.universe.tf/address-pool: spawnery-e2e
  routing:
    fallbackGroups:
      - lobby
---
# gateway-host binds 25565 on the node. The cluster has one node, so its
# second replica is the surplus the scheduler cannot place -- which is the
# point: it is the likeliest HostPort mistake a user can make, and this run
# gets it for free.
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway-host
  namespace: minecraft
spec:
  networkRef:
    name: production
  replicas: 2
  image: ghcr.io/spawnery/velocity:e2e-no-such-tag
  expose:
    type: HostPort
    hostPort:
      port: 25565
  routing:
    fallbackGroups:
      - lobby
---
# gateway-switch starts as NodePort and is patched to HostPort by
# aSwitchToHostPortRemovesTheService. It is a group of its own rather than a
# mutation of `gateway`, because the scenario list is ordered and every later
# scenario would inherit the change. Its host port is 25566, not 25565:
# gateway-host's pod holds that one on the single node whether or not its
# container ever starts.
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway-switch
  namespace: minecraft
spec:
  networkRef:
    name: production
  replicas: 1
  image: ghcr.io/spawnery/velocity:e2e-no-such-tag
  expose:
    type: NodePort
    nodePort:
      port: 30766
  routing:
    fallbackGroups:
      - lobby
---
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft-baseline
stringData:
  secret: e2e-forwarding-secret
---
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: production
  namespace: minecraft-baseline
spec:
  forwardingSecretRef:
    name: velocity-forwarding-secret
  defaults:
    minecraftVersion: "26.2"
---
# The group whose pods the API server refuses. Pod Security baseline
# disallows host ports outright, so no pod of this group can ever be created,
# and the refusal is the only thing this run can observe being enforced by
# anything.
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway-forbidden
  namespace: minecraft-baseline
spec:
  networkRef:
    name: production
  replicas: 1
  image: ghcr.io/spawnery/velocity:e2e-no-such-tag
  expose:
    type: HostPort
    hostPort:
      port: 25565
  routing:
    fallbackGroups:
      - lobby
```

The `minecraft-baseline` namespace itself is **not** in the manifest — it is
created by the script, for the reason the script already gives for
`minecraft`: the forwarding-secret grant has to exist before the operator
looks.

- [ ] **Step 2: Extend the script**

In `hack/e2e.sh`, immediately after the existing two lines
(`kubectl create namespace minecraft` and the `kubectl apply -n minecraft`):

```bash
# The second namespace exists to be hostile. Pod Security baseline disallows
# host ports, so the HostPort group the manifest puts here can never get a
# pod -- which is the one refusal this whole run can observe being enforced,
# and it is the API server enforcing it, not a CNI. The label goes on at
# creation rather than later so no pod can slip in before it.
kubectl create namespace minecraft-baseline
kubectl label namespace minecraft-baseline pod-security.kubernetes.io/enforce=baseline
kubectl apply -n minecraft-baseline -f config/rbac/forwarding-secret-reader.yaml
```

- [ ] **Step 3: Narrow the denial check**

In `test/e2e/e2e_test.go`, change the loop body of
`theOperatorWasNeverDenied` (`:207`):

```go
		// A Pod Security rejection is not an RBAC denial, and it carries the
		// same `is forbidden:` prefix. aForbiddenHostPortIsReportedOnTheGroup
		// causes one on purpose -- it is the only enforced refusal this run
		// can observe -- and without this exclusion the last and most
		// important scenario of the run would fail for a reason another
		// scenario created.
		//
		// The exclusion is one substring on purpose. Everything else the API
		// server phrases with `is forbidden:` still counts, including an RBAC
		// denial on a pod create, which shares nothing with this text.
		if strings.Contains(line, "is forbidden:") &&
			!strings.Contains(line, "violates PodSecurity") {
			offenders = append(offenders, line)
		}
```

- [ ] **Step 4: Write the four scenarios**

Create `test/e2e/expose_test.go` with the Apache header copied from
`test/e2e/e2e_test.go`, `package e2e`, and:

```go
// theLoadBalancerGroupGetsItsService checks the Service, and then checks that
// an assigned address is NOT published while nothing is serving.
//
// The name says Service rather than address on purpose. kind runs no load
// balancer controller, so the ingress entry below is written by this test.
// What that proves is the readiness gate on proxyAddress: no image in this
// run's manifest resolves, so no proxy is ever ready, and status.address must
// stay empty even with an address assigned. The other half of that branch --
// the address appearing once both conditions hold -- is proven in envtest,
// where a test can make a pod ready. Nothing here says a load balancer was
// involved, because none was.
func theLoadBalancerGroupGetsItsService(t *testing.T) {
	var svc corev1.Service
	eventually(t, 2*time.Minute, "the gateway-lb Service", func() (bool, string) {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-lb"}, &svc); err != nil {
			return false, err.Error()
		}
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			return false, "type is " + string(svc.Spec.Type)
		}
		return true, ""
	})

	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("externalTrafficPolicy = %q, want Local. Cluster SNATs the client "+
			"address away, and bans and rate limits are built on it",
			svc.Spec.ExternalTrafficPolicy)
	}
	if svc.Annotations["metallb.universe.tf/address-pool"] != "spawnery-e2e" {
		t.Errorf("annotations = %+v, want the manifest's address-pool. A pool selector "+
			"that does not reach the Service is how a LoadBalancer group silently "+
			"lands in the wrong network", svc.Annotations)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 25565 {
		t.Errorf("ports = %+v, want exactly 25565", svc.Spec.Ports)
	}

	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}
	if err := k8s.Status().Update(ctx, &svc); err != nil {
		t.Fatalf("write an ingress address the way a load balancer controller would: %v", err)
	}

	eventuallyStable(t, 90*time.Second, 30*time.Second,
		"status.address to stay empty while no proxy is ready", func() (bool, string) {
			var group spawneryv1alpha1.ProxyGroup
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-lb"}, &group); err != nil {
				return false, err.Error()
			}
			if group.Status.Address != "" {
				return false, "address is " + group.Status.Address
			}
			return true, ""
		})
}

// theHostPortGroupBindsThePortAndHasNoService is the strategy with no Service
// at all: nothing inside the cluster dials a proxy, so there is nothing for
// one to do.
//
// The group asks for two replicas on a one-node cluster. hostPort lets the
// scheduler place one of them, and the other is the surplus -- which the
// group has to explain, because a pod that exists, never runs, and never will
// is otherwise indistinguishable from one that is merely slow.
func theHostPortGroupBindsThePortAndHasNoService(t *testing.T) {
	eventually(t, 2*time.Minute, "a gateway-host pod carrying the host port", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx, &pods, client.InNamespace(testNamespace),
			client.MatchingLabels{"spawnery.cloud/group": "gateway-host"}); err != nil {
			return false, err.Error()
		}
		if len(pods.Items) == 0 {
			return false, "no pods yet"
		}
		for _, p := range pods.Items {
			for _, port := range p.Spec.Containers[0].Ports {
				if port.Name == "minecraft" && port.HostPort == 25565 {
					return true, ""
				}
			}
		}
		return false, fmt.Sprintf("%d pod(s), none binding 25565", len(pods.Items))
	})

	var svc corev1.Service
	err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-host"}, &svc)
	if !apierrors.IsNotFound(err) {
		t.Errorf("a HostPort group has a Service (err = %v). Nothing in the cluster "+
			"dials a proxy, so the object has no consumer and its node port is "+
			"held for nobody", err)
	}

	eventually(t, 3*time.Minute, "the group to report the pod it cannot place", func() (bool, string) {
		var group spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-host"}, &group); err != nil {
			return false, err.Error()
		}
		cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
		if cond == nil {
			return false, "no Degraded condition"
		}
		if cond.Status != metav1.ConditionTrue {
			return false, "Degraded is " + string(cond.Status) + "/" + cond.Reason
		}
		if cond.Reason != spawneryv1alpha1.ReasonProxyPodUnschedulable {
			return false, "reason is " + cond.Reason
		}
		return true, "message: " + cond.Message
	})
}

// aSwitchToHostPortRemovesTheService is the only place services: delete is
// exercised under the operator's own ServiceAccount. Everything else about
// that permission is a table entry and a generated role.
func aSwitchToHostPortRemovesTheService(t *testing.T) {
	eventually(t, 2*time.Minute, "the gateway-switch Service", func() (bool, string) {
		var svc corev1.Service
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-switch"}, &svc); err != nil {
			return false, err.Error()
		}
		if svc.Spec.Type != corev1.ServiceTypeNodePort {
			return false, "type is " + string(svc.Spec.Type)
		}
		return true, ""
	})

	var group spawneryv1alpha1.ProxyGroup
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-switch"}, &group); err != nil {
		t.Fatalf("get gateway-switch: %v", err)
	}
	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:     spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25566},
	}
	if err := k8s.Update(ctx, &group); err != nil {
		t.Fatalf("switch gateway-switch to HostPort: %v", err)
	}

	eventually(t, 2*time.Minute, "the Service to be removed", func() (bool, string) {
		var svc corev1.Service
		err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-switch"}, &svc)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "the Service is still there, holding node port " +
			fmt.Sprint(svc.Spec.Ports[0].NodePort)
	})

	eventually(t, 3*time.Minute, "a pod carrying the new host port", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx, &pods, client.InNamespace(testNamespace),
			client.MatchingLabels{"spawnery.cloud/group": "gateway-switch"}); err != nil {
			return false, err.Error()
		}
		for _, p := range pods.Items {
			for _, port := range p.Spec.Containers[0].Ports {
				if port.Name == "minecraft" && port.HostPort == 25566 {
					return true, ""
				}
			}
		}
		return false, fmt.Sprintf("%d pod(s), none binding 25566", len(pods.Items))
	})
}

// aForbiddenHostPortIsReportedOnTheGroup is the one refusal this repository
// has ever observed being enforced.
//
// Everything milestone 6b shipped is an object whose effect no run here can
// see: kindnet enforces no NetworkPolicy, so a correct policy and a wholly
// broken one produce the same green. This is different in kind. Pod Security
// baseline disallows host ports, the API server enforces it, and the pod
// genuinely does not come into existence. It is also the reason
// theOperatorWasNeverDenied excludes `violates PodSecurity`: this scenario
// puts an `is forbidden:` line in the operator's log on purpose.
func aForbiddenHostPortIsReportedOnTheGroup(t *testing.T) {
	const ns = "minecraft-baseline"

	eventually(t, 3*time.Minute, "the group to carry the API server's refusal", func() (bool, string) {
		var group spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "gateway-forbidden"}, &group); err != nil {
			return false, err.Error()
		}
		cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
		if cond == nil {
			return false, "no Degraded condition"
		}
		if cond.Status != metav1.ConditionTrue || cond.Reason != spawneryv1alpha1.ReasonProxyPodRejected {
			return false, "Degraded is " + string(cond.Status) + "/" + cond.Reason
		}
		if !strings.Contains(cond.Message, "PodSecurity") {
			return false, "message does not name PodSecurity: " + cond.Message
		}
		return true, ""
	})

	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(ns),
		client.MatchingLabels{"spawnery.cloud/group": "gateway-forbidden"}); err != nil {
		t.Fatalf("list the group's pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("%d pod(s) exist in a namespace that forbids host ports. Either the "+
			"label is not on the namespace or the pod does not carry the port, and "+
			"in both cases this scenario has been measuring nothing", len(pods.Items))
	}
}
```

Import whatever these need, following `test/e2e/e2e_test.go`'s import block:
`fmt`, `strings`, `testing`, `time`, `corev1`, `metav1`,
`apierrors "k8s.io/apimachinery/pkg/api/errors"`,
`"k8s.io/apimachinery/pkg/api/meta"`,
`"sigs.k8s.io/controller-runtime/pkg/client"`, and
`spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"`.

**Check the group label key before you rely on it.** The code above uses
`spawnery.cloud/group`; read `internal/podspec/labels.go` and use the constant
`podspec.LabelGroup`'s actual value. If `test/e2e` already imports `podspec`
elsewhere, import it and use the constant rather than a literal.

- [ ] **Step 5: Register the scenarios**

In `test/e2e/e2e_test.go`, insert four lines into
`TestSpawneryUnderItsOwnServiceAccount`'s list, after
`t.Run("the proxy group gets its Service", theProxyGroupGetsItsService)` and
before the operator/RBAC/policy scenarios:

```go
	t.Run("the LoadBalancer group gets its Service", theLoadBalancerGroupGetsItsService)
	t.Run("the HostPort group binds the port and has no Service", theHostPortGroupBindsThePortAndHasNoService)
	t.Run("a switch to HostPort removes the Service", aSwitchToHostPortRemovesTheService)
	t.Run("a forbidden host port is reported on the group", aForbiddenHostPortIsReportedOnTheGroup)
```

`theOperatorWasNeverDenied` stays last.

- [ ] **Step 6: Run the driven suite**

```
E2E_KEEP=1 nix develop -c make e2e
```

Expected: eighteen scenarios, all passing. Report the actual scenario count
and the run's wall-clock time.

If `aSwitchToHostPortRemovesTheService`'s last assertion times out, inspect
the group with `kubectl -n minecraft describe proxygroup gateway-switch` and
the pods with `kubectl -n minecraft get pods -l spawnery.cloud/group=gateway-switch -o yaml`
against the kept cluster, and report what the rollout actually did rather
than lengthening the deadline.

- [ ] **Step 7: Mutate, and report what happened**

These are cluster-level mutations; each needs its own `make e2e` run. Budget
the time.

1. Remove `pod-security.kubernetes.io/enforce=baseline` from the `kubectl
   label` line in `hack/e2e.sh`. Expected:
   `aForbiddenHostPortIsReportedOnTheGroup` fails — no refusal, and pods
   exist. **Report whether it fails on the condition, on the pod count, or on
   both.** If it fails on neither, the scenario is measuring nothing.
2. Revert 1, then restore `theOperatorWasNeverDenied`'s check to the
   un-narrowed `is forbidden:`. Expected: it fails, listing the PodSecurity
   line. This is the collision the design predicted; confirm it is real and
   that the narrowed form is what avoids it.
3. Revert 2, then change `gateway-host`'s replicas from 2 to 1 in the
   manifest. Expected: `theHostPortGroupBindsThePortAndHasNoService` times out
   waiting for the Degraded condition, since there is no surplus pod.

- [ ] **Step 8: Commit**

```
nix develop -c make test
git add -A
git commit
```

Subject: `test(6c): the driven run gets four scenarios and one real refusal`.
The body should say which of the four proves something enforced (one) and
what the other three prove (objects), and name the narrowed denial check
with its reason.

---

### Task 7: The record

**Files:**
- Modify: `README.md`, `docs/known-issues.md`, `config/samples/network.yaml`
- Create: `docs/handover-milestone-6c.md`

**Interfaces:**
- Consumes: everything. This task is written last because it describes what
  the run actually did, not what the plan expected.

- [ ] **Step 1: Correct what the removed refusal falsified**

`docs/known-issues.md` names the refusal in the present tense in two entries,
around lines 1485 and 1605 — grep for `ExposeNotImplemented` and for
`expose.type` to find them exactly. Rewrite both to the present tense of what
is now true: all three strategies are implemented, and the reason survives
only as a guard for an enum value the controller does not know.

`README.md`'s roadmap paragraph says milestone 6 continues with 6c, the
`LoadBalancer` and `HostPort` expose strategies. Update it to name 6d (the
Helm chart) and 6e (CI) as what remains, and point a reader starting 6d at
`docs/handover-milestone-6c.md`. Follow the shape of the paragraph that
currently points at `docs/handover-milestone-6b.md`.

**Do not retro-correct plan documents.** `docs/superpowers/plans/` is a
historical record of what was planned; specs and `known-issues.md` are the
living references. This is the rule milestone 6b arrived at
(`d63cfa7`) and it holds here.

- [ ] **Step 2: Extend the sample**

`config/samples/network.yaml` carries one `ProxyGroup` with
`expose.type: NodePort` around line 69. Add the other two strategies as
commented alternatives immediately below it, in the sample's existing voice —
it is a realistic starting point for a user, so the comments should say what
each strategy costs, not merely what it is:

- `LoadBalancer` needs a controller the cluster does not ship with on bare
  metal (MetalLB, kube-vip), and `externalTrafficPolicy` defaults to `Local`
  so the player's real IP survives.
- `HostPort` binds the port on every node running a proxy, so replicas are
  capped by the node count, and Pod Security `baseline` and `restricted` both
  disallow it — a group in such a namespace never gets a pod, and says so on
  its `Degraded` condition.

- [ ] **Step 3: Record the finding that has no home yet**

Add an entry to `docs/known-issues.md` for the conflict the design's §10
names: the RKE2 rollout at the end of milestone 6 is promised both CIS
`restricted` pod security and `HostPort` under the cluster's real CNI, and
Pod Security `baseline` — which `restricted` inherits — disallows host ports,
so the two cannot hold in one namespace. Say what the remedy is (a namespace
with a relaxed policy for the `HostPort` leg, or dropping one of the two
requirements) and that it is the runbook's to take, not the code's.

Follow the file's existing entry format; read two neighbouring entries first.

- [ ] **Step 4: Write the handover**

Create `docs/handover-milestone-6c.md` in the form of
`docs/handover-milestone-6b.md`: written for someone with no memory of how
any of this was built, starting 6d. It must cover, in the milestone's own
plain terms:

- **What was driven, and what only exists.** Name every claim by its
  evidence: the enforced Pod Security refusal is the one thing observed being
  enforced; the `LoadBalancer` address path is envtest only; the E2E's
  ingress entry was written by the test.
- **What 6d finds in place** — read off the code as 6c leaves it, not off
  this plan. In particular: `config/deploy/networkpolicy.yaml` still
  hard-codes `spawnery-system`, and the `+kubebuilder:rbac` markers for the
  TLS Secret (`internal/certs/store.go`) and the leases
  (`internal/controller/setup.go`) still carry
  `namespace=spawnery-system` as a literal. 6a's and 6b's handovers both call
  this the single most likely way the chart ships something that works on the
  author's machine and nowhere else. Restate it with current line numbers.
- **What the RKE2 rollout now owes**, including the `HostPort`-versus-CIS
  conflict from Step 3.
- **Every finding this milestone's reviews produced**, with what caught each
  one. If the pattern holds — findings coming from mutation and not from
  reading — say so with the count, the way 6b's handover does.

- [ ] **Step 5: Verify the documents against the code**

For every file path, line number, function name and constant you wrote into
the handover and the known-issues entry, grep for it. A handover is read by
someone who cannot check your work cheaply, and a wrong line number costs
them more than a missing one.

```
nix develop -c make test
```

- [ ] **Step 6: Commit**

```
git add -A
git commit
```

Subject: `docs(6c): what was driven, what only exists, and what 6d inherits`.

---

## Notes for the executor

**On the review loop.** This project's own record, across milestones 5 and
6b, is that thirteen findings came from mutation and none from reading, and
that they sat in test code the plan specified verbatim — including this
plan's author's. Treat every code block above as a proposal that has not been
run. When a mutation you were told to expect to fail does not fail, that is
the finding, and it outranks finishing the task on time.

**On the E2E's limits.** No image in `test/e2e/manifests/e2e.yaml` resolves,
by decision. No container process ever runs, nothing listens on 25565 or 8081
in a game namespace, and kindnet enforces no NetworkPolicy. Every scenario in
Task 6 is written to hold under all three. If you find yourself wanting a
scenario that needs a running process, stop and say so rather than making one
resolve.

**On what may be claimed.** The Global Constraints forbid claiming
reachability. That is not a stylistic preference: milestone 6b shipped a
security control and could not observe it doing anything, and the whole
handover chain now depends on each milestone stating exactly what it
measured. One enforced refusal, observed, is worth more than four
confidently-named tests.
