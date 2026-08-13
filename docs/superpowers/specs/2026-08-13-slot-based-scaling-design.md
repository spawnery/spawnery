# Milestone 4a: slot-based scaling

Status: design, 2026-08-13. The first of three sub-projects of milestone 4
(*slot-based scaling, player-aware drain, PDB protection, rolling updates of
ephemeral groups*), cut as:

| | Contents |
|---|---|
| **4a** | Slot-based scaling of ephemeral `ServerGroup`s — this document |
| **4b** | Rolling updates of ephemeral groups: stale generations, soft drain, `maxUnavailable`, `maxStaleSeconds`, per-group backoff |
| **4c** | Proxy drain and node drain: lowerable readiness, `ProxyGroup` scale-down without kicking, `unschedulable` nodes |

4a touches the operator only. No proto change, no agent change, no image
change.

## 1. What this closes

Milestone 1 sizes an ephemeral group at its floor and nothing more:
`ServerGroupReconciler.size` asks `group.DesiredReplicas()`, which returns
`spec.scaling.minReplicas`. Everything else the scaler needs already exists
and is read by nobody. `AggregateGroup` computes `FreeSlots` — generation-aware,
staleness-aware — and publishes it to `status.freeSlots` as an observation.
`spec.scaling.spareSlots` and `spec.scaling.scaleDownStabilizationSeconds` are
in the CRD, validated, and never consulted. `SelectDeletionCandidates` carries
the full "never nominate a server that may be carrying players" invariant and
is called only to trim a group back to its floor.

4a is the decision that reads them: create servers when free capacity falls
below `spareSlots`, remove them when it does not, and say so on the group when
the ceiling stops it.

Three gaps around that decision close in the same change, because the scaler
is what makes each of them matter:

- **`slots` is bounded by nothing.** `Registry.ReportPlayers` rejects
  `players > slots` but checks `slots` against no upper bound. Today `slots`
  only reaches `status.slots` and is cosmetic. From 4a it feeds the scaling
  decision for the whole group, so one pod reporting `slots: 1000000` makes
  its group look permanently spacious and suppresses every scale-up for all
  of its servers — an effect across pod boundaries that milestone 2a
  otherwise rules out.
- **There is no "empty since".** `scaleDownStabilizationSeconds` needs a
  clock reading that exists nowhere today.
- **`collectViews` reads a cache with no reservation** for a create the same
  reconcile just issued. Holding a floor hits that rarely; a scaler that
  creates servers in response to player counts hits it as a matter of course.

## 2. What is already in place

- `ServerView` (`internal/controller/candidates.go`) — the value type the
  group logic reasons over, carrying phase, players, slots, staleness,
  `wasRegistered`, `sessionsGone`, generation and creation time.
- `isOccupied` — the single occupancy rule, shared by the PDB's two sides.
- `SelectDeletionCandidates` — never nominates a server that may carry
  players, prefers servers that never took players, then the youngest.
- `AggregateGroup` — `Replicas`, `ReadyReplicas`, `OnlinePlayers`,
  `FreeSlots`.
- `agent.Registry` — in-memory player counts with an injectable clock, and
  `Snapshot.PlayersStale` at twice the report interval.
- `ServerGroupReconciler` — already carries a `Recorder` and an injectable
  `Clock`, and requeues every `resyncInterval` (5 s).
- The CRD — `ScalingSpec` complete, `minReplicas <= maxReplicas` enforced by
  CEL, `scaleDownStabilizationSeconds` defaulted to 300.

## 3. Decisions

**The rules are a pure function, the controller is its executor.** This
repository already puts its three hardest decisions in pure functions over
value types — `phase.Decide`, `SelectDeletionCandidates`, `AggregateGroup` —
and table-tests them without a cluster. `DecideSize` joins them. Writing the
rules inline in `size()` would save a file and cost every one of these rules
its test.

**The scale-up decision does not read `status.freeSlots`.** It reads a second
figure, *provisional* free slots, and the difference is the whole correctness
of the feature; §4.2 states it. `status.freeSlots` keeps the meaning its CRD
field documents — ready servers of the current generation — because 4b's
rolling update depends on exactly that meaning. **Two numbers, two purposes.**
They must not be unified later.

