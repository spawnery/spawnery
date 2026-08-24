# Milestone 4d: per-group backoff and the Degraded condition

Status: written 2026-08-13, at the start of the milestone, against `99ce7af`
(the merge of 4b).

Companion documents: `docs/superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`
§7 is the requirement; `docs/superpowers/specs/2026-08-13-rolling-updates-design.md`
§3.7 is the stopgap this replaces; `docs/known-issues.md` carries the open
entry under both "Preconditions for milestone 4" and "From milestone 4b".

**On the number.** 4a was slot-based scaling, 4b rolling updates, 4c is proxy
and node drain. This is 4d rather than a renumbering: it was cut out of 4b
during that milestone's brainstorm, on the measurement that it shares no code
with the rolling update, and 4c was already named. Nothing here depends on 4c
and nothing in 4c depends on this, so the two may be built in either order.

## 1. What this closes

A `Server` that cannot start goes to `Failed`. It stops counting toward its
group's size, so the group creates a replacement on the next five-second pass.
If the cause is the group's own configuration — a broken image, a bad
resource request — the replacement fails the same way, and the group creates
another. Forever.

Nothing bounds that today. `maxRetainedFailures = 1` caps how many corpses are
*kept*, and its own comment says it caps the footprint rather than the rate.
Master design §7 has asked for the rest since the beginning: a replacement is
created "but with exponential backoff per group — otherwise a broken
configuration spins an endless loop of pod creations. After several
consecutive failures the group sets the condition `Degraded` (reason
`CrashLoopBackoff`) and stops trying."

**Half of it is already built and unused.** `ConditionDegraded` and
`ReasonCrashLoopBackoff` exist in `api/v1alpha1/common_types.go`;
`derivePhase` turns a true `Degraded` into the group's phase, and the
`ProxyGroup` controller reads it too. Nothing ever sets it. This milestone
writes the half that decides.

**It also retires a stopgap.** 4b closed its own door onto the same loop by
suppressing the cold start while a `Failed` server of the current generation
is retained (§3.7 of that design), and said in as many words that this was
"deliberately not backoff". That suppression is removed here, because real
backoff subsumes it and does it better: a flat hour after any failure against
a delay that grows from seconds only if the failures keep coming.

## 2. What is already in place

- **`ConditionDegraded` and `ReasonCrashLoopBackoff`** exist as constants, and
  `derivePhase` (`internal/controller/servergroup_controller.go`) already maps
  a true `Degraded` onto `status.phase`.
- **Every failure carries its own timestamp.** `Server.status.failedAt` is
  stamped on entry to `Failed` and drives the retention today.
- **Every readiness carries one too.** `Server.status.readySince` is stamped on
  entry to `Ready`.
- **The condition pattern.** `ScalingLimited` (4a) is the house shape: a
  condition built false-by-default, flipped true with a specific reason and
  message, an event only on the flank, and an explicit "nothing was decided
  this pass" case rather than an all-clear nobody checked.
- **`views` is collected before anything acts on it**, and `pruneFailed` runs
  on the same slice later in the same reconcile, so a corpse is visible in the
  pass that removes it.

## 3. Decisions

### 3.1 The counter lives on the CR, not in memory

Two fields on `ServerGroupStatus`: `consecutiveFailures` and `lastFailureAt`.

**This is the opposite of 4a's choice for `EmptyFor`, for the same reason 4b
chose the opposite for `spec.retire`.** 4a put its empty-since clock in memory
because an operator restart resets it, which delays a scale-down — the error
points in the safe direction. Here a restart would reset the backoff and
restart the hot loop immediately, which is the unsafe direction. Durability is
what errs safely.

### 3.2 A failure is counted from the server's own timestamp, once

On each reconcile, before sizing: every view in phase `Failed` whose
`status.failedAt` is newer than `status.lastFailureAt` counts once, and
`lastFailureAt` moves to the newest of them.

**The window runs from `failedAt`, never from `now`.** Stamping `now` on
observation would extend the window on every reconcile and the backoff would
never expire. The timestamp belongs to the event, not to its observation.

**`failedAt > lastFailureAt` is what makes the count idempotent.** It is the
only thing standing between a counter and a five-second resync that would
otherwise re-count the same corpse forever.

Under-counting is possible — a corpse deleted by hand between reconciles is
never counted — and it errs toward more retries, which is the unsafe
direction. It is accepted because deleting a corpse is an explicit act, and
the alternative is a second bookkeeping object tracking failures the cluster
already records.

