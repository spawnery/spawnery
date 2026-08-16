# Milestone 5c — Forwarding Secret Rotation Detection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The operator notices when a Network's Velocity modern forwarding secret changes, reports it as a condition and an event, and stamps every pod it creates with which secret that process loaded — while recreating nothing.

**Architecture:** The Network controller reads `spec.forwardingSecretRef` through an uncached reader once per reconcile, digests the value salted with the Network's UID, and publishes it as `status.forwardingSecretHash`. The pod builders stamp that digest onto every pod as `spawnery.cloud/forwarding-hash`, and both pod-hash functions delete the label before digesting so a rotation never makes a pod stale. Comparing the digest against the live pods' stamps yields two conditions on the Network.

**Tech Stack:** Go, controller-runtime, kubebuilder markers, envtest, `k8s.io/apimachinery`.

**Spec:** `docs/superpowers/specs/2026-08-16-secret-rotation-design.md`

## Global Constraints

- **The forwarding digest must never enter the pod hash.** `DesiredServerHash` and `DesiredProxyHash` delete `podspec.LabelForwardingHash` before digesting. If it entered, a rotation would make every pod of the network stale and the operator would recreate all of them, proxies and backends interleaved — the uncoordinated version of the rollout the master design (§6.5) deliberately defers. Spec §1.1.
- **The operator recreates nothing on rotation.** No new candidate class in `DecidePersistentSize`, no rolling update trigger, no pod deletion. 5c detects and reports; the restarts follow a runbook. Spec §1.
- **The ClusterRole gains no `secrets` grant.** The read is authorised by a per-namespace `Role` shipped in `config/rbac/forwarding-secret-reader.yaml`, applied by an administrator one namespace at a time. `rbacaudit.RequiredCluster` is unchanged and `TestTheAuthorizerActuallyDenies` keeps its `secrets: get @foreignNamespace` denial probe, untouched and still passing. Spec §2.2.1.
- **No `list` and no `watch` on secrets, anywhere.** `internal/rbacaudit/required.go:171` says so and must stay true. Spec §2.2.
- **An empty hash means "unknown", never "stale".** A pod without the stamp does not raise `RotationPending`; it yields `Unknown`. Spec §3, §4.2.
- **The secret's bytes are never trimmed.** A trailing newline is a different digest. Spec §2.3.
- **`status.forwardingSecretHash` is written only after a successful read.** A transient failure leaves the previous value in place. Spec §2.4.
- Every new exported identifier carries a doc comment that says why it exists, matching the density of the surrounding code. Comments state what the code does; a sentence that justifies a step rather than describing it is the defect this repository has caught eight times.
- `make test` (which runs `manifests generate fmt vet` first) must be green at the end of every task.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/podspec/labels.go` | `LabelForwardingHash` constant and its rationale | 1 |
| `internal/podspec/hash.go` | `ForwardingHash`; the two deletions that keep it out of the pod hash | 1, 3 |
| `internal/podspec/hash_test.go` | digest properties; the two "rotation does not move the pod hash" tests | 1, 3 |
| `api/v1alpha1/network_types.go` | `NetworkStatus.ForwardingSecretHash` | 2 |
| `api/v1alpha1/common_types.go` | two condition types, eight reasons, two event reasons | 2 |
| `internal/podspec/server.go`, `proxy.go` | stamping the label onto built pods | 3 |
| `internal/controller/forwardingsecret.go` | reading, classifying, and the two conditions — pure, no reconciler | 4 |
| `internal/controller/forwardingsecret_test.go` | unit tests for all of it, with a stub reader | 4 |
| `config/rbac/forwarding-secret-reader.yaml` | the per-namespace Role and RoleBinding | 5 |
| `internal/rbacaudit/required.go` | `RequiredNetworkNamespace` | 5 |
| `internal/rbacaudit/audit_envtest_test.go` | both audit directions for the new Role, plus a live authorizer probe | 5 |
| `internal/controller/network_controller.go` | wiring: read, status, events, conditions | 6 |
| `internal/controller/setup.go` | `SecretReader: mgr.GetAPIReader()` | 6 |
| `internal/controller/network_controller_test.go` | envtest coverage of the wiring and the events | 6 |
| `docs/runbook-milestone-5c-secret-rotation.md` | the standing operating procedure | 7 |
| `docs/known-issues.md` | the "From milestone 5c" section | 7 |

`cmd/spawnery-operator/main.go` is deliberately **not** in this table: `SetupAll` reaches `mgr.GetAPIReader()` itself, so nothing new is threaded through `controller.Options`.

---

### Task 1: The digest and the label constant

**Files:**
- Modify: `internal/podspec/labels.go:55` (after the `LabelPodHash` block, inside the same `const` group)
- Modify: `internal/podspec/hash.go` (append after `DesiredServerHash`, which ends at line 140)
- Test: `internal/podspec/hash_test.go` (append)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `podspec.LabelForwardingHash` (untyped string constant, value `"spawnery.cloud/forwarding-hash"`) and `podspec.ForwardingHash(networkUID types.UID, value []byte) string`, returning 16 lowercase hex characters. Tasks 3, 4 and 6 use both.

- [ ] **Step 1: Write the failing test**

Append to `internal/podspec/hash_test.go`. The tests are in package `podspec` (in-package), so call the function unqualified, matching the file's existing tests:

```go
// ForwardingHash is what tells a rotation from a steady state, and it becomes
// a pod label, so five separate properties have to hold at once. They are one
// test because each is a single comparison and splitting them would repeat the
// fixture five times.
func TestForwardingHashIsStableSaltedAndUntrimmed(t *testing.T) {
	const uidA = types.UID("11111111-2222-3333-4444-555555555555")
	const uidB = types.UID("99999999-8888-7777-6666-555555555555")
	value := []byte("s3cret")

	got := ForwardingHash(uidA, value)

	if again := ForwardingHash(uidA, value); again != got {
		t.Errorf("ForwardingHash is not stable: %q then %q", got, again)
	}
	if len(got) != 16 {
		t.Errorf("ForwardingHash = %q, want 16 hex characters so it fits a label value", got)
	}
	if _, err := hex.DecodeString(got); err != nil {
		t.Errorf("ForwardingHash returned %q, which is not hex: %v", got, err)
	}
	if other := ForwardingHash(uidB, value); other == got {
		t.Errorf("the same secret in two networks hashed alike (%q); the UID salt is not reaching the digest", got)
	}
	if trailing := ForwardingHash(uidA, []byte("s3cret\n")); trailing == got {
		t.Error("a trailing newline hashed alike; the value is being trimmed somewhere it must not be")
	}
}

// Without a separator between the salt and the value, ("ab", "c") and
// ("a", "bc") are the same byte sequence and hash alike. Real UIDs are all the
// same length so this could never bite in production -- which is exactly why it
// needs a test rather than a reader noticing it.
func TestForwardingHashSeparatesTheSaltFromTheValue(t *testing.T) {
	if ForwardingHash("ab", []byte("c")) == ForwardingHash("a", []byte("bc")) {
		t.Error("the UID and the value run together in the digest; the separator byte is missing")
	}
}
```

Add `"encoding/hex"` and `"k8s.io/apimachinery/pkg/types"` to the test file's imports if they are not already there.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/podspec/ -run TestForwardingHash -v`
Expected: FAIL — `undefined: ForwardingHash`.

- [ ] **Step 3: Add the label constant**

In `internal/podspec/labels.go`, inside the same `const` block, immediately after the `LabelPodHash` declaration:

```go
	// LabelForwardingHash is podspec.ForwardingHash over the Network's
	// forwarding secret as it stood when this pod was created. It says which
	// secret this process read at startup, which is the only thing that
	// matters: the kubelet refreshes the projected file underneath a running
	// pod, and neither Velocity nor Paper reads it a second time.
	//
	// A pod without it is unknown, not stale. The pod builders omit it while
	// Network.status.forwardingSecretHash is empty, and the Network
	// controller reports that case as Unknown rather than raising a rotation
	// nobody can confirm.
	//
	// Deliberately not part of LabelPodHash: DesiredServerHash and
	// DesiredProxyHash delete it before digesting. Were it included, rotating
	// the secret would make every pod of the network stale at once and the
	// operator would recreate all of them, proxies and backends interleaved --
	// the uncoordinated version of the rollout the master design (section 6.5)
	// defers. Rotation is detected and reported; the restarts follow
	// docs/runbook-milestone-5c-secret-rotation.md.
	LabelForwardingHash = "spawnery.cloud/forwarding-hash"
```

- [ ] **Step 4: Add the digest**

