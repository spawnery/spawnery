# Milestone 4c-2: proxy rolling updates

**Status:** design of record. Written 2026-08-14, after 4c-1 merged.

## 1. What this builds

Today, changing a `ProxyGroup`'s image, resources, scheduling or config changes
nothing that is running. The operator writes the new spec, the CR reports
`Ready`, and every proxy pod goes on serving the old one. A security fix to the
proxy image reaches the cluster only if someone deletes the pods by hand.

4c-2 makes a spec change replace the group's proxies: the operator brings up a
proxy of the new shape, waits for it to be ready, then withdraws readiness from
one old one — and from there **4c-1's contract runs unchanged**. The pod goes
`NotReady`, the `Service` drops its endpoint, the players already on it keep
playing until they leave, and `spec.drain.timeoutSeconds` bounds the wait.

That is the whole shape of it. **4c-2 adds nothing to the drain; it creates one
from a new occasion.** Everything this milestone must get right is upstream of
the drain: which pods are out of date, which one goes next, and when.

The master design (`2026-08-07-minecraft-cloud-operator-design.md` §6.2) already
states the rule this implements:

> **Proxy replacement** runs one at a time: the new proxy becomes ready, then
> the old one goes `NotReady` and disappears from the LoadBalancer endpoints.
> Existing connections run out until the proxy is empty or
> `drain.timeoutSeconds` expires; remaining players are then disconnected.
> Unlike the server drain there is no active moving — the client connection
> terminates at the proxy.

## 2. What 4c-1 leaves in place, and what it does not

**In place, and reused as-is:**

- `SetReady` on the wire, and `Fleet.SetReady`'s per-session memo, which
  re-asserts on reconnect and on every `Resync`.
- The agent's ready gate, ordered against its latch under one monitor.
- The `spawnery.cloud/draining-since` annotation, written once per pod.
- The wait for *empty* — a fresh, zero player count — and the deadline that
  bounds it, with its `Warning ProxyDrainTimeout` event.
- `status.connectedPlayers`, which counts players on draining pods.

**Not in place, and this milestone's:**

- Nothing distinguishes a pod that matches the spec from one that does not.
- The deletion loop selects by *position* (`for i := len(pods)-1; i >= replicas`),
  which cannot express "this particular pod is out of date".
- `ProxyGroupReconciler.pods()` has no create/delete reservation, so the create
  path races the informer cache.
- The operator holds both the readiness it asserted and `isPodReady()` in one
  loop and never compares them.

## 3. Design

### 3.1 Staleness is a hash of the rendered pod

A pod carries a label whose value is a hash of the pod the operator *would*
build for this group right now. A pod is stale when its label differs from that
hash.

```
spawnery.cloud/pod-hash: <short hash>
```

The hash is taken over the output of `podspec.BuildProxyPod(network, group,
name, agentEndpoint)` with the pod's **name zeroed**, serialised deterministically
(`encoding/json` sorts map keys, so labels and annotations do not flap).

**Why the rendered pod and not a list of spec fields.** The operator renders the
desired pod anyway; hashing that output means no field can be forgotten,
because the hash covers exactly what gets created. A hand-picked field list has
the opposite failure: someone adds a spec field, does not extend the hash, and
builds a change that never rolls out and never says so. This repository counted
eleven instances of that defect class in 4c-1 alone — a claim that outlives what
the code beneath it does. A self-maintaining hash removes the opportunity.

> **Amended during implementation (4c-2).** "No field can be forgotten" is true
> of the rendered *pod* and not of the group's rendered state, and the
> difference turned out to matter. The pod is not the only rendered artefact:
> `reconcileConfigMap` renders a second one, and the pod references it by name
> in a projected volume rather than embedding it. So a field that reaches only
> the ConfigMap — `spec.config.motd`, `spec.config.onlineMode` — changes
> nothing the digest can see, and its sibling `spec.config.playerLimit` rolls
> the group because it reaches the pod as an env var. Three fields under one
> stanza, two behaviours. The hash is self-maintaining over its own input; what
> is not automatic is the choice of input. `known-issues.md` records the
> boundary and the operator instruction; hashing the rendered configuration
> alongside the rendered pod is left to a later milestone, because deciding
> when a ConfigMap edit rolls is a milestone's worth of decisions on its own.

