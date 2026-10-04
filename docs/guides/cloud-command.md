# The `/cloud` command

The agents add an in-game command, so somebody standing on a server can see the
network the operator sees and, if you let them, change it. It is granted to
nobody by default, which means a moderator who has not been given a permission
node will not find it: it does not appear, it does not answer, it looks exactly
as though it does not exist.

So the first thing to do is grant something. Spawnery ships no permission
system; use whatever your server already runs. With LuckPerms, giving your
moderators the reading half is:

```text
/lp group moderator permission set spawnery.cloud.read true
```

and from then on they have:

```text
/cloud list
/cloud info <name>
```

Everything else is a separate grant, deliberately.

## The seven nodes

| Node | Opens |
|---|---|
| `spawnery.cloud.read` | `/cloud list`, `/cloud info <name>` |
| `spawnery.cloud.retire` | `/cloud retire <name>`, `/cloud unretire <name>` |
| `spawnery.cloud.scale` | `/cloud scale <group> <count> [for <duration>]`, `/cloud scale <group> reset` |
| `spawnery.cloud.events` | `/cloud events [minimal\|normal\|verbose\|off]` |
| `spawnery.cloud.status` | `/cloud status [group\|server\|proxy]` |
| `spawnery.cloud.forcestop` | `/cloud forcestop <server>` (proxy only) |
| `spawnery.cloud.execute` | `/cloud execute <server\|group> <command>` (proxy only, and only with `spec.commands.execute`) |

**None of them implies another.** Retiring is its own node rather than a level
above reading, because the two are not the same kind of thing: reading is what
you give a moderator so they can see where people are, and retiring changes the
fleet. A permission system that made one imply the other would hand every
moderator the second the day somebody granted the first.

`retire` and `unretire` share one node for the same reason: somebody trusted
to retire a server is trusted to take that back.

Pinning a group and resetting it share the `scale` node, for the same reason:
whoever holds a group at a size must be able to let it go again, or a mistake
would stay until somebody with more rights undid it.

Any one of the seven makes the bare `/cloud` root visible. That is deliberate
too: a root demanding `spawnery.cloud.read` would hide the whole tree from
somebody granted only `spawnery.cloud.retire`, and hide it in the worst
possible way. The branches still gate themselves, so this widens what is
visible and nothing else. On a backend the two proxy-only nodes open nothing.

## Where a permission applies

Every Spawnery pod tells LuckPerms what it is, so a rule can name a place:

| Context | Value |
|---|---|
| `group` | the `ServerGroup` or `ProxyGroup` |
| `network` | the `Network` |
| `environment` | `paper` on a backend, `velocity` on a proxy |
| `server` | the pod's own name |

So the moderators above can be given the reading half on the lobbies alone:

```text
/lp group moderator permission set spawnery.cloud.read true group=lobby
```

`group` is the one to reach for. The `group=` at the end of that line is
the Spawnery group, not the LuckPerms group the command names first. `server`
is a pod name and changes every time a server is replaced, so it says where a
player is rather than granting anything. A pod fills it in only when LuckPerms
reported no server name of its own as the server started; set one in LuckPerms'
config afterwards and the servers have to restart before they stop filling it
in.

Nothing has to be switched on. The contexts appear when LuckPerms is installed
and nothing happens when it is not.

## What each branch does

**`/cloud list` and `/cloud info <name>`** are lookups in the agent's local
mirror of the network. They read memory and nothing else, so they cannot block
the server's main thread, time out, or fail because the operator is
unreachable, which is what makes them safe to run from a chat message. `info`
names a server's group, its phase and whether it is taking joins.

