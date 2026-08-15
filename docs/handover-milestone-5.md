# Handover to milestone 5

Status: end of milestone 5a, persistent groups exist (2026-08-15). Written
before the whole-branch review that closes this milestone (Task 8 of
`.superpowers/sdd/2026-08-15-persistent-groups/`); anything that review moves
will be recorded in place here, the way milestone 4's own reviews were.

This document is not a spec. It says where 5a stopped and what 5b — ordered
shutdown, `Recreate` updates, `storage.size` growth — and 5c — secret rotation
— each find in place. The design decisions live in
`docs/superpowers/specs/2026-08-15-persistent-groups-design.md`; the open
points are in `docs/known-issues.md`, whose "From milestone 5a" section this
document does not repeat in full.

**If you are looking for milestone 4's own record — proxy drain, node drain,
the three evidence runs against a real cluster — it stays at
[`handover-milestone-4.md`](handover-milestone-4.md).** This document starts
fresh rather than extending that one, for the same reason 4b got its own
document once it stopped being the thing to read `4c` against: milestone 5 is
a different subsystem (storage, not the proxy or scaling layer), and
`docs/superpowers/specs/2026-08-15-persistent-groups-design.md` is a different
spec from every one `handover-milestone-4.md` was written against.

## Where we are

A `ServerGroup` of type `Persistent` used to accept `spec.replicas`,
`spec.storage` and everything else the CRD already carried for it, and build
nothing: `size()` never asked how many servers such a group should have, no
`PersistentVolumeClaim` was ever created anywhere in this repository, and the
`dataVolume` pod-spec code that already pointed a persistent pod at a claim
named `DataClaimName(srv.Name)` pointed at a claim that would never exist.

5a closes that. A `Persistent` group with `replicas: 2` now produces
`<group>-0` and `<group>-1`, each with its own `PersistentVolumeClaim`, each
reaching `Ready` on a real cluster, each joinable. Lowering `replicas` removes
the highest ordinal through the ordinary drain 4c-3 already proved. Raising it
back brings that ordinal up again, and it finds its world where it left it —
proven at the object level by envtest (`TestDeletingAPersistentServerLeavesItsClaim`
and its neighbours in `internal/controller/server_controller_test.go`), and
not yet proven with an actual block placed and rejoined to on a real cluster —
see `docs/runbook-milestone-5a-evidence.md`, marked **NOT YET DRIVEN**.

## 5a has landed

- **The ordinal is the identity.** `PersistentServerName(group, ordinal)`
  (`internal/controller/persistent.go`) names a persistent server
  `<group>-<ordinal>` rather than the random-suffixed name `NewServerName`
  gives an ephemeral one, and `Server.Spec.Ordinal` carries the number. Every
  downstream naming decision follows from this one: `podspec.DataClaimName`
  derives the claim name from the server's own name, so the same ordinal
  always addresses the same claim across every deletion and recreation of the
  `Server` object. `OrdinalOf` also exists, to parse an ordinal back out of a
  name for diagnostics, but it has no production caller — the sizing rule
  reads `spec.ordinal` off `ServerView.Ordinal` instead, deliberately, because
  parsing the name back would be a second copy of the one truth already on
  the object, and it is the one place `NewServerName`'s own random suffix
  could theoretically collide with a valid ordinal string (see
  `docs/known-issues.md`'s carried-forward note on the sweep, below, for how
  that ambiguity was found and ruled on).
- **`DecidePersistentSize`** (`internal/controller/persistent.go`) is the
  second sizing rule design §3.2 asks for, standing beside `DecideSize` rather
  than inside it. It shares `SizeDecision` and nothing else: no spare-slot
  arithmetic, no player counts, no generations — its whole input is which
  ordinals exist, which of those are already leaving
  (`ServerView.leaving()`), and how many there should be
  (`spec.replicas`, by way of `ServerGroup.DesiredReplicas()`, already in
  place since before this milestone). Missing ordinals are created
  lowest-first; surplus ordinals are removed highest-first; an ordinal held by
  a server that is already leaving is neither recreated nor removed again —
  building a second server on a still-mounted `ReadWriteOnce` claim would hang
  on the volume rather than fail cleanly, so the replacement waits out the
  drain instead, bounded by `spec.drain.timeoutSeconds`.
