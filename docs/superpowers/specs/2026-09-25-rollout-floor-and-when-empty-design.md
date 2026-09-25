# A floor for rollouts, and rolling only what is empty

**Status:** design, decided 2026-09-25
**Date:** 2026-09-25

## 1. What goes wrong today

An ephemeral server group rolls like this: `coldStart` builds one server of
the new generation, and once one is Ready, `selectRetirement` puts stale
servers into `Retiring` one at a time (up to `spec.update.maxUnavailable`).
A retiring server is deregistered from the proxies: it takes no new joins,
and its players stay until they leave.

Two things cannot be said today.

**How many servers must stay joinable during a roll.** The only guard is
"one Ready server of the current generation exists". Everything else follows
from capacity: retiring a server removes its free slots, and the spare-slot
rule builds a replacement if the group is now short. A group whose free
slots are covered without the retired server builds nothing, and the number
of servers a player can actually join drops, one retirement at a time, to
one. For a group whose players pick a server (a lobby per game, a server per
map) one open server is not the same offer as three.

**That some servers must not be rolled at all.** Consider a round-based game:
a server fills a lobby, closes its door while a round runs, and ends when the
round does. `Retiring` is wrong for it twice over: a lobby still gathering
players stops receiving any, and `maxStaleSeconds`, if set, moves the players
of a running round off the server. What such a group wants is that an
occupied server is simply left alone, and replaced by the new generation
once it is empty.

## 2. The shape

Both are settings on an ephemeral `ServerGroup`, beside the ones that exist:

```yaml
kind: ServerGroup
spec:
  update:
    strategy: RollingUpdate   # new; RollingUpdate (default) or WhenEmpty
    maxUnavailable: 1         # exists
    maxStaleSeconds: 0        # exists
    minAvailable: 2           # new, optional
```

Unset `strategy` and unset `minAvailable` are today's behaviour, unchanged.
`spec.update` is already refused for persistent and on-demand groups, so both
fields are ephemeral-only without a new rule. Proxy groups are out of scope:
`DecideRollout` already never lets ready pods drop below `replicas`.

### 2.1 Validation

- `minAvailable`: optional, minimum 1, and less than `spec.scaling.maxReplicas`
  (CEL on the spec). A floor equal to the ceiling leaves no room for the one
  extra server the floor needs (§3.2), and the roll could never move.
- `strategy`: enum `RollingUpdate`, `WhenEmpty`, default `RollingUpdate`.
- `strategy: WhenEmpty` with `maxStaleSeconds > 0` is refused (CEL). That field
  actively empties stale servers after a while, which is exactly what
  `WhenEmpty` exists to prevent; accepting both would leave the reader to guess
  which one wins.

## 3. `minAvailable`

### 3.1 Joinable

A server is **joinable** when a player can be sent to it now:

- phase `Ready`,
- registered with the proxies (`Registered`) and its door open
  (`!JoinsClosed`),
- not retiring (`spec.retire`, or a retirement reserved this pass),
- not nominated for deletion (reserved delete) and not condemned by its node.

Stale and current servers count the same. The floor protects what players
see, which is how many doors are open, not which generation is behind them.
A server that shut its door for a running round is not joinable, whatever
its generation.

### 3.2 The rule

`minAvailable` applies while the group changes over (`staleRemains`) and at
no other time. Outside a changeover, `minReplicas` and `spareSlots` govern
size as they do now.

During a changeover:

- **Retirement.** `selectRetirement` nominates a stale server only if the
  group has at least `minAvailable` joinable servers after it. Retiring a
  server that is not joinable (a closed door) lowers the count by zero and
  is not held back. The existing guard (a Ready server of the current
  generation that is staying) stays as it is.
- **Scale-down of stale servers.** The demand rule already deletes only stale
  servers during a changeover. It is held to the same floor: an empty stale
  server that is joinable is not deleted if that would leave fewer than
  `minAvailable` joinable. Without this, the floor could be walked past by
  the other door.
- **The extra server.** When retirement is declined *only* because of the
  floor (budget free, a staying current server exists, a stale candidate
  exists), `decideSize` creates one server of the current generation, provided:
  no create is pending, no server of the current generation is still starting,
  and `alive < maxReplicas`. One at a time: once it is Ready the floor admits
  the next retirement, and the cycle repeats. The ordinary scale-down removes
  any surplus after the changeover.

