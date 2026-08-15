# Milestone 5a: persistent groups exist — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `ServerGroup` of type `Persistent` produces servers with stable
ordinal names, each on its own retained `PersistentVolumeClaim`, in both
directions of `spec.replicas`.

**Architecture:** The ordinal is the identity. A persistent server is named
`<group>-<ordinal>`, which makes its claim name `<group>-<ordinal>-data`
stable across every deletion and recreation — that stability is what makes a
world survive. A second pure sizing rule stands beside the slot rule and shares
its decision type; the Server controller creates the claim before the pod, with
no owner reference, so it outlives everything.

**Tech Stack:** Go, controller-runtime, envtest, kind + podman for the evidence
run.

**Spec:** `docs/superpowers/specs/2026-08-15-persistent-groups-design.md` — read
it alongside this plan; the plan argues from it and does not repeat its
reasoning.

## Global Constraints

- **A persistent server is named `<group>-<ordinal>`**, and `spec.ordinal`
  carries the number it was built from. Ephemeral naming (`NewServerName`, a
  random suffix) is untouched.
- **Gaps are filled, not skipped.** A missing ordinal below `replicas` is
  recreated at its own number.
- **An ordinal counts as taken while any server carrying it exists**, whatever
  phase that server is in. Building a second server on a claim the first still
  mounts would deadlock on a `ReadWriteOnce` volume rather than fail cleanly.
- **Missing ordinals are created lowest-first; surplus ordinals are removed
  highest-first.**
- **The claim has no owner reference.** It outlives the server, the group and
  the operator who deletes the wrong object. This is the one decision here
  where being wrong destroys user data.
- **The claim is created, never updated.** An existing claim is left exactly as
  it is; growing it is 5b's.
- **No waiting for `Bound`.** The pod is created straight after the claim.
  Waiting deadlocks under `volumeBindingMode: WaitForFirstConsumer`.
- **RBAC changes need two edits**: the `+kubebuilder:rbac` marker *and* a
  matching entry in `internal/rbacaudit/required.go`, which `make test`
  cross-checks against the generated role.
- **Every task ends green:** `make test` passes before the commit.

## Standing hazards in this repository

Two, and between them they produced seventeen findings in the previous
milestone. Every task below is bound by both.

1. **A sentence whose claim outlives the code beneath it.** Before each commit,
   sweep the staged diff **case-insensitively** and with the widened word list:

   ```bash
   git diff -U0 --staged | grep -inE '\b(no|none|any|all|both|never|only|nothing|exactly one|cannot|always|every)\b'
   ```

   Re-derive every hit against the code. The case-sensitive form missed every
   sentence-initial absolute for a whole milestone, and a sentence-opening
   absolute is the highest-yield position. Then, separately, read the sentences
   **already present** in the regions you touch — a new line falsifying an old
   one is invisible to any grep over a diff. Check sentences clause by clause,
   not as sentences: rewriting one to be more precise invites a second clause
   that gets the first's confidence without its checking.
2. **A test that passes for a reason unrelated to what it names.** For every
   assertion you add, delete or invert the production line it is meant to test,
   confirm *that* assertion fails rather than an earlier one, then restore.
   Report the mutation per assertion. The previous milestone produced five such
   tests and mutation found all five; reading found none.

---

### Task 1: Ordinal names, and `replicas` required

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (the `ServerGroupSpec` CEL block)
- Create: `internal/controller/persistent.go`
- Create: `internal/controller/persistent_test.go`
- Modify: `config/crd/bases/` (generated — do not hand-edit)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `func PersistentServerName(group string, ordinal int32) string`
  - `func OrdinalOf(group, server string) (int32, bool)`

- [ ] **Step 1: Write the failing test**

Create `internal/controller/persistent_test.go` with the repository's Apache
licence header (copy the 15-line block from the top of
`internal/controller/rollout.go`), then:

```go
package controller

import "testing"

func TestPersistentServerName(t *testing.T) {
	if got := PersistentServerName("survival", 0); got != "survival-0" {
		t.Errorf("PersistentServerName(survival, 0) = %q, want survival-0", got)
	}
	if got := PersistentServerName("survival", 12); got != "survival-12" {
		t.Errorf("PersistentServerName(survival, 12) = %q, want survival-12", got)
	}
}

func TestOrdinalOf(t *testing.T) {
	tests := []struct {
		name    string
		group   string
		server  string
		want    int32
		wantOK  bool
	}{
		{"the ordinary case", "survival", "survival-0", 0, true},
		{"more than one digit", "survival", "survival-12", 12, true},
		{"a different group's server", "survival", "creative-0", 0, false},
		{"an ephemeral name from the same group", "survival", "survival-a7kd", 0, false},
		{"the group name alone", "survival", "survival", 0, false},
		{"a negative number is not an ordinal", "survival", "survival--1", 0, false},
		{"a leading zero is not the same ordinal", "survival", "survival-01", 0, false},
		{"empty", "survival", "", 0, false},
		// A group whose own name ends in a number is the case that breaks a
		// naive suffix split: the boundary is the last hyphen, and everything
		// before it must equal the group exactly.
		{"a group name ending in a digit", "survival-2", "survival-2-3", 3, true},
		{"that group's own name is not one of its servers", "survival-2", "survival-2", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := OrdinalOf(tc.group, tc.server)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("OrdinalOf(%q, %q) = (%d, %v), want (%d, %v)",
					tc.group, tc.server, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
```

The leading-zero case is deliberate: `survival-01` must not read as ordinal 1,
because `PersistentServerName` would never produce that string and treating it
as ordinal 1 would let two names claim one identity.

