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

## The five nodes

| Node | Opens |
|---|---|
| `spawnery.cloud.read` | `/cloud list`, `/cloud info <name>` |
| `spawnery.cloud.retire` | `/cloud retire <name>`, `/cloud unretire <name>` |
| `spawnery.cloud.scale` | `/cloud start <group> <count> [for <duration>]`, `/cloud stop <group>` |
| `spawnery.cloud.events` | `/cloud events on`, `/cloud events off` |
| `spawnery.cloud.status` | `/cloud status [group\|server\|proxy]` |

**None of them implies another.** Retiring is its own node rather than a level
above reading, because the two are not the same kind of thing: reading is what
you give a moderator so they can see where people are, and retiring changes the
fleet. A permission system that made one imply the other would hand every
moderator the second the day somebody granted the first.

`retire` and `unretire` share one node for the same reason: somebody trusted
to retire a server is trusted to take that back.

`start` and `stop` share one node in the other direction, and for the matching
reason: somebody trusted to add servers is trusted to take back what they
added, and a grant that let a person start boosts without ending them would
leave them no way to undo their own mistake.

Any one of the five makes the bare `/cloud` root visible. That is deliberate
too: a root demanding `spawnery.cloud.read` would hide the whole tree from
somebody granted only `spawnery.cloud.retire`, and hide it in the worst
possible way. The branches still gate themselves, so this widens what is
visible and nothing else.

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

**`/cloud start <group> <count> for <duration>`** creates a `ScaleBoost`, which
is the same object [Scaling and boosts](scaling-and-boosts.md) describes. A
boost raised this way is created by the operator rather than by you, so it
gets an owner reference to the group: deleting the group takes it
along, which a boost you apply by hand does not. Leave `for` off and the boost
does not expire; `/cloud stop <group>` removes the group's boosts.

**`/cloud events on|off`** turns this player's cloud event feed on or off.

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

A network where nobody holds any of the five nodes has no in-game surface at
all, and that is a perfectly reasonable place to stay: everything `/cloud`
does is also a `kubectl` away. The point of the command is the case where it is
not: somebody who should be able to see which servers exist, or add capacity
before an event, without being given a kubeconfig and the cluster access that
comes with it.

The upgrade note for when this command first appeared is in
[Release notes](../archive/release-notes.md#the-agents-gain-a-cloud-command-granted-to-nobody).
