# Milestone 4d: per-group backoff — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `ServerGroup` whose servers cannot start stops creating replacements every five seconds: it waits longer after each failure, and after six in a row it gives up and says so.

**Architecture:** Two pure functions — one folds this pass's `Failed` servers into a running count, one turns that count into "may create / must wait / has given up". Both are table-tested without a cluster. The count lives on the group's status so an operator restart cannot reset it. The gate sits on *execution*: `DecideSize` is untouched and keeps computing what the group needs; `size()` simply does not carry out the creates while a window is open. Deletions, retirements and drains are never gated.

**Tech Stack:** Go, controller-runtime, kubebuilder CRDs, envtest. No proto, no agent, no image work.

## Global Constraints

- **Design of record:** `docs/superpowers/specs/2026-08-13-per-group-backoff-design.md`. Where this plan and the spec disagree, the spec wins — except where a task says it is correcting the spec, in which case that task amends the spec in the same commit.
- **The backoff gates creation only.** Deletions, retirements and drains run regardless. Those paths touch players and must not wait on an unrelated failure.
- **`DecideSize` is not touched** except by Task 6, which removes one term from `coldStart`. The capacity arithmetic stays generation-blind; `provisionalCapacity`, `readyContribution` and `readyFree` must not read the generation.
- **The load-bearing invariant:** a server that may be carrying players is never nominated for deletion. Nothing here goes near that path, and nothing may weaken it.
- **`internal/phase` is at 100% coverage and must stay there.** No task in this plan changes that package. `internal/controller` is at about 88.6% and must not fall below 88%. It varies by run — 88.4 to 89.3 measured on identical code — so do not chase a number.
- **Operator-only Go.** `git diff --name-only` must touch nothing under `agent/`, `image/`, `proto/`, `nix/`.
- **Every test whose expectations move gets its mutation made for real and the output reported.** On milestone 4b three tests' names claimed something their fixtures no longer measured, including that milestone's own end-to-end test, which passed with the mechanism it existed to prove reverted. Each was caught by running the mutation instead of trusting a green run.
- **Build and test:** `nix develop -c go test ./internal/controller/ -cover` (about 30 seconds; it boots envtest, which is not a hang). `nix develop -c make manifests` must produce no diff except where a task says it will.
- **Commit style:** conventional commits, scope `4d`. End every commit message with a blank line and:
  ```
  Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
  ```

## File structure

| File | Responsibility | Task |
|---|---|---|
| `internal/controller/backoff.go` | The four constants and the two pure rules | 1 |
| `internal/controller/backoff_test.go` | Their table tests | 1 |
| `api/v1alpha1/common_types.go` | `ConditionBackingOff`, `ReasonNoRecentFailures` | 2 |
| `api/v1alpha1/servergroup_types.go` | `status.consecutiveFailures`, `status.lastFailureAt` | 2 |
| `internal/controller/candidates.go` | `ServerView.FailedAt`, `ServerView.ReadySince` | 3 |
| `internal/controller/servergroup_controller.go` | Reading them; the generation clear, the count, the gate (4); the two conditions (5) | 3, 4, 5 |
| `internal/controller/scaling.go` | Removing 4b's cold-start suppression | 6 |
| `docs/` | 4b's spec correction, the closed known-issues entry | 7 |

Seven tasks. 1–3 are independent foundations; 4 and 5 wire them; 6 retires the stopgap this milestone replaces; 7 is the paperwork.

---

### Task 1: The two pure rules

**Files:**
- Create: `internal/controller/backoff.go`
- Test: `internal/controller/backoff_test.go`

**Interfaces:**
- Consumes: `ServerView` (existing), `phase.Failed`.
- Produces: `CountFailures(views []ServerView, prev int32, since time.Time) (int32, time.Time)`, `BackoffInputs`, `BackoffDecision`, `DecideBackoff(BackoffInputs) BackoffDecision`, and the constants `backoffBase`, `backoffFactor`, `backoffCap`, `backoffGiveUpAt`. Tasks 4 and 5 use all of these by name.

**Note:** this task reads `ServerView.FailedAt` and `ServerView.ReadySince`, which Task 3 adds. Add the two fields as part of this task if they are not there yet — a bare `FailedAt time.Time` and `ReadySince time.Time` with the doc comments Task 3 specifies — and let Task 3 populate them. Do not populate them here.

- [ ] **Step 1: Write the failing tests**

