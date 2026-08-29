# Cloud Events and the Chat Feed Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An administrator holding `spawnery.cloud.events` sees cloud events in
chat as they happen, from the same transitions `kubectl get events` shows.

**Architecture:** The operator's `events.EventRecorder` is wrapped once, so a
`CloudEvent` is *derived from* the recorded event rather than computed beside
it. Agents report their interest as a state, and the operator sends events only
to sessions that want them. The wire carries one raw event per transition;
coalescing into one readable line is presentation and happens in the shared
agent module, as a pure function.

**Tech Stack:** Go (operator, controller-runtime, `k8s.io/client-go/tools/events`),
protobuf, Kotlin (`agent/common`), Java (`agent/api`), Brigadier.

**Spec:** [`docs/superpowers/specs/2026-08-27-cloud-api-design.md`](../specs/2026-08-27-cloud-api-design.md)
— §4.4 "Events come from one place", §5.4 "The feed coalesces", §5.5 "Opt-out is
per session, on both sides", §6.4, §8.

---

## Global Constraints

Copied from the spec and from what 7b/7c-1/7c-2 already established. Every task
is bound by all of them.

- **`agent/api` has no platform dependency, no protobuf or gRPC dependency, and
  no Kotlin.** Java only. Every type in a public signature is `java.*` or the
  module's own — `PackagingInvariantTest` fails the build otherwise.
- **`agent/common` names no platform type in code.** The gate is
  `grep -rn 'ProxySelf\|ServerSelf\|CommandSourceStack\|CommandSource' agent/common/src/main | grep -vE ':\s*(\*|//)'`
  and it must be empty. Comments naming them are fine and expected.
- **The shared module compiles against Paper's Brigadier and runs against
  Velocity's.** `BrigadierCompatibilityTest` guards the measured two-class gap
  (`ContextChain`, `ContextChain$Stage`). Do not use either.
- **No cross-namespace anything.** A pod's namespace is its horizon, in every
  direction, for every verb. Bounds are structural where they can be.
- **No guarantee an event is delivered** (§8). Events are a feed, not a ledger.
  An agent that was disconnected missed what happened while it was gone; the
  mirror it re-syncs on reconnect is the correction.
- **No persistence of a player's preferences** (§8, §5.5). The opt-out lives
  for the session on both platforms.
- Every build/test command needs the prefix
  `nix --extra-experimental-features 'nix-command flakes' develop -c`.
- **The whole-tree Go suite needs `-p 1`.** This machine has 7 GB and parallel
  envtest packages each start their own etcd and apiserver; three at once kills
  the VM. Single packages are fine without it.
- `make agent-test` needs `CONTAINER=podman` and `TMPDIR="$HOME/.cache/spawnery-tmp"`.
- Conventional Commits, English subject, and every commit ends with exactly:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **Never push, never merge, never create a tag.** Nothing leaves the machine
  until the whole milestone is done and the user says so.

---

## Facts measured before this plan was written

Written down because each one closed a design question, and re-measuring them
is cheaper than rediscovering why.

**Paper and Velocity disagree about whether a player is a command source, and
that decides the adapter split.** On Velocity,
`com.velocitypowered.api.proxy.Player extends CommandSource` — the audience
type *is* the command source type. On Paper,
`io.papermc.paper.command.brigadier.CommandSourceStack` is an interface with
seven accessors and **no factory**: `getSender`, `getLocation`,
`getPlayerOrThrow`, `withLocation`, `withExecutor` and so on. There is no way
to turn a `Player` from `Bukkit.getOnlinePlayers()` into one.

So the feed **cannot** reuse `SourceAdapter<S>`: a `holders(): List<S>` method
would be unimplementable on Paper. Task 4 introduces a separate, non-generic
`FeedAudience` instead. Do not try to unify them.

**The operator records 30 events through `events.EventRecorder`**, an
interface with a single method:

```go
Eventf(regarding runtime.Object, related runtime.Object,
    eventtype, reason, action, note string, args ...interface{})
```

It is constructed in five places — four in `internal/controller/setup.go`
(server, network, servergroup, proxygroup) and one in
`cmd/spawnery-operator/main.go` (certs). Wrapping the interface at those five
sites catches all 30 call sites and every one added later.

**The phase transition that matters is one line.**
`internal/controller/server_controller.go:975`, inside `if d.Next != current`:

```go
r.Recorder.Eventf(srv, nil, corev1.EventTypeNormal, d.Reason, actionSyncStatus,
    "phase %s -> %s: %s", current, d.Next, d.Message)
```

The phases are `Pending`, `Starting`, `Ready`, `Draining`, `Terminating`,
`Failed` (`internal/phase/phase.go`).

**The fan-outs differ.** `internal/proxyreg/fleet.go:336` has a `broadcast`
helper; `internal/serverreg/registry.go` has none — its only fan-out is
`Resync`. Task 2 adds one to `serverreg` mirroring `Fleet`'s.

---

## Two places this plan departs from the spec, deliberately

**§3.2's sketch nests writes under a `Lifecycle` sub-interface and has `retire`
return a `RetireResult`.** What shipped in 7b and 7c-2 is flat on `SpawneryApi`
— `retire(String)` returning `CompletionStage<Void>`, `boost(...)`,
`stopBoosts(...)` — because `RetireResult` would have carried only the name the
caller passed in. That decision is made and released; this plan follows it and
adds `EventBus events()` flat alongside them rather than reintroducing a
nesting that exists nowhere else.

**§6.4's third bullet is stale.** It says an E2E scenario should assert
"`/cloud start` moved the `ServerGroup`'s `minReplicas`". `/cloud start` does
not touch the group's spec — that is the whole point of `ScaleBoost` (§4.4),
and the operator holds no write on `servergroups`. The equivalent assertion is
that a `ScaleBoost` exists, the group's `status.boostedReplicas` rose, and no
server retired. 7c-1 and 7c-2 already cover the first two; Task 7 records the
correction in the spec rather than leaving a bullet nobody can satisfy.

---

## File Structure

| File | Responsibility |
|---|---|
| `proto/spawnery/agent/v1alpha1/agent.proto` | `CloudEvent`, `EventInterest`; both added to the two operator→agent oneofs and one agent→operator oneof each |
| `internal/cloudevent/recorder.go` (new) | The `events.EventRecorder` wrapper: forwards, and derives a `CloudEvent` into a sink. Its own package so `serverreg`, `proxyreg` and `controller` can all import it without a cycle |
| `internal/cloudevent/derive.go` (new) | The pure part: one recorded event → zero or one `CloudEvent`. Decides what is worth a chat line |
| `internal/serverreg/registry.go` | `broadcast`, `SetInterest`, `Publish` |
| `internal/proxyreg/fleet.go` | `SetInterest`, `Publish` |
| `internal/agentserver/server.go` | Routes `EventInterest` from both stream kinds to the right registry |
| `cmd/spawnery-operator/main.go`, `internal/controller/setup.go` | Wrap the five recorders |
| `agent/common/.../CloudFeed.kt` (new) | `coalesce`, a pure function over a list of events |
| `agent/common/.../FeedState.kt` (new) | Who opted out, and whether this agent is interested at all |
| `agent/common/.../CloudCommand.kt` | `/cloud events on\|off` |
| `agent/common/.../SourceAdapter.kt` | A third method: `playerId` |
| `agent/api/.../EventBus.java`, `CloudEventInfo.java` (new) | The plugin-facing subscription |
| `agent/paper/.../PaperAudience.kt`, `agent/velocity/.../VelocityAudience.kt` (new) | The two four-line implementations |

---

## Task 1: `CloudEvent`, derived and not computed beside

The property §4.4 asks for is that **the chat feed shows the events
`kubectl get events` shows**. The only way to get that structurally rather than
by discipline is to make one derive from the other. A wrapper around
`events.EventRecorder` does it: there is no path that records without also
deriving, because there is no second path.

