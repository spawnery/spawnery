# On-demand servers implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `ServerGroup` of type `OnDemand` whose members are asked for by
name over the agent channel, each carrying its own world, so that one player's
private server can be started and stopped by a plugin.

**Architecture:** The third group type is a template that sizes nothing. Its
members are ordinary `Server` objects named `<group>-<key>`, created and
deleted by the operator in answer to two new requests on the agent channel —
the route `ScaleBoost` already takes. Storage is inherited: the claim is named
from the server, and this operator never deletes one, so the same key later
finds the same world.

**Tech Stack:** Go 1.24 with controller-runtime, envtest for anything that
touches the API server, protobuf/gRPC for the agent channel, Kotlin and Java
(Gradle) for the agents, Nix for every build and test command.

**Spec:** `docs/superpowers/specs/2026-09-20-on-demand-servers-design.md` —
read it first; this plan argues from it and does not repeat its reasoning.

## Global Constraints

- **Every command runs in the Nix dev shell.** The full form is
  `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c <command>`.
  Steps below write it as `nix develop -c <command>`; that is the same thing.
  A `cd` before `nix develop` breaks the build — the flake path is an argument.
- **Branch:** `feat/on-demand-servers`, which already carries the spec commit.
- **Machine:** `paul-desktop`, 32 cores and 93 GB. Run the full suite without
  `-p 1`; that flag is for the development VM.
- **Copyright header:** every new file starts with the Apache header whose
  first line is `Copyright paul_wtf.` — copy it verbatim from a neighbouring
  file in the same directory.
- **Commits** are Conventional Commits with a scope naming the part touched,
  subject saying what changed, body saying why, wrapped at 72 columns, and
  ending with `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`.
  Commits and tags are gpg-signed; on `paul-desktop` the pinentry dialog opens
  as its own window, so just commit.
- **Generated files are committed and CI diffs them.** After any change to API
  types, kubebuilder markers or the `.proto`, run
  `nix develop -c make manifests generate proto` and commit what moves:
  `config/crd/bases/`, `config/rbac/role.yaml`,
  `charts/spawnery/templates/{crds,rbac}.yaml`, `zz_generated.deepcopy.go`,
  `internal/agentpb/`, `agent/common/src/proto/java/`, and
  `docs/reference/crds.md`.
- **No RBAC marker moves in this plan.** If `config/rbac/role.yaml` changes,
  something is wrong: stop and find out what, rather than editing
  `internal/rbacaudit/required.go` to match.
- **Test helpers: use the file's own.** These exist and are used as written:
  `testenv.Client(t)` and `testenv.Namespace(t, ctx, c)` (`api/v1alpha1`),
  `newFixture(t)` with `f.c`, `f.ns`, `f.ctx` (`internal/controller`,
  `suite_test.go:219`), `dialAgent`, `f.token(sa, audiences, pod)` and
  `makeServer` (`internal/agentserver`, `retire_envtest_test.go`). Anything
  else a step names — `f.reconcileGroup`, `testGroup`, `testConnector` and
  their like — is a placeholder for the helper the neighbouring test in that
  same file already uses. Find it and use it; do not introduce a second style
  and do not write a parallel fixture.
- **Names fixed across tasks**, used exactly as written:
  `spawneryv1alpha1.ServerGroupOnDemand` (value `"OnDemand"`),
  `(*ServerGroup).IsOnDemand() bool`, `ServerGroupSpec.MaxInstances *int32`,
  `ServerSpec.Key string`, `instance.Name(group, key string) (string, error)`,
  `agentserver.StartedServer{Name string; AlreadyRunning bool}`,
  `SpawneryApi.startServer(String group, String key)`,
  `SpawneryApi.stopServer(String server)`.

---

### Task 1: The type, its fields and its rules

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (the `ServerGroupType` enum and
  its marker, the `ServerGroupSpec` CEL block, `MaxInstances`, `IsOnDemand`,
  `DesiredReplicas`)
- Modify: `api/v1alpha1/server_types.go` (`ServerSpec.Key`)
- Test: `api/v1alpha1/servergroup_envtest_test.go`,
  `api/v1alpha1/servergroup_types_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `ServerGroupOnDemand`, `(*ServerGroup).IsOnDemand() bool`,
  `ServerGroupSpec.MaxInstances *int32`, `ServerSpec.Key string`.
  `DesiredReplicas()` returns 0 for an `OnDemand` group.

- [ ] **Step 1: Write the failing CEL tests**

Add to `api/v1alpha1/servergroup_envtest_test.go`, beside the existing
`ephemeralGroup` and `persistentGroup` builders:

```go
func onDemandGroup(ns, name string) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:   spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:         spawneryv1alpha1.ServerGroupOnDemand,
			Image:        "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:   10,
			MaxInstances: ptr.To[int32](50),
			Storage: &spawneryv1alpha1.StorageSpec{
				Size:             resource.MustParse("2Gi"),
				StorageClassName: ptr.To("longhorn"),
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		},
	}
}

func TestServerGroupOnDemandAccepted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	if err := c.Create(ctx, onDemandGroup(ns, "private-servers")); err != nil {
		t.Fatalf("create on-demand group: %v", err)
	}
}

func TestServerGroupOnDemandRefusesSizingFields(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	tests := map[string]func(*spawneryv1alpha1.ServerGroup){
		"scaling": func(g *spawneryv1alpha1.ServerGroup) {
			g.Spec.Scaling = &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 2, SpareSlots: 1}
		},
		"replicas": func(g *spawneryv1alpha1.ServerGroup) {
			g.Spec.Replicas = ptr.To[int32](1)
		},
		"update": func(g *spawneryv1alpha1.ServerGroup) {
			g.Spec.Update = &spawneryv1alpha1.UpdateSpec{MaxUnavailable: 1}
		},
	}
	for field, mutate := range tests {
		t.Run(field, func(t *testing.T) {
			g := onDemandGroup(ns, "refuses-"+field)
			mutate(g)
			if err := c.Create(ctx, g); err == nil {
				t.Fatalf("spec.%s was accepted for type OnDemand", field)
			}
		})
	}
}

func TestServerGroupOnDemandRequiresStorageAndCeiling(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	noStorage := onDemandGroup(ns, "no-storage")
	noStorage.Spec.Storage = nil
	if err := c.Create(ctx, noStorage); err == nil {
		t.Fatal("an on-demand group without spec.storage was accepted")
	}

	noCeiling := onDemandGroup(ns, "no-ceiling")
	noCeiling.Spec.MaxInstances = nil
	if err := c.Create(ctx, noCeiling); err == nil {
		t.Fatal("an on-demand group without spec.maxInstances was accepted")
	}
}