- [ ] **Step 2: Run it and watch it fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestPersistentServerName|TestOrdinalOf' -v
```

Expected: compile failure, `undefined: PersistentServerName`.

- [ ] **Step 3: Write the implementation**

Create `internal/controller/persistent.go` with the licence header, then:

```go
package controller

import (
	"strconv"
	"strings"
)

// PersistentServerName is the name of the server holding one ordinal of a
// persistent group.
//
// The ordinal is the identity of a persistent server, and this name is how it
// is carried: podspec.DataClaimName derives the claim from the server's name,
// so this string is also what makes a world addressable across every deletion
// and recreation of the Server object. An ephemeral server is named by
// NewServerName instead, with a random suffix, because it has no identity to
// preserve.
func PersistentServerName(group string, ordinal int32) string {
	return group + "-" + strconv.Itoa(int(ordinal))
}

// OrdinalOf reads the ordinal back out of a server name, and reports whether
// the name is one PersistentServerName could have produced for this group.
//
// The boundary is the last hyphen and the prefix must equal the group exactly,
// which is what keeps a group whose own name ends in a number from reading its
// own name as an ordinal. The digits are parsed strictly: "survival-01" is
// refused rather than read as 1, because no name this package writes looks like
// that and accepting it would let two strings claim one identity.
func OrdinalOf(group, server string) (int32, bool) {
	prefix := group + "-"
	if !strings.HasPrefix(server, prefix) {
		return 0, false
	}
	digits := server[len(prefix):]
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 31)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}
```

- [ ] **Step 4: Run the tests and watch them pass**

```bash
nix develop -c go test ./internal/controller/ -run 'TestPersistentServerName|TestOrdinalOf' -v
```

Expected: PASS, both functions, twelve subtests in total.

- [ ] **Step 5: Add the CEL rule**

In `api/v1alpha1/servergroup_types.go`, add one line to the `ServerGroupSpec`
marker block, beside the three rules that already constrain the type:

```go
// +kubebuilder:validation:XValidation:rule="self.type != 'Persistent' || has(self.replicas)",message="spec.replicas is required for type Persistent"
```

Without it a persistent group with no `replicas` runs zero servers, reports
`Ready`, and does nothing — a state the API accepts and the operator cannot
explain.

- [ ] **Step 6: Regenerate and check what moved**

```bash
nix develop -c make manifests
git diff --stat config/crd/bases/
```

Expected: `servergroups.spawnery.cloud` gains the rule. If nothing moves, the
marker is in a block `controller-gen` does not scan — fix its placement rather
than editing the YAML.

- [ ] **Step 7: Run the full suite**

```bash
nix develop -c make test
```

Do not pipe it through `tail` or `head` — that makes the pipeline's exit code
the filter's.

- [ ] **Step 8: Sweep and commit**

Run both sweeps from "Standing hazards" over the staged diff, then:

```bash
git add api/v1alpha1/servergroup_types.go internal/controller/persistent.go internal/controller/persistent_test.go config/crd/bases
git commit -m "feat(5a): a persistent server is named after its ordinal"
```

---

### Task 2: `pending()` names the creates it is holding

**Files:**
- Modify: `internal/controller/expectations.go` (`pending`)
- Modify: `internal/controller/servergroup_controller.go` (`size`, the one call site)
- Test: `internal/controller/expectations_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func (e *expectations) pending(group string) (map[string]bool, map[string]bool, map[string]bool)` — creates, deletes, retires, all keyed by name.

**Context the brief cannot carry.** `pending` returns creates as an `int32`
count today, which is all the slot rule needs: it builds servers with random
names and only cares how many are in flight. The persistent rule needs to know
*which ordinals* are in flight, or it re-creates one whose create the cache has
not shown yet. Rather than adding a second accessor over the same map — two
readers of one truth is the shape this repository keeps recording the cost of —
the existing one returns the set and its single call site takes `len()`.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/expectations_test.go`, following the file's
existing style for building an `expectations` and a clock:

```go
// TestPendingNamesItsCreates is what the persistent sizing rule needs: not how
// many creates are in flight, but which ordinals they are for.
func TestPendingNamesItsCreates(t *testing.T) {
	e := newExpectations(func() time.Time { return time.Unix(0, 0) })
	e.expectCreated("ns/survival", "survival-0")
	e.expectCreated("ns/survival", "survival-2")
	e.expectDeleted("ns/survival", "survival-5")

	creates, deletes, _ := e.pending("ns/survival")
	if len(creates) != 2 || !creates["survival-0"] || !creates["survival-2"] {
		t.Fatalf("creates = %v, want survival-0 and survival-2", creates)
	}
	if len(deletes) != 1 || !deletes["survival-5"] {
		t.Fatalf("deletes = %v, want survival-5", deletes)
	}
	if creates["survival-5"] {
		t.Error("a delete reservation must not appear among the creates")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
nix develop -c go test ./internal/controller/ -run TestPendingNamesItsCreates -v
```

Expected: compile failure — `creates` is an `int32` and cannot be indexed.

- [ ] **Step 3: Change `pending`**

In `internal/controller/expectations.go`:

```go
func (e *expectations) pending(group string) (map[string]bool, map[string]bool, map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	creates := make(map[string]bool)
	deletes := make(map[string]bool)
	retires := make(map[string]bool)
	for name, exp := range e.byGroup[group] {
		switch exp.kind {
		case expectationCreate:
			creates[name] = true
		case expectationDelete:
			deletes[name] = true
		case expectationRetire:
			retires[name] = true
		}
	}
	return creates, deletes, retires
}
```

Update its doc comment to say why creates are named rather than counted: the
slot rule wants the count and takes `len()` at its call site, while the
persistent rule needs the identities, and one accessor keeps the two rules
reading the same reservations rather than two views of them.

