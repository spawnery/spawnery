# Rollout Floor and WhenEmpty Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An ephemeral `ServerGroup` can keep a floor of joinable servers while it changes over (`spec.update.minAvailable`), and can replace only stale servers that are empty (`spec.update.strategy: WhenEmpty`) without holding a network changeover place while it waits for occupied ones.

**Architecture:** Both are decisions inside the pure sizing core (`internal/controller/scaling.go`: `selectRetirement`, `decideSize`) and the changeover state (`internal/controller/changeover.go`). The ServerGroup reconciler only passes the two new spec values in and reports the new outcomes (`Progressing` reason `WaitingForMinAvailable`, `ScalingLimited` when the ceiling blocks the extra server, `status.changeover: Deferred`).

**Tech Stack:** Go, controller-runtime, kubebuilder markers + CEL, envtest.

**Spec:** `docs/superpowers/specs/2026-09-25-rollout-floor-and-when-empty-design.md`

## Global Constraints

- Unset `strategy` and unset `minAvailable` are today's behaviour, unchanged; no existing object rolls on upgrade (the pod hash does not read `spec.update`).
- `minAvailable`: optional, minimum 1, and less than `spec.scaling.maxReplicas` (CEL).
- `strategy`: enum `RollingUpdate`, `WhenEmpty`, default `RollingUpdate`.
- `strategy: WhenEmpty` with `maxStaleSeconds > 0` is refused (CEL).
- Joinable: phase `Ready`, `Registered`, `!JoinsClosed`, not retiring (`spec.retire` or reserved), not reserved for deletion, not condemned. Any generation.
- The floor applies only while stale servers remain (`staleRemains`).
- Known empty: `Players == 0 && !Stale`.
- One extra server at a time: no create pending, no current-generation server `Pending`/`Starting`, `alive < maxReplicas`.
- Generated files are committed: after API changes run `make manifests generate` and commit `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `zz_generated.deepcopy.go`, `docs/reference/crds.md`.
- Commits: Conventional Commits with scope, body wrapped at 72, signed, ending with the two trailers the session names.
- Copyright header on new files: `Copyright paul_wtf.` (copy the Apache header block from any existing file).
- Nothing about any particular network or consumer goes into this public repo; examples are invented ("a round-based game").

## Commands

All commands run in the dev shell with the flake path as an argument, never after a `cd`:

```bash
NIX="nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c"
# one package, one pattern
$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestName' -count=1
# generated files
$NIX make -C /home/paul/git/spawnery manifests generate
# the whole suite on the development VM (8 cores, 12 GB): envtest packages one at a time
$NIX make -C /home/paul/git/spawnery manifests generate fmt vet chart-lint toolchain-lint image-tag-lint docs-length-lint crd-docs-test chart-values-docs-test metrics-docs-test
$NIX go -C /home/paul/git/spawnery test -race -p 1 ./...
```

On `paul-desktop` drop `-p 1` and run `make test` as is.

## Review Focus

1. A retirement reserved this pass but not yet visible in the cache must not count as joinable, or the floor admits a second retirement on a count that is one too high. Pinned in Task 2 (`TestDecideSizeDoesNotCountAReservedRetirementAsJoinable`).
2. A stale server whose agent stream is down reports `Players == 0` with `Stale`; under `WhenEmpty` it must read as occupied and stay. Pinned in Task 3 (`TestWhenEmptyLeavesAServerWithAnUntrustedCount`).
3. The floor must not build a second extra server while the first is still starting or pending, or a slow start fans out to `maxReplicas`. Pinned in Task 2 (`TestDecideSizeBuildsOnlyOneExtraServerAtATime`).
4. The demand rule sheds empty stale servers during a changeover; it must not walk past the floor by deleting a joinable one. Pinned in Task 2 (`TestDecideSizeHoldsTheFloorAgainstScaleDownDuringAChangeover`).
5. A `Deferred` group must be invisible to `AdmitChangeovers` (neither holder nor waiter); today any state other than `""`/`Begun` is queued as waiting. Pinned in Task 4 (`TestAdmitChangeovers` case "a deferred group neither holds nor waits").

## Departure from the spec

§6 asks for an e2e run on kind. `make e2e` resolves no image by design (`hack/e2e.sh` header), so no server there ever becomes Ready or reports a player, and neither behaviour can be observed. Task 5's envtest tests drive the real reconcilers with Ready servers and reported player counts instead. The check on a live network follows the release.

---

### Task 1: API fields, validation, accessors

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (UpdateSpec at ~line 70, the XValidation list at ~line 111, the status `Changeover` enum at ~line 433, accessors after `UpdateMaxStale` at ~line 562)
- Modify: `api/v1alpha1/common_types.go` (ChangeoverState consts at ~line 313, reasons at ~line 205)
- Test: `api/v1alpha1/servergroup_types_test.go`, `api/v1alpha1/servergroup_envtest_test.go`
- Generated: `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`, `docs/reference/crds.md`

**Interfaces:**
- Produces:
  - `type UpdateStrategy string`; consts `UpdateRollingUpdate UpdateStrategy = "RollingUpdate"`, `UpdateWhenEmpty UpdateStrategy = "WhenEmpty"`
  - `UpdateSpec.Strategy UpdateStrategy`, `UpdateSpec.MinAvailable *int32`
  - `func (g *ServerGroup) UpdateWhenEmpty() bool`
  - `func (g *ServerGroup) UpdateMinAvailable() int32` (0 when unset)
  - `ChangeoverDeferred ChangeoverState = "Deferred"`
  - `ReasonWaitingForMinAvailable = "WaitingForMinAvailable"`

- [ ] **Step 1: Write the failing accessor test**

Append to `api/v1alpha1/servergroup_types_test.go`:

```go
func TestUpdateStrategyAndFloorAccessors(t *testing.T) {
	cases := []struct {
		name      string
		update    *UpdateSpec
		whenEmpty bool
		floor     int32
	}{
		{"no update policy", nil, false, 0},
		{"rolling update without a floor", &UpdateSpec{Strategy: UpdateRollingUpdate}, false, 0},
		{"when empty", &UpdateSpec{Strategy: UpdateWhenEmpty}, true, 0},
		{"a floor", &UpdateSpec{MinAvailable: ptr.To[int32](3)}, false, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &ServerGroup{Spec: ServerGroupSpec{Update: tc.update}}
			if got := g.UpdateWhenEmpty(); got != tc.whenEmpty {
				t.Errorf("UpdateWhenEmpty() = %v, want %v", got, tc.whenEmpty)
			}
			if got := g.UpdateMinAvailable(); got != tc.floor {
				t.Errorf("UpdateMinAvailable() = %d, want %d", got, tc.floor)
			}
		})
	}
}
```

Add `"k8s.io/utils/ptr"` to the file's imports if it is not there.

- [ ] **Step 2: Run it and watch it fail**

Run: `$NIX go -C /home/paul/git/spawnery test ./api/v1alpha1/ -run TestUpdateStrategyAndFloorAccessors -count=1`
Expected: FAIL to compile: `undefined: UpdateRollingUpdate`, `UpdateWhenEmpty`, `MinAvailable`.

- [ ] **Step 3: Add the types, fields, constants and accessors**

In `api/v1alpha1/servergroup_types.go`, above `UpdateSpec`:

```go
// UpdateStrategy is how a changeover replaces stale servers.
// +kubebuilder:validation:Enum=RollingUpdate;WhenEmpty
type UpdateStrategy string

