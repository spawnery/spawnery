# Paper Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Kotlin Paper plugin that opens a `ServerSession` to the operator and reports readiness and player counts, so a `Server` reaches phase `Ready`.

**Architecture:** A Gradle project under `agent/paper/`, built reproducibly in Nix through the nixpkgs Gradle setup hook with a checked-in per-artifact lockfile. It compiles against the Paper API that already ships inside the pinned Paper bundle, shades and relocates its gRPC stack away from Paper's own protobuf and Netty, and is baked into the base image where the entrypoint copies it into a writable plugins directory. Proof runs on three levels: JUnit against an in-process gRPC server, the real image against a Go stub operator, and the existing offline image test.

**Tech Stack:** Kotlin 2.x (via the Kotlin Gradle Plugin), Gradle 8.14.4 on JDK 21, grpc-java 1.83.1 with `grpc-okhttp`, the Shadow plugin, Nix (`gradle.fetchDeps`), Go (the stub operator), POSIX sh (the entrypoint).

## Global Constraints

These bind every task. Values are copied verbatim from
`docs/superpowers/specs/2026-08-09-paper-agent-design.md` and from measurements
recorded there.

- **Everything in this repository is written in English** — code, comments,
  commit messages, documentation. No exceptions.
- **Every commit ends with the trailer** `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **Commit messages describe why, not what.** Match the existing history's
  register (`git log` on master); no `feat:`/`fix:` prefixes — the repository
  does not use them.
- **grpc-java version is `1.83.1` everywhere.** That is the version of
  `protoc-gen-grpc-java` in the pinned nixpkgs; codegen and runtime must not
  drift apart.
- **JVM target is 21.** `jvmTarget = "21"`, `sourceCompatibility`/
  `targetCompatibility` = 21. No Java 25 toolchain, no
  `gradle.override { javaToolchains = … }`, and Gradle's toolchain
  auto-provisioning stays off.
- **The generated Java lives in its own source set** whose compile classpath
  carries gRPC and protobuf only, never the Paper libraries. `javac` 21 cannot
  read the major-69 Paper jars if it ever has to resolve a class out of them.
- **The Paper API comes from `packages.paper-repo`, never from a Maven
  repository.** Compile-only, and it must not appear in `deps.json`.
- **Relocation prefix is `cloud.spawnery.agent.shaded.`** Paper carries
  `protobuf-java 4.29.0` and `netty 4.2.15.Final` on its own classpath.
- **The jar must be bit-reproducible.** `make image-repro` compares two image
  builds byte for byte and the agent jar is inside the image.
- **`make test` stays Go-only and must not get slower.** It is 23.9 s today.
- **Paper facts, measured, not to be re-derived:** Paper version `26.2`, build
  `111`; `api-version` is `26.2` (from `apiVersioning.json` inside
  `paper-api-26.2.build.111-stable.jar`); the API jar is at
  `<paper-repo>/libraries/io/papermc/paper/paper-api/26.2.build.111-stable/paper-api-26.2.build.111-stable.jar`.
- **Pod contract, from `internal/podspec`, unchanged:** token at
  `/var/run/spawnery/token`, CA at `/var/run/spawnery/ca.crt`, endpoint in
  `SPAWNERY_OPERATOR_ENDPOINT`, context in `SPAWNERY_NETWORK`,
  `SPAWNERY_GROUP`, `SPAWNERY_SERVER`, `SPAWNERY_MAX_PLAYERS`.
- **The version string is `26.2-0.2.0`** — the image tag, the plugin version,
  and what `Hello.version` reports. One string, one source.
- **Timing is injected, never read from a global clock.** Every unit that
  schedules work takes a `ScheduledExecutorService`, so tests drive time
  instead of sleeping.

---

### Task 1: The Gradle project builds reproducibly in Nix

The riskiest step, deliberately first: if `gradle.fetchDeps` cannot produce a
lockfile for this dependency set, everything downstream changes shape. The full
runtime dependency set is declared here even though most of it is unused until
Task 5, so `deps.json` is generated once rather than churning every task.

**Files:**
- Create: `agent/paper/settings.gradle.kts`
- Create: `agent/paper/build.gradle.kts`
- Create: `agent/paper/gradle.properties`
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/ServerState.kt`
- Create: `agent/paper/src/test/kotlin/cloud/spawnery/agent/ServerStateTest.kt`
- Create: `agent/paper/deps.json` (generated, checked in)
- Create: `nix/paper-agent.nix`
- Modify: `flake.nix` (dev shell packages, `packages.paper-agent`)
- Modify: `Makefile` (`agent`, `agent-deps` targets, `all`)
- Modify: `.gitignore`

**Interfaces:**
- Consumes: `packages.paper-repo` from `flake.nix` (already exists).
- Produces: `packages.paper-agent` (a derivation whose `$out/share/spawnery/spawnery-agent.jar` is the shaded plugin jar — shading arrives in Task 3, the path is fixed from here). `cloud.spawnery.agent.ServerState` with the API below.

- [ ] **Step 1: Write the failing test**

`agent/paper/src/test/kotlin/cloud/spawnery/agent/ServerStateTest.kt`:

```kotlin
package cloud.spawnery.agent

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class ServerStateTest {
    @Test
    fun `starts not ready with no players`() {
        val state = ServerState()
        assertFalse(state.ready)
        assertEquals(0, state.players)
        assertEquals(0, state.slots)
    }

    @Test
    fun `markReady reports whether this was the transition`() {
        val state = ServerState()
        assertTrue(state.markReady(), "the first markReady is the transition")
        assertFalse(state.markReady(), "a repeated markReady is not")
        assertTrue(state.ready)
    }

    @Test
    fun `sample overwrites the last measurement`() {
        val state = ServerState()
        state.sample(players = 3, slots = 100)
        assertEquals(3, state.players)
        assertEquals(100, state.slots)
        state.sample(players = 7, slots = 100)
        assertEquals(7, state.players)
    }
}
```

- [ ] **Step 2: Write the Gradle project**

`agent/paper/settings.gradle.kts`:

```kotlin
rootProject.name = "spawnery-paper-agent"
```

`agent/paper/gradle.properties`:

```properties
org.gradle.parallel=false
org.gradle.caching=false
# Gradle must never reach for a JDK download. Everything compiles on the JDK
# Gradle itself runs on (21), which the design measured to be sufficient.
org.gradle.java.installations.auto-detect=false
org.gradle.java.installations.auto-download=false
```

`agent/paper/build.gradle.kts`:

```kotlin
plugins {
    // 2.4.10 and not lower: that is the version measured to read the
    // class-file-major-69 Paper jars on the compile classpath. An older Kotlin
    // may not, and the failure is an UnsupportedClassVersionError that names
    // the jar rather than the compiler.
    kotlin("jvm") version "2.4.10"
}

group = "cloud.spawnery"
version = providers.gradleProperty("agentVersion").getOrElse("0.0.0-dev")

repositories {
    mavenCentral()
}

// The Paper API comes from the pinned Paper bundle, never from a Maven
// repository, so the plugin cannot compile against a different API than the
// server that loads it. nix/paper-agent.nix symlinks packages.paper-repo here
// before the build; a developer running Gradle by hand creates the same link:
//
//   ln -sfn "$(nix build .#paper-repo --no-link --print-out-paths)" agent/paper/paper-repo
val paperLibraries = fileTree("paper-repo/libraries") { include("**/*.jar") }

// The generated protobuf and gRPC stubs live in their own source set. Its
// compile classpath must never contain paperLibraries: those jars are class
// file major 69, and javac 21 fails the moment it has to resolve a class out
// of one. Keeping them apart makes that impossible rather than unlikely.
val proto: SourceSet by sourceSets.creating {
    java.srcDir("src/proto/java")
}

val protoImplementation: Configuration by configurations.getting

dependencies {
    protoImplementation("io.grpc:grpc-api:1.83.1")
    protoImplementation("io.grpc:grpc-protobuf:1.83.1")
    protoImplementation("io.grpc:grpc-stub:1.83.1")
    protoImplementation("com.google.protobuf:protobuf-java:4.29.0")
    // The generated stubs carry @javax.annotation.Generated.
    protoImplementation("javax.annotation:javax.annotation-api:1.3.2")

    implementation(proto.output)
    implementation("io.grpc:grpc-okhttp:1.83.1")
    implementation("io.grpc:grpc-protobuf:1.83.1")
    implementation("io.grpc:grpc-stub:1.83.1")
    implementation("com.google.protobuf:protobuf-java:4.29.0")

    compileOnly(paperLibraries)

    testImplementation(proto.output)
    testImplementation(kotlin("test"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    testImplementation("io.grpc:grpc-inprocess:1.83.1")
    testImplementation("io.grpc:grpc-testing:1.83.1")
    testImplementation("org.bouncycastle:bcpkix-jdk18on:1.79")
    testImplementation(paperLibraries)
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_21)
    }
}

java {
    sourceCompatibility = JavaVersion.VERSION_21
    targetCompatibility = JavaVersion.VERSION_21
}

tasks.test {
    useJUnitPlatform()
    testLogging { showStandardStreams = true }
}

// make image-repro compares two image builds byte for byte, and this jar is
// inside the image. Without these two flags the archive carries build
// timestamps and a filesystem-order entry list, and the comparison fails for
// reasons that have nothing to do with the code.
tasks.withType<AbstractArchiveTask>().configureEach {
    isPreserveFileTimestamps = false
    isReproducibleFileOrder = true
}
```

`agent/paper/src/main/kotlin/cloud/spawnery/agent/ServerState.kt`:

```kotlin
package cloud.spawnery.agent

import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger

/**
 * The two facts the agent reports, held where both the Bukkit main thread and
 * the network thread can reach them.
 *
 * The main thread writes through [sample] and [markReady]; the network side
 * only reads. No Bukkit call happens from a gRPC callback, because
 * Bukkit.getOnlinePlayers() is not thread-safe.
 *
 * There is deliberately no way to clear [ready]. Hello{ready:false} cannot
 * lower a readiness the operator's registry has already recorded (see
 * docs/known-issues.md), so representing that state here would only invite
 * code that tries to express it.
 */
class ServerState {
    private val readyFlag = AtomicBoolean(false)
    private val playerCount = AtomicInteger(0)
    private val slotCount = AtomicInteger(0)

    val ready: Boolean get() = readyFlag.get()
    val players: Int get() = playerCount.get()
    val slots: Int get() = slotCount.get()

    /** Returns true only for the call that made the transition. */
    fun markReady(): Boolean = readyFlag.compareAndSet(false, true)

    fun sample(players: Int, slots: Int) {
        playerCount.set(players)
        slotCount.set(slots)
    }
}
```

`.gitignore` gains:

```
agent/paper/paper-repo
agent/paper/.gradle/
agent/paper/build/
```

- [ ] **Step 3: Write the Nix derivation**

`nix/paper-agent.nix`:

```nix
# The Spawnery Paper agent plugin.
#
# Dependencies come through the nixpkgs Gradle setup hook, whose lockfile
# (agent/paper/deps.json) carries one SHA-256 per artifact and is checked in.
# The Paper API does not: it is symlinked in from the already-pinned Paper
# bundle, so it can never drift from the server that loads the plugin.
{ lib
, stdenv
, gradle
, paper
, imageVersion ? "0.2.0"
}:

stdenv.mkDerivation (finalAttrs: {
  pname = "spawnery-paper-agent";
  version = "${paper.paperVersion}-${imageVersion}";

  src = ../agent/paper;

  nativeBuildInputs = [ gradle ];

  mitmCache = gradle.fetchDeps {
    pkg = finalAttrs.finalPackage;
    data = ../agent/paper/deps.json;
  };

  # Gradle resolves the Paper API through this link, not through a repository.
  postPatch = ''
    ln -sfn ${paper.repo} paper-repo
  '';

  gradleFlags = [ "-PagentVersion=${finalAttrs.version}" ];
  gradleBuildTask = "jar";

  doCheck = true;

  installPhase = ''
    runHook preInstall
    install -Dm644 build/libs/*.jar $out/share/spawnery/spawnery-agent.jar
    runHook postInstall
  '';

  meta = {
    description = "Spawnery agent plugin for Paper";
    platforms = lib.platforms.all;
  };
})
```

