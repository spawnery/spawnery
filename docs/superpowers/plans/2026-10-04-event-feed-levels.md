# Event feed levels and a transfer warning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The `/cloud` event feed shows each player one of four levels (`minimal` by default, `normal`, `verbose`, `off`), kept in LuckPerms meta where LuckPerms is loaded. On-demand members show under short names that link to `/cloud info`. Private events reach proxies only. The command replies get shorter. A player about to be force-transferred off a leaving proxy is warned 10 seconds ahead.

**Architecture:** The operator still sends one event per transition. It gains a single `ServerStopped` event at each server's end, and a `private` flag that only routes the event (`cloudevent.Private`, `cloudevent.Fanout`); the flag never goes on the wire. The agent maps each kind to a row of the spec's table (`EventLevels.kt`) and renders a window of events once per level (`coalesce(events, level, groupKind)`). Each player's level comes from `FeedLevels`, which reads LuckPerms' cached meta when LuckPerms is loaded and the agent's memory otherwise. The transfer warning is a new `TransferPolicy.warnings` beside `forced`, run first in `Transfers.pass`.

**Tech Stack:** Go (controller-runtime, envtest), protobuf (comment only), Kotlin (common, Paper and Velocity agents, Brigadier, MiniMessage strings, JUnit 5), Adventure, LuckPerms API 5.5, Nix.

**Spec:** `docs/superpowers/specs/2026-10-04-event-feed-levels-design.md`

## Global Constraints

- Work in the worktree `/home/paul/git/spawnery-feed`, branch `feat/event-feed`. It is based on `feat/cloud-commands` (PR #98), not on master. Read code from the worktree, not from `/home/paul/git/spawnery`.
- The branch does not contain master's #97 (transfer pacing: `Transfers.myTurn`, `Traveller.address`, `forced(..., admit)`). Do not merge master in. When the branch is rebased after #98 merges, resolve `Transfers.pass` by keeping master's pacing and running Task 7's warning loop before `if (!myTurn(picture, now)) return`, and keep both new `Traveller` members.
- Every build and test command runs in the dev shell, with the flake as an argument and never after a `cd`: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery-feed -c <cmd>`, started from the worktree root. Below, `NIX` stands for `nix --extra-experimental-features 'nix-command flakes'`.
- Check `hostname` once. On `paul-desktop` run tests without throttling. On the development VM pass `-p 1` to any `go test` that covers more than one envtest package.
- To read master quickly, `codegraph explore "<symbols or question>"` from `/home/paul/git/spawnery` returns verbatim source. For anything #98 changed (`agent/common` `CloudCommand.kt`, `Execute.kt`, `ListLines.kt`, `StatusLines.kt`, `internal/agentserver`) read the worktree directly.
- Nix builds read the git index: `git add` every new, changed or deleted file before `NIX build .#agents`. An untracked file does not exist for Nix.
- Kotlin and Java tests run only through `NIX build /home/paul/git/spawnery-feed#agents --no-link -L` (all JUnit suites). A failing test fails the build, and the log names it.
- Generated files are committed: after the `.proto` comment change run `NIX develop /home/paul/git/spawnery-feed -c make proto` and commit `internal/agentpb/` and `agent/common/src/proto/java/`.
- Names, exactly: event reason `ServerStopped`; LuckPerms meta key `spawnery-feed`; level words `minimal`, `normal`, `verbose`, `off`, plus `on` meaning `minimal`; translation key `spawnery.transfer.warning`, fallback `You will be reconnected in %s seconds.`; warning lead 10 s (`10_000` ms); a key is shortened when longer than 6 characters, to its first 6.
- The wire is unchanged: no new proto field. The privacy flag only decides which sessions an event is sent to.
- No version bump. The minor release after 0.19.0 is its own PR.
- Comments follow the user's rule: none unless a reader cannot derive it from the code beside it. Everything written is English. Prose that people read (docs, reply strings, commit bodies) goes through the `humanizer` skill in embedded mode.
- Commits are Conventional Commits with a scope (`feat(agent): …`), body wrapped at 72 columns, ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Commits are gpg-signed; on `paul-desktop` the passphrase dialog opens as a window, so just commit.
- New Go files and the new `LuckPermsFeedLevels.kt` carry the repository's licence header with `Copyright paul_wtf.`, as every Go file and `LuckPermsContexts.kt` do. The other new Kotlin files follow their neighbours in `agent/common` and `agent/velocity`, which have none.

## Review Focus

1. **A LuckPerms meta value written by hand.** `/lp user X meta set spawnery-feed " Normal"`, `VERBOSE`, `normal\n`, `loud`, or an empty string. Expected: case and surrounding whitespace are ignored, `on` reads as `minimal`, anything unreadable reads as `minimal`, and nothing throws into the feed tick. Pinned in Task 5 (`FeedLevelsTest`).
2. **Names at the edges of shortening.** A key of exactly 6 characters, a key shorter than 6, a group event whose subject is the group itself, a subject that does not start with `<group>-`, and a name holding a character a quoted MiniMessage argument cannot carry (`'`). Expected: only an on-demand member with a key longer than 6 is shortened. A name outside `[a-z0-9.-]` gets no hover or click and cannot break the line's markup. Pinned in Task 4.
3. **Collapsing when one window mixes rows.** Two `PodCreated` and one `ReadyGatePassed` in `lobby` at `minimal`. Expected: `[+] 2 lobby`, so the hidden ready event neither counts nor shows. At `normal` the same window gives two lines. Warnings never collapse and always come first. Pinned in Task 6.
4. **A warning with an empty note or an empty kind.** Expected: no line ends in a bare colon. `verbose` falls back to the short reason, and an empty kind reads `warning`. Pinned in Task 6.
5. **`forceAfterSeconds` below the 10-second lead, and 0.** Expected: the warning goes out on the first pass in which the proxy sees itself leaving, before that same pass transfers anyone. Seconds are rounded up and never shown as 0. A player whose door opens after the deadline is warned and moved in one pass. Pinned in Task 7.

---

### Task 1: One event at every server's end

The spec asks for the single event every server emits at its end. No such event exists today. A server's last phase change into `Terminating` carries one of a dozen reasons (`Drained`, `RetentionElapsed`, `ForceStopped`, …), and `PodDeleted` is missing when the pod vanished on its own. The finalizer coming off is the one place every server passes exactly once, so the event is recorded there.

**Files:**
- Modify: `internal/controller/server_controller.go` (constant after `ReasonPodNameConflict`, line 52; finalizer removal, lines 341-348)
- Test: `internal/controller/server_controller_test.go` (new test after `TestDeletionDrainsBeforeThePodIsDeleted`, which ends at line 1019)

**Interfaces:**
- Produces: constant `ReasonServerStopped = "ServerStopped"` (package `controller`); event `Normal ServerStopped "server <name> stopped"` regarding the `Server`, recorded once, after the update that removes `spawnery.cloud/drain`.

- [ ] **Step 1: Write the failing test** in `internal/controller/server_controller_test.go`, after `TestDeletionDrainsBeforeThePodIsDeleted`:

```go
func TestAServerRecordsServerStoppedOnceAtItsEnd(t *testing.T) {
	f := newFixture(t)
	rec := newRecorder()
	f.reconc.Recorder = rec
	uid := bringUpReady(t, f, "lobby-x7k2")
	if err := f.agents.ReportPlayers(uid, 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if err := f.c.Delete(f.ctx, f.server("lobby-x7k2")); err != nil {
		t.Fatalf("delete Server: %v", err)
	}
	f.reconcile("lobby-x7k2")

	f.clock.Advance(3 * time.Second)
	if err := f.agents.ReportPlayers(uid, 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}
	f.reconcile("lobby-x7k2")
	if _, ok := f.pod("lobby-x7k2"); ok {
		t.Fatal("pod still there after the drain finished")
	}
	seen := drainEvents(rec)
	if containsEvent(seen, ReasonServerStopped) {
		t.Fatal("ServerStopped recorded while the Server still held its finalizer")
	}

	f.reconcile("lobby-x7k2")
	err := f.c.Get(f.ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: f.ns}, &spawneryv1alpha1.Server{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Server still present: %v", err)
	}
	f.reconcile("lobby-x7k2")
	seen = append(seen, drainEvents(rec)...)

	stopped := 0
	for _, e := range seen {
		if eventHasReason(e, ReasonServerStopped) {
			stopped++
		}
	}
	if stopped != 1 {
		t.Errorf("ServerStopped recorded %d times, want exactly once: %v", stopped, seen)
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `NIX develop /home/paul/git/spawnery-feed -c go test ./internal/controller/ -run TestAServerRecordsServerStoppedOnceAtItsEnd -count=1`
Expected: build failure, `undefined: ReasonServerStopped`.

- [ ] **Step 3: Implement.** In `internal/controller/server_controller.go`, after `const ReasonPodNameConflict = "PodNameConflict"`:

```go

// ReasonServerStopped is the one event every Server records at its end, whatever ended it.
const ReasonServerStopped = "ServerStopped"
```

Replace the finalizer removal inside `if decision.Next == phase.Terminating && !podFound {`:

```go
		if !srv.DeletionTimestamp.IsZero() {
			srv.Finalizers = slices.DeleteFunc(srv.Finalizers, func(f string) bool { return f == ServerFinalizer })
			if err := r.Update(ctx, srv); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			r.Recorder.Eventf(srv, nil, corev1.EventTypeNormal, ReasonServerStopped, actionDeletePod,
				"server %s stopped", srv.Name)
			return ctrl.Result{}, nil
		}
```

`persistedServer` is not used here because it would turn a NotFound into success, and then record a second `ServerStopped` for an object another pass had already released.

- [ ] **Step 4: Run it and the package**

Run: `NIX develop /home/paul/git/spawnery-feed -c go test ./internal/controller/ -run 'TestAServerRecordsServerStoppedOnceAtItsEnd|TestDeletion|TestDeletingA|TestAServerDeletedUnderAPass' -count=1`
Expected: `ok`.
Run: `NIX develop /home/paul/git/spawnery-feed -c go test ./internal/controller/ -count=1`
Expected: `ok` (about 85 s). A test that asserts an exact event list now sees one more `ServerStopped` at the end; extend its expectation and do not suppress the event.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/server_controller.go internal/controller/server_controller_test.go
git commit -m "feat(controller): record ServerStopped once at every server's end"
```

Body: no existing event marks a server's end exactly once; the feed needs one to say a server is gone.

---

### Task 2: Private events reach proxies only

**Files:**
- Modify: `internal/cloudevent/derive.go` (new func after `Derive`)
- Modify: `internal/cloudevent/recorder.go` (`Sink`, lines 30-32; `Eventf`, line 62-64)
- Create: `internal/cloudevent/fanout.go`
- Modify: `cmd/spawnery-operator/main.go` (line 411; delete `bothFanouts`, lines 455-465; imports)
- Modify: `internal/serverreg/registry.go:197-199`, `internal/proxyreg/fleet.go:302-304` (doc comments only)
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto` (comment on `NetworkState.players`, lines 615-618)
- Test: `internal/cloudevent/derive_test.go`, `internal/cloudevent/recorder_test.go`, create `internal/cloudevent/fanout_test.go`
- Regenerate: `internal/agentpb/`, `agent/common/src/proto/java/`

**Interfaces:**
- Produces: `func Private(regarding runtime.Object) bool`; `type Sink interface { Publish(namespace string, ev *agentpb.CloudEvent, private bool) }`; `type Publisher interface { Publish(namespace string, ev *agentpb.CloudEvent) }`; `type Fanout struct { Backends, Proxies Publisher }` implementing `Sink`. `*serverreg.Registry` and `*proxyreg.Fleet` already satisfy `Publisher`, so their method does not change.

Private covers an on-demand `ServerGroup` as well as a `Server` with a key. The group's own events name members in their notes (`ServerCreated`: "created server challenge-3f2b1c9a"), and the backends' network picture leaves the group out too.

- [ ] **Step 1: Write the failing tests.**

`internal/cloudevent/derive_test.go`, at the end:

```go
func TestPrivateMarksOnDemandMembersAndTheirGroups(t *testing.T) {
	member := aServer()
	member.Name, member.Spec.GroupRef.Name, member.Spec.Key = "challenge-3f2b1c9a", "challenge", "3f2b1c9a"
	onDemand := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "challenge", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerGroupSpec{Type: spawneryv1alpha1.ServerGroupOnDemand},
	}
	ephemeral := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerGroupSpec{Type: spawneryv1alpha1.ServerGroupEphemeral},
	}
	proxyPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "gateway-abc", Namespace: "minecraft",
		Labels: map[string]string{podspec.LabelRole: podspec.RoleProxy, podspec.LabelGroup: "gateway"},
	}}

	for name, tc := range map[string]struct {
		obj  runtime.Object
		want bool
	}{
		"an on-demand member":  {member, true},
		"an on-demand group":   {onDemand, true},
		"an ephemeral server":  {aServer(), false},
		"an ephemeral group":   {ephemeral, false},
		"a proxy pod":          {proxyPod, false},
		"nothing at all":       {nil, false},
	} {
		if got := Private(tc.obj); got != tc.want {
			t.Errorf("%s: Private = %v, want %v", name, got, tc.want)
		}
	}
}
```

Add `"k8s.io/apimachinery/pkg/runtime"` to the file's imports.

`internal/cloudevent/recorder_test.go`: replace `fakeSink` with

```go
type fakeSink struct {
	namespaces []string
	events     []*agentpb.CloudEvent
	private    []bool
}

