# Round Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator see a round start and end — a closed door stops counting as capacity without leaving the routing table, and a server that says its round is over reaches a terminal phase of its own instead of being lifted again by kubelet.

**Architecture:** One bool on the agent channel splits into two claims. `accept` keeps its name and loses its side effect: it now only says "do not count my seats", and the operator no longer deregisters on it. A second field, `round_ended`, says "my round is over" — the operator deregisters, stamps `status.roundEndedAt`, and reads that stamp when the pod later terminates to choose between the new phase `Finished` and the existing `Failed`. Ephemeral pods get `restartPolicy: Never` so a finished pod stays finished.

**Tech Stack:** Go 1.x with controller-runtime, protobuf/gRPC for the agent channel, Java for the agent's public API, envtest for controller integration tests.

**Spec:** `docs/superpowers/specs/2026-09-06-round-lifecycle-design.md`

## Global Constraints

- Every Go command runs inside the devshell: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c <cmd>`. A `cd` before `nix develop` breaks the build.
- Commit messages follow Conventional Commits. Commits are gpg-signed; if signing fails, ask for an interactive unlock rather than disabling it.
- `go test ./...` for the whole tree needs `-p 1`, or parallel envtest packages exhaust the machine.
- Regenerating the proto is `make proto`; CRD changes need `make manifests generate`. Both must be run inside the devshell.
- The spec's open point about one message versus two is **resolved here as one message**: `round_ended` is a second field on `AcceptJoinsRequest`. One round-trip, one place a reader finds both claims, and the field comments carry the difference.
- Vocabulary is uniform: the wire says `round_ended`, Java says `endRound()`, the status says `roundEndedAt`, the phase is `Finished`. The routing-table effect is documented as the consequence, never as a second name.
- Proto zero values must mean today's behaviour: `round_ended` unset is a server that stays in the table.
- Persistent groups are untouched: `restartPolicy: Always`, no `Finished`.

---

### Task 1: The `Finished` phase and its decision

**Files:**
- Modify: `internal/phase/phase.go:28-51` (phase constants), `:130-145` (reasons), `:165-175` (Inputs), `:374-400` (Failed branch), `:514-521` (terminal-pod branch)
- Test: `internal/phase/phase_test.go:46` (the `TestDecide` table)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `phase.Finished Phase = "Finished"`; `phase.ReasonRoundFinished = "RoundFinished"`; `phase.ReasonFinishedRetentionElapsed = "FinishedRetentionElapsed"`; `Inputs.RoundEnded bool`; `Inputs.FinishedRetentionElapsed bool`. Later tasks set the two `Inputs` fields and read the phase value.

- [ ] **Step 1: Write the failing tests**

Add these four cases to the `cases` slice in `TestDecide`:

```go
{
	name:    "a terminal pod whose server said its round was over is Finished",
	current: Ready,
	in:      Inputs{PodExists: true, PodTerminal: true, RoundEnded: true},
	want:    Decision{Next: Finished, Deregister: true, Reason: ReasonRoundFinished},
},
{
	name:    "a terminal pod that said nothing still Fails",
	current: Ready,
	in:      Inputs{PodExists: true, PodTerminal: true},
	want:    Decision{Next: Failed, Deregister: true, Reason: ReasonPodTerminal},
},
{
	name:    "a finished server waits for its retention",
	current: Finished,
	in:      Inputs{PodExists: true, PodTerminal: true, RoundEnded: true},
	want:    Decision{Next: Finished, Reason: ReasonRoundFinished},
},
{
	name:    "a finished server is cleaned up once its retention elapses",
	current: Finished,
	in:      Inputs{PodExists: true, PodTerminal: true, RoundEnded: true, FinishedRetentionElapsed: true},
	want:    Decision{Next: Terminating, DeletePod: true, Reason: ReasonFinishedRetentionElapsed},
},
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/phase/ -run TestDecide`

Expected: FAIL — `undefined: Finished`, `undefined: ReasonRoundFinished`, `unknown field RoundEnded`.

- [ ] **Step 3: Add the phase, the reasons and the inputs**

In the phase constants block (after `Failed`):

```go
	// Finished means the server's round is over: it said so, and then its pod
	// stopped. It is terminal like Failed and its group replaces it at once,
	// but it is not a fault -- it is not counted against the backoff, it
	// raises no Degraded, and it is kept for its own short retention rather
	// than the hour a failure gets for diagnosis.
	Finished Phase = "Finished"
