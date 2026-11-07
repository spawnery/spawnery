# Persistent groups: rolling only what is empty, and worlds in the object store

**Status:** design, decided 2026-11-06
**Date:** 2026-11-06

Two features for `Persistent` groups, both of which exist for other group
types already:

- `spec.update.strategy: WhenEmpty`, from
  `2026-09-25-rollout-floor-and-when-empty-design.md` (ephemeral only today).
- `spec.storage.backend: ObjectStore`, from
  `2026-10-06-object-store-worlds-design.md` (on-demand only today).

They are independent of each other and can be built and shipped in either
order. They share a design because one case asks for both: a long-running
world that players stay on for hours, that an image bump must not restart
under them, and that a block volume should not tie to one node.

## 1. What goes wrong today

**An update restarts an occupied world.** A persistent group rolls by
deleting one stale ordinal at a time (`DecidePersistentSize`,
`internal/controller/persistent.go`). Deleting the `Server` is the drain
sequence: the server is deregistered, its players are moved to a fallback,
and the pod stops after at most `spec.drain.timeoutSeconds`. Whether
anybody is on the server plays no part in the decision. Gate B of the 5b
design (every ordinal below `replicas` is `Ready`) waits for the previous
replacement, not for players. A group with a single ordinal is therefore
restarted under its players by every change to the group's spec, the
network's spec or the operator version that moves `DesiredServerHash`.

The CRD refuses `spec.update` on a persistent group
(`self.type != 'Persistent' || !has(self.update)`), so there is no way to
ask for anything else.

**A persistent world can only live on a claim.** The CEL rule
`storage.backend ObjectStore needs type OnDemand and storage.keep` refuses
the object store for persistent groups. The object-store design left it out
because "`Persistent` would need a prefix per ordinal" (its §3.1). That is
all it needs: the node agent, the lease, the snapshots and the deletion
sweep know nothing about group types.

## 2. Goals, non-goals, anti-goals

Goals:

- A persistent group can say that its servers are restarted for an update
  only when nobody is on them.
- A persistent group can keep its worlds in the object store, with the same
  guarantees an on-demand member has: one node writes a world at a time,
  a stop uploads, a running server snapshots every interval.

Non-goals:

- A deadline after which an occupied stale ordinal is restarted anyway. The
  ephemeral `WhenEmpty` refuses `maxStaleSeconds` for the same reason (§3.1).
- Moving an existing persistent group between `Claim` and `ObjectStore` in
  place. The backend is immutable on a persistent group (§4.1). Moving a
  world is the import Job of the object-store design, plus deleting and
  recreating the group (§4.6).
- `restorePoints` and `restoreWorld` for persistent groups (§4.5).
- An export from the store back to a claim.
- Deleting a persistent world from the store. A world of an ordinal that was
  scaled away stays, as a claim does today.

Anti-goals, which would make this a failure even if it works:

- Any existing group restarting because of the upgrade. A persistent group
  on `Claim` without `spec.update` must render the same pod and the same
  hash as before; the goldens in `internal/podspec/hash_golden_test.go` do
  not move.
- A world that starts empty because of a configuration change, while the
  real one sits somewhere the server does not look. §4.1 exists to rule this
  out.
- An occupied world holding up every later changeover stage of the
  network for as long as players are on it (§3.4).

## 3. `WhenEmpty` for persistent groups

### 3.1 API

```yaml
kind: ServerGroup
spec:
  type: Persistent
  replicas: 1
  update:
    strategy: WhenEmpty    # RollingUpdate (default) or WhenEmpty
```

`spec.update` becomes allowed on a persistent group, with **only `strategy`**
meaningful there. 5b refused the whole of `UpdateSpec` because exposing it
exposes `maxStaleSeconds`, which means nothing on the persistent path, and
"a field that does nothing in its context is a trap" (5b §1). That argument
still holds for the other three fields, so they stay refused, by CEL on the
spec, replacing the blanket rule:

- `maxUnavailable` must be unset or 1. It defaults to 1 when `spec.update`
  is present, so the rule tests the value, not presence. One ordinal down at
  a time is fixed (5b §2).
