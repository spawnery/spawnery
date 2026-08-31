# Bringing a CloudNET network across

What a running CloudNET network needed from Spawnery that Spawnery did not
have. It was a list of gaps, kept so that none of them was rediscovered one at
a time during a migration.

**Four were closed in 0.2.15; a fifth was found while checking whether the
first four were enough, and is closed too.** The file stays for now because
what each gap cost and how each was measured is the answer to "why is it like
this" for several API fields and an image — and because a second CloudNET
network will put different weights on the same shapes. It goes when that stops
being useful.

## Where the numbers come from

One network, measured rather than imagined: **coding-area.net**, its configs
repository at `4eea9fa` on `staging`, read on 2026-08-31.

```
14 tasks     21 groups     19 templates     58 artifact descriptors
             (26 onedev-artifact across 19 projects, 17 modrinth,
              12 onedev-repo, 3 url)
```

That is one network's shape, and a second CloudNET network would put different
weights on the items below. What generalises is the shape of each gap, not the
counts — and the counts are here because they are what made the difference
between "worth doing later" and "nothing runs without it".

CloudNET composes a service from up to seven **templates**, merged by
ascending `priority`, the highest copied last and winning:

| priority | template | role |
|---|---|---|
| 0 | `Global/Global`, `Platform/Purpur` | network base + the server jar itself |
| 1 | `Global/Server` | every Minecraft server |
| 2 | `Global/Game` | game servers |
| 3 | `Global/Addon` | Arcadia addon games |
| 4 | `<Task>/default` | the individual task, always wins |

Spawnery has no equivalent, and does not need one: a *tool* can flatten those
seven into one tree per group before anything reaches the cluster. What
Spawnery needs is somewhere to put the result.

## The gaps

