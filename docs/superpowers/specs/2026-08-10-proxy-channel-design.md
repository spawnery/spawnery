# Design: the operator's proxy side (milestone 3a)

**Date:** 2026-08-10
**Status:** Draft for approval
**Scope:** Milestone 3a — everything the operator needs before a Velocity
process exists. `ProxySession`, the registration fan-out, the ProxyGroup
controller, and the three preconditions the handover marks as acceptance
criteria rather than notes.

This document refines sections 5.2, 5.4, 6.5 and 6.6 of
`2026-08-07-minecraft-cloud-operator-design.md` for the parts it implements.
Where it departs from that document, it says so and why.

## 1. Why milestone 3 is cut into three

Milestone 3 as written in section 11 of the main design — "ProxyGroup, the
Velocity agent, registration, the proxy ready gate, modern forwarding, fallback
routing" — is three independent subsystems, the same way milestone 2 was:

| | Contents | Proved by |
|---|---|---|
| **3a** | `ProxySession`, the `spawnery-proxy` bootstrap, the widened orphan sweep, the gRPC registrar, the ProxyGroup controller, NodePort expose | envtest against a real gRPC proxy client; pure Go tests of the fan-out |
| **3b** | `nix/oci-common.nix`, the pinned Velocity, the second image, the Go configuration renderer, the forwarding secret, `online-mode=false` | `make image-test`, `make image-repro`, Go unit tests of the renderer |
| **3c** | The shared Gradle subproject, the Velocity agent, the proxy ready gate, fallback routing | JUnit, `make agent-test`, a kind cluster — **a player can join** |

3a and 3b are independent of each other; 3c needs both. The four things the
handover says must land together do not straddle the cut: three of them
(`ProxySession`, the bootstrap entry, the sweep filter) are wholly inside 3a,
and the fourth (`oci-common.nix`, and not extending `set_property`) is wholly
inside 3b.

This document specifies 3a only.

## 2. What is already in place

Worth stating, because it is more than the handover suggests and it shrinks the
work:

- **Authentication is complete for proxies.** `grpcauth.RoleForMethod` already
  maps `/ProxySession` to `agent.RoleProxy`; `Authenticate` already refuses a
  server token on a proxy session and a pod whose `spawnery.cloud/role` label
  disagrees with the requested role; `podspec.ProxyServiceAccountName` already
  exists. `internal/grpcauth/auth_envtest_test.go` already mints proxy tokens
  against a real API server. The one change 3a makes there is additive: the
  pod's group label is carried out onto the `Identity` (section 5).
- **The registry already knows `RoleProxy`** and `Connect`/`Supersede`/
  `Disconnect` are role-agnostic.
- **The proto already carries the whole proxy direction** — `FullSync`,
  `RegisterServer`, `UnregisterServer`, `DrainPlayers`, `Heartbeat`,
  `PlayerJoinedServer`. No wire change is needed, only one corrected comment
  (section 8).
- **`controller.Registrar` is already the seam.** The Server controller calls
  `Register`, `Deregister` and `Drain` through it and `NoopRegistrar` is wired
  in `main.go`. 3a replaces the implementation, not the call sites.
- **The manager cache is unrestricted for `Server` objects.** `main.go`
  narrows `Cache.ByObject` for ConfigMaps and ServiceAccounts only, so a
  `client.Reader` over the manager's cache is a complete source for `FullSync`.

## 3. Decisions

**One expose strategy, NodePort.** Section 11 of the main design puts all three
strategies in milestone 6. 3a implements NodePort and refuses the other two
with `Accepted=False`, reason `ExposeStrategyNotImplemented`. NodePort is the
only strategy that works on the local kind cluster without MetalLB, so it is
the only one 3c's evidence run could exercise; a LoadBalancer path written now
would reach milestone 6 having never run. HostPort is forbidden by Pod Security
`restricted` regardless.

**The network scope is the namespace.** `grpcauth.Identity` carries no network,
and it does not need to: exactly one `Network` may exist per namespace, so the
proxy pod's namespace *is* its network. `FullSync` is scoped to it. This costs
no extra pod read and no new label plumbing.

**`ProxyGroup.status.readyReplicas` comes from the pod conditions**, not from
the agent registry. Section 6.6 makes the Velocity agent serve the pod's
readiness probe, so the kubelet has already written the answer into
`pod.status`. Reading it from the registry instead would be a second truth
about the same event.