- `maxStaleSeconds` must be unset or 0. Same reason for the default.
- `minAvailable` must be unset. It is a floor for groups that build extra
  servers during a roll, and a persistent group never builds one.

Each rule gets its own message naming the field and the type, so that the
person who sets one learns why it is refused. `strategy: RollingUpdate` and
an absent `spec.update` mean today's behaviour, unchanged.

`spec.update` is part of no rendered pod and of no config value, so setting
or changing it restarts nothing. The hash table test gets a row for that.

`OnDemand` keeps refusing `spec.update`; nothing rolls there.

### 3.2 The rule

`DecidePersistentSize` keeps its three takedown classes and their order
(surplus, stale, resize-pending) and both gates of 5b §2.1. Under
`WhenEmpty`, the **stale** and the **resize-pending** classes consider only
ordinals that are **known empty**: `Players == 0` and the count trusted
(`!Stale` on the `ServerView`), which is the test the ephemeral rule uses
(`internal/controller/scaling.go`, `selectRetirement`).

- An occupied stale ordinal is never nominated. It stays `Ready`,
  registered and joinable for as long as it has players.
- The highest known-empty stale ordinal is nominated, as the highest stale
  one is today. An occupied higher ordinal does not hold up an empty lower
  one.
- Gate A (nothing else of the group is leaving or reserved for deletion)
  and Gate B (every ordinal below `replicas` is `Ready`) apply as today. An
  occupied stale ordinal is `Ready` and does not trip Gate B.
- Surplus removal is unchanged. Lowering `replicas` is an explicit
  instruction, often given because something is wrong; it takes the highest
  surplus ordinal down whether or not it is occupied, as 5b argues for
  Gate B.
- Node condemnation is unchanged. A node that is leaving takes its
  servers with it.

A resize-pending restart is included because it is a restart the group
chooses on its own, like a stale one. Under `ObjectStore` there is no claim
and no resize; the class only matters on `Claim`.

How an ordinal becomes empty is the game's business. Players leave, or a
plugin moves them elsewhere. A person who wants an update now deletes the
`Server` object; the group recreates it under the same name with the current
spec, as after any takedown.

The rule accepts one race. The decision is taken on the last reported
count, so a player who joins between the decision and the deregistration is
moved off by the drain, as every player is today. Closing the door first
and confirming the count before deleting would close the window, at the
cost of a second state on the persistent path. The window is one reconcile
pass wide, and in the bad case one player gets today's behaviour.

### 3.3 Waiting is visible

A group whose stale ordinals are all occupied is not stuck, but from the
outside it looks the same as one that is. The `Progressing` condition
carries a new reason, `WaitingForEmpty`, while stale ordinals remain and
none of them can be nominated because each is occupied, with a message that
names them (`survival-0 has 3 players`). It takes the place of
`ReplacingServers` for such a group. The stage-wait reasons and
`RetireeStuck` stay ahead of it in `reportProgressing`, and the separate
`Degraded` and `BackingOff` conditions are not touched.

### 3.4 The changeover stage

Persistent groups take no budget place, but they gate later stages: a group
in `Waiting` or `Begun` keeps every group of a higher `changeoverStage` from
starting its changeover (`changeoverRefused`, `earliestStageBefore`,
`internal/controller/changeover.go`). `ownPersistentChangeover` reports
`Begun` from the first stale takedown until no stale ordinal remains, and
`Waiting` before it. Under `WhenEmpty` either can last as long as players
stay, and a later stage would wait for it.

So a `WhenEmpty` group gets `Deferred`, as ephemeral `WhenEmpty` does (its
§4.2), with one difference stated below. For a `WhenEmpty` group the state is
computed from the views each pass, without memory of the previous state:

| situation | state |
|---|---|
| no stale server that is not held or terminal | `None` |
| a stale ordinal below `replicas` is leaving or reserved for deletion | `Begun` |
| otherwise, a stale ordinal below `replicas` is known empty, or a stale ordinal is surplus | `Waiting` |
| otherwise (every stale ordinal is below `replicas` and occupied) | `Deferred` |

A stale surplus ordinal keeps the group `Waiting`, as it does today, since
surplus removal ignores occupancy.