```go
package controller

import (
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// failedAt builds a Failed server that failed at t.
func failedAt(name string, t time.Time) ServerView {
	return ServerView{Name: name, Phase: phase.Failed, FailedAt: t}
}

// readyAt builds a Ready server that became ready at t.
func readyAt(name string, t time.Time) ServerView {
	return ServerView{Name: name, Phase: phase.Ready, ReadySince: t, Slots: 100}
}

func TestCountFailuresCountsANewCorpseOnce(t *testing.T) {
	base := time.Now()
	views := []ServerView{failedAt("a", base)}

	got, newest := CountFailures(views, 0, time.Time{})
	if got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
	if !newest.Equal(base) {
		t.Errorf("newest = %v, want %v", newest, base)
	}

	// The same corpse on the next pass. Without the FailedAt > since test this
	// would climb by one every five-second resync forever, which is the whole
	// reason a counter can survive at all.
	got, _ = CountFailures(views, got, newest)
	if got != 1 {
		t.Errorf("count = %d after re-observing the same corpse, want 1", got)
	}
}

func TestCountFailuresCountsTwoInOnePass(t *testing.T) {
	base := time.Now()
	views := []ServerView{failedAt("a", base), failedAt("b", base.Add(time.Second))}

	got, newest := CountFailures(views, 0, time.Time{})
	if got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
	if !newest.Equal(base.Add(time.Second)) {
		t.Error("newest is not the newer of the two failures")
	}
}

func TestCountFailuresResetsOnASuccessAfterTheLastFailure(t *testing.T) {
	base := time.Now()
	// Three failures already counted, then a server came up.
	views := []ServerView{readyAt("b", base.Add(time.Minute))}

	got, _ := CountFailures(views, 3, base)
	if got != 0 {
		t.Errorf("count = %d, want 0: a success since the last failure breaks the streak", got)
	}
}

func TestCountFailuresIgnoresASuccessOlderThanTheLastFailure(t *testing.T) {
	// The rule that a plausible implementation gets wrong. "Any server is
	// Ready" would hold the counter at zero forever for a group with one
	// healthy server and one that crash-loops — a bad node, or a resource
	// request only some nodes satisfy — and the group would hammer
	// indefinitely. The success has to be *since* the failure.
	base := time.Now()
	views := []ServerView{
		readyAt("healthy", base.Add(-time.Hour)),
		failedAt("broken", base.Add(time.Second)),
	}

	got, _ := CountFailures(views, 3, base)
	if got != 4 {
		t.Errorf("count = %d, want 4: the healthy server predates the streak and does not break it", got)
	}
}

func TestCountFailuresStartsAFreshStreakAfterASuccess(t *testing.T) {
	// A success breaks the old streak, and a failure after that success starts
	// a new one at 1 rather than continuing the old count.
	base := time.Now()
	views := []ServerView{
		readyAt("recovered", base.Add(time.Minute)),
		failedAt("next", base.Add(2*time.Minute)),
	}

	got, newest := CountFailures(views, 3, base)
	if got != 1 {
		t.Errorf("count = %d, want 1: the success ended the old streak and the later failure begins a new one", got)
	}
	if !newest.Equal(base.Add(2 * time.Minute)) {
		t.Error("newest is not the failure that started the new streak")
	}
}

func TestDecideBackoffLetsTheFirstAttemptThrough(t *testing.T) {
	got := DecideBackoff(BackoffInputs{ConsecutiveFailures: 0, Now: time.Now()})
	if !got.MayCreate {
		t.Error("MayCreate = false with no failures; the first attempt has no window")
	}
	if got.GaveUp {
		t.Error("GaveUp = true with no failures")
	}
}

func TestDecideBackoffWaitsAndThenAllows(t *testing.T) {
	failed := time.Now()

	// One failure: a ten-second window.
	got := DecideBackoff(BackoffInputs{
		ConsecutiveFailures: 1, LastFailureAt: failed, Now: failed.Add(9 * time.Second),
	})
	if got.MayCreate {
		t.Error("MayCreate = true nine seconds into a ten-second window")
	}
	if got.RetryAfter != time.Second {
		t.Errorf("RetryAfter = %v, want 1s", got.RetryAfter)
	}

	got = DecideBackoff(BackoffInputs{
		ConsecutiveFailures: 1, LastFailureAt: failed, Now: failed.Add(10 * time.Second),
	})
	if !got.MayCreate {
		t.Error("MayCreate = false exactly at the end of the window")
	}
}

func TestDecideBackoffDoubles(t *testing.T) {
	failed := time.Now()
	for _, tc := range []struct {
		failures int32
		want     time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 80 * time.Second},
		{5, 160 * time.Second},
	} {
		got := DecideBackoff(BackoffInputs{
			ConsecutiveFailures: tc.failures, LastFailureAt: failed, Now: failed,
		})
		if got.RetryAfter != tc.want {
			t.Errorf("after %d failures RetryAfter = %v, want %v", tc.failures, got.RetryAfter, tc.want)
		}
	}
}

func TestDecideBackoffGivesUpAtTheThreshold(t *testing.T) {
	failed := time.Now()
	got := DecideBackoff(BackoffInputs{
		ConsecutiveFailures: backoffGiveUpAt, LastFailureAt: failed, Now: failed.Add(time.Hour),
	})
	if !got.GaveUp {
		t.Errorf("GaveUp = false at %d failures", backoffGiveUpAt)
	}
	if got.MayCreate {
		t.Error("MayCreate = true after giving up; an elapsed window must not resurrect it")
	}
}

func TestBackoffDelayIsCapped(t *testing.T) {
	// The cap is not reached at the shipped threshold — the largest delay
	// before giving up is 160s, well under five minutes. It exists so that
	// raising backoffGiveUpAt, the one of these four numbers somebody might
	// plausibly want larger, cannot turn the doubling into an unbounded wait.
	// So this case has to construct a count past the threshold rather than
	// assert the cap against the default, which would never reach it.
	if got := backoffDelay(20); got != backoffCap {
		t.Errorf("backoffDelay(20) = %v, want the cap %v", got, backoffCap)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestCountFailures|TestDecideBackoff|TestBackoffDelay' -v
```

Expected: compile failure — `undefined: CountFailures`, `undefined: DecideBackoff`, `unknown field FailedAt in struct literal`.

- [ ] **Step 3: Write `internal/controller/backoff.go`**

