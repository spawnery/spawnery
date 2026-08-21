# CA Rotation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The operator can replace its CA on a running cluster, with an
overlap, without a single agent losing its connection.

**Architecture:** Two CAs live in `spawnery-agent-tls` at once. `ca.crt`/`ca.key`
keep meaning "the CA that signs the serving certificate right now", so
`parseCA`, `Issue`, `Reissue`, `reissueOrIssue` and `Validate` do not change.
`Provider.CABundle` publishes the concatenation, which the existing
`Bootstrapper` copies into every namespace unchanged. A state machine in
`internal/certs`, driven by annotations on that secret and by `Provider.Start`'s
loop, walks the sequence: issue, distribute, wait, switch, hold.

**Tech Stack:** Go, controller-runtime, envtest, Prometheus client, bash +
containers for the cross-language phase.

## Global Constraints

- The spec is `docs/superpowers/specs/2026-08-21-ca-rotation-design.md`. Where
  this plan and the spec disagree, stop and ask — do not pick one.
- Every command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`.
  Envtest is slow by nature; a package run takes minutes. Let it finish.
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
- **`Issue`, `Reissue`, `reissueOrIssue`, `Validate`, `parseCA` and
  `parseServing` keep their current behaviour.** A cluster that never rotates
  must behave exactly as before. Task 1 refactors `Issue` internally; that is
  the only permitted change, and its existing tests must pass untouched.
- **`ca.crt`/`ca.key` always mean the CA that signs the serving certificate
  now.** Not "the original CA", not "the newest". Every phase in the spec's §3
  table obeys this, and it is what keeps the functions above untouched.
- **The CA ConfigMap keeps carrying no `OwnerReference`.**
- Comments explain why, not what.
- **A test that passes the moment it is written has proven nothing.** Each task
  says what its test must fail with first, and the failure goes in the commit.

---

## File Structure

| File | Change |
|---|---|
| `internal/certs/bundle.go` | four rotation slots on `Bundle`; `PublishedCA`; `IssueCA`; four pure transitions |
| `internal/certs/store.go` | the secret round-trips the new keys; `Provider.CABundle` publishes the concatenation; the loop's second cadence |
| `internal/certs/rotation.go` | new — the annotations, the gate, the window, `AdvanceRotation` |
| `internal/certs/events.go` | new — reasons and actions for the four events |
| `internal/certs/metrics.go` | new — two gauges, registered the way the other packages do |
| `cmd/spawnery-operator/main.go` | pass `--agent-session-deadline` and an event recorder into the store |
| `internal/rbacaudit/required.go` | the `configmaps: get` Why gains a second call site |
| `hack/agent-test.sh` | a sixth phase: the agent trusts the second PEM |
| `docs/known-issues.md` | the entry is replaced by what is now true |

---

## Task 1: A bundle can hold a second CA

**Files:**
- Modify: `internal/certs/bundle.go`
- Test: `internal/certs/bundle_test.go`

**Interfaces:**
- Produces, for Tasks 2 and 4:
  - `Bundle.NextCACertPEM`, `.NextCAKeyPEM`, `.PreviousCACertPEM`, `.PreviousCAKeyPEM []byte`
  - `func (b *Bundle) PublishedCA() []byte`
  - `func IssueCA(now time.Time) (certPEM, keyPEM []byte, err error)`
  - `func (b *Bundle) WithNextCA(certPEM, keyPEM []byte) *Bundle`
  - `func (b *Bundle) SwitchToNext(now time.Time, dnsNames []string) (*Bundle, error)`
  - `func (b *Bundle) RestorePrevious(now time.Time, dnsNames []string) (*Bundle, error)`
  - `func (b *Bundle) WithoutRotation() *Bundle`

This task is pure Go: no client, no API server, no envtest.

- [ ] **Step 1: Write the failing test**

Append to `internal/certs/bundle_test.go`:

```go
// The switch pairs each certificate with its own key, and both pairs stay
// usable afterwards.
//
// This is the one operation in the rotation that can produce a bundle which
// looks complete and is not: pairing the incoming certificate with the
// outgoing key, or the reverse, yields four non-empty PEMs that Validate
// rejects and that nothing else would notice until the operator refused to
// serve. parseCA already checks that a key matches its certificate, so the
// assertion is available -- it just has to be made about both pairs, not only
// the signing one.
func TestSwitchingToTheNextCAPairsEveryCertificateWithItsOwnKey(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	dnsNames := certs.ServingDNSNames("spawnery-operator", "spawnery-system")

	first, err := certs.Issue(now, dnsNames)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	nextCert, nextKey, err := certs.IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}

	switched, err := first.WithNextCA(nextCert, nextKey).SwitchToNext(now, dnsNames)
	if err != nil {
		t.Fatalf("SwitchToNext: %v", err)
	}

	if err := switched.Validate(now, dnsNames); err != nil {
		t.Fatalf("the switched bundle does not validate: %v", err)
	}
	if !bytes.Equal(switched.CACertPEM, nextCert) {
		t.Error("the signing CA after the switch is not the incoming one")
	}
	if !bytes.Equal(switched.PreviousCACertPEM, first.CACertPEM) {
		t.Error("the outgoing CA was not kept as the previous one")
	}
	if len(switched.NextCACertPEM) != 0 || len(switched.NextCAKeyPEM) != 0 {
		t.Error("the next slot is still occupied after the switch")
	}

	// The previous pair has to be usable, not merely present: it is what a
	// rollback signs with.
	rolledBack, err := switched.RestorePrevious(now, dnsNames)
	if err != nil {
		t.Fatalf("RestorePrevious: %v", err)
	}
	if err := rolledBack.Validate(now, dnsNames); err != nil {
		t.Fatalf("the rolled-back bundle does not validate: %v", err)
	}
	if !bytes.Equal(rolledBack.CACertPEM, first.CACertPEM) {
		t.Error("the rollback did not restore the original CA")
	}
	if len(rolledBack.NextCACertPEM) != 0 || len(rolledBack.PreviousCACertPEM) != 0 {
		t.Error("the rollback left a rotation slot occupied")
	}
}

