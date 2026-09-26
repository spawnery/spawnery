# Unretire and Proxy Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An admin can take a retirement back (`unretire`, which also holds the server against every automatic removal), and can see and retire single proxies, whose drain now waits for players instead of disconnecting them.

**Architecture:** Operator: `spec.hold` on `Server`, one way back out of `Retiring` in `phase.Decide`, held servers left out of the sizing and changeover rules; `ProxyView` learns a retire request, the proxy deletion loop only applies a deadline on a leaving node, an unknown count or `spec.update.maxStaleSeconds`, and proxy pods get their own feed events. Agent channel: `UnretireRequest`, `ProxyState` in `NetworkState`, `held` on `ServerState`; `RetireRequest` resolves a proxy pod when no server has the name. Agents: `unretire`, `proxies()`, `proxy(name)`, `ServerInfo.held()`, and `/cloud` learns both.

**Tech Stack:** Go (controller-runtime, envtest), protobuf, Java 21 API, Kotlin agents (Gradle via Nix).

**Spec:** `docs/superpowers/specs/2026-09-26-unretire-and-proxy-lifecycle-design.md`

## Global Constraints

- `unretire` is gated by `spawnery.cloud.retire`; no new permission node.
- Unretire is refused for `Draining`, `Terminating`, `Finished`, `Failed` ("already stopping") and for a server neither retiring nor carrying `spec.retire` ("not retiring"); accepted sets `spec.retire: false`, `spec.hold: true`.
- `Retiring → Ready` only when retirement is no longer requested and the pod runs with both ready signals; reason `RetirementWithdrawn`; `status.retiringSince` cleared.
- `spec.hold`: never nominated by `selectRetirement`, never in `deletable()`, never stale for `staleRemains`, `coldStart`, `ownServerChangeover`, and not counted as "older" on `Progressing`; still counts toward size; a node drain still condemns it; a manual retire still works.
- `RetireRequest` keeps its wire shape; a name that is not a `Server` resolves to a proxy pod, annotated `spawnery.cloud/retire-requested` (RFC 3339).
- Proxy drain deadline applies only on a leaving node, while the player count is unknown (`!snap.Connected || snap.PlayersStale`), or after `spec.update.maxStaleSeconds` (new on `ProxyGroup`, minimum 0, default 0 = never).
- Events regarding the proxy pod: `ProxyStarted` (first ready), `ProxyRetiring` (first mark; note: rolling update / retire requested / scaled down), `ProxyStopped` (deleted empty); `ProxyDrainTimeout` unchanged. `cloudevent.Derive` accepts a pod with `spawnery.cloud/role=proxy`.
- Proto field numbers: `CloudRequest.unretire = 10`, `CloudResponse.unretire = 11`, `NetworkState.proxies = 5`, `ServerState.held = 11`.
- `agent/api` keeps only `java.*` and `cloud.spawnery.agent.api.*` in public signatures (`PackagingInvariantTest`).
- Generated files are committed: `make manifests generate proto`; new files are `git add`ed before `make agent` (Nix reads the index).
- Commits: Conventional Commits with scope, body wrapped at 72, signed, ending with the session trailers.
- Nothing about a particular network or consumer goes into this public repo.

## Commands

```bash
NIX="nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c"
$NIX go -C /home/paul/git/spawnery test ./internal/phase/ -run 'TestName' -count=1
$NIX make -C /home/paul/git/spawnery manifests generate proto
$NIX make -C /home/paul/git/spawnery agent          # both plugins and their JUnit suites (git add first)
# whole suite on the development VM (8 cores, 12 GB):
$NIX make -C /home/paul/git/spawnery manifests generate fmt vet chart-lint toolchain-lint image-tag-lint docs-length-lint crd-docs-test chart-values-docs-test metrics-docs-test
$NIX go -C /home/paul/git/spawnery test -race -p 1 ./...
```

## Review Focus

1. Unretire racing the server running empty: the "no players left" branch must win, so an unretire that lands after the last player left changes nothing and the server still goes. Pinned in Task 2 (`running empty wins over a withdrawal`).
2. A held stale server must not keep the group's changeover (and a network budget place) open forever, and must not hold `Progressing` at "still being replaced". Pinned in Task 3 (`TestHoldEndsTheChangeoverForItsServer`, `TestProgressingDoesNotCountAHeldServer`).
3. A proxy with an unknown player count (agent gone) must still be removed at `drain.timeoutSeconds`, or a dead proxy drains forever. Pinned in Task 6 (`TestADrainingProxyWithAnUnknownCountStillHasADeadline`).
4. `RetireRequest` for a name that is a proxy already draining (rollout mark) must be refused as already retiring, not annotated a second time. Pinned in Task 5 (`TestRetireRefusesAProxyAlreadyDraining`).
5. `Derive` must keep game pods out of the feed while letting proxy pods in, so a kubelet-level event on a game pod never reaches players. Pinned in Task 6 (`TestAGamePodIsNotAnEvent`).

## Rulings made while planning

- **Node-leaving proxies keep `NodeDraining` and get no extra `ProxyRetiring`.** The spec's table lists "node leaving" as a `ProxyRetiring` note; the existing `NodeDraining` event already names the proxy and reaches the feed. Sending both would put the same fact in the feed twice. Cost if wrong: one extra event type to add.
- **The proxy annotations move to `podspec`** (`AnnotationProxyDrainingSince`, `AnnotationRetireRequested`, `AnnotationProxyReadySince`), with `controller.ProxyDrainingSinceAnnotation` kept as an alias, because `netstate` and `agentserver` read them and neither may import `controller`.
- **`ServerInfo` keeps its old 10-argument constructor** beside the new canonical one, so a plugin that builds a `ServerInfo` in its own tests keeps compiling.

---

### Task 1: API fields

**Files:**
- Modify: `api/v1alpha1/server_types.go` (ServerSpec, after `Retire`)
- Modify: `api/v1alpha1/proxygroup_types.go` (ProxyGroupSpec after `Drain`; accessor after `DrainTimeout`)
- Modify: `internal/podspec/labels.go` (annotation constants)
- Modify: `internal/controller/proxygroup_controller.go:62` (alias)
- Test: `api/v1alpha1/proxygroup_types_test.go`
- Generated: CRDs, chart template, deepcopy, `docs/reference/crds.md`

**Interfaces:**
- Produces: `ServerSpec.Hold bool` (`json:"hold,omitempty"`); `ProxyUpdateSpec{MaxStaleSeconds int32}`; `ProxyGroupSpec.Update *ProxyUpdateSpec`; `func (g *ProxyGroup) MaxStale() time.Duration`; `podspec.AnnotationProxyDrainingSince = "spawnery.cloud/draining-since"`, `podspec.AnnotationRetireRequested = "spawnery.cloud/retire-requested"`, `podspec.AnnotationProxyReadySince = "spawnery.cloud/ready-since"`.