```go
package controller

import (
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// The backoff's four numbers. They are constants rather than CRD fields
// because the master design does not ask for configurability, nobody has
// asked for it, and a knob nobody turns is a knob somebody turns wrongly.
// Adding a field later is cheap; removing one is not.
const (
	// backoffBase is the wait after the first failure. Short enough that a
	// single unlucky start costs the group seconds, not minutes.
	backoffBase = 10 * time.Second
	// backoffFactor is how much each further failure multiplies the wait.
	backoffFactor = 2
	// backoffCap bounds the doubling. It is NOT reached at backoffGiveUpAt:
	// the largest wait before the group gives up is 160s. It exists so that
	// raising backoffGiveUpAt — the one of these numbers somebody might
	// plausibly want larger — cannot produce an unbounded wait.
	backoffCap = 5 * time.Minute
	// backoffGiveUpAt is how many consecutive failures end the attempts. Six
	// gives one free attempt and five retries over roughly five minutes of
	// waiting; against a container that takes about ninety seconds to exhaust
	// its restarts, the whole run is about fourteen minutes. Long enough to
	// ride out a transient cluster problem, short enough not to spend an hour
	// confirming a broken image.
	backoffGiveUpAt int32 = 6
)

// CountFailures folds this pass's views into the group's running count of
// consecutive failures, and returns the newest failure timestamp it counted.
//
// A failure counts once, identified by its own status.failedAt being newer
// than the newest one already counted. That test is what makes the count
// idempotent: without it a five-second resync would re-count the same corpse
// forever. The window also runs from failedAt rather than from now, because
// stamping the observation would extend the window on every pass and the
// backoff would never expire.
//
// The streak breaks on a success *since* the last counted failure, not on any
// server being Ready. The weaker rule reads well and is wrong: a group with
// one healthy server and one that crash-loops — a bad node, a resource request
// only some nodes satisfy — would hold its count at zero forever and hammer
// indefinitely. A Failed server carries no ReadySince (the Server controller
// clears it on the way out of Ready), so a corpse can never look like the
// success that ends its own streak.
func CountFailures(views []ServerView, prev int32, since time.Time) (int32, time.Time) {
	var lastSuccess time.Time
	for _, v := range views {
		if v.ReadySince.After(lastSuccess) {
			lastSuccess = v.ReadySince
		}
	}

	count, from := prev, since
	if lastSuccess.After(since) {
		// Failures older than that success belong to the streak it ended, not
		// to a new one, so the count restarts and only failures after it are
		// counted below.
		count, from = 0, lastSuccess
	}

	newest := since
	for _, v := range views {
		if v.Phase != phase.Failed || !v.FailedAt.After(from) {
			continue
		}
		count++
		if v.FailedAt.After(newest) {
			newest = v.FailedAt
		}
	}
	return count, newest
}

// BackoffInputs is what the retry decision needs.
type BackoffInputs struct {
	// ConsecutiveFailures is the count CountFailures produced.
	ConsecutiveFailures int32
	// LastFailureAt is the newest counted failure. The window runs from here.
	LastFailureAt time.Time
	// Now is the reconciler's clock.
	Now time.Time
}

// BackoffDecision is what the group may do about creating this pass.
type BackoffDecision struct {
	// MayCreate is false while a window is open and false once the group has
	// given up. Deletions, retirements and drains are never gated by it.
	MayCreate bool
	// GaveUp is true past the threshold. Nothing is created until the group's
	// spec changes.
	GaveUp bool
	// RetryAfter is how long until the window closes, for the condition's
	// message. Zero when MayCreate or GaveUp.
	RetryAfter time.Duration
}

// DecideBackoff turns the count into permission to create.
func DecideBackoff(in BackoffInputs) BackoffDecision {
	if in.ConsecutiveFailures >= backoffGiveUpAt {
		// Terminal until the spec changes. An elapsed window must not
		// resurrect it, which is why this is tested before the window below.
		return BackoffDecision{GaveUp: true}
	}
	if in.ConsecutiveFailures == 0 {
		return BackoffDecision{MayCreate: true}
	}
	ready := in.LastFailureAt.Add(backoffDelay(in.ConsecutiveFailures))
	if !in.Now.Before(ready) {
		return BackoffDecision{MayCreate: true}
	}
	return BackoffDecision{RetryAfter: ready.Sub(in.Now)}
}

// backoffDelay is the window after n consecutive failures.
//
// Multiplied in a loop with the cap checked each time rather than computed as
// base * factor^(n-1): a large n would overflow the exponent long before it
// reached anything meaningful, and the cap makes every step past it identical
// anyway.
func backoffDelay(n int32) time.Duration {
	d := backoffBase
	for i := int32(1); i < n; i++ {
		d *= backoffFactor
		if d >= backoffCap {
			return backoffCap
		}
	}
	return d
}
```

- [ ] **Step 4: Add the two `ServerView` fields**

In `internal/controller/candidates.go`, at the end of `ServerView`:

```go
	// FailedAt is status.failedAt: when this server entered phase Failed.
	// Zero for a server that has not. The backoff counts a failure from this
	// rather than from the moment it observed one, so a window cannot be
	// extended by being looked at.
	FailedAt time.Time
	// ReadySince is status.readySince: when this server last entered phase
	// Ready. Zero for one that never did — and the Server controller clears
	// it on every exit from Ready, so a Failed server carries no ReadySince.
	ReadySince time.Time
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
nix develop -c go test ./internal/controller/ -run 'TestCountFailures|TestDecideBackoff|TestBackoffDelay' -v
```

Expected: PASS, all cases.

- [ ] **Step 6: Prove two of them can fail**

1. Change `CountFailures`'s `!v.FailedAt.After(from)` to `v.FailedAt.Before(from)`, so a re-observed corpse counts again. `TestCountFailuresCountsANewCorpseOnce` must fail on its second assertion. Revert.
2. Change the streak test from `lastSuccess.After(since)` to `!lastSuccess.IsZero()`, the "any server is Ready" rule. `TestCountFailuresIgnoresASuccessOlderThanTheLastFailure` must fail. Revert.

Report both outputs verbatim.

- [ ] **Step 7: Run the whole package and commit**

```bash
nix develop -c go test ./internal/controller/ -cover
git add internal/controller/backoff.go internal/controller/backoff_test.go internal/controller/candidates.go
git commit -m "feat(4d): count a group's consecutive failures and decide when it may retry"
```

---

### Task 2: The API surface