```

In the reasons block:

```go
	// ReasonRoundFinished marks a server whose round ended and whose pod then
	// stopped.
	ReasonRoundFinished = "RoundFinished"
	// ReasonFinishedRetentionElapsed marks a finished server that has been
	// kept long enough.
	ReasonFinishedRetentionElapsed = "FinishedRetentionElapsed"
```

In `Inputs`, beside `JoinsClosed`:

```go
	// RoundEnded is set once the server has said its round is over.
	//
	// Read from status.roundEndedAt and not from the registry, which is
	// memory: an operator restart between the pod stopping and the pass that
	// reads this would otherwise turn a finished round into a failure. It is
	// the server's own word, like JoinsClosed, and like that one it is false
	// for a server that never said it.
	RoundEnded bool

	// FinishedRetentionElapsed is true once a Finished server has been kept
	// for its group's finished retention.
	FinishedRetentionElapsed bool
```

- [ ] **Step 4: Add the `Finished` branch and split the terminal-pod branch**

Add a `case Finished:` beside `case Failed:` in `Decide`:

```go
	case Finished:
		if in.DeletionRequested || in.FinishedRetentionElapsed {
			// No drain: the pod is terminal, so its sessions went with it.
			// That is the same reasoning the terminal branch below gives, and
			// it is why this case is shorter than Failed's -- a Finished
			// server is only ever reached through a terminal pod.
			reason := ReasonFinishedRetentionElapsed
			message := "finished retention elapsed"
			if in.DeletionRequested {
				reason, message = ReasonDeletionRequested, "deletion requested for a finished server"
			}
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: reason, Message: message,
			}
		}
		return Decision{
			Next:   Finished,
			Reason: ReasonRoundFinished, Message: "the round is over",
		}
