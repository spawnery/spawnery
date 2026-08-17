# Handover to milestone 6

Status: **end of milestone 6a (2026-08-17). The operator runs inside a cluster,
under its own ServiceAccount, from an image the flake builds, and `make e2e`
drives it through twelve ordered scenarios and then reads its whole log for
`is forbidden:`.** Two of the design's nine acceptance criteria are **open**,
and they are open in the sense of "nobody has done it", not "it does not work":
no real `make publish` has been driven, so the digest reference in
`config/deploy/deployment.yaml` has never been resolved by anything. Both are
the repository owner's, like an evidence run. §1 says why.

This document is not a spec. It says where 6a stopped and what 6b —
NetworkPolicies — finds when it starts, checked against the code as 6a leaves
it rather than against the plan that preceded it. The design decisions live in
[`docs/superpowers/specs/2026-08-16-operator-image-and-e2e-design.md`](superpowers/specs/2026-08-16-operator-image-and-e2e-design.md);
the open points are in [`docs/known-issues.md`](known-issues.md), whose "From
milestone 6a" and "From the milestone 6a Task 4 measurement round" sections
this document does not repeat in full.

**If you are looking for milestone 5's own record — persistent groups, ordered
shutdown, secret rotation, three evidence runs against a real cluster — it
stays at [`handover-milestone-5.md`](handover-milestone-5.md).** This document
starts fresh rather than extending it, for the same reason that one started
fresh from milestone 4's: milestone 6 is a different subsystem — packaging,
deployment and cluster-level proof rather than storage — and it is written
against a different spec.

## 1. Where 6a stopped

**Built and driven:**

- `nix build .#operator-image` (`nix/operator-image.nix`) produces a tarball for
  the operator itself, non-root, on `x86_64-linux` only, rebuilt bit-identically
  by `make image-repro` alongside the other two images and the agent jars.
  `make operator-image-test` runs it.
- `hack/publish.sh` (`make publish`) copies all three images from their Nix
  archives straight to `ghcr.io/spawnery/` with `skopeo` — no local container
  store in between, so what lands in the registry is what the flake describes.
  It refuses a tag that is already there unless `FORCE=1`, prints without
  contacting anything under `DRY_RUN=1`, and can rewrite the operator's image
  reference in `config/deploy/deployment.yaml` to the returned digest under
  `WRITE_DIGEST=1`.
- `make e2e` (`hack/e2e.sh`) builds the operator image, creates a `kind`
  cluster, loads the archive into it, installs the CRDs, `config/rbac/role.yaml`
  and `config/deploy/`, applies milestone 5c's per-namespace forwarding-secret
  reader Role into `minecraft`, patches the Deployment's image and startup
  deadline, waits for the rollout, and runs `go test -tags e2e ./test/e2e/...`.
  It tears the cluster down on the way out, and dumps the operator log, the
  objects and the events first if the run failed.
- `config/deploy/deployment.yaml` is now a manifest a person installs rather
  than scaffolding: it carries the production `--startup-deadline=5m` (the
  E2E appends a second, shorter occurrence for its own run and Go's `flag`
  package takes the last), and `internal/rbacaudit`'s
  `TestTheOperatorDeploymentCarriesProductionFlags` guards both that floor and
  the flag list, rejecting any flag the test was not told about.

**Not driven, and belonging to the repository owner:**

- **The first real `make publish`.** It needs a GitHub token with
  `write:packages`, which nobody in milestone 6a had. What has been run is
  `DRY_RUN=1`. As of 2026-08-17 `ghcr.io/spawnery/paper` is confirmed *not*
  publicly pullable — an anonymous
  `https://ghcr.io/token?scope=repository:spawnery/paper:pull` returns 403,
  while the same request against a known-public repository returns a token.
  So acceptance criterion 8, "all three images are pullable from
  `ghcr.io/spawnery/` without a pull secret", is unmet by measurement rather
  than by assumption.
- **A digest reference that resolves** (acceptance criterion 7). The manifest
  still names `ghcr.io/spawnery/spawnery-operator:0.1.0`, a tag, because
  `WRITE_DIGEST=1` has never had a push to write back from. `make e2e` cannot
  close this either: it patches the image to the locally built archive and sets
  `imagePullPolicy: Never` on purpose, so the run tests the bits just built and
  never resolves the reference the manifest ships.