// What the agents pin, in each phase. Two PEMs, not one, and the signing CA
// first so that an unchanged phase produces an unchanged ConfigMap write.
func TestThePublishedBundleCarriesBothCAsWhileRotating(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	dnsNames := certs.ServingDNSNames("spawnery-operator", "spawnery-system")

	atRest, err := certs.Issue(now, dnsNames)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	nextCert, nextKey, err := certs.IssueCA(now)
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}

	count := func(b *certs.Bundle) int {
		t.Helper()
		n := 0
		for rest := b.PublishedCA(); len(rest) > 0; {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			n++
		}
		return n
	}

	if got := count(atRest); got != 1 {
		t.Errorf("a bundle at rest publishes %d certificates, want 1", got)
	}
	distributing := atRest.WithNextCA(nextCert, nextKey)
	if got := count(distributing); got != 2 {
		t.Errorf("a distributing bundle publishes %d certificates, want 2 — "+
			"an agent that never sees the incoming CA cannot survive the switch", got)
	}
	switched, err := distributing.SwitchToNext(now, dnsNames)
	if err != nil {
		t.Fatalf("SwitchToNext: %v", err)
	}
	if got := count(switched); got != 2 {
		t.Errorf("a switched bundle publishes %d certificates, want 2 — "+
			"the outgoing CA stays trusted until drop-old", got)
	}
	if got := count(switched.WithoutRotation()); got != 1 {
		t.Errorf("after drop-old the bundle publishes %d certificates, want 1", got)
	}
}
```

- [ ] **Step 2: Run it and record the failure**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/ -run 'TestSwitchingToTheNextCAPairs|TestThePublishedBundleCarries' -v`

Expected: **a compile error** — `certs.IssueCA` is undefined, and so are the
four methods and the four fields. Quote it in the commit.

- [ ] **Step 3: Add the fields**

In `internal/certs/bundle.go`, on `Bundle`:

```go
	// NextCACertPEM and NextCAKeyPEM hold the incoming CA while a rotation is
	// distributing, and are empty at every other time. The CA that signs the
	// serving certificate stays CACertPEM/CAKeyPEM throughout that phase --
	// still the outgoing one -- and that is what makes the phase safe: the new
	// CA is published for agents to trust well before anything is signed with
	// it.
	NextCACertPEM []byte
	NextCAKeyPEM  []byte

	// PreviousCACertPEM and PreviousCAKeyPEM hold the outgoing CA between the
	// switch and drop-old. The key is kept and not only the certificate,
	// because signing with it again is the whole content of a rollback; a
	// certificate on its own would be trust nobody can act on.
	PreviousCACertPEM []byte
	PreviousCAKeyPEM  []byte
```

- [ ] **Step 4: Factor out `IssueCA` and add `PublishedCA`**

`Issue` currently mints the CA inline. Move that into `IssueCA` and have
`Issue` call it — its behaviour must not change, and
`internal/certs/bundle_test.go`'s existing tests are the check.

```go
// IssueCA mints a self-signed CA. Issue calls it for the first one; a rotation
// calls it for the incoming one, which is why it exists separately: at that
// point there is no serving certificate to sign, and signing one would be
// exactly the thing the overlap window has to postpone.
func IssueCA(now time.Time) (certPEM, keyPEM []byte, err error) {
```

```go
// PublishedCA is what the agents pin: the CA that signs the serving
// certificate, followed by whichever second CA the rotation is currently
// holding. Order does not matter to the agent -- OperatorChannel.trustManager
// loads every certificate in the stream -- but it is deterministic so that a
// phase which has not changed produces a ConfigMap write that is a no-op.
func (b *Bundle) PublishedCA() []byte {
	switch {
	case len(b.NextCACertPEM) > 0:
		return slices.Concat(b.CACertPEM, b.NextCACertPEM)
	case len(b.PreviousCACertPEM) > 0:
		return slices.Concat(b.CACertPEM, b.PreviousCACertPEM)
	}
	return b.CACertPEM
}
```

`encodeCert` returns `pem.EncodeToMemory` output, which ends in a newline, so
concatenation needs no separator. Confirm that by reading `encodeCert` rather
than trusting this sentence.

