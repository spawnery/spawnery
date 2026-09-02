# Plugins from a volume

An administrator fills a volume with plugin jars and their configuration, and
every server or proxy of a group loads them. **No image is rebuilt, no release
is cut, and no fleet is rolled.**

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: minecraft
spec:
  # ...
  extraPlugins:
    claimName: minecraft-plugins
```

The same field exists on `ProxyGroup`.

Configuration that does not live under `plugins/` — Sponge's
`config/sponge/sponge.conf` is the case that forced the question — is not what
this field is for. [`extraFiles`](mounts.md#what-a-claim-is-for) is the same
mechanism one directory up, for exactly that case.

## Turning it on

The operator refuses a group naming `extraPlugins` unless it was started with
`--allow-plugin-volumes`. The chart passes the operator's arguments through, so
this is a values edit and a restart of one Deployment.

A claim-backed [`spec.mounts`](mounts.md) entry has its own switch,
`--allow-mount-volumes` — until 0.2.x it shared this one, and that flag's name
never promised it. Each claim-consuming field has its own switch now:
`--allow-plugin-volumes` for `extraPlugins`, `--allow-file-volumes` for
`extraFiles`, and `--allow-mount-volumes` for a claim-backed mount.

**None of the three is a security boundary, and nothing here will tell you it
is.** A `PersistentVolumeClaim` is a namespaced object in the same trust domain
as the group that names it: anybody who can write one can write the other, so
the switch stops nobody who was not already stopped. What it is for is an
operator being able to say *this installation runs no third-party plugins* and
have that be a fact rather than a convention.

A group that names a claim on an installation with the switch off is refused
with `Accepted=False`, reason `PluginVolumesDisabled`, and a message naming the
flag — not the claim, because the claim is probably fine.

## The claim must be `ReadWriteMany`

A group's servers are spread across nodes, and every one of them mounts this
volume. A `ReadWriteOnce` claim attaches to one node, so the second server
would sit `Pending` on a scheduling error about volume affinity — with nothing
naming the claim.

The operator refuses it instead: `Accepted=False`, reason
`PluginVolumeUnusable`, and a message carrying the claim's actual access modes.
The same refusal covers a claim that does not exist.

**A single-replica group is refused too, and that is deliberate rather than an
oversight.** `ReadWriteOnce` would work for it today. But `maxReplicas` is
raised by edits that have nothing to do with storage, and a group that worked
until somebody scaled it is a worse failure than one that never started.

On this project's own cluster that means Longhorn:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: minecraft-plugins
  namespace: minecraft
spec:
  accessModes: [ReadWriteMany]
  storageClassName: longhorn
  resources:
    requests:
      storage: 1Gi
```

### What Longhorn's RWX adds to the failure surface

Longhorn serves a `ReadWriteMany` volume through a **`share-manager` pod** that
exports NFS, which every consuming node then mounts. That is one more moving
part between the volume and a starting server than a `ReadWriteOnce` volume
has.

If the share-manager is down or being rescheduled, **the mount hangs and the
server does not start** — and the pod's events name an NFS mount, not a plugin
volume. Nothing here can change that; it is Longhorn's architecture. What this
page can do is put the sentence in front of you before the symptom is.

Longhorn's own requirement for RWX is an NFSv4 client on every node. Its node
objects report it:

```bash
kubectl -n longhorn-system get nodes.longhorn.io -o json |
  jq -r '.items[] | "\(.metadata.name) \(.status.conditions[] |
    select(.type=="NFSClientInstalled") | .status)"'
```

## What lands where

On every start, each entrypoint copies the **whole tree** from the volume into
the server's `plugins/` directory, and then copies the agent jar over it.

Jars and their configuration together, not jars alone. A plugin's configuration
lives at `plugins/<Name>/config.yml`, and a mechanism that carried one without
the other would leave every plugin at its defaults on an ephemeral group —
whose `/data` is an `emptyDir` and keeps nothing.

**The volume wins on every start.** A plugin that rewrites its own
configuration at runtime loses that change when the pod is replaced. On an
ephemeral group it would lose it anyway; on a persistent one this keeps the
volume's contents authoritative instead of letting each server drift.

**The agent jar wins over the volume.** A `spawnery-agent.jar` placed on the
volume is overwritten by the one the image ships. Without that order, somebody
pinning an older agent would leave the operator talking to a version it never
published — and every object in the cluster would say the right thing.

## Changing a plugin

Write to the volume, then restart the group's servers. Deleting the pods is
enough; the group replaces them.

**Nothing rolls on its own, and that is the point.** The operator holds a claim
name, not a filesystem, so nothing about the volume's contents can reach the
pod hash — which is what lets you change a plugin without an image rebuild, a
release, or a changeover. The cost is that saving a file changes nothing until
you say so.

Adding or removing the `extraPlugins` field itself *does* move the pod hash and
roll the group, because the rendered pod really is different.

## What this is not

- **Not a plugin manager.** Nothing installs, updates, resolves dependencies
  for, or version-checks anything. The tree is copied verbatim.
- **Not per-server.** The claim belongs to a group; every server in it gets the
  same tree.
- **Not two-way.** The copy runs one direction on every start.
- **Not in any image.** No third-party plugin ships in a Spawnery image, and
  this mechanism exists so none has to.

## Styling what the agent says

`Network.spec.defaults.feedFormat` sets the shape of every line the agent
writes into chat — both an announcement about the cloud and a reply to a
`/cloud` command. One field rather than two, because they come from the same
plugin and a network that styles one should not have to style the other to
match.

It is [MiniMessage](https://docs.advntr.dev/minimessage/format.html), which
both Paper and Velocity parse. `$EVENT_MESSAGE` is replaced by what the line
has to say; everything around it is yours. The default:

```yaml
spec:
  defaults:
    feedFormat: "<gray>»</gray> <gradient:aqua:green>Spawnery</gradient> <dark_gray>|</dark_gray> <gray>$EVENT_MESSAGE"
```

**Changing it rolls nothing.** The format travels in the network picture the
operator already sends, not in the pod — a pod's environment is part of the
pod hash, so a format carried there would make re-wording a chat line replace
every server on the network. An edit takes effect within a resync interval.

A blank value falls back to that default rather than printing nothing, which is
also what an agent does when talking to an operator too old to send the field.

**Colour is used where it carries meaning, and the format cannot change that
part.** Inside `$EVENT_MESSAGE` a server that takes joins is green and one that
does not is red — that is the question somebody is actually asking, and it
disagrees with the phase during a drain. Warnings are red for the same reason.