**Files:**
- Create: `internal/cloudevent/derive.go`, `internal/cloudevent/derive_test.go`
- Create: `internal/cloudevent/recorder.go`, `internal/cloudevent/recorder_test.go`
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`

**Interfaces:**
- Produces: `cloudevent.Sink` (interface, one method
  `Publish(namespace string, ev *agentpb.CloudEvent)`), `cloudevent.Recorder`
  (implements `events.EventRecorder`), `cloudevent.Derive(regarding runtime.Object,
  eventtype, reason, note string) (namespace string, ev *agentpb.CloudEvent, ok bool)`.
- Consumes: nothing.

- [ ] **Step 1: Add the messages to the proto**

In `proto/spawnery/agent/v1alpha1/agent.proto`, before `message RequestError`:

```protobuf
// CloudEvent is something that happened, on its way to somebody's chat.
//
// **A feed and not a ledger.** An agent that was disconnected missed what
// happened while it was gone, and nothing here resends it: the NetworkState it
// re-syncs on reconnect is the correction, and it is a better one than a
// replay would be -- it says what is true now rather than what was true in an
// order nobody was watching.
//
// It is derived from the event the operator records to Kubernetes rather than
// computed beside it. Two independent derivations of the same fact eventually
// disagree, and the one in the chat is the one nobody can audit.
message CloudEvent {
  // The kind, as a coarse verb a person reads: SERVER_READY, SERVER_FAILED,
  // and so on. A string and not an enum, for the reason NetworkState.phase is
  // a string: the operator's vocabulary gains values, and an agent older than
  // a value must show it rather than fail to parse the message it arrived in.
  string kind = 1;
  // The object this is about -- a server name, or a group name for an event
  // about a group.
  string subject = 2;
  // The group the subject belongs to, empty when the subject is the group.
  // What the agent coalesces by.
  string group = 3;
  // One sentence for a person. The operator's own words, so that rewording
  // them in an agent cannot make the chat disagree with kubectl.
  string message = 4;
  // Whether this is the ordinary case or the one somebody should look at.
  bool warning = 5;
}
```

Add to the two operator→agent oneofs, keeping the existing numbering
untouched. Find `message OperatorToServer` and `message OperatorToProxy` and
append one field to each, using the next free number in that oneof.

- [ ] **Step 2: Regenerate**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make proto`
Expected: `internal/agentpb/agent.pb.go` and the Java stubs under
`agent/common/src/proto/java/` change; new `CloudEvent*.java` files appear.

- [ ] **Step 3: Write the failing test for the pure derivation**

Create `internal/cloudevent/derive_test.go`. Note the licence header — copy the
16-line Apache header from `internal/agentserver/writer.go`.

```go
package cloudevent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func TestAServerPhaseTransitionBecomesAnEvent(t *testing.T) {
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-a3f9", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
	}

	ns, ev, ok := Derive(srv, corev1.EventTypeNormal, "ReadyGatePassed",
		"phase Starting -> Ready: the agent reported ready")
	if !ok {
		t.Fatal("a phase transition produced no event")
	}
	if ns != "minecraft" {
		t.Errorf("namespace = %q, want the object's own", ns)
	}
	if ev.GetSubject() != "lobby-a3f9" || ev.GetGroup() != "lobby" {
		t.Errorf("subject/group = %q/%q, want lobby-a3f9/lobby", ev.GetSubject(), ev.GetGroup())
	}
	if ev.GetKind() != "ReadyGatePassed" {
		t.Errorf("kind = %q, want the operator's own reason", ev.GetKind())
	}
	// The operator's words, unchanged. Rewording them here is how a chat feed
	// comes to disagree with kubectl about the same event.
	if ev.GetMessage() != "phase Starting -> Ready: the agent reported ready" {
		t.Errorf("message = %q, want the recorded note verbatim", ev.GetMessage())
	}
	if ev.GetWarning() {
		t.Error("a Normal event was marked a warning")
	}
}

func TestAWarningKeepsItsSeverity(t *testing.T) {
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-a3f9", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
	}

	_, ev, ok := Derive(srv, corev1.EventTypeWarning, "ReadinessLost", "the pod stopped answering")
	if !ok {
		t.Fatal("a warning produced no event")
	}
	if !ev.GetWarning() {
		t.Error("a Warning event was not marked as one, so the feed cannot tell it apart")
	}
}

func TestAnObjectWithNoNamespaceProducesNothing(t *testing.T) {
	// The certs recorder reports on Secrets in spawnery-system, which is not a
	// game namespace and has no agents in it. More to the point, an object
	// this cannot name is one the feed cannot address: a CloudEvent with no
	// namespace has nowhere to go, and inventing one is worse than dropping it.
	if _, _, ok := Derive(nil, corev1.EventTypeNormal, "Whatever", "note"); ok {
		t.Error("a nil object produced an event")
	}
}

func TestAGroupEventNamesTheGroupAsBothSubjectAndGroup(t *testing.T) {
	// So that coalescing has something to group by without a special case:
	// every event has a group, and for a group's own event it is itself.
	g := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft"},
	}

	_, ev, ok := Derive(g, corev1.EventTypeNormal, "ScalingLimited", "at maxReplicas")
	if !ok {
		t.Fatal("a group event produced nothing")
	}
	if ev.GetSubject() != "lobby" || ev.GetGroup() != "lobby" {
		t.Errorf("subject/group = %q/%q, want lobby/lobby", ev.GetSubject(), ev.GetGroup())
	}
}
```

- [ ] **Step 4: Run it and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/cloudevent/ -count=1`
Expected: FAIL, `undefined: Derive`.

- [ ] **Step 5: Write `Derive`**

Create `internal/cloudevent/derive.go` with the Apache header, then:

```go
// Package cloudevent turns what the operator records into what an
// administrator reads in chat.
//
// Its own package because three packages need it and none may import the
// others: internal/controller records the events, and internal/serverreg and
// internal/proxyreg deliver them. A copy in each would be the two independent
// derivations §4.4 of the design exists to prevent.
package cloudevent

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
)

// Derive turns one recorded event into at most one CloudEvent.
//
// **The note is carried verbatim.** It is the operator's own sentence, already
// written for a person, and rewording it here is exactly how a chat feed comes
// to disagree with `kubectl get events` about the same fact -- which is the
// property §4.4 of the design is protecting.
//
// It reports ok=false for anything the feed cannot address or would not want:
// an object with no namespace has nowhere to go, and a kind nobody would read
// in chat is noise that teaches people to turn the feed off.
func Derive(
	regarding runtime.Object, eventtype, reason, note string,
) (string, *agentpb.CloudEvent, bool) {
	var namespace, subject, group string
	switch o := regarding.(type) {
	case *spawneryv1alpha1.Server:
		namespace, subject, group = o.Namespace, o.Name, o.Spec.GroupRef.Name
	case *spawneryv1alpha1.ServerGroup:
		// Its own group, so that a coalescing rule needs no special case for
		// events about groups.
		namespace, subject, group = o.Namespace, o.Name, o.Name
	case *spawneryv1alpha1.ProxyGroup:
		namespace, subject, group = o.Namespace, o.Name, o.Name
	default:
		// Networks, Secrets, and anything added later. Silence rather than a
		// guess: a kind this has never seen is one nobody decided was worth a
		// chat line, and defaulting to "show it" would make every new event
		// type a surprise in somebody's chat.
		return "", nil, false
	}
	if namespace == "" || subject == "" {
		return "", nil, false
	}
	return namespace, &agentpb.CloudEvent{
		Kind:    reason,
		Subject: subject,
		Group:   group,
		Message: note,
		Warning: eventtype == corev1.EventTypeWarning,
	}, true
}
```

- [ ] **Step 6: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/cloudevent/ -count=1`
Expected: PASS.

- [ ] **Step 7: Write the failing test for the wrapper**

Create `internal/cloudevent/recorder_test.go` with the Apache header:

```go
package cloudevent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
)

type recordedCall struct {
	regarding             runtime.Object
	eventtype, reason     string
	action, note          string
	args                  []interface{}
}

type fakeRecorder struct{ calls []recordedCall }

func (f *fakeRecorder) Eventf(
	regarding runtime.Object, related runtime.Object,
	eventtype, reason, action, note string, args ...interface{},
) {
	f.calls = append(f.calls, recordedCall{regarding, eventtype, reason, action, note, args})
}

type fakeSink struct {
	namespaces []string
	events     []*agentpb.CloudEvent
}

func (f *fakeSink) Publish(namespace string, ev *agentpb.CloudEvent) {
	f.namespaces = append(f.namespaces, namespace)
	f.events = append(f.events, ev)
}

func aServer() *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-a3f9", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
	}
}

func TestTheWrapperStillRecordsToKubernetes(t *testing.T) {
	// The first thing to protect. A wrapper that fed the chat and swallowed
	// the Kubernetes event would be invisible in every test that looks at the
	// feed, and would take `kubectl get events` with it.
	inner, sink := &fakeRecorder{}, &fakeSink{}
	r := Recorder{Inner: inner, Sink: sink}

	r.Eventf(aServer(), nil, corev1.EventTypeNormal, "ReadyGatePassed", "SyncStatus",
		"phase %s -> %s", "Starting", "Ready")

	if len(inner.calls) != 1 {
		t.Fatalf("the inner recorder saw %d calls, want one", len(inner.calls))
	}
	if inner.calls[0].reason != "ReadyGatePassed" {
		t.Errorf("reason = %q, want it passed through unchanged", inner.calls[0].reason)
	}
}

func TestTheFeedGetsTheSameSentenceKubectlDoes(t *testing.T) {
	// The property §4.4 asks for, asserted rather than assumed: the note is
	// formatted once and both sides get that one string.
	inner, sink := &fakeRecorder{}, &fakeSink{}
	r := Recorder{Inner: inner, Sink: sink}

	r.Eventf(aServer(), nil, corev1.EventTypeNormal, "ReadyGatePassed", "SyncStatus",
		"phase %s -> %s", "Starting", "Ready")

	if len(sink.events) != 1 {
		t.Fatalf("the sink saw %d events, want one", len(sink.events))
	}
	if got := sink.events[0].GetMessage(); got != "phase Starting -> Ready" {
		t.Errorf("the feed's message = %q, want the formatted note", got)
	}
	if sink.namespaces[0] != "minecraft" {
		t.Errorf("published to %q, want the object's namespace", sink.namespaces[0])
	}
}

func TestAnEventTheFeedDoesNotWantIsStillRecorded() {}
```

Replace that last stub with the real test:

```go
func TestAnEventTheFeedDoesNotWantIsStillRecorded(t *testing.T) {
	// Derive returns ok=false for a Secret. That must not cost Kubernetes its
	// event: the feed is the optional half.
	inner, sink := &fakeRecorder{}, &fakeSink{}
	r := Recorder{Inner: inner, Sink: sink}

	r.Eventf(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "spawnery-system"}},
		nil, corev1.EventTypeNormal, "Rotated", "Rotate", "the CA rotated")

	if len(inner.calls) != 1 {
		t.Errorf("a Secret event was not recorded to Kubernetes: %d calls", len(inner.calls))
	}
	if len(sink.events) != 0 {
		t.Errorf("a Secret event reached the feed: %+v", sink.events)
	}
}

func TestANilSinkIsNotACrash(t *testing.T) {
	// The certs recorder in main.go is built before the fan-outs exist. A
	// wrapper that panicked on a nil sink would turn an ordering detail into a
	// startup crash, and the ordering is not this type's business.
	inner := &fakeRecorder{}
	r := Recorder{Inner: inner}

	r.Eventf(aServer(), nil, corev1.EventTypeNormal, "ReadyGatePassed", "SyncStatus", "up")

	if len(inner.calls) != 1 {
		t.Error("a nil sink cost Kubernetes its event")
	}
}
```

- [ ] **Step 8: Run it and watch it fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/cloudevent/ -count=1`
Expected: FAIL, `undefined: Recorder`.

- [ ] **Step 9: Write the wrapper**

Create `internal/cloudevent/recorder.go` with the Apache header:

```go
package cloudevent

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// Sink is where derived events go. The two fan-outs implement it.
//
// One method, and no error: a feed nobody is watching must not be able to fail
// a reconcile. Delivery is best-effort by design -- see agentpb.CloudEvent.
type Sink interface {
	Publish(namespace string, ev *agentpb.CloudEvent)
}

// Recorder records an event to Kubernetes and derives a CloudEvent from the
// same call.
//
// **One seam and not thirty.** The operator records through this interface in
// thirty places across five controllers, and every one of them now feeds the
// chat without knowing it does. The alternative -- a second call beside each
// recorder call -- is thirty chances to forget, and forgetting is invisible:
// the Kubernetes event is still there, so nothing looks broken except that one
// kind of thing never appears in chat.
//
// It implements events.EventRecorder, so wrapping is a one-line change at each
// construction site and no call site changes at all.
type Recorder struct {
	// Inner is the manager's own recorder. Required.
	Inner events.EventRecorder
	// Sink is where the feed's copy goes. Nil means no feed, which is a state
	// and not a bug: see the test.
	Sink Sink
}

// Eventf records, then derives.
//
// Kubernetes first, deliberately. If deriving ever panicked, the recorded
// event would already be queued -- and the feed is the half this project can
// afford to lose.
//
// The note is formatted once and the same string goes both ways, which is what
// makes "the chat shows what kubectl shows" true of the text and not only of
// the fact.
func (r Recorder) Eventf(
	regarding runtime.Object, related runtime.Object,
	eventtype, reason, action, note string, args ...interface{},
) {
	r.Inner.Eventf(regarding, related, eventtype, reason, action, note, args...)
	if r.Sink == nil {
		return
	}
	formatted := note
	if len(args) > 0 {
		formatted = fmt.Sprintf(note, args...)
	}
	if namespace, ev, ok := Derive(regarding, eventtype, reason, formatted); ok {
		r.Sink.Publish(namespace, ev)
	}
}
```

- [ ] **Step 10: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/cloudevent/ -count=1`
Expected: PASS, seven tests.

- [ ] **Step 11: Mutate each claim on its own**

Four mutations, one at a time, restoring between each. Each must fail, and you
must read *which* test failed — a mutation that fails the wrong test has told
you nothing.

1. In `Eventf`, delete the `r.Inner.Eventf(...)` line.
   Expected: `TestTheWrapperStillRecordsToKubernetes` fails.
2. In `Eventf`, replace `formatted` in the `Derive` call with `note`.
   Expected: `TestTheFeedGetsTheSameSentenceKubectlDoes` fails — the feed would
   carry `phase %s -> %s`.
3. In `Derive`, change the `default:` branch to return the event anyway with
   `namespace` read from a `metav1.Object` assertion.
   Expected: `TestAnEventTheFeedDoesNotWantIsStillRecorded` fails.
4. In `Derive`'s `*ServerGroup` case, set `group` to `""`.
   Expected: `TestAGroupEventNamesTheGroupAsBothSubjectAndGroup` fails.

- [ ] **Step 12: Commit**

```bash
git add -A
git commit -F - <<'EOF'
feat(operator): CloudEvent, derived from the event kubectl already shows

<body: the one-seam argument, and that the note is formatted once so both
sides get the same sentence. Record the four mutations and which test each
one felled.>

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
```

---

## Task 2: Interest is a state, and the operator honours it

Without this the operator would broadcast every event to every pod in the
namespace, which for a twenty-server rolling update is four hundred messages
delivered to nobody. The agent knows whether anyone is watching; the operator
does not and cannot.

**A state and not a subscribe/unsubscribe pair**, matching `PlayerCount`,
`PlayerRoster` and `NetworkState`: every message carries the whole answer, a
reconnect needs no catch-up, and a dropped one costs one report of freshness.
A subscribe/unsubscribe pair would need the operator to remember across a
make-before-break renewal, which `CloudRequest`'s own comment explains it
deliberately does not do.

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Modify: `internal/serverreg/registry.go`, `internal/proxyreg/fleet.go`
- Modify: `internal/agentserver/server.go`
- Test: `internal/serverreg/registry_test.go`, `internal/proxyreg/fleet_test.go`

**Interfaces:**
- Consumes: `cloudevent.Sink` from Task 1.
- Produces: `(*serverreg.Registry).SetInterest(podUID string, wanted bool)`,
  `(*serverreg.Registry).Publish(namespace string, ev *agentpb.CloudEvent)`,
  and the identical pair on `*proxyreg.Fleet`. Both types therefore satisfy
  `cloudevent.Sink`.

- [ ] **Step 1: Add `EventInterest` to the proto**

```protobuf
// EventInterest says whether this agent has anybody to show events to.
//
// **A state and not a subscription.** Every message carries the whole answer,
// so a reconnect needs no catch-up and the operator remembers nothing across
// the make-before-break renewal that briefly runs two streams -- which is the
// same reason CloudRequest ids are not remembered either.
//
// The agent sends one whenever the answer changes: an administrator holding
// the permission joined or left, or somebody typed `/cloud events off`. It
// sends one on every new stream regardless, because the operator's answer for
// a session it has never seen is "no".
//
// It is an optimisation and not a bound. An agent that lied and asked for
// events would receive events about its own namespace, which it can already
// see in its NetworkState.
message EventInterest {
  bool wanted = 1;
}
```

