# Per-start resources for on-demand servers: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `startServer` call may set memory request and limit, heap and CPU for one on-demand member, under a ceiling the group sets, and the member's JVM runs with the heap that fits.

**Architecture:** The group opts in with `spec.memberResources`. The operator resolves a start's values against the group's effective resources in one pure function, stamps the result on the `Server`, and `BuildServerPod` renders it, passing the heap as `SPAWNERY_HEAP` to the entrypoint. The Java API gains `StartOptions`; the agent refuses to trust an operator that does not echo `resources_understood`. The Server controller compares the agent's reported heap with the stamped one.

**Tech Stack:** Go (controller-runtime, kubebuilder CEL, envtest), protobuf/gRPC, POSIX sh entrypoint, Kotlin agent, Java plugin API.

**Spec:** `docs/superpowers/specs/2026-11-01-on-demand-start-resources-design.md`

## Global Constraints

- Every command runs in the dev shell: `nix develop -c <command>` from the repository root (written `nd <command>` below).
- On a small machine, envtest packages run with `-p 1` (see CLAUDE.md); each boots its own etcd and kube-apiserver.
- Commits: Conventional Commits with a scope (`feat(api): …`, `feat(agentserver): …`), body wrapped at 72. No line naming Claude, Claude Code or Anthropic, no `Co-Authored-By`, no "Generated with".
- New files carry the header `Copyright paul_wtf.` plus the Apache text, as existing files do.
- Comments: none unless a reader cannot derive it from the code beside it (foreign-system behaviour, an absence, an arbitrary-looking number). No history, no measurements in code.
- After any change to API types, markers or the `.proto`: `nd make manifests generate proto`, commit the generated files, and for the proto `nd go test ./internal/agentpb -run TestTheWireFormatMatchesTheGolden -update`.
- `git add` new files before any `nix build` or `make agent`.
- Values pinned by the spec: `nonHeap` default `2Gi`; heap tolerance 5 %; env var `SPAWNERY_HEAP` with value `<MiB rounded down>m`; `ServerState.resources` is field 15; `StartServerResult.resources` = 3, `resources_understood` = 4; `StartServerRequest.resources` = 3.
- A group without `memberResources` must render byte-identical pods; `internal/podspec/hash_golden_test.go` must pass unchanged.

## Review Focus

- A start that sets only CPU on a group whose memory limit is above `max.memory`: refused, naming `memory` (Task 3 test `TestResolveChecksGroupValuesAgainstMax`).
- Quantities in unusual but valid spellings (`"7G"`, `"7168Mi"`, `"0.5"` CPU) and invalid ones (`"7 Gi"`, `"-1Gi"`, `"0"`): valid resolve, invalid refuse naming the field (Task 3 `TestResolveParsesQuantities`).
- A heap that is not a whole MiB (`"5000000000"` bytes): `SPAWNERY_HEAP` rounds down, never up past the limit (Task 5 `TestServerPodRoundsTheHeapDownToMiB`).
- A group edited after a member started (new `max`, new `nonHeap`): the running member's second start answers `already_running` with its stamped values, not refused (Task 4 `TestStartOfARunningMemberIgnoresAGroupEditedSince`).
- A network default with only limits and no requests, and a group with no `spec.resources`: the resolved request equals the default's limit, lowered to a smaller new limit (Task 3 `TestResolveTakesNetworkDefaultsAndRequestEqualsLimit`).

---

### Task 1: API types and CEL

**Files:**
- Modify: `api/v1alpha1/servergroup_types.go` (new `MemberResourcesSpec`, field `MemberResources` on `ServerGroupSpec`, CEL rule on the spec next to the existing OnDemand rules)
- Modify: `api/v1alpha1/server_types.go` (`ServerSpec.Resources`, `ServerSpec.Heap`, immutability)
- Test: `api/v1alpha1/servergroup_envtest_test.go`, `api/v1alpha1/server_envtest_test.go`
- Generated: `zz_generated.deepcopy.go`, `config/crd/bases/`, `charts/spawnery/templates/crds.yaml`, `docs/reference/crds.md`

