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

Rule 3 makes the sequence self-limiting: the cycle only advances when 4c-1's
deletion loop removes the draining pod, which happens when it is empty or when
its deadline expires. Rule 4's wait for `Ready` is what holds ready capacity at
`replicas` — the surge pod is serving before the old one stops.

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
3. Ready capacity never falls below `replicas` during a rollout.
4. At most one pod is draining at any moment during a rollout.
5. A player connected to a replaced proxy keeps playing until they leave or the
   drain deadline expires — proved on a real cluster with a real client.
6. Editing the spec back before a marked pod is deleted cancels its drain and
   restores its readiness.
7. The scale-down selection prefers the emptiest proxy, and an untrusted count
   is treated as occupied.
8. A pod whose readiness disagrees with the asserted value for 60 seconds sets
   `ReadinessDiverged` and fires one `Warning` naming the pod.
9. The create path no longer double-creates when the informer cache lags.
10. `make manifests` produces **no** diff. This milestone adds no CRD field: the
    hash is a pod label, the condition type is a Go constant, and
    `ProxyGroupStatus.Conditions` already exists.
