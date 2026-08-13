# Milestone 4b: rolling updates of ephemeral groups — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An ephemeral `ServerGroup` whose spec changes replaces its servers by itself — a replacement comes up first, an old server stops taking joins, its players finish their session undisturbed, and it disappears when the last one leaves.

**Architecture:** A new `Server` phase, `Retiring`, is soft drain: deregistered, no active drain, no drain deadline. The `ServerGroup` controller decides who retires (it alone knows the generation and the budget) and says so through `Server.spec.retire`; the `Server` controller executes through the existing pure state machine. Adding `Retiring` to `leaving()` makes a retiring server stop counting toward the group's size, so the *existing* spare-slot rule orders a replacement when capacity needs one — the generation never enters the capacity arithmetic.

**Tech Stack:** Go, controller-runtime, kubebuilder CRDs, envtest. No proto, no agent, no image work.

## Global Constraints

- **Design of record:** `docs/superpowers/specs/2026-08-13-rolling-updates-design.md`. Where this plan and the spec disagree, the spec wins — except where a task explicitly says it is correcting the spec, in which case that task amends the spec in the same commit.
- **The load-bearing invariant:** a server that may be carrying players is never nominated for deletion. `SelectDeletionCandidates` holds it. No task weakens it.
- **Three capacity figures stay three.** `AggregateGroup`'s `FreeSlots`, `provisionalCapacity`'s sum and `readyFree` keep their separate meanings and filters. No task unifies them, and no task adds a generation filter to the latter two.
- **`internal/phase` is at 100% coverage and stays there.** `internal/controller` is at 87.9% and must not fall below 88% at the end.
- **Operator-only Go.** `git diff --name-only` must touch nothing under `agent/`, `image/`, `proto/`, `nix/`.
- **Every test whose expectations move gets its mutation made for real, and the output reported.** 4a's reviews found eleven assertions that could not fail. "The test stopped failing" and "the test stopped testing" look identical from outside.
- **Build and test:** `nix develop -c make test` (about 38s; `internal/controller` boots envtest and takes 34s of it — a slow run is not a hang). `nix develop -c make manifests` must produce no diff except where a task says it will.
- **Commit style:** conventional commits, scope `4b`. End every commit message with:
  ```
  Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
  ```

## File structure

| File | Responsibility | Task |
|---|---|---|
| `internal/phase/phase.go` | The `Retiring` phase, its entry from `Ready` and its four exits | 1 |
| `internal/phase/phase_test.go` | The table rows for all of the above | 1 |
| `api/v1alpha1/server_types.go` | `spec.retire`, `status.retiringSince` | 2 |
| `config/crd/bases/*.yaml` | Regenerated | 2 |
| `internal/controller/candidates.go` | `leaving()` gains `Retiring`; `ServerView.Retire` | 3 |
| `internal/controller/expectations.go` | The retire reservation | 4 |
| `internal/controller/scaling.go` | The cold start (5), the retirement nomination (6), 4a's leftover (10) | 5, 6, 10 |
| `internal/controller/servergroup_controller.go` | Reads `spec.retire`, executes `decision.Retire` | 7 |
| `internal/controller/server_controller.go` | Builds the two new `Inputs`, stamps `retiringSince` | 8 |
| `internal/controller/servergroup_controller_test.go` | The envtest that runs a real changeover | 9 |

Ten tasks. 1–4 are independent foundations; 5 and 6 are the rules; 7 and 8 wire them; 9 proves it end to end; 10 is a one-liner 4a left behind and is independent of everything else.

---

### Task 1: The `Retiring` phase

**Files:**
- Modify: `internal/phase/phase.go`
- Test: `internal/phase/phase_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `phase.Retiring Phase`, `phase.ReasonRetiring string`, `phase.ReasonMaxStaleElapsed string`, `Inputs.RetirementRequested bool`, `Inputs.MaxStaleReached bool`. Tasks 3, 7 and 8 rely on all of these by name.

- [ ] **Step 1: Write the failing tests**

Add these rows to the `cases` slice in `TestDecide` in `internal/phase/phase_test.go`, after the existing `Draining` rows:

```go
{
	name:    "ready retires when the group asks",
	current: Ready,
	in: Inputs{
		PodExists: true, PodRunning: true, PodReady: true, AgentReady: true,
		RetirementRequested: true, WasRegistered: true,
	},
	want: Decision{Next: Retiring, Deregister: true, Reason: ReasonRetiring},
},
{
	// Soft drain is deregistration without a move. The proxies learn the
	// server is gone from status.registered; nothing tells them to take
	// anyone off it, and internal/proxyreg only sends DrainPlayers for
	// phase Draining.
	name:    "retiring never asks for a drain while it waits",
	current: Retiring,
	in:      Inputs{PodExists: true, PodRunning: true, PlayersOnline: 3},
	want:    Decision{Next: Retiring, Reason: ReasonRetiring},
},
{
	name:    "retiring terminates once the last player leaves",
	current: Retiring,
	in:      Inputs{PodExists: true, PodRunning: true},
	want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonDrained},
},
{
	// The whole difference from Draining: an occupied retiring server has
	// no deadline over it at all until maxStaleSeconds says so.
	name:    "an occupied retiring server is never terminated on a drain deadline",
	current: Retiring,
	in: Inputs{
		PodExists: true, PodRunning: true, PlayersOnline: 1,
		DrainDeadlineReached: true,
	},
	want: Decision{Next: Retiring, Reason: ReasonRetiring},
},
{
	name:    "the stale deadline escalates to a real drain",
	current: Retiring,
	in: Inputs{
		PodExists: true, PodRunning: true, PlayersOnline: 1,
		MaxStaleReached: true,
	},
	want: Decision{Next: Draining, StartDrain: true, Reason: ReasonMaxStaleElapsed},
},
{
	// Whoever deletes a retiring server gets the proper move, not a drop:
	// it still has players on it.
	name:    "deleting a retiring server moves its players off",
	current: Retiring,
	in: Inputs{
		PodExists: true, PodRunning: true, PlayersOnline: 1,
		DeletionRequested: true,
	},
	want: Decision{Next: Draining, StartDrain: true, Reason: ReasonDeletionRequested},
},
{
	name:    "a lost pod ends a retirement without a drain",
	current: Retiring,
	in:      Inputs{PlayersOnline: 1, PlayersStale: true},
	want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonPodLost},
},
{
	name:    "a terminal pod ends a retirement without a drain",
	current: Retiring,
	in:      Inputs{PodExists: true, PodTerminal: true, PlayersOnline: 1},
	want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonPodTerminal},
},
```

Note on the "lost pod" row: `PodLost` is not an input the caller sets directly in this table — the existing rows for `Draining` express it as `Inputs{}` with no `PodExists`. Check how the neighbouring `Draining` `PodLost` row is written and match it exactly; if it sets `PodLost: true` explicitly, do the same here.

Then add a standalone test below `TestNoPathBackFromDraining`:

```go
func TestNoPathBackFromRetiring(t *testing.T) {
	// Retiring is one-way, like Draining. A server that is being replaced
	// must not re-register itself because its probe happens to be green:
	// the proxies would start sending joins to a server the group has
	// already decided to remove.
	in := Inputs{
		PodExists: true, PodRunning: true, PodReady: true, AgentReady: true,
		PlayersOnline: 1,
	}
	if got := Decide(Retiring, in); got.Next == Ready || got.Register {
		t.Errorf("Decide(Retiring, healthy) = %+v, want no way back to Ready", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
nix develop -c go test ./internal/phase/ -run 'TestDecide|TestNoPathBackFromRetiring' -v
```

Expected: compile failure — `undefined: Retiring`, `undefined: ReasonRetiring`, `undefined: ReasonMaxStaleElapsed`, and `unknown field RetirementRequested`/`MaxStaleReached` in `Inputs`.

- [ ] **Step 3: Add the phase, the reasons and the inputs**

In `internal/phase/phase.go`, in the phase `const` block, after `Ready` and before `Draining`:

```go
	// Retiring is soft drain: the server is deregistered and takes no new
	// joins, but its players are left alone until they leave of their own
	// accord. It is what a rolling update puts a stale server into, and it
	// is deliberately not Draining: no players are moved, and
	// spec.drain.timeoutSeconds does not hang over it, because a lobby can
	// legitimately sit here for hours. spec.update.maxStaleSeconds is the
	// only thing that bounds it, and only when configured non-zero.
	Retiring Phase = "Retiring"
```

In the reason `const` block:

```go
	ReasonRetiring         = "Retiring"
	ReasonMaxStaleElapsed  = "MaxStaleElapsed"
```

In `Inputs`, after `DrainDeadlineReached`:

```go
	// RetirementRequested is the group's instruction to enter soft drain,
	// read from Server.spec.retire. The group decides, because only it knows
	// the generation, the update budget and whether a replacement is ready;
	// this package only carries out the transition.
	RetirementRequested bool
	// MaxStaleReached is true once a retiring server has waited longer than
	// spec.update.maxStaleSeconds. It is measured from status.retiringSince
	// — the wait in soft drain — and not from the group's generation change.
	MaxStaleReached bool
```

- [ ] **Step 4: Add the `Retiring` case to `Decide`**

In `Decide`, add a case immediately after `case Draining:` and before `case Pending, Starting, Ready:`:

```go
	case Retiring:
		if in.PodLost {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonPodLost, Message: "pod disappeared while retiring",
			}
		}
		if in.PodTerminal {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason:  ReasonPodTerminal,
				Message: "pod reached a terminal phase while retiring, its players are already gone",
			}
		}
		// Empty first, and before the two escalations below: a retiring
		// server that has already run empty needs neither a drain nor a
		// deadline, and sending it through Draining would cost a reconcile
		// and emit a move for nobody.
		if !in.Occupied() {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonDrained, Message: "no players left",
			}
		}
		if in.DeletionRequested {
			return Decision{
				Next: Draining, StartDrain: true,
				Reason: ReasonDeletionRequested, Message: "deletion requested, moving players off",
			}
		}
		if in.MaxStaleReached {
			return Decision{
				Next: Draining, StartDrain: true,
				Reason:  ReasonMaxStaleElapsed,
				Message: "stale deadline reached, moving players off",
			}
		}
		// No Deregister here: entering Retiring did that once. Repeating it
		// every pass would re-emit the proxy call for the whole wait.
		return Decision{
			Next:   Retiring,
			Reason: ReasonRetiring, Message: "waiting for players to leave",
		}
