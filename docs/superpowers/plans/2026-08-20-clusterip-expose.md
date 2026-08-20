# ClusterIP Expose Strategy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a fourth `ProxyGroup.spec.expose` strategy, `ClusterIP`, for a
network something else publishes — an ingress controller, a gateway, a tunnel.

**Architecture:** A new value in the `ExposeType` enum with a required
`clusterIP.address` sub-block, admitted by `exposeImplemented`, served by an
explicit `case` in `reconcileService` (a ClusterIP Service, no node port, no
external traffic policy) and one in `proxyAddress` (the configured address,
echoed). The `default:` arms of both switches stop meaning NodePort and start
returning an error naming the unknown type.

**Tech Stack:** Go, controller-runtime, kubebuilder CRD markers with CEL
validation, envtest for admission, `hack/e2e.sh` with kind for the end-to-end
scenario.

## Global Constraints

- The spec is `docs/superpowers/specs/2026-08-20-clusterip-expose-design.md`.
  Where this plan and the spec disagree, stop and ask — do not pick one.
- Every command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`.
- Commits follow Conventional Commits with English subjects, and every commit
  ends with the two trailers this repository's history carries:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **Never push `master`, never merge, and never push a tag.**
- The strategy's name is exactly `ClusterIP`; the sub-block is `clusterIP`; its
  one field is `address`, and it is required.
- The address takes no port by default: `mc.paul.wtf` is a complete address
  because Minecraft clients default to 25565.
- `make manifests` regenerates `config/crd/bases/` **and**
  `charts/spawnery/templates/crds.yaml` through `hack/chart-templates.sh`. Both
  belong in the same commit as the API change; CI's `deps`/drift guard fails
  otherwise.
- **A test that passes the moment it is written has proven nothing.** Every
  task states what its test must fail with first, and the failure is recorded
  in the commit.

---

## File Structure

| File | Change |
|---|---|
| `api/v1alpha1/proxygroup_types.go` | `ExposeClusterIP` const, `ClusterIPSpec`, the `ExposeSpec.ClusterIP` field, enum marker, two CEL rules |
| `config/crd/bases/spawnery.cloud_proxygroups.yaml` | generated |
| `charts/spawnery/templates/crds.yaml` | generated |
| `api/v1alpha1/zz_generated.deepcopy.go` | generated |
| `internal/controller/proxygroup_controller.go` | `exposeImplemented`, `reconcileService`, `proxyAddress` |
| `internal/controller/expose_test.go` | the new cases and the transition tests |
| `api/v1alpha1/proxygroup_envtest_test.go` | the CEL cases, as rows in the table that already asks |
| `test/e2e/manifests/e2e.yaml` | a fourth `ProxyGroup` |
| `test/e2e/expose_test.go` | the end-to-end scenario |
| `config/samples/network.yaml` | the strategy as a documented alternative |
| `docs/known-issues.md` | the gap entry replaced by what now exists |

---

## Task 1: The API, and the test that already guards it

**Files:**
- Modify: `api/v1alpha1/proxygroup_types.go`
- Modify: `internal/controller/expose_test.go:412-430`
- Modify: `internal/controller/proxygroup_controller.go:1765-1774`
- Generated: `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`

**Interfaces:**
- Produces: `spawneryv1alpha1.ExposeClusterIP` (an `ExposeType` whose value is
  `"ClusterIP"`), `spawneryv1alpha1.ClusterIPSpec` with field
  `Address string`, and `ExposeSpec.ClusterIP *ClusterIPSpec`. Tasks 2 and 3
  use all three.

- [ ] **Step 1: Watch the existing guard fail**

Add the enum value and nothing else. In `api/v1alpha1/proxygroup_types.go`,
extend the marker above `ExposeType`:

```go
// +kubebuilder:validation:Enum=LoadBalancer;NodePort;HostPort;ClusterIP
```

and add the constant beside the other three:

```go
	// ExposeClusterIP is for a network something else publishes: an ingress
	// controller, a gateway, a tunnel. The operator creates the Service that
	// thing routes to, and nothing else. spec.expose.clusterIP.address says
	// where players connect, because the operator cannot learn it.
	ExposeClusterIP ExposeType = "ClusterIP"
```

- [ ] **Step 2: Run the guard and read what it says**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestExposeImplementedCoversTheEnumAndNothingElse -v`

Expected: **PASS**, and that is the point of running it. The test enumerates
known values by hand, so a value it has not been told about slips past. It
catches a strategy the *operator* refuses, not one the *test* has not learned —
which means nothing fires on its own when the enum grows, and the loop is
closed by hand in step 3. The spec's §3 was wrong about this twice before
settling on it; running the test here is how the plan avoids inheriting the
error.

