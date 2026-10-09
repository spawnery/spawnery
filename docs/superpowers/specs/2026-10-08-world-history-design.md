# World history for object store worlds

**Status:** implemented (branch feat/world-history)
**Date:** 2026-10-08

## 1. Goal

A world on the object store (`storage.backend: ObjectStore`) keeps exactly
one generation today. After each upload, `dropUnnamed` deletes every object
the previous manifest named and the new one does not, so the bucket can only
give back the state of the last snapshot. A world that a bug, a griefer or
the player broke cannot go back to how it was an hour or a day ago.

This design keeps older generations under a retention policy set per group,
in the style of Proxmox Backup Server's prune options, and lets a plugin
list a member's restore points and restore one while the member does not
run. Who may restore, and what a restore means for the game, stays with the
network's plugin.

## 2. What it costs

Files of 64 KiB and more are stored once per content under
`objects/<sha256>`. A snapshot uploads only the files whose content changed.
Smaller files travel in one pack per generation, and an unchanged small file
keeps pointing at the pack it came in. A generation that is kept therefore
costs what its upload cost and nothing more. What changes is only that its
objects are no longer deleted when the next generation commits.

Measured on 2026-10-08 on a network's on-demand worlds, a pregenerated
world of about 2.75 GB in some 800 region files:

| member | changed in the last 5 min | changed in the last 60 min |
|---|---|---|
| two players, 85 min after start | 8 files, 41.7 MB | the same 8 files, 41.7 MB |
| no players | 1 file, 0.7 MB | 1 file, 0.7 MB |

Region files of a pregenerated world are about 5 MB each, and a player
touches the few around them, again and again. One kept generation of a world
in play costs about one upload, here about 40 MB. A member nobody plays
writes no generations and costs nothing beyond its current state.

Uploads do not grow: a changed region file is uploaded whole today already.
They lose one step, the deletion after each commit, and gain a few small
writes (Section 4.2). Downloads do not change. A start reads the current
manifest and fetches what it names, as today. A restore fetches what the
restored generation names, the way a Proxmox Backup Server restore reads one
snapshot's index and the chunks it lists.

## 3. API

### 3.1 ServerGroup

```yaml
spec:
  type: OnDemand
  storage:
    backend: ObjectStore
    keep: [world]
    retention:
      last: 12
      hourly: 24
      daily: 7
      weekly: 4
      monthly: 3
      yearly: 0
```

`storage.retention` holds six non-negative integers, all optional, all
defaulting to 0. They follow the prune options of Proxmox Backup Server:

- `last`: the newest `last` generations.
- `hourly`, `daily`, `weekly`, `monthly`, `yearly`: of the generations older
  than everything an earlier option kept, the newest one in each of the
  newest N periods that have a generation. Periods without one do not count.

The options apply in this order and select the way Proxmox Backup Server's
`mark_selections` does: an option walks the generations newest first and
skips a period that already holds a generation an earlier option kept. With
`last: 1, daily: 2` over 10:50, 09:50 and 08:50 today and 23:50 yesterday,
it keeps 10:50 and yesterday's 23:50. Weeks are ISO weeks; all periods are
in UTC. The current generation is always kept and counts as one of `last`.

Without `storage.retention`, or with every option 0, a world keeps only its
current generation, which is the behaviour today. A CEL rule refuses
`retention` on a group without `backend: ObjectStore`. Changing the policy
does not restart members (Section 4.3). A group with `retention` cannot
switch back to `Claim` until the field is removed.

### 3.2 Plugin API

Two calls beside `startServer`, `stopServer` and `deleteServer`, with the
same `(group, key)` addressing and the same namespace scoping:

```java
CompletionStage<List<RestorePoint>> listRestorePoints(String group, String key);
CompletionStage<RestoredWorld> restoreWorld(String group, String key, long generation);

record RestorePoint(long generation, Instant taken, boolean current) {}
record RestoredWorld(long generation, long restoredFrom) {}
```

`listRestorePoints` returns the kept generations, newest first. `taken` is
when the node took the snapshot, not when it reached the bucket.

`restoreWorld` makes the content of `generation` the member's current world
as a new generation. The generation that was current before stays a restore
point until the member's next upload, so a restore can itself be undone;
from then on it stays only while the retention keeps it.

They fail the way the other calls do, with an `IllegalStateException`
whose message starts with the operator's reason:

| reason | when |
|---|---|
| `NOT_FOUND` | no such group, no world for the key, or no such generation |
| `REFUSED` | the group is not `OnDemand` with `ObjectStore`, or the operator runs without `--world-sync`, as for `startServer`; for `restoreWorld` also: the member's `Server` runs, the world is marked for deletion, or `generation` is the current one |
| `UNAVAILABLE` | `restoreWorld` only: the member's `Server` is being deleted, or the world's lease is held; after a stop, both mean the final upload is still running |