func (f *fakeSink) Publish(namespace string, ev *agentpb.CloudEvent, private bool) {
	f.namespaces = append(f.namespaces, namespace)
	f.events = append(f.events, ev)
	f.private = append(f.private, private)
}
```

and add

```go
func TestTheRecorderTellsTheSinkWhichEventsArePrivate(t *testing.T) {
	inner, sink := &fakeRecorder{}, &fakeSink{}
	r := Recorder{Inner: inner, Sink: sink}
	member := aServer()
	member.Spec.Key = "3f2b1c9a"

	r.Eventf(aServer(), nil, corev1.EventTypeNormal, "PodCreated", "CreatePod", "created pod %s", "lobby-a3f9")
	r.Eventf(member, nil, corev1.EventTypeNormal, "PodCreated", "CreatePod", "created pod %s", "x")

	if len(sink.private) != 2 || sink.private[0] || !sink.private[1] {
		t.Fatalf("private = %v, want [false true]", sink.private)
	}
}
```

Create `internal/cloudevent/fanout_test.go` (licence header as in `derive.go`):

```go
package cloudevent

import (
	"testing"

	"github.com/spawnery/spawnery/internal/agentpb"
)

type recordingPublisher struct{ subjects []string }

func (r *recordingPublisher) Publish(_ string, ev *agentpb.CloudEvent) {
	r.subjects = append(r.subjects, ev.GetSubject())
}

func TestAPrivateEventReachesTheProxiesAndNoBackend(t *testing.T) {
	backends, proxies := &recordingPublisher{}, &recordingPublisher{}
	f := Fanout{Backends: backends, Proxies: proxies}

	f.Publish("minecraft", &agentpb.CloudEvent{Kind: "PodCreated", Subject: "challenge-3f2b1c9a"}, true)
	f.Publish("minecraft", &agentpb.CloudEvent{Kind: "PodCreated", Subject: "lobby-a3f9"}, false)

	if len(backends.subjects) != 1 || backends.subjects[0] != "lobby-a3f9" {
		t.Errorf("backends got %v, want only lobby-a3f9", backends.subjects)
	}
	if len(proxies.subjects) != 2 {
		t.Errorf("proxies got %v, want both events", proxies.subjects)
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `NIX develop /home/paul/git/spawnery-feed -c go test ./internal/cloudevent/ -count=1`
Expected: build failure, `undefined: Private` and `undefined: Fanout`.

- [ ] **Step 3: Implement.**

`internal/cloudevent/derive.go`, after `Derive`:

```go
// Private reports whether an event may reach proxies only. An on-demand
// member is named by its own events and by its group's, and the backends'
// network picture leaves both out.
func Private(regarding runtime.Object) bool {
	switch o := regarding.(type) {
	case *spawneryv1alpha1.Server:
		return o.Spec.Key != ""
	case *spawneryv1alpha1.ServerGroup:
		return o.IsOnDemand()
	}
	return false
}
```

`internal/cloudevent/recorder.go`: the `Sink` interface becomes

```go
type Sink interface {
	Publish(namespace string, ev *agentpb.CloudEvent, private bool)
}
```

and the publish in `Eventf` becomes `r.Sink.Publish(namespace, ev, Private(regarding))`.

Create `internal/cloudevent/fanout.go` (licence header):

```go
package cloudevent

import "github.com/spawnery/spawnery/internal/agentpb"

// Publisher delivers an event to the sessions of one kind of agent.
type Publisher interface {
	Publish(namespace string, ev *agentpb.CloudEvent)
}

// Fanout sends an event to the backends and to the proxies, since an
// administrator may be on either; a private one goes to the proxies only.
type Fanout struct {
	Backends Publisher
	Proxies  Publisher
}

func (f Fanout) Publish(namespace string, ev *agentpb.CloudEvent, private bool) {
	if !private {
		f.Backends.Publish(namespace, ev)
	}
	f.Proxies.Publish(namespace, ev)
}
```

`cmd/spawnery-operator/main.go`: `Events: cloudevent.Fanout{Backends: servers, Proxies: proxies},`. Delete the `bothFanouts` type and its method. Add the import `"github.com/spawnery/spawnery/internal/cloudevent"` and remove `"github.com/spawnery/spawnery/internal/agentpb"`, which nothing else in the file uses.

In `internal/serverreg/registry.go` and `internal/proxyreg/fleet.go`, change "It implements cloudevent.Sink" to "It implements cloudevent.Publisher" in the `Publish` doc comment.

`proto/spawnery/agent/v1alpha1/agent.proto`, in the comment on `NetworkState.players`, replace the sentence `CloudEvent can still name a private member to a backend.` (and its leading `A`) with `A CloudEvent about either reaches proxies only.`

- [ ] **Step 4: Regenerate and run**

Run: `NIX develop /home/paul/git/spawnery-feed -c make proto`
Run: `NIX develop /home/paul/git/spawnery-feed -c go test ./internal/cloudevent/ ./internal/serverreg/ ./internal/proxyreg/ ./cmd/spawnery-operator/ ./internal/agentpb/ -count=1`
Expected: `ok` for each. `git status` shows the regenerated comment in `internal/agentpb/agent.pb.go` and under `agent/common/src/proto/java/`.

- [ ] **Step 5: Commit**

```bash
git add internal/cloudevent cmd/spawnery-operator/main.go internal/serverreg/registry.go internal/proxyreg/fleet.go proto internal/agentpb agent/common/src/proto/java
git commit -m "feat(cloudevent): send on-demand events to proxies only"
```

Body: backends' plugins no longer learn private servers from the event stream, matching the network picture; the flag routes and never goes on the wire.

---

### Task 3: The level model and the mapping table

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/FeedLevel.kt`
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/EventLevels.kt`
- Delete: `agent/common/src/main/kotlin/cloud/spawnery/agent/EventDirection.kt`, `agent/common/src/test/kotlin/cloud/spawnery/agent/EventDirectionTest.kt`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudFeed.kt` (its `sign` uses `direction`; a stopgap until Task 6)
- Modify: `internal/cloudevent/kotlin_agreement_test.go` (whole test function and `reasonsInThisOperator`)
- Test: create `agent/common/src/test/kotlin/cloud/spawnery/agent/EventLevelsTest.kt`, `agent/common/src/test/kotlin/cloud/spawnery/agent/FeedLevelTest.kt`

**Interfaces:**
- Produces: `enum class FeedLevel { OFF, MINIMAL, NORMAL, VERBOSE }` with `val word: String` and `companion fun parse(value: String?): FeedLevel?` and `fun of(value: String?): FeedLevel`; `internal enum class Row { CREATED, ARRIVED, READY, LEAVING, GONE, OTHER }`; `internal fun row(kind: String): Row`; `internal fun isWarning(event: CloudEvent): Boolean`; `internal fun shortReason(kind: String): String`; `internal fun words(kind: String): String`.

The rows, taken from the operator's code:

| Row | Kinds | Where recorded |
|---|---|---|
| CREATED | `PodCreated` | `server_controller.go:291`, on the Server |
| ARRIVED | `ProxyStarted` | `proxygroup_controller.go:953`, on the proxy pod, once, when it first passes its ready gate. A proxy has no earlier event. |
| READY | `ReadyGatePassed` | phase change on the Server |
| LEAVING | `Retiring`, `DeletionRequested`, `DrainingBeforeCleanup`, `RoundFinished`, `ForceStopped` (phase changes); `ProxyRetiring` (proxy pod) | |
| GONE | `ServerStopped` (Task 1), `ProxyStopped` (`proxygroup_controller.go:764`) | |
| OTHER | everything else, e.g. the group-level `ServerCreated`, `ServerRetiring`, `ServerRemoved`, `PodRunning`, `PodPending`, `JoinsOpen` | |

Warnings: every event the operator records as `Warning`, plus these phase changes that it records as `Normal` although they are failures: `StartupTimeout`, `Flapping`, `PodLost`, `PodNeverCreated`, `PodTerminal`, `DrainTimeout`. `ForceStopped` stays in LEAVING because a force-stop of a running pod already records the Warning `PodKilled` ("killed").

- [ ] **Step 1: Write the failing tests.**

`agent/common/src/test/kotlin/cloud/spawnery/agent/FeedLevelTest.kt`:

```kotlin
package cloud.spawnery.agent

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNull

class FeedLevelTest {
    @Test
    fun `the four words and on are read whatever their case and spacing`() {
        assertEquals(FeedLevel.NORMAL, FeedLevel.parse(" Normal"))
        assertEquals(FeedLevel.VERBOSE, FeedLevel.parse("VERBOSE"))
        assertEquals(FeedLevel.OFF, FeedLevel.parse("off\n"))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.parse("minimal"))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.parse("on"))
    }

    @Test
    fun `no value and an unreadable one both mean minimal`() {
        assertNull(FeedLevel.parse("loud"))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.of(null))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.of(""))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.of("loud"))
    }

    @Test
    fun `a level's word is what parse reads back`() {
        for (level in FeedLevel.entries) assertEquals(level, FeedLevel.parse(level.word))
    }
}
```

`agent/common/src/test/kotlin/cloud/spawnery/agent/EventLevelsTest.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertTrue

private fun event(kind: String, warning: Boolean = false): CloudEvent =
    CloudEvent.newBuilder().setKind(kind).setSubject("lobby-a").setGroup("lobby").setWarning(warning).build()

class EventLevelsTest {
    @Test
    fun `a server's start, ready gate, leaving and end each have a row`() {
        assertEquals(Row.CREATED, row("PodCreated"))
        assertEquals(Row.READY, row("ReadyGatePassed"))
        assertEquals(Row.LEAVING, row("Retiring"))
        assertEquals(Row.LEAVING, row("DeletionRequested"))
        assertEquals(Row.LEAVING, row("RoundFinished"))
        assertEquals(Row.GONE, row("ServerStopped"))
    }

    @Test
    fun `a proxy's start, leaving and end each have a row`() {
        assertEquals(Row.ARRIVED, row("ProxyStarted"))
        assertEquals(Row.LEAVING, row("ProxyRetiring"))
        assertEquals(Row.GONE, row("ProxyStopped"))
    }

    @Test
    fun `a group's own events and kinds this agent does not know are other`() {
        assertEquals(Row.OTHER, row("ServerCreated"))
        assertEquals(Row.OTHER, row("PodPending"))
        assertEquals(Row.OTHER, row("SomethingAddedInALaterRelease"))
    }

    @Test
    fun `a failure the operator records as Normal is still a warning`() {
        assertTrue(isWarning(event("StartupTimeout")))
        assertTrue(isWarning(event("PodLost")))
        assertTrue(isWarning(event("AnythingElse", warning = true)))
        assertFalse(isWarning(event("PodCreated")))
    }

    @Test
    fun `a known warning reads from the table`() {
        assertEquals("did not start in time", shortReason("StartupTimeout"))
        assertEquals("pod refused", shortReason("ServerPodRejected"))
        assertEquals("killed", shortReason("PodKilled"))
    }

