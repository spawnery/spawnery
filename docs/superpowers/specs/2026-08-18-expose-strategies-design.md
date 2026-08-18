# Milestone 6c — the LoadBalancer and HostPort expose strategies

## 1. What this milestone is

`ProxyGroup.spec.expose` has carried three strategies since milestone 1. One
of them works. `ProxyGroupReconciler` refuses the other two outright, with
`ReasonExposeNotImplemented` and a message naming milestone 6, and that
refusal was the honest answer at the time: a branch written then would have
reached this milestone having never run.

6c writes the two branches and runs them.

It also does one thing the expose work drags in and nothing else would: it
makes a proxy pod that cannot come into existence say so on its group.
`HostPort` is forbidden by Pod Security `baseline`, and the most likely
`HostPort` mistake a user can make — more replicas than nodes — leaves a pod
`Pending` forever. Today both are silent on the object and visible only in
the operator's log.

**What it does not do, stated here because the milestone before this one was
mostly a lesson in the difference:** nothing in 6c demonstrates that a client
can reach a proxy. No `LoadBalancer` controller runs anywhere in this
repository, no image in the E2E manifest resolves, and no container process
ever listens. 6c ships objects and one genuinely enforced refusal. Reaching
a proxy from outside belongs to the RKE2 rollout at the end of milestone 6,
and no test name, comment or commit message in 6c may claim otherwise.

## 2. What does not change

**The API.** No new field, no CRD regeneration. `LoadBalancerSpec` and
`HostPortSpec` already exist (`api/v1alpha1/proxygroup_types.go:41-91`),
along with the five CEL rules that require the matching sub-block and forbid
the other two, and the envtest table that exercises them
(`api/v1alpha1/proxygroup_envtest_test.go:44`). 6c fills in what was
described.

**The rollout.** `BuildProxyPod` already receives the whole `ProxyGroup`, so
adding a container `hostPort` changes `DesiredProxyHash` with no further
work. A switch **into or out of** `HostPort` therefore makes every live pod
stale, and the drain-aware rollout that milestone 4 built replaces them one
at a time without disconnecting anyone. This falls out of the shape already
there; it is written down because a reader will otherwise look for the code
that does it.

A switch between `NodePort` and `LoadBalancer` rolls nothing, and should not:
those two differ only in the Service, and the pods behind it are identical.
The pod hash is the right place for this distinction to live precisely
because it answers the question by construction rather than by a rule
somebody has to maintain.

**The NetworkPolicy.** Nothing 6b writes selects a proxy pod: the per-Network
policy's `podSelector` carries `spawnery.cloud/role=server`, and
`config/deploy/networkpolicy.yaml` selects the operator alone. So 6c changes
how a proxy is reached without touching a policy. 6b's handover §3 records
the trade that keeps it that way — a proxy's readiness is a `TCPSocket` from
the kubelet, which a policy might govern, so selecting proxies would put the
fleet's readiness at the mercy of a CNI's treatment of kubelet traffic.
`HostPort` sharpens that rather than weakening it: traffic arriving on a
node's own address has no pod peer for a selector to name.

## 3. The dispatch

`Reconcile`'s blanket refusal
(`internal/controller/proxygroup_controller.go:214`) becomes a `switch` on
`group.Spec.Expose.Type` with three arms and a `default`.

The `default` still refuses with `ReasonExposeNotImplemented`, with a message
that no longer names milestone 6. It is unreachable while the enum holds
three values the switch all handles. It is kept so that a fourth value added
to the enum without a branch produces a refusal on the object rather than a
nil dereference in `reconcileService` — the CEL rules guarantee the sub-block
matching a *known* type is present, and guarantee nothing about a type the
controller has never heard of.

Dropping the branch drops one of the three callers of `refuse`, which is
where `r.Divergence.forget` lives (`:314`). See §8.

## 4. The Service

`reconcileService` becomes strategy-aware and returns the Service it settled
on, so `setStatus` can read `status.loadBalancer` from it without a second
Get.

**NodePort** is what is there today, unchanged: type `NodePort`,
`externalTrafficPolicy: Local`, one port named `minecraft` on 25565 targeting
the named container port, `nodePort` from the spec. The existing comment
explaining why `Local` rather than the `Cluster` default — the client IP is
what bans and rate limits are built on — applies to both Service-backed
strategies and stays.

