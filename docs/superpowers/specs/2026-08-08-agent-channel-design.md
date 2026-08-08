# Design: The agent channel — a gRPC service with TLS and token auth

**Date:** 2026-08-08
**Status:** Draft for approval
**Scope:** Milestone 2a — the operator's half of the agent channel, entirely in
Go. The gRPC contract, the transport, the identity of the agents, and the wiring
all the way into the existing `agent.Registry`. The Paper agent and the base
images are milestone 2b and get a document of their own.

## 1. Purpose

Milestone 1 finished building the state machine but wired up only half of its
inputs. The two-stage ready gate asks in `server_controller.go:333-340` for
`PodReady` **and** `AgentReady`; the second condition is always false today,
because nobody fills the registry. The same holds for `status.players` and
`status.slots`: the throttling into them is in place
(`server_controller.go:556-564`), the source is missing.

This document describes that source: a gRPC service inside the operator process
that the agents in the game server pods open a session to, and the proof that
the identity at the other end is the one it claims to be.

**Success criterion:** A test agent that opens a `ServerSession` over a real TLS
connection with a real token issued through `TokenRequest` and reports `Ready`
brings a `Server` whose pod readiness is green into phase `Ready` — and its
player counts show up in the status. The same connection without a matching
audience, without pod binding, or with the proxy ServiceAccount is rejected.

**Not in 2a:** `ProxySession` (milestone 3), CA rotation with an overlap phase,
the Kotlin agent, the base images.

## 2. Why the cut is here

Milestone 2 spans three build worlds according to the design: Go, Gradle/JVM and
container builds. The Go part is the only one that can be proven in full here —
envtest needs neither a kubelet nor a container runtime — and it defines the
contract 2b builds against. Hence first.

## 3. Precondition: envtest on both development machines

The flake ties `KUBEBUILDER_ASSETS` to the nixpkgs package `kubernetes`, which
is not built for `aarch64-darwin`. On the NixOS desktop the suite runs; on the
Mac even evaluating `nix develop` fails. That makes proving this milestone
impossible on one of the two machines.

The controller-tools project publishes envtest binaries for all four relevant
platforms along with SHA-512 sums in a versioned list, among them
`envtest-v1.36.2-darwin-arm64.tar.gz`. The flake fetches them from there in
future, per platform and with checked-in hashes — the same pattern spec 5 of the
main design prescribes for the base images, and a version that matches the
`k8s.io/*` dependencies in `go.mod` (0.36).

That is step one of the plan, and it is a gate: **until envtest runs here, the
rest of this document is unprovable.** The binaries have been tried out in
advance — `kube-apiserver v1.36.2` and `etcd 3.6.8` start natively on
`aarch64-darwin`, and the archive unpacks to
`controller-tools/envtest/{etcd,kube-apiserver,kubectl}`.

### 3.1 The assumption under 6.3 has been measured

The design stands or falls on envtest issuing pod-bound ServiceAccount tokens
and the authenticator deriving the claims `authentication.kubernetes.io/pod-name`
and `pod-uid` from them. That was checked before the plan with a throwaway
probe, using the envtest binaries from section 3 on `aarch64-darwin`:

- `TokenRequest` with `audiences: [spawnery-operator]`, `expirationSeconds: 600`
  and a `boundObjectRef` onto the pod issues a token;
- `TokenReview` with the same audience reports `authenticated: true`, username
  `system:serviceaccount:<ns>:spawnery-server`, `audiences:
  [spawnery-operator]`;
- the extra claims contain `authentication.kubernetes.io/pod-name` and `pod-uid`
  with the name and UID of exactly that pod;
- with a foreign audience the same token reports `authenticated: false`.

No feature gate needed, roughly two seconds of runtime. Section 6.3 therefore
rests on a measurement, not on documentation — the same care with which the E2E
design measured its `SubjectAccessReview` promise up front.

## 4. Building blocks

| Package | Task |
|---|---|
| `proto/spawnery/agent/v1alpha1/agent.proto` | The contract from spec 5.2 of the main design, in full — both services, all messages. At the same time the artifact 2b builds the Kotlin agent against. |
| `internal/agentpb` | Generated Go, checked in like `zz_generated.deepcopy.go`. Produced by `make proto`. |
| `internal/certs` | Issue the CA and the serving certificate, hold them in a secret, renew them. Pure crypto logic plus one secret access; clock injectable. |
| `internal/grpcauth` | Check the `TokenReview` and turn it into an `Identity`. Knows neither gRPC handlers nor the registry. |
| `internal/agentserver` | The `AgentService`. Wires identity and messages into the `agent.Registry`. |