**Files:**
- Modify: `api/v1alpha1/common_types.go`, `api/v1alpha1/servergroup_types.go`
- Modify (generated): `config/crd/bases/spawnery.cloud_servergroups.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Produces: `spawneryv1alpha1.ConditionBackingOff`, `spawneryv1alpha1.ReasonNoRecentFailures`, `ServerGroupStatus.ConsecutiveFailures int32` (JSON `consecutiveFailures`), `ServerGroupStatus.LastFailureAt *metav1.Time` (JSON `lastFailureAt`). Tasks 4 and 5 use all four.

- [ ] **Step 1: Write the failing test**

The API server silently drops fields it does not know, so a Go-only change round-trips as the zero value with no error. Only a real round trip catches a forgotten `make manifests`. Add to `internal/controller/servergroup_controller_test.go`, in the file's own fixture idiom — read `newFixture` and a nearby test first and follow it:

```go
func TestGroupBackoffFieldsRoundTripThroughTheAPIServer(t *testing.T) {
	f := newFixture(t)
	g := f.group

	now := metav1.Now()
	g.Status.ConsecutiveFailures = 3
	g.Status.LastFailureAt = &now
	if err := f.c.Status().Update(f.ctx, g); err != nil {
		t.Fatalf("status update: %v", err)
	}

	got := &spawneryv1alpha1.ServerGroup{}
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(g), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.ConsecutiveFailures != 3 {
		t.Error("status.consecutiveFailures did not survive the API server; run make manifests")
	}
	if got.Status.LastFailureAt == nil {
		t.Error("status.lastFailureAt did not survive the API server; run make manifests")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
nix develop -c go test ./internal/controller/ -run TestGroupBackoffFieldsRoundTrip -v
```

Expected: compile failure, `unknown field ConsecutiveFailures`.

- [ ] **Step 3: Add the constants**

In `api/v1alpha1/common_types.go`, in the condition block:

```go
	// ConditionBackingOff reports that the group is waiting before it creates
	// another server, because one or more failed to start. It is deliberately
	// not folded into Degraded: derivePhase turns a true Degraded into the
	// group's phase, and a group waiting ten seconds after a single hiccup
	// would then be indistinguishable from one with a real fault.
	ConditionBackingOff = "BackingOff"
```

and in the reason block:

```go
	ReasonNoRecentFailures = "NoRecentFailures"
```

- [ ] **Step 4: Add the status fields**

In `api/v1alpha1/servergroup_types.go`, in `ServerGroupStatus` after `ObservedGeneration`:

```go
	// ConsecutiveFailures counts servers that failed to start with no success
	// since. It lives on the CR rather than in the operator's memory because a
	// restart must not reset it: that would restart the create loop it exists
	// to bound. This is the opposite of the choice made for the empty-since
	// clock in milestone 4a, where a reset delays a scale-down and so errs
	// safely.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// LastFailureAt is the newest status.failedAt this group has counted. It
	// is what makes the count idempotent across resyncs, and the instant the
	// backoff window runs from.
	// +optional
	LastFailureAt *metav1.Time `json:"lastFailureAt,omitempty"`
```

- [ ] **Step 5: Regenerate and check the diff is only these two fields**

```bash
nix develop -c make manifests generate
git diff --stat
git diff config/crd/bases/
```

Expected: `consecutiveFailures` and `lastFailureAt` under `status.properties` in `spawnery.cloud_servergroups.yaml`, plus the deepcopy for the new pointer field. **If any other CRD file changes, stop and report it** — something unrelated has drifted.

- [ ] **Step 6: Run the test and prove it can fail**

```bash
nix develop -c go test ./internal/controller/ -run TestGroupBackoffFieldsRoundTrip -v
```

Expected: PASS. Then delete the `consecutiveFailures` property from the generated YAML by hand, re-run, confirm the failure names it, and restore with `make manifests`. Report the output.

- [ ] **Step 7: Commit**

```bash
git add api/v1alpha1/ config/crd/bases/ internal/controller/servergroup_controller_test.go
git commit -m "feat(4d): give a group a failure counter and a backing-off condition"
```

---

### Task 3: The group reads the two timestamps

**Files:**
- Modify: `internal/controller/servergroup_controller.go` (`collectViews`)
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `ServerView.FailedAt` / `ReadySince` (Task 1).
- Produces: views whose two new fields are populated. Task 4 depends on this.

- [ ] **Step 1: Write the failing test**

```go
func TestCollectViewsCarriesTheFailureAndReadyTimestamps(t *testing.T) {
	// Both fields exist on the Server status and were simply never lifted into
	// the view. The backoff reads them, and a view that leaves them zero makes
	// every failure look like it happened at the epoch — which counts once and
	// then never again.
	f := newFixture(t)
	f.createServer("lobby-tsx1")

	failed := metav1.NewTime(f.clock.Now().Add(-time.Minute))
	ready := metav1.NewTime(f.clock.Now().Add(-time.Hour))
	srv := f.server("lobby-tsx1")
	srv.Status.Phase = string(phase.Failed)
	srv.Status.FailedAt = &failed
	srv.Status.ReadySince = &ready
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("status update: %v", err)
	}

	views, _, err := f.r.collectViews(f.ctx, f.group)
	if err != nil {
		t.Fatalf("collectViews: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1", len(views))
	}
	if !views[0].FailedAt.Equal(failed.Time) {
		t.Errorf("FailedAt = %v, want %v", views[0].FailedAt, failed.Time)
	}
	if !views[0].ReadySince.Equal(ready.Time) {
		t.Errorf("ReadySince = %v, want %v", views[0].ReadySince, ready.Time)
	}
}
```

Adapt `f.r`, `f.clock` and the helpers to whatever the fixture actually exposes; read it first.

- [ ] **Step 2: Run it to verify it fails**

```bash
nix develop -c go test ./internal/controller/ -run TestCollectViewsCarriesThe -v
```

Expected: FAIL — both fields are the zero time.

- [ ] **Step 3: Populate them**

In `collectViews`, in the `ServerView{...}` literal after `CreatedAt`:

```go
			}
			if srv.Status.FailedAt != nil {
				v.FailedAt = srv.Status.FailedAt.Time
			}
			if srv.Status.ReadySince != nil {
				v.ReadySince = srv.Status.ReadySince.Time
			}