**LoadBalancer** is type `LoadBalancer`, `externalTrafficPolicy` from
`spec.expose.loadBalancer.externalTrafficPolicy` (the CRD defaults it to
`Local`, for the reason above), the same single port, and **no** explicit
`nodePort` — the API server allocates one, and a `LoadBalancer` Service has
one whether anybody names it. The selector is the same
`podspec.ProxyLabels(...)` the NodePort branch uses, pinning the role as well
as the group name.

**HostPort** gets no Service. Nothing inside the cluster dials a proxy:
players arrive from outside, agents dial the operator, and Velocity dials
backends. If a Service under the group's name exists, is controlled by this
group, and carries `podspec.LabelManagedBy`, it is deleted; anything else is
left alone. Without the delete, switching to `HostPort` leaves an object that
still holds its node port and still selects the same pods, so the group stays
reachable by the route the switch was meant to end.

That delete is the one new permission in this milestone.

### 4.1 The annotations

`LoadBalancerSpec.Annotations` is the only place where a user writes into an
object a third-party controller also writes into: MetalLB and kube-vip both
annotate the Service they act on. So the operator cannot treat the spec's map
as the whole truth and delete what is not in it.

The operator records the keys it set under `spawnery.cloud/expose-annotations`
on the Service, as a comma-separated list. Each pass it writes the spec's
keys, then deletes any key named in the previous list that the spec no longer
names, then rewrites the list. Keys nobody claims are never touched.

The alternative — set, never delete — costs less and fails silently: a user
who removes a pool annotation from the spec sees nothing happen, permanently,
with no message anywhere.

## 5. The address

`proxyAddress` gains the strategy. All three branches keep the property the
function's existing doc comment claims, and that property is the reason the
`LoadBalancer` branch is shaped the way it is:

- **NodePort** — `hostIP:nodePort` of a ready pod. Unchanged.
- **HostPort** — `hostIP:hostPort` of a ready pod. The same rule with a
  different port.
- **LoadBalancer** — the first entry of `service.status.loadBalancer.ingress`
  (`ip` when set, else `hostname`) with `podspec.MinecraftPort`, which is the
  Service's own `port` rather than any node port, **and only while at least
  one proxy is ready.**

The readiness gate on the third branch is the part worth arguing. The address
there comes from the Service, which knows nothing about whether anything is
serving, so without the gate `status.address` would start pointing somewhere
the moment a load balancer answered — including for a group whose every pod
is in `ImagePullBackOff`. The other two branches publish an address only for
a node that demonstrably runs a ready proxy; keeping that invariant across
all three is worth more than reporting an assigned address a few seconds
earlier. Empty is the truthful answer while there is nowhere to connect.

The operator still reads no `Node` object for any of this. `hostIP` is on the
pod, which is already watched, and the ingress address is on the Service the
operator owns.

## 6. When a proxy pod cannot exist

Two different failures with the same visible shape — the group asks for a pod
and never gets one — and today neither reaches the object:

- **The API server refuses the create.** A `HostPort` group in a namespace
  enforcing Pod Security `baseline` or `restricted` is refused with
  `violates PodSecurity ...: hostPort`. `reconcileReplicas` returns the error,
  controller-runtime logs and requeues it, and the group reports `Pending`
  with no reason.
- **The scheduler cannot place the pod.** With `hostPort` at most one pod of
  the group fits per node, so `replicas` is silently capped by the node count.
  The surplus pod exists and stays `Pending` with `PodScheduled=False` and a
  scheduler message naming the port conflict.

6c reports both on the group's `Degraded` condition, with two reasons because
the remedies differ — this project's own rule, stated where the five
`ForwardingSecretResolved` reasons are defined. The message is the cluster's
own text in both cases; nothing is invented, and nothing is paraphrased.

The unschedulable half needs no `Node` read: `PodScheduled=False` and its
message are on the pod, which is already in the reconcile's pod list.

Both are equally not a prediction. The operator does not count nodes and
refuse a `HostPort` group whose `replicas` exceed them: doing that would mean
reimplementing the scheduler's view of node selectors, taints and foreign
`hostPort` holders in order to guess ahead of it, and being wrong the moment
a node joins.

**This is the first writer of `Degraded` on a `ProxyGroup`.** `setStatus`
already reads the condition and routes it to phase `Degraded`
(`internal/controller/proxygroup_controller.go:1355`); only
`ServerGroupReconciler` has ever set it.

## 7. What is proven, and where

**`internal/podspec` (unit).** `hostPort` appears on the container port for
exactly one strategy, and the pod hash differs across the three. The mutation
that has to fail: setting `hostPort` on the `NodePort` path.

