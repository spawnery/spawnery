# Runbook: milestone 4c-1's two cluster claims

Status: written 2026-08-14, at the end of milestone 4c-1, against branch
`milestone-4c1-readiness-contract`. Not yet run. Every command below was
checked against the repository it names — the Makefile targets, the CRD field
names, the label keys, the constants in `internal/podspec` — but the cluster
behaviour it predicts has not been observed. The first person to run this is
also its first corrector; do what
`docs/runbook-milestone-3-evidence.md` did after its first real run and fix
defects in place rather than noting them as a fork.

Design `docs/superpowers/specs/2026-08-14-proxy-readiness-contract-design.md`
§10 lists eight acceptance criteria. Five of them — criteria 3, 4, 5, 6 and 7
— are met by `go test`, `make agent-test`, `make image-test`,
`make image-repro` and `make manifests`. A sixth, criterion 8, is met by
running this document itself, as the paragraph below states. The remaining
two are not met by anything else, and this document exists for exactly
those two:

- **Criterion 2** — the removed proxy leaves the `Service`'s endpoints
  *before* it is deleted.
- **Criterion 1** — scaling a `ProxyGroup` from 2 to 1 with a player connected
  to the removed proxy does not disconnect them; the pod is deleted only once
  empty.

envtest has no kubelet, no readiness probes and no kube-proxy. It can prove
the operator asks for the right thing; it cannot prove that asking has the
effect the whole milestone is named after. Criterion 8 says so in as many
words: criteria 1 and 2 are measured against a real cluster or they are not
measured.

There is a third thing this runbook covers, and it is the one worth planning a
session around. §10 runs the **drain deadline** on purpose. It is the only
path in this milestone that disconnects anybody, and it should be seen once,
deliberately, by someone expecting it — not for the first time in production,
by someone who is not.

Each step says what to expect. A deviation from the expected output is a
defect somewhere in this milestone, not a documentation problem — read
`docs/known-issues.md` before assuming the runbook itself is wrong.

## What this runbook gets right, and why each one matters

Six facts, read out of the code rather than assumed. Each of them is a place a
reader could otherwise spend an hour chasing something that is working
correctly.

1. **The proxy that gets removed is the newest one.**
   `ProxyGroupReconciler.pods` sorts live pods by `creationTimestamp`, oldest
   first, tie-broken by name; `reconcileReplicas` deletes from index
   `spec.replicas` upward — the end of that list. So at `replicas: 2 → 1` the
   pod that goes is the one with the *later* `creationTimestamp`. This is the
   whole reason §8 exists: the client is routed by kube-proxy, which does not
   care which pod is doomed, so landing on the right one is chance unless you
   force it.

2. **Nothing logs when a proxy is told to stop being ready.** `ReadyGate.close()`
   releases the socket and returns; it logs only a *failed* bind or a failed
   accept. `Fleet.SetReady` sends the message and returns. The operator emits
   exactly one Kubernetes event in this whole milestone, and only from the
   deadline in §10. Silence at §9's scale-down is the designed behaviour, the
   same documented silence `agent/velocity/.../Drain.kt` keeps on success. The
   evidence is the pod's `Ready` condition and the `EndpointSlice`, and there
   is no log line to go looking for.

3. **`status.connectedPlayers` counts the player on the draining proxy, and
   it is the number to watch during the drain.** `setStatus` adds every live
   pod's last reported count outside the readiness guard, deliberately: a
   draining proxy is `NotReady` on purpose and still has on it the people this
   milestone exists to protect. So after the scale-down in §9 expect `kubectl
   get proxygroup` to show `READY 1`, `PLAYERS 1`, `PHASE Ready` — one ready
   proxy, and the player on the one that is leaving still counted. With
   nothing logging a readiness withdrawal (fact 2), this is the number that
   follows the player through the drain. It falls to `0` when the last player
   leaves or when the pod is deleted, whichever comes first: `pods()` drops a
   pod the moment it carries a deletion timestamp, so the count goes with it.
   Until 2026-08-14 this field skipped non-`Ready` pods and read `0` for the
   whole drain, which a real run showed with a person visibly in the game. So
   `PLAYERS 0` with somebody on a draining proxy now means something is off,
   and there are two likely somethings: an operator built before that fix, or
   an agent that has never reported. The sum is built from each agent's last
   word (fact 4), and a proxy whose agent never connected serves its players
   while contributing `0` to it.

4. **The count in §10's `Warning` event is the last known count, not a
   measurement.** It comes from `r.Agents.Lookup(pod.UID).Players` — whatever
   the pod's agent last reported over its gRPC stream. In the ordinary case
   the stream is alive (readiness has nothing to do with it) and the number is
   right. But a proxy whose agent stream broke with seven players on it is
   announced with whatever it last reported, and a proxy whose agent never
   connected at all is announced as `0 player(s)`. The event is authoritative
   about *which* pod was deleted and *that* sessions were lost; the number is
   a floor. §10 repeats this where you will be reading the event.

5. **A stale player count reads as occupied, which is why an agentless proxy
   waits out its full deadline.** `Registry.Lookup` marks a count stale after
   twice the report interval (`--report-interval`, 5s by default, so ~10s),
   and `reconcileReplicas` deletes only on `players == 0 && !PlayersStale`. A
   surplus proxy whose agent is gone is therefore never "known empty" and
   leaves by the deadline rather than immediately. That is deliberate — a
   dropped gRPC stream does not disconnect anybody, so deleting on a bare zero
   would kill exactly the sessions the wait exists to protect.

