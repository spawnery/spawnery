# On-demand worlds in an object store

**Status:** implemented (branch feat/object-store-worlds)
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
   creates a claim for such a group, and marks the world for deletion in the
   store when the agent channel asks to delete a member; a sweeper in the
   operator deletes it (§5.1).
3. **The Paper agent** holds the server in a bootstrapper, before Paper
   reads the world, until the world is on disk, and asks for a consistent snapshot every few minutes while
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
which `hostPath` is not. The pod also gets `SPAWNERY_WORLD_SYNC=1`, which the
agent reads. `SPAWNERY_WORLD_SYNC_INTERVAL` is added after the desired-state
hash is taken, like the AOT cache volume, so a new interval restarts nothing.

The desired-state hash is taken over the rendered pod, so switching the
backend changes it. Groups on `Claim` render the same pod as before, and the
hash goldens do not move. A member without `spec.key` would have a world
without a name; the operator creates no pod for it (`Accepted` is `False`,
reason `ServerKeyMissing`). Without `--world-sync` it creates no pod for any
member of such a group (reason `WorldSyncOff`).

### 3.3 The control directory

The node agent and the game server talk through files in
`/data/.spawnery-worldsync/`:

| file | written by | meaning |
|---|---|---|
| `ready` | node agent | every file of the world is on disk |
| `failed` | node agent | the download failed; the file holds the reason |
| `snapshot.request` | agent | a sequence number, asking for a snapshot |
| `snapshot.done` | node agent | the sequence number of the finished copy, or `failed <seq> <reason>` |

The node agent empties the directory at every publish. The prune never
deletes it, whatever the backend, and it is not synced.

### 3.4 The agent

- **Wait.** Paper's `Main` reads `level.dat` and the datapacks before any
  plugin's `onLoad` (checked with `javap` on the pinned Paper bundle on
  2026-10-06), so waiting in `onLoad` would let Paper create fresh world data
  over a world that is still downloading. The wait therefore runs in a Paper
  bootstrapper of `SpawneryAgent` (`bootstrapper:` in its `paper-plugin.yml`),
  which Paper runs before it touches the world. It waits for `ready` or
  `failed`. On `failed`, or when neither file appears within 10 minutes, it
  writes the reason to stderr and halts the JVM with exit code 1, so no
  shutdown hook saves a half-downloaded world. A plugin that reads or stages
  world files in its own bootstrapper declares
  `dependencies.bootstrap.SpawneryAgent.load: BEFORE`; a plugin's `onLoad`
  runs after every bootstrapper and needs nothing.
- **Snapshot.** From `onEnable`, every `SPAWNERY_WORLD_SYNC_INTERVAL` (default
  5 minutes), the agent runs step 1 of §4.4 on the main thread and waits off
  the main thread for `snapshot.done`. While it waits, autosave stays off. It
  restores each world's previous autosave setting when the answer arrives,
  after 60 s, or when any step throws. An interval that comes while the last
  snapshot is still waited for is skipped. The node agent answers only
  sequence numbers above the last one it answered, so the agent starts above
  any number already in `snapshot.request` or `snapshot.done`; a restarted
  container would otherwise go unanswered.

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
- `objects/<sha256>`: one object per file of 64 KiB or more, by content.
  Region files are already compressed and are stored as they are. The store
  bills at least 64 kB per object, so smaller ones would cost more than they
  hold. An object that already exists (`HEAD`) is not uploaded again.
- `packs/<generation>-<16 hex>.tar.gz`: every smaller file of that
  generation in one gzipped tar, under a fresh random name per upload
  attempt. A writer that loses the manifest race thus never overwrites the
  winner's pack. A pack is always fetched whole, and a download fails when a
  pack lacks an entry the manifest names.
- `lease.json`: `{node, pod, renewedAt}`.

