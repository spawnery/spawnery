# Runbook: milestone 3's two proofs

Status: written at the end of milestone 3c (2026-08-11), for a `x86_64-linux`
machine with rootless Podman, the same shape `README.md`'s local-cluster
section documents. Corrected 2026-08-12 after the first real run against a
`kind` cluster: six defects in the procedure below stopped that run at
various points and are fixed in place rather than noted as a fork, and §10 is
new, added for the manual proof this run could not attempt.

Design §10 states milestone 3's acceptance in two sentences: a player can
join, automated, and a player can join, manually, with a real Microsoft
account. This is every command between an empty machine and both of those,
plus the drain proof design's criterion 9 asks for. It is deliberately a
runbook and not a script — design §10 says why: the operator still runs
outside the cluster through `go run`, the local flow hand-builds the
`Service` and `Endpoints` its own pods dial, and turning that into a script is
milestone 6's work.

Each step says what to expect. A deviation from the expected output is a
defect somewhere earlier in this milestone, not a documentation problem — see
`docs/known-issues.md`'s "From milestone 3c" section before assuming the
runbook itself is wrong.

**Four things this runbook gets right that earlier drafts of the plan did
not**, established only by tasks 10 and 10b, late in this milestone:

1. **`online-mode` is turned off through `spec.config.onlineMode: false` on
   the `ProxyGroup`**, never through a `configOverlay`. `internal/render.Velocity`
   reasserts the four keys it owns — `enabled`, `online-mode`,
   `forwarding-secret-file` and `bind` — after merging any overlay, so an
   overlay could never have reached `online-mode` no matter how this runbook
   phrased it. The field exists on the CRD for exactly this reason and
   because a security switch belongs on an object a person reviews, not in a
   ConfigMap.
2. **The probe's username is `spawnery_probe`, with an underscore.** A
   hyphen is not a legal Minecraft username. Velocity accepts
   `spawnery-probe` and forwards it anyway; Paper then kills the forwarded
   connection with `Invalid characters in username`, and the proxy reports
   that to the client as a generic `disconnect.genericReason` that names
   neither the field nor the character. `cmd/spawnery-join`'s default is
   already `spawnery_probe` — do not override it to something with a hyphen
   while improvising a variant.
3. **`spawnery-join` closes its connection the moment it returns.** By the
   time a shell prompt comes back and the next line of this runbook runs
   `kubectl get`, the player has already left. Every use of `spawnery-join`
   below that expects `status.connectedPlayers` to be non-zero passes
   `--hold`.
4. **A held connection *is* counted in `status.connectedPlayers`, and no
   configuration-phase play-through is needed.** `spawnery-join --hold` stops
   one packet after `Login Acknowledged`, in the configuration state, and an
   earlier draft of this runbook forked on whether Velocity's own registry —
   which is what `ProxyState` samples through `proxy.playerCount` — carries a
   player who has not reached the play state. It does. Disassembled out of the
   pinned jar (3.5.1 build 615, 2026-08-11):
   `AuthSessionHandler.completeLoginProtocolPhaseAndInitialize` calls
   `VelocityServer.registerConnection` in its `LoginEvent` continuation,
   *before* it writes `ServerLoginSuccessPacket` to the client — so before
   Login Acknowledged, before `PostLoginEvent` and before
   `connectToInitialServer` — and `VelocityServer.getPlayerCount()` is
   `connectionsByUuid.size()`. The player is in that map from login success
   onwards.

   One thing still has to line up, and it is about the operator rather than
   the proxy: `status.connectedPlayers` is only current if a report and a
   reconcile happen *inside* the hold. That is why the holds below are 60
   seconds and not 20.

## 0. Prerequisites

- `x86_64-linux`. The image targets do not build on Darwin (`docs/known-issues.md`,
  "From milestone 2b").
- Rootless Podman, with `docker` aliased to it or `CONTAINER=podman` passed
  explicitly below. On a machine with no `docker` binary at all — measured on
  2026-08-12 — `CONTAINER=podman` is not optional: the Makefile defaults to
  `docker`, and every target below has to carry the override.
