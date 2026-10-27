# Canvas Game Image Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `ghcr.io/spawnery/canvas` game image that runs the one spawnery agent with correct tick reports, working world sync, login gate and `ExecuteCommand`, pinned, tested and released beside Purpur.

**Architecture:** `nix/canvas.nix` and `nix/canvas-image.nix` copy the Purpur recipe with Canvas's paperclip jar. The Paper agent moves to Paper's region schedulers and asks a `ServerPlatform` for ticks and region saves; a Canvas implementation in its own Gradle source set, compiled against the pinned Canvas bundle, is loaded by name only on Canvas. `spawnery-config` gains a `canvas` flavour whose `paper-global.yml` key tree comes from Canvas's own defaults.

**Tech Stack:** Kotlin (Paper agent, Gradle, JUnit 5), Go (renderer, stub operator, e2e), bash (hack scripts, entrypoint), Nix, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-10-26-canvas-image-design.md`

## Global Constraints

- Work happens in the worktree `~/git/spawnery-canvas` on branch `feat/canvas-image`; every command runs from its root. `ND` stands for `nix --extra-experimental-features 'nix-command flakes' develop . -c`.
- `hostname` before a heavy run: on `dev`, envtest packages with `-p 1`, and kind runs need `XDG_RUNTIME_DIR=/run/user/$(id -u)`, `DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus`, `TMPDIR` on disk (for example `~/.cache/spawnery-tmp`) and `systemd-run --scope --user --property=Delegate=yes`. On `paul-desktop` no throttling.
- `CONTAINER=podman` on both machines.
- Nix reads the git index: `git add` every new file before a `nix build` or `make agent`.
- Canvas pin to start from: Minecraft `26.3`, build `991`, URL `https://jenkins.canvasmc.io/job/Canvas/991/artifact/canvas-server/build/libs/canvas-build.991.jar`, hash `sha256-v+KIU6kIjYdBbUFdz5ML1fuZ7pAZ95I6EMfS7rJq7yc=`.
- Image: name `ghcr.io/spawnery/canvas`, tag `${canvasVersion}-${imageVersion}`, home `/opt/canvas`, env `SPAWNERY_PAPER_HOME=/opt/canvas` and `SPAWNERY_CONFIG_FLAVOR=canvas`, label `cloud.spawnery.canvas-build`.
- One agent jar for Purpur and Canvas. No Canvas type is referenced from the agent's main source set.
- Tick windows: TPS over one minute (`Frame._1_MINUTE`, as `getTPS()[0]`), MSPT over five seconds (`Frame._5_SECONDS`, as `getAverageTickTime()`).
- Canvas futures (`propagateSaveAllTicket`) are waited on only from the `AsyncScheduler`, never from the global or a region thread.
- No operator change, no CRD change, no hash-golden move. Version bumps (`imageVersion`, release number, minor step) go in the release PR, not in this plan.
- Comments: none unless a reader cannot derive the point from the code next to it (behaviour of Canvas that the code cannot show, a number that looks arbitrary). Fix stories go in commit messages.
- Commits: Conventional Commits with the repo's scopes (`feat(agent)`, `feat(images)`, `feat(render)`, `test(e2e)`, `ci`, `docs`), body wrapped at 72, gpg-signed, no tool attribution of any kind. Probe before the first commit of a session: `echo probe | gpg --clearsign --pinentry-mode error -u "$(git config user.signingkey)" > /dev/null`; non-zero means ask Paul to unlock, never disable signing.
- Gate rule (spec §6.6): if Task 4's region-read check, Task 5's autosave or futures check, or Task 7's login-gate check fails on Canvas, stop, do not ship the feature switched off, and report the finding with the ways around it that were found.

## Review Focus