**4a is generation-blind, and that is not an oversight.** Every edit to a
`ServerGroup` spec raises its `generation`. A scale-up rule that counted only
servers of the current generation would find, the instant any field changed,
that no running server contributes anything — and would order a full
replacement set up to `maxReplicas` on the next five-second pass. That is a
rolling update without `maxUnavailable`, without soft drain and without the
"at least one ready server of the new generation" guarantee: 4b's work, done
early and unbraked. So 4a's own two figures ignore the generation entirely,
and the generation filter arrives in 4b together with the rules that make it
safe. `AggregateGroup` keeps its filter unchanged, because `status.freeSlots`
is an observation today and 4b's rolling update is what will read it.

**The empty-since timestamp lives in memory, in the registry.** Design §6.4
already puts the scaling inputs there: "the operator keeps them in memory —
that is where the scaling logic makes its decisions." An operator restart
resets the timers and delays scale-down by up to one window; the error points
in the safe direction. The alternative, a `Server.status` field, would survive
a restart but make scale-down depend on a successful status write and create a
second source of a truth the memory already holds — the shape that produced
the `isOccupied` drift documented at `candidates.go:55`.

**The cache-lag reservation is name-based expectations in memory**, the shape
the `ReplicaSet` controller uses, keyed by name rather than by count so that
observing one is a set membership test and needs no ordering. Deterministic
ordinal names would make a duplicate create fail on its own, but the name
scheme reaches pods, labels and a large body of existing tests.

**A group pinned at its ceiling gets its own condition**, `ScalingLimited`,
not `Degraded`. A popular group sitting on `maxReplicas` works exactly as
configured; folding it into `Degraded` would move the group's phase through
`derivePhase` and make a real fault during peak load indistinguishable from
peak load. Conditions are a list, so this is not a schema change, and 4c can
reuse the type for the proxy side.

## 4. Components

### 4.1 `internal/agent` — emptiness

`entry` gains `emptySince time.Time`, maintained only by `ReportPlayers`:

- `players == 0` and `emptySince` unset → set it to `now()`.
- `players > 0` → clear it.

`Snapshot` gains `EmptyFor time.Duration`: `now - emptySince` when set, zero
otherwise. Three edges, decided here so nobody has to guess:

- **`Connect` clears it, `Supersede` keeps it.** The same reasoning that
  already governs `keepReady`: a fresh stream may have a restarted process
  behind it, a superseding one cannot. The first report re-establishes it,
  which at worst delays a scale-down.
- **`Disconnect` keeps it**, and that is inert: the count goes stale within
  twice the report interval, stale counts as occupied, and an occupied server
  is never a deletion candidate. Keeping is simpler than clearing and changes
  no decision.
- **`EmptyFor` never decides anything alone.** Every rule that reads it also
  asks `players == 0 && !stale`. A server that was never empty reports
  `EmptyFor == 0`, and `scaleDownStabilizationSeconds: 0` — which the CRD
  permits — would make a duration-only test true for every server in the
  group.

### 4.2 `internal/controller/scaling.go` — `DecideSize`

```go
type ScalingInputs struct {
	Views          []ServerView
	MinReplicas    int32
	MaxReplicas    int32
	SpareSlots     int32
	MaxPlayers     int32
	Stabilization  time.Duration
	PendingCreates int32
	PendingDeletes map[string]bool
}

type SizeDecision struct {
	Create  int32    // how many servers to create now
	Delete  []string // which servers to remove now
	Wanted  int32    // creates the slot rule asked for, before the ceiling
	Limited bool     // Wanted > Create: the ceiling is holding capacity back
}
```

`ServerView` gains one field, `EmptyFor time.Duration`.

**Provisional free slots.** A server created now is not `Ready` for tens of
seconds and contributes nothing to `AggregateGroup`'s `FreeSlots`. At a
five-second resync the scaler would see the same shortfall six to twelve times
and order the same replacement each time, until `maxReplicas` stopped it. That
is not an edge case; it is what every scale-up would do.

The scale-up rule therefore sums, over each server that `countsTowardSize()`
and is not pending deletion — whatever its generation, for the reason §3
gives:

| State | Contribution | Why |
|---|---|---|
| never reported (`Slots == 0`) | `MaxPlayers` | the capacity is ordered, it has not arrived yet |
| reported, count stale | `0` | unknown, and unknown counts as occupied throughout this repository |
| reported, count fresh | `max(0, Slots - Players)` | the same formula `AggregateGroup` uses |

plus `PendingCreates * MaxPlayers`, for creates the cache has not shown yet.
`Slots == 0` is what separates a server still starting up from one whose agent
went quiet: the first has never reported, the second has.

**The rules, in order.**

1. `alive` = servers that `countsTowardSize()` and are not pending deletion,
   plus `PendingCreates`.
