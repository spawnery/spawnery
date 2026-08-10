# Proxy Channel Implementation Plan (milestone 3a)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The operator's proxy side — `ProxySession` answers instead of returning `Unimplemented`, a real registration fan-out reaches every connected proxy, and a `ProxyGroup` produces proxy pods behind a NodePort Service.

**Architecture:** A new package `internal/proxyreg` owns the set of live proxy sessions and implements `controller.Registrar`. It is the mirror of `internal/agent`: that package is the port the gRPC server writes into and the controllers read from; this one is the port the controllers write into and the gRPC server reads from. `agentserver.ProxySession` joins the fleet and pumps its outbox onto the stream; it never writes into the fleet. A new `ProxyGroupReconciler` manages proxy pods and their Service directly, with no per-proxy CR.

**Tech Stack:** Go 1.x, controller-runtime, gRPC (`google.golang.org/grpc`), envtest, Prometheus client. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-10-proxy-channel-design.md`. Read it before Task 1; every "why" below is short because the spec carries the long form.

## Global Constraints

These bind every task.

- **Everything in this repository is written in English** — code, comments, commit messages, documentation. No exceptions.
- **Every commit ends with the trailer** `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
- **Commit messages describe why, not what.** Match the existing history's register (`git log` on master): a full sentence as the subject, no `feat:`/`fix:` prefixes — the repository does not use them.
- **`make test` stays Go-only and must not get slower.** It is ~24 s today. Run it before every commit.
- **Timing is injected, never read from a global clock.** Anything that schedules work takes a clock or exposes the tick as a method a test can call, so tests drive time instead of sleeping. `Fleet.Resync` is exported for exactly this reason.
- **Identity never comes from a message.** It comes from the bearer token through `grpcauth`, always.
- **No new RBAC verb without an entry in `internal/rbacaudit.RequiredCluster`.** Adding a kubebuilder marker without the table entry turns the audit red on purpose.
- **Every new exported symbol carries a doc comment that says why, not what.** Match the density of the surrounding code — it is unusually high in this repository and that is deliberate.
- **Run `make manifests` after touching any kubebuilder marker** and commit the regenerated `config/rbac` output with the change.

## File Structure

**Created:**

| File | Responsibility |
|---|---|
| `internal/proxyreg/fleet.go` | The `Fleet`: live sessions, join ordering, broadcast, backpressure, resync |
| `internal/proxyreg/metrics.go` | The one counter the fan-out needs |
| `internal/proxyreg/fleet_test.go` | Plain-Go tests of all of the above |
| `internal/podspec/proxy.go` | `BuildProxyPod` and the proxy pod constants |
| `internal/podspec/proxy_test.go` | Table tests for it |
| `internal/controller/proxygroup_controller.go` | `ProxyGroupReconciler`: pods, Service, status |
| `internal/controller/proxygroup_controller_test.go` | envtest for it |
| `internal/agentserver/proxy_envtest_test.go` | A real gRPC `ProxySession` client against a real operator |

**Modified:**

| File | Change |
|---|---|
| `internal/grpcauth/identity.go` | `Identity.Group`; `PodChecker.PodExists` becomes `LookupPod` |
| `internal/grpcauth/interceptor_test.go` | The fake checker follows the new interface |
| `internal/grpcauth/auth_envtest_test.go` | Asserts the group reaches the identity |
| `internal/agentserver/server.go` | `ProxySession`, `handleProxy`, `Options.Proxies`, the shared receive pump |
| `internal/podspec/labels.go` | `ProxyLabels` |
| `internal/controller/bootstrap.go` | Both ServiceAccounts |
| `internal/controller/orphan.go` | The widened sweep |
| `internal/controller/server_controller.go` | `wasRegistered` before `Register` |
| `internal/controller/setup.go` | Wire the ProxyGroup controller and the fleet |
| `cmd/spawnery-operator/main.go` | Build the fleet, replace `NoopRegistrar` |
| `internal/rbacaudit/required.go` | The new verbs |
| `config/samples/network.yaml` | A ProxyGroup |
| `proto/spawnery/agent/v1alpha1/agent.proto` | The corrected `slots` comment |
| `docs/known-issues.md` | Strike what 3a closes |

---

### Task 1: A pod's group reaches the identity of its stream

A `DrainPlayers` message carries the fallback groups of the ProxyGroup the receiving session belongs to, so the session has to know its own group. `ClientPodChecker` already fetches the pod and already reads two of its labels; reading a third costs nothing, and the alternative is a second read of the same object inside `proxyreg`.

**Files:**
- Modify: `internal/grpcauth/identity.go`
- Modify: `internal/grpcauth/interceptor_test.go`
- Test: `internal/grpcauth/auth_envtest_test.go`

**Interfaces:**
- Produces: `grpcauth.Identity.Group string`; `grpcauth.PodChecker` with the single method `LookupPod(ctx context.Context, namespace, name, uid string, role agent.Role) (group string, ok bool, err error)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/grpcauth/auth_envtest_test.go`:

```go
// The group label of the pod has to survive the trip through the token, because
// a proxy session's DrainPlayers messages carry the fallback groups of exactly
// that ProxyGroup and nothing on the wire could tell the operator which one it
// is.
func TestIdentityCarriesTheGroupLabel(t *testing.T) {
	f := newAuthFixture(t)

	server := f.pod("lobby-group")
	id, err := f.auth.Authenticate(f.ctx, f.token(podspec.ServerServiceAccountName,
		[]string{podspec.AgentTokenAudience}, server), agent.RoleServer)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Group != "lobby" {
		t.Errorf("server Group = %q, want %q", id.Group, "lobby")
	}

	proxy := f.proxyPod("gateway-group")
	id, err = f.auth.Authenticate(f.ctx, f.token(podspec.ProxyServiceAccountName,
		[]string{podspec.AgentTokenAudience}, proxy), agent.RoleProxy)
	if err != nil {
		t.Fatalf("Authenticate proxy: %v", err)
	}
	if id.Group != "gateway" {
		t.Errorf("proxy Group = %q, want %q", id.Group, "gateway")
	}
	if id.Role != agent.RoleProxy {
		t.Errorf("Role = %q, want %q", id.Role, agent.RoleProxy)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/grpcauth/ -run TestIdentityCarriesTheGroupLabel -v`
Expected: FAIL — `id.Group undefined (type grpcauth.Identity has no field or method Group)`.

- [ ] **Step 3: Add the field and widen the checker**

In `internal/grpcauth/identity.go`, add to `Identity`:

```go
	// Group is the pod's spawnery.cloud/group label. A proxy session needs it
	// to know which fallback groups its DrainPlayers messages carry, and the
	// pod is fetched during authentication anyway — reading it here costs one
	// map lookup and saves proxyreg a second Get of the same object.
	Group string
```

Replace the `PodChecker` interface and `ClientPodChecker.PodExists` with:

```go
// PodChecker answers whether a pod the token names is one of ours, in the role
// the caller wants to act as, and which group it belongs to.
type PodChecker interface {
	LookupPod(ctx context.Context, namespace, name, uid string, role agent.Role) (group string, ok bool, err error)
}

// LookupPod implements PodChecker. It insists on the managed-by label and the
// role label matching the requested role, so a hand-built pod — or one
// labelled for the other role — cannot open a session. This mirrors the two
// labels OrphanReconciler.Sweep uses to decide what "one of ours" means; the
// two places must agree, or a pod could pass here yet be swept from the
// registry as foreign.
func (c *ClientPodChecker) LookupPod(ctx context.Context, namespace, name, uid string, role agent.Role) (string, bool, error) {
	pod := &corev1.Pod{}
	err := c.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pod)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if string(pod.UID) != uid {
		return "", false, nil
	}
	if pod.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		return "", false, nil
	}
	if pod.Labels[podspec.LabelRole] != roleLabelFor(role) {
		return "", false, nil
	}
	return pod.Labels[podspec.LabelGroup], true, nil
}
```

In `Authenticate`, replace the `PodExists` call and the return:

```go
	group, exists, err := a.Pods.LookupPod(ctx, namespace, podName, podUID, want)
	if err != nil {
		return Identity{}, wrapUnavailable(fmt.Errorf("look up pod %s/%s: %w", namespace, podName, err))
	}
	if !exists {
		return Identity{}, fmt.Errorf("pod %s/%s is not a Spawnery pod", namespace, podName)
	}

	return Identity{
		Namespace:      namespace,
		PodName:        podName,
		PodUID:         podUID,
		ServiceAccount: name,
		Role:           want,
		Group:          group,
	}, nil
```

- [ ] **Step 4: Fix the fake checker**

`refusingPodChecker` lives in `internal/grpcauth/auth_envtest_test.go:352` and is used from `interceptor_test.go` as well. Change its one method:

```go
type refusingPodChecker struct{}

func (refusingPodChecker) LookupPod(context.Context, string, string, string, agent.Role) (string, bool, error) {
	return "", false, nil
}
```

Keep the comment that is on it today, whatever it says about why it refuses — it explains the test, not the signature.

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/grpcauth/ -v`
Expected: PASS, including every existing rejection test. If a rejection now passes where it used to fail, the label check was lost — re-read Step 3.

- [ ] **Step 6: Commit**

```bash
git add internal/grpcauth
git commit -m "$(cat <<'EOF'
A proxy session cannot learn its own group from anything on the wire

DrainPlayers carries the fallback groups of one ProxyGroup, and which one is
a property of the pod rather than of the stream. The authenticator already
fetches that pod and already reads two of its labels to decide whether the
pod is ours at all, so the group comes out of the same Get. The alternative
was a second read of the same object one package further on.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: The fan-out, and the ordering invariant that makes FullSync first

The failure this task exists to prevent is a registration that never arrives and never announces its absence. The defence is structural: entering a session and building its first messages happen under the same mutex a broadcast has to take, so a `RegisterServer` cannot overtake the `FullSync` it belongs after.

**Files:**
- Create: `internal/proxyreg/fleet.go`
- Create: `internal/proxyreg/metrics.go`
- Test: `internal/proxyreg/fleet_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `proxyreg.New(opts Options) *Fleet`
  - `proxyreg.Options{Reader client.Reader; ResyncInterval time.Duration; OutboxSize int}`
  - `(*Fleet).Join(ctx context.Context, namespace, group, podUID string) (<-chan *agentpb.OperatorToProxy, func(), error)`
  - `(*Fleet).Register|Deregister|Drain(ctx context.Context, srv *spawneryv1alpha1.Server) error` — satisfies `controller.Registrar`
  - `proxyreg.DefaultResyncInterval`, `proxyreg.DefaultOutboxSize`

- [ ] **Step 1: Write the failing tests**

Create `internal/proxyreg/fleet_test.go`. The fake reader is controller-runtime's own fake client, which the repository already has in `go.sum` through controller-runtime.

```go
package proxyreg_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/proxyreg"
)

const (
	ns    = "minecraft"
	group = "gateway"
)

// registered builds a Server in the state the fan-out cares about: the flag
// applyDecision writes next to its Register call, plus an address to route to.
func registered(name, address string) *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
		},
		Status: spawneryv1alpha1.ServerStatus{
			Phase:      "Ready",
			Registered: true,
			Address:    address,
		},
	}
}

func proxyGroup(fallbacks ...string) *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: group, Namespace: ns},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Replicas:   1,
			Image:      "example/velocity:1",
			Expose:     spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort},
			Routing:    spawneryv1alpha1.RoutingSpec{FallbackGroups: fallbacks},
		},
	}
}

func newFleet(t *testing.T, objects ...client.Object) *proxyreg.Fleet {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	// WithStatusSubresource, or Status().Update on the fake client silently
	// writes nothing and the resync test would pass for the wrong reason.
	reader := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).
		WithObjects(objects...).Build()
	return proxyreg.New(proxyreg.Options{Reader: reader})
}

// recv takes one message or fails. Nothing here blocks in practice: every
// message this package produces is already in the outbox before the call that
// produced it returns.
func recv(t *testing.T, outbox <-chan *agentpb.OperatorToProxy) *agentpb.OperatorToProxy {
	t.Helper()
	select {
	case msg, ok := <-outbox:
		if !ok {
			t.Fatal("outbox closed")
		}
		return msg
	default:
		t.Fatal("outbox empty")
		return nil
	}
}

func TestFullSyncIsTheFirstMessage(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"), registered("lobby-aaaa", "10.0.0.1:25565"))

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	sync := recv(t, outbox).GetFullSync()
	if sync == nil {
		t.Fatal("first message is not a FullSync")
	}
	if len(sync.GetServers()) != 1 {
		t.Fatalf("FullSync carries %d servers, want 1", len(sync.GetServers()))
	}
	got := sync.GetServers()[0]
	if got.GetName() != "lobby-aaaa" || got.GetAddress() != "10.0.0.1:25565" || got.GetGroup() != "lobby" {
		t.Errorf("server = %+v, want name/address/group lobby-aaaa, 10.0.0.1:25565, lobby", got)
	}
}

