# /cloud scale, forcestop and execute Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Three administrator commands on the proxy: `/cloud scale <group> <count> [for <duration>]` and `/cloud scale <group> reset` pin an ephemeral group to an exact size (replacing `/cloud start` and `/cloud stop`); `/cloud forcestop <server>` kills a hung server's pod without a drain; `/cloud execute <server|group> <command>` runs a console command on one or every server of a group and shows the result.

**Architecture:** A pin is a `ScaleBoost` with `mode: Exact`; `internal/boost.Exact` picks the newest live one, and `ScalingInputs` reads it as both floor and ceiling. A force-stop is `Server.spec.forceStop`, which `phase.Decide` turns into `Terminating` from every phase and the server controller carries out with a zero grace period. Execute is the first operator message on the server stream that expects an answer: the operator sends `ExecuteCommand` down each target's stream, the Paper agent runs it on the main thread and answers `ExecuteOutcome`, and the operator collects for at most 8 s. The proxy session answers execute requests off its receive loop so the wait never stalls its outbox. Forcestop and execute are proxy-only on the operator and exist only in the Velocity command tree.

**Tech Stack:** Go (controller-runtime, kubebuilder markers, CEL, envtest), protobuf, Kotlin (Paper and Velocity agents, Brigadier, JUnit 5), Java (plugin API), Nix, kind.

**Spec:** `docs/superpowers/specs/2026-10-04-cloud-scale-forcestop-execute-design.md`

## Global Constraints

- Work in the worktree `/home/paul/git/spawnery-cloud`, branch `feat/cloud-commands`.
- Every build and test command runs in the dev shell, with the flake as an argument and never after a `cd`: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery-cloud -c <cmd>`, started from the worktree root. Below, `NIX` stands for `nix --extra-experimental-features 'nix-command flakes'`.
- To read code fast, `codegraph explore "<symbols or question>"` from `/home/paul/git/spawnery` (main checkout, same code as master before this branch) returns verbatim source. Fall back to grep and Read.
- Nix builds read the git index: `git add` every new or changed file before `NIX build .#agents` or any image build.
- Kotlin and Java tests run only through `NIX build /home/paul/git/spawnery-cloud#agents --no-link -L` (all JUnit suites); a failing test fails the build.
- Generated files are committed: after API or `.proto` changes run `NIX develop /home/paul/git/spawnery-cloud -c make manifests generate proto` and commit `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`, `internal/agentpb/`, `agent/common/src/proto/java/`, `docs/reference/crds.md`.
- Field numbers, exactly: `CloudRequest.scale = 13`, `.force_stop = 14`, `.execute = 15`; `CloudResponse.scale = 14`, `.force_stop = 15`, `.execute = 16`; `OperatorToServer.execute_command = 6`; `ServerMessage.execute_outcome = 6`; `GroupState.pinned = 13`, `.pinned_replicas = 14`, `.pinned_until_unix = 15`; `ExecuteOutcome`: `server = 1`, `ok = 2`, `output = 3`, `error = 4`, `id = 5`.
- Permission nodes exactly `spawnery.cloud.scale` (existing), `spawnery.cloud.forcestop`, `spawnery.cloud.execute`.
- Pin duration: default 1 hour, at most 7 days (`168h`), units `s`, `m`, `h`, `d`. Execute: command at most 256 characters without the leading `/`; output at most 20 lines of at most 256 characters; the operator waits at most 8 s (the agent gives up after 10 s, `CloudConnector.TIMEOUT_MILLIS`).
- Refusal text without the switch, exactly: `execute is not enabled on this network`. Event reasons exactly `ForceStopped` and `CommandExecuted`; a `CommandExecuted` event never carries output.
- `ScaleBoost.spec.mode` values exactly `Add` and `Exact`, default `Add`.
- Nothing here enters a pod spec: `internal/podspec/hash_golden_test.go` stays untouched (Network.spec.commands is read by the agent endpoint only).
- No version bump. 0.19.0 for all three numbers is a separate PR after this branch merges.
- Comments follow the user's rule: none unless a reader cannot derive it from the code beside it. Everything written is English.
- Commits are Conventional Commits with a scope (`feat(api): …`), body wrapped at 72 columns, ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Commits are gpg-signed; on `paul-desktop` the passphrase dialog opens as a window, just commit.

## Review Focus

1. **A pin outliving its group.** A `ServerGroup` deleted and created again under the same name must not be pinned by its predecessor's `Exact` boost, which lingers until the garbage collector reaches it (envtest has none, so it lingers forever there). Expected: boosts owned by another `ServerGroup` UID are ignored. Pinned in Task 4 (`boost.Of`) and Task 4's envtest.
2. **A target's session drops while execute waits.** One server of a group never answers (agent reconnecting, older agent ignoring `ExecuteCommand`). Expected: the others' outcomes arrive, that one reads `no answer within 8s`, and the proxy is answered before its own 10 s timeout. Pinned in Task 6.
3. **Output carrying `§` colour codes or MiniMessage tags.** Expected: `§x` codes are stripped on Paper; `<click:…>` and other tags in output reach the admin's chat as literal text, never as markup. Pinned in Task 7 (strip) and Task 9 (escape).
4. **A forcestop on a server whose pod is already terminating** with its ordinary grace period. Expected: answered as success, and the pod is deleted again with a grace period of 0, which shortens the running one. Pinned in Task 5.
5. **Durations at the edges:** `0d` and `0m` (unreadable, named in the reply, nothing sent), `8d` (sent, refused by the operator naming the 7-day limit), an overflowing `99999999999999999d` (unreadable, no exception into Brigadier), and a raw `duration_seconds` so large that multiplying by `time.Second` overflows (refused, not wrapped to the default). Pinned in Task 6 and Task 9.

---

### Task 1: The API fields

**Files:**
- Modify: `api/v1alpha1/scaleboost_types.go` (spec type, printcolumns)
- Modify: `api/v1alpha1/server_types.go` (`ServerSpec`, after `Hold` at line 80)
- Modify: `api/v1alpha1/network_types.go` (`NetworkSpec` after `Update` at line 47; new type and method after `ChangeoverBudget`)
- Modify: `api/v1alpha1/servergroup_types.go` (`ServerGroupStatus`, after `BoostedReplicas` at line 445)
- Modify: `api/v1alpha1/common_types.go` (reason constant after `ReasonMaxReplicasReached`, line 108)
- Test: `api/v1alpha1/scaleboost_envtest_test.go`, `api/v1alpha1/network_types_test.go`, `api/v1alpha1/network_envtest_test.go`, `api/v1alpha1/server_envtest_test.go`, `api/v1alpha1/servergroup_envtest_test.go`
- Regenerate: CRDs, chart CRDs, deepcopy, `docs/reference/crds.md`

**Interfaces:**
- Produces: `type ScaleBoostMode string`; constants `ScaleBoostAdd = "Add"`, `ScaleBoostExact = "Exact"`; field `ScaleBoostSpec.Mode ScaleBoostMode`; field `ServerSpec.ForceStop bool`; `type NetworkCommands struct { Execute bool }`; field `NetworkSpec.Commands *NetworkCommands`; method `func (n *Network) ExecuteEnabled() bool`; fields `ServerGroupStatus.PinnedReplicas *int32`, `ServerGroupStatus.PinnedUntil *metav1.Time`; constant `ReasonPinned = "Pinned"`.

- [ ] **Step 1: Write the failing unit test** in `api/v1alpha1/network_types_test.go` (package `v1alpha1`, `ptr` already imported):

```go
func TestExecuteEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		net  *Network
		want bool
	}{
		{name: "no commands", net: &Network{}, want: false},
		{name: "commands without execute", net: &Network{Spec: NetworkSpec{Commands: &NetworkCommands{}}}, want: false},
		{name: "execute on", net: &Network{Spec: NetworkSpec{Commands: &NetworkCommands{Execute: true}}}, want: true},
	} {
		if got := tc.net.ExecuteEnabled(); got != tc.want {
			t.Errorf("%s: ExecuteEnabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./api/v1alpha1/ -run TestExecuteEnabled -count=1`
Expected: build failure, `undefined: NetworkCommands`.

- [ ] **Step 3: Add the types and fields.**

`api/v1alpha1/scaleboost_types.go`: replace `ScaleBoostSpec` and the printcolumns above `ScaleBoost`:

```go
// +kubebuilder:validation:Enum=Add;Exact
type ScaleBoostMode string

const (
	ScaleBoostAdd   ScaleBoostMode = "Add"
	ScaleBoostExact ScaleBoostMode = "Exact"
)

// +kubebuilder:validation:XValidation:rule="(has(self.mode) && self.mode == 'Exact') || self.replicas >= 1",message="an Add boost has to add at least one server"
type ScaleBoostSpec struct {
	// GroupRef names the ServerGroup this applies to.
	GroupRef ObjectRef `json:"groupRef"`
	// Mode Add raises the group's floor by Replicas; boosts on one group add
	// up. Mode Exact holds the group at exactly Replicas servers, 0 included:
	// the group's floor and ceiling both become that number, Add boosts stop
	// counting, and of several Exact boosts the newest wins. Neither lifts
	// spec.scaling.maxReplicas.
	// +kubebuilder:default=Add
	// +optional
	Mode ScaleBoostMode `json:"mode,omitempty"`
	// Replicas is how many servers to add (Add, at least 1) or to hold the
	// group at (Exact, at least 0).
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
	// ExpiresAt is when this boost stops counting. A boost without one never
	// expires; the tools that create boosts supply a default.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}
```

Add `// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`` directly after the `Group` printcolumn.

`api/v1alpha1/server_types.go`, after `Hold`:

```go
	// ForceStop kills this server's pod at once, with no drain and a grace
	// period of zero: its players lose their connection and the world loses
	// whatever it had not saved. Set by the agent endpoint's force-stop
	// request, which only a proxy may send.
	// +optional
	ForceStop bool `json:"forceStop,omitempty"`
```

`api/v1alpha1/network_types.go`, in `NetworkSpec` after `Update`:

```go
	// Commands opens in-game commands that reach past the operator's own
	// objects. Absent, all of them are off.
	// +optional
	Commands *NetworkCommands `json:"commands,omitempty"`
```

and after `ChangeoverBudget`:

```go
// NetworkCommands switches on the /cloud verbs that run code on a server.
type NetworkCommands struct {
	// Execute lets an administrator on a proxy run a console command on one
	// server or on every server of a group with /cloud execute. Off, the
	// operator refuses every such request.
	// +optional
	Execute bool `json:"execute,omitempty"`
}

func (n *Network) ExecuteEnabled() bool {
	return n.Spec.Commands != nil && n.Spec.Commands.Execute
}
```

`api/v1alpha1/servergroup_types.go`, after `BoostedReplicas`:

```go
	// PinnedReplicas is the size an Exact ScaleBoost holds this group at,
	// which can be 0. Absent while no pin holds.
	// +optional
	PinnedReplicas *int32 `json:"pinnedReplicas,omitempty"`

	// PinnedUntil is when that pin ends. Absent without a pin, and for a pin
	// without an end.
	// +optional
	PinnedUntil *metav1.Time `json:"pinnedUntil,omitempty"`
```

`api/v1alpha1/common_types.go`, after `ReasonMaxReplicasReached`:

```go
	ReasonPinned                 = "Pinned"
```

(Align with `gofmt`.)

- [ ] **Step 4: Regenerate and run the unit test**

Run: `NIX develop /home/paul/git/spawnery-cloud -c make manifests generate`
Then: `NIX develop /home/paul/git/spawnery-cloud -c go test ./api/v1alpha1/ -run TestExecuteEnabled -count=1`
Expected: PASS. `git diff --stat` lists the four CRD YAMLs under `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `api/v1alpha1/zz_generated.deepcopy.go`, `docs/reference/crds.md`. Open `config/crd/bases/spawnery.cloud_scaleboosts.yaml` and check the `x-kubernetes-validations` rule sits on `spec`, `minimum: 0` on `replicas` and `default: Add` on `mode`.

- [ ] **Step 5: Write the envtests.**

`api/v1alpha1/scaleboost_envtest_test.go`, appended:

```go
func TestAnExactScaleBoostOfZeroIsAccepted(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	if err := c.Create(ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "off", Namespace: ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
			Mode:     spawneryv1alpha1.ScaleBoostExact,
			Replicas: 0,
		},
	}); err != nil {
		t.Fatalf("a pin to zero was refused: %v", err)
	}
}

func TestAScaleBoostBelowZeroIsRefusedInEitherMode(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	for _, mode := range []spawneryv1alpha1.ScaleBoostMode{spawneryv1alpha1.ScaleBoostAdd, spawneryv1alpha1.ScaleBoostExact} {
		if err := c.Create(ctx, &spawneryv1alpha1.ScaleBoost{
			ObjectMeta: metav1.ObjectMeta{Name: "negative-" + strings.ToLower(string(mode)), Namespace: ns},
			Spec: spawneryv1alpha1.ScaleBoostSpec{
				GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
				Mode:     mode,
				Replicas: -1,
			},
		}); err == nil {
			t.Errorf("a %s boost of -1 was accepted", mode)
		}
	}
}

func TestAScaleBoostWithoutAModeAdds(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	if err := c.Create(ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
			Replicas: 1,
		},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &spawneryv1alpha1.ScaleBoost{}
	if err := c.Get(ctx, types.NamespacedName{Name: "plain", Namespace: ns}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Mode != spawneryv1alpha1.ScaleBoostAdd {
		t.Errorf("mode = %q, want Add: every boost created before the field means Add", got.Spec.Mode)
	}
}
```

Add `"strings"` to the imports. `TestAScaleBoostOfZeroReplicasIsRefused` (an Add boost of 0) stays as it is and must still pass.

`api/v1alpha1/network_envtest_test.go`, appended:

```go
func TestNetworkCarriesTheExecuteSwitch(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	if err := c.Create(ctx, &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
			Commands:            &spawneryv1alpha1.NetworkCommands{Execute: true},
		},
	}); err != nil {
		t.Fatalf("create Network: %v", err)
	}
	got := &spawneryv1alpha1.Network{}
	if err := c.Get(ctx, types.NamespacedName{Name: "production", Namespace: ns}, got); err != nil {
		t.Fatalf("get Network: %v", err)
	}
	if !got.ExecuteEnabled() {
		t.Error("spec.commands.execute did not survive a write and a read")
	}
}
```

`api/v1alpha1/server_envtest_test.go`, appended:

```go
func TestServerCarriesForceStop(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)

	if err := c.Create(ctx, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-x7k2", Namespace: ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: "lobby"},
			ForceStop: true,
		},
	}); err != nil {
		t.Fatalf("create Server: %v", err)
	}
	got := &spawneryv1alpha1.Server{}
	if err := c.Get(ctx, types.NamespacedName{Name: "lobby-x7k2", Namespace: ns}, got); err != nil {
		t.Fatalf("get Server: %v", err)
	}
	if !got.Spec.ForceStop {
		t.Error("spec.forceStop did not survive a write and a read")
	}
}
```

`api/v1alpha1/servergroup_envtest_test.go`, appended (`ptr` is already imported there; check and add `"time"` if missing):

```go
// Zero is a pin and absent is none; omitempty on a plain int32 would lose the difference.
func TestAPinToZeroSurvivesTheStatusWrite(t *testing.T) {
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	g := ephemeralGroup(ns, "lobby")
	if err := c.Create(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}
	until := metav1.NewTime(time.Now().Add(time.Hour).Truncate(time.Second))
	g.Status.PinnedReplicas = ptr.To[int32](0)
	g.Status.PinnedUntil = &until
	if err := c.Status().Update(ctx, g); err != nil {
		t.Fatalf("status update: %v", err)
	}
	got := &spawneryv1alpha1.ServerGroup{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(g), got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.PinnedReplicas == nil || *got.Status.PinnedReplicas != 0 {
		t.Errorf("pinnedReplicas = %v, want a present 0", got.Status.PinnedReplicas)
	}
	if got.Status.PinnedUntil == nil || !got.Status.PinnedUntil.Time.Equal(until.Time) {
		t.Errorf("pinnedUntil = %v, want %v", got.Status.PinnedUntil, until)
	}
}
```

- [ ] **Step 6: Run the package**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./api/v1alpha1/ -count=1`
Expected: PASS, including the existing `TestAScaleBoostOfZeroReplicasIsRefused`.

- [ ] **Step 7: Commit**

```bash
git add api config charts docs/reference
git commit -m "feat(api): ScaleBoost mode, spec.forceStop and spec.commands.execute" \
  -m "<body: the three fields and the two status fields, the CEL rule that keeps an Add boost at one or more, Add as the default so every existing boost keeps its meaning>"
```

---

### Task 2: The wire

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto` (oneofs at lines 97-109, 117-130, 462-468, 472-478; `GroupState` ends at line 586; new messages after `StopBoostResult` at line 283)
- Test: `internal/agentpb/contract_test.go`, `agent/common/src/test/kotlin/cloud/spawnery/agent/ContractTest.kt`
- Regenerate: `internal/agentpb/`, `agent/common/src/proto/java/`

**Interfaces:**
- Produces (Go, `internal/agentpb`): `ScaleRequest{Group, Replicas, DurationSeconds}`, `ScaleResult{Replicas, ExpiresAtUnix}`, `ForceStopRequest{Server, Issuer}`, `ForceStopResult{Server}`, `ExecuteRequest{Target, Command, Issuer}`, `ExecuteResult{Outcomes}`, `ExecuteOutcome{Server, Ok, Output, Error, Id}`, `ExecuteCommand{Id, Command}`; oneof wrappers `CloudRequest_Scale`, `CloudRequest_ForceStop`, `CloudRequest_Execute`, `CloudResponse_Scale`, `CloudResponse_ForceStop`, `CloudResponse_Execute`, `OperatorToServer_ExecuteCommand`, `ServerMessage_ExecuteOutcome`; `GroupState.Pinned`, `.PinnedReplicas`, `.PinnedUntilUnix`.
- Produces (Java, `cloud.spawnery.agent.pb`): the same messages; `OperatorToServer.MessageCase.EXECUTE_COMMAND`, `ServerMessage.Builder.setExecuteOutcome`, `CloudResponse.hasScale()/hasForceStop()/hasExecute()`.

- [ ] **Step 1: Write the failing contract test** in `internal/agentpb/contract_test.go`:

```go
func TestCloudCommandFieldNumbersAreFixed(t *testing.T) {
	for _, c := range []struct {
		msg   proto.Message
		field protoreflect.Name
		want  protoreflect.FieldNumber
	}{
		{&agentpb.CloudRequest{}, "scale", 13},
		{&agentpb.CloudRequest{}, "force_stop", 14},
		{&agentpb.CloudRequest{}, "execute", 15},
		{&agentpb.CloudResponse{}, "scale", 14},
		{&agentpb.CloudResponse{}, "force_stop", 15},
		{&agentpb.CloudResponse{}, "execute", 16},
		{&agentpb.OperatorToServer{}, "execute_command", 6},
		{&agentpb.ServerMessage{}, "execute_outcome", 6},
		{&agentpb.GroupState{}, "pinned", 13},
		{&agentpb.GroupState{}, "pinned_replicas", 14},
		{&agentpb.GroupState{}, "pinned_until_unix", 15},
		{&agentpb.ScaleRequest{}, "group", 1},
		{&agentpb.ScaleRequest{}, "replicas", 2},
		{&agentpb.ScaleRequest{}, "duration_seconds", 3},
		{&agentpb.ScaleResult{}, "replicas", 1},
		{&agentpb.ScaleResult{}, "expires_at_unix", 2},
		{&agentpb.ForceStopRequest{}, "server", 1},
		{&agentpb.ForceStopRequest{}, "issuer", 2},
		{&agentpb.ForceStopResult{}, "server", 1},
		{&agentpb.ExecuteRequest{}, "target", 1},
		{&agentpb.ExecuteRequest{}, "command", 2},
		{&agentpb.ExecuteRequest{}, "issuer", 3},
		{&agentpb.ExecuteResult{}, "outcomes", 1},
		{&agentpb.ExecuteOutcome{}, "server", 1},
		{&agentpb.ExecuteOutcome{}, "ok", 2},
		{&agentpb.ExecuteOutcome{}, "output", 3},
		{&agentpb.ExecuteOutcome{}, "error", 4},
		{&agentpb.ExecuteOutcome{}, "id", 5},
		{&agentpb.ExecuteCommand{}, "id", 1},
		{&agentpb.ExecuteCommand{}, "command", 2},
	} {
		md := c.msg.ProtoReflect().Descriptor()
		fd := md.Fields().ByName(c.field)
		if fd == nil {
			t.Errorf("%s has no field %s", md.Name(), c.field)
			continue
		}
		if fd.Number() != c.want {
			t.Errorf("%s.%s is field %d, want %d: a renumbered field is a silent wire break",
				md.Name(), c.field, fd.Number(), c.want)
		}
	}
}
```

