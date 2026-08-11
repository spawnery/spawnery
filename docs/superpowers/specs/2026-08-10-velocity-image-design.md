# Design: the Velocity image and configuration rendering (milestone 3b)

**Date:** 2026-08-10
**Status:** Draft for approval
**Scope:** Milestone 3b — the second image this repository builds, and the
configuration rendering that makes modern forwarding possible. Everything
3c needs before a Velocity agent can be written, and nothing about the agent
itself.

This document implements section 5.4 of
`2026-08-07-minecraft-cloud-operator-design.md` and the forwarding half of its
section 6.5. Where it departs from either, it says so and why.

## 1. Where 3b sits

Milestone 3 is cut into three (see
`2026-08-10-proxy-channel-design.md` section 1). 3a built the operator's proxy
side and is merged. 3b builds the image and the configuration; 3c builds the
Velocity agent and is the one that ends with a player joining.

3a and 3b are independent. 3c needs both.

The two preconditions `known-issues.md` reserved for this milestone are its
starting point: factor the shared image Nix while there is still exactly one
consumer, and do not extend `set_property` for the forwarding mode.

## 2. What is already in place

- **The Paper image and its build shape.** `nix/paper-image.nix` is a
  `dockerTools.buildLayeredImage` with layers ordered by rate of change, a
  numeric user, a rewritten entrypoint shebang, and `spawnery-slp` copied to
  the literal path `internal/podspec` names. `make image-test` runs it offline
  under the podspec's own constraints; `make image-repro` compares two builds
  byte for byte.
- **The `buildGoModule` path for a tool baked into an image.** `spawnery-slp`
  establishes it: pure Go, `CGO_ENABLED=0`, logic in `internal/slp` so it is
  testable without a container.
- **The proxy pod.** `podspec.BuildProxyPod` exists (3a) with its ports, its
  probe on the agent's readiness port, its projected agent volume, and
  `DefaultPlayerLimit`.
- **`ProxyGroup.spec.config`** already carries `playerLimit` and `motd`. It has
  never been rendered anywhere.
- **The forwarding secret exists as an API reference and nothing mounts it.**
  `Network.spec.forwardingSecretRef` names a Secret with the key `secret`.

## 3. Decisions

**Section 5.4 lands whole, not in part.** The operator renders one ConfigMap
per group; the pod mounts it; a program in the image merges it. The
alternative — env vars for the forwarding fields and nothing else — would have
left 5.4 unimplemented for a third milestone and would have put the rendering
question back on 3c, which has a Velocity agent to build.

**The rendered configuration is mounted read-only somewhere the process never
writes.** Paper writes `paper-global.yml` into `/data/config` itself, and a
ConfigMap volume is always read-only, so a mount there breaks the start —
`known-issues.md` has recorded that collision since 2b. The rendered
configuration therefore lands at `/etc/spawnery`, and a program in the image
reads it from there and writes the real files into `/data`. The collision does
not get resolved; it never arises.

Deliberately **not** under `/var/run/spawnery`: that is the agent's credential
mount, and `checkMountCollision` guards it with a bidirectional nesting check
it applies to nothing else. Keeping the two apart keeps that rule saying the
one thing it exists to say.

**Precedence is resolved in Go, not by mount order.** Section 5.4 promises
that user configuration outranks rendered defaults and is outranked by the
operationally critical fields. Three layers, applied in this order by one
program:

1. **Rendered defaults** — the operator's ConfigMap, in a neutral schema.
2. **The user overlay** — a partial file in the target's own dialect.
3. **Critical fields** — compiled into the renderer, overridable by nothing.

**The ConfigMap is neutral; the overlay is native.** The operator writes
`maxPlayers: 100`, not TOML and not YAML — it stays out of the business of two
config dialects, and adding a field later is one CRD field and one line in the
renderer. The overlay is the opposite: a partial `velocity.toml`,
`paper-global.yml` or `server.properties`, because its whole purpose is to
reach settings the API does not model. An overlay restricted to the neutral
schema could only set fields the CRD already sets, which would make it
redundant.

**The renderer takes `server.properties` too, and `set_property` is deleted.**
Leaving the properties file to the entrypoint would keep `maxPlayers` in two
places — an env var and a ConfigMap — which is the failure class this project
keeps finding. It would also keep the shell rewrite whose read-only failure
mode `known-issues.md` records: `grep -v … >tmp && mv tmp server.properties`
fails under `set -eu` with a bare `mv:` message that says nothing about why.
One program now owns every file the operator cares about.

`SPAWNERY_MAX_PLAYERS` leaves the pod contract with it. Only the entrypoint
ever read it — the Paper agent takes its `slots` from Bukkit, which takes them
from `server.properties` — so the value now travels one way.