- [ ] **Step 5: Add the four transitions**

```go
// WithNextCA returns a bundle carrying an incoming CA. The serving certificate
// is untouched and still chains to CACertPEM.
func (b *Bundle) WithNextCA(certPEM, keyPEM []byte) *Bundle

// SwitchToNext promotes the incoming CA to the signing one, demotes the
// outgoing one to the previous slot, and signs a fresh serving certificate
// with the new CA. This is the step the overlap window exists to protect, and
// the only one that can strand an agent.
func (b *Bundle) SwitchToNext(now time.Time, dnsNames []string) (*Bundle, error)

// RestorePrevious is SwitchToNext undone: the outgoing CA signs again and the
// incoming one is discarded. Meaningful only after a switch.
func (b *Bundle) RestorePrevious(now time.Time, dnsNames []string) (*Bundle, error)

// WithoutRotation empties both rotation slots, leaving the signing CA alone.
// It is what drop-old does, and what a rollback out of the distributing phase
// does.
func (b *Bundle) WithoutRotation() *Bundle
```

`SwitchToNext` and `RestorePrevious` both end by calling `Reissue` on a bundle
whose `CACertPEM`/`CAKeyPEM` are the pair that should sign — so neither
duplicates the signing code, and both refuse an empty slot with an error that
names which one is missing.

- [ ] **Step 6: Run the new tests, then the package**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/ -run 'TestSwitchingToTheNextCAPairs|TestThePublishedBundleCarries' -v
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/
```

Both must pass, and **every pre-existing test in the package must pass
unchanged**. If one needed editing, stop and report: this task was supposed to
be additive apart from `Issue`'s internal refactor.

- [ ] **Step 7: Prove the pairing test can fail**

In `SwitchToNext`, pair the incoming certificate with the outgoing key —
`CAKeyPEM: b.CAKeyPEM` instead of `b.NextCAKeyPEM`. Run only
`TestSwitchingToTheNextCAPairsEveryCertificateWithItsOwnKey`. It must go red on
`Validate`. Restore, and confirm with `git diff --stat internal/certs`. Record
the verbatim failure in the report; a mutation that leaves the test green means
the test does not test what it claims.

- [ ] **Step 8: Commit**

```bash
git add internal/certs
git commit -m "$(cat <<'EOF'
feat(certs): a bundle can hold the CA it is rotating to

Two slots beside the signing CA -- next while distributing, previous between
the switch and drop-old -- and a published bundle that concatenates whichever
is occupied. ca.crt and ca.key keep meaning "the CA that signs the serving
certificate now", so parseCA, Reissue, reissueOrIssue and Validate are
untouched and a cluster that never rotates behaves exactly as before.

Issue was refactored to call the new IssueCA rather than mint a CA inline: a
rotation needs a CA without a serving certificate, and signing one at that
moment is precisely what the overlap window has to postpone.

The failing test was a compile error:

  <paste it here>

The pairing test was shown to fire by signing with the outgoing key while
promoting the incoming certificate: four non-empty PEMs that Validate rejects,
which is the one way this step can produce a bundle that looks complete and is
not.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 2: The secret round-trips both CAs

**Files:**
- Modify: `internal/certs/store.go`
- Test: `internal/certs/store_envtest_test.go`

**Interfaces:**
- Consumes: everything Task 1 produced.
- Produces: `Store.secretFor` and `Store.Ensure` read and write `ca-next.crt`,
  `ca-next.key`, `ca-previous.crt`, `ca-previous.key`; `Provider.CABundle`
  returns `PublishedCA()`.

No state machine yet. This task only makes the storage layer able to hold a
rotation, so Task 4 has somewhere to put one.

- [ ] **Step 1: Write the failing test**

Append to `internal/certs/store_envtest_test.go`:

```go
// A rotation in progress survives the operator restarting.
//
// Everything about the sequence lives in the secret so that a new leader can
// pick it up, and this is the assertion that the storage layer actually keeps
// it. Ensure reads the secret back on every call; a rotation slot it did not
// know about would be dropped on the first renewal, silently, and the operator
// would go back to publishing one CA while agents were mid-window.
func TestEnsureCarriesARotationSlotThroughAReadBack(t *testing.T) {
	s, clock, ctx, ns := newStore(t)

	if _, err := s.Ensure(ctx); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	nextCert, nextKey, err := certs.IssueCA(clock.Now())
	if err != nil {
		t.Fatalf("IssueCA: %v", err)
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: certs.SecretName, Namespace: ns}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		t.Fatalf("get the secret: %v", err)
	}
	secret.Data["ca-next.crt"] = nextCert
	secret.Data["ca-next.key"] = nextKey
	if err := s.Client.Update(ctx, secret); err != nil {
		t.Fatalf("plant the incoming CA: %v", err)
	}

	// Far enough for the serving certificate to need renewing, so this goes
	// down Ensure's rewrite path rather than its no-op one.
	clock.Advance(80 * 24 * time.Hour)

	b, err := s.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure after the advance: %v", err)
	}
	if !bytes.Equal(b.NextCACertPEM, nextCert) {
		t.Error("Ensure did not read the incoming CA back out of the secret")
	}

	if err := s.Client.Get(ctx, key, secret); err != nil {
		t.Fatalf("get the secret again: %v", err)
	}
	if !bytes.Equal(secret.Data["ca-next.crt"], nextCert) {
		t.Error("a renewal dropped the incoming CA from the secret; a rotation " +
			"would silently lose its overlap on the next renewal")
	}
	if !bytes.Equal(secret.Data["ca-next.key"], nextKey) {
		t.Error("a renewal dropped the incoming CA's key from the secret")
	}
}
```