const (
	// UpdateRollingUpdate retires stale servers whether or not they have
	// players; the players stay until they leave.
	UpdateRollingUpdate UpdateStrategy = "RollingUpdate"
	// UpdateWhenEmpty retires only stale servers known to be empty. An
	// occupied stale server stays Ready and joinable until it empties.
	UpdateWhenEmpty UpdateStrategy = "WhenEmpty"
)
```

`UpdateSpec` becomes (existing fields unchanged, new ones added, CEL on the type):

```go
// UpdateSpec controls the rolling update of ephemeral groups.
// +kubebuilder:validation:XValidation:rule="!has(self.strategy) || self.strategy != 'WhenEmpty' || !has(self.maxStaleSeconds) || self.maxStaleSeconds == 0",message="spec.update.maxStaleSeconds must be 0 with strategy WhenEmpty: it drains the occupied servers WhenEmpty leaves alone"
type UpdateSpec struct {
	// Strategy is how stale servers are replaced.
	// +kubebuilder:default=RollingUpdate
	// +optional
	Strategy UpdateStrategy `json:"strategy,omitempty"`

	// MaxUnavailable ... (unchanged)
	MaxUnavailable int32 `json:"maxUnavailable,omitempty"`

	// MaxStaleSeconds ... (unchanged)
	MaxStaleSeconds int32 `json:"maxStaleSeconds,omitempty"`

	// MinAvailable is how many servers must stay joinable while the group
	// changes over: Ready, registered, door open, and not on their way out,
	// of either generation. The group builds one extra server at a time to
	// keep it. Unset keeps no floor beyond one Ready server of the current
	// generation.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinAvailable *int32 `json:"minAvailable,omitempty"`
}
```

Keep the existing comments and markers of `MaxUnavailable` and `MaxStaleSeconds` exactly as they are.

Add to the `ServerGroupSpec` XValidation list (after the `minReplicas <= maxReplicas` rule):

```go
// +kubebuilder:validation:XValidation:rule="!has(self.update) || !has(self.update.minAvailable) || !has(self.scaling) || self.update.minAvailable < self.scaling.maxReplicas",message="spec.update.minAvailable must be less than spec.scaling.maxReplicas: keeping the floor needs room for one extra server"
```

Change the status enum marker on `ServerGroupStatus.Changeover` to:

```go
	// +kubebuilder:validation:Enum="";Waiting;Begun;Deferred
```

After `UpdateMaxStale`:

```go
// UpdateWhenEmpty reports whether spec.update.strategy is WhenEmpty.
func (g *ServerGroup) UpdateWhenEmpty() bool {
	return g.Spec.Update != nil && g.Spec.Update.Strategy == UpdateWhenEmpty
}

// UpdateMinAvailable is spec.update.minAvailable, 0 when unset.
func (g *ServerGroup) UpdateMinAvailable() int32 {
	if g.Spec.Update == nil || g.Spec.Update.MinAvailable == nil {
		return 0
	}
	return *g.Spec.Update.MinAvailable
}
```

In `api/v1alpha1/common_types.go`, add to the `ChangeoverState` consts:

```go
	// ChangeoverDeferred means a WhenEmpty server group has a Ready server of
	// the current generation and its remaining stale servers wait for their
	// players: it holds no place in the network's budget.
	ChangeoverDeferred ChangeoverState = "Deferred"
```

and next to `ReasonWaitingForChangeoverBudget`:

```go
	// ReasonWaitingForMinAvailable: the next stale server would leave fewer
	// than spec.update.minAvailable joinable servers, and the extra server
	// that would keep the floor is not being built.
	ReasonWaitingForMinAvailable = "WaitingForMinAvailable"
```

- [ ] **Step 4: Regenerate and run the accessor test**

Run: `$NIX make -C /home/paul/git/spawnery manifests generate`
Then: `$NIX go -C /home/paul/git/spawnery test ./api/v1alpha1/ -run TestUpdateStrategyAndFloorAccessors -count=1`
Expected: PASS. `git -C /home/paul/git/spawnery status --short` shows the CRD, chart template, deepcopy and `docs/reference/crds.md` changed.

- [ ] **Step 5: Write the failing CEL tests**

Append three cases to the `cases` slice of `TestServerGroupCELRejections` in `api/v1alpha1/servergroup_envtest_test.go`:

```go
		{
			name: "minAvailable equal to maxReplicas",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Update = &spawneryv1alpha1.UpdateSpec{MinAvailable: ptr.To[int32](10)}
			},
		},
		{
			name: "minAvailable zero",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Update = &spawneryv1alpha1.UpdateSpec{MinAvailable: ptr.To[int32](0)}
			},
		},
		{
			name: "WhenEmpty with maxStaleSeconds",
			base: ephemeralGroup,
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Update = &spawneryv1alpha1.UpdateSpec{
					Strategy: spawneryv1alpha1.UpdateWhenEmpty, MaxStaleSeconds: 60,
				}
			},
		},