Nominating a stale ordinal always needs the stage gate, from whatever state.
`Waiting` asks it as today. A `Deferred` group never nominates, because it
has nothing empty to nominate; when an ordinal empties, the next pass sees
`Waiting` and asks the gate. Lower stages therefore still go first, which is
what the stages exist for (`2026-10-01-changeover-stages-design.md` §1).
`Deferred` does not gate higher stages (`changeoverInFlight` already treats
it so), so an occupied world holds up nobody. A second spec change while an
ordinal is occupied needs nothing extra: the state is recomputed and the
gate asked again.

The difference from the ephemeral `Deferred`: there it also means that the
group's new generation stands (a current server is Ready). A persistent
group has no extra server to stand up first, so here `Deferred` means only
"waiting for players, holding nobody". The API comment of
`ChangeoverDeferred` says both.

A `RollingUpdate` persistent group keeps today's states.

### 3.5 What does not change

- `DesiredServerHash`, `Server.spec.podHash` and the adopt-on-empty rule of
  5b §3.6.
- The one-ordinal invariant of 5b §2: at most one takedown the rule
  nominates is in flight. `WhenEmpty` narrows which ordinal is nominated,
  never how many.
- Creation of missing ordinals, all at once.

## 4. `ObjectStore` for persistent groups

### 4.1 API

```yaml
kind: ServerGroup
spec:
  type: Persistent
  replicas: 1
  storage:
    backend: ObjectStore
    size: 6Gi           # required, ignored under ObjectStore (object-store §8)
    keep:
      - worlds/world
      - plugins/Example/data
    retention:
      last: 6
      daily: 7
```

- The CEL rule becomes `ObjectStore needs type OnDemand or Persistent, and
  storage.keep`. `keep` stays required for the reason the object-store
  design gives: the keep entries are exactly what is synced.
- **`backend` is immutable on a persistent group** (CEL, comparing with
  `oldSelf`; absent and `Claim` are the same value). Changing it would change
  the pod hash, roll every ordinal, and start each on a world the new
  backend does not hold: a fresh world from an empty store, or a fresh claim.
  The old world would stay where the server no longer looks. The message
  points to the guide's migration section (§4.6). An on-demand group keeps
  the freedom to switch, since its members are short-lived and the guide
  already tells people to import before starting.
- `retention` is allowed, as for on-demand. It prunes at every upload; a
  running persistent server uploads every snapshot interval, so its history
  is pruned continuously rather than at stops.
- `storageClassName`, `accessModes` and `annotations` stay valid and are
  ignored, as for on-demand.

### 4.2 The world's name

A persistent world is `<namespace>/<group>/<ordinal>`, the ordinal in
decimal without leading zeros. It is the same identity the claim has today
(`<group>-<ordinal>-data`, from `PersistentServerName`), and it uses the
store layout of object-store §4.1 unchanged: the third segment is a key, and
an ordinal is a valid key.

The `Server` does not get `spec.key`. That field is documented as set for an
on-demand member and no other server, and other code reads it as "this is an
on-demand member". The world name is derived in one place in `podspec` (the
key for an on-demand member, the ordinal for a persistent server), and both
the volume attributes and every operator path that names a world use it.

The `ServerKeyMissing` acceptance check becomes "the server has no world
name": an on-demand member without a key, or a persistent server without an
ordinal. Neither happens through the operator's own creation paths.

A group deleted and recreated under the same name, same namespace, finds the
same worlds. That is what §4.6 relies on. An on-demand group and a
persistent group cannot share a name at the same time; one replacing the
other under the same name would share key `0`. That is the same as a claim
`<group>-0-data` outliving its group today, and is not guarded against.

### 4.3 The pod

The data volume is the CSI inline volume of object-store §3.2, with the
world name of §4.2. No claim is created for the ordinal. The pod gets
`SPAWNERY_WORLD_SYNC=1`; the interval is added after the hash is taken, as
today. Without `--world-sync` no pod is created (`WorldSyncOff`), as for
on-demand.

A persistent group on `Claim` renders exactly as before.

### 4.4 What the node agent already implies

Nothing in this section needs code. The guide documents each point.

