# Event feed levels and a transfer warning

**Status:** design, decided 2026-10-04
**Date:** 2026-10-04

## 1. What is wrong today

The `/cloud` event feed puts every event the operator records into chat, with
the operator's note verbatim ("pod lobby-x7k2 created on node server02 for
generation 14"). It is on by default for everyone holding
`spawnery.cloud.events`, and during a roll it fills the chat.

Three more problems sit beside it:

- A member of an on-demand group carries its key in its name. With a UUID as
  the key the name alone is longer than half a chat line.
- Every event, private members' included, goes to every backend that asked
  for events, so a plugin on a lobby learns the name of every private server.
  The network picture already leaves those out for backends.
- `/cloud events off` lasts until the proxy restarts. After every roll the
  feed is back on.

The replies of the `/cloud` commands were written one at a time and read that
way: long, uneven, and in places wordy.

Separately, a player on a proxy that is leaving and transfers players
(`spec.update.transfer`) is reconnected without warning when
`forceAfterSeconds` runs out.

## 2. Levels

Each player sees the feed at one of four levels:

| Event | `minimal` (default) | `normal` | `verbose` |
|---|---|---|---|
| a server or proxy is created | `[+] lobby-x7k2` | `[+] lobby-x7k2 starting` | the operator's note |
| a server passes its ready gate | | `[✓] lobby-x7k2 ready` | the operator's note |
| a server or proxy starts to leave (retiring, draining) | | `[-] lobby-x7k2 leaving` | the operator's note |
| a server or proxy is gone | `[-] lobby-x7k2` | `[-] lobby-x7k2 stopped` | the operator's note |
| any warning | `[!] lobby-x7k2 <short reason>` | `[!] lobby-x7k2 <short reason>` | the operator's note |
| any other kind | | | the operator's note |

`off` shows nothing.

- The agent maps each event kind to a row of this table. The operator keeps
  sending the same events as today, the same facts as `kubectl get events`.
- "Gone" is the one event every server and every proxy emits exactly once at
  its end; the implementation picks it from the code (for proxies
  `ProxyStopped`).
- A short reason comes from a small table (`StartupTimeout` → `did not start
  in time`, `PodRejected` → `pod refused`, `ForceStopped` → `killed`, and so
  on). A warning kind not in the table is read from its name
  (`PodNameConflict` → `pod name conflict`), so no warning is ever dropped.
- A kind the agent does not know appears only in `verbose`, so a new reason
  in the operator adds no unplanned chat.
- Lines of one kind in one group within the existing one-second window
  collapse as today (`[+] 5 lobby`). Warnings never collapse.
- `Network.spec.defaults.feedFormat` wraps every line, as today.

## 3. Names

A server or proxy is shown by its name, with one exception: a member of an
on-demand group whose key is longer than 6 characters is shown as its group,
a hyphen and the key's first 6 characters (`challenge-3f2b1c`). The agent
knows the group's type from its network picture.

Every name in the feed carries a hover with the full name and a click that
puts `/cloud info <full name>` into the chat box.

## 4. Choosing a level

```
/cloud events                          shows the current level
/cloud events minimal|normal|verbose|off
/cloud events on                       same as minimal
```

- Without `spawnery.cloud.events` a player sees nothing, as today.
- With LuckPerms on the proxy the level is the player's meta value
  `spawnery-feed`. The command sets it on the player and saves it; it is read
  through LuckPerms' meta cache, so inheritance applies:
  `/lp group admin meta set spawnery-feed normal` holds for every admin until
  one of them chooses otherwise. It survives a roll and a change of proxy.
- Without LuckPerms, or with LuckPerms not loaded, the level lives in the
  agent's memory, as the on/off switch does today.
- No value means `minimal`. An unreadable value is treated as `minimal`.

## 5. Who receives what

`cloudevent.Derive` marks an event about a Server with a non-empty
`spec.key` (an on-demand member) as private. A private event reaches proxy
sessions only, never backend sessions, in line with the network picture.
Plugins on backends that subscribe through the plugin API's `EventBus`
therefore no longer see private servers; `docs/guides/upgrading.md` says so.

## 6. The command replies

Every reply of the `/cloud` commands and every feed line in `agent/common`
goes through the `humanizer` skill once: `list`, `info`, `status`, `retire`,
`unretire`, `scale`, `forcestop`, `execute`, `events` and their errors. The
replies get shorter and uniform; what they say does not change. They stay
English. The end-to-end tests that read reply text change with them.

## 7. Warning before a forced transfer

A player whom `TransferPolicy.forced` is about to move gets a warning 10
seconds before the transfer is forced, or at once when `forceAfterSeconds`
is shorter than that.

- Exactly the players the forced pass would move: not behind a closed door,
  and only while another proxy of the group accepts transfers.
- Once per player per leaving proxy.
- On every proxy that leaves (a roll, a lowered `replicas`, `/cloud retire
  <proxy>`) in a group with `spec.update.transfer`. Without transfer nobody is
  moved and nobody is warned.
- Not on a change of server: that transfer happens at the moment the player
  switches.

The message is a translatable component in the pattern of the playable-slot
refusal: key `spawnery.transfer.warning`, fallback `You will be reconnected
in %s seconds.`, the argument being the seconds left. A network with its own
translations sets the key and may use any text. It is sent as a chat message
by the proxy.

The lead time is fixed at 10 seconds. A per-group field
(`spec.update.transfer.warnBeforeSeconds`) waits until someone needs another
value.

## 8. Tests

- Kotlin: the mapping table (every level, collapsing, an unknown kind only in
  `verbose`, an unknown warning readable); name shortening, hover and click;
  the level with a LuckPerms stand-in (reading with inheritance, writing, the
  fall-back without LuckPerms); the `events` command; the transfer warning
  (timing, once per player, not behind a closed door, not without a target
  proxy, not on a switch, key and fallback).
- Go: a private event reaches proxy sessions and no backend session.
- The end-to-end suite cannot see a player's chat (the test client holds no
  permission), so the feed is not tested end to end. The pull request lists
  what to check on staging with an administrator account, level by level, and
  how to trigger the transfer warning.

## 9. Versions

The agents and the operator change: a minor release after 0.19.0, as its own
pull request.