- **`BuildDataClaim`** (`internal/podspec/claim.go`) renders the
  `PersistentVolumeClaim` a persistent server's world lives on, from
  `spec.storage`'s size, class and access modes. Three properties, all
  deliberate: **no owner reference**, so the claim outlives the server, the
  group, and an operator who deletes the wrong object; **created, never
  updated**, so a claim already sitting there — the ordinary case, since a
  recreated ordinal is supposed to find its old world — is left exactly as it
  is; and **no wait for `Bound`**, because waiting would deadlock under
  `volumeBindingMode: WaitForFirstConsumer`, which is the default of the
  node-local storage classes this milestone's own failure modes are about.
- **The Server controller creates the claim, before the pod, tolerating
  `AlreadyExists` on both** (`internal/controller/server_controller.go`).
  Nothing here waits for the claim to bind; the pod is created straight after
  it, and binding is Kubernetes' problem from there.
- **`ServerGroupReconciler.size()` now branches on the group's type**
  (`internal/controller/servergroup_controller.go`): `DecideSize` for an
  ephemeral group, `DecidePersistentSize` for a persistent one, sharing one
  `Condemn` attachment, one backoff gate over creation, and one execution path
  for create, delete and retire — the two rules disagree about what a group's
  size means, not about what happens once a decision exists.
- **`ConditionBackingOff` and `ConditionDegraded` now publish for a group of
  either type.** They were built ephemeral-only in 4d, before a persistent
  group could ever have a server to fail; this milestone's own review found
  that leaving them behind the `if group.IsEphemeral()` gate left a
  persistent group backing off and giving up in total silence — counting
  failures nobody could see, on a status that read `Pending` throughout,
  indistinguishable from a slow start. `ScalingLimited` stays
  ephemeral-only, on purpose: it answers a question — is the `maxReplicas`
  ceiling holding back the capacity players need — that has no meaning for a
  group sized by a fixed number nobody's play session moves. `pruneFailed`
  stays ephemeral-only too, also on purpose: for a persistent group, a
  `Failed` corpse *is* what holds the ordinal, and pruning it early would
  accelerate the very thrash the failure path exists to stall.
- **`spec.replicas` is now required for `Persistent`,** by a CEL rule reading
  `self.type != 'Persistent' || has(self.replicas)`. Before this rule such a
  group was accepted and ran zero servers, silently.
- **RBAC: `persistentvolumeclaims: create;get;list;watch`**, and nothing else
  — no `delete`, no `update` — on the ClusterRole, with the omission
  documented and enforced by `internal/rbacaudit/required.go`: its tests
  compare the generated role against the hand-maintained table in both
  directions, so a `delete` or `update` marker added anywhere later turns the
  suite red before it can ship. The manager's cache over claims is restricted
  to `spawnery.cloud/managed-by=spawnery-operator`
  (`cmd/spawnery-operator/main.go`), the same mechanism already narrowing the
  cache over ConfigMaps and ServiceAccounts, so the informer this grants does
  not hold every claim in every watched namespace.

## What 5b and 5c find in place

Master design §8, carried forward rather than re-derived:

- **Ordinals, claims, and both directions of `spec.replicas`, with the claim
  retained across every one of them.** A lowered `replicas`, a server deleted
  by hand, a node drain condemning a persistent server — every path that
  removes a `Server` leaves its claim exactly where it was, proven at the
  object level by `TestDeletingAPersistentServerLeavesItsClaim` and its
  neighbours.
- **`DecidePersistentSize` is the single place a persistent group's size is
  decided**, table-tested without a cluster
  (`internal/controller/persistent_test.go`). 5b's `Recreate` updates are
  meant to layer on top of this rule — a third rule, or a modification of
  this one's output before it reaches execution — rather than being woven
  into its three existing ones. Whoever builds that should read `size()`'s
  own comment on why the two-rule split exists before adding a third
  concern to either rule directly.
- **The grace period is already on the pod.**
  `TerminationGracePeriodSeconds` reaches `podspec.BuildServerPod` already,
  from a milestone before this one; 5b orders the shutdown sequence, it does
  not have to invent the time to save a world.
- **`spec.drain.timeoutSeconds` already bounds the wait** for an ordinal held
  by a leaving server, the same field and the same accessor
  (`ServerGroup.DrainTimeout()`) 4c already built and 5a reused without
  modification.
- **A runbook that brings a persistent group up on a real cluster is
  drafted** — `docs/runbook-milestone-5a-evidence.md` — for 5b's own evidence
  run to start from rather than rebuild. It is marked **NOT YET DRIVEN**; see
  that document's own header for what driving it will need and what its
  acceptance test is.