- `TMPDIR` pointed at a real filesystem, not the default `/tmp`. Where `/tmp`
  is a tmpfs, Podman's OCI-archive extraction runs out of room silently
  enough to look like a hang. `mkdir -p "$HOME/.cache/spawnery-tmp"` once,
  and pass `env TMPDIR="$HOME/.cache/spawnery-tmp"` on every command below
  that builds or loads an image.
- A shell with `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` set — true of
  an interactive login shell, not true of every other shell a terminal
  multiplexer or a remote session hands you. Every `systemd-run --scope
  --user` command below needs both; without them it fails with `Failed to
  connect to user scope bus via local transport: $DBUS_SESSION_BUS_ADDRESS
  and $XDG_RUNTIME_DIR not defined`. Fix it once, before the first
  `systemd-run` command in §2:

  ```bash
  export XDG_RUNTIME_DIR=/run/user/$(id -u)
  export DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus
  ```

  This does not change Podman's own storage — measured, the runroot and
  graphroot are identical whether the export ran first or not, so an image
  loaded before the export stays visible to a `systemd-run` command that
  comes after it.
- A clone of this repository, `nix develop` available.
- The images carry no `grep` or `awk`. Any `kubectl exec` that inspects a
  file inside a pod has to `cat` it and filter on the host instead.
- For the manual proof (§7): a second machine or a real Minecraft client
  capable of a Microsoft login, and a NodePort this runbook's cluster
  publishes to somewhere that client can reach — see "The manual proof, for a
  later session" at the end of this document for the case where that machine
  is not this one.

## 1. Build and load both images

The Makefile's `CONTAINER` defaults to `docker`; where there is no `docker`
binary at all, it has to be overridden rather than relied on to fall back.
`TMPDIR` matters for the same reason it is in the prerequisites: Podman
extracts the OCI archive there, and the default `/tmp` on this machine is a
tmpfs too small for it.

```bash
cd /path/to/spawnery
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make image-load CONTAINER=podman          # builds .#paper-image, podman load
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make velocity-image-load CONTAINER=podman # builds .#velocity-image, podman load
```

Expect two image loads to report their tags:
`ghcr.io/spawnery/paper:26.2-0.2.0` and
`ghcr.io/spawnery/velocity:3.5.1-0.2.0` — the same two tags
`config/samples/network.yaml` already names. If the tags differ (a Paper or
Velocity bump since this was written), use the ones these two commands
actually print for every step below.

## 2. Create the kind cluster, with the two NodePorts published to the host

`kind`, not `k3d`: `docs/known-issues.md` under "From milestone 2b" records
why `k3d` cannot start against a rootless Podman socket at all. The
`systemd-run` wrapper is required for the same reason `README.md` uses it —
kind needs cgroup delegation kind's own check insists on a scope for, even
when the property is already set elsewhere.

This runbook uses two `ProxyGroup`s on two NodePorts — 30565 for the
automated proof, 30566 for the manual one — so both proofs can be repeated
independently without tearing the cluster down between them. Neither port is
in kind's default port mapping, so both are declared explicitly:

```bash
cat >/tmp/spawnery-evidence-kind.yaml <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30565
        hostPort: 30565
      - containerPort: 30566
        hostPort: 30566
EOF

# Needs XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS set — see §0 — or this
# fails with "Failed to connect to user scope bus via local transport".
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind create cluster --name spawnery-evidence \
  --config /tmp/spawnery-evidence-kind.yaml
```

## 3. Load the images into the cluster and apply the CRDs

**`kind load docker-image` does not work under Podman, no matter what
`KIND_EXPERIMENTAL_PROVIDER` says.** It shells out to the `docker` binary
unconditionally, so on a machine with no `docker` at all it fails with
`ERROR: image: "ghcr.io/spawnery/paper:26.2-0.2.0" not present locally` —
even though `podman image inspect` on the exact same name returns an ID at
that moment. This is the single most misleading failure in this runbook,
because the error blames the image rather than the tool. The supported path
under Podman is `kind load image-archive`, and `nix build` already produces
an archive shaped for it — no separate export step needed:

```bash
nix build .#paper-image --out-link "$HOME/.cache/spawnery-tmp/paper-img"
nix build .#velocity-image --out-link "$HOME/.cache/spawnery-tmp/velocity-img"

systemd-run --scope --user --property=Delegate=yes --quiet \
  env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" \
  nix develop -c kind load image-archive \
  "$HOME/.cache/spawnery-tmp/paper-img" --name spawnery-evidence
systemd-run --scope --user --property=Delegate=yes --quiet \
  env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" \
  nix develop -c kind load image-archive \
  "$HOME/.cache/spawnery-tmp/velocity-img" --name spawnery-evidence

nix develop -c kubectl apply -f config/crd/bases
```

Verify both landed, since the load itself prints nothing that names the
image:

```bash
podman exec spawnery-evidence-control-plane crictl images
```

Expect both `ghcr.io/spawnery/paper:26.2-0.2.0` and
`ghcr.io/spawnery/velocity:3.5.1-0.2.0` listed.

## 4. Run the operator outside the cluster, and hand-build what its pods dial

The operator has no image of its own — out of scope for all of milestone 3,
named as such in design §7 and again in §9 — so it runs here through `go
run`, exactly as `README.md`'s local-cluster section does, and the same gap
that section documents applies: a pod dials
`spawnery-operator.minecraft.svc:9443`, and nothing creates that `Service`
because the operator has no pod for a selector to find. `docs/known-issues.md`
under "From milestone 2c" — "The local kind flow needs a `Service` nothing
creates" — is the record of why this is a hand-built relay rather than a
missing step; do not read it as a shortcut this runbook took.

```bash
nix develop -c kubectl create namespace minecraft
nix develop -c go run ./cmd/spawnery-operator \
  --leader-elect=false --operator-namespace minecraft &
```

Rootless Podman's `kind` network cannot be reached at its gateway address
from inside a pod, and the one address that can reach the host
(`host.containers.internal`, a pasta link-local address) is rejected by the
API server in `Endpoints`. `README.md` measures the same wall and its fix —
a one-container relay on the same Podman network — applies verbatim here:

```bash
podman run -d --name spawnery-evidence-relay --network kind \
  -v /nix/store:/nix/store:ro \
  --entrypoint "$(nix build --no-link --print-out-paths nixpkgs#socat)/bin/socat" \
  ghcr.io/spawnery/paper:26.2-0.2.0 \
  TCP-LISTEN:9443,fork,reuseaddr TCP:host.containers.internal:9443
RELAY_IP=$(podman inspect spawnery-evidence-relay \
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

With a real Docker daemon instead of rootless Podman, skip the relay and
point the `Endpoints` at the bridge gateway (`172.17.0.1`) directly, the way
`README.md` notes.

**`v1 Endpoints` is deprecated as of Kubernetes 1.36**, in favour of
`discovery.k8s.io/v1 EndpointSlice`. It still works — measured, applying it
verbatim above still worked and the pod still routed through the relay — but
`kubectl apply` prints a deprecation warning, and this section will need
rewriting to `EndpointSlice` once that stops being merely a warning.

## 5. Apply the network

One `Network`, one forwarding-secret `Secret`, two `ServerGroup`s (`lobby` and
`hub`, one replica each to start — §8 scales `lobby`), and two `ProxyGroup`s
that share the same `fallbackGroups` and differ only in `onlineMode` and their
NodePort.

**Give both `ProxyGroup`s their own, smaller `spec.resources`.** This
runbook's four pods (two backends inheriting `Network.defaults.resources` at
2Gi request/limit, plus two proxies) ask for 8Gi on a node with roughly 8Gi
allocatable — measured: the fourth pod sits in `Pending` with `0/1 nodes are
available: 1 Insufficient memory`. Velocity does not need a backend's heap.
Both `ProxyGroup` manifests below carry `requests: cpu 500m, memory 1Gi;
limits: memory 1Gi` for exactly this reason, rather than as an optional
tuning note — without it this section does not fit an 8Gi machine.

**Two groups, in that order, on purpose.** `fallbackGroups` is a try list:
`agent/velocity/.../Router.choose` takes the first group that holds a
candidate and never looks at a later one, so with `lobby` ahead of `hub` every
ordinary join lands in `lobby` and `hub` is only reached when `lobby` offers
nothing. That second path is exactly what §8 drains into, and with a
single-group list it could not be observed at all.

```bash
nix develop -c kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft
stringData:
  secret: evidence-run-forwarding-secret
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
kind: ServerGroup
metadata:
  name: hub
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
  name: gateway-auto
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
      port: 30565
  routing:
    fallbackGroups:
      - lobby
      - hub
  config:
    playerLimit: 100
    motd: "spawnery-join, automated"
    onlineMode: false
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway-manual
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
      port: 30566
  routing:
    fallbackGroups:
      - lobby
      - hub
  config:
    playerLimit: 100
    motd: "manual join, online-mode default"
