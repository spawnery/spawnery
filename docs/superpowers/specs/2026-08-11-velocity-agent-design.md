# Milestone 3c: the Velocity agent

Status: design, 2026-08-11. Sub-project three of three in milestone 3.

Predecessors: `2026-08-10-proxy-channel-design.md` (3a, the operator's proxy
side) and `2026-08-10-velocity-image-design.md` (3b, the image and the
configuration rendering). The overall design is
`2026-08-07-minecraft-cloud-operator-design.md`; sections referenced bare
below — §5.2, §6.5, §6.6 — are its sections.

## 1. What this closes

Milestone 3's success criterion is one sentence: a player can join. Everything
that sentence needs exists except the thing that moves: a `ProxyGroup` renders
correct configuration onto disk, its pods start, `ProxySession` accepts a
stream and the fan-out is ready to push a server list — and nothing inside a
Velocity pod opens that stream. So no proxy pod ever turns ready, no
`ProxyGroup` leaves phase `Pending`, and a client that reaches port 25565 is
disconnected with Velocity's own "no available server".

3c is the second consumer of the milestone 2a channel. It does three things
that nothing else can do, and one piece of build work that makes them
affordable:

- **Serve the readiness probe** on port 8081, and only once a server list has
  arrived (§6.6).
- **Mirror the operator's server list** into Velocity's own registry.
- **Route players** — on join and on drain.
- **Split `agent/` into three Gradle projects** so the session machinery is
  written once rather than twice.

## 2. What is already in place

- **The channel** (2a): `ProxySession` joins the fan-out, streams `FullSync`,
  `RegisterServer`, `UnregisterServer`, `DrainPlayers`, `ReportInterval` and
  `SessionDeadline` back, and authenticates the pod through an audience-bound
  projected ServiceAccount token. `Bootstrapper` creates `spawnery-proxy`.
- **The pod** (3a): `podspec.BuildProxyPod` mounts the token and the CA at
  `/var/run/spawnery`, sets `SPAWNERY_OPERATOR_ENDPOINT`,
  `SPAWNERY_PLAYER_LIMIT`, `SPAWNERY_PROXY`, `SPAWNERY_NETWORK` and
  `SPAWNERY_GROUP`, declares the ready port 8081 and probes it over TCP.
- **The image** (3b): `nix build .#velocity-image` produces Velocity 3.5.1
  build 615 on a non-root base shared with the Paper image.
  `image/velocity-entrypoint.sh` already copies
  `$VELOCITY_HOME/agent/spawnery-agent.jar` into `plugins/` when one exists —
  written in 3b precisely so 3c does not change the image contract.
- **The configuration** (3b): `online-mode` is `false` on the backends and
  `true` on the proxy, the forwarding secret is mounted into both layers, and
  `velocity.toml` renders an empty `servers` table and an empty `try` list on
  purpose, because registration is dynamic and belongs to this milestone.
- **The Paper agent** (2c): the session loop, the token source, the channel
  construction, the credentials and the TLS-1.3 `ConnectionSpec` override,
  along with 1046 lines of tests that pin five separately-discovered defects.

## 3. Decisions

**`agent/common` holds the session loop, not only the plumbing.** The
alternative — sharing `Environment`, `TokenSource`, `OperatorChannel` and
`BearerCredentials` while copying `SessionLoop.kt` per agent — was rejected.
Those 748 lines are where all five of milestone 2c's defects lived, each with
a paragraph of reasoning in its comment: make-before-break timed on the
operator's answer, the reconnect obligation handed to a replacement, the bound
on an unanswered stream, the choice between graceful and forceful close, and
the channel released on every ended stream. A copy means two places to fix the
sixth one, and the second copy is the one nobody looks at.

The cost is real and is accepted: generifying that file means editing it.
`SessionLoopTest.kt` moves with it and keeps running against the Paper role, so
an edit that breaks one of the five invariants fails before it reaches an
image.

**One seam, not two.** The role interface exposes a single `onMessage` that
applies the role-specific effect and returns what the loop must act on.
Splitting it into `classify` plus `apply` would put two readers of the same
`messageCase` in two files.

