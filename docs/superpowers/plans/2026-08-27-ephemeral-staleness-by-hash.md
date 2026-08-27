# Milestone 7a — ephemeral staleness by render hash

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An ephemeral `ServerGroup` decides a server is stale by comparing render hashes, not `metadata.generation`, so that editing capacity stops replacing a fleet of functionally identical servers.

**Architecture:** `podspec.DesiredServerHash` is already computed on every pass, already stamped onto `Server.spec.podHash`, and already read by the persistent sizing rule. This milestone routes the ephemeral rule — and the two reporting functions beside it — through the same value, using the same adoption rule for a server that predates the field. No new concept is introduced; a third caller starts reading a value two callers already read.

**Tech Stack:** Go, controller-runtime, envtest. No proto change, no CRD change, no agent change.

**Spec:** [`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../specs/2026-08-27-cloud-api-design.md) §2

## Global Constraints

- Every build and test command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`
- Commit messages are Conventional Commits with English subjects, and every
  commit ends with exactly these two trailers:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **Never push, never merge, and never create a tag.** The repository owner
  authorises each push and each tag individually.
- **Never run `git config` in any form.**
- `make test` is slow because envtest is slow. That is expected; do not reach
  for a shortcut that skips it.
- envtest shares one control plane across the package, with no namespace
  controller and no cleanup between tests. Any test that Lists cluster-wide
  sees other tests' leftovers — scope every List to the namespace under test.

## What this plan does NOT change

Read this before starting: two uses of `metadata.generation` in this package
are correct and stay.

- `selectFailedForPruning` (`internal/controller/candidates.go:402`) sorts
  retained failures newest-generation-first. **A hash has no order.** Pruning
  needs to know which failure is more recent, which a digest cannot answer.
  `ServerView.Generation` stays on the type for this.
- `Server.spec.groupGeneration` keeps being stamped at creation. It is what
  the above reads.
- `ofGeneration` (`servergroup_controller.go:723`), which narrows an ephemeral
  group's views before `CountFailures` runs, **stays on the generation** — and
  this one was going to be changed until the code was read. Failure counting
  runs at `:277`, unconditionally and deliberately: a group whose Network was
  deleted is exactly the one that piles failures up. `podHash` is computed at
  `:395` under `if mayResize`, so on that path there is no hash to compare.
  A value that is always present cannot be replaced by one that is sometimes
  empty, on the path that matters most.

  This leaves a real open item rather than a tidy one, and Task 7 records it:
  raising `minReplicas` still clears the group's failure streak, so 7c's
  `/cloud start` will clear a `CrashLoopBackoff` as a side effect. **7c owns
  that**, and it has options this milestone does not — refusing a scale on a
  `Degraded` group, or carrying the streak across explicitly.

---

### Task 1: The staleness predicate

The rule is not `a != b`. A server created before `spec.podHash` existed
carries `""` and must be **adopted, not replaced** — otherwise the first
reconcile after an operator upgrade retires every ephemeral server in the
installation. `DecidePersistentSize` already has this rule inline
(`internal/controller/persistent.go:224`); this task lifts it into a named
function both callers can share.

**Files:**
- Modify: `internal/controller/candidates.go`
- Test: `internal/controller/candidates_test.go`

**Interfaces:**
- Consumes: `ServerView` (existing, `candidates.go:28`), whose `PodHash` field
  already exists at `candidates.go:70`.
- Produces: `func staleSpec(v ServerView, want string) bool` — reports whether
  `v` was rendered under a spec that is no longer current. False whenever
  either side is empty.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/candidates_test.go`:

```go
func TestStaleSpec(t *testing.T) {
	cases := []struct {
		name string
		view string
		want string
		out  bool
	}{
		{"same hash is current", "abc123", "abc123", false},
		{"different hash is stale", "abc123", "def456", true},
		// Both directions of adoption. A server that predates spec.podHash
		// carries "", and so does the desired hash on a pass that could not
		// compute one -- an unresolvable Network, for instance. Neither is
		// evidence that this server is out of date, and treating it as such
		// would retire the whole group on the first pass after an upgrade.
		{"view without a hash is adopted", "", "def456", false},
		{"no desired hash compares nothing", "abc123", "", false},
		{"both empty is not stale", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := staleSpec(ServerView{PodHash: tc.view}, tc.want)
			if got != tc.out {
				t.Fatalf("staleSpec(view %q, want %q) = %v, want %v",
					tc.view, tc.want, got, tc.out)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestStaleSpec`

Expected: a compile failure, `undefined: staleSpec`.

- [ ] **Step 3: Write the implementation**

Add to `internal/controller/candidates.go`, directly below the `ServerView`
type so it sits with the field it reads:

```go
// staleSpec reports whether this server was rendered under a spec that is no
// longer current, by comparing render hashes rather than generations.
//
// Either side being empty means "do not compare", and both cases are
// adoption rather than staleness. A view's empty hash is a server created
// before spec.podHash existed: nominating it would retire every server in
// the installation on the first reconcile after the upgrade. An empty want
// is a pass that could not compute a hash -- an unresolvable Network is the
// ordinary way -- and a rule that read that as "everything is stale" would
// turn a Network outage into a fleet changeover.
//
// DecidePersistentSize had this rule inline before this function existed
// (persistent.go, the stale loop) and now calls it, so the two group kinds
// cannot drift apart on what "stale" means.
func staleSpec(v ServerView, want string) bool {
	if want == "" || v.PodHash == "" {
		return false
	}
	return v.PodHash != want
}
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestStaleSpec -v`

Expected: PASS, five subtests.

- [ ] **Step 5: Route the persistent rule through it**

In `internal/controller/persistent.go`, replace the inline condition (the
`if in.PodHash == "" || v.PodHash == "" || v.PodHash == in.PodHash` line) with
its negation through the new function. The surrounding block keeps its
existing shape; only the condition changes:

```go
		if !staleSpec(v, in.PodHash) {
```

Keep the two comment lines above it that explain adoption — they now describe
what `staleSpec` does and are still the reason a reader is standing there.

- [ ] **Step 6: Run the persistent suite unchanged**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run 'Persistent'`

Expected: PASS. This is a refactor with no behaviour change; a failure here
means the negation was transcribed wrong.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/candidates.go internal/controller/candidates_test.go internal/controller/persistent.go
git commit -m "$(cat <<'EOF'
refactor(controller): the adoption rule for a render hash gets a name

DecidePersistentSize compared spec.podHash inline, with two empty checks
that are not an optimisation but the rule itself: a server created before
the field existed carries "", and so does the desired hash on a pass that
could not compute one. Reading either as stale would retire a fleet.

The ephemeral rule is about to need the same three-way comparison, and two
copies of a rule whose whole content is its edge cases is how the two group
kinds come to disagree about what stale means.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 2: The ephemeral sizing rule reads the hash

Four sites in `scaling.go` compare `v.Generation` against `in.Generation`.
They all become `staleSpec(v, in.PodHash)`. `ScalingInputs.Generation` is
deleted rather than left unread — a field nothing reads is a trap for the next
person who assumes it still decides something.

**Files:**
- Modify: `internal/controller/scaling.go:57-63` (the field), `:314`, `:357`, `:471`, `:678`
- Modify: `internal/controller/servergroup_controller.go:777-791` (the call site)
- Test: `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: `staleSpec(v ServerView, want string) bool` from Task 1.
- Produces: `ScalingInputs.PodHash string` replacing `ScalingInputs.Generation int64`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/scaling_test.go`. These are the two halves of
the milestone's acceptance criterion, written as the smallest cases that can
tell them apart:

```go
// A capacity edit must retire nothing. This is the whole point of 7a: before
// it, every field of the spec moved metadata.generation, so raising a floor
// replaced a fleet of functionally identical servers.
func TestDecideSizeCapacityEditRetiresNothing(t *testing.T) {
	views := []ServerView{
		{Name: "a", Phase: phase.Ready, PodHash: "same", Slots: 100, Players: 10},
		{Name: "b", Phase: phase.Ready, PodHash: "same", Slots: 100, Players: 10},
	}
	got := DecideSize(ScalingInputs{
		Views:       views,
		MinReplicas: 2,
		MaxReplicas: 10,
		SpareSlots:  40,
		MaxPlayers:  100,
		// The desired hash still matches: nothing that reaches the pod moved.
		PodHash:        "same",
		MaxUnavailable: 1,
		PendingDeletes: map[string]bool{},
		PendingRetires: map[string]bool{},
	})
	if len(got.Retire) != 0 {
		t.Fatalf("a capacity edit retired %v, want nothing", got.Retire)
	}
}

// An image edit must still retire, one at a time under maxUnavailable. This
// passes today and is the regression guard for the change above: a rule that
// retired nothing at all would satisfy the test before this one.
func TestDecideSizeImageEditStillRetires(t *testing.T) {
	views := []ServerView{
		{Name: "a", Phase: phase.Ready, PodHash: "old", Slots: 100, Players: 10},
		{Name: "b", Phase: phase.Ready, PodHash: "new", Slots: 100, Players: 10},
	}
	got := DecideSize(ScalingInputs{
		Views:       views,
		MinReplicas: 2,
		MaxReplicas: 10,
		SpareSlots:  40,
		MaxPlayers:  100,
		// The pod changed, so "old" is stale and "new" is its replacement.
		PodHash:        "new",
		MaxUnavailable: 1,
		PendingDeletes: map[string]bool{},
		PendingRetires: map[string]bool{},
	})
	if len(got.Retire) != 1 || got.Retire[0] != "a" {
		t.Fatalf("Retire = %v, want exactly [a]", got.Retire)
	}
}

// Adoption at the sizing rule, not only at the predicate: a group whose
// servers all predate spec.podHash retires none of them. Without this, the
// first reconcile after an operator upgrade is a full fleet changeover.
func TestDecideSizeAdoptsHashlessServers(t *testing.T) {
	views := []ServerView{
		{Name: "a", Phase: phase.Ready, PodHash: "", Slots: 100, Players: 10},
		{Name: "b", Phase: phase.Ready, PodHash: "", Slots: 100, Players: 10},
	}
	got := DecideSize(ScalingInputs{
		Views:          views,
		MinReplicas:    2,
		MaxReplicas:    10,
		SpareSlots:     40,
		MaxPlayers:     100,
		PodHash:        "fresh",
		MaxUnavailable: 1,
		PendingDeletes: map[string]bool{},
		PendingRetires: map[string]bool{},
	})
	if len(got.Retire) != 0 {
		t.Fatalf("hashless servers were retired: %v", got.Retire)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run 'TestDecideSize(CapacityEdit|ImageEdit|Adopts)'`

Expected: a compile failure, `unknown field PodHash in struct literal of type ScalingInputs`. That is the honest first failure — the field does not exist yet.

- [ ] **Step 3: Replace the field on `ScalingInputs`**

In `internal/controller/scaling.go`, delete the `Generation int64` field and
its comment (`:57-63`) and put this in its place:

```go
	// PodHash is podspec.DesiredServerHash for the group as it stands now: the
	// digest of the pod and config this operator would render for one of its
	// servers. A view whose spec.podHash differs is stale; see staleSpec for
	// what an empty hash on either side means.
	//
	// It replaced metadata.generation here in milestone 7a, and the reason is
	// what generation could not tell apart. Generation moves on *every* field
	// of the spec, so raising minReplicas made every running server stale and
	// the group replaced a fleet of functionally identical pods. The digest
	// moves only when the rendered pod or the config actually changes, which
	// is the question the changeover is really asking.
	//
	// Its job is unchanged and still confined: it selects which stale server
	// retires and whether a changeover has begun. It never enters
	// provisionalCapacity or readyFree. See the type comment above -- the
	// hazard it describes is about filtering capacity arithmetic and applies
	// to this field exactly as it applied to the last one.
	PodHash string
```

Then update the type comment above `ScalingInputs` (`:28-36`): it opens with
"The group's generation is here, and it is confined to one job." Replace that
sentence with "The group's render hash is here, and it is confined to one
job." and leave the rest of the paragraph — the runaway-creates hazard it
describes is unchanged and is still the reason the arithmetic stays blind.

- [ ] **Step 4: Convert the four comparison sites**

All four are mechanical, but they are not all the same direction — read each
one before changing it.

`coldStart`, `scaling.go:314`, inside the view loop:
```go
		if !staleSpec(v, in.PodHash) {
```
(was `if v.Generation == in.Generation {`)

`selectRetirement`, `scaling.go:357`:
```go
		if !staleSpec(v, in.PodHash) {
```
(was `if v.Generation == in.Generation {`)

`staleRemains`, `scaling.go:471`:
```go
		if staleSpec(v, in.PodHash) && v.countsTowardSize() {
```
(was `if v.Generation != in.Generation && v.countsTowardSize() {`)

`decideSize`'s eligibility loop, `scaling.go:678`:
```go
			if changeover && !staleSpec(v, in.PodHash) {
```
(was `if changeover && v.Generation == in.Generation {`)

Also fix `coldStart`'s comment at `:299`, which says "createServer stamps
group.Generation on it". It still does, but the sentence is now explaining the
wrong field. Replace that clause with "createServer stamps the group's current
render hash on it".

- [ ] **Step 5: Update the call site**

In `internal/controller/servergroup_controller.go`, in the `group.IsEphemeral()`
branch of `size` (`:777`), replace `Generation: group.Generation,` with:

```go
				PodHash:        podHash,
```

`podHash` is already a parameter of `size` and is already computed by the
caller — the persistent branch two cases below has been reading it since 5b.
Nothing new has to be plumbed.

- [ ] **Step 6: Run the new tests and the whole package**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run 'TestDecideSize' -v`

Expected: PASS, including the three new cases.

Then the whole package:

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/`

Expected: existing tests that built `ScalingInputs{Generation: ...}` fail to
compile. **Convert each to `PodHash`, and read what it was asserting as you
go.** A test that set two different generations was asserting a changeover;
give it two different hash strings. A test that set none was asserting no
changeover; leave it, since the zero value of the new field means "compare
nothing", which is the same outcome.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/scaling.go internal/controller/scaling_test.go internal/controller/servergroup_controller.go
git commit -m "$(cat <<'EOF'
fix(controller): an ephemeral group is stale by render hash, not generation

metadata.generation moves on every field of a ServerGroup's spec, and 4b's
retirement rule read it as the definition of stale. So retuning spareSlots,
or raising minReplicas, replaced every server in the group with a
functionally identical one -- players finishing their sessions on servers
being swapped for copies of themselves. 4b recorded this as its own open
item and it stayed open.

podspec.DesiredServerHash answers the question the changeover is actually
asking: has the rendered pod or its config changed. It has existed since
5b, is stamped on every Server at creation, and DecidePersistentSize and the
ProxyGroup rollout both already read it. This makes the ephemeral rule the
third reader rather than inventing anything.

ScalingInputs.Generation is deleted rather than left in place unread. The
capacity arithmetic stays blind to both, for the reason the type comment
gives unchanged: a filter there makes running servers stop counting the
instant the spec moves, and the group orders a full replacement set up to
maxReplicas.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 3: `status.freeSlots` stops lying after a capacity edit

`AggregateGroup` counts free slots on Ready servers **of the current
generation** (`candidates.go:488`). Left alone, Task 2 creates a reporting
defect it did not have before: after `minReplicas` is raised, the generation
moves, every server is a previous one, and a perfectly healthy group publishes
`FREE SLOTS 0` on its own printcolumn.

The generation filter is there for a real reason — its comment says a rolling
update would otherwise never create replacements — and that reason is served
by the hash exactly as well.

**Files:**
- Modify: `internal/controller/candidates.go:477-495`
- Modify: `internal/controller/servergroup_controller.go:651`
- Modify: `api/v1alpha1/servergroup_types.go:260-263` (doc comment only)
- Test: `internal/controller/candidates_test.go`

**Interfaces:**
- Consumes: `staleSpec` from Task 1.
- Produces: `func AggregateGroup(views []ServerView, podHash string) GroupTotals` —
  the second parameter changes type and meaning; the return type is unchanged.

- [ ] **Step 1: Write the failing test**

```go
// A capacity edit does not move the render hash, so every server still counts
// toward the published free slots. Before 7a this published zero, because the
// generation had moved and the filter read that as "everything is stale" --
// a healthy group reporting FREE SLOTS 0 on its own printcolumn.
func TestAggregateGroupCapacityEditKeepsFreeSlots(t *testing.T) {
	views := []ServerView{
		{Name: "a", Phase: phase.Ready, PodHash: "same", Slots: 100, Players: 30},
		{Name: "b", Phase: phase.Ready, PodHash: "same", Slots: 100, Players: 10},
	}
	got := AggregateGroup(views, "same")
	if got.FreeSlots != 160 {
		t.Fatalf("FreeSlots = %d, want 160", got.FreeSlots)
	}
}

// The filter's original job survives: a stale server's free slots must not
// satisfy the scaler, or a rolling update would never build a replacement.
func TestAggregateGroupExcludesStaleFreeSlots(t *testing.T) {
	views := []ServerView{
		{Name: "old", Phase: phase.Ready, PodHash: "old", Slots: 100, Players: 0},
		{Name: "new", Phase: phase.Ready, PodHash: "new", Slots: 100, Players: 40},
	}
	got := AggregateGroup(views, "new")
	if got.FreeSlots != 60 {
		t.Fatalf("FreeSlots = %d, want 60 (only the current server counts)", got.FreeSlots)
	}
	// The other three totals count every server whatever its hash, and always
	// did. Asserting them here is what keeps this change from narrowing them
	// by accident.
	if got.Replicas != 2 || got.ReadyReplicas != 2 || got.OnlinePlayers != 40 {
		t.Fatalf("totals = %+v, want Replicas 2, ReadyReplicas 2, OnlinePlayers 40", got)
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestAggregateGroup`

Expected: a compile failure — `AggregateGroup` still takes an `int64`.

- [ ] **Step 3: Change the signature and the filter**

In `internal/controller/candidates.go`:

```go
// AggregateGroup sums the views up for the group status.
func AggregateGroup(views []ServerView, podHash string) GroupTotals {
	var t GroupTotals
	for _, v := range views {
		t.Replicas++
		if v.Phase == phase.Ready {
			t.ReadyReplicas++
		}
		if !v.Stale {
			t.OnlinePlayers += v.Players
		}
		if v.Phase == phase.Ready && !staleSpec(v, podHash) && !v.Stale {
			free := v.Slots - v.Players
			if free > 0 {
				t.FreeSlots += free
			}
		}
	}
	return t
}
```

Update `GroupTotals.FreeSlots`'s comment (`:470-473`) — it says "of the current
generation". It is now "rendered under the group's current spec", and the
sentence after it, about why stale ones are excluded, is unchanged and still
correct.

- [ ] **Step 4: Update the caller**

`internal/controller/servergroup_controller.go:651`:

```go
	totals := AggregateGroup(views, podHash)
```

`podHash` is declared at `:395` and this line is `:651`, both inside
`Reconcile`, so it is in scope. Do not recompute it — two computations of one
digest in a pass is what `DesiredServerHash`'s sorted-marshal comment exists to
prevent.

**One behaviour change to make deliberately rather than discover.** `podHash` is
computed under `if mayResize` (`:394`), so on a pass where the group's Network
is unusable it stays `""`. `staleSpec` compares nothing against an empty want,
so on that path every Ready server now contributes to `FreeSlots`, where the
generation filter used to narrow them. That is the better answer of the two —
a group whose Network is gone is not scaling anyway, and publishing the
capacity that exists beats publishing zero — but it is a change, and it is
this step that makes it.

- [ ] **Step 5: Update the CRD field doc**

In `api/v1alpha1/servergroup_types.go:260-263`, the comment says "ready servers
of the current generation". Change "generation" to "spec". This is a comment on
a published API field; leaving it would describe a rule the code no longer has.

- [ ] **Step 6: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestAggregateGroup -v`

Expected: PASS.

Then regenerate the CRDs, since a field comment changed:

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make manifests`

Expected: a diff in `config/crd/bases/` and in the chart's generated CRD,
carrying the new description text and nothing else.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/candidates.go internal/controller/candidates_test.go internal/controller/servergroup_controller.go api/v1alpha1/servergroup_types.go config/crd charts
git commit -m "$(cat <<'EOF'
fix(controller): status.freeSlots follows the render hash too

AggregateGroup counted free slots only on Ready servers of the current
generation. With the sizing rule moved off generation, that filter would
have turned a capacity edit into a healthy group publishing FREE SLOTS 0 on
its own printcolumn -- a reporting defect the previous commit would have
introduced rather than one it found.

The filter's own reason survives intact: a stale server's free slots must
not satisfy the scaler, or a rolling update would never build a
replacement. The hash serves that as well as the generation did, and now
means it only when the pod actually changed.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 4: The test helpers stop describing the old rule

`servergroup_controller_test.go` carries two helpers whose names and comments
are about to describe a rule the code no longer has. Both keep working — but
for a reason neither one states, and a helper that passes for an unstated
reason is how a suite comes to assert nothing.

`bumpGeneration` (`:1967`) says it "moves group.Generation forward, so every
server created under the previous value reads as stale to the scaling rules."
After Task 2 that sentence is false. It goes on working only because its body
sets `Spec.Image`, which reaches `DesiredServerHash` — an accident from this
helper's point of view, and the only thing standing between eleven tests and
asserting nothing.

**Files:**
- Modify: `internal/controller/servergroup_controller_test.go:1967-1978`, `:1980-1990`, and its eleven call sites

**Interfaces:**
- Consumes: nothing.
- Produces: `func (f *fixture) bumpPodSpec(t *testing.T)`, replacing
  `bumpGeneration`. Same body, same effect, honest name.

- [ ] **Step 1: Confirm the count before touching anything**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c grep -c bumpGeneration internal/controller/servergroup_controller_test.go`

Expected: `11`. If it is not 11, the rename below has a call site this plan did
not see — find it before continuing.

- [ ] **Step 2: Rename the helper and correct its comment**

Rename `bumpGeneration` to `bumpPodSpec` at its definition and at all eleven
call sites. The body does not change.

Replace the comment's opening sentence — "bumpGeneration performs a real spec
update, the way an operator rolling a new image would: it moves
group.Generation forward, so every server created under the previous value
reads as stale to the scaling rules." — with:

```go
// bumpPodSpec performs a real spec update, the way an operator rolling a new
// image would. It changes spec.image, which is what reaches
// podspec.DesiredServerHash, so every server created under the previous value
// reads as stale to the scaling rules.
//
// It was called bumpGeneration until 7a, and the rename is not cosmetic. The
// old name described the mechanism the rules used to read, and after 7a a
// helper that only moved metadata.generation -- by editing spareSlots, say --
// would make nothing stale at all, while every test built on it went on
// passing and asserting nothing. The image edit below is now the load-bearing
// line, and the name says so.
```

Keep the rest of the comment — the paragraphs about pinning `spec.update`
explicitly and about why `spareSlots` is 150 are unchanged and still the
reason those two lines are in the body.

- [ ] **Step 3: Say why `serversOfGeneration` is still valid**

`serversOfGeneration` (`:1981`) filters on `Spec.GroupGeneration`, and seven
tests use it to mean "servers of the current spec". That is still true, but
only because `bumpPodSpec` moves the image and the generation together. Add
above it:

```go
// serversOfGeneration filters the group's servers down to one generation.
//
// Since 7a the sizing rules read spec.podHash and not this field, so this is a
// proxy for "of the current spec" rather than the rule itself. It is a sound
// proxy in every test here for one reason: bumpPodSpec changes spec.image,
// which moves the render hash and the generation together. A test that edited
// only capacity would move the generation alone, and this helper would report
// every server stale while the rules correctly reported none. Use PodHash
// directly in such a test rather than reaching for this.
```

- [ ] **Step 4: Run the suite**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/`

Expected: PASS. This is a rename plus comments; a failure means a call site was
missed.

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c grep -c bumpGeneration internal/controller/servergroup_controller_test.go`

Expected: no matches, exit status 1.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/servergroup_controller_test.go
git commit -m "$(cat <<'EOF'
test(controller): bumpGeneration is named for the rule it no longer uses

The helper's comment claimed it made servers stale by moving
metadata.generation. After 7a that is false, and it goes on working only
because its body happens to set spec.image, which is what reaches
DesiredServerHash. Eleven tests stand on that accident.

Renamed to bumpPodSpec, with the image edit named as the load-bearing line.
A helper that only moved the generation -- by editing spareSlots, say --
would now make nothing stale while every test built on it passed and
asserted nothing, which is the failure this rename exists to prevent.

serversOfGeneration keeps its name and gains the reason it is still a sound
proxy for "of the current spec": bumpPodSpec moves both values together.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 5: Ephemeral servers get adopted too

`adoptServers` stamps the current hash onto servers whose `spec.podHash` is
empty, and it returns early for ephemeral groups
(`servergroup_controller.go:1509-1517`): "Adoption applies to persistent groups
only; an ephemeral group's servers are skipped outright." That was correct
while nothing read the field for ephemeral groups. It is now a hole.

Without this task, an ephemeral server created before the upgrade carries `""`
forever, `staleSpec` adopts it on every pass, and **it is immune to every
future image change** until it happens to churn for some other reason.

**Files:**
- Modify: `internal/controller/servergroup_controller.go:1505-1543`
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `groupReconciler(f)`, `f.reconcileGroup(t, r)`, `f.createServer(name)`,
  `f.server(name)`, `f.listServers(t)` — all existing in this package's fixture
  (`suite_test.go:203`, `servergroup_controller_test.go:58,83,92`).
- Produces: no signature change. `adoptServers` keeps its parameters and drops
  its early return.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/servergroup_controller_test.go`:

```go
// An ephemeral server created before spec.podHash had a reader on this side
// must be stamped, not left hashless. staleSpec adopts a hashless view on
// every pass, so a server that is never stamped is never stale -- immune to
// every future image change until it churns for an unrelated reason.
func TestAdoptStampsEphemeralServers(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	srv := f.createServer("lobby-old")
	// The shape of a server from before the field had a reader here.
	srv.Spec.PodHash = ""
	if err := f.c.Update(f.ctx, srv); err != nil {
		t.Fatalf("clear podHash: %v", err)
	}

	f.reconcileGroup(t, r)

	if got := f.server("lobby-old").Spec.PodHash; got == "" {
		t.Fatal("an ephemeral server was left without a render hash: it can never be stale")
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestAdoptStampsEphemeral -v`

Expected: FAIL with "an ephemeral server was left without a render hash".

- [ ] **Step 3: Remove the early return**

In `adoptServers`, delete these three lines:

```go
	if group.IsEphemeral() {
		return nil
	}
```

Then rewrite the function's doc comment, because it currently states the rule
being removed:

```go
// adoptServers stamps the freshly computed render hash onto every server whose
// spec.podHash is still empty: one created before this field existed, or --
// for an ephemeral group -- before 7a gave the field a reader on this side.
//
// It covered persistent groups only until 7a. That was correct while nothing
// read the field for an ephemeral group, and became a hole the moment
// staleSpec did: a hashless view is adopted on every pass, so a server that is
// never stamped is never stale, and no image change would ever replace it.
```

Keep the existing comment on the `srv.Spec.PodHash != ""` guard and the long
one above the patch about what adoption costs — both apply to ephemeral
servers word for word, including the bounded one-edit window.

- [ ] **Step 4: Run the test and the package**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestAdoptStampsEphemeral -v`

Expected: PASS.

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/servergroup_controller.go internal/controller/servergroup_controller_test.go
git commit -m "$(cat <<'EOF'
fix(controller): an ephemeral server is adopted onto the render hash too

adoptServers returned early for ephemeral groups, which was correct while
nothing on that side read spec.podHash. staleSpec now does, and the early
return became a hole rather than an optimisation: a hashless view is adopted
on every pass, so a server that is never stamped is never stale. Every
ephemeral server predating the upgrade would have been immune to every
future image change until it churned for an unrelated reason.

The cost of adoption is unchanged and is the one already written above the
patch: one spec edit landing inside the same reconcile is adopted along with
the old pod instead of triggering a rebuild. It is a one-time window per
server and it closes for good the first time this runs.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 6: The acceptance criterion, driven against a real API server

Tasks 2 to 5 are unit-level except for adoption. This is the criterion from the
spec §9, driven through real reconciles: a capacity edit that retires nothing,
then an image edit that retires.

It exists because every test in Task 2 feeds `DecideSize` a hand-built
`ScalingInputs`. **None of them would fail if the call site passed the wrong
value**, which is exactly the defect shape `newServerGroupReconciler`'s comment
in `setup.go` describes — found in this package before by deleting an
assignment and watching the suite stay green.

**Files:**
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `newFixture`, `groupReconciler`, `reconcilePass(t, f, r)`
  (`:1908`), `f.markReadyWithPlayers(t, name, players)` (`:1928`),
  `f.listServers(t)` (`:92`), `f.bumpPodSpec(t)` from Task 4.
- Produces: nothing. This task adds no production code.

- [ ] **Step 1: Write the test**

```go
// The milestone's acceptance criterion, driven through real reconciles rather
// than against a hand-built ScalingInputs. Every unit test in this milestone
// supplies PodHash itself, so none of them can fail if `size` passes the wrong
// value at the call site -- the defect shape setup.go's comment on
// newServerGroupReconciler describes, and one this package has been bitten by.
func TestCapacityEditDoesNotRollAnEphemeralGroup(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// Two occupied Ready servers, stamped with the hash the group renders now.
	f.reconcileGroup(t, r)
	for _, s := range f.listServers(t) {
		f.markReadyWithPlayers(t, s.Name, 10)
	}
	reconcilePass(t, f, r)
	requireNoneRetiring(t, f, "before any edit")

	// A capacity edit. metadata.generation moves; the rendered pod does not.
	if err := f.c.Get(f.ctx,
		types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.MinReplicas = 4
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("raise minReplicas: %v", err)
	}
	reconcilePass(t, f, r)

	requireNoneRetiring(t, f, "after raising minReplicas")

	// The spec names spareSlots as its own criterion, and it is the field 4b's
	// open item was actually written about. Same class as minReplicas -- it
	// never reaches BuildServerPod -- but asserted rather than assumed.
	if err := f.c.Get(f.ctx,
		types.NamespacedName{Name: f.group.Name, Namespace: f.ns}, f.group); err != nil {
		t.Fatalf("get group: %v", err)
	}
	f.group.Spec.Scaling.SpareSlots = 60
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("retune spareSlots: %v", err)
	}
	reconcilePass(t, f, r)

	requireNoneRetiring(t, f, "after retuning spareSlots")

	// An image edit. Now the rendered pod really did change, and the
	// changeover must begin -- one at a time, under maxUnavailable.
	f.bumpPodSpec(t)
	reconcilePass(t, f, r)

	var retiring int
	for _, s := range f.listServers(t) {
		if s.Spec.Retire {
			retiring++
		}
	}
	if retiring != 1 {
		t.Fatalf("an image edit retired %d servers, want exactly 1", retiring)
	}
}

// requireNoneRetiring fails if any server of the group carries spec.retire.
// listServers is already scoped to the fixture's namespace, which matters:
// envtest shares one control plane with no cleanup between tests, so a
// cluster-wide List would make another test's leftovers this test's result.
func requireNoneRetiring(t *testing.T, f *fixture, when string) {
	t.Helper()
	for _, s := range f.listServers(t) {
		if s.Spec.Retire {
			t.Fatalf("%s: server %s is retiring and nothing asked it to", when, s.Name)
		}
	}
}
```

- [ ] **Step 2: Run it**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestCapacityEditDoesNotRollAnEphemeralGroup -v`

Expected: PASS. Tasks 2–5 already made it true; this is the guard, not the fix.

If the first half fails because the group creates more servers than expected
after `minReplicas` goes to 4, that is correct behaviour and not a defect —
the assertion is about `spec.retire`, not about the count. Only fix the test if
it is asserting something it should not.

- [ ] **Step 3: Prove the guard can actually fail**

A test that passes on its first run has not been shown to be capable of
failing. Temporarily change one line in `size`
(`servergroup_controller.go`, the ephemeral branch): `PodHash: podHash,` to
`PodHash: "",`.

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestCapacityEditDoesNotRollAnEphemeralGroup -v`

Expected: FAIL at the image-edit assertion, "an image edit retired 0 servers,
want exactly 1". An empty desired hash makes `staleSpec` compare nothing, so no
changeover ever begins.

**Restore the line**, then run `git diff` and confirm the working tree carries
nothing but the new test before continuing.

- [ ] **Step 4: Run the whole suite and the linter**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make test`

Expected: PASS.

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make lint`

Expected: no findings. If `golangci-lint` reports anything in a file this plan
touched, fix it here — CI runs on a cold cache and has found real issues that
five local runs against a warm one reported clean.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/servergroup_controller_test.go
git commit -m "$(cat <<'EOF'
test(controller): the capacity-edit criterion, through real reconciles

Every unit test for the new rule hands DecideSize a ScalingInputs it built
itself, so none of them can fail if `size` passes the wrong value at the
call site. setup.go's comment on newServerGroupReconciler is about exactly
that shape of defect, found in this package before by deleting an assignment
and watching the suite stay green.

Mutation-checked rather than assumed: with PodHash forced empty at the call
site, the image-edit half fails, because an empty desired hash compares
nothing and no changeover ever begins.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 7: Write down what an upgrade changes

An operator upgrade changes, with nobody editing anything, which edits roll a
group. `docs/upgrading.md` is where this repository records that class of fact,
and it already carries a section on an operator upgrade rolling a proxy fleet.

**Files:**
- Modify: `docs/upgrading.md`
- Modify: `docs/superpowers/specs/2026-08-27-cloud-api-design.md` §2.3

- [ ] **Step 1: Add the section to `docs/upgrading.md`**

Add after the existing "An operator upgrade can roll every proxy in the
cluster" section:

```markdown
## An operator upgrade changes which edits roll an ephemeral group

Before 7a, an ephemeral `ServerGroup` treated `metadata.generation` as the
definition of staleness, so *any* field of its spec moving replaced every
server it had. After 7a it compares `podspec.DesiredServerHash`, and only an
edit that changes the rendered pod or the group's config does that.

Nothing has to be done for this, and the direction is the safe one: strictly
fewer changeovers than before, never more. Three things are worth knowing.

**Every ephemeral server that predates the upgrade is adopted, not replaced.**
Servers created before `spec.podHash` had a reader on this side carry an empty
hash, and the first reconcile after the upgrade stamps them with the group's
current one rather than nominating them. So the upgrade itself rolls nothing.
The cost is a bounded one-time window: a spec edit landing inside that same
reconcile is adopted along with the old pod instead of triggering a rebuild.
It closes for good the first time the group reconciles.

**A capacity edit still resets the group's failure streak**, and that is the
one thing 7a did not change. The failure count is filtered by generation, not
by the render hash, because it runs before the hash exists and on a path where
no hash can be computed. `docs/known-issues.md` carries it.

**`status.freeSlots` no longer drops to zero on a capacity edit.** It counted
only servers of the current generation, so before 7a every spec edit briefly
published a healthy group as having no free capacity at all. It follows the
render hash now, and the printcolumn keeps telling the truth across an edit
that changes nothing about the pods.
```

- [ ] **Step 2: Record what 7a left open**

`docs/known-issues.md` holds what is open and nothing else — an entry is
deleted when it closes, so an empty file means nothing is. Add:

```markdown
## A capacity edit still clears a group's failure streak

`ofGeneration` narrows an ephemeral group's views to the current
`metadata.generation` before `CountFailures` runs, so raising `minReplicas`
resets `status.consecutiveFailures` and clears a `Degraded` condition. Scaling
a group up is not a fix for whatever its servers were failing on.

Milestone 7a moved every other staleness comparison onto
`podspec.DesiredServerHash` and deliberately did not move this one. Failure
counting runs unconditionally, before the hash is computed and on purpose: the
hash is gated on the group's Network being usable, and a group whose Network
was deleted is exactly the one that piles failures up. Replacing a value that
is always present with one that is sometimes empty, on that path, is the wrong
trade.

It matters more than it did. Milestone 7c turns a spec edit into a command an
admin types, so `/cloud start lobby` would clear a `CrashLoopBackoff` and start
hammering a broken image from a fresh window. 7c owns the fix and has two
options this milestone did not: refuse a scale while the group is `Degraded`,
or carry the streak across the edit explicitly.
```

- [ ] **Step 3: Correct the spec's own account of the change**

`docs/superpowers/specs/2026-08-27-cloud-api-design.md` §2.3 ends with "This
changes one comparison." That was written before the code was read and it is
wrong. Replace that sentence with:

```markdown
`metadata.generation` keeps every other job it has, including ordering retained
failures for pruning — a digest has no order, so that one cannot move and does
not.

The change reaches further than the sizing rule, and 7a's plan records the full
extent: four comparisons in `scaling.go`, the free-slot total in
`AggregateGroup`, and `adoptServers`, which skipped ephemeral groups outright
while nothing on that side read the field — a hole rather than a rename.

One comparison deliberately does **not** move, and `docs/known-issues.md`
carries it: the failure-count filter runs before the hash is computed and on a
path where no hash exists, so raising `minReplicas` still clears a group's
failure streak. That becomes 7c's problem the moment `/cloud start` exists, and
7c owns it.
```

- [ ] **Step 4: Commit**

```bash
git add docs/upgrading.md docs/known-issues.md docs/superpowers/specs/2026-08-27-cloud-api-design.md
git commit -m "$(cat <<'EOF'
docs(upgrading): what 7a changes about which edits roll a group

An operator upgrade changes, with nobody editing anything, which spec edits
replace an ephemeral group's servers. The direction is the safe one --
strictly fewer changeovers, never more -- but three consequences are worth
knowing before the upgrade rather than after: every pre-existing server is
adopted rather than replaced, status.freeSlots stops dropping to zero on an
edit that changes nothing about the pods, and the failure streak is the one
thing a capacity edit still clears.

That last one is recorded open rather than quietly left. The failure count
runs before the render hash is computed, on a path where no hash exists at
all, so it keeps reading the generation -- and /cloud start inherits the
consequence.

Also corrects this milestone's own spec, which claimed the change was one
comparison. That was written before the code was read.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Done when

- [ ] `make test` passes
- [ ] `make lint` reports nothing
- [ ] `grep -rn 'in\.Generation' internal/controller/` returns nothing
- [ ] `grep -rn 'bumpGeneration' internal/controller/` returns nothing
- [ ] The mutation in Task 6 Step 3 was run, failed as predicted, and was reverted
- [ ] Nothing was pushed and no tag was created

## What 7a deliberately leaves for a cluster

The spec's §9 asks for one thing this plan cannot do on its own: **7a is
observed once in a real cluster before 7b begins.** The check is small — raise
an ephemeral group's `minReplicas` on a running installation and confirm no
server enters `Retiring` — and it belongs to whoever runs the rollout, not to
this plan.
