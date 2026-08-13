# Milestone 4b: rolling updates of ephemeral groups

Status: written 2026-08-13, at the start of milestone 4b, against
`d7aefb0` (end of 4a).

Companion documents: `docs/handover-milestone-4b.md` is the handover this
design answers; `docs/superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`
§4.4 and §6.3 are the requirement; `docs/superpowers/specs/2026-08-13-slot-based-scaling-design.md`
is 4a, whose sizing rule this extends.

## 1. What this closes

An ephemeral `ServerGroup` whose spec changes raises its
`metadata.generation`, and every server created before it is *stale*. Today
nothing acts on that. The group keeps running its old servers forever; the
only way to a new image is to delete servers by hand and let the floor
recreate them, which disconnects whoever is on them.

4b makes the changeover happen by itself, surge-first and without kicks: a
replacement of the new generation comes up, an old server stops taking joins,
its players finish their session in peace, and the server disappears when the
last one leaves.

**Not in 4b:** per-group exponential backoff with the `Degraded` condition
(master design §7). The handover assigned it here on the grounds that it
touches the failure path "which the rolling update touches anyway" — measured,
it does not: 4b lands in `DecideSize`, in the phase machine and in one new
timestamp, and shares no code with `pruneFailed`. It gets its own spec after
this one. §6 records the one place the two meet.

**Not in 4b either:** persistent groups, which use `Recreate` with a drain in
front and belong to milestone 5. The CRD already refuses `spec.update` on
them.

## 2. What is already in place

- **The CRD is complete.** `UpdateSpec` carries `MaxUnavailable` (default 1,
  minimum 1) and `MaxStaleSeconds` (default 0, minimum 0), and a CEL rule
  refuses `spec.update` on persistent groups. Verified against the tree, as
  the handover asked.
- **The generation is on every server.** `Server.spec.groupGeneration` is
  written at creation and read into `ServerView.Generation`. Its only
  consumer today is `AggregateGroup`, which excludes stale servers from
  `status.freeSlots`.
- **`DecideSize`** is the sizing rule: a pure function over `ScalingInputs`,
  table-tested without a cluster. 4b extends it rather than standing up a
  second scaler.
- **`expectations`** reserves the creates and deletes a reconcile has issued
  and the cache has not shown, keyed by name, with a 30-second TTL.
- **Soft drain's primitive exists in two halves.** `Registrar` has
  `Deregister` separate from `Drain`, and `phase.Decision` has `Deregister`
  separate from `StartDrain`. Soft drain is the first without the second.
- **`countsTowardSize()`** already excludes `Draining`, `Terminating` and
  `Failed`, so a server that stops counting frees the scaler to replace it.

## 3. Decisions

### 3.1 Soft drain is a phase of its own, `Retiring`

The state is *deregistered, not being drained, players undisturbed, waiting to
empty on its own*, and unlike `Draining` it must not have
`spec.drain.timeoutSeconds` hanging over it — a lobby can sit in it for hours
legitimately.

Two measurements decided this against the alternatives the handover lists:

**`Server.status.phase` is a plain `string` with no kubebuilder enum marker.**
A new phase value is therefore not a CRD schema change, which removes the cost
the handover attributed to this option.

**`internal/proxyreg/fleet.go` turns phase `Draining` into a `DrainPlayers`
message on every snapshot it sends a proxy.** That is fatal to the
"`Draining` without `StartDrain`" alternative: the server would be in soft
drain and the proxies would move its players off anyway. Repairing it would
mean teaching `fleet.go` a second axis — which is the third alternative's
problem, acquired inside the second. With a phase of its own, `fleet.go`
simply does not match, and soft drain falls out of code that already exists.

The third alternative, a `status.retiring` flag beside the phase, is the shape
this repository has been bitten by: `candidates.go` records at length what two
implementations of one occupancy rule cost when they drifted.

### 3.2 The generation does not enter the capacity arithmetic

This is the design's central departure from both the master design's wording
and the handover's expectation, and it is what makes the feature safe.

The handover calls putting the generation filter into `DecideSize` "4b's
central task" and warns that it is "only safe together with the rules that
brake it". The braking problem disappears entirely if the filter never goes
in. The chain instead is:

1. `leaving()` gains `Retiring`, so a retiring server stops counting toward
   the group's size and stops contributing capacity.
2. The existing spare-slot rule therefore orders a replacement **when and only
   when capacity requires one** — no new code in the scaler.
3. `maxUnavailable` bounds how many may retire at once, so the overhang is
   bounded without a brake having to be built.

**The generation decides only *which* server retires, never *how many* get
built.** A full replacement set — the failure 4a deliberately avoided — is
mechanically impossible, because the capacity arithmetic stays
generation-blind exactly as 4a left it.

**The divergence, stated plainly.** Master design §4.4 step 1 says stale
servers no longer count towards the group's free slots. Here they count until
they retire. The design gives its own reason for step 1: "without it the
update would never terminate, because a lobby that, as a fallback target,
practically never empties would stay registered, and its free slots would
prevent new servers from being created." That purpose is served by §3.3's cold
start plus retirement-driven replacement. `status.freeSlots` keeps its
generation filter unchanged, because that field is an observation and its CRD
documentation says what it means.

#### The one removal decision the generation does enter

Retirement is how a server leaves during a changeover and deletion is how it
leaves for lack of demand. The two never happen in the same pass, but they do
apply to the same group at the same time, and one rule settles what happens
then: **while any stale server remains, the demand rule sheds stale servers
only — a current-generation server is not a demand candidate.**

The rule closes an oscillation. The retirement branch runs before demand, so
while the `maxUnavailable` budget is free a pass that could retire returns
there and demand never runs. When the budget is already spent — the long-lived
case, since a soft drain on an occupied server is exactly what holds it — the
retirement branch declines and the pass falls through to demand with stale
servers still standing. The demand rule then deletes the cold start's own
replacement, and *prefers* it: `SelectDeletionCandidates` sorts youngest-first
among servers that took players, so the fresh server loses to the stale one
beside it on age alone. With no current-generation server left, §3.3's cold
start fires on the next pass and builds another, for as long as the budget
stays spent — up to the whole of `maxStaleSeconds`. Skipping the current
generation makes the last current-generation server uncandidatable, so the
loop cannot start, and it corrects the backwards preference in the same
stroke.

**This does not weaken §3.2, and the reason is the direction the error can run
in.** A generation filter in the capacity arithmetic —
`provisionalCapacity`, `readyContribution`, `readyFree` — makes running
servers stop counting the instant any field of the spec changes, so the group
orders a full replacement set: runaway *creates*, up to `maxReplicas`, which
is the failure that disconnects players. A generation filter in deletion
candidacy can only make *fewer* servers deletable. It can hold a removal back;
it cannot create anything. The numbers stay generation-blind; only candidacy
reads the generation, alongside the retirement nomination that already did.

"Remains" is the same set §3.3's cold start counts as stale: a different
generation, no deletion already reserved, still counting toward the group's
size. A stale server that is `Failed`, `Draining` or `Terminating` is not
something the changeover is still racing to remove, and counting it would
suspend ordinary scale-downs for the whole failed-retention window.

### 3.3 The overhang is fixed at one server

Retirement needs a ready server of the current generation to exist first. When
every server is stale, none does, so nothing may retire, so no replacement is
ordered — a deadlock.

**The cold start breaks it: if a stale server exists and no server of the
current generation counts toward the group's size, exactly one is created,
regardless of free slots.** Once it is `Ready`, §3.2's chain carries the rest.

Two things bound it to the word "exactly". A create this reconciler has
already issued counts as a server of the current generation — `PendingCreates`
is always current-generation by construction — so the cold start fires once
and not once per pass while its server boots. And it does not fire at all
under §3.7's condition.

**A group at its ceiling cannot start a changeover, and says so.** The cold
start is a create like any other and the ceiling clamps it, so a group whose
`maxReplicas` equals its current size stalls with its old generation serving.
That is the right outcome — a lowered ceiling is an instruction, not a
suggestion — but it must not be silent, so `DecideSize` sets `Limited` in that
case and the existing `ScalingLimited` condition carries it onto the group.
Raising `maxReplicas` by one is the operator's way out.