**The proxy's initial-server choice: first group, fewest players.**
`routing.fallbackGroups` in order; the first group holding at least one
registered server wins; within it the server with the fewest players as
Velocity itself counts them, ties broken by name. Deterministic, testable, and
it needs no state the proxy does not already have. Its limit is stated rather
than hidden: with several proxies each balances only its own players, so the
distribution is per-proxy rather than network-wide. Network-wide placement
needs the operator's counts and a message that does not exist; it is not worth
a proto change for milestone 3.

**`DrainPlayers` is implemented here, not deferred to milestone 4.** The
operator already sends it — on every deregistration and again after every
periodic `FullSync`. An agent that logs and ignores it makes the whole drain
path from milestones 1 and 3a dead code: the `Server` reaches `Draining`,
the operator faithfully tells every proxy to move its players, nothing moves
them, and the pod's grace period expires with players still on it. That is not
a missing feature, it is a silent one.

What milestone 4 still owns is *proxy* drain — a proxy that stops accepting
new players while serving the ones it has. That needs a readiness the registry
can lower, which is a change to the 2a contract (see §11).

**Readiness is the bind, not the accept.** The ready gate is a `ServerSocket`
that is not bound until the first `FullSync` has been applied. Binding early
and accepting late would not work: the listen backlog completes the TCP
handshake without `accept()` ever being called, so the kubelet's `tcpSocket`
probe would turn the pod green with no server list behind it — the exact
failure §6.6 exists to prevent.

**The proxy sends no readiness on the wire.** `ProxyMessage` has no `Ready`
message and `Hello.ready` is documented as meaningful for server agents only;
`agentserver.handleProxy` says so in its own comment. `ProxyGroup.status`
derives `readyReplicas` from the pod conditions (3a, §3), so the kubelet is
the only path and a second one would be a second truth.

**Player counts are sampled on Velocity's scheduler.** Velocity is widely held
to be thread-safe where Bukkit is not, and reading `getPlayerCount()` straight
from the reporting timer would probably be fine. "Probably fine, by
reputation" is the class of assertion this project has paid for twice. A
repeating task on `ProxyServer.getScheduler()` writing two atomics costs
nothing and removes the question.

**One Nix derivation builds both agents.** `nix/agents.nix` replaces
`nix/paper-agent.nix` and installs two jars. One `deps.json`, one `make
agent-deps`, one Gradle invocation. The cost: a change to the Velocity agent
moves the shared derivation's store path and therefore the Paper image's agent
layer. Two derivations would mean two overlapping lockfiles that age
separately, which is worse.

**No kapt.** Velocity's plugin descriptor is generated by an annotation
processor shipped inside the proxy jar, but the descriptor is a plain JSON
file at the jar root with eight fields, and Velocity loads a hand-written one
identically. `src/main/resources/velocity-plugin.json` with the version
expanded through `processResources` — exactly what `paper-plugin.yml` already
does — avoids adding a Kotlin annotation-processing plugin to the build.

## 4. Components

### 4.1 The Gradle layout

`agent/` becomes the Gradle root with three subprojects.

```
agent/
  settings.gradle.kts        rootProject.name = "spawnery-agents"
  gradle.properties          moved up from agent/paper/
  deps.json                  one lockfile for the whole build
  common/     package cloud.spawnery.agent
    src/main/kotlin/    Environment, TokenSource, OperatorChannel,
                        BearerCredentials, SessionLoop, Session, AgentRole
    src/proto/java/     generated stubs, moved from paper/
  paper/      package cloud.spawnery.agent.paper
    src/main/kotlin/    AgentPlugin, ServerState, ServerRole
    src/main/resources/paper-plugin.yml
  velocity/   package cloud.spawnery.agent.velocity
    src/main/kotlin/    AgentPlugin, ProxyState, ProxyRole, ProxyEnvironment,
                        ReadyGate, ServerDirectory, Router
    src/main/resources/velocity-plugin.json
```

Both agents' entry classes are called `AgentPlugin`, so each moves into its own
subpackage. Paper's is a mechanical package move of six files and their tests
plus the `main` line of `paper-plugin.yml`; leaving Paper at the bare package
while Velocity sat in a subpackage would be less churn and worse — the
asymmetry would have to be explained every time either file is read.
Everything stays under `cloud/spawnery/agent/`, so the jar check's one real
invariant is unaffected.

