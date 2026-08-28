# Milestone 7b-3 — `NetworkState` on both channels

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Both agent kinds receive the same picture of their network — groups, servers, and who is online — on connect and on every resync. A Paper agent gains a downstream path it has never had.

**Architecture:** One builder produces the picture; two fan-outs deliver it. `proxyreg.Fleet` already carries messages to proxies and gains one more; a new `internal/serverreg` does for `ServerSession` what `Fleet` does for `ProxySession`. The two fan-outs are deliberately not one — see below.

**Tech Stack:** Go, protobuf, controller-runtime's cached reader. No CRD change, no agent-side consumption (that is 7b-4).

**Spec:** [`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../specs/2026-08-27-cloud-api-design.md) §3.1, §4.1, §9.1

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
- `make agent-test` needs `CONTAINER=podman` on this machine — there is no
  `docker` binary — and `TMPDIR` pointed somewhere that is not a small tmpfs.
- envtest shares one control plane with no cleanup. Scope every List to the
  namespace under test.

## Why two fan-outs and not one

Measured before deciding: `internal/proxyreg/fleet.go` is 480 lines and
eighteen functions, and they split almost exactly in half. Nine are session
machinery — `New`, `Join`, `leave`, `close`, `send`, `broadcast`, `Resync`,
`Start`, `NeedLeaderElection`. Nine are the proxy protocol — `snapshot`,
`fallbacks`, `registeredServer`, `drainMessage`, `Register`, `Deregister`,
`Drain`, `SetReady`, `readyMessage`.

Extracting the generic half is tempting and is the wrong trade here. The
server side needs strictly less than the proxy side: no `lastReady` memo, no
fallback lists, no drain orders, and no per-group snapshot, because a server's
view is its whole namespace. A generic that carried the proxy's per-session
hooks would be a parameterised version of the harder case serving the easier
one, and it would put a refactor of the drain's delivery path inside a
milestone about reporting — the mistake 7a exists to have avoided.

**What is genuinely shared is the picture, not the plumbing**, and that is what
Task 2 extracts: one builder, called by both. The fan-out mechanics — a
bounded outbox, send-or-cut, and building the first message under the same
lock the broadcasts take — are eighty lines of well-understood code in the new
package, and Task 4's comment names the duplication and says why it is one.

## A note on the test code below

Task 2's tests are written out, because that package is new and has no
fixtures to match. Tasks 3 to 6 give each test's name, its assertion and the
neighbouring test to copy the fixture from, and not a body. That is deliberate
rather than unfinished: those tests run against `fleet_test.go`'s and
`server_envtest_test.go`'s own helpers, and a body written here without
reading them would be code the executor has to rewrite before it compiles —
which is exactly what happened while this plan's predecessor was being written.
**Read the named neighbour first, then write the test in its idiom.**

## File structure

| Path | Responsibility |
|---|---|
| `proto/spawnery/agent/v1alpha1/agent.proto` | `NetworkState` and its parts, on both operator-to-agent messages |
| `internal/netstate/netstate.go` | Builds a `NetworkState` from the cache and the registry. The one shared piece |
| `internal/netstate/netstate_test.go` | Its rules, table-driven, with a fake reader |
| `internal/proxyreg/fleet.go` | `snapshot` gains the state; `Resync` carries it |
| `internal/serverreg/registry.go` | `ServerSession`'s fan-out: join, leave, send-or-cut, resync |
| `internal/serverreg/registry_test.go` | The session machinery, including the cut |
| `internal/agentserver/server.go` | `ServerSession` joins the new registry |
| `cmd/spawnery-operator/main.go` | Constructs it and adds it to the manager |
| `cmd/spawnery-stubop/main.go` | Sends one, so `agent-test.sh` can assert both images receive it |
| `hack/agent-test.sh` | A phase per image |

---

### Task 1: `NetworkState` on the wire

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Regenerate via `make proto`

**Interfaces:**
- Consumes: `RosterEntry` from 7b-2.
- Produces: `NetworkState { repeated GroupState groups; repeated ServerState servers; repeated RosterEntry players; }`,
  `GroupState { string name; Kind kind; int32 replicas; int32 ready_replicas; int32 online_players; int32 free_slots; }`
  with `Kind` an enum `KIND_UNSPECIFIED|EPHEMERAL|PERSISTENT|PROXY`, and
  `ServerState { string name; string group; string phase; int32 players; int32 slots; bool registered; }`.
  Reachable as `OperatorToProxy.network_state` (field 8) and
  `OperatorToServer.network_state` (field 3).

- [ ] **Step 1: Add the messages**

After `PlayerRoster`/`RosterEntry`, before `ProxyMessage`:

```proto
// NetworkState is the whole picture of one namespace, as the operator sees it.
//
// The mirror the plugin API reads from. It goes to both agent kinds, in the
// same shape, because the API's whole premise is that a plugin author moving
// between a backend and a proxy does not have to think differently -- and two
// shapes here would make that a lie one layer down.
//
// A state and not a diff. Every message carries the whole picture, so a
// reconnect needs no catch-up and a dropped one costs a resync interval of
// freshness. That is the same rule FullSync follows, for the same reason.
//
// The phase is a string rather than an enum, and deliberately: the operator's
// phase vocabulary lives in internal/phase and gains values, and an agent that
// is older than a value must read it as "something I do not know" rather than
// fail to parse the message. cloud.spawnery.agent.api.ServerPhase.fromWire is
// the other half of that decision.
message NetworkState {
  repeated GroupState groups = 1;
  repeated ServerState servers = 2;
  // Every player on this network, aggregated across the proxies. Empty when
  // no proxy has reported recently -- which is a real state and not an error,
  // and is why the API documents an empty list as ordinary.
  repeated RosterEntry players = 3;
}

// GroupState is one group as the operator's status fields describe it.
message GroupState {
  string name = 1;
  Kind kind = 2;
  int32 replicas = 3;
  int32 ready_replicas = 4;
  int32 online_players = 5;
  // The operator's own figure, which counts only ready servers of the group's
  // current spec. Not a sum an agent could compute from the servers below, and
  // the two can disagree while a rolling update is in flight.
  int32 free_slots = 6;

  // Which sizing rule the group answers to.
  //
  // KIND_UNSPECIFIED is what an operator sends for a kind this proto predates,
  // and what a proto3 default gives an agent that predates a value. Both read
  // as "unknown" rather than as a specific kind, which is the only reading
  // that cannot be wrong.
  enum Kind {
    KIND_UNSPECIFIED = 0;
    EPHEMERAL = 1;
    PERSISTENT = 2;
    PROXY = 3;
  }
}

// ServerState is one backend as the operator last observed it.
message ServerState {
  string name = 1;
  string group = 2;
  // The operator's phase spelling -- see NetworkState on why this is a string.
  string phase = 3;
  int32 players = 4;
  int32 slots = 5;
  // Whether the proxies have this server in their routing tables. A server can
  // be Ready and not registered -- that is the first half of a drain -- so a
  // plugin choosing where to send somebody wants this and not the phase.
  bool registered = 6;
}
```

Then the two fields:

```proto
// in OperatorToProxy
    NetworkState network_state = 8;

// in OperatorToServer
    NetworkState network_state = 3;
```

- [ ] **Step 2: Regenerate and build both sides**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make proto`

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go build ./...`

```bash
git add -A
```

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make agent`

Expected: green on both. The Java stubs are what prove the Kotlin side sees the
new types.

- [ ] **Step 3: Commit**

```bash
git commit -m "$(cat <<'EOF'
feat(proto): the whole picture of a namespace, in one message

NetworkState is the mirror the plugin API reads from, and it goes to both
agent kinds in the same shape. The API's premise is that a plugin author
moving between a backend and a proxy does not think differently; two shapes
here would make that a lie one layer down.

A state and not a diff, like FullSync and for the same reason: a reconnect
needs no catch-up and a dropped message costs a resync interval of freshness
rather than leaving an agent permanently wrong.

The phase is a string. internal/phase gains values, and an agent older than
one must read it as something it does not know rather than fail to parse the
message it arrived in -- which is the other half of the decision that put
UNKNOWN in the API's own ServerPhase.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 2: One builder, for both

The picture is the shared thing. Two fan-outs calling one builder cannot
disagree about what a network looks like; two builders eventually would.

**Files:**
- Create: `internal/netstate/netstate.go`
- Test: `internal/netstate/netstate_test.go`

**Interfaces:**
- Consumes: `agent.Registry.Roster` from 7b-2, `client.Reader`.
- Produces:

  ```go
  type Source struct {
      Reader client.Reader
      Agents *agent.Registry
  }
  func (s Source) Build(ctx context.Context, namespace string) (*agentpb.NetworkState, error)
  ```

- [ ] **Step 1: Write the failing tests**

Create `internal/netstate/netstate_test.go`. The fake reader is the one
`internal/proxyreg/fleet_test.go:84` builds — `fake.NewClientBuilder()
.WithScheme(scheme).WithStatusSubresource(&spawneryv1alpha1.Server{})
.WithObjects(objects...).Build()` — and the registry is built the way
`internal/agent`'s own tests build one.

```go
package netstate_test

func source(t *testing.T, objects ...client.Object) (netstate.Source, *agent.Registry, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	reg := agent.New(clock.Now, 5*time.Second, clock.now)
	reader := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).
		WithObjects(objects...).Build()
	return netstate.Source{Reader: reader, Agents: reg}, reg, clock
}

