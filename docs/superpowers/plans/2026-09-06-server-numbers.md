# Server Numbers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every server a short number within its group, carry it to the agents, and use it in the two places a player reads a server name.

**Architecture:** The `ServerGroup` reconciler assigns the number once, when it creates the server, and stores it in a new `Server.spec.number`. `internal/netstate` publishes it as `ServerState.number`, the agent's `NetworkMirror` puts it on `ServerInfo`, and two plugins in cyperia render `displayName + "-" + number`. Server names keep their random suffix; nothing about pod naming changes.

**Tech Stack:** Go 1.x with controller-runtime and envtest, protobuf/gRPC, Kotlin and Java 17 records built through Nix and Gradle, Helm, and two cyperia Gradle projects built through cadev.

**Spec:** `docs/superpowers/specs/2026-09-06-server-numbers-design.md`

## Global Constraints

- Every command in this repository runs inside the Nix dev shell: prefix with `nix develop -c`.
- The whole-tree Go suite needs `-p 1`, or the parallel envtest API servers exhaust this 7 GB machine (exit 137 looks like the session dying, not a test failure).
- **Nix builds read the git index, not the working tree.** `git add` every new file before `make agent` or any image build.
- Generated files are committed and CI diffs them. After the API change run `make manifests generate`; after the `.proto` change run `make proto`.
- Everything that lands in git is English: commit messages, comments, docs. Existing German stays German.
- Commit messages are Conventional Commits with a scope, subject saying what changed, body saying why, wrapped at 72 columns, and ending with:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
  ```
- Never pass `--no-gpg-sign`. If signing fails, stop and ask for an interactive unlock.
- Comments say only what the code cannot. One line is almost always enough; the rest belongs in the commit.
- The number is written `Hub-2`: the group's `displayName`, a hyphen, the number.
- `spec.number == 0` means "not numbered". Every reader falls back to what it shows today.
- Ephemeral numbers start at 1. A persistent server's number is its ordinal, which starts at 0.
- Target versions for the release: `imageVersion` 0.2.27, `operatorVersion` 0.2.26, chart 0.2.26.

---

## File Structure

**spawnery — the operator side**

| File | Responsibility |
|---|---|
| `api/v1alpha1/server_types.go` | declares `ServerSpec.Number` |
| `internal/controller/numbers.go` (new) | `NextNumber`, the pure "lowest free" rule |
| `internal/controller/numbers_test.go` (new) | its table test |
| `internal/controller/candidates.go` | `ServerView.Number` |
| `internal/controller/expectations.go` | reserves the number beside the name |
| `internal/controller/servergroup_controller.go` | builds the taken set, assigns, fills the view |
| `internal/controller/proxygroup_controller.go` | passes 0 to the changed `expectCreated` |
| `internal/controller/servernumber_envtest_test.go` (new) | the API server's own bound on the field |

**spawnery — the wire and the agent**

| File | Responsibility |
|---|---|
| `proto/spawnery/agent/v1alpha1/agent.proto` | `ServerState.number` |
| `internal/netstate/netstate.go` | fills it from `spec.number` |
| `agent/api/.../ServerInfo.java` | the record component a plugin reads |
| `agent/common/.../NetworkMirror.kt` | wire to record |

**cyperia — the two readers**

| File | Responsibility |
|---|---|
| `essentials/velocity/.../tablist/ServerDisplayNames.java` | the tab list name, gated on the group attribute |
| `lobby/common/.../selector/data/SelectableServerData.java` | carries a display name beside the route |
| `lobby/common/.../selector/data/SpawnerySelectorDataSource.java` | fills it |
| `lobby/common/.../selector/menu/ServerSelectorMenu.java` | renders it |

**cyperia/configs**

| File | Responsibility |
|---|---|
| `spawnery/base/groups/hub.yaml` | carries `tablist: numbered` |
| `spawnery/base/groups/*.yaml` | image bumps to 0.2.27 |

---

### Task 1: The field on the CRD and in the view

**Files:**
- Modify: `api/v1alpha1/server_types.go:26-31` (after `Ordinal`)
- Modify: `internal/controller/candidates.go:36` (after `ServerView.Ordinal`)
- Modify: `internal/controller/servergroup_controller.go:1282` (the `ServerView` literal)
- Create: `internal/controller/servernumber_envtest_test.go`
- Generated, committed: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`

**Interfaces:**
- Consumes: nothing.
- Produces: `spawneryv1alpha1.ServerSpec.Number int32` (json `number`), `controller.ServerView.Number int32`.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/servernumber_envtest_test.go`:

```go
/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/testenv"
)

// The floor under a server's number is the API server's, so it holds against
// every writer rather than against the one path the reconciler writes through.

func numberedServer(ns string, number int32) *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "hub-dvjk", Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "hub"},
			Number:   number,
		},
	}
}

func TestTheAPIServerAdmitsAServersNumber(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	srv := numberedServer(ns, 2)
	if err := c.Create(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}

	var read spawneryv1alpha1.Server
	if err := c.Get(ctx, client.ObjectKeyFromObject(srv), &read); err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.Spec.Number != 2 {
		t.Errorf("number = %d, want what was written", read.Spec.Number)
	}
}

func TestAServerWithoutANumberCarriesZero(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "hub-pgqg", Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "hub"},
		},
	}
	if err := c.Create(ctx, srv); err != nil {
		t.Fatalf("create: %v", err)
	}

	var read spawneryv1alpha1.Server
	if err := c.Get(ctx, client.ObjectKeyFromObject(srv), &read); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Zero is the whole contract for a server nobody numbered: every reader
	// falls back on it rather than asking whether the field was set.
	if read.Spec.Number != 0 {
		t.Errorf("number = %d, want 0", read.Spec.Number)
	}
}

func TestTheAPIServerRefusesANegativeNumber(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	err := c.Create(ctx, numberedServer(ns, -1))
	if err == nil {
		t.Fatal("create accepted a negative number")
	}
	if !strings.Contains(err.Error(), "number") {
		t.Errorf("error does not name the field: %v", err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/controller/ -run 'TestTheAPIServerAdmitsAServersNumber|TestAServerWithoutANumberCarriesZero|TestTheAPIServerRefusesANegativeNumber' -count=1`

Expected: FAIL to compile — `unknown field Number in struct literal of type v1alpha1.ServerSpec`.

- [ ] **Step 3: Add the field**

In `api/v1alpha1/server_types.go`, directly after the `Ordinal` field:

```go
	// Number is which of its group's servers this is, counted the way a
	// person counts: the second hub is 2. The group assigns it once, at
	// creation, and it is given out again only after this server is gone.
	//
	// Zero means nobody numbered this server, which is every server that was
	// already running when this field arrived. Nothing backfills them; a
	// reader showing this to a player falls back to the group's own name.
	//
	// Not Ordinal, and the difference is what each one is for: that one is a
	// persistent server's identity, the thing its storage claim is named
	// from, and its presence is read elsewhere as "this server is
	// persistent". This one is a label a player reads, and every server has
	// one. A persistent server's Number equals its Ordinal, so the number a
	// person sees agrees with the name the server already has.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Number int32 `json:"number,omitempty"`
```

- [ ] **Step 4: Regenerate what is committed**

Run: `nix develop -c make manifests generate`

Then confirm the field reached all three places:

```bash
grep -rn "number:" config/crd/bases/spawnery.cloud_servers.yaml
grep -c "number" charts/spawnery/templates/crds.yaml
```

Expected: the property appears under the `Server` CRD's `spec` in both files.

- [ ] **Step 5: Run the test and watch it pass**

Run: `nix develop -c go test ./internal/controller/ -run 'TestTheAPIServerAdmitsAServersNumber|TestAServerWithoutANumberCarriesZero|TestTheAPIServerRefusesANegativeNumber' -count=1`

Expected: PASS. This package boots its own etcd and API server and takes ~85 s even for three tests; that is normal, not a hang.

- [ ] **Step 6: Carry the field into the view**

In `internal/controller/candidates.go`, directly after `Ordinal *int32`:

```go
	// Number is spec.number: which of its group's servers this is, as a
	// person counts. Zero for a server created before the field existed.
	// Read here so the sizing pass can see which numbers are in use without
	// fetching the objects again.
	Number int32
```

In `internal/controller/servergroup_controller.go`, in the `ServerView` literal at line 1282, directly after `Ordinal: srv.Spec.Ordinal,`:

```go
			Number:   srv.Spec.Number,
```

- [ ] **Step 7: Build and run the package's tests**

Run: `nix develop -c go build ./... && nix develop -c go test ./api/... -count=1`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add api/v1alpha1/server_types.go api/v1alpha1/zz_generated.deepcopy.go \
  config/crd/bases charts/spawnery/templates/crds.yaml \
  internal/controller/candidates.go internal/controller/servergroup_controller.go \
  internal/controller/servernumber_envtest_test.go
git commit -m "$(cat <<'EOF'
feat(api): a server carries the number a person reads it by

spec.number is which of its group's servers this is, counted the way a
person counts. The group will assign it at creation; this commit only
declares it, defaults it to zero for every server that predates it, and
carries it into ServerView so the sizing pass can see which numbers are
already in use.

Not spec.ordinal: server_controller.go reads that field's presence as
"this server is persistent" when the group cannot be read, so setting it
on every server would make every server persistent to that path.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
```

---

### Task 2: The reservation carries the number

**Files:**
- Modify: `internal/controller/expectations.go:44-47` (the `expectation` struct), `:85-88` (`expectCreated`), `:100-110` (`record`), after `:222` (`pending`)
- Modify: `internal/controller/proxygroup_controller.go:1025`
- Modify: `internal/controller/servergroup_controller.go:944,951`
- Test: `internal/controller/expectations_test.go`

**Interfaces:**
- Consumes: `ServerView.Number int32` from Task 1.
- Produces: `expectCreated(group, name string, number int32)`, `pendingNumbers(group string) map[int32]bool`.

**Why this task exists at all.** `size` may create a server the cache has not shown yet and be called again before it appears — the comment on `expectations` calls that the ordinary case for a scaler that reacts to player counts, not the rare one. Without a reservation the second pass reads the same views, computes the same lowest free number, and hands it to a second server. A number that shifts is untidy; a duplicate sticks for the whole life of both servers, because nothing revisits an assignment.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/expectations_test.go`:

```go
func TestPendingNumbersHoldsWhatWasReserved(t *testing.T) {
	e := newExpectations(time.Now)

	e.expectCreated("ns/hub", "hub-dvjk", 1)
	e.expectCreated("ns/hub", "hub-pgqg", 3)

	got := e.pendingNumbers("ns/hub")
	if !got[1] || !got[3] {
		t.Errorf("pendingNumbers = %v, want 1 and 3", got)
	}
	if len(got) != 2 {
		t.Errorf("pendingNumbers = %v, want exactly two entries", got)
	}
}

func TestPendingNumbersSkipsZero(t *testing.T) {
	e := newExpectations(time.Now)

	// A proxy pod and a persistent server both reserve a name without
	// reserving a number, and pass zero to say so. Zero is not a number the
	// ephemeral rule may hand out, so letting it into the set would only
	// mislead a reader of it.
	e.expectCreated("ns/gateway", "gateway-a1b2", 0)

	if got := e.pendingNumbers("ns/gateway"); len(got) != 0 {
		t.Errorf("pendingNumbers = %v, want empty", got)
	}
}

func TestAnObservedCreateReleasesItsNumber(t *testing.T) {
	e := newExpectations(time.Now)
	e.expectCreated("ns/hub", "hub-dvjk", 1)

	e.observe("ns/hub", []ServerView{{Name: "hub-dvjk", Number: 1}})

	if got := e.pendingNumbers("ns/hub"); len(got) != 0 {
		t.Errorf("pendingNumbers = %v, want empty once the create was seen", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/controller/ -run 'TestPendingNumbers|TestAnObservedCreateReleasesItsNumber' -count=1`

Expected: FAIL to compile — `too many arguments in call to e.expectCreated` and `e.pendingNumbers undefined`.

- [ ] **Step 3: Implement**

In `internal/controller/expectations.go`, add the field to `expectation`:

```go
type expectation struct {
	kind    expectationKind
	expires time.Time
	// number is the server number this create reserved, or 0 for a
	// reservation that reserved no number: a proxy pod, or a persistent
	// server, whose number is its ordinal and comes from the sizing rule
	// rather than from the free-number search.
	number int32
}
```

Change `expectCreated` and `record`:

```go
// expectCreated records a Server this reconciler has just created, and the
// number it was given. Pass 0 where no number was assigned.
func (e *expectations) expectCreated(group, name string, number int32) {
	e.record(group, name, expectationCreate, number)
}

// expectDeleted records a Server whose removal this reconciler has just asked
// for.
func (e *expectations) expectDeleted(group, name string) {
	e.record(group, name, expectationDelete, 0)
}

// expectRetired records a Server this reconciler has just asked to retire.
func (e *expectations) expectRetired(group, name string) {
	e.record(group, name, expectationRetire, 0)
}

func (e *expectations) record(group, name string, kind expectationKind, number int32) {
	e.mu.Lock()
	defer e.mu.Unlock()

	m, ok := e.byGroup[group]
	if !ok {
		m = make(map[string]expectation)
		e.byGroup[group] = m
	}
	m[name] = expectation{kind: kind, expires: e.now().Add(expectationTTL), number: number}
}
```

Add, directly after `pending`:

```go
// pendingNumbers is the set of server numbers reserved by creates this
// reconciler has issued and the cache has not shown yet.
//
// Beside pending rather than a fourth return value from it: one caller wants
// this and every other caller would have to name and discard it.
func (e *expectations) pendingNumbers(group string) map[int32]bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	numbers := make(map[int32]bool)
	for _, exp := range e.byGroup[group] {
		if exp.kind == expectationCreate && exp.number > 0 {
			numbers[exp.number] = true
		}
	}
	return numbers
}
```

- [ ] **Step 4: Fix the three call sites**

`internal/controller/proxygroup_controller.go:1025` — a proxy pod is never numbered:

```go
		r.Expectations.expectCreated(key, pod.Name, 0)
```

`internal/controller/servergroup_controller.go:944` — leave as `0` for now; Task 3 replaces it with the assigned number:

```go
			r.Expectations.expectCreated(key, name, 0)
```

`internal/controller/servergroup_controller.go:951` — the persistent branch, permanently 0:

```go
			// Zero and not the ordinal: this loop's numbers come from
			// DecidePersistentSize, which reads them off the views and needs
			// no reservation of its own. Reserving one here would put a
			// persistent group's ordinal 0 into a set whose zero means "no
			// number".
			r.Expectations.expectCreated(key, name, 0)
```

- [ ] **Step 5: Run the tests and watch them pass**

Run: `nix develop -c go test ./internal/controller/ -run 'TestPendingNumbers|TestAnObservedCreateReleasesItsNumber|TestExpectations' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/expectations.go internal/controller/expectations_test.go \
  internal/controller/proxygroup_controller.go internal/controller/servergroup_controller.go
git commit -m "$(cat <<'EOF'
feat(controller): a create reserves its server number with its name

size may create a server the cache has not shown yet and be called again
before it appears -- the ordinary case for a scaler that reacts to player
counts, not the rare one. Without a reservation the second pass reads the
same views and hands out the same lowest free number twice.

A shifted number is untidy; a duplicated one sticks for the whole life of
both servers, because nothing revisits an assignment. So the reservation
carries the number beside the name, and zero says a create reserved no
number at all: a proxy pod, or a persistent server, whose number is the
ordinal the sizing rule already decided.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
```

---

### Task 3: The group assigns the number

**Files:**
- Create: `internal/controller/numbers.go`, `internal/controller/numbers_test.go`
- Modify: `internal/controller/servergroup_controller.go:937-952` (the create loops), `:1396-1417` (`createServer`), `:1419-1440` (`createPersistentServer`)

**Interfaces:**
- Consumes: `ServerView.Number` (Task 1), `expectCreated(group, name string, number int32)` and `pendingNumbers(group string) map[int32]bool` (Task 2).
- Produces: `NextNumber(taken map[int32]bool) int32`; `createServer(ctx, group, podHash string, number int32) (string, error)`.

- [ ] **Step 1: Write the failing test for the pure rule**

Create `internal/controller/numbers_test.go`:

```go
/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import "testing"

func TestNextNumber(t *testing.T) {
	cases := []struct {
		name  string
		taken map[int32]bool
		want  int32
	}{
		{"the first server of a group is 1", nil, 1},
		{"counting up", map[int32]bool{1: true}, 2},
		{"a gap in the middle is filled first", map[int32]bool{1: true, 3: true}, 2},
		{"a gap at the bottom is filled first", map[int32]bool{2: true, 3: true}, 1},
		// Zero is not a number this rule hands out, so a set carrying it says
		// nothing about where to start.
		{"zero holds nothing back", map[int32]bool{0: true}, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextNumber(tc.taken); got != tc.want {
				t.Errorf("NextNumber(%v) = %d, want %d", tc.taken, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/controller/ -run TestNextNumber -count=1`

Expected: FAIL to compile — `undefined: NextNumber`.

- [ ] **Step 3: Write the rule**

Create `internal/controller/numbers.go`:

```go
/*
Copyright The Spawnery Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// NextNumber is the lowest number from 1 upwards that taken does not hold.
//
// Lowest free rather than highest plus one, so a group that scales up and down
// all day keeps its numbers short instead of counting into three digits. The
// cost is that a number is handed out again once its server is gone, which
// ServerInfo.incarnation is what tells apart.
//
// The loop is bounded by len(taken)+1: each iteration that continues needs a
// distinct member of a finite set.
func NextNumber(taken map[int32]bool) int32 {
	for n := int32(1); ; n++ {
		if !taken[n] {
			return n
		}
	}
}
```

- [ ] **Step 4: Run it and watch it pass**

Run: `nix develop -c go test ./internal/controller/ -run TestNextNumber -count=1`

Expected: PASS.

- [ ] **Step 5: Write the failing test for the assignment**

Append to `internal/controller/numbers_test.go`:

```go
// takenNumbers is what the create loop feeds NextNumber: the numbers its
// group's servers hold, plus the ones its own unobserved creates reserved.
func TestTakenNumbers(t *testing.T) {
	views := []ServerView{
		{Name: "hub-dvjk", Number: 1},
		{Name: "hub-pgqg", Number: 3},
		// A server from before the field existed holds nothing back.
		{Name: "hub-old1", Number: 0},
	}

	got := takenNumbers(views, map[int32]bool{4: true})

	for _, n := range []int32{1, 3, 4} {
		if !got[n] {
			t.Errorf("takenNumbers = %v, want it to hold %d", got, n)
		}
	}
	if got[0] || got[2] {
		t.Errorf("takenNumbers = %v, want neither 0 nor 2", got)
	}
}
```

- [ ] **Step 6: Run it and watch it fail**

Run: `nix develop -c go test ./internal/controller/ -run TestTakenNumbers -count=1`

Expected: FAIL to compile — `undefined: takenNumbers`.

- [ ] **Step 7: Write it**

Append to `internal/controller/numbers.go`:

```go
// takenNumbers is the set NextNumber searches: every number a live server of
// the group holds, and every number a create this reconciler issued reserved
// before the cache showed it.
func takenNumbers(views []ServerView, pending map[int32]bool) map[int32]bool {
	taken := make(map[int32]bool, len(views)+len(pending))
	for _, v := range views {
		if v.Number > 0 {
			taken[v.Number] = true
		}
	}
	for n := range pending {
		taken[n] = true
	}
	return taken
}
```

- [ ] **Step 8: Run it and watch it pass**

Run: `nix develop -c go test ./internal/controller/ -run 'TestNextNumber|TestTakenNumbers' -count=1`

Expected: PASS.

- [ ] **Step 9: Wire it into the create loops**

In `internal/controller/servergroup_controller.go`, replace the ephemeral create loop (currently lines 938-945) with:

```go
		taken := takenNumbers(views, r.Expectations.pendingNumbers(key))
		for i := int32(0); i < decision.Create; i++ {
			// Added to the set as well as passed, so the second create of one
			// pass does not repeat the first one's number: the reservation
			// below only helps the next pass.
			number := NextNumber(taken)
			taken[number] = true
			name, err := r.createServer(ctx, group, podHash, number)
			if err != nil {
				return decision, err
			}
			r.Expectations.expectCreated(key, name, number)
		}
```

Change `createServer` (line 1396 onward):

```go
// createServer creates one interchangeable server of an ephemeral group, under
// a name with a random suffix because it has no identity to preserve, and with
// the number a person will read it by.
func (r *ServerGroupReconciler) createServer(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	podHash string,
	number int32,
) (string, error) {
	srv, err := r.newServer(group, NewServerName(group.Name), podHash)
	if err != nil {
		return "", err
	}
	srv.Spec.Number = number
	if err := r.Create(ctx, srv); err != nil {
		return "", err
	}
	r.Recorder.Eventf(group, nil, corev1.EventTypeNormal, "ServerCreated", actionCreateServer,
		"created server %s", srv.Name)
	return srv.Name, nil
}
```

In `createPersistentServer`, directly after `srv.Spec.Ordinal = &ordinal`:

```go
	// The same number, so the one a person reads agrees with the name this
	// server already has: survival-0 reads as Survival-0. It leaves persistent
	// numbers starting at 0 where ephemeral ones start at 1, and agreeing with
	// the name is worth more than agreeing with the other kind.
	srv.Spec.Number = ordinal
```

- [ ] **Step 10: Write the failing tests for the reconciler**

These use the package's existing envtest fixture: `newFixture(t)` brings up a namespace with a group called `lobby`, `groupReconciler(f)` wires a reconciler onto it, `f.reconcileGroup(t, r)` runs one pass, `f.listServers(t)` reads the result and `f.createServer(name)` puts a server in the group by hand. `TestGroupScalesUpToTheFloor` (`internal/controller/servergroup_controller_test.go:264`) is the shape being followed.

Append to `internal/controller/numbers_test.go` — and add `spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"` to its imports:

```go
func TestAGroupNumbersTheServersItCreates(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	f.group.Spec.Scaling.MinReplicas = 3
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}

	f.reconcileGroup(t, r)

	seen := map[int32]bool{}
	for _, s := range f.listServers(t) {
		if seen[s.Spec.Number] {
			t.Fatalf("two servers carry number %d", s.Spec.Number)
		}
		seen[s.Spec.Number] = true
	}
	for _, want := range []int32{1, 2, 3} {
		if !seen[want] {
			t.Errorf("numbers = %v, want 1, 2 and 3", seen)
		}
	}
}

func TestAGroupFillsTheLowestGapInItsNumbers(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)

	// A group whose middle server went away. The next one is 2 and not 4, or a
	// group that scales up and down all day counts into three digits.
	for name, number := range map[string]int32{"lobby-aaaa": 1, "lobby-bbbb": 3} {
		srv := f.createServer(name)
		srv.Spec.Number = number
		if err := f.c.Update(f.ctx, srv); err != nil {
			t.Fatalf("number %s: %v", name, err)
		}
	}
	f.group.Spec.Scaling.MinReplicas = 3
	if err := f.c.Update(f.ctx, f.group); err != nil {
		t.Fatalf("update group: %v", err)
	}

	f.reconcileGroup(t, r)

	servers := f.listServers(t)
	var created *spawneryv1alpha1.Server
	for i := range servers {
		if servers[i].Name != "lobby-aaaa" && servers[i].Name != "lobby-bbbb" {
			created = &servers[i]
		}
	}
	if created == nil {
		t.Fatal("the group created no third server")
	}
	if created.Spec.Number != 2 {
		t.Errorf("number = %d, want the gap at 2", created.Spec.Number)
	}
}
```

- [ ] **Step 11: Run the package**

Run: `nix develop -c go test ./internal/controller/ -count=1`

Expected: PASS. Roughly 85 s.

- [ ] **Step 12: Commit**

```bash
git add internal/controller/numbers.go internal/controller/numbers_test.go \
  internal/controller/servergroup_controller.go
git commit -m "$(cat <<'EOF'
feat(controller): the group hands each server the lowest free number

A player on a hub cannot say which hub they are on. The group now gives
every server it creates the lowest number from 1 upwards that none of its
live servers holds, counting the creates it has issued and not yet seen.

Lowest free rather than highest plus one: a group that scales up and down
all day keeps its numbers short instead of counting into three digits.
The cost is that a number returns once its server is gone, which
ServerInfo.incarnation is what tells apart.

A persistent server's number is its ordinal, so the number a person reads
agrees with the name the server already has.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
```

---

### Task 4: The number goes on the wire

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto` (`message ServerState`, after `string incarnation = 9;`)
- Modify: `internal/netstate/netstate.go:152-167`
- Test: `internal/netstate/netstate_test.go`
- Generated, committed: `internal/agentpb/`, `agent/common/src/proto/java/`

**Interfaces:**
- Consumes: `spawneryv1alpha1.ServerSpec.Number` from Task 1.
- Produces: `agentpb.ServerState.Number int32` (field 10), reachable in Kotlin as `it.number`.

- [ ] **Step 1: Write the failing test**

The fixture is the package's own: `source(t, objects...)` builds a `netstate.Source` over a fake client holding those objects, `ephemeralGroup(ns, name)` makes a group and `readyServer(ns, name, group, players, slots)` a server. `TestBuildSaysWhichRunOfAServerThisIs` (`internal/netstate/netstate_test.go:291`) is the shape being followed.

Append to `internal/netstate/netstate_test.go`:

```go
func TestBuildCarriesAServersNumber(t *testing.T) {
	// From the spec, like a group's display name: the group decided this once,
	// when it created the server, and nothing observes it afterwards.
	numbered := readyServer("ns", "hub-dvjk", "hub", 3, 100)
	numbered.Spec.Number = 1
	// A server from before the field existed. Zero is what every reader falls
	// back on, so the operator carries it rather than inventing one.
	old := readyServer("ns", "hub-old1", "hub", 0, 100)
	src, _ := source(t, ephemeralGroup("ns", "hub"), numbered, old)

	got, err := src.Build(context.Background(), "ns")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	numbers := map[string]int32{}
	for _, s := range got.GetServers() {
		numbers[s.GetName()] = s.GetNumber()
	}
	if numbers["hub-dvjk"] != 1 {
		t.Errorf("hub-dvjk = %d, want 1", numbers["hub-dvjk"])
	}
	if numbers["hub-old1"] != 0 {
		t.Errorf("hub-old1 = %d, want 0", numbers["hub-old1"])
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/netstate/ -run TestBuildCarriesAServersNumber -count=1`

Expected: FAIL — `got.GetServers()[0].GetNumber` undefined.

- [ ] **Step 3: Add the proto field**

In `proto/spawnery/agent/v1alpha1/agent.proto`, inside `message ServerState`, after `string incarnation = 9;`:

```proto
  // Which of its group's servers this is, counted the way a person counts:
  // the second hub is 2. Stable for as long as the server exists, and given
  // out again only after it is gone -- two servers that were both "Hub-2" at
  // different times are told apart by incarnation, not by this.
  //
  // 0 for a server the operator never numbered, which is every server that
  // was already running when this field arrived. A reader showing this to a
  // player falls back to the group's own name for those.
  //
  // A persistent server reports its ordinal here, so the number agrees with
  // the name it already has. That is why these start at 0 where an ephemeral
  // group's start at 1.
  int32 number = 10;
```

- [ ] **Step 4: Regenerate**

Run: `nix develop -c make proto`

Then confirm both sides moved:

```bash
git status --short internal/agentpb agent/common/src/proto/java
```

Expected: both directories show modified files.

- [ ] **Step 5: Fill it in netstate**

In `internal/netstate/netstate.go`, in the `agentpb.ServerState` literal, after `Incarnation: srv.Status.PodUID,`:

```go
			// From the spec and not the status: the group decided this when it
			// created the server, and nothing observes it afterwards.
			Number: srv.Spec.Number,
```

- [ ] **Step 6: Run the tests and watch them pass**

Run: `nix develop -c go test ./internal/netstate/ ./internal/agentpb/ -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add proto internal/agentpb agent/common/src/proto/java \
  internal/netstate/netstate.go internal/netstate/netstate_test.go
git commit -m "$(cat <<'EOF'
feat(netstate): the server number travels to the agents

ServerState gains number: which of its group's servers this is, as a
person counts. Zero for a server nobody numbered, which is every server
that predates the field -- a reader showing it to a player falls back to
the group's own name for those.

From spec.number and not from a status field: the group decided this once
when it created the server, and nothing observes it afterwards.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
```

---

### Task 5: The plugin API carries the number

**Files:**
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/ServerInfo.java:54-64` (the record header and its javadoc)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/NetworkMirror.kt:70-86`
- Test: `agent/api/src/test/java/cloud/spawnery/agent/api/ValueTypesTest.java:33,34,55,88`

**Interfaces:**
- Consumes: `agentpb.ServerState.Number` from Task 4 (`it.number` in Kotlin).
- Produces: `ServerInfo(String name, String group, ServerPhase phase, int players, int slots, boolean registered, String state, Map<String,String> attributes, String incarnation, int number)` and its accessor `int number()`.

**The component goes last.** Appending keeps the diff to one argument at each call site; inserting it beside `name` would silently change the meaning of every positional argument after it in code that still compiles.

- [ ] **Step 1: Write the failing test**

In `agent/api/src/test/java/cloud/spawnery/agent/api/ValueTypesTest.java`, add:

```java
    @Test
    void aServerCarriesTheNumberAPersonReadsItBy() {
        var second = new ServerInfo("hub-pgqg", "hub", ServerPhase.READY, 0, 100, true, "", Map.of(), "pod-2", 2);

        assertEquals(2, second.number());
    }

    @Test
    void aServerNobodyNumberedCarriesZero() {
        var old = new ServerInfo("hub-dvjk", "hub", ServerPhase.READY, 0, 100, true, "", Map.of(), "pod-1", 0);

        assertEquals(0, old.number());
    }
```

- [ ] **Step 2: Run it and watch it fail**

Run: `git add -A && nix develop -c make agent`

Expected: FAIL — the constructor takes nine arguments, not ten. **`git add` first is not optional:** Nix builds read the git index, so an untracked or unstaged change does not exist for `make agent`, and the symptom is a compile error naming a symbol that is plainly in the file.

- [ ] **Step 3: Add the component**

In `ServerInfo.java`, add to the class javadoc, after the `incarnation` paragraph:

```java
 * @param number which of its group's servers this is, counted the way a person
 *     counts: the second hub is 2. Stable for as long as the server exists.
 *     <p>Given out again once this server is gone, so two servers that were
 *     both "Hub-2" at different times are told apart by {@link #incarnation()}
 *     and never by this.
 *     <p>0 for a server nobody numbered, which is every server that was
 *     already running when this arrived. Show the group's own name for those
 *     rather than a zero.
```

and change the record header:

```java
public record ServerInfo(
        String name,
        String group,
        ServerPhase phase,
        int players,
        int slots,
        boolean registered,
        String state,
        Map<String, String> attributes,
        String incarnation,
        int number) {
```

The compact constructor is unchanged: an `int` has no null to guard, and a negative number cannot reach here because the CRD's own `Minimum=0` refuses one.

- [ ] **Step 4: Fix the four existing call sites**

In `ValueTypesTest.java`, append `, 1` to lines 33 and 34 (they must stay equal for the `equals` test), and `, 0` to lines 55 and 88.

- [ ] **Step 5: Fill it in the mirror**

In `NetworkMirror.kt`, in the `ServerInfo(` construction, after `it.incarnation,`:

```kotlin
                    it.number,
```

- [ ] **Step 6: Build the agents and watch them pass**

Run: `git add -A && nix develop -c make agent`

Expected: PASS, both plugins and their JUnit suites. If a previous hand-run left a `GradleDaemon` resident this can die with "Gradle daemon disappeared unexpectedly" and no compiler diagnostic — stop the daemons and rerun.

- [ ] **Step 7: Commit**

```bash
git add agent/api agent/common
git commit -m "$(cat <<'EOF'
feat(agent): ServerInfo carries the number a person reads

A plugin rendering a server name for a player had only the group's
display name, which reads the same for every server of the group, or the
pod name, which nobody can say out loud. ServerInfo.number is the short
number between them.

The component goes last. Appending keeps each call site to one added
argument; inserting it beside name would silently change the meaning of
every positional argument after it in code that still compiles. Four test
call sites move, and they are the only ones: no plugin constructs this.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
```

---

### Task 6: The release

**Files:**
- Modify: `flake.nix:248` (`imageVersion`), `flake.nix:327` (`operatorVersion`)
- Modify: `charts/spawnery/Chart.yaml:11` (`version`), `:61` (`appVersion`)
- Modify: `charts/spawnery/values.yaml:22` (`tag`)
- Modify: `README.md:106`, `charts/spawnery/README.md:11` (the `--version` in the install instructions)

**Interfaces:**
- Consumes: everything from Tasks 1-5.
- Produces: `cloud.spawnery:spawnery-api:0.2.27` on Maven Central, which Task 7 and Task 8 compile against.

**Why all three numbers move.** `operatorVersion` because the reconciler changed. The chart because the `Server` CRD lives in `charts/spawnery/templates/crds.yaml`, and `charts/` moving is what moves `Chart.yaml`'s `version`. `imageVersion` because the agent's Java API changed — and `nix/agents.nix:59` passes it as `-PagentVersion`, so it is also the version `spawnery-api` is published under.

- [ ] **Step 1: Move the four numbers and their four echoes**

```bash
sed -i 's/imageVersion = "0.2.26"/imageVersion = "0.2.27"/' flake.nix
sed -i 's/operatorVersion = "0.2.25"/operatorVersion = "0.2.26"/' flake.nix
sed -i 's/^version: 0.2.25$/version: 0.2.26/' charts/spawnery/Chart.yaml
sed -i 's/^appVersion: "0.2.25"$/appVersion: "0.2.26"/' charts/spawnery/Chart.yaml
sed -i 's/tag: "0.2.25"/tag: "0.2.26"/' charts/spawnery/values.yaml
sed -i 's/--version 0.2.25/--version 0.2.26/' README.md charts/spawnery/README.md
```

- [ ] **Step 2: Run the whole suite**

Run: `nix develop -c make test 2>&1 | tail -40`

Expected: PASS. Four `internal/rbacaudit` tests read these numbers out of the files and fail by name if one of the six edits was missed — `TestTheChartAgreesWithTheFlakeAboutTheOperatorRelease` and `TestTheInstallInstructionsNameTheChartVersion` are the two that catch a stale README.

- [ ] **Step 3: Commit and push the branch**

```bash
git add flake.nix charts README.md
git commit -m "$(cat <<'EOF'
chore: 0.2.27, every server carries a number a person can read

The operator hands each server the lowest free number of its group, the
agent carries it, and a plugin can render "Hub-2" where it had either a
name that reads the same for every server of the group or a pod name
nobody can say out loud.

All three numbers move. operatorVersion because the reconciler changed;
the chart because the Server CRD lives in its templates; imageVersion
because the agent's Java API changed, which is also the version
cloud.spawnery:spawnery-api is published under.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
git push -u origin feat/server-numbers
```

- [ ] **Step 4: Open the PR, merge it, and check CI on master**

```bash
gh pr create --base master --title "feat: every server carries a number a person can read" --body "$(cat <<'EOF'
The operator hands each server the lowest free number within its group and
stores it in `Server.spec.number`; `internal/netstate` publishes it as
`ServerState.number`; `ServerInfo` gains a `number()` a plugin can read.

Server names keep their random suffix. The suffix is what makes a duplicate
create a surplus rather than a collision, in three separate places, and
numbering the names would move every ephemeral group onto the code path
written for identities.

Spec: `docs/superpowers/specs/2026-09-06-server-numbers-design.md`

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
```

After merging, **check CI on master before tagging** — `make test` never goes through Nix, and the `e2e` job is where a `go.mod` drift or a stale `vendorHash` shows:

```bash
gh run list --workflow=ci.yml --limit 3
```

- [ ] **Step 5: Tag the release**

```bash
git checkout master && git pull
git tag -s v0.2.27 -m "v0.2.27"
git push origin v0.2.27
gh run watch --workflow=release.yml
```

Expected: the images and `spawnery-api:0.2.27` publish; the operator image and chart publish at their own numbers. Confirm `spawnery-api` reached Central before starting Task 7 — the cyperia builds resolve it from there.

---

### Task 7: The tab list numbers a group that asks for it

**Files:**
- Modify: `~/git/cyperia/essentials/gradle.properties:9` (`spawneryApiVersion`)
- Modify: `~/git/cyperia/essentials/velocity/src/main/java/net/codingarea/essentials/velocity/tablist/ServerDisplayNames.java`
- Test: `~/git/cyperia/essentials/velocity/src/test/java/net/codingarea/essentials/velocity/tablist/ServerDisplayNamesTest.java`

**Interfaces:**
- Consumes: `ServerInfo.number()` and `Group.attributes()` from `spawnery-api:0.2.27`.
- Produces: nothing other tasks read.

**The attribute is `tablist: numbered`.** A key that names the surface it governs, so nobody reads it as switching numbering off everywhere — the selector in Task 8 numbers regardless.

- [ ] **Step 1: Bump the API and fix the existing test helper**

```bash
sed -i 's/^spawneryApiVersion=.*/spawneryApiVersion=0.2.27/' ~/git/cyperia/essentials/gradle.properties
```

In `ServerDisplayNamesTest.java`, the `server` helper gains the number:

```java
  private static ServerInfo server(String name, String group) {
    return server(name, group, 0);
  }

  private static ServerInfo server(String name, String group, int number) {
    return new ServerInfo(name, group, ServerPhase.READY, 0, 20, true, "", Map.of(), "1", number);
  }