**Readiness stays a latch; the registry contract does not change.** A proxy
that drains wants to lower a readiness it already reported, and the contract in
`internal/agent/registry.go` cannot express that (`Hello{ready:false}` cannot
lower a recorded readiness). Section 6.6 is explicit that the proxy ready gate
"concerns startup only" — a proxy that is already ready stays ready when its
stream breaks. Proxy drain arrives with milestone 4, and if it needs a lowerable
readiness, that is a change to the milestone 2a contract and belongs there.

**Incremental messages, plus a periodic full resync.** Section 5.2 specifies
`FullSync` on connect and incremental `RegisterServer`/`UnregisterServer`
afterwards. 3a adds a periodic `FullSync` to every live session. The reason is
in section 6.

**The `wasRegistered` ordering fix lands here.** `applyDecision` calls
`Registrar.Register` and only afterwards writes `status.wasRegistered = true`.
While the registrar was a no-op that window was harmless. From 3a it is real:
if the status write is lost while players are already joining, a deletion in
that window takes the branch "never registered → terminate immediately, no
drain", which throws players out instead of moving them. The fix is one extra
`Status().Update` before the side effect, once per pod lifetime.

## 4. Components

### 4.1 `internal/proxyreg` — the fan-out port

A new package. It owns the set of live proxy sessions, keyed by pod UID and
grouped by namespace, and it is the only thing that knows how a registration
becomes a message.

```go
// Fleet is the set of live proxy sessions. Safe for concurrent use.
type Fleet struct { ... }

type Options struct {
    // Reader lists Server objects for FullSync. The manager's cached client.
    Reader client.Reader
    // ResyncInterval is how often every live session is re-sent a FullSync.
    ResyncInterval time.Duration
    // OutboxSize bounds how far a session may fall behind before it is cut.
    OutboxSize int
}

func New(opts Options) *Fleet

// Join enters a session and returns its outbox. The first message on the
// channel is always the FullSync. group is the session's ProxyGroup, which
// decides the fallback groups its DrainPlayers messages carry.
func (f *Fleet) Join(ctx context.Context, namespace, group, podUID string) (<-chan *agentpb.OperatorToProxy, func(), error)

// Register, Deregister and Drain satisfy controller.Registrar.
func (f *Fleet) Register(ctx context.Context, srv *spawneryv1alpha1.Server) error
func (f *Fleet) Deregister(ctx context.Context, srv *spawneryv1alpha1.Server) error
func (f *Fleet) Drain(ctx context.Context, srv *spawneryv1alpha1.Server) error

// Start runs the resync ticker. It satisfies manager.Runnable and is
// leader-bound, for the same reason the agent endpoint is: only the leader
// holds the streams.
func (f *Fleet) Start(ctx context.Context) error
func (f *Fleet) NeedLeaderElection() bool
```

`internal/proxyreg` imports `api/v1alpha1`, `internal/agentpb` and
`sigs.k8s.io/controller-runtime/pkg/client`. It does not import
`internal/controller` — the `Registrar` interface is declared there and
satisfied structurally, so there is no cycle. `internal/agentserver` imports
`internal/proxyreg`; `internal/controller/setup.go` wires it.

This mirrors `internal/agent`: that package is the port the gRPC server writes
into and the controllers read from; this one is the port the controllers write
into and the gRPC server reads from. Both directions cross the same kind of
seam, which is why neither one lives inside `agentserver`.

**The ordering invariant, and why it is a property of the code rather than of a
test.** `Join` does four things under the same mutex the broadcasts take:
enter the outbox into the set, list the servers, push the `FullSync`, and push
one `DrainPlayers` per draining server. Because the outbox is a FIFO and no
broadcast can acquire the mutex in between, `FullSync` is by construction the
first message a session sees, and any concurrent registration is queued behind
it. The list is a read from the informer's in-memory indexer, so holding the
mutex across it costs a map walk, not an API round trip.

What that invariant does **not** cover is cache staleness — see section 6.

### 4.2 `internal/agentserver.ProxySession`

The mirror of `ServerSession`, and deliberately built out of the same parts:

1. `grpcauth.IdentityFrom` for the identity, `Unauthenticated` without one.
2. `s.sessions.enter` for make-before-break, `Supersede` versus `Connect` on
   the registry exactly as the server side does it, and the same rule that only
   the current generation may report the disconnect.
