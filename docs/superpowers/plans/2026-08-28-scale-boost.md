# Milestone 7c-1 — `ScaleBoost`

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A group can be given extra capacity for a while, by an object that expires on its own — without anything writing the spec a person declared.

**Architecture:** A namespaced CRD owned by the group it names. The scaler adds live boosts to the group's floor and to nothing else; the ceiling still binds. Expired ones are swept. No command exists yet: `kubectl create` is the whole of the interface this milestone ships, which is what makes it testable on its own.

**Tech Stack:** Go, controller-runtime, envtest. A CRD, a chart change, and one new RBAC grant.

**Spec:** [`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../specs/2026-08-27-cloud-api-design.md) §3.2, §4.4, §9.2

## Global Constraints

- Every build and test command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`
- Commit messages are Conventional Commits with English subjects, and every
  commit ends with exactly these two trailers:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **Never push, never merge, and never create a tag.**
- **Never run `git config` in any form.**
- `make manifests` regenerates the CRDs **and** the chart templates; commit
  both with the change that caused them.
- **A new RBAC marker is not enough.** `internal/rbacaudit/required.go` holds a
  hand-maintained table compared against the generated ClusterRole in *both*
  directions, so a grant added to one and not the other turns `make test` red.
  That is the check working; add both.
- envtest shares one control plane with no cleanup. Scope every List to the
  namespace under test.

## Why this exists at all

Read §3.2 before starting. The short of it: the operator's ClusterRole grants
`get, list, watch` on `servergroups` and no write to the spec, and the
`ServerGroup` on this project's own cluster is Flux-managed — so a
`minReplicas` the operator wrote would be reverted at the next reconciliation.
An annotation needs the same grant. A separate object is the only shape where
the operator writes nothing a person declared.

**Do not "simplify" this into a spec edit.** It would pass every test in this
repository and fail on the one cluster the project runs.

## File structure

| Path | Responsibility |
|---|---|
| `api/v1alpha1/scaleboost_types.go` | The kind, its spec, and its printer columns |
| `internal/controller/boosts.go` | Reading live boosts for a group, and the expiry rule |
| `internal/controller/scaling.go` | `ScalingInputs.Boost`, added to the floor |
| `internal/controller/servergroup_controller.go` | Collecting boosts; publishing what they did |
| `internal/controller/orphan.go` | Sweeping expired boosts |
| `internal/rbacaudit/required.go` | The grant, in the table as well as the marker |
| `docs/upgrading.md` | What a boost is, and that a chart upgrade brings a CRD |

---

### Task 1: The kind

**Files:**
- Create: `api/v1alpha1/scaleboost_types.go`
- Regenerate: `make manifests`, `make generate`

**Interfaces:**
- Produces:
  ```go
  type ScaleBoostSpec struct {
      GroupRef  ObjectRef    `json:"groupRef"`
      Replicas  int32        `json:"replicas"`
      ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
  }
  ```
  and `ScaleBoost`, `ScaleBoostList`.

- [ ] **Step 1: Write the type**

Follow `api/v1alpha1/network_types.go` for the file's shape — licence header,
kubebuilder markers, doc comments that carry the reasoning rather than
restating the field name.

The comments that have to be there, because each is a decision somebody will
otherwise undo:

```go
// ScaleBoost is extra capacity for a group, for a while.
//
// It exists as its own object rather than as a field on the group, and the
// reason is not tidiness. The operator has no write access to a ServerGroup's
// spec -- its ClusterRole grants get, list and watch -- and on a
// GitOps-managed cluster that spec is owned by a file, so a floor the operator
// raised there would be reverted at the next reconciliation. An admin would
// type a command, watch the count rise, and find it back where the file has
// it. This object is the operator's own, and nothing outside the cluster
// claims it.
//
// It adds to the group's floor and never to its ceiling. spec.scaling.
// maxReplicas still binds, because a ceiling is an instruction -- milestone 4a
// established that -- and a boost is the one thing here that a person might
// create in a hurry.
```

```go
	// Replicas is how many servers to add to the group's own floor.
	//
	// Added, not set: two boosts on one group are two boosts, and a second
	// does not replace a first. That is what makes "somebody else already
	// boosted this" a non-event rather than a race.
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas"`

	// ExpiresAt is when this boost stops counting.
	//
	// Optional in the type and supplied by everything that creates one. A
	// boost with none never expires, which is a real need -- a maintenance
	// window somebody is watching -- and a real hazard: the boost from last
	// weekend still running in March, with nobody left who remembers why the
	// lobby has four servers. Whatever creates these gives them a default, and
	// the type does not, because a type that invented a time would make an
	// explicit "forever" impossible to write.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
```

Printer columns: the group, the replicas, the expiry, and the age. An admin
looking at `kubectl get scaleboosts` wants to know what is inflating a group
and when it stops.

- [ ] **Step 2: Regenerate**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make generate`

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make manifests`

Expected: a new CRD under `config/crd/bases/`, the same CRD in
`charts/spawnery/templates/crds.yaml`, and `zz_generated.deepcopy.go` grown.

- [ ] **Step 3: Confirm it round-trips through a real API server**

Add to `api/v1alpha1/` an envtest beside the ones already there — grep for
`_envtest_test.go` in that directory and follow the closest.

Modelled on `TestNetworkRoundTrip` in `network_envtest_test.go:36`, which is
the closest and uses the same `testenv.Client` / `testenv.Namespace` pair.

```go
// The CRD is installable and the fields survive a write and a read. Cheap, and
// it catches a marker that does not mean what it looks like -- a validation
// that rejects a legal value, or an optional field the API server drops.
func TestScaleBoostRoundTrip(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	expires := metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second))

	boost := &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-boost", Namespace: ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: "lobby"},
			Replicas:  2,
			ExpiresAt: &expires,
		},
	}
	if err := c.Create(ctx, boost); err != nil {
		t.Fatalf("create: %v", err)
	}

	got := &spawneryv1alpha1.ScaleBoost{}
	if err := c.Get(ctx, types.NamespacedName{Name: "lobby-boost", Namespace: ns}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.GroupRef.Name != "lobby" || got.Spec.Replicas != 2 {
		t.Errorf("spec = %+v, want lobby and 2", got.Spec)
	}
	if got.Spec.ExpiresAt == nil || !got.Spec.ExpiresAt.Time.Equal(expires.Time) {
		t.Errorf("expiresAt = %v, want %v", got.Spec.ExpiresAt, expires)
	}
}