```

- [ ] **Step 5: Add the entry from `Ready`**

In the `default: // Ready` block at the end of `Decide`, after the `if lost { ... }` block and before the final `return`:

```go
		// After the readiness check on purpose: a server that has just lost a
		// ready signal is already being deregistered by that path, and letting
		// retirement overtake it would swallow the readiness loss the flap
		// counter needs.
		if in.RetirementRequested {
			return Decision{
				Next: Retiring, Deregister: true,
				Reason: ReasonRetiring, Message: "retiring for a rolling update",
			}
		}
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
nix develop -c go test ./internal/phase/ -v -cover
```

Expected: PASS, coverage still `100.0% of statements`. If coverage dropped, a branch above has no row; add it rather than lowering the bar.

- [ ] **Step 7: Prove the new rows can fail**

Mutate `Decide`'s `Retiring` case so the `MaxStaleReached` branch returns `Next: Retiring` instead of `Draining`, run the tests, and confirm "the stale deadline escalates to a real drain" fails. Revert. Report the failure output.

- [ ] **Step 8: Commit**

```bash
git add internal/phase/phase.go internal/phase/phase_test.go
git commit -m "feat(4b): a server can be retired without its players being moved"
```

---

### Task 2: The two API fields

**Files:**
- Modify: `api/v1alpha1/server_types.go`
- Modify (generated): `config/crd/bases/spawnery.cloud_servers.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`
- Test: `internal/controller/server_controller_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `ServerSpec.Retire bool` (JSON `retire`), `ServerStatus.RetiringSince *metav1.Time` (JSON `retiringSince`). Tasks 3, 7 and 8 read and write these.

- [ ] **Step 1: Write the failing test**

This test exists to catch the specific mistake of editing the Go type and forgetting `make manifests`: the API server silently drops unknown fields, so a Go-only change round-trips as zero. Add to `internal/controller/server_controller_test.go`, inside the existing envtest suite (match the surrounding `Describe`/`It` or `t.Run` style of that file — read it first and follow it rather than introducing a second style):

```go
func TestServerRetireFieldsRoundTripThroughTheAPIServer(t *testing.T) {
	// A field added to the Go type but missing from the CRD is dropped by
	// the API server without an error, and every controller that reads it
	// then sees the zero value. Only a round trip through a real API server
	// catches that, which is why this is here and not a struct test.
	ctx := context.Background()
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "retire-roundtrip", Namespace: testNamespace},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "some-group"},
			Retire:   true,
		},
	}
	if err := k8sClient.Create(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, srv) })

	now := metav1.Now()
	srv.Status.RetiringSince = &now
	if err := k8sClient.Status().Update(ctx, srv); err != nil {
		t.Fatalf("status update: %v", err)
	}

	got := &spawneryv1alpha1.Server{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(srv), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Spec.Retire {
		t.Error("spec.retire did not survive the API server; run make manifests")
	}
	if got.Status.RetiringSince == nil {
		t.Error("status.retiringSince did not survive the API server; run make manifests")
	}
}
```

Adapt `testNamespace`, `k8sClient` and the suite entry point to whatever the file already uses.

- [ ] **Step 2: Run it to verify it fails**

```bash
nix develop -c go test ./internal/controller/ -run TestServerRetireFieldsRoundTrip -v
```

Expected: compile failure, `unknown field Retire in struct literal`.

- [ ] **Step 3: Add the fields**

In `api/v1alpha1/server_types.go`, in `ServerSpec` after `GroupGeneration`:

```go
	// Retire asks this server to stop taking joins and empty out, without its
	// players being moved. The ServerGroup controller sets it during a
	// rolling update; a user never does. It is also the single signal for
	// spec.update.maxUnavailable: a server counts against that budget while
	// this is true, which is what tells a retirement apart from a drain a
	// scale-down or a deletion started.
	// +optional
	Retire bool `json:"retire,omitempty"`
```

In `ServerStatus`, after `DrainStartedAt`:

```go
	// RetiringSince is when the server entered phase Retiring. It drives
	// spec.update.maxStaleSeconds and nothing else — what marks a server as
	// one this update made unavailable is spec.retire.
	// +optional
	RetiringSince *metav1.Time `json:"retiringSince,omitempty"`