```

and the `group` helper gains attributes:

```java
  private static Group group(String name, String displayName) {
    return new Group(name, Group.Kind.EPHEMERAL, 1, 1, 0, 20, Map.of(), displayName);
  }

  private static Group numberedGroup(String name, String displayName) {
    return new Group(name, Group.Kind.EPHEMERAL, 1, 1, 0, 20, Map.of("tablist", "numbered"), displayName);
  }
```

- [ ] **Step 2: Write the failing tests**

Append to `ServerDisplayNamesTest.java`:

```java
  @Test
  void aGroupThatAsksToBeNumberedReadsItsNumber() {
    assertEquals("Hub-2",
      of(List.of(server("hub-dvjk", "hub", 2)), List.of(numberedGroup("hub", "Hub"))).of("hub-dvjk"));
  }

  @Test
  void twoServersOfANumberedGroupReadDifferently() {
    ServerDisplayNames names = of(
      List.of(server("hub-dvjk", "hub", 1), server("hub-pgqg", "hub", 2)),
      List.of(numberedGroup("hub", "Hub")));
    assertEquals("Hub-1", names.of("hub-dvjk"));
    assertEquals("Hub-2", names.of("hub-pgqg"));
  }

  @Test
  void aGroupThatDidNotAskKeepsReadingTheSameForEveryServer() {
    // The game modes are this case: a player there is in the round, not on a
    // server, and a number in the tab list would be noise.
    ServerDisplayNames names = of(
      List.of(server("oneblockrace-solo-vz3g", "oneblockrace-solo", 1)),
      List.of(group("oneblockrace-solo", "OneBlockRace-Solo")));
    assertEquals("OneBlockRace-Solo", names.of("oneblockrace-solo-vz3g"));
  }

  @Test
  void aServerNobodyNumberedReadsTheGroupsName() {
    // Every server that was running when the field arrived. Nothing backfills
    // them, and "Hub-0" would be worse than "Hub".
    assertEquals("Hub",
      of(List.of(server("hub-dvjk", "hub", 0)), List.of(numberedGroup("hub", "Hub"))).of("hub-dvjk"));
  }
