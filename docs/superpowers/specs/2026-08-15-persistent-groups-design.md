# Milestone 5a: persistent groups exist

**Status:** design of record. Written 2026-08-15, after 4c-3 merged and its
evidence run completed.

## 1. What this builds

A `ServerGroup` of type `Persistent` produces nothing today. The CRD accepts
one — `spec.type`, `spec.replicas`, `spec.storage` are all there, with CEL
rules forbidding `scaling` and `update` on it — and the operator then builds no
server for it at all. `size()` does run for a persistent group, and since 4c-3
it runs for one whose `Network` is broken too, but it runs with
`mayResize := networkUsable && group.IsEphemeral()` false, and the branch that
takes returns a zero `SizeDecision` carrying only whatever the node drain
condemned. Nothing decides how many servers a persistent group should have,
because nothing asks. `ServerSpec.Ordinal` exists and no controller reads it. And **no
`PersistentVolumeClaim` is created anywhere in this repository**, though
`dataVolume` (`internal/podspec/server.go`) already points a persistent pod at
a claim named `DataClaimName(srv.Name)` — a claim that would never exist, so
the pod would sit `Pending` forever if anything ever created it.

5a closes that. A persistent group with `replicas: 2` produces `survival-0` and
`survival-1`, each with its own PVC, each reaching `Ready`, each joinable.
Lowering `replicas` removes the highest ordinal through the drain 4c-3 already
proved on a real cluster. Raising it back brings that ordinal up again — and it
finds its world where it left it.

Milestone 5 as the master design names it — *"Persistent groups with a PVC,
ordered shutdown and recreate updates; detecting secret rotation along with a
runbook"* — is cut into three, the way 4c was:

| | Scope | Depends on |
|---|---|---|
| **5a** | Persistent groups exist: ordinals, PVCs, both directions of `replicas` | nothing |
| **5b** | Ordered shutdown, `Recreate` updates, `storage.size` growth | 5a |
| **5c** | Secret rotation detection and its runbook | nothing |

5c shares no mechanism with the other two and could be built before, between or
after them.

## 2. What already exists, and what it implies

- **`DesiredReplicas()`** (`api/v1alpha1/servergroup_types.go`) already returns
  `*spec.replicas` for a persistent group, and `derivePhase` already reads it.
  The accessor is in place; nothing acts on it.
- **`dataVolume`** already branches on the type and names the claim
  `DataClaimName(srv.Name)` = `<server>-data`. That name is derived from the
  *server's* name, which is why §3's naming decision is load-bearing rather
  than cosmetic: a stable server name is what makes a stable claim name, and a
  stable claim name is what makes a world survive.
- **`TerminationGracePeriodSeconds`** already reaches the pod
  (`internal/podspec/server.go`). The time to save a world is not something 5b
  has to build; 5b orders the shutdown, it does not invent the grace.
- **The CEL rules already forbid** `scaling` and `update` on a persistent
  group, and require `storage`. They do not require `replicas` — see §9.
- **Condemnation by node drain already runs outside the sizing decision**
  (4c-3): `size()` returns `condemn(...)` on both of its branches, so a
  persistent group inherits node drain without a line of new code, the moment
  it has servers at all.

## 3. Design

### 3.1 The ordinal is the identity

An ephemeral server is named `<group>-<random>` by `NewServerName`. A
persistent server is named **`<group>-<ordinal>`** — `survival-0`,
`survival-1` — and `ServerSpec.Ordinal` carries the number it was built from.

Everything else follows from that name. `DataClaimName` makes the claim
`survival-0-data`, and that string is stable across every deletion and
recreation of the server object. The world is addressed by the ordinal, and
the ordinal is addressed by the name.

**Gaps are filled, not skipped.** With `replicas: 3` and someone deleting
`survival-1` by hand, the group recreates `survival-1` — which finds
`survival-1-data` waiting. Creating a `survival-3` instead would be the
alternative and would be wrong twice over: it strands one world and starts an
empty one.