| | what it blocks | state |
|---|---|---|
| [Per-group process settings](#per-group-process-settings) | 6 of 14 tasks | **closed**, `spec.env` |
| [A tree that is not `plugins/`](#a-tree-that-is-not-plugins) | every task | **closed**, `spec.mounts` |
| [A volume shared between groups](#a-volume-shared-between-groups) | 6 tasks | **closed**, `spec.mounts` |
| [Purpur, not Paper](#purpur-not-paper) | every backend | **closed**, `ghcr.io/spawnery/purpur` |
| [The rest of the config files](#the-rest-of-the-config-files) | every backend | **closed**, `configOverlay` |

### Per-group process settings

**Closed by `spec.env`.** See [`group-environment.md`](group-environment.md).

Six of the fourteen tasks were inexpressible without it, because they differ
from a sibling by nothing else at all — same templates, same jars, same
configuration:

```
Bingo-Solo          -Dgame.amountOfTeams=0
Bingo-Team          -Dgame.soloTeams=false -Dgame.teamSize=4
OneBlockRace-Solo   -Dgame.amountOfTeams=0
OneBlockRace-Team   -Dgame.soloTeams=false -Dgame.teamSize=3
Ragemode-Solo       -Dgame.amountOfTeams=0 -Dgame.minPlayers=2 …
Ragemode-Team       -Dgame.soloTeams=false -Dgame.teamSize=2 …
```

They travel as `JAVA_TOOL_OPTIONS`. `World-Generator`'s
`-XX:ActiveProcessorCount=4` travels the same way.

**One thing did not come with it.** `Global-Server` sets the *program*
arguments `--world-dir ./worlds/`, and `JAVA_TOOL_OPTIONS` carries JVM options
only. That one needs no fix: it is a layout choice, and a Spawnery-managed
Paper writes its worlds under `/data` because that is its working directory.
A network crossing over changes the layout rather than the flag.

### A tree that is not `plugins/`

**Closed by a claim source on `spec.mounts`.** See [`mounts.md`](mounts.md).

`extraPlugins` copies its volume into `plugins/`, and `spec.mounts` took
ConfigMaps and Secrets and nothing else — `api/v1alpha1/common_types.go` said
so in as many words: *"V1 supports ConfigMaps and Secrets only; the layered
template system is a later project."* Everything a template holds outside
`plugins/` therefore had nowhere to land:

```
worlds/     4 templates, all from base/asset/maps
            Global/Game, Hub/default, Games/Ragemode,
            OneBlockIslands/default
resources/  7 asset repositories, in Global/Global — reaches every
            backend and the proxy
addons/     4 game templates, the Arcadia addon jars
            (plugins/addons/config.yml is a different thing and did land)
```

**Not one of the fourteen tasks ran while this was open**: the Hub needs a
world, every backend and the proxy need `resources/`, every game needs its
addon jar. Each of those is now a claim at a path.

Three things came with it rather than out of it:

- **`ProxyGroup` gained `spec.mounts` too.** It had none, which was never a
  decision anybody took — and this network's proxies read the same
  `Global/Global` assets its backends do, out of a template that targets both
  `MINECRAFT_SERVER` and `VELOCITY`.
- **`subPath`**, because `bukkit.yml`, `spigot.yml` and `purpur.yml` are single
  files at the server root. A mount without it puts a *directory* at that path.
- **`server.properties` and `config/paper-*.yml` still belong in
  `configOverlay`**, not in a mount. They are rendered, and a mount would
  replace the rendering rather than merge with it.

It is still not a layered template system: no composition, no priority, no
per-server rendering. Flattening the seven templates into one tree per group is
assembly, and belongs outside the operator.

### A volume shared between groups

**Closed by the same field**, through `writable` on the claim.

Six tasks bind a Docker volume `world-pool:/world-pool` — `World-Generator`
fills it, and `Bingo-Solo`, `Bingo-Team`, `All-Items-SMP`, `Challenge` and
`Manhunt` read from it. A generated world is handed from one group to another
through the filesystem. `extraPlugins` could not express it: one claim,
read-only, and it lands in `plugins/`.

A claim mount can, and the writable half is opt-in so that the zero value is
the safe one. **Nothing coordinates two writers**, and nothing here pretends
to: two groups writing one claim get what two processes writing one filesystem
get.

What this costs an installation is real and is not new: a `ReadWriteMany`
storage class, and on this project's own cluster that means Longhorn's
share-manager on the failure path of every start —
[`plugins.md`](plugins.md#what-longhorns-rwx-adds-to-the-failure-surface) has
what that adds.

### Purpur, not Paper

**Closed by an image.** `ghcr.io/spawnery/purpur` is published from 0.2.15, and
`ghcr.io/spawnery/paper` is deprecated — still built, still tested, still
published, and going nowhere until a release note says so. See
[`upgrading.md`](upgrading.md#0215-purpur-is-the-backend-image-and-paper-is-deprecated).

`Platform/Purpur` is 165 files, 159 of them under `libraries/`, around a
`purpur.jar` descriptor. The image is not just a server jar — it carries
`spawnery-config`, the `spawnery-slp` binary the readiness probe execs, and the
agent — so "point `spec.image` at Purpur" was the answer somebody would reach
for and it did not work.

It turned out to be a smaller derivation than it looked, and every step of that
was measured on 2026-08-31 rather than assumed:

- **Same paperclip.** Purpur's jar carries
  `META-INF/license/paperclip-LICENSE.txt` and a `META-INF/download-context` in
  Paper's format, so `nix/paper.nix`'s build-time patching applies unchanged.
- **Same Mojang jar.** Purpur 26.2 build 2628 names exactly the object
  `nix/paper.nix` already pins, `823e2250d24b3ddac457a60c92a6a941943fcd6a`, so
  `nix/purpur.nix` takes it as an argument rather than pinning a second copy.
  That is safe rather than convenient: paperclip verifies the cached original
  against its own `download-context` before patching, so a pair that ever
  drifts fails the build instead of patching against the wrong original — and
  `hack/purpur-pin.sh` refuses before that, with both URLs in the message.
- **Same Java runtime.** `nix/paper-jre.nix`'s module list is a `jdeps`
  measurement, so it was re-derived over Purpur's own classpath — 109 jars
  against Paper's 105, empty stderr — and came out as Paper's thirteen exactly.
  `jdk.zipfs` is the fourteenth for the reason it always was.
- **Same entrypoint.** `image/entrypoint.sh` now takes its jar from
  `SPAWNERY_SERVER_JAR`, defaulting to what Paper always used. Two lines, and
  the Paper image renders identically.
- **Same test.** `hack/image-test.sh` runs against it unchanged and passes:
  the server answered a list ping after 14s, nothing was downloaded at start,
  the forwarding secret was read, the agent plugin loaded and its classes
  linked, and it shut down cleanly on `SIGTERM`.

One difference is real and is not papered over: PaperMC's API publishes a
SHA-256 for its launcher and Purpur's publishes an MD5, so
`hack/purpur-pin.sh` verifies its first download against a weaker digest and
computes the SHA-256 itself. What freezes the input either way is the hash in
the nix file. `hack/purpur-pin.sh`'s own header carries what that bound is
worth.

`purpur.yml` needs no rendering: it is a [mount](mounts.md) like any other
file, which is what the gap above bought.

### The rest of the config files

**Closed by `paper-world-defaults.yml` on `configOverlay`, and by a refusal.**

The gap above assumed `configOverlay` took care of everything under
`config/`. It does not: it took `server.properties` and `paper-global.yml`
only (`internal/render/paper.go`). The network's own files, counted:

```
server.properties           4 × configOverlay
config/paper-global.yml     2 × configOverlay
config/paper-world-defaults.yml   2 ×  — had no route at all
bukkit.yml 5 ×, spigot.yml 2 ×, purpur.yml 2 ×, permissions.yml 1 ×
                                     mounts, with subPath
config/sponge/sponge.conf   1 ×  — see below
```

`paper-world-defaults.yml` is now a `configOverlay` key. Its declared-key tree
comes from Paper's own default file, captured the way the other two were —
and **Purpur's copy of that file is byte-identical to Paper's**, measured on
2026-08-31 by booting both pinned jars against an empty data directory. One
tree serves both images.

The root-level files work as mounts with `subPath`, at the price of one line
per start. Measured against the real Purpur image: a read-only `bukkit.yml`
makes Bukkit log

```
[ERROR]: Could not save bukkit.yml
java.io.FileNotFoundException: bukkit.yml (Read-only file system)
```

and then reach `Done (11.200s)`. The settings are read before the save is
attempted, so the override applies; what it costs is an alarming-looking line
in every log.

**`config/sponge/sponge.conf` has no route, and that is now a refusal rather
than a mystery.** A mount anywhere under `/data/config` stops the server
writing its own configuration — measured in a kind cluster, see
[`mounts.md`](mounts.md#reserved-paths) — so the operator refuses it and says
what to use instead. A third-party plugin that insists on a file there needs
that file in an image, or the plugin's own config moved out of `config/`.
Design spec 4.3 used exactly this path as its example of a legitimate mount;
it was never one.

## What already lines up

Worth knowing before the list above reads as a wall.

**The versions match, without anybody having arranged it.** Purpur `26.2`
against `nix/paper.nix`'s `paperVersion = "26.2"`, and the proxies are the same
build on both sides: Velocity `3.5.1-615`.

**It is small.** The whole configs repository is 8.1 MB. The largest template
is `Platform/Purpur` at 2.0 MB, which becomes the image; after that come
`Global/Server` and `Global/Proxy` at 1.9 MB each, of which LuckPerms'
translation repository is 1.8 MB in both. A per-group volume costs nothing
worth planning around.

**The lifecycle maps cleanly.** Three tasks are `staticServices: true`
(`All-Items-SMP`, `Build`, `OneBlockIslands`) and become Persistent groups; the
`Proxy` task with `minServiceCount: 2` becomes a `ProxyGroup` with
`replicas: 2`; the other ten become Ephemeral groups. Nothing in that column
needed a new field.

## Not on this list

**Anything a tool can do.** Flattening templates by priority, resolving
artifact descriptors, substituting `{{ SECRET_* }}`, deciding which group a
rebuilt jar belongs to — that is assembly, and it belongs outside the operator.
A gap only lands here when there is no place in the cluster to put the result.

**Anything the network can decide for itself.** Whether to keep Purpur, where
worlds should live under `/data`, whether a game still wants to be six groups.
