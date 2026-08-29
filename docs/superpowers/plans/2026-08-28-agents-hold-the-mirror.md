# Milestone 7b-4 — the agents hold the mirror

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `Spawnery.api()` returns something that answers. Both agents keep the last `NetworkState` they were sent and serve every read from it, with one implementation rather than two that match.

**Architecture:** The symmetry the whole design promises is made **structural** here rather than tested into existence. `NetworkMirror` and `MirrorApi` live in `agent/common`; both platforms construct the same `MirrorApi` with their own `Self`. Two implementations that had to agree would eventually not.

**Tech Stack:** Kotlin (`agent/common`, both plugins) against the Java API from 7b-1. No proto change, no operator change.

**Spec:** [`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../specs/2026-08-27-cloud-api-design.md) §3.1, §3.2, §6.2, §9.1

## Global Constraints

- Every build and test command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`
- Commit messages are Conventional Commits with English subjects, and every
  commit ends with exactly these two trailers:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **Never push, never merge, and never create a tag.**
- **Never run `git config` in any form.**
- `nix build` filters the source tree through the git index. `git add` before
  every `make agent`.
- **No Kotlin file under `agent/` carries a licence header.** Match the
  neighbours; the previous plan asserted otherwise and was wrong.
- **A new constructor parameter goes last.** Every call site in these packages
  passes positionally, and a parameter added above rebinds them — silently,
  when the types happen to match.

## The permission question, answered

`docs/network-boundaries.md` left one thing open at the end of 7b-3: whether a
plugin should need a permission to read the roster.

**It cannot need one, because neither platform has a concept to hang it on.**
Bukkit permissions attach to a `CommandSender`, Velocity's to a
`CommandSource`; both are about a *player* or the console. There is no
`Plugin.hasPermission`, and a plugin calling `Spawnery.api()` presents no
identity to check. A gate here would have to be invented — a registry of
trusted plugin ids, say — and it would be worth nothing: any plugin on the
server can already read Bukkit's own player list, load classes, and call
whatever the JVM exposes.

So the boundary is the one that already exists and is already written down:
**who may install a plugin.** That is the same person who may create a pod in
the namespace, which `charts/spawnery/README.md` documents as one trust
domain and tells an operator to treat as such.

7b-5's `/cloud` command is different and does gate: a *command* has a
`CommandSource`, so `spawnery.cloud.read` is expressible there and is where
the permission belongs.

## File structure

| Path | Responsibility |
|---|---|
| `agent/common/src/main/kotlin/.../NetworkMirror.kt` | Holds the last state; converts proto to API types |
| `agent/common/src/main/kotlin/.../MirrorApi.kt` | The one `SpawneryApi` implementation |
| `agent/common/src/test/kotlin/.../MirrorApiTest.kt` | Its rules, and the symmetry claim |
| `agent/paper/src/main/kotlin/.../ServerRole.kt` | Applies `NETWORK_STATE` |
| `agent/velocity/src/main/kotlin/.../ProxyRole.kt` | The same |
| `agent/paper/src/main/kotlin/.../AgentPlugin.kt` | Builds a `ServerSelf`, installs, uninstalls |
| `agent/velocity/src/main/kotlin/.../AgentPlugin.kt` | Builds a `ProxySelf`, installs, uninstalls |
| `docs/network-boundaries.md` | The permission question, answered |

---

### Task 1: The mirror

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/NetworkMirror.kt`
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/NetworkMirrorTest.kt`

**Interfaces:**
- Consumes: `agentpb.NetworkState`, and the API's `Group`, `ServerInfo`,
  `CloudPlayer`, `ServerPhase`.
- Produces:
  ```kotlin
  class NetworkMirror {
      fun apply(state: NetworkState)
      fun groups(): List<Group>
      fun servers(): List<ServerInfo>
      fun players(): List<CloudPlayer>
  }
  ```

- [ ] **Step 1: Write the failing test**

```kotlin
package cloud.spawnery.agent

class NetworkMirrorTest {
    @Test
    fun `a mirror that has been told nothing answers empty rather than null`() {
        val mirror = NetworkMirror()
        assertEquals(emptyList(), mirror.groups())
        assertEquals(emptyList(), mirror.servers())
        assertEquals(emptyList(), mirror.players())
    }

    @Test
    fun `applying a state replaces what came before rather than merging`() {
        val mirror = NetworkMirror()
        mirror.apply(stateWith(servers = listOf("lobby-a", "lobby-b")))
        mirror.apply(stateWith(servers = listOf("lobby-b")))

        assertEquals(listOf("lobby-b"), mirror.servers().map { it.name() })
    }

    @Test
    fun `a phase this jar predates becomes UNKNOWN rather than throwing`() {
        val mirror = NetworkMirror()
        mirror.apply(stateWithPhase("SomethingLaterInvented"))
        assertEquals(ServerPhase.UNKNOWN, mirror.servers().single().phase())
    }

    @Test
    fun `a player on no backend carries an empty Optional`() {
        val mirror = NetworkMirror()
        mirror.apply(stateWithPlayer(uuid = someUuid, name = "alice", server = ""))
        assertTrue(mirror.players().single().server().isEmpty)
    }

    @Test
    fun `a player entry with an unparseable uuid is dropped rather than failing the apply`() {
        // The operator sends what a proxy reported. One malformed entry must
        // not cost this agent its whole mirror -- ProxyRole's own guard makes
        // the same trade for the same reason, and it is the reason a FullSync
        // skips a bad address rather than discarding the sync.
        val mirror = NetworkMirror()
        mirror.apply(stateWithPlayers(listOf("not-a-uuid" to "mallory", validUuid to "alice")))
        assertEquals(listOf("alice"), mirror.players().map { it.name() })
    }
}
```

Write the `stateWith*` helpers in the test file. They build `NetworkState`
protos and are three lines each.

- [ ] **Step 2: Run it and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :common:test --tests "*NetworkMirrorTest*" --console=plain --offline'`

- [ ] **Step 3: Write the mirror**

Hold the three lists behind one `@Volatile` reference to an immutable triple,
replaced whole on each `apply`. **Not three fields and not a lock**: a reader
taking groups from one state and players from the next would see a player on a
server that the same read says does not exist, and every reader here is on a
platform thread that must not block on a gRPC callback.

`apply` is called from `SessionLoop`'s callback thread; the readers are called
from Bukkit's or Velocity's, and from a plugin's own. Say that in the class
comment the way `ServerDirectory`'s does.

- [ ] **Step 4: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :common:test --console=plain --offline'`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/common/src
git commit -m "$(cat <<'EOF'
feat(agent): a mirror of the network, replaced whole

One volatile reference to an immutable snapshot, swapped on each apply,
rather than three fields or a lock. A reader taking groups from one state and
players from the next would see a player on a server the same read says does
not exist -- and every reader is on a platform thread that must not block
behind a gRPC callback.

A player entry with an unparseable UUID is dropped and the rest of the state
applies. One malformed entry must not cost an agent its whole mirror, which
is the trade ProxyRole already makes for a FullSync carrying one bad address.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 2: One implementation, and the symmetry it makes structural

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/MirrorApi.kt`
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/MirrorApiTest.kt`

**Interfaces:**
- Consumes: `NetworkMirror` from Task 1, `SpawneryApi` and `Self` from 7b-1.
- Produces: `class MirrorApi(private val mirror: NetworkMirror, private val self: Self) : SpawneryApi`.

- [ ] **Step 1: Write the failing test**

The important one is last, and it is §6.2's second invariant in the only form
that can still catch something:

```kotlin
class MirrorApiTest {
    @Test
    fun `a lookup by name finds what the list holds`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        val api = MirrorApi(mirror, serverSelf())

        assertEquals("lobby-a", api.server("lobby-a").orElseThrow().name())
        assertEquals("lobby", api.group("lobby").orElseThrow().name())
    }

    @Test
    fun `a lookup for something absent is empty rather than null`() {
        // Optional and not null, because a plugin that forgot a null check
        // gets an NPE at some later line while an empty Optional refuses at
        // the point of use.
        val api = MirrorApi(NetworkMirror(), serverSelf())

        assertTrue(api.server("nothing-here").isEmpty)
        assertTrue(api.group("nothing-here").isEmpty)
        assertTrue(api.player(UUID.randomUUID()).isEmpty)
    }

    @Test
    fun `self is whatever the platform supplied`() {
        val self = serverSelf()
        val api = MirrorApi(NetworkMirror(), self)

        assertSame(self, api.self())
        // The type is how a plugin asks which side it is on, so it has to
        // survive the round trip rather than being flattened to Self.
        assertTrue(api.self() is ServerSelf)
    }

    // §6.2's symmetry invariant. One implementation makes it structural, so
    // this is what is left to assert: given one state, the answers do not
    // depend on which side the API is running on. It would catch a future
    // `if (self is ProxySelf)` in a read method, which is the only way the two
    // sides could still come apart.
    @Test
    fun `both sides answer every read identically from one state`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        val onServer = MirrorApi(mirror, serverSelf())
        val onProxy = MirrorApi(mirror, proxySelf())

        assertEquals(onServer.groups(), onProxy.groups())
        assertEquals(onServer.servers(), onProxy.servers())
        assertEquals(onServer.players(), onProxy.players())
        assertEquals(onServer.server("lobby-a"), onProxy.server("lobby-a"))
        assertEquals(onServer.player(someUuid), onProxy.player(someUuid))
    }
}
```

`aRichState()` builds a `NetworkState` with one group `lobby`, two servers
`lobby-a` and `lobby-b`, and one player; `serverSelf()` and `proxySelf()` are
three-line anonymous implementations. Write all three in the test file — the
module has no mocking framework and must not gain one.

- [ ] **Step 2: Run and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :common:test --tests "*MirrorApiTest*" --console=plain --offline'`

- [ ] **Step 3: Write it**

Every method delegates to the mirror; `self()` returns the constructor
argument. The class comment carries the design decision:

```kotlin
/**
 * The one implementation of [SpawneryApi], for both platforms.
 *
 * **This is where the design's central promise stops being a claim.** A plugin
 * author moving between a backend and a proxy gets the same answers because
 * they are the same code, not because two implementations were written to
 * agree and a test checks they still do. The only platform-specific input is
 * [self], and it is a constructor argument rather than a branch.
 *
 * The reads never block and never fail: [NetworkMirror] hands back an
 * immutable snapshot, so a caller on Bukkit's main thread pays a volatile read
 * and nothing else.
 */
```

- [ ] **Step 4: Run and commit**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :common:test --console=plain --offline'`

```bash
git add agent/common/src
git commit -m "$(cat <<'EOF'
feat(agent): one SpawneryApi implementation, for both platforms

The design's central promise is that a plugin author moving between a backend
and a proxy does not think differently. This makes that structural: they get
the same answers because it is the same code, with the only
platform-specific input a constructor argument rather than a branch.

Section 6.2 of the spec asked for a test comparing the two platforms'
surfaces. With one implementation that comparison is a tautology, so the test
asserts what is left and what could still break: given one state, every read
answers identically whichever Self it was built with. It would catch a future
`if (self is ProxySelf)` inside a read, which is the only way the two sides
can now come apart.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 3: Both roles apply it

**Files:**
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/ServerRole.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/ProxyRole.kt`
- Test: both roles' existing test files

**Interfaces:**
- Consumes: `NetworkMirror` from Task 1.
- Produces: both roles take a `NetworkMirror` **as their last constructor
  parameter** and apply `NETWORK_STATE` to it, returning `Directive.None`.

- [ ] **Step 1: Write the failing tests**

One per role, and each asserting the same thing: a state message reaches the
mirror and the loop is told to do nothing else about it.

```kotlin
    @Test
    fun `a network state reaches the mirror`() {
        val mirror = NetworkMirror()
        val role = newRole(mirror = mirror)

        val directive = role.onMessage(networkState(servers = listOf("lobby-a")))

        assertEquals(Directive.None, directive)
        assertEquals(listOf("lobby-a"), mirror.servers().map { it.name() })
    }
```

`ProxyRole.onMessage` is wrapped in a `runCatching` that must not be
disturbed — read its comment. A malformed state must not end the session, and
the existing guard already ensures that; this test only has to not break it.

- [ ] **Step 2: Run and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :paper:test :velocity:test --console=plain --offline'`

- [ ] **Step 3: Add the branch to both**

`ServerRole`'s `when` gains `OperatorToServer.MessageCase.NETWORK_STATE`, and
`ProxyRole`'s gains `OperatorToProxy.MessageCase.NETWORK_STATE`. Both call
`mirror.apply(...)` and return `Directive.None`.

**`ServerRole`'s `when` has a comment saying `:common`'s `FakeRole` copies it
by hand and that nothing enforces the copy.** Read that comment and update
`FakeRole` too, or the loop tests go on exercising a mapping production no
longer has.

- [ ] **Step 4: Run both suites and commit**

```bash
git add agent
git commit -m "$(cat <<'EOF'
feat(agent): both roles apply the network state

The mirror is the last constructor parameter on both roles, for the reason
FakePlayer's uuid is: every call site in these packages passes positionally,
and a parameter added above rebinds them -- silently, where the types match.

ServerRole's `when` carries a comment saying :common's FakeRole copies it by
hand and that nothing enforces the copy. This change updates both, because
the alternative is a loop test that goes on exercising a mapping production
no longer has.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 4: Both plugins install it

**Files:**
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt`

**Interfaces:**
- Consumes: `MirrorApi` from Task 2, `Spawnery.install`/`uninstall` from 7b-1.
- Produces: no new type. Each plugin builds its own `Self` and installs.

- [ ] **Step 1: Build each platform's `Self`**

Paper's `ServerSelf` and Velocity's `ProxySelf` come from the environment the
plugin already reads — `ServerEnvironment`/`ProxyEnvironment` hold the pod
name, the group and the namespace. Read those two classes first and use what
they already parse; do not add a second reader of the same environment
variables.

If a value is genuinely absent there, **do not invent one**. Say so in the
task's commit and leave the field empty rather than guessing a pod name from a
hostname.

- [ ] **Step 2: Install on enable, uninstall on disable**

`Spawnery.install` refuses a second implementation, so a plugin that enabled
twice without uninstalling would throw on the second. Both plugins call
`Spawnery.uninstall()` in their shutdown path, beside where they already stop
the session loop.

**Install after the mirror exists and before the session loop starts.** A
plugin whose own enable runs between those two would otherwise get an API
whose mirror is empty and no way to know it is about to fill.

- [ ] **Step 3: Build the agents**

```bash
git add agent
```

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make agent`

Expected: green, both JUnit suites included.

- [ ] **Step 4: Commit**

```bash
git commit -m "$(cat <<'EOF'
feat(agent): both plugins install the API

Spawnery.api() answers on a real server now. Each plugin builds its own Self
from the environment it already parses -- no second reader of the same
variables -- and installs the shared MirrorApi.

Installed after the mirror exists and before the session loop starts: a
plugin enabling between those two points would otherwise hold an API whose
mirror is empty with no way to know it is about to fill.

Uninstalled on shutdown, because install refuses a second implementation and
a plugin that enabled twice would otherwise throw on the second.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 5: Answer the permission question in the docs

**Files:**
- Modify: `docs/network-boundaries.md`
- Modify: `agent/api/README.md`

- [ ] **Step 1: Replace the open question with its answer**

`docs/network-boundaries.md` ends with "What is **not** yet decided is whether
a *plugin* should need a permission to read the roster." Replace that paragraph
with the answer this plan's own section gives: no permission is possible,
because neither platform has an identity for a plugin to check; the boundary
is who may install one, which is the boundary the chart's README already
documents.

- [ ] **Step 2: Say the same thing where a plugin author reads**

`agent/api/README.md`'s "What it can see" section gains one sentence: reads
need no permission, and the reason — a plugin runs with the server's
authority, and anyone who can install one can already read the server's own
player list.

- [ ] **Step 3: Commit**

```bash
git add docs/network-boundaries.md agent/api/README.md
git commit -m "$(cat <<'EOF'
docs: a plugin needs no permission, and cannot be given one

7b-3 left this open and it turns out not to be a choice. Bukkit permissions
attach to a CommandSender and Velocity's to a CommandSource; both are about a
player or the console, and neither platform has a Plugin.hasPermission. A
plugin calling Spawnery.api() presents no identity to check.

A gate would have to be invented -- a registry of trusted plugin ids -- and
would be worth nothing, because any plugin on the server can already read the
platform's own player list and call whatever the JVM exposes.

So the boundary is the one already documented: who may install a plugin,
which is who may create a pod in the namespace, which the chart's README
already tells an operator to treat as one trust domain.

The /cloud command is the different case and does gate, because a command has
a CommandSource to check.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Done when

- [ ] `make agent` passes, both JUnit suites included
- [ ] `make test` passes
- [ ] `make lint` reports nothing
- [ ] `grep -rn 'ProxySelf\|ServerSelf' agent/common/src/main` finds nothing:
      the shared implementation must not branch on which side it is on
- [ ] Nothing was pushed and no tag was created

## What 7b-4 leaves, and one gap worth naming

**Nothing proves this works in a real image.** Every test here is JUnit against
a fake state. What no test in this milestone can observe is whether
`Spawnery.install` actually runs inside a shipped jar on a real platform —
that needs a second plugin in the test image calling `Spawnery.api()`, which is
its own build problem and its own plan.

`hack/agent-test.sh` cannot close it either: it observes the stub's view of the
agent, and installing an API leaves no trace on the wire.

**7b-5 closes it as a side effect**, and that is the honest reason not to build
a probe plugin here: `/cloud list` is a command a person runs against a real
image, and it answers from this API. A phase that runs it is the first thing
that proves any of this loaded.

Until then, this milestone's claim is "the logic is right", not "it runs".
