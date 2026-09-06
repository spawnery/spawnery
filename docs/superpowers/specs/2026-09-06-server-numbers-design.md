# Every server carries a number a person can read

## Goal

A player on a hub cannot say which hub they are on. The tab list shows the
group's `displayName` for every server of the group, so all of them read
"Hub"; the server selector shows the other extreme, the pod name
`oneblockrace-solo-vz3g`, which nobody can say out loud.

Both want the same missing thing: a short number that tells one server of a
group from its siblings. The operator hands one out, the agent carries it, and
the two places a player reads a server name use it.

## Decisions

| | |
|---|---|
| Who assigns | the `ServerGroup` reconciler, once, when it creates the server |
| Where it is stored | `spec.number` on `Server`, a new field |
| Value | the lowest number from 1 upwards that no live server of the group holds |
| Persistent groups | `spec.number` is set to the ordinal, so it matches the name |
| How it travels | `ServerState.number` in the agent proto, `ServerInfo.number()` in the API |
| Written as | `Hub-2` — the group's `displayName`, a hyphen, the number |
| Tab list | numbers only groups carrying the attribute `tablist: numbered` |
| Selector | always numbers; telling siblings apart is what it is for |
| Server names | unchanged, random suffix and all |

## Why the names stay as they are

The obvious move is to name the servers `hub-1`, `hub-2` and be done: the
number would then be in the pod name, in `kubectl`, in the logs and in
Velocity's own routing table, with nothing to publish and nothing to read.

It was rejected because the random suffix is load-bearing in a way the name
does not show. `NewServerName` draws four fresh characters on every call, and
three separate places rely on that:

- `ServerGroupReconciler.size` may create a server the cache has not shown yet
  and create again on the next pass. With a random name the second create is a
  genuinely distinct pod for a slot the first one already fills — a surplus the
  next pass removes. With a derived name it is an `AlreadyExists` collision,
  which is why `createPersistentServer` carries the squatter handling and
  `createServer` carries none.
- `ProxyGroupReconciler.reconcileReplicas` states the same thing about
  `NewProxyName` at length: `apierrors.IsAlreadyExists` below it has nothing to
  catch precisely because the names differ.
- `Server.reconcile` has a `nameStillHeld` branch for a create meeting the pod
  its predecessor left behind, and its comment says the case it was written for
  is the persistent one, because an ephemeral collision would need the same
  four characters twice running.

Numbering the names would move every ephemeral group onto the code path
written for identities. That is a large change to the part of this operator
that is hardest to test, for a cosmetic gain — so the number rides beside the
name instead.

## Why not `spec.ordinal`

`Server` already has a number: `spec.ordinal`, the identity of a persistent
server, which is what makes `podspec.DataClaimName` stable and therefore what
makes a world survive its pod. Setting it on ephemeral servers as well would
give this feature a field for free.

It cannot be done, and the reason is one line:

```go
// server_controller.go:849
groupType := spawneryv1alpha1.ServerGroupEphemeral
if srv.Spec.Ordinal != nil {
    groupType = spawneryv1alpha1.ServerGroupPersistent
}
```

That is the fallback group the `Server` reconciler synthesises when the real
one cannot be read, and it decides the type from the ordinal's presence.
Setting the field on every server would make every server persistent to that
path. `DecidePersistentSize`, `CountFailures` and `ordinalBefore` all read the
same field for the same meaning.

So `spec.ordinal` keeps meaning "this server has an identity", and the display
number is its own field.

## How the number is assigned

In `ServerGroupReconciler.size`, which already holds everything needed:
`views` for the numbers in use and `Expectations` for the creates it has issued
and not yet seen.

`ServerView` gains `Number int32`, filled from `spec.number` beside `Ordinal`
at the view's construction. Before the create loop the reconciler builds the
set of taken numbers from the views and from
`Expectations.pendingNumbers(key)`; each create takes the lowest number not in
that set and adds it to it, so the numbers handed out within one pass differ
from each other as well.

`expectation` gains a `number` field and `expectCreated` a parameter for it.
`ProxyGroupReconciler.reconcileReplicas` calls the same method and passes 0:
proxies are not numbered, and `pendingNumbers` skips 0 for exactly that reason.
`pending()` keeps its signature — a fourth return value would be read by one
caller — and a new `pendingNumbers` reads the same map.

**Why the reservation is worth the field.** Without it a scaler that creates in
response to player counts hands the same number to two servers whenever the
cache lags, which the expectations mechanism exists for and its own comment
calls the ordinary case rather than the rare one. A number that shifts is
untidy; a number that is duplicated sticks for the whole life of both servers,
because nothing later revisits an assignment.

`createPersistentServer` sets `spec.number` to the ordinal it was given. Then
there is one field to read everywhere downstream and no merge, and a persistent
server's number agrees with its own name. It also means persistent numbers
start at 0 where ephemeral ones start at 1. Ordinal zero is therefore
indistinguishable from an unnumbered server, and every reader falls back to
the name it already has for both. That costs nothing: a persistent server is
referred to by the name that names its world, so falling back to it loses
nothing a number would have added.

## Nothing is backfilled

A server that exists when this ships has no number, and none is written to it.
`spec.number == 0` means "not numbered" and every reader falls back to what it
shows today.

The alternative — a reconcile that numbers the servers it finds unnumbered — is
a write path onto existing objects for a transient state. The network rolls
every group on every assembly that changes it, so the unnumbered servers are
gone within hours of the release reaching it. Not worth the code.

## What travels

`ServerState` gains

