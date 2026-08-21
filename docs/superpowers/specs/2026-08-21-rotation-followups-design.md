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
with `CertificateFactory.generateCertificates`. So a bad `ca-next.crt` does not
cost a rotation; it costs every agent in every namespace its entire trust
store — the CA that is still signing included, because the failure is for the
stream and not for the block. Compare the same damage to `ca.crt`, which
`Ensure`'s `Validate`/`reissueOrIssue` path repairs by itself.

> **Corrected after review, and measured.** This section, `parsableCert`'s doc
> and the Go tests' failure messages all used to say that
> `generateCertificates` "throws for the whole stream rather than skipping the
> offending block" on anything that is not a certificate. Run against the
> OpenJDK in this repository's devshell, mirroring `trustManager` exactly, that
> is wrong in both directions:
>
> | second half of `good ca.crt ‖ slot` | result |
> |---|---|
> | plain garbage, no five-hyphen run | **OK**, n=1 |
> | good certificate + trailing garbage | **OK**, n=2 |
> | leading garbage line + good certificate | **OK**, n=2 |
> | `-- this is not a certificate --` (two hyphens) | **OK**, n=1 |
> | good certificate + a *second valid* certificate | **OK**, n=3 — all three trusted |
> | PEM envelope around a non-certificate | **throws** |
> | second block with malformed base64 | **throws** |
> | a bare `-----not a header-----` line | **throws** |
> | `-----BEGIN CERTIFICATE-----` with no footer | **throws** |
> | a valid `EC PRIVATE KEY` block | **throws** |
>
> `X509Factory.readOneBlock` skips everything before the first line beginning
> with a five-hyphen run and returns null at end of stream rather than
> throwing. What kills the stream is **a line beginning with a five-hyphen run
> that does not open a complete, decodable certificate block**; other stray
> bytes are stepped over, and anything already parsed is kept. (A stream in
> which *nothing* parsed and which was not empty throws "No certificate data
> found", which is why `trustManager`'s own "not a certificate" test passes.)
>
> Every treatment in this design was and remains conservative, so none of them
> changed because of this — but the reasoning left for the next maintainer was
> false, and one Go fixture (`-- this is not a certificate --`) named an
> outage the agent does not in fact suffer. §6 now requires a fixture that
> genuinely throws, and `agent/common`'s own test pins the claim in Kotlin.
>
> The last row is the one that changes an *argument* rather than a fixture:
> the multi-block mode §3 repairs is not an outage in the agent at all. It is
> a **silent trust expansion** — every extra block a hand-edit pasted is
> loaded into every agent's trust store as a CA. §3 is corrected accordingly;
> the treatment is unchanged, and the reason for it is now the stronger one.

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

The check is `pem.Decode` **and** `x509.ParseCertificate`, because a PEM
envelope around something that is not a certificate satisfies the first and
throws in the agent all the same.

**And it is deliberately stricter than the agent.** The table above says the
agent tolerates stray bytes that carry no five-hyphen run, and this check does
not. That tolerance is a property of one JDK's block scanner rather than of
the format, it is invisible in the bytes a human pastes, and a single `-----`
anywhere in them flips it. Publishing on the strength of it would mean the
fleet's trust store depends on whether a pasted comment line happened to use
five dashes. So the rule is the narrow one — exactly the PEM encoding of one
certificate — and §3 repairs or clears everything else.

## 3. The cleanup, and why it is not symmetric

`AdvanceRotation` re-reads the secret on every call, so it is where the loud
part belongs — and it is the only place that decides a rotation's phase.
(`Ensure` writes the slots too, via `carryRotation`, but only to carry forward
what a renewal would otherwise drop; it never decides anything about the
sequence.)
Finding a certificate slot an agent could not parse, it repairs the slot where
it can and clears it where it cannot, records what it did (§4), fires a
`Warning`, and leaves the rotation in a state that does not advance on its own.
Which of the two, and what that state is, turns on one question first and on
the slot second.

**The question is: is the slot's first PEM block a certificate?** One
predicate, two outcomes, the same for either slot — and it is `parsableCert`'s
own rule read backwards, since `parsableCert` accepts exactly a slot that *is*
that block and nothing more.

> **Revised after review.** This read "the failure mode decides", over a list
> of three modes. The list was both longer than it needed to be and short by
> one case: a slot with junk pasted *in front of* the certificate passed
> `parsableCert` outright, because `pem.Decode` skips it and the leftover
> `rest` the check looked at came back empty. Built on `firstPEMBlock`
> instead — the slot must be byte-identical to its own first block — the rule
> is one comparison, the hole is closed, and leading junk lands where trailing
> junk always did: repaired.

**If it is a certificate, the slot is repairable.** `parseCA` decodes the first
block and ignores everything around it, so a `ca-previous.crt` holding a
certificate with a chain pasted after it — a restore that carried an
intermediate along — still signs: `RestorePrevious` → `Reissue` → `parseCA`
succeeds on exactly those bytes.

What `trustManager` does with them is *not* to object — the measurement above
settles it: a stream of three valid blocks loads three CAs. That is worse than
an outage and not better, because it is silent. Every block a paste buffer
carried in becomes a certificate authority every agent in the fleet will accept
an operator identity from, and nothing anywhere says so. So that slot is
truncated to its first block instead of being thrown away. The first block is
precisely the one `parseCA` was already using, so the repair makes publication
and signing agree again — and their disagreement is the whole of the defect.
Nothing usable is lost, no phase moves, and the rotation carries on. This keys
on the predicate and not on the slot: `ca-next.crt` gets the same treatment for
the same reason.

**If it is not a certificate — not PEM at all, or a PEM envelope around
something that is not one — the slot is unusable.** `parseCA` fails on that
same first block, so it is unusable in fact and not only in publication. Those
slots are cleared, and only for them does the phase decide an end state.

The two outcomes stay distinct even though one predicate now selects between
them, because the reasoning behind them is different: a repair asserts that
something usable is still there, and a clearance asserts that nothing is. A
single "throw it away" would be simpler and would destroy rollbacks that work
(see the corrected paragraph below).

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
very bytes. A slot only reaches this branch when `parseCA` fails on the same
first block `parsableCert` rejected, so the rollback was already impossible. Clearing the slot takes away no ability; it records that the
ability is already gone. Nobody is stranded either — at `switched` the serving
certificate chains to the new CA, which every agent trusts, so narrowing the
published bundle to the new CA alone changes nothing they depend on.

> **Corrected after review.** This paragraph read "the moment they stopped
> parsing the rollback became impossible", without qualification, and that was
> false wherever `parseCA` reads only the first block and signs perfectly
> happily. Completing the drop there would have
> destroyed a rollback that worked, while the warning told the operator it had
> already been impossible. Nor could that be answered by clearing the slot and
> holding the phase: clearing kills the rollback whatever the reason, so the
> hold would then advertise a choice nobody could act on, which is worse than
> either alternative. Repairing every slot whose first block is a certificate
> is what makes the sentence above true of everything that now reaches it.

**With no phase set at all:** clear the slot and say so. A leftover
unparseable slot on an idle secret has nothing to abandon and nothing to
complete; it simply must not be published, and the annotation of §4 is what
tells whoever left it there.

**The rule keys on the predicate, then on the slot; the phase only decides the
end state.** Every certificate slot that is not exactly the PEM encoding of one
certificate is repaired or cleared, and recorded, always. Only a *cleared* slot ends anything,
and only when it is the one the current phase depends on — so a `ca-previous`
slot occupying a `distributing` secret, which is a state no transition
produces, is cleared and reported without disturbing the rotation, and two
broken slots are two changes under the same rule, which may well be one repair
and one clearance in the same step.

## 4. What survives the cleanup

The design owner chose self-cleanup over leaving the bytes in place, and its
stated cost was that nobody can then see what was written and that an event
expires after about an hour. That cost is paid off cheaply without keeping the
bytes:

```
spawnery.cloud/ca-rotation-discarded   ca-next.crt: not PEM (2026-08-21T14:02:11Z)
spawnery.cloud/ca-rotation-discarded   ca-previous.crt: more than its first PEM block; truncated to that block (2026-08-21T14:02:11Z)
```

The slot, the parse error, what became of the slot, and the time — which is
what a diagnosis needs; the raw bytes are not. It is written in the same
`applyStep` as the cleanup, so it cannot land without the cleanup or the
cleanup without it. It is cleared by the next accepted `start`, so it never
narrates an old failure beside a running rotation. **One annotation for both
outcomes**, as the two lines above show: it is the single durable answer to
"what happened to my slots", its own wording says which of the two happened,
and one key is one thing to clear and one thing for a reader to know about.
Two slots touched in one step are two entries in one value.

The event is `Warning`, on the secret, with the existing
`ReasonRotationRequestRefused`'s neighbours — a seventh reason,
`RotationSlotDiscarded`, because none of the six describes this: nothing was
refused, nothing was unrecognised, no gate is holding, and the phase change is
a consequence rather than the news.

The repair gets an eighth, `RotationSlotTruncated`, rather than sharing the
seventh with a note that says otherwise. The reason is the field a human
triages on, and nothing is discarded in a repair: the slot is still in the
secret, still in the rotation, and now usable. A `RotationSlotDiscarded` that
had to be read to the end before one could tell that nothing had been
discarded would teach a reader to distrust the reason on the events that mean
what they say. The annotation is shared; the reason is not.

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

- **Four slot shapes are omitted from `PublishedCA()`**: a PEM header whose
  body is not base64, a PEM envelope around something that is not a
  certificate, a valid certificate followed by a stray header, and a stray
  header *before* a valid certificate. Each is a shape the agent genuinely
  throws on, checked against the real parser rather than asserted (§2) — the
  fixture this list opened with, `-- this is not a certificate --`, was not.
  The four are not interchangeable: `pem.Decode` alone catches only the first,
  and the last is the one a "decode, then inspect the leftovers" check waves
  through, because there are no leftovers to inspect.
- **The repairable case is repaired, and the ability it would have cost is
  shown to be real**: a `ca-previous.crt` holding a certificate followed by a
  second block is truncated to the first, the phase stays `switched`, and a
  `rollback` issued afterwards succeeds and puts the original CA back in charge
  of signing. That last step is the finding itself: without it the test would
  assert the new behaviour without showing why the old one was wrong. The same
  repair on `ca-next.crt`, after which the rotation still reaches its switch,
  pins that the rule keys on the predicate and not on the slot.
- **Each of the three cleanup outcomes**, asserted on the secret read back:
  `ca-next` while `distributing` leaves no rotation and no phase; `ca-previous`
  while `switched` leaves no rotation and no phase; a slot with no phase set
  leaves the secret otherwise untouched. All three assert the discarded
  annotation names the slot and the reason.
- **The record carries the slot, the reason and the time**, the time asserted
  against the store's own clock rather than merely for being present.
- **Two broken slots in one call** — one cleared and one truncated — are one
  step, two entries in the record and two events of the right two reasons. With
  both cleared at `switched`, the "the drop was completed" clause appears only
  on the `ca-previous` event: it is a claim about the slot the hold existed
  for, and on the other event it would send a reader after a rollback that had
  nothing to do with it.
- **The discarded annotation is cleared by the next accepted `start`**, so it
  cannot narrate an old failure beside a live rotation.
- **`Ensure` still does not fail** on a corrupt slot — asserted directly,
  because making it fail is the obvious implementation and it takes the
  operator down at startup.
- **The conflict**, as §5 describes it: `start` lands, `rollback` survives.

Each of the first two groups gets a mutation: publishing the slot anyway, and
skipping the cleanup, must each turn its own test red. So do the three the
cleanup adds — discarding the repairable mode with the other two, dropping the
time from the record, and writing the outcome clause per call instead of per
slot.

**No `hack/agent-test.sh` phase.** The claim that the agent throws on a
malformed bundle is already the agent's own test, and the fix is that such a
bundle never leaves the operator — which is a Go-side property.

## 7. Acceptance criteria

1. A rotation slot whose certificate does not parse is absent from
   `PublishedCA()`, so it never reaches a namespace's `spawnery-ca` ConfigMap.
2. `Ensure` does not fail because of it, and the operator starts normally.
3. An unparseable `ca-next.crt` while `distributing` abandons the rotation,
   leaving the state `rollback` out of `distributing` already produces.
4. A `ca-previous.crt` while `switched` whose first PEM block `parseCA` also
   fails on completes the drop, and the design says why that is not the
   operator performing `drop-old` unasked. A slot that fails only for holding
   more than that block is instead truncated to it —
   the block `parseCA` already signs with — whichever of the two slots it is;
   no phase moves, and a `rollback` after the repair still works. The numbering
   here is unchanged from the original list: this criterion gained its second
   half rather than a criterion being inserted.
5. An unparseable slot with no phase set is cleared and nothing else changes.
6. Every repair and every cleanup writes `spawnery.cloud/ca-rotation-discarded`
   naming the slot, the parse error, what became of the slot and the time, in
   the same update as the change, and fires a `RotationSlotDiscarded` or
   `RotationSlotTruncated` warning accordingly.
7. The next accepted `start` clears that annotation.
8. A conflict on `applyStep`'s update, with a competing writer replacing
   `rotate-ca` between the read and the write, leaves the competing value in
   place while the acted-on step still lands.
9. `Bundle.Validate`, `parseCA`, `Reissue`, `reissueOrIssue` and `Issue` are
   unchanged, and a cluster that never rotates behaves exactly as before.