```

And a new test for what must be accepted:

```go
func TestServerGroupAcceptsAFloorAndWhenEmpty(t *testing.T) {
	c, ctx := testenv.Client(t)
	for _, tc := range []struct {
		name   string
		update *spawneryv1alpha1.UpdateSpec
	}{
		{"a floor one below the ceiling", &spawneryv1alpha1.UpdateSpec{MinAvailable: ptr.To[int32](9)}},
		{"WhenEmpty", &spawneryv1alpha1.UpdateSpec{Strategy: spawneryv1alpha1.UpdateWhenEmpty}},
		{"WhenEmpty with a floor", &spawneryv1alpha1.UpdateSpec{
			Strategy: spawneryv1alpha1.UpdateWhenEmpty, MinAvailable: ptr.To[int32](2),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := testenv.Namespace(t, ctx, c)
			g := ephemeralGroup(ns, "group")
			g.Spec.Update = tc.update
			if err := c.Create(ctx, g); err != nil {
				t.Fatalf("create: %v", err)
			}
			if g.Spec.Update.Strategy == "" {
				t.Errorf("strategy was not defaulted")
			}
		})
	}
}
```

(`c.Create` writes the defaulted object back into `g`, so the last check reads the server's default.)

- [ ] **Step 6: Run the CEL tests**

Run: `$NIX go -C /home/paul/git/spawnery test ./api/v1alpha1/ -run 'TestServerGroupCELRejections|TestServerGroupAcceptsAFloorAndWhenEmpty' -count=1`
Expected: PASS. Then prove the two new rules bite: in a throwaway worktree (`git worktree add --detach`), delete the minAvailable XValidation line, run `make manifests` and the same test: FAIL on "minAvailable equal to maxReplicas". Remove the worktree afterwards.

- [ ] **Step 7: Commit**

```bash
git -C /home/paul/git/spawnery add api config charts docs/reference/crds.md
git -C /home/paul/git/spawnery commit -m "feat(api): spec.update.minAvailable and strategy WhenEmpty" -m "<body: the two fields, the CEL rules and why, status.changeover Deferred, reason WaitingForMinAvailable; nothing acts on them yet>"
```

---

### Task 2: The floor in the sizing rule

**Files:**
- Modify: `internal/controller/scaling.go` (`ScalingInputs` ~line 37, `SizeDecision` ~line 106, `selectRetirement` ~line 408, `decideSize` ~line 577)
- Test: `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 (the pure core takes plain values).
- Produces:
  - `ScalingInputs.MinAvailable int32`, `ScalingInputs.WhenEmpty bool` (the second is used in Task 3)
  - `SizeDecision.FloorHeld bool`, `SizeDecision.Joinable int32`, `SizeDecision.FloorBlocked bool`
  - `func joinable(in ScalingInputs, v ServerView) bool`, `func joinableCount(in ScalingInputs) int32`, `func currentStarting(in ScalingInputs) bool`
  - `func selectRetirement(in ScalingInputs) (string, bool)`: the bool is "declined only because of the floor"

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/scaling_test.go`:

```go
// floorInputs is a changeover with room: two stale servers carrying players,
// one Ready replacement, budget for two retirements.
func floorInputs(minAvailable int32, views ...ServerView) ScalingInputs {
	return ScalingInputs{
		Views:   views,
		PodHash: "current", MaxUnavailable: 2, MinAvailable: minAvailable,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
		Stabilization: 5 * time.Minute,
	}
}

func TestDecideSizeKeepsTheFloorOfJoinableServers(t *testing.T) {
	got := DecideSize(floorInputs(3,
		staleReady("old1", 10, 100, "old"),
		staleReady("old2", 10, 100, "old"),
		ready("new", 0, 100),
	))
	if len(got.Retire) != 0 {
		t.Fatalf("Retire = %v, want none: three joinable, floor three", got.Retire)
	}
	if got.Create != 1 || !got.FloorHeld || got.Joinable != 3 {
		t.Errorf("Create = %d FloorHeld = %v Joinable = %d, want one extra server for the floor of 3 joinable",
			got.Create, got.FloorHeld, got.Joinable)
	}
}

func TestDecideSizeRetiresAboveTheFloor(t *testing.T) {
	got := DecideSize(floorInputs(2,
		staleReady("old1", 10, 100, "old"),
		staleReady("old2", 10, 100, "old"),
		ready("new", 0, 100),
	))
	if len(got.Retire) != 1 || got.Retire[0] != "old1" || got.FloorHeld {
		t.Errorf("Retire = %v FloorHeld = %v, want [old1]: two stay joinable", got.Retire, got.FloorHeld)
	}
}

func TestDecideSizeRetiresAClosedDoorWithoutTouchingTheFloor(t *testing.T) {
	closed := staleReady("zzz", 10, 100, "old")
	closed.JoinsClosed = true
	got := DecideSize(floorInputs(2,
		staleReady("old2", 10, 100, "old"),
		closed,
		ready("new", 0, 100),
	))
	if len(got.Retire) != 1 || got.Retire[0] != "zzz" {
		t.Errorf("Retire = %v, want [zzz]: it is not joinable, so retiring it keeps the two that are", got.Retire)
	}
}

func TestDecideSizeBuildsOnlyOneExtraServerAtATime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   []ServerView
		pending int32
	}{
		{"the extra server is starting", []ServerView{starting("surge")}, 0},
		{"the extra server is pending", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := floorInputs(3, append([]ServerView{
				staleReady("old1", 10, 100, "old"),
				staleReady("old2", 10, 100, "old"),
				ready("new", 0, 100),
			}, tc.extra...)...)
			in.PendingCreates = tc.pending
			got := DecideSize(in)
			if got.Create != 0 || len(got.Retire) != 0 || !got.FloorHeld {
				t.Errorf("Create = %d Retire = %v FloorHeld = %v, want nothing new while the extra server comes up",
					got.Create, got.Retire, got.FloorHeld)
			}
		})
	}
}