**`online-mode` is inverted between the two layers, and that is the one thing
worth a paired test.** The proxy runs `online-mode = true` and authenticates
players with Mojang. The backends run `online-mode=false` and trust what the
proxy forwards. Set the wrong way round, everything starts, every test passes,
and anyone can connect directly to a backend under any name. It belongs in the
critical layer and in an assertion that checks both sides in one run.

Paper makes this harder than it sounds by using the same words twice:
`server.properties` gets `online-mode=false`, while `paper-global.yml` gets
`proxies.velocity.online-mode: true`, meaning "trust the online-mode
information Velocity forwards". Both are correct simultaneously. The renderer
writes both and the code says why.

**One renderer binary, two flavours.** The layering rules are identical for
Paper and Velocity; only the translation to the target dialect differs. Two
binaries would be two copies of the part that is expensive to get wrong.

## 4. Components

### 4.1 `nix/oci-common.nix`

Extracted before the second image exists, which is the whole point of doing it
now. It holds what both images need verbatim: the `passwd`/`group` pair for the
numeric user, the entrypoint shebang rewrite through
`substitute --replace-fail`, the copy of a Go binary into `/usr/local/bin` at
the literal path a pod spec names, and the layered-image configuration around
them.

The Paper image is rewritten onto it in the same change. `make image-repro`
proves the rewrite changed nothing: the Paper image must still build byte for
byte identically to itself.

### 4.2 `nix/velocity.nix` — the pin

Velocity is a fat jar and needs no build-time patching, so this is simpler than
`nix/paper.nix`: a `fetchurl` with a checked-in hash, and the version and build
numbers beside it.

The version, build number, URL and hash are **measured during implementation
and written down there**, not guessed here. `nix/paper.nix` records the same
discipline and the reason: a checked-in hash does not make the source
trustworthy, it makes the artifact frozen — a changed upstream breaks the build
instead of substituting a jar quietly.

### 4.3 The Velocity image

`nix/velocity-image.nix`, built on `oci-common.nix`, with the same layer
ordering rationale: the JRE and the Velocity jar are large and almost static;
the entrypoint, the renderer and (in 3c) the agent are small and change per
commit.

It carries a JRE, the Velocity jar, `spawnery-config`, and its own entrypoint.
It does **not** carry `spawnery-slp`: a proxy's readiness is the agent's ready
port, not a server list ping.

Exposed port 25565, working directory `/data`, user `10001:10001` — the same
numeric user as the Paper image, which is what `oci-common.nix` existing makes
true by construction rather than by two files agreeing.

### 4.4 `cmd/spawnery-config` and `internal/render`

The binary is a thin main; the logic lives in `internal/render` so it is
testable without a container, the way `internal/slp` is.

```
spawnery-config --flavor paper|velocity
```

Inputs, all read-only:

| Path | Source | Required |
|---|---|---|
| `/etc/spawnery/config.yaml` | the operator's rendered ConfigMap | yes |
| `/etc/spawnery/overlay/…` | the user's `spec.configOverlay`, if declared | no |
| `/etc/spawnery/forwarding.secret` | the `Network`'s Secret, key `secret` | yes |

Outputs, written into `/data` before the JVM starts:

- **paper**: `server.properties` and `config/paper-global.yml`
- **velocity**: `velocity.toml`

The overlay is a directory whose file names are the targets they merge into —
`server.properties`, `paper-global.yml`, `velocity.toml` — so one overlay
ConfigMap can carry a fragment for each file the flavour writes, and a fragment
for a file this flavour does not write is an error rather than a silent no-op.

**The forwarding secret is never copied.** Velocity's
`forwarding-secret-file` is pointed at `/etc/spawnery/forwarding.secret`
directly, so the secret never lands in a writable layer. Paper has no file
reference for it, so there the renderer does write it into
`paper-global.yml` — which is why that file is written into `/data` and not
somewhere a later `kubectl cp` would find it by accident.

### 4.5 The two entrypoints

Both shrink. Each validates nothing about configuration any more — that moved
into the renderer, which has the values and can name the key that is missing —
and each runs `spawnery-config` before `exec`ing the JVM.

`image/entrypoint.sh` keeps the EULA line, the agent-jar copy into a writable
plugins directory, and the JVM flags. It loses `set_property` and the
`SPAWNERY_MAX_PLAYERS` validation.

