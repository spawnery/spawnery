# Design: milestone 6b — NetworkPolicies, and closing the agent channel's availability gap

**Date:** 2026-08-17
**Status:** Approved
**Scope:** Two NetworkPolicies — one shipped for the operator's own namespace,
one written per `Network` by the operator into the namespace that `Network`
owns — plus the bounds, cache and rate limit that close the agent channel's
availability half. No expose strategies, no Helm chart, no CI.

## 1. What this milestone is

Milestone 6 is four subsystems; `docs/handover-milestone-6.md` records the cut
into 6a–6e and 6a is done. 6b is the piece with a security consequence.

### 1.1 The invariant that has been overdue since 3b

`docs/known-issues.md` has carried the same entry since milestone 3b, and marks
it as the one most likely to be read as a formality: **a Paper server
authenticates nobody.** `online-mode` is `false` on the backend and `true` on
the proxy, so a backend trusts whatever opens a connection and completes the
modern-forwarding handshake with the right secret — and nothing today restricts
*who* may attempt that handshake. Every pod in the cluster can reach port 25565
of every game server, and every pod in the cluster can reach the operator's
agent endpoint on 9443.

The NetworkPolicy is what makes `online-mode=false` safe. Until it lands, the
forwarding secret is the only thing between any workload in the cluster and a
seat on someone's server.

### 1.2 What 6b does not touch

No `LoadBalancer` or `HostPort` — those are 6c and are still refused at
`internal/controller/proxygroup_controller.go:214`. No Helm chart; the
operator-side policy goes into `config/deploy/`, which 6d will template along
with everything else already there. No CI. No new CRD field, no new condition,
and no new `Status` — §2.4 says why the chosen shape needs none.

## 2. Who writes the policy

### 2.1 The decision, and why NetworkPolicy write is not RBAC write

The operator writes the per-namespace policy. The handover's §3 left this open
and named the question that decides it: is write access to NetworkPolicies the
same class of authority as write access to RBAC, which milestone 5c
deliberately refused the operator?