Do not mark either criterion met from the existence of the script. The script
is the mechanism; the run is the evidence, and this project's convention — five
evidence runbooks deep — is that a mechanism is not evidence.

## 2. What 6b finds in place

Milestone 6b is NetworkPolicies. What follows is the state of the tree it
starts from, read off the code rather than off the plan.

**The operator is in the cluster now, and that is what makes the policy
meaningful.** It runs as a `Deployment` in `spawnery-system` with pod labels
`app.kubernetes.io/name=spawnery` and `app.kubernetes.io/component=operator`,
and `config/deploy/service.yaml` is an ordinary selector Service over those two
labels exposing port 9443 only. Note what the operator pod does *not* carry:
`spawnery.cloud/managed-by`. A policy written to select managed pods will not
select the operator, which is a feature — the two ends of the agent channel need
different rules — but it is a trap if a rule is written by copying a selector.

**The traffic matrix 6b has to describe, as the code actually has it:**

| from | to | port | how the address is found |
|---|---|---|---|
| server pod, any namespace | operator pod, `spawnery-system` | 9443/TCP | `spawnery-operator.<operator-ns>.svc`, baked into `SPAWNERY_OPERATOR_ENDPOINT` at pod creation |
| proxy pod, any namespace | operator pod | 9443/TCP | the same |
| proxy pod | server pod, same namespace | 25565/TCP | the raw pod IP, delivered over the proxy's own gRPC stream |
| external client | proxy pod | 25565, via a NodePort Service | `externalTrafficPolicy: Local` |
| kubelet | proxy pod | 8081/TCP | readiness probe |
| kubelet | operator pod | 8081/TCP | liveness and readiness |
| operator | API server | — | controllers, plus a `TokenReview` per agent connection |

Four consequences worth having in front of you before writing a rule:

- **The agent channel is cross-namespace by construction.** `agentEndpoint`
  (`cmd/spawnery-operator/main.go`) builds the name from the *operator's* own
  namespace, not the pod's, so every managed pod in every namespace dials into
  `spawnery-system`. Any ingress rule on 9443 has to allow a namespace the
  operator's own chart does not know the name of — which means selecting on the
  pod label `spawnery.cloud/managed-by` across all namespaces, not on a
  namespace name.
- **A backend is reached by pod IP, never through a Service.** There is no
  Service in front of server pods at all; `Server.status.address` is
  `<podIP>:25565` and the proxy learns it over its stream. So "backends accept
  connections only from proxies" is a rule about pod selectors, and nothing
  about it can be expressed in terms of a Service.
- **The labels for all of this already exist and are stable.**
  `internal/podspec/labels.go` stamps `spawnery.cloud/managed-by`,
  `spawnery.cloud/network`, `spawnery.cloud/group` and `spawnery.cloud/role`
  (`server` or `proxy`) on every pod the operator creates, and
  `LabelNetwork`'s own doc comment has said "NetworkPolicies select on it"
  since before there were any. `ManagedSelector(network)` is the ready-made
  pair. Do not select on `spawnery.cloud/pod-hash`, `occupied` or
  `forwarding-hash`: those move under the pod's feet by design.
- **Egress is not only the proxy.** The endpoint is a DNS name, so DNS egress
  to `kube-system` is required or no agent connects at all. Paper also reaches
  `fill.papermc.io` (its update check) on startup, measured to fail harmlessly
  with no network — `docs/known-issues.md`'s milestone 2b section has both
  outbound calls and says the egress policy has to make a decision about them.
  A proxy configured `onlineMode: true` additionally needs Mojang's session
  server; a backend never does, because backends are always `online-mode=false`
  — which is the invariant the missing policy exists to protect.