- [ ] **Step 3: Add the new value to the guard's known list, and watch it fail**

In `internal/controller/expose_test.go`, add to the first loop of
`TestExposeImplementedCoversTheEnumAndNothingElse`:

```go
		spawneryv1alpha1.ExposeClusterIP,
```

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestExposeImplementedCoversTheEnumAndNothingElse -v`

Expected: **FAIL**, with

```
ClusterIP is in the CRD's enum, so a user can create a group asking for it,
and this operator refuses it
```

That message is the whole point: the CRD would accept the object and the
operator would refuse it.

- [ ] **Step 4: Teach `exposeImplemented` the strategy**

In `internal/controller/proxygroup_controller.go`:

```go
func exposeImplemented(t spawneryv1alpha1.ExposeType) bool {
	switch t {
	case spawneryv1alpha1.ExposeNodePort,
		spawneryv1alpha1.ExposeLoadBalancer,
		spawneryv1alpha1.ExposeHostPort,
		spawneryv1alpha1.ExposeClusterIP:
		return true
	default:
		return false
	}
}
```

Run the same test. Expected: PASS.

- [ ] **Step 5: Add the sub-block and its validation**

In `api/v1alpha1/proxygroup_types.go`, beside `HostPortSpec`:

```go
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
```

Add the field to `ExposeSpec`, after `HostPort`:

```go
	// ClusterIP configures type ClusterIP.
	// +optional
	ClusterIP *ClusterIPSpec `json:"clusterIP,omitempty"`
```

and two rules to `ExposeSpec`'s existing `XValidation` block, in the same
shape as the three pairs already there:

```go
// +kubebuilder:validation:XValidation:rule="self.type != 'ClusterIP' || has(self.clusterIP)",message="expose.clusterIP is required for type ClusterIP"
// +kubebuilder:validation:XValidation:rule="self.type == 'ClusterIP' || !has(self.clusterIP)",message="expose.clusterIP is only allowed for type ClusterIP"
```

- [ ] **Step 6: Regenerate and check what moved**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make manifests generate
git status --short
```

Expected: `config/crd/bases/spawnery.cloud_proxygroups.yaml`,
`charts/spawnery/templates/crds.yaml` and
`api/v1alpha1/zz_generated.deepcopy.go` all modified. If the chart template did
not move, stop: `hack/chart-templates.sh` is not running, and CI's drift guard
will fail on a tree that looks fine locally.

- [ ] **Step 7: Confirm the CRD carries the enum value and the rules**

```bash
grep -c 'ClusterIP' config/crd/bases/spawnery.cloud_proxygroups.yaml
grep -A2 'clusterIP is required for type' config/crd/bases/spawnery.cloud_proxygroups.yaml
```

Expected: a non-zero count, and the message present. Read the generated file
rather than trusting the generator: this repository has twice shipped a
generator whose output nobody looked at.

- [ ] **Step 8: Run the package's tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ ./api/...`

Expected: PASS. `reconcileService` still has no `ClusterIP` arm, but nothing
constructs such a group yet.

- [ ] **Step 9: Commit**

```bash
git add api/v1alpha1 config/crd charts/spawnery/templates/crds.yaml internal/controller
git commit -m "$(cat <<'EOF'
feat(expose): the ClusterIP strategy's API

A fourth value in the ExposeType enum, a required clusterIP.address, and the
two validation rules every other strategy's sub-block already has.

exposeImplemented learns the value in the same commit, because until it does
the CRD accepts an object the operator refuses -- which is exactly what
TestExposeImplementedCoversTheEnumAndNothingElse says when the value is added
to its known list. Worth recording: that test did *not* fail on the enum
change alone. It enumerates by hand, so it catches a strategy the operator has
not implemented, not one the test has not been told about. The plan expected
otherwise and was wrong.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 2: The Service, and the panic that proves the arm is needed

**Files:**
- Modify: `internal/controller/proxygroup_controller.go:1288-1310` (the switch in `reconcileService`)
- Modify: `internal/controller/expose_test.go`

**Interfaces:**
- Consumes: `ExposeClusterIP`, `ClusterIPSpec`, `ExposeSpec.ClusterIP` from Task 1.
- Produces: a `ClusterIP` group reconciles to a Service named after the group
  with `spec.type: ClusterIP`, one port named `minecraft` on 25565, no
  `nodePort`, and no `externalTrafficPolicy`.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/expose_test.go`:

```go
// A ClusterIP group gets a Service the thing in front of it can route to, and
// nothing that reaches outside the cluster on its own. The absence of a node
// port is half the point: the strategy exists because the NodePort workaround
// left one allocated that nobody dialled and a firewall had to cover.
func TestClusterIPServiceShape(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:      spawneryv1alpha1.ExposeClusterIP,
			ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
		}
	})

	svc, err := r.reconcileService(f.ctx, group)
	if err != nil {
		t.Fatalf("reconcileService: %v", err)
	}
	if svc == nil {
		t.Fatal("no Service was created; a ClusterIP group is fronted by something that needs one to route to")
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %s, want ClusterIP", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("externalTrafficPolicy = %q, want empty: the field is meaningless on a "+
			"ClusterIP Service and the API server rejects it", svc.Spec.ExternalTrafficPolicy)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("got %d ports, want exactly one", len(svc.Spec.Ports))
	}
	if got := svc.Spec.Ports[0].NodePort; got != 0 {
		t.Errorf("nodePort = %d, want 0: the strategy exists so that no node port is "+
			"allocated for a group nobody dials on a node", got)
	}
	if got := svc.Spec.Ports[0].Port; got != podspec.MinecraftPort {
		t.Errorf("port = %d, want %d", got, podspec.MinecraftPort)
	}
	if len(svc.Annotations) != 0 {
		t.Errorf("annotations = %v, want none: nothing reads annotations on a ClusterIP "+
			"Service that could change where traffic goes, and external-dns with "+
			"--publish-internal-services would publish the ClusterIP itself", svc.Annotations)
	}
}
```

The helpers are the package's own: `newFixture(t)` from `suite_test.go:194`,
`proxyGroupReconciler(f)` and `f.createProxyGroup(name, mutate...)` from
`proxygroup_controller_test.go:95` and `:119`, and `f.ctx`, `f.c`, `f.ns`.
`createProxyGroup` builds a NodePort group and applies the mutators, so the
whole `Expose` block is replaced above. `TestLoadBalancerServiceShape`
(`expose_test.go:42`) is the shape to follow.

- [ ] **Step 2: Run it and record the failure**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestClusterIPServiceShape -v`

Expected: **a nil-pointer panic**, not an assertion failure —
`reconcileService`'s `default:` arm reads `group.Spec.Expose.NodePort.Port`,
and a `ClusterIP` group has no `NodePort` block. Paste the panic into the
commit message in step 5. This is the failure mode the spec's §3 names, and
seeing it is what proves the `case` arm is the fix rather than decoration.

- [ ] **Step 3: Give the switch explicit arms**

In `reconcileService`, replace the `default:` arm with named cases:

```go
		switch group.Spec.Expose.Type {
		case spawneryv1alpha1.ExposeLoadBalancer:
			svc.Spec.Type = corev1.ServiceTypeLoadBalancer
			svc.Spec.ExternalTrafficPolicy = loadBalancerTrafficPolicy(group)
			// No node port is named. A LoadBalancer Service gets one anyway,
			// allocated by the API server, and naming one here would add a
			// second way for two groups in different namespaces to collide
			// over a number no player ever dials.
		case spawneryv1alpha1.ExposeClusterIP:
			// No external traffic policy: the field is meaningless on a
			// ClusterIP Service and the API server rejects it. No node port
			// either, which is the whole reason this strategy exists -- the
			// NodePort workaround it replaces left one allocated that nobody
			// dialled and a host firewall had to account for.
			svc.Spec.Type = corev1.ServiceTypeClusterIP
		case spawneryv1alpha1.ExposeNodePort:
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
			svc.Spec.Type = corev1.ServiceTypeNodePort
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
			port.NodePort = group.Spec.Expose.NodePort.Port
		default:
			// Unreachable while exposeImplemented and this switch agree, and
			// written out because that is exactly the assumption a fifth
			// strategy breaks: whoever adds one and updates only one of the
			// two gets a named error here instead of the nil dereference the
			// old default: arm produced by reading NodePort.Port.
			return fmt.Errorf("expose.type %s reached reconcileService without a branch",
				group.Spec.Expose.Type)
		}
```

`HostPort` is handled by the early return at the top of the function and needs
no arm here; leave that return as it is.

- [ ] **Step 4: Run it**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestClusterIPServiceShape -v`

Expected: PASS.

