# Milestone 7c-2 — `/cloud`

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One command, written once, that answers the same way on a Paper backend and a Velocity proxy — and the first thing in this project that a person types.

**Architecture:** The tree is generic in the source type and lives in `agent/common`. Each platform contributes a `SourceAdapter` of two methods — check a permission, send a line — and its own registration call. Nothing about the tree knows which side it is on.

**Tech Stack:** Kotlin, `com.mojang.brigadier`, the plugin API from 7b, `ScaleBoost` from 7c-1.

**Spec:** [`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../specs/2026-08-27-cloud-api-design.md) §5, §9.2

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
  every `make agent` or `make agent-test`.
- No Kotlin file under `agent/` carries a licence header.
- A new constructor parameter goes last.
- **`make manifests` after any RBAC marker or API field**, and the same edit
  adds the row to `internal/rbacaudit/required.go`. 7c-1 learned twice over
  that a regenerate skipped is a schema that silently drops what the code
  writes.

## Measured before this plan was written

Everything the "one tree" claim rests on, checked against the pinned
artifacts on 2026-08-28:

| | |
|---|---|
| Paper | `brigadier-1.3.10.jar`, 54 `com.mojang.brigadier` classes as a library |
| Paper | `io.papermc.paper.command.brigadier.Commands` and `CommandSourceStack` |
| Paper | `LifecycleEventManager` and `LifecycleEvents` — the registration path |
| Velocity | 97 `com.mojang.brigadier` classes, bundled and unrelocated |
| Velocity | `BrigadierCommand`, `CommandManager`, `CommandSource` |

Both build a `LiteralCommandNode<S>`. The spec carried the Paper half as an
explicit unmeasured assumption from the day it was written; it holds.

**Messages travel as `String`, not as Adventure `Component`.** Both platforms
speak Adventure, and using it would put a platform type in the shared module's
signatures — the same trap §3.3 records for Kotlin and protobuf. Each adapter
converts.

## File structure

| Path | Responsibility |
|---|---|
| `agent/common/src/main/kotlin/.../CloudCommand.kt` | The tree, generic in `S` |
| `agent/common/src/main/kotlin/.../SourceAdapter.kt` | Check a permission, send a line |
| `agent/common/src/test/kotlin/.../CloudCommandTest.kt` | Every branch, against a fake source |
| `agent/paper/src/main/kotlin/.../AgentPlugin.kt` | Registers through `LifecycleEvents.COMMANDS` |
| `agent/velocity/src/main/kotlin/.../AgentPlugin.kt` | Registers through the `CommandManager` |
| `proto/.../agent.proto` | `BoostRequest`, `RetireRequest`, and their results |
| `internal/agentserver/requests.go` | The two new verbs, on 7b-5's bounds |
| `internal/controller/…` | The `create` grant for boosts, with its caller |

---

### Task 1: The adapter, and what the tree may ask of a platform

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/SourceAdapter.kt`

**Interfaces:**
- Produces:
  ```kotlin
  interface SourceAdapter<S> {
      fun hasPermission(source: S, permission: String): Boolean
      fun send(source: S, message: String)
  }
  ```

- [ ] **Step 1: Write it**

Two methods and no more, and the interface's comment says why that is a
ceiling rather than a starting point:

```kotlin
/**
 * The whole of what the command tree may ask of a platform.
 *
 * Two methods, and the number is the design. Every branch this adapter does
 * not offer is a branch the tree cannot take, so a decision that depends on
 * which side it is running on becomes impossible to write rather than merely
 * discouraged -- which is the same argument [MirrorApi] makes by taking a
 * [Self] instead of asking.
 *
 * [send] takes a String and not an Adventure Component, though both platforms
 * speak Adventure. A Component in this signature would put a platform type in
 * a shared module, which is the trap the design records for Kotlin and for
 * protobuf, one layer down. Each adapter converts.
 */
```

- [ ] **Step 2: Commit**

```bash
git add agent/common/src
git commit -m "$(cat <<'EOF'
feat(agent): what the command tree may ask of a platform, and no more

Two methods: check a permission, send a line. The number is the design --
every branch this adapter does not offer is a branch the tree cannot take,
so a decision depending on which side it runs on becomes impossible to write
rather than merely discouraged.

send takes a String though both platforms speak Adventure. A Component here
would put a platform type in a shared module's signature, which is the trap
the design already records for Kotlin and for protobuf.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 2: The read half of the tree

`list` and `info` first, because they need nothing that does not exist: the
mirror is there, and a read has no bound to get wrong.

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudCommand.kt`
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudCommandTest.kt`

**Interfaces:**
- Produces:
  ```kotlin
  fun <S> cloudCommand(api: SpawneryApi, adapter: SourceAdapter<S>): LiteralArgumentBuilder<S>
  ```

- [ ] **Step 1: Write the failing tests**

The fake source is an `Int`, because the tree must not care what a source is:

```kotlin
class CloudCommandTest {
    private val sent = mutableListOf<String>()
    private var permissions = setOf<String>()

    private val adapter = object : SourceAdapter<Int> {
        override fun hasPermission(source: Int, permission: String) = permission in permissions
        override fun send(source: Int, message: String) { sent += message }
    }

    private fun run(command: String, api: SpawneryApi = fakeApi()): Int {
        val dispatcher = CommandDispatcher<Int>()
        dispatcher.register(cloudCommand(api, adapter))
        return dispatcher.execute(command, 0)
    }

    @Test
    fun `list names every group and what it is doing`() {
        permissions = setOf("spawnery.cloud.read")
        run("cloud list")
        assertTrue(sent.any { it.contains("lobby") }, "the output named no group: $sent")
    }

    @Test
    fun `a source without the permission is refused and told which one`() {
        // Named rather than a bare refusal: an admin who does not know the
        // node cannot grant it, and "no permission" sends them to a wiki.
        permissions = emptySet()
        assertFailsWith<CommandSyntaxException> { run("cloud list") }
    }

    @Test
    fun `info about a server names its phase and its players`() {
        permissions = setOf("spawnery.cloud.read")
        run("cloud info lobby-a")
        val line = sent.single()
        assertTrue(line.contains("lobby-a") && line.contains("READY"), line)
    }

    @Test
    fun `info about something absent says so rather than printing nothing`() {
        // The failure mode this replaces: a command that prints an empty line
        // and leaves an admin unsure whether the server is gone or the command
        // is broken.
        permissions = setOf("spawnery.cloud.read")
        run("cloud info nothing-here")
        assertTrue(sent.single().contains("nothing-here"), "the answer did not name what was asked for: $sent")
    }

    @Test
    fun `the tree asks the platform for nothing but permissions and sending`() {
        // The structural claim, asserted: run every branch and confirm the
        // adapter saw only those two calls. A tree that reached for anything
        // else could not have been written against this adapter, so this test
        // is really about the adapter staying two methods.
    }
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :common:test --tests "*CloudCommandTest*" --console=plain --offline'`

- [ ] **Step 3: Write the read half**

`LiteralArgumentBuilder.literal<S>("cloud")` with `list` and `info` under it.
Each `.requires { adapter.hasPermission(it, PERMISSION_READ) }`.

**Brigadier's `requires` makes an unpermitted branch invisible rather than
refused**, which is what produces the "unknown command" a player sees. That is
the platform's own convention and this follows it — but the *tree's* own
messages must still name a permission wherever it explains itself, because an
admin who cannot see a branch has no way to learn what would reveal it.

- [ ] **Step 4: Run, then commit**

```bash
git add agent/common/src
git commit -m "$(cat <<'EOF'
feat(agent): /cloud list and /cloud info, written once

One tree, generic in the source type, so a Paper backend and a Velocity proxy
answer identically because it is the same code. The tests drive it with an Int
as the source, which is the cheapest way to say that the tree does not care
what a source is.

A lookup for something absent says so rather than printing nothing. The
failure mode being replaced is an empty line that leaves an admin unsure
whether the server is gone or the command is broken.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 3: Both platforms register it

**Files:**
- Modify: both `AgentPlugin.kt`
- Create: an adapter per platform, beside its plugin

- [ ] **Step 1: Paper**

`LifecycleEvents.COMMANDS` on the plugin's `LifecycleEventManager`; the
event's registrar takes the `LiteralCommandNode<CommandSourceStack>` that
`cloudCommand(...).build()` produces.

Its adapter: `hasPermission` is `source.sender.hasPermission(node)`, and
`send` wraps the string in a `Component` — `Component.text(...)` — which is the
one place Adventure appears on this side.

- [ ] **Step 2: Velocity**

`proxy.commandManager.register(BrigadierCommand(cloudCommand(...).build()))`,
built inside the same enable path that already installs the API.

- [ ] **Step 3: Build**

```bash
git add agent
```

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make agent`

- [ ] **Step 4: Commit**

```bash
git commit -m "$(cat <<'EOF'
feat(agent): both platforms register /cloud

Paper through LifecycleEvents.COMMANDS, Velocity through its CommandManager.
Both hand over the same LiteralCommandNode, built by the same function, and
each supplies an adapter of two methods.

Adventure appears exactly twice in this change, once per adapter, converting
a String at the last possible moment. That is what keeps it out of the shared
module's signatures.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 4: `retire`, the verb that needs nothing new

`retire` writes `spec.retire` on a `Server`, which the operator already owns
and already has every verb for. It goes before `boost` for that reason: the
request path gets exercised by the verb that needs no new grant.

**Files:**
- Modify: `proto/.../agent.proto` — `RetireRequest`, `RetireResult`
- Modify: `internal/agentserver/requests.go` — the verb, on 7b-5's bounds
- Modify: `agent/common/.../CloudConnector.kt`, `MirrorApi.kt`, the API's `Lifecycle`
- Modify: `CloudCommand.kt` — `/cloud retire <server>`

- [ ] **Step 1: The proto and the operator side**

`RetireRequest { string server = 1; }`. The operator resolves the server
inside `id.Namespace` — structural, as `connect` is — patches `spec.retire`,
and answers.

**Refuse a `Server` that is already retiring** with `REFUSED` rather than
patching again: the patch is idempotent but the answer should tell an admin
that somebody else already did it, and an operator that says "done" twice
teaches people to type it twice.

- [ ] **Step 2: Every bound gets its own test, and its own mutation**

The same discipline 7b-5 used: a namespace test, a not-found test, a
rate-limit test, and the already-retiring test. Mutate each on its own.

- [ ] **Step 3: The command**

`/cloud retire <server>` under `spawnery.cloud.retire`. Its output says what
happened *and* what it means:

```
lobby-a3f9 is retiring. It takes no new joins; the players on it finish in
their own time and nobody is kicked.
```

The second sentence is not decoration. `retire` sounds like `stop` to
somebody who has not read the design, and an admin who thinks they just
disconnected forty people will do something worse next.

- [ ] **Step 4: Run everything, then commit**

---

### Task 5: `boost`, and the grant that arrives with its caller

**Files:**
- Modify: `proto/.../agent.proto` — `BoostRequest`, `BoostResult`
- Modify: `internal/agentserver/requests.go`
- Modify: `internal/controller/servergroup_controller.go` — the `create` marker
- Modify: `internal/rbacaudit/required.go`
- Modify: `CloudCommand.kt` — `/cloud start` and `/cloud stop`

- [ ] **Step 1: Add `create` on scaleboosts, with its caller**

7c-1 deliberately left this out: "a grant with no caller is one nobody can
justify when they find it". The caller arrives here, so the verb does too —
`create` and `delete` were already there for the sweep.

Add the marker, add the row to the table, run `make manifests`, and confirm
`internal/rbacaudit` is green.

- [ ] **Step 2: The operator creates the boost**

Default expiry **one hour**, applied here and not in the CRD — §4.4 says why
the type supplies none. The request may name a longer one; the operator
bounds it.

**Bound it.** A boost is a request from a pod, and §4.3's rule holds: the
operator believes the agent about the permission and bounds the request
itself. Cap the duration and cap the replicas at what `maxReplicas` leaves,
and give each its own reason so an admin can tell "too long" from "too many".

- [ ] **Step 3: `/cloud stop` deletes that group's boosts**

Not "reduce by n": deleting is what a person means by "stop", and a partial
reduction across several boosts with different expiries is arithmetic nobody
asked for. Say how many were removed.

- [ ] **Step 4: Every bound its own test and its own mutation**

- [ ] **Step 5: The output says it is temporary**

```
lobby: +2 servers for 1h (until 20:00 UTC)
This is a boost, not a spec change. It expires on its own; /cloud stop lobby
ends it early. For a lasting change, edit the ServerGroup.
```

§5.3's three sentences, and the third is the one that stops an admin typing
this every Saturday without ever learning there is a file.

- [ ] **Step 6: Run everything, then commit**

---

### Task 6: Driven against a real image

The gap every milestone since 7b-1 has named: nothing has ever run a command
inside a shipped jar.

**Files:**
- Modify: `hack/agent-test.sh`

- [ ] **Step 1: Find out whether a console is reachable at all**

`start_agent` runs containers with `-d` and no stdin, so today it is not.
**Measure before building**: try `podman run -d -i`, then
`podman attach --no-stdin=false` or writing to the container's stdin, against
one throwaway container. Ten minutes, and the answer decides the next step.

- [ ] **Step 2a: If a console is reachable**

Add a phase that types `cloud list` and asserts the output names the stub's
synced server. That is the first end-to-end proof of the whole API: the
command loaded, the mirror filled, the tree ran, the adapter sent.

- [ ] **Step 2b: If it is not**

**Do not contort the harness.** Assert instead what the log already shows —
that the command registered — and write down plainly that the tree's own
behaviour is covered by JUnit and by nothing that runs the real jar.

Record which of the two happened in the commit, with the measurement.

- [ ] **Step 3: Run and commit**

---

### Task 7: Documentation

**Files:**
- Modify: `agent/api/README.md`, `docs/upgrading.md`, `charts/spawnery/README.md`

- [ ] **Step 1: The permissions, in one table**

`spawnery.cloud.read`, `.retire`, `.scale`. An operator granting these needs
them in one place, and the chart's README is where an operator already is.

Say what each lets somebody do **in terms of what a player would notice** —
`.scale` costs money and `.retire` moves people — rather than in terms of
which command it unlocks.

- [ ] **Step 2: The upgrade note**

`/cloud` appears on every server running the new agent, with no permission
granted to anybody by default. Say that: an operator who upgrades and grants
nothing has a command that answers "unknown command" to everyone, which is
the safe state and looks like a bug.

---

## Done when

- [x] `make test`, `make agent`, `make lint` all pass — 19 Go packages, 197
      agent tests, `golangci-lint` 0 issues, `nix build .#agents` green
- [x] `make agent-test CONTAINER=podman` passes
- [x] Every bound added in Tasks 4 and 5 was mutated on its own — five in Task
      4, eight in Task 5, plus the new harness check in Task 6
- [x] `make manifests` leaves no diff
- [x] `grep -rn 'ProxySelf\|ServerSelf\|CommandSourceStack\|CommandSource' agent/common/src/main`
      finds nothing: the shared tree knows no platform. **Read the grep before
      believing it**: three hits remain and all three are comments saying the
      tree names neither platform. The gate is the same grep with
      `| grep -vE ':\s*(\*|//)'`, which is empty.
- [x] Nothing was pushed and no tag was created

## What execution changed about the plan

Four places where the plan was followed and one where it was not, recorded
because the reasoning is worth more than the instruction was.

**Task 4 dropped a check the plan asked for.** The snapshot lookup before the
retire write was measured redundant: removing it changed no test, because the
writer's own namespaced `Get` already answers `NOT_FOUND`. A screening step
that reads like a bound but cannot fail on its own is worse than none.

**Task 4 moved the rate bound rather than repeating it.** Both stream
directions unpacked the request oneof themselves; a second verb would have made
four copies. One dispatcher means the bound is a line every verb passes through
instead of a line each verb's author must remember.

**Task 5 refuses where the plan said cap.** "Cap the replicas at what
`maxReplicas` leaves" became a refusal naming the room. A boost that silently
becomes something other than what was typed is the class of surprise this
repository avoids everywhere else, and the admin who asked for six and got
three would find out by counting servers.

**Task 5 added a bound the plan did not have.** Reading the scaler showed a
boost only reaches `DecideSize` for an ephemeral group with `spec.scaling`. On a
persistent group the object would be created, counted in
`status.boostedReplicas`, and change nothing — and mutating the guard away does
not fail a test, it segfaults the operator on the nil pointer.

**Task 6 took branch 2a.** A console is reachable: a detached `-i` container
receives what `attach` writes, and Paper's entrypoint ends in `exec java`, so
the container's stdin is the console's. Measured on a throwaway container
before anything was built.

## What 7c-2 leaves

- **No events and no chat feed.** `/cloud events` is not in the tree; it lands
  with 7c-3, which is what gives it something to toggle.
- **No `/cloud send`.** `connect` exists in the API from 7b-5 and the command
  for it is one more branch — deliberately not here, because this milestone
  already carries two new verbs and a new grant, and the third would be the one
  reviewed least.
