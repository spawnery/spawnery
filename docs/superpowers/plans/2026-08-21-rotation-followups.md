# Rotation Follow-ups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A rotation slot whose certificate does not parse never reaches an
agent, is cleaned up loudly, and leaves a durable record — and the retry that
makes the operator's secret safe for two writers finally has a test.

**Architecture:** The publication guard goes in `Bundle.PublishedCA`, the single
function whose output reaches an agent. The cleanup goes in
`Store.AdvanceRotation`, which already re-reads the secret and is the only place
that decides a rotation's phase. Nothing else moves.

**Tech Stack:** Go, controller-runtime, envtest, `client/interceptor`.

## Global Constraints

- The spec is `docs/superpowers/specs/2026-08-21-rotation-followups-design.md`.
  Where this plan and the spec disagree, stop and ask — do not pick one.
- Every command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`.
  `internal/certs` contains envtest tests; a package run takes minutes and
  `make test` takes many. Let them finish.
- **Do not run `make e2e`.** This machine has 8 GB of RAM.
- **Never run `git config` in any form.** A worktree shares `.git/config` with
  the main repository; a previous agent set an identity there and rewrote the
  author name on real commits.
- **Never push, never merge, never create a tag.**
- Conventional Commits with English subjects. Every commit ends with exactly:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **`Bundle.Validate`, `parseCA`, `parseServing`, `Issue`, `Reissue` and
  `reissueOrIssue` keep their current behaviour**, and a cluster that never
  rotates behaves exactly as before. Every pre-existing test passes untouched.
- **The guard must not run through an `Ensure` error.** `Provider.Start`
  returns that from a `Runnable` (`store.go:269-271`), so it is fatal at
  startup, and a mistyped annotation must not take the operator down.
- **Only the certificate halves are validated.** A malformed key reaches no
  agent and already fails loudly in `Reissue` → `parseCA`.
- **The envtest hazard.** `internal/testenv` starts one control plane per test
  binary and registers no cleanup, and envtest runs no kube-controller-manager,
  so namespace deletion collects nothing. Any object a test creates that
  production code lists cluster-wide must be deleted in `t.Cleanup`.
  `createNetwork` and `createManagedPod` show the pattern.
- Comments explain why, not what.
- **A test that passes the moment it is written has proven nothing.**

---

## File Structure

| File | Change |
|---|---|
| `internal/certs/bundle.go` | `PublishedCA` omits an unparseable certificate slot; a `parsableCert` helper |
| `internal/certs/rotation.go` | the cleanup step, the discarded annotation, the seventh event's call site |
| `internal/certs/events.go` | `ReasonRotationSlotDiscarded`, `actionDiscardRotationSlot`, and the reason's doc |
| `internal/certs/events_test.go` | the new action joins `certsActions` |
| `internal/certs/rotation_test.go` | Task 1's pure tests |
| `internal/certs/rotation_sequence_envtest_test.go` | Tasks 2 and 3's tests |
| `docs/superpowers/specs/2026-08-21-ca-rotation-design.md` | §4's event count and inventory |

---

## Task 1: An unparseable slot is never published

**Files:**
- Modify: `internal/certs/bundle.go:174-182`
- Test: `internal/certs/rotation_test.go` (`package certs_test`)

**Interfaces:**
- Produces, for Task 2: `func parsableCert(pemBytes []byte) error` — unexported,
  returns nil when `pemBytes` decodes to exactly one PEM block that
  `x509.ParseCertificate` accepts, and an error naming what went wrong
  otherwise. Task 2 uses the same helper so the guard and the cleanup cannot
  disagree about what "parses" means.

- [ ] **Step 1: Write the failing test**

Append to `internal/certs/rotation_test.go`:

```go
// A rotation slot the agent could not parse is never published.
//
// PublishedCA's output travels Provider.Set -> Provider.CABundle ->
// Bootstrapper.CA -> the spawnery-ca ConfigMap of every namespace, and the
// consumer is OperatorChannel.trustManager, which parses the whole bundle with
// CertificateFactory.generateCertificates and throws on anything that is not a
// certificate. So a slot that does not parse does not cost a rotation; it
// costs every agent in every namespace its entire trust store. Only a
// hand-edited secret produces one, which is why this is a guard rather than a
// repair -- the repair is AdvanceRotation's, and it runs a tick later.
//
// The guard is here, at the one function whose output reaches an agent, rather
// than at a call site: a later path that publishes the bundle from somewhere
// else is exactly how this would come back.
func TestAnUnparseableSlotIsNotPublished(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	dnsNames := certs.ServingDNSNames("spawnery-operator", "spawnery-system")

	signing, err := certs.Issue(now, dnsNames)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	good, _, err := certs.IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}

	// Two shapes, because the agent throws on both and only the first is
	// caught by pem.Decode: bytes that are not PEM at all, and a PEM envelope
	// around something that is not a certificate.
	notPEM := []byte("-- this is not a certificate --\n")
	pemButNotACert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("nonsense")})

	for _, tc := range []struct {
		name string
		bad  []byte
	}{
		{"not PEM at all", notPEM},
		{"a PEM envelope around nonsense", pemButNotACert},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := &certs.Bundle{
				CACertPEM:      signing.CACertPEM,
				CAKeyPEM:       signing.CAKeyPEM,
				ServingCertPEM: signing.ServingCertPEM,
				ServingKeyPEM:  signing.ServingKeyPEM,
				NextCACertPEM:  tc.bad,
			}
			if got := next.PublishedCA(); !bytes.Equal(got, signing.CACertPEM) {
				t.Errorf("an unparseable ca-next was published; the bundle every agent "+
					"pins would have %d bytes of it, and trustManager throws on the whole "+
					"stream rather than skipping the bad block", len(tc.bad))
			}

			prev := &certs.Bundle{
				CACertPEM:         signing.CACertPEM,
				CAKeyPEM:          signing.CAKeyPEM,
				ServingCertPEM:    signing.ServingCertPEM,
				ServingKeyPEM:     signing.ServingKeyPEM,
				PreviousCACertPEM: tc.bad,
			}
			if got := prev.PublishedCA(); !bytes.Equal(got, signing.CACertPEM) {
				t.Error("an unparseable ca-previous was published")
			}
		})
	}

	// And the good case still publishes two, so the guard has not simply
	// disabled the feature.
	ok := &certs.Bundle{
		CACertPEM:      signing.CACertPEM,
		CAKeyPEM:       signing.CAKeyPEM,
		ServingCertPEM: signing.ServingCertPEM,
		ServingKeyPEM:  signing.ServingKeyPEM,
		NextCACertPEM:  good,
	}
	if got := ok.PublishedCA(); !bytes.Equal(got, slices.Concat(signing.CACertPEM, good)) {
		t.Error("a well-formed incoming CA was dropped from the published bundle")
	}
}
```

- [ ] **Step 2: Run it and record the failure**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/ -run TestAnUnparseableSlotIsNotPublished -v`