Then the whole package:

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/`

Expected: PASS. `TestReconcileAcceptsEveryStrategy` still covers three
strategies; Task 4 extends it.

- [ ] **Step 5: Prove the erroring `default:` can fire**

It is unreachable while `exposeImplemented` and the switch agree, so the only
way to see it work is to make them disagree. Temporarily add an invented type
to `exposeImplemented`:

```go
	case spawneryv1alpha1.ExposeNodePort,
		spawneryv1alpha1.ExposeLoadBalancer,
		spawneryv1alpha1.ExposeHostPort,
		spawneryv1alpha1.ExposeClusterIP,
		spawneryv1alpha1.ExposeType("Anycast"):
```

and write a throwaway test that reconciles a group whose `Expose.Type` is
`"Anycast"` — constructed in Go rather than created through the API server,
since the CRD's enum refuses it:

```go
func TestTemporaryUnknownStrategyErrors(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway")
	group.Spec.Expose.Type = spawneryv1alpha1.ExposeType("Anycast")

	_, err := r.reconcileService(f.ctx, group)
	if err == nil {
		t.Fatal("no error for a strategy with no branch")
	}
	if !strings.Contains(err.Error(), "Anycast") {
		t.Errorf("error = %v, want it to name the type", err)
	}
}
```

Run it. Expected: PASS, with the error naming `Anycast`. Then **remove both
the case and the test**, and confirm:

```bash
git diff --stat internal/controller
```

Expected: only the changes this task is meant to make. A mutation left behind
is a mutation that ships — this repository has a runbook section about exactly
that.

- [ ] **Step 6: Commit**

```bash
git add internal/controller
git commit -m "$(cat <<'EOF'
feat(expose): reconcileService serves ClusterIP, and names what it cannot

The failing test did not assert -- it panicked. The default: arm read
group.Spec.Expose.NodePort.Port, which a ClusterIP group does not have, so the
arm is what fixes a nil dereference rather than what tidies a switch:

  <paste the panic here>

Every strategy now has a named case and default: returns an error instead. It
is unreachable while exposeImplemented and this switch agree, which is the
point: whoever adds a fifth strategy and updates one of the two gets a message
naming the type rather than a panic three frames down.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 3: The published address

**Files:**
- Modify: `internal/controller/proxygroup_controller.go:1720-1745` (the switch in `proxyAddress`)
- Modify: `internal/controller/expose_test.go:185-300` (`TestProxyAddressPerStrategy`)

**Interfaces:**
- Consumes: everything from Tasks 1 and 2.
- Produces: `proxyAddress` returns `Expose.ClusterIP.Address` verbatim for a
  `ClusterIP` group with a ready pod, and `""` when no pod is ready.

- [ ] **Step 1: Add two rows to the table test**

`TestProxyAddressPerStrategy` (`expose_test.go:185`) is a table over
`{name, expose, pods, svc, want}` with `readyPod()` and `notReadyPod()`
helpers already defined. Add:

```go
		{
			name: "ClusterIP publishes the configured address",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeClusterIP,
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
			},
			pods: []corev1.Pod{readyPod()},
			want: "mc.example.test",
		},
		{
			// The address is a static string that needs no pod to compute, and
			// it is still withheld until one is ready. The column means the
			// same thing for all four strategies: you can connect here now.
			name: "ClusterIP publishes nothing until a proxy is ready",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeClusterIP,
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
			},
			pods: []corev1.Pod{notReadyPod()},
			want: "",
		},
```

Match the struct's actual field names as the file has them; if a row needs an
`svc` field, pass `nil`.

- [ ] **Step 2: Run and record the failure**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestProxyAddressPerStrategy -v`

Expected: **a plain value mismatch** on the first new row —
`proxyAddress = "", want "mc.example.test"` — and a PASS on the second.

An earlier version of this step predicted a nil-pointer panic here, by analogy
with Task 2. That was wrong, and the guard it overlooked was one the plan's
author had already read: `proxyAddress`'s `default:` arm opens with
`if group.Spec.Expose.NodePort == nil { return "" }`, which
`reconcileService`'s did not. A ClusterIP group falls through it to `""`
instead of dereferencing nil.

Both outcomes still carry their intent: the first row proves the arm is
missing, the second proves the ready-pod gate sits in front of the switch. A
test that fails for a plain reason is worth as much as one that panics.

- [ ] **Step 3: Give that switch explicit arms too**

In `proxyAddress`:

```go
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
	case spawneryv1alpha1.ExposeClusterIP:
		// Echoed, not composed: no port is appended, because a Minecraft
		// client defaults to 25565 and "mc.example.test" is the whole of what
		// a player types. An operator who needs another port writes it in.
		if group.Spec.Expose.ClusterIP == nil {
			return ""
		}
		return group.Spec.Expose.ClusterIP.Address
	case spawneryv1alpha1.ExposeNodePort:
		if group.Spec.Expose.NodePort == nil {
			return ""
		}
		return net.JoinHostPort(hostIP, port(group.Spec.Expose.NodePort.Port))
	default:
		// See reconcileService's default: for why this is written out rather
		// than folded into the NodePort arm.
		return ""
	}