6. **The drain deadline is compared against the *current* spec on every pass,
   but the annotation is stamped once.** `markDraining` writes
   `spawnery.cloud/draining-since` the first time a pod goes surplus and never
   moves it; `expired` recomputes `now - since >= group.DrainTimeout()` each
   reconcile. Lowering `spec.drain.timeoutSeconds` on a drain already in
   flight therefore expires it on the next pass. §10 uses this deliberately as
   a shortcut; know that it is why, so you do not do it by accident in §9.

## 0. Prerequisites

**Read `docs/runbook-milestone-3-evidence.md` §0 and satisfy it.** It is
correct and it is not repeated here: `x86_64-linux`, rootless Podman with
`docker` aliased to it *or* `CONTAINER=podman` passed explicitly, a `TMPDIR`
on a real filesystem where `/tmp` is a tmpfs, `XDG_RUNTIME_DIR` and
`DBUS_SESSION_BUS_ADDRESS` exported before the first `systemd-run --scope
--user`, a clone of this repository, and `nix develop`.

Two of those are conditional and were measured on this repository's own
machine on 2026-08-14:

- `/run/current-system/sw/bin/docker` is a symlink into
  `podman-docker-compat`, so the Makefile's `CONTAINER ?= docker` default
  already runs Podman here and `CONTAINER=podman` changes nothing.
- `/tmp` is part of the root filesystem, not a tmpfs, so the `TMPDIR`
  override is unnecessary here.

`make agent-test`, `make image-test` and `make image-repro` all pass on that
machine with plain defaults. **Check your own machine rather than copying
either answer** — `readlink -f "$(command -v docker)"` and `findmnt -no FSTYPE
/tmp` settle both in two commands. The overrides below are written out in full
because a different machine needs them; drop them if yours does not. Nothing
in this runbook depends on which branch you are on.

Beyond §0, this runbook needs what milestone 3's manual sections needed:

- **A licensed Minecraft Java Edition client at 26.2, protocol 776**, and a
  Microsoft account that owns the game. Paper 26.2 refuses any other protocol
  with a loud "Outdated client!" naming the version to install. This is not a
  number to approximate.
- **A person to drive that client**, in the game, for the length of §9 and
  §10. Both measurements turn on what the client did — whether the session
  survived, and whether it was cut. Only the person at the keyboard can attest
  to that; the logs say what the proxy did, not what the game showed.
- **Network reach from the client's machine to the cluster host's NodePorts
  30567 and 30568.** If the client is on another machine, milestone 3's
  runbook §10 gives the SSH tunnel that needs nothing changed on the host:
  `ssh -N -L 30567:127.0.0.1:30567 -L 30568:127.0.0.1:30568 <user>@<host>`.

## 1. Build and load both images

Identical to milestone 3's §1 and repeated here only because the tags matter
below. Drop the two overrides if §0 established you do not need them.

```bash
cd /path/to/spawnery
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make image-load CONTAINER=podman
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make velocity-image-load CONTAINER=podman
```

Expect two image loads reporting `ghcr.io/spawnery/paper:26.2-0.2.0` and
`ghcr.io/spawnery/velocity:3.5.1-0.2.0` — the same two tags
`config/samples/network.yaml` names. If a Paper or Velocity bump has moved
them, use the tags these two commands actually print everywhere below.

**The Velocity jar is on this milestone's critical path.** 4c-1 adds the
`SET_READY` branch to `ProxyRole` and the `close()` call it reaches, so an
image built before that branch existed will do everything in this runbook
except stop being ready — the pod stays green through the whole drain and
§9 fails at its first measurement, with nothing anywhere saying why. Build
from the working tree, not from a cached tag you happen to have.

## 2. Create the kind cluster, with two NodePorts published

`kind`, not `k3d`, for the reason `docs/known-issues.md` gives under "From
milestone 2b". The `systemd-run` wrapper is required for the cgroup
delegation kind's own check insists on.

Two ports, because §8 needs a second, hand-built `Service` to pin a client to
one specific proxy pod: **30567** for the `ProxyGroup`'s own Service and
**30568** for that pin. Both are outside kind's default mapping and outside
milestone 3's 30565/30566, so this cluster can coexist with that one.

```bash
cat >/tmp/spawnery-4c1-kind.yaml <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30567
        hostPort: 30567
      - containerPort: 30568
        hostPort: 30568
EOF

# Needs XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS — see milestone 3's §0.
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind create cluster --name spawnery-4c1 \
  --config /tmp/spawnery-4c1-kind.yaml
```

Expect a single-node cluster. Measured 2026-08-14 in this repository's
devshell: `kind v0.32.0`, whose default node image is
`kindest/node:v1.36.1`, with `kubectl` at `v1.36.3`. §6's choice of endpoint
API is decided by that 1.36 and says so.

**One node is not a limitation here, it is a requirement.** The `Service` the
operator builds carries `externalTrafficPolicy: Local`, so a client reaching a
node that runs no proxy pod for the group gets no answer at all. With both
proxies on the one node, the NodePort answers for both, and §8's pin works for
the same reason.

## 3. Load the images into the cluster and apply the CRDs

`kind load docker-image` does not work under Podman regardless of
`KIND_EXPERIMENTAL_PROVIDER` — it shells out to `docker` unconditionally and
fails blaming the image rather than the tool. Milestone 3's §3 has the full
diagnosis. Use `kind load image-archive`, which `nix build` already produces
the right shape for:

```bash
nix build .#paper-image --out-link "$HOME/.cache/spawnery-tmp/paper-img"
nix build .#velocity-image --out-link "$HOME/.cache/spawnery-tmp/velocity-img"

for img in paper velocity; do
  systemd-run --scope --user --property=Delegate=yes --quiet \
    env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" \
    nix develop -c kind load image-archive \
    "$HOME/.cache/spawnery-tmp/${img}-img" --name spawnery-4c1
done

nix develop -c kubectl apply -f config/crd/bases
podman exec spawnery-4c1-control-plane crictl images
```

Expect both `ghcr.io/spawnery/paper:26.2-0.2.0` and
`ghcr.io/spawnery/velocity:3.5.1-0.2.0` in the `crictl images` list; the load
itself prints nothing that names them.

## 4. Run the operator outside the cluster, and hand-build what its pods dial

Unchanged from milestone 3's §4, including the relay and its reason: the
operator has no image of its own, a proxy pod dials
`spawnery-operator.minecraft.svc:9443`, and nothing creates that `Service`
because there is no operator pod for a selector to find. See
`docs/known-issues.md`, "From milestone 2c".

```bash
nix develop -c kubectl create namespace minecraft
nix develop -c go run ./cmd/spawnery-operator \
  --leader-elect=false --operator-namespace minecraft &

podman run -d --name spawnery-4c1-relay --network kind \
  -v /nix/store:/nix/store:ro \
  --entrypoint "$(nix build --no-link --print-out-paths nixpkgs#socat)/bin/socat" \
  ghcr.io/spawnery/paper:26.2-0.2.0 \
  TCP-LISTEN:9443,fork,reuseaddr TCP:host.containers.internal:9443
RELAY_IP=$(podman inspect spawnery-4c1-relay \
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

Expect `kubectl apply` to print a deprecation warning on the `Endpoints`
object. It is expected on 1.36 and it is not the deprecation §6 is about —
this one is a hand-built relay for the operator's own gRPC port and has
nothing to do with the proxy `Service` whose endpoints this runbook measures.
Milestone 3's §4 records the same warning and that the relay still worked.

With a real Docker daemon rather than rootless Podman, skip the relay and
point the `Endpoints` at `172.17.0.1` directly.

**Leave the operator's log visible.** Nothing in this milestone logs the
readiness assertion (see fact 2 above), but a reconcile that is erroring —
against the CRDs, the Service, or the API server — says so there and nowhere
else, and a silently stopped operator looks exactly like a drain that is
patiently waiting.

## 5. Apply the network: one backend group, one proxy group at `replicas: 2`

Smaller than milestone 3's §5 on purpose. This milestone measures the proxy
layer, so one `ServerGroup` at one replica is all the backend a player needs
to be *somewhere*, and the second `ProxyGroup` milestone 3 carried for
`online-mode` has no job here.

**Both proxies get their own smaller `spec.resources`.** Milestone 3 measured
the fit problem: a backend inheriting `Network.defaults.resources` at 2Gi plus
proxies at the same size does not fit an 8Gi node, and the extra pod sits in
`Pending` with `0/1 nodes are available: 1 Insufficient memory`. This
manifest asks for 2Gi + 1Gi + 1Gi = 4Gi and fits comfortably. Velocity does
not need a backend's heap.

**`config.onlineMode` is deliberately left unset**, so the CRD's
`+kubebuilder:default=true` applies and the proxy demands a real Mojang
session. That is what a licensed client needs to log in with, and §0 already
requires one. Never try to reach `online-mode` through a `configOverlay`:
`internal/render.Velocity` reasserts the keys it owns after merging any
overlay, so an overlay could not touch it however it were phrased.

**`spec.drain.timeoutSeconds` is set explicitly to 900** rather than left at
its default of 300. Nothing about criterion 1 depends on the number; what
depends on it is whether you are under a five-minute clock while a person
walks around a Minecraft world checking things. The default is covered by
envtest and by the CRD's own `+kubebuilder:default={timeoutSeconds:300}`; this
run does not need to re-prove it. §10 lowers it on purpose.

```bash
nix develop -c kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft
stringData:
  secret: 4c1-evidence-run-forwarding-secret
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
  name: lobby
  namespace: minecraft
spec:
  networkRef:
    name: evidence
  type: Ephemeral
  image: ghcr.io/spawnery/paper:26.2-0.2.0
  maxPlayers: 100
  drain:
    timeoutSeconds: 60
  scaling:
    minReplicas: 1
    maxReplicas: 10
    spareSlots: 40
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft
spec:
  networkRef:
    name: evidence
  replicas: 2
  image: ghcr.io/spawnery/velocity:3.5.1-0.2.0
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      memory: 1Gi
  drain:
    timeoutSeconds: 900
  expose:
    type: NodePort
    nodePort:
      port: 30567
  routing:
    fallbackGroups:
      - lobby
  config:
    playerLimit: 100
    motd: "4c-1 readiness contract"
