# Readiness Holds Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A server stops taking players while a plugin is still starting — by
honouring a door the server has already closed, and by letting a plugin hold
readiness until its own initialisation is finished.

**Architecture:** Two independent changes that share one prerequisite. The
operator's ready gate stops registering a server whose door is shut (Go,
`internal/phase`). The agent's public API grows a readiness hold that delays
the single `markReady()` transition (Java in `agent/api`, Kotlin in
`agent/common` and `agent/paper`). The prerequisite is moving the agent's
`ServerLoadEvent` handler to `EventPriority.MONITOR` so a plugin's own handler
has spoken first.

**Tech Stack:** Go 1.x with controller-runtime; Kotlin and Java on Gradle,
built through Nix (`make agent`); JUnit 5 for the agent, table tests for the
operator.

**Spec:** `docs/superpowers/specs/2026-09-05-readiness-holds-design.md`

## Global Constraints

- Every command runs inside the Nix dev shell: prefix with `nix develop -c`.
- **There is no `./gradlew`.** The agent has no wrapper, and the `gradle` in
  the dev shell cannot resolve Mojang's brigadier -- `nix build .#agents`
  supplies those through `agent/deps.json` and a mitmCache. So every agent
  suite runs through `nix develop -c make agent`, which builds both plugins
  and runs every JUnit suite. `:api` is the exception: it depends on nothing
  but JUnit, so `cd agent && nix develop .. -c gradle :api:test` works and is
  seconds rather than minutes.
- A failing Nix build prints a store path. `nix-store -l <path>` is the full
  log, and the only place the test names appear.
- **Nix builds read the git index, not the working tree.** `git add` every new
  file before `make agent` or any image build. The symptom of forgetting is a
  compile error naming a symbol that is plainly in the file.
- Readiness is a one-way latch. `ServerState.markReady` is
  `compareAndSet(false, true)` and there is deliberately no way to clear it. A
  hold delays that single transition; nothing in this plan may reverse it.
- Comments only where the code cannot say it itself. A number that would look
  arbitrary, the behaviour of a foreign system, or why something is **not**
  there — those stay. Nothing else.
- Everything that lands in git is English.
- Commits are Conventional Commits with a scope: `fix(phase): …`,
  `feat(agent): …`.
- `internal/phase` has no client and no I/O. It stays a pure `Decide`.

---

### Task 1: The gate honours a closed door

Independently shippable and worth shipping whether or not the rest is built:
a server that has said "send me nobody" is registered anyway today, and
deregistered on the next pass.

**Files:**
- Modify: `internal/phase/phase.go:567-574`
- Test: `internal/phase/phase_test.go`

**Interfaces:**
- Consumes: `Inputs.JoinsClosed` (already exists, `phase.go:301`), fed from
  `!snap.AcceptingJoins` in `internal/controller/server_controller.go:731`.
- Produces: nothing new. `Decision.Register` already exists and the `Ready`
  branch already registers a server whose door opens later.

- [ ] **Step 1: Write the failing test**

Add to `internal/phase/phase_test.go`, beside the other door tests near
`TestAClosedDoorIsDeregisteredOnceAndNotOnEveryPass`:

```go
func TestTheGateDoesNotRegisterAServerWhoseDoorIsShut(t *testing.T) {
	// A plugin that closes the door while starting must not see the server
	// registered for one pass and deregistered on the next: those seconds are
	// the window this exists to close.
	in := Inputs{
		PodExists: true, PodRunning: true, PodReady: true, AgentReady: true,
		JoinsClosed: true,
	}

	got := Decide(Starting, in)

	if got.Next != Ready {
		t.Errorf("Next = %q, want %q: the server is ready, it is only closed",
			got.Next, Ready)
	}
	if got.Register {
		t.Error("Register = true, want false while the door is shut")
	}
	if got.Reason != ReasonJoinsClosed {
		t.Errorf("Reason = %q, want %q so a reader can tell why nobody arrives",
			got.Reason, ReasonJoinsClosed)
	}
}

func TestTheGateRegistersWhenNobodyClosedTheDoor(t *testing.T) {
	// The default matters more than the new branch: a network that never calls
	// acceptJoins must decide exactly what it decided before.
	in := Inputs{PodExists: true, PodRunning: true, PodReady: true, AgentReady: true}

	got := Decide(Starting, in)

	want := Decision{Next: Ready, Register: true, Reason: ReasonReadyGatePassed}
	if got.Next != want.Next || !got.Register || got.Reason != want.Reason {
		t.Errorf("Decide(Starting, open door) = %+v, want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify the first one fails**

```bash
nix develop -c go test ./internal/phase/ -run 'TestTheGate' -count=1
```

Expected: `TestTheGateDoesNotRegisterAServerWhoseDoorIsShut` fails with
`Register = true, want false`, and `Reason = "ReadyGatePassed", want
"JoinsClosed"`. The second test passes already.

- [ ] **Step 3: Write the implementation**

In `internal/phase/phase.go`, replace the `case Starting:` body:

```go
	case Starting:
		if in.PodExists && in.PodRunning && in.PodReady && in.AgentReady && in.AgentStreamDownFor < StreamDownGrace {
			// Registering a server that has already closed its door and
			// deregistering it on the next pass is five seconds in which the
			// proxies route to a server that asked for nobody. The Ready
			// branch below registers it when the door opens.
			if in.JoinsClosed {
				return Decision{
					Next:   Ready,
					Reason: ReasonJoinsClosed, Message: "the server is not taking new players",
				}
			}
			return Decision{
				Next: Ready, Register: true,
				Reason: ReasonReadyGatePassed, Message: "probe green and agent ready",
			}
		}
		return Decision{
			Next:   Starting,
			Reason: ReasonPodPending, Message: "waiting for both ready signals",
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
nix develop -c go test ./internal/phase/ -count=1
```

Expected: PASS, including every existing case — `Inputs{}` leaves
`JoinsClosed` false, so no fixture changes meaning.

- [ ] **Step 5: Run the controller suite, which drives the gate end to end**

```bash
nix develop -c go test ./internal/controller/ -count=1
```

Expected: PASS. Takes about 85 s; it boots its own etcd and kube-apiserver.

- [ ] **Step 6: Commit**

```bash
git add internal/phase/phase.go internal/phase/phase_test.go
git commit -m "fix(phase): do not register a server whose door is already shut

JoinsClosed was read in the Ready branch and nowhere else, so a server that
closed its door while starting was registered anyway and deregistered on the
next pass -- about five seconds in which the proxies route to a server that
asked for nobody.

The Ready branch already carries the other direction, so opening the door
still registers, by code that existed and was tested."
```

---

### Task 2: The agent decides last

Both halves of the design need a plugin's own `ServerLoadEvent` handler to run
before the agent's. Today the agent's is a bare `@EventHandler`, which is
`EventPriority.NORMAL`, and the order is plugin registration order.

**Files:**
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt:246-247`
- Test: `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/AgentPriorityTest.kt` (create)

**Interfaces:**
- Consumes: nothing.
- Produces: the guarantee Task 4 relies on — when the agent's handler runs,
  every hold a plugin takes from its own `ServerLoadEvent` handler is open.

- [ ] **Step 1: Write the failing test**

Create `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/AgentPriorityTest.kt`:

Read the source, not the annotation: loading `AgentPlugin` pulls in Paper's
API, which is compiled for a newer Java than the test JVM runs, and the class
throws `UnsupportedClassVersionError` before any assertion. The repository
already checks cross-language constants by reading source.

```kotlin
package cloud.spawnery.agent.paper

import java.nio.file.Path
import kotlin.io.path.readText
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

/**
 * Read from the source rather than from the annotation: loading AgentPlugin
 * pulls in Paper's API, which is compiled for a newer Java than this test JVM,
 * and the class cannot be loaded here at all.
 */
class AgentPriorityTest {
    @Test
    fun `the agent reads the load event after every other handler`() {
        val source = Path.of("src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt").readText()

        assertTrue(
            source.contains(
                "@EventHandler(priority = EventPriority.MONITOR)\n    fun onServerLoad(",
            ),
            "a plugin that holds readiness from its own ServerLoadEvent handler " +
                "must have spoken before the agent decides; on a bare @EventHandler " +
                "that is plugin registration order",
        )
    }
}
```

The working directory of a Gradle test is its project directory, which is what
`PackagingInvariantTest` already relies on.

- [ ] **Step 2: Run it to verify it fails**

```bash
nix develop -c make agent
```

Expected: FAIL. The store path in the error leads to the log; the line is
`AgentPriorityTest > the agent reads the load event after every other handler() FAILED`.

- [ ] **Step 3: Write the implementation**

In `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt`,
change the annotation:

```kotlin
    // MONITOR so every other handler has run: a plugin holding readiness from
    // its own ServerLoadEvent handler is ordered against this one by plugin
    // registration order otherwise. The agent reads the finished startup and
    // changes nothing about the event, which is what MONITOR is for.
    @EventHandler(priority = EventPriority.MONITOR)
    fun onServerLoad(event: ServerLoadEvent) {
```

and add the import beside the existing Bukkit ones:

```kotlin
import org.bukkit.event.EventPriority
```

- [ ] **Step 4: Run it to verify it passes**

```bash
nix develop -c make agent
```

Expected: PASS, the whole Paper suite.

- [ ] **Step 5: Commit**

```bash
git add agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt \
        agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/AgentPriorityTest.kt
git commit -m "fix(agent): read the load event on MONITOR, not on NORMAL

A bare @EventHandler is NORMAL, so a plugin that wants to speak before the
agent decides readiness is ordered against it by plugin registration order.
The agent reads the finished startup and changes nothing about the event."
```

---

### Task 3: A readiness hold in the public API

`agent/api` is published to Maven Central as `cloud.spawnery:spawnery-api` and
consumed `compileOnly`. This task adds the type and the method; nothing
implements the behaviour yet beyond the test double.

**Files:**
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/ReadinessHold.java`
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java`
- Modify: `agent/api/src/test/java/cloud/spawnery/agent/api/FakeApi.java`
- Test: `agent/api/src/test/java/cloud/spawnery/agent/api/ReadinessHoldTest.java` (create)

**Interfaces:**
- Produces, for Task 4 and for the network's own plugin:
  - `interface ReadinessHold extends AutoCloseable` with `void close()` — no
    checked exception, so try-with-resources needs no catch.
  - `ReadinessHold SpawneryApi.holdReadiness(String reason)`.

- [ ] **Step 1: Write the failing test**

Create `agent/api/src/test/java/cloud/spawnery/agent/api/ReadinessHoldTest.java`:

```java
package cloud.spawnery.agent.api;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.lang.reflect.Method;
import org.junit.jupiter.api.Test;

class ReadinessHoldTest {
    @Test
    void closeDeclaresNoCheckedException() throws Exception {
        // The whole point of narrowing AutoCloseable: a plugin writes
        // try (var hold = api.holdReadiness("...")) with no catch.
        Method close = ReadinessHold.class.getMethod("close");
        assertEquals(0, close.getExceptionTypes().length,
                "close must not declare a checked exception");
    }

    @Test
    void aHoldCanBeUsedInTryWithResources() {
        FakeApi api = new FakeApi();
        try (ReadinessHold hold = api.holdReadiness("mappings")) {
            assertTrue(api.heldReasons().contains("mappings"));
        }
        assertTrue(api.heldReasons().isEmpty(), "closing releases the hold");
    }

    @Test
    void closingTwiceReleasesOnce() {
        FakeApi api = new FakeApi();
        ReadinessHold hold = api.holdReadiness("mappings");
        hold.close();
        hold.close();
        assertTrue(api.heldReasons().isEmpty());
    }
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd agent && nix develop .. -c gradle :api:test --rerun-tasks
```

Expected: FAIL to compile — `cannot find symbol: class ReadinessHold`.

- [ ] **Step 3: Create the type**

`agent/api/src/main/java/cloud/spawnery/agent/api/ReadinessHold.java`, with the
Apache header copied verbatim from `Spawnery.java`:

```java
package cloud.spawnery.agent.api;

/**
 * A hold that keeps this server out of readiness until it is released.
 *
 * <p>Narrows {@link AutoCloseable#close()} to throw nothing, so a plugin can
 * write {@code try (var hold = api.holdReadiness("..."))} with no catch, and
 * can equally keep the handle and close it from a callback when its own
 * executor finishes.
 *
 * <p>Closing twice releases once.
 */
public interface ReadinessHold extends AutoCloseable {
    @Override
    void close();
}
```

- [ ] **Step 4: Add the method to the interface**

In `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java`, beside
`acceptJoins`:

```java
    /**
     * Holds this server back from readiness until the returned hold is closed.
     *
     * <p>For a plugin whose initialisation continues after the server has
     * finished enabling — a mapping table loaded on its own executor, a
     * database opened in the background. The agent reports readiness when the
     * last hold is released <em>and</em> the server has finished enabling,
     * whichever comes second, so a server that is not finished stays
     * {@code Starting} rather than becoming {@code Ready} with nobody able to
     * play on it.
     *
     * <p><b>It cannot lower a readiness already reported.</b> Readiness is a
     * one-way latch; a hold taken after the agent has reported does nothing
     * but log. To stop new players reaching a server that is already ready,
     * use {@link #acceptJoins}, which is what that method is for.
     *
     * <p>A hold that is never released pins the server in {@code Starting}
     * until the operator's startup deadline fails it. That is the intended
     * outcome — a plugin that never finishes starting is a broken server —
     * and {@code reason} is what names it in the log.
     *
     * <p>Servers only. A proxy has no readiness of this kind and this throws
     * {@link UnsupportedOperationException} there.
     *
     * @param reason what is being waited for, for the log. Required.
     */
    ReadinessHold holdReadiness(String reason);
```

- [ ] **Step 5: Implement it in the test double**

In `agent/api/src/test/java/cloud/spawnery/agent/api/FakeApi.java`, add the
field, the override and the accessor the test reads:

```java
    private final List<String> held = new ArrayList<>();

    @Override
    public ReadinessHold holdReadiness(String reason) {
        held.add(reason);
        return new ReadinessHold() {
            private boolean released;

            @Override
            public void close() {
                if (!released) {
                    released = true;
                    held.remove(reason);
                }
            }
        };
    }

    /** The reasons of the holds still open. For tests of this package. */
    public List<String> heldReasons() {
        return List.copyOf(held);
    }
```

Add `java.util.ArrayList` and `java.util.List` to the imports if they are not
already there.

- [ ] **Step 6: Run the api suite to verify it passes**

```bash
cd agent && nix develop .. -c gradle :api:test --rerun-tasks
```

Expected: PASS, including `PackagingInvariantTest`, which walks the compiled
classes of this package.

- [ ] **Step 7: Commit**

```bash
git add agent/api/src/main/java/cloud/spawnery/agent/api/ReadinessHold.java \
        agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java \
        agent/api/src/test/java/cloud/spawnery/agent/api/FakeApi.java \
        agent/api/src/test/java/cloud/spawnery/agent/api/ReadinessHoldTest.java
git commit -m "feat(api): a plugin can hold this server back from readiness

For a plugin whose initialisation continues after the server has finished
enabling. close() narrows AutoCloseable to throw nothing, so the common shape
is try-with-resources and the other one is a handle closed from a callback.

The interface only; the agent honours it in the next commit."
```

---

### Task 4: The agent honours the holds

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/ReadinessGate.kt`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/MirrorApi.kt`
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt`
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/ReadinessGateTest.kt` (create)
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/MirrorApiTest.kt`

**Interfaces:**
- Consumes: `ReadinessHold` from Task 3; the MONITOR ordering from Task 2.
- Produces:
  - `class ReadinessGate(onOpen: () -> Unit)` in `cloud.spawnery.agent` with
    `fun hold(reason: String): ReadinessHold`, `fun serverLoaded()`,
    `fun openReasons(): List<String>`.
  - `MirrorApi` gains a fifth constructor parameter
    `readiness: ReadinessGate? = null`. The default keeps the seventeen test
    call sites unchanged.

- [ ] **Step 1: Write the failing gate test**

Create `agent/common/src/test/kotlin/cloud/spawnery/agent/ReadinessGateTest.kt`:

```kotlin
package cloud.spawnery.agent

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class ReadinessGateTest {
    private fun counting(): Pair<ReadinessGate, () -> Int> {
        var opened = 0
        val gate = ReadinessGate { opened++ }
        return gate to { opened }
    }

    @Test
    fun `the load event alone opens the gate`() {
        val (gate, opened) = counting()
        gate.serverLoaded()
        assertEquals(1, opened())
    }

    @Test
    fun `a hold keeps the gate shut past the load event`() {
        val (gate, opened) = counting()
        val hold = gate.hold("mappings")
        gate.serverLoaded()
        assertEquals(0, opened(), "the hold is still open")
        hold.close()
        assertEquals(1, opened())
    }

    @Test
    fun `the last hold opens it, not the first`() {
        val (gate, opened) = counting()
        val one = gate.hold("mappings")
        val two = gate.hold("database")
        gate.serverLoaded()
        one.close()
        assertEquals(0, opened())
        two.close()
        assertEquals(1, opened())
    }

    @Test
    fun `a hold released before the load event does not open it early`() {
        val (gate, opened) = counting()
        gate.hold("mappings").close()
        assertEquals(0, opened(), "the server has not finished enabling yet")
        gate.serverLoaded()
        assertEquals(1, opened())
    }

    @Test
    fun `the gate opens once`() {
        val (gate, opened) = counting()
        gate.serverLoaded()
        gate.serverLoaded()
        assertEquals(1, opened())
    }

    @Test
    fun `a hold taken after the gate opened changes nothing`() {
        // Readiness is a one-way latch: ServerState.markReady cannot be
        // cleared, so a late hold must not pretend it can.
        val (gate, opened) = counting()
        gate.serverLoaded()
        gate.hold("too late").close()
        assertEquals(1, opened())
        assertTrue(gate.openReasons().isEmpty())
    }

    @Test
    fun `closing a hold twice releases once`() {
        val (gate, opened) = counting()
        val one = gate.hold("mappings")
        val two = gate.hold("database")
        gate.serverLoaded()
        one.close()
        one.close()
        assertEquals(0, opened(), "the second close must not stand in for two")
        two.close()
        assertEquals(1, opened())
    }

    @Test
    fun `open reasons name what is still awaited`() {
        val (gate, _) = counting()
        gate.hold("mappings")
        gate.hold("database")
        assertEquals(listOf("mappings", "database"), gate.openReasons())
    }
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
nix develop -c make agent
```

Expected: FAIL to compile — `unresolved reference: ReadinessGate`.

- [ ] **Step 3: Write the gate**

Create `agent/common/src/main/kotlin/cloud/spawnery/agent/ReadinessGate.kt`
with the Apache header copied from `MirrorApi.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.api.ReadinessHold

/**
 * What a server's readiness waits for.
 *
 * [onOpen] runs once, when the server has finished enabling and no hold is
 * left -- whichever of the two comes second. It runs outside this object's
 * lock, because on Paper it sends on the agent's stream.
 */
class ReadinessGate(private val onOpen: () -> Unit) {
    private val lock = Any()
    private val open = LinkedHashMap<Long, String>()
    private var loaded = false
    private var opened = false
    private var next = 0L

    fun hold(reason: String): ReadinessHold {
        synchronized(lock) {
            // A late hold is not an error worth throwing for: the plugin
            // cannot know it lost the race, and readiness cannot be lowered
            // anyway. It gets a handle that releases nothing.
            if (opened) return ReadinessHold {}
            val key = next++
            open[key] = reason
            return Release(key)
        }
    }

    fun serverLoaded() {
        val fire = synchronized(lock) {
            loaded = true
            openNow()
        }
        if (fire) onOpen()
    }

    fun openReasons(): List<String> = synchronized(lock) { open.values.toList() }

    private fun release(key: Long) {
        val fire = synchronized(lock) {
            if (open.remove(key) == null) return
            openNow()
        }
        if (fire) onOpen()
    }

    private fun openNow(): Boolean {
        if (opened || !loaded || open.isNotEmpty()) return false
        opened = true
        return true
    }

    private inner class Release(private val key: Long) : ReadinessHold {
        override fun close() = release(key)
    }
}
```

- [ ] **Step 4: Run the gate test to verify it passes**

```bash
nix develop -c make agent
```

Expected: PASS, all eight.

- [ ] **Step 5: Write the failing MirrorApi test**

Add to `agent/common/src/test/kotlin/cloud/spawnery/agent/MirrorApiTest.kt`:

```kotlin
    @Test
    fun `holdReadiness reaches the gate on a server`() {
        val gate = ReadinessGate {}
        val api = MirrorApi(
            NetworkMirror(), serverSelf(), connector(), CloudEvents(), gate,
        )

        api.holdReadiness("mappings")

        assertEquals(listOf("mappings"), gate.openReasons())
    }

    @Test
    fun `holdReadiness refuses on a proxy`() {
        val api = MirrorApi(NetworkMirror(), proxySelf(), connector(), CloudEvents())

        assertThrows(UnsupportedOperationException::class.java) {
            api.holdReadiness("mappings")
        }
    }
```

Add `org.junit.jupiter.api.Assertions.assertThrows` to the imports if it is
not already there.

- [ ] **Step 6: Run it to verify it fails**

```bash
nix develop -c make agent
```

Expected: FAIL to compile — `MirrorApi` takes four arguments and has no
`holdReadiness`.

- [ ] **Step 7: Wire the gate into MirrorApi**

In `agent/common/src/main/kotlin/cloud/spawnery/agent/MirrorApi.kt`, add the
parameter after `events` and the override beside `acceptJoins`:

```kotlin
    private val events: CloudEvents,
    /**
     * Null on a proxy, which has no readiness of this kind -- see ProxyState.
     * Defaulted so the many call sites that do not care stay as they are.
     */
    private val readiness: ReadinessGate? = null,
) : SpawneryApi {
```

```kotlin
    override fun holdReadiness(reason: String): ReadinessHold {
        val gate = readiness ?: throw UnsupportedOperationException(
            "this is a proxy; a proxy has no readiness to hold",
        )
        return gate.hold(reason)
    }
```

with `import cloud.spawnery.agent.api.ReadinessHold` beside the other api
imports.

- [ ] **Step 8: Run the common suite to verify it passes**

```bash
nix develop -c make agent
```

Expected: PASS. The seventeen existing `MirrorApi(...)` call sites compile
unchanged because the new parameter is defaulted.

- [ ] **Step 9: Let the Paper agent open the gate instead of the event**

In `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt`,
add the field beside the other agent state:

```kotlin
    private val readiness = ReadinessGate {
        if (state.markReady()) {
            loop?.send(role.ready())
        }
    }
```

pass it when the API is built (line 107):

```kotlin
                val api = MirrorApi(mirror, self, connector, events, readiness)
```

and reduce the event handler to the two things it still does:

```kotlin
    // MONITOR so every other handler has run: a plugin holding readiness from
    // its own ServerLoadEvent handler is ordered against this one by plugin
    // registration order otherwise. The agent reads the finished startup and
    // changes nothing about the event, which is what MONITOR is for.
    @EventHandler(priority = EventPriority.MONITOR)
    fun onServerLoad(event: ServerLoadEvent) {
        if (event.type != ServerLoadEvent.LoadType.STARTUP) return
        state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())
        // The operator cannot learn a hold's reason -- carrying it would be a
        // proto field for one log line -- so this is the only place the name
        // of a plugin that never finishes is written down.
        val waiting = readiness.openReasons()
        if (waiting.isNotEmpty()) {
            logger.info("not ready yet, waiting for: ${waiting.joinToString(", ")}")
        }
        readiness.serverLoaded()
    }
```

with `import cloud.spawnery.agent.ReadinessGate`.

- [ ] **Step 10: Say why the proxy passes none**

In `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt`
at line 255, above the unchanged call:

```kotlin
        // No ReadinessGate: a proxy has no readiness flag to hold, and
        // holdReadiness refuses here rather than pretending. See ProxyState.
        val api = MirrorApi(mirror, self, connector, events)
```

- [ ] **Step 11: Build both agents and run every suite**

```bash
git add agent/
nix develop -c make agent
```

Expected: both plugins and their JUnit suites build. The `git add` is not
optional: Nix builds read the git index, and `ReadinessGate.kt` and
`ReadinessHold.java` are new.

- [ ] **Step 12: Commit**

```bash
git add agent/
git commit -m "feat(agent): hold readiness until every plugin has finished

The gate opens when the server has finished enabling and no hold is left,
whichever comes second. It opens once: markReady is a latch and a hold taken
afterwards releases nothing, which is why holdReadiness says so rather than
throwing -- the plugin could not have known it lost the race.

A proxy has no readiness of this kind, so MirrorApi refuses there instead of
handing back a hold that holds nothing."
```

---

### Task 5: The version, the known issue, and the tree

**Files:**
- Modify: `flake.nix:248` and `flake.nix:327`
- Modify: `charts/spawnery/{Chart.yaml,values.yaml,README.md}`
- Modify: `README.md`
- Modify: `docs/known-issues.md`

**Interfaces:**
- Consumes: everything above.
- Produces: an agent version that can be deployed, and a known-issues file
  that no longer describes a solved problem.

- [ ] **Step 1: Move the agent version**

`imageVersion` in `flake.nix` is the agent and game-image version, and it is
also `agentVersion` for Gradle (`nix/agents.nix:59`), so it is what the
published `cloud.spawnery:spawnery-api` carries. `agent/` changed, so it
moves. `operatorVersion` moves too — `internal/phase` changed.

```nix
          imageVersion = "0.2.25";
```

```nix
          operatorVersion = "0.2.25";
```

**`operatorVersion` drags the chart with it**, and four source-reading tests
in `internal/rbacaudit` fail until it does. All of these move to `0.2.25`:

- `charts/spawnery/Chart.yaml` `appVersion` — tracks the operator, with a
  paragraph above it saying what this release changed
- `charts/spawnery/Chart.yaml` `version` — moves whenever `charts/` does
- `charts/spawnery/values.yaml` `image.tag`
- the `--version` in **both** `README.md` and `charts/spawnery/README.md`

- [ ] **Step 2: Delete the known-issues entry**

`docs/known-issues.md` holds only open problems. Remove the whole section
`## A server is joinable before its plugins have finished`, including the
`**Correction, 2026-09-05.**` paragraph added with the spec. Its story lives
in this commit.

- [ ] **Step 3: Run everything**

```bash
nix develop -c make test
nix develop -c make lint
```

Expected: PASS. `make test` runs `manifests generate fmt vet chart-lint
toolchain-lint` first; no API type or `.proto` changed, so nothing regenerates
and `git status` stays clean.

- [ ] **Step 4: Commit**

```bash
git add flake.nix charts/ README.md docs/known-issues.md
git commit -m "chore: 0.2.25, a server holds itself back while a plugin starts

imageVersion because agent/ changed -- it is agentVersion for Gradle too, so
it is the version cloud.spawnery:spawnery-api is published under.
operatorVersion because internal/phase decides differently.

The known issue goes with them: what remained of it was the window this
release closes, and the entry only holds open problems."
```

---

## What this does not finish

The network's own plugin still has to take the hold. Nothing in this
repository can: only the plugin knows when its initialisation is done, the
operator has no way to learn which plugins a server runs, and the agent has no
business special-casing one of them.

For the `paulwtf` network that is cyperia's `core`, holding from its own
`ServerLoadEvent` handler and releasing when ViaVersion reports its mapping
load finished. Until that lands, this release changes no behaviour on that
network beyond Task 1 — which changes nothing either, because nothing there
calls `acceptJoins` during startup yet.
