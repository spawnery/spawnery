# Milestone 4c-1: the proxy readiness contract — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lowering a `ProxyGroup`'s `spec.replicas` stops disconnecting everyone on the removed proxy: the operator asks it to stop taking new connections, waits for it to empty, and deletes it then.

**Architecture:** One new `OperatorToProxy` message carrying the proxy's *desired* readiness as a state, re-asserted whenever the operator syncs. The Velocity agent maps it onto `ReadyGate.open()`/`close()`, which already exist. The kubelet's probe then drives the pod condition, and the `Service` drops the endpoint — so the routing truth stays in Kubernetes and `internal/agent/registry.go` is not touched at all. `reconcileReplicas` asserts the state, waits for the proxy to report zero players, and deletes at zero or at `spec.drain.timeoutSeconds`.

**Tech Stack:** Go, controller-runtime, protobuf/gRPC, Kotlin (the Velocity agent), Gradle, nix, envtest.

## Global Constraints

- **Design of record:** `docs/superpowers/specs/2026-08-14-proxy-readiness-contract-design.md`. Where this plan and the spec disagree, the spec wins — except where a task says it is correcting the spec, in which case that task amends the spec in the same commit.
- **`internal/agent/registry.go` is not touched.** A proxy's readiness lives in the pod condition, which is what the `Service` obeys. A second copy in the registry is the shape `candidates.go` records as expensive.
- **The wait is for *empty*, not for `NotReady`.** `NotReady` stops the inflow; the players already connected are still there. Deleting at `NotReady` would disconnect exactly the people this milestone exists to protect.
- **Nobody is moved.** A proxy drain has nowhere to move players — the client's connection terminates at the proxy being removed.
- **This is the first change since milestone 3c to touch the proto, the Kotlin agent and the image.** `make agent`, `make agent-test`, `make image-test` and `make image-repro` are back in the path after four operator-only milestones. Expect them to be slow and expect the first run to surface drift.
- **The drain timeout defaults to 300 seconds** when `spec.drain` is absent, and the constant carries the reasoning from spec §3.6.
- **Every test whose expectations move gets its mutation made for real and the output reported.** On the last three milestones, four tests' names claimed something their fixtures no longer measured — including one milestone's own end-to-end test, which passed with the mechanism it existed to prove reverted. Each was caught by running the mutation instead of trusting a green run.
- **Build and test:** `nix develop -c go test ./internal/... -cover` for Go; `nix develop -c make agent` builds the jars; `nix develop -c make proto` regenerates stubs. `make manifests` must produce **no** diff — this milestone adds no CRD field.
- **Commit style:** conventional commits, scope `4c-1`. End every commit message with a blank line and:
  ```
  Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
  ```

## File structure

| File | Responsibility | Task |
|---|---|---|
| `proto/spawnery/agent/v1alpha1/agent.proto` | The `SetReady` message and its oneof slot | 1 |
| `internal/agentpb/`, `agent/proto/` (generated) | Regenerated stubs, Go and Java | 1 |
| `internal/proxyreg/fleet.go` | `SetReady` to one session, and the per-session memo | 2 |
| `agent/velocity/.../ProxyRole.kt` | The `when` branch and the standing-state guard | 3 |
| `api/v1alpha1/proxygroup_types.go` | `DrainTimeout()` and its default | 4 |
| `internal/controller/proxygroup_controller.go` | Asserting readiness and marking the drain (4), waiting and deleting (5) | 4, 5 |
| `cmd/spawnery-stubop/main.go`, `hack/agent-test.sh` | The contract over a real wire | 6 |
| `docs/runbook-milestone-4c1-evidence.md` | The two criteria envtest cannot reach | 7 |
| `docs/known-issues.md`, `docs/handover-milestone-4.md` | Upgrade ordering, and what 4c-2 finds | 8 |

Eight tasks. 1–3 are the contract; 4–5 are the caller; 6 proves it over a wire; 7 and 8 are what a person needs afterwards.

---

### Task 1: The message

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Modify (generated): `internal/agentpb/*`, the Java stubs under the agent's generated source set

**Interfaces:**
- Produces: `agentpb.SetReady{Ready bool}`, `agentpb.OperatorToProxy_SetReady`, and the Kotlin/Java `OperatorToProxy.MessageCase.SET_READY`. Tasks 2, 3 and 6 use all three.

**Why this task is alone.** Regenerating touches two toolchains whose versions are pinned in two places (`flake.nix` and `agent/paper/build.gradle.kts`), and `docs/known-issues.md` records that a `nix flake update` can silently desynchronise them. A regeneration that drifts is worth catching before anything depends on it.

- [ ] **Step 1: Add the message**

In `proto/spawnery/agent/v1alpha1/agent.proto`, after `DrainPlayers`:

```protobuf
// SetReady is the operator asserting whether this proxy should be taking new
// connections. It is a state and not an event, the same way Hello.ready is:
// the operator re-sends it whenever it syncs, so a reconnect cannot leave a
// proxy stuck in the wrong one, and a cancelled drain simply reverts.
//
// The agent maps it onto its readiness gate, which the kubelet probes. So the
// effect an operator is really asking for is "leave the Service's endpoints" —
// established connections are not touched, because Kubernetes does not close
// them when an endpoint is removed.
message SetReady {
  bool ready = 1;
}
```

