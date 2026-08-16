# Milestone 5b Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A persistent group can be changed — updated, scaled down, and grown —
one ordinal at a time, and a broken ordinal says so.

**Architecture:** No new controller and no new reconcile loop. Deleting a
`Server` object is already the drain sequence; 5b gives that mechanism two new
occasions (a stale render hash, a pending filesystem resize) and one budget (at
most one ordinal down at a time). Staleness is decided by a digest of what the
operator would render, built as the sibling of the shipped
`podspec.DesiredProxyHash`.

**Tech Stack:** Go, controller-runtime v0.24.1, envtest, kubebuilder CRD
markers with CEL validation.

**Spec:** `docs/superpowers/specs/2026-08-16-persistent-updates-design.md`

## Global Constraints

- **The invariant, verbatim from spec §2:** *At most one ordinal of a persistent
  group is down at a time, whatever the reason.*
- **Gate A (serialisation) applies to every takedown; Gate B (wait for `Ready`)
  applies to stale and resize-pending nominations only, never to surplus
  removal.** Spec §2.1 gives the reason and Task 4 must test it.
- **Candidate priority order:** missing (created all at once, not serialised) →
  surplus → stale → resize-pending.
- **`ServerView.Stale` is already taken** and means the *player count* cannot be
  trusted (`internal/controller/candidates.go:49`). Never overload it. Spec
  staleness is computed in the rule from `ServerView.PodHash`.
- **An empty `Server.spec.podHash` means adopt, not stale** (spec §3.6). Stamp
  the current hash and order no takedown.
- **`podspec` must not import `internal/render`** — a deliberate boundary stated
  at `internal/podspec/server.go:124`. Hash functions take marshalled config
  bytes; the controller package does the marshalling.
- **RBAC on `persistentvolumeclaims` gains `patch` and nothing else.** No
  `delete`, no `update`. `internal/rbacaudit/required.go` is hand-maintained and
  cross-checked in both directions.
- **The CEL rule forbidding `spec.update` on a `Persistent` group stays**
  (`api/v1alpha1/servergroup_types.go:106`).
- **Digest width and encoding follow the shipped sibling:** `encoding/json`
  marshal, `sha256`, `hex.EncodeToString(sum[:8])`.
- Every task ends green on `nix develop -c make test`.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/podspec/hash.go` | `DesiredServerHash` beside `DesiredProxyHash`; both take config bytes | 1, 2 |
| `internal/podspec/hash_test.go` | discrimination tables and determinism for both | 1, 2 |
| `internal/controller/servergroup_controller.go` | `serverConfigValues`, hash stamping, `size()` wiring, resize condition | 1, 3, 5, 9 |
| `internal/controller/proxygroup_controller.go` | pass config bytes to `DesiredProxyHash` | 2 |
| `api/v1alpha1/server_types.go` | `spec.podHash`, `status.storageResizePending` | 3, 8 |
| `api/v1alpha1/common_types.go` | `ConditionStorageResize` | 9 |
| `internal/controller/candidates.go` | `ServerView.PodHash`, `ServerView.ResizePending` | 3, 8 |
| `internal/controller/persistent.go` | the nomination rule and its two gates | 4, 8 |
| `internal/controller/persistent_test.go` | the rule's tables | 4, 8 |
| `internal/controller/backoff.go` | `CountFailures` reset narrowed for persistent groups | 6 |
| `internal/controller/server_controller.go` | claim patch, resize-pending status | 7, 8 |
| `internal/rbacaudit/required.go` | the `patch` row | 7 |
| `docs/known-issues.md`, `docs/handover-milestone-5.md`, `docs/runbook-milestone-5b-evidence.md` | the record | 10 |

---

## Task 1: `DesiredServerHash`

**Files:**
- Modify: `internal/podspec/hash.go`
- Modify: `internal/podspec/hash_test.go`
- Modify: `internal/controller/servergroup_controller.go:1069-1073` (extract `serverConfigValues`)

**Interfaces:**
- Consumes: `BuildServerPod(net, group, srv, agentEndpoint)` (`internal/podspec/server.go:222`).
- Produces: `podspec.DesiredServerHash(net *spawneryv1alpha1.Network, group *spawneryv1alpha1.ServerGroup, configValues []byte) (string, error)` and
  `controller.serverConfigValues(group *spawneryv1alpha1.ServerGroup) ([]byte, error)`.

**Why the signature takes bytes.** `internal/podspec/server.go:124` records that
`podspec` stays free of `internal/render` so that building a pod spec never
depends on a package that touches the filesystem. Passing the already-marshalled
config values keeps that boundary intact, and the single `serverConfigValues`
helper is what stops the ConfigMap and the hash from drifting apart.

- [ ] **Step 1: Write the failing tests**

In `internal/podspec/hash_test.go`:

```go
func serverHashFixtures(t *testing.T) (*spawneryv1alpha1.Network, *spawneryv1alpha1.ServerGroup) {
	t.Helper()
	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "fwd"},
		},
	}
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef: spawneryv1alpha1.ObjectRef{Name: "n"},
			Type:       spawneryv1alpha1.ServerGroupPersistent,
			Image:      "img:1",
			MaxPlayers: 20,
			Replicas:   ptr.To(int32(1)),
			Storage:    &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("1Gi")},
		},
	}
	return net, group
}

// The hash must not flap between passes: a PodSpec carries maps, and Go's map
// iteration order is unspecified. An unstable digest would restart every world
// on every operator restart, which is worse than the problem 5b solves.
func TestDesiredServerHashIsStableAcrossRuns(t *testing.T) {
	net, group := serverHashFixtures(t)
	values := []byte("maxPlayers: 20\n")

	first, err := podspec.DesiredServerHash(net, group, values)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := podspec.DesiredServerHash(net, group, values)
		if err != nil {
			t.Fatalf("DesiredServerHash run %d: %v", i, err)
		}
		if again != first {
			t.Fatalf("run %d gave %q, first run gave %q", i, again, first)
		}
	}
}

// The discrimination table is the whole point of the hash: it says, as a list a
// person can read, which edits restart a world and which do not.
func TestDesiredServerHashDiscriminates(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*spawneryv1alpha1.ServerGroup)
		values  []byte
		changed bool
	}{
		{
			name:    "image changes it",
			mutate:  func(g *spawneryv1alpha1.ServerGroup) { g.Spec.Image = "img:2" },
			changed: true,
		},
		{
			name:    "maxPlayers changes it, through the config values",
			values:  []byte("maxPlayers: 40\n"),
			changed: true,
		},
		{
			name:    "replicas does not change it",
			mutate:  func(g *spawneryv1alpha1.ServerGroup) { g.Spec.Replicas = ptr.To(int32(5)) },
			changed: false,
		},
		{
			name: "drain.timeoutSeconds does not change it",
			mutate: func(g *spawneryv1alpha1.ServerGroup) {
				g.Spec.Drain = &spawneryv1alpha1.DrainSpec{TimeoutSeconds: 999}
			},
			changed: false,
		},
	}

	base := []byte("maxPlayers: 20\n")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net, group := serverHashFixtures(t)
			before, err := podspec.DesiredServerHash(net, group, base)
			if err != nil {
				t.Fatalf("baseline: %v", err)
			}

			if tc.mutate != nil {
				tc.mutate(group)
			}
			values := base
			if tc.values != nil {
				values = tc.values
			}
			after, err := podspec.DesiredServerHash(net, group, values)
			if err != nil {
				t.Fatalf("mutated: %v", err)
			}

			if tc.changed && before == after {
				t.Fatalf("expected the hash to change, both are %q", before)
			}
			if !tc.changed && before != after {
				t.Fatalf("expected the hash to hold, got %q then %q", before, after)
			}
		})
	}
}

