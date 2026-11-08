# Fill routing: sending players to the fullest server so a spare one can empty

**Status:** design, decided 2026-11-08
**Date:** 2026-11-08

## 1. What goes wrong today

An ephemeral group that scaled up does not come back down while players keep
passing through it.

The demand rule in `decideSize` (`internal/controller/scaling.go`) removes a
server only when it has reported zero players for
`scaleDownStabilizationSeconds` (default 300) without a break, the servers
left behind still cover `spareSlots`, and no create is outstanding. The
registry's `emptySince` (`internal/agent/registry.go`) resets on any player.

Every way a player reaches a group picks the emptiest server:

- `Router.choose` (`agent/velocity/.../Router.kt`) takes the candidate with the
  fewest `playersConnected`, as this proxy sees them. It serves the initial
  join, the fallback after a kick, `Drain` and `Rescue`.
- `resolveTarget` (`internal/agentserver/requests.go`) answers a connect to a
  group with the member that has the most free playable slots.

The server a scale-up added is therefore the emptiest one, and it gets every
arrival. It empties only when nobody arrives for the whole stabilization
window. In a lobby group, where most visits last under a minute before the
player moves on to a game, that takes a quiet evening.

Measured on one network over two weeks: a lobby group scaled to two servers
and stayed at two for up to three hours with one to six players in total. In
one such period the second server's longest empty stretch was four minutes.
The median visit to the lobby lasted 52 s, and 87 % of visits were under five
minutes.

## 2. Goals, non-goals, anti-goals

Goals:

- A group can choose to send arriving players to its fullest server that
  still has playable room. A server the group does not need then stops
  receiving players and empties as they leave.
- The existing demand rule removes it, unchanged.
- The choice is per group. Groups that do not opt in behave exactly as today.

Non-goals:

- Moving players who are already on a server to consolidate. A spare server
  empties as its players leave by themselves.
- Changing the demand rule, the `spareSlots` arithmetic or the stabilization
  window.
- Changing the default for any group.

Anti-goals (a failure even if the code works):

- A burst of arrivals, or a drain moving many players at once, pushes a
  server past `maxPlayers` because every choice in the burst read the same
  stale count and picked the same server.
- Two proxies each fill a different server, so neither server empties.
  Per-proxy counts would do exactly that.
- A changeover stalls because fill routing keeps feeding stale servers that a
  `WhenEmpty` update is waiting on to empty.

## 3. Decisions

### 3.1 The opt-in

A new optional field on `ServerGroupSpec`:

```yaml
spec:
  routing:
    strategy: Fill   # or Spread
```

`Spread` is today's behaviour and the default when `routing` or `strategy` is
absent. `Fill` is the new behaviour. The enum sits inside a struct, as
`spec.update.strategy` does, so later routing settings have a place. Changing
it restarts no server; the proxies and the operator's connect path pick it up
at runtime.

The field is allowed on every group type. A `Persistent` group fills the same
way. An `OnDemand` group's members are not reached by routing into the group,
so the field has no effect there and the reference docs say so. The builder
may refuse it on `OnDemand` by CEL instead.

### 3.2 The fill line is `playableSlots`

Fill sends a player to the eligible server with the most players among those
still below their playable slots. The fill line is the server's resolved
playable slots, the figure `spareSlots` and `status.freeSlots` already count
in: the group's `spec.playableSlots`, overridden by a plugin's runtime
`playableSlots(n)`, and every seat up to `maxPlayers` when neither is set.

When no eligible server is below its fill line, the choice falls back to
`Spread`'s rule among servers below `maxPlayers` (most room first), so a full
group spreads its overflow instead of piling it onto one server.

Ties on player count break by server number, lowest first, then by name. The
tie-break exists only so that every proxy and the operator make the same
choice. It has no bearing on which server the demand rule removes later, since
that rule removes only an empty server and fill keeps the emptier one empty.

The seats between `playableSlots` and `maxPlayers` are the buffer for what
fill cannot see: parties arriving together, counts that lag, players picking a
specific server by name. A group that opts into `Fill` without setting
`playableSlots` fills to `maxPlayers` and has no buffer. The guide says so and
recommends setting it.

Setting `playableSlots` also shrinks the capacity `spareSlots` counts in, so a
group that adds it should lower `spareSlots` by the same amount to keep
scaling up at the same player count. A group with `maxPlayers` 40 and
`spareSlots` 20 scales up once a single server holds 21 players; with
`playableSlots` 32 and `spareSlots` 12 it still does. The guide gives this
pairing next to the recommendation.

`enforcePlayableSlots` keeps its meaning. With it set, a server refuses logins
at the fill line, and the overflow rule above places the next player.

### 3.3 The counts are network-wide

Fill reads each server's player count as the network sees it. With two or
more proxies, counting per proxy would let each proxy fill a different server.

Every proxy already has the network-wide count: `NetworkState.servers[].players`
in the mirror. It can be older than a report interval. `netstate.Visible`
blanks player counts on purpose, so a count change triggers no push, and the
proxies see new counts at the `Fleet` resync (`DefaultResyncInterval`, 30 s)
on top of the 5 s report interval: up to about 35 s with default flags. The
operator's own `resolveTarget` builds its picture per request from the cache
and is about one report interval behind.

This design keeps that lag. Fill needs every chooser to agree on which server
is fullest, and a count half a minute old is the same on every proxy. A stale
count can now and then send a player to the emptier server, which delays that
server's emptying by one visit. Pushing every count change to every proxy
would reverse a deliberate load decision for little gain.

