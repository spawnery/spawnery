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
  image: ghcr.io/spawnery/purpur:26.2-0.5.0
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

Nothing starts. That is the point of the type: an `OnDemand` group is a template
and has no size. `spec.scaling`, `spec.replicas` and `spec.update` are refused
for it, and `spec.storage` and `spec.maxInstances` are required. A server exists
only because a plugin asked for it, and it is one player's own — `Ephemeral`
groups answer *how many*, and this one answers *which*.

## Asking for one

The caller is a plugin, on either side of the proxy, over the same channel
`retire` and `boost` use. `key` is whatever your own system calls the world —
an instance id from a database, say:

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
before anything is created:** the key has to be a DNS label — lowercase letters,
digits and `-` — and the composed name has to fit 63 characters. A 36-character
UUID leaves 26 for the group's own name, which is roomy for `private-servers`
and is written here because the failure lands on a player's start, not on the
`kubectl apply` that could have prevented it.

**The stage completing means the server was asked for, not that it can take
players.** It starts out as any server does. Wait for `ServerInfo.phase()` to
say `READY`, or for the event that says so, before sending anybody there — and
send them from a proxy; see [below](#who-sees-them).

What comes back, by reason:

| Call | Answer | When |
|---|---|---|
| `startServer` | `already_running` set | the key is up. A player pressing a button twice needs no lock on your side. It says nothing about readiness. |
| `startServer` | `UNAVAILABLE` | the member of that key is still stopping, or the group is at `maxInstances` only because a member is draining. Neither is a refusal: ask again. |
| `startServer` | `REFUSED` | the key is not a label, the name would not fit, the group is not `OnDemand`, another group's server already holds the name, or the group is at `maxInstances` with nobody leaving. |
| `startServer` | `NOT_FOUND` | no such group. |
| `stopServer` | `REFUSED` | the server is not a member of an on-demand group. |
| `stopServer` | `NOT_FOUND` | no such server — including a stop that was carried out a moment ago. |

Every failure reaches Java as an `IllegalStateException` whose message is
`<REASON>: <the operator's sentence>`; the Javadoc says when it arrives wrapped.

**`stopServer` deletes a server**, and it is the only call on this channel that
does. What keeps a wrong name from taking down a lobby is that it checks the
`Server` carries a key, which only members of an on-demand group do: a lobby's
name gets `REFUSED`. That check is the bound, and **who may call is who may
install a plugin in the namespace** — the same boundary
[the plugin API](../plugin-api/index.md#what-it-can-see) describes for every
call — which is why `maxInstances` below is required rather than defaulted.

## The ceiling is a fleet's, not a player's

`spec.maxInstances` is how many members the group may have at once. It counts
members that are not over: a `Failed` world kept for diagnosis and a `Finished`
one waiting to be swept do not count against it, and a member that is draining
does, because it still exists. `0` is legal and closes the group — new starts
are refused and every world stays where it is, which is the state an incident
wants and a deletion would not give.

It is not a quota. *Who* may have a private server, and how many, is a question
about a player, a purchase and a ban, and the system that holds those is the one
that answers it. An operator that enforced it would need all three.

## Stopping keeps the world, and nothing here ever deletes it

`stopServer` deletes the `Server`, and everything after that is the path a
scale-down already takes: the players on it are moved through the proxies inside
`spec.drain.timeoutSeconds`, the pod goes, and the claim stays. There is no
"save the world" step because the world was never anywhere else. The next
`startServer` with the same key mounts the same claim.

**This operator never deletes a claim** — not on a stop, not when the group is
deleted, not ever, and the ClusterRole has no verb that could. That has a price
in this type that a persistent group does not pay: a group of `Persistent`
servers has as many claims as `spec.replicas`, and one of these has as many as
players who ever asked. Every claim is `spec.storage.size`, whether or not its
owner comes back. What a claim costs, how to find the ones nobody is using and
why removing one is a human act are in
[Persistent storage](persistent-worlds.md), and all of it holds here unchanged;
the claims of this group are named `<group>-<key>-data` and carry the same
labels.

Deleting an instance for good — a player's own action, with nothing left behind
— is therefore not something a plugin can ask for, and this operator will not
grow a call for it. It is a job on your side that holds `delete` on claims and
whose own code is the guard; that right cannot be narrowed to one group's claims
because RBAC selects by name and these names are minted at runtime.

### Other ways a member ends

- **Its own run ends.** A player typing `/stop` is a player stopping their
  server, and a member's pod is never restarted for it. Once the run is over the
  object is deleted at once (`Finished`) — the world is on the claim, and the
  object would only hold the one name its owner needs to start again. A member
  that ended in `Failed` is kept, under the cap of one per group that an
  ephemeral group's failures are kept under, so that a world that broke can be
  looked at; a start on that key replaces it rather than being refused by it —
  unless it is still draining, when the start answers `UNAVAILABLE` and asking
  again a moment later works.
  The cap is per group and not per key, so a second player's broken world
  removes the first one's `Server` — the object only, never the claim.
- **Its node drains.** A member on a node that is leaving goes like any other
  server on it, players moved first. Nothing recreates it. The world is on its
  claim, so the key is free and its owner starts it again as they did the
  first time.

## Nothing rolls

A spec edit does not touch a running member. It carries the spec it started with
— the pod hash is stamped as always and still tells a reader which one — and an
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
everyone on the network, including whoever is on a private server — taking them
out would drop a player from a count while they are still online. What a backend
does not learn is *where*: `CloudPlayer.server()` is empty for them, as it is
for a player between two backends, so the roster never names a server the same
picture refuses to list.

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