`flake.nix`: add `protoc-gen-grpc-java`, `gradle` and `jdk21_headless` to the
dev shell `packages` list, and inside the `packages` `let` block, after
`spawnery-slp`:

```nix
          paper-agent = pkgs.callPackage ./nix/paper-agent.nix {
            inherit paper;
          };
```

and expose it in the always-available attribute set (next to `paper-repo` and
`spawnery-slp`, **not** inside the `x86_64-linux` block — it is a JVM build,
not a Linux image):

```nix
          inherit spawnery-slp paper-agent;
```

`Makefile`:

```makefile
.PHONY: agent
agent:
	nix build .#paper-agent

# Regenerates agent/paper/deps.json. Runs outside the Nix sandbox because it
# has to reach Maven Central, so it is deliberately in no other target: a
# dependency change is an explicit act, not a side effect of `make all`.
.PHONY: agent-deps
agent-deps:
	nix run --impure .#paper-agent.mitmCache.updateScript
```

and `all` becomes:

```makefile
all: proto manifests generate fmt vet test build agent
```

- [ ] **Step 4: Generate the lockfile**

Run: `make agent-deps`

Expected: `agent/paper/deps.json` appears, containing a
`"https://repo.maven.apache.org/maven2"` object with one entry per artifact,
each holding a `sha256-…` value. If the update script cannot be resolved under
that attribute path, find the right invocation with
`nix eval .#paper-agent.mitmCache.updateScript` and record the working command
in the Makefile — do not hand-write the file.

- [ ] **Step 5: Run the build and verify the test runs and passes**

Run: `nix build .#paper-agent -L 2>&1 | tail -40`

Expected: the Gradle `test` task runs and reports 3 passing tests in
`ServerStateTest`, then `result/share/spawnery/spawnery-agent.jar` exists.

If this fails on toolchain resolution, the fix is in `gradle.properties`, not a
toolchain override — see the Global Constraints.

- [ ] **Step 6: Verify the test actually gates the build**

Temporarily change `assertEquals(0, state.slots)` to `assertEquals(1, state.slots)`.

Run: `nix build .#paper-agent -L 2>&1 | tail -20`

Expected: the build FAILS with a JUnit assertion failure. This proves `doCheck`
is wired; a green build whose tests never ran is the failure mode this step
exists to rule out. Revert the change afterwards and rebuild to green.

- [ ] **Step 7: Verify reproducibility**

Run: `nix build .#paper-agent --rebuild`

Expected: exit 0, no `differs` output.

- [ ] **Step 8: Commit**

```bash
git add agent/paper nix/paper-agent.nix flake.nix Makefile .gitignore
git commit
```

---

### Task 2: The stubs come from the same `.proto` as the Go side

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto` (two additive Java options)
- Modify: `Makefile` (`proto` target)
- Modify: `flake.nix` (dev shell already gained `protoc-gen-grpc-java` in Task 1 — verify it is there)
- Create: `agent/paper/src/proto/java/**` (generated, checked in)
- Create: `agent/paper/src/test/kotlin/cloud/spawnery/agent/ContractTest.kt`

**Interfaces:**
- Consumes: the `proto` source set from Task 1.
- Produces: Java classes in package `cloud.spawnery.agent.pb` — `ServerMessage`, `OperatorToServer`, `Hello`, `Ready`, `PlayerCount`, `ReportInterval`, `SessionDeadline`, and the service stub `AgentServiceGrpc` with `AgentServiceGrpc.newStub(channel).serverSession(responseObserver)`.

The `.proto` gains two options. This is the one change to that file this
milestone makes, it is additive, and Step 3 proves it changes nothing on the Go
side. Without them the generated Java lands in package `spawnery.agent.v1alpha1`
inside a single outer class, which every later task would have to work around.

- [ ] **Step 1: Add the Java options**

In `proto/spawnery/agent/v1alpha1/agent.proto`, directly below the existing
`option go_package` line:

```protobuf
option java_package = "cloud.spawnery.agent.pb";
option java_multiple_files = true;
option java_outer_classname = "AgentProto";
```

- [ ] **Step 2: Extend the proto target**

In `Makefile`, the `proto` target becomes:

```makefile
.PHONY: proto
proto:
	protoc \
		--proto_path=proto \
		--go_out=. --go_opt=module=github.com/spawnery/spawnery \
		--go-grpc_out=. --go-grpc_opt=module=github.com/spawnery/spawnery \
		proto/spawnery/agent/v1alpha1/agent.proto
	rm -rf agent/paper/src/proto/java
	mkdir -p agent/paper/src/proto/java
	protoc \
		--proto_path=proto \
		--java_out=agent/paper/src/proto/java \
		--grpc-java_out=agent/paper/src/proto/java \
		proto/spawnery/agent/v1alpha1/agent.proto
```

- [ ] **Step 3: Regenerate and prove the Go side is untouched**

> **Correction, made during execution.** This step originally demanded that
> `git diff --stat internal/agentpb/` be **empty**, and the implementer
> correctly stopped when it was not. The demand was impossible, not the
> assumption behind it: a `FileDescriptorProto` carries one `FileOptions` block
> shared by every target language, and `protoc-gen-go` embeds the whole
> serialized descriptor in `rawDesc` for reflection — so *any* additive
> file-level option for *any* language moves those bytes. Verified on the real
> diff: the only change is inside the `rawDesc` string literal, where
> `java_package`, `java_outer_classname` and `java_multiple_files` now appear
> beside an unchanged `go_package`. No exported Go symbol, message name or
> field number moves.
>
> The check below replaces it and tests what the original meant to test.

Run:

```bash
nix develop -c make proto
git diff --stat internal/agentpb/
git diff internal/agentpb/ | grep -E '^[+-][[:space:]]*(func|type|const|var) ' || echo "no Go API change"
nix develop -c go test ./internal/agentpb/...
```

Expected, all four:

1. `agent.pb.go` is the only file changed, by about four lines.
   `agent_grpc.pb.go` must be untouched — the service surface is what a
   consumer actually binds to.
2. The diff is confined to the `file_spawnery_agent_v1alpha1_agent_proto_rawDesc`
   literal, and the substring `Z-github.com/spawnery/spawnery/internal/agentpb`
   still appears in it.
3. `no Go API change` — no added or removed `func`, `type`, `const` or `var`.
4. The existing Go tests pass **unmodified**. If a Go test needs editing to
   stay green, stop and report: that is the wire-compatibility break this step
   exists to catch.

Then: `git status --short agent/paper/src/proto/java | head`

Expected: new files under `agent/paper/src/proto/java/cloud/spawnery/agent/pb/`.

- [ ] **Step 4: Write the failing contract test**

`agent/paper/src/test/kotlin/cloud/spawnery/agent/ContractTest.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.Hello
import cloud.spawnery.agent.pb.PlayerCount
import cloud.spawnery.agent.pb.ServerMessage
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

/**
 * The Kotlin counterpart of internal/agentpb/contract_test.go. It does not test
 * protobuf; it tests that the checked-in stubs were generated from the .proto
 * this repository holds, which is the thing that silently rots.
 */
class ContractTest {
    @Test
    fun `a server message round-trips through the wire format`() {
        val sent = ServerMessage.newBuilder()
            .setHello(Hello.newBuilder().setVersion("26.2-0.2.0").setReady(true))
            .build()

        val back = ServerMessage.parseFrom(sent.toByteArray())

        assertEquals(ServerMessage.MessageCase.HELLO, back.messageCase)
        assertEquals("26.2-0.2.0", back.hello.version)
        assertEquals(true, back.hello.ready)
    }

    @Test
    fun `player count carries both numbers`() {
        val sent = ServerMessage.newBuilder()
            .setPlayerCount(PlayerCount.newBuilder().setPlayers(3).setSlots(100))
            .build()

        val back = ServerMessage.parseFrom(sent.toByteArray())

        assertEquals(3, back.playerCount.players)
        assertEquals(100, back.playerCount.slots)
    }
}
```

- [ ] **Step 5: Run the build**

Run: `nix build .#paper-agent -L 2>&1 | tail -30`

Expected: 5 tests pass (3 from `ServerStateTest`, 2 from `ContractTest`).

- [ ] **Step 6: Commit**

```bash
git add proto Makefile internal/agentpb agent/paper
git commit
```

---

### Task 3: Shading, relocation, and a check that proves it

**Files:**
- Modify: `agent/paper/build.gradle.kts`
- Create: `hack/agent-jar-check.sh`
- Modify: `nix/paper-agent.nix`

**Interfaces:**
- Consumes: the build file and derivation from Task 1, the stubs from Task 2.
- Produces: `$out/share/spawnery/spawnery-agent.jar` is now the *shaded* jar. `hack/agent-jar-check.sh <jar>` exits 0 if the relocation holds.

- [ ] **Step 1: Write the failing check**

> **Correction, made during execution.** The script below is the version this
> task shipped, and the final whole-branch review found four blind spots in it:
> `com.google.gson` unchecked although Paper ships gson-2.14.0; the presence of
> `paper-plugin.yml` checked but not that its `${version}` expanded; nothing
> asserting the generated protobuf stubs are in the jar; and a collision message
> that is false for okio, okhttp3 and perfmark. The shipped
> `hack/agent-jar-check.sh` also gained a no-`.java` guard. Read that file, not
> this block.

`hack/agent-jar-check.sh`:

```bash
#!/usr/bin/env bash
# Checks that the agent jar's dependencies were relocated.
#
# Paper carries protobuf-java 4.29.0 and netty 4.2.15 on its own classpath (see
# <paper-repo>/libraries). A plugin that ships an unrelocated com/google/protobuf
# meets Paper's copy at class load, and the symptom is a NoSuchMethodError deep
# inside gRPC that names neither the plugin nor the conflict. This check is what
# keeps that from being discovered in a pod.
set -euo pipefail

JAR="${1:?usage: agent-jar-check.sh <jar>}"

entries="$(unzip -Z1 "$JAR")"

fail() {
	echo "agent-jar-check: $1" >&2
	exit 1
}

# Relocated packages must be present under the prefix...
grep -q '^cloud/spawnery/agent/shaded/com/google/protobuf/' <<<"$entries" ||
	fail "protobuf was not relocated under cloud/spawnery/agent/shaded/"
grep -q '^cloud/spawnery/agent/shaded/io/grpc/' <<<"$entries" ||
	fail "grpc was not relocated under cloud/spawnery/agent/shaded/"

# ...and absent at their original coordinates.
for pkg in com/google/protobuf io/grpc com/google/common okio com/squareup/okhttp3 io/perfmark; do
	if grep -q "^$pkg/" <<<"$entries"; then
		fail "$pkg is present unrelocated; it would meet Paper's own copy"
	fi
done

# gRPC resolves its transport through ServiceLoader. Relocation renames the
# provider classes, so the service files have to be merged and rewritten with
# them; without that the channel fails at runtime with "no functional channel
# service provider found" and nothing points at the shading as the cause.
grep -q '^META-INF/services/cloud.spawnery.agent.shaded.io.grpc.ManagedChannelProvider$' <<<"$entries" ||
	fail "the relocated ManagedChannelProvider service file is missing"

# The plugin descriptor is what makes this a plugin at all.
grep -q '^paper-plugin.yml$' <<<"$entries" ||
	fail "paper-plugin.yml is missing from the jar"

echo "agent-jar-check: ok"
```