3. `ReportInterval` and `SessionDeadline` sent first, before anything else.
4. `proxyreg.Join`, then a `time.AfterFunc` hard deadline, then a loop over
   the receive channel, the error channel, the outbox and `ctx.Done()`.
5. `OpenStreams.WithLabelValues("proxy")` — the metric is already labelled by
   role and has only ever been incremented for one of them.

Inbound messages:

- `Hello` — logged at V(1) with its version. A proxy's `ready` field is not
  meaningful (section 3).
- `PlayerCount` — `Agents.ReportPlayers`, with the slots correction of
  section 8.
- `Heartbeat` — nothing. The stream is its own liveness signal and the
  registry's staleness rule already derives from `ReportInterval`. Accepting it
  and doing nothing is the honest implementation; inventing a second liveness
  path would be a second truth.
- `PlayerJoinedServer` — accepted and ignored, at V(1). Nothing in milestones 3
  or 4 consumes it; player counts come from the servers. It exists on the wire
  for the dashboard in project 4.

The handler reads from `proxyreg` and never writes into it.

### 4.3 `internal/podspec/proxy.go`

`BuildProxyPod(net, group, name, agentEndpoint)` next to `BuildServerPod`:

- `ProxyLabels(network, group)` — `managed-by`, `network`, `group`,
  `role=proxy`. No `server` label: proxies have no `Server` object, and that
  absence is what the orphan sweep's existing `serverName == ""` guard already
  keys on.
- `ServiceAccountName: ProxyServiceAccountName`,
  `AutomountServiceAccountToken: false`, the same projected volume carrying the
  audience-bound token and the CA.
- `emptyDir` on `/data` and `/tmp`; no PVC. Proxies hold no state worth keeping
  — the cross-proxy player state is explicitly deferred in section 2 of the
  main design.
- Port 25565 (`minecraft`) and port 8081 (`ready`).
- `terminationGracePeriodSeconds` from `spec.drain.timeoutSeconds`.
- The same `securityContext`: non-root, read-only root filesystem, all
  capabilities dropped, `RuntimeDefault` seccomp.
- The same `checkMountCollision` rules for user mounts.

The forwarding secret and the rendered configuration are **not** here. They
belong with the renderer that consumes them, and adding them to both pod
builders in one change (3b) is what keeps the server and proxy layers from
drifting into two different answers about where the secret lives.

**The readiness probe is a `tcpSocket` on port 8081.** Velocity speaks no HTTP,
so an `httpGet` probe is out; an `exec` probe would need another binary in the
image and a second thing to keep bit-reproducible. A dedicated port the agent
binds only after it has processed its first `FullSync` is kubelet-native, needs
nothing in the image, and cannot turn green before the agent has a server list
— which is precisely what section 6.6 asks for. 3a writes the contract; 3c
honours it.

### 4.4 `internal/controller/proxygroup_controller.go`

Mirrors `ServerGroupReconciler` in shape, and is simpler because proxies are
fungible: there is no per-proxy CR and therefore no state machine.

- Resolve `spec.networkRef`; if the `Network` is missing or not accepted, set
  `Accepted=False` and requeue after `networkRetryInterval`, reusing
  `networkNotAcceptedMessage`.
- Refuse `expose.type` other than `NodePort` with `Accepted=False`, reason
  `ExposeStrategyNotImplemented`. This is a terminal refusal, not a requeue.
- `Bootstrapper.Ensure(namespace)` before the first pod.
- List the group's pods; create up to `spec.replicas` with `NewProxyName(group)`
  — the same generator and the same alphabet as `NewServerName` — and delete the
  surplus, oldest first. Rolling updates on image change are milestone 4.
- Ensure the NodePort `Service`, named after the group, selecting
  `managed-by`, `network`, `group`, `role=proxy`, port 25565 → the group's
  `expose.nodePort.port`. Owned by the ProxyGroup, so deleting the group
  cascades.
- Status: `readyReplicas` from the pods' `Ready` condition; `connectedPlayers`
  as the sum over the registry; `phase` derived the same way `derivePhase`
  does it (`Degraded` if the condition is set, `Ready` at full ready replicas,
  otherwise `Pending`); `observedGeneration`.