```

- [ ] **Step 4: Regenerate and check the diff is only these two fields**

```bash
nix develop -c make manifests generate
git diff --stat
git diff config/crd/bases/
```

Expected: `retire` under `spec.properties` and `retiringSince` under `status.properties` in `spawnery.cloud_servers.yaml`, the deepcopy for `RetiringSince`, and nothing else. Any other CRD file changing means something unrelated drifted — stop and find out what before continuing.

- [ ] **Step 5: Run the test to verify it passes**

```bash
nix develop -c go test ./internal/controller/ -run TestServerRetireFieldsRoundTrip -v
```

Expected: PASS.

- [ ] **Step 6: Prove the test can fail**

Delete the `retire` property from `config/crd/bases/spawnery.cloud_servers.yaml` by hand, re-run the test, confirm it fails with "spec.retire did not survive the API server". Restore with `make manifests`. Report the output.

- [ ] **Step 7: Commit**

```bash
git add api/v1alpha1/ config/crd/bases/ internal/controller/server_controller_test.go
git commit -m "feat(4b): give a Server a retirement instruction and a retirement clock"
```

---

### Task 3: A retiring server stops counting, and stays protected

**Files:**
- Modify: `internal/controller/candidates.go`
- Test: `internal/controller/candidates_test.go`

**Interfaces:**
- Consumes: `phase.Retiring` (Task 1), `ServerSpec.Retire` (Task 2).
- Produces: `ServerView.Retire bool`. Tasks 5, 6 and 7 read it.

- [ ] **Step 1: Write the failing tests**

Add to `internal/controller/candidates_test.go`:

```go
func TestRetiringDoesNotCountTowardSize(t *testing.T) {
	// This one line is the whole surge mechanism: a retiring server drops
	// out of the group's size, so the existing spare-slot rule orders its
	// replacement when — and only when — capacity actually needs one.
	v := ServerView{Name: "a", Phase: phase.Retiring}
	if v.countsTowardSize() {
		t.Error("a retiring server still holds the group at its floor")
	}
	if !v.leaving() {
		t.Error("leaving() does not recognise Retiring")
	}
}

func TestRetiringServerIsNeverNominatedForDeletion(t *testing.T) {
	// The invariant everything rests on. A retiring server has players on
	// it by definition — that is what it is waiting for — and the group
	// removes it by letting it empty, never by deleting it.
	views := []ServerView{
		{Name: "retiring", Phase: phase.Retiring, Players: 5, WasRegistered: true},
	}
	if got := SelectDeletionCandidates(views, 1); len(got) != 0 {
		t.Errorf("SelectDeletionCandidates = %v, want none", got)
	}
}

func TestRetiringServerStaysInsideTheDisruptionBudget(t *testing.T) {
	// occupiedPods is deliberately not phase-based: the pod still carries
	// the occupied label while anyone is on it, and minAvailable has to
	// match that pod for pod or kubectl drain gets an eviction to spend on
	// a pod with players. Nothing else in the tree would catch this
	// changing.
	views := []ServerView{
		{Name: "a", Phase: phase.Retiring, Players: 2, WasRegistered: true},
		{Name: "b", Phase: phase.Retiring, WasRegistered: true},
	}
	if got := occupiedPods(views); got != 1 {
		t.Errorf("occupiedPods = %d, want 1 — the retiring server with players", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestRetiring' -v
```

Expected: `TestRetiringDoesNotCountTowardSize` fails on both assertions; the other two pass already (they follow from rules that are player-based). That is the correct starting point — the two that pass are regression guards for behaviour the next tasks must not break, and the plan says so rather than pretending all three fail.

- [ ] **Step 3: Add `Retiring` to `leaving()` and the view field**

In `internal/controller/candidates.go`, change `leaving`:

```go
// leaving reports whether the server is already on its way out, so the group
// must not count it as a candidate again.
//
// Retiring is in here for a second reason beyond nomination: dropping out of
// the group's size is exactly what makes the spare-slot rule order a
// replacement for a server a rolling update has retired. The generation never
// enters the capacity arithmetic; this does the work instead.
func (v ServerView) leaving() bool {
	return v.Phase == phase.Draining || v.Phase == phase.Terminating || v.Phase == phase.Retiring
}
```

And add to `ServerView`, after `Generation`:

```go
	// Retire is spec.retire: the group has asked this server to retire. It
	// is the single signal for the update's maxUnavailable budget, and it
	// survives the escalation to Draining that maxStaleSeconds can force —
	// which is what tells that drain apart from one a scale-down started.
	Retire bool
```

- [ ] **Step 4: Run them to verify they pass**

```bash
nix develop -c go test ./internal/controller/ -run 'TestRetiring|TestCountsTowardSize|TestOccupiedPods|TestSelect' -v
```

Expected: PASS, including the pre-existing tests listed.

- [ ] **Step 5: Prove the first test can fail**

Revert the `leaving()` change, run, confirm `TestRetiringDoesNotCountTowardSize` fails. Re-apply. Report the output.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/candidates.go internal/controller/candidates_test.go
git commit -m "feat(4b): a retiring server leaves the size count and keeps its protection"
```

---

### Task 4: Reserving a retirement against the cache

**Files:**
- Modify: `internal/controller/expectations.go`
- Modify: every caller of `pending`, whose signature widens from two return values to three. Find them with `grep -rn '\.pending(' --include=*.go internal/` rather than trusting this list: at the time of writing they are `servergroup_controller.go`, `servergroup_controller_test.go` (`TestGroupRecordsWhatItIssued`) and `expectations_test.go` throughout. A caller left behind does not fail a test, it fails the build.
- Test: `internal/controller/expectations_test.go`

**Interfaces:**
- Consumes: `ServerView.Retire` (Task 3).
- Produces: `(*expectations).expectRetired(group, name string)`, and `pending` changes signature to `pending(group string) (int32, map[string]bool, map[string]bool)` — creates, deletes, retires. Tasks 6 and 7 use both.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/expectations_test.go`, matching the helpers already in that file:

```go
func TestExpectedRetireCountsUntilTheCacheShowsIt(t *testing.T) {
	// Without this reservation a second server can be nominated while the
	// first patch has not reached the cache, and maxUnavailable is exceeded
	// by one. The window is small; the standard here is not smallness.
	e := newExpectations(time.Now)
	e.expectRetired("ns/g", "a")

	_, _, retires := e.pending("ns/g")
	if !retires["a"] {
		t.Fatal("a retirement the cache has not shown is not reserved")
	}

	// The cache still shows the old spec: still reserved.
	e.observe("ns/g", []ServerView{{Name: "a"}})
	if _, _, retires = e.pending("ns/g"); !retires["a"] {
		t.Error("the reservation was dropped before the cache caught up")
	}

	// The cache shows the patch: the reservation has done its job.
	e.observe("ns/g", []ServerView{{Name: "a", Retire: true}})
	if _, _, retires = e.pending("ns/g"); retires["a"] {
		t.Error("the reservation outlived the observation")
	}
}

func TestExpectedRetireIsSatisfiedByDisappearance(t *testing.T) {
	// A server that finished retiring and was deleted between the patch and
	// the next list would otherwise hold a slot of the budget forever, or
	// until the TTL — and the TTL is the backstop, not the mechanism.
	e := newExpectations(time.Now)
	e.expectRetired("ns/g", "a")
	e.observe("ns/g", nil)
	if _, _, retires := e.pending("ns/g"); retires["a"] {
		t.Error("a retirement whose server is gone is still reserved")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
nix develop -c go test ./internal/controller/ -run TestExpectedRetire -v
```

Expected: compile failure — `e.expectRetired undefined` and `assignment mismatch: 3 variables but e.pending returns 2 values`.

- [ ] **Step 3: Add the kind, the recorder and the observation**

In `internal/controller/expectations.go`, add to the kind block:

```go
const (
	expectationCreate expectationKind = iota
	expectationDelete
	expectationRetire
)
```

Add the recorder next to the other two:

```go
// expectRetired records a Server this reconciler has just asked to retire.
func (e *expectations) expectRetired(group, name string) {
	e.record(group, name, expectationRetire)
}
```

Add the case to `observe`'s switch:

```go
		case expectationRetire:
			// Satisfied when the cache shows the patch, and also when the
			// server is gone: a retirement that completed between the patch
			// and this list has nothing left to reserve.
			if !present || v.Retire {
				delete(m, name)
			}
```

- [ ] **Step 4: Widen `pending`**

```go
// pending is what the group has outstanding: how many creates, which names are
// already on their way out, and which have been asked to retire.
func (e *expectations) pending(group string) (int32, map[string]bool, map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var creates int32
	deletes := make(map[string]bool)
	retires := make(map[string]bool)
	for name, exp := range e.byGroup[group] {
		switch exp.kind {
		case expectationCreate:
			creates++
		case expectationDelete:
			deletes[name] = true
		case expectationRetire:
			retires[name] = true
		}
	}
	return creates, deletes, retires
}
```

In `internal/controller/servergroup_controller.go`, update the single caller in `size()`. Discard the third value for now — `ScalingInputs.PendingRetires` does not exist until Task 6, and a named-but-unused variable would be dead code for two tasks:

```go
	pendingCreates, pendingDeletes, _ := r.Expectations.pending(key)
```

Task 7 gives it a name when it has somewhere to go.

- [ ] **Step 5: Run to verify it passes**

```bash
nix develop -c go test ./internal/controller/ -run TestExpect -v
```

Expected: PASS, including the pre-existing expectation tests.

- [ ] **Step 6: Prove it can fail**

Change `observe`'s retire case to `if !present` alone, run, confirm `TestExpectedRetireCountsUntilTheCacheShowsIt` fails on "the reservation outlived the observation". Revert. Report the output.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/expectations.go internal/controller/expectations_test.go internal/controller/servergroup_controller.go
git commit -m "feat(4b): reserve a retirement the cache has not shown yet"
```

---

### Task 5: The cold start

**Files:**
- Modify: `internal/controller/scaling.go`
- Modify: `docs/superpowers/specs/2026-08-13-rolling-updates-design.md`
- Test: `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: `ServerView.Generation` (already present), `phase.Failed`.
- Produces: `ScalingInputs.Generation int64`. Task 6 adds `MaxUnavailable` to the same struct; Task 7 passes both.

**Why this is its own task:** retirement needs a ready server of the current generation to exist before it may start. When every server is stale none does, so nothing retires, so nothing is created — a deadlock. The cold start is the one create that breaks it, and it is the only place the group is allowed to exceed what demand asks for.

- [ ] **Step 1: Write the failing tests**

Add to `internal/controller/scaling_test.go`. Note the file's existing `ready`/`starting` helpers leave `Generation` at zero, so "current generation" in these tests is generation 0 unless stated:

```go
// staleReady builds a Ready server of an older generation.
func staleReady(name string, players, slots int32, gen int64) ServerView {
	v := ready(name, players, slots)
	v.Generation = gen
	return v
}

func TestDecideSizeColdStartsAReplacementForAStaleGroup(t *testing.T) {
	// Two stale servers with plenty of free capacity between them. The
	// spare-slot rule is satisfied and would create nothing, so without the
	// cold start no server of the new generation ever exists, nothing may
	// retire, and the update never begins.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("a", 60, 100, 3),
			staleReady("b", 60, 100, 3),
		},
		Generation:  4,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want exactly 1 to start the changeover", got.Create)
	}
}

func TestDecideSizeColdStartsOnlyOnce(t *testing.T) {
	// The replacement is on order but has not reached the cache. Firing
	// again here would create one server per five-second pass for the whole
	// boot.
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{staleReady("a", 60, 100, 3)},
		Generation: 4, PendingCreates: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 while the cold start is outstanding", got.Create)
	}
}

func TestDecideSizeDoesNotColdStartWhenAReplacementIsAlreadyUp(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("a", 60, 100, 3),
			ready("b", 0, 100), // generation 0 == current
		},
		Generation:  0,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 when the current generation is already up", got.Create)
	}
}