    @Test
    fun `an unknown warning is read from its name, so none is dropped`() {
        assertEquals("pod name conflict", shortReason("PodNameConflict"))
        assertEquals("tls handshake failed", shortReason("TLSHandshakeFailed"))
        assertEquals("warning", shortReason(""))
    }
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: compile failure, `Unresolved reference: FeedLevel` and `Unresolved reference: Row`.

- [ ] **Step 3: Implement.**

`agent/common/src/main/kotlin/cloud/spawnery/agent/FeedLevel.kt`:

```kotlin
package cloud.spawnery.agent

enum class FeedLevel {
    OFF,
    MINIMAL,
    NORMAL,
    VERBOSE,
    ;

    val word: String get() = name.lowercase()

    companion object {
        /** `on` is what the command took before there were levels. */
        fun parse(value: String?): FeedLevel? = when (value?.trim()?.lowercase()) {
            "off" -> OFF
            "minimal", "on" -> MINIMAL
            "normal" -> NORMAL
            "verbose" -> VERBOSE
            else -> null
        }

        fun of(value: String?): FeedLevel = parse(value) ?: MINIMAL
    }
}
```

`agent/common/src/main/kotlin/cloud/spawnery/agent/EventLevels.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/**
 * The kinds are the operator's Kubernetes event reasons, passed through as strings;
 * `internal/cloudevent`'s agreement test keeps these names in step with them.
 */
internal enum class Row { CREATED, ARRIVED, READY, LEAVING, GONE, OTHER }

private val CREATED = setOf(
    "PodCreated",
)

/** A proxy records nothing before it first takes connections. */
private val ARRIVED = setOf(
    "ProxyStarted",
)

private val READY = setOf(
    "ReadyGatePassed",
)

private val LEAVING = setOf(
    "DeletionRequested",
    "DrainingBeforeCleanup",
    "ForceStopped",
    "ProxyRetiring",
    "Retiring",
    "RoundFinished",
)

private val GONE = setOf(
    "ProxyStopped",
    "ServerStopped",
)

/** Phase changes the operator records as Normal although they are failures. */
private val FAILURES = setOf(
    "DrainTimeout",
    "Flapping",
    "PodLost",
    "PodNeverCreated",
    "PodTerminal",
    "StartupTimeout",
)

private val SHORT = mapOf(
    "DrainTimeout" to "drain deadline, players cut off",
    "Flapping" to "keeps losing readiness",
    "ForceStopped" to "killed",
    "NamespaceNotBootstrapped" to "namespace not ready",
    "PodKilled" to "killed",
    "PodLost" to "pod vanished",
    "PodNeverCreated" to "pod never appeared",
    "PodTerminal" to "pod exited",
    "ProxyDrainTimeout" to "drain deadline, players cut off",
    "ProxyPodBlocked" to "proxy pod blocked",
    "ReadinessLost" to "lost readiness",
    "ServerClaimRejected" to "volume claim refused",
    "ServerPodRejected" to "pod refused",
    "StartupTimeout" to "did not start in time",
)

/** An unknown kind is [Row.OTHER], so a reason a newer operator adds shows only in verbose. */
internal fun row(kind: String): Row = when (kind) {
    in CREATED -> Row.CREATED
    in ARRIVED -> Row.ARRIVED
    in READY -> Row.READY
    in LEAVING -> Row.LEAVING
    in GONE -> Row.GONE
    else -> Row.OTHER
}

internal fun isWarning(event: CloudEvent): Boolean = event.warning || event.kind in FAILURES

internal fun shortReason(kind: String): String = SHORT[kind] ?: words(kind)

private val WORD_BREAK = Regex("(?<=[a-z0-9])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])")

internal fun words(kind: String): String =
    if (kind.isBlank()) "warning" else kind.replace(WORD_BREAK, " ").lowercase()
```

`CloudFeed.kt` still calls `direction`. Until Task 6 rewrites it, replace its `sign` with:

```kotlin
private fun sign(kind: String): String = when (row(kind)) {
    Row.CREATED, Row.ARRIVED, Row.READY -> Style.marker("+", "green")
    Row.LEAVING, Row.GONE -> Style.marker("-", "gold")
    Row.OTHER -> Style.marker("·", "dark_gray")
}
```

`CloudFeedTest` asserts the old signs for `Terminating` and the like. Change only the kinds in those sign tests: `Terminating` → `Retiring` for the gold minus, `ReadyGatePassed` stays for the green plus, and `PodPending` stays for the neutral dot. Task 6 replaces the whole file.

Delete `EventDirection.kt` and `EventDirectionTest.kt` (`git rm`).

`internal/cloudevent/kotlin_agreement_test.go`: replace `TestTheAgentsDirectionTableNamesReasonsThisOperatorHas` with

```go
// The agent's feed picks each line's row from a table of this operator's event
// reasons, spelled as strings in Kotlin. A renamed reason breaks nothing
// visibly: the agent just shows it in verbose only, forever.
//
// One direction only: the table names what has a row, not every reason.
func TestTheAgentsLevelTableNamesReasonsThisOperatorHas(t *testing.T) {
	const table = "agent/common/src/main/kotlin/cloud/spawnery/agent/EventLevels.kt"
	raw, err := os.ReadFile(testenv.RepoPath(t, table))
	if err != nil {
		t.Fatalf("read the agent's level table: %v", err)
	}

	sets := regexp.MustCompile(`(?s)private val (?:CREATED|ARRIVED|READY|LEAVING|GONE|FAILURES) = setOf\((.*?)\)`).
		FindAllSubmatch(raw, -1)
	if len(sets) != 6 {
		t.Fatalf("found %d of the six `private val … = setOf(...)` blocks in %s. Either they were "+
			"renamed, in which case this test has to follow them, or a row is gone", len(sets), table)
	}
	short := regexp.MustCompile(`(?s)private val SHORT = mapOf\((.*?)\n\)`).FindSubmatch(raw)
	if short == nil {
		t.Fatalf("found no `private val SHORT = mapOf(...)` block in %s", table)
	}

	named := map[string]bool{}
	for _, set := range sets {
		for _, m := range regexp.MustCompile(`"([A-Za-z]+)"`).FindAllSubmatch(set[1], -1) {
			named[string(m[1])] = true
		}
	}
	for _, m := range regexp.MustCompile(`"([A-Za-z]+)" to`).FindAllSubmatch(short[1], -1) {
		named[string(m[1])] = true
	}
	if len(named) == 0 {
		t.Fatal("the table names no reasons at all; a scanner that finds nothing " +
			"passes every assertion after it")
	}

	known := reasonsInThisOperator(t)
	for reason := range named {
		if !known[reason] {
			t.Errorf("the agent's table names %q, which is not an event reason this "+
				"operator records. Either it was renamed here, and the agent now shows it "+
				"in verbose only and says nothing, or the table has a typo. Reasons are "+
				"`Reason… = \"…\"` constants or string literals in an Eventf call under internal/.",
				reason)
		}
	}
}
```

In `reasonsInThisOperator`, scan for a second pattern beside the constant one, inside the same walk:

```go
	literal := regexp.MustCompile(`(?s)Eventf\(\s*[^,()]+,\s*[^,()]+,\s*corev1\.EventType(?:Normal|Warning),\s*"([A-Za-z]+)"`)
```

and after the existing loop over `re.FindAllSubmatch(raw, -1)`:

```go
		for _, m := range literal.FindAllSubmatch(raw, -1) {
			out[string(m[1])] = true
		}
```

The doc comment above the function becomes `// reasonsInThisOperator collects every `Reason… = "…"` constant and every reason spelled as a literal in an Eventf call under internal/.` `PodCreated`, `ProxyStarted`, `ProxyRetiring`, `ProxyStopped`, `PodKilled`, `ProxyPodBlocked` and `ProxyDrainTimeout` exist only as such literals.

- [ ] **Step 4: Run both sides**

Run: `git add -A agent internal && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: the build succeeds.
Run: `NIX develop /home/paul/git/spawnery-feed -c go test ./internal/cloudevent/ -run TestTheAgentsLevelTable -count=1 -v`
Expected: `PASS`. Then misspell one kind in `GONE` (`"ProxyStoped"`), rerun, see `FAIL` naming `"ProxyStoped"`, and put it back.

- [ ] **Step 5: Commit**

```bash
git add -A agent internal/cloudevent/kotlin_agreement_test.go
git commit -m "feat(agent): map event kinds to feed rows and levels"
```

---

### Task 4: Short names, hover and click

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/FeedNames.kt`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/Style.kt` (new function after `name`, line 8)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/NetworkMirror.kt` (new method after `joinRule`, line 107)
- Modify: `agent/common/src/test/kotlin/cloud/spawnery/agent/Plain.kt` (the regex)
- Test: create `agent/common/src/test/kotlin/cloud/spawnery/agent/FeedNamesTest.kt`; add to `StyleTest.kt` and `NetworkMirrorTest.kt`

**Interfaces:**
- Produces: `internal const val KEY_SHOWN: Int = 6`; `internal fun shortName(subject: String, group: String, kind: Group.Kind): String`; `Style.subject(full: String, shown: String = full): String`; `NetworkMirror.groupKind(name: String): Group.Kind` (`UNKNOWN` for a group not in the picture). `plain()` now strips tags that carry arguments (`<hover:…>`, `<click:…>`).

- [ ] **Step 1: Write the failing tests.**

`agent/common/src/test/kotlin/cloud/spawnery/agent/FeedNamesTest.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import kotlin.test.Test
import kotlin.test.assertEquals

class FeedNamesTest {
    @Test
    fun `an on-demand member with a long key shows its group and six characters`() {
        assertEquals("challenge-3f2b1c", shortName("challenge-3f2b1c9a0d4e", "challenge", Group.Kind.ON_DEMAND))
    }

    @Test
    fun `a key of six characters or fewer is shown whole`() {
        assertEquals("challenge-3f2b1c", shortName("challenge-3f2b1c", "challenge", Group.Kind.ON_DEMAND))
        assertEquals("challenge-ab", shortName("challenge-ab", "challenge", Group.Kind.ON_DEMAND))
    }

    @Test
    fun `nothing else is shortened`() {
        assertEquals("lobby-x7k2abcdef", shortName("lobby-x7k2abcdef", "lobby", Group.Kind.EPHEMERAL))
        assertEquals("challenge", shortName("challenge", "challenge", Group.Kind.ON_DEMAND))
        assertEquals("other-3f2b1c9a0d", shortName("other-3f2b1c9a0d", "challenge", Group.Kind.ON_DEMAND))
        assertEquals("challenge-3f2b1c9a", shortName("challenge-3f2b1c9a", "challenge", Group.Kind.UNKNOWN))
    }
}
```

`StyleTest.kt`, new tests:

```kotlin
    @Test
    fun `a subject carries its full name on hover and a click that suggests cloud info`() {
        assertEquals(
            "<hover:show_text:'challenge-3f2b1c9a'><click:suggest_command:'/cloud info challenge-3f2b1c9a'>" +
                "<aqua>challenge-3f2b1c</aqua></click></hover>",
            Style.subject("challenge-3f2b1c9a", "challenge-3f2b1c"),
        )
        assertEquals("challenge-3f2b1c", plain(Style.subject("challenge-3f2b1c9a", "challenge-3f2b1c")))
    }

    @Test
    fun `a name a quoted argument cannot carry gets no hover and no click`() {
        assertEquals("<aqua>it's</aqua>", Style.subject("it's"))
        assertEquals("<aqua>Lobby</aqua>", Style.subject("Lobby"))
    }
```

`NetworkMirrorTest.kt`, new test:

```kotlin
    @Test
    fun `a group's kind is found by name, and a group not in the picture is unknown`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addGroups(GroupState.newBuilder().setName("challenge").setKind(GroupState.Kind.ON_DEMAND))
                .build(),
        )

        assertEquals(Group.Kind.ON_DEMAND, mirror.groupKind("challenge"))
        assertEquals(Group.Kind.UNKNOWN, mirror.groupKind("lobby"))
    }
```

- [ ] **Step 2: Run them and see them fail**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: compile failure, `Unresolved reference: shortName`, `subject`, `groupKind`.

- [ ] **Step 3: Implement.**

`agent/common/src/main/kotlin/cloud/spawnery/agent/FeedNames.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group

/** A UUID as the key would take half a chat line. */
internal const val KEY_SHOWN: Int = 6

