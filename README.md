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
readiness all the way to phase `Ready`.

Milestone 2b is done: the Paper base image. `nix build .#paper-image` produces
a reproducible image holding Paper 26.2, a headless JDK 25 and `spawnery-slp`, the tool
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

Milestone 3a is done: the operator's proxy side. `ProxySession` joins
`ServerSession` in the fan-out instead of answering `Unimplemented`, the
bootstrap creates a `spawnery-proxy` ServiceAccount in every namespace it
touches alongside `spawnery-server`, and the orphan sweep no longer discards a
connected proxy's registry entry. None of it has an agent to drive it yet, so
this closes the operator's half of the contract without anything reaching
phase `Ready` for a proxy.

Milestone 3b is done: the Velocity image and configuration rendering.
`nix build .#velocity-image` produces a reproducible image holding Velocity
3.5.1 on the same JDK 25 and non-root base `nix/oci-common.nix` now shares
with the Paper image. `cmd/spawnery-config`, baked into both images, replaces
what the Paper entrypoint used to rewrite by hand: it resolves the operator's
rendered ConfigMap, a user overlay and a handful of fields neither may move
into `server.properties`, `config/paper-global.yml` and `velocity.toml`, and
refuses to start — naming the file and the key — rather than guess at
anything missing or malformed. `online-mode` is now `false` on the backends
and `true` on the proxy, which is what modern player-info forwarding actually
requires.

Milestone 3c is done: the Velocity agent. `nix build .#agents` produces both
shaded plugin jars — Paper's and Velocity's, sharing one session loop in
`agent/common` since this milestone's Gradle split. The Velocity plugin opens
an authenticated `ProxySession`, mirrors the operator's server list into
Velocity's own registry, binds its readiness port only once a server list has
arrived so a proxy pod cannot turn ready before it can route anyone, routes a
joining player through the group's `fallbackGroups` try-list, and moves
players off a backend the operator is draining onto whatever that try-list
offers next. A `ProxyGroup` now reaches phase `Ready` the way a `Server`
already did, and milestone 3's whole point — a player can join — has code
behind it.

Getting there found the milestone's one Critical defect, and it is worth
stating plainly rather than passed over: the paragraph above about milestone
3b claimed the rendered configuration was correct on disk, and that was not
true. `internal/render` wrote Paper's forwarding secret under the key
`secret-key`; Paper reads `secret`, silently ignores the one nobody asked
for, and disables forwarding in its own log — a line nothing had ever read.
Every backend this operator rendered came up healthy and refused every
forwarded join. It was found by the first real end-to-end join attempt, ten
tasks into this milestone, and no test until then could have caught it: every
render test asserts what the renderer wrote, none of them asked what Paper
read back. Both are now checked. `docs/known-issues.md`'s "From milestone
3c" section has the rest of what this milestone leaves open, in particular
that a NetworkPolicy restricting backends to proxies-only is now overdue
rather than deferred, and that proxy drain needs a readiness
`internal/agent/registry.go` cannot yet lower.

`make agent-test` runs both plugins, in the real images, against a real
operator-shaped gRPC server — including, for the proxy, that its ready port
stays closed until a server list arrives and opens once one does. The
cluster-level proof beyond that harness is written down as
[`docs/runbook-milestone-3-evidence.md`](docs/runbook-milestone-3-evidence.md),
and it has now been run once, against a real `kind` cluster (2026-08-12): an
automated join through `cmd/spawnery-join` reached a backend, and Paper's own
log and Velocity's own log confirmed it — the first time the forwarding chain
built in 3b and 3c was observed working end to end rather than merely
rendered correctly on disk. A manual join with a real Microsoft account still
needs a licensed client and a person to drive it, and has not been done. The
drain proof surfaced a real finding instead of a clean result: deleting a
`Server` under a player held open by the evidence tool disconnected them
rather than moving them, traced to the tool stopping short of the point Paper
counts a player as online rather than to the drain logic itself — full
diagnosis in [`docs/known-issues.md`](docs/known-issues.md), "From the
milestone 3c evidence run". Neither the manual join nor a real drain is
proven yet; both are what
[`docs/handover-milestone-4.md`](docs/handover-milestone-4.md) records as
still open, along with what running the automated half found.

Carry-overs and preconditions for later milestones — CA rotation, the
NetworkPolicy restricting backends to proxies-only that `online-mode=false`
now makes a real invariant rather than a deferred one, and what earlier
milestones leave open — are in
[`docs/known-issues.md`](docs/known-issues.md).

