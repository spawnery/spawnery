# Two follow-ups from the CA rotation's final review

## 1. What this is

The CA rotation landed on 2026-08-21 (`docs/superpowers/specs/2026-08-21-ca-rotation-design.md`).
Its final whole-branch review triaged sixteen deferred findings as "can stand
for merge" and singled out two as wanting a follow-up rather than a quiet
burial. This is that follow-up.

They are unrelated to each other except in provenance, and both are small.

**What this does not do:** it changes no phase, no transition, no annotation
vocabulary, and no timing. A rotation that runs on well-formed input behaves
exactly as it does today.

## 2. The first: a rotation slot nobody validates reaches every agent

`Store.Ensure` reads the four rotation keys straight out of the secret into the
bundle (`internal/certs/store.go:105-112`). `Bundle.Validate` does not look at
them, and must not — it keeps its current behaviour, and a cluster that never
rotates has to behave exactly as before. From there the bytes travel
`PublishedCA()` → `Provider.Set` → `Provider.CABundle` → `Bootstrapper.CA` →
the `spawnery-ca` ConfigMap of every namespace.

The consumer is `OperatorChannel.trustManager`, which parses the whole bundle
with `CertificateFactory.generateCertificates` and **throws** on input that is
not a certificate — its own test asserts that. So one bad byte in
`ca-next.crt` does not cost a rotation; it costs every agent in every namespace
its entire trust store. Compare the same damage to `ca.crt`, which `Ensure`'s
`Validate`/`reissueOrIssue` path repairs by itself.

Only a hand-edited secret produces it today. That is why it was Minor, and it
is also why it is worth closing: the blast radius is the whole fleet and the
fix is small.

### Where the guard goes, and where it does not

**Not in `Ensure`'s error return.** `Provider.Start` returns that error from a
`Runnable` (`store.go:269-271`), so it is fatal at startup. Refusing to serve
because somebody mistyped an annotation would be a worse outage than the one it
describes — the same argument the rotation's own code already makes one
paragraph further down about a rotation that cannot advance.

**The publication guard goes in `PublishedCA()`**, which is the single function
whose output reaches an agent. A slot whose certificate does not parse is
omitted from what it returns. Putting it at the chokepoint rather than at a
call site is deliberate: a later path that publishes the bundle from somewhere
else is exactly how this defect would come back, and a guard on the chokepoint
cannot be bypassed by one.

`PublishedCA` is pure and has no logger, so the omission there is silent. That
is acceptable because it is a safety net, not the report — the loud part is
§3, and it runs within one `RotationCheckInterval` while a rotation is in
flight and one `RenewCheckInterval` otherwise.

**Only the certificate halves are checked.** They are what gets published. A
malformed *key* reaches no agent, and it already fails loudly at the moment it
matters: `SwitchToNext` and `RestorePrevious` both route through `Reissue` →
`parseCA`, which refuses a key that does not parse or does not match its
certificate.

The check is `pem.Decode` **and** `x509.ParseCertificate`, because
`CertificateFactory.generateCertificates` throws on both a non-PEM blob and a
PEM envelope around something that is not a certificate.

## 3. The cleanup, and why it is not symmetric

`AdvanceRotation` re-reads the secret on every call, so it is where the loud
part belongs — and it is the only place that decides a rotation's phase.
(`Ensure` writes the slots too, via `carryRotation`, but only to carry forward
what a renewal would otherwise drop; it never decides anything about the
sequence.)
Finding an unparseable certificate slot, it clears that slot, records what it
discarded (§4), fires a `Warning`, and leaves the rotation in a state that does
not advance on its own. What that state is depends on which slot broke, and the
two cases are not mirror images.

**`ca-next.crt`, while `distributing`: abandon the rotation.** It never
distributed anything usable — no agent can have come to trust a certificate
that does not parse — so the correct end state is the one `rollback` out of
`distributing` already produces: `WithoutRotation()`, `clearRotationAnnotations`,
the signing CA published alone. Every agent trusted that CA throughout.

**`ca-previous.crt`, while `switched`: complete the drop.** This one looks like
the operator performing `drop-old` unasked — the branch's one irreversible step,
and the whole reason the sequence holds at `switched`. It is not. The hold has
exactly one purpose, which is that a rollback remains possible, and a rollback
signs with the previous CA: `RestorePrevious` → `Reissue` → `parseCA`, on those
very bytes. The moment they stopped parsing the rollback became impossible.
Clearing the slot takes away no ability; it records that the ability is already
gone. Nobody is stranded either — at `switched` the serving certificate chains
to the new CA, which every agent trusts, so narrowing the published bundle to
the new CA alone changes nothing they depend on.

**With no phase set at all:** clear the slot and say so. A leftover
unparseable slot on an idle secret has nothing to abandon and nothing to
complete; it simply must not be published, and the annotation of §4 is what
tells whoever left it there.