The lag must not let a burst overshoot `maxPlayers` (anti-goal 1). Each side
that chooses therefore keeps a tally of the players it has itself sent to a
server and adds it to that server's count:

- Each proxy counts what its `Router` sent. An entry lives until a state
  arrives that is newer than the send by more than one report interval, so
  that its count can include the player. Taking
  `max(network count, local playersConnected)` as the base works as well. The
  exact bookkeeping is the builder's.
- The operator counts what `resolveTarget` sent for `Fill` groups, keyed by
  server, and drops an entry once the registry shows a report taken after the
  move. Without it, a plugin connecting a party of eight to a group in one go
  would put all eight on the fullest server, whose room is only the buffer.

The requirement is anti-goal 1: a burst from one chooser never places more
players on a server than its `maxPlayers` while another eligible server has
room.

Bursts from several choosers at once can still overshoot the fill line by a
few players, which is what the buffer is for. They must not overshoot
`maxPlayers` any more than `Spread` can today.

### 3.4 Every place that picks a server of a group

Fill applies wherever the operator or the proxy agent chooses a server from a
group:

- the proxy's initial join and fallback (`AgentPlugin`, via `Router`),
- `Drain` and `Rescue` (via `Router`). A drain under fill sends players to the
  fullest server below its fill line, and the tally in 3.3 keeps a whole
  drained server from landing on one target.
- a plugin's `connect(player, Target.Group(...))`, answered by
  `resolveTarget` in the operator.

`Router` reads the strategy per group, because its try list can mix groups
that spread with groups that fill. The first group with a candidate still
wins, as today.

Eligibility stays as it is: the same exclusions, join rules, door and
registration checks decide which servers are candidates. Fill changes only
which candidate wins.

### 3.5 Changeovers prefer the current spec

Under `Fill`, a server of the group's current spec beats a stale one before
player counts are compared. Otherwise fill would keep feeding the occupied
stale servers a `WhenEmpty` update is waiting on (anti-goal 3), and a rolling
update would retire servers fill had just filled. The fill rule applies among
current servers, and among stale ones when no current one is eligible.

The proxy needs to know which servers are stale. A new field on `ServerState`
carries it, set from the same comparison `staleSpec` makes. Its name and shape
are the builder's.

The builder also chooses where netstate gets the group's desired hash.
`podspec.DesiredServerHash` is computed only inside the ServerGroup reconcile
and stored nowhere. The preferred source is an in-memory memo that the group
reconcile writes and netstate reads, which keeps the hash out of the public
API. A status field is acceptable if the memo turns out awkward. Recomputing
the pod per group on every state build is not, because `Build` also runs for
every connect request. A spec edit makes servers stale without changing
anything netstate compares, so proxies learn of it at the next resync, up to
30 s later. A changeover takes minutes, so that delay is acceptable.

### 3.6 Wire and version skew

The group's strategy reaches the proxies on `GroupState` in a new field;
absent reads as `Spread`. The stale flag on `ServerState` reads as "current"
when absent.

- An older proxy agent ignores both and spreads. A network that opts a group
  into `Fill` needs proxy images from the release that ships this; the field's
  doc and the guide name it.
- An older operator never sends the field, so a new agent spreads.

## 4. What the change touches

- `api/v1alpha1/servergroup_types.go`: `routing` and its enum, CEL as needed;
  regenerated CRD, chart CRD and `docs/reference/crds.md`.
- `proto/.../agent.proto`: `GroupState` strategy, `ServerState` stale flag;
  regenerated Go and Java.
- Operator: the new fields where `NetworkState` is built; the hash memo of
  3.5; `resolveTarget` honouring `Fill` with the same rule as the proxy, plus
  its tally from 3.3. One shared definition of the ordering would be better,
  but the two sides are Go and Kotlin; matching tables of test cases on both
  sides are enough.
- Velocity agent: `NetworkMirror` carries both fields; `Router` orders by
  strategy; the tally of 3.3.
- Docs: the scaling guide gets a section on fill routing, on why a spare server
  otherwise never empties, and on the `playableSlots` buffer. The plugin API
  docs for `connect` say that a group's routing strategy decides the server.

## 5. How we know it works

- Unit tests for the ordering on both sides: fill picks the fullest below the
  line; ties by number; overflow spreads; current before stale; `Spread`
  unchanged.
- A burst test on the proxy side: N arrivals with an unchanged network count
  never exceed `maxPlayers` on one server while another has room.
- An operator-side burst test: several connects to a `Fill` group before the
  registry shows any of them never exceed `maxPlayers` on one server.
- An end-to-end run in the kind suite with real images and joining clients,
  in the style of `test/e2e/tutorial_test.go` (the scaling scenario in
  `lifecycle_test.go` runs no agents and cannot carry players). A group with
  `Fill`, small `maxPlayers` and `playableSlots`, and two proxies: join enough
  players to scale up, let them leave until the count is below the scale-up
  line while new players keep arriving, and see the spare server reach zero
  players and be removed after the stabilization window. The same run with
  `Spread` keeps the spare server.
- On a real network: a lobby group switched to `Fill` scales back to its
  minimum within roughly the stabilization window after its player count
  drops below the scale-up line, measured from `spawnery_group_servers`.

## 6. Left to the builder

- The tallies' bookkeeping on both sides (3.3).
- The stale flag's name and shape, and memo or status field for the hash
  (3.5).
- Whether `routing` is refused on `OnDemand` groups (3.1).
- Test structure, and how the e2e scenario is split.
