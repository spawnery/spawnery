# Design: The Paper agent

Milestone 2c. Status: design, 2026-08-09.

This document covers the Kotlin plugin that closes the second half of the
two-stage ready gate, its reproducible build, and its way into the base image.
It builds on the agent channel from milestone 2a
(`2026-08-08-agent-channel-design.md`) and the base image from milestone 2b
(`2026-08-08-paper-base-image-design.md`), and it does not restate their
decisions.

## 1. Purpose

A `Server` pod today runs a real Paper process whose readiness probe turns
green, and the `Server` still stops in phase `Starting`. The ready gate wants
two signals and receives one. The missing one is an agent that opens a
`ServerSession` and says it is ready.

That is the whole of this milestone. After it, a `Server` reaches phase `Ready`
and `status.players` carries a real number. A player still cannot connect —
that needs the proxy layer from milestone 3 — but every piece on the backend
side is then in place.

## 2. What is in and what is not

**In scope:** the Paper agent plugin, its build, its place in the image, a JVM
toolchain in `flake.nix`, and a stub operator to test against.

**Out of scope, deliberately:**

- Everything proxy-side. `ProxySession`, the Velocity image, registration and
  forwarding are milestone 3.
- Slimming the JRE. `known-issues.md` dates that explicitly *after* this
  milestone: the module list has to be derived once from the complete
  classpath, and only milestone 3 makes it complete by adding Velocity. This
  milestone is where the Paper-side classpath stops moving, not where the cut
  happens.
- Factoring `nix/oci-common.nix`. That is a precondition for the second image,
  and this milestone does not create one.
- Any change to the `.proto`. The contract is frozen; both directions including
  the milestone 3 messages already exist.

## 3. What was measured before designing

The Paper bundle from milestone 2b was inspected directly
(`nix build .#paper-repo`, 166 MB, 103 jars). Four findings shape the design,
and each replaces an assumption that would otherwise have been guessed:

**The compile classpath already exists in the repository.** `libraries/`
carries `io/papermc/paper/paper-api/26.2.build.111-stable/paper-api-26.2.build.111-stable.jar`
(2.8 MB, 2527 entries) alongside Adventure 5.2.0, Guava 33.6.0-jre,
slf4j-api 2.0.18 and jspecify 1.0.0. The plugin can compile against the exact
API the server loads it into, with no new artifact and no new hash — the same
move as milestone 2b taking the Mojang hash out of the already-pinned Paper
jar.

**The shading trap from main design §5.2 is measured, not theoretical.**
`com/google/protobuf/protobuf-java/4.29.0/protobuf-java-4.29.0.jar` and
`io/netty/*/4.2.15.Final/` are on the server's own runtime classpath.

**`protoc-gen-grpc-java` 1.83.1 is packaged in nixpkgs** as a native binary,
next to `protobuf` 35.1. Stub generation is therefore a Nix-native step, exactly
like `make proto` is for Go, and needs no Gradle plugin.

**`paper-api` is class file major 69, i.e. Java 25.** Reading it requires a
Java 25 capable compiler. nixpkgs `gradle` 8.14.4 runs on JDK 21 by default;
see §4.4.

## 4. The build

### 4.1 Dependencies: `gradle.fetchDeps` and a checked-in lockfile

The plugin is a Gradle project built through the nixpkgs Gradle setup hook with
`mitmCache = gradle.fetchDeps { pkg = self; data = ./deps.json; }`.

`deps.json` is a lockfile holding **one SHA-256 per artifact**, checked into the
repository. That is not an approximation of main design §5's requirement that
"artifacts are checked against SHA-256 hashes that are checked into the
repository and never fetched from the download source" — it is literally that
requirement, at the granularity the requirement names.

Regenerating the lockfile runs outside the Nix sandbox, like `make proto`. It
gets a Makefile target and a README paragraph, so nobody discovers by accident
that a dependency change needs a second command.

### 4.2 Compile classpath: `packages.paper-repo`, not a Maven repository

`paper-api` and the other API-side jars come from the `paper-repo` derivation
(§3). They are wired into Gradle as a flat file dependency on the store path, so
`deps.json` never learns about them and a Paper bump cannot silently change the
API the plugin compiled against.

