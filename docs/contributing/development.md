# Development

Everything builds through Nix. `nix develop` puts Go, `controller-gen`, the
envtest assets, `kubectl`, `kind`, `k3d`, `protoc` with its Go and Java
plugins, a JDK 21 and Gradle on the path at the versions this repository
pins, so nothing here depends on what is installed on the machine.

```bash
nix develop
make test              # unit and envtest tests
make build             # bin/spawnery-operator
make agent             # both agent plugins and their JUnit suites
make e2e               # the driven run: the operator in a real kind cluster
```

## The targets

The first six are the commit loop and run anywhere. Everything from
`agent-test` down needs a container runtime and only works on `x86_64-linux` —
pass `CONTAINER=podman` if `docker` is not your runtime. Three reach the network
and are therefore part of no other target, not even `make all`: `agent-deps`,
`publish` and `publish-chart`.

| Target | What it does |
|---|---|
| `make test` | Unit and envtest tests, after `manifests`, `generate`, `fmt`, `vet`, `chart-lint` and `toolchain-lint` |
| `make lint` | `golangci-lint` — `errcheck` and `staticcheck`, uncapped |
| `make build` | `bin/spawnery-operator` |
| `make manifests` | CRDs, RBAC, the chart templates, and the generated `docs/reference` pages |
| `make proto` | Go code under `internal/agentpb` from the `.proto` |
| `make agent` | Both agent plugins, with their JUnit suites as the check phase |
| `make docs` | The site, built through Nix — `mkdocs build --strict` inside it is the project's only link checker |
| `make docs-assets` | Vendors `docs/assets/mermaid.min.js` and builds `docs/plugin-api/javadoc/` — both gitignored, needed once per checkout before `mkdocs serve`; the Javadoc build costs about 34s cold |
| `make docs-serve` | `mkdocs serve` for writing, after `docs-assets` |
| `make agent-deps` | Regenerates `agent/deps.json`. Reaches Maven Central — part of no other target |
| `make agent-test` | Both real images against the stub operator in `cmd/spawnery-stubop` |
| `make paper-pin` | Computes the Paper pin; `paper-pin-check` fails if `nix/paper.nix` is behind |
| `make image` | The Paper base image (`image-load`, `image-test` follow it) |
| `make purpur-image` | The Purpur base image — the backend image going forward; `purpur-image-load`, `purpur-image-test` |
| `make velocity-image` | The Velocity image, same three steps scoped to it alone |
| `make operator-image` | The operator's own image, same three steps |
| `make image-repro` | Builds each image twice and fails if the bytes differ |
| `make publish` | Copies the images to `ghcr.io/spawnery/` with `skopeo` |
| `make publish-chart` | Pushes the Helm chart to `oci://ghcr.io/spawnery/charts` |
| `make e2e` | Builds a kind cluster, installs the chart, drives twenty scenarios |
| `make all` | `proto manifests generate fmt vet test build agent` |

## Generated code

`make proto` regenerates the Go code under `internal/agentpb` from
`proto/spawnery/agent/v1alpha1/agent.proto`. It is checked in like
`zz_generated.deepcopy.go`, and `make test` does not regenerate it: after a
change to the `.proto`, run `make proto` and commit the diff with it.

## The agent plugins

`make agent` (`nix build .#agents`) builds Paper's plugin and Velocity's — they
share the session loop, token source and channel construction in `agent/common`
— and runs both JUnit suites as the derivations' check phases.

`agent/deps.json` is the checked-in lockfile pinning every Maven artifact by
hash across the Gradle subprojects. `make agent-deps` regenerates it, and is
needed only when a `build.gradle.kts` under `agent/` changes a dependency: it
reaches Maven Central, and a Nix build must never depend on the network.

`make agent-test` runs both real images against the Go stub operator in
`cmd/spawnery-stubop` and checks the handshake, the authorization header, the
player reports, the overlapping renewal and the bound on a session the operator
never answers — and, for the Velocity image, that its readiness port stays
closed until a server list has arrived and opens once one does. It needs a
container runtime and only works on `x86_64-linux`.

## The images

`make image-test` runs all three game images — Paper, Purpur and Velocity —
offline under the same constraints the podspec imposes, loading each first so
the target needs no separate build step of its own. `make purpur-image` and
`make velocity-image`, with their own `-load` and `-test` siblings, scope that
same build/load/test triple to one image, for when a change is known to touch
nothing on the others.

**Purpur goes through `hack/image-test.sh` unchanged**, the same script the
Paper image does: the script asserts on Paper's behaviour — that Paper rewrote
`/data/config/paper-global.yml`, that the agent plugin loaded, that nothing was
downloaded at start — and Purpur is a Paper fork that does all of it. If the two
ever diverge enough for that to stop being true, that run is what fails and says
so.