- [ ] **Step 1: Failing accessor test** — append to `api/v1alpha1/proxygroup_types_test.go`:

```go
func TestProxyGroupMaxStale(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update *ProxyUpdateSpec
		want   time.Duration
	}{
		{"no update block", nil, 0},
		{"zero means never", &ProxyUpdateSpec{MaxStaleSeconds: 0}, 0},
		{"a bound", &ProxyUpdateSpec{MaxStaleSeconds: 600}, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &ProxyGroup{Spec: ProxyGroupSpec{Update: tc.update}}
			if got := g.MaxStale(); got != tc.want {
				t.Errorf("MaxStale() = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run** `$NIX go -C /home/paul/git/spawnery test ./api/v1alpha1/ -run TestProxyGroupMaxStale -count=1` — Expected: compile FAIL (`undefined: ProxyUpdateSpec`).

- [ ] **Step 3: Implement.** In `server_types.go`, after the `Retire` field:

```go
	// Hold keeps this server against every automatic removal: a rolling
	// update does not retire it, and neither the demand rule nor a lowered
	// maxReplicas deletes it. It stays until it ends by itself. Set by the
	// agent endpoint's unretire request; a node drain still moves it, and a
	// retire request still retires it.
	// +optional
	Hold bool `json:"hold,omitempty"`
```

In `proxygroup_types.go`, after `Drain`:

```go
	// Update bounds how long a draining proxy may wait for its players.
	// +optional
	Update *ProxyUpdateSpec `json:"update,omitempty"`
```

and the type:

```go
// ProxyUpdateSpec controls how a proxy group lets draining proxies go.
type ProxyUpdateSpec struct {
	// MaxStaleSeconds disconnects the players left on a draining proxy after
	// this many seconds. 0, the default, means a drain waits for its players:
	// the proxy takes no new connections and stops once empty.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxStaleSeconds int32 `json:"maxStaleSeconds,omitempty"`
}
```

and after `DrainTimeout`:

```go
// MaxStale is spec.update.maxStaleSeconds, 0 when unset.
func (g *ProxyGroup) MaxStale() time.Duration {
	if g.Spec.Update == nil {
		return 0
	}
	return time.Duration(g.Spec.Update.MaxStaleSeconds) * time.Second
}
```

In `internal/podspec/labels.go`, beside the other annotation constants:

```go
	// AnnotationProxyDrainingSince dates when a proxy started draining.
	AnnotationProxyDrainingSince = "spawnery.cloud/draining-since"
	// AnnotationRetireRequested marks a proxy an admin asked to retire.
	AnnotationRetireRequested = "spawnery.cloud/retire-requested"
	// AnnotationProxyReadySince dates a proxy's first pass of the ready gate.
	AnnotationProxyReadySince = "spawnery.cloud/ready-since"
```

In `proxygroup_controller.go:62` replace the literal: `ProxyDrainingSinceAnnotation = podspec.AnnotationProxyDrainingSince`.

- [ ] **Step 4: Regenerate and run** `$NIX make -C /home/paul/git/spawnery manifests generate` then the Step 2 test — Expected: PASS; `git status` shows the CRDs, chart, deepcopy and `docs/reference/crds.md`.

- [ ] **Step 5: Commit** `feat(api): spec.hold on Server and spec.update.maxStaleSeconds on ProxyGroup`.

---

### Task 2: Phase — a withdrawn retirement goes back to Ready

**Files:**
- Modify: `internal/phase/phase.go` (reasons ~line 149; `case Retiring:` ~line 444)
- Modify: `internal/phase/phase_test.go` (`TestNoPathBackFromRetiring` ~line 598)
- Modify: `internal/controller/server_controller.go:1006` (`case phase.Ready:`)

**Interfaces:**
- Produces: `phase.ReasonRetirementWithdrawn = "RetirementWithdrawn"`.

- [ ] **Step 1: Failing tests** — append to `phase_test.go`:

```go
func TestAWithdrawnRetirementGoesBackToReady(t *testing.T) {
	healthy := Inputs{
		PodExists: true, PodRunning: true, PodReady: true, AgentReady: true, AgentConnected: true,
		PlayersOnline: 2,
	}
	for _, tc := range []struct {
		name string
		in   Inputs
		want Decision
	}{
		{
			name: "withdrawn and healthy",
			in:   healthy,
			want: Decision{Next: Ready, Register: true, Reason: ReasonRetirementWithdrawn},
		},
		{
			name: "withdrawn but the probe is red",
			in:   func() Inputs { in := healthy; in.PodReady = false; return in }(),
			want: Decision{Next: Retiring, Reason: ReasonRetiring},
		},
		{
			name: "running empty wins over a withdrawal",
			in:   func() Inputs { in := healthy; in.PlayersOnline = 0; return in }(),
			want: Decision{Next: Terminating, DeletePod: true, Reason: ReasonDrained},
		},
		{
			name: "still requested stays retiring",
			in:   func() Inputs { in := healthy; in.RetirementRequested = true; return in }(),
			want: Decision{Next: Retiring, Reason: ReasonRetiring},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(Retiring, tc.in)
			got.Message = ""
			if got != tc.want {
				t.Errorf("Decide(Retiring, %+v)\n got  %+v\n want %+v", tc.in, got, tc.want)
			}
		})
	}
}
```

Change `TestNoPathBackFromRetiring` so its `in` carries `RetirementRequested: true`, and its comment's first sentence reads: "Retiring is one-way while the retirement stands, like Draining."

Before running, check `Occupied()` in `phase.go`: if `PlayersOnline` is not the field it reads, use the field it does read for the player count (the empty case must be "not occupied"), and ledger it.

- [ ] **Step 2: Run** `$NIX go -C /home/paul/git/spawnery test ./internal/phase/ -count=1` — Expected: compile FAIL (`undefined: ReasonRetirementWithdrawn`); after adding only the constant, FAIL on "withdrawn and healthy" (got Retiring) and on `TestNoPathBackFromRetiring` passing only because of the new input.

- [ ] **Step 3: Implement.** Constant beside `ReasonRetiring`: `ReasonRetirementWithdrawn = "RetirementWithdrawn"`. In `case Retiring:`, after the `DeletionRequested` branch and before `MaxStaleReached`:

```go
		if !in.RetirementRequested {
			if in.PodRunning && in.PodReady && in.AgentReady && in.AgentStreamDownFor < StreamDownGrace {
				return Decision{
					Next: Ready, Register: true,
					Reason: ReasonRetirementWithdrawn, Message: "retirement was withdrawn",
				}
			}
			return Decision{
				Next:   Retiring,
				Reason: ReasonRetiring, Message: "retirement was withdrawn, waiting for both ready signals",
			}
		}
