# Handover to milestone 4

Status: end of milestone 3c, the Velocity agent (2026-08-11). Updated
2026-08-12 with what the evidence run below actually measured against a real
cluster, and again on 2026-08-13 with the manual session that closed the two
criteria that run left open; nothing before that section changed.

**If you are starting milestone 4b, read
[`handover-milestone-4b.md`](handover-milestone-4b.md) instead.** This
document remains the one to start 4c from, and the record of milestone 3's
evidence.

This document is not a spec. It says where 3c stopped and what milestone 4 —
scaling and drain — already finds in place. The design decisions live in
`docs/superpowers/specs/2026-08-11-velocity-agent-design.md`; the open points
are in `docs/known-issues.md`, whose "From milestone 3c" section this
document does not repeat in full.

## Where we are

A `Server` reaches phase `Ready` (milestone 2c), and now so does a
`ProxyGroup`: the Velocity agent opens `ProxySession`, mirrors the operator's
server list into Velocity's own registry, binds its readiness port on the
first `FullSync` and not before, routes a joining player by the group's
`fallbackGroups` try-list, and moves players off a backend the operator is
draining. Both halves of milestone 3's success criterion — a player can join,
automated and by hand — are implemented, and both now hold against a real
cluster: the evidence run below proved the automated half, and the manual
session after it proved the other half and the drain.

## 4a has landed

Milestone 4 was cut into four: 4a (slot-based scaling, done), 4b (rolling
updates of ephemeral groups — stale generations, soft drain, `maxUnavailable`,
`maxStaleSeconds`), 4d (per-group exponential backoff and the `Degraded`
condition, cut out of 4b's own brainstorm once the two turned out to share no
code) and 4c (proxy drain and node drain — the lowerable readiness
`internal/agent/registry.go` still cannot express, `ProxyGroup` scaling down
without kicking anyone, `unschedulable` nodes). What follows is what 4a built
and what 4b, 4d and 4c now find in place.

- **`DecideSize` in `internal/controller/scaling.go` is the sizing rule.** It
  is a pure function, table-tested without a cluster, and it already carries a
  comment on why it does not filter by generation: doing so would make every
  scale-down impossible from the moment anyone edits the group's spec. 4b's
  rolling update adds the stale-generation rules — `maxUnavailable`,
  `maxStaleSeconds`, soft drain — to this same function rather than standing
  up a second scaler beside it.
- **`expectations` exists**, in `internal/controller/expectations.go`, and is
  the mechanism `ProxyGroupReconciler` needs for the create/delete reservation
  it has never had (see "Closed by milestone 4a" and the rewritten
  `ProxyGroupReconciler.pods()` entry in `docs/known-issues.md`). It is
  the `ReplicaSet` controller's own mechanism, keyed by name; 4c can wire the
  same type in rather than design a new one.
- **`agent.Snapshot.EmptyFor` exists**, in `internal/agent/registry.go`, and
  `ServerView.EmptyFor` in `internal/controller/candidates.go` carries it
  through to the scaling decision. Both fields decide nothing on their own —
  every rule that reads either also asks `Players == 0 && !Stale` — which is
  the same caution 4b's soft drain and 4c's proxy drain will want for their
  own idle timers.
- **The `ScalingLimited` condition is the pattern 4c can reuse** for the
  proxy's own gaps. It is set on every reconcile of an ephemeral group (true
  exactly while `maxReplicas` is holding capacity back, false otherwise), and
  fires an event only on the flank — comparing `meta.IsStatusConditionTrue`
  before and after `SetStatusCondition`, since that call only moves
  `lastTransitionTime` on an actual change of status. The same shape works for
  whatever caps a `ProxyGroup`'s own scale-down.
- **Everything under "The one contract change milestone 4 has to make" is
  untouched and still 4c's.** 4a scaled ephemeral `ServerGroup`s by their free
  slots; it did not touch `internal/agent/registry.go`'s readiness, and a
  `ProxyGroup` still cannot lower a proxy's readiness without dropping its
  connection. That section below is exactly as 3c left it.

## 4b has landed

4b (rolling updates of ephemeral groups, 2026-08-13) makes a `ServerGroup`
whose spec changes replace its own servers: a replacement of the new
generation comes up, an old server stops taking joins, its players finish
their session undisturbed, and the server disappears once the last one
leaves. What follows is what 4b built and what 4c now finds in place.

- **A new `Server` phase, `Retiring`, is soft drain** (`internal/phase/phase.go`):
  deregistered, no active drain, no drain deadline, entered only from `Ready`.
  `internal/proxyreg` needed no change to support it — `fleet.go` turns phase
  `Draining` into a `DrainPlayers` message on every snapshot it sends a proxy
  and keys player-moving off that phase alone, so a server sitting in
  `Retiring` simply does not match and nothing is sent. Soft drain falls out
  of code that already existed rather than needing a second axis on the
  phase, which is why 4c inherits no change here at all.
- **`spec.retire` is the group's instruction channel to a server, and the
  single signal `spec.update.maxUnavailable` counts against.** The
  `ServerGroup` controller decides who retires — only it knows the
  generation, the budget and whether a ready replacement exists — and says so
  by patching `spec.retire = true`; the `Server` controller only carries the
  transition out. The field stays true across the escalation to `Draining`
  that `maxStaleSeconds` can force, so a forced drain keeps occupying the
  budget slot it started in, while a drain a scale-down or a user's own
  deletion started never had it and never counts.
- **`status.retiringSince` is the fifth phase-entry timestamp**, alongside
  `StartedAt`, `ReadySince`, `DrainStartedAt` and `FailedAt`, and drives
  `spec.update.maxStaleSeconds` on the same precedent `DrainStartedAt` set for
  the drain deadline — the group controller itself never reads it.
- **The generation is confined to two jobs and never reaches the capacity
  arithmetic.** It decides which stale server `selectRetirement` nominates,
  and it is the one exception the demand rule's changeover filter makes to
  4a's otherwise generation-blind numbers: while any stale server remains,
  demand sheds stale capacity before a current-generation server becomes a
  candidate, which closes an oscillation where the demand rule would
  otherwise delete the cold start's own replacement and prefer it, on age
  alone, over the stale server beside it. `provisionalCapacity`,
  `readyContribution` and `readyFree` are exactly as 4a left them —
  generation-blind — because reading the generation there would make every
  running server stop counting the instant any field of the group's spec
  changed, and order a full replacement set up to `maxReplicas`: the runaway
  4a was built to avoid, arriving through the capacity arithmetic instead of
  the demand rule.
- **`expectations` gained a third reservation kind, the retire
  reservation**, in the same shape as the create and delete reservations 4a
  introduced: a retirement this reconciler has patched and the cache has not
  shown yet still counts against `maxUnavailable`, so a second server cannot
  be nominated into the same budget slot while the first patch is still in
  flight.
- **4c's contract change is untouched and still 4c's.** 4b never touches
  `internal/agent/registry.go` — a `Server`'s soft drain is expressed
  entirely through `spec.retire` and the `Retiring` phase, neither of which
  needed a lowerable readiness. The lowerable readiness that `registry.go`
  cannot express — "connected, but no longer ready" — is what proxy drain and
  node drain still need, and "The one contract change milestone 4 has to
  make," below, is exactly as 3c left it.

## 4d has landed

4d (per-group backoff and the `Degraded` condition, 2026-08-13) closes the
loop 4b's own §3.7 had only half-closed: a `ServerGroup` whose servers cannot
start no longer creates a replacement every five-second pass. It counts
consecutive failures on its own status, waits 10s, 20s, 40s, 80s and 160s
between attempts, and after six in a row sets `Degraded`/`CrashLoopBackoff`
and creates nothing further until the spec changes. It was cut out of 4b
during that milestone's own brainstorm, on the measurement that it shares no
code with the rolling update; nothing in it depends on 4c and nothing in 4c
depends on it. What follows is what 4d built and what 4c now finds in place.

- **Two pure rules, `CountFailures` and `DecideBackoff`
  (`internal/controller/backoff.go`), the same shape as `DecideSize` and
  `phase.Decide`.** `CountFailures` folds a pass's `Failed` views into the
  running count, identified idempotently by each server's own `status.failedAt`
  being newer than the newest one already counted, and resets the streak on a
  `readySince` *after* the last counted failure rather than on any server
  being ready — the weaker rule would hold a mixed group's counter at zero
  forever against the one server that keeps crash-looping. `DecideBackoff`
  turns the count into a decision — `MayCreate`, `GaveUp`, `RetryAfter` —
  against four named constants (base 10s, factor 2, cap 5 minutes, give up at
  six), none of them a CRD field, for the same reason `spec.update` carries no
  knob nobody has asked for.