```

Place these after the literal is assigned to `v` and before the `if v.Phase == ""` normalisation, following the shape already there.

- [ ] **Step 4: Run it to verify it passes**

```bash
nix develop -c go test ./internal/controller/ -run TestCollectViewsCarriesThe -v
```

- [ ] **Step 5: Prove it can fail, then commit**

Comment out the `FailedAt` assignment, re-run, confirm the failure, restore. Report the output.

```bash
nix develop -c go test ./internal/controller/ -cover
git add internal/controller/servergroup_controller.go internal/controller/servergroup_controller_test.go
git commit -m "feat(4d): lift the failure and ready timestamps into the view"
```

---

### Task 4: Count, clear on a generation change, and gate the creates

**Files:**
- Modify: `internal/controller/servergroup_controller.go`
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–3.
- Produces: `size()` gains a `backoff BackoffDecision` parameter. Task 5 reads the same decision for the conditions.

- [ ] **Step 1: Write the failing tests**

```go
func TestGroupStopsCreatingWhileItBacksOff(t *testing.T) {
	// The point of the milestone: a group whose server failed does not build
	// another one on the next five-second pass.
	f := newFixture(t)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, f.r)

	name := f.oneServerName(t)
	f.failServer(t, name)          // phase Failed with status.failedAt stamped
	f.reconcileGroup(t, f.r)       // counts it, opens the window

	before := len(f.listServers(t))
	f.clock.Advance(resyncInterval)
	f.reconcileGroup(t, f.r)
	if got := len(f.listServers(t)); got != before {
		t.Errorf("group created a server inside the backoff window: %d -> %d", before, got)
	}

	g := f.reloadGroup(t)
	if g.Status.ConsecutiveFailures != 1 {
		t.Errorf("consecutiveFailures = %d, want 1", g.Status.ConsecutiveFailures)
	}
}

func TestGroupCreatesAgainOnceTheWindowCloses(t *testing.T) {
	f := newFixture(t)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, f.r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, f.r)

	before := len(f.listServers(t))
	f.clock.Advance(backoffBase + time.Second)
	f.reconcileGroup(t, f.r)
	if got := len(f.listServers(t)); got != before+1 {
		t.Errorf("servers = %d, want %d: the window closed and the group should build again", got, before+1)
	}
}

func TestGroupStillShedsWhileItBacksOff(t *testing.T) {
	// The backoff holds back building, not tidying up. A deletion path that
	// waited on an unrelated failure would leave surplus servers standing and
	// stale ones unretired for the whole window.
	f := newFixture(t)
	f.setMinReplicas(t, 1)
	f.setMaxReplicas(t, 1)
	f.reconcileGroup(t, f.r)
	// Two servers, one over the ceiling, plus a failure to open the window.
	extra := f.createIdleServer(t)
	f.failServer(t, f.oneOtherServerName(t, extra))
	f.reconcileGroup(t, f.r)

	if _, alive := f.serverIfPresent(t, extra); !alive {
		return // already gone: the surplus was shed, which is what this asserts
	}
	f.clock.Advance(resyncInterval)
	f.reconcileGroup(t, f.r)
	if _, alive := f.serverIfPresent(t, extra); alive {
		t.Error("the surplus server was not shed while the group was backing off")
	}
}

func TestGenerationChangeClearsTheBackoff(t *testing.T) {
	f := newFixture(t)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, f.r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, f.r)

	// The operator's answer to whatever failed.
	f.bumpGeneration(t)
	f.reconcileGroup(t, f.r)

	g := f.reloadGroup(t)
	if g.Status.ConsecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d after a spec change, want 0", g.Status.ConsecutiveFailures)
	}
	if g.Status.LastFailureAt != nil {
		t.Error("lastFailureAt survived a spec change")
	}
}
```

Several helpers here do not exist — `failServer`, `oneServerName`, `reloadGroup`, `createIdleServer`, `serverIfPresent`, `setMaxReplicas`, `bumpGeneration`. Read the fixture first: some exist under other names, and `bumpGeneration` was added by milestone 4b. Write the genuinely missing ones beside the tests in the file's own style. Do not build a new helper layer.

- [ ] **Step 2: Run them to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestGroupStopsCreating|TestGroupCreatesAgain|TestGroupStillSheds|TestGenerationChangeClears' -v
```

Expected: FAIL — the group keeps creating, and `consecutiveFailures` stays zero.

- [ ] **Step 3: Clear on a generation change, count, and decide**

In `Reconcile`, immediately before the block that calls `r.size(...)`:

```go
	// The counter and the two conditions belong to the spec that produced the
	// failures. A generation change is the operator's answer to whatever broke,
	// so the streak it caused is over and the next attempt is immediate.
	if group.Generation != group.Status.ObservedGeneration {
		group.Status.ConsecutiveFailures = 0
		group.Status.LastFailureAt = nil
		meta.RemoveStatusCondition(&group.Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
		meta.RemoveStatusCondition(&group.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	}

	var lastFailure time.Time
	if group.Status.LastFailureAt != nil {
		lastFailure = group.Status.LastFailureAt.Time
	}
	failures, newestFailure := CountFailures(views, group.Status.ConsecutiveFailures, lastFailure)
	group.Status.ConsecutiveFailures = failures
	if !newestFailure.IsZero() {
		stamped := metav1.NewTime(newestFailure)
		group.Status.LastFailureAt = &stamped
	}
	backoff := DecideBackoff(BackoffInputs{
		ConsecutiveFailures: failures,
		LastFailureAt:       newestFailure,
		Now:                 r.Clock(),
	})
```

- [ ] **Step 4: Gate the creates**

Change `size`'s signature and its one call site to carry the decision:

```go
func (r *ServerGroupReconciler) size(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
	servers map[string]*spawneryv1alpha1.Server,
	backoff BackoffDecision,
) (SizeDecision, error) {
```

and wrap only the create loop:

```go
	// The gate is on execution, not on the decision. DecideSize keeps
	// computing what the group needs, so Limited and ColdStartBlocked go on
	// telling the truth about the shortfall while the backoff separately says
	// the group is waiting — two facts an operator needs to see apart. It also
	// means expectations never reserves a create that did not happen.
	//
	// Only creation is gated. The deletes and retirements below run either
	// way: they touch players, and must not wait on an unrelated failure.
	if backoff.MayCreate {
		for i := int32(0); i < decision.Create; i++ {
			name, err := r.createServer(ctx, group)
			if err != nil {
				return decision, err
			}
			r.Expectations.expectCreated(key, name)
		}
	}
```

