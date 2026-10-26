# A Canvas game image

**Status:** proposed
**Date:** 2026-10-26

## 1. What this is

[Canvas](https://canvasmc.io) is a fork of Folia: one Minecraft server whose
worlds are cut into regions that tick on several threads. A network that
outgrows one core per server wants it, and it should run under spawnery like
Purpur does today: a `ghcr.io/spawnery/canvas` image with the same pod
contract, the same agent, and world sync.

The image is the small part. Folia changes the threading model under the
agent, and three of the agent's assumptions stop holding. This document is
mostly about those.

## 2. What the spike showed

Measured on 2026-10-26 on the dev VM (6 vCPUs, 12 GB), with Canvas 26.3
build 991 and an image built from `nix/purpur-image.nix` with only the jar
swapped (throwaway branch, not kept).

**The jar.** Canvas ships Paper's paperclip bootstrap. Its
`META-INF/download-context` names the same Mojang `server.jar`
(`33680f5f…`) that `nix/paper.nix` pins, so the patch-only build of
`nix/purpur.nix` and the flat launch of `nix/flat-launch.nix` work unchanged.
`paper-jre` started it; Canvas probes for the Vector API and falls back
without it. JVM start to `Done`: 7.8 s and 8.1 s, no AOT cache.

**The unchanged agent.** Canvas loaded the server and never loaded the
agent. There was no line in the log saying so: a plugin without
`folia-supported: true` is skipped without a message, and `hack/agent-test.sh`
timed out waiting for a hello.

**With the plugin flag and Paper's region schedulers** (`GlobalRegionScheduler`
for what ran on the main thread, `AsyncScheduler` for
`runTaskAsynchronously`), every backend phase of `hack/agent-test.sh` passed:
hello, API install, `/cloud list`, readiness (so `ServerLoadEvent` STARTUP
fires as on Paper), the first player count, overlapping renewals, supersession,
the muted operator. The proxy phases were not run (no Velocity image in that
run).

**Read from Canvas's source** (`CraftCanvasMC/Canvas`, main, same day):

- `CraftScheduler.handle` throws `UnsupportedOperationException` for every
  task, so any `server.scheduler` call is fatal at the call site.
- `Server.getTPS()` and `getAverageTickTime()` answer for the region that
  owns the calling thread. On the global region thread they answer for the
  global tick (`RegionizedServer.GlobalTickTickHandle`), which ticks weather,
  time and the console and has nothing to do with how the worlds tick. The
  number looks plausible and is wrong.
- `save-all` saves global data on the global thread, then hands every live
  region a `SaveAllTicket` and returns; each region saves at the start of its
  next tick. With no region alive it prints `No regions to save` and finishes
  at once, which is what the spike saw on an empty server.
- `CraftWorld.save()` propagates the same tickets.
- Canvas's API (`canvas-api`, bundled in the jar as
  `io.canvasmc.canvas:canvas-api:26.3.build.991-stable`) exposes
  `World.getRegionizer()`, `WorldRegionizer.computeForAllChunkRegions`,
  `ChunkRegion.getTickData()`, `RegionTickData.getTPS/getMSPT(Frame)` and
  `RegionTickData.propagateSaveAllTicket(flush): CompletableFuture<Void>`.
- Canvas's own spark integration reports the mean TPS over all regions plus
  the global tick.
- `paper-global.yml` carries one top-level section Paper's does not:
  `threaded-regions` (`threads`, `grid-exponent`, `scheduler`). Canvas also
  writes `config/canvas-server.yml` and `config/canvas-worlds.yml`.

**Distribution.** `https://canvasmc.io/api/v2/builds?project=canvas` lists
builds with `buildNumber`, `channelVersion`, `channelName`, `isExperimental`
and `downloadUrl` (Jenkins). It publishes no checksum. Its
`minecraft_version` filter is ignored: asking for 26.2 returned 26.3 builds.
Jenkins still served build 700 (April 2026), so old pins stay fetchable. One
download ran at 18 KB/s, a later one at 2.6 MB/s.

## 3. Goals

- `ghcr.io/spawnery/canvas:26.3-<imageVersion>`, built and released beside
  the Purpur image, same pod contract (port, `/data`, numeric user,
  `spawnery-slp`, read-only root).
- One agent jar for Purpur and Canvas. No second plugin, no build variant.
- Tick reports from a Canvas server that describe how its worlds tick.
- World sync on Canvas with the same guarantee it has on Purpur: a snapshot
  holds every chunk that was dirty when it was asked for.
- The login gate, cloud commands and `ExecuteCommand` work on Canvas.
- Pinned like Purpur: a pin script, the weekly pin bump, the release.

## 4. Non-goals

- A 26.2 Canvas image. 26.2 exists only to let networks move; nobody runs
  Canvas on it under spawnery yet.
- The AOT startup cache for Canvas. `--aot-cache` stays Purpur's; Canvas
  can follow once its numbers are measured.