func TestBuildDescribesEveryGroupAndServerInTheNamespace(t *testing.T) {
	src, _, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		proxyGroup("ns", "gateway"),
		readyServer("ns", "lobby-a", "lobby", 12, 100),
		readyServer("ns", "lobby-b", "lobby", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(got.GetGroups()) != 2 {
		t.Fatalf("groups = %v, want the ServerGroup and the ProxyGroup", got.GetGroups())
	}
	// Sorted, so this can be asserted as a list rather than a set.
	if got.GetGroups()[0].GetName() != "gateway" ||
		got.GetGroups()[0].GetKind() != agentpb.GroupState_PROXY {
		t.Errorf("groups[0] = %+v, want gateway as a PROXY group", got.GetGroups()[0])
	}
	if got.GetGroups()[1].GetKind() != agentpb.GroupState_EPHEMERAL {
		t.Errorf("lobby kind = %v, want EPHEMERAL", got.GetGroups()[1].GetKind())
	}
	if len(got.GetServers()) != 2 {
		t.Fatalf("servers = %v, want both", got.GetServers())
	}
	if got.GetServers()[0].GetPlayers() != 12 {
		t.Errorf("lobby-a players = %d, want 12", got.GetServers()[0].GetPlayers())
	}
}

func TestBuildIsScopedToOneNamespace(t *testing.T) {
	// The property the whole API rests on -- a plugin sees its own network and
	// nothing else -- and it is one List option away from being wrong.
	src, _, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		readyServer("ns", "lobby-a", "lobby", 0, 100),
		ephemeralGroup("other", "secret"),
		readyServer("other", "secret-a", "secret", 0, 100),
	)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, g := range got.GetGroups() {
		if g.GetName() == "secret" {
			t.Fatal("another namespace's group reached this network's state")
		}
	}
	for _, srv := range got.GetServers() {
		if srv.GetName() == "secret-a" {
			t.Fatal("another namespace's server reached this network's state")
		}
	}
}

