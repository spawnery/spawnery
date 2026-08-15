# Milestone 4c-3: node drain — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a node is on its way out of service, the operator empties
Spawnery's pods off it using drains that already exist, so `kubectl drain`
completes without anybody being kicked.

**Architecture:** A node is a *fact* the existing decisions consume, not a new
authority. One pure predicate answers "is this node departing". The ServerGroup
turns that into an unconditional deletion of the affected `Server` CRs — which
is already the 6.2 drain sequence. The ProxyGroup turns it into a second source
of staleness for `DecideRollout`, which already knows how to surge, replace and
drain one pod at a time. ProxyGroups also gain the `occupied` label and a
PodDisruptionBudget, without which the eviction beats the replacement.

**Tech Stack:** Go, controller-runtime, envtest, kind + podman for the evidence
run.

**Spec:** `docs/superpowers/specs/2026-08-15-node-drain-design.md` — read it
alongside this plan; the plan argues from it and does not repeat its reasoning.

## Global Constraints

- **A node is departing when `spec.unschedulable` is true, or it carries a
  taint whose key is in the configured list AND whose effect is `NoSchedule` or
  `NoExecute`.** `PreferNoSchedule` does not count — it does not stop the
  replacement landing on the same node, which would rotate forever.
- **`spec.unschedulable` is hardwired and not configurable.** The taint list is
  configurable and defaults to empty.
- **The operator reads `Node` objects and writes none.** No cordoning, no
  patching, no status updates on nodes.
- **A condemned server is deleted unconditionally:** not bounded by `Surplus`,
  not held back by `minReplicas`, and all condemned servers of a group in one
  pass.
- **`Delete` and `Condemn` never name the same server** — guaranteed by
  construction in `deletable`, not by a check after the fact.
- **Unknown player counts count as occupied.** The repository-wide rule
  (`candidates.go` `isOccupied`); it governs the new proxy `occupied` label too.
- **A PodDisruptionBudget uses `minAvailable` as an absolute number.** Never a
  percentage, never `maxUnavailable`: our pods have no controller with a scale
  subresource and Kubernetes rejects both.
- **No new `Server` status field.** `ServerGroupReconciler.collectViews` already
  resolves the pod through `podFor`; `pod.Spec.NodeName` is on that object.
- **Uncordon does not undo work already begun.** A marked proxy finishes its
  drain and replacement; only unmarked pods stop being stale.
- **Every task ends green:** `make test` passes before the commit.

---

### Task 1: The departing-node predicate

**Files:**
- Create: `internal/controller/nodes.go`
- Create: `internal/controller/nodes_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func IsDeparting(node *corev1.Node, taintKeys []string) bool`

- [ ] **Step 1: Write the failing test**

Create `internal/controller/nodes_test.go`:

```go
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func node(unschedulable bool, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: unschedulable, Taints: taints}}
}

func TestIsDeparting(t *testing.T) {
	tests := []struct {
		name  string
		node  *corev1.Node
		keys  []string
		want  bool
	}{
		{
			name: "a plain node is not departing",
			node: node(false),
			want: false,
		},
		{
			name: "cordoned, with no taint keys configured",
			node: node(true),
			want: true,
		},
		{
			name: "a configured key with NoSchedule",
			node: node(false, corev1.Taint{Key: "k", Effect: corev1.TaintEffectNoSchedule}),
			keys: []string{"k"},
			want: true,
		},
		{
			name: "the same key with NoExecute",
			node: node(false, corev1.Taint{Key: "k", Effect: corev1.TaintEffectNoExecute}),
			keys: []string{"k"},
			want: true,
		},
		{
			// PreferNoSchedule does not stop the replacement being scheduled
			// back onto this node, so treating it as departing would condemn
			// the replacement too, and the one after that, without end.
			name: "the same key with PreferNoSchedule is not enough",
			node: node(false, corev1.Taint{Key: "k", Effect: corev1.TaintEffectPreferNoSchedule}),
			keys: []string{"k"},
			want: false,
		},
		{
			name: "an unconfigured key with NoSchedule",
			node: node(false, corev1.Taint{Key: "other", Effect: corev1.TaintEffectNoSchedule}),
			keys: []string{"k"},
			want: false,
		},
		{
			name: "a taint is enough on its own, uncordoned",
			node: node(false, corev1.Taint{Key: "b", Effect: corev1.TaintEffectNoSchedule}),
			keys: []string{"a", "b"},
			want: true,
		},
		{
			name: "cordoned outranks a taint list that matches nothing",
			node: node(true, corev1.Taint{Key: "other", Effect: corev1.TaintEffectNoSchedule}),
			keys: []string{"k"},
			want: true,
		},
		{
			name: "a nil node is not departing",
			node: nil,
			keys: []string{"k"},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDeparting(tc.node, tc.keys); got != tc.want {
				t.Fatalf("IsDeparting() = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
go test ./internal/controller/ -run TestIsDeparting -v
```

Expected: compile failure, `undefined: IsDeparting`.

- [ ] **Step 3: Write the implementation**

Create `internal/controller/nodes.go` with the repository's Apache licence
header (copy the 15-line block from the top of
`internal/controller/rollout.go`), then:

```go
package controller

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// IsDeparting reports whether this node is on its way out of service, and the
// pods on it should be moved before somebody else moves them the hard way.
//
// Two ways in. spec.unschedulable is what kubectl cordon and kubectl drain
// set, and it is the criterion the master design names (§5.1); it is hardwired
// and not configurable. A taint key from the operator's -drain-taint list is
// the second, for autoscalers that taint before they cordon.
//
// The effect is part of the taint test and not decoration. A PreferNoSchedule
// taint does not stop the scheduler putting the replacement pod back on this
// same node: we would condemn a pod, rebuild it here, condemn that one next
// pass, and rotate for as long as the taint stands. Restricting the match to
// the two effects that actually repel a pod closes that loop by construction
// rather than with a guard somewhere downstream.
//
// A nil node is not departing. The caller reaches that case when a node cannot
// be read at all, and failing towards "not departing" keeps an unreadable Node
// from emptying a group; the watch and the resync bring the answer back within
// seconds.
func IsDeparting(node *corev1.Node, taintKeys []string) bool {
	if node == nil {
		return false
	}
	if node.Spec.Unschedulable {
		return true
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if slices.Contains(taintKeys, taint.Key) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the test and watch it pass**

```bash
go test ./internal/controller/ -run TestIsDeparting -v
```

Expected: PASS, nine subtests.

- [ ] **Step 5: Run the full suite**

```bash
make test
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/nodes.go internal/controller/nodes_test.go
git commit -m "feat(4c-3): a node is departing when cordoned or tainted to repel"
```

---

### Task 2: `DecideSize` condemns servers on a departing node

**Files:**
- Modify: `internal/controller/candidates.go` (`ServerView`, `leaving`)
- Modify: `internal/controller/scaling.go` (`SizeDecision`, `DecideSize`, `deletable`)
- Test: `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 — this task is pure arithmetic and does not
  know what a node is.