### 3.3 The streak breaks on a success *after* the last failure

The counter resets to zero when a view carries a `status.readySince` newer
than `status.lastFailureAt`.

**Not "any server is Ready", which is the tempting version and is wrong.** A
group with one healthy server and one that crash-loops — a bad node, a
resource request only some nodes can satisfy — would hold its counter at zero
forever on that rule and hammer indefinitely. "Consecutive" means unbroken by
a success, and a success is one that happened *since* the failure being
counted from.

### 3.4 The gate is on execution, not on the decision

`DecideSize` is untouched. It keeps computing what the group *needs*;
`ServerGroupReconciler.size()` simply does not carry out the creates while the
backoff window is open.

Three things follow, and each is why this boundary is where it is:

- The pure rule stays pure and its tests stay meaningful.
- `Limited` and `ColdStartBlocked` keep telling the truth about the shortfall,
  so the operator sees both "the group needs a server" and "it is waiting" as
  separate facts rather than one muddled one.
- `expectations` never reserves a create that did not happen, which it would
  if the decision itself were suppressed.

**Deletions and retirements are not gated.** The backoff holds back building,
not tidying up. A group that is backing off must still shed surplus servers,
still retire stale ones, and still drain what is being removed — those paths
touch players and cannot wait on an unrelated failure.

### 3.5 Giving up is terminal until the spec changes

At the threshold the group sets `Degraded` with reason `CrashLoopBackoff` and
creates nothing at all.

The way back is a spec change: `metadata.generation` moves, and the reconcile
that observes a generation newer than `status.observedGeneration` clears both
fields — `status.consecutiveFailures` and `status.lastFailureAt`. The next
attempt is then immediate, because the counter is zero.

**A caution for whoever extends this to `ProxyGroup` (§7 defers exactly
that).** `ServerGroupReconciler` writes `status.observedGeneration` in exactly
one place, at the very end of a pass that ran all the way through
(`servergroup_controller.go:573`); a pass that returns early on an error never
reaches it, so the field only ever advances alongside a reconcile this
controller is willing to call settled. The clear check at §3.5 leans on
that: `observedGeneration` stays behind `generation` for as long as the group
keeps failing for reasons unrelated to the spec, and a permanently-refused
group's counter is never touched by the field at all.
`docs/superpowers/specs/2026-08-23-proxygroup-status-on-every-path-design.md`
does not give `ProxyGroupReconciler` the same property: its
`status.observedGeneration` now advances on every pass that reached
`reconcileObserved` and got as far as observing the pods and the Service,
including a pass that then failed — a group permanently refused by Pod
Security, say, still writes `observedGeneration == generation` on every one
of those failing passes (this is already recorded above, for a different
reason). A `ProxyGroup` backoff built on this milestone's exact check —
comparing `status.observedGeneration` against `metadata.generation` to decide
whether the spec changed — cannot assume the field lags while the group is
stuck; it does not lag here, and a permanently-refused group would look to
that check exactly like a group that just finished a clean pass. Whatever
signals "the spec changed enough to deserve a fresh attempt" for `ProxyGroup`
has to be something else, or has to be read before this design's own
`reconcileObserved` write touches it within the same pass.

**The conditions are not removed; they are republished as `False` with reason
`NoRecentFailures` on that same pass.** The `BackingOff`/`Degraded` switch
below — the one whose first case is `!sized` — builds both conditions
false-by-default and calls `meta.SetStatusCondition` on each of them
unconditionally, on every pass, in every case including the one where nothing
was decided. That runs after the clear, so an explicit
`RemoveStatusCondition` beside the clear would be overwritten before anything
could ever observe its absence, which is why there is not one: the switch is
the only writer of these two conditions and it always writes. The operator's
`kubectl describe` therefore shows `BackingOff: False`/`NoRecentFailures` and
`Degraded: False`/`NoRecentFailures` after the fix, not two conditions that
have vanished — and `derivePhase`, which reads `Degraded`, moves the group off
`Degraded` for the same reason.

**This matches the cause.** A broken image does not become correct by waiting;
it becomes correct when somebody fixes it, and fixing it is a spec change. The
mechanism also already exists — 4b taught the group to react to a generation
bump.

The price is real and accepted: a group that gave up because of a transient
cluster problem needs a human to touch it. Master design §7 says "stops
trying", and a backoff that silently resumes would not be that.

### 3.6 The numbers are constants