- **The counter lives on `ServerGroupStatus`, not in memory, and that is the
  opposite of 4a's choice for `EmptyFor`, for the same reason 4b chose
  durability for `spec.retire`.** 4a's empty-since clock resets on an
  operator restart in the safe direction — it only delays a scale-down. Here
  a reset would restart the very loop this feature exists to bound,
  immediately, in the unsafe direction. `consecutiveFailures` and
  `lastFailureAt` are therefore fields on the CR, the same durability call 4b
  made when it put `spec.retire` on the `Server` rather than tracking a
  retirement in the reconciler's own memory.
- **The gate sits on execution, not on the decision.** `DecideSize` is
  untouched; `ServerGroupReconciler.size()` simply does not carry out
  `decision.Create` while `backoff.MayCreate` is false. Deletions,
  retirements and drains are never gated — the backoff holds back building,
  not tidying up, and those paths touch players and cannot wait on an
  unrelated failure. `ScalingLimited` keeps reporting the shortfall
  independently of whether the group may act on it, so an operator sees "the
  group needs a server" and "it is waiting" as two separate facts rather than
  one muddled one.
- **Two conditions, and `ConditionBackingOff` is kept separate from
  `Degraded` for the same reason `ScalingLimited` is its own condition rather
  than folded in — the pattern 4c will want for the proxy side.**
  `derivePhase` turns a true `Degraded` into the group's phase; a group
  waiting ten seconds after one hiccup would otherwise present as
  indistinguishable from a group with a real fault. `BackingOff` is true
  while a window is open, with the count and the remaining time in its
  message; once the group gives up it goes false, but with reason
  `CrashLoopBackoff` and a message saying a spec change is the way back,
  rather than an all-clear nobody checked.
- **Counting is scoped to the current generation.** `CountFailures` is only
  ever given the views `ofGeneration` filters to the group's current spec —
  a filter at the call site in `Reconcile`, not inside the function itself.
  Without it, the generation-change clear (`consecutiveFailures` and
  `lastFailureAt` reset to zero/nil the moment `metadata.generation` moves
  past `status.observedGeneration`) would undo itself on the very pass that
  performs it: the retained corpse of the generation just replaced is newer
  than the zero watermark it left behind and would be counted straight back
  in.
- **`ProxyGroup` has no equivalent, and that is deliberate — it belongs to
  4c.** The controller has no failure path of this shape yet; 4d's own
  design says so in as many words.

## 4c-1 has landed

4c-1 (the proxy readiness contract, 2026-08-14) gives a proxy a way to stop
being ready without dropping its connection, and spends it on the first drain
that needed it: a surplus proxy is told to stop taking connections, its agent
closes the port the kubelet probes, the pod goes `NotReady`, the `Service`
drops its endpoint so no new player is routed there, and the players already on
it keep playing until they leave of their own accord. Only then is the pod
deleted. Until this landed, lowering `spec.replicas` deleted the pod in the
same instant and disconnected everyone on it. 4c was cut into three at the
start of this milestone — 4c-1 the contract, 4c-2 proxy rolling updates, 4c-3
node drain — and what follows is what 4c-1 built and what the other two now
find in place.

- **`SetReady { bool ready = 1; }` is field 7 of `OperatorToProxy`'s oneof,
  and it carries a state rather than an event.** The operator asserts what
  each proxy's readiness *should* be on every pass, for every pod, surplus or
  not; the agent maps it onto `ReadyGate.open()` and `close()`, which already
  existed and did not change. State is this repository's own rule — `Hello`'s
  readiness and `FullSync` are both stated that way — and here it buys two
  things a one-shot `Drain` message would have lost. A proxy that reconnected
  mid-drain would have come back ready, and an operator that crashed between
  asking and deleting would have left a pod stuck `NotReady` holding a replica
  slot forever, because `reconcileReplicas` counts pods and not ready pods. It
  also makes the drain reversible: a cancelled scale-down reopens the gate and
  removes the annotation, deliberately unlike the server side, where
  `Retiring` has no path back to `Ready` — a `Server` on its way out is
  replaced, a proxy on its way out may simply be kept.
- **Upgrade proxy images before the operator.** The message is additive, so an
  agent that predates it ignores the field, its gate never closes, and the
  drain runs to its deadline and disconnects whoever is on it — having gone on
  taking new players for the whole window. `docs/known-issues.md`, "From
  milestone 4c-1", says what that looks like from outside and how to tell it
  apart from a proxy that is genuinely occupied. Nothing version-gates the
  message today.
- **`internal/agent/registry.go` was deliberately not touched, and "The one
  contract change milestone 4 has to make" below is wrong about it.** That
  section, written by 3c and carried unchanged through 4a, 4b and 4d, says the
  fix needs the registry to carry "ready" separately from "connected".
  Measured against the tree it does not: `Snapshot.Ready` has exactly one
  reader, the server state machine in `internal/controller/server_controller.go`,
  and `ProxyGroupReconciler` reads the registry only for player counts and
  takes a proxy's readiness from the pod condition — which is the one the
  `Service` actually obeys. "Connected, but no longer ready" was already
  expressible; the operator simply had no way to *ask* for it. A registry bit
  beside the pod condition would have been a second copy of one truth, the
  shape `candidates.go` already records the cost of. What was actually needed
  was one message and one `when` branch. See design §3.1.
- **The one thing stored is the drain's start time, and it lives on the pod.**
  `spawnery.cloud/draining-since` (`ProxyDrainingSinceAnnotation`,
  `internal/controller/proxygroup_controller.go`), an RFC 3339 timestamp,
  written when the operator first asks that pod to go not-ready and never
  moved afterwards — re-stamping it on each five-second pass would push the
  deadline forever and the drain would never end. A `Server` keeps the same
  clock in `status.drainStartedAt`, but a proxy has no CR of its own and a
  `ProxyGroup`'s status is per group, so an annotation is the only per-pod
  place that survives an operator restart. Everything else is re-derived every
  pass, which is why a restart mid-drain continues where it was and a
  cancelled scale-down needs nothing cleaned up.
- **`Fleet.SetReady` is per session, so a reconnect re-asserts for free.** The
  send-suppressing memo (`lastReady`/`lastReadySet`) lives on the session
  rather than beside the pod UID, so a new stream starts without one and the
  next pass re-sends. The agent holds the mirror of this: `ProxyRole` records
  the assertion in the same latch as its first-sync flag, so a pod told to
  drain before its first `FullSync` does not open its gate on that sync. 4c-2
  inherits both behaviours without doing anything.
- **The wait is for *empty*, and a count that cannot be trusted counts as
  occupied.** A surplus pod is deleted when its reported player count is fresh
  and zero, or when `spec.drain.timeoutSeconds` has elapsed since the
  annotation — 300 seconds by default, five minutes rather than the server
  side's sixty seconds because nobody is moved anywhere and the operator is
  waiting for people to leave on their own. The deadline is the only path in
  this milestone that disconnects anybody and it is the only thing here that
  emits an event: one `Warning`, reason `ProxyDrainTimeout`. A *stale* count
  waits for the deadline instead of deleting, which is the repository's own
  occupancy rule (`isOccupied`, `scaling.go`) and matters more than it sounds:
  an agent's gRPC stream breaking disconnects nobody, because Velocity goes on
  serving the sessions it holds, so deleting on a bare zero would disconnect
  exactly the people the wait exists to protect.
- **4c-2 still owns which proxy goes, and how fast.** The selection is
  unchanged — from the end of the pod list — and several surplus proxies drain
  at once, which is what lowering `spec.replicas` asked for. Surge,
  one-at-a-time replacement (master design §6.6's "one at a time" is about
  *replacement* during a rolling update, not about scale-down), preferring the
  emptiest proxy, and `ProxyGroupReconciler.pods()`'s missing expectations are
  all 4c-2's. The expectations concern the *create* path racing the informer
  cache, which 4c-1 neither causes nor worsens, and
  `internal/controller/expectations.go` is still the mechanism to copy rather
  than design again.
- **4c-3 owns node drain and depends on none of this.** It drains *servers*,
  which has worked since 4b, and nothing in the operator reads `Node` objects
  yet.
