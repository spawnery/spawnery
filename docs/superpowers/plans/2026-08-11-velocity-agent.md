# Velocity Agent Implementation Plan (milestone 3c)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A player can join — a Velocity plugin that opens a `ProxySession`,
serves the proxy pod's readiness probe once a server list has arrived, mirrors
that list into Velocity's registry, and places players on join and on drain.

**Architecture:** `agent/` becomes a three-project Gradle build. `common` holds
the session machinery generic over its message types; `paper` and `velocity`
each supply a role and their platform wiring. One Nix derivation builds both
shaded jars. The operator is untouched apart from one new environment variable.

**Tech Stack:** Kotlin 2.4.10 on JVM target 21, Gradle with the nixpkgs setup
hook and a checked-in `deps.json`, gRPC 1.83.1 with `grpc-okhttp`, protobuf
4.35.1, Velocity 3.5.1 build 615, Go 1.x for the operator and the join client,
Nix flakes, podman.

The design is `docs/superpowers/specs/2026-08-11-velocity-agent-design.md`.
Section references below (§3, §4.3, …) are its sections.

**On the style of this plan.** Where a task's deliverable is an interface, a
constant or a test *case*, it is written out verbatim and must be used exactly
as given. Where the deliverable is an implementation, this plan states the
invariants it must hold and does not transcribe the body. That is deliberate
and it is the lesson of the two preceding milestones: 3a's plan inlined
implementations and produced transcription defects that reached the branch,
while 3b's stated invariants and produced specification defects that surfaced
at task boundaries, where they are cheap. Test names are given without bodies
for the same reason — the name and the surrounding paragraph fix *what* is
pinned; how to pin it is the implementer's job and their reviewer's gate.

## Global Constraints

Every task's requirements implicitly include this section.

- **Commit messages use Conventional Commits**: `feat(scope): what changed`,
  `fix(scope): what changed`, `chore(scope): …`, `docs(scope): …`,
  `refactor(scope): …`, `test(scope): …`. This overrides the sentence-style
  subjects in this repository's own history. Subject in the imperative, body
  explaining *why*, and every commit ends with
  `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **JVM target is 21 everywhere.** Gradle's toolchain auto-detection and
  auto-download stay off (`agent/gradle.properties`). The images ship a JDK 25;
  the plugins are compiled against 21 and run on 25.
- **`nix build` filters source through the git index.** A new or moved file
  that has not been `git add`ed does not exist for the build. A suspiciously
  fast, quiet build right after adding or moving files is a stale tree, not
  success. `git add` before every `nix build` in this plan.
- **Every class in an agent jar lives under `cloud/spawnery/agent/`**, the
  plugin's own or a relocated dependency under
  `cloud.spawnery.agent.shaded.`. `hack/agent-jar-check.sh` fails the build on
  any other package, named in the relocation list or not.
- **`grpc-okhttp`, never `grpc-netty`**, and the TLS-1.3-only `ConnectionSpec`
  override stays. `internal/agentserver` serves `MinVersion: VersionTLS13`;
  grpc-okhttp's default spec on a JDK is TLS 1.2 only, and the handshake dies
  with a `protocol_version` alert before a byte of HTTP/2.
- **Kotlin sources carry no license header.** Go sources carry the Apache
  header from `hack/boilerplate.go.txt`; copy it from a neighbouring file.
- **Constants that cross the language boundary** are hard-coded on the Kotlin
  side with a comment naming their Go origin, exactly as `AGENT_DIR` already
  names `internal/podspec.AgentMountPath`. The values: agent mount path
  `/var/run/spawnery`, ready port `8081` (`internal/podspec.ProxyReadyPort`),
  Minecraft port `25565`.
- **Build and test commands**, verified on this machine:

  ```bash
  nix develop -c make test          # Go only, ~38 s; internal/controller is
                                    # ~34 of it, because envtest boots a real
                                    # API server. Not a hang.
  nix develop -c make agent         # both jars and both JUnit suites
  nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
      make agent-test CONTAINER=podman
  nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
      make image-test CONTAINER=podman
  ```

  `CONTAINER=podman` because the Makefile defaults to `docker`, which is not
  installed. `TMPDIR` on a disk-backed path because `/tmp` is a 2 GB tmpfs and
  podman extracts the OCI archive there. `~/.config/nix/nix.conf` already
  enables `nix-command flakes`, so no `--extra-experimental-features` prefix is
  needed anywhere.
- **A green image or agent test proves nothing if podman already has the
  layers.** When a task's verdict depends on a *first* successful run, clear
  with `podman rmi` and `podman system prune -af` first.
- **`imageVersion` is `0.2.0`** (`flake.nix`). Velocity is `3.5.1` build `615`,
  `config-version = "2.8"`.

---

## File Structure

**Moved (Task 1).** `agent/paper/` is dissolved into a three-project build:

| From | To |
|---|---|
| `agent/paper/settings.gradle.kts` | `agent/settings.gradle.kts` |
| `agent/paper/gradle.properties` | `agent/gradle.properties` |
| `agent/paper/deps.json` | `agent/deps.json` |
| `agent/paper/src/proto/java/` | `agent/common/src/proto/java/` |
| `Environment.kt`, `TokenSource.kt`, `OperatorChannel.kt`, `SessionLoop.kt` | `agent/common/src/main/kotlin/cloud/spawnery/agent/` |
| `AgentPlugin.kt`, `ServerState.kt` | `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/` |
| the matching test files | `agent/common/src/test/…` and `agent/paper/src/test/…/paper/` |

**Created:**

| File | Responsibility |
|---|---|
| `agent/common/src/main/kotlin/cloud/spawnery/agent/AgentRole.kt` | the one seam between the loop and a role: `AgentRole<Req, Resp>` and `Directive` |
| `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/ServerRole.kt` | Paper's role |
| `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt` | the only class that touches the Velocity API |
| `…/velocity/ProxyEnvironment.kt` | the two variables only a proxy needs, and the refusal |
| `…/velocity/ReadyGate.kt` | the readiness socket |
| `…/velocity/ProxyRegistry.kt` | the narrow seam onto Velocity's server registry, plus its adapter |
| `…/velocity/ServerDirectory.kt` | operator server list → Velocity registry |
| `…/velocity/Players.kt` | the narrow seam onto the connected players, plus its adapter |
| `…/velocity/Router.kt` | which server a player goes to |
| `…/velocity/Drain.kt` | moving players off a draining server |
| `…/velocity/ProxyState.kt` | the two numbers reported |
| `…/velocity/ProxyRole.kt` | the proxy's `AgentRole` |
| `agent/velocity/src/main/resources/velocity-plugin.json` | the plugin descriptor, written by hand |
| `nix/agents.nix` | one derivation, two jars (replaces `nix/paper-agent.nix`) |
| `internal/mcproto/` | Minecraft packet framing, extracted from `internal/slp` |
| `internal/mcjoin/` | a client that logs in far enough to be routed |
| `cmd/spawnery-join/` | the command around it |
| `docs/runbook-milestone-3-evidence.md` | how the milestone's evidence run is performed |
| `docs/handover-milestone-4.md` | what milestone 4 finds in place |

**Modified:** `Makefile`, `flake.nix`, `nix/velocity-image.nix`,
`hack/agent-jar-check.sh`, `hack/agent-test.sh`, `hack/velocity-image-test.sh`,
`cmd/spawnery-stubop/main.go`, `internal/podspec/proxy.go`, `internal/slp/slp.go`,
`README.md`, `docs/known-issues.md`.

---

## Task 1: Split `agent/` into `common` and `paper`

Behaviour must not change. This task moves files and rewires the build; the
proof is that every existing test still passes and the Paper image still works.

**Files:**
- Create: `agent/settings.gradle.kts`, `agent/gradle.properties`,
  `agent/common/build.gradle.kts`, `agent/paper/build.gradle.kts` (rewritten),
  `nix/agents.nix`
- Move: everything in the table under **File Structure** above
- Delete: `agent/paper/settings.gradle.kts`, `agent/paper/gradle.properties`,
  `nix/paper-agent.nix`
- Modify: `Makefile` (the `proto`, `agent` and `agent-deps` targets),
  `flake.nix`, `nix/paper-image.nix`, `hack/agent-jar-check.sh`

**Interfaces:**
- Produces: Gradle projects `:common`, `:paper`. `:common`'s main source set
  includes `src/proto/java` as an ordinary `java.srcDir` and exports the gRPC
  and protobuf stubs on its `api` configuration. `:paper` depends on it with
  `implementation(project(":common"))`.
- Produces: `nix/agents.nix` building `spawnery-agents`, version `imageVersion`,
  installing `$out/share/spawnery/paper/spawnery-agent.jar`. Flake attribute
  `agents`.
- Produces: Kotlin package `cloud.spawnery.agent` for the shared classes,
  `cloud.spawnery.agent.paper` for `AgentPlugin`.

**`ServerState` stays in `:common` for this task.** The first draft of this
plan moved it to `:paper` here, which cannot build: `SessionLoop` takes
`state: ServerState` in its constructor and `SessionLoopTest` constructs one in
seventeen places, so `:common` would depend on `:paper` while `:paper` already
depends on `:common` — a project-graph cycle, not an import problem. Task 3 is
what removes the reason for the dependency, by replacing that constructor
parameter with a role, and Task 3 therefore performs the move in the same
commit. Leaving it here would make Task 1 a rewrite, which is the one thing it
must not be.

- [ ] **Step 1: Read the two things that make this task non-mechanical**

`agent/paper/build.gradle.kts` in full, and its four long comments in
particular: the Paper-API-from-the-bundle rule, the separate `proto` source
set, the `plain` classifier on `tasks.jar`, and the relocation list. Three of
the four carry over unchanged. The second does not, and Step 3 says why.

- [ ] **Step 2: Create the root build**

`agent/settings.gradle.kts`:

```kotlin
rootProject.name = "spawnery-agents"

