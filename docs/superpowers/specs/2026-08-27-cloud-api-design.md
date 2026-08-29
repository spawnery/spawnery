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

`metadata.generation` keeps every other job it has, including ordering
retained failures for pruning — a digest has no order, so that one cannot move
and does not.

**This sentence used to read "This changes one comparison." It was written
before the code was read, and it was wrong.** The change is four comparisons in
`scaling.go`, plus three things the design did not anticipate: the free-slot
total in `AggregateGroup` and the `Progressing` condition in
`reportProgressing`, both of which would have started lying about a capacity
edit rather than merely staying still, and `adoptServers`, which skipped
ephemeral groups outright while nothing on that side read the field — a hole
rather than a rename.

One comparison deliberately does **not** move, and `docs/known-issues.md`
carries it: the failure-count filter runs before the hash is computed and on a
path where no hash exists, so raising `minReplicas` still clears a group's
failure streak. That becomes 7c's problem the moment `/cloud start` exists, and
7c owns it.

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

A new Gradle module `agent/api`, with **no platform dependency, no protobuf or
gRPC dependency, and no Kotlin**. It is types and interfaces only, written in
Java — §3.3 is why the language and the package name are both forced rather
than chosen.

```java
package cloud.spawnery.agent.api;

public interface SpawneryApi {
    Self self();
    List<Group> groups();
    Optional<Group> group(String name);
    List<ServerInfo> servers();
    Optional<ServerInfo> server(String name);
    List<CloudPlayer> players();
    Optional<CloudPlayer> player(UUID id);
    EventBus events();
    CompletionStage<ConnectResult> connect(UUID player, Target to);
    Lifecycle lifecycle();
}

public interface Lifecycle {
    /**
     * Adds capacity to a group for a while. See §4.4: this creates a boost
     * object and does not touch the group's own spec.
     */
    CompletionStage<BoostResult> boost(String group, int replicas, Duration forHowLong);

    /** Asks one server to empty out. Servers only -- see below. */
    CompletionStage<RetireResult> retire(String server);
}
```

Every type in those signatures is `java.*` or the module's own. That is the
whole constraint.

**`scale` writes no group's spec at all**, and the reason is measured rather
than chosen. Two things came out of reading the cluster this project runs on:

The operator's ClusterRole grants `get, list, watch` on `servergroups` and
write access only to `servergroups/status` and `/finalizers`. Scaling by
editing `spec.scaling.minReplicas` would need a new grant on the *declared*
object — write access to what a person wrote down — which is a large thing to
hand an operator for a convenience.

And it would not work anyway. The `ServerGroup` on this project's own cluster
lives in a Flux-managed file, so a `minReplicas` the operator wrote would be
reverted at the next reconciliation. An admin would type a command, watch the
count rise, and find it back where the file has it a few minutes later — which
is worse than a command that does not exist.

An annotation is no escape: writing one still needs `patch` on `servergroups`,
which is the same grant. **A separate object is the only shape where the
operator writes nothing a person declared**, and §4.4 is what it looks like.

**`retire` is `Server` objects only**, and it works with what the operator
already has: its ClusterRole carries `create, delete, get, list, patch,
update, watch` on `servers`, and a `Server` is an object the operator made
rather than one a person declared, so nothing in a GitOps repository owns it.
That is the whole difference between this verb and the one above. A proxy has no per-instance retirement:
`ProxyGroup` withdraws a pod through 4c-2's rollout, which chooses the pod
itself, and there is no field a person sets on one proxy. `retire` on a proxy
name fails with `NotRetirable` rather than being quietly reinterpreted as
something else — §8's rule that this milestone invents no new mechanism applies
here first.

`Self` says which role it is rather than being absent on one side:

```java
public sealed interface Self permits ServerSelf, ProxySelf {
    String name(); String group(); String network();
}
public non-sealed interface ServerSelf extends Self { int slots(); }
public non-sealed interface ProxySelf  extends Self { }
```

