# Milestone 7 — the cloud API, `/cloud`, and the event feed

## 1. What this milestone is

Three things a plugin author asks for and this repository has never offered:
a way to **read** what the network is doing, a way to **react** when it
changes, and a way to **act** on it — from either side of the proxy, with the
same code.

It is one design across three milestones because the first of them is a
prerequisite nobody would guess from the feature request, and because the
whole value of the other two depends on a property that has to be decided once
and held everywhere: **a plugin author moving between a Paper server and a
Velocity proxy should not have to think differently.**

- **7a** changes how an ephemeral `ServerGroup` decides a server is stale. No
  API, no proto, no command. It ships and is observed alone.
- **7b** is the API and the channel behind it: reading, events, and moving a
  player.
- **7c** is `/cloud` and the chat feed — the first two consumers, and the only
  evidence that 7b is usable rather than merely present.

What this is *not* is a second control plane. Everything the API can do, the
operator could already be asked to do with `kubectl`. The API is a second
*audience* for the same verbs.

## 2. Why 7a exists at all

The feature request is "start a server from in-game". In a declarative system
that sentence has no honest reading that leaves desired state alone: a server
created outside the group's own arithmetic is removed by the next reconcile.
So `/cloud start lobby 2` has to mean `scaling.minReplicas += 2` — permanent,
visible on the CR, and reversible by the same command.

That is a spec edit, and a spec edit bumps `metadata.generation`.

### 2.1 The measurement

There are three group kinds in this repository and, as of today, **three
different rules for what makes a member stale**:

| | Staleness is | Since |
|---|---|---|
| `ProxyGroup` | `spawnery.cloud/pod-hash`, a digest of the rendered pod | 4c-2 |
| `ServerGroup` type `Persistent` | `spec.podHash`, the same digest | 5b |
| `ServerGroup` type `Ephemeral` | **`metadata.generation`** | 4b |

`podspec.DesiredServerHash` (`internal/podspec/hash.go:116`) already exists, is
already computed, and is already stamped onto `Server.spec.podHash`
(`api/v1alpha1/server_types.go:38`). `internal/controller/persistent.go:83`
reads it. `internal/controller/scaling.go` does not: its `ScalingInputs.Generation`
(`scaling.go:57-63`) is what `selectRetirement` compares, and its own comment
says so — "A view whose Generation differs is stale."

So an ephemeral group rebuilds every server it has whenever *any* field of its
spec moves, including fields that render an identical pod. Milestone 4b
recorded this as its open item in exactly those words: "any spec change starts
a changeover: retuning `spareSlots` replaces a whole group of functionally
identical servers."

### 2.2 What that costs the feature

Without 7a, `/cloud start lobby 2` raises the floor **and rolls the entire
lobby** — every server retired one at a time under `maxUnavailable`, players
finishing their sessions on servers that are being replaced by identical ones.
An admin typing a capacity command would trigger a fleet changeover, and the
group's own status would not obviously say why.

### 2.3 The change

`ScalingInputs` carries `podspec.DesiredServerHash` for the group as it stands
now — the phrase `persistent.go:83` already uses for the same value — instead
of the group's generation, and a view is stale when its `Server.spec.podHash`
differs from it. That is the rule `persistent.go` and the `ProxyGroup` rollout
already use, applied to the third kind.

Two consequences, both wanted:

1. `minReplicas`, `maxReplicas`, `spareSlots`, `scaleDownStabilizationSeconds`
   and `update.*` stop rolling the group, because none of them reaches
   `DesiredServerHash`.
2. The three group kinds answer "is this member stale" the same way, which is
   the same symmetry argument the rest of this design rests on.

`metadata.generation` keeps every other job it has. This changes one comparison.

## 3. The API

### 3.1 The two principles

**Where the platforms could differ, the API takes the shape of the harder
one.**

`connect(player, target)` is a local call on Velocity that could return
immediately. On Paper it is a request that travels to the operator, to a proxy,
and back. If the signature follows the platform, it is synchronous on one side
and asynchronous on the other, and a plugin author moving between them has to
rewrite rather than recompile. So it returns a `CompletionStage` on **both**,
and the failure `NoProxyReachable` exists in the result type on Velocity, where
it never occurs.

The cost is real and belongs in the doc rather than in a surprise: **Velocity
plugins pay for an asynchrony they do not need.** That is the trade this
design makes on purpose.

**Reading is local and never a round trip.**

The operator keeps a mirror current in every agent, exactly as it already does
for the proxy's own server list (`ServerDirectory`, milestone 3c). `groups()`,
`servers()` and `players()` read that mirror. They have no latency, no timeout,
no failure mode and no `CompletionStage`. Only the three mutating calls cross
the wire.

### 3.2 The surface