```

- [ ] **Step 3: Run them and watch them fail**

Run: `cd ~/git/cyperia/essentials && cadev test` (mirror the flags cadev uses for this project; never invoke `./gradlew` bare).

Expected: FAIL — `Hub` where `Hub-2` was wanted.

- [ ] **Step 4: Implement**

In `ServerDisplayNames.java`, replace the `of` method and add one constant and one helper:

```java
  /**
   * The group attribute that asks for numbers. It names the surface it governs
   * on purpose: the server selector numbers whatever it lists, because telling
   * siblings apart is the entire job of an entry there.
   */
  static final String TABLIST_ATTRIBUTE = "tablist";
  static final String NUMBERED = "numbered";

  public String of(String registeredName) {
    if (registeredName == null || registeredName.isBlank()) {
      return registeredName;
    }
    try {
      Optional<ServerInfo> server = first(network.servers(), s -> registeredName.equals(s.name()));
      Optional<Group> group = server.flatMap(s -> first(network.groups(), g -> g.name().equals(s.group())));
      return group
        .map(Group::displayName)
        .filter(name -> !name.isBlank())
        .map(name -> numbered(name, group.get(), server.get()))
        .orElse(registeredName);
    } catch (Exception | LinkageError absent) {
      // No agent on this proxy: the registered name is the best there is.
      return registeredName;
    }
  }

  private static String numbered(String displayName, Group group, ServerInfo server) {
    if (server.number() <= 0 || !NUMBERED.equals(group.attributes().get(TABLIST_ATTRIBUTE))) {
      return displayName;
    }
    return displayName + "-" + server.number();
  }
