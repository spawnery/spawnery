# Milestone 7b-5 — requests, bounds, and `connect`

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An agent can *ask* the operator for something, bounded, and the first thing it asks for is moving a player. Until now this channel has only ever reported upward and instructed downward.

**Architecture:** `CloudRequest` up, `CloudResponse` down, correlated by an id the agent mints. The operator answers with the same bounds shape the connection limiter already uses — the pod's own namespace, a ceiling, and a rate. `connect` is the first and only verb, so that the request machinery is proven by something small before `scale` and `retire` are built on it.

**Tech Stack:** Kotlin (both agents, `agent/common`), Go (operator), protobuf.

**Spec:** [`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../specs/2026-08-27-cloud-api-design.md) §3.1, §4.1, §4.3, §9.1

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
- `make agent-test` needs `CONTAINER=podman` and a `TMPDIR` that is not a small
  tmpfs.
- No Kotlin file under `agent/` carries a licence header.
- A new constructor parameter goes last.

## Two things settled before this plan was written

**Both platforms speak `com.mojang.brigadier`.** Measured on 2026-08-28 against
the pinned artifacts: Paper ships `brigadier-1.3.10.jar` with 54
`com.mojang.brigadier` classes as a library, plus 45 classes of its own wrapper
under `io/papermc/paper/command/brigadier`; the pinned Velocity jar carries 97.
The spec has carried this as an explicit **unmeasured assumption** since it was
written, and it holds. It matters to 7c and not to this plan, and is recorded
here because this is where it was measured.

**`/cloud` is 7c's, not this plan's.** 7b-4's closing note said `/cloud list`
would close its own "nothing proves this loads in a real image" gap. That was
wrong about which milestone owns the command, and it was also the expensive way
to close the gap: `hack/agent-test.sh` starts its containers detached with no
stdin, so there is no console to type a command into. Task 1 closes it for one
log line instead.

## File structure

| Path | Responsibility |
|---|---|
| `agent/{paper,velocity}/.../AgentPlugin.kt` | One line at enable naming what was installed |
| `hack/agent-test.sh` | Asserts that line, in both images |
| `proto/.../agent.proto` | `CloudRequest`, `CloudResponse`, `ConnectRequest`, `ConnectResult` |
| `agent/common/.../Requests.kt` | Correlation, the deadline, and what a renewal does to one |
| `internal/agentserver/requests.go` | The operator's side, and its bounds |
| `internal/agentserver/metrics.go` | A refusal counter, labelled by reason |
| `agent/common/.../MirrorApi.kt` | `connect` stops throwing `UnsupportedOperation` |
| `docs/network-boundaries.md` | What an agent may now ask for |

---

### Task 1: The install is observable

7b-4 built an API and could not show that it loads. One log line closes it, and
`agent-test.sh` already reads container logs.

**Files:**
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt`
- Modify: `hack/agent-test.sh`

- [ ] **Step 1: Log it, on both**

Directly after `Spawnery.install(...)`, at info level:

```kotlin
logger.info("spawnery API installed for network ${self.network()} group ${self.group()}")
```

Use each plugin's own logger — Paper's `logger`, Velocity's injected `Logger`.
Hoist the `Self` into a local first, so the message and the API are built from
one object rather than two constructions that could drift.

**Info and not debug.** This line is how a server owner confirms the API is
available to their plugins, which is a question they will otherwise ask by
installing a plugin and watching it throw.

- [ ] **Step 2: Assert it in both phases**

In the Paper phase and the Velocity phase, after the agent has connected:

```bash
if ! "$CONTAINER" logs "$NAME" 2>&1 | grep -q 'spawnery API installed'; then
	echo "the agent never installed its plugin API" >&2
	echo "every test of that API is a JUnit test against a fake; this line is the only thing that says the install path runs inside the shipped jar" >&2
	"$CONTAINER" logs "$NAME" 2>&1 | grep -i spawnery >&2 || true
	exit 1
fi
echo "the plugin API is installed and available to other plugins"
```

- [ ] **Step 3: Run it**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make agent-test CONTAINER=podman`

Expected: green, with the new line in both phases.

- [ ] **Step 4: Commit**

```bash
git add agent hack/agent-test.sh
git commit -m "$(cat <<'EOF'
test(agent): the plugin API is observed loading in both real images

7b-4 built an API and named its own gap: every test of it is JUnit against a
fake, and nothing showed that Spawnery.install actually runs inside a shipped
jar on a real platform. Installing leaves no trace on the wire, so the stub
cannot see it either.

One log line closes it, and it is worth having on its own account: a server
owner confirming the API is available to their plugins would otherwise find
out by installing one and watching it throw.

7b-4's closing note said /cloud list would close this instead. That was wrong
twice -- the command belongs to 7c, and agent-test starts its containers
detached with no stdin, so there is no console to type into.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 2: A request and an answer, on the wire

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Regenerate via `make proto`

**Interfaces:**
- Produces, on both agent-to-operator messages and both operator-to-agent ones:
  ```proto
  message CloudRequest  { uint64 id = 1; oneof request { ConnectRequest connect = 2; } }
  message CloudResponse { uint64 id = 1; oneof result { ConnectResult connect = 2; RequestError error = 3; } }
  message ConnectRequest { string player_uuid = 1; string target = 2; }
  message ConnectResult  { bool moved = 1; string server = 2; }
  message RequestError   { Reason reason = 1; string message = 2;
                           enum Reason { REASON_UNSPECIFIED = 0; NOT_FOUND = 1; REFUSED = 2;
                                         RATE_LIMITED = 3; UNAVAILABLE = 4; } }
  ```

- [ ] **Step 1: Add the messages**

Write them with the comments that carry the decisions:

- **The id is the agent's**, minted per stream and monotonic. The operator
  echoes it and never remembers one across a reconnect, so there is nothing to
  reconcile after a renewal.
- **`RequestError.Reason` is an enum and the message is free text.** A caller
  branches on the reason; a person reads the message. Making the reason a
  string would have every caller matching on prose the operator is free to
  reword.
- **`REASON_UNSPECIFIED` at zero** for the reason `GroupState.Kind` has one: it
  is both what a newer operator sends for a reason this agent predates and what
  proto3 gives an older one, and reading either as "something went wrong and I
  do not know what" is the only reading that cannot be wrong.
- **No `scale` or `retire` yet.** `connect` alone, so the machinery is proven
  by the verb that touches no cluster state at all. §4.3's bounds are built
  here and the two mutating verbs land on them in 7c.

- [ ] **Step 2: Regenerate and build both sides**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make proto`

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go build ./...`

```bash
git add -A
```

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make agent`

- [ ] **Step 3: Commit**

```bash
git commit -m "$(cat <<'EOF'
feat(proto): the channel carries a request, for the first time

Until now an agent reports and the operator instructs; nothing asks. The id
is the agent's, minted per stream and monotonic, and the operator echoes it
without remembering one across a reconnect -- so a renewal leaves nothing to
reconcile.

RequestError separates a Reason a caller branches on from a message a person
reads. A reason as free text would have every caller matching on prose the
operator is free to reword, which is a compatibility break nobody would see
coming.

connect alone, and no scale or retire. The machinery is proven by the verb
that touches no cluster state, and the two that do land on the bounds this
milestone builds.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 3: The agent side — correlation, a deadline, and what a renewal does

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/Requests.kt`
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/RequestsTest.kt`

**Interfaces:**
- Produces:
  ```kotlin
  class Requests<Req, Resp>(
      private val send: (Long, Req) -> Unit,   // builds and sends, given an id
      private val timeout: Duration,
      private val clock: () -> Long,
  ) {
      fun <T> start(build: (Long) -> Req): CompletableFuture<T>
      fun complete(id: Long, value: Any)
      fun fail(id: Long, error: Throwable)
      fun failAll(error: Throwable)   // called when a stream is displaced
      fun expire()                    // called on the reporting tick
  }
  ```

- [ ] **Step 1: Write the failing tests**

```kotlin
class RequestsTest {
    // A clock the test moves by hand, so a deadline is asserted rather than
    // waited out -- a test that slept for its own timeout would be the slowest
    // one in the module and would still be racy on a loaded machine.
    private var now = 0L
    private val sent = mutableListOf<Long>()
    private fun requests(timeoutMillis: Long = 1_000) =
        Requests<Long, Long>(send = { id, _ -> sent += id }, timeout = timeoutMillis, clock = { now })

    @Test
    fun `an answer completes the future that asked`() {
        val r = requests()
        val pending = r.start<String> { id -> id }

        r.complete(sent.single(), "moved")

        assertEquals("moved", pending.get())
    }

    @Test
    fun `two outstanding requests are told apart by their id`() {
        val r = requests()
        val first = r.start<String> { id -> id }
        val second = r.start<String> { id -> id }

        // Answered out of order, which is the ordinary case: the operator
        // resolves two requests against different objects at different speeds.
        r.complete(sent[1], "second")
        r.complete(sent[0], "first")

        assertEquals("first", first.get())
        assertEquals("second", second.get())
    }

    @Test
    fun `an answer for an id nobody is waiting on is dropped rather than throwing`() {
        // A late answer to a request that already timed out. Throwing here
        // would end the session from inside a gRPC callback, which costs the
        // agent every other request it has outstanding.
        val r = requests()

        r.complete(9999L, "nobody asked for this")
    }

    @Test
    fun `a request that is never answered fails at its deadline`() {
        val r = requests(timeoutMillis = 1_000)
        val pending = r.start<String> { id -> id }

        now += 1_001
        r.expire()

        assertTrue(pending.isCompletedExceptionally)
    }

    @Test
    fun `a request inside its deadline is left alone by expire`() {
        // The other half of the case above: an expire that fired early would
        // fail a request whose answer is still on its way.
        val r = requests(timeoutMillis = 1_000)
        val pending = r.start<String> { id -> id }

        now += 999
        r.expire()

        assertFalse(pending.isDone)
    }

    // The rule from the spec's section 4.1, and the one worth getting right.
    @Test
    fun `a stream change fails every outstanding request rather than retrying it`() {
        val r = requests()
        val first = r.start<String> { id -> id }
        val second = r.start<String> { id -> id }
        val sentBefore = sent.size

        r.failAll(IllegalStateException("stream displaced"))

        assertTrue(first.isCompletedExceptionally)
        assertTrue(second.isCompletedExceptionally)
        assertEquals(sentBefore, sent.size, "failAll must not resend anything")
    }
}
```

The generic parameters above are the request and response message types; the
test instantiates them as `Long` because nothing here reads a message, only
routes an id.

- [ ] **Step 2: Run and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :common:test --tests "*RequestsTest*" --console=plain --offline'`

- [ ] **Step 3: Write it**

A `ConcurrentHashMap<Long, Pending>` keyed by id, an `AtomicLong` for the
counter, and a deadline per entry. `expire` is called from the same tick that
sends the periodic report, so there is no second timer.

The class comment carries the rule that matters:

```kotlin
/**
 * **An in-flight request does not survive a stream change.** `SessionLoop`
 * renews make-before-break, so two streams are live at once; a request whose
 * answer would arrive on the displaced one is failed rather than resent.
 *
 * Retrying looks kinder and is not. Only the caller knows whether their
 * request is safe to repeat, and none of the three this channel will carry is:
 * a connect delivered twice moves a player who has already moved, and a scale
 * delivered twice scales twice. Failing hands the decision to the one place
 * that can make it.
 */
```

- [ ] **Step 4: Run and commit**

```bash
git add agent/common/src
git commit -m "$(cat <<'EOF'
feat(agent): outstanding requests, and what a renewal does to them

An in-flight request does not survive a stream change. SessionLoop renews
make-before-break, so two streams are live at once, and a request whose
answer would arrive on the displaced one is failed rather than resent.

Retrying looks kinder and is not: only the caller knows whether a request is
safe to repeat, and none of the three this channel will carry is. A connect
delivered twice moves a player who has already moved; a scale delivered twice
scales twice.

An answer for an id nobody is waiting on is dropped rather than throwing --
a late answer to a request that already expired must not end the session from
inside a gRPC callback and cost the agent every other request it has out.

expire runs on the tick that already sends the periodic report, so there is
no second timer to reason about.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 4: The operator answers, within bounds

**Files:**
- Create: `internal/agentserver/requests.go`
- Modify: `internal/agentserver/metrics.go`, `internal/agentserver/server.go`
- Test: `internal/agentserver/requests_test.go`, `internal/agentserver/proxy_envtest_test.go`

**Interfaces:**
- Produces: `func (s *Server) handleRequest(...)`, and
  `RequestsRefused` — a `CounterVec` labelled `reason` and nothing else.

- [ ] **Step 1: Write the failing tests, one per bound**

Each bound gets its own test, because a single test asserting "it was refused"
passes when the wrong bound fired:

```go
func TestAConnectForAPlayerOnAnotherNetworkIsRefused(t *testing.T)
func TestAConnectToAServerThisNamespaceDoesNotHaveIsRefused(t *testing.T)
func TestRequestsPastTheRateAreRefusedWithTheirOwnReason(t *testing.T)
func TestARefusalCarriesTheReasonAndNotJustAFailure(t *testing.T)
```

The namespace bound is **structural** — the request names a player and a
target, never a namespace, and the operator resolves both inside
`id.Namespace`. Assert that a player on another network is `NOT_FOUND` rather
than moved, which is the same test from the other side.

- [ ] **Step 2: Run and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/agentserver/ -run 'TestAConnect|TestRequests|TestARefusal'`

- [ ] **Step 3: Write the handler**

Resolve the player through `Agents.Roster(id.Namespace)` — which is why 7b-2
came first — and the target through the same namespace's servers. Then send
the move to the proxy that has that player, through `proxyreg.Fleet`.

The rate limit reuses the shape `grpcauth.PeerLimiter` uses rather than a new
mechanism; read that file before writing a second one, and if a token bucket is
already available there, use it.

**Every refusal increments `RequestsRefused` with its reason, and no label
carries a player, a pod or a namespace.** A metric labelled by any of those is
a cardinality bomb, and the roster's own rule in
`docs/network-boundaries.md` is the same rule.

- [ ] **Step 4: Prove each bound independently**

For each of the four tests, mutate the single check it exercises and confirm
**that** test fails and the others do not. A bound that cannot be shown to fire
on its own is a bound that might be dead behind another one.

- [ ] **Step 5: Run the package and commit**

```bash
git add internal/agentserver
git commit -m "$(cat <<'EOF'
feat(agentserver): the operator answers a request, within bounds

The namespace bound is structural rather than checked: a request names a
player and a target and never a namespace, and both are resolved inside the
namespace the pod's own token authenticated. There is no field an agent could
put another network's name in.

Each remaining bound has its own test and each was mutated on its own, because
one test asserting "it was refused" passes when the wrong bound fired -- and a
bound that cannot be shown to fire alone might be dead behind another.

Refusals count by reason and by nothing else. A metric labelled by player, pod
or namespace is a cardinality bomb, which is the rule the roster already
follows.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 5: `connect`, end to end

**Files:**
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/MirrorApi.kt`
- Modify: both roles, to route a `CloudResponse` into `Requests`
- Test: `agent/common/.../MirrorApiTest.kt`

**Interfaces:**
- Produces: `CompletionStage<ConnectResult> connect(UUID player, Target to)` on
  `SpawneryApi`, plus the `Target` and `ConnectResult` types the spec names.

- [ ] **Step 1: Add the API types**

`Target` is a sealed interface over `Target.server(String)` and
`Target.group(String)` — a plugin naming a group means "wherever that group has
room", which is the operator's decision and not the plugin's.

`ConnectResult` is a record carrying whether the move happened and where the
player ended up. **Asynchronous on both platforms**, per §3.1, including
Velocity where it need not be. The javadoc says so rather than leaving a
Velocity author to wonder.

- [ ] **Step 2: Implement it in `MirrorApi`**

`MirrorApi` gains a `Requests` and turns a call into a `CloudRequest`. The
symmetry test from 7b-4 grows a case: `connect` on both sides produces the same
request for the same arguments.

- [ ] **Step 3: Route the answer**

Both roles gain a `CLOUD_RESPONSE` branch that hands the message to `Requests`
and returns `Directive.None`, exactly as `NETWORK_STATE` does.

- [ ] **Step 4: Drive it against the stub**

`cmd/spawnery-stubop` answers a `ConnectRequest`, and a phase asserts the
round trip: the agent asked, the stub answered, the future completed.

This is the first thing in the whole milestone that exercises a request from a
real jar.

- [ ] **Step 5: Run everything and commit**

Run: `make test`, `make agent`, `make agent-test CONTAINER=podman`, `make lint`.

---

### Task 6: Write down what an agent may now ask for

**Files:**
- Modify: `docs/network-boundaries.md`
- Modify: `agent/api/README.md`

- [ ] **Step 1: The boundaries document**

It currently says "a backend still cannot ask for anything, because the channel
carries no request from an agent at all". **That stops being true here** and
the sentence must not be left standing — 7b-3's own section had the same
problem and this is the second time.

Replace it with what is now true: an agent may ask to move a player, the
operator resolves everything inside the pod's own namespace, and the bounds are
a rate limit and the structural one. Say plainly that a compromised pod can now
move players around its own network, that this is new, and that it cannot reach
another network.

- [ ] **Step 2: The plugin author's page**

`connect` gets a section: what it does, that it is asynchronous on both
platforms and why, and that a failure is ordinary rather than exceptional — a
player who logged out between the call and the move is a `NOT_FOUND`, not a
bug.

---

## Done when

- [ ] `make test`, `make agent`, `make lint` all pass
- [ ] `make agent-test CONTAINER=podman` passes, including the install-line
      assertion and the connect round trip
- [ ] Each of Task 4's four bounds was mutated on its own and failed on its own
- [ ] `grep -rn 'RequestsRefused' internal/ | grep -i 'player\|pod\|namespace'`
      finds nothing
- [ ] Nothing was pushed and no tag was created

## What 7b-5 leaves for 7c

- **`scale` and `retire` are not built.** The bounds are, and they are what
  those two need; the verbs land on them next.
- **No command.** `/cloud` is 7c, and Task 1's measurement is what it starts
  from: both platforms speak `com.mojang.brigadier`, so the tree is one
  implementation.
- **The event feed is not built.** `CloudEvent` is in the spec and in no code.