// The per-ordinal identity must not reach the digest, or every ordinal would
// read as stale against every other and the group would never settle.
func TestDesiredServerHashIgnoresTheServerIdentity(t *testing.T) {
	net, group := serverHashFixtures(t)
	values := []byte("maxPlayers: 20\n")

	got, err := podspec.DesiredServerHash(net, group, values)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	// Two ordinals of the same group render two pods that differ in name, in
	// SPAWNERY_SERVER and in the claim they mount. None of that may move the
	// digest, so one call per group -- not per server -- is the whole API.
	pod0, err := podspec.BuildServerPod(net, group, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "g-0", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{Ordinal: ptr.To(int32(0))},
	}, "agent:9443")
	if err != nil {
		t.Fatalf("BuildServerPod g-0: %v", err)
	}
	pod1, err := podspec.BuildServerPod(net, group, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "g-1", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{Ordinal: ptr.To(int32(1))},
	}, "agent:9443")
	if err != nil {
		t.Fatalf("BuildServerPod g-1: %v", err)
	}
	if reflect.DeepEqual(pod0.Spec, pod1.Spec) {
		t.Fatal("fixture is not exercising the property: two ordinals rendered identical pods")
	}
	if got == "" {
		t.Fatal("empty hash")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `nix develop -c go test ./internal/podspec/ -run DesiredServerHash -v`
Expected: FAIL, `undefined: podspec.DesiredServerHash`.

- [ ] **Step 3: Implement `DesiredServerHash`**

Append to `internal/podspec/hash.go`:

```go
// DesiredServerHash digests everything this operator would render for one
// server of the group right now: the pod, with the server's name held at a
// fixed empty value so nothing derived from it can reach the digest, and the
// config values the group's ConfigMap carries.
//
// It is the sibling of DesiredProxyHash and follows it deliberately -- same
// empty-name technique, same encoding/json marshal so map keys sort and the
// digest does not flap, same eight-byte digest. Read that function's comment
// for the argument; it applies here unchanged.
//
// Two divergences from the sibling, both deliberate:
//
// The config values arrive as bytes rather than being rendered here, because
// podspec stays free of internal/render (see configSecretFile's comment). They
// are part of the digest because a pod-only hash would miss maxPlayers
// entirely: it never reaches the PodSpec, only the ConfigMap the pod mounts by
// name, so changing it would update the ConfigMap while every running server
// kept the old value and nothing reported the gap.
//
// The agent endpoint is not an input. It comes from an operator flag rather
// than from any spec, and including it would mean that restarting the operator
// with a different --operator-namespace restarts every world in the
// installation. DesiredProxyHash does take it, which is a real asymmetry
// between the two: a proxy rolled for that reason loses no world, so the
// argument that forces the exclusion here does not reach it.
func DesiredServerHash(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ServerGroup,
	configValues []byte,
) (string, error) {
	// The endpoint is a fixed sentinel rather than the real one, and rather
	// than "": BuildServerPod refuses an empty endpoint outright
	// (internal/podspec/server.go:231). A constant contributes a constant to
	// every digest and so discriminates nothing, which is exactly the intent.
	subject, err := BuildServerPod(net, group, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Namespace: group.Namespace},
	}, "spawnery.invalid:0")
	if err != nil {
		return "", err
	}
	// Belt-and-braces, for the reason DesiredProxyHash gives: BuildServerPod
	// never sets LabelPodHash, and this keeps the digest right if that stops
	// being true rather than feeding the label back into itself.
	delete(subject.Labels, LabelPodHash)

	encoded, err := json.Marshal(struct {
		Pod    *corev1.Pod `json:"pod"`
		Config []byte      `json:"config"`
	}{Pod: subject, Config: configValues})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8]), nil
}
```

**Note the sentinel is required, not defensive.** `DesiredProxyHash` passes `""`
for the *name*, which is fine, but the endpoint is a separate argument and
`BuildServerPod` returns an error on an empty one
(`internal/podspec/server.go:231`, verified). An empty-name `Server` is accepted:
that function validates `group.Spec.Image` and `agentEndpoint`, and nothing
about the server it is handed.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/podspec/ -run DesiredServerHash -v`
Expected: PASS, all three.

- [ ] **Step 5: Extract `serverConfigValues` so the ConfigMap and the hash cannot drift**

In `internal/controller/servergroup_controller.go`, replace the inline marshal
inside `reconcileConfigMap` (currently `:1069-1073`):

```go
// serverConfigValues is the config document a server group renders, and the
// single place it is built. reconcileConfigMap writes it to the ConfigMap and
// DesiredServerHash digests it; two constructions of the same document would
// drift, and the failure that drift produces is an update that never fires.
func serverConfigValues(group *spawneryv1alpha1.ServerGroup) ([]byte, error) {
	maxPlayers := group.Spec.MaxPlayers
	data, err := yaml.Marshal(render.Values{MaxPlayers: &maxPlayers})
	if err != nil {
		return nil, fmt.Errorf("marshal config.yaml for group %s: %w", group.Name, err)
	}
	return data, nil
}
```

and have `reconcileConfigMap` call it:

```go
	data, err := serverConfigValues(group)
	if err != nil {
		return err
	}
```

- [ ] **Step 6: Run the full suite**

Run: `nix develop -c make test`
Expected: exit 0, no FAIL.

- [ ] **Step 7: Commit**

```bash
git add internal/podspec/hash.go internal/podspec/hash_test.go internal/controller/servergroup_controller.go
git commit -m "A server group can say what it would render right now"
```

---

## Task 2: The `motd` hole in `DesiredProxyHash`

**Files:**
- Modify: `internal/podspec/hash.go:43-64`
- Modify: `internal/podspec/hash_test.go`
- Modify: `internal/controller/proxygroup_controller.go:628`

**Interfaces:**
- Consumes: `proxyConfigValues(group *spawneryv1alpha1.ProxyGroup) render.Values` (`internal/controller/proxygroup_controller.go:1289`).
- Produces: `DesiredProxyHash(net, group, agentEndpoint string, configValues []byte) (string, error)` — **the signature gains a parameter**, and its one caller changes with it.

**What this fixes.** Spec §3.7. `motd` appears nowhere in
`internal/podspec/proxy.go`; it reaches only the ConfigMap
(`internal/controller/proxygroup_controller.go:1301`). Staleness is
`pods[i].Labels[LabelPodHash] != wantHash` (`:655`), so no proxy ever goes stale
for it, `DecideRollout` orders nothing, and `proto/`/`internal/agentserver/`
carry no reload message. A changed `motd` never reaches a running proxy.
`playerLimit` is not affected — it is in the pod as `SPAWNERY_PLAYER_LIMIT`
(`internal/podspec/proxy.go:223`).

- [ ] **Step 1: Write the failing test**

In `internal/podspec/hash_test.go`:

```go
// motd reaches only the ConfigMap, never the rendered pod, so a pod-only digest
// left it invisible: the ConfigMap updated, no proxy went stale, DecideRollout
// ordered nothing, and there is no reload path. Shipped since 4c-2.
//
// playerLimit is here beside it precisely because it is *not* broken -- it
// rides in the pod as SPAWNERY_PLAYER_LIMIT -- so that a later refactor moving
// that env var out of the pod cannot break it in silence.
func TestDesiredProxyHashSeesTheConfigValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*spawneryv1alpha1.ProxyGroup)
	}{
		{
			name: "motd",
			mutate: func(g *spawneryv1alpha1.ProxyGroup) {
				g.Spec.Config.Motd = "a different motd"
			},
		},
		{
			name: "playerLimit",
			mutate: func(g *spawneryv1alpha1.ProxyGroup) {
				g.Spec.Config.PlayerLimit = 999
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			net, group := proxyHashFixtures(t)
			group.Spec.Config = &spawneryv1alpha1.ProxyConfig{PlayerLimit: 20, Motd: "before"}

			before, err := podspec.DesiredProxyHash(net, group, "agent:9443", proxyValuesBytes(t, group))
			if err != nil {
				t.Fatalf("baseline: %v", err)
			}

			tc.mutate(group)
			after, err := podspec.DesiredProxyHash(net, group, "agent:9443", proxyValuesBytes(t, group))
			if err != nil {
				t.Fatalf("mutated: %v", err)
			}

			if before == after {
				t.Fatalf("changing %s left the hash at %q, so no proxy would ever go stale for it", tc.name, before)
			}
		})
	}
}
```

Add `proxyHashFixtures` beside `serverHashFixtures` if the existing test file
has no equivalent, and a `proxyValuesBytes` helper that marshals the same
document the ProxyGroup controller writes:

```go
func proxyValuesBytes(t *testing.T, group *spawneryv1alpha1.ProxyGroup) []byte {
	t.Helper()
	// The production path marshals controller.proxyConfigValues(group); this
	// mirrors only the two fields under test, which is enough to prove the
	// bytes reach the digest.
	limit := group.Spec.Config.PlayerLimit
	motd := group.Spec.Config.Motd
	data, err := yaml.Marshal(render.Values{PlayerLimit: &limit, Motd: &motd})
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	return data
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `nix develop -c go test ./internal/podspec/ -run TestDesiredProxyHashSeesTheConfigValues -v`
Expected: the `motd` subtest FAILS with "changing motd left the hash at ...";
the `playerLimit` subtest PASSES. That split is the defect stated exactly.

Note this step will not even compile until Step 3 adds the parameter. Add the
parameter first if the compiler blocks the demonstration, and confirm the
failure by running the test with the new signature and the *old* body (digesting
`subject` only) before changing the body.

- [ ] **Step 3: Widen the digest**

In `internal/podspec/hash.go`, change the signature and the marshal:

```go
func DesiredProxyHash(
	net *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	agentEndpoint string,
	configValues []byte,
) (string, error) {
	subject, err := renderProxyPod(net, group, "", agentEndpoint)
	if err != nil {
		return "", err
	}
	delete(subject.Labels, LabelPodHash)

	encoded, err := json.Marshal(struct {
		Pod    *corev1.Pod `json:"pod"`
		Config []byte      `json:"config"`
	}{Pod: subject, Config: configValues})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8]), nil
}
```

and extend its doc comment with:

```go
// The config values are part of the digest because the rendered pod is not
// everything this operator writes for a proxy. playerLimit rides in the pod as
// SPAWNERY_PLAYER_LIMIT, but motd reaches only the ConfigMap -- so until this
// milestone a changed motd made no proxy stale, ordered no rollout, and never
// reached a running proxy at all. Widening the digest changes its value for
// every existing proxy, so the first reconcile after this ships rolls every
// proxy group once, through the ordinary surge-1 path.
```

- [ ] **Step 4: Update the one caller**

At `internal/controller/proxygroup_controller.go:628`:

```go
	configValues, err := yaml.Marshal(proxyConfigValues(group))
	if err != nil {
		return err
	}
	wantHash, err := podspec.DesiredProxyHash(network, group, r.AgentEndpoint, configValues)
```

Match the surrounding error-return shape; the enclosing function's signature
decides whether this is `return err` or `return ctrl.Result{}, err`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `nix develop -c go test ./internal/podspec/ ./internal/controller/ -run 'Hash|Rollout|Proxy' -v`
Expected: PASS.

- [ ] **Step 6: Run the full suite**

Run: `nix develop -c make test`
Expected: exit 0. Existing rollout tests that pin a literal hash value will need
their expected values updated — that is the widening, not a regression.

- [ ] **Step 7: Commit**

```bash
git add internal/podspec/hash.go internal/podspec/hash_test.go internal/controller/proxygroup_controller.go
git commit -m "A changed motd now reaches the proxy it was written for"
```

---

## Task 3: `Server.spec.podHash`, stamped and adopted

**Files:**
- Modify: `api/v1alpha1/server_types.go` (after `GroupGeneration`, `:36`)
- Modify: `internal/controller/candidates.go` (`ServerView`)
- Modify: `internal/controller/servergroup_controller.go` (`newServer:840`, `createPersistentServer:882`, `collectViews:729`)
- Modify: `config/crd/bases/spawnery.cloud_servers.yaml` (regenerated)
- Modify: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `podspec.DesiredServerHash`, `serverConfigValues` (Task 1).
- Produces: `Server.Spec.PodHash string`; `ServerView.PodHash string`;
  `newServer(group, name, podHash string)` — **the signature gains a parameter**,
  and both callers (`createServer`, `createPersistentServer`) change with it.

- [ ] **Step 1: Add the API field**

In `api/v1alpha1/server_types.go`, after `GroupGeneration`:

```go
	// PodHash is podspec.DesiredServerHash at the moment this server was
	// created: a digest of everything the operator would render for it. The
	// group compares it against a freshly computed one to decide whether this
	// ordinal is running the current spec.
	//
	// Empty means adopt, never stale. Every server that existed before this
	// field did carries an empty value, and reading that as stale would restart
	// every world in the installation on the first reconcile after an upgrade.
	// The group stamps the current hash onto such a server and orders no
	// takedown.
	// +optional
	PodHash string `json:"podHash,omitempty"`
```

- [ ] **Step 2: Add the view field**

In `internal/controller/candidates.go`, inside `ServerView` after `Generation`:

```go
	// PodHash is spec.podHash: the render digest this server was created under.
	// Empty means the server predates the field and is to be adopted rather
	// than replaced.
	//
	// Deliberately not called Stale, and deliberately not a bool. Stale is
	// already taken on this type and means the player count cannot be trusted;
	// spec staleness is a comparison the sizing rule makes, not a second flag
	// somebody could confuse with the first.
	PodHash string
```

- [ ] **Step 3: Write the failing test**

In `internal/controller/servergroup_controller_test.go`:

```go
// A persistent server is created carrying the hash of what the operator would
// render for it, so that the next pass can tell whether the spec has moved.
func TestAPersistentServerIsCreatedCarryingItsRenderHash(t *testing.T) {
	// envtest fixture setup follows the file's existing persistent-group
	// helpers; create a Persistent group at replicas: 1 and reconcile once.
	// Then:
	srv := &spawneryv1alpha1.Server{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "g-0", Namespace: ns}, srv); err != nil {
		t.Fatalf("get g-0: %v", err)
	}
	if srv.Spec.PodHash == "" {
		t.Fatal("server was created with no pod hash, so it can never be found stale")
	}

	values, err := serverConfigValues(group)
	if err != nil {
		t.Fatalf("serverConfigValues: %v", err)
	}
	want, err := podspec.DesiredServerHash(network, group, values)
	if err != nil {
		t.Fatalf("DesiredServerHash: %v", err)
	}
	if srv.Spec.PodHash != want {
		t.Fatalf("stamped %q, the group would render %q", srv.Spec.PodHash, want)
	}
}

// The upgrade case, and it must assert both halves. Asserting only that the
// field gets filled would pass while every world restarted.
func TestAServerWithNoHashIsAdoptedRatherThanReplaced(t *testing.T) {
	// Create the Persistent group, reconcile until g-0 is Ready, then blank the
	// field to simulate a server that predates it:
	srv := &spawneryv1alpha1.Server{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "g-0", Namespace: ns}, srv); err != nil {
		t.Fatalf("get g-0: %v", err)
	}
	uidBefore := srv.UID
	srv.Spec.PodHash = ""
	if err := k8sClient.Update(ctx, srv); err != nil {
		t.Fatalf("blank the hash: %v", err)
	}

	reconcileGroupOnce(t)

	after := &spawneryv1alpha1.Server{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "g-0", Namespace: ns}, after); err != nil {
		t.Fatalf("get g-0 after: %v", err)
	}
	if after.UID != uidBefore {
		t.Fatal("the ordinal was replaced; an empty hash must be adopted, not treated as stale")
	}
	if after.Spec.PodHash == "" {
		t.Fatal("the hash was not stamped, so the server stays unadoptable forever")
	}
	if !after.DeletionTimestamp.IsZero() {
		t.Fatal("a takedown was ordered for an adopted server")
	}
}
```

Reuse whatever the file already names for `ns`, `group`, `network`,
`reconcileGroupOnce` and its persistent-group helpers rather than introducing
new ones.

- [ ] **Step 4: Run to verify it fails**

Run: `nix develop -c go test ./internal/controller/ -run 'RenderHash|AdoptedRatherThanReplaced' -v`
Expected: FAIL — the field is never written.

- [ ] **Step 5: Stamp on create, carry on the view, adopt when empty**

`newServer` gains the parameter and sets the field:

```go
func (r *ServerGroupReconciler) newServer(
	group *spawneryv1alpha1.ServerGroup,
	name string,
	podHash string,
) (*spawneryv1alpha1.Server, error) {
```

with `PodHash: podHash` beside `GroupGeneration: group.Generation` in the
`ServerSpec` literal. Both callers compute it once per reconcile and pass it
down; computing it per server would render the same pod N times for one answer.

In `collectViews`, add `PodHash: srv.Spec.PodHash` to the `ServerView` literal.

Adoption happens in the group's reconcile, before sizing: for a persistent
group, any server whose `spec.podHash` is empty is patched to the freshly
computed hash. It is a `spec` write on a `Server` the group owns, in the same
place the group already writes `spec.retire`.

- [ ] **Step 6: Run to verify it passes**

Run: `nix develop -c go test ./internal/controller/ -run 'RenderHash|AdoptedRatherThanReplaced' -v`
Expected: PASS.

- [ ] **Step 7: Regenerate the CRDs and run the suite**

Run: `nix develop -c make manifests generate && nix develop -c make test`
Expected: `config/crd/bases/spawnery.cloud_servers.yaml` gains `podHash`; exit 0.

- [ ] **Step 8: Commit**

```bash
git add api/v1alpha1/server_types.go internal/controller/ config/crd/bases/
git commit -m "A persistent server remembers what it was rendered from"
```

---

## Task 4: The nomination rule and its two gates

**Files:**
- Modify: `internal/controller/persistent.go:63-140`
- Modify: `internal/controller/persistent_test.go`

**Interfaces:**
- Consumes: `ServerView.PodHash` (Task 3), `ServerView.leaving()`
  (`internal/controller/candidates.go:236`), `SizeDecision`
  (`internal/controller/scaling.go:73`).
- Produces: `PersistentInputs` gains `PodHash string`; `DecidePersistentSize`
  fills at most one name into `decision.Delete`.

**The rule, verbatim from spec §2 and §2.1.** Priority: missing (all at once) →
surplus (Gate A) → stale (Gates A and B). Gate A: no view is `leaving()` and no
`PendingDeletes` entry is outstanding. Gate B: every ordinal below `Replicas`
has a view in phase `Ready`.

- [ ] **Step 1: Write the failing table**

In `internal/controller/persistent_test.go`:

```go
func TestDecidePersistentSizeTakesOneOrdinalDownAtATime(t *testing.T) {
	ready := func(ordinal int32, hash string) ServerView {
		return ServerView{
			Name:    PersistentServerName("g", ordinal),
			Ordinal: ptr.To(ordinal),
			Phase:   phase.Ready,
			PodHash: hash,
		}
	}
	draining := func(ordinal int32, hash string) ServerView {
		v := ready(ordinal, hash)
		v.Phase = phase.Draining
		return v
	}

	cases := []struct {
		name       string
		replicas   int32
		podHash    string
		views      []ServerView
		wantCreate []int32
		wantDelete []string
	}{
		{
			name:       "missing ordinals are created all at once, not serialised",
			replicas:   4,
			podHash:    "h1",
			views:      []ServerView{ready(0, "h1")},
			wantCreate: []int32{1, 2, 3},
		},
		{
			name:     "surplus takes the highest, one only",
			replicas: 1,
			podHash:  "h1",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"), ready(3, "h1"),
			},
			wantDelete: []string{"g-3"},
		},
		{
			name:     "Gate A holds the next surplus while one is draining",
			replicas: 1,
			podHash:  "h1",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"), draining(3, "h1"),
			},
			wantDelete: nil,
		},
		{
			// Spec 2.1: a surplus ordinal sits above replicas, so Gate B cannot
			// see it. Gate A is what holds the invariant here, and this case is
			// what proves it does.
			name:     "Gate B does not apply to surplus: a sick ordinal 0 does not block a scale-down",
			replicas: 1,
			podHash:  "h1",
			views: []ServerView{
				{Name: "g-0", Ordinal: ptr.To(int32(0)), Phase: phase.Failed, PodHash: "h1"},
				ready(1, "h1"),
			},
			wantDelete: []string{"g-1"},
		},
		{
			name:     "stale takes the highest once no surplus remains",
			replicas: 3,
			podHash:  "h2",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"),
			},
			wantDelete: []string{"g-2"},
		},
		{
			name:     "surplus outranks stale",
			replicas: 2,
			podHash:  "h2",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"), ready(2, "h1"),
			},
			wantDelete: []string{"g-2"},
		},
		{
			// Gate B: the replacement for g-2 is back but not Ready yet, so g-1
			// waits. Deleting the previous object is not the same as the world
			// being back.
			name:     "Gate B holds the next stale while the replacement is still starting",
			replicas: 3,
			podHash:  "h2",
			views: []ServerView{
				ready(0, "h1"), ready(1, "h1"),
				{Name: "g-2", Ordinal: ptr.To(int32(2)), Phase: phase.Starting, PodHash: "h2"},
			},
			wantDelete: nil,
		},
		{
			name:     "an empty hash is adopted, never nominated",
			replicas: 2,
			podHash:  "h2",
			views:    []ServerView{ready(0, ""), ready(1, "")},
			wantDelete: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecidePersistentSize(PersistentInputs{
				Group:    "g",
				Replicas: tc.replicas,
				PodHash:  tc.podHash,
				Views:    tc.views,
			})
			if !reflect.DeepEqual(got.CreateOrdinals, tc.wantCreate) {
				t.Errorf("CreateOrdinals = %v, want %v", got.CreateOrdinals, tc.wantCreate)
			}
			if !reflect.DeepEqual(got.Delete, tc.wantDelete) {
				t.Errorf("Delete = %v, want %v", got.Delete, tc.wantDelete)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `nix develop -c go test ./internal/controller/ -run TakesOneOrdinalDownAtATime -v`
Expected: FAIL — `PodHash` is not a field of `PersistentInputs`, and the surplus
cases return every surplus name rather than one.

- [ ] **Step 3: Implement the gates**

In `internal/controller/persistent.go`, add to `PersistentInputs`:

```go
	// PodHash is podspec.DesiredServerHash for the group as it stands now. A
	// view whose PodHash differs is stale; a view whose PodHash is empty is
	// adopted rather than replaced, so an upgrade that introduces the field
	// does not restart every world in the installation.
	PodHash string
```

and replace the surplus loop with the two gates and the four classes. Keep the
existing create loop untouched. The two gates as their own functions, because
each is a sentence worth naming:

```go
// takedownInFlight is Gate A: is a takedown of this group already under way.
// It is what makes the highest-first order observable at all -- without it
// every nomination in a pass fires together and the ordering decides nothing.
func takedownInFlight(in PersistentInputs) bool {
	for _, v := range in.Views {
		if v.leaving() || in.PendingDeletes[v.Name] {
			return true
		}
	}
	return false
}

// groupRecovered is Gate B: does every ordinal the group is supposed to have
// currently have a Ready server.
//
// It deliberately does not gate surplus removal. A surplus ordinal sits above
// Replicas and is invisible to this test, so relying on it there would release
// the next nomination while the previous one was still draining -- Gate A is
// what holds the invariant. Beyond the mechanics: scaling down is an
// instruction an operator gave explicitly, often because something is wrong,
// and withholding it until an unrelated ordinal recovers withholds the remedy.
func groupRecovered(in PersistentInputs) bool {
	readyOrdinals := make(map[int32]bool, len(in.Views))
	for _, v := range in.Views {
		if v.Ordinal != nil && v.Phase == phase.Ready {
			readyOrdinals[*v.Ordinal] = true
		}
	}
	for ordinal := int32(0); ordinal < in.Replicas; ordinal++ {
		if !readyOrdinals[ordinal] {
			return false
		}
	}
	return true
}
```

The nomination, replacing the surplus loop's tail:

```go
	if takedownInFlight(in) {
		return decision
	}

	if len(surplus) > 0 {
		// surplus is already sorted highest-first.
		decision.Delete = append(decision.Delete, held[surplus[0]])
		return decision
	}

	if !groupRecovered(in) {
		return decision
	}

	stale := make([]int32, 0, len(held))
	for ordinal, name := range held {
		if ordinal >= in.Replicas {
			continue
		}
		v := viewByName(in.Views, name)
		// An empty hash is adopted, not replaced: see PersistentInputs.PodHash.
		if v.PodHash == "" || v.PodHash == in.PodHash {
			continue
		}
		stale = append(stale, ordinal)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i] > stale[j] })
	if len(stale) > 0 {
		decision.Delete = append(decision.Delete, held[stale[0]])
	}
	return decision
