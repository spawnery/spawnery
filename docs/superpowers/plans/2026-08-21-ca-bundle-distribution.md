# CA Bundle Distribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A game namespace's `spawnery-ca` ConfigMap tracks the operator's
current CA bundle even when no pod is ever created there.

**Architecture:** `Bootstrapper.Ensure` already does the right thing and says so
— idempotent, safe on every reconcile, rewriting `ca.crt` to the current bundle.
Only its call site is too narrow. `NetworkReconciler` gains the same
`Bootstrapper` the `ServerReconciler` holds and calls it once per reconcile of
an accepted `Network`, after the NetworkPolicy.

**Tech Stack:** Go, controller-runtime, envtest.

## Global Constraints

- The spec is `docs/superpowers/specs/2026-08-21-ca-bundle-distribution-design.md`.
  Where this plan and the spec disagree, stop and ask — do not pick one.
- Every command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`.
  To run one test, use `go test ./internal/controller/ -run <name> -v` — the
  Makefile's `test` target ignores `ARGS` and always runs everything.
- Conventional Commits with English subjects. Every commit ends with exactly:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **Never push, never merge, never create a tag.**
- **Do not give the CA ConfigMap or the ServiceAccounts an `OwnerReference`.**
  Their absence is deliberate: `Ensure`'s doc says the objects "are meant to
  outlive the operator, so a pod restarting during an operator outage still
  finds a CA to trust". A `Network` owning the ConfigMap would delete a running
  fleet's trust anchor when the `Network` is deleted.
- **Do not move or reorder `reconcileNetworkPolicy`.** Its comment states it
  runs before anything else in the reconcile because a Forbidden there is a
  security control failing to land. The new call goes after it.
- Comments explain why, not what.
- **A test that passes the moment it is written has proven nothing.** Each task
  says what its test must fail with first, and the failure goes in the commit.

---

## File Structure

| File | Change |
|---|---|
| `internal/controller/network_controller.go` | `Bootstrap` field on `NetworkReconciler`; one call after `reconcileNetworkPolicy` |
| `internal/controller/setup.go` | wire `Bootstrap` in `newNetworkReconciler`; correct `SetupAll`'s refusal message |
| `internal/controller/suite_test.go` | the fixture's `NetworkReconciler` gains a `Bootstrap` |
| `internal/controller/network_controller_test.go` | `networkReconcilerWithEvents` gains a `Bootstrap`; the new tests |
| `docs/known-issues.md` | the entry loses the half this closes |

---

## Task 1: The quiet namespace gets its bundle

**Files:**
- Modify: `internal/controller/network_controller.go:40-58` and `:107-109`
- Modify: `internal/controller/setup.go:84-86` and `:154-165`
- Modify: `internal/controller/suite_test.go:242`
- Modify: `internal/controller/network_controller_test.go:46-58`

**Interfaces:**
- Produces: `NetworkReconciler.Bootstrap *Bootstrapper`, set at every
  construction site. Task 2's tests set it too.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/network_controller_test.go`:

```go
// A namespace where nothing starts still tracks the operator's CA.
//
// Before this test's subject existed, Bootstrapper.Ensure ran only from
// ServerReconciler, on the path that creates a pod
// (server_controller.go:304). A namespace whose pods were all already
// running -- or which had none at all -- kept whatever ca.crt it was given
// the last time a pod happened to be created there, however long ago. That
// is the second half of docs/known-issues.md's "The CA has no rotation
// procedure", and it is what makes a rotation's overlap window impossible to
// close: the operator cannot tell whether a quiet namespace has the new
// bundle yet.
//
// No pod is created anywhere in this test. That is the whole point of it.
func TestAQuietNamespaceFollowsTheCABundle(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)

	ca := []byte("PEM-FIRST")
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return ca }}

	f.reconcileNetwork(t, r, f.network.Name)

	read := func() string {
		t.Helper()
		var cm corev1.ConfigMap
		key := client.ObjectKey{Namespace: f.ns, Name: podspec.CAConfigMapName}
		if err := f.c.Get(f.ctx, key, &cm); err != nil {
			t.Fatalf("read the CA ConfigMap back: %v", err)
		}
		return cm.Data[podspec.CAConfigMapKey]
	}

	if got := read(); got != string(ca) {
		t.Fatalf("ca.crt = %q after the first reconcile, want %q", got, ca)
	}

	ca = []byte("PEM-SECOND")
	f.reconcileNetwork(t, r, f.network.Name)

	if got := read(); got != string(ca) {
		t.Errorf("ca.crt = %q after the bundle changed, want %q. The namespace is quiet -- "+
			"no pod was created in it -- so nothing but the Network's own reconcile can "+
			"bring the new bundle here", got, ca)
	}
}
```