func TestMaxInstancesIsOnDemandOnly(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	g := ephemeralGroup(ns, "lobby-with-ceiling")
	g.Spec.MaxInstances = ptr.To[int32](5)
	if err := c.Create(ctx, g); err == nil {
		t.Fatal("spec.maxInstances was accepted on an ephemeral group")
	}
}
```

And in `api/v1alpha1/servergroup_types_test.go`, a pure unit test:

```go
func TestDesiredReplicasIsZeroForOnDemand(t *testing.T) {
	g := &spawneryv1alpha1.ServerGroup{
		Spec: spawneryv1alpha1.ServerGroupSpec{Type: spawneryv1alpha1.ServerGroupOnDemand},
	}
	if got := g.DesiredReplicas(); got != 0 {
		t.Fatalf("DesiredReplicas() = %d, want 0", got)
	}
	if !g.IsOnDemand() {
		t.Fatal("IsOnDemand() = false for a group of type OnDemand")
	}
	if g.IsEphemeral() {
		t.Fatal("IsEphemeral() = true for a group of type OnDemand")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `nix develop -c go test ./api/v1alpha1/ -run 'OnDemand|MaxInstances' -count=1`
Expected: compile failure — `ServerGroupOnDemand`, `MaxInstances`, `IsOnDemand`
are undefined.

- [ ] **Step 3: Add the type, the field and the helpers**

In `api/v1alpha1/servergroup_types.go`, extend the enum marker and the
constants:

```go
// ServerGroupType selects the operating mode of a group.
// +kubebuilder:validation:Enum=Ephemeral;Persistent;OnDemand
type ServerGroupType string

const (
	// ServerGroupEphemeral loses its state on stop: minigames and lobbies.
	ServerGroupEphemeral ServerGroupType = "Ephemeral"
	// ServerGroupPersistent keeps its world on a PVC: survival and creative.
	ServerGroupPersistent ServerGroupType = "Persistent"
	// ServerGroupOnDemand is a template whose members are asked for by name
	// rather than counted: one world per key, started when somebody asks and
	// gone when they are done. The group itself never creates one.
	ServerGroupOnDemand ServerGroupType = "OnDemand"
)
```

Add to the `ServerGroupSpec` CEL block, after the existing `Persistent` rules:

```go
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || !has(self.scaling)",message="spec.scaling is not allowed for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || !has(self.replicas)",message="spec.replicas is not allowed for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || !has(self.update)",message="spec.update is not allowed for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || has(self.storage)",message="spec.storage is required for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type != 'OnDemand' || has(self.maxInstances)",message="spec.maxInstances is required for type OnDemand"
// +kubebuilder:validation:XValidation:rule="self.type == 'OnDemand' || !has(self.maxInstances)",message="spec.maxInstances is only allowed for type OnDemand"
```

And the field, beside `Replicas`:

```go
	// MaxInstances is how many members this group may have at once.
	//
	// A fleet ceiling and not a per-player quota: who may have how many
	// private servers is a question about a player, a purchase and a ban,
	// and the system that knows those three is the one that answers it.
	//
	// Zero is legal and means the group is closed -- new starts are refused
	// and every world stays where it is, which is the state an incident wants
	// and a deletion would not give. Required rather than defaulted: a
	// ceiling nobody chose is a ceiling nobody thought about.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxInstances *int32 `json:"maxInstances,omitempty"`
```

The helper, beside `IsEphemeral`:

```go
// IsOnDemand reports whether this group's members are asked for by name.
func (g *ServerGroup) IsOnDemand() bool {
	return g.Spec.Type == ServerGroupOnDemand
}
```

And the explicit branch in `DesiredReplicas`, before the `spec.replicas`
fallthrough:

```go
	// Nothing is desired: an on-demand group's members exist because somebody
	// asked for them. The fallthrough below would reach the same 0 through
	// spec.replicas being nil, and an answer that correct by accident is one
	// a later edit can break without a test noticing.
	if g.IsOnDemand() {
		return 0
	}
```

In `api/v1alpha1/server_types.go`, add to `ServerSpec`, after `Ordinal`:

```go
	// Key is the caller's name for the world this member carries, and it is
	// set for a member of an OnDemand group and for no other server.
	//
	// It is here for the reason Ordinal is: a Server has to be able to say
	// what kind of member it is without its group, and the Server controller
	// reconstructs a synthetic group from this object alone when the real one
	// is gone. An inference from Ordinal alone was exhaustive while there were
	// two types; with three, a member with neither marker would read as
	// ephemeral, which is the one answer that is wrong about its world.
	// +optional
	Key string `json:"key,omitempty"`
```

- [ ] **Step 4: Regenerate and run the tests**

Run: `nix develop -c make manifests generate`
Then: `nix develop -c go test ./api/v1alpha1/ -count=1`
Expected: PASS. `git status` shows `config/crd/bases/`,
`charts/spawnery/templates/crds.yaml`, `zz_generated.deepcopy.go` and
`docs/reference/crds.md` changed, and `config/rbac/role.yaml` unchanged.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1 config/crd/bases charts/spawnery/templates/crds.yaml docs/reference/crds.md
git commit -m "feat(api): a third group type whose members are named"
```

---

### Task 2: The world follows the type

**Files:**
- Modify: `internal/podspec/server.go` (`dataVolume`, and only that)
- Test: `internal/podspec/server_test.go`

**Interfaces:**
- Consumes: `ServerGroupOnDemand` (Task 1).
- Produces: a pod for an `OnDemand` member mounts
  `podspec.DataClaimName(srv.Name)`, and its restart policy stays `Never`.

- [ ] **Step 1: Write the failing test**

In `internal/podspec/server_test.go`:

```go
func TestOnDemandServerMountsItsOwnClaim(t *testing.T) {
	network, group, srv := testNetwork(), testGroup(), testServer()
	group.Spec.Type = spawneryv1alpha1.ServerGroupOnDemand
	group.Spec.Storage = &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("2Gi")}
	srv.Name = "private-servers-c0ffee"
	srv.Spec.Key = "c0ffee"

	pod, err := podspec.BuildServerPod(network, group, srv, "operator:9443")
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}

	var data *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == podspec.DataVolumeName {
			data = &pod.Spec.Volumes[i]
		}
	}
	if data == nil {
		t.Fatal("the pod has no data volume")
	}
	if data.PersistentVolumeClaim == nil {
		t.Fatal("an on-demand member got an emptyDir, so its world would not survive a stop")
	}
	if got, want := data.PersistentVolumeClaim.ClaimName, podspec.DataClaimName(srv.Name); got != want {
		t.Errorf("claim = %q, want %q", got, want)
	}
	// Never, not Always: a player typing /stop is a player stopping their
	// server, and Always cannot tell that from a crash.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
}
```

`testNetwork`, `testGroup` and `testServer` are the builders the neighbouring
tests in this file already use; if their names differ, use theirs rather than
adding new ones.

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/podspec/ -run TestOnDemandServerMountsItsOwnClaim -count=1`
Expected: FAIL — the volume is an `emptyDir`, so `data.PersistentVolumeClaim`
is nil.

- [ ] **Step 3: Make the world a property of the type, not of one type**

In `internal/podspec/server.go`, add the predicate and use it in `dataVolume`
**and nowhere else**:

```go
// keepsWorld reports whether a server of this group has a world that outlives
// its pod. Persistent servers and on-demand members both do, and they are
// otherwise nothing alike: one is an ordinal a person wrote down, the other a
// key somebody asked for.
func keepsWorld(group *spawneryv1alpha1.ServerGroup) bool {
	return group.Spec.Type == spawneryv1alpha1.ServerGroupPersistent ||
		group.Spec.Type == spawneryv1alpha1.ServerGroupOnDemand
}

func dataVolume(group *spawneryv1alpha1.ServerGroup, srv *spawneryv1alpha1.Server) corev1.Volume {
	if keepsWorld(group) {
		return corev1.Volume{
			Name: DataVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: DataClaimName(srv.Name),
				},
			},
		}
	}
	return corev1.Volume{
		Name:         DataVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}
```

`restartPolicy` keeps its `== ServerGroupPersistent` comparison. Do not
"tidy" it into `keepsWorld`; the test above fails if you do.

- [ ] **Step 4: Run the package**