Make it executable: `chmod +x hack/agent-jar-check.sh`

Note: the `paper-plugin.yml` assertion fails until Task 7. That is intentional
and is resolved in Step 5 below.

- [ ] **Step 2: Run it against the current jar to see it fail**

Run: `nix build .#paper-agent --no-link --print-out-paths` then
`nix develop -c hack/agent-jar-check.sh <that path>/share/spawnery/spawnery-agent.jar`

Expected: FAIL with "protobuf was not relocated under cloud/spawnery/agent/shaded/".

- [ ] **Step 3: Add the Shadow plugin and the relocations**

In `agent/paper/build.gradle.kts`, the `plugins` block becomes:

```kotlin
plugins {
    // 2.4.10 and not lower: that is the version measured to read the
    // class-file-major-69 Paper jars on the compile classpath. An older Kotlin
    // may not, and the failure is an UnsupportedClassVersionError that names
    // the jar rather than the compiler.
    kotlin("jvm") version "2.4.10"
    id("com.gradleup.shadow") version "9.0.0"
}
```

and at the end of the file:

```kotlin
tasks.shadowJar {
    archiveClassifier.set("")

    // CORRECTED DURING EXECUTION - see the note under this code block. The
    // comment below claims a rule this list does not implement.
    //
    // Everything the plugin brings is relocated, without exception. The rule
    // is "relocate all of it" rather than "relocate what currently conflicts",
    // because the second list has to be revisited every time Paper changes a
    // bundled library and nobody will remember to.
    listOf(
        "com.google.protobuf",
        "com.google.common",
        "com.google.gson",
        "io.grpc",
        "io.perfmark",
        "okio",
        "com.squareup.okhttp3",
        "javax.annotation",
        "kotlin",
    ).forEach { relocate(it, "cloud.spawnery.agent.shaded.$it") }

    mergeServiceFiles()
}

tasks.build { dependsOn(tasks.shadowJar) }
```

> **Correction, made during execution.** That nine-entry list is exactly the
> "relocate what currently conflicts" list its own comment says not to keep, and
> it had already fallen behind by the end of the milestone: measured from the
> built jar, 1187 class files shipped unrelocated, three of those packages
> present in Paper's own `libraries` tree — `com.google.thirdparty` (guava's
> second top-level package, one line below `com.google.common` here),
> `com.google.errorprone` and `com.google.j2objc`. The shipped
> `agent/paper/build.gradle.kts` resolves this; read it rather than the block
> above.

In `nix/paper-agent.nix`, change `gradleBuildTask = "jar";` to
`gradleBuildTask = "shadowJar";` and add the check to the derivation:

```nix
  nativeBuildInputs = [ gradle unzip ];

  installCheckPhase = ''
    runHook preInstallCheck
    ${../hack/agent-jar-check.sh} $out/share/spawnery/spawnery-agent.jar
    runHook postInstallCheck
  '';
  doInstallCheck = true;
```

and add `unzip` to the function arguments.

- [ ] **Step 4: Regenerate the lockfile**

The Shadow plugin is a new dependency.

Run: `make agent-deps && git diff --stat agent/paper/deps.json`

Expected: `deps.json` grows entries for `com.gradleup.shadow`.

- [ ] **Step 5: Neutralise the descriptor assertion until Task 7**

The `paper-plugin.yml` check cannot pass yet. Rather than deleting and
re-adding it, create the descriptor now — it is three lines and Task 7 only
extends it.

`agent/paper/src/main/resources/paper-plugin.yml`:

```yaml
name: SpawneryAgent
version: '${version}'
main: cloud.spawnery.agent.AgentPlugin
api-version: '26.2'
description: Reports readiness and player counts to the Spawnery operator.
```

and in `build.gradle.kts`:

```kotlin
tasks.processResources {
    filesMatching("paper-plugin.yml") {
        expand("version" to project.version)
    }
}
```

`AgentPlugin` does not exist yet; that is fine, the descriptor is data.

- [ ] **Step 6: Build and verify the check passes**

Run: `nix build .#paper-agent -L 2>&1 | tail -20`

Expected: `agent-jar-check: ok` in the output, build succeeds.

- [ ] **Step 7: Verify reproducibility survives shading**

Run: `nix build .#paper-agent --rebuild`

Expected: exit 0. If it reports a difference, the cause is an archive task the
`AbstractArchiveTask` block did not reach, or `expand` writing a timestamp —
find it with `diffoscope` on the two store paths rather than guessing.

- [ ] **Step 8: Commit**

```bash
git add agent/paper hack/agent-jar-check.sh nix/paper-agent.nix
git commit
```

---

### Task 4: Reading the token and trusting only the mounted CA

**Files:**
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/TokenSource.kt`
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/OperatorChannel.kt`
- Create: `agent/paper/src/test/kotlin/cloud/spawnery/agent/TokenSourceTest.kt`
- Create: `agent/paper/src/test/kotlin/cloud/spawnery/agent/OperatorChannelTest.kt`
- Create: `agent/paper/src/test/kotlin/cloud/spawnery/agent/TestCerts.kt`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `class TokenSource(path: Path) { fun read(): String }`
  - `object OperatorChannel { fun trustManager(caBundlePem: ByteArray): javax.net.ssl.X509TrustManager; fun build(endpoint: String, caBundlePem: ByteArray): io.grpc.ManagedChannel }`
  - `object BearerCredentials { fun of(tokens: TokenSource): io.grpc.CallCredentials }`

- [ ] **Step 1: Write the test fixture helper**

`agent/paper/src/test/kotlin/cloud/spawnery/agent/TestCerts.kt`:

```kotlin
package cloud.spawnery.agent

import org.bouncycastle.asn1.x500.X500Name
import org.bouncycastle.cert.jcajce.JcaX509CertificateConverter
import org.bouncycastle.cert.jcajce.JcaX509v3CertificateBuilder
import org.bouncycastle.operator.jcajce.JcaContentSignerBuilder
import java.io.StringWriter
import java.math.BigInteger
import java.security.KeyPair
import java.security.KeyPairGenerator
import java.security.cert.X509Certificate
import java.util.Base64
import java.util.Date

/** A self-signed CA, generated per test so no fixture can expire. */
data class TestCa(val certificate: X509Certificate, val keyPair: KeyPair) {
    fun pem(): ByteArray = pemOf(certificate)
}

fun newTestCa(commonName: String): TestCa {
    val keyPair = KeyPairGenerator.getInstance("RSA").apply { initialize(2048) }.generateKeyPair()
    val now = System.currentTimeMillis()
    val name = X500Name("CN=$commonName")
    val builder = JcaX509v3CertificateBuilder(
        name,
        BigInteger.valueOf(now),
        Date(now - 60_000),
        Date(now + 3_600_000),
        name,
        keyPair.public,
    )
    val signer = JcaContentSignerBuilder("SHA256withRSA").build(keyPair.private)
    val certificate = JcaX509CertificateConverter().getCertificate(builder.build(signer))
    return TestCa(certificate, keyPair)
}

fun pemOf(certificate: X509Certificate): ByteArray {
    val encoder = Base64.getMimeEncoder(64, "\n".toByteArray())
    val body = encoder.encodeToString(certificate.encoded)
    val out = StringWriter()
    out.write("-----BEGIN CERTIFICATE-----\n")
    out.write(body)
    out.write("\n-----END CERTIFICATE-----\n")
    return out.toString().toByteArray()
}
```

- [ ] **Step 2: Write the failing tests**

`agent/paper/src/test/kotlin/cloud/spawnery/agent/TokenSourceTest.kt`:

```kotlin
package cloud.spawnery.agent

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path

class TokenSourceTest {
    @Test
    fun `reads the token from disk every time`(@TempDir dir: Path) {
        val path = dir.resolve("token")
        Files.writeString(path, "first")
        val tokens = TokenSource(path)

        assertEquals("first", tokens.read())

        // The kubelet replaces this file roughly every eight minutes. A token
        // cached at startup carries the first session and no later one, and
        // the failure would read as an authentication problem rather than a
        // caching bug.
        Files.writeString(path, "second")
        assertEquals("second", tokens.read())
    }

    @Test
    fun `strips the trailing newline a file may carry`(@TempDir dir: Path) {
        val path = dir.resolve("token")
        Files.writeString(path, "value\n")
        assertEquals("value", TokenSource(path).read())
    }
}
```

`agent/paper/src/test/kotlin/cloud/spawnery/agent/OperatorChannelTest.kt`:

```kotlin
package cloud.spawnery.agent

import io.grpc.CallOptions
import io.grpc.Metadata
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path
import java.util.concurrent.Executor

class OperatorChannelTest {
    @Test
    fun `accepts every certificate in a multi-PEM bundle`() {
        val first = newTestCa("first")
        val second = newTestCa("second")
        val bundle = first.pem() + second.pem()

        val trust = OperatorChannel.trustManager(bundle)

        val accepted = trust.acceptedIssuers.map { it.subjectX500Principal.name }.toSet()
        assertEquals(setOf("CN=first", "CN=second"), accepted)
    }

    @Test
    fun `rejects a bundle with no certificate in it`() {
        assertThrows(IllegalArgumentException::class.java) {
            OperatorChannel.trustManager("not a certificate".toByteArray())
        }
    }

    @Test
    fun `bearer credentials carry the current token, one space after Bearer`(@TempDir dir: Path) {
        val path = dir.resolve("token")
        Files.writeString(path, "abc")
        val credentials = BearerCredentials.of(TokenSource(path))

        assertEquals("Bearer abc", applyAndRead(credentials))

        Files.writeString(path, "def")
        assertEquals("Bearer def", applyAndRead(credentials))
    }

    private fun applyAndRead(credentials: io.grpc.CallCredentials): String {
        var seen: String? = null
        credentials.applyRequestMetadata(
            object : io.grpc.CallCredentials.RequestInfo() {
                override fun getMethodDescriptor() = throw UnsupportedOperationException()
                override fun getSecurityLevel() = io.grpc.SecurityLevel.PRIVACY_AND_INTEGRITY
                override fun getAuthority() = "operator"
                override fun getTransportAttrs() = io.grpc.Attributes.EMPTY
            },
            Executor { it.run() },
            object : io.grpc.CallCredentials.MetadataApplier() {
                override fun apply(headers: Metadata) {
                    seen = headers.get(
                        Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER),
                    )
                }

                override fun fail(status: io.grpc.Status) = throw AssertionError(status.toString())
            },
        )
        return requireNonNull(seen)
    }

    private fun requireNonNull(value: String?): String = value ?: throw AssertionError("no header applied")
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `nix build .#paper-agent -L 2>&1 | tail -20`

Expected: FAIL — `Unresolved reference: TokenSource`.

- [ ] **Step 4: Write the implementation**

`agent/paper/src/main/kotlin/cloud/spawnery/agent/TokenSource.kt`:

```kotlin
package cloud.spawnery.agent

import java.nio.file.Files
import java.nio.file.Path

/**
 * Reads the projected ServiceAccount token, every time it is asked.
 *
 * The token lives 600 seconds and the kubelet replaces the file in place. It is
 * deliberately never cached: a value read once at startup carries the first
 * session and none after it, and the resulting failure presents as an
 * authentication problem rather than as a caching bug.
 */
class TokenSource(private val path: Path) {
    fun read(): String = Files.readString(path).trim()
}
```

`agent/paper/src/main/kotlin/cloud/spawnery/agent/OperatorChannel.kt`:

```kotlin
package cloud.spawnery.agent

import io.grpc.CallCredentials
import io.grpc.CallOptions
import io.grpc.ManagedChannel
import io.grpc.Metadata
import io.grpc.Status
import io.grpc.okhttp.OkHttpChannelBuilder
import java.io.ByteArrayInputStream
import java.security.KeyStore
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import java.util.concurrent.Executor
import javax.net.ssl.SSLContext
import javax.net.ssl.TrustManagerFactory
import javax.net.ssl.X509TrustManager

private val AUTHORIZATION: Metadata.Key<String> =
    Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER)

/**
 * The channel to the operator. Trust comes from the mounted CA bundle and from
 * nowhere else — no system trust store — because that pinning is what makes the
 * pod's identity claim meaningful in both directions.
 */
object OperatorChannel {
    /**
     * The bundle may hold several concatenated PEMs. The agent channel design
     * keeps that format open so a later CA rotation can run old and new with
     * overlap; parsing only the first certificate would make the agent the one
     * thing that cannot survive such a rotation.
     */
    fun trustManager(caBundlePem: ByteArray): X509TrustManager {
        val factory = CertificateFactory.getInstance("X.509")
        val certificates = factory.generateCertificates(ByteArrayInputStream(caBundlePem))
        require(certificates.isNotEmpty()) { "the CA bundle contains no certificate" }

        val keyStore = KeyStore.getInstance(KeyStore.getDefaultType())
        keyStore.load(null, null)
        certificates.forEachIndexed { index, certificate ->
            keyStore.setCertificateEntry("ca-$index", certificate as X509Certificate)
        }

        val trustFactory = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm())
        trustFactory.init(keyStore)
        return trustFactory.trustManagers.filterIsInstance<X509TrustManager>().first()
    }

    fun build(endpoint: String, caBundlePem: ByteArray): ManagedChannel {
        val trust = trustManager(caBundlePem)
        val context = SSLContext.getInstance("TLS")
        context.init(null, arrayOf(trust), null)
        return OkHttpChannelBuilder.forTarget(endpoint)
            .useTransportSecurity()
            .sslSocketFactory(context.socketFactory)
            .build()
    }
}

/**
 * The bearer header, assembled by hand and therefore exactly:
 * `Authorization: Bearer <token>`, one space.
 *
 * internal/grpcauth/interceptor.go matches that prefix character for character
 * and fails closed on anything else — reporting "no token" rather than "wrong
 * spelling", which is why a mistake here would be expensive to find.
 *
 * Applied per call rather than per channel, which also makes a stale token
 * structurally impossible: every stream reads the file again.
 */
object BearerCredentials {
    fun of(tokens: TokenSource): CallCredentials = object : CallCredentials() {
        override fun applyRequestMetadata(
            requestInfo: RequestInfo,
            appExecutor: Executor,
            applier: MetadataApplier,
        ) {
            appExecutor.execute {
                try {
                    val headers = Metadata()
                    headers.put(AUTHORIZATION, "Bearer " + tokens.read())
                    applier.apply(headers)
                } catch (e: Exception) {
                    applier.fail(Status.UNAUTHENTICATED.withCause(e))
                }
            }
        }
    }
}
```

> **Correction, made during execution.** `OperatorChannel.build` above is
> missing `.tlsConnectionSpec(TLS_VERSIONS, CIPHER_SUITES)`, and without it the
> agent cannot complete a handshake at all. grpc-okhttp chooses its
> `ConnectionSpec` by platform: the TLS 1.3 + 1.2 spec only on Android, and a
> TLS-1.2-only legacy spec on every JDK. `internal/agentserver` sets
> `MinVersion: VersionTLS13`, so the default leaves the agent offering a version
> the operator refuses and the handshake dies with a `protocol_version` alert
> before a byte of HTTP/2. Nothing at level 1 could see it — the in-process
> transport used by the unit tests does no TLS — so it was found by
> `hack/agent-test.sh` against a real operator-shaped server. The shipped form
> is `OperatorChannel.kt:71-86`, which spells out TLS 1.3 and its three suites;
> `docs/handover-milestone-3.md` names it as the first thing to check when a new
> agent's handshake fails.

- [ ] **Step 5: Run to verify it passes**

Run: `nix build .#paper-agent -L 2>&1 | tail -20`

Expected: all tests pass.

- [ ] **Step 6: Commit**

```bash
git add agent/paper
git commit
```

---

### Task 5: The session connects, greets and reports

**Files:**
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/SessionLoop.kt`
- Create: `agent/paper/src/test/kotlin/cloud/spawnery/agent/FakeOperator.kt`
- Create: `agent/paper/src/test/kotlin/cloud/spawnery/agent/SessionLoopTest.kt`

**Interfaces:**
- Consumes: `ServerState` (Task 1), the stubs (Task 2), `TokenSource`/`BearerCredentials` (Task 4).
- Produces:
  ```kotlin
  class SessionLoop(
      private val channels: () -> ManagedChannel,
      private val credentials: CallCredentials,
      private val state: ServerState,
      private val scheduler: ScheduledExecutorService,
      private val version: String,
      private val log: (String, Throwable?) -> Unit,
  ) {
      fun start()
      fun stop()
      fun readyChanged()
  }
  ```
  `readyChanged()` is what `AgentPlugin` calls from the `ServerLoadEvent`
  listener in Task 7; Task 6 adds renewal and backoff to the same class.

- [ ] **Step 1: Write the test double for the operator**

`agent/paper/src/test/kotlin/cloud/spawnery/agent/FakeOperator.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.AgentServiceGrpc
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.ServerMessage
import io.grpc.Metadata
import io.grpc.Server
import io.grpc.ServerCall
import io.grpc.ServerCallHandler
import io.grpc.ServerInterceptor
import io.grpc.inprocess.InProcessChannelBuilder
import io.grpc.inprocess.InProcessServerBuilder
import io.grpc.stub.StreamObserver
import java.util.concurrent.ConcurrentLinkedQueue
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/** One accepted stream, with everything the test wants to assert about it. */
class AcceptedStream(val authorization: String?) {
    val received = ConcurrentLinkedQueue<ServerMessage>()
    val closed = CountDownLatch(1)
    lateinit var toAgent: StreamObserver<OperatorToServer>

    fun awaitMessage(predicate: (ServerMessage) -> Boolean): ServerMessage {
        val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5)
        while (System.nanoTime() < deadline) {
            received.firstOrNull(predicate)?.let { return it }
            Thread.sleep(10)
        }
        throw AssertionError("no matching message within 5s; saw: ${received.toList()}")
    }
}

/**
 * An in-process operator. It is not a mock of the real one — it only has to
 * accept a stream and record it, because what is under test is the agent's half
 * of the conversation.
 */
class FakeOperator(name: String) : AutoCloseable {
    val streams = ConcurrentLinkedQueue<AcceptedStream>()
    private val header = Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER)

    private val service = object : AgentServiceGrpc.AgentServiceImplBase() {
        override fun serverSession(
            responseObserver: StreamObserver<OperatorToServer>,
        ): StreamObserver<ServerMessage> {
            val accepted = AcceptedStream(currentAuthorization.get())
            accepted.toAgent = responseObserver
            streams.add(accepted)
            return object : StreamObserver<ServerMessage> {
                override fun onNext(value: ServerMessage) { accepted.received.add(value) }
                override fun onError(t: Throwable) { accepted.closed.countDown() }
                override fun onCompleted() { accepted.closed.countDown() }
            }
        }
    }

    private val currentAuthorization = ThreadLocal<String?>()

    private val recorder = object : ServerInterceptor {
        override fun <Q : Any, S : Any> interceptCall(
            call: ServerCall<Q, S>,
            headers: Metadata,
            next: ServerCallHandler<Q, S>,
        ): ServerCall.Listener<Q> {
            currentAuthorization.set(headers.get(header))
            return next.startCall(call, headers)
        }
    }

    private val server: Server = InProcessServerBuilder.forName(name)
        .directExecutor()
        .addService(io.grpc.ServerInterceptors.intercept(service, recorder))
        .build()
        .start()

    val channelName = name

    fun awaitStream(index: Int): AcceptedStream {
        val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5)
        while (System.nanoTime() < deadline) {
            streams.toList().getOrNull(index)?.let { return it }
            Thread.sleep(10)
        }
        throw AssertionError("stream $index never arrived; have ${streams.size}")
    }

    fun newChannel() = InProcessChannelBuilder.forName(channelName).directExecutor().build()

    override fun close() {
        server.shutdownNow()
        server.awaitTermination(5, TimeUnit.SECONDS)
    }
}
```

- [ ] **Step 2: Write the failing tests**

`agent/paper/src/test/kotlin/cloud/spawnery/agent/SessionLoopTest.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.ReportInterval
import cloud.spawnery.agent.pb.ServerMessage
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService

class SessionLoopTest {
    private val scheduler: ScheduledExecutorService = Executors.newSingleThreadScheduledExecutor()

    @AfterEach fun shutdown() { scheduler.shutdownNow() }

    private fun loopAgainst(
        operator: FakeOperator,
        state: ServerState,
        dir: Path,
    ): SessionLoop {
        val token = dir.resolve("token")
        Files.writeString(token, "test-token")
        return SessionLoop(
            channels = { operator.newChannel() },
            credentials = BearerCredentials.of(TokenSource(token)),
            state = state,
            scheduler = scheduler,
            version = "26.2-0.2.0",
            log = { _, _ -> },
        )
    }

    @Test
    fun `greets with the version and the current readiness`(@TempDir dir: Path) {
        FakeOperator("greets").use { operator ->
            val state = ServerState().apply { markReady() }
            loopAgainst(operator, state, dir).use { it.start()

                val stream = operator.awaitStream(0)
                val hello = stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                assertEquals("26.2-0.2.0", hello.hello.version)
                assertTrue(hello.hello.ready)
                assertEquals("Bearer test-token", stream.authorization)
            }
        }
    }

    @Test
    fun `sends Ready when readiness arrives after the greeting`(@TempDir dir: Path) {
        FakeOperator("ready-later").use { operator ->
            val state = ServerState()
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                val hello = stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                assertEquals(false, hello.hello.ready)

                state.markReady()
                loop.readyChanged()

                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.READY }
            }
        }
    }

    @Test
    fun `reports the player count at the interval the operator dictates`(@TempDir dir: Path) {
        FakeOperator("reports").use { operator ->
            val state = ServerState().apply { sample(players = 3, slots = 100) }
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                stream.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )

                val report = stream.awaitMessage {
                    it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT
                }
                assertEquals(3, report.playerCount.players)
                assertEquals(100, report.playerCount.slots)
            }
        }
    }

    @Test
    fun `does not report before the operator has dictated an interval`(@TempDir dir: Path) {
        FakeOperator("no-interval").use { operator ->
            val state = ServerState().apply { sample(players = 3, slots = 100) }
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                Thread.sleep(500)

                assertTrue(
                    stream.received.none { it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT },
                    "the interval is the operator's to set; both sides derive the staleness " +
                        "threshold from it, so guessing one locally would break that",
                )
            }
        }
    }
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `nix build .#paper-agent -L 2>&1 | tail -20`

Expected: FAIL — `Unresolved reference: SessionLoop`.

- [ ] **Step 4: Write the implementation**

