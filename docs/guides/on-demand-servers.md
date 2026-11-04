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
  image: ghcr.io/spawnery/purpur:26.3-0.34.0
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
| `startServer` with `StartOptions` | `REFUSED` | the group has no `spec.memberResources`, or a value breaks one of the rules in [Choosing resources at the start](#choosing-resources-at-the-start). A group whose own resources are above its `max` refuses a start without options too. |
| `startServer` with `StartOptions` | `already_running` set | the key is up and keeps what it started with; `resources()` says what that is. |
| `startServer` with `StartOptions` | `IllegalStateException`, no reason | the operator predates per-start resources. The member runs with the group's values, whether that operator started it or found it running. |
| `stopServer` | `REFUSED` | the server is not a member of an on-demand group. |
| `stopServer` | `NOT_FOUND` | no such server, including a stop that was carried out a moment ago. |
| `deleteServer` | `REFUSED` | the group is not `OnDemand`, the key is not a label, a claim of that name was not made by this operator for that group, or the world predates the key label (only an admin can delete it). |
| `deleteServer` | `NOT_FOUND` | no such group, or the key has neither a server nor a world, including a delete that finished a moment ago. |
| `startServer` | `UNAVAILABLE` | also: the key's world is still being deleted, or the group has `spec.memberResources` and its Network does not exist yet. |
| `restorePoints`, `listRestorePoints`, `restoreWorld` | `NOT_FOUND` | no such group, or the key has no world; for `restoreWorld` also a generation the world does not keep, unless the request repeats a restore that already went through. |
| `restorePoints`, `listRestorePoints`, `restoreWorld` | `REFUSED` | the group is not `OnDemand`, keeps its worlds on claims, the key is one no name can be built from, or the operator runs without world sync. |
| `restorePoints`, `listRestorePoints`, `restoreWorld` | `UNAVAILABLE` | the operator could not read the bucket just now. Ask again. |
| `restoreWorld` | `REFUSED` | also: the member is running, its world is being deleted, or the generation is the current one. |
| `restoreWorld` | `UNAVAILABLE` | the member is still stopping, or its final upload or another restore still writes the world. A restore whose operator died holds the world until its lease goes stale after 10 minutes. Ask again. |
| `restorePoints`, `listRestorePoints`, `restoreWorld` | `TimeoutException` | no answer within 75 seconds. The operator can take up to a minute on these two, so they wait longer than the ten seconds other calls get. A restore that timed out may or may not have happened. Ask again: while the member has not run since, a repeated restore answers with the generation the first one made. |

Every failure reaches Java as an `IllegalStateException` whose message is
`<REASON>: <the operator's sentence>`; the Javadoc says when it arrives wrapped.

**`stopServer` deletes a server**, and it is the only call on this channel that
does. What keeps a wrong name from taking down a lobby is that it checks the
`Server` carries a key, which only members of an on-demand group do: a lobby's
name gets `REFUSED`. That check is the bound. **Who may call is who may
install a plugin in the namespace**, the same boundary
[the plugin API](../plugin-api/index.md#what-it-can-see) describes for every
call, which is why `maxInstances` below is required rather than defaulted.

## Waiting for room

A start into a full cluster does not fail. The member is created, its pod stays
Pending, and left alone the member fails at the operator's startup deadline.
`ServerInfo.unschedulable()` says so while it is happening: true while the
server is Pending and the scheduler has found no node for its pod, whatever the
cause (no free memory, a node selector nothing matches). It is never true
together with a non-empty `node()`, and it reads false before the scheduler has
looked, so a start that was just asked for is neither placed nor unschedulable
for a moment. It also reads false while the member's own claim is still
binding under a storage class with `volumeBindingMode: Immediate`: the
scheduler's first tries fail until the volume exists, and the operator counts
that as waiting for the pod. Such a claim that never binds therefore shows no
`unschedulable()`, only the startup deadline. A `WaitForFirstConsumer` claim
binds only after the pod is placed and does not hold the flag back. An
operator older than 0.34.0 never sets the flag.

A plugin that does not want a player waiting on a full cluster watches the
member after `startServer`:

- `node()` is non-empty: the pod is placed, and the wait is the ordinary one for
  `READY`.
- `unschedulable()` is true: there is no room right now. Capacity can still
  arrive, because an autoscaler adds a node or a terminating pod frees its
  memory, and the operator reports no time for how long a server has been
  unschedulable, so the grace is yours to keep on your side. When it runs out,
  call `stopServer(name)`, tell the player, and queue them on your side.

**Stop it with `stopServer`, never `deleteServer`.** `deleteServer` removes the
world for good. A member that never took a player ends at once when it is
stopped, and a `startServer` of the same key a moment later can answer
`UNAVAILABLE` until the pod is gone; ask again.

`unschedulable()` comes from the same status the group's `Progressing`
condition reads, so the two agree. A proxy plugin that wants the scheduler's own
sentence subscribes to the event feed and matches `kind` `PodUnschedulable`;
that is a feed and not a ledger, and it has no counterpart for the moment the
pod is placed, which `node()` gives.

**Stopping before the deadline keeps the group's backoff out of it.** An
on-demand start is never held back by the creation backoff, so every start
creates a pod. A member that reaches the startup deadline Pending counts as a
failed round, the group reports `BackingOff`, and after six rounds `Degraded`,
which is something an administrator should see; a member stopped before then
leaves nothing to count.

## The ceiling is a fleet's, not a player's

`spec.maxInstances` is how many members the group may have at once. It counts
members that are not over: a `Failed` world kept for diagnosis and a `Finished`
one waiting to be swept do not count against it, and a member that is draining
does, because it still exists. `0` is legal and closes the group: new starts
are refused and every world stays where it is, which is the state an incident
wants and a deletion would not give.

A `Failed` member keeps its Server object, but not its pod, unless players are
still on it: the pod holds the world, through its claim or its lease in the
object store, and the next start of the key needs it. A pod that ended on its
own stays with its logs.

It is not a quota. *Who* may have a private server, and how many, is a question
about a player, a purchase and a ban, and the system that holds those is the one
that answers it. An operator that enforced it would need all three.

## Choosing resources at the start

Every member gets the group's `spec.resources`, or the network's default where
the group sets none, unless the group lets a start choose its own. A network
that offers private worlds in several sizes opts in with
`spec.memberResources`:

```yaml
spec:
  type: OnDemand
  resources:                 # what a start without values gets
    requests: { memory: 2Gi }
    limits:   { memory: 4Gi }
  memberResources:
    max:
      memory: 16Gi
      cpu: "8"
    nonHeap: 2Gi
```

`max` bounds every request and limit of the resources it names, including the
ones taken from the group. It has to name `memory`; `cpu` is optional,
and a start may not set a resource that `max` does not name. `nonHeap`, `2Gi`
unless set, is what a start's memory limit leaves outside the heap when the
start gives no heap of its own. The API server refuses a group whose
`max.memory` is not above it, and refuses `memberResources` on any group that
is not `OnDemand`.

The plugin passes the numbers as Kubernetes quantities:

```java
api.startServer("private-servers", key, StartOptions.builder()
        .memoryLimit("7Gi")          // heap 5Gi with the default nonHeap
        .build());

api.startServer("private-servers", key, StartOptions.builder()
        .memoryRequest("6Gi").memoryLimit("8Gi").heap("5500Mi")
        .cpuLimit("4")
        .build());
```

With the group above, the first one runs with a request of 2Gi, a limit of
7Gi and a 5Gi heap. The operator fills in what a start leaves out:

- An empty field takes the group's value. That is the group's
  `spec.resources`, or the network's `spec.defaults.resources` when the group
  has none, taken as a whole. A request the group leaves unwritten counts as
  its limit, as it would in Kubernetes.
- A memory request left empty keeps the group's request, lowered to the new
  limit if it is above it; CPU works the same way. A larger limit therefore
  reserves nothing more on the node unless the start also asks for a larger
  request. With no group value at all, the request equals the start's limit.
  The operator always writes the request out, so a namespace `LimitRange`
  cannot raise it to the new limit.
- The heap is the start's `heap`. Without one, a start that set a memory limit
  gets that limit minus `nonHeap`. A start with neither gets no heap, and its
  server sizes the heap as servers always have, with `MaxRAMPercentage=75` or
  the group's own `-Xmx`.

The start is `REFUSED`, with a message that names the field and the
numbers involved, when

- it carries values and the group has no `memberResources`;
- a value is not a quantity, is not positive, or is a heap below `1Mi`;
- it sets a resource `max` does not name;
- a request is above its limit;
- a memory limit leaves less than `1Mi` after `nonHeap` and the start gives no
  heap, for example a `memoryLimit` of `2Gi` with the default `nonHeap` of `2Gi`;
- the heap is not below the memory limit, or a heap is given and neither the
  start nor the group sets a memory limit;
- any resolved request or limit is above `max`, also one taken from the group
  or the network. A group whose own `spec.resources` is above its `max`
  therefore refuses every start, with values or without.

**A group with `memberResources` stamps every member**, including one started
without values. The member's `Server` carries the result in `spec.resources`
and `spec.heap`, and neither changes afterwards; a group edited later reaches a
key at its next start, as an image bump does ([Nothing rolls](#nothing-rolls)).
A running member cannot be resized, because a JVM cannot grow its maximum
heap. Stop it and start it again with the new values; the world survives that
as it survives any stop.

A start of a key that is already running is answered `already_running` with
the values the member runs with, whatever the request asked, and also when
`max` has since been lowered below them. `StartedServer.resources()` carries
them. Compare only the fields you set, and compare them as quantities: the
operator answers in canonical form, so a start that asked for `7168Mi` reads
back `7Gi`. A proxy sees the same values in `ServerInfo.resources()`; backends
do not see members at all.

### The start's heap wins over the group's `-Xmx`

The image passes the heap to `java` as `-Xmx`, in place of
`-XX:MaxRAMPercentage=75`. It comes after whatever `JDK_JAVA_OPTIONS` and
`JAVA_TOOL_OPTIONS` carry, and the JVM keeps the last `-Xmx` it reads, so a
member with a heap ignores an `-Xmx` the group sets in `spec.env`. A group that
relied on one will see members that a start sized run with the start's heap.
`_JAVA_OPTIONS` is the exception: the JVM applies it after the command line,
and an `-Xmx` there beats the start's.

The operator checks the result. The agent reports the JVM's maximum heap, and
when that is more than 5 % away from `spec.heap`, the `Server` gets the
condition `HeapMismatch` set to `True` (reason `HeapMismatched`, with both
numbers in the message) and a Warning event `HeapMismatch`. While the two
agree, the condition is `False` with reason `HeapAsAsked`. Besides
`_JAVA_OPTIONS`, this is how an image older than 0.33.0 shows up: it does not
read `SPAWNERY_HEAP` and ignores the heap the operator passes it.

A group-level `-Xms` in `spec.env` that is larger than a start's heap makes the
JVM refuse to start ("Initial heap size set to a larger value than the maximum
heap size"). Such a member never reports a heap, so `HeapMismatch` cannot flag
it. A group that uses `memberResources` should not set `-Xms`, or should keep
it at or below the smallest heap it starts.

```bash
kubectl get server private-servers-<key> -n minecraft \
  -o jsonpath='{.status.conditions[?(@.type=="HeapMismatch")]}'
```

Measured on 2026-11-01 in the on-demand e2e run (`make e2e-ondemand`, kind on
rootless Podman): a member started with only `memoryLimit("3Gi")`, in a group
with `nonHeap: 1Gi` under a network default of 2Gi request and 2Gi limit, got
a pod with a 3Gi limit and a 2Gi request, and its agent reported a maximum
heap within 5 % of 2Gi (`HeapAsAsked`). A start with an 8Gi limit against
`max.memory: 4Gi` was refused.

### Against older versions

An agent older than 0.33.0 has no `startServer` with `StartOptions`. A newer
agent against an operator that predates per-start resources fails the stage
with an `IllegalStateException` that says the operator does not know
per-start resources and that the member runs with the group's values. For a
key that was not running, that operator started the member with the group's
values, and it keeps running; stop it if those values are not good enough. For
a key that was running, it started nothing. A start without options is not
affected.

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
backends'.** A lobby's `servers()` never lists three hundred private servers. A
proxy's does, and that is where a plugin that sends players to them lives. A
proxy is sent a fresh picture when a member's phase, pod, node or
`unschedulable()` changes, at most once a second per namespace; player counts
and announcements wait for the 30 second resync, as they do everywhere.

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