func TestAScaleBoostWithoutAnExpiryIsAccepted(t *testing.T) {
	// The "forever" case the type deliberately allows. If this fails, the
	// +optional marker is not doing what the comment says it does.
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	err := c.Create(ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "forever", Namespace: ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
			Replicas: 1,
		},
	})
	if err != nil {
		t.Fatalf("a boost with no expiry was refused: %v", err)
	}
}

func TestAScaleBoostOfZeroReplicasIsRefused(t *testing.T) {
	// Minimum=1. A boost of zero is not a boost, and accepting one would put
	// an object in the cluster that inflates nothing and explains nothing.
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	err := c.Create(ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
			Replicas: 0,
		},
	})
	if err == nil {
		t.Error("a boost of zero replicas was accepted")
	}
}
```

- [ ] **Step 4: Commit**

```bash
git add api config charts
git commit -m "$(cat <<'EOF'
feat(api): ScaleBoost, extra capacity that is not a spec edit

Its own object rather than a field on the group, because the operator has no
write access to a ServerGroup's spec and on a GitOps-managed cluster that
spec is owned by a file: a floor the operator raised there would be reverted
at the next reconciliation, and an admin would watch a count rise and fall
back for no visible reason.

Replicas adds to the group's floor and never to its ceiling. maxReplicas
still binds, because a ceiling is an instruction -- which 4a established --
and a boost is the one thing here somebody might create in a hurry.

Two boosts on one group are two boosts. A second does not replace a first,
which makes "somebody already boosted this" a non-event rather than a race.

ExpiresAt is optional in the type and supplied by everything that creates
one. A type that invented a default would make an explicit "forever"
impossible to write, and forever is a real need even though it is also the
known failure mode.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 2: The grant, in both places