**`status.address` comes from `pod.status.hostIP`.** With NodePort, the address
players need is a node's address plus the node port, and the operator has no
node read right. It does not need one: `hostIP` on a running proxy pod is the
address of a node that demonstrably has a proxy on it, and it is already on an
object the operator watches. Granting cluster-wide node reads for a status
string would be the same trade the bootstrapper already refused when it
declined the `update` verb on ServiceAccounts to restore a cosmetic label. If
no proxy pod is ready, the field stays empty.

### 4.5 `Bootstrapper`

`ensureServiceAccount` becomes `ensureServiceAccounts` and creates both
`spawnery-server` and `spawnery-proxy`. Every property its comment defends
stays: Get-then-Create by construction, no `update` verb, `AlreadyExists`
treated as success. A namespace that will only ever run servers gets an unused
ServiceAccount, which is cheaper than teaching `Ensure` a role parameter that
both callers would have to get right.

### 4.6 The orphan sweep

Three changes to `OrphanReconciler.Sweep`:

1. List by `podspec.LabelManagedBy` only. Today it also filters on
   `role=server`, so every proxy pod is absent from `liveUIDs` and every proxy
   registry entry is forgotten within one sweep interval — the first Velocity
   agent to connect would be dropped from the registry a minute later, with
   nothing in the log tying the two events together.
2. The Server-existence check keeps running for server pods only. The existing
   `serverName == ""` guard already achieves that once the filter widens,
   because proxy pods carry no `spawnery.cloud/server` label — but the sweep
   must say so in a comment, since that is now load-bearing rather than
   incidental.
3. Delete a proxy pod whose `ProxyGroup` is gone, mirroring the Server case.
   Nothing else would ever remove it.

### 4.7 The `wasRegistered` ordering fix

In `applyDecision`, when `d.Register` is decided and
`srv.Status.WasRegistered` is not yet true: set the flag, persist it with
`Status().Update`, and only then call `Registrar.Register`. On a conflict,
return the error and let the reconcile retry — registering a server twice is
idempotent, and the resync would repair it in any case.

This costs one extra status write per pod lifetime, at the single transition
into `Ready`.

### 4.8 Wiring and RBAC

`controller.Options` gains nothing: `Registrar` is already a field, and
`main.go` swaps `NoopRegistrar{}` for the `*proxyreg.Fleet`. `SetupAll` adds
the ProxyGroup controller and `mgr.Add(fleet)` for the resync ticker.
`agentserver.Options` gains a `Proxies *proxyreg.Fleet` field.

`NoopRegistrar` stays. It is what the controller tests use to observe decisions
without a fan-out behind them, and the `Registrar` interface exists precisely
so that substitution is possible.

New RBAC markers:

- `services`: `get;list;watch;create;update`
- `proxygroups`: `get;list;watch`; `proxygroups/status`: `update`

`internal/rbacaudit` covers the new verbs, as it already does for the rest.

## 5. Data flows

**A proxy connects.** `grpcauth` checks the token, the ServiceAccount and the
role label. `sessions.enter` decides `Connect` versus `Supersede`.
`ReportInterval` and `SessionDeadline` go out first. `Join` then delivers, in
this order and under one lock, `FullSync{servers}` followed by one
`DrainPlayers{fromServer, toGroups}` for every `Server` in the namespace whose
phase is `Draining`. Both are derived from the CR status, so a reconnect in the
middle of a drain reconstructs the same state rather than undoing the
deregistration — which is the failure section 5.2 introduced the rule for.

**What `FullSync` contains.** Every `Server` in the proxy's namespace with
`status.registered == true` and a non-empty `status.address`, as
`RegisteredServer{name: srv.Name, address: srv.Status.Address, group:
srv.Spec.GroupRef.Name}`.

Section 5.2 of the main design words this as "exactly the registered servers,
meaning those in phase `Ready`". 3a keys on `status.registered` instead, and
the difference is deliberate: that flag is the record of what the operator
actually told the proxies — `applyDecision` writes it in the same block that
calls `Register` and `Deregister` — whereas the phase is the record of what the
server is. The two can disagree for a reconcile, and in that window the flag is
the one that matches what the proxies were sent.