include("common")
include("paper")
```

Move `agent/paper/gradle.properties` to `agent/gradle.properties` unchanged and
`agent/paper/deps.json` to `agent/deps.json` unchanged. The lockfile is
regenerated in Step 9; moving it first means the build has something to start
from.

- [ ] **Step 3: Create `agent/common/build.gradle.kts`**

The generated stubs become an ordinary source directory of `common`'s main
source set:

```kotlin
sourceSets.main {
    java.srcDir("src/proto/java")
}
```

**Do not carry over the separate `proto` source set.** It existed for exactly
one reason: `javac` 21 cannot resolve a class out of Paper's class-file-major-69
jars, so the generated Java had to compile with `paperLibraries` off its
classpath. `common` depends on no platform at all, so the hazard does not exist
here and the machinery that guarded against it would only obscure that. Record
that reasoning as a comment in the file — the next reader will otherwise
"restore" the source set.

Dependencies, with the versions copied verbatim from the old file:

```kotlin
dependencies {
    api("io.grpc:grpc-api:1.83.1")
    api("io.grpc:grpc-protobuf:1.83.1")
    api("io.grpc:grpc-stub:1.83.1")
    api("com.google.protobuf:protobuf-java:4.35.1")
    compileOnlyApi("javax.annotation:javax.annotation-api:1.3.2")
    implementation("io.grpc:grpc-okhttp:1.83.1")

    testImplementation(kotlin("test"))
    testImplementation(platform("org.junit:junit-bom:5.11.4"))
    testImplementation("org.junit.jupiter:junit-jupiter:5.11.4")
    testImplementation("io.grpc:grpc-inprocess:1.83.1")
    testImplementation("io.grpc:grpc-testing:1.83.1")
    testImplementation("org.bouncycastle:bcpkix-jdk18on:1.79")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}
```

`api` rather than `implementation` for the stub artifacts: `:paper` and
`:velocity` name the generated message types in their own sources, so those
types have to reach their compile classpath.

`common` applies the Kotlin plugin but **not** the shadow plugin — it produces a
plain jar that each agent's `shadowJar` bundles and relocates.

- [ ] **Step 4: Move the shared sources and rewrite `agent/paper/build.gradle.kts`**

Move `Environment.kt`, `TokenSource.kt`, `OperatorChannel.kt` (which also holds
`BearerCredentials`) and `SessionLoop.kt` to
`agent/common/src/main/kotlin/cloud/spawnery/agent/`, package unchanged. Their
tests — `EnvironmentTest.kt`, `TokenSourceTest.kt`, `OperatorChannelTest.kt`,
`SessionLoopTest.kt`, `FakeOperator.kt`, `TestCerts.kt`, `ContractTest.kt` —
move to `agent/common/src/test/kotlin/cloud/spawnery/agent/`.

Move `AgentPlugin.kt` and `ServerState.kt` to
`agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/`, changing the package
declaration to `cloud.spawnery.agent.paper` and adding imports for the shared
classes. `ServerStateTest.kt` moves the same way. Update the `main:` line of
`agent/paper/src/main/resources/paper-plugin.yml`.

`SessionLoop.kt` is still typed to `ServerMessage` and `OperatorToServer` at the
end of this task. Task 3 generifies it. Keeping the two changes apart is what
lets a reviewer tell a move from a rewrite.

`agent/paper/build.gradle.kts` keeps the shadow plugin, `paperLibraries`, the
`plain` classifier, the reproducible-archive flags, the relocation list and the
test configuration, and gains `implementation(project(":common"))`. It loses the
`proto` source set and the four dependencies that fed it.

- [ ] **Step 5: Rewrite the Makefile's `proto` target**

```make
	rm -rf agent/common/src/proto/java
	mkdir -p agent/common/src/proto/java
	protoc \
		--proto_path=proto \
		--java_out=agent/common/src/proto/java \
		--grpc-java_out=agent/common/src/proto/java \
		proto/spawnery/agent/v1alpha1/agent.proto
```

- [ ] **Step 6: Write `nix/agents.nix`**

Start from `nix/paper-agent.nix` verbatim and change five things:

- `pname = "spawnery-agents"`, `version = imageVersion`. The old
  `${paper.paperVersion}-${imageVersion}` said something true about an artifact
  belonging to one platform and would say something false about one belonging to
  two. Record that in a comment.
- `src = ../agent` rather than `../agent/paper`.
- `mitmCache` data is `../agent/deps.json`.
- `gradleBuildTask = "shadowJar"` — unqualified. Gradle resolves the name in
  every subproject that has such a task, and only `:paper` (and later
  `:velocity`) does. Naming tasks explicitly would depend on how the nixpkgs
  hook joins a multi-task value; one name depends on nothing.
- `installPhase` installs from the subproject's build directory:

  ```bash
  install -Dm644 paper/build/libs/spawnery-paper-agent-${finalAttrs.version}.jar \
    $out/share/spawnery/paper/spawnery-agent.jar
  ```

  The jar's base name comes from `:paper`'s `archivesName`; set it explicitly in
  `agent/paper/build.gradle.kts` (`base { archivesName = "spawnery-paper-agent" }`)
  rather than relying on the subproject directory name.

`installCheckPhase` calls `hack/agent-jar-check.sh` on the installed jar. Add a
third argument for the flavour now, defaulting to `paper`, so Task 4 only has to
add a list and not change the interface:

```bash
bash ${../hack/agent-jar-check.sh} $out/share/spawnery/paper/spawnery-agent.jar "$PWD" paper
```

- [ ] **Step 7: Rewire `flake.nix`, `nix/paper-image.nix` and the Makefile**

`flake.nix`: replace the `paper-agent` attribute with

```nix
agents = pkgs.callPackage ./nix/agents.nix { inherit paper imageVersion; };
```

`paper` stays an explicit argument — `pkgs.callPackage` fills arguments from
`pkgs` only, and `paper` is a local `let` binding. What changes is what it is
*for*: the `postPatch` symlink that hands Gradle the Paper API, and no longer
the version string.

Three more references to the old name have to move in the same file: the
`packages` set (`inherit spawnery-slp spawnery-stubop spawnery-config
paper-agent;` becomes `… agents;`), the `paper-image` call (`inherit paper
spawnery-slp spawnery-config paper-agent imageVersion oci-common;` becomes
`… agents …`), and any `checks` entry naming it. Grep for `paper-agent` and
leave none.

`nix/paper-image.nix` takes `agents` instead of `paper-agent` and reads
`${agents}/share/spawnery/paper/spawnery-agent.jar`.

Makefile:

```make
.PHONY: agent
agent:
	nix build .#agents

.PHONY: agent-deps
agent-deps:
	"$$(nix build --no-link --print-out-paths .#agents.mitmCache.updateScript)"
```

- [ ] **Step 8: `git add` everything, including the moved files**

```bash
git add -A agent nix flake.nix Makefile hack
git status --short
```

Nix builds from the git index. Skipping this makes the next step build the old
tree and pass for the wrong reason.

- [ ] **Step 9: Regenerate the lockfile and build**

```bash
nix develop -c make agent-deps
git add agent/deps.json
nix develop -c make agent
```

Expected: the build resolves `:common` and `:paper`, runs both `test` tasks,
and installs one jar. `make agent-deps` reaches Maven Central, so it needs
network.

- [ ] **Step 10: Run every gate that this task can break**

```bash
nix develop -c make test
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make image-test CONTAINER=podman
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make agent-test CONTAINER=podman
```

Expected: all green. `make agent-test` is the one that would catch a package
move that broke the plugin's `main:` reference — the plugin would load as
dormant or not at all, and the harness would see silence instead of a stream.

- [ ] **Step 11: Commit**

```bash
git add -A
git commit -m "refactor(agent): split agent/ into a common and a paper project"
```

The body should say that behaviour is unchanged, that the `proto` source set is
deliberately not carried into `common`, and that `SessionLoop` is still typed to
the server messages.

---

## Task 2: `SPAWNERY_FALLBACK_GROUPS` on the proxy pod spec

**Files:**
- Modify: `internal/podspec/proxy.go`
- Test: `internal/podspec/proxy_test.go`

**Interfaces:**
- Produces: `podspec.EnvFallbackGroups = "SPAWNERY_FALLBACK_GROUPS"`, set on the
  Velocity container to `strings.Join(group.Spec.Routing.FallbackGroups, ",")`.

- [ ] **Step 1: Write the failing test**

In `internal/podspec/proxy_test.go`, following the file's existing helpers for
building a `Network` and a `ProxyGroup`:

```go
func TestProxyPodCarriesTheFallbackGroups(t *testing.T) {
	net := testNetwork()
	group := testProxyGroup()
	group.Spec.Routing.FallbackGroups = []string{"lobby", "hub"}

	pod, err := podspec.BuildProxyPod(net, group, "edge-0", "operator:9443")
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}

	got := envValue(pod, podspec.EnvFallbackGroups)
	if got != "lobby,hub" {
		t.Fatalf("%s = %q, want %q", podspec.EnvFallbackGroups, got, "lobby,hub")
	}
}
```

Use the file's existing helper for reading an environment variable out of the
pod; if there is none, add `envValue(pod *corev1.Pod, name string) string`
returning `""` when absent.

- [ ] **Step 2: Run it and watch it fail**

```bash
nix develop -c go test ./internal/podspec/ -run TestProxyPodCarriesTheFallbackGroups
```

Expected: a compile error naming `EnvFallbackGroups`.

- [ ] **Step 3: Add the constant and the variable**

In the `const` block next to `EnvPlayerLimit`:

```go
// EnvFallbackGroups carries ProxyGroup.spec.routing.fallbackGroups to the
// agent, comma separated and in order. It is the same list the operator puts
// in DrainPlayers.toGroups, so a join and a drain resolve against one source
// rather than two that can disagree. The CRD marks the field required with
// MinItems=1, so the agent treats an empty value as an operator bug and
// refuses to connect rather than coming up unable to route.
EnvFallbackGroups = "SPAWNERY_FALLBACK_GROUPS"
```

and in the container's `Env` slice, after `EnvPlayerLimit`:

```go
{Name: EnvFallbackGroups, Value: strings.Join(group.Spec.Routing.FallbackGroups, ",")},
```

- [ ] **Step 4: Run the whole package**

```bash
nix develop -c go test ./internal/podspec/
```

Expected: PASS. Some existing test may assert the exact length or content of the
`Env` slice; update it rather than working around it.

- [ ] **Step 5: Commit**

```bash
git add internal/podspec
git commit -m "feat(podspec): carry the fallback groups to the proxy agent"
```

---

## Task 3: Generify `SessionLoop` over `<Req, Resp>`

The most delicate task in the plan. `SessionLoop.kt` is 748 lines of which most
are comments recording five separately-discovered defects, and `SessionLoopTest.kt`
is 1046 lines that pin them.

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/AgentRole.kt`
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/ServerRole.kt`
- Create: `agent/common/src/test/kotlin/cloud/spawnery/agent/FakeRole.kt`
- Create: `agent/common/src/test/kotlin/cloud/spawnery/agent/AgentRoleSeamTest.kt`
- Create: `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/ServerRoleTest.kt`
- Move: `ServerState.kt` and `ServerStateTest.kt` from `:common` to
  `agent/paper/src/{main,test}/kotlin/cloud/spawnery/agent/paper/`, package
  `cloud.spawnery.agent.paper`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/SessionLoop.kt`