```

In `server_controller.go`, `case phase.Ready:` gains `srv.Status.RetiringSince = nil`.

- [ ] **Step 4: Run** `$NIX go -C /home/paul/git/spawnery test ./internal/phase/ -count=1` — Expected: PASS.

- [ ] **Step 5: Commit** `feat(phase): a withdrawn retirement goes back to Ready`.

---

### Task 3: Held servers in the sizing and changeover rules

**Files:**
- Modify: `internal/controller/candidates.go` (`ServerView`)
- Modify: `internal/controller/servergroup_controller.go:1440` (view), `reportProgressing` ("older")
- Modify: `internal/controller/scaling.go` (`selectRetirement`, `deletable`, `coldStart`, `staleRemains`)
- Modify: `internal/controller/changeover.go` (`ownServerChangeover`)
- Test: `internal/controller/scaling_test.go`, `internal/controller/changeover_test.go`, `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Produces: `ServerView.Hold bool`.

- [ ] **Step 1: Failing tests** — append to `scaling_test.go`:

```go
func held(v ServerView) ServerView { v.Hold = true; return v }

func TestAHeldServerIsNeverRetired(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:   []ServerView{held(staleReady("old", 10, 100, "old")), ready("new", 0, 100)},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	})
	if len(got.Retire) != 0 {
		t.Errorf("Retire = %v, want none: the server is held", got.Retire)
	}
}

func TestAHeldServerIsNeverDeletedForDemandOrTheCeiling(t *testing.T) {
	idle := held(ready("idle", 0, 100))
	idle.EmptyFor = time.Hour
	other := ready("other", 0, 100)
	other.EmptyFor = time.Hour
	for _, max := range []int32{10, 1} {
		got := DecideSize(ScalingInputs{
			Views:       []ServerView{idle, other},
			MinReplicas: 0, MaxReplicas: max, SpareSlots: 0, MaxPlayers: 100,
			Stabilization: time.Minute,
		})
		for _, name := range got.Delete {
			if name == "idle" {
				t.Errorf("maxReplicas %d: Delete = %v, want the held server kept", max, got.Delete)
			}
		}
	}
}

func TestHoldEndsTheChangeoverForItsServer(t *testing.T) {
	in := ScalingInputs{
		Views:   []ServerView{held(staleReady("old", 10, 100, "old"))},
		PodHash: "current", MaxUnavailable: 1,
		MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100,
	}
	if staleRemains(in) || coldStart(in) {
		t.Errorf("staleRemains = %v coldStart = %v, want both false for a held server", staleRemains(in), coldStart(in))
	}
}

func TestANodeDrainStillCondemnsAHeldServer(t *testing.T) {
	v := held(ready("held", 5, 100))
	v.Condemned = true
	got := DecideSize(ScalingInputs{Views: []ServerView{v}, MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40, MaxPlayers: 100})
	if len(got.Condemn) != 1 {
		t.Errorf("Condemn = %v, want the held server: its node is leaving", got.Condemn)
	}
}
```

Add to `TestOwnServerChangeover`'s table in `changeover_test.go`:

```go
		{"a held stale server is no changeover", []ServerView{{Name: "old", PodHash: "old", Phase: phase.Ready, Hold: true}, current}, 0, false, "", spawneryv1alpha1.ChangeoverNone},
```

Append to `servergroup_controller_test.go`:

```go
func TestProgressingDoesNotCountAHeldServer(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{ObjectMeta: metav1.ObjectMeta{Name: "lobby", Generation: 2}}
	reportProgressing(group, []ServerView{
		{Name: "lobby-new", PodHash: "current", Phase: phase.Ready},
		{Name: "lobby-old", PodHash: "old", Phase: phase.Ready, Hold: true},
	}, "current", nil, FloorReport{})
	cond := meta.FindStatusCondition(group.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
	if cond == nil || cond.Reason != spawneryv1alpha1.ReasonAtDesiredState {
		t.Errorf("Progressing = %+v, want AtDesiredState: a held server is not being replaced", cond)
	}
}
```

- [ ] **Step 2: Run** `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'Held|Hold|TestOwnServerChangeover|TestProgressingDoesNotCountAHeldServer' -count=1` — Expected: compile FAIL (`unknown field Hold`), then after adding the field, assertion FAILs for all but `TestANodeDrainStillCondemnsAHeldServer` (guard).

- [ ] **Step 3: Implement.** `ServerView` gains `Hold bool` (after `Retire`). In the view built at `servergroup_controller.go:1440`: `Hold: srv.Spec.Hold,`. Then:
  - `selectRetirement`: the stale-collection condition gains `!v.Hold`.
  - `deletable`: `if in.PendingDeletes[v.Name] || in.PendingRetires[v.Name] || v.Retire || v.Condemned || v.Hold { continue }`.
  - `coldStart` and `staleRemains`: `if v.Hold { continue }` at the top of each loop, so a held server counts neither as stale nor as current there.
  - `ownServerChangeover`: `if v.Hold { continue }` at the top of the loop.
  - `reportProgressing`: `if !group.IsOnDemand() && staleSpec(v, podHash) && !v.Hold { older++ ... }`.

- [ ] **Step 4: Run** the Step 2 command and `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run 'TestDecideSize|TestWhenEmpty|Changeover|Progressing' -count=1` — Expected: PASS.

- [ ] **Step 5: Commit** `feat(controller): a held server is never removed automatically`.

---

### Task 4: Unretire over the agent channel (operator side)

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Generated: `internal/agentpb/*`, `agent/common/src/proto/java/**`
- Modify: `internal/agentserver/writer.go` (interface, `KubeWriter.Unretire`, sentinels)
- Modify: `internal/agentserver/requests.go` (dispatch, `answerUnretire`, `unretired`)
- Modify: `internal/netstate/netstate.go:210` (`Held`)
- Test: `internal/agentserver/requests_test.go`, `internal/agentserver/retire_envtest_test.go`, `internal/controller/unretire_envtest_test.go` (new)

**Interfaces:**
- Produces: proto `UnretireRequest{string server = 1}`, `UnretireResult{string server = 1}`, `CloudRequest.unretire = 10`, `CloudResponse.unretire = 11`, `ServerState.held = 11`, `ProxyState{name=1, group=2, ready=3, draining=4, players=5}`, `NetworkState.proxies = 5` (all proto changes of this plan land here, so `make proto` runs once); `ClusterWriter.Unretire(ctx, namespace, name string) error`; `ErrServerStopping`, `ErrNotRetiring`.

- [ ] **Step 1: Proto.** Add the messages and fields above, each with a one-line comment in the file's voice (`// UnretireRequest takes a retirement back ...`), then `$NIX make -C /home/paul/git/spawnery proto` and `git add` the generated trees.

- [ ] **Step 2: Failing writer tests** — append to `requests_test.go` (package `agentserver`, fake client as in `TestStartRefusesAnOnDemandGroupWithNoCeiling`):

