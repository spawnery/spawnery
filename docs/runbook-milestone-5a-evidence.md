# Runbook: a persistent server's world survives its pod

**Status: DRIVEN 2026-08-16, and the acceptance test passed.** Written
2026-08-15 at the end of milestone 5a's Task 7 against branch
`milestone-5a-persistent-groups`; driven the next day against master at
`f3c6fc1` by the human partner and the acting agent together, the way
`docs/runbook-milestone-4c1-evidence.md` §12 was. Blocks were placed at
**-74 / -10**, the pod was deleted, the client rejoined — and in the driver's
own words: *"ja die blöcke sind noch da."* Every step from §1 through §10 ran
as written; what the run corrected is corrected in place, and what it found
beyond the runbook is in `docs/known-issues.md` under "From the milestone 5a
evidence run" and in `docs/handover-milestone-5.md`.

**Four things this run corrected in place, all in what to *expect* rather than
what to do. No existing command below changed. Two were added: a query in §8
and a `sleep 5` in §10, each for a reason its own section gives.** §6's label
list was missing a fourth label the operator really sets. §8's promise that the
deleted `Server` is
observable — a deletion timestamp, then a `NotFound` — is wrong at any
polling rate a person can drive: the whole delete-and-recreate closed inside
a two-second sampling gap, and the UID is what shows it happened. §8 also did
not predict two artifacts of the event trail that look like defects and are
not (`PodAdopted`, and a `count: 2` on the new object's *first*
`ReadyGatePassed`). §9's instruction to disambiguate the rejoin by taking the
most recent line by timestamp is unnecessary here, for a reason better than
the rule it replaces.

**One finding this run produced that is not a runbook correction at all:** the
recreate path — the one this whole milestone rests on — logs a
`level=error` line with a full stacktrace every single time it runs, on the
happy path, in the very log §4 tells the driver to watch for real trouble.
`docs/known-issues.md` carries the trace, and is careful to say which part of
it the run established and which part it could not: three writes could have
produced that line, the log names none of them, and the entry declines to
guess.

This is a new document rather than a section of `docs/runbook-milestone-4c1-evidence.md`
or `docs/runbook-milestone-3-evidence.md`. Both of those measure the proxy
layer on a cluster shaped for it; this one measures a different claim —
storage — on a different cluster shape: a single-node `kind` cluster running
its *default* storage class, rather than the multi-node cluster 4c-3's node
drain needed or the one-node-is-required cluster 4c-1 through 4c-2 used.
Read `docs/runbook-milestone-4c1-evidence.md`'s own top-of-document note
before starting — it records what its own most recent run (§12, node drain)
corrected, and the shape a run of *this* document should be ready to correct
in the same way: predictions written before the run, timestamps trusted over
five-second polling, and every deviation recorded rather than smoothed over.

## What this measures, and why it cannot be argued with

`docs/superpowers/specs/2026-08-15-persistent-groups-design.md` §6 says what
envtest cannot show: there is no provisioner in envtest, so a claim there
never reaches `Bound` and a pod never runs. Every claim this milestone makes
about an *object* — that a `Persistent` group creates ordinal-named servers,
that each gets its own claim, that deleting a `Server` leaves the claim
standing — is proven at that level already, in
`internal/controller/server_controller_test.go` and
`internal/controller/persistent_test.go`. What none of that proves is that a
*world* survives, because envtest cannot make a claim bind or a pod boot Paper.

So the acceptance test here is the one thing that settles it beyond argument:
**place a block, delete the pod, rejoin, and the block is still there.** Not a
kubectl output, not a condition — a block placed by a person, in the game,
found again by that same person after the pod that held it is gone and a new
one has replaced it.

## 0. Prerequisites