A new Gradle module `agent/api`, with **no platform dependency and no
protobuf or gRPC dependency**. It is types and interfaces only.

```kotlin
package cloud.spawnery.api

interface SpawneryApi {
    val self: Self
    fun groups(): List<Group>
    fun group(name: String): Group?
    fun servers(): List<ServerInfo>
    fun server(name: String): ServerInfo?
    fun players(): List<CloudPlayer>
    fun player(id: UUID): CloudPlayer?
    fun events(): EventBus
    fun connect(player: UUID, to: Target): CompletionStage<ConnectResult>
    fun lifecycle(): Lifecycle
}

interface Lifecycle {
    fun scale(group: String, by: Int): CompletionStage<ScaleResult>
    fun retire(server: String): CompletionStage<RetireResult>
}
```

**`scale` moves a different field per group kind**, and that is deliberate:
`scaling.minReplicas` on an ephemeral `ServerGroup`, `spec.replicas` on a
persistent one and on a `ProxyGroup`. The caller names a group and a delta; the
operator knows which field that group's floor lives in, and `ScaleResult`
reports the field it moved by name so the caller never has to encode the
mapping. A caller that had to branch on group kind would be doing the operator's
job with worse information.

**`retire` is `Server` objects only.** A proxy has no per-instance retirement:
`ProxyGroup` withdraws a pod through 4c-2's rollout, which chooses the pod
itself, and there is no field a person sets on one proxy. `retire` on a proxy
name fails with `NotRetirable` rather than being quietly reinterpreted as
something else — §8's rule that this milestone invents no new mechanism applies
here first.

`Self` says which role it is rather than being absent on one side:

```kotlin
sealed interface Self { val name: String; val group: String; val namespace: String }
interface ServerSelf : Self { val slots: Int }
interface ProxySelf  : Self
```

The entry point is one line, and it is the *same* line on both platforms:

```kotlin
val api = Spawnery.api()
```

Behind it, each agent's plugin registers the implementation with its own
platform at enable time — Bukkit's `ServicesManager` on Paper, the plugin
manager on Velocity. Neither idiom reaches the plugin author.

### 3.3 Why the package is not relocated

Both agent jars are shaded, and `shadowJar` relocates every bundled dependency
to `cloud.spawnery.agent.shaded.*` (`agent/paper/build.gradle.kts:207`,
`agent/velocity/build.gradle.kts:234`). A public API type whose signature
carried a protobuf or gRPC class would be uncallable: a third-party plugin
compiles against `com.google.protobuf.X` and finds only
`cloud.spawnery.agent.shaded.com.google.protobuf.X` at runtime.

`cloud.spawnery.api` is therefore a package the relocation list must never
catch, and the module it comes from must never gain a dependency that would put
one of those types in a signature. **This is an invariant a test enforces, not
a convention** — see §6.2.

### 3.4 How a plugin depends on it

`compileOnly`. The API classes are loaded from the running agent plugin, and a
third-party plugin must not bundle its own copy: two copies on one server are
two `SpawneryApi` interfaces that are not the same type, and the cast fails at
the point of use with a message nobody can read.

`Spawnery.api()` throws a named exception when the agent plugin is not present
or has not finished enabling, rather than returning null — the two cases have
different remedies and a null cannot say which happened.

## 4. What the channel gains

### 4.1 Three messages

Symmetrically in `ProxyMessage` and `ServerMessage`, and in both operator-to-agent
messages:

| Direction | Message | Carries |
|---|---|---|
| agent → operator | `CloudRequest { id, oneof }` | `connect`, `scale`, `retire` |
| operator → agent | `CloudResponse { id, oneof }` | the answer to that `id` |
| operator → agent | `CloudEvent { oneof }` | unsolicited: phase transitions |

This is the first *request* this channel has ever carried. Until now an agent
reports and the operator instructs; nothing asks. Three things follow that do
not exist yet:

- **Correlation.** A response names the `id` it answers. Ids are per-stream and
  monotonic, so the operator never has to remember one across a reconnect.
- **A deadline.** A request that is never answered fails locally, with a
  timeout the API surfaces as an ordinary `CompletionStage` failure.
- **A rule for renewal.** `SessionLoop` renews make-before-break, so two
  streams are live at once. **An in-flight request does not survive the
  changeover: it fails with `Unavailable` rather than being retried on the new
  stream.** A `scale` delivered twice scales twice. Retrying is the caller's
  decision because only the caller knows whether the operation is safe to
  repeat, and none of the three are.

### 4.2 Interest is a state, not a subscription

An agent tells the operator, on its periodic report, whether anything is
listening — nobody with the permission online and no plugin registered means no
events wanted. This follows the pattern `Hello.ready` and `SetReady` already
set: a state, re-sent, so a reconnect cannot strand it and an operator restart
cannot forget it.