`agent/paper/src/main/kotlin/cloud/spawnery/agent/SessionLoop.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.AgentServiceGrpc
import cloud.spawnery.agent.pb.Hello
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.PlayerCount
import cloud.spawnery.agent.pb.Ready
import cloud.spawnery.agent.pb.ServerMessage
import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.StreamObserver
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference

/**
 * One live stream to the operator, and everything scheduled on it.
 *
 * The scheduler is injected rather than created here so tests drive time
 * instead of sleeping through it.
 */
private class Session(
    val channel: ManagedChannel,
    val toOperator: StreamObserver<ServerMessage>,
) {
    var reporting: ScheduledFuture<*>? = null

    fun close() {
        reporting?.cancel(false)
        runCatching { toOperator.onCompleted() }
        channel.shutdown()
    }
}

class SessionLoop(
    private val channels: () -> ManagedChannel,
    private val credentials: CallCredentials,
    private val state: ServerState,
    private val scheduler: ScheduledExecutorService,
    private val version: String,
    private val log: (String, Throwable?) -> Unit,
) : AutoCloseable {
    private val current = AtomicReference<Session?>(null)

    fun start() {
        connect()
    }

    /**
     * Called when the server finished loading. Readiness is a state, not an
     * event: every Hello carries it, and this only adds the immediate
     * notification so the operator does not wait for the next connect.
     */
    fun readyChanged() {
        val session = current.get() ?: return
        if (!state.ready) return
        send(session, ServerMessage.newBuilder().setReady(Ready.getDefaultInstance()).build())
    }

    // CORRECTED DURING EXECUTION. A graceful close here parks the transport
    // when stop() lands on an attempt still waiting for its first message: the
    // call it half-closes is one the operator will never finish, and a
    // half-close is not a cancellation. The shipped SessionLoop.stop() forces
    // that one down. Read the file, not this.
    fun stop() {
        current.getAndSet(null)?.close()
    }

    override fun close() = stop()

    private fun connect() {
        val channel = channels()
        val stub = AgentServiceGrpc.newStub(channel).withCallCredentials(credentials)

        // Assigned before the stub call, because the operator may answer
        // before newStub() returns and the handler needs somewhere to put it.
        val holder = AtomicReference<Session?>(null)

        val fromOperator = object : StreamObserver<OperatorToServer> {
            override fun onNext(value: OperatorToServer) {
                val session = holder.get() ?: return
                when (value.messageCase) {
                    OperatorToServer.MessageCase.REPORT_INTERVAL ->
                        startReporting(session, value.reportInterval.seconds)
                    else -> Unit
                }
            }

            override fun onError(t: Throwable) {
                log("the operator stream failed", t)
            }

            override fun onCompleted() {
                log("the operator closed the stream", null)
            }
        }

        val toOperator = stub.serverSession(fromOperator)
        val session = Session(channel, toOperator)
        holder.set(session)
        current.set(session)

        send(
            session,
            ServerMessage.newBuilder()
                .setHello(Hello.newBuilder().setVersion(version).setReady(state.ready))
                .build(),
        )
    }

    private fun startReporting(session: Session, seconds: Int) {
        if (seconds <= 0) return
        session.reporting?.cancel(false)
        session.reporting = scheduler.scheduleAtFixedRate(
            {
                send(
                    session,
                    ServerMessage.newBuilder()
                        .setPlayerCount(
                            PlayerCount.newBuilder()
                                .setPlayers(state.players)
                                .setSlots(state.slots),
                        )
                        .build(),
                )
            },
            0,
            seconds.toLong(),
            TimeUnit.SECONDS,
        )
    }

    private fun send(session: Session, message: ServerMessage) {
        try {
            synchronized(session) { session.toOperator.onNext(message) }
        } catch (e: Exception) {
            log("could not send to the operator", e)
        }
    }
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `nix build .#paper-agent -L 2>&1 | tail -30`

Expected: all tests pass.

- [ ] **Step 6: Commit**

```bash
git add agent/paper
git commit
```

---

### Task 6: Renewal with overlap, and backoff that never gives up

The obligation `docs/known-issues.md` calls non-optional. Without the overlap
every server drops out of `Ready` on the rhythm of the hard deadline,
deregisters from its proxies and collects a readiness loss — a self-inflicted
flap counter.

**Files:**
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/SessionLoop.kt`
- Modify: `agent/paper/src/test/kotlin/cloud/spawnery/agent/SessionLoopTest.kt`

**Interfaces:**
- Consumes: everything from Task 5.
- Produces: `SessionLoop`'s constructor gains `private val jitter: (Long) -> Long = { it }` as its last parameter, so tests can pin the delay. Behaviour is otherwise unchanged for existing callers.

- [ ] **Step 1: Write the failing tests**

Append to `SessionLoopTest.kt`:

```kotlin
    @Test
    fun `renews before the deadline and keeps the old stream open until the new one greets`(
        @TempDir dir: Path,
    ) {
        FakeOperator("renew").use { operator ->
            val state = ServerState().apply { markReady() }
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setSessionDeadline(
                            SessionDeadline.newBuilder()
                                .setRenewAfterSeconds(1)
                                .setHardDeadlineSeconds(3),
                        )
                        .build(),
                )

                val second = operator.awaitStream(1)
                val hello = second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // Readiness is repeated on every connect, so the operator's
                // Supersede carries it across the handover.
                assertTrue(hello.hello.ready)

                // Make before break: the first stream must still have been
                // open when the second greeted. If the agent closed first,
                // the operator sees the pod disconnect and the server drops
                // out of Ready for the length of the gap.
                assertTrue(
                    second.received.isNotEmpty(),
                    "the second stream greeted",
                )
                assertTrue(
                    first.closed.await(5, java.util.concurrent.TimeUnit.SECONDS),
                    "the first stream is closed afterwards",
                )
            }
        }
    }

    @Test
    fun `reconnects with backoff after the stream breaks`(@TempDir dir: Path) {
        FakeOperator("reconnect").use { operator ->
            val state = ServerState()
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onError(io.grpc.Status.UNAVAILABLE.asRuntimeException())

                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
            }
        }
    }

    @Test
    fun `backoff grows and is capped`() {
        val delays = (0..10).map { SessionLoop.backoffMillis(it) }
        assertEquals(1_000L, delays[0])
        assertEquals(2_000L, delays[1])
        assertEquals(4_000L, delays[2])
        assertTrue(delays.all { it <= 30_000L }, "capped at 30s: $delays")
        assertEquals(30_000L, delays.last())
    }
```

Add the import `import cloud.spawnery.agent.pb.SessionDeadline` at the top.

- [ ] **Step 2: Run to verify it fails**

Run: `nix build .#paper-agent -L 2>&1 | tail -20`

Expected: FAIL — `Unresolved reference: backoffMillis`, and the renewal test
times out waiting for stream 1.

- [ ] **Step 3: Implement renewal and backoff**

In `SessionLoop.kt`, add to the class:

```kotlin
    private var attempt = 0

    companion object {
        /** 1s doubling to a 30s cap. Never gives up: see the class comment. */
        fun backoffMillis(attempt: Int): Long =
            minOf(30_000L, 1_000L shl minOf(attempt, 20))
    }
```

Extend the `when` in `fromOperator.onNext`:

```kotlin
                    OperatorToServer.MessageCase.SESSION_DEADLINE ->
                        scheduleRenewal(session, value.sessionDeadline.renewAfterSeconds)
```

and add:

```kotlin
    /**
     * Make before break. The new stream is opened and greeted first; only then
     * is the old one closed. The reverse order makes the operator see a
     * disconnect on the rhythm of the hard deadline, which deregisters the
     * server from its proxies and books a readiness loss for a handover that
     * was supposed to be invisible.
     */
    private fun scheduleRenewal(session: Session, renewAfterSeconds: Int) {
        if (renewAfterSeconds <= 0) return
        val base = TimeUnit.SECONDS.toMillis(renewAfterSeconds.toLong())
        scheduler.schedule({
            if (current.get() !== session) return@schedule
            try {
                connect()
                session.close()
            } catch (e: Exception) {
                log("renewal failed; the old stream stays until it breaks", e)
            }
        }, jitter(base), TimeUnit.MILLISECONDS)
    }

    private fun reconnectLater() {
        val delay = backoffMillis(attempt)
        attempt++
        scheduler.schedule({ runCatching { connect() } }, delay, TimeUnit.MILLISECONDS)
    }
```

> **Correction, made during execution.** Three things in the two functions
> above are wrong, and each of them shipped as a defect somewhere before it was
> found.
>
> `connect(); session.close()` inside `scheduleRenewal` is **break before
> make**, which is the one thing this whole class exists to prevent. It reads
> like make before break and is not: the replacement needs a fresh TCP
> connection and a TLS handshake while the retirement travels an established
> one, so the close reliably overtakes the greeting and the operator sees a
> disconnect followed by a connect — every server dropping out of `Ready` on
> the rhythm of the hard deadline. `hack/agent-test.sh` measured it losing
> every time; the unit tests could not, because the in-process transport
> delivers the Hello synchronously. The shipped `scheduleRenewal`
> (`SessionLoop.kt:661-677`) has no `close()` at all: the outgoing stream is
> retired by `Session.takeOver()`, from `onNext`, once the operator has actually
> answered the replacement. `docs/known-issues.md:31-36` records it.
>
> `log("renewal failed; the old stream stays until it breaks", e)` leaves a
> failed renewal with nothing that retries it. The outgoing stream does still
> live until the hard deadline, so there is time — but only if something uses
> it. The shipped form logs and calls `reconnectLater()`.
>
> `runCatching { connect() }` swallows the one failure that has no other
> retry. `connect()` can throw before anything is registered anywhere — an
> unreadable token, a channel that will not build — and `runCatching` then ends
> the agent permanently and silently. The shipped `reconnectLater`
> (`SessionLoop.kt:679-693`) catches, logs, and reschedules itself.

Replace `fromOperator.onError` and `onCompleted` with:

```kotlin
            override fun onError(t: Throwable) {
                log("the operator stream failed", t)
                if (current.compareAndSet(holder.get(), null)) reconnectLater()
            }

            override fun onCompleted() {
                log("the operator closed the stream", null)
                if (current.compareAndSet(holder.get(), null)) reconnectLater()
            }
```

> **Correction, made during execution.** `if (current.compareAndSet(holder.get(),
> null)) reconnectLater()` is two separate defects at once, and the shipped code
> is `streamEnded` (`SessionLoop.kt:565-576`), which is neither.
>
> Guarding on `current` skips the reconnect exactly when it is most needed.
> `connect()` installs the session only after the Hello has gone out, so a
> stream that fails before that — a rejected token, an operator mid-rollout —
> leaves `current` pointing elsewhere, the compare-and-set fails, and the agent
> sits silent forever. That is strictly worse than reconnecting too eagerly, and
> `SessionLoop.kt:538-544` says so. A per-attempt `ended` flag says what
> `current` cannot: whether *this* stream was replaced on purpose.
>
> Booking the reconnect unconditionally is the other half.
> `internal/agentserver` cancels the displaced stream's context inside
> `sessions.enter()`, at the handler entry of the replacement — before
> `Supersede` and before either `Send` — so the agent always sees the outgoing
> stream fail *first*, on every renewal, while the replacement is still
> unanswered. A reconnect booked there is booked on every renewal, and since the
> replacement's first message resets the backoff to its 1 s floor, that
> reconnect supersedes the replacement a second later and the sequence repeats:
> a self-sustaining stream churn at roughly 1 Hz, per server, for as long as the
> fleet runs (`SessionLoop.kt:546-558`). The shipped code skips the reconnect
> when `hasReplacement()` says a replacement is already under way, and hands the
> obligation to it.
>
> Neither could be seen at level 1 alone, which is why `hack/agent-test.sh`
> phase 2 bounds the stream rate from above and phase 3 bounds it from below.

Reset `attempt = 0` at the end of `connect()`, after the `Hello` is sent.

