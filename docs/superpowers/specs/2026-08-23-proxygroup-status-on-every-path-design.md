# A ProxyGroup's status describes what was observed, on every path that observed it

## 1. What this is

`docs/known-issues.md`, under "From milestone 6c", records that
`status.address` can go on advertising a Service that has been deleted:

> A `NodePort` group publishing `10.0.0.7:30765` is switched to `HostPort` in
> a namespace enforcing Pod Security `baseline`. `reconcileService` deletes
> the Service; `reconcileReplicas` is then refused by the API server, so
> `Reconcile` returns before `setStatus`. `status.address` keeps naming the
> node port of a Service that no longer exists — and because the create can
> never succeed while the namespace's label stands, no later pass corrects
> it.

That entry named the remedy and deferred it: "recompute the status on every
return path rather than only the successful one — and that is a change to the
shape of `Reconcile`, not a patch to one branch of it. It is 6d's or later,
and it should be taken as a whole or not at all." 6d and 6e did not take it.
This design takes it.

**It is two changes, and the order matters.** Recomputing the address on more
paths is only safe once the address cannot be fabricated, and reading the code
for this design found that it can be — today, with no error path involved.

## 2. The second defect, which is the one that has to be fixed first

`proxyAddress` (`internal/controller/proxygroup_controller.go:1745`) mixes an
observation with a piece of spec. It scans the group's pods for the first that
is `Ready` and carries a `HostIP`, and then switches on
`group.Spec.Expose.Type` to decide what to append to that host.

Nothing ties the pod it found to the strategy it then reads. In the entry's own
scenario the spec already says `HostPort` while the still-running pods are the
`NodePort` generation, so the function takes an old pod's `HostIP`, appends
`spec.expose.hostPort.port`, and publishes `hostIP:25565` — an address whose
host is real, whose port is real, and which in combination no pod in existence
binds. The known-issues entry predicted this as the cost of one candidate fix.
It is not a cost of the fix; it is already the behaviour, and the fix merely
makes it reachable more often.

The pod-side fact that closes it is already in the tree.
`internal/podspec/proxy.go:227-229` sets the container's `HostPort` **only**
when `group.Spec.Expose.Type` is `HostPort`. A pod created under any other
strategy carries `HostPort == 0`. So "is this ready pod one that actually binds
the port I am about to publish" is answerable from the pod, exactly, without a
generation counter or a label.

## 3. Scope

**`ProxyGroup` only.** `ServerGroupReconciler.Reconcile` has the same
success-path-only status shape, but no address: a count that lags by one
reconcile and an address that names a port nobody serves are different kinds of
wrong, and only the second sends a person to a dead socket. Widening this to
every reconciler's status is a larger change with a much weaker case, and it is
not made here.

**`writeStatus` is not touched.** It is a bare `Status().Update` with no
conflict retry (`:1848`). This design does not change how often it is called
per pass in the ordinary case, so the conflict rate is unchanged, and adding
retry machinery on the way past would be scope creep. A conflict requeues.

**No CRD change.** No new condition, no new field. `Degraded` already carries
the reason a group has no address, and §4 is the reason nothing more is needed.

## 4. What `status.address` means

**An address is published only when the current expose strategy is observably
realised. Otherwise the field is empty, and empty means "I have none".**

A wrong address is worse than no address: it sends a player to a socket that
refuses them, and it does so with the operator's authority behind it. The
alternative considered was keeping the last known address beside a staleness
condition, which is more information but costs a new concept in the CRD and a
second place to be wrong; and `Degraded` already stands next to the empty field
saying why.

The cost is real and is accepted: during a strategy change, or any window with
no ready pod, the field goes empty and anything polling it sees that. Empty is
the correct answer in those windows — there is no address that works.

**But not every return path recomputes.** The rule is that the address changes
only where the operator actually looked. `Reconcile` returns before reading any
pod or Service on four paths: the group is gone, the group is being deleted, or
one of the three `refuse()` cases — `Network` missing, `Network` not
`Accepted`, expose type not implemented. On none of those has anything about
the serving world changed: the pods are still running, the Service is still
there, and people are still connected through it. A deleted `Network` does not
make an address wrong. Clearing the address there would be the regression the
known-issues entry warns about in a different guise — blanking a working
address because of a different object.

The alternative was to read pods and Service on those paths too, for a rule
with no exception. It was rejected: it spends two API calls per failing
reconcile, on paths that retry, to re-derive an answer nothing has changed.

So the rule has two halves, and both belong in the code's own words:

1. Every computation of the address is honest — it publishes only what is
   observably realised.
2. Not every path computes. A path that did not look does not write.

## 5. The shape

`Reconcile` splits in two.

The outer function keeps everything up to and including the three `refuse()`
paths — the object fetch, the deletion check, the `Network` resolution, the
expose-implemented guard, and the existing status write that persists
`Accepted=True` before any side effect is attempted. Those paths return
directly, and the address is untouched, per §4.

Everything from `Bootstrap.Ensure` onward moves into an inner method that
returns what it observed:

```go
type proxyObservation struct {
    observed bool          // reconcileService and the first pods read both returned
    pods     []corev1.Pod
    svc      *corev1.Service
}
```

The outer function then finalises, once, regardless of how the inner one
returned:

```go
obs, res, err := r.reconcileObserved(ctx, network, group)
if obs.observed {
    r.setStatus(group, obs.pods, obs.svc)
    if werr := r.writeStatus(ctx, group); werr != nil {
        if err == nil {
            return res, werr
        }
        log.FromContext(ctx).Error(werr, "recording the group's status")
    }
}
return res, err
```

