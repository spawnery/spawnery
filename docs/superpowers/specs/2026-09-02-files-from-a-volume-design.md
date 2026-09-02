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
but it costs one field and no branch in the entrypoint.

### 3.2 Mounted outside `/data`, at `/var/run/spawnery/files`

Read-only, like every mount this package renders, and outside `DataMountPath`
for the reason `PluginSourceMountPath` already carries: the claim cannot *be*
the directory it fills, because a read-only mount at `/data` breaks everything
the server writes. It is a source to copy out of.

The path joins the collision checks that already guard
`/var/run/spawnery/plugins`, so a user mount cannot shadow it.

### 3.3 Refused paths, and why refusal rather than precedence

The entrypoint scans the source tree **before copying anything** and exits
non-zero if it carries a path that belongs to another mechanism:

| Path in the claim | Match | Owned by |
|---|---|---|
| `plugins/` | prefix — the directory and everything under it | `extraPlugins` |
| `server.properties` | exact | the renderer, Paper flavour |
| `config/paper-global.yml` | exact | the renderer, Paper flavour |
| `config/paper-world-defaults.yml` | exact | the renderer, Paper flavour |
| `velocity.toml` | exact | the renderer, Velocity flavour |

The renderer's entries are **the running flavour's own** — `render.PaperFiles`
or `render.VelocityFiles`, read from the same lists the renderer refuses
overlay keys against, so the two can never drift. A Paper server does not
refuse `velocity.toml`: nothing writes it there, and refusing a file no
mechanism owns would be a rule with no reason behind it.

The message names the file **and** the mechanism that owns it, and goes to the
container log.

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

The copy goes immediately before the plugin copy, so the sequence reads
renderer → files → plugins.

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
- each refused path refuses on its own, and the message names the owning
  mechanism: `plugins/` and the three Paper files on a Paper server,
  `plugins/` and `velocity.toml` on a proxy
- a Paper server does **not** refuse `velocity.toml`, and a proxy does not
  refuse the Paper files — the list follows the flavour
- `lost+found` is skipped by name, as the plugin copy already does
- the volume is rendered read-only, on both group kinds
- a user mount at `/var/run/spawnery/files` is refused

The case that started this — a file under `config/` reaching a running server
— is what the first test asserts, with `config/sponge/sponge.conf` as the
path, because a nested target is the whole point.

## 6. Open

**Whether the proxy flavour should refuse more.** Velocity writes `lang/` and
migrates it on every start; a claim carrying `lang/messages.properties` would
be overwritten by Velocity rather than the other way round, which is confusing
but breaks nothing. Left out of the refusal list until somebody meets it.