and add to `OperatorToProxy`'s oneof, after `session_deadline`:

```protobuf
    SetReady set_ready = 7;
```

- [ ] **Step 2: Regenerate**

```bash
nix develop -c make proto
git status --short
```

Expected: only generated files change — the Go package under `internal/agentpb/` and the Java stubs. **If any hand-written file changes, stop and report it.**

- [ ] **Step 3: Confirm both toolchains produced the symbol**

```bash
grep -rn 'SetReady' internal/agentpb/ | head -5
grep -rn 'SET_READY\|setReady' agent/ --include=*.java | head -5
```

Expected: the Go struct and accessor, and the Java `MessageCase.SET_READY` plus a builder. Neither list may be empty.

- [ ] **Step 4: Build both sides**

```bash
nix develop -c go build ./...
nix develop -c make agent
```

Expected: both succeed. `make agent` builds the jars through Gradle and nix and is slow the first time; that is not a hang.

- [ ] **Step 5: Commit**

```bash
git add proto/ internal/agentpb/ agent/
git commit -m "feat(4c-1): the operator can tell a proxy whether it should be ready"
```

---

### Task 2: `Fleet.SetReady`

**Files:**
- Modify: `internal/proxyreg/fleet.go`
- Test: `internal/proxyreg/fleet_test.go`

**Interfaces:**
- Consumes: `agentpb.SetReady` (Task 1).
- Produces: `func (f *Fleet) SetReady(ctx context.Context, podUID string, ready bool) error`. Task 4 calls it.

**Naming.** It must not be called `Drain`. That name is taken in this same file by the *server* drain, which moves players off a backend; two unrelated meanings on one verb in one file is how a reader gets it wrong. `SetReady` also matches the proto message, so the wire name and the call site read the same.

- [ ] **Step 1: Write the failing tests**

```go
func TestSetReadyReachesOneSessionOnly(t *testing.T) {
	f := newTestFleet(t)
	a, stopA := joinTestSession(t, f, "pod-a")
	defer stopA()
	b, stopB := joinTestSession(t, f, "pod-b")
	defer stopB()

	if err := f.SetReady(context.Background(), "pod-a", false); err != nil {
		t.Fatalf("SetReady: %v", err)
	}

	msg := receive(t, a)
	if msg.GetSetReady() == nil {
		t.Fatalf("pod-a got %T, want a SetReady", msg.GetMessage())
	}
	if msg.GetSetReady().GetReady() {
		t.Error("pod-a was told ready=true, want false")
	}
	assertNothingPending(t, b, "pod-b is a different proxy and must not be told anything")
}

func TestSetReadyIsNotRepeatedForTheSameState(t *testing.T) {
	// The operator asserts the desired state on every reconcile, five seconds
	// apart. Without the memo a draining proxy would be told the same thing
	// for the whole of its drain.
	f := newTestFleet(t)
	s, stop := joinTestSession(t, f, "pod-a")
	defer stop()

	for i := 0; i < 3; i++ {
		if err := f.SetReady(context.Background(), "pod-a", false); err != nil {
			t.Fatalf("SetReady %d: %v", i, err)
		}
	}

	if got := drain(t, s); len(got) != 1 {
		t.Errorf("sent %d messages for three identical assertions, want 1", len(got))
	}
}

func TestSetReadyIsReassertedOnANewStream(t *testing.T) {
	// The memo lives on the session, so a reconnect starts without one. That
	// is what makes the state survive a stream that broke mid-drain: the
	// operator's next assertion lands even though the value has not changed.
	f := newTestFleet(t)
	s, stop := joinTestSession(t, f, "pod-a")
	if err := f.SetReady(context.Background(), "pod-a", false); err != nil {
		t.Fatalf("SetReady: %v", err)
	}
	drain(t, s)
	stop()

	s2, stop2 := joinTestSession(t, f, "pod-a")
	defer stop2()
	if err := f.SetReady(context.Background(), "pod-a", false); err != nil {
		t.Fatalf("SetReady after reconnect: %v", err)
	}
	if got := drain(t, s2); len(got) != 1 {
		t.Errorf("the new stream got %d messages, want the state re-asserted once", len(got))
	}
}

func TestSetReadyToAnUnknownPodIsNotAnError(t *testing.T) {
	// A proxy whose stream has gone is a proxy that is not taking connections
	// either. The reconcile that asked must not fail because of it — it will
	// assert again on the next pass if the pod comes back.
	f := newTestFleet(t)
	if err := f.SetReady(context.Background(), "pod-gone", false); err != nil {
		t.Errorf("SetReady to an unknown pod = %v, want nil", err)
	}
}
```

Read the file's existing tests first: `newTestFleet`, `joinTestSession`, `receive`, `drain` and `assertNothingPending` are placeholders for whatever it already provides. Two of this branch's earlier plans named fixture helpers that did not exist, and one named a field that made the test uncompilable — use what is there and write only what is genuinely missing.

- [ ] **Step 2: Run to verify they fail**