```

Delete the now-unused private `group(String)` method.

- [ ] **Step 5: Run them and watch them pass**

Run: `cd ~/git/cyperia/essentials && cadev test`

Expected: PASS, including the five tests that were there before.

- [ ] **Step 6: Commit on a new branch**

OneDev refuses updates to existing branches (it reports it as a non-fast-forward, which it is not), so push a new branch and target `staging`.

```bash
cd ~/git/cyperia/essentials
git checkout -b feat/tablist-server-numbers
git add gradle.properties velocity/src
git commit -m "$(cat <<'EOF'
feat(tablist): a numbered group reads its server's number

Every server of a group read the same name in the tab list, so a player
on a hub could not say which hub they were on. A group carrying the
attribute tablist=numbered now reads "Hub-2" instead.

Only a group that asks. A player on a game mode is there for the round
and not for the server, and a number there would be noise. A server the
operator never numbered keeps reading the group's name, because "Hub-0"
is worse than "Hub".

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
```

---

### Task 8: The selector shows the number instead of the pod name

**Files:**
- Modify: `~/git/cyperia/lobby/gradle.properties:6` (`spawneryApiVersion`)
- Modify: `~/git/cyperia/lobby/common/src/main/java/net/codingarea/lobby/common/selector/data/SelectableServerData.java`
- Modify: `~/git/cyperia/lobby/common/src/main/java/net/codingarea/lobby/common/selector/data/SpawnerySelectorDataSource.java:78-87`
- Modify: `~/git/cyperia/lobby/common/src/main/java/net/codingarea/lobby/common/selector/menu/ServerSelectorMenu.java:99`
- Modify: `~/git/cyperia/lobby/common/src/main/java/net/codingarea/lobby/common/selector/data/CloudNetSelectorDataSource.java:47`
- Test: `~/git/cyperia/lobby/common/src/test/java/net/codingarea/lobby/common/selector/data/SpawnerySelectorDataSourceTest.java`
- Test: `~/git/cyperia/lobby/common/src/test/java/net/codingarea/lobby/common/selector/data/SelectorRankingTest.java:14,20`

**Interfaces:**
- Consumes: `ServerInfo.number()` from `spawnery-api:0.2.27`.
- Produces: `SelectableServerData.displayName()`.

**Two fields, not one.** `serverName()` is the route — `sendToServer`, `SelectorRanking.isJoinable` and `QuickJoinCommand` all compare it — and `displayName()` is the label. Nothing that routes may read the label.

- [ ] **Step 1: Bump the API and fix the existing test helper**

```bash
sed -i 's/^spawneryApiVersion=.*/spawneryApiVersion=0.2.27/' ~/git/cyperia/lobby/gradle.properties
```

In `SpawnerySelectorDataSourceTest.java`:

```java
  private static ServerInfo server(String name, String group, ServerPhase phase, int players, int slots,
                                   String state, Map<String, String> attributes) {
    return server(name, group, phase, players, slots, state, attributes, 0);
  }

  private static ServerInfo server(String name, String group, ServerPhase phase, int players, int slots,
                                   String state, Map<String, String> attributes, int number) {
    return new ServerInfo(name, group, phase, players, slots, true, state, attributes, "pod", number);
  }