func TestBuildCarriesTheRoster(t *testing.T) {
	// Players come from the registry and not the cache: the operator holds
	// them in memory and puts them in no object.
	src, reg, _ := source(t, ephemeralGroup("ns", "lobby"))
	reg.Connect("proxy-a", agent.RoleProxy)
	if err := reg.ReportRoster("proxy-a", "ns", []agent.RosterEntry{
		{UUID: "u-alice", Name: "alice", Server: "lobby-a"},
	}); err != nil {
		t.Fatalf("ReportRoster: %v", err)
	}

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.GetPlayers()) != 1 || got.GetPlayers()[0].GetUuid() != "u-alice" {
		t.Fatalf("players = %v, want alice", got.GetPlayers())
	}
}

func TestBuildSurvivesANetworkWithNoProxyReports(t *testing.T) {
	// An empty roster is a state and not an error. A namespace whose proxies
	// have not reported has no player list, and the API documents that as
	// ordinary rather than as a failure a plugin has to handle.
	src, _, _ := source(t, ephemeralGroup("ns", "lobby"))

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build with no proxy reports: %v", err)
	}
	if len(got.GetPlayers()) != 0 {
		t.Errorf("players = %v, want none", got.GetPlayers())
	}
}

func TestAServersPhaseTravelsAsTheOperatorSpellsIt(t *testing.T) {
	// Not mapped to an enum here. A phase this proto predates has to reach an
	// agent as itself, so the agent can decide it does not know it -- which is
	// what ServerPhase.fromWire in the plugin API is for.
	src, _, _ := source(t,
		ephemeralGroup("ns", "lobby"),
		serverInPhase("ns", "lobby-a", "lobby", "Retiring"),
	)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.GetServers()[0].GetPhase() != "Retiring" {
		t.Errorf("phase = %q, want the operator's own spelling", got.GetServers()[0].GetPhase())
	}
}
```

`ephemeralGroup`, `proxyGroup`, `readyServer` and `serverInPhase` are small
builders this file writes for itself; `fleet_test.go` has ones of the same
shape to copy.

- [ ] **Step 2: Run and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/netstate/`

