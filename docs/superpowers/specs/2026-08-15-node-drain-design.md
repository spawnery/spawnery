# Milestone 4c-3: node drain

**Status:** design of record. Written 2026-08-15, after 4c-2 merged.

## 1. What this builds

Today the operator does not read `Node` objects. Nothing in `internal/` or
`cmd/` looks at one, the ClusterRole grants no verb on them, and no controller
has a watch. A node being taken out of service is therefore invisible to
Spawnery, and what happens next depends on which kind of pod sits on it:

- A **server pod with players** is protected by its group's
  PodDisruptionBudget, sized from the `spawnery.cloud/occupied` label
  (`servergroup_controller.go:712`, `server_controller.go:661`). The eviction
  API refuses, and `kubectl drain` blocks — for as long as somebody is playing,
  which may be hours. That is the load-bearing invariant working as designed
  (master design §6.2: *a pod with players is never deleted*), and also a
  drain that never finishes.
- A **proxy pod** has neither the label nor a budget. The eviction succeeds
  immediately and every player connected through that proxy is disconnected —
  the exact outcome 4c-1 and 4c-2 were built to avoid, arriving through a door
  neither of them watches.

4c-3 closes both. When a node is on its way out, the operator proactively
empties Spawnery's pods off it, using drains that already exist and are already
proven, so that `kubectl drain` completes without anybody being kicked.

The bound on how long that takes is the drain deadline per side — 60 seconds
for servers, 300 for proxies — and it holds while the cluster has somewhere to
put the replacements. It does not hold when it has not: §3.4 explains why a
group with nowhere to move to waits instead, and why `kubectl drain` blocking
in that case is the right answer rather than a defect.

The master design (`2026-08-07-minecraft-cloud-operator-design.md` §5.1) states
the rule this implements:

> So that a node drain does not hang indefinitely, the operator watches for a
> node being set `unschedulable` and proactively starts the drain sequence from
> 6.2 for the affected servers. The node drain then terminates once the players
> have been moved.

This milestone extends that sentence in one direction its author did not have
available: proxies, which became drainable in 4c-1 and replaceable in 4c-2.

## 2. What 4c-1 and 4c-2 leave in place

Almost all of the machinery. 4c-3 adds no drain of its own; like 4c-2 it
creates an occasion for drains that already run.

- **Deleting a `Server` CR *is* the 6.2 sequence.** `DeletionRequested` is fed
  from exactly one source, `!srv.DeletionTimestamp.IsZero()`
  (`server_controller.go:356`), and the finalizer `spawnery.cloud/drain`
  (`server_controller.go:44`) keeps the object alive until the players are
  safe. From the deletion onwards the state machine deregisters the server,
  sends `DrainPlayers`, waits for empty or for `drain.timeoutSeconds`, and only
  then deletes the pod. `phase.Inputs`' own comment reads "or the group decided
  to remove this server" (`phase.go:93`) and should not send anyone looking for
  a second signal: the group removes a server *by* deleting its CR, so both
  halves of that sentence are the deletion timestamp.
- **A leaving server is already excluded from the group's arithmetic.**
  `isLeaving` (`candidates.go:173`) counts `Draining`, `Terminating` and
  `Retiring` as gone, which is what makes the group build a replacement without
  being told to.
- **The proxy rollout already turns "this pod must go" into a safe
  replacement.** `DecideRollout` (`rollout.go:69`) reads staleness as a single
  `bool` per pod, opens a surge of 1, waits for the replacement to be ready,
  marks one pod at a time, and hands the rest to 4c-1's drain.
- **Proxy pods already carry `cluster-autoscaler.kubernetes.io/safe-to-evict:
  "false"`** (`podspec/proxy.go:258`). That is a signal to the autoscaler and,
  as the master design says in as many words, no protection against `kubectl
  drain`.

What is missing is the node knowledge, one rule per side to consume it, and —
on the proxy side — the budget that makes the rule matter.

## 3. Design

### 3.1 What "departing" means

A node is departing when either holds:

1. `spec.unschedulable` is true. This is what `kubectl cordon` and `kubectl
   drain` set, and it is the criterion the master design names. It is not
   configurable and cannot be switched off.
2. The node carries a taint whose key appears in a configured list **and**
   whose effect is `NoSchedule` or `NoExecute`.

The list comes from a repeatable operator flag, default empty:

```
-drain-taint <key>      # repeatable; follows cmd/spawnery-stubop/main.go:148
```