**Files:**
- Modify: `internal/controller/servergroup_controller.go` (the kubebuilder marker)
- Modify: `internal/rbacaudit/required.go`
- Regenerate: `make manifests`

- [ ] **Step 1: Add the marker**

Beside the ServerGroup markers on `ServerGroupReconciler`:

```go
// +kubebuilder:rbac:groups=spawnery.cloud,resources=scaleboosts,verbs=get;list;watch;delete
```

**No create and no update.** This milestone reads boosts and sweeps expired
ones; the thing that makes them is a command that does not exist yet, and
7c-2 adds `create` when it adds the caller. A grant with no caller is a grant
nobody can justify later.

- [ ] **Step 2: Add the same rows to the table**

`internal/rbacaudit/required.go`, beside the `servergroups` block:

```go
	{Group: "spawnery.cloud", Resource: "scaleboosts", Verb: "get", Why: "resolving a boost's group"},
	{Group: "spawnery.cloud", Resource: "scaleboosts", Verb: "list", Why: "ServerGroupReconciler adds live boosts to the floor"},
	{Group: "spawnery.cloud", Resource: "scaleboosts", Verb: "watch", Why: "a boost created or expiring has to wake its group"},
	{Group: "spawnery.cloud", Resource: "scaleboosts", Verb: "delete", Why: "the sweep removes expired boosts"},
```

- [ ] **Step 3: Regenerate and run the audit**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make manifests`

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/rbacaudit/`

Expected: PASS.

- [ ] **Step 4: Prove the audit is doing the work**

Delete one row from the table, leave the marker, and run the audit. Confirm it
fails naming that verb. **Restore it.** Then delete the marker, leave the
table, and confirm it fails the other way.

Both directions, because the table exists to catch drift in either, and a
check that only fires one way is half a check.

- [ ] **Step 5: Commit**

```bash
git add internal config charts
git commit -m "$(cat <<'EOF'
feat(rbac): the operator may read and sweep boosts, and nothing more

get, list, watch and delete. No create and no update: this milestone reads
boosts and removes expired ones, and the thing that makes them is a command
that does not exist yet. A grant with no caller is one nobody can justify
when they find it later.

Added to the marker and to internal/rbacaudit's table, which compares the two
in both directions -- and both directions were checked by deleting one side
at a time and watching it fail, because a check that only fires one way is
half a check.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 3: The rule — which boosts count

**Files:**
- Create: `internal/controller/boosts.go`
- Test: `internal/controller/boosts_test.go`

**Interfaces:**
- Produces: `func liveBoost(boosts []spawneryv1alpha1.ScaleBoost, group string, now time.Time) int32`

- [ ] **Step 1: Write the failing tests**

```go
func boost(group string, replicas int32, expires *time.Time) spawneryv1alpha1.ScaleBoost {
	b := spawneryv1alpha1.ScaleBoost{
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: group},
			Replicas: replicas,
		},
	}
	if expires != nil {
		b.Spec.ExpiresAt = &metav1.Time{Time: *expires}
	}
	return b
}

func TestBoostsAddUpRatherThanReplacing(t *testing.T) {
	// Two people boosting the same group at once is a non-event, not a race.
	now := time.Unix(1000, 0)
	later := now.Add(time.Hour)
	got := liveBoost([]spawneryv1alpha1.ScaleBoost{
		boost("lobby", 2, &later),
		boost("lobby", 3, &later),
	}, "lobby", now)

	if got != 5 {
		t.Errorf("boost = %d, want 5: two boosts are two boosts", got)
	}
}

func TestAnExpiredBoostCountsForNothing(t *testing.T) {
	// The rule the sweep also uses, and the reason the sweep is not what makes
	// it true: a boost stops counting the moment it expires, not whenever the
	// sweep next runs. Otherwise the boost's effect would outlive its own
	// stated end by up to a sweep interval, and the object would be telling
	// the truth while the group did not.
	now := time.Unix(1000, 0)
	past := now.Add(-time.Second)
	if got := liveBoost([]spawneryv1alpha1.ScaleBoost{boost("lobby", 4, &past)}, "lobby", now); got != 0 {
		t.Errorf("boost = %d, want 0 for an expired one", got)
	}
}

