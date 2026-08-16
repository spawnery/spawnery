# Milestone 5b: ordered shutdown, Recreate updates, storage growth

**Status: design of record, approved 2026-08-16.** It follows
`docs/superpowers/specs/2026-08-15-persistent-groups-design.md` (5a), which
gave a `Persistent` group ordinals, a `PersistentVolumeClaim` per ordinal and
both directions of `spec.replicas`. That milestone's evidence run was driven on
2026-08-16 and its acceptance test passed — a block placed, its pod deleted,
the block still there — so everything below builds on a claim that is settled
rather than argued.

Every file and line reference in this document was checked against the tree at
`074feb3` while the design was being written, not recalled.

## 1. What 5b is, and what it finds in place

5a made a persistent group *exist*. It did not make one *maintainable*. Three
things a person running such a group will try on the first day do nothing at
all today, and one reports nothing when it should:

- **Changing the image does nothing.** `DecidePersistentSize`
  (`internal/controller/persistent.go:102`) is deliberately generation-blind —
  its doc comment says so — so a persistent server keeps running the spec it
  was born under, without limit and without a signal. `Server.Spec.GroupGeneration`
  (`api/v1alpha1/server_types.go:36`) is already written at creation
  (`internal/controller/servergroup_controller.go:856`) and already read into
  `ServerView.Generation` (`:772`), but nothing on the persistent path compares
  it to anything.
- **Lowering `replicas` takes every surplus ordinal down at once.**
  `DecidePersistentSize` sorts surplus highest-first (`persistent.go:128`) and
  then appends all of them to `decision.Delete` in one pass. Dropping a group
  from five to one asks four worlds to save simultaneously. The sort order has
  no observable effect on any object, which is why 5a's own review recorded the
  mutation that reverses it as *dead by construction*.
- **Growing `spec.storage.size` does nothing.** The CRD has permitted growth
  since 5a — `storage.size must not shrink`
  (`api/v1alpha1/servergroup_types.go:112`) — but `podspec.BuildDataClaim` is
  *created, never updated*, and the ClusterRole carries no verb that can modify
  an existing claim — `get;list;watch;create`
  (`internal/controller/server_controller.go:107`, and the same four rows in
  `internal/rbacaudit/required.go:75-78`). The value diverges from the claim
  silently.
- **A broken ordinal may never reach `Degraded`.** `CountFailures`
  (`internal/controller/backoff.go:50`) resets the streak on the newest
  `ReadySince` across *all* of the group's servers. 5a's handover assigned the
  fix here.

A fifth was found while planning this milestone rather than while writing it,
and it is not about persistent groups at all:

- **A changed `motd` never reaches a running proxy.** `DesiredProxyHash` hashes
  the rendered pod, and `motd` reaches only the ConfigMap. Shipped since 4c-2;
  traced link by link in §3.7. It is in scope because the fix is the same
  change to the sibling of the function 5b is writing anyway.

5b closes all five. It adds no new controller and no new reconcile loop: the
mechanism for taking an ordinal down already exists and is proven — **deleting
the `Server` object is the drain sequence** — and this milestone gives that
mechanism two new occasions and one budget.

**What is deliberately *not* in 5b.** Secret rotation stays 5c. `spec.update`
remains forbidden for `Persistent` by the CEL rule at
`api/v1alpha1/servergroup_types.go:106`, and that rule is not lifted: the
one-at-a-time behaviour below is fixed, not configurable. `UpdateSpec` carries
`maxStaleSeconds`, which belongs to the ephemeral soft drain and would mean
nothing here; a field that does nothing in its context is a trap for the next
reader, and the way not to build that trap is not to expose the type.

## 2. One invariant, one nomination rule

The whole of 5b's control flow reduces to one sentence, which is the sentence a
test pins and an operator can be told:

> **At most one ordinal of a persistent group is down at a time, whatever the
> reason.**

That is a claim about the takedowns the rule below nominates, and it is not a
claim about the group. `Condemn` runs beside the rule and takes every server on
a departing node down at once — deliberately and unthrottled since 4c-3, whose
own known-issues entry gives the reason — so a node holding two ordinals empties
both. `docs/known-issues.md`'s 5b section carries that exception; §2 does not
try to close it, because a node that is leaving is not this rule's to negotiate
with.

