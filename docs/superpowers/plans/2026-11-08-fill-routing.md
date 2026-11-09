# Fill Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `ServerGroup` can opt into `spec.routing.strategy: Fill`, which sends arriving players to the fullest server below its playable slots, so that a spare server empties and the existing demand rule removes it.

**Architecture:** The strategy travels from the CRD to the proxies on `GroupState`, and a per-server stale flag on `ServerState` comes from a desired-hash memo in `agent.Registry` that the ServerGroup reconcile writes. One ordering rule, written once in Go (`internal/agentserver`) and once in Kotlin (`agent/velocity`) and pinned by the same table of cases on both sides, decides the server; each side adds its own tally of players it has just sent.

**Tech Stack:** Go (controller-runtime, envtest), protobuf (`make proto`), Kotlin (Velocity agent, JUnit), kind e2e.

**Spec:** `docs/superpowers/specs/2026-11-08-fill-routing-design.md`

## Global Constraints

- Field: `spec.routing.strategy`, enum `Spread | Fill`, default `Spread` when `routing` or `strategy` is absent.
- `Spread` behaviour is unchanged on every path, byte for byte: `Router` by `playersConnected` then name; `resolveTarget` by most free playable slots then most room.
- Fill line = the server's resolved playable slots (`ServerState.playable_slots`; 0 or above `slots` reads as `slots`).
- Network-wide counts (mirror / cache), never per-proxy counts alone.
- A burst from one chooser never places more than `maxPlayers` on a server while another eligible server has room.
- Wire: new fields only, absent reads as `Spread` / not stale. Older agents spread.
- Changing `routing` restarts no server: it must not feed `podspec.DesiredServerHash` (the hash golden in `internal/podspec/hash_golden_test.go` must not move).
- Commits: Conventional Commits with a scope (`feat(api): …`), body wrapped at 72. No version bumps in this plan; the release is a separate step on Paul's instruction.
- Run every build and test command through `nix develop -c`. On the `dev` machine run envtest packages with `-p 1`.
- The repo has a `.codegraph/` index: use `codegraph explore "<symbols>"` (or the `codegraph_explore` MCP tool) before grep/Read to find code.
- Code comments follow the repo's rule: none unless a reader could not derive it from the code.

## The ordering rule (shared by Task 4 and Task 6)

