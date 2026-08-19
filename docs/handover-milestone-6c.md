# Handover to milestone 6d

Status: **end of milestone 6c (2026-08-19). All three expose strategies —
`NodePort`, `LoadBalancer`, `HostPort` — reconcile, a proxy pod that cannot
come into existence says why on its group's `Degraded` condition, and
`make e2e` drives eighteen scenarios green in 139.0 seconds, four of them new.
Exactly one of those four observes anything being enforced.**

That last sentence is the milestone, not a caveat on it. No `LoadBalancer`
controller runs anywhere in this repository, no image in the E2E manifest
resolves so no container process ever runs, and kindnet — unchanged since
6b — enforces no `NetworkPolicy`. The one thing 6c observed being enforced is
the API server refusing a `HostPort` pod in a namespace enforcing Pod Security
`baseline`, at two levels (§2). Everything else 6c ships is an object: a
Service of the right type, a container port, a condition, a deletion. §2 says
exactly what was driven and what only exists; read it before writing a
sentence about what 6c proves.

**Superseded as the document to start from: milestone 6d has landed, and
anyone starting 6e begins at
[`handover-milestone-6d.md`](handover-milestone-6d.md).** This one is kept
unedited as the record of what 6d started from — §3's survey of the tree is
the evidence base for the decisions 6d then made, and its tense is the tense
of 2026-08-19, before 6d existed. §3 makes four distinct claims; all four
were checked against the code as it now stands. One is now false and must
not be read as open. "`spawnery-system` is still hard-wired in three places,
unchanged by 6c" is gone in the way that paragraph itself anticipated:
`config/deploy/networkpolicy.yaml:14`'s hard-coded namespace no longer
exists — the file it was in was deleted along with the rest of
`config/deploy/` — and the operator's NetworkPolicy is now
`charts/spawnery/templates/networkpolicy.yaml`, templated on
`{{ .Release.Namespace }}` like everything else the chart installs. The two
`+kubebuilder:rbac` markers this paragraph also names
(`internal/certs/store.go:57`, `internal/controller/setup.go:72`) still
carry the literal `namespace=spawnery-system` — controller-gen requires
*some* namespace to emit a Role at all — but each now carries a comment
saying the literal is a placeholder `hack/chart-templates.sh` replaces at
render time, not a statement about where the operator runs. The other three
claims hold exactly as written: `config/samples/network.yaml` still shows
all three strategies, with `NodePort` active and the other two commented
alongside the cost each carries; the refusal path
(`exposeImplemented`, now at `internal/controller/proxygroup_controller.go:1762`,
line numbers having moved since this document's own §3 was written)
is still a guard for an enum value the CRD cannot produce, unreachable while
`ExposeType`'s own `+kubebuilder:validation:Enum` marker and
`exposeImplemented` agree, kept for the day a fourth value is added to one
without the other; and `ProxyGroupReconciler.Recorder`'s own field doc
comment still undercounts what it announces, untouched by any 6d commit.
`docs/handover-milestone-6d.md`'s own §3 carries the current facts for the
claim that changed; this document's §3 is left as written, for the same
reason `handover-milestone-6b.md`'s was: it is the record of what 6d was
decided against, not a claim about what is true today.

This document is not a spec. It says where 6c stopped and what 6d — the Helm
chart — finds when it starts, checked against the code as 6c leaves it rather
than against the plan that preceded it. The design decisions live in
[`docs/superpowers/specs/2026-08-18-expose-strategies-design.md`](superpowers/specs/2026-08-18-expose-strategies-design.md);
the open points are in [`docs/known-issues.md`](known-issues.md), whose "From
milestone 6c" section this document does not repeat in full.

**Why this is a new document rather than a section appended to
[`handover-milestone-6b.md`](handover-milestone-6b.md).** That document was
written *for* 6c, and its §2 ("The one thing 6c must not misread") and §3
("What 6c finds in place") are the record of what 6c started from and had to
decide — including the CNI measurement its §2 carries, which nothing in 6c
re-measured, only inherited. Rewriting those sections into the past tense
would delete the evidence base for 6c's own decisions, and leaving them in the
present tense inside a document a 6d reader opens would report a settled
question as open. Milestone 5 started fresh from 4's for the same class of
reason, 4b got its own once it stopped being the thing to read 4c against, and
6a's handover stopped being that the moment 6b landed. It keeps its own value
as history. Everything in 6b's handover that is still true and still needed —
its §4 on what the RKE2 rollout owes, its §6 on the environment — is carried
forward here, re-checked against the tree rather than copied on faith.

## 1. Where 6c stopped

**Built and driven, task by task:**

- **The container binds the port for one strategy** (`b2d3a93`).
  `internal/podspec/proxy.go`'s `renderProxyPod` sets the Minecraft
  container's `HostPort` only when `group.Spec.Expose.Type == ExposeHostPort
  && group.Spec.Expose.HostPort != nil`, inside the function shared by
  `BuildProxyPod` and `DesiredProxyHash`, so a switch into or out of
  `HostPort` changes the hash and makes the drain-aware rollout milestone 4
  built replace every pod.
- **The Service and the address follow the strategy** (`89bf635`).
  `reconcileService` (`internal/controller/proxygroup_controller.go:1248`)
  builds a `NodePort` or `LoadBalancer` Service through one `CreateOrUpdate`,
  and for `HostPort` returns no Service and calls `deleteServiceIfOurs`
  (`:1366`), which removes a leftover Service only when
  `metav1.GetControllerOf(svc).UID == group.UID` — one this operator did not
  create is left alone. `proxyAddress` (`:1611`) takes `(group, pods, svc)`:
  every strategy shares one readiness gate (no address unless a pod is
  `Ready` with a non-empty `HostIP`), then branches — `NodePort`/`HostPort`
  use that pod's `HostIP` with the strategy's own port, `LoadBalancer` reads
  `svc.Status.LoadBalancer.Ingress`. RBAC gained `services: delete`
  (`:175`), mirrored in `internal/rbacaudit/required.go:142-147`.
- **The operator owns only the annotations it sets** (`740a073`).
  `applyExposeAnnotations` (`:1322`) copies
  `spec.expose.loadBalancer.annotations` onto the Service and records which
  keys it wrote in the bookkeeping annotation
  `podspec.AnnotationExposeAnnotations` (`spawnery.cloud/expose-annotations`,
  `internal/podspec/labels.go:102`); on the next reconcile it deletes only
  the keys that were both previously owned and no longer wanted, so a key
  MetalLB or kube-vip wrote is never touched.
- **The refusal narrows to a guard for an enum value the controller does not
  know** (`d217c0e`). The blanket `!= NodePort` refusal in `Reconcile` is
  replaced by `exposeImplemented` (`:1670`), a `switch` over the three known
  values, called at the guard on `:229`. `TestProxyGroupRefusesLoadBalancer`
  was deleted; its regression coverage now lives in
  `TestReconcileAcceptsEveryStrategy`.
- **A proxy pod that cannot exist says so on its group** (`9253ba2`, fix
  round `491fbc5`). `ConditionDegraded` on a `ProxyGroup` had no writer before
  this task. `setProxyPodsBlocked` (`:1472`) and `reportBlockedProxies`
  (`:1506`) now report `ReasonProxyPodRejected` when the API server refuses a
  create — wired into `Reconcile`'s error path at `:293-303`, gated on
  `apierrors.IsForbidden(err) || apierrors.IsInvalid(err)` — or
  `ReasonProxyPodUnschedulable` when the scheduler cannot place a pod, and
  `ReasonProxyPodsAdmitted` once every proxy pod exists. The fix round added
  the missing recovery event on the `True→False` flank and corrected the
  reconciler's type doc comment from "three occasions" to "four" (`:94-125`).
- **The driven run gets four scenarios and one real refusal** (`9e4b7a7`, fix
  round `47219c3`). `test/e2e/expose_test.go` (new) adds
  `theLoadBalancerGroupGetsItsService`, `theHostPortGroupBindsThePortAndHasNoService`,
  `aSwitchToHostPortRemovesTheService`, `aForbiddenHostPortIsReportedOnTheGroup`,
  registered in `TestSpawneryUnderItsOwnServiceAccount`'s ordered list
  (`test/e2e/e2e_test.go:113-116`) between "the proxy group gets its Service"
  and "the operator holds its secret and its lease". `hack/e2e.sh` creates a
  second namespace, `minecraft-baseline` (`:135`), and labels it
  `pod-security.kubernetes.io/enforce=baseline` on the next line (`:136`),
  before anything else is applied into it. `theOperatorWasNeverDenied`
  (`:199`) now excludes any log line containing `violates PodSecurity`
  from its offender check (`:221-222`) — see §2. The fix round moved
  `gateway-forbidden`'s `hostPort.port` from 25565 (shared with
  `gateway-host`, a confounder) to 25567.

**Verified at the end of the milestone:** `nix develop -c make test` green
(race detector on, per 6b's standing rule); `make e2e` green with all
eighteen scenarios, `139.022s`
(`ok github.com/spawnery/spawnery/test/e2e 139.022s`, `a_forbidden_host_port_is_reported_on_the_group`
at `0.10s`), no kind cluster left standing afterward.

**Re-verified after the final whole-branch review's fixes:** `make test`
green; `make e2e` green with the same eighteen scenarios, `142.766s`
(`ok github.com/spawnery/spawnery/test/e2e 142.766s`), no kind cluster left
standing. The one number that moved is the one §2 is about.

**Not driven, and not drivable here:** the `LoadBalancer` address path
against a real load balancer controller — envtest now drives it against a
Service whose ingress entry the test writes itself, which is the wiring and
not the load balancer (§2) — a real CNI's treatment of `HostPort`, and
anything about reachability. See §2.

## 2. The one thing 6d must not misread

**Exactly one thing in this milestone was observed being enforced: the API
server refusing a `HostPort` pod under Pod Security `baseline`.** It was
confirmed at two levels, not assumed at either:

- **envtest.** `TestARejectedProxyPodIsReportedOnTheGroup`
  (`internal/controller/expose_test.go`) labels the fixture namespace via
  `enforcePodSecurity(t, "baseline")` (`:711-727`,
  `pod-security.kubernetes.io/enforce`). Task 5's report records that, run
  before any implementation existed, this test's `Reconcile` call already
  returned the API server's own error — confirming the PodSecurity admission
  plugin is active in this project's envtest binary (Kubernetes **1.36.3**)
  and the blocker scenario is real, not a stand-in — and only then failed on
  the missing `Degraded` condition, because nothing yet wrote one.
- **A real cluster.** `aForbiddenHostPortIsReportedOnTheGroup`
  (`test/e2e/expose_test.go:198-229`) waits for `gateway-forbidden`, in the
  `minecraft-baseline` namespace, to carry `Degraded=True` with
  `ReasonProxyPodRejected` and a message naming `PodSecurity`, then asserts
  no pod for that group exists at all (`:219-228`).
- **The causal link was tested, not assumed.** Task 6's Mutation 1 commented
  out the label in `hack/e2e.sh`. The scenario failed both times it was run
  without the label — first (before a port-isolation fix) because
  `gateway-forbidden`'s admitted pod lost a scheduler race for host port
  25565 against `gateway-host`'s own pod on kind's single node, a confounder
  the fix round removed by moving `gateway-forbidden` to port 25567; and
  again, cleanly, after that fix — the pod was scheduled without incident and
  the group never went `Degraded` at all. Both outcomes are failures, by
  different mechanisms, and both confirm the label is load-bearing.

**The other three new E2E scenarios assert objects, not enforcement:**

- `theLoadBalancerGroupGetsItsService` — a `LoadBalancer` Service exists with
  the right type, `externalTrafficPolicy: Local`, the manifest's
  `metallb.universe.tf/address-pool` annotation, and port 25565. kind runs no
  load balancer controller, so the test writes
  `svc.Status.LoadBalancer.Ingress` itself and then asserts `status.address`
  **stays empty**, because no image in the manifest resolves so no proxy is
  ever `Ready`. What that proves is the readiness gate on `proxyAddress`, not
  the address path — the scenario's own doc comment says as much, and now
  names the envtest that proves the other half
  (`test/e2e/expose_test.go:21-34`).
- `theHostPortGroupBindsThePortAndHasNoService` — a pod's container port
  carries `hostPort: 25565`, no Service exists, and `gateway-host`'s surplus
  replica (two requested on a one-node cluster) is reported
  `Degraded`/`ReasonProxyPodUnschedulable`.
- `aSwitchToHostPortRemovesTheService` — the operator deletes the Service
  under its own ServiceAccount when a group switches strategy.

**The `LoadBalancer` address-appears path (an assigned ingress plus a `Ready`
pod producing a non-empty `status.address`) is now driven through a live
reconcile, and until the final whole-branch review of this branch it was
not.** What existed was the plain Go table test `TestProxyAddressPerStrategy`
(`internal/controller/expose_test.go`), case "LoadBalancer publishes the
assigned ingress IP", which calls `proxyAddress(group, pods, svc)` directly
with literal `*corev1.Pod`/`*corev1.Service` values — the function, not the
path through a running reconciler against a real object. Design §7 described
the envtest that was meant to close that gap and §12's third acceptance
criterion required it; neither was written, and replacing
`r.setStatus(group, pods, svc)` with a call whose Service had been stripped
of its status left `go test ./internal/controller/...` entirely green.

`TestTheLoadBalancerAddressAppearsOnceAProxyIsReady`
(`internal/controller/expose_test.go`) is that test. It reconciles a live
`LoadBalancer` group, patches the Service's status subresource with an
ingress entry the way MetalLB would, asserts `status.address` stays empty
while nothing is ready, marks a pod `Ready` the way a kubelet would, and
observes the assigned address come out. It is the only test in the package
that fails under that mutation. **It still proves no reachability**: envtest
runs no load balancer controller and no kubelet, so both halves are written
by the test itself, and what it establishes is the wiring — the Service
`reconcileService` returned reaching `setStatus`, and `status.address` coming
out of it.

**`theOperatorWasNeverDenied` is narrower than it was before this milestone,
on purpose, and that narrowing is worth stating plainly rather than folding
into a diff.** Its offender loop
(`test/e2e/e2e_test.go:221-222`) now reads
`strings.Contains(line, "is forbidden:") && !strings.Contains(line, "violates PodSecurity")`
— a log line containing `violates PodSecurity` no longer counts as an RBAC
denial. Without the exclusion, `aForbiddenHostPortIsReportedOnTheGroup` would
fail this check on every run: Task 6's Mutation 2 (removing the exclusion)
reproduced this exactly, with the operator's log showing **3,940** quoted
PodSecurity refusals in one 139-second run — none of them an RBAC problem,
all of them sharing the API server's `is forbidden:` prefix with a genuine
denial. The narrowing is real and is a weakening, however narrow, of the one
check the whole E2E package exists to pass; it is scoped to one exact
substring, and a genuine RBAC denial still fails the check regardless of what
else is in the log line.

**That 3,940 was recorded here, and in Task 6's report, as the price of a
refused create. It was not. It was a hot loop, and the final whole-branch
review of this branch is what found it — not any of the seven task
reviews, which had the number in front of them and read it as a cost.**
`setProxyPodsBlocked` stored the API server's verbatim refusal, which names
the pod it refused; `NewProxyName` draws a fresh random suffix per attempt,
so the message differed on every pass; `writeStatus` therefore always wrote
and always bumped `resourceVersion`; and `For(&spawneryv1alpha1.ProxyGroup{})`
carries no predicate, so the update event re-enqueued the group immediately,
ahead of the rate-limited retry. Roughly 28 reconciles a second, one create
attempt and one status update each, for as long as the refusal stood — which
for a Pod Security label is forever. Before 6c this path returned the error
with no status write at all, so nothing re-triggered the watch; 6c introduced
the write and, with it, the loop.

The fix is in `setProxyPodsBlocked`: a `Degraded` condition already saying
the same thing is left byte-for-byte alone, where "the same thing" means the
same `Status`, the same `Reason`, and a message that differs at most in the
name of the object the API server refused (`sameRefusal`). Nothing edits the
stored text — it stays the cluster's own words, verbatim, because the remedy
is in them and nothing else knows it — and a genuinely different refusal
under the same reason still replaces it. `TestARefusedProxyPodStopsRewritingTheGroup`
(`internal/controller/expose_test.go`) reconciles a refused group four times
and asserts `resourceVersion` stops changing; without the guard it reports
`[230 231 232 233]`. **`make e2e` was re-run on the fix: green, eighteen
scenarios, 142.8s, and the operator's log for the whole run holds 15 lines
containing `is forbidden:` — all fifteen of them `gateway-forbidden`'s, with
fifteen distinct pod names — against the 3,940 the same count produced
before.** Fifteen is what exponential backoff alone predicts. 6d should read
the old number wherever it still appears in
`.superpowers/sdd/2026-08-18-expose-strategies/task-6-report.md` as the
measurement of a defect, not of a cost.

**What follows for anything 6d writes or claims:** no image in
`test/e2e/manifests/e2e.yaml` resolves, so no container process runs in any
of the four new scenarios and nothing listens on 25565 in a game namespace;
kindnet enforces no `NetworkPolicy`, unchanged since 6b. Nothing in 6c
demonstrates that a client can reach a proxy through any of the three
strategies. The honest verb is that the operator *publishes* an address or
*deletes* an object, not that anything *reaches* or *works* through one.

## 3. What 6d finds in place

**`spawnery-system` is still hard-wired in three places, unchanged by
6c.** `config/deploy/networkpolicy.yaml:14` hard-codes
`namespace: spawnery-system`; the `+kubebuilder:rbac` marker for the TLS
Secret (`internal/certs/store.go:57`) and the one for the leases
(`internal/controller/setup.go:72`) both carry `namespace=spawnery-system` as
a literal. 6a's and 6b's handovers both call this the single most likely way
the chart ships something that works on the author's machine and nowhere
else; 6c neither added a new instance of the pattern nor closed any of the
three existing ones, because none of its work touched the operator's own
namespace — the Helm chart's templating of all three is 6d's to do.

**`config/samples/network.yaml` now shows all three strategies.** The active
`ProxyGroup` still uses `NodePort`; `LoadBalancer` and `HostPort` are added as
commented alternatives immediately below it, each with the cost it carries
stated in the comment (no bare-metal load balancer controller by default for
`LoadBalancer`; the node-count cap and the Pod Security refusal for
`HostPort`) rather than only what the strategy is. A reasonable starting
point for whoever writes the chart's default `values.yaml`.

**The refusal path is a guard for a value the CRD cannot produce today, not
dead code to delete.** `exposeImplemented` (`internal/controller/proxygroup_controller.go:1670`)
and the guard that calls it (`:229`) are unreachable while the CRD's enum
and `exposeImplemented` agree. What closes off a fourth value is
`ExposeType`'s own `+kubebuilder:validation:Enum=LoadBalancer;NodePort;HostPort`
marker (`api/v1alpha1/proxygroup_types.go:27`), rendered into the generated
CRD as a plain OpenAPI `enum:` field
(`config/crd/bases/spawnery.cloud_proxygroups.yaml:174-177`) and enforced by
structural-schema validation — not the five `XValidation` (CEL) rules at
`api/v1alpha1/proxygroup_types.go:72-76`, which do a different job entirely: they require the sub-block
matching whichever type *is* chosen (`nodePort`/`hostPort`/`loadBalancer`),
and say nothing about a fourth type value. Any object with an unrecognised
`expose.type` is rejected by the API server before this reconciler ever sees
it. The branch, and `refuse`'s
shared tail (`:355`), stay in place for the day a fourth value is added to
the enum without a branch to serve it; `docs/known-issues.md`'s "From
milestone 4c-3" section carried this in the present tense in two places and
now restates it this way instead (see §7 below for where).

**One doc comment 6c's own review deferred rather than fixed.**
`ProxyGroupReconciler.Recorder`'s own field comment (`:148-151`) still says
it "announces two things" — it already undercounted `NodeDraining` before
this milestone, and it now also omits the `ProxyPodBlocked`/`ProxyPodsAdmitted`
pair the type doc comment above it (`:94-125`) was corrected to include
during the Task 5 fix round. Nobody has yet gone back and fixed the field
comment to match.

## 4. What the RKE2 rollout now owes

6a's §6 and 6b's §4 stand unchanged in what they listed: all three images
pullable from `ghcr.io/spawnery/` without a pull secret, the operator running
from a digest, the production `--startup-deadline`, CIS `restricted` pod
security, `HostPort` under a real CNI, a reachable `LoadBalancer` address,
several nodes and therefore node drain and a PodDisruptionBudget under a real
eviction, a real join, and everything 6b's §4 lists about whether either
`NetworkPolicy` refuses anything. 6c adds one new, specific conflict inside
that list, not a new item beside it:

**`HostPort` and CIS `restricted` cannot both hold in one namespace, and the
rollout is currently promised both.** The design's §10
(`docs/superpowers/specs/2026-08-18-expose-strategies-design.md`) names it,
and `docs/known-issues.md`'s new "From milestone 6c" section is where it now
lives as a permanent record. Pod Security `baseline` — which `restricted`
inherits — disallows a container `hostPort` outright, which is exactly what
§2's enforced refusal measured. So a namespace enforcing either policy
refuses every `HostPort` pod's create; the rollout's promise of CIS
`restricted` everywhere and `HostPort` under the real CNI cannot both be kept
in the same namespace. 6c makes the refusal legible on the object; it cannot
make the two requirements compatible, and nothing in the code should try to.
**The remedy is the runbook's to take, not the code's:** give the namespace
running the `HostPort` `ProxyGroup` a relaxed Pod Security label (or a
namespace of its own), or drop the `HostPort` leg of the rollout and expose
only through `NodePort` or `LoadBalancer` where CIS `restricted` applies
everywhere.

## 5. Every finding this milestone's reviews produced

The SDD ledger
(`.superpowers/sdd/2026-08-18-expose-strategies/progress.md`) is the only
place this list exists in full; it is restated here with what caught each
one. The seven below came from the seven task reviews. The final
whole-branch review, which ran after all of them and is the last gate before
merge, produced eight more; they are the second list, and the fact that the
worst defect in this milestone is in the second list and not the first is
itself the finding worth carrying to 6d.

1. **Task 2, minor, deferred.** `proxyAddress`'s `svc == nil` guard in the
   `LoadBalancer` branch has no test and is unreachable as wired:
   `reconcileService` never returns `nil` for `LoadBalancer` without also
   returning an error, which short-circuits `Reconcile` before `setStatus`
   runs. Caught by review reading.
2. **Task 5, minor, deferred.** The event's Kubernetes `Reason` field is the
   generic literal `"ProxyPodBlocked"` for both `ReasonProxyPodRejected` and
   `ReasonProxyPodUnschedulable`; `ProxyGroupReconciler` uses the condition's
   own `Reason`, so `kubectl get events` alone cannot tell a rejected create
   from an unplaceable pod. Caught by review reading.
3. **Task 5, Important, fixed in round 1 (`491fbc5`).**
   `reportBlockedProxies`'s recovery write (`Degraded=False`/`ReasonProxyPodsAdmitted`)
   had no transition check and never fired an event, unlike every other
   condition pair this reconciler reports on the flank in both directions;
   the type doc comment also still said "three occasions." Fixed by gating
   the event on `wasBlocked`, correcting the doc comment to four occasions,
   and adding `TestARecoveredProxyGroupFiresAnEventOnlyOnTheFlank`, whose own
   two mutations (delete the event; fire it unconditionally) both went red.
   Caught by review reading.
4. **Task 5, minor, deferred.** `Recorder`'s own field doc comment
   (`:148-151`) says it announces two things; already undercounted
   `NodeDraining` before this milestone, now also omits the blocked/admitted
   pair (§3 above). Caught by review reading.
5. **Task 6, minor, deferred.** Commit `9e4b7a7`'s body writes
   "ProxyPodPodUnschedulable" (doubled "Pod") where it means
   `ReasonProxyPodUnschedulable`. Caught by review reading.
6. **Task 6, Important, fix round 1 dispatched (`47219c3`), confirmed still
   open.** `aForbiddenHostPortIsReportedOnTheGroup`'s pod-count assertion
   (`test/e2e/expose_test.go:224`) had never been shown able to fail on its
   own: it is unreachable whenever the scenario's first `eventually()` times
   out, since `t.Fatalf` unwinds the subtest via `runtime.Goexit` before the
   pod-count code runs, and the run that first exercised the relevant
   mutation happened to time out for exactly that reason. The fix round
   removed a real confounder (the port collision named in §2) and re-ran the
   mutation: the assertion still never executes, but now for a different,
   confirmed reason — there is no path in this scenario's design where the
   condition assertion fails *and* execution continues to the pod-count
   check. This is a genuine gap, not a false alarm, and it is not closed:
   closing it needs a mutation that makes the operator's condition-setting
   logic lie about a pod's existence without the pod actually stopping
   existing, which only an envtest case can induce, not a manifest or a
   shell-script mutation against a real cluster. Caught by mutation.
7. **Task 1, not deferred — closed within the same task, before commit.**
   The brief's own prescribed Mutation 1 (dropping the `Type ==
   ExposeHostPort` half of the guard in `renderProxyPod`) produced **no
   failure at all** against the original three-case table: every case with
   `Type != HostPort` also happened to have `HostPort == nil`, so the
   mutation was invisible to all three. A fourth subtest, "NodePort with a
   stray HostPort sub-block," was added, shown to fail under the mutation,
   and kept. Caught by mutation, during the task's own required
   mutation-testing step.

**Two of the seven — Task 1's and Task 6's — are the shape
`docs/known-issues.md` has tracked since milestone 5: a test assertion that
could not fail, or could not fail for the reason it claimed to, found by
mutating the code under test rather than by reading the test.**
`docs/known-issues.md`'s "From milestone 6b" section names 6b the seventh
milestone in a row where that specific shape of defect turned up, at six
instances, every one a mutation-only catch. 6c continues the streak at a
count of two. The other five findings above are a different shape and were
not mutation catches: an unreachable production-code guard (1), an event's
`Reason` field carrying less detail than the condition it mirrors (2), a
missing recovery event on a condition pair plus the doc comment that didn't
mention it (3), a second doc comment left stale by the same change (4), and a
doubled word in a commit message body (5) — none of them a claim a test makes
and fails to check, so mutation was never going to find any of them; reading
the diff against the pattern the surrounding code already set did. 6c's
finding set is therefore a mix in a way 6b's plan-test-code defects were not
— 6b's six were uniformly mutation-only — and the two that repeat that exact
shape, Task 1's and Task 6's, are the ones worth carrying forward as what to
watch for.

### The final whole-branch review

Eight findings and two suggestions, against the eleven commits as a whole
rather than task by task. Every one is fixed in this branch; the fix commits
are `07fa157`, `77be254`, `e00154f` and the docs commit that carries this
paragraph.

1. **Critical — the refused-create path was a reconcile hot loop.** §2 above
   tells it in full. Found by an envtest probe (three failing `Reconcile`
   calls, resourceVersions 230 → 231 → 232, three distinct pod names) and by
   re-reading this branch's own `task-6-report.md`: 3,940 refusals in 139
   seconds where backoff predicts about fifteen. Caught by reading a number
   the milestone had already measured and mis-explained.
2. **Important — the `LoadBalancer` address wiring was tested by nothing.**
   §2 above tells it in full. Caught by mutation: stripping the Service's
   status before `setStatus` left the whole package green.
3. **Important — a test comment claimed evidence that did not exist.**
   `test/e2e/expose_test.go`'s doc comment said the address-appears half was
   "proven in envtest, where a test can make a pod ready" while no such test
   existed; §2 of this handover then corrected that claim at length, and
   that correction is now itself rewritten, because the envtest exists.
   Caught by review reading, and only visible because finding 2 had already
   established there was nothing there.
4. **Minor — the `services: delete` ownership guard was half-tested and
   deviated from the design.** `TestSwitchingToHostPortLeavesAForeignServiceAlone`
   used a Service with no owner reference, so narrowing the guard to
   `owner == nil` left the suite green; and design §4 requires the guard to
   check `podspec.LabelManagedBy` as well as the controller reference, which
   the code did not, with no task report recording the deviation. Both
   halves are now checked and both are now tested. Caught by mutation.
5. **Minor — the milestone's one enforced observation was asserted loosely.**
   Both the envtest and the E2E asserted only that the refusal message
   contained `PodSecurity`, never `hostPort`; a proxy pod that acquired an
   unrelated `baseline` violation with the container host port dropped
   entirely would have kept both green with the strategy under test gone.
   Caught by review reading.
6. **Minor — `status.address` can advertise a Service that has been deleted.**
   Recorded rather than fixed; see `docs/known-issues.md`, "From milestone
   6c", for the exact scenario and why. Caught by review reading.
7. **Minor — the all-clear message asserted more than it checked.**
   `reportBlockedProxies` wrote "every proxy pod this group asked for exists"
   without comparing the pod count to `spec.Replicas`. The condition's
   semantics were right; the sentence was not, and it now says what the pass
   established. Caught by review reading.
8. **Minor — this handover miscounted its own record**, citing six task
   reports where there are seven. Fixed in §7. Caught by review reading.

Plus two suggestions, both taken: `internal/rbacaudit/required.go`'s `Why`
for `services: delete` named `reconcileService` where the `r.Delete` is in
`deleteServiceIfOurs`, and now names both; and design §7 said "the pod hash
differs across the three", contradicting design §2 and
`TestDesiredProxyHashSeparatesHostPortFromTheServiceStrategies`, which
deliberately make `NodePort` and `LoadBalancer` hash **identically**.

**What 6d should take from the split.** Two of the eight were mutation
catches (2 and 4), continuing the streak `docs/known-issues.md` has tracked
since milestone 5. The other six were reading, and the two that matter most
— the hot loop and the false evidence claim — were found by reading the
milestone's *own paperwork* against its own code: a number a task report had
recorded, and a comment a task had written. Neither is reachable by looking
at a single task's diff, which is exactly why neither of the seven task
reviews found them. A whole-branch pass over the branch's documents, not
only its code, is what this milestone says is worth keeping.

## 6. The environment

Unchanged from 6b's own §6; nothing in 6c touched the harness beyond adding
the `minecraft-baseline` namespace `hack/e2e.sh` creates (§1). Every command
runs inside `nix develop`.

```bash
nix develop -c make test
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

- `make e2e` is part of neither `make test` nor `make all`, deliberately.
- `kind` runs under rootless Podman here, which needs both
  `KIND_EXPERIMENTAL_PROVIDER=podman` and a systemd scope with `Delegate=yes`;
  `systemd-run --user` in turn needs `XDG_RUNTIME_DIR` and
  `DBUS_SESSION_BUS_ADDRESS`, which an interactive login shell has and a
  detached one does not.
- `TMPDIR` matters: the default `/tmp` is too small for an image archive
  here.
- The machine has 8 GB and no swap. Run one cluster at a time; `E2E_KEEP=1`
  leaves it standing and prints its `KUBECONFIG`.
- Every image derivation takes the working tree as its source, so editing a
  file under `docs/` changes the operator image's derivation hash and makes
  the next `make e2e` rebuild it — slow, not wrong.

## 7. Where everything lives

- Design:
  [`docs/superpowers/specs/2026-08-18-expose-strategies-design.md`](superpowers/specs/2026-08-18-expose-strategies-design.md).
- Open points: [`docs/known-issues.md`](known-issues.md), "From milestone
  6c", plus the two corrected entries under "From milestone 4c-3" that used
  to describe the removed refusal in the present tense.
- The strategies: `internal/podspec/proxy.go` (the container's `hostPort`),
  `internal/controller/proxygroup_controller.go` (`reconcileService`,
  `proxyAddress`, `applyExposeAnnotations`, `exposeImplemented`,
  `setProxyPodsBlocked`, `reportBlockedProxies`).
- The sample: [`config/samples/network.yaml`](../config/samples/network.yaml).
- The tests: `internal/controller/expose_test.go`, `test/e2e/expose_test.go`.
- The SDD record of how this milestone was built, task by task, including
  every mutation run and its verbatim output:
  [`.superpowers/sdd/2026-08-18-expose-strategies/`](../.superpowers/sdd/2026-08-18-expose-strategies/)
  (`task-1-report.md` through `task-7-report.md` — there are seven, not six —
  and `progress.md`, the ledger §5 restates).
- 6b's record, and what 6c started from:
  [`handover-milestone-6b.md`](handover-milestone-6b.md).
