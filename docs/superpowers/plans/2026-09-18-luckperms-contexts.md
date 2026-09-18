# LuckPerms contexts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every Spawnery pod reports what it is to LuckPerms, so a permission
rule can say where it applies.

**Architecture:** One pure function in `agent/common` turns the `Self` the agent
already built into a map of contexts; one `StaticContextCalculator` next to it
hands that map to LuckPerms; each agent calls one guarded registration line in
the branch where it installs the API. LuckPerms is `compileOnly` and optional
at runtime — the plugin descriptors carry the optionality, and a class probe
keeps a server without LuckPerms from losing its agent.

**Tech Stack:** Kotlin, Gradle (the `agent/` root), `net.luckperms:api:5.5`
(`compileOnly`), JUnit 5, Nix for the build.

**Spec:** `docs/superpowers/specs/2026-09-18-luckperms-contexts-design.md`

## Global Constraints

- **Everything in git is English** — code, comments, commit messages, docs.
- **Comments must earn their place.** The default is no comment. Write one only
  for what the code cannot say: foreign systems' behaviour, why something is
  *absent*, a number that would otherwise look arbitrary. No comment that
  retells the next lines, and no history of a bug.
- **Conventional Commits with a scope**: `feat(agent): …`, `docs(guides): …`.
  Subject says what changed, body says why, wrapped at 72 columns. End every
  commit message with `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **Every build and test command runs in the Nix dev shell**, with the full
  flag prefix and the flake as an argument, never a `cd` before it:
  `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c <command>`.
  The plan writes `nix develop -c <command>` for short; expand it.
- **Nix builds read the git index.** `git add` every new file *before* running
  `make agent` or any image build, or the compiler will report a symbol that is
  plainly in the file.
- **This machine is `paul-desktop`** (32 cores, 93 GB): no `-p 1`, no throttling.
- **Do not touch `flake.nix`.** Version numbers move in their own
  `chore: 0.x.y, …` release commit, never on a feature branch. See "After the
  plan" at the end.
- The branch is `feat/luckperms-contexts`, already created, with the spec
  committed on it.

---

### Task 1: The context map

The pure half: what the four keys are, and when `server` is among them. It
names no LuckPerms type, so it needs no dependency and no server to test.

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/LuckPermsContexts.kt`
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/LuckPermsContextsTest.kt`

**Interfaces:**
- Consumes: `cloud.spawnery.agent.api.Self`, `ServerSelf`, `ProxySelf` — already
  on `:common`'s compile classpath through `api(project(":api"))`. `Self` has
  `name()`, `group()`, `network()`; `ServerSelf` adds `slots(): Int`;
  `ProxySelf` adds nothing.
- Produces: `object LuckPermsContexts` with
  `const val UNCONFIGURED: String` (= `"global"`) and
  `fun of(self: Self, luckPermsServerName: String): Map<String, String>`.
  Task 2 adds `registerIfPresent` to the same object; Task 3 calls it.

- [ ] **Step 1: Write the failing test**

Create `agent/common/src/test/kotlin/cloud/spawnery/agent/LuckPermsContextsTest.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.ServerSelf
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse

private fun backend(
    pod: String = "lobby-7f3a",
    inGroup: String = "lobby",
    inNetwork: String = "cyperia",
) = object : ServerSelf {
    override fun name(): String = pod
    override fun group(): String = inGroup
    override fun network(): String = inNetwork
    override fun slots(): Int = 20
}

private fun proxy(
    pod: String = "edge-2c11",
    inGroup: String = "edge",
    inNetwork: String = "cyperia",
) = object : ProxySelf {
    override fun name(): String = pod
    override fun group(): String = inGroup
    override fun network(): String = inNetwork
}

class LuckPermsContextsTest {
    @Test
    fun `a backend reports its pod, group, network and platform`() {
        assertEquals(
            mapOf(
                "server" to "lobby-7f3a",
                "group" to "lobby",
                "network" to "cyperia",
                "environment" to "paper",
            ),
            LuckPermsContexts.of(backend(), LuckPermsContexts.UNCONFIGURED),
        )
    }

    @Test
    fun `a proxy is the velocity environment and names its own pod`() {
        assertEquals(
            mapOf(
                "server" to "edge-2c11",
                "group" to "edge",
                "network" to "cyperia",
                "environment" to "velocity",
            ),
            LuckPermsContexts.of(proxy(), LuckPermsContexts.UNCONFIGURED),
        )
    }

