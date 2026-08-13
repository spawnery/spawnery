# Milestone 4a: slot-based scaling — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An ephemeral `ServerGroup` creates servers when its free player slots fall below `spec.scaling.spareSlots` and removes them when they have been empty for `scaleDownStabilizationSeconds`, without ever ordering the same replacement twice.

**Architecture:** One new pure function, `DecideSize`, joins `phase.Decide`, `SelectDeletionCandidates` and `AggregateGroup` as a decision over value types; `ServerGroupReconciler.size` becomes its executor. Two supporting pieces: an "empty since" clock reading in the in-memory agent registry, and a per-group reservation of the creates and deletes the reconciler has issued but the cache has not yet shown.

**Tech Stack:** Go 1.24, controller-runtime, envtest, Kubernetes 1.36 API. No proto change, no agent change, no image change, no CRD schema change.

**Spec:** `docs/superpowers/specs/2026-08-13-slot-based-scaling-design.md`. Read it before Task 1; §4.2 in particular.

## Global Constraints

- **Commit messages use Conventional Commits** — `feat(4a):`, `fix(4a):`, `test(4a):`, `docs(4a):`, `refactor(4a):`. This deliberately overrides the repository's older sentence-style history. Every commit ends with the trailer `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- **Every Go command runs through the Nix shell:** `nix develop -c make test`. No `--extra-experimental-features` prefix is needed on this machine; `~/.config/nix/nix.conf` already sets `experimental-features = nix-command flakes`.
- **`make test` takes about 38 seconds**, of which `internal/controller` is about 34 — envtest boots a real API server. A slow run is not a hang.
- To run one package: `nix develop -c go test ./internal/agent/...`. To run one test: `nix develop -c go test ./internal/controller/ -run TestName -v`.
- **Licence header.** Every new `.go` file starts with the same Apache 2.0 header every existing file carries; copy it verbatim from `internal/controller/candidates.go`.
- **No new CRD field.** `make manifests` must produce no diff. If it does, something was added that this milestone does not add.
- **British/American spelling, comment voice:** match the surrounding files. Comments explain *why*, not *what*; this repository's comments carry the reasoning that would otherwise be lost.
- **Branch:** `milestone-4a`, already created, with the spec committed at `cabc532`.

## File structure

| File | Responsibility |
|---|---|
| `internal/agent/registry.go` (modify) | gains `entry.emptySince` and `Snapshot.EmptyFor` |
| `internal/agent/registry_test.go` (modify) | the four edges of emptiness |
| `internal/controller/candidates.go` (modify) | `ServerView.EmptyFor`; `clampReport` |
| `internal/controller/candidates_test.go` (modify) | `clampReport` table |
| `internal/controller/scaling.go` (create) | `ScalingInputs`, `SizeDecision`, `DecideSize`, `provisionalCapacity`, `readyContribution`, `deletable` |
| `internal/controller/scaling_test.go` (create) | the whole rule set, without a cluster |
| `internal/controller/expectations.go` (create) | the per-group reservation |
| `internal/controller/expectations_test.go` (create) | observation, expiry, counting |
| `internal/controller/servergroup_controller.go` (modify) | `collectViews` clamps and fills `EmptyFor`; `size` executes the decision; `Reconcile` publishes the condition |
| `internal/controller/setup.go` (modify) | constructs the expectations |
| `api/v1alpha1/common_types.go` (modify) | `ConditionScalingLimited`, two reasons |
| `api/v1alpha1/servergroup_types.go` (modify) | `DesiredReplicas` doc comment |
| `internal/controller/servergroup_controller_test.go` (modify) | the envtest that carries the milestone |
| `README.md`, `docs/known-issues.md`, `docs/handover-milestone-4.md` (modify) | Task 8 |

---

### Task 1: `emptySince` in the agent registry

**Files:**
- Modify: `internal/agent/registry.go`
- Test: `internal/agent/registry_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `agent.Snapshot.EmptyFor time.Duration` — how long the agent has been reporting zero players; zero when it has players and zero when it has never reported.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/registry_test.go`:

```go
func TestEmptyForStartsWhenTheCountReachesZero(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	if err := r.ReportPlayers("pod-uid-1", 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	if got := r.Lookup("pod-uid-1").EmptyFor; got != 0 {
		t.Errorf("EmptyFor = %v on an occupied server, want 0", got)
	}

	if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	clock.Advance(90 * time.Second)
	if got := r.Lookup("pod-uid-1").EmptyFor; got != 90*time.Second {
		t.Errorf("EmptyFor = %v, want 90s since the count reached zero", got)
	}

	// A second zero report does not restart the clock: the server has been
	// empty since the first one, and the stabilization window measures that.
	if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	if got := r.Lookup("pod-uid-1").EmptyFor; got != 90*time.Second {
		t.Errorf("EmptyFor = %v after a repeated zero report, want 90s", got)
	}
}

func TestEmptyForClearsWhenPlayersReturn(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	clock.Advance(time.Minute)
	if err := r.ReportPlayers("pod-uid-1", 1, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	if got := r.Lookup("pod-uid-1").EmptyFor; got != 0 {
		t.Errorf("EmptyFor = %v after a player joined, want 0", got)
	}
}

func TestEmptyForIsZeroBeforeTheFirstReport(t *testing.T) {
	r, clock := newTestRegistry()
	r.Connect("pod-uid-1", RoleServer)
	clock.Advance(time.Minute)

	if got := r.Lookup("pod-uid-1").EmptyFor; got != 0 {
		t.Errorf("EmptyFor = %v before the first report, want 0: a server that "+
			"has never reported is not known to be empty", got)
	}
}

// TestEmptyForAcrossStreamChanges pins the three edges the design fixes on
// purpose. Connect may have a restarted process behind it and must forget what
// the previous one reported; Supersede cannot, because the displaced stream was
// still live; Disconnect keeps it, which is inert because the count goes stale
// and stale counts as occupied.
func TestEmptyForAcrossStreamChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event func(r *Registry)
		want  time.Duration
	}{
		{"connect clears it", func(r *Registry) { r.Connect("pod-uid-1", RoleServer) }, 0},
		{"supersede keeps it", func(r *Registry) { r.Supersede("pod-uid-1", RoleServer) }, time.Minute},
		{"disconnect keeps it", func(r *Registry) { r.Disconnect("pod-uid-1") }, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, clock := newTestRegistry()
			r.Connect("pod-uid-1", RoleServer)
			if err := r.ReportPlayers("pod-uid-1", 0, 100); err != nil {
				t.Fatalf("ReportPlayers: %v", err)
			}
			clock.Advance(time.Minute)

			tc.event(r)

			if got := r.Lookup("pod-uid-1").EmptyFor; got != tc.want {
				t.Errorf("EmptyFor = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/agent/ -run TestEmptyFor -v`
Expected: FAIL to compile — `got.EmptyFor undefined (type Snapshot has no field or method EmptyFor)`.

- [ ] **Step 3: Implement it**

In `internal/agent/registry.go`, add the field to `Snapshot`, after `PlayersStale`:

```go
	// EmptyFor is how long the agent has been reporting zero players. It is
	// zero while players are on, and zero before the first report — a server
	// that has never reported is not known to be empty.
	//
	// It never decides anything on its own. Every rule that reads it also asks
	// players == 0 && !PlayersStale, because a server that was never empty
	// reports zero here too, and scaleDownStabilizationSeconds may be 0.
	EmptyFor time.Duration
```

Add the field to `entry`, after `slots`:

```go
	emptySince     time.Time
```

In `ReportPlayers`, after `e.slots = slots`:

```go
	// The stabilization window measures from the moment the server became
	// empty, not from the last report that found it empty, so a repeated zero
	// must not restart it.
	if players == 0 {
		if e.emptySince.IsZero() {
			e.emptySince = r.now()
		}
	} else {
		e.emptySince = time.Time{}
	}
```

In `connect`, inside the existing `if !keepReady` block:

```go
	if !keepReady {
		e.ready = false
		// A fresh stream may have a restarted process behind it, and that
		// process has reported nothing yet — the same reasoning that clears
		// readiness here. A superseding stream cannot: the displaced one was
		// still live, so the emptiness it observed still holds.
		e.emptySince = time.Time{}
	}
```