/** An on-demand member is named `<group>-<key>` by the operator's `instance.Name`. */
internal fun shortName(subject: String, group: String, kind: Group.Kind): String {
    if (kind != Group.Kind.ON_DEMAND) return subject
    val prefix = "$group-"
    if (!subject.startsWith(prefix)) return subject
    val key = subject.removePrefix(prefix)
    return if (key.length > KEY_SHOWN) prefix + key.take(KEY_SHOWN) else subject
}
```

`Style.kt`, after `name`:

```kotlin
    /** Only a Kubernetes-style name is linked: anything else could close the tag's quoted argument. */
    fun subject(full: String, shown: String = full): String =
        if (!LINKABLE.matches(full)) {
            name(shown)
        } else {
            "<hover:show_text:'$full'><click:suggest_command:'/cloud info $full'>${name(shown)}</click></hover>"
        }

    private val LINKABLE = Regex("[a-z0-9][a-z0-9.-]*")
```

`NetworkMirror.kt`, after `joinRule`:

```kotlin
    fun groupKind(name: String): Group.Kind =
        snapshot.groups.firstOrNull { it.name() == name }?.kind() ?: Group.Kind.UNKNOWN
```

`Plain.kt`: the regex becomes `Regex("(?<!\\\\)</?[a-z_]+(?::[^>]*)?>")`, and the KDoc gains "A tag's arguments, as in `<click:…>`, go with it."

- [ ] **Step 4: Run them**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: the build succeeds.

- [ ] **Step 5: Commit**

```bash
git add -A agent
git commit -m "feat(agent): short names for on-demand members, hover and click"
```

---

### Task 5: Where a player's level lives, and the `events` command

`FeedState` goes away. `FeedLevels` holds every player's level, in LuckPerms meta where LuckPerms is loaded and in memory otherwise. The feed still renders as before in this task; only "who wants it" now means "level is not `off`". Task 6 renders by level.

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/FeedLevels.kt`
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/LuckPermsFeedLevels.kt` (licence header, as `LuckPermsPermissions.kt`)
- Delete: `agent/common/src/main/kotlin/cloud/spawnery/agent/FeedState.kt`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/Feed.kt` (constructor, `wanted`, `deliver`)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudCommand.kt` (parameter `feed: FeedState` line 47; the `events` branch lines 226-239; `setFeed` lines 252-284)
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt` (import line 8, field line 103, lines 191 and 201)
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt` (import line 15, lines 62-63, line 106)
- Test: create `agent/common/src/test/kotlin/cloud/spawnery/agent/FeedLevelsTest.kt` and `agent/common/src/test/kotlin/cloud/spawnery/agent/StandInStore.kt`; modify `FeedTest.kt`, `CloudCommandTest.kt`, `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/ProxyRoleTest.kt:31-38`, `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/ServerRoleTest.kt:23-30`

**Interfaces:**
- Consumes: `FeedLevel` (Task 3).
- Produces: `interface LevelStore { fun available(): Boolean; fun read(player: UUID): String?; fun write(player: UUID, value: String): CompletionStage<*> }`; `class MemoryLevels : LevelStore`; `class FeedLevels(durable: LevelStore?, memory: LevelStore = MemoryLevels())` with `fun level(player: UUID): FeedLevel`, `fun set(player: UUID, level: FeedLevel): CompletionStage<*>`, `fun kept(): Boolean`; `object LuckPermsFeedLevels { const val META_KEY = "spawnery-feed"; fun storeIfPresent(): LevelStore? }`; `Feed(audience: FeedAudience, levels: FeedLevels, clock, windowMillis, format)`; `cloudCommand(api, adapter, levels: FeedLevels, format, proxy)`.

- [ ] **Step 1: Write the failing tests.**

`agent/common/src/test/kotlin/cloud/spawnery/agent/StandInStore.kt`:

```kotlin
package cloud.spawnery.agent

import java.util.UUID
import java.util.concurrent.CompletableFuture
import java.util.concurrent.CompletionStage

/** Stands in for LuckPerms: a value on the player wins over the one its group carries. */
internal class StandInStore(var loaded: Boolean = true) : LevelStore {
    val user = mutableMapOf<UUID, String>()
    val group = mutableMapOf<UUID, String>()
    var failWrites = false
    var failReads = false

    override fun available(): Boolean = loaded

    override fun read(player: UUID): String? {
        if (failReads) throw IllegalStateException("LuckPerms is reloading")
        return user[player] ?: group[player]
    }

    override fun write(player: UUID, value: String): CompletionStage<*> {
        if (failWrites) return CompletableFuture.failedFuture<Unit>(IllegalStateException("storage is read-only"))
        user[player] = value
        return CompletableFuture.completedFuture(Unit)
    }
}
```

`agent/common/src/test/kotlin/cloud/spawnery/agent/FeedLevelsTest.kt`:

```kotlin
package cloud.spawnery.agent

import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNull
import kotlin.test.assertTrue

class FeedLevelsTest {
    private val admin = UUID.nameUUIDFromBytes("admin".toByteArray())
    private val other = UUID.nameUUIDFromBytes("other".toByteArray())

    @Test
    fun `without a value a player is minimal`() {
        assertEquals(FeedLevel.MINIMAL, FeedLevels(null).level(admin))
        assertEquals(FeedLevel.MINIMAL, FeedLevels(StandInStore()).level(admin))
    }

    @Test
    fun `a value the player inherits counts, until the player sets their own`() {
        val store = StandInStore()
        store.group[admin] = "normal"
        val levels = FeedLevels(store)
        assertEquals(FeedLevel.NORMAL, levels.level(admin))

        levels.set(admin, FeedLevel.VERBOSE)
        assertEquals("verbose", store.user[admin])
        assertEquals(FeedLevel.VERBOSE, levels.level(admin))
    }

    @Test
    fun `a value written by hand is read whatever its case and spacing`() {
        val store = StandInStore()
        val levels = FeedLevels(store)
        store.user[admin] = " Normal\n"
        assertEquals(FeedLevel.NORMAL, levels.level(admin))
        store.user[admin] = "loud"
        assertEquals(FeedLevel.MINIMAL, levels.level(admin))
        store.user[admin] = ""
        assertEquals(FeedLevel.MINIMAL, levels.level(admin))
    }

    @Test
    fun `on is stored as minimal`() {
        val store = StandInStore()
        FeedLevels(store).set(admin, FeedLevel.parse("on")!!)
        assertEquals("minimal", store.user[admin])
    }

    @Test
    fun `a store that is not loaded leaves the level to memory`() {
        val store = StandInStore(loaded = false)
        val levels = FeedLevels(store)

        levels.set(admin, FeedLevel.OFF)

        assertEquals(FeedLevel.OFF, levels.level(admin))
        assertTrue(store.user.isEmpty(), "an unloaded store was written: ${store.user}")
        assertFalse(levels.kept())
        assertTrue(FeedLevels(StandInStore()).kept())
        assertFalse(FeedLevels(null).kept())
    }

    @Test
    fun `a store that throws on read means minimal, not a broken feed`() {
        val store = StandInStore()
        store.user[admin] = "verbose"
        store.failReads = true
        assertEquals(FeedLevel.MINIMAL, FeedLevels(store).level(admin))
    }

    @Test
    fun `one player's level is not another's`() {
        val levels = FeedLevels(null)
        levels.set(admin, FeedLevel.OFF)
        assertEquals(FeedLevel.OFF, levels.level(admin))
        assertEquals(FeedLevel.MINIMAL, levels.level(other))
    }

    @Test
    fun `without LuckPerms on the classpath there is no durable store`() {
        assertNull(LuckPermsFeedLevels.storeIfPresent())
    }
}
```

`FeedTest.kt`: replace `private val state = FeedState()` with `private val levels = FeedLevels(null)`, construct `Feed(audience, levels, { now }, format = { format })`, replace `state.optOut(bob)` with `levels.set(bob, FeedLevel.OFF)` and `state.optOut(alice)` with `levels.set(alice, FeedLevel.OFF)`. Rename the first test to `` `a closed window reaches everybody permitted whose level is not off` ``.

`CloudCommandTest.kt`:
- `private val feed = FeedState()` → `private var levels = FeedLevels(null)`; every `cloudCommand(api…, adapter, feed, …)` → `levels`; the two `FeedState()` at lines 1033 and 1061 → `FeedLevels(null)`.
- Replace the six tests from `` `events off tells the player it lasts for this session only` `` through `` `holding only events still opens the root` `` (lines 521-575) with:

```kotlin
    @Test
    fun `events alone shows the player's level, minimal by default`() {
        run("cloud events")

        assertTrue(plain(sent.single()).contains("minimal"), sent.single())
    }

    @Test
    fun `each level word sets that level, and on means minimal`() {
        val player = sourcePlayer!!
        for ((word, want) in listOf(
            "normal" to FeedLevel.NORMAL,
            "verbose" to FeedLevel.VERBOSE,
            "off" to FeedLevel.OFF,
            "minimal" to FeedLevel.MINIMAL,
            "off" to FeedLevel.OFF,
            "on" to FeedLevel.MINIMAL,
        )) {
            run("cloud events $word")
            assertEquals(want, levels.level(player), "after /cloud events $word")
        }
    }

    @Test
    fun `a level kept only in memory says a restart loses it`() {
        run("cloud events normal")

        val line = plain(sent.single())
        assertTrue(line.contains("normal") && line.contains("restart"), line)
    }

    @Test
    fun `a level LuckPerms keeps is saved there and not called temporary`() {
        val store = StandInStore()
        levels = FeedLevels(store)

        run("cloud events verbose")

        assertEquals("verbose", store.user[sourcePlayer!!])
        assertFalse(plain(sent.single()).contains("restart"), sent.single())
    }

    @Test
    fun `a save that fails is reported, not claimed`() {
        val store = StandInStore().apply { failWrites = true }
        levels = FeedLevels(store)

        run("cloud events normal")

        assertTrue(sent.single().startsWith("<red>✘</red> "), sent.single())
        assertTrue(sent.single().contains("read-only"), sent.single())
    }

    @Test
    fun `one player's level is not another's`() {
        val other = UUID.nameUUIDFromBytes("someone-else".toByteArray())
        run("cloud events off")

        assertEquals(FeedLevel.OFF, levels.level(sourcePlayer!!))
        assertEquals(FeedLevel.MINIMAL, levels.level(other))
    }

    @Test
    fun `the console is told it has no level rather than silently failing`() {
        sourcePlayer = null

        run("cloud events off")
        run("cloud events")

        assertEquals(2, sent.size, sent.toString())
        assertTrue(sent.all { it.contains("console") }, sent.toString())
    }

    @Test
    fun `an unknown level is an unknown command`() {
        assertFailsWith<CommandSyntaxException> { run("cloud events loud") }
    }

    @Test
    fun `events is invisible without its own permission`() {
        permissions = setOf(PERMISSION_READ)

        assertFailsWith<CommandSyntaxException> { run("cloud events off") }
    }

    @Test
    fun `holding only events still opens the root`() {
        permissions = setOf(PERMISSION_EVENTS)

        run("cloud events off")

        assertEquals(FeedLevel.OFF, levels.level(sourcePlayer!!))
    }
```

`ProxyRoleTest.kt` and `ServerRoleTest.kt`: `FeedState()` → `FeedLevels(null)`, import `cloud.spawnery.agent.FeedLevels` in place of `FeedState`.

- [ ] **Step 2: Run them and see them fail**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: compile failure, `Unresolved reference: LevelStore` / `FeedLevels`.

- [ ] **Step 3: Implement.**

`agent/common/src/main/kotlin/cloud/spawnery/agent/FeedLevels.kt`:

```kotlin
package cloud.spawnery.agent

import java.util.UUID
import java.util.concurrent.CompletableFuture
import java.util.concurrent.CompletionStage
import java.util.concurrent.ConcurrentHashMap

interface LevelStore {
    /** False while the store's own plugin is not loaded; [FeedLevels] then uses its memory. */
    fun available(): Boolean

    fun read(player: UUID): String?

    fun write(player: UUID, value: String): CompletionStage<*>
}

/**
 * Nothing removes a player who logs out; the map is bounded by the
 * administrators who type the command.
 */
class MemoryLevels : LevelStore {
    private val values = ConcurrentHashMap<UUID, String>()

    override fun available(): Boolean = true

    override fun read(player: UUID): String? = values[player]

    override fun write(player: UUID, value: String): CompletionStage<*> {
        values[player] = value
        return CompletableFuture.completedFuture(Unit)
    }
}

