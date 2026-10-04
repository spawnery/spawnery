# Moving players off a draining proxy

**Status:** design, decided 2026-10-01
**Date:** 2026-10-01

## 1. What goes wrong today

A proxy that a roll replaces, that a lowered `replicas` removes, or that an
admin retires takes no new connections and is stopped once it is empty. The
client's connection ends at that proxy, so nothing can move a player off it:
the drain waits for players to leave on their own, bounded only by
`spec.update.maxStaleSeconds`, which disconnects them. An idle player keeps an
old proxy, running old code, for as long as they stay connected.

Since Minecraft 1.20.5 a server can send a client a *transfer*: the client
closes its connection and opens a new one to a host and port it was given,
saying in the handshake that it is a transfer. Velocity (3.3.0 and later;
spawnery ships 3.5.1) exposes it as `Player#transferToHost`, and lets a proxy
store a small cookie on the client before the transfer and read it back
after.

A transfer drops the backend connection too: the encryption ends at the old
proxy, and no session can be handed from one proxy to another. The player sees
a loading screen and the backend sees a quit and a join. It can be put where
the player sees a loading screen anyway — a change of backend server — and
the player can be brought back to where they were.

## 2. The shape

```yaml
kind: ProxyGroup
spec:
  update:
    maxStaleSeconds: 0        # unchanged: the last bound, disconnects
    transfer:                 # new, optional; present = enabled
      forceAfterSeconds: 300  # default 300, minimum 0
```

Unset `transfer` is today's behaviour.

**Enabling it rolls the group once.** A proxy accepts a transferred client only
with `accepts-transfers = true` in `velocity.toml`, which the operator renders
only for a group with `transfer` set; that is part of the rendered config the
group's hash covers. The roll that enabling it causes is done by proxies that
do not have the setting yet, so they drain as before. Every roll after that
transfers. The hash goldens stay unchanged for groups without `transfer`.

The agent learns `transfer` and `forceAfterSeconds` from its pod's environment
(`SPAWNERY_TRANSFER_FORCE_AFTER_SECONDS`, absent when `transfer` is unset),
rendered by `internal/podspec`. That is also covered by the hash, which is
right for the same reason.

## 3. The rules

A proxy transfers only while it is **leaving**: its own entry in the network
picture the operator sends (`NetworkState.proxies`, matched by pod name) has
`draining` set, which `internal/netstate` derives from the draining-since
annotation. A readiness change alone (`SetReady(false)`) never starts a
transfer.

1. **On a change of server, at once.** A player on a leaving proxy who is about
   to connect to another backend (`ServerPreConnectEvent` for a player who
   already has a server) is not connected. The proxy stores the transfer
   cookie naming that target and transfers the client to the address it
   connected with (`Player#getVirtualHost`). Because the leaving pod is not
   Ready, the Service sends the new connection to another proxy.
2. **Forced, after `forceAfterSeconds`.** Counted from the first picture in which
   this agent process saw itself leaving. After it, every player whose
   current server's door is open is transferred, with their current server as the target. A player on
   a server whose door is closed (`AcceptJoins(false)`, a round in progress)
   is never forced; once that door opens and the time is past, they are
   transferred on the next pass. Passes run once a second.
3. **Each player is tried at most once.** A transfer that fails (a client older
   than 1.20.5 — Velocity refuses it — or a connection already closing)
   leaves the player as today, bounded by `maxStaleSeconds` and the drain
   deadline.
4. **Only when somewhere else exists.** The agent transfers only while its
   network picture shows at least one other proxy pod of its own group that
   is Ready and not leaving. Otherwise the transfer would land on nothing.

### 3.1 The cookie

Key `spawnery:transfer`. Value: player UUID, target server name, expiry (60 s
after it is written), and an HMAC-SHA256 over the three. The HMAC key is
derived from the network's forwarding secret (SHA-256 of a fixed label and
the secret), which every proxy of the network already mounts; the agent reads
it from the file named by `SPAWNERY_FORWARDING_SECRET_FILE`, set beside the
transfer variable. A rotation of that secret replaces every proxy with one
that reads the new value, and Velocity holds only one at a time: a cookie
written by an old proxy fails on a new one, and that player is routed as a
fresh join. Rotations are rare and announced; this is accepted.

### 3.2 Arriving

On a connection whose handshake intent is `TRANSFER`, the receiving proxy
requests the cookie before choosing the initial server; a normal join costs
no extra round trip. If the signature holds, the UUID is the player's, the
expiry has not passed and the target is a registered server in its network
picture, the player is connected there — whether or not its door is open:
the old proxy already made that decision, or the player was already on it.
In every other case the player is routed as a fresh join.

## 4. What the operator changes

