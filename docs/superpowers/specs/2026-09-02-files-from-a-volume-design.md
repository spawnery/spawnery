# Files from a volume

**Status:** design, awaiting review
**Date:** 2026-09-02

## 1. What this is for

`extraPlugins` gave an administrator a way to put a plugin and its
configuration on a server without rebuilding an image. It reaches exactly one
directory: `/data/plugins`.

**A plugin whose configuration does not live under `plugins/` therefore cannot
be configured at all.** Sponge is the case that forced this. It reads
`config/sponge/sponge.conf`, and there is no way to put a file there:

- **A mount cannot.** `internal/podspec/server.go` refuses any mount at or
  under `/data/config`, and the comment there carries the measurement from
  2026-08-31: the kubelet creates a mount's parent directory itself, root-owned
  and group-read-only, while `fsGroup` with `OnRootMismatch` only ever touches
  the volume's own root. The container is then unable to write into that
  directory, and the first thing it tries to write is the renderer's own
  `paper-global.yml`. Nothing this operator can do makes that work.
- **An overlay cannot.** `render.Paper` writes three files and
  `checkOverlayFiles` refuses every key that is not one of them. Adding a
  fourth is a code change and a release for each file anybody ever needs.

So the answer today is "put it in an image", which is the shape `extraPlugins`
already exists to avoid.

This design adds `extraFiles`: a volume an administrator fills, whose tree is
copied into `/data` on every start. It is `extraPlugins` one directory up, and
it is deliberately the same mechanism rather than a new one — the same volume
shape, the same "the source wins on every start", the same copy in the
entrypoint.

## 2. What was measured first

**The renderer runs before the plugin copy.** `image/entrypoint.sh` calls
`spawnery-config --flavor paper` at line 45 and copies out of
`PLUGIN_SOURCE` at lines 72-95. Anything copied after the renderer overwrites
it. That ordering is what makes section 3.3 a safety property rather than a
preference.

**`/data` is an `emptyDir` for an ephemeral group**, recorded in the
`extraPlugins` design and unchanged. There is nothing to preserve across a
pod, which is what makes "the source wins" coherent for the ephemeral case and
a deliberate trade for the persistent one.

**Sponge rewrites its own configuration.** `sponge.conf` carries
`# Active configuration version ... will be updated automatically`, so the file
is one the server writes back. A read-only delivery would be wrong for it even
if the kubelet allowed one: the server would fail on its own write rather than
on the mount. That is the second, independent reason this is a copy.

## 3. The shape

### 3.1 One new field, on both group kinds

```yaml
spec:
  extraFiles:
    claimName: <pvc>
```

`ExtraFiles` mirrors `ExtraPlugins`: a claim name, nothing else. It goes on
`ServerGroup` and `ProxyGroup` both, because `ExtraPlugins` is on both and a
mechanism that only half the objects have is one more rule to remember. The
proxy flavour has less to gain — Velocity keeps no configuration directory —
but the cost is one field and a second copy of the same block.

There is no flavour branch to write: `image/entrypoint.sh` and
`image/velocity-entrypoint.sh` are separate scripts that already know which
flavour they are (`spawnery-config --flavor paper` against `--flavor
velocity`), and the plugin copy is already duplicated between them. Each gets
its own refusal list, which is why section 3.3's list can differ by flavour
without anything having to decide at runtime.

### 3.2 Mounted outside `/data`, at `/var/run/spawnery/files`

Read-only, like every mount this package renders, and outside `DataMountPath`
for the reason `PluginSourceMountPath` already carries: the claim cannot *be*
the directory it fills, because a read-only mount at `/data` breaks everything
the server writes. It is a source to copy out of.

The path joins the collision checks that already guard
`/var/run/spawnery/plugins`, so a user mount cannot shadow it.

### 3.3 Refused paths, and why refusal rather than precedence

The entrypoint scans the source tree **before copying anything** and exits
non-zero if it carries a path that already has an owner:

| Path in the claim | Match | Owned by |
|---|---|---|
| `plugins/` | prefix — the directory and everything under it | `extraPlugins` |
| `server.properties` | exact | the renderer, Paper flavour |
| `config/paper-global.yml` | exact | the renderer, Paper flavour |
| `config/paper-world-defaults.yml` | exact | the renderer, Paper flavour |
| `velocity.toml` | exact | the renderer, Velocity flavour |
| `lang/` | prefix | Velocity itself, Velocity flavour |

Three kinds of owner, and the third is the one worth spelling out. `plugins/`
belongs to another mechanism and the flavour files to the renderer, but
**`lang/` belongs to the server program**: Velocity migrates
`lang/messages.properties` to MiniMessage on every start and writes the result
back, so a file placed there is overwritten before anybody reads it. Nothing
breaks — which is precisely why it is refused. A copy that silently does
nothing is the failure this design exists to prevent, and it is worse here than
a collision that announces itself.

The renderer's entries are **the running flavour's own** — `render.PaperFiles`
or `render.VelocityFiles`, read from the same lists the renderer refuses
overlay keys against, so the two can never drift. A Paper server does not
refuse `velocity.toml` or `lang/`: nothing writes them there, and refusing a
path no owner claims would be a rule with no reason behind it.

The message names the path **and** its owner, and goes to the container log.

