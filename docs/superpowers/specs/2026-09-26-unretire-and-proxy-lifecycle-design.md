# Unretire, and proxies an admin can see and retire

**Status:** design, decided 2026-09-26
**Date:** 2026-09-26

Three changes go out in one release. The first is already on the branch and
needs no design of its own; the other two are this document.

- **A — a finished round shuts down quietly** (commit `fix(phase): a server
  whose round ended shuts down as Finished`). A server that called `endRound`
  and then loses a ready signal goes straight to `Finished` instead of
  `Ready → Starting` with a `ReadinessLost` event, which the in-game feed showed
  as a server that missed a ready signal. A silent agent keeps the old path,
  because its players may still need the rescue drain.
- **B — unretire** (§1).
- **C — proxies in the network picture, the feed and `/cloud retire`, and a
  proxy drain that waits instead of disconnecting** (§2).

## 1. Unretire

### 1.1 What goes wrong today

`/cloud retire` and the plugin API's `retire(server)` put a server into soft
drain, and nothing takes that back. A server retired by mistake, or retired by
a roll while a group of players is in the middle of something, can only run
empty. `Retiring` is one-way by design (`TestNoPathBackFromRetiring`), and a
rolling update would retire a stale server again on the next pass even if it
could come back.

### 1.2 The shape

- A new agent request, `UnretireRequest{server}`, answered by
  `UnretireResult{server}`, in `CloudRequest`/`CloudResponse` beside
  `RetireRequest`.
- Plugin API: `CompletionStage<Void> unretire(String server)` on both
  platforms, next to `retire`.
- `/cloud unretire <name>`, gated by `spawnery.cloud.retire`. Somebody trusted
  to retire a server is trusted to take it back; the guide already applies the
  same reasoning to `start` and `stop` sharing one node. Suggestions offer the
  servers that are retiring.
- A new field on `Server`: `spec.hold` (bool, default false).

### 1.3 What the operator does

The writer handles `UnretireRequest` like `RetireRequest`: namespace-bound,
by name, one patch.

- **Refused** when the server is `Draining`, `Terminating`, `Finished` or
  `Failed` ("already stopping"), and when it is neither retiring nor carrying
  `spec.retire` ("not retiring"). The refusal is written for a person, as the
  retire refusals are.
- **Accepted** otherwise: `spec.retire: false`, `spec.hold: true`. A server
  that already has `spec.hold` and no `spec.retire` is refused as not retiring,
  so a repeated command says what happened.

### 1.4 Phase

`Retiring` gains exactly one way back: when retirement is no longer requested,
the pod is running and both ready signals hold, `Retiring → Ready` with
`Register` (reason `RetirementWithdrawn`, "retirement was withdrawn").
Otherwise it stays `Retiring` and the existing branches apply, including
"no players left", so an unretire that arrives after the server ran empty
changes nothing. `status.retiringSince` is cleared on the way back.

`TestNoPathBackFromRetiring` stays true for every server whose retirement is
still requested, which is the case it was written for.

### 1.5 Held servers

`spec.hold` means: nothing automatic removes this server. It stays until it
ends by itself.

- `selectRetirement` never nominates it.
- The demand rule and the ceiling never nominate it for deletion
  (`deletable()` leaves it out).
- It does not count as stale for the changeover: not in `staleRemains`, not in
  `coldStart`'s stale count, not in `ownServerChangeover`. A group whose only
  stale servers are held has finished its changeover and holds no budget
  place.
- It still counts toward the group's size and capacity like any server.
- A node drain still condemns it; the node leaves either way.
- A manual `retire` still works and clears nothing else: a held server that is
  retired again drains like any retired server.

`/cloud info` shows "held" for such a server; `ServerState` gains
`bool held`, and `ServerInfo.held()` exposes it.

## 2. Proxies

### 2.1 What goes wrong today

Proxies are pods of a `ProxyGroup` with no per-proxy object. The network
picture carries servers and groups but no single proxy, so `/cloud list` and
`/cloud info` cannot show one and `/cloud retire` cannot name one. Starting and
ordinary stopping of a proxy pod record no event, so the feed never shows
them. And a proxy that a rollout or a scale-down drains is deleted once its
`spec.drain.timeoutSeconds` has passed, disconnecting whoever is still on it,
where a server's retirement never disconnects anybody.

### 2.2 In the network picture

`NetworkState` gains `repeated ProxyState proxies`:

```proto
message ProxyState {
  string name = 1;     // the pod
  string group = 2;    // the ProxyGroup
  bool ready = 3;
  bool draining = 4;   // not taking new connections
  int32 players = 5;
}
```

