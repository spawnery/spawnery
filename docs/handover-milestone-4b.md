# Handover to milestone 4b: rolling updates of ephemeral groups

Status: end of milestone 4a, slot-based scaling (2026-08-13). Written for a
session that starts with no memory of this one, possibly on another machine.

This document is not a spec. It says where 4a stopped, what 4b is, what it
finds in place, and the handful of things it has to decide before any code is
written. The 4b spec and plan do not exist yet; writing them is the first
piece of work.

## 0. Start here

Read, in this order, and nothing else to begin with:

1. This document.
2. `docs/superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`
   §4.4 "Rolling updates" and §6.3 "Slot-based scaling" — twenty lines each,
   and they are the requirement 4b implements.
3. `docs/superpowers/specs/2026-08-13-slot-based-scaling-design.md` §3
   "Decisions" — 4a's reasoning, three of which 4b either inherits or
   deliberately reverses.
4. `internal/controller/scaling.go` in full. It is 249 lines and it is where
   most of 4b lands.

`docs/handover-milestone-4.md` is the previous handover, written at the end
of milestone 3c and since extended. It covers milestone 4 as a whole and is
still the right place for what 4c needs; this document does not repeat it.

## 1. Where the project is

Milestone 3 is closed, both halves of its criterion proven against real
clusters: a player joins automatically (2026-08-12) and by hand with a real
Microsoft account, and a deleted `Server` moves that player rather than
disconnecting them (2026-08-13, a different machine, from an empty cluster
upward). `docs/runbook-milestone-3-evidence.md` and the "evidence run" and
"manual session" sections of `docs/handover-milestone-4.md` carry the
measurements.

Milestone 4 was cut into three sub-projects:

| | Contents | State |
|---|---|---|
| **4a** | Slot-based scaling of ephemeral `ServerGroup`s | done |
| **4b** | Rolling updates of ephemeral groups — **this document** | done |
| **4c** | Proxy drain and node drain | after 4b |

4a made an ephemeral `ServerGroup` size itself from its free player slots
instead of sitting at `spec.scaling.minReplicas`. `DecideSize` in
`internal/controller/scaling.go` is the rule; `ServerGroupReconciler.size`
executes it.

## 2. What 4b is

From the master design, §4.4. For an **ephemeral** group whose spec changed —
which raises its `metadata.generation`, making every server created before it
*stale* — the changeover runs surge-first and without kicks:

1. Stale servers **no longer** count towards the group's free slots, so the
   scaler produces replacements of the new generation by itself.
2. Once enough ready capacity of the new generation exists — **at least one
   ready server, mandatory for fallback groups** — a stale server goes into
   **soft drain**: deregistered, accepting no new joins, its players
   undisturbed until it empties on its own.
3. `spec.update.maxUnavailable` (default 1) limits how many servers of the
   group are in `Draining` or `Terminating` at once because of a generation
   change.
4. `spec.update.maxStaleSeconds` (default 0 = unlimited) forces the active
   drain once it expires, if a server does not empty on its own.

The design states why step 1 is not optional: without it the update would
never terminate, because a lobby that as a fallback target practically never
empties would stay registered and its free slots would keep the scaler from
creating anything.

**Per-group exponential backoff belongs to 4b too.** `docs/known-issues.md`
carries it as a milestone 4 precondition and names 4b as its owner: master
design §7 asks for backoff with a `Degraded` condition and a stop after
repeated failures, and `internal/controller/candidates.go:235` currently
holds the line with a blunt `maxRetainedFailures = 1` whose own comment says
so. It is in 4b because it touches `pruneFailed` and the failure path, which
the rolling update touches anyway.

**Persistent groups are milestone 5**, not 4b. The CRD already refuses
`spec.update` on them (`api/v1alpha1/servergroup_types.go:106`).

## 3. What is in place

**The CRD is complete. 4b should need no new field.** `UpdateSpec` is at
`api/v1alpha1/servergroup_types.go:61` with both fields, their defaults and
their validation. Verify that claim before relying on it, but it held at the
time of writing.