A manifest is written last, with `If-Match` on the previous manifest's ETag
(or `If-None-Match: *` for the first). Objects and the pack the previous
manifest named and the new one does not are deleted after the new manifest is
in place. Objects and packs of an attempt that failed before its manifest
stay until the world is deleted.

The SDK retries a conditional `PutObject` after a 5xx or a reset connection.
When the first attempt had committed, the retry fails its own condition with
412, so a 412 on the manifest is read back: a stored manifest byte-equal to
the one sent is this upload's. The node agent goes further after a conflict
while its lease is unbroken: a stored manifest with the same `worldId`, the
next generation and exactly the snapshot's paths, sizes and mtimes is
adopted, and the packs it does not name are deleted. Any other conflict
orphans the local copy (§4.2).

### 4.2 The lease

- **Take:** create `lease.json` with `If-None-Match: *`. A lease with an
  empty `node` is free and is taken with `If-Match` on its ETag.
- **Hold:** rewrite it with `If-Match` every 30 s while the world is on the
  node with a pod or a pending upload, and once more right before every
  upload, so a node that was cut off learns that it lost the world before it
  writes a manifest.
- **Take over:** allowed when the lease is stale, with `If-Match` on the
  stale ETag, so two nodes cannot both win. This covers a node that died.
- **Release:** after the last upload, overwrite it with a tombstone (empty
  `node`) under `If-Match`. A delete cannot be conditional on the ETag, and a
  delete after a check could remove a successor's lease.
- **Lost:** a node that finds its lease taken over stops writing that world.
  `If-Match` on a key that no longer exists answers 404 on S3; that counts as
  lost too. A 412 on renewal is read back first: a lease that still names this
  node is a write that committed and whose SDK retry failed the condition,
  and its ETag is adopted. Otherwise the node moves its local copy to
  `orphans/` on the node and never uploads it. The copy is kept for manual
  recovery and counted in a metric. A pod still running on it keeps running
  on the moved copy; its snapshot requests are answered `failed`.

Staleness is judged by the store's clock alone: a lease is stale when the
`Date` of the response that read it is more than 10 minutes after its
`LastModified`. `renewedAt` is the holder's clock and only informational, so
clock skew between nodes cannot shorten a lease. A response without either
time fails the take instead of guessing.

### 4.3 NodePublishVolume

Called by the kubelet before the containers start.

0. Refuse a `world` whose namespace is not the pod's
   (`csi.storage.k8s.io/pod.namespace`), with `PERMISSION_DENIED`. Anyone
   who may create a pod can name the driver and any world in its
   attributes; without this check a pod could mount another namespace's
   world. A malformed `world` (not three non-empty segments, or `.` or `..`
   among them), an empty or malformed `keep`, a volume that is not inline
   ephemeral, and an empty volume ID or target path are `INVALID_ARGUMENT`.
1. Wait for an upload of this world that is in flight, for as long as the
   kubelet's call lasts (`UNAVAILABLE` after). A publish for the same target
   again, as after a node reboot, binds it again if the mount is gone. A
   publish for another target while the world is still published elsewhere
   on this node is the old pod's teardown arriving late: it is treated as
   that target's unpublish first.
2. Refuse the world with `UNAVAILABLE` while its deletion marker (§5.1)
   exists.
3. Take the lease, or confirm this node holds it. If another node holds a
   fresh lease, return `UNAVAILABLE`; the kubelet retries with back-off and
   the pod stays in `ContainerCreating`. This is the case of a world
   started elsewhere while the old node still uploads.
4. Read `manifest.json`.
   - No manifest: a new world. A local copy of an earlier world under that
     key is wiped. Mark it ready at once.
   - A manifest whose `worldId` and `generation` match the local state: the
     cache is current, also when snapshots of this node still wait to upload
     on top of it. Mark it ready at once.
   - Otherwise: when the local state holds snapshots it has not uploaded,
     move it to `orphans/`; else delete the keep paths. Start the download
     in the background, 32 requests at a time. Write `ready` when every file
     is on disk and fsynced, or `failed` with the reason.