- Produces: `ServerView.Condemned bool`; `SizeDecision.Condemn []string`.

**Context the brief cannot carry:** `DecideSize` is a chain of early returns in
a deliberate order (capacity, ceiling, demand — `scaling.go:387`). Do **not**
add `Condemn` to each return. Condemnation is not a sizing decision; it rides
alongside one. Wrap the existing body instead, as Step 3 shows.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/scaling_test.go`. Match the file's existing
helper style for building `ScalingInputs` and `ServerView` — read the top of
the file first and reuse whatever constructor is already there rather than
writing a second one.

```go
func TestDecideSizeCondemns(t *testing.T) {
	t.Run("a condemned server is named even with no surplus", func(t *testing.T) {
		in := ScalingInputs{
			Views: []ServerView{
				{Name: "a", Phase: phase.Ready, Slots: 10, Players: 3},
				{Name: "b", Phase: phase.Ready, Slots: 10, Players: 3, Condemned: true},
			},
			MinReplicas: 2, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		if len(got.Condemn) != 1 || got.Condemn[0] != "b" {
			t.Fatalf("Condemn = %v, want [b]", got.Condemn)
		}
		if got.Surplus != 0 {
			t.Fatalf("Surplus = %d, want 0: a node drain is not a scale-down", got.Surplus)
		}
	})

	t.Run("minReplicas does not hold a condemned server back", func(t *testing.T) {
		in := ScalingInputs{
			Views:       []ServerView{{Name: "a", Phase: phase.Ready, Slots: 10, Condemned: true}},
			MinReplicas: 1, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		if len(got.Condemn) != 1 || got.Condemn[0] != "a" {
			t.Fatalf("Condemn = %v, want [a]", got.Condemn)
		}
	})

	t.Run("the replacement is asked for in the same pass", func(t *testing.T) {
		// The only server is condemned, so it stops holding the floor and
		// stops contributing capacity: the same pass that condemns it must
		// order its replacement.
		in := ScalingInputs{
			Views:       []ServerView{{Name: "a", Phase: phase.Ready, Slots: 10, Condemned: true}},
			MinReplicas: 1, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		if got.Create < 1 {
			t.Fatalf("Create = %d, want at least 1", got.Create)
		}
	})

	t.Run("all condemned servers go in one pass", func(t *testing.T) {
		in := ScalingInputs{
			Views: []ServerView{
				{Name: "a", Phase: phase.Ready, Slots: 10, Condemned: true},
				{Name: "b", Phase: phase.Ready, Slots: 10, Condemned: true},
				{Name: "c", Phase: phase.Ready, Slots: 10},
			},
			MinReplicas: 3, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		if len(got.Condemn) != 2 {
			t.Fatalf("Condemn = %v, want two names", got.Condemn)
		}
	})

	t.Run("Delete and Condemn never name the same server", func(t *testing.T) {
		// A group over its ceiling with a condemned server in it: the surplus
		// rule must not nominate the pod the node drain is already taking.
		in := ScalingInputs{
			Views: []ServerView{
				{Name: "a", Phase: phase.Ready, Slots: 10, Condemned: true},
				{Name: "b", Phase: phase.Ready, Slots: 10},
				{Name: "c", Phase: phase.Ready, Slots: 10},
			},
			MinReplicas: 1, MaxReplicas: 2, MaxPlayers: 10, SpareSlots: 1,
		}
		got := DecideSize(in)
		for _, d := range got.Delete {
			for _, c := range got.Condemn {
				if d == c {
					t.Fatalf("%q is in both Delete and Condemn", d)
				}
			}
		}
	})

	t.Run("no condemned servers leaves Condemn nil", func(t *testing.T) {
		in := ScalingInputs{
			Views:       []ServerView{{Name: "a", Phase: phase.Ready, Slots: 10}},
			MinReplicas: 1, MaxReplicas: 5, MaxPlayers: 10, SpareSlots: 1,
		}
		if got := DecideSize(in); got.Condemn != nil {
			t.Fatalf("Condemn = %v, want nil", got.Condemn)
		}
	})
}
```

- [ ] **Step 2: Run them and watch them fail**

```bash
go test ./internal/controller/ -run TestDecideSizeCondemns -v
```

Expected: compile failure, `unknown field Condemned in struct literal`.

- [ ] **Step 3: Write the implementation**

In `internal/controller/candidates.go`, add the field to `ServerView` (after
`Retire`, before `CreatedAt`):

```go
	// Condemned is true when this server's pod sits on a node that is on its
	// way out of service. Set by collectViews from pod.spec.nodeName and the
	// operator's departing-node test; the view carries the conclusion so the
	// sizing rules stay free of node vocabulary, the same way Stale carries a
	// conclusion about the player count.
	Condemned bool
```

In the same file, extend `leaving`:

```go
func (v ServerView) leaving() bool {
	return v.Phase == phase.Draining || v.Phase == phase.Terminating ||
		v.Phase == phase.Retiring || v.Condemned
}
```

Update `leaving`'s doc comment to say why Condemned belongs there: it is the
same reason Retiring is — dropping out of the group's size is what makes the
spare-slot rule order the replacement, and it is what stops the deletion
nomination naming a server that is already going.

In `internal/controller/scaling.go`, add to `SizeDecision`:

```go
	// Condemn names the servers whose node is departing. They are deleted
	// unconditionally: not bounded by Surplus, not held back by MinReplicas,
	// and all of them in one pass. It is a separate field from Delete so the
	// two reasons never share a number — Delete is the scale-down nomination
	// and Surplus is what the ceiling asked for, and a node drain is about
	// neither.
	Condemn []string
```

Rename the existing `DecideSize` body to `decideSize` and add the wrapper:

```go
// DecideSize is the group's sizing rule, plus the one removal that is not a
// sizing decision at all.
//
// Condemnation rides alongside the size decision rather than inside it. The
// chain in decideSize is an ordered set of early returns — capacity, then the
// ceiling, then demand — and a node drain answers to none of those three: the
// node is leaving with or without the group's consent, so no branch may
// decline it and no branch may bound it. Attaching it to whichever decision
// comes back keeps that independence visible and keeps the chain unchanged.
func DecideSize(in ScalingInputs) SizeDecision {
	decision := decideSize(in)
	decision.Condemn = condemned(in)
	return decision
}

// condemned names every server whose node is departing and whose removal has
// not already been reserved. Nil when there are none, so a caller can tell
// "nothing to condemn" from "an empty list was built".
func condemned(in ScalingInputs) []string {
	var out []string
	for _, v := range in.Views {
		if v.Condemned && !in.PendingDeletes[v.Name] {
			out = append(out, v.Name)
		}
	}
	return out
}
```

In `deletable`, skip condemned servers so the surplus rule cannot nominate one:

```go
func deletable(in ScalingInputs) []ServerView {
	out := make([]ServerView, 0, len(in.Views))
	for _, v := range in.Views {
		// Condemned is skipped for the same reason as Retire: this server is
		// already leaving by another route, and naming it here would put it in
		// Delete and Condemn at once.
		if in.PendingDeletes[v.Name] || in.PendingRetires[v.Name] || v.Retire || v.Condemned {
			continue
		}
		out = append(out, v)
	}
	return out
}
```

- [ ] **Step 4: Run the tests and watch them pass**

```bash
go test ./internal/controller/ -run TestDecideSize -v
```

Expected: PASS, including every pre-existing `TestDecideSize*` case. If an old
case now fails, stop and report it — it means the `leaving` change moved
something this plan did not intend.

- [ ] **Step 5: Run the full suite**

```bash
make test
```

- [ ] **Step 6: Commit**

```bash
git add internal/controller/candidates.go internal/controller/scaling.go internal/controller/scaling_test.go
git commit -m "feat(4c-3): a condemned server leaves the group's arithmetic and its size"
```

---

### Task 3: Configuration, RBAC, and reading a node

**Files:**
- Modify: `internal/controller/nodes.go`
- Modify: `internal/controller/setup.go:29-54` (`Options`)
- Modify: `cmd/spawnery-operator/main.go:119-141` (flags), `:170-173` (cache)
- Modify: `internal/controller/servergroup_controller.go:82-85` (RBAC marker)
- Modify: `config/rbac/role.yaml` (generated — do not hand-edit)
- Test: `internal/controller/nodes_test.go`

**Interfaces:**
- Consumes: `IsDeparting(node *corev1.Node, taintKeys []string) bool` (Task 1).
- Produces:
  - `func nodeDeparting(ctx context.Context, reader client.Reader, nodeName string, taintKeys []string) bool`
  - `Options.DrainTaintKeys []string`
  - operator flag `-drain-taint` (repeatable)

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/nodes_test.go`. This one needs a client;
`testenv.Client(t)` returns `(client, ctx)` — the same call `newFixture` makes
at `suite_test.go:195`.

```go
func TestNodeDeparting(t *testing.T) {
	c, ctx := testenv.Client(t)

	// An unresolvable node is not departing: failing towards "stay" keeps an
	// unreadable Node from emptying a group on the strength of a cache miss.
	if nodeDeparting(ctx, c, "no-such-node", nil) {
		t.Error("an unreadable node must not read as departing")
	}
	// An empty node name is an unscheduled pod, which is on no node at all.
	if nodeDeparting(ctx, c, "", nil) {
		t.Error("an empty node name must not read as departing")
	}

	// A real node, before and after the cordon. Nodes are cluster-scoped, so
	// the name has to be unique across this whole test binary.
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-departing-test"}}
	if err := c.Create(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, node) })

	if nodeDeparting(ctx, c, node.Name, nil) {
		t.Error("a plain node reads as departing")
	}
	node.Spec.Unschedulable = true
	if err := c.Update(ctx, node); err != nil {
		t.Fatalf("cordon node: %v", err)
	}
	if !nodeDeparting(ctx, c, node.Name, nil) {
		t.Error("a cordoned node does not read as departing")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
go test ./internal/controller/ -run TestNodeDeparting -v
```

Expected: compile failure, `undefined: nodeDeparting`.

- [ ] **Step 3: Write the resolver**

Append to `internal/controller/nodes.go`:

```go
// nodeDeparting resolves a pod's node and asks IsDeparting about it.
//
// Every failure answers false. An empty name is an unscheduled pod, which is
// on no node; a Get that fails is a node we cannot read, and a group must not
// be emptied on the strength of a cache miss. The watch and the periodic
// resync both bring the question back within seconds, so a false answer here
// costs a few seconds and never a wrong deletion.
func nodeDeparting(ctx context.Context, reader client.Reader, nodeName string, taintKeys []string) bool {
	if nodeName == "" {
		return false
	}
	node := &corev1.Node{}
	if err := reader.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return false
	}
	return IsDeparting(node, taintKeys)
}
```

- [ ] **Step 4: Add the configuration**

In `internal/controller/setup.go`, add to `Options` (after `Proxies`):

```go
	// DrainTaintKeys are the taint keys that mark a node as departing, beside
	// spec.unschedulable. Empty is the default and the ordinary case: both
	// cluster-autoscaler and Karpenter cordon a node as well as tainting it,
	// so an empty list still sees them, a moment later.
	DrainTaintKeys []string
```

Pass `DrainTaintKeys: opts.DrainTaintKeys` into both the `ServerGroupReconciler`
and the `ProxyGroupReconciler` literals in `SetupAll`, and add the matching
field to both reconciler structs:

```go
	// DrainTaintKeys is Options.DrainTaintKeys. Nil means only cordoned nodes
	// count.
	DrainTaintKeys []string
```

In `cmd/spawnery-operator/main.go`, add the repeatable flag. Copy the collector
type from `cmd/spawnery-stubop/main.go:148-161`, renamed, and put it beside the
other flag declarations:

```go
// taintKeys collects a repeatable flag. A cluster may mark a departing node
// with more than one vendor's taint, and one flag per key beats parsing a
// separator out of a key that is allowed to contain almost anything.
type taintKeys []string

func (t *taintKeys) String() string { return strings.Join(*t, ",") }

func (t *taintKeys) Set(value string) error {
	if value == "" {
		return fmt.Errorf("an empty taint key would match nothing")
	}
	*t = append(*t, value)
	return nil
}
```

```go
	var drainTaints taintKeys
	flag.Var(&drainTaints, "drain-taint",
		"taint key that marks a node as departing, beside spec.unschedulable; repeatable")
```

and thread it into the `controller.Options` literal as
`DrainTaintKeys: drainTaints`.

- [ ] **Step 5: Add the RBAC marker and the cache restriction**

Add to the marker block at `internal/controller/servergroup_controller.go:82`:

```go
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
```

In `cmd/spawnery-operator/main.go`, extend the `Cache.ByObject` map:

```go
		// The node cache exists for one bool per node — cordoned, or tainted to
		// repel. status.images is tens of kilobytes per node and nothing here
		// reads it, so it is dropped on the way in, for the same reason the
		// ConfigMap restriction above exists.
		&corev1.Node{}: {
			Transform: func(obj any) (any, error) {
				if node, ok := obj.(*corev1.Node); ok {
					node.Status.Images = nil
				}
				return obj, nil
			},
		},
```

- [ ] **Step 6: Verify the namespace-scoped case**

The spec (§3.2) leaves one thing open deliberately. Check the vendored
controller-runtime version: does `Cache.DefaultNamespaces`
(`cmd/spawnery-operator/main.go:163`) leave cluster-scoped kinds cached
cluster-wide, or does it restrict them too?

```bash
grep -rn "DefaultNamespaces" $(go env GOMODCACHE)/sigs.k8s.io/controller-runtime@*/pkg/cache/cache.go | head
```

Read the surrounding doc comment. If cluster-scoped kinds are **not** left
cluster-wide, add an explicit `cache.Config{}` entry for `&corev1.Node{}` that
clears the namespace restriction, and write a one-paragraph comment recording
which version behaves which way. Either way, record the answer in the task
report — a later reader must not have to redo this.

- [ ] **Step 7: Regenerate and run**

```bash
make manifests
git diff --stat config/rbac/role.yaml
make test
```

Expected: `role.yaml` gains a `nodes` rule with `get;list;watch` under the core
API group. If it does not, the marker is in a file `make manifests` does not
scan — check the path rather than hand-editing the YAML.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/nodes.go internal/controller/nodes_test.go internal/controller/setup.go internal/controller/servergroup_controller.go internal/controller/proxygroup_controller.go cmd/spawnery-operator/main.go config/rbac/role.yaml
git commit -m "feat(4c-3): the operator may read nodes, and knows which taints matter"
```

---

### Task 4: The ServerGroup condemns and deletes

**Files:**
- Modify: `internal/controller/servergroup_controller.go` (`collectViews:553-569`,
  `size:496-508`, `SetupWithManager:785-799`)
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `nodeDeparting(ctx, reader, nodeName, taintKeys) bool` (Task 3);
  `ServerView.Condemned`, `SizeDecision.Condemn` (Task 2);
  `ServerGroupReconciler.DrainTaintKeys` (Task 3).
- Produces: `Server` CRs deleted when their node departs; event reason
  `NodeDraining` on the ServerGroup.

- [ ] **Step 1: Add the two shared fixture helpers**

Tasks 4 through 8 all need to put a pod on a node and to cordon that node.
Add both to `internal/controller/suite_test.go`, beside `setPodRunning`.

**`spec.nodeName` is immutable after creation.** The API server rejects an
update that sets or changes it, so a test cannot pin a controller-created pod
to a node with `Update`. The scheduler's own route is the `binding`
subresource, and envtest's API server serves it:

```go
// bindPodToNode does what a scheduler would. envtest runs none, and
// pod.spec.nodeName cannot be set by Update -- the API server rejects it --
// so the binding subresource is the only way a test can place a pod.
func (f *fixture) bindPodToNode(t *testing.T, pod *corev1.Pod, nodeName string) {
	t.Helper()
	binding := &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		Target:     corev1.ObjectReference{Kind: "Node", Name: nodeName},
	}
	if err := f.c.SubResource("binding").Create(f.ctx, pod, binding); err != nil {
		t.Fatalf("bind pod %s to node %s: %v", pod.Name, nodeName, err)
	}
}

// ensureNode creates a Node, cordoned or not, and cleans it up. Nodes are
// cluster-scoped, so unlike everything else these tests create they are not
// isolated by the per-test namespace and must carry a unique name.
func (f *fixture) ensureNode(t *testing.T, name string, unschedulable bool) *corev1.Node {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := controllerutil.CreateOrUpdate(f.ctx, f.c, node, func() error {
		node.Spec.Unschedulable = unschedulable
		return nil
	})
	if err != nil {
		t.Fatalf("ensure node %s: %v", name, err)
	}
	t.Cleanup(func() { _ = f.c.Delete(f.ctx, node) })
	return node
}
```

If `SubResource("binding").Create` is not available on the vendored
controller-runtime client, fall back to a raw client-go `Post()` against
`/api/v1/namespaces/{ns}/pods/{name}/binding` and record which it was in the
task report.

- [ ] **Step 2: Write the failing envtest**

Add to `internal/controller/servergroup_controller_test.go`:

```go
// TestACordonedNodeCondemnsTheServersOnIt is the server half of the milestone:
// a node on its way out empties itself of servers, and the group rebuilds them
// somewhere else without anybody being kicked.
func TestACordonedNodeCondemnsTheServersOnIt(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	rec := r.Recorder.(*record.FakeRecorder)

	// Two servers, both Ready, one occupied so the test also proves that
	// having players does not exempt a server from a node that is leaving.
	f.setMinReplicas(t, 2)
	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(servers))
	}
	f.markReadyWithPlayers(t, servers[0].Name, 3)
	f.markReady(t, servers[1].Name)

	// Place them on two nodes, then cordon the first.
	going := f.ensureNode(t, "node-going-"+f.ns, false)
	f.ensureNode(t, "node-staying-"+f.ns, false)
	for i, srv := range f.listServers(t) {
		reloaded := f.server(srv.Name)
		pod, ok := f.pod(reloaded.Status.PodName)
		if !ok {
			t.Fatalf("pod of %s not found", srv.Name)
		}
		if i == 0 {
			f.bindPodToNode(t, pod, going.Name)
		} else {
			f.bindPodToNode(t, pod, "node-staying-"+f.ns)
		}
	}
	f.ensureNode(t, going.Name, true)

	f.reconcileGroup(t, r)

	// The server on the cordoned node is going.
	condemned := f.server(servers[0].Name)
	if condemned.DeletionTimestamp.IsZero() {
		t.Error("the server on the cordoned node was not deleted; its players will be evicted instead of moved")
	}
	// The one beside it is not.
	if !f.server(servers[1].Name).DeletionTimestamp.IsZero() {
		t.Error("the server on the healthy node was deleted; only the departing node's servers may go")
	}
	// And the operator is told why.
	if n := scalingEvents(rec, "NodeDraining"); n != 1 {
		t.Errorf("NodeDraining events = %d, want exactly 1", n)
	}
}

// TestCondemnedServersAreReplaced states the property that makes the drain
// finite: the pass that condemns is the pass that orders the replacement, so
// the group is never left short while it waits for a second reconcile.
func TestCondemnedServersAreReplaced(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	f.markReady(t, servers[0].Name)

	node := f.ensureNode(t, "node-going-"+f.ns, false)
	pod, ok := f.pod(f.server(servers[0].Name).Status.PodName)
	if !ok {
		t.Fatal("pod not found")
	}
	f.bindPodToNode(t, pod, node.Name)
	f.ensureNode(t, node.Name, true)

	f.reconcileGroup(t, r)

	live := 0
	for _, s := range f.listServers(t) {
		if s.DeletionTimestamp.IsZero() {
			live++
		}
	}
	if live < 1 {
		t.Fatal("no replacement was ordered in the pass that condemned; the group would sit empty until the next one")
	}
}
```

- [ ] **Step 3: Run them and watch them fail**

```bash
go test ./internal/controller/ -run 'TestACordonedNodeCondemns|TestCondemnedServersAreReplaced' -v
```

Expected: FAIL — the server on the cordoned node is untouched.

- [ ] **Step 4: Set `Condemned` in `collectViews`**

`collectViews` already has the pod in hand at `servergroup_controller.go:545`.
Add to the `ServerView` literal at `:553`:

```go
			// podFound is required: a server with no pod is on no node, and one
			// whose pod already carries a deletion timestamp is leaving under
			// its own power. Neither is condemned by a node.
			Condemned: podFound && nodeDeparting(ctx, r.Client, pod.Spec.NodeName, r.DrainTaintKeys),
```

- [ ] **Step 5: Execute the condemnations**

In `size`, after the `decision.Delete` loop and before the `decision.Retire`
loop (`servergroup_controller.go:502`):

```go
	// Ungated by the backoff, like the deletes and retirements around it and
	// for the same reason: this touches players and must not wait on an
	// unrelated failure. Reserved after the delete, matching the loop above.
	for _, name := range decision.Condemn {
		if err := r.deleteServer(ctx, group, servers, name,
			"NodeDraining", "draining server %s off a node that is going away"); err != nil {
			return decision, err
		}
		r.Expectations.expectDeleted(key, name)
	}
```

Read `deleteServer` before writing this: confirm it is a no-op against a server
that already carries a deletion timestamp, so a condemned server that is
already `Draining` is not re-deleted every pass and does not re-emit its event.
If it is not, make it one, and say so in the report.

- [ ] **Step 6: Add the node watch**

In `SetupWithManager`, before `.Named("servergroup")`:

```go
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.groupsOnNode)).
```

and write the mapping function beside it:

```go
// groupsOnNode maps a Node event onto the ServerGroups with pods on that node.
//
// The five-second resync would find a cordoned node on its own; the watch is
// what makes the answer immediate, and an eviction issued in the same second
// as the cordon is exactly the race this milestone is about.
//
// It lists this operator's pods and filters by node rather than asking for a
// spec.nodeName field index. An index would have to be registered once and
// shared by two controllers -- registering it twice fails at manager start --
// and the population it would serve is this operator's own pods, which is
// small. A label-scoped list over a warm cache is cheaper than that
// coordination is worth.
func (r *ServerGroupReconciler) groupsOnNode(ctx context.Context, obj client.Object) []reconcile.Request {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelRole:      podspec.RoleServer,
	}); err != nil {
		return nil
	}
	seen := map[types.NamespacedName]bool{}
	var out []reconcile.Request
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != obj.GetName() {
			continue
		}
		group := pod.Labels[podspec.LabelGroup]
		if group == "" {
			continue
		}
		key := types.NamespacedName{Name: group, Namespace: pod.Namespace}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, reconcile.Request{NamespacedName: key})
	}
	return out
}
```

Check `internal/podspec/labels.go` for the exact constant names before writing
this — `LabelRole` and `RoleServer` are used here from
`podspec.GroupConfigMapName(group, podspec.RoleServer)`
(`servergroup_controller.go:766`), but confirm the label key that actually
lands on a server pod rather than assuming it.

- [ ] **Step 7: Run the tests**

```bash
go test ./internal/controller/ -run TestServerGroup -v
make test
```

- [ ] **Step 8: Commit**

```bash
git add internal/controller/servergroup_controller.go internal/controller/servergroup_controller_test.go
git commit -m "feat(4c-3): a departing node deletes the servers on it, and the group rebuilds"
```

---

### Task 5: A departing node makes a proxy stale

**Files:**
- Modify: `internal/controller/proxygroup_controller.go`
  (`reconcileReplicas:328-341`, `SetupWithManager:1022-1043`)
- Test: `internal/controller/rollout_test.go`,
  `internal/controller/proxygroup_controller_test.go`

**Interfaces:**
- Consumes: `nodeDeparting(...)` and `ProxyGroupReconciler.DrainTaintKeys`
  (Task 3).
- Produces: nothing new — `DecideRollout` is unchanged.

- [ ] **Step 1: Write the failing tests**

Two levels. In `rollout_test.go`, the uncordon case the spec (§3.6) says must be
proved rather than assumed:

```go
// Releasing the node mid-flight leaves the work already begun to finish: the
// marked pod is draining and not Ready, pick sorts it first, and the surplus
// branch keeps the mark on what pick returns. Spec §3.6 expects this to fall
// out of the existing rules; this test is what establishes it.
func TestDecideRolloutKeepsTheMarkAfterUncordon(t *testing.T) {
	pods := []ProxyView{
		{Name: "old", Stale: false, Ready: false, Draining: true, Players: 2},
		{Name: "new", Stale: false, Ready: true},
		{Name: "other", Stale: false, Ready: true},
	}
	got := DecideRollout(pods, 2)
	if got.Create != 0 {
		t.Fatalf("Create = %d, want 0", got.Create)
	}
	if len(got.Drain) != 0 {
		t.Fatalf("Drain = %v, want none: the draining guard holds", got.Drain)
	}
}
```

Then in `proxygroup_controller_test.go`, using the fixture helpers from Task 4:

```go
// TestAProxyOnACordonedNodeIsReplaced is the proxy half of the milestone. It
// asserts the order, not just the outcome: the replacement is ready before
// anything is withdrawn, which is what keeps the group's ready capacity from
// dipping while a player is connected.
func TestAProxyOnACordonedNodeIsReplaced(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(pods))
	}
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}

	going := f.ensureNode(t, "node-going-"+f.ns, false)
	staying := f.ensureNode(t, "node-staying-"+f.ns, false)
	f.bindPodToNode(t, &pods[0], going.Name)
	f.bindPodToNode(t, &pods[1], staying.Name)
	f.ensureNode(t, going.Name, true)

	// The surge pod comes first, and nothing is marked while it is unready.
	f.reconcileProxyGroup(r, "gateway")
	after := f.proxyPods("gateway")
	if len(after) != 3 {
		t.Fatalf("proxy pods = %d after the cordon, want 3 — the replacement must exist before anything is marked", len(after))
	}
	for i := range after {
		if _, dated := drainingSince(&after[i]); dated {
			t.Fatalf("pod %s was marked while the replacement is still unready", after[i].Name)
		}
	}

	// Once it is ready, the pod on the departing node is the one that goes.
	for i := range after {
		f.markProxyPodReady(t, &after[i])
	}
	f.reconcileProxyGroup(r, "gateway")

	var marked []string
	for _, p := range f.proxyPods("gateway") {
		if _, dated := drainingSince(&p); dated {
			marked = append(marked, p.Name)
		}
	}
	if len(marked) != 1 || marked[0] != pods[0].Name {
		t.Fatalf("marked = %v, want exactly [%s]: the pod on the departing node and no other", marked, pods[0].Name)
	}
}
```

The replacement pod is created by the reconciler and envtest schedules nothing,
so its `spec.nodeName` is empty — it reads as not departing, which is what the
test relies on.

- [ ] **Step 2: Run them and watch the envtest one fail**

```bash
go test ./internal/controller/ -run 'TestDecideRolloutKeepsTheMark|TestProxyGroup' -v
```

- [ ] **Step 3: Feed the node into staleness**

At `proxygroup_controller.go:334`:

```go
			// Two ways to be out of date, and the rollout does not distinguish
			// them: a pod whose rendered shape no longer matches the group, and
			// a pod on a node that is going away. Both have to be replaced by a
			// pod somewhere else, one at a time, without disconnecting anyone —
			// which is the sentence DecideRollout already implements.
			Stale: pods[i].Labels[podspec.LabelPodHash] != wantHash ||
				nodeDeparting(ctx, r.Client, pods[i].Spec.NodeName, r.DrainTaintKeys),