- **Criterion 3 is proven at both ends and nowhere in between.** That the
  desired readiness is re-asserted after a reconnect is proved at the `Fleet`
  level by `internal/proxyreg`'s tests and at the agent level by the Velocity
  agent's, and `make agent-test` phase 1 already proves a reconnect itself
  works — but nothing exercises a real reconnect against a real readiness
  assertion end to end. Extending phase 1 to assert readiness across a proxy
  reconnect is the stronger test; it was not in this plan, and the plan's own
  self-review says so rather than leaving it implied. It is cheap work for
  whoever is next in this area.
- **Criteria 1 and 2 need a real cluster, and were run the same day.** envtest
  has no kubelet, no probes and no kube-proxy, so "the endpoint disappears
  before the pod does" and "the established connection survives" are claims it
  cannot make. `docs/runbook-milestone-4c1-evidence.md` was written for exactly
  those two and driven twice on 2026-08-14 — see "The 4c-1 evidence runs"
  below. The runbook was corrected in place after each, the way milestone 3's
  was.

## The 4c-1 evidence runs

Twice on 2026-08-14, against `kind` v0.32.0 / Kubernetes v1.36.1 under
rootless Podman, one control-plane node, both images built from the tree under
test, a licensed client at 26.2 driven by the repository's owner. The second
run exists because the first found a defect that changed the operator.

**Criterion 1 — the endpoint goes before the pod.** Lowering `replicas` from 2
to 1 flipped the doomed pod's endpoint to `ready=false serving=false` between
8 and 12 seconds later, in both runs. That is the window the probe's own
numbers predict (period 5s × failure threshold 3). The address stayed listed
in the `EndpointSlice` until the pod was actually deleted — which is why the
runbook reads `endpointslices` rather than `endpoints`: the older API prints
only ready addresses, so "stopped being ready" and "was deleted" reach it as
the same absence, and criterion 1 is precisely the claim that one happens
before the other.

**Criterion 2 — the established session survives it.** Attested both times by
the person at the keyboard, in the game, playing through the transition:
nothing was noticed. The pod stayed `0/1 Running` for as long as it was
occupied — 93 seconds in the first run — and was deleted within seconds of the
player leaving, not by any deadline. A rejoin four seconds later landed on the
surviving proxy, which incidentally demonstrates the other half: a not-ready
endpoint takes no new connections.

**The deadline, run on purpose.** With `drain.timeoutSeconds` lowered to 60,
the operator disconnected the player and said so:
`Warning ProxyDrainTimeout — deleting proxy gateway-tseg after 1m0s with 1
player(s) still connected`. The count was right, because the agent was
connected and reporting; it understates only when the count is stale, which
`known-issues.md` records. The client showed **"proxy shutting down"** — that
is Velocity's own graceful shutdown on the `SIGTERM` the deletion sends, not
anything the operator says. Milestone 3's manual session saw no disconnect
screen at all, so this path differs and §10 now states the expected message.

**What the first run found.** `status.connectedPlayers` read `0` with a person
visibly in the game: `setStatus` skipped pods that were not `Ready` before
summing, and 4c-1 had quietly made `NotReady` a populated state. The
whole-branch review ruled it a defect rather than a naming question and it was
fixed; the second run confirmed `READY 1 PLAYERS 1` at the same moment in the
same exercise. It is the one measurement here that a unit test would not have
produced, because the field is written and never read in Go.

**What the second run needed.** The owner's join landed on the *surviving*
proxy — the case that measures nothing while looking like a pass, since the
client keeps playing exactly as it should. §8's pin was used for the first
time and worked first try. One honest limit, since it changes what was
measured: the pin `Service` runs `externalTrafficPolicy: Cluster` where the
group's own runs `Local`, so the second run's criterion 1 went through a
hand-built `Service`. The first run used no pin, so between them both paths
are covered.

**A rule the runbook gained, and immediately needed.** By the second run the
log held three `has connected` lines for one player, and the correct one was
not last in the output — `kubectl logs -l` prints one pod's matches and then
the next's rather than interleaving by time. Take the most recent by
timestamp. Without that rule this run would have read the wrong pod name at
the one step where reading it wrong is invisible.

**One thing a runbook cannot record after the fact.** A pod's logs die with
it, so the player's departure has to be captured *before* the deletion. The
first run lost it and had to reason from the pod's disappearance instead.

## What the whole-branch review found, and why 4c-2 should care

Every task here passed its own review. The whole-branch pass then found a
defect none of them could see, because it lives in the composition of three
of them — the fourth milestone running where that has been true.

**The gate could be left open on a proxy the operator had already withdrawn,
and nothing would ever repair it.** The agent's fold of `(synced, asserted)`
made the *read* atomic; the *gate call* sat outside it. A `FULL_SYNC` that had
read `(false, null)` could reopen the gate after a concurrent `SET_READY(false)`
closed it, ending at `asserted = false` with the gate open — pod `Ready`, in
the `Service`, taking players, while the drain clock ran against it.

The composition is the point. `Fleet.SetReady` suppresses repeats once it has
sent a value, and `Fleet.Resync` carried `FullSync` and `DrainPlayers` and
nothing else, so nothing re-asserted, ever. Each piece was correct alone: the
memo is what turned a millisecond into forever. It reproduced at roughly 1 in
570 on unfixed code, and the new two-thread test failed six runs of six.

The fix is in two halves, and 4c-2 should keep both in mind. The agent now
holds one monitor across latch-update-and-gate-call — the `AtomicReference` was
removed rather than kept beside it, because two mechanisms for one invariant,
where one no longer carries the argument, is the shape that produced this.
And `Resync` now re-asserts the last readiness it sent, which bounds *any*
future divergence to one resync interval whatever its cause.

**Still open, and 4c-2's to take if it wants it.** The operator already holds
both the asserted value and `isPodReady()` in the same loop and never compares
them. Closing that loop would make this whole class self-correcting rather than
merely bounded. The resync is what this milestone owed; the observation is a
larger and better claim.

**A general trap, twice recorded now.** `status.connectedPlayers` and
`derivePhase`'s use of `DesiredReplicas()` (see `known-issues.md`) are the same
shape: a guard that goes on compiling, passing its tests and reading sensibly
while the meaning of the state it filters on moves underneath it. Both were
found by reading a description against what the code had come to do. Neither
was found by a test.

## 4c-2 has landed

4c-2 (proxy rolling updates, 2026-08-15) makes a `ProxyGroup` whose spec
changes replace its own proxies: the operator brings up a proxy of the new
shape, waits for it to be `Ready`, then withdraws readiness from one old one —
and from there 4c-1's contract runs unchanged. The pod goes `NotReady`, the
`Service` drops its endpoint, the players already on it keep playing until they
leave, and `spec.drain.timeoutSeconds` bounds the wait. **4c-2 adds nothing to
the drain; it creates one from a new occasion.** Everything it had to get right
is upstream of the drain: which pods are out of date, which one goes next, and
when. What follows is what it built and what 4c-3 now finds in place.

- **Staleness is a digest of the rendered pod.** `podspec.DesiredProxyHash`
  renders the pod the operator
  would build for this group right now — with the pod's name held empty, so
  nothing derived from the name reaches the digest — serialises it with
  `encoding/json`, which sorts map keys so labels and annotations do not flap
  between passes, and takes a SHA-256 prefix of that. `BuildProxyPod` stamps
  the result on every proxy pod it builds as `spawnery.cloud/pod-hash`
  (`podspec.LabelPodHash`); a pod whose label differs from the current digest is
  stale. Hashing the rendered output rather than a hand-picked field list is
  what stops someone adding a spec field that shapes a pod and forgetting to
  make it roll — the defect class this repository has counted repeatedly, a
  claim outliving the code beneath it. It buys that at two costs, both recorded
  under "From milestone 4c-2" in `known-issues.md` and both worth reading before
  this is relied on: a change to the rendering code, or to the operator's own
  namespace, moves the digest for every group with no spec edited at all and
  the next upgrade rolls the fleet; and the guarantee reaches exactly as far as
  the pod does, so the two `spec.config` fields that land only in the group's
  ConfigMap — `motd` and `onlineMode` — change nothing about a running proxy,
  while `spec.drain.timeoutSeconds`, which reaches the pod as
  `terminationGracePeriodSeconds`, rolls the whole group.
- **`metadata.generation` was deliberately not reused, and the reason is
  specific to proxies.** 4b's rule is on the record above and in
  `known-issues.md`: the generation moves on every edit, so tuning a scaling
  knob replaces a group of functionally identical servers. On a `ProxyGroup`,
  `replicas` is the routine edit, so that rule would turn every scale-up and
  scale-down into a full replacement with a drain deadline behind each pod.
  Scaling a proxy group had to stay scaling.