In `Lookup`, after the `snap.PlayersStale` assignment:

```go
	if !e.emptySince.IsZero() {
		snap.EmptyFor = now.Sub(e.emptySince)
	}
```

`Disconnect` is not touched: keeping the timestamp there changes no decision, because the count goes stale within twice the report interval and a stale count is treated as occupied everywhere in this repository.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/agent/ -v`
Expected: PASS, including the existing tests.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/registry.go internal/agent/registry_test.go
git commit -m "$(cat <<'EOF'
feat(4a): record how long an agent has been reporting zero players

scaleDownStabilizationSeconds needs a moment to measure from, and none
existed. entry.emptySince is stamped on the flank into zero and cleared on
the flank out of it, so a repeated zero report does not restart the window.

Connect clears it and Supersede keeps it, for the same reason keepReady
already distinguishes them: a fresh stream may have a restarted process
behind it, a superseding one cannot. Disconnect keeps it, which is inert —
the count goes stale within twice the report interval and stale counts as
occupied.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `ServerView.EmptyFor` and the `slots` clamp

**Files:**
- Modify: `internal/controller/candidates.go`
- Modify: `internal/controller/servergroup_controller.go` (`collectViews`, around line 296)
- Test: `internal/controller/candidates_test.go`

**Interfaces:**
- Consumes: `agent.Snapshot.EmptyFor` from Task 1.
- Produces: `ServerView.EmptyFor time.Duration`; `clampReport(players, slots, maxPlayers int32) (int32, int32)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/candidates_test.go`:

```go
func TestClampReport(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		players, slots, maxPlay  int32
		wantPlayers, wantSlots   int32
	}{
		{"an honest report passes through", 7, 100, 100, 7, 100},
		{"a smaller capacity than the group's is the agent's business", 7, 40, 100, 7, 40},
		{"slots above the group's capacity are cut to it", 0, 1000000, 100, 0, 100},
		{"players follow the cut capacity down", 900, 1000, 100, 100, 100},
		{"a zero capacity leaves nothing", 5, 50, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotPlayers, gotSlots := clampReport(tc.players, tc.slots, tc.maxPlay)
			if gotPlayers != tc.wantPlayers || gotSlots != tc.wantSlots {
				t.Errorf("clampReport(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tc.players, tc.slots, tc.maxPlay,
					gotPlayers, gotSlots, tc.wantPlayers, tc.wantSlots)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `nix develop -c go test ./internal/controller/ -run TestClampReport`
Expected: FAIL to compile — `undefined: clampReport`.

- [ ] **Step 3: Implement it**

In `internal/controller/candidates.go`, add the field to `ServerView`, after `Slots`:

```go
	// EmptyFor is how long this server has been reporting zero players. Like
	// agent.Snapshot.EmptyFor it decides nothing on its own: a server that was
	// never empty carries zero here too, so every rule that reads it also asks
	// Players == 0 && !Stale.
	EmptyFor time.Duration
```

Add the helper, below `isOccupied`:

```go
// clampReport bounds what an agent reports about itself by the capacity the
// operator handed its group.
//
// Registry.ReportPlayers rejects a player count above the reported slots, but
// checks the slots against nothing, because it does not know which group a pod
// belongs to. Here the two meet. From milestone 4a the reported slots feed the
// group's scaling decision, so one pod reporting slots: 1000000 at zero players
// would make its whole group look permanently spacious and suppress every
// scale-up for every server in it — an effect reaching across pod boundaries,
// which is exactly what the agent channel's design rules out everywhere else.
//
// The players are cut to the clamped capacity rather than to maxPlayers, so a
// group whose servers legitimately report a smaller capacity than the group's
// bound stays consistent with itself.
func clampReport(players, slots, maxPlayers int32) (int32, int32) {
	if maxPlayers < 0 {
		maxPlayers = 0
	}
	if slots > maxPlayers {
		slots = maxPlayers
	}
	if players > slots {
		players = slots
	}
	return players, slots
}
```

In `internal/controller/servergroup_controller.go`, in `collectViews`, replace the construction of `v` with a clamped one. The current code reads:

```go
		snap := r.Agents.Lookup(podUID(pod, podFound))
		v := ServerView{
			Name:    srv.Name,
			Phase:   phase.Phase(srv.Status.Phase),
			Players: snap.Players,
			Slots:   snap.Slots,
			Stale:   snap.PlayersStale,
```

Replace those lines with:

```go
		snap := r.Agents.Lookup(podUID(pod, podFound))
		players, slots := clampReport(snap.Players, snap.Slots, group.Spec.MaxPlayers)
		if players != snap.Players || slots != snap.Slots {
			log.FromContext(ctx).V(1).Info("agent report clamped to the group's capacity",
				"server", srv.Name, "reportedPlayers", snap.Players, "reportedSlots", snap.Slots,
				"maxPlayers", group.Spec.MaxPlayers)
		}
		v := ServerView{
			Name:     srv.Name,
			Phase:    phase.Phase(srv.Status.Phase),
			Players:  players,
			Slots:    slots,
			EmptyFor: snap.EmptyFor,
			Stale:    snap.PlayersStale,
```

Leave the remaining fields of the literal — `WasRegistered`, `SessionsGone`, `Generation`, `CreatedAt` — exactly as they are, comments included, and re-align the `:` column of the whole literal with `gofmt`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/controller/ -run 'TestClampReport'` then `nix develop -c make test`
Expected: PASS. `make test` must stay green — this changes what `collectViews` produces for every existing group test.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/candidates.go internal/controller/candidates_test.go internal/controller/servergroup_controller.go
git commit -m "$(cat <<'EOF'
feat(4a): bound an agent's reported capacity by its group's

The registry rejects players above slots and checks slots against nothing,
because it does not know a pod's group. collectViews is where the two meet,
so the clamp goes there.

Today the reported slots only reach status.slots and are cosmetic. From the
next task they feed the scaling decision for the whole group, and one pod
reporting slots: 1000000 would suppress every scale-up for every server in
it — an effect across pod boundaries the agent channel otherwise rules out.

ServerView also carries EmptyFor from here on, unread until task 4.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `DecideSize` — provisional capacity, creates, and the ceiling

**Files:**
- Create: `internal/controller/scaling.go`
- Test: `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: `ServerView` (with `EmptyFor` from Task 2), `SelectDeletionCandidates`, `AggregateGroup`, `ServerView.countsTowardSize`.
- Produces:
  - `type ScalingInputs struct { Views []ServerView; MinReplicas, MaxReplicas, SpareSlots, MaxPlayers int32; Stabilization time.Duration; PendingCreates int32; PendingDeletes map[string]bool }`
  - `type SizeDecision struct { Create int32; Delete []string; Wanted int32; Surplus int32; Limited bool }`
  - `func DecideSize(in ScalingInputs) SizeDecision`
  - `func provisionalCapacity(v ServerView, maxPlayers int32) int32`
  - `func deletable(in ScalingInputs) []ServerView`

**Read first:** spec §3 and §4.2. Two things there are easy to get wrong and are the reason this task exists:

1. The provisional-capacity rule is **not** `AggregateGroup(...).FreeSlots`. Do not substitute it.
2. **Nothing here reads `ServerView.Generation`.** `ScalingInputs` has no generation field on purpose. A generation-aware scale-up would order a full replacement set on every spec edit, because every edit raises the group's generation — a rolling update without any of 4b's brakes. The generation arrives in 4b with the rules that make it safe.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/scaling_test.go` with the licence header copied from `candidates.go`, then:

```go
package controller

import (
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// ready builds a Ready server with a fresh report. ServerView.Generation is
// left at zero throughout this file: the sizing rules do not read it, and a
// test that set it would suggest they did.
func ready(name string, players, slots int32) ServerView {
	return ServerView{
		Name: name, Phase: phase.Ready, Players: players, Slots: slots,
		WasRegistered: true,
	}
}

// starting builds a server that has a pod and has never reported: no slots, and
// a count that is therefore stale.
func starting(name string) ServerView {
	return ServerView{Name: name, Phase: phase.Starting, Stale: true}
}

func TestDecideSizeCreatesTheFloor(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 2, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100,
	})
	if got.Create != 2 {
		t.Errorf("Create = %d, want 2 to reach the floor", got.Create)
	}
}

func TestDecideSizeCreditsCapacityThatIsOrderedButNotArrived(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ScalingInputs
		want int32
	}{
		{
			// The whole point of the milestone: a server that has a pod and has
			// not reported yet is capacity on its way, not a hole to fill.
			name: "a starting server covers the spare slots",
			in: ScalingInputs{
				Views:       []ServerView{starting("a")},
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
			},
			want: 0,
		},
		{
			name: "a create the cache has not shown yet counts the same way",
			in: ScalingInputs{
				MinReplicas: 1, MaxReplicas: 10,
				SpareSlots: 40, MaxPlayers: 100, PendingCreates: 1,
			},
			want: 0,
		},
		{
			// A server that reported once and then went quiet is not capacity:
			// unknown counts as occupied throughout this repository.
			name: "a server whose count went stale credits nothing",
			in: ScalingInputs{
				Views: []ServerView{{
					Name: "a", Phase: phase.Ready, Slots: 100, Stale: true,
					WasRegistered: true,
				}},
				MinReplicas: 1, MaxReplicas: 10,
				SpareSlots: 40, MaxPlayers: 100,
			},
			want: 1,
		},
		{
			// 4a does not roll updates, so it does not read the generation at
			// all. A rule that did would order a full replacement set on every
			// spec edit, because every edit raises the group's generation.
			name: "a server of another generation still credits its capacity",
			in: ScalingInputs{
				Views: []ServerView{{
					Name: "a", Phase: phase.Ready, Slots: 100,
					WasRegistered: true, Generation: 7,
				}},
				MinReplicas: 1, MaxReplicas: 10,
				SpareSlots: 40, MaxPlayers: 100,
			},
			want: 0,
		},
		{
			name: "a draining server credits nothing",
			in: ScalingInputs{
				Views:       []ServerView{{Name: "a", Phase: phase.Draining, Slots: 100}},
				MinReplicas: 0, MaxReplicas: 10,
				SpareSlots: 40, MaxPlayers: 100,
			},
			want: 1,
		},
		{
			name: "a server pending deletion credits nothing",
			in: ScalingInputs{
				Views:       []ServerView{ready("a", 0, 100)},
				MinReplicas: 0, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
				PendingDeletes: map[string]bool{"a": true},
			},
			want: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideSize(tc.in).Create; got != tc.want {
				t.Errorf("Create = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDecideSizeRoundsTheShortfallUp(t *testing.T) {
	for _, tc := range []struct {
		name       string
		free       int32
		spare      int32
		wantCreate int32
	}{
		{"no shortfall", 100, 40, 0},
		{"exactly at the mark", 40, 40, 0},
		{"one slot short orders one server", 39, 40, 1},
		{"a shortfall of exactly one server orders one", 0, 100, 1},
		{"one slot more orders two", 0, 101, 2},
		{"a large shortfall orders the ceiling of the quotient", 0, 250, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideSize(ScalingInputs{
				Views:       []ServerView{ready("a", 100-tc.free, 100)},
				MinReplicas: 1, MaxReplicas: 10,
				SpareSlots:  tc.spare, MaxPlayers: 100,
			})
			if got.Create != tc.wantCreate {
				t.Errorf("Create = %d, want %d", got.Create, tc.wantCreate)
			}
		})
	}
}

func TestDecideSizeReportsTheCeilingHoldingCapacityBack(t *testing.T) {
	in := ScalingInputs{
		Views:       []ServerView{ready("a", 100, 100), ready("b", 100, 100)},
		MinReplicas: 1, MaxReplicas: 2,
		SpareSlots:  40, MaxPlayers: 100,
	}
	got := DecideSize(in)
	if got.Create != 0 {
		t.Errorf("Create = %d, want 0 at the ceiling", got.Create)
	}
	if got.Wanted != 1 {
		t.Errorf("Wanted = %d, want 1 — the rule asked for one before the ceiling cut it", got.Wanted)
	}
	if !got.Limited {
		t.Error("Limited = false, want true: the ceiling is holding capacity back")
	}
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: a group short of capacity does not also shrink", got.Delete)
	}

	in.MaxReplicas = 5
	if got := DecideSize(in); got.Limited || got.Create != 1 {
		t.Errorf("with room: Create = %d, Limited = %v, want 1 and false", got.Create, got.Limited)
	}
}

func TestDecideSizeShrinksToALoweredCeilingWithoutWaiting(t *testing.T) {
	// A lowered maxReplicas is an instruction, not a suggestion: the
	// stabilization window does not apply. SelectDeletionCandidates still
	// refuses any server that may be carrying players, which is what keeps
	// this safe.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			ready("a", 0, 100), ready("b", 0, 100), ready("c", 5, 100),
		},
		MinReplicas: 1, MaxReplicas: 2,
		SpareSlots:  40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 {
		t.Fatalf("Delete = %v, want exactly one name", got.Delete)
	}
	if got.Delete[0] == "c" {
		t.Error("nominated the occupied server — core invariant broken")
	}
	if got.Surplus != 1 {
		t.Errorf("Surplus = %d, want 1", got.Surplus)
	}
}

func TestDecideSizeNeverNominatesAServerAlreadyBeingRemoved(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			ready("a", 0, 100), ready("b", 0, 100), ready("c", 0, 100),
		},
		MinReplicas: 1, MaxReplicas: 2,
		SpareSlots:  40, MaxPlayers: 100,
		// Set, so the demand rule of task 4 finds no stabilized candidate and
		// this test keeps asserting only what its name says.
		Stabilization:  5 * time.Minute,
		PendingDeletes: map[string]bool{"a": true},
	})
	for _, name := range got.Delete {
		if name == "a" {
			t.Fatal("nominated a server whose deletion has already been asked for")
		}
	}
	// a is gone from the count, so b and c are already at the ceiling of 2.
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none once the pending removal is counted", got.Delete)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/controller/ -run TestDecideSize`
Expected: FAIL to compile — `undefined: DecideSize`.

