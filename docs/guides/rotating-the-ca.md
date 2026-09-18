# Rotating the agent channel's CA

Putting a new CA behind the agent channel takes no agent down and no pod
restart, and it is a human's to drive: the operator waits between the steps
rather than taking them all at once. It runs on annotations on the operator's
own TLS secret, `spawnery-agent-tls`, in the namespace the chart was installed
into — `spawnery-system` in
[Installing the operator](../getting-started/index.md).

## The sequence

Ask for a rotation. A second CA is minted and published beside the one that is
still signing, and the phase becomes `distributing`:

```bash
kubectl -n spawnery-system annotate secret spawnery-agent-tls \
  spawnery.cloud/rotate-ca=start --overwrite
```

The operator answers in annotations on that same secret; this prints them all:

```bash
kubectl -n spawnery-system get secret spawnery-agent-tls \
  -o jsonpath='{.metadata.annotations}'
```

- `spawnery.cloud/ca-rotation-phase` reads `distributing`, then `switched`
  once the operator has switched on its own, then goes away.
- `spawnery.cloud/ca-rotation-blocked-on` names the namespaces that have not
  taken the new CA yet.
- `spawnery.cloud/ca-rotation-since` is stamped when the wait for those
  namespaces ended; after the switch it reads as how long the outgoing CA has
  been waiting for a human.
- `spawnery.cloud/ca-rotation-discarded` appears only if a rotation slot was
  hand-edited into something that would not parse.

**Nothing here happens quickly.** On an idle cluster `start` can sit for up to
an hour before it is picked up, and the switch follows roughly a quarter of an
hour after the last namespace has caught up. Both waits are what makes the
rotation safe.

A request the operator will not carry out — a `drop-old` sent before the phase
reads `switched`, most of all — is consumed without changing anything, and
leaves an event as its only trace:

```bash
kubectl -n spawnery-system get events \
  --field-selector involvedObject.name=spawnery-agent-tls
```

Once the phase reads `switched`, drop the CA that was replaced. This ends the
rotation and cannot be undone:

```bash
kubectl -n spawnery-system annotate secret spawnery-agent-tls \
  spawnery.cloud/rotate-ca=drop-old --overwrite
```

To abandon the rotation instead, from either phase before `drop-old`: out of
`distributing` this discards the incoming CA, out of `switched` it signs the
serving certificate back under the old one.

```bash
kubectl -n spawnery-system annotate secret spawnery-agent-tls \
  spawnery.cloud/rotate-ca=rollback --overwrite
```

## What the operator waits for

**`start` can sit for up to an hour.** It is read only on a tick of
`Provider.Start`'s loop, which runs at `RenewCheckInterval` — one hour — while
nothing is rotating, and until that tick there is no phase, no event and no
gauge change. Restarting the operator pod skips the wait, since
`Provider.Start` runs `AdvanceRotation` once before arming its first timer.
From `start` onwards the loop is on a 30-second cadence, which is how soon
`drop-old` and `rollback` are picked up.

