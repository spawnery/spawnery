# On-demand worlds in an object store

**Status:** design
**Date:** 2026-10-06

## 1. What goes wrong today

Every member of an `OnDemand` group keeps its world on a claim of its own,
`<member>-data`. That ties a world to whatever the storage class does on
attach. With a replicated block store, a start waits for the volume to
attach before the container can run, and the world stays on the nodes that
hold its replicas.

Measured on 2026-10-06 on a three-node cluster with a replicated block
store and world volumes of one replica each:

- An attach took 12 to 16 s per start, before the container started.
- With one replica, a world's data sits on one node. A member scheduled
  on another node did every read and write over the network.

The worlds themselves are small once played. Twelve stopped worlds of an
on-demand group, mounted read-only on 2026-10-06:

| world | size | files |
|---|---|---|
| typical, after the game trimmed unplayed regions | 109 to 173 MB | ~50 `.mca` |
| explored widely | 565 MB, 1.0 GB, 1.5 GB | 190 to 500 `.mca` |
| barely used | 3 MB | 4 `.mca` |
| a pregenerated world nobody trimmed | ~2.6 GB | ~900 `.mca` |

## 2. Measurements against an object store

From the same three nodes against Hetzner Object Storage (`fsn1`) on
2026-10-06, downloading objects of about 1.6 MB (the median) with rclone:

| | node A | node B | node C |
|---|---|---|---|
| 137 MB in 95 objects, 8 to 32 at once | 1.0 s | 2.4 s | 1.5 s |
| 2.1 GB in 1460 objects, 32 at once | 30 s | 31 s | 32 s |
| one object after another, per object | 71 ms | 128 ms | 93 ms |
| upload 150 MB in 60 objects, 16 at once | 2.6 s | 5.4 s | 2.7 s |

Bulk downloads settled at 62 to 72 MB/s on every node, so the limit is on
the store's side. One node reached only 21 MB/s on a single stream and
needs parallel requests.

Conditional writes, tried the same day:

- `PutObject` with `If-None-Match: *` creates once and answers 412 after.
- `PutObject` with `If-Match` answers 412 for a stale ETag and 200 for the
  current one, but only when the ETag is sent **without** its quotes. The
  quoted form, which RFC 9110 and AWS use, gets 412 even when it matches.
  The client must send the bare value. aws-sdk-go-v2 passes the string as
  given; minio-go adds quotes.

The provider documents 750 requests/s per bucket and per source IP and asks
for objects of about 1 MB or more; it bills at least 64 kB per object.

## 3. The shape

A group opts in with `spec.storage.backend: ObjectStore`. Its members then
get `/data` from a node-local directory that a node agent fills from the
object store and writes back to it. The pieces:

1. **`spawnery-worldsync`**, a new binary in `cmd/`. It is a CSI node plugin
   for inline ephemeral volumes only, run as a DaemonSet by the chart. It
   owns a directory per world on the node, the download, the upload, and a
   lease that keeps a world on one node at a time.
2. **The operator** renders a CSI inline volume instead of a claim, never
   creates a claim for such a group, and deletes the world in the store
   when the agent channel asks to delete a member.
3. **The Paper agent** holds the server before world load until the world
   is on disk, and asks for a consistent snapshot every few minutes while
   the server runs.

`backend: Claim` stays the default and behaves as today.

### 3.1 API

```yaml
spec:
  type: OnDemand
  storage:
    backend: ObjectStore     # Claim (default) | ObjectStore
    size: 6Gi                # still required; see §8
    keep:
      - worlds/world
      - plugins/Example/data
```

- `backend` is allowed only for `OnDemand` groups in this version.
  `Persistent` would need a prefix per ordinal and is left for later.
- `ObjectStore` requires `keep`. The keep entries are exactly what is
  synced: a world is the set of files under `/data` that a keep entry
  matches. Everything else on the node directory is scratch that the next
  start's prune deletes anyway.
- `backend` may change in both directions. Existing claims are left alone
  when a group moves to `ObjectStore`, and the store is left alone when it
  moves back. Moving data across is the import of §7.
- `storageClassName`, `accessModes` and `annotations` are ignored under
  `ObjectStore`. They stay valid so that a group can switch back without
  touching the immutable fields.

The store itself is not in the CRD. It is cluster configuration, set once
in the chart (§6), because the credentials must stay out of the game
namespaces.

### 3.2 The pod

```yaml
volumes:
  - name: data
    csi:
      driver: worldsync.spawnery.cloud
      volumeAttributes:
        world: <namespace>/<group>/<key>
        keep: "worlds/world\nplugins/Example/data"
```

CSI inline volumes are allowed under the `restricted` Pod Security level,
which `hostPath` is not. The pod also gets `SPAWNERY_WORLD_SYNC=1`, which
the agent and the prune read.

The desired-state hash is taken over the rendered pod, so switching the
backend changes it. Groups on `Claim` render the same pod as before, and
the hash goldens do not move.

### 3.3 The control directory

The node agent and the game server talk through files in
`/data/.spawnery-worldsync/`:

| file | written by | meaning |
|---|---|---|
| `ready` | node agent | every file of the world is on disk |
| `failed` | node agent | the download failed; the file holds the reason |
| `snapshot.request` | agent | a sequence number, asking for a snapshot |
| `snapshot.done` | node agent | the sequence number of the finished copy, or `failed <seq> <reason>` |

The prune never deletes this directory when `SPAWNERY_WORLD_SYNC` is set.
It is not synced.

### 3.4 The agent

- **Wait.** In `onLoad`, the Paper agent waits for `ready` or `failed`.
  Paper loads the worlds after every plugin's `onLoad`, so the server does
  not touch the world before it is complete. A plugin that reads the world
  in its own `onLoad` must load after the agent (`SpawneryAgent`,
  `load: BEFORE` in its dependencies). On `failed`, or when neither file
  appears within 10 minutes, the agent logs the reason and shuts the
  server down with exit code 1.
- **Snapshot.** From `onEnable`, every `SPAWNERY_WORLD_SYNC_INTERVAL`
  (default 5 minutes), the agent runs the steps of §4.4 on the main thread
  for the flush and waits off the main thread for `snapshot.done`. While it
  waits, autosave stays off; it turns autosave back on when the answer
  arrives or after 60 s, whichever comes first.

Both only run when `SPAWNERY_WORLD_SYNC=1`.

## 4. The node agent

### 4.1 Layout in the store

Under `<prefix>/<namespace>/<group>/<key>/`:

- `manifest.json`: the world as of one generation:
  `{worldId, generation, files: [{path, size, mode, mtime, object}]}`, where
  `object` names either an object by content or the generation's pack.
  `worldId` is random and set at the first upload. A world deleted and
  started again under the same key gets a new one, so no node mistakes an
  old cache for it.
- `objects/<sha256>`: one object per file of 1 MiB or more, by content. Region
  files are already compressed and are stored as they are.
- `packs/<generation>.tar.zst`: all smaller files of that generation in one
  object. A pack is always fetched whole.
- `lease.json`: `{node, pod, world, renewedAt}`.
- `deleted`: present while a deletion is pending (§5).

A manifest is written last, with `If-Match` on the previous manifest's
ETag (or `If-None-Match: *` for the first). Objects a manifest no longer
names are deleted after the new manifest is in place. The pack of the
previous generation goes the same way.

### 4.2 The lease

- **Take:** create `lease.json` with `If-None-Match: *`.
- **Hold:** rewrite it with `If-Match` every 30 s while the pod runs and
  while an upload is pending.
- **Take over:** allowed when `renewedAt` is more than 10 minutes old, with
  `If-Match` on the stale ETag, so two nodes cannot both win. This covers a
  node that died.
- **Release:** delete it after the final upload succeeded.
- **Lost:** a node that finds its lease taken over (412 on renewal, or a
  foreign holder at restart) stops writing that world. It moves its local
  copy to `orphans/` on the node and never uploads it. The copy is kept for
  manual recovery and reported as a metric.

The clock that matters for staleness is the store's: `renewedAt` is
compared with the `Date` header of the response that read the lease.

### 4.3 NodePublishVolume

Called by the kubelet before the containers start.

1. Take the lease, or confirm this node holds it. If another node holds a
   fresh lease, return `UNAVAILABLE`; the kubelet retries with back-off and
   the pod stays in `ContainerCreating`. This is the case of a world
   started elsewhere while the old node still uploads.
2. Read `manifest.json`.
   - No manifest: a new world. Mark it ready at once.
   - A manifest whose `worldId` and `generation` match the local state, or
     a local state of this node that is ahead of the manifest by snapshots
     still uploading: the cache is current. Mark it ready at once.
   - Otherwise: delete the keep paths in the local directory and start the
     download in the background, 32 requests at a time. Write `ready` when
     every file is on disk and fsynced, or `failed` with the reason.
3. Bind-mount the world directory onto the target path and return.

The call returns after the lease and one manifest read, about 0.1 to 0.3 s
by §2, so the container starts while the download runs.

### 4.4 Snapshots while the server runs

The agent asks, the node agent copies, and the upload runs from the copy:

1. The agent turns autosave off on every world, runs `save-all flush`, and
   writes `<control>/snapshot.request` with a sequence number.
2. The node agent sees the request (it polls every 500 ms; inotify does not
   cross the bind mount reliably enough to depend on). For every file a keep
   entry matches it compares size and mtime with the last uploaded state,
   copies the changed ones to `snapshots/<seq>/` on the same filesystem,
   and checks size and mtime again after the copy. A file that changed
   during its copy is copied again, up to three times; after that the
   snapshot fails and the agent is told so.
3. The node agent writes `snapshot.done` with the sequence number. The agent
   turns autosave back on. The world was frozen for the flush and the local
   copy, not for the upload.
4. The upload of `snapshots/<seq>/` and the new manifest run in the
   background. A snapshot request that arrives while an upload runs waits
   for it.

The agent asks every 5 minutes by default. The interval is the most play
that is lost when a node dies (§5).

### 4.5 NodeUnpublishVolume