Run: `nix develop -c go test ./internal/podspec/ -count=1`
Expected: PASS, **including `TestDesiredServerHashGolden`** in
`hash_golden_test.go`. Nothing an existing group renders changed, so the
goldens must not move. If one does, stop: it means an existing type's pod
changed and every server in every installation would roll on upgrade.

- [ ] **Step 5: Commit**

```bash
git add internal/podspec
git commit -m "feat(podspec): an on-demand member keeps its world on a claim"
```

---

### Task 3: A Server says what kind of member it is without its group

**Files:**
- Modify: `internal/controller/server_controller.go` (the synthetic group
  around line 859)
- Test: `internal/controller/server_controller_test.go`

**Interfaces:**
- Consumes: `ServerSpec.Key`, `ServerGroupOnDemand` (Task 1).
- Produces: a `Server` with a non-empty `spec.key` and no group resolves to a
  synthetic group of type `OnDemand`.

- [ ] **Step 1: Write the failing test**

```go
func TestSyntheticGroupOfAnOnDemandMember(t *testing.T) {
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "private-servers-c0ffee", Namespace: "mc"},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "private-servers"},
			Key:      "c0ffee",
		},
	}
	got := syntheticGroup(srv)
	if got.Spec.Type != spawneryv1alpha1.ServerGroupOnDemand {
		t.Fatalf("type = %q, want OnDemand: a member whose group is gone must not read as ephemeral, "+
			"or its claim is skipped", got.Spec.Type)
	}
}
```

Name the call after whatever the function is actually called in
`server_controller.go` around line 848 — the block whose comment begins "The
type is read off the Server rather than assumed". If it is inlined rather than
a function, extract it first, unchanged, in its own commit, and then write
this test.

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/controller/ -run TestSyntheticGroupOfAnOnDemandMember -count=1`
Expected: FAIL with `type = "Ephemeral", want OnDemand`.

- [ ] **Step 3: Teach the inference the third answer**

```go
	// Three types and two markers: spec.ordinal is written by
	// createPersistentServer and by nothing else, spec.key by the on-demand
	// create and by nothing else, and a server with neither is ephemeral.
	// The order matters not at all -- no server ever carries both -- and the
	// exhaustiveness does: an on-demand member read as ephemeral loses the
	// !IsEphemeral() branch in Reconcile, which is the one that grows its
	// claim.
	groupType := spawneryv1alpha1.ServerGroupEphemeral
	switch {
	case srv.Spec.Ordinal != nil:
		groupType = spawneryv1alpha1.ServerGroupPersistent
	case srv.Spec.Key != "":
		groupType = spawneryv1alpha1.ServerGroupOnDemand
	}
```

Update the comment above the block that explains the two-type inference so it
describes three.

- [ ] **Step 4: Run the package**

Run: `nix develop -c go test ./internal/controller/ -count=1`
Expected: PASS. This package boots its own etcd and apiserver and takes about
85 seconds.

- [ ] **Step 5: Commit**

```bash
git add internal/controller
git commit -m "fix(controller): a member without its group is not ephemeral by default"
```

---

### Task 4: The group sizes nothing and sweeps what ended

**Files:**
- Modify: `internal/controller/servergroup_controller.go` (`size()`'s switch,
  the `pruneFailed` call site, a new sweep)
- Create: `internal/controller/ondemand.go`
- Test: `internal/controller/ondemand_envtest_test.go`

**Interfaces:**
- Consumes: `IsOnDemand()` (Task 1).
- Produces: an `OnDemand` group neither creates nor condemns members;
  `sweepOnDemand` deletes members in phase `Finished`, and `pruneFailed` keeps
  the newest `Failed` ones for diagnosis.

This is the task the spec calls the risk of the change: a missed branch here
deletes a player's running world to satisfy a replica count that does not
exist. Write the tests first and do not shorten them.

- [ ] **Step 1: Write the failing tests**

In a new `internal/controller/ondemand_envtest_test.go`, using `newFixture(t)`
from `suite_test.go` exactly as the neighbouring envtest files do:

```go
// The group must not size its own members. Every other group type answers
// "how many", and the one thing that must never happen here is that an
// answer of zero -- which is what every sizing rule computes for a group
// with no sizing fields -- is acted on.
func TestOnDemandGroupCreatesAndDeletesNothing(t *testing.T) {
	f := newFixture(t)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")

	for i := 0; i < 3; i++ {
		f.reconcileGroup(t, group)
	}

	var servers spawneryv1alpha1.ServerList
	if err := f.c.List(f.ctx, &servers, client.InNamespace(f.ns)); err != nil {
		t.Fatalf("list servers: %v", err)
	}
	if len(servers.Items) != 1 {
		t.Fatalf("after three passes the group has %d servers, want exactly the one that was asked for",
			len(servers.Items))
	}
	if servers.Items[0].Name != member.Name {
		t.Fatalf("server = %q, want %q", servers.Items[0].Name, member.Name)
	}
	if !servers.Items[0].DeletionTimestamp.IsZero() {
		t.Fatal("the group condemned a member nobody asked it to remove")
	}
}

// A member that said its round was over and stopped is gone: its world is on
// the claim, and the object holds the one name its owner needs to start again.
func TestOnDemandFinishedMemberIsSwept(t *testing.T) {
	f := newFixture(t)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.setPhase(t, member, phase.Finished)

	f.reconcileGroup(t, group)

	var got spawneryv1alpha1.Server
	err := f.c.Get(f.ctx, client.ObjectKeyFromObject(member), &got)
	if err == nil && got.DeletionTimestamp.IsZero() {
		t.Fatal("a finished member was kept, so its key cannot be started again")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get member: %v", err)
	}
}

// Nothing rolls. An image bump reaches a world the next time its owner starts
// it, and throwing a player out of their own world to apply one is the
// opposite of what a private server is for.
func TestOnDemandMemberIsNotRolledBySpecChange(t *testing.T) {
	f := newFixture(t)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.setPhase(t, member, phase.Ready)
	f.reconcileGroup(t, group)

	group.Spec.Image = "ghcr.io/spawnery/paper:1.21.4-0.2.0"
	if err := f.c.Update(f.ctx, group); err != nil {
		t.Fatalf("bump the image: %v", err)
	}
	f.reconcileGroup(t, group)

	var got spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(member), &got); err != nil {
		t.Fatalf("the running member was removed by a spec change: %v", err)
	}
	if got.Spec.Retire {
		t.Fatal("a spec change retired a running private server")
	}
	if !got.DeletionTimestamp.IsZero() {
		t.Fatal("a spec change condemned a running private server")
	}
}

// A broken world is kept, because somebody has to be able to look at it.
func TestOnDemandFailedMemberIsKeptForDiagnosis(t *testing.T) {
	f := newFixture(t)
	group := f.createOnDemandGroup(t, "private-servers", 50)
	member := f.createOnDemandMember(t, group, "c0ffee")
	f.setPhase(t, member, phase.Failed)

	f.reconcileGroup(t, group)

	var got spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKeyFromObject(member), &got); err != nil {
		t.Fatalf("a failed member was swept away with nothing left to read: %v", err)
	}
}
```

Write the three fixture helpers in the same file:

```go
func (f *fixture) createOnDemandGroup(t *testing.T, name string, maxInstances int32) *spawneryv1alpha1.ServerGroup {
	t.Helper()
	g := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:   spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:         spawneryv1alpha1.ServerGroupOnDemand,
			Image:        "ghcr.io/spawnery/paper:1.21.4-0.1.0",
			MaxPlayers:   10,
			MaxInstances: ptr.To(maxInstances),
			Storage:      &spawneryv1alpha1.StorageSpec{Size: resource.MustParse("2Gi")},
		},
	}
	if err := f.c.Create(f.ctx, g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	return g
}