- **`DecideRollout` (`internal/controller/rollout.go`) sizes and replaces in one
  pure function**, over `[]ProxyView`, in the shape `DecideSize` established in
  4a and `phase.Decide` before it. The reconciler carries the answer out and
  makes none of it. The target is `replicas + surge`, where `surge` is 1 while
  any pod is stale, and then four rules in order: create the difference if there
  are fewer pods than the target; mark nothing further if anything is already
  draining; mark the surplus if there are more pods than the target; otherwise,
  if stale pods remain and the group has a ready pod to spare, mark one stale
  pod.
- **`surge` stays 1 while a *marked* pod still exists, and what that buys is
  the create branch, not the mark.** The temptation is to drop the surge the
  moment a pod is marked, on the reasoning that the replacement has been
  decided. Do not, and the reason is worth deriving rather than repeating,
  because the design's own reason for it is not the one that bites in the code
  as it shipped. §3.2 argues that dropping the surge leaves the group at
  `replicas + 1` against a target of `replicas`, so the surplus rule marks a
  second pod and the whole group drains at once — but the rules ship with the
  one-at-a-time guard *ahead* of the surplus rule, so that path is already
  closed by the guard. What is not closed is the create branch, which is
  checked before the guard: with the surge dropped, a group whose surge pod
  **dies while the old one is still draining** stands at `replicas` pods
  against a target of `replicas`, builds no replacement, and then drops to
  `replicas - 1` ready the moment the draining pod goes. Surge outliving the
  mark is what rebuilds it in place instead.
  `TestDecideRollout`'s "the surge pod dying mid-drain is replaced, because
  surge outlives the mark" is that case exactly. What advances the cycle either
  way is 4c-1's deletion loop removing the drained pod, when it is empty or when
  its deadline expires; until then the guard returns early and the pass decides
  nothing else.
- **Ready capacity is what the gate measures, not generation.** The last rule
  waits on `readyBeyond`: the group holds more ready, non-draining pods than
  `replicas`, counting stale and current alike. The design's §3.2 phrased that
  rule as "a current-generation pod beyond `replicas` is `Ready`", and it
  shipped the wider way on purpose — what protects a player is a ready proxy,
  and which generation supplies it does not change that. It also matters for a
  reverted spec, where the pod holding the spare capacity can be the stale one.