**From there the operator drives itself, checking every 30 seconds**
(`RotationCheckInterval`), and waits for two things in order. First, every
namespace where an agent could be running must show the new CA in its own
`spawnery-ca` ConfigMap — those holding a `Network` plus those holding a
managed pod not in a terminal phase (`namespacesMissingCA`); that needs no pod
restart, because `NetworkReconciler` calls `Bootstrapper.Ensure` on every
reconcile. Second, once they all have — the moment `ca-rotation-since` is
stamped, not `start` — a wait covering the kubelet's projection delay plus
[`--agent-session-deadline`](../reference/operator-flags.md#-agent-session-deadline):
`projectionMargin` (2 minutes) plus that deadline (10 minutes by default),
roughly a quarter of an hour, so that every agent stream open when the
ConfigMap changed has reopened once.

## What blocks the gate

**The gate stops running the moment `ca-rotation-since` is stamped** and is
never re-evaluated — deliberate, because a cluster where networks are created
regularly would otherwise push the switch out forever. Anything created after
it passed receives the two-CA bundle on its first reconcile and has never held
anything else.

**A namespace with leftover agent pods and no `Network` blocks the rotation**,
by design, and is named in `ca-rotation-blocked-on` like any other:
`ServerGroup` and `ProxyGroup` carry no `OwnerReference` to the `Network`, so
deleting a `Network` leaves their pods running with nothing refreshing
`spawnery-ca` there, and switching would strand them at their next handshake.
Delete or drain the leftover groups; the gate clears within one 30-second
check once their pods are gone.

**A `RotationBlocked` event does not repeat**: it fires only when the blocked
list *changes*, and Kubernetes expires it within the hour, after which nothing
in `kubectl describe secret spawnery-agent-tls` mentions it. The durable
signals are `ca-rotation-blocked-on` and the
`spawnery_ca_rotation_blocked_namespaces` gauge. Alert on the gauge.

## When a request is refused

A refusal consumes `rotate-ca` exactly as an accepted request does. A `start`
while a rotation is already open, a `drop-old` outside `switched`, and a
`rollback` with nothing in progress are all refused this way, each with a
`RotationRequestRefused` warning naming the phase that refused it. Within one
tick the annotation is gone; ask again by setting it again. The exception is
a value that is none of the three: it is never consumed, so its
`RotationRequestUnrecognised` event fires on every tick for as long as it sits
there — every 30 seconds while a rotation is in flight.

Eight reasons appear as events here (`internal/certs/events.go`):
`RotationStarted`, `RotationBlocked`, `RotationSwitched`, `RotationCompleted`,
`RotationRequestUnrecognised`, `RotationRequestRefused`,
`RotationSlotDiscarded` and `RotationSlotTruncated`. `rollback` alone ends a
rotation with no event. All expire within the hour; the gauge
`spawnery_ca_rotation_phase` (1 for the phase in effect, 0 for the others)
does not.

## Do not hand-edit the secret

On every tick, *before* it looks at `rotate-ca` at all, the operator re-reads
`ca-next.crt` and `ca-previous.crt` and checks that each is exactly the PEM
encoding of one certificate: those bytes go verbatim into every `spawnery-ca`
ConfigMap, and an agent that cannot parse the bundle loses its whole trust
store, not one slot. A failing slot is repaired (truncated to its first PEM
block, `RotationSlotTruncated`) or cleared (`RotationSlotDiscarded`) there and
then. Only a hand-edited or truncated secret reaches this; nothing the
procedure itself does can.

A cleared `ca-next.crt` while `distributing` **abandons the rotation**. A
cleared `ca-previous.crt` while `switched` **completes the irreversible drop,
within one 30-second tick**: those bytes are what a `rollback` would have
signed with, so a slipped paste leaves about one tick in which to put them
back. `spawnery.cloud/ca-rotation-discarded` is the durable record — slot,
parse error, outcome and time. It is cleared only by the next accepted
`start`, so a record sitting on a secret with no phase describes the last
thing that happened to a slot and not necessarily the rotation that just
finished: read its timestamp before reading it as news.
`internal/certs/rotation.go` carries the rest.

## Nothing starts this on its own

`CALifetime` (`internal/certs/bundle.go`) is ten years, and no path in the
operator checks a threshold or acts on how much life is left. That is
deliberate: how many days remaining should worry somebody is a fact about a
cluster, not about this code.

**What is not left to memory is the clock.**
`spawnery_ca_expiry_timestamp_seconds` and
`spawnery_serving_cert_expiry_timestamp_seconds` are written from
`Provider.Set`. The chart's optional `PrometheusRule`
(`metrics.prometheusRule.enabled`) fires `SpawneryCAExpiringSoon` at
`caExpiryWarningDays`, 90 by default, and
`SpawneryServingCertificateNotRenewing` when the *serving* certificate — which
renews itself at a third of its life remaining — has stopped doing so. Both
are in [Metrics and alerts](../reference/metrics-and-alerts.md).

**A compromised CA key is a different emergency**: the overlap above keeps
trusting the outgoing CA for the width of that wait, which is what a
compromise cannot afford. "Delete the secret, restart all pods" stays the
answer there.

Start with time in hand: the procedure above has a hold in the middle that
waits for one.