> **Correction, made during execution.** Resetting at the end of `connect()`
> resets it on a `Hello` merely handed to the transport, which proves nothing:
> a stream that always dies just after it opens then has its backoff cleared on
> every attempt, and the agent reconnects once a second for as long as the
> operator stays broken. The reset belongs on the operator's first message, and
> that is where it is — `SessionLoop.kt:455`, inside `onNext`, beside the two
> other things that wait for the same proof. It is the same argument as make
> before break: what the operator saw, not what the agent sent. Deviation (a) of
> task 6, rejected on the record. `SessionLoopTest.kt`'s `grows the backoff with
> every failed attempt and starts over once the operator answers` pins both
> directions.

Add the constructor parameter, last:

```kotlin
    private val jitter: (Long) -> Long = { base ->
        // ±10 %, so the pods of one group do not all renew in the same instant.
        base + (Math.random() * 0.2 * base - 0.1 * base).toLong()
    },
```

For tests, `loopAgainst` passes `jitter = { it }`. Update it accordingly.

- [ ] **Step 4: Run to verify it passes**

Run: `nix build .#paper-agent -L 2>&1 | tail -30`

Expected: all tests pass, including the two new session tests and the backoff
table.

- [ ] **Step 5: Commit**

```bash
git add agent/paper
git commit
```

---

### Task 7: The Bukkit half

**Files:**
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/AgentPlugin.kt`
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/Environment.kt`
- Create: `agent/paper/src/test/kotlin/cloud/spawnery/agent/EnvironmentTest.kt`
- Modify: `agent/paper/src/main/resources/paper-plugin.yml` (already created in Task 3 — verify only)

**Interfaces:**
- Consumes: everything from Tasks 1, 4, 5, 6.
- Produces: `cloud.spawnery.agent.AgentPlugin`, the class `paper-plugin.yml` names. `Environment.from(getenv, readFile)` returning `Environment.Configured(endpoint, caBundle, tokenPath)` or `Environment.Dormant(reason)`.

`AgentPlugin` itself is not unit tested: it exists to call Bukkit, and testing
it would mean mocking Bukkit, which proves nothing the level-2 test does not
prove better. Everything decidable without Bukkit lives in `Environment`, which
is tested.

- [ ] **Step 1: Write the failing test**

`agent/paper/src/test/kotlin/cloud/spawnery/agent/EnvironmentTest.kt`:

```kotlin
package cloud.spawnery.agent

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertInstanceOf
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path

class EnvironmentTest {
    @Test
    fun `is dormant without an endpoint`(@TempDir dir: Path) {
        val result = Environment.from(mapOf<String, String>()::get, dir)
        val dormant = assertInstanceOf(Environment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("SPAWNERY_OPERATOR_ENDPOINT"))
    }

    @Test
    fun `is dormant when the endpoint is set but empty`(@TempDir dir: Path) {
        val result = Environment.from(mapOf("SPAWNERY_OPERATOR_ENDPOINT" to "")::get, dir)
        assertInstanceOf(Environment.Dormant::class.java, result)
    }

    @Test
    fun `is dormant when the CA bundle is missing`(@TempDir dir: Path) {
        val env = mapOf("SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443")
        val result = Environment.from(env::get, dir)
        val dormant = assertInstanceOf(Environment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("ca.crt"), dormant.reason)
    }

    @Test
    fun `is configured when endpoint, CA and token are all present`(@TempDir dir: Path) {
        Files.writeString(dir.resolve("ca.crt"), "pem")
        Files.writeString(dir.resolve("token"), "t")
        val env = mapOf("SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443")

        val result = Environment.from(env::get, dir)

        val configured = assertInstanceOf(Environment.Configured::class.java, result)
        assertEquals("operator:9443", configured.endpoint)
        assertEquals("pem", String(configured.caBundle))
        assertEquals(dir.resolve("token"), configured.tokenPath)
    }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `nix build .#paper-agent -L 2>&1 | tail -20`

Expected: FAIL — `Unresolved reference: Environment`.

- [ ] **Step 3: Write `Environment`**

`agent/paper/src/main/kotlin/cloud/spawnery/agent/Environment.kt`:

```kotlin
package cloud.spawnery.agent

import java.nio.file.Files
import java.nio.file.Path

/**
 * Everything the agent needs from outside the JVM, and the decision whether it
 * has enough to run at all.
 *
 * Being dormant is a normal outcome, not a failure: the image is meant to be
 * runnable outside a cluster, and make image-test does exactly that. A missing
 * endpoint therefore produces one log line and silence, not a retry loop.
 *
 * CORRECTED DURING EXECUTION: carrying the bundle as a ByteArray read once in
 * Environment.from means the agent holds whatever was on disk at onEnable, so
 * it cannot survive the CA rotation the concatenated-PEM format exists to
 * allow. The shipped Environment hands on the path instead. Read that file.
 */
sealed interface Environment {
    data class Configured(
        val endpoint: String,
        val caBundle: ByteArray,
        val tokenPath: Path,
    ) : Environment

    data class Dormant(val reason: String) : Environment

    companion object {
        const val ENDPOINT = "SPAWNERY_OPERATOR_ENDPOINT"

        fun from(getenv: (String) -> String?, agentDir: Path): Environment {
            val endpoint = getenv(ENDPOINT)
            if (endpoint.isNullOrBlank()) {
                return Dormant("$ENDPOINT is not set; not connecting to an operator")
            }

            val ca = agentDir.resolve("ca.crt")
            if (!Files.isReadable(ca)) {
                return Dormant("$ca is not readable; refusing to trust anything else")
            }

            val token = agentDir.resolve("token")
            if (!Files.isReadable(token)) {
                return Dormant("$token is not readable")
            }

            return Configured(endpoint, Files.readAllBytes(ca), token)
        }
    }
}
```

- [ ] **Step 4: Write `AgentPlugin`**

`agent/paper/src/main/kotlin/cloud/spawnery/agent/AgentPlugin.kt`:

```kotlin
package cloud.spawnery.agent

import org.bukkit.Bukkit
import org.bukkit.event.EventHandler
import org.bukkit.event.Listener
import org.bukkit.event.server.ServerLoadEvent
import org.bukkit.plugin.java.JavaPlugin
import java.nio.file.Path
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit

/**
 * The only class in this plugin that touches Bukkit.
 *
 * That is not tidiness for its own sake: it is what lets every other unit be
 * tested with JUnit and no server. Nothing here decides anything — the
 * decisions live in Environment, ServerState and SessionLoop.
 */
class AgentPlugin : JavaPlugin(), Listener {
    private val state = ServerState()
    private lateinit var scheduler: ScheduledExecutorService
    private var loop: SessionLoop? = null

    override fun onEnable() {
        when (val env = Environment.from(System::getenv, Path.of(AGENT_DIR))) {
            is Environment.Dormant -> {
                logger.info("spawnery agent dormant: ${env.reason}")
                return
            }

            is Environment.Configured -> {
                scheduler = Executors.newSingleThreadScheduledExecutor { runnable ->
                    Thread(runnable, "spawnery-agent").apply { isDaemon = true }
                }

                val session = SessionLoop(
                    channels = { OperatorChannel.build(env.endpoint, env.caBundle) },
                    credentials = BearerCredentials.of(TokenSource(env.tokenPath)),
                    state = state,
                    scheduler = scheduler,
                    version = pluginMeta.version,
                    log = { message, error -> logger.log(java.util.logging.Level.INFO, message, error) },
                )
                loop = session

                server.pluginManager.registerEvents(this, this)

                // Bukkit.getOnlinePlayers() is not thread-safe. The count is
                // sampled here, on the main thread, and the network side only
                // ever reads what this wrote.
                server.scheduler.runTaskTimer(this, Runnable {
                    state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())
                }, 0L, SAMPLE_TICKS)

                session.start()
                logger.info("spawnery agent connecting to ${env.endpoint}")
            }
        }
    }

    override fun onDisable() {
        loop?.stop()
        if (::scheduler.isInitialized) {
            scheduler.shutdownNow()
            scheduler.awaitTermination(2, TimeUnit.SECONDS)
        }
    }

    @EventHandler
    fun onServerLoad(event: ServerLoadEvent) {
        if (event.type != ServerLoadEvent.LoadType.STARTUP) return
        state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())
        if (state.markReady()) {
            loop?.readyChanged()
        }
    }

    private companion object {
        // internal/podspec.AgentMountPath. Hard-coded rather than configurable:
        // the operator creates these pods and mounts exactly here, and a second
        // place to spell it would be a second place to get it wrong.
        const val AGENT_DIR = "/var/run/spawnery"

        // One second at 20 ticks. Fast enough that the reported count is never
        // stale by more than the report interval's own resolution, cheap enough
        // that it is invisible next to a tick.
        const val SAMPLE_TICKS = 20L
    }
}
```

- [ ] **Step 5: Verify the descriptor matches**

Run: `grep -n 'main:\|api-version:' agent/paper/src/main/resources/paper-plugin.yml`

Expected: `main: cloud.spawnery.agent.AgentPlugin` and `api-version: '26.2'`.
The API version was read out of `apiVersioning.json` inside the pinned
`paper-api` jar (`{"version":"26.2.build.111-stable","currentApiVersion":"26.2"}`)
— do not change it to a guess.

- [ ] **Step 6: Run the build**

Run: `nix build .#paper-agent -L 2>&1 | tail -30`

Expected: all tests pass, `agent-jar-check: ok`.

- [ ] **Step 7: Commit**

```bash
git add agent/paper
git commit
```

---

### Task 8: The plugin reaches the image

**Files:**
- Modify: `image/entrypoint.sh`
- Modify: `image/entrypoint_test.go`
- Modify: `nix/paper-image.nix`
- Modify: `flake.nix`

**Interfaces:**
- Consumes: `packages.paper-agent` (Task 1), whose jar is at `$out/share/spawnery/spawnery-agent.jar`.
- Produces: the image carries `/opt/paper/agent/spawnery-agent.jar`; the entrypoint copies it to `/data/plugins/spawnery-agent.jar` on every start; the image tag becomes `26.2-0.2.0`.

- [ ] **Step 1: Write the failing test**

Read `image/entrypoint_test.go` first to match its existing helpers exactly —
it drives the real script against a stub `java` on `PATH` using
`testenv.RepoPath`. Add:

```go
func TestCopiesTheAgentPluginIntoAWritablePluginsDirectory(t *testing.T) {
	dir := newEntrypointDir(t)

	// The image ships the jar in the read-only part; the entrypoint's job is
	// to get it somewhere Paper may also write, because Paper puts its
	// plugins' data folders inside the plugins directory.
	paperHome := filepath.Join(dir, "opt", "paper")
	if err := os.MkdirAll(filepath.Join(paperHome, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(paperHome, "agent", "spawnery-agent.jar")
	if err := os.WriteFile(jar, []byte("fresh"), 0o444); err != nil {
		t.Fatal(err)
	}

	// A stale copy from a previous start must lose: the image is the truth.
	if err := os.MkdirAll(filepath.Join(dir, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "plugins", "spawnery-agent.jar")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	runEntrypoint(t, dir, map[string]string{
		"SPAWNERY_MAX_PLAYERS": "100",
		"PAPER_HOME":           paperHome,
	})

	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("the agent jar is not in the plugins directory: %v", err)
	}
	if string(got) != "fresh" {
		t.Errorf("plugins/spawnery-agent.jar = %q, want the copy from the image", got)
	}
}
```

Adapt `newEntrypointDir` and `runEntrypoint` to whatever the existing file
calls them; do not introduce a second set of helpers.

- [ ] **Step 2: Run to verify it fails**

Run: `nix develop -c go test ./image/ -run TestCopiesTheAgentPlugin -v`

Expected: FAIL — the jar is not in the plugins directory.

- [ ] **Step 3: Extend the entrypoint**