**Read `docs/runbook-milestone-3-evidence.md` §0 and satisfy it.** Nothing
here is repeated: `x86_64-linux`, rootless Podman with `docker` aliased to it
or `CONTAINER=podman` passed explicitly, a `TMPDIR` on a real filesystem where
`/tmp` is a tmpfs, `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` exported
before the first `systemd-run --scope --user`, a clone of this repository, and
`nix develop`. `docs/runbook-milestone-4c1-evidence.md` §0 also records two of
those as conditional and measured false on this repository's own development
machine (`docker` already a Podman alias, `/tmp` not a tmpfs) — check your own
machine rather than copying that answer.

Beyond §0, this run needs what every manual session in this repository has
needed:

- **A licensed Minecraft Java Edition client at 26.2, protocol 776**, and a
  Microsoft account that owns the game. Paper 26.2 refuses any other protocol
  with a loud "Outdated client!" naming the version to install.
- **A person to drive that client**, for the whole of §7 through §9. Whether
  the block is still there after the pod comes back is a claim only a person
  looking at the screen can settle.
- **Network reach from the client's machine to the cluster host's NodePort
  30567.** If the client is on another machine, `docs/runbook-milestone-3-evidence.md`
  §10 has the SSH tunnel that needs nothing changed on the host.

**One property of `kind`'s own defaults this run depends on, worth knowing
before starting rather than discovering mid-run.** `kind` ships a default
`StorageClass` named `standard`, backed by Rancher's local-path provisioner,
with `volumeBindingMode: WaitForFirstConsumer` — a claim under it does not
bind until a pod actually mounts it. That is exactly the binding mode
`docs/superpowers/specs/2026-08-15-persistent-groups-design.md` §3.3 built
`BuildDataClaim` not to wait on, and exactly the shape of storage class
milestone 4c-3's own node-drain finding is about (`docs/known-issues.md`,
"A `Persistent` server on a node-pinned RWO volume may not be schedulable
anywhere else") — this run's cluster only ever has one node, so that finding
does not apply here; it belongs to whatever run first tries this against a
multi-node cluster.

**A second property, worth knowing even though it no longer blocks anything.**
`docs/known-issues.md`'s "From milestone 2b" section records that `fsGroup`
was missing from `podspec.BuildServerPod`'s `PodSecurityContext`, and that the
fix "has to land before the first persistent server exists." It landed inside
this same milestone, before this runbook was written against the branch that
carries it: `SecurityContext` now sets `FSGroup` to `10001` and
`FSGroupChangePolicy` to `OnRootMismatch` (verified against
`internal/podspec/server.go`), so a `PersistentVolumeClaim` arriving owned by
root no longer leaves the pod's uid 10001 unable to write into `/data`.

**This run's own cluster cannot exercise that fix either way, and that is
worth knowing before treating a clean run here as evidence of it working.**
`kind`'s local-path provisioner runs `mkdir -m 0777 -p "$VOL_DIR"` when it
provisions a volume (verified against `rancher/local-path-provisioner`'s own
`local-path-storage.yaml` on 2026-08-15), so the directory a claim binds to on
this cluster is world-writable regardless of `fsGroup` — the fix has nothing
to do here that the directory's own permissions weren't already doing. A real
cloud storage class handing back a directory owned by root at a narrower mode
is the case the fix exists for, and confirming it there is future work, not
something this runbook's `kind` cluster can settle. If Paper fails to start
against a freshly bound claim with a permissions error in its log on such a
cluster, `podspec.BuildServerPod`'s `PodSecurityContext` is the first thing to
check rather than assume fixed.

## 1. Build and load the images

Identical to `docs/runbook-milestone-4c1-evidence.md` §1: both Paper and
Velocity, from the working tree rather than a cached tag, because a proxy is
what turns a Mojang-authenticated client's connection into a Paper join this
milestone can measure at all — nothing in this run talks to a Paper server
directly.

```bash
cd /path/to/spawnery
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make image-load CONTAINER=podman
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make velocity-image-load CONTAINER=podman
```