```

and give the two bingo-team servers numbers, so the display test has siblings to tell apart:

```java
    server("bingo-team-c3d4", "bingo-team", ServerPhase.READY, 8, 16, ServiceStates.IN_GAME, Map.of(), 1),
    server("bingo-team-e5f6", "bingo-team", ServerPhase.STARTING, 0, 16, "", Map.of(), 2),
```

and give the solo one a number too:

```java
    server("bingo-solo-a1b2", "bingo-solo", ServerPhase.READY, 3, 16, ServiceStates.LOBBY, ARCADIA, 1),
```

- [ ] **Step 2: Write the failing tests**

Append to `SpawnerySelectorDataSourceTest.java`:

```java
  @Test
  void anEntryIsLabelledByTheGroupAndTheNumber() {
    SelectableServerData data = entry("Bingo", "bingo-solo-a1b2").orElseThrow();

    assertEquals("Bingo-Solo-1", data.displayName());
  }

  @Test
  void theRouteStaysThePodName() {
    // Two fields and not one: sendToServer, SelectorRanking and
    // QuickJoinCommand all compare serverName, and none of them may meet a
    // label.
    SelectableServerData data = entry("Bingo", "bingo-solo-a1b2").orElseThrow();

    assertEquals("bingo-solo-a1b2", data.serverName());
  }

  @Test
  void anUnnumberedServerIsLabelledByItsPodName() {
    // Nothing backfills a server that was running when the field arrived, and
    // "Bingo-Solo-0" would name a server that does not exist.
    SpawnerySelectorDataSource source = new SpawnerySelectorDataSource(
      new SpawnerySelectorDataSource.Network() {
        @Override
        public List<Group> groups() { return List.of(group("bingo-solo", "Bingo-Solo", Map.of("game", "Bingo"))); }

        @Override
        public List<ServerInfo> servers() {
          return List.of(server("bingo-solo-old1", "bingo-solo", ServerPhase.READY, 0, 16, "", Map.of()));
        }
      });

    assertEquals("bingo-solo-old1", source.getSelectorEntries("Bingo")[0].displayName());
  }