- [ ] **Step 4: Fix the call site**

In `size()`, the `DecideSize` call takes `PendingCreates: int32(len(pendingCreates))`.
`ScalingInputs.PendingCreates` keeps its `int32` type — the slot rule is
unchanged, only the conversion moves.

- [ ] **Step 5: Run the tests**

```bash
nix develop -c go test ./internal/controller/ -run 'TestExpect|TestPending|TestDecideSize' -v
nix develop -c make test
```

Confirm from the `-v` output that the new test actually ran. A filter matching
nothing has nearly slipped through in this repository before.

If a pre-existing test fails, stop and report it rather than adjusting it — it
would mean this change moved something the plan did not intend.

- [ ] **Step 6: Sweep and commit**

```bash
git add internal/controller/expectations.go internal/controller/expectations_test.go internal/controller/servergroup_controller.go
git commit -m "feat(5a): a create reservation is held by name, not by count"
```

---

### Task 3: `DecidePersistentSize`

**Files:**
- Modify: `internal/controller/persistent.go`
- Modify: `internal/controller/scaling.go` (`SizeDecision`)
- Test: `internal/controller/persistent_test.go`

**Interfaces:**
- Consumes: `OrdinalOf` (Task 1); `pending`'s named creates (Task 2).
- Produces:
  - `type PersistentInputs struct { Group string; Replicas int32; Views []ServerView; PendingCreates, PendingDeletes map[string]bool }`
  - `func DecidePersistentSize(in PersistentInputs) SizeDecision`
  - `SizeDecision.CreateOrdinals []int32`

**A deliberate refinement of the spec's signature.** §3.2 writes
`DecidePersistentSize(replicas int32, views []ServerView)`. That cannot see the
reservations, so a create the cache has not shown yet would be issued twice
under the same name. The input becomes a struct carrying the same reservations
the slot rule reads, which is within what §3.2 asks for — "which ordinals
exist" honestly includes the ones in flight — and outside what its literal
signature could express. It also needs the group name, because an ordinal is
read out of a server name relative to its group.

- [ ] **Step 1: Write the failing tests**

Add to `internal/controller/persistent_test.go`:

```go
func view(name string, phase phase.Phase) ServerView {
	return ServerView{Name: name, Phase: phase}
}

func TestDecidePersistentSize(t *testing.T) {
	t.Run("nothing exists and three are wanted", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{Group: "survival", Replicas: 3})
		want := []int32{0, 1, 2}
		if !equalOrdinals(got.CreateOrdinals, want) {
			t.Fatalf("CreateOrdinals = %v, want %v", got.CreateOrdinals, want)
		}
		if len(got.Delete) != 0 {
			t.Fatalf("Delete = %v, want none", got.Delete)
		}
	})

	t.Run("a gap in the middle is filled at its own number", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 3,
			Views: []ServerView{view("survival-0", phase.Ready), view("survival-2", phase.Ready)},
		})
		if !equalOrdinals(got.CreateOrdinals, []int32{1}) {
			t.Fatalf("CreateOrdinals = %v, want [1]: the gap is filled, not appended to", got.CreateOrdinals)
		}
	})

	t.Run("the surplus is taken from the top", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 1,
			Views: []ServerView{
				view("survival-0", phase.Ready),
				view("survival-1", phase.Ready),
				view("survival-2", phase.Ready),
			},
		})
		want := []string{"survival-2", "survival-1"}
		if len(got.Delete) != 2 || got.Delete[0] != want[0] || got.Delete[1] != want[1] {
			t.Fatalf("Delete = %v, want %v: highest ordinal first", got.Delete, want)
		}
	})

	t.Run("an ordinal held by a leaving server is neither missing nor removed again", func(t *testing.T) {
		// survival-1 is draining. It still holds ordinal 1, so nothing may be
		// built on its claim -- and it is already going, so it must not be
		// named for deletion a second time.
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 2,
			Views: []ServerView{view("survival-0", phase.Ready), view("survival-1", phase.Draining)},
		})
		if len(got.CreateOrdinals) != 0 {
			t.Errorf("CreateOrdinals = %v, want none: ordinal 1 is still held", got.CreateOrdinals)
		}
		if len(got.Delete) != 0 {
			t.Errorf("Delete = %v, want none: survival-1 is already leaving", got.Delete)
		}
	})

	t.Run("a create already reserved is not issued twice", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 2,
			Views:          []ServerView{view("survival-0", phase.Ready)},
			PendingCreates: map[string]bool{"survival-1": true},
		})
		if len(got.CreateOrdinals) != 0 {
			t.Fatalf("CreateOrdinals = %v, want none: survival-1's create is in flight", got.CreateOrdinals)
		}
	})

	t.Run("a delete already reserved is not issued twice", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 1,
			Views:          []ServerView{view("survival-0", phase.Ready), view("survival-1", phase.Ready)},
			PendingDeletes: map[string]bool{"survival-1": true},
		})
		if len(got.Delete) != 0 {
			t.Fatalf("Delete = %v, want none: survival-1's delete is in flight", got.Delete)
		}
	})

	t.Run("replicas zero empties the group", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 0,
			Views: []ServerView{view("survival-0", phase.Ready)},
		})
		if len(got.Delete) != 1 || got.Delete[0] != "survival-0" {
			t.Fatalf("Delete = %v, want [survival-0]", got.Delete)
		}
	})

	t.Run("a server whose name is not an ordinal of this group is ignored", func(t *testing.T) {
		// A leftover from an ephemeral past, or a hand-made object. It is not
		// an ordinal, so it neither fills one nor is removed as surplus --
		// removing it would be this rule deleting something it cannot name.
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 1,
			Views: []ServerView{view("survival-a7kd", phase.Ready)},
		})
		if !equalOrdinals(got.CreateOrdinals, []int32{0}) {
			t.Errorf("CreateOrdinals = %v, want [0]", got.CreateOrdinals)
		}
		if len(got.Delete) != 0 {
			t.Errorf("Delete = %v, want none", got.Delete)
		}
	})

	t.Run("an ordinal at or above replicas is surplus even with a gap below it", func(t *testing.T) {
		got := DecidePersistentSize(PersistentInputs{
			Group: "survival", Replicas: 2,
			Views: []ServerView{view("survival-0", phase.Ready), view("survival-7", phase.Ready)},
		})
		if !equalOrdinals(got.CreateOrdinals, []int32{1}) {
			t.Errorf("CreateOrdinals = %v, want [1]", got.CreateOrdinals)
		}
		if len(got.Delete) != 1 || got.Delete[0] != "survival-7" {
			t.Errorf("Delete = %v, want [survival-7]", got.Delete)
		}
	})
}

func equalOrdinals(got, want []int32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
```

