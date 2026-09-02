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
| `make manifests` | CRDs, RBAC, and the chart templates generated from them |
| `make proto` | Go code under `internal/agentpb` from the `.proto` |
| `make agent` | Both agent plugins, with their JUnit suites as the check phase |
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
| `make e2e` | Builds a kind cluster, installs the chart, drives eighteen scenarios |
| `make all` | `proto manifests generate fmt vet test build agent` |

The rest of this page is what each of those is worth knowing about beyond its
one-line summary.

## Generated code

`make proto` regenerates the Go code under `internal/agentpb` from
`proto/spawnery/agent/v1alpha1/agent.proto`. The generated code is checked in
like `zz_generated.deepcopy.go` — after a change to the `.proto`, run `make
proto` and commit the diff with it; `make test` does not regenerate it on its
own.

## The agent plugins

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

## The images

`make image` builds the Paper base image, `make image-load` hands it to the
local container runtime, and `make image-test` runs all three game images —
Paper, Purpur and Velocity — offline under the same constraints the podspec
imposes, loading each first so the target needs no separate build step of its
own. All of them need Docker or Podman and only work on `x86_64-linux`. Pass
`CONTAINER=podman` if `docker` is not your runtime. `make purpur-image` and
`make velocity-image`, with their own `-load` and `-test` siblings, are the
same three steps scoped to one image, for when a change is known to touch
nothing on the others.

**Purpur goes through `hack/image-test.sh` unchanged**, the same script the
Paper image does. It asserts on Paper's behaviour — that Paper rewrote
`/data/config/paper-global.yml`, that the agent plugin loaded, that nothing was
downloaded at start — and Purpur is a Paper fork that does all of it. A second
script would have been the same script with a different name; if the two ever
diverge enough for that to stop being true, that run is what fails and says so.

Purpur is the backend image going forward and the Paper image is deprecated.
Both are built, tested and published; see [`upgrading.md`](upgrading.md) for
what an installation does about it and `nix/paper-image.nix` for why nothing is
being removed.

`make agent-test` still drives the **Paper** image, and that is not an
oversight. What it exercises is the agent — a real gRPC session against
`cmd/spawnery-stubop`, TLS handshake and rotated CA bundle included — and the
agent jar in the two images is the same file. `make image-test` is what covers
the Purpur side of it: it boots that image and asserts the plugin loaded and
its classes linked. Doubling an expensive harness to run identical bytes twice
would buy nothing.

### Reproducibility

`make image-repro` builds each image and then rebuilds it with `nix build
--rebuild`, and fails if the two builds do not produce the same bytes. Design
§5.3 makes that reproducibility an acceptance criterion, not a one-off claim, so
this is the standing check for it — worth running again after any change to
`nix/paper.nix` or `nix/paper-image.nix`. Like `image-test`, it is not part of
`make test` or `make all`: it needs a build's worth of time and only runs on
`x86_64-linux`.

The plain build in front of each `--rebuild` is not redundant. `--rebuild`
compares a fresh build against the output already in the store, and with
nothing there it does not fail the check, it declines to run it — "some outputs
… are not valid, so checking is not possible". All three image derivations take
the working tree as their source: appending one line to a file in `docs/` was
measured to change the derivation hash of `paper-image`, `velocity-image` and
`operator-image` alike (`agents` was unaffected). So an edit almost anywhere
empties the store of them, and until milestone 6a's final fix wave this target
had nothing to check against on a tree anybody had touched.

### The operator's image

`make operator-image` builds the operator's own image, `make operator-image-load`
hands it to the local container runtime, and `make operator-image-test` runs it
under the constraints `charts/spawnery/templates/deployment.yaml` imposes —
non-root and a read-only root filesystem — rather than more comfortable ones, plus
`--network none`, which is the script's own choice and not the Deployment's,
and cheap here because the run only asks the binary to print its usage.
Since milestone 6a the operator is a container like the other two, and
`make image-repro` covers all three of them plus the agent jars. The whole
target was driven on 2026-08-17, after the milestone merged: all four
`--rebuild` comparisons — `paper.tar.gz`, `velocity.tar.gz`,
`spawnery-operator.tar.gz` and `spawnery-agents` — came back clean, exit 0.