**The rule keys on the slot, the phase only decides the end state.** Every
unparseable certificate slot is cleared and recorded, always. The rotation is
abandoned or completed only when the slot that broke is the one the current
phase depends on — so a `ca-previous` slot occupying a `distributing` secret,
which is a state no transition produces, is cleared and reported without
disturbing the rotation, and two broken slots are two cleanups under the same
rule.

## 4. What survives the cleanup

The design owner chose self-cleanup over leaving the bytes in place, and its
stated cost was that nobody can then see what was written and that an event
expires after about an hour. That cost is paid off cheaply without keeping the
bytes:

```
spawnery.cloud/ca-rotation-discarded   ca-next.crt: certificate is not PEM (2026-08-21T14:02:11Z)
```

The slot, the parse error and the time — which is what a diagnosis needs; the
raw bytes are not. It is written in the same `applyStep` as the cleanup, so it
cannot land without the cleanup or the cleanup without it. It is cleared by the
next accepted `start`, so it never narrates an old failure beside a running
rotation.

The event is `Warning`, on the secret, with the existing
`ReasonRotationRequestRefused`'s neighbours — a seventh reason,
`RotationSlotDiscarded`, because none of the six describes this: nothing was
refused, nothing was unrecognised, no gate is holding, and the phase change is
a consequence rather than the news.

## 5. The second: the conflict retry has no behavioural test

`applyStep` (`internal/certs/rotation.go:446-467`) wraps its work in
`retry.RetryOnConflict` with the `Get` **inside** the retried function, which is
the entire concurrency story for an object two parties write. Nothing exercises
it.

The test that reaches the part worth protecting is not "an unrelated annotation
survives a conflict". It is the `consume` closure at `rotation.go:158-165`:

```go
	if secret.Annotations[AnnotationRotateRequest] == request {
		delete(secret.Annotations, AnnotationRotateRequest)
	}
```

The first reviewer called this the subtlety most implementations get wrong. The
scenario it guards: the operator decides to act on `start`; between its read and
its write a human replaces the annotation with `rollback`; the `Update` conflicts;
the retry re-reads and finds `rollback`; and `consume` must **not** delete it,
because nobody has acted on it yet. Losing it there would silently discard an
instruction a human gave under pressure — and `start`'s own work must still land,
because it succeeded.

`sigs.k8s.io/controller-runtime/pkg/client/interceptor` is already imported in
`internal/certs/rotation_envtest_test.go` for the gate's unreadable-ConfigMap
test, so the mechanism needs nothing new: intercept `Update`, return a
`Conflict` on the first call and write the competing annotation as a side
effect, then let the second through.

## 6. How it is proven

- **A slot that is not PEM, and a PEM envelope around something that is not a
  certificate**, are both omitted from `PublishedCA()`. Two cases, because the
  agent throws on both and only one of them is caught by `pem.Decode`.
- **Each of the three cleanup outcomes**, asserted on the secret read back:
  `ca-next` while `distributing` leaves no rotation and no phase; `ca-previous`
  while `switched` leaves no rotation and no phase; a slot with no phase set
  leaves the secret otherwise untouched. All three assert the discarded
  annotation names the slot and the reason.
- **The discarded annotation is cleared by the next accepted `start`**, so it
  cannot narrate an old failure beside a live rotation.
- **`Ensure` still does not fail** on a corrupt slot — asserted directly,
  because making it fail is the obvious implementation and it takes the
  operator down at startup.
- **The conflict**, as §5 describes it: `start` lands, `rollback` survives.

Each of the first two groups gets a mutation: publishing the slot anyway, and
skipping the cleanup, must each turn its own test red.

**No `hack/agent-test.sh` phase.** The claim that the agent throws on a
malformed bundle is already the agent's own test, and the fix is that such a
bundle never leaves the operator — which is a Go-side property.

## 7. Acceptance criteria

1. A rotation slot whose certificate does not parse is absent from
   `PublishedCA()`, so it never reaches a namespace's `spawnery-ca` ConfigMap.
2. `Ensure` does not fail because of it, and the operator starts normally.
3. An unparseable `ca-next.crt` while `distributing` abandons the rotation,
   leaving the state `rollback` out of `distributing` already produces.
4. An unparseable `ca-previous.crt` while `switched` completes the drop, and
   the design says why that is not the operator performing `drop-old` unasked.
5. An unparseable slot with no phase set is cleared and nothing else changes.
6. Every cleanup writes `spawnery.cloud/ca-rotation-discarded` naming the slot,
   the parse error and the time, in the same update as the cleanup, and fires a
   `RotationSlotDiscarded` warning.
7. The next accepted `start` clears that annotation.
8. A conflict on `applyStep`'s update, with a competing writer replacing
   `rotate-ca` between the read and the write, leaves the competing value in
   place while the acted-on step still lands.
9. `Bundle.Validate`, `parseCA`, `Reissue`, `reissueOrIssue` and `Issue` are
   unchanged, and a cluster that never rotates behaves exactly as before.