2. `wanted = ceil((SpareSlots - provisional) / MaxPlayers)` when provisional is
   short, else 0.
3. `create = max(MinReplicas - alive, wanted)`. If `create > 0`, grant
   `min(create, max(0, MaxReplicas - alive))`, set `Limited = wanted > granted`,
   and **return** — a group that is short of capacity does not also delete.
   `Limited` is therefore reported even when the grant is zero.
4. Otherwise, if `alive > MaxReplicas`: delete the surplus through
   `SelectDeletionCandidates`, **without** the stabilization window. A lowered
   ceiling is an instruction, not a suggestion, and the selection already
   refuses any server that may carry players.
5. Otherwise, if `alive > MinReplicas`: delete **one** server, chosen by
   `SelectDeletionCandidates` from the candidates that additionally satisfy
   `Players == 0 && !Stale && EmptyFor >= Stabilization` and
   `readyFree - contribution(v) >= SpareSlots`. `readyFree` is the sum of
   `max(0, Slots - Players)` over the servers that are `Ready` with a fresh
   count — the arrived capacity, not the provisional figure, because
   scale-down must not count capacity that has not turned up yet.
   `contribution(v)` is that same expression for the one candidate, and zero
   for a server that is not `Ready` or not fresh, so removing a server that
   contributes nothing is tested against an unchanged total. Both are computed
   in `scaling.go` rather than taken from `AggregateGroup`, which filters by
   generation and would freeze every scale-down after a spec edit. Each
   candidate is tested independently, so an infeasible head does not hide a
   feasible tail. One per pass: every deletion
   costs a drain cycle, and a five-second resync converges quickly enough.

`MaxPlayers <= 0` cannot reach step 2 — the CRD requires it — but the division
is guarded anyway and yields `wanted = 0`.

### 4.3 `internal/controller/expectations.go`

A per-group map of names with deadlines, one entry per create and per delete
the reconciler has issued and the cache has not yet reflected.

```go
func (e *expectations) expectCreate(group, name string)
func (e *expectations) expectDelete(group, name string)
func (e *expectations) observe(group string, views []ServerView) // drops satisfied entries
func (e *expectations) pending(group string) (creates int32, deletes map[string]bool)
func (e *expectations) forget(group string)
```

An entry is dropped when the cache shows it (create), when the cache stops
showing it or shows it leaving (delete), or after 30 seconds regardless — so a
lost watch event delays the group rather than blinding it permanently. The
clock is the reconciler's existing `Clock`, so the expiry is testable without
sleeping. `forget` runs on the group's deletion path, where `Reconcile` already
returns early.

**Expectations stay out of the `ServerView` list.** Synthesising a placeholder
view for an unobserved create would be nominated for deletion by
`SelectDeletionCandidates` — which sorts never-registered servers *first* — and
would be counted by `AggregateGroup` and by the PDB, both of which read the
same slice. They are separate inputs to `DecideSize` instead, and `views`
keeps meaning what the cache actually shows.

### 4.4 Controller wiring

- `ServerGroupReconciler` gains an `Expectations *expectations` field,
  constructed alongside the reconciler in `internal/controller/setup.go`.
- `collectViews` clamps: `Slots` to `group.Spec.MaxPlayers`, then `Players` to
  the clamped `Slots`, with a `V(1)` log line when a clamp fires. This is the
  one place where the registry's number and the group's bound meet — the
  registry does not know a pod's group. It fills `EmptyFor` from the snapshot
  in the same loop.
- `size()` calls `observe`, then `pending`, then `DecideSize`, executes the
  decision through the existing `createServer` and `deleteServer`, and records
  each issued name back into the expectations. `deleteServer` already no-ops on
  a `Server` that carries a deletion timestamp; the expectation covers the
  window before the cache shows that timestamp.
- `ScalingLimited` is set or cleared on every pass with reason
  `MaxReplicasReached`, its message naming the wanted and granted counts. The
  event goes with the transition only: `meta.SetStatusCondition` moves
  `lastTransitionTime` on the edge, and the reconciler compares before and
  after to decide whether to emit.
- `derivePhase` is not touched.
- `DesiredReplicas()` loses its "slot-based scaling on top of this arrives in
  milestone 4" comment and becomes what it then is: the floor, one input among
  several.

### 4.5 API surface

`api/v1alpha1/common_types.go` gains `ConditionScalingLimited = "ScalingLimited"`
and `ReasonMaxReplicasReached`. No field is added to any CRD, so
`make manifests` produces no schema change; the CRD YAML changes not at all.

## 5. Data flows