- [ ] **Step 2: Run it and record the failure**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestAQuietNamespaceFollowsTheCABundle -v`

Expected: **a compile error** — `NetworkReconciler` has no field `Bootstrap`.
That is a real red, and it is the one this task starts from. Quote it in the
commit.

If it compiles, stop and report: it would mean the field already exists and
this task's premise is wrong.

- [ ] **Step 3: Add the field**

In `internal/controller/network_controller.go`, at the end of the
`NetworkReconciler` struct:

```go
	// Bootstrap puts the CA bundle and the agent ServiceAccounts into this
	// Network's namespace. It is the same instance ServerReconciler holds:
	// that one guarantees the objects exist before the first pod needs them,
	// and this one guarantees they stay current afterwards, in a namespace
	// where no pod is being created and nothing else would call Ensure at
	// all.
	Bootstrap *Bootstrapper
```

- [ ] **Step 4: Add the call**

In `Reconcile`, immediately after the `reconcileNetworkPolicy` block ends
(`network_controller.go:107-109`) and before the `serverGroups` List:

```go
	// After the policy, not before it: the comment above that call explains
	// that it goes first because a Forbidden there is a security control
	// failing to land. Ensure can fail for a reason that has nothing to do
	// with this namespace -- the operator has started but certs.Provider has
	// not published a bundle yet -- and a namespace left unprotected because a
	// ConfigMap could not be written would be the worse trade.
	//
	// The error is returned rather than swallowed. ServerReconciler does the
	// same on the same call, so neither path invents its own meaning for an
	// unavailable CA; and a swallowed error here would leave exactly the
	// silently stale ConfigMap this call exists to prevent.
	if err := r.Bootstrap.Ensure(ctx, network.Namespace); err != nil {
		return ctrl.Result{}, fmt.Errorf("bootstrap the namespace: %w", err)
	}
```

- [ ] **Step 5: Wire every construction site**

There are three, and missing one is a nil dereference at run time rather than
a compile error.

`internal/controller/setup.go`, in `newNetworkReconciler`:

```go
		SecretReader: mgr.GetAPIReader(),
		Bootstrap:    opts.Bootstrapper,
```

`internal/controller/suite_test.go:242`, the fixture's own reconciler. The
fixture already builds a `Bootstrapper` for its `ServerReconciler` at
`suite_test.go:220-223`, with `CA: func() []byte { return []byte("test-ca") }`.
Lift it into a local variable and give it to both, so the two halves of the
fixture cannot drift apart on what the test CA is:

```go
	bootstrap := &Bootstrapper{
		Client: c, Reader: c,
		CA: func() []byte { return []byte("test-ca") },
	}
```

then `Bootstrap: bootstrap` in the `ServerReconciler` at :220 and in the
`NetworkReconciler` at :242. Without the second, every fixture-driven Network
reconcile is a nil dereference.

`internal/controller/network_controller_test.go`, in
`networkReconcilerWithEvents` — a default that every existing caller can live
with:

```go
		SecretReader: f.c,
		// Every test that reconciles a Network now bootstraps its namespace.
		// A test that cares about the bundle replaces this; the rest need it
		// only to be non-nil and to return something.
		Bootstrap: &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return []byte("PEM-FIXTURE") }},
```

- [ ] **Step 6: Correct the refusal that now names one controller too few**

`internal/controller/setup.go:84-86` refuses a nil `Bootstrapper` with "the
server controller cannot create pods without one". Two controllers need it now:

```go
	if opts.Bootstrapper == nil {
		return fmt.Errorf("no bootstrapper: the server controller cannot create pods " +
			"without one, and the network controller cannot keep a namespace's CA current")
	}
```

A refusal that names the wrong reason sends whoever hits it to the wrong
controller.

- [ ] **Step 7: Run the test, then the package**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run TestAQuietNamespaceFollowsTheCABundle -v
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/
```

Expected: the new test passes, and the whole package passes. **If other tests
fail, read them before changing them**: a Network reconcile now writes two
ServiceAccounts and a ConfigMap into the fixture's namespace, and a test that
counted objects in that namespace would notice. That is a real interaction, not
noise.

The package passing is also what covers the spec's acceptance criterion 6 —
`ServerReconciler` still calls `Ensure` on the path that creates the first pod,
unchanged. The tests that exercise that path already exist
(`internal/controller/bootstrap_test.go` and the agent-channel envtests, which
set `f.reconc.Bootstrap` themselves); this task adds no second call and removes
none. Say so in the report rather than adding a test that asserts a line was
not deleted.

