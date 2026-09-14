# Runbook: ordered shutdown, `Recreate` updates and storage growth, on a real cluster

**Status: DRIVEN 2026-08-16, and every acceptance test it ran passed.** Written
the same day at the end of milestone 5b's Task 10 and driven immediately after
the merge, against `master` at `e66aeae`, by the human partner and the acting
agent together — the way `docs/runbook-milestone-5a-evidence.md`'s own run was.

**Four of the five acceptance tests were driven; §11 was deliberately skipped,
and the reasoning is worth more than the result would have been.** §8's update
took `survival-1` down first and did not touch `survival-0` until `survival-1`
read `Ready` again, and both blocks survived — in the driver's own words: *"auf
beiden servern ist noch die selbe welt mit den selben blöcken."* §9's scale-down
removed only the top ordinal and left its claim standing. §10's refusal
appeared as `StorageResize=False StorageResizeRefused` with `Degraded` staying
`False`. §12's `motd` reached the client's server list, which it could not have
done before this milestone.

**Every command below ran as written and none needed correcting** — unlike
`docs/runbook-milestone-5a-evidence.md`'s own run, which corrected four
expectations and added two commands. (It is not the first such run here:
`docs/runbook-milestone-3-evidence.md`'s note records the same of its own.)
The reason is that 5a's lesson was applied *before* this run rather than
learned during it: **no test here tries to catch a transition mid-flight.**
§8's whole observation is a UID that changed against one that had not yet, and
that was readable at a four-second poll with room to spare.

**Why §11 was skipped, recorded so nobody re-derives it.** The positive growth
path would have proven mostly what is already proven: that `growClaim` patches
a claim upward is pinned by Task 7's envtest against a *real* `StorageClass`
with `allowVolumeExpansion: true`, with a mutation confirming the test dies
when `growClaim` does nothing. The one link never exercised end to end is
narrower — **does a real driver set `FileSystemResizePending`, and does the
ordinal then restart exactly once and come back larger?** — and
`csi-driver-host-path` is unlikely to show it, because it expands online and
so may never set that condition at all. §11 would have cost real setup to
re-prove the proven while leaving the unproven untouched, and a half-proof that
reads like a whole one is worse than an open note. **That link belongs on a
real cloud storage class that expands offline**; until someone drives it there,
it is envtest-only, with the condition hand-written.

**Read `docs/runbook-milestone-5a-evidence.md`'s own top-of-document note
before starting.** Its most recent run corrected a prediction that the
delete-and-recreate cycle would show an observable intermediate state — a
deletion timestamp, then a `NotFound` — and found instead that the whole
cycle closed inside a two-second sampling gap. The durable lesson it drew:
**a runbook should predict what will still be true afterwards, not what will
be briefly visible during.** Every acceptance test below is written to that
rule — each one is confirmed by comparing an object's identity (its UID)
before and after, or by a claim's continued existence, never by catching a
transition mid-flight.

## What this measures

`docs/superpowers/specs/2026-08-16-persistent-updates-design.md` gives 5b one
invariant and four things built on it, none of which envtest can settle at
the level that matters:

- **§2's invariant** — at most one ordinal of a persistent group is down at a
  time, whatever the reason — is tabled without a cluster in
  `internal/controller/persistent_test.go`, but a table proves the *function*
  returns the right decision, not that a *world* stays reachable while its
  neighbour updates. §8's acceptance test 1 is where a person watches two
  ordinals cycle and confirms both blocks survive.
- **§4's storage growth** splits into a path `kind`'s own default storage
  class demonstrates for free — the refusal — and a path it cannot
  demonstrate at all, because `local-path` reports
  `allowVolumeExpansion: false`. §8's acceptance tests 3 and 4 are that split,
  and test 4 is a section a driver may skip.
- **§3.7's `motd` fix** closes a gap that shipped since milestone 4c-2:
  `internal/render`'s own tests prove what the renderer writes, never what a
  running proxy actually serves to a connecting client. §8's acceptance test
  5 is the first time anything in this repository puts a client's server list
  against a `motd` a `configOverlay`-free `ProxyGroup` field controls.

None of this is provable by envtest for the same reason 5a's own runbook gave:
there is no provisioner, no kubelet and no CSI driver in envtest, so a claim
there never binds and a pod never boots Paper. The acceptance tests below are
what settle each claim beyond argument.

## 0. Prerequisites

**Read `docs/runbook-milestone-5a-evidence.md` §0 and satisfy it.** Nothing
there is repeated: `x86_64-linux`, rootless Podman, a `TMPDIR` on a real
filesystem, a licensed Minecraft Java Edition client at 26.2 (protocol 776)
and a Microsoft account that owns the game, a person to drive that client, and
network reach from the client's machine to the cluster host's NodePort.

**This run needs a second ordinal's worth of everything 5a's run needed for
one** — one more block to place, one more Paper boot to watch — and,
for §11 only, a second storage class from a driver this repository does not
otherwise depend on. Nothing else changes about the environment.

## 1. Build and load the images

Identical to `docs/runbook-milestone-5a-evidence.md` §1: both Paper and
Velocity, from the working tree.

```bash
cd /path/to/spawnery
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make image-load CONTAINER=podman
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make velocity-image-load CONTAINER=podman
```

Use whatever tags these two commands actually print if a Paper or Velocity
bump has moved them since this was written.

## 2. Create a single-node `kind` cluster, with its default storage class

Identical in shape to `docs/runbook-milestone-5a-evidence.md` §2, with this
run's own cluster name so the two never collide if both happen to exist at
once.

```bash
cat >/tmp/spawnery-5b-kind.yaml <<'EOF'
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
  nix develop -c kind create cluster --name spawnery-5b \
  --config /tmp/spawnery-5b-kind.yaml

nix develop -c kubectl get storageclass
```

**Expect the same single `standard` / `rancher.io/local-path` /
`WaitForFirstConsumer` class 5a's run found**, `(default)` after its name. §10
below depends on this class specifically reporting
`allowVolumeExpansion: false` — if a future `kind` version changes that
default, §10's own expectation needs revisiting before this document is
trusted again.

## 3. Load the images into the cluster and apply the CRDs

Identical to `docs/runbook-milestone-5a-evidence.md` §3, with this run's own
cluster name:

```bash
nix build .#paper-image --out-link "$HOME/.cache/spawnery-tmp/paper-img"
nix build .#velocity-image --out-link "$HOME/.cache/spawnery-tmp/velocity-img"

for img in paper velocity; do
  systemd-run --scope --user --property=Delegate=yes --quiet \
    env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" \
    nix develop -c kind load image-archive \
    "$HOME/.cache/spawnery-tmp/${img}-img" --name spawnery-5b
done

nix develop -c kubectl apply -f config/crd/bases
podman exec spawnery-5b-control-plane crictl images
```

Expect both images in the `crictl images` list, and — worth confirming once,
since this branch adds two fields and one condition to the CRDs — that
`kubectl get crd servers.spawnery.cloud -o
jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.podHash}'`
and the equivalent for `.status.properties.storageResizePending` both return
something rather than nothing.

## 4. Run the operator outside the cluster, and hand-build what its pods dial

Identical to `docs/runbook-milestone-5a-evidence.md` §4.

```bash
nix develop -c kubectl create namespace minecraft
nix develop -c go run ./cmd/spawnery-operator \
  --leader-elect=false --operator-namespace minecraft &

podman run -d --name spawnery-5b-relay --network kind \
  -v /nix/store:/nix/store:ro \
  --entrypoint "$(nix build --no-link --print-out-paths nixpkgs#socat)/bin/socat" \
  ghcr.io/spawnery/paper:26.2-0.2.0 \
  TCP-LISTEN:9443,fork,reuseaddr TCP:host.containers.internal:9443
RELAY_IP=$(podman inspect spawnery-5b-relay \
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

**Leave the operator's log visible for the whole run.**
`docs/known-issues.md`'s "From the milestone 5a evidence run" section records
that the recreate path logs a benign `level=error` line with a stacktrace on
every ordinary recreate — expect one such line at every step below that
recreates an ordinal (§8, §9, and §11 if it reaches a restart), and read past
it rather than mistaking it for the run failing.

## 5. Apply the network: a two-ordinal persistent group, one proxy

The only shape change from `docs/runbook-milestone-5a-evidence.md` §5 is
`replicas: 2` on the `ServerGroup` — everything else this run needs from the
manifest is already there.

```bash
nix develop -c kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft
stringData:
  secret: 5b-evidence-run-forwarding-secret
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
  replicas: 2
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
    motd: "5b evidence run"
EOF

sleep 90
nix develop -c kubectl get network,servergroup,proxygroup,servers,pods,pvc -n minecraft
```

**Expect two `Ready` servers, `survival-0` and `survival-1`, two `1/1
Running` pods of the same names, and two `Bound` claims,
`survival-0-data` and `survival-1-data`** — twice §5's expectation in
`docs/runbook-milestone-5a-evidence.md`, nothing else different. Ninety
seconds is the same margin that document allows for one ordinal plus a
proxy; creation is not serialised (spec §2), so two ordinals starting
together should not need materially longer — check early rather than waiting
out the full budget before looking.

## 6. Confirm the objects, before touching the world

```bash
for ord in 0 1; do
  nix develop -c kubectl get server survival-$ord -n minecraft \
    -o jsonpath="survival-$ord: ordinal={.spec.ordinal} podHash={.spec.podHash}\n"
done
nix develop -c kubectl get pvc -n minecraft -l spawnery.cloud/group=survival \
  -o custom-columns=NAME:.metadata.name,OWNERS:.metadata.ownerReferences
```

**Expect `ordinal=0` and `ordinal=1` respectively, and a non-empty
`podHash` on both** — `spec.podHash` is 5b's own field, stamped at creation
by `podspec.DesiredServerHash`, and a freshly created server is never in the
empty-hash adoption case §3.6 of the design describes, so there is nothing to
adopt here. **Expect both claims' owner references to print `<none>`**, the
same load-bearing property `docs/runbook-milestone-5a-evidence.md` §6 checks.

## 7. Join, and place one block on each ordinal

Point the licensed client at `127.0.0.1:30567` (or the tunnelled port from
§0) and log in. Confirm which pod actually accepted the join the way every
runbook in this repository does:

```bash
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/role=server \
  --prefix=true --tail=-1 --timestamps | grep 'joined the game'
```

**Expect the join to land on `survival-0`.** Velocity's own router
(`agent/velocity/.../Router.kt`) picks the candidate with the fewest players
connected in the fallback group, ties broken by name — both ordinals start
at zero players, and `survival-0` sorts first.

**Place one block on `survival-0`, somewhere memorable, and write down
exactly where** — the same care `docs/runbook-milestone-5a-evidence.md` §7
asks for: a distinctive location (on top of the highest point nearby, or
inside an otherwise-empty carved space) removes any doubt later about whether
it is the same block.

**Switch to `survival-1` with Velocity's own `/server` command.** Nothing in
this repository restricts it — no permissions plugin is deployed, and
`internal/render/velocity.go` sets nothing about command access — so it
should be available with Velocity's own unmodified default. Run bare
`/server` first to list the registered backends and confirm both names
appear before switching:

```
/server
/server survival-1
```

`agent/velocity`'s `ServerDirectory` registers each backend under the exact
name its `Server` object carries (verified against
`internal/agentserver/proxy_envtest_test.go`, whose fixtures assert a
`RegisterServer` message names the backend `lobby-aaaa` for a `Server` object
of that name) — so `survival-1` is precisely what `/server` expects, and both
`survival-0` and `survival-1` should be in the list the bare command prints.
If the command turns out to be unavailable for some reason this repository
does not control, that is worth its own line in the evidence report rather
than a workaround improvised here: nothing in `DecidePersistentSize` offers a
way to make one ordinal temporarily unreachable by the router without
removing it outright, and removing `survival-0` is not this test's problem
to solve. Confirm the switch landed with the same log grep as above, expecting
a second `joined the game` line, this time against `survival-1`'s pod.
**Place a second block on `survival-1`, somewhere equally memorable, and write down
where.** Quit the client normally once both coordinates are recorded.

## 8. Acceptance test 1 — the update, on a two-ordinal group

This is §2's invariant, driven rather than tabled. Record both `Server`
objects' UIDs before touching anything, the same method
`docs/runbook-milestone-5a-evidence.md` §8 settled on after finding the
delete-and-recreate cycle too fast to catch any other way:

```bash
for ord in 0 1; do
  nix develop -c kubectl get server survival-$ord -n minecraft \
    -o jsonpath="survival-$ord before: {.metadata.uid}\n"
done
```

Change `spec.maxPlayers` — a config-only edit that reaches
`podspec.DesiredServerHash` through the config half described in
`docs/superpowers/specs/2026-08-16-persistent-updates-design.md` §3.2, not
through the rendered pod, and therefore marks both ordinals stale without
touching the image or anything else the pod itself carries:

```bash
nix develop -c kubectl patch servergroup survival -n minecraft --type=merge \
  -p '{"spec":{"maxPlayers":24}}'
```

**Expect `survival-1` to go down first, come back `Ready`, and only then
`survival-0` to go down.** The stale class in `DecidePersistentSize`
(`internal/controller/persistent.go`) sorts highest-ordinal-first, the same
order the surplus class uses, and Gate B holds the next nomination until
every required ordinal reads `Ready` again. Poll both objects' UID and phase
together every few seconds — not faster; `docs/runbook-milestone-5a-evidence.md`
§8's own lesson is that a five-second reconcile interval does not mean five
seconds of latency, and chasing a tighter poll buys nothing:

```bash
nix develop -c kubectl get server -n minecraft -l spawnery.cloud/group=survival \
  -o custom-columns=NAME:.metadata.name,UID:.metadata.uid,PHASE:.status.phase -w
```

**The property that matters is the order, not the timing**: `survival-1`'s
UID must have changed and its phase must read `Ready` *before*
`survival-0`'s UID changes at all. If `survival-0`'s UID changes while
`survival-1` is still short of `Ready`, that is the invariant broken, not a
detail to note in passing.

**The 2026-08-16 run, at a four-second poll**, with UIDs truncated to eight
characters:

```
17:00:11  survival-0=befd008b/Ready     survival-1=64e8970f/Starting
17:00:19  survival-0=befd008b/Ready     survival-1=64e8970f/Starting
17:00:23  survival-0=e814963d/Pending   survival-1=64e8970f/Ready
17:00:49  survival-0=e814963d/Ready     survival-1=64e8970f/Ready
```

`survival-1` carried a new UID and was still `Starting` while `survival-0`
sat unchanged at its original one; `survival-0`'s UID moved only in the same
sample that first showed `survival-1` `Ready`. Both ordinals were current
again 48 seconds after the patch.

**The event trail says the same thing independently of the poll**, which is
the better record of the two because it does not depend on when anyone
looked:

```
17:00:01  StaleSpec  ServerGroup/survival  removing server survival-1
17:00:23  StaleSpec  ServerGroup/survival  removing server survival-0
```

Twenty-two seconds apart, and the second lands exactly when `survival-1`
reached `Ready`. That is Gate B, visible on the objects. Note the reason
reads `StaleSpec` rather than the generic `ServerRemoved`, so the trail says
*which* occasion took each ordinal down.

Once both UIDs have changed, confirm the claims did not move:

```bash
for ord in 0 1; do
  nix develop -c kubectl get pvc survival-$ord-data -n minecraft \
    -o jsonpath="survival-$ord-data: {.metadata.uid}\n"
done
```

**Expect both claim UIDs unchanged from §6.** Rejoin — which ordinal the join
lands on does not matter here, since both are freshly recreated and tied on
player count again — and confirm from §7's log grep which pod actually
accepted it. Visit that ordinal's coordinates from §7 first, then use
`/server` the same way §7 did to reach the other one and visit its
coordinates too. **This is the whole of the test: are both blocks still
there?** Record the answer in the driver's own words, the way every manual
session in this repository's runbooks does — there is no `kubectl` output
that substitutes for a person looking at both locations.

## 9. Acceptance test 2 — the scale-down

```bash
nix develop -c kubectl get server survival-1 -n minecraft \
  -o jsonpath='before: {.metadata.uid}{"\n"}'
nix develop -c kubectl patch servergroup survival -n minecraft --type=merge \
  -p '{"spec":{"replicas":1}}'
```

**Expect `survival-1` to leave and `survival-0` to be untouched.**
`DecidePersistentSize`'s surplus class removes the highest ordinal at or
above the new `replicas` — here, only `survival-1` qualifies — while
`survival-0` sits below `replicas` and is invisible to that class entirely.

```bash
nix develop -c kubectl get server survival-0 -n minecraft \
  -o jsonpath='survival-0 after: {.metadata.uid}{"\n"}'
nix develop -c kubectl get server survival-1 -n minecraft
nix develop -c kubectl get pvc survival-1-data -n minecraft
```

**Expect `survival-0`'s UID unchanged, `survival-1` gone (`NotFound`), and
the claim `survival-1-data` still `Bound`** — 5a's own property, that
deleting a `Server` never deletes its claim, holding here through a route
5a's own evidence run never drove: a `spec.replicas` edit reaching
`DecidePersistentSize`'s surplus class, rather than a direct
`kubectl delete server`.

Restore `survival` to the shape §5 applied, so a driver returning to this
document later — or moving on to §10 — starts from a known state rather than
one left short by this test:

```bash
nix develop -c kubectl patch servergroup survival -n minecraft --type=merge \
  -p '{"spec":{"replicas":2}}'
sleep 30
nix develop -c kubectl get server survival-1 -n minecraft
```

**Expect a *new* `survival-1`, `Ready`, mounting the same claim** —
`survival-1-data`'s `AGE` should read older than the new `Server` object's,
the same "the claim is older than the server that uses it" reading 5a's own
run recorded.

The 2026-08-16 run: `survival-1` was gone eight seconds after the patch,
`survival-0`'s UID never moved (`e814963d…` before and after), and
`survival-1-data` stayed `Bound` on the same volume. After restoring
`replicas: 2`, the new `survival-1` was created at `17:04:57Z` and mounted a
claim created at `16:55:59Z` — nine minutes older than the server using it.

Worth naming because 5a's own evidence run could not: this is the *first*
time a claim has been shown to outlive its server through a `spec.replicas`
edit rather than a direct `kubectl delete server`. Same property, a route
nobody had driven.

## 10. Acceptance test 3 — growth, negative path, on the default cluster

`kind`'s `standard` storage class reports `allowVolumeExpansion: false` (§2),
so this path is provable on the same cluster the rest of this run already
has, with no extra setup.

```bash
nix develop -c kubectl patch servergroup survival -n minecraft --type=merge \
  -p '{"spec":{"storage":{"size":"2Gi"}}}'
sleep 15
nix develop -c kubectl get servergroup survival -n minecraft \
  -o jsonpath='{range .status.conditions[?(@.type=="StorageResize")]}{.status} {.reason} {.message}{"\n"}{end}'
```

**Expect `False StorageResizeRefused`, with a message naming the storage
class `standard` — the one `kind`'s `local-path` provisioner backs — as the
first thing to check**, not as the established cause: `growClaim`
(`internal/controller/server_controller.go`) records the API server's own
rejection text verbatim, because an unexpandable class and an unbound,
class-less claim return the identical `Forbidden` shape and the function
cannot tell them apart from the error alone.

The 2026-08-16 run got exactly that, and the message is quoted here in full
because its shape is the point:

```
False StorageResizeRefused
claim survival-0-data: the patch growing it to 2Gi was refused by the API
server: persistentvolumeclaims "survival-0-data" is forbidden: only
dynamically provisioned pvc can be resized and the storageclass that
provisions the pvc must support resize; check storage class "standard"
first, in particular whether it sets allowVolumeExpansion: true
```

Three things in it are deliberate. The API server's own sentence is carried
**verbatim**, because it is the part that actually identifies the cause.
`allowVolumeExpansion` is named as what to **check first**, not as what went
wrong. And the message names `survival-0-data` rather than `survival-1-data`:
both claims were refused, and the lowest ordinal wins the tie so that an
operator watching this condition sees one stable message rather than two
alternating between reconciles.

Both claims stayed at `1Gi` throughout, which is the other half of the
result — the group's spec says `2Gi` and the claims do not, and that
divergence is now *reported* rather than silent.

```bash
nix develop -c kubectl get servergroup survival -n minecraft \
  -o jsonpath='{.status.conditions[?(@.type=="Degraded")].status}{"\n"}'
```

**Expect `False`, or the condition absent entirely.** `StorageResize` is
deliberately its own condition rather than folded into `Degraded` — a
storage class that cannot grow and a group whose servers will not start are
different problems with different remedies, and this test is what confirms
the design decision holds in practice, not only in the code that states it.

**There is no step here to put the size back.** `spec.storage.size` cannot
shrink (the CEL rule at `api/v1alpha1/servergroup_types.go`), so `survival`
stays at `2Gi` for the rest of this document. Nothing later reads
`survival`'s own `spec.storage.size` — §11 below creates its own,
separately-sized `ServerGroup` — so this is a fact to know rather than a
state to restore.

## 11. Acceptance test 4 — growth, positive path (a section a driver may skip)

**NOT DRIVEN on 2026-08-16, deliberately.** The top-of-document note gives the
full reasoning; the short form is that this section would have re-proven what
Task 7's envtest already pins against a real expandable `StorageClass`, while
the one link that is genuinely untested end to end — a driver setting
`FileSystemResizePending`, and the ordinal restarting once and returning
larger — is the link `csi-driver-host-path` is least likely to exercise,
because it expands online. **A future driver reaching this section should
weigh the same question rather than running it out of completeness**: if the
available driver expands online, this section proves the claim grew, which is
already known, and says nothing about the restart. The restart belongs on a
storage class that expands offline.

**This section needs `csi-driver-host-path`, a CSI driver that supports
online volume expansion, deployed into the same cluster.** `kind`'s own
default class cannot demonstrate this half of §4 at all — see §2's note — so
there is no way to write this section without an extra dependency. Follow
`csi-driver-host-path`'s own deployment instructions for its project
(https://github.com/kubernetes-csi/csi-driver-host-path) against this
`kind` cluster; it registers its own `StorageClass` with
`allowVolumeExpansion: true`. If deploying it is not worth the time for a
given run, skip this section and say so in the evidence report — the
negative path in §10 already proves the condition machinery works, which is
most of what this section would add.

Once the driver's `StorageClass` exists — find its name with `kubectl get
storageclass` and confirm `ALLOWVOLUMEEXPANSION: true` — point a *fresh*
persistent group at it, since `storageClassName` is immutable once set:

```bash
nix develop -c kubectl apply -f - <<EOF
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: survival-expandable
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
    storageClassName: <the driver's class name>
  drain:
    timeoutSeconds: 60
EOF
sleep 60
nix develop -c kubectl get pvc survival-expandable-0-data -n minecraft \
  -o jsonpath='{.spec.resources.requests.storage}{"\n"}'
```

**Expect `1Gi`.** Grow it:

```bash
nix develop -c kubectl get server survival-expandable-0 -n minecraft \
  -o jsonpath='before: {.metadata.uid}{"\n"}'
nix develop -c kubectl patch servergroup survival-expandable -n minecraft \
  --type=merge -p '{"spec":{"storage":{"size":"2Gi"}}}'
sleep 15
nix develop -c kubectl get pvc survival-expandable-0-data -n minecraft \
  -o jsonpath='requests: {.spec.resources.requests.storage} conditions: {.status.conditions}{"\n"}'
```

**Expect `requests: 2Gi`** — `growClaim` patched the claim, and this driver's
admission accepted it rather than refusing it the way §10's did. **Whether
the ordinal restarts depends on the driver, and both outcomes are correct
readings of §4, not just one:**

- **If the claim never carries `FileSystemResizePending`**, the driver
  expanded online and `Server.status.storageResizePending` should read
  `false` throughout — confirm `survival-expandable-0`'s UID is unchanged
  from the value recorded above, and the volume simply grew under a running
  Paper process.
- **If the claim does carry `FileSystemResizePending` (`True`)**, expect
  `Server.status.storageResizePending` to read `true`, then a single restart
  of `survival-expandable-0` — its UID should change exactly once, the same
  UID-comparison method §8 and §9 use rather than trying to catch the
  transition mid-flight — and the pod should come back `Ready` with the
  larger volume already visible to Paper.

Either way, confirm `status.consecutiveFailures` on `survival-expandable`
stayed at `0` throughout. A resize-triggered restart is a takedown
`DecidePersistentSize` ordered, not a failure: `CountFailures` counts servers
in `Failed`, and a server this rule deletes leaves through `Draining`.

Clean up the extra group before moving on:

```bash
nix develop -c kubectl delete servergroup survival-expandable -n minecraft
nix develop -c kubectl get pvc survival-expandable-0-data -n minecraft
```

**Expect the claim still there after the group is gone** — no owner
reference on it either, the same property §6 checked for `survival`'s own
claims.

## 12. Acceptance test 5 — the `motd` fix

**Widening `DesiredProxyHash` to cover the rendered config values — the fix
itself — rolls every `ProxyGroup` in the installation once, the very first
time this operator's new code reconciles one, regardless of whether anyone
changed a `motd`.** Every proxy pod already running carries a `LabelPodHash`
computed under the old, pod-only hash; the new hash reads differently the
moment the digest widens, and there is no way to tell "the hash widened" from
"the image really changed" from the value alone — see
`docs/known-issues.md`'s "From milestone 5b" section. **This run's operator
has been running since §4, after this branch's code was already built, so
that one-time roll already happened before §5's `ProxyGroup` was even
created — nothing to wait out here.** A run driven by *upgrading* a
long-running operator in place, rather than starting fresh the way this
document does, has to let that first roll settle before this section can
mean anything: measuring the `motd` fix against a proxy that is rolling for
an unrelated reason proves nothing either way.

```bash
nix develop -c kubectl get pods -n minecraft -l spawnery.cloud/role=proxy \
  -o jsonpath='{.items[0].metadata.name} {.items[0].metadata.labels.spawnery\.cloud/pod-hash}{"\n"}'
nix develop -c kubectl patch proxygroup gateway -n minecraft --type=merge \
  -p '{"spec":{"config":{"motd":"5b evidence run, motd changed"}}}'
sleep 30
nix develop -c kubectl get pods -n minecraft -l spawnery.cloud/role=proxy \
  -o jsonpath='{.items[0].metadata.name} {.items[0].metadata.labels.spawnery\.cloud/pod-hash}{"\n"}'
```

**Expect a new proxy pod name and a changed `pod-hash` label** — the ordinary
surge-1 rollout `4c-2` built, driven this time by a config-only edit rather
than an image change.

Confirm the new `motd` actually reaches a connecting client, which is the
whole of what 5a's own defect chain (design §3.7) established was missing
before this milestone. `cmd/spawnery-join` performs a full login and carries
no server-list-ping mode, so this step needs the real client rather than a
scripted one: add `127.0.0.1:30567` (or the tunnelled address from §0) as a
server in the Minecraft client's multiplayer list — a server list ping,
rather than a direct connect, is what shows the motd — and read the line
under the server's name.

**Expect the new motd, "5b evidence run, motd changed", under the server's
name** — not the old one, and not empty. This step is the one that matters
more than the label check above it: a changed `pod-hash` label proves the pod
rolled, not that a player would ever see the new text.

The 2026-08-16 run: the proxy pod went from `gateway-t6tz` /
`b9ad1129925688d7` to `gateway-28h2` / `5dc256312b144cab` within 35 seconds
of the patch, and the driver confirmed the new motd in the client's server
list. A config-only edit rolled a proxy, which before this milestone it never
did — the ConfigMap updated, no proxy went stale, `DecideRollout` ordered
nothing, and the text reached nobody until an unrelated restart.

## 13. Clean up

```bash
pkill -x spawnery-operat
sleep 5
ps -eo pid,comm | grep spawnery   # expect no output
podman rm -f spawnery-5b-relay
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind delete cluster --name spawnery-5b
rm -f /tmp/spawnery-5b-kind.yaml
```

See `docs/runbook-milestone-5a-evidence.md` §10 for why `pkill -x` and the
`sleep 5` are what they are; that reasoning applies unchanged.
Deleting the cluster deletes every claim this run created, including
`survival-expandable-0-data` if §11 was run and its group deleted first —
there is nothing left to reap by hand.

## Where this goes

The coordinates and driver's answer from §8, the UID sequences from §8
through §11, the exact `StorageResize` message from §10, and the motd
observed in §12 belong in `docs/handover-milestone-5.md`, in the same shape
`docs/handover-milestone-4.md` and `docs/handover-milestone-5.md`'s own "The
evidence run" section record prior runs: what was measured, what (if
anything) this document's own steps needed correcting in place, and — for
any test that did not hold — a defect report against this milestone's
central claims, not a note that the runbook needs fixing.