```bash
nix develop -c go test ./internal/proxyreg/ -run TestSetReady -v
```

Expected: compile failure, `f.SetReady undefined`.

- [ ] **Step 3: Add the memo to the session**

In `type session struct`, after `closed`:

```go
	// lastReady is the readiness this session was last told to have, and
	// whether it has been told at all. The operator asserts the desired state
	// on every reconcile; without this the same message would go out every
	// five seconds for the whole of a drain.
	//
	// It belongs to the session and not to the Fleet on purpose: a new stream
	// starts without one, so the state is re-asserted on reconnect without the
	// operator having to know a reconnect happened.
	lastReady    bool
	lastReadySet bool
```

- [ ] **Step 4: Add the method**

```go
// SetReady tells one proxy whether it should be taking new connections.
//
// The proxy's readiness is what the kubelet probes and therefore what decides
// whether the Service keeps its endpoint, so this is how the operator takes a
// proxy out of rotation without touching the sessions already on it.
//
// A pod with no live stream is not an error: it is not taking connections
// either, and the next reconcile asserts the state again if it comes back.
func (f *Fleet) SetReady(ctx context.Context, podUID string, ready bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	s, ok := f.sessions[podUID]
	if !ok {
		return nil
	}
	if s.lastReadySet && s.lastReady == ready {
		return nil
	}
	s.lastReady, s.lastReadySet = ready, true
	f.send(s, &agentpb.OperatorToProxy{
		Message: &agentpb.OperatorToProxy_SetReady{
			SetReady: &agentpb.SetReady{Ready: ready},
		},
	})
	return nil
}
```

Check how the file's other senders take the lock — `Register`, `Deregister` and `Drain` are the neighbours — and match them rather than the shape above if they differ.

- [ ] **Step 5: Run to verify they pass**

```bash
nix develop -c go test ./internal/proxyreg/ -cover
```

- [ ] **Step 6: Prove the memo is load-bearing**

Remove the `s.lastReadySet && s.lastReady == ready` early return. `TestSetReadyIsNotRepeatedForTheSameState` must fail. Revert. Report the output.

- [ ] **Step 7: Commit**

```bash
git add internal/proxyreg/
git commit -m "feat(4c-1): tell one proxy whether it should be taking connections"
```

---

### Task 3: The agent obeys it

**Files:**
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/ProxyRole.kt`
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt`
- Test: `agent/velocity/src/test/kotlin/cloud/spawnery/agent/velocity/ProxyRoleTest.kt`

**Interfaces:**
- Consumes: `OperatorToProxy.MessageCase.SET_READY` (Task 1).
- Produces: `ProxyRole`'s constructor gains `onSetReady: (Boolean) -> Unit`. Nothing later depends on it.

**The ordering hazard this task exists to close.** `ProxyRole` opens the gate through `onFirstSync`, guarded by `synced.compareAndSet(false, true)`. If a pod becomes surplus *while it is still starting*, the operator asserts not-ready first and the first `FullSync` arrives afterwards — and `onFirstSync` would open the gate against a standing instruction. The proxy would go `Ready`, the `Service` would add its endpoint, and new players would arrive on a proxy that is being drained.

- [ ] **Step 1: Write the failing tests**

Add to `ProxyRoleTest.kt`, in the style of the tests already there:

```kotlin
@Test
fun `set ready closes and reopens the gate`() {
    val states = mutableListOf<Boolean>()
    val role = newRole(onSetReady = { states += it })

    role.onMessage(setReady(false))
    role.onMessage(setReady(true))

    assertEquals(listOf(false, true), states)
}

@Test
fun `a standing not-ready survives the first sync`() {
    // The pod became surplus while it was still starting: the operator's
    // instruction arrives before the first FullSync. Opening the gate on that
    // sync would put a draining proxy back into the Service's endpoints and
    // send it new players.
    val states = mutableListOf<Boolean>()
    val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

    role.onMessage(setReady(false))
    role.onMessage(fullSync())

    assertEquals(listOf(false), states, "the first sync must not open a gate the operator closed")
}

@Test
fun `the first sync still opens the gate when nothing was asserted`() {
    // The ordinary case, and the guard above must not break it.
    val states = mutableListOf<Boolean>()
    val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

    role.onMessage(fullSync())

    assertEquals(listOf(true), states)
}

@Test
fun `a not-ready after the first sync still closes the gate`() {
    val states = mutableListOf<Boolean>()
    val role = newRole(onFirstSync = { states += true }, onSetReady = { states += it })

    role.onMessage(fullSync())
    role.onMessage(setReady(false))

    assertEquals(listOf(true, false), states)
}
```

`newRole`, `setReady` and `fullSync` are helpers this file may or may not have — read it first and follow its conventions. The file already builds `OperatorToProxy` messages for the other cases, so the builder shape is there to copy.

- [ ] **Step 2: Run to verify they fail**

```bash
nix develop -c ./gradlew -p agent :velocity:test --tests '*ProxyRoleTest*'
```

Check how the repository actually invokes Gradle — read the `Makefile`'s `agent` target — and use that form. Expected: compile failure, no `onSetReady` parameter.