One is enough because the replacement is not one-for-one: a retiring server's
players stay where they are, and the group only rebuilds capacity the
spare-slot rule actually asks for. The update therefore costs at most one
extra server at any moment, which is why `maxUnavailable` needs no companion
`maxSurge` and no second meaning.

### 3.4 The "one ready server of the new generation" rule applies to every group

Master design §4.4 step 2 makes it "mandatory for fallback groups". A
`ServerGroup` cannot tell whether it is a fallback target: that lives in the
`ProxyGroup`s' `spec.fallbackGroups`, which this controller does not read.

Applying the rule to every group needs no cross-controller read, no watch and
no cache that can be wrong. For a group that is not a fallback target it is at
worst mildly conservative, and conservative here means *replacement first,
teardown second* — which is what one wants anyway. Where it bites is a group
whose new generation can never become ready: the update stalls, the old
generation keeps serving, and `maxStaleSeconds` is the configured way out.
Stalling in that state is the correct outcome, not a defect.

### 3.5 `maxStaleSeconds` runs from entry into `Retiring`

`ServerStatus` already carries four phase-entry timestamps — `StartedAt`,
`ReadySince`, `DrainStartedAt`, `FailedAt` — and each drives exactly one
deadline. `RetiringSince` is the fifth instance of that pattern, and
`DrainStartedAt` driving the drain deadline is its exact precedent.

The handover assumed the clock had to run from the generation change and
observed that nothing records such a timestamp. It does not have to: the
design's own wording is "forces the active drain once it expires, **if a
server does not empty on its own**", which is about the wait in soft drain,
not about time spent stale. A server still queued behind `maxUnavailable` is
not failing to empty; it has not been asked yet.

The consequence, stated so nobody meets it as a surprise: with
`maxUnavailable: 1` the whole changeover can take up to *n* ×
`maxStaleSeconds`, because the clock only ever runs for the server currently
retiring.

### 3.6 The retirement instruction is a spec field on the `Server`

The group decides — only it knows the generation, the budget, and whether a
ready replacement exists — and the `Server` controller executes. The channel
is `Server.spec.retire`, read into `phase.Inputs.RetirementRequested`,
mirroring how `DeletionRequested` reaches the same function.

It is a spec field rather than an annotation because this repository keeps its
API typed, and `spec.groupGeneration` is the precedent: a field on the child
that only the operator ever writes.

**It is on the CR rather than in memory, and that is the opposite of 4a's
choice for `EmptyFor`, deliberately.** 4a put the empty-since clock in memory
because a restart resets it, delays a scale-down and errs safely. Here a
restart would restart the whole changeover and re-arm every deadline, so
durability is what errs safely.

### 3.7 The cold start is suppressed while a failure of the current generation is retained

A broken image is the most likely thing to go wrong in an update, and it is
the case 4b makes most reachable. Without a guard: the cold-start server
fails, `countsTowardSize` excludes `Failed`, so the cold start fires again on
the next five-second pass, forever.

**That loop is not new.** The floor rule does the same thing today, and
`maxRetainedFailures`'s own comment describes it — "the group creates a
replacement on the next five-second pass" — while capping only the resulting
footprint, not the rate. Its owner is the backoff spec, not this one.

But 4b opens a second door into it, so 4b closes its own door: **the cold
start does not fire while a `Failed` server of the current generation is being
retained.** One condition, using the existing `failedRetentionSeconds` window
(an hour by default) as its interval. The effect is one attempt per window,
the old generation serving undisturbed throughout, and one corpse left
standing to diagnose from. This is deliberately not backoff — it does nothing
for the floor rule's loop, and says so.

### 3.8 `maxUnavailable` counts what this update made unavailable

**`spec.retire` is the single signal, and it is the whole rule.** A server
counts against the budget while its `spec.retire` is true, plus any retirement
this pass has reserved and the cache has not shown. It stays true across the
escalation to `Draining`, so a forced drain keeps occupying the budget it
started in; a drain that a scale-down or a deletion started never had it, and
does not count.

The master design says "because of a generation change", which asks for
exactly this distinction, and one durable field on the object answers it.

