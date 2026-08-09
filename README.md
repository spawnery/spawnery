# Spawnery

A Kubernetes-native cloud system for Minecraft networks.

Spawnery runs Paper game servers behind a Velocity proxy layer on Kubernetes —
dynamically scaling minigame and lobby groups as much as persistent survival
worlds. The target platform is RKE2 on bare metal, without ruling out other
distributions.

Servers are described in groups, not in pods:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
spec:
  networkRef: { name: production }
  type: Ephemeral
  maxPlayers: 100
  scaling:
    minReplicas: 1
    maxReplicas: 10
    spareSlots: 40      # how many free slots Spawnery keeps in reserve
```

Scaling follows free player slots rather than CPU, and a server with players is
never deleted: before it stops, its players are moved onto a fallback through
the proxy.

## Status

Under development. Milestone 1 is done: the four CRDs, the operator with the
Network, ServerGroup and Server controllers, the state machine including
readiness loss, and the orphan sweep.

Milestone 2a is done: the agent channel. A gRPC service inside the operator
process accepts TLS connections from game server pods, identifies them through a
pod-bound ServiceAccount token, and the registry behind it feeds the two-stage
ready gate and `status.players` that milestone 1 had left unwired. The channel
is proven end to end in envtest — a test agent brings a `Server` with green pod
readiness all the way to phase `Ready`. `ProxySession`, the other half of the
same contract, still answers `Unimplemented`; it belongs to milestone 3.

Milestone 2b is done: the Paper base image. `nix build .#paper-image` produces
a reproducible image holding Paper 26.2, a JRE 25 and `spawnery-slp`, the tool
the readiness probe calls to speak a real server list ping. Paper is patched at
build time, so a pod downloads nothing at startup; `make image-test` runs the
image offline to keep that true.

Milestone 2c is done: the Paper agent. `nix build .#paper-agent` produces a
shaded, bit-reproducible Kotlin plugin that is baked into the image; the
entrypoint copies it into the pod's writable plugins directory, and from there
it opens an authenticated `ServerSession` to the operator, reports its
readiness and its player counts, and renews the session with overlap so a
renewal never costs a server its `Ready`.

Both halves of the ready gate now close. Measured in a local kind cluster, a
`Server` reaches phase `Ready` about twenty seconds after its pod is created,
and `status.players` and `status.slots` carry what the agent read off the
running server rather than a placeholder.

What is still missing is the whole reason a player would care: the Velocity
proxy layer of milestone 3. Nothing routes a client to a backend,
`ProxySession` still answers `Unimplemented`, and no proxy pod has a
ServiceAccount to identify itself with — so a player still cannot connect to a
network that is otherwise fully ready.

Carry-overs and preconditions for later milestones — CA rotation, the missing
`spawnery-proxy` ServiceAccount, the orphan sweep that would discard a proxy
agent, and what milestones 2b and 2c leave open — are in
[`docs/known-issues.md`](docs/known-issues.md).

The design lives under [`docs/superpowers/specs/`](docs/superpowers/specs/), the
plans under [`docs/superpowers/plans/`](docs/superpowers/plans/).

Anyone starting milestone 3 begins at
[`docs/handover-milestone-3.md`](docs/handover-milestone-3.md): it says where 2c
stopped, what a Velocity agent finds already built, which parts of the Paper
agent apply again almost unchanged, and what has to be settled before code.
[`docs/handover-milestone-2c.md`](docs/handover-milestone-2c.md) and
[`docs/handover-milestone-2b.md`](docs/handover-milestone-2b.md) are its
predecessors, kept as the record of what those milestones started from.

## Development

```bash
nix develop            # Go, controller-gen, envtest assets, kubectl, kind, k3d,
                       # protoc with its Go and Java plugins, JDK 21, Gradle
make test              # unit and envtest tests
make build             # bin/spawnery-operator
make agent             # the Paper agent plugin and its JUnit suite
```

