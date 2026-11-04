# Unschedulable servers and lifecycle pushes to proxies

**Status:** proposed
**Date:** 2026-11-03

## 1. What is missing

A plugin that starts an on-demand member into a full cluster cannot tell a
member that is merely slow from one the scheduler cannot place. The operator
knows: the Server controller turns a pod's `PodScheduled=False` into a Ready
condition with reason `PodUnschedulable`, and the group's `Progressing`
condition already names such servers. None of it reaches a plugin except as a
`PodUnschedulable` event, which is a feed: lost on a reconnect, and with no
counterpart for the moment the pod is placed.

The second gap is freshness. `ServerInfo` is a copy of the last `NetworkState`,
and a proxy is sent one on connect, on each 30 second resync and after
`acceptJoins`. A start, a stop or a placement pushes nothing, so a plugin
watching `server(name)` after `startServer` can wait up to 30 seconds for the
phase it is waiting for. Two docs said otherwise (a "fresh picture for each
start and stop"); they described no code.

## 2. Design

**A bool on the wire.** `ServerState.unschedulable` (field 16), true while the
server is Pending and its Ready condition carries `PodUnschedulable`. The
scheduler's reason is the same whether the cluster is full or a node selector
matches nothing, and its message describes the cluster
and changes on every attempt, so neither is sent; `status()` and the event keep
carrying the message. The field follows `held` and `joins_closed`: an operator
older than the field sends false, which is today's behaviour. It is also
cleared whenever `node` is set, so the two never both say yes in the instant
between the bind and the next status write.

While the server's own claim exists and is not `Bound` and the scheduler's
message names unbound immediate claims, the Server controller reads the pod as
plainly waiting (`PodPending`) and not as unschedulable: an Immediate-binding
class makes the scheduler's first tries fail with unbound claims for seconds,
and a consumer acts on `true` by stopping the member. The message decides, not
the claim alone: a `WaitForFirstConsumer` claim stays unbound until the pod is
placed, so on a full cluster it is unbound for exactly as long as the pod is
unschedulable. The claim is not watched; a pod condition change or the 5
second resync reconciles again once it is bound.

The predicate is `netstate.Unschedulable(srv)`, the same one the server group
controller used for its `Progressing` message, so the agents, the state
machine and the group agree.

**An event-driven push.** `statepush.Notifier` turns "a server changed" into
`Fleet.SendState` for the proxies, at most once per namespace per second: the
first change in a quiet namespace goes out at once, later ones share one push
at the end of the interval. The Server reconciler calls it, not the request
handlers: `netstate.Build` reads the manager cache, which has not seen the
write a handler just made, so a push from there would carry the old picture. A
reconcile triggered by an informer event runs after the cache holds the change.

The reconciler keeps a memo of what each server looked like to proxies
(`netstate.Visible`: the entry with players, slots, announcements and the door
left blank) and calls the notifier when it differs from the last one, or when
the Server is gone. A player-count write changes nothing in it, so counts keep
travelling on the resync.

**Java.** `ServerInfo.unschedulable()` as a new last component; the 14
argument constructor stays and reads false.

## 3. Not in this change

- Backends. `serverreg` has no coalescing and no `Delivered`, and a lobby
  routes nowhere on this information; its mirror stays on the resync. The
  notifier targets proxies only.
- `InstanceStatus` and `/cloud status`.
- A time since which a server has been unschedulable. The Ready condition's
  transition time does not move when only the reason changes, so it would be a
  new status field; the plugin keeps its own grace instead.
- The create backoff. An on-demand start never reads it, a member stopped
  before its startup deadline never fails, and for the other group types
  backing off while the cluster is full is correct.
- A metric counter for pushes, a flag for the interval, a version bump.

## 4. Consequences

A plugin can start a member, watch `node()` and `unschedulable()`, give the
cluster a grace period of its own and `stopServer` the member before the
startup deadline. The wire change is one added field; the pod spec inputs are
untouched, so no hash moves and nothing rolls on upgrade.