### 4.3 Transport: `grpc-okhttp`, and relocation on top

The gRPC client uses **`grpc-okhttp`, not `grpc-netty`**. Paper carries Netty
4.2.15 on its classpath; grpc-netty pins the 4.1.x line. That is a real version
conflict on a shared package, not a hypothetical one. `grpc-okhttp` avoids
Netty entirely, is the ordinary choice for a client, and is a fraction of the
weight.

Relocation happens regardless, as main design §5.2 requires: the Shadow plugin
relocates every bundled dependency under `cloud.spawnery.agent.shaded.`,
`com.google.protobuf` included, because Paper brings its own protobuf-java and
the plugin must not meet it.

### 4.4 The toolchain

Gradle runs on the JDK it ships with (21). Compilation uses a **Java 25
toolchain**, registered by overriding the nixpkgs Gradle package:
`gradle.override { javaToolchains = [ jdk25_headless ]; }`, which writes
`org.gradle.java.installations.paths` into Gradle's own `gradle.properties`.

Three things about this must be **verified first, before any plugin code is
written**, because the answers change the build file and nothing else in this
design depends on them:

1. that Kotlin 2.x as resolved by the Kotlin Gradle Plugin accepts a class file
   major 69 jar on its compile classpath,
2. which `jvmTarget` the output should carry — 21 is sufficient, since the
   image runs a JDK 25 that reads it, and a lower target is the safer default,
3. that the Java 25 toolchain resolves inside the Nix sandbox without Gradle
   attempting a download (`toolchainManagement` auto-provisioning must be off).

The Kotlin compiler itself arrives through the Kotlin Gradle Plugin and thus
through `deps.json`. nixpkgs `kotlin` is not needed and is not added.

### 4.5 Reproducibility

`make image-repro` compares two independent image builds byte for byte, and the
agent jar is inside the image. The jar must therefore be bit-reproducible:
`preserveFileTimestamps = false` and `reproducibleFileOrder = true` on every
archive task, including the Shadow task. This is an acceptance criterion, not a
hope — the existing `make image-repro` is what enforces it, unchanged.

### 4.6 Generated stubs

`make proto` is extended. Alongside the Go code in `internal/agentpb`, it
generates the Java message and service stubs from the same
`proto/spawnery/agent/v1alpha1/agent.proto` using nixpkgs `protoc` and
`protoc-gen-grpc-java`, into `agent/paper/src/main/java/`. The generated code is
checked in, exactly like `internal/agentpb` and `zz_generated.deepcopy.go`, and
`make test` does not regenerate it.

One `.proto`, two generators, one command. The alternative — a Gradle protobuf
plugin — would pull a platform-specific `protoc` binary through `deps.json` and
pin the build to one architecture for no gain.

## 5. Layout

| Path | |
|---|---|
| `agent/paper/` | `build.gradle.kts`, `settings.gradle.kts`, `src/main/kotlin`, `src/main/java` (generated), `src/test/kotlin`, `deps.json` |
| `nix/paper-agent.nix` | the derivation behind `packages.paper-agent` |
| `cmd/spawnery-stubop/` | Go, test-only, never enters the image |
| `hack/agent-test.sh` | runs the image against the stub operator |

`packages.paper-agent` is defined on every system — it is a JVM build, not a
Linux image — unlike `packages.paper-image`.

New Makefile targets:

| Target | |
|---|---|
| `make agent` | builds the plugin jar and runs its Kotlin tests; joins `make all` |
| `make agent-deps` | regenerates `agent/paper/deps.json`; runs outside the sandbox and is never part of another target |
| `make agent-test` | runs the image against the stub operator; `x86_64-linux` only, like `image-test` |

`make proto` gains the Java stub generation (§4.6). `make test` is untouched.

## 6. The plugin

Five units, of which exactly **one** touches Bukkit. That is the precondition
for JUnit proving anything without a running server.