- [ ] **Step 3: Add the parameter and the branch**

In `ProxyRole`'s constructor, beside `onFirstSync`:

```kotlin
    /**
     * Sets the pod's readiness gate. Called for every SetReady the operator
     * sends, which it re-asserts whenever it syncs rather than only on a
     * change — so this may be called with the value it already has.
     */
    private val onSetReady: (Boolean) -> Unit,
```

Add the branch to the `when`, after `DRAIN_PLAYERS`:

```kotlin
            OperatorToProxy.MessageCase.SET_READY -> {
                // Remembered as well as applied: the first FullSync must not
                // open a gate the operator has already closed. A pod that goes
                // surplus while it is still starting gets the instruction
                // before its first sync, and opening on that sync would put a
                // draining proxy back into the Service's endpoints.
                asserted = message.setReady.ready
                onSetReady(message.setReady.ready)
                Directive.None
            }
```

with the field beside `synced`:

```kotlin
    /**
     * The last readiness the operator asserted, or null if it never has.
     * Read by the FullSync branch so a standing instruction wins over the
     * gate-opening that a first sync would otherwise do.
     */
    @Volatile
    private var asserted: Boolean? = null
```

and change the `FULL_SYNC` branch's latch line to respect it:

```kotlin
                if (synced.compareAndSet(false, true) && asserted != false) onFirstSync()
```

- [ ] **Step 4: Wire the plugin**

In `AgentPlugin.kt` where `ProxyRole(` is constructed, pass a lambda that drives the gate the same way `onFirstSync` does:

```kotlin
            onSetReady = { ready -> if (ready) gate.open() else gate.close() },
```

Read the surrounding construction first: `gate` is a local there, and `onFirstSync` already closes over it.

- [ ] **Step 5: Run to verify they pass**

```bash
nix develop -c ./gradlew -p agent :velocity:test
```

- [ ] **Step 6: Prove the ordering guard is load-bearing**

Change the latch line back to `if (synced.compareAndSet(false, true)) onFirstSync()`. `a standing not-ready survives the first sync` must fail. Revert. Report the output.

- [ ] **Step 7: Commit**

```bash
git add agent/velocity/
git commit -m "feat(4c-1): a proxy closes its gate when the operator says so"
```

---

### Task 4: The operator asserts the state

**Files:**
- Modify: `api/v1alpha1/proxygroup_types.go`
- Modify: `internal/controller/proxygroup_controller.go`
- Test: `internal/controller/proxygroup_controller_test.go`

**Interfaces:**
- Consumes: `Fleet.SetReady` (Task 2).
- Produces: `(*ProxyGroup).DrainTimeout() time.Duration`, the annotation constant `ProxyDrainingSinceAnnotation = "spawnery.cloud/draining-since"`. Task 5 uses both.

- [ ] **Step 1: Write the failing tests**

```go
func TestSurplusProxyIsToldToStopTakingConnections(t *testing.T) {
	f := newProxyFixture(t)
	f.setReplicas(t, 2)
	f.reconcile(t)
	pods := f.pods(t)
	if len(pods) != 2 {
		t.Fatalf("got %d pods, want 2", len(pods))
	}

	f.setReplicas(t, 1)
	f.reconcile(t)

	surplus := pods[len(pods)-1]
	if got := f.fleet.lastReady(string(surplus.UID)); got == nil || *got {
		t.Errorf("the surplus proxy was told ready=%v, want false", got)
	}
	if got := f.fleet.lastReady(string(pods[0].UID)); got == nil || !*got {
		t.Errorf("the surviving proxy was told ready=%v, want true", got)
	}
}

func TestSurplusProxyIsMarkedWithWhenTheDrainStarted(t *testing.T) {
	f := newProxyFixture(t)
	f.setReplicas(t, 2)
	f.reconcile(t)
	f.setReplicas(t, 1)
	f.reconcile(t)

	surplus := f.pods(t)[1]
	at, ok := surplus.Annotations[ProxyDrainingSinceAnnotation]
	if !ok {
		t.Fatal("the surplus proxy carries no draining-since annotation; the deadline has nothing to run from")
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("draining-since = %q, want RFC 3339: %v", at, err)
	}
}

func TestTheMarkIsNotMovedOnLaterPasses(t *testing.T) {
	// The deadline runs from the first assertion. Re-stamping it every five
	// seconds would push the deadline forever and the drain would never end.
	f := newProxyFixture(t)
	f.setReplicas(t, 2)
	f.reconcile(t)
	f.setReplicas(t, 1)
	f.reconcile(t)
	first := f.pods(t)[1].Annotations[ProxyDrainingSinceAnnotation]

	f.clock.Advance(time.Minute)
	f.reconcile(t)

	if got := f.pods(t)[1].Annotations[ProxyDrainingSinceAnnotation]; got != first {
		t.Errorf("draining-since moved from %q to %q", first, got)
	}
}

func TestACancelledScaleDownPutsTheProxyBack(t *testing.T) {
	f := newProxyFixture(t)
	f.setReplicas(t, 2)
	f.reconcile(t)
	f.setReplicas(t, 1)
	f.reconcile(t)

	f.setReplicas(t, 2)
	f.reconcile(t)

	pod := f.pods(t)[1]
	if got := f.fleet.lastReady(string(pod.UID)); got == nil || !*got {
		t.Errorf("the proxy was told ready=%v after the scale-down was cancelled, want true", got)
	}
	if _, ok := pod.Annotations[ProxyDrainingSinceAnnotation]; ok {
		t.Error("the draining-since annotation outlived the drain")
	}
}

func TestDrainTimeoutDefaultsWhenTheFieldIsAbsent(t *testing.T) {
	g := &spawneryv1alpha1.ProxyGroup{}
	if got := g.DrainTimeout(); got != defaultProxyDrainTimeout {
		t.Errorf("DrainTimeout() = %v with no spec.drain, want %v", got, defaultProxyDrainTimeout)
	}
}
```