func TestDecideSizeReportsTheFloorBlockedAtTheCeiling(t *testing.T) {
	busy := ready("busy", 20, 100)
	busy.JoinsClosed = true
	in := floorInputs(3,
		staleReady("old1", 10, 100, "old"),
		staleReady("old2", 10, 100, "old"),
		ready("new", 0, 100),
		busy,
	)
	in.MaxReplicas = 4
	got := DecideSize(in)
	if got.Create != 0 || len(got.Retire) != 0 {
		t.Fatalf("Create = %d Retire = %v, want nothing: at the ceiling with the floor reached", got.Create, got.Retire)
	}
	if !got.FloorHeld || !got.FloorBlocked || !got.Limited {
		t.Errorf("FloorHeld = %v FloorBlocked = %v Limited = %v, want all true", got.FloorHeld, got.FloorBlocked, got.Limited)
	}
}

func TestDecideSizeHoldsTheFloorAgainstScaleDownDuringAChangeover(t *testing.T) {
	retiring := staleReady("old1", 40, 100, "old")
	retiring.Phase = phase.Retiring
	retiring.Retire = true
	idle := staleReady("old2", 0, 100, "old")
	idle.EmptyFor = time.Hour
	in := floorInputs(2, retiring, idle, ready("new", 60, 100))
	in.MaxUnavailable = 1
	got := DecideSize(in)
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: old2 and new are the two joinable servers the floor keeps", got.Delete)
	}
}

func TestDecideSizeIgnoresTheFloorOutsideAChangeover(t *testing.T) {
	a := ready("a", 0, 100)
	a.EmptyFor = time.Hour
	b := ready("b", 0, 100)
	b.EmptyFor = time.Hour
	got := DecideSize(floorInputs(5, a, b))
	if len(got.Delete) != 1 {
		t.Errorf("Delete = %v, want one: nothing is stale, so the floor does not apply", got.Delete)
	}
}