Expected: the package does not exist.

- [ ] **Step 3: Write the builder**

`Build` lists `ServerGroupList`, `ProxyGroupList` and `ServerList` in the
namespace, maps each onto its proto shape, and asks the registry for the
roster. Sort every slice by name (and the roster by UUID) before returning:
a message built from map iteration would differ between two identical states,
and every consumer of this — a test, a diff, a resync that could have been
skipped — wants two identical states to produce two identical messages.

The group kind comes from the object's own type and, for a `ServerGroup`, from
`spec.type`. A type this build does not recognise maps to `KIND_UNSPECIFIED`
rather than to a guess.

- [ ] **Step 4: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/netstate/ -v`

Expected: PASS.

- [ ] **Step 5: Prove the namespace scope can fail**

Delete the `client.InNamespace(namespace)` option from one of the three Lists,
run `TestBuildIsScopedToOneNamespace`, and confirm it fails. **Restore it.**

This is the assertion the whole API rests on, and a List option is one word.

- [ ] **Step 6: Commit**

```bash
git add internal/netstate
git commit -m "$(cat <<'EOF'
feat(netstate): one builder for the picture both agent kinds get

The shared thing between the two channels is what a network looks like, not
how a message reaches an agent. Two fan-outs calling one builder cannot
disagree about it; two builders eventually would, and the disagreement would
show up as a plugin seeing different answers on a backend and on a proxy --
which is the exact promise this API makes.

Everything is sorted before it is returned. A message built from map
iteration differs between two identical states, and every consumer here wants
the opposite.

Mutation-checked: dropping client.InNamespace from one List makes the scope
test fail. That option is one word and it is what the whole API rests on.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 3: The proxies get it

The smaller half, and first because `Fleet` already works: this adds one
message to an existing snapshot rather than building a delivery path.

**Files:**
- Modify: `internal/proxyreg/fleet.go`
- Test: `internal/proxyreg/fleet_test.go`

**Interfaces:**
- Consumes: `netstate.Source` from Task 2.
- Produces: `Options` gains `State netstate.Source`; `snapshot` appends a
  `NetworkState` message after the `FullSync` and the drains.

- [ ] **Step 1: Write the failing test**

```go
func TestAJoiningProxyIsSentTheNetworkState(t *testing.T) {
	// After the FullSync and any DrainPlayers, and asserted by position:
	// the FullSync must stay first, because ProxyRole opens the ready gate on
	// it and a message before it would be applied by an agent that is not yet
	// routable.
}
```

Read `fleet_test.go`'s existing join test for how it builds a Fleet and reads
the outbox, and follow it.