- [ ] **Step 5: Run them to verify they pass**

```bash
nix develop -c go test ./internal/controller/ -cover
```

Expected: PASS across the package, coverage at or above 88%. If a pre-existing test broke, **stop and tell me before changing it** — a group that never fails should see no behaviour change at all, so a break means the gate is firing where it should not.

- [ ] **Step 6: Prove the gate is load-bearing**

Remove the `if backoff.MayCreate` wrapper so creates always run. `TestGroupStopsCreatingWhileItBacksOff` must fail. Revert. Report the output.

- [ ] **Step 7: Commit**

```bash
git add internal/controller/servergroup_controller.go internal/controller/servergroup_controller_test.go
git commit -m "feat(4d): hold back a group's creates while it backs off"
```

---

### Task 5: The two conditions

**Files:**
- Modify: `internal/controller/servergroup_controller.go`
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `BackoffDecision` (Task 1), the constants (Task 2).
- Produces: nothing further.

- [ ] **Step 1: Write the failing tests**

```go
func TestBackingOffConditionNamesTheCountAndTheWait(t *testing.T) {
	f := newFixture(t)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, f.r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, f.r)

	g := f.reloadGroup(t)
	c := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("BackingOff = %+v, want True", c)
	}
	if c.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("reason = %q, want %q", c.Reason, spawneryv1alpha1.ReasonCrashLoopBackoff)
	}
	if !strings.Contains(c.Message, "1 server") || !strings.Contains(c.Message, "next attempt in") {
		t.Errorf("message = %q, want the count and the remaining wait", c.Message)
	}
	// A group merely waiting is not a group with a fault. derivePhase turns a
	// true Degraded into the group's phase, so conflating the two would make a
	// ten-second wait look identical to a broken image.
	if meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Error("Degraded is true after a single failure")
	}
}

func TestGroupGivesUpAndSaysSo(t *testing.T) {
	f := newFixture(t)
	f.setMinReplicas(t, 1)
	// Drive the streak to the threshold: fail, wait out the window, repeat.
	for i := int32(0); i < backoffGiveUpAt; i++ {
		f.reconcileGroup(t, f.r)
		if name, ok := f.newestServerName(t); ok {
			f.failServer(t, name)
		}
		f.reconcileGroup(t, f.r)
		f.clock.Advance(backoffCap)
	}
	f.reconcileGroup(t, f.r)

	g := f.reloadGroup(t)
	if !meta.IsStatusConditionTrue(g.Status.Conditions, spawneryv1alpha1.ConditionDegraded) {
		t.Fatalf("Degraded is not true after %d failures", backoffGiveUpAt)
	}
	if got := meta.FindStatusCondition(g.Status.Conditions,
		spawneryv1alpha1.ConditionDegraded).Reason; got != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("Degraded reason = %q, want CrashLoopBackoff", got)
	}
	if g.Status.Phase != "Degraded" {
		t.Errorf("phase = %q, want Degraded: derivePhase already maps the condition", g.Status.Phase)
	}
	// BackingOff means "waiting, will try again". There is no pending retry
	// now, so it is false — but with a reason and a message that say why,
	// because NoRecentFailures there would be a lie.
	c := meta.FindStatusCondition(g.Status.Conditions, spawneryv1alpha1.ConditionBackingOff)
	if c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("BackingOff = %+v, want False once the group gave up", c)
	}
	if c.Reason != spawneryv1alpha1.ReasonCrashLoopBackoff {
		t.Errorf("reason = %q, want CrashLoopBackoff rather than an all-clear", c.Reason)
	}

	before := len(f.listServers(t))
	f.clock.Advance(time.Hour)
	f.reconcileGroup(t, f.r)
	if got := len(f.listServers(t)); got != before {
		t.Errorf("a group that gave up created a server after an hour: %d -> %d", before, got)
	}
}

func TestBackingOffEventFiresOnTheFlankOnly(t *testing.T) {
	f := newFixture(t)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, f.r)
	f.failServer(t, f.oneServerName(t))
	f.reconcileGroup(t, f.r)

	first := f.countEvents(t, spawneryv1alpha1.ReasonCrashLoopBackoff)
	if first != 1 {
		t.Fatalf("events = %d after the first failure, want 1", first)
	}
	f.clock.Advance(time.Second)
	f.reconcileGroup(t, f.r)
	if got := f.countEvents(t, spawneryv1alpha1.ReasonCrashLoopBackoff); got != first {
		t.Errorf("events = %d after a resync inside the same window, want %d: the event goes on the flank", got, first)
	}
}
```

`countEvents` and `newestServerName` may not exist; the fixture already records events for the `ScalingLimited` tests — read those and reuse what is there.

