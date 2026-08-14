# Milestone 4c-2: proxy rolling updates — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A spec change to a `ProxyGroup` replaces its proxy pods one at a time, with a surge pod ready before any old one stops being ready, and nobody disconnected except at the drain deadline 4c-1 already built.

**Architecture:** A pod carries a hash of the pod the operator would render for it right now; a mismatch means stale. A pure function decides, from a list of proxy views, how many pods to create and which single pod to mark as going. The `spawnery.cloud/draining-since` annotation stops meaning "in the surplus tail" and becomes the carrier of intent, so both the readiness assertion and the deletion loop derive from it rather than from a pod's position.

**Tech Stack:** Go, controller-runtime, envtest, Kubernetes pod labels and annotations.

## Global Constraints

- **Design of record:** `docs/superpowers/specs/2026-08-14-proxy-rolling-updates-design.md`. Where this plan and the spec disagree, the spec wins — except where a task says it is correcting the spec, in which case that task amends the spec in the same commit.
- **The label is exactly `spawnery.cloud/pod-hash`.** The annotation `spawnery.cloud/draining-since` keeps its name and its format (RFC3339) and changes only its meaning.
- **The condition type is exactly `ReadinessDiverged`**, declared in `api/v1alpha1/common_types.go` beside `ConditionDegraded` and `ConditionScalingLimited`.
- **The readiness-divergence grace period is exactly 60 seconds**, a constant in the controller package, not a CRD field.
- **The surge is fixed at 1.** No `maxSurge` field.
- **`surge = 1 while any pod is stale`, including one already draining; `target = replicas + surge`.** Dropping surge to 0 the moment a pod is marked makes the surplus rule mark a second pod, which drains the whole group at once.
- **`make manifests` must produce NO diff.** This milestone adds no CRD field: the hash is a pod label, the condition type is a Go constant, and `ProxyGroupStatus.Conditions` already exists.
- **`make agent-test` needs no change.** The wire contract is untouched; 4c-2 creates a drain from a new occasion.
- **Unknown counts as occupied**, everywhere — selection and deletion alike. `Registry.Lookup` returns `Snapshot{PlayersStale: true}` for a pod it has never heard of, so a zero from an unknown pod and a zero from a pod whose agent died look identical.
- **Every test whose expectations move gets its mutation run for real and the output reported.** On the last four milestones this is what caught tests whose names had outlived their fixtures; 4c-1 alone produced eleven instances of that defect class.
- **A comment asserting an ordering or a bound must name its constants**, so a reviewer can evaluate the claim rather than read it. Before committing, grep your own diff for `never`, `only`, `nothing else`, `exactly one`, `cannot` and evaluate each hit.
- **Build and test:** `nix develop -c go test ./internal/... -cover`; `nix develop -c make test` for the full suite. envtest boots no scheduler, so a deleted pod vanishes outright with no `deletionTimestamp` — a test cannot observe a Terminating pod.
- **`record.FakeRecorder` has a bounded channel**; a test that emits more events than its capacity blocks its writer and turns a failure into a ten-minute timeout.
- **Commit style:** conventional commits, scope `4c-2`. End every commit message with a blank line and:
  ```
  Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
  ```

## File structure

| File | Responsibility | Task |
|---|---|---|
| `internal/podspec/labels.go` | `LabelPodHash` | 1 |
| `internal/podspec/hash.go` (new) | `ProxyPodHash`, and stamping it | 1 |
| `internal/podspec/hash_test.go` (new) | What moves the hash and what does not | 1 |
| `internal/controller/rollout.go` (new) | `ProxyView`, `RolloutDecision`, `DecideRollout` | 2 |
| `internal/controller/rollout_test.go` (new) | The rules, as a table | 2 |
| `internal/controller/proxygroup_controller.go` | Both loops consume a set of names (3), then that set comes from `DecideRollout` (4), then the divergence condition (6) | 3, 4, 6 |
| `internal/controller/expectations.go` | A pod-shaped `observe` | 5 |
| `api/v1alpha1/common_types.go` | `ConditionReadinessDiverged` | 6 |
| `docs/known-issues.md`, `docs/handover-milestone-4.md`, `docs/runbook-milestone-4c1-evidence.md` | The upgrade hazard, what 4c-3 finds, the evidence section | 7 |

Seven tasks. 1–2 are pure and cluster-free; 3 is a behaviour-preserving refactor; 4 turns the rollout on; 5–6 are the two smaller findings; 7 is what a person needs afterwards.

---

### Task 1: The hash

**Files:**
- Modify: `internal/podspec/labels.go`
- Create: `internal/podspec/hash.go`
- Create: `internal/podspec/hash_test.go`
- Modify: `internal/podspec/proxy.go` (inside `BuildProxyPod`, before it returns)

**Interfaces:**
- Produces: `podspec.LabelPodHash` (string constant), and `podspec.ProxyPodHash(pod *corev1.Pod) (string, error)`. Task 4 calls `ProxyPodHash` on a freshly built pod and compares the result with a running pod's `LabelPodHash` label.

**Why the name is zeroed and the label excluded.** The hash must answer "would this pod be built the same way today", and a pod's own name is per-pod, not per-spec. The label is excluded because it is written *from* the hash; including it would make the value depend on itself.

- [ ] **Step 1: Add the label constant**

In `internal/podspec/labels.go`, beside `LabelOccupied`:

```go
// LabelPodHash is a digest of the pod this operator would render for its
// group right now, with the pod's own name and this label excluded. A pod
// whose label differs from the current digest is stale and gets replaced.
//
// It covers the rendered pod rather than a chosen list of spec fields, so a
// spec field added later cannot be forgotten here and silently never roll
// out. The cost is recorded in docs/known-issues.md: a change to the
// rendering code moves the digest for every group, so an operator upgrade
// that touches podspec rolls the fleet.
const LabelPodHash = "spawnery.cloud/pod-hash"
```

- [ ] **Step 2: Write the failing tests**

Create `internal/podspec/hash_test.go`. Read `internal/podspec/proxy_test.go` first and use its `buildProxy` helper's fixture shape for the `Network` and `ProxyGroup` values; the tests below assume a helper that returns a group you can mutate.

```go
func TestPodHashIsStableAcrossBuilds(t *testing.T) {
	net, group := proxyFixture(t)
	a, err := BuildProxyPod(net, group, "gateway-aaaa", "operator:9443")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	b, err := BuildProxyPod(net, group, "gateway-bbbb", "operator:9443")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a.Labels[LabelPodHash] != b.Labels[LabelPodHash] {
		t.Errorf("hash differs between two builds of one spec: %q vs %q — the pod name must not reach it",
			a.Labels[LabelPodHash], b.Labels[LabelPodHash])
	}
}

func TestPodHashMovesWithTheImage(t *testing.T) {
	net, group := proxyFixture(t)
	before, err := BuildProxyPod(net, group, "gateway-aaaa", "operator:9443")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	group.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	after, err := BuildProxyPod(net, group, "gateway-aaaa", "operator:9443")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if before.Labels[LabelPodHash] == after.Labels[LabelPodHash] {
		t.Error("hash unchanged after the image changed; a new image would never roll out")
	}
}

func TestPodHashIgnoresReplicas(t *testing.T) {
	net, group := proxyFixture(t)
	group.Spec.Replicas = 2
	before, err := BuildProxyPod(net, group, "gateway-aaaa", "operator:9443")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	group.Spec.Replicas = 5
	after, err := BuildProxyPod(net, group, "gateway-aaaa", "operator:9443")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if before.Labels[LabelPodHash] != after.Labels[LabelPodHash] {
		t.Error("hash moved when only replicas changed; scaling would trigger a full replacement")
	}
}

func TestPodHashDoesNotIncludeItself(t *testing.T) {
	net, group := proxyFixture(t)
	pod, err := BuildProxyPod(net, group, "gateway-aaaa", "operator:9443")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	again, err := ProxyPodHash(pod)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if again != pod.Labels[LabelPodHash] {
		t.Errorf("re-hashing a stamped pod gives %q, want the stamped %q — the label must be excluded from its own input",
			again, pod.Labels[LabelPodHash])
	}
}
```

- [ ] **Step 3: Run them and watch them fail**

Run: `nix develop -c go test ./internal/podspec/ -run TestPodHash -v`
Expected: FAIL, `undefined: ProxyPodHash` and `undefined: LabelPodHash` if Step 1 is not in yet.

- [ ] **Step 4: Implement the hash**

Create `internal/podspec/hash.go`:

```go
package podspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
)

// ProxyPodHash digests the pod as this operator would render it, ignoring the
// two things that are not part of "how it was rendered": the pod's own name,
// which is per-pod, and LabelPodHash itself, which is written from this value
// and would otherwise feed itself.
//
// encoding/json sorts map keys, so labels, annotations and node selectors
// serialise in a fixed order and the digest does not flap between passes.
func ProxyPodHash(pod *corev1.Pod) (string, error) {
	subject := pod.DeepCopy()
	subject.Name = ""
	subject.GenerateName = ""
	delete(subject.Labels, LabelPodHash)

	encoded, err := json.Marshal(subject)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8]), nil
}
```

- [ ] **Step 5: Stamp it in `BuildProxyPod`**

At the end of `BuildProxyPod` in `internal/podspec/proxy.go`, immediately before it returns the pod:

```go
	// Stamped here rather than by the caller: every proxy pod this operator
	// creates must carry it, and a caller that forgot would build a pod the
	// rollout reads as stale on the very next pass.
	hash, err := ProxyPodHash(pod)
	if err != nil {
		return nil, err
	}
	pod.Labels[LabelPodHash] = hash
```

If `BuildProxyPod` returns a value rather than a pointer at that point, adapt the two lines to its actual shape rather than changing the function's signature.

- [ ] **Step 6: Run the tests and the package**

Run: `nix develop -c go test ./internal/podspec/ -v`
Expected: PASS, including the pre-existing proxy tests. If an existing golden test compares the full label map, it will now see the new label — update it and say so in your report; do not delete the assertion.

- [ ] **Step 7: Run the mutations and report the victims by name**

Each in a throwaway worktree (`git worktree add --detach /tmp/t1-mut`), removed after; never in the shared tree.

1. Do not zero `subject.Name` → `TestPodHashIsStableAcrossBuilds` must fail.
2. Do not delete `LabelPodHash` from `subject.Labels` → `TestPodHashDoesNotIncludeItself` must fail.
3. Skip the stamp in `BuildProxyPod` → all four must fail.

- [ ] **Step 8: Commit**

```bash
git add internal/podspec/
git commit -m "feat(4c-2): a proxy pod carries a digest of how it was rendered"
```

---

### Task 2: The rollout decision, as a pure function

