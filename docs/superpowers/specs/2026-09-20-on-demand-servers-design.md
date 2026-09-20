# A group that starts nothing by itself

**Status:** design, decided 2026-09-20
**Date:** 2026-09-20

## 1. What this operator cannot do today

A private server is one player's world, started when they ask for it and
stopped when they are done, keeping what they built between the two. Cyperia
runs them on CloudNet today (`cyperia/private-server`, branch `develop`) behind
an `Orchestator` interface with one implementation, and spawnery is to replace
that platform.

Nothing here can express it. The three sizing rules this operator has are a
number a person writes (`Persistent`), a number free slots imply (`Ephemeral`),
and a number a boost adds for a while — and all three answer *how many*, while
a private server is a question of *which one*. The consequences are concrete:

- **A server cannot be asked for.** `ScaleBoost` raises a floor; it cannot say
  which world the new server carries, and the servers a boost creates are
  interchangeable by construction.
- **A persistent identity is an ordinal.** `spec.replicas` fills 0..N-1 densely,
  and lowering it takes the top ordinal. A set of worlds where player 4000 has
  one and players 1 through 3999 do not is not a range.
- **A game pod holds no Kubernetes credentials.** `AutomountServiceAccountToken`
  is false and the projected token's audience is `spawnery-operator`
  (`internal/podspec/proxy.go`, `internal/podspec/agent.go`), so the consumer
  cannot go around the operator and create objects itself. That is deliberate
  and stays; it is also what makes this a spawnery feature rather than a
  client's problem.

## 2. The shape

**A third `ServerGroup` type, `OnDemand`, is a template and nothing else.** It
carries what every group carries — image, resources, mounts, `extraPlugins`,
`configOverlay`, `env`, `storage`, `drain` — and it has no sizing rule at all.
It creates no server, it deletes none as surplus, and it rolls none.

**Its members are named, not counted.** A member is an ordinary `Server`
called `<group>-<key>`, where the key is whatever the caller uses to identify
the world: for Cyperia that is the instance UUID their database already keys
on. Its world is the claim `<group>-<key>-data`, and because this operator
never deletes a claim (`docs/guides/persistent-worlds.md`), the same key later
finds the same world.

**The caller is a plugin on the agent channel.** Two requests join `retire`
and `boost`, and they work the way those do: the plugin asks, the operator
writes the object, and the answer is about what it wrote
(`internal/agentserver/writer.go`). No new RBAC — creating and deleting
`Server` objects and creating claims are grants the operator already holds for
its other groups.

Everything that makes a server playable is therefore inherited rather than
rebuilt: the pod spec, the phase machine, the readiness probe, the drain,
proxy registration, the CA and the agent channel, the failure retention, the
metrics.

### What changes for whom

- **An installation without an `OnDemand` group** sees one new enum value in a
  CRD it does not use. Nothing else moves.
- **A plugin** gains two methods and loses nothing. `SpawneryApi` is consumed
  and not implemented, so an addition breaks no caller.
- **A game server's network picture loses entries it never wanted** — see §3.6,
  the one place where an existing contract gets narrower.

## 3. Mechanics

### 3.1 The type

`ServerGroupOnDemand ServerGroupType = "OnDemand"` joins the enum, and the CEL
rules on `ServerGroupSpec` gain the shape of it, in the form the existing ones
have:

- `spec.scaling` is not allowed — there is no sizing rule to configure.
- `spec.replicas` is not allowed — the same reason.
- `spec.update` is not allowed — see §3.5; nothing rolls.
- `spec.storage` is required — a member without a world is an `Ephemeral`
  server that took the long way round.
- `spec.maxInstances` is required, and allowed for this type only.

`MaxInstances int32` is new on `ServerGroupSpec`, minimum 0. It is the number
of members of this group that may exist at once, checked before a create the
way `boost` checks its headroom (`internal/agentserver/writer.go`, the
`MaxReplicas - MinReplicas - Boosted` arithmetic). Zero is a legal value and
means the group is closed: an installation turning private servers off leaves
the worlds standing and refuses new starts, which is the state an incident
wants and a deletion would not give.

