# Rolling a group, and what happens to the players on it

Editing a `ServerGroup` or `ProxyGroup` replaces its servers. This page is
about what that costs the people standing on them — which, most of the time
and by design, is nothing.

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

**`Retiring` — soft drain.** This is what a rolling update puts a stale server
into. The server is deregistered from the proxies, so it takes no new joins,
and **its existing players are left alone until they leave of their own
accord**. `spec.drain.timeoutSeconds` does not hang over it at all. A busy
lobby can legitimately sit in `Retiring` for hours, and that is correct
behaviour, not a stuck roll.

**`Draining` — the players are moved.** The server is deregistered *and* the
proxies are asked to move its players onto a fallback. There is no way back to
`Ready` from here. This is the phase `spec.drain.timeoutSeconds` bounds.

So a group whose servers sit at `Retiring` with `PLAYERS` above zero is not
stalled. It is waiting, which is what you asked it to do.

## Bounding the wait

If waiting for hours is not acceptable, say so — nothing else will:

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
happens when a change reaches every group at once — the network's defaults,
a config revision stamped onto all of them — and every group starts its own
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

Optional, minimum `1`. Unset means no cap — today's behaviour, unchanged.

A group is changing over from the moment it has a stale server (a stale pod,
for a proxy group) until the last one is gone, including one that is draining
or terminating, and it holds its place for that whole window — never paused
halfway. A group that must change over but has not begun waits its turn;
server groups and proxy groups are admitted together, by name. While it
waits, nothing about it changes except the roll: its stale servers keep
running and keep taking players, and only the cold start (for a proxy group,
the surge pod) is withheld until it is admitted. Player demand is not
withheld: a waiting group that still needs a new server to answer it builds
one at the current generation like any other, and that server is a begun
changeover holding a place of its own — a second way, besides the race below,
that the network can end up over the cap by one group. A group whose changeover is
failing (`BackingOff` or `Degraded`) holds no place, so one replacement that
cannot start does not stall every other group. A group whose cold start the
`maxReplicas` ceiling refuses does not wait for a place either — its own
`ScalingLimited` condition says why, not the budget.

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
`network` — useful for telling a rollout that is slow because the budget is
doing its job from one that is actually stuck.

Two reconcilers can admit themselves to the last place within moments of each
other. Once that happens both are holders, and neither is paused, so the
budget stays exceeded by one group until whichever of the two finishes its
changeover first — minutes, not seconds, once a drain is part of it. That is
accepted rather than locked against: what it prevents is a sustained surge
across the whole network, not a race between two groups.

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
proxy whose player count nobody can read — its agent is gone or its report
is stale — is removed at it too, so a dead proxy does not drain forever.

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

That is a wide net, and deliberately so — it is easier to reason about "the
pod would come out different, so the pod is replaced" than about a
hand-maintained list of which fields matter. Two consequences are worth
knowing:

- **A change to the group's image, resources, environment or rendered config
  rolls the whole group.** So does a change to the `Network` defaults those
  inherit from.
- **A change to `spec.scaling` does not.** Scaling numbers are not part of a
  pod, so editing `minReplicas`, `maxReplicas` or `spareSlots` changes how
  many servers exist without replacing the ones that already do.

One exclusion is deliberate and easy to be surprised by from the other side:
the forwarding-secret hash is removed from the digest on purpose, so
**rotating the forwarding secret does not make every server stale at once**.
That rotation has its own ordered procedure in [Rotating the forwarding
secret](rotating-the-forwarding-secret.md), and the reason it needs one is
precisely that the ordinary roll is not doing the work for it.

Changing the hash inputs is not a thing to do casually on a live network: it
means every existing server is replaced on the next pass. The repository
treats that as a deliberate act, and so should you.

## When it is the node leaving, not the group

A server is also moved off a node that is on its way out. The operator treats
a node as departing in two cases:

- **`spec.unschedulable`** — what `kubectl cordon` and `kubectl drain` set.
  This is hardwired and not configurable.
- **A taint whose key is in the operator's `--drain-taint` list**, for
  autoscalers that taint before they cordon. Only the effects that actually
  repel a pod count: a `PreferNoSchedule` taint is ignored, because the
  scheduler would happily put the replacement back on the same node and the
  group would rotate for as long as the taint stood.

There is no default list, and that is on purpose: reacting to another
project's taint key by default would tie this operator to a vocabulary that
project is free to rename. If you run an autoscaler, you must pass the flag.

The operator does warn rather than leave you guessing. It knows the keys
cluster-autoscaler and Karpenter use, and when a node turns up carrying one
that is *not* in the list it was given, it logs that — naming the node, the
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
