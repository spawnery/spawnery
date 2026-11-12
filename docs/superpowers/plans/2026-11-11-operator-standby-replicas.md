# Operator Standby Replicas Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the operator with standby replicas: every replica is Ready,
the leader publishes its own EndpointSlice for the agent Service, and a
standby takes over from a lost leader without any Server leaving `Ready`.

**Architecture:** The `spawnery-operator` Service loses its selector. A new
leader-bound runnable (`internal/agentendpoint.Publisher`) writes one
EndpointSlice per IP family once the agent listener is bound, re-asserts it,
cleans up what the old selector left behind, and empties it on shutdown.
`/readyz` becomes a plain ping, leader-only metrics are silent on a standby
(`internal/leaderonly`), and the agents' keepalive shrinks so a lost leader
node is noticed inside `phase.ReconnectGrace`.

**Tech Stack:** Go, controller-runtime v0.24.1, `k8s.io/api` v0.36.0
(`discovery.k8s.io/v1`), Helm chart, Kotlin (grpc-java OkHttp), envtest, kind.

**Spec:** `docs/superpowers/specs/2026-11-11-operator-standby-replicas-design.md`

## Global Constraints

- Every command runs in the dev shell: `nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c <cmd>` (written `nix develop -c` below).
- envtest packages on the `dev` VM run with `-p 1`.
- Commits: Conventional Commits with a scope (`feat(agentendpoint): …`), body wrapped at 72, no Claude attribution of any kind. Commits are gpg-signed; on `dev` probe first with `echo probe | gpg --clearsign --pinentry-mode error -u "$(git config user.signingkey)" >/dev/null` and ask Paul to unlock if it fails.
- After changing kubebuilder markers, chart templates or values: `nix develop -c make manifests` and commit the generated files (`config/rbac/role.yaml`, `charts/spawnery/templates/rbac.yaml`, `docs/reference/*.md`).
- Every new `+kubebuilder:rbac` marker gets its row in `internal/rbacaudit/required.go` in the same commit.
- A test that is meant to catch a regression is proven once: revert the code it guards in a throwaway worktree (`git worktree add --detach`), record the failure, remove the worktree.
- New code comments follow `~/.claude/CLAUDE.md`: none unless the code cannot say it.
- `DesiredServerHash`/`DesiredProxyHash` goldens must not move. If `internal/podspec/hash_golden_test.go` fails, something in this plan changed pod specs and is wrong.
- Version numbers (`imageVersion`, `operatorVersion`, chart) are moved by the release, not by these tasks. The release notes must say: upgrade the operator before the game images (§3.6).
- Names fixed by the spec: Service `spawnery-operator`, slices `spawnery-operator-agent-ipv4` / `-ipv6`, label `endpointslice.kubernetes.io/managed-by: spawnery.cloud-operator`, Service `spawnery-operator-metrics`, Lease `spawnery-operator.spawnery.cloud`.
- Values fixed by the spec: re-assert every 30 s, `GracefulShutdownTimeout` 7 s, `terminationGracePeriodSeconds` 10, `operator.replicas` default 2, agent keepalive 15 s / timeout 5 s, `MinKeepaliveInterval` 10 s.

## Review Focus

- A leader that was frozen (paused node, long GC, partition) resumes after a standby took over: its timers fire at once, and it must not re-assert its own address over the new leader's. Covered by `TestAssertDoesNothingWithoutTheLease` (Task 3) and the unpause check in `TestHALostLeaderNodeKeepsServersReady` (Task 6).
- A late shutdown of a replica that already lost the lease must not empty the new leader's slice. Covered by `TestWithdrawLeavesAnotherPodsSliceAlone` (Task 3).
- A single-stack cluster (`POD_IPS` holds one address) must get exactly one slice, and a slice for the other family left over from an earlier dual-stack run must not linger. Covered by `TestASingleStackPodGetsOneSlice` (Task 3).
- Someone deletes or edits the slice by hand: it comes back within one interval. Covered by `TestAssertRecreatesADeletedSlice` (Task 3).
- The metrics docs page must not change because collectors are gated: the generator runs as a "leader". Covered by `make manifests` producing no diff in `docs/reference/metrics-and-alerts.md` (Task 1).