**Deliberately not a second signal.** An earlier draft of this design counted
`status.retiringSince` together with the phase, alongside `spec.retire` — two
axes for one fact, which is the shape `candidates.go` records having drifted
once already. `retiringSince` is the deadline clock and nothing else; the
group controller never reads it.

## 4. Components

### 4.1 `api/v1alpha1`

Two fields. Both are real CRD changes, so `make manifests` will produce a diff
— limited to these two.

```go
// ServerSpec
// Retire asks this server to stop taking joins and empty out. Set by the
// ServerGroup controller during a rolling update; never set by a user.
// +optional
Retire bool `json:"retire,omitempty"`

// ServerStatus
// RetiringSince is when the server entered phase Retiring. It drives
// spec.update.maxStaleSeconds and nothing else; what marks a server as one
// this update made unavailable is spec.retire.
// +optional
RetiringSince *metav1.Time `json:"retiringSince,omitempty"`
```

### 4.2 `internal/phase`

- `Retiring Phase = "Retiring"`.
- `Inputs.RetirementRequested bool` and `Inputs.MaxStaleReached bool`.
- Reasons `ReasonRetiring` and `ReasonMaxStaleElapsed`.
- **Entry, from `Ready` only:** `RetirementRequested` yields
  `{Next: Retiring, Deregister: true}`. A stale server in `Starting` is not
  retired — it either reaches `Ready` and is asked then, or it fails. Keeping
  the entry narrow keeps the phase's meaning single.
- **`case Retiring:`** mirrors `Draining` with two differences: no
  `StartDrain`, and the deadline is `MaxStaleReached` rather than
  `DrainDeadlineReached`.
  - `PodLost` or `PodTerminal` → `Terminating`; the sessions are gone.
  - `DeletionRequested` → `Draining` **with** `StartDrain`. A retiring server
    has players, and whoever deletes it gets the proper move.
  - `!Occupied()` → `Terminating`, reason `ReasonDrained`.
  - `MaxStaleReached` → `Draining` with `StartDrain`, reason
    `ReasonMaxStaleElapsed`. This is the active drain of §6.2.
  - Otherwise stay.

### 4.3 `internal/controller/candidates.go`

- `leaving()` gains `Retiring`. Everything downstream follows without further
  edits: `countsTowardSize()` goes false, `provisionalCapacity` returns 0,
  `readyContribution` was already 0 for a non-`Ready` phase, and
  `SelectDeletionCandidates` never nominates it.
- `ServerView` gains one field, `Retire bool`, read from `spec.retire`. It is
  the budget signal of §3.8. The status timestamp is not mirrored here: the
  group controller has no rule that reads it.
- **`occupiedPods()` does not change.** `Occupied()` is player-based, not
  phase-based, so a retiring server with players stays inside the
  `PodDisruptionBudget`. That is correct and is asserted by its own test,
  because nothing else in the tree would catch it changing.

### 4.4 `internal/controller/scaling.go`

`ScalingInputs` gains `Generation int64` and `MaxUnavailable int32`.
`SizeDecision` gains `Retire []string`.

The order inside `DecideSize` becomes **capacity → ceiling → retirement →
demand**, and a pass that nominates a retirement returns there. Two removals
decided in one pass would be two decisions taken on two readings of the same
moment, which 4a already ruled out for creates and deletes.

Retirement cannot reuse `SelectDeletionCandidates`: that function excludes
servers that may be carrying players, and those are exactly the ones that
retire. The nomination is its own, and it is narrower rather than wider —
`Ready`, stale, not already retiring. Order: empty servers first, then the
oldest, ties broken by name. One per pass.

The demand rule gains the one generation test §3.2 permits: while any stale
server remains it skips current-generation servers, so a pass that falls
through a declined retirement sheds stale capacity rather than the
replacement.

`provisionalCapacity` also takes 4a's leftover one-liner: `SessionsGone` is
tested before `Slots == 0`, so a server whose pod has vanished stops being
credited a full `maxPlayers` it does not have. The handover assigns this here
because 4b is in this file anyway; testing `Stale` instead would be a
regression, and 4a's notes record the mutation that proves it.

### 4.5 `internal/controller/expectations.go`

