# The `/cloud` command

The agents add an in-game command, so somebody standing on a server can see the
network the operator sees — and, if you let them, change it. It is granted to
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

## The four nodes

| Node | Opens |
|---|---|
| `spawnery.cloud.read` | `/cloud list`, `/cloud info <name>` |
| `spawnery.cloud.retire` | `/cloud retire <name>` |
| `spawnery.cloud.scale` | `/cloud start <group> <count> [for <duration>]`, `/cloud stop <group>` |
| `spawnery.cloud.events` | `/cloud events on`, `/cloud events off` |

**None of them implies another.** Retiring is its own node rather than a level
above reading, because the two are not the same kind of thing: reading is what
you give a moderator so they can see where people are, and retiring changes the
fleet. A permission system that made one imply the other would hand every
moderator the second the day somebody granted the first.

`start` and `stop` share one node in the other direction, and for the matching
reason: somebody trusted to add servers is trusted to take back what they
added, and a grant that let a person start boosts without ending them would
leave them no way to undo their own mistake.

Any one of the four makes the bare `/cloud` root visible. That is deliberate
too — a root demanding `spawnery.cloud.read` would hide the whole tree from
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

`group` is the one to reach for. `server` is a pod name and changes every time a
server is replaced, so it says where a player is rather than granting anything
— and it is filled in only while LuckPerms' own config still says
`server: global`.

Nothing has to be switched on. The contexts appear when LuckPerms is installed
and nothing happens when it is not.

## What each branch does

**`/cloud list` and `/cloud info <name>`** are lookups in the agent's local
mirror of the network. They read memory and nothing else, so they cannot block
the server's main thread, time out, or fail because the operator is
unreachable — which is what makes them safe to run from a chat message. `info`
names a server's group, its phase and whether it is taking joins.

**`/cloud retire <name>`** is the exception: it changes an object in the
cluster. It does not block either — the request is handed off and the answer
arrives when the cluster replies — but unlike the reading branches it can fail
for reasons outside the server it was typed on. Retiring puts a server into the
soft drain described in [Rolling a
group](updates-and-drain.md#retiring-is-not-draining-and-the-difference-is-the-whole-point):
it stops taking joins, and the players already on it are left alone.

**`/cloud start <group> <count> for <duration>`** creates a `ScaleBoost`, which
is the same object [Scaling and boosts](scaling-and-boosts.md) describes. A
boost raised this way is created by the operator rather than by you, which
means it gets an owner reference to the group — so deleting the group takes it
along, which a boost you apply by hand does not. Leave `for` off and the boost
does not expire; `/cloud stop <group>` removes the group's boosts.

**`/cloud events on|off`** turns this player's cloud event feed on or off.

## Both sides, one command

The command is the same on Paper backends and on Velocity proxies. That is a
property of the code rather than of two implementations kept in step: it is
written once, generic in the platform's source type, and nothing in it names
Paper's `CommandSourceStack` or Velocity's `CommandSource`. What a platform can
be asked for is two methods — can this source do the thing, and send it this
text — and nothing else.

Which side a player types it on decides what they are looking at, not what the
command can do.

## Granting nothing is a choice, not an oversight

A network where nobody holds any of the four nodes has no in-game surface at
all, and that is a perfectly reasonable place to stay — everything `/cloud`
does is also a `kubectl` away. The point of the command is the case where it is
not: somebody who should be able to see which servers exist, or add capacity
before an event, without being given a kubeconfig and the cluster access that
comes with it.

The upgrade note for when this command first appeared is in
[Release notes](../archive/release-notes.md#the-agents-gain-a-cloud-command-granted-to-nobody).
