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

The agents register `group`, `network` and `environment` as LuckPerms contexts,
and `server` as well when the backend's LuckPerms server name is unset
(`global`). A grant can be limited to one of them. This one holds only for
players on a server of the `vip-lobby` group:

```
/lp group vip permission set network.vip true group=vip-lobby
```

The proxy asks with `server=<server name>`, the name the operator gave the
server. A backend has that same context only when its LuckPerms server name is
unset. Then a grant scoped to `server=<server name>` holds on both sides. With a
configured name, the two sides disagree. A grant on the spawnery name is seen by
the proxy, which lets the player through, and a `Required` backend refuses them.
A grant on the configured name is seen by the backend but not by the proxy,
which routes the player around the group.

## What a player sees

- On join, the proxy skips a fallback group the player may not join and tries
  the next group in the list.
- When no fallback group is open to the player, the proxy disconnects them with
  Velocity's own message, which says there are no available servers to connect
  them to.
- `/server` and other connects to such a group are refused with
  `You may not join <group>.`
- When the proxy lets a player through and the backend refuses the login, the
  result depends on the connect:
  - On the initial join, the proxy sends the player to the next fallback group
    it thinks open, without a message. Only when no such group is left does the
    player get disconnected with the refusal message.
  - On a switch from a server, the player stays on their current server. Velocity
    tells them the connect was refused, with the refusal message.

  The refusal message is the translatable key `spawnery.join.denied`, so a
  network with its own translations can set that key.

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
- Agents older than 0.18.0 ignore the rule, so the game images and the proxy
  image must carry 0.18.0 or later.

Changing the rule restarts nothing. It reaches the agents within 30 seconds.