```go
func unretireWriter(t *testing.T, srv *spawneryv1alpha1.Server) KubeWriter {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return KubeWriter{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(srv).WithStatusSubresource(srv).Build(), Clock: time.Now}
}

func serverIn(phase string, retire, hold bool) *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-a", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{Retire: retire, Hold: hold},
		Status:     spawneryv1alpha1.ServerStatus{Phase: phase},
	}
}

func TestUnretireTakesTheRetirementBackAndHoldsTheServer(t *testing.T) {
	w := unretireWriter(t, serverIn("Retiring", true, false))
	if err := w.Unretire(context.Background(), "ns", "lobby-a"); err != nil {
		t.Fatalf("Unretire: %v", err)
	}
	var got spawneryv1alpha1.Server
	_ = w.Client.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "lobby-a"}, &got)
	if got.Spec.Retire || !got.Spec.Hold {
		t.Errorf("spec retire=%v hold=%v, want false/true", got.Spec.Retire, got.Spec.Hold)
	}
}

func TestUnretireRefusesWhatIsAlreadyStoppingOrNotRetiring(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  *spawneryv1alpha1.Server
		want error
	}{
		{"draining", serverIn("Draining", true, false), ErrServerStopping},
		{"terminating", serverIn("Terminating", true, false), ErrServerStopping},
		{"finished", serverIn("Finished", false, false), ErrServerStopping},
		{"failed", serverIn("Failed", true, false), ErrServerStopping},
		{"never retired", serverIn("Ready", false, false), ErrNotRetiring},
		{"already held", serverIn("Ready", false, true), ErrNotRetiring},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unretireWriter(t, tc.srv).Unretire(context.Background(), "ns", "lobby-a")
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if err := unretireWriter(t, serverIn("Ready", true, false)).Unretire(context.Background(), "ns", "other"); !errors.Is(err, ErrNoSuchServer) {
		t.Errorf("unknown server: err = %v, want ErrNoSuchServer", err)
	}
}

func TestUnretireAnswersEachRefusalForAPerson(t *testing.T) {
	s := &Server{opts: Options{Writer: unretireWriter(t, serverIn("Draining", true, false))}, requestRate: newRequestLimiter(time.Now)}
	resp := s.answerUnretire(context.Background(), logr.Discard(),
		grpcauth.Identity{Namespace: "ns", PodName: "lobby-b", PodUID: "b", Role: agent.RoleServer},
		3, &agentpb.UnretireRequest{Server: "lobby-a"})
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED || !strings.Contains(resp.GetError().GetMessage(), "already stopping") {
		t.Errorf("answer = %+v, want REFUSED naming that it is already stopping", resp.GetResult())
	}
}
```

- [ ] **Step 3: Run** `$NIX go -C /home/paul/git/spawnery test ./internal/agentserver/ -run Unretire -count=1` — Expected: compile FAIL (`w.Unretire undefined`).

- [ ] **Step 4: Implement.** In `writer.go`: sentinels `ErrServerStopping = errors.New("server is already stopping")`, `ErrNotRetiring = errors.New("server is not retiring")`; `Unretire(ctx context.Context, namespace, name string) error` on the interface; `KubeWriter.Unretire`:

```go
func (w KubeWriter) Unretire(ctx context.Context, namespace, name string) error {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNoSuchServer
		}
		return err
	}
	switch phase.Phase(srv.Status.Phase) {
	case phase.Draining, phase.Terminating, phase.Finished, phase.Failed:
		return ErrServerStopping
	}
	if !srv.Spec.Retire && phase.Phase(srv.Status.Phase) != phase.Retiring {
		return ErrNotRetiring
	}
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Retire = false
	srv.Spec.Hold = true
	return w.Client.Patch(ctx, &srv, patch)
}
```

In `requests.go`: dispatch `case req.GetUnretire() != nil: return s.answerUnretire(ctx, logger, id, req.GetId(), req.GetUnretire())`, and

```go
func (s *Server) answerUnretire(
	ctx context.Context, logger logr.Logger, id grpcauth.Identity, reqID uint64, req *agentpb.UnretireRequest,
) *agentpb.CloudResponse {
	err := s.opts.Writer.Unretire(ctx, id.Namespace, req.GetServer())
	switch {
	case errors.Is(err, ErrNoSuchServer):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND, "no server by that name is on this network")
	case errors.Is(err, ErrServerStopping):
		return refuse(reqID, agentpb.RequestError_REFUSED, "that server is already stopping")
	case errors.Is(err, ErrNotRetiring):
		return refuse(reqID, agentpb.RequestError_REFUSED, "that server is not retiring")
	case err != nil:
		logger.V(1).Info("could not unretire a server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not write that just now")
	}
	return &agentpb.CloudResponse{Id: reqID, Result: &agentpb.CloudResponse_Unretire{Unretire: &agentpb.UnretireResult{Server: req.GetServer()}}}
}
```

In `netstate.go:210`, the `ServerState` literal gains `Held: srv.Spec.Hold,`. Every other `ClusterWriter` implementation in tests gains a stub `Unretire` returning `nil`.

- [ ] **Step 5: Run** the Step 3 command — Expected: PASS.

- [ ] **Step 6: Envtest over the wire** — append to `retire_envtest_test.go`, following `TestRetireSetsTheFlagAndSaysSo` (write `unretireOverTheWire` beside `retireOverTheWire`, sending `CloudRequest{Unretire: ...}`):

```go
func TestUnretireOverTheWireHoldsTheServer(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeServer(t, f, "lobby-aaaa")
	retireOverTheWire(t, f, pod, "lobby-aaaa")

	resp := unretireOverTheWire(t, f, pod, "lobby-aaaa")
	if resp.GetUnretire().GetServer() != "lobby-aaaa" {
		t.Fatalf("answer = %+v, want an UnretireResult naming lobby-aaaa", resp.GetResult())
	}
	if retiring(t, f, f.ns, "lobby-aaaa") {
		t.Error("spec.retire is still true")
	}
}
```

- [ ] **Step 7: Reconciler envtest** — create `internal/controller/unretire_envtest_test.go` (Apache header, `Copyright paul_wtf.`), using the fixture helpers from `rolloutfloor_envtest_test.go` (`setUpdatePolicy`, `reportPlayersOn`, `bringUpCurrent`) and `changeover_envtest_test.go` (`setImage`, `nextImage`):

```go
func TestAnUnretiredServerComesBackAndIsNotRetiredAgain(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.setUpdatePolicy(t, 1, &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1})
	f.reconcileNamedGroup(t, r, "lobby")
	f.readyAllServersOf(t, "lobby")
	name := f.serverNamesOfGroup(t, "lobby")[0]
	f.reportPlayersOn(t, name, 3)

	f.setImage(t, "lobby", nextImage)
	f.reconcileNamedGroup(t, r, "lobby")
	f.bringUpCurrent(t, "lobby")
	f.reconcileNamedGroup(t, r, "lobby")
	f.reconcile(name)
	if got := f.server(name).Status.Phase; got != string(phase.Retiring) {
		t.Fatalf("phase = %s, want Retiring before the unretire", got)
	}

	srv := f.server(name)
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Retire, srv.Spec.Hold = false, true
	if err := f.c.Patch(f.ctx, srv, patch); err != nil {
		t.Fatalf("patch: %v", err)
	}
	f.reconcile(name)
	f.reconcileNamedGroup(t, r, "lobby")
	f.reconcileNamedGroup(t, r, "lobby")

	got := f.server(name)
	if got.Status.Phase != string(phase.Ready) || !got.Status.Registered || got.Status.RetiringSince != nil {
		t.Errorf("phase=%s registered=%v retiringSince=%v, want Ready, registered, cleared",
			got.Status.Phase, got.Status.Registered, got.Status.RetiringSince)
	}
	if got.Spec.Retire {
		t.Error("the roll retired the held server again")
	}
}
```