EOF
```

`gateway-manual` deliberately omits `config.onlineMode`; the CRD's own
`+kubebuilder:default=true` fills it in, which is what a real Microsoft
account needs to answer an encryption request against.

Wait for both groups and the backend to reach `Ready`. Because the network
retry interval is 30 seconds and Paper's own start plus the agent's handshake
takes about the same again, allow a good 90 seconds before the first check:

```bash
sleep 90
nix develop -c kubectl get network,servergroup,proxygroup,servers,pods -n minecraft
```

Expect, by shape rather than by exact name (pod names carry a random
suffix):

```
NAME                                      ACCEPTED   SERVER GROUPS
network.spawnery.cloud/evidence           True       1

NAME                                PHASE   READY   FREE SLOTS
servergroup.spawnery.cloud/lobby    Ready   1       100
servergroup.spawnery.cloud/hub      Ready   1       100

NAME                                        PHASE   READY   ADDRESS
proxygroup.spawnery.cloud/gateway-auto      Ready   1       <nodeIP>:30565
proxygroup.spawnery.cloud/gateway-manual    Ready   1       <nodeIP>:30566

NAME                              PHASE   SLOTS   PLAYERS   REGISTERED
server.spawnery.cloud/lobby-xxxx  Ready   100     0         true
server.spawnery.cloud/hub-wwww    Ready   100     0         true

NAME              READY   STATUS
pod/lobby-xxxx     1/1    Running
pod/hub-wwww       1/1    Running
pod/gateway-auto-yyyy      1/1    Running
pod/gateway-manual-zzzz    1/1    Running
```

**The operator does not re-spec an existing pod.** If a pod is already
`Pending` on insufficient memory when its `ProxyGroup`'s `spec.resources` is
patched — for instance because an earlier attempt at this section used
`Network.defaults.resources` for the proxies too, before finding the fit
problem above — the patch alone does not fix it: the already-created pod
keeps the spec it was born with, and only `kubectl delete pod` on it lets the
operator recreate it with the new one. Rolling updates that would do this
automatically are milestone 4's, not this milestone's — see
`docs/handover-milestone-4.md`.

If a `ProxyGroup` stops in `Pending` with its pod `Running` and `READY 0/1`,
the ready gate did not bind — `docs/known-issues.md`'s "From milestone 3c"
entry on a silent bind failure applies: `kubectl logs` on the pod is the only
place that says why, the CR will not.

## 6. The automated proof

Design §10 criterion 7: `spawnery-join` reaches the point where Velocity
connects it to a backend, and the far side shows it — Paper's own log, then
Velocity's own log, then `status.connectedPlayers`.

`--hold` has to fit inside `--timeout`, and the default `--timeout` is 30s:
`--hold 60s` with no `--timeout` is refused outright —
`spawnery-join: --hold 1m0s does not fit inside --timeout 30s` — naming both
flags in the same line. That refusal is correct behaviour, not a bug to work
around quietly; the fix is to widen the deadline explicitly:

```bash
nix develop -c go build -o /tmp/spawnery-join ./cmd/spawnery-join
/tmp/spawnery-join --host 127.0.0.1 --port 30565 --hold 60s --timeout 90s
```

Expect exit code 0 and one line of JSON on stdout, of the shape

```json
{"protocol":776,"username":"spawnery_probe","uuid":"...","compressed":true}
```

`protocol` is whatever the pinned Velocity build announces (776 was measured
against 3.5.1 build 615 in `internal/mcjoin`'s package comment; a Velocity
bump moves this number, not this runbook). While the process is still
inside its 60-second hold, in a second shell, and not in the first ten
seconds of it — the agent samples once a second but the operator's report
interval and the `ProxyGroup` reconcile that writes the status are what
decide when it appears:

```bash
sleep 15
nix develop -c kubectl get proxygroup gateway-auto -n minecraft \
  -o jsonpath='{.status.connectedPlayers}'