**There is a precedent for the operator writing objects into namespaces it did
not create.** `Bootstrapper.Ensure` (`internal/controller/bootstrap.go`) creates
the `spawnery-ca` ConfigMap and the `spawnery-server` and `spawnery-proxy`
ServiceAccounts in every namespace it touches, called from the Server controller
and the ProxyGroup controller just before the first pod of that namespace could
exist. Everything it writes carries `LabelManagedBy` — because the manager's
cache is narrowed to that label, so an unlabelled object would be invisible to
the operator that wrote it — and **nothing it writes carries an owner
reference**, deliberately, so a pod restarting during an operator outage still
finds a CA to trust. §3 is why that precedent is a decision rather than a
template.

**`internal/rbacaudit` will go red the moment a `networkpolicies` marker appears
without a table entry, and this is not a claim about intent.** `make test` runs
`controller-gen` first, so a new `+kubebuilder:rbac` marker lands in
`config/rbac/role.yaml` in the same invocation that then tests it;
`TestClusterRoleGrantsNothingExtra`
(`internal/rbacaudit/audit_envtest_test.go`) reads that freshly generated role
and reports, per expanded triple, `the clusterrole grants
networking.k8s.io/networkpolicies/create, which no entry in the matching
rbacaudit table claims`. The reverse direction fails the same way with `the
rbacaudit table lists ..., which the clusterrole never mentions`. Adding the
entry to `RequiredCluster` in `internal/rbacaudit/required.go` — with a `Why`
that names the real call site, and see §6 on how easily that field goes stale —
is the whole cost, and `test/e2e/rbac_test.go` then covers the new permission
against a real cluster's authorizer for free.

**The E2E has a seam for new scenarios, and a hard limit 6b will hit.** A
scenario is a `func theXxx(t *testing.T)` in one of the files under `test/e2e/`
plus one line in `TestSpawneryUnderItsOwnServiceAccount`'s explicit `t.Run`
list; the order is written down rather than left to file naming because the
scenarios depend on one another, and the denial check is last because it judges
everything the run did. `eventually`, `eventuallyStable`, `applyManifest`,
`operatorPod` and `operatorLog` are there to reuse. Nothing in the package waits
a fixed time and then asserts — that is design §6.4, kept, because a run built
on sleeps turns flaky under load and a flaky E2E run is ignored within weeks —
and a new scenario should not be the first to. The 500 ms inside `eventually`
is a poll interval, not a wait.

The limit: **no image in that harness resolves**, by decision
(`test/e2e/manifests/e2e.yaml` names `:e2e-no-such-tag` for both game images),
so no container process ever runs. Nothing listens on 25565 or 8081, nothing
dials 9443, and there is no `kubectl exec` target. A connectivity assertion is
therefore not merely hard there, it is meaningless — a perfect policy and a
broken one produce the same observation. Two things *are* provable without an
image: that the operator created the objects it was supposed to (exactly the
shape of the existing `theProxyGroupGetsItsService`), and that its new
permission is granted and never denied, which the two RBAC scenarios pick up
with no new code.

And one more thing to settle before writing any test that asserts a connection
was *refused*: **enforcement is a property of the CNI, not of the object.**
`hack/e2e.sh` runs a bare `kind create cluster` with no `--config`, so the
default kindnet, and `kind` is unpinned in `flake.nix`. If that CNI drops
nothing, "the connection was blocked" and "the policy was never applied" are
the same green. Either verify kindnet's enforcement in the version nixpkgs
supplies, or bring up the cluster with `disableDefaultCNI: true` and a CNI that
enforces, or assert objects only and say so out loud in the test's own comment.

## 3. The one structural thing 6b has to decide

**Who writes the per-namespace NetworkPolicy — the operator, or a person — and
what the answer costs.** This is 6b's equivalent of 4b's soft-drain question.
It is not settled here.

The shape of the problem: a `NetworkPolicy` is namespaced and selects pods in
its own namespace. The rule that matters most — a backend accepts connections
only from proxies of its own network — has to exist **in the game namespace**,
and game namespaces are discovered at runtime. A `Network` may live in any
namespace, one per namespace
(`NetworkReconciler.pickNamespaceOwner`), and nothing tells the Helm chart at
install time where they will be. The operator-side rule is the easy half: an
ingress policy on 9443 selecting the operator pod and admitting only pods
labelled `spawnery.cloud/managed-by`, cluster-wide, is expressible with a
`namespaceSelector: {}` and ships perfectly well in the chart. It is the game
namespaces that force a choice.

