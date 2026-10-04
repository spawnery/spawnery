# /cloud scale, forcestop and execute

**Status:** design, decided 2026-10-04
**Date:** 2026-10-04

## 1. What changes

Three commands for administrators, typed on a proxy:

- `/cloud scale <group> <count> [for <duration>]` holds an ephemeral group at
  exactly `count` servers for a while, from 0 up to its `maxReplicas`.
  `/cloud scale <group> reset` ends that early. It replaces `/cloud start` and
  `/cloud stop`, which only raised a group's floor and could not take a group
  below `minReplicas`.
- `/cloud forcestop <server>` kills a hung server's pod at once, without a
  drain.
- `/cloud execute <server|group> <command>` runs a console command on one
  server or on every server of a group, and shows the result in chat.

## 2. `/cloud scale`

### 2.1 Command

```
/cloud scale <group> <count> [for <duration>]
/cloud scale <group> reset
```

- `count` is absolute: the group has exactly that many servers while the
  pin lives. 0 is allowed; more than the group's `maxReplicas` is refused,
  because a chat command must not lift a ceiling.
- `duration` is a number followed by `s`, `m`, `h` or `d`. Without one the pin
  lasts 1 hour; the longest is 7 days. A pin forgotten at 0 would otherwise
  keep a group off for weeks without anyone noticing.
- Only ephemeral groups. A persistent group is sized by its replica count and
  an on-demand group by requests, so both are refused with that reason.
- `reset` removes every boost on the group, pins and the older additive
  boosts alike.
- Of several live pins on one group the newest wins; they do not add up.

The permission is the existing `spawnery.cloud.scale`.

### 2.2 Resource

`ScaleBoost` gains a mode:

```yaml
kind: ScaleBoost
spec:
  groupRef: {name: lobby}
  mode: Exact        # new; Add (default) | Exact
  replicas: 0
  expiresAt: "2026-10-04T18:00:00Z"
```

- `mode` defaults to `Add`, so every existing boost keeps its meaning.
- A CEL rule bounds `replicas`: at least 1 for `Add`, at least 0 for `Exact`.
- `/cloud scale` creates an `Exact` boost with `generateName`, owned by the
  group, exactly as boosts are created today.

### 2.3 Sizing

`internal/boost` gains `Exact(boosts, group, now) (Pin, bool)`, where `Pin`
carries the replicas and the expiry: the newest
unexpired `Exact` boost of the group (creation timestamp, then name). While one
exists, the group's floor and its ceiling are both that number, and `Add`
boosts do not count. `ScalingInputs` carries it; `floor()` and the ceiling
read it.

Servers above the pinned number go in two ways. Empty ones are deleted, as
when `maxReplicas` is lowered (`SelectDeletionCandidates`, which never takes a
server that may carry players). Occupied ones are retired: the proxies stop
sending anyone there, the players on them stay until they leave, and each
server goes once it is empty. A pin to 0 therefore admits nobody new without
ending a running round; `/cloud forcestop` is the way to end one at once.
Lowering `maxReplicas` itself keeps today's behaviour.

While a pin holds, a rolling update cannot surge above the pinned number. That
is the behaviour of a group at its `maxReplicas`, and the guide says so.

### 2.4 Visibility

`ServerGroup.status` gains `pinnedReplicas` and `pinnedUntil`, both empty
without a pin. `GroupState` gains `pinned` (bool), `pinned_replicas` and
`pinned_until_unix`, so `/cloud info lobby` shows `pinned to 0 until 18:00
UTC`. The public `Group` record carries the same three values, with a
constructor kept for the 0.18 shape.

### 2.5 Wire

A new request, not a field on `BoostRequest`: an operator older than this
release would ignore a new field and turn "exactly 5" into "5 more", while it
refuses a request it does not know.

```proto
message ScaleRequest {
  string group = 1;
  int32 replicas = 2;
  int64 duration_seconds = 3; // 0 = the operator's default
}
message ScaleResult {
  int32 replicas = 1;
  int64 expires_at_unix = 2;
}
```

`reset` uses the existing `StopBoostRequest`.

### 2.6 Plugin API

`SpawneryApi` gains `scale(String group, int replicas, Duration forHowLong)`
and `resetScale(String group)`. `boost` and `stopBoosts` become `@Deprecated`
and keep working.

## 3. `/cloud forcestop`

### 3.1 Command

`/cloud forcestop <server>`, on the proxy only, permission
`spawnery.cloud.forcestop`. It answers at once (`lobby-x7k2 is being
killed`); `/cloud info` shows when the pod is gone. There is no confirmation
prompt: the permission is the safeguard, and an emergency command should not
ask.

### 3.2 Wire and authority

```proto
message ForceStopRequest {
  string server = 1;
  string issuer = 2; // the player who typed it, for the record only
}
message ForceStopResult {
  string server = 1;
}
```

The operator accepts it only from a proxy agent: it disconnects players
without a drain and can lose unsaved world data, so one compromised game
server must not be able to kill its neighbours. The server is looked up in the
proxy's own namespace. An unknown name and a proxy's name are refused.

### 3.3 What it does

`Server.spec.forceStop: true` is set by a patch, like `spec.retire`. In
`phase.Decide` a new input `ForceStopRequested` leads from every phase to
`Terminating`, with no drain and no deadline. The controller deletes the pod
with a grace period of 0, and the Server object goes the way every
`Terminating` server goes. What follows is the group's ordinary behaviour:

- ephemeral: the floor and demand build a new server if one is needed;
- persistent: the missing ordinal is created again, on the same claim and
  world;
- on-demand: the member stays stopped and its world stays, as after
  `stopServer`.

Players on the server lose their backend connection; the proxy's rescue moves
them to the next fallback group.

The server gets an event `ForceStopped` naming the proxy and the issuer, and
the operator logs the same.

## 4. `/cloud execute`

### 4.1 Command

`/cloud execute <server|group> <command>`, on the proxy only, permission
`spawnery.cloud.execute`. The command is the rest of the line without a
leading `/`, at most 256 characters. Completion offers server and group names.

### 4.2 Enabling it

```yaml
kind: Network
spec:
  commands:
    execute: true   # default false
```

Without it the operator refuses every execute request (`execute is not
enabled on this network`). A network that never needs it gains no new attack
surface.

### 4.3 Authority

The operator accepts an execute request only from a proxy agent. Every other
request on the agent channel may come from any agent in the namespace, which
lets one compromised game server retire or boost servers but never run code
on another. A console command on every server would break that, so backends
cannot ask for it, the Paper `/cloud` has no `execute`, and the plugin API has
no method for it.

### 4.4 Flow

1. The proxy sends `ExecuteRequest{target, command, issuer}`.
2. The operator checks the network switch and the caller's role, then
   resolves `target`: a server of that name, otherwise every `Ready` server of
   the group that has an agent session. A named server without a session is
   refused as UNAVAILABLE; group members without one are skipped, and a group
   left with none is refused as NOT_FOUND.
3. For each target it sends `ExecuteCommand{id, command}` down that server's
   stream. This is the first command on the server channel that expects an
   answer; the agent replies with `ExecuteOutcome{id, ok, output, error}`.
4. The Paper agent runs the command on the main thread with a sender from
   `Server.createCommandSender(Consumer<Component>)`: console permissions, its
   feedback collected as plain text, at most 20 lines of at most 256
   characters. `dispatchCommand` returning false is `ok = false` with
   `unknown command`.
5. The operator answers off the proxy session's request loop, so the wait
   cannot hold back the proxy's other traffic. It waits at most 8 seconds, inside the 10 seconds after which an
   agent gives up on a request, and answers `ExecuteResult{outcomes}`. A
   server that has not answered by then is listed with `no answer within 8 s`.

```proto
message ExecuteRequest {
  string target = 1;
  string command = 2;
  string issuer = 3;
}
message ExecuteResult {
  repeated ExecuteOutcome outcomes = 1;
}
message ExecuteOutcome {
  string server = 1;
  bool ok = 2;
  repeated string output = 3;
  string error = 4;
}
```

`ExecuteCommand` and `ExecuteOutcome` also travel on the server stream with
an `id` field.

### 4.5 What the issuer sees

For one server, the outcome and its output. For a group, one line per server
with `ok` or the error and no output, then a total such as `4 of 5 servers
ran it`.

### 4.6 Record

The operator logs network, proxy, issuer, target and command. Each target
server gets an event `CommandExecuted` with issuer and command, never the
output.

### 4.7 Left out

Proxies as targets; a plugin API method; an allowlist of commands. The
network switch and the permission are the safeguards; an allowlist would be a
project of its own.

## 5. Permissions

The `/cloud` root opens for any of seven nodes: `read`, `retire`, `scale`,
`status`, `events`, `forcestop`, `execute`. The guide's node table and the
three places that count the nodes move with it.

## 6. Upgrading

`/cloud start` and `/cloud stop` are removed. `docs/guides/upgrading.md` says
what replaces them. `boost` and `stopBoosts` in the plugin API keep working
and are deprecated.

## 7. Tests

- Go: `boost.Exact` (newest wins, expired ones do not count, `Add` ignored
  while an `Exact` lives); `decideSize` with a pin (floor and ceiling equal
  the pin, 0 included, surplus deleted); `phase.Decide` with
  `ForceStopRequested` from every phase; `answerScale`, `answerForceStop` and
  `answerExecute` with every refusal, the proxy-only rule and the network
  switch; envtest for the `ScaleBoost` CEL rule and the new fields; the
  contract test pinning the new field numbers.
- Kotlin: `parseDuration` with `d`; the command tree (permissions,
  completion, `start` and `stop` gone, `execute` and `forcestop` only on
  Velocity); collecting and bounding command output; the single-server and
  the group reply.
- End to end on real images, each with a mutation that makes it fail: a pin
  to 0 empties the lobby and `reset` brings it back; `forcestop` on a lobby
  server is followed by a new one; `execute` is refused without the network
  switch; with it, `say` runs and its output comes back. The test images
  carry no LuckPerms and the test client holds no permission, so the tests
  issue the commands from the proxy's console, which holds every
  permission. How the test reaches that console is the plan's to settle.

## 8. Docs

The `/cloud` guide gets a section per command, the switch for `execute` and
what it opens. `docs/explanation/agent-trust.md` says what a compromised proxy
can do with `execute` and `forcestop`, and that a backend can do neither.

## 9. Versions

New CRD fields and new requests are a minor step: 0.19.0 for all three
numbers, released as its own PR after the feature, as 0.18.0 was.
