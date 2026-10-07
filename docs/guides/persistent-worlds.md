# Persistent storage: what an operator owns

A complete persistent group: two ordinals, one 10Gi world each. It joins the
Network of `config/samples/network.yaml`; apply that first if the namespace has
none. Save this as `survival.yaml`:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: survival
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: Persistent
  image: ghcr.io/spawnery/purpur:26.3-0.24.0
  maxPlayers: 20
  replicas: 2
  storage:
    # Raising it grows existing claims; lowering it only shapes new ones, and
    # no claim ever shrinks. The storage class is immutable once set.
    # One claim per ordinal, and nothing in this operator ever deletes one.
    size: 10Gi
```

```bash
kubectl apply -f survival.yaml
kubectl get servergroup survival -n minecraft
kubectl get pvc -n minecraft
```

`PersistentServerName` is `<group>-<ordinal>` and `DataClaimName` appends
`-data`, so the two servers are `survival-0` and `survival-1` and their claims
are `survival-0-data` and `survival-1-data`.

A `Persistent` `ServerGroup` gives each ordinal a world on a
`PersistentVolumeClaim` of its own. Three consequences of that cannot be read
off the CRD. None is a defect; they follow from deliberate decisions, which is
why they are here and not in
[`known-issues.md`](../reference/known-issues.md), which carries problems.

In short:

- **This operator never deletes a persistent server's claim**: not on
  scale-down, not on group deletion, not ever. Orphans accumulate and removing
  one is a human act. The one claim it deletes is an on-demand member's world,
  on a plugin's `deleteServer` (see [Private servers](on-demand-servers.md)).
- **Deleting a claim deletes a world.** There is no undelete and no
  confirmation, because the operator is not the one deleting it.
- **A group whose storage is broken stalls rather than thrashing**, and takes
  roughly five and a half hours to say `Degraded` about it. Two status fields
  report it correctly from the first failure onward.

## Claims, and why they outlive their servers

**Claims accumulate, and this operator never removes one of a persistent
server.** Deleting a `Server` (by scaling down, by hand, or through the
failed-retention path in the next section) never deletes the
`PersistentVolumeClaim` it mounted: `podspec.BuildDataClaim` stamps no owner
reference, and the only `Delete` on a claim is `DeleteServer`'s, for an
on-demand member's world. That is enforced where the operator's code cannot
reach: the ClusterRole (`config/rbac/role.yaml`) grants `delete` on claims
because RBAC cannot select the names minted at runtime, and the chart's
`ValidatingAdmissionPolicy` `spawnery-world-deletion` admits the operator's
deletion only of a claim that carries this operator's label, an on-demand
key and the name `<group>-<key>-data`, and refuses it any change to those
three labels. A persistent server's claim never carries a key, so the
operator cannot delete it. `patch` is `growClaim`'s, which touches a claim's
requested size and no other field, and `internal/rbacaudit/required.go`
documents every verb.
`internal/rbacaudit`'s tests compare the
generated role against that table in both directions (extra grants as well as
missing ones), so a future `delete` marker added anywhere in the codebase
turns the audit red before it can ship. A lowered `spec.replicas`, a group
deleted outright, or an ordinal simply never brought back all leave their
claims standing, by design: §3.3 of the persistent-groups design settles that
a mistake here should cost a stray object, never a world.

To find what a namespace has accumulated:

```bash
kubectl get pvc -l spawnery.cloud/managed-by=spawnery-operator -n <namespace>
```

Every claim this operator ever created carries that label
(`podspec.LabelManagedBy`), and it is the only label that restricts the
manager's own cache over claims (`cmd/spawnery-operator/main.go`).
`podspec.BuildDataClaim` puts three more on every claim it renders,
`spawnery.cloud/network`, `spawnery.cloud/group` and `spawnery.cloud/server`;
none of those narrows anything the operator does; they are for whoever reads
claims by hand. To tell a claim still in
service from an orphan, compare each claim's `spawnery.cloud/server` label
against the `Server` objects that currently exist for that group: a claim
named `<group>-<ordinal>-data` whose `spawnery.cloud/server` names a `Server`
that is gone (scaled away, or the group itself deleted) is an orphan.
**Deleting a claim deletes a world.** There is no undelete, and no
confirmation this operator can offer, because it never performs the deletion
itself. Removing one is a deliberate human act with `kubectl delete pvc`,
outside this operator entirely, and belongs on the runbook that grows up
around this operator's use rather than in its own code.

**A claim that never binds ends in a stall, and the stall is deliberate.**
`docs/superpowers/specs/2026-08-15-persistent-groups-design.md` §3.5 is on its
third version for this mechanism (the first two were wrong, as its own
top-of-section note says), so what follows is checked against the code as it
stands:

- A pod that never becomes playable fails its server's startup deadline the
  same way an ephemeral one would; `phase.Decide`'s `Failed` case is
  type-blind.
- Nothing on the *group's* side ever removes a persistent server for having
  failed. `pruneFailed` only runs `if group.IsEphemeral()`
  (`internal/controller/servergroup_controller.go`), and
  `DecidePersistentSize` holds an ordinal for as long as any server carries
  it, in any phase, so a `Failed` corpse keeps its ordinal.
- What does eventually move it is `phase.Decide`'s own retention clock: once
  `now - status.failedAt >= spec.failedRetentionSeconds` (3600 seconds at the
  CRD default), the `Failed` case returns `Terminating`, and the **Server**
  controller deletes the object once its pod is gone
  (`internal/controller/server_controller.go`, the `decision.Next ==
  phase.Terminating && !podFound` branch). The ordinal is free the moment that
  delete lands.
- The group's very next pass sees the ordinal missing and creates it again,
  under the same deterministic name (`podspec.DataClaimName` derives the
  claim name from the server name, and the server name is `<group>-<ordinal>`),
  so the new server's claim-create call gets `AlreadyExists` and mounts the
  same, still-broken volume. `DecideBackoff`'s create gate
  (`backoff.MayCreate`, gating `CreateOrdinals` the same way it gates the
  ephemeral count in `internal/controller/servergroup_controller.go`'s
  `size()`) bounds the loop: after six counted failures the group gives up.
- So the period of the retry loop is `spec.failedRetentionSeconds`. The
  backoff window is at most 160 seconds (10s doubling to 160s across five gaps
  before the sixth failure), which at the CRD's 3600-second default never
  delays an attempt; the backoff contributes only the give-up.
- After the give-up the group waits indefinitely, though at first a `Server`
  object still exists for that ordinal. At the moment the count reaches the
  threshold the sixth corpse is still standing and still holding its ordinal,
  and it stays for one more `failedRetentionSeconds` before the Server
  controller takes it away. The ordinal is empty only from then on, roughly an
  hour later at the CRD default. The claim and the world on it are untouched
  throughout: the operator cannot delete a
  persistent server's claim, and its one write to it grows its size, per the
  RBAC point above. Fixing the storage changes nothing the servers start with, so it
  does not reset the counter by itself: a new value on the group's
  `spawnery.cloud/retry` annotation does, and brings the ordinal back.

Stalling is intended. A persistent world lives on one claim and nothing else
can serve it, so a rebuild only ever meets the same broken volume, one rebuild
at a time, since the corpse's pod is deleted before its replacement is
created. After six attempts roughly an hour apart the storage is what is
broken, and only a human can fix a storage class, a quota, or a stuck `WaitForFirstConsumer`
binding.

## What survives a start

A claim keeps everything a server ever wrote, and the entrypoint only adds to
it: it renders the configuration and copies `extraFiles` and `extraPlugins` in,
and never deletes. A plugin removed from the source stays on every claim, a
file nobody ships any more stays as it was, a config that carried a secret
stays too, and a world shipped by the source mixes with the stale files of the
last one.

`spec.storage.keep` turns that around into a list of what survives. When it is
set, every start first deletes everything on the claim that no entry matches, then renders
and copies as before:

```yaml
spec:
  storage:
    size: 10Gi
    keep:
      - world
      - plugins/ExampleGame/state