Three shapes, and the choice belongs in the 6b spec:

- **The operator writes them, following `Bootstrapper.Ensure`.** The precedent
  exists, the call site exists — the policy would land exactly when the first
  pod of a namespace does — and the policy can track the Network's own labels
  without anyone maintaining a copy. The cost is authority: the operator gains
  `networkpolicies: create` and `update` cluster-wide, which is the right to
  change any namespace's security posture. This project already has a ruling
  in that neighbourhood and it went the other way — milestone 5c deliberately
  kept `config/rbac/forwarding-secret-reader.yaml` out of `config/deploy/`, on
  the argument that an operator which may write RBAC makes every other
  restriction on it advisory. Whether NetworkPolicy write is the same class of
  authority as RBAC write is exactly the question, and it deserves an answer
  rather than an analogy. A second cost, smaller but real: `Bootstrapper`'s
  rule is *no owner reference*, so nothing would ever collect these policies —
  and a stale ConfigMap is inert where a stale NetworkPolicy silently drops
  traffic in a namespace nobody associates with Spawnery any more.
- **The chart ships them and an administrator applies one per game namespace**,
  the shape 5c chose, complete with a fourth `rbacaudit` table guarding the
  hand-written manifest the way `RequiredNetworkNamespace` guards the reader
  Role. The cost is that a security control which is off by default is, in most
  installations, absent. 5c could afford that because *not* applying its
  manifest produces a visible, reported condition —
  `ForwardingSecretResolved=Unknown/SecretReadForbidden`, with a message naming
  the file and the `kubectl apply` line. Not applying a NetworkPolicy produces
  nothing at all: the system works exactly as well without it, which is the
  whole reason it has been overdue since 3b. Taking this shape therefore also
  means inventing the report — a condition on the `Network` saying this
  namespace is unprotected — and that is new API surface, not a free choice.
- **The operator writes them, but only where it has been told to** — an opt-in
  field on the `Network` or an opt-in namespace label, so the authority is
  granted once at install and exercised only where an administrator asked for
  it. It splits the difference honestly and adds a third configuration axis
  somebody has to remember to read, which is the shape this repository has been
  bitten by before (`internal/controller/candidates.go` records what two
  implementations of one rule cost when they drifted). It also invites the
  worst failure mode of the three: a field that reads `enforced` while the
  cluster's CNI enforces nothing.

Whichever is chosen, two things must survive into the 6b spec as acceptance
criteria:

1. **The policy must not be able to break the readiness contract.** A proxy
   pod's readiness is a kubelet dial to port 8081, and 4c-1's whole drain
   mechanism is the agent closing that port on request. A policy that
   accidentally covers probe traffic converts every proxy in the cluster to
   `NotReady` and takes the fleet down; whether kubelet traffic is subject to
   policy at all is CNI-dependent, so this has to be *tested*, not reasoned.
2. **What the E2E can prove about it must be stated in the test rather than
   assumed by the reader** — see the CNI paragraph at the end of §2. A test
   that asserts the object and a test that asserts the block are different
   claims, and only one of them is available in this harness today.

## 4. The other decisions worth settling before code

- **Whether 6b also closes the agent channel's availability gap, or only its
  network half.** `docs/known-issues.md` records both under one heading:
  `grpc.NewServer` in `internal/agentserver` sets no `MaxConcurrentStreams`, no
  `ConnectionTimeout` and no keepalive policy (verified: the constructor takes
  exactly `grpc.Creds` and one stream interceptor); there is no rate limit in
  front of `Authenticator.Authenticate`; and every connection costs a live
  `TokenReview` write against the API server with no cache. The NetworkPolicy
  removes the anonymous half of that — today port 9443 is reachable from every
  pod in the cluster — but a compromised *managed* pod still has a label that
  passes the policy, so the policy alone does not close it. The gRPC bounds are
  a different kind of change in a different package; deciding they are 6b's is
  fine, and deciding they are not is fine, but leaving it unstated means
  quoting milestone 2a's isolation promise while the availability half is still
  open.
