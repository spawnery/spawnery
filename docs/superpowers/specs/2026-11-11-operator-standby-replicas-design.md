# Operator standby replicas: the leader publishes its own agent endpoint

**Status:** design, decided 2026-11-11
**Date:** 2026-11-11

## 1. What goes wrong today

The operator runs as one pod, and nothing takes over while that pod is gone.

Everything stateful in the operator is already bound to leader election: the
controllers, the agent gRPC endpoint (`internal/agentserver`), the two
fan-outs (`serverreg`, `proxyreg`), `statepush`, the certificate provider and
the world-sync sweepers all return `NeedLeaderElection() == true`. Only the
cache and the RBAC self-check (`rbacaudit.Checker`) run on every replica. The
`--leader-elect` flag defaults to on so that "extra replicas are not an
architecture change later".

The chart still pins `replicas: 1` and `strategy: Recreate`, and the helm-chart
design (2026-08-19) leaves `replicas` out on purpose. The reason is how agents
find the leader. `/readyz` has a `leader` check (`leaderReadyCheck` in
`cmd/spawnery-operator/main.go`) that stays red until the replica holds the
lock, so the `spawnery-operator` Service lists only the leader
(agent-channel design §7.2). That keeps agents off a standby, and it also
means:

- A standby is never Ready, so the Deployment reports 1/2 available forever.
  `helm upgrade --wait` times out, a Flux `HelmRelease` health check fails
  (and remediation may roll back), and `KubeDeploymentReplicasMismatch` fires.
- A rolling update deadlocks. The new pod cannot become Ready while the old
  leader holds the lock, and the old leader is not removed until the new pod
  is Ready. Hence `Recreate`.

What a single replica costs is the loss of the node it runs on. The pod is
evicted only after the default `unreachable` toleration (300 s), then it is
scheduled, pulls its image, syncs its cache and waits for the old lease to
expire. For about five minutes nothing scales, nothing drains, on-demand
starts fail and no server enters or leaves a proxy's list. Running game
servers keep running, and their agents keep retrying.

## 2. Goals, non-goals, anti-goals

Goals:

- The chart runs more than one operator replica, and a standby takes over
  from a crashed or evicted leader within seconds.
- Every replica is Ready when it is healthy, so the Deployment rolls with
  `RollingUpdate`, `helm --wait` and Flux health checks pass, and a node drain
  respects a PodDisruptionBudget.
- A planned handover (rollout, drain, `kubectl delete pod`) releases the lease
  at once instead of waiting for it to expire.
- No game server rolls on upgrade, and the serving certificate's names do not
  change.

Non-goals:

- Active-active. The agent registry (`internal/agent`) is in memory and the
  controllers read it directly. Splitting agents across replicas would mean a
  shared registry or forwarding between replicas, which is a different
  project.
- Faster lease timing. controller-runtime's defaults (15 s lease, 10 s renew
  deadline, 2 s retry) stay as they are.
- Changing how agents reconnect. The session loop's backoff (1 s doubling to
  30 s) and `phase.ReconnectGrace` (45 s) stay as they are. What changes is
  how fast an agent notices that its operator is gone (§3.6).

Anti-goals:

- A Ready Server leaving `Ready`, or being deregistered from a proxy, because
  of a handover. A failover has to look to the control loop exactly like an
  operator restart does today, which the registry already tolerates.
- Agents connecting to a standby. The standby does not run the gRPC server,
  and an agent that reached one would get connection refused at best.
- Two live replicas in the agent Service's endpoints at once, even briefly.
  A dead leader's address may linger until the next leader overwrites it
  (§3.2); a live one may not.
- A standby's metrics firing an alert. See §5.

## 3. Design

### 3.1 The agent Service has no selector

`spawnery-operator` keeps its name, namespace and port `agent: 9443`. It loses
its `selector`. The name stays because it is the single source of the
serving certificate's SANs (`certs.ServingDNSNames`) and of the dial address
every game pod carries (`agentEndpoint`); renaming it would roll every server
and reissue the certificate.

With no selector, Kubernetes writes no endpoints for the Service. The leader
writes them itself, as one `discovery.k8s.io/v1` EndpointSlice per IP family
of its pod:

- name `spawnery-operator-agent-ipv4` or `-ipv6`, in the operator namespace,
- labels `kubernetes.io/service-name: spawnery-operator` and
  `endpointslice.kubernetes.io/managed-by: spawnery.cloud-operator` (not the
  EndpointSlice controller's value, so that controller leaves it alone),
- an owner reference to the Service, so `helm uninstall` takes the slices
  with it,
- one endpoint: the pod's address for that family, `conditions.ready: true`,
  `targetRef` to the pod, `nodeName`,
- one port: `name: agent`, `port: 9443`, `protocol: TCP`. For a selectorless
  Service the Service's `targetPort` is not consulted; the slice's port is
  what kube-proxy uses, and its name has to match the Service port's.

The pod learns its addresses and name from the downward API (`status.podIPs`,
`metadata.name`, `metadata.uid`), alongside the existing `POD_NAMESPACE`.

### 3.2 Who writes the slice, and when

A new leader-bound runnable owns the slices (`NeedLeaderElection() == true`).
It lives beside the agent server, because its first write must follow the
listener:

1. **On election**, after `agentserver` has bound its listener and called
   `MarkServing`, it creates or overwrites both slices with this pod. An
   existing slice naming another pod is replaced outright, because the
   previous leader is gone or about to be.
2. **While leading**, it re-asserts the slices periodically (30 s, my call),
   so a slice deleted or edited by hand comes back. It uses an uncached client,
   like `certs.Store` does for Secrets: with `--namespace` set, the cache does
   not cover the operator's own namespace.
3. **On shutdown** (its context is cancelled), it empties the slices'
   endpoint lists, but only if they still name this pod's UID. A replica that
   lost the lease late must not wipe the new leader's endpoint. The emptied
   slice makes kube-proxy reject new connections at once, instead of sending
   them to an address that is about to disappear.

A replica that lost the lease without shutting down cleanly (crash, node
loss) leaves its slice behind. The next leader overwrites it in step 1, so
the stale address lives at most for the lease duration plus one retry
period, about 17 s.

### 3.3 Readiness and leader election

- `/readyz` drops the `leader` check and keeps `healthz.Ping`. A standby is
  Ready.
- `managerOptions` sets `LeaderElectionReleaseOnCancel: true`. The lease is
  released only after the other runnables, the leader runnables (in-flight
  reconciles included), the caches and the HTTP servers have stopped. That is
  bounded by `GracefulShutdownTimeout`, 30 s by default; `agentserver`'s 5 s
  `shutdownGrace` is only one part of it. Left at the default, SIGKILL at
  `terminationGracePeriodSeconds: 10` would come first, the lease would never
  be released, and the standby would wait out the 15 s. `managerOptions`
  therefore sets `GracefulShutdownTimeout` to 7 s (my call), leaving 3 s for
  the release and the exit; the builder confirms that the release lands before
  SIGKILL (§6).
- `managerOptions` sets `Controller.EnableWarmup: true`. Without it a
  standby's cache holds no informer at all: controller-runtime creates them
  when a controller starts its sources, and controllers start only on the
  leader. A takeover would then list every watched type afresh, including every
  pod in the cluster, which `grpcauth.ClientPodChecker` waits on before it
  admits an agent. With warmup the standby starts its controllers' sources
  without leading. The feature is beta in controller-runtime 0.24; §3.4
  already accounts for the doubled watch load.

The order of a planned handover, then:

1. The leader gets SIGTERM. Its slice writer empties the slices; `agentserver`
   stops gracefully and closes the streams.
2. The lease is released.
3. A standby acquires it within one retry period (2 s), starts `agentserver`
   (whose `MarkServing` starts the reconnect grace), then writes the slices.
4. Agents, in backoff since their stream closed, reach the new leader on
   their next attempt.

### 3.4 Chart

- `operator.replicas`, default **2** (my call; it doubles the operator's
  memory and its watch load on the API server, 128 Mi request each by
  default).
- `strategy: RollingUpdate` with `maxSurge: 1`, `maxUnavailable: 0`. This also
  works with one replica now, since the new pod becomes Ready without the
  lock. A rollout costs one or two handovers (the old leader may hand to an old
  standby that is replaced next).
- A PodDisruptionBudget with `maxUnavailable: 1`, rendered only when
  `replicas > 1`. With one replica a PDB would block every node drain.
- A soft `topologySpreadConstraints` on `kubernetes.io/hostname`
  (`maxSkew: 1`, `whenUnsatisfiable: ScheduleAnyway`) over the selector
  labels, unless `operator.topologySpreadConstraints` is set. Soft, so that a
  single-node cluster still runs two replicas.
- `POD_IPS`, `POD_NAME` and `POD_UID` from the downward API.
- The Service template drops `selector` and the `metrics` port.
- A new Service `spawnery-operator-metrics` with the selector and the
  `metrics` port. Its labels are `app.kubernetes.io/name`, the common labels
  and `app.kubernetes.io/component: metrics` in place of `operator`
  (`spawnery.labels` already sets `component: operator`, and a key holds one
  value). The ServiceMonitor's selector moves from the pod selector labels,
  which the agent Service also carries, to `component: metrics`, so it
  scrapes every replica through this Service alone. The NetworkPolicy needs
  no change: it already admits 8080 and 8081 to every operator pod, and 9443
  from agents.
- RBAC in the operator namespace's Role: `endpointslices` get, list, create,
  update, delete (group `discovery.k8s.io`), core `endpoints` get and delete
  (§3.5), and `services` get for the owner reference. Each needs its
  kubebuilder marker and its entry in `rbacaudit.RequiredNamespaced`.
- The Helm chart design's paragraph "`replicas` is deliberately absent" is
  superseded by this document; the comment above `strategy` in the template
  goes.

### 3.5 Upgrading from a selector Service

`helm upgrade` removes the selector from a Service that already has
endpoints, and both controllers that wrote them leave stale state behind:

- The EndpointSlice controller returns early for a Service without a selector
  and deletes nothing (`syncService` in
  `pkg/controller/endpointslice/endpointslice_controller.go`). Its slice keeps
  the old leader's address, marked ready, after that pod is gone.
- The endpoints controller leaves its `Endpoints` object too. That object
  carries `endpoints.kubernetes.io/managed-by: endpoint-controller` and no
  `endpointslice.kubernetes.io/skip-mirror`, so once the selector is gone the
  EndpointSlice mirroring controller turns it into one more slice with the
  same stale address.

Left alone, kube-proxy would spread new connections over the leader's slice
and two stale ones for good. So every assert in §3.2 also cleans up: the
leader deletes the `spawnery-operator` `Endpoints` object if one exists
(otherwise the mirroring controller writes it back) and every EndpointSlice
labelled `kubernetes.io/service-name: spawnery-operator` whose `managed-by`
is not `spawnery.cloud-operator`. The controller's slices
have generated names, hence the `list` and `delete` verbs in §3.4.

While the upgrade runs, the stale slice still routes to the old leader as long
as it lives, and its established streams continue. Once it is gone, agents
reconnect as they would after an operator restart.

A rollback to a chart with the selector brings the EndpointSlice controller's
slice back, but leaves this operator's slices in place, because their owner,
the Service, still exists. kube-proxy would then route to the old version's
leader and to an address that no longer serves. The rollback note in the docs
says to delete them:
`kubectl -n <ns> delete endpointslice -l endpointslice.kubernetes.io/managed-by=spawnery.cloud-operator`.

### 3.6 Agents notice a dead leader sooner

A planned handover and a crashed pod close the agents' streams, so they
redial at once. A lost node does not: packets to the old leader vanish, and
the agent learns of it only through its HTTP/2 keepalive, which pings after
45 s without received data and gives up 20 s later
(`KEEPALIVE_SECONDS`, `KEEPALIVE_TIMEOUT_SECONDS` in
`agent/common/.../OperatorChannel.kt`). The new leader serves after about
17 s, and `ReconnectGrace` ends 45 s after that, around 62 s. An agent that
noticed at 65 s would come back too late, and its server would leave `Ready`
and be deregistered, which §2 rules out.

Decided 2026-11-11: the agents' keepalive gets shorter.

- Agent: `KEEPALIVE_SECONDS` 15, `KEEPALIVE_TIMEOUT_SECONDS` 5. A dead
  stream is noticed within about 20 s, which leaves the reconnect inside the
  grace with room for one backoff step.
- Operator: `agentserver.MinKeepaliveInterval` from 30 s to 10 s, so a 15 s
  ping earns no strike. `PermitWithoutStream` stays false.
- The cost is one ping per agent every 15 s on an otherwise quiet stream:
  about 13 per second at 200 agents.
- The two numbers are kept apart by a source-reading test, in the style of
  `internal/podspec/kotlin_agreement_test.go` (my call): the agent's keepalive
  must stay above the operator's minimum.

Version skew: the agent change moves `imageVersion`, the operator change
`operatorVersion`. A new agent against an operator that still enforces 30 s
collects strikes and gets a GOAWAY, so the operator has to go first; the
release notes say so. A server pinned to an older image keeps the 45 s
keepalive and the old exposure until it runs a new one. The builder checks
what grpc-java's OkHttp transport does on a `too_many_pings` GOAWAY (whether
it backs its keepalive off or reconnects in a loop), because that decides
whether an operator rollback with new agents in the field is harmless or noisy.