**Why not `metadata.generation`, which 4b uses.** `known-issues.md` records
4b's cost under "From milestone 4b": *"Any spec change begins a changeover.
`metadata.generation` moves on every edit, so tuning `minReplicas`,
`spareSlots` or `maxReplicas` marks every running server stale."* For a
`ProxyGroup` that would be worse than untidy. Changing `replicas` is the
routine operation on a proxy group, and under a generation rule every scale-up
and scale-down would trigger a full replacement — each pod waiting out an
attrition-bound drain and disconnecting whoever is left at the deadline.
Scaling must stay scaling.

**The accepted cost, which belongs in `known-issues.md`.** A change to the
*rendering code* — a new default, an added env var, a renamed label — changes
the hash for every group without any spec being edited. The next operator
upgrade will therefore find the whole fleet stale and roll it, and at the end
of each drain the deadline disconnects whoever remains. This is the price of a
hash that cannot forget a field, and it is a real operational hazard, not a
footnote: an operator upgrade becomes a fleet-wide proxy roll. It must be
written down where an operator will meet it before it happens.

### 3.2 The rollout decision is a pure function

Sizing and replacement decide together, over a list of proxy views, with no
client and no cluster:

```go
type ProxyView struct {
    Name          string
    Stale         bool
    Ready         bool
    Draining      bool      // carries the draining-since annotation
    Players       int32
    PlayersStale  bool      // the count could not be trusted
    CreatedAt     time.Time
}

type RolloutDecision struct {
    Create int32    // how many new-generation pods to create
    Drain  []string // pods to mark draining, this pass
}

func DecideRollout(pods []ProxyView, replicas int32) RolloutDecision
```

Pure, table-tested, in the shape `DecideSize` established in 4a and which held
up through 4b and 4d. The reconciler carries the decision out; it does not make
it.

**First, the target size.** One line decides it, and getting it wrong is how a
rollout marks two pods at once:

```
surge  = 1 if any pod is stale, else 0
target = replicas + surge
```

**`surge` is 1 while any stale pod exists — including one that is already
draining.** That is what keeps the count stable through the cycle. Were surge
to drop to 0 the moment a pod was marked, the group would stand at
`replicas + 1` against a target of `replicas`, and the surplus rule below would
immediately mark a second pod: a rolling update that drains the whole group at
once, which is precisely what "one at a time" forbids.

> **Amended during implementation (4c-2): the conclusion holds, this reason
> does not.** It depends on the rule order printed below, and the code ships
> rules 2 and 3 the other way round — see the next note. With the guard ahead
> of the surplus rule, a group standing at `replicas + 1` with one pod marked
> returns at the guard and the surplus rule is never reached, so that path is
> closed with surge or without it.
>
> What surge outliving the mark actually buys is rule 1, which is checked
> *before* the guard. With surge dropped, a group whose surge pod dies while
> the old one is still draining stands at `replicas` against a target of
> `replicas`, builds no replacement, and drops to `replicas - 1` ready when the
> draining pod goes. `TestDecideRollout`'s "the surge pod dying mid-drain is
> replaced, because surge outlives the mark" pins it, and
> `docs/handover-milestone-4.md` carries the full derivation.

**Then the rules, in order:**

1. **Fewer pods than `target`** — create the difference. This covers cold
   start, plain scale-up, and the surge pod that begins each replacement; they
   are the same case and need no separate branch.
2. **More pods than `target`** — mark the surplus draining, chosen by §3.4.
   With surge accounted for, this fires only for a genuine scale-down.
3. **Something is already draining** — mark nothing further. This is "one at a
   time", and it is one guard rather than a counter.
4. **A current-generation pod beyond `replicas` is `Ready`, stale pods exist,
   nothing is draining** — mark **one** stale pod draining, chosen by §3.4.