// A server the operator never told the proxies about must not appear, and the
// flag is what records that — not the phase. The two disagree for exactly one
// reconcile after a deregistration, and in that window the flag is the one
// that matches what was sent.
func TestFullSyncOmitsUnregisteredAndAddresslessServers(t *testing.T) {
	unregistered := registered("lobby-bbbb", "10.0.0.2:25565")
	unregistered.Status.Registered = false
	addressless := registered("lobby-cccc", "")

	f := newFleet(t, proxyGroup("lobby"), unregistered, addressless)

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	if servers := recv(t, outbox).GetFullSync().GetServers(); len(servers) != 0 {
		t.Errorf("FullSync carries %d servers, want none", len(servers))
	}
}

// Section 5.2 of the main design: after every FullSync the draining servers are
// re-announced, or a proxy reconnecting mid-drain would undo the
// deregistration and start sending players back.
func TestJoinRepeatsTheDrainsAfterTheFullSync(t *testing.T) {
	draining := registered("lobby-dddd", "10.0.0.4:25565")
	draining.Status.Phase = "Draining"
	draining.Status.Registered = false

	f := newFleet(t, proxyGroup("lobby", "fallback"), draining)

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	if recv(t, outbox).GetFullSync() == nil {
		t.Fatal("first message is not a FullSync")
	}
	drain := recv(t, outbox).GetDrainPlayers()
	if drain == nil {
		t.Fatal("second message is not a DrainPlayers")
	}
	if drain.GetFromServer() != "lobby-dddd" {
		t.Errorf("fromServer = %q, want lobby-dddd", drain.GetFromServer())
	}
	if len(drain.GetToGroups()) != 2 || drain.GetToGroups()[0] != "lobby" {
		t.Errorf("toGroups = %v, want the ProxyGroup's fallback list", drain.GetToGroups())
	}
}

func TestRegisterReachesOnlyItsOwnNamespace(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	mine, leaveMine, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leaveMine()
	other, leaveOther, err := f.Join(context.Background(), "elsewhere", group, "uid-2")
	if err != nil {
		t.Fatalf("Join other: %v", err)
	}
	defer leaveOther()

	recv(t, mine)  // the FullSync
	recv(t, other) // the FullSync

	if err := f.Register(context.Background(), registered("lobby-eeee", "10.0.0.5:25565")); err != nil {
		t.Fatalf("Register: %v", err)
	}

	msg := recv(t, mine).GetRegisterServer()
	if msg == nil || msg.GetServer().GetName() != "lobby-eeee" {
		t.Fatalf("the session in the namespace got %+v", msg)
	}
	select {
	case msg := <-other:
		t.Fatalf("a session in another namespace received %+v", msg)
	default:
	}
}

func TestLeaveStopsDelivery(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	recv(t, outbox)
	leave()

	if err := f.Register(context.Background(), registered("lobby-ffff", "10.0.0.6:25565")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := <-outbox; ok {
		t.Error("a message reached a session that had left")
	}
}

// Make-before-break: the agent opens its next stream before the current one
// ends, so two Joins for one pod overlap. The displaced session's leave must
// not remove the fresh one — the same hazard sessions.leave guards against on
// the gRPC side, for the same reason.
func TestASupersededSessionDoesNotRemoveItsSuccessor(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))

	_, leaveFirst, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("first Join: %v", err)
	}
	second, leaveSecond, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("second Join: %v", err)
	}
	defer leaveSecond()

	leaveFirst()
	recv(t, second) // the FullSync

	if err := f.Register(context.Background(), registered("lobby-gggg", "10.0.0.7:25565")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if recv(t, second).GetRegisterServer() == nil {
		t.Error("the successor session stopped receiving when its predecessor left")
	}
}

func TestRegisterWithNoSessionsIsNotAnError(t *testing.T) {
	f := newFleet(t, proxyGroup("lobby"))
	if err := f.Register(context.Background(), registered("lobby-hhhh", "10.0.0.8:25565")); err != nil {
		t.Errorf("Register with no proxies connected: %v", err)
	}
}
```

Add the `runtime` import (`k8s.io/apimachinery/pkg/runtime`) that `newFleet` needs.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/proxyreg/ -v`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write the fleet**

Create `internal/proxyreg/fleet.go` with the Apache header every file in this repository carries (copy it from `internal/agent/registry.go`).

```go
// Package proxyreg is the port the controllers reach the proxies through. It
// owns every live proxy session and turns a registration decision into
// messages on them.
//
// It is the mirror of internal/agent: that package is what the gRPC server
// writes into and the controllers read from, this one is what the controllers
// write into and the gRPC server reads from. Neither lives inside
// internal/agentserver, so that neither direction has to know about TLS,
// tokens or streams.
package proxyreg

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/phase"
)

const (
	// DefaultResyncInterval is how often every live session is re-sent the
	// construction Join builds. See Resync for why it is not optional.
	DefaultResyncInterval = 30 * time.Second
	// DefaultOutboxSize is how far a session may fall behind before it is cut.
	DefaultOutboxSize = 64
)

// Options configures a Fleet.
type Options struct {
	// Reader lists Server objects and reads ProxyGroups. The manager's cached
	// client: FullSync is a read of the informer's indexer, not an API round
	// trip, which is what makes it cheap enough to build under the mutex.
	Reader client.Reader
	// ResyncInterval is how often Start re-syncs every session. Zero means
	// DefaultResyncInterval.
	ResyncInterval time.Duration
	// OutboxSize bounds a session's queue. Zero means DefaultOutboxSize.
	OutboxSize int
}

// session is one live proxy stream's queue.
type session struct {
	namespace string
	group     string
	outbox    chan *agentpb.OperatorToProxy
	// closed guards against a double close. A session is closed either because
	// its stream left or because it fell behind, and both can happen.
	closed bool
}

// Fleet is every live proxy session. Safe for concurrent use.
type Fleet struct {
	mu sync.Mutex
	// sessions is keyed by pod UID, the same key the agent registry uses.
	sessions map[string]*session
	opts     Options
}

// New creates a Fleet.
func New(opts Options) *Fleet {
	if opts.ResyncInterval <= 0 {
		opts.ResyncInterval = DefaultResyncInterval
	}
	if opts.OutboxSize <= 0 {
		opts.OutboxSize = DefaultOutboxSize
	}
	return &Fleet{sessions: make(map[string]*session), opts: opts}
}

// Join enters a session and returns its outbox together with the function that
// removes it. The first message on the channel is always the FullSync.
//
// That guarantee is the whole point of this function's shape. Everything
// between the lock and the unlock — reading the servers, building the messages,
// filling the queue, entering the session — happens where no broadcast can run,
// because every broadcast takes the same mutex. A RegisterServer therefore
// cannot overtake the FullSync it belongs after, and the ordering is a property
// of the code rather than of a test that has to win a race to notice.
//
// The Fleet closes the channel if the session falls too far behind. A caller
// that reads a closed channel must end its stream; see send.
func (f *Fleet) Join(ctx context.Context, namespace, group, podUID string) (<-chan *agentpb.OperatorToProxy, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	initial, err := f.snapshot(ctx, namespace, group)
	if err != nil {
		return nil, nil, err
	}

	// The queue is sized to hold the initial burst on top of the steady-state
	// budget, so a namespace that happens to be draining many servers at once
	// cannot cut a session off at the moment it joins.
	s := &session{
		namespace: namespace,
		group:     group,
		outbox:    make(chan *agentpb.OperatorToProxy, f.opts.OutboxSize+len(initial)),
	}
	for _, msg := range initial {
		s.outbox <- msg
	}
	f.sessions[podUID] = s

	return s.outbox, func() { f.leave(podUID, s) }, nil
}

// leave removes a session, if it is still the one registered for that pod.
//
// The guard matters for the same reason it does in agentserver's sessions:
// make-before-break means an agent opens its next stream before the current
// one ends, so a displaced session's leave runs after its successor has
// already entered. Without the identity check it would remove the live one.
func (f *Fleet) leave(podUID string, s *session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessions[podUID] != s {
		return
	}
	delete(f.sessions, podUID)
	f.close(s)
}

// close closes a session's outbox at most once. Callers hold f.mu.
func (f *Fleet) close(s *session) {
	if s.closed {
		return
	}
	s.closed = true
	close(s.outbox)
}

// send queues a message, or cuts the session loose if its queue is full.
//
// Dropping the message instead would leave the proxy routing on a list it has
// no way of knowing is wrong, looking healthy the whole time, until the next
// resync. Closing is loud: the agent's stream ends, it reconnects, and it is
// rebuilt from a fresh FullSync. A proxy that cannot accept a queue this deep
// is not serving players either.
//
// Callers hold f.mu.
func (f *Fleet) send(s *session, msg *agentpb.OperatorToProxy) {
	if s.closed {
		return
	}
	select {
	case s.outbox <- msg:
	default:
		SessionsCut.Inc()
		f.close(s)
	}
}

// snapshot builds what a session is sent on join and on every resync: the full
// registered list, followed by one DrainPlayers per draining server.
//
// Callers hold f.mu.
func (f *Fleet) snapshot(ctx context.Context, namespace, group string) ([]*agentpb.OperatorToProxy, error) {
	servers := &spawneryv1alpha1.ServerList{}
	if err := f.opts.Reader.List(ctx, servers, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list servers in %s: %w", namespace, err)
	}

	sync := &agentpb.FullSync{}
	var draining []*spawneryv1alpha1.Server
	for i := range servers.Items {
		srv := &servers.Items[i]
		// status.registered, not phase Ready. The flag is the record of what
		// this operator actually told the proxies — applyDecision writes it in
		// the same block that calls Register — while the phase is the record of
		// what the server is. They disagree for one reconcile after a
		// deregistration, and there the flag is the one that matches the wire.
		if srv.Status.Registered && srv.Status.Address != "" {
			sync.Servers = append(sync.Servers, registeredServer(srv))
		}
		if srv.Status.Phase == string(phase.Draining) {
			draining = append(draining, srv)
		}
	}
	// Sorted so two snapshots of the same state are the same bytes, which is
	// what lets an agent treat an unchanged resync as a no-op.
	sort.Slice(sync.Servers, func(i, j int) bool { return sync.Servers[i].GetName() < sync.Servers[j].GetName() })
	sort.Slice(draining, func(i, j int) bool { return draining[i].Name < draining[j].Name })

	out := []*agentpb.OperatorToProxy{{
		Message: &agentpb.OperatorToProxy_FullSync{FullSync: sync},
	}}
	fallbacks := f.fallbacks(ctx, namespace, group)
	for _, srv := range draining {
		out = append(out, drainMessage(srv, fallbacks))
	}
	return out, nil
}

// fallbacks reads one ProxyGroup's fallback list.
//
// Deliberately per group rather than a union across the namespace: two
// ProxyGroups of one network may route to different fallbacks, and a union
// would tell a proxy to move players onto a group it has no server for.
//
// A missing ProxyGroup is not an error. A proxy pod outlives its group by
// however long the orphan sweep takes, and an empty list is the honest answer
// for that window.
func (f *Fleet) fallbacks(ctx context.Context, namespace, group string) []string {
	pg := &spawneryv1alpha1.ProxyGroup{}
	key := types.NamespacedName{Name: group, Namespace: namespace}
	if err := f.opts.Reader.Get(ctx, key, pg); err != nil {
		return nil
	}
	return pg.Spec.Routing.FallbackGroups
}

func registeredServer(srv *spawneryv1alpha1.Server) *agentpb.RegisteredServer {
	return &agentpb.RegisteredServer{
		Name:    srv.Name,
		Address: srv.Status.Address,
		Group:   srv.Spec.GroupRef.Name,
	}
}

func drainMessage(srv *spawneryv1alpha1.Server, fallbacks []string) *agentpb.OperatorToProxy {
	return &agentpb.OperatorToProxy{
		Message: &agentpb.OperatorToProxy_DrainPlayers{
			DrainPlayers: &agentpb.DrainPlayers{
				FromServer: srv.Name,
				ToGroups:   fallbacks,
			},
		},
	}
}

// broadcast delivers one message to every session in a namespace. build takes
// the session because DrainPlayers differs per ProxyGroup.
func (f *Fleet) broadcast(namespace string, build func(*session) *agentpb.OperatorToProxy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.namespace != namespace {
			continue
		}
		f.send(s, build(s))
	}
}

// Register implements controller.Registrar.
//
// It returns nil when no proxy is connected. That is not a degraded state: a
// Network with no ProxyGroup is legitimate, and it is the state every server
// ran in through milestone 2.
func (f *Fleet) Register(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	f.broadcast(srv.Namespace, func(*session) *agentpb.OperatorToProxy {
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_RegisterServer{
				RegisterServer: &agentpb.RegisterServer{Server: registeredServer(srv)},
			},
		}
	})
	return nil
}

// Deregister implements controller.Registrar.
func (f *Fleet) Deregister(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	f.broadcast(srv.Namespace, func(*session) *agentpb.OperatorToProxy {
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_UnregisterServer{
				UnregisterServer: &agentpb.UnregisterServer{Name: srv.Name},
			},
		}
	})
	return nil
}

// Drain implements controller.Registrar.
func (f *Fleet) Drain(ctx context.Context, srv *spawneryv1alpha1.Server) error {
	f.broadcast(srv.Namespace, func(s *session) *agentpb.OperatorToProxy {
		return drainMessage(srv, f.fallbacks(ctx, s.namespace, s.group))
	})
	return nil
}

// Sessions is how many proxy sessions are live. For tests and for the metric.
func (f *Fleet) Sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}
```