**Changed:** `internal/podspec` (ServiceAccount, projected token, CA mount,
endpoint), a namespace bootstrap in `internal/controller`,
`cmd/spawnery-operator` (wiring and flags), `config/deploy/` extended by a
service and the gRPC port in the deployment, `config/rbac/` along with the
permission table in `internal/rbacaudit`.

**Unchanged, although it belongs to the milestone:** The two-stage ready gate and
the player count in the status already exist. 2a gives them data, not new logic.

## 5. The contract

The `.proto` covers spec 5.2 in full, including the proxy direction. The
contract is frozen in the main design anyway, and changing a field number later
costs more than a few unused messages today. Only `ServerSession` is implemented
and tested in 2a; `ProxySession` answers `Unimplemented`.

Both directions are streams of `oneof` envelopes. An unknown branch is ignored,
so a newer agent does not crash an older operator.

### 5.1 One addition over spec 5.2

New is `SessionDeadline{renewAfterSeconds, hardDeadlineSeconds}`, operator →
agent, sent on connect. The reasoning is in 7.1. The main design does not know
the message; this addition is deliberate and commits 2b to overlapping
reconnects.

## 6. Security

### 6.1 Transport

The endpoint speaks TLS and nothing else, on port 9443, reachable through a
ClusterIP service `spawnery-operator` in the operator namespace. There are no
client certificates — the agents identify themselves with the token.

### 6.2 Certificates

The secret `spawnery-agent-tls` in the operator namespace holds `ca.crt`,
`ca.key`, `tls.crt` and `tls.key`. Both keys are ECDSA P-256. The CA is valid
for ten years, the serving certificate for 90 days, with the SANs
`spawnery-operator`, `spawnery-operator.<ns>`, `spawnery-operator.<ns>.svc` and
`spawnery-operator.<ns>.svc.cluster.local`.

On startup the operator reads the secret; if it is absent, incomplete or
expired, it issues a new one. An hourly runnable renews the serving certificate
from the same CA once less than a third of its lifetime remains. The gRPC server
picks up its certificate through `tls.Config.GetCertificate` from an
`atomic.Pointer`; renewal costs neither a restart nor a dropped connection.

Issuing happens only on the leader — see 7.2 — so there is no race for the
secret.

**The CA ConfigMap holds a bundle**, meaning concatenated PEMs, even though 2a
only ever writes exactly one into it. A later CA rotation needs the overlap
phase of old plus new; keeping the format open now costs nothing, changing it
later breaks every connected agent. The rotation path itself does not belong to
this milestone.

### 6.3 Identity

The interceptor takes the bearer token from the `authorization` header and
accepts the stream only if all of the following hold:

1. `TokenReview` with `spec.audiences: ["spawnery-operator"]` reports
   `authenticated: true`, and the audience appears in the response.
2. The username parses as `system:serviceaccount:<ns>:<name>`; the name is
   `spawnery-server` for `ServerSession` and `spawnery-proxy` for
   `ProxySession`. Without that separation a compromised game server could open
   a proxy session, read the topology through `FullSync` and steer scaling with
   forged reports (spec 5.2, point 2).
3. The extra claims `authentication.kubernetes.io/pod-name` and `pod-uid` are
   set. If they are missing, the token is not pod-bound and is rejected.
4. The named pod exists in the manager's cache, lives in this namespace, carries
   `spawnery.cloud/role=server` and has exactly this UID.

The identity of the stream is therefore `{Namespace, PodName, PodUID, Role}` and
comes from the token alone. **No identity comes out of `Hello`** — only version
and the ready flag. If the pod name came from the message, a compromised server
could report `PlayerCount{0}` for a foreign one and have it deleted.

Point 4 is defense in depth. Without it a self-built pod using the same
ServiceAccount could never speak for a foreign server — its identity is its own
— but it could fill the registry with entries that have no CR behind them.

**The registry key is the pod UID** — that is how `Lookup` and `Forget` already
key them today (`server_controller.go:392-396`, `orphan.go:93`). It comes
straight from the claim `authentication.kubernetes.io/pod-uid`, so from the same
source as the identity. The pod name is needed only for the log and for the
existence check in point 4.

### 6.4 What goes into the pods

The game server pods keep `automountServiceAccountToken: false` and get the
ServiceAccount `spawnery-server`. Mounted is a projected volume under
`/var/run/spawnery` with two sources:

- `token` — a `serviceAccountToken` with audience `spawnery-operator` and
  `expirationSeconds: 600`. The kubelet rotates it.