**The generation is recorded on every server and read by exactly one thing
today.** `Server.spec.groupGeneration` is written once, at creation
(`internal/controller/servergroup_controller.go:421`), and read once, into
`ServerView.Generation` (`:378`). Its only consumer is `AggregateGroup`
(`internal/controller/candidates.go:301`), which excludes stale generations
from `status.freeSlots` — the hook §4.4 step 1 asks for, already built and
so far unused by any decision.

**`DecideSize` is the sizing rule and 4b extends it rather than standing up a
second scaler.** It is a pure function over value types, table-tested without
a cluster, alongside `phase.Decide` and `SelectDeletionCandidates`. Its
inputs are a `ScalingInputs` struct; adding the generation and the update
knobs to that struct is the natural shape.

**`expectations`** (`internal/controller/expectations.go`) reserves the
creates and deletes a reconcile has issued and the cache has not shown, keyed
by name, with a 30-second TTL. A rolling update creates and deletes far more
than a floor ever did, so 4b leans on it harder than 4a does.

**Soft drain's primitive already exists, in two halves.** `Registrar`
(`internal/controller/registrar.go`) has `Register`, `Deregister` and `Drain`
as three separate calls, and `phase.Decision` has `Deregister` and
`StartDrain` as two separate flags. Soft drain is `Deregister` without
`Drain`. Milestone 3c proved the proxy side of both against a live cluster.

**`ServerView.EmptyFor`** carries how long a server has been reporting zero
players, from `agent.Snapshot.EmptyFor`. `maxStaleSeconds` needs a different
clock — time since the server became stale, not since it emptied — but the
shape to copy is there, including the rule that neither value ever decides
anything on its own: every rule that reads one also asks
`Players == 0 && !Stale`.

**`countsTowardSize()`** (`internal/controller/candidates.go:165`) already
excludes `Draining`, `Terminating` and `Failed`. A server that enters soft
drain therefore stops holding the group at its floor, and the scaler orders
its replacement on the next pass. That is the surge in §4.4 step 2, and it
falls out of existing code rather than needing new code.

## 4. The one structural thing 4b has to decide

**Soft drain is a state the `Server` machine cannot express today, and this
is 4b's equivalent of the contract change 4c owns.**

`phase.Decision.Deregister` is documented as "set on every exit from Ready",
and every path that sets it also leaves `Ready`. The phases available are:

- `Ready` — registered, taking joins, holding players.
- `Draining` — deregistered, players actively being moved off, bounded by
  `spec.drain.timeoutSeconds`, after which the server goes to `Terminating`
  **with players still online**.

Soft drain is neither. It is *deregistered, not being drained, players
undisturbed, waiting to empty on its own* — and, unlike `Draining`, it must
not have `spec.drain.timeoutSeconds` hanging over it, because a lobby can sit
in it for hours legitimately. `spec.update.maxStaleSeconds` is what bounds
it, and only when configured non-zero.

Three shapes are open, and the choice belongs in the 4b spec:

- **A new phase**, e.g. `Retiring`, between `Ready` and `Draining`. Clearest
  to read on a CR, and every consumer of the phase — `countsTowardSize`,
  `mayHavePlayers`, the PDB's `occupiedPods`, `derivePhase` — has to be
  revisited, which is a feature rather than a cost: each one is a question
  worth answering explicitly.
- **`Draining` entered without `StartDrain`**, with the drain deadline
  suppressed while the reason is a generation change. Smallest diff, and it
  overloads a phase whose name then means two things — and §4.4 step 3 counts
  "`Draining` or `Terminating`" against `maxUnavailable`, which would then
  silently include soft-drained servers.
- **A field beside the phase**, e.g. `status.retiring`, leaving the phase
  alone. Avoids touching the state machine and creates a second axis somebody
  has to remember to read — the shape this repository has been bitten by
  before (`internal/controller/candidates.go:60` records what two
  implementations of one rule cost when they drifted).

Whichever is chosen, the invariant that governs everything else must survive
unchanged: **a server that may be carrying players is never nominated for
deletion.** `SelectDeletionCandidates` holds it today and every 4a review
checked it. Write it into the 4b spec as an acceptance criterion.

