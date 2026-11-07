# WhenEmpty and ObjectStore for persistent groups: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `Persistent` group can restart its ordinals for an update only when they are empty (`spec.update.strategy: WhenEmpty`), and can keep its worlds in the object store (`spec.storage.backend: ObjectStore`).

**Architecture:** Two independent strands on the persistent path. WhenEmpty: CEL admits `spec.update` with only `strategy` on a persistent group; `DecidePersistentSize` filters its stale and resize-pending classes to known-empty ordinals; `ownPersistentChangeover` computes `Deferred` for a group whose stale ordinals are all occupied; `reportProgressing` says `WaitingForEmpty`. ObjectStore: CEL admits the backend on persistent groups and freezes it there; `podspec.WorldKey` names a persistent world by its ordinal, and everything else (node agent, lease, snapshots, retention) is reused unchanged.

**Tech Stack:** Go (controller-runtime, kubebuilder CEL, envtest), bash e2e harnesses on kind.

**Spec:** `docs/superpowers/specs/2026-11-06-persistent-when-empty-and-object-store-design.md`

## Global Constraints

- Every command runs in the dev shell from the repository root: `nix develop -c <command>` (written `nd <command>` below).
- On the `dev` VM (2 CPUs), envtest packages run with `-p 1`; check `hostname` before a heavy run.
- Commits: Conventional Commits with a scope (`feat(api): …`, `feat(controller): …`, `docs(guides): …`), body wrapped at 72, no line over 150. No line naming Claude, Claude Code or Anthropic, no `Co-Authored-By`, no "Generated with".
- New files carry the header `Copyright paul_wtf.` plus the Apache text, as existing files do.
- Comments: none unless a reader cannot derive it from the code beside it. No history, no measurements in code.
- After any change to API types or markers: `nd make manifests generate`, and commit `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `zz_generated.deepcopy.go` and `docs/reference/crds.md` with it.
- `internal/podspec/hash_golden_test.go` passes unchanged in every task. A persistent group on `Claim` without `spec.update` renders a byte-identical pod.
- Names pinned by the spec and this plan: condition reason `WaitingForEmpty` (constant `ReasonWaitingForEmpty`); world name `<namespace>/<group>/<ordinal>`, ordinal in decimal without leading zeros.
- No version bump in these commits. The release (minor step, `operatorVersion` moves, `imageVersion` does not) is a separate step on instruction.

## Review Focus

- A persistent `WhenEmpty` group whose only ordinal reports players through an untrusted count (agent session gone, `Stale`): never restarted, reported as waiting, not as replacing (Task 3 `TestWhenEmptyPersistentSkipsAnUntrustedCount`, Task 5 row "untrusted count").
- `spec.update: {}` on a persistent group (empty object, every field defaulted): accepted, behaves as `RollingUpdate` (Task 1 `TestPersistentUpdateTakesOnlyStrategy` case "empty update").
- A persistent group created with no `backend`, then updated to `backend: Claim` explicitly: accepted, since absent and `Claim` are one value (Task 2 `TestPersistentBackendIsImmutable` case "absent to Claim").
- A persistent `WhenEmpty` group with a stale surplus ordinal that is occupied, after `replicas` was lowered: the surplus ordinal is removed anyway (Task 3 `TestWhenEmptyPersistentStillRemovesOccupiedSurplus`).
- A persistent `ObjectStore` group's hash is the same for every ordinal, so ordinal 0 and ordinal 1 are not stale against each other (Task 6 `TestAPersistentObjectStoreHashIgnoresTheOrdinal`).

---

### Task 1: `spec.update` on persistent groups takes only `strategy`

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (replace the CEL rule `spec.update is not allowed for type Persistent` on `ServerGroupSpec`; doc comment of `UpdateSpec` and `UpdateStrategy`)
- Modify: `api/v1alpha1/common_types.go` (doc comment of `ChangeoverDeferred`)
- Test: `api/v1alpha1/servergroup_envtest_test.go`
- Generated: CRDs, chart CRDs, `docs/reference/crds.md`

**Interfaces:**
- Produces: CEL rules on `ServerGroupSpec`, each with this exact message:
  - `self.type != 'Persistent' || !has(self.update) || !has(self.update.maxUnavailable) || self.update.maxUnavailable == 1` → `spec.update.maxUnavailable must be 1 for type Persistent: one ordinal is down at a time`
  - `self.type != 'Persistent' || !has(self.update) || !has(self.update.maxStaleSeconds) || self.update.maxStaleSeconds == 0` → `spec.update.maxStaleSeconds must be 0 for type Persistent: a persistent server is never drained for staleness`
  - `self.type != 'Persistent' || !has(self.update) || !has(self.update.minAvailable)` → `spec.update.minAvailable is not allowed for type Persistent: a persistent group builds no extra server`
- `ChangeoverDeferred` comment: for an ephemeral or proxy group, the new generation stands and the stale ones wait for their players; for a persistent group, every stale ordinal waits for its players. Either way it holds no budget place and gates no later stage.

- [ ] **Step 1: Write the failing test** `TestPersistentUpdateTakesOnlyStrategy` in `servergroup_envtest_test.go`, table over `persistentGroup(ns, name)` with `Spec.Update` set:
  - "WhenEmpty" `{Strategy: WhenEmpty}` → created; read back `Spec.Update.MaxUnavailable == 1`.
  - "RollingUpdate" `{Strategy: RollingUpdate}` → created.
  - "empty update" `{}` → created.
  - "maxUnavailable 2" `{Strategy: WhenEmpty, MaxUnavailable: 2}` → error containing `maxUnavailable must be 1 for type Persistent`.
  - "maxStaleSeconds" `{MaxStaleSeconds: 60}` → error containing `maxStaleSeconds must be 0 for type Persistent`.
  - "minAvailable" `{Strategy: WhenEmpty, MinAvailable: ptr.To[int32](1)}` → error containing `minAvailable is not allowed for type Persistent`.
  - Also `onDemandGroup` with `{Strategy: WhenEmpty}` → error containing `spec.update is not allowed for type OnDemand` (unchanged rule).
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./api/v1alpha1 -run TestPersistentUpdateTakesOnlyStrategy -count=1`; the "WhenEmpty" case fails with `spec.update is not allowed for type Persistent`.
- [ ] **Step 3: Replace the rule** with the three above, update the two doc comments, `nd make manifests generate`.
- [ ] **Step 4: Run, expect PASS**: same command, then `nd go test ./api/v1alpha1 -count=1`.
- [ ] **Step 5: Commit**: `feat(api): spec.update.strategy on persistent groups`, with generated files.