Leave `sigs.k8s.io/controller-runtime/pkg/log` out of the import block for now — Task 3 is the first code here that logs.

Create `internal/proxyreg/metrics.go`:

```go
package proxyreg

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// SessionsCut counts proxy sessions ended because their queue filled up. It is
// the only outward sign of a proxy that cannot keep up: the stream simply ends
// and the agent reconnects, which on its own looks like an ordinary reconnect.
var SessionsCut = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "spawnery_proxy_sessions_cut_total",
		Help: "Proxy sessions ended because the session fell too far behind.",
	},
)

func init() { metrics.Registry.MustRegister(SessionsCut) }
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/proxyreg/ -v`
Expected: PASS, all eight tests.

- [ ] **Step 5: Run the whole suite**

Run: `make test`
Expected: PASS, and no slower than before.

- [ ] **Step 6: Commit**

```bash
git add internal/proxyreg
git commit -m "$(cat <<'EOF'
The order a proxy learns things in is a property of the lock, not of a test

A registration that overtakes the FullSync it belongs after is lost for good
and says nothing about it. Rather than testing for the race, Join holds the
same mutex every broadcast has to take across reading the servers, building
the messages and entering the session, so the interleaving cannot occur.

A full queue cuts the session instead of dropping the message. A dropped
message leaves a proxy routing on a list it cannot know is wrong while it
looks healthy; a cut stream is something the agent already knows how to
recover from.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: The resync, and what it is for

The ordering invariant of Task 2 closes the window between a broadcast and a join. It does not close the one between the Server controller's status write and the cache `snapshot` reads from. That window is what this task covers, and the comment explaining it is as much a deliverable as the code — without it, the next reader removes the ticker as redundant.

**Files:**
- Modify: `internal/proxyreg/fleet.go`
- Test: `internal/proxyreg/fleet_test.go`

**Interfaces:**
- Produces: `(*Fleet).Resync(ctx context.Context)`, `(*Fleet).Start(ctx context.Context) error`, `(*Fleet).NeedLeaderElection() bool`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/proxyreg/fleet_test.go`:

```go
// The scenario the resync exists for, played out exactly: a registration is
// broadcast while the reader still shows the old world, the session's FullSync
// is therefore built without it, and nothing else would ever correct that.
func TestResyncHealsARegistrationTheCacheHadNotSeen(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).
		WithObjects(proxyGroup("lobby")).Build()
	f := proxyreg.New(proxyreg.Options{Reader: reader})

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()
	if servers := recv(t, outbox).GetFullSync().GetServers(); len(servers) != 0 {
		t.Fatalf("FullSync carries %d servers, want none", len(servers))
	}

	// The world moves on without the session having been told.
	late := registered("lobby-iiii", "10.0.0.9:25565")
	if err := reader.Create(context.Background(), late); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := reader.Status().Update(context.Background(), late); err != nil {
		t.Fatalf("status update: %v", err)
	}

	f.Resync(context.Background())

	sync := recv(t, outbox).GetFullSync()
	if sync == nil {
		t.Fatal("the resync did not send a FullSync")
	}
	if len(sync.GetServers()) != 1 || sync.GetServers()[0].GetName() != "lobby-iiii" {
		t.Errorf("resynced FullSync = %+v, want the late registration", sync.GetServers())
	}
}

// A proxy that cannot keep up must not be left holding a partial list while
// looking healthy.
func TestAFullOutboxCutsTheSession(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).
		WithObjects(proxyGroup("lobby")).Build()
	f := proxyreg.New(proxyreg.Options{Reader: reader, OutboxSize: 2})

	outbox, leave, err := f.Join(context.Background(), ns, group, "uid-1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	defer leave()

	// Nothing reads the outbox. The FullSync used its own slot, so two more
	// fit and the third has nowhere to go.
	for i := 0; i < 3; i++ {
		if err := f.Register(context.Background(), registered("lobby-jjjj", "10.0.0.10:25565")); err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
	}

	// Drain what was queued; the channel must then be closed rather than empty.
	for range outbox {
	}

	if f.Sessions() != 1 {
		t.Errorf("Sessions() = %d — a cut session stays in the map until its stream leaves", f.Sessions())
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/proxyreg/ -run 'TestResync|TestAFullOutbox' -v`
Expected: FAIL — `f.Resync undefined`. `TestAFullOutboxCutsTheSession` may hang instead of failing if `send` does not close; that hang is the failure.

- [ ] **Step 3: Add Resync and Start**

Add to `internal/proxyreg/fleet.go` (and add `"sigs.k8s.io/controller-runtime/pkg/log"` to the imports, removing the placeholder line from Task 2):

```go
// Resync re-sends every live session the same construction Join builds.
//
// This is not in section 5.2 of the main design, and it is not redundant with
// the ordering invariant in Join. That invariant closes the window between a
// broadcast and a session entering. It cannot close the one between the Server
// controller writing a status and the cache snapshot reads from catching up:
//
//	Deregister for X is broadcast and queued on a session
//	the same session rejoins
//	its FullSync is built from a cache that still shows X as registered
//	the proxy ends up with X in its list after being told to drop it
//
// Nothing else would ever correct that. The window is short and this closes it
// within one interval. Do not remove it because "the list is derived from the
// CRs anyway" — that is exactly the reasoning that leaves it broken.
//
// It is exported rather than only ticked, so tests drive it instead of
// sleeping.
func (f *Fleet) Resync(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for podUID, s := range f.sessions {
		messages, err := f.snapshot(ctx, s.namespace, s.group)
		if err != nil {
			// One unreadable namespace must not stop the others. The next tick
			// tries again, and the session keeps its last known list until then
			// — which is the correct answer while the operator cannot read.
			log.FromContext(ctx).V(1).Info("skipped a proxy resync",
				"pod", podUID, "namespace", s.namespace, "reason", err.Error())
			continue
		}
		for _, msg := range messages {
			f.send(s, msg)
		}
	}
}

// Start runs the resync ticker until ctx ends. It implements manager.Runnable.
func (f *Fleet) Start(ctx context.Context) error {
	ticker := time.NewTicker(f.opts.ResyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			f.Resync(ctx)
		}
	}
}

// NeedLeaderElection makes this leader-bound, for the same reason the agent
// endpoint is: only the leader holds the streams these messages go to.
func (f *Fleet) NeedLeaderElection() bool { return true }
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/proxyreg/ -v`
Expected: PASS, all ten tests.

- [ ] **Step 5: Commit**

```bash
git add internal/proxyreg
git commit -m "$(cat <<'EOF'
The lock cannot order what the cache has not seen yet

Join guarantees a FullSync precedes every broadcast that follows it. It says
nothing about a broadcast that already happened against a status the cache
has not caught up with — a deregistration queued on a session whose next
FullSync is built from a reader that still shows the server as registered.
The proxy is then left holding a server it was told to drop, and no later
event corrects it.

The ticker is thirty seconds and the comment above it is the deliverable:
without the losing sequence written out, this reads like a redundant timer
over a list that is derived from the CRs anyway.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: ProxySession answers, and is measured on the client's side of the wire

Milestone 2c produced five defects in a row, and not one was in the code the tests were checking — every one was in an assumption about which side of the wire the test measured. Every assertion here is on what a real gRPC client receives.

**Files:**
- Modify: `internal/agentserver/server.go`
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Test: `internal/agentserver/proxy_envtest_test.go` (create)

**Interfaces:**
- Consumes: `grpcauth.Identity.Group` (Task 1); `proxyreg.Fleet.Join` (Task 2).
- Produces: `agentserver.Options.Proxies *proxyreg.Fleet`.

- [ ] **Step 1: Correct the proto comment**

In `proto/spawnery/agent/v1alpha1/agent.proto`, replace the `PlayerCount` comment:

```protobuf
// PlayerCount is the periodic report.
//
// A proxy reports its configured player limit as slots. The obvious
// alternative — leaving slots at zero, as an earlier draft of this file said —
// collides with the registry, which discards any report where players exceed
// slots: a proxy with one player online would have every report thrown away,
// visible only as a counter, while its connected player count sat at zero.
// One rule in the registry is worth more than a role-dependent one, and a
// proxy does have a capacity: ProxyGroup.spec.config.playerLimit.
message PlayerCount {
  int32 players = 1;
  int32 slots = 2;
}
```

Run `make proto` and commit the regenerated Go and Java together with it — the comment reaches both.

- [ ] **Step 2: Write the failing test**

Create `internal/agentserver/proxy_envtest_test.go`. Read `internal/agentserver/server_envtest_test.go` first and reuse its fixture wholesale — do not build a second one. You need a `dialProxy` next to `dialAgent`, differing only in the RPC it opens, and a proxy pod and proxy token from the same helpers.

```go
package agentserver_test

// dialProxy opens a ProxySession the way a real Velocity agent would.
// It is dialAgent with one line changed; the two are kept apart rather than
// generified because the stream types differ and the duplication is four
// lines against an abstraction nobody would read.
func dialProxy(t *testing.T, ctx context.Context, addr string, ca []byte, token string) (
	grpc.BidiStreamingClient[agentpb.ProxyMessage, agentpb.OperatorToProxy], func()) {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("CA bundle unusable")
	}
	creds := credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: "spawnery-operator.spawnery-system.svc",
		MinVersion: tls.VersionTLS13,
	})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	stream, err := agentpb.NewAgentServiceClient(conn).ProxySession(streamCtx)
	if err != nil {
		conn.Close()
		t.Fatalf("open ProxySession: %v", err)
	}
	return stream, func() { _ = conn.Close() }
}

// A proxy agent connects and is told everything it needs before it is asked
// for anything. Every assertion is on what the client received.
func TestAProxyReceivesItsIntervalDeadlineAndFullSync(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-aaaa")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if first.GetReportInterval() == nil {
		t.Fatalf("first message = %+v, want a ReportInterval", first)
	}
	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if second.GetSessionDeadline() == nil {
		t.Fatalf("second message = %+v, want a SessionDeadline", second)
	}
	third, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if third.GetFullSync() == nil {
		t.Fatalf("third message = %+v, want a FullSync", third)
	}
}

// A registration made after the session is up reaches it.
func TestARegistrationReachesAConnectedProxy(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-bbbb")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()

	for i := 0; i < 3; i++ {
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
	}

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-aaaa", Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
		},
		Status: spawneryv1alpha1.ServerStatus{Address: "10.0.0.1:25565"},
	}
	if err := f.proxies.Register(f.ctx, srv); err != nil {
		t.Fatalf("Register: %v", err)
	}

	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if got := msg.GetRegisterServer().GetServer().GetName(); got != "lobby-aaaa" {
		t.Errorf("received %+v, want a RegisterServer for lobby-aaaa", msg)
	}
}