- [ ] **Step 3: Implement it**

Create `internal/controller/scaling.go` with the licence header, then:

```go
package controller

import "time"

// ScalingInputs is everything the sizing decision needs. Like the other
// decisions in this package it is a value type, so the rules are pure and
// table-tested without a cluster.
// Nothing here is the group's generation, and that is deliberate. Every edit to
// a ServerGroup spec raises it, so a scale-up rule that only credited servers of
// the current generation would order a full replacement set on the next
// five-second pass after any edit — a rolling update without maxUnavailable,
// without soft drain and without the guarantee of one ready server of the new
// generation. Those rules, and with them the generation, arrive in milestone 4b.
type ScalingInputs struct {
	// Views is what the cache shows of the group's servers.
	Views []ServerView
	// MinReplicas is the floor the group is held at.
	MinReplicas int32
	// MaxReplicas is the ceiling it may not pass.
	MaxReplicas int32
	// SpareSlots is the free player capacity the group keeps available.
	SpareSlots int32
	// MaxPlayers is the capacity of a single server of this group.
	MaxPlayers int32
	// Stabilization is how long a server must have been empty before it may
	// be removed for lack of demand.
	Stabilization time.Duration
	// PendingCreates is how many servers the reconciler has created and the
	// cache has not shown yet.
	PendingCreates int32
	// PendingDeletes are the servers whose removal it has already asked for
	// and the cache still shows.
	PendingDeletes map[string]bool
}

// SizeDecision is what the group does about its size this pass.
type SizeDecision struct {
	// Create is how many servers to create now.
	Create int32
	// Delete names the servers to remove now.
	Delete []string
	// Wanted is how many servers the spare-slot rule asked for, before the
	// ceiling. Wanted > Create is the definition of Limited.
	Wanted int32
	// Surplus is how many servers the ceiling asked to have removed, whether
	// or not that many could be nominated.
	Surplus int32
	// Limited is true while maxReplicas is holding capacity back.
	Limited bool
}

// provisionalCapacity is one server's contribution to the figure the scale-up
// rule reads. It is deliberately not AggregateGroup's FreeSlots.
//
// A server created now is not Ready for tens of seconds and contributes nothing
// to FreeSlots. At a five-second resync a scaler reading FreeSlots would see the
// same shortfall six to twelve times and order the same replacement each time,
// until maxReplicas stopped it. That is not an edge case — it is what every
// scale-up would do.
//
// So capacity that has been ordered counts before it arrives. Slots == 0 is what
// separates a server still starting up, which has never reported, from one whose
// agent went quiet, which has: the first is credited in full, the second not at
// all, because unknown counts as occupied everywhere in this repository.
//
// status.freeSlots keeps AggregateGroup's meaning — Ready servers of the current
// generation — because that is what its CRD field documents and what the rolling
// update of milestone 4b needs. Two numbers, two purposes; they must not be
// unified.
func provisionalCapacity(v ServerView, maxPlayers int32) int32 {
	if !v.countsTowardSize() {
		return 0
	}
	if v.Slots == 0 {
		return maxPlayers
	}
	if v.Stale {
		return 0
	}
	if free := v.Slots - v.Players; free > 0 {
		return free
	}
	return 0
}

// deletable is the candidate pool: what the cache shows, minus the servers
// whose removal this reconciler has already asked for. Leaving them in would
// let one pass nominate the same server the previous pass already deleted, and
// count it twice against the surplus.
func deletable(in ScalingInputs) []ServerView {
	if len(in.PendingDeletes) == 0 {
		return in.Views
	}
	out := make([]ServerView, 0, len(in.Views))
	for _, v := range in.Views {
		if !in.PendingDeletes[v.Name] {
			out = append(out, v)
		}
	}
	return out
}

// DecideSize is the group's sizing rule.
//
// The order matters and is the design's, not an accident: capacity first, then
// the ceiling, then demand. A group that is short of capacity never also
// shrinks in the same pass.
func DecideSize(in ScalingInputs) SizeDecision {
	alive := in.PendingCreates
	provisional := in.PendingCreates * in.MaxPlayers
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] {
			continue
		}
		if v.countsTowardSize() {
			alive++
		}
		provisional += provisionalCapacity(v, in.MaxPlayers)
	}

	var wanted int32
	if in.MaxPlayers > 0 && provisional < in.SpareSlots {
		gap := in.SpareSlots - provisional
		wanted = (gap + in.MaxPlayers - 1) / in.MaxPlayers
	}

	create := wanted
	if floor := in.MinReplicas - alive; floor > create {
		create = floor
	}
	if create > 0 {
		room := in.MaxReplicas - alive
		if room < 0 {
			room = 0
		}
		granted := create
		if granted > room {
			granted = room
		}
		return SizeDecision{Create: granted, Wanted: wanted, Limited: wanted > granted}
	}

	if surplus := alive - in.MaxReplicas; surplus > 0 {
		return SizeDecision{
			Surplus: surplus,
			Delete:  SelectDeletionCandidates(deletable(in), int(surplus)),
		}
	}

	return SizeDecision{}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/controller/ -run TestDecideSize -v`