## Publishing

`make publish` (`hack/publish.sh`) copies all three images from their Nix
archives straight to `ghcr.io/spawnery/` with `skopeo`, so what reaches the
registry is what the flake describes rather than what a previous
`podman load` left in a local store. It is part of no other target, because it
contacts a registry and needs a GitHub token with `write:packages`. `DRY_RUN=1`
still builds every image it was asked for — on a machine without them cached
that is the expensive part — and then prints what it would copy where instead
of copying it, so nothing reaches the registry and no credential is needed;
`FORCE=1` overwrites a tag that already exists, which it otherwise refuses to
do; `WRITE_DIGEST=1` writes the digest `skopeo copy` reported for the push it
just made into `charts/spawnery/values.yaml`'s `image.digest` key — the chart
is the only installation form since milestone 6d, and therefore the only
place a digest means anything.

`make publish IMAGES=operator-image` publishes one image rather than all three,
and that is the ordinary case rather than an escape hatch: `flake.nix` keeps
`operatorVersion` apart from `imageVersion` on purpose, so after a reconciler
fix exactly one of the three tags is new. Asking for all three then stops,
correctly, at the first tag that is already published, and never reaches the
one that changed — and `FORCE=1` would get past that only by re-pushing about
1.4 GB over tags that were already right.

Since milestone 6e, `.github/workflows/release.yml` does this on a `v*` tag,
and seventeen releases have been published that way — `v0.1.0` on 2026-08-20
through `v0.2.13` on 2026-08-30. It invokes the script once per image rather
than once for all three, which is what lets a release move one version and not
the others, and the six most recent have put that through both directions
repeatedly: `v0.2.6`, `v0.2.8` and `v0.2.11` bumped `operatorVersion` alone, so
the two game images were correctly refused at tags a cluster had already pulled
while the operator's push went through; `v0.2.7` and `v0.2.9` moved both,
because each changed the operator and the agents together; and `v0.2.10` moved
`imageVersion` alone, because its whole change was `image/entrypoint.sh`, which
ships in the game images and not in the operator.

`make publish-chart` (`hack/publish-chart.sh`) is the fourth artefact and the
newest: since `v0.2.14` the chart is pushed to
`oci://ghcr.io/spawnery/charts/spawnery`, so installing the operator needs no
checkout. Two things about it are worth knowing before reading the script.

It packages from `git archive HEAD` and not from the working tree, because on
the release runner those two stop being the same file: `WRITE_DIGEST=1` above
rewrites `charts/spawnery/values.yaml` in place, minutes before the chart step
runs. Archiving `HEAD` makes the ordering irrelevant instead of making it a
comment in `release.yml` that somebody has to keep obeying. It also refuses,
with no `FORCE=1` escape, to publish a chart whose *committed* `image.digest`
is non-empty — the one state nothing else here catches, because
`internal/rbacaudit`'s `TestTheOperatorImageIsNotAMutableTag` returns early
when a digest is set instead of failing.

Its "already there" refusal is exit 3, same as `hack/publish.sh`'s and for the
same reason, and in the chart's case that is the ordinary outcome rather than
a rare one: most tags change nothing under `charts/`. `make publish-chart-test`
drives nine cases past it — five against this repository, four against
throwaway git repositories built on the spot, none against a registry.

`hack/publish-api.sh` is the fifth artefact and the only one that is not a
container: `cloud.spawnery:spawnery-api` on Maven Central, so that a plugin can
compile against the API without a checkout and without a jar somebody carries
by hand.