Input: the eligible candidates of one group (eligibility exactly as today), and for each candidate `players` (base count plus the chooser's tally), `slots`, `line` (resolved playable slots), `stale`, `number`, `name`.

`slots == 0` means the server has not reported: treat it as having room, with line = slots = unlimited.

Sort key, first difference wins:

1. has room (`players < slots`) before full;
2. current (`!stale`) before stale;
3. below line (`players < line`) before at or above;
4. if below line: most `players` first; otherwise: most room (`slots - players`) first;
5. `number` ascending, with `number == 0` after every numbered server;
6. `name` ascending.

The first candidate wins. Step 1 is a refinement of spec 3.5 (my call, Paul has not been asked): a full current server never beats a stale one with room, or a join would be refused.

Case table both Task 4 and Task 6 test, as `(name, players, slots, line, stale, number)` → winner:

| # | candidates | winner |
|---|---|---|
| A | a(5,25,20,f,1) b(0,25,20,f,2) | a |
| B | a(20,25,20,f,1) b(3,25,20,f,2) | b |
| C | a(20,25,20,f,1) b(22,25,20,f,2) | a (more room) |
| D | a(4,25,20,f,1) b(4,25,20,f,2) | a (number) |
| E | a(4,25,20,f,0) b(4,25,20,f,2) | b (0 last) |
| F | a(10,25,20,t,1) b(2,25,20,f,2) | b (current first) |
| G | a(10,25,20,f,1) b(25,25,20,f,2) c(3,25,20,t,3) | a |
| H | a(25,25,20,f,1) c(3,25,20,t,3) | c (room before current) |
| I | a(0,0,0,f,1) b(3,25,20,f,2) | b (a below unlimited line with 0 players) |

## Review Focus

- A group switched between `Spread` and `Fill` at runtime: the next state must change routing without any server restarting (Task 1 asserts the hash does not move; Task 3 asserts the strategy reaches `GroupState`).
- A server whose agent has not reported (`slots == 0`) under `Fill`: must not be chosen ahead of a reporting server below its line, and must not be excluded (case I; Task 4 and Task 6 table).
- A drain under `Fill` of a server with more players than one target's room: no target past `maxPlayers` while another has room (Task 6 burst test drives `Drain` through `Router`).
- A plugin connecting a whole party to a `Fill` group in one go (Task 4 operator burst test).
- An old proxy agent against a new operator: `Fill` groups spread there, and nothing errors (Task 2 wire golden plus Task 5 test that an absent field reads as `Spread`).

---

### Task 1: `spec.routing` on ServerGroup

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (new `RoutingSpec`, `RoutingStrategy`, field `Routing` on `ServerGroupSpec` next to `Update`; CEL rule on `ServerGroupSpec`)
- Generated: `zz_generated.deepcopy.go`, `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `docs/reference/crds.md`
- Test: `api/v1alpha1/servergroup_envtest_test.go`, `internal/podspec/hash_golden_test.go` (unchanged, must stay green)

**Interfaces:**
- Produces: `type RoutingStrategy string`; constants `RoutingSpread RoutingStrategy = "Spread"`, `RoutingFill RoutingStrategy = "Fill"`; `type RoutingSpec struct { Strategy RoutingStrategy \`json:"strategy,omitempty"\` }` with `+kubebuilder:validation:Enum=Spread;Fill` and `+kubebuilder:default=Spread`; `ServerGroupSpec.Routing *RoutingSpec \`json:"routing,omitempty"\``; method `func (g *ServerGroup) RoutingStrategy() RoutingStrategy` returning `RoutingSpread` for nil or empty.

- [ ] **Step 1: Write the failing envtest** `TestRoutingIsRefusedOnAnOnDemandGroup` and `TestRoutingFillIsAcceptedOnAnEphemeralGroup` in `servergroup_envtest_test.go`, in the style of the existing CEL tests there: an OnDemand group with `routing: {strategy: Fill}` is rejected with a message containing `spec.routing`; an Ephemeral one is accepted and reads back `Fill`; a value `Pack` is rejected by the enum.
- [ ] **Step 2: Run** `nix develop -c go test ./api/v1alpha1/ -run 'TestRouting' -count=1`. Expected: FAIL (field unknown).
- [ ] **Step 3: Implement the types**, the CEL rule `self.type != 'OnDemand' || !has(self.routing)` with message `spec.routing is not allowed for type OnDemand: its members are addressed by name, not routed into`, and the `RoutingStrategy()` method. Field doc says what `Fill` does in two sentences and that proxy images need the release that ships it.
- [ ] **Step 4: Regenerate** `nix develop -c make manifests generate`.
- [ ] **Step 5: Run** the Step 2 command and `nix develop -c go test ./internal/podspec/ -run Golden -count=1`. Expected: both PASS, golden unchanged (`git diff internal/podspec/testdata` empty).
- [ ] **Step 6: Commit** `feat(api): spec.routing.strategy on ServerGroup` with all generated files.

### Task 2: Wire fields

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Generated: `internal/agentpb/`, `agent/common/src/proto/java/`, `internal/agentpb/testdata/agent.wire.golden`

**Interfaces:**
- Produces: in `GroupState`, `enum RoutingStrategy { ROUTING_SPREAD = 0; ROUTING_FILL = 1; }` and `RoutingStrategy routing_strategy = 16;`. In `ServerState`, `bool stale = 17;`. Comments: absent strategy reads as spread; `stale` is true when the server's pod hash differs from its group's current one, false when either is unknown.

- [ ] **Step 1: Edit the proto**, then `nix develop -c make proto`.
- [ ] **Step 2: Run** `nix develop -c go test ./internal/agentpb/ -count=1`. Expected: FAIL in `TestTheWireFormatMatchesTheGolden`.
- [ ] **Step 3: Update the golden** with `nix develop -c go test ./internal/agentpb -run TestTheWireFormatMatchesTheGolden -update` and rerun Step 2. Expected: PASS.
- [ ] **Step 4: Commit** `feat(agentpb): routing strategy and stale flag on the agent channel`.

### Task 3: Desired-hash memo and netstate

**Files:**
- Modify: `internal/agent/registry.go` (memo), `internal/controller/servergroup_controller.go:281` (write the memo after `DesiredServerHash` succeeds), `internal/netstate/netstate.go` (`Build`, `serverState`)
- Test: `internal/agent/registry_test.go`, `internal/netstate/netstate_test.go`

**Interfaces:**
- Consumes: Task 1 `(*ServerGroup).RoutingStrategy()`, Task 2 fields.
- Produces: `func (r *Registry) SetDesiredServerHash(namespace, group, hash string)`, `func (r *Registry) DesiredServerHash(namespace, group string) string` (empty when unknown). `GroupState.RoutingStrategy` set from the spec for every ServerGroup; `ServerState.Stale = want != "" && srv.Spec.PodHash != "" && srv.Spec.PodHash != want` with `want = s.Agents.DesiredServerHash(namespace, srv.Spec.GroupRef.Name)`.

- [ ] **Step 1: Write failing tests:** `TestTheDesiredHashIsRememberedPerGroup` (set, read back; other namespace/group reads empty); in netstate, `TestARoutingStrategyReachesTheGroupState` (Fill group → `ROUTING_FILL`, group without routing → `ROUTING_SPREAD`) and `TestAServerIsStaleOnlyAgainstAKnownHash` (memo `h2`, server `h1` → true; server `h2` → false; server `""` → false; no memo → false).
- [ ] **Step 2: Run** `nix develop -c go test ./internal/agent/ ./internal/netstate/ -count=1 -p 1`. Expected: FAIL.
- [ ] **Step 3: Implement** the memo (a map under the existing mutex, keyed `namespace/group`), the write in the reconcile right after `podHash` is computed, and the two netstate fields. `Visible` keeps `Stale` (it is not a count).
- [ ] **Step 4: Run** Step 2, then `nix develop -c go test ./internal/controller/ -count=1 -p 1`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(netstate): tell the proxies each group's routing and which servers are stale`.

### Task 4: Fill in the operator's connect

**Files:**
- Create: `internal/agentserver/fill.go` (ordering rule and tally), `internal/agentserver/fill_test.go`
- Modify: `internal/agentserver/requests.go` (`answerConnect`, `resolveTarget`), `internal/agentserver/server.go` (tally on `Server`)
- Test: `internal/agentserver/requests_test.go`

**Interfaces:**
- Consumes: Task 2/3 fields on `NetworkState`.
- Produces:
  - `type fillCandidate struct { Name string; Players, Slots, Line, Number int32; Stale bool }`
  - `func chooseFill(cands []fillCandidate) (string, bool)` implementing the ordering rule above.
  - `type pendingMoves struct` with `func newPendingMoves(clock func() time.Time, ttl time.Duration) *pendingMoves`, `func (p *pendingMoves) add(namespace, server string)`, `func (p *pendingMoves) count(namespace, server string) int32`. TTL is `3 * opts.ReportInterval` (15 s with defaults): `Status.Players` lags the report by a reconcile, so dropping on the registry's report (the spec's wording) could undercount; my call.
  - `resolveTarget(state *agentpb.NetworkState, req *agentpb.ConnectRequest, pending func(server string) int32) (string, bool)`: for a group whose `GroupState.RoutingStrategy` is `ROUTING_FILL`, build candidates from registered members with `Players + pending(name)` and call `chooseFill`; otherwise the existing loop unchanged. `answerConnect` records `add` after a Fill target is chosen and before `Move`.

- [ ] **Step 1: Write failing tests** in `fill_test.go`: `TestChooseFillFollowsTheSharedTable` with the nine cases A to I as a table; `TestPendingMovesExpireAfterTheTTL` (add at t0, count 1 at t0+14s, 0 at t0+15s with a fake clock).
- [ ] **Step 2: Write the failing burst test** `TestAPartyConnectedToAFillGroupNeverPassesMaxPlayers` in `requests_test.go` on the existing `connectFixture` pattern: a Fill group with two registered servers, `slots` 10, `playable` 8, players 7 and 0; ten connects of distinct players in a row with an unchanging picture; count targets per server; assert neither exceeds 10 and the first one went to the server with 7. Plus `TestASpreadGroupStillPicksTheMostFreeSlots`, the old behaviour, unchanged.
- [ ] **Step 3: Run** `nix develop -c go test ./internal/agentserver/ -run 'Fill|Party|Spread|Pending' -count=1`. Expected: FAIL.
- [ ] **Step 4: Implement** `fill.go` and the `resolveTarget` / `answerConnect` changes.
- [ ] **Step 5: Run** Step 3, then `nix develop -c go test ./internal/agentserver/ -count=1 -p 1`. Expected: PASS.
- [ ] **Step 6: Commit** `feat(agentserver): connect to a Fill group picks the fullest server`.

### Task 5: The mirror carries routing and staleness

**Files:**
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/NetworkMirror.kt`
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/NetworkMirrorTest.kt`

**Interfaces:**
- Consumes: Task 2 Java fields.
- Produces (internal, not on the public `SpawneryApi`): `enum class Routing { SPREAD, FILL }`; `fun routing(group: String): Routing` (unknown group or absent field → `SPREAD`); `fun fillView(name: String): FillView?` with `data class FillView(val players: Int, val slots: Int, val line: Int, val stale: Boolean, val number: Int)` where `line` resolves 0 or `> slots` to `slots`; `fun appliedAtNanos(): Long` (monotonic time of the last `apply`, 0 before the first), taken from a constructor clock `NetworkMirror(clock: () -> Long = System::nanoTime)`.

- [ ] **Step 1: Write failing tests:** `a Fill group reads as FILL and a group without the field as SPREAD`; `fillView resolves an unset playable figure to slots`; `fillView carries the stale flag`; `appliedAt moves with every state`.
- [ ] **Step 2: Run** `nix develop -c make agent`. Expected: FAIL in `NetworkMirrorTest`. (`git add` new files first: Nix reads the index.)
- [ ] **Step 3: Implement** inside the existing `Snapshot` swap so a read never mixes two states.
- [ ] **Step 4: Run** `nix develop -c make agent`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): the network mirror carries routing and staleness`.

### Task 6: Fill in the proxy's Router

**Files:**
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/Fill.kt` (ordering rule, `SentTally`)
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/Router.kt`, `AgentPlugin.kt` (construct `Router(directory, FillSource(mirror, tally))`)
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/FillTest.kt`, `RouterTest.kt`, `DrainTest.kt`