### Task 2: `ObjectStore` on persistent groups, backend frozen there

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (CEL rule `storage.backend ObjectStore needs …`; new immutability rule; doc comment of `StorageSpec.Backend`)
- Test: `api/v1alpha1/servergroup_envtest_test.go` (`TestObjectStoreBackendNeedsOnDemandAndKeep` is rewritten, `TestTheBackendMayChangeBothWays` keeps passing unchanged)
- Generated: as Task 1

**Interfaces:**
- Produces, on `ServerGroupSpec`:
  - `!has(self.storage) || !has(self.storage.backend) || self.storage.backend != 'ObjectStore' || ((self.type == 'OnDemand' || self.type == 'Persistent') && has(self.storage.keep))` → `storage.backend ObjectStore needs type OnDemand or Persistent, and storage.keep`
  - `self.type != 'Persistent' || ((has(self.storage) && has(self.storage.backend) ? self.storage.backend : 'Claim') == (has(oldSelf.storage) && has(oldSelf.storage.backend) ? oldSelf.storage.backend : 'Claim'))` → `storage.backend is immutable for type Persistent: a new backend would start every ordinal on a world it does not hold; see the persistent-worlds guide for moving a world`. The rule needs `oldSelf`, so it is a transition rule; a create is unaffected.
- `StorageSpec.Backend` comment: `ObjectStore` needs type `OnDemand` or `Persistent` and `keep`; it is immutable on a `Persistent` group and may change either way on an `OnDemand` one.