- [ ] **Step 8: Commit**

```bash
git add internal/controller
git commit -m "$(cat <<'EOF'
feat(certs): a quiet namespace follows the operator's CA bundle

Bootstrapper.Ensure is idempotent, says so, and rewrites ca.crt to the current
bundle on every call. Only its call site was too narrow: ServerReconciler
calls it on the path that creates a pod, so a namespace where nothing starts
kept whatever bundle it was given the last time one did.

NetworkReconciler now calls it too, after reconcileNetworkPolicy -- after,
because that call goes first on purpose, and a CA that cannot be written must
not stop a security control from landing.

The failing test was a compile error:

  <paste it here>

Three construction sites had to learn the field; a missed one is a nil
dereference at run time rather than a compile error, which is why they are
listed in the plan. SetupAll's refusal now names both controllers that need a
Bootstrapper instead of only the one.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 2: The three decisions the spec records

**Files:**
- Modify: `internal/controller/network_controller_test.go`

**Interfaces:**
- Consumes: `NetworkReconciler.Bootstrap` from Task 1.

Each test here guards a decision that a later reader would plausibly undo.

- [ ] **Step 1: A losing Network writes nothing**

```go
// The Network that does not own its namespace bootstraps nothing.
//
// pickNamespaceOwner gives the namespace to the oldest Network, and the
// loser's reconcile returns before it writes anything. That already governed
// the NetworkPolicy; it governs the CA ConfigMap for the same reason, and
// this test exists because the new call sits close enough to the acceptance
// branch that moving it above one line would silently change that.
func TestALosingNetworkDoesNotBootstrapTheNamespace(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return []byte("PEM-A") }}

	younger := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "staging", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	if err := f.c.Create(f.ctx, younger); err != nil {
		t.Fatalf("create the younger Network: %v", err)
	}

	key := client.ObjectKey{Namespace: f.ns, Name: podspec.CAConfigMapName}
	var cm corev1.ConfigMap
	if err := f.c.Get(f.ctx, key, &cm); err == nil {
		if err := f.c.Delete(f.ctx, &cm); err != nil {
			t.Fatalf("clear the ConfigMap before the loser reconciles: %v", err)
		}
	}

	f.reconcileNetwork(t, r, younger.Name)

	if err := f.c.Get(f.ctx, key, &cm); err == nil {
		t.Error("the losing Network created the CA ConfigMap; only the namespace's owner writes here")
	}
}
```

If the fixture's own setup makes `production` the owner by a route other than
age, read `pickNamespaceOwner` and construct the loser so that it genuinely
loses — the test is worthless if `staging` happens to win.

- [ ] **Step 2: The ConfigMap still carries no owner reference**

```go
// The objects Ensure writes carry no OwnerReference, and that is deliberate:
// they are meant to outlive the operator so a pod restarting during an
// outage still finds a CA to trust. Making the Network own the ConfigMap is
// the tidy-looking change this design refused, and it would delete a running
// fleet's trust anchor the moment somebody deleted a Network. Asserted here
// because "tidy up the ownership" is exactly the kind of edit that arrives
// later with a green suite.
func TestTheCAConfigMapIsOwnedByNothing(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return []byte("PEM-A") }}

	f.reconcileNetwork(t, r, f.network.Name)

	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: f.ns, Name: podspec.CAConfigMapName}
	if err := f.c.Get(f.ctx, key, &cm); err != nil {
		t.Fatalf("read the CA ConfigMap back: %v", err)
	}
	if len(cm.OwnerReferences) != 0 {
		t.Errorf("the CA ConfigMap has %d owner reference(s): %v. It must have none — "+
			"deleting a Network would otherwise take the trust anchor of every pod still "+
			"running in the namespace with it", len(cm.OwnerReferences), cm.OwnerReferences)
	}
}
```

- [ ] **Step 3: An unavailable CA fails the reconcile**

```go
// A reconcile that runs before certs.Provider has published fails and
// requeues rather than passing quietly. Swallowing it would leave the
// silently stale ConfigMap this whole change exists to prevent, and
// ServerReconciler already treats the same call the same way.
func TestAReconcileWithoutACABundleFails(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	r.Bootstrap = &Bootstrapper{Client: f.c, Reader: f.c, CA: func() []byte { return nil }}

	_, err := r.Reconcile(f.ctx, ctrlreconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: f.ns, Name: f.network.Name},
	})
	if err == nil {
		t.Fatal("the reconcile succeeded with no CA bundle available")
	}
	if !strings.Contains(err.Error(), "bootstrap") {
		t.Errorf("error = %v, want it to name the bootstrap step", err)
	}

	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: f.ns, Name: podspec.CAConfigMapName}
	if err := f.c.Get(f.ctx, key, &cm); err == nil {
		t.Error("a ConfigMap was written despite there being no bundle to write")
	}
}
```

Note that `f.reconcileNetwork` is a helper that fails the test on error, so
this one calls `Reconcile` directly. Read the helper before assuming its
signature.

- [ ] **Step 4: Prove each of the three can fail**

A guard nobody has seen fire is a guard nobody has tested. For each, make the
smallest change that should break it, confirm it does, and undo it:

| test | mutation | expected |
|---|---|---|
| losing Network | move the `Ensure` call above the acceptance branch's early return | the loser writes the ConfigMap; test fails |
| no owner reference | add `controllerutil.SetControllerReference(network, cm, r.Scheme)` in `ensureConfigMap` | test fails naming the owner |
| unavailable CA | swallow the error — `_ = r.Bootstrap.Ensure(...)` | test fails on the nil error |

After each, restore and confirm with `git diff --stat internal/controller`.
Record all three outcomes in the report; a mutation that leaves its test green
means the test does not test what it claims.

- [ ] **Step 5: Run the package and commit**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/
git add internal/controller
git commit -m "$(cat <<'EOF'
test(certs): the three decisions the CA bootstrap rests on

A losing Network writes nothing, the ConfigMap is owned by nothing, and a
reconcile with no bundle available fails instead of passing quietly. Each
guards a decision a later reader would plausibly undo -- particularly the
ownership, where the tidy-looking change deletes a running fleet's trust
anchor.

All three were shown to fire by mutating the code they guard and watching
them go red; the mutations were removed and the diff checked back to this
task's own changes.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 3: The entry loses the half this closes

**Files:**
- Modify: `docs/known-issues.md`

- [ ] **Step 1: Rewrite the entry**

Find `**The CA has no rotation procedure.**` under "On the agent channel". Its
first half stays true and its second half does not. Replace the second half —
the sentences beginning "Even then a new CA does not reach every namespace
immediately" — with what is now the case:

```markdown
A new CA would now reach every namespace holding a `Network` without waiting
for a pod: `NetworkReconciler` calls `Bootstrapper.Ensure` on every reconcile,
so a namespace's `ca.crt` follows the operator's current bundle within one
`resyncInterval`. That was the half of this entry a rotation could not be
built on — an overlap window closes on "does every agent have the new bundle
yet", and while a quiet namespace could hold an old one indefinitely, that
question had no answer. See
`docs/superpowers/specs/2026-08-21-ca-bundle-distribution-design.md`.