Expected: a real red on both subtests — `PublishedCA` concatenates whatever is
in the slot. Quote it in the commit.

- [ ] **Step 3: Add the helper and the guard**

In `internal/certs/bundle.go`:

```go
// parsableCert reports whether pemBytes is something an agent's trust store
// will accept: one PEM block holding one certificate. Both halves matter --
// OperatorChannel.trustManager parses with CertificateFactory.generateCertificates,
// which throws on bytes that are not PEM and on a PEM envelope around
// something that is not a certificate, and it throws for the whole stream
// rather than skipping the offending block.
func parsableCert(pemBytes []byte) error {
```

and in `PublishedCA`, treat a slot that fails `parsableCert` as absent. Keep the
existing precedence — `Next` before `Previous` — and keep the function pure and
without a logger; the report is Task 2's.

- [ ] **Step 4: Run the test, then the package**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/ -run TestAnUnparseableSlotIsNotPublished -v
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/
```

Every pre-existing test must pass untouched.

- [ ] **Step 5: Commit**

```bash
git add internal/certs
git commit -m "$(cat <<'EOF'
fix(certs): never publish a rotation slot the agent cannot parse

PublishedCA's output reaches every namespace's spawnery-ca ConfigMap, and
OperatorChannel.trustManager throws on a stream containing anything that is
not a certificate rather than skipping the block. So a hand-edited ca-next.crt
did not cost a rotation, it cost every agent in every namespace its whole
trust store -- while the same damage to ca.crt is repaired by Ensure on the
next pass.

