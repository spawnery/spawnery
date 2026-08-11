# Runbook: milestone 3's two proofs

Status: written at the end of milestone 3c (2026-08-11), for a `x86_64-linux`
machine with rootless Podman, the same shape `README.md`'s local-cluster
section documents.

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
4. **Whether a held connection is *counted* in `status.connectedPlayers` is
   unmeasured going into this run.** `spawnery-join --hold` stops one packet
   after `Login Acknowledged`, in the configuration state; nothing in this
   milestone measured whether Velocity's own player registry — which is what
   `ProxyState` samples — already carries a player who has not yet reached
   the play state. §9's automated-proof step below is written as a fork for
   exactly that reason: if the count reads 1, record it and move on; if it
   reads 0, that is not necessarily a broken proxy — see the fork for what to
   check before concluding that, and for the small, scoped fix if it turns
   out to be real (two packet-id constants and a `case` in
   `mcjoin`'s `holdOpen`, not a rewrite).

## 0. Prerequisites

- `x86_64-linux`. The image targets do not build on Darwin (`docs/known-issues.md`,
  "From milestone 2b").
- Rootless Podman, with `docker` aliased to it or `CONTAINER=podman` passed
  explicitly below.
- A clone of this repository, `nix develop` available.
- For the manual proof (§4): a second machine or a real Minecraft client
  capable of a Microsoft login, and a NodePort this runbook's cluster
  publishes to somewhere that client can reach — see §4 for the local case.

## 1. Build and load both images

```bash
cd /path/to/spawnery
nix develop -c make image-load          # builds .#paper-image, podman load
nix develop -c make velocity-image-load # builds .#velocity-image, podman load
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

systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind create cluster --name spawnery-evidence \
  --config /tmp/spawnery-evidence-kind.yaml
```

## 3. Load the images into the cluster and apply the CRDs

```bash
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind load docker-image ghcr.io/spawnery/paper:26.2-0.2.0 \
  --name spawnery-evidence
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind load docker-image ghcr.io/spawnery/velocity:3.5.1-0.2.0 \
  --name spawnery-evidence

nix develop -c kubectl apply -f config/crd/bases
```

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

## 5. Apply the network

One `Network`, one forwarding-secret `Secret`, one `ServerGroup` (`lobby`,
one replica to start — §6 below scales it), and two `ProxyGroup`s that share
the same `fallbackGroups` and differ only in `onlineMode` and their NodePort.

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
kind: ProxyGroup
metadata:
  name: gateway-auto
  namespace: minecraft
spec:
  networkRef:
    name: evidence
  replicas: 1
  image: ghcr.io/spawnery/velocity:3.5.1-0.2.0
  expose:
    type: NodePort
    nodePort:
      port: 30565
  routing:
    fallbackGroups:
      - lobby
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
  expose:
    type: NodePort
    nodePort:
      port: 30566
  routing:
    fallbackGroups:
      - lobby
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

NAME                                        PHASE   READY   ADDRESS
proxygroup.spawnery.cloud/gateway-auto      Ready   1       <nodeIP>:30565
proxygroup.spawnery.cloud/gateway-manual    Ready   1       <nodeIP>:30566

NAME                              PHASE   SLOTS   PLAYERS   REGISTERED
server.spawnery.cloud/lobby-xxxx  Ready   100     0         true

NAME              READY   STATUS
pod/lobby-xxxx     1/1    Running
pod/gateway-auto-yyyy      1/1    Running
pod/gateway-manual-zzzz    1/1    Running
```

If a `ProxyGroup` stops in `Pending` with its pod `Running` and `READY 0/1`,
the ready gate did not bind — `docs/known-issues.md`'s "From milestone 3c"
entry on a silent bind failure applies: `kubectl logs` on the pod is the only
place that says why, the CR will not.

## 6. The automated proof

Design §10 criterion 7: `spawnery-join` reaches the point where Velocity
connects it to a backend, and the far side shows it — Paper's own log, then
Velocity's own log, then `status.connectedPlayers`.

```bash
nix develop -c go build -o /tmp/spawnery-join ./cmd/spawnery-join
/tmp/spawnery-join --host 127.0.0.1 --port 30565 --hold 20s
```

Expect exit code 0 and one line of JSON on stdout, of the shape

```json
{"protocol":776,"username":"spawnery_probe","uuid":"...","compressed":true}
```

`protocol` is whatever the pinned Velocity build announces (776 was measured
against 3.5.1 build 615 in `internal/mcjoin`'s package comment; a Velocity
bump moves this number, not this runbook). While the process is still
inside its 20-second hold, in a second shell:

```bash
nix develop -c kubectl get proxygroup gateway-auto -n minecraft \
  -o jsonpath='{.status.connectedPlayers}'
```

**Two outcomes, both worth recording, and neither is "the run failed":**

- **Reads `1`.** The unmeasured question in item 4 above is answered: a
  held, configuration-state connection is counted. Record the value and the
  timestamp in `docs/handover-milestone-4.md` and move on.
- **Reads `0`.** Before concluding the proxy under-reports, check
  `kubectl logs` on the `gateway-auto` pod for a `player_count` line (or
  restart the phase-4 style check from `hack/agent-test.sh` against this
  proxy) to see whether Velocity's own player registry saw the connection at
  all. If it did and the operator's own count is still zero, the gap is real
  and belongs in `docs/known-issues.md`; if Velocity's registry itself did
  not count a configuration-state player, the fix is scoped and small — two
  packet-id constants (`idFinishConfiguration`,
  `idAcknowledgeFinishConfiguration`) and one `case` in `mcjoin`'s
  `holdOpen` that answers the finish-configuration handshake, not a rewrite
  of the client. Do not spend more than this runbook's own budget chasing it
  by hand; file it and move on to the manual proof, which does not depend on
  this count.

Then the logs, which are the primary evidence regardless of which branch
above was hit:

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

The `lobby` group needs a second server for the move to land on —
`agent/velocity/.../Router.choose` excludes the server being drained from its
own candidate list, so with only one server in the group there is nowhere to
move to and the drain would correctly strand the player rather than prove
anything. Scale it up first:

```bash
nix develop -c kubectl patch servergroup lobby -n minecraft --type=merge \
  -p '{"spec":{"scaling":{"minReplicas":2,"maxReplicas":10,"spareSlots":40}}}'
sleep 40
nix develop -c kubectl get servers -n minecraft
```

Expect two `Ready` servers, e.g. `lobby-aaaaa` and `lobby-bbbbb`. Join and
hold long enough to survive the drain — the operator repeats `DrainPlayers`
on roughly a 30-second cadence alongside its periodic `FullSync`
(`internal/proxyreg/fleet.go`), so the hold has to clear at least one of
those:

```bash
/tmp/spawnery-join --host 127.0.0.1 --port 30565 --hold 60s &
sleep 5
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=gateway-auto --tail=5
```

Note which of the two servers the log names — that is the one to delete.
Then, while the hold is still running:

```bash
nix develop -c kubectl delete server lobby-aaaaa -n minecraft   # whichever one it joined
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/group=gateway-auto -f
```

**What to expect is Velocity's own connection log, not a line from the agent
itself.** `agent/velocity/.../Drain.kt` logs a `spawnery:` line only on
failure — no target available, or a move that threw — and stays silent on
success by design, so its comment reads: "[Drain] itself remembers nothing
between calls — the proxy's own connection state is the only memory this
needs." The proof of a successful move is the same shape of line §6 already
established, twice: a disconnect-and-reconnect for `spawnery_probe`, ending
on the server that was not deleted:

```
[server connection] spawnery_probe -> lobby-bbbbb has connected
```

If nothing moves within about 40 seconds of the delete, check the proxy log
for `no target available` first — that names the actual failure (an empty
`toGroups`, or the second server not yet `Ready`) rather than leaving a
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

## Where this goes

The output of §6 through §8 — exit codes, JSON lines, `status` fields, log
lines, timestamps — belongs in `docs/handover-milestone-4.md`, in the
section that document leaves for it. This runbook is the procedure; that
document is the record of what running it actually produced.