func TestABoostWithNoExpiryCountsForever(t *testing.T) {
	now := time.Unix(1000, 0)
	if got := liveBoost([]spawneryv1alpha1.ScaleBoost{boost("lobby", 2, nil)}, "lobby", now); got != 2 {
		t.Errorf("boost = %d, want 2: no expiry means no end", got)
	}
}

func TestAnotherGroupsBoostIsNotThisGroupsCapacity(t *testing.T) {
	now := time.Unix(1000, 0)
	later := now.Add(time.Hour)
	if got := liveBoost([]spawneryv1alpha1.ScaleBoost{boost("arena", 9, &later)}, "lobby", now); got != 0 {
		t.Errorf("boost = %d, want 0: a boost names one group", got)
	}
}

func TestABoostExpiringExactlyNowHasExpired(t *testing.T) {
	// The boundary, asserted rather than left to whichever way the comparison
	// happened to be written. "Until 20:00" means it is over at 20:00.
	now := time.Unix(1000, 0)
	if got := liveBoost([]spawneryv1alpha1.ScaleBoost{boost("lobby", 2, &now)}, "lobby", now); got != 0 {
		t.Errorf("boost = %d, want 0: expiring now means expired", got)
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run 'TestBoosts|TestAnExpired|TestABoost|TestAnotherGroups'`

- [ ] **Step 3: Write it**

A loop, a sum, and the two filters. Its doc comment carries the one thing the
signature does not say: the clock is passed in rather than read, so a test
asserts a boundary instead of racing one.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/boosts.go internal/controller/boosts_test.go
git commit -m "$(cat <<'EOF'
feat(controller): which boosts count, as a pure function

A boost stops counting the moment it expires and not whenever the sweep next
runs, which is the difference between an object that tells the truth and one
whose effect outlives its own stated end by up to a sweep interval.

Two boosts on one group add up. A second does not replace a first, so two
people boosting at once is a non-event rather than a race.

Expiring exactly now counts as expired, and there is a test for the boundary
rather than whichever way the comparison happened to be written: "until
20:00" means it is over at 20:00.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 4: The scaler reads it

**Files:**
- Modify: `internal/controller/scaling.go`
- Modify: `internal/controller/servergroup_controller.go`
- Test: `internal/controller/scaling_test.go`

**Interfaces:**
- Produces: `ScalingInputs.Boost int32`, added to `MinReplicas` wherever the
  floor is read.

- [ ] **Step 1: Write the failing tests**

```go
func TestABoostRaisesTheFloor(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100,
		Boost:   2,
		PodHash: "current",
	})
	if got.Create != 3 {
		t.Errorf("Create = %d, want 3: a floor of 1 plus a boost of 2", got.Create)
	}
}

func TestTheCeilingStillBindsAgainstABoost(t *testing.T) {
	// The one a person could get wrong in a hurry, and the reason the boost
	// is added to the floor rather than to both: maxReplicas is an
	// instruction, and a command typed in a chat window must not lift it.
	got := DecideSize(ScalingInputs{
		MinReplicas: 1, MaxReplicas: 2,
		SpareSlots: 40, MaxPlayers: 100,
		Boost:   50,
		PodHash: "current",
	})
	if got.Create > 2 {
		t.Errorf("Create = %d, want at most the ceiling of 2", got.Create)
	}
}

