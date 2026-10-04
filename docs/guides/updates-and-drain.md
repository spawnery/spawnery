# Rolling a group, and what happens to the players on it

Editing a `ServerGroup` or `ProxyGroup` replaces its servers. This page is
about what that costs the people standing on them. Most of the time, by
design, it costs nothing.

If a roll is happening right now, this is the command that tells you where it
has got to and whether anybody is being moved:

```bash
kubectl get servers -n minecraft
```

`PHASE` is the answer. `Retiring` means players are being left alone;
`Draining` means they are being moved. `PLAYERS` next to it says how many are
still there. And to see which pods are old and which are new:

```bash
kubectl get pods -n minecraft -l spawnery.cloud/role=server \
  -L spawnery.cloud/pod-hash
```

Two distinct hashes inside one group means that group is mid-roll; one value
everywhere means done or never started.

## `Retiring` is not `Draining`, and the difference is the whole point

These are two different phases with two different promises, and confusing them
is how people talk themselves into believing a rolling update disconnects
players.

**`Retiring`: soft drain.** This is what a rolling update puts a stale server
into. The server is deregistered from the proxies, so it takes no new joins,
and **its existing players are left alone until they leave of their own
accord**. `spec.drain.timeoutSeconds` does not hang over it at all. A busy
lobby can legitimately sit in `Retiring` for hours, and that is correct
behaviour.

**`Draining`: the players are moved.** The server is deregistered *and* the
proxies are asked to move its players onto a fallback. There is no way back to
`Ready` from here. This is the phase `spec.drain.timeoutSeconds` bounds.

So a group whose servers sit at `Retiring` with `PLAYERS` above zero is
waiting, as configured, not stalled.

## Bounding the wait

If waiting for hours is not acceptable, say so. Nothing else will:

```yaml
spec:
  update:
    # At most this many servers draining or terminating at once because of a
    # generation change. Default 1.
    maxUnavailable: 1
    # After this long in Retiring, empty the server actively instead of
    # waiting. 0, the default, means never.
    maxStaleSeconds: 1800
  drain:
    # The upper bound once a drain has actually started.
    timeoutSeconds: 120
```

`maxStaleSeconds` is the only thing that turns a patient `Retiring` into an
active `Draining`, and it is off by default. A group that never seems to
finish rolling is usually a group with players who never leave and a
`maxStaleSeconds` of `0`.

`maxUnavailable` defaults to `1`: one server at a time, so capacity dips by
one server rather than by the fleet. Raising it rolls faster and costs more
capacity while it does.

## Keeping servers joinable during a roll

A roll takes servers out of the proxies' tables one at a time and builds
replacements only as far as the group's free slots need them. A group whose
players choose a server, rather than being placed on any, can end up with a
single open server for a while. `minAvailable` says how many must stay open:

```yaml
spec:
  update:
    minAvailable: 2
```

While stale servers remain, the next one is retired only if at least that
many servers stay joinable afterwards: Ready, registered, door open, and not
on their way out, old or new alike. If retiring it would leave fewer, the
group first builds one extra server, waits for it to be Ready, and then
retires. The extra servers are removed by the ordinary scale-down once the
roll is over. It must be below `maxReplicas`, because keeping the floor needs
room for one more.

A roll held at the floor that cannot build its extra server says so on
`Progressing` (`WaitingForMinAvailable`), and on `ScalingLimited` if the
ceiling is the reason.

## Rolling only what is empty

Some servers are not worth rolling while they are in use: a round-based game
whose server gathers a lobby, closes its door for the round and ends with it.
`Retiring` would stop the lobby filling, and `maxStaleSeconds` would move the
players of a running round. `WhenEmpty` leaves them alone:

```yaml
spec:
  update:
    strategy: WhenEmpty
```

Only stale servers that are known to be empty are retired, and those go at
once. An occupied stale server stays `Ready` and joinable for as long as it
has players; once it is empty it is replaced like any other. A server whose
player count cannot be trusted counts as occupied. `maxStaleSeconds` cannot be
combined with it, and a node drain still moves these servers, because the
node is leaving either way.

Such a group takes a network changeover place only for its first new server.
Once that is Ready, `status.changeover` reads `Deferred`: the rest waits for
players, not for the budget, and other groups are not held behind it.

## Changing over a whole network

`maxUnavailable` bounds one group's own roll. It says nothing about what
happens when a change reaches every group at once (the network's defaults,
a config revision stamped onto all of them) and every group starts its own
extra server in the same pass. A network on two nodes sized to its groups has
room for one or two of those, not for all of them at once; the rest sit
Pending, run into their startup timeout, fail and back off, while the stale
servers they were meant to replace hold exactly the memory the replacements
need.

`spec.update.maxConcurrentChangeovers`, on the `Network`, bounds that instead:

```yaml
kind: Network
spec:
  update:
    maxConcurrentChangeovers: 1
```

Optional, minimum `1`. Unset means no cap, which is today's behaviour.

A group is changing over from the moment it has a stale server (a stale pod,
for a proxy group) until it is `Deferred` (see [Stages](#stages)) or its last
stale server or pod is gone. It holds its place for that whole window and is
never paused halfway. A group that must change over but has not begun waits its turn;
groups are admitted by stage, then by name (see [Stages](#stages) below).
While it waits, nothing about it changes except the roll: its stale servers
keep running and keep taking players, and only the cold start (for a proxy
group, the surge pods) is withheld until it is admitted. Player demand is not
withheld: a waiting group that still needs a new server to answer it builds
one at the current generation like any other, and that server is a begun
changeover holding a place of its own. That is a second way, besides the race
below, that the network can end up over the cap by one group. A group whose changeover is
failing (`BackingOff` or `Degraded`) holds no place, so one replacement that
cannot start does not stall every other group. A group whose cold start the
`maxReplicas` ceiling refuses does not wait for a place either; its own
`ScalingLimited` condition says why.

A waiting group shows up across the whole network at a glance:

```bash
kubectl get servergroups -n minecraft \
  -o custom-columns='NAME:.metadata.name,CHANGEOVER:.status.changeover'
```

and names who is holding the places it is waiting for, in its own
`Progressing` condition:

```bash
kubectl get servergroup <name> -n minecraft \
  -o jsonpath='{range .status.conditions[?(@.type=="Progressing")]}{.reason}: {.message}{"\n"}{end}'
# WaitingForChangeoverBudget: waiting for a changeover place; changing over: hub, lobby
```

A waiting proxy group says the same thing in its `ChangingOver` condition
instead.

The `Network` also reports two gauges, `spawnery_network_changeovers_in_flight`
and `spawnery_network_changeovers_waiting`, both labelled `namespace` and
`network`. They tell a rollout that is slow because the budget is doing its
job apart from one that is actually stuck.

Two reconcilers can admit themselves to the last place within moments of each
other. Once that happens both are holders, and neither is paused, so the
budget stays exceeded by one group until whichever of the two finishes its
changeover first, which takes minutes once a drain is part of it. The budget
accepts that race instead of locking against it; it exists to prevent a
sustained surge across the whole network.

### Stages

The budget says how many groups change over at once; it says nothing about
which ones go first. `changeoverStage` does:

```yaml
kind: ProxyGroup
metadata:
  name: edge
spec:
  changeoverStage: -10
---
kind: ServerGroup
metadata:
  name: arena
spec:
  changeoverStage: 10
```

Optional `int32` on `ServerGroup` and `ProxyGroup`, default `0`, negative
values allowed, so a group can be moved ahead of every group that sets
nothing. A group with a stale server waits while any group of a lower stage
is still changing over, or has a spec change the operator has not reconciled
yet; groups of one stage change over together, within the budget. The gate
applies whether or not a budget is set: an unset budget caps nothing, and it
does not reorder anything either. A group whose
changeover is failing (`BackingOff` or `Degraded`) gates nothing, the same as
for the budget, and so does one whose cold start the `maxReplicas` ceiling
refuses; its `ScalingLimited` condition says why. A group gated by its stage
that builds a server for player demand has begun, and is not paused, the same
as with the budget.

A group stops gating once its new generation stands, regardless of what its
old one is still doing. `status.changeover` reports this as `Deferred`:

- A `RollingUpdate` server group reaches it once every stale server still
  around is retiring or draining and every current server is Ready.
- A proxy group reaches it once at least `replicas` current pods are Ready
  and every stale pod still present is draining or terminating.

`Deferred` holds no budget place and gates no later stage, and a readiness
blip does not take either back once reached. That is also its cost: the old
pods keep their memory while the next stage starts its own extra server. On
a cluster sized for exactly one extra server at a time, that server stays
`Pending` until the earlier group's drain actually ends. `WhenEmpty` groups
have made the same trade since they were introduced.

Persistent groups wait for their stage and gate later ones until no stale
ordinal is left, but take no budget place of their own: they never surge.
On-demand groups refuse the field; their members never roll.

A group gated by its stage reports it the same way as the budget, in its own
`Progressing` condition (a proxy group's `ChangingOver`):

```bash
kubectl get servergroup <name> -n minecraft \
  -o jsonpath='{range .status.conditions[?(@.type=="Progressing")]}{.reason}: {.message}{"\n"}{end}'
# WaitingForEarlierStage: waiting for stage -10: edge
```

Unset everywhere, every group is in stage 0 and admission is exactly as
above: by name, within the budget.

## Proxies wait for their players too

A proxy that a roll replaces, that a lowered `replicas` removes, or that an
admin retires takes no new connections and is stopped once it is empty.
Nobody on it is disconnected, however long that takes. To bound it:

```yaml
kind: ProxyGroup
spec:
  update:
    # Disconnect whoever is left on a draining proxy after this long.
    # 0, the default, means a drain waits until the proxy is empty.
    maxStaleSeconds: 1800
```

Two cases keep `spec.drain.timeoutSeconds` as a deadline. A proxy on a node
that is leaving is removed at it, because the node goes either way. And a
proxy whose player count nobody can read (its agent is gone or its report
is stale) is removed once it has been unreadable that long, so a dead proxy
does not drain forever; an operator restart, after which every count is
unreadable until the agents reconnect, does not end a drain.

A forced pass sends at most one player per client address at a time. The next
player from that address goes once the proxy's `login-ratelimit` (3 s unless an
overlay changes it) and another half second have passed, because the receiving
proxy refuses a second login from one address inside that window as too fast.
That matters for a household or a school behind one address. Proxies of a group
that leave together (both halves of a blue/green roll) take turns: each sends
only in its own window of that length, with an empty window between any two,
so players behind one address who sit on different old proxies do not collide
either. A server switch from an address that was just used, or outside the
proxy's window, goes ahead on the old proxy without a transfer. Behind a front end that does not pass the client's address on (no
PROXY protocol), every player shares the front end's address, and a forced pass
moves one player every few seconds.

A plugin that announces quits and joins can ask whether one is a transfer; see
[A player who changed proxies](../plugin-api/index.md#a-player-who-changed-proxies).

A roll replaces proxies blue/green, not one at a time: every stale pod gets
its own replacement up front, one extra pod for each proxy being replaced. A
stale pod serving nobody is marked to drain at once, so a crashlooping proxy
cannot hold its own replacement back; once at least `replicas` of the new
pods are Ready, every other stale pod still around is marked in the same
pass, each on its own `maxStaleSeconds` deadline. A `replicas` lowered
mid-roll, while fewer than `replicas` new pods are Ready, takes its surplus
from the new pods, never from the old ones still serving.

The group reports `status.changeover` as `Deferred` once at least `replicas`
current pods are Ready and every stale pod still present is draining or
terminating: it holds neither a changeover place nor a later stage for as
long as the last players take to leave. `spec.update.maxStaleSeconds` is
what bounds that drain.

### Moving players to another proxy

Since Minecraft 1.20.5 a server can hand a client a *transfer*: the client
closes its connection and opens a new one elsewhere, and the proxy it lands
on can read back a cookie the old one set. `spec.update.transfer` puts that
to use instead of waiting for `maxStaleSeconds`, or for the player to leave
on their own:

```yaml
kind: ProxyGroup
spec:
  update:
    transfer:                 # unset = today's behaviour
      forceAfterSeconds: 120  # default 120, minimum 0
```

**Enabling it rolls the group once.** A proxy only accepts a transferred
client with `accepts-transfers = true` in `velocity.toml`, which the
operator renders only for a group with `transfer` set, and that line is part
of the rendered config the group's pod hash covers. The proxies doing that
first roll do not have the setting yet, so they still drain the old way;
every roll after that transfers.

A group whose `configOverlay` sets a `config-version` older than 2.7 in
`velocity.toml` is refused while `transfer` is set: Velocity would migrate such
a file on start and switch `accepts-transfers` off again. Drop the key from the
overlay, or bring the overlay up to the current version.

**Disabling it is safe.** It rolls the group the same way, and the old
proxies, which still transfer, only send players to proxies that accept
them: the new ones do not, so the old ones drain the old way.

Once enabled, a leaving proxy moves players in two moments:

- **At once, on a server switch.** A player who is about to connect to a
  different backend (`/server arena`, a plugin sending them on) is
  transferred there instead, landing on another proxy of the group along the
  way.
- **Forced, after `forceAfterSeconds`.** Counted from when the proxy's agent
  first sees itself leaving, which is within one resync (30 s) of the
  operator starting the drain. Keep it below `drain.timeoutSeconds` and any
  `maxStaleSeconds`, or those disconnect the players first. After that,
  every player whose current server's door is open is transferred back to
  that same server. A player on a server whose door is
  closed (`AcceptJoins` false, a round in progress) is never forced; once the
  door opens and the deadline has passed, they go on the next pass, which
  runs once a second.

A transfer only happens while another proxy of the same group is Ready, not
itself leaving, and accepts transfers. Otherwise there is nowhere to send
the player, and the proxy leaves them where they are. Each player is tried
once per leaving proxy, so one who comes back to it is not sent round again;
anyone whose client is older than 1.20.5 cannot be transferred and stays
behind, same as before, bounded by `maxStaleSeconds` and the drain deadline.

The transfer sends the client to the host and port it typed in, not to a
particular proxy: the Service in front of the group picks where the player
lands. An SRV record or a front end that maps ports works as
long as that name still leads to the group. With `expose.type: HostPort`
the address is a node's, and there the port still belongs to the leaving
pod, so the transfer brings the player back to it; it does not transfer
them a second time, but they do not move either.

The player lands on the new proxy at the server they were going to, or were
already on, via a signed cookie keyed off the forwarding secret every proxy
in the network already mounts. That gives the secret a second job: whoever
holds it can mint a cookie that sends their own player to any registered
server, past whatever the proxy would have chosen, so treat a plugin that
can read it as one that can route.

A cookie that does not check out routes the player as an ordinary fresh join
rather than to the named server: one that has expired (they are good for
60 s), belongs to another player, names a server the receiving proxy does not
know, or was written before a forwarding-secret rotation.

What the player sees is a loading screen; what the backend sees is a quit
followed by a join, the same as any reconnect. That has not been measured
against a real client yet, so take "loading screen" as the shape of it, not
a claim that it feels seamless.

The agent logs every transfer and every cookie it refuses:

```
spawnery: transferred 'Notch' (switch) toward 'arena'
spawnery: transfer cookie from 'Notch' refused: expired
```

A roll replaces proxies blue/green (the new pods come up and serve while
the old ones drain), so during a roll there is always somewhere for a
transfer to land. Retiring or scaling down a single replica has no such
guarantee: when no other proxy of the group is Ready, nobody is transferred.

## Taking a retirement back

`/cloud unretire <name>` (or `unretire(server)` from a plugin) takes a
server's retirement back while it is still `Retiring`: it goes back to
`Ready`, takes joins again, and carries `spec.hold`. A held server is never
removed automatically: a rolling update leaves it on its old generation, and
neither scale-down nor a lowered `maxReplicas` deletes it. It stays until it
ends by itself, and it does not keep the group's changeover open. A node drain
still moves it, and a `retire` still retires it.

## What actually makes a group roll

The operator compares each server against a hash of the whole desired pod plus
the rendered configuration files. Anything that changes either one makes every
existing server stale, and the group replaces them.

That is a wide net, on purpose: "the pod would come out different, so the
pod is replaced" is easier to reason about than a hand-maintained list of
which fields matter. Two consequences:

- **A change to the group's image, resources, environment or rendered config
  rolls the whole group.** So does a change to the `Network` defaults those
  inherit from.
- **A change to `spec.scaling` does not.** Scaling numbers are not part of a
  pod, so editing `minReplicas`, `maxReplicas` or `spareSlots` changes how
  many servers exist without replacing the ones that already do.

One exclusion is easy to be surprised by from the other side:
the forwarding-secret hash is removed from the digest on purpose, so
**rotating the forwarding secret does not make every server stale at once**.
That rotation has its own ordered procedure in [Rotating the forwarding
secret](rotating-the-forwarding-secret.md), which it needs because the
ordinary roll does not do that work.

Changing the hash inputs is not a thing to do casually on a live network: it
means every existing server is replaced on the next pass. The repository
treats that as a deliberate act.

## When it is the node leaving, not the group

A server is also moved off a node that is on its way out. The operator treats
a node as departing in two cases:

- **`spec.unschedulable`**, which `kubectl cordon` and `kubectl drain` set.
  This is hardwired and not configurable.
- **A taint whose key is in the operator's `--drain-taint` list**, for
  autoscalers that taint before they cordon. Only the effects that actually
  repel a pod count: a `PreferNoSchedule` taint is ignored, because the
  scheduler would happily put the replacement back on the same node and the
  group would rotate for as long as the taint stood.

There is no default list, and that is on purpose: reacting to another
project's taint key by default would tie this operator to a vocabulary that
project is free to rename. If you run an autoscaler, you must pass the flag.

The operator does warn, though. It knows the keys
cluster-autoscaler and Karpenter use, and when a node turns up carrying one
that is *not* in the list it was given, it logs that, naming the node, the
project and the flag. It never acts on it. A warning that stops appearing
costs a warning; a drain that stops working costs a node's worth of players.

```bash
kubectl logs -n spawnery-system deployment/spawnery-operator | grep drain-taint
```

## This page and the other two

This page is about what *your own* edit rolls, and what that costs the people
standing on the servers. [Upgrading](upgrading.md) is the other direction:
what a *release* rolls when nobody has edited anything, because the operator's
rendering code moved underneath every group. If you are asking "what did
0.2.33 do to me", that is [Release notes](../archive/release-notes.md).

Every field named here is in the generated [custom resource
reference](../reference/crds.md#servergroup), and `--drain-taint` is in
[Operator flags](../reference/operator-flags.md).