> **Amended during implementation (4c-2), three ways.**
>
> **Rules 2 and 3 ship swapped: the guard runs first.** Under the order printed
> above, a scale-down issued while a replacement drain is in flight reaches the
> surplus rule and marks a second pod while one is already going — the
> all-at-once outcome rule 3 exists to prevent, arriving through rule 2. Put
> the guard first and the drain in flight is finished before any surplus is
> decided. The reorder is why the surge rationale above needed correcting; it
> is the code that is right.
>
> **Rule 4 counts ready pods of any generation, not current-generation ones.**
> The gate that shipped is `readyBeyond`: strictly more ready, non-draining
> pods than `replicas`, stale and current alike. What protects a player is a
> ready proxy and not which generation supplies it, and the distinction is load
> bearing for a reverted spec, where the pod holding the spare capacity is the
> stale one.
>
> **Rule 4 has a second disjunct: a stale pod that is not `Ready` may go
> regardless.** Written as stated, rule 4 has a state it cannot leave. A stale
> pod that never becomes `Ready` counts towards `stale`, so it holds the surge
> open and keeps the group at `target`; it contributes nothing to the ready
> count, so it can never supply the capacity the rule waits for. Every rule
> declines, none changes the state they declined on, and the rollout stops —
> silently, with `observedGeneration` advanced and the phase `Ready`. The way
> in is the ordinary one: an operator bumps `spec.image` because a proxy is
> crashlooping, and the crashlooping proxy deadlocks the roll issued to replace
> it. Retiring a pod the kubelet does not call `Ready` costs zero ready
> capacity, so the rule permits it, and §3.4's order was extended to match so
> that the pod selected under the new disjunct is that pod.

Rule 3 makes the sequence self-limiting: the cycle only advances when 4c-1's
deletion loop removes the draining pod, which happens when it is empty or when
its deadline expires. Rule 4's wait for `Ready` is what holds ready capacity at
`replicas` — the surge pod is serving before the old one stops.

> **Amended during implementation (4c-2): that last clause is the healthy path,
> not the rule.** Rule 4 waits for ready capacity *or* for a stale pod that is
> not serving, so the surge pod is not always serving when an old pod is
> marked. At `replicas: 2` with A stale and `Ready`, B stale and unready, and
> the surge pod S itself still unready, `readyBeyond` is false and
> `anyStaleNotReady` is true, so B is marked while S is still pulling its
> image. Confirmed against the code, not derived on paper: `DecideRollout`
> returns `Create=0 Drain=[b]` for exactly that input.
>
> No ready capacity is lost by it: it is 1 before the mark and 1 after,
> because B was contributing nothing — see the note under "Ready capacity never
> dips below `replicas`" below for the exact sense in which that property still
> holds. There is a cost, and it is time rather than capacity: B waits out
> `drain.timeoutSeconds` before deletion, because a pod whose agent stream is
> down has an untrusted count and is not "known empty".
>
> What the surge wait buys is that a *serving* pod is not stopped before its
> replacement is serving, and that survives by construction rather than by
> luck. The new disjunct fires only when a stale pod is not serving, and §3.4's
> readiness clause guarantees that is the pod handed back — so the mark it
> permits always lands on a pod behind no Service endpoint. A is not marked
> here; B is.
>
> The walk-through that follows is therefore the ready path — the ordinary
> rollout where every pod comes up healthy. It is still the right thing to read
> first. It is no longer the only sequence rule 4 admits.

Walking a group of two through it: both stale, so target is 3 and rule 1
creates one. It comes up; nothing changes while it is unready. It turns
`Ready`, and rule 4 marks one stale pod. Stale pods still exist, so target
stays 3 and nothing else is created or marked. The marked pod empties and 4c-1
deletes it, leaving two — one stale, one current — and rule 1 creates the next.
When the last stale pod goes, `surge` falls to 0, target returns to `replicas`,
and the group rests at two current pods.

**Ready capacity never dips below `replicas`.** That is the property the surge
exists for and the one the tests must state directly rather than infer from a
pod count.

> **Read precisely, after the rule-4 amendment above.** The property is about
> what a mark this function makes *costs*: no decision here lowers ready
> capacity below `replicas`. It is not a claim that a group always has
> `replicas` ready pods, which nothing in an operator can promise — a pod that
> crashloops takes the group below its count without anything here having
> decided anything. That is exactly why the new disjunct is sound: marking a
> pod that is not `Ready` subtracts zero from a count that is already whatever
> the cluster made it.

### 3.3 The annotation becomes the carrier of intent

4c-1's deletion loop walks the tail by index. A stale pod is not necessarily in
the tail — it may be the oldest pod in the group — so position can no longer
express which pod is going.

**`spawnery.cloud/draining-since` becomes the marker of intent**, and both loops
derive from it:

- The readiness assertion loop asserts `SetReady(!draining)` per pod, where
  `draining` is "carries the annotation", not "index ≥ replicas".