The inner method carries the result as well as the observation, so the
`RequeueAfter: resyncInterval` that the successful path returns today comes
back out of it unchanged. The finaliser above adds nothing to the result and
subtracts nothing from it; a status write is not a reason to requeue
differently.

**`observed` is a flag, not a nil check.** `svc == nil` is the legitimate
normal state for `HostPort`, which creates no Service at all, so nil-ness
cannot stand in for "did not look". The flag is set once `reconcileService`
and the first `r.pods` call have both returned without error.

The second `pods` read — the one after `reconcileReplicas`, which exists so the
status describes what is there rather than what was there — updates the
snapshot only if it succeeds. If it fails, the status describes the last
observation that did succeed, which is a weaker statement than the pass
intended to make and a much stronger one than none.

**Why a value and not a `defer`.** The point of the entry this design answers
is that the omission was invisible: a `return` added later silently skips the
status write, and nothing says so. A `defer` fixes that too, but the tail has
three ordering constraints — `reportBlockedProxies` before `setStatus` because
`setStatus` derives the phase from the `Degraded` condition, and
`protectOccupiedProxies` before it because the budget's selector has to find
the label on the same pass — and a `defer` either drags those inside itself or
leaves them where a later editor can reorder them. Making the observation a
value keeps the ordering where it is readable and makes "did we look" a thing
the compiler can see being passed around.

**A simplification falls out.** The `IsForbidden || IsInvalid` branch inside
`reconcileReplicas`'s error handling currently sets the condition, sets
`group.Status.Phase = "Degraded"` by hand, and writes the status itself. The
hand-set phase goes: `setStatus`'s own switch already derives `Degraded` from
the condition. The write goes: the outer finaliser does it. The branch keeps
the condition and the event, which are the things only it knows.

## 6. `proxyAddress`, grounded in observation

| strategy | today | after |
|---|---|---|
| `HostPort` | any ready pod's `HostIP` + `spec.expose.hostPort.port` | a ready pod **whose container declares that `hostPort`**, and its `HostIP` |
| `NodePort` | any ready pod's `HostIP` + `spec.expose.nodePort.port` | `svc != nil`, and the port from `svc.Spec.Ports[].NodePort` — what the API server actually allocated |
| `ClusterIP` | echoes `spec.expose.clusterIP.address` | the same, but only when `svc != nil` |
| `LoadBalancer` | `svc.Status.LoadBalancer.Ingress` | unchanged; it was already grounded |

The `NodePort` row changes the source of the port as well as adding a
condition. `reconcileService` writes `port.NodePort` from the spec (`:1320`),
and `NodePortSpec.Port` is required with `+kubebuilder:validation:Minimum=1`,
so the spec never asks for none and the API server never has to allocate one
on its behalf. The spec's port is only a request, though, and the Service is
the only place that says whether anything is still listening on it — a
deleted Service means there is nothing to dial whatever the spec asks for.
Reading the port back from the Service is therefore the honest value, and the
`svc != nil` condition is what makes the deleted case say so.

The ready-pod requirement stays for every strategy. It is what
`test/e2e/expose_test.go` already relies on — no image resolves in that
harness, so no pod is ready, so nothing is published — and that behaviour is
deliberate and must not regress.

## 7. How it is proven

- **`proxyAddress` as a unit test, four strategies plus the fabrication.** The
  fabrication case is the one that matters: spec says `HostPort`, the ready
  pods are the `NodePort` generation with `HostPort == 0` on their containers,
  expected `""`. **That test must fail before the change and pass after**, and
  the report must say so with the verbatim failure — it is the only evidence
  that the second defect was real rather than argued.
- **The entry's scenario in envtest**, beside
  `TestARejectedProxyPodIsReportedOnTheGroup`
  (`internal/controller/expose_test.go`): a `NodePort` group publishing an
  address, switched to `HostPort` in a namespace enforcing `baseline`, the
  create refused — assert the address is empty **and** `Degraded` is true. Both
  halves: an empty address with no explanation beside it is its own defect.
- **The counter-test, so the fix does not become the regression the entry
  warns about**: a group that stays `NodePort`, whose Service and ready pods
  are untouched, meeting an unrelated failure — the address stays.
- **The early paths**: delete the `Network` under a group with a published
  address and confirm the address is unchanged after the refusal.
- **Mutations**, one per test, each run against only the test it should break,
  with verbatim output recorded and reverted.

**One existing test will move.** `proxygroup_controller_test.go:318` expects
`hostIP:spec.expose.nodePort.port`. The value stays the same in that fixture —
`reconcileService` writes the spec's port into the Service — but the
expectation must now be read from the Service, or the comment beside it ("both
sides of the address are read back from where they were written, not hardcoded
twice") stops being true.

**What is not proven here.** No cluster is driven for this. `paulwtf` runs one
`ProxyGroup`, `expose.type: ClusterIP`, `Ready` with two replicas — the one
strategy whose row in §6 gains only a `svc != nil` guard, and the one whose
address is echoed rather than composed. The `HostPort` rows are envtest and
unit tests only, which is the same level milestone 6c reached for them and is
stated rather than dressed up.

## 8. Acceptance criteria

1. A group whose expose strategy is not observably realised publishes no
   address, on any path that reads pods and Service.
2. `proxyAddress` never composes an address from a pod that does not implement
   the strategy being published.
3. A path that returns before reading pods and Service leaves `status.address`
   as it was.
4. A refused proxy pod leaves the group `Degraded=True` with the API server's
   message, and an empty address.
5. A group meeting an unrelated failure, with its Service and ready pods
   intact, keeps its address.
6. `test/e2e/expose_test.go`'s assertion that nothing is published while no pod
   is ready still holds.
7. No CRD change, and `writeStatus` is unchanged.