- [ ] **Step 2: Run it and see it fail**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/agentpb/ -run TestCloudCommandFieldNumbersAreFixed -count=1`
Expected: build failure, `undefined: agentpb.ScaleRequest`.

- [ ] **Step 3: Extend the `.proto`.** Oneof arms:

```proto
    DeleteServerRequest delete_server = 12;
    ScaleRequest scale = 13;
    ForceStopRequest force_stop = 14;
    ExecuteRequest execute = 15;
```

```proto
    DeleteServerResult delete_server = 13;
    ScaleResult scale = 14;
    ForceStopResult force_stop = 15;
    ExecuteResult execute = 16;
```

In `ServerMessage`: `ExecuteOutcome execute_outcome = 6;`. In `OperatorToServer`: `ExecuteCommand execute_command = 6;`.

At the end of `GroupState`:

```proto
  // An Exact ScaleBoost holds the group at pinned_replicas servers, which can
  // be 0. Without a pin both fields below are zero and mean nothing.
  bool pinned = 13;
  int32 pinned_replicas = 14;
  // When the pin ends, on the operator's clock; 0 for a pin without an end.
  int64 pinned_until_unix = 15;
```

New messages after `StopBoostResult`:

```proto
// ScaleRequest holds an ephemeral group at exactly `replicas` servers for a
// while, 0 included. Its own message and not a field on BoostRequest: an
// operator older than this one would ignore the field and read "exactly 5" as
// "5 more", while it refuses a request it does not know. The reset is
// StopBoostRequest.
message ScaleRequest {
  string group = 1;
  int32 replicas = 2;
  // Zero means the operator's default of an hour; it refuses more than seven
  // days.
  int64 duration_seconds = 3;
}

// ScaleResult is the pin that now holds.
message ScaleResult {
  int32 replicas = 1;
  int64 expires_at_unix = 2;
}

// ForceStopRequest kills one server's pod without a drain. Refused from a
// backend: it can lose unsaved world data, and one compromised server must not
// be able to do that to its neighbours.
message ForceStopRequest {
  string server = 1;
  // Who typed it, for the operator's record only.
  string issuer = 2;
}

message ForceStopResult {
  string server = 1;
}

// ExecuteRequest runs one console command on a server, or on every Ready
// server of a group whose agent is connected. Refused from a backend and on a
// network without spec.commands.execute.
message ExecuteRequest {
  // A server name, else a group name.
  string target = 1;
  // Without the leading slash, at most 256 characters.
  string command = 2;
  string issuer = 3;
}

// ExecuteResult has one outcome per server the command reached, sorted by
// server. A server that did not answer in time is listed with an error.
message ExecuteResult {
  repeated ExecuteOutcome outcomes = 1;
}

// ExecuteOutcome is one server's answer. On the server stream it carries the
// id of the ExecuteCommand it answers and no server name: the operator knows
// whom it asked.
message ExecuteOutcome {
  string server = 1;
  bool ok = 2;
  // The command's feedback as plain text: at most 20 lines of 256 characters.
  repeated string output = 3;
  string error = 4;
  uint64 id = 5;
}
```

In the server-direction section, before `ServerMessage`:

```proto
// ExecuteCommand asks this server to run one console command and answer with
// an ExecuteOutcome carrying the same id. An agent that predates it ignores
// it, and the operator lists that server as not answering.
message ExecuteCommand {
  uint64 id = 1;
  string command = 2;
}
```

- [ ] **Step 4: Regenerate and run**

Run: `NIX develop /home/paul/git/spawnery-cloud -c make proto`
Then: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/agentpb/ -count=1`
Expected: PASS.

- [ ] **Step 5: Kotlin round trip** in `ContractTest.kt` (add imports `cloud.spawnery.agent.pb.ExecuteOutcome`, `cloud.spawnery.agent.pb.ExecuteCommand`, `cloud.spawnery.agent.pb.OperatorToServer`):

```kotlin
    @Test
    fun `an execute command and its outcome round-trip with their id`() {
        val down = OperatorToServer.newBuilder()
            .setExecuteCommand(ExecuteCommand.newBuilder().setId(7).setCommand("list"))
            .build()
        val downBack = OperatorToServer.parseFrom(down.toByteArray())
        assertEquals(OperatorToServer.MessageCase.EXECUTE_COMMAND, downBack.messageCase)
        assertEquals(7L, downBack.executeCommand.id)

        val up = ServerMessage.newBuilder()
            .setExecuteOutcome(ExecuteOutcome.newBuilder().setId(7).setOk(true).addOutput("There are 0 players"))
            .build()
        val upBack = ServerMessage.parseFrom(up.toByteArray())
        assertEquals(7L, upBack.executeOutcome.id)
        assertEquals(listOf("There are 0 players"), upBack.executeOutcome.outputList)
    }
```

Run: `git add -A && NIX build /home/paul/git/spawnery-cloud#agents --no-link -L 2>&1 | tail -30`
Expected: build succeeds; the test is listed as passed.

- [ ] **Step 6: Commit**

```bash
git add proto internal/agentpb agent/common
git commit -m "feat(proto): scale, force-stop and execute on the agent channel" \
  -m "<body: the new requests and the first server-stream message that expects an answer; why scale is its own request and not a BoostRequest field>"
```

---

### Task 3: Pins in the sizing rule

**Files:**
- Modify: `internal/boost/boost.go`
- Modify: `internal/controller/scaling.go` (`ScalingInputs` lines 31-75, `floor()` line 79, `decideSize` lines 435, 455, 473, 492)
- Test: `internal/boost/boost_test.go`, `internal/controller/scaling_test.go`

**Interfaces:**
- Consumes: `spawneryv1alpha1.ScaleBoostExact` (Task 1).
- Produces: `type boost.Pin struct { Replicas int32; ExpiresAt *metav1.Time }`; `func boost.Exact(boosts []spawneryv1alpha1.ScaleBoost, group string, now time.Time) (boost.Pin, bool)`; `func boost.Of(boosts []spawneryv1alpha1.ScaleBoost, group *spawneryv1alpha1.ServerGroup) []spawneryv1alpha1.ScaleBoost`; `boost.Live` ignores Exact boosts; `ScalingInputs.Pinned bool`, `ScalingInputs.Pin int32`; `func (ScalingInputs) ceiling() int32`.

Spec §2.3 names `Exact(...) (int32, bool)`. The status needs the pin's end as well (§2.4), so `Exact` returns a `Pin` carrying both; the rule is the spec's.

- [ ] **Step 1: Write the failing boost tests** in `internal/boost/boost_test.go`. Add a helper and tests:

```go
func pinFor(group, name string, replicas int32, created time.Time, expires *time.Time) spawneryv1alpha1.ScaleBoost {
	b := boostFor(group, replicas, expires)
	b.Name = name
	b.Spec.Mode = spawneryv1alpha1.ScaleBoostExact
	b.CreationTimestamp = metav1.NewTime(created)
	return b
}

func TestTheNewestPinWins(t *testing.T) {
	now := time.Unix(10_000, 0)
	later := now.Add(time.Hour)

	got, ok := Exact([]spawneryv1alpha1.ScaleBoost{
		pinFor("lobby", "lobby-old", 5, now.Add(-time.Hour), &later),
		pinFor("lobby", "lobby-new", 0, now.Add(-time.Minute), &later),
	}, "lobby", now)

	if !ok || got.Replicas != 0 {
		t.Errorf("pin = %+v, %v, want the newer pin of 0: pins replace, they do not add up", got, ok)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Time.Equal(later) {
		t.Errorf("expiresAt = %v, want %v", got.ExpiresAt, later)
	}
}

func TestPinsCreatedInTheSameSecondFallToTheName(t *testing.T) {
	now := time.Unix(10_000, 0)
	at := now.Add(-time.Minute)

	got, _ := Exact([]spawneryv1alpha1.ScaleBoost{
		pinFor("lobby", "lobby-b", 2, at, nil),
		pinFor("lobby", "lobby-a", 7, at, nil),
	}, "lobby", now)

	if got.Replicas != 2 {
		t.Errorf("pin = %d, want lobby-b's 2: every reader has to pick the same one", got.Replicas)
	}
}

func TestAnExpiredPinIsNoPin(t *testing.T) {
	now := time.Unix(10_000, 0)
	past := now.Add(-time.Second)

	if _, ok := Exact([]spawneryv1alpha1.ScaleBoost{pinFor("lobby", "p", 0, now.Add(-time.Hour), &past)}, "lobby", now); ok {
		t.Error("an expired pin still holds")
	}
	if _, ok := Exact([]spawneryv1alpha1.ScaleBoost{pinFor("lobby", "p", 0, now.Add(-time.Hour), &now)}, "lobby", now); ok {
		t.Error("a pin expiring exactly now still holds")
	}
}

func TestAnAddBoostIsNoPinAndAPinAddsNothing(t *testing.T) {
	now := time.Unix(10_000, 0)
	later := now.Add(time.Hour)
	boosts := []spawneryv1alpha1.ScaleBoost{
		boostFor("lobby", 3, &later),
		pinFor("lobby", "p", 1, now.Add(-time.Minute), &later),
	}

	if got := Live(boosts, "lobby", now); got != 3 {
		t.Errorf("Live = %d, want 3: a pin is not extra capacity", got)
	}
	if _, ok := Exact([]spawneryv1alpha1.ScaleBoost{boostFor("lobby", 3, &later)}, "lobby", now); ok {
		t.Error("an Add boost read as a pin")
	}
}

func TestAnotherGroupsPinIsNotThisGroups(t *testing.T) {
	now := time.Unix(10_000, 0)
	if _, ok := Exact([]spawneryv1alpha1.ScaleBoost{pinFor("arena", "p", 0, now, nil)}, "lobby", now); ok {
		t.Error("a pin on arena held lobby")
	}
}

func TestABoostOwnedByAGroupThatIsGoneIsNotTheSuccessors(t *testing.T) {
	group := &spawneryv1alpha1.ServerGroup{ObjectMeta: metav1.ObjectMeta{Name: "lobby", UID: "new-uid"}}
	mine := pinFor("lobby", "mine", 2, time.Unix(1, 0), nil)
	mine.OwnerReferences = []metav1.OwnerReference{{Kind: "ServerGroup", Name: "lobby", UID: "new-uid"}}
	stale := pinFor("lobby", "stale", 0, time.Unix(2, 0), nil)
	stale.OwnerReferences = []metav1.OwnerReference{{Kind: "ServerGroup", Name: "lobby", UID: "old-uid"}}
	byHand := boostFor("lobby", 1, nil)

	got := Of([]spawneryv1alpha1.ScaleBoost{mine, stale, byHand, boostFor("arena", 1, nil)}, group)

	if len(got) != 2 || got[0].Name != "mine" || got[1].Spec.Mode != "" {
		t.Errorf("Of = %+v, want mine and the unowned boost: a predecessor's boost must not pin its successor", got)
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/boost/ -count=1`
Expected: build failure, `undefined: Exact`.

- [ ] **Step 3: Implement** in `internal/boost/boost.go` (add `metav1` import):

```go
// Live is how many extra servers a group's unexpired Add boosts ask for. A
// boost stops counting when it expires, not when the sweep removes it. Add
// boosts on one group add up; Exact boosts are Exact's business.
func Live(boosts []spawneryv1alpha1.ScaleBoost, group string, now time.Time) int32 {
	var total int32
	for i := range boosts {
		b := &boosts[i]
		if b.Spec.GroupRef.Name != group || b.Spec.Mode == spawneryv1alpha1.ScaleBoostExact || expired(b, now) {
			continue
		}
		total += b.Spec.Replicas
	}
	return total
}

// Pin is the size an Exact boost holds a group at.
type Pin struct {
	Replicas int32
	// ExpiresAt is nil for a pin without an end.
	ExpiresAt *metav1.Time
}

// Exact is the group's newest unexpired Exact boost: pins replace each other
// rather than adding up. A tie on the creation second falls to the name, so
// every reader picks the same one.
func Exact(boosts []spawneryv1alpha1.ScaleBoost, group string, now time.Time) (Pin, bool) {
	var newest *spawneryv1alpha1.ScaleBoost
	for i := range boosts {
		b := &boosts[i]
		if b.Spec.GroupRef.Name != group || b.Spec.Mode != spawneryv1alpha1.ScaleBoostExact || expired(b, now) {
			continue
		}
		if newest == nil || newer(b, newest) {
			newest = b
		}
	}
	if newest == nil {
		return Pin{}, false
	}
	return Pin{Replicas: newest.Spec.Replicas, ExpiresAt: newest.Spec.ExpiresAt}, true
}

// Of is the boosts that belong to group: those naming it, minus those owned
// by another ServerGroup of the same name. A group deleted and created again
// keeps its name, and its predecessor's boosts stay until the garbage
// collector reaches them.
func Of(boosts []spawneryv1alpha1.ScaleBoost, group *spawneryv1alpha1.ServerGroup) []spawneryv1alpha1.ScaleBoost {
	var mine []spawneryv1alpha1.ScaleBoost
	for i := range boosts {
		b := boosts[i]
		if b.Spec.GroupRef.Name != group.Name || ownedByAnother(&b, group) {
			continue
		}
		mine = append(mine, b)
	}
	return mine
}

func ownedByAnother(b *spawneryv1alpha1.ScaleBoost, group *spawneryv1alpha1.ServerGroup) bool {
	for _, ref := range b.OwnerReferences {
		if ref.Kind == "ServerGroup" && ref.UID != group.UID {
			return true
		}
	}
	return false
}

// Expiring exactly now has expired: "until 20:00" is over at 20:00.
func expired(b *spawneryv1alpha1.ScaleBoost, now time.Time) bool {
	return b.Spec.ExpiresAt != nil && !b.Spec.ExpiresAt.After(now)
}

func newer(a, b *spawneryv1alpha1.ScaleBoost) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return b.CreationTimestamp.Before(&a.CreationTimestamp)
	}
	return a.Name > b.Name
}
```

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/boost/ -count=1`
Expected: PASS, the five existing tests included.

- [ ] **Step 4: Write the failing scaling tests** in `internal/controller/scaling_test.go`, after `TestABoostAlsoHoldsCapacityAgainstAScaleDown`:

```go
func TestAPinIsTheFloor(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100,
		Pinned: true, Pin: 3,
		PodHash: "current",
	})
	if got.Create != 3 {
		t.Errorf("Create = %d, want 3: the pin", got.Create)
	}
}

func TestAPinIsTheCeilingAgainstDemand(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 400, MaxPlayers: 100,
		Pinned: true, Pin: 2,
		PodHash: "current",
	})
	if got.Create != 2 || !got.Limited {
		t.Errorf("Create = %d, Limited = %v; want 2 and limited: demand asks for 4", got.Create, got.Limited)
	}
}

func TestAPinOfZeroRemovesEveryServer(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{ready("a", 0, 100), ready("b", 5, 100), ready("c", 0, 100)},
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 40, MaxPlayers: 100,
		Pinned: true, Pin: 0,
		PodHash: "current",
	})
	if got.Surplus != 3 || len(got.Delete) != 3 {
		t.Errorf("Surplus = %d, Delete = %v; want all three gone", got.Surplus, got.Delete)
	}
}

func TestAPinBelowTheRunningCountTakesTheEmptiestFirst(t *testing.T) {
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{ready("a", 10, 100), ready("b", 0, 100), ready("c", 3, 100)},
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 0, MaxPlayers: 100,
		Pinned: true, Pin: 1,
		PodHash: "current",
	})
	slices.Sort(got.Delete)
	if !slices.Equal(got.Delete, []string{"b", "c"}) {
		t.Errorf("Delete = %v, want b and c: the busiest server stays", got.Delete)
	}
}

func TestAnAddBoostDoesNotCountWhileAPinHolds(t *testing.T) {
	got := DecideSize(ScalingInputs{
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 0, MaxPlayers: 100,
		Boost:  5,
		Pinned: true, Pin: 2,
		PodHash: "current",
	})
	if got.Create != 2 {
		t.Errorf("Create = %d, want 2: the pin, not the floor plus a boost", got.Create)
	}
}

func TestAPinAboveMaxReplicasStopsAtTheCeiling(t *testing.T) {
	// /cloud scale refuses this; a ScaleBoost written by hand can still say it.
	got := DecideSize(ScalingInputs{
		MinReplicas: 1, MaxReplicas: 3,
		SpareSlots: 0, MaxPlayers: 100,
		Pinned: true, Pin: 20,
		PodHash: "current",
	})
	if got.Create != 3 {
		t.Errorf("Create = %d, want maxReplicas 3", got.Create)
	}
}

func TestAPinOfZeroLeavesAHeldServer(t *testing.T) {
	held := ready("a", 0, 100)
	held.Hold = true
	got := DecideSize(ScalingInputs{
		Views:       []ServerView{held},
		MinReplicas: 1, MaxReplicas: 10,
		SpareSlots: 0, MaxPlayers: 100,
		Pinned: true, Pin: 0,
		PodHash: "current",
	})
	if len(got.Delete) != 0 {
		t.Errorf("Delete = %v, want none: a held server ends by itself", got.Delete)
	}
}
```

Add `"slices"` to the imports if missing. Check that `ServerView` has a `Hold` field (`internal/controller/candidates.go`); `deletable` reads `v.Hold`.

- [ ] **Step 5: Run and see them fail**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/controller/ -run 'Pin' -count=1`
Expected: build failure, `unknown field Pinned in struct literal`.

- [ ] **Step 6: Implement** in `internal/controller/scaling.go`. In `ScalingInputs`, after `MaxReplicas`:

```go
	// Pinned is an Exact ScaleBoost on the group. Pin is then both the floor
	// and the ceiling, bounded by MaxReplicas, and Boost does not count.
	Pinned bool
	Pin    int32
```

Replace `floor()` and add `ceiling()`:

```go
// floor is MinReplicas plus live boosts, or the pin. Both the create rule and
// the guard against shedding below the floor must read the same number.
func (in ScalingInputs) floor() int32 {
	if in.Pinned {
		return in.ceiling()
	}
	return in.MinReplicas + in.Boost
}

// ceiling is maxReplicas, lowered to the pin while one holds.
func (in ScalingInputs) ceiling() int32 {
	if in.Pinned && in.Pin < in.MaxReplicas {
		return in.Pin
	}
	return in.MaxReplicas
}
```

In `decideSize` replace the four reads of `in.MaxReplicas` with `in.ceiling()`: `room := in.ceiling() - alive`, both `if surplus := alive - in.ceiling(); surplus > 0`, and `if alive < in.ceiling()` in the floor-held branch.

- [ ] **Step 7: Run the sizing tests**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/controller/ -run 'Pin|Boost|DecideSize|Ceiling|Surplus' -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/boost internal/controller/scaling.go internal/controller/scaling_test.go
git commit -m "feat(controller): an Exact boost pins a group's floor and ceiling" \
  -m "<body: newest pin wins, Add boosts pause under a pin, maxReplicas still binds, a predecessor's boost no longer counts for a group of the same name>"
```

---

### Task 4: The pin reaches the group, its status and the mirror

**Files:**
- Modify: `internal/controller/servergroup_controller.go` (boost list at lines 356-366, `size` signature at line 631-643 and the `ScalingInputs` literal at lines 664-683, the `ScalingLimited` block at lines 385-413, status at line 546)
- Modify: `internal/agentserver/writer.go` (`Headroom`, line 255-277)
- Modify: `internal/netstate/netstate.go` (`GroupState` literal, lines 103-117)
- Test: `internal/controller/servergroup_controller_test.go` (after `TestABoostActuallyCreatesAServer`, ~line 4019), `internal/netstate/netstate_test.go`

**Interfaces:**
- Consumes: `boost.Of`, `boost.Live`, `boost.Exact`, `boost.Pin` (Task 3); `ServerGroupStatus.PinnedReplicas/PinnedUntil`, `ReasonPinned` (Task 1); `GroupState.Pinned/PinnedReplicas/PinnedUntilUnix` (Task 2).
- Produces: `type groupBoosts struct { Add int32; Pin boostpkg.Pin; Pinned bool }` in `internal/controller`; `size(..., boosts groupBoosts, ...)`.

- [ ] **Step 1: Write the failing envtests** in `servergroup_controller_test.go`:

```go
func (f *fixture) createPin(t *testing.T, name string, replicas int32, owner *metav1.OwnerReference) {
	t.Helper()
	expires := metav1.NewTime(f.clock.now.Add(time.Hour))
	b := &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: f.group.Name},
			Mode:      spawneryv1alpha1.ScaleBoostExact,
			Replicas:  replicas,
			ExpiresAt: &expires,
		},
	}
	if owner != nil {
		b.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	if err := f.c.Create(f.ctx, b); err != nil {
		t.Fatalf("create the pin: %v", err)
	}
}