**Interfaces:**
- Produces:
  ```go
  type MemberResourcesSpec struct {
      Max     corev1.ResourceList `json:"max"`               // required; max.memory required (CEL)
      NonHeap *resource.Quantity  `json:"nonHeap,omitempty"` // +kubebuilder:default="2Gi"
  }
  // ServerGroupSpec
  MemberResources *MemberResourcesSpec `json:"memberResources,omitempty"`
  // ServerSpec, both immutable via CEL self == oldSelf
  Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
  Heap      *resource.Quantity           `json:"heap,omitempty"`
  ```
  CEL messages (exact): `spec.memberResources is only allowed for type OnDemand`, `spec.memberResources.max.memory is required`, `spec.memberResources.max.memory must be above nonHeap`, `spec.resources is immutable`, `spec.heap is immutable`.

- [ ] **Step 1: Write the failing tests**
  - `TestMemberResourcesIsOnDemandOnly`: `ephemeralGroup(ns, "x")` with `MemberResources{Max: {memory: 16Gi}}` → create error containing `only allowed for type OnDemand`; `onDemandGroup` with the same → created, and read back `NonHeap` equals `2Gi`.
  - `TestMemberResourcesNeedsAMemoryCeilingAboveNonHeap`: `Max: {cpu: 8}` → error `max.memory is required`; `Max: {memory: 2Gi}, NonHeap: 2Gi` → error `must be above nonHeap`; `Max: {memory: 3Gi}, NonHeap: 2Gi` → created.
  - `TestServerResourcesAndHeapAreImmutable`: create a `Server` with `Resources.Limits.memory=4Gi`, `Heap=2Gi`; updating either → error `is immutable`; updating `Spec.Retire` → succeeds.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./api/v1alpha1 -run 'TestMemberResources|TestServerResourcesAndHeap' -count=1` fails to compile on the missing fields.
- [ ] **Step 3: Add the types and markers**, then `nd make manifests generate`.
- [ ] **Step 4: Run, expect PASS**: same command; then `nd go test ./api/v1alpha1 -count=1` all green.
- [ ] **Step 5: Commit**: `feat(api): memberResources on on-demand groups, resources and heap on servers`, including generated files.

### Task 2: Wire format

**Files:**
- Modify: `proto/spawnery/agent/v1alpha1/agent.proto` (`StartResources`, the three fields from the spec, `ServerState.resources = 15`)
- Generated: `internal/agentpb/`, `agent/common/src/proto/java/`, `internal/agentpb/testdata/agent.wire.golden`

**Interfaces:**
- Produces: `agentpb.StartResources{MemoryRequest, MemoryLimit, CpuRequest, CpuLimit, Heap string}`; `StartServerRequest.Resources *StartResources`; `StartServerResult.Resources *StartResources`, `.ResourcesUnderstood bool`; `ServerState.Resources *StartResources`. Comments on the messages are the ones in spec 3.2.

- [ ] **Step 1: Edit the proto**, `nd make proto`.
- [ ] **Step 2: Run** `nd go test ./internal/agentpb -count=1`, expect FAIL in `TestTheWireFormatMatchesTheGolden` naming the new message.
- [ ] **Step 3: Update the golden** with `-update`, rerun, expect PASS; `nd go build ./...` succeeds.
- [ ] **Step 4: Commit**: `feat(proto): resources on a start, its answer and a server's state`.

### Task 3: Resolution

**Files:**
- Create: `internal/agentserver/startresources.go`
- Test: `internal/agentserver/startresources_test.go` (plain unit test, no envtest)

**Interfaces:**
- Consumes: Task 1 types; Task 2 `agentpb.StartResources`.
- Produces:
  ```go
  type Resolved struct {
      Resources corev1.ResourceRequirements
      Heap      *resource.Quantity
  }
  // base is the group's effective resources (group, else network default; nil if neither).
  func EffectiveResources(net *spawneryv1alpha1.Network, g *spawneryv1alpha1.ServerGroup) *corev1.ResourceRequirements
  // req nil or all-empty means "no values". stamp=false means: leave the Server unstamped.
  func ResolveStartResources(g *spawneryv1alpha1.ServerGroup, base *corev1.ResourceRequirements, req *agentpb.StartResources) (res Resolved, stamp bool, err error)
  var ErrNoMemberResources = errors.New("that group sets no spec.memberResources, so a start cannot choose its own resources")
  type ResourceRefusal struct{ Field, Message string } // implements error; Message names the field and both numbers
  ```
  Rules, in the spec's order (3.2): empty field ← base; an unwritten base request counts as its limit; empty request ← base request lowered to the resolved limit; requests always written explicitly; heap ← given, else limit − `NonHeap` when the start set a memory limit, else nil; refusals as spec 3.2 step 3, including base values above `Max`. `stamp` is true whenever `g.Spec.MemberResources != nil`.

