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
readiness all the way to phase `Ready` — but no real agent talks to it yet: a
player still cannot connect, because two things are missing: the Velocity proxy
layer (milestone 3; until then `ProxySession` only answers `Unimplemented`) and
the Kotlin agent (milestone 2c) — the base image milestone 2b needed no longer
stands in the way, but pod readiness is only half of the ready gate.

Milestone 2b is done: the Paper base image. `nix build .#paper-image` produces
a reproducible image holding Paper 26.2, a JRE 25 and `spawnery-slp`, the tool
the readiness probe calls to speak a real server list ping. Paper is patched at
build time, so a pod downloads nothing at startup; `make image-test` runs the
image offline to keep that true.

A pod now starts and its readiness probe turns green — and the `Server` stops
in phase `Starting`, because the second half of the ready gate wants an agent.
That agent is the Kotlin plugin from milestone 2c, and until it exists no
player can join: the Velocity proxy layer (milestone 3) is missing too.

Details of what 2a deliberately leaves open — CA rotation, the Kotlin agent's
obligation to reconnect with overlap, the missing `spawnery-proxy` ServiceAccount
— are in [`docs/known-issues.md`](docs/known-issues.md).

The design lives under [`docs/superpowers/specs/`](docs/superpowers/specs/), the
plans under [`docs/superpowers/plans/`](docs/superpowers/plans/).

Milestone 2b started from
[`docs/handover-milestone-2b.md`](docs/handover-milestone-2b.md): it says what
the channel from 2a provides to an agent, which paths and binaries a base image
has to bring along, and what the development environment additionally needs for
it.

## Development

```bash
nix develop            # Go, controller-gen, envtest assets, kubectl, kind, k3d
make test              # unit and envtest tests
make build             # bin/spawnery-operator
```

`make proto` regenerates the Go code under `internal/agentpb` from
`proto/spawnery/agent/v1alpha1/agent.proto`. The generated code is checked in
like `zz_generated.deepcopy.go` — after a change to the `.proto`, run `make
proto` and commit the diff with it; `make test` does not regenerate it on its
own.

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
wrong SANs. The agent channel itself stays unused in this flow: the operator does
bootstrap the CA ConfigMap and the `spawnery-server` ServiceAccount in the
namespace, but the pod dials `spawnery-operator.<ns>.svc:9443` — a service this
flow never creates, because the operator process does not run in the cluster and
there would be no endpoint pointing at it.

```bash
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind create cluster --name spawnery-dev
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind load docker-image ghcr.io/spawnery/paper:26.2-0.1.0 --name spawnery-dev
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
nix develop -c go run ./cmd/spawnery-operator --leader-elect=false --operator-namespace minecraft &
sleep 60
nix develop -c kubectl get networks,servergroups,servers,pods -n minecraft
```

The first server can take a good half minute to appear: if the ServerGroup
meets its network before the Network controller has accepted it, it tries
again only after `networkRetryInterval` (30 seconds). The 60 seconds above
also cover loading the 724 MB image into the cluster, which is not instant.

Expected:

- `network production` with `Accepted=True`,
- `servergroup lobby` in phase `Pending` with `READY 0` — that field is
  `status.readyReplicas`, servers that reached phase `Ready`, and none do at
  this milestone, so `Pending` is the correct end state and not a stall,
- a pod `lobby-xxxx` in `Running` with `READY 1/1` — the readiness probe spoke
  a real server list ping to a real Paper process,
- a `server lobby-xxxx` in phase `Starting`, staying there. That is the
  expected end state after milestone 2b: pod readiness is one half of the
  gate, and the other half waits for the agent from milestone 2c.

Afterwards, clean up:

```bash
kill %1
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind delete cluster --name spawnery-dev
```
