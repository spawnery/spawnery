# Handover to milestone 6c

Status: **end of milestone 6b (2026-08-17). Two `NetworkPolicy` objects ship —
one written per accepted `Network` into that network's own namespace, one in
`config/deploy/` for the operator's agent endpoint — and the agent channel's
availability half is closed with gRPC bounds, a `TokenReview` cache and a
per-peer rate limit. Not one connection has been observed being refused.**

That last sentence is the milestone, not a caveat on it. The CNI in this
repository's end-to-end harness was measured in 6b to enforce nothing — a
policy rule that denied the kubelet's probe changed no observable behaviour —
so every test 6b ships asserts an object, and the question of whether any of
those objects protect anything is a property of a cluster's CNI that no run
here has tested on any CNI. §2 is that measurement, what it rules out, and how
far it does and does not generalise. Read it before writing a sentence about
what 6b protects.

**Superseded as the document to start from: milestone 6c has landed, and
anyone starting 6d begins at
[`handover-milestone-6c.md`](handover-milestone-6c.md).** This one is kept
unedited as the record of what 6c started from — §3's survey of the tree is
the evidence base for the decisions 6c then made, and its tense is the tense
of 2026-08-17, before 6c existed. §3 makes seven distinct claims; all seven
were checked against the code as it now stands. Two are now false and must
not be read as open. "The refusal 6c replaces is still where 6a left it" is
gone in three ways at once: `ProxyGroupReconciler` now accepts all three
strategies `expose.type` has always named, and `ReasonExposeNotImplemented`'s
guard is a fail-safe for a fourth, unrecognised value — what closes that
value off is `ExposeType`'s own
`+kubebuilder:validation:Enum=LoadBalancer;NodePort;HostPort` marker
(`api/v1alpha1/proxygroup_types.go:27`), rendered into the generated CRD as a
plain OpenAPI `enum:` field
(`config/crd/bases/spawnery.cloud_proxygroups.yaml:174-177`) and enforced by
structural-schema validation; the
unconditional `group.Spec.Expose.NodePort` dereference that same paragraph
locates at the two call sites is gone with it — `proxyAddress` no longer
takes a plain `int32`, it takes `(group, pods, svc)` and branches on the
strategy itself (now at
`internal/controller/proxygroup_controller.go:1611`), and `reconcileService`
— the Service builder that paragraph points at — moved to `:1248` with a
rewritten signature, `(ctx, group) (*corev1.Service, error)`, in place of the
old inline dereference; and a container `hostPort` is now set outside the
generated deepcopy, in `internal/podspec/proxy.go`'s `renderProxyPod`
(`:227-229`), for the `HostPort` strategy alone. "The E2E seam is unchanged
and now carries fourteen scenarios" is unchanged in its mechanics but not its
count — fourteen is now eighteen. The other five claims hold exactly as
written: the per-`Network` policy still selects no proxy pod; the operator's
namespace is still reached two ways with only one parameterised
(`config/deploy/networkpolicy.yaml` still hard-codes `spawnery-system`);
`internal/rbacaudit` still goes red on any marker/table disagreement, and
6c's one new entry (`services: delete`) left the five `networkpolicies`
entries this paragraph describes untouched; `reconcileNetworkPolicy`'s
ordering behaviour is exactly as described, because `internal/controller/network_controller.go`
is a file no 6c commit touched; and the `grpcauth` fixture claim holds for
the same reason — no 6c commit touched anything under `internal/grpcauth/`.
`docs/handover-milestone-6c.md`'s own §1 and §3 carry the current figures and
line numbers for the two claims that changed; this document's §3 is left as
written, for the same reason `handover-milestone-6.md`'s was: it is the
record of what 6c was decided against, not a claim about what is true today.

This document is not a spec. It says where 6b stopped and what 6c — the
`LoadBalancer` and `HostPort` expose strategies — finds when it starts, checked
against the code as 6b leaves it rather than against the plan that preceded it.
The design decisions live in
[`docs/superpowers/specs/2026-08-17-network-policies-design.md`](superpowers/specs/2026-08-17-network-policies-design.md);
the open points are in [`docs/known-issues.md`](known-issues.md), whose "From
milestone 6b" section this document does not repeat in full.