```proto
// Which of its group's servers this is, counted the way a person counts:
// the second hub is 2. Stable for as long as the server exists, and given
// out again only after it is gone.
//
// 0 for a server the operator never numbered, which is every server that
// was already running when this field arrived. A reader that shows this to
// a player should fall back to the group's name for those.
int32 number = 10;
```

`internal/netstate` fills it from `spec.number`. `make proto` regenerates
`internal/agentpb` and `agent/common/src/proto/java`, both committed.
`NetworkMirror.kt` passes it into `ServerInfo`, which gains an `int number`
component with the same wording.

**`ServerInfo` is a record, so this breaks its canonical constructor.** Every
call site is a test and every one is in-house: `ValueTypesTest` in this
repository, and five sites across four test classes in cyperia. They get the
extra argument. A second constructor keeping the old arity would live
forever to save a one-line edit that happens once.

## What the two readers do

**The tab list** (`essentials/velocity`, `ServerDisplayNames.of`) returns
`displayName + "-" + number` when the server's group carries the attribute
`tablist: numbered` and the number is not 0. Otherwise it returns the bare
`displayName`, as it does today, and where no agent answers it returns the
registered name, as it does today.

The attribute is what keeps the game modes out of it. A player on
`oneblockrace-solo` is there for the round, not for the server, and a number in
the tab list would be noise; a player on a hub wants to know which hub, because
that is how they meet somebody. `tablist` as the key and `numbered` as the
value rather than a boolean `numbered`: the key then says which surface it
governs, and cannot be misread as switching numbering off everywhere. It goes
on `hub.yaml` beside `game: Hub`, and on no other group.

**The selector** (`lobby/common`) gains a `displayName` component on
`SelectableServerData`, filled by `describe()` with the same string and falling
back to the server name when the number is 0. `ServerSelectorMenu` renders it
where it currently renders `data.serverName()`; `sendToServer` keeps
`serverName()`, which is the route and not a label. `SelectorRanking` and
`QuickJoinCommand` compare `serverName()` and are untouched.

The selector does not read the attribute. It lists the servers of one group
side by side, so distinguishing them is the entire job of the entry, and a
switch that turned that off would only ever be wrong.

## Testing

- `internal/controller`: the lowest free number is taken; a number held by a
  pending create is not handed out again; two creates in one pass differ; a
  persistent server's number equals its ordinal.
- `internal/netstate`: the number reaches `ServerState`; a server without one
  travels as 0.
- `internal/agentpb/contract_test.go` is unaffected: it guards the service
  name, the two streams' shapes and one `PlayerCount` round trip, none of
  which touches `ServerState`.
- `agent/api`: the record carries the number through `equals` and the
  null-and-default rules.
- essentials: a numbered group, a group without the attribute, a server whose
  number is 0, and no agent at all.
- lobby: the entry's `displayName` carries the number while `serverName` stays
  the pod name.

## Versions and order

All three numbers move. `operatorVersion` because the reconciler changes;
the chart because the CRD lives in `charts/spawnery/templates/crds.yaml`, which
drags `Chart.yaml`'s `version` and `appVersion` and the `--version` in both
READMEs; `imageVersion` because the agent's Java API changes, which is also the
version `cloud.spawnery:spawnery-api` is published to Maven Central under.

The order is forced by that last one: release spawnery, wait for the API to
appear on Central, bump `spawneryApiVersion` in cyperia, then the group images
in configs, then the attribute on `hub.yaml`. The attribute is inert until the
plugins that read it are deployed, so it may travel with the images or after
them.

## Not part of this

The `/cloud` commands keep printing pod names. That is an admin surface, and
the pod name is the thing an admin needs in order to reach for `kubectl`.

Proxies get no number. A player never reads a proxy's name — the proxy is what
they connect through, not a place they are — so `ProxyState` is untouched.

Pods are not renamed, `spec.ordinal` is not touched, and no group's
`displayName` changes.

## Open points

**A number is reused as soon as its server is gone.** Two players comparing
notes across a scale-down and a scale-up can both have been on "Hub-2" and mean
different servers. `ServerInfo.incarnation` is what tells those apart and
already exists; the number is a label for a conversation, not an identity, and
making it unique over time would mean never reusing one and watching the hub
count climb into three digits.

**The number is assigned, not derived.** It therefore cannot be recomputed from
anything, and a `Server` whose `spec.number` somebody edits by hand will simply
be that number. Nothing validates uniqueness after the fact, which is the same
position `spec.ordinal` is in, where a duplicate is reported rather than
prevented. If duplicates turn out to happen, the honest fix is the one
persistent groups have: report them on the group's conditions.

**The reservation has a TTL, and that is a second way to reach the hole above.**
`expectationTTL` (30 s, in `internal/controller/expectations.go`) bounds how
long an unobserved create is allowed to hold its number. If the create is
still unobserved when its reservation expires, the number stops being held and
the next pass is free to hand it out again while the first create is still on
its way in. The count self-heals from this: a create that lands after all is a
surplus, and surplus is deleted. The number does not self-heal the same way,
because nothing later revisits an assignment, so two live servers can end up
publishing the same one for the rest of both their lives.

**Numbers are not contiguous after a rolling update.** A replacement is
created while the server it is replacing still holds its number, so a full
roll of `{1, 2, 3}` at `maxUnavailable: 1` ends at `{1, 2, 4}` rather than back
at `{1, 2, 3}`: the number the retiring server frees is never reused, because
nothing revisits an assignment, and `NextNumber` has already moved past it by
the time it would be free. It is bounded by replicas plus one, so it cannot
climb without bound, but it is the first thing an operator watching an update
will ask about.