**The generated stubs are ordinary sources of `common`'s main source set**, not
a separate one. The separation in `agent/paper` existed for exactly one reason:
`javac` 21 cannot resolve a class out of Paper's class-file-major-69 jars, so
the stubs had to compile without `paperLibraries` on their classpath. `common`
depends on neither platform, so the hazard does not exist there and the source
set that guarded against it is not carried over. Each agent adds its own
platform jar as `compileOnly`, and nothing that consumes it is Java.

`Makefile`'s `proto` target writes to `agent/common/src/proto/java`. `nix build`
filters source through the git index, so every moved file has to be `git add`ed
before a build means anything — a fast, quiet build right after the move is a
stale tree, not success.

### 4.2 `agent/common` — the shared session

`Environment`, `TokenSource`, `OperatorChannel` and `BearerCredentials` move
unchanged. `SessionLoop` and its private `Session` become generic over
`<Req, Resp>` with one injected role:

```kotlin
interface AgentRole<Req, Resp> {
    /** Which rpc this role speaks. */
    fun open(
        channel: ManagedChannel,
        credentials: CallCredentials,
        observer: StreamObserver<Resp>,
    ): StreamObserver<Req>

    /** The first message on every stream. */
    fun hello(version: String): Req

    /** The periodic report. */
    fun playerCount(): Req

    /**
     * Applies the role-specific effect of one operator message and returns
     * what the loop itself must act on.
     */
    fun onMessage(message: Resp): Directive
}

sealed interface Directive {
    data class Report(val seconds: Int) : Directive
    data class Deadline(val renewAfterSeconds: Int, val hardDeadlineSeconds: Int) : Directive
    data object None : Directive
}
```

`SessionLoop.readyChanged()` is replaced by a public `send(message: Req)` that
delivers on the current session if there is one and does nothing otherwise. The
Paper plugin calls it with a `Ready` message under its own `state.ready` guard;
the proxy never calls it.

Everything else about the class is preserved verbatim, including every comment.
The five invariants it encodes are restated here so a reviewer can check them
against the generic version rather than against memory:

1. A handover retires the outgoing stream only when the operator has answered
   the replacement — never on a Hello merely handed to the transport.
2. A stream that ends after a replacement has been opened books no reconnect;
   the replacement owes it.
3. An accepted-but-unanswered stream is bounded by the operator's own
   `hardDeadlineSeconds`, falling back to five minutes before the operator has
   stated one.
4. A stream the operator will never finish is closed forcefully; one it has
   answered is closed gracefully.
5. Every ended stream releases its channel, whether or not it books a
   reconnect.

### 4.3 `agent/velocity` — the plugin

**`AgentPlugin`** is the only class that touches the Velocity API, the same
rule the Paper plugin follows and for the same reason: everything else is
testable with JUnit and no proxy. It is a `@Plugin`-annotated class with an
`@Inject` constructor taking `ProxyServer` and a `Logger`, and it subscribes to
four events and no others:

| Event | What it does |
|---|---|
| `ProxyInitializeEvent` | reads the environment, starts the sampling task and the session loop |
| `ProxyShutdownEvent` | stops the loop, closes the ready gate, shuts the scheduler down |
| `PlayerChooseInitialServerEvent` | asks `Router` and calls `setInitialServer` |
| `ServerConnectedEvent` | sends `PlayerJoinedServer{player, server}` |

**The agent never sends `Heartbeat`.** The message exists in `ProxyMessage` and
the operator has a branch for it that deliberately does nothing: the stream is
its own liveness signal and the registry's staleness rule already derives from
`ReportInterval`, so a heartbeat would be a second truth about the same fact.
It stays on the wire format and unused, and this paragraph exists so the next
reader does not take the empty branch for an oversight.

**`ProxyEnvironment`** extends `Environment`'s decision with the two variables
only a proxy needs, and refuses rather than guesses:

- `SPAWNERY_PLAYER_LIMIT` missing or not a positive integer → `Dormant`,
  naming the variable. Reporting `slots = 0` would have the registry discard
  every count as `players > slots`, visible only as a counter.
- `SPAWNERY_FALLBACK_GROUPS` missing or empty → `Dormant`, naming the
  variable. A proxy that turns ready and then disconnects every player is
  worse than one that never turns ready. `RoutingSpec.FallbackGroups` carries
  `MinItems=1` and is required, so this is unreachable unless the operator
  itself is wrong — which is when a named refusal is worth the most.

