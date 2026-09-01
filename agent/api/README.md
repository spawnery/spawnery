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

The value records gain components as the operator learns to say more, and
`ServerInfo` has gained two. Read them through their accessors, which is what
they are for; a plugin that constructs a `ServerInfo` of its own — in a test
double, say — is the one thing that has to be rebuilt when they do.

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

## Telling one run of a server from the next

`ServerInfo.incarnation()` is an opaque token that changes whenever the process
behind a name is replaced, and never otherwise. Compare it; never parse it.

The name cannot answer that on its own, and for one kind of server it never
will: an ephemeral server is named afresh every time, but a persistent one
keeps its name across every restart, because that name is the identity of its
world. Anything that remembers a server and later asks whether this is still
the one it meant — a rejoin, a queue, a scoreboard that outlives a
reconnect — compares this and not the name.

It is empty for a server whose pod the operator has not seen yet, which is a
server nobody is being sent to.

## Closing this server to new players

`acceptJoins(false)` stops the proxies sending anybody new here.
`acceptJoins(true)` undoes it.

**It is not `retire`.** Retiring says the server is finished: it stops taking
joins, empties out, and is taken down once it is empty. This says only the
first of those, it says it for as long as you like, and you can take it back. A
round that has started is not a server that is going away, and asking for the
one when you mean the other reads as a decommissioning to everybody who looks
at it afterwards.

**Nobody is moved.** The players already here go on playing until they leave on
their own.

**The phase does not change.** A closed server is still `READY`, because the
phase is the operator's account of a server's lifecycle and shutting a door is
not a lifecycle event. What changes is `ServerInfo.registered()` — the field a
caller choosing where to send somebody already reads.

Your group notices: a closed server's empty seats stop counting as the group's
free capacity, so a group sized by spare slots builds a replacement rather than
sitting at its floor while every server in it has shut its door.

Refused on a proxy, which is not in anybody's routing table but is the routing
table. And like `announce`, it survives a reconnection without being called
again — the agent restates the last door state on every new session, because
the operator's default for a session it has never seen is open.

## Saying what this server is doing

`announce(state, attributes)` publishes a short description of this server that
every other agent in the network reads back as `ServerInfo.state()` and
`ServerInfo.attributes()`.

**The cloud carries it and never reads it.** Nothing the operator decides looks
at a word of it — not where a player is sent, not when a server is replaced,
not how a group is sized. That is what makes it safe to put anything in, and
useless to put an instruction in.

**It is not `ServerInfo.phase()`.** The phase is the operator's account of a
server's lifecycle and no plugin can write it. This is the server's own account
of itself, and the two are meant to disagree: a server is `READY` from the
moment it can take players until it stops, and what is happening inside that
window is a question only the thing running there can answer. `/cloud info`
prints both, and prints yours after the word `says` so that an admin reading a
line where they disagree can tell which is which.

**Each call replaces the last one whole.** Attributes are not merged: publish
the whole description each time, which is also the only way an attribute can be
taken back. An empty announcement clears the description, and that is a
description like any other — a game that finished and said so does not come
back after a reconnect still claiming to be running.

The operator refuses rather than trims: at most 64 characters of state, at most
16 attributes, at most 64 and 256 characters of name and value. It refuses a
call from a proxy too, which has no per-instance record in the network picture
for a description to appear in. Each refusal says which.

You do not have to re-announce after a reconnection. The agent holds the last
description you published and re-publishes it on every new session, so an
operator that restarts does not leave a running game described as nothing.

### What a person wrote down about a group

`Group.attributes()` is the counterpart, and the difference is who writes it. A
server describes what it is doing right now; a group's attributes are written
by a person in the group's own definition — `spec.attributes` on a
`ServerGroup` or a `ProxyGroup` — and change when somebody edits that file.
Read them for what no server could tell you: which permission a group is
behind, which of several games it runs, whose it is.

The operator carries those too and reads none of them. They stop at the same
bounds, enforced by the API server rather than by the operator: sixteen
entries, names of at most 64 characters, values of at most 256.

## Hearing what happened

`events()` hands back an `EventBus`, the same one every time, so a plugin may
hold it. `subscribe(listener)` returns an `AutoCloseable` — close it on
disable, or the listener outlives your plugin in a classloader the platform is
trying to unload, which is the ordinary way a reload turns into a memory leak.
Closing twice is fine.

```java
try (AutoCloseable events = Spawnery.get().events().subscribe(e -> {
        if (e.warning()) {
            getLogger().warning(e.subject() + ": " + e.message());
        }
})) {
    // ...
}
```

**A feed and not a ledger.** An agent that was disconnected missed what
happened while it was gone, and nothing replays it: the network picture it
re-syncs on reconnect is the correction, and a better one than a replay would
be — it says what is true now rather than what was true in an order nobody was
watching. If you need a ledger, watch the objects.

**You get the facts, one per transition.** What a player sees in chat is a
collapsed summary — ten `Ready` transitions become one line — and you get the
ten. The `message` is the operator's own sentence, the same one `kubectl get
events` shows, so logging it puts you in agreement with whoever is reading the
cluster.

**`kind` is a string and not an enum.** The operator's vocabulary gains values,
and an agent older than one has to show it rather than fail to parse the
message it arrived in. Match on the ones you know; pass the rest through.

**Your listener runs on a network callback thread.** Do not block it and do not
touch the world from it — hand the work to your platform's scheduler. A
listener that throws is dropped from the next dispatch rather than taking the
session down with it, but it is still your bug and nothing tells you twice.

## What is not here yet

Nothing from the design remains unbuilt at this layer. Methods will be
**added** to `SpawneryApi`, never changed: plugins consume this interface and
do not implement it, so an addition breaks no caller.
