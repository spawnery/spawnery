# Keeping a round of an ephemeral group

## 1. What it is for

A round runs on a member of an `Ephemeral` group and ends with its server. A
game plugin sometimes wants one round back later: paused, continued by the
same players on another day, or looked at once more. Today that needs an
`OnDemand` group, which is sized by requests and not by `spareSlots`, so a game
whose rounds are interchangeable until one of them turns out worth keeping has
no fit.

## 2. The shape

An `Ephemeral` group stays sized by `spareSlots` exactly as today. With
`spec.storage` (backend `ObjectStore` only), a running member can give its own
world a key with `keyWorld(key)`. From then on its world is synced to the
object store as an on-demand member's is. `startServer(group, key)` later
starts `<group>-<key>` on that world, outside the group's sizing, and
`deleteServer(group, key)` drops the world. A member that never keys its world
never touches the store.

Non-goals: restoring older generations of a kept world, a TTL, resuming
automatically after a drain, giving a world a second key.

## 3. API

- `ServerGroup.spec.storage` and `spec.maxInstances` are allowed on
  `Ephemeral`. CEL: storage there needs backend `ObjectStore` and `keep`;
  storage needs `maxInstances`, which bounds only resumed members; storage
  cannot be removed once set (`!has(oldSelf.storage) || has(self.storage)`).
  `memberResources` stays `OnDemand`-only.
- `Server.spec.worldKey` (set once, immutable after) and `spec.resumed`
  (immutable, needs `worldKey`), exclusive with `spec.key`. `spec.key` is not
  reused: `netstate.IsPrivateServer`, `fallbackGroup`, `cloudevent` and
  `podspec.BuildDataClaim` read it as "on-demand member".
- Proto, additions only: `CloudRequest.key_world` (18), `CloudResponse.key_world`
  (19), `ServerState.world_key` (17) and `resumed` (18).
- Java API: `SpawneryApi.keyWorld(key)`, `ServerInfo.worldKey()`/`resumed()`
  through a new canonical constructor with the old shape kept, and
  `ServerSelf.worldKey()` as a default method.

## 4. The pieces

**Pod.** A member of an ephemeral group with storage and no `worldKey` gets the
CSI volume in standby (`standby: <ns>/<group>/<server>`, no `world`) and
`SPAWNERY_WORLD_SYNC=standby`. A member with `worldKey` renders exactly the
on-demand world mode. `DesiredServerHash` renders a server without a name or
key, so it is the standby form, and for groups without storage it does not
move (`goldenEphemeralServerDigest` stays).

**Node agent.** A standby publish makes no store call and takes no lease: an
empty directory under `worldState.Slot` (`<ns>/<group>/standby.<server>`, a
name no key or world can have), `ready`, bind. Its unpublish deletes the
directory. `pollKeyRequests` reads `key.request` (only a trigger), then the
operator's `binding.json` under the world's prefix, which must name this slot's
server and pod; checks the deletion marker; takes the lease; refuses if a
manifest exists; and re-keys the state onto the world, keeping the slot's
directory. It answers `key.done` with `<key>` or `failed <key> <reason>`, and
answers again when asked again. Everything after that is the existing code. The
slot's directory stays the world's cache, so a resumed member on the same node
downloads nothing. The ager, the prune sweep and history listings only read
history, object and pack keys; the deletion sweep deletes the binding with the
rest.

**Operator.** `KeyWorld` (answered off the session loop) checks that the caller
is a server of an `Ephemeral` + `ObjectStore` group, that `<group>-<key>` is a
valid name and not of the random suffix's shape, and that no deletion is
pending, no world exists and no other live `Server` carries the key or the
name. It writes the binding with `If-None-Match: *` first and patches the
immutable `spec.worldKey` only once the binding names the caller, so a refused
caller keeps no key; a binding that fails answers `UNAVAILABLE`. A binding
that names a server no longer live, on a world with no manifest and no lease
that names a node, is left over from a request that never finished and is
replaced with `If-Match`. A released lease is an empty object that stays
behind, also after a keying the node refused, so it counts as no world. A process-wide mutex keeps two members from taking one key at
once. `StartServer` on such a group answers the live member that carries
the key, `NOT_FOUND` without a world, and otherwise creates `<group>-<key>` with
`worldKey` and `resumed`, counting only live resumed members against
`maxInstances`. `DeleteServer` finds the carrier by `worldKey`. `StopServer`
accepts a server with `worldKey`.

**Group reconciler.** `ServerView.Resumed`. The sizing, the changeover, the
failure streak, `FreeSlots` and the Progressing report see the group without
resumed members; condemnation, the PDB and the counts see all of them. Keyed
members that the group started count as before. Finished resumed members are
swept like on-demand ones.

**Routing.** `resolveTarget` and the Velocity router skip resumed members for
a group target; they are reachable by name.

**Phase.** `HoldsWorld` is true for `worldKey` too; `ServerKeyMissing` does not
apply to ephemeral groups.

## 5. Calls the designer made

- No lease-freshness check in `StartServer`: a resume after a node died waits
  in `ContainerCreating` up to the lease timeout.
- No TTL: a kept world lives until `deleteServer`.
- Resumed members are `<group>-<key>`, and `keyWorld` refuses a key of the
  random suffix's shape (four characters of the suffix alphabet).
- `startServer` on a key without a world answers `NOT_FOUND`.
- No `dropWorldKey` and no automatic resume after a drain in this version.
- The standby directory lives on the node agent's host path and is not counted
  as the pod's ephemeral storage.

## 6. Testing

Unit and envtest: the CEL rules both ways; `holdsWorld`; sizing with resumed
views; standby and world rendering and the unchanged golden; the node agent
against `MemStore` (no store calls in standby, binding mismatch, existing
manifest, restart mid-keying, cache reuse); CSI standby in a foreign
namespace; `KeyWorld`, `StartServer`, `StopServer` and `DeleteServer` on fake
clients; routing; the Kotlin control file names.

End to end, not automated in this change, because the key has to come from a
game server's own session and needs the Paper agent: on the world-sync kind
cluster, start an ephemeral group with storage, have a test plugin call
`keyWorld`, end the round, `startServer` the key on another node and find the
world's files, then `deleteServer` and see the bucket swept.