**`internal/controller` (unit and envtest).** The three Service shapes. The
delete branch, including its refusal to touch a Service this group does not
control. The annotation bookkeeping, including removal. `proxyAddress` in all
three branches. Both `Degraded` reasons.

envtest is where the `LoadBalancer` address is proven end to end, and it can
do it because it runs no kubelet: the test writes pod status itself, marks
pods ready, patches the Service's status subresource with an ingress entry,
and observes `status.address`. Nothing there pretends a load balancer
controller ran.

**`test/e2e` (four scenarios).** The seam is unchanged: a
`func theXxx(t *testing.T)` per scenario plus a line in
`TestSpawneryUnderItsOwnServiceAccount`'s explicit ordered `t.Run` list, with
`theOperatorWasNeverDenied` staying last.

1. **`theLoadBalancerGroupGetsItsService`** — a `gateway-lb` group. Asserts
   the Service's type, `externalTrafficPolicy` and the spec's annotations;
   then patches its status with an ingress address and asserts
   `status.address` stays **empty**, because no proxy is ready. In the E2E
   the patch proves the gate of §5, not the address. The scenario's name says
   Service for that reason.
2. **`theHostPortGroupBindsThePortAndHasNoService`** — a `gateway-host`
   group with `replicas: 2`. The pods carry `hostPort: 25565`; no Service
   named `gateway-host` exists; and because a single-node cluster fits one of
   them, the group reports `Degraded` with the scheduler's own message.
3. **`aSwitchToHostPortRemovesTheService`** — `gateway-switch`, a group that
   starts as `NodePort`, is patched to `HostPort`; the Service disappears and
   the pods roll. It is a group of its own rather than a mutation of
   `gateway`, because the scenario list is ordered and every later scenario
   would inherit the change. This is the only place `services: delete` is
   exercised under the operator's real ServiceAccount.
4. **`aForbiddenHostPortIsReportedOnTheGroup`** — a second namespace labelled
   `pod-security.kubernetes.io/enforce=baseline`, with its own forwarding
   Secret, its own `Network`, and the `forwarding-secret-reader` grant
   `hack/e2e.sh` already applies to `minecraft`. The `HostPort` group there
   never gets a pod, and the group carries the API server's own message.

Scenario 4 is the only refusal this repository has ever observed being
enforced. It is enforced by the API server, not by a CNI, which is exactly
why it can be observed here at all.

The existing `minecraft` namespace cannot carry the `baseline` label instead:
scenario 2 needs `hostPort` to be *allowed* somewhere, and the two scenarios
would cancel each other out in one namespace.

### 7.1 The collision with `theOperatorWasNeverDenied`

Scenario 4 deliberately causes a denial, and the API server phrases it
`pods "..." is forbidden: violates PodSecurity "baseline:latest": hostPort`.
`theOperatorWasNeverDenied` scans the operator's log for the substring
`is forbidden:` (`test/e2e/e2e_test.go:207`), so scenario 4 would turn the
last scenario of the run red.

The exclusion must be exactly one string: a line also containing
`violates PodSecurity` does not count; every other `is forbidden:` line still
does. A Pod Security rejection is not an RBAC denial, and telling the two
apart is the whole point of the scenario that would otherwise break.

This is recorded here rather than discovered during implementation because
the failure it produces — the run's final and most important scenario failing
for a reason another scenario caused — is the kind that gets misdiagnosed.

## 8. What the removed refusal takes with it

Four places name it, and all four become false the moment the branch goes:

- `internal/controller/readinessdivergence.go:44` — its doc comment names
  `NetworkNotFound`, `NetworkNotAccepted` and `ExposeNotImplemented` as the
  three steady-state early returns that reach `forget`. Two remain. (The
  `forget` itself lives once, in `refuse` at
  `internal/controller/proxygroup_controller.go:314`; the comment counts
  callers, not calls.)
- `internal/controller/proxygroup_controller_test.go:294` asserts that a
  `LoadBalancer` group is refused.
- `docs/known-issues.md` says it twice, in the present tense, at the entries
  around lines 1485 and 1605.
- `README.md`'s roadmap paragraph.

## 9. The rest of the surface

- `+kubebuilder:rbac` gains `delete` on `services`
  (`internal/controller/proxygroup_controller.go:162`), `make manifests`
  regenerates `config/rbac/role.yaml`, and
  `internal/rbacaudit/required.go` gains the matching row with a `Why` naming
  `reconcileService`'s delete branch. That table goes red when marker and
  table disagree in either direction, so the two cannot drift apart quietly.