- [ ] **Step 2: Run it and record the failure**

Run: `nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/ -run TestEnsureCarriesARotationSlotThroughAReadBack -v`

Expected: a real red — `Ensure` returns a bundle with an empty
`NextCACertPEM`, and the read-back finds the planted keys gone, because
`secretFor` writes exactly four keys and `Update` replaces `Data` wholesale
(`store.go:130`). Quote both failures in the commit.

- [ ] **Step 3: Read and write the new keys**

In `Store.Ensure`, where the bundle is built from `secret.Data`, add the four
slots. In `secretFor`, write them **only when they are non-empty** — an empty
key in a secret is a key, and `drop-old` has to be able to remove one:

```go
	if len(b.NextCACertPEM) > 0 {
		data["ca-next.crt"] = b.NextCACertPEM
		data["ca-next.key"] = b.NextCAKeyPEM
	}
```

Name the four literals as constants beside `SecretName`, rather than repeating
them across this file and `rotation.go`:

```go
const (
	keyNextCACert     = "ca-next.crt"
	keyNextCAKey      = "ca-next.key"
	keyPreviousCACert = "ca-previous.crt"
	keyPreviousCAKey  = "ca-previous.key"
)
```

- [ ] **Step 4: Publish the concatenation**

`Provider.Set` stores `ca: b.CACertPEM` (`store.go:171`). It becomes
`b.PublishedCA()`. One line, and it is the line the whole design turns on: from
here the existing `Bootstrapper` carries two PEMs into every namespace without
knowing anything has changed.

- [ ] **Step 5: Run the package**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/
```

Expected: green, including every pre-existing test.

- [ ] **Step 6: Prove the read-back test can fail**

Delete the four lines added to `secretFor` in Step 3. The test must go red on
the read-back assertion, not only on the returned bundle — if it only fails on
the first, the test is not covering the write path. Restore and confirm with
`git diff --stat internal/certs`.

- [ ] **Step 7: Commit**

```bash
git add internal/certs
git commit -m "$(cat <<'EOF'
feat(certs): the secret round-trips a rotation in progress

Ensure reads the two rotation slots back and secretFor writes them, so a
rotation survives a renewal, an operator restart and a leader change --
everything about the sequence lives in the secret precisely so that it does.
The slots are written only when occupied, because drop-old has to be able to
remove one and an empty value in a secret is still a key.

Provider.Set now publishes PublishedCA() rather than CACertPEM. That is the
line the design turns on: the existing Bootstrapper carries two PEMs into every
namespace from here without knowing anything changed.

The failing test showed both halves -- a bundle read back without its slot, and
a renewal that dropped the planted keys:

  <paste it here>

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 3: The gate

**Files:**
- Create: `internal/certs/rotation.go`
- Test: `internal/certs/rotation_envtest_test.go`

**Interfaces:**
- Produces, for Task 4:
  ```go
  // namespacesMissingCA returns the namespaces that hold a Network but whose
  // spawnery-ca ConfigMap does not yet carry the given certificate, sorted.
  func (s *Store) namespacesMissingCA(ctx context.Context, caCertPEM []byte) ([]string, error)
  ```

- [ ] **Step 1: Write the failing test**

Create `internal/certs/rotation_envtest_test.go` and write a test that:

1. Creates two namespaces, each with a `spawneryv1alpha1.Network` (the CRDs are
   installed — `internal/testenv` registers `spawneryv1alpha1` and points
   envtest at `config/crd`, so `Network` objects can simply be created).
2. Writes a `spawnery-ca` ConfigMap into the first namespace holding a
   two-PEM bundle that includes the target certificate, and into the second
   namespace one holding only an unrelated certificate.
3. Creates a **third** namespace with a `spawnery-ca` ConfigMap holding the
   stale certificate and **no** `Network` — the deleted-network case.
4. Asserts the result is exactly the second namespace.

Point 3 is the reason this function exists in this shape, so assert it
explicitly and say so in the test's comment:

```go
// The gate is driven from the Network objects, not from the ConfigMaps.
//
// "A Network owns its namespace" is the one-per-namespace rule
// (pickNamespaceOwner), not a Kubernetes OwnerReference -- the operator never
// creates a namespace and never owns one -- and the CA ConfigMap deliberately
// carries no owner reference so that it outlives the operator. So a
// spawnery-ca ConfigMap whose Network was deleted stays in its namespace
// forever with whatever bundle it last received. A gate phrased as "every
// managed CA ConfigMap" would wait on that dead namespace until somebody
// cleaned it up by hand, which is to say: a rotation would never complete on
// any cluster where a Network had ever been deleted.
```