// The rule the proto comment now states. A proxy reporting a real player count
// against its own limit must not be discarded.
func TestAProxyPlayerCountAgainstItsLimitIsAccepted(t *testing.T) {
	f := newServerFixture(t)
	pod := f.proxyPod("gateway-cccc")
	stream, done := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	for i := 0; i < 3; i++ {
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("Recv %d: %v", i, err)
		}
	}

	if err := stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_PlayerCount{
			PlayerCount: &agentpb.PlayerCount{Players: 7, Slots: 500},
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The registry is the operator's side, so poll it rather than assert once.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if snap := f.agents.Lookup(string(pod.UID)); snap.Players == 7 && snap.Slots == 500 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the report never landed: %+v", f.agents.Lookup(string(pod.UID)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
```

The fixture needs two additions: a `proxies *proxyreg.Fleet` field wired into `agentserver.Options`, and the `proxyPod` helper (copy it from `internal/grpcauth/auth_envtest_test.go`, which already has one). Build the fleet with `proxyreg.New(proxyreg.Options{Reader: f.c})`.

- [ ] **Step 3: Run it and watch it fail**

Run: `go test ./internal/agentserver/ -run TestAProxy -v`
Expected: FAIL — `rpc error: code = Unimplemented desc = proxy sessions arrive with milestone 3`.

- [ ] **Step 4: Extract the receive pump**

Both sessions need the same "receiving blocks, so it runs in its own goroutine" construction. It is the fiddliest part of either handler and writing it twice is how the second copy drifts. In `internal/agentserver/server.go`:

```go
// recvPump moves a stream's receive side onto channels, because Recv blocks
// and the handler has three other things to select on. It is generic over the
// message type: the two sessions differ in nothing else here, and this is the
// part that is easy to get subtly wrong twice.
//
// The goroutine ends when Recv fails or when ctx is done, so it cannot outlive
// its stream.
func recvPump[T any](ctx context.Context, recv func() (T, error)) (<-chan T, <-chan error) {
	received := make(chan T)
	errs := make(chan error, 1)
	go func() {
		defer close(received)
		for {
			msg, err := recv()
			if err != nil {
				errs <- err
				return
			}
			select {
			case received <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	return received, errs
}
```

Replace the inline goroutine in `ServerSession` with `received, errs := recvPump(ctx, stream.Recv)`.

- [ ] **Step 5: Run the existing server-session tests**

Run: `go test ./internal/agentserver/ -run TestA -v` and then the whole package.
Expected: every pre-existing test still passes. If any ordering test changed behaviour, the pump is not equivalent — compare it against `git show HEAD:internal/agentserver/server.go`.

- [ ] **Step 6: Add the option and implement ProxySession**

In `Options`:

```go
	// Proxies is the fan-out every proxy session joins. Required for
	// ProxySession; a nil one is a programming error, not a runtime state.
	Proxies *proxyreg.Fleet
```

Replace the `ProxySession` stub:

```go
// ProxySession is the Velocity agent's channel. It reads from the fan-out and
// never writes into it: everything a proxy is told is decided by a controller,
// so a compromised proxy cannot make the operator say anything.
func (s *Server) ProxySession(stream agentpb.AgentService_ProxySessionServer) error {
	id, ok := grpcauth.IdentityFrom(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "no identity on the stream")
	}
	logger := log.FromContext(stream.Context()).WithValues("pod", id.PodName, "namespace", id.Namespace)
	openedAt := s.opts.Clock()

	ctx, gen, superseded := s.sessions.enter(stream.Context(), id.PodUID)
	defer func() {
		if s.sessions.leave(id.PodUID, gen) {
			s.opts.Agents.Disconnect(id.PodUID)
		}
		logger.V(1).Info("session ended", "after", s.opts.Clock().Sub(openedAt))
	}()

	if superseded {
		s.opts.Agents.Supersede(id.PodUID, agent.RoleProxy)
	} else {
		s.opts.Agents.Connect(id.PodUID, agent.RoleProxy)
	}
	OpenStreams.WithLabelValues(string(agent.RoleProxy)).Inc()
	defer OpenStreams.WithLabelValues(string(agent.RoleProxy)).Dec()

	if err := stream.Send(&agentpb.OperatorToProxy{
		Message: &agentpb.OperatorToProxy_ReportInterval{
			ReportInterval: &agentpb.ReportInterval{Seconds: seconds(s.opts.ReportInterval)},
		},
	}); err != nil {
		return err
	}
	if err := stream.Send(&agentpb.OperatorToProxy{
		Message: &agentpb.OperatorToProxy_SessionDeadline{
			SessionDeadline: &agentpb.SessionDeadline{
				RenewAfterSeconds:   seconds(s.opts.RenewAfter),
				HardDeadlineSeconds: seconds(s.opts.HardDeadline),
			},
		},
	}); err != nil {
		return err
	}

	// Joining after the two fixed messages, not before: the FullSync is the
	// first thing whose content depends on the cluster, and the agent needs the
	// deadline in hand before it starts processing a server list.
	outbox, leave, err := s.opts.Proxies.Join(ctx, id.Namespace, id.Group, id.PodUID)
	if err != nil {
		return status.Errorf(codes.Unavailable, "join the proxy fleet: %v", err)
	}
	defer leave()

	deadline := time.AfterFunc(s.opts.HardDeadline, func() {
		logger.V(1).Info("closing the stream at its hard deadline")
		s.sessions.cancel(id.PodUID, gen)
	})
	defer deadline.Stop()

	received, errs := recvPump(ctx, stream.Recv)

	for {
		select {
		case <-ctx.Done():
			return status.Error(codes.Unavailable, "session ended, reconnect with a fresh token")
		case err := <-errs:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case msg := <-received:
			s.handleProxy(logger, id, msg)
		case msg, ok := <-outbox:
			if !ok {
				// The fan-out cut us loose. Ending the stream is the point:
				// a partial server list is worse than a reconnect.
				return status.Error(codes.ResourceExhausted, "proxy fell behind, reconnect for a fresh sync")
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// handleProxy applies one message from a proxy. An unknown branch is ignored so
// a newer agent against an older operator keeps working.
func (s *Server) handleProxy(logger logr.Logger, id grpcauth.Identity, msg *agentpb.ProxyMessage) {
	switch m := msg.GetMessage().(type) {
	case *agentpb.ProxyMessage_Hello:
		// A proxy's readiness is not carried here. The agent serves the pod's
		// readiness probe itself (design 6.6), so the kubelet has already
		// written the answer where the ProxyGroup controller reads it.
		logger.V(1).Info("proxy connected", "version", m.Hello.GetVersion())
	case *agentpb.ProxyMessage_PlayerCount:
		if err := s.opts.Agents.ReportPlayers(id.PodUID,
			m.PlayerCount.GetPlayers(), m.PlayerCount.GetSlots()); err != nil {
			RejectedReports.WithLabelValues(string(agent.RoleProxy)).Inc()
			logger.V(1).Info("discarded a player count", "reason", err.Error())
		}
	case *agentpb.ProxyMessage_Heartbeat:
		// Nothing. The stream is its own liveness signal and the registry's
		// staleness rule already derives from ReportInterval. A second liveness
		// path would be a second truth about the same fact.
	case *agentpb.ProxyMessage_PlayerJoinedServer:
		// Accepted and ignored. Nothing in milestones 3 or 4 consumes it —
		// player counts come from the servers — and it is on the wire for the
		// dashboard in project 4.
		logger.V(1).Info("player joined a server",
			"player", m.PlayerJoinedServer.GetPlayer(), "server", m.PlayerJoinedServer.GetServer())
	}
}
```

- [ ] **Step 7: Run the tests**

Run: `go test ./internal/agentserver/ -v`
Expected: PASS. Note that `server_envtest_test.go:575` asserts a server token on a proxy session yields `Unauthenticated` **or** `Unimplemented`; it stays green, but tighten it to `Unauthenticated` alone now that `Unimplemented` is no longer a possible answer.

- [ ] **Step 8: Run the whole suite and commit**

Run: `make test`

```bash
git add internal/agentserver proto internal/agentpb agent/paper/src/proto
git commit -m "$(cat <<'EOF'
A proxy reporting a real player count would have had every report discarded

The proto told proxy agents to leave slots at zero while the registry rejects
any report where players exceed slots. One player online and every report
goes to a counter nobody watches, with the connected count sitting at zero.
Proxies report their configured limit instead, so the registry keeps one rule
rather than a role-dependent one.

ProxySession is otherwise the server session with different message types,
including the receive pump both now share. Sharing it is not tidiness: it is
the part that is easy to get subtly wrong, and two copies drift.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: The proxy pod

**Files:**
- Create: `internal/podspec/proxy.go`
- Modify: `internal/podspec/labels.go`
- Test: `internal/podspec/proxy_test.go` (create)

**Interfaces:**
- Produces: `podspec.ProxyLabels(network, group string) map[string]string`; `podspec.BuildProxyPod(net *spawneryv1alpha1.Network, group *spawneryv1alpha1.ProxyGroup, name, agentEndpoint string) (*corev1.Pod, error)`; `podspec.ProxyContainerName`, `podspec.ProxyReadyPort`, `podspec.ProxyReadyPortName`, `podspec.DefaultPlayerLimit`, `podspec.EnvPlayerLimit`.

- [ ] **Step 1: Write the failing tests**

Create `internal/podspec/proxy_test.go`. The package is `podspec` (internal tests — `server_test.go` is in the same package), and `testNetwork()` from `server_test.go` is reused as is.

Note before writing: `ProxyGroupSpec` has **no** `Mounts` field, so there is no user-mount path on a proxy pod and no collision test to write. Do not add the field — user mounts on proxies are not in this milestone's scope.

```go
package podspec

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func testProxyGroup() *spawneryv1alpha1.ProxyGroup {
	return &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "minecraft", UID: "pg-uid"},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "production"},
			Replicas:   2,
			Image:      "ghcr.io/spawnery/velocity:3.4.0-0.2.0",
			Expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			Routing: spawneryv1alpha1.RoutingSpec{FallbackGroups: []string{"lobby"}},
		},
	}
}

func buildProxy(t *testing.T) *corev1.Pod {
	t.Helper()
	pod, err := BuildProxyPod(testNetwork(), testProxyGroup(), "gateway-abcd", testEndpoint)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	return pod
}

func proxyEnv(pod *corev1.Pod, name string) string {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// The probe is the ready gate of design 6.6. It has to be a tcpSocket on a
// port of its own: Velocity speaks no HTTP, and an exec probe would need
// another binary in the image and another thing to keep reproducible. The
// agent binds this port only after it has processed its first FullSync, so a
// proxy cannot go green without a server list.
func TestProxyPodReadinessProbeIsTheAgentsOwnPort(t *testing.T) {
	pod := buildProxy(t)
	probe := pod.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.TCPSocket == nil {
		t.Fatalf("readiness probe = %+v, want a tcpSocket", probe)
	}
	if probe.TCPSocket.Port.IntVal != podspec.ProxyReadyPort {
		t.Errorf("probe port = %v, want %d", probe.TCPSocket.Port, podspec.ProxyReadyPort)
	}
	if pod.Spec.Containers[0].LivenessProbe != nil {
		t.Error("a liveness probe would restart the container and kick every player on it")
	}
}

// The player limit is not cosmetic: the agent reports it as slots, and the
// registry discards any report above it. A group that sets none must still
// produce a workable number.
func TestProxyPodCarriesAPlayerLimit(t *testing.T) {
	pod := buildProxy(t)
	if got := proxyEnv(pod, EnvPlayerLimit); got != "500" {
		t.Errorf("%s = %q, want the default %d", EnvPlayerLimit, got, DefaultPlayerLimit)
	}

	group := testProxyGroup()
	group.Spec.Config = &spawneryv1alpha1.ProxyConfigSpec{PlayerLimit: 120}
	configured, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	if got := proxyEnv(configured, EnvPlayerLimit); got != "120" {
		t.Errorf("%s = %q, want the group's 120", EnvPlayerLimit, got)
	}
}

func TestProxyPodHasNoServerLabel(t *testing.T) {
	pod := buildProxy(t)
	if _, ok := pod.Labels[LabelServer]; ok {
		// The orphan sweep keys on its absence to tell the two kinds of pod
		// apart once it lists by managed-by alone.
		t.Error("a proxy pod must carry no server label")
	}
	if pod.Labels[LabelRole] != RoleProxy {
		t.Errorf("role label = %q, want %q", pod.Labels[LabelRole], RoleProxy)
	}
	if pod.Labels[LabelGroup] != "gateway" {
		t.Errorf("group label = %q, want gateway", pod.Labels[LabelGroup])
	}
}

// The pod has no CR of its own, so the group is what deletion cascades from.
func TestProxyPodIsOwnedByItsGroup(t *testing.T) {
	pod := buildProxy(t)
	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("owner references = %+v, want exactly the ProxyGroup", pod.OwnerReferences)
	}
	owner := pod.OwnerReferences[0]
	if owner.Kind != "ProxyGroup" || owner.Name != "gateway" || owner.UID != "pg-uid" {
		t.Errorf("owner = %+v, want the ProxyGroup gateway", owner)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Error("the ProxyGroup must be the controlling owner")
	}
}

// The network's defaults reach the proxy layer too — a nodeSelector that keeps
// game servers off the control plane has to keep proxies off it as well.
func TestProxyPodInheritsTheNetworkDefaults(t *testing.T) {
	pod := buildProxy(t)
	if pod.Spec.NodeSelector["node-role/minecraft"] != "true" {
		t.Errorf("NodeSelector = %v, want the network default", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "registry-credentials" {
		t.Errorf("ImagePullSecrets = %v, want the network default", pod.Spec.ImagePullSecrets)
	}
	if pod.Spec.Containers[0].Resources.Requests.Cpu().String() != "1" {
		t.Errorf("cpu request = %v, want the network default", pod.Spec.Containers[0].Resources.Requests.Cpu())
	}
}

func TestProxyPodRefusesAGroupWithoutAnImage(t *testing.T) {
	group := testProxyGroup()
	group.Spec.Image = ""
	if _, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint); err == nil {
		t.Fatal("a ProxyGroup with no image was accepted")
	}
}

func TestProxyPodMountsTheAgentCredentialsAndNothingElse(t *testing.T) {
	pod := buildProxy(t)
	if pod.Spec.ServiceAccountName != ProxyServiceAccountName {
		t.Errorf("ServiceAccountName = %q, want %q", pod.Spec.ServiceAccountName, ProxyServiceAccountName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("AutomountServiceAccountToken must be false: the pod carries no Kubernetes credentials")
	}

	var projected *corev1.Volume
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		// No PVC: proxies hold no state worth keeping, and the cross-proxy
		// player state that would need one is deferred by the main design.
		if v.PersistentVolumeClaim != nil {
			t.Errorf("volume %q is a PVC", v.Name)
		}
		if v.Name == AgentVolumeName {
			projected = v
		}
	}
	if projected == nil || projected.Projected == nil {
		t.Fatalf("no projected agent volume among %+v", pod.Spec.Volumes)
	}
	if len(projected.Projected.Sources) != 2 {
		t.Fatalf("projected sources = %+v, want the token and the CA", projected.Projected.Sources)
	}
	token := projected.Projected.Sources[0].ServiceAccountToken
	if token == nil || token.Audience != AgentTokenAudience {
		t.Errorf("token projection = %+v, want audience %q", token, AgentTokenAudience)
	}
}

func TestProxyPodExposesBothPorts(t *testing.T) {
	pod := buildProxy(t)
	ports := map[string]int32{}
	for _, p := range pod.Spec.Containers[0].Ports {
		ports[p.Name] = p.ContainerPort
	}
	if ports[MinecraftPortName] != MinecraftPort {
		t.Errorf("minecraft port = %d, want %d", ports[MinecraftPortName], MinecraftPort)
	}
	if ports[ProxyReadyPortName] != ProxyReadyPort {
		t.Errorf("ready port = %d, want %d", ports[ProxyReadyPortName], ProxyReadyPort)
	}
}

// The drain window is how long existing sessions may run out when a proxy is
// replaced. A grace period shorter than it would have the kubelet kill the
// process mid-drain.
func TestProxyPodGracePeriodComesFromTheDrainWindow(t *testing.T) {
	group := testProxyGroup()
	group.Spec.Drain = &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 120}
	pod, err := BuildProxyPod(testNetwork(), group, "gateway-abcd", testEndpoint)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	if pod.Spec.TerminationGracePeriodSeconds == nil || *pod.Spec.TerminationGracePeriodSeconds != 120 {
		t.Errorf("grace period = %v, want 120", pod.Spec.TerminationGracePeriodSeconds)
	}

	unset := buildProxy(t) // testProxyGroup sets no drain block
	if unset.Spec.TerminationGracePeriodSeconds == nil ||
		*unset.Spec.TerminationGracePeriodSeconds != int64(DefaultDrainTimeoutSeconds) {
		t.Errorf("grace period without a drain block = %v, want the default %d",
			unset.Spec.TerminationGracePeriodSeconds, DefaultDrainTimeoutSeconds)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/podspec/ -run TestProxyPod -v`
Expected: FAIL — `undefined: podspec.BuildProxyPod`.

- [ ] **Step 3: Add ProxyLabels**

In `internal/podspec/labels.go`:

```go
// ProxyLabels are the labels of a Velocity pod. There is deliberately no
// LabelServer: a proxy has no Server object, and the orphan sweep tells the
// two kinds of managed pod apart by that absence.
func ProxyLabels(network, group string) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNetwork:   network,
		LabelGroup:     group,
		LabelRole:      RoleProxy,
	}
}
```

- [ ] **Step 4: Write BuildProxyPod**

Create `internal/podspec/proxy.go` with the Apache header (copy it from `internal/podspec/server.go`).

```go
package podspec

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	// ProxyContainerName is the name of the Velocity container.
	ProxyContainerName = "velocity"

	// ProxyReadyPort is the ready gate of design 6.6. The Velocity agent binds
	// it only once it has processed its first FullSync, so a proxy cannot turn
	// green before it has a server list — a plain tcpSocket check on 25565
	// would, and a proxy that gets traffic without a list disconnects every
	// player with "no available server".
	ProxyReadyPort int32 = 8081
	// ProxyReadyPortName names that port.
	ProxyReadyPortName = "ready"

	// EnvPlayerLimit names the container env var carrying the proxy's player
	// limit. The agent reports it as slots, and the registry discards any
	// report above it, so it is load-bearing rather than cosmetic.
	EnvPlayerLimit = "SPAWNERY_PLAYER_LIMIT"
	// EnvProxy names the container env var carrying the pod's own name.
	EnvProxy = "SPAWNERY_PROXY"

	// DefaultPlayerLimit is what a ProxyGroup that sets none gets. Zero would
	// be worse than a guess: the registry rejects every report where players
	// exceed slots, so a limit of zero would silently discard every count.
	DefaultPlayerLimit int32 = 500

	// DefaultDrainTimeoutSeconds mirrors the CRD default on
	// ProxyGroup.spec.drain. It is repeated here because a ProxyGroup built in
	// a unit test never passes through the API server's defaulting, and a nil
	// drain block must not produce a grace period of zero — that would kill a
	// proxy's sessions the instant it was replaced.
	DefaultDrainTimeoutSeconds int32 = 300
)

// BuildProxyPod renders one pod of a ProxyGroup. The group owns the pod, so
// deleting the group cascades — there is no per-proxy CR to hang it from, and
// none is wanted: proxies are fungible and have no state machine.
func BuildProxyPod(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	name string,
	agentEndpoint string,
) (*corev1.Pod, error) {
	if group.Spec.Image == "" {
		return nil, fmt.Errorf("proxy group %q has no image", group.Name)
	}
	if agentEndpoint == "" {
		return nil, fmt.Errorf("proxy group %q has no agent endpoint", group.Name)
	}

	resources := group.Spec.Resources
	if resources == nil && net.Spec.Defaults != nil {
		resources = net.Spec.Defaults.Resources
	}

	// A group's scheduling replaces the network default wholesale, exactly as
	// it does for server groups: merging the two would make it impossible to
	// drop an inherited nodeSelector.
	scheduling := group.Spec.Scheduling
	if scheduling == nil && net.Spec.Defaults != nil {
		scheduling = net.Spec.Defaults.Scheduling
	}

	var pullSecrets []corev1.LocalObjectReference
	if net.Spec.Defaults != nil {
		pullSecrets = net.Spec.Defaults.ImagePullSecrets
	}

	playerLimit := DefaultPlayerLimit
	if group.Spec.Config != nil && group.Spec.Config.PlayerLimit > 0 {
		playerLimit = group.Spec.Config.PlayerLimit
	}

	grace := DefaultDrainTimeoutSeconds
	if group.Spec.Drain != nil {
		grace = group.Spec.Drain.TimeoutSeconds
	}

	volumes := []corev1.Volume{
		{
			Name:         DataVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name:         TmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: AgentVolumeName,
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					Sources: []corev1.VolumeProjection{
						{
							// The audience is what makes a standard API server
							// token worthless here, and the short expiry keeps
							// the replay window small. The kubelet rotates it.
							ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
								Audience:          AgentTokenAudience,
								ExpirationSeconds: ptr.To(TokenExpirationSeconds),
								Path:              AgentTokenPath,
							},
						},
						{
							ConfigMap: &corev1.ConfigMapProjection{
								LocalObjectReference: corev1.LocalObjectReference{Name: CAConfigMapName},
								Items: []corev1.KeyToPath{
									{Key: CAConfigMapKey, Path: AgentCAPath},
								},
							},
						},
					},
				},
			},
		},
	}

	container := corev1.Container{
		Name:  ProxyContainerName,
		Image: group.Spec.Image,
		Ports: []corev1.ContainerPort{
			{
				Name:          MinecraftPortName,
				ContainerPort: MinecraftPort,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          ProxyReadyPortName,
				ContainerPort: ProxyReadyPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		Env: []corev1.EnvVar{
			{Name: "SPAWNERY_NETWORK", Value: net.Name},
			{Name: "SPAWNERY_GROUP", Value: group.Name},
			{Name: EnvProxy, Value: name},
			{Name: EnvPlayerLimit, Value: strconv.FormatInt(int64(playerLimit), 10)},
			{Name: EnvOperatorEndpoint, Value: agentEndpoint},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: DataVolumeName, MountPath: DataMountPath},
			{Name: TmpVolumeName, MountPath: TmpMountPath},
			{Name: AgentVolumeName, MountPath: AgentMountPath, ReadOnly: true},
		},
		// Readiness only, for the same reason the server pod has no liveness
		// probe: a restart would disconnect every player on this proxy, and
		// the client connection terminates here.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt32(ProxyReadyPort),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       5,
			TimeoutSeconds:      3,
			FailureThreshold:    3,
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	if resources != nil {
		container.Resources = *resources
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: group.Namespace,
			Labels:    ProxyLabels(net.Name, group.Name),
			Annotations: map[string]string{
				AnnotationSafeToEvict: "false",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         spawneryv1alpha1.GroupVersion.String(),
				Kind:               "ProxyGroup",
				Name:               group.Name,
				UID:                group.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{container},
			Volumes:                       volumes,
			RestartPolicy:                 corev1.RestartPolicyAlways,
			ServiceAccountName:            ProxyServiceAccountName,
			AutomountServiceAccountToken:  ptr.To(false),
			ImagePullSecrets:              pullSecrets,
			TerminationGracePeriodSeconds: ptr.To(int64(grace)),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
		},
	}

	if scheduling != nil {
		pod.Spec.NodeSelector = scheduling.NodeSelector
		pod.Spec.Tolerations = scheduling.Tolerations
		pod.Spec.Affinity = scheduling.Affinity
	}

	return pod, nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/podspec/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/podspec
git commit -m "$(cat <<'EOF'
A proxy with no player limit would discard every count it ever reported

The agent reports the limit as slots and the registry throws away any report
above it, so an unset limit is not a cosmetic default — it is a number that
silences the pod. Five hundred is Velocity's own, and it is written down here
rather than left to whatever the image happens to render.

The readiness port is a contract this milestone only writes: 8081, bound by
the agent after its first FullSync. A tcpSocket check on 25565 would go green
while the proxy still has no servers, and a proxy with no servers disconnects
everyone who reaches it.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: The namespace bootstrap creates both ServiceAccounts

**Files:**
- Modify: `internal/controller/bootstrap.go`
- Test: `internal/controller/bootstrap_test.go`

**Interfaces:**
- Produces: no signature change. `Bootstrapper.Ensure` now also creates `podspec.ProxyServiceAccountName`.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/bootstrap_test.go`:

```go
// Without this a proxy pod would have no identity to present at all: the token
// projection names a ServiceAccount, and the kubelet cannot mint a token for
// one that does not exist — the pod fails before it reaches the first TLS
// handshake, with an error about a volume rather than about credentials.
func TestEnsureCreatesBothServiceAccounts(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		sa := &corev1.ServiceAccount{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sa); err != nil {
			t.Errorf("get ServiceAccount %s: %v", name, err)
			continue
		}
		if sa.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
			t.Errorf("ServiceAccount %s is unlabelled", name)
		}
	}
}

// The no-write guarantee has to hold for both. It is what lets the operator
// keep get;list;watch;create on serviceaccounts and no update verb at all.
func TestEnsureLeavesExistingServiceAccountsAlone(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	b := &Bootstrapper{Client: c, Reader: c, CA: func() []byte { return []byte("PEM-A") }}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}

	before := map[string]string{}
	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		sa := &corev1.ServiceAccount{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sa); err != nil {
			t.Fatalf("get ServiceAccount %s: %v", name, err)
		}
		before[name] = sa.ResourceVersion
	}

	if err := b.Ensure(ctx, ns); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}

	for name, was := range before {
		sa := &corev1.ServiceAccount{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sa); err != nil {
			t.Fatalf("get ServiceAccount %s: %v", name, err)
		}
		if sa.ResourceVersion != was {
			t.Errorf("ServiceAccount %s was written on the second Ensure — the update verb is not granted", name)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/controller/ -run TestEnsureCreatesBothServiceAccounts -v`
Expected: FAIL — `serviceaccounts "spawnery-proxy" not found`.

- [ ] **Step 3: Loop over both names**

In `internal/controller/bootstrap.go`, rename `ensureServiceAccount` to `ensureServiceAccounts` and wrap its body in a loop over `[]string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName}`. Keep every property the existing comment defends — Get-then-Create by construction, no `update` verb, `AlreadyExists` as success — and extend the comment with one sentence:

```go
// Both ServiceAccounts are created in every namespace, including one that will
// only ever run servers. An unused ServiceAccount costs nothing; teaching
// Ensure a role parameter would put the decision at two call sites that both
// have to get it right, and the Server controller calling it does not know
// whether a ProxyGroup will appear tomorrow.
```

- [ ] **Step 4: Run the package**

Run: `go test ./internal/controller/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/bootstrap.go internal/controller/bootstrap_test.go
git commit -m "$(cat <<'EOF'
A proxy pod with no ServiceAccount has no identity to present at all

The token projection names one and the kubelet cannot mint a token for a
ServiceAccount that does not exist, so the pod fails before it reaches the
first TLS handshake. Both are created in every namespace, including one that
will only ever run servers: an unused ServiceAccount costs nothing, and the
Server controller calling Ensure cannot know whether a ProxyGroup appears
tomorrow.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: The ProxyGroup controller

**Files:**
- Create: `internal/controller/proxygroup_controller.go`
- Test: `internal/controller/proxygroup_controller_test.go` (create)
- Modify: `config/samples/network.yaml`

**Interfaces:**
- Consumes: `podspec.BuildProxyPod`, `podspec.ProxyLabels` (Task 5); `Bootstrapper.Ensure` (Task 6).
- Produces: `controller.ProxyGroupReconciler` with fields `client.Client`, `Scheme *runtime.Scheme`, `Recorder record.EventRecorder`, `Agents *agent.Registry`, `Clock func() time.Time`, `Bootstrap *Bootstrapper`, `AgentEndpoint string`; `controller.NewProxyName(group string) string`; `(*ProxyGroupReconciler).SetupWithManager(mgr ctrl.Manager) error`.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/proxygroup_controller_test.go`. It reuses `newFixture` from `suite_test.go`, which already creates and accepts a `Network` in an isolated namespace. envtest runs no kubelet, so pod readiness and `hostIP` are written onto `pod.Status` by the test — exactly as `f.setPodRunning` already does for server pods.

```go
package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

// createProxyGroup adds a ProxyGroup to the fixture's network. Task 8's sweep
// tests use it too — both files are in package controller.
func (f *fixture) createProxyGroup(name string, mutate ...func(*spawneryv1alpha1.ProxyGroup)) *spawneryv1alpha1.ProxyGroup {
	f.t.Helper()
	group := &spawneryv1alpha1.ProxyGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ProxyGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: f.network.Name},
			Replicas:   2,
			Image:      "ghcr.io/spawnery/velocity:3.4.0-0.2.0",
			Expose: spawneryv1alpha1.ExposeSpec{
				Type:     spawneryv1alpha1.ExposeNodePort,
				NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30001},
			},
			Routing: spawneryv1alpha1.RoutingSpec{FallbackGroups: []string{"lobby"}},
		},
	}
	for _, m := range mutate {
		m(group)
	}
	if err := f.c.Create(f.ctx, group); err != nil {
		f.t.Fatalf("create ProxyGroup: %v", err)
	}
	return group
}

func proxyGroupReconciler(f *fixture) *ProxyGroupReconciler {
	return &ProxyGroupReconciler{
		Client:   f.c,
		Scheme:   testenv.Scheme(f.t),
		Recorder: record.NewFakeRecorder(100),
		Agents:   f.agents,
		Clock:    f.clock.Now,
		Bootstrap: &Bootstrapper{
			Client: f.c, Reader: f.c,
			CA: func() []byte { return []byte("test-ca") },
		},
		AgentEndpoint: "spawnery-operator.spawnery-system.svc:9443",
	}
}

func (f *fixture) reconcileProxyGroup(r *ProxyGroupReconciler, name string) {
	f.t.Helper()
	if _, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: f.ns},
	}); err != nil {
		f.t.Fatalf("reconcile ProxyGroup %s: %v", name, err)
	}
}

func (f *fixture) proxyPods(group string) []corev1.Pod {
	f.t.Helper()
	pods := &corev1.PodList{}
	if err := f.c.List(f.ctx, pods, client.InNamespace(f.ns), client.MatchingLabels{
		podspec.LabelRole:  podspec.RoleProxy,
		podspec.LabelGroup: group,
	}); err != nil {
		f.t.Fatalf("list proxy pods: %v", err)
	}
	live := make([]corev1.Pod, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.DeletionTimestamp.IsZero() {
			live = append(live, p)
		}
	}
	return live
}

func (f *fixture) proxyGroup(name string) *spawneryv1alpha1.ProxyGroup {
	f.t.Helper()
	group := &spawneryv1alpha1.ProxyGroup{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, group); err != nil {
		f.t.Fatalf("get ProxyGroup %s: %v", name, err)
	}
	return group
}

func TestProxyGroupCreatesItsPodsAndService(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) != 2 {
		t.Fatalf("proxy pods = %d, want the group's 2 replicas", len(pods))
	}

	svc := &corev1.Service{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway", Namespace: f.ns}, svc); err != nil {
		t.Fatalf("get Service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("Service type = %q, want NodePort", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("Service ports = %+v, want exactly the Minecraft port", svc.Spec.Ports)
	}
	if svc.Spec.Ports[0].Port != podspec.MinecraftPort || svc.Spec.Ports[0].NodePort != 30001 {
		t.Errorf("Service port = %+v, want 25565 on node port 30001", svc.Spec.Ports[0])
	}
	// A selector that does not match the pods produces a Service with no
	// endpoints — reachable, silent, and wrong.
	for k, v := range svc.Spec.Selector {
		if pods[0].Labels[k] != v {
			t.Errorf("Service selector %s=%q does not match the pods", k, v)
		}
	}
	if svc.Spec.Selector[podspec.LabelRole] != podspec.RoleProxy {
		t.Error("the Service selector must pin the proxy role, or it would also select server pods")
	}
}

// Milestone 6 owns the other two strategies. Until then the refusal has to be
// visible on the object rather than buried in a log line — a ProxyGroup that
// silently does nothing is indistinguishable from an operator that is down.
func TestProxyGroupRefusesLoadBalancer(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Expose = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeLoadBalancer}
	})

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	if !hasCondition(group.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonExposeNotImplemented) {
		t.Errorf("conditions = %+v, want Accepted=False/%s",
			group.Status.Conditions, spawneryv1alpha1.ReasonExposeNotImplemented)
	}
	if n := len(f.proxyPods("gateway")); n != 0 {
		t.Errorf("proxy pods = %d, want none for a strategy that is not implemented", n)
	}
}

// With NodePort the address a player needs is a node's, and the operator has
// no right to read Node objects. hostIP on a running proxy pod is the address
// of a node that demonstrably has a proxy on it.
func TestProxyGroupAddressComesFromAReadyPodsHostIP(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")

	pods := f.proxyPods("gateway")
	if len(pods) == 0 {
		t.Fatal("no proxy pods to mark ready")
	}
	pod := &pods[0]
	pod.Status.Phase = corev1.PodRunning
	pod.Status.HostIP = "192.168.1.10"
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(f.clock.Now()),
	}}
	if err := f.c.Status().Update(f.ctx, pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")

	group := f.proxyGroup("gateway")
	if group.Status.Address != "192.168.1.10:30001" {
		t.Errorf("status.address = %q, want 192.168.1.10:30001", group.Status.Address)
	}
	if group.Status.ReadyReplicas != 1 {
		t.Errorf("status.readyReplicas = %d, want 1", group.Status.ReadyReplicas)
	}
}

// Empty is the truthful answer while nothing is ready: there is nowhere to
// connect yet, and a node address for a proxy that is not serving would send
// players at a closed port.
func TestProxyGroupAddressIsEmptyWithNoReadyPod(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	if got := f.proxyGroup("gateway").Status.Address; got != "" {
		t.Errorf("status.address = %q, want empty while no proxy is ready", got)
	}
}

func TestProxyGroupScalesDown(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	group := f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Replicas = 3
	})
	f.reconcileProxyGroup(r, "gateway")
	if n := len(f.proxyPods("gateway")); n != 3 {
		t.Fatalf("proxy pods = %d, want 3", n)
	}

	group = f.proxyGroup("gateway")
	group.Spec.Replicas = 1
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("scale down: %v", err)
	}
	f.reconcileProxyGroup(r, "gateway")

	if n := len(f.proxyPods("gateway")); n != 1 {
		t.Errorf("proxy pods = %d, want 1 after scaling down", n)
	}
}

// The bootstrap has to run before the first pod, or the pod would mount a
// ConfigMap that does not exist and never start.
func TestProxyGroupBootstrapsTheNamespace(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")

	f.reconcileProxyGroup(r, "gateway")

	sa := &corev1.ServiceAccount{}
	key := types.NamespacedName{Name: podspec.ProxyServiceAccountName, Namespace: f.ns}
	if err := f.c.Get(f.ctx, key, sa); err != nil {
		t.Fatalf("the proxy ServiceAccount was not bootstrapped: %v", err)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/controller/ -run TestProxyGroup -v`
Expected: FAIL — `undefined: ProxyGroupReconciler`.

- [ ] **Step 3: Write the reconciler**

First add the reason to `api/v1alpha1/common_types.go`, next to the other reasons:

```go
	ReasonExposeNotImplemented = "ExposeStrategyNotImplemented"
```

Then create `internal/controller/proxygroup_controller.go` with the Apache header. Read `servergroup_controller.go` alongside this — the network resolution, the condition helpers and the status write all follow its shape, and where this file says "as the ServerGroup controller does" it means copy that structure rather than invent a second one.

```go
package controller

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

// NewProxyName builds a unique proxy pod name below the group prefix. Same
// generator and same alphabet as NewServerName: a proxy has no CR of its own,
// so the pod name is the only handle anyone has on it, and it has to be as
// readable off a terminal as a server's.
func NewProxyName(group string) string { return NewServerName(group) }

// ProxyGroupReconciler keeps a proxy group at its replica count, keeps its
// Service in step, and publishes where players connect.
//
// Unlike ServerGroupReconciler it manages pods directly. Proxies are fungible:
// there is no per-proxy object, no state machine, and nothing to drain on the
// operator's side — a proxy's own agent moves its players, and that is
// milestone 4.
type ProxyGroupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Agents is the runtime state reported by the in-game agents. Read for the
	// connected player count.
	Agents *agent.Registry
	// Clock is injectable so the time rules are testable.
	Clock func() time.Time
	// Bootstrap puts the CA bundle and the ServiceAccounts into the namespace
	// before the first pod is created there.
	Bootstrap *Bootstrapper
	// AgentEndpoint is the address the in-game agent dials.
	AgentEndpoint string
}

// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups,verbs=get;list;watch
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups/status,verbs=update
// +kubebuilder:rbac:groups=spawnery.cloud,resources=proxygroups/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update

// Reconcile brings one ProxyGroup in line with its spec.
func (r *ProxyGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	group := &spawneryv1alpha1.ProxyGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !group.DeletionTimestamp.IsZero() {
		// The pods and the Service are owned by this object, so the API server
		// removes them. There is nothing to drain here: moving players is the
		// proxy's own job and belongs to milestone 4.
		return ctrl.Result{}, nil
	}

	network := &spawneryv1alpha1.Network{}
	key := types.NamespacedName{Name: group.Spec.NetworkRef.Name, Namespace: group.Namespace}
	switch err := r.Get(ctx, key, network); {
	case apierrors.IsNotFound(err):
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonNetworkNotFound,
			fmt.Sprintf("Network %q does not exist", group.Spec.NetworkRef.Name))
		return ctrl.Result{RequeueAfter: networkRetryInterval}, r.writeStatus(ctx, group)
	case err != nil:
		return ctrl.Result{}, err
	}
	if !meta.IsStatusConditionTrue(network.Status.Conditions, spawneryv1alpha1.ConditionAccepted) {
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonNetworkNotAccepted,
			networkNotAcceptedMessage(network))
		return ctrl.Result{RequeueAfter: networkRetryInterval}, r.writeStatus(ctx, group)
	}

	// Milestone 6 owns the other two strategies. Refusing is the honest
	// version: a LoadBalancer branch written now would reach milestone 6 having
	// never run, because the local flow cannot produce a cluster that would
	// exercise it. A refusal on the object is also the only form a user can
	// see — a group that silently does nothing looks like a dead operator.
	if group.Spec.Expose.Type != spawneryv1alpha1.ExposeNodePort {
		setProxyGroupAccepted(group, false, spawneryv1alpha1.ReasonExposeNotImplemented,
			fmt.Sprintf("expose.type %s arrives with milestone 6; only NodePort is implemented",
				group.Spec.Expose.Type))
		return ctrl.Result{}, r.writeStatus(ctx, group)
	}
	setProxyGroupAccepted(group, true, spawneryv1alpha1.ReasonAccepted, "")

	if err := r.Bootstrap.Ensure(ctx, group.Namespace); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileService(ctx, group); err != nil {
		return ctrl.Result{}, err
	}

	pods, err := r.pods(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileReplicas(ctx, network, group, pods); err != nil {
		return ctrl.Result{}, err
	}

	// Re-read after the changes, so the status describes what is there rather
	// than what was there when the reconcile started.
	pods, err = r.pods(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.setStatus(group, pods)
	return ctrl.Result{RequeueAfter: resyncInterval}, r.writeStatus(ctx, group)
}

// pods lists the group's live proxy pods, oldest first, so scale-down is
// deterministic rather than map-order.
func (r *ProxyGroupReconciler) pods(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(group.Namespace), client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelRole:      podspec.RoleProxy,
		podspec.LabelGroup:     group.Name,
	}); err != nil {
		return nil, err
	}
	live := make([]corev1.Pod, 0, len(list.Items))
	for _, pod := range list.Items {
		if pod.DeletionTimestamp.IsZero() {
			live = append(live, pod)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].CreationTimestamp.Equal(&live[j].CreationTimestamp) {
			return live[i].Name < live[j].Name
		}
		return live[i].CreationTimestamp.Before(&live[j].CreationTimestamp)
	})
	return live, nil
}

// reconcileReplicas creates or removes pods until the count matches the spec.
// Scale-down takes the newest first: an older proxy has had longer to collect
// players, and this milestone has no way to move them.
func (r *ProxyGroupReconciler) reconcileReplicas(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
) error {
	for i := int32(len(pods)); i < group.Spec.Replicas; i++ {
		pod, err := podspec.BuildProxyPod(network, group, NewProxyName(group.Name), r.AgentEndpoint)
		if err != nil {
			return err
		}
		if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	for i := len(pods) - 1; i >= int(group.Spec.Replicas); i-- {
		if err := r.Delete(ctx, &pods[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// reconcileService keeps the NodePort Service in step with the group.
func (r *ProxyGroupReconciler) reconcileService(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: group.Name, Namespace: group.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue
		svc.Spec.Type = corev1.ServiceTypeNodePort
		// The selector must pin the role as well as the group: without it the
		// Service would also select any server pod that happened to share the
		// group name, and players would land on a backend directly.
		svc.Spec.Selector = podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       podspec.MinecraftPortName,
			Port:       podspec.MinecraftPort,
			TargetPort: intstr.FromString(podspec.MinecraftPortName),
			NodePort:   group.Spec.Expose.NodePort.Port,
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(group, svc, r.Scheme)
	})
	return err
}

// setStatus writes what is observably true of the group's pods.
func (r *ProxyGroupReconciler) setStatus(group *spawneryv1alpha1.ProxyGroup, pods []corev1.Pod) {
	var ready int32
	var players int32
	for i := range pods {
		if !isPodReady(&pods[i]) {
			continue
		}
		ready++
		players += r.Agents.Lookup(string(pods[i].UID)).Players
	}

	group.Status.ReadyReplicas = ready
	group.Status.ConnectedPlayers = players
	group.Status.Address = proxyAddress(pods, group.Spec.Expose.NodePort.Port)
	group.Status.ObservedGeneration = group.Generation

	switch {
	case meta.IsStatusConditionTrue(group.Status.Conditions, spawneryv1alpha1.ConditionDegraded):
		group.Status.Phase = "Degraded"
	case ready >= group.Spec.Replicas && ready > 0:
		group.Status.Phase = string(phase.Ready)
	default:
		group.Status.Phase = string(phase.Pending)
	}
}

// proxyAddress is where players connect.
//
// With NodePort that is a node's address plus the node port, and the operator
// has no right to read Node objects — nor does it need one: hostIP on a ready
// proxy pod is the address of a node that demonstrably has a proxy on it, and
// the pod is already watched. Granting a cluster-wide node read for a status
// string would be the same trade the bootstrapper refused when it declined the
// update verb on ServiceAccounts to restore a cosmetic label.
//
// Empty while nothing is ready, which is the truthful answer: there is nowhere
// to connect yet, and printing a node address for a proxy that is not serving
// would send players at a closed port.
func proxyAddress(pods []corev1.Pod, nodePort int32) string {
	for i := range pods {
		if isPodReady(&pods[i]) && pods[i].Status.HostIP != "" {
			return fmt.Sprintf("%s:%d", pods[i].Status.HostIP, nodePort)
		}
	}
	return ""
}

// setProxyGroupAccepted records whether the operator manages this group.
func setProxyGroupAccepted(group *spawneryv1alpha1.ProxyGroup, ok bool, reason, message string) {
	meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:    spawneryv1alpha1.ConditionAccepted,
		Status:  conditionStatus(ok),
		Reason:  reason,
		Message: message,
	})
}

func (r *ProxyGroupReconciler) writeStatus(ctx context.Context, group *spawneryv1alpha1.ProxyGroup) error {
	return r.Status().Update(ctx, group)
}

// SetupWithManager registers the controller.
func (r *ProxyGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spawneryv1alpha1.ProxyGroup{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
```

Two helpers:

- `conditionStatus(bool) metav1.ConditionStatus` already exists at `internal/controller/server_controller.go:641`. Use it; do not write a second one.
- `isPodReady` does **not** exist. `server_controller.go:373` reads the condition inline into `phase.Inputs`. Add it here rather than reaching into that:

```go
// isPodReady reports what the kubelet says about the pod's readiness probe.
// For a proxy that is the whole ready gate: design 6.6 has the agent serve the
// probe itself and only turn it green after it has processed its FullSync, so
// this condition already carries the answer the registry would otherwise be
// asked for.
func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
```

Add the `time` and `github.com/spawnery/spawnery/internal/phase` imports the struct and `setStatus` need.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/controller/ -run TestProxyGroup -v`
Expected: PASS.

- [ ] **Step 5: Add a ProxyGroup to the shipped sample**

Append to `config/samples/network.yaml`:

```yaml
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft
spec:
  networkRef:
    name: production
  replicas: 1
  # The Velocity image arrives with milestone 3b. Until then this group is
  # accepted, its Service is created and its pods stay in ImagePullBackOff.
  image: ghcr.io/spawnery/velocity:3.4.0-0.2.0
  expose:
    type: NodePort
    nodePort:
      port: 30001
  routing:
    fallbackGroups:
      - lobby
  config:
    playerLimit: 500
    motd: "A Spawnery network"
```

Also update the comment at the top of the file — it currently says the proxy layer arrives in milestone 3 and that nothing routes a player yet. Say what is now true: the proxy layer's operator side is here, the image is not.

- [ ] **Step 6: Run the sample test and the suite**

Run: `go test ./internal/controller/ -run TestSampleManifest -v` then `make test`
Expected: PASS. The sample test creates every object against the real structural schema and CEL rules, so a wrong `expose` block fails here rather than on a cluster.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/proxygroup_controller.go internal/controller/proxygroup_controller_test.go api/v1alpha1/common_types.go config/samples/network.yaml config/rbac
git commit -m "$(cat <<'EOF'
Where players connect is on the pod already, so no node read is needed

With NodePort the useful address is a node's, and the obvious way to get one
is to list Nodes — a cluster-wide read granted for a status string. hostIP on
a running proxy pod is the address of a node that demonstrably has a proxy on
it, and the pod is watched anyway. Same trade the bootstrapper made when it
declined an update verb to repair a cosmetic label.

Only NodePort is implemented. The other two strategies belong to milestone 6,
and a LoadBalancer branch written now would arrive there having never run:
the local flow cannot produce a cluster that would exercise it.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: The orphan sweep stops discarding proxy agents

Today `Sweep` lists pods with `role=server` and then forgets every registry entry not in that list. The first Velocity agent to open a session is dropped from the registry within one sweep interval, and nothing in the log ties the two events together.

**Files:**
- Modify: `internal/controller/orphan.go`
- Test: `internal/controller/orphan_test.go`

**Interfaces:**
- Produces: no signature change.

- [ ] **Step 1: Write the failing tests**

Add to `internal/controller/orphan_test.go`. It already has `newFixture`, `orphanReconciler` and the imports these need.

```go
// createProxyPod adds a managed proxy pod belonging to the named group, and
// returns its UID — the key the agent registry is on.
func (f *fixture) createProxyPod(name, group string) string {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ns,
			Labels:    podspec.ProxyLabels("production", group),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "velocity", Image: "velocity"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		f.t.Fatalf("create proxy pod: %v", err)
	}
	return string(pod.UID)
}

// `createProxyGroup` comes from Task 7's proxygroup_controller_test.go — both
// files are in package controller, so it is already available here.

// The defect this task exists to remove: a proxy agent connects, one sweep
// runs, and the registry has forgotten it. Nothing logs a reason, because from
// the sweep's point of view the pod never existed — it filtered on role=server
// and then pruned every registry key not in that list.
func TestSweepKeepsAConnectedProxyAgent(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)
	f.createProxyGroup("gateway")
	uid := f.createProxyPod("gateway-abcd", "gateway")

	f.agents.Connect(uid, agent.RoleProxy)

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if snap := f.agents.Lookup(uid); !snap.Known || !snap.Connected {
		t.Errorf("the sweep forgot a connected proxy agent: %+v", snap)
	}
}

// The mirror of the Server case. A proxy has no CR of its own, so its group is
// what owns it, and nothing else would ever remove the pod.
func TestSweepDeletesAProxyPodWhoseGroupIsGone(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)
	f.createProxyPod("gateway-orphan", "does-not-exist")

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway-orphan", Namespace: f.ns}, &corev1.Pod{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a proxy pod whose group is gone survived the sweep: %v", err)
	}
}

// The widened list must not make proxy pods candidates for the Server check.
// They carry no server label and never will, and deleting them for that would
// be the same bug pointing the other way.
func TestSweepKeepsAProxyPodThatHasItsGroup(t *testing.T) {
	f := newFixture(t)
	o := orphanReconciler(f)
	f.createProxyGroup("gateway")
	f.createProxyPod("gateway-abcd", "gateway")

	if err := o.Sweep(f.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if err := f.c.Get(f.ctx, types.NamespacedName{Name: "gateway-abcd", Namespace: f.ns}, &corev1.Pod{}); err != nil {
		t.Fatalf("the sweep deleted a proxy pod that has its group: %v", err)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/controller/ -run TestSweep -v`
Expected: `TestSweepKeepsAConnectedProxyAgent` FAILs; the other two fail on missing behaviour.

- [ ] **Step 3: Widen the sweep**

In `internal/controller/orphan.go`, change the List and add the ProxyGroup branch:

```go
	// Managed-by alone, not managed-by plus role=server. The role filter made
	// every proxy pod invisible here, and the registry pruning at the bottom
	// then forgot every proxy agent within one interval — a disconnect nothing
	// logged, because from this function's point of view the pod never existed.
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
	}); err != nil {
		return err
	}
```

Inside the loop, split on the role label rather than relying on the empty server name. Replace the body between the `DeletionTimestamp` check and the end of the loop:

```go
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}

		// Branch on the role explicitly. Before this milestone the List filter
		// made every pod here a server pod, and the empty-server-name guard
		// below was the only thing standing between a proxy pod and the Server
		// lookup. It is defence for a server pod that lost its label now, not
		// the mechanism that keeps the two roles apart.
		var err error
		switch pod.Labels[podspec.LabelRole] {
		case podspec.RoleServer:
			err = r.sweepServerPod(ctx, logger, pod)
		case podspec.RoleProxy:
			err = r.sweepProxyPod(ctx, logger, pod)
		}
		if err != nil {
			return err
		}
	}
```

And add the two halves below `Sweep`:

```go
// sweepServerPod deletes a managed server pod whose Server object is gone.
// Nobody owns such a pod, so nobody would ever drain it, and deleting it is
// the only way the group's count converges.
func (r *OrphanReconciler) sweepServerPod(ctx context.Context, logger logr.Logger, pod *corev1.Pod) error {
	serverName := pod.Labels[podspec.LabelServer]
	if serverName == "" {
		// A server pod with no server label: not something this function can
		// act on, and not something to guess about.
		return nil
	}

	srv := &spawneryv1alpha1.Server{}
	key := types.NamespacedName{Name: serverName, Namespace: pod.Namespace}
	err := r.Get(ctx, key, srv)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	logger.Info("deleting a managed pod whose Server is gone", "pod", pod.Name, "namespace", pod.Namespace)
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// sweepProxyPod deletes a proxy pod whose ProxyGroup is gone. A proxy has no
// CR of its own — the group owns the pod directly — so if the group is gone
// there is no controller left that would ever remove it.
func (r *OrphanReconciler) sweepProxyPod(ctx context.Context, logger logr.Logger, pod *corev1.Pod) error {
	groupName := pod.Labels[podspec.LabelGroup]
	if groupName == "" {
		return nil
	}

	group := &spawneryv1alpha1.ProxyGroup{}
	key := types.NamespacedName{Name: groupName, Namespace: pod.Namespace}
	err := r.Get(ctx, key, group)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	logger.Info("deleting a proxy pod whose ProxyGroup is gone", "pod", pod.Name, "namespace", pod.Namespace)
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
```

Add `"github.com/go-logr/logr"` to the imports. The registry pruning at the bottom of `Sweep` needs no change — it now sees both roles because `liveUIDs` is built from the widened list, which is the whole point.

Add the ProxyGroup read to the RBAC markers on this file if it is not already granted by Task 7's reconciler markers; `controller-gen` unions them, so a duplicate marker is harmless but an absent one is not.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/controller/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/orphan.go internal/controller/orphan_test.go
git commit -m "$(cat <<'EOF'
The sweep forgot every proxy agent a minute after it connected

It listed pods by role=server and then dropped every registry entry not in
that list, so a proxy's own entry was foreign by construction. The symptom
would have been a proxy that disconnects roughly once a minute with nothing
in the log about it, because from the sweep's point of view the pod never
existed.

Listing by managed-by alone fixes it and immediately raises the mirror case:
a proxy pod has no Server object and never will, so the two roles now branch
explicitly instead of leaning on an empty label to keep them apart.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: The registration intent is durable before the registration happens

**Files:**
- Modify: `internal/controller/server_controller.go`
- Test: `internal/controller/server_controller_test.go`

**Interfaces:**
- Produces: no signature change.

- [ ] **Step 1: Write the failing test**

`recordingRegistrar` already exists in `internal/controller/suite_test.go`. Give it a hook so a test can look at the cluster at the moment the side effect runs. In `suite_test.go`:

```go
// recordingRegistrar remembers the calls the controller made.
type recordingRegistrar struct {
	registered   []string
	deregistered []string
	drained      []string

	// onRegister runs inside Register, before it returns. It is how a test
	// observes what was already durable at the moment the proxies were told —
	// the ordering that matters here cannot be seen from outside the call.
	onRegister func(*spawneryv1alpha1.Server) error
}

func (r *recordingRegistrar) Register(_ context.Context, s *spawneryv1alpha1.Server) error {
	r.registered = append(r.registered, s.Name)
	if r.onRegister != nil {
		return r.onRegister(s)
	}
	return nil
}
```

Then in `internal/controller/server_controller_test.go`:

```go
// While the registrar was a no-op this window was harmless. With a real one it
// is not: if the status write is lost while players are already joining, a
// deletion in that window takes the branch "never registered, terminate
// immediately, no drain" and throws them out instead of moving them.
//
// The assertion is deliberately made from inside Register, against the API
// server rather than against the in-memory object: what matters is that the
// intent was durable before the proxies heard anything, and only a read-back
// can tell the difference.
func TestWasRegisteredIsDurableBeforeTheProxiesAreTold(t *testing.T) {
	f := newFixture(t)

	var wasDurable bool
	f.registrar.onRegister = func(s *spawneryv1alpha1.Server) error {
		persisted := &spawneryv1alpha1.Server{}
		key := types.NamespacedName{Name: s.Name, Namespace: f.ns}
		if err := f.c.Get(f.ctx, key, persisted); err != nil {
			t.Errorf("read the Server back inside Register: %v", err)
			return nil
		}
		wasDurable = persisted.Status.WasRegistered
		return nil
	}

	f.createServer("lobby-order")
	bringUpNamed(t, f, "lobby-order")

	if len(f.registrar.registered) != 1 {
		t.Fatalf("registered = %v, want exactly one registration", f.registrar.registered)
	}
	if !wasDurable {
		t.Error("the proxies were told about a server whose registration intent was not yet persisted")
	}
}

// The flag must not be left set with no registration behind it looking like a
// success: the reconcile has to fail so the next pass tries again.
func TestAFailedRegisterFailsTheReconcile(t *testing.T) {
	f := newFixture(t)
	f.registrar.onRegister = func(*spawneryv1alpha1.Server) error {
		return errors.New("no proxy accepted the registration")
	}

	f.createServer("lobby-refuse")
	f.reconcile("lobby-refuse")
	pod, ok := f.pod("lobby-refuse")
	if !ok {
		t.Fatal("reconcile did not create the pod")
	}
	f.setPodRunning("lobby-refuse", false)
	f.reconcile("lobby-refuse")

	f.setPodRunning("lobby-refuse", true)
	f.agents.Connect(string(pod.UID), agentRoleServer())
	f.agents.MarkReady(string(pod.UID))
	if err := f.agents.ReportPlayers(string(pod.UID), 0, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	// f.reconcile fails the test on error, so call the reconciler directly.
	_, err := f.reconc.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: types.NamespacedName{Name: "lobby-refuse", Namespace: f.ns},
	})
	if err == nil {
		t.Fatal("a refused registration did not fail the reconcile")
	}

	if got := f.server("lobby-refuse").Status.Registered; got {
		t.Error("status.registered is true after a registration that failed")
	}
}
```

`bringUpNamed` walks the server to `Ready` in three passes and already lives in `suite_test.go`. Add `"errors"` to the test file's imports.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/controller/ -run TestWasRegistered -v`
Expected: FAIL — the read inside `Register` sees `wasRegistered: false`.

- [ ] **Step 3: Persist the intent first**

In `applyDecision`, replace the `d.Register` block:

```go
	if d.Register {
		// Persisted before the side effect, not after. Remembered for the life
		// of this pod: from here on a deletion has to drain, even if the server
		// falls back out of Ready first. Writing it afterwards means a lost
		// status update in this window makes a later deletion take the "never
		// registered, terminate immediately" branch — with players already on
		// the server, because the proxies were told about it a moment ago.
		//
		// One extra status write, at the single transition into Ready.
		if !srv.Status.WasRegistered {
			srv.Status.WasRegistered = true
			if err := r.Status().Update(ctx, srv); err != nil {
				return fmt.Errorf("persist the registration intent for %s: %w", srv.Name, err)
			}
		}
		if err := r.Registrar.Register(ctx, srv); err != nil {
			return fmt.Errorf("register %s: %w", srv.Name, err)
		}
		srv.Status.Registered = true
	}
```

Check the surrounding code before writing this: `applyDecision` ends with a single `Status().Update`, and the extra write here must not lose the conditions set earlier in the reconcile. Read `ensureFinalizer`'s comment about status subresources being overwritten — the same hazard applies to any `Status().Update` in the middle of a reconcile, and the fix is that `Status().Update` writes only the status, so the object in hand keeps its conditions. Verify that with the test rather than assuming it.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/controller/ -v`
Expected: PASS, including every existing state-machine test.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/server_controller.go internal/controller/server_controller_test.go
git commit -m "$(cat <<'EOF'
A lost status write would throw out the players it was meant to move

status.wasRegistered was written after Register rather than before it. Lose
that write while players are already joining and a deletion in the window
takes the branch for a server the proxies never heard of: terminate at once,
no drain. The players it would have moved get disconnected instead.

Harmless while the registrar logged and returned nil. Not harmless from the
commit that gives it a fan-out to talk to, which is why it is fixed in the
same milestone rather than noted for a later one.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: Wire it up, and make the audit agree

**Files:**
- Modify: `internal/controller/setup.go`
- Modify: `internal/agentserver/server.go` (nil check on `Options.Proxies`)
- Modify: `cmd/spawnery-operator/main.go`
- Modify: `internal/rbacaudit/required.go`
- Modify: `docs/known-issues.md`
- Test: `internal/rbacaudit/audit_envtest_test.go` (already asserts the table against the generated role)

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Add the audit entries first, and watch the audit fail**

The markers from Task 7 are already in the tree, so `make manifests` has already widened the ClusterRole. Run the audit to see it red:

Run: `go test ./internal/rbacaudit/ -v`
Expected: FAIL — the generated role grants `services` and `proxygroups/status` which the table does not claim.

- [ ] **Step 2: Claim them**

In `internal/rbacaudit/required.go`, add:

```go
	// The proxy layer's Service. One per ProxyGroup, and the only way a player
	// reaches a proxy at all.
	{Group: "", Resource: "services", Verb: "get", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "list", Why: "ProxyGroupReconciler Owns(&corev1.Service{})"},
	{Group: "", Resource: "services", Verb: "watch", Why: "ProxyGroupReconciler Owns(&corev1.Service{})"},
	{Group: "", Resource: "services", Verb: "create", Why: "CreateOrUpdate in ProxyGroupReconciler"},
	{Group: "", Resource: "services", Verb: "update", Why: "CreateOrUpdate in ProxyGroupReconciler"},
```

and extend the ProxyGroup block, replacing the "nothing fetches a single ProxyGroup" comment, which stops being true in this milestone:

```go
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "get", Why: "ProxyGroupReconciler.Reconcile and proxyreg.fallbacks"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "list", Why: "NetworkReconciler counts proxy groups"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "watch", Why: "ProxyGroupReconciler For(&ProxyGroup{})"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Subresource: "status", Verb: "update", Why: "ProxyGroupReconciler writes replicas, address and conditions"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion on the pod and Service owner references"},
```

Run: `go test ./internal/rbacaudit/ -v`
Expected: PASS.

- [ ] **Step 3: Refuse a nil fleet at startup**

In `agentserver.New`, next to the existing defaulting:

```go
	// Refused at construction rather than at the first proxy stream: a nil
	// fleet would surface as a panic inside a gRPC handler, minutes after
	// start and in a goroutine, instead of as a startup error.
	if opts.Proxies == nil {
		panic("agentserver: no proxy fleet")
	}
```

If `New` has no error return today and adding one ripples through the tests, prefer the panic above with that comment; it is a programming error, not a runtime state. Check what `SetupAll` does for `opts.Bootstrapper == nil` and match the register.

- [ ] **Step 4: Wire the controller**

In `internal/controller/setup.go`, register the ProxyGroup controller alongside the others and add the fleet as a runnable:

```go
	if err := (&ProxyGroupReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorderFor("proxygroup"),
		Agents:        opts.Agents,
		Clock:         opts.Clock,
		Bootstrap:     opts.Bootstrapper,
		AgentEndpoint: opts.AgentEndpoint,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup proxy group controller: %w", err)
	}
```

- [ ] **Step 5: Build the fleet in main**

In `cmd/spawnery-operator/main.go`, construct it once and give it to both sides — the controllers write into it, the gRPC server reads from it, and there must be exactly one:

```go
	proxies := proxyreg.New(proxyreg.Options{Reader: mgr.GetClient()})
	if err := mgr.Add(proxies); err != nil {
		setupLog.Error(err, "unable to add the proxy resync")
		os.Exit(1)
	}
```

Replace `Registrar: controller.NoopRegistrar{}` with `Registrar: proxies`, and add `Proxies: proxies` to the `agentserver.Options`. Keep `NoopRegistrar` in the tree: it is what the controller tests use to observe decisions without a fan-out behind them.

- [ ] **Step 6: Update known-issues.md**

Under "Preconditions for milestone 3 (proxy integration)", strike what this milestone closed and say where it went — the orphan sweep, `ProxySession`, the `spawnery-proxy` bootstrap, and the `Register`-before-`WasRegistered` ordering. Leave the two 3b items (`oci-common.nix`, not extending `set_property`) in place. Add anything 3a discovered that 3b or 3c inherits.

- [ ] **Step 7: Full verification**

Run, and paste the real output into the commit or the summary rather than asserting it passed:

```bash
make manifests
make generate
make fmt
make vet
make test
go build ./...
```

Expected: `make test` green, and no longer than roughly 24 s plus whatever the new envtest packages genuinely cost. If it has grown by more than a couple of seconds, find out which test is sleeping.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "$(cat <<'EOF'
One fleet, written by the controllers and read by the gRPC endpoint

The two directions of the agent channel now both go through a port package:
internal/agent is what the endpoint writes and the controllers read,
internal/proxyreg is the reverse. main.go builds exactly one of each and
hands them to both sides, so there is no configuration in which a
registration reaches a fan-out nobody is streaming from.

The audit is part of the change rather than a follow-up. A new marker without
a table entry turns it red on purpose, and the entry is where the reason for
the permission is written down.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
EOF
)"
```

---

## Done when

1. A gRPC client with a real `spawnery-proxy` token opens a `ProxySession` and receives `ReportInterval`, `SessionDeadline` and a `FullSync`, in that order.
2. A `Server` reaching `Ready` produces a `RegisterServer` on every open proxy session in its namespace; entering `Draining` produces a `DrainPlayers`.
3. A proxy reconnecting during a drain receives a `FullSync` without the draining server, followed by its `DrainPlayers`.
4. A registration lost to cache staleness is present in the next `Resync`.
5. A `ProxyGroup` with `expose.type: NodePort` produces `spec.replicas` pods, a NodePort Service, and `status.address` of `<hostIP>:<nodePort>` once a pod is ready. `LoadBalancer` and `HostPort` produce `Accepted=False`.
6. A bootstrapped namespace holds both ServiceAccounts.
7. An orphan sweep with a connected proxy agent leaves its registry entry alone.
8. `status.wasRegistered` is durable before the first `Register` reaches a proxy.
9. `make test` is green and no slower than today.
