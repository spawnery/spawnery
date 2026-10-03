# Plugins from a volume

An administrator fills a volume with plugin jars and their configuration, and
every server or proxy of a group loads them, without an image rebuild, a
release, or a rolled fleet.

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

Configuration outside `plugins/` (Sponge's `config/sponge/sponge.conf` is the
case that forced the question) belongs in
[`extraFiles`](#files-from-a-volume), the same mechanism one directory up.

## Turning it on

The operator refuses a group naming `extraPlugins` unless it was started with
`--allow-plugin-volumes`. The chart passes the operator's arguments through, so
this is a values edit and a restart of one Deployment.

A claim-backed [`spec.mounts`](mounts-and-files.md) entry has its own switch,
`--allow-mount-volumes`. Until 0.2.x it shared this one, which that flag's name
never promised. Each claim-consuming field has its own switch now:
`--allow-plugin-volumes` for `extraPlugins`, `--allow-file-volumes` for
`extraFiles`, and `--allow-mount-volumes` for a claim-backed mount.

None of the three is a security boundary. A `PersistentVolumeClaim` is a
namespaced object in the same trust domain as the group that names it: anybody
who can write one can write the other, so the switch stops nobody who was not
already stopped. It lets an operator make *this installation runs no
third-party plugins* a fact rather than a convention.

A group that names a claim on an installation with the switch off is refused
with `Accepted=False`, reason `PluginVolumesDisabled`, and a message naming the
flag. It does not name the claim, because the claim is probably fine.

## The claim must be `ReadWriteMany`

A group's servers are spread across nodes, and every one of them mounts this
volume. A `ReadWriteOnce` claim attaches to one node, so the second server
would sit `Pending` on a scheduling error about volume affinity, with nothing
naming the claim.

The operator refuses it instead: `Accepted=False`, reason
`PluginVolumeUnusable`, and a message carrying the claim's actual access modes.
The same refusal covers a claim that does not exist.

A single-replica group is refused too, on purpose. `ReadWriteOnce` would
work for it today, but `maxReplicas` is raised by edits that have nothing to
do with storage, and a group that worked until somebody scaled it is a worse
failure than one that never started.

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

Longhorn serves a `ReadWriteMany` volume through a `share-manager` pod that
exports NFS, which every consuming node then mounts. That is one more moving
part between the volume and a starting server than a `ReadWriteOnce` volume
has.

If the share-manager is down or being rescheduled, the mount hangs and the
server does not start, and the pod's events name an NFS mount, not a plugin
volume. This is Longhorn's architecture and nothing in Spawnery can change it.

Longhorn's own requirement for RWX is an NFSv4 client on every node. Its node
objects report it:

```bash
kubectl -n longhorn-system get nodes.longhorn.io -o json |
  jq -r '.items[] | "\(.metadata.name) \(.status.conditions[] |
    select(.type=="NFSClientInstalled") | .status)"'
```

## What lands where

On every start, each entrypoint copies the whole tree from the volume into
the server's `plugins/` directory, and then copies the agent jar over it.

The tree holds jars and their configuration together. A plugin's configuration
lives at `plugins/<Name>/config.yml`, and a mechanism that carried one without
the other would leave every plugin at its defaults on an ephemeral group,
whose `/data` is an `emptyDir` and keeps nothing.

The volume wins on every start. A plugin that rewrites its own
configuration at runtime loses that change when the pod is replaced. On an
ephemeral group it would lose it anyway; on a persistent one this keeps the
volume's contents authoritative instead of letting each server drift.

The agent jar wins over the volume. A `spawnery-agent.jar` placed on the
volume is overwritten by the one the image ships. Without that order, somebody
pinning an older agent would leave the operator talking to a version it never
published, while every object in the cluster would say the right thing.

## Changing a plugin

Write to the volume, then restart the group's servers. Deleting the pods is
enough; the group replaces them.

Nothing rolls on its own. The operator holds a claim name, not a filesystem,
so nothing about the volume's contents can reach the pod hash. That is what
lets you change a plugin without an image rebuild, a release, or a changeover;
the cost is that saving a file changes nothing until you say so.

Adding or removing the `extraPlugins` field itself *does* move the pod hash and
roll the group, because the rendered pod really is different.

## What this is not

It is not a plugin manager: nothing installs, updates, resolves dependencies
for, or version-checks anything, and the tree is copied verbatim. It is not
per-server, since the claim belongs to a group and every server in it gets the
same tree. The copy runs one direction on every start. No third-party plugin
ships in a Spawnery image, and this mechanism exists so none has to.

## Files from a volume

`extraFiles` is the same mechanism one directory up. Its claim is copied into
the server's whole working directory rather than into `plugins/`. That is
where a file that is not a plugin and that no mount can reach belongs, such as
the `config/sponge/sponge.conf` Sponge reads.

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: minecraft
spec:
  # ...
  extraFiles:
    claimName: minecraft-files
```

The same field exists on `ProxyGroup`, and the claim carries the same
`ReadWriteMany` requirement as an `extraPlugins` claim, refused the same way and
for the same reason, with `FileVolumeUnusable` rather than
`PluginVolumeUnusable` so that the message sends somebody to the field they
actually wrote.

It needs its own switch, `--allow-file-volumes`. In the chart that is `operator.allowFileVolumes`,
default `false`:

```yaml
operator:
  allowFileVolumes: true
```

A group naming a claim on an installation with the switch off is refused with
`Accepted=False`, reason `FileVolumesDisabled`, and a message naming the flag.

The volume wins on every start, as it does for `extraPlugins`: a file the
server rewrote at runtime is replaced by the claim's version the next time the
pod starts. A world therefore does not belong in this claim, because it would
be overwritten on every start. `spec.storage` and a claim-backed
[`spec.mounts`](mounts-and-files.md) entry carry one.

Nothing about the contents reaches the pod hash. Writing to the volume rolls
nothing; the files reach a server on its next start, which somebody triggers by
deleting the group's pods. Adding or removing the field itself *does* move the
hash, because the rendered pod really is different.

### Paths a claim may not carry

Three things write into a server's working directory on a start: the operator's
renderer, this copy, and the `extraPlugins` copy. Instead of letting the copy
order decide who wins, each entrypoint scans the claim before copying anything
and refuses to start if it carries a path one of the others owns.

| Path in the claim | Matches | Owned by | Refused on |
|---|---|---|---|
| `plugins` | the name and everything under it | `extraPlugins` | Paper and Velocity |
| `server.properties` | that path exactly | the renderer | Paper only |
| `config/paper-global.yml` | that path exactly | the renderer | Paper only |
| `config/paper-world-defaults.yml` | that path exactly | the renderer | Paper only |
| `velocity.toml` | that path exactly | the renderer | Velocity only |
| `lang` | the name and everything under it | Velocity itself | Velocity only |

`plugins` and `lang` are refused whether the claim holds a directory or a plain
file of that name.

The list follows the flavour. A Paper server does not refuse `velocity.toml` or
`lang/`: nothing on a Paper server writes either, so refusing them would be a
rule with no reason behind it, and would crash-loop a group whose claim carries
a `lang` directory for something else entirely. A proxy likewise does not
refuse the Paper files.

Velocity migrates `lang/messages.properties` to MiniMessage on every start and
writes the result back, so a file placed there is overwritten before anybody
reads it. Nothing breaks, which is why it is refused: a copy that silently does
nothing is worse than a collision that announces itself.

The remedy is never "put it somewhere else in the claim". For the renderer's
files it is `spec.configOverlay`; for `plugins/` it is `extraPlugins`.

The refusal arrives at start, not at admission. A claim's contents are not
knowable when somebody writes the group, so a wrong file crash-loops the pod
rather than failing the `kubectl apply`. To make that liveable, the message
says what is wrong in one sentence, in the container log an operator
already reads when a group does not come up:

```
spawnery: spec.extraFiles carries server.properties, which the operator writes itself.
spawnery: use spec.configOverlay for it. Refusing to start.
```

## From an image instead of a claim

`extraPlugins` and `extraFiles` can name an image instead of a claim:

```yaml
spec:
  extraPlugins:
    image: registry.example.net/lobby-plugins@sha256:…
  extraFiles:
    image: registry.example.net/lobby-files@sha256:…
```

The image's filesystem root is what the claim's root would be. It is mounted
read-only as an image volume and copied exactly as a claim is. Every node pulls
and caches it on its own, so no single server holds the group's plugins, and a
new digest rolls the group like any other change. The pod's
`imagePullSecrets` (from the Network) apply, and `pullPolicy` defaults to
`IfNotPresent`. An image source needs neither `--allow-plugin-volumes` nor
`--allow-file-volumes`.

Image volumes need Kubernetes 1.31 or later with the `ImageVolume` feature
enabled (on by default only in recent releases) and a container runtime that
supports them, such as containerd 2.1 or later. On a cluster without them the
API server drops the volume's source and the pods of the group are refused,
so check before switching a group: `kubectl explain pod.spec.volumes.image`
answers on a cluster that has the field.

## Placeholders filled at start

```yaml
spec:
  substitution:
    prefix: SECRET_
  env:
    - name: SECRET_DB_PASSWORD
      valueFrom:
        secretKeyRef: { name: lobby-db, key: password }
```

After copying, the entrypoint replaces every `{{ SECRET_… }}` in the copied
text files (`.yml`, `.yaml`, `.json`, `.properties`, `.conf`, `.toml`, `.txt`,
`.cfg`) with the variable of that name, verbatim. A placeholder whose variable
is missing stops the start and names the file and the placeholder. This keeps
secrets out of the image or claim: the artifact carries the placeholder, the
cluster the value.

## Styling what the agent says

`Network.spec.defaults.feedFormat` sets the shape of every line the agent
writes into chat: both an announcement about the cloud and a reply to a
`/cloud` command. It is one field because both come from the same plugin, and
a network that styles one should not have to style the other to match.

It is [MiniMessage](https://docs.advntr.dev/minimessage/format.html), which
both Paper and Velocity parse. `$EVENT_MESSAGE` is replaced by what the line
has to say; everything around it is yours. The default:

```yaml
spec:
  defaults:
    feedFormat: "<gray>»</gray> <gradient:aqua:green>Spawnery</gradient> <dark_gray>|</dark_gray> <gray>$EVENT_MESSAGE"
```

Changing it rolls nothing. The format travels in the network picture the
operator already sends, not in the pod. A pod's environment is part of the
pod hash, so a format carried there would make re-wording a chat line replace
every server on the network. An edit takes effect within a resync interval.

A blank value falls back to that default rather than printing nothing, which is
also what an agent does when talking to an operator too old to send the field.

Colour inside `$EVENT_MESSAGE` carries meaning, and the format cannot change
it. A server that takes joins is green and one that does not is red, because
that is the question somebody is actually asking, and it disagrees with the
phase during a drain. Warnings are red for the same reason.