Use the real names: `podspec.CAConfigMapName`, `podspec.CAConfigMapKey`,
`podspec.LabelManagedBy`, `podspec.ManagedByValue`. Read them from
`internal/podspec` rather than writing the strings out — `internal/certs`
importing `internal/podspec` introduces no cycle (nothing in `podspec` or
`api/` imports `certs`; checked).

- [ ] **Step 2: Run it and record the failure**

Expected: a compile error — `namespacesMissingCA` does not exist.

- [ ] **Step 3: Implement it**

`List` the `Network` objects across all namespaces, reduce to the distinct set
of namespaces, and for each one `Get` `spawnery-ca` and decode every PEM block
in `ca.crt`, comparing SHA-256 of the DER against the target's. Compare
fingerprints, not bytes: a re-encoded PEM with different line wrapping is the
same certificate, and a substring match on the PEM text would be a subtler way
of saying the same thing wrong.

A namespace whose ConfigMap is absent counts as missing, not as an error. A
namespace whose ConfigMap cannot be read for any other reason is an error —
answering "everything is fine" because a read failed is the one outcome this
function must never produce.

Sort the result. It goes into an annotation and an event, and an unstable order
would rewrite the secret on every check.

- [ ] **Step 4: Run the test and the package**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/ -run TestTheGate -v
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/
```

- [ ] **Step 5: Prove the deleted-network case can fail**

Change the implementation to list `ConfigMap` objects by
`podspec.LabelManagedBy` instead of listing `Network` objects — the
implementation the test exists to forbid. The third namespace must now appear
in the result and the test must go red. Restore and confirm with
`git diff --stat internal/certs`.

- [ ] **Step 6: Commit**

```bash
git add internal/certs
git commit -m "$(cat <<'EOF'
test(certs): the rotation gate, driven from the Networks

Which namespaces must hold the new CA before a switch is safe: the ones with a
Network in them, not the ones with a CA ConfigMap in them. The distinction is
load-bearing. "A Network owns its namespace" is the one-per-namespace rule and
not an OwnerReference, the ConfigMap deliberately carries no owner reference so
it outlives the operator, and so a namespace whose Network was deleted keeps a
stale spawnery-ca forever. A gate over the ConfigMaps would wait on it until
somebody cleaned up by hand -- which is to say a rotation would never complete
on any cluster where a Network had ever been deleted.

Certificates are compared by SHA-256 of their DER, not by PEM bytes: a
re-encoding is the same certificate, and a substring match would be a quieter
way of being wrong about that.

Shown to fire by listing ConfigMaps by label instead -- the implementation this
test exists to forbid -- which brings the orphaned namespace back into the
result:

  <paste it here>

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 4: The sequence

**Files:**
- Modify: `internal/certs/rotation.go`, `internal/certs/store.go`
- Modify: `cmd/spawnery-operator/main.go`
- Test: `internal/certs/rotation_envtest_test.go`

**Interfaces:**
- Consumes: `namespacesMissingCA` from Task 3, the transitions from Task 1.
- Produces, for Task 5:
  ```go
  // AdvanceRotation performs at most one step of a rotation and returns the
  // bundle to publish and whether a rotation is still in flight -- which is
  // what decides the provider's cadence.
  func (s *Store) AdvanceRotation(ctx context.Context, current *Bundle) (*Bundle, bool, error)
  ```
- Produces: `Store.AgentSessionDeadline time.Duration`, set from
  `--agent-session-deadline` in `main.go`.

**Constants** (in `rotation.go`, exported where a test or an operator names
them):

```go
const (
	AnnotationRotateRequest     = "spawnery.cloud/rotate-ca"
	AnnotationRotationPhase     = "spawnery.cloud/ca-rotation-phase"
	AnnotationRotationSince     = "spawnery.cloud/ca-rotation-since"
	AnnotationRotationBlockedOn = "spawnery.cloud/ca-rotation-blocked-on"

	RequestStart    = "start"
	RequestDropOld  = "drop-old"
	RequestRollback = "rollback"

	PhaseDistributing = "distributing"
	PhaseSwitched     = "switched"
)

// RotationCheckInterval is how often the provider looks while a rotation is in
// flight. RenewCheckInterval's hour is right for a certificate that lives 90
// days and wrong for a procedure that takes a quarter of an hour.
const RotationCheckInterval = 30 * time.Second

// projectionMargin covers the gap between the operator writing the CA
// ConfigMap and the kubelet projecting it into the pods that mount it. The
// kubelet's --sync-frequency defaults to one minute and the watch-based
// projection is faster than that in practice, so two minutes is the margin
// rather than the estimate. It is arithmetic on a documented period, not a
// measurement: the thing that would make it wrong is a cluster configured with
// a much longer sync frequency, which this operator cannot see.
const projectionMargin = 2 * time.Minute

// maxBlockedNamesInAnnotation bounds the blocked-on list. Ten namespace names
// at the 63-character maximum, with separators and the prefix, stay well below
// the 1024 bytes an event note allows (internal/controller/events.go documents
// that limit as probed against a real API server).
const maxBlockedNamesInAnnotation = 10
```

- [ ] **Step 1: Write the failing tests — one per claim**

