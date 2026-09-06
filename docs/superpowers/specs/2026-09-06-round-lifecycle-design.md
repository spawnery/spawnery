# A round the operator can see start and end

## Goal

A minigame group runs one round per server, and the operator can see neither
end of one. Two faults follow, and they are the same blind spot from opposite
sides.

A round that **starts** takes its server out of the routing table. The plugin
calls `acceptJoins(false)`, `JoinsClosed` follows, and `phase.go` deregisters
it. Nobody can reach that server any more — including the spectator who wanted
to watch. Meanwhile its empty seats are still counted as capacity by the rule
that decides whether to build another server, so the group believes it has room
and adds nothing. The players see a full network; the scaler sees an empty one.

A round that **ends** is invisible. The server shuts down, and kubelet lifts the
same pod again with the same `emptyDir` and therefore the same world. The
operator never observes a terminated pod, so it never deletes the `Server`, and
the group never replaces it with a fresh one.

## Decisions

| | |
|---|---|
| Closed door | stops counting as capacity, stays in the routing table |
| Leaving the routing table | a second, separate signal on the agent channel |
| Which signal a round start sends | the door only |
| Which signal a round end sends | the table one; the door is already shut |
| Ephemeral `restartPolicy` | `Never`, so a finished pod stays finished |
| Persistent `restartPolicy` | unchanged, `Always` |
| How a clean end is told from a crash | whether the server sent the table signal, not the exit code and not the announcement |
| Clean end | a new terminal phase, `Finished` |
| Where that word is kept | `status.roundEndedAt` on `Server`, stamped while it still runs |

## Why one signal was never enough

`AcceptJoinsRequest` carries one bool and is made to do two jobs. Its own
documentation describes the first: no new players are routed here, and
**nobody already on it is moved**. That is a capacity statement. The operator
then acts on it by deregistering, which is a reachability statement, and the
two are not the same claim.

They come apart the moment somebody wants to reach a server on purpose. A
spectator, a `/join <friend>`, a selector click on a running round — each names
a server that is deliberately not taking the general public. Deregistration
answers all of them with "no such server".

So the door stays a door: `accept=false` means *do not count my seats and do
not send me the next player looking for a game*. A second signal says *take me
out of the table*, and only shutdown and the end of a round use it.

`ProxyGroup.spec.routing.fallbackGroups` is what makes this safe. A proxy's
fallback is an explicit list of groups, not everything registered, so a server
that stays in the table while a round runs is not somewhere a player can be put
by accident. Reaching it takes naming it.

## The two capacity numbers both learn the door

There are two, deliberately, and `provisionalCapacity` says why: `status.
freeSlots` describes Ready servers of the current generation, while the
scale-up rule needs capacity that has been ordered but has not arrived, or it
orders the same replacement six times over. Two numbers, two purposes; that
stays.

What they must share is what counts as reachable.

`AggregateGroup` gets it right today, and its comment names this exact case:
free seats on a server no proxy will send anybody to are not capacity, and
counting them would let a group sit at its floor while every server in it had
shut its door. It reaches that answer through `v.Registered`.

`provisionalCapacity` asks only `countsTowardSize()`, which knows `leaving()`
and `Failed` and nothing about doors. A server two minutes into a round with
2 of 80 seats taken contributes 78 places nobody can occupy. With
`oneblockrace-solo` at `spareSlots: 80` and two servers, one running round
still sums to 158 and the group builds nothing.

After this change `Registered` is no longer a usable proxy for reachability —
a closed server stays registered on purpose. So **both** functions read
`JoinsClosed` outright. `AggregateGroup` changes with `provisionalCapacity` or
it starts counting the seats it was written to exclude.

The intent this restores is already written down in the groups themselves:
almost every game group sets `spareSlots` equal to `maxPlayers` — Bingo 80/80,
Challenge 100/100, Ragemode 12/12. That is a request for one whole free server
at all times, and it cannot be honoured while a running round is credited with
a server's worth of seats.

## Why a phase and not a reason

With `restartPolicy: Never`, a finished round reaches `phase.Decide` as a
terminal pod, and today every terminal pod becomes `Failed`. Left there, normal
play would be recorded as failure:

- `CountFailures` counts any `Failed` server whose `FailedAt` is newer than the
  streak, and six consecutive failures end a group's attempts for good — a
  give-up only a spec edit clears. A group at `maxReplicas` with no room for a
  replacement never gets the Ready server that would break the streak.
- `failedRetentionSeconds` defaults to 3600, so every ended round leaves a
  corpse for an hour, for a diagnosis nobody needs.
- Every ended round raises `Degraded`.