Append to `internal/podspec/hash.go`, after `DesiredServerHash`:

```go
// ForwardingHash digests a Network's forwarding secret for LabelForwardingHash:
// the network's UID, a zero byte, then the secret's bytes, truncated to eight
// bytes of SHA-256 like the two pod digests above.
//
// The UID is a salt. This value becomes a pod label, and read access to pods is
// granted far more freely than read access to Secrets, so an unsalted truncated
// digest of a weakly chosen secret would turn "no access to the Secret" into an
// off-the-shelf dictionary attack with the precomputation shared across every
// installation of this operator. Salting per network forces that work to be
// redone for each one. It does not defeat a targeted attack on a weak secret;
// docs/known-issues.md records that rather than dressing it up.
//
// The zero byte keeps the two inputs from running together: without it,
// ("ab", "c") and ("a", "bc") are one byte sequence.
//
// The value is not trimmed. A trailing newline is a different digest and is
// reported as a rotation, because the digest covers exactly the bytes the pod
// mounts; what Velocity and Paper make of them is theirs to decide.
func ForwardingHash(networkUID types.UID, value []byte) string {
	sum := sha256.New()
	sum.Write([]byte(networkUID))
	sum.Write([]byte{0})
	sum.Write(value)
	return hex.EncodeToString(sum.Sum(nil)[:8])
}
```

Add `"k8s.io/apimachinery/pkg/types"` to `hash.go`'s imports.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/podspec/ -run TestForwardingHash -v`
Expected: PASS, both tests.

- [ ] **Step 6: Run the whole suite**

Run: `make test`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/podspec/labels.go internal/podspec/hash.go internal/podspec/hash_test.go
git commit -m "A salted digest of the forwarding secret, and the label that carries it"
```

---

### Task 2: The API surface

**Files:**
- Modify: `api/v1alpha1/network_types.go:33-52` (the `NetworkStatus` struct)
- Modify: `api/v1alpha1/common_types.go:20-61` (the condition-type block) and `:63-84` (the reason block)
- Regenerate: `config/crd/bases/spawnery.cloud_networks.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`
- Test: `api/v1alpha1/network_envtest_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces, all in package `spawneryv1alpha1`, used by Tasks 4 and 6:
  - `NetworkStatus.ForwardingSecretHash string` (json `forwardingSecretHash`, omitempty)
  - `ConditionForwardingSecretResolved`, `ConditionForwardingSecretRotationPending`
  - `ReasonSecretResolved`, `ReasonSecretNotFound`, `ReasonSecretKeyMissing`, `ReasonSecretReadForbidden`, `ReasonSecretReadFailed`
  - `ReasonRotationPending`, `ReasonForwardingSecretInSync`, `ReasonPodsPredateTracking`, `ReasonSecretUnresolved`
  - `EventForwardingSecretRotated`, `EventForwardingSecretNotFound`

- [ ] **Step 1: Write the failing test**

Append to `api/v1alpha1/network_envtest_test.go`. It proves the CRD really carries the new field — a Go struct tag that never reached the schema would be silently dropped by the API server:

```go
// The status field has to survive a round trip through a real API server, not
// only through the Go type: a field missing from the generated CRD schema is
// pruned on write and the operator would re-detect the same rotation forever.
func TestNetworkStatusCarriesTheForwardingSecretHash(t *testing.T) {
	ctx, c, ns := envtestClient(t)

	net := &Network{
		ObjectMeta: metav1.ObjectMeta{Name: "fwd-hash", Namespace: ns},
		Spec:       NetworkSpec{ForwardingSecretRef: ObjectRef{Name: "fwd"}},
	}
	if err := c.Create(ctx, net); err != nil {
		t.Fatalf("create network: %v", err)
	}

	net.Status.ForwardingSecretHash = "0123456789abcdef"
	if err := c.Status().Update(ctx, net); err != nil {
		t.Fatalf("update status: %v", err)
	}

	got := &Network{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(net), got); err != nil {
		t.Fatalf("get network: %v", err)
	}
	if got.Status.ForwardingSecretHash != "0123456789abcdef" {
		t.Errorf("status.forwardingSecretHash = %q, want %q — the field is missing from the generated CRD schema",
			got.Status.ForwardingSecretHash, "0123456789abcdef")
	}
}
```

Read the top of `api/v1alpha1/network_envtest_test.go` first and use whatever helper that file already has for obtaining a client and a namespace; the call above is written as `envtestClient(t)` and must be replaced with the file's actual helper.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./api/v1alpha1/ -run TestNetworkStatusCarriesTheForwardingSecretHash -v`
Expected: FAIL — the field does not exist.

- [ ] **Step 3: Add the status field**

In `api/v1alpha1/network_types.go`, inside `NetworkStatus`, after `OnlinePlayers`:

```go
	// ForwardingSecretHash is podspec.ForwardingHash over this network's
	// forwarding secret as the operator last read it. The pod builders stamp
	// it onto every pod they create (podspec.LabelForwardingHash), which is
	// how a rotation becomes visible: a pod whose stamp differs is running on
	// the previous secret.
	//
	// Written only after a successful read. A read failure leaves the previous
	// value in place, because clearing it would leave every pod created during
	// the failure unstamped, and an unstamped pod is one the operator can say
	// nothing about afterwards.
	// +optional
	ForwardingSecretHash string `json:"forwardingSecretHash,omitempty"`
```

- [ ] **Step 4: Add the condition types**

In `api/v1alpha1/common_types.go`, inside the condition-type `const` block, after `ConditionStorageResize`:

```go
	// ConditionForwardingSecretResolved reports whether this network's
	// forwarding secret can be read and carries a usable value. It is
	// deliberately not folded into Accepted: servergroup_controller.go derives
	// networkUsable from Accepted, proxygroup_controller.go gates on it, and
	// since 5b mayResize equals networkUsable — so reporting an unreadable
	// secret there would stop all sizing for the network, turning a
	// five-second API hiccup into a self-inflicted outage. Accepted keeps its
	// meaning: this Network owns its namespace.
	ConditionForwardingSecretResolved = "ForwardingSecretResolved"
	// ConditionForwardingSecretRotationPending is true while pods of this
	// network run on a forwarding secret that is no longer the current one.
	// The operator reports; it recreates nothing. Neither Velocity nor Paper
	// accepts two forwarding secrets at once, so any rollout has a window in
	// which joins fail, and the master design (section 6.5) leaves the order
	// to a runbook: all server groups first, then all proxy groups.
	//
	// Unknown is a real answer here rather than an omission — see
	// ReasonPodsPredateTracking.
	ConditionForwardingSecretRotationPending = "ForwardingSecretRotationPending"
```

- [ ] **Step 5: Add the reasons and the event reasons**

In the same file, inside the reason `const` block, after `ReasonStorageResizeRefused`:

```go
	// The five ForwardingSecretResolved reasons. Three failures rather than
	// one because each has a different remedy: a name the user can fix, an
	// install step that was skipped, and neither of those.
	ReasonSecretResolved      = "SecretResolved"
	ReasonSecretNotFound      = "SecretNotFound"
	ReasonSecretKeyMissing    = "SecretKeyMissing"
	ReasonSecretReadForbidden = "SecretReadForbidden"
	ReasonSecretReadFailed    = "SecretReadFailed"

	// The four ForwardingSecretRotationPending reasons.
	// ReasonPodsPredateTracking is the Unknown that keeps an operator upgrade
	// from reading as a rotation: after an upgrade no running pod carries a
	// stamp, and calling that True would instruct every user to perform a
	// runbook they do not need.
	ReasonRotationPending        = "RotationPending"
	ReasonForwardingSecretInSync = "ForwardingSecretInSync"
	ReasonPodsPredateTracking    = "PodsPredateTracking"
	ReasonSecretUnresolved       = "SecretUnresolved"
)

// Event reasons. Separate from the condition reasons above because these name
// a transition rather than a state: both are emitted on entering a condition,
// never once per resync.
const (
	// EventForwardingSecretRotated fires when status.forwardingSecretHash
	// moves from a non-empty value to a different one. Empty to a value is
	// adoption, not rotation, and emits nothing.
	EventForwardingSecretRotated = "ForwardingSecretRotated"
	// EventForwardingSecretNotFound fires on entering SecretNotFound. It is
	// the loud channel for a misconfiguration that is otherwise reported under
	// the wrong name: the pods hang in ContainerCreating and the only
	// operator-side account arrives after --startup-deadline as a counted
	// startup failure, which is what a bad image looks like too.
	EventForwardingSecretNotFound = "ForwardingSecretNotFound"
```