Base 10 s, factor 2, cap 5 min, give up after 6 consecutive failures — named
constants beside `MaxReadinessLosses` and `StreamDownGrace`, each with its
reasoning in a comment.

No CRD field. The master design does not ask for configurability, nobody has
asked for it, and a knob nobody turns is a knob somebody turns wrongly.
Adding a field later is cheap; removing one is not. `spec.update` has just
demonstrated the cost of an optional block, where a nil parent meant its
child's default never applied and `maxUnavailable` arrived as 0 — "never
roll" — with no error and no signal.

The sequence is one free attempt, then waits of 10, 20, 40, 80 and 160
seconds before attempts two through six, then the group gives up. Against a
container that takes about ninety seconds to exhaust its restarts, the whole
run is roughly fourteen minutes, of which about five are waiting. That is the
intended balance: relieve the cluster, do not delay the diagnosis.

**That sequence is the experience of a group at `minReplicas 1`, and only of
such a group. The threshold counts failed *servers*, not failed rounds.**
`CountFailures` counts every `Failed` view in a pass, and `size()` creates the
whole shortfall in one pass — for a group starting from nothing the floor term
`minReplicas - alive` is the entire floor at once. So a group's budget of six
is spent in `⌈6 / minReplicas⌉` rounds: at `minReplicas 2` the group gets three
attempts, at `minReplicas 3` two, and at `minReplicas 6` or above it gives up
after **one round, with no retry at all**. Measured, not reasoned about:
`DecideSize({MinReplicas: 6, MaxReplicas: 10, SpareSlots: 10, MaxPlayers: 100})`
returns `Create: 6`; `CountFailures` over those six same-instant `Failed` views
returns 6; `DecideBackoff` turns that straight into
`GaveUp: true, MayCreate: false`.

Acceptance criterion 1 is an upper bound — "at most six creation attempts" —
so this satisfies it, and the group still stops rather than hammering, which
is the property the milestone exists for. But the consequence is real and is
not what the paragraph above describes: a transient scheduler or registry
problem that fails a whole floor at once takes a group with a large
`minReplicas` straight to `Degraded`, and §3.5 makes that terminal until a
human edits the spec. An operator running a floor above one should know this
before it happens; it is in `docs/known-issues.md` under "From milestone 4d"
for that reason.

Counting rounds instead of servers, or scaling the threshold with
`minReplicas`, would change the schedule's meaning and is a design decision
this milestone did not make. It is left open in §11.

**The cap is not reached at the shipped threshold, and that is deliberate
rather than an oversight.** The largest delay before giving up is 160 seconds,
well under the five-minute cap. The cap is there so that raising the threshold
— the one of these four numbers somebody might plausibly want larger — cannot
turn the doubling into an unbounded wait. Say so at the constant, or the next
reader will find a limit that never applies and take it for dead code. It is
the only one of the four with no effect on the shipped behaviour, and its test
has to construct a threshold high enough to reach it rather than asserting it
against the default.

### 3.7 Waiting has its own condition, separate from Degraded

`ConditionBackingOff`, true while a window is open, with the count and the
remaining time in its message. An event on the flank only, as `ScalingLimited`
does.

**It is not folded into `Degraded`, and that is the same judgement 4a made
about `ScalingLimited`.** `derivePhase` turns `Degraded` into the group's
phase. A group waiting ten seconds after a single hiccup would then present
as degraded, indistinguishable from a group with a real fault. Two states,
two conditions, each saying one thing.

The two are mutually exclusive. `BackingOff` true means "waiting, will try
again". Once the group gives up there is no pending retry, so `BackingOff`
goes false — but with reason `CrashLoopBackoff` and a message saying it is not
retrying and a spec change is the way back. A false with `NoRecentFailures`
there would be a lie.

### 3.8 Counting is scoped to the current generation

`CountFailures` is given the views of the servers the group's *current*
generation produced, and no others. It is a filter at the call site in
`Reconcile`; the function itself takes whatever views it is handed.

**The counter answers "how many attempts under this spec have failed", and
only a current-generation server can answer it.** The previous generation's
corpse says nothing about the current spec — which is the reasoning
`selectFailedForPruning` already carries when it keeps the newest
generation's failure — and by the same token a previous generation's server
going `Ready` says nothing about it either.

**Without the filter, §3.5's clear undoes itself on the very pass that
performs it.** The clear sets `lastFailureAt` to nil, so the comparison point
becomes the zero time, and the retained corpse of the spec just replaced is
newer than that and is counted straight back in. The group comes out of the
operator's fix with one failure already against it and a window it did not
earn. This was measured, not reasoned about: with the filter removed the
group creates *nothing* on the pass that observes the generation bump.