Refusal rather than an order that quietly decides: a claim carrying
`config/paper-global.yml` is somebody expecting to configure the Velocity
block, and the two honest answers are "this file is the operator's" and
"your file is now the truth". Silently keeping one of the two is the failure
mode this whole area of the design exists to prevent — the same reasoning
`checkOverlayFiles` already applies to an unknown overlay key.

**The cost is that the refusal arrives at start, not at admission.** A claim's
contents are not knowable when somebody writes the group, so a wrong file
crash-loops the pod rather than failing the write. `extraPlugins` has the same
property. What makes it liveable is that the message says what is wrong in one
sentence, in the log an operator already reads when a group does not come up.

### 3.4 The source wins on every start

Word for word the `extraPlugins` rule, and for the same reason: a server that
rewrote a file finds the administrator's version again next start, which is
what makes the claim the truth rather than a first-boot seed.

**A world in the claim is therefore overwritten on every start.** That follows
from the rule rather than qualifying it, and belongs in the field's own
documentation. A world does not belong in this claim; `spec.mounts` and
`spec.storage` are what carry one.

### 3.5 Order between the three writers is irrelevant by construction

The renderer, this copy and the plugin copy all write into `/data`. Section 3.3
guarantees their paths are disjoint, so the order between them cannot change
the result. That is the property to preserve: a design where the order decides
would make the entrypoint's line numbers load-bearing.

The copy goes immediately before the plugin copy in both scripts, so the
sequence reads renderer → files → plugins on either flavour.

### 3.6 Its own operator switch

`--allow-file-volumes`, default false, rendered by the chart as
`operator.allowFileVolumes`. A group naming `extraFiles` on an installation
that has not enabled it is refused exactly the way `extraPlugins` is, with its
own reasons — `FileVolumesDisabled` and `FileVolumeUnusable` — so a group
setting both wrong says which field it means.

**A second flag rather than widening the first.** `--allow-plugin-volumes`
exists so an operator can say "this installation runs no third-party plugins"
and have it be a fact; making it also govern files would leave the flag's name
covering something that is not a plugin. The two statements are different and
an installation may want one without the other.

And, for the same reason the `extraPlugins` design gives: **this is an
operational switch and not a security boundary.** A `PersistentVolumeClaim` is
a namespaced object in the same trust domain as the group naming it, so the
switch stops nobody who was not already stopped. The chart's documentation for
it must say so as plainly as the existing one does.

Worth recording because it was raised and answered: `extraFiles` cannot
undermine what `--allow-plugin-volumes` promises, since section 3.3 refuses
`plugins/` outright. That is an argument for needing no switch at all, and it
loses to symmetry — a mechanism that is `extraPlugins` one directory up should
be governable the same way.

### 3.7 The claim must be `ReadWriteMany`

The rule `extraPlugins` already has, refused by the same helper
(`checkClaimMountable`): every pod of a group mounts it, and a ReadWriteOnce
claim would leave the second server Pending with a scheduling error naming
volume affinity rather than the cause.

## 4. What this does not do

**It does not make `/data/config` mountable.** The kubelet's ownership is
unchanged and the mount refusal stays exactly as it is. This delivers files by
copying them as the container user, which is the only thing that ever worked
there.

**It does not validate what it copies.** A malformed `sponge.conf` is Sponge's
problem, the way a malformed plugin config is the plugin's.

**It does not replace `extraPlugins`.** Plugins keep their own field, their own
volume and their own refusal, and this one refuses their directory.

## 5. What proves it

`image/entrypoint_test.go` and the podspec tests both exist and both grow:

- a nested file from the claim lands at its path under `/data` and is writable
- each refused path refuses on its own, and the message names the owner:
  `plugins/` and the three Paper files on a Paper server, `plugins/`,
  `velocity.toml` and `lang/` on a proxy
- a Paper server does **not** refuse `velocity.toml` or `lang/`, and a proxy
  does not refuse the Paper files — the list follows the flavour
- `lang/` refuses on the directory, not only on `lang/messages.properties`:
  the whole directory is Velocity's
- `lost+found` is skipped by name, as the plugin copy already does
- the volume is rendered read-only, on both group kinds
- a user mount at `/var/run/spawnery/files` is refused
- a group naming `extraFiles` without `--allow-file-volumes` is refused with
  `FileVolumesDisabled`, and the message names the flag
- a claim that is ReadWriteOnce is refused with `FileVolumeUnusable`
- a group setting both `extraPlugins` and `extraFiles` wrong reports the
  plugin one first, the order `checkGroupVolumes` already uses

The case that started this — a file under `config/` reaching a running server
— is what the first test asserts, with `config/sponge/sponge.conf` as the
path, because a nested target is the whole point.

## 6. Open

**Whether the Paper flavour has an equivalent of `lang/`.** Refusing a
directory the server program rewrites raises the question for the other
flavour, and it is not answered here. Paper's own directories under `/data`
that nobody else writes — `cache/`, `versions/`, `libraries/` — behave
differently from each other: `libraries/` is a directory a server is
routinely given content for, while the other two are the image's. Whichever of
them Paper rewrites on start belongs in the table for the same reason `lang/`
does, and finding out means measuring a start rather than reading. Until then
the Paper list stays the renderer's three, which are the ones something
demonstrably overwrites.