func TestABoostOfZeroChangesNothing(t *testing.T) {
	// The ordinary case: no boost objects exist. It must produce exactly what
	// the group produced before this field existed.
	with := DecideSize(ScalingInputs{MinReplicas: 2, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100, Boost: 0, PodHash: "current"})
	without := DecideSize(ScalingInputs{MinReplicas: 2, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100, PodHash: "current"})
	if with.Create != without.Create {
		t.Errorf("a zero boost changed the decision: %d vs %d", with.Create, without.Create)
	}
}
```

- [ ] **Step 2: Run and watch them fail**

- [ ] **Step 3: Add the field and use it**

`ScalingInputs.Boost`, with a comment saying it is added to `MinReplicas` and
to nothing else, and that the ceiling is applied after — with the reason.

**Read every use of `MinReplicas` in `scaling.go` before editing.** There is
more than one, and a boost that reached the demand rule but not the floor rule
(or the reverse) would be a group that creates servers it then deletes.

- [ ] **Step 4: Collect boosts at the call site**

In `ServerGroupReconciler`, list `ScaleBoostList` in the group's namespace and
pass `liveBoost(...)` into `ScalingInputs`. Scope the List to the namespace.

Add a `Watches` on `ScaleBoost` so a created or deleted boost wakes its group
rather than waiting out the resync — and add the `watch` verb's justification
to the table entry, which Task 2 already wrote.

- [ ] **Step 5: Run the package**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/`

- [ ] **Step 6: Commit**

```bash
git add internal/controller
git commit -m "$(cat <<'EOF'
feat(controller): a boost raises a group's floor, never its ceiling

Added to MinReplicas and to nothing else. maxReplicas is applied after, and
there is a test for a boost of fifty against a ceiling of two: a ceiling is an
instruction, and the thing that will create these is a command somebody types
in a hurry.

A boost of zero produces exactly what the group produced before the field
existed, asserted rather than assumed, because that is the state every group
in every existing cluster is in.

The group watches ScaleBoosts, so one created or deleted wakes it rather than
waiting out a resync -- a boost that took thirty seconds to do anything would
be typed twice.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 5: The group says what a boost did

An unexplained number is this milestone's likeliest failure. An admin sees a
group at four servers with `minReplicas: 1` and nothing anywhere saying why.

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (one status field)
- Modify: `internal/controller/servergroup_controller.go`
- Test: `internal/controller/servergroup_controller_test.go`

- [ ] **Step 1: Write the failing envtest**

```go
func TestAGroupSaysHowMuchOfItsFloorIsABoost(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	expires := metav1.NewTime(f.clock.now.Add(time.Hour))
	if err := f.c.Create(f.ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-boost", Namespace: f.ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			Replicas:  2,
			ExpiresAt: &expires,
		},
	}); err != nil {
		t.Fatalf("create the boost: %v", err)
	}

	f.reconcileGroup(t, r)

	// Re-read: the reconciler wrote the status, and the fixture's copy is the
	// one it held before.
	group := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx,
		types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, group); err != nil {
		t.Fatalf("get the group: %v", err)
	}
	if got := group.Status.BoostedReplicas; got != 2 {
		t.Errorf("status.boostedReplicas = %d, want 2", got)
	}
}

func TestAGroupWithNoBoostReportsZeroRatherThanNothing(t *testing.T) {
	// Zero and present, not absent: an admin comparing two groups should not
	// have to tell "no boost" from "this operator is too old to say".
}
```

- [ ] **Step 2: Add the field and publish it**

```go
	// BoostedReplicas is how much of this group's current floor comes from
	// ScaleBoost objects rather than from spec.scaling.minReplicas.
	//
	// It exists because the alternative is a group running four servers with a
	// declared floor of one and nothing anywhere explaining it. A person
	// looking at that will edit the spec, which is the one thing that would
	// not help.
	// +optional
	BoostedReplicas int32 `json:"boostedReplicas"`
```

Add it to the printer columns beside `READY`.

- [ ] **Step 3: Regenerate, run, commit**

```bash
git add api internal config charts
git commit -m "$(cat <<'EOF'
feat(api): a group reports how much of its floor is a boost

The likeliest failure of this milestone is not a wrong number, it is an
unexplained one: a group running four servers with a declared floor of one
and nothing saying why. A person meeting that will edit the spec, which is
the single thing that would not help.

Zero and present rather than absent, so that comparing two groups does not
mean telling "no boost" from "this operator is too old to say".

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 6: Expired boosts are swept