`pendingRetires`, in the same shape as creates and deletes. Without it a
second server can be nominated while the first has not appeared in the cache,
and the budget is exceeded by one. Rare, but the standard here is not rarity.

### 4.6 `internal/controller/servergroup_controller.go`

- `collectViews` reads `spec.retire` into the view.
- `size()` passes `group.Generation` and the update knobs, and executes
  `decision.Retire` by patching `spec.retire = true`, one event per server,
  one expectation reserved.

### 4.7 `internal/controller/server_controller.go`

- Builds `RetirementRequested` from `spec.retire` and `MaxStaleReached` from
  `retiringSince` plus the group's `spec.update.maxStaleSeconds`. The
  controller already reads its group and already has a `fallbackGroup`
  stand-in for when it is gone; that stand-in gains the update defaults.
- Sets `status.retiringSince` on entry to `Retiring`, in the same timestamp
  switch that already handles `Draining` and `Failed`.

### 4.8 `internal/proxyreg/fleet.go`

**No change.** Deregistration removes the server from the `FullSync`, which
stops new joins, and `Retiring` does not match the `Draining` test, so no
`DrainPlayers` goes out. This is the whole of soft drain on the wire, and it
is already written.

## 5. Data flow

A `lobby`: ephemeral, `maxPlayers 100`, `spareSlots 40`, `minReplicas 1`,
`maxUnavailable 1`, `maxStaleSeconds 0`. Two `Ready` servers `A` and `B` of
generation 3, sixty players each. Someone edits `spec.image`; the generation
becomes 4.

| Pass | State | Decision |
|---|---|---|
| 1 | stale `{A,B}`, nothing of generation 4 | **cold start:** create `C` |
| 2…k | `C` starting | nothing — `A` and `B` still contribute capacity, `C` is credited in full |
| k | `C` is `Ready` | **retire `A`:** `spec.retire=true`, event, reservation |
| k+1 | `A` is `Retiring`, deregistered | `A` leaves the size count and the capacity sum. Free: `B` 40 + `C` 100 = 140 ≥ 40, so **no replacement is ordered** |
| … | `A` empties | → `Terminating`, pod deleted, budget free again |
| … | stale `{B}` | `B` retires the same way |
| end | `C` alone, generation 4 | converged |

`A`'s sixty players stay on `A` until they leave of their own accord. New
joins go to `B` and `C`, because `A` is deregistered. Only a configured
`maxStaleSeconds` changes that, by moving `A` to `Draining`, at which point
`fleet.go` sends `DrainPlayers` and the players are moved onto the fallback
groups. The difference between "undisturbed" and "moved" is exactly one
configured value, and at the default it never happens.

A generation raised again mid-update needs no special handling: `C` becomes
stale in its turn and the same rules run from the top, cold start included.

## 6. Error handling

- **A broken new image.** Covered by §3.7: one cold-start attempt per
  `failedRetentionSeconds` window, the old generation serving throughout, one
  `Failed` server kept to diagnose from. The wider create loop this sits
  inside belongs to the backoff spec.
- **The update stalls because no replacement becomes ready.** The correct
  outcome (§3.4). Nothing retires, nobody is disconnected, and the group keeps
  serving its old generation until either the image is fixed or
  `maxStaleSeconds` forces the issue.
- **A server is deleted while retiring** → `Draining` with `StartDrain`, so
  its players are moved rather than dropped.
- **Its pod dies or goes terminal while retiring** → `Terminating`, exactly as
  from `Draining`.
- **The operator restarts mid-update.** Nothing is lost: `spec.retire` and
  `status.retiringSince` are on the CRs (§3.6).
- **`maxStaleSeconds` expires with no fallback available.** The escalation
  enters the existing drain path and inherits its behaviour unchanged; 4b adds
  nothing of its own here.

## 7. What 4b deliberately does not do

- **It does not unify the three capacity figures.** `AggregateGroup`'s
  `FreeSlots`, `provisionalCapacity`'s sum and `readyFree` stay three numbers
  with three purposes. 4b changes none of their filters — the handover
  anticipated that it would change the first one's; §3.2 is why it does not.
- **It does not weaken `SelectDeletionCandidates`.** Retirement is a separate
  nomination with a narrower filter, not a loosening of the existing one.