## 4. What does not change

- `phase.ReconnectGrace` counts from `MarkServing`, not from process start
  (`agent.Registry.Lookup`). A standby that has been running for hours and
  then takes over does not see every unknown pod as down for hours.
- The CA and serving certificate live in the `spawnery-agent-tls` Secret;
  a new leader reads the same bundle.
- `DesiredServerHash` and `DesiredProxyHash` do not move: the agent endpoint
  and its environment are unchanged.

## 5. Metrics on a standby

Most `spawnery_*` collectors are written only by leader-bound code, but they
are registered in `init()` on every replica. On a standby they read zero.
Once the standby is scraped, that fires alerts: `SpawneryCAExpiringSoon` and
`SpawneryServingCertificateNotRenewing` (critical) read
`spawnery_ca_expiry_timestamp_seconds` and
`spawnery_serving_cert_expiry_timestamp_seconds`, and zero means "expired in
1970" (`observeExpiry` already avoids writing zero for exactly that reason).

Every collector fed only by leader-bound code is therefore wrapped so that it
collects nothing until `mgr.Elected()` is closed. That covers the
certificate, agent server, network, changeover, fan-out and world-sync
collectors. `spawnery_permissions_missing` stays unwrapped, because the
self-check runs on every replica. The builder takes the list from the
`MustRegister` calls and confirms it against a standby's `/metrics` (§6).