```

`proxyAddress` returns a string and cannot report an error, so its `default:`
returns `""` — a group whose type nothing serves publishes no address, which
is the honest answer and matches every other "cannot say" path in this
function.

- [ ] **Step 4: Run it**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestProxyAddressPerStrategy -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller
git commit -m "$(cat <<'EOF'
feat(expose): proxyAddress publishes the configured address

Echoed rather than composed: no port is appended, because a Minecraft client
defaults to 25565 and the configured string is the whole of what a player
types.

The ready-pod gate applies here too, and the table test says why in the row
that asserts it: the address needs no pod to compute, and is still withheld
until one is ready, so the column means the same thing for all four
strategies.

default: returns "" rather than an error -- the function has no way to report
one, and "cannot say" is already how every other unanswerable path here reads.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 4: Admission, against a real API server

**Files:**
- Modify: `api/v1alpha1/proxygroup_envtest_test.go:44` (`TestProxyGroupExposeValidation`)

**Interfaces:**
- Consumes: the CRD generated in Task 1.
- Produces: nothing other tasks use.

**Corrected after Task 1's review.** An earlier version of this task created
`internal/controller/expose_admission_test.go`. It should not:
`TestProxyGroupExposeValidation` is an existing table over exactly this
question — a `ProxyGroup` submitted to a real API server, `wantErr` per row —
and it lives in `api/v1alpha1`, the package the markers themselves are in. Two
files asking the same question in two packages is one file too many. The
repository owner ruled that the table is extended.

The table's rows are `{name string, expose spawneryv1alpha1.ExposeSpec, wantErr bool}`
and the client comes from `testenv.Client(t)`. It has no message assertion
today; this task adds one, because "refused" is not the same claim as "refused
for the stated reason" — a rule that rejects everything would satisfy `wantErr`
on every row.

- [ ] **Step 1: Give the table a message to check**

Add a field to the case struct and assert it where the error is checked:

```go
	cases := []struct {
		name    string
		expose  spawneryv1alpha1.ExposeSpec
		wantErr bool
		// wantMsg is a substring of the refusal. Empty means the row only
		// claims that the API server refused, which is the weaker claim the
		// rows written before CEL messages were worth asserting still make.
		wantMsg string
	}{
```

and in the body, after the existing `wantErr` handling confirms an error:

```go
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantMsg)
			}
```

Read the existing body before editing it and keep its shape; if the error
variable is not called `err` there, use the name it has.

- [ ] **Step 2: Add the five ClusterIP rows**

```go
		{
			name: "clusterip with matching sub-block",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeClusterIP,
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
			},
		},
		{
			name:    "clusterip without sub-block",
			expose:  spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeClusterIP},
			wantErr: true,
			wantMsg: "expose.clusterIP is required for type ClusterIP",
		},
		{
			name: "clusterip sub-block under another type",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeNodePort,
				NodePort:  &spawneryv1alpha1.NodePortSpec{Port: 30565},
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
			},
			wantErr: true,
			wantMsg: "expose.clusterIP is only allowed for type ClusterIP",
		},
		{
			name: "an address is not a URL",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeClusterIP,
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "tcp://mc.example.test"},
			},
			wantErr: true,
			wantMsg: "not a URL",
		},
		{
			name: "an address carries no spaces",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeClusterIP,
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test "},
			},
			wantErr: true,
			wantMsg: "no spaces",
		},
		{
			name: "an empty address is refused",
			expose: spawneryv1alpha1.ExposeSpec{
				Type:      spawneryv1alpha1.ExposeClusterIP,
				ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: ""},
			},
			wantErr: true,
			wantMsg: "should be at least 1 chars long",
		},
```

The first row is the one that keeps the other five honest: five refusals prove
nothing about a rule if no shape gets through.

- [ ] **Step 3: Run it**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./api/... -run TestProxyGroupExposeValidation -v`

Not `make test ARGS=...`: `$(ARGS)` appears nowhere in the Makefile, whose
`test` target always runs `go test -race ./...`. The `ARGS` form was this
plan's invention and it silently runs the whole suite.

Expected: PASS. **These rows were written after the markers they check, so they
are regression tests and not design pressure** — say so in the commit rather
than implying they drove anything.

- [ ] **Step 4: Show they can fail**