**Interfaces:**
- Consumes: Task 5 `NetworkMirror.routing`, `fillView`, `appliedAtNanos`.
- Produces:
  - `internal data class FillCandidate(val name: String, val players: Int, val slots: Int, val line: Int, val stale: Boolean, val number: Int)` and `internal fun chooseFill(candidates: List<FillCandidate>): String?` implementing the ordering rule.
  - `class SentTally(private val clock: () -> Long, private val stateAt: () -> Long, private val margin: Duration = Duration.ofSeconds(10), private val maxAge: Duration = Duration.ofMinutes(2))` with `fun sent(server: String)` and `fun pending(server: String): Int`. An entry counts while `sentAt > stateAt() - margin` and `sentAt > now - maxAge`. Margin is two default report intervals (my call).
  - `class FillSource(val mirror: NetworkMirror, val tally: SentTally)`.
  - `Router(directory: ServerDirectory, fill: FillSource? = null)`; `choose` keeps its signature. For a group with `routing == FILL` and `fill != null`, candidates get `players = max(view.players, playersConnected.size) + tally.pending(name)`; a candidate with no `fillView` gets `slots = 0` (unknown). The winner is recorded with `tally.sent(name)`. Otherwise the existing `minWithOrNull` line, unchanged.

- [ ] **Step 1: Write failing tests** in `FillTest.kt`: `chooseFill follows the shared table` (cases A to I, same values as Task 4); `a sent player counts until a state arrives a margin later`; `a sent player stops counting after maxAge without any state`.
- [ ] **Step 2: Write failing Router tests** in `RouterTest.kt`: `a Fill group sends the next player to the fullest server below its line`; `two proxies with the same mirror pick the same server` (two `Router`s over separate registries with different `playersConnected`, same mirror); `a burst never puts more than maxPlayers on one server while another has room` (thirteen choices, exactly the free seats, with the mirror unchanged for the whole burst: `slots` 10, line 8, players 7 and 0; both servers end at 10); `a Spread group is chosen exactly as before` (the existing fewest-players test, with a mirror present).
- [ ] **Step 3: Write the failing drain test** in `DrainTest.kt`: `draining a full server into a Fill group never overfills one target`, twelve players drained into two targets with `slots` 10, line 8, players 6 and 0.
- [ ] **Step 4: Run** `nix develop -c make agent`. Expected: FAIL in the new tests.
- [ ] **Step 5: Implement** `Fill.kt`, the `Router` branch and the wiring in `AgentPlugin`.
- [ ] **Step 6: Run** `nix develop -c make agent`. Expected: PASS.
- [ ] **Step 7: Commit** `feat(velocity): Fill routing on join, fallback, drain and rescue`.

