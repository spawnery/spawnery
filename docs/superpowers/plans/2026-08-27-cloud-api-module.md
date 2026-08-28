# Milestone 7b-1 — the `agent/api` module

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Java module `agent/api` whose types a third-party Paper or Velocity plugin can compile against and call at runtime, shipped inside both agent jars, with the packaging rule that makes that possible enforced by a test rather than by a comment.

**Architecture:** Both agent jars relocate every bundled dependency — including the Kotlin standard library — under `cloud.spawnery.agent.shaded.*`. A public API can therefore carry no type that relocation touches, which rules out Kotlin entirely and rules out protobuf, gRPC and Guava in any signature. The module is Java, depends on nothing, and sits at `cloud.spawnery.agent.api` because `hack/agent-jar-check.sh` fails on any class outside `cloud/spawnery/agent/`.

**Tech Stack:** Java 21 (records, sealed interfaces), Gradle multi-project, JUnit 5. No Kotlin, no external dependencies.

**Spec:** [`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../specs/2026-08-27-cloud-api-design.md) §3.2, §3.3, §6.2, §9.1

## Global Constraints

- Every build and test command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`
- Commit messages are Conventional Commits with English subjects, and every
  commit ends with exactly these two trailers:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **Never push, never merge, and never create a tag.** The repository owner
  authorises each push and each tag individually.
- **Never run `git config` in any form.**
- **`agent/api` must never gain a dependency.** Not a logging facade, not
  annotations, not Guava, not the Kotlin stdlib. Task 2's test is what holds
  this, and the reason is §3.3: anything the module carries into a public
  signature is a type the shipped jar has relocated out from under the caller.
- `make agent` needs a container runtime and only works on `x86_64-linux`. It
  runs both JUnit suites as the derivations' check phases, so a failing test in
  this module fails the Nix build.

## What this plan deliberately does not do

- **No implementation of `SpawneryApi`.** Nothing in `:paper` or `:velocity`
  implements it yet, and nothing should: the mirror those methods read from is
  7b-3's work, and an implementation written before it would be a stub whose
  tests assert the stub.
- **No `events()`, `connect()` or `lifecycle()` on `SpawneryApi`.** They belong
  to 7b-3 and 7b-4, and designing them now would be designing against a
  delivery mechanism that does not exist (§4.1: the server side has no fan-out
  at all). Adding a method to this interface later is safe in the one direction
  that matters: third-party plugins *consume* `SpawneryApi` and never implement
  it, which Task 4's javadoc says out loud.
- **No proto change.** 7b-4.

## File structure

| Path | Responsibility |
|---|---|
| `agent/settings.gradle.kts` | Gains `include("api")` |
| `agent/api/build.gradle.kts` | Java-only module, JVM 21, zero dependencies except the JUnit test set |
| `agent/api/src/main/java/cloud/spawnery/agent/api/Self.java` | Who this agent is: the sealed `Self` and its two shapes |
| `agent/api/src/main/java/cloud/spawnery/agent/api/Group.java` | A group as a plugin sees it |
| `agent/api/src/main/java/cloud/spawnery/agent/api/ServerInfo.java` | One backend |
| `agent/api/src/main/java/cloud/spawnery/agent/api/ServerPhase.java` | The phase vocabulary, as an enum |
| `agent/api/src/main/java/cloud/spawnery/agent/api/CloudPlayer.java` | A player and where they are |
| `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java` | The read surface |
| `agent/api/src/main/java/cloud/spawnery/agent/api/Spawnery.java` | The entry point, and the exception when there is none |
| `agent/api/src/test/java/cloud/spawnery/agent/api/PackagingInvariantTest.java` | §6.2's first invariant |
| `agent/common/build.gradle.kts` | `api(project(":api"))`, so the module reaches both shaded jars |
| `agent/api/README.md` | What a plugin author has to do, and the one thing they must not |

---

### Task 1: The module exists and reaches both shaded jars

A module nothing depends on is a module the shaded jars do not carry, and a
type a plugin cannot load at runtime is not an API. This task ends by opening
the built jar and looking.

**Files:**
- Modify: `agent/settings.gradle.kts`
- Create: `agent/api/build.gradle.kts`
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/Self.java`
- Modify: `agent/common/build.gradle.kts`

**Interfaces:**
- Consumes: nothing.
- Produces: the Gradle project `:api`; `cloud.spawnery.agent.api.Self` with
  `String name()`, `String group()`, `String namespace()`, and its two
  permitted shapes `ServerSelf` (adding `int slots()`) and `ProxySelf`.

- [ ] **Step 1: Add the module to the build**

`agent/settings.gradle.kts` — add the line, keeping the existing order:

```kotlin
rootProject.name = "spawnery-agents"

include("api")
include("common")
include("paper")
include("velocity")
```

- [ ] **Step 2: Write the module's build file**

Create `agent/api/build.gradle.kts`:

```kotlin
plugins {
    `java-library`
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

repositories {
    mavenCentral()
}

// **This block stays empty of everything but the test framework, and that is
// the module's whole design.** Both agent jars relocate every bundled
// dependency under cloud.spawnery.agent.shaded.* -- the Kotlin standard
// library included, measured as 1045 classes against 0 at the real
// coordinates. A type from any dependency appearing in a public signature here
// would be a type the shipped jar has moved out from under a plugin compiled
// against the real one, and the symptom is a NoSuchMethodError at the call
// rather than a compile error anywhere. PackagingInvariantTest is what holds
// this; the emptiness here is what makes it easy to hold.
//
// No Kotlin plugin either, for the same measurement: a Kotlin class carries a
// @kotlin.Metadata annotation, that annotation is relocated with everything
// else, and a Kotlin compiler reading the shipped jar then finds no metadata
// and sees plain Java -- no nullability, no default arguments, no data
// classes. Writing this module in Java is what makes what a plugin author
// compiles against the same thing they get.
dependencies {
    testImplementation(platform("org.junit:junit-bom:5.11.4"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}

java {
    sourceCompatibility = JavaVersion.VERSION_21
    targetCompatibility = JavaVersion.VERSION_21
}

tasks.test {
    useJUnitPlatform()
    testLogging {
        showStandardStreams = true
    }
}
```

- [ ] **Step 3: Write the first types**

Create `agent/api/src/main/java/cloud/spawnery/agent/api/Self.java`:

```java
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

package cloud.spawnery.agent.api;

/**
 * What this process is, in the network's own vocabulary.
 *
 * <p>Sealed rather than a single type with a nullable {@code slots}: a proxy
 * has no player capacity of the kind a backend has, and a field that is
 * meaningless on one of two shapes is a field every caller has to remember is
 * meaningless. The two shapes say which one they are by their type, and a
 * plugin that only ever runs on one platform never meets the other.
 *
 * <p>The names are the Kubernetes object names, so what a plugin prints here
 * is what an operator can paste into {@code kubectl}.
 */
public sealed interface Self permits ServerSelf, ProxySelf {
    /** The name of this pod's own {@code Server} or proxy pod. */
    String name();

    /** The {@code ServerGroup} or {@code ProxyGroup} this belongs to. */
    String group();

    /** The namespace, which is also the boundary of everything this API can see. */
    String namespace();
}
```

Create `ServerSelf.java` in the same package:

```java
package cloud.spawnery.agent.api;

/** A Paper backend's view of itself. */
public non-sealed interface ServerSelf extends Self {
    /**
     * The player capacity this server was configured with -- the group's
     * {@code spec.maxPlayers}, which is also what the agent reports upward.
     */
    int slots();
}
```

Create `ProxySelf.java` in the same package:

```java
package cloud.spawnery.agent.api;

/**
 * A Velocity proxy's view of itself.
 *
 * <p>It adds nothing to {@link Self}, and the empty body is the point: a proxy
 * has no capacity of its own that a plugin should read as a backend's slots.
 * The type exists so that {@code self() instanceof ProxySelf} is how a plugin
 * asks which side it is running on, rather than a string comparison.
 */
public non-sealed interface ProxySelf extends Self {
}
```

Each of the three files carries the same Apache header as `Self.java` above.
Every file under `agent/` in this repository has one; a file without it is a
lint finding waiting to happen.

- [ ] **Step 4: Make `:common` depend on it**

In `agent/common/build.gradle.kts`, add to the `dependencies` block, above the
gRPC entries:

```kotlin
    // `api` and not `implementation`: the module's types appear in signatures
    // :paper and :velocity will implement, so both need it on their compile
    // classpath, and both shadowJars need it on their runtime one. This is
    // also what carries it into the shipped jars at all -- see Task 1's jar
    // check.
    api(project(":api"))
```

- [ ] **Step 5: Track the new files before building**

```bash
git add agent/settings.gradle.kts agent/api agent/common/build.gradle.kts
```

**This is not tidiness, it is a precondition.** `nix build` filters the source
tree through the git index, so an untracked directory does not exist inside the
sandbox. Skipping it gives a failure that reads like a configuration error and
is not:

```
No matching variant of project :api was found ... - No variants exist.
```

`settings.gradle.kts` includes `api`, the directory the sandbox sees is empty,
and the project therefore produces nothing to depend on. Every later `make
agent` in this plan has the same requirement.

- [ ] **Step 6: Build the agents**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make agent`

Expected: both jars build, both JUnit suites pass, and the derivation's install
check — which runs `hack/agent-jar-check.sh` against each jar — passes.

If the jar check fails with "these packages ship unrelocated", read the package
it names. If it is `cloud/spawnery/agent/api`, something has gone wrong with
the package name: the check permits everything under `cloud/spawnery/agent/`
and that is inside it.

- [ ] **Step 7: Open the jar and look**

A green build says the jar was produced, not that it carries these classes.

Run:

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c python3 - <<'PY'
import zipfile, glob
for jar in sorted(glob.glob("result*/share/spawnery/*/spawnery-agent.jar")):
    names = zipfile.ZipFile(jar).namelist()
    api = [n for n in names if n.startswith("cloud/spawnery/agent/api/")]
    print(f"{jar}: {len(api)} api classes")
    for n in sorted(api):
        print("   ", n)
PY
```

Expected: both jars list `cloud/spawnery/agent/api/Self.class`,
`ServerSelf.class` and `ProxySelf.class`. **A count of 0 means the module was
built and then not bundled**, which is the failure this step exists to catch —
`make agent` is green either way.

If `result*` does not point at the agents build, find the store path with
`nix --extra-experimental-features 'nix-command flakes' build --no-link
--print-out-paths .#agents` and use that.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "$(cat <<'EOF'
feat(agent): a Java module for the API a plugin compiles against

Both agent jars relocate every bundled dependency under
cloud.spawnery.agent.shaded.*, and the shipped Velocity jar was measured
carrying 0 classes under kotlin/ against 1045 under the shaded prefix. So a
public API can carry no type that relocation touches, and that rules out
Kotlin itself: a Kotlin class's @Metadata annotation is relocated with
everything else, and a compiler reading the shipped jar then sees plain Java.

This module is therefore Java, and it depends on nothing at all. The empty
dependencies block is the design rather than a starting point.

The package is cloud.spawnery.agent.api and not cloud.spawnery.api because
hack/agent-jar-check.sh fails on any class outside cloud/spawnery/agent/ --
which is how it catches a dependency that shipped unrelocated, so the name
sits inside the prefix rather than beside it.

Verified by opening both built jars rather than by a green build: a module
that is built and then not bundled produces exactly the same green build and
no loadable class.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 2: The packaging invariant, as a test

§3.3's rule is currently a comment in a build file and a paragraph in a design
document. Both are what a future dependency would be added past. This task
turns it into something that fails.

**Files:**
- Create: `agent/api/src/test/java/cloud/spawnery/agent/api/PackagingInvariantTest.java`

**Interfaces:**
- Consumes: `Self`, `ServerSelf`, `ProxySelf` from Task 1.
- Produces: nothing. This task adds no production code.

- [ ] **Step 1: Write the test**

Create the file with the standard Apache header, then:

```java
package cloud.spawnery.agent.api;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.IOException;
import java.lang.reflect.Constructor;
import java.lang.reflect.Method;
import java.lang.reflect.Modifier;
import java.lang.reflect.ParameterizedType;
import java.lang.reflect.Type;
import java.lang.reflect.TypeVariable;
import java.lang.reflect.WildcardType;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;

/**
 * The rule that makes this module consumable at all, as something that fails.
 *
 * <p>Both agent jars relocate every bundled dependency under
 * {@code cloud.spawnery.agent.shaded.*}. A public signature here carrying a
 * type from anywhere but {@code java.*} or this package would be a signature
 * the shipped jar has moved out from under a plugin compiled against the real
 * one -- a {@code NoSuchMethodError} at the call, with nothing failing at
 * compile time on either side.
 *
 * <p>A build file's comment does not stop a dependency being added. This does.
 */
class PackagingInvariantTest {
    private static final Path CLASSES = Path.of("build/classes/java/main");

    private static boolean permitted(String name) {
        return name.startsWith("java.")
                || name.startsWith("cloud.spawnery.agent.api.");
    }

    @Test
    void everyPublicSignatureUsesOnlyJavaOrThisModulesOwnTypes() throws Exception {
        List<Class<?>> classes = compiledClasses();
        // A scanner that finds nothing passes every assertion below it, which
        // is the one way this test could lie about the thing it exists for.
        assertFalse(
                classes.isEmpty(),
                "no compiled classes under " + CLASSES.toAbsolutePath()
                        + " -- this test found nothing to check and would have passed anyway");

        List<String> offenders = new ArrayList<>();
        for (Class<?> c : classes) {
            for (Method m : c.getDeclaredMethods()) {
                if (!isPublicApi(m.getModifiers())) {
                    continue;
                }
                collect(offenders, c + "." + m.getName() + " returns", m.getGenericReturnType());
                for (Type t : m.getGenericParameterTypes()) {
                    collect(offenders, c + "." + m.getName() + " takes", t);
                }
            }
            for (Constructor<?> k : c.getDeclaredConstructors()) {
                if (!isPublicApi(k.getModifiers())) {
                    continue;
                }
                for (Type t : k.getGenericParameterTypes()) {
                    collect(offenders, c + " constructor takes", t);
                }
            }
        }
        assertTrue(
                offenders.isEmpty(),
                "these public signatures carry a type the agent jars relocate, so a plugin "
                        + "compiled against the real one fails at the call:\n  "
                        + String.join("\n  ", offenders));
    }

    /**
     * No Kotlin anywhere in this module, checked at the annotation rather than
     * at the build file. A Kotlin class carries {@code @kotlin.Metadata}, that
     * annotation is relocated with the rest of the stdlib, and a compiler
     * reading the shipped jar then finds no metadata at all.
     */
    @Test
    void nothingHereIsCompiledFromKotlin() throws Exception {
        List<String> kotlinish = new ArrayList<>();
        for (Class<?> c : compiledClasses()) {
            for (var a : c.getDeclaredAnnotations()) {
                if (a.annotationType().getName().startsWith("kotlin")) {
                    kotlinish.add(c.getName() + " carries " + a.annotationType().getName());
                }
            }
        }
        assertTrue(kotlinish.isEmpty(), String.join("\n  ", kotlinish));
    }

    private static boolean isPublicApi(int modifiers) {
        return Modifier.isPublic(modifiers) || Modifier.isProtected(modifiers);
    }

    private static void collect(List<String> into, String where, Type t) {
        if (t instanceof Class<?> c) {
            Class<?> element = c;
            while (element.isArray()) {
                element = element.getComponentType();
            }
            if (element.isPrimitive()) {
                return;
            }
            if (!permitted(element.getName())) {
                into.add(where + " " + element.getName());
            }
            return;
        }
        if (t instanceof ParameterizedType p) {
            collect(into, where, p.getRawType());
            for (Type arg : p.getActualTypeArguments()) {
                collect(into, where, arg);
            }
            return;
        }
        if (t instanceof WildcardType w) {
            for (Type b : w.getUpperBounds()) {
                collect(into, where, b);
            }
            for (Type b : w.getLowerBounds()) {
                collect(into, where, b);
            }
            return;
        }
        if (t instanceof TypeVariable<?> v) {
            for (Type b : v.getBounds()) {
                collect(into, where, b);
            }
        }
    }

    private static List<Class<?>> compiledClasses() throws IOException {
        if (!Files.isDirectory(CLASSES)) {
            return List.of();
        }
        try (Stream<Path> walk = Files.walk(CLASSES)) {
            List<Class<?>> out = new ArrayList<>();
            for (Path p : walk.filter(x -> x.toString().endsWith(".class")).toList()) {
                String name = CLASSES.relativize(p).toString()
                        .replace(java.io.File.separatorChar, '.')
                        .replaceAll("\\.class$", "");
                try {
                    out.add(Class.forName(name, false, PackagingInvariantTest.class.getClassLoader()));
                } catch (ClassNotFoundException e) {
                    throw new AssertionError("compiled class " + name + " is not loadable", e);
                }
            }
            return out;
        }
    }
}
```

- [ ] **Step 2: Run it and watch it pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :api:test --console=plain'`

Expected: two tests, both PASS.

If Gradle cannot resolve dependencies offline, run the module's tests through
the Nix build instead — `make agent` runs every subproject's `test` as the
derivation's check phase.

- [ ] **Step 3: Prove the guard can fail, twice**

A test that has only ever passed has not been shown to catch anything. Both
halves get a mutation.

**First**, add a method to `Self.java` that returns a type from outside the
module. `java.util.List` is permitted, so use something that is not:

```java
    /** TEMPORARY -- mutation check, remove me. */
    javax.naming.Context leak();
```

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :api:test --console=plain'`

Expected: FAIL, naming `javax.naming.Context` and the method it is on.

**Remove that method.** Confirm with `git diff` that `Self.java` is back.

**Second**, the Kotlin half. Add `kotlin("jvm")` to the module's plugins block
and rename one file to `.kt`… this is the expensive mutation and it is not
worth running: adding the Kotlin plugin changes the module's dependency
resolution and `agent/deps.json`, which reaches Maven Central. **Record that
`nothingHereIsCompiledFromKotlin` is asserted rather than mutation-checked**,
in the test's own comment, rather than claiming a check that was not run.

Add to that test's javadoc:

```java
     * <p>Unlike its neighbour, this one has not been mutation-checked. Making
     * it fail means adding the Kotlin plugin to this module, which changes
     * dependency resolution and would need agent/deps.json regenerated against
     * a real Maven Central. It is asserted, not measured, and this sentence is
     * here so nobody reads it as the stronger thing.
```

- [ ] **Step 4: Run the whole agent build**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make agent`

Expected: green, with `:api:test` among the suites that ran.

- [ ] **Step 5: Commit**

```bash
git add agent/api/src/test
git commit -m "$(cat <<'EOF'
test(agent): the API module's packaging rule, as something that fails

The rule that makes this module consumable was a comment in a build file and
a paragraph in a design document, and both are what a future dependency gets
added past. A public signature carrying a relocated type fails at the call
with a NoSuchMethodError and at compile time on neither side, so nothing
except this test would notice.

Mutation-checked in one half: a method returning javax.naming.Context makes
it fail, naming the type and the method. The Kotlin half is asserted and not
measured -- making it fail means adding the Kotlin plugin, which changes
dependency resolution and needs deps.json regenerated against a real Maven
Central -- and the test's own comment says so rather than letting it read as
the stronger claim.

The scanner refuses to run on an empty class list, because a scanner that
finds nothing passes every assertion after it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 3: The value types

What a plugin reads. Records, because they are values: two `ServerInfo`s
describing the same server at the same moment must be equal, and a plugin that
puts one in a `Set` should get that for free.

**Files:**
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/ServerPhase.java`
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/ServerInfo.java`
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/Group.java`
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/CloudPlayer.java`
- Test: `agent/api/src/test/java/cloud/spawnery/agent/api/ValueTypesTest.java`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `ServerPhase` (enum, with `ServerPhase.fromWire(String)`), and the
  records `ServerInfo(String name, String group, ServerPhase phase, int
  players, int slots, boolean registered)` with an added `int freeSlots()`,
  `Group(String name, Group.Kind kind, int replicas, int readyReplicas, int
  onlinePlayers, int freeSlots)` where `Kind` is an enum nested inside `Group`,
  and `CloudPlayer(UUID id, String name, Optional<String> server)`.

- [ ] **Step 1: Write the failing test**

Create `ValueTypesTest.java` with the Apache header, then:

```java
package cloud.spawnery.agent.api;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.Optional;
import java.util.Set;
import java.util.UUID;
import org.junit.jupiter.api.Test;

class ValueTypesTest {
    @Test
    void twoDescriptionsOfTheSameServerAreEqual() {
        var a = new ServerInfo("lobby-a3f9", "lobby", ServerPhase.READY, 12, 100, true);
        var b = new ServerInfo("lobby-a3f9", "lobby", ServerPhase.READY, 12, 100, true);
        assertEquals(a, b);
        assertEquals(1, Set.of(a, b).size());
    }

    @Test
    void aPlayerWhoIsOnNoServerSaysSoWithAnEmptyOptional() {
        var p = new CloudPlayer(UUID.randomUUID(), "someone", Optional.empty());
        assertTrue(p.server().isEmpty());
    }

    // A record with a null component is a record that hands every caller an
    // NPE at some unrelated line later. The compact constructors refuse it at
    // the point of construction, where the stack trace still names the cause.
    @Test
    void aNullComponentIsRefusedWhereItIsBuilt() {
        assertThrows(NullPointerException.class,
                () -> new ServerInfo(null, "lobby", ServerPhase.READY, 0, 100, false));
        assertThrows(NullPointerException.class,
                () -> new CloudPlayer(UUID.randomUUID(), "someone", null));
        assertThrows(NullPointerException.class,
                () -> new Group(null, Group.Kind.EPHEMERAL, 1, 1, 0, 100));
    }

    // An unknown phase is not an error and must not throw: the operator may
    // publish a phase this jar predates, and a plugin that crashed on one
    // would break on an operator upgrade it had nothing to do with.
    @Test
    void anUnknownPhaseBecomesUnknownRatherThanAnException() {
        assertEquals(ServerPhase.UNKNOWN, ServerPhase.fromWire("SomethingLaterInvented"));
        assertEquals(ServerPhase.READY, ServerPhase.fromWire("Ready"));
    }
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :api:test --console=plain'`

Expected: a compile failure — none of `ServerInfo`, `CloudPlayer`, `Group` or
`ServerPhase` exists yet.

- [ ] **Step 3: Write the types**

`ServerPhase.java`, with the Apache header:

```java
package cloud.spawnery.agent.api;

/**
 * Where a server is in its life, in the operator's own vocabulary.
 *
 * <p>{@link #UNKNOWN} is not a failure case, it is forward compatibility: the
 * operator may publish a phase this jar predates, and a plugin that threw on
 * one would break on an operator upgrade it had nothing to do with. Use
 * {@link #fromWire(String)} rather than {@code valueOf}, which throws.
 */
public enum ServerPhase {
    PENDING,
    STARTING,
    READY,
    RETIRING,
    DRAINING,
    TERMINATING,
    FAILED,
    UNKNOWN;

    /** Maps the operator's spelling onto this enum, never throwing. */
    public static ServerPhase fromWire(String phase) {
        if (phase == null) {
            return UNKNOWN;
        }
        for (ServerPhase p : values()) {
            if (p != UNKNOWN && p.name().equalsIgnoreCase(phase)) {
                return p;
            }
        }
        return UNKNOWN;
    }
}
```

`ServerInfo.java`:

```java
package cloud.spawnery.agent.api;

import java.util.Objects;

/**
 * One backend, as the operator last described it.
 *
 * <p>A value and not a handle: it is a description of a moment, and the moment
 * has passed by the time a plugin reads it. Calling {@link SpawneryApi#server}
 * again gets a newer one; holding this and expecting it to change gets
 * nothing.
 *
 * @param registered whether the proxies have this server in their routing
 *     tables. A server can be {@link ServerPhase#READY} and not registered --
 *     that is the first half of a drain -- so a plugin deciding where to send
 *     somebody wants this and not the phase.
 */
public record ServerInfo(
        String name,
        String group,
        ServerPhase phase,
        int players,
        int slots,
        boolean registered) {
    public ServerInfo {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(group, "group");
        Objects.requireNonNull(phase, "phase");
    }

    /** How many more players this server would accept, never negative. */
    public int freeSlots() {
        return Math.max(0, slots - players);
    }
}
```

`Group.java`:

```java
package cloud.spawnery.agent.api;

import java.util.Objects;

/**
 * A group of servers, as the operator last described it.
 *
 * @param freeSlots the operator's own figure, which counts only ready servers
 *     of the group's current spec. It is what the scaler publishes rather than
 *     a sum a plugin could compute from {@link SpawneryApi#servers()}, and the
 *     two can disagree while a rolling update is in flight.
 */
public record Group(
        String name,
        Kind kind,
        int replicas,
        int readyReplicas,
        int onlinePlayers,
        int freeSlots) {
    public Group {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(kind, "kind");
    }

    /** Which sizing rule this group answers to. */
    public enum Kind {
        /** Sized by free player slots; servers are interchangeable. */
        EPHEMERAL,
        /** Sized by a number a person wrote down; servers own their worlds. */
        PERSISTENT,
        /** A group of proxies. */
        PROXY,
        /** A kind this jar predates. See {@link ServerPhase#UNKNOWN}. */
        UNKNOWN
    }
}
```

`CloudPlayer.java`:

```java
package cloud.spawnery.agent.api;

import java.util.Objects;
import java.util.Optional;
import java.util.UUID;

/**
 * A player, and which backend they are on.
 *
 * @param server empty when the proxy has them and no backend does: during the
 *     login handshake, and between one backend and the next. It is not an
 *     error and a plugin must handle it -- a player in flight is exactly the
 *     player a drain is about.
 */
public record CloudPlayer(UUID id, String name, Optional<String> server) {
    public CloudPlayer {
        Objects.requireNonNull(id, "id");
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(server, "server");
    }
}
```

- [ ] **Step 4: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :api:test --console=plain'`

Expected: PASS, including `PackagingInvariantTest` — which now has real
signatures to walk, with `java.util.Optional` and `java.util.UUID` in them.

- [ ] **Step 5: Commit**

```bash
git add agent/api/src
git commit -m "$(cat <<'EOF'
feat(agent-api): the values a plugin reads

Records, because they are values: two descriptions of the same server at the
same moment are equal, and a plugin putting one in a Set gets that without
writing anything. Each compact constructor refuses a null where it is built,
rather than handing the caller an NPE at some unrelated line later.

Two decisions are about a version skew nobody has hit yet. ServerPhase and
Group.Kind both carry UNKNOWN and are read through a fromWire that never
throws, because the operator may publish a value this jar predates and a
plugin that threw on one would break on an operator upgrade it had nothing
to do with.

CloudPlayer.server is an Optional and its absence is documented as ordinary
rather than exceptional: a player between two backends is exactly the player
a drain is about, and milestone 6's own drain gap was a player nobody
counted.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 4: `SpawneryApi` and the way a plugin gets one

**Files:**
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java`
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/Spawnery.java`
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryUnavailableException.java`
- Test: `agent/api/src/test/java/cloud/spawnery/agent/api/SpawneryTest.java`

**Interfaces:**
- Consumes: `Self`, `Group`, `ServerInfo`, `CloudPlayer` from Tasks 1 and 3.
- Produces: `SpawneryApi` (read surface only), `Spawnery.api()`,
  `Spawnery.install(SpawneryApi)` for the agent to call, and
  `SpawneryUnavailableException`.

- [ ] **Step 1: Write the failing test**

```java
package cloud.spawnery.agent.api;

import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

class SpawneryTest {
    @AfterEach
    void clear() {
        Spawnery.uninstall();
    }

    @Test
    void withNoAgentTheFailureNamesTheRemedy() {
        var e = assertThrows(SpawneryUnavailableException.class, Spawnery::api);
        // The two cases have different remedies -- the plugin is missing, or
        // it has not finished enabling -- and a null return could say neither.
        assertTrue(e.getMessage().contains("spawnery"),
                "the message must name the plugin a server owner has to install: " + e.getMessage());
    }

    @Test
    void onceInstalledTheSameInstanceComesBack() {
        SpawneryApi api = new FakeApi();
        Spawnery.install(api);
        assertSame(api, Spawnery.api());
    }

    @Test
    void installingTwiceIsRefusedRatherThanSilentlyWinning() {
        Spawnery.install(new FakeApi());
        assertThrows(IllegalStateException.class, () -> Spawnery.install(new FakeApi()));
    }
}
```

Beside it, `FakeApi.java` under `src/test/java` in the same package. It exists
so this test needs no mocking framework — which would be a dependency, and this
module has none:

```java
package cloud.spawnery.agent.api;

import java.util.List;
import java.util.Optional;
import java.util.UUID;

/** An implementation that answers nothing, for tests about the holder. */
final class FakeApi implements SpawneryApi {
    @Override
    public Self self() {
        return new ProxySelf() {
            @Override public String name() { return "gateway-0"; }
            @Override public String group() { return "gateway"; }
            @Override public String namespace() { return "minecraft"; }
        };
    }

    @Override public List<Group> groups() { return List.of(); }
    @Override public Optional<Group> group(String name) { return Optional.empty(); }
    @Override public List<ServerInfo> servers() { return List.of(); }
    @Override public Optional<ServerInfo> server(String name) { return Optional.empty(); }
    @Override public List<CloudPlayer> players() { return List.of(); }
    @Override public Optional<CloudPlayer> player(UUID id) { return Optional.empty(); }
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :api:test --console=plain'`

Expected: a compile failure — `Spawnery` does not exist.

- [ ] **Step 3: Write the interface**

`SpawneryApi.java`, with the Apache header:

```java
package cloud.spawnery.agent.api;

import java.util.List;
import java.util.Optional;
import java.util.UUID;

/**
 * What a plugin can ask the cloud, from either side of the proxy.
 *
 * <p><b>Every method here is a local read.</b> The operator keeps a mirror
 * current in each agent, so none of these calls crosses a network, blocks, or
 * fails -- there is no timeout and no exception to handle. What they return is
 * the last thing the operator said, which during a reconnect may be a few
 * seconds old and is never wrong about a moment that happened.
 *
 * <p><b>Consume this interface; do not implement it.</b> Methods are added
 * here as later milestones land -- events, moving a player, starting a server
 * -- and adding one breaks an implementor while leaving every caller alone.
 * The agent supplies the implementation; a plugin obtains it from
 * {@link Spawnery#api()}.
 *
 * <p>Everything is scoped to this pod's own namespace, which is the whole of
 * what this agent can see. There is no method that reaches another network,
 * and that is structural rather than a check somebody could forget.
 */
public interface SpawneryApi {
    /** What this process is. Use {@code instanceof} to learn which side. */
    Self self();

    /** Every group in this network, in no particular order. */
    List<Group> groups();

    /** One group by name, empty if this network has none. */
    Optional<Group> group(String name);

    /** Every server in this network, in no particular order. */
    List<ServerInfo> servers();

    /** One server by name, empty if this network has none. */
    Optional<ServerInfo> server(String name);

    /** Every player on this network, whichever backend they are on. */
    List<CloudPlayer> players();

    /** One player by UUID, empty if they are not on this network. */
    Optional<CloudPlayer> player(UUID id);
}
```

- [ ] **Step 4: Write the entry point**

`SpawneryUnavailableException.java`:

```java
package cloud.spawnery.agent.api;

/**
 * Thrown by {@link Spawnery#api()} when no agent has installed one.
 *
 * <p>An exception and not a null return, because the two ways to get here have
 * different remedies and a null could tell them apart for nobody: either the
 * agent plugin is not installed at all, which is a server owner's problem, or
 * it has not finished enabling, which is a plugin load-order problem. The
 * message names both.
 */
public class SpawneryUnavailableException extends IllegalStateException {
    private static final long serialVersionUID = 1L;

    public SpawneryUnavailableException(String message) {
        super(message);
    }
}
```

`Spawnery.java`:

```java
package cloud.spawnery.agent.api;

import java.util.Objects;
import java.util.concurrent.atomic.AtomicReference;

/**
 * How a plugin gets a {@link SpawneryApi}, in one line and the same line on
 * both platforms.
 *
 * <p>A static holder rather than each platform's service registry, and that is
 * the point: Bukkit has {@code ServicesManager}, Velocity has Guice, and a
 * plugin author moving between them should not have to learn which. The agent
 * calls {@link #install} once as it enables; everybody else calls
 * {@link #api()}.
 */
public final class Spawnery {
    private static final AtomicReference<SpawneryApi> INSTALLED = new AtomicReference<>();

    private Spawnery() {
    }

    /**
     * The API this server's agent installed.
     *
     * @throws SpawneryUnavailableException if no agent has installed one --
     *     see that type for the two ways that happens.
     */
    public static SpawneryApi api() {
        SpawneryApi api = INSTALLED.get();
        if (api == null) {
            throw new SpawneryUnavailableException(
                    "no Spawnery agent has installed an API on this server. Either the spawnery "
                            + "agent plugin is not installed, or it has not finished enabling yet -- "
                            + "call this from your own enable step or later, not from a constructor.");
        }
        return api;
    }

    /** Whether {@link #api()} would return rather than throw. */
    public static boolean isAvailable() {
        return INSTALLED.get() != null;
    }

    /**
     * Installs the implementation. Called by the agent and by nothing else.
     *
     * <p>Refuses a second install rather than replacing the first: two agents
     * on one server is a misconfiguration, and the failure mode of letting the
     * second win is that half the plugins hold a handle to a dead one.
     *
     * @throws IllegalStateException if one is already installed
     */
    public static void install(SpawneryApi api) {
        Objects.requireNonNull(api, "api");
        if (!INSTALLED.compareAndSet(null, api)) {
            throw new IllegalStateException(
                    "a Spawnery API is already installed on this server; two agent plugins "
                            + "are running where there should be one");
        }
    }

    /** Removes the installed API. For the agent's own shutdown, and for tests. */
    public static void uninstall() {
        INSTALLED.set(null);
    }
}
```

- [ ] **Step 5: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :api:test --console=plain'`

Expected: PASS, every suite.

- [ ] **Step 6: Build the agents and look in the jar again**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make agent`

Then repeat Task 1 Step 6's jar inspection.

Expected: both jars now list every class of this module —
`SpawneryApi`, `Spawnery`, `SpawneryUnavailableException`, `Self`,
`ServerSelf`, `ProxySelf`, `Group`, `Group$Kind`, `ServerInfo`, `ServerPhase`,
`CloudPlayer`.

- [ ] **Step 7: Commit**

```bash
git add agent/api/src
git commit -m "$(cat <<'EOF'
feat(agent-api): the read surface, and one line to obtain it

Spawnery.api() is the same line on a Paper server and on a Velocity proxy,
which is what a static holder buys over each platform's own registry: Bukkit
has ServicesManager, Velocity has Guice, and a plugin author moving between
them should not have to learn which.

install() refuses a second implementation rather than replacing the first.
Two agents on one server is a misconfiguration either way, and the failure
mode of letting the second win is that half the plugins hold a handle to a
dead one.

api() throws rather than returning null, because the two ways to get there
need different remedies -- the plugin is not installed, or it has not
finished enabling -- and a null tells a caller neither. The message names
both.

SpawneryApi carries the read methods alone. events(), connect() and
lifecycle() wait for the milestones that can deliver them: the mirror those
methods would read does not exist yet, and the server side has no downstream
path at all. Adding a method later is safe in the direction that matters,
and the javadoc says so -- plugins consume this interface and never
implement it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 5: What a plugin author has to do

An API nobody can find is an API nobody uses, and the one rule that breaks a
consumer — bundling the module — is invisible until it fails at runtime.

**Files:**
- Create: `agent/api/README.md`
- Modify: `docs/README.md` (the index gains it)

- [ ] **Step 1: Write the module's README**

Create `agent/api/README.md`:

````markdown
# The Spawnery plugin API

What a Paper or Velocity plugin can ask the cloud, with the same calls on both.

```java
if (Spawnery.isAvailable()) {
    SpawneryApi api = Spawnery.api();
    for (ServerInfo s : api.servers()) {
        getLogger().info(s.name() + " has " + s.freeSlots() + " free slots");
    }
}
```

## Depending on it

**`compileOnly`, always.** The classes are loaded from the running agent
plugin, and a plugin that bundles its own copy puts a second
`cloud.spawnery.agent.api.SpawneryApi` on the server — a different type with
the same name, so the cast at your first call fails with a message about two
classes that look identical.

```kotlin
dependencies {
    compileOnly("cloud.spawnery:spawnery-api:<version>")
}
```

Your plugin must declare a dependency on the `spawnery` plugin so it enables
after the agent. `Spawnery.api()` throws
`SpawneryUnavailableException` if you call it before that, and the message says
which of the two causes it was.

## What it can see

Everything is scoped to the pod's own namespace, which is one `Network`. There
is no call that reaches another network, and that is structural rather than a
check: the agent's own credentials are a pod-bound ServiceAccount token, so
there is nothing to widen.

## What is a value and what is a moment

`ServerInfo`, `Group` and `CloudPlayer` are records describing what the
operator last said. They do not update. Ask again for a newer one.

Reads never block, never time out, and throw nothing: the operator keeps a
mirror current inside the agent, so `servers()` is a lookup in a local map.

## Version skew

`ServerPhase` and `Group.Kind` both carry `UNKNOWN`, and the operator is free
to publish a value your copy of this jar predates. Handle it. A `switch` that
throws on an unrecognised phase breaks on an operator upgrade that had nothing
to do with your plugin.
````

- [ ] **Step 2: Add it to the docs index**

In `docs/README.md`, in the "Working on Spawnery" table, add a row after the
`development.md` one:

```markdown
| [`../agent/api/README.md`](../agent/api/README.md) | The plugin API: what a Paper or Velocity plugin can ask the cloud, and the one rule that breaks a consumer |
```

- [ ] **Step 3: Verify every link in the new files resolves**

```bash
for f in docs/README.md agent/api/README.md; do
  d=$(dirname "$f")
  grep -o '](\([^)]*\))' "$f" | sed 's/](//; s/)$//' | grep -v '^http' | while read -r t; do
    base="${t%%#*}"; [ -z "$base" ] && continue
    [ -e "$d/$base" ] || echo "BROKEN in $f -> $t"
  done
done; echo "link check done"
```

Expected: no `BROKEN` lines.

- [ ] **Step 4: Commit**

```bash
git add agent/api/README.md docs/README.md
git commit -m "$(cat <<'EOF'
docs(agent-api): what a plugin author has to do, and the one thing they must not

The rule that breaks a consumer is invisible until it fails: bundling the API
puts a second cloud.spawnery.agent.api.SpawneryApi on the server, a different
type with the same name, and the cast at the first call fails with a message
about two classes that look identical. compileOnly is the whole answer and it
is the first thing this page says.

Also the two things a plugin author would otherwise learn from a crash: that
reads are local and throw nothing, and that ServerPhase carries UNKNOWN
because the operator may publish a value their jar predates.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Done when

- [ ] `make agent` is green, with `:api:test` among the suites that ran
- [ ] Both shipped jars carry every class of `cloud/spawnery/agent/api/`,
      verified by opening them rather than inferred from a green build
- [ ] `hack/agent-jar-check.sh` passes on both jars, unchanged
- [ ] `agent/api/build.gradle.kts` declares no dependency but the test framework
- [ ] The mutation in Task 2 Step 3 was run, failed as predicted, and reverted
- [ ] `make lint` reports nothing (it lints Go; this plan touches none, so a
      finding here means something unrelated crept in)
- [ ] Nothing was pushed and no tag was created

## What 7b-1 leaves for its successors

- **Nothing implements `SpawneryApi`.** 7b-3 does, once there is a mirror to
  read from.
- **§6.2's second invariant is 7b-3's.** "The two platforms expose the same
  surface" is a test comparing the Paper and Velocity implementations, and
  there are none to compare. It is named here so it is not lost: without it,
  the symmetry this whole design is about is checked by nobody.
- **`agent/deps.json` is untouched.** This module has no external dependency,
  so nothing reaches Maven Central. If `make agent` ever asks for a
  regeneration after this plan, something acquired a dependency it should not
  have — read Task 2's test before adding one.
- **No artifact is published.** `agent/api/README.md` shows a `compileOnly`
  coordinate that nothing resolves yet; publishing it is its own decision and
  its own plan, and until then a plugin author builds against the jar from a
  release.
