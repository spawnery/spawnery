# Persistent storage: what an operator owns

A `Persistent` `ServerGroup` gives each ordinal a world on a
`PersistentVolumeClaim` of its own. Three things follow from that which nobody
can read off the CRD, and none of them is a defect: they are consequences of
decisions taken deliberately, and they are here rather than in
[`known-issues.md`](known-issues.md) because that file carries problems and
these are properties.

The short version, for somebody who is in the middle of something:

- **This operator never deletes a claim.** Not on scale-down, not on group
  deletion, not ever. Orphans accumulate and removing one is a human act.
- **Deleting a claim deletes a world.** There is no undelete and no
  confirmation, because the operator is not the one deleting it.
- **A group whose storage is broken stalls rather than thrashing**, and takes
  roughly five and a half hours to say `Degraded` about it. There are two
  status fields that say something true from the first failure onward.

## Claims, and why they outlive their servers

**Claims accumulate, and this operator can never remove one.** Deleting a
`Server` — by scaling down, by hand, or through the failed-retention path
in the next section — never deletes the `PersistentVolumeClaim` it mounted:
`podspec.BuildDataClaim` stamps no owner reference, and nothing in this
operator calls `Delete` on a claim anywhere. That is not merely the observed
behaviour, it is enforced structurally: the ClusterRole
(`config/rbac/role.yaml`) grants `persistentvolumeclaims:
create;get;list;watch` and nothing else, and `internal/rbacaudit/required.go`
documents exactly those four verbs with a comment explaining why `delete` and
`update` are absent on purpose. `internal/rbacaudit`'s tests compare the
generated role against that table in both directions — extra grants as well as
missing ones — so a future `delete` marker added anywhere in the codebase
turns the audit red before it can ship. A lowered `spec.replicas`, a group
deleted outright, or an ordinal simply never brought back all leave their
claims standing, by design: §3.3 of the persistent-groups design settles that
a mistake here should cost a stray object, never a world.

To find what a namespace has accumulated:

```bash
kubectl get pvc -l spawnery.cloud/managed-by=spawnery-operator -n <namespace>
```

Every claim this operator ever created carries that label
(`podspec.LabelManagedBy`), and it is the one — the only one — that restricts
the manager's own cache over claims (`cmd/spawnery-operator/main.go`).
`podspec.BuildDataClaim` puts three more on every claim it renders,
`spawnery.cloud/network`, `spawnery.cloud/group` and `spawnery.cloud/server`;
none of those narrows anything the operator does, and they are there for
whoever is reading claims by hand. To tell a claim still in
service from an orphan, compare each claim's `spawnery.cloud/server` label
against the `Server` objects that currently exist for that group: a claim
named `<group>-<ordinal>-data` whose `spawnery.cloud/server` names a `Server`
that is gone (scaled away, or the group itself deleted) is an orphan.
**Deleting a claim deletes a world** — there is no undelete, and no
confirmation this operator can offer, because it never performs the deletion
itself. Removing one is a deliberate human act with `kubectl delete pvc`,
outside this operator entirely, and belongs on the runbook that grows up
around this operator's use rather than in its own code.

**A claim that never binds ends in a stall, and the stall is deliberate.**
`docs/superpowers/specs/2026-08-15-persistent-groups-design.md` §3.5 is on its
third version for exactly this mechanism — the first two were wrong, and its
own top-of-section note says so — so what follows is checked against the code
as it stands rather than repeated from memory:

- A pod that never becomes playable fails its server's startup deadline the
  same way an ephemeral one would; `phase.Decide`'s `Failed` case is
  type-blind.
- Nothing on the *group's* side ever removes a persistent server for having
  failed. `pruneFailed` only runs `if group.IsEphemeral()`
  (`internal/controller/servergroup_controller.go`), and
  `DecidePersistentSize` holds an ordinal for as long as any server carries
  it, in any phase — so a `Failed` corpse keeps its ordinal.