**Files:**
- Create: `internal/controller/rollout.go`
- Create: `internal/controller/rollout_test.go`

**Interfaces:**
- Produces: `ProxyView`, `RolloutDecision` and `DecideRollout(pods []ProxyView, replicas int32) RolloutDecision`. Task 3 constructs `ProxyView`s; Task 4 calls `DecideRollout`.

**Why a pure function.** `DecideSize` in `internal/controller/scaling.go` is the precedent: the rules live in a function with no client and no cluster, table-tested, and the reconciler only carries out what it returns. Read `scaling.go` before writing this — match its voice and its comment density.

- [ ] **Step 1: Write the types and the failing table**

Create `internal/controller/rollout.go` with only the types, so the test compiles:

```go
package controller

import "time"

// ProxyView is what the rollout decision needs to know about one proxy pod.
// It is deliberately not a corev1.Pod: the rules are about staleness,
// occupancy and intent, and a view keeps them testable without a cluster.
type ProxyView struct {
	Name         string
	Stale        bool
	Ready        bool
	Draining     bool
	Players      int32
	PlayersStale bool
	CreatedAt    time.Time
}

// RolloutDecision is what one pass should do: how many pods to create, and
// which pods to mark as going. Drain carries names rather than indices,
// because after this milestone a pod's fate no longer depends on its position.
type RolloutDecision struct {
	Create int32
	Drain  []string
}
```

Create `internal/controller/rollout_test.go`:

```go
package controller

import (
	"testing"
	"time"
)

func at(min int) time.Time {
	return time.Date(2026, 8, 14, 12, min, 0, 0, time.UTC)
}

func TestDecideRollout(t *testing.T) {
	tests := []struct {
		name     string
		pods     []ProxyView
		replicas int32
		want     RolloutDecision
	}{
		{
			name:     "cold start creates the whole group",
			pods:     nil,
			replicas: 2,
			want:     RolloutDecision{Create: 2},
		},
		{
			name: "a group at size with nothing stale does nothing",
			pods: []ProxyView{
				{Name: "a", Ready: true, CreatedAt: at(0)},
				{Name: "b", Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
		{
			name: "all stale: the surge pod is created before anything is marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{Create: 1},
		},
		{
			name: "the surge pod is not ready yet, so nothing is marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, CreatedAt: at(1)},
				{Name: "c", Ready: false, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
		{
			name: "the surge pod is ready, so exactly one stale pod is marked",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, Players: 3, CreatedAt: at(0)},
				{Name: "b", Stale: true, Ready: true, Players: 1, CreatedAt: at(1)},
				{Name: "c", Ready: true, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"b"}},
		},
		{
			name: "one already draining: no second replacement begins",
			pods: []ProxyView{
				{Name: "a", Stale: true, Ready: true, CreatedAt: at(0)},
				{Name: "b", Stale: true, Draining: true, Players: 1, CreatedAt: at(1)},
				{Name: "c", Ready: true, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
		{
			name: "scale-down takes the emptiest",
			pods: []ProxyView{
				{Name: "a", Ready: true, Players: 4, CreatedAt: at(0)},
				{Name: "b", Ready: true, Players: 0, CreatedAt: at(1)},
				{Name: "c", Ready: true, Players: 2, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"b"}},
		},
		{
			name: "an untrusted count sorts last, even at zero",
			pods: []ProxyView{
				{Name: "a", Ready: true, Players: 0, PlayersStale: true, CreatedAt: at(0)},
				{Name: "b", Ready: true, Players: 2, CreatedAt: at(1)},
				{Name: "c", Ready: true, Players: 5, CreatedAt: at(2)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"b"}},
		},
		{
			name: "equal counts break by age, oldest first",
			pods: []ProxyView{
				{Name: "young", Ready: true, Players: 1, CreatedAt: at(9)},
				{Name: "old", Ready: true, Players: 1, CreatedAt: at(1)},
				{Name: "mid", Ready: true, Players: 1, CreatedAt: at(5)},
			},
			replicas: 2,
			want:     RolloutDecision{Drain: []string{"old"}},
		},
		{
			name: "a scale-down during a rollout takes the stale pod first",
			pods: []ProxyView{
				{Name: "stale-full", Stale: true, Ready: true, Players: 9, CreatedAt: at(0)},
				{Name: "current-empty", Ready: true, Players: 0, CreatedAt: at(1)},
			},
			replicas: 1,
			want:     RolloutDecision{Drain: []string{"stale-full"}},
		},
		{
			name: "a cancelled rollout creates nothing and marks nothing",
			pods: []ProxyView{
				{Name: "a", Ready: true, CreatedAt: at(0)},
				{Name: "b", Ready: true, CreatedAt: at(1)},
			},
			replicas: 2,
			want:     RolloutDecision{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRollout(tc.pods, tc.replicas)
			if got.Create != tc.want.Create {
				t.Errorf("Create = %d, want %d", got.Create, tc.want.Create)
			}
			if len(got.Drain) != len(tc.want.Drain) {
				t.Fatalf("Drain = %v, want %v", got.Drain, tc.want.Drain)
			}
			for i := range got.Drain {
				if got.Drain[i] != tc.want.Drain[i] {
					t.Errorf("Drain[%d] = %q, want %q", i, got.Drain[i], tc.want.Drain[i])
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/controller/ -run TestDecideRollout -v`
Expected: FAIL with `undefined: DecideRollout`.