Run `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -run TestAnUnretiredServerComesBackAndIsNotRetiredAgain -count=1` — Expected: PASS. Prove it bites in a throwaway worktree by removing `Hold: srv.Spec.Hold,` from the view (expected FAIL "retired the held server again").

- [ ] **Step 8: Commit** `feat(agentserver): unretire takes a retirement back and holds the server` (proto, generated code, writer, requests, netstate, tests).

---

### Task 5: Retire a proxy, and proxies in the network picture

**Files:**
- Modify: `internal/agentserver/writer.go` (`KubeWriter.Retire`)
- Modify: `internal/agentserver/requests.go` (`answerRetire` not-found message)
- Modify: `internal/netstate/netstate.go` (`Build`: proxies)
- Test: `internal/agentserver/requests_test.go`, `internal/netstate/netstate_test.go`

**Interfaces:**
- Consumes: `podspec.AnnotationRetireRequested`, `podspec.AnnotationProxyDrainingSince`, proto `ProxyState` (Task 4).
- Produces: nothing new beyond behaviour.

- [ ] **Step 1: Failing tests** — `requests_test.go`:

```go
func proxyPod(name string, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "ns", Annotations: annotations,
		Labels: podspec.ProxyLabels("production", "gateway"),
	}}
}

func proxyWriter(t *testing.T, objs ...client.Object) KubeWriter {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = spawneryv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return KubeWriter{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), Clock: time.Now}
}

func TestRetireMarksAProxyWhenNoServerHasTheName(t *testing.T) {
	w := proxyWriter(t, proxyPod("gateway-abc", nil))
	applied, err := w.Retire(context.Background(), "ns", "gateway-abc")
	if err != nil || !applied {
		t.Fatalf("Retire = %v, %v; want applied", applied, err)
	}
	var pod corev1.Pod
	_ = w.Client.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "gateway-abc"}, &pod)
	if _, ok := pod.Annotations[podspec.AnnotationRetireRequested]; !ok {
		t.Error("the proxy carries no retire request")
	}
}

func TestRetireRefusesAProxyAlreadyDraining(t *testing.T) {
	for _, ann := range []string{podspec.AnnotationRetireRequested, podspec.AnnotationProxyDrainingSince} {
		w := proxyWriter(t, proxyPod("gateway-abc", map[string]string{ann: "2026-09-26T12:00:00Z"}))
		if applied, err := w.Retire(context.Background(), "ns", "gateway-abc"); err != nil || applied {
			t.Errorf("%s: Retire = %v, %v; want not applied", ann, applied, err)
		}
	}
}

func TestRetireDoesNotTouchAGamePodOfThatName(t *testing.T) {
	game := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lobby-a", Namespace: "ns",
		Labels: map[string]string{podspec.LabelRole: podspec.RoleServer}}}
	if _, err := proxyWriter(t, game).Retire(context.Background(), "ns", "lobby-a"); !errors.Is(err, ErrNoSuchServer) {
		t.Errorf("err = %v, want ErrNoSuchServer", err)
	}
}
```

`netstate_test.go`: the `source` helper's scheme gains `corev1.AddToScheme(scheme)`, then

```go
func proxyPodIn(ns, name string, ready, draining bool) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns, UID: types.UID(name + "-uid"),
		Labels: podspec.ProxyLabels("production", "gateway"),
	}}
	if draining {
		pod.Annotations = map[string]string{podspec.AnnotationProxyDrainingSince: "2026-09-26T12:00:00Z"}
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return pod
}

func TestBuildListsEveryProxy(t *testing.T) {
	src, reg := source(t,
		proxyGroupNamed("ns", "gateway"),
		proxyPodIn("ns", "gateway-a", true, false),
		proxyPodIn("ns", "gateway-b", true, true),
		proxyPodIn("other", "gateway-x", true, false),
	)
	reg.Connect("gateway-a-uid", agent.RoleProxy)
	if err := reg.ReportPlayers("gateway-a-uid", 3, 100); err != nil {
		t.Fatalf("ReportPlayers: %v", err)
	}

	got, err := src.Build(context.Background(), "ns", netstate.ForProxies)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got.GetProxies()) != 2 {
		t.Fatalf("proxies = %v, want the two in this namespace", got.GetProxies())
	}
	a, b := got.GetProxies()[0], got.GetProxies()[1]
	if a.GetName() != "gateway-a" || !a.GetReady() || a.GetDraining() || a.GetPlayers() != 3 || a.GetGroup() != "gateway" {
		t.Errorf("gateway-a = %+v", a)
	}
	if b.GetName() != "gateway-b" || !b.GetDraining() {
		t.Errorf("gateway-b = %+v, want it draining", b)
	}
}
```

- [ ] **Step 2: Run** `$NIX go -C /home/paul/git/spawnery test ./internal/agentserver/ ./internal/netstate/ -run 'Retire|Proxy|BuildListsEveryProxy' -count=1` — Expected: FAIL (proxy not found / no proxies).

- [ ] **Step 3: Implement.** `KubeWriter.Retire`: on `IsNotFound` for the `Server`, `Get` a `corev1.Pod` of that name; if not found or its `podspec.LabelRole` is not `podspec.RoleProxy`, return `ErrNoSuchServer`; if it carries `AnnotationRetireRequested` or `AnnotationProxyDrainingSince`, return `(false, nil)`; else merge-patch `AnnotationRetireRequested: w.clock().UTC().Format(time.RFC3339)` and return `(true, nil)`. `answerRetire`'s not-found message becomes `"no server or proxy by that name is on this network"`.

`netstate.Build`: after the proxy groups, list `corev1.PodList` in the namespace with `client.MatchingLabels{podspec.LabelRole: podspec.RoleProxy}`; skip pods with a deletion timestamp; for each, `snap := s.Agents.Lookup(string(pod.UID))` and append

```go
state.Proxies = append(state.Proxies, &agentpb.ProxyState{
	Name:     pod.Name,
	Group:    pod.Labels[podspec.LabelGroup],
	Ready:    podReady(&pod),
	Draining: pod.Annotations[podspec.AnnotationProxyDrainingSince] != "",
	Players:  snap.Players,
})
```