- **Whether the policy is per network or per namespace.** They coincide today —
  one accepted `Network` owns a namespace — but `spawnery.cloud/network` exists
  as a label precisely so a rule can be narrower than the namespace, and the
  answer determines whether a second Network arriving in a namespace changes
  anything.
- **What an unlabelled pod in a game namespace gets.** A pod-selecting policy
  leaves everything it does not select entirely unrestricted; a namespace-wide
  default-deny protects more and breaks any co-tenant workload the operator
  never knew about. Spawnery does not own those namespaces.

## 5. What 6c and 6d inherit

- **`spawnery-system` is still hard-wired in the RBAC markers.** The
  `+kubebuilder:rbac` markers for the TLS secret (`internal/certs/store.go`) and
  the leases (`internal/controller/setup.go`) carry
  `namespace=spawnery-system` as a literal, so `controller-gen` produces a Role
  bound in that namespace whatever the operator's actual one. The operator then
  fails at its first `certs.Ensure` or during leader election, with RBAC never
  reporting where the problem is. 6a runs in `spawnery-system`, so it does not
  bite there. **The chart has to parameterize the namespace in the markers, not
  only in the object names** — this is the single most likely way 6d ships
  something that works on the author's machine and nowhere else.
- **`config/rbac/role.yaml` cannot be applied before its namespace exists.** It
  carries a cluster-scoped ClusterRole *and* a namespaced Role, and a plain
  `kubectl apply -f config/deploy/` walks the directory alphabetically, so the
  Deployment precedes the Namespace. The first of those was reproduced on the
  first run of `hack/e2e.sh` — `namespaces "spawnery-system" not found`, not a
  hypothetical — and the second follows from the same ordering; the script now
  applies `namespace.yaml` on its own before either. Helm has its own answer to
  install ordering; use it, rather than
  porting the script's sequence.
- **Only `NodePort` is implemented, and the refusal of the other two is
  deliberate.** `ProxyGroupReconciler` refuses any other `expose.type` with
  `ReasonExposeNotImplemented` and a message naming milestone 6, while still
  requeueing so a group with occupied pods keeps its budget maintained. The API
  enum already carries `LoadBalancer` and `HostPort`, and `HostPortSpec` exists
  with nothing setting a container `hostPort` anywhere. One concrete trap for
  6c: `proxyAddress` dereferences `group.Spec.Expose.NodePort` unconditionally
  when deriving `status.address`, so that call site has to branch before a
  second strategy can exist.
- **Publishing is hand-driven.** Automating it is 6e's, along with the
  `agent/deps.json` drift guard that `docs/known-issues.md` has been parking
  under "belongs with CI in milestone 6" since milestone 2c. Where CI runs is a
  question no code in this repository answers; whatever runs it needs a real
  Docker daemon, a rootful Podman socket, or `kind` under rootless Podman set
  up the way §7 describes.

## 6. What the RKE2 rollout at the end of milestone 6 owes

Design §12, restated because it is what 6a's decisions were made against: all
three images come from `ghcr.io/spawnery/` without a pull secret, the operator
runs from the digest in `config/deploy/deployment.yaml` (by then rendered by
the chart), and `--startup-deadline` is the production value.

What that run owes and 6a cannot: CIS `restricted` pod security against the
operator's own security context and against a game server namespace, `HostPort`
under the cluster's actual CNI, a `LoadBalancer` address a client can reach,
several nodes — and therefore node drain and a PodDisruptionBudget under a real
eviction, none of which a single-node `kind` cluster can touch — and a real
join. It is a runbook, driven once, marked `DRIVEN`, in the manner of
`docs/runbook-milestone-3-evidence.md` and its four successors.

Two things the E2E has already established that this run should not re-derive,
and one it should:

- The denial check `theOperatorWasNeverDenied` catches a missing **write**
  permission and does not catch a missing **read** permission. The asymmetry is
  measured; the explanation for it — that reads go through the manager's cache
  and never reach the API server — is a hypothesis nothing here established, so
  do not carry it forward as fact. Measured both ways: a revoked
  `pods: create` produced a quoted denial immediately, a
  revoked `pods: list` produced nothing at all across seven and three-quarter
  minutes of continuous watching, with no 403 in the operator's own metrics and
  no effect on `Available`. Both halves are in `docs/known-issues.md`.