func TestDecideSizeDoesNotColdStartAgainstARetainedFailure(t *testing.T) {
	// A broken image is the most likely way an update goes wrong, and the
	// cold start would otherwise re-create against it every five seconds
	// forever. The retained failure is the window: one attempt per
	// failedRetentionSeconds, the old generation serving throughout, one
	// corpse left to diagnose from. This is not backoff and does nothing
	// for the floor rule's own loop.
	failed := ServerView{Name: "b", Phase: phase.Failed, Generation: 4}
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{staleReady("a", 60, 100, 3), failed},
		Generation: 4,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 while a failure of the current generation is retained", got.Create)
	}
}

func TestDecideSizeStaleFailureDoesNotBlockTheColdStart(t *testing.T) {
	// The corpse of the *old* generation says nothing about whether the new
	// image works, so it must not hold the update back.
	failed := ServerView{Name: "b", Phase: phase.Failed, Generation: 3}
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{staleReady("a", 60, 100, 3), failed},
		Generation: 4,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 1 {
		t.Errorf("Create = %d, want 1 — a stale failure is not evidence about the new generation", got.Create)
	}
}

func TestDecideSizeReportsAColdStartTheCeilingRefuses(t *testing.T) {
	// A group pinned at maxReplicas has no room for the one extra server the
	// changeover needs, so the update cannot begin. Stalling is right — the
	// ceiling is an instruction — but it must not stall silently, and
	// ScalingLimited is the condition that already exists to say so.
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{staleReady("a", 0, 100, 3)},
		Generation: 4,
		MinReplicas: 1, MaxReplicas: 1, SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 at the ceiling", got.Create)
	}
	if !got.Limited {
		t.Error("Limited = false, want true so the stalled changeover is visible")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run TestDecideSizeColdStart -v
```

Expected: compile failure, `unknown field Generation in struct literal of type ScalingInputs`.

- [ ] **Step 3: Add the input and the rule**

In `internal/controller/scaling.go`, add to `ScalingInputs` after `PendingDeletes`, and amend the type's leading comment:

```go
	// Generation is the group's current metadata.generation. A view whose
	// Generation differs is stale.
	//
	// It decides only *which* server retires, never how many get built: the
	// capacity arithmetic below stays generation-blind, exactly as milestone
	// 4a left it. See the type comment above.
	Generation int64
```

Replace the "Nothing here is the group's generation" paragraph of the `ScalingInputs` doc comment with:

```go
// The group's generation is here, and it is confined to one job. It selects
// which stale server retires and whether the changeover has begun; it never
// enters provisionalCapacity or readyFree. A scale-up rule that credited only
// servers of the current generation would find, the instant any field of the
// group's spec changed, that nothing running counts — and would order a full
// replacement set up to maxReplicas on the next five-second pass. Keeping the
// arithmetic generation-blind is what makes that impossible rather than merely
// braked.
```

Add the rule above `DecideSize`:

```go
// coldStart reports whether the group must create the first server of its
// current generation before anything can retire.
//
// Retirement needs a ready server of the current generation to exist. When
// every server is stale none does, so nothing may retire, so nothing drops out
// of the size count, so the spare-slot rule creates nothing — a deadlock that
// only an unconditional create breaks. This is the one create in the system
// that is not answering demand, and it is why the changeover costs at most one
// extra server.
//
// It is suppressed while a Failed server of the *current* generation is being
// retained. Without that, a broken new image fails, drops out of
// countsTowardSize, and is re-created on the next five-second pass forever. The
// retention window (an hour by default) becomes the interval: one attempt, the
// old generation serving undisturbed, one corpse to diagnose from. A stale
// failure does not suppress it — the old generation's corpse says nothing about
// whether the new image works.
//
// This is deliberately not backoff. The floor rule has the same loop and keeps
// it; giving it a real per-group backoff with a Degraded condition is its own
// milestone.
func coldStart(in ScalingInputs) bool {
	if in.PendingCreates > 0 {
		// A pending create is always of the current generation, by
		// construction: createServer stamps group.Generation on it.
		return false
	}
	var stale, current int
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] {
			continue
		}
		if v.Generation == in.Generation {
			// A retained failure counts here even though countsTowardSize
			// excludes it, and that is the suppression.
			if v.Phase == phase.Failed || v.countsTowardSize() {
				current++
			}
			continue
		}
		if v.countsTowardSize() {
			stale++
		}
	}
	return stale > 0 && current == 0
}
```

- [ ] **Step 4: Wire it into `DecideSize`**

In `DecideSize`, replace the block that computes `create`, `room`, `granted` and `limited` with:

```go
	create := wanted
	if floor := in.MinReplicas - alive; floor > create {
		create = floor
	}
	cold := coldStart(in)
	if cold && create < 1 {
		create = 1
	}
	room := in.MaxReplicas - alive
	if room < 0 {
		room = 0
	}
	granted := create
	if granted > room {
		granted = room
	}
	// A cold start the ceiling refuses is a changeover that cannot begin.
	// Stalling is correct — a lowered maxReplicas is an instruction — but it
	// must be visible, and ScalingLimited is the condition that says exactly
	// "the ceiling is holding capacity back".
	limited := wanted > granted || (cold && granted < 1)