```

**Expect `1`.** Item 4 above is why this is an expectation and not a fork: a
held player sits in Velocity's `connectionsByUuid` from login success
onwards, which is what `proxy.playerCount` counts and what the agent reports.

A `0` here is therefore a real defect, not an artifact of stopping in the
configuration state, and the place to look is the path between the proxy and
the CR rather than the client: `kubectl logs` on the `gateway-auto` pod for
what the agent reported, then whether a reconcile has run since. Record it in
`docs/known-issues.md` and carry on to the manual proof, which does not depend
on this count.

Then the logs, which are the primary evidence either way:

```bash
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=lobby | grep -i 'spawnery_probe\|UUID of player'
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=gateway-auto | grep -i 'spawnery_probe\|server connection'
```

Expect a Velocity line of the shape

```
[server connection] spawnery_probe -> lobby-xxxx has connected
```

and a Paper line naming the same player's UUID. Both together are the
criterion-7 proof independent of what `status.connectedPlayers` read: the
far side shows the join whether or not the operator's own counter agrees.

## 7. The manual proof

Design §10 criterion 8: one join with a real Microsoft account against
`online-mode: true`, which needs a client that can answer an encryption
request — `spawnery-join` explicitly cannot (`mcjoin.Join` returns an error
naming exactly this if it ever receives one).

Point a real Minecraft Java client at `127.0.0.1:30566` if the client runs on
this machine, or at this machine's address on port 30566 from another one —
the `extraPortMappings` in §2 is what makes the NodePort reachable at all
under kind. Log in with a Microsoft account that has purchased the game.

Expect the same two log lines as §6, this time for the real account's
username, plus — because this is `online-mode: true` — a successful Mojang
authentication with no manual workaround: unlike the measurement recorded in
`internal/mcjoin`'s package comment, this runs the shipped renderer
unmodified, which is the point of proving it this way rather than by hand.

```bash
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=gateway-manual | tail -20
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=lobby | tail -20
nix develop -c kubectl get proxygroup gateway-manual -n minecraft \
  -o jsonpath='{.status.connectedPlayers}'
```

Record the account's username (or a redacted form of it, if that matters to
whoever reviews this), the timestamps, and the log lines in
`docs/handover-milestone-4.md`.

## 8. The drain proof

Design §10 criterion 9: deleting a `Server` with a player on it moves that
player to a fallback rather than disconnecting them.

There are two ways the move can land, and this section proves both, in this
order. `agent/velocity/.../Router.choose` excludes the server being drained
from its own candidate list, so with `lobby` still at one replica the
exclusion empties the first group entirely and the try list has to fall
through to `hub`. Scaling `lobby` up afterwards gives the second shape, where
the first group still holds a candidate and `hub` is never consulted. Only the
first is new to this milestone's coverage, and it is the one a single-group
`fallbackGroups` could not have shown at all.

### 8a. The move that falls through to the second group

`lobby` still has exactly one server here — do not scale it yet. Join and hold
long enough to survive the drain: the operator repeats `DrainPlayers` on
roughly a 30-second cadence alongside its periodic `FullSync`
(`internal/proxyreg/fleet.go`), so the hold has to clear at least one of
those.

**Use a username other than the default here.** If §6's own held connection
has not fully closed yet — measured, on a machine going through this runbook
top to bottom without pausing between sections — this join collides with it:
both would be `spawnery_probe`, and Velocity refuses the second one with
`disconnected: You are already connected to this proxy!`. Give this join its
own identity, `drain_probe` (underscore, for the same reason item 2 of this
document's own preamble gives for `spawnery_probe`'s underscore — a hyphen is
not a legal Minecraft username):

```bash
/tmp/spawnery-join --host 127.0.0.1 --port 30565 --username drain_probe \
  --hold 60s --timeout 90s &