The reason is fan-out. A namespace with two hundred servers and two proxies has
two hundred and two streams; if every phase transition went to all of them, the
event volume would be quadratic in a quantity that is meant to scale. In
practice the admins are on the proxies, and the fan-out is two.

### 4.3 The bounds are the operator's, not the agent's

The agent checks the player's in-game permission and makes the request. The
operator believes it — the pod is already authenticated to a namespace by a
pod-bound ServiceAccount token, and nothing the operator could add would make
an in-game permission check verifiable from outside the game.

But belief is not permission. The operator bounds the request independently:

- **Namespace.** A request may only reach objects in the requesting pod's own
  namespace. This is structural, from the token, not a check that can be
  forgotten.
- **Ceiling.** `scale` may not raise `minReplicas` above the group's own
  `maxReplicas`. A group's ceiling is an instruction, and 4a already treats it
  as one.
- **Rate.** A per-pod token bucket, in the shape `internal/agentserver`'s
  connection bound already uses, with refusals published as a counter and
  alertable.

The result: **a compromised pod can do nothing its own group could not already
do**, which is the availability half of milestone 2a's promise, stated for a
new verb.

### 4.4 Events come from one place

The operator already records phase transitions through
`mgr.GetEventRecorder(...)`. `CloudEvent` is derived from that same
transition, not computed beside it. The property this buys is worth stating as
a requirement: **the chat feed shows the events `kubectl get events` shows.**
Two independent derivations of the same fact eventually disagree, and the one
in the chat is the one nobody can audit.

## 5. `/cloud` and the feed

### 5.1 One command tree

Velocity ships `com.mojang.brigadier` unrelocated — measured, 97 classes
including `CommandDispatcher`, in the pinned `velocity-3.5.1-615.jar`. Paper's
modern command API is Brigadier over the same package. **The Paper half is an
assumption this design has not measured**: the Paper artifact in the Nix store
is the Paperclip launcher (584 entries, no Brigadier), and the real classes are
fetched into `agent/paper/paper-repo/libraries` at build time. Task 1 of 7c's
plan verifies it before anything is built on it.

If it holds, the tree is written once, generic in the source type:

```kotlin
fun <S> cloudCommand(api: SpawneryApi, adapter: SourceAdapter<S>): LiteralArgumentBuilder<S>
```

`SourceAdapter<S>` supplies exactly two things — check a permission, send a
message — so `CommandSourceStack` and `CommandSource` never reach the tree. If
the assumption fails, `SourceAdapter` grows to cover the difference and the
tree stays one implementation; the cost is an abstraction, not a fork.

### 5.2 The verbs are the operator's

| Command | Effect | Permission |
|---|---|---|
| `/cloud list`, `/cloud info <x>` | Read the local mirror | `spawnery.cloud.read` |
| `/cloud start <group> [n]` | Raise the group's floor by `n` | `spawnery.cloud.scale` |
| `/cloud stop <group> [n]` | Lower it by `n` | `spawnery.cloud.scale` |
| `/cloud retire <server>` | `spec.retire = true` on one `Server` | `spawnery.cloud.retire` |
| `/cloud send <player> <target>` | `connect` | `spawnery.cloud.send` |
| `/cloud events on\|off` | The feed, for the caller | `spawnery.cloud.events` |

`start`/`stop` move capacity; `retire` moves one instance. Two verbs rather
than one ambiguous one, so `/cloud stop lobby` and `/cloud stop lobby-a3f9`
cannot be confused with each other by a typo in a server name.

`spec.retire` already exists and is tested — milestone 4b built it, and its
field comment says "a user never does", which this milestone changes on
purpose. Nobody is kicked: a retiring server takes no new joins and its players
finish in their own time.

### 5.3 `/cloud start` says what it did

```
lobby: scaling.minReplicas 1 -> 3
This is the new floor. /cloud stop lobby 2 lowers it again.
```

The field is named rather than implied, because it is not the same field for
every group kind (§3.2) and because the next thing an admin does is often to
look at the object.

A command that permanently changes desired state while looking like a one-shot
nudge is the class of surprise this repository avoids everywhere else. The
second line is not optional output.

### 5.4 The feed coalesces

A rolling update of a ten-server group produces ten `Ready` transitions in a
few seconds. Ten lines is a feed people turn off, which costs more than it
gives. Events are batched in a one-second window and collapsed by kind and
group:

```
[cloud] 3 servers ready in lobby (lobby-a3f9, lobby-b71c, lobby-c02e)
```

### 5.5 Opt-out is per session, on both sides

Paper could persist it in a player's `PersistentDataContainer`. Velocity has no
equivalent. Symmetry wins: the setting lives for the session on both platforms,
the feed is on again after a rejoin for anyone holding the permission, and
`/cloud events off` says so in its own output.