- `config/samples/network.yaml` gains the other two strategies as commented
  alternatives beside the NodePort group it already carries.
- `test/e2e/manifests/e2e.yaml` gains four `ProxyGroup`s — `gateway-lb`,
  `gateway-host` and `gateway-switch` in `minecraft`, and one `HostPort` group
  in the second namespace — plus that namespace with its own forwarding Secret
  and `Network`. Every image there stays deliberately unresolvable, and the
  existing `gateway` group is left exactly as it is: `theProxyGroupGetsItsService`
  (`test/e2e/persistence_test.go:115`) is the one scenario that reads it, and it
  is the NodePort regression this milestone must not disturb.
- `hack/e2e.sh` applies the `forwarding-secret-reader` grant to the second
  namespace as well.
- `docs/handover-milestone-6c.md`, the cold-start entry point for 6d, in the
  form of its two predecessors.

## 10. What this hands the RKE2 rollout

Everything about reachability, as §1 says. Plus one finding that is new, and
that nothing in the milestone's paperwork has recorded until now:

**`HostPort` and CIS `restricted` are mutually exclusive in one namespace,
and the rollout is currently promised both.** 6a's handover §6 lists CIS
`restricted` pod security and `HostPort` under the cluster's actual CNI among
what the rollout owes. Pod Security `baseline` — which `restricted` inherits
— disallows host ports outright, so a namespace enforcing either will refuse
every `HostPort` pod. The runbook has to use a namespace with a relaxed
policy for the `HostPort` leg or drop one of the two requirements. 6c makes
that refusal legible; it cannot make it go away, and neither can anything
else.

## 11. Facts this design asserts about the code already here

Each is a claim an implementer can check before trusting anything above.

1. `internal/controller/proxygroup_controller.go:214` refuses any
   `expose.type` other than `NodePort` with `ReasonExposeNotImplemented`,
   and returns through `refuse`, which requeues.
2. `refuse` calls `r.Divergence.forget` once, at `:314`.
3. `reconcileService` (`:1200`) builds a `NodePort` Service unconditionally,
   with `ExternalTrafficPolicy: Local` and the group's `nodePort` at `:1228`.
4. `setStatus` (`:1351`) dereferences `group.Spec.Expose.NodePort`
   unconditionally when deriving `status.address`.
5. `proxyAddress` (`:1376`) takes a plain `int32` port and needs no signature
   change beyond the strategy; the unconditional dereference is at its call
   site, not inside it.
6. The `services` RBAC marker at `:162` grants
   `get;list;watch;create;update` and no `delete`;
   `internal/rbacaudit/required.go:142-146` mirrors exactly those five.
7. Nothing writes `ConditionDegraded` on a `ProxyGroup`; `:1355` only reads
   it.
8. `BuildProxyPod` (`internal/podspec/proxy.go:76`) receives the
   `*ProxyGroup`, and the container's ports are built at `:212`.
   `DesiredProxyHash` renders through the same `renderProxyPod`.
9. No Go file outside the generated deepcopy sets a container `hostPort`
   anywhere.
10. `test/e2e/e2e_test.go:104-117` lists fourteen scenarios in an explicit
    ordered `t.Run` list, `theOperatorWasNeverDenied` last; its matcher is at
    `:207`.
11. `hack/e2e.sh` creates the `minecraft` namespace and applies
    `config/rbac/forwarding-secret-reader.yaml` into it before the manifest
    is applied.
12. `test/e2e/manifests/e2e.yaml` declares one `ProxyGroup`, `gateway`, with
    `expose.type: NodePort` and `nodePort.port: 30765`.

## 12. Acceptance

1. `make test` green, including the race detector 6b turned on.
2. `make e2e` green with eighteen scenarios, `theOperatorWasNeverDenied`
   still last and still passing.
3. A `LoadBalancer` group produces a `LoadBalancer` Service carrying the
   spec's annotations and external traffic policy, and publishes an assigned
   ingress address once a proxy is ready — the second half proven in envtest.
4. A `HostPort` group produces pods binding the port and no Service, and a
   group switched to `HostPort` loses the Service it had.
5. A `HostPort` group in a `baseline` namespace reports the API server's
   refusal on its own `Degraded` condition, observed in a real cluster.
6. `internal/rbacaudit` agrees with the markers, and the markers grant
   nothing beyond what a named call site uses.
7. No test name, comment, commit message or document claims that any client
   reached any proxy.