`newProxyFixture`, `f.fleet.lastReady` and `f.clock` are placeholders. Read `proxygroup_controller_test.go` and `suite_test.go` first. The fixture will need a `Fleet` test double that records the last readiness per pod UID; if none exists, write the smallest one beside the tests.

- [ ] **Step 2: Run to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestSurplusProxy|TestTheMarkIs|TestACancelled|TestDrainTimeoutDefaults' -v
```

Expected: compile failure — no `DrainTimeout`, no annotation constant.

- [ ] **Step 3: Add the accessor and the constant**

In `api/v1alpha1/proxygroup_types.go`:

```go
// defaultProxyDrainTimeout is how long a proxy may take to empty before it is
// removed anyway.
//
// Five minutes rather than the sixty seconds a ServerGroup uses, because the
// two waits are not the same wait. A server drain moves its players to another
// backend, which is quick; a proxy drain has nowhere to move them — the
// client's connection terminates at the proxy being removed — so this waits
// for people to leave on their own. There is no honest default: a play session
// runs to tens of minutes, so every number short of that disconnects somebody.
// Five minutes lets a scale-down in a quiet period finish without kicks while
// still bounding a deploy. An operator who cares about this should set
// spec.drain.timeoutSeconds.
const defaultProxyDrainTimeout = 300 * time.Second

// DrainTimeout is how long existing sessions may run out before a proxy being
// removed is deleted anyway.
func (g *ProxyGroup) DrainTimeout() time.Duration {
	if g.Spec.Drain == nil || g.Spec.Drain.TimeoutSeconds < 1 {
		return defaultProxyDrainTimeout
	}
	return time.Duration(g.Spec.Drain.TimeoutSeconds) * time.Second
}
```

In `internal/controller/proxygroup_controller.go`:

```go
// ProxyDrainingSinceAnnotation records when the operator first asked a proxy
// pod to stop taking connections, as an RFC 3339 timestamp.
//
// It is on the pod because that is the only per-pod place that survives an
// operator restart: a proxy has no CR of its own, and the ProxyGroup's status
// is per group. Everything else about a drain is re-derived every pass — which
// pods are surplus, and therefore what readiness each should have — so this is
// the only thing that has to be written down.
const ProxyDrainingSinceAnnotation = "spawnery.cloud/draining-since"
```

- [ ] **Step 4: Assert the state and mark the drain**

In `reconcileReplicas`, add the assertion loop **before** the existing deletion loop, and leave both the create loop and the deletion loop exactly as they are. Task 5 changes when deletion happens; this task only adds what the operator says and writes down.

Leaving the immediate deletion in place for one task is deliberate. Removing it here would leave a commit where a `ProxyGroup` cannot scale down at all — green, because no test asserts the old behaviour, and behaviourally broken, which is worse than a failing build because nothing announces it.

```go
	// The desired readiness is derived, not remembered: this loop already
	// knows which pods are surplus, so it asserts the answer for every pod on
	// every pass. An operator restart recomputes the same thing, and a
	// cancelled scale-down corrects itself without anything to clean up.
	for i := range pods {
		surplus := i >= int(group.Spec.Replicas)
		if err := r.Proxies.SetReady(ctx, string(pods[i].UID), !surplus); err != nil {
			return err
		}
		if err := r.markDraining(ctx, &pods[i], surplus); err != nil {
			return err
		}
	}