- **The annotation is now the carrier of intent, and position carries
  nothing.** 4c-1's loops walked the tail by index (`for i := len(pods)-1; i >=
  replicas`), which cannot express "this particular pod is out of date" — a
  stale pod may be the oldest in the group. `spawnery.cloud/draining-since` is
  now the marker both loops derive from: the readiness loop asserts
  `SetReady(!draining)` per pod, and the deletion loop iterates the pods
  carrying the mark in any order, applying 4c-1's rule unchanged. A surplus pod
  and a stale pod became the same case, distinguished only by what caused the
  mark. This removed a coupling rather than adding a mechanism, and it is the
  part of 4c-2 most worth knowing before touching this file: after it, nothing
  about a pod's fate depends on where it sits in a sorted list.
- **The marks are re-derived every pass, and holding one across passes is
  arithmetic, not memory.** `DecideRollout` names nobody while another pod is
  draining, so a mark made last pass has to be kept by the reconciler or it
  would be cancelled and re-made on alternate passes — and each cancellation
  deletes the annotation, so the deadline would restart from zero forever. A
  stale pod's mark is kept per pod, because a stale pod has to go whatever else
  is true. Being *surplus* is not a property of any pod at all: it is one number
  short of another, so asking each marked pod "is the group over its count?"
  gives every one of them the same answer, and a group that lowered `replicas`
  twice and then raised them partway would keep every mark it ever made. The
  count kept is `len(views) - staleMarks - replicas`, and it subtracts stale
  *marks* rather than stale pods, because a stale pod nobody has marked is still
  serving. `TestARevertedSpecChangeKeepsTheMarkItAlreadyMade` pins the one state
  where those two counts come apart — a spec change reverted while a proxy is
  draining for it, after which the marked pod matches the spec again and the
  surge pod does not.
- **One selection rule now serves every reason a pod goes** (`pick`, in
  `rollout.go`): stale before current, because a stale pod has to go regardless
  and taking a current one first would drain two pods for one replacement; then
  fewest players, because the emptiest finishes soonest and disconnects fewest
  people at the deadline; with an untrusted count sorting last, on the
  repository's own occupancy rule — a pod whose agent stream is down may hold
  players nobody can see, so unknown counts as occupied. It replaced 4c-1's
  scale-down rule as well, and that is a behaviour change worth stating
  outright: **lowering `replicas` no longer necessarily removes the newest
  pod.** With one player on the newer of two proxies and both counts trusted,
  the pod that goes is the older, empty one.
- **The age tie-break points the way it does for a reason, and no envtest test
  can see it.** Age breaks what the counts leave, newest first. That is not
  symmetry and it is not arbitrary: it is the same guess 4c-1's rule was making,
  that an older proxy has had longer to collect players. The guess is worth
  nothing between two known counts, and it is the only thing there is when every
  count is untrusted — an operator that has just restarted, or a fleet whose
  agent streams are all down. Marking the oldest there picks the pod most likely
  to be occupied, which then reads as occupied, which holds the drain open to
  the full deadline before disconnecting whoever was on it. **No envtest test
  here can catch a reversal of that clause**, by construction: envtest creates
  a group's pods within one second of each other, `CreationTimestamp` has
  second resolution, so they tie, the comparator's last clause does not fire,
  and `sort.SliceStable` falls through to list order. What pins the direction
  is `TestDecideRollout`'s table, in two subtests — "equal counts break by age,
  newest first" and "untrusted counts all round still take the newest".
  Somebody who flips the clause on symmetry grounds, runs the cluster-level
  suite, and sees nothing break there is reintroducing a real defect with a
  green run behind them.
- **One at a time is a property of replacement, not of scaling.** Master design
  §6.6's "one at a time" governs *replacement*, and `DecideRollout`'s
  one-at-a-time guard is what implements it. A lowered `replicas` asked for all
  the surplus pods to go, and the surplus rule marks all of them on one pass —
  several proxies then drain simultaneously, each on its own deadline. That was
  4c-1's behaviour, it is unchanged, and nothing in this milestone narrows it;
  it is written down here because nobody had written it down.
- **`ReadinessDiverged` reports; it does not repair.** The operator already held
  both the readiness it asserted and the pod's own `Ready` condition in one
  loop, and 4c-1's whole-branch review left closing that loop as 4c-2's to take
  if it wanted it. It took it, as far as reporting.
  `ConditionReadinessDiverged` (`api/v1alpha1/common_types.go`) is true while at
  least one pod has disagreed for longer than `readinessDivergenceGrace` — 60
  seconds, a constant, chosen to clear both known delays with margin: the
  kubelet needs 10–15s to flip a condition (period 5s × failure threshold 3),
  and `Fleet.Resync` re-asserts every 30s. One `Warning` on the false→true
  flank, compared the way `ScalingLimited` does it. Repair was considered and
  rejected rather than skipped: 4c-1's `Resync` already re-sends the last
  asserted readiness every tick, so a divergence caused by a lost message heals
  itself within one interval. What survives that is an agent that received the
  withdrawal and did not act on it — a broken build, a leaked socket — and
  re-sending does not fix it. Being told, before the deadline disconnects people
  from a proxy that never stopped taking them, is what is actually useful there.
- **The condition shipped narrower than the design's letter, deliberately.**
  Design §3.6 says the condition is true when actual readiness "has disagreed
  with" the asserted value, unqualified — both directions. It ships covering
  only the withdrawal direction: asserted `false`, pod still `Ready`. The
  reverse would have been a misdiagnosis rather than noise. Every non-draining
  pod is asserted `SetReady(true)` from the moment it exists, before any kubelet
  has probed it once, so a proxy pulling this repository's own images — the
  Paper one is measured at 735 MB as a tarball under "From milestone 2b" in
  `known-issues.md`, and the Velocity one is built the same way — onto a cold
  node trips the 60-second grace and gets named in a `Warning` saying the agent
  heard the instruction and did not act, when it is starting up and has
  disobeyed nothing. That direction already has a better diagnosis elsewhere:
  the group sits below its ready count, and "a proxy that cannot bind its ready
  port is silent on the CR" — an entry in both `known-issues.md`'s "From
  milestone 3c" section and in this document's own "What 3c leaves open,
  briefly" —
  is the diagnosis that direction actually needs. Widening the condition later
  means solving the cold-start case first, not deleting a clause.
- **`readinessDivergence` is per group and holds no TTL**, unlike
  `expectations`. An entry measures how long a pod diverged *while something was
  watching*, so a pass that does not observe a group voids its measurement
  rather than letting a stale first-seen timestamp survive the gap and fire the
  moment observation resumes. `Reconcile`'s three steady-state early returns
  therefore call `forget` explicitly. The error returns above `reconcileReplicas`
  leave the same shape and do not — `known-issues.md` records that, and the
  structural fix that would make the type enforce it, as a deferred decision.
- **`expectations` now covers the proxy create path**, closing the half of a
  milestone-4 precondition that 4a left open — `known-issues.md`'s
  "`ProxyGroupReconciler.pods()` has no expectations tracking", which 4a closed
  for `ServerGroup` and left the rest to 4c. `observePods` is a second, narrow
  method beside `observe`
  rather than a generic one — a proxy has no per-pod CR and so no retire
  reservation, and two small methods that each read clearly beat one that has to
  explain an absent third case to half its callers. The correction is arithmetic
  and sits on `DecideRollout`'s answer (`create = decision.Create -
  pendingCreates`, floored at zero) rather than inside it, so `rollout.go`'s
  sizing still knows nothing about reservations. This mattered more after 4c-2
  than before: a rollout creates a pod per replacement rather than only at
  scale-up, so the create path races the informer cache as a matter of course.
- **4c-3's node drain depends on none of this.** It drains *servers*, which has
  worked since 4b, and nothing in the operator reads `Node` objects yet. The
  contract 4c-1 added is untouched here — no wire change, no new CRD field, and
  `make agent-test` needed no extension — so what 4c-3 finds is exactly what 4c-1
  left it, plus a proxy group that now replaces its own pods.

## The 4c-2 evidence run

Driven 2026-08-15 against merged `master` on the same `kind` setup as 4c-1's
two runs, with a licensed client and the repository's owner at the keyboard,
following the runbook's §11. All seven expectations held.

**The rollout, as it happened.** Both proxies started on one digest. Four
seconds after the image was patched a third pod existed on a new digest and
nobody was marked; eight seconds later that pod was ready, the *empty* old
proxy was already gone, and the second replacement had been created. The
occupied proxy was marked at twenty seconds and left the endpoints at
thirty-two. Through all of it the group never showed fewer than two proxies at
`1/1` — briefly three, which is the surge — never more than one
`draining-since`, and `PLAYERS 1` throughout, including while the pod the
player was on was draining.

**The criterion, which only the person driving can attest:** two replacements,
one of them on their own proxy, and the session ran through untouched — no
disconnect, no stutter, nothing noticed at all.

**Three things the run established that reading could not.** The `ctr` retag
§11 depends on was the one step nobody had ever executed, and it works. The
empty old proxy vanishes without its `draining-since` ever becoming
observable — four-second polling never caught it — which is what §11's
expectation 2 predicts and is a rare case of a runbook correctly predicting an
*absence*. And the departure was captured this time, four seconds before the
pod disappeared, which is the ordering 4c-1's §9 wanted to evidence and whose
evidence died with the pod that day.

**The end state was exactly as specified:** two pods, not three, both on the
new digest, none marked, `READY 2 PLAYERS 0`, and no `ProxyDrainTimeout` event
anywhere — nobody was disconnected, because the player left of their own accord
and the deadline never came into it.

## 4c-3 has landed

4c-3 (node drain, 2026-08-15) closes the gap `2026-08-15-node-drain-design.md`
opened with: until this milestone the operator did not read `Node` objects at
all, so a node being cordoned or drained was invisible to it, and what
happened next depended entirely on which kind of pod sat there — an occupied
server was protected by its group's `PodDisruptionBudget` and `kubectl drain`
simply hung for as long as somebody was playing, and an occupied proxy had no
protection at all and every player on it was disconnected the moment the
eviction API reached the pod. 4c-3 gives both sides a way to empty themselves
proactively, so `kubectl drain` finishes instead of hanging and nobody on a
departing node is disconnected by surprise. **It adds no drain of its own** —
the same restraint 4c-2 exercised on the rollout side: a departing node is a
new *occasion* for drains that already exist and were already proven by
earlier milestones, not a second mechanism running beside them. What follows
is what it built and what the next milestone finds in place.

- **Deleting a `Server` CR *is* the drain sequence, and a departing node is
  simply a new occasion for it.** Nothing was added to the drain itself:
  `DeletionRequested` is still fed from exactly one source, the state machine
  in `internal/phase` runs exactly as milestone 4b left it, and a condemned
  server is deleted the same way a scale-down's surplus server or a rolling
  update's retiree is — through `deleteServer` and
  `Expectations.expectDeleted`, with only the event reason changed to
  `NodeDraining` so an operator reading events can tell why a given server
  went. Whoever next touches drain timing, the finalizer, or the phase state
  machine is not touching anything node-drain-specific by doing so; there is
  nothing node-drain-specific there to touch.
- **A proxy on a departing node is *stale*, and 4c-2's rollout does the rest
  unchanged.** `DecideRollout` (`internal/controller/rollout.go`) itself did
  not move; the node fact feeds in at the single site where
  `ProxyGroupReconciler.reconcileReplicas` builds each pod's view, alongside
  the pod-hash mismatch that already made a pod stale for 4c-2's reasons. A
  pod stale for a departing node and a pod stale for an out-of-date spec are
  not distinguished anywhere downstream — the same surge, the same
  one-at-a-time guard, the same `pick` ordering (stale before current, fewest
  players, untrusted counts last, ties broken by age) decides both, which is
  deliberate: §3.4 of the design argues that ranking a node reason against a
  hash reason would need a new clause for no behavioural gain, since the
  property that actually matters at a deadline — who gets disconnected — is
  occupancy, and `pick` already sorts on that.
- **`ServerView` gained `Condemned bool` and `NodeName string`; `SizeDecision`
  gained `Condemn []string`; no `Server` status field was added.** The group
  already resolves each server's pod through `podFor` to read its player
  count, and `pod.Spec.NodeName` is right there on the same object — so
  `collectViews` (`internal/controller/servergroup_controller.go`) reads it
  directly rather than mirroring it into `ServerStatus`, the same discipline
  `candidates.go` already keeps for player counts. `NodeName` is
  reporting-only: it rides on the view alongside `Condemned` but the only
  thing that reads it is diagnostic, and no sizing rule branches on it —
  `Condemned` alone is what `DecideSize` consumes, so node vocabulary never
  reaches the scaling arithmetic. A server whose pod `podFor` cannot resolve
  is never condemned — `podFound` is false on any of that function's three
  routes: no `status.podName` yet, a `Get` that failed, or a pod already
  carrying a deletion timestamp and leaving under its own power regardless
  of the node. The middle route is the one worth naming separately: a failed
  `Get` may in truth be a live pod on a departing node this pass simply
  could not read, so "never condemned" there is the safe direction chosen
  rather than a claim that no such pod exists — the next reconcile tries
  again. `collectViews`'s own comment currently states this as "in all
  three cases there is no such pod to make the claim about," which is exact
  for the other two routes and not for this one; it is a standing parked
  finding from this milestone's own review (`.superpowers/sdd/2026-08-15-node-drain/progress.md`,
  Task 4's parked minor 2) rather than something Task 9 corrects, since
  Task 9 writes no code.
- **`Condemn` is unconditional, all-at-once, and counted as leaving in the
  same pass — three separate properties, each load-bearing.** Unconditional,
  because the node is leaving with or without this operator's consent and a
  budget that declined the deletion would only delay moving those players
  rather than keep the server running. All at once, because draining one
  condemned server per pass would turn `kubectl drain` into the sum of one
  `drain.timeoutSeconds` window per occupied server on the node rather than
  one window for the whole node. Counted as leaving in the same pass, because
  the capacity arithmetic that orders a replacement has to see a condemned
  server as gone in the identical pass that condemns it, or the replacement
  would not be ordered until a pass later. **Only the create half of `size()`
  is gated by the group's backoff** — the condemn loop runs every pass
  regardless, the same as the delete and retire loops beside it, because it
  touches players and must not wait on a failure that has nothing to do with
  the node leaving; `docs/known-issues.md`'s "From milestone 4c-3" records
  what that costs a group already in backoff when the two coincide.
- **`ServerView.leaving()` was split into `leavingByPhase() || Condemned`,
  because `expectations.go`'s delete reservation must be satisfied by
  evidence of a removal, not by a node signal alone.** The first draft simply
  added `Condemned` to the existing three-phase `leaving` predicate; the
  milestone's own review caught that `expectations.go` reused that same
  predicate to decide whether a delete reservation had been satisfied, and a
  node being condemned is not evidence the server actually left — only
  `Draining`, `Terminating` or `Retiring` are. `leavingByPhase()` now carries
  the original three-phase test alone, and `expectations.go` calls that;
  `leaving()` stays `leavingByPhase() || Condemned` for the capacity
  arithmetic, which does need to know about a condemnation the instant it is
  decided, before the phase has had a chance to move.
- **Both group kinds now carry a `PodDisruptionBudget`, both maintain
  `spawnery.cloud/occupied`, and there are two occupancy rules that differ
  for a stated reason.** The `ServerGroup` has had `isOccupied`
  (`candidates.go`) since milestone 4b; the `ProxyGroup` gains
  `proxyOccupied` (`proxygroup_controller.go`) this milestone, evaluated
  exactly once per pod per pass by `syncOccupiedLabels` and handed both to
  the label it writes and to the count `reconcileProxyPDB` sizes
  `minAvailable` from — never two separate registry reads for one budget,
  which the milestone's own review found reachable as a real race
  (`docs/known-issues.md`'s Critical 2 under the 4c-3 review, closed
  structurally rather than by a comment). The two rules disagree on purpose:
  `isOccupied` treats a stale count as empty unless the server was ever
  `WasRegistered`, because an unregistered server's stream going stale is
  ordinary during startup; `proxyOccupied` has no such qualifier and treats
  *any* stale or disconnected count as occupied, because a proxy sits behind
  the `Service` directly and a stream nobody is updating says nothing about
  who Velocity itself is still serving. Both `PodDisruptionBudget`s are
  named through `podspec.GroupPDBName(group, role)` now, not through the
  group's bare name — the `ServerGroup`'s budget was renamed to this scheme
  in the same milestone that introduced the `ProxyGroup`'s, because a
  `ServerGroup` and a `ProxyGroup` sharing a name would otherwise fight over
  one budget the way `GroupConfigMapName`'s own doc comment already
  narrates for the ConfigMap collision this repository lived through once
  before. `docs/known-issues.md` says what an already-running cluster finds
  left behind by that rename and how to clear it.
- **The operator now caches every `Node` in the cluster, with
  `status.images` stripped on the way in.** `cmd/spawnery-operator/main.go`'s
  `Cache.ByObject` entry for `corev1.Node{}` carries a `Transform` that nils
  `Status.Images` before the object ever reaches an informer, beside the
  `ConfigMap` and `ServiceAccount` restrictions already there for the same
  reason: nothing in this operator reads that field, and it is tens of
  kilobytes per node. `Node` is cluster-scoped, so `-namespace` does not
  narrow it — the design flagged this as needing verification against the
  vendored controller-runtime, and it does not need an explicit
  `Cache.DefaultNamespaces` override: this version (v0.24.1) routes
  cluster-scoped kinds to a separate cluster-wide cache regardless of that
  setting. `ServerGroupReconciler` and `ProxyGroupReconciler` each
  `Watches(&corev1.Node{}, ...)`, mapping a node event onto the groups with
  pods on it — no new controller, and no `NodeReconciler` writing behind
  `expectations.go`'s back.
- **`IsDeparting(node, taintKeys)` is a pure function, table-tested without a
  cluster** (`internal/controller/nodes.go`), and it is the one place both
  reconcilers ask whether a node is on its way out: `spec.unschedulable`,
  always honoured, or a taint whose key is in the operator's `-drain-taint`
  list *and* whose effect is `NoSchedule` or `NoExecute` — deliberately not
  `PreferNoSchedule`, which does not stop the scheduler placing a replacement
  right back where it started. The list is a repeatable flag, empty by
  default; `docs/known-issues.md` says what an empty default costs a
  cluster-autoscaler user, corrected in place during this milestone's own
  review after an earlier design draft overclaimed that cluster-autoscaler
  cordons a node in addition to tainting it.
- **A `NodeDraining` condition and a matching event exist on both group
  kinds.** `drainingCondition` (`nodes.go`) builds `ConditionNodeDraining`
  from the departing node names each reconciler has already computed this
  pass — `ServerView.Condemned` on the `ServerGroup` side,
  `reconcileReplicas`'s own per-pod verdict on the `ProxyGroup` side — so
  neither caller asks `nodeDeparting` about the same pod twice. One event per
  group, reason `NodeDraining`, fired on the transition a server is condemned
  or a proxy is marked, not on every pass it stays that way — the same
  restraint `retireServer` already used for exactly this reason.
- **Uncordon: begun stays begun, and this is established by an envtest, not a
  table case, because the mechanism does not live where the design first
  looked for it.** The design's own §3.6 originally located the
  mark-preservation in `DecideRollout` and asked for a table case to prove
  it; the milestone's own review found that with nothing stale, `DecideRollout`'s
  `draining > 0` guard returns before `pick` is ever reached, so a table case
  over that function cannot exercise the constraint at all. The mark actually
  survives in `reconcileReplicas`, which reconstructs `surplusMarks` fresh
  every pass — an uncordoned pod is still draining but no longer stale, so it
  lands in the surplus set on arithmetic alone and keeps the mark it already
  has. The design was corrected in place (`b568fb2`) to name the real
  mechanism, and `TestAnUncordonedNodeKeepsTheMarkAlreadyMade` drives the real
  reconciler rather than the pure function to prove it — the first attempt at
  a table case for this passed for the wrong reason, which is exactly why the
  envtest exists.

- **The absolute-word sweep is this repository's one countermeasure that has
  caught this milestone's signature defect prospectively, and it needs a
  correction before the next milestone inherits it broken.** The sweep —
  `git diff -U0 | grep -nE '\b(never|only|nothing|exactly one|cannot|always|
  every)\b'` over a staged diff, then re-deriving each hit against the code
  beneath it — is what this milestone leaned on to catch a sentence, a
  comment or a test name whose claim had outlived what the code actually
  does: sixteen instances across 4c-3 alone, four of them introduced *by the
  fix for another instance*, the same shape recurring one layer down each
  time a sentence was rewritten to be more precise and the rewrite's own new
  clause went unchecked. **The grep has to be case-insensitive, and for this
  entire milestone it was not.** A sentence-initial capitalised "Only" went
  straight through a case-sensitive sweep — in a sentence that was itself
  written to fix an overclaim the sweep had just caught, and that opened with
  the very word the sweep exists to find. That is not an edge case: a
  sentence *opening* with an absolute word is where an overclaim is likeliest
  to sit, since the word states the sentence's whole force before anything
  qualifies it, and the case-sensitive form was blind to exactly that
  position. **Two other shapes the sweep does not catch at all, and each
  needs its own separate pass, because grep only ever reads the lines a diff
  touches — added or removed — never a line that sat there unchanged.** A
  claim in the present tense about wiring that does not exist yet — "the
  watch and the resync bring the answer back within seconds," written before
  anything watched anything — reads as ordinary present-tense prose and
  contains none of the sweep's flagged words; the same sentence can just as
  easily go stale from the other direction, once the wiring it once
  correctly denied has since been built and nobody returns to update it,
  which is exactly what happened to this one once two later tasks registered
  the watch it had described before either existed. And a new addition can falsify an
  *old* sentence sitting undisturbed nearby: a fixture comment said NodePorts
  were cluster-scoped "unlike every other object these tests create," true
  when it was written and false the moment a later helper started creating a
  `Node` — itself cluster-scoped — beside it. A diff-based grep never
  surfaces that: the sentence that became wrong is not among the lines that
  changed, only the addition sitting next to it is. Neither was found by the
  sweep or any other mechanical pass; both were found by a person reading the
  surrounding prose rather than only the diff in front of them.