- [ ] **Step 1: Write the failing tests**
  - Rewrite `TestObjectStoreBackendNeedsOnDemandAndKeep` → `TestObjectStoreBackendNeedsAWorldKeepingTypeAndKeep`: `persistentGroup` with `ObjectStore` and `Keep: {"world"}` → created; `persistentGroup` with `ObjectStore` and no keep → error with the new message; `onDemandGroup` without keep → same error; with keep → created; `ephemeralGroup` cannot carry storage at all (existing rule, leave untested here).
  - `TestPersistentBackendIsImmutable`, table, each case creates a fresh `persistentGroup` (with `Keep: {"world"}` where needed) and updates `Spec.Storage.Backend`:
    - "Claim to ObjectStore" → error containing `storage.backend is immutable for type Persistent`
    - "ObjectStore to Claim" → same error
    - "absent to ObjectStore" → same error
    - "absent to Claim" → accepted
    - "ObjectStore, other field changed" (`Spec.MaxPlayers++`) → accepted
  - `TestTheBackendMayChangeBothWays` (on-demand) stays as it is and must keep passing.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./api/v1alpha1 -run 'TestObjectStoreBackend|TestPersistentBackendIsImmutable|TestTheBackendMayChange' -count=1`.
- [ ] **Step 3: Change the rule, add the transition rule, update the comment**, `nd make manifests generate`.
- [ ] **Step 4: Run, expect PASS**: same command, then `nd go test ./api/v1alpha1 -count=1`.
- [ ] **Step 5: Commit**: `feat(api): ObjectStore backend on persistent groups`.

### Task 3: `DecidePersistentSize` nominates only empty ordinals under `WhenEmpty`

**Files:**
- Modify: `internal/controller/candidates.go` (helper)
- Modify: `internal/controller/persistent.go` (`PersistentInputs.WhenEmpty`, the stale and resizing loops)
- Modify: `internal/controller/scaling.go` (`selectRetirement` uses the helper instead of its inline test; no behaviour change)
- Test: `internal/controller/persistent_test.go`

**Interfaces:**
- Produces:
  - `func (v ServerView) knownEmpty() bool` → `v.Players == 0 && !v.Stale`
  - `PersistentInputs.WhenEmpty bool`: under it, the stale and resize-pending classes skip views where `!knownEmpty()`. Surplus, the order (highest first), Gate A (`takedownInFlight`), Gate B (`groupRecovered`) and `ChangeoverRefused` stay as they are.
- Consumes: nothing new.

- [ ] **Step 1: Write the failing tests** in `persistent_test.go`, using `ordinalView` and the `ready` helper style of `TestDecidePersistentSizeTakesOneOrdinalDownAtATime`; group `g`, `podHash: "h2"`, stale views carry `"h1"`:
  - `TestWhenEmptyPersistentSkipsAnOccupiedOrdinal`: replicas 2, `g-0` stale empty, `g-1` stale with `Players: 3` → `Delete == ["g-0"]`, `DeleteReason == "StaleSpec"`.
  - `TestWhenEmptyPersistentTakesTheHighestEmpty`: replicas 3, all three stale and empty → `["g-2"]`.
  - `TestWhenEmptyPersistentSkipsAnUntrustedCount`: replicas 1, `g-0` stale, `Players: 0, Stale: true` → no delete.
  - `TestWhenEmptyPersistentWaitsWhenAllAreOccupied`: replicas 2, both stale and occupied → no delete, no create.
  - `TestWhenEmptyPersistentStillRemovesOccupiedSurplus`: replicas 1, `g-0` current, `g-1` current with `Players: 5` → `["g-1"]`, `"SurplusOrdinal"`.
  - `TestWhenEmptyPersistentSkipsAnOccupiedResize`: replicas 1, `g-0` current, `ResizePending: true`, `Players: 1` → no delete; with `Players: 0` → `["g-0"]`, `"ResizePending"`.
  - `TestWhenEmptyPersistentKeepsBothGates`: replicas 2, `g-1` stale empty, `g-0` `phase.Starting` → no delete (Gate B); `g-0` `phase.Draining` current, `g-1` stale empty → no delete (Gate A).
  - The same inputs with `WhenEmpty: false` in the first and fourth tests nominate `g-1` (today's behaviour), as a second assertion in each.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./internal/controller -run 'TestWhenEmptyPersistent' -count=1` fails to compile on `WhenEmpty`.