```

- [ ] **Step 3: Run them and watch them fail**

Run: `cd ~/git/cyperia/lobby && cadev test`

Expected: FAIL to compile — `cannot find symbol: method displayName()`.

- [ ] **Step 4: Implement**

In `SelectableServerData.java`, add the component after `serverName` and document it:

```java
/**
 * @param displayName what a player reads on the entry: the group's name and the server's number,
 *     "Bingo-Solo-1". Never the route -- {@link #serverName()} is that, and everything which
 *     connects, ranks or quick-joins compares it.
 * @param task {@code Bingo-Solo} for every solo bingo server -- what
 *     {@link QuickJoinMode#matchesTask} compares against.
 */
public record SelectableServerData(String serverName, String displayName, String task, int playerLimit,
    int playerCount, int maxOnlineUsers, Optional<Integer> minPlayers, String permission, boolean ingame,
    Optional<Boolean> starting, Optional<Boolean> allowSpectators, Optional<Integer> teamSize,
    Optional<Integer> amountOfTeams) {
```

In `SpawnerySelectorDataSource.describe`, replace the return with:

```java
    return new SelectableServerData(server.name(), label(server, group), group.displayName(), playerLimit,
      server.players(), server.slots(), minPlayers, permission, ingame, starting, allowSpectators, teamSize,
      amountOfTeams);
  }

  // The selector lists the servers of one group side by side, so it numbers
  // whatever it lists and reads no group attribute: a switch that turned this
  // off would only ever be wrong here.
  private static String label(ServerInfo server, Group group) {
    if (server.number() <= 0 || group.displayName().isBlank()) {
      return server.name();
    }
    return group.displayName() + "-" + server.number();
  }