In `image/entrypoint.sh`, after the `set_property` calls and before the `exec
java` line:

```sh
# The agent plugin. It ships in the read-only part of the image and is copied
# out on every start, unconditionally: the image is the truth, not whatever a
# previous start left in the volume.
#
# It cannot simply be loaded from where it ships. Paper writes its plugins'
# data folders inside the plugins directory - measured in milestone 2b, which
# saw plugins/spark/config.json and plugins/bStats/config.yml appear on a plain
# run - so pointing --plugins at a read-only directory takes Paper's own
# bundled plugins down with it.
#
# A read-only mount at /data/plugins therefore breaks the start here, with a
# bare cp error. Mounts below /data are allowed by internal/podspec, so this is
# reachable; see docs/known-issues.md.
if [ -f "$PAPER_HOME/agent/spawnery-agent.jar" ]; then
	mkdir -p plugins
	cp -f "$PAPER_HOME/agent/spawnery-agent.jar" plugins/spawnery-agent.jar
fi
```

> **Correction, made during execution.** This step originally added
> `chmod u+w plugins/spawnery-agent.jar` after the copy, justified by the claim
> that "`cp -f` unlinks rather than writing through — the chmod makes the next
> start's overwrite work regardless." The reviewer tested `cp -f` against a
> genuinely read-only 0444 destination: it already unlinks and recreates, which
> needs only the *directory* to be writable, and `/data` always is under the
> podspec contract. The `chmod` was therefore dead code carrying a false
> explanation — the exact defect class milestone 2b's final review named, a
> comment asserting a mechanism the code does not rely on. Removed, along with
> the claim.

- [ ] **Step 4: Run to verify it passes**

Run: `nix develop -c go test ./image/ -v`

Expected: PASS, including every pre-existing entrypoint test.

- [ ] **Step 5: Put the jar into the image**

In `nix/paper-image.nix`:

- add `paper-agent` to the function arguments,
- change `imageVersion ? "0.1.0"` to `imageVersion ? "0.2.0"`,
- add a new derivation next to `paperHome`:

```nix
  # Its own layer: it changes on every commit, while the JRE and the patched
  # Paper repo above it do not.
  agent = runCommand "paper-agent-image-path" { } ''
    mkdir -p $out/opt/paper/agent
    cp ${paper-agent}/share/spawnery/spawnery-agent.jar $out/opt/paper/agent/spawnery-agent.jar
  '';
```

- add `agent` to `contents`, after `paperHome`.

In `flake.nix`, pass it through:

```nix
          paper-image = pkgs.callPackage ./nix/paper-image.nix {
            inherit paper spawnery-slp paper-agent;
          };
```

- [ ] **Step 6: Verify the image**

Run: `nix eval --raw .#paper-image.imageTag`

Expected: `26.2-0.2.0`

Run: `make image-test`

Expected: `image-test: ok`. This is the level-3 proof from the design: the
plugin is present but has no `SPAWNERY_OPERATOR_ENDPOINT`, so the server must
still start and answer a ping.

- [ ] **Step 7: Assert the plugin loaded and left no stack trace**

Extend `hack/image-test.sh`, after the existing `check_no_download` call on
`container_logs`:

```bash
# The plugin is in the image but has no operator to reach here. Two things have
# to be true, and neither is visible from a green ping alone.
#
# That it loaded at all: a paper-plugin.yml naming a class that is not there,
# or an api-version Paper rejects, produces a server that starts perfectly and
# silently has no agent.
if ! grep -q 'spawnery agent dormant' <<<"$container_logs"; then
	echo "the agent plugin did not load, or did not report why it stayed dormant:" >&2
	grep -iE 'spawnery|plugin' <<<"$container_logs" >&2 || true
	exit 1
fi
echo "the agent plugin loaded and stayed dormant without an operator"

# And that nothing broke at class load. Note what this does NOT prove: with no
# operator endpoint the plugin goes dormant before SessionLoop, OperatorChannel
# or BearerCredentials are ever constructed, and those are the classes that
# import io.grpc. Class loading is lazy, so the shaded gRPC tree is never
# touched in this run. A shading regression confined to the operator-connection
# path would pass here. That proof is make agent-test's, in the next task.
if grep -qE 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs"; then
	echo "a linkage error appeared while loading the plugin:" >&2
	grep -B2 -A10 -E 'NoSuchMethodError|NoClassDefFoundError|LinkageError' <<<"$container_logs" >&2
	exit 1
fi
echo "the plugin's own classes load without a linkage error"
```

> **Correction, made during execution.** The original text of this check
> claimed "a protobuf or Netty conflict with Paper's own copies surfaces
> exactly here, at class load" and printed "the relocation holds". It does not
> and it does not: the dormant path never reaches a class that imports
> `io.grpc`. The check is still worth having — it catches a plugin that fails
> to load at all — but it had to stop claiming the one thing it cannot see.
> Section 9 of the design already assigns that proof to level 2.

Run: `make image-test`

Expected: both new lines appear and the script ends with `image-test: ok`.

- [ ] **Step 8: Verify reproducibility of the whole image**

Run: `make image-repro`

Expected: exit 0.

- [ ] **Step 9: Commit**

```bash
git add image nix/paper-image.nix flake.nix hack/image-test.sh
git commit
```

---

### Task 9: The stub operator, and the level-2 proof

**Files:**
- Create: `cmd/spawnery-stubop/main.go`
- Create: `cmd/spawnery-stubop/main_test.go`
- Create: `hack/agent-test.sh`
- Modify: `Makefile`
- Modify: `flake.nix` (`packages.spawnery-stubop`)

**Interfaces:**
- Consumes: `internal/agentpb` (already exists), the image from Task 8.
- Produces: a binary that serves `AgentService` over TLS, writes its CA and a token to a directory, and prints one JSON object per observed event to stdout.

- [ ] **Step 1: Write the failing test**

`cmd/spawnery-stubop/main_test.go`:

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterialiseWritesACaBundleAndTokenTheAgentCanRead(t *testing.T) {
	dir := t.TempDir()

	material, err := materialise(dir, []string{"stubop"})
	if err != nil {
		t.Fatalf("materialise: %v", err)
	}

	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("ca.crt: %v", err)
	}
	if !strings.HasPrefix(string(ca), "-----BEGIN CERTIFICATE-----") {
		t.Errorf("ca.crt is not PEM: %q", ca[:min(40, len(ca))])
	}

	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if len(token) == 0 {
		t.Error("the token file is empty")
	}

	// The agent validates the serving certificate against the mounted bundle
	// and nothing else, so the SAN has to be the name the container dials.
	if got := material.Certificate.Leaf.DNSNames; len(got) != 1 || got[0] != "stubop" {
		t.Errorf("SANs = %v, want [stubop]", got)
	}
}

