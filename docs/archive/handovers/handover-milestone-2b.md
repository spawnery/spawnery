# Handover to milestone 2b

Status: end of milestone 2a (2026-08-08), merge commit `f0705b2`.

This document is not a spec. It says where 2a stopped and what 2b already finds
in place when it starts — so the work can continue on another machine without
questions. The design decisions live in
`superpowers/specs/2026-08-08-agent-channel-design.md`, the open points in
`known-issues.md`.

## Where we are

The operator has a secured channel to the agents: gRPC over TLS on port 9443, a
serving certificate from a self-issued CA, identity from a pod-bound
ServiceAccount token. A `Server` reaches phase `Ready` once the readiness probe
is green **and** an agent has reported its readiness over that channel; player
counts arrive in the status in throttled form.

What is still missing before a player can connect: the base images (2b) and the
proxy layer (3). Without a base image the pod stays stuck in `ErrImagePull` —
that is the expected end state.

## What 2b covers

From the milestone table of the main design, minus what 2a has already done:

- **A base image for Paper**, versioned, with a reproducible build. No download
  at runtime; the Paper jar and the agent plugin are checked at build time
  against checked-in SHA-256 hashes that do **not** come from the same source as
  the artifact.
- **The SLP health tool** in the image, called by the readiness probe.
- **The Paper agent** as a Kotlin plugin serving the channel from 2a.

The Velocity image belongs to milestone 3, not here.

## The contract 2b builds against

**The `.proto` is frozen.** `proto/spawnery/agent/v1alpha1/agent.proto` contains
both directions, including the proxy messages from milestone 3. Do not change
field numbers — the generated Go code is checked into `internal/agentpb` and
`make proto` regenerates it.

**Three obligations on the agent** are described at length in `known-issues.md`,
section "Preconditions for milestone 2b"; they are only named here so they do not
get lost during planning:

1. reconnect with overlap, before `renewAfterSeconds` expires,
2. set the `Bearer ` header character for character,
3. `Hello{ready:false}` does not lower a readiness once reported.

**What the pod provides to the agent** (from `internal/podspec`):

| | |
|---|---|
| Token | `/var/run/spawnery/token`, audience `spawnery-operator`, 600 s, rotated by the kubelet |
| CA | `/var/run/spawnery/ca.crt` — validate against this and nothing else |
| Endpoint | Environment variable `SPAWNERY_OPERATOR_ENDPOINT` |
| Context | `SPAWNERY_NETWORK`, `SPAWNERY_GROUP`, `SPAWNERY_SERVER`, `SPAWNERY_MAX_PLAYERS` |

## What the base image has to satisfy

The podspec already wires all of this up today. An image that deviates from it
either does not start or never becomes ready:

- **`/usr/local/bin/spawnery-slp`** must exist and be executable. The readiness
  probe calls it as `spawnery-slp --host 127.0.0.1 --port 25565`, first after 20
  seconds, then every 5 seconds, with a 5-second timeout and three failures
  before it goes red. It has to speak a real server list ping — a port check
  would turn green before the world is loaded.
- **Port 25565**, TCP.
- **Working directory `/data`**, scratch under `/tmp`. Both are mounted;
  everything else in the filesystem is **read-only**
  (`readOnlyRootFilesystem`).
- **No root**: `runAsNonRoot: true`, all capabilities dropped, no privilege
  escalation, seccomp `RuntimeDefault`. The image therefore has to set a numeric
  user.
- **Do not plan for liveness probe behaviour** — there deliberately is none. A
  restart would kick every player on the server.
- User mounts must not nest inside `/var/run/spawnery`; `/data/config` is the
  documented pattern and stays allowed.

The gRPC libraries in the plugin **must be shaded and relocated** — protobuf
classpath conflicts are a known trap with Paper plugins (main design, section
5.2).

## The environment on the new machine

```bash
git clone git@github.com:spawnery/spawnery.git
cd spawnery
nix develop        # Go, controller-gen, protoc, envtest assets, kubectl, k3d
make test          # must be green before anything is touched
```

`nix develop` works on `x86_64-linux`, `aarch64-linux` and `aarch64-darwin`. On
Darwin the envtest binaries come from the controller-tools releases with a hash
pinned in `flake.nix`; a new Kubernetes version requires a new hash there.

**New for 2b: a container runtime is required.** Docker or Podman, for the image
build and for k3d. The machine 2a was built on had neither — which is why the
k3d flow in the README has to this day never been executed anywhere. Whoever
starts 2b should catch up on that first: the expected end state of 2a is a pod
in `ErrImagePull`, and that is exactly what 2b can use as its starting point.

For the Kotlin part a JVM toolchain is added (Gradle, JDK). It is not yet in
`flake.nix` and belongs in the first planning step of 2b.

## First step

2b has no spec yet. The route is the same as for 2a: brainstorm first, then
write the spec and have it approved, then the plan, then the implementation.

Two questions the brainstorming should settle early, because everything else
hangs off them:

- **One milestone or two?** The image build and the Kotlin plugin are two build
  worlds with little overlap, much as 2a and 2b were.
- **Nix or a Dockerfile for the image build?** The spec demands reproducibility
  and checked-in hashes; the project already uses flakes. Documented in the main
  design as a checked alternative: using `itzg` images pinned by digest as the
  base layer with the agent as a layer of its own on top.