**`ReadyGate`** owns the socket. `open()` binds `0.0.0.0:8081` once, is
idempotent, and starts a daemon thread that accepts and immediately closes.
`close()` runs on `ProxyShutdownEvent`. The port is a constant in the plugin
with a comment naming `internal/podspec.ProxyReadyPort`, on the same reasoning
`AGENT_DIR` already carries: the operator builds these pods and probes exactly
there, and a second configurable place would be a second place to be wrong. No
unit test can pin the number across languages; §9's harness phase does.

**`ServerDirectory`** maps operator messages onto Velocity's registry and holds
two structures of its own:

- `name → group`, because `ServerInfo` carries no group and the router needs
  one.
- the set of names *this agent* registered. 3b renders an empty `servers`
  table, but a user overlay may add entries, and a `FullSync` must not remove
  what the agent did not add.

Every registration consults `proxy.getServer(name)` first: absent → register;
present with the same address → nothing; present with a different address →
unregister then register. What Velocity does on a duplicate `registerServer`
is not measured, and this sequence means it does not have to be.

Addresses arrive as `podIP:25565` (`Server.status.address`) and become
`InetSocketAddress.createUnresolved(host, port)`. The ordinary constructor
performs a blocking DNS lookup on the calling thread, and the calling thread
here is a gRPC callback.

**`Router`** implements the selection once and both callers use it:

```kotlin
fun choose(groups: List<String>, excludingServer: String? = null): RegisteredServer?
```

Walks `groups` in order, takes the first with at least one known server, and
within it the one with the fewest `getPlayersConnected()`, ties by name.

**`ProxyState`** holds `players` and `slots` as atomics, written by a repeating
task on `ProxyServer.getScheduler()` and read only by the reporting timer.

**`ProxyRole`** implements `AgentRole<ProxyMessage, OperatorToProxy>`: it calls
`proxySession`, sends `Hello{version}` with `ready` left unset, reports
`PlayerCount{players, slots}` from `ProxyState`, and dispatches `FullSync`,
`RegisterServer`, `UnregisterServer` and `DrainPlayers` to `ServerDirectory`
and `Router`, opening the `ReadyGate` after the first applied `FullSync`.

### 4.4 Operator-side changes

`internal/podspec/proxy.go` gains one environment variable:

```go
// EnvFallbackGroups carries routing.fallbackGroups to the agent, comma
// separated. It is the same list the operator puts in DrainPlayers.toGroups,
// so join and drain resolve against one source.
EnvFallbackGroups = "SPAWNERY_FALLBACK_GROUPS"
```

Nothing else in the operator changes. `proxyreg`, the registry, the
controllers and the proto are untouched — 3a finished the operator's half of
the contract, and 3c fills it in.

### 4.5 Build and image

`nix/agents.nix` replaces `nix/paper-agent.nix`: one derivation over `agent/`,
installing `share/spawnery/paper/spawnery-agent.jar` and
`share/spawnery/velocity/spawnery-agent.jar`.

`gradleBuildTask` stays the single name `shadowJar`. Gradle resolves an
unqualified task name in every subproject that has one, and the shadow plugin
is applied to `paper` and `velocity` only — `common` produces a plain jar that
both consume through `implementation(project(":common"))` and that each
`shadowJar` bundles and relocates. Naming the two tasks explicitly would work
too and would depend on how the nixpkgs Gradle hook joins a multi-task value;
one name depends on nothing.

The derivation's version becomes `imageVersion` alone. It used to be
`${paper.paperVersion}-${imageVersion}`, which said something true about an
artifact belonging to one platform and would say something false about an
artifact belonging to two. That version reaches both plugin descriptors through
the `agentVersion` Gradle property, so `Hello.version` on either stream names
the same build.

`nix/velocity-image.nix` gains the agent layer at
`/opt/velocity/agent/spawnery-agent.jar`, which
`image/velocity-entrypoint.sh` already copies into `plugins/`.

`hack/agent-jar-check.sh` takes a flavour. The invariant — no class outside
`cloud/spawnery/agent/`, named in a list or not — is unchanged and is what
actually enforces the relocation. The per-platform collision list that follows
it differs, and the Velocity one is measured from the pinned jar rather than
assumed to match Paper's (§8).

Each agent's `shadowJar` relocates everything under
`cloud.spawnery.agent.shaded.`, including the Kotlin standard library, which
Velocity does not ship.