```

- [ ] **Step 4: Add the node watch**

Mirror Task 5's ServerGroup watch: `Watches(&corev1.Node{}, ...)` on the
ProxyGroup with a mapping function that returns the ProxyGroups with pods on
that node, reusing the `spec.nodeName` index registered in Task 4.

- [ ] **Step 5: Run the tests**

```bash
make test
```

- [ ] **Step 6: Commit**

```bash
git add internal/controller/proxygroup_controller.go internal/controller/rollout_test.go internal/controller/proxygroup_controller_test.go
git commit -m "feat(4c-3): a proxy on a departing node is stale, and the rollout does the rest"
```

---

### Task 6: The proxy `occupied` label

**Files:**
- Modify: `internal/controller/proxygroup_controller.go`
- Test: `internal/controller/proxygroup_controller_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func proxyOccupied(snap agent.Snapshot) bool`; the label
  `podspec.LabelOccupied` maintained on proxy pods.

**Context the brief cannot carry:** the emptiness rule already exists, inline,
in the deletion loop at `proxygroup_controller.go:644`:
`players == 0 && !snap.PlayersStale && snap.Connected`. Extract it so the label
and that loop cannot disagree — a budget counting fewer pods than carry the
label hands the eviction API a disruption to spend on an occupied pod
(`candidates.go:74`). Do not write a second rule.

- [ ] **Step 1: Write the failing test**

```go
func TestProxyOccupied(t *testing.T) {
	tests := []struct {
		name string
		snap agent.Snapshot
		want bool
	}{
		{"known empty on a live stream", agent.Snapshot{Players: 0, Connected: true}, false},
		{"players on a live stream", agent.Snapshot{Players: 3, Connected: true}, true},
		{"a stale count counts as occupied", agent.Snapshot{Players: 0, PlayersStale: true, Connected: true}, true},
		{"a dead stream counts as occupied", agent.Snapshot{Players: 0, Connected: false}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := proxyOccupied(tc.snap); got != tc.want {
				t.Fatalf("proxyOccupied() = %v, want %v", got, tc.want)
			}
		})
	}
}
```

And the envtest case that proves the label follows the rule on a real pod:

```go
// TestTheOccupiedLabelFollowsTheProxyPlayerCount is what makes the budget in
// Task 7 mean anything: the selector matches occupied pods and stops matching
// when they empty.
func TestTheOccupiedLabelFollowsTheProxyPlayerCount(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}
	f.reportProxyPlayers(t, pods[0], 4)
	f.reportProxyPlayers(t, pods[1], 0)
	f.reconcileProxyGroup(r, "gateway")

	byName := map[string]corev1.Pod{}
	for _, p := range f.proxyPods("gateway") {
		byName[p.Name] = p
	}
	if _, ok := byName[pods[0].Name].Labels[podspec.LabelOccupied]; !ok {
		t.Errorf("pod %s has players and no occupied label; the budget would let it be evicted", pods[0].Name)
	}
	if _, ok := byName[pods[1].Name].Labels[podspec.LabelOccupied]; ok {
		t.Errorf("pod %s is known empty and still labelled occupied; the budget would block a drain for nobody", pods[1].Name)
	}
}
```

- [ ] **Step 2: Run and watch fail**

```bash
go test ./internal/controller/ -run TestProxyOccupied -v
```

- [ ] **Step 3: Extract the rule and maintain the label**

```go
// proxyOccupied is the single occupancy rule for a proxy pod. Both sides of
// the PodDisruptionBudget are computed from it: this reconciler labels pods
// with it and sizes the budget's minAvailable from the same answer, and the
// two have to agree pod for pod.
//
// A proxy has no wasRegistered qualifier the way a server does — it sits behind
// the Service and players reach it directly — so for a running pod there is no
// state in which a stale count is safe to read as empty. Staleness alone is
// enough, and so is a stream that is down: Velocity goes on serving the
// sessions it holds after its agent's stream breaks, so a count nobody is
// updating says nothing about who is on it.
func proxyOccupied(snap agent.Snapshot) bool {
	return !(snap.Players == 0 && !snap.PlayersStale && snap.Connected)
}
```

Replace the inline condition at `:644` with `!proxyOccupied(snap)` so there is
one rule and not two that happen to agree. Then add the label pass:

```go
// syncOccupiedLabels keeps podspec.LabelOccupied on the group's pods in step
// with proxyOccupied, so the PodDisruptionBudget's selector and its
// minAvailable are computed from the same answer pod for pod. A budget that
// counts fewer pods than carry the label hands the eviction API a disruption
// to spend on an occupied one.
func (r *ProxyGroupReconciler) syncOccupiedLabels(ctx context.Context, pods []corev1.Pod) error {
	for i := range pods {
		pod := &pods[i]
		occupied := proxyOccupied(r.Agents.Lookup(string(pod.UID)))
		_, labelled := pod.Labels[podspec.LabelOccupied]
		// No write when nothing changed: this runs every five seconds per pod,
		// and a patch per pass would be a write per pod per pass for the life
		// of the group.
		if occupied == labelled {
			continue
		}
		patched := pod.DeepCopy()
		if occupied {
			if patched.Labels == nil {
				patched.Labels = map[string]string{}
			}
			patched.Labels[podspec.LabelOccupied] = "true"
		} else {
			delete(patched.Labels, podspec.LabelOccupied)
		}
		if err := r.Patch(ctx, patched, client.MergeFrom(pod)); err != nil {
			return err
		}
	}
	return nil
}
```

Call it from `Reconcile` on the pods `pods()` returned, and before
`reconcileProxyPDB` in Task 7. Both compute occupancy from `proxyOccupied`, so
they agree on the number whatever the order — but the label has to be *on the
pods* before the budget counts on it. A budget asking for `minAvailable: 1`
whose selector currently matches no pod is unsatisfiable, and an unsatisfiable
budget blocks every eviction in the group rather than one.

- [ ] **Step 4: Run the tests**

```bash
make test
```

- [ ] **Step 5: Commit**

```bash
git add internal/controller/proxygroup_controller.go internal/controller/proxygroup_controller_test.go
git commit -m "feat(4c-3): proxy pods carry the occupied label, on one shared rule"
```

---

### Task 7: The ProxyGroup PodDisruptionBudget

**Files:**
- Modify: `internal/controller/proxygroup_controller.go`
- Test: `internal/controller/proxygroup_controller_test.go`

**Interfaces:**
- Consumes: `proxyOccupied` and `podspec.LabelOccupied` on proxy pods (Task 6).
- Produces: one `policyv1.PodDisruptionBudget` per ProxyGroup, owned by it.

- [ ] **Step 1: Write the failing envtest**

```go
// TestTheProxyGroupBudgetSizesToOccupiedPods is the object the eviction API
// consults. Every assertion here is a way the budget can be present and still
// protect nobody.
func TestTheProxyGroupBudgetSizesToOccupiedPods(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
	}
	f.reportProxyPlayers(t, pods[0], 4)
	f.reportProxyPlayers(t, pods[1], 0)
	f.reconcileProxyGroup(r, "gateway")

	pdb := &policyv1.PodDisruptionBudget{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway", Namespace: f.ns}, pdb); err != nil {
		t.Fatalf("get proxy PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("minAvailable = %v, want 1 — exactly one proxy is occupied", pdb.Spec.MinAvailable)
	}
	if pdb.Spec.MaxUnavailable != nil {
		t.Error("maxUnavailable is set; Kubernetes rejects it for pods with no controller carrying a scale subresource")
	}
	if got := pdb.Spec.Selector.MatchLabels[podspec.LabelOccupied]; got != "true" {
		t.Errorf("selector occupied = %q, want \"true\": a selector that matches empty pods blocks drains for nobody", got)
	}
	if len(pdb.OwnerReferences) == 0 {
		t.Error("the PDB carries no owner reference; it would outlive the group and block evictions forever")
	}
}
```

Then a second case: with both proxies reporting a fresh zero, `minAvailable`
drops to 0 — otherwise a drained group would still block its own node.

- [ ] **Step 2: Run and watch fail**

- [ ] **Step 3: Write `reconcileProxyPDB`**

```go
// reconcileProxyPDB keeps the group's PodDisruptionBudget in step with the
// number of occupied proxy pods.
//
// The same formulation as the ServerGroup's, for the same reason: for pods
// without a controller carrying a scale subresource, Kubernetes allows neither
// maxUnavailable nor percentages in a PDB. The absolute number of occupied
// pods is the only one that works, and it makes the eviction API refuse to
// evict any of them.
//
// Without this object, kubectl drain evicts a proxy in the same second the
// node is cordoned, while the replacement this operator ordered is still
// pulling its image -- and everyone on that proxy is disconnected by the
// eviction rather than carried by the drain.
func (r *ProxyGroupReconciler) reconcileProxyPDB(
	ctx context.Context,
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
) error {
	var occupied int32
	for i := range pods {
		if proxyOccupied(r.Agents.Lookup(string(pods[i].UID))) {
			occupied++
		}
	}
	minAvailable := intstr.FromInt32(occupied)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: group.Name, Namespace: group.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Spec.MinAvailable = &minAvailable
		pdb.Spec.MaxUnavailable = nil
		// Derived from ProxyLabels, the same function reconcileService uses as
		// the Service selector, plus the occupancy label -- not a hand-written
		// map that happens to match one.
		selector := podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)
		selector[podspec.LabelOccupied] = "true"
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		return controllerutil.SetControllerReference(group, pdb, r.Scheme)
	})
	return err
}
```

Check that `podspec.ProxyLabels` returns a fresh map on each call before
mutating it here. If it returns a shared one, copy it first — mutating it would
also change the `Service` selector, and a `Service` that only routes to
occupied proxies would strand every new player.

Call it from `Reconcile` after the pods are read, and add
`Owns(&policyv1.PodDisruptionBudget{})` to `SetupWithManager`. RBAC is already
granted (`servergroup_controller.go:85`).

- [ ] **Step 4: Run the tests**

```bash
make test
```

- [ ] **Step 5: Commit**

```bash
git add internal/controller/proxygroup_controller.go internal/controller/proxygroup_controller_test.go
git commit -m "feat(4c-3): a ProxyGroup budget the eviction API has to respect"
```

---

### Task 8: Reporting — the `NodeDraining` condition

**Files:**
- Modify: `api/v1alpha1/common_types.go:21-51`
- Modify: `internal/controller/servergroup_controller.go`,
  `internal/controller/proxygroup_controller.go`
- Test: both controller test files

**Interfaces:**
- Consumes: `ServerView.Condemned` (Task 2), the proxy staleness source
  (Task 5).
- Produces: `spawneryv1alpha1.ConditionNodeDraining = "NodeDraining"`.

- [ ] **Step 1: Write the failing tests**

One per group kind. The ServerGroup case:

```go
// TestNodeDrainingConditionNamesTheNode is what an operator sees in
// kubectl describe. A bare True would tell them something is happening
// without telling them where.
func TestNodeDrainingConditionNamesTheNode(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.reconcileGroup(t, r)
	servers := f.listServers(t)
	f.markReady(t, servers[0].Name)

	node := f.ensureNode(t, "node-going-"+f.ns, false)
	pod, ok := f.pod(f.server(servers[0].Name).Status.PodName)
	if !ok {
		t.Fatal("pod not found")
	}
	f.bindPodToNode(t, pod, node.Name)
	f.ensureNode(t, node.Name, true)
	f.reconcileGroup(t, r)

	cond := meta.FindStatusCondition(f.reloadGroup(t).Status.Conditions,
		spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("NodeDraining = %v, want True", cond)
	}
	if !strings.Contains(cond.Message, node.Name) {
		t.Errorf("message %q does not name the node", cond.Message)
	}

	// Release it: with no pods left on a departing node the condition goes
	// False rather than staying True until something else clears it.
	f.ensureNode(t, node.Name, false)
	f.reconcileGroup(t, r)
	cond = meta.FindStatusCondition(f.reloadGroup(t).Status.Conditions,
		spawneryv1alpha1.ConditionNodeDraining)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("NodeDraining after uncordon = %v, want False", cond)
	}
}
```

The ProxyGroup case is the same shape against `f.createProxyGroup`,
`f.reconcileProxyGroup` and `f.proxyGroup("gateway").Status.Conditions`.

Note what the second half does **not** assert: that the condemned server came
back. It did not, and must not — the CR is deleted and §3.6 is explicit that
work already begun finishes. The condition is about where pods are now.

- [ ] **Step 2: Run and watch fail**

- [ ] **Step 3: Add the constant**

```go
	// ConditionNodeDraining is true while this group has pods on nodes that
	// are on their way out of service, and names them. It reports; the
	// removals it describes are decided elsewhere.
	ConditionNodeDraining = "NodeDraining"
