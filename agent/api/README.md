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

## What is not here yet

Reading only. Subscribing to events, moving a player between backends, and
starting or stopping servers are designed
([`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../../docs/superpowers/specs/2026-08-27-cloud-api-design.md))
and not yet built. Methods will be **added** to `SpawneryApi`, never changed:
plugins consume this interface and do not implement it, so an addition breaks
no caller.