Add `EventInterest event_interest = N;` to the `ServerMessage` and
`ProxyMessage` oneofs, using the next free number in each.

- [ ] **Step 2: Regenerate**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c make proto`

- [ ] **Step 3: Write the failing tests for `serverreg`**

Add to `internal/serverreg/registry_test.go`. It is an external test package
(`package serverreg_test`) and it already has a `newRegistry(t, opts,
objects...)` helper at line 47 that builds a fake client and a clock — use it
rather than building a second fixture.

Both helpers below are **bounded**. A test that proves nothing arrives by
receiving on a channel forever hangs the suite instead of failing it, which is
a mistake this project has made before.

```go
// cloudEventsIn drains a session's outbox for a short grace period and returns
// the CloudEvents that arrived. Bounded, and it skips the other messages a
// joining session is sent -- the NetworkState comes first on every stream.
func cloudEventsIn(ch <-chan *agentpb.OperatorToServer) []*agentpb.CloudEvent {
	var got []*agentpb.CloudEvent
	deadline := time.After(250 * time.Millisecond)
	for {
		select {
		case msg := <-ch:
			if ev := msg.GetCloudEvent(); ev != nil {
				got = append(got, ev)
			}
		case <-deadline:
			return got
		}
	}
}

func TestAnEventReachesOnlyTheSessionsThatWantOne(t *testing.T) {
	// The whole point of the interest state. A broadcast that ignored it would
	// pass every other test in this file.
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"))
	watching, leaveA, err := r.Join(context.Background(), "ns", "pod-watching")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveA()
	quiet, leaveB, err := r.Join(context.Background(), "ns", "pod-quiet")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveB()
	r.SetInterest("pod-watching", true)

	r.Publish("ns", &agentpb.CloudEvent{Kind: "ReadyGatePassed", Subject: "lobby-a"})

	if got := cloudEventsIn(watching); len(got) != 1 || got[0].GetSubject() != "lobby-a" {
		t.Errorf("the interested session got %+v, want one event for lobby-a", got)
	}
	if got := cloudEventsIn(quiet); len(got) != 0 {
		t.Errorf("a session that never asked for events received %+v", got)
	}
}

func TestInterestIsForgottenWithTheSession(t *testing.T) {
	// Otherwise the map grows for the operator's lifetime, and a pod that
	// reconnects into a fresh session would inherit an answer it never gave.
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"))
	_, leave, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	r.SetInterest("pod-a", true)
	leave()

	if r.Interested("pod-a") {
		t.Error("interest outlived the session that declared it")
	}
}

func TestAnEventDoesNotCrossANamespace(t *testing.T) {
	// Structural everywhere else in this project; a check here, because
	// Publish takes the namespace as an argument rather than deriving it from
	// a token. A check is exactly what needs its own test.
	r := newRegistry(t, serverreg.Options{}, group("ns", "lobby"), group("other", "lobby"))
	other, leave, err := r.Join(context.Background(), "other", "pod-other")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	r.SetInterest("pod-other", true)

	r.Publish("ns", &agentpb.CloudEvent{Kind: "ReadyGatePassed", Subject: "lobby-a"})

	if got := cloudEventsIn(other); len(got) != 0 {
		t.Errorf("an event reached a session in another namespace: %+v", got)
	}
}
```

`Interested` is exported for these tests. That is a deliberate cost: the
alternative is asserting a leak through a channel that stays empty either way,
which is the same as not asserting it.

- [ ] **Step 4: Run and watch them fail**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/serverreg/ -count=1`
Expected: FAIL, `r.SetInterest undefined`.

- [ ] **Step 5: Implement on `serverreg`**

`internal/serverreg/registry.go` has no `broadcast`; add one mirroring
`proxyreg/fleet.go:336`, then:

```go
// SetInterest records whether this session's agent has anybody to show events
// to. Unknown pods are ignored: a report can arrive from a session that has
// just been displaced by a renewal, and creating an entry for it would leak
// one per reconnect.
func (r *Registry) SetInterest(podUID string, wanted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[podUID]; ok {
		s.wantsEvents = wanted
	}
}

// Interested reports what SetInterest last recorded. For tests.
func (r *Registry) Interested(podUID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[podUID]
	return ok && s.wantsEvents
}

// Publish sends one event to every session in the namespace that asked for
// events.
//
// It implements cloudevent.Sink, and it reports nothing: this is called from
// inside a reconcile, and a feed nobody is watching must not be able to fail
// one. A full outbox drops the event, which agentpb.CloudEvent documents as
// the ordinary case rather than an error.
func (r *Registry) Publish(namespace string, ev *agentpb.CloudEvent) {
	r.broadcast(namespace, func(s *session) *agentpb.OperatorToServer {
		if !s.wantsEvents {
			return nil
		}
		return &agentpb.OperatorToServer{
			Message: &agentpb.OperatorToServer_CloudEvent{CloudEvent: ev},
		}
	})
}
```

Make `broadcast` skip a `nil` build result rather than sending it — that is
what lets one broadcast serve both "everyone" and "only the interested", and
it needs its own line of comment saying so.

Add `wantsEvents bool` to the `session` struct. It is read and written under
`r.mu` like every other session field.

- [ ] **Step 6: Run the tests**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/serverreg/ -count=1`
Expected: PASS.

- [ ] **Step 7: Do the same for `proxyreg`**

Identical three methods on `*Fleet`, using its existing `broadcast`, plus the
same three tests in `internal/proxyreg/fleet_test.go` against
`*agentpb.OperatorToProxy`. Do not skip the tests because the code looks the
same: the two types have different session structs, and "it is the same shape"
is how the copy that is subtly not the same gets in.

- [ ] **Step 8: Route the message in `agentserver`**

In `internal/agentserver/server.go`, in the server-direction switch:

```go
case *agentpb.ServerMessage_EventInterest:
	s.opts.Servers.SetInterest(id.PodUID, m.EventInterest.GetWanted())
```

and the matching `*agentpb.ProxyMessage_EventInterest` case calling
`s.opts.Proxies.SetInterest(...)`. Both `ServerFanout` and `ProxyFleet` are
narrow interfaces in that package — add `SetInterest(podUID string, wanted bool)`
to each, which is what keeps the endpoint's reach readable.

- [ ] **Step 9: Wrap the five recorders**

In `internal/controller/setup.go`, the four sites become:

```go
Recorder: cloudevent.Recorder{Inner: mgr.GetEventRecorder("server"), Sink: opts.Events},
```

Add `Events cloudevent.Sink` to `controller.Options`. In
`cmd/spawnery-operator/main.go`, pass a sink that fans out to both registries:

```go
// Both fan-outs, because an administrator may be on a backend or on a proxy
// and the operator cannot know which. A type rather than a closure so the nil
// case is one thing to reason about.
type bothFanouts struct {
	servers *serverreg.Registry
	proxies *proxyreg.Fleet
}

func (b bothFanouts) Publish(namespace string, ev *agentpb.CloudEvent) {
	b.servers.Publish(namespace, ev)
	b.proxies.Publish(namespace, ev)
}
```

Leave the certs recorder in `main.go` unwrapped, and say why in a comment: it
reports on Secrets in `spawnery-system`, which `Derive` drops anyway, so
wrapping it would add a construction that can only ever produce nothing.

- [ ] **Step 10: Run the affected suites**

Run:
```
nix --extra-experimental-features 'nix-command flakes' develop -c go test -p 1 \
  ./internal/serverreg/ ./internal/proxyreg/ ./internal/agentserver/ \
  ./internal/cloudevent/ ./internal/controller/ -count=1
```
Expected: all `ok`.

- [ ] **Step 11: Mutate the interest bound**

In `Registry.Publish`, delete the `if !s.wantsEvents { return nil }`.
Expected: `TestAnEventReachesOnlyTheSessionsThatWantOne` fails in both
`serverreg` and `proxyreg`. Restore.

- [ ] **Step 12: Commit**

---

## Task 3: The coalescing, as a pure function

§5.4: a rolling update of a ten-server group produces ten `Ready` transitions
in a few seconds, and ten lines is a feed people turn off — which costs more
than it gives.

**This lives in the agent and not the operator.** The wire carries one raw
event per transition so that `kubectl` and the feed stay the same list of
facts; collapsing them into a sentence is presentation, and presentation
belongs where the message is rendered. It also means a plugin subscribing
through `EventBus` (Task 6) receives events rather than somebody else's
summary.

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudFeed.kt`
- Create: `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudFeedTest.kt`

