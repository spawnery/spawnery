# Mounting files into a group

`spec.mounts` puts a ConfigMap, a Secret or a PersistentVolumeClaim at a path
inside every pod of a group. Both `ServerGroup` and `ProxyGroup` have it.

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: bingo-solo
spec:
  # ...
  mounts:
    - name: worlds                  # read-only: the group consumes maps
      mountPath: /data/worlds
      persistentVolumeClaim:
        claimName: map-pool
    - name: assets                  # a ConfigMap, as before
      mountPath: /data/resources
      configMap:
        name: shared-assets
```

Exactly one source per mount. Naming none, or two, is refused by the API
server rather than rendering an empty volume or quietly dropping one of them.

## What a claim is for

`extraPlugins` is deliberately narrow: one claim, read-only, and the entrypoint
copies it into `plugins/`. [`extraFiles`](plugins-from-a-volume.md#files-from-a-volume) is
the same mechanism one directory up: its claim is copied into the whole
working directory, which is where a file that is not a plugin and that no mount
can reach belongs (`config/sponge/sponge.conf` motivated it). A claim mount
carries everything that belongs anywhere else: a world tree, a directory of
assets every server reads, the output of one group that another consumes.

`extraPlugins` and `extraFiles` (not `spec.mounts`) can also name an image
instead of a claim; see [From an image instead of a claim](plugins-from-a-volume.md#from-an-image-instead-of-a-claim).

A mount is one volume at one path, with no composition, priority or
per-server rendering. Assembling what goes in the volume is somebody else's
job.

`extraFiles` and a claim mount share two properties that an administrator
would otherwise discover the hard way. The source wins on every start: a
server that rewrites a file it was seeded with finds the claim's version again
next start, which is why a world does not belong in either. And nothing about
a claim's contents reaches the pod hash. The operator holds a claim name, not a
filesystem, so a changed file reaches a server on its next start rather than
replacing one already running. An image source is the exception: its
reference is part of the pod, so a new digest rolls the group.

## One file, not a directory

A mount lands the *whole* source at `mountPath`, so a path that names a file
gets a directory there: `/data/bukkit.yml/` holding the ConfigMap's keys as
separate files. A server looking for that file finds a directory and reports a
parse error, which says nothing about a mount.

`subPath` is how a single file lands:

```yaml
    - name: bukkit
      mountPath: /data/bukkit.yml
      subPath: bukkit.yml
      configMap:
        name: server-files
```

A ConfigMap or Secret mounted through `subPath` does not update. Kubernetes
refreshes a projected volume in place, and a `subPath` mount is a bind of one
file out of it that the kubelet never re-points. Editing the ConfigMap changes
nothing in a running pod, and nothing reports that. Without `subPath` the file
does update, eventually and with no restart. That difference is why this is a
field you opt into rather than something inferred from the path looking like a
file.

Either way, the contents reach no digest, so editing a ConfigMap rolls nothing
on its own.

## Read-only unless it says otherwise

```yaml
      persistentVolumeClaim:
        claimName: world-pool
        writable: true
```

The field is `writable` and not `readOnly` so that the zero value is the safe
one: an omitted field, a field somebody has not heard of, and a field lost in a
hand-edited manifest all land on read-only.

Writable applies to the claim and to nothing else. A ConfigMap or Secret is
mounted read-only by the kubelet whatever anybody writes, so the pod says so
too.

Nothing here coordinates two writers. This exists for one group filling a pool
that others read. Two groups that both write the same claim get
exactly what two processes writing one filesystem get, and the operator has no
way to know which of them meant to.

## The claim must be `ReadWriteMany`

The same rule [`extraPlugins`](plugins-from-a-volume.md#the-claim-must-be-readwritemany)
follows, for the same reason and with the same refusal: every pod of the group
mounts it, they are spread across nodes, and a `ReadWriteOnce` claim would
leave the second one `Pending` on a scheduling error about volume affinity with
nothing naming the claim. The operator refuses it up front instead with
`Accepted=False`, reason `MountVolumeUnusable`, with the claim's actual access
modes in the message. A claim that does not exist is refused the same way.

The rule does not soften for a group that runs one replica today, because
`maxReplicas` is raised by edits that have nothing to do with storage.

## It needs `--allow-mount-volumes`

A claim-backed mount needs its own switch, `--allow-mount-volumes`.
`--allow-plugin-volumes` governs `spec.extraPlugins` and, as of 0.2.x, only
that. A group naming a claim on an installation with `--allow-mount-volumes`
off is refused with `MountVolumesDisabled` and a message pointing at the
operator's arguments rather than at the storage, which is fine.

ConfigMap and Secret mounts are not gated. They carry configuration an
administrator wrote, not a filesystem somebody filled.

The flag is not a security boundary: a `PersistentVolumeClaim` is a namespaced
object in the same trust domain as the group naming it, so the switch stops
nobody who was not already stopped. It lets an operator make "this
installation mounts no claim" a fact rather than a convention.

## Reserved paths

A mount may not land on the paths the operator uses itself. `/var/run/spawnery`
and `/etc/spawnery` are refused along with anything nested inside them or above
them: a mount at `/var/run/spawnery/token` would shadow the agent's own
credentials, and the server would fail to authenticate with nothing naming the
mount. `/data` and `/tmp` are refused only as exact matches, since mounting inside
`/data` is the ordinary way to add files and is why the example above works.
`/data/plugins` is refused; that is what `extraPlugins` is for.

`/data/config` is refused too, at it and inside it, and that one was measured
rather than reasoned. The kubelet creates a mount's parent directory
itself, root-owned and group-read-only:

```
/data          drwxrwsrwx  0 10001    ← fsGroup makes the volume root writable
/data/config   drwxr-sr-x  0 10001    ← the mount's parent does not inherit it
```

`fsGroup` with `OnRootMismatch` only ever touches the volume's own root, so the
container cannot write into that directory, and the first thing it tries to
write is `spawnery-config`'s own `paper-global.yml`. The server never starts,
and the error names a file rather than a mount.

The operator cannot make it work: the ownership is the kubelet's, and changing
it would need a root init container, which every pod here is built not to
have. So it is refused, with a message naming `spec.configOverlay`, which is
where `server.properties`, `paper-global.yml`
and `paper-world-defaults.yml` belong anyway.

## Mounts and `spec.storage.keep`

A mount point under `/data` is never deleted by `spec.storage.keep`, and
neither is anything below it or the directories above it, whether the mount is
read-only or writable. The entry does not have to list it.
[What survives a start](persistent-worlds.md#what-survives-a-start) has the
rest.

## Editing a mount replaces the group's servers

A mount shapes the pod, so it is in the group's pod digest, and adding one,
removing one or flipping `writable` rolls the group the way an image bump does.

The volume's *contents* reach no digest at all, because the operator holds a
claim name, not a filesystem. Writing to the volume changes what the next server to
start reads, and changes nothing about the servers already running. That is the
same trade [Plugins from a volume](plugins-from-a-volume.md#changing-a-plugin) describes, and the
remedy is the same: restart the group's servers when you want the new contents.
