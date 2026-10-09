# Scaling on free slots, and boosting for an event

An ephemeral `ServerGroup` is sized by the free player slots it can offer, not
by CPU: the operator builds another server when the seats a proxy could send
somebody to fall below `spec.scaling.spareSlots`. A `ScaleBoost` raises that
group's floor for a while, which is how you put capacity in place before an
event instead of after it.

Here is a group whose three scaling numbers are chosen rather than inherited.
It joins the Network of `config/samples/network.yaml`; apply that first if the
namespace has none. Save this as `hub.yaml`:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: hub
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: Ephemeral
  image: ghcr.io/spawnery/purpur:26.3-0.26.0
  maxPlayers: 50
  drain:
    timeoutSeconds: 60
  scaling:
    minReplicas: 2
    maxReplicas: 8
    spareSlots: 30
    # The default, written out because this is the number that decides how
    # twitchy the group is on the way down.
    scaleDownStabilizationSeconds: 300
```

```bash
kubectl apply -f hub.yaml
kubectl get servergroup hub -n minecraft
```

The `PLAYERS`, `FREE SLOTS` and `BOOSTED` columns show the control loop:
`FREE SLOTS` is the number the scaler compares against `spareSlots`, and
`BOOSTED` is how much of the group's floor comes from boosts rather than from
`minReplicas`.

## Two more servers, until Friday night is over

A boost is its own object, applied and forgotten: it stops counting at the
instant it names. Save this as `boost.yaml`:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ScaleBoost
metadata:
  name: hub-friday
  namespace: minecraft
spec:
  groupRef:
    name: hub
  replicas: 2
  expiresAt: "2026-09-19T23:00:00Z"
```

```bash
kubectl apply -f boost.yaml
kubectl get boosts -n minecraft
kubectl get servergroup hub -n minecraft
```

`spec` has exactly those three fields and nothing else; a boost has no status
at all, because whether it is live is its expiry against the clock and what it
did is on the group, as `status.boostedReplicas`. The floor of `hub` is now 4
while the boost lasts, so the group builds up to it even with nobody online,
and `maxReplicas: 8` still binds: a boost adds to the floor and never to the
ceiling.

Timestamps are RFC 3339 and the operator's clock decides, not yours. A boost
with no `expiresAt` never expires, which is occasionally what you want and is
the one that is still running in March with nobody left who remembers why.

To take it back early, delete it:

```bash
kubectl delete boost hub-friday -n minecraft
```

The group returns to its declared floor on the next pass and sheds the extra
servers by the ordinary scale-down rule below, not by killing them.

## Seats that count, and seats that do not

A round-based game often wants its servers to admit more players than a round
takes, so that spectators can still join a full round. `maxPlayers` is the
limit a server enforces; `playableSlots` is how many of those seats count as
capacity:

```yaml
spec:
  maxPlayers: 100
  playableSlots: 12
  scaling:
    minReplicas: 1
    maxReplicas: 8
    spareSlots: 12
```

A server's free seats are then `max(0, playable − players)`, where `playable`
is the figure its plugin set with `Spawnery.api().playableSlots(n)`, else
`spec.playableSlots`, else its slots, and never more than its slots. A lobby with
twelve players is full, and `spareSlots` orders the next server while its
countdown runs, not after its round has started. Players beyond the twelve are
still let in up to `maxPlayers` (unless the group enforces its playable seats,
below), and make the server full rather than overfull. `status.freeSlots`,
`/cloud` and a connect to the group all count the same seats.

### Making the playable seats a door

`enforcePlayableSlots: true` turns the count into a limit. Once a server holds
as many players as its playable seats, a further login is refused, except
for a player with the permission `spawnery.join.full.<group>`, who is always
let in up to `maxPlayers` and never takes a seat at the door, so an admin
watching a round does not keep a player out. The operator still counts every
player: its free seats, the scaler and a connect to the group see the admin as
seated, so a group may order its next server one watcher early. The flag is
read at runtime and restarts nothing; a server registers the check only once
its group turns it on, because a login listener switches off Paper's
reconfiguration API for the whole server.

The check is the server's own: its agent counts its online players exactly,
plus those it admitted in the same moment who have not joined yet, so a rush
on the last seat does not overshoot. It runs on Bukkit's `PlayerLoginEvent`,
the only login event that knows the player's permissions; Paper has marked it
for removal, and when it goes the check moves to the proxy.

The refused player sees a translatable message, key `spawnery.join.full` with
the group's display name as its argument and `This round is full.` as the
fallback. A network with its own translations renders the key in the player's
language; one without shows the fallback. On a server switch the player stays
where they were; on the first join into the network the proxy sends them to the
next server, as for any failed connect.

## The arithmetic, in the order the operator runs it

The operator runs this every five seconds for each group: capacity first, then
the ceiling, then demand. A group that is short of capacity never also shrinks in
the same pass.

**Capacity.** Each server contributes its free seats, and the group wants
enough servers to cover the gap:

```text
wanted = ceil((spareSlots - provisional) / capacity)     when provisional < spareSlots
floor  = minReplicas + live boosts
create = max(wanted, floor - alive)                      then capped at maxReplicas
```

`capacity` is `playableSlots` if the group sets it, else `maxPlayers`: what one
server brings before it has said anything.