```

and add:

```go
// markDraining writes or removes the annotation that dates a proxy's drain.
//
// Written once and never moved: the deadline runs from the first assertion,
// and re-stamping it on every five-second pass would push it forever and the
// drain would never end.
func (r *ProxyGroupReconciler) markDraining(ctx context.Context, pod *corev1.Pod, draining bool) error {
	_, marked := pod.Annotations[ProxyDrainingSinceAnnotation]
	switch {
	case draining && !marked:
		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[ProxyDrainingSinceAnnotation] = r.Clock().UTC().Format(time.RFC3339)
		return r.Patch(ctx, pod, patch)
	case !draining && marked:
		patch := client.MergeFrom(pod.DeepCopy())
		delete(pod.Annotations, ProxyDrainingSinceAnnotation)
		return r.Patch(ctx, pod, patch)
	}
	return nil
}
```

If `ProxyGroupReconciler` has no `Clock` or no `Proxies` field, add them following `ServerGroupReconciler`'s, and wire them in `SetupWithManager` and wherever the reconcilers are constructed.

- [ ] **Step 5: Run to verify they pass**

```bash
nix develop -c go test ./internal/controller/ -cover
```

Expected: PASS. **A surplus pod is no longer deleted at all after this task** — that is Task 5's, and any pre-existing test asserting the old immediate deletion will fail here. **If one does, stop and tell me before changing it.**

- [ ] **Step 6: Prove the mark is written once**

Change `markDraining` to stamp unconditionally when draining. `TestTheMarkIsNotMovedOnLaterPasses` must fail. Revert. Report the output.

- [ ] **Step 7: Commit**

```bash
git add api/v1alpha1/ internal/controller/
git commit -m "feat(4c-1): ask a surplus proxy to stop taking connections, and date it"
```

---

### Task 5: The wait, and the deletion

**Files:**
- Modify: `internal/controller/proxygroup_controller.go`
- Test: `internal/controller/proxygroup_controller_test.go`

**Interfaces:**
- Consumes: `DrainTimeout()`, `ProxyDrainingSinceAnnotation` (Task 4).
- Produces: nothing further.

- [ ] **Step 1: Write the failing tests**

```go
func TestADrainingProxyWithPlayersIsNotDeleted(t *testing.T) {
	// The promise of the milestone. NotReady stops the inflow; it says nothing
	// about the three people already connected.
	f := newProxyFixture(t)
	f.setReplicas(t, 2)
	f.reconcile(t)
	surplus := f.pods(t)[1]
	f.reportPlayers(t, surplus, 3)

	f.setReplicas(t, 1)
	f.reconcile(t)
	f.clock.Advance(resyncInterval)
	f.reconcile(t)

	if len(f.pods(t)) != 2 {
		t.Error("the draining proxy was deleted with three players on it")
	}
}

func TestADrainingProxyIsDeletedOnceEmpty(t *testing.T) {
	f := newProxyFixture(t)
	f.setReplicas(t, 2)
	f.reconcile(t)
	surplus := f.pods(t)[1]
	f.reportPlayers(t, surplus, 3)
	f.setReplicas(t, 1)
	f.reconcile(t)

	f.reportPlayers(t, surplus, 0)
	f.reconcile(t)

	if got := len(f.pods(t)); got != 1 {
		t.Errorf("got %d pods after the proxy emptied, want 1", got)
	}
}

func TestTheDeadlineDeletesLoudly(t *testing.T) {
	f := newProxyFixture(t)
	f.setReplicas(t, 2)
	f.reconcile(t)
	surplus := f.pods(t)[1]
	f.reportPlayers(t, surplus, 3)
	f.setReplicas(t, 1)
	f.reconcile(t)

	f.clock.Advance(f.group.DrainTimeout() + time.Second)
	f.reconcile(t)

	if got := len(f.pods(t)); got != 1 {
		t.Fatalf("got %d pods after the deadline, want 1", got)
	}
	ev := f.events(t)
	if !containsEvent(ev, "ProxyDrainTimeout") {
		t.Fatalf("events = %v, want a ProxyDrainTimeout", ev)
	}
	if !containsSubstring(ev, "3 player") {
		t.Errorf("events = %v, want the number of players lost named", ev)
	}
}
```

Placeholders again — `f.reportPlayers`, `f.events`, `containsEvent`, `containsSubstring`. Read the file first; the `ServerGroup` tests have event helpers to copy.

- [ ] **Step 2: Run to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestADraining|TestTheDeadline' -v
```

Expected: all three fail, and for different reasons worth telling apart in the report. The first two fail because Task 4 left the immediate deletion in place, so the surplus pod is gone on the first pass whether or not anyone is on it. The third fails because no code path emits the event yet.

- [ ] **Step 3: Delete when empty or when the deadline passes**

In `reconcileReplicas`, replace the existing deletion loop — the one Task 4 deliberately left alone — with this:

```go
	for i := len(pods) - 1; i >= int(group.Spec.Replicas); i-- {
		pod := &pods[i]
		players := r.Agents.Lookup(string(pod.UID)).Players
		since, err := drainingSince(pod)
		if err != nil {
			return err
		}
		expired := !since.IsZero() && r.Clock().Sub(since) >= group.DrainTimeout()

		switch {
		case players == 0:
			// Empty: nobody is on it, so removing it costs nothing.
		case expired:
			// The one path in this milestone that disconnects anybody. It is
			// configured rather than accidental, so it says so loudly and
			// names the cost.
			r.Recorder.Eventf(group, corev1.EventTypeWarning, "ProxyDrainTimeout",
				"deleting proxy %s after %s with %d player(s) still connected",
				pod.Name, group.DrainTimeout(), players)
		default:
			// Still draining. Nothing to do; the next pass looks again.
			continue
		}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
```

and:

```go
// drainingSince reads the annotation Task 4 writes. A pod without one has not
// been asked to drain yet, which reads as the zero time and therefore as no
// deadline.
func drainingSince(pod *corev1.Pod) (time.Time, error) {
	raw, ok := pod.Annotations[ProxyDrainingSinceAnnotation]
	if !ok {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("proxy %s has an unparsable %s: %w",
			pod.Name, ProxyDrainingSinceAnnotation, err)
	}
	return at, nil
}
```