EOF
```

Wait for everything to reach `Ready`. Milestone 3's runbook allows 90 seconds
and its second run saw 21; allow the 90 and check early.

```bash
sleep 90
nix develop -c kubectl get network,servergroup,proxygroup,servers,pods -n minecraft
```

Expect, by shape rather than by exact name — pod names carry a random suffix:

```
NAME                                PHASE   READY   ADDRESS         PLAYERS
proxygroup.spawnery.cloud/gateway   Ready   2       <nodeIP>:30567  0
```

plus one `Ready` `lobby` server, and three `1/1 Running` pods: one `lobby-*`
and two `gateway-*`.

**`READY 2` is the precondition for everything below.** A `ProxyGroup` stuck
at `READY 1` with both pods `Running` means one ready gate did not bind —
`kubectl logs` on that pod is the only place that says so, the CR will not.
`docs/known-issues.md`'s "From milestone 3c" entry on a silent bind failure
applies unchanged.

Note that the operator **does not re-spec an existing pod**. If a proxy pod is
already `Pending` on insufficient memory when you patch `spec.resources`, the
patch alone does not fix it — `kubectl delete pod` on it lets the operator
recreate it with the new spec. Rolling updates are 4c-2's, not this
milestone's.

## 6. How to read readiness and endpoints — and why `endpointslices`

Two commands do all the observation in §9 and §10. Get comfortable with both
before a client is connected and the clock matters.

**Use `kubectl get endpointslices`, not `kubectl get endpoints`.** Three
reasons, in order of how much they matter:

1. **A not-ready endpoint does not vanish from the slice — its `ready`
   condition goes `false`, and the address stays until the pod is deleted.**
   Those are two different events, seconds to minutes apart, and telling them
   apart *is* criterion 2: "leaves the endpoints **before** it is deleted"
   only means something if you can see the before. The `EndpointSlice` names
   the condition. `kubectl get endpoints`'s summary column collapses the two
   into an address that is simply no longer printed, and a reader watching
   only that cannot say which of the two just happened.
2. **`EndpointSlice` is what kube-proxy actually consumes**, so it is the
   object whose change is the cause of new connections stopping, rather than a
   compatibility view of it.
3. `v1 Endpoints` is deprecated as of Kubernetes 1.36, which is the version §2
   pins. It still answers, and milestone 3's runbook §4 records measuring
   exactly that — but this runbook should not add a new dependency on it.

```bash
# One line per proxy: pod name, IP, and whether kube-proxy will route to it.
nix develop -c kubectl get endpointslice -n minecraft \
  -l kubernetes.io/service-name=gateway -o json | nix develop -c jq -r \
  '.items[].endpoints[] | [.targetRef.name, .addresses[0], "ready=\(.conditions.ready)", "serving=\(.conditions.serving)"] | @tsv'