1. **One lagging region among many idle ones:** the report must carry that region's TPS and MSPT, not an average. Task 4, `RegionTicksTest`.
2. **A region whose save never completes** (it died or merged between ticket and tick): the snapshot gives up at the existing deadline, restores autosave, logs, and the next interval snapshots again. Task 5, `SnapshotWaitTest` with a future that never completes.
3. **A snapshot on an empty Canvas server** (no live region, `save-all` says `No regions to save`): the snapshot completes with no futures to wait on. Task 5's agent-test phase checks the first snapshot before any chunk is force-loaded.
4. **Purpur after the scheduler move:** readiness, first report, `ExecuteCommand` and snapshots behave as before. Task 2 runs `make agent-test` on Purpur as well as Canvas, Task 7 runs the tutorial e2e on both.
5. **A `threaded-regions` overlay on a Purpur group:** refused with the key named, not silently ignored. Task 6, `TestPaperRefusesThreadedRegions`.

---

### Task 1: The Canvas pin and image

**Files:**
- Create: `nix/canvas.nix`, `nix/canvas-image.nix`
- Modify: `flake.nix` (packages `canvas`, outputs `canvas-repo`, `canvas-image`), `Makefile` (`CANVAS_IMAGE`, `canvas-image`, `canvas-image-load`, `canvas-image-test`, `image-repro`), `hack/image-test.sh` (`AOT=0` skips the startup-cache section), `nix/paper-jre.nix` only if jdeps names a new module

**Interfaces:**
- Produces: `canvas.canvasVersion`, `canvas.canvasBuild`, `canvas.canvasJar`, `canvas.repo` (same shape as `purpur.repo`: `versions/ libraries/ cache/`); flake outputs `.#canvas-repo`, `.#canvas-image` (x86_64-linux only); Make variable `CANVAS_IMAGE`; `make canvas-image-test` runs `hack/image-test.sh` with `AOT=0`.

- [ ] **Step 1: Write `nix/canvas.nix`** as `nix/purpur.nix` with `canvasVersion = "26.3"`, `canvasBuild = "991"`, the Jenkins URL and hash from Global Constraints, `pname = "canvas-repo"`, Paper's `mojangJar` copied to `work/cache/mojang_${canvasVersion}.jar`.
- [ ] **Step 2: Write `nix/canvas-image.nix`** as `nix/purpur-image.nix` with the image values from Global Constraints, `paper-jre`, the same agent jar path and `image/entrypoint.sh`, launch files from `nix/flat-launch.nix` with `home = "/opt/canvas"`.
- [ ] **Step 3: Wire the flake and Makefile.** `canvas = pkgs.callPackage ./nix/canvas.nix { inherit (paper) mojangJar; };`, `canvas-repo = canvas.repo;`, `canvas-image` next to `purpur-image`. Make targets mirror the `purpur-image*` ones with out-link `result-canvas`; `image-repro` builds `.#canvas-image` twice like the others. `hack/image-test.sh` skips everything from its AOT section on when `AOT=0`.
- [ ] **Step 4: Build.** Run: `git add -A && nix build .#canvas-image --out-link result-canvas`. Expected: success.
- [ ] **Step 5: Re-derive the JRE modules** with the `jdeps` command in `nix/paper-jre.nix`'s comment over `.#canvas-repo` and the agent jar. Expected: no module outside the existing list; if there is one, add it to the shared list.
- [ ] **Step 6: Run the image test and record the failure Task 2 fixes.** Run: `ND make canvas-image-test CONTAINER=podman`. Expected: FAIL with `the agent plugin did not load, or did not report why it stayed dormant` (Canvas skips a plugin without `folia-supported`). Anything failing earlier is a Task 1 defect.
- [ ] **Step 7: Commit** `feat(images): a Canvas 26.3 image beside Purpur's`, body naming build 991 and that the agent does not load yet.

### Task 2: The agent on Paper's region schedulers

**Files:**
- Modify: `agent/paper/src/main/resources/paper-plugin.yml`, `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt`
- Modify: `Makefile` (`canvas-agent-test`: `agent-test` with `IMAGE=$(CANVAS_IMAGE)`)