func (f *fixture) createOnDemandMember(t *testing.T, g *spawneryv1alpha1.ServerGroup, key string) *spawneryv1alpha1.Server {
	t.Helper()
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: g.Name + "-" + key, Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: g.Name},
			Key:      key,
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create member: %v", err)
	}
	return srv
}

func (f *fixture) setPhase(t *testing.T, srv *spawneryv1alpha1.Server, p phase.Phase) {
	t.Helper()
	srv.Status.Phase = string(p)
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("set phase %s: %v", p, err)
	}
}
```

`f.reconcileGroup` is whatever the existing envtest files in this package call
to drive one `ServerGroup` pass — reuse it rather than writing a second one.

- [ ] **Step 2: Run them and watch them fail**

Run: `nix develop -c go test ./internal/controller/ -run TestOnDemand -count=1`
Expected: the first test fails or panics inside `size()`'s default branch,
which is the persistent path reaching for `spec.replicas`; the finished-sweep
test fails because nothing deletes the member.

- [ ] **Step 3: Give the switch its own case and add the sweep**

In `size()` (around line 898), add the case **before** the default:

```go
	case group.IsOnDemand():
		// No size is decided and none can be: the members of this group
		// exist because somebody asked for them by name, and every rule
		// below computes "how many", which for this group is a question
		// with no answer rather than one whose answer is zero. Falling
		// through to the persistent path would read spec.replicas -- nil
		// here -- as zero servers wanted, and condemn every world running.
```

In a new `internal/controller/ondemand.go`:

```go
// sweepOnDemand removes the members of an on-demand group whose run is over.
//
// Two phases and two answers. Finished means the server said its round was
// over and then its pod stopped -- a player closing their own world -- and
// there is nothing about it left to keep: the world is on its claim, which
// this operator never deletes, while the object holds the one name its owner
// needs in order to start again. Failed is not swept here: a world that broke
// is one somebody has to be able to look at, and pruneFailed already keeps
// the newest of them and no more. What stops a corpse from blocking a restart
// is the start request, which replaces a terminal member of the key it was
// asked for.
func (r *ServerGroupReconciler) sweepOnDemand(
	ctx context.Context,
	group *spawneryv1alpha1.ServerGroup,
	views []ServerView,
	servers map[string]*spawneryv1alpha1.Server,
) error {
	for _, v := range views {
		if v.Phase != phase.Finished || v.leaving() {
			continue
		}
		if err := r.deleteServer(ctx, group, servers, v.Name, "InstanceFinished",
			"removing finished on-demand member %s, its world is on its claim"); err != nil {
			return err
		}
	}
	return nil
}
```

At the `pruneFailed` call site (around line 751), make both types reach the
sweep they need:

```go
	if group.IsEphemeral() || group.IsOnDemand() {
		if err := r.pruneFailed(ctx, group, views, servers); err != nil {
			return ctrl.Result{}, err
		}
	}
	if group.IsOnDemand() {
		if err := r.sweepOnDemand(ctx, group, views, servers); err != nil {
			return ctrl.Result{}, err
		}
	}
