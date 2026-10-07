# Worlds in an object store

An on-demand group whose worlds live in an S3 bucket instead of on a claim per
member. It joins the Network of `config/samples/network.yaml`, and the
installation needs the chart's `worldSync` values first (below):

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: private-servers
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: OnDemand
  image: ghcr.io/spawnery/purpur:26.3-0.24.3
  maxPlayers: 10
  maxInstances: 200
  storage:
    backend: ObjectStore
    # Required by the schema, not enforced on the node; see Limits.
    size: 2Gi
    # Exactly what is synced.
    keep:
      - world
      - plugins/Example/data
```

Members are started, stopped and deleted as in
[Private servers](on-demand-servers.md). What changes is where the world is
between runs, and what a start waits for.

## What the backend does

A member gets `/data` from a directory on the node it runs on, through the CSI
driver `worldsync.spawnery.cloud`. A node agent, `spawnery-worldsync`, runs on
every node as a DaemonSet and fills that directory from the bucket, uploads it
back, and holds a lease in the bucket so that a world is written by one node
at a time.

At a start, the node agent takes the lease and reads the world's manifest.
When its own copy is current, the world is ready at once; otherwise it
downloads the files `keep` matches, 32 requests at a time. The container
starts meanwhile, and the Paper agent holds the server back from reading the
world until the download is done. It waits 10 minutes at most, and a failed
download ends the run with exit code 1.

While the member runs, the agent turns autosave off every snapshot interval,
runs `save-all flush` and asks for a snapshot. The node agent copies what
changed to a directory beside the world and answers, autosave goes back on,
and the upload runs from the copy in the background.

After the container ends, the node agent takes a last snapshot and answers the
kubelet. The upload follows in the background and retries until it succeeds,
and then the lease is released. The node keeps its copy as a cache for the
next start.

A start on the same node can begin while the previous run still uploads; it
waits only for an upload attempt already in flight. A start on another node
waits in `ContainerCreating` until the lease is released, or, if the old node
is gone, until the lease has been silent for 10 minutes.

## Installing the node agent

The bucket is cluster configuration, set once in the chart; game namespaces
never see its credentials. The operator gets the same settings, because it
marks deleted worlds in the bucket and sweeps them.

```yaml
# worldsync-values.yaml
worldSync:
  enabled: true
  namespace: spawnery-worldsync
  objectStore:
    endpoint: https://s3.example.net
    region: eu-central
    bucket: example-worlds
    prefix: ""
    credentialsSecret: worldsync-s3
  snapshotInterval: 5m
```

The node agent runs privileged: it bind-mounts worlds into the kubelet's pod
directories. Its namespace must admit privileged pods, so it cannot be an
operator namespace held to the `restricted` Pod Security level. The chart
creates neither the namespace nor the Secret, and the Secret has to exist in
both namespaces:

```bash
kubectl create namespace spawnery-worldsync
kubectl label namespace spawnery-worldsync pod-security.kubernetes.io/enforce=privileged
for ns in spawnery-system spawnery-worldsync; do
  kubectl create secret generic worldsync-s3 -n "$ns" \
    --from-literal=AWS_ACCESS_KEY_ID="$KEY_ID" \
    --from-literal=AWS_SECRET_ACCESS_KEY="$SECRET"
done
helm upgrade spawnery oci://ghcr.io/spawnery/charts/spawnery --version 0.24.0 \
  --namespace spawnery-system --reuse-values -f worldsync-values.yaml
```

With `enabled: true` the operator runs with `--world-sync`. Without it, a
member of an `ObjectStore` group gets no pod (`Accepted` is `False`, reason
`WorldSyncOff`) and `deleteServer` refuses it.

The DaemonSet tolerates every taint and runs at `system-node-critical` by
default: a member scheduled to a node without the node agent never gets its
volume. Narrow it with `worldSync.nodeSelector` and `worldSync.tolerations`
only together with the game pods' own scheduling. Every value is listed in
[Chart values](../reference/chart-values.md).

The store has to support conditional writes, `If-None-Match: *` and
`If-Match` on `PutObject`, and accept the ETag in `If-Match` without quotes,
which is the form the node agent sends. Requests use path-style addressing.

## What the group needs

The API refuses `ObjectStore` without `type: OnDemand` and `keep`. `keep` is
exactly what is synced, so list every level directory and every plugin
directory that holds state. Paper 26.3, which the example's image runs, keeps
all dimensions of a world inside its level directory
(`world/dimensions/minecraft/the_nether` and `.../the_end`) and moves the
`world_nether` and `world_the_end` directories of older versions in there at
its first start, so `world` covers the nether and the end. On older versions,
list `world_nether` and `world_the_end` as well.

What `keep` does not match stays on the node and never reaches the bucket.
When a member stops, the node agent deletes those paths, except one that holds
a world by the prune's rule (a `level.dat`, a `region` directory or an `.mca`
file) and that `replace` does not list. Such a path stays on the node, every
snapshot of a running member fails with `spec.storage.keep does not keep
<path>, which holds a world`, and the next start on that node is refused by
the prune as it would be on a claim. The snapshot at a stop still saves what
`keep` matches. Add the path to `keep`, or to `replace` if the sources ship it.

The group's image needs spawnery's agent 0.24.0 or later, as in the
`purpur:26.3-0.24.3` of the example. Its Paper bootstrapper is what holds the
server back until the world is on disk. An older image, or a custom one
without the agent, starts the server on a half-downloaded world, which the
server can then corrupt. The operator cannot tell which agent an image
carries.

`size`, `storageClassName`, `accessModes` and `annotations` stay valid and are
ignored, so a group can switch back to `Claim` without touching the immutable
fields. Switching either way moves no data: claims stay where they are, and so
does the bucket. A running member keeps the backend it started with, and the
switch reaches it at its next start.

The node agent gives what it writes to the pod's `fsGroup`, which spawnery's
server pods always set.

## Plugins that touch the world early

Paper reads `level.dat` and the datapacks before any plugin's `onLoad`, so the
agent waits in a Paper bootstrapper of `SpawneryAgent`. A plugin's `onLoad`
and `onEnable` run after that wait and need nothing. A plugin whose own
bootstrapper reads or stages world files must have Paper run the agent's
bootstrapper first, in its `paper-plugin.yml`:

```yaml
dependencies:
  bootstrap:
    SpawneryAgent:
      load: BEFORE