Expected: PASS, every subtest.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/scaling.go internal/controller/scaling_test.go
git commit -m "$(cat <<'EOF'
feat(4a): decide a group's size from its free slots

DecideSize joins phase.Decide and SelectDeletionCandidates as a pure rule
over value types. This half covers creates and the ceiling; the scale-down
for lack of demand follows.

Its one hard part is that a server created now contributes nothing to
AggregateGroup's FreeSlots for tens of seconds, so a scaler reading that
figure would order the same replacement on every five-second resync until
maxReplicas stopped it. provisionalCapacity credits ordered-but-unarrived
capacity instead, and Slots == 0 is what separates a server still starting
from one whose agent went quiet.

Nothing calls it yet.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: `DecideSize` — the scale-down for lack of demand

**Files:**
- Modify: `internal/controller/scaling.go`
- Test: `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: `DecideSize`, `ScalingInputs`, `deletable` from Task 3; `ServerView.EmptyFor` from Task 2.
- Produces: `func readyContribution(v ServerView) int32`, `func readyFree(views []ServerView) int32`; `DecideSize` now returns at most one name in `Delete` for the demand path.

**Do not call `AggregateGroup` here.** It filters by generation, and 4a is generation-blind (spec §3): using it would freeze every scale-down the moment anyone edits the group's spec, because every edit raises the generation and the whole group's free slots would read zero.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/scaling_test.go`:

```go
// empty builds a Ready, empty server that has been empty for d.
func empty(name string, slots int32, d time.Duration) ServerView {
	v := ready(name, 0, slots)
	v.EmptyFor = d
	return v
}

func TestDecideSizeWaitsForTheStabilizationWindow(t *testing.T) {
	in := ScalingInputs{
		Views: []ServerView{
			empty("a", 100, 4*time.Minute),
			empty("b", 100, 4*time.Minute),
		},
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots:  40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	}
	if got := DecideSize(in); len(got.Delete) != 0 {
		t.Errorf("Delete = %v before the window elapsed, want none", got.Delete)
	}

	in.Views[0].EmptyFor = 5 * time.Minute
	in.Views[1].EmptyFor = 5 * time.Minute
	got := DecideSize(in)
	if len(got.Delete) != 1 {
		t.Fatalf("Delete = %v, want exactly one — one per pass", got.Delete)
	}
}

func TestDecideSizeHoldsTheFloor(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{empty("a", 100, time.Hour)},
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots:  40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v at the floor, want none", got.Delete)
	}
}

func TestDecideSizeKeepsEnoughFreeSlotsAfterTheRemoval(t *testing.T) {
	// Two empty servers, spare 150: removing either leaves 100 free, which is
	// short. Nothing may go, even though both have waited out the window.
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			empty("a", 100, time.Hour),
			empty("b", 100, time.Hour),
		},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots:  150, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: the removal would fall below spareSlots", got.Delete)
	}
}

// TestDecideSizeTestsEachCandidateOnItsOwn pins that an infeasible head of the
// candidate list does not hide a feasible tail.
//
// Both servers are empty and past the window, so both are candidates.
// SelectDeletionCandidates puts servers that never took players first, so
// "fresh" is the head. Free slots are 100 + 30 = 130: removing "fresh" leaves
// 30, short of the 40 spare, while removing "small" leaves 100. A rule that
// tested only the head would delete nothing.
func TestDecideSizeTestsEachCandidateOnItsOwn(t *testing.T) {
	fresh := empty("fresh", 100, time.Hour)
	fresh.WasRegistered = false
	small := empty("small", 30, time.Hour)

	got := DecideSize(ScalingInputs{
		Views:       []ServerView{fresh, small},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots:  40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 || got.Delete[0] != "small" {
		t.Errorf("Delete = %v, want [small]: removing fresh would leave 30 free slots, short of 40", got.Delete)
	}
}

func TestDecideSizeNeverRemovesAServerWithAnUnreliableCount(t *testing.T) {
	stale := empty("a", 100, time.Hour)
	stale.Stale = true

	got := DecideSize(ScalingInputs{
		Views:       []ServerView{stale, empty("b", 100, time.Hour)},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots:  0, MaxPlayers: 100, Stabilization: 5 * time.Minute,
	})
	if len(got.Delete) != 1 || got.Delete[0] != "b" {
		t.Fatalf("Delete = %v, want [b]: a server whose player count cannot be "+
			"trusted is never removed, and the one beside it still can be", got.Delete)
	}
}

func TestDecideSizeDoesNotShrinkWhileACreateIsOutstanding(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views: []ServerView{
			empty("a", 100, time.Hour), empty("b", 100, time.Hour),
		},
		MinReplicas: 0, MaxReplicas: 10,
		SpareSlots:  40, MaxPlayers: 100, Stabilization: 5 * time.Minute,
		PendingCreates: 1,
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v while a create is outstanding, want none", got.Delete)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/controller/ -run TestDecideSize -v`
Expected: FAIL — exactly the three that assert a removal happens (`...WaitsForTheStabilizationWindow`, `...TestsEachCandidateOnItsOwn`, `...NeverRemovesAServerWithAnUnreliableCount`), each reporting an empty `Delete`. The three that assert nothing is removed pass vacuously for now; they earn their keep once the rule exists. Every test from Task 3 must still pass — if one of those broke, the demand rule was written into the wrong branch.

- [ ] **Step 3: Implement it**

In `internal/controller/scaling.go`, add above `DecideSize`:

```go
// readyContribution is the free capacity one server actually has right now:
// arrived, unlike provisionalCapacity, because a removal must be judged against
// capacity that exists rather than capacity that is on order.
//
// It is AggregateGroup's formula without the generation filter, and not a call
// to AggregateGroup, for the reason ScalingInputs gives: filtering by generation
// would make every scale-down impossible from the moment anyone edits the
// group's spec, because the whole group would read as stale and contribute
// nothing.
func readyContribution(v ServerView) int32 {
	if v.Phase != phase.Ready || v.Stale {
		return 0
	}
	if free := v.Slots - v.Players; free > 0 {
		return free
	}
	return 0
}

// readyFree is the group's arrived free capacity, the total the feasibility
// test subtracts one candidate's share from.
func readyFree(views []ServerView) int32 {
	var free int32
	for _, v := range views {
		free += readyContribution(v)
	}
	return free
}
```

and add the import of `"github.com/spawnery/spawnery/internal/phase"`.

Replace the final `return SizeDecision{}` of `DecideSize` with:

```go
	// Demand. Never in the same pass as a create — reaching here means the
	// group is not short of capacity, but an outstanding create says capacity
	// is on its way, and removing a server against that is a decision made on
	// two different readings of the same moment.
	if in.PendingCreates == 0 && alive > in.MinReplicas {
		pool := deletable(in)
		free := readyFree(pool)

		eligible := make([]ServerView, 0, len(pool))
		for _, v := range pool {
			// EmptyFor decides nothing on its own: a server that was never
			// empty carries zero here too, and Stabilization may be zero.
			if v.Players != 0 || v.Stale || v.EmptyFor < in.Stabilization {
				continue
			}
			// Each candidate on its own, so an infeasible head of the list does
			// not hide a feasible tail.
			if free-readyContribution(v) < in.SpareSlots {
				continue
			}
			eligible = append(eligible, v)
		}
		// One per pass: every removal costs a drain cycle, and the five-second
		// resync converges quickly enough.
		if names := SelectDeletionCandidates(eligible, 1); len(names) > 0 {
			return SizeDecision{Delete: names}
		}
	}

	return SizeDecision{}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/controller/ -run TestDecideSize -v` then `nix develop -c make test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/scaling.go internal/controller/scaling_test.go
git commit -m "$(cat <<'EOF'
feat(4a): remove servers that have been empty long enough

The other half of DecideSize. A candidate has to be empty, its count fresh,
the stabilization window behind it, and its removal has to leave the group's
free slots still covering spareSlots — tested per candidate, so an
infeasible head of the list does not hide a feasible tail.

One per pass, because every removal costs a drain cycle and the five-second
resync converges. Never in the same pass as a create.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: the expectations

**Files:**
- Create: `internal/controller/expectations.go`
- Test: `internal/controller/expectations_test.go`

**Interfaces:**
- Consumes: `ServerView`, `ServerView.leaving`.
- Produces:
  - `func newExpectations(now func() time.Time) *expectations`
  - `func (e *expectations) expectCreated(group, name string)`
  - `func (e *expectations) expectDeleted(group, name string)`
  - `func (e *expectations) observe(group string, views []ServerView)`
  - `func (e *expectations) pending(group string) (int32, map[string]bool)`
  - `func (e *expectations) forget(group string)`

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/expectations_test.go` with the licence header, then:

```go
package controller

import (
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

func newTestExpectations() (*expectations, *testClock) {
	clock := &testClock{now: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	return newExpectations(clock.Now), clock
}

func TestExpectedCreateCountsUntilTheCacheShowsIt(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")

	creates, deletes := e.pending("ns/lobby")
	if creates != 1 || len(deletes) != 0 {
		t.Fatalf("pending = (%d, %v), want (1, empty)", creates, deletes)
	}

	e.observe("ns/lobby", []ServerView{{Name: "lobby-aaaa", Phase: phase.Pending}})
	if creates, _ := e.pending("ns/lobby"); creates != 0 {
		t.Errorf("creates = %d once the cache shows it, want 0", creates)
	}
}

func TestExpectationsExpire(t *testing.T) {
	e, clock := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")

	clock.Advance(expectationTTL - time.Second)
	e.observe("ns/lobby", nil)
	if creates, _ := e.pending("ns/lobby"); creates != 1 {
		t.Errorf("creates = %d before the TTL, want 1", creates)
	}

	clock.Advance(2 * time.Second)
	e.observe("ns/lobby", nil)
	if creates, _ := e.pending("ns/lobby"); creates != 0 {
		t.Errorf("creates = %d after the TTL, want 0: a lost watch event must "+
			"delay the group, not blind it", creates)
	}
}

func TestExpectedDeleteIsSatisfiedByDisappearanceOrDeparture(t *testing.T) {
	for _, tc := range []struct {
		name  string
		views []ServerView
		want  int
	}{
		{"still there, unchanged", []ServerView{{Name: "lobby-aaaa", Phase: phase.Ready}}, 1},
		{"gone from the cache", nil, 0},
		{"draining", []ServerView{{Name: "lobby-aaaa", Phase: phase.Draining}}, 0},
		{"terminating", []ServerView{{Name: "lobby-aaaa", Phase: phase.Terminating}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newTestExpectations()
			e.expectDeleted("ns/lobby", "lobby-aaaa")

			e.observe("ns/lobby", tc.views)

			_, deletes := e.pending("ns/lobby")
			if len(deletes) != tc.want {
				t.Errorf("pending deletes = %v, want %d entries", deletes, tc.want)
			}
		})
	}
}

func TestExpectationsAreKeptPerGroup(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")
	e.expectCreated("ns/arena", "arena-bbbb")

	if creates, _ := e.pending("ns/arena"); creates != 1 {
		t.Errorf("arena creates = %d, want 1", creates)
	}
	e.observe("ns/lobby", []ServerView{{Name: "lobby-aaaa"}})
	if creates, _ := e.pending("ns/arena"); creates != 1 {
		t.Errorf("arena creates = %d after observing lobby, want 1", creates)
	}
}

func TestForgetDropsAGroupEntirely(t *testing.T) {
	e, _ := newTestExpectations()
	e.expectCreated("ns/lobby", "lobby-aaaa")
	e.expectDeleted("ns/lobby", "lobby-bbbb")

	e.forget("ns/lobby")

	creates, deletes := e.pending("ns/lobby")
	if creates != 0 || len(deletes) != 0 {
		t.Errorf("pending = (%d, %v) after forget, want (0, empty)", creates, deletes)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/controller/ -run 'TestExpect|TestForget'`
Expected: FAIL to compile — `undefined: newExpectations`.

- [ ] **Step 3: Implement it**

Create `internal/controller/expectations.go` with the licence header, then:

```go
package controller

import (
	"sync"
	"time"
)

// expectationTTL bounds how long an unobserved create or delete is believed.
// Without it, a lost watch event would leave a reservation standing forever and
// the group could never size itself again; with it, the group decides on what
// the cache shows, which by then is correct.
const expectationTTL = 30 * time.Second

type expectationKind int

const (
	expectationCreate expectationKind = iota
	expectationDelete
)

type expectation struct {
	kind    expectationKind
	expires time.Time
}

// expectations reserves the creates and deletes a reconcile has issued and the
// cache has not caught up with yet.
//
// collectViews lists Servers through the manager's cached client. A reconcile
// triggered by its own create event can therefore read a cache that has not
// caught up, see the group still short, create a second server, and have the
// next pass delete the surplus again. Holding a floor hits that rarely; a
// scaler that creates servers in response to player counts hits it as a matter
// of course.
//
// This is the ReplicaSet controller's mechanism, keyed by name rather than by
// count, which makes observing one a set membership test that needs no
// ordering. It is deliberately not folded into the ServerView list:
// SelectDeletionCandidates sorts servers that never took players first and
// would nominate a placeholder immediately, and AggregateGroup and the
// PodDisruptionBudget read that same slice.
//
// Safe for concurrent use: one instance is shared by every reconcile of every
// group.
type expectations struct {
	mu      sync.Mutex
	now     func() time.Time
	byGroup map[string]map[string]expectation
}

func newExpectations(now func() time.Time) *expectations {
	return &expectations{now: now, byGroup: make(map[string]map[string]expectation)}
}

// expectCreated records a Server this reconciler has just created.
func (e *expectations) expectCreated(group, name string) {
	e.record(group, name, expectationCreate)
}

// expectDeleted records a Server whose removal this reconciler has just asked
// for.
func (e *expectations) expectDeleted(group, name string) {
	e.record(group, name, expectationDelete)
}

func (e *expectations) record(group, name string, kind expectationKind) {
	e.mu.Lock()
	defer e.mu.Unlock()

	m, ok := e.byGroup[group]
	if !ok {
		m = make(map[string]expectation)
		e.byGroup[group] = m
	}
	m[name] = expectation{kind: kind, expires: e.now().Add(expectationTTL)}
}

// observe drops every reservation the cache has caught up with, and every one
// that has waited longer than expectationTTL.
func (e *expectations) observe(group string, views []ServerView) {
	e.mu.Lock()
	defer e.mu.Unlock()

	m := e.byGroup[group]
	if len(m) == 0 {
		return
	}
	seen := make(map[string]ServerView, len(views))
	for _, v := range views {
		seen[v.Name] = v
	}

	now := e.now()
	for name, exp := range m {
		if !now.Before(exp.expires) {
			delete(m, name)
			continue
		}
		v, present := seen[name]
		switch exp.kind {
		case expectationCreate:
			if present {
				delete(m, name)
			}
		case expectationDelete:
			// Gone, or on its way out: either way the cache has caught up with
			// the removal, and the group may size itself on what it shows.
			if !present || v.leaving() {
				delete(m, name)
			}
		}
	}
	if len(m) == 0 {
		delete(e.byGroup, group)
	}
}

// pending is what the group has outstanding: how many creates, and which names
// are already on their way out.
func (e *expectations) pending(group string) (int32, map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var creates int32
	deletes := make(map[string]bool)
	for name, exp := range e.byGroup[group] {
		switch exp.kind {
		case expectationCreate:
			creates++
		case expectationDelete:
			deletes[name] = true
		}
	}
	return creates, deletes
}

// forget drops a group entirely, so the map does not grow with every group
// that ever existed.
func (e *expectations) forget(group string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.byGroup, group)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/controller/ -run 'TestExpect|TestForget' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/expectations.go internal/controller/expectations_test.go
git commit -m "$(cat <<'EOF'
feat(4a): reserve the creates and deletes the cache has not shown yet

collectViews lists Servers through the manager's cached client with no
reservation for a create the same reconcile issued. Holding a floor hits
that rarely; a scaler that creates servers in response to player counts hits
it as a matter of course.

The ReplicaSet controller's mechanism, keyed by name rather than by count.
Kept out of the ServerView list on purpose: SelectDeletionCandidates sorts
never-registered servers first and would nominate a placeholder at once, and
AggregateGroup and the PodDisruptionBudget read that same slice.

Nothing calls it yet.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: wire the decision into the controller

**Files:**
- Modify: `internal/controller/servergroup_controller.go` (`createServer`, `size`, `Reconcile`)
- Modify: `internal/controller/setup.go` (around line 76)
- Modify: `api/v1alpha1/servergroup_types.go` (`DesiredReplicas` doc comment, line 256)
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `DecideSize`, `ScalingInputs`, `SizeDecision`, `newExpectations` and its methods.
- Produces: `ServerGroupReconciler.Expectations *expectations`; `size` now returns `(SizeDecision, error)`; `createServer` now returns `(string, error)`.

**Note on what envtest can and cannot prove here:** the fixture's client is a direct client (`testenv.Client`), not a cached one, so there is no cache lag to reproduce and no envtest can make the expectations *matter*. Their behaviour is Task 5's unit tests. What the envtest below proves is the wiring: that `size` records what it issued.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/servergroup_controller_test.go`:

```go
// TestGroupCreatesTheShortfallOnceWhileTheNewServersStart is the test that
// carries this milestone.
//
// A group short of spare slots orders replacements. Those replacements are not
// Ready for tens of seconds, and a scaler reading status.freeSlots would see the
// same shortfall on every five-second pass and order the same replacement again,
// until maxReplicas stopped it. An assertion on a single decision cannot see
// that; only one that keeps reconciling can.
func TestGroupCreatesTheShortfallOnceWhileTheNewServersStart(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// minReplicas 1, maxReplicas 5, spareSlots 40, maxPlayers 100.
	f.group.Spec.Scaling.MaxReplicas = 5
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want the floor of 1", len(servers))
	}
	uid := bringUpNamed(t, f, servers[0].Name)
	if err := f.agents.ReportPlayers(uid, 70, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	// 30 free slots against 40 spare: exactly one server short.
	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != 2 {
		t.Fatalf("got %d servers after the shortfall, want 2", got)
	}

	// Ten more passes while the new server has no pod and no agent. Its
	// capacity is ordered, so nothing more may be ordered on top of it.
	//
	// The first server keeps reporting throughout, as a real agent does every
	// five seconds. Without that its count would go stale after two intervals
	// and the test would pass for the wrong reason — a stale server contributes
	// nothing either.
	for i := 0; i < 10; i++ {
		f.clock.Advance(resyncInterval)
		if err := f.agents.ReportPlayers(uid, 70, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
		f.reconcileGroup(t, r)
	}
	if got := len(f.listServers(t)); got != 2 {
		t.Fatalf("got %d servers after ten further passes, want 2 — the scaler "+
			"ordered the same replacement again while it was still starting", got)
	}
}

func TestGroupShrinksOnceTheStabilizationWindowElapses(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.setMinReplicas(t, 3)
	f.reconcileGroup(t, r)
	uids := map[string]string{}
	for _, s := range f.listServers(t) {
		uids[s.Name] = bringUpNamed(t, f, s.Name)
	}
	f.setMinReplicas(t, 1)

	// All three are empty, but none has waited out the window yet.
	f.reconcileGroup(t, r)
	if got := len(f.listServers(t)); got != 3 {
		t.Fatalf("got %d servers before the window elapsed, want 3", got)
	}

	// The window is the CRD default, 300 seconds. The agents keep reporting
	// across it, as real ones do: a count older than twice the report interval
	// is stale, and stale counts as occupied. A repeated zero must not restart
	// the emptiness window, which is what makes this assertion meaningful.
	f.clock.Advance(301 * time.Second)
	for _, uid := range uids {
		if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
			t.Fatalf("ReportPlayers: %v", err)
		}
	}
	f.reconcileGroup(t, r)

	var leaving int
	for _, s := range f.listServers(t) {
		if !s.DeletionTimestamp.IsZero() {
			leaving++
		}
	}
	if leaving != 1 {
		t.Fatalf("%d servers marked for deletion, want exactly one per pass", leaving)
	}
}

func TestGroupRecordsWhatItIssued(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.reconcileGroup(t, r)

	creates, _ := r.Expectations.pending(f.ns + "/lobby")
	if creates != 1 {
		t.Errorf("pending creates = %d right after the create, want 1: size() "+
			"did not record what it issued", creates)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix develop -c go test ./internal/controller/ -run 'TestGroupCreatesTheShortfall|TestGroupShrinksOnce|TestGroupRecordsWhatItIssued' -v`
Expected: FAIL — `r.Expectations undefined`, and the first two fail on the counts because `size` still asks only for the floor.

- [ ] **Step 3: Implement it**

In `internal/controller/servergroup_controller.go`, add the field to `ServerGroupReconciler`, after `Clock`:

```go
	// Expectations reserves the creates and deletes this reconciler has issued
	// and the cache has not shown yet. One instance is shared across groups.
	Expectations *expectations
```

Change `createServer` to return the name it created — its last two statements become:

```go
	r.Recorder.Eventf(group, corev1.EventTypeNormal, "ServerCreated", "created server %s", srv.Name)
	return srv.Name, nil
```

and its signature and two earlier `return err` statements become `return "", err`:

```go
func (r *ServerGroupReconciler) createServer(ctx context.Context, group *spawneryv1alpha1.ServerGroup) (string, error) {
```

Replace the body of `size` entirely:

```go
// size brings the group to the size DecideSize asks for and reports that
// decision, so Reconcile can publish the part of it that belongs on the status.
func (r *ServerGroupReconciler) size(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
	servers map[string]*spawneryv1alpha1.Server,
) (SizeDecision, error) {
	logger := log.FromContext(ctx)
	if group.Spec.Scaling == nil {
		return SizeDecision{}, nil
	}
	key := group.Namespace + "/" + group.Name

	r.Expectations.observe(key, views)
	pendingCreates, pendingDeletes := r.Expectations.pending(key)

	decision := DecideSize(ScalingInputs{
		Views:         views,
		MinReplicas:   group.Spec.Scaling.MinReplicas,
		MaxReplicas:   group.Spec.Scaling.MaxReplicas,
		SpareSlots:    group.Spec.Scaling.SpareSlots,
		MaxPlayers:    group.Spec.MaxPlayers,
		Stabilization: time.Duration(group.Spec.Scaling.ScaleDownStabilizationSeconds) * time.Second,

		PendingCreates: pendingCreates,
		PendingDeletes: pendingDeletes,
	})

	for i := int32(0); i < decision.Create; i++ {
		name, err := r.createServer(ctx, group)
		if err != nil {
			return decision, err
		}
		r.Expectations.expectCreated(key, name)
	}
	if int32(len(decision.Delete)) < decision.Surplus {
		logger.Info("fewer free servers than the surplus, trying again later",
			"group", group.Name, "surplus", decision.Surplus, "free", len(decision.Delete))
	}
	for _, name := range decision.Delete {
		if err := r.deleteServer(ctx, group, servers, name,
			"ServerRemoved", "removing server %s"); err != nil {
			return decision, err
		}
		r.Expectations.expectDeleted(key, name)
	}
	return decision, nil
}
```

In `Reconcile`, change the deletion path to release the group's reservations:

```go
	if !group.DeletionTimestamp.IsZero() {
		// The Server objects are owned by the group; Kubernetes garbage
		// collection cascades, and each Server drains through its finalizer.
		r.Expectations.forget(group.Namespace + "/" + group.Name)
		return ctrl.Result{}, nil
	}
```

and adapt the sizing call to the new signature — the decision it now returns is read in Task 7, so discard it here:

```go
	if networkUsable && group.IsEphemeral() {
		if _, err := r.size(ctx, group, views, servers); err != nil {
			return ctrl.Result{}, err
		}
	}
```

In `internal/controller/setup.go`, construct it:

```go
	if err := (&ServerGroupReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Recorder:     mgr.GetEventRecorderFor("servergroup"),
		Agents:       opts.Agents,
		Clock:        opts.Clock,
		Expectations: newExpectations(opts.Clock),
	}).SetupWithManager(mgr); err != nil {
```

In `internal/controller/servergroup_controller_test.go`, give the test reconciler one too:

```go
func groupReconciler(f *fixture) *ServerGroupReconciler {
	return &ServerGroupReconciler{
		Client:       f.c,
		Scheme:       f.reconc.Scheme,
		Recorder:     record.NewFakeRecorder(100),
		Agents:       f.agents,
		Clock:        f.clock.Now,
		Expectations: newExpectations(f.clock.Now),
	}
}
```

In `api/v1alpha1/servergroup_types.go`, replace the `DesiredReplicas` doc comment:

```go
// DesiredReplicas is the number of servers the group must have at minimum. For
// an ephemeral group it is the floor only: the size it actually runs at is
// DecideSize's, which reads this as one input among several.
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c make test`
Expected: PASS, the whole suite. Existing group tests that relied on the group sitting exactly at its floor may now see one more server, because a group with `spareSlots: 40` and no ready capacity is short by one. If a pre-existing test fails, read it before changing it: the new count is usually correct and the assertion needs the new number with a comment saying why, but a test that fails because an *occupied* server was nominated is a real defect and must be fixed in `scaling.go`, never in the test.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/servergroup_controller.go internal/controller/setup.go internal/controller/servergroup_controller_test.go api/v1alpha1/servergroup_types.go
git commit -m "$(cat <<'EOF'
feat(4a): size ephemeral groups by their free slots

size() becomes DecideSize's executor and records every create and delete it
issues, so the next pass does not order it again.

The test that carries the milestone reconciles ten more times after a
shortfall is covered and asserts the count has not moved: an assertion on a
single decision cannot see a scaler that re-orders the same replacement on
every pass, and that is precisely the failure the provisional capacity
exists to prevent.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: the `ScalingLimited` condition

**Files:**
- Modify: `api/v1alpha1/common_types.go`
- Modify: `internal/controller/servergroup_controller.go` (`Reconcile`)
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `SizeDecision.Limited`, `SizeDecision.Wanted` from Task 6.
- Produces: `spawneryv1alpha1.ConditionScalingLimited`, `ReasonMaxReplicasReached`, `ReasonWithinLimits`.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/servergroup_controller_test.go`:

```go
// TestGroupSaysWhenItsCeilingHoldsCapacityBack closes a gap this repository
// already has once: a proxy that cannot bind its ready port says so only in a
// container log. A group that cannot serve its spareSlots because maxReplicas
// stops it is the same kind of silence, and this is the milestone that would
// otherwise add a second one next to the first.
func TestGroupSaysWhenItsCeilingHoldsCapacityBack(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.group.Spec.Scaling.MaxReplicas = 1
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	uid := bringUpNamed(t, f, servers[0].Name)
	if err := f.agents.ReportPlayers(uid, 100, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcileGroup(t, r)

	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionScalingLimited)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ScalingLimited = %+v, want True with the group full at its ceiling", cond)
	}
	if cond.Reason != spawneryv1alpha1.ReasonMaxReplicasReached {
		t.Errorf("reason = %q, want %q", cond.Reason, spawneryv1alpha1.ReasonMaxReplicasReached)
	}
	if !strings.Contains(cond.Message, "maxReplicas") {
		t.Errorf("message = %q, want it to name the limit that is holding", cond.Message)
	}
	if group.Status.Phase == "" {
		t.Error("phase went empty")
	}
	if meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Error("a group working exactly as configured must not be Degraded")
	}

	// Room again: the condition has to come back down.
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.MaxReplicas = 5
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}
	f.reconcileGroup(t, r)

	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby", Namespace: f.ns}, group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	if meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionScalingLimited) {
		t.Error("ScalingLimited still True after maxReplicas was raised")
	}
}
```

`meta` is `k8s.io/apimachinery/pkg/api/meta`; add the import if the file does not have it.

- [ ] **Step 2: Run the test to verify it fails**

Run: `nix develop -c go test ./internal/controller/ -run TestGroupSaysWhenItsCeiling -v`
Expected: FAIL to compile — `undefined: spawneryv1alpha1.ConditionScalingLimited`.

- [ ] **Step 3: Implement it**

In `api/v1alpha1/common_types.go`, add to the condition types:

```go
	// ConditionScalingLimited reports that a group would create more servers
	// to cover its spareSlots and maxReplicas is stopping it. Deliberately not
	// Degraded: a popular group sitting on its ceiling works exactly as
	// configured, and folding the two together would move the group's phase and
	// make a real fault during peak load indistinguishable from peak load.
	ConditionScalingLimited = "ScalingLimited"
```

and to the reasons:

```go
	ReasonMaxReplicasReached   = "MaxReplicasReached"
	ReasonWithinLimits         = "WithinLimits"
```

In `internal/controller/servergroup_controller.go`, capture the decision Task 6 discards:

```go
	var decision SizeDecision
	if networkUsable && group.IsEphemeral() {
		var err error
		if decision, err = r.size(ctx, group, views, servers); err != nil {
			return ctrl.Result{}, err
		}
	}
```

and publish it, after the sizing block and before the aggregated status:

```go
	if group.IsEphemeral() {
		limited := metav1.Condition{
			Type:    spawneryv1alpha1.ConditionScalingLimited,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonWithinLimits,
			Message: "free slots cover spec.scaling.spareSlots",
		}
		if decision.Limited {
			limited.Status = metav1.ConditionTrue
			limited.Reason = spawneryv1alpha1.ReasonMaxReplicasReached
			limited.Message = fmt.Sprintf(
				"%d more server(s) needed to cover spareSlots %d, but maxReplicas %d is reached",
				decision.Wanted, group.Spec.Scaling.SpareSlots, group.Spec.Scaling.MaxReplicas)
		}
		// The event goes on the flank only. SetStatusCondition moves
		// lastTransitionTime just on a change of status, so comparing across
		// the call is what tells a transition from a resync.
		was := meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionScalingLimited)
		meta.SetStatusCondition(&group.Status.Conditions, limited)
		if decision.Limited != was {
			eventType := corev1.EventTypeNormal
			if decision.Limited {
				eventType = corev1.EventTypeWarning
			}
			r.Recorder.Event(group, eventType, limited.Reason, limited.Message)
		}
	}