The entry point is one line, and it is the *same* line on both platforms:

```java
SpawneryApi api = Spawnery.api();
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

**This section said only that until the shipped jar was opened, and the jar had
two more things to say.** Both were measured on 2026-08-27 against
`spawnery-agents-0.2.0`'s Velocity jar, and both change the design rather than
decorate it.

**The Kotlin standard library is relocated too, so the API cannot be written in
Kotlin.** The relocate list ends with `"kotlin"`, and the jar carries **0**
entries under `kotlin/` against **1045** under
`cloud/spawnery/agent/shaded/kotlin/`. `ProxyRole.class` carries
`Lcloud/spawnery/agent/shaded/kotlin/Metadata;` and not `Lkotlin/Metadata;`.

Two consequences follow. A Kotlin compiler reading the shipped jar finds no
Kotlin metadata at all and treats every class as plain Java — no nullability,
no default arguments, no data-class semantics. And any signature carrying a
Kotlin function type would demand
`cloud.spawnery.agent.shaded.kotlin.jvm.functions.Function1` from a caller
compiled against the real one, which is a `NoSuchMethodError` at the call
rather than a compile error anywhere.

So `agent/api` is **Java**. The alternatives were considered and declined: the
Kotlin stdlib could be excluded from relocation, but the "relocate all of it"
rule exists because Paper ships its own copies and an earlier list-based
version of it went stale; or the API could stay Kotlin with signatures
restricted to JVM-mapped types, which keeps the metadata loss and holds only
by a test nobody would think to write. Java is the one option where a
third-party plugin — Kotlin or Java — sees exactly what it compiled against.
The rest of the agent stays Kotlin; this module is a thin boundary of
interfaces and value types, which is what Java expresses well.

**The package is `cloud.spawnery.agent.api`, not `cloud.spawnery.api`.**
`hack/agent-jar-check.sh` fails on any class outside `cloud/spawnery/agent/`,
by design: that check is what catches a dependency that shipped unrelocated,
and its message says so. A package one level up would be reported as exactly
that failure. The name sits inside the prefix and is still not a relocation
target, since the list relocates *source* prefixes into
`cloud.spawnery.agent.shaded.*` and never touches this one.

The module must also never gain a dependency that would put a relocated type
in a signature. **This is an invariant a test enforces, not a convention** —
see §6.2.

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

**There is also no way to deliver any of this to a Paper agent today.**
`ServerSession` sends exactly two messages when a stream opens -- ReportInterval
and SessionDeadline -- and then never sends again. `internal/proxyreg.Fleet`,
with its `Join`, `broadcast`, `snapshot` and `Resync`, exists for proxies
alone. The server side has no registry, no per-session channel and no fan-out
at all, so 7b's largest single piece is the one this design did not name: the
server-side counterpart of `Fleet`. It is why 7b is a sequence of plans rather
than one (§9.1).

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

## 4.4 Extra capacity, and where it lives

`/cloud start lobby 2` creates a **`ScaleBoost`**: a namespaced object owned by
the group it names, holding a replica count and an expiry.

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ScaleBoost
metadata:
  generateName: lobby-
  namespace: minecraft
  ownerReferences: [{kind: ServerGroup, name: lobby, ...}]
spec:
  groupRef: {name: lobby}
  replicas: 2
  expiresAt: "2026-08-28T20:00:00Z"
```

**The scaler adds live boosts to the group's own floor and to nothing else.**
`maxReplicas` still binds — a ceiling is an instruction, which is what 4a
established and what a command typed in a chat window must not be able to
lift. A boost raises what the group tries for; it never raises what it may
reach.

**It expires, and by default it expires soon.** An hour unless the command says
otherwise. What `/cloud start` usually means is an event, a rush, a Saturday
night — and the failure mode of a permanent one is well known: the boost from
last weekend is still there in March and nobody remembers why the lobby runs
four servers. A need that outlives an evening belongs in the file GitOps owns,
where a person reviews it.