- Modify: `agent/common/src/test/kotlin/cloud/spawnery/agent/SessionLoopTest.kt`
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt`

**Why the `ServerState` move is in this task and not in Task 1.** Task 1 left
it in `:common` because `SessionLoop`'s constructor takes one, and moving it
while that was true would have made `:common` depend on `:paper` — a
project-graph cycle. Step 2 removes that parameter. The move belongs in the
same commit as its own justification, and `FakeRole` (Step 5) carries its own
counters rather than reusing `ServerState`, so `:common`'s tests do not
reintroduce the dependency by the back door.

**Interfaces:**
- Produces: `AgentRole<Req, Resp>` and `Directive` exactly as written in Step 1.
- Produces: `SessionLoop<Req, Resp>(channels, credentials, role, scheduler,
  version, log, jitter, fallbackAnswerBoundMillis)` — `state: ServerState` is
  gone from the constructor, replaced by `role`.
- Produces: `SessionLoop.send(message: Req)`, public, delivering on the current
  session if there is one and doing nothing otherwise.
- Produces: `ServerRole(state: ServerState)` with `fun ready(): ServerMessage`.

- [ ] **Step 1: Write `AgentRole.kt`**

```kotlin
package cloud.spawnery.agent

import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.StreamObserver

/**
 * Everything about one agent's stream that differs between the two roles.
 *
 * Deliberately one method for incoming messages rather than a classify/apply
 * pair: two readers of the same messageCase in two files is how the two halves
 * drift.
 */
interface AgentRole<Req, Resp> {
    /** Which rpc this role speaks. */
    fun open(
        channel: ManagedChannel,
        credentials: CallCredentials,
        observer: StreamObserver<Resp>,
    ): StreamObserver<Req>

    /** The first message on every stream. */
    fun hello(version: String): Req

    /** The periodic report. */
    fun playerCount(): Req

    /**
     * Applies the role-specific effect of one operator message and returns
     * what the loop itself must act on. Returning [Directive.None] is the
     * normal outcome for a role-specific message and for one this agent does
     * not recognise.
     */
    fun onMessage(message: Resp): Directive
}

sealed interface Directive {
    data class Report(val seconds: Int) : Directive
    data class Deadline(val renewAfterSeconds: Int, val hardDeadlineSeconds: Int) : Directive
    data object None : Directive
}
```

- [ ] **Step 2: Generify `SessionLoop` and `Session`**

Mechanical, and the rule is: **change type parameters and dispatch, change
nothing else.** Every comment stays, including the ones naming
`internal/agentserver/server.go` line numbers.

- `private class Session(...)` becomes `private class Session<Req>(...)`;
  `AtomicReference<StreamObserver<ServerMessage>?>`, `send(message: ServerMessage)`
  and the `ClientCallStreamObserver<ServerMessage>` cast in `close` follow.
- `class SessionLoop(...)` becomes `class SessionLoop<Req, Resp>(...)`. Replace
  the `state: ServerState` parameter with `role: AgentRole<Req, Resp>`.
- In `connect()`, `AgentServiceGrpc.newStub(channel).withCallCredentials(credentials)`
  and `stub.serverSession(fromOperator)` collapse into
  `role.open(channel, credentials, fromOperator)`. The `try`/`catch` around it,
  which shuts the channel down and rethrows, stays exactly as it is.
- The Hello send becomes `send(session, role.hello(version))`.
- `fromOperator.onNext` keeps `answerArrived()`, `attempt.set(0)` and
  `takeOver()` in that order and with their comments, then:

  ```kotlin
  when (val directive = role.onMessage(value)) {
      is Directive.Report -> startReporting(session, directive.seconds)
      is Directive.Deadline -> {
          hardDeadlineMillis.set(
              TimeUnit.SECONDS.toMillis(directive.hardDeadlineSeconds.toLong()),
          )
          scheduleRenewal(session, directive.renewAfterSeconds)
      }
      Directive.None -> Unit
  }
  ```

- `startReporting`'s timer body becomes `send(session, role.playerCount())`.
- `readyChanged()` is deleted and replaced by:

  ```kotlin
  /**
   * Sends one message on the current stream if there is one, and does nothing
   * if there is not. The agent's own state is the caller's business: this is
   * the immediate notification, not the state itself, and every Hello carries
   * the state anyway.
   */
  fun send(message: Req) {
      val session = current.get() ?: return
      send(session, message)
  }
  ```

  The private `send(session, message)` keeps its name; Kotlin resolves the two
  by arity.

- [ ] **Step 3: Write `ServerRole.kt`**

```kotlin
package cloud.spawnery.agent.paper

// imports: cloud.spawnery.agent.AgentRole, Directive, the pb types, grpc types

/** Paper's half of the channel. */
class ServerRole(private val state: ServerState) : AgentRole<ServerMessage, OperatorToServer> {
    override fun open(
        channel: ManagedChannel,
        credentials: CallCredentials,
        observer: StreamObserver<OperatorToServer>,
    ): StreamObserver<ServerMessage> =
        AgentServiceGrpc.newStub(channel).withCallCredentials(credentials).serverSession(observer)

    override fun hello(version: String): ServerMessage =
        ServerMessage.newBuilder()
            .setHello(Hello.newBuilder().setVersion(version).setReady(state.ready))
            .build()

    override fun playerCount(): ServerMessage =
        ServerMessage.newBuilder()
            .setPlayerCount(
                PlayerCount.newBuilder().setPlayers(state.players).setSlots(state.slots),
            )
            .build()

    override fun onMessage(message: OperatorToServer): Directive =
        when (message.messageCase) {
            OperatorToServer.MessageCase.REPORT_INTERVAL ->
                Directive.Report(message.reportInterval.seconds)
            OperatorToServer.MessageCase.SESSION_DEADLINE ->
                Directive.Deadline(
                    message.sessionDeadline.renewAfterSeconds,
                    message.sessionDeadline.hardDeadlineSeconds,
                )
            else -> Directive.None
        }

    /** The immediate readiness notification. Readiness itself rides on Hello. */
    fun ready(): ServerMessage =
        ServerMessage.newBuilder().setReady(Ready.getDefaultInstance()).build()
}
```

- [ ] **Step 4: Rewire Paper's `AgentPlugin`**

Construct `ServerRole(state)` and pass it to `SessionLoop`. `onServerLoad`
becomes:

```kotlin
if (state.markReady()) {
    loop?.send(role.ready())
}
```

with `role` held as a field alongside `loop`. The guard that used to live inside
`readyChanged()` — `if (!state.ready) return` — is now the `markReady()` check,
which is strictly stronger: it fires only on the transition.

- [ ] **Step 5: Adapt `SessionLoopTest.kt`**

Every construction of `SessionLoop(...)` gains `role = ServerRole(state)` in
place of `state = state`, and every `loop.readyChanged()` becomes
`loop.send(role.ready())`. **Nothing else about the suite changes.** If a test
needs more than that to pass, the generification changed behaviour and the
change is wrong, not the test.

Note that `SessionLoopTest` now lives in `common` while `ServerRole` lives in
`paper`. Two ways out, and the second is the one to take:

- Move the suite to `:paper`. Rejected: the tests are about `SessionLoop`, and
  putting them where the class is not makes the next reader look in the wrong
  project.
- Give `:common`'s test source set a role of its own. `FakeRole` in Step 6 is
  written for the seam test; make it complete enough that `SessionLoopTest` uses
  it too, wrapping the same `ServerState`-shaped counters. The suite then tests
  the loop against a role that does exactly what `ServerRole` does, in the
  project the loop lives in.

Concretely: `FakeRole` speaks the real `ServerMessage`/`OperatorToServer` types
and the real `serverSession` rpc, so the existing `FakeOperator` and every
assertion about wire content keep working unchanged. What it adds over
`ServerRole` is recording: how many times `hello` and `playerCount` were built,
and every `Directive` returned. It also carries a `ready(): ServerMessage`, so
`loop.send(role.ready())` reads the same in the suite as in production.

`FakeRole` is a test double, so `ServerRole` itself would then be untested.
Add `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/ServerRoleTest.kt`
with four cases, all of them about the mapping and none about the loop:

```kotlin
@Test fun `hello carries the version and the current readiness`()
@Test fun `the report carries the sampled players and slots`()
@Test fun `a report interval message yields a Report directive`()
@Test fun `a session deadline message yields a Deadline directive`()
```

- [ ] **Step 6: Write `AgentRoleSeamTest.kt`**

Four tests, each on the seam and not on the loop:

```kotlin
@Test fun `a report directive starts the reporting timer`()
@Test fun `a deadline directive schedules a renewal and sets the answer bound`()
@Test fun `an unrecognised message produces no directive and no scheduling`()
@Test fun `the hello the role builds is the first message on the wire`()
```

Each drives a `SessionLoop` over the in-process transport against `FakeOperator`
with a `FakeRole` whose `onMessage` returns a directive the test dictates, and
asserts on the scheduler and on what `FakeOperator` received. The point is that
a role returning `Directive.Report(5)` starts a timer regardless of which
message produced it — that is the contract the Velocity role will rely on.

- [ ] **Step 7: Build and run both suites**

```bash
git add -A agent
nix develop -c make agent
```

Expected: green. A failure inside `SessionLoopTest` means one of the five
invariants moved; do not adapt the test, fix the loop.

- [ ] **Step 8: Prove it on the wire**

```bash
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make agent-test CONTAINER=podman
```

Expected: all three phases pass. This is the level that measures the renewal
overlap on a real connection, and the one that caught the in-process
transport's synchronous delivery hiding a wrong ordering in milestone 2c. A
generification that broke make-before-break would pass Step 7 and fail here.

- [ ] **Step 9: Commit**

```bash
git add -A agent
git commit -m "refactor(agent): make the session loop generic over its role"
```

---

## Task 4: The Velocity subproject, `ProxyEnvironment` and `ReadyGate`

At the end of this task the Velocity image carries a plugin that loads, stays
dormant without an operator, and is proven not to have a linkage error.

**Files:**
- Create: `agent/velocity/build.gradle.kts`,
  `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt`,
  `…/ProxyEnvironment.kt`, `…/ReadyGate.kt`,
  `agent/velocity/src/main/resources/velocity-plugin.json`
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/ProxyEnvironmentTest.kt`,
  `…/ReadyGateTest.kt`