- [ ] **Step 3: Implement the decision**

Append to `internal/controller/rollout.go`:

```go
import "sort"

// DecideRollout decides one pass: how many pods to create, and which pods to
// mark as going.
//
// The target size is replicas plus a surge of 1 while any pod is stale --
// including one that is already draining. Dropping the surge the moment a pod
// is marked would leave the group one over its target, and the surplus rule
// below would mark a second pod on the same pass: a rolling update that drains
// the whole group at once, which is exactly what one-at-a-time forbids.
func DecideRollout(pods []ProxyView, replicas int32) RolloutDecision {
	var stale, draining int32
	for _, p := range pods {
		if p.Stale {
			stale++
		}
		if p.Draining {
			draining++
		}
	}

	var surge int32
	if stale > 0 {
		surge = 1
	}
	target := replicas + surge
	total := int32(len(pods))

	if total < target {
		return RolloutDecision{Create: target - total}
	}

	// One at a time. The cycle advances only when the deletion loop removes
	// the draining pod, which happens when it is empty or when its deadline
	// expires.
	if draining > 0 {
		return RolloutDecision{}
	}

	if total > target {
		return RolloutDecision{Drain: pick(pods, total-target)}
	}

	// At target with stale pods left: mark one, but only once a
	// current-generation pod beyond replicas is serving, so ready capacity
	// never falls below replicas.
	if stale > 0 && readyCurrentBeyond(pods, replicas) {
		return RolloutDecision{Drain: pick(staleOnly(pods), 1)}
	}
	return RolloutDecision{}
}

// readyCurrentBeyond reports whether the surge pod has arrived: more
// current-generation pods are Ready than the group needs, so withdrawing one
// stale pod leaves replicas of them still serving.
func readyCurrentBeyond(pods []ProxyView, replicas int32) bool {
	var ready int32
	for _, p := range pods {
		if !p.Stale && p.Ready && !p.Draining {
			ready++
		}
	}
	return ready > replicas-int32(countStaleReady(pods))
}

func countStaleReady(pods []ProxyView) int {
	n := 0
	for _, p := range pods {
		if p.Stale && p.Ready && !p.Draining {
			n++
		}
	}
	return n
}

func staleOnly(pods []ProxyView) []ProxyView {
	out := make([]ProxyView, 0, len(pods))
	for _, p := range pods {
		if p.Stale {
			out = append(out, p)
		}
	}
	return out
}

// pick returns the n pods that should go, in order. Stale before current,
// because a stale pod has to go regardless and taking a current one first
// would drain two pods for one replacement. Then fewest players, because the
// emptiest finishes soonest and disconnects fewest people at the deadline. An
// untrusted count sorts last on the repository's own rule -- unknown counts as
// occupied, since a pod whose agent stream is down may hold players nobody can
// see. Age breaks ties so the order is deterministic and a test can name the
// pod it expects rather than counting survivors.
func pick(pods []ProxyView, n int32) []string {
	candidates := append([]ProxyView(nil), pods...)
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Stale != b.Stale {
			return a.Stale
		}
		if a.PlayersStale != b.PlayersStale {
			return !a.PlayersStale
		}
		if a.Players != b.Players {
			return a.Players < b.Players
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	out := make([]string, 0, n)
	for i := int32(0); i < n && int(i) < len(candidates); i++ {
		out = append(out, candidates[i].Name)
	}
	return out
}
```

- [ ] **Step 4: Run the table**

Run: `nix develop -c go test ./internal/controller/ -run TestDecideRollout -v`
Expected: PASS, every subtest.

**If `readyCurrentBeyond` does not produce the expected answers, simplify it rather than tuning it.** The property it must express is "a current-generation pod is serving that the group does not already need". A count of ready current pods greater than `replicas` minus the ready stale pods is one way; a direct `total > replicas && readyCurrent >= 1` is another. Pick the one you can state in a comment without a second clause, make the table pass, and say in your report which you chose and why.

- [ ] **Step 5: Run the mutations and report the victims by name**

In a throwaway worktree:

1. `surge` computed as `1` only when no pod is draining → "one already draining: no second replacement begins" must fail.
2. Remove the `draining > 0` guard → the same test must fail.
3. Drop the `readyCurrentBeyond` check → "the surge pod is not ready yet" must fail.
4. Sort by `Players` without the `PlayersStale` clause → "an untrusted count sorts last" must fail.
5. Sort stale *after* current → "a scale-down during a rollout takes the stale pod first" must fail.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/rollout.go internal/controller/rollout_test.go
git commit -m "feat(4c-2): decide the rollout as a pure function over proxy views"
```

---

### Task 3: The annotation becomes the carrier of intent

**Files:**
- Modify: `internal/controller/proxygroup_controller.go` (`reconcileReplicas`, and the deletion loop below it)
- Test: `internal/controller/proxygroup_controller_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: both loops read a `map[string]bool` of pod names that should be draining, computed once per pass. Task 4 replaces how that map is computed.

**This task changes no behaviour.** It is the refactor that makes Task 4 possible: today both the readiness assertion and the deletion loop derive from a pod's *index*, and a stale pod is not necessarily in the tail. The existing tests are the net — they must all still pass, unchanged.

**One trap to avoid.** Do not derive the set by re-reading the annotation from the pod: on the pass that first marks a pod, the annotation is not there yet when the readiness loop runs, so readiness would lag by a pass. Compute the set first, then use it for both the assertion and the mark.