Called after the pod's containers ended. The server saved on shutdown, so
the directory is consistent.

1. Unmount the bind mount, take a final snapshot (the copy of §4.4 step 2,
   with nothing running that could change a file) and return. The kubelet
   waits for the local copy only, not for the upload.
2. In the background: upload the snapshot and write the manifest, retrying
   with back-off up to one minute between attempts and no limit on the
   attempts. Then delete the non-keep paths from the local directory and
   release the lease, unless a pod has published the world again on this
   node in the meantime.

Because the upload runs from the snapshot, the same world may start again
on this node at once (§4.3) while its last stop is still uploading.

The local keep paths stay as a cache. A cache that is clean and has no pod
is evicted after 24 hours, or earlier, oldest first, when the directory's
filesystem has less than 15 % free.

### 4.6 Restarts of the node agent

Per world, `state.json` on the node records the `worldId`, the uploaded
generation, the lease ETag, the published target path if any, and a pending
upload. On start the node agent rereads every state file, resumes lease
renewal and pending uploads, and resumes watching published volumes for
snapshot requests. The bind mounts of running pods are kernel mounts in the
kubelet's directory and survive the node agent's restart; the DaemonSet
mounts its root with `Bidirectional` propagation for that reason.

## 5. Failure cases

- **The store is unreachable at start.** The lease cannot be taken, so the
  start waits, also when the cache on this node is current. Without the
  lease there is no proof that no other node writes the world.
- **The download fails.** The node agent writes `failed`. The agent stops
  the server with an error and the operator's usual retry starts it again.
- **The store is unreachable at stop.** The upload retries until it works.
  The world stays on that node and can start there again; a start on
  another node waits for the lease.
- **A node dies.** Its worlds lose the play since their last snapshot, at
  most the snapshot interval. After 10 minutes another node may take the
  lease and starts from the last manifest. When the dead node returns it
  finds its leases taken and moves those worlds to `orphans/`.
- **A pod is deleted while a snapshot copy runs.** The copy is discarded and
  the final upload of §4.5 runs over the directory instead.
- **The agent never asks for a snapshot** (an older agent, or a group whose
  plugins crash it): the world is still uploaded at every stop. Only the
  protection against node loss is missing.
- **The world is deleted** (§5.1).

### 5.1 Deleting a world

`DeleteServer(group, key)` deletes the `Server`, as today, and then writes
`deleted` under the world's prefix. A runnable in the operator lists
`deleted` markers once a minute. For each world whose lease is absent or
stale it deletes the lease, every object and pack, the manifest, and the
marker last.

A start of the same key while `deleted` exists is refused by the node agent
(`UNAVAILABLE`) until the deletion finished, then begins as a new world with
a new `worldId`.

## 6. The chart

```yaml
worldSync:
  enabled: false
  objectStore:
    endpoint: https://fsn1.your-objectstorage.com
    region: fsn1
    bucket: worlds
    prefix: ""
    credentialsSecret: worldsync-s3   # keys AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
  namespace: spawnery-worldsync
  hostPath: /var/lib/spawnery/worldsync
  snapshotInterval: 5m
  nodeSelector: {}
  tolerations: []
```

The values above are an example, not defaults. With `enabled: true` the
chart renders:

- the `CSIDriver` object (`attachRequired: false`, `podInfoOnMount: true`,
  `volumeLifecycleModes: [Ephemeral]`),
- the DaemonSet with the node agent and `node-driver-registrar`, privileged,
  in `worldSync.namespace` (the release namespace when empty). That
  namespace must admit privileged pods; an operator namespace held to
  `restricted` does not, which is why the two are separate. The chart does
  not create the namespace.
- the operator's read of the same secret, for deletions. The secret has to
  exist in both namespaces.

The game namespaces never see the credentials. The snapshot interval
reaches the agent as `SPAWNERY_WORLD_SYNC_INTERVAL` through the operator.

## 7. Import

`spawnery-worldsync import --world <namespace>/<group>/<key> --dir <path>`
uploads the keep paths of a directory as generation 1, and refuses when the
world already has a manifest. Run as a Job that mounts a stopped member's
claim read-only, it moves an existing world into the store. There is no
export in this version.

## 8. Limits of this version

- `storage.size` is not enforced on the node directory. A world can use as
  much of the node's disk as it writes.
- Only `OnDemand` groups.
- The cache is per node. A start on another node downloads the world even
  if a third node holds a current copy.
- Metrics: download duration and bytes per start, upload lag per world,
  worlds with a pending upload, lease conflicts and takeovers, orphans.

## 9. Testing

- Unit tests for the manifest diff, the pack format, the lease rules and the
  snapshot copy, against an in-memory S3 fake that implements `If-Match`
  with bare ETags and `If-None-Match`.
- The CSI node service against csi-sanity's node tests for ephemeral
  volumes.
- e2e in kind with an S3 server in the cluster: a member starts, writes,
  stops, starts on another node, and finds its data; a member whose node
  agent is killed during a run uploads after the restart; a delete leaves
  nothing under the prefix.
- The agent's wait and snapshot steps in the Paper image test.