**This does not weaken the standing constraint, because it runs in the
opposite direction.** The rule that the *capacity arithmetic* stays
generation-blind — `provisionalCapacity`, `readyContribution`, `readyFree`,
carried in `ScalingInputs`' type comment — exists because a generation filter
there makes every running server stop counting the instant the spec changes,
so the group orders a full replacement set up to `maxReplicas`: runaway
creates, the failure that disconnects players. A filter on *counting
failures* can only hold a create back. It cannot order one, and it never
reaches `DecideSize`.

## 4. Components

### 4.1 `api/v1alpha1`

```go
// common_types.go
// ConditionBackingOff reports that the group is waiting before it creates
// another server, after one or more failed to start.
ConditionBackingOff = "BackingOff"

ReasonNoRecentFailures = "NoRecentFailures"

// servergroup_types.go, on ServerGroupStatus
// ConsecutiveFailures counts servers that failed to start with no success
// since. It is on the CR rather than in memory because an operator restart
// must not reset it: that would restart the create loop this bounds.
// +optional
ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

// LastFailureAt is the newest status.failedAt this group has counted. It is
// what makes counting idempotent across resyncs, and the instant the backoff
// window runs from.
// +optional
LastFailureAt *metav1.Time `json:"lastFailureAt,omitempty"`
```

`ReasonCrashLoopBackoff` already exists and carries both true cases.

### 4.2 `internal/controller/backoff.go` — new, two pure functions

```go
// CountFailures folds this pass's views into the running count.
func CountFailures(views []ServerView, prev int32, since time.Time) (int32, time.Time)

// BackoffInputs is what the retry decision needs.
type BackoffInputs struct {
	ConsecutiveFailures int32
	LastFailureAt       time.Time
	Now                 time.Time
}

// BackoffDecision is what the group may do about creating this pass.
type BackoffDecision struct {
	// MayCreate is false while a window is open or the group has given up.
	MayCreate bool
	// GaveUp is true past the threshold: no further attempts until the spec
	// changes.
	GaveUp bool
	// RetryAfter is how long until the window closes. Zero when MayCreate.
	RetryAfter time.Duration
}

func DecideBackoff(in BackoffInputs) BackoffDecision
```

Both are table-tested without a cluster, beside `DecideSize` and
`phase.Decide`.

### 4.3 `internal/controller/candidates.go`

`ServerView` gains `FailedAt time.Time` and `ReadySince time.Time`, read from
the status fields that have carried them since milestone 1.

### 4.4 `internal/controller/servergroup_controller.go`

- `collectViews` reads the two timestamps into the view.
- Before `size()`: clear the two status fields if the generation has moved
  past `status.observedGeneration`; then `CountFailures`, write the result
  back to the status, and `DecideBackoff`.
- `size()` takes the decision and does not execute `decision.Create` while
  `MayCreate` is false. Deletes and retirements run regardless.
- The `BackingOff` condition, built false-by-default and flipped, with an
  event on the flank — the `ScalingLimited` block is the template, including
  its "nothing was decided this pass" case.
- `Degraded` with `ReasonCrashLoopBackoff` when `GaveUp`, and back to
  `False`/`NoRecentFailures` when the generation moves — republished by the
  same switch, not removed (§3.5).

### 4.5 `internal/controller/scaling.go`

`coldStart`'s suppression term — the `v.Phase == phase.Failed` branch that
counts a retained current-generation failure — is removed, along with the
tests that pin it. It exists only because 4b needed a stopgap for this
milestone's feature.

## 5. Data flow

A group at `minReplicas 1`. Someone points `spec.image` at a broken tag. The
floor matters to how this table reads: the threshold counts failed servers,
so a group with a larger floor gets through the same six in fewer rounds —
§3.6.

| Time | What happens | State |
|---|---|---|
| 0 | generation 3 → 4, both fields cleared | the cold start creates `C` immediately — a zero counter means no window, and generation 3's retained corpse is not counted against generation 4 (§3.8) |
| ~90 s | `C` exhausts its restarts and fails | `failedAt` stamped |
| +5 s | the next reconcile counts it | `consecutiveFailures 1`, `BackingOff` true with the count and the remaining time, one event |
| +10 s | the window closes | `D` is created |
| … | the same at 20, 40, 80, 160 s | the counter climbs |
| ~14 min | the sixth failure | **gives up:** `Degraded`/`CrashLoopBackoff`, nothing is created |