One alert is added (my call): `SpawneryNoLeader`, when no scraped replica
reports `leader_election_master_status{name="spawnery-operator.spawnery.cloud"}`
as 1 for 2 minutes, `absent()` included.

## 6. Verification

Unit and envtest:

- The slice writer creates both families, overwrites a slice naming another
  pod, re-asserts after a deletion, and on shutdown empties only a slice that
  names its own UID.
- In `cmd/spawnery-operator`, two managers against one envtest API server: both
  Ready, exactly one slice naming the leader, and after the leader's context
  is cancelled the other one leads within a few seconds and the slice moves.
  The existing `leaderReadyCheck` tests go with the check.
- A standby's `/metrics` carries no leader-only `spawnery_*` series.

e2e (`make e2e`, which installs the chart with two replicas from now on):

- `operatorPod` returns the leader, read from the Lease's `holderIdentity`,
  instead of asserting there is exactly one operator pod. The failure dumps in
  `hack/e2e*.sh` print the logs of every operator pod.
- Planned handover: with servers `Ready` and a proxy holding them,
  `kubectl delete pod` on the leader. No Server leaves `Ready`, no
  deregistration is sent, the slice names the new leader, and every agent is
  connected again within `ReconnectGrace`.
- Crash: the same with `--grace-period=0 --force`.
- Rollout: `kubectl rollout restart deployment/spawnery-operator` completes,
  and the same holds.
- Node loss: pause the leader's kind node container (`podman pause` /
  `docker pause`), with game servers on other nodes. The same holds: no
  Server leaves `Ready`, and every agent is back within `ReconnectGrace`.
  §3.6 exists for this case, so the test is required. It is also where the
  builder measures how long a connect attempt to the dead address hangs
  before the agent's backoff resumes (the stale slice lives for up to about
  17 s); if grpc-java's connect timeout makes that longer than the margin,
  the plan has to bound it.
- Upgrade: install the latest released chart, then upgrade to this one, and
  check that within one assert period only the leader's slice is listed for
  `spawnery-operator` and no `Endpoints` object of that name exists.

`helm upgrade --wait` against the e2e cluster returns successfully with two
replicas.

## 7. Docs

- `docs/reference/operator-flags.md`: `/readyz` no longer depends on the lock;
  `--leader-elect` describes standby replicas.
- `docs/reference/chart-values.md` is regenerated.
- Commands of the form `kubectl logs deployment/spawnery-operator` pick an
  arbitrary pod. Where they look for something only the leader logs, they
  name the leader from the Lease instead.
- A short section on running more than one replica: what it buys (node loss),
  what it costs (memory, one or two reconnects per rollout), and how to find
  the leader.

## 8. Left to the builder

- Where the slice writer lives (`internal/agentserver` or a small package of
  its own) and how it learns that the listener is bound.
- Whether the re-assert is a timer or a watch on the two slices.
- The wrapper for leader-only collectors.
- Whether a single-stack cluster gets a slice for the missing family at all
  (it should not).