The guard sits in PublishedCA rather than at a call site because that is the
one function whose output reaches an agent, and a later path publishing the
bundle from somewhere else is exactly how this would come back.

The failing test covered both shapes the agent throws on, since only one of
them is caught by pem.Decode:

  <paste it here>

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 2: The cleanup, and what it leaves behind

**Files:**
- Modify: `internal/certs/rotation.go`, `internal/certs/events.go`,
  `internal/certs/events_test.go`
- Modify: `docs/superpowers/specs/2026-08-21-ca-rotation-design.md` (§4)
- Test: `internal/certs/rotation_sequence_envtest_test.go`

**Interfaces:**
- Consumes: `parsableCert` from Task 1.
- Produces:
  ```go
  AnnotationRotationDiscarded = "spawnery.cloud/ca-rotation-discarded"
  ReasonRotationSlotDiscarded = "RotationSlotDiscarded"
  actionDiscardRotationSlot   = "DiscardRotationSlot"
  ```

- [ ] **Step 1: Write the failing tests**

Four, in `internal/certs/rotation_sequence_envtest_test.go`. Each is its own
function; do not fold them into a table.

```go
// A broken ca-next while distributing abandons the rotation.
func TestAnUnparseableIncomingCAAbandonsTheRotation(t *testing.T)

// A broken ca-previous while switched completes the drop.
//
// This looks like the operator performing drop-old unasked, and it is not.
// The hold at `switched` exists so a rollback stays possible; a rollback signs
// with the previous CA, through RestorePrevious -> Reissue -> parseCA, on
// exactly these bytes. They stopped parsing, so the rollback was already
// impossible. Clearing the slot takes away no ability -- it records that the
// ability is gone. Nobody is stranded: the serving certificate chains to the
// new CA, which every agent trusts.
func TestAnUnparseableOutgoingCACompletesTheDrop(t *testing.T)

// A broken slot with no phase set is cleared and nothing else changes.
func TestAnUnparseableSlotWithNoRotationIsJustCleared(t *testing.T)

// The next accepted start clears the record, so it never narrates an old
// failure beside a live rotation.
func TestAStartClearsTheDiscardedRecord(t *testing.T)

// Ensure does not fail because of a corrupt slot, and the operator starts.
//
// This is the guard on the obvious implementation. Provider.Start returns
// Ensure's error from a Runnable, so an error here is fatal at startup:
// validating in Ensure would mean a hand-edited annotation takes the operator
// down, which is a worse outage than the one it describes. Asserted directly
// because "just validate it where you read it" is what a later reader will
// reach for.
func TestACorruptSlotDoesNotFailEnsure(t *testing.T)
```

The second of these is the one whose assertion is easiest to get wrong, so it
is written out; the others follow its shape.