`make proto` regenerates the Go code under `internal/agentpb` from
`proto/spawnery/agent/v1alpha1/agent.proto`. The generated code is checked in
like `zz_generated.deepcopy.go` — after a change to the `.proto`, run `make
proto` and commit the diff with it; `make test` does not regenerate it on its
own.

`make agent` builds the Paper agent plugin (`nix build .#paper-agent`) and runs
its JUnit suite as the derivation's check phase — the target to reach for after
any change under `agent/paper/`. `make agent-deps` regenerates
`agent/paper/deps.json`, the checked-in lockfile that pins every Maven artifact
by hash; it is needed only when `agent/paper/build.gradle.kts` changes a
dependency, and it is deliberately part of no other target, not even `make
all`, because it reaches Maven Central — a dependency change is an explicit act
and a Nix build must never depend on the network. `make agent-test` runs the
real image against the Go stub operator in `cmd/spawnery-stubop` and checks the
handshake, the authorization header, the player reports, the overlapping
renewal and the bound on a session the operator never answers; it is the target
to run after any change to the agent's session handling, and like the image
targets below it needs a container runtime and only works on `x86_64-linux`.

`make image` builds the Paper base image, `make image-load` hands it to the
local container runtime, and `make image-test` runs it offline under the same
constraints the podspec imposes. All three need Docker or Podman and only work
on `x86_64-linux`. Pass `CONTAINER=podman` if `docker` is not your runtime.

`make image-repro` rebuilds the image with `nix build .#paper-image --rebuild`
and fails if the two builds do not produce the same bytes. Design §5.3 makes
that reproducibility an acceptance criterion, not a one-off claim, so this is
the standing check for it — worth running again after any change to
`nix/paper.nix` or `nix/paper-image.nix`. Like `image-test`, it is not part of
`make test` or `make all`: it needs a build's worth of time and only runs on
`x86_64-linux`.