**4c is now complete as three sub-milestones — 4c-1 the readiness contract,
4c-2 proxy rolling updates, 4c-3 node drain — and what remains is proof, not
code.** §12 of `docs/runbook-milestone-4c1-evidence.md` is written for the two
claims envtest cannot make — that `kubectl drain` actually completes on a real
kubelet, and that a real player survives a real cordon — but it is marked not
yet driven: it is run by the human partner and the acting agent together,
after the whole-branch review this milestone's own implementation record
(`.superpowers/sdd/2026-08-15-node-drain/progress.md`) has not yet had. Until
that run, criteria 1 through 5 of the design's §7 acceptance criteria are
proven only at the envtest level named in the design's §6, the same limit
4c-1's own evidence runs existed to close for the readiness contract.

## The evidence run

`docs/runbook-milestone-3-evidence.md` was run against a real `kind` cluster
on 2026-08-12: kind v0.32.0, Kubernetes v1.36.1, rootless Podman, one
control-plane node, 8 GiB RAM and 8 vCPU, images
`ghcr.io/spawnery/paper:26.2-0.2.0` and `ghcr.io/spawnery/velocity:3.5.1-0.2.0`,
operator run outside the cluster through `go run` with a socat relay on the
kind network. Six defects in the runbook itself stopped the run at various
points and are now corrected there; they are not repeated here.