- [ ] **Step 1: Compute the set once, before both loops**

In `reconcileReplicas`, after the create loop and before the readiness loop:

```go
	// Which pods are going, decided once and used by both loops below. Today
	// this is still the surplus tail; Task 4 replaces the right-hand side with
	// the rollout decision, and nothing else here has to move.
	//
	// Derived rather than re-read from the annotation: on the pass that first
	// marks a pod, the annotation is not on it yet when the readiness loop
	// runs, and readiness would lag a whole pass behind the mark.
	leaving := make(map[string]bool, len(pods))
	for i := range pods {
		if i >= int(group.Spec.Replicas) {
			leaving[pods[i].Name] = true
		}
	}
```

- [ ] **Step 2: Make the readiness loop consume it**

Replace the body of the readiness loop:

```go
	for i := range pods {
		going := leaving[pods[i].Name]
		if err := r.Proxies.SetReady(ctx, string(pods[i].UID), !going); err != nil {
			return err
		}
		if err := r.markDraining(ctx, &pods[i], going); err != nil {
			return err
		}
	}
```

- [ ] **Step 3: Make the deletion loop consume it**

Replace the index-based `for i := len(pods) - 1; i >= int(group.Spec.Replicas); i--` header with an iteration over the pods that are going, keeping the body — the `players == 0 && !snap.PlayersStale` case, the `expired` case with its `Warning` event, and the `default` — exactly as it stands:

```go
	for i := range pods {
		if !leaving[pods[i].Name] {
			continue
		}
		pod := &pods[i]
		// ... body unchanged ...
	}
```

Update the loop's own comment: it no longer walks a tail, and the sentence that says so must go with it.

- [ ] **Step 4: Run the whole package**

Run: `nix develop -c go test ./internal/controller/ -count=1`
Expected: PASS, with no test edited. **If any test needed editing, stop and report it** — this task is behaviour-preserving, and a test that had to change means it did not preserve behaviour.

- [ ] **Step 5: Prove the refactor is load-bearing**