Expect two image loads reporting `ghcr.io/spawnery/paper:26.2-0.2.0` and
`ghcr.io/spawnery/velocity:3.5.1-0.2.0` — the tags
`config/samples/network.yaml` names. Use whatever tags these two commands
actually print if a Paper or Velocity bump has moved them. Drop the `TMPDIR`
override if §0 established this machine does not need it.

## 2. Create a single-node `kind` cluster, with its default storage class

Deliberately the simplest cluster shape any runbook in this repository uses —
one node, `kind`'s own defaults, nothing added for storage. Verifying the
default storage class exists and is marked default is the one thing worth
doing before anything else, since everything from §5 onward depends on it
silently rather than by name (`spec.storage.storageClassName` is left unset
in §5's manifest for exactly this reason).

```bash
cat >/tmp/spawnery-5a-kind.yaml <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30567
        hostPort: 30567
EOF

systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind create cluster --name spawnery-5a \
  --config /tmp/spawnery-5a-kind.yaml
```

Only one NodePort is needed — 30567, the proxy's own port, in the shape
`docs/runbook-milestone-4c1-evidence.md` §2 already uses. There is no second
pin to arrange: with `replicas: 1` on both groups there is only ever one proxy
and one persistent server, so there is nothing to distinguish and nothing to
force a client onto.

```bash
nix develop -c kubectl get storageclass
```

**Expect exactly one `StorageClass`, named `standard`, provisioner
`rancher.io/local-path`, `(default)` after its name, `VOLUMEBINDINGMODE`
reading `WaitForFirstConsumer`.** If nothing is marked default, `spec.storage`
in §5 needs `storageClassName: standard` added explicitly — check this before
applying the manifest rather than after a claim sits unbound with no consumer
to explain why.

## 3. Load the images into the cluster and apply the CRDs

Identical to `docs/runbook-milestone-4c1-evidence.md` §3, with this section's
own cluster name:

```bash
nix build .#paper-image --out-link "$HOME/.cache/spawnery-tmp/paper-img"
nix build .#velocity-image --out-link "$HOME/.cache/spawnery-tmp/velocity-img"

for img in paper velocity; do
  systemd-run --scope --user --property=Delegate=yes --quiet \
    env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" \
    nix develop -c kind load image-archive \
    "$HOME/.cache/spawnery-tmp/${img}-img" --name spawnery-5a
done

nix develop -c kubectl apply -f config/crd/bases
podman exec spawnery-5a-control-plane crictl images
```

Expect both `ghcr.io/spawnery/paper:26.2-0.2.0` and
`ghcr.io/spawnery/velocity:3.5.1-0.2.0` in the `crictl images` list.

## 4. Run the operator outside the cluster, and hand-build what its pods dial

Unchanged from `docs/runbook-milestone-3-evidence.md` §4 and every runbook
since: the operator has no image of its own, a pod dials
`spawnery-operator.minecraft.svc:9443`, and nothing creates that `Service`
without a hand-built relay. See `docs/known-issues.md`, "From milestone 2c."

```bash
nix develop -c kubectl create namespace minecraft
nix develop -c go run ./cmd/spawnery-operator \
  --leader-elect=false --operator-namespace minecraft &

podman run -d --name spawnery-5a-relay --network kind \
  -v /nix/store:/nix/store:ro \
  --entrypoint "$(nix build --no-link --print-out-paths nixpkgs#socat)/bin/socat" \
  ghcr.io/spawnery/paper:26.2-0.2.0 \
  TCP-LISTEN:9443,fork,reuseaddr TCP:host.containers.internal:9443
RELAY_IP=$(podman inspect spawnery-5a-relay \
  --format '{{.NetworkSettings.Networks.kind.IPAddress}}')

nix develop -c kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: spawnery-operator
  namespace: minecraft
spec:
  ports:
    - name: agent
      port: 9443
      targetPort: 9443
      protocol: TCP
---
apiVersion: v1
kind: Endpoints
metadata:
  name: spawnery-operator
  namespace: minecraft
subsets:
  - addresses:
      - ip: $RELAY_IP
    ports:
      - name: agent
        port: 9443
        protocol: TCP
EOF
```

With a real Docker daemon rather than rootless Podman, skip the relay and
point the `Endpoints` at `172.17.0.1` directly, as milestone 3's own §4 notes.

**Leave the operator's log visible.** A reconcile that is erroring —
against the CRDs, the claim, the Service — says so there and nowhere else. A
claim stuck `Pending` past the point a pod should have consumed it and an
operator that has quietly stopped reconciling look identical from `kubectl`
alone.

## 5. Apply the network: one persistent group at `replicas: 1`, one proxy

The whole point of this manifest is the `type: Persistent` group and its
`storage` block; the `ProxyGroup` beside it exists only to let a licensed
client reach it, in the smallest shape that does — one proxy, `replicas: 1`,
routing to the persistent group as its only fallback.

**`storageClassName` is left unset deliberately**, so the claim falls onto
whichever class §2 confirmed carries the default annotation — `standard`, on
an unmodified `kind` cluster.

```bash
nix develop -c kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft
stringData:
  secret: 5a-evidence-run-forwarding-secret
---
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: evidence
  namespace: minecraft
spec:
  forwardingSecretRef:
    name: velocity-forwarding-secret
  defaults:
    minecraftVersion: "26.2"
    resources:
      requests:
        cpu: "1"
        memory: 2Gi
      limits:
        memory: 2Gi
---
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: survival
  namespace: minecraft
spec:
  networkRef:
    name: evidence
  type: Persistent
  replicas: 1
  image: ghcr.io/spawnery/paper:26.2-0.2.0
  maxPlayers: 20
  storage:
    size: 1Gi
  drain:
    timeoutSeconds: 60
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft
spec:
  networkRef:
    name: evidence
  replicas: 1
  image: ghcr.io/spawnery/velocity:3.5.1-0.2.0
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      memory: 1Gi
  expose:
    type: NodePort
    nodePort:
      port: 30567
  routing:
    fallbackGroups:
      - survival
  config:
    playerLimit: 20
    motd: "5a evidence run"
EOF
```

**`spec.scaling` and `spec.update` are absent, deliberately** — the CRD's own
CEL rules forbid both on a `Persistent` group (`self.type != 'Persistent' ||
!has(self.scaling)`, and the same for `update`), so including either would
reject the apply outright rather than being ignored.

Wait for both groups to reach `Ready`. Milestone 3's runbook allows 90 seconds
for a similar two-pod-plus-proxy startup and its own runs stayed well under
that; allow the same margin here and check early.

```bash
sleep 90
nix develop -c kubectl get network,servergroup,proxygroup,servers,pods,pvc -n minecraft
```

Expect, by shape rather than exact name — `survival`'s server carries no
random suffix, so its name is exact where the proxy's pod name is not:

```
NAME                                PHASE   READY   ADDRESS         PLAYERS
proxygroup.spawnery.cloud/gateway   Ready   1       <nodeIP>:30567  0

NAME                                    PHASE   READY   ...
servergroup.spawnery.cloud/survival     Ready   1       ...
```

one `Ready` `Server` named exactly `survival-0`, one `1/1 Running` pod named
exactly `survival-0`, one `gateway-*` proxy pod, and one `PersistentVolumeClaim`
named exactly `survival-0-data`, `STATUS: Bound`.

**If the claim reads `Pending` rather than `Bound` once the pod is
`1/1 Running`,** something is wrong with the storage class or its provisioner
— it is not this operator's to fix, since `BuildDataClaim` never waits on
`Bound` and never checks it. `kubectl describe pvc survival-0-data -n
minecraft` and `kubectl logs -n local-path-storage -l app=local-path-provisioner`
are where a `kind` cluster's own provisioner explains itself.

**If `survival` never leaves `Pending`, and specifically if the pod is stuck
`ContainerCreating` or `CrashLoopBackOff` rather than merely slow,** check the
pod's own events and its container log before assuming the operator is at
fault. A permissions error writing to `/data` is not expected on this
cluster — §0's second note above explains why this `kind` cluster's own
provisioner cannot surface one either way — but is still worth ruling out on
any other cluster this runbook gets run against.

## 6. Confirm the objects, before touching the world

Three things worth reading directly off the objects before a client ever
joins, because each is the specific claim this milestone makes and none of
them requires a person in the game to check.

```bash
nix develop -c kubectl get server survival-0 -n minecraft -o jsonpath='{.spec.ordinal}{"\n"}'
nix develop -c kubectl get pvc survival-0-data -n minecraft -o jsonpath='{.metadata.labels}{"\n"}'
nix develop -c kubectl get pvc survival-0-data -n minecraft -o jsonpath='{.metadata.ownerReferences}{"\n"}'
```

**Expect `0`** — the ordinal is on the object, not just implied by the name.

**Expect four labels: `spawnery.cloud/managed-by: spawnery-operator`,
`spawnery.cloud/server: survival-0`, `spawnery.cloud/group: survival`, and
`spawnery.cloud/network: evidence`.** The fourth was missing from this list
until the 2026-08-16 run printed it; `podspec.BuildDataClaim` sets it, and the
list was written from the three that carry meaning for the checks below.
Nothing depends on the network label here — it is named only so that reading
the real output against this line does not raise a question it cannot answer.
The first three are what matter: this
is what lets `kubectl get pvc -l spawnery.cloud/managed-by=spawnery-operator` find
every claim this operator has ever created, and what `docs/known-issues.md`'s
"From milestone 5a" section names as how to tell a live claim from an orphan.

**Expect the owner references to print nothing — an empty array.** This is
`podspec.BuildDataClaim`'s single most load-bearing property: the claim
carries no owner, so nothing garbage-collects it when its server, its group,
or the whole `Network` is deleted. If this ever prints a `Server` or a
`ServerGroup` as owner, that is the milestone's central safety property gone,
and it belongs in an incident report before it belongs in this runbook's
results.

## 7. Join, place a block, and note exactly where

Point the licensed client at `127.0.0.1:30567`, or the tunnelled port from §0
if the client is on another machine. Log in with the Microsoft account. Expect
to land in the `survival` world — there is only one fallback group, so
`Router.choose` has nowhere else to send a join.

**Confirm which pod actually accepted the join**, the way every prior runbook
in this repository insists on rather than assuming:

```bash
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/role=proxy \
  --prefix=true --tail=-1 --timestamps | grep 'has connected'
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/role=server \
  --prefix=true --tail=-1 --timestamps | grep 'joined the game'
```

Expect a `[connected player] <name> ... has connected` line from the proxy
pod and a `<name> joined the game` line from `survival-0` — Paper's own
`joined the game` line means the client completed the configuration phase and
Paper is actually counting the session, the same signal
`docs/handover-milestone-4.md`'s manual session used to distinguish a real
join from a held one.

**Now place one block, somewhere memorable, and write down exactly where.**
The specific block and the specific coordinates do not matter to this
milestone — what matters is that a person can look at the same coordinates
after §8 and say, unaided by any log, whether it is still there. `F3` shows
the player's own coordinates in the debug screen; note them, and note which
block was placed and its orientation if that matters to telling it apart from
the surrounding terrain. A single placed block against a naturally-generated
world is easy to mistake for a similar naturally-occurring block from a
distance — placing it somewhere structurally distinctive (on top of the
highest point nearby, or inside an otherwise-empty naturally-carved space)
costs nothing and removes the doubt.

Quit the client normally once the coordinates are recorded. `survival`
started at `spec.maxPlayers: 20`, and there is no drain to wait out here: this
run never scales the group down and never asks for the server's own deletion,
so the only thing about to happen to `survival-0` is the direct pod deletion
in §8, which is a different path from every drain this repository's other
runbooks exercise.

## 8. Delete the pod directly — not the `Server`, not the group

This is the step the milestone's whole claim rests on, and it is worth being
precise about which object this deletes and which path in the code it drives,
because it is not the drain path any other runbook in this repository
measures.

```bash
nix develop -c kubectl delete pod survival-0 -n minecraft
```

**What this actually does, read off `internal/phase/phase.go` and
`internal/controller/server_controller.go` rather than assumed:** deleting the
*pod* directly, with the `Server` object left untouched, is not a drain —
nobody asked `survival-0` to go away, its pod simply stopped existing out from
under it. The very next reconcile of `survival-0` finds `status.podName` set
and no pod behind it — `PodLost` — and `phase.Decide`'s `Pending`/`Starting`/`Ready`
branch answers that with `Next: Terminating, DeletePod: true` regardless of
which of those three phases `survival-0` was actually in. Because
`decision.Next == Terminating` is reached with `podFound` already false and
`srv.DeletionTimestamp` still zero — nobody deleted the `Server`, only its pod
— `server_controller.go`'s own comment calls this exactly: *"Terminating
without a deletion request means the state machine decided the server is
finished. Remove the object so the group creates a replacement."* It deletes
the **`Server` object itself**, not merely its pod. That delete lands on an
object still carrying the drain finalizer `ensureFinalizer` put there before
the pod ever existed, so it does not vanish instantly — it picks up a deletion
timestamp, and one further reconcile (now with the timestamp set) clears the
finalizer and lets the object actually go.

**So the ordinal is freed by the `Server` controller, in roughly two
five-second reconciles, not by anything the `ServerGroup` controller does
directly.** Once `survival-0` the object is actually gone, `DecidePersistentSize`
sees ordinal `0` missing on the group's very next pass and creates a **new**
`Server` object under the identical name `survival-0` — the ordinal is the
identity, and the name is deterministic from it. That new object's own
reconcile creates its claim before its pod, gets `AlreadyExists` on the claim
(`survival-0-data` is still sitting there, exactly as it was), and mounts it.

**Do not expect to catch the `Server` object mid-flight. The 2026-08-16 run
tried, at two-second sampling, and the entire cycle closed inside one gap.**
An earlier version of this section promised the deletion timestamp would show
briefly (`kubectl get server survival-0 -n minecraft -o
jsonpath='{.metadata.deletionTimestamp}'` printing something) and then a
`NotFound`. Neither was observable: at 11:23:00 the object carried UID
`582933f7…` and phase `Ready`; at 11:23:02 it carried UID `c7880082…` and
phase `Pending`. `PodLost`, the `Server` delete, the finalizer release, the
object's actual disappearance and the group's recreation of it all landed
between two samples two seconds apart. Chasing the gap with a tighter poll is
the wrong instinct — a five-second reconcile interval does not mean five
seconds of latency per step, and this path is watch-driven throughout.

**What is observable, and is the better evidence anyway, is the UID.** Record
`kubectl get server survival-0 -n minecraft -o jsonpath='{.metadata.uid}'`
before §8's delete and again after. A changed UID under an unchanged name is
precisely the milestone's claim — the ordinal is the identity, the object is
not — and unlike a timestamp glimpsed in a race, it is still true an hour
later. Do the same for the pod, and for the claim, where the point is the
opposite: **the claim's UID must not change.** The 2026-08-16 run measured
`Server` `582933f7…` → `c7880082…`, pod `788ef8f6…` → `fb8812de…`, and claim
`2b0f11b8…` → `2b0f11b8…`, created `11:20:22Z` before and after.

**Expect, over roughly the next half-minute:** a new `Server` named
`survival-0` in place of the old one; its pod up and `Ready` well within the
operator's `--startup-deadline` (five minutes by default — the 2026-08-16 run
took **22 seconds** from pod creation to `ReadyGatePassed`, with Paper's own
`Done (3.411s)`, nowhere near that bound); and `kubectl get pvc
survival-0-data -n minecraft` never once showing anything but the one claim
created in §5 — no second claim, no gap where it is missing, `STATUS: Bound`
throughout once it first bound.