```

Keep the existing `PendingDeletes` and `leaving()` checks that guarded the old
loop — Gate A subsumes them, so removing them is correct, but say so in a
comment rather than leaving a reader to wonder where they went.

- [ ] **Step 4: Run to verify it passes**

Run: `nix develop -c go test ./internal/controller/ -run TakesOneOrdinalDownAtATime -v`
Expected: PASS, all eight subtests.

- [ ] **Step 5: Mutation-test the three properties**

Apply each mutation, confirm a *named* test fails, revert:

1. Delete the `takedownInFlight` early return → "Gate A holds the next surplus
   while one is draining" must fail.
2. Delete the `groupRecovered` early return → "Gate B holds the next stale while
   the replacement is still starting" must fail, and **"Gate B does not apply to
   surplus" must still pass**.
3. Reverse the `stale` sort to lowest-first → "stale takes the highest once no
   surplus remains" must fail.
4. Apply `groupRecovered` to the surplus branch as well → "Gate B does not apply
   to surplus" must fail.

If any mutation leaves the suite green, the test does not execute the mutated
line or does not assert the property it corrupts. Fix the test, not the count.

- [ ] **Step 6: Run the full suite and commit**

```bash
nix develop -c make test
git add internal/controller/persistent.go internal/controller/persistent_test.go
git commit -m "One ordinal comes down at a time, and two gates say which"
```

---

## Task 5: Wire the rule into `size()`

**Files:**
- Modify: `internal/controller/servergroup_controller.go:559-609`
- Modify: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Consumes: `DecidePersistentSize` with `PodHash` (Task 4), `serverConfigValues`
  and `podspec.DesiredServerHash` (Task 1).
- Produces: no new exported name.

- [ ] **Step 1: Write the failing envtest**

```go
// The invariant, at the only layer that can show it: two ordinals, a spec
// change, and never two down at once across the whole sequence.
func TestAPersistentGroupUpdatesOneOrdinalAtATime(t *testing.T) {
	// Create a Persistent group at replicas: 2, drive both ordinals to Ready.
	// Then change the image and reconcile repeatedly, driving pods to Ready as
	// they appear, recording after every pass how many ordinals are not Ready.
	worst := 0
	for pass := 0; pass < 40; pass++ {
		reconcileGroupOnce(t)
		driveNewPodsReady(t)

		down := 0
		for _, ordinal := range []int32{0, 1} {
			srv := &spawneryv1alpha1.Server{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name: PersistentServerName("g", ordinal), Namespace: ns,
			}, srv)
			if apierrors.IsNotFound(err) || (err == nil && srv.Status.Phase != string(phase.Ready)) {
				down++
			}
		}
		if down > worst {
			worst = down
		}
	}
	if worst > 1 {
		t.Fatalf("%d ordinals were down at once; the invariant allows one", worst)
	}
	if worst == 0 {
		t.Fatal("no ordinal ever went down, so this test proves nothing about the update")
	}
}
```

**The `worst == 0` assertion is not decoration.** Without it the test passes when
the update never fires at all — which is exactly the failure mode a hash bug
produces, and exactly the shape of non-discriminating assertion 5a's review
found seven of.

- [ ] **Step 2: Run to verify it fails**

Run: `nix develop -c go test ./internal/controller/ -run UpdatesOneOrdinalAtATime -v`
Expected: FAIL on `worst == 0` — nothing computes a hash in `size()` yet.

- [ ] **Step 3: Compute the hash once per reconcile and pass it in**

In `size()`'s `default:` branch (`:601-608`):

```go
	default:
		values, err := serverConfigValues(group)
		if err != nil {
			return SizeDecision{}, err
		}
		podHash, err := podspec.DesiredServerHash(network, group, values)
		if err != nil {
			return SizeDecision{}, err
		}
		decision = DecidePersistentSize(PersistentInputs{
			Group:          group.Name,
			Replicas:       group.DesiredReplicas(),
			PodHash:        podHash,
			Views:          views,
			PendingCreates: pendingCreates,
			PendingDeletes: pendingDeletes,
		})
