# A server is not ready while a plugin is still starting

## What this corrects

`docs/known-issues.md` proposes that "the agent runs inside [the server] and
could report readiness when the server has finished enabling rather than when
its socket opens". That is already what happens, and the entry is wrong about
where the window is.

The gate wants both signals:

```go
// internal/phase/phase.go:569
if in.PodExists && in.PodRunning && in.PodReady && in.AgentReady && ... {
    return Decision{Next: Ready, Register: true, ...}
}
```

and the Paper agent raises `AgentReady` from `ServerLoadEvent(STARTUP)`
(`AgentPlugin.kt:247`), which Bukkit fires with the `Done` line, after every
plugin has been enabled. Measured 2026-09-05 on `hub-gakt`: `Done (48.914s)`
at 10:26:10, the `Server` object `Ready` at 10:26:12. The socket had been open
since 10:25:43.

The real window is narrower and the operator cannot see it: a plugin whose
initialisation continues on its own executor **past** `ServerLoadEvent`. On
the same start ViaVersion made it, by nothing:

```
10:26:10 [ViaVersion] Finished mapping loading, shutting down loader executor.
10:26:10 Done (48.914s)!
```

On the start recorded in the known issue it did not, and a client joining in
between was told `Outdated client! Please use 26.2`.

Nobody outside the plugin knows when it is finished. So the design does not
try to find out — it gives the plugin a way to say so, and fixes the one place
where the operator ignores it being said.

## Two changes, and why they are one document

**A** makes the operator honour a door the server has already closed. It is a
defect on its own: `JoinsClosed` is read in the `Ready` branch
(`phase.go:652`) and nowhere else, so a server that closed its door while
starting is registered anyway and deregistered on the next pass — about five
seconds of exactly the window this document is about.

**B** gives a plugin a way to hold readiness, so a server that has not
finished starting stays `Starting` rather than becoming `Ready` with the door
shut.

They are separable and A is worth doing whether or not B is built. They share
one prerequisite (the event priority below) and one consumer, so they are
specified together.

## A: the gate honours a closed door

```go
case Starting:
    if in.PodExists && in.PodRunning && in.PodReady && in.AgentReady && ... {
        return Decision{Next: Ready, Register: !in.JoinsClosed, ...}
    }
```

Nothing else changes. The `Ready` branch already carries the other direction:

```go
if !in.JoinsClosed && !in.Registered {
    return Decision{Next: Ready, Register: true, Reason: ReasonJoinsOpen, ...}
}
```

so the server is registered on the pass after the plugin opens the door, by
code that exists and is tested.

`JoinsClosed` is `!snap.AcceptingJoins` (`server_controller.go:731`), and the
registry answers `AcceptingJoins: true` for a pod it knows nothing about
(`registry.go:648`). A network that never calls `acceptJoins` is therefore
unaffected: `JoinsClosed` is false, `Register` is `true`, and the decision is
the one it is today.

### What A does not fix

A `Ready` server counts its free slots toward the group's `spareSlots`
whatever its door says: `countsTowardSize` (`candidates.go:300`) asks only
whether the server is leaving or failed. A group whose only server is
`Ready`-with-the-door-shut therefore reads as having capacity nobody can
reach, and builds nothing.

That is already true for a server that closes its door mid-life, and the
`JoinsClosed` documentation is explicit that a closed door is not a lifecycle
event. A inherits the behaviour rather than introducing it, and the window is
seconds. Changing it would mean teaching the scaling arithmetic about the
door, which is a separate decision with its own consequences for a network
that uses `acceptJoins` as intended.

This is the reason B exists.

## B: a plugin can hold readiness

`SpawneryApi` gains one method:

```java
/**
 * Holds this server back from readiness until the returned handle is closed.
 */
ReadinessHold holdReadiness(String reason);
```

`ReadinessHold extends AutoCloseable` with a `close()` that throws nothing, so
it works in try-with-resources and in a callback alike. `reason` is required
and is what a stuck hold is named by.

The agent keeps the open holds. `markReady()` runs when the last one is
released **and** `ServerLoadEvent` has fired — whichever comes second. A hold
taken after readiness has already been reported does nothing but log, because:

`ServerState.markReady` is `compareAndSet(false, true)` and the class states
there is deliberately no way to clear it: `Hello{ready:false}` cannot lower a
readiness the operator's registry has already recorded. B must not weaken
that. A hold is a thing that delays the single transition, never one that
reverses it. A plugin that wants the door shut after readiness has
`acceptJoins(false)`, which is what that method is for.

### The failure mode is already bounded

A plugin that never releases its hold pins the server in `Starting`. The
operator fails it at `-startup-deadline`, five minutes by default
(`main.go:289`), with `ReasonStartupTimeout`. That is the right outcome: a
plugin that never finishes starting is a broken server, and it is reported as
one rather than silently taking players.

The operator's message cannot name the hold, because the operator never learns
its reason -- carrying it would be a proto field spent on one log line. The
agent writes it instead, once, when the server has finished enabling and holds
are still open. That is the moment a reader needs it, and it names the plugin
rather than the symptom.

### Only servers

`ProxyState` has no readiness flag at all — "a proxy's readiness is a
different thing" (`ProxyState.kt:18`). `holdReadiness` is on the server side
of the API and the Velocity agent does not implement it.

## The prerequisite both need

The agent's handler is a bare `@EventHandler`, which is `EventPriority.NORMAL`:

```kotlin
@EventHandler
fun onServerLoad(event: ServerLoadEvent) {
```

A plugin that closes the door (A) or takes a hold (B) from its own
`ServerLoadEvent` handler at `NORMAL` is then ordered against the agent by
plugin registration order, which is luck. The agent moves to
`EventPriority.MONITOR`, so every other handler has spoken before it decides.

`MONITOR` is documented for observers that do not change the outcome, and the
agent does not change this event's outcome — it reads the completed startup.
The alternative, `HIGHEST`, would be a lie about intent and would still sit
before other plugins' `MONITOR` handlers.

## What the network does with it

Not part of this repository, and named so the design is not read as complete
without it: the cyperia `core` plugin takes a hold — or closes the door, under
A alone — and releases it when ViaVersion reports its mapping load finished.

Spawnery must not learn about ViaVersion. The operator has no way to know
which plugins a server runs, the agent has no business special-casing one, and
the next network will have a different plugin with the same shape.

## Not part of this

The readiness probe. A server list ping is what a Minecraft server offers
before anything else answers; asking it to mean "every plugin has finished" is
asking the wrong party. It stays what it is, and `AgentReady` remains the
second half of the gate.

The scaling arithmetic. See "What A does not fix".

`Hello{ready:false}`. Readiness stays a one-way latch.

## Open points

**A alone leaves the capacity wrinkle.** If B is not built, a network using
the door during startup should keep `minReplicas` above one, or accept that
the group will not replace a server nobody can join for those seconds.

**A hold has no expiry of its own.** The startup deadline is the only bound,
and it is per-server rather than per-hold. A per-hold timeout was considered
and left out: it would be the same guess at how long somebody's plugin takes
that this design exists to avoid, and it would turn a broken plugin into a
server that takes players anyway.
