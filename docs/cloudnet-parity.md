# Bringing a CloudNET network across

What a running CloudNET network needs from Spawnery that Spawnery does not
have. It is a list of gaps, kept so that none of them is rediscovered one at a
time during a migration, and it is deleted when it is empty.

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
| [Purpur, not Paper](#purpur-not-paper) | every backend | open |

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

`Platform/Purpur` is 165 files, 159 of them under `libraries/`, around a
`purpur.jar` descriptor: the network is Purpur, and Spawnery's backend image is
Paper. The image is not just a server jar — it carries `spawnery-config`, the
`spawnery-slp` binary the readiness probe execs, and the agent — so "point
`spec.image` at Purpur" is the answer somebody will reach for and it does not
work.

**Whether Purpur is still wanted after the move is the network's decision, not
this file's.** What follows is what the work would be, measured on 2026-08-31
so that the decision is the only thing left to make.

It is a smaller derivation than it looks. Purpur ships the same paperclip
bootstrap Paper does — `META-INF/license/paperclip-LICENSE.txt`, a
`META-INF/download-context` in the identical format — so `nix/paper.nix`'s
build-time patching applies unchanged. And Purpur 26.2 build 2628 asks for
**exactly the Mojang jar `nix/paper.nix` already pins**: same URL, same object
`823e2250d24b3ddac457a60c92a6a941943fcd6a`. That half is shared, not
duplicated.

```
purpur 26.2 build 2628
  sha256  75b9c49ffd09f26180fb4ab285d840da806f79b347f2fe2256ade2691da15492
  md5     9298c8a949c7a1c6166e1de8e0f26427   (from api.purpurmc.org)
```

One difference from Paper worth knowing before it is a surprise: PaperMC's API
publishes a SHA-256 for the launcher and Purpur's publishes an MD5. Both come
from the host that serves the artifact, so neither is what freezes the input —
the hash checked into nix is — but a pin script for Purpur would verify its
first download against a weaker digest than `hack/paper-pin.sh` does.

What an added image touches, none of it hard and all of it permanent:
`nix/purpur.nix` and `nix/purpur-image.nix`, one flake output, one line in
`hack/publish.sh`'s ordered list, a `hack/purpur-pin.sh`, an image test, an
entry in `hack/image-derivations-changed.sh`, and CI time on every run
thereafter. Nothing in `internal/` changes: `spawnery-config --flavor paper` is
already right for Purpur, which reads Paper's own configuration files, and
`purpur.yml` is a [mount](mounts.md) like any other file.

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