```

In `ServerSelectorMenu.java:99`:

```java
      .name(data.displayName());
```

Two other places construct this record and both take the argument:

`CloudNetSelectorDataSource.java:47` — under CloudNET the service name already was what a player read, which is the whole reason `ServerDisplayNames` exists on the Spawnery side. So the label is the name:

```java
        return new SelectableServerData(serverName, serverName, task, playerLimit, playerCount, maxOnlineUsers, minPlayers, permission, ingame, starting, allowSpectators, teamSize, amountOfTeams);
```

`SelectorRankingTest.java:14` and `:20` — the ranking never reads the label, so the two helpers repeat the name they already pass:

```java
    return new SelectableServerData("Bingo-Solo-1", "Bingo-Solo-1", "Bingo-Solo", playerLimit, playerCount, playerLimit,
        Optional.empty(), null, ingame, starting, allowSpectators, Optional.empty(), Optional.empty());
```

```java
    return new SelectableServerData("Bingo-Team-1", "Bingo-Team-1", "Bingo-Team", playerLimit, playerCount, playerLimit,
        Optional.empty(), null, false, Optional.empty(), Optional.empty(), Optional.of(teamSize),
        Optional.of(amountOfTeams));
```

- [ ] **Step 5: Run them and watch them pass**

Run: `cd ~/git/cyperia/lobby && cadev test`

Expected: PASS, including every test that was there before — `serverName()` did not move, so the existing assertions hold unchanged.

- [ ] **Step 6: Commit on a new branch**

```bash
cd ~/git/cyperia/lobby
git checkout -b feat/selector-server-numbers
git add gradle.properties common/src
git commit -m "$(cat <<'EOF'
feat(selector): an entry is labelled by the group and the number

