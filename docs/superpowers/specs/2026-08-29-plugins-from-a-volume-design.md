# Plugins from a volume

**Status:** design, awaiting review
**Date:** 2026-08-29

## 1. What this is for

Today a plugin reaches a Spawnery server exactly one way: it is built into the
image by Nix, and `image/entrypoint.sh` copies it out of the read-only part of
the image into `/data/plugins` on every start. That is how
`spawnery-agent.jar` gets there, and it is the only mechanism there is.

**So adding LuckPerms means rebuilding and republishing two images, and
changing a plugin's configuration means doing it again.** For the agent that is
correct — the agent belongs to the operator's own release and the image *should*
be the truth. For a third-party plugin an administrator chose, it is the wrong
shape: the plugin has nothing to do with the operator's version, and coupling
the two means a permission tweak rides on an image publish.

This design adds a second source: a volume an administrator fills, whose
contents are copied into `/data/plugins` on every start alongside the agent
jar.

**It deliberately does not put any third-party plugin into the images.** The
images stay exactly what they are.

## 2. What was measured first

Recorded because each one closed a question, and because re-measuring is
cheaper than rediscovering why.

**`/data` is an `emptyDir` for an ephemeral group.** `internal/podspec/server.go`
gives the server's working directory an `emptyDir` unless the group is
persistent. Anything a plugin writes there is gone on the next pod. That is
what makes "the source wins on every start" the only coherent rule rather than
one option among several — there is nothing to preserve.

**Every user mount is read-only, unconditionally**, and `/data/plugins` is the
one path a user mount may not take. `internal/podspec/server.go:53` carries the
measurement and the reason: the entrypoint copies the agent jar into that
directory, so a read-only mount there fails the copy under `set -eu` with a
bare `cp:` that names no cause — and Paper writes its plugins' data folders
inside it, so pointing `--plugins` at a read-only path takes Paper's own
bundled plugins down too.

**Both entrypoints already do the copy, identically.**
`image/entrypoint.sh:47` and `image/velocity-entrypoint.sh:25` each `mkdir -p
plugins` and `cp -f` the agent jar in. The mechanism this design needs is the
one already there, extended.

**A ConfigMap or Secret cannot carry a plugin.** Both cap at 1 MiB; LuckPerms
is about 10 MB. That is why `Mount`'s two existing sources do not solve this
and a volume is needed at all.

**`configOverlay` cannot carry plugin configuration.** It reads exactly three
keys — `server.properties`, `paper-global.yml`, `velocity.toml`
(`internal/render/paper.go:88,113`, `velocity.go:75`) — and silently ignores
anything else. A key like `plugins/LuckPerms/config.yml` would be accepted by
the API and do nothing.

**Longhorn on this cluster is ready for `ReadWriteMany`.** All three nodes
report `NFSClientInstalled`, `RequiredPackages`, `KernelModulesLoaded`,
`MountPropagation` and `Multipathd` as `True`. There is no RWX volume on
Longhorn today, and *that absence is not a measurement* — an earlier draft of
this design read it as one and chose the NFS CSI classes instead, which are
reserved on this cluster for bulk storage.

## 3. The shape

### 3.1 One new field, not a new mount source

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
spec:
  extraPlugins:
    claimName: minecraft-plugins