- Modify: `agent/settings.gradle.kts`, `nix/agents.nix`, `nix/velocity-image.nix`,
  `flake.nix`, `hack/agent-jar-check.sh`, `hack/velocity-image-test.sh`, `Makefile`

**Interfaces:**
- Consumes: `Environment` from `:common` (Task 1), `AgentRole` (Task 3).
- Produces: `ProxyEnvironment.from(getenv, agentDir): ProxyEnvironment` with
  `Configured(base: Environment.Configured, playerLimit: Int, fallbackGroups: List<String>)`
  and `Dormant(reason: String)`.
- Produces: `ReadyGate(port: Int, log: (String, Throwable?) -> Unit)` with
  `open()`, `val boundPort: Int`, `val isOpen: Boolean`, `close()`.
- Produces: `$out/share/spawnery/velocity/spawnery-agent.jar` and the image
  layer at `/opt/velocity/agent/spawnery-agent.jar`.

- [ ] **Step 1: Write the `ProxyEnvironment` test first**

```kotlin
class ProxyEnvironmentTest {
    // Uses @TempDir for the agent directory and writes readable ca.crt and
    // token files into it, the way EnvironmentTest already does.

    @Test fun `a complete environment is configured`() {
        // SPAWNERY_OPERATOR_ENDPOINT, SPAWNERY_PLAYER_LIMIT=500,
        // SPAWNERY_FALLBACK_GROUPS="lobby,hub"
        // -> Configured with playerLimit 500 and ["lobby", "hub"]
    }

    @Test fun `a missing endpoint is dormant without mentioning the proxy variables`()
    @Test fun `a missing player limit is dormant and names SPAWNERY_PLAYER_LIMIT`()
    @Test fun `a non-numeric player limit is dormant and names SPAWNERY_PLAYER_LIMIT`()
    @Test fun `a zero player limit is dormant and names SPAWNERY_PLAYER_LIMIT`()
    @Test fun `a missing fallback list is dormant and names SPAWNERY_FALLBACK_GROUPS`()
    @Test fun `an empty fallback list is dormant and names SPAWNERY_FALLBACK_GROUPS`()
    @Test fun `blank entries in the fallback list are dropped, not kept`()
}
```

The last one matters: `"lobby,,hub"` and `"lobby, hub"` both have to yield
`["lobby", "hub"]`, because a group named `""` or `" hub"` would silently match
nothing in the router and the failure would look like "no servers registered".

The dormancy order is deliberate: the endpoint is checked first, so an image run
outside a cluster — `make image-test` — reports the plain "not connecting to an
operator" reason rather than complaining about a player limit nobody set.

- [ ] **Step 2: Write the `ReadyGate` test**

```kotlin
class ReadyGateTest {
    @Test fun `a fresh gate is closed and refuses connections`() {
        // Construct with port 0, do not open. Nothing to connect to; assert
        // isOpen is false.
    }

    @Test fun `open binds and accepts`() {
        val gate = ReadyGate(0) { _, _ -> }
        gate.open()
        Socket("127.0.0.1", gate.boundPort).use { assertTrue(it.isConnected) }
        gate.close()
    }

    @Test fun `open is idempotent and keeps the same port`() {
        val gate = ReadyGate(0) { _, _ -> }
        gate.open()
        val first = gate.boundPort
        gate.open()
        assertEquals(first, gate.boundPort)
        gate.close()
    }

    @Test fun `the gate accepts more than one connection`() {
        // The kubelet probes every 5 s forever; a gate that served one probe
        // and stopped would turn a pod ready and then not-ready again.
        //
        // The count has to exceed the listen backlog, and by enough that it
        // is obvious why. Measured on this machine: with ServerSocket's
        // default listen(50) and no accept() at all, 51 connections complete
        // and the 52nd times out. So `repeat(8)` — the first draft of this
        // plan — passes against a gate that binds and never starts its
        // acceptor, which is the one thing this test exists to catch. Use 64,
        // with an explicit 3000 ms connect timeout to match the pod spec's
        // own timeoutSeconds; without the timeout a broken gate fails only
        // after the kernel's two-minute SYN-retry schedule.
    }

    @Test fun `close releases the port`() {
        // After close(), a fresh ServerSocket can bind the same port.
    }

    @Test fun `a port already in use is reported and leaves the gate closed`() {
        // Hold the port with a plain ServerSocket, then open() the gate on it.
        // Assert the log callback was called and isOpen is false. It must not
        // throw: this runs on a gRPC callback thread.
    }
}
```

Port `0` in the tests and `8081` in production is what makes these tests free of
a fixed port and therefore free of a race with anything else on the machine.

- [ ] **Step 3: Run both, watch them fail**

```bash
nix develop -c bash -c 'cd agent && gradle :velocity:test'
```

Expected: the project does not exist yet. That failure is the signal to do Step
4; the tests are written first so the interfaces are decided by their callers.

- [ ] **Step 4: Create the subproject**

`agent/settings.gradle.kts` gains `include("velocity")`.

`agent/velocity/build.gradle.kts` mirrors `agent/paper/build.gradle.kts`, with
three differences:

- The platform jar is the pinned Velocity jar, not a `libraries` tree. Nix
  symlinks it in as `agent/velocity/velocity.jar` through `postPatch`, the way
  `paper-repo` is symlinked today, and the build declares
  `compileOnly(files("velocity.jar"))`. Record the by-hand equivalent in a
  comment, as `agent/paper/build.gradle.kts` already does for `paper-repo`:

  ```
  ln -sfn "$(nix build .#velocity-jar --no-link --print-out-paths)" agent/velocity/velocity.jar
  ```

- No `exclude` filter is needed on it. Measured on 2026-08-11: the Velocity fat
  jar contains no protobuf, no gRPC, no okhttp/okio and no Kotlin. It does
  contain Netty, Guava, Gson, Guice, Log4j, Adventure and Brigadier. Put that
  measurement and the command that produced it in the comment, because the next
  version bump has to re-run it rather than trust it.
- `base { archivesName = "spawnery-velocity-agent" }`.

The relocation list is copied verbatim from `:paper`. It is longer than
Velocity's own conflict set requires, and that is correct: the list is not what
enforces the rule — `hack/agent-jar-check.sh` is — and a shorter list would have
to be revisited on every dependency change.

- [ ] **Step 5: Write `velocity-plugin.json`**

```json
{
  "id": "spawnery",
  "name": "Spawnery Agent",
  "version": "${version}",
  "description": "Connects this proxy to the Spawnery operator",
  "authors": ["The Spawnery Authors"],
  "dependencies": [],
  "main": "cloud.spawnery.agent.velocity.AgentPlugin"
}
```

Expanded through `processResources` exactly as `paper-plugin.yml` is:

```kotlin
tasks.processResources {
    filesMatching("velocity-plugin.json") {
        expand("version" to project.version)
    }
}
```

The eight fields are measured from
`com.velocitypowered.api.plugin.ap.SerializedPluginDescription`. Velocity ships
an annotation processor that generates this same file to this same place; a
hand-written one avoids adding kapt to a Kotlin build for eight fields. **If the
plugin does not load in Step 9, this is the first suspect** — the fallback is
kapt with the Velocity jar on the `annotationProcessor` configuration, and the
`@Plugin` annotation carrying the same values.

- [ ] **Step 6: Write `AgentPlugin.kt`**

At this stage it does the minimum that makes the image test meaningful: read the
environment, log the dormancy reason, and hold a `ReadyGate` it does not yet
open. Task 7 fills in the session.

```kotlin
@Plugin(
    id = "spawnery",
    name = "Spawnery Agent",
    version = "0.0.0",   // the descriptor in velocity-plugin.json is what
                         // Velocity reads; this value is never consulted
)
class AgentPlugin @Inject constructor(
    private val proxy: ProxyServer,
    private val logger: Logger,
) {
    private var gate: ReadyGate? = null

    @Subscribe
    fun onInitialize(event: ProxyInitializeEvent) {
        when (val env = ProxyEnvironment.from(System::getenv, Path.of(AGENT_DIR))) {
            is ProxyEnvironment.Dormant -> {
                logger.info("spawnery agent dormant: ${env.reason}")
                return
            }
            is ProxyEnvironment.Configured -> {
                gate = ReadyGate(READY_PORT) { message, error -> logger.warn(message, error) }
                logger.info("spawnery agent connecting to ${env.base.endpoint}")
            }
        }
    }

    @Subscribe
    fun onShutdown(event: ProxyShutdownEvent) {
        gate?.close()
    }

    private companion object {
        // internal/podspec.AgentMountPath. Hard-coded rather than configurable:
        // the operator creates these pods and mounts exactly here, and a second
        // place to spell it would be a second place to get it wrong.
        const val AGENT_DIR = "/var/run/spawnery"

        // internal/podspec.ProxyReadyPort, for the same reason.
        const val READY_PORT = 8081
    }
}
```

Task 7 replaces the `Configured` branch's body and adds the two player events;
everything else in this class is final as written. The gate is constructed but
never opened here, so the image test in Step 9 sees a plugin that loads, logs
and does nothing — which is exactly the claim that step makes.

`logger` is `org.slf4j.Logger`, which Velocity bundles and injects.