With the group above and 75 players on it (one server full, one with 25 free
seats), `provisional` is 25, the gap to `spareSlots: 30` is 5, and
`ceil(5 / 50)` is one more server. Not two: `capacity` is the unit the gap is
divided by, so a group of large servers answers a small shortfall with one
server and a group of small ones with several.

`provisional` is deliberately not the `FREE SLOTS` column. It credits capacity
that has been *ordered* as well as capacity that has arrived, because a server
takes tens of seconds to become Ready and a scaler reading only arrived
capacity would order the same replacement on every pass until `maxReplicas`
stopped it. What it refuses to credit is capacity nobody can reach: a server
that has closed its door for a running round, one the proxies have dropped, one
whose counts are stale. Unknown counts as occupied everywhere in this
repository.

## The extra server you will see at the start

A freshly created server is credited **zero** for the pass or two in which its
`Server` object exists and the operator's pod cache has not caught up. So the
sum reads low, `wanted` reads high, and the group builds one more server than
it needs. That is why a brand-new group with `minReplicas: 1` comes up with two
servers, as the [tutorial](../tutorial/index.md) shows at its step 4.

This is deliberate: a server too many costs money, a server too few
costs joins. The correction is the scale-down rule, and it arrives once the
extra server has been reporting empty for `scaleDownStabilizationSeconds`,
five minutes by default, which is why a short demonstration never sees it.

## Coming back down

A server is removed for lack of demand only when all of this holds:

- the group has more servers than its floor (`minReplicas` **plus live
  boosts**), so a boost stops a scale-down as firmly as the spec does;
- no create is outstanding, because removing a server while capacity is on
  order is a decision made on two different readings of the same moment;
- the server reports zero players, its counts are fresh, and it has been empty
  for at least `scaleDownStabilizationSeconds`;
- removing it still leaves the group with `spareSlots` free seats. Each
  candidate is tested on its own, so an infeasible one does not hide a feasible
  one behind it.

One server per pass, never one that might be carrying players, and among the
eligible ones those that never took a player go first, then the youngest. A
server that had players on it is the last thing this group gives up.

## Choosing the three numbers

`spareSlots` is a headroom in *seats*, so read it as: how many people may join
between two passes without anybody meeting a full network. A group whose
servers hold 50 and whose `spareSlots` is 30 keeps well over half a server free
at all times, which for a lobby is right and for a 20-slot minigame group would
be nearly two whole servers of idle capacity.

`minReplicas` is what stands ready before anyone arrives; the join at 03:00
lands on it. `maxReplicas` binds
against demand and against boosts alike, and a group at its ceiling that needs
more says so on itself.

```bash
kubectl get servergroup hub -n minecraft \
  -o jsonpath='{.status.conditions[?(@.type=="ScalingLimited")]}'
```

`True` with reason `MaxReplicasReached` means the ceiling is holding capacity
back, and the message names how many servers the group wanted against what the
ceiling allows. The same condition also covers the one case where the ceiling
blocks a rolling update rather than a rush: a changeover needs room for one new
server before anything old can retire, so a group sitting exactly at
`maxReplicas` stalls until it is raised by one. The message says which of the
two is happening.

## Holding a group at a size

An `Add` boost raises a floor. To hold an ephemeral group at an exact size,
including 0, a `ScaleBoost` takes `mode: Exact`:

```yaml
kind: ScaleBoost
spec:
  groupRef: {name: lobby}
  mode: Exact
  replicas: 0
  expiresAt: "2026-10-04T18:00:00Z"
```

While it lives, the group's floor and its ceiling are both `replicas`, and
`Add` boosts pause. Of several live `Exact` boosts only the newest counts.
Empty servers above the number are deleted. Occupied ones are retired:
the proxies stop sending anyone there, the players stay until they leave, and
the server goes once it is empty. A pin to 0 therefore admits nobody new
without ending a running round. A group with `spec.update.maxStaleSeconds`
also bounds these retirements: after that window the server is drained.
A rolling update cannot surge above the pin, as at `maxReplicas`.

`status.pinnedReplicas` and `status.pinnedUntil` show the pin on the group.
`/cloud scale` creates exactly this object, owned by the group.

## Two things a boost is not

**It is not an edit to the group.** The operator has no write access to a
`ServerGroup`'s spec, and on a GitOps-managed cluster that spec belongs to a
file, and a floor raised there would be reverted at the next reconciliation. The
boost is a separate object, which is what lets somebody raise capacity on a
cluster whose manifests are owned elsewhere.

A boost raised from in-game rather than from a terminal is created by the
operator, which gives it an owner reference to the group, so deleting the group
takes that boost with it. One you apply yourself carries whatever you wrote and
nothing adds the reference for you. If you delete the group, delete the
boost too, or it sits in the namespace naming a group that is gone.

**It is not a setting with one value.** Boosts add. Two on one group are two
boosts and the second does not replace the first, which makes "somebody else
already boosted this" a non-event rather than a race between two people typing.
Expiry is read from the clock, so a boost stops counting the moment it says it
will; the sweep that deletes the object afterwards only tidies up.

A boost on a **persistent** group is accepted by the API, shows up in
`BOOSTED`, and changes nothing: such a group is sized by `spec.replicas` and
its floor is not what a boost moves.

Every field of both objects is in the generated
[custom resource reference](../reference/crds.md#servergroup), including the
[`ScaleBoost`](../reference/crds.md#scaleboost) spec and the status fields the
columns above are rendered from.