It is a script rather than a Gradle plugin, and that is a decision about this
repository rather than a preference. The Central Portal takes one signed
archive over its own HTTP API instead of a Maven deploy, and every Gradle
plugin that speaks that API is a third-party plugin — which would enter
`agent/deps.json`, the lockfile that makes `nix build .#agents` reproducible.
The cost of the convenience would be paid by every build of this repository,
forever, to save one `curl`. Gradle's own `maven-publish` writes the repository
layout into `agent/api/build/staging-deploy` and its own `signing` puts a
`.asc` beside every file; what is left is to zip that tree and post it.

**Two of its inputs are secrets nobody can grant from inside a workflow**: a
Central Portal token pair and an ASCII-armoured signing key, both belonging to
a person. `release.yml` therefore skips this step rather than failing it when
they are absent, and says which artefact it left out — a hard failure would
make every image and the chart hostage to a secret that has nothing to do with
them. `DRY_RUN=1` builds the bundle, prints what would go where, and needs
neither.

Adding the chart also changed what "this tag releases nothing" means, which
`release.yml`'s guard now reflects: a tag whose whole change is under
`charts/` publishes a chart and no image, and is a correct release. Before
`v0.2.14` that combination failed the run.

**It changed that in two places, and the first attempt at `v0.2.14` only found
one of them.** `release.yml` has a preflight step, "The tag, against the
versions in flake.nix", whose whole premise is that a tag must name a version
some artefact carries — and it knew about three images. `v0.2.14` names the
chart's version and no image's, so it was refused with "names a version no
image in flake.nix carries", correctly by that step's old premise and wrongly
by the release's new one. The fix is the same sentence the other guard
already had: the chart is an artefact, so `Chart.yaml`'s version is a third
number a tag may name. Nothing was published on that attempt — the step runs
before any credential is used, which is what a preflight is for.

**Both numbers therefore have gaps, and none of them is a miscount.**
`imageVersion` reads `0.2.5, 0.2.7, 0.2.9, 0.2.10, 0.2.12, 0.2.13`;
`operatorVersion` reads `…, 0.2.9, 0.2.11, 0.2.12`. The chart's own `version`
tracks neither: it moves whenever anything under `charts/` does, which
`v0.2.13` did through a CRD field's description alone, while its `appVersion`
stayed with the operator it deploys. A missing number is the record of a release that built
nothing on that side, and giving an artefact a number from a release it was not
in would be the lie. A local `make publish` is for the case a tag cannot
cover.

## The end-to-end run

`make e2e` (`hack/e2e.sh`) is the driven end-to-end run: it builds the operator
image, creates a `kind` cluster, loads the image into it, and installs
`charts/spawnery` with `helm install --create-namespace` — which is also where
the CRDs come from now, so there is no separate apply for them — into a
namespace, `platform-system`, that shares nothing with the chart's own
documented default, `spawnery-system`. It then runs a Go test package that
drives the operator through eighteen ordered scenarios under its own
ServiceAccount before reading its whole log and failing on `is forbidden:`.
The operator runs *in* the cluster here, from its own image, so nothing
hand-builds a `Service` — which is the difference between this and the local
flow below. It is part of neither
`make test` nor `make all`: it builds a cluster and takes minutes, and the
commit loop stays where it is. Like the image targets it needs a container
runtime and only works on `x86_64-linux`. On a machine where `kind` runs under
rootless Podman, the invocation is:

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

This section is the hand-driven flow, and it runs the operator **outside** the
cluster through `go run`. If what you want is the operator running inside a
cluster, `make e2e` above does the whole thing automatically and needs none of
the workarounds below — the `Service` there has a selector, because there is a
pod for it to select. What this flow still gives you that `make e2e` does not
is a real Paper image, a server that reaches `Ready`, and an agent that reports
players.

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
  nix develop -c kind load docker-image ghcr.io/spawnery/paper:26.2-0.2.5 --name spawnery-dev
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
# --leader-elect=false is no longer needed to *start* -- the lease goes in
# --operator-namespace since 2026-08-24, so a local run no longer looks for a
# ServiceAccount mount it does not have. It is still what you want here: with
# leader election on, a local run contends for the same lease as an operator
# already installed in that namespace and waits out its lease before doing
# anything.
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