- [ ] **Step 2: Run and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/proxyreg/ -run TestAJoiningProxy`

- [ ] **Step 3: Append it in `snapshot`**

`snapshot` returns a slice; append the state message last. A build error from
the state is **logged and skipped, not returned**: a namespace whose groups
cannot be listed must still get its `FullSync`, because routing is what keeps
players connected and the mirror is what a plugin reads. Losing the second
must not cost the first.

- [ ] **Step 4: Run the package**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/proxyreg/`

Expected: PASS, including every existing drain and readiness test. This is the
guard that says the message was added and nothing was reordered.

- [ ] **Step 5: Commit**

```bash
git add internal/proxyreg
git commit -m "$(cat <<'EOF'
feat(proxyreg): a proxy's snapshot carries the network state

One message appended to a path that already works, rather than a new
delivery mechanism. It goes last, after the FullSync and the drains, and the
test asserts the position: ProxyRole opens the pod's ready gate on the
FullSync, so anything ahead of it would reach an agent that is not yet
routable.

A state that cannot be built is logged and skipped rather than failing the
snapshot. Routing is what keeps players connected; the mirror is what a
plugin reads. Losing the second must not cost the first.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 4: The server side gets a fan-out at all

`ServerSession` sends two messages when a stream opens and never sends again.
This is the path it has never had.

**Files:**
- Create: `internal/serverreg/registry.go`
- Test: `internal/serverreg/registry_test.go`

**Interfaces:**
- Consumes: `netstate.Source` from Task 2.
- Produces:
  ```go
  type Registry struct{ ... }
  func New(opts Options) *Registry
  func (r *Registry) Join(ctx context.Context, namespace, podUID string) (<-chan *agentpb.OperatorToServer, func(), error)
  func (r *Registry) Resync(ctx context.Context)
  func (r *Registry) Start(ctx context.Context) error
  func (r *Registry) NeedLeaderElection() bool
  ```
  `Options{ State netstate.Source; ResyncInterval time.Duration; OutboxSize int }`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAJoiningServerIsSentTheStateFirst(t *testing.T) {
	// The first message on the channel is the state, built under the same
	// lock the join takes -- so a resync cannot overtake it.
}

func TestASessionThatFallsBehindIsCutRatherThanSilentlyStale(t *testing.T) {
	// Fill the outbox past its bound with resyncs and read nothing. The
	// channel closes. Dropping instead would leave an agent serving a mirror
	// it has no way of knowing is wrong, looking healthy until the next
	// resync -- and proxyreg made the same choice for the same reason.
}

func TestLeavingRemovesTheSession(t *testing.T) {
	// And a resync after it must not panic on a closed channel, which is the
	// bug a double close would be.
}

func TestResyncReachesEverySessionOfEveryNamespace(t *testing.T) {
	// Two namespaces, two sessions, each getting its own namespace's state.
}

func TestOneUnreadableNamespaceDoesNotStopTheOthers(t *testing.T) {
	// The rule Fleet.Resync follows: a namespace whose build fails is skipped
	// with a log line and the tick carries on. The alternative is one broken
	// namespace freezing every mirror in the cluster.
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/serverreg/`

- [ ] **Step 3: Write it**

Mirror `internal/proxyreg/fleet.go`'s session machinery: a mutex, a map keyed
by pod UID, a bounded outbox, `send` that closes the session rather than
dropping a message, and a `Join` that builds the first message and enters the
session **under one lock**, so nothing can overtake it.

Open the file with a comment that names the duplication rather than leaving a
reader to find it:

```go
// The session machinery here is proxyreg.Fleet's, deliberately duplicated
// rather than extracted, and this comment is the argument.
//
// Fleet is eighteen functions and they split in half: nine are this
// machinery, nine are the proxy protocol -- fallback lists, drain orders, the
// lastReady memo, per-group snapshots. A server needs none of those. A
// generic carrying the proxy's per-session hooks would be a parameterised
// version of the harder case serving the easier one, and building it would
// have put a refactor of the drain's delivery path inside a milestone about
// reporting.
//
// What is shared is the picture, and that is shared: both sides call
// netstate.Source.Build, so the two cannot disagree about what a network looks
// like. The eighty lines below are the plumbing, and the two rules that matter
// in them -- build the first message under the lock that broadcasts take, and
// cut a session that falls behind rather than dropping its message -- are
// stated in both places on purpose.
```