Purpur is the backend image going forward and the Paper image is deprecated.
Both are built, tested and published; see the
[release notes](../archive/release-notes.md#0215-purpur-is-the-backend-image-and-paper-is-deprecated)
for what an installation does about it and `nix/paper-image.nix` for why
nothing is being removed.

`make agent-test` still drives the **Paper** image, and that is not an
oversight: what it exercises is the agent, and the agent jar in the two images
is the same file. `make image-test` covers the Purpur side, booting that image
and asserting the plugin loaded and its classes linked.

### Reproducibility

`make image-repro` is the standing check that the images are reproducible, worth
running again after any change to `nix/paper.nix` or `nix/paper-image.nix`.

The plain build in front of each `--rebuild` is not redundant. `--rebuild`
compares a fresh build against the output already in the store, and with
nothing there it does not fail the check, it declines to run it — "some outputs
… are not valid, so checking is not possible". All three image derivations take
the working tree as their source: appending one line to a file in `docs/` was
measured to change the derivation hash of `paper-image`, `velocity-image` and
`operator-image` alike (`agents` was unaffected). So an edit almost anywhere
empties the store of them.

### The operator's image

`make operator-image` builds the operator's own image, `make operator-image-load`
hands it to the local container runtime, and `make operator-image-test` runs it
under the constraints `charts/spawnery/templates/deployment.yaml` imposes —
non-root and a read-only root filesystem — rather than more comfortable ones,
plus `--network none`, which is the script's own choice and not the
Deployment's, and cheap here because the run only asks the binary to print its
usage. `make image-repro` covers this image beside the other two and the agent
jars.

## Publishing

Five artefacts, three scripts, all of which contact a registry or Maven Central
and are therefore part of no other target.

`make publish` (`hack/publish.sh`) copies the three images from their Nix
archives straight to `ghcr.io/spawnery/` with `skopeo`, so what reaches the
registry is what the flake describes rather than what a previous `podman load`
left in a local store. It needs a GitHub token with `write:packages`.
`DRY_RUN=1` still builds every image it was asked for — on a machine without
them cached that is the expensive part — then prints what it would copy where,
needing no credential. `FORCE=1` overwrites a tag that already exists, which it
otherwise refuses to do with exit 3. `WRITE_DIGEST=1` writes the digest `skopeo
copy` reported into `charts/spawnery/values.yaml`'s `image.digest` key — the
chart is the only installation form, so the only place a digest means anything.

`make publish IMAGES=operator-image` publishes one image rather than all three,
and that is the ordinary case: `flake.nix` keeps `operatorVersion` apart from
`imageVersion`, so after a reconciler fix exactly one tag is new. Asking for all
three stops, correctly, at the first tag already published and never reaches the
one that changed, and `FORCE=1` would get past that only by re-pushing about
1.4 GB over tags that were already right.
`.github/workflows/release.yml` invokes the script once per image on a `v*`
tag, which is what lets a release move one version and not the others.

`make publish-chart` (`hack/publish-chart.sh`) pushes the chart to
`oci://ghcr.io/spawnery/charts/spawnery`. It packages from `git archive HEAD`
and not from the working tree, because on the release runner those two stop
being the same file: `WRITE_DIGEST=1` rewrites `charts/spawnery/values.yaml` in
place minutes before the chart step runs, and archiving `HEAD` makes the
ordering irrelevant instead of a comment somebody has to keep obeying. It also
refuses, with no `FORCE=1` escape, to publish a chart whose *committed*
`image.digest` is non-empty — the one state nothing else catches, because
`internal/rbacaudit`'s `TestTheOperatorImageIsNotAMutableTag` returns early when
a digest is set instead of failing. Its "already there" refusal is exit 3 too,
and here that is the ordinary outcome: most tags change nothing under `charts/`.
`make publish-chart-test` drives nine cases past it, none against a registry.

`hack/publish-api.sh` publishes the only artefact that is not a container,
`cloud.spawnery:spawnery-api` on Maven Central, so a plugin can compile against
the API without a checkout. It is a script rather than a Gradle plugin because
every plugin that speaks the Central Portal's HTTP API is third-party and would
enter `agent/deps.json`, a cost every build of this repository would pay forever
to save one `curl`. **Two of its inputs are secrets nobody can grant from inside
a workflow**: a Central Portal token pair and an
ASCII-armoured signing key, both belonging to a person. `release.yml` therefore
skips this step rather than failing it when they are absent, and says which
artefact it left out — a hard failure would make every image and the chart
hostage to a secret that has nothing to do with them. `DRY_RUN=1` builds the
bundle, prints what would go where, and needs neither.

A tag whose whole change is under `charts/` publishes a chart and no image, and
is a correct release. **Both image numbers therefore have gaps, and none is a
miscount**: `imageVersion` reads `0.2.5, 0.2.7, 0.2.9, 0.2.10, 0.2.12, 0.2.13`,
`operatorVersion` reads `…, 0.2.9, 0.2.11, 0.2.12`, and the chart's own
`version` tracks neither — it moves whenever anything under `charts/` does,
while its `appVersion` stays with the operator it deploys. A missing number is
the record of a release that built nothing on that side; giving an artefact a
number from a release it was not in would be the lie. A local `make publish` is
for the case a tag cannot cover.

## The end-to-end run

`make e2e` (`hack/e2e.sh`) installs `charts/spawnery` with `helm install
--create-namespace` — which is also where the CRDs come from, so there is no
separate apply — into a namespace, `platform-system`, that shares nothing with
the chart's own documented default, `spawnery-system`. The Go test package drives
the operator under its own ServiceAccount, then reads its whole log and fails on
`is forbidden:`. The operator runs *in* the cluster here, from its own image, so
nothing hand-builds a `Service` — the difference between this and the local flow
below. On a machine where `kind` runs under rootless Podman, the invocation is:

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
```

`E2E_KEEP=1` leaves the cluster standing afterwards and prints its
`KUBECONFIG`; a failed run dumps the operator log, the objects and the events
before tearing down.

Running this image accepts
[Mojang's EULA](https://www.minecraft.net/eula) on your behalf: the entrypoint
writes `eula=true`, because Paper does not start otherwise.

## Trying it locally against kind

This is the hand-driven flow, and it runs the operator **outside** the cluster
through `go run`. `make e2e` above needs none of the workarounds below — its
`Service` has a selector, because there is a pod for it to select. What this
flow gives that `make e2e` does not is a real Paper image, a server that reaches
`Ready`, and an agent that reports players.

Under rootless Podman (measured with 5.8.4) `k3d` cannot bring up a cluster at
all: its tools node always bind-mounts the runtime socket to
`/var/run/docker.sock` inside itself, and rootless Podman refuses to create that
mount point (`mkdir /var/run/docker.sock: permission denied`) — no `DOCKER_HOST`
value fixes it, since the failure is in the tools node's own container creation,
not in the client reaching the socket. `kind` under
`KIND_EXPERIMENTAL_PROVIDER=podman` does work against the same rootless socket,
and is what the flow below uses. Anyone with a real Docker daemon (or a rootful
Podman socket) can use `k3d` the same way instead — the manifests and the
operator invocation are identical either way.

kind additionally needs cgroup delegation to run under systemd as a regular
user, hence the `systemd-run --scope --user --property=Delegate=yes` wrapper
around every kind command below: without it kind refuses with a `Delegate=yes`
error even when that property is already set on the user's systemd service —
the scope is what its check actually looks for.

The operator runs through `go run` outside the cluster, so without
`POD_NAMESPACE` from the downward API. `--operator-namespace` therefore has to
be set explicitly; without the flag the process refuses to start (see
`validateAgentFlags`), because the serving certificate would otherwise carry the
wrong SANs.

One gap this flow has to close by hand is the difference between a `Server` that
reaches `Ready` and one that does not. The pod dials
`spawnery-operator.<ns>.svc:9443` and nothing creates that Service, since the
operator is not in the cluster and no selector could find it. A selector-less
`Service` with a hand-written `Endpoints` pointing at the host closes it — the
serving certificate already carries that DNS name, so TLS verifies against the
CA the pod was given.

Which address goes into those `Endpoints` depends on the runtime. With a real
Docker daemon it is the bridge gateway, `172.17.0.1`. Under rootless Podman it
is none of the obvious candidates: the gateway of the `kind` network
(`10.89.0.1` here) lives inside the rootless network namespace, where the
operator is not listening and a connection is refused, and the one address that
does reach the host — the pasta link-local `169.254.1.2`, which Podman also
publishes as `host.containers.internal` — is rejected by the API server in both
`Endpoints` and `EndpointSlice` with `may not be in the link-local range`. What
works, measured, is one more container on the same Podman network relaying to
the host: it gets a routable address on that network, and pods reach it.

```bash
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind create cluster --name spawnery-dev
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind load docker-image ghcr.io/spawnery/paper:26.2-0.2.5 --name spawnery-dev
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
# --leader-elect=false is not needed to *start*, but it is what you want here:
# with leader election on, a local run contends for the same lease as an
# operator already installed in that namespace and waits it out first.
nix develop -c go run ./cmd/spawnery-operator --leader-elect=false --operator-namespace minecraft &

# Rootless Podman only. With a real Docker daemon, skip this and use
# 172.17.0.1 as the endpoint address below.
podman run -d --name spawnery-relay --network kind \
  -v /nix/store:/nix/store:ro \
  --entrypoint "$(nix build --no-link --print-out-paths nixpkgs#socat)/bin/socat" \
  ghcr.io/spawnery/paper:26.2-0.2.5 \
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
its own wait: at 26.2-0.2.1 the Paper image is 372 MB as a tarball and the
Velocity one 170 MB. They were 735 MB and 533 MB until 2026-08-25, when both
stopped shipping a whole headless JDK and started shipping a runtime jlink'd
to the modules each actually resolves — see `nix/paper-jre.nix` and
`nix/velocity-jre.nix`.

Expected, as measured on 2026-08-10 against `kind` v1.36.1 under rootless
Podman:

- `network production` with `Accepted=True` and `SERVER GROUPS 1`,
- `servergroup lobby` in phase `Ready` with `READY 1` and `FREE SLOTS 100` —
  `READY` is `status.readyReplicas`,
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