**Interfaces:**
- Produces: no `server.scheduler` call anywhere under `agent/paper/src/main`; `make canvas-agent-test`.

- [ ] **Step 1: The failing check** is Task 1's Step 6. Also run `ND make canvas-agent-test CONTAINER=podman` once and keep the `no hello within` failure.
- [ ] **Step 2: Implement.** `folia-supported: true` in `paper-plugin.yml`. In `AgentPlugin.kt`: `execute()`, the snapshot timer and the snapshot's restore on `server.globalRegionScheduler` (`execute`, `runAtFixedRate`), the snapshot's waiting on `server.asyncScheduler.runNow`, the sampling timer on `globalRegionScheduler.runAtFixedRate(this, {…}, 1L, SAMPLE_TICKS)`. The existing `state.sample` before `session.start()` stays.
- [ ] **Step 3: Verify on Canvas.** Run: `ND make canvas-image-test canvas-agent-test CONTAINER=podman`. Expected: image test passes through the dormant and shutdown checks; every agent-test phase passes. If `All RegionFile I/O tasks to complete` never appears on Canvas, find Canvas's shutdown line in its log and make the image test accept either, naming both.
- [ ] **Step 4: Verify on Purpur.** Run: `ND make purpur-image-test agent-test CONTAINER=podman`. Expected: PASS, unchanged.
- [ ] **Step 5: Commit** `feat(agent): run on Paper's region schedulers so Canvas loads it`.

### Task 3: The stub operator sends commands and records ticks

**Files:**
- Modify: `cmd/spawnery-stubop/main.go`, `cmd/spawnery-stubop/main_test.go`

**Interfaces:**
- Produces: repeatable flag `--execute <command line>`; on a backend stream, after the agent's `Ready`, the stub sends `ExecuteCommand{id: 1, command: first}`, and after each `ExecuteOutcome` the next one, each command once per stub process across streams. Event `execute_outcome` with fields `id`, `ok`, `output` (array), `error`. Event `player_count` gains `tps` and `mspt`.

- [ ] **Step 1: Write the failing tests** in `main_test.go`, using its existing harness for a fake backend stream:
  - `TestExecuteSendsCommandsInOrderAfterReady`: stub with `--execute "a" --execute "b"`; nothing sent before `Ready`; after `Ready` the stream receives `ExecuteCommand{Id: 1, Command: "a"}`; after an `ExecuteOutcome{Id: 1, Ok: true}` it receives `{Id: 2, Command: "b"}`; the events file holds `execute_outcome` with `"id":1,"ok":true`.
  - `TestPlayerCountRecordsTicks`: a `PlayerCount{Tps: 19.5, Mspt: 12.25}` is recorded with `"tps":19.5,"mspt":12.25`.
- [ ] **Step 2: Run** `ND go test ./cmd/spawnery-stubop/ -run 'TestExecuteSendsCommandsInOrderAfterReady|TestPlayerCountRecordsTicks' -count=1`. Expected: FAIL (unknown flag, missing fields).
- [ ] **Step 3: Implement** in `observeServer` (sending through its return value keeps `Send` on one goroutine) and `playerCount`.
- [ ] **Step 4: Run** the same command. Expected: PASS. Then `ND go test ./cmd/spawnery-stubop/ -count=1`: PASS.
- [ ] **Step 5: Commit** `test(stubop): send commands after Ready and record tick reports`.

### Task 4: Tick reports from the platform