`listRestorePoints` answers for a running member and for a world marked for
deletion too; it only reads.

A plugin that gets `UNAVAILABLE` right after a stop asks again a few seconds
later. The API's Javadoc says so.

### 3.3 Protocol

`ListRestorePointsRequest{group, key}` → `ListRestorePointsResult{repeated
RestorePoint points}` and `RestoreWorldRequest{group, key, generation}` →
`RestoreWorldResult{generation, restored_from}` join the `CloudRequest` and
`CloudResponse` oneofs. Any agent pod in the namespace may call them, as for
`deleteServer`. Whether a player may restore is the plugin's decision.

## 4. The bucket

### 4.1 Layout

Per world, beside `manifest.json`, `lease.json`, `objects/` and `packs/`:

```
history/<generation, 20 digits>-<taken, unix ms>.json
```

A history entry is a copy of a manifest that was current once. The node
writes it from its own state, so it matches the manifest in content, not
necessarily in bytes; the operator's restore copies the bytes. Its
key carries what retention and `listRestorePoints` need, so both work from
one listing and read no entry. The current generation is not in `history/`;
it is `manifest.json`.

`Manifest` gains `taken` (unix ms, the node's clock when it took the
snapshot) and `restoredFrom` (the generation a restore copied, else
absent). A manifest written before this version has no `taken`; when it
moves into history, the node uses the manifest's `LastModified` instead.

Per group, written by the operator:

```
.retention/<namespace>/<group>.json   {"last":12,"hourly":24,...}
```

### 4.2 Upload

`UploadSnapshot` keeps its order and gains two steps:

1. Objects and the pack, as today.
2. New: if the group's policy, as last read (Section 4.3), keeps more
   than the current generation,
   put the previous manifest's bytes at its history key, `If-None-Match`. A
   412 means an earlier attempt already wrote it, which is fine.
3. The manifest, `If-Match` the previous ETag, as today.
4. Changed: instead of `dropUnnamed`, prune (Section 4.4).

A crash between 2 and 3 leaves a history entry for a generation that is
still current. Prune treats an entry whose generation equals the current
manifest's as already covered and deletes it.

### 4.3 How the node learns the policy

The operator's ServerGroup reconciler writes `.retention/<ns>/<group>.json`
for every `ObjectStore` group when its content differs, and deletes it when
the group drops `retention`. Deleting the group leaves the file in place, so
the group's worlds keep their history. The node agent reads it at publish and before
each upload: a `HEAD`, and a new read only when the ETag changed. A missing
file means "current only". A file it cannot read or parse leaves the last
good policy in force; before the first good read the node writes history
and deletes nothing.

The policy does not travel through the CSI volume attributes, because those
are part of the pod spec and a change would roll every running member. A
policy change takes effect at each world's next upload. A world that nobody
plays keeps its history as it is until it is played or deleted. Since every
option counts periods that have a generation, not periods of the clock, an
idle world's history would not change under an unchanged policy either.

### 4.4 Prune

Prune runs where `dropUnnamed` runs today, by the holder of the world's
lease, right after a manifest commit. It is a function of the worldsync
package that the operator's restore calls too.

1. List `history/`. Parse generation and time from each key.
2. Select what the policy keeps (Section 3.1), with the current manifest as
   the newest point.
3. Delete the history entries not kept.
4. Delete every object and pack that a dropped manifest names and no kept
   manifest, current included, names.

Step 4 needs the object sets of the kept manifests. The node holds them per
world in memory and fills the set once per publish by reading the kept
entries; the operator reads them on each restore. Each entry is the size of
a manifest (16 kB for a world of 1000 files).

Every 12th prune of a world (hourly at one snapshot per five minutes), and
at the release of its lease, prune also
lists `objects/` and `packs/` and deletes what no kept manifest names. That
removes what upload attempts left behind before their manifest, which
closes the known issue "Upload attempts that fail leave objects in the
bucket". The lease makes this safe: no other writer can have uploaded an
object it has not committed yet.

The sweep at the release of a lease runs only after a renewal proves the
lease is still this node's, and only when the bucket's manifest is the one
the node holds (same ETag, world id and generation); otherwise it is
skipped. Prune and sweep deadlines start at the renewal that justifies
them. A prune or sweep that runs while the node holds the world's lock is
bounded to 2 minutes. A cut-off one is safe, because entries go before
objects and the next sweep collects strays.

`dropStrayPacks` in `adoptCommitted` goes; the full sweep covers it.

### 4.5 Restore

