# A JVM startup cache in the game images

**Status:** approved, planned (docs/superpowers/plans/2026-10-05-aot-cache.md)
**Date:** 2026-10-05

## 1. What goes wrong today

A Paper or Purpur server spends its first seconds in work that is the same
on every start: loading and linking the JDK's and Minecraft's classes,
building registries, initialising DataFixer. The images run a jlink'd JRE
without a CDS archive, so not even the JDK's own classes come from a shared
archive. On a busy node that work stretches: a game server with a small CPU
request on a loaded node took 25 s from JVM start to "Starting Minecraft
server", for work an idle node does in 6 s.

Java 25 can record what a training run loaded and linked into an AOT cache
(JEP 483, 514, 515) and map it at the next start.

Measured on 2026-10-05, on a 16-vCPU node (AMD Ryzen 9 7950X3D, KVM), with
`ghcr.io/spawnery/purpur:26.3-0.22.0` and no plugins, from JVM start to
`Done`:

| | runs |
|---|---|
| without a cache | 8.9 s, 8.5 s |
| with `-XX:AOTCache`, trained with `-XX:AOTCacheOutput` in one run | 5.4 s, 5.6 s |

The cache was 160 MB. Its training run warned
`Skipping paperclip/libs/…: Unlinked class not supported by AOTClassLinking`:
the bundler starts the server through a class loader of its own, and the
cache only holds classes from the JDK's built-in loaders. The 36 % came from
the JDK's classes and the part of Minecraft's the bundler leaves to the
application loader.

The spike the first version of this design asked for ran on the same node
and image the same day:

| | JVM start to `Done` |
|---|---|
| bundler launch, no cache | 8.9 s |
| flat launch, no cache | 8.0 s |
| flat launch, cache | 3.7 s, 3.8 s |
| flat launch, cache trained under another directory | 3.5 s |

- The flat launch needs the server jar from `versions/` ahead of the
  libraries on the class path; the other way round, Mojang's logging library
  shadows the server's patched `LogUtils` (`NoSuchMethodError`).
- Two training runs on the same input gave different cache files (196 MB,
  different SHA-256), so the training cannot sit in the reproducible build.
- A cache trained with the class path under another directory was mapped
  and used, so the training does not need the runtime paths.

Training under the image's entrypoint, on the dev VM the same day:

- On a world the training boot creates, writing the cache failed twice in a
  row: the JDK's interned MethodType table grew past 256 KiB, the most the
  cache holds for one object. On a world an earlier boot created, the cache
  was written (193 MB) each time.
- The JVM writes the cache from a child process that inherits every flag
  while the training JVM still holds its heap. With `AlwaysPreTouch` under
  a 2 GiB limit the child was OOM-killed; without it, under 4 GiB, the cache
  was written.

## 2. The shape

Two changes to the Purpur images, and an operator flag that mounts the
cache (§3). Velocity is unchanged.

1. **A flat launch.** The entrypoint starts the server's main class directly
   on a class path built from the bundler's own manifests, instead of
   `-jar paper.jar`. Minecraft's and Paper's classes then load through the
   application loader and land in the cache.
2. **A trained cache beside the image.** A training run boots the server on
   a flat world it created in an earlier boot, with no plugins, until
   `Done`, and stops it. The cache it writes ships as an image of its own
   (below), and the entrypoint passes `-XX:AOTCache=…` when the file is
   mounted.

Plugins load through Paper's plugin class loaders and stay out of the cache;
a group's plugins cost what they cost today.

## 3. Decisions

- **Trained on the image's own class path, not per server.** The cache
  depends on the JDK build and the exact class path, both fixed by the image
  tag; it does not depend on a group's plugins, files or world. One cache per
  image serves every group.
- **The cache is a separate image.** Training output is not reproducible
  (above), and the game images are bit-for-bit reproducible
  (`make image-repro`), which stays. The release workflow trains each game
  image it publishes and pushes the cache as
  `ghcr.io/spawnery/purpur-aot:<tag>`, an image holding `/server.aot` and
  nothing else, declared as not reproducible. The operator mounts it as an
  image volume at `/var/run/spawnery/aot` for servers running one of
  spawnery's own game images at an image version that has caches; for any
  other image, or a version from before, nothing is mounted and the server
  starts as today.