Delete the `contains('://')` half of the CEL rule in
`api/v1alpha1/proxygroup_types.go`, regenerate, re-run:

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make manifests
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./api/... -run TestProxyGroupExposeValidation -v
```

Expected: the "not a URL" row fails. Restore the rule, regenerate again, and
confirm the tree is clean:

```bash
git diff --stat api/v1alpha1 config/crd charts/spawnery/templates/crds.yaml
```

Expected: empty for the generated files. A mutation that is not restored is a
mutation that ships.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/proxygroup_envtest_test.go
git commit -m "$(cat <<'EOF'
test(expose): the ClusterIP admission rows, in the table that already asks

TestProxyGroupExposeValidation submits a ProxyGroup to a real API server and
records whether it was refused, which is exactly the question the ClusterIP
rules raise -- so the rows go there rather than into a second file in another
package.

The table gained a wantMsg field along the way. "Refused" and "refused for the
stated reason" are different claims, and a rule that rejected everything would
have satisfied every wantErr in the table as it stood.

These rows were written after the markers they check: they are regression
tests, not design pressure. That they can fail was shown by deleting the
contains('://') half of the rule, regenerating, and watching the URL row go
red; the deletion was restored and the generated files checked back to clean.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 5: The transitions

**Files:**
- Modify: `internal/controller/expose_test.go`

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: nothing other tasks use.

The spec's §5 names three transitions. Each is asserted against the Service
**read back from the client**, not against the object `reconcileService`
returned — the difference is what caught three defects in this repository
already.

- [ ] **Step 1: Write the three tests**

**Corrected after Task 2's fix round.** Task 2 already produced a test over the
`NodePort` → `ClusterIP` transition —
`TestTheAPIServerClearsExternalTrafficPolicyOnTheMoveToClusterIP` — after a
review finding about `ExternalTrafficPolicy` turned out not to reproduce: the
API server normalises the field away on the type change, which the test now
holds it to. **Do not write a second test over the same transition.** Extend
that one with the node-port assertion instead, so one place answers "what does
the API server do when this group changes strategy":

```go
	// ... after the existing type and ExternalTrafficPolicy assertions:
	if got := stored.Spec.Ports[0].NodePort; got != 0 {
		t.Errorf("nodePort = %d, want 0: it was allocated under the previous strategy "+
			"and nothing dials it now. Unlike the traffic policy above, this is not the "+
			"API server's doing -- reconcileService rebuilds svc.Spec.Ports from a fresh "+
			"literal on every pass, so the operator sends 0 itself. A red here means that "+
			"reconstruction changed", got)
	}
```

Widen its doc comment to cover both fields **and to state their asymmetry**,
which an earlier version of this step got wrong by asserting a parity that does
not exist:

- `ExternalTrafficPolicy` sits on `svc.Spec`, which `CreateOrUpdate` fetched
  and the `ClusterIP` arm never reassigns — so a stale `Local` goes out and the
  API server strips it. That half guards the API server.
- `NodePort` sits on the port, and `reconcileService` rebuilds
  `svc.Spec.Ports` from a fresh literal on every pass
  (`proxygroup_controller.go:1282-1287,1326`) — so the operator sends `0`
  itself and nothing is carried forward. That half guards the operator's own
  port reconstruction.

Rename the test if its name no longer reads true; the earlier name credited
the API server with both.

```go
// LoadBalancer -> ClusterIP releases exactly the annotations the operator set
// and leaves every foreign key alone. Milestone 6c built that mechanism;
// this is the first strategy to leave LoadBalancer for something other than
// NodePort.
func TestSwitchingFromLoadBalancerToClusterIPReleasesTheAnnotations(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type: spawneryv1alpha1.ExposeLoadBalancer,
			LoadBalancer: &spawneryv1alpha1.LoadBalancerSpec{
				Annotations: map[string]string{"lbipam.cilium.io/ips": "203.0.113.5"},
			},
		}
	})
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	var svc corev1.Service
	key := client.ObjectKey{Namespace: f.ns, Name: group.Name}
	if err := f.c.Get(f.ctx, key, &svc); err != nil {
		t.Fatalf("reading the Service back: %v", err)
	}
	svc.Annotations["someone.else/key"] = "left alone"
	if err := f.c.Update(f.ctx, &svc); err != nil {
		t.Fatalf("adding a foreign annotation: %v", err)
	}

	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:      spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
	}
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var stored corev1.Service
	if err := f.c.Get(f.ctx, key, &stored); err != nil {
		t.Fatalf("reading the Service back: %v", err)
	}
	if _, still := stored.Annotations["lbipam.cilium.io/ips"]; still {
		t.Error("the operator's own annotation survived the move off LoadBalancer")
	}
	if stored.Annotations["someone.else/key"] != "left alone" {
		t.Error("a foreign annotation was removed; the operator releases only what it set")
	}
}