It is not, and the difference is categorical rather than one of degree. **RBAC
write is self-amplifying:** an operator that may create Roles can grant itself
every other permission, which is exactly why 5c's argument runs "an operator
which may write RBAC makes every other restriction on it advisory"
(`config/rbac/forwarding-secret-reader.yaml`'s own header). **NetworkPolicy
write is not:** it cannot yield the operator a single additional API
permission. It can deny traffic or admit it in namespaces the operator already
acts in — real, and bounded to a different dimension.

The comparison that settles it: the operator already holds `pods: create`
cluster-wide (`internal/rbacaudit/required.go:52`), and the same
`forwarding-secret-reader.yaml` header argues from that fact — a pod the
operator creates can mount any Secret in its namespace, which is why a
name-scoped `secrets: get` was judged "defence in depth rather than a
boundary". Whoever may already do that gains little from `networkpolicies:
create`.

The two rejected shapes and their costs are recorded in
`docs/handover-milestone-6.md` §3 and are not restated here. One point from
that record does carry into this design as a positive argument: the shape where
an administrator applies a manifest per namespace would also have required
inventing a report — a condition on the `Network` saying this namespace is
unprotected — because unlike 5c's Role, a missing NetworkPolicy produces no
symptom at all. §2.4 is the other side of that.

### 2.2 Why the `NetworkReconciler`, and not the `Bootstrapper`

`Bootstrapper.Ensure` (`internal/controller/bootstrap.go`) is the existing
precedent for the operator writing objects into namespaces it did not create:
it writes the `spawnery-ca` ConfigMap and the two ServiceAccounts, called from
the Server and ProxyGroup controllers just before the first pod of a namespace
could exist.

The policy does not follow it, for a reason that is about ownership rather than
timing. The policy is per `Network` and carries an owner reference to that
`Network` (§2.3); the `NetworkReconciler` already holds that object, and the
policy belongs in the same namespace it does. Writing it from the group
controllers would mean fetching the `Network` again solely to own something.

**Only the accepted `Network` writes one.** `NetworkReconciler.pickNamespaceOwner`
already decides which `Network` owns a namespace when several exist; a rejected
one writes nothing, or two `Network` objects would fight over one policy on
every pass.

A second argument arrives from an unexpected direction and is worth recording,
because it says the chart could not have done this job even if the authority
question had gone the other way: **the egress half has to name the operator's
own namespace**, which is configurable (`--operator-namespace`, defaulting from
`POD_NAMESPACE`). A chart rendering policies at install time knows neither the
game namespaces, which are discovered at runtime, nor reliably which namespace
each policy should point back at. The operator knows both.

### 2.3 The one place this departs from `Bootstrapper`: the owner reference

`Bootstrapper` writes **no** owner reference, deliberately, so that a pod
restarting during an operator outage still finds a CA to trust. The policy
takes the opposite rule and carries an owner reference to its `Network`.

The asymmetry is the difference between what a stale object of each kind does.
A stale ConfigMap is inert. **A stale NetworkPolicy silently drops traffic** in
a namespace nobody associates with Spawnery any more — the worst kind of
failure this project can ship, because nothing reports it and the symptom
appears somewhere else entirely.

The owner reference is namespace-local and therefore legal: a `Network` owns its
namespace, so the policy and its owner are always in the same one.

Its consequence for RBAC is in §4: the operator needs no `delete` on
`networkpolicies`, because the garbage collector removes them.

### 2.4 No new status, and why that is a property of the choice

The administrator-applies shape would have needed a condition on the `Network`
reporting an unprotected namespace, because an administrator can forget.
Nothing can be forgotten here: the policy lands on the same reconcile that
accepts the `Network`. So 6b adds no condition, no reason constant and no
status field. This is not an omission to revisit — it is what the chosen shape
buys.

## 3. The two policies

### 3.1 The operator-side policy, shipped in `config/deploy/`

Ingress to the operator pod on 9443, from pods carrying
`spawnery.cloud/managed-by=spawnery-operator` **in any namespace**.

Two details decide its shape:

- **The agent channel is cross-namespace by construction.** `agentEndpoint`
  builds the dial name from the *operator's* namespace, so every managed pod in
  every namespace dials into `spawnery-system`. The rule therefore selects
  peers by pod label with `namespaceSelector: {}` — an empty selector meaning
  all namespaces — and never by namespace name, which the operator's own chart
  cannot know.
- **The operator pod does not carry `spawnery.cloud/managed-by`.** It is
  selected by its own two labels, `app.kubernetes.io/name=spawnery` and
  `app.kubernetes.io/component=operator`, the same pair
  `config/deploy/service.yaml` already selects on. Copying a managed-pod
  selector here would produce a policy that selects nothing, which fails open
  and looks identical to a policy that works.

This policy also implicitly denies all other ingress to the operator pod —
selecting a pod at all makes it default-deny for that direction — which
includes the kubelet's probes on 8081 and any scrape of the metrics port 8080.
**The policy must admit both explicitly.**

That makes the operator the one place 6b does touch probe traffic, which §3.3
refuses to do for proxies. The difference is what a mistake costs. A policy
that accidentally covers a proxy's probe converts **every proxy in the cluster**
to `NotReady` and no player can connect. The same mistake here stops one pod:
reconciliation halts, and every game server and proxy already running keeps
running and keeps its players, because no player's traffic ever flows through
the operator — a proxy learns a backend's address from it, but the connection
it then opens does not touch it. What stops is the control path: a new server
would not be registered with a proxy while the operator is unreachable. That is
a bounded, visible, recoverable failure against an
unbounded one — and it is why this risk is taken and tested (acceptance
criterion 6) while the other is designed away.

### 3.2 The per-`Network` policy

One object per accepted `Network`, in that `Network`'s namespace, named after
it, carrying `spawnery.cloud/managed-by` and `spawnery.cloud/network`, and an
owner reference to the `Network`.

**Corrected after the milestone's final review.** This paragraph said the
`managed-by` label was there "so the manager's own restricted cache can see
it". No such restriction was ever added: `cmd/spawnery-operator`'s
`Cache.ByObject` has no `NetworkPolicy` entry, and `Owns(&NetworkPolicy{})`
starts an unrestricted informer. Both labels are metadata for a human reading
`kubectl` output in a namespace Spawnery does not own — nothing selects on
either. The claim was deleted rather than the restriction added, for two
reasons. NetworkPolicies are low-cardinality, a handful per cluster, unlike the
ConfigMaps and claims the existing entries exist to bound, so an unrestricted
informer over them costs almost nothing. And restricting them would be a
regression: `reconcileNetworkPolicy`'s `CreateOrUpdate` reads through that
cache, so a pre-existing *unlabelled* object at `<network>-backends` would be
invisible to the `Get`, and every pass would `Create` and take `AlreadyExists`,
forever.

**Ingress**, `podSelector` = `ManagedSelector(network)` plus
`spawnery.cloud/role=server`:

- from pods matching `ManagedSelector(network)` plus
  `spawnery.cloud/role=proxy`, with **no** `namespaceSelector` — which restricts
  the peer to the policy's own namespace, and is correct because a `Network`
  owns its namespace;
- on port 25565/TCP.

**Egress**, same `podSelector`:

- 53/UDP and 53/TCP to the `kube-system` namespace, selected by
  `kubernetes.io/metadata.name` — the label Kubernetes sets on every namespace
  itself, on by default since 1.21 and GA in 1.22. No pod selector on the DNS server: CoreDNS
  labels are conventional rather than guaranteed, and narrowing to them buys
  nothing a namespace selector does not.
- 9443/TCP to the operator pod, selected by the same namespace label plus the
  operator's two `app.kubernetes.io` labels.
- Nothing else.

A backend runs `online-mode=false` — the invariant this milestone exists to
protect — so it never authenticates a player and never needs Mojang. Its only
other measured outbound call is Paper's update check to `fill.papermc.io`, and
`docs/known-issues.md`'s milestone 2b section records that it fails harmlessly
with no network reachable: the server still reaches `Done` and answers a ping,
which is what `make image-test` relies on.

### 3.3 What is deliberately not selected, and the asymmetry that makes it safe

**Proxy pods are not selected by any policy in 6b.** The handover makes the
readiness contract an acceptance criterion — a policy that accidentally covers
probe traffic converts every proxy to `NotReady` and takes the fleet down, and
whether kubelet traffic is subject to policy at all is CNI-dependent. This
design does not test that risk; it removes it, and the reason is an asymmetry
in how the two pod classes are probed:

| | readiness probe | is it ingress? |
|---|---|---|
| server pod | `exec` of `/usr/local/bin/spawnery-slp` against `127.0.0.1:25565` (`internal/podspec/server.go:342`) | no — it runs inside the container, over loopback, which no NetworkPolicy governs |
| proxy pod | `TCPSocket` from the kubelet to `ProxyReadyPort` (`internal/podspec/proxy.go:236-241`) | **yes** |

So a policy on server pods *cannot* break their readiness, and a policy on
proxy pods might. Since the invariant at stake is entirely about backends, 6b
selects backends and leaves proxies alone.

What that gives up is small and worth naming: an unmanaged pod may still open a
TCP connection to a proxy's 25565. The proxy is the public front door — it sits
behind a NodePort with `externalTrafficPolicy: Local` and accepts connections
from the internet — so a rule there would have to admit `0.0.0.0/0` on that
port anyway. It authenticates its players; the backend is the one that does not.

**No namespace-wide default-deny.** A policy that selects only Spawnery's pods
leaves every co-tenant workload in the namespace untouched. Spawnery does not
own those namespaces, and an operator that writes a default-deny into a
namespace it did not create, without being asked, is precisely the overreach
that made §2.1's decision contentious. The cost is stated rather than hidden: a
pod in the game namespace that carries no Spawnery label is unrestricted by
anything 6b writes.

## 4. RBAC

`networkpolicies` in `networking.k8s.io`: `get`, `list`, `watch`, `create`,
`update`. Cluster-wide, because game namespaces are discovered at runtime.

**No `delete`, and no `patch`.** The owner reference removes the object; the
operator never needs to. This follows 5a's `persistentvolumeclaims` grant
exactly — `internal/rbacaudit/required.go:77-81` documents that omission and the
audit enforces it in both directions — so a `delete` marker added later turns
`make test` red before it can ship. The entry in `RequiredCluster` names the
real call site in its `Why`; §7 of the handover records how easily that field
goes stale, and milestone 6a found one that had.

`internal/rbacaudit` needs no new mechanism for this. `make test` runs
`controller-gen` first, so a new marker lands in `config/rbac/role.yaml` in the
same invocation that then audits it, and `test/e2e/rbac_test.go` picks the new
permission up against a real cluster's authorizer with no new code.

## 5. The agent channel's availability gap

`docs/known-issues.md` records milestone 2a's isolation promise with an
explicit caveat: it holds for identity and confidentiality, not for
availability. Four things are named — no `MaxConcurrentStreams`, no
`ConnectionTimeout`, no keepalive policy, no rate limit in front of
`Authenticator.Authenticate`, and a live `TokenReview` per connection with no
cache. Verified: `internal/agentserver/server.go:166-169` constructs the server
with exactly `grpc.Creds` and one stream interceptor.

6b closes all of it. The NetworkPolicy removes the *anonymous* half — today
port 9443 is reachable from every pod in the cluster — but a compromised
*managed* pod carries the label that passes the policy, so the policy alone
does not close it.

### 5.1 What each gRPC bound actually bounds

Stated precisely, because the convenient reading is that these four options
close the gap and they do not:

- **`MaxConcurrentStreams` bounds streams per connection**, not in total. An
  agent needs exactly one stream, so the bound is right — but a pod that opens
  many *connections* is untouched by it.
- **`ConnectionTimeout`** bounds how long a half-finished handshake holds
  resources.
- **A keepalive enforcement policy** stops a client ping flood;
  `MaxConnectionIdle` reaps connections whose peer is gone.
- **None of them bounds how many connections one pod may open**, which is the
  documented attack.

### 5.2 The `TokenReview` cache, and where its line runs

`Authenticate` (`internal/grpcauth/identity.go:156`) does two things: a
`TokenReview` against the API server (`:161`), and then `LookupPod` through the
manager's client. The first is a real network call to the API server and is the
cost the attack multiplies. The second reads a cache the manager already
maintains and is free.

**Only the `TokenReview` is cached. The pod lookup runs every time.**

That line is the design. The revocation an operator actually performs — delete
the pod — stays immediate, because the uncached half is the half that ties an
identity to a live pod. What the cache can delay is narrower: a token that is
revoked while its pod still runs, which in Kubernetes means deleting the
ServiceAccount.

Three properties:

- **Keyed on a hash of the token, never the token.** The operator should not
  hold bearer tokens in a map.
- **Positive entries live 60 seconds, negative entries 10.** The asymmetry is
  deliberate: a cached "no" that was wrong — clock skew, a token checked before
  its ServiceAccount existed — should heal quickly, while a cached "yes" is
  what removes the load. Both are far inside the token's own life: projected
  tokens expire after 600 seconds (`internal/podspec/server.go:143`) and the
  kubelet rotates them.
- **`unavailableErr` is never cached.** `internal/grpcauth/identity.go:129-142`
  already separates "the API server could not answer" from "the token is bad",
  and the interceptor already maps the first to `codes.Unavailable` so an agent
  backs off rather than concluding its credentials are wrong. Caching that
  would extend an outage past its end.

### 5.3 The rate limit, on cache misses only

A token bucket per peer address, from `peer.FromContext`, consulted **only when
the cache misses**, in the interceptor before `Authenticate`.

This is where the three pieces compose. An attacker in a connection loop
replays the same token, hits the cache, and costs the API server nothing.
Feeling the limiter at all requires presenting *new* tokens, and those cannot be
manufactured: `TokenReview` is audience-bound (`podspec.AgentTokenAudience`) and
the token is signed by the cluster. What a compromised pod does get is one
genuinely new token per rotation — one per 600 seconds, against a bucket that
refills one per 10.

That legitimate mass reconnects are unaffected is not luck but a consequence of
the key: in a rollout every agent reconnects from a **different** pod IP. The
"reconnect with overlap" failure mode `docs/known-issues.md` records from the
Kotlin agent is the same path and is equally unaffected.

The bucket: 5 misses of burst, refilled one per 10 seconds. A legitimate agent
misses the cache at most once per token rotation and once per reconnect, so the
burst is generous by an order of magnitude. A rejected attempt returns
`codes.ResourceExhausted` — distinct from both `Unauthenticated` and
`Unavailable`, so an agent's logs say which of the three happened.

### 5.4 What is deliberately not built: a global ceiling

A global bound on concurrent in-flight `TokenReview` calls was considered and
rejected. It defends against many compromised pods at once, which is not the
documented attack — that entry names *a single pod in a connection loop* — and
it introduces a failure mode the current code does not have: after an operator
restart the whole fleet reconnects at once and would queue behind it.

Recorded here so the absence reads as a decision rather than an oversight.

### 5.5 Metrics

`internal/grpcauth/metrics.go:26` already registers `AuthFailures` on the
controller-runtime registry; the new counters follow it exactly — cache hits,
cache misses, and rate-limited attempts.

This is not decoration. Milestone 6a established that the end-to-end run's
denial check sees only what something logs, and `docs/known-issues.md` carries
the general form: a mechanism that reports nothing is indistinguishable from an
absent one. A rate limiter with no counter cannot be shown to be working.

## 6. Error handling and edge cases

- **A hand-deleted policy comes back.** `NetworkReconciler` gains
  `Owns(&networkingv1.NetworkPolicy{})`, so a deletion triggers a reconcile
  rather than waiting for the next periodic pass.
- **The operator cannot write the policy.** A `Forbidden` on create is a real
  failure of a security control and must not pass silently. It is logged and
  the reconcile requeues; the `Network`'s existing `Accepted` condition is not
  overloaded to carry it, because §2.4's whole argument is that this shape needs
  no new report — and a condition that appears only on an RBAC misconfiguration
  is a report about the installation, not about the `Network`.
- **Egress to a Service ClusterIP is CNI-dependent, and this is the design's
  one portability trap.** The agent dials `spawnery-operator.<ns>.svc`, which
  resolves to a Service ClusterIP, and kube-proxy DNATs it to a pod IP. Whether
  an egress rule with a `podSelector` matches depends on whether the CNI
  evaluates policy before or after that translation. The common behaviour is to
  evaluate after, so the pod selector matches; a CNI that evaluates before would
  need an `ipBlock` covering the Service CIDR instead. This design does not
  assert which side any particular CNI falls on — that is the kind of claim
  this project keeps catching itself making from memory. The pod-selector form
  is what ships,
  because it is correct on the CNIs this project targets and because a Service
  CIDR is not discoverable from inside the operator. §8 makes verifying it a
  named obligation of the RKE2 rollout rather than an assumption.
- **The metrics and health ports of the operator.** §3.1 notes that selecting
  the operator pod makes it default-deny for ingress, which covers the kubelet's
  probes to 8081 and any scrape of 8080. Both are admitted explicitly. Whether
  kubelet traffic is subject to policy at all is CNI-dependent, which is exactly
  why this is the one place 6b cannot avoid the question — and why acceptance
  criterion 6 tests it instead of arguing it.
- **A second `Network` in a namespace** changes nothing: the loser writes no
  policy, and the winner's policy selects only pods carrying its own
  `spawnery.cloud/network` value. A pod of the losing network is unselected and
  therefore unrestricted, which is the same thing that happens to it everywhere
  else in the system.
- **An unlabelled pod in a game namespace** is unrestricted, by §3.3.

## 7. Tests

**Unit, no cluster.** The policy builder is a pure function from a `Network` and
the operator's namespace to a `networkingv1.NetworkPolicy`, and it belongs in
`internal/podspec` beside `BuildServerPod` and `BuildDataClaim` — it is the same
kind of thing, a rendering of an object from a spec, and it needs the label
helpers that live there. It is tested the way those are: assert the selectors,
the ports, the owner reference, and the absence of a
`namespaceSelector` on the ingress peer. The cache and the rate limiter take an
injected clock, the same shape the controllers already use, so their expiry and
refill are table tests rather than sleeps.

**envtest.** The `Network` controller creates the policy; a rejected `Network`
does not; a deleted policy is recreated. The owner reference's *effect* cannot
be tested here — envtest runs no garbage collector — and the test says so in its
own doc comment rather than implying otherwise. Milestone 6a's own record is the
reason that sentence is required: the claim that envtest cannot see an owner
reference at all was false, because a direct field read sees it anywhere; only
the collection is real-cluster-only.

**Mutation is the acceptance criterion, not a green run.** At minimum, each
performed and its output recorded: remove the `role=server` term from the
`podSelector` and the ingress test must fail naming it; drop the owner
reference and the envtest must fail; make the cache return a hit for an expired
entry and its table must fail; and remove `create` from the new RBAC marker,
which must turn both `internal/rbacaudit` and the end-to-end run red, in two
different ways.

## 8. What the E2E can and cannot prove

`test/e2e` grows two scenarios and no new machinery: a scenario is a
`func theXxx(t *testing.T)` plus one line in
`TestSpawneryUnderItsOwnServiceAccount`'s explicit `t.Run` list, above the
denial check, which stays last.

**Provable there:** that the operator created the policy it was supposed to,
with the selectors it was supposed to have — the same shape as the existing
`theProxyGroupGetsItsService` — and that the new permission is granted and never
denied, which the two existing RBAC scenarios pick up for free.

**Not provable there, and the test must say so in its own comment:** that the
policy *blocks* anything. Two reasons compound. No image in that harness
resolves, by decision, so no process listens on 25565 and there is nothing to
connect from or to. And enforcement is a property of the CNI rather than of the
object: `hack/e2e.sh` runs a bare `kind create cluster` with the default
kindnet, and if that CNI drops nothing then "the connection was blocked" and
"the policy was never applied" produce the same green.

So 6b asserts objects, states that it asserts objects, and hands the enforcement
claim to the RKE2 rollout at the end of milestone 6 — which is the run that has
a real CNI, real images and a real client.

## 9. Acceptance criteria

1. `make test` is green, and the new unit and envtest coverage exists.
2. A `Network` reconcile creates a `NetworkPolicy` in its namespace, owned by
   the `Network`, carrying `spawnery.cloud/managed-by`.
3. A `Network` rejected by `pickNamespaceOwner` creates none.
4. Deleting the policy by hand brings it back without waiting for a periodic
   pass.
5. `config/deploy/` gains the operator-side policy, and the existing manifest
   tests accept it.
6. **The operator stays `Ready` with its own policy applied** — the one place
   6b touches probe traffic, tested in the end-to-end run rather than reasoned
   about.
7. Removing `create` from the new marker turns `make test` red from
   `internal/rbacaudit` and `make e2e` red from the authorizer scenario.
8. `make e2e` passes with two new scenarios, and the enforcement limitation is
   stated in the test's own comment.
9. Every mutation in §7 performed, with its real output recorded.

## 10. What 6b leaves open

- **Enforcement is unproven until the RKE2 rollout.** Both the traffic rules and
  the DNAT question in §6 are objects only, everywhere 6b can reach.
- **Proxy pods are unselected**, so nothing restricts who may open a TCP
  connection to a proxy's 25565 from inside the cluster.
- **Unlabelled pods in game namespaces are unrestricted**, by §3.3.
- **Proxy egress is unrestricted**, because vanilla NetworkPolicy cannot express
  a destination by DNS name and Mojang's addresses are not stable. A cluster
  wanting that needs a CNI with FQDN policies, which is not a portable
  assumption.
- **The `Why` field of the new RBAC entry will go stale** the first time a
  second call site appears, exactly as `configmaps`' and `pods: patch`'s did.
  Nothing in the audit can catch it; `docs/known-issues.md` carries both
  precedents.

## 11. Facts this design asserts about the code already here

Each measured this session, not remembered:

1. `internal/agentserver/server.go:166-169` constructs the gRPC server with
   exactly `grpc.Creds(creds)` and one stream interceptor — no limits of any
   kind.
2. `internal/grpcauth/identity.go:156` is `Authenticate`; `:161` issues the
   `TokenReview`; `:129-142` defines `unavailableErr` and its unwrapping.
3. `internal/grpcauth/interceptor.go:60` is `StreamInterceptor`, and it already
   maps `isUnavailable` to `codes.Unavailable` and everything else to
   `codes.Unauthenticated`.
4. `internal/grpcauth/metrics.go:26` registers `AuthFailures` on
   controller-runtime's registry, in an `init()`.
5. `internal/podspec/server.go:143` sets `TokenExpirationSeconds = 600`.
6. `internal/podspec/agent.go:29` sets `AgentTokenAudience = "spawnery-operator"`.
7. `internal/podspec/server.go:342` is the server's readiness probe: an `exec`
   of `SLPHealthBinary` against `127.0.0.1` and `MinecraftPort`.
8. `internal/podspec/proxy.go:236-241` is the proxy's: a `TCPSocket` to
   `ProxyReadyPort`, which `:42` sets to 8081.
9. `internal/podspec/labels.go` defines `ServerLabels`, `ProxyLabels` and
   `ManagedSelector(network)`; `LabelRole` carries `server` or `proxy`, and
   `LabelNetwork`'s doc comment already says NetworkPolicies select on it.
10. `internal/rbacaudit/required.go:52` grants `pods: create` cluster-wide;
    `:77-81` is the `persistentvolumeclaims` grant with `delete` and `update`
    deliberately absent.
11. `config/deploy/service.yaml` selects the operator pod on
    `app.kubernetes.io/name=spawnery` and
    `app.kubernetes.io/component=operator`; the operator pod carries no
    `spawnery.cloud/managed-by`.
12. `config/rbac/forwarding-secret-reader.yaml`'s header carries 5c's argument
    verbatim, including that the operator holding `pods: create` cluster-wide
    makes a name-scoped `secrets: get` "defence in depth rather than a
    boundary".