```

- [ ] **Step 4: Set it on both controllers**

Build it false-by-default and flip it, the way `ScalingLimited` and
`BackingOff` are built in `ServerGroupReconciler.Reconcile`. The event fires on
the flank only — `SetStatusCondition` moves `lastTransitionTime` only on a real
change, which is the same reason `reportReadinessDivergence`
(`proxygroup_controller.go:670`) puts its event there.

The ProxyGroup also owes the event spec §3.7 asks for and Task 5 did not add:
one `NodeDraining` event on the group when a proxy is marked because its node
is departing — not when it is marked for a hash mismatch, which is 4c-2's
occasion and already reported as a rolling update. Fire it where
`markDraining` is called (`proxygroup_controller.go:570`), gated on the pod's
node being the reason.

- [ ] **Step 5: Run the tests**

```bash
make test
```

- [ ] **Step 6: Commit**

```bash
git add api/v1alpha1/common_types.go internal/controller/servergroup_controller.go internal/controller/proxygroup_controller.go internal/controller/servergroup_controller_test.go internal/controller/proxygroup_controller_test.go
git commit -m "feat(4c-3): a group says which of its nodes are going away"
```

---

### Task 9: Documentation and the evidence runbook

**Files:**
- Modify: `docs/known-issues.md`
- Modify: `docs/handover-milestone-4.md`
- Modify: `docs/runbook-milestone-4c1-evidence.md` (new §12)

**Interfaces:**
- Consumes: everything above.
- Produces: no code.

- [ ] **Step 1: Known issues**

Add a "From milestone 4c-3" section covering, one paragraph each:

- **A node holding a whole group empties it at once**, so its players go to the
  fallback groups rather than to the group's own replacements. The nature of
  losing that node, not a choice — but an operator meeting it deserves to find
  it written down.
- **A `Persistent` server on a node-pinned RWO volume may not be schedulable
  anywhere else.** It is condemned all the same and its replacement then sits
  `Pending`. A limit of the storage class.
- **The taint list is trusted, not validated.** A key configured with an effect
  this operator ignores simply never matches; there is no warning.

- [ ] **Step 2: Handover**

Add a "4c-3 has landed" section in the register of the existing "4c-1 has
landed" and "4c-2 has landed" sections: what it built, what the next milestone
finds in place, and — explicitly — that `Server` gained no status field because
the group already holds the pod.

- [ ] **Step 3: Write runbook §12**

The evidence run. Two things this section must solve rather than discover:

1. **A multi-node kind cluster.** 4c-1's is single-node
   (`runbook-milestone-4c1-evidence.md:251`). Use one control-plane and two
   workers.
2. **The NodePort mapping.** A kind `extraPortMappings` host port binds on one
   node container, and the replacement proxy lands on a different node. Map
   **two** ports across **two** workers — the player keeps their existing
   connection through the old proxy's port while the new proxy becomes
   reachable on the other.

Then the steps, in the register of §§1-11: bring the cluster up, put a real
client on a proxy, `kubectl cordon` the worker holding it, watch the
replacement come up on the other worker, watch the old proxy go `NotReady` and
lose its Service endpoint, confirm the player is still playing, then
`kubectl drain --ignore-daemonsets` and confirm it completes rather than
hanging. The same for an occupied server pod: the players are moved, not
kicked.

Mark the section as not yet driven. It is driven by the human partner and me
together, after the branch review, and the header is rewritten then.

- [ ] **Step 4: Commit**

```bash
git add docs/known-issues.md docs/handover-milestone-4.md docs/runbook-milestone-4c1-evidence.md
git commit -m "docs(4c-3): what node drain leaves behind, and how to see it work"
```

---

## Notes for the executor

- **The absolute-word sweep is not optional.** Before each task's commit, run
  `git diff -U0 | grep -nE '\b(never|only|nothing|exactly one|cannot|always|every)\b'`
  over the staged diff and **re-derive** each hit against the code beneath it.
  Across milestones 4c-1 and 4c-2 this caught eighteen instances of a comment,
  test name or sentence whose claim had outlived its code — several inside
  fixes for that same defect. Careful reading caught none of them.
- **`make test` is the gate, and its exit code is the answer.** Do not pipe it
  through `tail` or `head`; that makes the pipeline's exit code the filter's.
- **envtest runs no scheduler and no kube-controller-manager.** Pods get no
  `spec.nodeName` unless the test sets it, deleted pods vanish with no
  deletion timestamp, and a PDB's `status.disruptionsAllowed` is never
  computed — so the *eviction* half of Task 7 cannot be proved there. That is
  what runbook §12 is for; assert the object's shape in envtest and say so.
- **No field index.** Both node watches list this operator's pods and filter by
  node. An index would have to be registered once and shared by two
  controllers, and registering it twice fails at manager start — coordination
  that is not worth buying for a pod population this size.
- **`make agent-test` must stay green** and should need no extension: this
  milestone changes no agent-facing message. Run it once, at the end of Task 8,
  and report the result. If it *did* need extending, that is a finding worth
  raising rather than a step to improvise.