- [ ] **Step 3: Implement** the field and the helper; filter inside the two existing loops.
- [ ] **Step 4: Run, expect PASS**: same command, then `nd go test ./internal/controller -run 'TestDecidePersistentSize|TestWhenEmpty' -count=1` (the ephemeral `WhenEmpty` tables must still pass after the helper refactor).
- [ ] **Step 5: Prove the tests bite** in a throwaway worktree (`git worktree add --detach`, removed afterwards): (a) drop the `knownEmpty` filter → `TestWhenEmptyPersistentSkipsAnOccupiedOrdinal` fails; (b) make `knownEmpty` read only `Players == 0` → `TestWhenEmptyPersistentSkipsAnUntrustedCount` fails; (c) apply the filter to surplus → `TestWhenEmptyPersistentStillRemovesOccupiedSurplus` fails. Paste each failure into the task report.
- [ ] **Step 6: Commit**: `feat(controller): persistent WhenEmpty takes down only empty ordinals`.

### Task 4: The changeover state of a persistent `WhenEmpty` group

**Files:**
- Modify: `internal/controller/changeover.go` (`ownPersistentChangeover`)
- Modify: `internal/controller/servergroup_controller.go` (persistent branch of the sizing switch: pass `group.UpdateWhenEmpty()` to `ownPersistentChangeover` and as `PersistentInputs.WhenEmpty`)
- Test: `internal/controller/changeover_test.go`, `internal/controller/changeover_envtest_test.go`

**Interfaces:**
- Consumes: `ServerView.knownEmpty()` (Task 3), `PersistentInputs.WhenEmpty` (Task 3).
- Produces: `func ownPersistentChangeover(views []ServerView, podHash string, pendingDeletes map[string]bool, replicas int32, whenEmpty bool, was spawneryv1alpha1.ChangeoverState) spawneryv1alpha1.ChangeoverState`. With `whenEmpty == false` it returns exactly what it returns today. With `whenEmpty == true` it ignores `was` and follows spec §3.4's table, over views that are not `Hold`, are `staleSpec`, and are not `phase.Terminal`:
  1. none → `None`
  2. one with `Ordinal < replicas` that is `leaving()` or in `pendingDeletes` → `Begun`
  3. one with `Ordinal < replicas` and `knownEmpty()`, or one with `Ordinal >= replicas` → `Waiting`
  4. else → `Deferred`