```

`size()` does not currently receive the `Network`. Thread it through from the
caller rather than fetching it a second time inside — the reconcile already has
it, and a second `Get` would be a second source of truth for the same object.

The delete this decision produces already flows through the existing
`deleteServer` loop; give it a reason string that distinguishes the two
occasions, so the event trail says which one it was:
`"SurplusOrdinal"` and `"StaleSpec"`.

- [ ] **Step 4: Run to verify it passes**

Run: `nix develop -c go test ./internal/controller/ -run UpdatesOneOrdinalAtATime -v`
Expected: PASS with `worst == 1`.

- [ ] **Step 5: Run the full suite and commit**

```bash
nix develop -c make test
git add internal/controller/
git commit -m "Changing a persistent group's image now moves it"
```

---

## Task 6: The failure streak reaches `Degraded`

**Files:**
- Modify: `internal/controller/backoff.go:50-77`
- Modify: `internal/controller/backoff_test.go`
- Modify: `internal/controller/servergroup_controller.go` (the `CountFailures` call site)

**Interfaces:**
- Produces: `CountFailures(views []ServerView, prev int32, since time.Time, requiredOrdinals int32) (int32, time.Time)` — **the signature gains a parameter**. Pass `0` for an ephemeral group, which keeps the existing maximum rule exactly.

**What changes, from spec §5.2.** For a persistent group `lastSuccess` becomes
the **minimum** `ReadySince` over the required ordinals rather than the maximum
over all views, so the streak resets only when the group has recovered rather
than when some sibling is fine.

- [ ] **Step 1: Write the failing test**

```go
// A broken ordinal must reach the give-up threshold however often a healthy
// sibling flaps. The old rule took the maximum ReadySince across all views, so
// a neighbour that regained readiness faster than failures arrived reset the
// count more often than it incremented and six was never reached.
//
// One ordinal cannot show this, which is why the existing
// TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp could not.
func TestAFlappingSiblingDoesNotClearABrokenOrdinalsStreak(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	count := int32(0)
	since := time.Time{}
	for i := 0; i < 6; i++ {
		failedAt := base.Add(time.Duration(i) * time.Hour)
		// g-1 blips ready halfway through every interval -- more often than
		// g-0 fails, which is the rate comparison that broke the old rule.
		siblingReady := failedAt.Add(30 * time.Minute)

		views := []ServerView{
			{Name: "g-0", Ordinal: ptr.To(int32(0)), Phase: phase.Failed, FailedAt: failedAt},
			{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Ready, ReadySince: siblingReady},
		}
		count, since = CountFailures(views, count, since, 2)
	}

	if count < 6 {
		t.Fatalf("counted %d failures; a flapping sibling is still clearing the streak", count)
	}
}