**A leaving server still holds its ordinal.** A server draining out of the
group has not released its number yet; the group must not build a second
server on the same claim while the first still has it mounted, which with
`ReadWriteOnce` would deadlock on the volume rather than fail cleanly. So an
ordinal counts as taken while any server carrying it exists, whatever phase it
is in.

### 3.2 A second sizing rule, beside the existing one

`DecideSize` is the slot rule, and the CRD forbids `scaling` on a persistent
group precisely because that rule does not apply to one. A second pure
function stands beside it:

```go
// DecidePersistentSize decides which ordinals a persistent group is missing
// and which it has too many of.
func DecidePersistentSize(replicas int32, views []ServerView) SizeDecision
```

It shares `SizeDecision` with the slot rule — including the `Condemn` field the
node drain attaches — and reads none of what `ScalingInputs` carries about
spare slots, player counts, capacity or generations, because none of it means
anything here. Its whole input is: which ordinals exist, which of them are
already leaving, and how many there should be.

`size()` picks between the two on the group's type. Both return the same shape,
so the execution below them — create, delete, reserve with `expectations` —
does not branch.

Three rules, each with its reason:

- **Missing ordinals are created lowest-first.** `survival-0` before
  `survival-1`, so a group coming up looks the way a reader expects and a
  half-built group is describable.
- **Surplus ordinals are removed highest-first.** The reverse, for the same
  reason, and because the highest ordinal is the one most recently added and
  least likely to be the one a player thinks of as *the* server.
- **A server already leaving is neither counted as missing nor removed
  again.** `ServerView.leaving()` covers phases and condemnation alike since
  4c-3, and this rule reads it rather than the phase directly.

### 3.3 The claim

A new builder, beside the pod builders it already lives with:

```go
// BuildDataClaim renders the PersistentVolumeClaim a persistent server's world
// lives on.
func BuildDataClaim(group *spawneryv1alpha1.ServerGroup, srv *spawneryv1alpha1.Server) *corev1.PersistentVolumeClaim
```

It translates `spec.storage` — size, `storageClassName`, `accessModes` — into a
claim named `DataClaimName(srv.Name)`. The **Server** controller creates it,
before the pod, as master design §6.1 states.

Three properties, each with its reason:

- **No owner reference.** The claim belongs to nothing and therefore outlives
  everything: the server, the group, and the operator who deletes the wrong
  object. This is the one decision in this milestone where being wrong
  destroys user data, and it is settled in the direction where a mistake costs
  a stray object rather than a world. It is also what a Kubernetes operator
  already expects: a `StatefulSet` retains its claims both on scale-down and
  on deletion, by default.
- **Created, never updated.** If the claim already exists it is left exactly as
  it is — which is the ordinary case, because a recreated ordinal is *supposed*
  to find its old world. Growing it is 5b's.
- **No waiting for `Bound`.** The pod is created straight after the claim, and
  binding is left to Kubernetes. Waiting would deadlock under
  `volumeBindingMode: WaitForFirstConsumer`, which binds a volume only once a
  pod demands it — and that is the default of most topology-aware storage
  classes, including exactly the node-local ones this milestone's failure modes
  are about.

Two costs, stated rather than discovered:

- **RBAC.** `persistentvolumeclaims: create;get;list;watch` on the existing
  ClusterRole — *and* a matching entry in `internal/rbacaudit/required.go`,
  which is a hand-maintained table `make test` cross-checks against the
  generated role. A grant without its justification there fails the suite.
- **The cache.** A cached `Get` on claims means an informer over every PVC in
  the namespace. It is restricted by the `managed-by` label, the same
  `Cache.ByObject` mechanism `cmd/spawnery-operator/main.go` already applies to
  ConfigMaps and ServiceAccounts for the same reason — which means the builder
  must put that label on the claim.

### 3.4 Node drain needs nothing new

`collectViews` computes `Condemned` from the pod's node without caring what
type of group it belongs to, and condemnation executes outside the sizing
decision. So a persistent server on a departing node is deleted like any
other, its players are moved, and the ordinal comes back — finding its claim.

Where the storage cannot follow, the replacement sits `Pending` until the node
returns. That is a limit of the storage class and not one this milestone can
move; `docs/known-issues.md` already records it from 4c-3, written before
anything could reach it, and 5a is what makes it reachable.