- [ ] **Step 2: Run them to verify they fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestBackingOff|TestGroupGivesUp' -v
```

Expected: FAIL — neither condition is ever set.

- [ ] **Step 3: Set the conditions**

In `Reconcile`, inside the existing `if group.IsEphemeral() { ... }` block that already sets `ScalingLimited`, after that condition is handled:

```go
		// Built false-by-default and flipped, like ScalingLimited above.
		backingOff := metav1.Condition{
			Type:    spawneryv1alpha1.ConditionBackingOff,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonNoRecentFailures,
			Message: "no server has failed to start recently",
		}
		degraded := metav1.Condition{
			Type:    spawneryv1alpha1.ConditionDegraded,
			Status:  metav1.ConditionFalse,
			Reason:  spawneryv1alpha1.ReasonNoRecentFailures,
			Message: "servers are starting normally",
		}
		switch {
		case backoff.GaveUp:
			// No pending retry, so BackingOff is false — but an all-clear
			// reason here would be a lie, so it carries the real one.
			backingOff.Reason = spawneryv1alpha1.ReasonCrashLoopBackoff
			backingOff.Message = fmt.Sprintf(
				"not retrying: %d servers failed to start in a row; change the group's spec to try again",
				group.Status.ConsecutiveFailures)
			degraded.Status = metav1.ConditionTrue
			degraded.Reason = spawneryv1alpha1.ReasonCrashLoopBackoff
			degraded.Message = backingOff.Message
		case backoff.RetryAfter > 0:
			backingOff.Status = metav1.ConditionTrue
			backingOff.Reason = spawneryv1alpha1.ReasonCrashLoopBackoff
			backingOff.Message = fmt.Sprintf(
				"%d server(s) failed to start in a row; next attempt in %s",
				group.Status.ConsecutiveFailures, backoff.RetryAfter.Round(time.Second))
		}

		wasBackingOff := meta.IsStatusConditionTrue(group.Status.Conditions,
			spawneryv1alpha1.ConditionBackingOff)
		wasDegraded := meta.IsStatusConditionTrue(group.Status.Conditions,
			spawneryv1alpha1.ConditionDegraded)
		meta.SetStatusCondition(&group.Status.Conditions, backingOff)
		meta.SetStatusCondition(&group.Status.Conditions, degraded)
		// The event goes on the flank only, for the reason the ScalingLimited
		// block gives: a five-second resync would otherwise announce the same
		// wait over and over for its whole duration.
		if isTrue := backingOff.Status == metav1.ConditionTrue; isTrue != wasBackingOff {
			eventType := corev1.EventTypeNormal
			if isTrue {
				eventType = corev1.EventTypeWarning
			}
			r.Recorder.Event(group, eventType, backingOff.Reason, backingOff.Message)
		}
		if isTrue := degraded.Status == metav1.ConditionTrue; isTrue != wasDegraded {
			eventType := corev1.EventTypeNormal
			if isTrue {
				eventType = corev1.EventTypeWarning
			}
			r.Recorder.Event(group, eventType, degraded.Reason, degraded.Message)
		}
```

- [ ] **Step 4: Run them to verify they pass**

```bash
nix develop -c go test ./internal/controller/ -cover
```

- [ ] **Step 5: Prove two of them can fail**

1. Change the give-up branch so `degraded.Status` stays false. `TestGroupGivesUpAndSaysSo` must fail on the condition and on the phase. Revert.
2. Move the event out of the flank test so it fires every pass. `TestBackingOffEventFiresOnTheFlankOnly` must fail. Revert.

Report both outputs.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/servergroup_controller.go internal/controller/servergroup_controller_test.go
git commit -m "feat(4d): say on the group that it is waiting, and when it has given up"
```

---

### Task 6: Retire 4b's cold-start suppression

**Files:**
- Modify: `internal/controller/scaling.go`
- Test: `internal/controller/scaling_test.go`

**Interfaces:** none new. This removes code.

**Why this is its own task, and the one to be most careful in.** Milestone 4b suppressed the cold start while a `Failed` server of the current generation was retained, so a broken new image produced one attempt per `failedRetentionSeconds` (an hour) instead of one every five seconds. Its design says in as many words that this was "deliberately not backoff" — a stopgap for this milestone. The backoff replaces it and does better: seconds after a single failure, growing only if failures keep coming.

**This task deletes tests, and that is where coverage disappears silently.** The order below is not negotiable.

- [ ] **Step 1: Prove the backoff already guards the loop, before removing anything**

Write this test first, with the suppression still in place:

```go
func TestGroupWithABrokenNewImageDoesNotRebuildEveryPass(t *testing.T) {
	// The loop 4b's cold-start suppression was a stopgap for: a broken new
	// image fails, stops counting toward the group's size, and is recreated on
	// the next five-second pass. This asserts the backoff bounds it, so the
	// stopgap can be removed without reopening it.
	f := newFixture(t)
	f.setMinReplicas(t, 1)
	f.reconcileGroup(t, f.r)
	f.markReady(t, f.oneServerName(t))

	f.bumpGeneration(t)          // the changeover begins; the cold start builds one
	f.reconcileGroup(t, f.r)
	created := len(f.listServers(t))

	// Every replacement fails immediately, and the clock moves only by the
	// resync interval — far less than the backoff's first window.
	for i := 0; i < 10; i++ {
		if name, ok := f.newestServerName(t); ok {
			f.failServer(t, name)
		}
		f.clock.Advance(resyncInterval)
		f.reconcileGroup(t, f.r)
	}

	if got := len(f.listServers(t)); got > created+1 {
		t.Errorf("group built %d servers across ten passes, want at most %d: the backoff must bound the loop",
			got, created+1)
	}
}
```

- [ ] **Step 2: Run it, and mutate to prove it is the backoff doing the work**

```bash
nix develop -c go test ./internal/controller/ -run TestGroupWithABrokenNewImage -v
```

Expected: PASS. Now **temporarily** remove the `v.Phase == phase.Failed ||` term from `coldStart` — the stopgap — and re-run. It must still pass, which is what shows the backoff is holding the loop rather than the stopgap. Then restore the term, remove the backoff gate instead (`if backoff.MayCreate` in `size()`), and re-run: it must **fail**. Restore both.

Report both outputs. **If the test passes with the backoff gate removed, stop and tell me** — that would mean the stopgap is still doing the work and removing it is not yet safe.

- [ ] **Step 3: Remove the suppression**

In `coldStart`, the current-generation branch becomes:

```go
		if v.Generation == in.Generation {
			if v.countsTowardSize() {
				current++
			}
			continue
		}
```

and its doc comment loses the paragraph beginning "It is suppressed while a `Failed` server of the *current* generation is being retained", replaced by:

```go
// A Failed server of the current generation does not suppress this. It used to
// — milestone 4b's stopgap against a broken image being recreated every five
// seconds — and the per-group backoff replaced it: the same loop is now bounded
// by a window that starts at seconds and grows only if the failures keep
// coming, rather than by a flat retention hour after any single failure.
```

- [ ] **Step 4: Remove the tests that pinned it**