**Interfaces:**
- Produces: `fun coalesce(events: List<CloudEvent>): List<String>` in package
  `cloud.spawnery.agent`, where `CloudEvent` is `cloud.spawnery.agent.pb.CloudEvent`.

- [ ] **Step 1: Write the failing tests**

Kotlin files in this project carry **no licence header** — do not add one.

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

private fun event(kind: String, subject: String, group: String, warning: Boolean = false): CloudEvent =
    CloudEvent.newBuilder()
        .setKind(kind).setSubject(subject).setGroup(group)
        .setMessage("$subject: $kind").setWarning(warning)
        .build()

class CloudFeedTest {
    @Test
    fun `one event is one line and says what happened`() {
        val lines = coalesce(listOf(event("ReadyGatePassed", "lobby-a3f9", "lobby")))

        assertEquals(1, lines.size)
        assertTrue(lines.single().contains("lobby-a3f9"), lines.single())
    }

    @Test
    fun `many of one kind in one group collapse to one line that names them`() {
        // The case section 5.4 exists for: a rolling update of a ten-server
        // group. Ten lines is a feed people turn off.
        val lines = coalesce(
            listOf(
                event("ReadyGatePassed", "lobby-a3f9", "lobby"),
                event("ReadyGatePassed", "lobby-b71c", "lobby"),
                event("ReadyGatePassed", "lobby-c02e", "lobby"),
            ),
        )

        assertEquals(1, lines.size)
        val line = lines.single()
        assertTrue(line.contains("3"), "the count is missing: $line")
        assertTrue(line.contains("lobby"), "the group is missing: $line")
        // The names, because "3 servers are ready" leaves an admin unable to
        // tell which -- and the one they were waiting for is the question.
        assertTrue(
            line.contains("lobby-a3f9") && line.contains("lobby-b71c") && line.contains("lobby-c02e"),
            "the names are missing: $line",
        )
    }

    @Test
    fun `two kinds in one group stay two lines`() {
        // Collapsing across kinds would produce "4 things happened in lobby",
        // which is the shape of a summary that says nothing.
        val lines = coalesce(
            listOf(
                event("ReadyGatePassed", "lobby-a3f9", "lobby"),
                event("Terminating", "lobby-b71c", "lobby"),
            ),
        )

        assertEquals(2, lines.size)
    }

    @Test
    fun `the same kind in two groups stays two lines`() {
        val lines = coalesce(
            listOf(
                event("ReadyGatePassed", "lobby-a3f9", "lobby"),
                event("ReadyGatePassed", "arena-1", "arena"),
            ),
        )

        assertEquals(2, lines.size)
        assertTrue(lines.any { it.contains("lobby") }, lines.toString())
        assertTrue(lines.any { it.contains("arena") }, lines.toString())
    }

    @Test
    fun `a warning is never collapsed into a normal line`() {
        // A failure hidden inside "3 servers ready" is the one event in this
        // feed somebody actually needs to see.
        val lines = coalesce(
            listOf(
                event("ReadyGatePassed", "lobby-a3f9", "lobby"),
                event("PodRejected", "lobby-b71c", "lobby", warning = true),
            ),
        )

        assertEquals(2, lines.size)
        assertTrue(lines.any { it.contains("lobby-b71c") }, "the warning vanished: $lines")
    }

    @Test
    fun `a lone warning keeps the operator's own sentence`() {
        // A count and a name is the right shape for ten identical successes
        // and the wrong shape for one failure: the sentence is what says why.
        val lines = coalesce(
            listOf(
                CloudEvent.newBuilder()
                    .setKind("PodRejected").setSubject("lobby-b71c").setGroup("lobby")
                    .setMessage("the node had no room").setWarning(true).build(),
            ),
        )

        assertTrue(lines.single().contains("the node had no room"), lines.single())
    }

    @Test
    fun `an empty window produces no lines rather than an empty one`() {
        assertTrue(coalesce(emptyList()).isEmpty())
    }

    @Test
    fun `a very wide collapse names some and counts the rest`() {
        // Forty names is not a chat line. The bound is stated rather than
        // discovered by whoever first scales a group to forty.
        val many = (1..40).map { event("ReadyGatePassed", "lobby-%02d".format(it), "lobby") }

        val line = coalesce(many).single()

        assertTrue(line.contains("40"), "the total is missing: $line")
        assertTrue(line.length < 200, "the line is ${line.length} characters: $line")
        assertTrue(line.contains("lobby-01"), "it named none of them: $line")
    }
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `cd agent && nix --extra-experimental-features 'nix-command flakes' develop -c gradle :common:test --tests '*CloudFeedTest*' --console=plain --offline`
Expected: FAIL, `Unresolved reference 'coalesce'`.

- [ ] **Step 3: Write `coalesce`**

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/**
 * How many names a collapsed line prints before it starts counting.
 *
 * Six, which is a chat line and not a wall. The alternative -- printing all of
 * them -- turns a forty-server scale-up into a message that pushes everything
 * else off the screen, which is a feed people turn off for the same reason
 * ten separate lines is.
 */
private const val NAMES_SHOWN = 6

/**
 * Collapses one window of events into the lines a person reads.
 *
 * **Pure, and that is the whole design.** It takes a list and returns strings:
 * no clock, no platform, no I/O. The window is somebody else's problem (see
 * [CloudFeedBuffer]), which is what makes every rule here assertable as one
 * table of inputs and outputs.
 *
 * **It runs on the agent and not the operator.** The wire carries one event per
 * transition so that the feed and `kubectl get events` stay the same list of
 * facts; collapsing them into a sentence is presentation, and a plugin
 * subscribing through the API gets events rather than somebody else's summary.
 *
 * Grouped by kind and group, never across either: "4 things happened in lobby"
 * is the shape of a summary that says nothing.
 *
 * **Warnings are never collapsed.** A failure hidden inside "3 servers ready"
 * is the one event in this feed somebody actually needs to see, and it keeps
 * the operator's own sentence -- a count and a name is the right shape for ten
 * identical successes and the wrong shape for one failure.
 */
fun coalesce(events: List<CloudEvent>): List<String> {
    val lines = mutableListOf<String>()
    val (warnings, ordinary) = events.partition { it.warning }

    for (w in warnings) {
        lines += "[cloud] ${w.subject}: ${w.message}"
    }

    // LinkedHashMap: the order events arrived is the order they are read, and
    // a map iteration order that varied would make two identical windows
    // produce two different feeds.
    val byKindAndGroup = LinkedHashMap<Pair<String, String>, MutableList<CloudEvent>>()
    for (e in ordinary) {
        byKindAndGroup.getOrPut(e.kind to e.group) { mutableListOf() } += e
    }

    for ((key, group) in byKindAndGroup) {
        val (kind, groupName) = key
        if (group.size == 1) {
            lines += "[cloud] ${group.single().subject}: ${group.single().message}"
            continue
        }
        val shown = group.take(NAMES_SHOWN).joinToString(", ") { it.subject }
        val rest = group.size - minOf(group.size, NAMES_SHOWN)
        val names = if (rest > 0) "$shown and $rest more" else shown
        lines += "[cloud] ${group.size} $kind in $groupName ($names)"
    }
    return lines
}
```

- [ ] **Step 4: Run the tests**

Run: `cd agent && nix --extra-experimental-features 'nix-command flakes' develop -c gradle :common:test --tests '*CloudFeedTest*' --console=plain --offline`
Expected: PASS, eight tests.

- [ ] **Step 5: Mutate three claims**

1. Remove the `partition` and treat warnings like anything else.
   Expected: `a warning is never collapsed into a normal line` fails.
2. Group by `kind` alone, dropping `group` from the key.
   Expected: `the same kind in two groups stays two lines` fails.
3. Replace `take(NAMES_SHOWN)` with the whole list.
   Expected: `a very wide collapse names some and counts the rest` fails on
   the length assertion.

- [ ] **Step 6: Commit**

---

## Task 4: The feed reaches a player

Two things the shared module cannot do for itself: find who is online, and
know what the clock says. This task adds the one adapter for the first and the
buffer for the second, then four lines per platform.

**`FeedAudience` is separate from `SourceAdapter<S>` and that is measured, not
stylistic.** On Velocity `Player extends CommandSource`, so a
`holders(): List<S>` would work. On Paper it cannot:
`io.papermc.paper.command.brigadier.CommandSourceStack` is an interface with
accessors and no factory, and there is no way to build one from a `Player`. An
adapter that worked on one platform and not the other is exactly the asymmetry
this whole design refuses.

**Files:**
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/FeedAudience.kt`
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudFeedBuffer.kt`
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/Feed.kt`
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/FeedState.kt`
- Create: `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudFeedBufferTest.kt`
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/PaperAudience.kt`
- Create: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/VelocityAudience.kt`
- Modify: both `AgentPlugin.kt` files

**Interfaces:**
- Consumes: `coalesce(List<CloudEvent>): List<String>` from Task 3.
- Produces:
  - `interface FeedAudience { fun holders(permission: String): List<UUID>; fun send(player: UUID, message: String) }`
  - `class FeedState { fun optOut(player: UUID); fun optIn(player: UUID); fun wants(player: UUID): Boolean }`
    in `agent/common/.../FeedState.kt` — created here because `Feed` reads it;
    Task 5 is what gives a person a way to change it
  - `class CloudFeedBuffer(clock: () -> Long, windowMillis: Long, deliver: (List<String>) -> Unit)`
    with `fun add(event: CloudEvent)` and `fun tick()`.
  - `const val PERMISSION_EVENTS: String = "spawnery.cloud.events"` and
    `class Feed(audience, state, clock, windowMillis)` with `onEvent`, `tick`
    and `wanted()`, both in `agent/common/.../Feed.kt`

- [ ] **Step 1: Write `FeedAudience`**

```kotlin
package cloud.spawnery.agent

import java.util.UUID

/**
 * Where the feed goes: everybody online who may see it.
 *
 * **Separate from [SourceAdapter], and measured rather than chosen.** On
 * Velocity `Player` extends `CommandSource`, so the two could have been one
 * interface. On Paper they cannot: `CommandSourceStack` is an interface with
 * accessors and no factory, and nothing turns a `Player` from
 * `Bukkit.getOnlinePlayers()` into one. A `holders(): List<S>` on
 * [SourceAdapter] would compile on one platform and be unimplementable on the
 * other, which is the asymmetry this design refuses everywhere else.
 *
 * Two methods and a UUID between them rather than one that takes a lambda: the
 * caller has to filter by who opted out, and it needs an identity to do that.
 */
interface FeedAudience {
    /** The ids of everybody online holding [permission]. */
    fun holders(permission: String): List<UUID>