It is a fleet ceiling and not a per-player quota. Who may have how many
private servers is a product question its owner answers in its own database,
where the player, the purchase and the ban live; an operator enforcing it
would need all three.

`spec.type` is already immutable, so no group crosses into or out of this
type.

### 3.2 The name is the identity

`StartServerRequest` carries a group and a key, and the operator mints
`<group>-<key>`. Two bounds fall out of Kubernetes and are checked by the
writer rather than discovered by a pod that never schedules:

- The key must be a DNS-1123 label, because the composed name is one.
- The composed name must fit 63 characters. A 36-character UUID therefore
  bounds an `OnDemand` group's own name at 26, which is roomy for
  `private-servers` and worth saying out loud in the CRD's documentation,
  since the failure lands on a player's start rather than on the admin's
  `kubectl apply`.

A second start on a key whose server exists collides on the name. That is the
answer, not an accident to paper over: the operator returns `already_running`
with the server's name, and no caller needs a lock to ask twice.

`spec.ordinal` stays unset on these servers. Its presence means "persistent"
elsewhere in this operator, and a member of an `OnDemand` group is not that:
its world is addressed by a key its caller chose, not by a position in a
range. `spec.number` stays 0, which already reads as "nobody numbered this
server" and makes a reader fall back to the name — and the name here is the
one thing that means something.

**`ServerSpec` gains `Key string`**, set for these members and empty for every
other, and it earns its place the way `ordinal` did: a `Server` has to be able
to say what kind of member it is without its group. `server_controller.go`
reconstructs a synthetic group from the `Server` alone when the real one is
gone and infers the type from `spec.ordinal != nil`
(`internal/controller/server_controller.go:859`) — an inference that is
exhaustive only while there are two types. With three, a member with neither
ordinal nor key marker reads as ephemeral, which is the one answer that is
wrong about its world. The field also spares every reader the job of parsing
the key back out of the name.

### 3.3 The world

`podspec` decides the world's storage from the group's type, and the claim's
name from the server's (`internal/podspec/server.go`, `dataVolume` and
`DataClaimName`). The change is a predicate: `keepsWorld(type)` is true for
`Persistent` and `OnDemand`, and it replaces the type comparison at the claim
volume and at claim creation.

**It does not replace the one in `restartPolicy`.** A persistent server keeps
`Always` because its world is a claim and its pod is meant to come back. An
`OnDemand` member gets `Never`: a player typing `/stop` is a player stopping
their server, and a policy that cannot tell that from a crash would restart it
against them. What happens after the pod ends is §3.5.

### 3.4 The two requests

On the wire, inside `CloudRequest`/`CloudResponse`
(`proto/spawnery/agent/v1alpha1/agent.proto`):

```proto
message StartServerRequest {
  string group = 1;
  string key = 2;
}

message StartServerResult {
  string server = 1;
  bool already_running = 2;
}

message StopServerRequest {
  string server = 1;
}

message StopServerResult {
  string server = 1;
}
```

Neither carries a namespace, for the reason `RetireRequest` carries none: the
pod's own token authenticated into exactly one namespace, and there is no
field in which another network could be named.

In Java, `SpawneryApi` gains `startServer(String group, String key)` returning
the server's name and `stopServer(String server)`, both `CompletionStage` on
both platforms, both round trips.

A create is refused, as a `RequestError` with `REFUSED` and the operator's own
sentence, when: the group does not exist, the group is not `OnDemand`, the key
is not a label or the name would not fit, or `maxInstances` is reached.

`stopServer` deletes the `Server`. Everything after that is the path a
scale-down already takes: the players on it are moved through the proxies
inside `spec.drain.timeoutSeconds`, the pod goes, and the claim stays. There
is no separate "save the world" step and there is nothing to add: the world
was never anywhere else. (The consumer's `Orchestator` calls this
`stopAndDeployInstance`, a name CloudNet earned by copying a world into a
template on every stop. Here the deploy half is simply gone.)