- **The failure path is a stall, not a loop, and it is documented.** A claim
  that never binds fails its server, which is deleted and recreated onto the
  same claim roughly once an hour (`spec.failedRetentionSeconds`) until the
  per-group backoff gives up at six counted failures — see
  `docs/known-issues.md`'s "From milestone 5a" for the full mechanism, checked
  link by link against the code. 5b's `Recreate` updates and 5c's secret
  rotation both touch persistent servers and should read that entry before
  assuming a `Failed` persistent server behaves the way an ephemeral one
  does.

## What 5a leaves open, briefly

`docs/known-issues.md`'s "From milestone 5a" section is the full list, checked
against the code; restated in one line each:

- **Claims accumulate and this operator can never remove one** — by design,
  and enforced by the RBAC audit rather than merely documented.
- **A persistent server on a node-pinned volume cannot follow a node
  drain** — 4c-3 recorded the limit before anything could reach it; 5a is
  what makes it reachable.
- **A claim that never binds ends in a deliberate stall**, and `Degraded`
  does not appear until roughly five and a half hours after the first failure
  at the CRD's default retention — six counted failures span five gaps, not
  six, and each gap runs close to `failedRetentionSeconds` (3600s) plus the
  replacement's own `--startup-deadline` (300s) before it can fail in turn,
  not an even hour. `status.consecutiveFailures` and `status.lastFailureAt`
  are the earlier signal.
- **A squatter — an object already holding a persistent ordinal's name
  without `spec.ordinal` — stalls that ordinal silently**, retried once every
  five-second pass forever, with no event, condition or log naming it.
- **`spec.replicas` is now required for `Persistent`**, which cannot reject an
  already-running group: none could exist before this milestone.

## Carrying the method forward

Milestone 4's own handover records two corrections the absolute-word sweep
earned across that milestone: the grep has to be case-insensitive, and its
word list needs `no`, `none`, `any`, `all` and `both` beside `never`, `only`,
`nothing`, `exactly one`, `cannot`, `always` and `every`. Both were used from
the first task of this milestone onward, and both earned their keep again —
this milestone found no new gap in the sweep itself, only in what a person has
to do once it has found something. What 5a adds is what happened *after* the
grep flagged a line: how often the flagged sentence really was wrong, and how
a test that looks like it is checking something can pass while checking
nothing at all.

**Seven instances of the signature defect** — a sentence that reads plausibly
while describing a mechanism the code does not have — surfaced across this
milestone's seven tasks, checked against
`.superpowers/sdd/2026-08-15-persistent-groups/progress.md` and the task
reports rather than repeated from memory:

- Task 4's implementer caught two, in the plan's own doc-comment text, before
  committing: present-tense claims that `docs/known-issues.md` already
  documented orphaned claims and that `cmd/spawnery-operator/main.go` already
  restricted its cache over them — both true only of tasks that had not yet
  run. This is the same shape milestone 4c-3's handover names as invisible to
  a diff-based grep — a claim about wiring that does not exist yet reads as
  ordinary prose and trips none of the flagged words — caught here
  prospectively, by a person reading the sentence against the state of the
  tree rather than by the sweep.
- Design §3.5 itself carries its own history of two more, deliberately kept
  rather than deleted: a first version claiming a persistent group "reports
  `Degraded`" on a failed claim, which this milestone's own review
  (Task 6) found false — neither `ConditionBackingOff` nor `ConditionDegraded`
  was published outside `if group.IsEphemeral()` at the time — and a second
  version, written to correct the first, that overcorrected into declaring
  *both* halves of the original claim false when only the reporting half was;
  commit `18dbcd5` is that second version, and its own message says as much
  ("section 3.5 was wrong on both halves, and the stall is the right part").
- The second version's own replacement text carried a new self-contradiction
  — asserting in the same breath that no replacement is ever ordered *and*
  that `status.consecutiveFailures` climbs toward the give-up threshold, which
  cannot both be true, since the counter only advances on a counted failure
  and nothing recreates a failed ordinal without a replacement being ordered.
  Task 6's fix round 1 caught this and produced commit `e838ba0`, "the third
  version of 3.5, this one verified link by link" — the version
  `docs/known-issues.md`'s own entry above is checked against.