- **A cache that does not fit is ignored, not fatal.** The JVM prints a
  warning and starts without it when the JDK, the class path or a relevant
  flag differs. The entrypoint passes the flag only when the file exists,
  and the image tests assert that the shipped cache is actually used (no
  "unable to use" in the start log, `-Xlog:aot` reports it mapped).
- **Training runs through the entrypoint.** `SPAWNERY_AOT_OUTPUT` makes the
  entrypoint pass `-XX:AOTCacheOutput` instead of `-XX:AOTCache`, so the GC
  and heap flags are the ones every server starts with. The one flag it
  drops while training is `AlwaysPreTouch` (above); pre-touching is not part
  of what the cache records.
- **The cache is behind an operator flag, off by default.** An image volume
  needs the `ImageVolume` feature, and on a cluster without it the API
  server refuses every pod that carries one. `--aot-cache` (chart value
  `operator.aotCache`) turns the mount on. The volume is added after the pod
  is rendered, outside the pod hash, so turning the flag on restarts no
  server; each picks it up at its next start.
- **The cache goes up before its game image.** A mounted image that cannot
  be pulled keeps the pod from starting, so the release pushes
  `purpur-aot:<tag>` first. A new package on ghcr.io starts private; the
  first release needs both switched to public.
- **Purpur only; the Paper image is retired.** `ghcr.io/spawnery/paper` has
  been deprecated since 0.2.15. This release stops building and publishing
  it rather than giving it a flat launch and a cache; tags already published
  stay on the registry. `nix/paper.nix` stays, as the source of Purpur's
  Mojang jar and of the renderer tests' Paper repo.
- **No bundler fallback.** An image without the launch files refuses to
  start with a message naming the missing file, rather than starting the old
  way without a cache and without anyone noticing.
- **The flat launch reads the bundler's manifests at build time.** The class
  path and main class come from `META-INF/libraries.list`,
  `META-INF/versions.list` and `META-INF/main-class` inside the server jar.
  They are written into the image as a file the entrypoint reads, so a
  Paper or Purpur update that changes its libraries changes the file with
  it.

## 4. Delivery

- `nix/flat-launch.nix`, used by `nix/purpur-image.nix`: derives the class path
  file and the main class from the bundler manifests.
- `image/entrypoint.sh`: `exec java … -cp "$(cat classpath)" <main>` instead
  of `-jar`, plus `-XX:AOTCache=<file>` when the cache file exists.
- `hack/aot-image.sh`: trains a game image and builds its cache image; the
  release workflow calls it after publishing each game image.
- `internal/podspec`, `internal/controller`, `cmd/spawnery-operator`, the
  chart: the cache image volume for spawnery's own game images behind
  `--aot-cache`.
- `hack/image-test.sh`: the cache is used, and the flat launch reaches
  `Done`.

## 5. Versions

The image changes (entrypoint, contents): a minor step for `imageVersion`.
The operator changes too (it mounts the volume): a minor step for the
operator and the chart.

## 6. Testing

- Entrypoint tests: the flat launch's command line, with and without a cache
  file.
- Image test: the server reaches `Done` with the flat launch; trained under
  one memory limit and run under another, the start log shows the cache
  mapped; with `-XX:-UseCompressedOops` in `JAVA_TOOL_OPTIONS` the cache is
  dropped and the server starts anyway.
- The trainer fails, naming the boot, when the server does not reach `Done`.
- Measurement, recorded in the release notes: JVM start to `Done` with and
  without the cache, flat launch, on the same node as above.
- `make image-repro` stays green.

## 7. Not in this design

- Caching plugin classes. Paper's plugin class loaders are outside what the
  JDK's AOT cache can hold.
- The Velocity image.
- Training on a group's real plugins and world.