```

The guard is `group.IsEphemeral()` rather than the sizing branch's condition: a group whose Network is missing is not limited by `maxReplicas`, and its `Accepted` condition already says what is wrong with it. `decision` is the zero value on that path, so the condition publishes False, which is true.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c make test` and `nix develop -c make manifests && git diff --exit-code config/`
Expected: PASS, and no manifest diff — conditions are a list, so this adds no schema.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/common_types.go internal/controller/servergroup_controller.go internal/controller/servergroup_controller_test.go
git commit -m "$(cat <<'EOF'
docs(4a): say on the group when its ceiling holds capacity back

A group that would create more servers to cover spareSlots and cannot is
invisible today. This repository already has one silence of that kind — a
proxy that cannot bind its ready port says so only in a container log — and
a scaling milestone is the wrong place to add a second one beside it.

Its own condition, not Degraded: a popular group on its ceiling works
exactly as configured, and folding the two together would move the group's
phase through derivePhase and make a real fault during peak load
indistinguishable from peak load. No schema change; conditions are a list.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: documentation

**Files:**
- Modify: `README.md` (the milestone paragraphs, around line 82–130)
- Modify: `docs/known-issues.md` (the milestone 4 preconditions, around line 770–825)
- Modify: `docs/handover-milestone-4.md`

**Interfaces:**
- Consumes: everything Tasks 1–7 landed.
- Produces: nothing code depends on.

- [ ] **Step 1: Update the README**

After the milestone 3c paragraphs and before the "Anyone starting milestone 4" paragraph, insert:

```markdown
Milestone 4a is done: slot-based scaling. An ephemeral `ServerGroup` no longer
sits at its floor. It creates servers as soon as its free player slots fall
below `spec.scaling.spareSlots`, bounded by `maxReplicas`, and removes them
again — one per pass, and only while the group's free slots would still cover
the spare — once a server has been empty for `scaleDownStabilizationSeconds`.
The rule is `DecideSize` in `internal/controller/scaling.go`, a pure function
beside `phase.Decide` and `SelectDeletionCandidates`, and the invariant those
already carried holds unchanged: a server that may be carrying players is
never nominated.

The one thing worth naming is what the scale-up rule reads. A server created
now is not `Ready` for tens of seconds and adds nothing to `status.freeSlots`,
so a scaler reading that figure would see the same shortfall on every
five-second pass and order the same replacement again, until `maxReplicas`
stopped it. It reads a second figure instead, one that credits capacity that
has been ordered and has not arrived. The two are deliberately not the same
number, and the envtest that carries this milestone is the one that keeps
reconciling for ten more passes and asserts the count has not moved — a single
decision cannot show that failure.

Milestone 4 continues with 4b, rolling updates of ephemeral groups, and 4c,
proxy and node drain, which owns the readiness
`internal/agent/registry.go` still cannot lower.
```

- [ ] **Step 2: Close and rewrite the known-issues entries**

In `docs/known-issues.md`, under "Preconditions for milestone 4 (scaling and drain)":

- **"Nothing bounds the reported `slots` against the group's `maxPlayers`"** — closed by Task 2. Move it out of the preconditions into a "Closed by milestone 4a" note naming `clampReport` in `internal/controller/candidates.go`, or delete it and record the closure in the handover. Do not leave it standing as open.
- **"`ProxyGroupReconciler.pods()` has no expectations tracking"** — half closed. Rewrite it: the `ServerGroup` side is closed by `internal/controller/expectations.go`; the `ProxyGroup` side is untouched and now belongs to 4c, which is the milestone that makes proxy replica counts move. Say that the mechanism to copy already exists, so 4c does not have to design it again.
- The remaining preconditions — the PDB's missing counterpart, terminating pods counting as "process gone", exponential backoff per group, orphaned `Server`s without a pod — stay, and each gains the sub-milestone that owns it: backoff → 4b, the rest → 4c.

Add one new entry under a "From milestone 4a" heading:

> **`status.freeSlots` and the scaler's own figure are two numbers.** `AggregateGroup` computes free slots over `Ready` servers of the current generation; `provisionalCapacity` in `internal/controller/scaling.go` computes a second figure that also credits servers whose capacity is ordered and has not arrived. Anyone reading the code for the first time will want to unify them. They must not be: the first is what `status.freeSlots` documents and what 4b's rolling update needs; the second is what stops the scaler ordering the same replacement on every five-second pass. Both files say so; this entry is the third place, so a search finds it.

- [ ] **Step 3: Record 4a in the milestone 4 handover**

In `docs/handover-milestone-4.md`, add a section near the top — after "Where we are" — saying that 4a has landed, what it built, and what 4b and 4c now find in place:

- `DecideSize` in `internal/controller/scaling.go` is the sizing rule; 4b's rolling update adds the stale-generation rules to it rather than a second scaler beside it.
- `expectations` exists and is the mechanism 4c needs for `ProxyGroupReconciler`.
- `agent.Snapshot.EmptyFor` exists.
- The `ScalingLimited` condition is the pattern 4c can reuse for the proxy's own gaps.
- Everything under "The one contract change milestone 4 has to make" is untouched and still 4c's.

- [ ] **Step 4: Verify**

Run: `nix develop -c make test`
Expected: PASS. Then re-read every claim written in this task against the tree — the specs of this repository assert measured facts, and a handover that names a function that does not exist is worse than no handover.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/known-issues.md docs/handover-milestone-4.md
git commit -m "$(cat <<'EOF'
docs(4a): record what slot-based scaling landed and what it leaves

Two milestone 4 preconditions close here: the unbounded reported slots, and
the missing create reservation on the ServerGroup side. The ProxyGroup side
of the second stays open and now names 4c as its owner, with a pointer to
the mechanism it can copy.

One new entry: status.freeSlots and the scaler's provisional figure are two
numbers on purpose, and the code says so in two places already. This is the
third, so a search finds it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Definition of done

Every acceptance criterion of the spec, mapped to what proves it:

| # | Criterion | Proof |
|---|---|---|
| 1 | `make test` green | Task 8, step 4 |
| 2 | A group short of `spareSlots` creates, bounded by `maxReplicas` | `TestDecideSizeRoundsTheShortfallUp`, `TestDecideSizeReportsTheCeilingHoldingCapacityBack` |
| 3 | The shortfall is ordered once, not once per reconcile | `TestGroupCreatesTheShortfallOnceWhileTheNewServersStart` |
| 4 | Scale-down waits out the window, one per pass, only while `freeSlots` stays covered | `TestDecideSizeWaitsForTheStabilizationWindow`, `TestDecideSizeKeepsEnoughFreeSlotsAfterTheRemoval`, `TestGroupShrinksOnceTheStabilizationWindowElapses` |
| 5 | A server that may carry players is never nominated | `TestDecideSizeShrinksToALoweredCeilingWithoutWaiting`, `TestDecideSizeNeverRemovesAServerWithAnUnreliableCount`, and the pre-existing `TestOccupiedServerSurvivesAContinuousScaleDown` |
| 6 | A lowered `maxReplicas` shrinks without waiting | `TestDecideSizeShrinksToALoweredCeilingWithoutWaiting` |
| 7 | An over-reported capacity cannot influence the group | `TestClampReport` |
| 8 | `ScalingLimited` is true exactly while the ceiling holds, and the phase does not move | `TestGroupSaysWhenItsCeilingHoldsCapacityBack` |
| 9 | A create the cache has not shown is not ordered twice | `TestExpectedCreateCountsUntilTheCacheShowsIt`, `TestDecideSizeCreditsCapacityThatIsOrderedButNotArrived`, `TestGroupRecordsWhatItIssued` |