**Files:**
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/ServerPlatform.kt` (`TickSample`, `ServerPlatform`, `PaperPlatform`, `Platforms`), `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/RegionTicks.kt`, `agent/paper/src/canvas/kotlin/cloud/spawnery/agent/paper/canvas/CanvasPlatform.kt`
- Test: `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/RegionTicksTest.kt`, `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/PlatformsTest.kt`
- Modify: `agent/paper/build.gradle.kts` (source set `canvas`), `nix/agents.nix` (link `paper/canvas-repo`), `flake.nix` (pass `canvas` to `agents`), `AgentPlugin.kt` (sample through the platform), `hack/agent-test.sh` (tick sanity)

**Interfaces:**
- Produces:
  - `data class TickSample(val tps: Double, val mspt: Double)`
  - `interface ServerPlatform { fun ticks(): TickSample; fun saveRegions(): List<CompletableFuture<*>> }`, both called on the global region thread.
  - `class PaperPlatform(server: Server) : ServerPlatform`: `ticks()` = `TickSample(Bukkit.getTPS()[0], Bukkit.getAverageTickTime())`, `saveRegions()` = empty list.
  - `object Platforms { const val CANVAS_MARKER = "io.canvasmc.canvas.region.WorldRegionizer"; const val CANVAS_IMPL = "cloud.spawnery.agent.paper.canvas.CanvasPlatform"; fun detect(server: Server, loader: ClassLoader): ServerPlatform }`: `CANVAS_IMPL` through its `(Server)` constructor when `CANVAS_MARKER` loads, else `PaperPlatform`.
  - `object RegionTicks { fun worst(regions: List<TickSample>, fallback: TickSample): TickSample }`: lowest TPS and highest MSPT taken independently; `fallback` when `regions` is empty.
  - `class CanvasPlatform(server: Server) : ServerPlatform`: one `TickSample(getTPS(_1_MINUTE), getMSPT(_5_SECONDS))` per region of every world from `computeForAllChunkRegions`, skipping `hasNoAliveSections()` and state `DEAD`; fallback is the global tick (`Bukkit.getTPS()[0]`, `getAverageTickTime()` on the global thread). `saveRegions()` is Task 5's.

- [ ] **Step 1: Write the failing tests.**
  - `RegionTicksTest`: `` `one lagging region among thirty idle ones is the report` `` (29 × `TickSample(20.0, 3.0)` plus `TickSample(8.0, 120.0)` → `TickSample(8.0, 120.0)`); `` `tps and mspt may come from different regions` `` (`(19.0, 60.0)` and `(12.0, 40.0)` → `(12.0, 60.0)`); `` `no region reports the fallback` `` (`worst(emptyList(), TickSample(20.0, 1.5))` → `TickSample(20.0, 1.5)`).
  - `PlatformsTest`: `` `without the Canvas marker the platform is Paper's` `` (a `ClassLoader` that cannot load `CANVAS_MARKER` gives a `PaperPlatform`). A wrong `CANVAS_IMPL` shows in Step 6, where the Canvas log must name `CanvasPlatform`.
- [ ] **Step 2: Run** `ND make agent`. Expected: FAIL to compile (`RegionTicks`, `Platforms` unknown).
- [ ] **Step 3: Add the source set.** In `agent/paper/build.gradle.kts`: `canvasLibraries = fileTree("canvas-repo/libraries")` (jars, protobuf excluded as for Paper); source set `canvas` with `compileOnly` on `canvasLibraries` and on the main source set's output, never on `paperLibraries` (Canvas's API jar carries its own Bukkit copy); `shadowJar` includes the canvas output. `nix/agents.nix` takes `canvas` and links `${canvas.repo}` to `paper/canvas-repo`. No new Maven coordinates, so `agent/deps.json` stays; if Gradle disagrees, `make agent-deps` and commit the result.
- [ ] **Step 4: Implement** the Interfaces block, then in `AgentPlugin.onEnable` pick `Platforms.detect(server, server.javaClass.classLoader)` once and sample `state.sampleTicks` from `platform.ticks()`, logging the chosen platform once at INFO.
- [ ] **Step 5: Run** `git add -A && ND make agent`. Expected: PASS, JUnit green, `hack/agent-jar-check.sh` green.
- [ ] **Step 6: Tick sanity in the agent test.** `hack/agent-test.sh` phase one: the first `player_count` with `tps > 0` has `tps <= 20` and `mspt >= 0`. Add a Canvas-only step (when `$CONTAINER image inspect` shows the label `cloud.spawnery.canvas-build`): the log names `CanvasPlatform` as the chosen platform; stub `--execute "forceload add 0 0 15 15"`, then the log shows no exception from the platform and later reports still satisfy the bounds. That step is the gate check that region tick data can be read from the global thread.
- [ ] **Step 7: Run** `ND make agent-test canvas-agent-test CONTAINER=podman`. Expected: PASS on both. Gate rule applies to the Canvas step.
- [ ] **Step 8: Commit** `feat(agent): report the worst region's ticks on Canvas`.

