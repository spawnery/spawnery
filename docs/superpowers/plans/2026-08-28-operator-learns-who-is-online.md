# Milestone 7b-2 — the operator learns who is online

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The operator holds, in memory, which players are on the network — UUID, name, and the backend each is on — aggregated across every proxy of a namespace and aged out on the same staleness rule the counts already use.

**Architecture:** The proxy is the only honest source: every player reaches a backend through one, and `VelocityPlayers` already reads the state a roster needs. `PlayerRef` gains a UUID, `ProxyRole.extraReports` sends a `PlayerRoster` beside the `BackendPlayers` it already sends, and `agent.Registry` stores it per session exactly as it stores `backends`. Nothing downstream consumes it yet; 7b-3 is what puts it on the wire to agents.

**Tech Stack:** Go (operator, registry), Kotlin (Velocity agent), protobuf. No CRD change, no chart change.

**Spec:** [`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../specs/2026-08-27-cloud-api-design.md) §9.1, "The identities, and what holding them costs"

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
- **`nix build` filters the source tree through the git index.** A new file
  that is not `git add`ed does not exist inside the sandbox, and the failure
  reads like a configuration error. `git add` before every `make agent`.
- `make proto` regenerates `internal/agentpb` and the Java stubs. The generated
  code is checked in; run it after a `.proto` change and commit the diff with
  the change.
- envtest shares one control plane with no cleanup between tests. Scope every
  List to the namespace under test.

## What the operator must not do with this

The spec says it and every task here inherits it: the roster is **in memory in
`agent.Registry`** and reaches no CR, no etcd, no log line at default
verbosity, and **no metric label**. A metric labelled by player name is both a
cardinality bomb and a retention decision nobody made. Task 4's registry test
is the only place a name is allowed to appear in an assertion.

## File structure

| Path | Responsibility |
|---|---|
| `proto/spawnery/agent/v1alpha1/agent.proto` | `PlayerRoster` and `RosterEntry`, on `ProxyMessage` |
| `agent/velocity/src/main/kotlin/.../Players.kt` | `PlayerRef` gains `uuid` |
| `agent/velocity/src/main/kotlin/.../ProxyRole.kt` | `extraReports` sends the roster beside the counts |
| `internal/agent/registry.go` | `RosterEntry`, `ReportRoster`, `Roster` |
| `internal/agentserver/server.go` | The `PLAYER_ROSTER` branch in `handleProxy` |
| `cmd/spawnery-stubop/main.go` | Records rosters so `agent-test.sh` can assert one |
| `hack/agent-test.sh` | A phase driving a real Velocity image and asserting the roster |
| `docs/network-boundaries.md` | What the operator now holds about a person |

---

### Task 1: The proxy's roster carries a UUID

`PlayerRef` has `username`, `currentServer` and `attachedServer` and no
identity. Velocity has one — `AgentPlugin.onServerConnected` already reads
`event.player.uniqueId` for the rescue set — the agent's own abstraction just
never carried it.

**Files:**
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/Players.kt`
- Modify: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/FakePlayers.kt`
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/PlayersTest.kt` (create if absent)

**Interfaces:**
- Consumes: nothing.
- Produces: `PlayerRef.uuid: UUID`, implemented by `VelocityPlayer` as
  `player.uniqueId` and by the test double from a constructor argument.

- [ ] **Step 1: Read the test double first**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c cat agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/FakePlayers.kt`

Every existing test builds players through it, so its constructor is what the
new field has to fit into without rewriting fifty call sites. **Give `uuid` a
default** derived from the username — `UUID.nameUUIDFromBytes(username.toByteArray())` —
so existing call sites keep compiling and a test that cares about identity
passes one explicitly.

- [ ] **Step 2: Write the failing test**

Append to `PlayersTest.kt` (create it with the repository's Apache header if it
does not exist):

```kotlin
class PlayersTest {
    @Test
    fun `a player ref carries the identity the operator has no other source for`() {
        val id = UUID.fromString("00000000-0000-4000-8000-000000000001")
        val player = FakePlayer(username = "somebody", uuid = id)
        assertEquals(id, player.uuid)
        assertEquals("somebody", player.username)
    }
}
```

- [ ] **Step 3: Run it and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :velocity:test --tests "*PlayersTest*" --console=plain --offline'`

Expected: a compile failure — `uuid` is not a member of `PlayerRef`.

- [ ] **Step 4: Add the field**

In `Players.kt`, on the `PlayerRef` interface, above `username`:

```kotlin
    /**
     * This player's Minecraft UUID.
     *
     * The operator has no other source for it. `PlayerJoinedServer` carries a
     * username and the operator's handler discards it; the registry keeps
     * counts. So until 7b-2 nothing upstream could name a person, and
     * `SpawneryApi.players()` had no way to be answered.
     */
    val uuid: UUID
```

On `VelocityPlayer`:

```kotlin
    override val uuid: UUID
        get() = player.uniqueId
```

Add `import java.util.UUID` to both files if absent.

- [ ] **Step 5: Run the velocity suite**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :velocity:test --console=plain --offline'`

Expected: PASS, every existing test included. A failure here means the default
in Step 1 was not applied and existing call sites lost their constructor.

- [ ] **Step 6: Commit**

```bash
git add agent/velocity/src
git commit -m "$(cat <<'EOF'
feat(velocity-agent): a player ref carries its UUID

The agent's own abstraction had username, currentServer and attachedServer
and no identity, while Velocity has had one all along -- onServerConnected
already reads player.uniqueId for the rescue set.

It matters now because the operator has no other source. PlayerJoinedServer
carries a username and the operator's handler discards it, with a comment
saying nothing consumes it; the registry keeps counts. So nothing upstream
could name a person, which is why SpawneryApi.players() shipped in 7b-1 with
no way to be answered.

The test double defaults the UUID from the username, so every existing call
site keeps compiling and only a test that cares about identity passes one.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 2: `PlayerRoster` on the wire

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Regenerate: `internal/agentpb/*` and the Java stubs, via `make proto`

**Interfaces:**
- Consumes: nothing.
- Produces: `PlayerRoster { repeated RosterEntry players = 1; }` and
  `RosterEntry { string uuid = 1; string name = 2; string server = 3; }`,
  reachable as `ProxyMessage.player_roster` (field 6).

- [ ] **Step 1: Add the messages**

In `agent.proto`, after `BackendPlayers` and before `ProxyMessage`:

```proto
// PlayerRoster is who this proxy is serving, by identity.
//
// It exists because the operator had no source for one. PlayerJoinedServer
// carries a username and is accepted and ignored; PlayerCount and
// BackendPlayers are counts. So the operator could say how many people were on
// a backend and never who, and an API promising a player list had nothing to
// answer from.
//
// **Beside BackendPlayers rather than replacing it, deliberately.** That
// message is load-bearing for the drain, and its own comment reasons carefully
// about which players it counts -- ConnectedPlayer.getConnectionInFlightOrConnectedServer,
// so that a player still handshaking is included. Deriving it from this one
// would put the drain's correctness at the mercy of a change made for a
// reporting feature. The two are built from one read of the same roster on the
// same tick, and only this one is new.
//
// A state and not an event, like every other report here: each message carries
// the whole roster, so a dropped one costs a report interval of freshness
// rather than leaving somebody stranded on a list forever. A player absent
// from the roster is a player this proxy no longer has.
message PlayerRoster {
  repeated RosterEntry players = 1;
}

// RosterEntry is one player, as the proxy sees them right now.
message RosterEntry {
  // The Minecraft UUID, which is the only stable identity here: a name can be
  // changed and reused, a UUID cannot.
  string uuid = 1;
  // The username, for a plugin that wants to print something a person
  // recognises. The operator holds it in memory and puts it nowhere else --
  // no CR, no etcd, no metric label.
  string name = 2;
  // The backend this player is on, or on their way to, empty when neither.
  // Read from the same field BackendPlayers counts, so the two agree by
  // construction rather than by two implementations staying in step.
  string server = 3;
}
```

In `ProxyMessage`, add the field:

```proto
    PlayerRoster player_roster = 6;
```

- [ ] **Step 2: Regenerate**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make proto`

Expected: a diff under `internal/agentpb/` and the generated Java sources.

- [ ] **Step 3: Confirm it builds on both sides**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go build ./...`

Expected: clean.

```bash
git add -A
```

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make agent`

Expected: green. The Java stubs are generated into `:common`'s
`src/proto/java`, so this is what proves the Kotlin side can see the new type.

- [ ] **Step 4: Commit**

```bash
git commit -m "$(cat <<'EOF'
feat(proto): a proxy reports who it is serving, not only how many

The operator had no source for a player's identity. PlayerJoinedServer
carries a username and the handler discards it; PlayerCount and
BackendPlayers are counts. So it could say how many people were on a backend
and never who.

PlayerRoster sits beside BackendPlayers rather than replacing it, and that is
deliberate: BackendPlayers is load-bearing for the drain, and its comment
reasons carefully about counting a player who is still handshaking. Deriving
it from this message would put the drain's correctness at the mercy of a
change made for a reporting feature. Both are built from one read of the same
roster on the same tick.

A state and not an event, like every report on this channel: each message
carries the whole roster, so a dropped one costs a report interval of
freshness rather than stranding somebody on a list forever.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 3: The proxy sends it

**Files:**
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/ProxyRole.kt`
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/ProxyRoleTest.kt`

**Interfaces:**
- Consumes: `PlayerRef.uuid` from Task 1, `PlayerRoster` from Task 2.
- Produces: `extraReports()` returns two messages — the existing
  `BackendPlayers` first, then a `PlayerRoster`.

- [ ] **Step 1: Write the failing test**

Append to `ProxyRoleTest.kt`:

```kotlin
    @Test
    fun `the periodic report carries the roster beside the counts`() {
        val id = UUID.fromString("00000000-0000-4000-8000-00000000000a")
        val players = FakePlayers(
            FakePlayer(username = "alice", uuid = id, attachedServer = "lobby-a"),
            FakePlayer(username = "bob", attachedServer = null),
        )
        val role = proxyRole(players = players)

        val reports = role.extraReports()

        // Both, and the counts first: BackendPlayers is what the drain reads,
        // and a change here must not reorder what an operator already parses.
        assertEquals(2, reports.size)
        assertTrue(reports[0].hasBackendPlayers())
        assertTrue(reports[1].hasPlayerRoster())

        val roster = reports[1].playerRoster.playersList
        assertEquals(2, roster.size, "a player on no server is still on this proxy")
        val alice = roster.single { it.name == "alice" }
        assertEquals(id.toString(), alice.uuid)
        assertEquals("lobby-a", alice.server)
        assertEquals("", roster.single { it.name == "bob" }.server,
            "a player attached to nothing carries an empty server, not a missing entry")
    }
```

`proxyRole(players = ...)` is this file's existing helper; read the file's other
tests for its exact name and defaults before writing this, and use whatever
they use.

- [ ] **Step 2: Run it and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :velocity:test --tests "*ProxyRoleTest*" --console=plain --offline'`

Expected: FAIL — `extraReports` returns one message.

- [ ] **Step 3: Send both**

Replace `extraReports`:

```kotlin
    override fun extraReports(): List<ProxyMessage> {
        // One read of the roster, two messages from it. Reading players.all()
        // twice would let a join land between them and put a player in the
        // roster who is not in the counts -- two answers to one question, from
        // one tick.
        val snapshot = players.all()

        val counts = mutableMapOf<String, Int>()
        val roster = PlayerRoster.newBuilder()
        for (player in snapshot) {
            val attached = player.attachedServer
            if (attached != null) {
                counts[attached] = (counts[attached] ?: 0) + 1
            }
            // Everyone, including a player attached to nothing: they are on
            // this proxy, which is what the roster is about. The counts skip
            // them because they are on no backend, which is what those are
            // about. The two differ here on purpose.
            roster.addPlayers(
                RosterEntry.newBuilder()
                    .setUuid(player.uuid.toString())
                    .setName(player.username)
                    .setServer(attached ?: ""),
            )
        }

        return listOf(
            ProxyMessage.newBuilder()
                .setBackendPlayers(BackendPlayers.newBuilder().putAllPlayers(counts))
                .build(),
            ProxyMessage.newBuilder()
                .setPlayerRoster(roster)
                .build(),
        )
    }
```

Add the imports for `PlayerRoster` and `RosterEntry`.

- [ ] **Step 4: Run the whole velocity suite**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c bash -c 'cd agent && gradle :velocity:test --console=plain --offline'`

Expected: PASS. Existing tests that assert `extraReports().size == 1` will
fail; each is asserting the old shape and each should now assert the counts are
`reports[0]`, not that there is one message.

- [ ] **Step 5: Commit**

```bash
git add agent/velocity/src
git commit -m "$(cat <<'EOF'
feat(velocity-agent): the periodic report carries the roster

One read of players.all(), two messages from it. Reading twice would let a
join land between them and put a player in the roster who is not in the
counts -- two answers to one question from a single tick.

The two deliberately differ on one point: a player attached to no backend is
in the roster and not in the counts. The roster is about who is on this
proxy; the counts are about who is on a backend, and that player is on
neither.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 4: The registry keeps it

**Files:**
- Modify: `internal/agent/registry.go`
- Test: `internal/agent/registry_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (this side is fed by Task 5).
- Produces:
  - `type RosterEntry struct { UUID, Name, Server string }`
  - `func (r *Registry) ReportRoster(key, namespace string, players []RosterEntry) error`
  - `func (r *Registry) Roster(namespace string) (players []RosterEntry, stale bool)`

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/registry_test.go`. Read the file's existing helpers
for building a connected proxy entry and use them rather than inventing new
ones:

```go
func TestRosterMergesEveryProxyInTheNamespace(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	for _, uid := range []string{"proxy-a", "proxy-b"} {
		r.Connect(uid, RoleProxy)
	}
	if err := r.ReportRoster("proxy-a", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-0"},
	}); err != nil {
		t.Fatalf("report from proxy-a: %v", err)
	}
	if err := r.ReportRoster("proxy-b", "minecraft", []RosterEntry{
		{UUID: "u-bob", Name: "bob", Server: "lobby-1"},
	}); err != nil {
		t.Fatalf("report from proxy-b: %v", err)
	}

	got, stale := r.Roster("minecraft")
	if stale {
		t.Error("stale = true with two fresh reports")
	}
	if len(got) != 2 || got[0].UUID != "u-alice" || got[1].UUID != "u-bob" {
		t.Fatalf("roster = %+v, want both players sorted by UUID", got)
	}
}

func TestRosterKeepsOneEntryPerPlayerAcrossProxies(t *testing.T) {
	// A player appears on two proxies while a rollout hands them over. They
	// are one person and must be counted once -- a plugin iterating this to
	// message everybody would otherwise message them twice.
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-a", RoleProxy)
	r.Connect("proxy-b", RoleProxy)

	if err := r.ReportRoster("proxy-a", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-0"},
	}); err != nil {
		t.Fatalf("report from proxy-a: %v", err)
	}
	clock.now = clock.now.Add(time.Second)
	if err := r.ReportRoster("proxy-b", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-1"},
	}); err != nil {
		t.Fatalf("report from proxy-b: %v", err)
	}

	got, _ := r.Roster("minecraft")
	if len(got) != 1 {
		t.Fatalf("roster = %+v, want one entry for one player", got)
	}
	if got[0].Server != "lobby-1" {
		t.Errorf("server = %q, want the most recently reported one", got[0].Server)
	}
}

func TestRosterSkipsAProxyWhoseReportWentStale(t *testing.T) {
	// The rule the counts already use: older than twice the report interval.
	// A proxy that stopped reporting must stop asserting who is online rather
	// than freezing a roster.
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-a", RoleProxy)
	if err := r.ReportRoster("proxy-a", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-0"},
	}); err != nil {
		t.Fatalf("report: %v", err)
	}

	clock.now = clock.now.Add(11 * time.Second) // > 2 * 5s

	got, stale := r.Roster("minecraft")
	if !stale {
		t.Error("stale = false for a report older than twice the interval")
	}
	if len(got) != 0 {
		t.Errorf("roster = %+v, want nothing from a stale proxy", got)
	}
}

func TestARosterFromAServerAgentIsRefused(t *testing.T) {
	// A backend has no view of anybody but its own players and no UUIDs at
	// all, so a roster from one is a bug in an agent rather than a state to
	// store. Same rule, same reason, as ReportBackends.
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("pod-uid-1", RoleServer)

	if err := r.ReportRoster("pod-uid-1", "minecraft", nil); err == nil {
		t.Error("a server agent's roster was accepted")
	}
}

func TestRosterIsScopedToItsNamespace(t *testing.T) {
	// namespace comes from the authenticated identity at the call site, never
	// from the message. This asserts the reader's half.
	clock := &fakeClock{now: time.Unix(1000, 0)}
	r := New(clock.Now, 5*time.Second, clock.now)
	r.Connect("proxy-other", RoleProxy)
	if err := r.ReportRoster("proxy-other", "minecraft", []RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-0"},
	}); err != nil {
		t.Fatalf("report: %v", err)
	}

	if got, _ := r.Roster("other"); len(got) != 0 {
		t.Errorf("roster = %+v, want nothing from another namespace", got)
	}
}
```

These use the same `fakeClock` and `New(clock.Now, 5*time.Second, clock.now)`
construction every test in this file uses — read
`TestAttachedToSumsTheProxiesThatAreCurrent` beside them for the pattern.

- [ ] **Step 2: Run and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/agent/ -run TestRoster`

Expected: a compile failure — `RosterEntry` and `ReportRoster` do not exist.

- [ ] **Step 3: Write the storage**

In `registry.go`, add to the `entry` struct beside `backends`:

```go
	// roster is who this proxy said it was serving, and when. Kept beside
	// backends rather than derived from it: that map is counts, and this is
	// the only place in the operator that holds a person's name.
	//
	// In memory and nowhere else. It reaches no CR, no etcd, no log line at
	// default verbosity and no metric label -- a metric labelled by player
	// name is a cardinality bomb and a retention decision nobody made.
	roster   []RosterEntry
	rosterAt time.Time
```

Then the type and the two methods:

```go
// RosterEntry is one player as a proxy last saw them.
type RosterEntry struct {
	// UUID is the Minecraft UUID, and the identity everything else keys on: a
	// name can be changed and reused, a UUID cannot.
	UUID string
	// Name is the username, for something a person recognises.
	Name string
	// Server is the backend this player is on or heading for, empty for
	// neither. The same field BackendPlayers counts, so the two agree by
	// construction.
	Server string
}

// ReportRoster records who a proxy is serving.
//
// namespace comes from the authenticated identity rather than from the
// message, for the reason the package comment gives about every other fact
// here: an agent may not say which namespace's players it is talking about.
func (r *Registry) ReportRoster(key, namespace string, players []RosterEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if e.role != RoleProxy {
		// A backend sees its own players and no UUIDs at all, so a roster from
		// one is a bug in an agent rather than a state to store. Same rule and
		// same reason as ReportBackends.
		return fmt.Errorf("roster from a %s agent %q", e.role, key)
	}
	e.namespace = namespace
	e.roster = players
	e.rosterAt = r.now()
	return nil
}

// Roster is every player the live proxies of a namespace say they are
// serving, and whether any proxy's answer is too old to believe.
//
// A player is on exactly one proxy, but not for the whole of a proxy
// changeover: during a handover both may report them for a report interval or
// two. They are one person, so the entry with the newer report wins -- a
// plugin iterating this to message everybody must not message somebody twice
// because a rollout was in flight.
func (r *Registry) Roster(namespace string) ([]RosterEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type dated struct {
		entry RosterEntry
		at    time.Time
	}
	byPlayer := make(map[string]dated)
	stale := false

	for _, e := range r.entries {
		if e.role != RoleProxy || e.namespace != namespace {
			continue
		}
		if e.rosterAt.IsZero() {
			continue
		}
		if !e.connected || r.now().Sub(e.rosterAt) > 2*r.reportInterval {
			stale = true
			continue
		}
		for _, p := range e.roster {
			if prev, ok := byPlayer[p.UUID]; ok && prev.at.After(e.rosterAt) {
				continue
			}
			byPlayer[p.UUID] = dated{entry: p, at: e.rosterAt}
		}
	}

	out := make([]RosterEntry, 0, len(byPlayer))
	for _, d := range byPlayer {
		out = append(out, d.entry)
	}
	// Sorted so a caller gets a stable order out of a map, and so a test can
	// assert a list rather than a set.
	sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
	return out, stale
}
```

Add `"sort"` to the imports if absent.

- [ ] **Step 4: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/agent/`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "$(cat <<'EOF'
feat(agent-registry): the operator holds who is online, in memory only

Kept beside backends rather than derived from it: that map is counts, and
this is the only place in the operator that holds a person's name. Where it
does not go is asserted by where it is not written -- no CR, no etcd, no
default-verbosity log line and no metric label, because a metric labelled by
player name is a cardinality bomb and a retention decision nobody made.

A player is on exactly one proxy, but not for the whole of a proxy
changeover: during a handover both may report them for an interval or two.
The newer report wins, so a plugin iterating this to message everybody does
not message somebody twice because a rollout was in flight.

Staleness is the rule the counts already use -- older than twice the report
interval, or the stream is gone -- so a proxy that stopped reporting stops
asserting who is online rather than freezing a roster.

A roster from a server agent is refused for the reason ReportBackends
refuses one: a backend sees its own players and no UUIDs at all.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 5: The operator wires the message to the registry

**Files:**
- Modify: `internal/agentserver/server.go` (`handleProxy`)
- Test: `internal/agentserver/proxy_envtest_test.go`

**Interfaces:**
- Consumes: `Registry.ReportRoster` from Task 4, `PlayerRoster` from Task 2.
- Produces: nothing new. A `ProxyMessage_PlayerRoster` case in `handleProxy`.

- [ ] **Step 1: Write the failing test**

Append to `proxy_envtest_test.go`, following the file's existing shape for
driving a proxy stream:

```go
// A roster sent on the stream reaches the registry, keyed by the namespace the
// token authenticated rather than anything the message said. Modelled on
// TestABackendReportReachesTheRegistryUnderTheAuthenticatedNamespace, which is
// the same shape for the same reason.
func TestAProxyRosterReachesTheRegistryUnderTheAuthenticatedNamespace(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-aaaa")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the opening message never arrived: %v", err)
	}

	if err := stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_PlayerRoster{
			PlayerRoster: &agentpb.PlayerRoster{
				Players: []*agentpb.RosterEntry{
					{Uuid: "u-alice", Name: "alice", Server: "lobby-0"},
				},
			},
		},
	}); err != nil {
		t.Fatalf("send the roster: %v", err)
	}

	// The registry is written from the receive loop, so poll rather than
	// assume the send has been applied by the time Send returned.
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, stale := f.agents.Roster(f.ns)
		if len(got) == 1 && got[0].UUID == "u-alice" && !stale {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("roster = %+v stale=%v, want one fresh entry for alice", got, stale)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// And it did not land under some other namespace's name, which is what a
	// report trusted about its own scope would have allowed.
	if got, _ := f.agents.Roster("somewhere-else"); len(got) != 0 {
		t.Errorf("roster in another namespace = %+v, want none", got)
	}
}
```

- [ ] **Step 2: Run and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/agentserver/ -run TestAProxyRoster`

Expected: FAIL — the roster never arrives, because nothing handles the message.

- [ ] **Step 3: Handle it**

In `handleProxy`, beside the `BackendPlayers` case:

```go
	case *agentpb.ProxyMessage_PlayerRoster:
		entries := make([]agent.RosterEntry, 0, len(m.PlayerRoster.GetPlayers()))
		for _, p := range m.PlayerRoster.GetPlayers() {
			// An entry with no UUID is dropped rather than stored under "":
			// the reader keys on UUID, so a second one would silently replace
			// the first and the roster would show one player where two are on.
			if p.GetUuid() == "" {
				continue
			}
			entries = append(entries, agent.RosterEntry{
				UUID:   p.GetUuid(),
				Name:   p.GetName(),
				Server: p.GetServer(),
			})
		}
		if err := s.opts.Agents.ReportRoster(id.PodUID, id.Namespace, entries); err != nil {
			// V(1) and without a name in it. The roster is the one thing here
			// that identifies a person, and a rejected report is an agent bug
			// rather than something an operator acts on per player.
			logger.V(1).Info("roster report refused", "reason", err.Error())
		}
```

- [ ] **Step 4: Run the package**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/agentserver/`

Expected: PASS.

- [ ] **Step 5: Prove the namespace really comes from the token**

Change the handler to pass a literal `"other"` instead of `id.Namespace`, run
the test, and confirm it fails — the registry stores under a namespace nobody
authenticated, and `Roster(ns)` finds nothing.

**Restore the line** and confirm with `git diff`.

- [ ] **Step 6: Commit**

```bash
git add internal/agentserver
git commit -m "$(cat <<'EOF'
feat(agentserver): a proxy's roster reaches the registry

The namespace comes from the authenticated identity and never from the
message, which is the rule every other fact on this channel follows.
Mutation-checked rather than asserted: passing a literal namespace instead of
id.Namespace makes the test fail, because the registry then stores under a
namespace nobody authenticated.

An entry with no UUID is dropped rather than stored under "". The reader keys
on UUID, so a second empty one would silently replace the first and the
roster would show one player where two are on.

The refusal log carries no player name. This message is the one thing on this
channel that identifies a person, and a rejected report is an agent bug
rather than something an operator acts on per player.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 6: Driven from a real image

Every test above uses a stub or a fake. This runs the real Velocity jar against
the stub operator and asserts a roster arrives with a real UUID in it —
`hack/agent-test.sh` is where this repository proves an agent does what its
unit tests say.

**Files:**
- Modify: `cmd/spawnery-stubop/main.go`
- Modify: `hack/agent-test.sh`

**Interfaces:**
- Consumes: everything above.
- Produces: the stub records rosters and exposes them the way it exposes its
  other observations; a new phase in `agent-test.sh`.

- [ ] **Step 1: Read how the stub records an observation**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c grep -n 'PlayerJoinedServer\|BackendPlayers' cmd/spawnery-stubop/main.go`

The `PlayerJoinedServer` branch near line 939 already builds a map for its
observation log. Follow that shape exactly rather than adding a second
mechanism.

- [ ] **Step 2: Record rosters in the stub**

Add a `PlayerRoster` branch beside it, recording `uuid`, `name` and `server`
per entry into the same observation log.

- [ ] **Step 3: Add the phase to `agent-test.sh`**

Read the existing phases first — the file numbers them and each has the same
shape. Add one that:

1. starts the Velocity image against the stub as the other proxy phases do,
2. connects one player with `cmd/spawnery-join --hold`, as the runbook's own
   evidence steps do,
3. waits for a report interval,
4. asserts the stub observed a `PlayerRoster` containing that player's UUID.

**Assert the UUID, not the name.** The name is what the join tool was told to
use, so asserting it proves only that the string travelled. The UUID is
computed by Velocity from the player it authenticated, and is the thing this
milestone added.

- [ ] **Step 4: Run it**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make agent-test`

Expected: green, with the new phase reporting the UUID it saw.

This needs a container runtime and only works on `x86_64-linux`. Pass
`CONTAINER=podman` if `docker` is not the runtime.

- [ ] **Step 5: Commit**

```bash
git add cmd/spawnery-stubop hack/agent-test.sh
git commit -m "$(cat <<'EOF'
test(agent): the roster is driven from the real Velocity image

Every other test of this milestone uses a fake player list. This runs the
shipped jar against the stub operator with a real connection and asserts a
roster arrives.

It asserts the UUID and not the name. The name is what the join tool was told
to use, so asserting it would prove only that a string travelled; the UUID is
what Velocity computed for the player it authenticated, and is the thing this
milestone added.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 7: Write down what the operator now holds

`docs/network-boundaries.md` is where this repository records what a security
feature is worth and what it does not cover. The operator holding a person's
name is a change to what a compromised operator, or a person with read access
to it, could learn.

**Files:**
- Modify: `docs/network-boundaries.md`

- [ ] **Step 1: Add the section**

Add at the end:

```markdown
## What the operator knows about a person

The operator holds, for every player on a network, their Minecraft UUID,
their username, and the backend they are on. Before milestone 7b-2 it held
counts and no identity at all.

**It is in memory and nowhere else.** `agent.Registry` keeps it per proxy
session; it reaches no custom resource, no etcd, no log line at default
verbosity and no metric label. The last of those is deliberate twice over: a
metric labelled by player name would multiply every series by the player base,
and it would turn a live figure into whatever the monitoring stack's retention
is — a decision nobody made and one this project should not make by accident.

It expires on its own. A roster older than twice the report interval is
skipped, and a proxy whose stream is gone contributes nothing, so an operator
that stops hearing from a proxy stops claiming to know who is online rather
than serving a frozen list.

What this does **not** do is bound who can read it. Anyone who can reach the
operator's process — a debugger, a core dump, a memory-reading exploit — reads
the roster, and no NetworkPolicy in this repository is about that. The bound
that exists is the one that always existed: the agent channel is
mutually authenticated and namespace-scoped, so a proxy learns about its own
network and nothing else, and the operator never sends a roster to anybody
until milestone 7b-3 gives it a reason to.
```

- [ ] **Step 2: Commit**

```bash
git add docs/network-boundaries.md
git commit -m "$(cat <<'EOF'
docs(network-boundaries): what the operator now knows about a person

Holding a name and a UUID where the operator previously held a count is a
change to what somebody with access to the process could learn, and this file
is where this project records that class of fact rather than leaving it to be
discovered.

It says where the data does not go, why the metric label in particular is
refused -- a cardinality multiplier and a retention decision nobody made --
and, in the same paragraph rather than a later one, the bound that does not
exist: nothing here stops whoever can read the operator's memory.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Done when

- [ ] `make test` passes
- [ ] `make agent` passes
- [ ] `make agent-test` passes, including the new phase
- [ ] `make lint` reports nothing
- [ ] The mutation in Task 5 Step 5 was run, failed as predicted, and reverted
- [ ] `grep -rn 'roster' internal/ --include='*.go' | grep -i 'metric\|label\|log.*Name'`
      finds nothing: the roster reaches no metric and no default-verbosity log
- [ ] Nothing was pushed and no tag was created

## What 7b-2 leaves for its successors

- **Nothing reads the roster.** `Registry.Roster` has one caller in the tests
  and none in production. 7b-3 is what puts it on the wire, and until then this
  milestone is the operator learning something it does not yet use — which is
  worth stating plainly rather than dressing up, because a reviewer will
  otherwise look for the consumer.
- **`SpawneryApi.players()` still cannot be answered.** The source exists now;
  the delivery does not. 7b-4 closes it.
- **A Paper agent contributes nothing here.** A backend knows its own players
  and no UUIDs, so the roster is the proxy's alone. If a network ever runs with
  no proxy, it has no roster — and it also has no way for anyone to join.
