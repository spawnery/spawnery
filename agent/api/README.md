# The Spawnery plugin API

What a Paper or Velocity plugin can ask the cloud, with the same calls on both.

```java
if (Spawnery.isAvailable()) {
    SpawneryApi api = Spawnery.api();
    for (ServerInfo s : api.servers()) {
        getLogger().info(s.name() + " has " + s.freeSlots() + " free slots");
    }
}
```

## Depending on it

**`compileOnly`, always.** The classes are loaded from the running agent
plugin, and a plugin that bundles its own copy puts a second
`cloud.spawnery.agent.api.SpawneryApi` on the server — a different type with
the same name, so the cast at your first call fails with a message about two
classes that look identical.

```kotlin
dependencies {
    compileOnly("cloud.spawnery:spawnery-api:<version>")
}
```

Nothing publishes that coordinate yet. Until something does, build against the
jar from a release.

Your plugin must also declare a dependency on the `spawnery` plugin so it
enables after the agent. `Spawnery.api()` throws
`SpawneryUnavailableException` if you call it before that, and the message says
which of the two causes it was — the agent is missing, or it has not finished
enabling.

## What it can see

Everything is scoped to the pod's own namespace, which is one `Network`. There
is no call that reaches another network, and that is structural rather than a
check: the agent's own credentials are a pod-bound ServiceAccount token, so
there is nothing to widen.

Reads need no permission, and there is no way to require one: Bukkit and
Velocity attach permissions to a player or the console, never to a plugin, so
a plugin calling this API presents no identity to check. A plugin already
reads the platform's own player list; what this adds is the rest of the
network. The boundary is who may install a plugin on the server, which is the
same person who may create a pod in the namespace.

`/cloud` is different and does check a permission, because a command has
somebody running it.

## What is a value and what is a moment

`ServerInfo`, `Group` and `CloudPlayer` are records describing what the
operator last said. They do not update. Ask again for a newer one.

Reads never block, never time out, and throw nothing: the operator keeps a
mirror current inside the agent, so `servers()` is a lookup in a local map.

`CloudPlayer.server()` is empty for a player the proxy has and no backend does
— during login, and between one backend and the next. That is ordinary, not an
error, and it is exactly the player a drain is about.

## Version skew

`ServerPhase` and `Group.Kind` both carry `UNKNOWN`, and the operator is free
to publish a value your copy of this jar predates. Handle it. A `switch` that
throws on an unrecognised phase breaks on an operator upgrade that had nothing
to do with your plugin. Read a phase with `ServerPhase.fromWire`, which never
throws, rather than `valueOf`, which does.

## Moving a player

```java
api.connect(player.getUniqueId(), Target.group("lobby"))
   .thenAccept(r -> getLogger().info("ordered: " + r.ordered() + " -> " + r.target()))
   .exceptionally(e -> { getLogger().warning("could not move them: " + e); return null; });
```

`Target.server(...)` says exactly where. `Target.group(...)` says "wherever
that group has room" and lets the operator pick, which it can do better than
you can: it compares every backend's occupancy without racing the mirror.

**It returns a `CompletionStage` on both platforms**, including the proxy where
it need not. Following the platform would make this synchronous on one side
and not the other, and moving a plugin between them would be a rewrite rather
than a recompile.

**`ordered` is not `moved`.** The proxy that carries the move does not wait on
Velocity's own future — blocking a network callback on a round trip to a
backend is a cost the agent cannot pay — so nothing in this system can tell you
the player arrived. If you need to know, read `player(uuid)` a moment later.

A failure is ordinary. A player who logged out between your call and the
operator reading it fails with `NOT_FOUND`; so does a target that is not
routable yet. Handle it as a normal outcome rather than as a bug.

## Changing the fleet

Two calls write, and both are round trips through the operator on either
platform — nothing local can answer them.

`retire(server)` asks one server to stop taking joins and empty out. **It is
not a stop.** Nobody is moved and nobody is kicked; the players on it finish in
their own time, and the server goes away once it is empty. Asking for a server
that is already retiring **fails**, on purpose: the operator distinguishes "you
retired it" from "somebody had already asked", and a caller that wants to treat
the second as success can do that far more safely than one that was never told.

`boost(group, replicas, forHowLong)` adds capacity for a while, as a
`ScaleBoost` object rather than as an edit to the group. Pass `null` for the
operator's default of an hour.

**It adds to what a group tries for and never to what it may reach.** The
group's `maxReplicas` still binds, and a request for more than the ceiling
leaves is refused rather than trimmed — so what you asked for is what you got,
or you were told why not. The operator also bounds how long you may ask for.
Boosts add rather than replace: two calls make two boosts, which is what makes
"somebody else already boosted this" a non-event rather than a race.

`stopBoosts(group)` ends all of them and reports how many there were. Zero is
an ordinary answer, not a failure.

**Neither call can change what a group is.** A boost expires; a group that
needs to be permanently bigger needs its `ServerGroup` edited by a person, and
this API deliberately cannot do that — the operator holds no write on
`servergroups` at all.

## What is not here yet

Subscribing to events is designed
([`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../../docs/superpowers/specs/2026-08-27-cloud-api-design.md))
and not yet built. Methods will be **added** to `SpawneryApi`, never changed:
plugins consume this interface and do not implement it, so an addition breaks
no caller.