### 3.5 The failure path: the group stalls, and says so

**This is the third version of this section. The first was wrong, the second
was wrong about how the first was wrong, and the third is the one whose every
link was verified in the code rather than argued from.** That history is worth
keeping, because the failure it describes is the one this repository keeps
producing: a sentence that reads plausibly and describes a mechanism the code
does not have.

The first version said: a claim that never binds makes the pod stay `Pending`,
the startup deadline expires, the server goes `Failed`, *"the group reports
`Degraded`, and 4d's per-group backoff stops it throwing the same ordinal at
the same broken volume in a loop."* The second version declared both halves
false. **Only the first half was.**

**What is actually true**, each link established in code by the Task 6
re-review:

- **The reporting half was false.** Neither `ConditionBackingOff` nor
  `ConditionDegraded` was published outside `if group.IsEphemeral()`, so
  `derivePhase` could never return `Degraded` for a persistent group; the phase
  merely dropped to `Pending`, indistinguishable from a slow start.
- **The backoff half was true.** `size()` runs the `CreateOrdinals` loop under
  `backoff.MayCreate`, and `GaveUp` ends the attempts. The backoff does gate a
  persistent group's rebuilds, exactly as the first version claimed.
- **There is a retry loop, and it is not the group's.** A `Failed` server is
  moved to `Terminating` by `phase.Decide` once `FailedRetentionElapsed`; the
  **Server** controller then deletes the object; the ordinal is free; the group
  creates it again next pass, and the deterministic name re-finds its claim
  through `AlreadyExists`. So the period of the loop is
  `spec.failedRetentionSeconds` — 3600 seconds at the CRD default — not the
  backoff window, which is at most 160. At that default the backoff's *waiting*
  half never delays an attempt; what it contributes is the **give-up**, after
  six counted failures.
- **After the give-up the group waits indefinitely**, with no `Server` object
  for that ordinal at all. The claim and the world are untouched: nothing in
  this operator deletes or updates a claim, and the ClusterRole grants neither
  verb. A spec change resets the counter and brings the ordinal back.

**The stall is right; the silence was the defect.** A persistent world lives on
one claim and nothing else can serve it, so a rebuild meets the *same* broken
volume — sequentially, never concurrently, since the corpse's pod goes first.
`ReadWriteOnce` forces that serialization but not the waiting; the waiting is
the give-up's doing, and it is correct: after six attempts, roughly an hour
apart, the thing that is broken is the storage, and only a human fixes that.

So 5a lifts `BackingOff` and `Degraded` out of the ephemeral-only block; both
describe failures either kind of group can have. `ScalingLimited` stays
ephemeral-only — it reads fields only `DecideSize` fills, and a fixed replica
count has no ceiling to be limited by. `pruneFailed` stays too: it caps
retained corpses because an ephemeral group rebuilds every five seconds, while
for a persistent group the corpse *is* what holds the ordinal, and pruning it
would free ordinals early and accelerate the very thrash this section wants
stalled.

Master design §7 asks for "a condition instead of waiting silently" here. After
the lift that condition exists and this path reaches it. **One honest limit
remains, and it goes in `docs/known-issues.md`:** at the default retention the
group is visibly backing off for only ten to a hundred and sixty seconds of
each roughly hourly cycle. For the rest of it the backoff window is shut and
the group publishes `BackingOff: False` — "no server has failed to start
recently" — beside a `Failed` corpse.

**`Degraded` therefore first appears roughly five and a half hours after the
first failure**, and the arithmetic is worth writing out because the obvious
number is wrong twice over. `backoffGiveUpAt` is 6, so the condition needs six
counted failures — and six failures are separated by **five** cycles, not six.
Each cycle is `failedRetentionSeconds` (3600 at the CRD default) plus the
startup deadline each attempt burns before it fails (300 by the operator's
`--startup-deadline` flag): about 65 minutes. Five of those is five hours and
twenty-five minutes. The
condition is right and the wait is deliberate; the delay before either becomes
visible is not something this milestone fixes.