- **It does not answer "is this group finished changing over?"** `derivePhase`
  measures readiness against `DesiredReplicas()`, which since 4a is only the
  group's floor, so the group phase does not answer the question a rolling
  update makes people ask. Answering it means a new status field or condition;
  `kubectl get servers` answers it today. Carried forward as an open point
  instead of bolted on here.
- **It does not touch the agents, the images or the proto.** 4b is
  operator-only Go, as 4a was; `git diff --name-only` is the check.

## 8. Facts this design asserts about the code already here

Each was read in the tree at `d7aefb0` rather than remembered:

- `Server.status.phase` is `string` with no enum marker, so a new phase value
  is not a schema change.
- `internal/proxyreg/fleet.go` tests `srv.Status.Phase == string(phase.Draining)`
  and builds one `DrainPlayers` per match; nothing else reads a phase there.
- `internal/controller/server_controller.go` fetches the owning `ServerGroup`
  and falls back to a synthetic one carrying the CRD defaults when it is gone.
- Its status-timestamp switch already sets `DrainStartedAt` and `FailedAt` on
  entry to their phases; `RetiringSince` joins them.
- `candidates.go`'s `leaving()` is `Draining || Terminating` and is the only
  gate `countsTowardSize()` consults besides `Failed`.
- `mayHavePlayers()` is player-based, not phase-based, so it keeps protecting
  a retiring server without modification.
- `UpdateSpec` exists with both fields, their defaults and their validation,
  and a CEL rule refuses it on persistent groups.
- `maxRetainedFailures = 1` caps retained failures, not the rate at which they
  are produced, and its comment says so.

## 9. Test strategy

The rules are pure functions, so nearly all of this is table-tested without a
cluster, in the shape `phase.Decide` and `DecideSize` already use.

- **`internal/phase`** is at 100% and stays there: the `Retiring` case and the
  `Ready` entry join the existing table, including every exit
  (`PodLost`, `PodTerminal`, `DeletionRequested`, emptied, `MaxStaleReached`).
- **`scaling_test.go`**: the cold start with and without a retained `Failed`
  server of the current generation; nomination of a server that has players;
  the capacity → ceiling → retirement → demand order; and the budget counted
  against a concurrent scale-down drain that must *not* count.
- **`candidates_test.go`**: `leaving()` includes `Retiring`;
  `SelectDeletionCandidates` never nominates one; `occupiedPods()` still
  counts one that has players.
- **`expectations_test.go`**: `pendingRetires`, observed and expired.
- **envtest**: a real generation bump on an occupied group, run to
  convergence.

**Any test whose expectations move gets its mutation made for real and the
output reported.** 4a's reviews found eleven assertions that could not fail,
the worst of them a test that had silently stopped testing; "the test stopped
failing" and "the test stopped testing" look identical from outside.

## 10. Acceptance criteria

1. **A server that may be carrying players is never nominated for deletion.**
   Mutation-tested, not asserted.
2. A generation change on an occupied group disconnects nobody while
   `maxStaleSeconds` is 0.
3. The group converges on servers of the current generation only.
4. At most `maxUnavailable` servers are unavailable because of the update at
   any moment.
5. The group never exceeds its demand-driven size by more than one server.
6. `make manifests` produces a diff limited to the two new fields, and
   `git diff --name-only` touches no agent, image or proto file.
7. Coverage stays at or above 88% for `internal/controller` and 100% for
   `internal/phase`.

## 11. What 4b leaves open

- **Per-group exponential backoff and the `Degraded` condition** (master
  design §7), with `maxRetainedFailures = 1` still standing in. Its own spec,
  next.
- **`derivePhase` against `DesiredReplicas()`** (§7): the group phase does not
  say whether a changeover has finished.
- **The drain's blind spot from milestone 3's evidence run**: a player
  connected at the proxy but not yet counted by the backend sits outside
  `Occupied()`. Unchanged by 4b, still milestone 4's to decide.
- **Proxy drain and node drain**, with the lowerable readiness
  `internal/agent/registry.go` cannot express — milestone 4c, and
  `docs/handover-milestone-4.md` remains its entry point.
