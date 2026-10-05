# A JVM startup cache in the game images

**Status:** design, in review
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

## 2. The shape

Two changes to the game images (Paper and Purpur); Velocity and the operator
are unchanged.

1. **A flat launch.** The entrypoint starts the server's main class directly
   on a class path built from the bundler's own manifests, instead of
   `-jar paper.jar`. Minecraft's and Paper's classes then load through the
   application loader and land in the cache.
2. **A trained cache in the image.** A training run boots the server once,
   with a flat world and no plugins, until `Done`, and stops it. The cache it
   writes ships in the image, and the entrypoint passes `-XX:AOTCache=…`
   when the file is there.

Plugins load through Paper's plugin class loaders and stay out of the cache;
a group's plugins cost what they cost today.

## 3. Decisions

- **Trained on the image's own class path, not per server.** The cache
  depends on the JDK build and the exact class path, both fixed by the image
  tag; it does not depend on a group's plugins, files or world. One cache per
  image serves every group.
- **Where the training runs is decided by a spike, first task of the plan.**
  The images are bit-for-bit reproducible (`make image-repro`), and that
  stays:
  - If two training runs in the Nix build give identical cache files, and the
    JVM accepts a cache trained under `/nix/store/…/opt/paper` when the same
    layout sits at `/opt/paper` (relocated class path, JDK 19 and later), the
    training runs in the Nix build and the cache is part of the image.
  - Otherwise the cache becomes a separate image,
    `ghcr.io/spawnery/<flavor>-aot:<tag>`, built and trained by the release
    workflow from the published game image. The operator mounts it as an
    image volume at `/var/run/spawnery/aot` for every server whose image has
    one. The game image stays reproducible, and the cache image is declared
    as not being so.
- **A cache that does not fit is ignored, not fatal.** The JVM prints a
  warning and starts without it when the JDK, the class path or a relevant
  flag differs. The entrypoint passes the flag only when the file exists,
  and the image tests assert that the shipped cache is actually used (no
  "unable to use" in the start log, `-Xlog:aot` reports it mapped).
- **Training flags match the entrypoint's.** The GC and heap flags the
  entrypoint passes are the ones the training uses, so the cache is never
  refused over a flag.
- **The flat launch reads the bundler's manifests at build time.** The class
  path and main class come from `META-INF/libraries.list`,
  `META-INF/versions.list` and `META-INF/main-class` inside the server jar.
  They are written into the image as a file the entrypoint reads, so a
  Paper or Purpur update that changes its libraries changes the file with
  it.

## 4. Delivery

- `nix/paper.nix`, `nix/purpur.nix`: derive the class path file and the main
  class from the bundler manifests.
- `image/entrypoint.sh`: `exec java … -cp "$(cat classpath)" <main>` instead
  of `-jar`, plus `-XX:AOTCache=<file>` when the cache file exists.
- The training, in the Nix build or the release workflow per the spike.
- `hack/image-test.sh`: the cache is used, and the flat launch reaches
  `Done`.

## 5. Versions

The image changes (entrypoint, contents): a minor step for `imageVersion`.
If the spike chooses the separate cache image, the operator changes too (it
mounts the volume): a minor step for the operator and the chart.

## 6. Testing

- Entrypoint tests: the flat launch's command line, with and without a cache
  file.
- Image test: the server reaches `Done` with the flat launch; the start log
  shows the cache mapped and no refusal.
- Measurement, recorded in the release notes: JVM start to `Done` with and
  without the cache, flat launch, on the same node as above.
- `make image-repro` stays green.

## 7. Not in this design

- Caching plugin classes. Paper's plugin class loaders are outside what the
  JDK's AOT cache can hold.
- The Velocity image.
- Training on a group's real plugins and world.