func TestAPinOfZeroLeavesTheGroupEmptyAndSaysSo(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPin(t, "lobby-off", 0, nil)

	f.reconcileGroup(t, r)

	if got := len(f.listServers(t)); got != 0 {
		t.Fatalf("got %d servers, want 0 under a pin of 0", got)
	}
	group := f.serverGroup(t, f.group.Name)
	if group.Status.PinnedReplicas == nil || *group.Status.PinnedReplicas != 0 {
		t.Errorf("status.pinnedReplicas = %v, want a present 0", group.Status.PinnedReplicas)
	}
	if group.Status.PinnedUntil == nil {
		t.Error("status.pinnedUntil is empty for a pin that ends")
	}
}

func TestAPinBuildsExactlyItsNumber(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPin(t, "lobby-four", 4, nil)

	f.reconcileGroup(t, r)

	if got := len(f.listServers(t)); got != 4 {
		t.Fatalf("got %d servers, want the pinned 4", got)
	}
}

// envtest runs no garbage collector, so the stale pin stays as it would for a while in a cluster.
func TestAPredecessorsPinDoesNotHoldTheGroup(t *testing.T) {
	f := newFixture(t)
	r := groupReconciler(f)
	f.createPin(t, "lobby-stale", 0, &metav1.OwnerReference{
		APIVersion: spawneryv1alpha1.GroupVersion.String(), Kind: "ServerGroup",
		Name: f.group.Name, UID: "00000000-0000-0000-0000-00000000dead",
	})

	f.reconcileGroup(t, r)

	if got := len(f.listServers(t)); got != 1 {
		t.Fatalf("got %d servers, want the floor of 1: the pin belongs to a group that is gone", got)
	}
	if p := f.serverGroup(t, f.group.Name).Status.PinnedReplicas; p != nil {
		t.Errorf("status.pinnedReplicas = %d, want absent", *p)
	}
}
```

`f.serverGroup(t, name)` exists in `changeover_envtest_test.go:419`.

In `internal/netstate/netstate_test.go`:

```go
func TestBuildCarriesAGroupsPin(t *testing.T) {
	pinned := ephemeralGroup("ns", "lobby")
	until := metav1.NewTime(time.Unix(1_800_000_000, 0))
	pinned.Status.PinnedReplicas = ptr.To[int32](0)
	pinned.Status.PinnedUntil = &until
	src, _ := source(t, pinned, ephemeralGroup("ns", "arena"))

	got, err := src.Build(context.Background(), "ns", netstate.ForServers)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Sorted: arena, lobby.
	if g := got.GetGroups()[0]; g.GetPinned() {
		t.Errorf("arena = %+v, want no pin", g)
	}
	if g := got.GetGroups()[1]; !g.GetPinned() || g.GetPinnedReplicas() != 0 || g.GetPinnedUntilUnix() != 1_800_000_000 {
		t.Errorf("lobby = %+v, want pinned to 0 until 1800000000", g)
	}
}
```

Add `ptr`, `metav1`, `time` imports where missing.

- [ ] **Step 2: Run and see them fail**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/netstate/ -run Pin -count=1`
Expected: FAIL, `lobby = … want pinned to 0`.
Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/controller/ -run 'APin|Predecessor' -count=1`
Expected: FAIL, `got 1 servers, want 0 under a pin of 0`.

- [ ] **Step 3: Read the boosts once, per group.** Replace the block at lines 356-366 of `servergroup_controller.go`:

```go
	// A failed boost list sizes on the declared floor alone rather than failing
	// the pass, so dead servers are still replaced.
	var boosts groupBoosts
	boostList := &spawneryv1alpha1.ScaleBoostList{}
	if err := r.List(ctx, boostList, client.InNamespace(group.Namespace)); err != nil {
		log.FromContext(ctx).V(1).Info("could not read scale boosts; sizing on the declared floor alone",
			"group", group.Name, "reason", err.Error())
	} else {
		mine := boostpkg.Of(boostList.Items, group)
		now := r.Clock()
		boosts.Add = boostpkg.Live(mine, group.Name, now)
		if group.IsEphemeral() && group.Spec.Scaling != nil {
			boosts.Pin, boosts.Pinned = boostpkg.Exact(mine, group.Name, now)
		}
	}
```

Next to `size`, add the type:

```go
// groupBoosts is what a group's ScaleBoosts say this pass.
type groupBoosts struct {
	Add    int32
	Pin    boostpkg.Pin
	Pinned bool
}
```

Change `size`'s parameter `boost int32` to `boosts groupBoosts`, pass `boosts` at the call site, and in the `ScalingInputs` literal replace `Boost: boost,` with:

```go
				Boost:         boosts.Add,
				Pinned:        boosts.Pinned,
				Pin:           boosts.Pin.Replicas,
```

Search the file for any other use of the old `boost` variable (`grep -n '\bboost\b' internal/controller/servergroup_controller.go`) and move it to `boosts.Add`.

- [ ] **Step 4: Status.** Replace `group.Status.BoostedReplicas = boost` with:

```go
	group.Status.BoostedReplicas = boosts.Add
	group.Status.PinnedReplicas, group.Status.PinnedUntil = nil, nil
	if boosts.Pinned {
		group.Status.PinnedReplicas = ptr.To(boosts.Pin.Replicas)
		group.Status.PinnedUntil = boosts.Pin.ExpiresAt
	}
```

(`k8s.io/utils/ptr`; add the import if the file lacks it.) In the `ScalingLimited` block, make the pin the first case of the message chain:

```go
		if decision.Limited {
			limited.Status = metav1.ConditionTrue
			limited.Reason = spawneryv1alpha1.ReasonMaxReplicasReached
			switch {
			case boosts.Pinned:
				limited.Reason = spawneryv1alpha1.ReasonPinned
				limited.Message = fmt.Sprintf(
					"a ScaleBoost pins the group to %d server(s); spareSlots %d asks for %d more",
					boosts.Pin.Replicas, group.Spec.Scaling.SpareSlots, decision.Wanted)
			case decision.ColdStartBlocked:
				// …the existing message, unchanged
			case decision.FloorBlocked:
				// …the existing message, unchanged
			default:
				// …the existing message, unchanged
			}
		}
```

(Convert the existing `if / else if / else` into these `case`s, keeping their bodies and comments.)

- [ ] **Step 5: Headroom ignores a predecessor's boosts too.** In `writer.go`'s `Headroom`, replace `Boosted: boost.Live(boosts.Items, group, w.now()),` with `Boosted: boost.Live(boost.Of(boosts.Items, &g), group, w.now()),`.

- [ ] **Step 6: The mirror.** In `netstate.Build`'s server-group literal, after `JoinPermissionDenyOnly`:

```go
			Pinned:                 g.Status.PinnedReplicas != nil,
			PinnedReplicas:         ptr.Deref(g.Status.PinnedReplicas, 0),
			PinnedUntilUnix:        unixOrZero(g.Status.PinnedUntil),
```

and at the end of the file:

```go
func unixOrZero(t *metav1.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}
```

- [ ] **Step 7: Run**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/netstate/ ./internal/boost/ -count=1`
Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/controller/ -run 'APin|Predecessor|Boost' -count=1`
Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/agentserver/ -run Boost -count=1`
Expected: PASS everywhere.

- [ ] **Step 8: Commit**

```bash
git add internal/controller internal/netstate internal/agentserver/writer.go
git commit -m "feat(controller): a pinned group says so in its status and to every agent" \
  -m "<body: status.pinnedReplicas/pinnedUntil, ScalingLimited reason Pinned, GroupState pinned fields>"
```

---

### Task 5: Force-stop in the state machine and the server controller

**Files:**
- Modify: `internal/phase/phase.go` (`Inputs` after `RetirementRequested` ~line 185; reason list ~line 85-110; `Decision.DeletePod` doc ~line 212; `Decide` line 223)
- Modify: `internal/controller/server_controller.go` (`collectInputs` literal at lines 530-540; the pod delete in `applyDecision` at lines 810-815)
- Test: `internal/phase/phase_test.go`, `internal/controller/server_controller_test.go`

**Interfaces:**
- Consumes: `ServerSpec.ForceStop` (Task 1).
- Produces: `phase.Inputs.ForceStopRequested bool`; `phase.ReasonForceStopped = "ForceStopped"`; `func stillGraceful(pod *corev1.Pod) bool` in `internal/controller`.

- [ ] **Step 1: Write the failing phase test** in `phase_test.go`:

```go
func TestAForceStopEndsEveryPhaseAtOnce(t *testing.T) {
	for _, current := range declaredPhases(t) {
		in := healthyReady()
		in.PlayersOnline = 12
		in.ForceStopRequested = true

		got := Decide(current, in)

		if got.Next != Terminating || !got.DeletePod || got.StartDrain || got.Reason != ReasonForceStopped {
			t.Errorf("%s: decision = %+v, want Terminating, DeletePod, no drain, ForceStopped", current, got)
		}
		if !got.Deregister {
			t.Errorf("%s: a registered server was not deregistered", current)
		}
	}

	in := healthyReady()
	in.Registered = false
	in.ForceStopRequested = true
	if Decide(Starting, in).Deregister {
		t.Error("an unregistered server was deregistered")
	}
}
```

- [ ] **Step 2: Run and see it fail**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/phase/ -run ForceStop -count=1`
Expected: build failure, `unknown field ForceStopRequested`.

- [ ] **Step 3: Implement.** In `Inputs`, after `RetirementRequested`:

```go
	// ForceStopRequested is Server.spec.forceStop: kill the pod now, whatever
	// is on it.
	ForceStopRequested bool
```

Reason, in the const block after `ReasonTerminating`: `ReasonForceStopped = "ForceStopped"`.

`Decision.DeletePod` doc becomes:

```go
	// DeletePod means no players are at risk, or an administrator accepted the
	// risk with spec.forceStop.
	DeletePod bool
```

At the top of `Decide`, before the `switch`:

```go
	if in.ForceStopRequested {
		return Decision{
			Next: Terminating, DeletePod: true, Deregister: in.Registered,
			Reason: ReasonForceStopped, Message: "force-stopped: the pod is killed without a drain",
		}
	}
```

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/phase/ -count=1`
Expected: PASS.

- [ ] **Step 4: Write the failing controller tests** in `server_controller_test.go`:

```go
func (f *fixture) forceStop(t *testing.T, name string) {
	t.Helper()
	srv := f.server(name)
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.ForceStop = true
	if err := f.c.Patch(f.ctx, srv, patch); err != nil {
		t.Fatalf("set spec.forceStop on %s: %v", name, err)
	}
}

func (f *fixture) podAnyway(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := f.c.Get(f.ctx, types.NamespacedName{Name: name, Namespace: f.ns}, pod); err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return pod
}

func TestAForceStopKillsThePodWithoutAGracePeriod(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")
	pod, _ := f.pod("lobby-x7k2")
	f.bindPodToNode(t, pod, f.ensureNode(t, "node-force-"+f.ns, false).Name)
	f.holdPodOnDelete(t, pod)
	t.Cleanup(func() { f.retirePodTheWayAKubeletWould(t, pod) })

	f.forceStop(t, "lobby-x7k2")
	f.reconcile("lobby-x7k2")

	got := f.podAnyway(t, "lobby-x7k2")
	if got.DeletionTimestamp.IsZero() {
		t.Fatal("the pod was not deleted")
	}
	if g := got.DeletionGracePeriodSeconds; g == nil || *g != 0 {
		t.Errorf("deletionGracePeriodSeconds = %v, want 0", g)
	}
	if phase.Phase(f.server("lobby-x7k2").Status.Phase) != phase.Terminating {
		t.Errorf("phase = %s, want Terminating", f.server("lobby-x7k2").Status.Phase)
	}
	if !slices.Contains(f.registrar.deregistered, "lobby-x7k2") {
		t.Errorf("deregistered = %v, want the server taken out of the proxies", f.registrar.deregistered)
	}
	if len(f.registrar.drained) != 0 {
		t.Errorf("drained = %v, want no drain", f.registrar.drained)
	}
}

func TestAForceStopShortensAGracePeriodAlreadyRunning(t *testing.T) {
	f := newFixture(t)
	bringUpReady(t, f, "lobby-x7k2")
	pod, _ := f.pod("lobby-x7k2")
	f.bindPodToNode(t, pod, f.ensureNode(t, "node-force-"+f.ns, false).Name)
	f.holdPodOnDelete(t, pod)
	t.Cleanup(func() { f.retirePodTheWayAKubeletWould(t, pod) })
	if err := f.c.Delete(f.ctx, pod); err != nil {
		t.Fatalf("delete the pod gracefully: %v", err)
	}
	if g := f.podAnyway(t, "lobby-x7k2").DeletionGracePeriodSeconds; g == nil || *g == 0 {
		t.Fatalf("the ordinary delete left grace %v; the test needs a running grace period", g)
	}

	f.forceStop(t, "lobby-x7k2")
	f.reconcile("lobby-x7k2")

	if g := f.podAnyway(t, "lobby-x7k2").DeletionGracePeriodSeconds; g == nil || *g != 0 {
		t.Errorf("deletionGracePeriodSeconds = %v, want 0 after the force-stop", g)
	}
}
```

Check `f.registrar.drained` is the field name (`suite_test.go:141`); add `"slices"` to the imports if missing.

- [ ] **Step 5: Run and see them fail**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/controller/ -run AForceStop -count=1`
Expected: FAIL, `deletionGracePeriodSeconds = 0x…(30), want 0` (or the pod not deleted).

- [ ] **Step 6: Implement.** In `collectInputs`, add `ForceStopRequested: srv.Spec.ForceStop,` beside `RetirementRequested`. In `applyDecision`, replace the pod delete:

```go
	switch {
	case d.DeletePod && pod != nil && srv.Spec.ForceStop && stillGraceful(pod):
		if err := r.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		r.Recorder.Eventf(srv, nil, corev1.EventTypeWarning, "PodKilled", actionDeletePod,
			"killed pod %s without a grace period: %s", pod.Name, d.Message)
	case d.DeletePod && podFound && pod.DeletionTimestamp.IsZero():
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		r.Recorder.Eventf(srv, nil, corev1.EventTypeNormal, "PodDeleted", actionDeletePod,
			"deleted pod %s: %s", pod.Name, d.Message)
	}
```

and below `applyDecision`:

```go
// stillGraceful is a pod the kubelet may still take time to stop. fetchPod
// reports a terminating pod as not found, so the force-stop branch reads the
// pod itself.
func stillGraceful(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp.IsZero() ||
		pod.DeletionGracePeriodSeconds == nil || *pod.DeletionGracePeriodSeconds > 0
}
```

- [ ] **Step 7: Run**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/phase/ -count=1`
Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/controller/ -run 'AForceStop|Terminating|Retire|Drain' -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/phase internal/controller/server_controller.go internal/controller/server_controller_test.go
git commit -m "feat(controller): spec.forceStop kills a server's pod without a drain" \
  -m "<body: Terminating from every phase, grace period 0, a second delete shortens a grace period already running>"
```

---

### Task 6: The operator answers scale, forcestop and execute

**Files:**
- Modify: `internal/agentserver/requests.go` (constants lines 38-60, `answerCloudRequest` switch lines 142-168)
- Modify: `internal/agentserver/writer.go` (errors near line 39-91, `ClusterWriter` lines 96-126, `Boost` lines 279-308)
- Modify: `internal/agentserver/server.go` (`ServerFanout` lines 98-103, `Options` lines 105-126, `Server` struct lines 129-137, `ServerSession` handle lines 413-454, `ProxySession` loop lines 456-509, `handleProxy` lines 514-582)
- Modify: `internal/serverreg/registry.go` (new `Send` after `SetInterest`)
- Modify: `cmd/spawnery-operator/main.go` (`agentserver.Options` at line 376-403)
- Create: `internal/agentserver/commands.go`, `internal/agentserver/execute.go`
- Test: `internal/agentserver/commands_test.go`, `internal/agentserver/execute_test.go`, `internal/agentserver/execute_envtest_test.go`, `internal/serverreg/registry_test.go`; `internal/agentserver/server_test.go` (`stubFanout` gains `Send`)

**Interfaces:**
- Consumes: `agentpb` messages (Task 2); `ScaleBoostExact`, `ServerSpec.ForceStop`, `Network.ExecuteEnabled` (Task 1); `boost.Of` (Task 3).
- Produces: constants `ScaleDefaultDuration = time.Hour`, `ScaleMaxDuration = 7 * 24 * time.Hour`, `ExecuteWait = 8 * time.Second`, `ExecuteMaxCommandLength = 256`, `ExecuteMaxLines = 20`, `ExecuteMaxLineLength = 256`; `ClusterWriter.Pin(ctx, namespace, group string, replicas int32, expiresAt time.Time) error`; `ClusterWriter.ForceStop(ctx, namespace, name string) error`; `ErrNotAServer`; `ServerFanout.Send(podUID string, msg *agentpb.OperatorToServer) bool`; `(*serverreg.Registry).Send`; `Options.Recorder events.EventRecorder` (nil records nothing); `Options.ExecuteWait time.Duration` (zero means `ExecuteWait`).

Why the proxy session answers execute off its loop: `ProxySession` handles each message inline (`server.go` around line 490) and sends the answer before reading its outbox again. An 8 s wait there would hold every registration and drain order behind it, and a full outbox cuts the proxy (`proxyreg` closes a session that falls behind). A server session never waits: execute is refused there before anything else.

- [ ] **Step 1: Write the failing unit tests** in `internal/agentserver/commands_test.go`:

```go
package agentserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/netstate"
)

var (
	commandNow   = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	proxyCaller  = grpcauth.Identity{Namespace: "ns", PodName: "gateway-0", PodUID: "proxy-a", Role: agent.RoleProxy}
	serverCaller = grpcauth.Identity{Namespace: "ns", PodName: "lobby-a", PodUID: "pod-a", Role: agent.RoleServer}
)

func commandFixture(t *testing.T, objects ...client.Object) (*Server, client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spawneryv1alpha1.Server{}).WithObjects(objects...).Build()
	rec := events.NewFakeRecorder(16)
	clock := func() time.Time { return commandNow }
	s := &Server{
		opts: Options{
			Writer:      KubeWriter{Client: c, Clock: clock},
			State:       netstate.Source{Reader: c},
			Servers:     &recordingFanout{live: map[string]bool{}},
			Recorder:    rec,
			Clock:       clock,
			ExecuteWait: 300 * time.Millisecond,
		},
		requestRate: newRequestLimiter(time.Now),
	}
	return s, c, rec
}

func scalableGroup(name string, minR, maxR int32) *spawneryv1alpha1.ServerGroup {
	return &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: "uid-" + name},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			Type:    spawneryv1alpha1.ServerGroupEphemeral,
			Scaling: &spawneryv1alpha1.ScalingSpec{MinReplicas: minR, MaxReplicas: maxR, SpareSlots: 10},
		},
	}
}

func askScale(s *Server, id grpcauth.Identity, group string, replicas int32, seconds int64) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), id, &agentpb.CloudRequest{
		Id: 1, Request: &agentpb.CloudRequest_Scale{Scale: &agentpb.ScaleRequest{
			Group: group, Replicas: replicas, DurationSeconds: seconds,
		}},
	})
}

func TestAScaleCreatesAnExactBoostOwnedByTheGroup(t *testing.T) {
	s, c, _ := commandFixture(t, scalableGroup("lobby", 1, 5))

	resp := askScale(s, serverCaller, "lobby", 0, 0)

	if resp.GetScale().GetReplicas() != 0 || resp.GetScale().GetExpiresAtUnix() != commandNow.Add(time.Hour).Unix() {
		t.Fatalf("answer = %+v, want a pin of 0 for the default hour", resp.GetResult())
	}
	var list spawneryv1alpha1.ScaleBoostList
	if err := c.List(context.Background(), &list, client.InNamespace("ns")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("boosts = %d, want 1", len(list.Items))
	}
	b := list.Items[0]
	if b.Spec.Mode != spawneryv1alpha1.ScaleBoostExact || b.Spec.Replicas != 0 || b.Spec.GroupRef.Name != "lobby" {
		t.Errorf("boost = %+v, want Exact 0 on lobby", b.Spec)
	}
	if len(b.OwnerReferences) != 1 || b.OwnerReferences[0].UID != "uid-lobby" {
		t.Errorf("owners = %+v, want the group", b.OwnerReferences)
	}
}