**Nothing about it moves `metadata.generation`,** because it is a different
object. Two things follow, and the second was not designed for:

The group's servers do not become stale, so no boost rolls a fleet. That is
true whatever the staleness rule is, but it is worth stating beside 7a's work
rather than inferred from it.

And the group's failure streak is untouched. `ofGeneration` filters
`CountFailures`'s input by `group.Generation`, which a boost does not move —
so `/cloud start lobby` cannot clear a `CrashLoopBackoff`. `docs/known-issues.md`
recorded that hazard as this milestone's to solve; the GitOps constraint solved
it by forcing a design that never edits the group. The underlying entry stays
open, because a *person* editing `minReplicas` still clears the streak; what
goes is the claim that a command would.

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
| `/cloud start <group> [n] [for <duration>]` | Create a `ScaleBoost` (§4.4) | `spawnery.cloud.scale` |
| `/cloud stop <group>` | Delete that group's boosts | `spawnery.cloud.scale` |
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
lobby: +2 servers for 1h (until 20:00 UTC)
This is a boost, not a spec change. It expires on its own; /cloud stop lobby
ends it early. For a lasting change, edit the ServerGroup.
```

Three sentences and each earns its place. The first says what was created, the
second that it is temporary and how to end it, and the third points at the
thing a person should edit when the need is not temporary — because the
command deliberately cannot do that, and an admin who does not know why will
type it again next week.

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
- An E2E scenario drives `/cloud start` and asserts a `ScaleBoost` exists, the
  group's `status.boostedReplicas` rose, and **no server retired** — which is
  7a and 7c meeting.

  *This bullet asked for `minReplicas` to have moved until 2026-08-29.* It was
  written before §4.4 existed, and §4.4 is the reason it cannot: `/cloud start`
  writes no group spec at all, the operator holds no write on `servergroups`,
  and a `minReplicas` it wrote would be reverted by the next Flux
  reconciliation. Corrected rather than deleted — a spec that quietly loses a
  requirement is one nobody can review against. 7c-1 covers the boost and the
  status field; 7c-2 covers the command.

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
8. **The Kotlin standard library is relocated.** The shipped Velocity jar
   carries 0 entries under `kotlin/` and 1045 under
   `cloud/spawnery/agent/shaded/kotlin/`, and `ProxyRole.class` references
   `Lcloud/spawnery/agent/shaded/kotlin/Metadata;`. This is why `agent/api` is
   Java (§3.3).
9. **`hack/agent-jar-check.sh` fails on any class outside
   `cloud/spawnery/agent/`.** This is why the API package sits inside that
   prefix rather than beside it (§3.3).
10. **`ServerSession` has no downstream path after its opening two sends**, and
    `proxyreg.Fleet` covers proxies only. Building the server side's
    counterpart is 7b's largest piece (§4.1).

## 8. What this milestone does not do

- **One new CRD, and it was not the plan.** §8 of this document said "no new
  CRD: everything the API mutates is a field that already exists". That held
  until the operator's own RBAC and this project's GitOps repository were read
  — see §3.2. `ScaleBoost` exists because the alternative was write access to
  what a person declared, for an effect that a Flux reconciliation would undo.
- **No cross-namespace anything.** A pod's namespace is its horizon, in every
  direction, for every verb.
- **No persistence of a player's preferences.** §5.5.
- **No API for creating a group.** `/cloud` scales and retires what a person
  declared; declaring is still `kubectl` or GitOps. A command that creates
  groups needs a template concept this design does not have and does not want
  to guess at.
- **No stability promise below the API.** `cloud.spawnery.agent.api` is a contract.
  `spawnery.cloud/v1alpha1` underneath it is not, and may still move.
- **No guarantee an event is delivered.** Events are a feed, not a ledger. An
  agent that was disconnected missed what happened while it was gone, and the
  mirror it re-syncs on reconnect is the correction. A plugin that needs a
  ledger should watch the objects.

## 9.2 How 7c is cut

Three plans, and the split follows what each needs rather than what each is
about.

| | | Needs |
|---|---|---|
| **7c-1** | `ScaleBoost`: the CRD, the scaler reading it, the sweep of expired ones | a CRD and a chart change |
| **7c-2** | `/cloud` — one Brigadier tree over both platforms, and the permissions | 7c-1, for `start`/`stop` |
| **7c-3** | `CloudEvent`, the interest state, the coalescing, and the chat feed | 7c-2, for `/cloud events` |

7c-1 goes first and alone because it is the only one that touches the sizing
rule, and 7a's lesson is that a load-bearing rule and a feature in one
milestone give a regression two possible causes. It also ships something
useful on its own: `kubectl create` of a boost works without any command
existing.

Both platforms speak `com.mojang.brigadier` — measured 2026-08-28: Paper ships
`brigadier-1.3.10.jar` with 54 classes plus 45 of its own wrapper, and the
pinned Velocity jar carries 97 — so 7c-2's tree is one implementation over a
generic source type.

## 9.1 How 7b is cut

7b is four plans, not one, and the reason is §4.1: the largest piece is the
server-side fan-out this design did not name, and it is independent of the API
surface it will eventually carry. Each produces working, testable software on
its own.

| | | Depends on |
|---|---|---|
| **7b-1** | `agent/api`: the Java module, its types, and the packaging invariant | nothing |
| **7b-2** | The operator learns who is online: the proxy reports a roster with identities, the registry keeps and ages it | nothing |
| **7b-3** | The server-side fan-out — `ServerSession`'s counterpart to `proxyreg.Fleet` — and `NetworkState` delivered on both channels | 7b-2 |
| **7b-4** | Both agents hold the mirror, `SpawneryApi` answers from it, and §6.2's symmetry test compares the two | 7b-1, 7b-3 |
| **7b-5** | `CloudRequest`/`CloudResponse`, the operator's bounds, and `connect` | 7b-4 |

7b-1 goes first under any ordering: every other plan depends on those types
existing, and it is the one that settles the packaging bet — a module that
cannot be consumed by a third-party plugin makes the rest pointless, and
§3.3's measurements say that bet is narrower than it looked.

**Two things reshaped this table after 7b-1 shipped, and both were found by
reading rather than by a failure.**

A fan-out with nothing to fan out is not testable software, so the mechanism
and its first payload cannot be separate plans. That merged what were 7b-2 and
half of 7b-3.

And `SpawneryApi.players()` shipped in 7b-1 with no source. The Velocity agent
sends `PlayerJoinedServer` carrying a **username**, the operator's handler
accepts and ignores it — its own comment says "Nothing in milestones 3 or 4
consumes it" — and `agent.Registry` keeps counts and never an identity. So the
operator has no UUID and no stored name for anybody, and the mirror could not
have answered that method. Building the identities is now 7b-2, ahead of
everything that reads them, rather than a gap discovered when the mirror was
already being written.

### The identities, and what holding them costs

The proxy is the only honest source: every player on the network reaches a
backend through one, and `VelocityPlayers` already reads exactly the state a
roster needs. `PlayerRef` gains a `uuid` from `Player.getUniqueId()`, which
`AgentPlugin.onServerConnected` already reads for the rescue set.

This is the first time the operator holds a player's name and UUID rather than
a count, and that is worth naming rather than sliding past. It stays **in
memory in `agent.Registry`** and reaches no CR, no etcd, no log line at default
verbosity, and no metric label — a metric labelled by player name would be
both a cardinality bomb and a retention decision nobody made. It ages out with
the report that carried it, on the staleness rule counts already use, so a
proxy that goes silent stops asserting who is online rather than freezing a
roster.

## 9. Acceptance

**7a** — an ephemeral group's `minReplicas` changes and no server retires; its
`image` changes and every server does, one at a time. Driven in envtest, and
observed once in a real cluster before 7b begins.

**7b** — a plugin compiled only against `cloud.spawnery.agent.api`, bundling nothing,
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