Add the `phase` import (`github.com/spawnery/spawnery/internal/phase`).

- [ ] **Step 2: Run them and watch them fail**

```bash
nix develop -c go test ./internal/controller/ -run TestDecidePersistentSize -v
```

Expected: compile failure, `undefined: DecidePersistentSize`.

- [ ] **Step 3: Add the decision field**

In `internal/controller/scaling.go`, add to `SizeDecision`:

```go
	// CreateOrdinals names the ordinals a persistent group is missing, lowest
	// first. Empty for an ephemeral group, which builds servers by count and
	// not by identity -- that is what Create carries instead.
	CreateOrdinals []int32
```

- [ ] **Step 4: Write the rule**

Append to `internal/controller/persistent.go`:

```go
// PersistentInputs is everything the persistent sizing rule may look at. It
// carries none of what ScalingInputs holds about slots, capacity, player
// counts or generations, because a persistent group is sized by a number a
// user wrote down and by nothing else.
type PersistentInputs struct {
	// Group is the group's name, which is the prefix every one of its ordinal
	// names is built from.
	Group string
	// Replicas is spec.replicas: how many ordinals this group should have.
	Replicas int32
	// Views are the group's servers.
	Views []ServerView
	// PendingCreates are the servers this reconciler has asked to create and
	// the cache has not shown yet, by name. Without them a create in flight is
	// issued a second time under the same name.
	PendingCreates map[string]bool
	// PendingDeletes are the removals it has asked for and not yet seen.
	PendingDeletes map[string]bool
}

// DecidePersistentSize decides which ordinals a persistent group is missing
// and which it has too many of.
//
// It stands beside DecideSize rather than inside it: the two share a decision
// type and nothing else. The slot rule asks what capacity the players need;
// this one asks what number the user wrote in spec.replicas. The CRD forbids
// spec.scaling on a persistent group for exactly that reason.
//
// An ordinal counts as taken while any server carrying it exists, whatever
// phase that server is in. A draining server has not released its claim, and
// building its replacement now would mean two pods mounting one ReadWriteOnce
// volume -- which does not fail cleanly, it hangs on the volume. So the
// replacement waits for the drain, bounded by spec.drain.timeoutSeconds.
//
// A server whose name is not an ordinal of this group is ignored in both
// directions. It fills no ordinal, and it is not deleted as surplus: this rule
// removes what it can name, and something it cannot name is not its to remove.
func DecidePersistentSize(in PersistentInputs) SizeDecision {
	held := make(map[int32]string, len(in.Views))
	for _, v := range in.Views {
		if ordinal, ok := OrdinalOf(in.Group, v.Name); ok {
			held[ordinal] = v.Name
		}
	}

	var decision SizeDecision
	for ordinal := int32(0); ordinal < in.Replicas; ordinal++ {
		if _, taken := held[ordinal]; taken {
			continue
		}
		if in.PendingCreates[PersistentServerName(in.Group, ordinal)] {
			continue
		}
		decision.CreateOrdinals = append(decision.CreateOrdinals, ordinal)
	}

	surplus := make([]int32, 0, len(held))
	for ordinal := range held {
		if ordinal >= in.Replicas {
			surplus = append(surplus, ordinal)
		}
	}
	sort.Slice(surplus, func(i, j int) bool { return surplus[i] > surplus[j] })
	for _, ordinal := range surplus {
		name := held[ordinal]
		if in.PendingDeletes[name] {
			continue
		}
		if viewByName(in.Views, name).leaving() {
			continue
		}
		decision.Delete = append(decision.Delete, name)
	}
	return decision
}

func viewByName(views []ServerView, name string) ServerView {
	for _, v := range views {
		if v.Name == name {
			return v
		}
	}
	return ServerView{}
}
```

Add the `sort` import.

- [ ] **Step 5: Run the tests and watch them pass**

```bash
nix develop -c go test ./internal/controller/ -run 'TestDecidePersistentSize|TestOrdinalOf|TestPersistentServerName' -v
nix develop -c make test
```

- [ ] **Step 6: Mutation-test every assertion**

In a throwaway git worktree, one at a time: drop the `PendingCreates` guard;
drop the `PendingDeletes` guard; drop the `leaving()` guard; reverse the
`surplus` sort; change the create loop to append rather than fill the gap.
Confirm each mutation fails *its own* assertion and not an earlier one, then
restore. Report the mutation per assertion.

Use a throwaway worktree rather than the shared tree: a mutant left on the
working tree is indistinguishable from the defect it imitates.

- [ ] **Step 7: Sweep and commit**