- `ProxyGroupSpec.Update.Transfer *ProxyTransferSpec{ForceAfterSeconds *int32}`,
  minimum 0, default 300 applied in code when unset inside a present
  `transfer`.
- `internal/render`: `accepts-transfers = true` in `velocity.toml` when
  `transfer` is set.
- `internal/podspec`: the environment variable above.
- `agent.proto`: `ServerState.joins_closed` (bool, false = open; a server that
  never closed its door is open, so agents that predate the field read
  nothing new). `internal/netstate` fills it from the server's status.
  `ProxyState.draining` and `ready` already exist and carry "leaving" and rule 4.
- Old agents ignore the new field and the environment variable, and never
  transfer. A new agent under an old operator gets no environment variable
  and never transfers either.

## 5. What an operator sees

- The guide on updates and drain gets a section: what a transfer is, the two
  moments, closed doors, the client floor of 1.20.5, the enabling roll.
- The agent logs each transfer (player, from-server, to-target, reason:
  `switch` or `forced`) and each refused cookie with its reason.

## 6. Testing

- **Kotlin unit tests (agent):** cookie round trip, wrong UUID, expired, bad
  signature; the
  decision who is transferred when (before the deadline only on a switch,
  after it everyone behind an open door, a closed door shields, a player is
  tried once, nothing while no other Ready proxy exists, nothing while not
  leaving); the receiver's choice (valid cookie → target even behind a closed
  door; invalid or unknown target → normal routing).
- **Go:** render with and without `transfer`; env var; CEL; `joins_closed` from the server status;
  hash goldens unchanged for a group without `transfer`; the proto contract
  test.
- **e2e (tutorial suite, real images):** `spawnery-join` learns to follow a
  transfer (store cookies, reconnect with intent `TRANSFER`, answer the cookie
  request). A client joins and holds; the proxy group (`transfer` set,
  `forceAfterSeconds: 0`) is rolled; the client ends on a new proxy pod and on
  the same backend server it was on. A second case closes the backend's door
  first and asserts the client is still on the old proxy after the deadline.
  Shown to bite by disabling the transfer call in a throwaway worktree.
- **By hand, with a real client:** what the transfer looks like (loading
  screen, how long), once on a switch and once forced. Written up as steps for
  the person running it; the operator cannot see a client's screen.

## 7. Not in this

- A plugin API to hold a single player against a forced transfer. A closed
  door covers rounds; an API comes when a mode needs finer control.
- A per-server-group override of `forceAfterSeconds`.
- Moving players off a draining *backend* without a reconnect: that already
  happens inside one proxy, and is unchanged.

## 8. Addendum, 2026-10-04: what plugins and the rate limit need

Running a network's own proxy plugins against this showed two gaps.

**Plugins cannot tell a transfer from a quit and a join.** Friend
notifications, parties and first-join greetings react to the disconnect on the
old proxy and the login on the new one. `SpawneryApi` gains two local reads:
`leavingByTransfer(uuid)`, true on the old proxy from the transfer until the
last `DisconnectEvent` listener (the agent forgets the player at
`Short.MIN_VALUE`), and `arrivedByTransfer(uuid)`, true on the new proxy for a
player whose cookie verified, until they disconnect. The handshake intent alone
is not offered as the answer because any client can send it. Both are false on
a server.

**A forced pass trips the receiving proxy's login rate limit.** Velocity
refuses a second login from one address within `login-ratelimit` (3000 ms as
spawnery renders it), and with the PROXY protocol that address is the
client's. A pass now admits one player per address, and an address that was
used waits `login-ratelimit` plus 500 ms; a switch from such an address is not
transferred and goes ahead on the old proxy. The limit is read from the proxy's
own configuration, so an overlay that changes it is followed.

Spacing inside one proxy is not enough: a blue/green roll drains every old
proxy at once, and two of them can send players behind one address in the same
second (seen on a local cluster: the second login was refused). The proxies of
a group that are leaving sort their names, and the one at index i sends only
while `(now / spacing) mod 2n == 2i`. Two windows of different proxies are
then at least one spacing apart. A switch outside the proxy's window is not
transferred.

## 9. Addendum, 2026-10-04: forcing only in some groups

A network wanted forced transfers in its hub only: its game servers close
their door during a round, but their waiting lobbies, build worlds and
private servers keep it open, and a player there should move when they next
change server and not before. `spec.update.transfer.forceGroups` lists the
server groups whose players may be forced; empty or unset is every group, as
before. The operator passes it as `SPAWNERY_TRANSFER_FORCE_GROUPS`, comma
separated and absent when empty, so groups without it keep their pod hash.
The agent maps each server to its group from the network picture; a server
it cannot place is not forced. The warning before a forced transfer follows
the same rule. Transfers on a server switch are unaffected.