Note the closing `)` above `// Event reasons`: the reason block ends there and a new `const` block begins. Keep the file's existing closing parenthesis for the reason block rather than adding a second one.

- [ ] **Step 6: Regenerate and run the test**

Run: `make manifests generate && go test ./api/v1alpha1/ -run TestNetworkStatusCarriesTheForwardingSecretHash -v`
Expected: PASS. `config/crd/bases/spawnery.cloud_networks.yaml` now contains `forwardingSecretHash`.

- [ ] **Step 7: Run the whole suite**

Run: `make test`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add api/v1alpha1/ config/crd/bases/
git commit -m "Two conditions, nine reasons and one status field for the forwarding secret"
```

---

### Task 3: Stamping the pod, and keeping the digest out of the pod hash

This is the load-bearing task of the milestone. Read the Global Constraints again before starting.

**Files:**
- Modify: `internal/podspec/server.go` (in `BuildServerPod`, just before its `return pod, nil`)
- Modify: `internal/podspec/proxy.go` (in `renderProxyPod`, just before its `return`)
- Modify: `internal/podspec/hash.go:67-71` and `:126-129` (the two deletions and their comments)
- Test: `internal/podspec/hash_test.go` (append)

**Interfaces:**
- Consumes: `podspec.LabelForwardingHash` and `podspec.ForwardingHash` (Task 1); `spawneryv1alpha1.NetworkStatus.ForwardingSecretHash` (Task 2).
- Produces: every pod built by `BuildServerPod` and `BuildProxyPod` carries `LabelForwardingHash` when `net.Status.ForwardingSecretHash` is non-empty, and omits the label entirely when it is empty. Task 4 reads those labels.

**Why the stamp goes in the shared render path.** `DesiredServerHash` calls `BuildServerPod` (`hash.go:120`) and `DesiredProxyHash` calls `renderProxyPod` (`hash.go:63`). Stamping there means the label really is present in the digest input, so the deletion in Step 4 is load-bearing and Step 1's tests exercise it. Stamping after the digest instead would make those tests pass vacuously, and a later refactor could reorder the two without any test noticing.

- [ ] **Step 1: Write the failing tests**

Append to `internal/podspec/hash_test.go`:

```go
// The whole of milestone 5c rests on this: if the forwarding stamp reached the
// pod hash, rotating the secret would make every pod of the network stale at
// once and the operator would recreate all of them, proxies and backends
// interleaved. The master design defers that rollout deliberately; this test is
// what keeps it deferred.
func TestRotatingTheForwardingSecretDoesNotMoveTheServerPodHash(t *testing.T) {
	net, group := serverHashFixtures(t)
	values := []byte("maxPlayers: 20\n")

	net.Status.ForwardingSecretHash = "aaaaaaaaaaaaaaaa"
	before, err := DesiredServerHash(net, group, values)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}

	net.Status.ForwardingSecretHash = "bbbbbbbbbbbbbbbb"
	after, err := DesiredServerHash(net, group, values)
	if err != nil {
		t.Fatalf("DesiredServerHash after rotation: %v", err)
	}

	if before != after {
		t.Errorf("the pod hash moved from %q to %q when only the forwarding secret changed; "+
			"a rotation now restarts every server of the group", before, after)
	}
}

func TestRotatingTheForwardingSecretDoesNotMoveTheProxyPodHash(t *testing.T) {
	net, group := proxyHashFixtures(t)
	values := proxyValuesBytes(t, group)

	net.Status.ForwardingSecretHash = "aaaaaaaaaaaaaaaa"
	before, err := DesiredProxyHash(net, group, "operator:9443", values)
	if err != nil {
		t.Fatalf("DesiredProxyHash: %v", err)
	}

	net.Status.ForwardingSecretHash = "bbbbbbbbbbbbbbbb"
	after, err := DesiredProxyHash(net, group, "operator:9443", values)
	if err != nil {
		t.Fatalf("DesiredProxyHash after rotation: %v", err)
	}

	if before != after {
		t.Errorf("the proxy pod hash moved from %q to %q when only the forwarding secret changed; "+
			"a rotation now rolls every proxy group", before, after)
	}
}

// The stamp has to be on the pod for the Network controller to read it back,
// and absent while the operator does not know the digest -- an absent label is
// "unknown", and a wrong one would be a lie about what the process loaded.
func TestServerPodCarriesTheForwardingStampOnlyWhenItIsKnown(t *testing.T) {
	net, group := serverHashFixtures(t)
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "g-0", Namespace: "ns"},
	}

	unknown, err := BuildServerPod(net, group, srv, "operator:9443")
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}
	if _, ok := unknown.Labels[LabelForwardingHash]; ok {
		t.Errorf("labels = %v, want no %s while the network reports no digest",
			unknown.Labels, LabelForwardingHash)
	}

	net.Status.ForwardingSecretHash = "0123456789abcdef"
	known, err := BuildServerPod(net, group, srv, "operator:9443")
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}
	if got := known.Labels[LabelForwardingHash]; got != "0123456789abcdef" {
		t.Errorf("%s = %q, want %q", LabelForwardingHash, got, "0123456789abcdef")
	}
}

