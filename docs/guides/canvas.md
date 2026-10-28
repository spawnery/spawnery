# Canvas servers

[Canvas](https://canvasmc.io) is a fork of Folia: one Minecraft server whose
worlds are cut into regions that tick on several threads. Spawnery publishes
it as `ghcr.io/spawnery/canvas`, beside the Purpur image. A group uses it by
naming the image. The pod contract, the agent, the join permission and world
sync are the same as on Purpur. What follows was measured on the development
VM on 2026-10-26 and 2026-10-27 with Canvas 26.3 build 991, or read from
Canvas's committed defaults and source.

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: survival
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: Persistent
  image: ghcr.io/spawnery/canvas:26.3-0.32.0
  maxPlayers: 100
  configOverlay:
    name: survival-config
```

The tag is the Minecraft version and spawnery's image version, as for Purpur.
The operator does not know which of the two images a group runs.

## Plugins must declare Folia support

Canvas loads a plugin only if its `plugin.yml` or `paper-plugin.yml` says
`folia-supported: true`. It skips every other plugin at startup and logs one
line for each:

```
Could not load plugin 'Example' as it is not marked as supporting Folia!
```

The flag is the plugin author's statement that the plugin schedules through
the region schedulers. A plugin written for Paper
and Purpur usually makes no such statement and does not run on Canvas. The
spawnery agent declares it, and so can a plugin of yours that uses
`GlobalRegionScheduler`, `RegionScheduler` and `AsyncScheduler` instead of
`BukkitScheduler`. The [plugin API](../plugin-api/index.md) is unchanged.

## Configuration

The image sets `SPAWNERY_CONFIG_FLAVOR=canvas`. `spawnery-config` then renders
the same files as for Purpur and also declares Canvas's own keys, which
matters for `spec.configOverlay` in two places.

`paper-global.yml` accepts a `threaded-regions` section, which Canvas adds to
Paper's file:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: survival-config
  namespace: minecraft
data:
  paper-global.yml: |
    threaded-regions:
      threads: 8
      grid-exponent: 4
      scheduler: AFFINITY
```

`grid-exponent: 4` and `scheduler: AFFINITY` are Canvas's defaults. The
default for `threads` is `-1`, which leaves the count to Canvas.
`paper-world-defaults.yml` takes Canvas's own keys too. A Purpur group that
sets any of these is refused, like any key its server does not declare.

Canvas writes two more files on its first start, `config/canvas-server.yml`
and `config/canvas-worlds.yml`. The renderer does not write them, so an
overlay cannot reach them. They come in through
[`extraFiles`](plugins-from-a-volume.md#files-from-a-volume) like any other
file, at those paths in the claim or image.

## What the tick numbers mean

A Canvas server has no single tick. `/cloud info` and the metrics report the
worst live region: the lowest TPS and the highest MSPT among the regions of
all worlds. One region at 8 TPS shows as 8 TPS however many idle regions
surround it, and that is the figure its players feel. The windows are Paper's,
so the numbers compare with a Purpur server's: TPS over one minute, capped at
20, and MSPT over five seconds. With no live region, on an empty server, the
global tick's figures stand in, so the report is not zero.

## Commands

`/cloud execute` runs a command the way Canvas's console does, on the global
region thread. A command that needs a position's region is refused there:
`setblock` and `execute if block` answer "That position is not loaded" or
throw. The outcome still comes back with `ok: true` and Canvas's refusal in its
output, because `ok` means the command is known, as on Purpur. `say` and
`list` work. The agent does not route a command to a region, since a command
line names no position.

## World sync

[Worlds in an object store](object-store-worlds.md) work on Canvas. A snapshot
waits for every region to save before the world is flushed, so it holds every
chunk that was dirty when the snapshot was asked for.

Global data (scoreboards, maps, command storage) has its own autosave on
Canvas. The snapshot's autosave freeze does not stop it, so those files can
change while a snapshot is copied.

## What is not there

- No startup cache. `--aot-cache` is Purpur's: a Canvas group starts as it does
  with the flag off, and the `purpur-aot` images are not pulled for it.

The login gate works as on Purpur. A group that enforces playable slots or a
[join permission](join-permission.md) refuses and admits players on Canvas.