```

- [ ] **Step 5: Run to verify they pass**

```bash
nix develop -c go test ./internal/controller/ -run TestDecideSize -v
```

Expected: PASS, including every pre-existing `TestDecideSize*`. If a pre-existing test broke, the cold start is firing where it should not — the existing tests leave `Generation` at zero on both the input and the views, so `coldStart` must find no stale server and return false. Fix the rule, not the test.

- [ ] **Step 6: Prove the suppression can fail**

Remove `v.Phase == phase.Failed ||` from `coldStart`, run, confirm `TestDecideSizeDoesNotColdStartAgainstARetainedFailure` fails. Revert. Report the output.

- [ ] **Step 7: Record the ceiling consequence in the spec**

The spec does not say what happens to a group pinned at `maxReplicas`. Add to §3.3, after the "Two things bound it" paragraph:

```markdown
**A group at its ceiling cannot start a changeover, and says so.** The cold
start is a create like any other and the ceiling clamps it, so a group whose
`maxReplicas` equals its current size stalls with its old generation serving.
That is the right outcome — a lowered ceiling is an instruction, not a
suggestion — but it must not be silent, so `DecideSize` sets `Limited` in that
case and the existing `ScalingLimited` condition carries it onto the group.
Raising `maxReplicas` by one is the operator's way out.
```

- [ ] **Step 8: Commit**

```bash
git add internal/controller/scaling.go internal/controller/scaling_test.go docs/superpowers/specs/2026-08-13-rolling-updates-design.md
git commit -m "feat(4b): break the changeover deadlock with one cold-start server"
```

---

### Task 6: The retirement nomination

**Files:**
- Modify: `internal/controller/scaling.go`
- Test: `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: `ScalingInputs.Generation` (Task 5), `ServerView.Retire` (Task 3), `pending`'s retires map (Task 4).
- Produces: `ScalingInputs.MaxUnavailable int32`, `ScalingInputs.PendingRetires map[string]bool`, `SizeDecision.Retire []string`. Task 7 supplies the first two and executes the third.

- [ ] **Step 1: Write the failing tests**

```go
func TestDecideSizeRetiresAStaleServerOnceAReplacementIsReady(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("old", 60, 100, 3),
			ready("new", 0, 100),
		},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Errorf("Retire = %v, want [old]", got.Retire)
	}
}

func TestDecideSizeRetiresAServerThatStillHasPlayers(t *testing.T) {
	// The point of soft drain, and the reason retirement cannot reuse
	// SelectDeletionCandidates: that function excludes exactly these
	// servers, and these are the ones that have to go.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("old", 99, 100, 3),
			ready("new", 0, 100),
		},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 {
		t.Fatalf("Retire = %v, want the occupied stale server", got.Retire)
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none — retirement is not deletion", got.Delete)
	}
}

func TestDecideSizeDoesNotRetireWithoutAReadyReplacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		new  ServerView
	}{
		{"the replacement is still starting", starting("new")},
		{"the replacement failed", ServerView{Name: "new", Phase: phase.Failed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideSize(ScalingInputs{
				Views:      []ServerView{staleReady("old", 60, 100, 3), tc.new},
				Generation: 0, MaxUnavailable: 1,
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
			})
			if len(got.Retire) != 0 {
				t.Errorf("Retire = %v, want none until a replacement is Ready", got.Retire)
			}
		})
	}
}

func TestDecideSizeRespectsTheUpdateBudget(t *testing.T) {
	// One is already retiring. maxUnavailable is 1, so the second stale
	// server waits.
	retiring := staleReady("first", 5, 100, 3)
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{retiring, staleReady("second", 5, 100, 3), ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none while the budget is spent", got.Retire)
	}
}

func TestDecideSizeCountsAForcedDrainAgainstTheBudget(t *testing.T) {
	// maxStaleSeconds escalated a retirement into a real drain. spec.retire
	// stays true across that, so it keeps occupying the budget it started
	// in — the server is still unavailable because of this update.
	forced := staleReady("first", 5, 100, 3)
	forced.Phase = phase.Draining
	forced.Retire = true
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{forced, staleReady("second", 5, 100, 3), ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none — the forced drain still holds the budget", got.Retire)
	}
}

func TestDecideSizeDoesNotCountAScaleDownDrainAgainstTheUpdateBudget(t *testing.T) {
	// The complement, and the reason spec.retire exists rather than the
	// phase being read: a server draining because demand fell was not made
	// unavailable by this update and must not spend its budget.
	unrelated := staleReady("shrinking", 0, 100, 3)
	unrelated.Phase = phase.Draining // Retire stays false
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{unrelated, staleReady("old", 5, 100, 3), ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Errorf("Retire = %v, want [old]", got.Retire)
	}
}

func TestDecideSizeCountsAReservedRetirementAgainstTheBudget(t *testing.T) {
	// The patch is out, the cache has not shown it. Counting only what the
	// cache shows would spend the budget twice.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			staleReady("first", 5, 100, 3),
			staleReady("second", 5, 100, 3),
			ready("new", 0, 100),
		},
		Generation: 0, MaxUnavailable: 1,
		PendingRetires: map[string]bool{"first": true},
		MinReplicas:    1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none while a reservation stands", got.Retire)
	}
}

func TestDecideSizeRetiresEmptyServersFirstThenTheOldest(t *testing.T) {
	base := time.Now()
	older := staleReady("older", 5, 100, 3)
	older.CreatedAt = base
	newer := staleReady("newer", 5, 100, 3)
	newer.CreatedAt = base.Add(time.Minute)
	empty := staleReady("empty", 0, 100, 3)
	empty.CreatedAt = base.Add(2 * time.Minute)

	got := DecideSize(ScalingInputs{
		Views:      []ServerView{newer, older, empty, ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "empty" {
		t.Fatalf("Retire = %v, want [empty] — an empty server costs nobody anything", got.Retire)
	}

	got = DecideSize(ScalingInputs{
		Views:      []ServerView{newer, older, ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 1 || got.Retire[0] != "older" {
		t.Errorf("Retire = %v, want [older] when none is empty", got.Retire)
	}
}

func TestDecideSizeDoesNotRetireAndShrinkInOnePass(t *testing.T) {
	// 4a's precedent: two removals decided in one pass are two decisions
	// taken on two readings of the same moment. Retirement comes first and
	// the pass ends there.
	idle := staleReady("idle", 0, 100, 3)
	idle.EmptyFor = time.Hour
	got := DecideSize(ScalingInputs{
		Views:      []ServerView{idle, staleReady("busy", 5, 100, 3), ready("new", 0, 100)},
		Generation: 0, MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: time.Minute,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none in a pass that retires", got.Delete)
	}
	if len(got.Retire) != 1 {
		t.Errorf("Retire = %v, want one", got.Retire)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestDecideSize(Retires|Respects|Counts|DoesNotRetire|DoesNotCount)' -v
```