**The `@Plugin` annotation is inert at runtime, and the comment must say so.**
The first draft of this plan claimed it is "what marks the class Guice
instantiates". It is not: the proxy jar holds exactly three references to
`com/velocitypowered/api/plugin/Plugin` — the annotation, the annotation
processor and `SerializedPluginDescription` — and all three are compile-time
machinery. The descriptor's `main` field is what names the class. Keep the
annotation, because the processor requires it if anyone ever takes the kapt
fallback, and use its `version` line to record why that fallback is a trap:
a processor-generated descriptor is written to the same path from the
annotation, carrying `0.0.0`, no `description` and no `authors`, and both
descriptor guards in `hack/agent-jar-check.sh` would still pass — a `"version":`
key is present and no `${` remains. Every proxy would then report version
`0.0.0` with nothing failing anywhere.

- [ ] **Step 7: Teach `nix/agents.nix` and the image about the second jar**

`nix/agents.nix`: add `velocity` to the argument set for the symlink, extend
`postPatch`:

```bash
ln -sfn ${velocity.jar} velocity/velocity.jar
```

and install and check the second jar:

```bash
install -Dm644 velocity/build/libs/spawnery-velocity-agent-${finalAttrs.version}.jar \
  $out/share/spawnery/velocity/spawnery-agent.jar
```

```bash
bash ${../hack/agent-jar-check.sh} $out/share/spawnery/velocity/spawnery-agent.jar "$PWD" velocity
```

`hack/agent-jar-check.sh` gains a `velocity` flavour. The universal check —
nothing outside `cloud/spawnery/agent/` — is unchanged and unconditional. The
per-flavour part below it enumerates what that platform would actually collide
with, and for `velocity` that is `io.netty`, `com.google.common`,
`com.google.thirdparty`, `com.google.gson`, `com.google.inject`,
`org.apache.logging`, `net.kyori` and `com.mojang.brigadier`. Put the command
that measured the list in the script's comment.

`flake.nix` passes `velocity` into `agents` and `agents` into `velocity-image`.
`nix/velocity-image.nix` gains a layer:

```nix
(runCommand "velocity-agent" { } ''
  install -Dm644 ${agents}/share/spawnery/velocity/spawnery-agent.jar \
    $out/opt/velocity/agent/spawnery-agent.jar
'')
```

placed after `velocityHome` in `contents`, matching the ordering comment already
in that file. `image/velocity-entrypoint.sh` needs no change — it already copies
this exact path when the file exists.

- [ ] **Step 8: Build**

```bash
git add -A agent nix flake.nix hack
nix develop -c make agent-deps   # the Velocity subproject adds no new
                                 # dependencies, but the lockfile records
                                 # per-artifact hashes for the whole build
git add agent/deps.json
nix develop -c make agent
```

- [ ] **Step 9: Extend `hack/velocity-image-test.sh` and run it cold**

Add two assertions, modelled on the ones `hack/image-test.sh` already makes for
the Paper plugin:

- the agent plugin loaded and stayed dormant without an operator — grep the
  container log for the dormancy line naming `SPAWNERY_OPERATOR_ENDPOINT`;
- the plugin's own classes load without a linkage error — the dormancy line is
  itself the proof, because it is printed from the plugin's own code after
  Guice constructed it.

Run it from cold podman storage, so the result is about the image and not about
the cache:

```bash
podman rmi -a -f ; podman system prune -af
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make image-test CONTAINER=podman
```

Expected: both images pass. A plugin that fails to load shows up here as the
absence of the dormancy line, with Velocity's own error above it.

- [ ] **Step 10: Prove reproducibility**

```bash
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make image-repro CONTAINER=podman
nix build .#agents --rebuild
```

Expected: both identical. The `AbstractArchiveTask` flags carry over from
`:paper`; if `:velocity` is missing them, this is where it shows.

- [ ] **Step 11: Commit**

```bash
git add -A
git commit -m "feat(agent): add the Velocity plugin, its environment and its ready gate"
```

---

## Task 5: `ServerDirectory`

**Files:**
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/ProxyRegistry.kt`,
  `…/ServerDirectory.kt`
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/FakeRegistry.kt`,
  `…/ServerDirectoryTest.kt`

**Interfaces:**
- Produces:

  ```kotlin
  /** The three things the agent does to Velocity's server registry. */
  interface ProxyRegistry {
      fun server(name: String): RegisteredServer?
      fun register(info: ServerInfo): RegisteredServer
      fun unregister(info: ServerInfo)
  }

  /** The operator's view of one backend. */
  data class Backend(val name: String, val address: String, val group: String)

  class ServerDirectory(
      private val registry: ProxyRegistry,
      private val log: (String, Throwable?) -> Unit,
  ) {
      fun apply(servers: List<Backend>)      // FullSync
      fun add(backend: Backend)              // RegisterServer
      fun remove(name: String)               // UnregisterServer
      fun inGroup(group: String): List<RegisteredServer>
      fun names(): Set<String>               // what this agent registered
  }
  ```

- Produces: `VelocityRegistry(proxy: ProxyServer) : ProxyRegistry`, the adapter,
  three one-line methods. `proxy.getServer` returns `Optional`; the adapter
  unwraps it to a nullable.

- [ ] **Step 1: Write `FakeRegistry` and a fake `RegisteredServer`**

`FakeRegistry` holds a `LinkedHashMap<String, FakeServer>` keyed by the lower-cased
name and records every `register`/`unregister` call in order, so a test can
assert that an address change produced an unregister *and then* a register
rather than two registers.

`FakeServer` implements `RegisteredServer`. Only `getServerInfo()`,
`getPlayersConnected()`, both `ping()` overloads and `ChannelMessageSink`'s two
`sendPluginMessage` overloads are abstract; everything `Audience` contributes is
a default method. `ping()` and `sendPluginMessage` throw
`UnsupportedOperationException` — the agent never calls them, and a fake that
returns a plausible value for a method under test elsewhere is a trap.
`getPlayersConnected()` returns a settable list so Task 6 can drive the router.

- [ ] **Step 2: Write `ServerDirectoryTest`**

```kotlin
@Test fun `a full sync registers every server it carries`()
@Test fun `a second identical full sync registers nothing again`()
@Test fun `a full sync unregisters a server it no longer carries`()
@Test fun `a full sync leaves a server this agent never registered alone`()
@Test fun `a changed address unregisters and then registers, in that order`()
@Test fun `add registers one server and remove takes it away`()
@Test fun `remove ignores a name this agent never registered`()
@Test fun `inGroup returns only the servers of that group`()
@Test fun `inGroup returns an empty list for an unknown group`()
@Test fun `a malformed address is skipped and the rest of the sync applies`()
@Test fun `an address without a port is skipped and logged, naming the server`()
```

The fourth is the one with a real consequence: `velocity.toml` renders an empty
`servers` table, but a user's `configOverlay` may add entries, and a `FullSync`
must not remove what the agent did not add. Drive it by pre-populating
`FakeRegistry` with a server the directory never saw.

The fifth is what makes the directory independent of a Velocity behaviour this
plan did not measure — what `registerServer` does when the name already exists.
Consulting `server(name)` first means the answer never matters.

The last two: `"10.0.0.1"`, `"10.0.0.1:"`, `"10.0.0.1:notaport"` and `""` are
all skipped with a log line naming the server, and the other entries of the same
sync still apply. One bad entry must not cost a proxy its whole list.

- [ ] **Step 3: Run them and watch them fail**

```bash
nix develop -c bash -c 'cd agent && gradle :velocity:test --tests "*ServerDirectory*"'
```

- [ ] **Step 4: Implement `ServerDirectory`**

Invariants, rather than a transcription:

- It holds `name → Backend` for everything **it** registered, and nothing else.
  `names()` returns those keys; `apply` diffs against them.
- Every registration path consults `registry.server(name)` first: absent →
  register; present with an equal `ServerInfo` → nothing; present with a
  different address → `unregister(existing.serverInfo)` then `register(new)`.
- Addresses parse as `host:port` splitting on the **last** colon, so a
  dual-stack cluster's IPv6 pod IP does not fail. A port outside 1–65535, a
  missing port or an empty host is a skip with a log naming the server, not an
  exception — this runs on a gRPC callback thread.
- The `InetSocketAddress` is built with `createUnresolved(host, port)`. The
  ordinary constructor performs a blocking DNS lookup on the calling thread,
  and the calling thread here is that gRPC callback.
- `inGroup` resolves through `registry.server(name)` rather than caching
  `RegisteredServer` handles, so a server Velocity dropped for its own reasons
  cannot be handed to the router.
- Lookups are case-insensitive on the name, because Velocity's own
  `getServer(String)` is and a `Server` CR name is DNS-1123 lower-case anyway;
  matching Velocity here means the two never disagree about identity.

- [ ] **Step 5: Run them and watch them pass**

```bash
nix develop -c bash -c 'cd agent && gradle :velocity:test'
```

- [ ] **Step 6: Commit**

```bash
git add -A agent
git commit -m "feat(agent): mirror the operator's server list into Velocity"
```

---

## Task 6: `Router`, `Players` and `Drain`

**Files:**
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/Players.kt`,
  `…/Router.kt`, `…/Drain.kt`
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/FakePlayers.kt`,
  `…/RouterTest.kt`, `…/DrainTest.kt`

**Interfaces:**
- Consumes: `ServerDirectory`, `Backend`, `FakeRegistry`, `FakeServer` (Task 5).
- Produces:

  ```kotlin
  /** One connected player, as much of one as the agent needs. */
  interface PlayerRef {
      val username: String
      val currentServer: String?
      fun moveTo(target: RegisteredServer)
  }

  /** The players this proxy is serving. */
  interface Players {
      fun all(): List<PlayerRef>
      fun count(): Int
  }

  class Router(private val directory: ServerDirectory) {
      fun choose(groups: List<String>, excluding: String? = null): RegisteredServer?
  }

  class Drain(
      private val players: Players,
      private val router: Router,
      private val log: (String, Throwable?) -> Unit,
  ) {
      fun run(fromServer: String, toGroups: List<String>)
  }
  ```

- Produces: `VelocityPlayers(proxy: ProxyServer) : Players`, the adapter.
  `PlayerRef.moveTo` calls `player.createConnectionRequest(target).connectWithIndication()`;
  `currentServer` reads `player.currentServer.map { it.server.serverInfo.name }`.

- [ ] **Step 1: Write `RouterTest`**