```go
func TestAnUnparseableOutgoingCACompletesTheDrop(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	rec := events.NewFakeRecorder(8)
	s.Recorder = rec

	b := switchedAndHolding(t, s, clock, ctx, ns) // helper: a rotation driven to PhaseSwitched

	// Break the outgoing CA in place, the way a hand-edit would.
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: certs.SecretName, Namespace: ns}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		t.Fatalf("get the secret: %v", err)
	}
	secret.Data["ca-previous.crt"] = []byte("-- not a certificate --\n")
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("break the outgoing CA: %v", err)
	}

	if _, _, err := s.AdvanceRotation(ctx, b); err != nil {
		t.Fatalf("AdvanceRotation over a broken outgoing CA: %v", err)
	}

	if err := s.Client.Get(ctx, key, secret); err != nil {
		t.Fatalf("read the secret back: %v", err)
	}
	for _, k := range []string{"ca-previous.crt", "ca-previous.key"} {
		if len(secret.Data[k]) != 0 {
			t.Errorf("%s survived the cleanup", k)
		}
	}
	// The drop is complete, not merely the slot emptied: the hold at
	// `switched` exists so a rollback stays possible, and a rollback signs
	// with these very bytes through RestorePrevious -> Reissue -> parseCA.
	// They stopped parsing, so it was already impossible; leaving the phase
	// at `switched` would advertise a choice nobody can make.
	if got := secret.Annotations[certs.AnnotationRotationPhase]; got != "" {
		t.Errorf("phase = %q after the outgoing CA was discarded, want it cleared — "+
			"the hold's only purpose is a rollback, and the bytes it would sign with are gone", got)
	}
	if got := secret.Annotations[certs.AnnotationRotationDiscarded]; !strings.Contains(got, "ca-previous.crt") {
		t.Errorf("the discarded record = %q, want it to name the slot", got)
	}
	expectEvent(t, rec, corev1.EventTypeWarning, certs.ReasonRotationSlotDiscarded)
}
```

Each of the first three asserts, on the secret read back: the slot's two keys
are gone; `spawnery.cloud/ca-rotation-discarded` names the slot and the parse
error; the phase annotation is what §3 of the spec says for that case; and a
`RotationSlotDiscarded` warning was recorded. Assert on a recorder the test
controls — `Store.Recorder` is exported, so a `package certs_test` file can set
a `FakeRecorder`; `rotation_envtest_test.go`'s existing event assertions show
the shape, and `expectEvent` there is the helper to follow.

**Say in your report which package each test went into and why.** The fixture
helpers (`newStore`, `testClock`, `startedAndDistributed`, `phaseOf`) live in
`package certs_test`; `rotation_envtest_test.go` is white-box `package certs`
because it names the unexported `namespacesMissingCA`. Use whichever lets the
test reach what it needs without duplicating a fixture.

- [ ] **Step 2: Run them and record the failure**

Expected: compile errors — the three constants do not exist — and, once they
do, four real reds. Quote the first.

- [ ] **Step 3: Add the constants and the reason's doc**

In `internal/certs/events.go`, beside the six existing reasons. The doc says
why this is a seventh rather than a reuse: nothing was refused, nothing was
unrecognised, no gate is holding, and the phase change is a consequence rather
than the news. Add `actionDiscardRotationSlot` to `certsActions` in
`events_test.go` — that map is hand-maintained and
`TestNoCertsActionConstantIsEmpty` is what covers it, since
`internal/controller/events_test.go`'s AST scan does not reach this package.

- [ ] **Step 4: Add the cleanup**

In `AdvanceRotation`, **before** the request switch and immediately after the
`Get`. It is the step this call takes, and `AdvanceRotation` performs at most
one step per call — so it returns rather than falling through to a request or
to `drivePhase`. A request left in place is picked up on the next tick, by
which time the state it would act on is the cleaned one.

The rule, from spec §3: every unparseable certificate slot is cleared and
recorded, always; the rotation is abandoned or completed only when the slot
that broke is the one the current phase depends on. A `ca-previous` occupying a
`distributing` secret — a state no transition produces — is cleared and
reported without disturbing the rotation.

Both the data change and the annotation go through one `applyStep`, so the
record cannot land without the cleanup or the cleanup without it.

Use `%.150s` on the parse error in the event note, for the reason
`rotation.go`'s existing notes give: `fmt` counts a string precision in runes,
so it cuts on a rune boundary, and the note stays inside the 1024 **bytes**
`internal/controller/events.go` documents. Check your arithmetic against the
fixed text around it rather than assuming — that exact claim was wrong once on
this feature already.