## 5. The other decisions worth settling before code

- **Where the generation filter goes back into `DecideSize`, and with what
  brakes.** 4a is deliberately generation-blind; §6 below says why, and it
  matters more here than anywhere else in this document.
- **What "enough ready capacity of the new generation" means**, given §4.4
  step 2 makes one ready server mandatory for fallback groups. A group knows
  it is a fallback target only through the `ProxyGroup`s that name it in
  `spec.fallbackGroups`, which the `ServerGroup` controller does not read
  today. Either it learns to, or the rule is applied to every group.
- **How `maxUnavailable` is counted** — against servers made unavailable by
  *this* update, or against every `Draining`/`Terminating` server whatever
  the cause, including a scale-down that 4a's demand rule started. The design
  says "because of a generation change", which points at the former and needs
  a way to tell the two apart.
- **What clock `maxStaleSeconds` measures from.** The server has no "became
  stale at" — its generation is fixed at creation and the *group's* moved.
  The group's `metadata.generation` change has no timestamp of its own
  either. Something has to record one.
- **Whether a rolling update and a scale-down may run in the same pass.** 4a
  established the precedent that a group short of capacity never also shrinks
  for lack of demand, and that the ceiling is an instruction that overrides
  the shortfall. A third source of removals needs its place in that order,
  and the order is in `DecideSize`, in one function, deliberately.

## 6. What 4a left for 4b, deliberately

**`docs/known-issues.md`, section "From milestone 4a", holds both in full.**
In short:

**4a reads no generation at all, and that was a correction made during
planning, not an oversight.** A scale-up rule that credited only servers of
the current generation would find, the instant any field of the group's spec
changed, that nothing running counts — and would order a full replacement set
up to `maxReplicas` on the next five-second pass. That is precisely 4b's
rolling update, performed without `maxUnavailable`, without soft drain and
without the "at least one ready server of the new generation" guarantee.
**Putting the generation back into `DecideSize` is 4b's central task, and it
is only safe together with the rules that brake it.** Do not do it first and
add the brakes afterwards; a tree in that state kicks players.

**`provisionalCapacity` cannot tell "has never reported" from "the pod
vanished".** Both present as `Slots == 0`, because `Registry.Lookup` on an
unknown pod returns a zero snapshot, so a server whose pod is gone is
credited a full `maxPlayers` it does not have for the seconds until the
`Server` controller fails it. The blast radius is under-creation for a resync
or two and no invariant is touched. The obvious fix — testing `Stale` before
`Slots == 0` — is a regression, verified by mutation: a genuinely starting
server is also stale and has never reported, and crediting it zero
reintroduces the runaway the rule exists to prevent. The right signal is
`ServerView.SessionsGone`, already on the view and unused here: one line at
the top of the function, plus its unit tests. It belongs with 4b because 4b
is already in `scaling.go`.

**`derivePhase` measures readiness against `DesiredReplicas()`**, which
before 4a *was* the size the group ran at and is now only its floor. A group
scaled to five for spare slots publishes `status.phase: Ready` with one
server up and four starting. Defensible, but the field changed meaning
silently, and a rolling update is exactly where somebody will want to ask
"is this group finished changing over?" and find that the phase does not
answer it.

## 7. What 4b must not do

- **Do not unify `status.freeSlots` with the scaler's own figures.** There
  are three numbers on purpose: `AggregateGroup`'s `FreeSlots`
  (generation-filtered, `Ready` only, published on the CR),
  `provisionalCapacity`'s sum (credits ordered-but-unarrived capacity,
  generation-blind, drives scale-up) and `readyFree` (arrived capacity,
  generation-blind, the denominator of the scale-down feasibility test).
  `scaling.go` says why in comments, the 4a spec says why in §3 and §4.2, and
  `docs/known-issues.md` says why a third time so that a search finds it. A
  reader meeting them for the first time will want to collapse them; 4b will
  change what the first one is filtered by, and that is the only one of the
  three whose meaning is meant to move.