- The deletion loop iterates the pods that carry the annotation, in any order,
  applying 4c-1's rule unchanged: delete when the count is fresh and zero, or
  when the deadline has expired.

A surplus pod and a stale pod become the same case, distinguished only by what
caused the mark. This removes a coupling rather than adding a mechanism: after
this change, nothing about a pod's fate depends on where it sits in a sorted
list.

**A cancelled rollout reverts.** If the spec is edited back before a marked pod
is deleted, that pod is no longer stale, its mark is removed, and readiness is
re-asserted as `true` — the same path 4c-1 already has for a cancelled
scale-down, which is already tested.

### 3.4 One selection rule, for every reason a pod goes

When a pod must be marked, sort by:

1. **Stale before current.** Stale pods have to go regardless; taking a current
   one first would mean draining two pods for one replacement.
2. **Fewest players first.** The emptiest pod finishes soonest and disconnects
   fewest people at the deadline.
3. **An untrusted count sorts last.** Same rule 4c-1 applies to deletion:
   unknown counts as occupied. A pod whose agent stream is down may hold
   players nobody can see.
4. **Oldest first** as the tie-break, so the order is deterministic and a test
   can name the expected pod rather than counting survivors.

The rule replaces "newest first" for scale-down as well. The old rationale —
an older proxy has had longer to collect players — was a proxy for the player
count, which the operator now has directly.

**Selection cannot oscillate.** The annotation is written once and never moved,
so a pod chosen on one pass stays chosen even if the counts change on the next.

> **Amended during implementation (4c-2).** Three differences; the first was
> found by the whole-branch review, the other two while reconciling this
> section against `pick` at HEAD.
>
> **A readiness clause was added, between 1 and 2: not-`Ready` before
> `Ready`.** A pod the kubelet does not call `Ready` is behind no Service
> endpoint, so retiring it costs zero ready capacity — the cheapest thing
> there is to retire. It sits *above* the player clauses deliberately: a fallen
> pod's last reported count is what it held before it fell over, so ranking it
> on that figure ranks it on a number that has stopped meaning anything. This
> is the half of the §3.2 rule-4 fix that decides *which* pod goes; without it
> that rule's new disjunct would permit a mark and then hand back a ready pod.
>
> **The age tie-break ships newest-first, not oldest-first.** Rule 4 above says
> oldest; the code sorts `CreatedAt.After`. The reasoning is in `pick`'s doc
> comment and it is not symmetry: age is a stand-in for the occupancy the
> operator cannot see, and the case where it carries any information is the one
> where every count is untrusted — an operator that has just restarted, or a
> fleet whose agent streams are all down. Marking the oldest there picks the
> pod most likely to be occupied. Between two counts that are both trusted and
> equal it decides nothing worth having either way, and only keeps the order
> deterministic. Note this makes the paragraph above the list only half
> right: "newest first" was dropped as the *primary* rule for scale-down, since
> the player count is now had directly, but it survives as the tie-break for
> exactly the case where that count is unavailable.
>
> **The annotation is not written once and never moved.** The surplus-release
> loop in `reconcileReplicas` hands a mark back when a scale-down is cancelled
> or partly reversed, and `markDraining` deletes the annotation to do it —
> `TestACancelledScaleDownPutsTheProxyBack` pins that. Readiness is derived on
> every pass rather than remembered, which is what makes a drain cancellable at
> all; a persisted mark would leave a pod `NotReady` until it emptied and then
> replace it with one of the same shape. Oscillation is still ruled out, but by
> a different argument than the one printed here: a released pod loses its
> annotation on the same pass, so it is not `Draining` on the next one and
> drops out of the candidate set, and nothing but a fresh decision can re-mark
> it — which `DecideRollout` makes none of while anything is draining. Note
> also the cost of a release, recorded at the call site: the pod's deadline
> restarts from zero if it is marked again.

### 3.5 Pacing: the existing deadline, reused

A stale pod is treated exactly as a surplus one, including
`spec.drain.timeoutSeconds`. A full rollout therefore takes at most
`replicas × drainTimeout` in the worst case, and usually far less, because a
proxy that empties early is deleted early.