```

- [ ] **Step 4: Run the package**

Run: `nix develop -c go test ./internal/controller/ -count=1`
Expected: PASS, the three new tests included.

- [ ] **Step 5: Commit**

```bash
git add internal/controller
git commit -m "feat(controller): an on-demand group sizes nothing and sweeps what ended"
```

---

### Task 5: The wire

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto`
- Test: `internal/agentpb/contract_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `agentpb.StartServerRequest{Group, Key}`,
  `agentpb.StartServerResult{Server, AlreadyRunning}`,
  `agentpb.StopServerRequest{Server}`, `agentpb.StopServerResult{Server}`,
  the two new `CloudRequest`/`CloudResponse` oneof arms, and
  `agentpb.GroupState_ON_DEMAND`.

- [ ] **Step 1: Write the failing test**

In `internal/agentpb/contract_test.go`, beside the existing contract
assertions:

```go
func TestOnDemandRequestsAreOnTheWire(t *testing.T) {
	req := &agentpb.CloudRequest{
		Id: 1,
		Request: &agentpb.CloudRequest_StartServer{
			StartServer: &agentpb.StartServerRequest{Group: "private-servers", Key: "c0ffee"},
		},
	}
	if req.GetStartServer().GetKey() != "c0ffee" {
		t.Fatal("the key does not survive the round trip through the oneof")
	}
	if agentpb.GroupState_ON_DEMAND == agentpb.GroupState_KIND_UNSPECIFIED {
		t.Fatal("ON_DEMAND must be its own value, not the unspecified one")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/agentpb/ -count=1`
Expected: compile failure — the types do not exist.

- [ ] **Step 3: Add the messages**

In `proto/spawnery/agent/v1alpha1/agent.proto`, add two arms to `CloudRequest`
and two to `CloudResponse`, using the next free field numbers in each oneof,
and the messages, in the house style — the prose in this file is part of the
contract, so write it:

```proto
// StartServerRequest asks for the member of an OnDemand group that carries
// this key.
//
// It carries no namespace, for the reason RetireRequest carries none: the
// group is resolved inside the namespace the pod's own token authenticated.
//
// The key is the caller's name for a world, not a name for a server: the
// operator composes "<group>-<key>" and that server is where this key's world
// is, this time and every later time. Asking twice while it runs is answered
// rather than refused -- see already_running -- because a caller that has to
// take a lock to ask a question is a caller that will forget to.
message StartServerRequest {
  string group = 1;
  string key = 2;
}

// StartServerResult is the member that now exists.
message StartServerResult {
  // The server the operator composed, echoed so a caller that built the key
  // from a UUID sees the name players and logs will use.
  string server = 1;
  // True when the member was already there, which is a success and not a
  // refusal: what the caller asked for is the case.
  bool already_running = 2;
}

// StopServerRequest deletes one member of an OnDemand group.
//
// **A stop and not a retire.** Retiring closes a server's door and waits for
// it to empty in its own time; this says the owner is done with it, so the
// players on it are moved through the proxies inside the group's own drain
// timeout and the pod goes. The world is untouched: it is on a claim this
// operator never deletes, and the next start of the same key finds it.
//
// It refuses a server that is not a member of an OnDemand group. A caller
// naming an ordinary backend here has made a mistake that would otherwise
// delete a lobby.
message StopServerRequest {
  string server = 1;
}

// StopServerResult says the member is going.
message StopServerResult {
  string server = 1;
}
```

And in `GroupState.Kind`:

```proto
    ON_DEMAND = 4;
```

- [ ] **Step 4: Regenerate and run**

Run: `nix develop -c make proto`
Then: `nix develop -c go test ./internal/agentpb/ -count=1`
Expected: PASS. `internal/agentpb/` and `agent/common/src/proto/java/` are
regenerated and both are committed.

- [ ] **Step 5: Commit**

```bash
git add proto internal/agentpb agent/common/src/proto/java
git commit -m "feat(proto): two requests for a server asked for by name"
```

---

### Task 6: The operator answers start and stop

**Files:**
- Create: `internal/instance/name.go`, `internal/instance/name_test.go`
- Modify: `internal/agentserver/writer.go` (the `ClusterWriter` interface, its
  errors, `KubeWriter`)
- Modify: `internal/agentserver/requests.go` (the dispatch and two verbs)
- Test: `internal/agentserver/ondemand_envtest_test.go`

**Interfaces:**
- Consumes: Tasks 1, 4, 5.
- Produces: `instance.Name(group, key string) (string, error)`;
  `ClusterWriter.StartServer(ctx, namespace, group, key) (StartedServer, error)`
  and `ClusterWriter.StopServer(ctx, namespace, name) error`;
  `StartedServer{Name string; AlreadyRunning bool}`; the sentinels
  `ErrGroupNotOnDemand`, `ErrTooManyInstances`, `ErrNotAnInstance`.

- [ ] **Step 1: Write the failing name test**

`internal/instance/name_test.go`:

```go
func TestNameComposesGroupAndKey(t *testing.T) {
	got, err := instance.Name("private-servers", "c0ffee")
	if err != nil {
		t.Fatalf("Name: %v", err)
	}
	if got != "private-servers-c0ffee" {
		t.Fatalf("Name = %q", got)
	}
}

func TestNameRefusesWhatKubernetesWould(t *testing.T) {
	tests := map[string]struct{ group, key string }{
		"upper case":    {"private-servers", "C0FFEE"},
		"underscore":    {"private-servers", "c0f_fee"},
		"empty":         {"private-servers", ""},
		"too long":      {"private-servers-of-the-whole-network", strings.Repeat("a", 36)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := instance.Name(tc.group, tc.key); err == nil {
				t.Fatal("accepted, so the failure would land on a player's start instead")
			}
		})
	}
}

func TestNameAcceptsAUUID(t *testing.T) {
	if _, err := instance.Name("private-servers", "3f2b1c8a-9d4e-4f11-b2a6-77c0de1234ab"); err != nil {
		t.Fatalf("a UUID key was refused: %v", err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/instance/ -count=1`
Expected: the package does not exist.

- [ ] **Step 3: Write the name rule**

`internal/instance/name.go`:

```go
// Package instance composes the name of a member of an OnDemand group.
//
// Its own package for the reason internal/boost is one: the operator's request
// endpoint and its controllers both need this rule and must not import each
// other.
package instance

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ErrBadKey is a key that cannot be part of an object's name.
var ErrBadKey = errors.New("that key cannot be part of a server name")

// MaxNameLength is what a Server's name may be, because it is a DNS label and
// the pod that carries it is named from it.
const MaxNameLength = validation.DNS1123LabelMaxLength

// Name composes the member of group that carries key.
//
// Checked here rather than left to the API server: a name that is refused on
// create fails a player's start with whatever the apiserver says about label
// syntax, which is an answer about Kubernetes to a person asking about their
// server. A group whose own name is long is the case that bites -- a
// 36-character UUID key leaves 26 characters for the group -- and the refusal
// says so.
func Name(group, key string) (string, error) {
	if errs := validation.IsDNS1123Label(key); len(errs) > 0 {
		return "", fmt.Errorf("%w: %s", ErrBadKey, errs[0])
	}
	name := group + "-" + key
	if len(name) > MaxNameLength {
		return "", fmt.Errorf("%w: %q is %d characters and a server name may be %d",
			ErrBadKey, name, len(name), MaxNameLength)
	}
	return name, nil
}
```

- [ ] **Step 4: Run it**

Run: `nix develop -c go test ./internal/instance/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/instance
git commit -m "feat(instance): the name a key composes"
```

- [ ] **Step 6: Write the failing writer and verb tests**

`internal/agentserver/ondemand_envtest_test.go`, following
`retire_envtest_test.go` for the fixture, `dialAgent` and `f.token`:

```go
// startOverTheWire asks on a real proxy stream and returns the answer. The
// consumer's plugin runs on a proxy, so that is the session this is asked on.
func startOverTheWire(t *testing.T, f *serverFixture, pod *corev1.Pod, group, key string) *agentpb.CloudResponse {
	t.Helper()
	return askOverTheWire(t, f, pod, &agentpb.CloudRequest_StartServer{
		StartServer: &agentpb.StartServerRequest{Group: group, Key: key},
	})
}

func TestStartCreatesTheMemberAndEchoesItsName(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := makePod(t, f)

	resp := startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if resp.GetError() != nil {
		t.Fatalf("refused: %s", resp.GetError().GetMessage())
	}
	if got := resp.GetStartServer().GetServer(); got != "private-servers-c0ffee" {
		t.Fatalf("server = %q", got)
	}
	if resp.GetStartServer().GetAlreadyRunning() {
		t.Fatal("already_running on the first start")
	}

	// An answer is not evidence that anything was written.
	var srv spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "private-servers-c0ffee"}, &srv); err != nil {
		t.Fatalf("the member was not created: %v", err)
	}
	if srv.Spec.Key != "c0ffee" {
		t.Errorf("spec.key = %q, want c0ffee", srv.Spec.Key)
	}
	if srv.Spec.GroupRef.Name != "private-servers" {
		t.Errorf("spec.groupRef = %q", srv.Spec.GroupRef.Name)
	}
}

func TestStartTwiceIsAnAnswerAndNotASecondServer(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := makePod(t, f)

	startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	resp := startOverTheWire(t, f, pod, "private-servers", "c0ffee")
	if resp.GetError() != nil {
		t.Fatalf("the second start was refused: %s", resp.GetError().GetMessage())
	}
	if !resp.GetStartServer().GetAlreadyRunning() {
		t.Fatal("already_running is false on the second start")
	}

	var servers spawneryv1alpha1.ServerList
	if err := f.c.List(f.ctx, &servers, client.InNamespace(f.ns)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(servers.Items) != 1 {
		t.Fatalf("%d servers exist, want 1", len(servers.Items))
	}
}

func TestStartRefusesPastTheCeiling(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 1)
	pod := makePod(t, f)

	startOverTheWire(t, f, pod, "private-servers", "one")
	resp := startOverTheWire(t, f, pod, "private-servers", "two")
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
}

func TestStartRefusesAGroupThatIsNotOnDemand(t *testing.T) {
	f := newServerFixture(t)
	makeEphemeralGroup(t, f, "lobby")
	pod := makePod(t, f)

	resp := startOverTheWire(t, f, pod, "lobby", "c0ffee")
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
}

// The other half of the audit in §3.7: the boost headroom check refuses
// anything that is not an ephemeral group with scaling, and a third type must
// not slip past it into a ScaleBoost that is created, counted, and changes
// nothing.
func TestBoostRefusesAnOnDemandGroup(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := makePod(t, f)

	resp := askOverTheWire(t, f, pod, &agentpb.CloudRequest_Boost{
		Boost: &agentpb.BoostRequest{Group: "private-servers", Replicas: 1},
	})
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
}

func TestStopDeletesTheMember(t *testing.T) {
	f := newServerFixture(t)
	makeOnDemandGroup(t, f, "private-servers", 2)
	pod := makePod(t, f)
	startOverTheWire(t, f, pod, "private-servers", "c0ffee")

	resp := stopOverTheWire(t, f, pod, "private-servers-c0ffee")
	if resp.GetError() != nil {
		t.Fatalf("refused: %s", resp.GetError().GetMessage())
	}
	var srv spawneryv1alpha1.Server
	err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "private-servers-c0ffee"}, &srv)
	if err == nil && srv.DeletionTimestamp.IsZero() {
		t.Fatal("the member is still there and not going")
	}
}

