# A permission to join a server group

**Status:** design, decided 2026-10-03
**Date:** 2026-10-03

## 1. What is missing

Anyone who reaches a network can join every server group in it. The only
login check spawnery has is the playable-slot limit (`spec.enforcePlayableSlots`,
bypassed by `spawnery.join.full.<group>`). A network that wants a group for
its staff, a beta, or a VIP lobby has to write its own plugin, and that plugin
cannot steer the proxies' routing: a player sent to a fallback group they may
not enter is refused and disconnected, instead of landing in the next group
of the list.

Two kinds of rule are wanted:

- **Required.** Only players who hold the permission may join.
- **Deny only.** Everyone may join, except players for whom the permission is
  explicitly set to `false` (in LuckPerms, `/lp user <name> permission set
  <node> false`).

## 2. The shape

```yaml
kind: ServerGroup
spec:
  joinPermission:          # new, optional; absent = no rule
    node: network.vip      # optional; empty = spawnery.join.<group>
    mode: Required         # Required (default) | DenyOnly
```

- `node` must look like a permission node: lowercase letters, digits, `.`,
  `_` and `-`, at most 128 characters. Empty means `spawnery.join.<group name>`.
- `mode` is an enum with the default `Required`.
- The field exists for all three group types. Section 6 says what is
  different for `OnDemand`.

The rule is not part of any pod spec and does not move `DesiredServerHash`.
Changing it rolls nothing; it reaches the agents with the next network state.

## 3. The wire

`GroupState` gains two fields:

```proto
// The permission a player needs (or must not have denied) to join this
// group's servers, already resolved: empty means the group has no rule.
string join_permission = 11;
// True for mode DenyOnly: only an explicit false refuses.
bool join_permission_deny_only = 12;
```

The operator resolves the default node name in `internal/netstate`, so the
naming rule exists once. `internal/agentpb/contract_test.go` pins both field
numbers. An agent older than these fields ignores them and enforces nothing.

## 4. The decision

One pure function in `agent/common`, used by both platforms:

```kotlin
fun mayJoin(rule: JoinRule?, value: PermissionValue): Boolean
// rule == null            -> true
// Required: true only for PermissionValue.TRUE
// DenyOnly: false only for PermissionValue.FALSE
```

`PermissionValue` is `TRUE`, `FALSE` or `UNDEFINED`. Each platform turns its
own permission answer into one of the three.

## 5. Where it is enforced

**The backend decides.** The Paper agent's `LoginGateListener` checks the
rule on `PlayerLoginEvent`, before the playable-slot limit. Bukkit answers in
the server's own LuckPerms contexts: `UNDEFINED` when
`isPermissionSet(node)` is false, otherwise `hasPermission(node)`. A refused
player is kicked with a translatable component, key `spawnery.join.denied`,
fallback text `You may not join <group display name>.`, the same pattern as
the full-round refusal.

Paper turns its reconfiguration API off server-wide while any
`PlayerLoginEvent` listener is registered, which is why the listener is
registered only for a group that enforces playable slots today. It is now
registered for a group that enforces playable slots or has a join rule, and
for no other. The guide says so.

**The proxy advises.** It evaluates the same rule earlier, so routing goes
around a group a player may not enter instead of into a refusal:

- `JoinPermissions` looks up a player's value for a node *in the contexts of
  the target server*: `server`, `group`, `network` and `environment`, the four
  contexts the agents register (2026-09-18). It asks LuckPerms through its API
  with those contexts when LuckPerms is installed on the proxy, and falls back
  to Velocity's `Player#getPermissionValue` without contexts when it is not.
  The proxy's own contexts would be wrong: a grant limited to `group=vip-lobby`
  never matches on a proxy whose group is `proxies`. The LuckPerms lookup is
  guarded the way `LuckPermsContexts.registerIfPresent` is, because an
  installed LuckPerms that failed to load throws on first use.
- `Router.choose` takes a per-player predicate. A group the player may not
  join is skipped like an empty one, and the next group of the list is tried.
  This covers the first server on join, `Drain` and `Rescue`.
- A transfer landing (`spec.update.transfer`) whose server the player may not
  join is ignored, and the router chooses instead.
- `ServerPreConnectEvent` denies a connection to a server whose group the
  player may not join, with the same message. That catches `/server`, other
  plugins, and moves the operator orders.

When the proxy is wrong (no LuckPerms, a grant only the backend sees), the
backend still refuses. The player then sees the refusal instead of a detour
to the next group.

## 6. On-demand groups

A backend's network picture leaves out on-demand groups (`netstate.Audience`),
so the backend of an on-demand member never sees its own group's rule. For
those groups only the proxy enforces. That is complete in practice, because a
backend is reachable only through a proxy, but it has no second check behind
it. Splitting the network picture per recipient would fix it and belongs to
the event feed rework, which has to split the feed per recipient anyway.

## 7. The plugin API

Nothing changes. `connect()` already answers `ordered`, which means the
operator gave the order, not that the player arrived. The operator does not
know permissions and cannot tell beforehand. A connect to a group the player
may not join ends with `ordered` and the player sees the refusal; the Javadoc
of `connect` says so.

## 8. Left out

- A player already on a server is not kicked when the permission is taken
  away. The check runs when a player joins.
- Proxy groups get no join rule. Who may connect to a network is the proxy's
  or an auth plugin's business.
- No bypass permission. `Required` is already a grant, and an administrator
  holding `*` passes it; `DenyOnly` refuses only an explicit `false`.

## 9. Tests

- Kotlin unit tests: the full `mayJoin` table (two modes times three values,
  plus no rule); `Router` skipping a forbidden group on join, drain and
  rescue; `LoginGateListener` refusing before the slot limit;
  `JoinPermissions` asking with the target's contexts and falling back without
  LuckPerms.
- Go: envtest for the CRD validation (pattern, enum, default); `netstate`
  carrying the resolved node and mode, including an on-demand group to the
  proxies; the contract test pinning the field numbers.
- On real images, a subtest of the tutorial e2e. The fallback list is
  `[vip, lobby]` with `vip` on `Required`; the test images have no LuckPerms,
  so nobody holds the node and the player lands in `lobby`. With `DenyOnly`
  the same player lands in `vip`. The test must fail with the router's
  predicate taken out.

## 10. Versions

A new CRD field is a minor step (rule of 2026-09-08). `imageVersion` moves for
the agents, `operatorVersion` for `netstate`, and the chart for the CRD. The
feature is released on its own, before the `/cloud` command and event feed
work.
