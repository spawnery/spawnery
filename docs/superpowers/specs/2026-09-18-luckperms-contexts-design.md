# LuckPerms contexts from the pod

**Status:** design, awaiting review
**Date:** 2026-09-18

## 1. What this is for

A permission rule usually wants to say *where* it applies: this moderator on
the lobbies, that command only on a proxy. LuckPerms answers that with
contexts — key/value pairs a plugin registers, which then qualify any node
(`/lp group mod permission set x true group=lobby`).

On a Spawnery network nothing registers any. A server pod knows exactly what it
is — `SPAWNERY_NETWORK`, `SPAWNERY_GROUP`, `SPAWNERY_SERVER` are on every game
pod (`internal/podspec/server.go:462`), `SPAWNERY_PROXY` on every proxy
(`internal/podspec/proxy.go:58`) — and none of it reaches LuckPerms. So a grant
is either global or it is written against LuckPerms' `server` key, which
without a calculator stays `global` on every pod in the cluster.

CloudNet solved this years ago, and a network moving off CloudNet loses it:
`cloudnet-luckperms.jar` is one of the plugins the cloud injects into every
service, and `devtool` deletes it on the way to Spawnery
(`pruneCloudNetPlugins`, cyperia's `devtool/layout.go:239`) because it would
contact a CloudNet wrapper that is not there.

## 2. What CloudNet actually does

Read rather than assumed, from
`plugins/luckperms/src/main/java/eu/cloudnetservice/plugins/luckperms/` in
`CloudNetService/CloudNet`:

- One `StaticContextCalculator` — pod-wide values, nothing per player.
- It sets `service`, `service-uuid`, `task`, `node`, `environment`, and
  `group` once per CloudNet group the service belongs to.
- It additionally sets LuckPerms' own `DefaultContextKeys.SERVER_KEY` to the
  service name, **but only when `LuckPermsProvider.get().getServerName()` is
  still `global`.** Its comment gives the reason: LuckPerms uses that key to
  record where a player is, and with no value it writes the whole context set
  into that database column instead.
- It is a separate jar per platform, each declaring a hard dependency on
  LuckPerms and registering in `onLoad`.

Nothing in it crosses a network. Every value comes from the local service
configuration, which is why the same shape works here from environment
variables.

## 3. The shape

### 3.1 Four keys, in this project's vocabulary

```
server       = $SPAWNERY_SERVER, or $SPAWNERY_PROXY on a proxy
group        = $SPAWNERY_GROUP
network      = $SPAWNERY_NETWORK
environment  = paper | velocity
```

CloudNet's own key names are not carried over. `task` names a concept this
project does not have, `node` would be the Kubernetes node and the agent is
never told it, and `service-uuid` has no counterpart at all — a pod name is
already unique. The names above are the ones `Self` already uses
(`agent/api/.../Self.java`), so what a plugin reads from the API and what an
administrator writes in an `/lp` command are one vocabulary.

The cost is that permission data written against a CloudNet network does not
carry over unchanged: a node qualified `task=lobby` has to become
`group=lobby`. That is a one-time rewrite of existing grants, and it was
chosen over keeping a foreign vocabulary permanently in the documentation.

**`server` is LuckPerms' own key**, not one this design invents — it is the
literal value of `DefaultContextKeys.SERVER_KEY`. So it follows CloudNet's
rule exactly: set it to the pod name only while LuckPerms' configured server
name is still `global`. A context set is a multimap; setting it unconditionally
next to an administrator's configured `server: lobby` would leave both values
present and both rules matching, which is not a thing anyone asked for.

**The pod name and not the group name.** It changes with every pod, so it is
useless for a grant — and that is the point: grants have `group`, which is
stable and means what they want. `server` stays what LuckPerms uses it for,
which is saying precisely where a player is.

**A Purpur server reports `environment=paper`.** The value names the plugin
API the agent runs against, not the jar, so a grant does not change meaning
when someone swaps Purpur for Paper. Both are the same agent and the same
`:paper` subproject.

**An empty value is omitted rather than set to the empty string.** A context
`group=` matches nothing and reads like a bug in every listing that prints it.

### 3.2 In the agent jar, optional

No new jar. The agent is already in every pod, already reads all four inputs,
and a second artefact would duplicate the whole delivery chain — Gradle
subproject, Nix derivation, `deps.json` entry, a copy in both entrypoints, a
version to move — for about sixty lines.

The split is the one the rest of the agent uses:

- **`agent/common`** gets the key building as a pure function: the `Self` the
  agent already built and LuckPerms' configured server name in, pairs out. It
  names no LuckPerms type and needs no server to test.

  It takes `Self` rather than reading the environment a second time, which also
  settles `environment`: `Self` is sealed on exactly `ServerSelf` and
  `ProxySelf`, so the value is derived from which shape this is and cannot
  disagree with the side it is running on.
- **`agent/common`** also gets the calculator and the registration, and this is
  the one place the design differs from CloudNet's. Nothing about either is
  per-platform — the calculator names only LuckPerms types, and
  `LuckPermsProvider.get().getContextManager().registerCalculator(…)` is the
  same call on both sides. Two copies of it would be two copies that can drift.
- **`agent/paper` and `agent/velocity`** each get one call and one descriptor
  entry. No new class on either side.

Registration happens in the `Environment.Configured` branch of `onEnable`,
where the API is installed today. That gives one property for free: **a dormant
agent registers nothing.** A pod that is not a Spawnery pod has no endpoint,
takes the `Dormant` branch, and never reaches the registration — no separate
check, and no chance of the two disagreeing.

**A class probe in front of the registration, and it is not belt-and-braces.**
Without LuckPerms the API cannot resolve, and the resulting
`NoClassDefFoundError` would leave `onEnable` through Paper's plugin manager,
which disables the plugin that threw it. A server running no permission plugin
would lose its cloud connection over a permission feature it never asked for.
The probe is also what the unit tests can reach: the API is kept off the test
classpath, so a test that calls the registration exercises exactly that path.

**The descriptors carry the optionality, not the code.** `paper-plugin.yml`
gains `dependencies.server.LuckPerms` with `required: false` and
`load: BEFORE`; `velocity-plugin.json` gains a dependency entry with
`optional: true`. Both give ordering when LuckPerms is present and silence when
it is not.

The Velocity trap, already documented at `agent/velocity/.../AgentPlugin.kt:67`:
the `@Plugin` annotation there is read by nothing. The hand-written
`velocity-plugin.json` is the real descriptor, and it is the file that has to
change.

### 3.3 What this does not touch

The operator, the CRDs, the chart templates and the public Java API are all
unchanged. This is agent-side only, which is why there is no new field for
anyone to configure: the four values are already on the pod, and a network that
does not run LuckPerms sees no difference.

## 4. What moves with it

- **`net.luckperms:api` is `compileOnly`**, so it is not bundled and
  `hack/agent-jar-check.sh` — which fails on any class outside
  `cloud/spawnery/agent/` — stays green.
- **`agent/deps.json` has to be regenerated.** It is the Maven lockfile the Nix
  build reads, and `make agent-deps` is the only thing that writes it: it
  reaches Maven Central, is part of no other target, and CI diffs the result.
- **`imageVersion` in `flake.nix` moves**, `operatorVersion` does not. The
  chart's `image.tag` follows, so `make manifests` runs and its diff on
  `docs/reference/chart-values.md` is committed with the bump.
- **The release takes a minor step.** New behaviour, by the rule decided on
  2026-09-08.
- **Documentation**: a short section in `docs/guides/cloud-command.md`, which
  today says Spawnery ships no permission system and shows an `/lp` line. That
  page carries no ceiling in `hack/docs-length.sh`.
- New files must be `git add`ed before any Nix build, which reads the index.

## 5. What proves it

**Unit tests, in `agent/common`:** the four keys from a full environment; the
proxy taking its name from `SPAWNERY_PROXY`; an empty variable omitted rather
than emitted empty; `server` present when LuckPerms reports `global` and absent
when it reports anything else.

**A running pod, which the tests cannot stand in for.** A server with LuckPerms
from an `extraPlugins` claim, joined, and `/lp user <name> info` listing all
four keys under the player's current contexts — then a grant qualified
`group=<a group>` taking effect on that group's servers and not on another's.
That is the step that shows the descriptors, the load order and the registration
were right, none of which a JUnit run can see.

## 6. Open

**Whether `environment` should be `server` / `proxy` instead.** `paper` and
`velocity` name the platform, which is what CloudNet's `environment` means and
what a plugin author recognises. The pair `server` / `proxy` would be this
project's own vocabulary and would not go stale if a third backend platform
ever appeared. Decided for `paper` / `velocity` on the grounds above; the
reversal costs a rewrite of any grant using the key.