No new field, no second meaning for "deadline", and the path is the one proved
against a real cluster on 2026-08-14 with a real client. `maxStaleSeconds`,
which 4b needed for servers, has no analogue here: for a server the second stage
means *moving* players, and for a proxy it can only mean disconnecting them,
which the deadline already does.

### 3.6 Readiness divergence is reported, not repaired

The operator already holds, in one loop, both the readiness it asserted and the
pod's actual `Ready` condition. When they disagree for longer than a grace
period, it sets a condition on the group and fires one event on the flank.

```
ConditionReadinessDiverged = "ReadinessDiverged"   // api/v1alpha1/common_types.go
```

- **True** while at least one pod's `isPodReady()` has disagreed with the
  asserted value for longer than the grace period. The message names the pod.
- **False** otherwise.

> **Amended during implementation (4c-2): narrowed to the withdrawal
> direction.** "Disagreed with the asserted value" is unqualified here, so it
> reads bidirectionally. What shipped is `going && isPodReady(pod)` — a pod
> just told to stop taking connections that the kubelet still calls `Ready`.
>
> The reverse direction, asserted ready but not yet actually `Ready`, is
> deliberately not checked. `SetReady(true)` is asserted for a non-draining pod
> from the moment it exists, before any kubelet has probed it once, and a cold
> pull of the Velocity image outruns the 60s grace easily. Reporting that would
> name a perfectly behaving pod and tell the operator its agent heard an
> instruction and ignored it — a misdiagnosis, not noise, sending somebody
> hunting a broken build while the kubelet is still pulling. The withdrawal
> direction is the one this section's own "why report rather than repair"
> argument is about. The other direction is already visible: the group sits
> below its ready count, and `known-issues.md` carries 3c's
> "a proxy that cannot bind its ready port is silent on the CR" for the
> never-turns-ready case.
>
> Recorded as a narrowing of this section's letter rather than dressed up as a
> clarification. If the bidirectional form can be had safely — by counting only
> a pod that has been `Ready` at least once — that is a later milestone's to
> weigh.
>
> One addition too, to the third bullet above: the event fires on *both*
> flanks, not only false→true. A `Warning ReadinessDiverged` going in, and a
> `Normal ReadinessAgrees` coming back out, so a divergence that clears says so
> rather than leaving the last word on the object a Warning. The comparison is
> still 4a's `ScalingLimited` shape.
- One `Warning` event on the false→true edge, compared the way 4a's
  `ScalingLimited` does it — `meta.IsStatusConditionTrue` before and after
  `SetStatusCondition`, because that call only moves `lastTransitionTime` on an
  actual change of status.

**Grace period: 60 seconds, a constant in the code.** It must clear both known
delays: the kubelet's probe takes 10–15s to flip a condition (period 5s ×
failure threshold 3), and `Resync` re-asserts every 30s. 60s clears both with
margin. A constant rather than a CRD field, the same decision 4d made for its
backoff numbers.

**Why report rather than repair.** 4c-1's `Resync` already re-sends the last
asserted readiness on every tick, so a divergence caused by a *lost message*
heals itself within one interval. What survives that is an agent that received
the message and did not act — a broken build, a leaked socket — and re-sending
cannot fix it. What an operator needs there is to be told, before the deadline
disconnects players from a proxy that never stopped taking them.

### 3.7 `expectations` for the proxy create path

`ProxyGroupReconciler.pods()` reads the informer cache. A pod created on one
pass may not appear on the next, so the reconciler can create a second pod for
the same slot. 4c-2 makes this materially more likely: a rollout creates a pod
per replacement rather than only at scale-up.

`internal/controller/expectations.go` is the mechanism, unchanged in design —
name-keyed reservations with a 30s TTL, the ReplicaSet controller's own
approach. Only *create* and *delete* apply; there is no proxy analogue of
`expectationRetire`.

**Shape.** `observe` is typed to `[]ServerView` and reads `leaving()` and
`Retire`. Rather than making it generic, add a second, narrow method for pods
that shares the expiry logic. Two small methods that each read clearly beat one
generic method that must explain why a third of its cases do not exist for half
its callers.

## 4. Out of scope, deliberately

- **`maxSurge` as a CRD field.** The overhang is fixed at 1, as in 4b. A group
  of twenty proxies rolls slowly; the fix for that is a knob someone can ask
  for once the slowness is real.
- **Any change to the wire contract.** `make agent-test` needs no extension:
  4c-2 creates a drain from a new occasion and changes nothing the agent sees.