- What does eventually move it is `phase.Decide`'s own retention clock: once
  `now - status.failedAt >= spec.failedRetentionSeconds` (3600 seconds at the
  CRD default), the `Failed` case returns `Terminating`, and the **Server**
  controller deletes the object once its pod is gone
  (`internal/controller/server_controller.go`, the `decision.Next ==
  phase.Terminating && !podFound` branch). The ordinal is free the moment that
  delete lands.
- The group's very next pass sees the ordinal missing and creates it again,
  under the identical deterministic name — `podspec.DataClaimName` derives the
  claim name from the server name, and the server name is `<group>-<ordinal>`
  — so the new server's claim-create call gets `AlreadyExists` and mounts the
  same, still-broken volume. `DecideBackoff`'s create gate
  (`backoff.MayCreate`, gating `CreateOrdinals` the same way it gates the
  ephemeral count in `internal/controller/servergroup_controller.go`'s
  `size()`) is what turns this from an unbounded loop into a bounded one: six
  counted failures and the group gives up.
- So the period of the retry loop is `spec.failedRetentionSeconds`, not the
  backoff window — the backoff window is at most 160 seconds (10s doubling to
  160s across five gaps before the sixth failure), which at the CRD's
  3600-second default never actually delays an attempt. What the backoff
  contributes here is only the give-up.
- After the give-up the group waits indefinitely — but not, at first, with no
  `Server` object for that ordinal. At the moment the count reaches the
  threshold the sixth corpse is still standing and still holding its ordinal,
  and it stays for one more `failedRetentionSeconds` before the Server
  controller takes it away. The empty-ordinal state is where this settles,
  roughly an hour later at the CRD default, not where it begins. The claim and
  the world on it are untouched throughout — nothing in this operator can
  update or delete a claim, per the RBAC point above — and a spec change (any
  edit that moves `metadata.generation`) resets the counter and brings the
  ordinal back.

Stalling is the right outcome rather than a tolerated one. A persistent world
lives on one claim and nothing else can serve it, so a rebuild only ever meets
the same broken volume — sequentially, never concurrently, since the corpse's
pod is deleted before its replacement is created. After six attempts roughly
an hour apart, what is broken is the storage and not the server, and only a
human can fix a storage class, a quota, or a stuck `WaitForFirstConsumer`
binding.

## The failure clock, and why `Degraded` is late

**`Degraded` is late, and that is worth knowing before it fires.** At the
default `failedRetentionSeconds` of 3600 the group is visibly backing off
(`BackingOff: True`) for only ten to a hundred and sixty seconds of each
roughly hourly cycle. For the rest of each cycle it publishes `BackingOff:
False` with the reason "no server has failed to start recently" — true in the
narrow sense the field means, and easy to read as "nothing is wrong" while a
`Failed` corpse is sitting right there holding the ordinal. **Six counted
failures span five gaps, not six**, and each gap is longer than the
retention window alone: the corpse's `failedRetentionSeconds` (3600s) has to
elapse before the `Server` object is removed and a replacement created, and
that replacement then runs its own `--startup-deadline` (300s by default)
before it can fail in turn and be counted as the next failure. Each gap is
therefore close to `3600 + 300` = 3900 seconds, about sixty-five minutes, not
an even hour. `Degraded` therefore does not turn true until roughly **five
and a half hours** after the first failure — five gaps of about sixty-five
minutes each — not six.

The figure holds at any `replicas`, which is newer than it looks: a healthy
sibling used to reset a broken ordinal's streak, so at two or more ordinals
`Degraded` could be delayed without bound or never arrive at all.
`CountFailures` takes `requiredOrdinals` now and, for a persistent group,
breaks the streak only when *every* required ordinal has a ready server. What
is left is the lateness itself, which is arithmetic rather than a defect.

An operator watching for a stall in that window should not wait for `Degraded`
or for `BackingOff: True`: both `status.consecutiveFailures` and
`status.lastFailureAt` are written from the very first counted failure, for a
group of either type — that counting is unconditional in `Reconcile`, not
behind `if group.IsEphemeral()` the way the two conditions used to be before
this milestone's own review lifted them out.

