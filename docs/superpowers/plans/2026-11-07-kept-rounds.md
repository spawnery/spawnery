# Keeping a round of an ephemeral group: implementation plan

**Spec:** `docs/superpowers/specs/2026-11-07-kept-rounds-design.md`

Every command runs in the dev shell. After the API and proto changes, run
`make manifests generate proto` and
`go test ./internal/agentpb -run TestTheWireFormatMatchesTheGolden -update`.

## Tasks

1. **API.** `storage` and `maxInstances` on `Ephemeral` groups with the CEL
   rules of the spec; `Server.spec.worldKey` and `spec.resumed`. Tests: every
   rule both ways in envtest, storage that cannot be removed, `worldKey` set
   once.
2. **Proto.** `key_world` request and result, `ServerState.world_key` and
   `resumed`; regenerate the Go and Java code and the wire golden.
3. **podspec.** The standby volume and `SPAWNERY_WORLD_SYNC=standby` for an
   unkeyed member, the on-demand world mode for a keyed one. Tests: both
   renders, the unchanged ephemeral golden, the hash moving when a group
   gains storage.
4. **Controller.** `ServerView.Resumed` kept out of sizing, changeover,
   failure streak, free slots and the Progressing report; finished resumed
   members swept; `HoldsWorld` for `worldKey`.
5. **instance.** `KeptName` refusing a key of the random suffix's shape.
6. **worldsync.** Standby publish and unpublish without store calls; the key
   request polled, checked against the binding, the deletion marker, the
   lease and the manifest, then the slot re-keyed in place; a standby publish
   that waits while a world keyed in a slot of its name still owes uploads;
   `ReplaceBinding` for a stale binding. Tests against `MemStore`: no store
   calls in standby, every refusal, a held lease, an unreadable manifest,
   restart mid-keying, cache reuse.
7. **agentserver.** `KeyWorld` writing the binding before the immutable
   `spec.worldKey`, replacing a binding whose server is gone; `startServer`,
   `stopServer` and `deleteServer` on kept worlds, reading carriers past the
   cache and deleting every one. Tests on fake clients.
8. **netstate and agents.** `world_key` and `resumed` on the server state;
   group routing in the operator and the Velocity agent skipping resumed
   members; `SpawneryApi.keyWorld`, `ServerInfo.worldKey()`/`resumed()` with
   the old constructor kept. Tests: routing, the Kotlin control file names,
   `RecordCompatibilityTest`.
9. **Docs.** The section in the object-store guide, including what has to be
   rolled out before a group gains storage, and the plugin API pages.

## Review focus

- A refused `keyWorld` never leaves `spec.worldKey` set.
- A binding is replaced only while its world has no manifest and no lease.
- `deleteServer` leaves no running server on a world marked deleted.
- `hash_golden_test.go` passes unchanged; a group without storage does not
  roll.
