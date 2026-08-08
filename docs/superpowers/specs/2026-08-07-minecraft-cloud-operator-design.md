# Design: Spawnery — a Kubernetes-native cloud system for Minecraft networks

**Date:** 2026-08-07
**Status:** Draft for approval
**Scope of this document:** Projects 1 + 2 (operator core and proxy integration)

## 1. Purpose and audience

An open-source cloud system for running Minecraft networks on Kubernetes —
dynamic minigame servers as much as persistent survival worlds, behind a
Velocity proxy layer. The target platform is RKE2 on bare metal, without ruling
out other Kubernetes distributions.

The audience is Minecraft network operators, not platform engineers. The
interface thinks in server groups, not in pods.

### Why a new project

The gap in the market is real. CloudNet v4 and SimpleCloud v3, the two
widespread cloud systems, are not Kubernetes based. The only serious K8s attempt
from that camp, `theSimpleCloud/SimpleCloud-Kubernetes`, was archived unfinished
on 2024-04-05.

The only mature prior art is [Shulker](https://github.com/jeremylvln/Shulker): a
Rust operator built on Agones with the CRD triangle MinecraftCluster →
ProxyFleet → MinecraftServerFleet. Architecturally instructive, but:
MinecraftServerFleets are explicitly ephemeral with no persistent storage, there
is no concept for survival worlds or lobbies holding data; the project has been
feature-stagnant since release v0.13.0 (2025-04-05) with a single maintainer; and
it is under AGPL-3.0.

Alongside it there is
[kubernetes-minecraft-operator](https://github.com/JamesLaverack/kubernetes-minecraft-operator),
which manages individual Minecraft Java servers as `MinecraftServer` objects. It
addresses a different task — no proxy, no server groups, no scaling — and is
therefore not prior art for a network cloud system, though it is evidence that
the operator approach for single servers is already considered obvious.

That makes this project's differentiation concrete:

1. Persistent and ephemeral servers as equal first-class concepts.
2. No Agones dependency — one operator instead of two, no SDK requirement inside
   the server process, no narrow Kubernetes version window.
3. The familiar group UX of the established systems, translated onto Kubernetes.
4. A permissive licence and active maintenance.

**Licence hygiene:** Shulker is AGPL-3.0. Its code is not adopted, only its
architecture read as a reference. The same applies to other third-party
projects: check the licence before any reuse.

### Why not Agones

Agones' core value is dynamic hostPort brokering so clients can connect directly
to an assigned game server. Behind a Velocity proxy that almost entirely falls
away — only the proxies need external reachability. What is actually needed
(Velocity registry, player routing, PVC persistence, template provisioning) is
not what Agones provides.

On top of that: persistent servers do not fit structurally into the fleet model,
whose rolling updates replace GameServers. The core differentiator would
therefore end up being built around the Agones model anyway. The cost side would
be a second operator, mandatory admission webhooks, an SDK sidecar and a
three-minor version window with no bare-metal test coverage.

The good ideas are adopted regardless: the explicit state machine as a status
subresource, protecting occupied servers from eviction, health through standard
probes.

### Why not control-plane-first

Our own API with a database as the source of truth and Kubernetes merely as an
execution layer would be the CloudNet architecture. It gives away exactly what
makes Kubernetes-native valuable — declarative CRDs, GitOps, kubectl,
reconciliation, RBAC — and creates a second consistency problem between database
and cluster state. CLI, dashboard and REST API come later as a thin layer over
the CRDs.

## 2. How the work is cut

The total scope falls into four projects, each with its own spec:

| # | Project | Contents |
|---|---------|--------|
| 1 | Operator core | CRDs, reconciliation, pod lifecycle, ready gates, player-aware drain, expose strategies |
| 2 | Proxy integration | Velocity agent, Paper agent, gRPC channel, modern forwarding, fallback routing |
| 3 | Templates & provisioning | Layered file overlays, OCI/S3 sources, plugin downloads with pinned checksums, backups, image build pipeline |
| 4 | CLI & dashboard | The `spawnery` CLI, web UI, metrics path |

**This document specifies projects 1 and 2 together.** They cannot sensibly be
separated: without proxy integration the operator is not playable, without the
operator the agent has nothing to register.

### Success criterion

*A player connects to the network, lands on a lobby, and during scaling, rolling
updates and restarts of individual backend servers nobody loses their
connection.*

The guarantee explicitly covers the **backend lifecycle**. A proxy restart
disconnects existing connections once its drain window expires, and that is
inherent — the client connection terminates at the proxy, and handing a session
between proxies presupposes the cross-proxy player state that has been deferred.

### Not in this version

Explicitly deferred, not forgotten: the layered template system (V1 uses plain
ConfigMap, Secret and PVC mounts), CLI and dashboard, Redis and cross-proxy
player state, matchmaking, signs and NPCs, permission sync, MOTD and tablist
sync, multi-cluster, BungeeCord, Fabric and Forge, automatic backups, a transfer
command of our own (`/play`), automatically orchestrated forwarding secret
rotation, runtime plugin downloads.

## 3. Architecture at a glance

```
                    ┌──────────────────────────────┐
   kubectl / GitOps │  CRs: Network, ProxyGroup,   │
   ────────────────▶│       ServerGroup, Server    │  (etcd = source of truth)
                    └───────────────┬──────────────┘
                                    │ watch / reconcile
                            ┌───────▼────────┐
                            │    Operator    │  Go, controller-runtime
                            │  (1 replica,   │
                            │   leader elect)│
                            └───┬────────┬───┘
                     create pods│        │ gRPC over TLS (bidirectional)
                 ┌───────────────┘        └──────────────┐
                 │                                       │
        ┌────────▼────────┐                     ┌────────▼────────┐
        │  Proxy pods     │  registry updates   │  Server pods    │
        │  Velocity       │◀───────────────────▶│  Paper          │
        │  + Velocity     │                     │  + Paper agent  │
        │    agent        │   Minecraft traffic │                 │
        │                 │────────────────────▶│                 │
        └────────▲────────┘                     └─────────────────┘
                 │ Expose: LoadBalancer | NodePort | HostPort
             Players
```

The operator is a single Go process. It reconciles CRs into pods and hosts the
gRPC service the in-game agents talk to. There is no second store of data:
high-frequency runtime data (player counts) lives in the operator's memory and is
written into the CR status only in a throttled form.

## 4. API model

**Project name:** Spawnery. **API group:** `spawnery.cloud`,
**version:** `v1alpha1`. All resources are namespaced.

The group name is fixed deliberately early, because changing it later requires
conversion webhooks.

**One namespace, one network.** Staging and production belong in separate
namespaces. The reason is network isolation: NetworkPolicies select over labels,
and two networks in the same namespace could be separated by a network label,
but every additional unmanaged pod in the namespace undermines the assumption.
The operator rejects a second `Network` in the same namespace with a condition.

### 4.1 Network

The root resource. Carries the forwarding secret and the defaults that
subordinate resources inherit.

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: production
  namespace: minecraft
spec:
  forwardingSecretRef:
    name: velocity-forwarding-secret   # key: "secret"
  defaults:
    minecraftVersion: "1.21.4"
    imagePullSecrets:
      - name: registry-credentials
    resources:
      requests: { cpu: "1", memory: "2Gi" }
      limits:   { memory: "2Gi" }
    scheduling:
      nodeSelector: { node-role/minecraft: "true" }
      tolerations: []
      affinity: {}
status:
  conditions: [...]
  proxyGroups: 1
  serverGroups: 3
  onlinePlayers: 42
```

`ProxyGroup` and `ServerGroup` reference their network through
`spec.networkRef.name` and inherit `defaults`, but can override any field.
Resources without a valid reference are not reconciled and report that as a
condition.

### 4.2 ProxyGroup

The Velocity layer, and the only part of the system reachable from outside.

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: edge
  namespace: minecraft
spec:
  networkRef: { name: production }
  replicas: 2
  image: ghcr.io/<org>/velocity:3.4.0-<agentversion>
  resources: {...}
  scheduling: {...}                 # overrides Network.defaults.scheduling
  expose:
    type: LoadBalancer              # LoadBalancer | NodePort | HostPort
    loadBalancer:
      annotations: {}               # e.g. MetalLB pool selection
      externalTrafficPolicy: Local
    # nodePort: { port: 30565 }
    # hostPort: { port: 25565 }
  routing:
    fallbackGroups: ["lobby"]       # ordered try list on join
  drain:
    timeoutSeconds: 300             # how long existing sessions may run out
  config:
    playerLimit: 500
    motd: "§bMy network"
status:
  phase: Ready
  readyReplicas: 2
  address: "203.0.113.10:25565"
  connectedPlayers: 42
  conditions: [...]
```

**Expose strategies.** Exactly one of the three is active; the operator
validates through a CEL rule that the matching sub-block is set.

- `LoadBalancer` creates a `type: LoadBalancer` service. On bare metal that
  requires MetalLB or kube-vip; RKE2 ships no active LoadBalancer controller
  (ServiceLB is opt-in and should stay disabled). The default for
  `externalTrafficPolicy` is `Local` so the client IP is preserved — bans and
  rate limits depend on it.
- `NodePort` creates a `type: NodePort` service. Ports outside the default range
  30000–32767 require adjusting `service-node-port-range` on the API server; we
  document that but do not check it.
- `HostPort` binds a **fixed** port directly on the nodes. There is no port
  allocator of our own: conflict avoidance is handled natively by the
  kube-scheduler, which places at most one pod with the same hostPort per node.
  In practice that means at most one proxy replica per node; if replicas stay
  `Pending` for lack of free nodes, the operator reports that as a condition.
  Whether it works at all depends on the CNI: Canal works, Cilium only with
  `kubeProxyReplacement` or portmap chaining. On CIS-hardened RKE2 clusters pod
  security `restricted` forbids host ports — the namespace then needs an
  exception.

`hostNetwork` is not offered. The latency gain is around half a millisecond and
costs network isolation.

**Player-initiated moves between groups.** V1 ships no transfer command of its
own. Velocity's built-in `/server` (permission `velocity.command.server`, active
for all players by default) is therefore the only player-initiated route between
groups and is deliberately left open. Draining servers cannot be reached through
it because they are deregistered before the drain, and full servers are refused
by Paper itself. The price: players can target individual instances and thereby
bypass load distribution. A `/play <group>` with a group policy in the Velocity
agent follows in a later project.

### 4.3 ServerGroup

The abstraction network admins think in. The `type` field distinguishes the two
modes of operation.

**Ephemeral** — minigames and lobbies, state is lost when they stop:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: minecraft
spec:
  networkRef: { name: production }
  type: Ephemeral
  image: ghcr.io/<org>/paper:1.21.4-<agentversion>
  maxPlayers: 100
  resources: {...}
  scheduling: {...}
  mounts:                          # V1: plain mounts, no overlay system
    - name: lobby-config
      configMap: { name: lobby-config }
      mountPath: /data/config
  scaling:
    minReplicas: 1
    maxReplicas: 10
    spareSlots: 40                 # free player slots kept in reserve
    scaleDownStabilizationSeconds: 300
  update:
    maxUnavailable: 1              # stale servers replaced at the same time
    maxStaleSeconds: 0             # 0 = never actively empty stale servers
  drain:
    timeoutSeconds: 60
  failedRetentionSeconds: 3600
status:
  phase: Ready                     # derived: Pending | Ready | Degraded
  replicas: 2
  readyReplicas: 2
  onlinePlayers: 42
  freeSlots: 158
  conditions: [...]
```

**Persistent** — survival, creative, build servers; the world survives
restarts:

```yaml
spec:
  type: Persistent
  replicas: 1                      # usually 1, stable names survival-0, survival-1, ...
  storage:
    size: 20Gi
    storageClassName: longhorn
    accessModes: [ReadWriteOnce]
  drain:
    timeoutSeconds: 120
  terminationGracePeriodSeconds: 300   # time to save the world
```

**Validation through CEL** (`x-kubernetes-validations` in the CRDs, no admission
webhooks): with `Persistent`, `scaling` and `update` are forbidden; with
`Ephemeral`, `storage` and `replicas` are. In addition `spec.type` and, for
persistent groups, `storage.storageClassName` and `storage.accessModes` are
immutable through a transition rule (`self.type == oldSelf.type`);
`storage.size` may only grow, and actually expanding the PVC additionally
requires `allowVolumeExpansion` on the StorageClass. Changing the type of an
existing group requires deleting and recreating it. All of these rules are pure
cross-field checks on the same object and therefore implementable without a
webhook — which keeps installation down to one `helm install` with no
certificate management.

**`Degraded`** is a condition with a reason (such as `CrashLoopBackoff`,
`NoFallbackAvailable`). A group's phase is derived from it and shows `Degraded`
for as long as the condition is `True`.

### 4.4 Server

A running instance, owned by its ServerGroup. A separate object per instance is
a deliberate choice: `kubectl get servers` shows the state of the network,
events land in the right place, and CLI and dashboard need no data source of
their own later.

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: Server
metadata:
  name: lobby-x7k2
  ownerReferences: [{ kind: ServerGroup, name: lobby, controller: true }]
spec:
  groupRef: { name: lobby }
  ordinal: 0                       # only set for Persistent
  groupGeneration: 7               # snapshot of the group generation at creation
status:
  phase: Ready
  podName: lobby-x7k2
  address: "10.42.3.17:25565"
  players: 12
  slots: 100
  playersUpdatedAt: "2026-08-07T12:34:56Z"
  registered: true                 # currently registered with the proxies
  conditions: [...]
```

#### State machine

The transitions are the only place where registration and deletion are decided.

```
              ┌───────────── readiness loss ─────────────────┐
              ▼                                              │
Pending ──▶ Starting ──▶ Ready ──▶ Draining ──▶ Terminating ─┴─▶ (deleted)
   │            │          │                        ▲
   └────────────┴──────────┴──── Failed ────────────┘
```

- **Pending** — the CR exists, the pod is not created or not scheduled yet.
- **Starting** — the pod runs, but at least one ready signal is missing.
- **Ready** — registered with the proxies, accepting players.
- **Draining** — deregistered, players are being moved. No way back to Ready.
- **Terminating** — empty or the drain timeout was reached, the pod is being
  deleted.
- **Failed** — a startup error or repeated crashes. No way back into service;
  entering it from `Ready` deregisters immediately. The object stays around for
  diagnosis and is cleaned up through `Terminating` after
  `failedRetentionSeconds` (without a drain, since there are no players left).

**Readiness loss (`Ready → Starting`)** is the most important addition over the
naive model: a container restart or a readiness probe going red makes a server
unplayable without it leaving the phase — players would keep being routed onto a
booting server. The transition fires when the readiness probe goes red **or**
the agent stream has been down for more than 15 seconds; the operator
deregisters immediately. The way back to `Ready` requires **both** signals again
(see 6.1) — the ready gate applies to every entry into `Ready`, not only to the
first start. Repeated readiness loss in quick succession counts against the same
counter as repeated crashes and leads to `Failed` once the threshold is passed,
so a flapping server is not registered and deregistered forever.

#### Rolling updates

When the group spec changes, its `generation` goes up. Servers with an outdated
`groupGeneration` are *stale*.

For **ephemeral** groups the changeover runs surge-first and without kicks:

1. Stale servers **no longer** count towards the group's free slots (see 6.3).
   The scaler therefore produces replacements of the new generation by itself.
2. Once enough ready capacity of the new generation exists — at least one ready
   server, mandatory for fallback groups — a stale server goes into **soft
   drain**: it is deregistered and accepts no new joins, but its players stay
   undisturbed until it empties on its own.
3. `update.maxUnavailable` (default 1) limits how many servers of the group are
   in `Draining` or `Terminating` at the same time because of a generation
   change.
4. `update.maxStaleSeconds` (default 0 = unlimited) forces the active drain from
   6.2 once it expires, if a server does not empty on its own.

Without step 1 the update would never terminate: a lobby that, as a fallback
target, practically never empties would stay registered, and its free slots
would prevent new servers from being created.

**Persistent** servers use `Recreate` with a drain in front, because the PVC can
only be bound once.

## 5. Components

Four artifacts, each with one job:

**Operator** (Go, kubebuilder/controller-runtime). Four controllers — one per
CRD — plus the gRPC server in the same process. In V1 it runs with one replica;
leader election is built in from the start so that multiple replicas are not an
architectural change later.

**Velocity agent** (Kotlin plugin). Opens a bidirectional gRPC stream to the
operator on startup: receives registry updates and drain commands, reports
player counts and join events back. Also serves the readiness endpoint of the
proxy pod (see 6.6).

**Paper agent** (Kotlin plugin). Reports the server's readiness and then
periodically its player count and slots. It executes no drain commands — moving
players is exclusively the proxies' business (see 5.2).

**Base images** for Paper and Velocity, versioned, with a preinstalled agent and
an SLP health tool for the readiness probe. That is a deliberate departure from
the `itzg` images: with Shulker, broken upstream images and failed plugin
downloads are a recurring source of failure in the issues.

Provisioning happens **exclusively at image build time**: the Paper jar, the
Velocity jar and the agent plugin are checked against SHA-256 hashes that are
checked into the repository and never fetched from the download source — a
checksum from the same source as the artifact only secures the transport, not
the upstream. No downloads happen at runtime; configurable plugin and template
downloads are project 3.

**V1 supports exactly one Paper/Minecraft version and one Velocity version.**
The version matrix is kept deliberately small: the image tag
`1.21.4-<agentversion>` otherwise creates a maintenance load (new Minecraft
releases within days, CVE rebuilds) that belongs in project 3 as a standing item
and not in the first version.

### 5.1 Pod management without Deployments

The operator creates pods directly — no Deployments, no StatefulSets, for either
server type. The reason is player-aware scale-down: a Deployment decides for
itself which pod it terminates. Fighting a controller with different ideas costs
more than managing ordinals and PVCs ourselves. One code path instead of two.

**Protecting occupied pods from eviction.** The operator maintains the label
`spawnery.cloud/occupied="true"` on pods with players and keeps one
PodDisruptionBudget per group whose selector points at that label and whose
`minAvailable` is kept in step, as an **absolute number**, with the current
count of occupied pods. For pods without a controller carrying a scale
subresource, Kubernetes allows neither `maxUnavailable` nor percentages in a
PDB — the absolute number is the only variant that works. With that, the
eviction API blocks every eviction of an occupied pod.

The annotation `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` is set
in addition, but is explicitly **only** a signal to the cluster autoscaler and
no protection against `kubectl drain`.

So that a node drain does not hang indefinitely, the operator watches for a node
being set `unschedulable` and proactively starts the drain sequence from 6.2 for
the affected servers. The node drain then terminates once the players have been
moved.

### 5.2 The gRPC contract

```protobuf
service AgentService {
  rpc ProxySession(stream ProxyMessage)   returns (stream OperatorToProxy);
  rpc ServerSession(stream ServerMessage) returns (stream OperatorToServer);
}
```

**Operator → proxy:** `FullSync{servers[]}` (on connect),
`RegisterServer{name, address, group}`, `UnregisterServer{name}`,
`DrainPlayers{fromServer, toGroups[]}`, `ReportInterval{seconds}`.

**Proxy → operator:** `Hello{version}`, `PlayerCount{n}`,
`PlayerJoinedServer{player, server}`, `Heartbeat`.

**Operator → server:** `ReportInterval{seconds}`.

**Server → operator:** `Hello{version, ready}`, `Ready`, `PlayerCount{n, slots}`.

Two decisions that follow from failure cases:

**`FullSync` contains exactly the registered servers**, meaning those in phase
`Ready`. Otherwise a proxy reconnect during a drain would undo the
deregistration, and the draining server would get joins again. Immediately after
every `FullSync` the operator sends `DrainPlayers` again to that proxy for every
server in phase `Draining`. Both messages are idempotent and derived purely from
the CR status, so a reconnect or an operator restart during a drain
deterministically reconstructs the same state.

**Ready is a state, not an event.** The Paper agent sets its internal ready flag
on `ServerLoadEvent(STARTUP)` and reports it in **every** `Hello` on connect. The
separate `Ready` message stays as an immediate notification but is not the only
source — otherwise a server would hang in `Starting` forever if the operator
restarted in exactly the window between the probe turning green and `Ready`
being received.

**Report interval:** The operator communicates it on connect through
`ReportInterval`; the default is 5 seconds, for player counts as much as for the
proxy heartbeat. Both sides therefore use the same value by construction, and
the staleness threshold from 6.3 (twice that, so 10 seconds) is unambiguously
derived.

**Drain is proxy-driven.** There is deliberately no drain message to the server:
only the proxy can move players, and the operator sees the server empty through
the server agent's `PlayerCount{n=0}`. A second, server-side drain path would be
a second truth about the same event.

#### Authentication and transport

The gRPC endpoint speaks **TLS only**. On startup the operator issues itself a
serving certificate, stores the CA and the certificate in a secret in its own
namespace, and rotates it in good time before expiry. Since it creates the agent
pods itself, it mounts the CA bundle into every pod; the agents validate against
that pinned CA and nothing else. No cert-manager, no webhook caBundle machinery
— the operator manages its serving certificate itself, and the goal "one `helm
install` is enough" stays intact.

Authentication uses a projected ServiceAccount token that the operator checks
through `TokenReview`. Three decisions turn that into a real identity:

1. **A dedicated audience.** The token is issued with the audience
   `spawnery-operator` and a short lifetime (`expirationSeconds: 600`, the
   kubelet rotates it automatically); the operator sets exactly that audience in
   the TokenReview's `spec.audiences`. Standard API server tokens are therefore
   worthless at the operator, and the replay window of an intercepted token is
   short.
2. **Separate ServiceAccounts for proxy and server pods.** `ProxySession` is
   only authorized for proxy SA tokens, `ServerSession` only for server SA
   tokens. Without that separation a compromised backend pod could open a
   ProxySession, read the entire internal topology through `FullSync` and
   manipulate scaling with forged reports.
3. **Pod identity from the token, not from the message.** The identity of the
   stream comes exclusively from the extra claims of the pod-bound token
   (`authentication.kubernetes.io/pod-name` and `pod-uid`) in the TokenReview
   result. Every report on a stream applies to that pod and no other. If the pod
   name came out of the `Hello` message, a compromised server could report
   `PlayerCount{0}` for a foreign server and have a full server deleted — a
   direct break of the core invariant.

In addition, `automountServiceAccountToken: false` applies on all game server
and proxy pods; the only thing mounted is the projected, audience-bound token.
Only with that is the statement true that the pods carry no Kubernetes
credentials.

As defence in depth the operator discards `PlayerCount` reports larger than
`slots` and damps scale-up on erratic reports.

**Agent dependencies must be shaded and relocated.** Shulker has a documented
protobuf classpath conflict on BungeeCord — the same trap applies to gRPC
libraries in any plugin classpath.

### 5.3 Why the agents do not read the Kubernetes API themselves

The obvious alternative pattern (Kuvel) has every proxy discover pods itself
through an informer. Against it: we need the return channel for player counts
and drain commands anyway, one channel is better than two, and the proxy pods
stay free of Kubernetes credentials (see the audience and automount decisions in
5.2).

The price is that the operator becomes relevant at runtime. That is cushioned:
if the stream breaks, the proxy keeps its last known server list and reconnects
with backoff. An operator restart throws nobody out of the game; only scaling
and new registrations pause.

### 5.4 Configuration rendering

The operator renders one ConfigMap per group with the values derived from
`spec.config` and the `Network` (MOTD, player limit, forwarding mode). The base
image's entrypoint script merges it into `velocity.toml` or `paper-global.yml`
respectively on startup. User mounts from `mounts` take precedence over
defaults, but not over operationally critical fields (forwarding mode,
`online-mode`, ports).

For Velocity, `forwarding-secret-file` points straight at the secret mount.
Paper knows no file reference for the secret, so the entrypoint injects it from
the mount into `paper-global.yml` on startup.

Configuration changes take effect on the next pod restart — consistent with the
update-by-attrition model from 4.4.

## 6. Data flows

### 6.1 A server comes online

1. The ServerGroup controller notices that the free slots have fallen below
   `spareSlots` and creates a `Server` CR.
2. The Server controller creates the pod (for `Persistent`, the PVC first).
   Phase: `Starting`.
3. The pod's readiness probe turns green. It is an **exec probe** calling the
   SLP health tool shipped in the base image — a real server list ping, not a
   plain port check. The kubelet knows no SLP probe type, and a `tcpSocket`
   probe on 25565 would already turn green before the world is loaded.
4. The Paper agent reports `Ready` (or `Hello{ready: true}` after a reconnect)
   over the gRPC stream.
5. **Only once both signals are in** does the phase go to `Ready`. That applies
   to every entry into `Ready`, including after a readiness loss.
6. The operator sends `RegisterServer` to all connected proxies.

The ready gate is two-stage because even a successful SLP answers before plugins
are fully loaded. A player routed in that window lands on a half-loaded server.
Parsing the log for `Done (x.xs)!` would be the third option and is too fragile
for a product.

### 6.2 A server goes offline

The ordering is the real substance of the system:

1. Phase to `Draining`; `UnregisterServer` to all proxies — no new joins from
   here on.
2. `DrainPlayers` to the proxies: players are moved onto the fallback groups
   through `createConnectionRequest`. A `KickedFromServerEvent` redirect catches
   whatever fails in the process.
3. Wait until the server agent reports `PlayerCount{n=0}` or
   `drain.timeoutSeconds` expires.
4. Phase `Terminating`, delete the pod. For `Persistent` the
   `terminationGracePeriodSeconds` for saving the world runs then.

For the soft drain of a stale server (see 4.4), step 2 is skipped: the server is
only deregistered and runs empty without anybody being moved.

**The load-bearing invariant: a pod with players is never deleted.** It holds
equally for scale-down, rolling updates and node drains, and it is the reason no
HorizontalPodAutoscaler is used — CPU-based scaling misses this domain.

**Proxy replacement** runs one at a time: the new proxy becomes ready (gate see
6.6), then the old one goes `NotReady` and disappears from the LoadBalancer
endpoints. Existing connections run out until the proxy is empty or
`drain.timeoutSeconds` expires; remaining players are then disconnected. Unlike
the server drain there is no active moving — the client connection terminates at
the proxy.

### 6.3 Slot-based scaling

Free slots are the sum of `slots - players` over all `Ready` servers **of the
current generation**. Stale servers do not count; without that restriction a
rolling update would never produce replacements (see 4.4).

- **Scale up** as soon as `freeSlots < spareSlots`: create as many servers as
  needed to close the gap, bounded by `maxReplicas`.
- **Scale down** only if, after removing a server, `freeSlots >= spareSlots`
  would still hold, `replicas > minReplicas` applies, and an **empty** server has
  been continuously empty for `scaleDownStabilizationSeconds`.

If a server's player counts are older than 10 seconds (twice the report interval
from 5.2), it counts as occupied. Better one server too many than a kick.

### 6.4 Player counts and etcd

The agents report player counts every 5 seconds. The operator keeps them in
memory — that is where the scaling logic makes its decisions — and writes them
into `Server.status` only every 30 seconds or on a significant change. With 200
servers, unthrottled updates would be dozens of etcd writes per second. The CR
status is there for observers, not for the control loop.

### 6.5 Player forwarding

Modern forwarding only. The secret exists as a Kubernetes secret and is mounted
into both layers; 5.4 describes the rendering. Backends run with
`online-mode=false` and `enforce-secure-profile=false` and are reachable
exclusively from the proxy through the NetworkPolicy the operator creates —
without that barrier every backend server would be freely joinable.

**Secret rotation is a manual maintenance operation in V1.** The operator
detects the change (a secret watch with hash comparison) and reports it as the
condition `ForwardingSecretRotationPending` along with a Kubernetes event; the
restarts follow a documented runbook: roll all backend groups first, then all
proxy groups.

The reason for that honesty: neither Velocity nor Paper accepts two forwarding
secrets at once. There is necessarily a window in which joins and transfers
between already-rotated and not-yet-rotated layers fail with "Unable to verify
player details" — and in exactly that window an automatic drain would want to
move its players onto fallback servers it can no longer reach. Automatically
orchestrated rotation would have to make registration generation-aware
(registering backends only with proxies of the same secret generation) and
buffer drains until the generations match. That is a network-wide coordinated
rollout with error handling of its own, not needed for the success criterion,
and therefore deferred to a later project.

### 6.6 A proxy comes online

A Velocity pod that gets traffic before its agent has the server list
disconnects every player with "no available server". So the Velocity agent
serves the proxy pod's readiness probe itself: it only turns green after the
agent has established the gRPC stream and processed the `FullSync`. A plain TCP
check on 25565 is not enough. Only then does the pod count towards
`status.readyReplicas` and receive LoadBalancer traffic.

The gate concerns startup only. A proxy that is already ready stays ready when
the stream breaks and keeps serving its last known server list.

## 7. Error handling

**Server does not start** (crash loop, broken configuration): after a timeout
the `Server` goes to `Failed`, with a meaningful reason in the status and a
Kubernetes event. A replacement is created, but with exponential backoff per
group — otherwise a broken configuration spins an endless loop of pod creations.
After several consecutive failures the group sets the condition `Degraded`
(reason `CrashLoopBackoff`) and stops trying.

**Operator goes down:** servers and proxies keep running, players notice
nothing, the proxies keep their server list. On restart the operator
reconstructs its state entirely from the CRs and the running pods — it relies on
nothing that only existed in memory. Player counts come back with the agent
reconnects; until then all servers count as occupied, which prevents scale-down
until recovery.

**Agent connection breaks:** the agent reconnects with backoff and receives a
`FullSync` including a repeat of any open `DrainPlayers`. The operator marks the
player count as stale and conservatively treats the server as occupied. If the
stream stays down for more than 15 seconds, that additionally triggers the
`Ready → Starting` transition with deregistration (see 4.4).

**Node dies:** the pods are gone and so are the players — nothing helps against
that. The operator cleans up orphaned `Server` CRs and restores the target
count. For persistent servers this depends on the PVC: with `ReadWriteOnce` the
volume has to be released first, which can hang. The operator detects that and
reports it as a condition instead of waiting silently.

**Node is drained:** the operator detects `unschedulable` and starts the drain
from 6.2 for the affected servers, so `kubectl drain` does not hang on the PDB
of occupied pods.

**Drain timeout:** if players are still online when it expires (the fallback is
full or has itself failed), a requested deletion goes ahead anyway — but loudly,
with an event and a metric. A scale-down aborts in that case instead and tries
again later.

**Proxy fallback unavailable:** if no fallback group is `Ready`, the operator
refuses the drain and sets the condition `Degraded` (reason
`NoFallbackAvailable`) on the ServerGroup whose server was to be drained.
Sending players into the void is worse than letting a server run longer.

**Orphaned pods:** every pod created carries an owner reference and labels. A
periodic sweep finds pods without a CR and CRs without a pod and corrects both
directions.

## 8. Security and RKE2 specifics

The operator itself runs strictly `restricted`-compliant: non-root, read-only
root filesystem, `RuntimeDefault` seccomp, no privilege escalation, all
capabilities dropped.

**RBAC.** Namespace-scoped the operator needs: its own CRDs, pods, PVCs,
services, events, PodDisruptionBudgets, NetworkPolicies and secrets — the last
only with `get`/`watch` and restricted through `resourceNames` to the secrets
referenced in networks. Cluster-scoped it needs a separate, minimal ClusterRole
with `create` on `tokenreviews.authentication.k8s.io` (agent authentication) and
`get`/`list` on `nodes` (address discovery for HostPort).

The ServiceAccounts of the game server and proxy pods carry **no** Role or
ClusterRole bindings whatsoever and have `automountServiceAccountToken: false`.
That is not cosmetic: whoever may create pods may assign them any ServiceAccount
in the namespace — without this rule the operator's `pods/create` permission
would be a stepping stone to every SA in the namespace.

**NetworkPolicies.** The Helm chart ships the network-independent policies:
default-deny ingress for all managed pods, and agent → operator on the gRPC
port. The policy proxy → server on 25565 is created by the **operator** per
`Network`, with the label `spawnery.cloud/network=<name>` in the `podSelector`
and the ingress rule. On a hardened cluster without these policies internal
communication breaks; without them the offline-mode backends would be open.

**Pod security:** The RKE2 CIS profile enforces `restricted` cluster-wide, which
forbids HostPort and hostNetwork. Anyone using the HostPort strategy needs a
`baseline` label or an exception for the game server namespace. The Helm chart
documents that and warns when rendering if `expose.type: HostPort` is combined
with a `restricted` namespace.

**CNI dependency:** HostPort works with Canal, and with Cilium only when
`kubeProxyReplacement` is active or portmap chaining is in place; there are
documented regressions after RKE2 upgrades. That is why a reachability test per
expose strategy belongs in CI and not in the first users' bug reports.

**With HostPort**, RKE2 without an explicit `node-external-ip` sets the internal
IP as the node address — the operator would then report an unreachable address.
That is detected and emitted as a warning.

**Image references** in the shipped manifests are pinned by digest; tags are
mutable. The `image` fields of the CRs accept digest references, and the
documentation recommends them.

## 9. Installation

One `helm install` must be enough, and something playable has to be standing
afterwards. Setup friction is the most frequently named pain point of the
established systems; a years-long RC phase like CloudNet v4's is a negative
example.

The chart installs the CRDs, the operator, RBAC, ServiceAccounts and the
network-independent NetworkPolicies. A shipped example manifest creates a
network with one proxy and one lobby group that works without further
configuration. No admission webhooks, so no external certificate management is
needed — validation runs through CEL rules in the CRDs, and the operator manages
its gRPC serving certificate itself.

## 10. Test strategy

Development is test-driven; the test comes before the implementation.

**Unit tests** cover the decision logic without Kubernetes: the scaling
algorithm, the drain candidate selection and the update ordering, table-driven.
This is where the central invariant is checked — no candidate with players is
ever selected for deletion, not even on stale data. Likewise the state machine:
every permitted and every forbidden transition, including `Ready → Starting` on
readiness loss.

**Controller tests with envtest** bring up a real API server and check
reconciliation against real CRDs: does a ServerGroup create the right pods? Does
pod-ready plus agent-ready cause the phase change, and pod-ready alone not? Is
the PDB kept in step when occupancy changes? Do the CEL validations apply,
including the transition rules with `oldSelf`? Fast enough for every commit.

**End-to-end on k3d and RKE2** with real Paper and Velocity processes, driven by
a headless Minecraft client that actually joins. Only this level proves that
forwarding, registration and drain really work. Core scenarios:

- A join lands on the lobby.
- Scale-down with a player online moves them instead of kicking them.
- A rolling update of an occupied lobby group terminates and loses nobody.
- A persistent server keeps its world across a restart.
- An operator restart in the window between probe-green and `Ready` being
  received still causes the phase change after the agent reconnects.
- A proxy scale-up while a client joins produces no "no available server"
  disconnect.
- A reachability test per expose strategy.

## 11. Milestones

| M | Result |
|---|----------|
| 1 | CRDs and the operator skeleton; ServerGroup creates ephemeral pods; the state machine including readiness loss; the orphan sweep |
| 2 | Base images with a reproducible build; the gRPC service with TLS and token auth; the Paper agent; the two-stage ready gate; player count reporting |
| 3 | ProxyGroup, the Velocity agent, registration, the proxy ready gate, modern forwarding, fallback routing — **a player can join** |
| 4 | Slot-based scaling, player-aware drain, PDB protection, rolling updates of ephemeral groups (attrition, `maxUnavailable`) |
| 5 | Persistent groups with a PVC, ordered shutdown and recreate updates; detecting secret rotation along with a runbook |
| 6 | All three expose strategies, NetworkPolicies, the Helm chart, RKE2 E2E in CI |

Milestone 3 is the point from which the system can be demonstrated.

## 12. Open points for later projects

Deliberately left open, with consequences for later specs:

- The format of the layered templates (project 3) must be backwards compatible
  with the `mounts` from V1.
- The automated image build pipeline — following new Paper and Velocity releases
  within days, CVE rebuilds of the base layer — belongs to project 3. Documented
  as a checked alternative: `itzg` images pinned by digest as the base layer with
  an agent layer of our own on top.
- The metrics path for the dashboard (project 4) will likely use Prometheus
  metrics from the operator rather than an aggregation of its own.
- Automatically orchestrated forwarding secret rotation requires
  generation-aware registration (see 6.5).
- A `/play <group>` command with a group policy in the Velocity agent
  (`ServerPreConnectEvent`).