5. Give the world to the pod's `fsGroup` (§4.7) and bind-mount it onto the
   target path.

The call returns after the deletion check, the lease and one manifest read,
about 0.1 to 0.3 s by §2, so the container starts while the download runs.

### 4.4 Snapshots while the server runs

The agent asks, the node agent copies, and the upload runs from the copy:

1. The agent turns autosave off on every world, runs `save-all flush`, and
   writes `<control>/snapshot.request` with a sequence number.
2. The node agent sees the request (it polls every 500 ms; inotify does not
   cross the bind mount reliably enough to depend on). It copies every file
   below 64 KiB that a keep entry matches, and every larger one whose size
   or mtime differs from the newest queued snapshot, or from the uploaded
   state when none is queued, to `snapshots/<seq>/` on the same filesystem.
   It checks size and mtime again after each copy. A file that changed
   during its copy is copied again, up to three times; after that the
   snapshot fails and the agent is told so. Unchanged large files are
   carried over by reference.
3. The node agent writes `snapshot.done` with the sequence number. The agent
   turns autosave back on. The world was frozen for the flush and the local
   copy, not for the upload.
4. The upload of `snapshots/<seq>/` and the new manifest run in the
   background, one snapshot after another per world. A snapshot taken while
   an upload runs is queued behind it.

Uploads run outside the world's lock, so a slow upload holds up neither
snapshot requests nor lease renewals. Every short store call has a deadline
of 30 s and every upload attempt one of 10 minutes; a hung call would
otherwise hold a world's lock. A failed attempt is retried from the same
snapshot with back-off from 1 s, doubling, to at most 1 minute, without a
limit on attempts.

The agent asks every 5 minutes by default. The interval is the most play
that is lost when a node dies (§5), plus an upload that was still running.

### 4.5 NodeUnpublishVolume

Called after the pod's containers ended. The server saved on shutdown, so
the directory is consistent.

1. Unmount the bind mount, stop a download that still runs, take a final
   snapshot (the copy of §4.4 step 2, with nothing running that could change
   a file) and return. The kubelet waits for the local copy only, not for
   the upload. A final snapshot that fails is retried in the background.
2. In the background: upload the queued snapshots and write the manifests,
   with the retries of §4.4. Then delete the non-keep paths from the local
   directory and release the lease, unless a pod has published the world
   again on this node in the meantime.

Because the upload runs from the snapshot, the same world may start again
on this node while its last stop is still uploading (§4.3 step 1 waits only
for the attempt in flight).

The local keep paths stay as a cache. A cache that is clean and has no pod
is evicted after 24 hours, or earlier, oldest first, when the directory's
filesystem has less than 15 % free. Eviction runs once a minute.

### 4.6 Restarts of the node agent

Per world, `worlds/<namespace>/<group>/<key>/state.json` on the node records
the `worldId`, the uploaded generation and its files, the manifest and lease
ETags, the keep list, the pod and its `fsGroup`, the published target path
if any, the queued snapshots, the last answered request, and whether a
download or a final snapshot was left unfinished. On start the node agent
rereads every state file, resumes lease renewal and pending uploads, and
resumes watching published volumes for snapshot requests. A world whose
download the restart cut off gets `failed`, and its server ends. The bind
mounts of running pods are kernel mounts in the kubelet's directory and
survive the node agent's restart; the DaemonSet mounts the kubelet's pod
directory and its own root with `Bidirectional` propagation for that reason.

### 4.7 What the node agent trusts