// HostPort -> ClusterIP has to create a Service where the HostPort strategy
// deleted one.
func TestSwitchingFromHostPortToClusterIPCreatesTheService(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{
			Type:     spawneryv1alpha1.ExposeHostPort,
			HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25565},
		}
	})
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	key := client.ObjectKey{Namespace: f.ns, Name: group.Name}
	var absent corev1.Service
	if err := f.c.Get(f.ctx, key, &absent); err == nil {
		t.Fatal("a HostPort group left a Service behind; the rest of this test proves nothing")
	}

	group.Spec.Expose = spawneryv1alpha1.ExposeSpec{
		Type:      spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "mc.example.test"},
	}
	if _, err := r.reconcileService(f.ctx, group); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var stored corev1.Service
	if err := f.c.Get(f.ctx, key, &stored); err != nil {
		t.Fatalf("no Service after moving to ClusterIP: %v", err)
	}
	if stored.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("type = %s, want ClusterIP", stored.Spec.Type)
	}
}
```

- [ ] **Step 2: Run them**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run 'TestSwitchingFrom' -v`

Expected: all three PASS.

**If `TestSwitchingFromNodePortToClusterIPReleasesTheNodePort` fails on the
node port, that is a finding, not a broken test.** It would mean this
transition needs the port cleared explicitly, and the fix belongs in
`reconcileService`'s ClusterIP arm as `port.NodePort = 0`. Record which of the
two happened; the spec says this must be measured rather than believed, and
either outcome answers it.

This package's fixture runs against envtest, so the question is put to a real
API server and its answer is the API server's own. That is the only reason
this test can settle it.

- [ ] **Step 3: Commit**

```bash
git add internal/controller
git commit -m "$(cat <<'EOF'
test(expose): the three transitions into ClusterIP

Each asserts against the Service read back from the client rather than the
object reconcileService returned -- the difference this repository has been
caught by three times.

NodePort -> ClusterIP is the one the spec insisted on measuring rather than
believing: the documentation says the API server clears an allocated node
port on a type change, and now something checks.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 6: The end-to-end scenario

**Files:**
- Modify: `test/e2e/manifests/e2e.yaml`
- Modify: `test/e2e/expose_test.go`

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: nothing other tasks use.

- [ ] **Step 1: Add a fourth ProxyGroup to the manifest**

In `test/e2e/manifests/e2e.yaml`, in the `minecraft` namespace beside the
existing groups:

```yaml
---
# gateway-clusterip exercises the strategy for a network something else
# publishes. Nothing in this harness stands in front of it -- the point is the
# Service's shape and the published address, both of which the operator owns
# and neither of which needs a real ingress to check.
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway-clusterip
  namespace: minecraft
spec:
  networkRef:
    name: production
  replicas: 1
  image: ghcr.io/spawnery/velocity:e2e-no-such-tag
  expose:
    type: ClusterIP
    clusterIP:
      address: mc.e2e.test
  routing:
    fallbackGroups:
      - lobby
```

The image tag stays unresolvable, exactly like the groups already there: this
run is about the objects the operator creates, and
`test/e2e/manifests/e2e.yaml`'s header says why pulling 724 MB would be a
non-goal.

- [ ] **Step 2: Write the scenario**

In `test/e2e/expose_test.go`, beside `theLoadBalancerGroupGetsItsService`:

```go
// The ClusterIP group gets a Service with no way out of the cluster of its
// own, and publishes the address its operator wrote down.
func theClusterIPGroupGetsAPlainServiceAndPublishesItsAddress(t *testing.T) {
	var svc corev1.Service
	eventually(t, 2*time.Minute, "the gateway-clusterip Service", func() (bool, string) {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "gateway-clusterip"}, &svc); err != nil {
			return false, err.Error()
		}
		if svc.Spec.Type != corev1.ServiceTypeClusterIP {
			return false, "type is " + string(svc.Spec.Type)
		}
		return true, ""
	})

	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %s, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("got %d ports, want one", len(svc.Spec.Ports))
	}
	if got := svc.Spec.Ports[0].NodePort; got != 0 {
		t.Errorf("nodePort = %d, want 0", got)
	}
	if svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("externalTrafficPolicy = %q, want empty", svc.Spec.ExternalTrafficPolicy)
	}
}
```

`eventually`, `k8s`, `ctx` and `testNamespace` are the file's own, used
exactly as `theLoadBalancerGroupGetsItsService` (`test/e2e/expose_test.go:35`)
uses them. Register the scenario wherever that one is registered; do not add a
second way of fetching objects to this file.

The address is deliberately **not** asserted here: the group's pods never
become ready with an unresolvable image, so `proxyAddress` publishes nothing,
and asserting an empty string would be asserting the image tag. The ready-pod
gate is covered by Task 3's table.

- [ ] **Step 3: Run the E2E**

Run, on a machine with the container runtime this repository's
`docs/handover-milestone-6.md` §7 describes:

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix --extra-experimental-features 'nix-command flakes' develop -c \
  env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

Expected: the existing scenarios plus this one, all green. If the machine
cannot run it — 8 GB and one cluster at a time, per that same section — say so
and let CI's `e2e` job be the evidence. Do not report a scenario as driven
because it was written.

- [ ] **Step 4: Commit**

```bash
git add test/e2e
git commit -m "$(cat <<'EOF'
test(expose): an end-to-end ClusterIP group