## 5. Data flows

**A proxy pod starts.** The entrypoint renders configuration and copies the
plugin. Velocity loads it; `ProxyInitializeEvent` fires;
`Environment`/`ProxyEnvironment` decide whether there is enough to run. If so
the agent starts its scheduler task, its `SessionLoop`, and nothing else — the
ready port is still closed, so the kubelet's probe fails and the pod stays
`NotReady`.

**The stream comes up.** `Hello{version}` goes out. The operator answers with
`ReportInterval` and `SessionDeadline`, then `Join` delivers `FullSync` under
one lock followed by a `DrainPlayers` for every `Server` in the namespace
already draining. `ServerDirectory` applies the sync, `ReadyGate.open()` binds
8081, the next probe succeeds, the kubelet writes the pod condition, and the
`ProxyGroup` controller counts the replica and moves to `Ready`.

**A player joins.** `PlayerChooseInitialServerEvent` fires;
`Router.choose(fallbackGroups)` returns a server and the agent calls
`setInitialServer`. Velocity connects the player through modern forwarding
with the shared secret; Paper accepts without authenticating, because
`online-mode` is `false` and the secret matched. `ServerConnectedEvent` sends
`PlayerJoinedServer{player, server}`, which the operator logs and nothing
consumes yet.

**A server is deleted.** The `Server` controller writes `Draining`, calls
`Registrar.Drain`, and the fan-out pushes `DrainPlayers{fromServer,
toGroups}`. Every player whose current server matches is offered
`Router.choose(toGroups, excludingServer = fromServer)` through
`createConnectionRequest(...).connectWithIndication()`. The operator repeats
the message after every periodic `FullSync`; a player already moved is no
longer on `fromServer`, so the repetition is a no-op. That idempotence is what
makes a reconnect or an operator restart mid-drain safe.

**The stream breaks.** `SessionLoop` reconnects with backoff and never gives
up. The ready gate stays open and Velocity keeps its last known server list —
§6.6's rule that the gate concerns startup only. On reconnect the operator
sends a fresh `FullSync` derived from the CRs, and `ServerDirectory`'s diff
makes an unchanged list produce no churn.

**The proxy shuts down.** `ProxyShutdownEvent` stops the loop and closes the
gate. `terminationGracePeriodSeconds` comes from `spec.drain.timeoutSeconds`
(default 300) and the JVM is PID 1, so SIGTERM reaches Velocity directly.

## 6. Error handling

| Condition | Behaviour |
|---|---|
| No `SPAWNERY_OPERATOR_ENDPOINT` | Dormant. One log line, no retry loop — the image must be runnable outside a cluster, and `make image-test` does exactly that. |
| CA or token unreadable | Dormant, naming the path. |
| `SPAWNERY_PLAYER_LIMIT` absent or unparseable | Dormant, naming the variable. |
| `SPAWNERY_FALLBACK_GROUPS` absent or empty | Dormant, naming the variable. |
| Operator unreachable | Reconnect with backoff, forever. The pod stays up; a proxy already ready keeps serving. |
| Operator accepts and never answers | Bounded by the loop's answer deadline; the stream is cancelled and another opened. |
| Ready port already bound | Logged at SEVERE. The pod never turns ready, the `ProxyGroup` stays `Pending`. Nothing on the CR explains it, which is recorded in §11 rather than papered over. |
| `Router.choose` finds nothing on join | Nothing is set; Velocity disconnects the player with its own message. Logged at WARNING with the groups that were searched. |
| `Router.choose` finds nothing on drain | The player stays where they are and the move is logged. The next repetition of `DrainPlayers` tries again, which is the whole point of the repetition. |
| A drain connection request fails | Logged. Same recovery: the repetition retries. |
| `FullSync` carries a malformed address | That entry is skipped and logged, naming the server. The rest of the sync applies — one bad entry must not cost a proxy its whole list. |

## 7. What 3c deliberately does not do

- **No NetworkPolicy.** Milestone 6 owns them as a group. This is the most
  consequential omission in the milestone and is recorded as overdue rather
  than deferred: with `online-mode=false` on the backends, a Paper server
  authenticates no one and trusts whatever completes the forwarding handshake,
  and nothing restricts who may attempt it.
- **No proxy drain.** A proxy that stops accepting new players while serving
  its current ones needs a readiness the registry can lower. Milestone 4.