func TestDecideSizeDoesNotCountAReservedRetirementAsJoinable(t *testing.T) {
	in := floorInputs(2,
		staleReady("old1", 10, 100, "old"),
		staleReady("old2", 10, 100, "old"),
		ready("new", 0, 100),
	)
	in.PendingRetires = map[string]bool{"old1": true}
	got := DecideSize(in)
	if len(got.Retire) != 0 || got.Create != 1 {
		t.Errorf("Retire = %v Create = %d, want no retirement and one extra server: "+
			"old1 is already going, so old2 and new are the only joinable two", got.Retire, got.Create)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestDecideSize(KeepsTheFloor|RetiresAboveTheFloor|RetiresAClosedDoor|BuildsOnlyOneExtra|ReportsTheFloorBlocked|HoldsTheFloorAgainst|IgnoresTheFloor|DoesNotCountAReserved)' -count=1`
Expected: FAIL to compile (`unknown field MinAvailable`, `got.FloorHeld undefined`). After adding only the struct fields (Step 3's first block) the compile passes and these fail on their assertions: KeepsTheFloor, RetiresAClosedDoor, ReportsTheFloorBlocked, HoldsTheFloorAgainst, DoesNotCountAReserved. RetiresAboveTheFloor, BuildsOnlyOneExtra's "Create" half, and IgnoresTheFloor are guards that may already pass; note which in the ledger.

- [ ] **Step 3: Implement**

`ScalingInputs`, after `PendingRetires`:

```go
	// MinAvailable is spec.update.minAvailable: how many servers stay
	// joinable while stale ones remain. 0 is no floor.
	MinAvailable int32
	// WhenEmpty is spec.update.strategy WhenEmpty: only stale servers known
	// to be empty are retired.
	WhenEmpty bool
```

`SizeDecision`, after `ChangeoverWaiting`:

```go
	// FloorHeld is true when a changeover retirement was declined only
	// because it would leave fewer than MinAvailable joinable servers.
	FloorHeld bool
	// Joinable is the count FloorHeld was judged on.
	Joinable int32
	// FloorBlocked is true when FloorHeld and the extra server the floor
	// needs cannot be built because the group is at maxReplicas.
	FloorBlocked bool
```

New helpers next to `staleRemains`:

```go
// joinable reports whether a player can be sent to this server now: Ready,
// in the proxies' tables, door open, and not already on its way out by any
// route. Either generation.
func joinable(in ScalingInputs, v ServerView) bool {
	return v.Phase == phase.Ready && v.Registered && !v.JoinsClosed &&
		!v.Retire && !in.PendingRetires[v.Name] && !in.PendingDeletes[v.Name] && !v.Condemned
}

func joinableCount(in ScalingInputs) int32 {
	var n int32
	for _, v := range in.Views {
		if joinable(in, v) {
			n++
		}
	}
	return n
}

// currentStarting reports whether a server of the current generation is on
// its way up, which is the extra server the floor already asked for.
func currentStarting(in ScalingInputs) bool {
	for _, v := range in.Views {
		if in.PendingDeletes[v.Name] || staleSpec(v, in.PodHash) {
			continue
		}
		if v.Phase == phase.Pending || v.Phase == phase.Starting {
			return true
		}
	}
	return false
}
```

`selectRetirement` returns `(string, bool)`. Its early return becomes `return "", false`. Replace the final `return stale[0].Name` with:

```go
	if in.MinAvailable < 1 {
		return stale[0].Name, false
	}
	open := joinableCount(in)
	for _, v := range stale {
		if !joinable(in, v) || open-1 >= in.MinAvailable {
			return v.Name, false
		}
	}
	return "", true
```

Add one sentence to its doc comment: the floor picks the first candidate in that order whose retirement keeps `MinAvailable` joinable servers, and the bool reports a decline that only the floor caused.

In `decideSize`, replace the retirement block with:

```go
	name, floorHeld := selectRetirement(in)
	if name != "" {
		return SizeDecision{Retire: []string{name}, ChangeoverWaiting: waiting}
	}
	var floorOpen int32
	var floorBlocked bool
	if floorHeld {
		floorOpen = joinableCount(in)
		if in.PendingCreates == 0 && !currentStarting(in) {
			if alive < in.MaxReplicas {
				return SizeDecision{Create: 1, FloorHeld: true, Joinable: floorOpen, ChangeoverWaiting: waiting}
			}
			floorBlocked = true
		}
	}
```

In the demand block, before the `eligible` loop: `open := joinableCount(in)`. Inside the loop, after the changeover filter:

```go
			if changeover && in.MinAvailable > 0 && joinable(in, v) && open-1 < in.MinAvailable {
				continue
			}
```

Carry the floor fields out of both later returns:

```go
			return SizeDecision{Delete: names, ChangeoverWaiting: waiting,
				FloorHeld: floorHeld, Joinable: floorOpen, FloorBlocked: floorBlocked}
```

```go
	return SizeDecision{Wanted: wanted, Limited: limited || floorBlocked, ColdStartBlocked: coldBlocked,
		ChangeoverWaiting: waiting, FloorHeld: floorHeld, Joinable: floorOpen, FloorBlocked: floorBlocked}
```

- [ ] **Step 4: Run the new tests and the whole sizing suite**

Run: `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestDecideSize|TestSelect' -count=1`
Expected: PASS, including every pre-existing `TestDecideSize*`.

- [ ] **Step 5: Mutant check**

In a throwaway worktree, drop `!in.PendingRetires[v.Name] &&` from `joinable`: `TestDecideSizeDoesNotCountAReservedRetirementAsJoinable` must FAIL. Drop `!currentStarting(in)`: `TestDecideSizeBuildsOnlyOneExtraServerAtATime/the_extra_server_is_starting` must FAIL. Remove the worktree.

- [ ] **Step 6: Commit**

```bash
git -C /home/paul/git/spawnery add internal/controller/scaling.go internal/controller/scaling_test.go
git -C /home/paul/git/spawnery commit -m "feat(controller): a floor of joinable servers during a changeover" -m "<body>"
```

---

### Task 3: WhenEmpty in the retirement choice

**Files:**
- Modify: `internal/controller/scaling.go` (`selectRetirement`'s stale collection)
- Test: `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: `ScalingInputs.WhenEmpty`, `selectRetirement(in) (string, bool)` from Task 2.
- Produces: nothing new.

- [ ] **Step 1: Write the failing tests**

```go
func whenEmptyInputs(views ...ServerView) ScalingInputs {
	in := floorInputs(0, views...)
	in.WhenEmpty = true
	return in
}

func TestWhenEmptyLeavesAnOccupiedServer(t *testing.T) {
	got := DecideSize(whenEmptyInputs(staleReady("old", 10, 100, "old"), ready("new", 0, 100)))
	if len(got.Retire) != 0 || got.FloorHeld || got.Create != 0 {
		t.Errorf("Retire = %v FloorHeld = %v Create = %d, want nothing: the round on old goes on",
			got.Retire, got.FloorHeld, got.Create)
	}
}

func TestWhenEmptyRetiresAnEmptyServer(t *testing.T) {
	got := DecideSize(whenEmptyInputs(staleReady("old", 0, 100, "old"), ready("new", 0, 100)))
	if len(got.Retire) != 1 || got.Retire[0] != "old" {
		t.Errorf("Retire = %v, want [old]", got.Retire)
	}
}

func TestWhenEmptyLeavesAServerWithAnUntrustedCount(t *testing.T) {
	quiet := staleReady("old", 0, 100, "old")
	quiet.Stale = true
	got := DecideSize(whenEmptyInputs(quiet, ready("new", 0, 100)))
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none: a count nobody can trust reads as occupied", got.Retire)
	}
}

func TestWhenEmptyRetiresTheEmptyServerBesideAnOccupiedOne(t *testing.T) {
	got := DecideSize(whenEmptyInputs(
		staleReady("a", 5, 100, "old"),
		staleReady("b", 0, 100, "old"),
		ready("new", 0, 100),
	))
	if len(got.Retire) != 1 || got.Retire[0] != "b" {
		t.Errorf("Retire = %v, want [b]", got.Retire)
	}
}

func TestWhenEmptyKeepsTheFloor(t *testing.T) {
	in := whenEmptyInputs(staleReady("old", 0, 100, "old"), ready("new", 0, 100))
	in.MinAvailable = 2
	got := DecideSize(in)
	if len(got.Retire) != 0 || got.Create != 1 {
		t.Errorf("Retire = %v Create = %d, want an extra server before the empty old one goes", got.Retire, got.Create)
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestWhenEmpty' -count=1`
Expected: FAIL: `TestWhenEmptyLeavesAnOccupiedServer` (Retire = [old]), `TestWhenEmptyLeavesAServerWithAnUntrustedCount` (Retire = [old]). The other three are guards.

- [ ] **Step 3: Implement**

In `selectRetirement`, where a stale server is appended to `stale`:

```go
		if v.Phase == phase.Ready && !in.PendingDeletes[v.Name] && !v.Condemned {
			if in.WhenEmpty && (v.Players != 0 || v.Stale) {
				continue
			}
			stale = append(stale, v)
		}
```

Add a sentence to the doc comment: under WhenEmpty only stale servers known to be empty are candidates.

- [ ] **Step 4: Run**

Run: `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestWhenEmpty|TestDecideSize' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add internal/controller/scaling.go internal/controller/scaling_test.go
git -C /home/paul/git/spawnery commit -m "feat(controller): WhenEmpty retires only empty stale servers" -m "<body>"
```

---

### Task 4: Deferred changeovers hold no place

**Files:**
- Modify: `internal/controller/changeover.go` (`AdmitChangeovers` ~line 47, `ownServerChangeover` ~line 145)
- Test: `internal/controller/changeover_test.go`

**Interfaces:**
- Consumes: `spawneryv1alpha1.ChangeoverDeferred` (Task 1).
- Produces: `func ownServerChangeover(views []ServerView, podHash string, pendingCreates int32, whenEmpty bool) spawneryv1alpha1.ChangeoverState`

- [ ] **Step 1: Write the failing tests**

Add a case to the `cases` table of `TestAdmitChangeovers`:

```go
		{
			"a deferred group neither holds nor waits",
			[]ChangeoverView{
				{Kind: "ServerGroup", Name: "arena", State: spawneryv1alpha1.ChangeoverDeferred},
				{Kind: "ServerGroup", Name: "lobby", State: spawneryv1alpha1.ChangeoverWaiting},
			},
			1,
			map[string]bool{"ServerGroup/lobby": true},
		},
```

New test:

```go
func TestOwnServerChangeover(t *testing.T) {
	old := ServerView{Name: "old", PodHash: "old", Phase: phase.Ready}
	current := ServerView{Name: "new", PodHash: "current", Phase: phase.Ready}
	startingCurrent := ServerView{Name: "new", PodHash: "current", Phase: phase.Starting}
	for _, tc := range []struct {
		name      string
		views     []ServerView
		pending   int32
		whenEmpty bool
		want      spawneryv1alpha1.ChangeoverState
	}{
		{"nothing stale", []ServerView{current}, 0, true, spawneryv1alpha1.ChangeoverNone},
		{"stale only, nothing asked for", []ServerView{old}, 0, true, spawneryv1alpha1.ChangeoverWaiting},
		{"WhenEmpty with its first server starting", []ServerView{old, startingCurrent}, 0, true, spawneryv1alpha1.ChangeoverBegun},
		{"WhenEmpty with a Ready current server", []ServerView{old, current}, 0, true, spawneryv1alpha1.ChangeoverDeferred},
		{"RollingUpdate with a Ready current server", []ServerView{old, current}, 0, false, spawneryv1alpha1.ChangeoverBegun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownServerChangeover(tc.views, "current", tc.pending, tc.whenEmpty); got != tc.want {
				t.Errorf("ownServerChangeover = %q, want %q", got, tc.want)
			}
		})
	}
}
```

Add `"github.com/spawnery/spawnery/internal/phase"` to the test file's imports if missing.

- [ ] **Step 2: Run and watch them fail**

Run: `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestAdmitChangeovers|TestOwnServerChangeover' -count=1`
Expected: FAIL to compile (`too many arguments in call to ownServerChangeover`); after Step 3's signature change alone, the Deferred case of `TestOwnServerChangeover` and the new `TestAdmitChangeovers` case fail (`arena` admitted as a waiter).

- [ ] **Step 3: Implement**

`AdmitChangeovers`, first check in the loop:

```go
		if g.State == spawneryv1alpha1.ChangeoverNone || g.State == spawneryv1alpha1.ChangeoverDeferred || g.Failing {
			continue
		}
```

`ownServerChangeover`:

```go
func ownServerChangeover(views []ServerView, podHash string, pendingCreates int32, whenEmpty bool) spawneryv1alpha1.ChangeoverState {
	var stale, current, readyCurrent bool
	for _, v := range views {
		if staleSpec(v, podHash) {
			if !phase.Terminal(v.Phase) {
				stale = true
			}
		} else if v.countsTowardSize() {
			current = true
			if v.Phase == phase.Ready {
				readyCurrent = true
			}
		}
	}
	switch {
	case !stale:
		return spawneryv1alpha1.ChangeoverNone
	case whenEmpty && readyCurrent:
		return spawneryv1alpha1.ChangeoverDeferred
	case current || pendingCreates > 0:
		return spawneryv1alpha1.ChangeoverBegun
	default:
		return spawneryv1alpha1.ChangeoverWaiting
	}
}
```

Extend its doc comment by one sentence: a WhenEmpty group with a Ready current server is Deferred, because what remains waits for players and not for the budget.

Update the one caller in `servergroup_controller.go` (~line 922):

```go
			own := ownServerChangeover(views, podHash, int32(len(pendingCreates)), group.UpdateWhenEmpty())
```

- [ ] **Step 4: Run**

Run: `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestAdmitChangeovers|TestOwnServerChangeover|TestOwnProxyChangeover' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git -C /home/paul/git/spawnery add internal/controller/changeover.go internal/controller/changeover_test.go internal/controller/servergroup_controller.go
git -C /home/paul/git/spawnery commit -m "feat(controller): a WhenEmpty group waiting on its players holds no changeover place" -m "<body>"
```

---

### Task 5: Reconciler wiring, conditions, and the envtest proof

**Files:**
- Modify: `internal/controller/servergroup_controller.go` (`DecideSize` call ~line 927, ScalingLimited message ~line 548, `reportProgressing` ~line 1270 and its caller ~line 793)
- Modify: `internal/controller/servergroup_controller_test.go` (three existing `reportProgressing(` calls at ~lines 4526, 4846, 4877 gain a `FloorReport{}` argument)
- Create: `internal/controller/rolloutfloor_envtest_test.go`

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces: `type FloorReport struct{ Joinable, Min int32 }`; `func reportProgressing(group *spawneryv1alpha1.ServerGroup, views []ServerView, podHash string, waitingFor []string, floor FloorReport)`

- [ ] **Step 1: Write the failing unit test for the condition**

Append to `internal/controller/servergroup_controller_test.go`:

```go
func TestProgressingNamesTheFloor(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{ObjectMeta: metav1.ObjectMeta{Name: "lobby", Generation: 2}}
	views := []ServerView{
		{Name: "lobby-new", PodHash: "current", Phase: phase.Ready},
		{Name: "lobby-old", PodHash: "old", Phase: phase.Ready},
	}
	reportProgressing(group, views, "current", nil, FloorReport{Joinable: 2, Min: 2})
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil || cond.Reason != spawneryv1alpha1.ReasonWaitingForMinAvailable {
		t.Fatalf("Progressing = %+v, want reason %s", cond, spawneryv1alpha1.ReasonWaitingForMinAvailable)
	}
	if !strings.Contains(cond.Message, "minAvailable 2") {
		t.Errorf("message %q does not name the floor", cond.Message)
	}

	group.Status.Conditions = nil
	views = append(views, ServerView{Name: "lobby-surge", PodHash: "current", Phase: phase.Starting})
	reportProgressing(group, views, "current", nil, FloorReport{Joinable: 2, Min: 2})
	cond = meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil || cond.Reason != spawneryv1alpha1.ReasonServersStarting {
		t.Errorf("Progressing = %+v, want %s while the extra server starts", cond, spawneryv1alpha1.ReasonServersStarting)
	}
}
```

Add `FloorReport{}` as the last argument to the three existing `reportProgressing(` calls in that file.

- [ ] **Step 2: Run and watch it fail**

Run: `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestProgressingNamesTheFloor|TestAFailedRetireeIsNamedOnProgressing' -count=1`
Expected: FAIL to compile (`undefined: FloorReport`); after adding the type and parameter only, `TestProgressingNamesTheFloor` fails on the reason (`ReplacingServers`).

- [ ] **Step 3: Implement the condition and the wiring**

Next to `reportProgressing`:

```go
// FloorReport is a changeover held at spec.update.minAvailable; the zero value
// is none.
type FloorReport struct {
	Joinable, Min int32
}
```

`reportProgressing` takes `floor FloorReport` as its last parameter. Add a case after `case len(stuck) > 0:` and before `case starting > 0:`:

```go
	case floor.Min > 0 && starting == 0:
		condition.Status = metav1.ConditionTrue
		condition.Reason = spawneryv1alpha1.ReasonWaitingForMinAvailable
		condition.Message = fmt.Sprintf(
			"%d server(s) joinable, minAvailable %d: the next stale server waits for an extra server "+
				"that is not being built (see ScalingLimited and BackingOff)", floor.Joinable, floor.Min)
```

At the caller (~line 793):

```go
	var floor FloorReport
	if decision.FloorHeld {
		floor = FloorReport{Joinable: decision.Joinable, Min: group.UpdateMinAvailable()}
	}
	reportProgressing(group, views, podHash, waitingFor, floor)
```

In the `DecideSize` call (~line 927), after `MaxUnavailable`:

```go
				MinAvailable:   group.UpdateMinAvailable(),
				WhenEmpty:      group.UpdateWhenEmpty(),
```

In the ScalingLimited block, extend the `if decision.ColdStartBlocked { ... } else { ... }` to:

```go
			switch {
			case decision.ColdStartBlocked:
				// existing message, unchanged
			case decision.FloorBlocked:
				limited.Message = fmt.Sprintf(
					"the changeover keeps minAvailable %d joinable and needs one extra server; the group is at maxReplicas %d",
					group.UpdateMinAvailable(), group.Spec.Scaling.MaxReplicas)
			default:
				// existing shortfall message, unchanged
			}
```

- [ ] **Step 4: Run the unit tests**

Run: `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'Progressing|Retiree' -count=1`
Expected: PASS.

- [ ] **Step 5: Write the envtest proof**

Create `internal/controller/rolloutfloor_envtest_test.go` (Apache header with `Copyright paul_wtf.` as in every other file, `package controller`):

```go
import (
	"testing"

	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
)

// setUpdatePolicy gives the fixture's group a floor of servers and an update
// policy before its first reconcile.
func (f *fixture) setUpdatePolicy(t *testing.T, minReplicas int32, update *spawneryv1alpha1.UpdateSpec) {
	t.Helper()
	g := f.serverGroup(t, f.group.Name)
	g.Spec.Scaling.MinReplicas = minReplicas
	g.Spec.Update = update
	if err := f.c.Update(f.ctx, g); err != nil {
		t.Fatalf("update group: %v", err)
	}
}

func (f *fixture) reportPlayersOn(t *testing.T, name string, players int32) {
	t.Helper()
	pod, ok := f.pod(name)
	if !ok {
		t.Fatalf("no pod for %s", name)
	}
	if err := f.agents.ReportPlayers(string(pod.UID), players, 100); err != nil {
		t.Fatalf("report %d players on %s: %v", players, name, err)
	}
}

// bringUpCurrent readies every server of the group's current generation that
// is not Ready yet.
func (f *fixture) bringUpCurrent(t *testing.T, group string) {
	t.Helper()
	g := f.serverGroup(t, group)
	for _, name := range f.serverNamesOfGroup(t, group) {
		srv := f.server(name)
		if srv.Spec.GroupGeneration == g.Generation && srv.Status.Phase != string(phase.Ready) {
			bringUpNamed(t, f, name)
		}
	}
}

func TestWhenEmptyKeepsAnOccupiedServerThroughAChangeover(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setUpdatePolicy(t, 2, &spawneryv1alpha1.UpdateSpec{
		Strategy: spawneryv1alpha1.UpdateWhenEmpty, MaxUnavailable: 2,
	})
	f.reconcileNamedGroup(t, r, "lobby")
	f.readyAllServersOf(t, "lobby")
	names := f.serverNamesOfGroup(t, "lobby")
	if len(names) != 2 {
		t.Fatalf("servers = %v, want 2", names)
	}
	occupied, empty := names[0], names[1]
	f.reportPlayersOn(t, occupied, 5)

	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, r, "lobby")
	f.bringUpCurrent(t, "lobby")
	for i := 0; i < 3; i++ {
		f.reconcileNamedGroup(t, r, "lobby")
	}

	if !f.server(empty).Spec.Retire {
		t.Errorf("%s is empty and stale, want it retired", empty)
	}
	if srv := f.server(occupied); srv.Spec.Retire || srv.Status.Phase != string(phase.Ready) {
		t.Fatalf("%s retire = %v phase = %s, want it left Ready while it has players",
			occupied, srv.Spec.Retire, srv.Status.Phase)
	}
	if got := f.serverGroup(t, "lobby").Status.Changeover; got != spawneryv1alpha1.ChangeoverDeferred {
		t.Errorf("status.changeover = %q, want Deferred", got)
	}

	f.reportPlayersOn(t, occupied, 0)
	f.reconcileNamedGroup(t, r, "lobby")
	if !f.server(occupied).Spec.Retire {
		t.Errorf("%s is empty now, want it retired", occupied)
	}
}

func TestMinAvailableBuildsAnExtraServerBeforeRetiring(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setUpdatePolicy(t, 2, &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 2, MinAvailable: ptr.To[int32](2)})
	f.reconcileNamedGroup(t, r, "lobby")
	f.readyAllServersOf(t, "lobby")
	stale := f.serverNamesOfGroup(t, "lobby")

	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, r, "lobby") // cold start
	f.bringUpCurrent(t, "lobby")
	f.reconcileNamedGroup(t, r, "lobby") // three joinable: the first stale one may go

	retired := 0
	for _, name := range stale {
		if f.server(name).Spec.Retire {
			retired++
		}
	}
	if retired != 1 {
		t.Fatalf("%d stale servers retired, want 1", retired)
	}

	f.reconcileNamedGroup(t, r, "lobby") // two joinable: the floor asks for an extra server
	if n := len(f.serverNamesOfGroup(t, "lobby")); n != 4 {
		t.Fatalf("group has %d servers, want 4: two stale, the cold start, and the extra server", n)
	}
	if c := f.progressing(t, "lobby"); c.Reason != spawneryv1alpha1.ReasonServersStarting {
		t.Errorf("Progressing = %s, want %s while the extra server starts", c.Reason, spawneryv1alpha1.ReasonServersStarting)
	}

	f.bringUpCurrent(t, "lobby")
	f.reconcileNamedGroup(t, r, "lobby")
	for _, name := range stale {
		if !f.server(name).Spec.Retire {
			t.Errorf("%s still not retired once the extra server is Ready", name)
		}
	}
}
```

If a pass declines because a retire or create reservation has not been observed yet (the group's expectations), reconcile once more at that point and record it in the ledger as a ruling; do not weaken an assertion.

- [ ] **Step 6: Run the envtest proof, then prove it bites**

Run: `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestWhenEmptyKeepsAnOccupiedServerThroughAChangeover|TestMinAvailableBuildsAnExtraServerBeforeRetiring' -count=1`
Expected: PASS. Then in a throwaway worktree, remove `WhenEmpty: group.UpdateWhenEmpty(),` from the `DecideSize` call: the first test must FAIL on "want it left Ready". Remove `MinAvailable: group.UpdateMinAvailable(),`: the second must FAIL on "want 4". Remove the worktree.

- [ ] **Step 7: Commit**

```bash
git -C /home/paul/git/spawnery add internal/controller
git -C /home/paul/git/spawnery commit -m "feat(controller): the reconciler keeps the floor and WhenEmpty, and says when it waits" -m "<body>"
```

---

### Task 6: The guide

**Files:**
- Modify: `docs/guides/updates-and-drain.md` (new sections after "Bounding the wait", before "Changing over a whole network")

- [ ] **Step 1: Write the two sections**

```markdown
## Keeping servers joinable during a roll

A roll takes servers out of the proxies' tables one at a time and builds
replacements only as far as the group's free slots need them. A group whose
players choose a server, rather than being placed on any, can end up with a
single open server for a while. `minAvailable` says how many must stay open:

```yaml
spec:
  update:
    minAvailable: 2
```

While stale servers remain, the next one is retired only if at least that
many servers stay joinable afterwards: Ready, registered, door open, and not
on their way out, old or new alike. If retiring it would leave fewer, the
group first builds one extra server, waits for it to be Ready, and then
retires. The extra servers are removed by the ordinary scale-down once the
roll is over. It must be below `maxReplicas`, because keeping the floor needs
room for one more.

A roll held at the floor that cannot build its extra server says so on
`Progressing` (`WaitingForMinAvailable`), and on `ScalingLimited` if the
ceiling is the reason.

## Rolling only what is empty

Some servers are not worth rolling while they are in use: a round-based game
whose server gathers a lobby, closes its door for the round and ends with it.
`Retiring` would stop the lobby filling, and `maxStaleSeconds` would move the
players of a running round. `WhenEmpty` leaves them alone:

```yaml
spec:
  update:
    strategy: WhenEmpty
```

Only stale servers that are known to be empty are retired, and those go at
once. An occupied stale server stays `Ready` and joinable for as long as it
has players; once it is empty it is replaced like any other. A server whose
player count cannot be trusted counts as occupied. `maxStaleSeconds` cannot be
combined with it, and a node drain still moves these servers, because the
node is leaving either way.

Such a group takes a network changeover place only for its first new server.
Once that is Ready, `status.changeover` reads `Deferred`: the rest waits for
players, not for the budget, and other groups are not held behind it.
```

- [ ] **Step 2: Check the docs build and the length lint**

Run: `$NIX make -C /home/paul/git/spawnery docs-length-lint`
Expected: PASS. If `make docs` is available on the machine, run it too (`--strict` catches broken links).

- [ ] **Step 3: Commit**

```bash
git -C /home/paul/git/spawnery add docs/guides/updates-and-drain.md
git -C /home/paul/git/spawnery commit -m "docs(guides): a floor for rolls, and rolling only what is empty" -m "<body>"
```

---

### Task 7: Whole suite

- [ ] **Step 1: Run everything `make test` runs, envtest packages one at a time on the VM**

Run the two `Commands` lines for the whole suite. Expected: all green, and `git status` clean after `make manifests generate` (generated files already committed). `internal/podspec`'s hash golden must pass unchanged: nothing in this plan feeds the pod hash.

- [ ] **Step 2: Record the result in the ledger.**

Release (0.7.0: operator and chart move, images do not) and the check on a live network are not part of this plan; they follow the review, with Paul's go.