```bash
kubectl get servergroup <name> \
  -o jsonpath='{.status.consecutiveFailures} {.status.lastFailureAt}'
```

That says something true from the first failure onward, hours before either
condition would.


## Two things a lowered `replicas` and a dead node each cost

**Lowering `replicas` nominates the top ordinal whoever is on it.** The two
sizing rules do not agree about this, and they share one delete path.
`SelectDeletionCandidates` (`internal/controller/candidates.go`) skips any
server that `mayHavePlayers()`, so an ephemeral group shrinks around its
players and takes an empty server instead. `DecidePersistentSize`
(`internal/controller/persistent.go`) has no such guard in its surplus loop: it
sorts the ordinals at or above the new `replicas` and names them, highest
first. Lowering `replicas` from 3 to 2 therefore asks for `survival-2` with
players still on it.

What protects them from there is the ordinary drain, and only that: the Server
controller moves them through the proxies and waits
`spec.drain.timeoutSeconds` (60 by the CRD default), and anyone still connected
when that deadline passes is disconnected with the pod. Design §7's acceptance
criterion 3 now carries that qualifier; it previously read "without
disconnecting a player on it", unconditionally, which is true only of a drain
that finishes in time.

The alternative is worth naming rather than assuming: a `mayHavePlayers()`
guard here would mean a lowered `replicas` is not honoured at all while anyone
is online, because no other server can take ordinal N's place — an ephemeral
group has a different server to delete instead, and a persistent group does
not. Neither direction is free. If you need the players off first, empty the
ordinal before lowering `replicas`, or raise `spec.drain.timeoutSeconds` on the
group beforehand so the drain has time to finish.

**An ordinal waits, visibly, for a pod that a dead node will never finish
terminating.** As of the branch review closing this milestone, the Server
controller refuses to create a pod while a pod of the same name still exists,
terminating or not (`internal/controller/server_controller.go`). That is what
it must do — creating into the name gets `AlreadyExists`, and the controller
would then adopt a pod it did not create and delete its own `Server` one pass
later — but it means the wait inherits whatever bound the termination has. For
an ordinary pod deletion that is `spec.terminationGracePeriodSeconds`. For a
pod on a node that has gone `NotReady`, there is none: the API server keeps the
object until a kubelet confirms the kill, and there is no kubelet to confirm
it.

The `Server` says so rather than sitting silent — `Accepted: False` with reason
`PodNameTerminating` and the pod's name in the message — and it says nothing
else: the server never reaches `Failed`, the per-group backoff never counts it,
and the phase stays `Pending` for as long as the wait lasts.

That used to be an accident and is a decision since 2026-08-24.
`status.startedAt` is now stamped when the operator accepts a Server rather than
beside its pod, so a Server with no pod does have a clock — and the deadline
that clock drives is deliberately not run while the pod's *name* is held by
another pod. Failing here would make the situation worse than the wait: the
replacement is derived from the same ordinal name and meets the same pod, a
`Failed` server holds its ordinal in `DecidePersistentSize`'s held map, and
`pruneFailed` does not run for a persistent group — so the object would stay for
its full `failedRetentionSeconds`, an hour by default, **including after
somebody force-deletes the stuck pod below.** The wait, by contrast, ends the
moment the name comes free.

```bash
kubectl get server <group>-<ordinal> -n <namespace> \
  -o jsonpath='{range .status.conditions[?(@.type=="Accepted")]}{.reason}: {.message}{"\n"}{end}'
```

The remedy is the same one a `StatefulSet` needs in this situation, and it
carries the same warning: force-deleting the pod object tells the API server
the container is gone without anything having verified that it is. On a node
that is merely unreachable rather than dead, the process may still be running
and still holding the volume, and the replacement will then contend for a
`ReadWriteOnce` claim the old pod has not released — which hangs on the volume.
Confirm the node is really gone first.

```bash
kubectl delete pod <group>-<ordinal> -n <namespace> --force --grace-period=0
```