| Unit | Job |
|---|---|
| `AgentPlugin` | `onEnable`/`onDisable`, the `ServerLoadEvent` listener, the main-thread timer. The only Bukkit contact point. |
| `ServerState` | the `ready` flag and the last sampled player count. No Bukkit, no gRPC. |
| `TokenSource` | reads `/var/run/spawnery/token`. |
| `OperatorChannel` | builds the TLS channel from `ca.crt` and `SPAWNERY_OPERATOR_ENDPOINT`. |
| `SessionLoop` | the state machine: connect, report, renew, back off. |

The plugin ships a `paper-plugin.yml`, not a `plugin.yml`, so Paper gives it an
isolated classloader. That narrows the conflict surface; it does not remove the
need for relocation, because the server's own libraries still live on the
parent.

### 6.1 Threading

`Bukkit.getOnlinePlayers()` is not thread-safe. A `runTaskTimer` on the main
thread samples the count and `Bukkit.getMaxPlayers()` into `ServerState`; the
network coroutine only ever reads. No Bukkit call is made from a gRPC callback.

`slots` is reported as `Bukkit.getMaxPlayers()` rather than the
`SPAWNERY_MAX_PLAYERS` environment variable. The entrypoint derives the former
from the latter, so they agree — but the server's own number is the one it
actually enforces, and reporting anything else would be reporting an intention
instead of a fact.

### 6.2 The token is re-read for every stream

The projected token lives 600 seconds and the kubelet replaces the file. A token
read once at startup carries the first session and no later one, and the failure
would present as an authentication problem rather than as a caching bug. It is
therefore read from disk on every stream open and never cached.

This also settles how the header is applied: as `CallCredentials` per call,
which makes a stale token structurally impossible.

### 6.3 Trust comes from the mount and nowhere else

The TLS channel validates against `/var/run/spawnery/ca.crt` only — no system
trust store. The bundle may hold several concatenated PEMs (agent channel design
§6.2, which keeps that format open for a later CA rotation), so all of them are
parsed, not just the first.

### 6.4 The header, character for character

`Authorization: Bearer <token>`, one space. `internal/grpcauth/interceptor.go`
matches that prefix exactly and fails closed on anything else, reporting "no
token" rather than "wrong spelling". This is an obligation `known-issues.md`
already records; §9 turns it into an assertion.

### 6.5 Ready is a state

The `ServerLoadEvent` listener sets the flag on `LoadType.STARTUP`. Every
`Hello` carries the current value. A transition to `true` while a stream is live
additionally sends `Ready{}` at once.

There is no path back. `Hello{ready:false}` cannot lower a readiness the
registry has already recorded (`known-issues.md`), so the agent does not try to
express "connected but no longer ready". That state is not representable in this
contract, and pretending otherwise in the agent would only hide it.

### 6.6 Renew before expiry, never after

On `SessionDeadline{renewAfterSeconds}` the `SessionLoop` waits that long, then
opens a **new** stream, sends `Hello` on it, and closes the old one only
afterwards. Make before break.

Without the overlap every server drops out of `Ready` on the rhythm of the hard
deadline, deregisters from the proxies and collects a readiness loss — a
self-inflicted flap counter. `Registry.Supersede` exists on the operator side to
carry readiness across the handover and has no effect without an agent that
actually overlaps.

A jitter of ±10 % is applied to the renewal delay so the pods of one group do
not all renew in the same instant.

### 6.7 Failure posture: the agent never takes the server down

A broken stream is retried with exponential backoff and jitter, from 1 s to a
cap of 30 s, without limit. The agent never delays server startup and never
shuts the server down.

That is safe rather than lax, and the reason is on the operator's side: without
an agent the `Server` stays in phase `Starting`, is never registered with a
proxy, and no player reaches it. A self-shutdown would add a crash loop and
destroy the logs that would explain the cause.

**Without `SPAWNERY_OPERATOR_ENDPOINT` the plugin stays dormant** — one log
line, then nothing. No connection attempt, no backoff loop. This keeps
`make image-test` clean and keeps the image usable outside a cluster.

## 7. The way into the image

The jar sits in the image at `/opt/paper/agent/spawnery-agent.jar`, in the
read-only part. The entrypoint creates `/data/plugins` and copies it there on
every start, unconditionally: the image is the truth, not last week's volume
content.