A group already below the floor before its changeover (fewer joinable servers
than `minAvailable` because demand never asked for more) gets the same extra
server: the floor is reached by building, never by refusing to start.

### 3.3 When it cannot move

If the extra server cannot be built (the ceiling is reached, or new servers
keep failing and the backoff holds the create), the changeover waits. That is
visible: the group's `Progressing` condition carries reason
`WaitingForMinAvailable` with a message naming the joinable count and the
floor. The ceiling case also sets `ScalingLimited`, as a refused cold start
does today.

## 4. `strategy: WhenEmpty`

### 4.1 The rule

`selectRetirement` considers only stale servers that are **known empty**:
`Players == 0` and the count trusted (`!Stale`). Occupied stale servers are
never retired, never drained, and never counted as candidates; they stay
`Ready`, registered, and joinable for as long as they have players.

An empty stale server is retired as soon as the ordinary guards allow
(`maxUnavailable`, the staying current server, and `minAvailable` if set).
Being empty, it goes away at once, and the spare-slot rule builds a current
one if the group needs it.

How a server becomes empty is the game's business: players leave, a round ends
and the plugin sends them elsewhere, or the plugin stops the server process.
In the last case the container restarts (`RestartPolicy: Always`) and the
server comes back empty and stale, and is retired like any other empty one.

Unchanged under `WhenEmpty`:

- a node drain still condemns and deletes the group's servers; the node is
  leaving either way;
- the demand rule still removes empty stale servers first;
- `maxUnavailable` still bounds concurrent retirements;
- `minAvailable` still applies, per §3.

### 4.2 The changeover budget

A group holds a network changeover place from its first current server until
its last stale one is gone. Under `WhenEmpty` that window is as long as the
longest round, and a place held for it would stall every other group of the
network behind a game in progress.

So a `WhenEmpty` group holds a place only for its cold start:

- `Waiting` and `Begun` mean what they mean today until the group has a
  current-generation server that is Ready.
- From then on it reports a new state, `Deferred`: stale servers remain, but
  they are waiting for their players, and the group holds no place.
  `Deferred` groups are neither holders nor waiters in `AdmitChangeovers` and
  in the network gauges.

A `Deferred` group can still build: the spare-slot rule answers demand as
always, and §3.2's extra server may be built without a place. Both cost at
most one server beyond demand, the same trade the budget already accepts for
demand in a waiting group.

## 5. Status and docs

- `ServerGroup.status.changeover` gains `Deferred`.
- New condition reason `WaitingForMinAvailable` on `Progressing`.
- `docs/guides/updates-and-drain.md` gets a section on the floor and one on
  `WhenEmpty`, with the round-based example; the CRD reference is regenerated.
- Release notes name both fields and the new state.

## 6. Testing

- **Decision tables** for `selectRetirement` and `decideSize`:
  retirement declined at the floor, allowed above it; a closed-door server
  retired without touching the floor; the extra server built once, not while
  one is pending or starting, not beyond `maxReplicas`; a group below the
  floor before its changeover; the demand rule held to the floor during a
  changeover and not outside one; `WhenEmpty` skipping occupied and
  untrusted-count servers and retiring an empty one; `WhenEmpty` with the
  floor.
- **Changeover state:** `ownServerChangeover` returns `Deferred` for a
  `WhenEmpty` group with a Ready current server and occupied stale ones;
  `AdmitChangeovers` treats it as neither holder nor waiter.
- **envtest** for the three CEL rules (floor at the ceiling refused, below
  accepted; `WhenEmpty` with `maxStaleSeconds > 0` refused).
- **e2e on kind:** a `WhenEmpty` group with a player on a server is edited;
  the occupied server stays `Ready` and registered while an empty sibling is
  replaced; after the player leaves, the old server goes and a current one
  stands in its place. A `minAvailable: 2` group rolls without the joinable
  count dropping below 2.

## 7. Not in this design

- A plugin call by which a server protects itself from a roll. `WhenEmpty`
  reads occupancy, which a running round always has; a per-server signal can
  be added on top if occupancy ever turns out not to be enough.
- Proxy groups, persistent groups, on-demand groups.
- A floor outside changeovers. Open doors during normal operation are the
  spare-slot rule's job, which already stops counting a closed server's seats.
