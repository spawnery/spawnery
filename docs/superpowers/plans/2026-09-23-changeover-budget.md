# Changeover Budget Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `Network.spec.update.maxConcurrentChangeovers` caps how many server and proxy groups of a network change over at once; a group that must wait keeps its stale servers and gets no cold start or surge pod until a place is free.

**Architecture:** Each group publishes its own changeover state in `status.changeover` (`""`, `Waiting`, `Begun`). A pure function `AdmitChangeovers` turns the network's group states and the budget into the set of groups that may change over. The ServerGroup reconciler feeds the result into `DecideSize` (a refused cold start, through the existing `ColdStartBlocked` path), the ProxyGroup reconciler into `DecideRollout` (no surge). The Network reconciler exports two gauges.

**Tech Stack:** Go, controller-runtime, kubebuilder markers/CEL, envtest, Prometheus client.

**Spec:** `docs/superpowers/specs/2026-09-23-changeover-budget-design.md`

## Global Constraints

- All commands run in the Nix dev shell: `nix develop -c <cmd>`. On the development VM (8 cores, 12 GB) envtest packages run with `-p 1`; `internal/controller` alone takes ~85 s.
- After any change to API types or markers: `nix develop -c make manifests generate` and commit the generated files (`config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `zz_generated.deepcopy.go`, `docs/reference/crds.md`, and `docs/reference/metrics-and-alerts.md` once metrics change).
- The hash goldens in `internal/podspec/hash_golden_test.go` must not move. Nothing here feeds a pod.
- Unset `maxConcurrentChangeovers` means no cap; every existing test keeps passing unchanged in behaviour.
- New files carry the header `Copyright paul_wtf.` like their neighbours. Comments only where the code cannot say it.
- The repository is public: no names of any real installation in code, tests, docs or commits.
- Commits: Conventional Commits, GPG-signed, trailers `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01JQ1B3g7MgNUnuusWCn8aW6`.
- Work on branch `feat/changeover-budget` (worktree already exists, spec committed there).

## Review Focus

- A group whose own changeover finishes in this pass must not keep reporting `Begun`, or it holds its place forever — Task 4 pins `status.changeover` clearing to `""`.
- A failing group (`BackingOff` or `Degraded` True) that has begun must release its place — Task 2 table.
- Budget unset (nil) must behave exactly like today for server and proxy groups — Tasks 3 and 5 run the existing tables with `admitted: true` and add a nil-budget case.
- A proxy group and a server group with the same name are two entries — Task 2 table.
- A waiting group with players on its stale servers must not lose or drain any of them — Task 4 asserts no retirement happens while waiting.

---

### Task 1: API fields and reason

**Files:**
- Modify: `api/v1alpha1/network_types.go` (NetworkSpec, new NetworkUpdateSpec)
- Modify: `api/v1alpha1/servergroup_types.go` (ServerGroupStatus.Changeover)
- Modify: `api/v1alpha1/proxygroup_types.go` (ProxyGroupStatus.Changeover)
- Modify: `api/v1alpha1/common_types.go` (ChangeoverState type and values, ReasonWaitingForChangeoverBudget)
- Test: `api/v1alpha1/network_validation_test.go` (new, envtest; if the package keeps validation tests in another file, add there)

**Interfaces:**
- Produces: `NetworkSpec.Update *NetworkUpdateSpec`; `NetworkUpdateSpec.MaxConcurrentChangeovers *int32` (`+kubebuilder:validation:Minimum=1`); `func (n *Network) ChangeoverBudget() int32` (0 = no cap); `type ChangeoverState string` with `ChangeoverNone = ""`, `ChangeoverWaiting = "Waiting"`, `ChangeoverBegun = "Begun"`; `ServerGroupStatus.Changeover ChangeoverState` and `ProxyGroupStatus.Changeover ChangeoverState` (`json:"changeover,omitempty"`, `+kubebuilder:validation:Enum="";Waiting;Begun`); `ReasonWaitingForChangeoverBudget = "WaitingForChangeoverBudget"`.

- [ ] **Step 1: Failing test.** Add a test that creates a Network with `Update: &NetworkUpdateSpec{MaxConcurrentChangeovers: ptr.To[int32](0)}` and expects the API server to reject it, one with `1` that is accepted, and a unit test of `ChangeoverBudget()`: nil update → 0, nil field → 0, 2 → 2. Follow the envtest client setup the package's existing validation tests use (`grep -ln "testenv.Client" api/v1alpha1/*_test.go`).

- [ ] **Step 2: Run it, see it fail to compile** (`nix develop -c go test ./api/v1alpha1/ -run Changeover -count=1 -p 1`): undefined `NetworkUpdateSpec`.

- [ ] **Step 3: Implement.** In `network_types.go`, add to `NetworkSpec` after `Scheduling`:

```go
	// Update is how the network's groups change over to a new spec.
	// +optional
	Update *NetworkUpdateSpec `json:"update,omitempty"`
```

and the type:

```go
// NetworkUpdateSpec bounds changeovers across the network's groups.
type NetworkUpdateSpec struct {
	// MaxConcurrentChangeovers is how many server and proxy groups may change
	// over at the same time. A group changing over runs one server more than
	// its size until its last stale server is gone; a change that reaches
	// every group at once needs that room for every group at once. Unset
	// means no cap.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConcurrentChangeovers *int32 `json:"maxConcurrentChangeovers,omitempty"`
}

// ChangeoverBudget is spec.update.maxConcurrentChangeovers, 0 when unset.
func (n *Network) ChangeoverBudget() int32 {
	if n.Spec.Update == nil || n.Spec.Update.MaxConcurrentChangeovers == nil {
		return 0
	}
	return *n.Spec.Update.MaxConcurrentChangeovers
}
```

In `common_types.go` add the `ChangeoverState` type and constants, and the reason next to `ReasonServersStarting` with a one-line comment: `// ReasonWaitingForChangeoverBudget: the network's changeover budget is spent by other groups.` Add `Changeover ChangeoverState` with the enum marker and a two-line comment ("This group's changeover as the network's budget sees it; written by its own reconcile and read by its siblings'.") to both status structs.

- [ ] **Step 4: Generate and run.** `nix develop -c make manifests generate`, then the test from Step 2 → PASS. `git status` shows the CRD, chart template, deepcopy and `docs/reference/crds.md` changes.

- [ ] **Step 5: Commit** `feat(api): a network-wide changeover budget and each group's changeover state` with all generated files.

---

### Task 2: `AdmitChangeovers`

**Files:**
- Create: `internal/controller/changeover.go`
- Test: `internal/controller/changeover_test.go`

**Interfaces:**
- Consumes: `spawneryv1alpha1.ChangeoverState` (Task 1).
- Produces:

```go
type ChangeoverView struct {
	Kind    string // "ServerGroup" or "ProxyGroup"
	Name    string
	State   spawneryv1alpha1.ChangeoverState
	Failing bool
}

func changeoverKey(kind, name string) string { return kind + "/" + name }

// AdmitChangeovers returns the groups, keyed "Kind/Name", that may change over
// now. budget < 1 means no cap.
func AdmitChangeovers(groups []ChangeoverView, budget int32) map[string]bool
```

- [ ] **Step 1: Failing table test** `TestAdmitChangeovers` with these cases (each asserts the exact result map):
  1. no cap: `lobby` Waiting, `hub` Begun → both admitted.
  2. budget 1, `hub` Begun, `lobby` Waiting → only `ServerGroup/hub`.
  3. budget 1, nothing begun, `lobby` and `arena` Waiting → only `ServerGroup/arena` (name order).
  4. budget 1, `zeta` Begun, `alpha` Waiting → `zeta` stays, `alpha` not admitted (a begun group is never displaced).
  5. budget 1, `hub` Begun and Failing, `lobby` Waiting → only `lobby` (a failing holder frees its place; the failing group itself is not admitted).
  6. budget 2, `lobby` Waiting and Failing, `arena` Waiting → only `arena`.
  7. budget 1, `ProxyGroup/gateway` Begun, `ServerGroup/gateway` Waiting → only `ProxyGroup/gateway` (two entries).
  8. budget 2, two Begun holders (over budget after a race) and one Waiting → both holders, not the waiting one.
  9. a group with State `""` is never in the result, with or without a cap.

- [ ] **Step 2: Run, see it fail** (`nix develop -c go test ./internal/controller/ -run TestAdmitChangeovers -count=1`): undefined.

- [ ] **Step 3: Implement.**

```go
func AdmitChangeovers(groups []ChangeoverView, budget int32) map[string]bool {
	admitted := map[string]bool{}
	var waiting []ChangeoverView
	var holders int32
	for _, g := range groups {
		if g.State == spawneryv1alpha1.ChangeoverNone || g.Failing {
			continue
		}
		if budget < 1 || g.State == spawneryv1alpha1.ChangeoverBegun {
			admitted[changeoverKey(g.Kind, g.Name)] = true
			if g.State == spawneryv1alpha1.ChangeoverBegun {
				holders++
			}
			continue
		}
		waiting = append(waiting, g)
	}
	if budget < 1 {
		return admitted
	}
	sort.Slice(waiting, func(i, j int) bool {
		if waiting[i].Name != waiting[j].Name {
			return waiting[i].Name < waiting[j].Name
		}
		return waiting[i].Kind < waiting[j].Kind
	})
	for _, g := range waiting {
		if holders >= budget {
			break
		}
		admitted[changeoverKey(g.Kind, g.Name)] = true
		holders++
	}
	return admitted
}
```

- [ ] **Step 4: Run** → 9 cases PASS.

- [ ] **Step 5: Commit** `feat(controller): decide which groups may change over under a network budget`.

---

### Task 3: `DecideSize` and `DecideRollout` learn "not admitted"

**Files:**
- Modify: `internal/controller/scaling.go` (ScalingInputs, decideSize)
- Modify: `internal/controller/rollout.go` (DecideRollout signature)
- Modify: `internal/controller/proxygroup_controller.go:947` (call site, `true` for now)
- Test: `internal/controller/scaling_test.go`, `internal/controller/rollout_test.go`

**Interfaces:**
- Produces: `ScalingInputs.ChangeoverRefused bool` (named for the refusal so the zero value keeps today's behaviour in every existing table); `DecideRollout(pods []ProxyView, replicas int32, surgeAllowed bool) RolloutDecision`; `SizeDecision.ChangeoverWaiting bool`.

- [ ] **Step 1: Failing tests.**
  - `TestDecideSizeWithholdsTheColdStartWhileNotAdmitted`: one stale Ready server, `PodHash: "new"`, MinReplicas 1, MaxReplicas 3, `ChangeoverRefused: true` → `Create == 0`, `ChangeoverWaiting == true`, no `Retire`, no `Delete`. Same input with `ChangeoverRefused: false` → `Create == 1` (today's cold start).
  - `TestDecideSizeStillAnswersDemandWhileWaiting`: two stale servers, one full, spare-slot shortfall → the demand create still happens (waiting withholds only the cold start).
  - In `TestDecideRollout`, add the parameter `true` to the existing call (`DecideRollout(tc.pods, tc.replicas, true)`) and add cases with `surgeAllowed: false`: two stale Ready pods at replicas 2 → no create, no drain; one stale not-Ready pod among two → it is marked (replaced in place, no extra pod).

- [ ] **Step 2: Run, see them fail** (`go test ./internal/controller/ -run 'TestDecideSize|TestDecideRollout' -count=1`).

- [ ] **Step 3: Implement.**
  - `ScalingInputs`: add after `PodHash`:

```go
	// ChangeoverRefused withholds the cold start: the network's changeover
	// budget is spent by other groups. Demand is still answered.
	ChangeoverRefused bool
```

  - `decideSize`: replace `cold := coldStart(in)` with

```go
	cold := coldStart(in)
	waiting := cold && in.ChangeoverRefused
	if waiting {
		cold = false
	}
```

  and set `ChangeoverWaiting: waiting` on every `SizeDecision` the function returns (add the field to the struct with the comment "ChangeoverWaiting is a cold start withheld by the network's changeover budget.").
  - `DecideRollout`: add `surgeAllowed bool`; `if stale > 0 && surgeAllowed { surge = 1 }`. Extend its doc comment by one sentence: "Without surgeAllowed the network's changeover budget is spent: the group waits at its size, and only a stale pod that serves nobody is replaced, in place."
  - Proxy call site: `DecideRollout(views, group.Spec.Replicas, true)`.

- [ ] **Step 4: Run** the two test groups, then the whole package once: `nix develop -c go test ./internal/controller/ -count=1 -p 1` → PASS.

- [ ] **Step 5: Commit** `feat(controller): sizing and proxy rollout can withhold the surge`.

---

### Task 4: ServerGroup reconciler

**Files:**
- Modify: `internal/controller/servergroup_controller.go` (around the `DecideSize(ScalingInputs{...})` call at ~905, `reportProgressing` at ~1236, the status write)
- Create: `internal/controller/changeover_envtest_test.go`

**Interfaces:**
- Consumes: `AdmitChangeovers`, `ChangeoverView` (Task 2); `ScalingInputs.ChangeoverRefused`, `SizeDecision.ChangeoverWaiting` (Task 3); `Network.ChangeoverBudget()` (Task 1).
- Produces: `func (r *ServerGroupReconciler) changeoverSiblings(ctx, group, network) ([]ChangeoverView, error)`; `func ownServerChangeover(views []ServerView, podHash string, pendingCreates int32) spawneryv1alpha1.ChangeoverState`.

- [ ] **Step 1: Failing envtest** `TestChangeoverBudgetHoldsTheSecondGroup` in the new file:

```go
func TestChangeoverBudgetHoldsTheSecondGroup(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setChangeoverBudget(t, 1)
	arena := f.createEphemeralGroupLike(t, "arena") // copy of f.group's spec under another name
	for _, name := range []string{"arena", "lobby"} {
		f.reconcileNamedGroup(t, r, name)
		f.readyAllServersOf(t, name)
	}
	f.setImage(t, "arena", "ghcr.io/spawnery/paper:1.21.4-0.2.0")
	f.setImage(t, "lobby", "ghcr.io/spawnery/paper:1.21.4-0.2.0")

	f.reconcileNamedGroup(t, r, "arena")
	f.reconcileNamedGroup(t, r, "lobby")

	if n := len(f.serverNamesOfGroup(t, "arena")); n != 2 {
		t.Fatalf("arena has %d servers, want 2: first by name, it should hold the place", n)
	}
	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 1 {
		t.Fatalf("lobby has %d servers, want 1: the budget is spent", n)
	}
	if c := f.progressing(t, "lobby"); c.Reason != spawneryv1alpha1.ReasonWaitingForChangeoverBudget ||
		!strings.Contains(c.Message, "arena") {
		t.Fatalf("lobby Progressing = %s %q, want %s naming arena", c.Reason, c.Message,
			spawneryv1alpha1.ReasonWaitingForChangeoverBudget)
	}
	if got := f.serverGroup(t, "arena").Status.Changeover; got != spawneryv1alpha1.ChangeoverBegun {
		t.Fatalf("arena status.changeover = %q, want Begun", got)
	}

	// arena finishes: its stale server goes, its new one is Ready.
	f.finishChangeover(t, r, "arena")
	if got := f.serverGroup(t, "arena").Status.Changeover; got != spawneryv1alpha1.ChangeoverNone {
		t.Fatalf("arena status.changeover = %q after its changeover, want empty", got)
	}
	f.reconcileNamedGroup(t, r, "lobby")
	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 2 {
		t.Fatalf("lobby has %d servers, want 2: the place is free again", n)
	}
}
```

Write the helpers it names in the same file, each a few lines over existing fixture helpers: `setChangeoverBudget` (update `f.network.Spec.Update`), `createEphemeralGroupLike` (deep copy of `f.group` with a new name, created), `readyAllServersOf` (for every server of the group: `f.setPodRunning(name, true)` and reconcile it through `f.reconc`, as the existing ephemeral tests do to reach Ready), `setImage` (edit the group's `spec.image`), `serverGroup` (get by name), `finishChangeover` (ready the group's current-generation servers, delete its stale Server objects, reconcile the group twice). Add `TestChangeoverBudgetUnsetChangesNothing`: same setup without `setChangeoverBudget` → both groups have 2 servers after the image change.

- [ ] **Step 2: Run, see it fail** (`nix develop -c go test ./internal/controller/ -run TestChangeoverBudget -count=1 -p 1`): compile errors, then lobby with 2 servers.

- [ ] **Step 3: Implement.**
  - `ownServerChangeover`: stale views that count toward size exist (`staleSpec(v, podHash) && v.countsTowardSize()`) → if a current-generation view counts toward size or `pendingCreates > 0`, `Begun`, else `Waiting`; no stale → `""`. On-demand and persistent groups always `""` (they never surge).
  - `changeoverSiblings`: list `ServerGroupList` and `ProxyGroupList` in the namespace from the cache; for every group of the same network other than this one, a `ChangeoverView{Kind, Name, State: status.changeover, Failing: BackingOff or Degraded True}`.
  - Before `DecideSize` (ephemeral branch only): `own := ownServerChangeover(views, podHash, int32(len(pendingCreates)))`; `admitted := AdmitChangeovers(append(siblings, ChangeoverView{"ServerGroup", group.Name, own, failing(group)}), network.ChangeoverBudget())`; pass `ChangeoverRefused: own == spawneryv1alpha1.ChangeoverWaiting && !admitted[changeoverKey("ServerGroup", group.Name)]`.
  - After the decision: set `group.Status.Changeover` to `Begun` if the decision creates the cold start (`own == Waiting && decision.Create > 0`), else to `own`; it is written with the rest of the status in the existing status update.
  - `reportProgressing`: give it the holders' names (`waitingFor []string`, nil when not waiting) and add a first case: `condition.Status = True`, `Reason = ReasonWaitingForChangeoverBudget`, `Message = "waiting for a changeover place; changing over: " + strings.Join(waitingFor, ", ")`. Update its three existing test call sites with `nil`.

- [ ] **Step 4: Run** `-run 'TestChangeoverBudget|TestReportProgressing|TestDecideSize'` → PASS, then the whole package `-p 1` → PASS.

- [ ] **Step 5: Commit** `feat(controller): server groups wait for a place in the network's changeover budget`.

---

### Task 5: ProxyGroup reconciler

**Files:**
- Modify: `internal/controller/proxygroup_controller.go` (~947 call site, status, Progressing/ChangingOver reporting)
- Test: `internal/controller/changeover_envtest_test.go`

**Interfaces:**
- Consumes: Task 2, Task 3 `surgeAllowed`, the sibling listing of Task 4 (move `changeoverSiblings` to `changeover.go` as a function over `client.Reader` so both reconcilers use it).

- [ ] **Step 1: Failing envtest** `TestChangeoverBudgetHoldsAProxyGroupBehindAServerGroup`: budget 1; `lobby` begun (bump its image and reconcile once, so it holds the place); a proxy group `gateway` with 2 Ready pods (existing helpers `createProxyGroup`, `createProxyPod`, `reconcileProxyGroup`); change its image; reconcile → still 2 pods, `status.changeover == Waiting`, `ChangingOver` condition True with a message naming `lobby`. Finish lobby's changeover (Task 4 helper), reconcile the proxy group → 3 pods (the surge), `status.changeover == Begun`.

- [ ] **Step 2: Run, see it fail.**

- [ ] **Step 3: Implement.** Own state: any pod stale by hash (`Labels[podspec.LabelPodHash] != wantHash`, not the node-draining reason) → `Begun` if any pod carries `wantHash`, else `Waiting`; none → `""`. `surgeAllowed := own != Waiting || admitted[changeoverKey("ProxyGroup", group.Name)]`. After the decision set `status.changeover` to `Begun` when a surge pod is created from `Waiting`. In `reportChangingOver`, when waiting, set the message to `"waiting for a changeover place; changing over: <holders>"` (condition stays True with its existing reason; the proxy group has no Progressing condition).

- [ ] **Step 4: Run** the new test and `-run TestProxyGroup` → PASS; whole package `-p 1` → PASS.

- [ ] **Step 5: Commit** `feat(controller): proxy groups wait for a place in the network's changeover budget`.

---

### Task 6: Gauges

**Files:**
- Create: `internal/controller/metrics.go`
- Modify: `internal/controller/network_controller.go` (`countGroups`)
- Test: `internal/controller/network_controller_test.go`

**Interfaces:**
- Produces: `ChangeoversInFlight`, `ChangeoversWaiting` (`prometheus.GaugeVec`, label `namespace`, `network`), names `spawnery_network_changeovers_in_flight`, `spawnery_network_changeovers_waiting`, registered in `init()` on `metrics.Registry` like `internal/serverreg/metrics.go`.

- [ ] **Step 1: Failing test:** two server groups with `status.changeover` `Begun` and `Waiting` (write status directly), reconcile the network, read the gauges with `testutil.ToFloat64(ChangeoversInFlight.WithLabelValues(ns, "production"))` → 1 and 1.
- [ ] **Step 2: Run, see it fail.**
- [ ] **Step 3: Implement** the counting in `countGroups` (Begun and not failing → in flight; Waiting → waiting), and delete the label pair when the Network is deleted where the reconciler already cleans up on deletion.
- [ ] **Step 4: Run**; then `nix develop -c make manifests` to regenerate `docs/reference/metrics-and-alerts.md` (add the two gauges to whatever table `hack/metrics-docs.sh` reads if it does not discover them itself).
- [ ] **Step 5: Commit** `feat(controller): gauges for changeovers in flight and waiting`.

---

### Task 7: Docs and the full gate

**Files:**
- Modify: `docs/guides/updates-and-drain.md`
- Modify: `config/samples/` Network sample if one lists spec fields

- [ ] **Step 1:** Add a section "Changing over a whole network" to the guide: what happens when a change reaches every group, the field with a YAML example, what a waiting group looks like (`kubectl get servergroups`, the reason), the gauges. Generic example only ("a network on two nodes sized to its groups").
- [ ] **Step 2:** `nix develop -c make test` (full gate: manifests, generate, fmt, vet, linters, `go test -race ./...`; on the development VM run as `nix develop -c make test GOFLAGS=-p=1` if the default parallelism exhausts memory) → PASS, and `git status` clean apart from what this task changed.
- [ ] **Step 3:** `nix develop -c make lint` → PASS.
- [ ] **Step 4: Commit** `docs(guides): the network's changeover budget`.

Releases (operator version, chart version) are not part of this plan; they follow the repository's release process once the branch is merged.