In a throwaway worktree, make `leaving` always empty. Expected: `TestProxyGroupScalesDown` and `TestADrainingProxyIsDeletedOnceEmpty` fail. Report the victims by name.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/proxygroup_controller.go
git commit -m "refactor(4c-2): a pod goes because it was chosen, not because of where it sits"
```

---

### Task 4: The rollout goes live

**Files:**
- Modify: `internal/controller/proxygroup_controller.go`
- Test: `internal/controller/proxygroup_controller_test.go`

**Interfaces:**
- Consumes: `podspec.ProxyPodHash` (Task 1), `DecideRollout` / `ProxyView` (Task 2), the `leaving` map (Task 3).

- [ ] **Step 1: Build the views and call the decision**

Replace the create loop and the `leaving` computation with:

```go
	desired, err := podspec.BuildProxyPod(network, group, "", r.AgentEndpoint)
	if err != nil {
		// The one place in this function where a single failure stops the
		// pass: without the current digest no pod's staleness can be judged,
		// so continuing would either roll nothing or roll everything.
		return err
	}
	wantHash := desired.Labels[podspec.LabelPodHash]

	views := make([]ProxyView, 0, len(pods))
	for i := range pods {
		snap := r.Agents.Lookup(string(pods[i].UID))
		_, dated := drainingSince(&pods[i])
		views = append(views, ProxyView{
			Name:         pods[i].Name,
			Stale:        pods[i].Labels[podspec.LabelPodHash] != wantHash,
			Ready:        isPodReady(&pods[i]),
			Draining:     dated,
			Players:      snap.Players,
			PlayersStale: snap.PlayersStale,
			CreatedAt:    pods[i].CreationTimestamp.Time,
		})
	}

	decision := DecideRollout(views, group.Spec.Replicas)

	for i := int32(0); i < decision.Create; i++ {
		pod, err := podspec.BuildProxyPod(network, group, NewProxyName(group.Name), r.AgentEndpoint)
		if err != nil {
			return err
		}
		if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	leaving := make(map[string]bool, len(decision.Drain))
	for _, name := range decision.Drain {
		leaving[name] = true
	}
	// A pod already carrying the mark keeps it: the annotation is written once
	// and never moved, so a choice made on an earlier pass cannot be revisited
	// when the player counts shift.
	for i := range pods {
		if _, dated := drainingSince(&pods[i]); dated {
			leaving[pods[i].Name] = true
		}
	}
```

**`BuildProxyPod` tolerates an empty name — checked while writing this plan.** It validates `group.Spec.Image` and `agentEndpoint` and returns an error naming the group for either, but it never inspects `name`, and Task 1's hash zeroes the name anyway. So the first call is safe as written. If that changes under you, add a `podspec.DesiredProxyHash(net, group, agentEndpoint) (string, error)` helper in Task 1's file rather than passing a throwaway name, and say so in your report.

- [ ] **Step 2: Write the failing envtest tests**

Add to `internal/controller/proxygroup_controller_test.go`. Read the neighbouring tests first and use the fixture's own helpers (`f.createProxyGroup`, `f.reconcileProxyGroup`, `f.proxyPods`, `f.reportProxyPlayers`, `f.setProxyReplicas`, `f.pod`) rather than new ones.

```go
// TestAStaleProxyIsReplacedOneAtATime is the milestone's subject: a spec
// change replaces the group's proxies without the ready count ever dipping.
func TestAStaleProxyIsReplacedOneAtATime(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	if len(before) != 2 {
		t.Fatalf("proxy pods = %d, want 2", len(before))
	}
	for i := range before {
		f.markPodReady(t, &before[i])
	}

	// A spec change every pod's digest disagrees with.
	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.client.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")
	after := f.proxyPods("gateway")
	if len(after) != 3 {
		t.Fatalf("proxy pods = %d after the spec change, want 3 — the surge pod must exist before anything is marked", len(after))
	}
	for i := range after {
		if _, dated := drainingSince(&after[i]); dated {
			t.Errorf("pod %s was marked while the surge pod is still unready; ready capacity would dip below replicas", after[i].Name)
		}
	}
}

// TestTheSurgePodMustBeReadyBeforeAnyPodIsMarked states the property the
// surge exists for, and it is the one a pod count alone cannot show.
func TestTheSurgePodMustBeReadyBeforeAnyPodIsMarked(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	before := f.proxyPods("gateway")
	for i := range before {
		f.markPodReady(t, &before[i])
	}
	g := f.proxyGroup("gateway")
	g.Spec.Image = "ghcr.io/spawnery/velocity:3.5.2-0.2.0"
	if err := f.client.Update(f.ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")

	// Make the surge pod ready and reconcile again.
	pods := f.proxyPods("gateway")
	for i := range pods {
		if _, dated := drainingSince(&pods[i]); dated {
			t.Fatal("a pod was marked before the surge pod turned ready")
		}
		f.markPodReady(t, &pods[i])
	}
	f.reconcileProxyGroup(r, "gateway")

	marked := 0
	for _, p := range f.proxyPods("gateway") {
		if _, dated := drainingSince(&p); dated {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("marked = %d, want exactly 1 — a rolling update replaces one proxy at a time", marked)
	}
}

// TestChangingReplicasAloneRollsNothing is the reason staleness is a digest of
// the rendered pod rather than metadata.generation.
func TestChangingReplicasAloneRollsNothing(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")
	for i, p := range f.proxyPods("gateway") {
		_ = i
		f.markPodReady(t, &p)
	}

	f.setProxyReplicas("gateway", 3)
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 3 {
		t.Fatalf("proxy pods = %d, want 3", len(pods))
	}
	for _, p := range pods {
		if _, dated := drainingSince(&p); dated {
			t.Errorf("pod %s was marked draining after a replicas change; scaling must not roll the group", p.Name)
		}
	}
}
```

If the fixture has no `markPodReady`, write one next to the existing helpers that patches the pod's `Ready` condition to `True`, and say in your report that you added it.

- [ ] **Step 3: Run them and watch them fail**

Run: `nix develop -c go test ./internal/controller/ -run 'TestAStaleProxy|TestTheSurgePod|TestChangingReplicasAlone' -v`
Expected: FAIL — before Step 1 lands, no pod is ever stale, so no surge pod is created.

- [ ] **Step 4: Run the whole package**

Run: `nix develop -c go test ./internal/controller/ -count=1`
Expected: PASS. Existing scale-down tests may move because selection changed from newest-first to emptiest-first — **that is an expectation change and needs its own mutation proof.** Report which tests moved and why.

- [ ] **Step 5: Run the mutations and report the victims by name**

1. `Stale` always false → all three new tests fail.
2. Create the surge pod but mark a stale pod on the same pass without the readiness check → `TestTheSurgePodMustBeReadyBeforeAnyPodIsMarked` fails.
3. Drop the "a pod already carrying the mark keeps it" loop → report what fails; if nothing does, say so plainly and write the test that would.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/
git commit -m "feat(4c-2): a spec change replaces a group's proxies, one at a time"
```

---

### Task 5: `expectations` for the proxy create path

**Files:**
- Modify: `internal/controller/expectations.go`
- Modify: `internal/controller/proxygroup_controller.go` (`pods()` and the create path)
- Test: `internal/controller/expectations_test.go`

**Interfaces:**
- Produces: `func (e *expectations) observePods(group string, pods []corev1.Pod)`.

**Why now.** A rollout creates a pod per replacement rather than only at scale-up, so the create path racing the informer cache goes from rare to routine. `internal/controller/expectations.go` is the mechanism — name-keyed reservations with a 30s TTL, the ReplicaSet controller's own approach — and only *create* and *delete* apply; there is no proxy analogue of `expectationRetire`.

- [ ] **Step 1: Write the failing test**

```go
func TestObservePodsClearsACreateReservation(t *testing.T) {
	e := newExpectations(func() time.Time { return time.Unix(0, 0) })
	e.expectCreated("gateway", "gateway-aaaa")

	pending, _, _ := e.pending("gateway")
	if pending != 1 {
		t.Fatalf("pending = %d, want 1 before the pod appears", pending)
	}

	e.observePods("gateway", []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "gateway-aaaa"}}})

	pending, _, _ = e.pending("gateway")
	if pending != 0 {
		t.Errorf("pending = %d, want 0 once the cache shows the pod", pending)
	}
}

func TestObservePodsClearsADeleteReservationWhenThePodIsGone(t *testing.T) {
	e := newExpectations(func() time.Time { return time.Unix(0, 0) })
	e.expectDeleted("gateway", "gateway-aaaa")

	e.observePods("gateway", nil)

	pending, leaving, _ := e.pending("gateway")
	if pending != 0 || len(leaving) != 0 {
		t.Errorf("pending = %d, leaving = %v, want both empty once the pod is gone", pending, leaving)
	}
}
```

Match the constructor to whatever `expectations_test.go` already uses; if it builds the struct directly rather than through a `newExpectations`, do the same.

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/controller/ -run TestObservePods -v`
Expected: FAIL with `undefined: observePods`.

- [ ] **Step 3: Implement it**

Add to `internal/controller/expectations.go`, beside `observe`:

```go
// observePods is observe for pods. A proxy has no retire reservation -- only
// the ServerGroup controller retires anything -- so this handles create and
// delete and nothing else, rather than sharing a generic method that would
// have to explain an absent third case to half its callers.
func (e *expectations) observePods(group string, pods []corev1.Pod) {
	e.mu.Lock()
	defer e.mu.Unlock()

	m := e.byGroup[group]
	if len(m) == 0 {
		return
	}
	seen := make(map[string]bool, len(pods))
	for i := range pods {
		seen[pods[i].Name] = true
	}

	now := e.now()
	for name, exp := range m {
		if !now.Before(exp.expires) {
			delete(m, name)
			continue
		}
		switch exp.kind {
		case expectationCreate:
			if seen[name] {
				delete(m, name)
			}
		case expectationDelete:
			if !seen[name] {
				delete(m, name)
			}
		}
	}
	if len(m) == 0 {
		delete(e.byGroup, group)
	}
}
```

- [ ] **Step 4: Wire it into the proxy controller**

In `pods()`, after the list and sort, call `r.Expectations.observePods(group.Name, pods)`. In the create path in `reconcileReplicas`, call `expectCreated` with the pod's name before `r.Create`, and in the delete path call `expectDeleted` before `r.Delete`. Read how `ServerGroupReconciler` does both and follow it exactly — including where it reads `pending` to decide whether to hold off.

- [ ] **Step 5: Run the package**

Run: `nix develop -c go test ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 6: Run the mutation**

Remove the `expectationCreate` arm from `observePods`. Expected: `TestObservePodsClearsACreateReservation` fails and nothing else does.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/
git commit -m "feat(4c-2): reserve a proxy create against the cache that has not caught up"
```

---

### Task 6: `ReadinessDiverged`

**Files:**
- Modify: `api/v1alpha1/common_types.go`
- Modify: `internal/controller/proxygroup_controller.go`
- Test: `internal/controller/proxygroup_controller_test.go`

**Interfaces:**
- Produces: `spawneryv1alpha1.ConditionReadinessDiverged`.

**What it is for.** `Resync` already re-sends the last asserted readiness every 30 seconds, so a divergence caused by a lost message heals itself. What survives that is an agent that received the message and did not act — a broken build, a leaked socket — and re-sending cannot fix it. What an operator needs is to be told, before the deadline disconnects players from a proxy that never stopped taking them.

- [ ] **Step 1: Add the constant**

In `api/v1alpha1/common_types.go`, beside `ConditionScalingLimited`:

```go
	// ConditionReadinessDiverged is true while a proxy pod's actual readiness
	// has disagreed with the readiness the operator asserted for longer than
	// the grace period. The operator does not try to repair it: a divergence
	// from a lost message is already corrected by the next resync, so what
	// remains is an agent that heard the instruction and did not act on it.
	ConditionReadinessDiverged = "ReadinessDiverged"
```

- [ ] **Step 2: Write the failing test**

```go
func TestAProxyThatIgnoresItsWithdrawalIsReported(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	for i := range pods {
		f.markPodReady(t, &pods[i])
	}
	f.reportProxyPlayers(t, pods[1], 1)

	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	// The pod stays Ready even though its readiness was withdrawn: an agent
	// that heard the instruction and did not act. Nothing is reported yet.
	f.reconcileProxyGroup(r, "gateway")
	g := f.proxyGroup("gateway")
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Fatal("reported before the grace period elapsed")
	}

	f.clock.Advance(readinessDivergenceGrace + time.Second)
	f.reconcileProxyGroup(r, "gateway")

	g = f.proxyGroup("gateway")
	if !meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionReadinessDiverged) {
		t.Error("a proxy that stayed Ready after its readiness was withdrawn was not reported")
	}
	if !containsEvent(rec, "Warning", "ReadinessDiverged") {
		t.Error("no Warning event named the pod")
	}
}
```

Use the file's existing `containsEvent` helper rather than a new one; read its signature first. Keep `record.NewFakeRecorder`'s capacity above the number of events this test can produce.

- [ ] **Step 3: Run it and watch it fail**

Run: `nix develop -c go test ./internal/controller/ -run TestAProxyThatIgnores -v`
Expected: FAIL with `undefined: readinessDivergenceGrace`.

- [ ] **Step 4: Implement it**

Add the constant near `defaultProxyDrainTimeout`:

```go
// readinessDivergenceGrace is how long a pod's actual readiness may disagree
// with the asserted one before the group says so. It must clear both known
// delays: the kubelet's probe takes 10 to 15 seconds to flip a condition
// (period 5s times failure threshold 3), and Fleet.Resync re-asserts every 30
// seconds. 60 clears both with margin.
const readinessDivergenceGrace = 60 * time.Second
```

Track the first time each pod was seen diverging, in a per-reconciler map keyed by pod UID, cleared when the pod agrees again or disappears. Set the condition and fire the event on the flank, comparing `meta.IsStatusConditionTrue` before and after `meta.SetStatusCondition` — that call only moves `lastTransitionTime` on an actual change of status, which is what makes the flank detectable. Read how `ServerGroupReconciler` sets `ScalingLimited` and follow it.

- [ ] **Step 5: Run the package**

Run: `nix develop -c go test ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 6: Run the mutations**

1. Report without the grace period → the test's "reported before the grace period elapsed" assertion fails.
2. Set the condition but skip the flank comparison, firing an event every pass → write the assertion that catches it if none exists, or report that the FakeRecorder's capacity catches it by blocking.

- [ ] **Step 7: Commit**

```bash
git add api/v1alpha1/common_types.go internal/controller/
git commit -m "feat(4c-2): say so when a proxy's readiness disagrees with the operator's"
```

---

### Task 7: The paperwork

**Files:**
- Modify: `docs/known-issues.md`
- Modify: `docs/handover-milestone-4.md`
- Modify: `docs/runbook-milestone-4c1-evidence.md`

- [ ] **Step 1: Record the upgrade hazard**

`docs/known-issues.md` gains a "From milestone 4c-2" section whose first entry is the accepted cost of hashing the rendered pod: **a change to the rendering code moves the digest for every group without any spec being edited, so the next operator upgrade finds the whole fleet stale and rolls it, disconnecting whoever is left at each drain deadline.** Say what an operator would see, not only what is true, and say what to do about it — that a rollout is visible in advance as pods whose `spawnery.cloud/pod-hash` label differs from a freshly rendered pod's, and that `spec.drain.timeoutSeconds` is the knob that bounds the damage.

Compare it to the 4b entry above it (`metadata.generation` moving on every edit) and say why 4c-2 chose the other trade: for a `ProxyGroup`, `replicas` changes are routine, so a generation rule would turn every scale-up into a full replacement.

- [ ] **Step 2: Record what 4c-3 finds**

`docs/handover-milestone-4.md` gains a "4c-2 has landed" section in the shape its 4a, 4b, 4d and 4c-1 sections use. Cover: staleness as a digest of the rendered pod and why not the generation; the surge rule and why `surge` stays 1 while a marked pod still exists; that the annotation is now the carrier of intent, so nothing about a pod's fate depends on its position; the unified selection rule; `ReadinessDiverged` and why it reports rather than repairs; and that 4c-3's node drain depends on none of it.

- [ ] **Step 3: Extend the runbook**

`docs/runbook-milestone-4c1-evidence.md` gains a section after §10: with a client connected, change the group's image and confirm the session survives the replacement. It must say which pod to watch, what the `pod-hash` label should look like before and after, that ready capacity should never show fewer than `replicas` pods `1/1`, and what a correct end state is — `replicas` pods, all on the new digest, none marked.

Reuse §7's log command and its take-the-most-recent-by-timestamp rule rather than restating them; that rule earned itself in both 4c-1 runs.

- [ ] **Step 4: Commit**

```bash
git add docs/
git commit -m "docs(4c-2): record the upgrade hazard, and what 4c-3 finds"
```

---

## Before the branch is merged

- [ ] `nix develop -c make test` — green.
- [ ] `nix develop -c make agent-test` — five phases green. The wire contract is unchanged, so this is a regression check rather than new evidence.
- [ ] `nix develop -c make image-test` and `make image-repro` — green.
- [ ] `nix develop -c make manifests` — **no diff**.
- [ ] `git diff --name-only master...HEAD` — `internal/podspec`, `internal/controller`, `api/v1alpha1` and `docs/`. Nothing else.
- [ ] One whole-branch review before merge. On the last four milestones it found what no task-scoped review could — most recently a race whose permanence came from a memo in a different task than the race. The equivalent risk here is the interaction between the digest, the surge rule and the annotation that is now write-once *and* load-bearing for selection: no single task exercises a group whose spec changes twice while a pod is already marked.
- [ ] The evidence section from Task 7, driven with a real client.

## Self-review

**Spec coverage.** §3.1 → Task 1. §3.2 → Task 2, wired in Task 4. §3.3 → Task 3. §3.4 → Task 2's `pick`. §3.5 → nothing, correctly: the deadline is 4c-1's and is reused unchanged. §3.6 → Task 6. §3.7 → Task 5. §4 → nothing, correctly. §5 → Task 4's `BuildProxyPod` error path, and 4c-1's existing tolerance elsewhere. §6 → each task's tests plus the pre-merge list. §7 → criteria 1–4 and 6–7 are Tasks 2 and 4; criterion 5 is Task 7's evidence section; criterion 8 is Task 6; criterion 9 is Task 5; criterion 10 is the pre-merge list.

**One risk this plan cannot remove.** `readyCurrentBeyond` in Task 2 is the only expression here I could not state in one clause, and Step 4 says so rather than pretending otherwise: the implementer is told to simplify it to something they can comment without a second clause, and to report which form they chose. An ordering claim nobody can evaluate is the shape this project has produced eleven times.

**Placeholders:** none. Every step carries its code or names the exact file to read first.

**Type consistency:** `ProxyView` and `RolloutDecision` are defined in Task 2 and consumed in Task 4. `podspec.LabelPodHash` and `podspec.ProxyPodHash` are defined in Task 1 and consumed in Task 4. `observePods` is defined in Task 5 and used there. `readinessDivergenceGrace` and `ConditionReadinessDiverged` are defined and used in Task 6. Task 4 depends on `BuildProxyPod` accepting an empty name and says what to do if it does not.