## 4. Out of scope, deliberately

- **`Recreate` updates and ordered shutdown** — 5b. Note the grace period to
  save a world already reaches the pod; 5b orders the sequence, it does not
  build the grace.
- **Growing `storage.size`**, and the `allowVolumeExpansion` question with it —
  5b.
- **Secret rotation** — 5c.
- **Collecting orphaned claims.** They accumulate; that is the point. How to
  find and remove them belongs in `docs/known-issues.md` and the runbook.
  Automating it would be a mechanism that deletes worlds, which is the thing
  §3.3's decision exists to prevent.
- **Slot scaling for persistent groups.** Already forbidden by the CRD.

## 5. Error handling

- **`replicas` unset on a persistent group** — see §9; a CEL rule makes it
  required, so the case does not reach the controller.
- **A claim that cannot be created** (quota, an invalid storage class name)
  fails the Server's reconcile before the pod exists. The server never starts,
  the startup deadline fails it, and 4d bounds the retry.
- **A claim that exists with a different size or class than `spec.storage`
  asks for** is left alone in 5a, because 5a never updates a claim. That
  divergence is visible on the object and becomes 5b's when growth arrives.
- **An ordinal held by a server that will not finish draining** blocks its own
  recreation for as long as the drain runs, bounded by
  `spec.drain.timeoutSeconds`. This is intended: building a second server on a
  mounted `ReadWriteOnce` volume would hang on the volume instead.

## 6. Testing

**Pure, table-tested, no cluster.** `DecidePersistentSize`: nothing exists and
three are wanted; one missing in the middle of three; one surplus at the top;
a leaving server counted as neither missing nor surplus; `replicas: 0`; and
the same server both leaving and highest-numbered, so the surplus rule does not
name it twice.

**envtest.** A persistent group creates N `Server` objects with ordinal names
and `spec.ordinal` set; each Server creates its claim before its pod; the claim
carries `spec.storage`'s size, class and access modes plus the `managed-by`
label; and — the one that matters — **deleting a Server leaves its claim
standing, and the same ordinal picks it up again.**

**What envtest cannot show is the point.** There is no provisioner there, so no
claim ever binds and no pod ever runs. That a *world survives* needs a real
cluster, and `kind` ships a working default storage class (the local-path
provisioner) that can bind one. The runbook section for 5a therefore has an
acceptance test that cannot be argued with: place a block, delete the pod,
rejoin, and the block is still there.

## 7. Acceptance criteria

1. A `Persistent` group with `replicas: 2` reaches `Ready` with servers
   `<group>-0` and `<group>-1`, each with its own bound claim, and a player can
   join one.
2. Deleting a persistent server's pod brings the same ordinal back on the same
   claim, and the world it contains is unchanged — proven on a real cluster by
   an artefact placed before the deletion and found after it.
3. Lowering `replicas` removes the highest ordinal through the existing drain,
   without disconnecting a player on it, and leaves its claim behind.
4. Raising `replicas` again brings that ordinal back onto the claim it left.
5. A persistent server on a cordoned node is condemned like any other, and
   `kubectl drain` completes where the storage can follow.
6. `make test` and `make agent-test` stay green; neither needs extending for
   this milestone.

## 8. What 5b and 5c find in place

- Ordinals, claims, and both directions of `replicas`, with the claim retained
  across every one of them.
- `DecidePersistentSize` as the single place a persistent group's size is
  decided, table-tested, with `Recreate` to be layered on top rather than
  woven in.
- The grace period already on the pod, and `spec.drain.timeoutSeconds` already
  bounding the wait.
- A runbook section that brings a persistent group up on a real cluster, which
  5b's own evidence can start from rather than rebuild.

## 9. One CRD change

`spec.replicas` is optional with no default, so a persistent group without it
runs zero servers — a group that is accepted, reports `Ready`, and does
nothing. A CEL rule makes it required for the type, in the same form the CRD
already uses for `storage`:

```
self.type != 'Persistent' || has(self.replicas)
```

Ephemeral groups are unaffected: they are sized by `scaling` and the rule does
not reach them.