```kotlin
@Test fun `the first group with a server wins, even if a later one has more`()
@Test fun `an empty group is skipped and the next one is tried`()
@Test fun `within a group the server with the fewest players wins`()
@Test fun `a tie is broken by name, so the choice is deterministic`()
@Test fun `no group with a server yields null`()
@Test fun `an empty group list yields null`()
@Test fun `the excluded server is never chosen even when it is the only one`()
@Test fun `excluding the emptiest server picks the next emptiest`()
```

The first pins the ordering rule against the obvious wrong implementation
("search every group, take the global minimum"): `fallbackGroups` is a *try
list*, so a lobby group with servers always beats a hub group with emptier ones.

Build the fixture through a real `ServerDirectory` over `FakeRegistry`, not
through a mock router input — that composition is what runs in production, and
Task 5's fake already sets player counts.

- [ ] **Step 2: Write `DrainTest`**

```kotlin
@Test fun `every player on the draining server is moved`()
@Test fun `players on other servers are not touched`()
@Test fun `a player is moved to a server chosen from toGroups only`()
@Test fun `the draining server is never the target, even if it is in toGroups`()
@Test fun `with no target available nothing moves and the reason is logged`()
@Test fun `a second identical drain moves nobody, because nobody is left`()
@Test fun `a player whose current server is unknown is not touched`()
```

The last two are the load-bearing ones. The operator repeats `DrainPlayers`
after every periodic `FullSync` — roughly every 30 seconds for as long as the
server drains — and the sixth test is what proves the repetition is free rather
than a move-storm.

**None of the seven pins "once per player", and a test has to.** The first
draft of this plan declared that invariant load-bearing and then listed seven
tests, none of which can catch it: `FakePlayers.moveTo` only records the move,
so no `FakeServer`'s player list changes during a `Drain.run`, so
`Router.choose` is a pure function of state that cannot change mid-loop and
returns the same answer however often it is called. A "compute the target once
and reuse it for every player" implementation therefore produces identical
moves and passes all seven. Add an eighth that measures the call *frequency*
rather than the outcome — count `ProxyRegistry.server` lookups, which
`ServerDirectory.inGroup` makes once per backend per call and never caches, so
three draining players across a two-backend group is six lookups and the
hoisted mutant is two.

`FakePlayers` records each `moveTo` as `(username, targetName)` in order and
lets a test set each player's `currentServer`.

- [ ] **Step 3: Run them and watch them fail**

```bash
nix develop -c bash -c 'cd agent && gradle :velocity:test --tests "*Router*" --tests "*Drain*"'
```

- [ ] **Step 4: Implement `Router` and `Drain`**

`Router.choose` walks `groups` in order; for each it takes
`directory.inGroup(group)`, drops the excluded name if any, and returns the
element minimising `(getPlayersConnected().size, serverInfo.name)`. The first
group producing a non-empty candidate list wins; later groups are not consulted.

`Drain.run` filters `players.all()` to those whose `currentServer` equals
`fromServer` (case-insensitively, matching the directory), asks
`router.choose(toGroups, excluding = fromServer)` **once per player** rather than
once per drain — so ten players on a draining server spread across the targets
instead of piling onto whichever was emptiest when the message arrived — and
calls `moveTo`. A null choice logs once per drain, not once per player.

Exceptions out of `moveTo` are caught and logged. `connectWithIndication`
returns a `CompletableFuture`; the agent does not wait on it, because waiting
would block a gRPC callback thread on a network round trip to a backend.

- [ ] **Step 5: Run them and watch them pass, then the whole suite**

```bash
nix develop -c bash -c 'cd agent && gradle :velocity:test'
```

- [ ] **Step 6: Commit**

```bash
git add -A agent
git commit -m "feat(agent): choose a server on join and move players on drain"
```

---

## Task 7: `ProxyState`, `ProxyRole` and the plugin wiring

The task that makes the pieces move. At the end of it the plugin connects,
reports, opens its gate and routes — proven by unit tests here and on the wire
in Task 8.

**Files:**
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/ProxyState.kt`,
  `…/ProxyRole.kt`
- Modify: `…/AgentPlugin.kt`, `…/Players.kt` and `…/ProxyRegistry.kt` (the
  adapters)
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/ProxyStateTest.kt`,
  `…/ProxyRoleTest.kt`

**Interfaces:**
- Consumes: `AgentRole`, `Directive`, `SessionLoop` (Task 3);
  `ServerDirectory`, `Backend` (Task 5); `Router`, `Drain`, `Players` (Task 6);
  `ProxyEnvironment`, `ReadyGate` (Task 4).
- Produces:

  ```kotlin
  class ProxyState(val slots: Int) {
      val players: Int
      fun sample(players: Int)
  }

  class ProxyRole(
      private val state: ProxyState,
      private val directory: ServerDirectory,
      private val drain: Drain,
      private val onFirstSync: () -> Unit,
      private val log: (String, Throwable?) -> Unit,
  ) : AgentRole<ProxyMessage, OperatorToProxy>
  ```

- [ ] **Step 1: Write `ProxyRoleTest`**

```kotlin
@Test fun `hello carries the version and leaves ready unset`()
@Test fun `the report carries the sampled players and the configured slots`()
@Test fun `a report interval message yields a Report directive`()
@Test fun `a session deadline message yields a Deadline directive`()
@Test fun `a full sync applies to the directory`()
@Test fun `the first full sync opens the gate`()
@Test fun `a second full sync does not open the gate again`()
@Test fun `a register and an unregister reach the directory`()
@Test fun `a drain message reaches the drain`()
@Test fun `an unrecognised message yields None and touches nothing`()
```

`hello` leaving `ready` unset is not cosmetic: `ProxyMessage` carries no
readiness at all, `agentserver.handleProxy` says so in its own comment, and a
`true` there would look like a second, contradicting source for something the
kubelet owns.

`onFirstSync` is a lambda in the test, counting calls. In production it is
`gate::open`, which is itself idempotent — so the "does not open again" test
pins the role's own once-only behaviour rather than leaning on the gate's.

- [ ] **Step 2: Write `ProxyStateTest`**

```kotlin
@Test fun `slots is what it was constructed with and never changes`()
@Test fun `players starts at zero and reads back what was sampled`()
```

Two tests for six lines is right: `slots` never moving is the property the
registry depends on, and it is worth one assertion.

- [ ] **Step 3: Run them and watch them fail**

```bash
nix develop -c bash -c 'cd agent && gradle :velocity:test --tests "*ProxyRole*" --tests "*ProxyState*"'
```

- [ ] **Step 4: Implement `ProxyState` and `ProxyRole`**

`ProxyState` is an `AtomicInteger` behind `players` and a `val slots`. No
`markReady` equivalent: a proxy's readiness is the socket, not a flag.

`ProxyRole.open` calls
`AgentServiceGrpc.newStub(channel).withCallCredentials(credentials).proxySession(observer)`.

`ProxyRole.onMessage` maps:

| `OperatorToProxy.MessageCase` | Effect | Directive |
|---|---|---|
| `FULL_SYNC` | `directory.apply(...)`, then `onFirstSync()` on the first one only | `None` |
| `REGISTER_SERVER` | `directory.add(...)` | `None` |
| `UNREGISTER_SERVER` | `directory.remove(name)` | `None` |
| `DRAIN_PLAYERS` | `drain.run(fromServer, toGroupsList)` | `None` |
| `REPORT_INTERVAL` | — | `Report(seconds)` |
| `SESSION_DEADLINE` | — | `Deadline(renewAfter, hardDeadline)` |
| anything else | — | `None` |

The proto's `RegisteredServer` maps to `Backend(name, address, group)` in one
private helper used by both `FULL_SYNC` and `REGISTER_SERVER`.

The whole body runs inside a `runCatching` that logs: this is called from a gRPC
callback thread, and an exception escaping it would take down the stream for a
malformed message rather than skipping it.

- [ ] **Step 5: Wire `AgentPlugin`**

On `ProxyInitializeEvent`, in this order:

1. `ProxyEnvironment.from(System::getenv, Path.of(AGENT_DIR))`. `Dormant` → log
   the reason and return, registering no events and starting no threads.
2. Build `ReadyGate(READY_PORT, log)`, `VelocityRegistry(proxy)`,
   `ServerDirectory`, `VelocityPlayers(proxy)`, `Router`, `Drain`,
   `ProxyState(env.playerLimit)`, `ProxyRole`.
3. Start a repeating task on `proxy.scheduler` that calls
   `state.sample(players.count())` every second — through the `Players` seam,
   not through `proxy.playerCount` directly, so the sampling path is the one
   the tests can drive. Velocity's API is widely held to be thread-safe, and
   running this on Velocity's own scheduler sidesteps the question entirely
   rather than resting on it: the reporting timer then only ever reads an
   atomic.
4. Sample once immediately, before the loop starts. The operator's
   `ReportInterval` schedules its first report at delay zero, so without this
   the first `PlayerCount` of the process carries a zero the counters were
   constructed with. This is the same fix Paper's `AgentPlugin` already carries,
   with the same comment.
5. Construct `SessionLoop(channels = { OperatorChannel.build(env.base.endpoint,
   Files.readAllBytes(env.base.caBundlePath)) }, credentials =
   BearerCredentials.of(TokenSource(env.base.tokenPath)), role = role, …)` and
   `start()` it. The bundle is read per attempt, not once — the kubelet replaces
   both files in place.
6. Nothing. **Do not call `proxy.eventManager.register(this, this)`** — the
   first draft of this plan said to, and it throws. Measured out of the pinned
   3.5.1-615 jar: `VelocityEventManager.register` compares its two arguments by
   identity and raises `IllegalArgumentException("The plugin main instance is
   automatically registered.")`, because `VelocityServer` already calls
   `registerInternally` for every plugin's own instance — which is how
   `onInitialize` was being delivered in the first place. The `@Subscribe`
   methods on the plugin class are live from plugin load and need no
   registration.

   That has a consequence step 1 assumed away: a dormant agent cannot register
   *no* events, because the handlers exist before `ProxyEnvironment` is ever
   consulted. Make them inert with null guards on the fields they touch, and
   keep the observable behaviour — a dormant agent does nothing — the same.

`PlayerChooseInitialServerEvent`: `router.choose(env.fallbackGroups)`; non-null →
`event.setInitialServer(it)`; null → log at WARNING naming the groups searched,
and set nothing, so Velocity disconnects with its own message.

`ServerConnectedEvent`: `loop.send(ProxyMessage.newBuilder().setPlayerJoinedServer(...))`.