**Why this is a new document rather than a section appended to
[`handover-milestone-6.md`](handover-milestone-6.md).** That document was
written *for* 6b, and its §2 ("What 6b finds in place") and §3 ("The one
structural thing 6b has to decide") are the record of what 6b started from and
what it had to settle — including the CNI warning at the end of its §2, which
6b then went and measured. Rewriting those two sections into the past tense
would delete the evidence base for 6b's own decisions, and leaving them in the
present tense inside a document a reader of 6c opens would tell that reader a
question is open which is now answered. Milestone 5 started fresh from 4's for
the same class of reason, and 4b got its own once it stopped being the thing to
read 4c against; 6a's handover stopped being that the moment 6b landed. It
keeps its own value as history and now carries a header saying so. Everything
in it that is still true and still needed — its §5 on what 6c and 6d inherit,
its §6 on what the RKE2 rollout owes, its §7 on the environment — is carried
forward here, re-checked against the tree rather than copied on faith.

## 1. Where 6b stopped

**Built and driven:**

- `podspec.BuildNetworkPolicy` (`internal/podspec/netpol.go`) renders the
  per-`Network` policy: it selects that network's server pods
  (`managed-by` + `network` + `role=server`), admits ingress on 25565 from that
  network's own **proxies in the same namespace** — the peer carries no
  `namespaceSelector`, which is what confines it — and allows egress to cluster
  DNS (53/UDP and 53/TCP into `kube-system`) and to the operator's pod on 9443.
  Both `PolicyTypes` are declared explicitly, because a policy carrying egress
  rules without `PolicyTypeEgress` applies none of them and the API server
  accepts it without complaint.
- The policy carries an owner reference to its `Network`, which is the one
  place it departs from `Bootstrapper.Ensure`'s rule of writing nothing owned.
  The argument is in the builder's own doc comment: a stale ConfigMap is inert,
  a stale `NetworkPolicy` would silently drop traffic in a namespace nobody
  associates with Spawnery any more. `APIVersion` is built from
  `spawneryv1alpha1.GroupVersion.String()` rather than a literal, so it cannot
  drift from the scheme and leave the policy orphaned.
- `NetworkReconciler.reconcileNetworkPolicy`
  (`internal/controller/network_controller.go`) writes it through
  `controllerutil.CreateOrUpdate` on every reconcile of an accepted `Network`,
  and the reconciler gained `Owns(&networkingv1.NetworkPolicy{})`, so a
  hand-deleted policy comes back on a watch event instead of waiting out
  `resyncInterval`. A `Network` that lost `pickNamespaceOwner`'s contest returns
  before the policy call and writes none.
- `config/deploy/networkpolicy.yaml` is the network-independent half: it selects
  the operator pod by its own two labels — the operator pod does **not** carry
  `spawnery.cloud/managed-by`, and copying `ManagedSelector` here is the trap —
  admits 9443 from pods labelled `spawnery.cloud/managed-by` in **any**
  namespace, and admits 8081 and 8080 from anywhere with no `from` at all,
  because the kubelet's source is a node rather than a pod and no selector
  names it.
- RBAC: `networking.k8s.io/networkpolicies` `get;list;watch;create;update`,
  cluster-wide because game namespaces are discovered at runtime. **No `delete`
  and no `patch`**, and the omission is enforced in both directions by
  `internal/rbacaudit`'s table rather than merely written down.
- The agent channel: `internal/agentserver/server.go` now constructs the gRPC
  server with `MaxConcurrentStreams` (8), `ConnectionTimeout` (30s),
  `MaxConnectionIdle` (5m) and a keepalive enforcement policy (`MinTime` 30s,
  `PermitWithoutStream: false`). `internal/grpcauth/cache.go` caches the
  `TokenReview` — 60 seconds for an accepted token, 10 for a refusal, and never
  the third answer, "the API server could not say" — keyed on a SHA-256 of the
  token, and
  deliberately does **not** cache the pod lookup. `internal/grpcauth/limiter.go`
  is a per-peer token bucket, 5 of burst refilled one per 10 seconds, consulted
  only on a cache miss, refusing with `codes.ResourceExhausted`. Three new
  counters make all of it visible:
  `spawnery_agent_token_review_cache_hits_total`, its `_misses_` twin, and
  `spawnery_agent_rate_limited_total`.
- `test/e2e` grew two scenarios and no new machinery, so the ordered list is
  fourteen: `the network gets its policy` and `the operator stays ready behind
  its own policy`, both above the denial check, which stays last.

**Verified at the end of the milestone**, on the tree as committed:
`nix develop -c make test` green; one foreground `make e2e` green with all
fourteen subtests; and `git diff master...HEAD --name-only` naming nothing
under `agent/` or `proto/`, which is the evidence that no agent-facing message
moved. `make agent-test` was nevertheless run once during the milestone — Task
4, from a pruned podman store — because the keepalive enforcement policy is the
one change that could have regressed a real agent, and it did not: both agents
connected, no `ENHANCE_YOUR_CALM`. The agents send no keepalive at all
(`agent/common`'s `SessionLoop` says so in its own comment), which is why the
enforcement policy has nothing legitimate to throttle.

**Not driven, and not drivable here:** anything about enforcement. See §2.

## 2. The one thing 6c must not misread

**kindnet was measured not to enforce a `NetworkPolicy` ingress rule, and
measured is what makes this usable.** Task 3 deleted the peerless kubelet-probe
rule from `config/deploy/networkpolicy.yaml`. What remains selects the operator
pod —
which makes it default-deny for ingress — and admits only the agent peer on
9443, so the kubelet's probe to the health port is denied by the object in
force. `make e2e` stayed green: `deployment "spawnery-operator" successfully
rolled out`, all twelve subtests of the day passing.

Both alternative explanations were closed rather than waved at:

- **The probe path was genuinely exercised.** The operator's readiness probe is
  an `httpGet` to `/readyz` on the health port
  (`config/deploy/deployment.yaml`), which travels the real network path, and
  `kubectl rollout status` cannot return success unless one passes.
- **The policy was genuinely in force.** `hack/e2e.sh` creates the cluster from
  scratch on every run, and that run's apply log reads
  `networkpolicy.networking.k8s.io/spawnery-operator-agent created`, not
  `unchanged`.

That leaves one explanation: the CNI passed traffic the policy denied. Hold the
scope of that straight, because everything downstream leans on it. **Measured:
one ingress rule, on one path.** kindnet implementing no NetworkPolicy
controller at all — no ingress rule and no egress rule, for any pod — is what
kindnet's own documentation says, and this project's rule is that a mechanism
is not evidence, which applies to a CNI's README as much as to a shell script.
The two agree; only one of them was checked here, and the egress half was
checked by neither.

**What follows for anything 6c writes or claims:**

- Every NetworkPolicy assertion available in this repository is an assertion
  about an **object**: the rendered spec (`internal/podspec/netpol_test.go`),
  the shipped manifest (`internal/rbacaudit/deploy_envtest_test.go`), the
  created object and the operator's continued readiness
  (`test/e2e/netpol_test.go`).
  On this harness a correct policy and a wholly broken one produce the same
  green. `theOperatorStaysReadyBehindItsOwnPolicy` cannot fail for the reason
  its name suggests, and its own doc comment says so; it is kept as a
  regression guard for the day the harness gains an enforcing CNI.
- The invariant open since 3b — a Paper server runs `online-mode=false`,
  authenticates nobody, and trusts whatever completes the modern-forwarding
  handshake with the right secret — now has a policy written against it and no
  demonstration that the policy does anything.
- Do not write "the policy blocks", "prevents" or "protects" into a document,
  a test name or a commit message on the strength of the object existing. The
  honest verb is that the policy *admits* a set of peers, and that whether
  anything else is refused is the CNI's to decide.

## 3. What 6c finds in place

Milestone 6c is `LoadBalancer` and `HostPort`. What follows is read off the
code as 6b leaves it.

**Nothing 6b writes selects a proxy pod.** The per-`Network` policy's
`podSelector` carries `spawnery.cloud/role=server`, and
`config/deploy/networkpolicy.yaml` selects the operator pod alone. So the
expose path 6c is about — external client to proxy — is governed by no policy
in this repository, and 6c can change how a proxy is reached without touching
one. The reason is an asymmetry worth carrying rather than rediscovering: a
server's readiness is an `exec` against `127.0.0.1` inside its own container,
which no policy governs, while a proxy's is a `TCPSocket` from the kubelet,
which one might — so selecting proxies would have put the fleet's readiness at
the mercy of a CNI's treatment of kubelet traffic. If 6c ever does want a rule
in front of a proxy, that trade is what it is re-opening, and `HostPort`
sharpens it: traffic arriving on a node's own address is exactly the case where
"which peer is this" has no pod answer.

**The refusal 6c replaces is still where 6a left it.**
`ProxyGroupReconciler` refuses any `expose.type` other than `NodePort` with
`ReasonExposeNotImplemented` while still requeueing, so a group with occupied
pods keeps its budget maintained. 6a's handover puts the unconditional
`group.Spec.Expose.NodePort` dereference inside `proxyAddress`; read against
the code it is in the two **call sites** —
`proxyAddress(pods, group.Spec.Expose.NodePort.Port)` at
`internal/controller/proxygroup_controller.go:1351`, and the Service builder at
`:1228` — while `proxyAddress` itself takes a plain `int32` and needs no
change. Both call sites have to branch before a second strategy can exist.
`HostPortSpec` exists in the API, and no Go file outside the generated deepcopy
sets a container `hostPort` anywhere.

**The operator's namespace reaches the policy two different ways, and only one
of them is parameterised.** The per-`Network` policy's egress peer names the
operator's namespace from `NetworkReconciler.OperatorNamespace`, which
`SetupAll` fills from the same `--operator-namespace` flag that builds
`AgentEndpoint` (default `POD_NAMESPACE`, which
`config/deploy/deployment.yaml` sets from the downward API) — so it follows the
operator wherever it runs, and the two values cannot disagree.
`config/deploy/networkpolicy.yaml`, by contrast, hard-codes
`namespace: spawnery-system` and the operator's two pod labels, exactly like
the `+kubebuilder:rbac` markers 6a flagged for 6d. **The Helm chart has to
template that manifest's namespace along with the markers'.** Untemplated, an
operator installed elsewhere gets one of two failures: a loud one if
`spawnery-system` does not exist, since the apply fails the way
`config/rbac/role.yaml`'s did on 6a's first run; and a silent one if it does,
since the policy then lands in a namespace holding no operator pod, selects
nothing, and reports nothing — a security control that is absent and looks
installed.

**The E2E seam is unchanged and now carries fourteen scenarios.** A scenario is
a `func theXxx(t *testing.T)` in a file under `test/e2e/` plus one line in
`TestSpawneryUnderItsOwnServiceAccount`'s explicit `t.Run` list; order is
written down because the scenarios depend on one another, and
`theOperatorWasNeverDenied` stays last because it judges everything the run
did. `eventually`, `eventuallyStable`, `applyManifest`, `operatorPod` and
`operatorLog` are there to reuse; nothing in the package sleeps and then
asserts. The harness's hard limit is also unchanged: no image in
`test/e2e/manifests/e2e.yaml` resolves, by decision, so no container process
ever runs, nothing listens on 25565 or 8081 in a game namespace, and a
connectivity assertion there would be meaningless twice over — once for the
missing process, once for the CNI.

**`internal/rbacaudit` will go red the moment a marker and the table disagree**,
in either direction, and 6b added five entries to `RequiredCluster` for
`networkpolicies`. A `delete` or `patch` marker added later turns `make test`
red before it can ship. The `Why` fields name
`NetworkReconciler.reconcileNetworkPolicy` and the `Owns` call; the file's
standing weakness applies to them as it did to `configmaps` and `pods: patch` —
nothing catches a `Why` that goes stale when a second call site appears.

**One behaviour of the reconcile that the design did not predict, and 6c should
know before it edits `NetworkReconciler`.** `reconcileNetworkPolicy` runs after
the `Accepted` condition is set on the in-memory object but before any
`Status().Update`. An error there returns before the condition is persisted,
and both `ServerGroupReconciler` and `ProxyGroupReconciler` gate on the
`Network`'s `Accepted=True` — so in a cluster where the operator cannot write
NetworkPolicies, a **fresh** `Network` never becomes usable and every group in
its namespace refuses with `ReasonNetworkNotAccepted` and the message
`network "..." has not been accepted yet`. True, and misleading in the same
breath. An already-accepted `Network` keeps its persisted condition, so only
new ones are affected. Failing closed is the right direction — an unprotected
namespace does not quietly come up — but it is a consequence rather than a
decision, and only the operator's log names the cause.

**The `grpcauth` fixture does not wire the cache by default.**
`newAuthFixture` (`internal/grpcauth/auth_envtest_test.go`) builds an
`Authenticator` with no `Cache` and no `Limiter`, which works because both
types' methods are nil-safe, and which is why the cache needed a test of its
own (`TestDeletingAPodRevokesImmediatelyDespiteTheCache`) — a mutation to the
cache broke nothing in the existing suite, because the existing suite never
had one. Anyone adding a case to that package gets the uncached, unlimited
path unless they ask for the other one.

## 4. What the RKE2 rollout now owes

6a's §6 stands unchanged in what it listed: all three images pullable from
`ghcr.io/spawnery/` without a pull secret, the operator running from a digest,
the production `--startup-deadline`, CIS `restricted` pod security, `HostPort`
under a real CNI, a reachable `LoadBalancer` address, several nodes and
therefore node drain and a PodDisruptionBudget under a real eviction, and a
real join. What 6b adds is the whole of its own subject matter, because none of
it could be proven here:

1. **That either policy refuses anything at all.** A single observed refusal —
   an unlabelled pod failing to reach a backend's 25565, or failing to reach
   9443 — is more than this repository has ever produced. It also needs its
   complement: that a *labelled* proxy still reaches its backend and that
   agents still connect, because a policy that refuses everything looks
   identical to a working one in every test 6b ships.
2. **That kubelet probe traffic survives the operator's own policy under a CNI
   that enforces.** This is the risk the peerless rule exists to remove, and
   the run where the rule being wrong would take the operator `NotReady` is the
   first run where it is a risk at all.
3. **The DNAT question.** The per-`Network` policy's egress rule to the
   operator names the operator's pod by selector, while the agent dials a
   Service ClusterIP that kube-proxy DNATs to that pod. Whether the selector
   matches depends on whether the CNI evaluates policy before or after the
   translation; a CNI that evaluates before would need an `ipBlock` over the
   Service CIDR, which the operator cannot discover from inside. The symptom
   would be every agent in every namespace failing to connect at once, with
   every object looking correct.
4. **That the rate limit and the cache behave against real agents at scale.**
   Both are unit-tested. `make test` now passes `-race`, so the detector is a
   standing check rather than something a reviewer remembered to run; the
   50-goroutine hammering of the cache was a throwaway and is still **not** in
   the tree. `make agent-test` shows the keepalive policy does not regress a
   real agent, but no run has ever had many real agents reconnecting at once.
   The claim that a mass reconnect is unaffected rests on the limiter's key
   being one bucket per pod IP — reasoning, not measurement, and it was
   reasoning about something that was **not true until the final fix wave**:
   the key was `IP:ephemeral-port`, so it named a connection rather than a pod
   and every reconnect started a fresh burst. `docs/known-issues.md` carries
   the full entry. Take the reasoning in this bullet as reasoning.

Carried forward from 6a because they are still owed and still unmet: the two
table entries no driven scenario reaches (`persistentvolumeclaims: patch`,
measured; `tokenreviews: create`, reasoned), the `pods: patch` `Why` that names
a call site the harness never reaches, and the coverage gap in
`theOperatorWasNeverDenied` — it catches denied **writes** and was measured not
to catch two cache-backed **lists**, with reads as a class unmeasured. 6b's own
`Forbidden` path is a write, so it is inside what that check covers.

## 5. Open points

[`docs/known-issues.md`](known-issues.md), "From milestone 6b", carries them in
full: the kindnet measurement, the unselected proxies, unlabelled pods in game
namespaces, unrestricted proxy egress, the DNAT question, the peerless rule and
the single unit test standing behind it, the reconcile-ordering consequence
above, the six defects this milestone found in its own plan's test code, and a
tail of smaller ones.

**A whole-branch review ran after the eight tasks and returned "do not merge
as-is"; one fix wave followed and is the last commits on the branch.** What it
found is in `docs/known-issues.md` under the same heading, and three of them
change what a reader of this document should believe. The per-peer rate limit
was per *connection* — `peer.Addr.String()` is `IP:ephemeral-port` — so the
documented attack reset it on every reconnect, and nothing in the suite
exercised the limiter's key at all. The `TokenReview` cache's bound was not a
bound, and its test could not tell. And four places, including a test's own
failure message, said the per-`Network` policy's `managed-by` label existed so
a restricted cache could see the object; no such restriction is in
`cmd/spawnery-operator`, and the claim was deleted rather than the restriction
added, because `CreateOrUpdate` reads through that cache and a pre-existing
unlabelled object would become invisible to it. The design was corrected in
two places as well: §2.4's "needs no report" and §6's DNAT bullet, which named
only the operator hop when the DNS rule has the same exposure.

Two entries elsewhere in that file were amended rather than
deleted, and the amendments are the point: the invariant carried since 3b (now
"the policy is written, and nothing here has seen it refuse a connection") and
milestone 2a's isolation promise, whose availability half 6b **narrows** rather
than closes — nothing bounds how many connections one pod may open, and a
compromised *managed* pod carries the label the policy admits.

## 6. The environment

Unchanged from 6a's own section, plus the last bullet, which 6b learned. Every
command runs inside `nix develop`.

```bash
nix develop -c make test
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

- `make e2e` is part of neither `make test` nor `make all`, deliberately.
- `kind` runs under rootless Podman here, which needs both
  `KIND_EXPERIMENTAL_PROVIDER=podman` and a systemd scope with `Delegate=yes`.
  `systemd-run --user` in turn needs `XDG_RUNTIME_DIR` and
  `DBUS_SESSION_BUS_ADDRESS`, which an interactive login shell has and a
  detached or non-interactive one does not; without them the failure comes
  before `make e2e` starts, as `Failed to connect to user scope bus via local
  transport`. Export `XDG_RUNTIME_DIR=/run/user/$(id -u)` and
  `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus`.
- `TMPDIR` matters: the default `/tmp` is too small for an image archive here.
- The machine has 8 GB and no swap. Run `free -g` rather than quoting a number
  out of a document — 6a's whole image-reproducibility claim went unbacked for
  a milestone because a stale measurement read exactly like a current one. Run
  one cluster at a time; `E2E_KEEP=1` leaves it standing and prints its
  `KUBECONFIG`, and the next run will fight it for memory if it is not deleted.
- Every image derivation takes the working tree as its source, so editing a
  file under `docs/` changes the operator image's derivation hash and makes the
  next `make e2e` rebuild it. That is a slow run, not a wrong one.

## 7. Where everything lives

- Design:
  [`docs/superpowers/specs/2026-08-17-network-policies-design.md`](superpowers/specs/2026-08-17-network-policies-design.md).
- Open points: [`docs/known-issues.md`](known-issues.md), "From milestone 6b",
  plus the amended entries under the milestone 3 preconditions, "From milestone
  3c", and "Preconditions for milestone 6".
- The policies: `internal/podspec/netpol.go` (the per-`Network` one, a pure
  function), `internal/controller/network_controller.go` (who writes it and
  when), `config/deploy/networkpolicy.yaml` (the operator's own).
- The agent channel's bounds: `internal/agentserver/server.go`,
  `internal/grpcauth/cache.go`, `internal/grpcauth/limiter.go`,
  `internal/grpcauth/interceptor.go`, `internal/grpcauth/metrics.go`.
- The tests: `internal/podspec/netpol_test.go`,
  `internal/controller/network_controller_test.go`,
  `internal/rbacaudit/deploy_envtest_test.go`,
  `internal/grpcauth/cache_test.go`, `internal/grpcauth/limiter_test.go`,
  `internal/agentserver/server_envtest_test.go`, `test/e2e/netpol_test.go`.
- 6a's record, and what 6b started from:
  [`handover-milestone-6.md`](handover-milestone-6.md).