- [ ] **Step 1: Write the failing table tests**
  - `TestResolveWithoutMemberResources`: req nil → `stamp=false`, no error; req `{MemoryLimit:"4Gi"}` → `errors.Is(err, ErrNoMemberResources)`.
  - `TestResolveFillsEmptyFieldsFromTheGroup`: base req 2Gi/limit 4Gi cpu 1/2, `Max{memory:16Gi,cpu:8}`, req `{MemoryLimit:"7Gi"}` → limits.memory 7Gi, requests.memory 2Gi, cpu 1/2 kept, heap 5Gi.
  - `TestResolveLowersTheRequestToANewSmallerLimit`: base request 4Gi, req `{MemoryLimit:"3Gi"}` → requests.memory 3Gi, heap 1Gi.
  - `TestResolveTakesNetworkDefaultsAndRequestEqualsLimit`: `EffectiveResources` with group `Resources==nil`, network default limits only `{memory:4Gi}`; req `{MemoryLimit:"8Gi"}` → requests.memory 4Gi (written), limits 8Gi; req `{MemoryLimit:"3Gi"}` → requests.memory 3Gi.
  - `TestResolveUsesAGivenHeap`: req `{MemoryLimit:"8Gi", Heap:"5500Mi"}` → heap 5500Mi.
  - `TestResolveStampsAStartWithoutValues`: req nil on a group with `MemberResources` → `stamp=true`, resources == base, heap nil.
  - `TestResolveRefuses` (subtests, each asserting `ResourceRefusal.Field` and that `Message` contains both numbers): request above limit (`memory`), heap not below limit (`heap`), heap without any memory limit (`heap`), limit above max (`memory`), cpu above max (`cpu`), cpu set while `Max` names no cpu (`cpu`).
  - `TestResolveChecksGroupValuesAgainstMax`: base limit 20Gi, `Max.memory` 16Gi, req `{CpuLimit:"2"}` → refusal `Field=="memory"`.
  - `TestResolveParsesQuantities`: `"7G"`, `"7168Mi"`, cpu `"0.5"` resolve; `"7 Gi"`, `"-1Gi"`, `"0"` refuse with the field named.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./internal/agentserver -run TestResolve -count=1` (compile error).
- [ ] **Step 3: Implement** in `startresources.go` with `resource.ParseQuantity` and `Quantity.Cmp`; the derived heap is `limit.DeepCopy()` then `Sub(nonHeap)`.
- [ ] **Step 4: Run, expect PASS.**
- [ ] **Step 5: Commit**: `feat(agentserver): resolve a start's resources against its group`.

### Task 4: The start writes and answers them

**Files:**
- Modify: `internal/agentserver/writer.go` (`ClusterWriter.StartServer`, `KubeWriter.StartServer`, `occupant`, `StartedServer`)
- Create: `internal/netstate/resources.go` (`ToStartResources`)
- Modify: `internal/agentserver/requests.go` (`answerStartServer`)
- Modify: every fake `ClusterWriter` in `internal/agentserver/*_test.go` that implements `StartServer`
- Test: `internal/agentserver/ondemand_envtest_test.go`

**Interfaces:**
- Consumes: Task 3 `EffectiveResources`, `ResolveStartResources`, `ErrNoMemberResources`, `ResourceRefusal`.
- Produces:
  ```go
  StartServer(ctx context.Context, namespace, group, key string, req *agentpb.StartResources) (StartedServer, error)
  type StartedServer struct {
      Name           string
      AlreadyRunning bool
      Resources      *agentpb.StartResources // from the Server's Spec.Resources/Heap; nil when unstamped
  }
  // in internal/netstate/resources.go, so Task 7's netstate code shares it:
  func netstate.ToStartResources(r *corev1.ResourceRequirements, heap *resource.Quantity) *agentpb.StartResources // nil for nil r; Quantity.String() per field
  ```
  Order inside `KubeWriter.StartServer`: unchanged up to the member loop; the running-key return and `occupant` fill `Resources` from the existing member; after the loop and before the ceiling, fetch the `Network` named by `g.Spec.NetworkRef`, resolve, and stamp `Spec.Resources`/`Spec.Heap` when `stamp`. `answerStartServer` maps `ErrNoMemberResources` and `ResourceRefusal` to `REFUSED` with the error text, and sets `ResourcesUnderstood` when the request carried any non-empty field.