- The seventh was caught inside the very commit that fixed the sixth: the
  implementer's own replacement sentence for the self-contradiction above
  claimed a persistent group's failures would otherwise be invisible
  "anywhere" — false, since `status.consecutiveFailures` is written
  unconditionally regardless of which conditions publish. Caught in the same
  pass, before committing, which is the argument for re-reading a fix rather
  than trusting that fixing one defect could not introduce a neighbour.

**Six assertions that could not discriminate — that passed whether the
production behaviour they claimed to test was present or not — surfaced
across five distinct shapes, and mutation found every one of the six; reading
found none of them:**

1. **Foreclosed by an earlier `Fatalf`.** `TestPendingNamesItsCreates`'
   (Task 2) third assertion, on `deletes`, could never fail on its own terms —
   its own first assertion's `t.Fatalf` on `creates` always halted the test
   first on the mutation that would have tripped the third.
2. **Never reaching the branch it named.** `DecidePersistentSize`'s
   `leaving()` guard in the surplus loop (Task 3) had a test case whose one
   leaving server kept its ordinal *below* `replicas`, so it never entered the
   surplus set the guard exists to protect — the guard could be deleted
   outright and the whole table stayed green.
3. **Dead by construction.** Task 5's own plan text asked for a mutation
   reversing the order `decision.Delete` names surplus ordinals in; with one
   surplus ordinal (or several, all removed in the same pass), the order has
   no observable effect on any object envtest can read, so no mutation of it
   could ever fail an envtest assertion — the property is real and pinned
   elsewhere (`internal/controller/persistent_test.go`'s own unit table over
   `DecidePersistentSize` directly), just not by the mutation the plan
   proposed.
4. **Unobservable at its layer, while pinned at another.** Two instances,
   both Task 6: `TestAPersistentServerGetsItsClaimBeforeItsPod`'s ordering
   claim cannot be observed in envtest, because both objects exist by the time
   a reconcile returns and the API server never checks that a pod's claimed
   volume actually exists — swapping the two `Create` calls passes the test
   unchanged. And `TestDeletingAPersistentServerLeavesItsClaim`'s claim about
   the claim's owner reference cannot be observed there either, because
   envtest runs no garbage collector — an owned claim outlives its deleted
   owner exactly as an unowned one would, so a mutation removing the owner
   reference altogether left the test passing. Both are disclosed in the
   tests' own doc comments rather than papered over; neither claim is false,
   only unpinnable at this layer.
5. **Green for the wrong reason, because a clock advance closed the window
   the assertion needed.** The first draft of
   `TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp` (Task 6, fix round
   1) folded a server's failure and the retention clock that clears its
   corpse into one helper, so by the time the group actually reconciled, the
   hour-long clock advance had already closed the backoff window the first
   assertion needed — `BackingOff` read `False` for a reason that had nothing
   to do with the property under test. Splitting "the failure opens the
   window" from "the retention clock closes it" into two explicit steps is
   what made the assertion mean what it claimed to.

**The envtest trap worth more than the rest of them together.** The API
server's `StorageObjectInUseProtection` admission plugin stamps
`kubernetes.io/pvc-protection` on every `PersistentVolumeClaim` at creation,
and no controller runs in envtest to take it back off — so a *deleted* claim
keeps carrying a deletion timestamp and keeps answering `Get` forever. The
first run of the mutation that added a stray `r.Delete(claim)` to the Server
controller's cleanup path — the single line that would have deleted a world —
passed both tests guarding this milestone's entire point, silently. A fixture
helper has to treat a deletion timestamp as gone, not merely an absent object;
`f.pod` already applied that rule for pods, and `f.claim`
(`internal/controller/server_controller_test.go`) now applies it for claims,
with the reasoning written into its own doc comment. Anyone writing a new
envtest fixture over any kind that carries a finalizer envtest itself does not
clear — this is not unique to PVCs — should assume `Get` returning success is
not the same claim as "still here," and check `DeletionTimestamp.IsZero()`
explicitly.

## The environment

Unchanged from `docs/handover-milestone-4.md`'s own "The environment" section:
`nix develop`, `make test`, `make agent-test`, `make image-test`,
`make image-repro`. Nothing under `proto/` or `agent/` moved on this branch
(`git diff master...HEAD --name-only`), so 5a added no agent-facing message and
`make agent-test` needed no extension — a claim checked against the diff
rather than against a run of the target itself, which Task 7 did not perform.