`DecidePersistentSize` grows from *which ordinals are missing and which are
surplus* into *which are missing, and which single ordinal comes down next, and
why*. Three candidate classes, in strict priority order:

1. **Missing** ordinals below `replicas` — created, **all at once**, not
   serialised.
2. **Surplus** ordinals (`ordinal >= replicas`) — the highest one, subject to
   Gate A of §2.1.
3. **Stale** ordinals (§3's hash differs) — the highest one, subject to Gates A
   and B, and only once no surplus remains.

A fourth class, **resize-pending**, is added by §4 at the lowest priority,
subject to the same two gates as stale.

**Creation is not serialised, and that asymmetry is deliberate.** Creating an
ordinal takes nothing down: the volumes are independent, no player is
disconnected, and one Minecraft world does not have to be up before another can
start — a persistent server has an identity, but nothing about that identity
makes its startup depend on a lower ordinal's, which is what StatefulSet's
ordered startup exists for. Raising `replicas` from
one to five should produce five worlds as fast as the cluster can schedule
them. Ordered *shutdown* is what this milestone's name asks for, and ordered
startup is not silently included with it.

**Surplus outranks stale** because updating a server that is about to be
deleted spends downtime on a world nobody will use again.

### 2.1 "Nothing is down" is two gates, not one

A single test does not hold the invariant, and the first draft of this section
used one. The two are separate because they answer different questions.

**Gate A — serialisation. Applies to every takedown, always.** No server of the
group is `leaving()`, and no delete reservation for one is outstanding in
`Expectations`. This is what makes the highest-first order observable at all:
without it, every nomination in a pass fires together and the ordering is the
dead line 5a's review found.

**Gate B — recovery. Applies to stale and resize-pending nominations only.**
Every ordinal below `replicas` has a `Ready` server. This is the strong reading
of "down", chosen over the weak one — *the previous object has disappeared* —
which would let ordinal 4 still be booting while ordinal 3 begins to drain,
putting two worlds out at once while the invariant's sentence still claimed
one. StatefulSet waits for `Ready` for the same reason.

**Gate B deliberately does not apply to surplus removal**, and the first draft
of this design got that wrong in a way worth recording. A surplus ordinal sits
*above* `replicas`, so it is invisible to Gate B's own test — during its drain
the gate reads clear and would have released the next nomination while it was
still draining. Gate A is what actually holds the invariant there. Beyond the
mechanics, the behaviour is the one to want: scaling down is an instruction the
operator gave explicitly, often *because* something is wrong, and blocking it
until an unrelated sick ordinal recovers would withhold the remedy. Taking a
world away that nobody asked to keep does not compound an outage; taking a
healthy one down to update it does.

The invariant survives on Gate A alone. Gate B is the extra caution that
applies when the operator's edit, rather than the operator's instruction, is
what causes the outage.

**The strong reading has a price, and it is stated rather than designed
away: a permanently broken ordinal stalls the group's whole update.** This is
StatefulSet's best-known sharp edge and 5b inherits it knowingly. It is
tolerable here only because the stall is *reported*: `ConditionBackingOff` and
`ConditionDegraded` publish for persistent groups since 5a, and §5's streak fix
is what makes `Degraded` actually arrive rather than being reset forever by a
healthy sibling. A stall that announces itself is an operational fact; a silent
one would be a defect.

**Execution reuses 5a unchanged.** Nominating an ordinal means deleting its
`Server` object. `DecidePersistentSize` already counts an ordinal as taken
while any server carries it, in any phase, so the ordinal is held through the
drain — the replacement cannot race the volume — and the next pass recreates it
under the identical name, onto the identical claim, with the current spec. The
drain is bounded by `spec.drain.timeoutSeconds` exactly as it is today.

## 3. Staleness: hash what the operator renders

### 3.1 This function already exists for proxies

The obvious implementation is a hand-maintained list of the pod-affecting
fields, hashed. It carries a defect this repository has now seen in several
shapes: the list drifts from `podspec.BuildServerPod` silently. Someone adds a
field, the pod changes, the hash does not, and the result is an update that
never fires and never says why.

**Milestone 4c-2 already rejected that approach and built the alternative.**
`podspec.DesiredProxyHash` (`internal/podspec/hash.go:43`) renders the pod with
its name held at a fixed empty value, marshals it with `encoding/json` — whose
sorted map keys are what keeps the digest from flapping between passes — and
returns `hex.EncodeToString(sum[:8])`. Its doc comment makes the same argument
this section reached independently: holding the name empty is *stronger than
excluding the fields a name reaches today*, because a field derived from the
name added later needs no change to the hash function.

So 5b does not invent a mechanism. It writes **`DesiredServerHash`, the sibling
of an existing and shipped function**, with the same shape, the same digest
width and the same reasoning, and the plan should treat any divergence between
the two as something to justify rather than as freedom.

### 3.2 What is hashed

`podspec.DesiredServerHash(net, group)` renders and hashes. It calls
`BuildServerPod` with a **constant dummy `Server`** and hashes the result
together with the rendered config values.

The dummy neutralises exactly the inputs that are per-ordinal and are supposed
to be: `srv.Name` (used at `internal/podspec/server.go:335`, `:369`, `:371`,
`:378`), `srv.UID` (`:379`), and `DataClaimName(srv.Name)` (`:445`). Everything
that survives is what the group and the network contribute — image, resources,
scheduling, image pull secrets, mounts, config overlay, termination grace
period, and the name of the forwarding secret. **There is no list to maintain.
A change to `BuildServerPod` changes the hash, which is the entire point.**

**The rendered pod is not everything the operator writes for a server, and a
pod-only hash would have missed it.** `maxPlayers` never reaches the `PodSpec`
at all: `reconcileConfigMap` (`internal/controller/servergroup_controller.go:1069`)
marshals it into the group's ConfigMap, which `BuildServerPod` mounts *by name*
(`:285`). Changing `maxPlayers` updates the ConfigMap immediately while every
running server keeps the old value until some unrelated restart, and nothing
reports the gap. The hash therefore covers **both halves of what the operator
renders for a server** — the `PodSpec` and the config values. Both are pure
functions of `(net, group)`, both are written by the same reconciler, and both
belong in one answer to "is this ordinal current".

### 3.3 Two deliberate exclusions

- **`agentEndpoint` is excluded.** It comes from an operator flag, not from any
  spec. Including it would mean that restarting the operator with a different
  `--operator-namespace` restarts every world in the installation. The dummy
  argument keeps it out.
- **`storage.size` is excluded.** Growth is handled precisely in §4, with a
  restart only when the volume actually demands one. Folding size into the hash
  would restart an ordinal for every size change, including the online
  expansions where no restart is needed.

### 3.4 Determinism is a requirement, not a property

A `PodSpec` contains maps — labels, resource quantities, node selectors — and
Go's map iteration order is unspecified. A hash that differs between processes
would restart every world on every operator restart, which is worse than the
problem 5b set out to solve. Serialisation therefore goes through
`encoding/json`, which sorts map keys, before the digest. The test asserts
stability across repeated runs, not merely that equal inputs agree.

### 3.5 Where the hash lives

A new field `Server.spec.podHash`, beside the existing `spec.groupGeneration`,
which is left untouched for the ephemeral path. The `ServerGroup` controller
writes it when it creates a server and compares it when it nominates one. The
`Server` controller takes no part: it builds the pod exactly as it does today.

**`ServerView.Stale` is already taken and means something else.** It reports
that the *player count* cannot be trusted (`internal/controller/candidates.go:49`),
and a rule reading it asks `Players == 0 && !Stale`. Nothing in 5b may overload
that name. The view carries `PodHash string`, and staleness of the spec is
computed in the rule from a comparison, never stored as a second boolean called
something confusable.

### 3.6 The upgrade must not restart every world

Introducing the field creates a migration hazard severe enough to be a design
decision rather than an implementation note: **every server that already exists
carries an empty `spec.podHash`.** Compared naively against a freshly computed
hash, every ordinal in every persistent group is stale the moment the new
operator starts, and the first reconcile after an upgrade walks the whole
installation, restarting every world one ordinal at a time. Nobody asked for
that, and it would arrive as a surprise during a routine operator bump.

**An empty `spec.podHash` therefore means *adopt*, not *stale*.** The group
stamps the current hash onto such a server and orders no takedown. The server
is then current by definition, and the first genuine spec change after the
upgrade is what moves it. The cost is that one spec edit made *during* the
upgrade window can be missed on an already-running ordinal, which is a far
smaller surprise than a full restart and is bounded by the next edit.

The same reasoning applies wherever a hash is *widened* rather than
introduced — see §3.7, where extending the proxy hash changes its value for
every existing proxy.

### 3.7 The same gap on the proxy side: `motd`

Writing §3.2 turned up a defect in shipped code, and 5b fixes it because the
fix is the same change to the sibling function.

`DesiredProxyHash` hashes the rendered proxy pod only. Of the two fields in
`ProxyGroupSpec.Config` (`api/v1alpha1/proxygroup_types.go:106,110`),
**`playerLimit` is safe** — `renderProxyPod` puts it in the pod as the
`SPAWNERY_PLAYER_LIMIT` env var (`internal/podspec/proxy.go:223`), so the hash
sees it and a change rolls the group. **`motd` is not.** It appears nowhere in
`internal/podspec/proxy.go`; it reaches only the ConfigMap
(`internal/controller/proxygroup_controller.go:1301`). The consequence, checked
link by link:

- Changing `spec.config.motd` updates the ConfigMap.
- `ProxyGroup` reconciles — it does `Owns(&corev1.ConfigMap{})`
  (`internal/controller/proxygroup_controller.go:1482`) — but the requeue
  changes nothing, because staleness is `pods[i].Labels[LabelPodHash] !=
  wantHash` (`:655`) and `wantHash` never saw the motd.
- No proxy is stale, so `DecideRollout` orders nothing.
- There is no config-reload path: `proto/` and `internal/agentserver/` carry no
  reload message.

**So a changed `motd` never reaches a running proxy.** It takes an unrelated
restart, and nothing reports the gap. Shipped since 4c-2.

The fix is to include the rendered config values in `DesiredProxyHash`, which
is exactly what `DesiredServerHash` does for the server side, so the two stay
siblings rather than diverging on the very point that motivated the change.

**The price, stated plainly: widening the hash changes its value for every
existing proxy, so the first reconcile after this upgrade rolls every proxy
group once.** Unlike §3.6's case there is no adopt-on-empty escape, because the
label is present and merely different — telling "widened hash" apart from "the
image really changed" is not possible from the value alone. The rollout is at
least controlled: it goes through 4c-2's own surge-1, one-at-a-time path, which
exists for exactly this. It belongs in the release notes, and in the evidence
run's expectations so nobody reads it as a fault.

## 4. Storage growth: one field, upward only

5a's rule was: the claim is **created, never written**. 5b breaks it, narrowly
enough that the precise sentence still holds — *one field, upward only* — and
the load-bearing half of the rule is untouched.

**That distinction is the argument for this section.** The verb that destroys a
world is `delete`. `update` was the bonus. Growth costs the bonus and leaves
the guard.

**The `Server` controller patches**, because it already fetches the claim and
already holds the group. When `group.spec.storage.size` exceeds the claim's
`spec.resources.requests.storage`, it patches the request upward. It can never
patch downward: the CEL rule at `api/v1alpha1/servergroup_types.go:112` refuses
a shrinking spec and the API refuses a shrinking claim. A claim *larger* than
the spec — someone grew it by hand — is left alone and is **not** reported as
divergence; it is not the operator's to heal.

**The restart happens only when the driver asks for it.** Most CSI drivers
expand online and the pod never notices. Only when the claim carries the
`FileSystemResizePending` condition does the driver require a restart. The
`Server` controller records that as `Server.status.storageResizePending`; the
group reads it through `ServerView` as it reads everything else, and nominates
the ordinal through **§2's one-ordinal budget**, as the fourth and lowest-priority
candidate class. The invariant stays one sentence.

**A storage class that cannot expand gets its own condition.**
`allowVolumeExpansion: false` is not an edge case — `kind`'s own default
`local-path` class is exactly that, as the 5a evidence run's `kubectl get
storageclass` output shows. The API rejects the patch. That must not settle
into the reconcile log; it gets a dedicated condition on the `ServerGroup`,
kept separate from `Degraded` so that "your storage cannot grow" and "your
servers will not start" do not collapse into one field, plus an event naming
the storage class.

**RBAC gains `patch` and nothing else:**
`persistentvolumeclaims: create;get;list;watch;patch`. **`delete` stays out, and
so does `update`** — `patch` is sufficient for one field, where `update`
replaces the whole object and is more authority than the purpose needs.
`internal/rbacaudit/required.go` continues to compare the generated role against
its hand-maintained table in both directions, so a `delete` added later turns
the suite red before it can ship.

This is the only part of 5b that grants the operator new write authority, and
it should read that way in the record rather than being folded into the rest.

## 5. The failure streak: from "someone is healthy" to "the group recovered"

### 5.1 A correction to 5a's handover, because it changes the fix

`docs/handover-milestone-5.md` says a sibling that "blips readiness once" can
keep `Degraded` from ever arriving. That is not what happens. A single blip
resets the streak exactly once, and failures after it count normally —
`backoff.go:43-49` argues the rule against precisely that objection, correctly.

The real mechanism is a **comparison of rates**, which the handover does not
name. `lastSuccess` is the maximum `ReadySince` across all views
(`backoff.go:51-56`). A sibling that repeatedly loses and regains readiness
advances that maximum each time. At `replicas: 1` a failure arrives roughly
every `failedRetentionSeconds` plus the startup deadline — an hour and some. A
sibling flapping more often than that resets the counter more often than it
increments, and six is never reached. Not one blip: a blip that recurs faster
than the failure does.

### 5.2 The fix

**For a persistent group the reset is gated on every required ordinal having a
ready server, and `lastSuccess` becomes the maximum `ReadySince` over those
ordinals.**

The reset condition changes from *some server has been ready since the last
counted failure* to *every ordinal below `replicas` has a ready server, and the
last of them became ready since the last counted failure*. An ordinal with no
ready server — broken, being rebuilt, or not yet created — fails the gate
outright and the count keeps running. A permanently broken `survival-0` can no
longer be bought out by a neighbour, however often that neighbour flaps.

**The gate is what defeats the flapping sibling; the maximum only says *when*
the group recovered.** Separating the two is the correction this section
needed. It first asked for the minimum, on the reasoning that "the group has
not recovered until its slowest required ordinal has" — true about readiness,
but the minimum does not yield the moment of recovery. `ReadySince` does not
advance for an ordinal that never restarts, so the minimum is *when the
longest-running ordinal started*, which precedes every later failure. The
maximum is when the group finished recovering, and that is what the streak
breaks on.

Four consequences, all deliberate:

- **No new status field.** `ServerView` already carries `Ordinal`, `ReadySince`
  and `FailedAt`. This is why the narrowed reset is preferable to the full
  per-ordinal streak: it produces the behaviour `Degraded` is supposed to
  report without changing what `status.consecutiveFailures` means for either
  kind of group.
- **Ephemeral groups keep the maximum over all views, unchanged.**
  Interchangeability is the point there and the existing rule is right for it.
  The difference is decided by the group's type, not by new configuration.
- **A scale-up briefly delays the reset**, because the new ordinal has no ready
  server yet. That is the correct answer — the group is not recovered at that
  moment — but it is named here so that nobody later reads it as a defect.
- **A group in steady state resets on every recovery, and it has to.** This is
  the consequence the minimum got wrong rather than deliberately accepted, and
  it is written down because the shape recurs: an ordinal that stays up is
  invisible to a rule that reads timestamps, and reads as older than everything
  else. Under the minimum a group of two or more where one ordinal never
  restarts had no reset path at all — six isolated failures, each fully
  recovered before the next arrived, still reached `backoffGiveUpAt`, and only
  a spec edit cleared it. At `replicas: 1` the minimum worked by accident,
  because the only required ordinal is the one that was recreated.

### 5.3 The test that could not have shown this

`TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp` runs a single ordinal
and therefore cannot exhibit the defect at all. A second ordinal in it is the
first thing written, not an addition afterwards: it is what makes the rule
falsifiable.

## 6. What the tests must show, and what they cannot

**Pure rules, tabled without a cluster.** The nomination rule stays a value
function beside `DecidePersistentSize` and is tabled the same way. One thing
about that improves on 5a: the review of that milestone recorded the mutation
reversing the highest-first sort as *dead by construction*, because with every
surplus ordinal removed in one pass the order had no observable effect on any
object. **§2's one-ordinal budget makes that mutation live** — which ordinal
goes first is now a statement about the outcome, and the rule finally gets the
test it could not have in 5a.

**The hash needs a discrimination table, and it is the point of §3.** `image`
changes it; `maxPlayers` changes it, through the config half; **`replicas` does
not**; **`drain.timeoutSeconds` does not**. Plus the determinism assertion from
§3.4, across repeated runs rather than within one.

**The proxy side gets the mirror of that table**, and one row of it is the
whole of §3.7: a changed `motd` must change `DesiredProxyHash`. That test fails
today, before any code is written, which is the cleanest possible statement of
the defect. `playerLimit` must change it too — it already does, and the row is
there so that a later refactor moving the env var out of the pod cannot break
it silently.

**And §3.6's migration needs its own case, because it is the one an upgrade
gets wrong once and irreversibly:** a server with an empty `spec.podHash` is
adopted and stamped, and **no takedown is ordered**. Asserting only that the
field gets filled would pass while every world restarted.

**envtest, with 5a's traps carried forward.** There is no scheduler, no
kubelet, no provisioner and no garbage collector, and a deleted
`PersistentVolumeClaim` keeps answering `Get` indefinitely because
`StorageObjectInUseProtection` stamps `kubernetes.io/pvc-protection` and nothing
removes it — the `f.claim` fixture treats a deletion timestamp as gone, and
that rule applies here unchanged. §2's invariant is the central envtest case:
two ordinals, a spec change, and an assertion across the whole sequence that two
are never down at once.

**Two limits, named here rather than discovered during the evidence run.**
`FileSystemResizePending` is set by the external resizer, which envtest does not
run, so the condition is written onto the claim by hand in that test. More
seriously, **the positive half of storage growth cannot be shown on the cluster
shape 5a used at all**: `kind`'s `local-path` reports
`allowVolumeExpansion: false`. That cuts two ways, and both belong in the
runbook:

- The **negative** path comes free and is worth having: the default cluster
  demonstrates, with no extra setup, that the "this storage class cannot grow"
  condition appears when it should.
- The **positive** path needs a driver that supports expansion —
  `csi-driver-host-path` does. That is extra setup, and it belongs in a section
  of the runbook a driver may skip rather than in the main sequence.

**Mutation testing, under the rule 5a sharpened:** a mutant dies only to a test
that both executes the mutated line *and* asserts the property that line
corrupts. Four mutations are required to die, and the pairing of the first two
is the point: **removing Gate A** must fail an assertion that two ordinals are
never down at once, and **removing Gate B** must fail a *different* one — that a
stale nomination waits for the previous replacement to be `Ready`, not merely
for its object to be gone. A test that only pins the first would let Gate B be
deleted outright. The other two are **reversing the surplus sort order**, which
§2's budget makes observable for the first time, and **reversing maximum to
minimum** in §5 — which needs a case where every required ordinal is ready and
the two timestamps disagree, since that is the only shape in which the choice
is made at all.

Gate B carries one more obligation, because §2.1's own argument creates it: a
case must assert that a **surplus** nomination proceeds while an ordinal below
`replicas` is *not* ready. Without it, someone later "tightens" the rule by
applying Gate B to surplus as well, every test still passes, and a scale-down
issued to relieve a broken group silently stops working.

## 7. CRD changes

Two field additions and one condition, and nothing else. **§3.7's `motd` fix
needs no CRD change at all** — the field already exists and is already
validated; only what the hash reads changes:

- `Server.spec.podHash` (§3.5)
- `Server.status.storageResizePending` (§4)
- a `ServerGroup` condition for a blocked resize (§4)

**The CEL rule forbidding `spec.update` on a `Persistent` group stays.** §1
gives the reason: one-at-a-time is fixed rather than configurable, and exposing
`UpdateSpec` would expose `maxStaleSeconds` along with it, which has no meaning
on this path.

## 8. What 5c finds in place

- **A working recreate mechanism for one ordinal at a time**, which secret
  rotation can drive rather than rebuild. 5c's rotation is another occasion to
  take an ordinal down; §2's rule is where a fifth candidate class would go,
  and the priority order is where it has to be argued.
- **A hash that already covers the forwarding secret's *name*** (`net.Spec.ForwardingSecretRef.Name`,
  read at `internal/podspec/server.go:285`), but **not its contents**. Pointing
  the network at a differently-named secret is already a spec change 5b will act
  on; rotating the value *inside* one secret is invisible to §3's hash by
  construction, and closing that is exactly 5c's problem.
- **Write authority over claims, limited to `patch`**, and an RBAC audit that
  will turn red if 5c widens it without saying so.
- **A streak that reaches `Degraded` for a persistent group**, so that a stall
  5c introduces will be visible rather than silent.