```bash
git add internal/controller/persistent.go internal/controller/persistent_test.go internal/controller/scaling.go
git commit -m "feat(5a): the persistent sizing rule, beside the slot rule"
```

---

### Task 4: `BuildDataClaim`

**Files:**
- Create: `internal/podspec/claim.go`
- Create: `internal/podspec/claim_test.go`

**Interfaces:**
- Consumes: `podspec.DataClaimName` (already exists).
- Produces: `func BuildDataClaim(group *spawneryv1alpha1.ServerGroup, srv *spawneryv1alpha1.Server) *corev1.PersistentVolumeClaim`

- [ ] **Step 1: Write the failing test**

Create `internal/podspec/claim_test.go` with the licence header. Read
`internal/podspec/server_test.go` first and reuse whatever group and server
fixtures it already builds rather than writing new ones.

```go
func TestBuildDataClaim(t *testing.T) {
	group := persistentGroupFixture(t) // 20Gi, storageClassName "longhorn", ReadWriteOnce
	srv := serverFixture(t, "survival-0")

	claim := BuildDataClaim(group, srv)

	if claim.Name != "survival-0-data" {
		t.Errorf("name = %q, want survival-0-data: the claim is named from the server, which is named from the ordinal", claim.Name)
	}
	if claim.Namespace != group.Namespace {
		t.Errorf("namespace = %q, want %q", claim.Namespace, group.Namespace)
	}
	if len(claim.OwnerReferences) != 0 {
		t.Errorf("owner references = %v, want none: the claim outlives the server, the group, and a mistaken delete", claim.OwnerReferences)
	}
	if claim.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("managed-by = %q, want %q: the operator's cache is restricted to this label", claim.Labels[LabelManagedBy], ManagedByValue)
	}
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(group.Spec.Storage.Size) != 0 {
		t.Errorf("size = %v, want %v", got, group.Spec.Storage.Size)
	}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "longhorn" {
		t.Errorf("storageClassName = %v, want longhorn", claim.Spec.StorageClassName)
	}
	if len(claim.Spec.AccessModes) != 1 || claim.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v, want [ReadWriteOnce]", claim.Spec.AccessModes)
	}
}

func TestBuildDataClaimWithoutAStorageClass(t *testing.T) {
	// storageClassName is optional: unset means the cluster's default class,
	// and a claim carrying an empty string instead would mean "no class at
	// all", which is a different and usually wrong thing.
	group := persistentGroupFixture(t)
	group.Spec.Storage.StorageClassName = nil
	claim := BuildDataClaim(group, serverFixture(t, "survival-0"))
	if claim.Spec.StorageClassName != nil {
		t.Fatalf("storageClassName = %v, want nil so the cluster default applies", claim.Spec.StorageClassName)
	}
}
```

Write the two fixtures if the file has none, and label them clearly as
persistent.

- [ ] **Step 2: Run and watch fail**

```bash
nix develop -c go test ./internal/podspec/ -run TestBuildDataClaim -v
```

- [ ] **Step 3: Write the builder**

Create `internal/podspec/claim.go` with the licence header:

```go
package podspec

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// BuildDataClaim renders the PersistentVolumeClaim a persistent server's world
// lives on.
//
// It carries no owner reference, and that is the load-bearing property rather
// than an omission. The claim outlives its server -- which is the whole point,
// since a recreated ordinal is meant to find its old world -- and it outlives
// its group, and the operator who deletes the wrong object. A StatefulSet
// retains its claims on both scale-down and deletion for the same reason. The
// cost is that claims accumulate and are removed by hand; docs/known-issues.md
// says how to find them.
//
// LabelManagedBy is not decoration: cmd/spawnery-operator restricts the
// manager's cache for claims to that label, so an unlabelled claim this
// function wrote would be invisible to the very next Get.
func BuildDataClaim(
	group *spawneryv1alpha1.ServerGroup,
	srv *spawneryv1alpha1.Server,
) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DataClaimName(srv.Name),
			Namespace: srv.Namespace,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelNetwork:   group.Spec.NetworkRef.Name,
				LabelGroup:     group.Name,
				LabelServer:    srv.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      group.Spec.Storage.AccessModes,
			StorageClassName: group.Spec.Storage.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: group.Spec.Storage.Size,
				},
			},
		},
	}
}
```

Check `corev1.VolumeResourceRequirements` against the vendored Kubernetes
version — it was `corev1.ResourceRequirements` before 1.29 — and use whichever
the tree compiles against.

- [ ] **Step 4: Run the tests**

```bash
nix develop -c go test ./internal/podspec/ -run TestBuildDataClaim -v
nix develop -c make test
```

- [ ] **Step 5: Mutation-test each assertion**

Drop the owner-reference omission by adding one; drop the label; hard-code an
empty `StorageClassName`; swap `srv.Name` for `group.Name` in the claim name.
Confirm each fails its own assertion.

- [ ] **Step 6: Sweep and commit**

```bash
git add internal/podspec/claim.go internal/podspec/claim_test.go
git commit -m "feat(5a): the claim a persistent world lives on"
```

---

### Task 5: The ServerGroup sizes a persistent group

**Files:**
- Modify: `internal/controller/servergroup_controller.go` (`size`, `createServer`, the `size` call site)
- Test: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `DecidePersistentSize`, `PersistentInputs`, `SizeDecision.CreateOrdinals` (Task 3); `PersistentServerName` (Task 1); `pending`'s named creates (Task 2).
- Produces: persistent groups that create and remove servers.

**Context the brief cannot carry.** `size()` today is called with
`mayResize := networkUsable && group.IsEphemeral()`, and returns early — doing
nothing but condemning — when that is false. Both halves of that flag must now
be separated: the Network still gates *all* sizing, but the type selects
*which rule* rather than whether to size at all.