class FeedLevels(private val durable: LevelStore?, private val memory: LevelStore = MemoryLevels()) {
    private fun store(): LevelStore = durable?.takeIf { it.available() } ?: memory

    /** Read on the feed's tick, where a throw would cost every other player their lines. */
    fun level(player: UUID): FeedLevel =
        FeedLevel.of(
            try {
                store().read(player)
            } catch (_: RuntimeException) {
                null
            },
        )

    fun set(player: UUID, level: FeedLevel): CompletionStage<*> =
        try {
            store().write(player, level.word)
        } catch (e: RuntimeException) {
            CompletableFuture.failedFuture<Unit>(e)
        }

    /** Whether a level set now outlives this process. */
    fun kept(): Boolean = store() !== memory
}
```

`agent/common/src/main/kotlin/cloud/spawnery/agent/LuckPermsFeedLevels.kt` (licence header first):

```kotlin
package cloud.spawnery.agent

import net.luckperms.api.LuckPermsProvider
import net.luckperms.api.node.NodeType
import net.luckperms.api.node.types.MetaNode
import java.util.UUID
import java.util.concurrent.CompletionStage

object LuckPermsFeedLevels {
    const val META_KEY: String = "spawnery-feed"

    /** Probed for the same reason as [LuckPermsContexts.registerIfPresent]. */
    fun storeIfPresent(): LevelStore? {
        try {
            Class.forName("net.luckperms.api.LuckPermsProvider")
        } catch (_: ClassNotFoundException) {
            return null
        }
        return Store
    }

    private object Store : LevelStore {
        /** [LuckPermsProvider.get] throws until LuckPerms has enabled, and after it failed to. */
        override fun available(): Boolean =
            try {
                LuckPermsProvider.get()
                true
            } catch (_: IllegalStateException) {
                false
            }

        /** The cached meta, so a value set on a group reaches its members. */
        override fun read(player: UUID): String? =
            LuckPermsProvider.get().userManager.getUser(player)?.cachedData?.metaData?.getMetaValue(META_KEY)

        override fun write(player: UUID, value: String): CompletionStage<*> =
            LuckPermsProvider.get().userManager.modifyUser(player) { user ->
                user.data().clear(NodeType.META.predicate { it.metaKey == META_KEY })
                user.data().add(MetaNode.builder(META_KEY, value).build())
            }
    }
}
```

`Feed.kt`: the constructor's second parameter becomes `private val levels: FeedLevels`; then

```kotlin
    fun wanted(subscribers: Int): Boolean =
        subscribers > 0 || audience.holders(PERMISSION_EVENTS).any { levels.level(it) != FeedLevel.OFF }
```

and in `deliver` the recipients become `audience.holders(PERMISSION_EVENTS).filter { levels.level(it) != FeedLevel.OFF }`.

`CloudCommand.kt`:
- parameter `feed: FeedState,` → `levels: FeedLevels,`
- replace the whole `.then(LiteralArgumentBuilder.literal<S>("events") … )` block with `.then(eventsBranch(adapter, format, levels))`
- replace `setFeed` with:

```kotlin
private val LEVEL_WORDS = listOf("minimal", "normal", "verbose", "off", "on")

private fun <S> eventsBranch(
    adapter: SourceAdapter<S>,
    format: () -> String,
    levels: FeedLevels,
): LiteralArgumentBuilder<S> {
    val branch = LiteralArgumentBuilder.literal<S>("events")
        .requires { adapter.hasPermission(it, PERMISSION_EVENTS) }
        .executes { ctx -> showLevel(adapter, format, levels, ctx.source) }
    // Literals rather than one argument, so a typo is an unknown command
    // instead of a silent no-op.
    for (word in LEVEL_WORDS) {
        val level = FeedLevel.parse(word)!!
        branch.then(
            LiteralArgumentBuilder.literal<S>(word)
                .executes { ctx -> setLevel(adapter, format, levels, ctx.source, level) },
        )
    }
    return branch
}

private fun <S> showLevel(adapter: SourceAdapter<S>, format: () -> String, levels: FeedLevels, source: S): Int {
    val player = adapter.playerId(source) ?: return consoleHasNoLevel(adapter, format, source)
    replyOk(adapter, format, source,
        Style.quiet("Your feed level is ") + Style.number(levels.level(player).word) + Style.quiet(". Change it with ") +
            Style.number("/cloud events minimal|normal|verbose|off") + Style.quiet("."))
    return 1
}

private fun <S> setLevel(
    adapter: SourceAdapter<S>,
    format: () -> String,
    levels: FeedLevels,
    source: S,
    level: FeedLevel,
): Int {
    val player = adapter.playerId(source) ?: return consoleHasNoLevel(adapter, format, source)
    val kept = levels.kept()
    levels.set(player, level).whenComplete { _, failure ->
        if (failure != null) {
            replyFail(adapter, format, source,
                Style.bad("could not save your feed level") + Style.quiet(": ") + Style.bad(reason(failure)))
        } else {
            replyOk(adapter, format, source,
                Style.good("Feed level: ${level.word}.") + if (kept) "" else Style.quiet(" Kept until the next restart."))
        }
    }
    return 1
}

private fun <S> consoleHasNoLevel(adapter: SourceAdapter<S>, format: () -> String, source: S): Int {
    replyFail(adapter, format, source,
        Style.bad("the console has no feed level") + Style.quiet(": the feed goes to players only"))
    return 0
}
```

The old console reply said the lines "are already in its log". Nothing logs cloud events, so the new wording drops that claim.

`agent/velocity/.../AgentPlugin.kt`: import `cloud.spawnery.agent.FeedLevels` and `cloud.spawnery.agent.LuckPermsFeedLevels` instead of `FeedState`; the field becomes

```kotlin
    /** Built here rather than in start(), because it outlives a reconnect. */
    private val feedLevels = FeedLevels(LuckPermsFeedLevels.storeIfPresent())