- [ ] **Step 5: Clear the record on the next accepted `start`**

In `applyRequest`'s `RequestStart` branch, delete
`AnnotationRotationDiscarded` in the same mutate that sets the phase.

- [ ] **Step 6: Amend design §4**

It currently says "Six events" and lists six. There are seven. Add
`RotationSlotDiscarded` and one sentence for why it is not one of the others —
the same amendment this feature's §4 has taken twice before, for the same
reason.

- [ ] **Step 7: Prove the cleanup can fail**

| test | mutation | expected |
|---|---|---|
| abandons the rotation | skip the cleanup entirely | the phase stays `distributing`; test fails |
| completes the drop | clear the slot but leave the phase | the phase stays `switched`; test fails |
| the record | skip writing the discarded annotation, keeping the cleanup | the record is absent after one `AdvanceRotation`; test fails |

That third row is what "one `applyStep`" means observably: after a single
`AdvanceRotation` call, one read-back shows both the cleared slot and the
record. A second update would still pass that assertion, so do not claim the
test proves atomicity — it proves the record is not forgotten. Say so in the
report rather than overstating it.

Record all three outcomes verbatim. A mutation that leaves its test green is a
finding about the test.

- [ ] **Step 8: Run the package and the repository, then commit**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/
nix --extra-experimental-features 'nix-command flakes' develop -c make test
nix --extra-experimental-features 'nix-command flakes' develop -c make lint
```

---

## Task 3: The retry that makes two writers safe

**Files:**
- Test: `internal/certs/rotation_sequence_envtest_test.go`

No production change. If you find one is needed, stop and report — this task
exists because the behaviour is believed correct and untested.

- [ ] **Step 1: Write the test**

```go
// A human's instruction survives a conflict on the operator's own write.
//
// applyStep wraps its update in retry.RetryOnConflict with the Get inside the
// retried function, which is the whole concurrency story for a secret two
// parties write. The part worth protecting is the consume closure: it deletes
// rotate-ca only when the value is still the one this call decided on, so a
// request nobody has acted on yet cannot be swallowed by somebody else's
// retry.
//
// The scenario: the operator decides to act on start; between its read and its
// write a human replaces the annotation with rollback; the update conflicts;
// the retry re-reads, finds rollback, and must leave it alone -- while start's
// own work still lands, because it succeeded.
func TestAConflictDoesNotSwallowAnInstructionNobodyActedOn(t *testing.T) {
```

Build it with `interceptor.NewClient`, which
`internal/certs/rotation_envtest_test.go` already imports for the gate's
unreadable-ConfigMap test. Intercept `Update`: on the first call, write the
competing annotation through the underlying client and return
`apierrors.NewConflict(...)`; let the second through.

Assert both halves: `rotate-ca` reads `rollback` afterwards, and the phase is
`distributing` with `ca-next.crt` present — `start` landed.

- [ ] **Step 2: Run it**

Expected: **green on the first run.** That is the one case in this plan where
green-first is the correct outcome, because the behaviour already exists and
this task is only closing a coverage gap. Do not manufacture a red.

- [ ] **Step 3: Prove it would catch the regression**

Change `consume` to delete unconditionally:

```go
	consume := func(secret *corev1.Secret) {
		delete(secret.Annotations, AnnotationRotateRequest)
	}
```

The test must fail, naming the lost `rollback`. Restore and confirm with
`git diff --stat internal/certs`. This mutation is what makes the test worth
having; record its output verbatim.

- [ ] **Step 4: Run the package and commit**

---

## What this plan does not cover

- Validating the rotation slots' **keys**. They reach no agent, and `Reissue` →
  `parseCA` already refuses them at the moment they matter.
- Changing `OperatorChannel.trustManager` to skip unparseable blocks. That
  would fix every source of a malformed bundle rather than this one, and it
  changes agent code this feature deliberately left untouched — an agent that
  silently trusts less than its bundle says is its own class of problem.
- Any change to a phase, a transition, the annotation vocabulary, or the
  timing.