func TestAScaleIsRefusedWhereItCannotHold(t *testing.T) {
	persistent := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerGroupSpec{Type: spawneryv1alpha1.ServerGroupPersistent},
	}
	s, _, _ := commandFixture(t, scalableGroup("lobby", 1, 5), persistent)

	for name, c := range map[string]struct {
		group    string
		replicas int32
		seconds  int64
		reason   agentpb.RequestError_Reason
		says     string
	}{
		"below zero":              {"lobby", -1, 0, agentpb.RequestError_REFUSED, "below zero"},
		"above maxReplicas":       {"lobby", 6, 0, agentpb.RequestError_REFUSED, "maxReplicas is 5"},
		"eight days":              {"lobby", 1, 8 * 24 * 3600, agentpb.RequestError_REFUSED, "7 days"},
		"seconds that overflow":   {"lobby", 1, 1 << 62, agentpb.RequestError_REFUSED, "7 days"},
		"a persistent group":      {"survival", 1, 0, agentpb.RequestError_REFUSED, "ephemeral"},
		"a group that is not here": {"nowhere", 1, 0, agentpb.RequestError_NOT_FOUND, "no group"},
	} {
		got := askScale(s, serverCaller, c.group, c.replicas, c.seconds).GetError()
		if got.GetReason() != c.reason || !strings.Contains(got.GetMessage(), c.says) {
			t.Errorf("%s: error = %v, want %s mentioning %q", name, got, c.reason, c.says)
		}
	}
}

func TestSevenDaysExactlyIsAllowed(t *testing.T) {
	s, _, _ := commandFixture(t, scalableGroup("lobby", 1, 5))
	if resp := askScale(s, serverCaller, "lobby", 2, 7*24*3600); resp.GetScale() == nil {
		t.Errorf("answer = %+v, want a pin: seven days is the limit, not past it", resp.GetResult())
	}
}

func askForceStop(s *Server, id grpcauth.Identity, server string) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), id, &agentpb.CloudRequest{
		Id: 2, Request: &agentpb.CloudRequest_ForceStop{ForceStop: &agentpb.ForceStopRequest{Server: server, Issuer: "alice"}},
	})
}

func readyServer(name, group, podUID string) *spawneryv1alpha1.Server {
	return &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: group}},
		Status:     spawneryv1alpha1.ServerStatus{Phase: "Ready", PodName: name, PodUID: podUID},
	}
}

func TestAForceStopFromAProxySetsTheFlagAndLeavesARecord(t *testing.T) {
	s, c, rec := commandFixture(t, readyServer("lobby-a", "lobby", "pod-a"))

	resp := askForceStop(s, proxyCaller, "lobby-a")

	if resp.GetForceStop().GetServer() != "lobby-a" {
		t.Fatalf("answer = %+v, want lobby-a", resp.GetResult())
	}
	var srv spawneryv1alpha1.Server
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "lobby-a"}, &srv); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !srv.Spec.ForceStop {
		t.Error("spec.forceStop was not set")
	}
	select {
	case ev := <-rec.Events:
		for _, want := range []string{"ForceStopped", "alice", "gateway-0"} {
			if !strings.Contains(ev, want) {
				t.Errorf("event %q does not name %q", ev, want)
			}
		}
	default:
		t.Error("no event was recorded")
	}

	if again := askForceStop(s, proxyCaller, "lobby-a"); again.GetForceStop() == nil {
		t.Errorf("a second force-stop = %+v, want success: it is what shortens a grace period already running", again.GetResult())
	}
}

func TestAForceStopIsRefusedFromABackendAndForAProxyOrNothing(t *testing.T) {
	gateway := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway-0", Namespace: "ns",
		Labels: map[string]string{"spawnery.cloud/role": "proxy"}}}
	s, c, _ := commandFixture(t, readyServer("lobby-a", "lobby", "pod-a"), gateway)

	if got := askForceStop(s, serverCaller, "lobby-a").GetError(); got.GetReason() != agentpb.RequestError_REFUSED ||
		!strings.Contains(got.GetMessage(), "proxy") {
		t.Errorf("from a backend: %v, want REFUSED naming the proxy rule", got)
	}
	var srv spawneryv1alpha1.Server
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "lobby-a"}, &srv)
	if srv.Spec.ForceStop {
		t.Error("a backend's force-stop set the flag")
	}
	if got := askForceStop(s, proxyCaller, "gateway-0").GetError(); got.GetReason() != agentpb.RequestError_REFUSED {
		t.Errorf("a proxy's name: %v, want REFUSED", got)
	}
	if got := askForceStop(s, proxyCaller, "nobody").GetError(); got.GetReason() != agentpb.RequestError_NOT_FOUND {
		t.Errorf("an unknown name: %v, want NOT_FOUND", got)
	}
}
```

Use `podspec.LabelRole` / `podspec.RoleProxy` instead of the literal label if `internal/podspec` is importable here (writer.go already imports it).

In `internal/agentserver/execute_test.go`:

```go
package agentserver

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/agent"
)

// recordingFanout is a server fan-out whose live sessions a test chooses, and
// which hands every ExecuteCommand to onSend.
type recordingFanout struct {
	mu     sync.Mutex
	live   map[string]bool
	sent   []string
	onSend func(podUID string, cmd *agentpb.ExecuteCommand)
}

func (f *recordingFanout) Join(context.Context, string, string) (<-chan *agentpb.OperatorToServer, func(), error) {
	return nil, func() {}, nil
}

func (f *recordingFanout) SetInterest(string, bool) {}

func (f *recordingFanout) Send(podUID string, msg *agentpb.OperatorToServer) bool {
	f.mu.Lock()
	live := f.live[podUID]
	if live {
		f.sent = append(f.sent, podUID)
	}
	on := f.onSend
	f.mu.Unlock()
	if live && on != nil {
		on(podUID, msg.GetExecuteCommand())
	}
	return live
}

func network(execute bool) *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "ns"},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "secret"},
			Commands:            &spawneryv1alpha1.NetworkCommands{Execute: execute},
		},
	}
}

func askExecute(s *Server, id grpcauth.Identity, target, command string) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), id, &agentpb.CloudRequest{
		Id: 3, Request: &agentpb.CloudRequest_Execute{Execute: &agentpb.ExecuteRequest{
			Target: target, Command: command, Issuer: "alice",
		}},
	})
}

// answering makes every live server answer through the real receive path.
func answering(s *Server, fan *recordingFanout, reply func(podUID string, cmd *agentpb.ExecuteCommand) *agentpb.ExecuteOutcome) {
	fan.onSend = func(podUID string, cmd *agentpb.ExecuteCommand) {
		outcome := reply(podUID, cmd)
		if outcome == nil {
			return
		}
		go s.handle(context.Background(), logr.Discard(),
			grpcauth.Identity{Namespace: "ns", PodUID: podUID, Role: agent.RoleServer},
			&agentpb.ServerMessage{Message: &agentpb.ServerMessage_ExecuteOutcome{ExecuteOutcome: outcome}})
	}
}

func TestExecuteIsRefusedBeforeAnythingRuns(t *testing.T) {
	off, _, _ := commandFixture(t, network(false), readyServer("lobby-a", "lobby", "pod-a"))
	on, _, _ := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	none, _, _ := commandFixture(t, readyServer("lobby-a", "lobby", "pod-a"))

	for name, c := range map[string]struct {
		s       *Server
		id      grpcauth.Identity
		target  string
		command string
		reason  agentpb.RequestError_Reason
		says    string
	}{
		"from a backend":        {on, serverCaller, "lobby", "list", agentpb.RequestError_REFUSED, "proxy"},
		"switch off":            {off, proxyCaller, "lobby", "list", agentpb.RequestError_REFUSED, "execute is not enabled on this network"},
		"no Network at all":     {none, proxyCaller, "lobby", "list", agentpb.RequestError_REFUSED, "execute is not enabled on this network"},
		"empty command":         {on, proxyCaller, "lobby", " / ", agentpb.RequestError_REFUSED, "no command"},
		"257 characters":        {on, proxyCaller, "lobby", strings.Repeat("x", 257), agentpb.RequestError_REFUSED, "256"},
		"nothing by that name":  {on, proxyCaller, "nowhere", "list", agentpb.RequestError_NOT_FOUND, "no server or group"},
	} {
		got := askExecute(c.s, c.id, c.target, c.command).GetError()
		if got.GetReason() != c.reason || !strings.Contains(got.GetMessage(), c.says) {
			t.Errorf("%s: %v, want %s mentioning %q", name, got, c.reason, c.says)
		}
		if sent := c.s.opts.Servers.(*recordingFanout).sent; len(sent) != 0 {
			t.Errorf("%s: a command went out to %v", name, sent)
		}
	}
}

func TestExecuteOnOneServerBringsItsOutputBack(t *testing.T) {
	s, _, rec := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	fan := s.opts.Servers.(*recordingFanout)
	fan.live["pod-a"] = true
	answering(s, fan, func(_ string, cmd *agentpb.ExecuteCommand) *agentpb.ExecuteOutcome {
		if cmd.GetCommand() != "list" {
			t.Errorf("command = %q, want list without the slash", cmd.GetCommand())
		}
		return &agentpb.ExecuteOutcome{Id: cmd.GetId(), Ok: true, Output: []string{"There are 0 of a max of 20 players online"}}
	})

	got := askExecute(s, proxyCaller, "lobby-a", "/list").GetExecute().GetOutcomes()

	if len(got) != 1 || got[0].GetServer() != "lobby-a" || !got[0].GetOk() || len(got[0].GetOutput()) != 1 {
		t.Fatalf("outcomes = %+v, want lobby-a ok with one line", got)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "CommandExecuted") || !strings.Contains(ev, "alice") || strings.Contains(ev, "players online") {
			t.Errorf("event %q: want CommandExecuted naming the issuer and never the output", ev)
		}
	default:
		t.Error("no event was recorded")
	}
}

func TestAServerThatDoesNotAnswerIsListedAndTheRestStillArrive(t *testing.T) {
	s, _, _ := commandFixture(t, network(true),
		readyServer("lobby-a", "lobby", "pod-a"), readyServer("lobby-b", "lobby", "pod-b"),
		readyServer("lobby-c", "lobby", "pod-c"))
	fan := s.opts.Servers.(*recordingFanout)
	fan.live["pod-a"], fan.live["pod-b"] = true, true
	answering(s, fan, func(podUID string, cmd *agentpb.ExecuteCommand) *agentpb.ExecuteOutcome {
		if podUID == "pod-b" {
			return nil
		}
		return &agentpb.ExecuteOutcome{Id: cmd.GetId(), Ok: true}
	})

	start := time.Now()
	got := askExecute(s, proxyCaller, "lobby", "say hi").GetExecute().GetOutcomes()

	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the answer took %s; the wait is 300ms", took)
	}
	if len(got) != 2 {
		t.Fatalf("outcomes = %+v, want lobby-a and lobby-b: lobby-c has no session", got)
	}
	if got[0].GetServer() != "lobby-a" || !got[0].GetOk() {
		t.Errorf("first = %+v, want lobby-a ok", got[0])
	}
	if got[1].GetServer() != "lobby-b" || got[1].GetOk() || !strings.Contains(got[1].GetError(), "no answer within") {
		t.Errorf("second = %+v, want lobby-b with no answer", got[1])
	}
}

func TestOneServerCannotAnswerForAnother(t *testing.T) {
	s, _, _ := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	fan := s.opts.Servers.(*recordingFanout)
	fan.live["pod-a"] = true
	fan.onSend = func(_ string, cmd *agentpb.ExecuteCommand) {
		go s.handle(context.Background(), logr.Discard(),
			grpcauth.Identity{Namespace: "ns", PodUID: "pod-evil", Role: agent.RoleServer},
			&agentpb.ServerMessage{Message: &agentpb.ServerMessage_ExecuteOutcome{
				ExecuteOutcome: &agentpb.ExecuteOutcome{Id: cmd.GetId(), Ok: true, Output: []string{"forged"}},
			}})
	}

	got := askExecute(s, proxyCaller, "lobby-a", "list").GetExecute().GetOutcomes()

	if len(got) != 1 || got[0].GetOk() || len(got[0].GetOutput()) != 0 {
		t.Errorf("outcomes = %+v, want lobby-a unanswered: pod-evil answered for it", got)
	}
}

func TestANamedServerWithoutASessionIsUnavailable(t *testing.T) {
	s, _, _ := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	if got := askExecute(s, proxyCaller, "lobby-a", "list").GetError(); got.GetReason() != agentpb.RequestError_UNAVAILABLE {
		t.Errorf("error = %v, want UNAVAILABLE", got)
	}
}

func TestAServersOutputIsBoundedAgainByTheOperator(t *testing.T) {
	s, _, _ := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	fan := s.opts.Servers.(*recordingFanout)
	fan.live["pod-a"] = true
	answering(s, fan, func(_ string, cmd *agentpb.ExecuteCommand) *agentpb.ExecuteOutcome {
		lines := make([]string, 50)
		for i := range lines {
			lines[i] = strings.Repeat("é", 400)
		}
		return &agentpb.ExecuteOutcome{Id: cmd.GetId(), Ok: true, Output: lines}
	})

	out := askExecute(s, proxyCaller, "lobby-a", "list").GetExecute().GetOutcomes()[0].GetOutput()

	if len(out) != ExecuteMaxLines {
		t.Errorf("%d lines, want %d", len(out), ExecuteMaxLines)
	}
	if n := len([]rune(out[0])); n != ExecuteMaxLineLength {
		t.Errorf("a line of %d characters, want %d, cut on a character boundary", n, ExecuteMaxLineLength)
	}
}
```

In `server_test.go`, add to `stubFanout`:

```go
func (stubFanout) Send(string, *agentpb.OperatorToServer) bool { return false }
```

In `internal/serverreg/registry_test.go` (check its existing helpers for building a `Registry` with a fake `netstate.Source`; reuse them):

```go
func TestSendReachesOnlyTheNamedSession(t *testing.T) {
	r := newTestRegistry(t) // use the file's existing constructor helper
	a, leaveA, err := r.Join(context.Background(), "ns", "pod-a")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer leaveA()
	b, leaveB, err := r.Join(context.Background(), "ns", "pod-b")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer leaveB()
	<-a
	<-b

	msg := &agentpb.OperatorToServer{Message: &agentpb.OperatorToServer_ExecuteCommand{
		ExecuteCommand: &agentpb.ExecuteCommand{Id: 1, Command: "list"}}}
	if !r.Send("pod-a", msg) {
		t.Fatal("Send to a live session reported failure")
	}
	if got := <-a; got.GetExecuteCommand().GetId() != 1 {
		t.Errorf("pod-a got %v", got)
	}
	select {
	case got := <-b:
		t.Errorf("pod-b got %v, want nothing", got)
	default:
	}
	if r.Send("pod-gone", msg) {
		t.Error("Send to no session reported success")
	}
}
```

- [ ] **Step 2: Run them and see them fail**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/agentserver/ ./internal/serverreg/ -run 'Scale|ForceStop|Execute|Answer|Bounded|Send' -count=1`
Expected: build failure, `unknown field Recorder in struct literal` / `r.Send undefined`.

- [ ] **Step 3: The writer.** In `writer.go` add:

```go
// ErrNotAServer is a server verb aimed at a proxy.
var ErrNotAServer = errors.New("that is a proxy, not a server")
```

In `ClusterWriter`:

```go
	// Pin creates an Exact ScaleBoost on a group, owned by it. The caller has
	// already bounded the numbers.
	Pin(ctx context.Context, namespace, group string, replicas int32, expiresAt time.Time) error

	// ForceStop sets spec.forceStop on a server; ErrNotAServer for a proxy's
	// name, ErrNoSuchServer for anything else unknown.
	ForceStop(ctx context.Context, namespace, name string) error
```

Turn `Boost`'s body into `createBoost` and call it from both:

```go
func (w KubeWriter) Boost(ctx context.Context, namespace, group string, replicas int32, expiresAt time.Time) error {
	return w.createBoost(ctx, namespace, group, spawneryv1alpha1.ScaleBoostAdd, replicas, expiresAt)
}

func (w KubeWriter) Pin(ctx context.Context, namespace, group string, replicas int32, expiresAt time.Time) error {
	return w.createBoost(ctx, namespace, group, spawneryv1alpha1.ScaleBoostExact, replicas, expiresAt)
}
```

`createBoost` is the old `Boost` body with `mode spawneryv1alpha1.ScaleBoostMode` as its fourth parameter and `Mode: mode,` in the spec literal; keep its doc comment (`generateName` so two admins get two boosts).

```go
// ForceStop does not refuse a server already set: a second force-stop is what
// shortens a grace period that began before the first.
func (w KubeWriter) ForceStop(ctx context.Context, namespace, name string) error {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		var pod corev1.Pod
		if perr := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); perr == nil &&
			pod.Labels[podspec.LabelRole] == podspec.RoleProxy {
			return ErrNotAServer
		}
		return ErrNoSuchServer
	}
	if srv.Spec.ForceStop {
		return nil
	}
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.ForceStop = true
	return w.Client.Patch(ctx, &srv, patch)
}
```

- [ ] **Step 4: Scale and force-stop answers** in a new `internal/agentserver/commands.go`:

```go
package agentserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
)

const (
	ScaleDefaultDuration = time.Hour
	// ScaleMaxDuration: a pin forgotten at 0 would otherwise keep a group off
	// for weeks without anybody noticing.
	ScaleMaxDuration = 7 * 24 * time.Hour

	issuerMaxLength = 64
)

// answerScale pins an ephemeral group, resolved under id.Namespace. A pin
// above maxReplicas is refused rather than capped, as a boost is.
func (s *Server) answerScale(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ScaleRequest,
) *agentpb.CloudResponse {
	if req.GetReplicas() < 0 {
		return refuse(reqID, agentpb.RequestError_REFUSED, "a group cannot be pinned below zero servers")
	}
	// Bounded before the multiplication, which would wrap a large count of
	// seconds into a negative duration and then into the default.
	if req.GetDurationSeconds() > int64(ScaleMaxDuration/time.Second) {
		return refuse(reqID, agentpb.RequestError_REFUSED, "the longest a pin may hold is 7 days")
	}
	duration := time.Duration(req.GetDurationSeconds()) * time.Second
	if duration <= 0 {
		duration = ScaleDefaultDuration
	}

	headroom, err := s.opts.Writer.Headroom(ctx, id.Namespace, req.GetGroup())
	switch {
	case errors.Is(err, ErrNoSuchGroup):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND, "no group by that name is on this network")
	case errors.Is(err, ErrGroupNotScalable):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"only an ephemeral group can be pinned: a persistent group is sized by its replica count, "+
				"an on-demand group by requests")
	case err != nil:
		logger.V(1).Info("could not read a group for a scale request", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not read that group just now")
	}
	if req.GetReplicas() > headroom.MaxReplicas {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			fmt.Sprintf("that group's maxReplicas is %d; a pin may not lift it", headroom.MaxReplicas))
	}

	expiresAt := s.opts.Clock().Add(duration)
	if err := s.opts.Writer.Pin(ctx, id.Namespace, req.GetGroup(), req.GetReplicas(), expiresAt); err != nil {
		if errors.Is(err, ErrNoSuchGroup) {
			return refuse(reqID, agentpb.RequestError_NOT_FOUND, "no group by that name is on this network")
		}
		logger.V(1).Info("could not create a pin", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not write that just now")
	}
	return &agentpb.CloudResponse{
		Id: reqID,
		Result: &agentpb.CloudResponse_Scale{Scale: &agentpb.ScaleResult{
			Replicas:      req.GetReplicas(),
			ExpiresAtUnix: expiresAt.Unix(),
		}},
	}
}

// answerForceStop is proxy-only: it disconnects players without a drain and
// can lose unsaved world data, so one compromised game server must not be able
// to do it to its neighbours.
func (s *Server) answerForceStop(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ForceStopRequest,
) *agentpb.CloudResponse {
	if id.Role != agent.RoleProxy {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"only a proxy may force-stop a server: one compromised game server must not kill its neighbours")
	}
	err := s.opts.Writer.ForceStop(ctx, id.Namespace, req.GetServer())
	switch {
	case errors.Is(err, ErrNoSuchServer):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND, "no server by that name is on this network")
	case errors.Is(err, ErrNotAServer):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that is a proxy: /cloud retire drains a proxy, and nothing kills one from chat")
	case err != nil:
		logger.V(1).Info("could not force-stop a server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not write that just now")
	}
	issuer := clip(req.GetIssuer(), issuerMaxLength)
	logger.Info("force-stopping a server", "server", req.GetServer(), "proxy", id.PodName, "issuer", issuer)
	s.recordOn(ctx, id.Namespace, req.GetServer(), corev1.EventTypeWarning, "ForceStopped", "ForceStop",
		fmt.Sprintf("force-stopped by %s on proxy %s", issuer, id.PodName))
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_ForceStop{ForceStop: &agentpb.ForceStopResult{Server: req.GetServer()}},
	}
}

// recordOn reports nothing: a lost event must not fail a request that was
// carried out.
func (s *Server) recordOn(ctx context.Context, namespace, server, eventType, reason, action, note string) {
	if s.opts.Recorder == nil {
		return
	}
	var srv spawneryv1alpha1.Server
	if err := s.opts.State.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: server}, &srv); err != nil {
		return
	}
	s.opts.Recorder.Eventf(&srv, nil, eventType, reason, action, "%s", note)
}

// clip cuts on a character boundary.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
```