---

### Task 1: Leader-only metrics

**Files:**
- Create: `internal/leaderonly/leaderonly.go`, `internal/leaderonly/leaderonly_test.go`, `internal/leaderonly/export_test.go`
- Modify: `internal/certs/metrics.go:60-63`, `internal/agentserver/metrics.go:106`, `internal/grpcauth/metrics.go:49-51`, `internal/controller/metrics.go:39`, `internal/controller/networkmetrics.go:78`, `internal/proxyreg/metrics.go:33`, `internal/serverreg/metrics.go:34`, `cmd/spawnery-operator/main.go:424`, `internal/docsgen/metrics/main.go:65`
- Not touched: `internal/rbacaudit/metrics.go` (the self-check runs on every replica)

**Interfaces:**
- Produces: `func Wrap(cs ...prometheus.Collector) prometheus.Collector` (Describe always delegates to every `cs`; Collect delegates only after `Elect`), `func Elect()` (idempotent, process-wide), `func Watch(elected <-chan struct{})` (spawns a goroutine that calls `Elect` once `elected` closes). `export_test.go` exposes `reset()` for tests.

- [ ] **Step 1: Write the failing tests** in `internal/leaderonly/leaderonly_test.go`:
  - `TestAWrappedCollectorIsSilentUntilElected`: a `prometheus.NewGauge` set to 42, wrapped and registered on a fresh `prometheus.NewRegistry()`; `Gather()` has no family of that name; after `Elect()` it has one with value 42.
  - `TestAWrappedCollectorStillDescribes`: `Describe` on the wrapper yields the gauge's `Desc` before `Elect`.
  - `TestWatchElectsWhenTheChannelCloses`: `Watch(ch)`, close `ch`, the gauge appears in `Gather()` within 1 s.
- [ ] **Step 2: Run** `nix develop -c go test ./internal/leaderonly -count=1`. Expected: FAIL, package does not compile.
- [ ] **Step 3: Implement** `Wrap`, `Elect`, `Watch` with an `atomic.Bool`.
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Wrap the registrations.** Each listed `MustRegister(x, y)` becomes `MustRegister(leaderonly.Wrap(x, y))`; `main.go:424` wraps `worldsync.OperatorCollectors()...`. In `cmd/spawnery-operator/main.go`, call `leaderonly.Watch(mgr.Elected())` right after `ctrl.NewManager`. In `internal/docsgen/metrics/main.go`, call `leaderonly.Elect()` at the top of its `init()` so `gatherTypes` still observes every type.
- [ ] **Step 6: Verify** `nix develop -c make manifests && git diff --exit-code docs/reference/metrics-and-alerts.md` (expected: no diff), then `nix develop -c go test -p 1 ./internal/certs ./internal/agentserver ./internal/grpcauth ./internal/controller ./internal/proxyreg ./internal/serverreg ./internal/docsgen/... -count=1` (expected: PASS; tests read the raw collectors, not the registry).
- [ ] **Step 7: Commit** `feat(metrics): leader-only collectors stay silent on a standby`.

### Task 2: Agents notice a dead operator within 20 s

**Files:**
- Modify: `agent/common/src/main/kotlin/cloud/spawnery/agent/OperatorChannel.kt:60-74`, `internal/agentserver/server.go:85-87`
- Create: `internal/agentserver/keepalive_agreement_test.go`

**Interfaces:**
- Produces: `agentserver.MinKeepaliveInterval = 10 * time.Second`.

- [ ] **Step 1: Write the failing tests** in `keepalive_agreement_test.go`, reading the Kotlin source the way `internal/podspec/kotlin_agreement_test.go` does (regexes `KEEPALIVE_SECONDS = (\d+)L` and `KEEPALIVE_TIMEOUT_SECONDS = (\d+)L`):
  - `TestTheAgentKeepaliveStaysAboveTheOperatorsMinimum`: `keepalive*time.Second > MinKeepaliveInterval`; the message names both files.
  - `TestADeadOperatorIsNoticedWithinTwentySeconds`: `(keepalive+timeout)*time.Second <= 20*time.Second`; the message cites §3.6 of the spec (takeover ~17 s plus `phase.ReconnectGrace` 45 s).