**Who may call it is who may install a plugin in the namespace**, exactly as
for every other call on this channel, and the group's `maxInstances` is what
bounds the damage. This is the same boundary `docs/plugin-api/index.md`
already describes, and it is the reason `maxInstances` is required rather than
defaulted: a group whose ceiling nobody chose is one nobody thought about.

### 3.5 The controller does almost nothing

The `ServerGroup` reconciler learns the type and then declines most of its own
work for it:

- **No sizing.** `size()` is not consulted; the group creates no server and
  condemns none. The existing surplus path must not see these members, which
  is the one place where a missed branch would delete a player's running
  server.
- **No rolling update.** A spec edit does not make a running member stale.
  The pod hash is still stamped at creation and still tells a reader which
  spec a member started with, but nothing acts on a difference: a member
  starts with whatever the group says at *its* start, and an image bump
  reaches a world the next time its owner starts it. Throwing a player out of
  their own world to apply a version bump is the opposite of what a private
  server is for, and it is why `spec.update` is rejected for this type rather
  than ignored.
- **It does clean up.** A member whose pod has ended is deleted, and with it
  the object: the world is on the claim, so there is nothing about a stopped
  member worth keeping, and a leftover object holds the one name its owner
  needs to start again. A member that reached `Failed` is deleted on the same
  rule rather than retained — but a start on a key whose last run failed must
  not be refused by the corpse of that run, and this is the ordering that
  guarantees it.
- **It still reconciles the group's own furniture**: the ConfigMap, the
  conditions, the storage-resize condition, the PDB.

### 3.6 The network picture is split

`internal/netstate` builds one picture per namespace and both agent kinds
receive it — `internal/proxyreg` for proxies, `internal/serverreg` for
backends. Unfiltered, a network with three hundred private servers running
hands every lobby three hundred entries and a fresh picture on every start and
stop, for servers no lobby will ever send anyone to.

`Source.Build` gains an audience. Members of an `OnDemand` group, and the
group itself, reach the proxies' picture and not the backends'. Proxies need
them because that is where routing lives and where the consumer's plugin runs;
a backend needs neither.

**This is the only contract in this design that gets narrower**, and it is
deliberately done now rather than later: once a plugin on a game server can
see private servers in `servers()`, taking them back out is a breaking change
to somebody's code. A private server's own plugins lose nothing they had —
they never had a picture containing themselves before this feature existed,
their own `announce` and `acceptJoins` are their session and not the picture,
and what the consumer's system knows about its instances it knows from its own
database.

`CLAUDE.md`'s architecture section says both agent kinds see one identical
picture. That sentence stops being true and is part of this change.

`GroupState.Kind` on the wire gains `ON_DEMAND`. It is an enum that already
documents what an agent does with a value it predates — read it as unknown,
never fail to parse — so adding one is inside its own contract, and
`serverGroupKind` (`internal/netstate/netstate.go:198`) gains the case rather
than letting a new type report as unspecified.

### 3.7 The type is a binary everywhere today

This is the largest single risk in the change and the reason it is named in
the design rather than found during it: **nothing in this operator asks which
of the types a group is. It asks `IsEphemeral()`** — fourteen places, and each
of them silently files a third type under "the other one". Every site needs a
deliberate answer, and the plan carries them one by one. The shape of it:

- **Right by inheritance, and the reason this design is cheap.** Claim
  creation (`server_controller.go:359`) and claim growth plus the resize
  condition (`:284`) are guarded by `!IsEphemeral()` and are exactly what an
  `OnDemand` member wants — `AlreadyExists` on the claim is already documented
  there as the ordinary case for a server that comes back to the world it had.
  The boost headroom check (`agentserver/writer.go:189`) refuses a boost on
  anything not ephemeral, which is also correct here.