// The mistake this refusal exists for: a caller naming a lobby would
// otherwise delete it.
func TestStopRefusesAServerThatIsNotAnInstance(t *testing.T) {
	f := newServerFixture(t)
	makeEphemeralGroup(t, f, "lobby")
	makeServer(t, f, "lobby-abc")
	pod := makePod(t, f)

	resp := stopOverTheWire(t, f, pod, "lobby-abc")
	if resp.GetError().GetReason() != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED", resp.GetError().GetReason())
	}
	var srv spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: f.ns, Name: "lobby-abc"}, &srv); err != nil {
		t.Fatalf("the lobby server was deleted by a refused request: %v", err)
	}
}
```

Write `askOverTheWire`, `stopOverTheWire`, `makeOnDemandGroup`,
`makeEphemeralGroup` and `makePod` in this file, modelled on
`retireOverTheWire` and `makeServer` in `retire_envtest_test.go`. One pod per
test unless the test is about asking twice.

- [ ] **Step 7: Run them and watch them fail**

Run: `nix develop -c go test ./internal/agentserver/ -run 'TestStart|TestStop' -count=1`
Expected: compile failure — `StartServer` is not on `ClusterWriter`.

- [ ] **Step 8: Extend the writer**

In `internal/agentserver/writer.go`, the sentinels beside the existing ones:

```go
// ErrGroupNotOnDemand is what a start gets for a group whose members are
// counted rather than named. Creating a Server in one by hand would make the
// group's own sizing pass condemn it on the next reconcile, which reads from
// outside as a server that started and vanished.
var ErrGroupNotOnDemand = errors.New("that group is not on-demand")

// ErrTooManyInstances is the group's own ceiling, reached.
var ErrTooManyInstances = errors.New("that group is at spec.maxInstances")

// ErrNotAnInstance is a stop aimed at a server that no key names.
var ErrNotAnInstance = errors.New("that server is not an on-demand member")
```

The two methods on the interface, with their prose:

```go
	// StartServer creates the member of an OnDemand group that carries key.
	//
	// Reports AlreadyRunning for a member that was already there, which is a
	// success: what the caller asked for is the case. It returns
	// ErrNoSuchGroup, ErrGroupNotOnDemand, ErrTooManyInstances, or
	// instance.ErrBadKey for a key no name can be built from.
	StartServer(ctx context.Context, namespace, group, key string) (StartedServer, error)

	// StopServer deletes one member of an OnDemand group.
	//
	// It returns ErrNoSuchServer for a name this namespace does not have and
	// ErrNotAnInstance for a server that is not a member of such a group --
	// the refusal that keeps a mistyped name from deleting a lobby.
	StopServer(ctx context.Context, namespace, name string) error
```

```go
// StartedServer is the member a start request produced.
type StartedServer struct {
	Name           string
	AlreadyRunning bool
}
```

And the `KubeWriter` implementation:

```go
// StartServer creates the member, or reports the one that is already there.
//
// The count that bounds it is of members that are not terminal. A failed
// world is kept for diagnosis and a finished one is swept within a pass, and
// counting either against the ceiling would make a group drift closed as its
// players' servers ended.
//
// The create is what decides the race between two callers asking for the same
// key: AlreadyExists comes back to exactly one of them, and it is the answer
// rather than an error -- which is why nothing here takes a lock and why the
// read above it is an optimisation and not the bound.
func (w KubeWriter) StartServer(
	ctx context.Context, namespace, group, key string,
) (StartedServer, error) {
	name, err := instance.Name(group, key)
	if err != nil {
		return StartedServer{}, err
	}

	var g spawneryv1alpha1.ServerGroup
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: group}, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return StartedServer{}, ErrNoSuchGroup
		}
		return StartedServer{}, err
	}
	if !g.IsOnDemand() {
		return StartedServer{}, ErrGroupNotOnDemand
	}

	var members spawneryv1alpha1.ServerList
	if err := w.Client.List(ctx, &members, client.InNamespace(namespace)); err != nil {
		return StartedServer{}, err
	}
	live := 0
	for i := range members.Items {
		m := &members.Items[i]
		if m.Spec.GroupRef.Name != group || m.Spec.Key == "" {
			continue
		}
		if m.Name == name {
			// A terminal run of this very key is replaced rather than
			// reported: its world is on the claim, the object is a corpse,
			// and refusing here would leave the owner waiting out a
			// retention they cannot see.
			if isTerminal(m.Status.Phase) {
				if err := w.Client.Delete(ctx, m); err != nil && !apierrors.IsNotFound(err) {
					return StartedServer{}, err
				}
				continue
			}
			return StartedServer{Name: name, AlreadyRunning: true}, nil
		}
		if !isTerminal(m.Status.Phase) {
			live++
		}
	}
	if g.Spec.MaxInstances != nil && int32(live) >= *g.Spec.MaxInstances {
		return StartedServer{}, ErrTooManyInstances
	}

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       g.Name,
				UID:        g.UID,
			}},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: group},
			Key:      key,
		},
	}
	if err := w.Client.Create(ctx, srv); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return StartedServer{Name: name, AlreadyRunning: true}, nil
		}
		return StartedServer{}, err
	}
	return StartedServer{Name: name}, nil
}

// StopServer deletes one member.
//
// The key check is the bound and not a courtesy: this is the only verb on
// this channel that deletes a server outright, and a name that belongs to a
// lobby has to fail rather than work.
func (w KubeWriter) StopServer(ctx context.Context, namespace, name string) error {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNoSuchServer
		}
		return err
	}
	if srv.Spec.Key == "" {
		return ErrNotAnInstance
	}
	if err := w.Client.Delete(ctx, &srv); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// isTerminal is whether a member's run is over: its object may be replaced
// and it counts against no ceiling.
func isTerminal(p string) bool {
	return p == string(phase.Failed) || p == string(phase.Finished)
}
```

- [ ] **Step 9: Add the two verbs and the dispatch**

In `internal/agentserver/requests.go`, two cases in `answerCloudRequest`,
before the default:

```go
	case req.GetStartServer() != nil:
		return s.answerStartServer(ctx, logger, id, req.GetId(), req.GetStartServer())
	case req.GetStopServer() != nil:
		return s.answerStopServer(ctx, logger, id, req.GetId(), req.GetStopServer())
