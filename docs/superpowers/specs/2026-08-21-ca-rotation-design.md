# Rotating the CA

## 1. What this is

`docs/known-issues.md` records, under "On the agent channel", that the CA has
no rotation procedure. Its first half is what remains open:

> today exactly one is ever written, and the overlap path itself does not
> exist. If the CA expires in ten years, or has to be replaced after a
> compromise, the only recipe is "delete the secret, restart all pods".

This design builds that path. The second half — that a new CA would not reach a
namespace where no pod is created — was closed by
`docs/superpowers/specs/2026-08-21-ca-bundle-distribution-design.md`, and this
design is the reason that one existed: an overlap window closes on the question
"does every agent have the new bundle yet?", and until a quiet namespace
followed the operator's bundle that question had no answer.

**What it does not do**, stated first. It does not rotate on its own: nothing
watches the CA's expiry, `CALifetime` stays at ten years, and no schedule is
added. A rotation is asked for by hand, with an annotation. It does not replace
the emergency recipe either — a compromised CA key is exactly the case where an
orderly overlap is the wrong move, because the overlap means continuing to
trust the compromised key for another quarter of an hour. Deleting the secret
and restarting the pods stays the answer to that, and `docs/known-issues.md`
keeps saying so.

What it adds is the ordinary case: replacing the CA on a running cluster
without a single agent losing its connection.

## 2. The guarantee this rests on, and how it was checked

The overlap window closes when every agent trusts the new CA. The mechanism
that makes that finite is not new, and it is not incidental:

**Every agent re-reads `ca.crt` from disk at least every
`--agent-session-deadline`.** `SessionDeadline`
(`proto/spawnery/agent/v1alpha1/agent.proto:47`) bounds the lifetime of one
authenticated stream: the agent opens a fresh stream after
`--agent-session-renew-after` (8 minutes by default), and the operator closes
it at `--agent-session-deadline` (10 minutes) regardless. `main.go:78` refuses
a configuration where the first is not below the second.

Renewal goes through `SessionLoop.scheduleRenewal` → `connect()`, and
`connect()` begins with `val channel = channels()` — the factory
`AgentPlugin` passes in, which is
`OperatorChannel.build(endpoint, Files.readAllBytes(env.caBundlePath))`
(`agent/paper/.../AgentPlugin.kt:53`, `agent/velocity/.../AgentPlugin.kt:191`).
So a channel — and a file read — happens per attempt.