```

Without it, the plugin may see a world that is still downloading, or write
files that the download then replaces.

## Snapshots and what a node loss costs

`worldSync.snapshotInterval` (operator flag `--world-sync-snapshot-interval`,
default 5 minutes) is how often a running member asks for a snapshot. Changing
it restarts nothing; members pick it up at their next start.

A snapshot freezes saving for the flush and for a copy on the node's own disk,
not for the upload. Files under 64 KiB are copied every time; larger ones only
when their size or modification time changed. A file that keeps changing
while it is copied fails that snapshot after three attempts, and the next one
tries again.

If a node dies, its running worlds lose the play since their last snapshot
that finished uploading: at most the interval, plus an upload that was still
running. After 10 minutes another node may take the lease and start the
world from the bucket. When the dead node comes back, it finds its leases
taken and moves its copies to `orphans/` under the node directory
(`worldSync.hostPath`), where they stay for manual recovery and never upload.
A bucket that stays unreachable for more than 10 minutes after a stop has the
same effect: another node may take the lease and start from the last upload,
and the stopped run's copy goes to `orphans/` once the bucket answers again.

A stop always uploads, whether or not the agent ever asked for a snapshot.

## Deleting a world

`deleteServer(group, key)` writes a deletion marker into the bucket and then
deletes the member's `Server`. The operator sweeps marked worlds once a minute, as
soon as no node holds their lease. Until the sweep is done, `startServer` for
the same key answers `UNAVAILABLE`; after it, the key starts an empty world.
Without `--world-sync`, `startServer` refuses every key of an `ObjectStore`
group.

## Moving existing worlds in

`spawnery-worldsync import` uploads the `keep` paths of a directory as the
first generation of a world, and refuses a world that already has a manifest.
Run it as a Job that mounts a stopped member's claim read-only. The claim is
in the game namespace, so the Job runs there and needs the credentials Secret
there for the import; delete it afterwards.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: import-0b5c1c82
  namespace: minecraft
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      securityContext:
        runAsNonRoot: true
        runAsUser: 10001
        runAsGroup: 10001
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: import
          image: ghcr.io/spawnery/spawnery-worldsync:0.24.0
          args:
            - import
            - --world=minecraft/private-servers/0b5c1c82-4c7f-4a6e-9d1b-2c1f2b9d5e10
            - --dir=/world
            # spec.storage.keep, one entry per line
            - |-
              --keep=world
              plugins/Example/data
          env:
            - name: WORLDSYNC_ENDPOINT
              value: https://s3.example.net
            - name: WORLDSYNC_REGION
              value: eu-central
            - name: WORLDSYNC_BUCKET
              value: example-worlds
            - name: WORLDSYNC_PREFIX
              value: ""
            - name: AWS_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef: {name: worldsync-s3, key: AWS_ACCESS_KEY_ID}
            - name: AWS_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef: {name: worldsync-s3, key: AWS_SECRET_ACCESS_KEY}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
          volumeMounts:
            - name: world
              mountPath: /world
              readOnly: true
            # The image has no /tmp; the import stages its snapshot there.
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: world
          persistentVolumeClaim:
            claimName: private-servers-0b5c1c82-4c7f-4a6e-9d1b-2c1f2b9d5e10-data
            readOnly: true
        - name: tmp
          emptyDir: {}
```

The world name is `<namespace>/<group>/<key>`, the claim `<group>-<key>-data`.
The import copies the world into `/tmp` before uploading it, so the `emptyDir`
needs room for one world.

Import every world before the group switches to `ObjectStore`, with its member
stopped. A member that starts under `ObjectStore` before its import begins an
empty world, and once that world has uploaded, the import refuses the key.
There is no export in this version.

## Limits

- `storage.size` is not enforced on the node directory. A world can use as
  much of the node's disk as it writes.
- Only `OnDemand` groups.
- The cache is per node. A start on another node downloads the world even if a
  third node holds a current copy. A cache nobody uses is evicted after 24
  hours, or earlier, oldest first, when the node directory's filesystem has
  less than 15 % free.
- Objects and packs written by an upload attempt that failed stay in the
  bucket until the world is deleted.
- The node agent serves metrics on port 8090, which the chart does not scrape:
  `spawnery_worldsync_download_seconds`, `spawnery_worldsync_pending_snapshots`,
  `spawnery_worldsync_lease_conflicts_total` and
  `spawnery_worldsync_orphans_total`.

The design and its measurements are in
`docs/superpowers/specs/2026-10-06-object-store-worlds-design.md`.