- **No LoadBalancer or HostPort expose.** Milestone 6.
- **No `/play <group>` command or group policy.** Velocity's built-in
  `/server` stays open, as §4.2 of the overall design already states.
- **No operator image.** Out of scope for all of milestone 3, and §9's
  evidence run works around its absence for the second time.
- **No transfer of players between proxies.** Nothing needs it in V1.

## 8. Facts this design asserts about foreign systems

Milestone 3b's whole-branch review found that nearly every defect in both 3a
and 3b originated in a plan asserting something about a system the repository
does not own. This section exists so the claims are visible and their status
is explicit.

**Measured, on 2026-08-11, against the pinned jar** — reproduce with
`JAR=$(nix build .#velocity-jar --no-link --print-out-paths)`:

| Claim | How |
|---|---|
| The proxy jar contains the plugin API (205 classes under `com/velocitypowered/api/`), so no Maven artifact is needed for it | `jar tf "$JAR"` |
| The API's class files are major 65 (Java 21), so `javac` 21 reads them | `jar xf "$JAR" com/velocitypowered/api/proxy/ProxyServer.class` then `od -An -tu1 -j7 -N1` |
| The jar bundles Netty, Guava, Gson, Guice, Log4j, Adventure and Brigadier, and bundles **no** protobuf, gRPC, okhttp/okio or Kotlin | `jar tf "$JAR"` filtered by package root |
| `ProxyServer` has `registerServer(ServerInfo)`, `unregisterServer(ServerInfo)`, `getServer(String)`, `getAllServers()`, `getScheduler()` | `javap -cp "$JAR" com.velocitypowered.api.proxy.ProxyServer` |
| `RegisteredServer` has `getServerInfo()` and `getPlayersConnected()` | `javap` |
| `ServerInfo` is `(String, InetSocketAddress)` with value equality | `javap` |
| `PlayerChooseInitialServerEvent` has `getPlayer()`, `getInitialServer()`, `setInitialServer(RegisteredServer)` | `javap` |
| `Player` has `getCurrentServer()`, `createConnectionRequest(RegisteredServer)`, `getUsername()` | `javap` |
| The plugin descriptor has exactly eight fields: `id`, `name`, `version`, `description`, `url`, `authors`, `dependencies`, `main` | `javap -p com.velocitypowered.api.plugin.ap.SerializedPluginDescription` |
| `Server.status.address` is `podIP:25565` | `internal/controller/server_controller.go:575` |
| `ProxyGroup.spec.routing.fallbackGroups` is required with `MinItems=1` | `api/v1alpha1/proxygroup_types.go` |
| `ProxyMessage` carries no readiness, and the operator says so | `proto/…/agent.proto`, `internal/agentserver/server.go` |

**Asserted and not measured.** Each is designed around rather than relied on:

- *What Velocity does on a duplicate `registerServer`.* `ServerDirectory`
  consults `getServer` first, so the answer does not matter.
- *That a hand-written `velocity-plugin.json` at the jar root is loaded.* The
  annotation processor generates that same file to that same place, so this is
  strongly implied but not run. The first implementation task proves it by
  loading the plugin in the real image; if it does not hold, kapt is the
  fallback and the descriptor stays a resource either way.
- *That Velocity's API is safe to call from any thread.* Sidestepped: counts
  are sampled on Velocity's own scheduler.
- *How far a client must get through login before Velocity connects it to a
  backend.* This is §9's one genuinely open measurement, and the plan's first
  step on that task is to measure it rather than to assume it.
- *That the listen backlog completes a TCP handshake without `accept()`.* This
  is a property of Linux sockets, not of Velocity, and it is the reason the
  gate binds late rather than accepting late. The harness phase in §9 proves
  the resulting behaviour end to end regardless of the mechanism.

## 9. Test strategy

**Level 1 — JUnit, no proxy.** `ServerDirectory`, `Router`, `ProxyState`,
`ProxyEnvironment` and `ProxyRole` against a fake `ProxyServer`. The
`SessionLoop` suite moves to `common` and keeps running against the Paper role;
a second, much smaller suite exercises the generic loop against a fake role to
prove the seam itself.

**Level 1 — Go.** `internal/mcjoin` against a fake server that speaks the
packet framing back, so the client's own encoding is pinned without a cluster.