- **Right by arithmetic, and to be made explicit anyway.**
  `DesiredReplicas()` falls through to `spec.replicas`, which an `OnDemand`
  group does not have, and returns 0. The answer is correct and the route to
  it is an accident; it gets its own branch.
- **Wrong until each is looked at.** The five sizing and update branches in
  `servergroup_controller.go` (`:427`, `:528`, `:588`, `:751`, `:898`) are
  where a missed inversion deletes a player's running world to satisfy a
  replica count that does not exist. These carry the risk of the change.
- **Wrong and fixed by `spec.key`.** The synthetic group in
  `server_controller.go:859`, per §3.2.

`IsEphemeral()` stays — it is honest about what it answers. What must not
appear anywhere is a new `!IsEphemeral()` standing in for "persistent".

## 4. The consumer side

Out of scope for this repository and recorded here because the design was cut
against it. In `cyperia/private-server`:

- **`orchestrator:spawnery`**, a module beside `orchestrator:cloudnet`,
  implementing the same `Orchestator` interface against
  `cloud.spawnery:spawnery-api` as a `compileOnly` dependency — the way
  `core` and `arcadia` already consume it. `streamRunningServices()` is a
  lookup in the agent's local mirror filtered to the group, a phase maps to
  their `InstanceState`, `incarnation` becomes their `InstanceServiceIdModel`,
  and the owner of an instance comes from their own database, as it does for
  CloudNet. Which orchestrator a proxy runs is a configuration entry; CloudNet
  stays until the platform is retired.
- **Resetting a world** is a flag in their database and a wipe performed by
  their own plugin on the private server at its next start, before the world
  loads. It needs nothing from this operator and no cluster access.
- **Deleting an instance for good** — the player's own action, with nothing
  left behind — needs the claim gone, and this operator will not do that. It
  is a job on their side: a `CronJob` in the game namespace that takes its
  work from their database and deletes the claims of instances marked deleted,
  refusing any claim whose `Server` still exists.

  **Its right cannot be narrowed to those claims.** Kubernetes RBAC selects by
  name, never by label, and these names are minted at runtime; the namespace
  is shared with every other world because a `Network` is one namespace and a
  player cannot be moved across networks. So the job holds `delete` on every
  claim in the namespace and its own code is the guard. That it is a job
  pulling work rather than a service answering the game is what keeps the
  reach out of the plugin's hands, and it is why this is not a dashboard
  button wired to a deletion API.

## 5. What does not change

- No RBAC grant is added, to the operator or to anyone else. In particular the
  operator still holds no `delete` on `persistentvolumeclaims`, and
  `internal/rbacaudit`'s table is untouched.
- No existing group type changes behaviour, and no existing installation is
  asked to do anything.
- The agent channel's authentication, the pod-bound token and the namespace
  boundary are as they were.
- A claim is still never deleted by this operator, on any path.

## 6. Version

A minor step, by the rule of 2026-09-08: a new type, two new CRD fields
(`ServerGroupSpec.MaxInstances`, `ServerSpec.Key`), two requests and one enum
value on the wire, and two methods added to the Java API. `operatorVersion`
and `imageVersion` both move — the agent jar changes — and the chart's
`version` and `appVersion` with them, since `templates/crds.yaml` changes.

`make manifests generate proto` covers the generated half: the CRDs, the
chart templates, `zz_generated.deepcopy.go`, `internal/agentpb`,
`agent/common/src/proto/java` and the generated CRD reference page. No RBAC
marker moves, so `internal/rbacaudit`'s table does not.

## 7. Out of scope

- Per-player quotas (§3.1).
- Anything about the owner of a world reaching this operator. The consumer's
  database has the player, and a second place to read that from is a second
  place for it to be wrong.
- Claim deletion, reclamation and orphan sweeping (§4).
- Moving an existing CloudNet world onto a claim. A migration is its own
  design and belongs to whoever schedules the platform switch.