```

Replace the terminal-pod branch at `:514` with:

```go
	if in.PodTerminal {
		// A terminal pod is never drained: the process is already down and its
		// sessions went with it, so there is nobody left to move off.
		//
		// The server's own word decides which terminal phase this is. Not the
		// exit code: a System.exit(0) in a shutdown hook would make a crash
		// read as a win, and the word is the one thing only the server knows.
		if in.RoundEnded {
			return Decision{
				Next: Finished, Deregister: current == Ready,
				Reason: ReasonRoundFinished, Message: "the round is over and the pod stopped",
			}
		}
		return Decision{
			Next: Failed, Deregister: current == Ready,
			Reason: ReasonPodTerminal, Message: "pod reached a terminal phase",
		}
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/phase/`

Expected: PASS, including the pre-existing `TestNoPathBackFromDraining` and `TestNoPathBackFromRetiring`.

- [ ] **Step 6: Commit**

```bash
git add internal/phase/phase.go internal/phase/phase_test.go
git commit -m "feat(phase): a round that ended is Finished, not Failed"
```

---

### Task 2: The status stamp and the finished retention

**Files:**
- Modify: `api/v1alpha1/server_types.go:186` (beside `FailedAt`), `api/v1alpha1/servergroup_types.go:299-303` (beside `FailedRetentionSeconds`), `:454` (beside `FailedRetention()`)
- Test: `api/v1alpha1/servergroup_types_test.go` (create if absent)

**Interfaces:**
- Consumes: nothing.
- Produces: `ServerStatus.RoundEndedAt *metav1.Time` (json `roundEndedAt`); `ServerGroupSpec.FinishedRetentionSeconds int32` (json `finishedRetentionSeconds`, default 300); `func (g *ServerGroup) FinishedRetention() time.Duration`.

- [ ] **Step 1: Write the failing test**

Create `api/v1alpha1/servergroup_types_test.go`:

```go
package v1alpha1

import (
	"testing"
	"time"
)

func TestFinishedRetentionIsSecondsNotTheFailedOne(t *testing.T) {
	g := &ServerGroup{}
	g.Spec.FailedRetentionSeconds = 3600
	g.Spec.FinishedRetentionSeconds = 300

	if got, want := g.FinishedRetention(), 5*time.Minute; got != want {
		t.Errorf("FinishedRetention() = %v, want %v", got, want)
	}
	if got, want := g.FailedRetention(), time.Hour; got != want {
		t.Errorf("FailedRetention() = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./api/v1alpha1/ -run TestFinishedRetention`

Expected: FAIL — `g.Spec.FinishedRetentionSeconds undefined`.

- [ ] **Step 3: Add the fields**

In `ServerStatus`, after `FailedAt`:

```go
	// RoundEndedAt is when the server said its round was over. Nil for one
	// that never did.
	//
	// Stamped while the server is still running, which is what makes the
	// distinction survive an operator restart: the registry that heard the
	// word is memory, and this object is not.
	// +optional
	RoundEndedAt *metav1.Time `json:"roundEndedAt,omitempty"`
```

In `ServerGroupSpec`, after `FailedRetentionSeconds`:

```go
	// FinishedRetentionSeconds is how long a Finished server is kept.
	//
	// Shorter than the failed retention on purpose: that one buys somebody
	// time to look at a fault, and a round that ended as it should is not one.
	// What it buys instead is a window in which the last round is still
	// visible to anybody asking what just happened.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	// +optional
	FinishedRetentionSeconds int32 `json:"finishedRetentionSeconds,omitempty"`
```

After `FailedRetention()`:

```go
// FinishedRetention is how long a Finished server is kept.
func (g *ServerGroup) FinishedRetention() time.Duration {
	return time.Duration(g.Spec.FinishedRetentionSeconds) * time.Second
}
```

- [ ] **Step 4: Regenerate and run**

Run:
```
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make manifests generate
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./api/v1alpha1/
```

Expected: PASS, and `git status` shows regenerated files under `config/crd/bases/` and `charts/spawnery/templates/`.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/ config/crd/bases/ charts/spawnery/
git commit -m "feat(api): a server records when its round ended, and a group how long to keep it"
```

---

### Task 3: The second field on the wire

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto:280-285` (`AcceptJoinsRequest`)
- Modify: `internal/agent/registry.go:74-80` (`Snapshot`), `:141-152` (`entry`), `:396-413` (`ReportAcceptJoins`), `:636-660` (`Lookup`)
- Modify: `internal/agentserver/requests.go:487`
- Test: `internal/agent/registry_test.go:824`

**Interfaces:**
- Consumes: nothing.
- Produces: `agentpb.AcceptJoinsRequest.GetRoundEnded() bool`; `agent.Registry.ReportAcceptJoins(key string, accept, roundEnded bool) error`; `agent.Snapshot.RoundEnded bool`.

- [ ] **Step 1: Write the failing test**

Add to `internal/agent/registry_test.go`:

```go
func TestARoundEndIsRememberedAfterTheStreamDrops(t *testing.T) {
	r := newTestRegistry(t)
	connectServer(t, r, "pod-a", "ns", "srv-a")

	if got := r.Lookup("pod-a").RoundEnded; got {
		t.Error("a server that has said nothing has not ended a round")
	}
	if err := r.ReportAcceptJoins("pod-a", false, true); err != nil {
		t.Fatalf("ReportAcceptJoins: %v", err)
	}
	if got := r.Lookup("pod-a"); !got.RoundEnded || got.AcceptingJoins {
		t.Errorf("after the round ended: %+v", got)
	}

	// The pod stops; the word has to outlive its stream, because the phase
	// that reads it only runs once the pod is terminal.
	r.Disconnect("pod-a")
	if got := r.Lookup("pod-a").RoundEnded; !got {
		t.Error("the round end was forgotten when the stream dropped")
	}
}
```

Adjust the existing calls at `registry_test.go:824`, `:831` and `:844` to the new signature: `ReportAcceptJoins("pod-a", false, false)`, `("pod-a", true, false)`, `("pod-a", false, false)`, and `("proxy-a", false, false)` at `:857`.

- [ ] **Step 2: Run it to verify it fails**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/agent/ -run TestARoundEnd`

Expected: FAIL — too many arguments to `ReportAcceptJoins`.

- [ ] **Step 3: Add the proto field**

In `AcceptJoinsRequest`, after `accept`:

```proto
  // True says the server's round is over: take it out of the routing table
  // and treat the pod stopping after this as an ending rather than a fault.
  //
  // Unset is a server that stays in the table, which is what every agent that
  // predates this field does and what closing the door alone now means. The
  // two fields are two claims: `accept` is about capacity -- do not count my
  // seats -- and this one is about reachability and about how the end of this
  // pod is to be read.
  //
  // It is here rather than in AnnounceRequest because the operator acts on it.
  // What the operator acts on needs a schema, an error path and a version
  // story; what it only carries needs a length bound.
  bool round_ended = 2;
```

- [ ] **Step 4: Regenerate and thread it through**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make proto`

In `registry.go`, add to `Snapshot` after `AcceptingJoins`:

```go
	// RoundEnded is whether this server has said its round is over.
	//
	// False for a pod the registry has never seen, which is the default that
	// cannot surprise anybody: an unknown pod that stops has not been told to
	// count as a finished round.
	RoundEnded bool
```

Add to `entry` beside `joinsClosed`:

```go
	// roundEnded is set once this server has said its round is over. It is
	// never cleared: a round does not restart inside one pod.
	roundEnded bool
```

Replace the body of `ReportAcceptJoins`:

```go
func (r *Registry) ReportAcceptJoins(key string, accept, roundEnded bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if e.role != RoleServer {
		return fmt.Errorf("accept-joins from a %s agent %q", e.role, key)
	}
	e.joinsClosed = !accept
	// Only ever set. A server that ends a round and then reopens its door --
	// which nothing does today -- has still ended that round, and the pod it
	// ended in is the one this entry is about.
	if roundEnded {
		e.roundEnded = true
	}
	return nil
}
```

In `Lookup`, add `RoundEnded: e.roundEnded,` to the populated `Snapshot`. The unknown-pod `Snapshot` above it needs no change: `false` is its zero value and the right answer.

In `internal/agentserver/requests.go:487`:

```go
	if err := s.opts.Agents.ReportAcceptJoins(id.PodUID, req.GetAccept(), req.GetRoundEnded()); err != nil {
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/agent/ ./internal/agentserver/`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add proto/ internal/agentpb/ agent/common/src/proto/java/ internal/agent/ internal/agentserver/
git commit -m "feat(agent): a server can say its round is over"
```

---

### Task 4: The operator stamps the word and reads it back

**Files:**
- Modify: `internal/controller/server_controller.go:726-731` (inputs), `:822` (retention)
- Test: `internal/controller/server_controller_test.go`

**Interfaces:**
- Consumes: `agent.Snapshot.RoundEnded` (Task 3), `ServerStatus.RoundEndedAt` and `ServerGroup.FinishedRetention()` (Task 2), `phase.Inputs.RoundEnded` and `.FinishedRetentionElapsed` (Task 1).
- Produces: `status.roundEndedAt` is written the first time a snapshot reports `RoundEnded`.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/server_controller_test.go`:

```go
func TestTheRoundEndIsStampedWhileTheServerStillRuns(t *testing.T) {
	// The stamp is what survives an operator restart. Taking it only when the
	// pod is already terminal would lose the distinction to the restart this
	// field exists for.
	srv := &spawneryv1alpha1.Server{}
	snap := agent.Snapshot{Known: true, Connected: true, RoundEnded: true}

	changed := stampRoundEnd(srv, snap, time.Unix(1000, 0))

	if !changed || srv.Status.RoundEndedAt == nil {
		t.Fatalf("the round end was not stamped: changed=%v status=%+v", changed, srv.Status)
	}
	first := *srv.Status.RoundEndedAt

	if stampRoundEnd(srv, snap, time.Unix(2000, 0)) {
		t.Error("the stamp moved on a second pass; it must be taken once")
	}
	if !srv.Status.RoundEndedAt.Equal(&first) {
		t.Errorf("the stamp changed: %v then %v", first, *srv.Status.RoundEndedAt)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/controller/ -run TestTheRoundEndIsStamped`

Expected: FAIL — `undefined: stampRoundEnd`.

- [ ] **Step 3: Add the helper and wire the inputs**

Add to `server_controller.go`, beside the other input helpers:

```go
// stampRoundEnd records the first time a server said its round was over, and
// reports whether it wrote anything.
//
// Once, never moved: the field answers "when did this end", and a later pass
// re-reading the same live word must not turn it into "when was this last
// observed".
func stampRoundEnd(srv *spawneryv1alpha1.Server, snap agent.Snapshot, now time.Time) bool {
	if !snap.RoundEnded || srv.Status.RoundEndedAt != nil {
		return false
	}
	srv.Status.RoundEndedAt = &metav1.Time{Time: now}
	return true
}
```

Call it where the other status fields are settled, before `phase.Decide`, and feed the inputs after `in.JoinsClosed`:

```go
	stampRoundEnd(srv, snap, now)
	// The object and not the snapshot: the registry is memory, and this
	// decision has to hold across an operator restart.
	in.RoundEnded = srv.Status.RoundEndedAt != nil
```

Beside the failed-retention line at `:822`:

```go
	if srv.Status.RoundEndedAt != nil {
		in.FinishedRetentionElapsed = now.Sub(srv.Status.RoundEndedAt.Time) >= group.FinishedRetention()
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/controller/ -run TestTheRoundEndIsStamped`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/server_controller.go internal/controller/server_controller_test.go
git commit -m "feat(controller): the round's end is stamped once and read from the object"
```

---

### Task 5: A closed door no longer leaves the routing table

**Files:**
- Modify: `internal/phase/phase.go:568-580` (the Starting branch), `:660-672` (the Ready branch)
- Test: `internal/phase/phase_test.go`

**Interfaces:**
- Consumes: `Inputs.RoundEnded` (Task 1).
- Produces: deregistration follows `RoundEnded`, never `JoinsClosed`.

- [ ] **Step 1: Write the failing tests**

Add to the `TestDecide` table:

```go
{
	name:    "a closed door keeps its place in the routing table",
	current: Ready,
	in:      Inputs{PodExists: true, PodRunning: true, PodReady: true, AgentReady: true, JoinsClosed: true, Registered: true},
	want:    Decision{Next: Ready, Reason: ReasonReadyGatePassed},
},
{
	name:    "a server that has ended its round leaves it",
	current: Ready,
	in:      Inputs{PodExists: true, PodRunning: true, PodReady: true, AgentReady: true, JoinsClosed: true, RoundEnded: true, Registered: true},
	want:    Decision{Next: Ready, Deregister: true, Reason: ReasonRoundFinished},
},
{
	name:    "a server that closed its door on the way up is still registered",
	current: Starting,
	in:      Inputs{PodExists: true, PodRunning: true, PodReady: true, AgentReady: true, JoinsClosed: true},
	want:    Decision{Next: Ready, Register: true, Reason: ReasonReadyGatePassed},
},
```

- [ ] **Step 2: Run them to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/phase/ -run TestDecide`

Expected: FAIL — the first two get `Deregister: true` with `ReasonJoinsClosed`, the third gets `Ready` without `Register`.

- [ ] **Step 3: Make deregistration follow the round, not the door**

In the `Starting` branch, delete the `if in.JoinsClosed { ... }` block entirely: a server that closed its door still belongs in the table, so it registers like any other.

In the `Ready` branch, replace the two door blocks with:

```go
		// The round's end takes a server out of the table; a closed door does
		// not. They were one signal once, and a spectator asking for a running
		// round was answered "no such server" -- deregistration is about
		// whether anybody can reach this server at all, and the door is about
		// whether its seats are capacity.
		//
		// Both directions are conditioned on what the proxies currently have,
		// so this speaks only when something changes. Without that a finished
		// server would be deregistered again on every pass, and every one of
		// those is a broadcast to every proxy in the namespace.
		if in.RoundEnded && in.Registered {
			return Decision{
				Next: Ready, Deregister: true,
				Reason: ReasonRoundFinished, Message: "the round is over",
			}
		}
		if !in.RoundEnded && !in.Registered {
			return Decision{
				Next: Ready, Register: true,
				Reason: ReasonJoinsOpen, Message: "the server is reachable",
			}
		}
```

- [ ] **Step 4: Run the whole phase package**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/phase/`

Expected: PASS. Pre-existing cases asserting deregistration on `JoinsClosed` must be updated to assert it on `RoundEnded`; a case that still expects the old behaviour is the bug this task removes, not a regression.

- [ ] **Step 5: Commit**

```bash
git add internal/phase/phase.go internal/phase/phase_test.go
git commit -m "fix(phase): a closed door stops joins, it does not hide the server"
```

---

### Task 6: Both capacity numbers learn the door

**Files:**
- Modify: `internal/controller/candidates.go:28-73` (`ServerView`), `:524-548` (`AggregateGroup`)
- Modify: `internal/controller/scaling.go:191-225` (`provisionalCapacity`)
- Modify: `internal/controller/servergroup_controller.go:1291-1310` (view construction)
- Test: `internal/controller/candidates_test.go`, `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: `agent.Snapshot.RoundEnded` is not needed here; `snap.AcceptingJoins` already reaches this function.
- Produces: `ServerView.JoinsClosed bool`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/controller/candidates_test.go`:

```go
func TestAClosedDoorIsNotFreeCapacity(t *testing.T) {
	// A running round holds seats nobody can take. Counting them let a group
	// sit at its floor while every server in it was playing.
	views := []ServerView{
		{Name: "a", Phase: phase.Ready, Registered: true, JoinsClosed: true, Players: 2, Slots: 80},
		{Name: "b", Phase: phase.Ready, Registered: true, Players: 0, Slots: 80},
	}

	if got, want := AggregateGroup(views, "").FreeSlots, int32(80); got != want {
		t.Errorf("FreeSlots = %d, want %d — the playing server's seats were counted", got, want)
	}
}
```

Add to `internal/controller/backoff_test.go`:

```go
func TestAFinishedRoundSpendsNoneOfTheBackoffBudget(t *testing.T) {
	// Six consecutive failures end a group's attempts for good. A group that
	// plays six rounds has not failed once, and this is what keeps the two
	// apart -- CountFailures reads the phase, so Finished has to be its own
	// phase for this to hold.
	ended := time.Unix(2000, 0)
	views := []ServerView{
		{Name: "a", Phase: phase.Finished, FailedAt: ended},
		{Name: "b", Phase: phase.Finished, FailedAt: ended},
	}

	count, _ := CountFailures(views, 0, time.Unix(1000, 0), 0)
	if count != 0 {
		t.Errorf("consecutiveFailures = %d, want 0 — finished rounds are not faults", count)
	}
}
```

Add to `internal/controller/scaling_test.go`:

```go
func TestAGroupBuildsARoomWhileItsOnlyServerIsPlaying(t *testing.T) {
	// spareSlots == maxPlayers is a request for one whole free server. A
	// running round cannot be the one.
	in := ScalingInputs{
		MaxPlayers: 80,
		SpareSlots: 80,
		Views: []ServerView{
			{Name: "a", Phase: phase.Ready, Registered: true, JoinsClosed: true, Players: 2, Slots: 80},
		},
		PendingDeletes: map[string]bool{},
	}

	if got := decideSize(in).Create; got < 1 {
		t.Errorf("create = %d, want at least 1 — nobody can join the running round", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/controller/ -run 'TestAClosedDoorIsNotFreeCapacity|TestAGroupBuildsARoom|TestAFinishedRoundSpendsNone'`

Expected: FAIL — `unknown field JoinsClosed`, then `FreeSlots = 158` and `create = 0`.

- [ ] **Step 3: Add the field and teach both functions**

In `ServerView`, after `Registered`:

```go
	// JoinsClosed is whether the server has shut its door: it wants no new
	// players, while going on playing with the ones it has.
	//
	// Beside Registered rather than derived from it. They were the same
	// question while a closed door was also deregistered; now a playing server
	// stays in the table on purpose, so "can anybody be sent here" and "would
	// anybody be sent here" have come apart, and capacity is the second one.
	JoinsClosed bool
```

In `servergroup_controller.go`, in the `ServerView` literal after `Registered:`:

```go
			// The door, from the same snapshot the counts come from.
			JoinsClosed: !snap.AcceptingJoins,
```

In `AggregateGroup`, replace the capacity condition:

```go
		// Not JoinsClosed, and Registered as well: the first is the server
		// saying it wants nobody, the second is the proxies being able to
		// reach it at all. Free seats behind either are not capacity.
		if v.Phase == phase.Ready && v.Registered && !v.JoinsClosed &&
			!staleSpec(v, podHash) && !v.Stale {
```

In `provisionalCapacity`, immediately after the `countsTowardSize` guard:

```go
	// The same door AggregateGroup reads. The two numbers stay two -- one is
	// Ready servers of the current generation, the other counts capacity that
	// has been ordered and not arrived -- but what makes a seat reachable is
	// one question with one answer.
	if v.JoinsClosed {
		return 0
	}
```

- [ ] **Step 4: Run the controller package**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/controller/`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/
git commit -m "fix(scaling): a playing server's empty seats are not capacity"
```

---

### Task 7: An ephemeral pod that stops stays stopped

**Files:**
- Modify: `internal/podspec/server.go:531`
- Test: `internal/podspec/server_test.go`

**Interfaces:**
- Consumes: `spawneryv1alpha1.ServerGroupSpec.Type`.
- Produces: nothing later tasks read.

- [ ] **Step 1: Write the failing test**

Add to `internal/podspec/server_test.go`:

```go
func TestAnEphemeralPodIsNotLiftedAgainByKubelet(t *testing.T) {
	// RestartPolicyAlways restarts the container inside the same pod, and an
	// ephemeral server's /data is an emptyDir -- so the same world comes back
	// and the operator never sees a pod that stopped.
	ephemeral := serverPodForTest(t, spawneryv1alpha1.ServerGroupEphemeral)
	if got, want := ephemeral.Spec.RestartPolicy, corev1.RestartPolicyNever; got != want {
		t.Errorf("ephemeral restartPolicy = %q, want %q", got, want)
	}

	persistent := serverPodForTest(t, spawneryv1alpha1.ServerGroupPersistent)
	if got, want := persistent.Spec.RestartPolicy, corev1.RestartPolicyAlways; got != want {
		t.Errorf("persistent restartPolicy = %q, want %q", got, want)
	}
}
```

If `serverPodForTest` does not exist, write it as a thin wrapper over whatever constructor the existing tests in that file already use, taking the group type and returning the rendered pod.

- [ ] **Step 2: Run it to verify it fails**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/podspec/ -run TestAnEphemeralPod`

Expected: FAIL — ephemeral restartPolicy is `Always`.

- [ ] **Step 3: Choose the policy by group type**

Replace `RestartPolicy: corev1.RestartPolicyAlways,` at `:531` with:

```go
			// Never for an ephemeral server, so a pod that stopped stays
			// stopped and the operator gets to see it. Always would restart
			// the container inside the same pod, over the same emptyDir, which
			// is how a finished round used to come back with its own world.
			//
			// A persistent server keeps Always: its world is a claim, its
			// identity is its ordinal, and a round's end is not a thing that
			// happens to it.
			RestartPolicy: restartPolicy(group),
```

Add beside the other helpers in the file:

```go
func restartPolicy(group *spawneryv1alpha1.ServerGroup) corev1.RestartPolicy {
	if group.Spec.Type == spawneryv1alpha1.ServerGroupPersistent {
		return corev1.RestartPolicyAlways
	}
	return corev1.RestartPolicyNever
}
```

- [ ] **Step 4: Run the package**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/podspec/`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/podspec/
git commit -m "feat(podspec): an ephemeral pod that stops stays stopped"
```

---

### Task 8: The Java API says it

**Files:**
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java:186`
- Modify: the class implementing `SpawneryApi` in `agent/common` (find with `grep -rl "implements SpawneryApi" agent/`)
- Modify: `agent/api/src/test/java/cloud/spawnery/agent/api/FakeApi.java:67`

**Interfaces:**
- Consumes: `AcceptJoinsRequest.round_ended` (Task 3).
- Produces: `CompletionStage<Void> endRound()` on `SpawneryApi`.

- [ ] **Step 1: Write the failing test**

Add to the agent API's test for the accept-joins path (beside the existing `acceptJoins` test; find it with `grep -rl acceptJoins agent/*/src/test`):

```java
  @Test
  void endRoundClosesTheDoorAndSaysTheRoundIsOver() {
    var sent = new ArrayList<AcceptJoinsRequest>();
    var api = apiRecording(sent);

    api.endRound().toCompletableFuture().join();

    assertEquals(1, sent.size());
    assertFalse(sent.get(0).getAccept(), "a finished round takes no new players");
    assertTrue(sent.get(0).getRoundEnded(), "the round's end has to be on the wire");
  }
```

- [ ] **Step 2: Run it to verify it fails**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c ./gradlew :agent:api:test --tests '*endRound*'`

Expected: FAIL to compile — `cannot find symbol: method endRound()`.

- [ ] **Step 3: Add the method**

In `SpawneryApi.java`, after `acceptJoins`:

```java
    /**
     * Says this server's round is over.
     *
     * <p>Two things follow, and only the operator can do either: the server
     * leaves the proxies' routing table, so nobody new arrives at a round that
     * has finished, and the pod stopping after this reads as an ending rather
     * than a fault — its group replaces it without counting a failure.
     *
     * <p>It does not stop the server. Call it when the round is over and let
     * the process end as it always did.
     */
    CompletionStage<Void> endRound();
```

Implement it in the class implementing `SpawneryApi` by sending `AcceptJoinsRequest.newBuilder().setAccept(false).setRoundEnded(true).build()` through the same path `acceptJoins` uses, and add the trivial override to `FakeApi`.

- [ ] **Step 4: Run the agent tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c ./gradlew :agent:api:test :agent:common:test`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent/
git commit -m "feat(agent-api): a plugin can say its round is over"
```

---

### Task 9: The path end to end

**Files:**
- Test: `internal/controller/servergroup_controller_test.go` — the package's envtest file, whose helpers this test reuses

**Interfaces:**
- Consumes: everything above.
- Produces: nothing.

- [ ] **Step 1: Write the failing test**

```go
func TestAFinishedRoundIsReplacedWithoutCountingAFailure(t *testing.T) {
	// The whole path: the server says its round is over, its pod stops, and
	// the group builds a replacement -- without spending a failure from the
	// backoff budget, which is what would eventually stop the group for good.
	ctx, c := envtestClient(t)
	group := newEphemeralGroup(t, ctx, c, 1 /* minReplicas */)

	srv := waitForOneServer(t, ctx, c, group)
	markRoundEnded(t, ctx, c, srv)
	stopPod(t, ctx, c, srv)

	waitFor(t, func() bool {
		got := getServer(t, ctx, c, srv.Name)
		return got.Status.Phase == string(phase.Finished)
	}, "the server never reached Finished")

	replacement := waitForServerOtherThan(t, ctx, c, group, srv.Name)
	if replacement.Name == "" {
		t.Fatal("the group did not replace the finished server")
	}
	if got := getGroup(t, ctx, c, group.Name).Status.ConsecutiveFailures; got != 0 {
		t.Errorf("consecutiveFailures = %d, want 0 — a finished round is not a fault", got)
	}
}
```

Reuse the helpers the package's existing envtest file already defines; write only the ones missing, following that file's style.

- [ ] **Step 2: Run it to verify it fails**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test ./internal/controller/ -run TestAFinishedRoundIsReplaced -count=1`

Expected: FAIL before the tasks above are all in; PASS after. Note the envtest control plane is shared and registers no cleanup — every object this test creates must be removed in `t.Cleanup`, or a later test's cluster-wide List sees them.

- [ ] **Step 3: Run the whole suite**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c go test -p 1 ./...`

Expected: PASS. `-p 1` is required; parallel envtest packages exhaust the machine.

- [ ] **Step 4: Commit**

```bash
git add internal/controller/
git commit -m "test(controller): a finished round is replaced and costs no failure"
```

---

### Task 10: Release notes and version bump

**Files:**
- Modify: `flake.nix:248` (`imageVersion`), `:327` (`operatorVersion`), `charts/spawnery/Chart.yaml`, `charts/spawnery/README.md`, `README.md`
- Modify: `docs/upgrading.md`

**Interfaces:**
- Consumes: everything above.
- Produces: nothing.

- [ ] **Step 1: Write the upgrade note**

Add to `docs/upgrading.md`, in the section for this release:

```markdown
## A closed door no longer hides a server

`acceptJoins(false)` used to do two things: stop counting the server's seats
as capacity, and take it out of the proxies' routing table. It now does only
the first. A server that has closed its door stays reachable, which is what
lets a spectator into a running round and a selector click land on one.

This applies to every agent, including ones built before this release: the
meaning of the field they already send has narrowed. Nothing else changes for
them — a server that never sends the new `round_ended` stays in the table
exactly as it does today.

To take a server out of the table, say the round is over: `endRound()` in the
Java API, `round_ended` on the wire. A pod that stops after that reaches the
new phase `Finished` instead of `Failed`, is replaced at once, and costs its
group no failure from the backoff budget.

Ephemeral server pods now carry `restartPolicy: Never`. A pod that stops stays
stopped, which is what lets the operator see a round end at all. Persistent
groups are unchanged.
```

- [ ] **Step 2: Bump the three versions**

Follow the release convention this repository already uses: the chart moves because the CRDs changed (`Server.status.roundEndedAt`, `ServerGroup.spec.finishedRetentionSeconds`), `operatorVersion` moves because the reconciler changed, and `imageVersion` moves because the agent's Java API gained a method.

- [ ] **Step 3: Verify the pins agree**

Run: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make test`

Expected: PASS, including `toolchain-lint` and `chart-lint`.

- [ ] **Step 4: Commit**

```bash
git add flake.nix charts/ README.md docs/upgrading.md
# The three numbers are at flake.nix:248 (imageVersion, 0.2.27 today),
# flake.nix:327 (operatorVersion, 0.2.26) and Chart.yaml (0.2.26). All three
# move; the release takes imageVersion's new number as its name.
git commit -m "chore: 0.2.28, a round the operator can see start and end"
```

---

## Outside this repository

Two changes live in the network's own repositories and are not tasks here.
They are what makes the fix visible to a player, so neither is optional.

- **`arcadia`** — `SpawneryCloudServiceProvider.setEnding()` calls the new
  `endRound()`. Its existing comment, "No door() call: a round that is over is
  not one anybody joins", becomes true at that point; today it deregisters
  nothing. `setIngame(true)` keeps its `door(false)` unchanged — that call is
  now exactly and only what it always claimed to be.
- **`configs`** — the `spawnery` group manifests pin the image version. They
  must reach the release from Task 10 before any plugin built against
  `endRound()` is deployed, or the method is missing at runtime.

## Verification in a real network

- The **round end** can be checked in cadev: end a round, watch a new pod
  arrive with a fresh world. `maxReplicas: 1` does not get in the way.
- The **scale-up** cannot. cadev renders every group with `maxReplicas: 1`,
  because every server of a group mounts the same files claim and a Minecraft
  level can be opened by exactly one server. Checking it needs paulwtf, or
  cadev needs an answer of its own for a group holding more than one server.
  That question is open and belongs to whoever picks up cadev next.
