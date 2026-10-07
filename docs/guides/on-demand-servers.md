# Private servers: one world each, asked for by name

An on-demand group in full. It joins the Network of
`config/samples/network.yaml`; apply that first if the namespace has none. Save
this as `private-servers.yaml`:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: private-servers
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: OnDemand
  image: ghcr.io/spawnery/purpur:26.3-0.24.3
  maxPlayers: 10
  # A fleet ceiling, not a per-player quota: who may have one is a question
  # about a player, and the system that knows the player answers it.
  maxInstances: 200
  storage:
    size: 2Gi
```

```bash
kubectl apply -f private-servers.yaml
kubectl get servergroup private-servers -n minecraft
```

Nothing starts: an `OnDemand` group is a template and has no size. `spec.scaling`, `spec.replicas` and `spec.update` are refused
for it, and `spec.storage` and `spec.maxInstances` are required. A server exists
only because a plugin asked for it, and it is one player's own. `Ephemeral`
groups answer *how many*; this one answers *which*.

A `spec.joinPermission` on an on-demand group is checked by the proxies only: a member's server does not see its own group.

## Asking for one

The caller is a plugin, on either side of the proxy, over the same channel
`retire` and `boost` use. `key` is whatever your own system calls the world,
such as an instance id from a database:

```java
SpawneryApi api = Spawnery.api();
String key = instance.uuid().toString();

api.startServer("private-servers", key)
   .thenAccept(s -> getLogger().info(s.name() + (s.alreadyRunning() ? " was up" : " asked for")))
   .exceptionally(e -> { getLogger().warning("no server: " + e); return null; });

// later, when its owner is done:
api.stopServer("private-servers-" + key);
```

The server it names is `<group>-<key>`, so this one is
`private-servers-0b5c1c82-4c7f-4a6e-9d1b-2c1f2b9d5e10`, and its world is the
claim `<group>-<key>-data`. `startServer` returns that name in
`StartedServer.name()`, and `stopServer` takes it; compose it yourself only if
you have never received it. **Two bounds fall out of Kubernetes and are checked
before anything is created:** the key has to be a DNS label (lowercase letters,
digits and `-`), and the composed name has to fit 63 characters. A 36-character
UUID leaves 26 for the group's own name, which is roomy for `private-servers`
and is written here because the failure lands on a player's start, not on the
`kubectl apply` that could have prevented it.

**The stage completing means the server was asked for, not that it can take
players.** It starts out as any server does. Wait for `ServerInfo.phase()` to
say `READY`, or for the event that says so, before sending anybody there, and
send them from a proxy (see [below](#who-sees-them)).

What comes back, by reason:

| Call | Answer | When |
|---|---|---|
| `startServer` | `already_running` set | the key is up. A player pressing a button twice needs no lock on your side. It says nothing about readiness. |
| `startServer` | `UNAVAILABLE` | the member of that key is still stopping, or the group is at `maxInstances` only because a member is draining. Neither is a refusal: ask again. |
| `startServer` | `REFUSED` | the key is not a label, the name would not fit, the group is not `OnDemand`, another group's server already holds the name, or the group is at `maxInstances` with nobody leaving. |
| `startServer` | `NOT_FOUND` | no such group. |
| `stopServer` | `REFUSED` | the server is not a member of an on-demand group. |
| `stopServer` | `NOT_FOUND` | no such server, including a stop that was carried out a moment ago. |
| `deleteServer` | `REFUSED` | the group is not `OnDemand`, the key is not a label, a claim of that name was not made by this operator for that group, or the world predates the key label (only an admin can delete it). |
| `deleteServer` | `NOT_FOUND` | no such group, or the key has neither a server nor a world, including a delete that finished a moment ago. |
| `startServer` | `UNAVAILABLE` | also: the key's world is still being deleted. |

Every failure reaches Java as an `IllegalStateException` whose message is
`<REASON>: <the operator's sentence>`; the Javadoc says when it arrives wrapped.