- **Do not weaken `SelectDeletionCandidates`.** Every path that removes a
  server goes through it, and its `mayHavePlayers` guard is what the whole
  system rests on.
- **Do not make a test pass by changing what it asserts** without first
  proving the property it guarded is still measured somewhere. 4a's reviews
  found eleven assertions that could not fail, and the worst of them was a
  pre-existing test that had silently stopped testing: mutating
  `collectViews` to a constant `false` — the exact defect the test named in
  its own failure message — left it green, because a new filter excluded the
  server before the old guard was ever consulted. "The test stopped failing"
  and "the test stopped testing" look identical from outside. For any test
  whose expectations move, make the mutation for real and report the output.

## 8. The environment

```bash
nix develop          # Go, controller-gen, protoc, envtest assets, kubectl, kind, k3d, JDK 21, Gradle
nix develop -c make test        # Go only. Must be green before anything is touched.
nix develop -c make manifests   # Must produce no diff unless a CRD field really changed.
```

`make test` takes about 38 seconds, of which `internal/controller` is about
34: envtest boots a real API server. A slow run is not a hang. Coverage sits
at 88% for `internal/controller` and 98% for `internal/agent`; both should
stay there.

**4b is operator-only Go, as 4a was.** No proto change, no agent change, no
image change — so `make agent`, `make agent-test`, `make image-test` and
`make image-repro` are not in its path. Verify that with
`git diff --name-only` before merging rather than assuming it.

**envtest's client is a direct client, not a cached one.** No envtest can
reproduce the cache lag `expectations` exists for; that behaviour lives in
its unit tests. Do not try to build a cache-lag envtest — 4a's plan says so
and the attempt wastes an afternoon.

**Machine-specific notes** live in this repository's `docs/known-issues.md`
and, on the machine 4a was built on, in that session's project memory: image
builds there need `CONTAINER=podman` and a disk-backed `TMPDIR`, and Gradle
daemons exhaust a small machine. None of it applies to 4b's Go-only path, and
none of it should be assumed on a different machine.

## 9. Where everything lives

| | |
|---|---|
| Master design | `docs/superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md` |
| 4a design | `docs/superpowers/specs/2026-08-13-slot-based-scaling-design.md` |
| 4a plan | `docs/superpowers/plans/2026-08-13-slot-based-scaling.md` |
| Open points, by owning milestone | `docs/known-issues.md` |
| Milestone 4 as a whole, and 4c's contract change | `docs/handover-milestone-4.md` |
| Milestone 3's evidence | `docs/runbook-milestone-3-evidence.md` |

The working method that produced 4a, and which is worth repeating: brainstorm
to a design spec, then a plan of bite-sized tasks each with its failing test
written out, then a fresh implementer per task with a two-stage review after
each, then one whole-branch review before merge. The whole-branch review
earned its keep on 4a — it found a fixed point no per-task review could see,
where a lowered `maxReplicas` was never enforced on a group that was also
short of spare slots.

## 4b has landed

4b closed against this document's own decisions: `Retiring` as a phase of its
own (§4), the generation kept out of the capacity arithmetic and readmitted
only for retirement candidacy and the demand rule's changeover filter (§5,
§6), and every open question in §5 resolved and recorded either here or in
`docs/known-issues.md`, "From milestone 4b". The design it produced is
`docs/superpowers/specs/2026-08-13-rolling-updates-design.md`; that document's
§11, "What 4b leaves open," is where the changeover's own gaps live —
`derivePhase` still not answering "has this group finished changing over?",
and the drain's proxy-side blind spot from milestone 3, both carried forward
rather than closed here.

**Per-group exponential backoff is next.** §1 of the 4b design measured, and
found false, the handover's own assumption that backoff belongs with 4b
because it "touches the failure path which the rolling update touches
anyway" — 4b shares no code with `pruneFailed`. `maxRetainedFailures = 1`
still stands in for it, capping the footprint of a retained failure but not
the rate at which the group tries again; 4b's own cold start inherits and
extends that gap rather than closing it (`docs/known-issues.md`, "From
milestone 4b"). Master design §7's `Degraded` condition is that spec's to
write.