```

and both `feedState` arguments (the `Feed(...)` at line 191 and `cloudCommand(...)` at line 201) become `feedLevels`.

`agent/paper/.../AgentPlugin.kt`: the same import change; `private val feedLevels = FeedLevels(LuckPermsFeedLevels.storeIfPresent())`; `Feed(PaperAudience, feedLevels, …)`; `cloudCommand(api, PaperSource, feedLevels, mirror::feedFormat)`. The store only probes the class at construction and asks LuckPerms per call, so building it before LuckPerms has enabled is safe.

Delete `FeedState.kt` (`git rm`).

- [ ] **Step 4: Run them**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: the build succeeds. `CloudCommandTest`'s `` `the tree asks the platform for nothing but the permissions it declares` `` still runs `cloud events off` and still passes.

- [ ] **Step 5: Commit**

```bash
git add -A agent
git commit -m "feat(agent): feed levels kept in LuckPerms meta or in memory"
```

Body: `/cloud events` shows or sets minimal, normal, verbose or off; with LuckPerms the level is the `spawnery-feed` meta value, read with inheritance, so it survives a roll and a change of proxy.

---

### Task 6: Render the feed by level

**Files:**
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudFeed.kt` (whole file)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudFeedBuffer.kt` (`deliver` type, `tick`)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/Feed.kt` (constructor, `deliver`)
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt` (the `Feed(...)` at line 191)
- Test: replace `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudFeedTest.kt`; modify `CloudFeedBufferTest.kt`, `FeedTest.kt`

**Interfaces:**
- Consumes: `Row`, `row`, `isWarning`, `shortReason`, `words` (Task 3); `shortName`, `Style.subject`, `NetworkMirror.groupKind` (Task 4); `FeedLevels`, `FeedLevel` (Task 5).
- Produces: `internal fun coalesce(events: List<CloudEvent>, level: FeedLevel, groupKind: (String) -> Group.Kind = { Group.Kind.UNKNOWN }): List<String>`; `CloudFeedBuffer(clock, windowMillis, deliver: (List<CloudEvent>) -> Unit)`; `Feed(audience, levels, clock, windowMillis, format, groupKind: (String) -> Group.Kind = { Group.Kind.UNKNOWN })`.

What each level prints (`plain` text):

| Row | `minimal` | `normal` | `verbose` |
|---|---|---|---|
| CREATED | `[+] lobby-x7k2` | `[+] lobby-x7k2 starting` | `[+] lobby-x7k2: <note>` |
| ARRIVED | `[+] gateway-4d1` | `[✓] gateway-4d1 ready` | `[✓] gateway-4d1: <note>` |
| READY | | `[✓] lobby-x7k2 ready` | `[✓] lobby-x7k2: <note>` |
| LEAVING | | `[-] lobby-x7k2 leaving` | `[-] lobby-x7k2: <note>` |
| GONE | `[-] lobby-x7k2` | `[-] lobby-x7k2 stopped` | `[-] lobby-x7k2: <note>` |
| OTHER | | | `[·] lobby-x7k2: <note>` |
| warning | `[!] lobby-x7k2 <short reason>` | same | `[!] lobby-x7k2: <note, or short reason if empty>` |

Collapsing: more than one non-warning event in one group within the window. In `minimal` and `normal` the events are grouped by row, after hiding the rows the level does not show, and print as `[+] 5 lobby` / `[+] 5 lobby starting`. In `verbose` they are grouped by kind, as today, and print as `[+] 5 PodCreated in lobby (a, b, c, d, e, f and 2 more)`. Warnings come first and never collapse.

- [ ] **Step 1: Write the failing tests.** Replace `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudFeedTest.kt` with:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.pb.CloudEvent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

private fun event(
    kind: String,
    subject: String,
    group: String = "lobby",
    warning: Boolean = false,
    message: String = "$subject: $kind",
): CloudEvent =
    CloudEvent.newBuilder()
        .setKind(kind).setSubject(subject).setGroup(group)
        .setMessage(message).setWarning(warning)
        .build()

private fun lines(
    level: FeedLevel,
    vararg events: CloudEvent,
    kinds: (String) -> Group.Kind = { Group.Kind.EPHEMERAL },
): List<String> = coalesce(events.toList(), level, kinds).map(::plain)

class CloudFeedTest {
    @Test
    fun `minimal shows a server arriving and going and nothing between`() {
        assertEquals(listOf("[+] lobby-x7k2"), lines(FeedLevel.MINIMAL, event("PodCreated", "lobby-x7k2")))
        assertEquals(listOf("[-] lobby-x7k2"), lines(FeedLevel.MINIMAL, event("ServerStopped", "lobby-x7k2")))
        assertEquals(
            emptyList(),
            lines(
                FeedLevel.MINIMAL,
                event("ReadyGatePassed", "lobby-a"),
                event("Retiring", "lobby-b"),
                event("PodPending", "lobby-c"),
            ),
        )
    }

    @Test
    fun `normal names each step in a word`() {
        assertEquals(
            listOf("[+] lobby-a starting", "[✓] lobby-b ready", "[-] lobby-c leaving", "[-] lobby-d stopped"),
            lines(
                FeedLevel.NORMAL,
                event("PodCreated", "lobby-a"),
                event("ReadyGatePassed", "lobby-b"),
                event("DeletionRequested", "lobby-c"),
                event("ServerStopped", "lobby-d"),
            ),
        )
    }

    @Test
    fun `a proxy arrives at its ready gate`() {
        val started = event("ProxyStarted", "gateway-4d1", group = "gateway")
        assertEquals(listOf("[+] gateway-4d1"), lines(FeedLevel.MINIMAL, started))
        assertEquals(listOf("[✓] gateway-4d1 ready"), lines(FeedLevel.NORMAL, started))
        assertEquals(
            listOf("[-] gateway-4d1 leaving", "[-] gateway-4d1 stopped"),
            lines(
                FeedLevel.NORMAL,
                event("ProxyRetiring", "gateway-4d1", group = "gateway"),
                event("ProxyStopped", "gateway-4d1", group = "gateway"),
            ),
        )
    }

    @Test
    fun `verbose shows the operator's note`() {
        assertEquals(
            listOf("[+] lobby-x7k2: created pod lobby-x7k2"),
            lines(FeedLevel.VERBOSE, event("PodCreated", "lobby-x7k2", message = "created pod lobby-x7k2")),
        )
        assertEquals(
            listOf("[·] lobby-x7k2: phase Pending -> Starting: pod is running"),
            lines(
                FeedLevel.VERBOSE,
                event("PodRunning", "lobby-x7k2", message = "phase Pending -> Starting: pod is running"),
            ),
        )
    }

    @Test
    fun `a kind this agent does not know shows only in verbose`() {
        val later = event("SomethingAddedInALaterRelease", "lobby-a")
        assertEquals(emptyList(), lines(FeedLevel.MINIMAL, later))
        assertEquals(emptyList(), lines(FeedLevel.NORMAL, later))
        assertEquals(1, lines(FeedLevel.VERBOSE, later).size)
    }

    @Test
    fun `a warning is a short reason below verbose and the note in it`() {
        val timeout = event(
            "StartupTimeout", "lobby-x7k2",
            message = "phase Starting -> Failed: server did not become ready in time",
        )
        assertEquals(listOf("[!] lobby-x7k2 did not start in time"), lines(FeedLevel.MINIMAL, timeout))
        assertEquals(listOf("[!] lobby-x7k2 did not start in time"), lines(FeedLevel.NORMAL, timeout))
        assertEquals(
            listOf("[!] lobby-x7k2: phase Starting -> Failed: server did not become ready in time"),
            lines(FeedLevel.VERBOSE, timeout),
        )
    }

    @Test
    fun `a warning nobody put in the table is read from its kind`() {
        assertEquals(
            listOf("[!] lobby-x7k2 pod name conflict"),
            lines(FeedLevel.MINIMAL, event("PodNameConflict", "lobby-x7k2", warning = true)),
        )
    }

    @Test
    fun `a warning with no note or no kind still says something`() {
        assertEquals(
            listOf("[!] lobby-x7k2: did not start in time"),
            lines(FeedLevel.VERBOSE, event("StartupTimeout", "lobby-x7k2", message = "")),
        )
        assertEquals(
            listOf("[!] lobby-x7k2 warning"),
            lines(FeedLevel.MINIMAL, event("", "lobby-x7k2", warning = true, message = "")),
        )
    }

    @Test
    fun `many of one row in one group collapse to a count`() {
        val three = arrayOf(event("PodCreated", "lobby-a"), event("PodCreated", "lobby-b"), event("PodCreated", "lobby-c"))
        assertEquals(listOf("[+] 3 lobby"), lines(FeedLevel.MINIMAL, *three))
        assertEquals(listOf("[+] 3 lobby starting"), lines(FeedLevel.NORMAL, *three))
    }

    @Test
    fun `a row the level hides neither shows nor counts`() {
        val window = arrayOf(event("PodCreated", "lobby-a"), event("PodCreated", "lobby-b"), event("ReadyGatePassed", "lobby-c"))
        assertEquals(listOf("[+] 2 lobby"), lines(FeedLevel.MINIMAL, *window))
        assertEquals(listOf("[+] 2 lobby starting", "[✓] lobby-c ready"), lines(FeedLevel.NORMAL, *window))
    }

    @Test
    fun `one row in two groups stays two lines`() {
        assertEquals(
            listOf("[+] lobby-a", "[+] arena-a"),
            lines(FeedLevel.MINIMAL, event("PodCreated", "lobby-a"), event("PodCreated", "arena-a", group = "arena")),
        )
    }

    @Test
    fun `warnings come first and never collapse`() {
        assertEquals(
            listOf("[!] lobby-b did not start in time", "[!] lobby-c did not start in time", "[+] lobby-a"),
            lines(
                FeedLevel.MINIMAL,
                event("PodCreated", "lobby-a"),
                event("StartupTimeout", "lobby-b"),
                event("StartupTimeout", "lobby-c"),
            ),
        )
    }

    @Test
    fun `verbose collapses by kind and names the servers`() {
        assertEquals(
            listOf("[+] 3 PodCreated in lobby (lobby-a, lobby-b, lobby-c)"),
            lines(FeedLevel.VERBOSE, event("PodCreated", "lobby-a"), event("PodCreated", "lobby-b"), event("PodCreated", "lobby-c")),
        )
        val eight = (1..8).map { event("PodCreated", "lobby-$it") }.toTypedArray()
        assertTrue(lines(FeedLevel.VERBOSE, *eight).single().endsWith("lobby-6 and 2 more)"), lines(FeedLevel.VERBOSE, *eight).single())
    }

    @Test
    fun `off shows nothing at all`() {
        assertEquals(emptyList(), coalesce(listOf(event("StartupTimeout", "lobby-a")), FeedLevel.OFF))
    }

    @Test
    fun `an on-demand member is shown short, linked by its full name`() {
        val kinds = { g: String -> if (g == "challenge") Group.Kind.ON_DEMAND else Group.Kind.EPHEMERAL }
        val created = event("PodCreated", "challenge-3f2b1c9a0d4e", group = "challenge")

        assertEquals(listOf("[+] challenge-3f2b1c"), lines(FeedLevel.MINIMAL, created, kinds = kinds))
        val raw = coalesce(listOf(created), FeedLevel.MINIMAL, kinds).single()
        assertTrue(raw.contains("show_text:'challenge-3f2b1c9a0d4e'"), raw)
        assertTrue(raw.contains("suggest_command:'/cloud info challenge-3f2b1c9a0d4e'"), raw)
    }

    @Test
    fun `only the sign carries the row's colour`() {
        val raw = coalesce(listOf(event("PodCreated", "lobby-a")), FeedLevel.MINIMAL).single()
        assertTrue(raw.startsWith("<dark_gray>[</dark_gray><green>+</green><dark_gray>]</dark_gray> "), raw)
        val warning = coalesce(listOf(event("StartupTimeout", "lobby-a")), FeedLevel.MINIMAL).single()
        assertTrue(warning.startsWith("<dark_gray>[</dark_gray><red>!</red>"), warning)
        assertTrue(warning.endsWith("<red>did not start in time</red>"), warning)
    }
}
```

`CloudFeedBufferTest.kt`: `private val delivered = mutableListOf<List<CloudEvent>>()`. Rename `` `the window closing delivers one collapsed batch` `` to `` `the window closing delivers its events as one batch` `` and make its assertion `assertEquals(2, delivered.single().size, "two events made ${delivered.single().size}")`. In the last test the final assertion becomes `assertEquals("lobby-b", delivered[1].single().subject, delivered[1].toString())`.

`FeedTest.kt`: in `anEvent`, `setKind("ReadyGatePassed")` becomes `setKind("PodCreated")`, since `minimal` does not show a ready gate. Add:

```kotlin
    @Test
    fun `each player sees the window at their own level`() {
        audience.online += listOf(alice, bob)
        audience.permitted += listOf(alice, bob)
        levels.set(bob, FeedLevel.NORMAL)

        feed.onEvent(anEvent("lobby-a"))
        feed.onEvent(
            CloudEvent.newBuilder().setKind("ReadyGatePassed").setSubject("lobby-a").setGroup("lobby")
                .setMessage("phase Starting -> Ready").build(),
        )
        now = 1_000
        feed.tick()

        assertEquals(1, audience.sent.count { it.first == alice }, audience.sent.toString())
        assertEquals(2, audience.sent.count { it.first == bob }, audience.sent.toString())
    }
```

- [ ] **Step 2: Run them and see them fail**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: compile failure, `coalesce` called with three arguments, and `CloudFeedBuffer` delivering `List<String>`.

- [ ] **Step 3: Implement.** Replace `CloudFeed.kt` with:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.pb.CloudEvent

/** Six names fit a chat line; a forty-server scale-up printed in full is a wall. */
private const val NAMES_SHOWN = 6

/**
 * Collapsed here and not on the operator, so the wire stays one event per
 * transition, the same facts as `kubectl get events`.
 *
 * Warnings are never collapsed: two failures rarely fail for the same reason.
 */
internal fun coalesce(
    events: List<CloudEvent>,
    level: FeedLevel,
    groupKind: (String) -> Group.Kind = { Group.Kind.UNKNOWN },
): List<String> {
    if (level == FeedLevel.OFF) return emptyList()
    val name = { e: CloudEvent -> Style.subject(e.subject, shortName(e.subject, e.group, groupKind(e.group))) }
    val lines = mutableListOf<String>()
    val (warnings, ordinary) = events.partition(::isWarning)

    for (w in warnings) {
        lines += if (level == FeedLevel.VERBOSE) {
            "$WARNING ${name(w)}${Style.quiet(": ")}${Style.bad(w.message.ifBlank { shortReason(w.kind) })}"
        } else {
            "$WARNING ${name(w)} ${Style.bad(shortReason(w.kind))}"
        }
    }

    val verbose = level == FeedLevel.VERBOSE
    val byKey = LinkedHashMap<Pair<String, String>, MutableList<CloudEvent>>()
    for (e in ordinary) {
        if (word(row(e.kind), level) == null) continue
        val key = (if (verbose) e.kind else row(e.kind).name) to e.group
        byKey.getOrPut(key) { mutableListOf() } += e
    }

    for ((key, collapsed) in byKey) {
        val group = key.second
        val r = row(collapsed.first().kind)
        val sign = sign(r, level)
        val only = collapsed.singleOrNull()
        lines += when {
            verbose && only != null ->
                "$sign ${name(only)}${Style.quiet(": ")}${Style.quiet(only.message.ifBlank { words(only.kind) })}"
            verbose -> {
                val shown = collapsed.take(NAMES_SHOWN).joinToString(Style.quiet(", ")) { name(it) }
                val rest = collapsed.size - minOf(collapsed.size, NAMES_SHOWN)
                val names = if (rest > 0) shown + Style.quiet(" and $rest more") else shown
                "$sign ${Style.number(collapsed.size)} ${Style.number(key.first)}${Style.quiet(" in ")}" +
                    "${Style.subject(group)}${Style.quiet(" (")}$names${Style.quiet(")")}"
            }
            only != null -> "$sign ${name(only)}" + suffix(word(r, level))
            else -> "$sign ${Style.number(collapsed.size)} ${Style.subject(group)}" + suffix(word(r, level))
        }
    }
    return lines
}

/** Null hides the row at this level; empty shows the name alone. */
private fun word(row: Row, level: FeedLevel): String? = when (level) {
    FeedLevel.OFF -> null
    FeedLevel.MINIMAL -> when (row) {
        Row.CREATED, Row.ARRIVED, Row.GONE -> ""
        else -> null
    }
    FeedLevel.NORMAL -> when (row) {
        Row.CREATED -> "starting"
        Row.ARRIVED, Row.READY -> "ready"
        Row.LEAVING -> "leaving"
        Row.GONE -> "stopped"
        Row.OTHER -> null
    }
    FeedLevel.VERBOSE -> ""
}

private fun suffix(word: String?): String = if (word.isNullOrEmpty()) "" else " " + Style.quiet(word)

/** At minimal a proxy's ready gate is its arrival, so it takes the arrival's sign. */
private fun sign(row: Row, level: FeedLevel): String = when (row) {
    Row.CREATED -> Style.marker("+", "green")
    Row.ARRIVED -> if (level == FeedLevel.MINIMAL) Style.marker("+", "green") else Style.marker("✓", "green")
    Row.READY -> Style.marker("✓", "green")
    Row.LEAVING, Row.GONE -> Style.marker("-", "gold")
    Row.OTHER -> Style.marker("·", "dark_gray")
}

private val WARNING = Style.marker("!", "red")
```

`CloudFeedBuffer.kt`: the third constructor parameter becomes `private val deliver: (List<CloudEvent>) -> Unit`, and `tick` becomes

```kotlin
    fun tick() {
        if (pending.isEmpty()) return
        if (clock() - openedAt < windowMillis) return
        val batch = pending.toList()
        // Cleared first, so a throwing deliver does not resend this window on every tick.
        pending.clear()
        deliver(batch)
    }
```

`Feed.kt`: add the constructor parameter after `format`:

```kotlin
    /** On-demand members get short names; a backend's picture has no on-demand groups, so it never shortens. */
    private val groupKind: (String) -> Group.Kind = { Group.Kind.UNKNOWN },
```

(import `cloud.spawnery.agent.api.Group`) and replace `deliver` with

```kotlin
    private fun deliver(events: List<CloudEvent>) {
        // Read once, not per line, so nobody gets a partial batch.
        val byLevel = audience.holders(PERMISSION_EVENTS).groupBy(levels::level)
        if (byLevel.keys.all { it == FeedLevel.OFF }) return
        // Read once, so a resync mid-loop cannot give two players different shapes.
        val shape = format().ifBlank { DEFAULT_FORMAT }
        for ((level, players) in byLevel) {
            val lines = coalesce(events, level, groupKind)
            for (who in players) {
                for (line in lines) {
                    audience.send(who, shape.replace(MESSAGE_TOKEN, line))
                }
            }
        }
    }
```