The data directory is writable by the pod, and the node agent runs as root.
A pod may swap any entry below it for a symlink, a FIFO or a device node at
any moment. So every access below a data directory is confined: no symlink
is ever followed. Paths are walked one component at a time from an open
directory with `O_NOFOLLOW`, and the last component is handled with `*at`
calls. A file is opened as `O_PATH`, checked to be regular, and only then
reopened, with `O_NONBLOCK` so that a file lease the pod holds fails the open
instead of stalling it. A file is placed through a temporary file and a
rename, which replaces a symlink rather than writing through it. Relative
paths over 4096 bytes are refused, which also bounds a walk a pod could make
arbitrarily deep. Other systems than Linux get no such walk and refuse.

Server pods run as a non-root user. The `CSIDriver` sets `fsGroupPolicy:
File` and the plugin advertises `VOLUME_MOUNT_GROUP`, so the kubelet hands it
the pod's `fsGroup` and skips its own ownership pass, which would run once
after the publish while the download still writes. Everything the node
agent creates or places below the data directory, now and later, goes to
that group, group-writable, with setgid directories; a cache from an earlier
run is regrouped at publish. Symlinks, FIFOs and devices a pod planted are
left alone.

## 5. Failure cases

- **The store is unreachable at start.** The lease cannot be taken, so the
  start waits, also when the cache on this node is current. Without the
  lease there is no proof that no other node writes the world.
- **The download fails.** The node agent writes `failed`. The agent stops
  the server with an error, and the member ends as `Failed`; the next start
  of the key tries again.
- **The store is unreachable at stop.** The upload retries until it works.
  The world stays on that node and can start there again; a start on
  another node waits for the lease.
- **A node dies.** Its worlds lose the play since their last uploaded
  snapshot, at most the snapshot interval plus an upload in flight. After
  10 minutes another node may take the lease and starts from the last
  manifest. When the dead node returns it finds its leases taken and moves
  those worlds to `orphans/`.
- **A pod is deleted while a snapshot copy runs.** The unpublish waits for
  the copy, which holds the world's lock, and takes the final snapshot after
  it.
- **The agent never asks for a snapshot** (an older agent, or a group whose
  plugins crash it): the world is still uploaded at every stop. Only the
  protection against node loss is missing.
- **The world is deleted** (§5.1).

### 5.1 Deleting a world

`DeleteServer(group, key)` deletes the `Server`, as today, and then writes a
marker `<prefix>/.deletions/<namespace>/<group>/<key>`. The markers live
under one prefix of their own so that finding them is one listing, not a
walk over every world. A runnable in the operator, under leader election,
lists them once a minute. For each world whose lease is absent, released or
stale it deletes every key under the world's prefix except the manifest,
then the manifest, then the marker. The marker goes last, so a sweep cut off
halfway is finished by the next one, and nodes refuse the world until then.
`DeleteServer` answers `NOT_FOUND` when there is neither a `Server`
nor a manifest, and refuses while the operator runs without `--world-sync`.

A start of the same key while its marker exists is refused by the node agent
(`UNAVAILABLE`) until the deletion finished, then begins as a new world with
a new `worldId`. The node checks the marker before it takes the lease. A
marker written between that check and the lease can let the sweep run under
the starting member; its next manifest write then fails, and the node
orphans its copy of a world that was being deleted anyway.

## 6. The chart

```yaml
worldSync:
  enabled: false
  namespace: ""                  # the release namespace when empty
  image:
    repository: ghcr.io/spawnery/spawnery-worldsync
    tag: ""                      # the chart's appVersion when empty
    digest: ""
  registrarImage: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.18.0
  objectStore:
    endpoint: ""
    region: ""
    bucket: ""
    prefix: ""
    credentialsSecret: ""        # keys AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
  hostPath: /var/lib/spawnery/worldsync
  snapshotInterval: 5m
  nodeSelector: {}
  tolerations:
    - operator: Exists
  priorityClassName: system-node-critical
  resources: {requests: {cpu: 50m, memory: 64Mi}, limits: {memory: 512Mi}}
```

With `enabled: true` the chart renders:

- the `CSIDriver` object (`attachRequired: false`, `podInfoOnMount: true`,
  `fsGroupPolicy: File`, `volumeLifecycleModes: [Ephemeral]`). The namespace
  check of §4.3 depends on `podInfoOnMount`: the kubelet sets the
  `csi.storage.k8s.io/*` attributes itself, whatever the pod wrote.
- the DaemonSet with the node agent and `node-driver-registrar`, privileged,
  with a ServiceAccount that mounts no token, in `worldSync.namespace` (the
  release namespace when empty). That namespace must admit privileged pods;
  an operator namespace held to `restricted` does not, which is why the two
  are separate. The chart does not create the namespace. The DaemonSet
  tolerates every taint and runs at `system-node-critical` by default,
  because a game pod on a node without the node agent never gets its volume
  and an evicted node agent strands the worlds it holds. Its objects carry
  labels of their own (`app.kubernetes.io/component: worldsync`) without the
  operator's selector pair, so nothing that selects the operator matches
  them. The node agent serves metrics on port 8090; the chart does not
  scrape them.
- `--world-sync` and `--world-sync-snapshot-interval` on the operator, and the
  same `WORLDSYNC_*` environment as the node agent from one helper, so both
  name the same bucket and prefix. The operator reads the secret for
  deletions, so the secret has to exist in both namespaces.

The game namespaces never see the credentials. The snapshot interval
reaches the agent as `SPAWNERY_WORLD_SYNC_INTERVAL` through the operator.

## 7. Import

`spawnery-worldsync import --world <namespace>/<group>/<key> --dir <path>
--keep <entries>` uploads the keep paths of a directory as generation 1, and
refuses when the world already has a manifest: the manifest is written with
`If-None-Match: *`, after the objects. It copies the world into a temporary
directory first, and the image has no `/tmp`, so the Job mounts an
`emptyDir` there. Run as a Job that mounts a stopped member's claim
read-only, it moves an existing world into the store. There is no export in
this version.

## 8. Limits of this version

- `storage.size` is not enforced on the node directory. A world can use as
  much of the node's disk as it writes.
- Only `OnDemand` groups.
- The cache is per node. A start on another node downloads the world even
  if a third node holds a current copy.
- Objects and packs of upload attempts that failed before their manifest
  stay in the bucket until the world is deleted.
- Downloads have no deadline, and the content of an object is not checked
  against its hash on download.
- Metrics: `spawnery_worldsync_download_seconds` (duration of a download at
  publish), `spawnery_worldsync_pending_snapshots` (snapshots on the node not
  yet in the bucket), `spawnery_worldsync_lease_conflicts_total` (publishes
  refused because another node held the world) and
  `spawnery_worldsync_orphans_total`. Bytes per start, upload lag per world
  and takeovers are not measured.

## 9. Testing

- Unit tests for the manifest diff, the pack format, the lease rules, the
  snapshot copy, the node agent's publish, snapshot, upload, release and
  restart paths, and the deletion sweep, against an in-memory store that
  implements `If-Match` with bare ETags, `If-None-Match`, 404 for `If-Match`
  on a missing key, and the `Date` and `LastModified` times. The S3 client
  is tested against an HTTP fake for a bare `If-Match`, `If-None-Match: *`
  and the mapping of status codes.
- Confinement tests that plant symlinks, FIFOs and deep directory chains
  under a data directory, and group tests for what the node agent creates
  under a pod's `fsGroup`.
- The CSI node service against its own tests for the namespace check,
  malformed attributes, `UNAVAILABLE` for a held world and the mount group
  capability. csi-sanity is not used.
- The Paper agent's control-file handling in JUnit, and a Go test that reads
  the Kotlin constants for the file and environment names.
- e2e in kind with MinIO in the cluster (`make e2e-worldsync`): a member
  starts, writes, stops, starts on another node, finds its data, and a
  delete leaves nothing under its prefix. As of 2026-10-06 that run has not
  happened yet. A node agent killed during a run has no e2e test.