### Task 5: World sync on Canvas

**Files:**
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/SnapshotWait.kt`
- Test: `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/SnapshotWaitTest.kt`
- Modify: `CanvasPlatform.kt` (`saveRegions`), `AgentPlugin.kt` (`snapshot`), `hack/agent-test.sh` (phase `snapshot`, `AGENT_TEST_ONLY=snapshot`)

**Interfaces:**
- Consumes: `ServerPlatform.saveRegions()` (Task 4), `WorldSync.Outcome`, `RegionWrites`.
- Produces: `object SnapshotWait { fun await(saves: List<CompletableFuture<*>>, timeoutMillis: Long): WorldSync.Outcome }`: `Ready` when all complete, `Failed(reason)` naming the first failure's message, `TimedOut` at the deadline; an empty list is `Ready` at once. `CanvasPlatform.saveRegions()` returns `propagateSaveAllTicket(false)` of every live region of every world.

- [ ] **Step 1: Write the failing tests** in `SnapshotWaitTest`: `` `no regions is ready at once` ``; `` `all saves complete` `` (two completed futures → `Ready`); `` `a failed save fails the snapshot with its reason` `` (one `completeExceptionally(IllegalStateException("region 7"))` → `Failed` whose reason contains `region 7`); `` `a save that never completes times out` `` (an incomplete future, timeout 50 → `TimedOut` within one second).
- [ ] **Step 2: Run** `ND make agent`. Expected: FAIL to compile.
- [ ] **Step 3: Implement** `SnapshotWait` and `CanvasPlatform.saveRegions()`. In `AgentPlugin.snapshot`: on the global thread, autosave off, the existing `save-all` (or `save-all flush`), then `val saves = platform.saveRegions()`; on the async scheduler `SnapshotWait.await(saves, SNAPSHOT_WAIT_MILLIS)` first, and only on `Ready` the existing `writes.await(levels)`, request and wait; any other outcome skips the request and goes to the restore and its warning as today.
- [ ] **Step 4: Run** `git add -A && ND make agent`. Expected: PASS.
- [ ] **Step 5: Add the `snapshot` phase** to `hack/agent-test.sh`, run for any image:
  1. Empty server: volume prepared through `--entrypoint /bin/bash` with `/data/.spawnery-worldsync/ready`; container with `SPAWNERY_WORLD_SYNC=1`, `SPAWNERY_WORLD_SYNC_INTERVAL=10s` and a stub without `--execute`. The first `snapshot.request` is answered (its number written to `snapshot.done`) and the log shows no `spawnery world sync: snapshot` warning before the next one. This is Review Focus 3. Stop the container.
  2. Fresh volume and container as in 1, stub with `--execute "forceload add 0 0"` `--execute "setblock 0 100 0 minecraft:gold_block"`. Requests that arrive before both `execute_outcome` events are `ok` are answered without copying.
  3. The first `snapshot.request` after both outcomes is answered the moment it appears by copying `/data/world` out (`$CONTAINER exec … tar -C /data -cf - world`), then writing `snapshot.done`.
  4. `$CONTAINER kill` the server (no shutdown save). New volume with the control files and the copied `world`; a new container and stub with `--execute "forceload add 0 0"` `--execute "execute if block 0 100 0 minecraft:gold_block"`. Expected: the second outcome is `ok` with `Test passed` in its output.
- [ ] **Step 6: The autosave gate check**, in the same phase on Canvas: overlay `paper-world-defaults.yml` with `chunks: {auto-save-interval: 20}`; stub commands `forceload add 0 0`, `save-all`, `save-off`, `setblock 0 101 0 minecraft:gold_block`; the mtime of `/data/world/dimensions/minecraft/overworld/region/r.0.0.mca` does not change within 5 s; after `save-on` it changes within 5 s. `save-off` sets the same per-level flag as `World.setAutoSave(false)`. Gate rule applies.
- [ ] **Step 7: Run** `ND env AGENT_TEST_ONLY=snapshot make agent-test canvas-agent-test CONTAINER=podman`. Expected: PASS on both images.
- [ ] **Step 8: Show what the phase catches.** In a throwaway worktree (`git worktree add --detach`), drop the `SnapshotWait.await` call, rebuild the Canvas image, run the phase five times on Canvas and write down how many fail. Remove the worktree. If it does not fail reliably, say so in the commit body: `SnapshotWaitTest` is then the guard, and the phase proves the integration.
- [ ] **Step 9: Run the whole agent test once more** (`ND make agent-test canvas-agent-test CONTAINER=podman`). Expected: PASS.
- [ ] **Step 10: Commit** `feat(agent): wait for Canvas's region saves before a snapshot`, body with Step 8's count.