Running this image accepts
[Mojang's EULA](https://www.minecraft.net/eula) on your behalf: the entrypoint
writes `eula=true`, because Paper does not start otherwise.

### Trying it locally against kind

These steps need a container runtime — Docker or Podman — for a local
Kubernetes cluster. On the machine this was last run on, `docker` is a Podman
5.8.4 alias with no `/var/run/docker.sock`, and only a rootless Podman socket
is available. Under that setup `k3d` cannot bring up a cluster at all: its
tools node always bind-mounts the runtime socket to `/var/run/docker.sock`
inside itself, and rootless Podman refuses to create that mount point
(`mkdir /var/run/docker.sock: permission denied`) — no `DOCKER_HOST` value
fixes it, since the failure is in the tools node's own container creation, not
in the client reaching the socket. `kind` under
`KIND_EXPERIMENTAL_PROVIDER=podman` does work against the same rootless
socket, and is what the flow below uses. Anyone with a real Docker daemon (or
a rootful Podman socket) can use `k3d` the same way instead — the manifests
and the operator invocation are identical either way.

kind additionally needs cgroup delegation to run under systemd as a regular
user, hence the `systemd-run --scope --user --property=Delegate=yes` wrapper
around every kind command below: without it, kind refuses with a
`Delegate=yes` error even when that property is already set on the user's
systemd service — the scope is what its check actually looks for.

The operator runs here through `go run` outside the cluster, so without
`POD_NAMESPACE` from the downward API. `--operator-namespace` therefore has to
be set explicitly — without the flag the process refuses to start (see
`validateAgentFlags`), because the serving certificate would otherwise carry the
wrong SANs.

That leaves one gap this flow has to close by hand, and since milestone 2c it
is the difference between a `Server` that reaches `Ready` and one that does
not. The pod dials `spawnery-operator.<ns>.svc:9443`, and nothing creates that
Service: the operator is not in the cluster, so no selector could find it. A
selector-less `Service` with a hand-written `Endpoints` pointing at the host
closes it — the serving certificate already carries that DNS name, so TLS
verifies against the CA the pod was given.

Which address goes into those `Endpoints` depends on the runtime. With a real
Docker daemon it is the bridge gateway, `172.17.0.1`, and nothing further is
needed. Under rootless Podman it is none of the obvious candidates: the gateway
of the `kind` network (`10.89.0.1` here) lives inside the rootless network
namespace, where the operator is not listening and a connection is refused, and
the one address that does reach the host — the pasta link-local
`169.254.1.2`, which Podman also publishes as `host.containers.internal` — is
rejected by the API server in both `Endpoints` and `EndpointSlice` with
`may not be in the link-local range`. What works, measured, is one more
container on the same Podman network relaying to the host: it gets a routable
address on that network, and pods reach it.

```bash
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind create cluster --name spawnery-dev
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind load docker-image ghcr.io/spawnery/paper:26.2-0.2.0 --name spawnery-dev
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
nix develop -c go run ./cmd/spawnery-operator --leader-elect=false --operator-namespace minecraft &

# Rootless Podman only. With a real Docker daemon, skip this and use
# 172.17.0.1 as the endpoint address below.
podman run -d --name spawnery-relay --network kind \
  -v /nix/store:/nix/store:ro \
  --entrypoint "$(nix build --no-link --print-out-paths nixpkgs#socat)/bin/socat" \
  ghcr.io/spawnery/paper:26.2-0.2.0 \
  TCP-LISTEN:9443,fork,reuseaddr TCP:host.containers.internal:9443
RELAY_IP=$(podman inspect spawnery-relay \
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

sleep 90
nix develop -c kubectl get networks,servergroups,servers,pods -n minecraft
```

The image only needs a rootfs for the relay, which is why the Paper image
stands in for one; `socat` itself comes out of the mounted Nix store.

The first server can take a good half minute to appear: if the ServerGroup
meets its network before the Network controller has accepted it, it tries again
only after `networkRetryInterval` (30 seconds). The 90 seconds above also cover
Paper's own start — about seven seconds to a first answered ping — and the
agent's handshake after it. Loading the image into the cluster beforehand is
its own wait: at 26.2-0.2.0 it is 735 MB as a tarball, a little over a gigabyte
unpacked.

Expected, as measured on 2026-08-10 against `kind` v1.36.1 under rootless
Podman:

- `network production` with `Accepted=True` and `SERVER GROUPS 1`,
- `servergroup lobby` in phase `Ready` with `READY 1` and `FREE SLOTS 100` —
  `READY` is `status.readyReplicas`, and since 2c a server does reach `Ready`,
  so unlike after 2b this is no longer `Pending 0`,
- a pod `lobby-xxxx` in `Running` with `READY 1/1` — the readiness probe spoke
  a real server list ping to a real Paper process,
- a `server lobby-xxxx` in phase `Ready` with `SLOTS 100`, `PLAYERS 0` and
  `REGISTERED true`. `SLOTS` is what the agent reported from
  `SPAWNERY_MAX_PLAYERS`, `PLAYERS` what it counted on the running server —
  zero, because nobody can join yet.

If the `Server` stops in `Starting` instead, the agent cannot reach the
operator: `kubectl logs` on the pod shows the reason, and it has so far always
been the `Service`/`Endpoints` pair above, not the agent.

Leaving it running for a quarter of an hour shows the other half of what the
agent is for. The session renews after eight minutes
(`--agent-session-renew-after`), and if the replacement stream did not overlap
the outgoing one, the server would drop out of `Ready` on that rhythm. Measured
over thirteen minutes:

```bash
nix develop -c kubectl get server lobby-xxxx -n minecraft \
  -o jsonpath='{.status.readinessLosses} {.status.readySince} {.status.playersUpdatedAt}'
# 0 2026-08-09T22:37:36Z 2026-08-09T22:49:57Z
```

`readinessLosses` still zero and `readySince` still the original timestamp,
while `playersUpdatedAt` keeps moving — the renewal happened and cost the
server nothing.

Afterwards, clean up:

```bash
kill %1
podman rm -f spawnery-relay
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind delete cluster --name spawnery-dev
```