- Overlays for `canvas-server.yml` and `canvas-worlds.yml`. They reach a
  server through `extraFiles` like any file the renderer does not write.
- Making third-party plugins Folia-safe. A plugin without
  `folia-supported: true` will not load on Canvas; that is Canvas's rule.
- Operator changes. The operator does not know which image a group runs and
  should not start to.

## 5. Anti-goals

- A tick figure that hides a lagging region behind 30 idle ones.
- A snapshot that can miss the chunks a region had not yet saved, without
  anything saying so.
- Purpur behaving differently afterwards, apart from where the scheduler
  calls now go (Paper runs global-region tasks on the main thread, so the
  observable order there is unchanged).

## 6. Design

### 6.1 Image and pins

`nix/canvas.nix` mirrors `nix/purpur.nix` (`canvasVersion`, `canvasBuild`,
`canvasJar`, `repo`, Paper's `mojangJar`), with the Jenkins `downloadUrl`
of the pinned build. `nix/canvas-image.nix` mirrors `nix/purpur-image.nix`:
home `/opt/canvas`, `SPAWNERY_PAPER_HOME=/opt/canvas`, label
`cloud.spawnery.canvas-build`, the same agent jar and the same
`image/entrypoint.sh`. The flake exposes `canvas-image` and `canvas-repo`.

`hack/canvas-pin.sh` follows `hack/purpur-pin.sh` (latest build, given
build, `CHECK=1`), with three differences forced by the API: it filters the
build list itself by `channelVersion`, takes only `result == SUCCESS`,
`isExperimental == false` and `channelName == stable`, and, having no
checksum to compare against, says so and leaves the review of the diff to
the person running it. It refuses a build whose `download-context` names
another Mojang jar than `nix/paper.nix`'s, as the Purpur script does.

The weekly pin bump runs it after Paper and Purpur and tests the Canvas image
too, in the same pull request. A Canvas pin that fails its tests is taken
back out of that pull request and named in its description, so a Canvas
problem never holds back a Paper or Purpur bump. CI builds `canvas-image`;
the release publishes it like the Purpur image, minus the startup cache.

The JRE module list is re-derived with the `jdeps` command in
`nix/paper-jre.nix` over Canvas's repo. If Canvas needs a module Paper does
not, it goes into the shared list rather than into a second JRE.

### 6.2 The renderer

`spawnery-config` learns a `canvas` flavour. It writes the files the Paper
flavour writes, and its declared key tree for `paper-global.yml` comes from
Canvas's own default file, so a `threaded-regions` overlay is accepted on
Canvas and still refused on Purpur, where Paper would ignore it. The
default file is captured from a Canvas server's first start and committed
under `internal/render/defaults/`, as Paper's is. The image tells the
entrypoint its flavour (`SPAWNERY_CONFIG_FLAVOR=canvas`); without the
variable the entrypoint keeps `paper`.

### 6.3 The agent

**Plugin metadata.** `paper-plugin.yml` gains `folia-supported: true`.
Paper reads and ignores the key.

**Scheduling.** Every `server.scheduler` call moves to Paper's region
schedulers, which Paper, Purpur and Canvas all implement:

- the sampling timer, `ExecuteCommand`, the snapshot timer and the snapshot's
  restore run on the `GlobalRegionScheduler`;
- the snapshot's waiting runs on the `AsyncScheduler`.

`GlobalRegionScheduler.runAtFixedRate` refuses an initial delay below 1, so
the sampling timer starts one tick later than today. That is harmless: the
explicit `state.sample` before `session.start()` already covers the first
report.

What then runs on the global region thread and has to be allowed there:
`Bukkit.getOnlinePlayers()`, `getMaxPlayers()`, `dispatchCommand` with the
console or a `createCommandSender` sender (Canvas allows exactly those on the
global thread), `World.isAutoSave`/`setAutoSave`, registering listeners.
The implementation confirms each of these on Canvas.

**Tick reports.** A small interface picks the source once at enable:

- on Paper and Purpur, `Bukkit.getTPS()[0]` and `getAverageTickTime()`, as
  today;
- on Canvas (detected by `io.canvasmc.canvas.region.WorldRegionizer` being
  loadable), every live region of every world through
  `computeForAllChunkRegions`, reporting the **lowest TPS** and the
  **highest MSPT** over them. With no live region (an empty
  server), the global tick's figures stand in, so the report is not zero.

Lowest and highest rather than Canvas's mean: the network dashboard already
summarises a group by its lowest TPS, and one region at 8 TPS is the one its
players notice. The windows are Paper's: one minute for TPS, as
`getTPS()[0]`, and five seconds for MSPT, as `getAverageTickTime()`
(`getMSPTData5s()`), so a Purpur and a Canvas server report comparable
numbers.

The Canvas side compiles against the `canvas-api` jar inside the pinned
Canvas bundle (`canvas.repo`'s `libraries/`), the way the Paper module
compiles against `paper.repo` today, so it follows every Canvas pin and an
API break fails `make agent` in the pin bump. `canvas-api` carries its own
copy of the Bukkit API and cannot share a compile classpath with
`paper.repo`, so the Canvas code lives in its own source set (or
subproject) that is packed into the one agent jar. Only classes loaded after
the detection check touch it, so Paper never resolves a Canvas type.

**World sync.** The snapshot keeps its shape: freeze autosave, save, wait
for the writes off the global thread, request, wait for the answer,
restore. On Canvas, "save" and "wait" change:

1. On the global thread: autosave off, then `save-all` for the global data,
   then `propagateSaveAllTicket(false)` on every live region, collecting the
   futures.
2. On the async scheduler: wait for all futures (bounded by the existing
   snapshot deadline), then `MoonriseRegionFileIO.flush` per level as today,
   then request the snapshot.

The futures must never be waited on from the global thread or a region
thread; Canvas documents that as a deadlock. A future that fails or times
out fails the snapshot the way a failed write does today, logged and with
autosave restored. Whether the explicit tickets duplicate the ones
`save-all` already handed out, and whether `save-all` can be replaced by a
narrower global-data save, is for the implementation to settle against
Canvas's `TicketHolder`; the requirement is only that every region's save
has completed before the flush.

The implementation also checks that `setAutoSave(false)` stops Canvas's
per-region chunk autosave. If it does not, the freeze has to be made some
other way, because a region saving during the copy is what the freeze
exists to prevent.

`WorldSyncBootstrap` runs before any world exists and needs no change.

**`ExecuteCommand`.** On Canvas the agent runs a command the way Canvas's
console does, on the global thread. Commands Canvas refuses or cannot run
there (many that touch a position) fail, and the failure goes back in the
`ExecuteOutcome` like any other. The agent does not try to route a command
to a region thread: a command line carries no position, and adding one is
an operator change §4 rules out. The guide says so.

**Login gate.** `PlayerLoginEvent` is not covered by the spike, which had
no player. The implementation joins a player to a Canvas server whose group
enforces playable slots and shows the gate refusing and admitting there. If
Canvas fires the event off the global thread, the gate's
`getOnlinePlayers()`/`hasPermission` reads are checked for safety on that
thread.

### 6.4 Tests

- JUnit, without Canvas: the lowest/highest aggregation including the
  empty-server fallback; the snapshot's wait over fake futures (all complete,
  one fails, one never completes).
- `hack/agent-test.sh`'s backend phases against the Canvas image, from
  `make agent-test`; the first player count's TPS is above 0 and at most 20.
- `hack/image-test.sh` against the Canvas image.
- `hack/e2e-worldsync.sh` runs once per image (Purpur and Canvas), nightly.
- A check that fails with today's snapshot flow on Canvas and passes with
  the new one: a chunk dirtied in a live region right before a snapshot
  (by a joined player, or by a command Canvas runs from the console, per
  §6.3) is in the world the next start restores. The implementation shows
  the failure with the old flow before relying on it.
- The login gate on Canvas, by a join (`spawnery-join`) in the e2e run or
  the agent test, whichever is cheaper.

### 6.5 Docs and versions

A guide page on running a group on Canvas: the image, that plugins must be
Folia-ready, the `threaded-regions` overlay, `canvas-server.yml` through
`extraFiles`, and what the TPS figure means there. The image reference lists
Canvas.

`imageVersion` moves (agent and image change). The release is a minor step:
a new image and a changed tick report on Canvas count as behaviour.
`operatorVersion` and the chart do not move unless the release carries
other changes.

### 6.6 Order

The open checks of §6.3 come first, before the image, pin and release
work: that `setAutoSave(false)` freezes region autosave, that every
region's save can be awaited before the flush, that the region tick data can
be read from the global thread, and where and on which thread
`PlayerLoginEvent` fires. World sync and the login gate are required on
Canvas, so a check that fails is not worked around by shipping the feature
switched off. Work stops and the finding goes back to the author, together
with the ways around it that were found.

## 7. Decided along the way

These were decided without asking; each is cheap to turn around.

- Lowest TPS / highest MSPT over regions, not the mean Canvas's spark
  integration uses (§6.3). Changing it is one function.
- Compiling against `canvas-api` from the pinned bundle rather than
  reflection or Canvas's Maven repository. It ties the agent derivation to
  the Canvas pin (as it is tied to Paper's) and needs its own source set;
  reflection would avoid both and lose the compiler's help.
- `ExecuteCommand` runs what Canvas's console runs, with no routing to
  region threads (§6.3).
- Canvas in the weekly bump's pull request, taken back out when it
  fails (§6.1), rather than a second pull request competing for the next
  `imageVersion`.
- A `canvas` renderer flavour rather than adding `threaded-regions` to
  the Paper tree for everyone, so Purpur keeps refusing a key it would
  ignore.
- No mirror for the jar. Jenkins still served build 700 from April 2026;
  if CI downloads prove slow, a mirror can come later without touching the
  pin's hash.