### Task 6: The `canvas` renderer flavour

**Files:**
- Create: `internal/render/defaults/canvas-paper-global.default.yml` (and `canvas-paper-world-defaults.default.yml` only if Canvas's differs from Paper's)
- Modify: `internal/render/declared.go`, `internal/render/paper.go`, `cmd/spawnery-config/main.go`, `image/entrypoint.sh`, `internal/render/paper_test.go` (regeneration note), `image/entrypoint_test.go`

**Interfaces:**
- Produces: `func Canvas(v Values, secret string, overlay map[string]string) (map[string][]byte, error)` writing the same `PaperFiles` as `Paper` with the Canvas key tree for `paper-global.yml`; `spawnery-config --flavor canvas`; the entrypoint runs `spawnery-config --flavor "${SPAWNERY_CONFIG_FLAVOR:-paper}"`.

- [ ] **Step 1: Capture Canvas's defaults** from a first start of `.#canvas-image` with a data volume (`podman cp` of `config/paper-global.yml` and `config/paper-world-defaults.yml`), next to the regeneration note for Paper's. Diff the world defaults against `paper-world-defaults.default.yml`: identical means one file serves both, as for Purpur.
- [ ] **Step 2: Write the failing tests** in `internal/render`: `TestCanvasAcceptsThreadedRegions` (overlay `paper-global.yml` = `threaded-regions:\n  threads: 4\n` renders, output contains `threads: 4`); `TestPaperRefusesThreadedRegions` (same overlay through `Paper` fails with an error containing `threaded-regions`); `TestCanvasWritesThePaperFiles` (`Canvas` returns exactly the keys of `PaperFiles`). In `image/entrypoint_test.go`: with `SPAWNERY_CONFIG_FLAVOR=canvas` the `spawnery-config` double receives `--flavor canvas`, without it `--flavor paper`.
- [ ] **Step 3: Run** `ND go test ./internal/render/ ./image/ -count=1`. Expected: FAIL.
- [ ] **Step 4: Implement.** `paperGlobal` takes the declared tree as a parameter; `Paper` and `Canvas` share one body. `spawnery-config` accepts `canvas`. If `image/reserved_env_test.go` objects to the new variable, follow what it asks for.
- [ ] **Step 5: Run** `ND go test ./internal/render/ ./image/ ./cmd/spawnery-config/ -count=1`, then `ND make test` (with `-p 1` for envtest on `dev`). Expected: PASS.
- [ ] **Step 6: Rebuild and run** `ND make canvas-image-test purpur-image-test CONTAINER=podman`. Expected: PASS.
- [ ] **Step 7: Commit** `feat(render): a canvas flavour that accepts threaded-regions`.