- Two table entries no driven scenario reaches:
  `persistentvolumeclaims: patch` (measured — nothing in the harness grows a
  claim) and `tokenreviews: create` (reasoned, not measured — no agent process
  ever runs, and the client metric has no per-resource label to prove it with).
  A real RKE2 rollout with resolvable images exercises both, and is the first
  thing that can.
- **The one to re-derive:** `internal/rbacaudit/required.go`'s `Why` field for
  `pods: patch` names `syncOccupiedLabel`, a call site nothing in the harness
  reaches, while `ProxyGroupReconciler.syncOccupiedLabels` exercises the grant
  on every run. That is the second entry of its shape in
  `docs/known-issues.md`'s "On the RBAC audit" list, after the `configmaps`
  one. A rollout that drives real agents is the first occasion to check whether
  the *named* call site works at all.

## 7. The environment

Unchanged from `docs/handover-milestone-5.md`'s own section, plus what 6a adds.
Every command runs inside `nix develop`.

```bash
nix develop -c make test
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make image-test CONTAINER=podman
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" make operator-image-test CONTAINER=podman
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

- `make e2e` is part of neither `make test` nor `make all`, deliberately: it
  builds an image and a cluster and takes minutes, and the commit loop stays at
  around twenty-five seconds.
- On this machine `kind` runs under rootless Podman, which needs both
  `KIND_EXPERIMENTAL_PROVIDER=podman` and a systemd scope with
  `Delegate=yes` — `kind` checks for the scope, not for the property being set
  on the user's service. `hack/e2e.sh` hard-codes neither; its header carries
  the invocation.
- `TMPDIR` matters: the default `/tmp` is too small for an image archive on
  this machine.
- `systemd-run --user` needs `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS`
  in the environment. An interactive login shell has both; a detached or
  non-interactive one may not, and the failure comes before `make e2e` starts,
  as `Failed to connect to user scope bus via local transport`. Exporting
  `XDG_RUNTIME_DIR=/run/user/$(id -u)` and
  `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus` is the fix. This
  is not a defect in `hack/e2e.sh`, which hard-codes neither.
- The machine has **3.9 GB of RAM and no swap**. Run `make e2e` in the
  foreground and one cluster at a time. Do not run `make agent` here — it
  exhausts memory. `E2E_KEEP=1` leaves the cluster standing for inspection and
  prints its `KUBECONFIG`; remember to `kind delete cluster --name spawnery-e2e`
  afterwards, because the next run will otherwise fight it for memory.
- Nothing under `proto/` or `agent/` moved on the 6a branch
  (`git diff master...HEAD --name-only`), so 6a added no agent-facing message
  and `make agent-test` needed no extension. That is checked against the diff,
  and `make agent`/`make agent-test` were deliberately not run — the memory
  limit above is the reason, and the diff is the evidence that they did not
  need to be.

## 8. Where everything lives

- Design: `docs/superpowers/specs/2026-08-16-operator-image-and-e2e-design.md`.
  Its §2 says which parts of the older
  `docs/superpowers/specs/2026-08-07-e2e-testcluster-design.md` it supersedes;
  that document now carries a status header saying the same from its own side.
- Open points: `docs/known-issues.md`, sections "From milestone 6a", "From the
  milestone 6a Task 4 measurement round", "Preconditions for milestone 6
  (Helm, RBAC, E2E)", and the bullet lists under "On the RBAC audit" and "On
  the agent channel".
- The harness: `hack/e2e.sh` (plumbing only — every claim is made by Go),
  `test/e2e/` (the claims), `test/e2e/manifests/e2e.yaml` (the fixture).
- The images: `nix/operator-image.nix`, `nix/oci-common.nix`,
  `hack/operator-image-test.sh`, `hack/publish.sh`.
- The installed manifests: `config/deploy/`, `config/rbac/role.yaml`, and
  `config/rbac/forwarding-secret-reader.yaml`, which is deliberately not part
  of `config/deploy/` and which `hack/e2e.sh` applies by hand because the run
  is the first thing in this repository that has to act as the administrator.