The operator's `restoreWorld`:

1. Resolves the group (OnDemand, ObjectStore) and the member's name, and
   refuses while the member's `Server` exists and is not terminal.
2. Refuses while a deletion marker exists for the world.
3. Takes the world's lease as `spawnery-operator/<restore id>`, a name of
   its own per restore. A held lease is
   `UNAVAILABLE`. A node publishing the member meanwhile gets
   `UNAVAILABLE` from its own lease attempt and kubelet retries, so a start
   during a restore waits for it instead of racing it.
4. Reads the current manifest and its ETag, and the history entry of
   `generation` (`NOT_FOUND` if absent).
5. Puts the current manifest's bytes at its history key.
6. Puts a manifest with the same `worldId`, generation current+1, `taken`
   now, `restoredFrom` set, and the files of the restored entry, `If-Match`
   the current ETag.
7. Prunes, keeping the generation it replaced whatever the policy says,
   then releases the lease.
8. Records an Event on the ServerGroup: `world <key> restored to generation
   <n> of <taken>`.

Each restore holds the lease under its own name, so a second restore of the
same world finds it held and answers `UNAVAILABLE`. `If-Match` on the
manifest remains the guard against any writer that got past the lease.

A restore of the generation the current manifest was restored from, whose
entry names the same files, answers the current generation and writes
nothing. A plugin that asks again after its 10 s timeout gets the
generation the first request made instead of another one.

The operator runs a restore on a context detached from the plugin's request,
with its own timeout of 60 s, and answers listing and restore off the agent
session's loop, so a slow bucket does not hold the other requests of that
pod.

A restore writes a new generation number because the node's cache identity is
`(worldId, generation)` plus a size check of each file. A restore that
rewrote an old generation number would let a node holding another copy
under that number count it as current. With current+1, every node's cache is
stale after a restore and the next start downloads the world, as after a
start on another node. The next upload's `prev+1` cannot hit a history
entry, since all entries are older than the current manifest.

Every file the restored manifest names is an object or pack that a kept
entry names, so prune has not deleted it.

### 4.6 Deletion

The deletion sweep deletes everything under the world's prefix, `history/`
included, and needs no change. `.retention/<ns>/<group>.json` stays when
its group is deleted: members still running prune by it at their final
upload, and a recreated group overwrites it.

## 5. Metrics and events

- `spawnery_worldsync_pruned_objects_total` (node): objects and packs prune
  deleted.
- `spawnery_world_restores_total{result}` (operator): `restored`,
  `refused` (`NOT_FOUND` included), `unavailable`, `failed`.
- The Event of Section 4.5 for each restore.

The dashboard's Transfers row gets a panel for pruned objects, and the
operator dashboard gets restores.

## 6. Limits of this version

- A policy change does not prune idle worlds; they keep the history they
  had until their next upload.
- A restore downloads the whole world at the next start, even on a node
  holding most of its files. Reusing local files whose object matches is
  left for later.
- Retention periods are UTC.
- The history holds what the snapshots held: `keep` paths at snapshot time,
  about every five minutes while the member runs. Play after the last
  snapshot before a crash is not in any generation.
- No restore of a deleted world.
- The member's first upload after a restore prunes by the policy alone. It
  keeps the generation the restore replaced only under `last` of 3 or more
  (the upload, the restored generation and that one), or when a period
  option keeps it from an earlier period than the upload's.
- The retention counts have no upper bound. Every prune and restore reads
  each kept entry once per cold cache, so very large counts make restores
  slow; the plugin request times out after 10 s.

## 7. Testing

- Retention selection as table tests, including the 10-year example from
  the Proxmox Backup Server documentation, empty periods, ties
  within one period, and the current generation as one of `last`.
- Upload, prune and restore against the in-memory store: history keys,
  `If-None-Match` on a repeated history write, the leftover entry from a
  crash between steps 2 and 3, objects shared between kept and dropped
  generations survive, a restore's new generation and its cache effect on a
  node (full download, no orphan), restore refused under a held lease.
- The full sweep deletes stray objects of a failed attempt and nothing a
  kept manifest names.
- CRD: envtest for the CEL rule and the defaults.
- Operator: `answerListRestorePoints` and `answerRestoreWorld` against a
  fake writer, the reasons of Section 3.2, and the `.retention` file written
  and deleted by the reconciler.
- Agent API: JUnit for both calls and their error mapping; the records in
  `ValueTypesTest`.
- e2e in kind with MinIO (`make e2e-worldsync`): a member writes a marker,
  snapshots, writes a second marker, stops; `listRestorePoints` shows both
  generations; restore the first; the next start has the first marker and
  not the second; restore again to the newer one and find both.
