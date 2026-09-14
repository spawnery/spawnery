# Handover to milestone 2c

Status: end of milestone 2b, the Paper base image (2026-08-08), merge commit
`af26233`.

This document is not a spec. It says where 2b stopped and what 2c already finds
in place. The design decisions live in
`superpowers/specs/2026-08-08-paper-base-image-design.md`, the open points in
`known-issues.md`.

## Where we are

A `Server` pod runs a real Paper process. The readiness probe speaks a real
server list ping and turns green, and the `Server` then stops in phase
`Starting` — because the two-stage ready gate wants a second signal, and nothing
sends it.

That second signal is the whole of milestone 2c: the Paper agent, a Kotlin
plugin that opens a `ServerSession` on the channel milestone 2a built and
reports its readiness and its player counts.

Everything else for a player to connect belongs to milestone 3: the Velocity
image, `ProxySession`, registration and forwarding.

## What 2c covers

- **The Paper agent** as a Kotlin plugin serving the channel from 2a.
- **Its place in the image**, which is not free — see below.
- **A JVM toolchain in `flake.nix`**, which does not exist yet.

## The contract 2c builds against

**The `.proto` is frozen.** `proto/spawnery/agent/v1alpha1/agent.proto` carries
both directions, including the proxy messages milestone 3 will need. Do not
change field numbers. The generated Go is checked in under `internal/agentpb`
and `make proto` regenerates it; the Kotlin side generates its own from the same
file.

**Three obligations on the agent** are described at length in `known-issues.md`,
section "Preconditions for milestone 2c". They are named here only so planning
does not lose them:

1. reconnect with overlap, before `renewAfterSeconds` expires,
2. set the `Bearer ` header character for character,
3. `Hello{ready:false}` does not lower a readiness once reported.

**The gRPC libraries must be shaded and relocated.** Protobuf classpath
conflicts are a documented trap in Paper plugins (main design, section 5.2), and
Paper's own bundle already ships `protobuf-java` — you can see it in the image
under `/opt/paper/repo/libraries/com/google/protobuf/`. An unrelocated plugin
will meet it.

**What the pod provides** (from `internal/podspec`, unchanged by 2b):

| | |
|---|---|
| Token | `/var/run/spawnery/token`, audience `spawnery-operator`, 600 s, rotated by the kubelet |
| CA | `/var/run/spawnery/ca.crt` — validate against this and nothing else |
| Endpoint | `SPAWNERY_OPERATOR_ENDPOINT` |
| Context | `SPAWNERY_NETWORK`, `SPAWNERY_GROUP`, `SPAWNERY_SERVER`, `SPAWNERY_MAX_PLAYERS` |

## What the image provides

Built by `nix/paper-image.nix`, tagged `ghcr.io/spawnery/paper:26.2-0.1.0`:

| Path | |
|---|---|
| `/opt/paper/paper.jar` | the paperclip launcher |
| `/opt/paper/repo` | `versions/`, `libraries/`, `cache/` — pre-patched at build time, read-only |
| `/usr/local/bin/spawnery-slp` | the readiness probe's tool |
| `/usr/local/bin/spawnery-entrypoint` | EULA, three enforced fields, `exec java` |

The Paper API the server actually runs against ships inside the image as
`/opt/paper/repo/libraries/io/papermc/paper/paper-api/26.2.build.111-stable/`.
That is the version the plugin must compile against; resolve the Maven
coordinate for it rather than assuming one.

## The one thing to settle before writing code

**The plugin has nowhere to live yet, and the obvious answer is wrong.**

At container start `/data` is an empty `emptyDir` and `/opt/paper` is
read-only. There is no `plugins/` directory anywhere, and the entrypoint's
`exec java` line is a fixed argv ending in `-jar "$PAPER_HOME/paper.jar"
--nogui` — no `--plugins`.

The obvious move is to bake a `plugins/` directory into the image beside
`repo/` and point `--plugins` at it. That breaks the server. Paper's plugin
*data* folders live inside the plugins directory — measured: a plain run writes
`/data/plugins/spark/config.json` and `/data/plugins/bStats/config.yml` — so a
read-only plugins directory takes Paper's own bundled plugins down with it.

Whatever 2c chooses has to keep the plugins directory writable while the agent
jar itself comes from the image. Copying or linking the jar out of a read-only
path into `/data/plugins/` in the entrypoint is the shape that satisfies both,
but that is a decision for 2c's design, not a conclusion this document gets to
make. What it does get to say is: decide it deliberately, not against a
crash-looping pod.

## The harder problem: building the plugin reproducibly

2b's reproducibility came cheaply. Two jars, two checked-in hashes, `fetchurl`,
done — and `make image-repro` proves it stays true.

A Kotlin plugin does not come cheaply. Gradle resolves a dependency tree at
build time, which is exactly what a Nix sandbox forbids, and the main design's
section 5 still demands artifacts checked against hashes that are checked in.
Reconciling those two is the real work of 2c's first planning step, not an
afterthought — and it is worth settling before any Kotlin is written, because
the answer shapes where the build lives.

## The environment

```bash
nix develop        # Go, controller-gen, protoc, envtest assets, kubectl, kind, k3d
make test          # must be green before anything is touched
```

**There is no JVM toolchain in the dev shell.** No JDK, no Gradle, no Kotlin.
The handover into 2b predicted this would be 2b's first planning step; it turned
out 2b never needed one, because the image build brings its own JDK as a
derivation input. 2c does need one, and it is genuinely the first step now.

**A container runtime is required** for the image targets, and one specific
thing is worth knowing before losing an hour to it: `k3d` does not work against
a rootless Podman socket at all. `kind` does, wrapped in `systemd-run --scope
--user --property=Delegate=yes`. The README documents the working flow;
`known-issues.md` records why.

**The image builds only on `x86_64-linux`.** The flake omits `packages.paper-image`
elsewhere on purpose, so an arm64 build cannot be mislabelled as amd64.
`make test` runs everywhere.

New since 2b:

| | |
|---|---|
| `make image` | build the image |
| `make image-load` | hand it to the local container runtime |
| `make image-test` | run it offline under the pod spec's own constraints |
| `make image-repro` | build twice and compare — the reproducibility criterion, as a standing check |

## First step

2c has no spec. The route is the one 2a and 2b took: brainstorm first, then
write the spec and have it approved, then the plan, then the implementation.

Two questions the brainstorming should settle early, because everything else
hangs off them:

- **How does a Gradle build become reproducible inside Nix?** See above. This is
  the question that decides the shape of the milestone.
- **Where does the agent jar live and how does it reach a writable plugins
  directory?** See above. Small, but it touches the entrypoint, and the
  entrypoint is covered by tests that will need extending with it.