### Task 7: Canvas in the end-to-end runs

**Files:**
- Modify: `hack/e2e-worldsync.sh`, `hack/e2e-tutorial.sh`, `test/e2e/` (manifest image substitution), `Makefile` (`e2e-worldsync-canvas`, `e2e-tutorial-canvas`)

**Interfaces:**
- Produces: `GAME=purpur|canvas` (default `purpur`) for both scripts: build, archive and `kind load` `.#${GAME}-image`; `SPAWNERY_E2E_GAME_IMAGE=<repo:tag>` handed to `go test`; the e2e manifest helper replaces an `image: ghcr.io/spawnery/purpur:<tag>` line with that image when the variable is set. The scripts' own "manifest names the image this run builds" checks compare against the Purpur reference the manifest carries, as today.

- [ ] **Step 1: Implement** the knob and the substitution; `make e2e-worldsync-canvas` is `GAME=canvas hack/e2e-worldsync.sh`, likewise for the tutorial.
- [ ] **Step 2: Run on Purpur** `make e2e-worldsync` and `make e2e-tutorial` (rootless podman wrapper from Global Constraints). Expected: PASS, unchanged.
- [ ] **Step 3: Run on Canvas** `make e2e-worldsync-canvas` and `make e2e-tutorial-canvas`. Expected: PASS. The tutorial's playable-slots test is the login-gate gate check (gate rule applies); its `cloud execute lobby say …` shows `ExecuteCommand` through a real proxy on Canvas. If `PlayerLoginEvent` fires off the global thread, check that `LoginGateListener`'s `getOnlinePlayers()`/`hasPermission` reads are allowed there before calling it passed.
- [ ] **Step 4: Commit** `test(e2e): run the world-sync and tutorial paths on Canvas`.

### Task 8: Pinning Canvas

**Files:**
- Create: `hack/canvas-pin.sh`, `hack/canvas-pin-test.sh`, `hack/testdata/canvas-builds.json`
- Modify: `Makefile` (`canvas-pin`, `canvas-pin-check`, `canvas-pin-test` added where `make test` runs the other hack tests), `.github/workflows/pin-bump.yml`, `hack/image-version-bump.sh` and its test, `hack/image-tag-pins-agree.sh`

**Interfaces:**
- Produces: `hack/canvas-pin.sh [VERSION [BUILD]]`, `CHECK=1`, exit codes as `hack/purpur-pin.sh`; reads `API` (default `https://canvasmc.io/api/v2/builds?project=canvas`) so the test can point it at a `file://` fixture, and `NIX_FILE` (default `nix/canvas.nix`).