Plugin API: `List<ProxyInfo> proxies()` and `Optional<ProxyInfo> proxy(name)`.
`/cloud list` lists a proxy group's proxies under it; `/cloud info <name>`
answers for a proxy too, and its suggestions include proxy names.

### 2.3 In the feed

The proxy group controller records events regarding the **proxy pod**, and
`cloudevent.Derive` accepts a pod carrying the proxy role label: the subject is
the pod's name, the group its `spawnery.cloud/group` label. Game pods stay out,
as today.

| Reason | Type | When |
|---|---|---|
| `ProxyStarted` | Normal | the pod passes the ready gate for the first time |
| `ProxyRetiring` | Normal | the pod is first marked draining; the note says why: rolling update, retire requested, scaled down, node leaving |
| `ProxyStopped` | Normal | the pod is deleted because it ran empty |
| `ProxyDrainTimeout` | Warning | unchanged: deleted at its deadline with players on it |

Each fires once, on the pass that changes the pod, like the existing
`NodeDraining` event.

### 2.4 Retiring a proxy

`RetireRequest` keeps its wire shape. The writer looks the name up as a
`Server` first, as today; if there is none, it looks for a proxy pod of that
name in the namespace and sets the annotation
`spawnery.cloud/retire-requested` (an RFC 3339 time). A proxy already carrying
it, or already draining, is refused as already retiring. Neither exists:
"no server or proxy called <name>".

The proxy group controller treats a pod with that annotation as stale in
`DecideRollout`: it counts toward the surge, it is picked first among stale
pods, and it is drained one at a time behind a ready replacement, like a pod
of an older generation.

### 2.5 Drain without disconnecting

A draining proxy is deleted once it is empty, and a deadline disconnects the
players left on it only in two cases:

- **its node is leaving** (`spec.drain.timeoutSeconds`, as today), and
- **`spec.update.maxStaleSeconds`** on the `ProxyGroup` has passed since it
  started draining, for a group that wants rolls bounded. `ProxyGroup` has no
  `spec.update` today; the block is new, optional, with this one field
  (minimum 0, default 0 = never), named as on `ServerGroup` so the two read
  alike.

A rollout, a scale-down and a retire otherwise wait: the proxy takes no new
connections and is stopped once empty.

One exception keeps a proxy from waiting forever on a number nobody can read:
while its player count is unknown (the agent stream is down or its report is
stale), `spec.drain.timeoutSeconds` still applies. The soft wait is for players
the operator can see.

This is a change in behaviour for every proxy group: a roll that used to
finish after `drain.timeoutSeconds` now waits for the proxy to empty. Release
notes say so, and name `spec.update.maxStaleSeconds` as the way back to a
bounded roll.

## 3. Versions

The agents, the Java API and the proto change, so this release moves
`imageVersion` as well as the operator and the chart: a minor step for all
three by the rule of 2026-09-08. Existing objects validate unchanged; the new
fields are optional.

## 4. Testing

- **phase:** table cases for `Retiring → Ready` on withdrawal (healthy only),
  staying `Retiring` while requested, running empty wins over withdrawal.
- **scaling:** held servers are never retired, never deleted by demand or
  ceiling, do not keep a changeover open; a node drain still condemns them.
- **writer:** unretire refusals (stopping phases, not retiring, repeated) and
  the accepted patch; retire resolving a proxy pod, refusing a draining one,
  naming both kinds when neither exists.
- **proxy rollout:** a retire-requested pod is drained behind a surge; a
  draining proxy with known players is not deleted at `drain.timeoutSeconds`;
  it is at the deadline with an unknown count, on a leaving node, and after
  `maxStaleSeconds`; it is deleted once empty.
- **events:** each proxy event once per change; `Derive` maps a proxy pod and
  ignores a game pod.
- **agent (Kotlin):** `/cloud unretire`, proxy lines in `list`/`info`, proxy
  names in suggestions; the Java API surface on both platforms.
- **envtest:** unretire end to end through the reconciler (Retiring with a
  player, unretire, back to Ready and registered, not retired again by the
  roll); a retired proxy replaced and deleted once empty.
- **On the live network after release:** unretire a retiring server with a
  player on it; retire a proxy and watch the feed.

## 5. Not in this design

- Unretiring a proxy. A proxy is fungible; a retired one that should stay can
  be answered by scaling the group.
- Moving players between proxies (transfer packets). Draining waits for them
  instead.
- A time limit on `spec.hold`.