In `internal/certs/rotation_envtest_test.go`. Each is a separate function with
its own comment; do not fold them into a table.

```go
// start publishes the incoming CA and signs nothing with it.
func TestStartPublishesTheIncomingCAWithoutSigningWithIt(t *testing.T)

// The gate holds the phase while any namespace with a Network lacks it, and
// the missing namespaces are named on the secret.
func TestTheSwitchWaitsForEveryNamespaceThatHoldsANetwork(t *testing.T)

// The window is projectionMargin + the operator's own session deadline,
// measured from the moment the gate passed.
func TestTheSwitchWaitsOutTheWindowAfterTheGatePasses(t *testing.T)

// After the switch the serving certificate chains to the new CA, the old one
// is still published, and nothing advances without a second annotation.
func TestTheSwitchHoldsUntilDropOldIsAsked(t *testing.T)

// drop-old narrows the bundle and removes the outgoing key.
func TestDropOldNarrowsTheBundleAndRemovesTheOutgoingKey(t *testing.T)

// rollback abandons the rotation from either phase and leaves nothing that
// would advance on its own.
func TestRollbackAbandonsTheRotationFromEitherPhase(t *testing.T)

// A Network created during the window does not postpone the switch.
//
// Its namespace's ConfigMap receives the current bundle -- already two PEMs --
// on its first reconcile, and its pods have never held anything else.
// Re-checking the gate is the obvious implementation and would let a cluster
// where networks are created regularly push the switch out forever.
func TestANetworkCreatedDuringTheWindowDoesNotPostponeTheSwitch(t *testing.T)

// A request the operator does not recognise is left alone and reported; one it
// does recognise is removed once acted on.
//
// Clearing an annotation you did not understand hides the typo that produced
// it, and leaving one you did act on would fire it again on the next tick.
func TestAnUnknownRequestIsLeftInPlaceAndAKnownOneIsConsumed(t *testing.T)

// A switch with no session deadline configured is refused rather than
// performed against a window that is short by ten minutes.
func TestAMissingSessionDeadlineRefusesTheSwitch(t *testing.T)
```

Write each body against the fixture `newStore(t)` already provides, advancing
`clock` to move the window. `AdvanceRotation` is called directly; there is no
need to run `Provider.Start`.

Two of them are written out in full below, because they are the two whose
assertions are easy to get subtly wrong in a way that leaves the test green.
The other seven follow the same shape: annotate, call `AdvanceRotation`, read
the secret back, assert on `Data` and on the annotations by their exported
constant names.

```go
func TestTheSwitchWaitsOutTheWindowAfterTheGatePasses(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute
	b := startedAndDistributed(t, s, clock, ctx, ns) // helper: start + a Network whose ConfigMap already holds the new CA

	// The gate passes on this call, which stamps `since`. Nothing switches yet:
	// the window has not begun to run until it is stamped.
	b, inFlight, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation at the gate: %v", err)
	}
	if !inFlight {
		t.Fatal("the rotation reported itself finished at the moment the gate passed")
	}
	if len(b.NextCACertPEM) == 0 {
		t.Fatal("the switch happened on the same call that passed the gate; the window never ran")
	}

	// One second short of projectionMargin + AgentSessionDeadline.
	clock.Advance(12*time.Minute - time.Second)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation just before the window: %v", err)
	}
	if len(b.NextCACertPEM) == 0 {
		t.Error("the switch happened one second early. The window is the kubelet's " +
			"projection margin plus this operator's own --agent-session-deadline, and " +
			"an agent that has not re-read the file yet is locked out by the switch")
	}

	clock.Advance(2 * time.Second)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation after the window: %v", err)
	}
	if len(b.NextCACertPEM) != 0 {
		t.Fatal("the switch did not happen after the window elapsed")
	}
	if got := phaseOf(t, s, ctx, ns); got != certs.PhaseSwitched {
		t.Errorf("phase = %q after the window, want %q", got, certs.PhaseSwitched)
	}
}

func TestANetworkCreatedDuringTheWindowDoesNotPostponeTheSwitch(t *testing.T) {
	s, clock, ctx, ns := newStore(t)
	s.AgentSessionDeadline = 10 * time.Minute
	b := startedAndDistributed(t, s, clock, ctx, ns)

	// Pass the gate and stamp `since`.
	b, _, err := s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation at the gate: %v", err)
	}

	// A Network appears halfway through the window, in a namespace with no CA
	// ConfigMap at all -- the state a re-run of the gate would call "missing".
	clock.Advance(6 * time.Minute)
	latecomer := testenv.Namespace(t, ctx, s.Client)
	createNetwork(t, ctx, s.Client, latecomer, "arrived-late")

	clock.Advance(6*time.Minute + time.Second)
	b, _, err = s.AdvanceRotation(ctx, b)
	if err != nil {
		t.Fatalf("AdvanceRotation after the window: %v", err)
	}
	if len(b.NextCACertPEM) != 0 {
		t.Fatal("a Network created during the window postponed the switch. Its " +
			"namespace's ConfigMap receives the current bundle -- already two PEMs -- " +
			"on its first reconcile, and its pods have never held anything else, so " +
			"there is nothing for the window to protect. Re-checking the gate lets a " +
			"cluster where networks are created regularly push the switch out forever")
	}
}
```