**Paper's own boot line is worth reading here, though it settles nothing.**
The 2026-08-16 run's replacement logged `Done preparing level "world"
(0.162s)` — a world read, not generated. Treat it as a hint that §9 is about
to go well, never as a substitute for §9: a log line saying the level loaded
fast is not a person looking at a block.

```bash
nix develop -c kubectl get server survival-0 -n minecraft
nix develop -c kubectl get pvc survival-0-data -n minecraft
nix develop -c kubectl get events -n minecraft \
  --field-selector involvedObject.kind=Server,involvedObject.name=survival-0 \
  --sort-by=.lastTimestamp
nix develop -c kubectl get events -n minecraft \
  --field-selector involvedObject.kind=ServerGroup,involvedObject.name=survival \
  --sort-by=.lastTimestamp
```

**Two separate queries, because the events land on two different objects.**
`PodLost` and the later `PodCreated` are both recorded on the `Server`
(`r.Recorder.Eventf(srv, ...)` — `internal/controller/server_controller.go`),
so the first query is where to expect them: a `PodLost` reason from the phase
transition, and a `PodCreated` naming the new pod once the replacement comes
up. `ServerCreated`, by contrast, is recorded on the **`ServerGroup`**, not
the `Server` — `createPersistentServer` calls
`r.Recorder.Eventf(group, ...)`, not `Eventf(srv, ...)` — so it can never
appear under the first query's `involvedObject.kind=Server` filter, however
long you wait. The second query is where to find it, naming `survival-0` in
its message rather than as its `involvedObject`. Together the two show what
"the same claim, found by the same name" looks like as an event trail:
`PodLost` and `PodCreated` on the server named `survival-0` across two
different objects that carried that name, and `ServerCreated` on the group
in between them, naming the server it just (re)built.

**Two things in that trail look like defects and are not. Both surfaced on the
2026-08-16 run; neither was predicted here.**

**`count: 2` on the *new* object's first `ReadyGatePassed`.** The replacement
`Server` had gone ready exactly once, and its `ReadyGatePassed` event arrived
reading `COUNT 2`. Client-go's event aggregator keys on namespace, kind,
**name**, reason and type — not on UID — so the counter carried over from the
old object that had held the same name, while the event itself was written
against the new object's UID. Read as-is, the trail appears to say one server
went ready twice. Add `involvedObject.uid` to settle it, which is worth doing
for the whole trail rather than only this row:

```bash
nix develop -c kubectl get events -n minecraft \
  --field-selector involvedObject.kind=Server,involvedObject.name=survival-0 \
  -o custom-columns=REASON:.reason,COUNT:.count,OBJUID:.involvedObject.uid,LAST:.lastTimestamp \
  --sort-by=.lastTimestamp