What remains open is the rotation itself: nothing issues a second CA, the
bundle format's support for several concatenated PEMs is still unexercised,
and the sequencing — distribute, wait, switch the serving certificate, drop
the old — exists nowhere. The agent side is ready for it:
`Environment.Configured` holds paths rather than contents precisely so a
running agent re-reads the bundle, and `OperatorChannel.trustManager` parses a
multi-PEM one.
```

Keep the entry's first sentence and its ten-year-lifetime observation. Read
what is there before replacing it; the text above is what to say, not
necessarily where every existing sentence goes.

- [ ] **Step 2: Check the claim you just made**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/controller/ -run 'TestAQuietNamespaceFollowsTheCABundle' -v
```

The entry now asserts a behaviour. The test that proves it must be green at the
moment the claim is written — this repository has spent a day removing
documentation that described things which were not so.

- [ ] **Step 3: Run everything and commit**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make test
nix --extra-experimental-features 'nix-command flakes' develop -c make lint
git add docs/known-issues.md
git commit -m "$(cat <<'EOF'
docs(known-issues): the CA entry loses the half that is now closed

A new CA reaches every namespace holding a Network without waiting for a pod,
which was the half a rotation could not be built on: an overlap window closes
on "does every agent have the new bundle yet", and a quiet namespace could
hold an old one indefinitely.

The rotation itself stays open and the entry says what is missing -- nothing
issues a second CA, the multi-PEM bundle format is unexercised, and the
sequencing exists nowhere -- alongside what is already prepared for it on the
agent side.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## What this plan does not cover

- Issuing a second CA, the overlap, the switch, or the cleanup. That is the
  rotation, and it is the next design.
- Any change to certificate lifetimes.
- An end-to-end scenario. `hack/e2e.sh` creates pods, so its namespaces are
  never quiet and a scenario there would exercise the path that already worked.