In `answerCloudRequest`'s switch, before `default`:

```go
	case req.GetScale() != nil:
		return s.answerScale(ctx, logger, id, req.GetId(), req.GetScale())
	case req.GetForceStop() != nil:
		return s.answerForceStop(ctx, logger, id, req.GetId(), req.GetForceStop())
	case req.GetExecute() != nil:
		return s.answerExecute(ctx, logger, id, req.GetId(), req.GetExecute())
```

In `server.go`'s `Options`, after `Status`:

```go
	// Recorder records ForceStopped and CommandExecuted on the Server. Nil
	// records nothing.
	Recorder events.EventRecorder
	// ExecuteWait zero means ExecuteWait.
	ExecuteWait time.Duration
```

(`k8s.io/client-go/tools/events`.) In `Server`, add `executions executions` (the zero value is ready to use). In `ServerFanout`:

```go
	// Send queues one message for one session and reports whether it went out.
	Send(podUID string, msg *agentpb.OperatorToServer) bool
```

- [ ] **Step 5: Execute** in a new `internal/agentserver/execute.go`:

```go
package agentserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/phase"
)

const (
	// ExecuteWait sits inside the ten seconds after which an agent gives up on
	// a request, so the proxy hears about the servers that did answer.
	ExecuteWait             = 8 * time.Second
	ExecuteMaxCommandLength = 256
	ExecuteMaxLines         = 20
	ExecuteMaxLineLength    = 256
)

var errNoTarget = errors.New("no server or group by that name")

type executionKey struct {
	pod string
	id  uint64
}

// executions pairs an outcome with the command it answers. The pod UID is
// part of the key, so a server can only answer for itself.
type executions struct {
	mu      sync.Mutex
	next    uint64
	waiting map[executionKey]chan *agentpb.ExecuteOutcome
}

func (e *executions) open(pod string) (uint64, <-chan *agentpb.ExecuteOutcome, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.waiting == nil {
		e.waiting = map[executionKey]chan *agentpb.ExecuteOutcome{}
	}
	e.next++
	key := executionKey{pod: pod, id: e.next}
	answer := make(chan *agentpb.ExecuteOutcome, 1)
	e.waiting[key] = answer
	return key.id, answer, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.waiting, key)
	}
}

// deliver drops an outcome nobody waits for: a late one, or one for another
// pod's command.
func (e *executions) deliver(pod string, outcome *agentpb.ExecuteOutcome) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := executionKey{pod: pod, id: outcome.GetId()}
	if answer, ok := e.waiting[key]; ok {
		delete(e.waiting, key)
		answer <- outcome
	}
}

// answerExecute is proxy-only, behind the network's switch: every other
// request on the channel may come from any agent, and a console command on
// every server must not.
func (s *Server) answerExecute(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ExecuteRequest,
) *agentpb.CloudResponse {
	if id.Role != agent.RoleProxy {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"only a proxy may run a command on a server: one compromised game server must not reach the others' consoles")
	}
	command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.GetCommand()), "/"))
	if command == "" {
		return refuse(reqID, agentpb.RequestError_REFUSED, "there is no command to run")
	}
	if n := len([]rune(command)); n > ExecuteMaxCommandLength {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			fmt.Sprintf("that command is %d characters and the operator carries at most %d", n, ExecuteMaxCommandLength))
	}
	network, enabled, err := s.executeSwitch(ctx, id.Namespace)
	if err != nil {
		logger.V(1).Info("could not read the network for an execute request", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not read this network just now")
	}
	if !enabled {
		return refuse(reqID, agentpb.RequestError_REFUSED, "execute is not enabled on this network")
	}
	targets, single, err := s.executeTargets(ctx, id.Namespace, req.GetTarget())
	switch {
	case errors.Is(err, errNoTarget):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no server or group by that name is on this network, or the group has no Ready server")
	case err != nil:
		logger.V(1).Info("could not list servers for an execute request", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not read this network just now")
	}

	issuer := clip(req.GetIssuer(), issuerMaxLength)
	logger.Info("running a console command", "network", network, "proxy", id.PodName,
		"issuer", issuer, "target", req.GetTarget(), "command", command)

	type waiting struct {
		server  string
		answer  <-chan *agentpb.ExecuteOutcome
		forget  func()
	}
	var waits []waiting
	for i := range targets {
		srv := &targets[i]
		execID, answer, forget := s.executions.open(srv.Status.PodUID)
		sent := srv.Status.PodUID != "" && s.opts.Servers.Send(srv.Status.PodUID, &agentpb.OperatorToServer{
			Message: &agentpb.OperatorToServer_ExecuteCommand{
				ExecuteCommand: &agentpb.ExecuteCommand{Id: execID, Command: command},
			},
		})
		if !sent {
			forget()
			continue
		}
		s.recordOn(ctx, id.Namespace, srv.Name, corev1.EventTypeNormal, "CommandExecuted", "Execute",
			fmt.Sprintf("%s ran %q from proxy %s", issuer, command, id.PodName))
		waits = append(waits, waiting{server: srv.Name, answer: answer, forget: forget})
	}
	if len(waits) == 0 {
		if single {
			return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
				"that server's agent is not connected, so nothing can reach its console")
		}
		return refuse(reqID, agentpb.RequestError_NOT_FOUND, "no Ready server of that group has its agent connected")
	}

	wait := s.opts.ExecuteWait
	if wait <= 0 {
		wait = ExecuteWait
	}
	deadline, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	outcomes := make([]*agentpb.ExecuteOutcome, 0, len(waits))
	for _, w := range waits {
		select {
		case got := <-w.answer:
			outcomes = append(outcomes, bounded(w.server, got))
		case <-deadline.Done():
			w.forget()
			outcomes = append(outcomes, &agentpb.ExecuteOutcome{
				Server: w.server, Error: fmt.Sprintf("no answer within %s", wait),
			})
		}
	}
	slices.SortFunc(outcomes, func(a, b *agentpb.ExecuteOutcome) int { return strings.Compare(a.GetServer(), b.GetServer()) })
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_Execute{Execute: &agentpb.ExecuteResult{Outcomes: outcomes}},
	}
}

// executeSwitch reads the first Network, as netstate does: the Network
// controller refuses a second one per namespace.
func (s *Server) executeSwitch(ctx context.Context, namespace string) (string, bool, error) {
	var networks spawneryv1alpha1.NetworkList
	if err := s.opts.State.Reader.List(ctx, &networks, client.InNamespace(namespace)); err != nil {
		return "", false, err
	}
	if len(networks.Items) == 0 {
		return "", false, nil
	}
	return networks.Items[0].Name, networks.Items[0].ExecuteEnabled(), nil
}

// executeTargets is the server of that name, else the group's Ready servers.
// single reports the first case.
func (s *Server) executeTargets(ctx context.Context, namespace, target string) ([]spawneryv1alpha1.Server, bool, error) {
	var list spawneryv1alpha1.ServerList
	if err := s.opts.State.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, false, err
	}
	for i := range list.Items {
		if list.Items[i].Name == target {
			return list.Items[i : i+1], true, nil
		}
	}
	var members []spawneryv1alpha1.Server
	for _, srv := range list.Items {
		if srv.Spec.GroupRef.Name == target && phase.Phase(srv.Status.Phase) == phase.Ready {
			members = append(members, srv)
		}
	}
	if len(members) == 0 {
		return nil, false, errNoTarget
	}
	return members, false, nil
}

// bounded applies the agent's limits again: the operator does not trust a
// backend to have kept them.
func bounded(server string, got *agentpb.ExecuteOutcome) *agentpb.ExecuteOutcome {
	out := &agentpb.ExecuteOutcome{
		Server: server,
		Ok:     got.GetOk(),
		Error:  clip(got.GetError(), ExecuteMaxLineLength),
	}
	for i, line := range got.GetOutput() {
		if i == ExecuteMaxLines {
			break
		}
		out.Output = append(out.Output, clip(line, ExecuteMaxLineLength))
	}
	return out
}
```

- [ ] **Step 6: The receive paths.** In `server.go`'s `handle`, a new case:

```go
	case *agentpb.ServerMessage_ExecuteOutcome:
		s.executions.deliver(id.PodUID, m.ExecuteOutcome)
```

In `ProxySession`, before the loop: `late := make(chan *agentpb.OperatorToProxy)`. Pass it to `handleProxy` (new last parameter `late chan<- *agentpb.OperatorToProxy`), and add a case to the loop's `select`:

```go
		case answer := <-late:
			if err := sendBounded(SendDeadline, "an answer", func() error { return stream.Send(answer) }); err != nil {
				return err
			}
```

In `handleProxy`, the `CloudRequest` case becomes:

```go
	case *agentpb.ProxyMessage_CloudRequest:
		if m.CloudRequest.GetExecute() != nil {
			// Waits on servers for up to ExecuteWait; inline, it would hold this
			// proxy's outbox as long.
			go func() {
				answer := &agentpb.OperatorToProxy{
					Message: &agentpb.OperatorToProxy_CloudResponse{
						CloudResponse: s.answerCloudRequest(ctx, logger, id, m.CloudRequest),
					},
				}
				select {
				case late <- answer:
				case <-ctx.Done():
				}
			}()
			return nil
		}
		return &agentpb.OperatorToProxy{
			Message: &agentpb.OperatorToProxy_CloudResponse{
				CloudResponse: s.answerCloudRequest(ctx, logger, id, m.CloudRequest),
			},
		}
```

In `internal/serverreg/registry.go`, after `SetInterest`:

```go
// Send queues one message for one session. False when the pod has no live
// session, or when the message cut a session that had fallen behind.
func (r *Registry) Send(podUID string, msg *agentpb.OperatorToServer) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[podUID]
	if !ok || s.closed {
		return false
	}
	r.send(s, msg)
	return !s.closed
}
```

In `cmd/spawnery-operator/main.go`'s `agentserver.Options`, add `Recorder: mgr.GetEventRecorder("agentserver"),`. A plain recorder and not `cloudevent.Recorder`: a `CommandExecuted` event carries the command, and the chat feed reaches every backend that asked for events.

- [ ] **Step 7: Run the unit tests**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/agentserver/ ./internal/serverreg/ -run 'Scale|ForceStop|Execute|Answer|Bounded|Send|SevenDays|OneServer' -count=1 -race`
Expected: PASS.

- [ ] **Step 8: Write the envtest** `internal/agentserver/execute_envtest_test.go` (package `agentserver_test`, beside `boost_envtest_test.go`; reuse `newServerFixture`, `f.pod`, `f.proxyPod`, `f.token`, `dialAgent`, `dialProxy`):

```go
// The real session code: an execute waits off the proxy's loop, so a status
// request sent after it is answered first, while the server has not replied.
func TestAnExecuteWaitsWithoutHoldingTheProxysOtherAnswers(t *testing.T) {
	f := newServerFixture(t)
	if err := f.c.Create(f.ctx, &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "secret"},
			Commands:            &spawneryv1alpha1.NetworkCommands{Execute: true},
		},
	}); err != nil {
		t.Fatalf("create Network: %v", err)
	}
	pod := f.pod("lobby-aaaa")
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-aaaa", Namespace: f.ns},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create Server: %v", err)
	}
	srv.Status = spawneryv1alpha1.ServerStatus{Phase: "Ready", PodName: pod.Name, PodUID: string(pod.UID)}
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("Server status: %v", err)
	}

	server, closeServer := dialAgent(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeServer()
	proxy, closeProxy := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, f.proxyPod("gateway-0")))
	defer closeProxy()

	// The server's session has to exist before the command can reach it.
	waitFor(t, func() bool { return f.agents.Lookup(string(pod.UID)).Connected })

	send := func(id uint64, req *agentpb.CloudRequest) {
		req.Id = id
		if err := proxy.Send(&agentpb.ProxyMessage{Message: &agentpb.ProxyMessage_CloudRequest{CloudRequest: req}}); err != nil {
			t.Fatalf("send %d: %v", id, err)
		}
	}
	send(1, &agentpb.CloudRequest{Request: &agentpb.CloudRequest_Execute{Execute: &agentpb.ExecuteRequest{
		Target: "lobby-aaaa", Command: "list", Issuer: "alice"}}})
	send(2, &agentpb.CloudRequest{Request: &agentpb.CloudRequest_Status{Status: &agentpb.StatusRequest{}}})

	var command *agentpb.ExecuteCommand
	for command == nil {
		msg, err := server.Recv()
		if err != nil {
			t.Fatalf("server Recv: %v", err)
		}
		command = msg.GetExecuteCommand()
	}

	var order []uint64
	answered := func() {
		for {
			msg, err := proxy.Recv()
			if err != nil {
				t.Fatalf("proxy Recv: %v", err)
			}
			if resp := msg.GetCloudResponse(); resp != nil {
				order = append(order, resp.GetId())
				if resp.GetId() == 1 {
					outcomes := resp.GetExecute().GetOutcomes()
					if len(outcomes) != 1 || !outcomes[0].GetOk() || outcomes[0].GetOutput()[0] != "nobody" {
						t.Errorf("execute answer = %+v, want lobby-aaaa ok with its line", resp)
					}
				}
				return
			}
		}
	}
	answered()
	if err := server.Send(&agentpb.ServerMessage{Message: &agentpb.ServerMessage_ExecuteOutcome{
		ExecuteOutcome: &agentpb.ExecuteOutcome{Id: command.GetId(), Ok: true, Output: []string{"nobody"}},
	}}); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	answered()

	if len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Errorf("answers arrived in order %v, want the status (2) before the execute (1)", order)
	}
}
```

Check `waitFor`, `dialProxy` and `f.agents.Lookup(...).Connected` against `server_envtest_test.go` and `proxy_envtest_test.go`; adapt names if they differ.

- [ ] **Step 9: Run the package and the operator build**

Run: `NIX develop /home/paul/git/spawnery-cloud -c go test ./internal/agentserver/ ./internal/serverreg/ -count=1`
Run: `NIX develop /home/paul/git/spawnery-cloud -c go build ./...`
Expected: PASS and a clean build.

- [ ] **Step 10: Commit**

```bash
git add internal/agentserver internal/serverreg cmd/spawnery-operator
git commit -m "feat(agentserver): answer scale, force-stop and execute" \
  -m "<body: pins bounded by maxReplicas and 7 days, force-stop and execute proxy-only, execute behind spec.commands.execute, fanned out and collected for 8 s off the proxy's session loop, outcomes bounded again; events ForceStopped and CommandExecuted on a plain recorder, not the chat feed>"
```

---

### Task 7: The Paper agent runs the command

**Files:**
- Create: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/CommandRun.kt`
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/ServerRole.kt` (constructor line 20-26, `onMessage` lines 56-79)
- Modify: `agent/paper/src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt` (`role` at line 64; new `execute` method)
- Test: `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/CommandRunTest.kt`, `agent/paper/src/test/kotlin/cloud/spawnery/agent/paper/ServerRoleTest.kt`

**Interfaces:**
- Consumes: `ExecuteCommand`, `ExecuteOutcome`, `ServerMessage.setExecuteOutcome` (Task 2).
- Produces: `internal class CommandOutput(maxLines: Int = 20, maxChars: Int = 256)` with `add(Component)`, `addText(String)`, `lines(): List<String>`; `internal fun runCommand(command: ExecuteCommand, dispatch: (String, (Component) -> Unit) -> Boolean): ExecuteOutcome`; `ServerRole(..., execute: (ExecuteCommand) -> Unit = {})`.

`org.bukkit.Server.createCommandSender(Consumer<? super Component>)` is present in the pinned paper-api (`26.2.build.119`); its sender holds the console's permissions.

- [ ] **Step 1: Write the failing tests** `CommandRunTest.kt`:

```kotlin
package cloud.spawnery.agent.paper

import cloud.spawnery.agent.pb.ExecuteCommand
import net.kyori.adventure.text.Component
import net.kyori.adventure.text.format.NamedTextColor
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class CommandRunTest {
    private fun command(text: String) = ExecuteCommand.newBuilder().setId(42).setCommand(text).build()

    @Test
    fun `feedback arrives as plain text`() {
        val output = CommandOutput()
        output.add(Component.text("There are ").append(Component.text("0", NamedTextColor.RED)).append(Component.text(" players")))
        assertEquals(listOf("There are 0 players"), output.lines())
    }

    @Test
    fun `legacy colour codes are stripped and tags are left as text`() {
        val output = CommandOutput()
        output.addText("§aGreen §LBold §x§f§f§0§0§0§0hex")
        output.addText("<click:run_command:/op mallory>press</click>")
        assertEquals(listOf("Green Bold hex", "<click:run_command:/op mallory>press</click>"), output.lines())
    }

    @Test
    fun `output is bounded in lines and in characters`() {
        val output = CommandOutput()
        output.addText((1..25).joinToString("\n") { "line $it" })
        output.addText("x".repeat(300))
        assertEquals(20, output.lines().size)
        assertEquals("line 20", output.lines().last())

        val long = CommandOutput()
        long.addText("x".repeat(300))
        assertEquals(256, long.lines().single().length)
    }

    @Test
    fun `a known command is ok and carries its feedback and id`() {
        val outcome = runCommand(command("list")) { line, feedback ->
            assertEquals("list", line)
            feedback(Component.text("There are 0 of a max of 20 players online"))
            true
        }
        assertTrue(outcome.ok)
        assertEquals(42L, outcome.id)
        assertEquals(listOf("There are 0 of a max of 20 players online"), outcome.outputList)
    }

    @Test
    fun `an unknown command is not ok`() {
        val outcome = runCommand(command("nonsense")) { _, _ -> false }
        assertFalse(outcome.ok)
        assertEquals("unknown command", outcome.error)
    }

    @Test
    fun `a command that throws is not ok and says why`() {
        val outcome = runCommand(command("boom")) { _, feedback ->
            feedback(Component.text("before the throw"))
            throw IllegalStateException("plugin broke")
        }
        assertFalse(outcome.ok)
        assertEquals("plugin broke", outcome.error)
        assertEquals(listOf("before the throw"), outcome.outputList)
    }
}
```

In `ServerRoleTest.kt`:

```kotlin
    @Test
    fun `an execute command goes to the executor and changes nothing else`() {
        val handed = mutableListOf<Long>()
        val role = ServerRole(ServerState(), NetworkMirror(), dormantConnector(), aFeed(), CloudEvents()) { handed += it.id }

        val directive = role.onMessage(
            OperatorToServer.newBuilder()
                .setExecuteCommand(cloud.spawnery.agent.pb.ExecuteCommand.newBuilder().setId(9).setCommand("list"))
                .build(),
        )

        assertEquals(Directive.None, directive)
        assertEquals(listOf(9L), handed)
    }
```

- [ ] **Step 2: Run and see them fail**

Run: `git add -A && NIX build /home/paul/git/spawnery-cloud#agents --no-link -L 2>&1 | tail -30`
Expected: compile failure, `Unresolved reference: CommandOutput`.

- [ ] **Step 3: Implement** `CommandRun.kt`:

```kotlin
package cloud.spawnery.agent.paper

import cloud.spawnery.agent.pb.ExecuteCommand
import cloud.spawnery.agent.pb.ExecuteOutcome
import net.kyori.adventure.text.Component
import net.kyori.adventure.text.serializer.plain.PlainTextComponentSerializer

internal const val OUTPUT_MAX_LINES = 20
internal const val OUTPUT_MAX_CHARS = 256

/** One console command's feedback, as text a proxy can show in chat. */
internal class CommandOutput(
    private val maxLines: Int = OUTPUT_MAX_LINES,
    private val maxChars: Int = OUTPUT_MAX_CHARS,
) {
    private val lines = mutableListOf<String>()

    fun add(message: Component) = addText(PlainTextComponentSerializer.plainText().serialize(message))

    /** Plugins still write `§` codes into text components; in chat they are noise. */
    fun addText(text: String) {
        for (line in text.split('\n')) {
            if (lines.size >= maxLines) return
            lines += LEGACY_CODE.replace(line, "").take(maxChars)
        }
    }

    fun lines(): List<String> = lines.toList()

    private companion object {
        val LEGACY_CODE = Regex("§[0-9a-fk-orx]", RegexOption.IGNORE_CASE)
    }
}

/**
 * Feedback a command sends after [dispatch] returns, from a later tick or
 * another thread, is not in the outcome.
 */
internal fun runCommand(
    command: ExecuteCommand,
    dispatch: (String, (Component) -> Unit) -> Boolean,
): ExecuteOutcome {
    val output = CommandOutput()
    val outcome = ExecuteOutcome.newBuilder().setId(command.id)
    try {
        val known = dispatch(command.command) { output.add(it) }
        outcome.setOk(known)
        if (!known) outcome.setError("unknown command")
    } catch (failure: Exception) {
        outcome.setOk(false).setError(failure.message ?: failure.javaClass.simpleName)
    }
    return outcome.addAllOutput(output.lines()).build()
}
```