Expected: compile failure — `unknown field MaxUnavailable`, `PendingRetires`, and `got.Retire undefined`.

- [ ] **Step 3: Add the inputs and the output field**

In `ScalingInputs`, after `Generation`:

```go
	// MaxUnavailable is spec.update.maxUnavailable: how many servers this
	// update may have unavailable at once.
	MaxUnavailable int32
	// PendingRetires are the servers this reconciler has asked to retire and
	// the cache has not shown yet.
	PendingRetires map[string]bool
```

In `SizeDecision`, after `Delete`:

```go
	// Retire names the stale servers to put into soft drain now. Never the
	// same server as Delete, and never in the same pass: retirement is how a
	// server leaves during a changeover, deletion is how it leaves for lack
	// of demand.
	Retire []string
```

- [ ] **Step 4: Add the nomination**

Add above `DecideSize`:

```go
// selectRetirement nominates one stale server for soft drain, or nothing.
//
// It cannot reuse SelectDeletionCandidates: that function refuses any server
// that may be carrying players, and those are precisely the ones a changeover
// has to retire. This is not a loosening of that rule — a retiring server is
// still never nominated for deletion — it is a different question asked of a
// narrower set: Ready, stale, not already retiring.
//
// Empty servers first, because retiring one costs nobody anything, then the
// oldest, so the longest-lived sessions are disturbed last. Ties by name, so
// two passes over the same state agree.
//
// One per pass. Every retirement is a replacement's worth of work, and the
// five-second resync converges quickly enough.
func selectRetirement(in ScalingInputs) string {
	var (
		readyCurrent bool
		unavailable  int32
		stale        []ServerView
	)
	for _, v := range in.Views {
		// spec.retire is the single budget signal. It survives the
		// escalation to Draining that maxStaleSeconds can force, so a
		// forced drain keeps holding the slot it started in — while a drain
		// that a scale-down or a deletion began never had it.
		if v.Retire || in.PendingRetires[v.Name] {
			unavailable++
			continue
		}
		if v.Generation == in.Generation {
			if v.Phase == phase.Ready {
				readyCurrent = true
			}
			continue
		}
		if v.Phase == phase.Ready && !in.PendingDeletes[v.Name] {
			stale = append(stale, v)
		}
	}
	// At least one ready server of the current generation, for every group
	// and not only for fallback targets: a ServerGroup cannot tell whether a
	// ProxyGroup names it, and learning to would cost a watch and a cache
	// that can be wrong for a distinction that only permits emptying a
	// non-fallback group faster.
	if !readyCurrent || unavailable >= in.MaxUnavailable || len(stale) == 0 {
		return ""
	}
	sort.SliceStable(stale, func(i, j int) bool {
		if ei, ej := stale[i].Players == 0, stale[j].Players == 0; ei != ej {
			return ei
		}
		if !stale[i].CreatedAt.Equal(stale[j].CreatedAt) {
			return stale[i].CreatedAt.Before(stale[j].CreatedAt)
		}
		return stale[i].Name < stale[j].Name
	})
	return stale[0].Name
}
```

Add `"sort"` to the file's imports.

- [ ] **Step 5: Place it in the order**

In `DecideSize`, between the ceiling block and the demand block — that is, after the `if surplus := alive - in.MaxReplicas; surplus > 0 { ... }` that follows the `if create > 0` block, and before the `if in.PendingCreates == 0 && alive > in.MinReplicas` demand block:

```go
	// Retirement, after the ceiling and before demand. The ceiling is an
	// instruction and outranks a changeover; demand does not, because a pass
	// that retires has already decided how this group loses a server and a
	// second removal would be a second decision on the same reading.
	if name := selectRetirement(in); name != "" {
		return SizeDecision{Retire: []string{name}}
	}
```

- [ ] **Step 6: Run to verify they pass**

```bash
nix develop -c go test ./internal/controller/ -v -cover
```

Expected: PASS across the package, coverage at or above 88%.

- [ ] **Step 7: Prove two of them can fail**

1. Change `unavailable >= in.MaxUnavailable` to `unavailable > in.MaxUnavailable`; confirm `TestDecideSizeRespectsTheUpdateBudget` fails. Revert.
2. Change the budget test `if v.Retire || in.PendingRetires[v.Name]` to `if v.Phase == phase.Retiring`; confirm `TestDecideSizeCountsAForcedDrainAgainstTheBudget` fails. Revert.

Report both outputs.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/scaling.go internal/controller/scaling_test.go
git commit -m "feat(4b): nominate one stale server at a time for soft drain"
```

---

### Task 7: The group controller asks

**Files:**
- Modify: `internal/controller/servergroup_controller.go`
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2–6.
- Produces: `(*ServerGroupReconciler).retireServer(ctx, group, servers, name) error`. Task 9's envtest exercises the whole path.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/servergroup_controller_test.go`, following the file's existing envtest setup:

```go
func TestGroupPatchesRetireOntoTheNominatedServer(t *testing.T) {
	// The group decides and the Server controller executes; spec.retire is
	// the whole channel between them. If this patch does not land, the
	// changeover is a rule nobody carries out.
	ctx := context.Background()
	group := newTestGroup(t, "retire-patch")   // follow the file's helper
	old := newTestServer(t, group, "old")      // stale: groupGeneration behind
	newSrv := newTestServer(t, group, "new")   // current generation, Ready

	markReady(t, old)
	markReady(t, newSrv)
	bumpGeneration(t, group)

	reconcileGroup(t, group)

	got := &spawneryv1alpha1.Server{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(old), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Spec.Retire {
		t.Error("the stale server was not asked to retire")
	}
}
```

Read the file first and use its actual helpers and assertion style; the names above are placeholders for whatever it already provides. If it has no such helpers, write the objects out longhand rather than inventing a helper layer this one test needs.

- [ ] **Step 2: Run to verify it fails**

```bash
nix develop -c go test ./internal/controller/ -run TestGroupPatchesRetire -v
```

Expected: FAIL, "the stale server was not asked to retire".

- [ ] **Step 3: Read `spec.retire` into the view**

In `collectViews`, in the `ServerView{...}` literal after `Generation`:

```go
				Retire:     srv.Spec.Retire,
```

- [ ] **Step 4: Pass the new inputs and execute the nomination**

In `size()`, replace the `_ = pendingRetires` line from Task 4 and extend the `DecideSize` call:

```go
	decision := DecideSize(ScalingInputs{
		Views:         views,
		MinReplicas:   group.Spec.Scaling.MinReplicas,
		MaxReplicas:   group.Spec.Scaling.MaxReplicas,
		SpareSlots:    group.Spec.Scaling.SpareSlots,
		MaxPlayers:    group.Spec.MaxPlayers,
		Stabilization: time.Duration(group.Spec.Scaling.ScaleDownStabilizationSeconds) * time.Second,

		Generation:     group.Generation,
		MaxUnavailable: group.UpdateMaxUnavailable(),

		PendingCreates: pendingCreates,
		PendingDeletes: pendingDeletes,
		PendingRetires: pendingRetires,
	})
```

After the `decision.Delete` loop, add:

```go
	for _, name := range decision.Retire {
		if err := r.retireServer(ctx, group, servers, name); err != nil {
			return decision, err
		}
		r.Expectations.expectRetired(key, name)
	}
```

- [ ] **Step 5: Add the accessor and the patch**

In `api/v1alpha1/servergroup_types.go`, beside the existing `DrainTimeout()` and `FailedRetention()` accessors:

```go
// UpdateMaxUnavailable is how many servers a rolling update may have
// unavailable at once. A group with no spec.update gets the CRD's own default,
// so the rule is the same whether the field was written out or left off.
func (g *ServerGroup) UpdateMaxUnavailable() int32 {
	if g.Spec.Update == nil || g.Spec.Update.MaxUnavailable < 1 {
		return 1
	}
	return g.Spec.Update.MaxUnavailable
}

// UpdateMaxStale is how long a server may wait in soft drain before its
// players are moved off. Zero means never.
func (g *ServerGroup) UpdateMaxStale() time.Duration {
	if g.Spec.Update == nil {
		return 0
	}
	return time.Duration(g.Spec.Update.MaxStaleSeconds) * time.Second
}
```

In `servergroup_controller.go`, beside `deleteServer`:

```go
// retireServer asks one server to enter soft drain.
//
// A patch rather than an update: the group holds a cached copy and the Server
// controller writes status on the same object, so a full update here would
// race it for no reason.
func (r *ServerGroupReconciler) retireServer(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	servers map[string]*spawneryv1alpha1.Server,
	name string,
) error {
	srv, ok := servers[name]
	if !ok {
		return nil
	}
	// Already asked. The nomination reads the cache, which lags the patch by
	// a reconcile or two, so without this the same server collects the same
	// event every resync for the whole of its retirement.
	if srv.Spec.Retire {
		return nil
	}
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Retire = true
	if err := r.Patch(ctx, srv, patch); err != nil {
		return err
	}
	r.Recorder.Eventf(group, corev1.EventTypeNormal, "ServerRetiring",
		"retiring server %s for a rolling update", name)
	return nil
}
```

- [ ] **Step 6: Run to verify it passes**

```bash
nix develop -c go test ./internal/controller/ -v
```

Expected: PASS.

- [ ] **Step 7: Prove it can fail**

Comment out the `decision.Retire` loop, run, confirm the test fails. Restore. Report the output.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/servergroup_controller.go api/v1alpha1/servergroup_types.go internal/controller/servergroup_controller_test.go
git commit -m "feat(4b): the group asks its stale servers to retire"
```

---

### Task 8: The server controller executes

**Files:**
- Modify: `internal/controller/server_controller.go`
- Test: `internal/controller/server_controller_test.go`

**Interfaces:**
- Consumes: `phase.Inputs.RetirementRequested`/`MaxStaleReached` (Task 1), the API fields (Task 2), `UpdateMaxStale()` (Task 7).
- Produces: nothing new. This closes the loop.

- [ ] **Step 1: Write the failing tests**

```go
func TestRetiringServerStampsItsClockAndDeregisters(t *testing.T) {
	ctx := context.Background()
	group := newTestGroup(t, "retire-exec")
	srv := newTestServer(t, group, "a")
	markReady(t, srv)                 // registered with the proxies

	patchRetire(t, srv, true)         // what the group controller does
	reconcileServer(t, srv)

	got := &spawneryv1alpha1.Server{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(srv), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != string(phase.Retiring) {
		t.Errorf("phase = %q, want Retiring", got.Status.Phase)
	}
	if got.Status.RetiringSince == nil {
		t.Error("retiringSince was not stamped, so maxStaleSeconds can never fire")
	}
	if got.Status.Registered {
		t.Error("a retiring server is still registered, so it is still taking joins")
	}
}

func TestMaxStaleZeroNeverForcesADrain(t *testing.T) {
	// The default. A retiring server with players waits indefinitely, which
	// is the promise the whole feature makes.
	in := (&ServerReconciler{Clock: func() time.Time { return time.Now() }}).
		collectInputs(retiringFor(time.Hour*24), groupWithMaxStale(0), nil, false)
	if in.MaxStaleReached {
		t.Error("MaxStaleReached with maxStaleSeconds = 0")
	}
}

func TestMaxStaleFiresOnceTheWaitExceedsIt(t *testing.T) {
	in := (&ServerReconciler{Clock: func() time.Time { return time.Now() }}).
		collectInputs(retiringFor(2*time.Minute), groupWithMaxStale(60), nil, false)
	if !in.MaxStaleReached {
		t.Error("MaxStaleReached is false after twice the configured wait")
	}
}
```

`retiringFor` and `groupWithMaxStale` are small local helpers this test file needs; write them beside the tests:

```go
func retiringFor(d time.Duration) *spawneryv1alpha1.Server {
	since := metav1.NewTime(time.Now().Add(-d))
	return &spawneryv1alpha1.Server{
		Status: spawneryv1alpha1.ServerStatus{
			Phase:         string(phase.Retiring),
			RetiringSince: &since,
		},
	}
}

func groupWithMaxStale(seconds int32) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Update: &spawneryv1alpha1.UpdateSpec{MaxStaleSeconds: seconds},
		},
	}
}
```

If `collectInputs` needs more of the reconciler than `Clock` (an `Agents` registry, for instance), build the minimal one the existing tests in that file already build rather than a new pattern.

- [ ] **Step 2: Run to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestRetiringServerStamps|TestMaxStale' -v
```

Expected: FAIL — the phase never becomes `Retiring`, and `MaxStaleReached` is always false.

- [ ] **Step 3: Build the two inputs**

In `collectInputs`, add to the `phase.Inputs{...}` literal:

```go
		RetirementRequested: srv.Spec.Retire,
```

and below the `DrainStartedAt` block:

```go
	// Measured from the wait in soft drain, not from the group's generation
	// change: a server still queued behind maxUnavailable is not failing to
	// empty, it has not been asked yet. Zero means never, which is the CRD
	// default and the promise that nobody is moved unless somebody asked for
	// it.
	if srv.Status.RetiringSince != nil {
		if window := group.UpdateMaxStale(); window > 0 {
			in.MaxStaleReached = now.Sub(srv.Status.RetiringSince.Time) >= window
		}
	}
```

- [ ] **Step 4: Stamp the clock**

In the status-timestamp switch, add beside the `phase.Draining` case:

```go
	case phase.Retiring:
		if srv.Status.RetiringSince == nil {
			srv.Status.RetiringSince = &now
		}
		srv.Status.ReadySince = nil
```

- [ ] **Step 5: Give the stand-in group the defaults**

In `fallbackGroup`, add to the `ServerGroupSpec` literal so a Server that outlives its group still behaves:

```go
			Update: &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1, MaxStaleSeconds: 0},
```

- [ ] **Step 6: Run to verify they pass**

```bash
nix develop -c go test ./internal/controller/ -v -cover
```

Expected: PASS, coverage at or above 88%.

- [ ] **Step 7: Prove one can fail**