`agent/velocity/.../AgentPlugin.kt`: `Feed(VelocityAudience(proxy), feedLevels, System::currentTimeMillis, format = mirror::feedFormat, groupKind = mirror::groupKind)`. Paper keeps the default: its audience is empty and its picture has no on-demand groups.

- [ ] **Step 4: Run them**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: the build succeeds.

- [ ] **Step 5: Commit**

```bash
git add -A agent
git commit -m "feat(agent): render the event feed at each player's level"
```

Body: minimal shows servers and proxies arriving and going plus warnings with a short reason; normal adds ready and leaving; verbose is the operator's note; a kind the agent does not know shows in verbose only.

---

### Task 7: Warning before a forced transfer

**Files:**
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/TransferWarning.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/TransferPolicy.kt` (new `Warning`, `warned`, `warnings`; `leaving` lines 23-27; `forget` lines 51-53)
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/Transfers.kt` (`Traveller`, lines 10-18; `pass`, lines 39-46; `VelocityTraveller`, lines 105-127)
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/TransferPolicyTest.kt`, `TransfersTest.kt` (its `FakeTraveller`, lines 41-54)

**Interfaces:**
- Produces: `TransferPolicy.Warning(id: UUID, seconds: Long)`; `fun TransferPolicy.warnings(picture: Picture, occupants: List<Occupant>, leadMillis: Long): List<Warning>`; `Traveller.tell(message: Component)`; `object TransferWarning { KEY, FALLBACK, LEAD_MILLIS; fun message(seconds: Long): Component }`.

- [ ] **Step 1: Write the failing tests.** `TransferPolicyTest.kt`, new tests (the class already has `alice`, `bob`, `now`, `policy`, `picture`, `leavingAlone`):

```kotlin
    private fun occupants(vararg ids: UUID) = ids.map { TransferPolicy.Occupant(it, "lobby-1") }

    @Test
    fun `nobody is warned while more than the lead is left`() {
        val p = policy(forceAfterMillis = 120_000L)
        val pic = picture(leavingAlone)

        assertEquals(emptyList(), p.warnings(pic, occupants(alice), 10_000L))
        now += 110_000L - 1
        assertEquals(emptyList(), p.warnings(pic, occupants(alice), 10_000L))
        now += 1
        assertEquals(listOf(TransferPolicy.Warning(alice, 10)), p.warnings(pic, occupants(alice), 10_000L))
    }

    @Test
    fun `the seconds are rounded up`() {
        val p = policy(forceAfterMillis = 15_000L)
        val pic = picture(leavingAlone)
        p.warnings(pic, occupants(alice), 10_000L)
        now += 5_500L

        assertEquals(listOf(TransferPolicy.Warning(alice, 10)), p.warnings(pic, occupants(alice), 10_000L))
    }

    @Test
    fun `a forceAfter shorter than the lead warns at once`() {
        assertEquals(
            listOf(TransferPolicy.Warning(alice, 3)),
            policy(forceAfterMillis = 3_000L).warnings(picture(leavingAlone), occupants(alice), 10_000L),
        )
    }

    @Test
    fun `a forceAfter of zero warns once with one second, before the same pass moves the player`() {
        val p = policy(forceAfterMillis = 0L)
        val pic = picture(leavingAlone)

        assertEquals(listOf(TransferPolicy.Warning(alice, 1)), p.warnings(pic, occupants(alice), 10_000L))
        assertEquals(listOf(alice to "lobby-1"), p.forced(pic, occupants(alice)))
    }

    @Test
    fun `each player is warned once per leaving proxy`() {
        val p = policy(forceAfterMillis = 0L)
        val pic = picture(leavingAlone)
        assertEquals(1, p.warnings(pic, occupants(alice), 10_000L).size)
        assertEquals(emptyList(), p.warnings(pic, occupants(alice), 10_000L))

        val back = picture(listOf(ProxyInfo("edge-1", "edge", true, false, 0, ""), ProxyInfo("edge-2", "edge", true, false, 0, "")))
        p.warnings(back, occupants(alice), 10_000L)
        assertEquals(1, p.warnings(pic, occupants(alice), 10_000L).size, "a second leave did not warn again")
    }

    @Test
    fun `only those the forced pass would move are warned`() {
        val p = policy(forceAfterMillis = 0L)

        assertEquals(
            emptyList(),
            p.warnings(picture(leavingAlone, closedDoors = setOf("lobby-1")), occupants(alice), 10_000L),
            "a player behind a closed door was warned",
        )
        val nowhere = picture(listOf(ProxyInfo("edge-1", "edge", false, true, 0, ""), ProxyInfo("hub-1", "hub", true, false, 0, "")))
        assertEquals(emptyList(), p.warnings(nowhere, occupants(alice), 10_000L), "warned with nowhere to go")
        assertEquals(
            emptyList(),
            p.warnings(picture(leavingAlone), listOf(TransferPolicy.Occupant(bob, null)), 10_000L),
            "a player between servers was warned",
        )
    }

    @Test
    fun `a player already moved on a switch is not warned`() {
        val p = policy(forceAfterMillis = 0L)
        val pic = picture(leavingAlone)
        assertTrue(p.onSwitch(pic, alice))

        assertEquals(emptyList(), p.warnings(pic, occupants(alice), 10_000L))
    }
```

`TransfersTest.kt`: `FakeTraveller` gains

```kotlin
        val told = mutableListOf<Component>()

        override fun tell(message: Component) {
            told += message
        }
```

(imports `net.kyori.adventure.text.Component` and `net.kyori.adventure.text.TranslatableComponent`), and new tests:

```kotlin
    @Test
    fun `a pass warns a player before it transfers them`() {
        val alice = FakeTraveller("alice", "lobby-1", host)

        transfers.pass(listOf(alice))

        val warning = alice.told.single() as TranslatableComponent
        assertEquals("spawnery.transfer.warning", warning.key())
        assertEquals(1, alice.sent.size)
    }

    @Test
    fun `a switch transfers without a warning`() {
        val alice = FakeTraveller("alice", "lobby-1", host)

        assertTrue(transfers.onSwitch(alice, "arena-1"))
        assertTrue(alice.told.isEmpty(), alice.told.toString())
    }

    @Test
    fun `a player without a virtual host is not warned either`() {
        val alice = FakeTraveller("alice", "lobby-1", null)

        transfers.pass(listOf(alice))

        assertTrue(alice.told.isEmpty(), alice.told.toString())
    }

    @Test
    fun `the warning is translatable, with the seconds as its argument and a fallback`() {
        val c = TransferWarning.message(7) as TranslatableComponent
        assertEquals("spawnery.transfer.warning", c.key())
        assertEquals("You will be reconnected in %s seconds.", c.fallback())
        assertEquals(Component.text(7L), c.arguments().single().asComponent())
    }
```

- [ ] **Step 2: Run them and see them fail**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: compile failure, `Unresolved reference: warnings`, `tell`, `TransferWarning`.

- [ ] **Step 3: Implement.**

`agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/TransferWarning.kt`:

```kotlin
package cloud.spawnery.agent.velocity

import net.kyori.adventure.text.Component

object TransferWarning {
    const val KEY = "spawnery.transfer.warning"
    const val FALLBACK = "You will be reconnected in %s seconds."
    const val LEAD_MILLIS = 10_000L

    /** Translatable, so a network with its own translations can render it. */
    fun message(seconds: Long): Component =
        Component.translatable()
            .key(KEY)
            .fallback(FALLBACK)
            .arguments(Component.text(seconds))
            .build()
}
```

`TransferPolicy.kt`: after `data class Occupant(...)`:

```kotlin
    data class Warning(val id: UUID, val seconds: Long)
```

after `private val tried = …`:

```kotlin
    private val warned = ConcurrentHashMap.newKeySet<UUID>()
```

in `leaving`, the not-draining branch also calls `warned.clear()` after `tried.clear()`. After `forced`:

```kotlin
    /** The players [forced] will move, each named once, [leadMillis] before it or at once when less is left. */
    fun warnings(picture: Picture, occupants: List<Occupant>, leadMillis: Long): List<Warning> {
        if (!leaving(picture) || !somewhereElse(picture)) return emptyList()
        val left = forceAfterMillis - (clock() - firstLeavingAt.get())
        if (left > leadMillis) return emptyList()
        // Rounded up and never 0: "in 0 seconds" reads as already missed.
        val seconds = maxOf(1L, (maxOf(0L, left) + 999) / 1000)
        val result = mutableListOf<Warning>()
        for (occupant in occupants) {
            val server = occupant.server ?: continue
            if (server in picture.closedDoors || occupant.id in tried) continue
            if (warned.add(occupant.id)) result += Warning(occupant.id, seconds)
        }
        return result
    }
```

`forget` becomes

```kotlin
    fun forget(picture: Picture, player: UUID) {
        if (!leaving(picture)) {
            tried.remove(player)
            warned.remove(player)
        }
    }
```

`Transfers.kt`: `Traveller` gains

```kotlin
    fun tell(message: Component)
```

(import `net.kyori.adventure.text.Component`); `pass` becomes

```kotlin
    fun pass(players: List<Traveller>) {
        val movable = players.filter { it.virtualHost != null }.associateBy { it.uuid }
        val occupants = movable.values.map { TransferPolicy.Occupant(it.uuid, it.currentServer) }
        val picture = picture()
        for (warning in policy.warnings(picture, occupants, TransferWarning.LEAD_MILLIS)) {
            movable[warning.id]?.tell(TransferWarning.message(warning.seconds))
        }
        for ((id, target) in policy.forced(picture, occupants)) {
            val player = movable[id] ?: continue
            send(player, player.virtualHost ?: continue, target, "forced")
        }
    }
```

`VelocityTraveller` gains `override fun tell(message: Component) = player.sendMessage(message)`.

- [ ] **Step 4: Run them**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: the build succeeds. The existing `TransfersTest` cases still pass: their policy forces at 0, so every forced player is now also told, and none of them asserts otherwise.

- [ ] **Step 5: Commit**

```bash
git add -A agent
git commit -m "feat(agent): warn a player 10 seconds before a forced transfer"
```

Body: translatable key `spawnery.transfer.warning`, sent once per player per leaving proxy, only to players the forced pass would move.

---

### Task 8: The replies, once through the humanizer

The spec asks for shorter, uniform replies that say the same things. Most of the work is in `CloudCommand.kt`. The labels in `ListLines.kt` and `StatusLines.kt` (`Node`, `Players`, `ready`, `not scheduled`) are already terse, so they pass through the skill but are expected to stay.

**Files:**
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudCommand.kt`, `Execute.kt`; and `ListLines.kt`, `StatusLines.kt` if the skill finds anything
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudCommandTest.kt`, `ExecuteLinesTest.kt`; `test/e2e/tutorial_test.go` only if a phrase it reads moves (it should not)

**Interfaces:** none.

These phrases are read by `test/e2e/tutorial_test.go` and stay word for word: `<group> is pinned to <n> servers until HH:MM UTC` (line 847), `<server> is being killed.` (line 895), `<n> of <m> servers ran it` (line 937), `<server> ran it` (line 962). `execute is not enabled on this network` comes from the operator and is not touched.

The target text (the plain words; the `Style` calls around them stay as they are):

| Where | Now | After |
|---|---|---|
| `info`, not found (l. 96) | `no server, proxy or group called X on this network` | `nothing called X on this network` |
| `retire` ok (l. 135-138) | `X is retiring. It takes no new joins; the players on it finish in their own time and nobody is kicked.` | `X is retiring: no new joins, and nobody is kicked.` Delete the comment above it (line 132): the reply now says it. |
| `unretire` ok (l. 169-170) | `X takes joins again. Nothing automatic removes it now; it stays until it ends by itself.` | `X takes joins again and stays until it ends by itself.` |
| duration (l. 209-214) | `could not read X as a length of time. Try 30m, 2h or 3d.` | `X is not a duration. Try 30m, 2h or 3d.` |
| `status` failed (l. 308) | `no status: R` | `could not get the status: R` |
| `scale` ok, second line (l. 340) | `It ends on its own; /cloud scale X reset ends it early.` | `/cloud scale X reset ends it early.` |
| `reset`, nothing (l. 356) | `X had no pin or boost` | `X has no pin or boost` |
| `reset` ok (l. 358-360) | `X: removed N pins and boosts. The group returns to its own floor and ceiling.` | `removed N pins and boosts from X; it is back to its own floor and ceiling` (`pin and boost` for 1) |
| `forcestop` ok (l. 383-384) | `X is being killed. /cloud info shows when its pod is gone.` | `X is being killed. /cloud info shows when it is gone.` |
| `execute`, too long (l. 417-419) | `that command is N characters; the operator carries at most 256` | `that command is N characters long; the limit is 256` |
| every `could not <verb> X: R` | | unchanged; it is already the shared pattern |

- [ ] **Step 1: Run the skill.** Load the `humanizer` skill and give it, in embedded mode, the "After" column above, the `events` replies from Task 5 (`Your feed level is minimal. Change it with /cloud events minimal|normal|verbose|off.`, `Feed level: normal. Kept until the next restart.`, `could not save your feed level: R`, `the console has no feed level: the feed goes to players only`), the feed words from Task 6 (`starting`, `ready`, `leaving`, `stopped`) and the short reasons in `EventLevels.kt`'s `SHORT`. Tell it these are chat replies of a server-admin command, that `X`, `N` and `R` are placeholders, and that the four e2e phrases above must stay exactly as written. Use its output where it differs, keeping every placeholder and the meaning.

- [ ] **Step 2: Update the tests that pin the old wording.**

Run: `git grep -n -e 'their own time' -e 'Nothing automatic' -e 'length of time' -e 'no status' -e 'had no pin' -e 'returns to its own' -e 'its pod is gone' -e 'carries at most' -e 'called' -- agent/common/src/test test/e2e`
Change each hit to the new text and keep what the test is about. If Step 1 changed a short reason or a feed word, apply the same change to `EventLevelsTest`, `CloudFeedTest` and Task 5's `CloudCommandTest` assertions.

- [ ] **Step 3: Apply the text** in `CloudCommand.kt` (and `Execute.kt` only if Step 1 changed something there), one `Style` call per phrase, the same colours as today.

- [ ] **Step 4: Run**

Run: `git add -A agent && NIX build /home/paul/git/spawnery-feed#agents --no-link -L 2>&1 | tail -20`
Expected: the build succeeds.
Run: `git grep -n -e 'is pinned to' -e 'is being killed\.' -e 'servers ran it' -e '" ran it"' -- agent/common/src/main`
Expected: all four phrases are still there.

