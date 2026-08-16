# Handover to milestone 5

Status: end of milestone 5a, persistent groups exist (2026-08-15), **and the
evidence run has been driven (2026-08-16) — the acceptance test passed.**
First written before the whole-branch review that closes this milestone
(Task 8 of `.superpowers/sdd/2026-08-15-persistent-groups/`), revised in place
after it, the way milestone 4's own reviews were — that review's three
Important findings and its triage of the parked minors are recorded where they
belong below rather than appended as a postscript — and revised once more after
the evidence run, which has its own section below rather than a postscript for
the same reason.

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
**proven with an actual block, on a real cluster, on 2026-08-16** — see
`docs/runbook-milestone-5a-evidence.md`, and "The evidence run" below.

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

## The evidence run (2026-08-16)

`docs/runbook-milestone-5a-evidence.md` was driven against master at `f3c6fc1`
on a single-node `kind` cluster with its own default storage class —
`standard`, `rancher.io/local-path`, `WaitForFirstConsumer`, exactly as §2
predicted. It is the first run in this repository's history to measure storage
rather than the proxy layer.

**The acceptance test passed.** Blocks were placed at **-74 / -10**, the client
left, `kubectl delete pod survival-0` removed the pod, and the client rejoined.
In the driver's own words: *"ja die blöcke sind noch da."* That sentence is the
whole of what envtest could not reach, and it is why this milestone's central
claim is now settled rather than argued.

The measurements, for whoever writes 5b's run and wants numbers rather than
margins:

- `survival-0` passed its ready gate **24 seconds** after the apply
  (`ReadyGatePassed` at 11:20:46, applied 11:20:22), against the 90 seconds §5
  allows; the proxy was there at 12. Paper's first boot: `Done (4.626s)`.
- The claim `survival-0-data` was `Bound` within 12 seconds — a
  `WaitForFirstConsumer` class binding as soon as the pod that consumes it
  exists, which is precisely the behaviour `BuildDataClaim` declines to wait
  for.
- `spec.ordinal` read `0` on the object. The claim carried **no owner
  reference** — the milestone's single most load-bearing property, checked
  before the client ever joined and again after the recreate.