- `ca.crt` — the namespace's ConfigMap `spawnery-ca`.

Plus the environment variable `SPAWNERY_OPERATOR_ENDPOINT` holding
`spawnery-operator.<operator-ns>.svc:9443`. The operator namespace comes from a
flag defaulting to `POD_NAMESPACE`.

### 6.5 Namespace bootstrap

In every namespace where it creates pods, the operator reconciles two objects:
the ConfigMap `spawnery-ca` and the ServiceAccount `spawnery-server`. The server
controller calls that reconcile before it creates a pod — and only then. There
is no path that carries a changed CA into every already known namespace on its
own: a namespace where no pod is currently being created keeps its old `ca.crt`
until the next pod is created there. Together with the missing CA rotation this
is recorded in `docs/known-issues.md`.

The CA is public, hence a ConfigMap: a secret would demand cluster-wide secret
write permissions from the operator without the contents being secret. A
ServiceAccount with no binding grants nothing by itself.

**The cache stays narrow.** Both object types carry
`spawnery.cloud/managed-by: spawnery-operator` just like the pods, and the
manager cache is restricted to that label for ConfigMaps and ServiceAccounts.
Without the restriction the operator would hold every ConfigMap in the cluster
in memory, `kube-root-ca.crt` from every namespace included. If an object loses
its label, the reconcile no longer sees it and creates it anew; the API server
answers that with `AlreadyExists`, whereupon the reconcile reads uncached and
puts the label back.

### 6.6 Permissions

New in the ClusterRole:

- `authentication.k8s.io/tokenreviews: create`
- `configmaps: get, list, watch, create, update` — the CA ConfigMap per
  namespace
- `serviceaccounts: get, list, watch, create` — the ServiceAccount per namespace

New in a **`Role` in the operator namespace**, the first one alongside the
ClusterRole:

- `secrets: get, create, update` — the TLS secret

Cluster-wide secret write permissions would be the wrong signal in precisely the
milestone that introduces security. Since the split appears anyway, the leases
move with it: today they sit cluster-wide even though leader election only locks
in the operator's own namespace. `docs/known-issues.md` lists that as an open
point for milestone 6 — it is settled with 2a.

`internal/rbacaudit` splits its table accordingly into a cluster-wide and a
namespace-local half; both halves are still checked in both directions, file
based and through `SubjectAccessReview`.

## 7. The life of a session

### 7.1 Make before break

A verified identity is not valid indefinitely: after `hardDeadlineSeconds` the
operator closes the stream and the agent reconnects with a fresh token. An
intercepted token is therefore useful at most until it expires.

If the operator simply closed the stream, every server would get a 15-second
window every ten minutes (`phase.go:341`) in which it drops out of `Ready`,
deregisters from the proxies and collects a readiness loss. That would be a
home-made flap counter.

So the operator announces the deadline instead. The agent opens a new stream
after `renewAfterSeconds`, **before** the old one ends. Because a second stream
from the same pod supersedes the first without calling `Disconnect` (see 7.3),
`Connected` never goes false in the process. The hard close remains the safety
net for an agent that does not play along.

Values: `renewAfterSeconds: 480`, `hardDeadlineSeconds: 600`, matching the token
lifetime.

### 7.2 Only on the leader

The gRPC service runs as a runnable bound to leader election, because the
registry is useful exactly where the controllers read it. An agent that landed
on a standby would fill a registry nobody reads — the server would never reach
`Ready`.

So that the service does not list a standby as an endpoint in the first place,
its `/readyz` only turns green after it has acquired the leader lock. With one
replica in V1 that has no effect; without the coupling, multiple replicas would
quietly break later.

### 7.3 One stream per pod

If the same pod opens a second stream, the new one supersedes the old, and the
old is closed **without** calling `registry.Disconnect`. Otherwise tearing down
the superseded stream would delete the state of the fresh one, and the server
would drop out of `Ready` for no reason.

### 7.4 Messages

On connect the operator sends `ReportInterval{5s}` and `SessionDeadline`.
After that:

| From the agent | Effect |
|---|---|
| `Hello{version, ready}` | `registry.Connect`, plus `MarkReady` when `ready:true`. Ready is a state, not an event — otherwise a server would hang in `Starting` forever after an operator restart. |
| `Ready` | `registry.MarkReady` as an immediate notification. |
| `PlayerCount{n, slots}` | `registry.ReportPlayers`. |
| Stream ends | `registry.Disconnect`, except when superseded per 7.3. |