- [ ] **Step 1: Write the failing envtest**

Add to `internal/controller/servergroup_controller_test.go`, using the
fixture's own helpers. `newFixture` builds an ephemeral group, so this test
creates its own persistent one in the same namespace.

```go
// TestAPersistentGroupBuildsItsOrdinals is the milestone's subject: a number
// in spec.replicas becomes servers with stable names.
func TestAPersistentGroupBuildsItsOrdinals(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 2) // helper added in this step

	f.reconcilePersistentGroup(t, r, "survival")

	names := f.serverNamesOfGroup(t, "survival")
	if len(names) != 2 || names[0] != "survival-0" || names[1] != "survival-1" {
		t.Fatalf("servers = %v, want [survival-0 survival-1]", names)
	}
	for _, name := range names {
		srv := f.server(name)
		ordinal, _ := OrdinalOf("survival", name)
		if srv.Spec.Ordinal == nil || *srv.Spec.Ordinal != ordinal {
			t.Errorf("%s spec.ordinal = %v, want %d", name, srv.Spec.Ordinal, ordinal)
		}
	}
}

// TestAPersistentGroupRemovesTheHighestOrdinal is the other direction, and it
// goes through the ordinary drain: deleting the Server CR is what starts it.
func TestAPersistentGroupRemovesTheHighestOrdinal(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 3)
	f.reconcilePersistentGroup(t, r, "survival")
	if got := len(f.serverNamesOfGroup(t, "survival")); got != 3 {
		t.Fatalf("servers = %d, want 3", got)
	}

	f.setPersistentReplicas(t, "survival", 2)
	f.reconcilePersistentGroup(t, r, "survival")

	srv := f.server("survival-2")
	if srv.DeletionTimestamp.IsZero() {
		t.Error("survival-2 was not deleted; the highest ordinal is the one that goes")
	}
	if !f.server("survival-0").DeletionTimestamp.IsZero() ||
		!f.server("survival-1").DeletionTimestamp.IsZero() {
		t.Error("a lower ordinal was deleted; only the surplus at the top may go")
	}
}
```

Write the three helpers (`createPersistentGroup`, `reconcilePersistentGroup`,
`setPersistentReplicas`, `serverNamesOfGroup`) beside the fixture's existing
ones, and have `serverNamesOfGroup` sort by name so the assertions can name
what they expect.

- [ ] **Step 2: Run and watch fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestAPersistentGroup' -v
```

Expected: FAIL — no servers are created at all.

- [ ] **Step 3: Split the Network gate from the type**

At the `size()` call site, `mayResize` becomes the Network question alone:

```go
	decision, err := r.size(ctx, group, views, servers, backoff, networkUsable)
```

and inside `size()`:

```go
	// The Network gates sizing of either kind: a group that cannot resolve its
	// Network cannot render a pod, so building one would only queue a failure.
	// Condemnation is not sizing and runs regardless -- see the call to
	// condemn on this path.
	if !mayResize {
		return SizeDecision{}, r.condemn(ctx, group, servers, key,
			condemned(ScalingInputs{Views: views, PendingDeletes: pendingDeletes}))
	}

	var decision SizeDecision
	if group.IsEphemeral() {
		if group.Spec.Scaling == nil {
			return SizeDecision{}, r.condemn(ctx, group, servers, key,
				condemned(ScalingInputs{Views: views, PendingDeletes: pendingDeletes}))
		}
		decision = DecideSize(ScalingInputs{ /* unchanged */ })
	} else {
		decision = DecidePersistentSize(PersistentInputs{
			Group:          group.Name,
			Replicas:       group.DesiredReplicas(),
			Views:          views,
			PendingCreates: pendingCreates,
			PendingDeletes: pendingDeletes,
		})
		decision.Condemn = condemned(ScalingInputs{Views: views, PendingDeletes: pendingDeletes})
	}
```

**Read `DecideSize` before you write that last line.** It attaches `Condemn`
itself, in a wrapper around an unexported body — so the ephemeral branch needs
no such line and the persistent one does. If you can make both attach it in one
place instead, do that: two branches that must each remember the same step is
the shape this repository has already paid for once.

- [ ] **Step 4: Create by ordinal**

Add `createPersistentServer(ctx, group, ordinal)` beside `createServer` rather
than giving `createServer` a name parameter. The two share everything but the
name and `spec.ordinal`, so factor the common object out if that reads better —
but leave the ephemeral path's own entry point alone, because it is the one
every existing test drives and this milestone has no business moving it.

```go
// createPersistentServer creates the server holding one ordinal. Unlike
// createServer's random suffix, this name is derived and stable: it is what
// makes the claim name stable, which is what makes the world survive.
func (r *ServerGroupReconciler) createPersistentServer(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	ordinal int32,
) (string, error)
```

It sets `Spec.Ordinal` to the ordinal it was given, and otherwise builds the
same object `createServer` does — same labels, same owner reference, same
`GroupGeneration`, same `ServerCreated` event.

The persistent execution loop:

```go
	if backoff.MayCreate {
		for _, ordinal := range decision.CreateOrdinals {
			name, err := r.createPersistentServer(ctx, group, ordinal)
			if err != nil {
				return decision, err
			}
			r.Expectations.expectCreated(key, name)
		}
	}