## 6. What proves it

### 6.1 7a

The load-bearing test is not about the API at all:

- An ephemeral group whose `minReplicas` changes retires **nothing**. This
  fails today.
- An ephemeral group whose `image` changes retires everything, one at a time,
  under `maxUnavailable`. This passes today and must keep passing.
- A group whose `spareSlots` changes retires nothing, which is milestone 4b's
  open item closing.

### 6.2 The API's own invariants

- **No relocated type in a public signature.** A test walks `agent/api`'s
  compiled classes and fails on any `com.google.protobuf.*`, `io.grpc.*` or
  `cloud.spawnery.agent.*` reference in a public or protected member. This is
  the invariant §3.3 rests on, and a convention would not hold it.
- **The two platforms expose the same surface.** A test asserts that the Paper
  and Velocity implementations of `SpawneryApi` declare identical public
  method signatures — which is the whole premise, and is otherwise checked by
  nobody.

### 6.3 The channel

- `cmd/spawnery-stubop` answers `CloudRequest`, and `hack/agent-test.sh` gains
  a phase driving a `scale` request **from inside the real image** to the stub.
- Envtest per bound, one at a time: a request above `maxReplicas`, a request
  naming another namespace, a request past the rate limit. Each refused, each
  with its counter moving.
- A request in flight across a make-before-break renewal fails `Unavailable`
  and is not delivered twice. This is asserted by counting deliveries at the
  stub, not by observing the failure — a retry that never happened and a retry
  that was silently dropped look the same from the caller.

### 6.4 The command and the feed

- The command tree is driven in `:common` against a fake API, with a fake
  `SourceAdapter` — the pattern `FakeOperator` and `FakeRole` already set.
- The feed's coalescing is a pure function over a list of events and a window,
  tested as one.
- An E2E scenario drives `/cloud start` and asserts the `ServerGroup`'s
  `minReplicas` moved and **no server retired**, which is 7a and 7c meeting.

## 7. Facts this design asserts about the code already here

Each was measured on 2026-08-27 and each is a thing a plan may rely on:

1. `podspec.DesiredServerHash` exists, is stamped onto `Server.spec.podHash`,
   and is read by `internal/controller/persistent.go` and by nothing in
   `internal/controller/scaling.go`.
2. `ScalingInputs.Generation` is what decides which ephemeral server retires.
3. `Server.spec.retire` exists, drives phase `Retiring`, and its comment says a
   user never sets it.
4. Both agent jars relocate every bundled dependency to
   `cloud.spawnery.agent.shaded.*`; the agent's own `cloud.spawnery.*` classes
   are not relocated.
5. The pinned Velocity jar ships `com.mojang.brigadier` unrelocated.
6. The pinned Paper artifact in the store is a launcher and carries no
   Brigadier; the Paper-side assumption in §5.1 is **unmeasured**.
7. `AgentRole` has no seam for an outbound request. `hello`, `playerCount` and
   `extraReports` all produce messages on a timer; nothing produces one on
   demand. 7b adds that seam.

## 8. What this milestone does not do

- **No new CRD.** Everything the API mutates is a field that already exists on
  an object that already exists.
- **No cross-namespace anything.** A pod's namespace is its horizon, in every
  direction, for every verb.
- **No persistence of a player's preferences.** §5.5.
- **No API for creating a group.** `/cloud` scales and retires what a person
  declared; declaring is still `kubectl` or GitOps. A command that creates
  groups needs a template concept this design does not have and does not want
  to guess at.
- **No stability promise below the API.** `cloud.spawnery.api` is a contract.
  `spawnery.cloud/v1alpha1` underneath it is not, and may still move.
- **No guarantee an event is delivered.** Events are a feed, not a ledger. An
  agent that was disconnected missed what happened while it was gone, and the
  mirror it re-syncs on reconnect is the correction. A plugin that needs a
  ledger should watch the objects.

## 9. Acceptance

**7a** — an ephemeral group's `minReplicas` changes and no server retires; its
`image` changes and every server does, one at a time. Driven in envtest, and
observed once in a real cluster before 7b begins.

**7b** — a plugin compiled only against `cloud.spawnery.api`, bundling nothing,
loads on both a Paper server and a Velocity proxy from the shipped images,
calls the same four read methods and the same `connect`, and gets the same
answers in the same types. A `scale` beyond `maxReplicas` is refused by the
operator with its counter moving.

**7c** — an admin holding `spawnery.cloud.scale` types `/cloud start lobby 2`
on a Paper server and on a Velocity proxy, sees the same output both times,
and the group's floor moves once per command with nothing retired. A second
player holding `spawnery.cloud.events` sees the resulting servers reach Ready
as **one** coalesced line, and the same transitions appear in
`kubectl get events`.