**Where `toGroups` comes from.** `routing.fallbackGroups` of the ProxyGroup the
receiving session belongs to — not a union across the namespace. Two
ProxyGroups of one network may legitimately route to different fallbacks, and a
union would tell a proxy to move players onto a group it has no servers for.

That means a session has to know its own group, and `grpcauth.Identity` does
not carry it today. It costs nothing to add: `ClientPodChecker.PodExists`
already fetches the pod and already reads two of its labels, so it returns the
`spawnery.cloud/group` label alongside its verdict and `Authenticate` puts it
on the `Identity`. The alternative — a second pod read inside `proxyreg` —
would read the same object twice per session for a value the authenticator
already had in hand.

**A server is registered.** `applyDecision` persists `wasRegistered`, then
calls `Register`, which pushes `RegisterServer` into every outbox in the
namespace. `Deregister` and `Drain` work the same way. A push that would block
is handled as in section 6.

**Resync.** Every `ResyncInterval` (default 30 s), each live session receives
the same construction `Join` builds: a fresh `FullSync` plus the drain
repetitions. For the agent this is a diff against its own list, so an unchanged
list produces no churn.

**Operator restart.** The outboxes are gone, the streams break, the agents
reconnect on their own backoff, and `FullSync` is rebuilt from etcd. There is
no in-memory state that has to survive a restart, and that is the point: the
proxies' view of the world is a function of the CRs, not an accumulation of
events.

## 6. Error handling

**A proxy that falls behind.** The outbox is buffered at `OutboxSize` (64). If
a push would block, the session is cancelled rather than the message dropped.
A dropped message would heal at the next resync, but until then the proxy looks
healthy while routing on a list that is wrong — the failure mode is silent, and
silent is the one thing this subsystem must not be. Cancelling is loud: the
agent reconnects and is rebuilt from scratch. A proxy that cannot accept 64
queued registrations is not serving players either. A counter records it.

**Cache staleness.** This is the one gap the ordering invariant of section 4.1
does not close. `Join` lists through the manager's cache, which can lag the
Server controller's status write. A concrete losing sequence: `Deregister` for
server X is broadcast and pushed into a session's outbox; the same session then
rejoins and its `FullSync` is built from a cache that still shows X as
registered; the proxy ends up with X in its list after having been told to drop
it. The window is short and the resync closes it within one interval.

This is written down because it is the reason the resync exists. Without the
paragraph, the resync reads like a redundant timer and the next person removes
it.

**An unreachable API server.** Already handled: `grpcauth` distinguishes
"refused" from "could not be reached" and maps the latter to `Unavailable`, so
an agent backs off instead of concluding its credentials are wrong. The
`FullSync` list is a cache read and does not fail on an API server outage; it
serves the last known state, which is the correct answer.

**A registrar call with no proxies connected.** Returns nil. There is nothing
to deliver and nothing has gone wrong; a `Network` with no ProxyGroup is a
legitimate state, and it is the one milestone 2c ran in.

**A proxy pod that disappears.** The stream breaks, `Disconnect` runs, and the
widened sweep drops the registry entry within one interval.

## 7. What 3a deliberately does not do

- No Velocity image, no Velocity agent, no `velocity.toml`. 3b and 3c.
- No forwarding secret mount, no `online-mode=false`. 3b, together with the
  renderer that writes them.
- No `NetworkPolicy`. Section 6.5 makes the backends reachable from the proxies
  only, and that barrier is worth nothing until `online-mode` is actually off;
  building it now would give an untestable false sense of the invariant.
  Milestone 6 owns NetworkPolicies, and 3b is where the pairing with
  `online-mode` becomes checkable.
- No PodDisruptionBudget for proxy groups, no rolling updates, no proxy drain.
  Milestone 4.
- No LoadBalancer or HostPort expose. Milestone 6.
- No lowerable readiness in `internal/agent.Registry`. See section 3.

## 8. Contract corrections

**`PlayerCount.slots` for proxies.** The proto comment reads "Proxy agents
leave slots at zero", but `Registry.ReportPlayers` rejects any report where
`players > slots`. A proxy with one player online would therefore have every
report discarded — silently, visible only as a `RejectedReports` counter, and
`ProxyGroup.status.connectedPlayers` would sit at zero forever while players
were connected.