- [ ] **Step 1: Write the failing tests**
  - In `TestOwnPersistentChangeover`, pass `false` for the new parameter in every existing row (they must keep passing), and add `TestOwnPersistentChangeoverWhenEmpty` with rows: nothing stale → `None`; stale occupied (`Players: 2`) → `Deferred`; stale occupied, `was: Begun` → `Deferred`; stale empty, `was: Deferred` → `Waiting`; stale empty draining → `Begun`; stale empty in `pendingDeletes` → `Begun`; stale untrusted (`Stale: true`) → `Deferred`; stale occupied plus a stale surplus ordinal (`Ordinal: 2`, replicas 2) → `Waiting`; held stale → `None`.
  - In `changeover_envtest_test.go`, `TestAPersistentWhenEmptyGroupDefersAndStillWaitsForItsStage`, modelled on `TestAPersistentGroupWaitsForItsStage`: lobby (stage 0) and persistent `world` (stage 10, `Update: {Strategy: WhenEmpty}`), both Ready; `f.reportPlayersOn(t, "world-0", 2)`; image change on both; reconcile both. Assert `world` status `Deferred`, `world-0` not deleted or draining, and a third group `late` (ephemeral, stage 20, image changed) is not refused by `world` (its Progressing reason is not `WaitingForEarlierStage` naming `world`). Then `f.reportPlayersOn(t, "world-0", 0)` while lobby is still in flight: `world` is `Waiting`, `world-0` untouched, Progressing `WaitingForEarlierStage`. Then `f.finishChangeover(t, r, "lobby")`, reconcile: `world-0` is being taken down, status `Begun`.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./internal/controller -run 'TestOwnPersistentChangeover|TestAPersistentWhenEmptyGroupDefers' -count=1`.
- [ ] **Step 3: Implement** the parameter and the table; wire both values in the persistent branch of the sizing switch.
- [ ] **Step 4: Run, expect PASS**: same command, then `nd go test ./internal/controller -run 'Changeover|Persistent' -count=1`.
- [ ] **Step 5: Prove the gate bites** in a throwaway worktree: make the persistent branch pass `ChangeoverRefused: false` when the previous state was `Deferred` → the envtest fails at the "lower stage still in flight" assertion. Paste the failure.
- [ ] **Step 6: Commit**: `feat(controller): a persistent WhenEmpty group defers while its ordinals are occupied`.

### Task 5: `WaitingForEmpty` on `Progressing`

**Files:**
- Modify: `api/v1alpha1/common_types.go` (`ReasonWaitingForEmpty = "WaitingForEmpty"`, with a one-line comment like its neighbours)
- Modify: `internal/controller/servergroup_controller.go` (`reportProgressing`)
- Test: `internal/controller/servergroup_controller_test.go`, beside the existing `reportProgressing` tables

**Interfaces:**
- Produces: in `reportProgressing`, a case after `starting > 0` and before `older > 0`, taken when the group is persistent, `UpdateWhenEmpty()`, `older > 0`, and every counted stale view has `Ordinal < replicas`, is not `leaving()`, and is not `knownEmpty()`. Status `True`, reason `ReasonWaitingForEmpty`, message exactly `stale servers wait for their players to leave: ` followed by the stale servers sorted by name, joined with `, `, each as `<name> has <n> player(s)` (`1 player`, `3 players`) or `<name> has an unknown number of players` when the count is untrusted.

- [ ] **Step 1: Write the failing test** `TestReportProgressingWaitingForEmpty`, table, persistent group with replicas 2 and `Update{Strategy: WhenEmpty}`, `podHash "current"`:
  - two stale occupied (`w-0` 3 players, `w-1` 1 player) → `WaitingForEmpty`, message `stale servers wait for their players to leave: w-0 has 3 players, w-1 has 1 player`.
  - untrusted count (`w-0` `Stale: true`, `WasRegistered: true`) → message ends `w-0 has an unknown number of players`.
  - one stale empty beside an occupied one → `ReplacingServers` (it can move, so it is not waiting).
  - one stale occupied while a current one is `Starting` → `ServersStarting`.
  - same views with `RollingUpdate` → `ReplacingServers`.
  - `ChangeoverWait{Reason: ReasonWaitingForEarlierStage, …}` given → that reason wins.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./internal/controller -run TestReportProgressingWaitingForEmpty -count=1`.
- [ ] **Step 3: Implement** the constant and the case, `nd make manifests` (no CRD change expected; it keeps `docs/reference` in step if the constant is listed there).
- [ ] **Step 4: Run, expect PASS**: same command, then `nd go test ./internal/controller -run 'TestReportProgressing' -count=1`.
- [ ] **Step 5: Controller envtest** `TestAPersistentWhenEmptyGroupKeepsAnOccupiedOrdinal` in `rolloutfloor_envtest_test.go`: `f.createPersistentGroup(t, "world", 1)` with `Update{Strategy: WhenEmpty}`, Ready, `f.reportPlayersOn(t, "world-0", 2)`, `f.setImage(t, "world", nextImage)`, reconcile twice: `world-0` still present and not draining, Progressing `WaitingForEmpty`, status `Deferred`. Then report 0 players, reconcile: `world-0` deleted or draining. Finish the drain the way `TestAPersistentGroupWaitsForItsStage` does, reconcile: `world-0` exists again with `Spec.PodHash` equal to the group's new hash. Run `nd go test ./internal/controller -run TestAPersistentWhenEmptyGroupKeepsAnOccupiedOrdinal -count=1`, expect PASS.
- [ ] **Step 6: Commit**: `feat(controller): WaitingForEmpty while every stale ordinal is occupied`.

### Task 6: A persistent world in the object store

