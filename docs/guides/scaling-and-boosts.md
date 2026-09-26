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
  image: ghcr.io/spawnery/purpur:26.2-0.8.0
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

The `PLAYERS`, `FREE SLOTS` and `BOOSTED` columns are the whole control loop
made visible: `FREE SLOTS` is the number the scaler compares against
`spareSlots`, and `BOOSTED` is how much of the group's floor comes from boosts
rather than from `minReplicas`.

## Two more servers, until Friday night is over

A boost is its own object, applied and forgotten — it stops counting at the
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
and `maxReplicas: 8` still binds — a boost adds to the floor and never to the
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

## The arithmetic, in the order the operator runs it

Every five seconds, for each group, in this order — capacity first, then the
ceiling, then demand. A group that is short of capacity never also shrinks in
the same pass.

**Capacity.** Each server contributes its free seats, and the group wants
enough servers to cover the gap:

```text
wanted = ceil((spareSlots - provisional) / maxPlayers)   when provisional < spareSlots
floor  = minReplicas + live boosts
create = max(wanted, floor - alive)                      then capped at maxReplicas
```

With the group above and 75 players on it — one server full, one with 25 free
seats — `provisional` is 25, the gap to `spareSlots: 30` is 5, and
`ceil(5 / 50)` is one more server. Not two: `maxPlayers` is the unit the gap is
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

This is chosen, not tolerated: a server too many costs money, a server too few
costs joins. The correction is the scale-down rule, and it arrives once the
extra server has been reporting empty for `scaleDownStabilizationSeconds` —
five minutes by default, which is why a short demonstration never sees it.

## Coming back down

A server is removed for lack of demand only when all of this holds:

- the group has more servers than its floor — `minReplicas` **plus live
  boosts**, so a boost stops a scale-down as firmly as the spec does;
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

`minReplicas` is what stands ready before anyone arrives — the join at 03:00
lands on it. `maxReplicas` is an instruction rather than a hint: it binds
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

## Two things a boost is not

**It is not an edit to the group.** The operator has no write access to a
`ServerGroup`'s spec, and on a GitOps-managed cluster that spec belongs to a
file — a floor raised there would be reverted at the next reconciliation. The
boost is a separate object, which is what lets somebody raise capacity on a
cluster whose manifests are owned elsewhere.

A boost raised from in-game rather than from a terminal is created by the
operator, which gives it an owner reference to the group, so deleting the group
takes that boost with it. One you apply yourself carries whatever you wrote and
nothing adds the reference for you — so if you delete the group, delete the
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