`ServerRole.kt`: add the constructor parameter last, `private val execute: (ExecuteCommand) -> Unit = {},` (import `cloud.spawnery.agent.pb.ExecuteCommand`), and in `onMessage` before `else`:

```kotlin
            OperatorToServer.MessageCase.EXECUTE_COMMAND -> {
                execute(message.executeCommand)
                Directive.None
            }
```

`AgentPlugin.kt`: `private val role = ServerRole(state, mirror, connector, feed, events, ::execute)` and:

```kotlin
    /** Bukkit runs commands on the main thread only; the answer goes back on the session. */
    private fun execute(command: ExecuteCommand) {
        server.scheduler.runTask(this, Runnable {
            val outcome = runCommand(command) { line, feedback ->
                server.dispatchCommand(server.createCommandSender { feedback(it) }, line)
            }
            loop?.send(ServerMessage.newBuilder().setExecuteOutcome(outcome).build())
        })
    }
```

- [ ] **Step 4: Run and see it pass**

Run: `git add -A && NIX build /home/paul/git/spawnery-cloud#agents --no-link -L 2>&1 | grep -E 'CommandRunTest|ServerRoleTest|FAILED|BUILD' | tail -20`
Expected: every `CommandRunTest` and `ServerRoleTest` case PASSED, the build succeeds.

- [ ] **Step 5: Commit**

```bash
git add agent/paper
git commit -m "feat(agent): a Paper server runs an operator's console command" \
  -m "<body: main thread, a command sender with console permissions, feedback as plain text with legacy codes stripped, 20 lines of 256>"
```

---

### Task 8: The connector and the plugin API

**Files:**
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/ScaleResult.java`
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/SpawneryApi.java` (after `stopBoosts`, line 160; `@Deprecated` on `boost` line 152 and `stopBoosts` line 160)
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/Group.java`
- Modify: `agent/api/src/test/java/cloud/spawnery/agent/api/FakeApi.java`, `.../RecordCompatibilityTest.java`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudConnector.kt`, `MirrorApi.kt`, `NetworkMirror.kt` (Group construction at lines 43-54)
- Create: `agent/common/src/main/kotlin/cloud/spawnery/agent/Execute.kt` (data types only in this task)
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudConnectorTest.kt`, `NetworkMirrorTest.kt`

**Interfaces:**
- Consumes: Task 2's messages.
- Produces: `public record ScaleResult(int replicas, Instant expiresAt)`; `SpawneryApi.scale(String group, int replicas, Duration forHowLong): CompletionStage<ScaleResult>`; `SpawneryApi.resetScale(String group): CompletionStage<Integer>`; `Group` gains `boolean pinned`, `int pinnedReplicas`, `Instant pinnedUntil` (null without a pin or without an end) with an 8-argument compatibility constructor; `data class ExecuteLine(val server: String, val ok: Boolean, val output: List<String>, val error: String)`; `CloudConnector.scale(group, replicas, forHowLong): CompletionStage<ScaleResult>`, `resetScale(group): CompletionStage<Int>`, `forceStop(server, issuer): CompletionStage<String>`, `execute(target, command, issuer): CompletionStage<List<ExecuteLine>>`.

- [ ] **Step 1: Write the failing tests.** In `CloudConnectorTest.kt` (alias the wire messages: `import cloud.spawnery.agent.pb.ScaleResult as PbScaleResult`, plus `ExecuteResult`, `ExecuteOutcome`, `ForceStopResult`):

```kotlin
    private fun answer(connector: CloudConnector, build: CloudResponse.Builder.() -> Unit) =
        connector.answer(CloudResponse.newBuilder().setId(requested.last().id).apply(build).build())

    @Test
    fun `scale sends a scale request and reads the pin back`() {
        val connector = connector()
        val stage = connector.scale("lobby", 0, Duration.ofDays(2))

        assertEquals("lobby", requested.single().scale.group)
        assertEquals(0, requested.single().scale.replicas)
        assertEquals(172_800L, requested.single().scale.durationSeconds)
        answer(connector) { setScale(PbScaleResult.newBuilder().setReplicas(0).setExpiresAtUnix(1_800_000_000)) }

        val result = stage.toCompletableFuture().get(1, TimeUnit.SECONDS)
        assertEquals(0, result.replicas())
        assertEquals(java.time.Instant.ofEpochSecond(1_800_000_000), result.expiresAt())
    }

    @Test
    fun `reset is the stop-boost request`() {
        val connector = connector()
        val stage = connector.resetScale("lobby")
        assertEquals("lobby", requested.single().stopBoost.group)
        answer(connector) { setStopBoost(cloud.spawnery.agent.pb.StopBoostResult.newBuilder().setRemoved(2)) }
        assertEquals(2, stage.toCompletableFuture().get(1, TimeUnit.SECONDS))
    }

    @Test
    fun `force-stop and execute carry who typed them`() {
        val connector = connector()
        val stopped = connector.forceStop("lobby-a", "alice")
        assertEquals("alice", requested.last().forceStop.issuer)
        answer(connector) { setForceStop(ForceStopResult.newBuilder().setServer("lobby-a")) }
        assertEquals("lobby-a", stopped.toCompletableFuture().get(1, TimeUnit.SECONDS))

        val ran = connector.execute("lobby", "list", "console")
        assertEquals("console", requested.last().execute.issuer)
        assertEquals("list", requested.last().execute.command)
        answer(connector) {
            setExecute(
                ExecuteResult.newBuilder()
                    .addOutcomes(ExecuteOutcome.newBuilder().setServer("lobby-a").setOk(true).addOutput("hi"))
                    .addOutcomes(ExecuteOutcome.newBuilder().setServer("lobby-b").setError("no answer within 8s")),
            )
        }
        assertEquals(
            listOf(ExecuteLine("lobby-a", true, listOf("hi"), ""), ExecuteLine("lobby-b", false, emptyList(), "no answer within 8s")),
            ran.toCompletableFuture().get(1, TimeUnit.SECONDS),
        )
    }
```

In `NetworkMirrorTest.kt`:

```kotlin
    @Test
    fun `a pinned group says to what and until when`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addGroups(GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                    .setPinned(true).setPinnedReplicas(0).setPinnedUntilUnix(1_800_000_000))
                .addGroups(GroupState.newBuilder().setName("arena").setKind(GroupState.Kind.EPHEMERAL))
                .build(),
        )
        val lobby = mirror.groups().single { it.name() == "lobby" }
        assertTrue(lobby.pinned())
        assertEquals(0, lobby.pinnedReplicas())
        assertEquals(java.time.Instant.ofEpochSecond(1_800_000_000), lobby.pinnedUntil())
        assertFalse(mirror.groups().single { it.name() == "arena" }.pinned())
    }
```

In `RecordCompatibilityTest.java`:

```java
    @Test
    void theZeroEighteenGroupConstructorStillBuildsAndReadsAsUnpinned() {
        Group group = new Group("lobby", Group.Kind.EPHEMERAL, 1, 1, 0, 100, Map.of(), "Lobby");
        assertEquals(false, group.pinned());
        assertEquals(0, group.pinnedReplicas());
        assertEquals(null, group.pinnedUntil());
    }
```

- [ ] **Step 2: Run and see them fail**

Run: `git add -A && NIX build /home/paul/git/spawnery-cloud#agents --no-link -L 2>&1 | tail -30`
Expected: compile failure, `Unresolved reference: scale`.

- [ ] **Step 3: The API.** `ScaleResult.java` (copy the licence header from `BoostResult.java`):

```java
package cloud.spawnery.agent.api;

import java.time.Instant;
import java.util.Objects;

/**
 * The pin a {@link SpawneryApi#scale} call created.
 *
 * <p><b>{@code expiresAt} is the operator's clock, not yours</b>; it is good
 * for telling a person when the pin ends.
 *
 * @param replicas the size the group is now held at.
 */
public record ScaleResult(int replicas, Instant expiresAt) {
    public ScaleResult {
        Objects.requireNonNull(expiresAt, "expiresAt");
    }
}
```

`SpawneryApi.java`, after `stopBoosts`:

```java
    /**
     * Holds an ephemeral group at exactly this many servers for a while.
     *
     * <p>Unlike {@link #boost}, which adds to the floor, a pin is both the
     * floor and the ceiling: servers above the number are drained and
     * removed, 0 included. The group's own {@code maxReplicas} still binds; a
     * pin above it is refused. Of several pins on one group the newest wins,
     * and boosts do not count while one holds.
     *
     * <p>The stage fails when the operator refuses: a group it does not have,
     * a group that is not ephemeral, more than {@code maxReplicas}, or longer
     * than seven days. Each says which.
     *
     * @param forHowLong how long the pin holds, or {@code null} for the
     *     operator's default of an hour.
     */
    CompletionStage<ScaleResult> scale(String group, int replicas, Duration forHowLong);

    /**
     * Ends every pin and every boost on a group and reports how many there
     * were. Zero is an ordinary answer.
     */
    CompletionStage<Integer> resetScale(String group);
```

On `boost` and `stopBoosts`: add `@Deprecated` and a Javadoc line `@deprecated use {@link #scale}, which sets the size rather than adding to it; this keeps working.` (for `stopBoosts`: `use {@link #resetScale}`).

`Group.java`: extend the record and keep the old shape:

```java
public record Group(
        String name,
        Kind kind,
        int replicas,
        int readyReplicas,
        int onlinePlayers,
        int freeSlots,
        Map<String, String> attributes,
        String displayName,
        boolean pinned,
        int pinnedReplicas,
        Instant pinnedUntil) {
    public Group {
        Objects.requireNonNull(name, "name");
        Objects.requireNonNull(kind, "kind");
        attributes = attributes == null ? Map.of() : Map.copyOf(attributes);
        // Here and not in the operator, so an operator that predates the field
        // reads like one that left it out.
        displayName = displayName == null || displayName.isEmpty() ? name : displayName;
        if (!pinned) {
            pinnedReplicas = 0;
            pinnedUntil = null;
        }
    }

    /** The 0.18 shape: no pin. */
    public Group(
            String name,
            Kind kind,
            int replicas,
            int readyReplicas,
            int onlinePlayers,
            int freeSlots,
            Map<String, String> attributes,
            String displayName) {
        this(name, kind, replicas, readyReplicas, onlinePlayers, freeSlots, attributes, displayName, false, 0, null);
    }
    // … Kind unchanged
```

Add `import java.time.Instant;` and `@param` lines to the record's Javadoc for the three new components: `pinned` (an Exact ScaleBoost holds the group), `pinnedReplicas` (the size it holds, 0 without a pin), `pinnedUntil` (when it ends on the operator's clock, null for a pin without an end and without a pin).

`FakeApi.java`: add `scale` and `resetScale` returning `CompletableFuture.failedFuture(new UnsupportedOperationException("fake"))`, as its neighbours do.

- [ ] **Step 4: The connector and the mirror.** `Execute.kt`:

```kotlin
package cloud.spawnery.agent

/** One server's answer to `/cloud execute`. */
data class ExecuteLine(val server: String, val ok: Boolean, val output: List<String>, val error: String)
```

In `CloudConnector.kt` (imports `cloud.spawnery.agent.api.ScaleResult`, `cloud.spawnery.agent.pb.ScaleRequest`, `ForceStopRequest`, `ExecuteRequest`):

```kotlin
    /** The same duration rule as [boost]. */
    fun scale(group: String, replicas: Int, forHowLong: Duration?): CompletionStage<ScaleResult> =
        requests.start<ScaleResult> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setScale(
                        ScaleRequest.newBuilder()
                            .setGroup(group)
                            .setReplicas(replicas)
                            .setDurationSeconds(forHowLong?.seconds ?: 0L),
                    )
                    .build(),
            )
        }

    /** The operator's StopBoostRequest removes pins and boosts alike. */
    fun resetScale(group: String): CompletionStage<Int> = stopBoosts(group)

    fun forceStop(server: String, issuer: String): CompletionStage<String> =
        requests.start<String> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setForceStop(ForceStopRequest.newBuilder().setServer(server).setIssuer(issuer))
                    .build(),
            )
        }

    fun execute(target: String, command: String, issuer: String): CompletionStage<List<ExecuteLine>> =
        requests.start<List<ExecuteLine>> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setExecute(ExecuteRequest.newBuilder().setTarget(target).setCommand(command).setIssuer(issuer))
                    .build(),
            )
        }
```

In `answer`, before the `else` branch:

```kotlin
            response.hasScale() -> requests.complete(
                response.id,
                ScaleResult(response.scale.replicas, Instant.ofEpochSecond(response.scale.expiresAtUnix)),
            )
            response.hasForceStop() -> requests.complete(response.id, response.forceStop.server)
            response.hasExecute() -> requests.complete(
                response.id,
                response.execute.outcomesList.map { ExecuteLine(it.server, it.ok, it.outputList, it.error) },
            )
```

`MirrorApi.kt` (import `cloud.spawnery.agent.api.ScaleResult`):

```kotlin
    override fun scale(group: String, replicas: Int, forHowLong: Duration?): CompletionStage<ScaleResult> =
        connector.scale(group, replicas, forHowLong)

    override fun resetScale(group: String): CompletionStage<Int> =
        connector.resetScale(group)
```

`NetworkMirror.kt`, the `Group(…)` call gains three arguments after `it.displayName`:

```kotlin
                    it.pinned,
                    it.pinnedReplicas,
                    if (it.pinned && it.pinnedUntilUnix > 0) Instant.ofEpochSecond(it.pinnedUntilUnix) else null,
```

(import `java.time.Instant` if missing).

- [ ] **Step 5: Run and see it pass**

Run: `git add -A && NIX build /home/paul/git/spawnery-cloud#agents --no-link -L 2>&1 | grep -E 'CloudConnectorTest|NetworkMirrorTest|RecordCompatibilityTest|FAILED|BUILD' | tail -20`
Expected: all passed. (`CloudCommandTest` still compiles: `start` and `stop` call `boost` and `stopBoosts`, which still exist.)

- [ ] **Step 6: Commit**

```bash
git add agent/api agent/common
git commit -m "feat(agent): scale, resetScale and a pinned Group in the plugin API" \
  -m "<body: ScaleResult; boost and stopBoosts deprecated and still working; Group keeps its 0.18 constructor; connector verbs for force-stop and execute>"
```

---

### Task 9: The command tree

**Files:**
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/CloudCommand.kt` (constants lines 17-28, `cloudCommand` lines 36-270, `startBoost` lines 340-373, `AT_MINUTE_UTC` line 375, `parseDuration` lines 379-389)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/Execute.kt` (`ProxyCommands`, `executeLines`)
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/ListLines.kt` (`groupInfoLines`, lines 98-138)
- Modify: `agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt` (line 196)
- Test: `agent/common/src/test/kotlin/cloud/spawnery/agent/CloudCommandTest.kt`, `ExecuteLinesTest.kt` (new)

**Interfaces:**
- Consumes: `CloudConnector.forceStop/execute`, `ExecuteLine`, `SpawneryApi.scale/resetScale`, `Group.pinned()/pinnedReplicas()/pinnedUntil()` (Task 8).
- Produces: `const val PERMISSION_FORCESTOP = "spawnery.cloud.forcestop"`, `const val PERMISSION_EXECUTE = "spawnery.cloud.execute"`; `class ProxyCommands<S>(val connector: CloudConnector, val issuer: (S) -> String)`; `fun <S> cloudCommand(api, adapter, feed, format = …, proxy: ProxyCommands<S>? = null)`; `internal fun executeLines(target: String, outcomes: List<ExecuteLine>): List<String>`; `internal val AT_MINUTE_UTC`; `parseDuration` accepts `d`.

The replies the e2e test (Task 11) reads, exactly: a pin `<group> is pinned to <n> server(s) until HH:mm UTC`; force-stop `<server> is being killed.`; execute on one server `<server> ran it`; on a group `<ok> of <total> servers ran it`. Keep this wording.

- [ ] **Step 1: Rewrite the tests that name `start` and `stop`** in `CloudCommandTest.kt`. Import `cloud.spawnery.agent.pb.ScaleResult as PbScaleResult`, `ForceStopResult`, `ExecuteResult`, `ExecuteOutcome`. Replace:

- `every reply wears it, not only the first`: `run("cloud scale lobby 2")`, answer `setScale(PbScaleResult.newBuilder().setReplicas(2).setExpiresAtUnix(Instant.parse("2026-08-30T20:00:00Z").epochSecond))`, expect `2` lines.
- `the tree asks the platform for nothing but the permissions it declares`: `run("cloud scale lobby 1")` and `run("cloud scale lobby reset")` in place of start/stop; the expected set is unchanged.
- `start says what it created, …` becomes:

```kotlin
    @Test
    fun `scale says what it pinned, until when, and how to end it`() {
        run("cloud scale lobby 0 for 2d")

        val request = requested.single().scale
        assertEquals("lobby", request.group)
        assertEquals(0, request.replicas)
        assertEquals(172_800L, request.durationSeconds)

        answer {
            setScale(PbScaleResult.newBuilder().setReplicas(0)
                .setExpiresAtUnix(java.time.Instant.parse("2026-10-06T18:00:00Z").epochSecond))
        }

        assertEquals(2, sent.size, sent.toString())
        assertTrue(plain(sent[0]).contains("lobby is pinned to 0 servers until 18:00 UTC"), sent[0])
        assertTrue(plain(sent[1]).contains("/cloud scale lobby reset"), sent[1])
    }
```

- `start without a count asks for one` becomes `scale without a duration leaves it to the operator`: `run("cloud scale lobby 3")`, assert `replicas == 3`, `durationSeconds == 0L`.
- `an unreadable duration is named rather than silently defaulted`: `run("cloud scale lobby 2 for 2hh")`.
- `stop says how many it removed` → `run("cloud scale lobby reset")`, `requested.single().stopBoost.group == "lobby"`, answer removed 2, `plain(sent.single()).contains("removed 2")`.
- `stopping a group with no boosts says so plainly` → `cloud scale lobby reset`, removed 0, `contains("had no pin or boost")`.
- `scaling is invisible without its own permission`: `cloud scale lobby 1` and `cloud scale lobby reset` throw `CommandSyntaxException`.
- `holding only scale still opens the root`: `run("cloud scale lobby 1")`, `requested.single().scale.group == "lobby"`.
- In `CloudCompletionTest`, `start and stop offer groups and no server` becomes `scale offers groups and no server`: `completions("cloud scale ")` is `["bingo", "lobby"]`.

New tests in `CloudCommandTest`:

```kotlin
    @Test
    fun `start and stop are gone`() {
        assertFailsWith<CommandSyntaxException> { run("cloud start lobby 2") }
        assertFailsWith<CommandSyntaxException> { run("cloud stop lobby") }
    }

    @Test
    fun `durations at the edges are named, never sent and never thrown`() {
        for (text in listOf("0d", "0m", "99999999999999999d", "-1h", "5w")) {
            sent.clear()
            run("cloud scale lobby 1 for $text")
            assertTrue(requested.isEmpty(), "$text reached the operator: $requested")
            assertTrue(sent.single().contains(text), "the answer did not name $text: $sent")
        }
        run("cloud scale lobby 1 for 8d")
        assertEquals(691_200L, requested.single().scale.durationSeconds, "8d is the operator's to refuse")
    }

    @Test
    fun `a negative count is a syntax error`() {
        assertFailsWith<CommandSyntaxException> { run("cloud scale lobby -1") }
    }

    @Test
    fun `info on a pinned group says to what and until when`() {
        val state = NetworkState.newBuilder()
            .addGroups(GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                .setPinned(true).setPinnedReplicas(0)
                .setPinnedUntilUnix(java.time.Instant.parse("2026-10-04T18:00:00Z").epochSecond))
            .build()

        run("cloud info lobby", api(state))

        assertTrue(sent.any { plain(it).contains("Pinned") && plain(it).contains("0 servers until 18:00 UTC") }, "$sent")
    }

    private val proxyCommands = ProxyCommands<Int>(connector) { "alice" }

    private fun runOnProxy(command: String): Int {
        val dispatcher = CommandDispatcher<Int>()
        dispatcher.register(cloudCommand(api(), adapter, feed, { format }, proxyCommands))
        return dispatcher.execute(command, 0)
    }

    @Test
    fun `forcestop and execute exist only on a proxy`() {
        permissions = permissions + PERMISSION_FORCESTOP + PERMISSION_EXECUTE
        assertFailsWith<CommandSyntaxException> { run("cloud forcestop lobby-a") }
        assertFailsWith<CommandSyntaxException> { run("cloud execute lobby-a list") }
    }

    @Test
    fun `on a backend forcestop and execute alone do not open the root`() {
        permissions = setOf(PERMISSION_FORCESTOP, PERMISSION_EXECUTE)
        assertFailsWith<CommandSyntaxException> { run("cloud list") }
    }

    @Test
    fun `forcestop answers at once and names the issuer to the operator`() {
        permissions = setOf(PERMISSION_FORCESTOP)

        runOnProxy("cloud forcestop lobby-a")

        assertEquals("lobby-a", requested.single().forceStop.server)
        assertEquals("alice", requested.single().forceStop.issuer)
        answer { setForceStop(ForceStopResult.newBuilder().setServer("lobby-a")) }
        assertTrue(plain(sent.single()).contains("lobby-a is being killed."), sent.single())
    }

    @Test
    fun `execute sends the rest of the line without its slash`() {
        permissions = setOf(PERMISSION_EXECUTE)

        runOnProxy("cloud execute lobby /say hello world")

        val request = requested.single().execute
        assertEquals("lobby", request.target)
        assertEquals("say hello world", request.command)
        assertEquals("alice", request.issuer)
    }

    @Test
    fun `execute refuses a command longer than the operator carries before sending it`() {
        permissions = setOf(PERMISSION_EXECUTE)

        runOnProxy("cloud execute lobby " + "x".repeat(257))

        assertTrue(requested.isEmpty(), "an over-long command reached the operator")
        assertTrue(sent.single().contains("256"), sent.single())
    }

    @Test
    fun `execute without the network switch says so in the operator's words`() {
        permissions = setOf(PERMISSION_EXECUTE)
        runOnProxy("cloud execute lobby list")
        answer {
            setError(RequestError.newBuilder().setReason(RequestError.Reason.REFUSED)
                .setMessage("execute is not enabled on this network"))
        }
        assertTrue(sent.single().contains("execute is not enabled on this network"), sent.single())
    }

    @Test
    fun `forcestop and execute each need their own node`() {
        permissions = setOf(PERMISSION_READ)
        assertFailsWith<CommandSyntaxException> { runOnProxy("cloud forcestop lobby-a") }
        assertFailsWith<CommandSyntaxException> { runOnProxy("cloud execute lobby-a list") }
    }

    @Test
    fun `holding only execute opens the root on a proxy`() {
        permissions = setOf(PERMISSION_EXECUTE)
        runOnProxy("cloud execute lobby-a list")
        assertEquals("lobby-a", requested.single().execute.target)
    }