- [ ] **Step 4: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/serverreg/ -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/serverreg
git commit -m "$(cat <<'EOF'
feat(serverreg): ServerSession gets a downstream path

Until now a server agent received two messages when its stream opened and
nothing ever again. proxyreg.Fleet -- with Join, broadcast, snapshot and
Resync -- has always been the proxies' alone, so there was no registry, no
per-session channel and no fan-out for the other half of the fleet.

The session machinery is Fleet's, duplicated rather than extracted, and the
package comment is the argument rather than an apology. Fleet splits in half:
nine functions of this machinery and nine of proxy protocol a server needs
none of. A generic carrying the proxy's per-session hooks would have put a
refactor of the drain's delivery path inside a milestone about reporting.

What is shared is shared: both sides build their picture through
netstate.Source, so they cannot disagree about what a network looks like.

A session that falls behind is cut rather than having its message dropped,
which is the choice proxyreg made and for the reason it gives -- an agent
serving a mirror it cannot know is wrong looks healthy the whole time.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 5: Wire it into the session and the binary

**Files:**
- Modify: `internal/agentserver/server.go` (`Options`, `New`, `ServerSession`)
- Modify: `cmd/spawnery-operator/main.go`
- Test: `internal/agentserver/server_envtest_test.go`

**Interfaces:**
- Consumes: `serverreg.Registry` from Task 4.
- Produces: `agentserver.Options` gains `Servers ServerFanout`, an interface
  with the one method `ServerSession` calls, so the package's tests can
  substitute one — the shape `Proxies` already has.

- [ ] **Step 1: Write the failing envtest**

```go
func TestAServerAgentReceivesItsNetworkState(t *testing.T) {
	// Open an authenticated server stream through this file's helper, read
	// past the two opening messages, and find a NetworkState naming this
	// namespace's group.
}
```

Read `server_envtest_test.go`'s existing opening-messages test for the helper
and follow it.

- [ ] **Step 2: Run and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/agentserver/ -run TestAServerAgentReceivesItsNetworkState`

- [ ] **Step 3: Join the registry in `ServerSession`**

The shape `ProxySession` uses: join after the prologue, `defer` the leave, and
select on the outbox alongside the receive channel and the context. **Read
`ProxySession`'s loop and copy its structure**, including what it does when the
outbox closes — a closed outbox means the session was cut and the handler must
end the stream, not spin.

Refuse a nil `Servers` in `New` the way it already refuses a nil `Proxies`: a
nil here would be a panic inside a session minutes after start, in a goroutine.

- [ ] **Step 4: Construct it in the operator**

In `cmd/spawnery-operator/main.go`, build a `serverreg.Registry` with the same
`netstate.Source` the `Fleet` gets, add it to the manager as a `Runnable`, and
pass it to `agentserver.Options`.

- [ ] **Step 5: Run the package and the envtests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/agentserver/`

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make test`

Expected: PASS.

- [ ] **Step 6: Prove the wiring is load-bearing**

`SetupAll`'s own comment warns that an assignment only made in wiring is an
assignment nothing can observe. Delete the `Servers:` line from the operator's
`agentserver.Options`, run `make test`, and confirm something fails. If
nothing does, the envtest in Step 1 is not reaching the real construction and
needs to. **Restore the line.**

- [ ] **Step 7: Commit**

```bash
git add internal/agentserver cmd/spawnery-operator
git commit -m "$(cat <<'EOF'
feat(agentserver): a server agent receives its network state

ServerSession joins the new registry the way ProxySession joins the fleet,
selects on its outbox beside the receive channel, and ends the stream when
that outbox closes -- a closed outbox means the session was cut, and a
handler that spun on it would hold a stream nobody is reading.

New refuses a nil fanout for the reason it already refuses a nil proxy
fleet: a nil here surfaces as a panic inside a session, minutes after start
and in a goroutine, rather than as a startup error.