    /**
     * Sends one line to one player. A no-op for a player who has left, which
     * is ordinary: the list above is a moment old by the time it is used.
     */
    fun send(player: UUID, message: String)
}
```

- [ ] **Step 2: Write `FeedState`**

```kotlin
package cloud.spawnery.agent

import java.util.UUID
import java.util.concurrent.ConcurrentHashMap

/**
 * Who has turned the feed off, for as long as they stay connected.
 *
 * **Opt-out and not opt-in.** Somebody granted `spawnery.cloud.events` was
 * granted it to see them; making them ask again every session would mean the
 * permission does nothing on its own, which is not what granting a permission
 * looks like anywhere else on a server.
 *
 * It holds the *off* set rather than the *on* set for the same reason: an
 * empty state has to mean "everybody who may see this, sees this", and a set
 * of opted-in players would start empty and show nobody anything.
 *
 * Nothing removes a player who logs out. The set is bounded by how many
 * distinct administrators type the command in one agent's lifetime, which is
 * small, and a rejoin re-arms the feed anyway because [wants] is only ever
 * asked about players who are online.
 */
class FeedState {
    private val off = ConcurrentHashMap.newKeySet<UUID>()

    fun optOut(player: UUID) { off += player }
    fun optIn(player: UUID) { off -= player }
    fun wants(player: UUID): Boolean = player !in off
}
```

- [ ] **Step 3: Write the failing tests for the buffer**

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class CloudFeedBufferTest {
    private val delivered = mutableListOf<List<String>>()
    private var now = 0L
    private fun buffer() = CloudFeedBuffer({ now }, 1_000L) { delivered += it }

    @Test
    fun `nothing is delivered before the window closes`() {
        // The whole reason the buffer exists. Delivering on arrival is ten
        // lines for a rolling update, which is what section 5.4 refuses.
        val b = buffer()
        b.add(anEvent("lobby-a"))
        now = 999
        b.tick()

        assertTrue(delivered.isEmpty(), "delivered early: $delivered")
    }

    @Test
    fun `the window closing delivers one collapsed batch`() {
        val b = buffer()
        b.add(anEvent("lobby-a"))
        b.add(anEvent("lobby-b"))
        now = 1_000
        b.tick()

        assertEquals(1, delivered.size)
        assertEquals(1, delivered.single().size, "two events made ${delivered.single().size} lines")
    }

    @Test
    fun `an empty window delivers nothing at all`() {
        // Not an empty batch: a deliver call with no lines would have every
        // implementation of it writing a guard this one can write once.
        val b = buffer()
        now = 5_000
        b.tick()

        assertTrue(delivered.isEmpty())
    }

    @Test
    fun `the window restarts from the first event and not from the last tick`() {
        // Otherwise a steady trickle -- one event every 900ms -- would never
        // close a window and the feed would go silent exactly when something
        // is happening.
        val b = buffer()
        b.add(anEvent("lobby-a"))
        now = 900
        b.tick()
        b.add(anEvent("lobby-b"))
        now = 1_000
        b.tick()

        assertEquals(1, delivered.size, "a trickle never closed its window")
    }

    private fun anEvent(name: String): CloudEvent =
        CloudEvent.newBuilder()
            .setKind("ReadyGatePassed").setSubject(name).setGroup("lobby")
            .setMessage("$name is ready").build()
}
```

- [ ] **Step 4: Run and watch them fail**

Run: `cd agent && nix --extra-experimental-features 'nix-command flakes' develop -c gradle :common:test --tests '*CloudFeedBufferTest*' --console=plain --offline`
Expected: FAIL, `Unresolved reference 'CloudFeedBuffer'`.

- [ ] **Step 5: Write the buffer**

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/**
 * Holds a window of events and hands over the lines when it closes.
 *
 * The clock is a parameter and the tick is a call, so a test asserts the
 * boundary instead of racing it -- the same rule internal/boost.Live follows on
 * the operator's side, for the same reason.
 *
 * **The window starts at the first event, not at the last delivery.** A window
 * measured from the last tick would never close under a steady trickle, and the
 * feed would go quiet exactly when something is happening.
 *
 * Not thread-safe by itself. Both plugins call it from their own scheduler, and
 * events arrive on a gRPC callback thread, so the caller synchronises -- see
 * each AgentPlugin.
 */
class CloudFeedBuffer(
    private val clock: () -> Long,
    private val windowMillis: Long,
    private val deliver: (List<String>) -> Unit,
) {
    private val pending = mutableListOf<CloudEvent>()
    private var openedAt = 0L

    fun add(event: CloudEvent) {
        if (pending.isEmpty()) {
            openedAt = clock()
        }
        pending += event
    }

    fun tick() {
        if (pending.isEmpty()) return
        if (clock() - openedAt < windowMillis) return
        val lines = coalesce(pending.toList())
        pending.clear()
        if (lines.isNotEmpty()) {
            deliver(lines)
        }
    }
}
```

- [ ] **Step 6: Run the tests**

Expected: PASS, four tests.

- [ ] **Step 7: Mutate the window rule**

Change `openedAt = clock()` to run on every `add`.
Expected: `the window restarts from the first event and not from the last tick`
fails. Restore.

- [ ] **Step 8: Write the two platform audiences**

`agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/PaperAudience.kt`:

```kotlin
package cloud.spawnery.agent.paper

import cloud.spawnery.agent.FeedAudience
import net.kyori.adventure.text.Component
import org.bukkit.Bukkit
import java.util.UUID

/**
 * Paper's half of the feed's audience. See the Velocity counterpart: the two
 * files are the same four lines against two platforms.
 *
 * Adventure appears here and in that counterpart and nowhere else, for the
 * reason PaperSource gives.
 */
object PaperAudience : FeedAudience {
    override fun holders(permission: String): List<UUID> =
        Bukkit.getOnlinePlayers().filter { it.hasPermission(permission) }.map { it.uniqueId }