- [ ] **Step 1: Write the failing envtests** (extend `makeOnDemandGroup` with an option for `MemberResources`; `startOverTheWire` gains a variant taking `*agentpb.StartResources`)
  - `TestStartWithResourcesStampsTheMember`: `{MemoryLimit:"7Gi"}` on a group with `Max.memory` 16Gi → `member(...).Spec.Resources.Limits.memory` 7Gi, `Spec.Heap` 5Gi; answer `Resources.MemoryLimit=="7Gi"`, `Heap=="5Gi"`, `ResourcesUnderstood`.
  - `TestStartWithResourcesOnAGroupWithoutMemberResourcesIsRefused`: `REFUSED`, no `Server` exists.
  - `TestStartAboveMaxIsRefusedEvenAtTheCeiling`: `maxInstances` 1 already used by another key, `{MemoryLimit:"32Gi"}` → `REFUSED` with the resources message, not the ceiling one.
  - `TestStartOfARunningMemberIgnoresAGroupEditedSince`: start with 7Gi; patch group `Max.memory` to 4Gi; start again with `{MemoryLimit:"7Gi"}` → `AlreadyRunning`, `Resources.MemoryLimit=="7Gi"`, no error.
  - `TestStartWithoutValuesLeavesAGroupWithoutMemberResourcesUnstamped`: `Spec.Resources==nil`, answer `Resources==nil`, `ResourcesUnderstood==false`.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./internal/agentserver -run 'TestStart' -count=1 -p 1`.
- [ ] **Step 3: Implement** the signature change, update fakes and the existing caller.
- [ ] **Step 4: Run, expect PASS**, then `nd go test ./internal/agentserver -count=1 -p 1` all green.
- [ ] **Step 5: Commit**: `feat(agentserver): a start may set its member's resources`.

### Task 5: The pod

**Files:**
- Modify: `internal/podspec/server.go` (`BuildServerPod` resources and env)
- Modify: `api/v1alpha1/common_types.go` only if the reserved names are listed there (they are matched by prefix; check `internal/podspec/env_test.go`)
- Test: `internal/podspec/server_test.go`

**Interfaces:**
- Consumes: Task 1 `ServerSpec.Resources`, `ServerSpec.Heap`.
- Produces: `const EnvHeap = "SPAWNERY_HEAP"` in `internal/podspec/server.go`; env entry appended with the operator's own four, before group env.

- [ ] **Step 1: Write the failing tests**
  - `TestServerPodTakesTheServersResources`: `srv.Spec.Resources` limits 7Gi beats group 4Gi in the container.
  - `TestServerPodCarriesTheHeap`: `Heap` 5Gi → env `SPAWNERY_HEAP=5120m`; no `Heap` → no such env var.
  - `TestServerPodRoundsTheHeapDownToMiB`: `Heap` `resource.MustParse("5000000000")` → `SPAWNERY_HEAP=4768m`.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./internal/podspec -run 'TestServerPod' -count=1`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run, expect PASS**; `nd go test ./internal/podspec -count=1` all green, `TestDesired*Golden` in `hash_golden_test.go` unchanged (do not pass `-update`).
- [ ] **Step 5: Commit**: `feat(podspec): a server's own resources and heap reach its pod`.

### Task 6: The entrypoint

**Files:**
- Modify: `image/entrypoint.sh` (heap flag before `exec java`)
- Test: `image/entrypoint_test.go`

**Interfaces:**
- Consumes: Task 5 `SPAWNERY_HEAP`.
- Produces: read as `${SPAWNERY_HEAP:-}`; when non-empty the argv carries `-Xmx$SPAWNERY_HEAP` and no `-XX:MaxRAMPercentage=75`.