**`stopServer` deletes a server**, and it is the only call on this channel that
does. What keeps a wrong name from taking down a lobby is that it checks the
`Server` carries a key, which only members of an on-demand group do: a lobby's
name gets `REFUSED`. That check is the bound. **Who may call is who may
install a plugin in the namespace**, the same boundary
[the plugin API](../plugin-api/index.md#what-it-can-see) describes for every
call, which is why `maxInstances` below is required rather than defaulted.

## The ceiling is a fleet's, not a player's

`spec.maxInstances` is how many members the group may have at once. It counts
members that are not over: a `Failed` world kept for diagnosis and a `Finished`
one waiting to be swept do not count against it, and a member that is draining
does, because it still exists. `0` is legal and closes the group: new starts
are refused and every world stays where it is, which is the state an incident
wants and a deletion would not give.

It is not a quota. *Who* may have a private server, and how many, is a question
about a player, a purchase and a ban, and the system that holds those is the one
that answers it. An operator that enforced it would need all three.

## Stopping keeps the world; deleting removes it

`stopServer` deletes the `Server`, and everything after that is the path a
scale-down already takes: the players on it are moved through the proxies inside
`spec.drain.timeoutSeconds`, the pod goes, and the claim stays. There is no
"save the world" step because the world was never anywhere else. The next
`startServer` with the same key mounts the same claim.

**A stop never deletes a claim**, and neither does deleting the group. That has
a price in this type that a persistent group does not pay: a group of `Persistent`
servers has as many claims as `spec.replicas`, and one of these has as many as
players who ever asked. Every claim starts at `spec.storage.size` (see
[Claims that grow by themselves](persistent-worlds.md#claims-that-grow-by-themselves)),
whether or not its owner comes back. What a claim costs, how to find the ones nobody is using and
why removing one is a human act are in
[Persistent storage](persistent-worlds.md), and all of it holds here unchanged;
the claims of this group are named `<group>-<key>-data` and carry the same
labels.

What a start does to a claim's content is the same too: it adds to it. A group
that wants stale plugins, configs and worlds gone on every start lists what
survives in `spec.storage.keep`; see
[What survives a start](persistent-worlds.md#what-survives-a-start).

`deleteServer(group, key)` deletes an instance for good: a player's own action,
with nothing left behind. A running member is stopped first, exactly as
`stopServer` stops one; its claim is deleted at once, and Kubernetes keeps it
until the pod no longer mounts it. A stopped member is only its claim, which is
why the call takes the group and the key rather than a server name. There is no
undo: the volume goes with the claim under the usual `Delete` reclaim policy.
A `startServer` of the same key answers `UNAVAILABLE` while the world is going
and starts an empty one afterwards.

This is the one place the operator deletes a claim, and it holds `delete` on
claims cluster-wide for it, because RBAC selects by name and these names are
minted at runtime. What narrows the right is the chart's
`ValidatingAdmissionPolicy` `spawnery-world-deletion`: as the operator's
ServiceAccount, the API server lets through only the deletion of a claim that
carries this operator's `spawnery.cloud/managed-by`, an on-demand member's
`spawnery.cloud/key`, and the name `<group>-<key>-data`. A persistent server's
world, a database's claim or anything else in the namespace is refused there,
whatever the operator's code does, and even with its credentials. The policy
needs Kubernetes 1.30.

The same policy refuses the operator any change to those three labels, so a
key cannot be added to a claim later. A world created before the key label
existed can therefore only be deleted by hand; `deleteServer` refuses it and
says so.

### Other ways a member ends

- **Its own run ends.** A player typing `/stop` is a player stopping their
  server, and a member's pod is never restarted for it. Once the run is over the
  object is deleted at once (`Finished`): the world is on the claim, and the
  object would only hold the one name its owner needs to start again. A member
  that ended in `Failed` is kept, under the cap of one per group that an
  ephemeral group's failures are kept under, so that a world that broke can be
  looked at; a start on that key replaces it rather than being refused by it,
  unless it is still held by its drain; then the start answers `UNAVAILABLE` and
  asking again a moment later works.
  The cap is per group and not per key, so a second player's broken world
  removes the first one's `Server` (the object only, never the claim).
- **Its node drains.** A member on a node that is leaving goes like any other
  server on it, players moved first. Nothing recreates it. The world is on its
  claim, so the key is free and its owner starts it again as they did the
  first time.

**And one way a member does not end: being left alone.** There is no idle
timeout here, and nothing reaps a member because it is empty. A world whose
owner closed the game without typing `/stop` keeps running, and keeps its slot
against `spec.maxInstances`, for as long as nobody asks it to stop. This follows
the same line as the rest of the type (the operator does not decide that
somebody is finished playing), but it is the first thing a network meets once
it has more players than slots, because a handful of forgotten worlds is a
ceiling that never comes back on its own.

Whether a player is finished is a question about that player, and the system
that knows them is the one that can answer it: a plugin watching its own
sessions, an idle check of your own, a nightly job. Whatever decides calls
`stopServer` with the member's name, and that ends the member the way its
owner's own `/stop` would have: the players are drained, the claim stays, and
the next start mounts the same world.

## Nothing rolls

A spec edit does not touch a running member. It carries the spec it started with
(the pod hash is stamped as always and still tells a reader which one), and an
image bump reaches a world the next time its owner starts it. Throwing a player
out of their own world to apply a version bump is the opposite of what a
private server is for, and it is why `spec.update` is refused here rather than
ignored.

The group's `Progressing` condition is about phases only for this reason: it
says nothing about members being an old spec, because that is not a state the
operator is working to end.

## Who sees them

**Members, and the group itself, are in the proxies' network picture and not the
backends'.** A lobby's `servers()` never lists three hundred private servers,
and never receives a fresh picture for each start and stop. A proxy's does, and
that is where a plugin that sends players to them lives.

**The players stay, their whereabouts do not.** A backend's `players()` lists
everyone on the network, including whoever is on a private server; taking them
out would drop a player from a count while they are still online. What a backend
does not learn is *where*: `CloudPlayer.server()` is empty for them, as it is
for a player between two backends, so the network picture a backend receives
never names a server its own `servers()` does not list.

**The event feed is split the same way.** An event about a member or its group
reaches proxies only, so a backend plugin subscribed to events never hears of a
private server. In a proxy's chat feed a member shows as its group and the
first six characters of its key.

The same line bounds `connect`:

- A backend that names a private server in `Target.server(...)` is refused, with
  a message saying private servers are addressed through a proxy. `NOT_FOUND`
  would tell it that a server which is running fine does not exist.
- **A `connect` naming an on-demand *group* is refused for everyone**, proxy or
  backend. `Target.group(...)` lets the operator pick whichever member has room,
  and for this type that is whichever stranger's world does.

A private server's own plugins lose nothing by this: their own `announce` and
`acceptJoins` are their session, not the picture, and what your system knows
about its instances it knows from its own database.