```

And the verbs, in the shape `answerRetire` and `answerBoost` have — each
refusal saying which one it was, because a caller told only "refused" asks
again:

```go
// answerStartServer creates the member of an on-demand group that carries a
// key.
//
// The namespace bound is structural, as it is for retire and boost: the group
// is resolved under id.Namespace and the request has no field that could name
// another network's.
//
// Asking for a key that is already running is answered and not refused, which
// is the one place this verb differs from every other writing verb here. The
// difference is in who asks: retire and boost are typed by an admin, for whom
// "somebody already did this" is news, while this is called by a plugin
// reacting to a player pressing a button twice.
func (s *Server) answerStartServer(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.StartServerRequest,
) *agentpb.CloudResponse {
	member, err := s.opts.Writer.StartServer(ctx, id.Namespace, req.GetGroup(), req.GetKey())
	switch {
	case errors.Is(err, ErrNoSuchGroup):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no group by that name is on this network")
	case errors.Is(err, ErrGroupNotOnDemand):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group's servers are counted rather than named, so it has no member to ask for")
	case errors.Is(err, ErrTooManyInstances):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group is at spec.maxInstances")
	case errors.Is(err, instance.ErrBadKey):
		return refuse(reqID, agentpb.RequestError_REFUSED, err.Error())
	case err != nil:
		logger.V(1).Info("could not start an on-demand server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	}

	return startedServer(reqID, &agentpb.StartServerResult{
		Server:         member.Name,
		AlreadyRunning: member.AlreadyRunning,
	})
}
```

`startedServer` and `stoppedServer` are two new response helpers beside the
existing `refuse` and `retired` in this package — one line each, wrapping the
result in a `CloudResponse` with the request's id, exactly as `retired` does.
Write them first; the verbs below will not compile without them.

```go
// answerStopServer deletes one member.
//
// The refusal for a server that no key names is the bound that matters here.
// This is the only request on this channel that deletes a server outright,
// and a mistyped name that happened to be a lobby's would otherwise take the
// lobby down with every player on it.
func (s *Server) answerStopServer(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.StopServerRequest,
) *agentpb.CloudResponse {
	err := s.opts.Writer.StopServer(ctx, id.Namespace, req.GetServer())
	switch {
	case errors.Is(err, ErrNoSuchServer):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no server by that name is on this network")
	case errors.Is(err, ErrNotAnInstance):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that server is not a member of an on-demand group")
	case err != nil:
		logger.V(1).Info("could not stop an on-demand server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	}

	return stoppedServer(reqID, &agentpb.StopServerResult{Server: req.GetServer()})
}
```

- [ ] **Step 10: Run the package**

Run: `nix develop -c go test ./internal/agentserver/ -count=1`
Expected: PASS. Every other `ClusterWriter` implementation in the tests gains
the two methods; a test double that does not compile is the compiler telling
you where the fakes are.

- [ ] **Step 11: Commit**

```bash
git add internal/agentserver internal/instance
git commit -m "feat(agentserver): start and stop a server asked for by name"
```

---

### Task 7: The picture splits by audience

**Files:**
- Modify: `internal/netstate/netstate.go` (`Source.Build`, `serverGroupKind`)
- Modify: the two callers, `internal/proxyreg` and `internal/serverreg`
- Test: `internal/netstate/netstate_test.go`

**Interfaces:**
- Consumes: Tasks 1 and 5.
- Produces: `Source.Build(ctx, namespace, audience)` with
  `netstate.ForProxies` and `netstate.ForServers`; on-demand members and their
  group appear only in the proxies' picture; `GroupState_ON_DEMAND` is
  reported for such a group.

- [ ] **Step 1: Write the failing test**

```go
func TestOnDemandMembersReachProxiesOnly(t *testing.T) {
	src := testSource(t,
		onDemandGroup("private-servers"),
		serverOf("private-servers", "private-servers-c0ffee", withKey("c0ffee")),
		ephemeralGroup("lobby"),
		serverOf("lobby", "lobby-abc"),
	)

	forProxies, err := src.Build(ctx, ns, netstate.ForProxies)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !hasServer(forProxies, "private-servers-c0ffee") {
		t.Error("a proxy cannot route to a private server it cannot see")
	}
	if kindOf(forProxies, "private-servers") != agentpb.GroupState_ON_DEMAND {
		t.Error("the group reports a kind this agent cannot read")
	}

	forServers, err := src.Build(ctx, ns, netstate.ForServers)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if hasServer(forServers, "private-servers-c0ffee") {
		t.Error("every lobby is carrying an entry for a server nobody will be sent to")
	}
	if hasGroup(forServers, "private-servers") {
		t.Error("the on-demand group itself reached a backend's picture")
	}
	if !hasServer(forServers, "lobby-abc") {
		t.Error("an ordinary server fell out of the backends' picture")
	}
}
```

Build the helpers on whatever `netstate_test.go` already uses to construct a
`Source` and assert over a `NetworkState`; do not add a second style.

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c go test ./internal/netstate/ -count=1`
Expected: compile failure — `Build` takes two arguments.

- [ ] **Step 3: Give Build an audience**

```go
// Audience is which kind of agent a picture is for.
//
// One picture per namespace was true until on-demand groups: a network with
// three hundred private servers running would hand every lobby three hundred
// entries, and a fresh picture on every start and stop, for servers no lobby
// will ever send anyone to. Proxies need them -- that is where routing is --
// and backends do not.
//
// The split is deliberately by audience and not by a flag on the group: a
// backend that could opt into seeing them would be a backend whose plugins
// start reading them, and then the entries are load-bearing everywhere.
type Audience int

const (
	// ForProxies is the whole namespace.
	ForProxies Audience = iota
	// ForServers leaves out on-demand groups and their members.
	ForServers
)
```

In `Build`, skip an on-demand group when the audience is `ForServers`, and
skip a server whose `spec.key` is set. In `serverGroupKind`, add the case:

```go
	case spawneryv1alpha1.ServerGroupOnDemand:
		return agentpb.GroupState_ON_DEMAND
```

Then update the two callers to pass their own audience:
`internal/proxyreg` passes `netstate.ForProxies`, `internal/serverreg` passes
`netstate.ForServers`.

- [ ] **Step 4: Run the affected packages**

Run: `nix develop -c go test ./internal/netstate/ ./internal/proxyreg/ ./internal/serverreg/ ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/netstate internal/proxyreg internal/serverreg
git commit -m "feat(netstate): private servers reach the proxies' picture only"
```

---

### Task 8: The Java API

**Files:**
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/StartedServer.java`
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudConnector.kt`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/MirrorApi.kt`
- Test: the JUnit suite under `agent/api/src/test/java/cloud/spawnery/agent/api/`
  and whatever `agent/common` tests already cover `CloudConnector`'s decoding

**Interfaces:**
- Consumes: Task 5's generated Java protobuf.
- Produces: `SpawneryApi.startServer(String group, String key)` returning
  `CompletionStage<StartedServer>` and `SpawneryApi.stopServer(String server)`
  returning `CompletionStage<Void>`; the record
  `StartedServer(String name, boolean alreadyRunning)`.

- [ ] **Step 1: Write the failing decoding test**

In the `agent/common` test source set, beside the existing `CloudConnector`
decoding tests:

```kotlin
@Test
fun `a start answer completes with the composed name`() {
    val connector = testConnector()
    val future = connector.startServer("private-servers", "c0ffee")

    connector.onResponse(
        CloudResponse.newBuilder()
            .setId(1)
            .setStartServer(
                StartServerResult.newBuilder()
                    .setServer("private-servers-c0ffee")
                    .setAlreadyRunning(true),
            )
            .build(),
    )

    val started = future.toCompletableFuture().get(1, TimeUnit.SECONDS)
    assertEquals("private-servers-c0ffee", started.name())
    assertTrue(started.alreadyRunning())
}
```

Use the same construction the neighbouring decoding tests use to make a
connector and feed it a response; `onResponse` above stands for whatever they
call.

- [ ] **Step 2: Run it and watch it fail**

Run: `nix develop -c make agent`
Expected: compile failure in the test source set — `startServer` does not
exist.

- [ ] **Step 3: Add the record, the two methods and the plumbing**

`StartedServer.java`, in the style of `BoostResult.java` (copy its header):

```java
/**
 * The private server a {@link SpawneryApi#startServer} produced.
 *
 * @param name the server's name, composed by the operator from the group and
 *     the key — this is what {@link SpawneryApi#servers()} calls it and what
 *     {@link SpawneryApi#stopServer} takes
 * @param alreadyRunning whether it was already there, which is a success and
 *     not a refusal: what you asked for is the case
 */
public record StartedServer(String name, boolean alreadyRunning) {}
```

On `SpawneryApi`, beside `boost`:

```java
    /**
     * Asks for the private server that carries this key.
     *
     * The key names a world and not a server: the operator composes the
     * server's name from the group and the key, and the same key later finds
     * the same world. Only a group of type {@code OnDemand} has members to
     * ask for; anything else fails.
     *
     * Asking for one that is already running succeeds with {@link
     * StartedServer#alreadyRunning()} set, so a player pressing a button
     * twice needs no lock on your side. It fails with the operator's own
     * sentence for a key that cannot be part of a name, for a group at its
     * {@code spec.maxInstances}, and for a group that is not on-demand.
     */
    CompletionStage<StartedServer> startServer(String group, String key);

    /**
     * Stops one private server and leaves its world where it is.
     *
     * Not {@link #retire}: retiring closes a server's door and lets it empty
     * in its own time, while this says its owner is done with it — the
     * players on it are moved through the proxies and the pod goes. The world
     * is on a claim nothing deletes, so the next {@link #startServer} of the
     * same key finds it.
     *
     * It fails for a server that is not a member of an on-demand group, which
     * is what keeps a wrong name from taking down a lobby.
     */
    CompletionStage<Void> stopServer(String server);
```

In `CloudConnector.kt`, beside `boost`:

```kotlin
    fun startServer(group: String, key: String): CompletionStage<StartedServer> =
        requests.start<StartedServer> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setStartServer(
                        StartServerRequest.newBuilder()
                            .setGroup(group)
                            .setKey(key),
                    )
                    .build(),
            )
        }

    fun stopServer(server: String): CompletionStage<Void> =
        requests.start<Void> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setStopServer(StopServerRequest.newBuilder().setServer(server))
                    .build(),
            )
        }