    @Test
    fun `a configured LuckPerms server name is left to stand alone`() {
        val contexts = LuckPermsContexts.of(backend(), "lobby")

        assertFalse("server" in contexts, "it overwrote a configured name: $contexts")
        assertEquals(
            mapOf("group" to "lobby", "network" to "cyperia", "environment" to "paper"),
            contexts,
        )
    }

    @Test
    fun `a variable the pod does not carry is left out rather than sent empty`() {
        val contexts = LuckPermsContexts.of(
            backend(inGroup = "", inNetwork = ""),
            LuckPermsContexts.UNCONFIGURED,
        )

        assertEquals(mapOf("server" to "lobby-7f3a", "environment" to "paper"), contexts)
    }
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run:

```bash
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery \
  -c gradle -p agent :common:test --tests '*LuckPermsContextsTest*'
```

Expected: FAIL at compile time — `unresolved reference: LuckPermsContexts`.

If `gradle -p agent` is not on the dev shell's PATH, build the agents through
Nix instead (`git add` the two files first, see Global Constraints):

```bash
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make agent
```

That runs both plugins' JUnit suites as part of the derivation. Use whichever
of the two works; the rest of this plan writes the Gradle form for brevity.

- [ ] **Step 3: Write the implementation**

Create `agent/common/src/main/kotlin/cloud/spawnery/agent/LuckPermsContexts.kt`:

```kotlin
/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.Self

/**
 * What this pod tells LuckPerms about itself, so that a permission rule can
 * say where it applies.
 *
 * The values are pod-wide and never per player, which is what lets the
 * calculator answer without being given anybody.
 */
object LuckPermsContexts {
    /** What LuckPerms reports when nobody has configured a server name. */
    const val UNCONFIGURED = "global"

    /**
     * @param luckPermsServerName LuckPerms' own configured server name.
     *   [UNCONFIGURED] means nobody set one, and only then is `server` filled
     *   in: `server` is LuckPerms' own key, and a second value for it would
     *   leave both an administrator's rule and ours matching.
     */
    fun of(self: Self, luckPermsServerName: String): Map<String, String> {
        val contexts = LinkedHashMap<String, String>()
        if (luckPermsServerName == UNCONFIGURED) {
            contexts.putIfCarried("server", self.name())
        }
        contexts.putIfCarried("group", self.group())
        contexts.putIfCarried("network", self.network())
        // The plugin API this agent runs against and not the jar, so a Purpur
        // server reports `paper`: a grant must not change meaning when
        // somebody swaps Purpur for Paper.
        contexts["environment"] = if (self is ProxySelf) "velocity" else "paper"
        return contexts
    }

    // Not named `set`: the standard library already has that extension on
    // MutableMap, and two of them in scope with the same signature is a
    // resolution question nobody should have to answer while reading this.
    private fun MutableMap<String, String>.putIfCarried(key: String, value: String) {
        if (value.isNotBlank()) this[key] = value
    }
}
```

- [ ] **Step 4: Run the test and watch it pass**

Run:

```bash
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery \
  -c gradle -p agent :common:test --tests '*LuckPermsContextsTest*'
```

Expected: PASS, four tests.

- [ ] **Step 5: Commit**

```bash
git add agent/common/src/main/kotlin/cloud/spawnery/agent/LuckPermsContexts.kt \
        agent/common/src/test/kotlin/cloud/spawnery/agent/LuckPermsContextsTest.kt
git commit -F - <<'EOF'
feat(agent): the contexts a pod reports about itself

Four keys, built from the Self the agent already holds rather than from a
second reading of the pod's environment: server, group, network and the
platform. The server key is LuckPerms' own, so it is filled in only while
LuckPerms still reports the unconfigured name -- an administrator who set
one would otherwise find two values under it and both rules matching.

A pure function, deliberately: it names no LuckPerms type, so the part
that decides is testable without a server and without the API on the
classpath.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 2: The calculator, and a guard that cannot cost the agent

The half that touches LuckPerms. The guard is the point of the task: without
it, a server that has no LuckPerms hits `NoClassDefFoundError` inside
`onEnable`, Paper disables the plugin, and the pod loses its cloud connection
over a permission feature nobody asked for.

**Files:**
- Modify: `agent/common/build.gradle.kts` (the `dependencies` block)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/LuckPermsContexts.kt`
- Modify: `agent/common/src/test/kotlin/cloud/spawnery/agent/LuckPermsContextsTest.kt`
- Modify: `agent/deps.json` (generated, never hand-edited)

**Interfaces:**
- Consumes: `LuckPermsContexts.of(self, luckPermsServerName)` from Task 1.
- Produces: `fun LuckPermsContexts.registerIfPresent(self: Self, log: (String) -> Unit)`
  — Task 3 calls exactly this, once per platform.

- [ ] **Step 1: Write the failing test**

Append to `agent/common/src/test/kotlin/cloud/spawnery/agent/LuckPermsContextsTest.kt`,
inside the existing class, and add `import kotlin.test.assertTrue` and
`import kotlin.test.assertFailsWith` at the top:

```kotlin
    @Test
    fun `a pod without LuckPerms is silence and not a crash`() {
        // The precondition is asserted rather than assumed: this test proves
        // the absent path only while LuckPerms is off the test classpath, and
        // adding it as a test dependency would otherwise turn this green for
        // the opposite reason.
        assertFailsWith<ClassNotFoundException> {
            Class.forName("net.luckperms.api.LuckPermsProvider")
        }
        val said = mutableListOf<String>()

        LuckPermsContexts.registerIfPresent(backend(), said::add)

        assertTrue(said.isEmpty(), "it spoke without LuckPerms: $said")
    }
```

- [ ] **Step 2: Run the test and watch it fail**

Run:

```bash
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery \
  -c gradle -p agent :common:test --tests '*LuckPermsContextsTest*'
```

Expected: FAIL at compile time — `unresolved reference: registerIfPresent`.

- [ ] **Step 3: Add the dependency**

In `agent/common/build.gradle.kts`, inside `dependencies { … }`, directly above
the `testImplementation(kotlin("test"))` line:

```kotlin
    // compileOnly and deliberately not on the test classpath: the plugin is
    // optional at runtime, and a test that calls registerIfPresent with no
    // LuckPerms to find is the only thing that proves the guard.
    compileOnly("net.luckperms:api:5.5")
```

- [ ] **Step 4: Write the implementation**

In `agent/common/src/main/kotlin/cloud/spawnery/agent/LuckPermsContexts.kt`,
add to the imports:

```kotlin
import net.luckperms.api.LuckPermsProvider
import net.luckperms.api.context.ContextConsumer
import net.luckperms.api.context.ContextSet
import net.luckperms.api.context.ImmutableContextSet
import net.luckperms.api.context.StaticContextCalculator
```

and add to the `LuckPermsContexts` object, after `of`:

```kotlin
    /**
     * Registers this pod's contexts with LuckPerms, if there is a LuckPerms.
     *
     * Probed rather than caught. Without the plugin the class below cannot
     * resolve, and the NoClassDefFoundError would leave `onEnable` through
     * Paper's plugin manager, which disables the agent -- so a server with no
     * permission plugin would lose the cloud over a permission feature.
     *
     * @param log where to say what was registered. Nothing is said when there
     *   is no LuckPerms, which is the ordinary case.
     */
    fun registerIfPresent(self: Self, log: (String) -> Unit) {
        try {
            Class.forName("net.luckperms.api.LuckPermsProvider")
        } catch (_: ClassNotFoundException) {
            return
        }
        register(self, log)
    }

    private fun register(self: Self, log: (String) -> Unit) {
        val luckPerms = LuckPermsProvider.get()
        val contexts = ImmutableContextSet.builder()
        for ((key, value) in of(self, luckPerms.serverName)) {
            contexts.add(key, value)
        }
        val set = contexts.build()
        luckPerms.contextManager.registerCalculator(Calculator(set))
        log("spawnery LuckPerms contexts: $set")
    }

    /**
     * One fixed answer for everybody on this pod -- a player is never looked
     * at, which is what [StaticContextCalculator] is for.
     */
    private class Calculator(private val contexts: ImmutableContextSet) : StaticContextCalculator {
        override fun calculate(consumer: ContextConsumer) = consumer.accept(contexts)

        override fun estimatePotentialContexts(): ContextSet = contexts
    }
```

- [ ] **Step 5: Run the test and watch it pass**

Run:

```bash
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery \
  -c gradle -p agent :common:test --tests '*LuckPermsContextsTest*'
```

Expected: PASS, five tests.

- [ ] **Step 6: Regenerate the dependency lockfile**

`agent/deps.json` is the Maven lockfile the Nix build reads. It has to be
regenerated whenever a `build.gradle.kts` dependency changes; the target
reaches Maven Central, is part of no other target, and CI diffs the result.
It only does the right thing from the repository root:

```bash
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make agent-deps
git diff --stat agent/deps.json
```

Expected: `agent/deps.json` gains entries for `net.luckperms:api:5.5`. If the
diff is empty, the dependency did not land — re-read Step 3 before going on.

- [ ] **Step 7: Build both agents through Nix**

`git add` first: the Nix build reads the git index, and an unadded file does
not exist for it.

```bash
git add agent/common/build.gradle.kts agent/deps.json \
        agent/common/src/main/kotlin/cloud/spawnery/agent/LuckPermsContexts.kt \
        agent/common/src/test/kotlin/cloud/spawnery/agent/LuckPermsContextsTest.kt
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make agent
```

Expected: both plugins build and both JUnit suites pass. This also runs
`hack/agent-jar-check.sh`, which fails on any class in the jar outside
`cloud/spawnery/agent/` — a `compileOnly` dependency is not bundled, so it
must stay green. If it names a `net/luckperms/` class, the dependency was
written as `implementation` rather than `compileOnly`.

- [ ] **Step 8: Commit**

```bash
git commit -F - <<'EOF'
feat(agent): hand the contexts to LuckPerms when there is a LuckPerms

A StaticContextCalculator over the map from the previous commit, and a
class probe in front of it. The probe is the substance: without LuckPerms
the API cannot resolve, and the NoClassDefFoundError would leave onEnable
through Paper's plugin manager and disable the agent -- a server with no
permission plugin would lose the cloud over a permission feature.

The API is compileOnly, so nothing of it is bundled and
hack/agent-jar-check.sh stays green. It is kept off the test classpath on
purpose: the new test calls the registration with no LuckPerms to find,
which is the only thing that can prove the probe.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 3: Both agents register it

One line per platform, plus the descriptor entries that give ordering when
LuckPerms is there and silence when it is not.

**Files:**
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt` (after the `logger.info("spawnery API installed …")` call in the `Environment.Configured` branch of `onEnable`)
- Modify: `agent/paper/src/main/resources/paper-plugin.yml`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt` (after `Spawnery.install(api)`, around line 258)
- Modify: `agent/velocity/src/main/resources/velocity-plugin.json`

**Interfaces:**
- Consumes: `LuckPermsContexts.registerIfPresent(self, log)` from Task 2, and
  the `self` value both plugins already build (`ServerSelf` on Paper,
  `ProxySelf` on Velocity).
- Produces: nothing further tasks rely on.

- [ ] **Step 1: Register on Paper**

In `agent/paper/…/AgentPlugin.kt`, add to the imports:

```kotlin
import cloud.spawnery.agent.LuckPermsContexts
```

and insert directly after the `logger.info("spawnery API installed for network …")`
call, inside the `is Environment.Configured ->` branch:

```kotlin
                // In this branch and not beside it: a dormant agent is a pod
                // that is not ours, and it should not be labelling anybody's
                // permissions.
                LuckPermsContexts.registerIfPresent(self, logger::info)
```

- [ ] **Step 2: Declare LuckPerms optional to Paper**

Replace the whole of `agent/paper/src/main/resources/paper-plugin.yml` with:

```yaml
name: SpawneryAgent
version: '${version}'
main: cloud.spawnery.agent.paper.AgentPlugin
api-version: '26.2'
authors: [paulwtf]
description: Reports readiness and player counts to the Spawnery operator.
dependencies:
  server:
    LuckPerms:
      load: BEFORE
      required: false
```

`required: false` is what keeps a server without LuckPerms loading this plugin
at all; `load: BEFORE` is what makes `LuckPermsProvider.get()` answer rather
than throw when it is there.

- [ ] **Step 3: Register on Velocity**

In `agent/velocity/…/AgentPlugin.kt`, add to the imports:

```kotlin
import cloud.spawnery.agent.LuckPermsContexts
```

and insert directly after `Spawnery.install(api)`:

```kotlin
        LuckPermsContexts.registerIfPresent(self, logger::info)
```

- [ ] **Step 4: Declare LuckPerms optional to Velocity**

In `agent/velocity/src/main/resources/velocity-plugin.json`, replace the empty
`"dependencies": []` with:

```json
  "dependencies": [
    {
      "id": "luckperms",
      "optional": true
    }
  ],
```

This file and not the `@Plugin` annotation: the annotation in `AgentPlugin.kt`
is read by nothing, as its own comment at line 67 says. The hand-written
descriptor is the one Velocity loads.

- [ ] **Step 5: Build both agents**

```bash
git add -u agent
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make agent
```

Expected: both plugins build, both JUnit suites pass, `hack/agent-jar-check.sh`
green. A failure naming `logger::info` as ambiguous means the overload could
not be picked — replace the method reference with `{ logger.info(it) }` on that
platform and note it in the commit.

- [ ] **Step 6: Commit**

```bash
git commit -F - <<'EOF'
feat(agent): both sides report their contexts

The registration sits in the branch that installs the API, so a dormant
agent registers nothing -- a pod that is not ours has no business
labelling anybody's permissions, and this needs no check of its own to
get that.

The optionality is in the descriptors rather than in the code: Paper
takes required:false with load:BEFORE, Velocity an optional dependency
in velocity-plugin.json, which is the descriptor it actually reads --
the @Plugin annotation beside it is read by nothing.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

### Task 4: Say so in the guide

`docs/guides/cloud-command.md` is where an administrator already meets
LuckPerms — it says Spawnery ships no permission system and shows an `/lp`
line. The contexts belong there, not on a page of their own.

**Files:**
- Modify: `docs/guides/cloud-command.md`

**Interfaces:**
- Consumes: the four keys from Task 1.
- Produces: nothing.

- [ ] **Step 1: Read the page first**

```bash
cat docs/guides/cloud-command.md
```

The new section goes after "The four nodes" table and before whatever follows
it. Place it where the reading flows; the text below is what it says.

- [ ] **Step 2: Write the section**

````markdown
## Where a permission applies

Every Spawnery pod tells LuckPerms what it is, so a rule can name a place:

| Context | Value |
|---|---|
| `group` | the `ServerGroup` or `ProxyGroup` |
| `network` | the `Network` |
| `environment` | `paper` on a backend, `velocity` on a proxy |
| `server` | the pod's own name |

So the moderators above can be given the reading half on the lobbies alone:

```text
/lp group moderator permission set spawnery.cloud.read true group=lobby
```

`group` is the one to reach for. `server` is a pod name and changes every time a
server is replaced, so it says where a player is rather than granting anything
— and it is filled in only while LuckPerms' own config still says
`server: global`.

Nothing has to be switched on. The contexts appear when LuckPerms is installed
and nothing happens when it is not.
````

- [ ] **Step 3: Check the page against the documentation guards**

```bash
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make test
```

Expected: PASS. `make test` runs the pin, length and docs linters along with
the Go suite. `hack/docs-length.sh` carries no ceiling for this page, so the
addition cannot fail it; if it does, the table in that script has gained an
entry and the ceiling needs a line and a sentence in the commit saying what
the page gained.

- [ ] **Step 4: Commit**

```bash
git add docs/guides/cloud-command.md
git commit -F - <<'EOF'
docs(guides): where a permission applies

The page already meets LuckPerms -- it is where the /lp line granting the
read node lives -- so the contexts belong beside it rather than on a page
of their own.

It says to reach for group and not for server, because the second is a
pod name: it changes with every replacement, and a grant written against
it stops applying the moment the server it named is gone.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
```

---

## What only a cluster can prove

The tests above cover the decision. They cannot see the descriptors, the load
order, or whether LuckPerms accepted the calculator — nothing on a JUnit
classpath can. These steps are Paul's, on a network with LuckPerms installed
through an `extraPlugins` claim:

1. Bring up a server from a group with LuckPerms in its `extraPlugins` claim
   and join it.
2. `/lp user <name> info` — the current contexts must list `server`, `group`,
   `network` and `environment`, with `environment=paper`.
3. `/lp group default permission set some.node true group=<that group>`, then
   confirm the node applies on that group's servers and not on another group's.
4. On a proxy: the same `/lp user <name> info` must show `environment=velocity`
   and the proxy pod's own name under `server`.
5. The negative case, which is the one the guard exists for: a group **without**
   LuckPerms must still come up `Ready` and its agent must still connect.
   `kubectl logs` on such a pod must show `spawnery API installed …` and no
   LuckPerms line at all.

## After the plan

`imageVersion` in `flake.nix` moves when this is released — anything under
`agent/` moves it — and this is **a minor step**, being new behaviour by the
rule decided on 2026-09-08. `operatorVersion` does not move. The chart's
`image.tag` follows `imageVersion`, so the release commit also runs
`make manifests` and carries the resulting diff in
`docs/reference/chart-values.md`. None of that belongs on this branch: version
numbers move in their own `chore: 0.x.y, …` commit.