- Recreate: pod deleted at 11:23:02, replacement pod created at 11:23:02,
  `ReadyGatePassed` at 11:23:24. **22 seconds**, against a five-minute
  `--startup-deadline`. Paper's second boot logged `Done preparing level
  "world" (0.162s)` — read, not generated.
- Identities across the recreate: `Server` `582933f7…` → `c7880082…`, pod
  `788ef8f6…` → `fb8812de…`, **claim `2b0f11b8…` → `2b0f11b8…`**, created
  `11:20:22Z` both before and after. In `kubectl` output the whole milestone
  reads as one line: after the recreate the claim's `AGE` was 3m46s and the
  `Server` mounting it was 67s old — *the claim is older than the server that
  uses it.*

**Four expectations the runbook got wrong, all corrected in place; no command
in it changed.** Three are small — a fourth label on the claim it did not
list, and two event-trail artifacts it did not predict (`PodAdopted` on first
boot, and a `count: 2` on the *new* object's first `ReadyGatePassed`, because
client-go's aggregator keys on name and not UID). The fourth is worth carrying
as a method note rather than a typo:

**§8 promised the deleted `Server` would be observable — a deletion timestamp,
then a `NotFound` — and at two-second sampling neither ever appeared.** At
11:23:00 the object was UID `582933f7…`/`Ready`; at 11:23:02 it was UID
`c7880082…`/`Pending`. `PodLost`, the delete, the finalizer release, the
object's disappearance and the group's recreation all closed inside one
two-second gap. This is the same lesson
`docs/runbook-milestone-4c1-evidence.md`'s own header records from its §12 run
— *trust timestamps over polling* — arriving from the other direction: there,
polling was too coarse to time a transition; here, the transition does not
have an observable intermediate state at any rate a person can drive. The
durable form of the rule is that **a runbook should predict what will still be
true afterwards, not what will be briefly visible during.** The UID is that:
a changed `Server` UID under an unchanged name *is* the claim "the ordinal is
the identity, the object is not," and it is as true an hour later as it is in
the moment. §8 now asks for UIDs before and after instead.

**One finding that is not a runbook correction, and is the run's real
yield:** the recreate path logs `level=error` with a full stacktrace every
single time it runs, on the happy path. A reconcile writes to the `Server`
object after the recreate has already removed it — the reconciler reads through
a cache that can hand back an object the API server no longer has — and
`NotFound` escapes unwrapped. Three writes in
`internal/controller/server_controller.go` can be the one, and **none of the
three tolerates `NotFound`** (`:319`, `:340`, `:685`), while both `Delete`
calls on the same path carry an explicit `IsNotFound` guard (`:671`, `:348`).
Which write it is, the log does not say, and `docs/known-issues.md` declines to
guess rather than naming the likeliest and moving on. Nothing breaks: the
requeued pass fetches the `Server` at `:116`, where `client.IgnoreNotFound`
treats the absence as ordinary, and returns cleanly. What breaks is the
instruction
in the runbook's own §4, which tells the driver to watch the operator log
because a failing reconcile "says so there and nowhere else" — the most
important transition in this milestone announces itself as an error, and an
operator who learns to read past it will read past a real one.
`docs/known-issues.md`'s "From the milestone 5a evidence run" carries the trace
and argues why the fix belongs to 5b rather than to a documentation pass: a
lost status write is *supposed* to fail loudly there (it is what `:319`'s
recovery and the `PodAdopted` event depend on), and the obvious envtest for it
would pass without reproducing anything, because the cache-staleness window is
what produces the error in the first place.

**What the run could not settle, and did not pretend to.** `kind`'s local-path
provisioner runs `mkdir -m 0777`, so the `fsGroup: 10001` fix this milestone
landed had nothing to do on this cluster — a clean run here is not evidence
that fix works. §0 said so before the run and the run changed nothing about
it. The same goes for the node-drain limit on a node-pinned RWO volume: one
node, so nothing to pin against. Both wait for a run against a real cloud
storage class.

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
- **A runbook that brings a persistent group up on a real cluster is drafted
  *and driven*** — `docs/runbook-milestone-5a-evidence.md`, driven 2026-08-16 —
  for 5b's own evidence run to start from rather than rebuild. Every command in
  it ran as written and none needed changing; four *expectations* did, and two
  commands were added rather than corrected. 5b's run inherits a document whose
  §5 manifest, §4 relay and §8 event queries are known to work, which is most of
  the cost of an evidence run.
- **`fsGroup` is on every server pod now**, and 5b and 5c inherit it whether
  or not they think about storage. `podspec.BuildServerPod` sets
  `PodSecurityContext.FSGroup` to 10001 — the uid and gid `nix/oci-common.nix`
  builds the image with — together with `FSGroupChangePolicy: OnRootMismatch`.
  It closed a precondition `docs/known-issues.md` recorded two milestones ago
  under "From milestone 2b", which said the missing `fsGroup` "has to land
  before the first persistent server exists": without it a freshly provisioned
  claim arrives root-owned, the container runs as 10001, and Paper cannot write
  `/data` at all. 5a is what creates the first persistent server, so the
  precondition came due here. Two decisions inside it are worth carrying rather
  than rediscovering. It is set on **every** server pod rather than gated on
  the group's type — one `PodSecurityContext` shape to reason about, and an
  `emptyDir` is created fresh and empty so the chown costs nothing there. And
  the policy is `OnRootMismatch` rather than the kubelet's `Always` default,
  because `Always` recursively chowns a whole persistent world on every pod
  start; the cost of that choice, stated in the code, is that files deep in the
  tree with wrong ownership are not corrected. `BuildProxyPod` is untouched: a
  proxy mounts no claim.
- **The failure path is a stall, not a loop, and it is documented.** A claim
  that never binds fails its server, which is deleted and recreated onto the
  same claim roughly once an hour (`spec.failedRetentionSeconds`) until the
  per-group backoff gives up at six counted failures — see
  `docs/known-issues.md`'s "From milestone 5a" for the full mechanism, checked
  link by link against the code. 5b's `Recreate` updates and 5c's secret
  rotation both touch persistent servers and should read that entry before
  assuming a `Failed` persistent server behaves the way an ephemeral one
  does.
- **The failure streak is per group, and for a persistent group it should not
  be — that is 5b's, and it was ruled so deliberately.** `CountFailures`
  (`internal/controller/backoff.go`) resets the streak to zero whenever any of
  the group's servers carries a `ReadySince` newer than the last counted
  failure. For interchangeable servers that is the right rule; for ordinals
  that each own a world it is not, so a broken `survival-0` beside a
  `survival-1` that blips readiness once can keep `Degraded` from ever
  arriving. The durable fix is a **per-ordinal streak**, or more cheaply a
  reset restricted to the ordinal whose failure it would clear; either changes
  what `BackoffInputs` means for both kinds of group, which is why it is a
  design of its own rather than an appendix to "persistent groups exist".
  Whoever takes it should start from `CountFailures`' own doc comment, which
  argues the current rule correctly for the case it was written for, and from
  `TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp`, which runs a single
  ordinal and therefore cannot show the problem — a second ordinal in that test
  is the first thing to write.

## What 5a leaves open, briefly

`docs/known-issues.md`'s "From milestone 5a" section is the full list, checked
against the code; restated in one line each:

- **Claims accumulate and this operator can never remove one** — by design,
  and enforced by the RBAC audit rather than merely documented.
- **A persistent server on a node-pinned volume cannot follow a node
  drain** — 4c-3 recorded the limit before anything could reach it; 5a is
  what makes it reachable.
- **A claim that never binds ends in a deliberate stall**, and at
  `replicas: 1` `Degraded` does not appear until roughly five and a half hours
  after the first failure at the CRD's default retention — six counted
  failures span five gaps, not six, and each gap runs close to
  `failedRetentionSeconds` (3600s) plus the replacement's own
  `--startup-deadline` (300s) before it can fail in turn, not an even hour.
  `status.consecutiveFailures` and `status.lastFailureAt` are the earlier
  signal at that replica count. Note also that the give-up does not begin with
  an empty ordinal: the sixth corpse holds it for one more
  `failedRetentionSeconds` first, and the empty-ordinal state is where the path
  settles rather than where it starts.
- **With two or more ordinals that figure has no ceiling at all**, because
  `CountFailures` resets the streak on any sibling's newer `ReadySince` — see
  the next section for the durable fix 5b takes.
- **Lowering `replicas` nominates the top ordinal whoever is on it**, unlike
  the ephemeral rule, which skips a server that `mayHavePlayers()`. The
  players are protected by the drain and by `spec.drain.timeoutSeconds`, and
  by nothing after it.
- **Two servers carrying one `spec.ordinal` are invisible in both
  directions** — never surplus, never recreated — the mirror of the squatter
  entry.
- **A persistent group from before this upgrade keeps a stale `Ready: False`**
  saying persistent groups arrive in milestone 5, beside running pods. Nothing
  republishes it and nothing removes it; known-issues carries the patch that
  clears it.
- **`replicas: 0` reports `Pending` forever**, because `derivePhase` requires
  `readyReplicas > 0`. Pre-existing arithmetic; 5a is what makes zero a
  deliberate action.
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

**Nine instances of the signature defect** — a sentence that reads plausibly
while describing a mechanism the code does not have — surfaced across this
milestone's eight tasks. The count and its distribution are read off
`.superpowers/sdd/2026-08-15-persistent-groups/progress.md`, which numbers them
to nine, rather than repeated from memory; an earlier version of this paragraph
said seven across seven, and being wrong about the tally of this particular
defect in the document that exists to record it is itself the defect:

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
- The eighth is a shape the earlier seven do not cover: **cross-file within
  one task.** Task 8's own test comment claimed the runbook's `kind` run "is
  exactly" what verifies the `fsGroup` chown on a real cluster, while the
  runbook rewrite the same implementer had just made two files away said
  `kind`'s local-path provisioner masks the fix entirely. Both files were in
  one staged diff and the absolute-word sweep over that diff is what found it.
  A per-file read would not have: neither sentence is wrong on its own.
- The ninth is the tally's own author's, in a correction to a correction to a
  correction. The design-spec fix for the `Degraded` delay led with a bolded
  "roughly five hours" and conceded "five and a half in practice" two clauses
  later, while both operator-facing documents led with five and a half — and
  two paragraphs above it the unhedged "after six attempts at an hour apart"
  survived untouched, the same phrase that fix round had just softened in
  `docs/known-issues.md`. Caught by a re-review asked to judge the reviewer's
  own commit by the standard it had applied to the implementer's, and fixed in
  `9905713`.

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

**The closing review's fix wave found a seventh assertion of shape 2 —
*never reaching the branch it named* — and it was on the test guarding this
milestone's entire point.** `TestDeletingAPersistentServerLeavesItsClaim` ran
one reconcile after deleting the Server. That reconcile still sees the pod, so
it only asks for the pod's deletion; the branch that releases the finalizer and
lets the Server object go needs the pod already gone, and runs on the *next*
one. A `Delete` on the claim added to that release branch — the single line
that destroys a world — was therefore invisible to the test written to catch
exactly it. Mutation found this too, and reading did not, in a test that had
already been mutation-tested once for a different mutation on a different
branch. The lesson is narrower than "mutate more": a mutation kills only the
lines the test actually executes, so a single passing mutation says nothing
about a branch the test never enters. The test now runs two reconciles and
asserts the object is gone, which is what proves it got there.

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
`make agent-test` needed no extension. That was first written as a claim
checked against the diff alone, because nobody had run the target; it has since
been run, at the close of Task 7, and exited 0.