Change `>= window` to `> 24*time.Hour`, run, confirm `TestMaxStaleFiresOnceTheWaitExceedsIt` fails. Revert. Report the output.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/server_controller.go internal/controller/server_controller_test.go
git commit -m "feat(4b): a server carries out its retirement and bounds the wait"
```

---

### Task 9: The changeover, end to end

**Files:**
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: everything.
- Produces: nothing.

- [ ] **Step 1: Write the test**

This one is written last on purpose: it asserts the behaviour the milestone promises rather than any single rule, so it should pass the moment Tasks 1–8 are correct and fail if any of them regresses.

```go
func TestARollingUpdateReplacesAnOccupiedGroupWithoutKickingAnyone(t *testing.T) {
	// The milestone's acceptance criterion, in one test: two occupied stale
	// servers, a spec change, and a group that ends up entirely on the new
	// generation with nobody having been moved.
	ctx := context.Background()
	group := newTestGroup(t, "changeover")
	a := newTestServer(t, group, "a")
	b := newTestServer(t, group, "b")
	markReadyWithPlayers(t, a, 60)
	markReadyWithPlayers(t, b, 60)

	bumpGeneration(t, group)

	// 1. The cold start: exactly one server of the new generation.
	reconcileGroup(t, group)
	fresh := serversOfGeneration(t, group, group.Generation)
	if len(fresh) != 1 {
		t.Fatalf("cold start created %d servers, want exactly 1", len(fresh))
	}

	// 2. Nothing retires until it is Ready.
	reconcileGroup(t, group)
	if n := retiringCount(t, group); n != 0 {
		t.Fatalf("%d servers retiring before a replacement was Ready", n)
	}

	// 3. Once it is, one stale server retires — and only one, because
	//    maxUnavailable defaults to 1.
	markReady(t, fresh[0])
	reconcileGroup(t, group)
	reconcileGroup(t, group)
	if n := retiringCount(t, group); n != 1 {
		t.Fatalf("%d servers retiring, want exactly 1 (maxUnavailable)", n)
	}

	// 4. The retiring server keeps its players and is not deleted.
	retiring := firstRetiring(t, group)
	if !retiring.DeletionTimestamp.IsZero() {
		t.Error("a retiring server with players was deleted")
	}
	if retiring.Status.Registered {
		t.Error("a retiring server is still registered")
	}
}
```

Use the file's own helpers; write the missing ones beside the test in the same style. If reconciles in that suite are driven by a running manager rather than by direct `Reconcile` calls, use `Eventually` with the suite's existing timeout instead of the explicit `reconcileGroup` calls above.

- [ ] **Step 2: Run it**

```bash
nix develop -c go test ./internal/controller/ -run TestARollingUpdate -v
```

Expected: PASS. A failure here is a real defect in Tasks 1–8, not a test to adjust — find which step's assertion broke and fix the rule it names.

- [ ] **Step 3: Prove it can fail**

Revert Task 3's `leaving()` change, run, confirm the test fails (the retiring server keeps holding the group's size, so the changeover stalls). Re-apply. Report the output.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/servergroup_controller_test.go
git commit -m "test(4b): run a whole changeover against an occupied group"
```

---

### Task 10: 4a's leftover in `provisionalCapacity`

**Files:**
- Modify: `internal/controller/scaling.go`
- Test: `internal/controller/scaling_test.go`

**Interfaces:** none. Independent of every other task; it is here because 4b is in this file anyway.

**Background:** `provisionalCapacity` cannot tell "has never reported" from "the pod vanished" — both present as `Slots == 0`, because `Registry.Lookup` on an unknown pod returns a zero snapshot. So a server whose pod is gone is credited a full `maxPlayers` it does not have, until the Server controller fails it. **The obvious fix is a regression:** testing `Stale` before `Slots == 0` also credits a genuinely starting server zero, which reintroduces the runaway scale-up the rule exists to prevent. `ServerView.SessionsGone` is the right signal and is already on the view.

- [ ] **Step 1: Write the failing test**

```go
func TestProvisionalCapacityDoesNotCreditAServerWhosePodIsGone(t *testing.T) {
	// Slots == 0 means two different things: a server that has never
	// reported (capacity on its way, credit it) and one whose pod vanished
	// (nothing there, credit nothing). SessionsGone is what separates them.
	gone := ServerView{Name: "a", Phase: phase.Starting, Stale: true, SessionsGone: true}
	if got := provisionalCapacity(gone, 100); got != 0 {
		t.Errorf("provisionalCapacity = %d, want 0 for a server whose pod is gone", got)
	}
}

func TestProvisionalCapacityStillCreditsAStartingServer(t *testing.T) {
	// The guard against the obvious wrong fix: a starting server is stale
	// and has never reported too, and crediting it zero brings back the
	// runaway scale-up 4a built this rule to stop.
	if got := provisionalCapacity(starting("a"), 100); got != 100 {
		t.Errorf("provisionalCapacity = %d, want the full 100 for a starting server", got)
	}
}
```

- [ ] **Step 2: Run to verify the first fails**

```bash
nix develop -c go test ./internal/controller/ -run TestProvisionalCapacity -v
```

Expected: the first FAILs with `= 100, want 0`; the second PASSes and is the regression guard.

- [ ] **Step 3: Add the one line**

At the top of `provisionalCapacity`, after the `countsTowardSize` check:

```go
	// Before the Slots == 0 credit below, and not merged into it: a server
	// whose pod is gone reads exactly like one that has never reported, and
	// only this flag tells them apart. Testing Stale here instead would be a
	// regression — a genuinely starting server is stale too, and crediting it
	// zero is the runaway this function exists to prevent.
	if v.SessionsGone {
		return 0
	}
```

- [ ] **Step 4: Run to verify both pass**

```bash
nix develop -c go test ./internal/controller/ -v -cover
```

Expected: PASS across the package.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/scaling.go internal/controller/scaling_test.go
git commit -m "fix(4b): stop crediting capacity to a server whose pod is gone"
```

---

## Before the branch is merged

- [ ] `nix develop -c make test` — green, coverage at or above 88% for `internal/controller` and 100% for `internal/phase`.
- [ ] `nix develop -c make manifests` — no diff beyond Task 2's two fields.
- [ ] `git diff --name-only master...HEAD` — nothing under `agent/`, `image/`, `proto/`, `nix/`.
- [ ] `docs/known-issues.md` gains a "From milestone 4b" section: the group at its ceiling that cannot start a changeover (Task 5), and the cold-start loop that remains the backoff spec's to close properly.
- [ ] `docs/handover-milestone-4.md`'s "4a has landed" section gains 4b, and the sub-project table in `docs/handover-milestone-4b.md` marks 4b done.
- [ ] One whole-branch review before merge. On 4a it found a fixed point no per-task review could see; the equivalent risk here is the interaction between the cold start, the ceiling and the budget, which no single task's tests exercise together.

## Self-review

**Spec coverage.** §3.1 → Task 1. §3.2 → Task 3 (`leaving()`) and Task 6 (the generation confined to nomination). §3.3 → Task 5. §3.4 → Task 6's `readyCurrent`. §3.5 → Tasks 2 and 8. §3.6 → Tasks 2 and 7. §3.7 → Task 5. §3.8 → Tasks 4 and 6. §4.1 → Task 2. §4.2 → Task 1. §4.3 → Task 3. §4.4 → Tasks 5, 6, 10. §4.5 → Task 4. §4.6 → Task 7. §4.7 → Task 8. §4.8 → nothing, correctly: the spec's claim is that `fleet.go` needs no change, and Task 9 is what would catch it if that were wrong. §9's test strategy → Tasks 1–10's tests. §10's acceptance criteria → the pre-merge checklist plus Task 9.

**One thing the spec asserts that no task proves directly:** that a retiring server never receives a `DrainPlayers`. It follows from `fleet.go` matching only `phase.Draining`, which Task 1 does not change and no task touches. Task 9's step 4 checks the observable half — the server is deregistered and undeleted — which is what a player would feel. A test inside `internal/proxyreg` asserting the negative would be stronger; it is not in this plan because it belongs to that package's own suite, and it is written down here so the whole-branch review can decide whether to add it.

**Placeholders:** none. Every step carries its code. Tasks 7, 8 and 9 name helper functions from the existing envtest suites rather than inventing them, and each says to read the file first and follow what is there.

**Type consistency:** `ServerView.Retire` (Task 3) is read by `selectRetirement` (Task 6), written by `collectViews` (Task 7) and observed by `expectations` (Task 4) under that one name. `pending` returns `(int32, map[string]bool, map[string]bool)` from Task 4 onward, and Task 7 is the only caller. `SizeDecision.Retire []string` (Task 6) is consumed by `size()` (Task 7). `phase.Retiring`, `ReasonRetiring`, `ReasonMaxStaleElapsed`, `Inputs.RetirementRequested` and `Inputs.MaxStaleReached` (Task 1) are used by Tasks 3, 6, 7 and 8 under exactly those names. `UpdateMaxUnavailable()` and `UpdateMaxStale()` are both added in Task 7 and the second is first used in Task 8.