```

Keep the reservation after the create and the error return before it, matching
the loop beside it: nothing is reserved for a create that did not happen.

A create that returns `AlreadyExists` is not an error here — it means the cache
lagged and the object is already what we wanted. Treat it as success and still
reserve, or the next pass tries again forever.

- [ ] **Step 5: Run the tests**

```bash
nix develop -c go test ./internal/controller/ -run 'TestAPersistentGroup|TestDecideSize|TestServerGroup' -v
nix develop -c make test
```

- [ ] **Step 6: Pin that node drain reaches a persistent group**

The spec says condemnation needs nothing new here, and that is true of the
*code*. It is not true of the tests: nothing anywhere exercises a persistent
server on a departing node, because until this milestone no persistent server
could exist. Acceptance criterion 5 would otherwise ship untested, resting on
an argument rather than a run.

The fixture helpers already exist, added by 4c-3: `f.ensureNode(t, name,
unschedulable)` and `f.bindPodToNode(t, pod, nodeName)` in
`internal/controller/suite_test.go`. Note that `bindPodToNode` uses the
`binding` subresource because `pod.Spec.NodeName` is immutable after creation.

```go
// TestAPersistentServerOnACordonedNodeIsCondemned pins acceptance criterion 5.
// Nothing in the production code is specific to persistent groups here -- that
// is the claim, and this is what holds it.
func TestAPersistentServerOnACordonedNodeIsCondemned(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPersistentGroup(t, "survival", 1)
	f.reconcilePersistentGroup(t, r, "survival")

	srv := f.server("survival-0")
	f.markReady(t, srv.Name)
	node := f.ensureNode(t, "node-going-"+f.ns, false)
	pod, ok := f.pod(f.server(srv.Name).Status.PodName)
	if !ok {
		t.Fatal("pod not found")
	}
	f.bindPodToNode(t, pod, node.Name)
	f.ensureNode(t, node.Name, true)

	f.reconcilePersistentGroup(t, r, "survival")

	if f.server("survival-0").DeletionTimestamp.IsZero() {
		t.Fatal("the persistent server on the cordoned node was not condemned")
	}
}
```

- [ ] **Step 7: Mutation-test each new assertion**

Reverse the surplus order; drop `spec.ordinal`; make the create loop use
`NewServerName`; and for Step 6's case, uncordon the node so the condemnation
has no cause. Confirm each fails its own assertion and not an earlier one.

- [ ] **Step 8: Sweep and commit**

```bash
git add internal/controller/servergroup_controller.go internal/controller/servergroup_controller_test.go
git commit -m "feat(5a): a persistent group builds and removes its ordinals"
```

---

### Task 6: The Server controller creates the claim

**Files:**
- Modify: `internal/controller/server_controller.go` (the `persistentUnsupported` guard, the pod-creation block, the RBAC marker block)
- Modify: `internal/rbacaudit/required.go`
- Modify: `cmd/spawnery-operator/main.go` (`Cache.ByObject`)
- Modify: `config/rbac/role.yaml` (generated — do not hand-edit)
- Test: `internal/controller/server_controller_test.go`

**Interfaces:**
- Consumes: `podspec.BuildDataClaim` (Task 4).
- Produces: persistent servers with a claim and a pod.

**Context the brief cannot carry.** `server_controller.go` currently refuses to
build a pod for a persistent server on purpose: `persistentUnsupported :=
groupFound && !group.IsEphemeral()`, with the condition reason
`ReasonNotImplemented` and a message saying persistent groups arrive in
milestone 5. This task is that milestone. Remove the guard and everything that
exists only to serve it — but read what its comment says about the finalizer
first: the guard was deliberately placed *after* the deletion path, so a
persistent Server could still be deleted. Whatever you remove must not move
that ordering.

- [ ] **Step 1: Write the failing envtest**

```go
// TestAPersistentServerGetsItsClaimBeforeItsPod pins the order master design
// 6.1 asks for, and the retention that makes a world survive.
func TestAPersistentServerGetsItsClaimBeforeItsPod(t *testing.T) {
	f := newFixture(t)
	f.createPersistentGroup(t, "survival", 1)
	srv := f.createPersistentServer(t, "survival", 0)

	f.reconcile(srv.Name)

	claim := f.claim("survival-0-data")
	if claim == nil {
		t.Fatal("no claim was created; a persistent pod would reference one that does not exist and stay Pending forever")
	}
	if len(claim.OwnerReferences) != 0 {
		t.Error("the claim carries an owner reference; deleting the server would take the world with it")
	}
	if _, ok := f.pod(srv.Name); !ok {
		t.Error("no pod was created")
	}
}

// TestDeletingAPersistentServerLeavesItsClaim is the property the whole
// milestone turns on.
func TestDeletingAPersistentServerLeavesItsClaim(t *testing.T) {
	f := newFixture(t)
	f.createPersistentGroup(t, "survival", 1)
	srv := f.createPersistentServer(t, "survival", 0)
	f.reconcile(srv.Name)
	if f.claim("survival-0-data") == nil {
		t.Fatal("no claim to begin with")
	}

	if err := f.c.Delete(f.ctx, srv); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	f.reconcile(srv.Name)

	if f.claim("survival-0-data") == nil {
		t.Fatal("the claim went with the server; the world is gone")
	}
}
```

Add the `f.claim(name)` and `f.createPersistentServer` helpers.

envtest runs no provisioner, so the claim will never reach `Bound` and the pod
will never run. Neither test asserts either — they assert the objects, which is
all this layer can honestly show. The world surviving is Task 7's runbook.

- [ ] **Step 2: Run and watch fail**

```bash
nix develop -c go test ./internal/controller/ -run 'TestAPersistentServerGetsItsClaim|TestDeletingAPersistentServer' -v
```

- [ ] **Step 3: Remove the refusal**

Delete `persistentUnsupported`, its log line, its `case` in the `setAccepted`
switch, and its term in the `createPod` condition. Check whether
`spawneryv1alpha1.ReasonNotImplemented` still has another user; if it does not,
leave the constant but do not invent a new use for it.

- [ ] **Step 4: Create the claim before the pod**

Inside the `if createPod` block, before `BuildServerPod`:

```go
		if !group.IsEphemeral() {
			claim := podspec.BuildDataClaim(group, srv)
			if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
		}