**A group fills up.** Agents report; `ReportPlayers` clears `emptySince` on the
servers that gained players. The next reconcile builds views, clamps them, and
`DecideSize` computes provisional free slots below `spareSlots`. It orders
`ceil(gap / maxPlayers)` servers, bounded by the ceiling, and records them as
expected creates. The following reconcile — five seconds later, cache possibly
still behind — counts those creates as alive and credits them `maxPlayers`
each, finds no shortfall, and orders nothing. Thirty seconds later the new
servers reach `Ready`, their real counts replace the provisional credit, and
the expectations have long since been observed away.

**A group empties.** Counts fall to zero; `ReportPlayers` stamps `emptySince`
on each server as it empties. Five minutes later `EmptyFor` passes the window.
`DecideSize` finds no shortfall, `alive > minReplicas`, and one candidate whose
removal still leaves `freeSlots >= spareSlots`. It names one. The Server
controller drains it — instantly, since it holds nobody — and the next pass
considers the next one.

**The ceiling holds.** Provisional free slots stay below `spareSlots` with
`alive == maxReplicas`. `DecideSize` returns `Create: 0`, `Wanted: n`,
`Limited: true`. The group publishes `ScalingLimited=True` with the numbers in
its message and emits one event. When capacity returns, the condition goes
false and the group is quiet again.

## 6. Error handling

- **A compromised or broken agent over-reporting `slots`** is clamped in
  `collectViews` to the group's own `maxPlayers`. The purpose is that the
  decision cannot be poisoned across pod boundaries, not that this is an alert
  path; the `V(1)` line is diagnostic.
- **A stale count** contributes zero provisional capacity and makes its server
  ineligible for deletion — the conservative answer on both sides.
- **A lost watch event** leaves an expectation to expire after 30 seconds; the
  group then decides on what the cache shows, which is by then correct.
- **An operator restart** loses every `emptySince` and every expectation.
  Scale-down waits one more window; scale-up briefly loses its reservation and
  may create one server too many in the first seconds. Both errors point away
  from kicking players.
- **A `Server` create that fails** returns the error from `size()` as today
  and no expectation is recorded, so the next pass tries again.
- **Fewer free candidates than the surplus** already logs and retries; that
  path is unchanged.

## 7. What 4a deliberately does not do

- **Rolling updates, and with them any notice of the generation at all.**
  Stale generations, soft drain, `maxUnavailable`, `maxStaleSeconds` — 4b.
  `AggregateGroup` already excludes stale generations from `FreeSlots`, which
  is the hook 4b needs; 4a's own rules do not read it, because a
  generation-aware scale-up without the rest of the rolling-update rules
  surges a full replacement set on every spec edit (§3).
- **Per-group exponential backoff and a `Degraded` condition on repeated
  failures.** Listed in `docs/known-issues.md` as a milestone 4 precondition;
  it touches `pruneFailed` and the failure path, which 4b touches anyway.
- **`isOccupied` and terminating pods.** Needs a real cluster, not envtest.
- **Orphaned `Server`s without a pod.** The orphan sweep, not the scaler.
- **Anything on the proxy side.** `ProxyGroup.spec.replicas` stays a fixed
  number; 4c moves it.
- **Scaling persistent groups.** Milestone 5.

## 8. Facts this design asserts about the code already here

Every claim below was read out of the tree at `7f8fd33`, not remembered.

| Claim | Where |
|---|---|
| `size()` sizes only to the floor | `servergroup_controller.go:231` — `desired := group.DesiredReplicas()` |
| `DesiredReplicas()` returns `MinReplicas` for ephemeral groups | `servergroup_types.go:264` |
| `AggregateGroup` excludes stale counts and other generations from `FreeSlots` | `candidates.go:272` |
| `SelectDeletionCandidates` never nominates a server that may carry players, and sorts never-registered first, then youngest | `candidates.go:153`, `candidates.go:159` |
| `ReportPlayers` rejects `players > slots` and bounds `slots` against nothing | `registry.go:162` |
| A count is stale at twice the report interval | `registry.go:228` |
| `connect(keepReady)` already distinguishes `Connect` from `Supersede` | `registry.go:122` |
| `collectViews` lists `Server`s through the manager's cached client | `servergroup_controller.go:280` |
| `deleteServer` no-ops on a `Server` that already carries a deletion timestamp | `servergroup_controller.go:379` |
| The reconciler carries a `Recorder` and an injectable `Clock` | `servergroup_controller.go:68` |
| The requeue is five seconds | `server_controller.go:69` |
| `derivePhase` reads `Degraded` and would move the phase | `servergroup_controller.go:264` |
| Three condition types exist: `Accepted`, `Ready`, `Degraded` | `common_types.go:23` |
| `minReplicas <= maxReplicas` is enforced by CEL | `servergroup_types.go:108` |
| `scaleDownStabilizationSeconds` defaults to 300 | `servergroup_types.go:54` |