func TestProxyPodCarriesTheForwardingStampOnlyWhenItIsKnown(t *testing.T) {
	net, group := proxyHashFixtures(t)
	values := proxyValuesBytes(t, group)

	unknown, err := BuildProxyPod(net, group, "g-0", "operator:9443", values)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	if _, ok := unknown.Labels[LabelForwardingHash]; ok {
		t.Errorf("labels = %v, want no %s while the network reports no digest",
			unknown.Labels, LabelForwardingHash)
	}

	net.Status.ForwardingSecretHash = "0123456789abcdef"
	known, err := BuildProxyPod(net, group, "g-0", "operator:9443", values)
	if err != nil {
		t.Fatalf("BuildProxyPod: %v", err)
	}
	if got := known.Labels[LabelForwardingHash]; got != "0123456789abcdef" {
		t.Errorf("%s = %q, want %q", LabelForwardingHash, got, "0123456789abcdef")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/podspec/ -run 'TestRotatingTheForwardingSecret|CarriesTheForwardingStamp' -v`
Expected: the two `CarriesTheForwardingStamp` tests FAIL (the label is never set). The two `Rotating` tests PASS vacuously at this point, because nothing puts the digest into the pod yet — they become meaningful only after Step 3, which is why Step 5 runs them again.

- [ ] **Step 3: Stamp the label in both builders**

In `internal/podspec/server.go`, immediately before `BuildServerPod`'s `return pod, nil`:

```go
	// Stamped from the Network's status rather than computed here: one reader
	// of the Secret is the whole point (design section 2.1), and the group
	// controllers copy a string out of an object they already hold. Empty
	// means the operator does not know the digest yet, and an absent label is
	// "unknown" — see LabelForwardingHash.
	if hash := net.Status.ForwardingSecretHash; hash != "" {
		pod.Labels[LabelForwardingHash] = hash
	}
```

In `internal/podspec/proxy.go`, immediately before `renderProxyPod`'s `return`, add the same block with the same comment. It goes in `renderProxyPod` and not in `BuildProxyPod` so that `DesiredProxyHash`, which calls `renderProxyPod` directly, really sees the label and really has to delete it.

- [ ] **Step 4: Delete it in both hash functions**

In `internal/podspec/hash.go`, replace the deletion in `DesiredProxyHash` (currently lines 67-71) with:

```go
	// Two labels come out before the digest, for two different reasons.
	// LabelPodHash is belt-and-braces: renderProxyPod never sets it, and this
	// keeps the digest right if that ever stops being true rather than feeding
	// the label back into itself. LabelForwardingHash is not belt-and-braces —
	// renderProxyPod does set it, and removing it here is what stops a rotated
	// forwarding secret from making every proxy of the group stale at once.
	// See LabelForwardingHash's own comment.
	delete(subject.Labels, LabelPodHash)
	delete(subject.Labels, LabelForwardingHash)
```

Replace the deletion in `DesiredServerHash` (currently lines 126-129) with:

```go
	// Two labels come out, for the two reasons DesiredProxyHash gives:
	// LabelPodHash is belt-and-braces because BuildServerPod never sets it,
	// and LabelForwardingHash is not, because BuildServerPod does — removing
	// it here is what stops a rotated forwarding secret from making every
	// server of the group stale at once.
	delete(subject.Labels, LabelPodHash)
	delete(subject.Labels, LabelForwardingHash)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/podspec/ -run 'TestRotatingTheForwardingSecret|CarriesTheForwardingStamp' -v`
Expected: PASS, all four. The two `Rotating` tests are now meaningful: comment out either `delete(subject.Labels, LabelForwardingHash)` line and the corresponding test must fail. Do that check once by hand, confirm the failure, then restore the line.

- [ ] **Step 6: Run the whole suite**

Run: `make test`
Expected: PASS. Watch in particular for `TestDesiredServerHashDiscriminates` and `TestPodHashMatchesWhatTheOperatorStamped` in the same file: neither sets `net.Status.ForwardingSecretHash`, so neither changes.

- [ ] **Step 7: Commit**

```bash
git add internal/podspec/
git commit -m "Every pod says which forwarding secret it loaded, and the pod hash never hears about it"
```

---

### Task 4: Reading and classifying

Pure logic with no reconciler and no envtest: everything here takes its inputs as arguments, so a stub `client.Reader` covers the failure paths — including `Forbidden`, which a test running as envtest's admin could not otherwise reach.

**Files:**
- Create: `internal/controller/forwardingsecret.go`
- Test: `internal/controller/forwardingsecret_test.go`

**Interfaces:**
- Consumes: `podspec.ForwardingHash`, `podspec.LabelForwardingHash`, `podspec.LabelGroup`, `podspec.LabelRole`, `podspec.RoleServer`, `podspec.ForwardingSecretKey` (Task 1 and existing); the conditions, reasons and event reasons of Task 2.
- Produces, all unexported, in package `controller`, used by Task 6:
  - `type forwardingRead struct { Hash, Reason, Message string; Status metav1.ConditionStatus }`
  - `readForwardingSecret(ctx context.Context, reader client.Reader, net *spawneryv1alpha1.Network) forwardingRead`
  - `resolvedCondition(read forwardingRead) metav1.Condition`
  - `type forwardingStamp struct { Group, Role, Hash string }`
  - `forwardingStamps(pods []corev1.Pod) []forwardingStamp`
  - `rotationCondition(read forwardingRead, stamps []forwardingStamp) metav1.Condition`
  - `const rotationRunbook = "docs/runbook-milestone-5c-secret-rotation.md"`

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/forwardingsecret_test.go`:

```go
package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// stubReader answers every Get with the same secret or the same error, which
// is what makes Forbidden testable at all: a test running against envtest holds
// admin credentials and can never be denied.
type stubReader struct {
	secret *corev1.Secret
	err    error
}

func (s stubReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if s.err != nil {
		return s.err
	}
	*obj.(*corev1.Secret) = *s.secret
	return nil
}

func (s stubReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	panic("readForwardingSecret must not list")
}

func testNetworkForSecret() *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "mc", UID: "u-1"},
		Spec:       spawneryv1alpha1.NetworkSpec{ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "fwd"}},
	}
}

func secretsResource() schema.GroupResource {
	return schema.GroupResource{Resource: "secrets"}
}

// Each read outcome has its own remedy, so each gets its own reason. A single
// "could not read it" would send a user with a typo and a user with a missing
// RoleBinding to the same place.
func TestReadForwardingSecretNamesEachOutcome(t *testing.T) {
	net := testNetworkForSecret()

	for _, tc := range []struct {
		name       string
		reader     stubReader
		wantStatus metav1.ConditionStatus
		wantReason string
		wantHash   bool
	}{
		{
			name: "readable",
			reader: stubReader{secret: &corev1.Secret{
				Data: map[string][]byte{podspec.ForwardingSecretKey: []byte("s3cret")},
			}},
			wantStatus: metav1.ConditionTrue,
			wantReason: spawneryv1alpha1.ReasonSecretResolved,
			wantHash:   true,
		},
		{
			name:       "absent",
			reader:     stubReader{err: apierrors.NewNotFound(secretsResource(), "fwd")},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonSecretNotFound,
		},
		{
			name:       "denied",
			reader:     stubReader{err: apierrors.NewForbidden(secretsResource(), "fwd", nil)},
			wantStatus: metav1.ConditionUnknown,
			wantReason: spawneryv1alpha1.ReasonSecretReadForbidden,
		},
		{
			name:       "api down",
			reader:     stubReader{err: apierrors.NewServiceUnavailable("etcd is having a day")},
			wantStatus: metav1.ConditionUnknown,
			wantReason: spawneryv1alpha1.ReasonSecretReadFailed,
		},
		{
			name:       "no key",
			reader:     stubReader{secret: &corev1.Secret{Data: map[string][]byte{"other": []byte("x")}}},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonSecretKeyMissing,
		},
		{
			name: "empty key",
			reader: stubReader{secret: &corev1.Secret{
				Data: map[string][]byte{podspec.ForwardingSecretKey: {}},
			}},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonSecretKeyMissing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readForwardingSecret(context.Background(), tc.reader, net)

			if got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Errorf("read = %s/%s, want %s/%s", got.Status, got.Reason, tc.wantStatus, tc.wantReason)
			}
			if (got.Hash != "") != tc.wantHash {
				t.Errorf("hash = %q, want non-empty: %v", got.Hash, tc.wantHash)
			}
			if got.Message == "" {
				t.Error("message is empty; every outcome has to say what happened")
			}
		})
	}
}

// The Forbidden message is the only place an administrator learns that an
// install step was skipped, so it has to name the manifest.
func TestForbiddenNamesTheManifestToApply(t *testing.T) {
	got := readForwardingSecret(context.Background(),
		stubReader{err: apierrors.NewForbidden(secretsResource(), "fwd", nil)}, testNetworkForSecret())

	if !strings.Contains(got.Message, "config/rbac/forwarding-secret-reader.yaml") {
		t.Errorf("message = %q, want it to name config/rbac/forwarding-secret-reader.yaml", got.Message)
	}
	if !strings.Contains(got.Message, "mc") {
		t.Errorf("message = %q, want it to name the namespace so the apply can be copied", got.Message)
	}
}

func stampPod(group, role, hash string, terminating bool) corev1.Pod {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      group + "-" + role,
		Namespace: "mc",
		Labels: map[string]string{
			podspec.LabelGroup: group,
			podspec.LabelRole:  role,
		},
	}}
	if hash != "" {
		pod.Labels[podspec.LabelForwardingHash] = hash
	}
	if terminating {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"test/hold"}
	}
	return pod
}

func TestForwardingStampsSkipTerminatingPods(t *testing.T) {
	got := forwardingStamps([]corev1.Pod{
		stampPod("lobby", podspec.RoleServer, "aaaa", false),
		stampPod("edge", podspec.RoleProxy, "aaaa", true),
	})

	if len(got) != 1 || got[0].Group != "lobby" {
		t.Errorf("stamps = %+v, want only the lobby pod; a pod on its way out must not hold the report open", got)
	}
}

func TestRotationConditionFollowsItsPrecedence(t *testing.T) {
	resolved := forwardingRead{Hash: "aaaa", Status: metav1.ConditionTrue, Reason: spawneryv1alpha1.ReasonSecretResolved}
	unresolved := forwardingRead{Status: metav1.ConditionUnknown, Reason: spawneryv1alpha1.ReasonSecretNotFound, Message: "no such secret"}

	for _, tc := range []struct {
		name       string
		read       forwardingRead
		pods       []corev1.Pod
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "an unreadable secret outranks everything",
			read:       unresolved,
			pods:       []corev1.Pod{stampPod("lobby", podspec.RoleServer, "bbbb", false)},
			wantStatus: metav1.ConditionUnknown,
			wantReason: spawneryv1alpha1.ReasonSecretUnresolved,
		},
		{
			name:       "a stale pod outranks an unstamped one",
			read:       resolved,
			pods:       []corev1.Pod{stampPod("lobby", podspec.RoleServer, "bbbb", false), stampPod("edge", podspec.RoleProxy, "", false)},
			wantStatus: metav1.ConditionTrue,
			wantReason: spawneryv1alpha1.ReasonRotationPending,
		},
		{
			name:       "an unstamped pod alone is unknown, never pending",
			read:       resolved,
			pods:       []corev1.Pod{stampPod("lobby", podspec.RoleServer, "", false)},
			wantStatus: metav1.ConditionUnknown,
			wantReason: spawneryv1alpha1.ReasonPodsPredateTracking,
		},
		{
			name:       "all current is in sync",
			read:       resolved,
			pods:       []corev1.Pod{stampPod("lobby", podspec.RoleServer, "aaaa", false)},
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonForwardingSecretInSync,
		},
		{
			name:       "no pods at all is vacuously in sync",
			read:       resolved,
			wantStatus: metav1.ConditionFalse,
			wantReason: spawneryv1alpha1.ReasonForwardingSecretInSync,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rotationCondition(tc.read, forwardingStamps(tc.pods))

			if got.Type != spawneryv1alpha1.ConditionForwardingSecretRotationPending {
				t.Errorf("type = %q, want %q", got.Type, spawneryv1alpha1.ConditionForwardingSecretRotationPending)
			}
			if got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Errorf("condition = %s/%s, want %s/%s", got.Status, got.Reason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// The message is read by somebody about to execute the roll, so it lists the
// work in the order they are to do it: every server group before every proxy
// group, each sorted by name.
func TestRotationMessageListsServersBeforeProxies(t *testing.T) {
	read := forwardingRead{Hash: "aaaa", Status: metav1.ConditionTrue, Reason: spawneryv1alpha1.ReasonSecretResolved}
	got := rotationCondition(read, forwardingStamps([]corev1.Pod{
		stampPod("edge", podspec.RoleProxy, "bbbb", false),
		stampPod("survival", podspec.RoleServer, "bbbb", false),
		stampPod("lobby", podspec.RoleServer, "bbbb", false),
	}))

	want := "server/lobby=1, server/survival=1, proxy/edge=1"
	if !strings.Contains(got.Message, want) {
		t.Errorf("message = %q, want it to contain %q", got.Message, want)
	}
	if !strings.Contains(got.Message, rotationRunbook) {
		t.Errorf("message = %q, want it to name %s", got.Message, rotationRunbook)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run 'ForwardingSecret|ForwardingStamps|RotationCondition|RotationMessage|Forbidden' -v`
Expected: FAIL to compile — `undefined: readForwardingSecret`.

- [ ] **Step 3: Write the implementation**

Create `internal/controller/forwardingsecret.go` with the standard Apache licence header this repository puts on every Go file (copy it from `internal/controller/network_controller.go:1-15`), then:

```go
package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/podspec"
)

// rotationRunbook is where the condition messages and the rotation event send
// an operator. Named once so the three cannot drift apart.
const rotationRunbook = "docs/runbook-milestone-5c-secret-rotation.md"

// forwardingRead is what one attempt at reading a Network's forwarding secret
// produced: the digest when it worked, and in every case the
// ForwardingSecretResolved condition it justifies.
type forwardingRead struct {
	// Hash is podspec.ForwardingHash over the secret's value, empty unless the
	// read succeeded and the value was usable. Callers test this rather than
	// Reason, because "is there a digest to compare against" is the question
	// every one of them is actually asking.
	Hash string
	// Status, Reason and Message make up ForwardingSecretResolved.
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// readForwardingSecret fetches the Network's forwarding secret and digests it.
//
// The reader is an argument rather than a field of the reconciler because it
// has to be the uncached one: a cached Secret would need an informer over every
// Secret in scope, and this operator holds no list or watch on them.
func readForwardingSecret(ctx context.Context, reader client.Reader, net *spawneryv1alpha1.Network) forwardingRead {
	name := net.Spec.ForwardingSecretRef.Name
	secret := &corev1.Secret{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: net.Namespace, Name: name}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return forwardingRead{
			Status: metav1.ConditionFalse,
			Reason: spawneryv1alpha1.ReasonSecretNotFound,
			Message: fmt.Sprintf("spec.forwardingSecretRef names secret %q, which does not exist in namespace %q",
				name, net.Namespace),
		}
	case apierrors.IsForbidden(err):
		return forwardingRead{
			Status: metav1.ConditionUnknown,
			Reason: spawneryv1alpha1.ReasonSecretReadForbidden,
			Message: fmt.Sprintf("the operator may not read secret %q in namespace %q; grant it with "+
				"kubectl apply -n %s -f config/rbac/forwarding-secret-reader.yaml",
				name, net.Namespace, net.Namespace),
		}
	case err != nil:
		return forwardingRead{
			Status:  metav1.ConditionUnknown,
			Reason:  spawneryv1alpha1.ReasonSecretReadFailed,
			Message: fmt.Sprintf("reading secret %q in namespace %q failed: %v", name, net.Namespace, err),
		}
	}

	value := secret.Data[podspec.ForwardingSecretKey]
	if len(value) == 0 {
		return forwardingRead{
			Status: metav1.ConditionFalse,
			Reason: spawneryv1alpha1.ReasonSecretKeyMissing,
			Message: fmt.Sprintf("secret %q carries no non-empty %q key, which is where the Velocity "+
				"modern forwarding secret belongs", name, podspec.ForwardingSecretKey),
		}
	}

	return forwardingRead{
		Hash:    podspec.ForwardingHash(net.UID, value),
		Status:  metav1.ConditionTrue,
		Reason:  spawneryv1alpha1.ReasonSecretResolved,
		Message: fmt.Sprintf("secret %q carries a %q key", name, podspec.ForwardingSecretKey),
	}
}

// resolvedCondition turns a read into the condition it justifies.
func resolvedCondition(read forwardingRead) metav1.Condition {
	return metav1.Condition{
		Type:    spawneryv1alpha1.ConditionForwardingSecretResolved,
		Status:  read.Status,
		Reason:  read.Reason,
		Message: read.Message,
	}
}

// forwardingStamp is one running pod's contribution to the rotation report.
type forwardingStamp struct {
	Group string
	Role  string
	// Hash is podspec.LabelForwardingHash, empty when the pod carries none.
	Hash string
}

// forwardingStamps reduces a network's pods to what the rotation report needs.
// Pods with a DeletionTimestamp are dropped: one on its way out must not hold
// the report open after the replacement that fixes it already exists.
func forwardingStamps(pods []corev1.Pod) []forwardingStamp {
	stamps := make([]forwardingStamp, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		stamps = append(stamps, forwardingStamp{
			Group: pod.Labels[podspec.LabelGroup],
			Role:  pod.Labels[podspec.LabelRole],
			Hash:  pod.Labels[podspec.LabelForwardingHash],
		})
	}
	return stamps
}

// rotationCondition decides ForwardingSecretRotationPending, in this
// precedence: an unreadable secret, then a stale pod, then an unstamped one. A
// known problem outranks an unknown one.
func rotationCondition(read forwardingRead, stamps []forwardingStamp) metav1.Condition {
	cond := metav1.Condition{Type: spawneryv1alpha1.ConditionForwardingSecretRotationPending}

	if read.Hash == "" {
		cond.Status = metav1.ConditionUnknown
		cond.Reason = spawneryv1alpha1.ReasonSecretUnresolved
		cond.Message = "the forwarding secret could not be read, so whether a rotation is pending " +
			"cannot be told: " + read.Message
		return cond
	}

	stale := map[string]int{}
	untracked := 0
	for _, s := range stamps {
		switch {
		case s.Hash == "":
			untracked++
		case s.Hash != read.Hash:
			stale[s.Role+"/"+s.Group]++
		}
	}

	switch {
	case len(stale) > 0:
		cond.Status = metav1.ConditionTrue
		cond.Reason = spawneryv1alpha1.ReasonRotationPending
		cond.Message = fmt.Sprintf("still on the previous forwarding secret: %s; roll the server "+
			"groups first, then the proxy groups — see %s", staleSummary(stale), rotationRunbook)
	case untracked > 0:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = spawneryv1alpha1.ReasonPodsPredateTracking
		cond.Message = fmt.Sprintf("%d pod(s) carry no forwarding stamp, so whether they run on the "+
			"current secret cannot be told; they were created before this operator stamped it and "+
			"clear as pods turn over", untracked)
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = spawneryv1alpha1.ReasonForwardingSecretInSync
		cond.Message = "every pod of this network runs on the current forwarding secret"
	}
	return cond
}

// staleSummary renders the stale counts as role/group=count, every server entry
// before every proxy entry and each sorted by name. The order is the runbook's:
// whoever reads this message is about to do the work it lists.
func staleSummary(stale map[string]int) string {
	keys := make([]string, 0, len(stale))
	for k := range stale {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		iServer := strings.HasPrefix(keys[i], podspec.RoleServer+"/")
		jServer := strings.HasPrefix(keys[j], podspec.RoleServer+"/")
		if iServer != jServer {
			return iServer
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, stale[k]))
	}
	return strings.Join(parts, ", ")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'ForwardingSecret|ForwardingStamps|RotationCondition|RotationMessage|Forbidden' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `make test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/forwardingsecret.go internal/controller/forwardingsecret_test.go
git commit -m "Read the secret, sort the pods, and say which of the four things is true"
```

---

### Task 5: The per-namespace reader Role and its audit

Independent of Tasks 1, 3, 4 and 6 — it touches no Go code they touch. Read the third Global Constraint before starting.

**Files:**
- Create: `config/rbac/forwarding-secret-reader.yaml`
- Modify: `internal/rbacaudit/required.go` (append a third table after `RequiredNamespaced`, which ends at line 184)
- Test: `internal/rbacaudit/audit_envtest_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces: `rbacaudit.RequiredNetworkNamespace []Permission`. Nothing else depends on it.

**The trap in this task, stated up front.** `TestTheAuthorizerActuallyDenies` (`internal/rbacaudit/audit_envtest_test.go:182`) requires `secrets: get` in `foreignNamespace` (`"minecraft"`) to be **denied**. The new test applies a RoleBinding that grants exactly that. If it applies into `foreignNamespace`, the two tests share one envtest API server and the suite becomes order-dependent — passing or failing by which ran first. The new test therefore uses **its own namespace**, and `foreignNamespace` stays clean. Do not touch `TestTheAuthorizerActuallyDenies`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/rbacaudit/audit_envtest_test.go`. Match the file's existing helpers — `applyDeploymentAndDeriveSubject`, `allowed`, `assertNothingExtra`, `requireGranted` — reading their signatures before writing:

```go
// readerProbeNamespace is deliberately not foreignNamespace. The Role applied
// below grants secrets/get, which is exactly what TestTheAuthorizerActuallyDenies
// requires to stay denied in foreignNamespace — applying it there would make
// this suite pass or fail by test order.
const readerProbeNamespace = "spawnery-reader-probe"

// The reader Role is hand-written rather than generated from a marker, because
// the namespace is not known until an administrator applies it. Both directions
// of the audit therefore matter more here, not less: nothing else compares this
// file against anything.
func TestTheForwardingSecretReaderGrantsNothingExtra(t *testing.T) {
	role, _ := readForwardingSecretReader(t)
	assertNothingExtra(t, "forwarding-secret-reader role", role.Rules, rbacaudit.RequiredNetworkNamespace)
}

func TestTheForwardingSecretReaderGrantsEverythingRequired(t *testing.T) {
	role, _ := readForwardingSecretReader(t)
	granted, err := rbacaudit.ExpandRules(role.Rules)
	if err != nil {
		t.Fatalf("expand rules: %v", err)
	}
	if diff := rbacaudit.Compare(rbacaudit.RequiredNetworkNamespace, granted); len(diff.Missing) > 0 {
		t.Errorf("the reader role is missing %v — the operator cannot read a forwarding secret "+
			"even where an administrator applied it", diff.Missing)
	}
}

// The file has to work when applied, not only when parsed: a RoleBinding whose
// subject names the wrong ServiceAccount parses perfectly and grants nothing.
func TestTheForwardingSecretReaderOpensExactlyOneNamespace(t *testing.T) {
	subject := applyDeploymentAndDeriveSubject(t)
	applyForwardingSecretReader(t, readerProbeNamespace)

	if ok, reason := allowed(t, subject, authzv1.ResourceAttributes{
		Namespace: readerProbeNamespace, Resource: "secrets", Verb: "get",
	}); !ok {
		t.Errorf("secrets/get is denied in %s after applying the reader role — reason: %q",
			readerProbeNamespace, reason)
	}

	for _, verb := range []string{"list", "watch", "create", "update", "delete"} {
		t.Run(verb, func(t *testing.T) {
			if ok, _ := allowed(t, subject, authzv1.ResourceAttributes{
				Namespace: readerProbeNamespace, Resource: "secrets", Verb: verb,
			}); ok {
				t.Errorf("the reader role allows secrets/%s in %s; it exists to grant get and "+
					"nothing else", verb, readerProbeNamespace)
			}
		})
	}
}
```

Write two helpers beside them. `readForwardingSecretReader(t) (*rbacv1.Role, *rbacv1.RoleBinding)` reads and decodes `config/rbac/forwarding-secret-reader.yaml` — model it on `readGeneratedRoles`, which is already in this file and already solves finding the repository root and splitting a multi-document YAML. `applyForwardingSecretReader(t, namespace)` creates the probe namespace if absent, then creates the decoded Role and RoleBinding with `metadata.namespace` set to it — which is what an administrator's `kubectl apply -n` does.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/rbacaudit/ -run ForwardingSecretReader -v`
Expected: FAIL — `undefined: rbacaudit.RequiredNetworkNamespace`, and the manifest does not exist.

- [ ] **Step 3: Write the manifest**

Create `config/rbac/forwarding-secret-reader.yaml`:

```yaml
# The operator reads each Network's forwarding secret to detect a rotation of
# the Velocity modern forwarding secret. This file is what authorises that,
# one namespace at a time.
#
# Deliberately NOT part of config/deploy/. The ClusterRole grants no access to
# secrets outside the operator's own namespace, and an administrator opens
# exactly the namespaces that hold a Network:
#
#   kubectl apply -n <network-namespace> -f config/rbac/forwarding-secret-reader.yaml
#
# Neither object carries metadata.namespace, so the apply supplies it. The
# operator never creates these itself: one that may write RBAC makes every
# other restriction on it advisory, which is why the same audit denies it
# clusterroles/create.
#
# A namespace where nobody applied this reports itself — the Network's
# ForwardingSecretResolved condition reads Unknown/SecretReadForbidden and its
# message names this file. Milestone 6's Helm chart is where rendering it for
# each configured namespace belongs.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: spawnery-forwarding-secret-reader
  labels:
    app.kubernetes.io/name: spawnery
rules:
- apiGroups:
  - ""
  resources:
  - secrets
  verbs:
  - get
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: spawnery-forwarding-secret-reader
  labels:
    app.kubernetes.io/name: spawnery
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: spawnery-forwarding-secret-reader
subjects:
- kind: ServiceAccount
  name: spawnery-operator
  namespace: spawnery-system
```

- [ ] **Step 4: Add the third table**

In `internal/rbacaudit/required.go`, after `RequiredNamespaced`:

```go
// RequiredNetworkNamespace is what the operator needs in every namespace that
// holds a Network. It is granted by config/rbac/forwarding-secret-reader.yaml
// rather than by the ClusterRole, and an administrator applies it per
// namespace.
//
// Not one line in RequiredCluster, which is what it would have been: a
// cluster-wide secrets/get makes the operator's ServiceAccount a reader of
// every Secret in the cluster. "get without list means you must know the name"
// carries less than it sounds like — Secret names are visible in the pod specs
// this operator already lists. TestTheAuthorizerActuallyDenies probes
// secrets/get in a foreign namespace and requires a denial; that probe is right
// and stays.
//
// Unlike the other two tables this one is compared against a hand-written
// manifest rather than a generated one, so both directions of the comparison
// are the only thing checking that file at all.
var RequiredNetworkNamespace = []Permission{
	{Group: "", Resource: "secrets", Verb: "get", Why: "readForwardingSecret digests the forwarding secret to detect a rotation"},
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/rbacaudit/ -v`
Expected: PASS — including `TestTheAuthorizerActuallyDenies`, whose `secrets/get@minecraft` subtest must still pass. If it fails, the new test applied its RoleBinding into the wrong namespace.

- [ ] **Step 6: Confirm the ClusterRole did not move**

Run: `make manifests && git diff --stat config/rbac/role.yaml`
Expected: no diff. This task adds no kubebuilder marker, so the generated role is untouched — which is the third Global Constraint, checked rather than assumed.

- [ ] **Step 7: Run the whole suite and commit**

Run: `make test`
Expected: PASS.

```bash
git add config/rbac/forwarding-secret-reader.yaml internal/rbacaudit/
git commit -m "One namespace at a time: the reader role, and both directions of its audit"
```

---

### Task 6: Wiring the Network controller

**Files:**
- Modify: `internal/controller/network_controller.go:36-42` (the struct), `:44-46` (the markers), `:76-110` (the accepted branch of `Reconcile`)
- Modify: `internal/controller/forwardingsecret.go` (created by Task 4 — append `hasConditionReason` at the bottom, per Step 4)
- Modify: `internal/controller/setup.go:84-89` (the `NetworkReconciler` construction)
- Test: `internal/controller/network_controller_test.go` (modify `networkReconciler` at line 32, and append tests)

**Interfaces:**
- Consumes: everything Tasks 2 and 4 produce; `podspec.LabelManagedBy`, `podspec.ManagedByValue`, `podspec.LabelNetwork`.
- Produces: `NetworkReconciler.SecretReader client.Reader`. Nothing later depends on it.

- [ ] **Step 1: Write the failing tests**

First change the fixture helper at `internal/controller/network_controller_test.go:32` so tests can read the events back, keeping every existing call site working:

```go
func networkReconciler(f *fixture) *NetworkReconciler {
	r, _ := networkReconcilerWithEvents(f)
	return r
}

// networkReconcilerWithEvents hands back the recorder too, which the forwarding
// secret tests need: the events are emitted on entering a state, so proving
// "exactly once" means reading the channel rather than the object.
func networkReconcilerWithEvents(f *fixture) (*NetworkReconciler, *record.FakeRecorder) {
	events := record.NewFakeRecorder(100)
	return &NetworkReconciler{
		Client:       f.c,
		Scheme:       f.reconc.Scheme,
		Recorder:     events,
		Clock:        f.clock.Now,
		SecretReader: f.c,
	}, events
}
```

Then append the tests. `f.ns` is the fixture's namespace and its Network is named `production`; `drainEvents` collects what a reconcile emitted:

```go
func drainEvents(events *record.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-events.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

func putForwardingSecret(t *testing.T, f *fixture, value string) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "forwarding-secret", Namespace: f.ns},
		Data:       map[string][]byte{podspec.ForwardingSecretKey: []byte(value)},
	}
	if err := f.c.Create(f.ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create secret: %v", err)
		}
		existing := &corev1.Secret{}
		if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(secret), existing); err != nil {
			t.Fatalf("get secret: %v", err)
		}
		existing.Data = secret.Data
		if err := f.c.Update(f.ctx, existing); err != nil {
			t.Fatalf("update secret: %v", err)
		}
	}
}

// The first sight of a secret is adoption, not rotation. Emitting an event
// there would mean every operator start announces a rotation that never
// happened, on every network at once.
func TestFirstSightOfTheForwardingSecretIsAdoption(t *testing.T) {
	f := newFixture(t)
	r, events := networkReconcilerWithEvents(f)
	putForwardingSecret(t, f, "first")

	f.reconcileNetwork(t, r, "production")

	got := f.getNetwork(t, "production")
	if got.Status.ForwardingSecretHash == "" {
		t.Error("status.forwardingSecretHash is empty after a successful read")
	}
	for _, e := range drainEvents(events) {
		if strings.Contains(e, spawneryv1alpha1.EventForwardingSecretRotated) {
			t.Errorf("the first read emitted %q; an empty recorded hash is adoption", e)
		}
	}
}

// The event fires on the transition and not once per resync: at a five-second
// requeue, an event per pass would be seven hundred an hour for one unremedied
// rotation.
func TestARotationIsAnnouncedExactlyOnce(t *testing.T) {
	f := newFixture(t)
	r, events := networkReconcilerWithEvents(f)
	putForwardingSecret(t, f, "first")
	f.reconcileNetwork(t, r, "production")
	drainEvents(events)

	putForwardingSecret(t, f, "second")
	f.reconcileNetwork(t, r, "production")
	first := drainEvents(events)
	f.reconcileNetwork(t, r, "production")
	second := drainEvents(events)

	if n := countEvents(first, spawneryv1alpha1.EventForwardingSecretRotated); n != 1 {
		t.Errorf("the rotation emitted %d events, want exactly 1: %v", n, first)
	}
	if n := countEvents(second, spawneryv1alpha1.EventForwardingSecretRotated); n != 0 {
		t.Errorf("the next reconcile emitted %d more events, want 0: %v", n, second)
	}
}

func countEvents(events []string, reason string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, reason) {
			n++
		}
	}
	return n
}

// A stale pod is the whole signal. It is created by hand here rather than by a
// group controller, because what is under test is the comparison and not how
// pods come to exist.
func TestAStalePodRaisesRotationPending(t *testing.T) {
	f := newFixture(t)
	r, _ := networkReconcilerWithEvents(f)
	putForwardingSecret(t, f, "first")
	f.reconcileNetwork(t, r, "production")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lobby-0",
			Namespace: f.ns,
			Labels: map[string]string{
				podspec.LabelManagedBy:       podspec.ManagedByValue,
				podspec.LabelNetwork:         "production",
				podspec.LabelGroup:           "lobby",
				podspec.LabelRole:            podspec.RoleServer,
				podspec.LabelForwardingHash:  "0000000000000000",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "paper", Image: "img:1"}}},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	f.reconcileNetwork(t, r, "production")

	got := f.getNetwork(t, "production")
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionForwardingSecretRotationPending,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonRotationPending) {
		t.Errorf("conditions = %+v, want RotationPending=True/RotationPending", got.Status.Conditions)
	}
}

// Accepted is what servergroup_controller.go derives networkUsable from, and
// since 5b mayResize equals networkUsable. A missing secret must not reach it,
// or a typo in one field stops the network from sizing at all.
func TestAMissingSecretLeavesAcceptedAlone(t *testing.T) {
	f := newFixture(t)
	r, events := networkReconcilerWithEvents(f)

	f.reconcileNetwork(t, r, "production")

	got := f.getNetwork(t, "production")
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionAccepted,
		metav1.ConditionTrue, spawneryv1alpha1.ReasonAccepted) {
		t.Errorf("conditions = %+v, want Accepted=True despite the missing secret", got.Status.Conditions)
	}
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionForwardingSecretResolved,
		metav1.ConditionFalse, spawneryv1alpha1.ReasonSecretNotFound) {
		t.Errorf("conditions = %+v, want ForwardingSecretResolved=False/SecretNotFound", got.Status.Conditions)
	}
	if !hasCondition(got.Status.Conditions, spawneryv1alpha1.ConditionForwardingSecretRotationPending,
		metav1.ConditionUnknown, spawneryv1alpha1.ReasonSecretUnresolved) {
		t.Errorf("conditions = %+v, want RotationPending=Unknown/SecretUnresolved", got.Status.Conditions)
	}
	if n := countEvents(drainEvents(events), spawneryv1alpha1.EventForwardingSecretNotFound); n != 1 {
		t.Errorf("the missing secret emitted %d events, want exactly 1", n)
	}
}
```

The fixture's Network must reference a secret named `forwarding-secret`. Read `newFixture` in `internal/controller/suite_test.go` first: if its Network names a different secret, use that name in `putForwardingSecret` rather than renaming the fixture.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run 'ForwardingSecret|RotationPending|StalePod|Adoption|AnnouncedExactlyOnce|MissingSecret' -v`
Expected: FAIL to compile — `SecretReader` is not a field of `NetworkReconciler`.

- [ ] **Step 3: Add the field and the marker**

In `internal/controller/network_controller.go`, add to the struct after `Recorder`:

```go
	// SecretReader reads the Network's forwarding secret. It must be an
	// uncached reader — mgr.GetAPIReader(), which setup.go supplies: a cached
	// Secret would need an informer over every Secret in scope, and this
	// operator deliberately holds no list or watch on them
	// (internal/rbacaudit/required.go).
	SecretReader client.Reader
```

Add one marker beside the existing ones. It documents this controller's need and changes nothing in the generated role, because the Server controller already grants `pods: list` cluster-wide:

```go
// +kubebuilder:rbac:groups="",resources=pods,verbs=list
```

Add **no** marker for secrets. That grant is `config/rbac/forwarding-secret-reader.yaml`, applied per namespace, and a marker here would put it in the ClusterRole — which is the third Global Constraint.

- [ ] **Step 4: Do the work in Reconcile**

In `internal/controller/network_controller.go`, after the group counting and before the final `return`, insert:

```go
	// The forwarding secret. This sits after the Accepted branch above returns,
	// so a Network that does not own its namespace never reads a secret it does
	// not manage.
	read := readForwardingSecret(ctx, r.SecretReader, network)
	if read.Hash != "" {
		if previous := network.Status.ForwardingSecretHash; previous != "" && previous != read.Hash {
			r.Recorder.Eventf(network, corev1.EventTypeWarning,
				spawneryv1alpha1.EventForwardingSecretRotated,
				"the forwarding secret changed; roll the server groups first, then the proxy groups — see %s",
				rotationRunbook)
		}
		// Only on a successful read: see NetworkStatus.ForwardingSecretHash.
		network.Status.ForwardingSecretHash = read.Hash
	}
	// Both events fire on entering a state, so the condition as it stands
	// before SetStatusCondition below is what says whether this is an entry.
	if read.Reason == spawneryv1alpha1.ReasonSecretNotFound &&
		!hasConditionReason(network.Status.Conditions,
			spawneryv1alpha1.ConditionForwardingSecretResolved, read.Reason) {
		r.Recorder.Eventf(network, corev1.EventTypeWarning,
			spawneryv1alpha1.EventForwardingSecretNotFound, "%s", read.Message)
	}
	meta.SetStatusCondition(&network.Status.Conditions, resolvedCondition(read))

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(network.Namespace), client.MatchingLabels{
		podspec.LabelManagedBy: podspec.ManagedByValue,
		podspec.LabelNetwork:   network.Name,
	}); err != nil {
		return ctrl.Result{}, err
	}
	meta.SetStatusCondition(&network.Status.Conditions,
		rotationCondition(read, forwardingStamps(pods.Items)))
```

Add the helper at the bottom of `internal/controller/forwardingsecret.go`:

```go
// hasConditionReason reports whether the object already carries this condition
// with this reason — which is how the events tell entering a state from staying
// in it. At a five-second requeue the difference is one event against seven
// hundred an hour.
func hasConditionReason(conditions []metav1.Condition, condType, reason string) bool {
	cond := meta.FindStatusCondition(conditions, condType)
	return cond != nil && cond.Reason == reason
}
```

Add `"k8s.io/apimachinery/pkg/api/meta"` to that file's imports, and `corev1 "k8s.io/api/core/v1"` plus `"github.com/spawnery/spawnery/internal/podspec"` to `network_controller.go`'s.

- [ ] **Step 5: Wire the reader**

In `internal/controller/setup.go`, in the `NetworkReconciler` construction:

```go
	if err := (&NetworkReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("network"),
		Clock:    opts.Clock,
		// Uncached, for the reason SecretReader's own comment gives. The
		// Bootstrapper takes the same reader for the same reason.
		SecretReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
```

Nothing is added to `controller.Options` and `cmd/spawnery-operator/main.go` is not touched.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'ForwardingSecret|RotationPending|StalePod|Adoption|AnnouncedExactlyOnce|MissingSecret' -v`
Expected: PASS.

- [ ] **Step 7: Run the whole suite**

Run: `make test`
Expected: PASS. Existing Network controller tests now run against a namespace with no forwarding secret, so they will see the two new conditions; none of them asserts on the full condition list, so none should need changing. If one does, fix the test rather than weakening the controller.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/ config/rbac/role.yaml
git commit -m "The Network controller reads its secret, records it, and says what it found"
```

---

### Task 7: The runbook and the known issues

**Files:**
- Create: `docs/runbook-milestone-5c-secret-rotation.md`
- Modify: `docs/known-issues.md` (append a "From milestone 5c" section, matching the "From milestone 5b" section's shape)

**Interfaces:**
- Consumes: every label, condition, reason and manifest path the earlier tasks produced. Verify each against the code as it now stands rather than against this plan.
- Produces: nothing code depends on.

- [ ] **Step 1: Write the runbook**

Create `docs/runbook-milestone-5c-secret-rotation.md`. Unlike the milestone evidence runbooks beside it, this is a standing operating procedure and stays true after the milestone ships. It must contain, in this order:

1. **What this is for and when it applies** — rotating a Network's Velocity modern forwarding secret. One paragraph on why it is manual: neither Velocity nor Paper accepts two forwarding secrets at once, so there is a window in which joins between an already-rotated and a not-yet-rotated layer fail with "Unable to verify player details". Cite `docs/superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md` §6.5.
2. **Why server groups first, as a reason and not only an order** — a proxy holding the old secret and a backend holding the new reject each other. Rolling the proxies first throws every connected player out in the same second *and* drops them into a network where no backend is reachable. Rolling the servers first leaves the proxies up and players connected while unreachability grows group by group; the one hard cut stays at the end and lasts as long as a proxy restart.
3. **Prerequisite** — the namespace needs `config/rbac/forwarding-secret-reader.yaml`. Give the `kubectl apply -n <ns> -f ...` line, and say that without it the Network reports `ForwardingSecretResolved=Unknown/SecretReadForbidden` and detects nothing.
4. **The progress command**, which is the reason the stamp is a label:

   ```
   kubectl get pods -n <ns> -l spawnery.cloud/network=<net> \
     -L spawnery.cloud/role -L spawnery.cloud/group -L spawnery.cloud/forwarding-hash
   ```

5. **The steps**: write down the current value (`kubectl get secret <name> -n <ns> -o jsonpath='{.data.secret}' | base64 -d`); rotate it; confirm the operator saw it, with the exact commands for the condition and the event; roll each server group; verify no pod of that group carries the old hash; roll each proxy group; confirm `ForwardingSecretRotationPending` reads `False/ForwardingSecretInSync`.
6. **Two warnings, each with its consequence spelled out:**
   - `kubectl delete pod` bypasses the PodDisruptionBudget. The PDB protects an occupied pod against the eviction API; a direct deletion is not an eviction. Deleting an occupied pod disconnects the players on it. Rotation is a maintenance window.
   - 5b's "at most one ordinal of a persistent group down at a time" binds the takedowns the *operator* nominates. A human deleting every pod of a persistent group takes every world offline at once. Pace it: delete one ordinal, wait for it to be `Ready` and to carry the new hash, then the next.
7. **Rollback** — write the old value back and roll whatever has already been rolled, which is why the value is written down before anything changes.
8. **What each condition state means**, as a table: the four `ForwardingSecretRotationPending` states and the five `ForwardingSecretResolved` states, each with what to do about it. `PodsPredateTracking` needs its own sentence: the operator cannot tell, this is normal after an upgrade, and it clears as pods turn over.

- [ ] **Step 2: Verify every command in the runbook**

Run each `kubectl` line in the runbook against the CRD and label names as they exist in the tree — `grep` for each label key in `internal/podspec/labels.go` and each condition and reason in `api/v1alpha1/common_types.go`. A runbook naming a label that does not exist is worse than no runbook.

- [ ] **Step 3: Write the known issues**

Append a "From milestone 5c" section to `docs/known-issues.md`, matching the shape of "From milestone 5b" above it, with these four entries, each checked against the code before it is written:

- **The salted short hash does not defeat a targeted dictionary attack** on a weakly chosen forwarding secret. Anyone with pod read access in the namespace can test guesses offline against `spawnery.cloud/forwarding-hash`. The salt makes precomputation worthless across networks; it does nothing against a guess aimed at one.
- **The stamp says what a pod loaded at start**, not what it would load now. That is the point — but it means a pod that never started, because its secret is missing, carries a stamp describing an intention rather than a fact.
- **Rotation detection is off until an install step is performed per namespace.** Until `config/rbac/forwarding-secret-reader.yaml` is applied, the Network reports `SecretReadForbidden` and names the manifest — the gap announces itself, but it is a gap, and closing it belongs to milestone 6's Helm chart.
- **No per-group condition.** Which group is still stale is in the Network condition's message and nowhere else.

- [ ] **Step 4: Run the whole suite and commit**

Run: `make test`
Expected: PASS.

```bash
git add docs/runbook-milestone-5c-secret-rotation.md docs/known-issues.md
git commit -m "The rotation runbook, and what 5c leaves open"
```

---

## Plan Self-Review

**Spec coverage.** §1 → Global Constraints and Task 3. §1.1 → Task 3, Steps 1 and 4. §1.2 → the constraint that nothing recreates; no task touches `persistent.go`. §2.1 → Task 6, Step 5. §2.2 and §2.2.1 → Task 5 entire. §2.3 → Task 1. §2.4 → Task 2 Step 3 and Task 6 Step 4. §3 → Task 3. §4 boundary cases → Task 4's `no pods at all` case and Task 6's placement after the Accepted branch. §4.1 → Task 4 Step 1's table. §4.2 → Task 4's precedence test. §4.3 → Task 6's `TestAMissingSecretLeavesAcceptedAlone`. §4.4 → Task 6's two event tests. §4.5 → no per-group condition anywhere; recorded in Task 7. §5 → Task 7. §6 → covered by Task 4's read table and Task 6's tests; the identical-value case falls out of the digest and needs no test of its own. §7's nine tests → Tasks 1, 3, 4, 5 and 6, with test 8 (`TestTheAuthorizerActuallyDenies` untouched) enforced by Task 5's `readerProbeNamespace`. §8 → Task 7 Step 3. §9 is the evidence run, after the branch merges, and is deliberately not a task.

**Type consistency.** `ForwardingHash(types.UID, []byte) string` is defined in Task 1 and used in Task 4 only. `LabelForwardingHash` is defined in Task 1 and read in Tasks 3, 4 and 6. `forwardingRead`'s four fields are constructed only in Task 4 and read in Tasks 4 and 6. `rotationRunbook` is defined in Task 4 and used in Tasks 4 and 6; Task 7 creates the file it names. The condition and reason identifiers in Tasks 4 and 6 are exactly the ones Task 2 declares.

**One ordering constraint worth stating.** Tasks 1 → 3 and 2 → 3 → 6 are hard: Task 3 needs both the label and the status field, and Task 6 needs Task 4's functions. Task 5 depends on nothing and may run at any point.