```

- An entry is a path relative to `/data`. Each segment may use `*` and `?`, and
  never `[`, `]` or `\`; `/x`, `a//b` and `..` are refused by the API.
- A matched directory is kept whole. List the level directory and the state
  directories of the plugins, not single dimensions or files inside them. The
  datapacks of a world are part of it and persist with the save on purpose, so
  new chunks generate like the old ones.
- A mount point, the directories above it and the root `lost+found` are never
  deleted, read-only or writable.
- Everything under `config/` comes from the renderer and `configOverlay`, so it
  is deleted unless `config` is listed. `paper-world-defaults.yml` is rendered
  only when a `configOverlay` names it: set per-world defaults there, or list
  `config` and accept that it is then never refreshed.
- Unset, nothing is deleted.

Two refusals stop the start before anything is deleted, with a message naming
the path. A path no entry keeps that is, or holds, a `level.dat*` file, a
`region` directory or an `.mca` file is one: the list is wrong rather than the
world disposable. It is exempt only when `extraFiles` or `extraPlugins` ships
all of it: every file and directory below it exists in a source at the same
place, as the same kind of entry. The copy writes such a path back, so world
templates the server never loads can stay out of the list. One extra file
refuses, be it player data, a file from an older version of the source, or the
`level.dat_old` a world the server loads gains. A source that carries a path
the list keeps is the other: the copy would replace saved state with the
shipped file on every start, so keep one or ship the other.