```

Expect every row before the delete to carry the old UID and every row after it
the new one, splitting cleanly at `PodLost`. `ServerCreated` on the group is
the one place `count` means what it looks like — `COUNT 2` there really is two
creations of `survival-0`, because the group is genuinely one object across
both.

**`PodAdopted — adopted existing pod survival-0 after a lost status write`, on
the very first boot.** This is not part of the delete path at all; it fires
during §5, before anything is deleted. The Server controller created the pod
and its `Status().Update` lost an optimistic-concurrency race, so the next
reconcile found a pod it had no `status.podName` for and adopted it — the
recovery `server_controller.go`'s own comment above that branch describes,
working as designed.

Conflicts of this shape are not a startup phenomenon, which is worth knowing
before reading them as a symptom of something. The 2026-08-16 run logged
twelve `the object has been modified` errors across all three controllers, and
their timestamps cluster on **every state transition of the run**, not on its
first minute: three at the apply, two at first readiness, then one each at the
join, the leave, the pod delete, the replacement's readiness, and two more at
the rejoin. Every one self-healed on requeue and none changed an outcome
anywhere in this document. They are logged at `level=error` with a full
stacktrace all the same — see §4's note about leaving the operator log
visible, and the entry this run added to `docs/known-issues.md` about what
that costs a person actually watching it.

## 9. Rejoin, and confirm the block is where it was left

Point the client back at `127.0.0.1:30567` and log in again, then confirm from
§7's log command that the join landed on the new `survival-0` pod.

**There is nothing to disambiguate here, and the reason is better evidence
than the rule it replaces.** Other runbooks in this repository take the most
recent line by timestamp, because `kubectl logs -l` prints one pod's matches
and then the next's rather than interleaving by time. That rule is sound and
unnecessary on this path: the old pod was deleted in §8, so its log went with
it. The server-side grep returns **exactly one** `joined the game` line — the
rejoin — and the first join is simply not there to be confused with it. Read
it for what it is rather than as a formality: the pod that just accepted the
player has never seen that player before. It has the world and not the
session, which is the whole distinction this milestone exists to make. The
2026-08-16 run measured the first join at 11:21:08 against pod `788ef8f6…`
and the rejoin at 11:24:23 against pod `fb8812de…`, with only the latter
present in any surviving log. The proxy's own log, by contrast, spans both —
`gateway-ndh5` was never restarted — so it is where both connections appear
side by side.

**Go to the coordinates §7 recorded. This is the whole of the milestone's
claim, and only the person at the keyboard can settle it: is the block still
there?**

Record the answer in the person's own words, the way every manual session in
this repository's other runbooks does. There is no log line and no `kubectl`
output that substitutes for this — the claim `PersistentVolumeClaim`'s own
name states in the abstract, `DataClaimName`'s doc comment states in code, and
`TestDeletingAPersistentServerLeavesItsClaim` states at the object level, is
settled here at the one layer none of those can reach.

## 10. Clean up

```bash
pkill -x spawnery-operat
sleep 5                           # controller-runtime shuts down gracefully
ps -eo pid,comm | grep spawnery   # expect no output
podman rm -f spawnery-5a-relay
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind delete cluster --name spawnery-5a
rm -f /tmp/spawnery-5a-kind.yaml
```

`docs/runbook-milestone-4c1-evidence.md` §13 explains at length why
`pkill -x spawnery-operat` (fifteen characters, Linux's own truncation of
`spawnery-operator`) is the right incantation and `pkill -f` is not — signalling
by full command line risks matching the very shell driving this document, and
signalling `go run`'s own wrapper does not reach the compiled child it starts.
That reasoning is not repeated here; it applies unchanged.

**The `sleep 5` was added by the 2026-08-16 run, which briefly mistook a
correct `pkill` for a failed one.** The process was still in the table when
`ps` ran immediately after, because controller-runtime handles SIGTERM by
stopping its controllers, draining its work queues and shutting down the
metrics and health servers before exiting — the log's last line is `Wait
completed, proceeding to shutdown the manager`, several seconds after the
signal. If `ps` still shows it after five seconds, that is a real hang worth
looking at; before then it means nothing.

**Deleting the cluster deletes the claim along with it.** There is no
`kubectl delete pvc` step above and none is needed: `kind delete cluster`
tears down the whole control plane and every volume the local-path provisioner
backed with it. A run against a real cluster, where a claim genuinely outlives
its cluster's own churn, is the only kind of run where the accumulation
`docs/known-issues.md`'s "From milestone 5a" section describes is something to
go looking for by hand.

## Where this goes

The coordinates recorded in §7, the event trail from §8, and the answer to
§9's one question — in the driver's own words — belong in
`docs/handover-milestone-5.md`, in the same shape
`docs/handover-milestone-4.md` records milestone 3's manual session and
4c-1/4c-2/4c-3's own evidence runs: what was measured, what (if anything) this
document's own steps needed correcting in place, and — if the block is not
where it was left — a defect report against this milestone's central claim,
not a note that the runbook needs fixing.