**The effect is part of the test, not decoration.** A `PreferNoSchedule` taint
does not stop the scheduler placing the replacement pod back on the same node.
Treating such a node as departing would condemn a pod, build its replacement in
the same place, condemn that one on the next pass, and rotate for as long as the
taint stands. Restricting the match to the two effects that actually repel a pod
closes that loop by construction rather than by a guard somewhere downstream.

The test itself is a pure function:

```go
// IsDeparting reports whether this node is on its way out of service.
func IsDeparting(node *corev1.Node, taintKeys []string) bool
```

It takes the key list rather than reading configuration, so it is table-tested
without a cluster and without a manager — the shape `DecideSize` and
`DecideRollout` already have.

Both cluster-autoscaler and Karpenter cordon a node in addition to tainting it,
so the default empty list still sees autoscaler-driven scale-in; the flag exists
to see it a moment earlier, and to cover a taint vocabulary this project does not
have to know in advance.

### 3.2 How the operator learns it

`Watches(&corev1.Node{})` on the **ServerGroup and ProxyGroup reconcilers**,
mapping a node event onto the groups with pods on that node, plus
`nodes: get;list;watch` on the existing ClusterRole (`config/rbac/role.yaml:3`).

No new controller. Every existing decision about which pod of a group goes is
made by that group's reconciler — `DecideSize` for servers, `DecideRollout` for
proxies — and a node is a fact those decisions consume, not a second authority
over the same pods. A `NodeReconciler` deleting `Server` objects would be
writing behind the back of the `expectations` bookkeeping that exists to keep
exactly those writes straight (`expectations.go`).

Two costs, stated rather than discovered:

- **The cache now holds every `Node` in the cluster.** `status.images` alone is
  tens of kilobytes per node and nothing here reads it. A `Cache.ByObject`
  entry with a `Transform` that drops `status.images` goes in beside the
  ConfigMap and ServiceAccount restrictions already there for the same reason
  (`cmd/spawnery-operator/main.go:170`).
- **`-namespace` restricts the cache, and nodes are cluster-scoped.** Whether
  `Cache.DefaultNamespaces` (`main.go:163`) leaves cluster-scoped kinds
  cluster-wide in this controller-runtime version is to be verified against the
  vendored version during implementation, and an explicit `ByObject` entry added
  if it does not.

### 3.3 The server path: deletion is already the drain

**The group already holds the pod, so nothing new has to be stored.**
`collectViews` resolves each server's pod through `podFor`
(`servergroup_controller.go:545`) to key the registry lookup for its player
count, and `pod.Spec.NodeName` is on that object. No mirror into
`ServerStatus`, no second copy of a truth Kubernetes already keeps — the shape
`candidates.go:74` records the cost of.

`ServerView` gains `Condemned bool`, set in `collectViews` from
`pod.Spec.NodeName` and the departing set. The view carries the conclusion
rather than the node name, so `DecideSize` stays free of node vocabulary — the
same treatment `Stale` gets for player counts.

A server whose pod `podFor` does not resolve is not condemned: either it has no
pod yet, or the pod already carries a deletion timestamp and is on its way out
under its own power.

`DecideSize` gains one rule and one output field:

```go
// SizeDecision
// Condemn names the servers whose node is departing. They are deleted
// unconditionally: not bounded by Surplus, not held back by minReplicas,
// and all of them in one pass.
Condemn []string
```

Three properties of that rule, each with its reason:

- **Unconditional.** The node is leaving with or without our consent. A budget
  that declined the deletion would not keep the server running; it would only
  keep us from moving its players before somebody else's eviction moves them
  the hard way.
- **All at once.** One `drain.timeoutSeconds` window for the whole node rather
  than one per server. Draining serially would make `kubectl drain` stand for
  the sum of the windows — ten occupied servers at the 60-second server
  deadline is ten minutes of exactly the hanging this milestone exists to end.
- **Counted as leaving in the same pass.** The capacity arithmetic treats a
  condemned server the way `isLeaving` treats a draining one, so the pass that
  condemns is also the pass that asks for the replacement. The replacement
  cannot land on the departing node: a cordoned node takes no new pods, and
  §3.1 restricted taint matching to the effects that repel one.

`Condemn` is a separate field from `Delete` so that the two reasons never share
a number. `Delete` is the scale-down nomination and `Surplus` is what the
ceiling asked for; the log line comparing them (`servergroup_controller.go:493`)
is about demand, and a node drain is not about demand. Execution reuses the
existing path — `deleteServer` plus `Expectations.expectDeleted` — with its own
event reason, `NodeDraining`, in place of `ServerRemoved`.