A fourth group in the manifest, with the same unresolvable image tag as the
others, because this scenario is about the objects the operator creates.

The published address is deliberately not asserted: with no pod ever ready,
proxyAddress publishes nothing, and asserting the empty string would be
asserting the image tag rather than the strategy. The ready-pod gate has its
own row in TestProxyAddressPerStrategy.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 7: The documentation the gap left behind

**Files:**
- Modify: `config/samples/network.yaml`
- Modify: `docs/known-issues.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: everything.

- [ ] **Step 1: Show the strategy in the sample**

`config/samples/network.yaml` already carries the `LoadBalancer` alternative as
a commented block under the `gateway` group's `expose`. Add a third, in the
same style:

```yaml
    # Alternative: ClusterIP, for a network something else publishes -- an
    # ingress controller with a TCP entry point, a gateway, a tunnel. The
    # operator creates the Service that thing routes to and nothing else, and
    # address is what a player types. It is not checked: nothing here knows
    # whether the name resolves or leads anywhere.
    # type: ClusterIP
    # clusterIP:
    #   address: mc.example.com
```

- [ ] **Step 2: Replace the gap entry in known-issues**

`docs/known-issues.md`'s "From the RKE2 rollout" section opens with an entry
beginning **`ProxyGroup.spec.expose` has no strategy for "something else fronts
me".** Replace that entry with:

```markdown
**`ProxyGroup.spec.expose` gained a fourth strategy for this.** The rollout
put a network behind Traefik's TCP entryPoint and had to use `NodePort` as a
stand-in, which left a node port allocated that nobody dialled and made
`status.address` report `<node>:<nodePort>` — an address nobody plays on.
`type: ClusterIP` with a required `clusterIP.address` replaces that: the
operator creates the Service the fronting thing routes to, publishes the
address it was given, and creates no routing object and verifies no address.
See `docs/superpowers/specs/2026-08-20-clusterip-expose-design.md` §4 for why
each of those is a refusal rather than an omission.
```

- [ ] **Step 3: Say it in the README**

Find the paragraph describing milestone 6c's expose strategies and change
"all three expose strategies" to four, naming `ClusterIP` and what it is for in
one sentence. Keep the existing voice: what it does, what it costs, what it
does not establish.

- [ ] **Step 4: Run everything**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make test
nix --extra-experimental-features 'nix-command flakes' develop -c make lint
git status --short
```

Expected: both green, and no unstaged generated files — if `make manifests`
output appears now, Task 1's regeneration was incomplete and the drift guard
would have caught it in CI instead.

- [ ] **Step 5: Commit**

```bash
git add config/samples docs README.md
git commit -m "$(cat <<'EOF'
docs(expose): the fourth strategy, in the three places that promised it

known-issues.md carried this as an open gap from the RKE2 rollout; it now
carries what exists instead, including the two things the strategy refuses to
do. The sample shows it beside the LoadBalancer alternative it already
documents, and the README's milestone 6c paragraph says four rather than
three.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## What this plan does not cover

- Creating any routing object — `Ingress`, `IngressRouteTCP`, `HTTPRoute`,
  `Gateway`. Spec §4 says why that stays out of the operator.
- Validating that the address resolves or leads anywhere.
- PROXY protocol, TLS, or anything else the fronting proxy owns.
- Changing the running `paulwtf` network over to the new strategy. That is a
  separate change in the `fluxcd` repository, and it needs a released chart
  carrying the new CRD first.