- [ ] **Step 4: Run to verify they pass**

```bash
nix develop -c go test ./internal/controller/ -cover
```

- [ ] **Step 5: Prove both halves**

1. Change `case players == 0:` to `case true:` so it deletes regardless. `TestADrainingProxyWithPlayersIsNotDeleted` must fail. Revert.
2. Change `expired` to always false. `TestTheDeadlineDeletesLoudly` must fail. Revert.

Report both outputs.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/
git commit -m "feat(4c-1): wait for a proxy to empty before removing it"
```

---

### Task 6: The contract over a real wire

**Files:**
- Modify: `cmd/spawnery-stubop/main.go`
- Modify: `hack/agent-test.sh`

**Interfaces:** none new.

**Why this task.** Everything so far is tested on one side of the wire or the other. `make agent-test` runs the real jar in the real image against a stub operator, and phase 4 is already called "the proxy, and the gate that must not open early" — it exists to prove exactly this kind of claim. Extending it is how the contract gets exercised end to end without a cluster.

- [ ] **Step 1: Teach the stub operator to send it**

Add a flag beside `--proxy`, in that file's own voice — read the `--proxy` and `--full-sync-after` flags first and match their documentation style:

```go
	setReadyAfter := fs.Duration("set-ready-after", 0,
		"after a proxy's FullSync, wait this long and then tell it to stop being ready; "+
			"0 disables. Used by hack/agent-test.sh phase 4 to prove the gate closes on the "+
			"operator's word rather than only at shutdown")