sleep 5
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=gateway-auto --tail=5
```

Expect the log to name the one `lobby` server — `lobby` is first in
`fallbackGroups` and has a candidate, so `hub` is not consulted on the join.
Then, while the hold is still running, delete it:

```bash
nix develop -c kubectl delete server lobby-xxxx -n minecraft
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=gateway-auto -f
```

**What to expect is Velocity's own connection log, not a line from the agent
itself.** `agent/velocity/.../Drain.kt` logs a `spawnery:` line only on
failure — no target available, or a move that threw — and stays silent on
success by design, so its comment reads: "[Drain] itself remembers nothing
between calls — the proxy's own connection state is the only memory this
needs." The proof of a successful move is the same shape of line §6 already
established, this time ending on a server in the *other* group:

```
[server connection] drain_probe -> hub-wwww has connected
```

That line is the fall-through: `lobby` held one server, the exclusion took it
away, and the router went on to `hub` instead of giving up. A `no target
available` line here instead means the router stopped at the emptied group,
which is the defect `RouterTest`'s "a group the exclusion empties falls
through to the next group" pins in the unit suite.

The `ServerGroup` will bring `lobby` back to its `minReplicas` on its own;
wait for it before 8b.

### 8b. The move that stays inside the first group

Scale `lobby` to two so the exclusion leaves a candidate behind:

```bash
nix develop -c kubectl patch servergroup lobby -n minecraft --type=merge \
  -p '{"spec":{"scaling":{"minReplicas":2,"maxReplicas":10,"spareSlots":40}}}'
