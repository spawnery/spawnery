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
the base images along with the Kotlin agent (milestone 2b) — without an image a
pod stays stuck in `ErrImagePull`, however well the channel waiting for it has
been proven.

Details of what 2a deliberately leaves open — CA rotation, the Kotlin agent's
obligation to reconnect with overlap, the missing `spawnery-proxy` ServiceAccount
— are in [`docs/known-issues.md`](docs/known-issues.md).

The design lives under [`docs/superpowers/specs/`](docs/superpowers/specs/), the
plans under [`docs/superpowers/plans/`](docs/superpowers/plans/).

Anyone starting milestone 2b begins at
[`docs/handover-milestone-2b.md`](docs/handover-milestone-2b.md): it says what
the channel from 2a provides to an agent, which paths and binaries a base image
has to bring along, and what the development environment additionally needs for
it.

## Development

```bash
nix develop            # Go, controller-gen, envtest assets, kubectl, k3d
make test              # unit and envtest tests
make build             # bin/spawnery-operator
```

`make proto` regenerates the Go code under `internal/agentpb` from
`proto/spawnery/agent/v1alpha1/agent.proto`. The generated code is checked in
like `zz_generated.deepcopy.go` — after a change to the `.proto`, run `make
proto` and commit the diff with it; `make test` does not regenerate it on its
own.

### Trying it locally against k3d

These steps need a container runtime (Docker or Podman) for k3d. The development
environment milestone 1 was built in had none — which is why this flow has **not**
been run automatically in CI or anywhere else. The wiring and the state machine
were instead proven with a real, running manager against the envtest control
plane (see `internal/controller/setup_test.go`); what is only observable with a
real kubelet — that the pod hangs in `ErrImagePull` for want of a base image —
is unproven.

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
nix develop -c k3d cluster create spawnery-dev --agents 1
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
nix develop -c go run ./cmd/spawnery-operator --leader-elect=false --operator-namespace minecraft &
sleep 45
nix develop -c kubectl get networks,servergroups,servers,pods -n minecraft
```

The first server can take a good half minute to appear: if the ServerGroup meets
its network before the Network controller has accepted it, it tries again only
after `networkRetryInterval` (30 seconds). Hence the 45 seconds above.

Expected:

- `network production` with `Accepted=True`,
- `servergroup lobby` with `REPLICAS 1`,
- a `server lobby-xxxx` in phase `Pending` or `Starting`,
- a pod `lobby-xxxx` that cannot pull its image (`ErrImagePull`) — the base image
  arrives in milestone 2b. That is still the expected end state, milestone 2a
  included: the agent channel stays unused here (see above), so it changes
  nothing about the pod never starting.

Afterwards, clean up:

```bash
kill %1
nix develop -c k3d cluster delete spawnery-dev
```