This was checked rather than assumed, because it is the load-bearing fact and
the kind that quietly stops being true. Two comments in the agent already say
it must hold and name this rotation as the reason: `SessionLoop.kt:174-177`
("A channel is built per attempt … and AgentPlugin's factory memoises
nothing") and `Environment.Configured`, whose doc says paths rather than
contents are stored precisely so a bundle captured at `onEnable` cannot strand
the agent through a rotation.

The window is therefore **one observation plus one calculation**, not two
calculations:

| step | where the certainty comes from |
|---|---|
| the CA ConfigMap holds `old‖new` in every namespace with a `Network` | **observed** — the operator reads the ConfigMaps back |
| the kubelet projects it onto the pod's filesystem | calculated — the kubelet sync period, plus a margin |
| the agent re-reads the file | calculated — `--agent-session-deadline`, which this operator itself dictates |

`OperatorChannel.trustManager` parses the bundle with
`CertificateFactory.generateCertificates`, which reads every certificate in the
stream rather than the first, and loads each into the trust store. The agent
side needs no change at all.

## 3. The secret layout

`Bundle.parseCA` (`internal/certs/bundle.go:187`) decodes a single PEM block
and requires the key to match the certificate. `ca.crt` and `ca.key` are one
PEM each.

That rule does not change. **`ca.crt` and `ca.key` keep meaning "the CA that
signs the serving certificate right now"** — which is exactly what `parseCA`,
`Reissue`, `reissueOrIssue` and `Validate` already assume, so none of them is
touched. The second CA gets keys of its own in the same secret; the type
`kubernetes.io/tls` requires `tls.crt` and `tls.key` to be present and permits
anything else beside them, which is already how `ca.crt` and `ca.key` live
there today.

| phase | `ca.crt` / `ca.key` | `ca-next.*` | `ca-previous.*` | published bundle |
|---|---|---|---|---|
| at rest | the CA | — | — | `ca.crt` |
| `distributing` | the old CA | the new one | — | `ca.crt` ‖ `ca-next.crt` |
| `switched` | the new CA | — | the old one | `ca.crt` ‖ `ca-previous.crt` |
| after `drop-old` | the new CA | — | — | `ca.crt` |

The switch is one swap: `ca-next.*` becomes `ca.crt`/`ca.key`, the outgoing CA
moves to `ca-previous.*`, and `Reissue` signs a fresh serving certificate with
the code that is already there.

Only `Provider.CABundle` learns the concatenation. `snapshot.ca` is
`b.CACertPEM` today (`store.go:171`); it becomes the published bundle. The
`Bootstrapper` copies that value into the ConfigMap unchanged, so nothing
between the provider and the agent needs to know a rotation is happening.

**The outgoing key stays until `drop-old`, and that is the only thing that
justifies the hold.** While `ca-previous.key` is there, the operator can sign a
serving certificate with the old CA again. Without a way to actually do that,
keeping the key would be storing material nobody can use.

## 4. The sequence, and where its state lives

Everything is annotations on `spawnery-agent-tls`. No new CRD, and the state
survives an operator restart and a leader change because it lives where the
operator already reads and writes.

```
spawnery.cloud/rotate-ca               start | drop-old | rollback   ← written by a human
spawnery.cloud/ca-rotation-phase       distributing | switched       ← written by the operator
spawnery.cloud/ca-rotation-since       2026-08-21T14:02:11Z
spawnery.cloud/ca-rotation-blocked-on  minigames,creative
```

`start` issues a second CA into `ca-next.*` and republishes the bundle as
`ca.crt‖ca-next.crt`. The serving certificate is untouched and still chains to
the old CA. Once the gate of §5 passes and the window of §6 has elapsed, the
operator performs the swap, signs a new serving certificate, sets the phase to
`switched` — and stops there. The old CA is still trusted; nothing is broken;
the cluster can stay in this state indefinitely.

`drop-old` removes `ca-previous.*`, publishes `ca.crt` alone, and clears the
rotation annotations. This is the step that cannot be undone.

`rollback` abandons the rotation from wherever it stands. From `distributing`
it discards `ca-next.*`; from `switched` it signs a serving certificate with
`ca-previous.*` first and restores that pair to `ca.crt`/`ca.key`, discarding
the new CA. Either way it ends with `ca.crt` published alone, neither
`ca-next.*` nor `ca-previous.*` present, and the rotation annotations cleared.
**The rotation is abandoned, not paused**, and narrowing the bundle back to the
old CA is safe at any moment, because every agent trusted that one throughout.

> **Corrected while writing this design.** The option as it was put to the
> repository owner said `rollback` returns to `distributing`, "back in the
> overlap state, from which a second attempt is possible". That is wrong, and
> not by a little: `distributing` advances on its own once its window elapses,
> so returning there would re-perform the switch that was just undone, after
> the same wait, with nobody asking for it. A rollback has to land in a state
> that does not advance. Abandoning the rotation outright is that state, it
> needs no fourth phase, and retrying is `start` again — which costs the twelve
> minutes of distribution over, and nothing else.

The operator **removes** `rotate-ca` once it has acted on it, so a request is
consumed exactly once and a leftover `start` cannot fire twice. A value it does
not recognise it leaves in place and reports as a warning: clearing an
annotation you did not understand hides the typo that produced it. It stays a
warning, though — the operator steps over it and drives the phase as it would
on any other tick, because a value it cannot read is no reason to freeze a
rotation that is already past its gate, and the step it would otherwise take
is one `rollback` undoes.

`start` while the phase is already `switched` is refused — a third CA is not a
state this design has, and it would need a third slot in §3's table.
`drop-old` during `distributing` is refused too: the CA it would drop is the
one currently signing. A refused request is **consumed like an accepted one**,
and the refusal reported. Not symmetry for its own sake: the phase is driven
only on a tick with no request pending, so a refusal left in place would stall
the sequence where it stands rather than merely repeat its complaint — a
`drop-old` set during `distributing` would hold the rotation mid-window
indefinitely. The unrecognised value above is the only one left in place, and
it is also the only one that is never acted on.

`Provider.Start` drives all of it. It is already a leader-bound `Runnable`, so
only one process ever writes. Its loop ticks hourly today
(`RenewCheckInterval`), which is far too coarse for a rotation, so it gains a
second cadence: while `ca-rotation-phase` is set, it looks every 30 seconds
instead.

A rotation is visible without reading logs. Five events go on the secret —
`RotationStarted`, `RotationBlocked`, `RotationSwitched`, `RotationCompleted`,
and `RotationRequestUnrecognised` for the warning two paragraphs up: it is not
reported as `RotationBlocked`, because an operator triaging a `RotationBlocked`
warning would go looking for a namespace that is not there — nothing is
gated on anything, the rotation is only waiting on a second, correctly spelled
annotation. `internal/certs/metrics.go` registers two gauges in the shape the other
packages already use (`internal/agentserver/metrics.go`,
`internal/grpcauth/metrics.go`): `spawnery_ca_rotation_phase`, labelled by
phase and carrying 1 for the active one, and
`spawnery_ca_rotation_blocked_namespaces`. The second exists because "stuck in
`distributing` for two days" is the failure this design most plausibly
produces, and it should be a query rather than something somebody happens to
notice.

Every write is a fresh `Get` followed by `Update`, retried on conflict. The
store already uses the uncached client (`main.go:239`), and the secret is now
written by two parties — an update from a stale copy would silently discard the
annotation a human had just set.

## 5. The gate, and the two things it is driven from

Before the window of §6 starts running, the operator confirms that every
namespace where an agent could be running holds the new CA: it takes the union
of the namespaces of the `Network` objects and the namespaces of the managed
pods that still run a process, reads `spawnery-ca` in each, parses the PEMs in
`ca.crt` and looks for one whose SHA-256 matches `ca-next.crt`.

**Driven from the `Network` objects and the pods, not from the ConfigMaps.**
This is the correction that came out of reading the code rather than assuming
it. "A `Network` owns its namespace" is a rule about which of two `Network`
objects wins (`pickNamespaceOwner`), not a Kubernetes `OwnerReference` on a
`Namespace` — the operator never creates a namespace and never owns one. The CA
ConfigMap carries no owner reference either, deliberately, so that it outlives
the operator. So a `spawnery-ca` ConfigMap whose `Network` was deleted stays in
its namespace forever with whatever bundle it last received, and a gate phrased
as "every managed CA ConfigMap" would wait on a dead namespace until somebody
cleaned it up by hand.

**The `Network` objects alone are not the whole answer either.** That is the
second half of the union, and it came out of following the deleted-`Network`
case the rest of the way. A `ServerGroup` or `ProxyGroup` carries no
`OwnerReference` to its `Network` — `ServerGroupReconciler` sets group →
`Server` and nothing sets `Network` → group — so deleting a `Network` leaves
the groups and their pods running. In that namespace nothing refreshes
`spawnery-ca` any more: `NetworkReconciler` needs the `Network`,
`ProxyGroupReconciler` returns through `refuse` before its `Bootstrap.Ensure`,
and `ServerReconciler` reaches `Ensure` only under its `createPod` guard. A
gate listing `Network`s alone skips that namespace entirely — the window
elapses, the switch runs, and every agent there fails its next handshake. So
the gate also takes every namespace holding a pod labelled
`spawnery.cloud/managed-by` that is not in a terminal phase, using the same
label and the same cluster-wide `pods: list` the orphan sweep already has.

This is not a retreat from the paragraph above it. A ConfigMap outlives
everything, which is exactly why driving the gate from ConfigMaps would block a
rotation forever on a namespace nobody will ever clean up. Pods do not outlive
everything: they are deleted, or they finish, and either way they go away. And
a namespace with running agent pods and no `Network` **should** block the
rotation and be named in `ca-rotation-blocked-on`, because that is precisely a
namespace where the switch would strand somebody — blocking loudly is this
design's own chosen behaviour for "I cannot yet prove this is safe", and the
remedy is a human deleting or draining the leftover groups. The gate's real
question was always "where could an agent be running", and `Network` alone was
an incomplete answer to it.

Terminal is the only exclusion and it is narrow on purpose. A `Succeeded` or
`Failed` pod runs no process and will never open another agent stream. A
`Pending` pod counts, because it is about to start and will read whatever
bundle it finds; a terminating one counts too, because it is still running its
agent until the kubelet is done with it. This is deliberately not
`podTerminal`, `internal/controller`'s definition of the same-sounding
question: that one also calls a crash-looping pod finished, which is right for
a drain and wrong here, since such a pod's restart policy is `Always` — it
comes back, re-reads the bundle, and has to find the new CA in it.

While any namespace is missing, the phase stays `distributing`, the missing
namespaces are named in `ca-rotation-blocked-on`, and a `RotationBlocked`
warning is recorded. There is no timeout. The state it is blocked in — bundle
`old‖new`, serving certificate signed by the old CA — is fully working: nobody
is locked out and nothing expires. Waiting costs nothing, and switching is the
only step in the sequence that can do harm.

An annotation is not an unbounded field, so `ca-rotation-blocked-on` carries at
most ten namespace names followed by `and N more`; the event and the log carry
the same summary.

**Once the gate passes, it is not checked again.** `ca-rotation-since` is
stamped and the window runs. Neither half of the union is re-evaluated: a
`Network` created during the window does not reset it, and neither does a pod
started during it. Its namespace's ConfigMap receives the current bundle —
already `old‖new` — on its first reconcile, and its pods have never held
anything else. Re-checking would be the obvious implementation and would let a
cluster where networks are created regularly push the switch out forever.

## 6. The window

`projectionMargin + HardDeadline`, measured from `ca-rotation-since`.

`HardDeadline` is the running operator's own `--agent-session-deadline`, not an
estimate — it has to be passed from `main.go` into the store, which is one new
field. The bound is right because the hard-deadline timer is armed per stream
when that stream starts (`agentserver/server.go:338`), so a stream that began
just before the file changed is closed at most `HardDeadline` later, and the
reconnect that follows re-reads the file.

`projectionMargin` is a constant of two minutes: the kubelet's `--sync-frequency`
defaults to one minute and the watch-based projection is faster than that in
practice, so two is the margin rather than the estimate. It is stated here as
arithmetic on documented periods, not measured, because the thing that would
make it wrong — a cluster running a much longer sync frequency — is a
configuration this operator cannot see. If that ever needs to be tunable it
becomes a flag; it does not need to be one now.

## 7. Failure, and the cases that will actually happen

**The secret cannot be written.** The rotation stalls where it is. Every phase
of this design is a working state, so a stall is a delay and not an outage. The
error is logged and the phase gauge keeps reporting where it stopped.

**The operator restarts mid-rotation.** The next leader reads the phase and the
timestamp out of the secret and carries on. The only thing lost is the
in-memory tick; the 30-second cadence picks it up again. This is the reason the
state is in the secret rather than in the process.

**An agent is disconnected for the whole window.** It reads the file when it
next connects, which is after the switch, and by then the file holds `old‖new`.
It is not stranded. An agent whose pod never comes back is not stranded either,
because it is not connected in the first place.

**A namespace never accepts the bundle.** §5: blocked, named, indefinitely.
This is the failure that `NetworkReconciler`'s own bootstrap event now reports
independently, so the same obstacle shows up on the `Network` and on the
secret, saying the same thing in both places.

## 8. What it costs when nothing is wrong

Nothing, outside a rotation. `Provider.Start` reads one annotation per hourly
tick — the secret `Get` it already performs — and the 30-second cadence only
exists while a phase is set.

During a rotation, the gate is one `List` of `Network` objects and one `Get`
per distinct namespace, every 30 seconds, against the uncached client. For a
cluster with a handful of networks that is a handful of requests per minute for
about a quarter of an hour, once. No new RBAC is needed: the ClusterRole
already carries `configmaps: get;list;watch;create;update` from the
bootstrapper's marker, and `secrets: get;create;update` in the operator's own
namespace from the store's. The gate reads each ConfigMap with a
`Get` through the uncached client, so `internal/rbacaudit/required.go` gains a
second call site in the `configmaps: get` entry's Why, the way `pods: patch`
did — not in the `list` entry, whose Why is about the restricted cache and
stays true as it is.

## 9. How it is proven

**In `internal/certs`, against a real API server.** `store_envtest_test.go`
already runs against envtest with an injectable clock, which is what makes a
window testable at all. The sequence gets one test per claim rather than one
test for the sequence: that `start` publishes two PEMs and leaves the serving
certificate chaining to the old CA; that the gate blocks while a `Network`'s
namespace holds a stale ConfigMap and names it; that the window does not elapse
early; that the switch produces a serving certificate chaining to the new CA
while the old one is still published; that `switched` does not advance on its
own; that `drop-old` narrows the bundle and removes the key; that `rollback`
restores a serving certificate chaining to the old CA and abandons the
rotation.

Two that guard decisions §4 and §5 record, because each is a line a later
reader would plausibly move:

- **A `Network` created during the window does not reset it.** Directly
  asserted, because re-checking the gate is the natural implementation.
- **An unrecognised `rotate-ca` value is left in place and warned about**, and
  a recognised one is removed once acted on.

**In `internal/certs/bundle_test.go`**, that `parseCA` still finds the signing
CA when the published bundle holds two, and that the swap moves the right key
with the right certificate. A swap that pairs a certificate with the other CA's
key would produce a bundle `Validate` rejects, and that is the failure this
test exists to catch before it reaches a cluster.

**Across the language boundary, in `hack/agent-test.sh`.** The claim that a
real agent trusts a two-PEM bundle and connects to an operator presenting a
certificate signed by the *second* of them is not something Go tests or Kotlin
tests can make on their own — it needs a real JVM, a real handshake, and a real
mounted file, which is exactly what that script already provides for its five
existing phases. A sixth phase serves from `spawnery-stubop` with a certificate
signed by the second PEM. Like the other phases it needs the built images, so
it runs where those are available rather than on every `make test`.

**No `hack/e2e.sh` scenario.** A rotation takes a quarter of an hour of
wall-clock by construction, and the parts an end-to-end run could observe are
the parts envtest already observes with a controllable clock.

## 10. Acceptance criteria

1. Annotating `spawnery.cloud/rotate-ca=start` publishes a bundle holding two
   CA certificates, while the serving certificate still chains to the old one.
2. The rotation does not advance while any namespace holding a `Network` lacks
   the new CA in its `spawnery-ca` ConfigMap, and those namespaces are named on
   the secret and in an event.
3. Once every such namespace holds it, the switch happens after
   `projectionMargin + --agent-session-deadline` and not before.
4. After the switch the serving certificate chains to the new CA, the old CA is
   still published, and the operator does not advance further without a second
   annotation.
5. `drop-old` publishes the new CA alone and removes the old key.
6. `rollback` returns the serving certificate to one chaining to the old CA and
   abandons the rotation, leaving no state that would advance on its own.
7. A `Network` created during the window does not postpone the switch.
8. `Bundle.parseCA`, `Reissue`, `reissueOrIssue` and `Validate` are unchanged,
   and a cluster that never rotates behaves exactly as before.
9. A real agent, against a real operator-shaped server, connects when the
   server's certificate is signed by the second certificate in its mounted
   bundle.
10. `docs/known-issues.md`'s "The CA has no rotation procedure" entry is
    replaced by what is now true, and keeps saying that a compromised key is
    still an emergency with a different recipe.