- The ordinal is no longer pinned to a node by its volume. A replacement
  pod may schedule anywhere. On another node it waits in
  `ContainerCreating` until the old node has uploaded the final snapshot and
  released the lease (object-store §4.3 step 3), then downloads. By
  object-store §2, a 150 MB world downloads in 1 to 2.5 s and uploads in 3
  to 6 s.
- 5b holds an ordinal through its drain because two pods on one
  `ReadWriteOnce` volume hang. Under `ObjectStore` the reason becomes that
  the lease refuses two nodes on one world, and the rule stays as it is.
- When a node dies, the world loses the play since its last uploaded
  snapshot. The ordinal's pod is named after its `Server`, and no
  replacement is created while a pod of that name exists
  (`PodNameTerminating`), so the ordinal waits for the pod on the dead node
  to be force-deleted, as it does on `Claim` today (`persistent-worlds.md`,
  "An ordinal waits, visibly, for a pod that a dead node will never finish
  terminating"). After the force-delete the replacement waits for the dead
  node's lease to go stale (10 minutes) before it can take the world. The
  guide's warning that a force-delete risks two pods on one volume reads
  differently under `ObjectStore`: the lease refuses the second writer. The
  guide gets that difference; nothing is built for it.
- Snapshots run all the time. An on-demand member snapshots while it is
  started, and a persistent server is always started. That is one copy of the changed
  files every `snapshotInterval` and one upload, for as long as the group
  exists.
- A scaled-away ordinal's world stays in the store, as its claim stays
  today. Raising `replicas` again resumes it. The age limit of
  `2026-10-20-retention-max-age-design.md` prunes its history like any
  world nobody plays, if the group sets one.

### 4.5 What stays on-demand only

`restorePoints(group, key)` and `restoreWorld(group, key, generation)`
refuse a persistent group, as they do today (`ErrGroupNotOnDemand`). A
restore needs the world's server stopped and kept stopped while the
manifest is rewritten; spawnery never stops a persistent ordinal on request,
and a stopped one is recreated within a pass. A restore for persistent
groups needs a hold on one ordinal, which is a design of its own. The
history is kept in the meantime, so a restore built later finds it.

`startServer` and `deleteServer` stay on-demand only, as today.

### 4.6 Moving a world onto the store

Because the backend is immutable (§4.1), a persistent group moves to the
store like this, which the guide documents step by step:

1. Lower `replicas` to 0, or delete the group. Either way the claims stay
   (claims carry no owner reference). Wait until no server of the group is
   left, so no pod has a claim mounted.
2. For each ordinal, run the import Job of object-store §7 against
   `<group>-<ordinal>-data` with world `<namespace>/<group>/<ordinal>` and the
   group's keep list. The import refuses a world that already has a
   manifest.
3. Delete the group, if step 1 did not, and create it again under the same
   name with `backend: ObjectStore`. Each ordinal finds its manifest.
4. Delete the claims once the worlds are confirmed in the store.

Going back has no tool, since there is no export. The guide says so.

The import must not run while a server of the group holds the claim; that
is the same rule the import already states for on-demand members.

### 4.7 Docs and chart

- `docs/guides/persistent-worlds.md`: a section on `ObjectStore` with §4.4's
  consequences and the §4.6 move.
- `docs/guides/object-store-worlds.md`: drop "only `OnDemand`", name the
  world layout for persistent groups.
- `docs/guides/updates-and-drain.md`: `WhenEmpty` for persistent groups,
  forcing an update by deleting the `Server`, and `WaitingForEmpty`.
- The CRD field comments of `StorageSpec.Backend` and `UpdateSpec` say which
  types take them. The generated CRD reference follows.
- The object-store design's §8 "Only `OnDemand` groups" no longer holds; the
  design files are history and stay as they are.
- No chart change.

## 5. Versions

Both features add behaviour to a CRD and to decisions the operator makes,
so the release takes a minor step (CLAUDE.md, "Which number moves").
`operatorVersion` moves. `imageVersion` does not: the agent and the images
already do everything a persistent `ObjectStore` server needs, and the
requirement of game images 0.24.3 or later (the bootstrapper that waits for
the world) carries over unchanged.

## 6. Testing

**Decision tables**, beside the existing ones for `DecidePersistentSize`:

- `WhenEmpty` skips an occupied stale ordinal and nominates a lower empty
  one; nominates the highest of several empty ones; skips one whose count is
  untrusted; nominates nothing when all stale ones are occupied.
- `WhenEmpty` does not hold up surplus removal of an occupied ordinal.
- `WhenEmpty` applies to resize-pending: an occupied resize-pending ordinal
  is skipped.
- Gates A and B still hold under `WhenEmpty`: with an empty stale ordinal
  and another ordinal not `Ready`, nothing is nominated; with a takedown in
  flight, nothing else is.
- `RollingUpdate` and absent `spec.update` nominate as today (the existing
  table, unchanged, must still pass).

Mutations that must die: dropping the emptiness check (a stale occupied
ordinal gets nominated); reading `Players == 0` without `!Stale`; applying
the emptiness check to surplus.

**Changeover state**: the table of §3.4, row by row, for a `WhenEmpty`
group, including a previous state of `Deferred` going to `Waiting` when an
ordinal empties and to `Begun` once it is taken down. A later-stage group is
not refused while the persistent group is `Deferred`. A `Waiting` group
whose lower stage is in flight nominates nothing, also when it was
`Deferred` the pass before (mutation: skipping the gate after `Deferred`).

**Pod and hash**: a persistent `ObjectStore` server renders the CSI volume
with world `<ns>/<group>/<ordinal>` and no claim; a persistent `Claim`
group's hash is unchanged (the existing goldens); setting `spec.update` on a
persistent group does not change `DesiredServerHash`.

**envtest** for the CEL rules: `spec.update.strategy: WhenEmpty` accepted on
`Persistent`; `maxUnavailable: 2`, `maxStaleSeconds: 60` and `minAvailable: 1`
refused there, each with its message; `spec.update` still refused on
`OnDemand`; `ObjectStore` accepted on `Persistent` with `keep`, refused
without; `backend` change refused on `Persistent` in both directions,
including absent to `ObjectStore`; `backend` change still accepted on
`OnDemand`.

**envtest for the controller**: a persistent `WhenEmpty` group with one
occupied ordinal and a spec change keeps its `Server`, reports
`WaitingForEmpty` and `Deferred`; once the agent registry reports zero
players, the `Server` is deleted and recreated with the new hash. A
persistent `ObjectStore` group creates no claim.

**e2e**: `hack/e2e-worldsync.sh` gains a persistent group: what ordinal 0
wrote survives the deletion of its pod, and the replacement (on another node
where the cluster allows) finds it, as the on-demand case shows today.

The main e2e (`hack/e2e.sh`) runs no game process and nobody can join, so
the `WhenEmpty` case goes into the tutorial run (`hack/e2e-tutorial.sh`),
the only one with real joins (`startHeldJoin` in
`test/e2e/tutorial_test.go`): a held join keeps ordinal 0 up through a spec
change, and after it ends the ordinal restarts with the new hash. If the
tutorial's network cannot take a persistent group without changing what the
tutorial teaches, the controller envtest of this section is the evidence
and the e2e case is dropped; the plan says which.

## 7. Calls the designer made

Settled without asking, and cheap to turn around:

- Only `strategy` is exposed on persistent groups; the other `UpdateSpec`
  fields are refused by value, not hidden by a separate type (§3.1).
- No forced update after a deadline (§2, §3.2).
- The join race between decision and deregistration is accepted (§3.2).
- `WhenEmpty` also governs resize-pending restarts, and not surplus
  removal or node condemnation (§3.2).
- A deferred group holds no stage, and an ordinal that empties still waits
  for lower stages (§3.4).
- The `WhenEmpty` e2e goes into the tutorial run, or is dropped for the
  envtest if the tutorial cannot take it (§6).
- World name `<ns>/<group>/<ordinal>`, derived in `podspec`, no
  `spec.key` on persistent servers (§4.2).
- History is kept, but restore for persistent groups is left for a later
  design (§4.5).

Decided with the maintainer on 2026-11-06: the backend is immutable on a
persistent group, and a move goes through import and recreation (§4.1,
§4.6).