**Level 2 — `hack/agent-test.sh`, the real image against a real
operator-shaped server with real TLS and a real token.** `cmd/spawnery-stubop`
gains `ProxySession` and a `--proxy` behaviour that sends a `FullSync` with one
fabricated backend after the `Hello`. Two new phases:

1. **Passive plus ready gate.** Port 8081 is closed before the stub sends
   `FullSync` and open after. This is the milestone's new invariant and no
   other level can see it: a unit test can assert at most that `bind` was
   called. The phase also asserts the stub saw the `Hello` on `ProxySession`
   and a `PlayerCount` whose `slots` equals the configured limit.
2. **Supersede.** The stub retires the displaced stream where and when
   `internal/agentserver` does, and the same two-sided bound on stream-open
   rate applies as on the Paper side.

The mute phase is not repeated for the proxy. It exercises loop internals only,
and from 3c the loop is shared code that the Paper phase still drives. Supersede
*is* repeated even though the same argument would excuse it — because milestone
2c's lesson was that the assumptions hide in the harness, and the cost of being
wrong about which side of the wire a quantity is measured on is a defect that
survives to a cluster.

**Level 3 — the evidence run.** §10.

## 10. Acceptance criteria

1. `nix build .#agents` produces both shaded jars; `hack/agent-jar-check.sh`
   passes on each with its own flavour; `nix build .#agents --rebuild` is
   bit-identical.
2. `make test` is green, including the new `podspec` assertion that
   `SPAWNERY_FALLBACK_GROUPS` carries `routing.fallbackGroups`.
3. `make agent` runs both JUnit suites.
4. `make agent-test` passes all five phases: the three existing Paper ones and
   the two new proxy ones.
5. `make image-test` and `make image-repro` still pass on both images.
6. In a local kind cluster: a `ProxyGroup` with two replicas reaches phase
   `Ready`, `status.readyReplicas` is 2, and `status.address` is a reachable
   `nodeIP:nodePort`.
7. **A player can join, automated.** `cmd/spawnery-join` against a proxy whose
   `online-mode` is `false` through a `configOverlay` — a real user path, not a
   test-only build — reaches the point where Velocity connects it to a backend.
   The assertion is on the far side: the Paper server logs the join, Velocity
   logs the connection to that server, and `ProxyGroup.status.connectedPlayers`
   reaches 1.
8. **A player can join, manually.** One join with a real Microsoft account
   against `online-mode: true`, recorded in the handover document with the log
   lines that show it.
9. Deleting a `Server` with a player on it moves that player to a fallback
   server rather than disconnecting them, observed in the proxy log.

Criterion 7 is scoped by one measurement the design does not have: whether
completing login is enough for Velocity to start the backend connection, or
whether the client must also play the configuration phase. `internal/mcjoin` is
structured so that the configuration phase is an extension and not a rewrite,
and the implementation measures this against the real proxy before deciding.

The cluster orchestration for 7 through 9 is a documented runbook, not a
script. The operator still runs outside the cluster through `go run` and the
local flow hand-builds the `Service` and `Endpoints` its own pods dial; turning
that into a script is milestone 6's work, and doing it here would build the
wrong thing twice.

## 11. What 3c leaves open

- **The ready port number is spelled in two languages** —
  `internal/podspec.ProxyReadyPort` and a Kotlin constant — with no test that
  can compare them. Only the level-2 harness catches a divergence, and only
  when it runs.
- **A proxy that cannot bind its ready port is silent on the CR.** It stays
  `Pending` with the reason only in the container log. This is the same shape
  as the `playerLimit` defect 3b found and fixed, in a place where the operator
  has nothing to write.
- **`SPAWNERY_FALLBACK_GROUPS` is a third spelling of the fallback list**,
  after the CRD field and `DrainPlayers.toGroups`. The pod spec builds it from
  the same source, so it cannot disagree today; nothing pins that it will not.
- **Per-proxy load balancing.** Stated in §3, worth restating: with several
  proxies, placement is even per proxy and not necessarily across the network.
- **Proxy drain still needs a lowerable readiness** in
  `internal/agent/registry.go`. That is a milestone 2a contract change and
  belongs to milestone 4, which owns proxy drain.
- **The NetworkPolicy is overdue.** See §7. Until it lands, a backend pod
  accepts a connection attempt from any pod in the cluster that can reach port
  25565.