The selector named each entry by the pod, "oneblockrace-solo-vz3g", which
nobody can say out loud. It now reads "Bingo-Solo-1".

Two fields and not one: serverName stays the route, which sendToServer,
SelectorRanking and QuickJoinCommand all compare, and displayName is the
label. The selector reads no group attribute -- it lists the servers of
one group side by side, so telling them apart is the entire job of an
entry, and a switch for that would only ever be wrong.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
```

---

### Task 9: The hub asks for numbers, and the images move

**Files:**
- Modify: `~/git/cyperia/configs/spawnery/base/groups/hub.yaml:14-16` (the `attributes` block)
- Modify: `~/git/cyperia/configs/spawnery/base/groups/*.yaml` (the `image:` line of all fourteen groups)

**Interfaces:**
- Consumes: the attribute key `tablist: numbered` read by Task 7; the images published by Task 6.
- Produces: nothing.

**Order.** The attribute is inert until the plugins that read it are deployed, so it may travel with the images or after them. The images must not move before Task 6's release finished publishing them.

- [ ] **Step 1: Ask for numbers on the hub, and nowhere else**

In `spawnery/base/groups/hub.yaml`, extend the `attributes` block:

```yaml
  attributes:
    game: Hub
    # Die Tablist haengt an dieses Gruppen-Anzeigenamen die Servernummer an,
    # damit ein Spieler sagen kann, auf welchem Hub er ist. Die Spielmodi
    # tragen es nicht: dort ist man in der Runde und nicht auf dem Server.
    tablist: numbered
```

The surrounding comments in this file are German and stay German; a new comment in a wholly German file is written German to match.

- [ ] **Step 2: Move the fourteen images**

```bash
cd ~/git/cyperia/configs
sed -i 's/:26\.2-0\.2\.26$/:26.2-0.2.27/; s/:3\.5\.1-0\.2\.26$/:3.5.1-0.2.27/' spawnery/base/groups/*.yaml
grep -rn "image:" spawnery/base/groups/*.yaml
```

Expected: thirteen `purpur:26.2-0.2.27` and one `velocity:3.5.1-0.2.27`.

- [ ] **Step 3: Commit on a new branch and open the PR against `staging`**

```bash
git checkout -b chore/images-0-2-27
git add spawnery/base/groups
git commit -m "$(cat <<'EOF'
chore: 0.2.27, and the hub asks for numbered servers in the tab list

The agent now carries each server's number, so a group can ask the tab
list to append it. The hub asks; the game modes do not, because a player
there is in the round and not on a server.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01FbwDaFeEUAXku6iLfgp8fZ
EOF
)"
```

OneDev's suggested PR URL defaults to `target=…:production`; the right target is `staging`.

- [ ] **Step 4: Watch the roll**

Once the PR is merged and the assembly has run, the group manifests change, `spawnery-sync` stamps the changed groups, and the operator rolls them one at a time. Confirm on the cluster:

```bash
kubectl -n minecraft get servers -o custom-columns=NAME:.metadata.name,NUMBER:.spec.number,PHASE:.status.phase
```

Expected: every server created after the roll carries a number; the hub's are 1, 2, 3 with no repeats.

---

## Verification of the whole thing

- [ ] Join the network and read the tab list on a hub: it says `Hub-1` or `Hub-2`, not `Hub`.
- [ ] Join a game-mode server and read the tab list: it says `OneBlockRace-Solo`, unchanged and unnumbered.
- [ ] Open the server selector for a game mode: the entries read `OneBlockRace-Solo-1`, `-2`, `-3` and clicking one still connects.
- [ ] Scale a hub down and up and read the tab list again: the surviving hub keeps its number, and the new one takes the freed one rather than the next one up.