```

`AlreadyExists` is the ordinary case rather than an error: a recreated ordinal
is *supposed* to find its claim, and that is the whole point of the milestone.
Nothing updates the claim — growing it is 5b's.

Do not wait for the claim to reach `Bound`. Under
`volumeBindingMode: WaitForFirstConsumer` — the default of most topology-aware
storage classes, and of the node-local ones this milestone's failure modes are
about — a volume binds only once a pod demands it, so waiting deadlocks. Say
that in a comment where the next reader will look for it.

- [ ] **Step 5: RBAC, the audit table, and the cache**

Add to the marker block in `server_controller.go`:

```go
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
```

No `delete`, no `update`: this operator never removes a world and never resizes
one in 5a. The absence is the safety property, so say so in the entry you add
to `internal/rbacaudit/required.go` — that table is hand-maintained and
`make test` fails without a matching row.

In `cmd/spawnery-operator/main.go`, add claims to the label-restricted
`Cache.ByObject` entries beside ConfigMaps and ServiceAccounts, for the reason
already written there: without it the cache holds every claim in the namespace.

- [ ] **Step 6: Regenerate and run**

```bash
nix develop -c make manifests
git diff --stat config/rbac/role.yaml
nix develop -c make test
```

- [ ] **Step 7: Mutation-test each new assertion**

Add an owner reference; skip the claim entirely; create the claim after the
pod instead of before. Confirm each fails its own assertion — and note that
the third may not be observable at this layer, in which case say so rather
than claiming coverage you do not have.

- [ ] **Step 8: Sweep and commit**

```bash
git add internal/controller/server_controller.go internal/controller/server_controller_test.go internal/rbacaudit/required.go cmd/spawnery-operator/main.go config/rbac/role.yaml
git commit -m "feat(5a): a persistent server gets its claim, and keeps it"
```

---

### Task 7: Documentation and the evidence runbook

**Files:**
- Modify: `docs/known-issues.md`
- Modify: `docs/handover-milestone-4.md` (or a new milestone-5 handover — see Step 2)
- Create: `docs/runbook-milestone-5a-evidence.md`

- [ ] **Step 1: Known issues**

A "From milestone 5a" section, one paragraph each:

- **Claims accumulate and are never removed by this operator.** How to find
  them (`kubectl get pvc -l spawnery.cloud/managed-by=spawnery`), how to tell
  a live one from an orphan (its `spawnery.cloud/server` label against the
  Servers that exist), and that deleting one deletes a world. Say that the
  operator holds no `delete` verb on claims at all, so this cannot happen by
  accident.
- **A persistent server on a node-pinned volume cannot follow a node drain.**
  4c-3 recorded this before anything could reach it; 5a is what makes it
  reachable. The replacement ordinal sits `Pending` until the node returns.
- **A claim that never binds fails the server, not the group.** The startup
  deadline expires, the server goes `Failed`, the group reports `Degraded`,
  and 4d's backoff bounds the retry. Name the events an operator will actually
  see from the scheduler and the provisioner.
- **`spec.replicas` is now required for `Persistent`.** An existing group
  without it — none can exist, since nothing could create one — would be
  rejected on its next apply.

- [ ] **Step 2: The handover**

`docs/handover-milestone-4.md` is milestone 4's record and is already long.
Start `docs/handover-milestone-5.md` instead, opening with what 5a built, what
5b and 5c find in place, and the interfaces §8 of the spec lists. Cross-link
it from milestone 4's handover so a reader arriving at the old one finds the
new.

- [ ] **Step 3: The runbook**

A new document rather than a section of 4c-1's: this measures a different
claim on a different cluster shape. It needs a single-node `kind` cluster with
its default storage class (the local-path provisioner, which `kind` ships and
which binds `WaitForFirstConsumer` claims once a pod demands them), a
persistent group at `replicas: 1`, and a licensed client.

The acceptance test is the one that cannot be argued with: **place a block,
delete the pod, rejoin, and the block is still there.** Write the steps in the
register of `docs/runbook-milestone-4c1-evidence.md` — numbered sections,
"Expect" lines that say what to look for and why, and every claim about the
code checked against the code.

Mark it **NOT YET DRIVEN**, and say that it is driven by the human partner and
the acting agent together after the branch review, the way §12 of the 4c-1
runbook was.

- [ ] **Step 4: Commit**

```bash
git add docs/known-issues.md docs/handover-milestone-5.md docs/handover-milestone-4.md docs/runbook-milestone-5a-evidence.md
git commit -m "docs(5a): what persistent groups leave behind, and how to see a world survive"
```

---

## Notes for the executor

- **`make test`'s exit code is the answer.** Never pipe it through `tail` or
  `head`; that makes the pipeline's exit code the filter's, and a failure reads
  as a pass. Redirect to a file and check `$?`.
- **envtest runs no provisioner and no kubelet.** A claim never binds there and
  a pod never runs, so anything about a world actually surviving belongs in the
  runbook and not in a test that would be green for the wrong reason.
- **`make agent-test` should need no extension** — this milestone changes no
  agent-facing message. Run it once at the end of Task 6 and report the result.
  If it *did* need extending, that is a finding worth raising rather than a
  step to improvise.
- **Two tasks touch `internal/controller/servergroup_controller.go`** (2 and 5)
  and two touch `internal/controller/persistent.go` (1 and 3). Neither pair
  overlaps in region, but read the file as it stands rather than as the plan
  describes it.