```

New file `ExecuteLinesTest.kt`:

```kotlin
package cloud.spawnery.agent

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class ExecuteLinesTest {
    @Test
    fun `one server shows its output under a line saying it ran`() {
        val lines = executeLines("lobby-a", listOf(ExecuteLine("lobby-a", true, listOf("There are 0 players"), "")))
        assertEquals(2, lines.size)
        assertTrue(lines[0].startsWith("<green>✔</green>") && plain(lines[0]).contains("lobby-a ran it"), lines[0])
        assertTrue(plain(lines[1]).contains("There are 0 players"), lines[1])
    }

    @Test
    fun `output reaches chat as text, never as markup`() {
        val lines = executeLines("lobby-a", listOf(ExecuteLine("lobby-a", true, listOf("<click:run_command:/op mallory>press"), "")))
        assertTrue(lines[1].contains("\\<click:run_command"), "a tag in output was left live: ${lines[1]}")
    }

    @Test
    fun `one server that failed says why`() {
        val lines = executeLines("lobby-a", listOf(ExecuteLine("lobby-a", false, emptyList(), "unknown command")))
        assertTrue(lines.single().startsWith("<red>✘</red>") && plain(lines.single()).contains("unknown command"), lines.single())
    }

    @Test
    fun `a group gets a line per server without output and a total`() {
        val lines = executeLines(
            "lobby",
            listOf(
                ExecuteLine("lobby-b", false, emptyList(), "no answer within 8s"),
                ExecuteLine("lobby-a", true, listOf("hidden"), ""),
            ),
        )
        assertEquals(3, lines.size)
        assertTrue(plain(lines[0]).contains("lobby-a") && plain(lines[0]).contains("ok"), lines[0])
        assertTrue(plain(lines[1]).contains("lobby-b") && plain(lines[1]).contains("no answer within 8s"), lines[1])
        assertTrue(lines.none { it.contains("hidden") }, "a group answer showed output: $lines")
        assertTrue(lines[2].startsWith("<red>✘</red>") && plain(lines[2]).contains("1 of 2 servers ran it"), lines[2])
    }
}
```

In `CloudCompletionTest`:

```kotlin
    private fun proxyCompletions(command: String): List<String> {
        val dispatcher = CommandDispatcher<Int>()
        val connector = CloudConnector(Requests(timeoutMillis = 1_000, clock = System::currentTimeMillis)) { }
        dispatcher.register(cloudCommand(api(), adapter, FeedState(), { Feed.MESSAGE_TOKEN }, ProxyCommands(connector) { "x" }))
        return dispatcher.getCompletionSuggestions(dispatcher.parse(command, 0)).join().list.map { it.text }
    }

    @Test
    fun `forcestop offers servers and execute offers servers and groups`() {
        assertEquals(listOf("bingo-x", "lobby-a"), proxyCompletions("cloud forcestop ").sorted())
        assertEquals(listOf("bingo", "bingo-x", "lobby", "lobby-a"), proxyCompletions("cloud execute ").sorted())
    }
```

- [ ] **Step 2: Run and see them fail**

Run: `git add -A && NIX build /home/paul/git/spawnery-cloud#agents --no-link -L 2>&1 | tail -30`
Expected: compile failure, `Unresolved reference: ProxyCommands`.

- [ ] **Step 3: `Execute.kt`**, appended:

```kotlin
/**
 * The verbs only a proxy sends. Paper builds the tree without it, so neither
 * `forcestop` nor `execute` exists there; the operator refuses both from a
 * backend as well.
 */
class ProxyCommands<S>(
    val connector: CloudConnector,
    /** For the operator's record: a player's name, or "console". */
    val issuer: (S) -> String,
)

/** For one server its output; for a group a line per server and a total, no output. */
internal fun executeLines(target: String, outcomes: List<ExecuteLine>): List<String> {
    val single = outcomes.singleOrNull()
    if (single != null && single.server == target) {
        if (!single.ok) {
            return listOf(Layout.fail(Style.name(single.server) + Style.quiet(": ") + Style.bad(single.error)))
        }
        return listOf(Layout.ok(Style.name(single.server) + Style.good(" ran it"))) +
            single.output.map { Layout.entry(Style.quiet(it)) }
    }
    val ran = outcomes.count { it.ok }
    val lines = outcomes.sortedBy { it.server }.map {
        Layout.entry(Layout.joined(Style.name(it.server), if (it.ok) Style.good("ok") else Style.bad(it.error)))
    }
    val total = Style.number(ran) + Style.quiet(" of ") + Style.number(outcomes.size) + Style.quiet(" servers ran it")
    return lines + if (ran == outcomes.size) Layout.ok(total) else Layout.fail(total)
}
```

- [ ] **Step 4: `CloudCommand.kt`.** Constants:

```kotlin
/** Covers pinning and resetting, so whoever holds a group at a size can let it go. */
const val PERMISSION_SCALE: String = "spawnery.cloud.scale"

/** Proxy only. Kills a pod without a drain. */
const val PERMISSION_FORCESTOP: String = "spawnery.cloud.forcestop"

/** Proxy only, and only on a network with spec.commands.execute. */
const val PERMISSION_EXECUTE: String = "spawnery.cloud.execute"

internal const val EXECUTE_MAX_COMMAND: Int = 256
```

`cloudCommand` gains the parameter `proxy: ProxyCommands<S>? = null` after `format` and becomes a block body: build the root as today into `val root = …`, then

```kotlin
    if (proxy != null) {
        root.then(forceStopBranch(api, adapter, format, proxy)).then(executeBranch(api, adapter, format, proxy))
    }
    return root
```

The root's `requires`:

```kotlin
        // Any branch's permission opens the root; the branches gate themselves.
        .requires { source -> rootNodes(proxy != null).any { adapter.hasPermission(source, it) } }
```

with

```kotlin
/** Read first, so a reader is asked one question, as before. */
private fun rootNodes(onProxy: Boolean): List<String> =
    listOf(PERMISSION_READ, PERMISSION_RETIRE, PERMISSION_SCALE, PERMISSION_STATUS, PERMISSION_EVENTS) +
        if (onProxy) listOf(PERMISSION_FORCESTOP, PERMISSION_EXECUTE) else emptyList()
```

Replace the `start` and `stop` branches with one `scale` branch:

```kotlin
        .then(
            LiteralArgumentBuilder.literal<S>("scale")
                .requires { adapter.hasPermission(it, PERMISSION_SCALE) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("group", StringArgumentType.word())
                        .suggests(suggesting { api.groups().filter { it.kind() == Group.Kind.EPHEMERAL }.map(Group::name) })
                        .then(
                            LiteralArgumentBuilder.literal<S>("reset")
                                .executes { ctx -> resetScale(api, adapter, format, ctx.source, group(ctx)) },
                        )
                        .then(
                            RequiredArgumentBuilder.argument<S, Int>("count", IntegerArgumentType.integer(0))
                                .executes { ctx -> pin(api, adapter, format, ctx.source, group(ctx), count(ctx), null) }
                                .then(
                                    LiteralArgumentBuilder.literal<S>("for")
                                        .then(
                                            RequiredArgumentBuilder.argument<S, String>(
                                                "duration",
                                                StringArgumentType.word(),
                                            ).executes { ctx ->
                                                val text = StringArgumentType.getString(ctx, "duration")
                                                val span = parseDuration(text)
                                                if (span == null) {
                                                    replyFail(adapter, format,
                                                        ctx.source,
                                                        Style.bad("could not read") + " " +
                                                            Style.name(text) +
                                                            Style.quiet(" as a length of time. Try ") +
                                                            Style.number("30m") + Style.quiet(", ") +
                                                            Style.number("2h") + Style.quiet(" or ") +
                                                            Style.number("3d") + Style.quiet("."),
                                                    )
                                                    return@executes 0
                                                }
                                                pin(api, adapter, format, ctx.source, group(ctx), count(ctx), span)
                                            },
                                        ),
                                ),
                        ),
                ),
        )
```

Replace `startBoost` with:

```kotlin
private fun <S> pin(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    format: () -> String,
    source: S,
    group: String,
    replicas: Int,
    forHowLong: java.time.Duration?,
): Int {
    api.scale(group, replicas, forHowLong).whenComplete { result, failure ->
        if (failure != null) {
            replyFail(adapter, format, source,
                Style.bad("could not scale") + " " + Style.name(group) + Style.quiet(": ") + Style.bad(reason(failure)))
            return@whenComplete
        }
        replyOk(adapter, format, source,
            Style.name(group) + Style.quiet(" is pinned to ") + Style.number(result.replicas()) +
                Style.quiet(if (result.replicas() == 1) " server" else " servers") +
                Style.quiet(" until ") + Style.number(AT_MINUTE_UTC.format(result.expiresAt())) + Style.quiet(" UTC"))
        reply(adapter, format, source,
            Style.quiet("It ends on its own; ") + Style.number("/cloud scale $group reset") + Style.quiet(" ends it early."))
    }
    return 1
}

private fun <S> resetScale(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    format: () -> String,
    source: S,
    group: String,
): Int {
    api.resetScale(group).whenComplete { removed, failure ->
        when {
            failure != null -> replyFail(adapter, format, source,
                Style.bad("could not reset") + " " + Style.name(group) + Style.quiet(": ") + Style.bad(reason(failure)))
            removed == 0 -> replyOk(adapter, format, source, Style.name(group) + Style.quiet(" had no pin or boost"))
            else -> replyOk(adapter, format, source,
                Style.name(group) + Style.quiet(": removed ") + Style.number(removed) +
                    Style.good(" pin${if (removed == 1) "" else "s"} and boosts.") +
                    Style.quiet(" The group returns to its own floor and ceiling."))
        }
    }
    return 1
}
```

Check the `removed 2` wording against the rewritten test (`plain(...).contains("removed 2")`).

The two proxy branches, below `resetScale`:

```kotlin
private fun <S> forceStopBranch(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    format: () -> String,
    proxy: ProxyCommands<S>,
): LiteralArgumentBuilder<S> =
    LiteralArgumentBuilder.literal<S>("forcestop")
        .requires { adapter.hasPermission(it, PERMISSION_FORCESTOP) }
        .then(
            RequiredArgumentBuilder.argument<S, String>("server", StringArgumentType.word())
                .suggests(suggesting { api.servers().map(ServerInfo::name) })
                .executes { ctx ->
                    val name = StringArgumentType.getString(ctx, "server")
                    val source = ctx.source
                    proxy.connector.forceStop(name, proxy.issuer(source)).whenComplete { _, failure ->
                        if (failure == null) {
                            replyOk(adapter, format, source,
                                Style.name(name) + Style.bad(" is being killed.") +
                                    Style.quiet(" /cloud info shows when its pod is gone."))
                        } else {
                            replyFail(adapter, format, source,
                                Style.bad("could not force-stop") + " " + Style.name(name) +
                                    Style.quiet(": ") + Style.bad(reason(failure)))
                        }
                    }
                    1
                },
        )

private fun <S> executeBranch(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    format: () -> String,
    proxy: ProxyCommands<S>,
): LiteralArgumentBuilder<S> =
    LiteralArgumentBuilder.literal<S>("execute")
        .requires { adapter.hasPermission(it, PERMISSION_EXECUTE) }
        .then(
            RequiredArgumentBuilder.argument<S, String>("target", StringArgumentType.word())
                .suggests(suggesting {
                    api.servers().map(ServerInfo::name) +
                        api.groups().filter { it.kind() != Group.Kind.PROXY }.map(Group::name)
                })
                .then(
                    RequiredArgumentBuilder.argument<S, String>("command", StringArgumentType.greedyString())
                        .executes { ctx ->
                            val target = StringArgumentType.getString(ctx, "target")
                            val command = StringArgumentType.getString(ctx, "command").trim().removePrefix("/")
                            val source = ctx.source
                            if (command.length > EXECUTE_MAX_COMMAND) {
                                replyFail(adapter, format, source,
                                    Style.bad("that command is ") + Style.number(command.length) +
                                        Style.bad(" characters; the operator carries at most ") +
                                        Style.number(EXECUTE_MAX_COMMAND))
                                return@executes 0
                            }
                            proxy.connector.execute(target, command, proxy.issuer(source)).whenComplete { outcomes, failure ->
                                if (failure != null) {
                                    replyFail(adapter, format, source,
                                        Style.bad("could not run it on") + " " + Style.name(target) +
                                            Style.quiet(": ") + Style.bad(reason(failure)))
                                } else {
                                    executeLines(target, outcomes).forEach { reply(adapter, format, source, it) }
                                }
                            }
                            1
                        },
                ),
        )
```

`AT_MINUTE_UTC` becomes `internal val`. `parseDuration`:

```kotlin
/** Not java.time's ISO-8601 parser: nobody types `PT30M` into a chat window. */
internal fun parseDuration(text: String): java.time.Duration? {
    if (text.length < 2) return null
    val amount = text.dropLast(1).toLongOrNull() ?: return null
    if (amount <= 0) return null
    return try {
        when (text.last()) {
            's' -> java.time.Duration.ofSeconds(amount)
            'm' -> java.time.Duration.ofMinutes(amount)
            'h' -> java.time.Duration.ofHours(amount)
            'd' -> java.time.Duration.ofDays(amount)
            else -> null
        }
    } catch (_: ArithmeticException) {
        null
    }
}
```

Remove the now unused `startBoost` and the `start`/`stop` branches entirely.

- [ ] **Step 5: `/cloud info` shows a pin.** In `ListLines.kt`'s `groupInfoLines`, in the non-proxy branch right after the `Players` field:

```kotlin
        if (g.pinned()) {
            val count = Style.number(g.pinnedReplicas()) + Style.quiet(if (g.pinnedReplicas() == 1) " server" else " servers")
            val until = g.pinnedUntil()
            lines += Layout.field(
                "Pinned",
                if (until == null) count + Style.quiet(", no end")
                else count + Style.quiet(" until ") + Style.number(AT_MINUTE_UTC.format(until)) + Style.quiet(" UTC"),
            )
        }
```

- [ ] **Step 6: Velocity wiring.** In the Velocity `AgentPlugin.kt`, replace line 196:

```kotlin
        val proxyCommands = ProxyCommands<CommandSource>(connector) { source -> (source as? Player)?.username ?: "console" }
        val command = BrigadierCommand(
            cloudCommand(api, VelocitySource, feedState, mirror::feedFormat, proxyCommands).build(),
        )
```

Import `cloud.spawnery.agent.ProxyCommands`, `com.velocitypowered.api.command.CommandSource` and `com.velocitypowered.api.proxy.Player` if absent. The Paper `AgentPlugin` keeps its call as it is.

- [ ] **Step 7: Run and see it pass**

Run: `git add -A && NIX build /home/paul/git/spawnery-cloud#agents --no-link -L 2>&1 | grep -E 'CloudCommandTest|CloudCompletionTest|ExecuteLinesTest|FAILED|BUILD' | tail -40`
Expected: all passed, both plugin jars built.

- [ ] **Step 8: Commit**

```bash
git add agent/common agent/velocity
git commit -m "feat(agent): /cloud scale, forcestop and execute; start and stop removed" \
  -m "<body: scale with reset and d durations, info shows a pin, forcestop and execute only in the Velocity tree with their own nodes, output escaped so a server cannot put markup into an admin's chat>"
```

---

### Task 10: Docs

**Files:**
- Modify: `docs/guides/cloud-command.md` (node table lines 26-34, the `start`/`stop` paragraph lines 45-48, "Any one of the five" line 50, the `start` section lines 140-145, "any of the five" line 163)
- Modify: `docs/guides/scaling-and-boosts.md` (new section before `## Two things a boost is not`, line 231)
- Modify: `docs/explanation/agent-trust.md` (new section before `## Revocation is not instant`, line 71)
- Modify: `docs/explanation/network-boundaries.md` (lines 350-354)
- Modify: `docs/getting-started/index.md` (line 106)
- Modify: `docs/guides/upgrading.md` (new section before `## Older installations`)
- Modify: `hack/docs-length.sh` (`upgrading.md` ceiling 950 → 1050)

**Interfaces:** none. Run every new paragraph through the `humanizer` skill (embedded mode) before writing it; plain prose, no em dashes, no bold labels.

`docs/guides/upgrading.md` stands at 944 of its 950-word ceiling, so the upgrade note cannot fit without raising it; 1050 keeps the 12 % headroom the script's header describes.

- [ ] **Step 1: `cloud-command.md`.**
  1. Heading `## The seven nodes`. Table rows: `spawnery.cloud.scale` opens `/cloud scale <group> <count> [for <duration>]`, `/cloud scale <group> reset`; new rows `spawnery.cloud.forcestop` → `/cloud forcestop <server>` (proxy only) and `spawnery.cloud.execute` → `/cloud execute <server|group> <command>` (proxy only, and only with `spec.commands.execute`).
  2. Replace the start/stop paragraph: pinning and resetting share one node so whoever holds a group at a size can let it go.
  3. "Any one of the seven makes the bare `/cloud` root visible" and add: on a backend the two proxy-only nodes open nothing.
  4. Replace the `/cloud start` section with three sections. **`/cloud scale`**: holds an ephemeral group at exactly that many servers, 0 included, for the given time (`30m`, `2h`, `3d`; default an hour, at most 7 days); refused above `maxReplicas` and for persistent and on-demand groups; servers above the number are drained to the fallback groups, emptiest first; a held server (after `/cloud unretire`) stays; while a pin holds a rolling update cannot surge above it, as at `maxReplicas`; `/cloud info <group>` shows `Pinned 0 servers until 18:00 UTC`; `reset` removes every pin and boost on the group. **`/cloud forcestop <server>`**: kills the pod at once with no drain and no confirmation; players lose their connection and the proxy moves them to the next fallback group; unsaved world data is lost; what follows is the group's own rule (a new ephemeral server, the same persistent ordinal on its claim, an on-demand member stays stopped with its world); `/cloud info` shows when the pod is gone; recorded as a `ForceStopped` event naming the issuer and the proxy. **`/cloud execute <server|group> <command>`**: off until the Network says
     ```yaml
     kind: Network
     spec:
       commands:
         execute: true
     ```
     runs the command with console permissions on one server or on every Ready server of a group whose agent is connected; one server answers with up to 20 lines of output, a group with a line per server and a total such as `4 of 5 servers ran it`; a server silent for 8 s is listed as such; feedback a command sends later (from another tick or thread) is not shown; never a proxy; recorded as `CommandExecuted` on each server with issuer and command, never output; the operator logs network, proxy, issuer, target and command. Say what switching it on opens: anybody holding the node can run any console command on any server, which on most servers includes `op`.
  5. "where nobody holds any of the seven nodes".