with a local `podReady` reading the `corev1.PodReady` condition (the controller's `isPodReady` is in another package). Sort by name so the picture is stable.

- [ ] **Step 4: Run** the Step 2 command — Expected: PASS.

- [ ] **Step 5: Commit** `feat(agentserver): retire a proxy by name, and proxies in the network picture`.

---

### Task 6: Proxy drain that waits, retire requests, and proxy events

**Files:**
- Modify: `internal/controller/rollout.go` (`ProxyView`, `pick`)
- Modify: `internal/controller/proxygroup_controller.go` (views ~915, `drainDeparting` marks ~1238 and deletion loop ~1334, a new `announceReady`, the reconciler doc comment listing events)
- Modify: `internal/cloudevent/derive.go`
- Test: `internal/controller/rollout_test.go`, `internal/controller/proxygroup_controller_test.go`, `internal/cloudevent/derive_test.go`

**Interfaces:**
- Consumes: `ProxyGroup.MaxStale()` (Task 1), `podspec.AnnotationRetireRequested`, `podspec.AnnotationProxyReadySince`.
- Produces: `ProxyView.RetireRequested bool`.

- [ ] **Step 1: Failing tests.** `rollout_test.go`, a new case in the `DecideRollout` table:

```go
{
	name: "a retire request is drained first among stale pods",
	pods: []ProxyView{
		{Name: "a", Ready: true, Stale: true, Players: 0, CreatedAt: at(0)},
		{Name: "b", Ready: true, Stale: true, RetireRequested: true, Players: 5, CreatedAt: at(1)},
		{Name: "c", Ready: true, Players: 0, CreatedAt: at(2)},
		{Name: "d", Ready: true, Players: 0, CreatedAt: at(3)},
	},
	replicas: 3,
	want:     RolloutDecision{Drain: []string{"b"}},
},
```

`derive_test.go`:

```go
func TestAProxyPodIsAnEventAboutThatProxy(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway-abc", Namespace: "minecraft",
		Labels: map[string]string{podspec.LabelRole: podspec.RoleProxy, podspec.LabelGroup: "gateway"}}}
	ns, ev, ok := Derive(pod, corev1.EventTypeNormal, "ProxyStarted", "proxy gateway-abc is taking connections")
	if !ok || ns != "minecraft" || ev.GetSubject() != "gateway-abc" || ev.GetGroup() != "gateway" {
		t.Errorf("Derive = %q %+v %v, want the proxy as subject and its group", ns, ev, ok)
	}
}

func TestAGamePodIsNotAnEvent(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lobby-a", Namespace: "minecraft",
		Labels: map[string]string{podspec.LabelRole: podspec.RoleServer, podspec.LabelGroup: "lobby"}}}
	if _, _, ok := Derive(pod, corev1.EventTypeWarning, "Unhealthy", "probe failed"); ok {
		t.Error("a game pod's event reached the feed")
	}
}
```

`proxygroup_controller_test.go` (envtest, helpers `createProxyGroup`, `reconcileProxyGroup`, `proxyPods`, `sortPodsOldestFirst`, `reportProxyPlayers`, `setProxyReplicas`, `markProxyPodReady`, `drainEvents`; the clock is `f.clock`, advance it with the fixture's existing helper):

```go
func TestADrainingProxyWithPlayersIsNotDeletedAtTheDrainTimeout(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	surplus := pods[1]
	f.reportProxyPlayers(t, surplus, 3)
	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")

	f.clock.advance(time.Hour) // far past drain.timeoutSeconds
	f.reportProxyPlayers(t, surplus, 3)
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got != 2 {
		t.Fatalf("proxy pods = %d, want 2: a proxy with players waits", got)
	}
}

func drainOneProxy(t *testing.T, f *fixture, r *ProxyGroupReconciler, mutate ...func(*spawneryv1alpha1.ProxyGroup)) corev1.Pod {
	t.Helper()
	f.createProxyGroup("gateway", mutate...)
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	surplus := pods[1]
	f.reportProxyPlayers(t, surplus, 3)
	f.setProxyReplicas("gateway", 1)
	f.reconcileProxyGroup(r, "gateway")
	return surplus
}

func TestADrainingProxyWithAnUnknownCountStillHasADeadline(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)
	surplus := drainOneProxy(t, f, r)

	f.agents.Disconnect(string(surplus.UID))
	f.clock.Advance(time.Hour)
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got != 1 {
		t.Fatalf("proxy pods = %d, want 1: an unknown count keeps its deadline", got)
	}
	if !containsEvent(drainEvents(rec), "ProxyDrainTimeout") {
		t.Error("no ProxyDrainTimeout event for the proxy deleted at its deadline")
	}
}

func TestMaxStaleSecondsBoundsAProxyDrain(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	surplus := drainOneProxy(t, f, r, func(g *spawneryv1alpha1.ProxyGroup) {
		g.Spec.Update = &spawneryv1alpha1.ProxyUpdateSpec{MaxStaleSeconds: 600}
	})

	f.clock.Advance(11 * time.Minute)
	f.reportProxyPlayers(t, surplus, 3)
	f.reconcileProxyGroup(r, "gateway")
	if got := len(f.proxyPods("gateway")); got != 1 {
		t.Fatalf("proxy pods = %d, want 1: maxStaleSeconds has passed", got)
	}
}

func TestARetiredProxyIsReplacedAndGoesOnceEmpty(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)
	f.createProxyGroup("gateway")
	f.reconcileProxyGroup(r, "gateway")
	pods := f.proxyPods("gateway")
	sortPodsOldestFirst(pods)
	for i := range pods {
		f.markProxyPodReady(t, &pods[i])
		f.reportProxyPlayers(t, pods[i], 2)
	}
	target := pods[0]
	patch := client.MergeFrom(target.DeepCopy())
	target.Annotations = map[string]string{podspec.AnnotationRetireRequested: "2026-09-26T12:00:00Z"}
	if err := f.c.Patch(f.ctx, &target, patch); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	f.reconcileProxyGroup(r, "gateway")
	now := f.proxyPods("gateway")
	if len(now) != 3 {
		t.Fatalf("proxy pods = %d, want 3: a replacement before the retired one drains", len(now))
	}
	for i := range now {
		if now[i].Name != target.Name && now[i].Name != pods[1].Name {
			f.markProxyPodReady(t, &now[i])
			f.reportProxyPlayers(t, now[i], 0)
		}
	}
	f.reconcileProxyGroup(r, "gateway")
	ev := drainEvents(rec)
	if !containsEvent(ev, "ProxyRetiring") || !strings.Contains(strings.Join(ev, "\n"), "retire requested") {
		t.Errorf("events = %v, want ProxyRetiring naming the retire request", ev)
	}

	f.reportProxyPlayers(t, target, 0)
	f.reconcileProxyGroup(r, "gateway")
	for _, p := range f.proxyPods("gateway") {
		if p.Name == target.Name {
			t.Fatal("the retired proxy is still there after it ran empty")
		}
	}
	if !containsEvent(drainEvents(rec), "ProxyStopped") {
		t.Error("no ProxyStopped event for the proxy that ran empty")
	}
}

func TestAProxyThatPassesTheReadyGateIsAnnouncedOnce(t *testing.T) {
	f := newFixture(t)
	r := proxyGroupReconciler(f)
	rec := r.Recorder.(*nonBlockingRecorder)
	f.createProxyGroup("gateway", func(g *spawneryv1alpha1.ProxyGroup) { g.Spec.Replicas = 1 })
	f.reconcileProxyGroup(r, "gateway")
	pod := f.proxyPods("gateway")[0]
	f.markProxyPodReady(t, &pod)

	f.reconcileProxyGroup(r, "gateway")
	f.reconcileProxyGroup(r, "gateway")
	started := 0
	for _, e := range drainEvents(rec) {
		if strings.Contains(e, "ProxyStarted") {
			started++
		}
	}
	if started != 1 {
		t.Errorf("ProxyStarted events = %d, want exactly 1", started)
	}
	if f.proxyPods("gateway")[0].Annotations[podspec.AnnotationProxyReadySince] == "" {
		t.Error("the proxy carries no ready-since")
	}
}
```

If `containsEvent` takes a reason rather than a substring, use it as it is defined in the package; `f.agents.Disconnect`, `f.clock.Advance` and `markProxyPodReady` exist in the fixture (`internal/agent/registry.go:634`, `suite_test.go:149`, `proxygroup_controller_test.go:239`).

- [ ] **Step 2: Run** `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ ./internal/cloudevent/ -run 'Proxy|DecideRollout|Derive|GamePod|MaxStale' -count=1` — Expected: FAIL (compile on `RetireRequested`, then the behaviours).

- [ ] **Step 3: Implement.**
  - `ProxyView` gains `RetireRequested bool`. In the view loop: `requested := pods[i].Annotations[podspec.AnnotationRetireRequested] != ""`; `Stale: ... || nodeGoing[i] || requested`, `RetireRequested: requested`.
  - `pick`'s comparator: after the `Stale` clause, `if a.RetireRequested != b.RetireRequested { return a.RetireRequested }`.
  - In `drainDeparting`'s marking loop, beside the `NodeDraining` event (`going && !wasMarked`), when `!nodeGoing[i]`:

```go
if going && !wasMarked && !nodeGoing[i] {
	why := "scaled down"
	switch {
	case pods[i].Annotations[podspec.AnnotationRetireRequested] != "":
		why = "retire requested"
	case pods[i].Labels[podspec.LabelPodHash] != wantHash:
		why = "rolling update"
	}
	r.Recorder.Eventf(&pods[i], nil, corev1.EventTypeNormal, "ProxyRetiring", actionDrainProxy,
		"proxy %s takes no new connections and stops once empty: %s", pods[i].Name, why)
}
```

  (`wantHash` is passed into `drainDeparting` from `reconcileReplicas`, where it is computed.)
  - Deletion loop:

```go
since, dated := drainingSince(pod)
unknown := !snap.Connected || snap.PlayersStale
expired := dated && (((nodeGoing[i] || unknown) && r.Clock().Sub(since) >= group.DrainTimeout()) ||
	(group.MaxStale() > 0 && r.Clock().Sub(since) >= group.MaxStale()))
```

    and in the `!proxyOccupied(snap)` case set `announce` to a `ProxyStopped` event regarding the pod: `"proxy %s stopped: no players left"`.
  - `announceReady(ctx, pods)`, called in `Reconcile` right after the pods are listed: for each pod with `isPodReady` and no `AnnotationProxyReadySince`, merge-patch the annotation (RFC 3339 of `r.Clock()`), and on success record `ProxyStarted` regarding the pod, `"proxy %s is taking connections"`. A `NotFound` on the patch is skipped silently.
  - `cloudevent.Derive`: a new case

```go
	case *corev1.Pod:
		if o.Labels[podspec.LabelRole] != podspec.RoleProxy {
			return "", nil, false
		}
		namespace, subject, group = o.Namespace, o.Name, o.Labels[podspec.LabelGroup]
```

  - The reconciler's doc comment that lists its event occasions gains the three new ones in one sentence each, and its "Pod creation and ordinary deletion stay silent" sentence goes.

- [ ] **Step 4: Run** the Step 2 command, then the whole controller package `$NIX go -C /home/paul/git/spawnery test ./internal/controller/ -count=1` — Expected: PASS. Existing tests that asserted deletion at `drain.timeoutSeconds` with known players now contradict the spec: change each to use an unknown count or a leaving node, and ledger every one by name.

- [ ] **Step 5: Mutants** in a throwaway worktree: drop `unknown` from `expired` → `TestADrainingProxyWithAnUnknownCountStillHasADeadline` FAILs; drop the `RoleProxy` check in `Derive` → `TestAGamePodIsNotAnEvent` FAILs.

- [ ] **Step 6: Commit** `feat(controller): proxies drain without disconnecting, retire on request, and say so in the feed`.

---

### Task 7: Agents — unretire, proxies, held

**Files:**
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java`
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/ServerInfo.java`
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/ProxyInfo.java`
- Modify: `agent/api/src/test/java/cloud/spawnery/agent/api/FakeApi.java`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/{CloudConnector,MirrorApi,NetworkMirror,CloudCommand}.kt`
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/{CloudCommandTest,MirrorApiTest}.kt`

**Interfaces:**
- Consumes: proto from Task 4.
- Produces: `CompletionStage<Void> unretire(String server)`, `List<ProxyInfo> proxies()`, `Optional<ProxyInfo> proxy(String name)` on `SpawneryApi`; `record ProxyInfo(String name, String group, boolean ready, boolean draining, int players)`; `ServerInfo.held()`.

- [ ] **Step 1: Failing Kotlin tests** — `CloudCommandTest.kt` (its `aNetwork()` gains a retiring server `lobby-r` with `held=false`, a held server `lobby-h`, and two proxies `gateway-a` (ready) and `gateway-b` (draining), group `gateway`):

```kotlin
@Test
fun `unretire asks the operator and says what it means`() {
    run("cloud unretire lobby-r")
    assertEquals("lobby-r", requested.single().unretire.server)
    assertTrue(sent.isEmpty(), "the command answered before the operator did: $sent")
    answer { setUnretire(UnretireResult.newBuilder().setServer("lobby-r")) }
    val line = sent.single()
    assertTrue(line.contains("lobby-r") && line.contains("takes joins again"), line)
}

@Test
fun `unretire says why the operator refused`() {
    run("cloud unretire lobby-r")
    answer { setError(RequestError.newBuilder().setReason(RequestError.Reason.REFUSED).setMessage("that server is already stopping")) }
    assertTrue(sent.single().contains("already stopping"), sent.single())
}

@Test
fun `list shows a proxy group's proxies`() {
    run("cloud list")
    assertTrue(sent.any { it.contains("gateway-a") } && sent.any { it.contains("gateway-b") && it.contains("draining") }, "$sent")
}

@Test
fun `info answers for a proxy`() {
    run("cloud info gateway-b")
    assertTrue(sent.single().contains("gateway-b") && sent.single().contains("draining"), sent.single())
}

@Test
fun `info says a held server is held`() {
    run("cloud info lobby-h")
    assertTrue(sent.single().contains("held"), sent.single())
}
```

`MirrorApiTest.kt`, beside `both sides build the same request for the same retire`:

```kotlin
@Test
fun `both sides build the same request for the same unretire`() {
    val mirror = NetworkMirror().also { it.apply(aRichState()) }
    MirrorApi(mirror, serverSelf(), connector(), CloudEvents()).unretire("lobby-a")
    MirrorApi(mirror, proxySelf(), connector(), CloudEvents()).unretire("lobby-a")

    assertEquals(2, requested.size)
    assertEquals(requested[0].unretire, requested[1].unretire)
    assertEquals("lobby-a", requested[0].unretire.server)
}
```

- [ ] **Step 2: Run** `git -C /home/paul/git/spawnery add -A agent && $NIX make -C /home/paul/git/spawnery agent` — Expected: FAIL (compile: `unretire`, `setUnretire`, `ProxyInfo`).

- [ ] **Step 3: Implement.**
  - `ProxyInfo.java` (Apache header), a record `(String name, String group, boolean ready, boolean draining, int players)` with a class Javadoc: "One proxy of this network, as the operator last described it."
  - `ServerInfo`: new trailing component `boolean held`; a second constructor with the old 10 arguments delegating with `held = false`; Javadoc on the component: "Whether an admin took this server's retirement back: nothing automatic removes it."
  - `SpawneryApi`: after `retire`,

```java
    /**
     * Takes a server's retirement back. It takes joins again and nothing automatic
     * removes it any more; it stays until it ends by itself.
     *
     * <p>Fails for a server that is already stopping or is not retiring, with the
     * operator's reason.
     */
    CompletionStage<Void> unretire(String server);
```

    and after `server(String)`:

```java
    /** Every proxy in this network, in no particular order. */
    List<ProxyInfo> proxies();

    /** One proxy by name, empty if this network has none. */
    Optional<ProxyInfo> proxy(String name);
```

  - `FakeApi`: `unretire` → failed future `UnsupportedOperationException("fake")`; `proxies()` → `List.of()`; `proxy` → `Optional.empty()`.
  - `CloudConnector.unretire(server)`: as `retire`, with `setUnretire(UnretireRequest.newBuilder().setServer(server))`; `answer(...)` gains `response.hasUnretire() -> requests.complete(response.id, null)`.
  - `NetworkMirror`: the snapshot gains `proxies = state.proxiesList.map { ProxyInfo(it.name, it.group, it.ready, it.draining, it.players) }`; `fun proxies(): List<ProxyInfo> = snapshot.proxies`; `ServerInfo(...)` gets `it.held` as its last argument.
  - `MirrorApi`: `override fun unretire(server: String) = connector.unretire(server)`, `override fun proxies() = mirror.proxies()`, `override fun proxy(name: String) = Optional.ofNullable(mirror.proxies().firstOrNull { it.name() == name })`.
  - `CloudCommand`: an `unretire` literal gated by `PERMISSION_RETIRE`, suggesting `api.servers().filter { it.phase() == ServerPhase.RETIRING }.map(ServerInfo::name)`, replying on success `Style.name(name) + Style.good(" takes joins again.") + Style.quiet(" Nothing automatic removes it now; it stays until it ends by itself.")` and on failure `Style.bad("could not unretire") + " " + Style.name(name) + Style.quiet(": ") + Style.bad(reason(failure))`. `retire`'s suggestions add `api.proxies().map(ProxyInfo::name)`, and its comment "Servers only" becomes "Servers and proxies". `list`: under each group of kind `PROXY`, one line per proxy of that group via `describeProxy`. `info`: after the server lookup, `api.proxy(name)` answered with `describeProxy`; suggestions add proxy names. `describeProxy(p)`: name, " in " group, then `ready`/`not ready`, then `draining` if so, then players. `describe(server)` appends `Style.quiet(", ") + Style.bad("held")` when `server.held()`.

- [ ] **Step 4: Run** `git -C /home/paul/git/spawnery add -A agent && $NIX make -C /home/paul/git/spawnery agent` — Expected: PASS, including `PackagingInvariantTest`.

- [ ] **Step 5: Commit** `feat(agent): unretire, proxies and held servers in the API and /cloud`.

---

### Task 8: Docs

**Files:**
- Modify: `docs/guides/cloud-command.md` (the node table: `unretire` beside `retire`; "What each branch does": unretire, retiring a proxy, proxies in list/info)
- Modify: `docs/guides/updates-and-drain.md` (proxy drain waits; `spec.update.maxStaleSeconds` on ProxyGroup; unretire and held servers)
- Modify: `docs/plugin-api/what-a-plugin-can-do.md` (`unretire`, `proxies()`, `ServerInfo.held()`)

- [ ] **Step 1: Write the sections.** In `cloud-command.md`, the table row for `spawnery.cloud.retire` opens `/cloud retire <name>`, `/cloud unretire <name>`, followed by one sentence on why they share a node (the reasoning the guide already gives for `start`/`stop`). The branch section says: unretire takes a retirement back while the server is still retiring and not yet being stopped, and holds it; `retire` takes a proxy name too, and a retired proxy takes no new connections and stops once empty; `list` and `info` show proxies. In `updates-and-drain.md`, a section "Proxies wait for their players too" with the ProxyGroup YAML:

```yaml
kind: ProxyGroup
spec:
  update:
    # Disconnect whoever is left on a draining proxy after this long.
    # 0, the default, means a drain waits until the proxy is empty.
    maxStaleSeconds: 1800
```

  and one paragraph each on the two exceptions (leaving node; unknown player count). A section "Taking a retirement back" on `unretire` and `spec.hold`. In the plugin-api page, `unretire(server)` next to `retire(server)` and `proxies()`/`proxy(name)` next to the server reads.

- [ ] **Step 2: Check** `$NIX make -C /home/paul/git/spawnery docs-length-lint` and, after `git add`, `$NIX make -C /home/paul/git/spawnery docs` — Expected: PASS.

- [ ] **Step 3: Commit** `docs(guides): unretire, held servers, and proxies that wait for their players`.

---

### Task 9: Whole suite

- [ ] **Step 1:** Run both whole-suite commands from Commands and `make agent`. Expected: all green, `git status` clean after `make manifests generate proto`. `internal/podspec/hash_golden_test.go` passes unchanged: nothing here feeds a pod hash.
- [ ] **Step 2:** Ledger the result. Release (a minor step for operator, chart and images) follows the review, on Paul's go.