- [ ] **Step 2: Run** `nix develop -c go test ./internal/agentserver -run 'TestTheAgentKeepalive|TestADeadOperator' -count=1`. Expected: the second test FAILS (45+20).
- [ ] **Step 3: Change** `KEEPALIVE_SECONDS` to `15L`, `KEEPALIVE_TIMEOUT_SECONDS` to `5L` and the KDoc's "30 s" to the new minimum; set `MinKeepaliveInterval = 10 * time.Second`. The Go comment above it wrongly says the agents send no keepalive; it now names the agent's 15 s ping.
- [ ] **Step 4: Run** the step 2 command (PASS), then `nix develop -c make agent` (the plugins' JUnit suites, PASS).
- [ ] **Step 5: Check grpc-java's `too_many_pings` handling** for the version in `agent/deps.json`: read `OkHttpChannelBuilder` in that version's sources and confirm the transport backs its keepalive off (`AtomicBackoff`) instead of reconnecting in a loop. Record the version and the finding in the commit body; if it loops, stop and report, because an operator rollback would then be noisy.
- [ ] **Step 6: Commit** `feat(agent): keepalive every 15 s so a lost operator node is noticed in time`.

### Task 3: The EndpointSlice publisher

**Files:**
- Create: `internal/agentendpoint/publisher.go`, `internal/agentendpoint/publisher_envtest_test.go`
- Modify: `internal/agentserver/server.go` (`Server` struct, `New`, `Start` after `MarkServing`), `internal/rbacaudit/required.go:144-157`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `func (s *agentserver.Server) Serving() <-chan struct{}`, closed once, right after `MarkServing`.
  - `package agentendpoint`:
    ```go
    const ManagedBy = "spawnery.cloud-operator"
    const DefaultInterval = 30 * time.Second
    var ErrNotHolder = errors.New(...)
    type Identity struct {
        Name, Hostname, Node string
        UID  types.UID
        IPs  []string
    }
    type Publisher struct {
        Client    client.Client // uncached
        Namespace string
        Service   string        // podspec.AgentServiceName
        Port      int32         // agentserver.DefaultPort
        Lease     string        // the manager's LeaderElectionID
        Pod       Identity
        Serving   <-chan struct{}
        Interval  time.Duration
    }
    func ParseIPs(podIPs string) ([]string, error) // POD_IPS, comma-separated
    func SliceName(service string, family discoveryv1.AddressType) string // "<service>-agent-ipv4"
    func (p *Publisher) Assert(ctx context.Context) error
    func (p *Publisher) Withdraw(ctx context.Context) error
    func (p *Publisher) Start(ctx context.Context) error
    func (p *Publisher) NeedLeaderElection() bool // true
    ```

`Assert`, in order: read the Lease; unless `spec.holderIdentity` starts with `Pod.Hostname + "_"` (controller-runtime's identity is `<hostname>_<uuid>`), return `ErrNotHolder` and write nothing. Read the Service for its UID. For each family in `Pod.IPs`, create or update the slice as §3.1 lists (labels, owner reference, one endpoint with `ready: true`, `targetRef` with the pod's name and UID, `nodeName`, one port `agent`/`Port`/TCP); delete this publisher's slice for a family the pod lacks. Delete every EndpointSlice labelled `kubernetes.io/service-name: <Service>` whose `managed-by` is not `ManagedBy`, and the `Endpoints` object named `<Service>` if it exists. `Start`: wait for `Serving` or `ctx`; `Assert`; then every `Interval`, `Assert`, logging and continuing on error; on `ctx.Done()`, `Withdraw` with a fresh 2 s context (`context.WithoutCancel`). `Withdraw`: for each of its slices whose endpoint's `targetRef.uid == Pod.UID`, set `endpoints` to empty; a conflict or a slice naming another UID is left alone.

RBAC markers in `publisher.go`, namespace `spawnery-system` like `internal/certs/store.go:70`: `discovery.k8s.io` `endpointslices` get;list;create;update;delete, core `endpoints` get;delete, core `services` get. Leases get is already granted.

- [ ] **Step 1: Write the failing envtest tests** in `publisher_envtest_test.go` (namespace and Service `spawnery-operator` created per test via `testenv`; a Lease `spawnery-operator.spawnery.cloud` whose holder is `host-a_123` unless a test says otherwise; `Pod{Name: "op-a", Hostname: "host-a", UID: "uid-a", Node: "n1", IPs: [...]}`):
  - `TestAssertWritesOneSlicePerFamily`: IPs `10.0.0.5`, `fd00::5` → two slices named by `SliceName`, each with both labels, the Service owner reference, one ready endpoint with that address and `targetRef.uid == "uid-a"`, `nodeName == "n1"`, one port `agent`/9443/TCP.
  - `TestASingleStackPodGetsOneSlice`: IPs `10.0.0.5` after an earlier `Assert` with both families → only the IPv4 slice remains.
  - `TestAssertReplacesASliceNamingAnotherPod`: pre-existing IPv4 slice naming `uid-b` → after `Assert` it names `uid-a` and holds one endpoint.
  - `TestAssertRecreatesADeletedSlice`.
  - `TestAssertRemovesWhatTheSelectorLeftBehind`: a slice labelled `kubernetes.io/service-name: spawnery-operator` with `managed-by: endpointslice-controller.k8s.io`, an `Endpoints` object `spawnery-operator`, and a slice of another Service → the first two are gone, the third remains.
  - `TestAssertDoesNothingWithoutTheLease`: holder `host-b_456` → `errors.Is(err, ErrNotHolder)` and no slice exists.
  - `TestWithdrawEmptiesItsOwnSlice` and `TestWithdrawLeavesAnotherPodsSliceAlone` (slice rewritten to `uid-b` before `Withdraw` → still names `uid-b`).
  - `TestStartWaitsForTheListener`: `Serving` open → no slice after 1 s; close it → slice within 2 s; cancel the context → endpoints empty within 3 s.
  - `TestParseIPs`: `"10.0.0.5,fd00::5"` → both; `""` and `"nope"` → error.
- [ ] **Step 2: Run** `nix develop -c go test ./internal/agentendpoint -count=1`. Expected: FAIL, does not compile.
- [ ] **Step 3: Implement** `publisher.go` and `Server.Serving()`.
- [ ] **Step 4: Run** step 2's command (PASS) and `nix develop -c go test ./internal/agentserver -count=1` (PASS).
- [ ] **Step 5: Add the RBAC rows** to `RequiredNamespaced`, one per verb, each `Why` naming the `Publisher` method that needs it; run `nix develop -c make manifests` and `nix develop -c go test ./internal/rbacaudit -count=1`. Expected: PASS, and `config/rbac/role.yaml` / `charts/spawnery/templates/rbac.yaml` gain exactly those verbs.
- [ ] **Step 6: Commit** `feat(agentendpoint): the leader publishes the agent Service's endpoints`.

### Task 4: Wire it into the operator

**Files:**
- Modify: `cmd/spawnery-operator/main.go` (flags, `managerOptions`, readyz, publisher), `cmd/spawnery-operator/main_test.go:60-87`, `docs/reference/operator-flags.md`
- Create: `cmd/spawnery-operator/standby_envtest_test.go`

**Interfaces:**
- Consumes: `agentendpoint.Publisher`, `agentendpoint.ParseIPs`, `agentserver.Server.Serving()`, `leaderonly.Watch` (Task 1, already called).
- Produces: flag `--publish-agent-endpoint` (bool, default true); `func podIdentity(getenv func(string) string) (agentendpoint.Identity, error)` reading `POD_NAME`, `POD_UID`, `POD_IPS`, `NODE_NAME` and `os.Hostname()`; `const gracefulShutdownTimeout = 7 * time.Second`.

- [ ] **Step 1: Write the failing tests** in `main_test.go`, deleting `TestLeaderReadyCheckIsRedBeforeElectionAndGreenAfter` and `TestLeaderReadyCheckDoesNotBlock`:
  - `TestTheManagerReleasesTheLeaseWithinTheGracePeriod`: `managerOptions(...)` has `LeaderElectionReleaseOnCancel == true` and `*GracefulShutdownTimeout == 7*time.Second`, which is below the chart's `terminationGracePeriodSeconds: 10` (say so in the message).
  - `TestAStandbyWarmsItsControllers`: `*opts.Controller.EnableWarmup == true`.
  - `TestPodIdentityNeedsTheDownwardAPI`: missing `POD_NAME`, `POD_UID` or `POD_IPS` → error naming the variable; all set → the parsed `Identity`.
- [ ] **Step 2: Write** `TestAStandbyTakesOverTheAgentEndpoint` in `standby_envtest_test.go`: one namespace with the Service; two managers from `managerOptions(managerFlags{leaderElect: true, operatorNamespace: ns, metricsAddr: "0", probeAddr: "0"})`, each with a `Publisher` (`op-a`/`uid-a`, `op-b`/`uid-b`, both `Hostname` from `os.Hostname()`, an already closed `Serving`). Within 20 s exactly one slice exists and names one of the two UIDs; cancel that manager; within 10 s the slice names the other UID; the first manager's `Start` returned without error.
- [ ] **Step 3: Run** `nix develop -c go test ./cmd/spawnery-operator -count=1`. Expected: FAIL.
- [ ] **Step 4: Implement.** Remove `leaderReadyCheck` and its `AddReadyzCheck`; set the three manager options; add the flag; when it is true, build the identity (exit with the error otherwise) and `mgr.Add` a `Publisher` with `Client: directClient`, `Namespace: operatorNamespace`, `Service: podspec.AgentServiceName`, `Port: agentserver.DefaultPort`, `Lease: "spawnery-operator.spawnery.cloud"` (one constant shared with `managerOptions`), `Interval: agentendpoint.DefaultInterval`, `Serving` from the agent server. Document the flag and the new readyz behaviour in `operator-flags.md` (`TestFlagsAreDocumented` and `TestFlagDefaultsAreDocumented` check it).
- [ ] **Step 5: Run** step 3's command. Expected: PASS.
- [ ] **Step 6: Commit** `feat(operator): standby replicas are Ready and hand over the agent endpoint`.

### Task 5: Chart

**Files:**
- Modify: `charts/spawnery/templates/{deployment,service,servicemonitor,prometheusrule}.yaml`, `charts/spawnery/values.yaml`, `charts/spawnery/values.schema.json`, `internal/rbacaudit/deploy_envtest_test.go`
- Create: `charts/spawnery/templates/{metrics-service,poddisruptionbudget}.yaml`

**Interfaces:**
- Consumes: the flag and environment variables from Task 4.
- Produces: values `operator.replicas` (integer ≥ 1, default 2), `operator.topologySpreadConstraints` (list, default empty).

Deployment: `replicas: {{ .Values.operator.replicas }}`; `RollingUpdate` with `maxSurge: 1`, `maxUnavailable: 0`, and the comment about Recreate goes; env `POD_NAME` (`metadata.name`), `POD_UID` (`metadata.uid`), `POD_IPS` (`status.podIPs`), `NODE_NAME` (`spec.nodeName`); `topologySpreadConstraints` from values, else one on `kubernetes.io/hostname`, `maxSkew: 1`, `whenUnsatisfiable: ScheduleAnyway`, over `spawnery.selectorLabels`; `terminationGracePeriodSeconds` stays 10. Service: no `selector`, only the `agent` port. Metrics Service and ServiceMonitor as spec §3.4. PDB `maxUnavailable: 1` when `replicas > 1`. Alert `SpawneryNoLeader` (critical, `for: 2m`): `max(leader_election_master_status{name="spawnery-operator.spawnery.cloud", namespace="{{ .Release.Namespace }}"}) < 1 or absent(leader_election_master_status{name="spawnery-operator.spawnery.cloud", namespace="{{ .Release.Namespace }}"})`, with a description and runbook text in the style of the file's other alerts.

- [ ] **Step 1: Rewrite the tests** in `deploy_envtest_test.go`:
  - `renderedObjectKeys` gains `PodDisruptionBudget/spawnery-operator` and `Service/spawnery-operator-metrics`.
  - `TestAgentServiceReachesTheOperatorPods` becomes `TestTheAgentServiceIsLeftToTheLeader`: name `podspec.AgentServiceName`, empty `Spec.Selector`, exactly one port `agent`/9443, the container still declares `agent: 9443`.
  - `TestTheMetricsServiceReachesEveryOperatorPod`: selector ⊆ pod labels, `component: metrics` label, port `metrics` targets a declared container port.
  - `TestTheOperatorIsReplacedRatherThanRolled` becomes `TestTheOperatorRollsWithoutLosingAnEndpoint`: replicas 2, `RollingUpdate`, `maxSurge` 1, `maxUnavailable` 0.
  - `TestThePDBNeedsASecondReplica`: `renderChartWith(t, "operator.replicas=1")` renders no PDB; the default renders `maxUnavailable: 1` over the selector labels.
  - `TestTheOperatorPodsSpreadAcrossNodes`: the default constraint as above; a value given in `operator.topologySpreadConstraints` replaces it.
  - `TestTheOperatorKnowsItsOwnPod`: the four env vars with their field paths.
  - `TestTheServiceMonitorScrapesTheMetricsService`: with `metrics.serviceMonitor.enabled=true`, the selector is `component: metrics` and the agent Service does not match it.
  - `TestTheNoLeaderAlertNamesTheLease`: with `metrics.prometheusRule.enabled=true`, `SpawneryNoLeader` exists and its expression contains `spawnery-operator.spawnery.cloud`.
- [ ] **Step 2: Run** `nix develop -c go test ./internal/rbacaudit -count=1`. Expected: the new and rewritten tests FAIL.
- [ ] **Step 3: Implement** the templates, values and schema.
- [ ] **Step 4: Run** `nix develop -c make manifests` and `nix develop -c make test`. Expected: PASS; `docs/reference/chart-values.md` and the alerts section of `metrics-and-alerts.md` are regenerated.
- [ ] **Step 5: Commit** `feat(chart): two operator replicas behind a leader-written agent endpoint`.

### Task 6: e2e

**Files:**
- Modify: `test/e2e/e2e_test.go:115-230` (`operatorPod`, `operatorLog`, `theOperatorWasNeverDenied`), failure dumps in `hack/e2e.sh`, `hack/e2e-tutorial.sh:39`, `hack/e2e-worldsync.sh:38`, `hack/e2e-ondemand.sh:43`, `Makefile`
- Create: `test/e2e/ha_test.go`, `test/e2e/ha-kind.yaml`, `hack/e2e-ha.sh`

**Interfaces:**
- Produces in `test/e2e`: `operatorPods(t, ns) []corev1.Pod`; `operatorPod(t, ns) *corev1.Pod` now returns the pod whose name prefixes the Lease's `holderIdentity` before `_`; `theOperatorWasNeverDenied` reads every operator pod's log; `serversStayReady(t, ns string, hold time.Duration, during func())` polls every Server's `status.phase` every 500 ms from before `during` until `hold` after it returns and fails on any phase other than `Ready`.

`hack/e2e-ha.sh` follows `hack/e2e-worldsync.sh`: builds the operator, Purpur and Velocity images, creates the cluster from `ha-kind.yaml` (control plane and three workers), installs the chart with the defaults (two replicas), and runs `SPAWNERY_E2E_HA=1 go test -tags e2e -run 'TestHA' ./test/e2e/...`. Every `TestHA*` skips unless `SPAWNERY_E2E_HA=1`. Each sets up one Network with a proxy group and an ephemeral group of two servers, scheduled by the group's `affinity` away from both operator pods' nodes, and waits until both are `Ready` and registered with the proxy.

- [ ] **Step 1: Switch the shared helpers** to multiple replicas and run `nix develop -c make e2e` (single node, two replicas now). Expected: PASS. The dumps use `kubectl logs -l app.kubernetes.io/name=spawnery,app.kubernetes.io/component=operator --prefix --tail=-1`.
- [ ] **Step 2: Write** `ha_test.go`. Every case asserts, through `serversStayReady` with `hold` = 60 s (`phase.ReconnectGrace` plus 15 s), that no Server leaves `Ready`, that no proxy log line matches `spawnery: unregistered backend`, and that afterwards the slice `spawnery-operator-agent-ipv4` names the new leader's UID:
  - `TestHAPlannedHandoverKeepsServersReady`: `kubectl delete pod` on the leader.
  - `TestHACrashedLeaderKeepsServersReady`: the same with `--grace-period=0 --force`.
  - `TestHARolloutKeepsServersReady`: `kubectl rollout restart deployment/spawnery-operator`, then `rollout status` succeeds, then `helm upgrade spawnery charts/spawnery --reuse-values --wait` returns 0.
  - `TestHALostLeaderNodeKeepsServersReady`: pause the leader's node container (`podman` when `KIND_EXPERIMENTAL_PROVIDER=podman`, else `docker`), unpause in `t.Cleanup`. Besides the shared assertions: after unpausing, for 60 s the slice keeps naming the new leader. Log how long the first reconnect took, for the commit body.
  - `TestHAStandbyExportsNoLeaderOnlySeries`: port-forward the standby's 8080; `/metrics` has `leader_election_master_status` and neither `spawnery_ca_expiry_timestamp_seconds` nor `spawnery_serving_cert_expiry_timestamp_seconds`.
  - `TestHAUpgradeFromASelectorServiceLeavesOnlyTheLeadersSlice`: in its own namespace, `helm install` the latest released chart from `oci://ghcr.io/spawnery/charts/spawnery`, then `helm upgrade` to `charts/spawnery`; within 40 s the only EndpointSlice labelled `kubernetes.io/service-name=spawnery-operator` is the leader's and no `Endpoints/spawnery-operator` exists.
- [ ] **Step 3: Run** `systemd-run --scope --user --property=Delegate=yes -- nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e-ha` (on `dev` with the `XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS` and `TMPDIR` exports from `~/.claude/CLAUDE.md`). Expected: PASS. If the node-loss case leaves `Ready`, the measured reconnect time decides the fix (spec §6: bound the connect timeout); stop and report the numbers.
- [ ] **Step 4: Prove the lease check** in a throwaway worktree: drop the holder check from `Assert`, rerun `TestHALostLeaderNodeKeepsServersReady`, record the failure, remove the worktree.
- [ ] **Step 5: Commit** `test(e2e): handovers, a lost node and an upgrade keep servers Ready`.

### Task 7: Docs

**Files:**
- Modify: `docs/guides/upgrading.md`, `docs/guides/updates-and-drain.md:566`, `docs/tutorial/index.md` (only if a command breaks with two pods), `charts/spawnery/templates/NOTES.txt`
- Create: a section "Running more than one operator replica" in the docs page that covers operating the operator (pick the existing page the nav lists for operations; create none if one fits)

- [ ] **Step 1: Write** the replica section: what a standby buys (a lost node), what it costs (memory, one or two agent reconnects per rollout), how to find the leader (`kubectl -n spawnery-system get lease spawnery-operator.spawnery.cloud -o jsonpath='{.spec.holderIdentity}'`, pod name before `_`), and that game images must not move ahead of the operator.
- [ ] **Step 2: Add the rollback note** from spec §3.5 to `upgrading.md`, with the `kubectl delete endpointslice -l endpointslice.kubernetes.io/managed-by=spawnery.cloud-operator` line.
- [ ] **Step 3: Fix commands** that read `logs deployment/spawnery-operator` for something only the leader logs.
- [ ] **Step 4: Run** `nix develop -c make test` (docs-length and chart-version linters). Expected: PASS; raise a ceiling in `hack/docs-length.sh` only with the reason in the commit.
- [ ] **Step 5: Commit** `docs: standby replicas, finding the leader, rolling back`.
