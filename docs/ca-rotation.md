# Rotating the agent channel's CA

The operator issues one CA and serves one certificate from it. Replacing that
CA without restarting a single agent is a procedure a human drives, and this
page is it.

Nothing here is a problem. `known-issues.md` carries the one thing about this
that is: nothing in the operator starts a rotation on its own.

Design: `superpowers/specs/2026-08-21-ca-rotation-design.md`, built on the
distribution guarantee established in
`superpowers/specs/2026-08-21-ca-bundle-distribution-design.md`. A driven run
end to end is scenario 10 of `runbook-milestone-6-rollout.md`.

What makes the overlap work at all: a new CA reaches every namespace holding
a `Network` without waiting for a pod restart, because `NetworkReconciler`
calls `Bootstrapper.Ensure` on every reconcile.

The whole interface is annotations on that secret
(`internal/certs/rotation.go`):

- `spawnery.cloud/rotate-ca=start` mints a second CA and publishes it beside
  the one that is still signing — the serving certificate keeps chaining to
  the old CA throughout (`Store.applyRequest`'s `RequestStart` case,
  `Bundle.WithNextCA`). `spawnery.cloud/ca-rotation-phase` becomes
  `distributing`.
- **`start` can sit unnoticed for up to an hour.** The annotation is only read
  on a tick of `Provider.Start`'s loop, and while nothing is rotating that loop
  ticks at `RenewCheckInterval` — one hour (`checkInterval(false)`). On an idle
  cluster the worst case after annotating is sixty minutes in which nothing at
  all happens: no phase, no event, no change to either gauge. The 30-second
  cadence only engages once a phase is set, which is to say after the tick that
  picked the request up. If that wait is not acceptable, restart the operator
  pod: `Provider.Start` runs `Store.Ensure` and `AdvanceRotation` once
  immediately, before it arms its first timer, so the new leader picks the
  request up as it comes up. `start` is the only request this affects: it is the
  only one sent while no rotation is in flight. `drivePhase` reports both
  `distributing` and `switched` as in flight, so from `start` onwards — across
  restarts, since the phase is re-read from the secret — the loop stays on the
  30-second cadence and a `drop-old` or `rollback` is picked up within it.
- From there the operator drives itself, checking every 30 seconds
  (`RotationCheckInterval`). It waits for two things in order. First, every
  namespace where an agent could be running to show the new CA in its own
  `spawnery-ca` ConfigMap: the union of the namespaces holding a `Network` and
  the namespaces holding a managed pod that is not in a terminal phase, read
  from the cluster on each check until the gate passes (`namespacesMissingCA`).
  Until that is true, `spawnery.cloud/ca-rotation-blocked-on` names the
  namespaces still missing it (truncated past ten). Second, once every
  namespace has caught up — `spawnery.cloud/ca-rotation-since` is stamped at
  that moment, not at `start` — a further wait covering the kubelet's
  projection delay plus the operator's own `--agent-session-deadline`
  (`drivePhase`; `projectionMargin` (2 minutes) + `Store.AgentSessionDeadline`,
  which defaults to 10 minutes — roughly a quarter of an hour in total, longer
  if that flag is raised), so that every agent stream open when the ConfigMap
  changed has had a chance to close and reopen at least once.
- **The gate stops running the moment `since` is stamped**, and is never
  re-evaluated for that rotation — neither half of the union. That is
  deliberate (design, section 5: a cluster where networks are created regularly
  would otherwise push the switch out forever), and it is the fact to reason
  from when reasoning about safety: what the switch is safe against is the set
  of namespaces as it stood at the instant the gate passed, plus the argument
  that anything created afterwards receives the two-CA bundle on its first
  reconcile and has never held anything else.
- **A `RotationBlocked` event does not repeat.** It fires only when the list of
  blocked namespaces *changes* (`drivePhase` compares the new note against
  `ca-rotation-blocked-on` before writing either), so a gate blocked on the
  same namespace for days fires once and then goes quiet — and Kubernetes
  expires the event out of the API within the hour, after which nothing in
  `kubectl describe secret spawnery-agent-tls` mentions it. The durable signals
  are the `ca-rotation-blocked-on` annotation and the
  `spawnery_ca_rotation_blocked_namespaces` gauge; alert on the gauge, not on
  the event.