Write the two helpers (`startedAndDistributed`, `phaseOf`, `createNetwork`) in
the test file; they are fixture plumbing, not behaviour.

- [ ] **Step 2: Run them and record the failure**

Expected: a compile error — `AdvanceRotation` does not exist. Quote it.

- [ ] **Step 3: Implement `AdvanceRotation`**

One step per call, never two: read the secret fresh, decide, write at most
once, return. A function that walked several transitions in one call would be
untestable at the boundaries and would make a partial write ambiguous.

**Every write is a fresh `Get` followed by an `Update`, retried on conflict.**
The secret now has two writers — this operator and whoever annotates it — and
the store uses the uncached client, so an `Update` built on a copy read a
moment earlier would silently discard the annotation a human had just set.
`retry.RetryOnConflict` with `retry.DefaultRetry` is the idiom; the `Get` has
to be inside the retried function, not outside it, or the retry re-sends the
same stale object and achieves nothing.

The refusals are as much of the behaviour as the transitions:
`start` while `switched` is refused (a third CA has no slot); `drop-old` while
`distributing` is refused (the CA it would drop is the one signing); a switch
with `AgentSessionDeadline == 0` is refused with an error naming the missing
wiring rather than computed against a window that is short by exactly the
deadline.

- [ ] **Step 4: Add the second cadence and the wiring**

In `Provider.Start`, replace the fixed `time.Ticker` with a timer whose
interval is `RotationCheckInterval` while `AdvanceRotation` reports a rotation
in flight and `RenewCheckInterval` otherwise. Call `Ensure` first, then
`AdvanceRotation`, then `Set` — `Ensure` keeps its job of making a usable
bundle exist, and the rotation is additive on top of it.

In `cmd/spawnery-operator/main.go`, pass the already-parsed `hardDeadline` into
the store as `AgentSessionDeadline`. It is the running operator's own flag
value, which is what makes the window a bound rather than an estimate.

- [ ] **Step 5: Run the package, then the repository**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/
nix --extra-experimental-features 'nix-command flakes' develop -c make test
```

- [ ] **Step 6: Prove three of the guards can fail**

| test | mutation | expected |
|---|---|---|
| the window | drop `projectionMargin` from the sum | the switch happens two minutes early; test fails |
| the new Network | re-run the gate on every call instead of only before stamping `since` | the switch is postponed; test fails |
| the consumed request | stop removing `rotate-ca` after acting | `start` fires twice; test fails |

After each, restore and confirm with `git diff --stat`. Record all three.

- [ ] **Step 7: Commit**

Split this into two commits if the diff is large: the state machine, then the
cadence and the wiring. Both carry the trailer.

---

## Task 5: Saying what is happening

**Files:**
- Create: `internal/certs/events.go`, `internal/certs/metrics.go`
- Modify: `internal/certs/rotation.go`, `cmd/spawnery-operator/main.go`
- Modify: `internal/rbacaudit/required.go`
- Test: `internal/certs/rotation_envtest_test.go`, `internal/certs/events_test.go`

**Interfaces:**
- Consumes: `AdvanceRotation` from Task 4.
- Produces: `Store.Recorder events.EventRecorder`, set from
  `mgr.GetEventRecorder("certs")` in `main.go`.

- [ ] **Step 1: The four events**

`RotationStarted`, `RotationBlocked` (Warning), `RotationSwitched`,
`RotationCompleted`, recorded on the secret. Match the argument shape the
controllers use — `mgr.GetEventRecorder` returns an `events.EventRecorder`,
whose `Eventf` takes `(object, related, eventtype, reason, action, messageFmt,
args...)`.

**Two things to know before writing these, both checked rather than assumed:**

The action identifiers are **not** covered by
`internal/controller/events_test.go`'s AST scan. That test walks `"."` from its
own package directory, so its corpus is `internal/controller` and nothing else.
Declare the certs actions as constants in `internal/certs/events.go` and add
the one check that scan opens with — that no action constant is empty, because
`events.k8s.io/v1` refuses an event without one. Say in the commit that the
scan does not reach here, so the next reader does not assume it does.

The note limit is **1024 bytes**, not characters — probed against a real API
server and documented at `internal/controller/events.go:124-128`. `eventNote`
is unexported there and this package cannot use it. Rather than copy it, bound
the notes at the source: the blocked-on list is capped at
`maxBlockedNamesInAnnotation` names, and any interpolated error goes in as
`%.150s`, whose precision `fmt` counts in **runes** for strings — so it cuts on
a rune boundary by construction and yields at most 600 bytes, leaving the rest
of the note far inside the limit.

- [ ] **Step 2: The two gauges**

`internal/certs/metrics.go`, in the shape `internal/agentserver/metrics.go` and
`internal/grpcauth/metrics.go` already use — `func init() {
metrics.Registry.MustRegister(...) }`:

```go
var (
	// RotationPhase carries 1 for the active phase and 0 for the others, so a
	// query can ask "is anything rotating" without knowing which phase to look
	// for.
	RotationPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spawnery_ca_rotation_phase",
		Help: "1 for the CA rotation phase currently in effect, 0 for the others.",
	}, []string{"phase"})

	// RotationBlockedNamespaces is the point of this file. "Stuck in
	// distributing for two days" is the failure this design most plausibly
	// produces, and it should be a query rather than something somebody happens
	// to notice.
	RotationBlockedNamespaces = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spawnery_ca_rotation_blocked_namespaces",
		Help: "Namespaces holding a Network whose CA ConfigMap does not yet carry the incoming CA.",
	})
)