On `ProxyShutdownEvent`: stop the loop, close the gate, shut the sampling task
down.

**The agent never sends `Heartbeat`.** The message exists and the operator has a
branch for it that deliberately does nothing — the stream is its own liveness
signal. Put that sentence in a comment where a reader would otherwise wonder.

- [ ] **Step 6: Run the whole agent build**

```bash
git add -A agent
nix develop -c make agent
```

- [ ] **Step 7: Commit**

```bash
git add -A agent
git commit -m "feat(agent): connect the Velocity proxy to the operator"
```

---

## Task 8: `ProxySession` in the stub operator, and two proxy phases

The level that milestone 2c's five defects taught this project to trust. A unit
test cannot see a TLS handshake, a real classloader, or a port that is or is not
bound inside a container.

**Files:**
- Modify: `cmd/spawnery-stubop/main.go`, `hack/agent-test.sh`, `Makefile`

**Interfaces:**
- Consumes: the Velocity image with the agent baked in (Task 4), a plugin that
  connects (Task 7).
- Produces: `spawnery-stubop --proxy`, serving `ProxySession` and sending one
  `FullSync` after the `Hello`, with the same JSON event trace the server side
  already emits.

- [ ] **Step 1: Read how the stub records, before changing it**

`cmd/spawnery-stubop/main.go`'s `stub.ServerSession` and `stub.observe`, and
`hack/agent-test.sh`'s `await_event` and `explain_silence`. The proxy side
reuses all four; what changes is which rpc and which messages.

- [ ] **Step 2: Implement `stub.ProxySession`**

Structurally the same as `ServerSession`: `enter` under `--supersede`, the same
`ReportInterval` and `SessionDeadline` sends, the same receive goroutine, the
same `observe` calls emitting `stream_opened`, `hello`, `player_count` and
`stream_closed` events. Two additions:

- After the two opening sends, and only under `--proxy`, send one
  `FullSync{servers: [{name: "lobby-0", address: "10.255.255.1:25565", group:
  "lobby"}]}`. The address is deliberately unroutable: nothing in this harness
  is supposed to connect to it, and a reachable one would invite a later test to
  depend on a backend that is not there.
- Emit the `full_sync_sent` event **after** the send returns, so the script can
  order its port assertions against it.

`--proxy` gates only the `FullSync`; the stub serves both rpcs unconditionally,
because refusing one would make a wrong-rpc bug in the agent look like a
connection failure rather than naming itself.

- [ ] **Step 3: Add the two phases to `hack/agent-test.sh`**

The script currently runs three phases against `$IMAGE`. Add `VELOCITY_IMAGE` as
a second input and two phases against it.

**Phase 4 — passive plus ready gate.** Start the stub with `--proxy` but hold
the `FullSync` until the script says so; the simplest form of "hold" is a second
flag, `--full-sync-after <seconds>`, defaulting to zero. Then:

1. Start the Velocity container with the same rendered `/etc/spawnery` fixture
   `hack/velocity-image-test.sh` builds, plus the CA, token and endpoint the
   Paper phases already mount, plus `SPAWNERY_PLAYER_LIMIT=100` and
   `SPAWNERY_FALLBACK_GROUPS=lobby`.
2. Wait for `hello` in the trace.
3. **Assert 8081 is closed.** Check from *inside* the container:

   ```bash
   "$CONTAINER" exec "$name" bash -c 'exec 3<>/dev/tcp/127.0.0.1/8081' 2>/dev/null
   ```

   and require a non-zero exit. Not through a published port: a rootless podman
   port forwarder may complete the host-side handshake before anything inside
   listens, which would make this assertion pass for a reason that has nothing
   to do with the agent. Bash's `/dev/tcp` is a builtin, and bash is already in
   the image because the entrypoint's shebang points at it.
4. Let the `FullSync` go out; wait for `full_sync_sent`.
5. **Assert 8081 is open**, same command, requiring exit zero, with a short
   retry loop — the gate binds from a gRPC callback thread and the script is
   racing it.
6. Assert the trace holds a `player_count` with `slots: 100`. That number is the
   whole reason `SPAWNERY_PLAYER_LIMIT` is load-bearing: the registry discards
   any report where `players > slots`.

Assertion 3 is what makes assertion 5 mean anything, and it is also the one that
would catch a container runtime accepting on the script's behalf.

**Phase 5 — supersede.** The stub's existing `--supersede` behaviour against the
Velocity container, with the same two-sided bound on the stream-open rate the
Paper phase uses. The mute phase is deliberately **not** repeated: it exercises
loop internals only, and from Task 3 the loop is shared code that the Paper
phase still drives. Supersede is repeated even though the same argument would
excuse it, because milestone 2c's lesson is that the assumptions hide in the
harness.

- [ ] **Step 4: Teach the Makefile to load both images**

```make
agent-test: image-load velocity-image-load
	CONTAINER=$(CONTAINER) IMAGE=$(IMAGE) VELOCITY_IMAGE=$(VELOCITY_IMAGE) \
		STUBOP=$(STUBOP) hack/agent-test.sh
```

- [ ] **Step 5: Run it**

```bash
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make agent-test CONTAINER=podman
```

Expected: five phases pass. A silent proxy container is the failure mode to
expect first; `explain_silence` already prints the container log, and a dormancy
line naming a variable is the answer.

- [ ] **Step 6: Prove the closed-state assertion can fail**

Temporarily change the agent to open the gate on `ProxyInitializeEvent` instead
of on the first `FullSync`, re-run phase 4, and confirm assertion 3 fails.
Revert. A gate assertion that cannot fail is the shape of defect this project
has now found twice, and five minutes here is cheaper than finding it in a
cluster.

- [ ] **Step 7: Commit**

```bash
git add cmd/spawnery-stubop hack Makefile
git commit -m "test(agent): prove the ready gate waits for a server list"
```

---

## Task 9: Extract `internal/mcproto`

`internal/slp` already speaks VarInts, length-prefixed strings and packet
framing, all unexported. The join client needs the same four functions, and a
second copy of packet framing is not acceptable.

**Files:**
- Create: `internal/mcproto/mcproto.go`, `internal/mcproto/mcproto_test.go`
- Modify: `internal/slp/slp.go`, `internal/slp/slp_test.go`

**Interfaces:**
- Produces:

  ```go
  package mcproto

  func AppendVarInt(b []byte, v int32) []byte
  func AppendString(b []byte, s string) []byte
  func ReadVarInt(r io.ByteReader) (int32, error)
  func WritePacket(w io.Writer, id int32, payload []byte) error
  func ReadPacket(r io.Reader) (id int32, payload []byte, err error)
  func ByteReader(r io.Reader) io.ByteReader
  ```

  `ReadPacket` and `ByteReader` are new; the other four move.

- [ ] **Step 1: Move the four functions**

`appendVarInt`, `appendString`, `readVarInt`, `writePacket` and the
`singleByteReader` type move to `internal/mcproto`, exported, with the Apache
header from `hack/boilerplate.go.txt` and their existing comments. `singleByteReader`
becomes unexported behind `ByteReader(r io.Reader) io.ByteReader`.

- [ ] **Step 2: Add `ReadPacket`**

```go
// ReadPacket reads one uncompressed packet: a VarInt length, then a VarInt
// packet id, then the rest. The length bounds the read, so a server that
// announces more than it sends fails here rather than blocking forever.
func ReadPacket(r io.Reader) (int32, []byte, error)
```

Cap the announced length at 2 MiB and return an error above it — a hostile or
confused peer must not be able to make this allocate without bound.

- [ ] **Step 3: Point `internal/slp` at it**

`slp.go` keeps `handshakeProtocolVersion`, `Ping`, `Status`, `Version`,
`writeHandshake` and `readStatusResponse`, and calls `mcproto` for the rest.
`readStatusResponse` can now be expressed through `ReadPacket`; do that only if
it comes out shorter and clearer, and leave it alone otherwise. This task must
not change `slp`'s behaviour.

- [ ] **Step 4: Write `mcproto_test.go`**

Move the VarInt and string cases out of `slp_test.go` and add round-trip cases
for `ReadPacket`:

```go
func TestVarIntRoundTrip(t *testing.T)          // 0, 1, 127, 128, 255, 2097151,
                                                 // 2147483647, -1
func TestReadVarIntRejectsAnOverlongEncoding(t *testing.T)
func TestPacketRoundTrip(t *testing.T)
func TestReadPacketRejectsAnAbsurdLength(t *testing.T)
func TestReadPacketFailsOnATruncatedBody(t *testing.T)
```

`-1` matters: VarInts are zigzag-free two's complement here, so a negative value
encodes as five bytes and a naive loop drops it.

- [ ] **Step 5: Run both packages**

```bash
nix develop -c go test ./internal/slp/ ./internal/mcproto/
```

Expected: PASS, with `slp`'s existing tests unchanged in content.

- [ ] **Step 6: Commit**

```bash
git add internal/slp internal/mcproto
git commit -m "refactor(slp): extract the packet framing into internal/mcproto"
```

---

## Task 10: `internal/mcjoin` and `cmd/spawnery-join`

The automated half of the milestone's success criterion. This task begins with a
measurement, because the design does not know the answer and guessing it is how
the last two milestones produced their defects.

**Files:**
- Create: `internal/mcjoin/mcjoin.go`, `internal/mcjoin/mcjoin_test.go`,
  `cmd/spawnery-join/main.go`
- Modify: `flake.nix` (a `spawnery-join` package, alongside `spawnery-stubop`,
  which is likewise test-only and not in any image)

**Interfaces:**
- Consumes: `mcproto` (Task 9), `slp.Ping` for the protocol version.
- Produces:

  ```go
  package mcjoin

  type Result struct {
      Protocol  int
      Username  string
      UUID      string
      Compressed bool
  }

  // Join connects, logs in as username against an offline-mode server, and
  // returns once the server has acknowledged the login.
  func Join(ctx context.Context, host string, port int, username string) (*Result, error)
  ```

- [ ] **Step 1: Measure how far a login has to get**

Before writing the client, build a rig that needs no cluster:

1. Render a `/etc/spawnery` fixture the way `hack/velocity-image-test.sh` does,
   with a `configOverlay` that sets `online-mode = false` and a `servers` table
   holding one entry pointing at an address nothing answers.