// The ephemeral rule must not move: interchangeable servers are exactly the
// case the maximum is right for.
func TestAnEphemeralGroupKeepsTheMaximumRule(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	views := []ServerView{
		{Name: "a", Phase: phase.Failed, FailedAt: base},
		{Name: "b", Phase: phase.Ready, ReadySince: base.Add(time.Minute)},
	}
	count, _ := CountFailures(views, 3, time.Time{}, 0)
	if count != 0 {
		t.Fatalf("count = %d; a ready sibling must still break an ephemeral streak", count)
	}
}

// The group is not recovered while an ordinal is missing entirely, so the
// streak must not reset then either.
func TestAMissingOrdinalDoesNotCountAsRecovered(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	views := []ServerView{
		{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Ready, ReadySince: base.Add(time.Hour)},
	}
	count, _ := CountFailures(views, 4, base, 2)
	if count != 4 {
		t.Fatalf("count = %d; ordinal 0 has no ready server, so the group has not recovered", count)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `nix develop -c go test ./internal/controller/ -run 'FlappingSibling|MaximumRule|MissingOrdinal' -v`
Expected: FAIL — the signature has four parameters only after Step 3.

- [ ] **Step 3: Narrow the reset**

Replace the `lastSuccess` computation:

```go
	// requiredOrdinals is spec.replicas for a persistent group and zero for an
	// ephemeral one, which is what selects between the two rules below.
	var lastSuccess time.Time
	if requiredOrdinals == 0 {
		// Ephemeral: any success breaks the streak. Interchangeable servers are
		// the case this is right for, and it is unchanged from before 5b.
		for _, v := range views {
			if v.ReadySince.After(lastSuccess) {
				lastSuccess = v.ReadySince
			}
		}
	} else {
		// Persistent: the streak breaks when the *group* recovered, not when
		// some sibling is fine. Each ordinal owns a world, so a healthy g-1
		// says nothing about a broken g-0 -- and a g-1 that regains readiness
		// more often than g-0 fails would otherwise reset the count faster
		// than it climbs, and Degraded would never arrive.
		//
		// The minimum over the required ordinals is that rule: an ordinal with
		// no ready server pulls it to zero, whether it is broken, being
		// rebuilt, or not created yet.
		ready := make(map[int32]time.Time, len(views))
		for _, v := range views {
			if v.Ordinal == nil || v.Phase != phase.Ready {
				continue
			}
			if cur, ok := ready[*v.Ordinal]; !ok || v.ReadySince.After(cur) {
				ready[*v.Ordinal] = v.ReadySince
			}
		}
		lastSuccess = time.Time{}
		for ordinal := int32(0); ordinal < requiredOrdinals; ordinal++ {
			at, ok := ready[ordinal]
			if !ok {
				lastSuccess = time.Time{}
				break
			}
			if lastSuccess.IsZero() || at.Before(lastSuccess) {
				lastSuccess = at
			}
		}
	}
```

Update the call site to pass `group.DesiredReplicas()` for a persistent group
and `0` for an ephemeral one.

- [ ] **Step 4: Run to verify they pass**

Run: `nix develop -c go test ./internal/controller/ -run 'FlappingSibling|MaximumRule|MissingOrdinal' -v`
Expected: PASS.

- [ ] **Step 5: Give the existing envtest a second ordinal**

`TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp` runs one ordinal and
cannot show the defect. Add a second, healthy ordinal to it and assert
`Degraded` still arrives. Spec §5.3: this is what makes the rule falsifiable,
not an addition afterwards.

- [ ] **Step 6: Mutate to confirm**

Reverse minimum to maximum in the persistent branch;
`TestAFlappingSiblingDoesNotClearABrokenOrdinalsStreak` must fail. If it does
not, the fixture's sibling is not flapping faster than the failures arrive.

- [ ] **Step 7: Run the full suite and commit**

```bash
nix develop -c make test
git add internal/controller/backoff.go internal/controller/backoff_test.go internal/controller/servergroup_controller.go
git commit -m "A broken ordinal reaches Degraded whatever its neighbours do"
```

---

## Task 7: Grow the claim

**Files:**
- Modify: `internal/controller/server_controller.go:287-299` (and its comment, which currently says growing a world is 5b's)
- Modify: `internal/controller/server_controller.go:107` (the RBAC marker)
- Modify: `internal/rbacaudit/required.go:75-78`
- Modify: `internal/controller/server_controller_test.go`
- Modify: `config/rbac/role.yaml` (regenerated)

**Interfaces:**
- Produces: no new exported name. The `Server` controller patches
  `spec.resources.requests.storage` upward on the claim it already fetches.

- [ ] **Step 1: Write the failing tests**

```go
// Growing spec.storage.size reaches the claim. This is the one write 5b adds
// to an object 5a declared created-never-written, and it is deliberately the
// narrowest possible: one field, upward only.
func TestGrowingStorageSizePatchesTheClaim(t *testing.T) {
	// Persistent group at 1Gi, server reconciled so the claim exists. Then:
	group.Spec.Storage.Size = resource.MustParse("2Gi")
	if err := k8sClient.Update(ctx, group); err != nil {
		t.Fatalf("grow the group: %v", err)
	}
	reconcileServerOnce(t, "g-0")

	claim := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "g-0-data", Namespace: ns}, claim); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	got := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("claim requests %s, group asks for 2Gi", got.String())
	}
}

// A claim someone grew by hand is not the operator's to shrink, and the API
// would refuse anyway. Asserting the claim is untouched is the point: a
// controller that "corrected" it would be reaching past its authority.
func TestAClaimLargerThanTheSpecIsLeftAlone(t *testing.T) {
	// Claim at 5Gi, group still at 1Gi.
	reconcileServerOnce(t, "g-0")

	claim := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "g-0-data", Namespace: ns}, claim); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	got := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Fatalf("claim was changed to %s; a hand-grown claim is not a divergence to heal", got.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `nix develop -c go test ./internal/controller/ -run 'PatchesTheClaim|LargerThanTheSpec' -v`
Expected: the first FAILS (claim stays at 1Gi); the second PASSES already, and
that is fine — it is a regression guard for what Step 3 must not break.

- [ ] **Step 3: Patch upward, and only upward**

Replace the claim block:

```go
		if !group.IsEphemeral() {
			claim := podspec.BuildDataClaim(group, srv)
			if err := r.Create(ctx, claim); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return ctrl.Result{}, err
				}
				// The claim is already there, which is the ordinary case for a
				// recreated ordinal. 5a left it exactly as it was; 5b grows it,
				// and nothing else about it. One field, upward only: the CRD
				// refuses a shrinking spec and the API refuses a shrinking
				// claim, and a claim somebody grew by hand is not this
				// controller's to correct.
				if err := r.growClaim(ctx, group, srv); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
```

with:

```go
// growClaim raises the claim's storage request to match spec.storage.size, and
// never lowers it. It is the only write this operator makes to a claim, and the
// RBAC it needs is patch -- not update, which would replace the whole object
// for one field, and never delete, which is the verb that destroys a world.
func (r *ServerReconciler) growClaim(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	srv *spawneryv1alpha1.Server,
) error {
	if group.Spec.Storage == nil {
		return nil
	}
	claim := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Name: podspec.DataClaimName(srv.Name), Namespace: srv.Namespace}
	if err := r.Get(ctx, key, claim); err != nil {
		return client.IgnoreNotFound(err)
	}
	want := group.Spec.Storage.Size
	have := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if want.Cmp(have) <= 0 {
		return nil
	}
	patch := client.MergeFrom(claim.DeepCopy())
	claim.Spec.Resources.Requests[corev1.ResourceStorage] = want
	return r.Patch(ctx, claim, patch)
}
```

- [ ] **Step 4: Widen the RBAC in both places**

At `internal/controller/server_controller.go:107`:

```go
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;patch
```

and add one row to `internal/rbacaudit/required.go` beside the existing four:

```go
	{Group: "", Resource: "persistentvolumeclaims", Verb: "patch", Why: "ServerReconciler grows a world's claim when spec.storage.size grows; never update, never delete"},
```

- [ ] **Step 5: Run to verify, regenerate, run the suite**

```bash
nix develop -c go test ./internal/controller/ -run 'PatchesTheClaim|LargerThanTheSpec' -v
nix develop -c make manifests generate
nix develop -c make test
```
Expected: PASS; `config/rbac/role.yaml` gains `patch` and only `patch`; the
rbacaudit tests agree in both directions.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/ internal/rbacaudit/required.go config/rbac/
git commit -m "A world can grow, by one field and upward only"
```

---

## Task 8: Restart only when the driver asks

**Files:**
- Modify: `api/v1alpha1/server_types.go` (status)
- Modify: `internal/controller/server_controller.go`
- Modify: `internal/controller/candidates.go` (`ServerView`)
- Modify: `internal/controller/persistent.go` (fourth candidate class)
- Modify: `internal/controller/persistent_test.go`, `internal/controller/server_controller_test.go`

**Interfaces:**
- Produces: `Server.Status.StorageResizePending bool`; `ServerView.ResizePending bool`; the fourth class in `DecidePersistentSize`.

- [ ] **Step 1: Add the status field**

```go
	// StorageResizePending is true while this server's claim carries the
	// FileSystemResizePending condition: the CSI driver has grown the volume
	// and needs the pod restarted before the filesystem follows. Most drivers
	// expand online and never set it.
	// +optional
	StorageResizePending bool `json:"storageResizePending,omitempty"`
```

- [ ] **Step 2: Write the failing tests**

The unit case, in `internal/controller/persistent_test.go`, added to Task 4's
table:

```go
		{
			name:     "resize-pending is nominated, but only after stale",
			replicas: 2,
			podHash:  "h1",
			views: []ServerView{
				{Name: "g-0", Ordinal: ptr.To(int32(0)), Phase: phase.Ready, PodHash: "h1", ResizePending: true},
				{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Ready, PodHash: "h1", ResizePending: true},
			},
			wantDelete: []string{"g-1"},
		},
		{
			name:     "a stale ordinal outranks a resize-pending one",
			replicas: 2,
			podHash:  "h2",
			views: []ServerView{
				{Name: "g-0", Ordinal: ptr.To(int32(0)), Phase: phase.Ready, PodHash: "h1"},
				{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Ready, PodHash: "h2", ResizePending: true},
			},
			wantDelete: []string{"g-0"},
		},
```

The envtest case, in `internal/controller/server_controller_test.go`:

```go
// envtest runs no external resizer, so the condition is written by hand. That
// is the honest way to test this: the operator's job is to react to the
// condition, and nothing in envtest can produce one.
func TestAResizePendingClaimMarksItsServer(t *testing.T) {
	claim := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "g-0-data", Namespace: ns}, claim); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	claim.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{
		Type:   corev1.PersistentVolumeClaimFileSystemResizePending,
		Status: corev1.ConditionTrue,
	}}
	if err := k8sClient.Status().Update(ctx, claim); err != nil {
		t.Fatalf("set the condition: %v", err)
	}

	reconcileServerOnce(t, "g-0")

	srv := &spawneryv1alpha1.Server{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "g-0", Namespace: ns}, srv); err != nil {
		t.Fatalf("get g-0: %v", err)
	}
	if !srv.Status.StorageResizePending {
		t.Fatal("the claim asked for a restart and the server does not say so")
	}
}
```

- [ ] **Step 3: Run to verify they fail, then implement**

Run: `nix develop -c go test ./internal/controller/ -run 'ResizePending|resize' -v`
Expected: FAIL.

In the `Server` controller, after `growClaim`, read the claim's conditions and
set `srv.Status.StorageResizePending`. Add `ResizePending: srv.Status.StorageResizePending`
to the `ServerView` literal in `collectViews`. In `DecidePersistentSize`, after
the stale block:

```go
	resizing := make([]int32, 0, len(held))
	for ordinal, name := range held {
		if ordinal >= in.Replicas {
			continue
		}
		if viewByName(in.Views, name).ResizePending {
			resizing = append(resizing, ordinal)
		}
	}
	sort.Slice(resizing, func(i, j int) bool { return resizing[i] > resizing[j] })
	if len(resizing) > 0 {
		decision.Delete = append(decision.Delete, held[resizing[0]])
	}
```

guarded by the same `groupRecovered` gate the stale class uses, and reached only
when the stale block nominated nothing.

- [ ] **Step 4: Run, regenerate, run the suite, commit**

```bash
nix develop -c make manifests generate
nix develop -c make test
git add api/v1alpha1/ internal/controller/ config/crd/bases/
git commit -m "A filesystem that needs a restart to grow gets one"
```

---

## Task 9: Say so when the storage class cannot grow

**Files:**
- Modify: `api/v1alpha1/common_types.go` (after `ConditionNodeDraining:55`)
- Modify: `internal/controller/servergroup_controller.go`
- Modify: `internal/controller/servergroup_controller_test.go`

**Interfaces:**
- Produces: `spawneryv1alpha1.ConditionStorageResize = "StorageResize"`.

**Why a condition of its own.** Spec §4: `allowVolumeExpansion: false` is not an
edge case — `kind`'s own default `local-path` class is exactly that. Folding it
into `Degraded` would make "your storage cannot grow" and "your servers will not
start" the same field.

- [ ] **Step 1: Add the constant**

```go
	// ConditionStorageResize reports on a persistent group's attempt to grow
	// its claims. It is separate from Degraded on purpose: a storage class that
	// refuses expansion and a group whose servers will not start are different
	// problems with different remedies, and one field cannot carry both.
	ConditionStorageResize = "StorageResize"
```

- [ ] **Step 2: Write the failing test**

```go
func TestAGroupSaysWhenItsStorageClassCannotGrow(t *testing.T) {
	// Group at 2Gi against a claim whose class refuses expansion; the patch in
	// Task 7 comes back as a Forbidden/Invalid error.
	reconcileGroupOnce(t)

	got := &spawneryv1alpha1.ServerGroup{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "g", Namespace: ns}, got); err != nil {
		t.Fatalf("get group: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionStorageResize)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected StorageResize=False, got %+v", cond)
	}
	if !strings.Contains(cond.Message, "allowVolumeExpansion") {
		t.Fatalf("the message does not name the cause: %q", cond.Message)
	}

	degraded := meta.FindStatusCondition(got.Status.Conditions, spawneryv1alpha1.ConditionDegraded)
	if degraded != nil && degraded.Status == metav1.ConditionTrue {
		t.Fatal("a storage class that cannot expand is not a degraded group")
	}
}
```

The second assertion is the one that matters: it pins the separation the
condition exists for, and without it someone could satisfy this test by setting
`Degraded` as well.

- [ ] **Step 3: Implement, run, commit**

Publish the condition from the group's reconcile, with `True` when every claim
matches its spec, `False` with a message naming the storage class when a patch
was refused, and an event beside it. Then:

```bash
nix develop -c make test
git add api/v1alpha1/common_types.go internal/controller/ config/crd/bases/
git commit -m "A storage class that cannot grow says so where an operator looks"
```

---

## Task 10: The record

**Files:**
- Modify: `docs/known-issues.md`
- Modify: `docs/handover-milestone-5.md`
- Create: `docs/runbook-milestone-5b-evidence.md`

- [ ] **Step 1: `docs/known-issues.md` — a "From milestone 5b" section**

One line each, checked against the code rather than against this plan:

- **A permanently broken ordinal stalls the group's whole update** (spec §2).
  Inherited from StatefulSet's shape knowingly; `Degraded` is what makes it
  visible, and Task 6 is what makes `Degraded` arrive.
- **A spec edit made during the upgrade window can be missed on an ordinal that
  is adopted rather than replaced** (spec §3.6), bounded by the next edit.
- **Widening the proxy hash rolls every proxy group once on upgrade** (spec
  §3.7). No adopt-on-empty escape exists, because the label is present and
  merely different.
- **`DesiredProxyHash` takes the agent endpoint and `DesiredServerHash` does
  not** (Task 1's doc comment). An operator restarted with a different
  `--operator-namespace` therefore rolls every proxy group. That is a real
  asymmetry rather than an oversight — a rolled proxy loses no world — but it is
  worth knowing before someone reads it as one.
- **The positive half of storage growth cannot be shown on `kind`'s default
  class** (spec §6).

- [ ] **Step 2: `docs/handover-milestone-5.md` — what 5c finds in place**

Rewrite the "What 5b and 5c find in place" section so it describes what 5c
finds, from spec §8: a working one-at-a-time recreate mechanism, a hash that
covers the forwarding secret's *name* but not its *contents* — which is exactly
5c's problem — `patch` authority over claims with an audit that turns red if 5c
widens it, and a streak that reaches `Degraded`.

- [ ] **Step 3: `docs/runbook-milestone-5b-evidence.md`**

Follow `docs/runbook-milestone-5a-evidence.md`'s shape, and read its
top-of-document note first: it records what its own run corrected and why a
runbook should predict what stays true rather than what is briefly visible.

Acceptance tests, in order:

1. **The update, on a two-ordinal group.** Place a block on each of `g-0` and
   `g-1`, change `maxPlayers`, and watch: `g-1` goes down first, comes back
   Ready, *then* `g-0` goes down. Both blocks survive. Confirm from UIDs before
   and after — not from a deletion timestamp, for the reason 5a's §8 records.
2. **The scale-down.** `replicas` 2 → 1, and `g-1` leaves while `g-0` is
   untouched; the claim `g-1-data` is still there afterwards.
3. **Growth, negative path, on the default cluster.** Raise `storage.size` and
   confirm `StorageResize=False` naming `local-path`, with `Degraded` absent.
4. **Growth, positive path — a section a driver may skip.** Requires
   `csi-driver-host-path`, which supports expansion. Raise `storage.size`, watch
   the claim grow, and if the driver sets `FileSystemResizePending`, watch the
   ordinal restart once and come back with the larger volume.
5. **The motd fix.** Change a `ProxyGroup`'s `motd` and confirm the proxy rolls
   and the new motd appears in the client's server list. Note in the runbook
   that the *first* reconcile after upgrading rolls every proxy group once
   regardless, so this must be measured after that settles or it proves nothing.

Mark it **NOT YET DRIVEN**, the way 5a's was, and say it is driven by the human
partner and the acting agent together after the branch review.

- [ ] **Step 4: Commit**

```bash
git add docs/
git commit -m "What 5b leaves open, and how to prove it works"
```

---

## Self-Review

**Spec coverage.** §1 → Tasks 1-9. §2 and §2.1 (invariant, two gates, priority)
→ Task 4, wired in Task 5. §3.1-3.5 (the hash) → Task 1. §3.6 (adopt on empty)
→ Tasks 3 and 4. §3.7 (motd) → Task 2. §4 (growth, restart, condition, RBAC) →
Tasks 7, 8, 9. §5 (streak) → Task 6. §6 (tests, mutations, evidence limits) →
the mutation steps in Tasks 4 and 6 and the runbook in Task 10. §7 (CRD
changes) → Tasks 3, 8, 9. §8 (what 5c finds) → Task 10 Step 2.

**Signature changes, gathered because three tasks change a caller.**
`newServer` gains `podHash` (Task 3, two callers). `DesiredProxyHash` gains
`configValues` (Task 2, one caller). `CountFailures` gains `requiredOrdinals`
(Task 6, one call site). `size()` gains the `Network` (Task 5). An implementer
who finds a compile error at one of these has hit a known change, not a
mistake.

**Names used consistently throughout:** `DesiredServerHash`,
`DesiredProxyHash`, `serverConfigValues`, `proxyConfigValues`,
`takedownInFlight`, `groupRecovered`, `growClaim`, `PersistentInputs.PodHash`,
`ServerView.PodHash`, `ServerView.ResizePending`,
`Server.Spec.PodHash`, `Server.Status.StorageResizePending`,
`ConditionStorageResize`.

**One gap, recorded rather than papered over.** Task 9's test needs a storage
class that refuses expansion, and envtest has no provisioner and no storage
classes at all. The implementer will have to make the patch fail some other way
— a claim whose class name does not resolve, or an interceptor on the fake
client — and should say in the test's own comment which mechanism it used and
what that does *not* prove. Reaching for `kind` from a unit test is not an
option; the honest coverage for the real refusal is the runbook's §3.