func init() { metrics.Registry.MustRegister(RotationPhase, RotationBlockedNamespaces) }
```

Set both from `AdvanceRotation`, including on the paths that end a rotation —
a gauge left at its last value after `drop-old` reports a rotation that
finished as one still running.

- [ ] **Step 3: The RBAC audit's Why**

`internal/rbacaudit/required.go:110` explains `configmaps: get` as
"Bootstrapper.Ensure reads the CA ConfigMap". The gate is a second call site.
Name both, the way the `pods: patch` entry does. The `list` entry is **not**
the one to change: its Why is about the restricted cache, the gate reads each
ConfigMap with a `Get` through the uncached client, and that Why stays true as
written.

- [ ] **Step 4: Assert the events, not just the code**

Extend the Task 4 tests: the blocked case records a Warning naming the
namespaces, and the completed case records `RotationCompleted`. Assert on a
recorder the test controls.

- [ ] **Step 5: Run the package, the repository, and the linter**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/ ./internal/rbacaudit/
nix --extra-experimental-features 'nix-command flakes' develop -c make test
nix --extra-experimental-features 'nix-command flakes' develop -c make lint
```

- [ ] **Step 6: Commit**

---

## Task 6: The proof that crosses the language boundary

**Files:**
- Modify: `hack/agent-test.sh`
- Modify: `cmd/spawnery-stubop/main.go` if it cannot yet be told which CA to
  sign with

This task is described rather than spelled out, and deliberately so: the
script's phases are built out of its own helpers and fixture layout, and a code
block written here without reading them would be a guess presented as a
requirement. Read the script, then follow it.

The claim is: a real agent, with a two-PEM bundle mounted, connects to a server
whose certificate is signed by the **second** of them. Neither the Go tests nor
the Kotlin tests can make it — it needs a real JVM, a real TLS handshake and a
real mounted file, which is what this script already provides for its five
existing phases.

- [ ] **Step 1: Read the script's existing phases first**

Its header explains what each phase proves and why that level is necessary.
Follow the shape of the phase that already exercises the pinned CA rather than
inventing a new one, and put the new phase where its dependencies are already
set up.

- [ ] **Step 2: Add the phase**

The stub serves a certificate signed by a CA that is second in the bundle the
agent mounts. Success is the agent completing a session; failure is a
handshake error, which is what a single-PEM parse in `trustManager` would
produce.

- [ ] **Step 3: State plainly what was and was not run**

The script needs the built images (`IMAGE` must be set) and is not part of
`make test`. If you cannot run it in this environment, **say so in the report
in exactly those words** rather than reporting the phase as passing. An
unrunnable proof reported as a proof is worse than no proof.

- [ ] **Step 4: Commit**

---

## Task 7: The entry is replaced by what is now true

**Files:**
- Modify: `docs/known-issues.md`

- [ ] **Step 1: Rewrite the entry**

Find `**The CA has no rotation procedure.**` under "On the agent channel". After
this branch the heading itself is false. Replace the entry with what is now
the case, keeping three things that stay true:

- the ten-year `CALifetime`, and that nothing watches it — a rotation is asked
  for, never scheduled;
- that a **compromised** CA key is still an emergency with a different recipe,
  because an orderly overlap means continuing to trust the compromised key for
  another quarter of an hour. "Delete the secret, restart all pods" stays the
  answer to that one, and the entry must keep saying so;
- the reference to the design,
  `docs/superpowers/specs/2026-08-21-ca-rotation-design.md`, and to the
  distribution design it was built on.

Write what the procedure is — the three annotations and what each does — so
that somebody who has to rotate at three in the morning does not have to read a
design document first.

- [ ] **Step 2: Check every claim you just made**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c go test ./internal/certs/
```

The entry now asserts behaviour. The tests that prove it must be green at the
moment the claim is written. This repository has spent days removing
documentation that described things which were not so.

- [ ] **Step 3: Run everything and commit**

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make test
nix --extra-experimental-features 'nix-command flakes' develop -c make lint
```

---

## What this plan does not cover

- Rotating on a schedule, or watching the CA's expiry. `CALifetime` stays at
  ten years and nothing looks at it.
- The compromised-key case. An overlap is the wrong shape for it, and the
  emergency recipe stays documented.
- A `hack/e2e.sh` scenario. A rotation takes a quarter of an hour of wall-clock
  by construction, and the parts an end-to-end run could observe are the parts
  envtest observes with a controllable clock.
- Changing `internal/controller/events_test.go`'s AST scan to cover the whole
  repository. It is worth doing and it is not this branch's job; Task 5 records
  that the scan does not reach `internal/certs`.