- [ ] **Step 5: Commit**

```bash
git add -A agent test/e2e
git commit -m "feat(agent): shorter, uniform /cloud replies"
```

---

### Task 9: Docs

**Files:**
- Modify: `docs/guides/cloud-command.md` (node table row line 33; the `/cloud events on|off` paragraph line 189)
- Modify: `docs/guides/updates-and-drain.md` (after the paragraph ending "runs once a second.", about line 334)
- Modify: `docs/guides/upgrading.md` (new section before `## Older installations`, line 122)
- Modify: `docs/guides/on-demand-servers.md` (the paragraph at lines 220-226)
- Modify: `docs/plugin-api/what-a-plugin-can-do.md` (after the "You get the facts, one per transition." paragraph, about line 274)
- Modify: `hack/docs-length.sh` (`docs/guides/upgrading.md` ceiling 1050 → 1150)

**Interfaces:** none. Run every new paragraph through the `humanizer` skill (embedded mode) before writing it: plain prose, no em dashes, no bold labels. Facts below are fixed; the wording is the skill's.

`upgrading.md` stands at 1020 of its 1050-word ceiling, so the note does not fit without raising it. 1150 keeps the script's stated 12 % headroom over the new length.

- [ ] **Step 1: `cloud-command.md`.**
  1. Table row: `| `spawnery.cloud.events` | `/cloud events [minimal\|normal\|verbose\|off]` |`.
  2. Replace the `/cloud events on|off` paragraph with a section on the feed. Without a word the command shows the player's level; `on` means `minimal`, the default. Give the table:

     | Event | `minimal` | `normal` | `verbose` |
     |---|---|---|---|
     | a server or proxy appears | `[+] lobby-x7k2` | `[+] lobby-x7k2 starting`; a proxy `[✓] gateway-4d1 ready` | the operator's note |
     | a server passes its ready gate | | `[✓] lobby-x7k2 ready` | the operator's note |
     | a server or proxy starts to leave | | `[-] lobby-x7k2 leaving` | the operator's note |
     | a server or proxy is gone | `[-] lobby-x7k2` | `[-] lobby-x7k2 stopped` | the operator's note |
     | a warning or a failure | `[!] lobby-x7k2 did not start in time` | the same | the operator's note |
     | anything else | | | the operator's note |

     Then: several lines of one kind in one group within a second become one (`[+] 5 lobby`), and warnings never do. An on-demand member shows as its group and the first six characters of its key (`challenge-3f2b1c`). Hovering over a name shows it in full, and clicking it puts `/cloud info <name>` in the chat box. With LuckPerms the level is the player's meta value `spawnery-feed`, read with inheritance, so `/lp group admin meta set spawnery-feed normal` covers every admin who has not chosen their own, and it survives a roll and a change of proxy. Without LuckPerms the proxy keeps it in memory until it restarts. `off` shows nothing.

- [ ] **Step 2: `updates-and-drain.md`.** One paragraph after the forced-transfer bullets. Ten seconds before the forced transfer, or at once when `forceAfterSeconds` is shorter, each player it will move gets a chat line from the proxy: `You will be reconnected in 10 seconds.` It is the translatable key `spawnery.transfer.warning` with the seconds as its one argument, so a network with its own translations can say it its own way. A player is warned once per leaving proxy, and only when a forced transfer is coming for them: not behind a closed door, not while no other proxy can take them, and not on a server switch, which transfers at once.

- [ ] **Step 3: `upgrading.md`**, before `## Older installations`:

```markdown
## Events about private servers stay on the proxies

Since 0.20.0 an event about a member of an on-demand group, or about the
group itself, reaches proxies only. A backend plugin subscribed to `EventBus`
no longer hears of private servers, which matches `servers()`, where they
never appeared.
```

(after the humanizer pass), and raise the ceiling in `hack/docs-length.sh`.

- [ ] **Step 4: `on-demand-servers.md`.** Replace the paragraph that starts **"The event feed is not split this way, and today that is a limitation."** with one saying that the event feed is split the same way. An event about a member or its group reaches proxies only, so a backend plugin subscribed to events never hears of a private server. In a proxy's chat feed a member shows as its group and the first six characters of its key.

- [ ] **Step 5: `what-a-plugin-can-do.md`.** After "You get the facts, one per transition.", add: on a backend you do not get events about on-demand groups or their members; those reach proxies only, as the network picture does.

- [ ] **Step 6: Check and commit**

Run: `NIX develop /home/paul/git/spawnery-feed -c hack/docs-length.sh`
Expected: exit 0.
Run: `git grep -n -e 'events on|off' -e 'not split this way' -- docs ':!docs/archive' ':!docs/superpowers'`
Expected: nothing.

```bash
git add docs hack/docs-length.sh
git commit -m "feat(docs): feed levels, the transfer warning, private events"
```

---

### Task 10: Final gates

**Files:** none new; whatever the gates ask to regenerate.

- [ ] **Step 1: Run every gate**

Run: `NIX develop /home/paul/git/spawnery-feed -c make test`
Run: `NIX develop /home/paul/git/spawnery-feed -c make lint`
Run: `git add -A && NIX build /home/paul/git/spawnery-feed#agents --no-link`
Run: `NIX build /home/paul/git/spawnery-feed#spawnery-operator --no-link`
Expected: `go test` shows no `FAIL`; `golangci-lint` reports `0 issues.`; both Nix builds succeed. `make test` includes the generated-file, pin and docs-length checks. If it regenerates anything, commit it.

- [ ] **Step 2: End to end** (on `paul-desktop`; it reads the reply phrases Task 8 kept)

Run: `systemd-run --scope --user --property=Delegate=yes -- NIX develop /home/paul/git/spawnery-feed -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e`
Expected: `ok` for `test/e2e`. Read the output file, not the wrapper's exit code.

- [ ] **Step 3: Check what must not have moved**

Run: `git diff --quiet origin/feat/cloud-commands -- internal/podspec/hash_golden_test.go flake.nix charts/spawnery/Chart.yaml charts/spawnery/values.yaml && echo unchanged`
Expected: `unchanged` (no pod hash moved, no version bumped).
Run: `git grep -n 'FeedState\|EventDirection\|direction(' -- agent`
Expected: nothing.

- [ ] **Step 4: Commit anything the gates regenerated**

```bash
git add -A
git commit -m "chore: regenerate after the gates"
```

Only if Step 1 changed files.

- [ ] **Step 5: The staging checklist for the PR description.** The e2e client holds no permission and cannot see chat, so the PR lists what to check on staging with an administrator account (holding `spawnery.cloud.events`, LuckPerms on the proxies):
  1. `/cloud events` answers `minimal`. Scale a group up by one: `[+] <server>` appears, and nothing more until it goes. Scale it down: `[-] <server>`.
  2. `/cloud events normal`, then the same: `starting`, `ready`, `leaving`, `stopped`. `/lp user <you> meta info` shows `spawnery-feed` = `normal`. Reconnect through another proxy: still `normal`.
  3. `/lp group admin meta set spawnery-feed verbose` on a second admin without their own value: that admin sees the operator's notes.
  4. Start an on-demand member with a UUID key: the line names `<group>-<6 characters>`; hover shows the full name; click puts `/cloud info <full name>` in the chat box. On a lobby, a plugin subscribed to `EventBus` logs nothing for it.
  5. `/cloud events off`: nothing during a roll.
  6. Transfer warning: on a proxy group with `spec.update.transfer.forceAfterSeconds: 30`, stay connected and `/cloud retire <your proxy>`. About 20 s after the agent sees itself leaving (within one 30-s resync), chat says `You will be reconnected in 10 seconds.`, once, and 10 s later the client is transferred. With `forceAfterSeconds: 5` the line comes at once and says 5 or less.

The PR is opened by the lead after the final review; the release is its own PR after the merge.

---

## Self-review

- Spec §1 problems: wordy lines → Tasks 3, 6; long on-demand names → Task 4; private names on backends → Task 2; `off` lost on restart → Task 5; uneven replies → Task 8; no transfer warning → Task 7.
- Spec §2 levels and table: Task 3 (rows, short reasons, unknown kinds, warnings read from their name), Task 6 (rendering, collapsing, warnings never collapse, `feedFormat` still wraps every line in `Feed.deliver`). "Gone" for servers is the new `ServerStopped` (Task 1); for proxies `ProxyStopped`.
- Spec §3 names, hover and click: Task 4, wired in Task 6.
- Spec §4 choosing a level: Task 5 (command words, LuckPerms meta with inheritance, memory fallback, unreadable → minimal).
- Spec §5 who receives what: Task 2; docs in Task 9.
- Spec §6 replies: Task 8.
- Spec §7 transfer warning: Task 7 (timing, once, closed door, no target, not on a switch, key and fallback).
- Spec §8 tests: Kotlin in Tasks 3-7; Go in Tasks 1-3; the staging list in Task 10.
- Spec §9 versions: Global Constraints and Task 10, Step 3.
- Review Focus lines and their tests: 1 → `` `a value written by hand is read whatever its case and spacing` ``, `` `the four words and on are read whatever their case and spacing` ``, `` `a store that throws on read means minimal, not a broken feed` ``; 2 → `FeedNamesTest`, `` `a name a quoted argument cannot carry gets no hover and no click` ``; 3 → `` `a row the level hides neither shows nor counts` ``, `` `warnings come first and never collapse` ``; 4 → `` `a warning with no note or no kind still says something` ``; 5 → `` `a forceAfter shorter than the lead warns at once` ``, `` `a forceAfter of zero warns once with one second, before the same pass moves the player` ``, `` `a pass warns a player before it transfers them` ``.
- Types across tasks: `FeedLevel` (Task 3) is used in Tasks 5 and 6; `LevelStore`/`FeedLevels` (Task 5) in Task 6's `Feed`; `shortName`/`Style.subject`/`groupKind` (Task 4) in Task 6; `Sink.Publish(…, private)` (Task 2) is called only by `Recorder`.