Pointing `--plugins` at a directory inside the image is not an option, and this
was measured in milestone 2b: Paper writes its plugins' *data* folders inside
the plugins directory (`plugins/spark/config.json`, `plugins/bStats/config.yml`).
A read-only plugins directory takes Paper's own bundled plugins down with it.

The image tag moves from `26.2-0.1.0` to `26.2-0.2.0` — `imageVersion` in
`nix/paper-image.nix` — and `Hello.version` reports that same string. One
version, not two.

**Known limitation, recorded rather than worked around:** a user mount at
`/data/plugins` breaks startup. Mounts below `/data` are explicitly allowed by
`podspec`, a ConfigMap mount is read-only, and the copy then fails under
`set -eu`. This is the same crack as the already-documented `/data/config`
collision, it belongs in the same list, and the fix belongs with that one — not
in a contortion inside the entrypoint.

## 8. Error handling

| Situation | Behaviour |
|---|---|
| `SPAWNERY_OPERATOR_ENDPOINT` unset or empty | one log line, plugin dormant |
| `ca.crt` or token file missing | log at error, dormant — no retry loop against a missing file |
| Operator unreachable | backoff 1 s → 30 s, forever |
| Stream broken mid-session | same backoff, `Hello` carries the current ready state on reconnect |
| Token rejected | same backoff; the token is re-read each time, so a rotation heals it |
| `onDisable` | cancel the scope, close the channel; no report on the way out |

Nothing in this table takes the server down, and nothing blocks startup.

## 9. How it is proven

Three levels. Each proves something the others structurally cannot.

**Level 1 — JUnit inside the Gradle build**, executed in the Nix derivation via
`doCheck`, running on every platform. `SessionLoop` runs against an in-process
gRPC server:

- the new stream exists *before* the old one is closed (§6.6),
- the backoff sequence on a broken stream,
- `ready` propagation, including the immediate `Ready{}` on transition,
- the `Authorization` header, byte for byte, via a recording interceptor,
- the token is re-read per stream open, not cached.

`ServerState` and `TokenSource` get plain unit tests. `make test` stays Go-only
and fast; `make agent` is the target that runs the Kotlin tests, and it joins
`make all`.

**Level 2 — `hack/agent-test.sh` against `cmd/spawnery-stubop`.** The real image
on a private container network against a small Go stub serving the real
`AgentService` over TLS, with a self-generated CA and a token file at the paths
`podspec` prescribes. Asserted: `Hello{ready:true}` arrives, the header is
byte-exact, `PlayerCount` arrives at the dictated interval with `slots` matching
the enforced maximum — and, with a deliberately short `renewAfterSeconds`, that
two streams genuinely overlap.

That last assertion is the one `known-issues.md` calls non-optional and the one
a unit test can only claim, not demonstrate: it needs a real JVM, a real TLS
handshake against the pinned CA, and Paper's real classloader.

Like the other image targets, level 2 runs on `x86_64-linux` only.

**Level 3 — `make image-test`, unchanged.** It thereby becomes evidence for two
things: that an unreachable operator does not stop the server from starting, and
— through the check that the plugin loads and leaves no stack trace — that the
relocation holds. A protobuf conflict surfaces exactly there, at class load.

## 10. Acceptance criteria

1. `nix build .#paper-agent` produces the shaded plugin jar and runs its tests.
2. `make image-repro` still passes: two image builds, byte-identical, with the
   agent jar inside.
3. `make agent-test` shows `Hello`, `PlayerCount`, the exact header, and two
   overlapping streams across a renewal.
4. `make image-test` still passes offline, with the plugin loaded and no stack
   trace.
5. `make test` is unchanged in scope and does not get slower.
6. The evidence run against kind reaches phase `Ready` for a `Server`, and
   `status.players` reports a real number.

## 11. What this milestone leaves open

To be added to `known-issues.md` on completion:

- the `/data/plugins` mount collision (§7),
- the JRE module list, now derivable because the Paper-side classpath is
  complete, to be cut together with Velocity in milestone 3,
- whether `deps.json` regeneration needs a CI guard, which is a milestone 6
  question.