```

and, on a proxy stream after the `FullSync`, send `OperatorToProxy_SetReady{Ready:false}` once the delay elapses. Emit an event line for it the way the file does for its other actions, so the shell can assert on it.

- [ ] **Step 2: Extend phase 4**

`hack/agent-test.sh` phase 4 already starts a proxy, waits for the gate to open, and probes it. Add to the end of that phase: run the stub with `--set-ready-after`, wait, and probe the gate again — it must now refuse the connection. Then assert on the stub's event stream that the message was sent and on the probe that the port closed.

Follow the phase's existing structure exactly, including how it probes the port and how it reports a failure; the surrounding phases show the idiom.

- [ ] **Step 3: Run it**

```bash
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make agent-test CONTAINER=podman
```

This builds both images and runs five phases; it is slow. **If `CONTAINER=podman` or `TMPDIR` turn out not to be needed on this machine, drop them** — `docs/runbook-milestone-3-evidence.md` §0 records why they were needed on the machine that wrote it, and `docs/known-issues.md` records that the Makefile defaults to `docker`.

Expected: all five phases pass, phase 4 now covering the close.

- [ ] **Step 4: Prove the new assertion can fail**

Run the phase against an agent built without Task 3's `when` branch — the simplest way is to comment out the branch, rebuild with `make agent`, and run phase 4. It must fail on the second probe. Restore and re-run. Report the output.

- [ ] **Step 5: Check the image still holds**

```bash
nix develop -c make image-test CONTAINER=podman
nix develop -c make image-repro
```

Expected: both green. These have not run since milestone 3c; if either fails for a reason unrelated to this change, **stop and report it** rather than fixing it here.

- [ ] **Step 6: Commit**

```bash
git add cmd/spawnery-stubop/ hack/
git commit -m "test(4c-1): prove the gate closes on the operator's word, over the wire"
```

---

### Task 7: The runbook

**Files:**
- Create: `docs/runbook-milestone-4c1-evidence.md`

**Interfaces:** none.

**Why.** Two acceptance criteria are claims about a real cluster: that the removed proxy leaves the `Service`'s endpoints, and that an established player connection survives it. envtest has no kubelet, no probes and no kube-proxy, so neither can be measured there. Milestone 3 met the same wall and answered it with a runbook plus a driven session; `docs/runbook-milestone-3-evidence.md` is the shape to follow, including its habit of saying what to expect at each step and treating a deviation as a defect rather than a documentation problem.

- [ ] **Step 1: Write it**

It must carry, at minimum:

- the prerequisites, by reference to the milestone-3 runbook's §0 rather than by repeating them — that file already records the `CONTAINER=podman`, `TMPDIR` and D-Bus requirements and why;
- building and loading both images, and a `kind` cluster with the NodePort published;
- a `ProxyGroup` at `replicas: 2` with both proxies ready;
- how to see which proxy a client landed on, and how to read the `Service`'s endpoints (`kubectl get endpoints` or `endpointslices`, whichever the cluster's version uses — say which and why);
- joining with a real client, then lowering `replicas` to 1 **while that client is connected to the proxy that will be removed** — including how to force that, since the selection is from the end of the pod list and the client may have landed on the other one;
- the measurements: the endpoint disappears, the client stays connected and playing, the pod is deleted only after the client leaves;
- the deadline case, run deliberately: set `spec.drain.timeoutSeconds` low, repeat, and confirm the client *is* disconnected and the `Warning` event names the count. **This is the only path that disconnects anyone and it should be seen once on purpose rather than met in production.**

- [ ] **Step 2: Commit**

```bash
git add docs/runbook-milestone-4c1-evidence.md
git commit -m "docs(4c-1): a runbook for the two claims envtest cannot make"
```

---

### Task 8: The paperwork

**Files:**
- Modify: `docs/known-issues.md`
- Modify: `docs/handover-milestone-4.md`

**Interfaces:** none.

- [ ] **Step 1: Record the upgrade ordering**

`docs/known-issues.md` gains a "From milestone 4c-1" entry: an agent that predates `SetReady` ignores the unknown `oneof` field, so its gate never closes, the pod stays `Ready`, and the drain runs to its deadline and disconnects whoever is on it. The deadline bounds it and the `Warning` event names it, but **images must be upgraded before the operator** until something version-gates the message. Say what an operator would see, not only what is true.

- [ ] **Step 2: Record what 4c-2 and 4c-3 find**

`docs/handover-milestone-4.md` gains a 4c-1 section in the shape its 4a, 4b and 4d sections use: what landed, and what the next pieces find in place. Cover the message and that it is a state rather than an event; that the registry was deliberately not touched and why the handover's own prediction there was wrong; the annotation and why it is on the pod; that `Fleet.SetReady` is per-session and re-asserts on reconnect; and that 4c-2 still owns choosing which proxy goes, surge, one-at-a-time replacement and `pods()`'s missing expectations, while 4c-3 owns node drain and depends on none of it.

- [ ] **Step 3: Commit**

```bash
git add docs/
git commit -m "docs(4c-1): record the upgrade ordering and what 4c-2 finds"
```

---

## Before the branch is merged

- [ ] `nix develop -c make test` — green.
- [ ] `nix develop -c make agent-test` — five phases green.
- [ ] `nix develop -c make image-test` and `make image-repro` — green.

  Add `CONTAINER=podman` to the two container targets only where the Makefile's
  `docker` default is not already Podman, and a disk-backed `TMPDIR` only where
  `/tmp` is a tmpfs. On the machine that ran this milestone neither applies —
  `docker` is a symlink into `podman-docker-compat` and `/tmp` is part of the
  root filesystem — and all four targets pass with plain defaults (measured
  2026-08-14). `docs/runbook-milestone-3-evidence.md` §0 states both conditions.
- [ ] `nix develop -c make manifests` — **no diff**; this milestone adds no CRD field.
- [ ] `git diff --name-only master...HEAD` — the proto, its generated stubs, `internal/proxyreg`, `internal/controller`, `api/v1alpha1`, `agent/velocity`, `cmd/spawnery-stubop`, `hack/` and `docs/`. Nothing else.
- [ ] One whole-branch review before merge. On the last three milestones this review found what no task-scoped review could: a ceiling never enforced on a group that was also short, a fixed point where a refused create suppressed the branch that would have freed its room, and a give-up test that could not fail. The equivalent risk here is the interaction between the assertion loop, the annotation and the deletion loop — no single task exercises all three against a pod whose stream is flapping.
- [ ] The evidence run from Task 7, driven with a real client.

## Self-review

**Spec coverage.** §3.1 → Task 4's assertion is per-pod through `Fleet`, and no task touches `registry.go`. §3.2 → Tasks 1 and 3. §3.3 → Task 4's derive-every-pass loop and Task 2's session memo. §3.4 → Task 4's annotation. §3.5 → Task 5. §3.6 → Task 4's `DrainTimeout`. §3.7 → nothing, correctly: the selection is unchanged and no task touches it. §4.1–§4.4 → Tasks 1, 2, 3, 4, 5. §4.5 → nothing, correctly. §5 → Task 5's tests plus Task 7's runbook. §6 → Task 4's cancelled-scale-down test, Task 5's deadline test, Task 8's upgrade-ordering entry. §9 → each task's tests plus Task 6. §10 → the pre-merge checklist; criteria 1 and 2 are Task 7's.

**One acceptance criterion no task proves directly.** Criterion 3 — that the desired state is re-asserted after a reconnect — is proved at the `Fleet` level by Task 2 and at the agent level by Task 3, but nothing exercises a real reconnect end to end. `make agent-test` phase 1 already proves the reconnect itself works for a server; extending it to assert readiness across a proxy reconnect would be the stronger test and is not in this plan. Written down rather than dropped, so the whole-branch review can decide.

**Placeholders:** none. Every step carries its code. Tasks 2, 3, 4 and 5 name fixture helpers that may not exist and say to read the file first — three earlier plans on this project named helpers or fields that did not, and one of those tests could not have compiled.

**Type consistency:** `Fleet.SetReady(ctx, podUID string, ready bool) error` is defined in Task 2 and called in Task 4. `ProxyDrainingSinceAnnotation` and `DrainTimeout()` are defined in Task 4 and consumed in Task 5. `onSetReady: (Boolean) -> Unit` is added to `ProxyRole` in Task 3 and passed in the same task. `agentpb.SetReady` and `OperatorToProxy.MessageCase.SET_READY` come from Task 1 and are used by name in Tasks 2, 3 and 6.