The deletion of a condemned server runs through the same block as the others,
which is to say it is not gated by the per-group backoff. That is the existing
rule and the right one here: "the deletes and retirements below run either way:
they touch players, and must not wait on an unrelated failure"
(`servergroup_controller.go:481`).

**What this costs when a node holds a whole group.** Every server is condemned
at once, so the players are moved onto the fallback groups rather than onto the
group's own replacements, which are not ready yet. That is the nature of losing
the node those servers were on, not a choice this design makes — but it belongs
in `docs/known-issues.md` where an operator will look, not only here.

### 3.4 The proxy path: departure is a second kind of staleness

A proxy pod whose node is departing has to be replaced by a pod somewhere else,
one at a time, with a surge, without disconnecting anyone. That is the sentence
`DecideRollout` already implements. So the node feeds the existing rule rather
than a new one, at the single site where views are built
(`proxygroup_controller.go:334`):

```go
Stale: pods[i].Labels[podspec.LabelPodHash] != wantHash ||
    departing(pods[i].Spec.NodeName),
```

Everything downstream is 4c-2 unchanged: surge 1 while any pod is stale, the
replacement built before anything is marked, `pick` choosing among stale
candidates, the `draining-since` annotation carrying the intent, and the pod
deleted when it is empty or when `spec.drain.timeoutSeconds` expires.

Two consequences worth naming because a reader will ask:

- **A hash mismatch and a departing node are not ranked against each other.**
  `pick` sorts stale before current, then not-Ready first, then by players;
  within the stale set the reason for staleness does not appear. Two pods that
  are equally stale for different reasons are therefore ordered by occupancy,
  which is the property that decides who gets disconnected at a deadline. That
  is the right tiebreaker and it needs no new clause.
- **The rollout can stall on capacity, and should.** If the cluster has nowhere
  to put the replacement, it stays `Pending`, `readyBeyond` is false, and — for
  a departing pod the kubelet still calls Ready — `anyStaleNotReady` is false
  too, so nothing is marked. The group waits instead of lowering its ready
  capacity. `kubectl drain` then blocks, correctly: there is genuinely nowhere
  for those players to go.

### 3.5 The proxy budget, without which §3.4 loses the race

`kubectl drain` evicts. The operator reconciles. Against an unprotected proxy
pod the eviction wins essentially always — it is issued in the same second the
node is cordoned, while the replacement proxy needs a pod schedule, an image
start and a readiness gate. §3.4 would then apply to a pod that is already gone,
and the players with it.

So the ProxyGroup gains the mechanism the ServerGroup has had since 4b:

- **The `spawnery.cloud/occupied` label on its pods**, maintained by the
  ProxyGroup reconciler from the player counts it already reads for `pick`.
  Note the asymmetry with the server side, where the *Server* controller keeps
  the label (`server_controller.go:661`) and the group only sizes the budget:
  there is no per-proxy controller, so the group does both.
- **One PodDisruptionBudget per ProxyGroup**, selecting on that label, with
  `minAvailable` as an **absolute number** kept in step with the count of
  occupied proxy pods — the same shape and the same reason as `reconcilePDB`
  (`servergroup_controller.go:712`): for pods without a controller carrying a
  scale subresource, Kubernetes accepts neither `maxUnavailable` nor
  percentages.

The two sides of the budget must agree pod for pod, which is the rule
`isOccupied` already states for servers (`candidates.go:74`): a budget counting
fewer pods than carry the label hands the eviction API a disruption to spend on
an occupied pod. The proxy side reuses the repository's occupancy rule
unchanged, including that an untrusted count counts as occupied.

The PDB is owned by the ProxyGroup (`Owns(&policyv1.PodDisruptionBudget{})`),
and the ClusterRole already grants the verbs.

### 3.6 Uncordon: begun stays begun

If the node is released while the work is in flight, a proxy already marked is
drained and replaced anyway; only pods not yet marked stop being stale. The
server side cannot do otherwise — its CR is deleted and the finalizer is
counting players, not reasons — and holding both sides to one answer is worth
more than the pod a reversal would save. An accidental `cordon` costs one proxy
rotation and no player.

This is expected to fall out of the existing release path rather than needing a
clause: once marked, the pod is draining and not Ready, `pick` sorts it first,
and the surplus branch keeps the mark on what `pick` returns
(`rollout.go:209ff`). **Expected, not established** — the implementation proves
it with a table case rather than asserting it.

### 3.7 Reporting