### Task 7: Docs

**Files:**
- Modify: `docs/guides/scaling-and-boosts.md` (new section `## Letting a spare server empty`, after `## Coming back down`), `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java` (Javadoc of `connect`)

- [ ] **Step 1: Write the guide section:** why a spare server never empties under `Spread` when visits are short (the demand rule wants it empty for `scaleDownStabilizationSeconds`, every arrival goes to the emptiest server); what `Fill` does; the `playableSlots` buffer and the `spareSlots` pairing with the spec's example (`maxPlayers` 40, `spareSlots` 20 → `playableSlots` 32, `spareSlots` 12, both scale up at 21 players on one server); that proxy images must carry the release that ships it; that existing players are not moved.
- [ ] **Step 2: Update the `connect` Javadoc:** for a group target, the group's `spec.routing.strategy` decides which server.
- [ ] **Step 3: Run** `nix develop -c make test` (docs linters included) on `paul-desktop`, or on `dev` `nix develop -c make manifests` plus the docs lint targets it lists. Expected: PASS, no generated diff.
- [ ] **Step 4: Commit** `docs(guides): fill routing and the playableSlots buffer`.

### Task 8: End-to-end

**Files:**
- Modify: `test/e2e/tutorial_test.go` (new `TestTutorialFillRouting`), `hack/e2e-tutorial.sh` (add it to the default `E2E_RUN` list).