func TestEventsAreOneJSONObjectPerLine(t *testing.T) {
	var out strings.Builder
	recorder := newRecorder(&out)

	recorder.record("hello", map[string]any{"version": "26.2-0.2.0", "ready": true})

	var event struct {
		Kind    string `json:"kind"`
		Version string `json:"version"`
		Ready   bool   `json:"ready"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &event); err != nil {
		t.Fatalf("not a JSON line: %v (%q)", err, out.String())
	}
	if event.Kind != "hello" || event.Version != "26.2-0.2.0" || !event.Ready {
		t.Errorf("event = %+v", event)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `nix develop -c go test ./cmd/spawnery-stubop/`

Expected: FAIL — `undefined: materialise`.

- [ ] **Step 3: Write the stub operator**

`cmd/spawnery-stubop/main.go` — a single file. It must:

- generate a self-signed CA and a serving certificate whose DNS SANs come from
  a repeatable `--san` flag (default `stubop`),
- write the CA PEM to `<dir>/ca.crt` and a random token to `<dir>/token`, both
  world-readable, since the container runs as uid 10001 and the directory is
  bind-mounted,
- serve `agentpb.AgentServiceServer` over TLS on `--listen` (default `:9443`),
  accepting **any** bearer token — it is not testing authentication, it is
  recording what the agent sends,
- on each accepted `ServerSession`, immediately send
  `ReportInterval{seconds: --report-interval}` (default 1) and
  `SessionDeadline{renew_after_seconds: --renew-after, hard_deadline_seconds: --hard-deadline}`
  (defaults 5 and 20 — deliberately far below the operator's 480/600, so a
  renewal is observable inside a test's lifetime),
- record one JSON line per event to stdout with `kind` values `stream_opened`,
  `hello`, `ready`, `player_count`, `stream_closed`, each carrying
  `stream` (a monotonically increasing index) and `authorization` (the raw
  header, verbatim — the test asserts on the exact string, so it must not be
  parsed or normalised here),
- exit 0 on SIGTERM.

Two functions must be extracted for the tests above:

```go
// materialise generates the CA and serving certificate and writes ca.crt and
// token into dir.
func materialise(dir string, sans []string) (*material, error)

type material struct {
	Certificate tls.Certificate
	Token       string
}

// newRecorder returns a recorder writing one JSON object per line to w.
func newRecorder(w io.Writer) *recorder
func (r *recorder) record(kind string, fields map[string]any)
```

`recorder.record` must write `{"kind":"<kind>", ...fields}` and flush, so a
reader tailing the file sees events as they happen.

- [ ] **Step 4: Run to verify it passes**

Run: `nix develop -c go test ./cmd/spawnery-stubop/ -v`

Expected: PASS.

- [ ] **Step 5: Add the package to the flake**

In `flake.nix`, next to `spawnery-slp`:

```nix
          spawnery-stubop = pkgs.buildGoModule {
            pname = "spawnery-stubop";
            version = "0.2.0";
            src = ./.;
            vendorHash = "sha256-93cgbNfJURfz1mOM0nnOp9WGuMcFqkKlFGJ4tmdXeiw=";
            subPackages = [ "cmd/spawnery-stubop" ];
            env.CGO_ENABLED = 0;
          };
```

and add `spawnery-stubop` to the always-available `inherit` line. If the
vendor hash differs from `spawnery-slp`'s, Nix will print the correct one —
use that, do not invent one.

- [ ] **Step 6: Write the level-2 test script**

`hack/agent-test.sh`:

```bash
#!/usr/bin/env bash
# The Paper agent, proven against a real operator-shaped counterpart.
#
# This is the level that the unit tests structurally cannot reach. Three of the
# agent's obligations only exist inside a real JVM with Paper's classloader, a
# real TLS handshake against the pinned CA, and a real HTTP/2 stream:
#
#   - that the shaded gRPC stack does not meet Paper's own protobuf and Netty,
#   - that the channel trusts the mounted bundle and nothing else,
#   - that a renewal really overlaps rather than merely being scheduled.
#
# The last one is what docs/known-issues.md calls non-optional, and a unit test
# can only claim it.
set -euo pipefail

CONTAINER="${CONTAINER:-docker}"
IMAGE="${IMAGE:?IMAGE must be set}"
STUBOP="${STUBOP:?STUBOP must be set}"
DEADLINE="${DEADLINE:-240}"

NAME="spawnery-agent-test-$$"
VOLUME="spawnery-agent-test-$$"
WORK="$(mktemp -d)"
EVENTS="$WORK/events.jsonl"
STUB_PID=""

cleanup() {
	[ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
	"$CONTAINER" rm -f "$NAME" >/dev/null 2>&1 || true
	"$CONTAINER" volume rm -f "$VOLUME" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

mkdir -p "$WORK/agent"

# host-gateway is understood by both Docker and Podman, so the container
# reaches the stub the same way under either runtime, and the SAN below is the
# name it dials.
"$STUBOP" \
	--dir "$WORK/agent" \
	--san stubop \
	--listen ":19443" \
	--report-interval 1 \
	--renew-after 5 \
	--hard-deadline 20 \
	>"$EVENTS" 2>"$WORK/stub.log" &
STUB_PID=$!

# The container runs as uid 10001 and reads these through a bind mount.
sleep 1
chmod 0755 "$WORK/agent"
chmod 0644 "$WORK/agent/ca.crt" "$WORK/agent/token"

"$CONTAINER" volume create "$VOLUME" >/dev/null
"$CONTAINER" run -d --name "$NAME" \
	--add-host stubop:host-gateway \
	--read-only --tmpfs /tmp:rw,exec,size=256m \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--memory 2g \
	-v "$VOLUME:/data" \
	-v "$WORK/agent:/var/run/spawnery:ro" \
	-e SPAWNERY_MAX_PLAYERS=100 \
	-e SPAWNERY_OPERATOR_ENDPOINT=stubop:19443 \
	"$IMAGE" >/dev/null

await_event() {
	local what="$1" start=$SECONDS
	until jq -e "select(.kind == \"$what\")" <"$EVENTS" >/dev/null 2>&1; do
		if [ -z "$("$CONTAINER" ps -q --filter "name=^${NAME}$")" ]; then
			echo "the container exited before sending $what" >&2
			"$CONTAINER" logs "$NAME" >&2
			cat "$WORK/stub.log" >&2
			exit 1
		fi
		if [ $((SECONDS - start)) -gt "$DEADLINE" ]; then
			echo "no $what within ${DEADLINE}s" >&2
			cat "$EVENTS" >&2
			"$CONTAINER" logs "$NAME" | tail -40 >&2
			exit 1
		fi
		sleep 2
	done
}

echo "waiting up to ${DEADLINE}s for the agent to greet..."
await_event hello
echo "the agent connected"

# The header the operator's interceptor matches character for character.
expected="Bearer $(cat "$WORK/agent/token")"
actual="$(jq -r 'select(.kind == "hello") | .authorization' <"$EVENTS" | head -1)"
if [ "$actual" != "$expected" ]; then
	echo "authorization header is $(printf '%q' "$actual"), want $(printf '%q' "$expected")" >&2
	exit 1
fi
echo "authorization header is exact"

await_event ready
echo "the agent reported readiness"

await_event player_count
slots="$(jq -r 'select(.kind == "player_count") | .slots' <"$EVENTS" | head -1)"
if [ "$slots" != "100" ]; then
	echo "slots = $slots, want 100 from the server's own max-players" >&2
	exit 1
fi
echo "the agent reports player counts with the enforced maximum"

# The overlap. Two streams must exist, and the second must have greeted before
# the first was closed - which is exactly what a make-before-break renewal
# looks like from the operator's side, and what a break-before-make one does
# not.
echo "waiting for a renewal..."
start=$SECONDS
until [ "$(jq -rs '[.[] | select(.kind == "stream_opened")] | length' <"$EVENTS")" -ge 2 ]; do
	if [ $((SECONDS - start)) -gt 60 ]; then
		echo "no second stream within 60s of a 5s renewal deadline" >&2
		cat "$EVENTS" >&2
		exit 1
	fi
	sleep 2
done

if ! jq -rs '
	(map(select(.kind == "hello" and .stream == 1)) | length) as $second_greeted |
	(map(select(.kind == "stream_closed" and .stream == 0)) | length) as $first_closed |
	if $second_greeted == 0 then "the second stream never greeted"
	elif $first_closed > 0 and (
		(map(select(.kind == "stream_closed" and .stream == 0)) | first | .seq) <
		(map(select(.kind == "hello" and .stream == 1)) | first | .seq)
	) then "the first stream closed before the second greeted: break before make"
	else empty end
' <"$EVENTS" | grep -q .; then
	echo "the renewal overlapped: the new stream greeted before the old one closed"
else
	jq -rs '.' <"$EVENTS" >&2
	echo "renewal did not overlap" >&2
	exit 1
fi

echo "agent-test: ok"
```

The `seq` field is a monotonically increasing counter the recorder adds to
every event; add it in `newRecorder` if it is not there yet, since the overlap
assertion is the whole point of this script and needs a total order.

- [ ] **Step 7: Wire it into the Makefile**

```makefile
STUBOP ?= $(shell nix build .#spawnery-stubop --no-link --print-out-paths)/bin/spawnery-stubop

# The level-2 proof from design section 9. Not part of `test` or `all`, for the
# same reason image-test is not: it needs a container runtime and only works on
# x86_64-linux.
.PHONY: agent-test
agent-test: image-load
	CONTAINER=$(CONTAINER) IMAGE=$(IMAGE) STUBOP=$(STUBOP) hack/agent-test.sh
```

Add `jq` to the dev shell packages in `flake.nix`.

- [ ] **Step 8: Run it**

Run: `make agent-test`

Expected: every echoed line appears in order, ending with `agent-test: ok`.

If the container cannot reach `stubop:19443`, check `--add-host` support first
(`podman run --rm --add-host stubop:host-gateway alpine getent hosts stubop`)
before changing the agent.

- [ ] **Step 9: Commit**

```bash
git add cmd/spawnery-stubop hack/agent-test.sh Makefile flake.nix
git commit
```

---

### Task 10: The evidence run and the documents

**Files:**
- Modify: `README.md`
- Modify: `docs/known-issues.md`
- Create: `docs/handover-milestone-3.md`
- Delete: `docs/handover-milestone-2b.md` is **kept**, not deleted.

**Interfaces:**
- Consumes: everything.
- Produces: nothing code depends on.

- [ ] **Step 1: Run the full suite**

Run: `nix develop -c make test && nix develop -c make agent && make image-test && make agent-test && make image-repro`

Expected: all green. Record the `make test` wall-clock time; the Global
Constraints require it not to have grown.

- [ ] **Step 2: The evidence run against kind**

Follow `README.md`'s existing "Trying it locally against kind" section verbatim,
with the new image tag. Capture the actual output of:

```bash
nix develop -c kubectl get networks,servergroups,servers,pods -n minecraft
```

The `Server` should now reach phase `Ready` and `status.players` should carry a
number. **Write down what actually happened**, not what was predicted — the
README's expected list is corrected to the observed output, not the other way
round. If the `Server` does not reach `Ready`, that is a finding, not a
formatting problem: report it and stop.

- [ ] **Step 3: Update the README**

- The Status section: milestone 2c is done, a `Server` reaches `Ready`, and
  what is still missing is the proxy layer (milestone 3) — a player still
  cannot connect.
- The Development section: `make agent`, `make agent-deps`, `make agent-test`,
  each with one line on when it is needed. `make agent-deps` needs the note
  that it reaches the network and is therefore in no other target.
- The kind section: the corrected expected list from Step 2.
- The pointer at the end: `docs/handover-milestone-3.md`.

- [ ] **Step 4: Update known-issues.md**

Add a "From milestone 2c" section carrying at minimum:

- **A read-only mount at `/data/plugins` breaks the start.** Mounts below
  `/data` are allowed by `podspec` and a ConfigMap mount is read-only, so the
  `cp` in the entrypoint fails under `set -eu` with a bare message. Same shape
  as the `/data/config` collision; both want the same fix.
- **The JRE module list is now derivable.** The Paper-side classpath stopped
  moving with this milestone. gRPC and okhttp pull modules Paper alone does
  not, so the list has to come from the complete classpath — and milestone 3
  adds Velocity, so cut it once, there, for both images.
- **`deps.json` has no CI guard.** Nothing fails if it drifts from
  `build.gradle.kts`; only a `make agent-deps` that produces a diff reveals it.
  Belongs with CI in milestone 6.
- **grpc-java's version is pinned twice.** `protoc-gen-grpc-java` comes from
  nixpkgs (1.83.1 at this pin) and the runtime artifacts from `deps.json`. A
  nixpkgs bump moves one without the other, and the symptom is a generated stub
  that does not match its runtime.
- Anything the evidence run in Step 2 turned up.

Also revisit the existing "Preconditions for milestone 2c" section: the three
obligations there are now met, and the section should say so and where, rather
than being deleted — the reasoning is what makes the next agent's obligations
legible.

- [ ] **Step 5: Write the handover**

`docs/handover-milestone-3.md`, modelled on `docs/handover-milestone-2c.md`:
where 2c stopped, what milestone 3 finds in place, the contract it builds
against, and the questions worth settling before code. At minimum it must carry
the four milestone-3 preconditions already in `known-issues.md` (the orphan
sweep, `ProxySession` plus the `spawnery-proxy` ServiceAccount, the
`oci-common.nix` factoring, and not extending `set_property`), plus what this
milestone learned that a Velocity agent will need: the Gradle-in-Nix shape,
the relocation set, and the stub-operator harness, all of which apply again
almost unchanged.

- [ ] **Step 6: Verify the documents against reality**

Run: `grep -rn '0\.1\.0\|1\.21\.4' README.md docs/ config/samples/ || true`

Expected: no stale image version or Minecraft version remains. Fix any that do.

- [ ] **Step 7: Commit**

```bash
git add README.md docs
git commit
```

---

## Self-Review

**Spec coverage.** §2 scope → Tasks 1-10 and nothing beyond; §3 measurements →
Global Constraints; §4.1 lockfile → Task 1 Step 4; §4.2 compile classpath →
Task 1 Step 2; §4.3 okhttp and relocation → Task 1 and Task 3; §4.4 toolchain →
Task 1 `gradle.properties` and the `proto` source set; §4.5 reproducibility →
Task 1 Step 7, Task 3 Step 7, Task 8 Step 8; §4.6 stubs → Task 2; §5 layout and
targets → Tasks 1, 9; §6 the five units → Tasks 1, 4, 5, 6, 7; §6.1 threading →
Task 7; §6.2 token → Task 4; §6.3 CA bundle → Task 4; §6.4 header → Task 4 and
Task 9; §6.5 ready → Tasks 5, 7; §6.6 renewal → Task 6 and Task 9; §6.7 failure
posture → Tasks 6, 7 and Task 8 Step 7; §7 image → Task 8; §8 error table →
Tasks 6, 7; §9 all three levels → Tasks 5/6, 9, 8; §10 acceptance → Task 10
Step 1; §11 open points → Task 10 Step 4.

**One deviation from the spec's implied approach, stated rather than buried.**
The spec's language rationale mentioned coroutines. The plan uses a
`ScheduledExecutorService` instead: `protoc-gen-grpc-kotlin` is not in nixpkgs,
so the generated stub is callback-based grpc-java, and wrapping it in coroutines
would add `kotlinx-coroutines` to the shaded jar to gain nothing a scheduler
does not already do — while an injected scheduler is what lets the tests drive
time instead of sleeping. Kotlin remains the language, as decided.

**Placeholder scan.** Task 9 Step 3 describes `main.go` as a specification
rather than as literal code — it is the one place where a full listing (roughly
250 lines of certificate generation and gRPC plumbing) would bury the
requirements it has to satisfy. The two extracted functions its tests call are
given as exact signatures, and the JSON event shape the level-2 script parses is
specified field by field. Every other code step carries the actual code.

**Type consistency.** `ServerState.markReady()/sample(players, slots)` is used
identically in Tasks 1, 5, 7. `SessionLoop`'s constructor is defined in Task 5
and extended once, explicitly, in Task 6 (`jitter` appended last).
`Environment.Configured(endpoint, caBundle, tokenPath)` in Task 7 matches its
consumer in the same task. `OperatorChannel.build(endpoint, caBundlePem)` and
`BearerCredentials.of(tokens)` in Task 4 match their call sites in Tasks 5
and 7. The jar path `$out/share/spawnery/spawnery-agent.jar` is fixed in Task 1
and consumed unchanged in Tasks 3 and 8.
