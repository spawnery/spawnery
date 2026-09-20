# Writing a plugin against the cloud

What a Paper or Velocity plugin can ask the cloud, with the same calls on both.

Here is the whole of a plugin that reads the network. Three files, and the
only Spawnery-specific parts are the `compileOnly` line, the dependency on the
agent, and the calls themselves.

```kotlin title="build.gradle.kts"
dependencies {
    compileOnly("cloud.spawnery:spawnery-api:0.5.0")
}
```

```yaml title="src/main/resources/paper-plugin.yml"
name: SlotBoard
version: '1.0.0'
main: com.example.slotboard.SlotBoardPlugin
api-version: '26.2'
dependencies:
  server:
    SpawneryAgent:
      load: BEFORE
      required: true
```

```java title="src/main/java/com/example/slotboard/SlotBoardPlugin.java"
package com.example.slotboard;

import cloud.spawnery.agent.api.ServerInfo;
import cloud.spawnery.agent.api.Spawnery;
import cloud.spawnery.agent.api.SpawneryApi;
import org.bukkit.plugin.java.JavaPlugin;

public final class SlotBoardPlugin extends JavaPlugin {
    @Override
    public void onEnable() {
        if (!Spawnery.isAvailable()) {
            getLogger().warning("no Spawnery agent on this server");
            return;
        }
        SpawneryApi api = Spawnery.api();
        for (ServerInfo s : api.servers()) {
            getLogger().info(s.name() + " has " + s.freeSlots() + " free slots");
        }
    }
}
```

**The name to depend on is the agent plugin's, and it differs by platform.**
On Paper it is `SpawneryAgent`; on Velocity the plugin id is `spawnery`, so
the same dependency is an annotation argument:

```java
@Plugin(id = "slotboard", name = "SlotBoard", version = "1.0.0",
        dependencies = @Dependency(id = "spawnery"))
```

Either way it is not optional. `Spawnery.api()` throws
`SpawneryUnavailableException` if you call it before the agent has enabled,
and the message says which of the two causes it was — the agent is missing, or
it has not finished enabling.

## Depending on it

**`compileOnly`, always.** The classes are loaded from the running agent
plugin, and a plugin that bundles its own copy puts a second
`cloud.spawnery.agent.api.SpawneryApi` on the server — a different type with
the same name, so the cast at your first call fails with a message about two
classes that look identical.

It is on Maven Central, so nothing has to be configured to resolve it. The
version is the one the agent inside the game images carries — the same number,
because they are built from the same source, and a plugin compiled against one
runs against the other.

**Compile against the oldest version you mean to support, not the newest.**
Methods are added to `SpawneryApi` and components are added to the value
records; a plugin built against 0.2.20 runs on a later agent, while one built
against a later agent and run on 0.2.20 meets a `NoSuchMethodError` at the
first call the older jar does not have.

## What it can see

Everything is scoped to the pod's own namespace, which is one `Network`. There
is no call that reaches another network, and that is structural rather than a
check: the agent's own credentials are a pod-bound ServiceAccount token, so
there is nothing to widen.

**A backend's mirror leaves out the private servers of an on-demand group, and
the group itself.** A plugin on a proxy sees them; one on a backend does not,
because routing lives on the proxies and a backend would otherwise carry an
entry, and a fresh picture on every start and stop, for servers nobody sends
anyone to. [Private servers](../guides/on-demand-servers.md#who-sees-them) says
what that changes for `connect`.

Reads need no permission, and there is no way to require one: Bukkit and
Velocity attach permissions to a player or the console, never to a plugin, so
a plugin calling this API presents no identity to check. A plugin already
reads the platform's own player list; what this adds is the rest of the
network. The boundary is who may install a plugin on the server, which is the
same person who may create a pod in the namespace.

[`/cloud`](../guides/cloud-command.md) is different and does check a
permission, because a command has somebody running it.

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

The value records gain components as the operator learns to say more, and
`ServerInfo` has gained two. Read them through their accessors, which is what
they are for; a plugin that constructs a `ServerInfo` of its own — in a test
double, say — is the one thing that has to be rebuilt when they do.

---

[What a plugin can do](what-a-plugin-can-do.md) covers the calls that change
something: moving a player, retiring a server, boosting a group, starting and
stopping a private server, closing a door, describing a round, and reading the
event feed. Every type and method is
in the [Javadoc](javadoc/index.html).
