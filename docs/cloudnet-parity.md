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
| [A tree that is not `plugins/`](#a-tree-that-is-not-plugins) | every task | open |
| [A volume shared between groups](#a-volume-shared-between-groups) | 6 tasks | open |
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

`extraPlugins` copies its volume into `plugins/`. `spec.mounts` takes
ConfigMaps and Secrets and nothing else — `api/v1alpha1/common_types.go` says
so in as many words: *"V1 supports ConfigMaps and Secrets only; the layered
template system is a later project."*

Everything a template holds outside `plugins/` therefore has nowhere to land:

```
worlds/     4 templates, all from base/asset/maps
            Global/Game, Hub/default, Games/Ragemode,
            OneBlockIslands/default
resources/  7 asset repositories, in Global/Global — reaches every
            backend and the proxy
addons/     4 game templates, the Arcadia addon jars
            (plugins/addons/config.yml is a different thing and does land)
```

`server.properties`, `config/paper-global.yml` and `config/paper-world-defaults.yml`
are fine: they are what `configOverlay` is for. `bukkit.yml`, `spigot.yml` and
`purpur.yml` are not — nothing renders them.

**Nothing runs while this is open.** Not one of the fourteen tasks: the Hub
needs a world, every backend and the proxy need `resources/`, every game needs
its addon jar.

Two coherent ways out, and the choice is a design decision rather than a
technical one:

- **Put it in the image.** Purpur, worlds, `resources/` and `addons/` baked in;
  `extraPlugins` carries only the jars somebody is actually iterating on. It
  fits how this network already runs — its tasks are `runtime: docker-jvm`, so
  images are not foreign — and it fits a development tool, where the image
  changes rarely and the jars change constantly. It costs an image build per
  content change.
- **Let `Mount` take a PVC.** Then volumes carry worlds and assets the way
  CloudNET's templates do. It is a smaller change to the API than it looks and
  a larger one to what an installation has to run: an RWX class, and Longhorn's
  share-manager on the failure path of every start
  ([`plugins.md`](plugins.md) has what that adds).

### A volume shared between groups

Six tasks bind a Docker volume `world-pool:/world-pool` — `World-Generator`
fills it, and `Bingo-Solo`, `Bingo-Team`, `All-Items-SMP`, `Challenge` and
`Manhunt` read from it. A generated world is handed from one group to another
through the filesystem.

Spawnery's only PVC mount is `extraPlugins`, which is read-only and lands in
`plugins/`. There is no way to express a writable volume shared between two
groups. This is the same gap as the one above seen from the other side, and a
`Mount` that took a PVC would close both — but only if it can be writable for
one group and readable for the others, which `extraPlugins` deliberately is
not.

### Purpur, not Paper

`Platform/Purpur` is 165 files, 159 of them under `libraries/`, around a
`purpur.jar` descriptor: the network is Purpur, and Spawnery's backend image is
Paper. The image is not just a server
jar: it carries `spawnery-config`, the `spawnery-slp` binary the readiness
probe execs, and the agent. So this is a Purpur variant of the image rather
than a `spec.image` override — `nix/paper.nix` is where it would fork.

Whether Purpur is still wanted after the move is a question for the network,
not for this file. It is listed because "point `spec.image` at Purpur" is the
answer somebody will reach for, and it does not work.

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