- **Node drain**, which is 4c-3's and depends on none of this.
- **Moving players between proxies.** Impossible by construction — the client's
  connection terminates at the proxy being replaced.

## 5. Error handling

- **A pod that vanishes mid-pass** is tolerated, as 4c-1 already does:
  `client.IgnoreNotFound` on both the annotation patch and the delete.
- **A create that fails** leaves the group short; the next pass tries again.
  4d's per-group backoff already covers a group that cannot start a pod.
- **An unparsable `draining-since`** is re-stamped rather than fatal, as 4c-1
  fixed: one pod's corrupt annotation must not stop the group converging.
- **A hash that cannot be computed** — `BuildProxyPod` returning an error —
  aborts the pass with the error, because no pod's staleness can be judged
  without it. This is the one place where failing the whole reconcile is
  correct, and the reason belongs in a comment.

## 6. Testing

**Unit, no cluster.** `DecideRollout` gets the table: cold start, plain scale-up
and scale-down, a rollout from all-stale, a rollout with one already draining,
a cancelled rollout, a scale-down during a rollout, ties broken by age, and an
untrusted count sorting last. The pure function is where the rules are proved.

**envtest.** The sequencing the pure function cannot show: that the surge pod is
created before any mark is written; that the mark waits for the surge pod to be
`Ready`; that ready capacity never falls below `replicas` across a full rollout;
that a second replacement does not begin while one is draining; that a rollout
finishes and leaves exactly `replicas` current-generation pods; and that the
`ReadinessDiverged` condition and its event fire once on the flank.

**Every test whose expectations move gets its mutation run for real and the
output reported.** Non-negotiable: on the last four milestones this is what
caught tests whose names had outlived their fixtures.

**Evidence, appended to `runbook-milestone-4c1-evidence.md`** rather than a new
document: with a client connected, change the group's image, and confirm the
session survives the replacement and the group ends on the new hash. Same setup
as 4c-1's two runs; only the trigger differs.

## 7. Acceptance criteria

1. Changing a `ProxyGroup`'s image replaces every proxy pod, and the group ends
   with `replicas` pods all carrying the current hash.
2. Changing `spec.replicas` alone rolls nothing: the hash does not move.
3. No decision this milestone makes lowers ready capacity below `replicas`
   during a rollout. **Amended during implementation (4c-2)** — it read "Ready
   capacity never falls below `replicas`", which is not a criterion an operator
   can meet: a proxy that crashloops takes the group below its count without
   anything here having decided anything, and rule 4's second disjunct
   deliberately marks such a pod, subtracting zero from a count the cluster
   already set. See the two notes in §3.2.
4. At most one pod is draining at any moment during a rollout.
5. A player connected to a replaced proxy keeps playing until they leave or the
   drain deadline expires — proved on a real cluster with a real client.
6. Editing the spec back before a marked pod is deleted cancels its drain and
   restores its readiness.
7. The scale-down selection prefers a proxy that is not `Ready`, then the
   emptiest, and an untrusted count is treated as occupied. **Amended during
   implementation (4c-2)** — it read "prefers the emptiest proxy", which the
   readiness clause added to §3.4 makes false as stated, and not only in a
   rollout: with no staleness anywhere, a plain scale-down from 3 to 2 over an
   empty `Ready` pod, a `Ready` pod with 3 players and an unready pod with 7
   returns the *unready* one. Checked against the code rather than reasoned
   about. That is the intended order — a pod behind no Service endpoint costs
   nothing to retire — but "emptiest" alone no longer describes it.
8. A pod that is still `Ready` 60 seconds after the operator withdrew its
   readiness sets `ReadinessDiverged` and fires one `Warning` naming the pod;
   the condition clearing fires a `Normal`. **Amended during implementation
   (4c-2)** — it read "whose readiness disagrees with the asserted value",
   which is bidirectional, and named only the `Warning`. Both were narrowed and
   widened respectively in §3.6; see the note there for why the
   asserted-ready-but-not-yet-`Ready` direction is deliberately not checked.
9. The create path no longer double-creates when the informer cache lags.
10. `make manifests` produces **no** diff. This milestone adds no CRD field: the
    hash is a pod label, the condition type is a Go constant, and
    `ProxyGroupStatus.Conditions` already exists.