A world template that changes between releases trips the first refusal on
every claim that still holds the old copy. `spec.storage.replace` names such
paths. They are deleted at start without the check, and the copy writes the
current version back:

```yaml
spec:
  storage:
    keep:
      - world
    replace:
      - worlds/templates
```

Entries follow the syntax of `keep`. A path both lists match is kept, the same
entry in both lists is refused, and so is `replace` without `keep`, because
without `keep` nothing is deleted at all. A `replace` entry inside a world that
`keep` does not list replaces only that part: the rest of the world is still
checked as a world, so its `playerdata` stops the start as before.

There is no dry-run field. Every path the start removes is logged as
`spawnery: keep: removing <path>`, so the first start after a change shows what
the list does.

The list is part of the pod. Changing it on a persistent group rolls the group.
On an on-demand group nothing rolls: a running member keeps its pod and its old
list, and gets the new list, and the current image, at its next start, because
the server is created from the group as it is then.

Upgrade the operator and the chart before a group uses `keep` or `replace`,
and the image with them. An operator older than the field drops it from the
spec, and an image older than the field ignores `SPAWNERY_KEEP`. Both keep
everything, so the group runs without the cleanup it asks for and nothing says
so. For `replace` it is the other way round: an operator older than the field
drops it, an image older than it ignores `SPAWNERY_REPLACE`, and either way the
start refuses a stale template as before.

## Claims that grow by themselves

A group can start every claim small and let an external autoresizer grow the
ones that fill up. The operator's part is small:

- Each new claim requests `spec.storage.size`. Lower it to start new claims
  smaller; existing claims are never shrunk.
- `spec.storage.annotations` are copied onto each claim when it is created. Existing
  claims are not changed. Keys must be valid Kubernetes annotation keys, and
  at most 64 are allowed.
- A claim larger than `spec.storage.size`, grown by hand or by an autoresizer,
  is left alone. Raising `size` above the annotated ceiling still grows claims;
  the autoresizer just stops at its ceiling.
- The server's `status.storageResizeError` and the group's `StorageResize`
  condition report a patch of this operator's own that the API server refused,
  or a resize from any requester that the storage driver failed. A patch the
  autoresizer had refused, for example for a missing `allowVolumeExpansion` or
  a quota, shows only in the autoresizer's own events and logs.
- In a persistent group, a driver that expands offline sets
  `FileSystemResizePending`; the operator then drains and restarts that server
  at a time the autoresizer picks. Drivers that expand online are unaffected.

```yaml
  storage:
    size: 5Gi
    annotations:
      resize.topolvm.io/storage_limit: 20Gi
```

