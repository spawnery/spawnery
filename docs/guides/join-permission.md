# Who may join a group

A `ServerGroup` can limit who may join its servers. The rule is `spec.joinPermission`:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: vip-lobby
  namespace: minecraft
spec:
  networkRef:
    name: production
  # ...
  joinPermission:
    node: network.vip   # optional; default spawnery.join.<group name>
    mode: Required      # Required (default) or DenyOnly
```

`node` is a LuckPerms permission node. It may contain lowercase letters, digits,
`.`, `_` and `-`, up to 128 characters. When it is empty, the node is
`spawnery.join.<group name>`. Leave `joinPermission` out and the group has no
rule.

`Required` admits only players who hold the node. An administrator with `*`
holds every node, so they pass.

`DenyOnly` admits everyone except players for whom the node is set to `false`:

```
/lp user <name> permission set <node> false
```

## Scoping a grant with contexts

The agents register four LuckPerms contexts: `server`, `group`, `network` and
`environment`. A grant can be limited to one of them. This one holds only for
players on a server of the `vip-lobby` group:

```
/lp group vip permission set network.vip true group=vip-lobby
```

The proxy asks with `server=<server name>`. That matches only when the backend's
LuckPerms server name is unset, which means `global`. A grant scoped to a
configured LuckPerms server name is seen by the backend but not by the proxy.
The proxy then routes the player around the group, and the backend never
decides.

## What a player sees

- On join, the proxy skips a fallback group the player may not join and tries
  the next group in the list.
- `/server` and other connects to such a group are refused with
  `You may not join <group>.`
- When the proxy lets a player through and the backend refuses the login, the
  player is disconnected with the same message. The message is the translatable
  key `spawnery.join.denied`, so a network with its own translations can set
  that key.

## Limits

- The check runs when a player joins. A player already on a server stays there
  when the permission changes.
- On an `OnDemand` group, only the proxies check the rule. See
  [Private servers](on-demand-servers.md).
- A group with a join rule registers a login listener on its Paper servers. As
  with `enforcePlayableSlots`, that listener turns off Paper's reconfiguration
  API on those servers.
- The operator does not know permissions. A plugin's `connect()` answers
  `ordered` even when the player is then refused.

Changing the rule restarts nothing. It reaches the agents with the next network
state.