    override fun send(player: UUID, message: String) {
        // Null for a player who left between the list and this call, which is
        // ordinary rather than exceptional.
        Bukkit.getPlayer(player)?.sendMessage(Component.text(message))
    }
}
```

`agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/VelocityAudience.kt`:

```kotlin
package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.FeedAudience
import com.velocitypowered.api.proxy.ProxyServer
import net.kyori.adventure.text.Component
import java.util.UUID

class VelocityAudience(private val proxy: ProxyServer) : FeedAudience {
    override fun holders(permission: String): List<UUID> =
        proxy.allPlayers.filter { it.hasPermission(permission) }.map { it.uniqueId }

    override fun send(player: UUID, message: String) {
        proxy.getPlayer(player).ifPresent { it.sendMessage(Component.text(message)) }
    }
}
```

`PaperAudience` is an object and `VelocityAudience` a class because the
Velocity one needs the injected `ProxyServer` and Paper's static `Bukkit` needs
nothing. Do not make them match for symmetry's sake.

- [ ] **Step 9: Write the shared half of the wiring**

Everything below except the two lambdas is the same on both platforms, so it
goes in `agent/common` and not in either plugin. Create
`agent/common/src/main/kotlin/cloud/spawnery/agent/CloudFeed.kt`'s companion —
a new file `Feed.kt`:

```kotlin
package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/** The permission a source needs to see the feed. */
const val PERMISSION_EVENTS: String = "spawnery.cloud.events"

/**
 * The feed, from an arriving event to a line in somebody's chat.
 *
 * Written once for both platforms, because the only thing that differs is who
 * is online and how a line reaches them -- which is exactly [FeedAudience]'s
 * two methods and nothing else.
 *
 * **The lock separates two threads that both exist on both platforms**: events
 * arrive on a gRPC callback thread and [tick] runs on the platform's own
 * scheduler. Without it the buffer's list would be mutated during its own
 * iteration, which fails as a ConcurrentModificationException on a network
 * callback -- and a throw there costs the session, not just the line.
 */
class Feed(
    private val audience: FeedAudience,
    private val state: FeedState,
    clock: () -> Long,
    windowMillis: Long = WINDOW_MILLIS,
) {
    private val lock = Any()
    private val buffer = CloudFeedBuffer(clock, windowMillis) { lines -> deliver(lines) }

    /** Called from the network callback. */
    fun onEvent(event: CloudEvent) = synchronized(lock) { buffer.add(event) }

    /** Called from the platform's scheduler. */
    fun tick() = synchronized(lock) { buffer.tick() }

    /**
     * Whether anybody is here to read this.
     *
     * Recomputed rather than tracked, because a player joining, leaving,
     * gaining a permission or typing the command all change the answer, and
     * three of those four are events this agent would otherwise have to watch
     * for. Once a tick over the online list is cheaper than being wrong.
     */
    fun wanted(): Boolean = audience.holders(PERMISSION_EVENTS).any(state::wants)

    private fun deliver(lines: List<String>) {
        // The list is read once and reused for every line: a player who left
        // between two lines gets a no-op send, which FeedAudience documents.
        val recipients = audience.holders(PERMISSION_EVENTS).filter(state::wants)
        for (who in recipients) {
            for (line in lines) {
                audience.send(who, line)
            }
        }
    }

    companion object {
        /** Section 5.4's window. */
        const val WINDOW_MILLIS: Long = 1_000
    }
}
```

- [ ] **Step 10: Wire both plugins**

Each `AgentPlugin` builds one `Feed`, ticks it on the timer it already runs,
routes `CloudEvent` into `onEvent`, and reports interest.

Paper, inside the existing `server.scheduler.runTaskTimer` block that already
samples the player count:

```kotlin
feed.tick()
reportInterest(feed.wanted())
```

Velocity, in its own repeating task, identically.

`reportInterest` is one private method per plugin, and its whole job is not to
send the same answer twice:

```kotlin
// Sent only when the answer changes, plus once per stream: EventInterest is a
// state, and a state resent every second would be a report the operator has to
// read to learn nothing. `lastInterest` is null until the first send, so a new
// stream reports even when the answer has not moved -- the operator's answer
// for a session it has never seen is "no", and it does not remember one across
// a renewal.
private var lastInterest: Boolean? = null

private fun reportInterest(wanted: Boolean) {
    if (lastInterest == wanted) return
    lastInterest = wanted
    loop?.send(
        ServerMessage.newBuilder()
            .setEventInterest(EventInterest.newBuilder().setWanted(wanted))
            .build(),
    )
}
```

Reset `lastInterest` to null wherever the plugin already learns its stream
changed — the same place `CloudConnector.onStreamChanged` is called. Without
that reset a renewal would leave the operator believing "no" while this agent
believes it already said "yes", and the feed would go silent for exactly as
long as nobody's permissions changed.

- [ ] **Step 11: Run everything and commit**

Run: `cd agent && nix --extra-experimental-features 'nix-command flakes' develop -c gradle test --console=plain --offline`
Then: `nix --extra-experimental-features 'nix-command flakes' build .#agents`

---

## Task 5: `/cloud events on|off`

§5.5: the setting lives for the session on both platforms. Paper could persist
it in a `PersistentDataContainer` and Velocity has no equivalent, so symmetry
wins — and `/cloud events off` says so in its own output, because a setting
that quietly comes back after a rejoin is a setting people report as a bug.

**Files:**
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/SourceAdapter.kt`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudCommand.kt`
- Modify: `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudCommandTest.kt`
- Modify: both platform `SourceAdapter` implementations

**Interfaces:**
- Consumes: `PERMISSION_EVENTS` and
  `class FeedState { fun optOut(player: UUID); fun optIn(player: UUID); fun wants(player: UUID): Boolean }`,
  both from Task 4 — which already reads `wants` in `Feed.deliver`, so this
  task only gives somebody a way to move it.
- Produces: `SourceAdapter<S>.playerId(source: S): UUID?`, and a `/cloud
  events on|off` branch under `PERMISSION_EVENTS`.

- [ ] **Step 1: Grow `SourceAdapter` to three methods**

```kotlin
    /**
     * The id of the player behind this source, or null when there is not one.
     *
     * The third method, and the comment above about "exactly two" is now
     * wrong -- fix it rather than leaving it. It earns its place because
     * `/cloud events off` is a setting *for somebody*, and the tree has no
     * other way to ask who is typing.
     *
     * Null for the console, and that is a real answer rather than a gap: the
     * console is not a player, has no chat to feed, and already has every one
     * of these lines in its log.
     */
    fun playerId(source: S): UUID?
```

Paper: `(source.sender as? org.bukkit.entity.Player)?.uniqueId`.
Velocity: `(source as? com.velocitypowered.api.proxy.Player)?.uniqueId`.

- [ ] **Step 2: Write the failing command tests**

Add to `CloudCommandTest.kt`. The fixture's `permissions` set needs
`PERMISSION_EVENTS` added, and `run` needs a source that has a player id — the
existing `Int` source returns null from `playerId`, so add a second adapter or
give the fake one a settable id. Prefer the second: one adapter, one field.

```kotlin
    @Test
    fun `events off tells the player it lasts for this session only`() {
        // The sentence section 5.5 asks for. A setting that quietly comes back
        // after a rejoin is a setting people report as a bug.
        run("cloud events off")

        val line = sent.single()
        assertTrue(line.contains("off"), line)
        assertTrue(
            line.contains("rejoin") || line.contains("session"),
            "it did not say the setting is for this session: $line",
        )
    }

    @Test
    fun `events off then on leaves the player wanting them again`() {
        run("cloud events off")
        assertTrue(!feed.wants(sourcePlayer), "the player still wants events after off")

        run("cloud events on")
        assertTrue(feed.wants(sourcePlayer), "on did not undo off")
    }

    @Test
    fun `the console is told it cannot opt out rather than silently failing`() {
        // playerId is null for a console. Without this the command would
        // appear to work and change nothing, which is the worst of the three
        // possible behaviours.
        sourcePlayer = null

        run("cloud events off")

        assertTrue(sent.single().contains("console"), sent.toString())
    }

    @Test
    fun `events is invisible without its own permission`() {
        permissions = setOf(PERMISSION_READ)

        assertFailsWith<CommandSyntaxException> { run("cloud events off") }
    }
```

- [ ] **Step 3: Run and watch them fail**
- [ ] **Step 4: Add the branch**

`/cloud events on|off` under `PERMISSION_EVENTS`, added to the root's
`requires` chain alongside the other three. Two literal children rather than an
argument, so tab-completion offers `on` and `off` and a typo is an unknown
command rather than a silent no-op.

Output for `off`:

```
The cloud feed is off for you. It comes back when you rejoin -- this setting
lives for the session, on purpose: the proxy has nowhere to keep it, and one
platform remembering while the other forgets would be worse than neither.
```

- [ ] **Step 5: Run the tests, mutate two claims**

1. Make `off` a no-op that still prints the line.
   Expected: `events off then on leaves the player wanting them again` fails.
2. Have the console branch return silently instead of sending.
   Expected: `the console is told it cannot opt out` fails.

- [ ] **Step 6: Commit**

---

## Task 6: `EventBus`, and the harness sees a feed

The API half. §3.2 lists `EventBus events()`, and `agent/api/README.md`
currently says event subscription is "designed and not yet built" — that
sentence comes out in this task.

**Files:**
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/EventBus.java`
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/CloudEventInfo.java`
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java`
- Modify: `agent/api/src/test/java/cloud/spawnery/agent/api/FakeApi.java`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/MirrorApi.kt`
- Modify: `hack/agent-test.sh`

- [ ] **Step 1: The two API types**

Both carry the 16-line Apache header every file in `agent/api` has — copy it
from `ConnectResult.java`.

`CloudEventInfo` mirrors the proto with no protobuf type in sight, which is
what `PackagingInvariantTest` enforces:

```java
package cloud.spawnery.agent.api;

import java.util.Objects;

/**
 * One thing that happened in the cloud.
 *
 * <p>These are the facts, one per transition, and not the collapsed summary a
 * player sees in chat — the agent collapses for readability and you get what
 * it collapsed. The {@code message} is the operator's own sentence, the same
 * one {@code kubectl get events} shows for this event, because both are
 * derived from one call rather than computed twice.
 *
 * @param kind the operator's reason, in UpperCamelCase — {@code
 *     ReadyGatePassed}, {@code PodRejected}. A string and not an enum: the
 *     operator's vocabulary gains values, and an agent older than one must
 *     show it rather than fail. Match on the ones you know and pass the rest
 *     through.
 * @param group the group the subject belongs to, or the subject itself when
 *     the event is about a group. Never empty, so grouping needs no special
 *     case.
 * @param warning whether this is the ordinary case or the one somebody should
 *     look at.
 */
public record CloudEventInfo(
        String kind, String subject, String group, String message, boolean warning) {
    public CloudEventInfo {
        Objects.requireNonNull(kind, "kind");
        Objects.requireNonNull(subject, "subject");
        Objects.requireNonNull(group, "group");
        Objects.requireNonNull(message, "message");
    }
}
```

`EventBus` is one method:

```java
    /**
     * Calls {@code listener} for every cloud event this agent receives, until
     * the returned handle is closed.
     *
     * <p><b>A feed and not a ledger.</b> An agent that was disconnected missed
     * what happened while it was gone, and nothing replays it — the network
     * picture it re-syncs on reconnect is the correction, and a better one
     * than a replay would be: it says what is true now rather than what was
     * true in an order nobody was watching. A plugin that needs a ledger
     * should watch the objects.
     *
     * <p>Events arrive uncollapsed, one per transition. What a player sees in
     * chat is a collapsed summary of them; you get the facts.
     *
     * <p>The listener runs on a network callback thread. Do not block it, and
     * do not touch the world from it — hand the work to your platform's
     * scheduler.
     */
    AutoCloseable subscribe(Consumer<CloudEventInfo> listener);
```

Add `EventBus events();` to `SpawneryApi` with a short doc, and implement it in
`MirrorApi` over a listener list the connector already feeds.

- [ ] **Step 2: Update `FakeApi`**

It implements every method; a missing one is a compile error in the `api`
module's own tests, which is the point of that fake existing.

- [ ] **Step 3: A JUnit test that a delivered event reaches a subscriber**

In `:common`, against the real `MirrorApi` and a fake wire, in the pattern
`CloudCommandTest` sets: feed a `CloudEvent` in, assert the `CloudEventInfo`
that comes out carries the same five fields.

- [ ] **Step 4: Extend `hack/agent-test.sh`**

7c-2 added `require_cloud_list`, which types `cloud list` into the container's
console and waits for the answer. Add a second check in the same style, and
against the same container:

The stub gains a flag that makes it send one `CloudEvent` after the first
`NetworkState`. The harness then waits for the container's log to carry the
feed line for it. The console holds every permission on Paper, so nothing needs
granting — **but the console is not a player**, and `PaperAudience.holders`
returns online players only. So the console will *not* receive the feed.

That is the correct behaviour and it makes this check unassertable through the
console. **Do not contort the harness.** Assert instead the thing that is
reachable: that the agent reported `EventInterest{wanted:false}` with nobody
online, which the stub can record as an event and `await_event` already knows
how to wait for. Write down plainly, in the script and in the commit, that the
delivery to a player's chat is covered by JUnit and by nothing that runs the
real jar — and why: this harness has no Minecraft client.

- [ ] **Step 5: Run the full harness**

Run:
```
TMPDIR="$HOME/.cache/spawnery-tmp" nix --extra-experimental-features 'nix-command flakes' \
  develop -c make agent-test CONTAINER=podman
```
Expected: `agent-test: ok`. It takes about thirteen minutes — run it in the
background and read the log, not the exit code.

- [ ] **Step 6: Commit**

---

## Task 7: Documentation, and the spec's stale bullet

**Files:**
- Modify: `agent/api/README.md`, `charts/spawnery/README.md`, `docs/upgrading.md`
- Modify: `docs/superpowers/specs/2026-08-27-cloud-api-design.md`

- [ ] **Step 1: The fourth permission**

`charts/spawnery/README.md` has a three-row permission table from 7c-2, with a
"what a player would notice" column. Add `spawnery.cloud.events`: it changes
nothing and costs nothing, it is chat traffic for the holder alone, and it is
**on by default once granted** — `/cloud events off` is per session and comes
back on a rejoin.

- [ ] **Step 2: The upgrade note**

`docs/upgrading.md` gains a section saying the feed exists, is silent until
somebody is granted `spawnery.cloud.events`, and that an operator watching
`kubectl get events` sees exactly the same transitions — because they are the
same events, derived from one call.

- [ ] **Step 3: Correct §6.4 in the spec**

Its third bullet asks an E2E scenario to assert `/cloud start` moved
`minReplicas`. It does not and must not — §4.4 is the reason. Replace the
bullet with the assertion that actually holds (a `ScaleBoost` exists,
`status.boostedReplicas` rose, no server retired) and leave one sentence
saying the original was written before §4.4 existed. Do not silently delete it:
a spec that quietly loses a requirement is one nobody can review against.

- [ ] **Step 4: Commit**

---

## Done when

- [ ] `make test`, `make agent`, `make lint` all pass
- [ ] `make agent-test CONTAINER=podman` passes
- [ ] The Go suite is run as `go test -p 1 ./internal/... ./api/...` and every
      package is `ok`
- [ ] Every bound and every collapsing rule was mutated on its own, and the
      *named* test that failed was read each time
- [ ] `make manifests` leaves no diff
- [ ] `grep -rn 'ProxySelf\|ServerSelf\|CommandSourceStack\|CommandSource' agent/common/src/main | grep -vE ':\s*(\*|//)'`
      is empty — the naive grep matches comments, so read it before believing it
- [ ] Nothing was pushed and no tag was created

## What 7c-3 leaves

- **No `/cloud send`.** `connect` has been in the API since 7b-5 and the
  command for it is one more branch. Still deliberately not here: it is the
  branch that would be reviewed least.
- **No delivery guarantee, by design** (§8). Nothing retries an event, and
  nothing tells a plugin it missed one.
- **No persisted preference** (§5.5). A rejoin re-arms the feed.
- **No feed to the console.** The console is not a player and already has every
  one of these lines in its own log.