**Criterion 7 — a player can join, automated. PROVEN.** Clean run, exit 0:

```
$ spawnery-join --host 127.0.0.1 --port 30565 --hold 45s --timeout 75s
{"protocol":776,"username":"spawnery_probe","uuid":"bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f","compressed":true}
```

Velocity's own log (`gateway-auto`) and Paper's (`lobby-q7mv`), the same
second:

```
[06:01:39 INFO]: [server connection] spawnery_probe -> lobby-q7mv has connected
[06:01:39 INFO]: UUID of player spawnery_probe is bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f
```

On an earlier run in the same cluster, `kubectl get proxygroup gateway-auto
-o jsonpath='{.status.connectedPlayers}'` read **`1`** during the hold,
confirming the whole-branch review's prediction that a held connection is
counted.

Routing honoured the try list: `fallbackGroups: [lobby, hub]`, and the player
landed in `lobby`.

**The forwarding chain is proven live**, read directly out of the running
pods rather than inferred. Velocity's `/data/velocity.toml`:

```
bind = '0.0.0.0:25565'
config-version = '2.8'
forwarding-secret-file = '/etc/spawnery/forwarding.secret'
online-mode = false
player-info-forwarding-mode = 'modern'
show-max-players = 100
```

Paper's `/data/config/paper-global.yml`, under `proxies.velocity`, **as
Paper itself wrote it back**:

```
    enabled: true
    online-mode: true
    secret: <redacted>
```

and `server.properties` carries `online-mode=false`.

That `enabled: true` is the milestone's most important single artifact and
deserves to be called out as such: before `494fa47` fixed the rendered key
from `secret-key` to `secret`, Paper's own post-processing set this to
`false` and logged why in every container since milestone 3b (see
`docs/known-issues.md`, "From milestone 3c"). This is the first time the
forwarding chain has been observed working end to end, not merely rendered
correctly on disk.

`spec.config.onlineMode: false` reaching `online-mode = false` in the
rendered TOML is the second artifact worth naming: it is the CRD field added
in `14331b2`, doing exactly what it was added to do.

**Criterion 8 — a player can join manually, with a real Microsoft account —
was not attempted in this run.** It needs a licensed Minecraft client and a
person to drive it, neither available in this session.
`docs/runbook-milestone-3-evidence.md` §10, "The manual proof, for a later
session", was written for whoever ran it next. That session happened the
following day and is recorded under "The manual session" below.

**Criterion 9 — deleting a `Server` moves a connected player rather than
disconnecting them — could not be proven by this run, and the reason is its
most important finding.** Deleting a `Server` with a `spawnery-join --hold`
player on it disconnected the player instead of moving them. The defect is
in the evidence tool's fit for this criterion, not in the drain logic: a
held join never reaches the point where Paper counts it as an online player,
so `Server.status.players` reads zero for a connection the proxy is still
holding, and the drain's own exit condition
(`internal/phase/phase.go:224`, `if !in.Occupied()`) reads that zero and
deletes the pod. Full diagnosis, the measured Kubernetes events, and why
prior reviews missed it are in `docs/known-issues.md`, "From the milestone 3c
evidence run (2026-08-12)". Two things follow from it, kept
separate there: criterion 9 can only be proven manually until
`cmd/spawnery-join` plays the configuration phase through, and a narrower
product finding — a player connected at the proxy but not yet counted by the
backend sits outside the drain's protection today — that belongs to
milestone 4's own design work on drain, not to this evidence tool.

## The manual session

`docs/runbook-milestone-3-evidence.md` §10 was run on 2026-08-13, on a
different machine from the day before (NixOS, 93 GiB RAM, rootless Podman
5.8.4, kind v0.32.0, Kubernetes v1.36.1), against a fresh `spawnery-evidence`
cluster built from §0 upward exactly as §10 instructs. The runbook needed no
correction this time: every section ran as written, and all four pods reached
`Ready` 21 seconds after `kubectl apply`. Log timestamps below are the
containers' own clock (UTC); the host ran CEST, two hours ahead.

**Criterion 7 re-confirmed first, before spending the account's login** — §10
asks for this so that an environment problem cannot be mistaken for a product
one:

```
$ spawnery-join --host 127.0.0.1 --port 30565 --hold 60s --timeout 90s
{"protocol":776,"username":"spawnery_probe","uuid":"bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f","compressed":true}
exit=0
```

`gateway-auto.status.connectedPlayers` read `1` six seconds into the hold —
faster than the runbook's own "not in the first ten seconds" caution
suggests, so that caution is a floor and not a measurement. Both log lines
appeared as on 2026-08-12, this time naming `lobby-6yw2`.

**That `online-mode` was really on for the manual proof was measured, not
assumed.** `gateway-manual`'s `/data/velocity.toml`, read out of the running
pod, carried `online-mode = true` and `player-info-forwarding-mode =
'modern'`; and `spawnery-join` pointed at 30566 was refused exactly where it
should be:

```
spawnery-join: the server is in online mode and asked for encryption, which this client cannot answer
```

That refusal does double duty — it proves the NodePort is reachable from the
host and that a real Mojang session is genuinely being demanded there. It is
worth running before the manual join for that reason.

**Criterion 8 — a player can join manually, with a real Microsoft account.
PROVEN.** A licensed Minecraft Java 26.2 client on the cluster host joined
`127.0.0.1:30566`. Velocity's log (`gateway-manual`) and Paper's (`lobby-6yw2`):

```
[15:04:49 INFO]: [connected player] paul_wtf (/10.244.0.1:50113) has connected
[15:04:49 INFO]: [server connection] paul_wtf -> lobby-6yw2 has connected
[15:04:49 INFO]: UUID of player paul_wtf is 836fe395-9e8b-4985-b8c9-cc93afe43995
[15:04:50 INFO]: paul_wtf joined the game
[15:04:50 INFO]: paul_wtf[/10.244.0.1:35400] logged in with entity id 16 at ([minecraft:overworld]-21.5, 71.0, 40.5)
```

**The UUID is the artifact, and it reads as one against the probe's.**
`836fe395-9e8b-4985-b8c9-cc93afe43995` is version 4 — the `4` leading the
third group — a UUID Mojang minted and handed back only after the client
proved its session. `spawnery_probe`'s `bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f`
is version 3, the name-derived offline form, which proves nothing about who
connected. The two sit side by side in the same cluster's logs, an hour
apart, and the difference between them is the whole of what
`online-mode: true` buys. `paul_wtf joined the game` is the second half of
it: unlike the held probe, this client completed the configuration phase, so
Paper counted it and `server/lobby-6yw2` showed `PLAYERS 1`.

**Criterion 9 — deleting a `Server` moves a connected player rather than
disconnecting them. PROVEN, manually, on that same live player**, which is
the only way it could be proven at all (see the finding above). `kubectl
delete server lobby-6yw2` while the account was in the game:

```
15:05:25  DeletionRequested  server/lobby-6yw2  phase Ready -> Draining: deletion requested, moving players off
15:05:25  [gateway-manual]   [server connection] paul_wtf -> hub-tmdd has connected
15:05:25  [gateway-manual]   [server connection] paul_wtf -> lobby-6yw2 has disconnected
15:05:26  [hub-tmdd]         UUID of player paul_wtf is 836fe395-9e8b-4985-b8c9-cc93afe43995
15:05:26  [hub-tmdd]         paul_wtf joined the game
15:05:26  [hub-tmdd]         paul_wtf[/10.244.0.1:49170] logged in with entity id 29 at ([minecraft:overworld]-92.5, 73.0, -180.5)
15:05:30  PodDeleted         server/lobby-6yw2  deleted pod lobby-6yw2: no players left
15:05:30  Drained            server/lobby-6yw2  phase Draining -> Terminating: no players left
```

**Three things in that sequence carry the proof, and each is worth naming.**

1. **The new connection precedes the old one's close**, in Velocity's own
   log and in that order. That is a move, not a reconnect after a drop.