The fix is that **a proxy reports its configured player limit as `slots`**. It
keeps one rule in the registry instead of a role-dependent one, it gives
`status.connectedPlayers` a capacity to sit next to, and a proxy genuinely does
have a capacity — `ProxyGroup.spec.config.playerLimit` already exists in the
API. The proto comment is corrected accordingly. This is a comment change and a
behaviour agreement; the wire format does not move.

3c implements the reporting side. 3a's stub client reports the same way, so the
rule is exercised before the real agent exists.

## 9. Test strategy

| Level | What it measures |
|---|---|
| `internal/proxyreg`, plain Go | `FullSync` is the first message even under a concurrent broadcast; a duplicate `RegisterServer` is idempotent; a broadcast lost to cache staleness is healed by the resync; a full outbox cancels the session; `Leave` removes it; `Join` on a namespace with no servers sends an empty `FullSync` rather than nothing |
| `internal/agentserver`, envtest | A **real** gRPC `ProxySession` client with a real projected token and real TLS. Every assertion is on what the client receives, never on `fleet`'s own counters |
| `internal/controller`, envtest | ProxyGroup creates `replicas` pods and the NodePort Service; `LoadBalancer` is refused with `Accepted=False`; the sweep no longer forgets proxy registry entries; a proxy pod whose group is gone is deleted; `wasRegistered` is persisted before `Register` — asserted with a `Registrar` that records call order and can fail on demand |
| `internal/grpcauth`, envtest | The `Identity` of a proxy token carries the pod's group label; every existing rejection still rejects |
| `internal/podspec`, table tests | `BuildProxyPod`: labels, the projected volume, both ports, the readiness probe, the grace period, mount collisions |
| `internal/rbacaudit` | The new verbs on `services` |

**The second row is the lesson from 2c.** Milestone 2c produced five defects in
a row and not one of them was in the code the tests were checking — every one
was in an assumption about which side of the wire the test measured. Every
assertion about this channel is therefore made on the client's side of it. A
test that reads `Fleet`'s internal maps proves the operator did what the
operator thinks it did, which is exactly the class of test that was green
throughout 2c while the agent leaked a `ManagedChannel` per reconnect.

`make test` stays Go-only and must not get slower than its current ~24 s.

## 10. Acceptance criteria

1. A gRPC client authenticating with a real `spawnery-proxy` token opens a
   `ProxySession` and receives `ReportInterval`, `SessionDeadline` and a
   `FullSync` — in that order — without the operator returning `Unimplemented`.
2. A `Server` reaching `Ready` causes a `RegisterServer` on every open proxy
   session in its namespace; entering `Draining` causes a `DrainPlayers`.
3. A proxy that reconnects during a drain receives a `FullSync` without the
   draining server, followed by its `DrainPlayers`.
4. A registration lost to cache staleness is present in the next resync.
5. A `ProxyGroup` with `expose.type: NodePort` produces `spec.replicas` pods,
   a NodePort Service, and a `status.address` of `<hostIP>:<nodePort>` once a
   proxy pod is ready. `LoadBalancer` and `HostPort` produce `Accepted=False`.
6. A namespace bootstrapped by the operator holds both the `spawnery-server`
   and the `spawnery-proxy` ServiceAccount.
7. An orphan sweep with a connected proxy agent leaves its registry entry
   alone.
8. `status.wasRegistered` is durable before the first `Register` reaches a
   proxy.
9. `make test` is green and no slower than today.

## 11. Questions 3b and 3c inherit

- **Does the Velocity agent share a Gradle subproject with the Paper agent?**
  Decided: yes — `agent/common` holds the session loop, the token source, the
  channel construction, the credentials and the TLS-1.3 `ConnectionSpec`
  override. The cost is that the two agents can no longer be versioned apart,
  and 3c is where that has to be lived with.
- **Where does the forwarding secret reach the backend?** Decided: mounted as a
  file into both layers; Velocity points `forwarding-secret-file` at it
  directly, and a small Go program baked into the image merges `online-mode`
  and the secret into `paper-global.yml`. 3b builds it, on the `buildGoModule`
  path `spawnery-slp` already establishes.
- **Does the operator run inside the cluster for the E2E flow?** Still open.
  Today it runs outside through `go run` and the local flow hand-builds the
  `Service` and `Endpoints` its own pods dial. Workable for one person at a
  terminal, a wall for milestone 6's CI. An operator image is not in 3's scope,
  but 3c's evidence run is where the absence starts to cost.