sleep 40
nix develop -c kubectl get servers -n minecraft
```

Expect two `Ready` lobby servers, e.g. `lobby-aaaaa` and `lobby-bbbbb`, plus
the one `hub` server. Repeat the join (still `--username drain_probe`, still
`--timeout 90s`), the five-second wait and the delete from 8a against
whichever lobby server the log names. This time the move must stay in the
first group:

```
[server connection] drain_probe -> lobby-bbbbb has connected
```

`hub` appearing here instead would mean the try list is being searched rather
than walked in order.

If nothing moves within about 40 seconds of either delete, check the proxy log
for `no target available` first — that names the actual failure (an empty
`toGroups`, or the surviving server not yet `Ready`) rather than leaving a
silent hang to debug from nothing.

## 9. Clean up

```bash
kill %1   # the operator's go run
podman rm -f spawnery-evidence-relay
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind delete cluster --name spawnery-evidence
rm -f /tmp/spawnery-evidence-kind.yaml /tmp/spawnery-join
```

## 10. The manual proof, for a later session

Written 2026-08-12, after an automated run of §1–§6 and §8 against a real
`kind` cluster proved criterion 7 and found the criterion-9 defect recorded
in `docs/known-issues.md`'s "From the milestone 3c evidence run (2026-08-12)"
section. Criterion 8 — a real Microsoft account joining against
`online-mode: true` — was not attempted that day: it needs a licensed
Minecraft Java client and a person to drive it, neither of which was
available in that session. This section is written for whoever does have
both, in a session with no memory of the one that wrote it. Start by reading
`docs/known-issues.md`'s criterion-9 entry before running anything below —
it explains why the automated proof cannot cover this section's second half.

**The cluster from the automated run is gone.** `spawnery-evidence` was torn
down by §9's cleanup at the end of that session; nothing here can reuse its
state, its images, or its NodePort mapping. Start over from §1: build and
load both images, create a fresh `kind` cluster with §2's `extraPortMappings`
(30565 and 30566 published to the cluster host's loopback), load both images
per the corrected §3, run the operator and its relay per §4, and apply the
network manifests — with the `ProxyGroup` resource requests §5 now
carries — per §5. Do not skip §6 in the automated form either: confirming
criterion 7 still holds on this machine, cheaply, before spending a real
account's login on the manual proof, catches an environment problem before
it looks like a product one.

**What this needs beyond the earlier sections:**

- A Minecraft Java Edition client at **26.2**, protocol 776 — the exact
  number `spawnery-join`'s own JSON output printed in the automated proof
  (`{"protocol":776,...}`), and Paper 26.2 will refuse any client that
  announces a different one with a loud "Outdated client!" naming the
  version to install instead. This is not a number to approximate.
- A Microsoft account that owns the game, able to complete a normal
  Minecraft login.
- Network reach from the machine running that client to the cluster host's
  NodePort 30566 (`gateway-manual`, the `ProxyGroup` that leaves
  `config.onlineMode` at its CRD default of `true`).

**Reaching the NodePort from a different host.** `kind`'s
`extraPortMappings` publishes a NodePort to the *cluster host's own*
loopback, `127.0.0.1:30566` — nothing else, by default. If the client runs on
the same machine that ran §1–§5, point it at `127.0.0.1:30566` directly and
skip the rest of this paragraph. If the client is on a different machine —
the expected case, since a session with no Minecraft client available is
exactly why this section exists — the two options are opening 30566 on the
cluster host's LAN address in whatever firewall sits in front of it, or an
SSH tunnel, which needs nothing changed on the host at all. From the
client's machine, with SSH access to the cluster host:

```bash
ssh -N -L 30566:127.0.0.1:30566 <user>@<cluster-host>
```

Then point the Minecraft client at `127.0.0.1:30566` on the client's own
machine; the tunnel forwards it to the cluster host's published NodePort from
there.

**The join itself.** Log in with the Microsoft account, normally, against
`127.0.0.1:30566` (or the LAN address, if that path was used instead).
`gateway-manual` deliberately omits `config.onlineMode` in §5's manifest, so
the CRD's `+kubebuilder:default=true` applies and the proxy issues a real
encryption request the client answers with a genuine Mojang session — this
is what `spawnery-join` cannot do, by design (`internal/mcjoin`'s own package
comment names the limitation). Expect the same two log lines §6 already
established for the automated proof, this time carrying the real account's
username and its real Mojang UUID rather than `spawnery_probe`'s offline-mode
one:

```bash
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=gateway-manual | tail -20
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=lobby | tail -20
```

**The UUID being a real one is the point, not an incidental detail.** An
offline-mode UUID is derived from the username alone and proves nothing about
who is connecting; a real Mojang UUID is only handed back after the client
proved its session to Mojang and the proxy verified it — so seeing it in
Paper's log is what shows the proxy actually authenticated the player, rather
than merely trusting whatever name arrived.

**Criterion 9, folded into this same session, on this same real player.**
This is the part the automated run could not do at all — see
`docs/known-issues.md`'s "From the milestone 3c evidence run (2026-08-12)"
section for the full diagnosis of why `spawnery-join --hold` cannot stand in
for a real client here: it stops one packet after `Login
Acknowledged`, before Paper's own player list ever contains it, so
`Server.status.players` reads zero for a connection the proxy is still
counting, and the operator's drain reads that zero and deletes the pod out
from under an occupied session. A real client completes the configuration
phase on its own, Paper counts it, and the drain that depends on that count
actually waits. While the real player from the join above is still connected
and still in `lobby`, find and delete the `Server` they are on:

```bash
nix develop -c kubectl get servers -n minecraft          # find the one the proxy log named
nix develop -c kubectl delete server lobby-xxxx -n minecraft
```

Expect the proxy to log a move to the *other* fallback group, the same shape
of line established throughout this runbook:

```
[server connection] <player> -> hub-xxxx has connected
```

— a move to `hub` rather than another `lobby` server, because §5's `lobby`
`ServerGroup` starts at one replica and the drain's own exclusion empties it,
the same fall-through §8a already exercised with the held probe. Expect the
player to stay in the game throughout, landing in a different world with no
visible disconnect. A disconnect instead, or a `spawnery:` line reading `no
target available`, is a real defect distinct from the one criterion 9's
automated attempt already found — the automated failure was the operator
acting on a stale player count, not the move logic itself, so a failure here
implicates `agent/velocity/.../Drain.kt` or `Router.kt` directly and is worth
its own investigation rather than being folded into the known criterion-9
entry.

**What to record**, all of it into `docs/handover-milestone-4.md` under the
same heading the automated evidence occupies: the two log lines from each
side of the join (proxy and backend), the player's real Mojang UUID, and the
drain's move line from the criterion-9 step above, each with its timestamp.

## Where this goes

The output of §6 through §8, and of §10 once it is run — exit codes, JSON
lines, `status` fields, log lines, timestamps — belongs in
`docs/handover-milestone-4.md`, in the section that document leaves for it.
This runbook is the procedure; that document is the record of what running
it actually produced.