Most of those fourteen minutes are startup, not waiting: six attempts of about
ninety seconds each against roughly five minutes of windows.

**The group runs below its floor throughout**, and at `minReplicas 1` that
means nobody can join. That is the trade this feature makes deliberately: a
group creating a pod every five seconds that dies gets no player into the game
either, and takes the rest of the cluster down with it.

Recovery is the way in reversed: fix the image, the generation moves, the
fields and conditions clear, and the cold start builds at once.

## 6. Error handling

- **Operator restart mid-backoff** — nothing is lost; the fields are on the CR
  (§3.1).
- **A corpse deleted by hand** — not counted, so the group retries sooner.
  Under-counting errs unsafe, and is accepted because the deletion is an
  explicit act (§3.2).
- **Two failures in one pass** — both counted; the window runs from the newer.
- **Giving up with healthy servers still running** — at `minReplicas 3` with
  one server failing repeatedly, the group stops creating the third and the
  two healthy ones keep serving. `Degraded` is on the group and reaches
  `status.phase` through `derivePhase`, which is correct: something needs
  attention. Nothing else reads that condition on a `ServerGroup`.
- **`ScalingLimited` and `BackingOff` at once** — no conflict; each is true for
  its own reason.
- **A changeover whose new generation keeps failing** — the group backs off and
  gives up, the operator sees `Degraded`/`CrashLoopBackoff`, and the changeover
  stalls. What happens to the old generation depends on whether any
  new-generation server ever reached `Ready`.

  **If none ever did**, the old generation keeps serving untouched.
  `selectRetirement` requires a `Ready` server of the current generation before
  it will nominate a stale one, there is none, so retirement never starts. This
  is what 4b's §3.4 describes as the correct outcome, made visible sooner and
  far more cheaply by the backoff.

  **If one did, retirement starts and stale servers drain.** That case is not
  exotic — it is §3.3's own scenario, a group with one server that comes up and
  one that crash-loops, put into a changeover. The first new-generation server
  reaches `Ready`, later ones fail, and §3.3's rule that a success must be
  *since* the failure is precisely what stops that early success from resetting
  the streak. So the group reaches the threshold **with a `Ready`
  current-generation server standing**, which is exactly what
  `selectRetirement`'s gate asks for. It nominates a stale server, and
  `maxStaleSeconds` escalates that retirement to `Draining`, which moves
  players off.

  **This is bounded, and the bound is one below the floor.** Retirements are
  not gated — §3.4's rule is right and stays; a retirement stalled behind an
  unrelated failure is a changeover that stops mid-flight. But `DecideSize`
  checks capacity before it reaches the retirement branch, and the floor term
  (`in.MinReplicas - alive`) only makes `create > 0` once `alive` is *strictly
  below* `minReplicas` — one retirement later than "at the floor". So the last
  retirement still fires with `alive == minReplicas`, and only the pass after
  that, with `alive` one short, takes the `create > 0` return instead. The
  group settles at `minReplicas - 1` rather than emptying. What it does not do
  is guarantee that nobody is moved: a stalled
  changeover under this shape drains part of its old generation with no
  replacement ever created, and an operator should read `Degraded` here as
  "sessions are being moved and not replaced", not as "everything is frozen
  where it stood".

## 7. What this milestone deliberately does not do

- **It does not make the numbers configurable** (§3.6).
- **It does not gate deletions, retirements or drains** (§3.4).
- **It does not touch `DecideSize`** beyond removing `coldStart`'s stopgap
  term.
- **It does not add backoff to the `ProxyGroup`.** That controller has no
  failure path of this shape yet, and its own gaps belong to 4c.
- **It does not resume automatically after giving up** (§3.5).

## 8. Facts this design asserts about the code already here

Each was read in the tree at `99ce7af` rather than remembered:

- `ConditionDegraded` and `ReasonCrashLoopBackoff` are defined in
  `api/v1alpha1/common_types.go` and set by nothing; `derivePhase` and
  `ProxyGroupReconciler` both read `Degraded`.
- `Server.status.failedAt` is stamped on entry to `Failed`, and
  `status.readySince` on entry to `Ready`, in the same switch in
  `server_controller.go`.
- `ServerGroupReconciler.Reconcile` collects `views`, calls `size()`, sets the
  `ScalingLimited` condition, then calls `pruneFailed` on the same slice — so a
  corpse is visible in the pass that prunes it.