The design lives under [`docs/superpowers/specs/`](docs/superpowers/specs/), the
plans under [`docs/superpowers/plans/`](docs/superpowers/plans/).

Milestone 4a is done: slot-based scaling. An ephemeral `ServerGroup` no longer
sits at its floor. It creates servers as soon as its free player slots fall
below `spec.scaling.spareSlots`, bounded by `maxReplicas`, and removes them
again — one per pass, and only while the group's free slots would still cover
the spare — once a server has been empty for `scaleDownStabilizationSeconds`.
The rule is `DecideSize` in `internal/controller/scaling.go`, a pure function
beside `phase.Decide` and `SelectDeletionCandidates`, and the invariant those
already carried holds unchanged: a server that may be carrying players is
never nominated.

The one thing worth naming is what the scale-up rule reads. A server created
now is not `Ready` for tens of seconds and adds nothing to `status.freeSlots`,
so a scaler reading that figure would see the same shortfall on every
five-second pass and order the same replacement again, until `maxReplicas`
stopped it. It reads a second figure instead, one that credits capacity that
has been ordered and has not arrived. The two are deliberately not the same
number, and the envtest that carries this milestone is the one that keeps
reconciling for ten more passes and asserts the count has not moved — a single
decision cannot show that failure.

Milestone 4 continues with 4b, rolling updates of ephemeral groups, and 4c,
proxy and node drain, which owns the readiness
`internal/agent/registry.go` still cannot lower.

Anyone starting milestone 4 begins at
[`docs/handover-milestone-4.md`](docs/handover-milestone-4.md): it says where
3c stopped, the one milestone 2a contract change proxy drain needs in
`internal/agent/registry.go`, and what 3c leaves in place for milestone 4 to
build on. [`docs/handover-milestone-3.md`](docs/handover-milestone-3.md),
[`docs/handover-milestone-2c.md`](docs/handover-milestone-2c.md) and
[`docs/handover-milestone-2b.md`](docs/handover-milestone-2b.md) are its
predecessors, kept as the record of what those milestones started from.

## Development

```bash
nix develop            # Go, controller-gen, envtest assets, kubectl, kind, k3d,
                       # protoc with its Go and Java plugins, JDK 21, Gradle
make test              # unit and envtest tests
make build             # bin/spawnery-operator
make agent             # both agent plugins (Paper and Velocity) and their JUnit suites
```

`make proto` regenerates the Go code under `internal/agentpb` from
`proto/spawnery/agent/v1alpha1/agent.proto`. The generated code is checked in
like `zz_generated.deepcopy.go` — after a change to the `.proto`, run `make
proto` and commit the diff with it; `make test` does not regenerate it on its
own.

`make agent` builds both agent plugins (`nix build .#agents`) — Paper's and
Velocity's, sharing the session loop, token source and channel construction in
`agent/common` since milestone 3c's Gradle split — and runs both JUnit suites
as the derivations' check phases; it is the target to reach for after any
change under `agent/`. `make agent-deps` regenerates `agent/deps.json`, the
checked-in lockfile that pins every Maven artifact by hash across all three
Gradle subprojects; it is needed only when a `build.gradle.kts` under `agent/`
changes a dependency, and it is deliberately part of no other target, not even
`make all`, because it reaches Maven Central — a dependency change is an
explicit act and a Nix build must never depend on the network. `make
agent-test` runs both real images against
the Go stub operator in `cmd/spawnery-stubop` and checks the handshake, the
authorization header, the player reports, the overlapping renewal and the
bound on a session the operator never answers — and, for the Velocity image,
that its readiness port stays closed until a server list has arrived and
opens once one does; it is the target to run after any change to either
agent's session handling, and like the image targets below it needs a
container runtime and only works on `x86_64-linux`.

`make image` builds the Paper base image, `make image-load` hands it to the
local container runtime, and `make image-test` runs both the Paper and the
Velocity images offline under the same constraints the podspec imposes —
loading both first, so the target needs no separate `make velocity-image`
step of its own. All three need Docker or Podman and only work on
`x86_64-linux`. Pass `CONTAINER=podman` if `docker` is not your runtime.
`make velocity-image`, `make velocity-image-load` and `make
velocity-image-test` are the same three steps scoped to only the Velocity
image, for when a change is known to touch nothing on the Paper side.

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