`image/velocity-entrypoint.sh` is new and is the smaller of the two: run the
renderer, copy the agent jar (3c ships one; 3b's copy step is written now and
copies nothing until then, exactly as the Paper entrypoint's was in 2b), exec
the JVM. `exec`, so the JVM is PID 1 and receives SIGTERM directly — a proxy
that does not get its signal drops every player on it without draining.

### 4.6 The operator side

**Rendering.** Both group controllers ensure one ConfigMap per group, named
after the group and its role and owned by the group, so deletion cascades.
The role is part of the name, not an afterthought: a `ServerGroup` and a
`ProxyGroup` are different Kinds, so Kubernetes allows them the same name in
one namespace, and a name that was only the group's own name would then
collide between the two controllers — or, just as easily, with a user's own
ConfigMap that happens to be named after their group, which the operator
would silently adopt. The ConfigMap carries only what a user can influence:

- ServerGroup: `maxPlayers`, which the CRD makes required — a `ServerGroup`
  cannot omit it.
- ProxyGroup: `playerLimit`, `motd`. Unlike `maxPlayers`, `playerLimit` is
  `+optional` on the CRD, and the controller defaults an unset one to
  `podspec.DefaultPlayerLimit` — the exact constant `BuildProxyPod` already
  defaults the pod's own `SPAWNERY_PLAYER_LIMIT` environment variable from.
  The two have to agree: a `ProxyGroup` that sets no `spec.config` still gets
  a workable ConfigMap this way, rather than one `render.Velocity` refuses to
  start from while the pod's own environment claims a limit already.

Critical fields are deliberately absent. They live in the renderer and nowhere
else — one truth per fact.

Two things that are easy to miss and expensive to find:

- **The ConfigMap must exist before the first pod of the group.** A pod whose
  projected volume names a ConfigMap that is not there does not start. The
  render step therefore runs in the same place `Bootstrapper.Ensure` already
  does, before any pod is created, and for the same reason.
- **It must carry `spawnery.cloud/managed-by`.** `cmd/spawnery-operator`
  narrows the manager's cache for ConfigMaps to objects with that label, so an
  unlabelled one is invisible to the controller that just wrote it — the exact
  trap `Bootstrapper.ensureConfigMap` documents at length. No new RBAC verb is
  needed: `configmaps` already carries `get;list;watch;create;update` for the
  CA bundle.

**The config volume.** A projected volume mounted read-only at `/etc/spawnery`
with three sources: the rendered ConfigMap, the `Network`'s forwarding secret,
and the overlay if declared. One volume, one mount path, added to both
`BuildServerPod` and `BuildProxyPod` in one change so the two layers cannot
drift into different answers about where configuration lives.

**`spec.configOverlay`** is added to both group specs: an optional reference to
a ConfigMap whose keys are target file names. A dedicated field rather than a
reserved name inside `mounts`, because `mounts` is documented as raw files for
plugins and worlds, and a name-based convention is invisible until someone
picks that name by accident.

ConfigMaps only, not Secrets. A configuration override that must stay secret is
a real need and this is not the milestone that establishes how — the forwarding
secret has its own path, and anything else waits for the layered template
system that project 3 owns.

## 5. Data flows

**A server pod starts.** The kubelet mounts `/etc/spawnery` read-only and
`/data` writable. The entrypoint writes `eula.txt`, copies the agent jar into
`/data/plugins`, and runs `spawnery-config --flavor paper`. The renderer reads
the three layers, resolves them, and writes `server.properties` and
`config/paper-global.yml` into `/data`. The entrypoint execs the JVM. Paper
reads what the renderer wrote, and writes its own further files into
`/data/config` — a directory nothing has mounted over.

**A proxy pod starts.** The same shape with `--flavor velocity`, writing
`velocity.toml`. `[servers]` is empty and `try` is empty: the Velocity agent
registers backends over the channel 3a built, and a static server list would be
a second truth about which servers exist.

**A configuration change.** The user edits `spec.config` or the overlay
ConfigMap; the controller re-renders. **The change takes effect on the next pod
restart**, consistent with the update-by-attrition model in section 4.4 of the
main design. Nothing reloads a running process, and nothing pretends to.

## 6. Error handling

The renderer refuses to start rather than continue, in every case below, and
names the file and key in its message:

- **The rendered ConfigMap is missing, or a required value is absent.** A Paper
  server that starts with the upstream default of 20 players while its group
  promises 100 makes the operator plan against capacity that server can never
  honour. That refusal exists in `image/entrypoint.sh` today against an env
  var; it moves to the renderer against its own input rather than disappearing.
- **The forwarding secret is missing or empty.** A backend with
  `online-mode=false` and no forwarding secret is joinable by anyone who can
  reach it.
- **The overlay does not parse, or names a file this flavour does not write.**
  An overlay that silently does nothing looks exactly like a configuration that
  did not take effect, which is the most expensive kind of wrong.

A refusal crash-loops the pod, and the Server state machine fails it after the
startup deadline. That is the loud path and it is the one wanted here: a proxy
layer that comes up with the wrong forwarding configuration is worse than one
that does not come up.

## 7. What 3b deliberately does not do

- **No Velocity agent.** 3c. The entrypoint's agent-jar copy is written now and
  copies nothing until then.
- **No NetworkPolicy.** Section 7 of the 3a design deferred it until
  `online-mode` was actually off, which happens here — so this is the milestone
  where pairing them becomes checkable, and the milestone where leaving it out
  stops being free. It stays out because milestone 6 owns NetworkPolicies as a
  group and splitting one out early would give an isolation guarantee only for
  the shape milestone 6 then has to redo. `known-issues.md` must say so
  explicitly rather than let it lapse.
- **No JRE module cut.** `known-issues.md` proposes deriving the module list
  once, for both images, now that the classpath has stopped moving. It is an
  image-size optimisation and no part of a player joining; doing it here would
  put a measurement exercise on the critical path of forwarding.
- **No secret rotation handling.** Section 6.5 of the main design makes
  rotation a manual runbook in V1 and defers automatic orchestration. 3b
  changes nothing about that.
- **No reload of a running process.** Configuration changes take effect on
  restart.

## 8. Contract changes

- **`SPAWNERY_MAX_PLAYERS` is removed** from `internal/podspec` and from
  `image/entrypoint.sh`. Nothing else reads it.
- **`spec.configOverlay`** is added to `ServerGroupSpec` and `ProxyGroupSpec`.
- **A second pod volume** at `/etc/spawnery` on both server and proxy pods.
- **`set_property` is deleted** from `image/entrypoint.sh`.

## 9. Test strategy

| Level | What it measures |
|---|---|
| `internal/render`, table tests | the three-layer precedence per flavour; that a critical field is unreachable from either lower layer; every refusal, by message |
| `internal/podspec`, table tests | the config volume on both pod builders; the overlay is optional; `SPAWNERY_MAX_PLAYERS` is gone |
| `internal/controller`, envtest | each group controller renders and owns its ConfigMap; content follows `spec.config` |
| `make image-test` | both images, offline, under the podspec's constraints. The Velocity run is new and asserts the proxy answers on 25565 |
| `make image-repro` | both images byte for byte — including the Paper image after the `oci-common.nix` rewrite, which must be unchanged |
| **the paired test** | `online-mode` is `false` on the backend and `true` on the proxy **in one run**. Nothing else catches the inversion, and the inversion is silent |

The paired assertion is called out separately because it is the only one whose
absence would not show up as a failure anywhere else. Both halves individually
look correct in isolation; only together do they say the network is not open.

`make test` stays Go-only. Measured at the 3a merge:
`go test ./... -count=1` takes **37.7 s**. That number was measured, not
remembered — the previous spec inherited a remembered "~24 s" that had been
wrong for two milestones.

## 10. Acceptance criteria

1. `nix/oci-common.nix` exists and both images build on it. `make image-repro`
   shows the Paper image unchanged by the extraction.
2. `nix build .#velocity-image` produces an image that starts offline under the
   podspec's constraints, as uid 10001, and accepts a TCP connection on 25565.
3. `spawnery-config --flavor paper` writes `server.properties` and
   `config/paper-global.yml`; `--flavor velocity` writes `velocity.toml`.
4. A value in the rendered ConfigMap reaches the written file. A value in the
   overlay outranks it. A critical field outranks both.
5. A missing forwarding secret, a missing required value, and an unparseable
   overlay each refuse the start with a message naming the file and key.
6. `online-mode` is `false` in the backend's `server.properties` and `true` in
   the proxy's `velocity.toml`, asserted in one test.
7. `set_property` and `SPAWNERY_MAX_PLAYERS` no longer exist anywhere in the
   repository.
8. Each group controller creates and owns a ConfigMap for its group.
9. `make test` is green and no slower than 37.7 s by more than the new tests
   genuinely cost.

## 11. Questions 3c inherits

- **The Velocity agent's shared Gradle subproject.** Decided in 3a: `agent/common`
  holds the session loop, the token source, the channel construction, the
  credentials and the TLS-1.3 `ConnectionSpec` override. The cost — the two
  agents can no longer be versioned apart — is 3c's to live with.
- **The proxy's readiness port.** 3a's podspec probes a `tcpSocket` on 8081,
  which the agent must bind only after it has processed its first `FullSync`.
  3b does not touch it; 3c has to honour it.
- **`[servers]` is empty by design.** The agent fills the server list from the
  channel. A Velocity that starts with servers in its config would have two
  truths about which backends exist.
- **Whether the operator runs inside the cluster for the E2E flow** is still
  open, and 3c's evidence run is where its absence has to be worked around a
  second time.