- [ ] **Step 1: Write the fixture and failing test.** `canvas-builds.json` holds, newest first: an experimental 26.3 build 995, a 26.3 build 994 with `channelName` other than `stable`, a 26.3 `FAILURE` build 993, a 26.4 build 992, a stable 26.3 build 991. `canvas-pin-test.sh` asserts `CHECK=1 API=file://… hack/canvas-pin.sh 26.3` picks 991, and that asking for build 995 is refused as experimental.
- [ ] **Step 2: Run** `hack/canvas-pin-test.sh`. Expected: FAIL (no script).
- [ ] **Step 3: Implement** after `hack/purpur-pin.sh`: client-side filter on `channelVersion`, `result == "SUCCESS"`, `isExperimental == false`, `channelName == "stable"`; SHA-256 computed from the download, with a line saying the API publishes no checksum and the diff wants review; refuse a build whose `META-INF/download-context` names another Mojang jar than `nix/paper.nix`.
- [ ] **Step 4: Run** `hack/canvas-pin-test.sh` and `ND make canvas-pin-check`. Expected: PASS, and the check reports build 991 or a newer stable one.
- [ ] **Step 5: Version tooling.** `hack/image-version-bump.sh` and `hack/image-tag-pins-agree.sh` treat `canvas` like `purpur` (`canvasVersion` from `nix/canvas.nix`); extend their tests with a `canvas:` tag.
- [ ] **Step 6: The weekly bump.** In `pin-bump.yml`, after the Paper and Purpur pins move and before anything is committed: move the Canvas pin, run `make canvas-image-test canvas-agent-test`; on failure `git checkout nix/canvas.nix` and keep `canvas_failed=<build>` for the pull request body ("Canvas build N failed its tests and is not in this bump"). The Canvas pin, when kept, is committed with the others and counted in the summary.
- [ ] **Step 7: Run** `ND make test` (`-p 1` on `dev`). Expected: PASS. `actionlint` over the workflow if the dev shell has it.
- [ ] **Step 8: Commit** `feat(images): pin Canvas and move it in the weekly bump`.

### Task 9: CI, nightly and release

**Files:**
- Modify: `.github/workflows/ci.yml`, `.github/workflows/nightly.yml`, `.github/workflows/release.yml`, `hack/publish.sh`, `hack/require-no-red-nightly.sh` (text)

**Interfaces:**
- Consumes: `.#canvas-image`, `make canvas-image-test`, `make canvas-agent-test`, `make e2e-worldsync-canvas`, `make e2e-tutorial-canvas`.

- [ ] **Step 1: CI** builds `.#canvas-image` in "Build the game images".
- [ ] **Step 2: Nightly** runs `make e2e-worldsync-canvas` and `make e2e-tutorial-canvas` as their own steps with their own issue texts, and `image-repro` covers Canvas through the Makefile.
- [ ] **Step 3: Release.** `release.yml` lists the Canvas tag, includes `canvas:${canvas_tag}` in the tag/imageVersion pair check and `canvas-image` in the publish loop; `hack/publish.sh` adds `"canvas-image:result-canvas"` after the Purpur entries, and its AOT branch stays `purpur-image*` only.
- [ ] **Step 4: Dry run.** `ND env DRY_RUN=1 CONTAINER=podman hack/publish.sh canvas-image`. Expected: exit 0, names `ghcr.io/spawnery/canvas:26.3-<imageVersion>`, trains no cache.
- [ ] **Step 5: Commit** `ci: build, test and publish the Canvas image`.

### Task 10: Docs

**Files:**
- Create: `docs/guides/canvas.md`
- Modify: `mkdocs.yml` (nav entry "Canvas servers" under Guides), `docs/contributing/development.md` (the Canvas make targets), `README.md` if it lists the images

- [ ] **Step 1: Write the guide:** the image and its tag; plugins must declare `folia-supported: true` or Canvas skips them without a log line; `threaded-regions` through the `paper-global.yml` overlay; `canvas-server.yml` and `canvas-worlds.yml` through `extraFiles`; TPS/MSPT are the worst region's, with the windows; `ExecuteCommand` runs what Canvas's console runs; no startup cache for Canvas. Each claim carries its measurement date where the repo's docs do.
- [ ] **Step 2: Build the docs** with the repo's docs target (`nix build .#docs-site --no-link`). Expected: success, no broken links.
- [ ] **Step 3: Humanizer** over the new prose (embedded mode), then commit `docs(guides): running a group on Canvas`.

### After the last task

- [ ] Remove the spike: `git -C ~/git/spawnery worktree remove ~/git/spawnery-canvas-spike` and `podman rmi ghcr.io/spawnery/canvas-spike:26.3-0.28.1`.
- [ ] Push `feat/canvas-image` and open the pull request (not merge); the body lists the gate checks with their results and Task 5 Step 8's count.