## 9. Test strategy

**Table tests, no cluster** — `scaling_test.go`, `expectations_test.go`,
`registry_test.go`:

- Provisional capacity per state: never reported, stale, fresh, other
  generation, leaving, `Failed`.
- `wanted` rounding: a gap of one slot orders one server, a gap of exactly
  `maxPlayers` orders one, `maxPlayers + 1` orders two.
- The ceiling: `Limited` true with `Create: 0` when `alive == maxReplicas`,
  false as soon as the gap closes.
- The floor beats the slot rule and vice versa, whichever is larger.
- A lowered ceiling deletes without waiting for the window; a group above its
  floor waits for it.
- Feasibility per candidate: an infeasible first candidate does not hide a
  feasible second.
- Expectations: satisfied by observation, expired by the clock, unobserved and
  therefore counted.
- `emptySince` on both flanks, and across `Connect`, `Supersede` and
  `Disconnect`.

**envtest — `servergroup_controller_test.go`.** One test carries the
milestone's whole claim:

> A group with `minReplicas: 1`, `maxReplicas: 5`, `spareSlots: 40`,
> `maxPlayers: 100`. Its one server reports 70 players — 30 free slots.
> **Exactly one** server is created, and over the next ten reconciles no
> further one is, although it never becomes `Ready`. Then the count falls to
> zero, the window elapses, and the group returns to its floor.

The second sentence is the test. An assertion on a single decision cannot see
the runaway; only one that ticks the clock across several reconciles can. This
is milestone 3c's lesson restated in the terms of this milestone: what is
asserted has to be what could actually break, measured where it would break.

A second envtest covers `ScalingLimited`: the condition appears when the
ceiling holds, its message names the numbers, and it clears when capacity
returns.

## 10. Acceptance criteria

1. `make test` green.
2. A group short of `spareSlots` creates servers, bounded by `maxReplicas`.
3. A scale-up creates the shortfall **once** and not once per reconcile while
   the new servers start.
4. A group above its floor with a server empty for longer than
   `scaleDownStabilizationSeconds` removes exactly one server per pass, and
   only while `freeSlots` would still cover `spareSlots`.
5. A server that may be carrying players is never nominated — the invariant
   `SelectDeletionCandidates` already holds, still held with the new callers.
6. A lowered `maxReplicas` shrinks the group without waiting for the window.
7. An agent reporting `slots` above the group's `maxPlayers` cannot influence
   the group's scaling beyond `maxPlayers`.
8. `ScalingLimited` is true exactly while the ceiling holds capacity back, and
   the group's phase does not change because of it.
9. A create the cache has not yet shown is not ordered a second time.

## 11. What 4a leaves open

- **`status.freeSlots` and the scaler's two figures are not the same number,
  and differ in two ways.** `AggregateGroup` filters by generation and counts
  only `Ready` servers; `provisionalCapacity` credits ordered-but-unarrived
  capacity and ignores the generation; `readyFree` sits between them. Anyone
  reading the code for the first time will want to unify them. §3 and §4.2 say
  why they must not be, and `docs/known-issues.md` gets the same entry so a
  search finds it.
- **4a's blindness to the generation means a group freezes nothing and rolls
  nothing.** After a spec edit it goes on scaling exactly as before, on
  servers of the old generation, until 4b teaches it the difference. That is
  the intended state between the two sub-milestones, not a defect to be fixed
  locally.
- **Scale-down removes one server per pass.** With a large group emptying at
  once, returning to the floor takes one resync per server. Deliberate, and
  revisit only if it is ever measured to matter.
- **The scaler does not know about the proxies' own view of player counts.**
  The evidence run of milestone 3c found that a player connected at the proxy
  but not yet counted by a backend sits outside the drain's protection
  (`docs/known-issues.md`, "From the milestone 3c evidence run"). The same
  window applies to scale-down: a server the backend reports as empty may have
  a player mid-handshake. The window is one round trip, `SelectDeletionCandidates`
  is not the place to close it, and 4c — which owns drain — is.
- **An operator restart resets every stabilization timer.** Accepted; the
  error delays a scale-down rather than causing a kick.