```

In the response branch of `CloudConnector.kt` (around line 226, beside
`response.hasBoost()`):

```kotlin
            response.hasStartServer() -> requests.complete(
                response.id,
                StartedServer(
                    response.startServer.server,
                    response.startServer.alreadyRunning,
                ),
            )
            response.hasStopServer() -> requests.complete(response.id, null)
```

And the two delegations in `MirrorApi.kt`, which asks nothing about which side
it is on:

```kotlin
    override fun startServer(group: String, key: String): CompletionStage<StartedServer> =
        connector.startServer(group, key)

    override fun stopServer(server: String): CompletionStage<Void> =
        connector.stopServer(server)
```

- [ ] **Step 4: Build the agents**

Run: `nix develop -c make agent`
Expected: both plugins and their JUnit suites build and pass.

Nix builds read the git index: `git add` the new `StartedServer.java` before
running this, or the build fails on a symbol that is plainly in the file.

- [ ] **Step 5: Commit**

```bash
git add agent
git commit -m "feat(agent): startServer and stopServer on the plugin API"
```

---

### Task 9: Documentation, the sample, and the numbers

**Files:**
- Create: `docs/guides/on-demand-servers.md`, `config/samples/ondemand.yaml`
- Modify: `docs/plugin-api/what-a-plugin-can-do.md` ("Changing the fleet"),
  `docs/index.md` (the four-kinds table), `mkdocs.yml` (nav), `CLAUDE.md`
  (the "one identical picture" sentence), `flake.nix`
  (`operatorVersion`, `imageVersion`), `charts/spawnery/Chart.yaml`
  (`version`, `appVersion`)

**Interfaces:**
- Consumes: everything above.
- Produces: nothing code depends on.

- [ ] **Step 1: Write the guide**

`docs/guides/on-demand-servers.md`, in this repository's house style — show
before explain, measurements rather than assumptions. It carries: the group
in full as YAML; that a member is `<group>-<key>` and its world
`<group>-<key>-data`; the two plugin calls with a short Java example; that
`maxInstances` is a fleet ceiling and not a per-player quota; that stopping
leaves the world and that nothing in this operator ever deletes a claim, with
a pointer to `persistent-worlds.md` for what that costs; and that members are
not in a backend's network picture.

- [ ] **Step 2: Write the sample**

`config/samples/ondemand.yaml`, joining the `Network` of
`config/samples/network.yaml`, in the shape `persistent-worlds.md` uses:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: private-servers
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: OnDemand
  image: ghcr.io/spawnery/purpur:26.2-0.4.0
  maxPlayers: 10
  # A fleet ceiling, not a per-player quota: who may have one is a question
  # about a player, and the system that knows the player answers it.
  maxInstances: 200
  storage:
    size: 2Gi
```

- [ ] **Step 3: Add the two calls to the plugin-API page**

Under "Changing the fleet" in `docs/plugin-api/what-a-plugin-can-do.md`, which
currently opens "Two calls write". Make it four and keep the sentence true.
Say plainly that `stopServer` deletes a server and that the key check is what
bounds it.

- [ ] **Step 4: Correct CLAUDE.md**

The architecture section says both agent kinds see one identical network
picture. Replace that clause with what is now true: both build their view
through `internal/netstate`, which serves one picture per audience — on-demand
members reach proxies only.

Add `OnDemand` to the CRD list at the top of the file, which names the four
kinds and their modes.

- [ ] **Step 5: Move the numbers**

A minor step in `flake.nix` for both `operatorVersion` and `imageVersion` (the
agent jar changed), and in `charts/spawnery/Chart.yaml` for `version` and
`appVersion`. Then:

Run: `nix develop -c make manifests`
Expected: `docs/reference/chart-values.md` moves with the chart's
`image.tag`; commit its diff with the bump or CI fails after the push.

- [ ] **Step 6: Run everything**

Run: `nix develop -c make test`
Then: `nix develop -c make lint`
Expected: PASS, including the linters that check that generated files are in
step with their sources.

- [ ] **Step 7: Commit**

```bash
git add docs mkdocs.yml CLAUDE.md config/samples flake.nix charts
git commit -m "docs(guides): private servers, and the numbers that carry them"
```

---

### Task 10: One private server, end to end

**Files:**
- Create: `test/e2e/ondemand_test.go` (build tag `e2e`)

**Interfaces:**
- Consumes: everything above.
- Produces: nothing.

This is the test that answers the question the unit tests cannot: whether a
player's server actually comes up. Everything below it proves that objects
were written.

- [ ] **Step 1: Write the test**

Following the existing files in `test/e2e/` for the cluster fixture and the
`//go:build e2e` tag: install the chart, apply a `Network` and an `OnDemand`
group, then

1. ask for `key=c0ffee` through a real agent session on a proxy pod,
2. wait for `private-servers-c0ffee` to reach phase `Ready` and for the claim
   `private-servers-c0ffee-data` to exist,
3. write a marker file into the world through the pod,
4. stop it, wait for the `Server` to be gone and assert the claim is still
   there,
5. ask for the same key again and assert the marker file is in the new pod.

Step 5 is the whole point: it is the only test in this repository that proves
a world survives a stop, and it is the promise the feature is for.

- [ ] **Step 2: Run it**

Run: `nix develop -c make e2e`
Expected: PASS. It takes minutes and builds a kind cluster. `E2E_KEEP=1`
keeps the cluster and prints its `KUBECONFIG` if it fails. Under rootless
Podman, the invocation is the one in `CLAUDE.md`.

- [ ] **Step 3: Commit**

```bash
git add test/e2e
git commit -m "test(e2e): a world survives the stop of its server"
```

---

## What this plan does not build

Named here so that nobody reaches the end and wonders.

- **The consumer side**, `orchestrator:spawnery` in `cyperia/private-server`,
  and the reset and deletion paths described in §4 of the spec. Its own plan,
  in its own repository.
- **A `/cloud` subcommand** for starting and stopping instances by hand. The
  `/cloud` command checks a permission and is for admins; nothing in the spec
  asks for it, and an admin has `kubectl`.
- **Per-player quotas**, for the reason `spec.maxInstances` gives in its own
  documentation.