**Files:**
- Modify: `internal/controller/orphan.go`
- Test: `internal/controller/orphan_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestTheSweepRemovesAnExpiredBoostAndLeavesALiveOne(t *testing.T) {
	// Both in one test, because a sweep that deleted everything would pass a
	// test that only asserted the expired one was gone.
	f := newFixture(t)
	past := metav1.NewTime(f.clock.now.Add(-time.Minute))
	future := metav1.NewTime(f.clock.now.Add(time.Hour))
	for name, expires := range map[string]metav1.Time{"stale": past, "live": future} {
		if err := f.c.Create(f.ctx, &spawneryv1alpha1.ScaleBoost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
			Spec: spawneryv1alpha1.ScaleBoostSpec{
				GroupRef:  spawneryv1alpha1.ObjectRef{Name: f.group.Name},
				Replicas:  1,
				ExpiresAt: &expires,
			},
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	sweeper := &OrphanReconciler{Client: f.c, Agents: f.agents, Clock: f.clock.Now}
	if err := sweeper.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	boosts := &spawneryv1alpha1.ScaleBoostList{}
	if err := f.c.List(f.ctx, boosts, client.InNamespace(f.ns)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(boosts.Items) != 1 || boosts.Items[0].Name != "live" {
		t.Fatalf("boosts = %v, want only the live one", boosts.Items)
	}
}
```

**Read `orphan_test.go` for how it builds an `OrphanReconciler` before using
the literal above** — the struct has gained fields across milestones and this
plan names the ones it had when the plan was written, which is exactly the
kind of thing that has drifted before.

- [ ] **Step 2: Add it to the sweep**

The orphan sweep is the right home and the comment should say why: it already
exists to remove objects that should no longer be there, and an expired boost
is exactly that. A reconciler of its own would be a second timer for one
`List` and one `Delete`.

**The sweep is not what makes a boost stop counting** — Task 3's rule is, and
it reads the clock. The sweep only removes the object. Say that where somebody
would otherwise assume the interval is the resolution of the expiry.

- [ ] **Step 3: Run and commit**

```bash
git add internal/controller
git commit -m "$(cat <<'EOF'
feat(controller): the sweep removes expired boosts

The orphan sweep already exists to remove objects that should no longer be
there, and an expired boost is exactly that; a reconciler of its own would be
a second timer for one List and one Delete.

It is not what makes a boost stop counting. liveBoost reads the clock, so the
effect ends at the stated time and the sweep only tidies the object away
afterwards -- which is the difference between an expiry that means something
and one whose resolution is a sweep interval.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 7: Write down what an upgrade brings

**Files:**
- Modify: `docs/upgrading.md`
- Modify: `docs/network-boundaries.md`

- [ ] **Step 1: The upgrade note**

A chart upgrade installs a fifth CRD. Nothing uses it until somebody creates
one, so the upgrade changes no running group — say that plainly, because "a
new CRD" reads as "something will move".

Also: a boost is how capacity is added at runtime, and the `ServerGroup`'s own
spec stays the place for a lasting change. An operator who wants four servers
every Saturday should edit the file, not type a command every Saturday.

- [ ] **Step 2: The boundaries note**

The operator can now delete objects a person may have created by hand. It is
scoped to expired ones and to `ScaleBoost` alone, and that is worth one
sentence where the rest of what the operator may touch is recorded.

- [ ] **Step 3: Commit**

---

## Done when

- [ ] `make test` passes, including `internal/rbacaudit`
- [ ] `make lint` reports nothing
- [ ] `make manifests` leaves no diff — the committed CRDs and chart match
- [ ] Both RBAC drift directions were checked by deleting one side at a time
- [ ] `grep -rn 'scaleboosts' config/rbac/role.yaml` shows no `create` and no
      `update`: this milestone reads and sweeps, and 7c-2 adds the maker
- [ ] Nothing was pushed and no tag was created

## What 7c-1 leaves

- **Nothing creates a boost.** `kubectl create` is the interface, which is the
  honest state of a milestone that deliberately ships the mechanism before the
  command. 7c-2 adds `/cloud start` and the `create` grant together.
- **No default expiry exists anywhere.** The type allows none and supplies
  none; the hour the spec talks about is the *command's* default and lands
  with the command.
- **`Lifecycle.boost` is not on the API.** It needs the request machinery from
  7b-5 pointed at a new verb, which is 7c-2's first task.