- [ ] **Step 2: `scaling-and-boosts.md`**, new section `## Holding a group at a size`: the `ScaleBoost` YAML from spec §2.2 (`mode: Exact`, `replicas: 0`, `expiresAt`), one paragraph on how it differs from an `Add` boost (floor and ceiling, newest wins, `Add` boosts pause), `status.pinnedReplicas` and `status.pinnedUntil`, and that `/cloud scale` creates exactly this object owned by the group.

- [ ] **Step 3: `agent-trust.md`**, new section `## What a compromised proxy can do that a server cannot`: a proxy's token can force-stop any server of its namespace and, where `spec.commands.execute` is on, run any console command on any of them, which on a server with `op` is the server; a backend can do neither, the operator refuses both from a server's token, and the Paper `/cloud` has neither verb; the plugin API offers no method for either; so `spec.commands.execute` is the line to leave off on a network whose proxies run third-party plugins.

- [ ] **Step 4: The node counts.** `network-boundaries.md`: "It carries seven: `spawnery.cloud.read`, `.retire`, `.scale`, `.status`, `.events`, and on a proxy `.forcestop` and `.execute`, listed …"; keep the next sentence and add "and killing or commanding a server is a proxy's alone." `getting-started/index.md` line 106: "covers the seven nodes".

- [ ] **Step 5: `upgrading.md`**, before `## Older installations`:

```markdown
## `/cloud start` and `/cloud stop` are gone

Since 0.19.0, `/cloud scale <group> <count> [for <duration>]` replaces
`start`. Where `start` raised a group's floor, `scale` holds the group at
exactly that many servers, so it can also take a group below `minReplicas`.
`/cloud scale <group> reset` replaces `stop` and removes pins and boosts
alike. The node is still `spawnery.cloud.scale`. A plugin calling `boost` or
`stopBoosts` keeps working; both are deprecated in favour of `scale` and
`resetScale`.
```

In `hack/docs-length.sh`: `"docs/guides/upgrading.md:1050"`.

- [ ] **Step 6: Check**

Run: `git add -A && NIX build /home/paul/git/spawnery-cloud#docs-site --no-link 2>&1 | tail -5; NIX develop /home/paul/git/spawnery-cloud -c bash hack/docs-length.sh; NIX develop /home/paul/git/spawnery-cloud -c bash hack/docs-length-test.sh`
Expected: the site builds (mkdocs `--strict`), both scripts exit 0.
Run: `grep -rn 'cloud start\|cloud stop\|five nodes\|carries five' docs --include=*.md | grep -v 'docs/archive\|superpowers'`
Expected: nothing.

- [ ] **Step 7: Commit**

```bash
git add docs hack/docs-length.sh
git commit -m "feat(docs): /cloud scale, forcestop and execute" \
  -m "<body: the guide's seven nodes and three sections, the execute switch and what it opens, a compromised proxy in agent-trust, the upgrade note; upgrading.md's ceiling moves to 1050 because the page was at 944 of 950>"
```

---

### Task 11: End to end on real images

**Files:**
- Modify: `test/e2e/tutorial_test.go` (new test after `TestTutorialJoinPermission`; helpers at the end of the file)
- Modify: `hack/e2e-tutorial.sh` (the `go test` line 90: `-run` gains the test and an `E2E_RUN` override, `-timeout 21m` → `28m`)
- Modify: `.github/workflows/nightly.yml` (`tutorial-e2e` `timeout-minutes: 30` → `40`)

**Interfaces:**
- Consumes: the reply wording from Task 9; event reason `ForceStopped` (Task 6); `ServerGroupStatus.PinnedReplicas` (Task 1); helpers in this file and `e2e_test.go`: `applyManifest`, `eventuallyIn`, `readPodLog`, `readyLobbyServers`, `podReady`, `k8s`, `clientset`, `ctx`, constants `tutorialNamespace`, `tutorialManifest`, `tutorialServerGroup`, `tutorialProxyGroup`, `tutorialOperatorNamespace`; `podspec.ProxyContainerName` (`"velocity"`), `podspec.ContainerName` (`"minecraft"`), `podspec.LabelRole`, `podspec.LabelGroup`, `podspec.RoleProxy`.

**The route to the console, decided.** The test images carry no LuckPerms and the join client holds no permission, so the commands are typed as Velocity's console, which holds every permission. The proxy container already has `Stdin: true` (`internal/podspec/proxy.go:229`), the image's entrypoint `exec`s java so it is PID 1 (`image/velocity-entrypoint.sh`), and the image ships `bash` and `coreutils` (`nix/velocity-image.nix`). So `kubectl exec <proxy> -c velocity -- bash -c 'printf "%s\n" "$1" > /proc/1/fd/0' _ "<line>"` writes one line into the console's stdin, and Velocity's JLine reader takes it as typed. runc hands PID 1's stdio to the container user, so the non-root user can reopen it. The reply goes to the console, which is the pod log. This exercises the real path: Brigadier tree, connector, gRPC, operator, Paper agent. It needs no pod-spec change, so the proxy pod hash and `hash_golden_test.go` stay as they are and no proxy rolls. The rejected alternatives: `kubectl attach -i` reaches the same stdin but never exits on its own (the output stream stays open), so it needs a kill timed against the log; a fake proxy identity calling the operator directly (as `cmd/spawnery-stubop` tools do) would skip the command tree and the connector, which are half of what is being tested.

- [ ] **Step 1: Prove the route before writing the test.** On `paul-desktop`: `systemd-run --scope --user --property=Delegate=yes -- NIX develop /home/paul/git/spawnery-cloud -c env KIND_EXPERIMENTAL_PROVIDER=podman E2E_KEEP=1 E2E_RUN=TestTutorialPath make e2e-tutorial` (after Step 4 adds `E2E_RUN`; until then run the script once with `E2E_KEEP=1` and let it finish). With the printed `KUBECONFIG`:

```bash
P=$(kubectl -n spawnery-tutorial get pods -l spawnery.cloud/role=proxy -o jsonpath='{.items[0].metadata.name}')
kubectl -n spawnery-tutorial exec "$P" -c velocity -- bash -c 'printf "%s\n" "$1" > /proc/1/fd/0' _ "cloud list"
kubectl -n spawnery-tutorial logs "$P" -c velocity --tail=20
```

Expected: the log shows the `/cloud list` heading and the lobby. If the `exec` fails with `Permission denied`, use the attach route instead and write `velocityConsole` (Step 2) as: start `kubectl -n <ns> attach -i -q <pod> -c velocity` with `Stdin` a pipe, write the line, close the pipe, then poll the pod log for a line written after the call (compare `readPodLog` lengths before and after) for up to 10 s and kill the process. Record which route worked in the task report. Delete the cluster afterwards (`kind delete cluster --name spawnery-e2e-tutorial`).

- [ ] **Step 2: Helpers**, at the end of `tutorial_test.go`:

```go
// velocityConsole types one line into a proxy's console: java is PID 1 and
// the container keeps stdin open, so PID 1's stdin is where Velocity reads
// typed commands. The console holds every permission.
func velocityConsole(t *testing.T, pod, line string) {
	t.Helper()
	cmd := exec.Command("kubectl", "-n", tutorialNamespace, "exec", pod, "-c", podspec.ProxyContainerName,
		"--", "bash", "-c", `printf '%s\n' "$1" > /proc/1/fd/0`, "console", line)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("type %q into %s's console: %v\n%s", line, pod, err, out)
	}
}

func readyGateway() string {
	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(tutorialNamespace),
		client.MatchingLabels{podspec.LabelRole: podspec.RoleProxy, podspec.LabelGroup: tutorialProxyGroup}); err != nil {
		return ""
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp.IsZero() && podReady(&pods.Items[i]) {
			return pods.Items[i].Name
		}
	}
	return ""
}

func proxyLogMatches(pod string, pattern *regexp.Regexp) bool {
	log, err := readPodLog(tutorialNamespace, pod, &corev1.PodLogOptions{Container: podspec.ProxyContainerName})
	return err == nil && pattern.MatchString(log)
}

func lobbyServerCount() int {
	var list spawneryv1alpha1.ServerList
	if err := k8s.List(ctx, &list, client.InNamespace(tutorialNamespace)); err != nil {
		return -1
	}
	n := 0
	for _, s := range list.Items {
		if s.Spec.GroupRef.Name == tutorialServerGroup {
			n++
		}
	}
	return n
}
```

- [ ] **Step 3: The test.**

```go
// TestTutorialCloudCommands pins the lobby to zero and resets it, force-stops
// a lobby server, and runs console commands on the lobby, all typed into the
// gateway's console. Each reply is read back from the gateway's log.
func TestTutorialCloudCommands(t *testing.T) {
	if os.Getenv("SPAWNERY_E2E_TUTORIAL") != "1" {
		t.Skip("set SPAWNERY_E2E_TUTORIAL=1; hack/e2e-tutorial.sh does this nightly")
	}
	applyManifest(t, tutorialManifest)
	groupKey := client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialServerGroup}
	t.Cleanup(func() {
		_ = k8s.DeleteAllOf(ctx, &spawneryv1alpha1.ScaleBoost{}, client.InNamespace(tutorialNamespace))
	})

	eventuallyIn(t, tutorialOperatorNamespace, 5*time.Minute, "a Ready lobby server and a Ready gateway", func() (bool, string) {
		return len(readyLobbyServers()) >= 1 && readyGateway() != "",
			fmt.Sprintf("lobby=%v gateway=%q", readyLobbyServers(), readyGateway())
	})
	gateway := readyGateway()

	// A pin of 0 empties the lobby.
	velocityConsole(t, gateway, "cloud scale lobby 0 for 10m")
	eventuallyIn(t, tutorialOperatorNamespace, 4*time.Minute, "a pin of 0 to empty the lobby", func() (bool, string) {
		var g spawneryv1alpha1.ServerGroup
		if err := k8s.Get(ctx, groupKey, &g); err != nil {
			return false, err.Error()
		}
		pinned := g.Status.PinnedReplicas != nil && *g.Status.PinnedReplicas == 0
		n := lobbyServerCount()
		return pinned && n == 0, fmt.Sprintf("pinnedReplicas=%v servers=%d", g.Status.PinnedReplicas, n)
	})
	if !proxyLogMatches(gateway, regexp.MustCompile(`lobby is pinned to 0 servers until \d\d:\d\d UTC`)) {
		t.Error("the gateway's console never said the lobby is pinned")
	}

	// reset brings it back.
	velocityConsole(t, gateway, "cloud scale lobby reset")
	eventuallyIn(t, tutorialOperatorNamespace, 5*time.Minute, "reset to bring a lobby server back", func() (bool, string) {
		var g spawneryv1alpha1.ServerGroup
		if err := k8s.Get(ctx, groupKey, &g); err != nil {
			return false, err.Error()
		}
		return g.Status.PinnedReplicas == nil && len(readyLobbyServers()) >= 1,
			fmt.Sprintf("pinnedReplicas=%v ready=%v", g.Status.PinnedReplicas, readyLobbyServers())
	})

	// forcestop is followed by a new server.
	victim := readyLobbyServers()[0]
	var before spawneryv1alpha1.Server
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: victim}, &before); err != nil {
		t.Fatalf("get %s: %v", victim, err)
	}
	velocityConsole(t, gateway, "cloud forcestop "+victim)
	eventuallyIn(t, tutorialOperatorNamespace, 5*time.Minute, "a new lobby server after the force-stop", func() (bool, string) {
		var now spawneryv1alpha1.Server
		err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: victim}, &now)
		gone := apierrors.IsNotFound(err) || (err == nil && now.UID != before.UID)
		var others []string
		for _, name := range readyLobbyServers() {
			if name != victim {
				others = append(others, name)
			}
		}
		return gone && len(others) >= 1, fmt.Sprintf("victim gone=%v others=%v", gone, others)
	})
	if !proxyLogMatches(gateway, regexp.MustCompile(regexp.QuoteMeta(victim)+` is being killed\.`)) {
		t.Error("the gateway's console never confirmed the force-stop")
	}
	events, err := clientset.CoreV1().Events(tutorialNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + victim,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	recorded := false
	for _, e := range events.Items {
		recorded = recorded || e.Reason == "ForceStopped"
	}
	if !recorded {
		t.Errorf("no ForceStopped event on %s", victim)
	}

	// execute is refused without the switch.
	velocityConsole(t, gateway, "cloud execute lobby list")
	eventuallyIn(t, tutorialOperatorNamespace, time.Minute, "the refusal without spec.commands.execute", func() (bool, string) {
		return proxyLogMatches(gateway, regexp.MustCompile(`execute is not enabled on this network`)), "not in the log yet"
	})

	// With it, say runs on every lobby server, and list's output comes back.
	setExecute := func(on bool) {
		var n spawneryv1alpha1.Network
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: "tutorial"}, &n); err != nil {
			t.Fatalf("get Network: %v", err)
		}
		patch := client.MergeFrom(n.DeepCopy())
		n.Spec.Commands = &spawneryv1alpha1.NetworkCommands{Execute: on}
		if err := k8s.Patch(ctx, &n, patch); err != nil {
			t.Fatalf("patch Network: %v", err)
		}
	}
	setExecute(true)
	t.Cleanup(func() { setExecute(false) })

	marker := fmt.Sprintf("spawnery-e2e-%d", time.Now().UnixNano())
	velocityConsole(t, gateway, "cloud execute lobby say "+marker)
	eventuallyIn(t, tutorialOperatorNamespace, time.Minute, "say to reach every lobby server", func() (bool, string) {
		total := regexp.MustCompile(`(\d+) of (\d+) servers ran it`)
		log, err := readPodLog(tutorialNamespace, gateway, &corev1.PodLogOptions{Container: podspec.ProxyContainerName})
		if err != nil {
			return false, err.Error()
		}
		m := total.FindAllStringSubmatch(log, -1)
		if len(m) == 0 || m[len(m)-1][1] != m[len(m)-1][2] {
			return false, fmt.Sprintf("totals so far: %v", m)
		}
		for _, name := range readyLobbyServers() {
			var s spawneryv1alpha1.Server
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: name}, &s); err != nil {
				return false, err.Error()
			}
			serverLog, err := readPodLog(tutorialNamespace, s.Status.PodName, &corev1.PodLogOptions{Container: podspec.ContainerName})
			if err != nil || !strings.Contains(serverLog, marker) {
				return false, name + " has not logged the marker"
			}
		}
		return true, ""
	})

	target := readyLobbyServers()[0]
	velocityConsole(t, gateway, "cloud execute "+target+" list")
	eventuallyIn(t, tutorialOperatorNamespace, time.Minute, "list's output back in the gateway's log", func() (bool, string) {
		ran := proxyLogMatches(gateway, regexp.MustCompile(regexp.QuoteMeta(target)+` ran it`))
		output := proxyLogMatches(gateway, regexp.MustCompile(`players online`))
		return ran && output, fmt.Sprintf("ran=%v output=%v", ran, output)
	})
}
```

Check the Network's name in `docs/tutorial/network.yaml` (`tutorial`) and that `podspec.ContainerName` is the Paper container (`internal/podspec/server.go:36`).

- [ ] **Step 4: The script and the nightly budget.** In `hack/e2e-tutorial.sh`, the last line becomes:

```bash
SPAWNERY_E2E_TUTORIAL=1 go test -tags e2e -count=1 -v -timeout 28m \
	-run "${E2E_RUN:-TestTutorialPath|TestTutorialPlayableSlots|TestTutorialChangeoverStages|TestTutorialTransferOnDrain|TestTutorialJoinPermission|TestTutorialCloudCommands}" ./test/e2e/...
```

and add `E2E_RUN` to the variables at the top with a one-line comment: `# E2E_RUN narrows -run, for proving one test bites.` In `.github/workflows/nightly.yml`, `tutorial-e2e`'s `timeout-minutes: 40`.

- [ ] **Step 5: Run it**

Run (rootless podman on `paul-desktop`): `systemd-run --scope --user --property=Delegate=yes -- NIX develop /home/paul/git/spawnery-cloud -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e-tutorial`
Expected: every tutorial test PASS, `TestTutorialCloudCommands` included. `hack/e2e-tutorial.sh` builds the images from this branch (`nix build .#purpur-image` etc., tagged `0.18.0`, the tag the tutorial manifest names) and loads them; `git add -A` first.

- [ ] **Step 6: Prove each part bites.** Four mutations, each in its own throwaway worktree (`git worktree add --detach /tmp/claude-1000/…/mut-N HEAD`, `git add -A` inside it, then the same command with `E2E_RUN=TestTutorialCloudCommands`), each removed afterwards with `git worktree remove --force`:
  1. `internal/controller/scaling.go`: `floor()` and `ceiling()` ignore `in.Pinned`. Expected: FAIL at "a pin of 0 to empty the lobby".
  2. `internal/controller/server_controller.go`: drop `ForceStopRequested: srv.Spec.ForceStop` from `collectInputs`. Expected: FAIL at "a new lobby server after the force-stop".
  3. `internal/agentserver/execute.go`: drop the `if !enabled` refusal. Expected: FAIL at "the refusal without spec.commands.execute".
  4. `agent/paper/.../CommandRun.kt`: `runCommand` drops `addAllOutput`. Expected: FAIL at "list's output back in the gateway's log".
  Paste the passing run's summary and the four failures (the failing `eventuallyIn` line each) into the task report.

- [ ] **Step 7: Commit**

```bash
git add test/e2e/tutorial_test.go hack/e2e-tutorial.sh .github/workflows/nightly.yml
git commit -m "test(e2e): /cloud scale, forcestop and execute from the proxy's console" \
  -m "<body: the console route and why (stdin already open, no pod-spec change, no proxy roll), the four mutations that make it fail, the longer nightly budget>"
```

---

### Task 12: Final gates

**Files:** none new; whatever the gates ask to regenerate.

- [ ] **Step 1: Run every gate**

Run: `NIX develop /home/paul/git/spawnery-cloud -c make test`
Run: `NIX develop /home/paul/git/spawnery-cloud -c make lint`
Run: `git add -A && NIX build /home/paul/git/spawnery-cloud#agents --no-link`
Run: `NIX build /home/paul/git/spawnery-cloud#spawnery-operator --no-link`
Expected: `go test` shows no `FAIL`; `golangci-lint` reports `0 issues.`; both Nix builds succeed. `make test` includes the generated-file, pin and docs-length checks; if it regenerates anything, commit it.

- [ ] **Step 2: Check what must not have moved**

Run: `git diff --quiet origin/master -- internal/podspec/hash_golden_test.go flake.nix charts/spawnery/Chart.yaml charts/spawnery/values.yaml && echo unchanged`
Expected: `unchanged` (no pod hash moved, no version bumped).
Run: `git grep -n 'cloud start\|cloud stop\|startBoost' -- agent docs ':!docs/archive' ':!docs/superpowers'`
Expected: nothing.

- [ ] **Step 3: Commit anything the gates regenerated**

```bash
git add -A
git commit -m "chore: regenerate after the gates"
```

Only if Step 1 changed files. The PR is opened by the lead after the final review; the 0.19.0 release is its own PR after the merge.

---

## Self-review

- Spec §2.1 command and limits: Tasks 6 (operator bounds), 9 (command, `d`, reset). §2.2 resource: Task 1. §2.3 sizing: Task 3 (`Exact` returns a `Pin` so the status can carry the end). §2.4 visibility: Tasks 1, 4, 8, 9. §2.5 wire: Task 2. §2.6 API: Task 8. §3 forcestop: Tasks 1, 5, 6, 9. §4 execute: Tasks 1, 2, 6, 7, 9. §5 permissions: Task 9 and Task 10. §6 upgrading: Task 10. §7 tests: every task; e2e Task 11. §8 docs: Task 10. §9 versions: Global Constraints and Task 12.
- Review Focus lines and their tests: 1 → `TestABoostOwnedByAGroupThatIsGoneIsNotTheSuccessors`, `TestAPredecessorsPinDoesNotHoldTheGroup`; 2 → `TestAServerThatDoesNotAnswerIsListedAndTheRestStillArrive`, `TestOneServerCannotAnswerForAnother`; 3 → `legacy colour codes are stripped and tags are left as text`, `output reaches chat as text, never as markup`; 4 → `TestAForceStopShortensAGracePeriodAlreadyRunning`, the second force-stop in `TestAForceStopFromAProxySetsTheFlagAndLeavesARecord`; 5 → `durations at the edges are named, never sent and never thrown`, `TestAScaleIsRefusedWhereItCannotHold` (eight days, overflow).