- [ ] **Step 1: Write the failing tests** (use `runEntrypoint` and `javaArgv` with `writeLaunchFiles`, as `TestEntrypointStartsTheServerFlatFromTheLaunchFiles` does)
  - `TestEntrypointTakesTheHeapFromSpawneryHeap`: `SPAWNERY_HEAP=5120m` → argv contains `-Xmx5120m`, not `MaxRAMPercentage`.
  - `TestEntrypointKeepsMaxRAMPercentageWithoutSpawneryHeap`: unset → `-XX:MaxRAMPercentage=75`, no `-Xmx`.
  - `TestEntrypointHeapComesAfterJdkJavaOptions`: `JDK_JAVA_OPTIONS=-Xmx2048m` and `SPAWNERY_HEAP=5120m` → argv contains `-Xmx5120m` (the launcher prepends `JDK_JAVA_OPTIONS`, so presence on the command line is the check).
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./image -run 'TestEntrypoint(TakesTheHeap|KeepsMaxRAM|HeapComesAfter)' -count=1`.
- [ ] **Step 3: Implement**; `TestEveryVariableTheEntrypointsReadIsReserved` must still pass.
- [ ] **Step 4: Run, expect PASS**; `nd go test ./image -count=1`.
- [ ] **Step 5: Commit**: `feat(image): SPAWNERY_HEAP sets the server's maximum heap`.

### Task 7: The picture and the heap check

**Files:**
- Modify: `internal/netstate/netstate.go` (`ServerState.Resources`)
- Create: `internal/controller/heapcheck.go`
- Modify: `internal/controller/server_controller.go` (call after `mirrorPlayerCount`)
- Modify: `api/v1alpha1/server_types.go` (constants `ConditionHeapMismatch = "HeapMismatch"`, reasons `HeapMismatched`, `HeapAsAsked`) and run `nd make generate`
- Test: `internal/netstate/netstate_test.go`, `internal/controller/heapcheck_test.go`

**Interfaces:**
- Consumes: Task 4 `netstate.ToStartResources`.
- Produces:
  ```go
  // reported is the agent's heap max in bytes, 0 when unknown.
  func heapMismatch(want *resource.Quantity, reported int64) (mismatch bool, message string)
  ```
  Mismatch when `want != nil`, `reported > 0`, and `|reported − want| > want/20`. Message names both values in MiB. The controller sets condition `HeapMismatch` True/False only while `want != nil` and the agent has reported, and emits a Warning event `HeapMismatch` on the change to True only.

- [ ] **Step 1: Write the failing tests**
  - `TestPrivateServerStateCarriesItsResources` in netstate: a member with `Spec.Resources`/`Heap` → `ForProxies` state has `Resources.MemoryLimit` and `Heap`; an unstamped member → `Resources==nil`.
  - `TestHeapMismatch` table: want 5Gi, reported 5Gi → false; reported 4.8Gi (4 %) → false; reported 4.5Gi → true, message contains `5120` and `4608`; want nil → false; reported 0 → false.
- [ ] **Step 2: Run, expect FAIL**: `nd go test ./internal/netstate ./internal/controller -run 'TestPrivateServerStateCarriesItsResources|TestHeapMismatch' -count=1 -p 1`.
- [ ] **Step 3: Implement** and wire into `server_controller.go` using `snap.HeapMax`.
- [ ] **Step 4: Run, expect PASS**; `nd go test ./internal/netstate ./internal/controller -count=1 -p 1`.
- [ ] **Step 5: Commit**: `feat(controller): proxies see a member's resources, and a heap off its ask is flagged`.

### Task 8: The Java API and the agent