```

Expect two lines, both `ready=true serving=true`, before anything is scaled.

The `-l kubernetes.io/service-name=gateway` is not optional decoration: the
`minecraft` namespace also holds a mirrored slice for §4's hand-built
`spawnery-operator` `Endpoints`, and an unfiltered list mixes the relay in
with the proxies.

The second command is the pods themselves — creation order, readiness, and
the drain annotation, which is what §9 and §10 both turn on:

```bash
nix develop -c kubectl get pods -n minecraft \
  -l spawnery.cloud/role=proxy,spawnery.cloud/group=gateway \
  --sort-by=.metadata.creationTimestamp -o json | nix develop -c jq -r \
  '.items[] | [.metadata.name, .metadata.creationTimestamp,
    ([.status.conditions[] | select(.type=="Ready") | .status][0] // "?"),
    (.metadata.annotations["spawnery.cloud/draining-since"] // "-")] | @tsv'
```

Expect two lines in creation order, both `True`, both with `-` for the
annotation. **The second line is the pod that a scale-down to 1 will remove**
— fact 1 above. Write its name down; §8 needs it.

`spawnery.cloud/draining-since` is the exact annotation the operator stamps
(`internal/controller/proxygroup_controller.go`), an RFC 3339 timestamp
written once when the pod first goes surplus and removed again if the
scale-down is cancelled.

## 7. Join with a real client, and find out which proxy you are on

Point the licensed client at `127.0.0.1:30567` if it runs on the cluster host,
or at the tunnelled port from §0 if it does not. Log in with the Microsoft
account. Expect to land in the `lobby` world.

There is **no per-pod player count on any object** — `status.connectedPlayers`
is the group's total. The proxy's own log is the only place that names which
pod accepted a given player, and only the accepting pod logs the line at all:

```bash
nix develop -c kubectl logs -n minecraft \
  -l spawnery.cloud/role=proxy,spawnery.cloud/group=gateway \
  --prefix=true --tail=-1 --timestamps | grep 'connected player.*has connected'
```

`--tail` defaults to 10 lines per pod, not to "all", the moment a selector
(`-l`) is in play — `kubectl logs --help` says so in as many words — and by
the time this command is repeated in §8 and §10, minutes and several log
lines later, the line this command is looking for is very likely past that
window. `--tail=-1` turns that default back off. The pattern keeps both the
marker and the verb, `connected player.*has connected`, and each is doing a
different job. The verb excludes a disconnect on its own terms, without
needing to know how Velocity actually spells one: `has connected` cannot
occur inside `has disconnected`, because the word after `has ` differs —
true whatever the disconnect line looks like. The marker is needed for a
different reason: on the very join this command is looking for, the
accepting pod's own log carries a *second* line that also contains `has
connected` — `[server connection] <player> -> <backend> has connected`,
logged immediately after the `[connected player]` line
(`docs/handover-milestone-4.md:333-334`, also `:217` and `:358`). A
verb-only grep matches both lines from the one join and turns one accepted
player into two matching lines; keeping the `connected player` marker
excludes the second. (This repository's own record of the marker,
`docs/handover-milestone-4.md:333`, shows only the connect spelling under
`[connected player]`; it does not record what a disconnect on that marker
looks like, so the verb's exclusion above rests on the words themselves, not
on a disconnect line anyone has observed here.)

Expect one line, prefixed with the pod it came from and timestamped by
`kubectl` itself ahead of Velocity's own embedded time:

```
[pod/gateway-xxxx/velocity] 2026-08-14T15:04:49.123456789Z [15:04:49 INFO]: [connected player] <player> (/10.244.0.1:50113) has connected
```

**Once a second join has happened — the coin-flip fallback below, or §8 and
§10's rejoin — take the most recent line by the `kubectl`-added timestamp,
not by its position in the output.** The surviving pod keeps its own earlier
`has connected` line in its history, so the selector-wide grep then returns
one line per pod that has ever accepted a player, not one line total; and
`kubectl logs -l` does not interleave pods in time order, it prints one
pod's matches and then the next's, so where a line sits in the output says
nothing about when it happened.

Compare that pod name with the second line from §6's pod command — the one a
scale-down will delete.

**If they are the same pod, you are ready for §9; skip §8.** If they are not,
the run as it stands would measure the wrong thing, and it would *look like a
pass*: the client keeps playing exactly as it should, because the pod it is on
is not going anywhere. A run that measures the surviving proxy and reports
success is worse than a run that fails. Do not proceed on the assumption that
it probably worked.

The cheap fix is to quit and rejoin: two ready endpoints on one node means
roughly a coin flip each time. The reliable fix is §8.

## 8. Forcing the client onto the proxy that will be removed

Deterministic, and it keeps kube-proxy in the path — which matters, because
"an established connection survives" is a claim about kube-proxy's handling of
an established TCP session, and a `kubectl port-forward` would prove it about
the API server's tunnel instead.

Proxy pods carry no per-pod label (`podspec.ProxyLabels` is network, group,
role and managed-by, and deliberately no per-pod name), so add one by hand.
The operator does not re-spec existing pods, so a label you add stays.

```bash
DOOMED=$(nix develop -c kubectl get pods -n minecraft \
  -l spawnery.cloud/role=proxy,spawnery.cloud/group=gateway \
  --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{.items[-1:].metadata.name}')
echo "will be removed: $DOOMED"

nix develop -c kubectl label pod -n minecraft "$DOOMED" evidence.local/pin=doomed

nix develop -c kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: gateway-pin
  namespace: minecraft
spec:
  type: NodePort
  selector:
    evidence.local/pin: doomed
  ports:
    - name: minecraft
      port: 25565
      targetPort: minecraft
      nodePort: 30568
      protocol: TCP
EOF
```

`targetPort: minecraft` is the container port's own name
(`podspec.MinecraftPortName`), so this does not hardcode 25565 twice. The
`Service` is left at the default `externalTrafficPolicy: Cluster` — on a
single-node cluster it makes no difference, and `Local` would only add a
second way for this to answer nothing.

Now quit the client and rejoin against **`127.0.0.1:30568`**. Repeat §7's log
command; it now returns one line per pod that has ever accepted a player, so
take the most recent by timestamp, per §7's rule. Expect that line to name
`$DOOMED`, every time.

**Two honest limits on this pin.** It is a second route to the same pod, not a
different kind of connection — but not an identical one either.
`gateway-pin` is left at the default `externalTrafficPolicy: Cluster`, so
kube-proxy SNATs through it, where the group's own `Service` runs `Local` and
does not. That is the one respect in which the two paths differ: criterion 1
ends up measured through this hand-built `Service`, not the operator's own.
The mechanism under test — conntrack keeping an established NodePort DNAT
session alive across an endpoint going not-ready — is identical either way,
so the method is sound and §9's survival measurement is made on the real
path. And when the draining proxy's readiness drops, `gateway-pin`'s endpoint
goes not-ready too — readiness is a property of the pod, not of the
`Service` — so no new connection will arrive through it either. That is
fine; §9 needs the session you already have, not a new one.

Leave `gateway-pin` in place for §10, which repeats the exercise.

### Optional: a dry run without spending the account's session

`cmd/spawnery-join --hold` is fit for this milestone's purposes in a way it was
*not* fit for milestone 3's criterion 9. That failure was about the backend's
count: a held join stops one packet after `Login Acknowledged`, before Paper
counts it as an online player. This milestone's wait reads the **proxy's**
count, and a held connection is in Velocity's `connectionsByUuid` from login
success onwards — milestone 3's runbook item 4 disassembles the jar to
establish it. So a held probe genuinely occupies a proxy.

```bash
nix develop -c go build -o /tmp/spawnery-join ./cmd/spawnery-join
/tmp/spawnery-join --host 127.0.0.1 --port 30568 --username drain_probe \
  --hold 120s --timeout 150s &
```

`--hold` must fit inside `--timeout` or the tool refuses with both flags
named. The username is `drain_probe` with an underscore: a hyphen is not a
legal Minecraft username, and Velocity forwards it anyway before Paper kills
it with a message that names neither the field nor the character.

Run §9's steps against that hold and you will exercise the operator half —
the annotation, the readiness drop, the endpoint condition, the pod surviving
while occupied, and the deletion once the hold expires. What it cannot show is
criterion 1, because there is no game to look at and nobody to say whether the
session was interrupted. Use it to shake the environment out before the real
client joins; do not record it as the proof.

## 9. The measurement: scale to 1 with the player on the doomed proxy

With the client connected and confirmed to be on `$DOOMED`, in a second
shell:

```bash
nix develop -c kubectl patch proxygroup gateway -n minecraft --type=merge \
  -p '{"spec":{"replicas":1}}'
```

Then watch, in roughly this order. Nothing here needs to be caught in a
particular second, but the annotation and the endpoint condition both appear
within a reconcile or two (`resyncInterval` is 5 seconds), so start looking
straight away.

**Expectation 1 — the annotation appears, on the doomed pod only.** Re-run
§6's pod command. Expect `$DOOMED`'s last column to become an RFC 3339
timestamp within about 5 seconds, and the surviving pod's to stay `-`.

**Expectation 2 — the doomed pod goes `NotReady`, and neither log says
anything.** Its `Ready` column flips from `True` to `False` roughly 10 to 15
seconds after the annotation: the ready gate closes on the operator's message,
and the kubelet's `tcpSocket` probe on port 8081 needs three consecutive
failures at a 5-second period to declare it. Where in that period the gate
closed decides whether it is nearer 10 or nearer 15. Expect no line in the
proxy's log and none in the operator's — fact 2.

**Expectation 3, and this is criterion 2 — the endpoint stops being ready
while the pod is still there.** Re-run §6's endpointslice command:

```
gateway-xxxx  10.244.0.7  ready=true   serving=true
gateway-yyyy  10.244.0.8  ready=false  serving=false
```

with `gateway-yyyy` being `$DOOMED`. The address is still listed. That is
correct and it is the point: kube-proxy will send no new connection to a
`ready=false` endpoint, and criterion 2 asks for exactly that separation
between "out of rotation" and "gone". A pod still listed as `ready=true`
fifteen seconds after the annotation is the failure — either the agent never
received `SetReady`, or the jar predates the branch that acts on it (§1).

**Expectation 4, and this is criterion 1 — the player keeps playing.** Ask
the person at the client. Expect an entirely uninterrupted session: no
disconnect screen, no rubber-banding, no lost chunks. The proxy has been taken
out of rotation, not shut down, and the TCP session terminating on it was
never touched.

Only the person driving can attest to this. Record what they say, in their
words — milestone 3's manual session did the same and it is the half of the
record the logs cannot supply.

**Expectation 5 — the group's own status goes on counting your player.**
Expect `kubectl get proxygroup gateway -n minecraft` to read `READY 1`,
`PLAYERS 1`, `PHASE Ready`: one ready proxy, and the person on the draining
one still in the sum. Fact 3 is why. `PLAYERS 0` with somebody in the game is
worth stopping for — it is what this field did before 2026-08-14, so the first
thing to check is that the operator you are running is built from this branch
(§4). The count dropping is not an anomaly to excuse here; it is the drain
finishing, and expectation 7 is where it should happen.

**Expectation 6 — the pod is not deleted.** Confirm it, more than once, over
whatever length of session you want to give it. `kubectl get pods -n minecraft
-l spawnery.cloud/role=proxy` keeps showing two pods, one of them `0/1
Running`. You have 900 seconds from the annotation before §5's drain timeout
fires; past that the deadline in §10 takes over and the measurement becomes
§10's rather than this one's. If you want longer, raise
`spec.drain.timeoutSeconds` — it is compared against the current spec on every
pass (fact 6), so raising it mid-drain works.

**Expectation 7 — the pod is deleted, promptly, once the player leaves.** Have
the client quit the game normally. Expect `$DOOMED` to disappear within
roughly 10 to 15 seconds: the agent reports the new count on its 5-second
report interval, the next reconcile reads `players == 0 && !PlayersStale`, and
deletes.

**Start following the doomed proxy's log before the client quits, and keep
what it prints.** The proxy's own log is where the player's departure is
recorded, and it goes with the pod: 10 to 15 seconds after the quit there is
no pod left to read it from, and nothing is also what a proxy that logged
nothing looks like. So `kubectl logs -f -n minecraft "$DOOMED"` in its own
shell, started while the pod is still there. This is the one thing the run on
2026-08-14 got wrong: the departure was polled for after the deletion, found
absent, and reported as the proxy having said nothing — an absence of
observation written down as an absence of the thing. If you want to evidence
"deleted only after the player left", this is the capture that evidences it.

Then:

```bash
nix develop -c kubectl get pods -n minecraft -l spawnery.cloud/role=proxy
nix develop -c kubectl get events -n minecraft \
  --field-selector involvedObject.kind=ProxyGroup,involvedObject.name=gateway
```

Expect one proxy pod left, and **no `ProxyDrainTimeout` event**. Its absence
is part of the proof: the pod left because it was empty, not because a clock
ran out. An event here means the deadline fired and criterion 1 was not met by
this run.

Expectation 3 and expectations 4/6/7 together are criteria 2 and 1. Record all
of it — the endpointslice output at each stage, the annotation timestamp, the
pod list before and after, the absence of the event, and the player's own
account of the session.

### If you want criterion 4 as well, for free

Criterion 3 (re-assertion after a reconnect, covered by `internal/proxyreg`'s
unit tests) and criterion 4 (a cancelled scale-down reopens the gate, covered
by envtest) are the test suite's, not this runbook's. But criterion 4 costs
one command here and is worth seeing on a real kubelet, because envtest cannot
show the probe and the endpoint coming back — only that the operator asked.
Before the player quits, and while the pod is still `NotReady`:

```bash
nix develop -c kubectl patch proxygroup gateway -n minecraft --type=merge \
  -p '{"spec":{"replicas":2}}'
```

Expect, within about 15 seconds: the `draining-since` annotation removed, the
pod's `Ready` condition back to `True`, and its endpoint back to `ready=true`
— with the player who was on it never having noticed any of it. Then set
`replicas` back to 1 and carry on with expectation 7.

## 10. The deadline case, run on purpose

**This is the only path in milestone 4c-1 that disconnects a player.** It is
run here so that it has been seen once, by someone expecting it, rather than
met for the first time by an operator who scaled a group down at a bad moment
and does not know why somebody got dropped.

Lower the timeout first, before scaling down. It can be lowered mid-drain and
that works (fact 6), but doing it as a separate step keeps the deadline
running from a moment you chose.

```bash
nix develop -c kubectl patch proxygroup gateway -n minecraft --type=merge \
  -p '{"spec":{"drain":{"timeoutSeconds":60}}}'
```

60 is comfortably above the CRD's `minimum: 1` and comfortably above the ~15
seconds the readiness drop takes, so there is time to confirm the drain
started before it ends. Make sure `replicas` is back at 2 and both pods are
`Ready`, then repeat §8 — the `$DOOMED` pod is whichever is newest *now*, and
after §9 that is a different pod from before, so re-run the lookup and move
the `evidence.local/pin` label rather than assuming.

```bash
nix develop -c kubectl label pods -n minecraft \
  -l spawnery.cloud/role=proxy,spawnery.cloud/group=gateway evidence.local/pin-
DOOMED=$(nix develop -c kubectl get pods -n minecraft \
  -l spawnery.cloud/role=proxy,spawnery.cloud/group=gateway \
  --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{.items[-1:].metadata.name}')
nix develop -c kubectl label pod -n minecraft "$DOOMED" evidence.local/pin=doomed
```

Rejoin with the real client on `127.0.0.1:30568`, confirm from §7's log
command — taking the most recent line by timestamp, its rule for a repeat
join — that it landed on `$DOOMED`, and scale down:

```bash
nix develop -c kubectl patch proxygroup gateway -n minecraft --type=merge \
  -p '{"spec":{"replicas":1}}'
```

**Expect §9's expectations 1 through 5 exactly as before** — annotation,
readiness drop, `ready=false` on a still-listed address, an uninterrupted
session, `PLAYERS 1` for as long as that session lasts. Nothing about the
deadline changes any of them. Then, about 60 seconds after the annotation's
timestamp:

**Expectation A — the player is disconnected, and the client tells them
something.** Expect a disconnect at the moment the pod is deleted. On
2026-08-14, driven with a real client against this repository's own machine,
the message on screen was **"proxy shutting down"**, a little over a minute
after the scale-down. That text is Velocity's, from its graceful shutdown on
the `SIGTERM` the pod deletion sends; the operator writes nothing to the
player and has no channel to. Milestone 3's manual session saw no disconnect
screen at all, so a message here is what this path added rather than a fault —
and a disconnect with no message, or with a network error instead, is the
thing worth reporting. Confirm it with the person driving rather than
inferring it from the pod list, and write down the words they saw. A session
that survives past the deadline means the deadline did not fire, which is the
defect, not the disconnect.

Nothing hands the player anywhere else, and that is by design rather than an
omission: a draining *server* can move its players because their connections
terminate at the proxy, which stays; a draining *proxy* has no such option,
because the connection terminates at the thing being removed.

**Expectation B — one `Warning` event, naming the pod, the timeout and the
count.**

```bash
nix develop -c kubectl get events -n minecraft \
  --field-selector involvedObject.kind=ProxyGroup,involvedObject.name=gateway \
  --sort-by=.lastTimestamp
```

Expect exactly one, of this shape:

```
Warning   ProxyDrainTimeout   proxygroup/gateway   deleting proxy gateway-yyyy after 1m0s with 1 player(s) still connected
```

`1m0s` is `spec.drain.timeoutSeconds` rendered as a Go duration, so it tracks
whatever you patched — 60 prints as `1m0s`, 300 as `5m0s`, 90 as `1m30s`.

**The count is the last known count, not a measurement, and this is where you
need to know it.** It is whatever the pod's agent last reported. In this run
the agent is alive and streaming — readiness has nothing to do with the gRPC
session — so `1` is what you should see, and anything else is worth
investigating. But in the field the number is a floor, not a total: a proxy
whose agent stream broke with seven players still on it is announced with
whatever it last reported, and a proxy whose agent never connected at all is
announced as `0 player(s)` while disconnecting everyone on it. The event is
right about *which* pod and *that* sessions were lost. If you ever see
`0 player(s)` on an event that plainly dropped somebody, that is the known
shape of this and not a new defect.

Related, and visible only if you go looking: a surplus proxy that has
*crashed* also waits out its full deadline and is then announced as losing the
players its last report named — players the crash had already disconnected.
The event overstates there. Both behaviours err the same way, towards keeping
a pod that might still have someone on it, and both are bounded by the
deadline.

**Expectation C — the pod is gone.** One proxy pod left, and the group back to
`READY 1 PLAYERS 0 PHASE Ready`. The count drops as soon as the pod carries a
deletion timestamp rather than when the client's screen changes (fact 3), so
`PLAYERS 0` here can be true a moment before the person tells you they were
dropped.

Record the event verbatim, the annotation timestamp it should be 60 seconds
after, and the player's account of the disconnect — including what the client
actually showed them, which is the part nobody can reconstruct later.

## 11. Clean up

Stop the operator first and confirm it is stopped, before the cluster goes:
until `kind delete cluster` finishes there is still an API server for a
surviving operator to reconcile against, silently, on a cluster you have just
finished measuring.

```bash
pkill -x spawnery-operat
ps -eo pid,comm | grep spawnery   # expect no output
```

Both of those exit 1 when they find nothing — `pkill` because it matched no
process, `grep` because it printed no line — which is the good outcome here
and an aborted script under `set -e`. Give each a `|| true` if you are driving
this from one.

Then the rest:

```bash
podman rm -f spawnery-4c1-relay
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind delete cluster --name spawnery-4c1
rm -f /tmp/spawnery-4c1-kind.yaml /tmp/spawnery-join
```

**Neither the `pkill` nor the `ps` above matches on a command line, and that
is the point.** `pkill -f` and `pgrep -f` match against every process's *full
command line*, and a shell driving this document non-interactively — one fed to
a shell as `sh -c "<script text>"` — carries the whole script in its own
command line, including §4's `go run ./cmd/spawnery-operator`. Bracketing the
pattern does not save it: `pkill -f`'s pattern is an Extended Regular
Expression, so `[g]o run …` matches the *text* `go run …`, and §4 put that
text in the driving shell's command line. Measured 2026-08-14 on this
repository's machine: from inside an `sh -c` script containing both spellings,
`pgrep -af '[g]o run ./cmd/spawnery-operator'` printed the driving shell's own
PID — and the parent shell that had launched it, whose command line contained
the script text too. With `pkill` in place of `pgrep` that is a script killing
the shell running it. Earlier revisions of this section argued the opposite
and were wrong; the bracket trick only holds while the pattern's text occurs
nowhere else in that command line, and §4 is the occurrence that breaks it.

Without `-f`, `pkill`, `pgrep` and `ps -o comm` match the process *name*
instead: the basename of the executable the process is actually running, which
is set at `exec` and owes nothing to the arguments. A shell running this
document as a script is therefore named after its own interpreter, not after
the script or its contents — measured the same day, a script named
`spawnery-operator-run.sh` runs under the name `bash`. That is what makes a
name match answer "is the operator running" rather than "am I running a script
that mentions the operator".

The name is spelled `spawnery-operat` because Linux truncates it to 15
characters and `spawnery-operator` is 17: `ps -eo comm` prints
`spawnery-operat` for the running operator. Spelling it in full matches
nothing, and both tools say so rather than failing quietly — `pkill -x
spawnery-operator` answers *"pattern that searches for process name longer
than 15 characters will result in zero matches"* and exits 1. All three
measured 2026-08-14 against a binary of that name; `pkill -x spawnery-operat`
against the same process exits 0 and terminates it.

**Killing the compiled binary is the target, not the `go run` around it.**
`go run` compiles to a temporary binary and runs it as a child process, and
the signal does not travel down: sending `SIGTERM` to the wrapper left the
compiled binary running and reparented — measured 2026-08-14 in this
repository's devshell, Go 1.26.5. Upwards it does travel: killing the child by
name ends it, `go run` prints `signal: terminated` and exits, and the
backgrounded job ends with it — in the run measured that day, `nix develop -c`
had `exec`ed into `go` itself, so the job's PID and `go`'s were one and the
same. So the single `pkill` above is aimed at the process that actually
matters and the wrappers follow it. The `ps` line is there because all of that
is an argument, and an argument is not a check.

That also settles what to do with the job number. If you started the operator
at an *interactive* prompt, `kill %1` works, because job control puts the job
in its own process group and the compiled child inherits it, so the signal
reaches the whole group — measured 2026-08-14. In a script job control is off,
the child sits in the shell's own process group, and `kill %1` there left the
compiled operator running — measured in the same pair of runs, one with job
control on and one with it off, differing in nothing else.
`pkill -x spawnery-operat` behaves the same either way, which is why it is
what this section uses.

What the `ps` line answers is narrow, and worth knowing precisely. It lists
the processes on this machine whose *executable* is named after this
repository: the operator §4 started is one, and a `spawnery-stubop` left over
from an interrupted `make agent-test` would be another. If it prints anything,
`kill` that PID and run it again. The agents are not in scope for it — they
are plugins inside a JVM in the cluster's own containers, and the executable
there is Java's — and neither is anything running inside the cluster, which
`kind delete cluster` takes below.

`gateway-pin` and the `evidence.local/pin` label go with the cluster; neither
exists anywhere in this repository's manifests, and neither should be
recreated outside this runbook.

## Where this goes

Everything §9 and §10 produce — the endpointslice lines at each stage, the
annotation timestamps, the pod lists, the `ProxyDrainTimeout` event, and the
player's own account of both sessions — belongs in
`docs/handover-milestone-4.md`, beside the record of milestone 3's manual
session, unless milestone 4c-1 gets a handover document of its own, in which
case there. This file is the procedure; that one is the record of what running
it produced.

Three things are worth stating explicitly in whatever you write:

- **which pod the client was on**, and how you established it. A run that
  cannot answer that has not proven criterion 1, however well the session
  went.
- **which `Service` the surviving session ran through** — the group's own on
  30567, or §8's `gateway-pin` on 30568. They differ in
  `externalTrafficPolicy` (`Local` versus `Cluster`), and a later reader of
  the evidence cannot reconstruct which path was actually measured without
  being told.
- **what the player saw**, in both §9 and §10. The logs prove what the proxy
  did. Only the person at the keyboard can say what the game showed, and in
  §10 the whole finding is that it showed a disconnect.