## 8. Error handling

| Case | Behaviour |
|---|---|
| `TokenReview` unreachable | Rejected with `Unavailable`; the agent retries with backoff. No positive cache: with 200 servers and one check per stream per ten minutes that is roughly 0.3 checks per second. |
| Token rejected | Rejected with `Unauthenticated`, counter up, log with namespace and ServiceAccount — never with the token. |
| `PlayerCount` above `slots` | Dropped, logged, counter up, stream stays open. That is what spec 5.2 says; tearing down would be a reconnect loop on the agent's say-so. |
| Unknown `oneof` branch | Ignored. |
| Operator restart | The agents reconnect and send `Hello{ready:true}`. For unknown pods the registry measures `StreamDownFor` from process start, so the grace period in `phase.go` applies. Already built this way. |
| Pod disappears | The pod-bound token becomes invalid and the stream dies. `Forget` stays the orphan sweep's job. |
| CA ConfigMap deleted or tampered with | The bootstrap reconcile restores it. Running streams are unaffected; a starting pod waits until the file is correct again. |

Three Prometheus metrics, because rejected streams would otherwise only appear
in the log: open streams as a gauge, rejected authentications and dropped player
counts as counters.

## 9. Tests

Everything here runs in `make test`, without a kubelet and without a container
runtime.

**Probe (plan step 1).** envtest issues a pod-bound token with an audience, and
`TokenReview` returns `pod-name` and `pod-uid`. See 3.1 — this test carries the
assumption 6.3 stands on.

**`internal/certs`, unit.** Issuance sets the expected SANs; renewal fires below
the threshold and not above it; a tampered secret leads to reissuance rather
than a crash. Clock injected.

**`internal/certs`, envtest.** The secret survives a restart: the second start
keeps using the same CA.

**`internal/grpcauth`, envtest.** Against the real authenticator, with tokens
from `TokenRequest`:

- accepted: a pod-bound token of the `spawnery-server` ServiceAccount on
  `ServerSession`, and the identity names the pod name and UID of the right pod;
- rejected: missing audience, wrong audience, a token without pod binding, a
  token of the `spawnery-proxy` ServiceAccount on `ServerSession`, garbage
  instead of a token, an empty token — and, each as its own test, a token for a
  pod the operator does not manage: without the `managed-by` label, with the
  label of the other role, or created by hand.

An expired token is deliberately absent from that list: the API server enforces
a minimum lifetime for tokens issued through `TokenRequest`, so such a case
could only be constructed by waiting or by hand-building a token. Expiry is
checked by the API server anyway, not by `internal/grpcauth` — the test would
prove somebody else's guarantee.

The rejections are the actual proof — a test that only checks acceptance would
stay green even if the interceptor let everyone through.

**`internal/agentserver`, envtest.** A Go test agent over a real TLS connection
against a real listener: `Hello`, `Ready` and `PlayerCount` land in the registry;
a drop sets `Connected` false; a second stream supersedes the first without
deleting the state; the hard close fires; overlapping reconnects keep
`Connected` true throughout.

**`internal/controller`, envtest.** With a running manager and gRPC service: a
`Server` whose pod readiness is set and whose agent reports `Ready` reaches phase
`Ready` — and its player counts appear in `status.players`. The tests still set
pod readiness themselves; there is no kubelet.

**`internal/podspec`, unit.** ServiceAccount, projected volume with audience and
expiry, CA path, endpoint variable.

**Namespace bootstrap, envtest.** ConfigMap and ServiceAccount appear; a changed
CA is carried forward on the next reconcile of that namespace; a foreign change
to the ConfigMap is corrected.

**`internal/rbacaudit`, envtest.** The table covers the new permissions and is
split into a cluster-wide and a namespace-local half. Plus a probe that asks for
a permission known **not** to be granted and insists on a denial — with that,
the first open point in `docs/known-issues.md`, section "On the RBAC audit", is
settled.

## 10. What 2a leaves open

- **`ProxySession`** answers `Unimplemented`. Milestone 3 fills it, and does so
  together with the widened filter in the orphan sweep — otherwise
  `OrphanReconciler.Sweep` throws every proxy agent out of the registry within
  one interval (`docs/known-issues.md`).
- **CA rotation** with an overlap phase. The bundle format is in place, the path
  is not.
- **The agent itself.** Until 2b only the test agent talks to the service; that
  proves the contract, but not one line of Kotlin.
- **The `spawnery-proxy` ServiceAccount** is not created by 2a. The bootstrap
  learns about it once there are proxy pods.