**Files:**
- Modify: `internal/podspec/worldsync.go` (`WorldKey`, used by `worldSyncVolume`)
- Modify: `internal/controller/server_controller.go` (`ServerKeyMissing` check reads `podspec.WorldKey(srv) == ""`; the condition message says the server has no world name)
- Test: `internal/podspec/worldsync_test.go`, `internal/podspec/hash_test.go` (or wherever `DesiredServerHash`'s discrimination table lives), `internal/controller/server_controller_test.go`

**Interfaces:**
- Produces: `func WorldKey(srv *spawneryv1alpha1.Server) string` in `internal/podspec/worldsync.go`: `srv.Spec.Key` when set, else `strconv.Itoa(int(*srv.Spec.Ordinal))` when `Ordinal != nil`, else `""`. `worldSyncVolume` uses it for the third segment of `world`. `DesiredServerHash`'s dummy `Server` has neither, so the hash keeps an empty key and is the same for every ordinal.
- `ReasonServerKeyMissing` keeps its name (a rename would move a public reason); its message becomes: `a member of a group that keeps its worlds in an object store needs a world name (spec.key for an on-demand member, spec.ordinal for a persistent server); no pod is created without it`.

- [ ] **Step 1: Write the failing tests**
  - `TestAPersistentObjectStoreServerNamesItsWorldByOrdinal` (`worldsync_test.go`): `testGroup()` as `Persistent` with `Storage{Size: 6Gi, Backend: ObjectStore, Keep: {"world"}}`, `testServer()` with `Ordinal: ptr.To[int32](3)` and no key → data volume is CSI, `world == srv.Namespace+"/"+group.Name+"/3"`, no `PersistentVolumeClaim` volume in the pod, `SPAWNERY_WORLD_SYNC == "1"`.
  - `TestWorldKey`: key `"c0ffee"` → `"c0ffee"`; ordinal 0 → `"0"`; ordinal 12 → `"12"`; neither → `""`; both (not produced by the operator) → the key.
  - `TestAPersistentObjectStoreHashIgnoresTheOrdinal`: `DesiredServerHash` for the persistent `ObjectStore` group is equal across two calls, and differs from the same group on `Claim` (the backend is part of the pod).
  - `TestPodHashIgnoresSpecUpdate` (`hash_test.go`, beside `TestPodHashIgnoresReplicas`): a persistent `Claim` group's `DesiredServerHash` is the same with `Spec.Update` nil and with `{Strategy: WhenEmpty}`.
  - `TestAPersistentObjectStoreServerGetsNoClaim` (`server_controller_test.go`, modelled on `TestAPersistentServerGetsItsClaimBeforeItsPod` and `ondemand_envtest_test.go`'s `TestAnObjectStoreMemberGetsNoClaim`): reconciler with `WorldSync: true`; the pod is created, no claim `<server>-data` exists, the pod's data volume names world `<ns>/<group>/0`.
  - `TestAPersistentObjectStoreServerWaitsWhenWorldSyncIsOff`: `WorldSync: false` → no pod, `Accepted` False with `ReasonWorldSyncOff`.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./internal/podspec ./internal/controller -run 'PersistentObjectStore|TestWorldKey' -count=1`. The volume test fails on world `…/` with an empty key, the controller test on `ServerKeyMissing`.
- [ ] **Step 3: Implement** `WorldKey`, use it in `worldSyncVolume` and in the acceptance check.
- [ ] **Step 4: Run, expect PASS**: same command, then `nd go test ./internal/podspec -count=1` (hash goldens unchanged) and `nd go test ./internal/controller -run 'ObjectStore|Persistent' -count=1`.
- [ ] **Step 5: Commit**: `feat(podspec): persistent worlds in the object store, named by ordinal`.

### Task 7: Guides

**Files:**
- Modify: `docs/guides/updates-and-drain.md`
- Modify: `docs/guides/persistent-worlds.md`
- Modify: `docs/guides/object-store-worlds.md`

**Interfaces:** none. Content comes from spec §3 and §4; run the text through the humanizer before committing, as every doc text here.

- [ ] **Step 1: `updates-and-drain.md`**: a section "WhenEmpty for persistent groups": the YAML of spec §3.1; which takedowns wait (stale, resize) and which do not (surplus, node drain); the `Deferred` state and that lower stages still go first; `WaitingForEmpty` with a sample message; forcing an update with `kubectl delete server <group>-<ordinal> -n <ns>`; the one-pass join race.
- [ ] **Step 2: `persistent-worlds.md`**: a section "Worlds in the object store": the YAML of spec §4.1; immutability and the reason; the four points of spec §4.4; the dead-node section gets an `ObjectStore` paragraph (the force-delete is still needed for the pod name, the lease refuses a second writer, and the replacement waits up to 10 minutes for the lease); "Moving a world onto the object store" with the four steps of §4.6, including the import Job from `object-store-worlds.md` with `--world <namespace>/<group>/<ordinal>`; no way back.
- [ ] **Step 3: `object-store-worlds.md`**: drop the on-demand-only statements; name the world layout for both types; link to the persistent section; restore stays on-demand only.
- [ ] **Step 4: Verify**: `nd make test` passes the docs linters (it runs them before `go test`).
- [ ] **Step 5: Commit**: `docs(guides): WhenEmpty and the object store for persistent groups`.

### Task 8: End-to-end evidence

**Files:**
- Modify: `test/e2e/worldsync_test.go`, `hack/e2e-worldsync.sh` (the `-run` pattern)
- Modify: `test/e2e/tutorial_test.go`, `hack/e2e-tutorial.sh` (`E2E_RUN` default)

**Interfaces:** reuses `listStore`, `getPod`, `assertWorldGroupWritable` (worldsync) and `startHeldJoin`, `applyManifest`, `tutorialNamespace`, `tutorialProxyGroup` (tutorial).

- [ ] **Step 1: `TestAPersistentObjectStoreWorldTravelsBetweenNodes`** in `worldsync_test.go`, modelled on `TestAnObjectStoreWorldTravelsBetweenNodes`: a persistent group with `replicas: 1`, `ObjectStore`, the same keep list; wait for `<group>-0` Ready; write the marker the existing test writes; delete the pod's `Server` object after cordoning its node; wait for `<group>-0` Ready on another node; read the marker; assert `listStore("<prefix>/<ns>/<group>/0/")` lists `manifest.json`; uncordon and delete the group in cleanup. Add it to the `-run` pattern in `hack/e2e-worldsync.sh` (`TestAnObjectStoreWorld|TestAPersistentObjectStoreWorld`).
- [ ] **Step 2: `TestTutorialPersistentWhenEmpty`** in `tutorial_test.go`: create a persistent group `keep` in `tutorialNamespace` (`replicas: 1`, `Update{Strategy: WhenEmpty}`, the lobby's image, CPU request `100m`, claim storage `1Gi`); patch the gateway's `routing.fallbackGroups` to `["keep"]`, restored in `t.Cleanup`; `startHeldJoin` until the player is on `keep-0`; change the group's `spec.env` (adds `E2E_ROLL=1`) so the hash moves; for 60 s assert every few seconds that `keep-0` keeps its pod UID and the group reports `WaitingForEmpty`; end the held join; wait until `keep-0` carries the group's new `spec.podHash` and is Ready. Add it to `E2E_RUN` in `hack/e2e-tutorial.sh`. If the 4-CPU runner cannot hold `keep` beside the lobby and gateway even at `100m`, drop this step, say so in the task report, and leave Task 5's envtest as the evidence (spec §6).
- [ ] **Step 3: Run** on `paul-desktop` (both need three kind nodes or real images; not on `dev`): `systemd-run --scope --user --property=Delegate=yes -- nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e-worldsync` and the same with `make e2e-tutorial` (or `E2E_RUN=TestTutorialPersistentWhenEmpty hack/e2e-tutorial.sh` through the same wrapper). Expected: both new tests PASS. If the session runs where neither is possible, the task report says so and names the commands; the e2e is not claimed.
- [ ] **Step 4: Commit**: `test(e2e): persistent worlds in the object store and WhenEmpty`.

### Task 9: Whole-branch verification

- [ ] **Step 1**: `nd make test` (on `dev`, run the envtest packages afterwards with `-p 1` if the parallel run is killed). Expected: PASS, generated files in step, hash goldens unchanged.
- [ ] **Step 2**: `nd make lint`. Expected: no findings.
- [ ] **Step 3**: `git status` is clean, and `git diff origin/master --stat` touches no file under `image/`, `agent/` or `charts/spawnery/values.yaml` (no image change, no chart change).
