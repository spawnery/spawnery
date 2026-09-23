# A network-wide budget for changeovers

**Status:** design, decided 2026-09-23
**Date:** 2026-09-23

## 1. What goes wrong today

Every group rolls on its own. When a group's spec changes, all its servers
become stale, `coldStart` creates one server of the new generation, and stale
servers retire only once a current one is Ready. A proxy group does the same
through `DecideRollout`'s surge of one. Per group that is sound: a changeover
costs at most one extra server.

Nothing adds those extras up. A change that reaches every group at once (a new
image in the network's defaults, a resource change in every group, a
content revision stamped on all of them) makes every group start its extra
server in the same pass. A network whose cluster is sized to its groups has
room for one or two of those, not for all of them. The rest stay Pending, run
into their startup timeout, fail, and back off; the stale servers they were
meant to replace stay Ready, holding exactly the memory the replacements
need. Observed on a two-node cluster: every group of a network stuck in that
state until servers were deleted by hand.

`spec.update.maxUnavailable` does not help: it bounds retiring, and retiring
only begins once a replacement is Ready.

## 2. The shape

**A network may cap how many of its groups change over at the same time.**

```yaml
kind: Network
spec:
  update:
    maxConcurrentChangeovers: 1
```

Optional, minimum 1. Unset means no cap, which is today's behaviour: an
upgrade changes nothing for a network that does not ask for it.

**A group is changing over while it still has a stale server** (a stale pod,
for a proxy group). That is exactly the window in which it runs one server
more than its size, so counting groups counts extra servers.

**Who holds a place:**

- A group that has **begun** its changeover (it has a server of the current
  generation) holds a place until its changeover ends. It is never paused
  halfway: a paused changeover still holds its extra server and would only
  make the wait longer.
- A group whose changeover is **failing** (its `BackingOff` or `Degraded`
  condition is True) holds no place. One replacement that cannot start must
  not stop every other group; the failing group competes again once it
  recovers.

**Who waits:** a group that must change over but has not begun is admitted
only while places are free. Waiting groups are admitted by name, server groups
and proxy groups in one order, so every reconciler reading the same cache
reaches the same answer without coordinating.

**What a waiting group does:** nothing new. Its stale servers keep running and
keep taking players; only the cold start (for a proxy group, the surge pod)
is withheld until it is admitted.

## 3. Where it is decided

**Each group decides for itself, from the shared cache.** A group knows its
own changeover state only as a by-product of its own reconcile: whether a
server is stale depends on the group's desired pod hash, which takes the
network, the group and its config overlay to compute. So every group
publishes that state in its status, `status.changeover`: empty when it is not
changing over, `Waiting` when it must and has not begun, `Begun` when it has a
server or pod of the current generation beside stale ones. Before sizing, a
reconciler reads its siblings' `status.changeover` and conditions from the
informer cache, adds its own state computed this pass, and calls one pure
function:

```go
// ChangeoverView is one group as the budget sees it.
type ChangeoverView struct {
    Kind     string // "ServerGroup" or "ProxyGroup"
    Name     string
    Changing bool   // has a stale server or pod
    Begun    bool   // has a server or pod of the current generation
    Failing  bool   // BackingOff or Degraded is True
}

// AdmitChangeovers returns the groups that may change over now, keyed
// "Kind/Name". budget < 1 means no cap.
func AdmitChangeovers(groups []ChangeoverView, budget int32) map[string]bool
```

Holders are the groups that are changing, begun and not failing. The places
left (`budget - holders`) go to the remaining changing, not-failing groups in
name order. A group that is not changing is never in the result and never
needs to be.

A group name is unique per kind but a server group and a proxy group may share
one; the result is keyed by `Kind/Name`.

**ServerGroup:** a group that is changing and not admitted gets no cold start.
`DecideSize` already has the path for a refused cold start
(`ColdStartBlocked`), used today for a ceiling that leaves no room; a new input
`ChangeoverRefused bool` (named for the refusal, so its zero value is today's
behaviour) refuses it the same way, and the fall-through that
keeps a refused cold start from stalling the demand rule applies unchanged.

**ProxyGroup:** `DecideRollout` gets the same input and computes its target
without the surge of one when the group is not admitted. With no surge, the
create branch never fires, and the at-target branch marks a pod only when a
ready one is to spare or a stale one serves nobody; the replacement for such a
pod takes its place rather than adding to it, so a waiting proxy group costs
no extra pod.

**The race.** Two reconcilers can read the cache a moment apart and both admit
themselves to the last place, and a sibling's `status.changeover` is one
status write behind its reconcile. The budget is then exceeded by one for the few
seconds until both see the other's new server, after which the later one is a
holder like any other. This is accepted rather than locked against: the
failure it prevents is a sustained surge of every group, not a transient one
of two.

**Wake-up.** A waiting group needs no watch on its siblings: every group is
reconciled at least every five seconds, which is also how often a changeover
can make progress.

## 4. What an operator sees

- A waiting group reports `Progressing=True` with reason
  `WaitingForChangeoverBudget` and a message naming the groups holding the
  places, e.g. `waiting for a changeover place; changing over: hub, lobby`.
- The Network reports `spawnery_network_changeovers_in_flight` (holders) and
  `spawnery_network_changeovers_waiting` as gauges.
- The field and the reason are documented in the CRD reference and in the
  guide on updates and drain (`docs/guides/updates-and-drain.md`), with the
  two-node example of §1 told generically.

## 5. Testing

- **`AdmitChangeovers`, table tests:** no cap admits every changing group;
  a full budget admits none of the waiting; a begun group is always admitted
  and never displaced by a name that sorts earlier; a failing holder frees its
  place for the first waiting group; a failing waiting group is not admitted;
  a server group and a proxy group of the same name are two entries.
- **`DecideSize` / `DecideRollout`:** a changing, not-admitted group creates
  nothing and reports the waiting reason; admitted, both behave exactly as
  today (the existing tables, which leave `ChangeoverRefused` false).
- **envtest:** a network with `maxConcurrentChangeovers: 1`, three server
  groups and one proxy group; change the network's default image. Assert that
  two groups never hold a current-generation server that is not Ready for
  longer than the race of §3 allows (two reconcile passes, ten seconds), and
  that all four end at the new generation.
- **Hash goldens stay unchanged:** the field lives on the Network and feeds no
  pod.

## 6. Not in this

- A budget in servers or in memory rather than in groups. Groups count extra
  servers exactly today; a memory budget would need the scheduler's view.
- Changing a persistent group's roll or an on-demand group's members: neither
  surges.
- Priorities between groups beyond name order.