- **A namespace with leftover agent pods and no `Network` blocks the
  rotation**, by design, and is named in `ca-rotation-blocked-on` like any
  other. `ServerGroup` and `ProxyGroup` carry no `OwnerReference` to the
  `Network`, so deleting a `Network` leaves the groups and their pods running,
  and nothing refreshes `spawnery-ca` in that namespace afterwards — switching
  would strand those agents at their next handshake. The remedy is to delete or
  drain the leftover groups; the gate clears within one 30-second check once
  their pods are gone.
- Once both conditions hold, the operator switches on its own: the serving
  certificate is re-signed under the new CA,
  `spawnery.cloud/ca-rotation-phase` becomes `switched`, and
  `spawnery.cloud/ca-rotation-since` is restamped — from here it reads as how
  long the outgoing CA has been waiting for a human. The operator then
  **holds**: the old CA stays published and trusted, and nothing moves
  further without a second annotation.
- `spawnery.cloud/rotate-ca=drop-old`, sent once `switched`, drops the
  outgoing CA; `ca.crt` is the only CA published afterwards, and all three
  rotation annotations are cleared.
- `spawnery.cloud/rotate-ca=rollback` **abandons** the rotation rather than
  pausing it, and works from either phase: out of `distributing` it discards
  the unused incoming CA, and out of `switched` it re-signs the serving
  certificate back under the old one (`Bundle.RestorePrevious`). Either way
  nothing is left that would advance on its own.

The operator clears `rotate-ca` once it has acted on whatever value it held —
a refusal consumes the annotation exactly as an accepted request does. A
`start` while a rotation is already open, a `drop-old` outside `switched`, and
a `rollback` with nothing in progress are all refused this way, each with a
`RotationRequestRefused` warning naming the phase that refused it. That event
is the only trace a refusal leaves: within one tick the annotation itself is
gone. Asking again means setting the annotation again.