A reason string on `Failed` would fix the counting and leave every status
output claiming a failure that did not happen. This repository models a
server's life in phases and prints that phase; `Finished` is what actually
happened, and `Phase` is a plain `string` in the CRD with no enum to migrate.

`Finished` behaves like `Failed` everywhere the group's arithmetic is
concerned: deregistered, outside `countsTowardSize`, so the replacement is
ordered in the same pass. It differs in three places — it is not counted by
`CountFailures`, it raises no `Degraded`, and it has its own short retention
instead of the hour.

## Why the server's word and not the exit code

A round that ends and a crash that was tidied up both leave a process that
stopped. The exit code distinguishes them only as well as the game plugin
happens to exit, and a `System.exit(0)` in a shutdown hook would make a crash
indistinguishable from a win.

The server can say which it is, and this is the line `JoinsClosed` already
draws: the server's own word and not the operator's.

**The word is the new verb, not the announcement.** `setEnding()` already
publishes the state `ending`, and reading that would be the shorter road. It is
closed on purpose. `AnnounceRequest` says the operator reads none of it —
nothing there reaches scheduling, routing or scaling — because a field the
operator acted on would need a schema, a validation error path and a version
story, while a field it only carries needs a length bound. It says the second
half too: the announcement is not the phase, and an agent cannot write one.
Deciding `Finished` from a free-form string would break both sentences at once.

So the round's end travels as the verb this design is adding anyway. A server
that takes itself out of the routing table is at the end of its round: a
rolling update deregisters through `retire`, which the operator initiates, so
nothing else has cause to send it. The operator deregisters and stamps
`status.roundEndedAt` in the same step.

That stamp lands while the server is still running, which is also what makes it
survive an operator restart between the pod ending and the reconcile that reads
it. The phase decision reads the object, never the registry — the registry is
memory, and a restart would otherwise turn a clean round into a `Failed` one.

`state = ending` stays exactly as it is: published by the plugin, carried to the
other agents, read by nobody in the operator.

## What travels

| | |
|---|---|
| `AcceptJoinsRequest` | second field for "take me out of the table"; unset means in it, so an older agent keeps its place |
| That same field | the operator's only source for a round's end — typed, so it may be acted on where the announcement may not |
| `Server.status.roundEndedAt` | new, `*metav1.Time` |
| `phase.Inputs` | `RoundEnded bool` |
| `phase.Phase` | new terminal value `Finished` |
| `ServerGroupSpec` | retention for a finished server, separate from `failedRetentionSeconds` |

## Compatibility

The meaning of `accept=false` narrows for every agent, including ones already
deployed: they stop being deregistered when they close their door. That is the
wanted direction — it is what lets a spectator in — but it is a behaviour
change nobody opts into, and it belongs in the release notes rather than in a
footnote.

Nothing else moves for an agent that never sets the new field. A server that
has said nothing stays in the table, exactly as one that has never closed its
door stays open.

## Testing

The capacity half is arithmetic and belongs in unit tests: `decideSize` and
`AggregateGroup` against a view whose door is shut, asserting that its seats
are not counted and that the group orders a replacement.

The phase half is a pure function too — `phase.Decide` with a terminal pod,
with and without `roundEndedAt` — plus `CountFailures` proving a `Finished`
server does not spend the backoff budget.

Deregistration and the round-end-to-replacement path need envtest.

In the real network the two halves differ:

- The **round end** can be verified in cadev. End a round, watch a new pod
  arrive with a fresh world. `maxReplicas: 1` does not get in the way.
- The **scale-up** cannot. cadev renders every group with `maxReplicas: 1`,
  because every server of a group mounts the same files claim and a Minecraft
  level can be opened by exactly one server. Verifying it needs paulwtf, or
  cadev needs an answer of its own for a group that may hold more than one
  server. That question is open and belongs to the plan, not here.

## Not part of this

A server that takes itself out of the table and then does not exit hangs,
before this change and after it. Bounding that is a deadline on the signal and
a separate
piece of work.

Persistent groups keep `restartPolicy: Always`. A persistent server's world is
a claim rather than an `emptyDir`, its identity is its ordinal, and nothing
about a round's end applies to it.

## Open points

- The retention default for `Finished`. Zero deletes it as soon as the
  replacement is ordered, which is tidy but leaves no window to look at what
  the last round did. A short non-zero default may be the better answer.
- Whether the table signal is a second field on `AcceptJoinsRequest` or a verb
  of its own. One message keeps the door and the table together, where a reader
  finds both at once; two messages say plainly that they are different claims,
  which is the whole argument of this design.