The cluster has to provide the rest. The StorageClass needs
`allowVolumeExpansion: true`. For
[pvc-autoresizer](https://github.com/topolvm/pvc-autoresizer), the StorageClass
needs the annotation `resize.topolvm.io/enabled: "true"` (or the autoresizer runs
with `--no-annotation-check`), and Prometheus has to scrape the kubelet's volume
stats.

Claims that exist already do not get the annotations. Add them by hand:

```bash
kubectl annotate pvc -n <namespace> -l spawnery.cloud/group=<group> \
  resize.topolvm.io/storage_limit=20Gi --overwrite
```

## The failure clock, and why `Degraded` is late

**`Degraded` is late.** At the
default `failedRetentionSeconds` of 3600 the group is visibly backing off
(`BackingOff: True`) for only ten to a hundred and sixty seconds of each
roughly hourly cycle. For the rest of each cycle it publishes `BackingOff:
False` with the reason "no server has failed to start recently": true in the
narrow sense the field means, and easy to read as "nothing is wrong" while a
`Failed` corpse is sitting right there holding the ordinal. **Six counted
failures span five gaps, not six**, and each gap is longer than the
retention window alone: the corpse's `failedRetentionSeconds` (3600s) has to
elapse before the `Server` object is removed and a replacement created, and
that replacement then runs its own `--startup-deadline` (300s by default)
before it can fail in turn and be counted as the next failure. Each gap is
therefore close to `3600 + 300` = 3900 seconds, about sixty-five minutes, not
an even hour, and `Degraded` does not turn true until roughly **five and a half
hours** after the first failure (five gaps of about sixty-five minutes each,
not six).

The figure holds at any `replicas`, which is newer than it looks: a healthy
sibling used to reset a broken ordinal's streak, so at two or more ordinals
`Degraded` could be delayed without bound or never arrive at all.
`CountFailures` takes `requiredOrdinals` now and, for a persistent group,
breaks the streak only when *every* required ordinal has a ready server. What
is left is the lateness itself, which is arithmetic, not a defect.

An operator watching for a stall in that window should not wait for `Degraded`
or for `BackingOff: True`: both `status.consecutiveFailures` and
`status.lastFailureAt` are written from the very first counted failure, for a
group of either type. That counting is unconditional in `Reconcile`; the two
conditions sat behind `if group.IsEphemeral()` until this milestone's own
review lifted them out.

```bash
kubectl get servergroup <name> \
  -o jsonpath='{.status.consecutiveFailures} {.status.lastFailureAt}'
```

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

From there only the ordinary drain protects them: the Server controller moves them through the proxies and waits
`spec.drain.timeoutSeconds` (60 by the CRD default), and anyone still connected
when that deadline passes is disconnected with the pod. Design §7's acceptance
criterion 3 now carries that qualifier; it previously read "without
disconnecting a player on it", unconditionally, which is true only of a drain
that finishes in time.

A `mayHavePlayers()` guard here would mean a lowered `replicas` is not honoured at all while anyone
is online, because no other server can take ordinal N's place: an ephemeral
group has a different server to delete instead, and a persistent group does
not. Neither direction is free. If you need the players off first, empty the
ordinal before lowering `replicas`, or raise `spec.drain.timeoutSeconds` on the
group beforehand so the drain has time to finish.

**An ordinal waits, visibly, for a pod that a dead node will never finish
terminating.** As of the branch review closing this milestone, the Server
controller refuses to create a pod while a pod of the same name still exists,
terminating or not (`internal/controller/server_controller.go`). It has to:
creating into the name gets `AlreadyExists`, and the controller would then
adopt a pod it did not create and delete its own `Server` one pass later. But
it means the wait inherits whatever bound the termination has. For
an ordinary pod deletion that is `spec.terminationGracePeriodSeconds`. For a
pod on a node that has gone `NotReady`, there is none: the API server keeps the
object until a kubelet confirms the kill, and there is no kubelet to confirm
it.

The `Server` reports it as `Accepted: False` with reason `PodNameTerminating`
and the pod's name in the message, and reports nothing else: the server never reaches `Failed`, the per-group backoff never counts it,
and the phase stays `Pending` for as long as the wait lasts.

That used to be an accident and is a decision since 2026-08-24.
`status.startedAt` is now stamped when the operator accepts a Server rather than
beside its pod, so a Server with no pod does have a clock, and the deadline
that clock drives is deliberately not run while the pod's *name* is held by
another pod. Failing here would make the situation worse than the wait: the
replacement is derived from the same ordinal name and meets the same pod, a
`Failed` server holds its ordinal in `DecidePersistentSize`'s held map, and
`pruneFailed` does not run for a persistent group, so the object would stay for
its full `failedRetentionSeconds`, an hour by default, **including after
somebody force-deletes the stuck pod below.** The wait ends the moment the name
comes free.

```bash
kubectl get server <group>-<ordinal> -n <namespace> \
  -o jsonpath='{range .status.conditions[?(@.type=="Accepted")]}{.reason}: {.message}{"\n"}{end}'
```

The remedy is the same one a `StatefulSet` needs in this situation, and it
carries the same warning: force-deleting the pod object tells the API server
the container is gone without anything having verified that it is. On a node
that is merely unreachable rather than dead, the process may still be running
and still holding the volume, and the replacement will then contend for a
`ReadWriteOnce` claim the old pod has not released, and hang on the volume.
Confirm the node is really gone first.

```bash
kubectl delete pod <group>-<ordinal> -n <namespace> --force --grace-period=0
```

