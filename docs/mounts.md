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
copies it into `plugins/`. Everything that belongs anywhere else had nowhere to
go — a world tree, a directory of assets every server reads, the output of one
group that another consumes. That is what a claim mount carries.

It is still not a layered template system. There is no composition, no
priority, no per-server rendering. A mount is one volume at one path, and
assembling what goes in the volume is somebody else's job.

## One file, not a directory

A mount lands the *whole* source at `mountPath`, so a path that names a file
gets a directory there — `/data/bukkit.yml/` holding the ConfigMap's keys as
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

**A ConfigMap or Secret mounted through `subPath` does not update.** Kubernetes
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

**Nothing here coordinates two writers.** One group filling a pool that others
read is the case this exists for. Two groups that both write the same claim get
exactly what two processes writing one filesystem get, and the operator has no
way to know which of them meant to.

## The claim must be `ReadWriteMany`

The same rule [`extraPlugins`](plugins.md#the-claim-must-be-readwritemany)
follows, for the same reason and with the same refusal: every pod of the group
mounts it, they are spread across nodes, and a `ReadWriteOnce` claim would
leave the second one `Pending` on a scheduling error about volume affinity with
nothing naming the claim. The operator refuses it up front instead —
`Accepted=False`, reason `MountVolumeUnusable`, with the claim's actual access
modes in the message. A claim that does not exist is refused the same way.

The rule does not soften for a group that runs one replica today, because
`maxReplicas` is raised by edits that have nothing to do with storage.

## It needs `--allow-plugin-volumes`

The flag that gates `extraPlugins` gates a claim mount too. One switch rather
than two, because what it turns off is the same sentence for each: content this
installation did not ship reaches a game server from a volume. A group naming a
claim on an installation with the flag off is refused with
`MountVolumesDisabled` and a message pointing at the operator's arguments
rather than at the storage, which is fine.

ConfigMap and Secret mounts are not gated. They carry configuration an
administrator wrote, not a filesystem somebody filled.

The flag is not a security boundary, and [`plugins.md`](plugins.md#turning-it-on)
says at more length why not.

## Reserved paths

A mount may not land on the paths the operator uses itself. `/var/run/spawnery`
and `/etc/spawnery` are refused along with anything nested inside them or above
them: a mount at `/var/run/spawnery/token` would shadow the agent's own
credentials, and the server would fail to authenticate with nothing naming the
mount. `/data` and `/tmp` are refused only as exact matches — mounting *inside*
`/data` is the ordinary way to add files, which is why the example above works.
`/data/plugins` is refused; that is what `extraPlugins` is for.

## Editing a mount replaces the group's servers

A mount shapes the pod, so it is in the group's pod digest, and adding one,
removing one or flipping `writable` rolls the group the way an image bump does.

The volume's *contents* reach no digest at all — the operator holds a claim
name, not a filesystem. Writing to the volume changes what the next server to
start reads, and changes nothing about the servers already running. That is the
same trade [`plugins.md`](plugins.md#changing-a-plugin) describes, and the
remedy is the same: restart the group's servers when you want the new contents.