Mutation-checked at the wiring, because setup.go's own comment warns that an
assignment made only there is one no test can observe: deleting the Servers
line from the operator's options makes the suite fail.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 6: Both real images receive one

**Files:**
- Modify: `cmd/spawnery-stubop/main.go`
- Modify: `hack/agent-test.sh`

- [ ] **Step 1: Make the stub send one**

The stub already sends a `FullSync` to the proxy on demand; add a
`NetworkState` it sends to both a proxy and a server session, with one group
and one server in it, so both phases have something to observe.

- [ ] **Step 2: Assert both images tolerate it**

The agents do not consume it yet — 7b-4 does. What this phase proves is that
the shipped jars **receive and ignore an unknown-to-them message without
ending their session**, which is the property every additive proto change
rests on and which no unit test can see.

Add to the Paper phase and the Velocity phase: after sending the state, assert
the agent's stream is still up and its next periodic report still arrives.

**This is the assertion that matters here.** A jar that ended its session on an
unrecognised field would fail exactly on the first operator that sent one,
which is a fleet-wide outage on an operator upgrade, and it would look like a
network problem.

- [ ] **Step 3: Run it**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make agent-test CONTAINER=podman`

Expected: `agent-test: ok`.

- [ ] **Step 4: Commit**

```bash
git add cmd/spawnery-stubop hack/agent-test.sh
git commit -m "$(cat <<'EOF'
test(agent): both shipped jars survive a message they do not know

Neither agent consumes NetworkState yet, so what these two phases prove is
the property every additive proto change rests on and no unit test can see:
the shipped jars receive an unrecognised message and keep their session.

A jar that ended its stream on one would fail on the first operator that sent
it -- a fleet-wide outage on an operator upgrade, looking like a network
problem the whole time.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

### Task 7: Write down what now reaches an agent

**Files:**
- Modify: `docs/network-boundaries.md`

- [ ] **Step 1: Extend the section 7b-2 added**

That section ends by saying the operator sends the roster to nobody. That
stops being true here, and the sentence must not be left standing.

Replace its last paragraph with what is now true: every agent in a namespace
receives every player in that namespace, by name and UUID, on connect and on
every resync. A compromised game server pod therefore learns who is on the
network — which it could already infer for its own players and could not for
anybody else's. Say plainly that this is a widening, that the namespace
remains the boundary, and that the milestone which lets a *plugin* read it
(7b-4) is the one that decides whether a plugin needs a permission to.

- [ ] **Step 2: Commit**

```bash
git add docs/network-boundaries.md
git commit -m "$(cat <<'EOF'
docs(network-boundaries): the roster now reaches every agent

The section 7b-2 added ended by saying the operator sends this to nobody.
That is no longer true, and a sentence like that left standing is worse than
one never written.

Every agent in a namespace now receives every player in it, by name and UUID.
A compromised game server pod learns who is on the network -- which it could
already infer for its own players and could not for anybody else's. That is a
widening, it is written down as one, and the namespace is still the boundary.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Done when

- [ ] `make test` passes
- [ ] `make agent` passes
- [ ] `make agent-test` passes under podman, with both new phases
- [ ] `make lint` reports nothing
- [ ] Both mutations — Task 2 Step 5 and Task 5 Step 6 — were run, failed as
      predicted, and reverted
- [ ] Nothing was pushed and no tag was created

## What 7b-3 leaves for its successors

- **No agent stores it.** Both jars receive a `NetworkState` and drop it on the
  floor. 7b-4 is what holds it and answers `SpawneryApi` from it, and until
  then this milestone delivers a message nobody reads — which is worth stating
  plainly, because a reviewer will look for the consumer.
- **No permission gates it.** Every plugin on a server that has the agent will
  be able to read every player on the network once 7b-4 lands. Whether that
  needs a permission is 7b-4's decision and is recorded in
  `docs/network-boundaries.md` as open.
- **The resync interval is the operator's, not a plugin's.** A plugin reading
  the mirror sees state up to one resync old. Nothing here offers a way to ask
  for a fresher one, and nothing should until something needs it.