One condition per group, `NodeDraining`, true while the group has pods on
departing nodes, with the node names in the message — the form 4c-2 chose for
`ReadinessDiverged`. One event at the moment of decision, reason `NodeDraining`,
on the ServerGroup when a server is condemned and on the ProxyGroup when a proxy
is marked for a departing node. The event fires on the transition, not on every
pass, following `retireServer`'s guard against re-eventing a decision the cache
has not caught up with (`servergroup_controller.go:669`).

## 4. Out of scope, deliberately

- **Nodes that fail rather than depart** (`NotReady`, unreachable). The pod is
  already gone and its players with it; that is a different problem with a
  different answer, and `PodLost` already covers what the state machine can do
  about it.
- **PVCs pinned to a node.** A `Persistent` server on an RWO volume backed by
  node-local storage may not be schedulable anywhere else. It is condemned all
  the same — the node is going — and its replacement then sits `Pending`. That
  is a limit of the storage class, not one this milestone can move; it goes in
  `docs/known-issues.md`.
- **Cordoning anything ourselves.** The operator reads `Node` objects and
  writes none.
- **Tolerations on our pods.** They tolerate nothing today and this milestone
  does not change that.
- **Ranking hash-staleness against node-staleness.** §3.4 explains why the
  existing order is already the one wanted.

## 5. Error handling

- **A node that cannot be read** (cache miss, transient error): the pod is not
  treated as condemned. Failing towards "not departing" keeps an unreadable
  `Node` from emptying a group; the watch and the periodic resync bring the
  decision back within seconds.
- **A pod with no `spec.nodeName`** is unscheduled and cannot be on a departing
  node. Not condemned.
- **A `Server` with an empty `status.nodeName`** is treated the same way. The
  field is written by the Server controller on the pass that first sees the
  scheduled pod, so the window is one reconcile.
- **A condemned server already `Draining` or `Terminating`** is not re-deleted;
  `deleteServer` is idempotent against an object that already carries a deletion
  timestamp, and `expectations` covers the cache lag.
- **PDB write failures** on the proxy side surface as the group's existing
  error path. They do not block the drain decisions, which touch players.

## 6. Testing

**Pure functions, table-tested, no cluster.**
- `IsDeparting`: `spec.unschedulable` alone; a matching taint key with
  `NoSchedule`; the same key with `NoExecute`; the same key with
  `PreferNoSchedule` (**not** departing); a non-matching key with `NoSchedule`;
  an empty key list; both criteria at once.
- `DecideSize`: a condemned server is deleted while `Surplus` is 0; while the
  group is at `minReplicas`; several condemned servers in one pass; the
  replacement is asked for in the same pass; `Delete` and `Condemn` do not name
  the same server.
- `DecideRollout` is unchanged, so the new cases sit at the view-construction
  level: a pod stale only by node, a pod stale by both, and §3.6's uncordon case
  — a marked pod stays marked when nothing is stale any more.

**envtest.** `Node` objects exist there, and the absence of a scheduler is an
advantage for once: the tests set `spec.nodeName` themselves, which is what they
would have to do anyway. Create a node, point pods at it, cordon it, and assert
that the `Server` is deleted and the proxy pod is marked. This proves the
mechanics without a cluster.

**Evidence run, real cluster.** What envtest cannot show is the point of the
milestone: that `kubectl drain` *completes* instead of hanging, and that a
player on a drained node is moved rather than disconnected. This needs a
**multi-node** kind cluster; 4c-1's has a single node
(`runbook-milestone-4c1-evidence.md:251`). One detail the plan must solve rather
than discover: a kind `extraPortMappings` host port binds to one node container,
and the replacement proxy lands on a different node — so the runbook maps two
ports across two workers rather than one port across both. The run becomes §12
of the existing runbook.

## 7. Acceptance criteria

1. A cordoned node holding an occupied server pod: the server is deleted, its
   players are moved to a fallback group, the pod goes, a replacement comes up
   on another node, and `kubectl drain` on that node completes.
2. A cordoned node holding an occupied proxy pod: a replacement proxy comes up
   on another node and becomes ready, then the old proxy goes `NotReady`, its
   `Service` endpoint disappears, and the player connected through it keeps
   playing until they leave.
3. `kubectl drain` on a node holding an occupied proxy pod does not disconnect
   that player — the PodDisruptionBudget refuses the eviction until the pod is
   empty.
4. A node carrying a configured taint with effect `NoSchedule` is treated the
   same as a cordoned one; the same key with `PreferNoSchedule` is not.
5. Releasing the node mid-flight leaves the work already begun to finish and
   condemns nothing further.
6. `make test` and `make agent-test` stay green; neither needed extending for
   this milestone.