2. **`no players left` arrives *after* the move, not during it.** The
   2026-08-12 failure logged the identical message while the player was still
   attached — same words, opposite meaning. Here the drain waited, because
   `Server.status.players` actually held the player this time, which is
   precisely the count the held probe could never produce.
3. **The player saw no disconnect screen**, reported by the person driving
   the client. The logs prove what the proxy did; only they could attest to
   what the game showed, and it showed an uninterrupted session that woke up
   in a different world.

The move landed in `hub`, not in another `lobby` server — the fall-through
§8a describes: `lobby` held exactly one server, `Router.choose`'s exclusion
emptied that group, and the try list went on to the second one rather than
giving up. `agent/velocity/.../Drain.kt` logged no `spawnery:` line at all,
which is its documented silence on success. The `ServerGroup` then brought
`lobby` back to `minReplicas` on its own as `lobby-svq7`.

**Milestone 3's acceptance is therefore closed in full**: criteria 7, 8 and 9
are all proven against a real cluster. What is *not* closed by this session is
finding 2 above — a player connected at the proxy but not yet counted by the
backend still sits outside `Occupied()`'s protection. A real client crosses
that window in a single round trip, which is why this session succeeded where
the held probe failed; the window is narrow, not absent, and deciding what to
do about it remains milestone 4's.

## The one contract change milestone 4 has to make

**Read this against "4c-1 has landed" above before acting on it.** 4c-1
answered this section and did not make the change it predicts: the registry was
left alone deliberately, because a proxy's readiness already lives in the pod
condition the `Service` obeys. The diagnosis below is still worth reading — it
is what led to the message that did land — but its conclusion, that this is a
milestone 2a change spanning the registry, is wrong. The section is kept as 3c
wrote it rather than rewritten, so that what was predicted and what was
measured can be compared.

`internal/agent/registry.go` cannot express "connected, but no longer
ready." `Registry.MarkReady` is only ever called on `Hello{ready:true}` or
the standalone `Ready` message; `Hello{ready:false}` is a no-op once
readiness has latched (`docs/known-issues.md`, the milestone 2c precondition
this repeats because milestone 4 is where it stops being avoidable). Milestone
2c's Paper agent never needed to lower readiness — a server latches ready and
stays that way even if its stream later breaks — and 3a built the proxy's
readiness the same way on purpose: "a proxy's readiness startup-only: once
ready, a proxy stays ready even if its stream later breaks" (design §3, §6.6).
3c inherited that and did not change it: `ReadyGate.open()` is reachable only
from the first `FullSync`, `ReadyGate.close()` only from `onShutdown`, and
nothing in `ProxyRole` ever asks the gate to close while the proxy is still
running.

That is exactly backwards from what proxy drain needs. Draining a proxy means:
stop sending it new players while it still serves the ones already
connected — which is "connected, but no longer ready" stated plainly, the
same shape `Hello{ready:false}` cannot express for a server agent and the
same reason it was left unfixed there. Milestone 4 cannot work around this
the way 3a and 3c did by simply not needing it; a `ProxyGroup` that scales
down or rolls an update has no way today to take a proxy out of a Service's
endpoints without disconnecting everyone on it in the same step; see
"`ProxyGroupReconciler.pods()` has no expectations tracking" in
`docs/known-issues.md` for the concrete failure this produces today.

The shape of the fix is a milestone 2a change, not a milestone 4-local one:
`internal/agent/registry.go`'s entry needs a way to carry "ready" separately
from "connected" so a proxy can lower the former without dropping the
latter, `internal/agentserver` needs a message or a field that lets an agent
say it, and the Velocity agent needs to call `ReadyGate.close()` from
somewhere other than shutdown — on receipt of that message, most plausibly a
new `OperatorToProxy` case sent when the operator decides a `ProxyGroup` is
draining a specific pod. None of that exists yet; all of it is milestone 4's
to design.

## What 3c leaves open, briefly

`docs/known-issues.md`'s "From milestone 3c" section is the full list; the
entries most relevant to this milestone's own scope, restated in one line
each:

- **Per-proxy load balancing.** With several proxies, placement is even per
  proxy and not necessarily across the network — `Router` only ever sees the
  players Velocity itself can see. Worth revisiting once milestone 4 makes
  proxy replica counts move.
- **The NetworkPolicy restricting backends to proxies-only is overdue, not
  deferred**, now that `online-mode=false` on the backends and forwarding
  actually working make the invariant it would guard real. Milestone 6 owns
  it, but a scaling milestone that adds and removes pods more often is where
  the exposure gets exercised more, not less.
- **The ready port is spelled in two languages** —
  `internal/podspec.ProxyReadyPort` and a Kotlin constant in
  `agent/velocity` — with nothing that fails if they diverge except the
  level-2 harness, and only when it runs.
- **A proxy that cannot bind its ready port is silent on the CR**; `Pending`
  with the reason only in the container log. Anyone building milestone 4's
  drain signalling on top of `ProxyGroup.status` should notice this gap
  rather than assume the status already carries every failure mode a proxy
  pod can hit.

## What 3c built that milestone 4 gets almost for free

**Backend drain already routes through the proxy correctly**, and milestone
4 does not have to touch it to add proxy drain on top. `agent/velocity/.../Drain.kt`
receives `DrainPlayers{fromServer, toGroups}` on every repeated send — the
operator resends it alongside `FullSync` roughly every 30 seconds for as long
as a `Server` keeps draining — and re-reads each player's current server on
every call rather than trusting a cached list, which is what makes a dropped
message or an operator restart mid-drain safe: a repeat that finds nobody
still on `fromServer` moves nobody. `Router.choose` is the same code path a
join uses, so a drain target is chosen by the identical rule a join would
have used, not a separate policy that can silently disagree.

**`ServerDirectory` and `ProxyRegistry` are the seam a proxy-drain signal
would arrive through.** Both already exist as the mechanism that keeps
Velocity's own server registry in step with the operator's, driven entirely
from the gRPC callback thread `SessionLoop` runs on. A new `OperatorToProxy`
case telling a proxy to lower its own readiness would be one more branch in
`ProxyRole.apply`, in the same shape `DRAIN_PLAYERS` already is — not a new
subsystem.

**`ReadyGate` already has the primitive milestone 4 needs on the proxy
side**, `close()`, and it is already correct: idempotent, safe to call on a
gate never opened, and synchronized against the accept loop so a close racing
an open cannot leak a bound socket. What is missing is only ever calling it
from somewhere other than shutdown — see "The one contract change" above.

## The environment

```bash
nix develop        # Go, controller-gen, protoc, envtest assets, kubectl, kind, k3d, JDK 21, Gradle
make test           # Go only; must be green before anything is touched
make agent          # both agent Gradle subprojects and their JUnit suites
make agent-test     # both agents against the stub operator, in the real images
make image-test     # both images offline, under the pod spec's constraints
make image-repro    # both images, rebuilt and compared byte for byte
```

**A container runtime is required** for every target above except `make
test`, and the image targets only work on `x86_64-linux`. `docs/known-issues.md`
records the Podman-under-`kind` story in full; nothing about it changed in
3c.

**`agent/common`, `agent/paper` and `agent/velocity` are versioned
together, not apart** — the decision recorded in `docs/handover-milestone-3.md`
under "Questions worth settling before code" and made permanent by 3c's
Gradle split. A change to `agent/common`'s session loop is a change both
agents ship on their next build, whether or not the other agent's own code
moved.

## Questions worth settling before code

- **What message carries a proxy's own drain signal, and who decides to send
  it?** `DrainPlayers` already exists for backends and is the wrong shape
  reused: a backend drains because a `Server` is being deleted; a proxy
  drains because its own pod is being removed by a scale-down or a rolling
  update, which is a `ProxyGroupReconciler` decision, not a `Server`
  controller one. Whether this is a new message, a new field on an existing
  one, or a repurposing of a field `ProxyMessage.Hello` already reserves is
  open.
- **Does a draining proxy still receive `FullSync` and `RegisterServer`?**
  Its own players still need to be moved off it — the same `Router.choose`
  and `Drain` machinery a backend drain already uses — which means the
  proxy's own server list has to stay current for exactly as long as it is
  still routing anyone. A drain that also stops the server-list stream would
  strand whoever it has not yet moved.
- **What does `ProxyGroup.status` show while a proxy is draining, given the
  ready-port bind failure is already silent on the CR today?** Milestone 4
  is a natural place to close both gaps in the same change rather than adding
  a second kind of silence next to the first.