```

The same field on `ProxyGroup`. The operator mounts that claim **read-only** at
a fixed path outside `/data`, and each entrypoint copies its whole tree into
`/data/plugins` before copying the agent jar.

**A dedicated field rather than a `persistentVolumeClaim` source on `mounts`**,
and the reason is the entrypoint. A user-chosen `mountPath` would have to reach
the entrypoint somehow — an environment variable it reads, which is a second
thing to keep in step with a constant, and the kind of coupling this repository
has been bitten by before. A fixed path known to both sides needs no message.

It is also not the same *kind* of thing. `mounts` is documented as raw files
for plugins and worlds, mounted where the user says and left alone. This is a
source the operator copies from, with its own precedence rule. Two behaviours
under one field would need a convention to tell them apart, and a name-based
convention is invisible until somebody picks that name by accident.

### 3.2 The whole tree, and the source wins

Everything under the volume is copied, not only `*.jar`: a plugin's
configuration lives at `plugins/<Name>/config.yml`, and a mechanism that
carried jars but not their configuration would leave every plugin at its
defaults on an ephemeral group.

**The source wins on every start.** A plugin that rewrites its own
configuration at runtime loses that change at the next start. On an ephemeral
group it would lose it anyway — `/data` is an `emptyDir` — so this rule costs
nothing there and makes the persistent case predictable instead of
accumulating. What is on the volume is what runs.

### 3.3 The agent jar wins over the volume

The copy order is: the volume's tree first, then the agent jar over it.

**This is a bound and not a detail.** Without it, a `spawnery-agent.jar` in the
volume — placed by mistake, or by somebody pinning an older agent — would
silently replace the one the operator shipped, and the operator would then be
talking to an agent whose version it never published. The failure would surface
as a version skew nobody could explain from the object model, because every
object would say the right thing.

### 3.4 `ReadWriteMany`, and RWO is refused

A group's servers are spread across nodes. A `ReadWriteOnce` claim mounts on
one node only, so the second server would sit `Pending` with a scheduling error
that names volume affinity and not the actual cause.

**The operator reads the claim's `accessModes` and refuses one without
`ReadWriteMany`**, setting `Accepted=False` with a reason and recording an
event that says which claim and what is wrong.

**A reviewer might disagree here, and this is the place to do it.** A group with
one replica works perfectly well on a `ReadWriteOnce` claim, and this rule
refuses it anyway. The simpler rule was chosen deliberately: `maxReplicas` can
be raised at any time by an edit that has nothing to do with storage, and a
group that worked until somebody scaled it is a worse failure than one that
never started. But it is a real cost and it is not hidden in a footnote.

### 3.5 An operator switch, and what it is not

`--allow-plugin-volumes`, default false. A group naming `extraPlugins` on an
installation that has not enabled it is refused the same way an RWO claim is.

**It is an operational switch and not a security boundary, and the code must
say so.** An earlier draft of this design proposed a `hostPath` source, where a
switch would have been a real bound: whoever could write a `ServerGroup` could
have read any node-local path into a container. With a `PersistentVolumeClaim`
that argument evaporates — a claim is a namespaced object in the same trust
domain as the group that names it, and anybody who can write one can write the
other.

What the switch is actually for: an operator saying "this installation runs no
third-party plugins", and having that be true rather than a convention. That is
a legitimate thing to want and a different claim from the one that motivated
it. Documenting it as a security control would be the kind of check that reads
like a bound and cannot fail — worse than no check, because the next reader
would trust it.

### 3.6 Nothing about the volume reaches the pod hash

The operator cannot read the volume's contents — it holds a claim name, not a
filesystem — so nothing about what is on it can enter `podspec.DesiredServerHash`.

**Two consequences, and both are wanted.** Changing a plugin does not roll a
fleet, which is the whole point: no image rebuild, no republish, no changeover.
And a change therefore does not take effect until the group restarts, which the
administrator triggers. A mechanism that watched the volume and rolled on every
write would turn saving a config file into a fleet-wide changeover.

Adding or removing the `extraPlugins` field itself *does* move the hash, because
it changes the rendered pod. That is correct: the pod really is different.

### 3.7 What Longhorn RWX adds to the failure surface

Longhorn serves a `ReadWriteMany` volume through a `share-manager` pod that
exports NFS, which every consuming node then mounts. **That is one more moving
part between the volume and a starting server than an RWO volume has.** If the
share-manager is down or being rescheduled, the mount hangs and the server does
not start — and the pod's events name an NFS mount, not a plugin volume.

This is stated rather than mitigated. It is Longhorn's architecture and not
something this design can change; what it can do is make sure the person
reading `docs/` has met the sentence before they meet the symptom.

## 4. What this does not do

- **No third-party plugin in any image.** The images are unchanged.
- **No plugin lifecycle.** Nothing installs, updates, resolves dependencies for
  or version-checks a plugin. The volume's contents are copied verbatim.
- **No per-server plugins.** The volume belongs to a group; every server in it
  gets the same tree.
- **No write-back.** The copy is one-way on every start. A plugin's runtime
  state is not preserved and section 3.2 says why that is the coherent rule.
- **No hostPath.** It was the first shape considered and it is not in this
  design: it would have required the same path on all three nodes with nothing
  keeping them in step, or pinning groups to one node and giving up the spread
  that the PodDisruptionBudget and node-drain work exist to protect.

## 5. What proves it

- The copy order — volume first, agent jar second — asserted directly, and
  mutated by reversing it: a volume carrying a `spawnery-agent.jar` must not
  win.
- An RWO claim refused, with the condition and the event asserted separately so
  that a test cannot pass on the wrong one.
- The switch off with `extraPlugins` set: refused, and the refusal names the
  flag rather than the claim.
- The switch on with no `extraPlugins`: nothing changes about the rendered pod,
  so the golden pod digests do not move for installations that do not use this.
- `internal/podspec/hash_golden_test.go` will move once, when the field is
  added to the render path — that is a decision to take and record, as it was
  for `stdin`.
- End to end in `hack/agent-test.sh`: a container started with a directory
  standing in for the volume, asserting a jar placed there is loaded by the
  real image. The harness has no Kubernetes, so it tests the entrypoint's half
  and not the operator's.

## 6. Open

**Whether `ProxyGroup` needs it at all in the first cut.** The stated need is
LuckPerms, which is wanted on both sides; but the proxy's plugin surface is
smaller and doing one side first would halve the change. This design covers
both because the entrypoints are already identical and splitting them now would
be the only asymmetry in the pair.