**Files:**
- Create: `agent/api/src/main/java/cloud/spawnery/agent/api/StartOptions.java`, `agent/api/src/main/java/cloud/spawnery/agent/api/ServerResources.java`
- Modify: `agent/api/src/main/java/cloud/spawnery/agent/api/{SpawneryApi,StartedServer,ServerInfo}.java`
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/{CloudConnector,MirrorApi,NetworkMirror}.kt`
- Test: `agent/api/src/test/java/cloud/spawnery/agent/api/{RecordCompatibilityTest,ValueTypesTest}.java`, `agent/api/src/test/java/cloud/spawnery/agent/api/FakeApi.java`, `agent/common/src/test/kotlin/cloud/spawnery/agent/{CloudConnectorTest,NetworkMirrorTest,MirrorApiTest}.kt`

**Interfaces:**
- Consumes: Task 2 generated Java classes.
- Produces:
  ```java
  public record ServerResources(Optional<String> memoryRequest, Optional<String> memoryLimit,
                                Optional<String> cpuRequest, Optional<String> cpuLimit, Optional<String> heap) {
      public static final ServerResources NONE; }
  public final class StartOptions {
      public static Builder builder();
      public ServerResources resources();
      public static final class Builder { memoryRequest(String), memoryLimit(String), cpuRequest(String),
                                          cpuLimit(String), heap(String), build(); } }
  public record StartedServer(String name, boolean alreadyRunning, ServerResources resources) // + old 2-arg ctor → NONE
  public record ServerInfo(..., int playableSlots, ServerResources resources)                 // + old 13-arg ctor → NONE
  CompletionStage<StartedServer> startServer(String group, String key, StartOptions options); // Javadoc: "Agents older than 0.33.0 do not have this method."
  ```
  `CloudConnector.startServer(group, key, resources: ServerResources)` sends only non-empty fields; on the answer, when any field was sent and `resourcesUnderstood` is false, completes exceptionally with `IllegalStateException("the operator does not know per-start resources; the member it started runs with the group's")`.

- [ ] **Step 1: Write the failing tests**
  - `CloudConnectorTest`: `a start with resources sends them` (request has `memoryLimit=="7Gi"`, empty others); `a start with resources fails when the operator does not understand them` (answer with `resourcesUnderstood=false` → stage fails with that message); `an already running answer with other resources succeeds and carries them`.
  - `NetworkMirrorTest`: `a server state's resources reach ServerInfo` and `a state without resources reads as NONE`.
  - `RecordCompatibilityTest`: the old `StartedServer` and `ServerInfo` constructors give `ServerResources.NONE`.
  - `ValueTypesTest`: `StartOptions.builder().memoryLimit("7Gi").build().resources().memoryLimit()` is `Optional.of("7Gi")`, others empty.
- [ ] **Step 2: Run, expect FAIL**: `git add -A agent && nd make agent` fails on the missing types.
- [ ] **Step 3: Implement**; `FakeApi` and any other `SpawneryApi` implementation gain the overload.
- [ ] **Step 4: Run, expect PASS**: `nd make agent`.
- [ ] **Step 5: Commit**: `feat(agent): StartOptions lets a plugin choose its member's resources`.

### Task 9: Documentation and e2e

**Files:**
- Modify: `docs/guides/on-demand-servers.md` (new section "Choosing resources at the start", rows in the answers table)
- Modify: `docs/plugin-api/what-a-plugin-can-do.md` (one line on `StartOptions`)
- Modify: `test/e2e/manifests/ondemand.yaml` (group gains `memberResources: {max: {memory: 4Gi}, nonHeap: 1Gi}`; network defaults unchanged)
- Modify: `test/e2e/ondemand_test.go` (`startServer` helper gains resources)

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Write the e2e test** `TestAMemberStartsWithTheResourcesItAskedFor`: start key `sized` with `{MemoryLimit:"3Gi"}`; wait Ready; the pod's container limit is `3Gi` and request `2Gi`; the agent-reported heap (`spawnery_server_heap_max_bytes` from the operator's metrics, or `kubectl exec … jcmd 1 VM.flags` if the metric path is awkward) is 2 GiB ± 5 %; a second key with `{MemoryLimit:"8Gi"}` is `REFUSED`.
- [ ] **Step 2: Write the guide section** from spec sections 3.1, 3.2 and 4: the example, how empty fields and the derived heap resolve, the refusals, that a start's heap beats an env `-Xmx` except `_JAVA_OPTIONS`, `HeapMismatch`, and that values take effect at the next start.
- [ ] **Step 3: Run** `nd make test` (all linters, docs checks) and `nd make e2e` (under rootless Podman the `systemd-run` form from CLAUDE.md), expect PASS.
- [ ] **Step 4: Commit**: `docs(guides): choosing a member's resources at the start` and `test(e2e): a member starts with the resources it asked for`.

Release notes (spec section 7) and the `imageVersion`/`operatorVersion` bump to 0.33.0 belong to the release, not to this plan.