2. Run the Velocity image with it, no operator endpoint, so the agent stays
   dormant and the static `servers` entry is the only routing there is.
3. Drive it with the simplest possible client — handshake, login start — and
   read the container log.

The question: does Velocity attempt the backend connection after login alone, or
does it wait for the client to finish the configuration phase? The answer decides
whether `Join` stops after `Login Acknowledged` or has to speak
`FinishConfiguration`/`AcknowledgeFinishConfiguration` too.

Write the answer, and the log lines it rests on, into `internal/mcjoin`'s package
comment. It is the one thing about this task that no one can re-derive from the
code.

- [ ] **Step 2: Write the fake-server test first**

`mcjoin_test.go` stands up a `net.Listener` that speaks the server half:

```go
func TestJoinCompletesAnOfflineLogin(t *testing.T)
func TestJoinSendsTheProtocolVersionItWasGiven(t *testing.T)
func TestJoinHandlesSetCompression(t *testing.T)
func TestJoinReportsADisconnectReason(t *testing.T)
func TestJoinFailsCleanlyOnAClosedConnection(t *testing.T)
func TestJoinRespectsAContextDeadline(t *testing.T)
```

The compression test is not optional: Velocity's default
`compression-threshold` is 256, so a real server sends `Set Compression` during
login and every packet after it carries an extra data-length VarInt. A client
that ignores it desynchronises on the very next packet and the symptom is a
nonsense packet id.

The disconnect test pins the useful failure: an online-mode server answers a
login with `Disconnect`, and the tool has to print that reason rather than
"unexpected packet 0x00".

- [ ] **Step 3: Run them and watch them fail**

```bash
nix develop -c go test ./internal/mcjoin/
```

- [ ] **Step 4: Implement `Join`**

The sequence:

1. `slp.Ping` to learn `version.protocol`. Asking the server removes the one
   constant this client would otherwise have to guess and keep in step with a
   Paper bump — and `internal/slp`'s own comment already records that a server
   answers a handshake announcing a different version than it speaks.
2. Handshake with that protocol version, the host and port as dialled, and
   `nextState = 2`.
3. `Login Start`: the username, then the offline-mode UUID, which is the MD5
   name-based UUID of `OfflinePlayer:<username>` with version 3 — the same one
   Velocity and Paper compute, so the client does not present an identity the
   proxy would reject.
4. Read packets until `Login Success`, handling `Set Compression` by switching
   the framing, and `Disconnect` by returning its JSON reason as an error.
5. Send `Login Acknowledged`, and whatever more Step 1 measured to be necessary.

Compressed framing is: length VarInt, data-length VarInt, then either the raw
packet when data-length is zero or a zlib stream of it. `compress/zlib` is in
the standard library.

`cmd/spawnery-join` is a thin main: `--host`, `--port` (default 25565),
`--username` (default `spawnery-probe`), `--timeout` (default 30 s). It prints
the `Result` as one JSON line so a script can assert on it, and exits non-zero
with the reason on failure.

- [ ] **Step 5: Run the tests, then the rig from Step 1 again**

```bash
nix develop -c go test ./internal/mcjoin/
nix develop -c go build ./cmd/spawnery-join
```

Then point the built binary at the Velocity container from Step 1 and confirm
the log shows what Step 1 measured.

- [ ] **Step 6: Commit**

```bash
git add internal/mcjoin cmd/spawnery-join flake.nix
git commit -m "feat(mcjoin): log in far enough to be routed to a backend"
```

---

## Task 11: The evidence runbook and the documentation

**Files:**
- Create: `docs/runbook-milestone-3-evidence.md`, `docs/handover-milestone-4.md`
- Modify: `README.md`, `docs/known-issues.md`

- [ ] **Step 1: Write the evidence runbook**

Every command, in order, from an empty machine to the two proofs. It covers:

- creating the kind cluster the way `README.md` documents, under
  `systemd-run --scope --user --property=Delegate=yes`;
- loading both images;
- running the operator outside the cluster through `go run`, and hand-building
  the `Service` and `Endpoints` its own pods dial — with a pointer to the
  known-issues entry that explains why, so the next person does not think it is
  a mistake;
- the manifests: a `Network`, a forwarding-secret `Secret`, a `ServerGroup`
  named `lobby` with one replica, and a `ProxyGroup` with
  `routing.fallbackGroups: ["lobby"]`, NodePort expose, and
  `spec.config.onlineMode: false`;
- the automated proof: `spawnery-join --host <nodeIP> --port <nodePort>`
  against that group, then `kubectl get proxygroup -o jsonpath` for
  `status.connectedPlayers`, then the proxy and backend logs;
- the manual proof: the same network without the `online-mode` overlay, joined
  from a real client with a Microsoft account;
- the drain proof: `kubectl delete server` with a player connected, and the
  proxy log line showing the move.

Each step states what to expect, so a deviation is recognisable as one.

**Four things the runbook has to get right that this plan originally got
wrong**, all established by tasks 10 and 10b:

- `online-mode` is turned off through `spec.config.onlineMode: false` on the
  `ProxyGroup`, not through a `configOverlay`. The renderer reasserts the four
  keys it owns after merging an overlay, so an overlay never could have
  reached it; the field exists because the automated proof needs it and
  because a security switch belongs on the custom resource.
- The probe's username is `spawnery_probe`. A hyphen is not a legal Minecraft
  username: Velocity accepts it and Paper then kills the forwarded connection.
- `spawnery-join` closes its connection when it returns, so the
  `status.connectedPlayers` assertion needs `--hold`.
- Whether a held connection is *counted* is unmeasured. It sits in the
  configuration state, and the rig that measured the routing had no operator.
  If the count reads zero, the fix is to play the configuration phase through
  — two packet-id constants and one `case` in `holdOpen`, not a rewrite. Write
  the runbook so that outcome is a recognisable branch rather than a puzzle.

  *That estimate was measured on 2026-08-25 and is wrong: the client has to
  drive the exchange rather than answer one packet, and it is four constants
  and a small state machine. `docs/known-issues.md`, under the milestone 3c
  evidence run, carries what the wire actually does.*

- [ ] **Step 2: Perform the runbook and record what happened**

This step is executed by the session controller together with the human partner,
not by a task subagent: it needs a cluster on the human's machine and, for the
manual join, their Microsoft account. Paste the real output — timestamps,
`status` fields, log lines — into `docs/handover-milestone-4.md`, the way
`docs/handover-milestone-3.md` records milestone 2c's measured `Ready` timing.

If something fails here, it is a defect in an earlier task, not a documentation
problem. Fix it there.

- [ ] **Step 3: Update `docs/known-issues.md`**

Add a "From milestone 3c" section carrying §11 of the design — the ready port
spelled in two languages, the silent CR on a failed bind, the third spelling of
the fallback list, per-proxy load balancing, the lowerable readiness milestone 4
needs, and the NetworkPolicy that is now overdue rather than deferred. Mark the
milestone-3 preconditions section as fully discharged.

Add whatever the implementation actually discovered. That is the part of this
step with real value; the list above is only the part that is already known.

- [ ] **Step 4: Update `README.md`**

A "Milestone 3c is done" paragraph in the Status section, in the same register
as the others: what now works, measured, and what is still missing. Then replace
the pointer to `docs/handover-milestone-3.md` with one to
`docs/handover-milestone-4.md`, keeping the older ones listed as predecessors.

- [ ] **Step 5: Write `docs/handover-milestone-4.md`**

What milestone 4 — scaling and drain — finds in place, what 3c leaves open, and
the one contract change it will have to make: `internal/agent/registry.go`
cannot express "connected, but no longer ready", and proxy drain needs exactly
that. Include the evidence from Step 2.

- [ ] **Step 6: Run every gate one last time**

```bash
nix develop -c make test
nix develop -c make agent
podman rmi -a -f ; podman system prune -af
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make image-test CONTAINER=podman
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make agent-test CONTAINER=podman
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make image-repro CONTAINER=podman
```

From cold podman storage, so the result is about the images rather than about
the cache.

- [ ] **Step 7: Commit**

```bash
git add docs README.md
git commit -m "docs(3c): record what the Velocity agent closed and what it opens"
```

---

## Facts this plan asserts about foreign systems

Carried from §8 of the design, plus what the plan adds. Measured facts name the
command; unmeasured ones name the task that measures them.

**Measured on 2026-08-11** with
`JAR=$(nix build .#velocity-jar --no-link --print-out-paths)`:

- The Velocity fat jar carries the plugin API (205 classes) at class-file major
  65, so `javac` 21 reads it and no Maven artifact is needed — `jar tf`, then
  `od -An -tu1 -j7 -N1` on an extracted class.
- It bundles Netty, Guava, Gson, Guice, Log4j, Adventure and Brigadier, and
  bundles no protobuf, gRPC, okhttp/okio or Kotlin — `jar tf` by package root.
- `ProxyServer`, `RegisteredServer`, `ServerInfo`, `Player`,
  `ConnectionRequestBuilder` and `PlayerChooseInitialServerEvent` have the
  signatures Tasks 5 to 7 call — `javap -cp "$JAR"`.
- The plugin descriptor has eight fields — `javap -p` on
  `SerializedPluginDescription`.
- `Server.status.address` is `podIP:25565` —
  `internal/controller/server_controller.go:575`.
- `routing.fallbackGroups` is required with `MinItems=1` —
  `api/v1alpha1/proxygroup_types.go`.
- `ProxyMessage` carries no readiness and the operator's own comment says so —
  `internal/agentserver/server.go`.

**Not measured, and designed around rather than relied on:**

- *What Velocity does on a duplicate `registerServer`.* Task 5 consults
  `server(name)` first, so it never matters.
- *That a hand-written `velocity-plugin.json` is loaded.* Task 4 Step 9 proves
  it by loading the plugin in the real image; the fallback is named in Task 4
  Step 5.
- *That Velocity's API is safe to call from any thread.* Task 7 Step 5 samples
  on Velocity's own scheduler and sidesteps it.
- *That a rootless podman port forwarder does not accept before the container
  does.* Task 8 checks from inside the container instead, and Step 6 proves the
  closed-state assertion can fail.
- *How far a login must get before Velocity connects the player to a backend.*
  Task 10 Step 1 measures it before any client code is written.