**`/cloud retire <name>`** is the exception: it changes an object in the
cluster. It does not block either (the request is handed off and the answer
arrives when the cluster replies), but unlike the reading branches it can fail
for reasons outside the server it was typed on. Retiring puts a server into the
soft drain described in [Rolling a
group](updates-and-drain.md#retiring-is-not-draining-and-the-difference-is-the-whole-point):
it stops taking joins, and the players already on it are left alone.

**`/cloud retire <name>`** takes a proxy's name too. A retired proxy is
replaced first, then takes no new connections and stops once it is empty;
nobody on it is disconnected.

**`/cloud unretire <name>`** takes a retirement back while the server is
still retiring and not yet being stopped: it takes joins again, and it is
held: nothing automatic removes it any more, not a rolling update and not a
scale-down, and it stays until it ends by itself. A server that is already
draining, terminating or finished is refused. `/cloud info` says "held" for
such a server.

`/cloud list` shows each proxy under its proxy group, and `/cloud info
<name>` answers for a proxy: ready or not, draining or not, and its players.

**`/cloud status`** shows how the network is doing: its pods' CPU and memory
against what they requested, one line per group with the lowest TPS among its
servers, and a line for the namespace's other pods. `/cloud status <group>`
lists the group's servers or proxies with TPS, MSPT, usage and age;
`/cloud status <server|proxy>` shows one of them against its requests and
limits. It asks the operator, so it answers only while the agent is
connected.

Usage comes from the cluster's metrics API (metrics-server). Without one the
answer still arrives, with `–` where usage would be. A pod started within the
last minute may not have a sample yet; the totals then say how many pods they
cover. `TPS –` is a server that has not reported a tick rate, such as one
running an agent older than 0.9.0.

Nothing outside the network's namespace is shown. A backend server's answer
leaves private servers and on-demand groups out, as `/cloud list` does.

`/cloud info` and `/cloud status` name the node a server or proxy runs on, or
say it is not scheduled yet. Nothing else about the node is shown.

Every answer opens with a heading and sorts what follows into sections; bars
show players against playable seats (a server whose playable seats differ from
its slots reads `9 / 12 · max 100`), TPS against 20, and CPU and memory against
their limit (or their request where a container has no limit). One-line
answers begin with ✔ or ✘.

**`/cloud scale <group> <count> [for <duration>]`** holds an ephemeral group at
exactly that many servers, 0 included, for the given time. A duration is a
number and a unit, `30m`, `2h` or `3d`; without one the pin lasts an hour, and
the longest is 7 days. A count above the group's `maxReplicas` is refused, and
so is a persistent or on-demand group, which are sized by `spec.replicas` and
by requests. A pin below the group's `minReplicas` is accepted only from a
proxy, so a plugin on a backend can pin within the floor and ceiling but
cannot take a group under its floor. It creates an `Exact` `ScaleBoost`,
described in [Holding a group at a
size](scaling-and-boosts.md#holding-a-group-at-a-size). The operator
creates it, so it has an owner reference to the group and goes with it.

Empty servers above the number are deleted. Occupied ones are retired,
emptiest first: the proxies send nobody new, and a server goes once its players
have left, or at `spec.update.maxStaleSeconds` if the group sets one. A server
you took hold of with `/cloud unretire` stays. While a pin holds, a rolling
update cannot surge above it, the same as for a group at `maxReplicas`.
`/cloud info <group>` shows `Pinned 0 servers until 18:00 UTC`.
`/cloud scale <group> reset` removes every pin and every boost on the group.

**`/cloud forcestop <server>`** kills the server's pod at once. There is no
drain and no confirmation. Players lose their connection and the proxy moves
them to the next fallback group. Unsaved world data is lost. What follows is
the group's own rule: an ephemeral group builds a new server if it needs one, a
persistent group restarts the same ordinal on its claim, and an on-demand
member stays stopped with its world. `/cloud info` shows when the pod is gone.
The server gets a `ForceStopped` event naming the issuer and the proxy.

**`/cloud execute <server|group> <command>`** is off until the Network says so:

```yaml
kind: Network
spec:
  commands:
    execute: true
```

It then runs the command with console permissions on one server, or on every
Ready server of a group. One server answers with up to
20 lines of output. A group answers with a line per server and a total such as
`4 of 5 servers ran it`. A server that stays silent for 8 seconds is listed as
such, and so is one whose agent is not connected, which counts as not having
run it. Feedback a command sends later, from another tick or thread, is not
shown. A proxy is never a target. Each server gets a `CommandExecuted` event
with the issuer and the command, never the output, and the operator logs
network, proxy, issuer, target and command.

Switching it on means that anybody holding the node can run any console command
on any server, with no allowlist, and on most servers that includes `op`.

**`/cloud events`** sets how much of the cloud event feed this player sees in
chat. Without a word it shows the player's current level. `on` means `minimal`,
which is the default, and `off` shows nothing.

| Event | `minimal` | `normal` | `verbose` |
|---|---|---|---|
| a server or proxy appears | `[+] lobby-x7k2` | `[+] lobby-x7k2 starting`; a proxy `[✓] gateway-4d1 ready` | the operator's note |
| a server passes its ready gate | | `[✓] lobby-x7k2 ready` | the operator's note |
| a server or proxy starts to leave | | `[-] lobby-x7k2 leaving` | the operator's note |
| a server or proxy is gone | `[-] lobby-x7k2` | `[-] lobby-x7k2 stopped` | the operator's note |
| a warning or a failure | `[!] lobby-x7k2 did not start in time` | the same | the operator's note |
| anything else | | | the operator's note |

Several lines of one kind in one group within a second become one
(`[+] 5 lobby`). At `verbose` the merged line names the kind and lists up to six
names (`8 PodCreated in lobby (lobby-a, lobby-b, lobby-c, lobby-d, lobby-e,
lobby-f and 2 more)`), and a single event shows as `name: note`. Warnings are
never merged. An on-demand member shows as its
group and the first six characters of its key (`challenge-3f2b1c`). Hovering
over a name shows it in full, and clicking it puts `/cloud info <name>` in the
chat box.

With LuckPerms the level is the player's meta value `spawnery-feed`, read with
inheritance, so `/lp group admin meta set spawnery-feed normal` covers every
admin who has not chosen their own. It survives a roll and a change of proxy
when the proxies share LuckPerms storage, because LuckPerms loads a player's
data from storage at login, so the level follows the player from their next
login on any proxy. Without
LuckPerms the proxy keeps the level in memory until it restarts.

## Both sides, one command

The command is the same on Paper backends and on Velocity proxies. That is a
property of the code rather than of two implementations kept in step: it is
written once, generic in the platform's source type, and nothing in it names
Paper's `CommandSourceStack` or Velocity's `CommandSource`. What a platform can
be asked for is two methods (can this source do the thing, and send it this
text) and nothing else.

Which side a player types it on decides what they are looking at, not what the
command can do.

## Granting nothing is a choice, not an oversight

A network where nobody holds any of the seven nodes has no in-game surface at
all, and that is a perfectly reasonable place to stay: everything `/cloud`
does is also a `kubectl` away. The point of the command is the case where it is
not: somebody who should be able to see which servers exist, or add capacity
before an event, without being given a kubeconfig and the cluster access that
comes with it.

The upgrade note for when this command first appeared is in
[Release notes](../archive/release-notes.md#the-agents-gain-a-cloud-command-granted-to-nobody).