A value that is none of `start`, `drop-old` or `rollback` is the one
exception: it is left in place and does not halt whatever step was already due
— the sequence keeps advancing on its own schedule regardless
(`AdvanceRotation`'s default case). Because it is deliberately never consumed,
its `RotationRequestUnrecognised` event fires on **every** tick for as long as
the value sits on the secret — every 30 seconds while a rotation is in flight.
Correcting or deleting the annotation is what stops it.

**The operator repairs or throws away a hand-edited rotation slot, and it can
end the rotation while doing so.** On every tick, *before* it looks at
`rotate-ca` at all, `AdvanceRotation` re-reads `ca-next.crt` and
`ca-previous.crt` out of the secret and checks that each is exactly the PEM
encoding of one certificate — those bytes are published verbatim into every
namespace's `spawnery-ca` ConfigMap, and an agent that cannot parse the bundle
loses its entire trust store, not merely the slot. A slot that fails the check
is dealt with there and then, and that is the tick's one step: a `rotate-ca`
set at the same moment is **not** consumed, and is picked up on the next tick
against the state the cleanup left. Only a hand-edited or truncated secret
reaches this; nothing the procedure itself does can.

Which of the two happens turns on one question — **is the slot's first PEM
block a certificate?**

- **Yes: the slot is repaired.** It is truncated to that first block, with a
  `RotationSlotTruncated` warning. This is what a pasted chain, or a stray line
  before or after the certificate, produces. The operator was already signing
  with that first block, so nothing usable is lost, no phase moves, and the
  rotation carries on. It is not cosmetic: a surplus block that happens to be a
  valid certificate is loaded by every agent as another CA it will accept the
  operator's identity from, and nothing else would ever say so.

  It also fires on a difference you would not call damage. The test is
  byte-exact, so a certificate that is merely re-wrapped to a different column,
  saved with CRLF line endings, or left with a blank line at the end is
  repaired too — same warning, same record, and while `distributing` the same
  restarted window. One rule covers stray bytes and a surplus certificate
  because separating them would mean trusting that stray bytes never happen to
  contain a PEM header, which is not a property worth resting a fleet on. The
  operator's own writes are always canonical, so this only ever follows a hand
  edit, and it happens once: the repaired slot is canonical and is not touched
  again.
- **No — not PEM at all, or a PEM envelope around something that is not a
  certificate: the slot is cleared**, with a `RotationSlotDiscarded` warning.
  The bytes are gone; they are kept nowhere. What becomes of the rotation then
  depends on the phase:
  - **`ca-next.crt` while `distributing` — the rotation is abandoned.** The end
    state is the one `rollback` out of `distributing` produces: no slot, no
    phase, `ca.crt` published alone. Nothing usable had been distributed, and
    every agent trusted `ca.crt` throughout. Start again when ready.
  - **`ca-previous.crt` while `switched` — the drop is completed.** This is the
    procedure's one irreversible step, performed without anyone asking, and it
    is the behaviour worth having read *before* meeting it. The hold at
    `switched` exists for exactly one purpose — that a `rollback` stays
    possible — and a rollback signs with the previous CA's bytes
    (`RestorePrevious` → `Reissue` → `parseCA`). A slot only reaches this
    branch when those bytes will not parse, so the rollback was already
    impossible; clearing the slot records that rather than causing it. Nobody
    is stranded: the serving certificate chains to the new CA, which every
    agent came to trust during the overlap. But the bytes are gone once the
    operator has acted, and while a rotation is in flight it acts within 30
    seconds — so a `ca-previous.crt` damaged by a slipped paste leaves about
    one tick in which to put it back. After that the old CA can only come from
    a backup of the secret, not from the operator.
  - **Anything else** — a `ca-previous` sitting on a `distributing` secret, or
    either slot on an idle one — is cleared and reported, and a rotation that
    did not depend on it carries on untouched.

**Repairing `ca-next.crt` during `distributing` also deletes
`spawnery.cloud/ca-rotation-since` and re-runs the gate.** The wait starts
over, and `ca-rotation-blocked-on` may reappear naming namespaces that had
already caught up: the gate had passed against bytes that are no longer in the
slot, and if what was pasted in front of the certificate is a different CA,
switching to it would strand every agent in the fleet. Expect to lose up to the
quarter of an hour again. A `ca-previous` repair at `switched` does not do
this — there is no window there, and `since` means the age of the hold.

**`spawnery.cloud/ca-rotation-discarded` is the durable record of all of the
above**, and it is where to look once the events have expired. It carries the
slot, the parse error, whether the slot was cleared or truncated, and the time
— everything except the bytes, which are the one thing that must stop existing.
It is one annotation for both outcomes, and two slots touched in one step are
two entries in one value, `;`-separated, with a single timestamp at the end:

```
spawnery.cloud/ca-rotation-discarded: ca-next.crt: not PEM (2026-08-21T14:02:11Z)
spawnery.cloud/ca-rotation-discarded: ca-next.crt: more than its first PEM block; truncated to that block; ca-previous.crt: parse certificate: x509: malformed certificate (2026-08-21T14:02:11Z)
```

**It is removed only by the next accepted `start`.** Neither `drop-old` nor
`rollback` touches it, and neither does a refusal; a later repair or clearance
overwrites it with that step's own entries. So a record on a secret with no
phase set describes the last thing that happened to a slot, which is not
necessarily anything to do with the rotation that has just finished — check its
timestamp before reading it as news.

Eight reasons appear as events on the secret — `RotationStarted`,
`RotationBlocked`, `RotationSwitched`, `RotationCompleted`,
`RotationRequestUnrecognised`, `RotationRequestRefused`,
`RotationSlotDiscarded` and `RotationSlotTruncated`
(`internal/certs/events.go`); `rollback` alone ends a rotation with no event of
its own, since the human-written annotation already says what happened. All of
them expire out of the API within the hour, which is why the last two have
`ca-rotation-discarded` behind them. Two gauges track it from outside
(`internal/certs/metrics.go`): `spawnery_ca_rotation_phase`, 1 for the current
phase and 0 for the others so "is anything rotating" is one query, and
`spawnery_ca_rotation_blocked_namespaces`, the count
`ca-rotation-blocked-on` is built from.

**A compromised CA key is still a different emergency, not this procedure.**
The overlap above is orderly precisely because it keeps trusting the outgoing
CA for the width of that wait — on the order of a quarter of an hour. That is
exactly what a compromise cannot afford to do. "Delete the secret, restart all
pods" stays the answer to that case.