`TestDecideSizeDoesNotColdStartAgainstARetainedFailure` asserted the stopgap and is now false — the cold start *does* fire against a retained failure, and the backoff is what holds it. Delete it. `TestDecideSizeStaleFailureDoesNotBlockTheColdStart` asserted that a *stale* failure does not suppress; with no suppression at all it can no longer fail, so delete it too rather than leave a case that cannot fail.

**Say in the commit body which tests were removed and which test now covers the property**, so a reader of the history can find the replacement.

- [ ] **Step 5: Run the whole package**

```bash
nix develop -c go test ./internal/controller/ -cover
```

Expected: PASS, coverage at or above 88%. **If it drops below, stop and tell me** — that is the signal that removing those cases took cover with them.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/scaling.go internal/controller/scaling_test.go internal/controller/servergroup_controller_test.go
git commit -m "fix(4d): retire the cold-start stopgap the backoff replaces"
```

---

### Task 7: The paperwork

**Files:**
- Modify: `docs/superpowers/specs/2026-08-13-rolling-updates-design.md`
- Modify: `docs/known-issues.md`
- Modify: `docs/handover-milestone-4.md`

**Interfaces:** none.

- [ ] **Step 1: Correct 4b's spec §3.7**

That section describes the cold-start suppression as current behaviour. Add a correction note in the shape this repository already uses, immediately under its heading:

```markdown
> **Superseded by milestone 4d.** The suppression described below was removed
> once per-group backoff landed. It was always a stopgap — this section says
> so itself — and the backoff bounds the same loop with a window that starts
> at ten seconds and grows, instead of a flat retention hour after any single
> failure. The reasoning below is kept because it is the record of why the
> loop needed closing at all; the mechanism it describes is gone. See
> `2026-08-13-per-group-backoff-design.md`.
```

- [ ] **Step 2: Close the known-issues entry**

`docs/known-issues.md` carries "The cold-start loop is only half-closed" under "From milestone 4b" and "Exponential backoff per group" under "Preconditions for milestone 4". Close both in the file's own `*Met* by …` style, naming what actually landed: `CountFailures` and `DecideBackoff` in `internal/controller/backoff.go`, the counter on `ServerGroupStatus`, `ConditionBackingOff`, and `Degraded`/`CrashLoopBackoff` at six consecutive failures with a spec change as the way back.

Keep the reasoning both entries carry. In particular keep the sentence explaining that `maxRetainedFailures = 1` bounds the footprint rather than the rate — that is still true of that constant, which stays.

- [ ] **Step 3: Add 4d to the handover**

`docs/handover-milestone-4.md` has a section per landed sub-milestone. Add one for 4d in the same shape: what it built, and what 4c now finds in place. Cover the two pure rules, the counter's home on the CR and why, the gate sitting on execution rather than on the decision, and the two conditions — `ConditionBackingOff` being separate from `Degraded` for the same reason `ScalingLimited` is, which is the pattern 4c will want for the proxy side.

- [ ] **Step 4: Commit**

```bash
git add docs/
git commit -m "docs(4d): close the backoff entries and record what 4c finds"
```

---

## Before the branch is merged

- [ ] `nix develop -c make test` — green, `internal/controller` at or above 88%, `internal/phase` at 100%.
- [ ] `nix develop -c make manifests` — no diff beyond Task 2's two status fields.
- [ ] `git diff --name-only master...HEAD` — nothing under `agent/`, `image/`, `proto/`, `nix/`.
- [ ] One whole-branch review before merge. On 4a and again on 4b this review found interactions no task-scoped review could see — a lowered ceiling never enforced on a group that was also short, then a fixed point where a refused cold start suppressed the very branch that would have freed its room. The equivalent risk here is the interaction between the backoff gate, the changeover's cold start and `pruneFailed`, which no single task exercises together.

## Self-review

**Spec coverage.** §3.1 → Task 2 (fields) and Task 4 (writing them). §3.2 → Task 1's `CountFailures`. §3.3 → Task 1, the two tests that separate a success after from a success before. §3.4 → Task 4's gate and its "still sheds" test. §3.5 → Task 4's generation clear and Task 5's give-up branch. §3.6 → Task 1's constants. §3.7 → Task 5. §4.1 → Task 2. §4.2 → Task 1. §4.3 → Tasks 1 and 3. §4.4 → Tasks 4 and 5. §4.5 → Task 6. §5's flow → Tasks 4 and 5's envtests. §6's edges: restart durability is Task 2's fields plus Task 4's read; the deleted-corpse case is documented and deliberately untested, since it is a human action with an accepted consequence; the changeover interaction is Task 6's `TestGroupWithABrokenNewImageDoesNotRebuildEveryPass`. §9 → each task's tests. §10's criteria → the pre-merge checklist plus Tasks 4, 5 and 6.

**One criterion no task proves directly.** Criterion 5, that the counter survives an operator restart, follows from the fields being on the CR rather than in memory — there is no in-process state to lose. An envtest cannot restart the operator, and a test that reconstructed a reconciler and re-read the group would prove that the API server stores what Task 2 already proves it stores. It is written down here rather than quietly dropped so the whole-branch review can decide whether it wants more.

**Placeholders:** none. Every step carries its code. Tasks 3, 4 and 5 name fixture helpers that may not exist and say to read the fixture first and write only what is genuinely missing.

**Type consistency:** `CountFailures(views, prev, since) (int32, time.Time)` is defined in Task 1 and called in Task 4 under that signature. `BackoffDecision`'s three fields — `MayCreate`, `GaveUp`, `RetryAfter` — are produced in Task 1, gated on in Task 4, and read for the messages in Task 5. `ServerView.FailedAt` / `ReadySince` are declared in Task 1, populated in Task 3, consumed in Task 1's own rules. `ConditionBackingOff` and `ReasonNoRecentFailures` are added in Task 2 and used in Task 5. `backoffBase`, `backoffCap` and `backoffGiveUpAt` are used by name in Tasks 4, 5 and 6's tests.