- The `ScalingLimited` block sets its condition false-by-default, flips it,
  fires an event only when the status changed across `SetStatusCondition`, and
  carries a separate message for the case where nothing was decided.
- `maxRetainedFailures = 1` caps retained failures and its comment says
  explicitly that it bounds the footprint rather than the failure rate, and
  that proper backoff belongs to milestone 4.
- `coldStart` counts a `Failed` server of the current generation as suppressing
  the cold start; `selectFailedForPruning` orders by generation first and then
  keeps the oldest within one.

## 9. Test strategy

Both new functions are pure, so nearly all of this is table-tested without a
cluster.

- **`backoff_test.go`** — `DecideBackoff`: the sequence 10/20/40/80/160 with
  the 5-minute cap, the threshold, and a zero counter meaning no window at all.
  `CountFailures`: a new corpse counts; the same corpse does not count twice
  across passes; two in one pass both count; a `readySince` **after**
  `lastFailureAt` resets; a `readySince` **before** it does not. That last pair
  is what pins §3.3, and it is the case a plausible implementation gets wrong.
- **`candidates_test.go`** — the two new view fields carry through.
- **envtest** — a group whose servers keep failing backs off, shows the
  condition, fires one event per flank rather than per resync, and gives up;
  a generation bump clears everything and creates immediately.

**This milestone removes tests, and that is where coverage disappears
silently.** 4b's cold-start suppression and its cases go. The loop those cases
guarded must be demonstrably guarded by a backoff test *before* they are
deleted: write the new test, mutate the backoff away, watch it fail, restore,
and only then remove the old ones. A branch that deletes a guard and its test
in one commit has no way to tell that the replacement works.

**Any test whose expectations move gets its mutation made for real and the
output reported.** On 4b, three tests' names claimed something their fixtures
no longer measured — including that milestone's own end-to-end test, which
passed with the mechanism it existed to prove reverted, and whose own proof
step would have reported success. Each was caught by running the mutation
rather than trusting a green run.

## 10. Acceptance criteria

1. A group with a permanently broken image makes at most six creation
   attempts — one free, then five after growing waits — and then stops.
2. The interval between attempts grows and is capped.
3. A success after the last failure resets the counter; a success before it
   does not.
4. A generation change clears the counter and both conditions, and the next
   attempt is immediate.
5. The counter survives an operator restart.
6. **Deletions, retirements and drains are never gated by the backoff.**
7. Removing 4b's cold-start suppression does not reopen the loop it closed —
   mutation-tested, not asserted.
8. `make manifests` produces a diff limited to the two new status fields, and
   `git diff --name-only` touches no agent, image or proto file.
9. Coverage stays at or above 88% for `internal/controller` and 100% for
   `internal/phase`.

## 11. What this leaves open

- **The `ProxyGroup` has no equivalent**, and its `pods()` still has no
  expectations tracking — both 4c's.
- **`derivePhase` measures readiness against `DesiredReplicas()`**, which since
  4a is only the group's floor, so the group phase still does not say whether a
  changeover has finished. Carried from 4a and 4b.
- **Any spec change still begins a changeover** (4b's own entry): the counter
  and conditions clearing on a generation bump is the same mechanism, so a
  scaling-knob edit also clears a `Degraded` a broken image caused. That is
  harmless — the retry it permits is exactly one — but it is worth knowing
  before someone treats `Degraded` as sticky.
- **The threshold counted failed servers, not failed rounds** (§3.6), so a
  group's six attempts became `⌈6 / minReplicas⌉` rounds and a group at
  `minReplicas 6` or above gave up after one. Whether the schedule should be
  per-round, or the threshold should scale with the floor, was the design
  decision this milestone did not make.

  **Decided 2026-08-24: per round.** `CountFailures` adds one per pass that
  sees a new failure, however many corpses that pass sees, so the schedule §3.6
  narrates — one free attempt and five growing waits — is what every group gets
  rather than what only a group with a floor of one gets.

  It was worse than this paragraph said. `DecideSize` runs a group *above* its
  floor to cover `spareSlots`, so even at `minReplicas 1` a recovery builds two
  servers and loses two: measured across the ten passes of
  `TestGroupWithABrokenNewImageDoesNotRebuildEveryPass`, the count ran 1, 3, 5
  where the schedule wants 1, 2, 3. The budget was being spent at double speed
  at the very floor this design narrates, not only at six.
- **Milestone 4c**: proxy drain, node drain, and the readiness contract
  `internal/agent/registry.go` still cannot express.