**Interfaces:**
- Consumes: everything above; helpers already in `tutorial_test.go`: `startHeldJoin`, `heldJoin.stop`, `whereIs`, `lobbyServerPlayers`, `loginPacer`.

- [ ] **Step 1: Write the test.** Patch the lobby group to `maxPlayers` 4, `playableSlots` 3, `spareSlots` 2, `scaleDownStabilizationSeconds` 60, `routing.strategy: Fill`, and scale the tutorial's gateway ProxyGroup (`docs/tutorial/network.yaml`, `replicas: 1`) to 2. Hold-join two players: the group scales to two servers. Stop both held joins, then for three minutes join one short visit (hold 20 s) every 30 s, paced by `loginPacer`. Assert every visit landed on the same server, that the other server reached zero players, and that the group is back at one server before the three minutes are over. Then, as a second subtest with `Spread`, the same visits land on the spare server at least once and the group is still at two servers at the end. Restore the group spec and the proxy replicas in `t.Cleanup`.
- [ ] **Step 2: Run** `E2E_RUN=TestTutorialFillRouting nix develop -c make e2e-tutorial` (on `dev`: export `XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS`, and a disk-backed `TMPDIR`). Expected: PASS. Before that, temporarily set `Fill` to behave as `Spread` in a throwaway worktree (`git worktree add --detach`) and record the failure of the Fill subtest; remove the worktree.
- [ ] **Step 3: Commit** `test(e2e): a spare lobby server empties under Fill routing`.

### After the release (not a task here)

Spec section 5's last check runs on a real network once the release is out:
switch a lobby group to `Fill` and watch `spawnery_group_servers` return to
its minimum within roughly the stabilization window after the player count
drops below the scale-up line.
