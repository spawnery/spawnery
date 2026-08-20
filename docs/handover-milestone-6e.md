# Handover to the RKE2 rollout

Status: **end of milestone 6e (2026-08-20). GitHub Actions blocks four jobs
on every pull request — `test`, `lint`, `deps`, `e2e` — runs a nightly
reproducibility check, and holds a release workflow ready for a `v*` tag
nobody has pushed. The milestone's own first cold-cache CI run found
`make lint` was never actually green: five real `SA1019` findings that three
local runs and two reviewers had all missed, hidden by a stale
`golangci-lint` cache no runner carries. The count this milestone's own
design spec and its own Task 2 commit message call final, 33, was measured
against that same stale cache and is wrong; the true figure, from a cleared
cache, is 38.**

That correction is the milestone, more than any workflow file is. Four
things follow from it, and this document says each one where it belongs
rather than only once: fixing the five findings forced a real migration off
a deprecated recorder API, twenty-three call sites and twenty-one fake
recorders; that migration needed a new RBAC grant that nothing but a real
`make e2e` run under the operator's own ServiceAccount could ever have
proven wired correctly, since `internal/rbacaudit` checks role against
table and envtest grants its test client everything; the new recorder's
sink enforces a 1024-byte note limit the old one never had, which was fixed
at five sites and is recorded open at a sixth; and the truncation cap
`golangci-lint` applies by default, separately from the cache problem,
undercounted the same tree at seventeen findings where explicit flags
showed thirty-three (itself later corrected to thirty-eight) — a design-time
note claiming the undercounted subset varied from run to run was checked
again by a reviewer and did not reproduce.

This document is not a spec. It says where 6e stopped and what the RKE2
rollout — the last thing milestone 6 owes — finds when it starts, checked
against the code as 6e leaves it rather than against the plan that preceded
it. The design decisions live in
[`docs/superpowers/specs/2026-08-19-ci-design.md`](superpowers/specs/2026-08-19-ci-design.md);
the open points are in [`docs/known-issues.md`](known-issues.md), whose new
"From milestone 6e" section this document does not repeat in full.

**Why this is a new document rather than a section appended to
[`handover-milestone-6d.md`](handover-milestone-6d.md).** That document was
written *for* 6e, and its §3 ("What 6e finds in place") is the record of
what 6e started from — checked against the tree as 6e now leaves it, and all
six of its claims still hold, unchanged, because nothing in this milestone's
scope touched the chart's namespace templating, the three-namespace scheme,
`config/rbac/forwarding-secret-reader.yaml`, or `hack/chart-templates.sh`.
Rewriting that section would delete the evidence base for what 6e found
already true; leaving it in present tense inside a document a rollout reader
opens would misstate nothing, since nothing in it needed correcting — but
the pattern the last four milestones have all followed is to hand off with a
fresh document regardless, so that "what the next reader finds in place" is
always a survey of the tree, not a diff against a survey two milestones old.
`handover-milestone-6d.md` now carries a header saying it is superseded, in
the same pattern `handover-milestone-6c.md` and its own predecessors carry.

## 1. Where 6e stopped

**Built and driven, task by task:**

- **One job, and the number that decided against a cache** (`03d8383..80ddc32`).
  `.github/workflows/ci.yml`'s `test` job: `actions/checkout@v4`,
  `cachix/install-nix-action@v31` (the brief's own `v27` pin was four majors
  stale, checked against the action's own GitHub releases), a timed
  `nix develop -c true`, then `nix develop -c make test`. On a cold hosted
  runner the dev shell was ready in **25s** against a **338s** total job
  (about 7%) — run
  [`32307146517`](https://github.com/spawnery/spawnery/actions/runs/32307146517),
  job `test` in 5m38s, reproduced at 5m49s on
  [`32307721048`](https://github.com/spawnery/spawnery/actions/runs/32307721048)
  after a message-only amend. Both green on the first attempt. Per the
  design's own "under two minutes on a job that runs fifteen is noise" bar,
  25s against 338s is an easier call than that example, and no
  `actions/cache` was added — there is nothing to compare a cached run
  against, because none was built.
- **`.golangci.yml`, and the 26 unchecked returns** (`80ddc32..168b839`).
  `errcheck` and `staticcheck`, `max-issues-per-linter: 0`,
  `max-same-issues: 0` — golangci-lint 2.12.2. Fixed all 26 `errcheck`
  findings; three bare `defer x.Close()` calls were read in full call-chain
  context and left as `_ = x.Close()` rather than instrumented, because
  every write in this codebase reaches the wire in one `Write` call with no
  buffering layer between the framing code and the socket, so a `Close()`
  error here reports on connection state, never on lost data. The brief's
  own Step 5 mutation (a bare `fmt.Fprintf(os.Stderr, ...)`) produced no new
  finding — `errcheck` does not flag a direct `os.Stderr` write by default,
  which is also why the original 26 all went through an `io.Writer`
  parameter and never the identifier itself; the corrected mutation, through
  a function's own `stderr` parameter, produced the expected 8th finding and
  was reverted.
- **The seven `staticcheck` findings** (`168b839..8ca89d5`). Four `QF1008`s
  (a redundant `.Time` selector on `metav1.Time`), one `SA4006` (a dead
  local whose only value was its side effect, confirmed by tracing every
  read between its declaration and its overwrite), one `QF1001` (De Morgan's
  law applied to `proxyOccupied`'s guard, a genuine judgement call between
  two readings the function's own doc comment supports, resolved toward the
  disjunctive form and documented rather than silenced), and one `SA1019` on
  `scheme.Builder` — kept and silenced with this milestone's one
  `//nolint:staticcheck` (`api/v1alpha1/groupversion_info.go:47`), reasoned
  in the surrounding comment: migrating to apimachinery's own
  `runtime.SchemeBuilder` would touch all four `api/v1alpha1` type files
  outside this task's scope, for no functional motivation. `make lint`
  reported `0 issues.` — genuinely, for the six files this task touched;
  what it did not yet know about is next.
- **The finding that reopened this task** (`3cfb575` onward, on top of
  `168b839..8ca89d5`). Task 4's first CI run of the new `lint` job — a
  runner with no `golangci-lint` cache — found five real `SA1019` findings
  in `internal/controller/setup.go` (`mgr.GetEventRecorderFor`, deprecated),
  present since milestone 6d's `722d9e1` and touched by no commit in this
  milestone. Three local runs and two reviewers had reported `make lint`
  clean; `golangci-lint cache clean` followed by a plain run reproduced the
  five findings locally, confirming the cache — not the tree — had been
  answering "clean." The same cache-clearing also corrected the milestone's
  own count: 33, as Task 2 measured and committed it, was itself measured
  against a stale cache; the true count, from a `git worktree` checked out
  at the tree just before Task 2's fix, cache cleared, was **38** (26
  `errcheck`, 12 `staticcheck` — the 7 this task fixed plus the 5 `setup.go`
  findings nobody had yet seen). A fix round was dispatched and correctly
  stopped short of a silent migration: `GetEventRecorderFor` returns
  `record.EventRecorder` (3 methods), the named replacement
  `GetEventRecorder` returns `events.EventRecorder` (1 method, different
  shape) — not a same-shape rename, and not this task's decision to make
  unilaterally.
- **The `lint` and `deps` jobs, and a sandbox that was never the problem**
  (`3cfb575..8e8f7ae`, with four superseded detours in between still visible
  in `git log`). `deps` runs `make agent-deps` against a real Maven Central,
  then `git diff --exit-code -- agent/deps.json`; the brief's own path,
  `agent/paper/deps.json`, has been `agent/deps.json` since milestone 6d's
  `14eee4f`. The job's first real run failed inside `make agent-deps` itself
  with `bwrap: setting up uid map: Permission denied` — two attempts at
  Nix's own sandbox setting (`sandbox = false`; a single-user Nix install)
  changed nothing, because Nix's sandbox was never the thing failing.
  `nix show-derivation` on the failing `.drv` showed the real cause: the
  nixpkgs `gradle.fetchDeps` update script wraps *itself* in an independent
  `bwrap --unshare-all` call, unrelated to Nix's sandbox, on by default,
  gated by an env var the script itself defines. `USE_BWRAP: "0"` on the
  `make agent-deps` step fixed it on the first try. The guard was then shown
  to fire, not only wired: a corrupted hash in `agent/deps.json`
  (`b4efe0f`) reproduced through a real Gradle resolution (`BUILD
  SUCCESSFUL`) and the comparison step failed naming the file and the exact
  diff — run
  [`32313750063`](https://github.com/spawnery/spawnery/actions/runs/32313750063),
  job `deps` #96261773008; the revert (`a07d342`) went green again on
  [`32313926601`](https://github.com/spawnery/spawnery/actions/runs/32313926601).
- **The migration this lint fix forced** (`21d1d20`, with a fix round
  `21d1d20..6a854e3`). `internal/controller`'s five `Recorder` fields moved
  from `record.EventRecorder` to `events.EventRecorder` — twenty-three
  production `.Event`/`.Eventf` call sites (not the twenty-four first
  guessed; one of the six sites originally thought to need translating was
  already an `Eventf` with a latent bare-format-string bug, fixed in
  passing) and twenty-one `record.NewFakeRecorder` constructions across
  eight test files. `internal/controller/events.go` (new, no licence
  header — an oversight, unlike 16 of the package's other 18 production
  files) fixes an `action` value at every call site by one rule: a
  subordinate-object mutation names its verb and kind, everything else is
  `actionSyncStatus`. The new sink is `events.k8s.io/v1`, needing its own
  RBAC grant (`config/rbac/role.yaml`, `charts/spawnery/templates/rbac.yaml`,
  `internal/rbacaudit/required.go:57-58`); the old core-group grant stays,
  because controller-runtime's own leader-election lock still uses the
  deprecated recorder internally. The fix round closed an Important finding
  the migration itself introduced: `events.k8s.io/v1` refuses a note over
  1024 bytes, measured against envtest's real API server to be a byte, not
  character, count (512 em-dashes — 512 characters, 1536 bytes — refused).
  `eventNote()` (`internal/controller/events.go:125`) formats, truncates on
  a rune boundary, and appends a marker pointing at the full text on the
  object's condition; applied at five of the sites that build a note from
  runtime text
  (`proxygroup_controller.go:297`, `:1131`, `:1605`; `server_controller.go:287`;
  `network_controller.go:160`). `internal/controller/servergroup_controller.go:437`
  is the same shape — `resize.Message`, traced through
  `storageResizeCondition` → `worst.ResizeError` →
  `resizeConditionError` (`server_controller.go:471`) to a
  `PersistentVolumeClaimControllerResizeError`/`NodeResizeError` message an
  external CSI driver writes — and is **not fixed**; see §2.
- **The `e2e` job, on somebody else's container runtime** (`6c2a6cc`). Copies
  the other jobs' `checkout`→`Install Nix` shape, one step,
  `nix develop -c make e2e`, 45-minute timeout. `hack/e2e.sh` needed **no
  change** to run on a hosted runner's Docker daemon — the design's own open
  assumption, now measured. Green on the first attempt: run
  [`32332616823`](https://github.com/spawnery/spawnery/actions/runs/32332616823),
  job `e2e` (96316013283) in 7m00s, eighteen scenarios,
  `theOperatorWasNeverDenied` last and passing. This is also the only real
  evidence anywhere that the new `events.k8s.io/v1` grant above is
  sufficient — see §2 for exactly how far that evidence reaches, and for
  the grep this task's own report first offered and then withdrew.
- **The nightly and the release workflow** (`af976fd`, `4155c7d`).
  `.github/workflows/nightly.yml` runs `make image-repro` — Paper, Velocity
  and the operator image built twice each, checking milestone 6a's
  bit-identical-rebuild criterion continuously rather than once — on a
  `17 3 * * *` cron plus `workflow_dispatch`.
  `.github/workflows/release.yml` authenticates `skopeo` to `ghcr.io` and
  runs `hack/publish.sh` with `WRITE_DIGEST=1` on a `v*` tag push, then
  fails loudly if no digest landed in `charts/spawnery/values.yaml`.
  `workflow_dispatch` cannot fire on a workflow absent from the default
  branch, so `nightly.yml` carried a temporary `pull_request:` trigger for
  one push, driving run
  [`32333966209`](https://github.com/spawnery/spawnery/actions/runs/32333966209) —
  green, `image-repro`, **9m21s** (`05:00:37Z`–`05:09:58Z`) — before the
  trigger was removed in the final commit. See §2 for what that means for
  the file that actually merges.

**Verified at the end of the milestone:** `nix --extra-experimental-features
'nix-command flakes' develop -c make test` green (§7 has the Step 7
verification run, including a `golangci-lint cache clean` confirming `make
lint` is genuinely 0 from a cleared cache at the tip of this branch, not
only from a run that happened not to need clearing). The last driven `CI`
run on PR #1,
[`32334756738`](https://github.com/spawnery/spawnery/actions/runs/32334756738),
all four jobs green: `deps` 1m32s, `lint` 2m4s, `test` 5m19s, `e2e` 7m14s
(`gh api .../jobs`, timestamps `05:13:04Z` start for all four,
`05:14:36Z`/`05:15:08Z`/`05:18:23Z`/`05:20:18Z` finish respectively).

**Not driven, and not drivable here:** `release.yml`'s real publish path,
end to end — see §2. `nightly.yml`'s actual merge-shape triggers,
`workflow_dispatch` and `schedule` — see §2. And, unchanged since it was
first stated in milestone 6a's own handover, whether a client can reach
anything through any expose strategy on a real CNI; nothing in 6e touches
that question.

## 2. What the RKE2 rollout must not misread

**"33" is wrong wherever it still appears, and the corrected number is 38,
not a rounder guess.** `docs/superpowers/specs/2026-08-19-ci-design.md` and
this milestone's own `168b839` commit message both assert 33 as the real
count of pre-existing `golangci-lint` findings. Both were measured against a
`golangci-lint` cache warmed before the five `setup.go` `SA1019` findings
existed. `golangci-lint cache clean` followed by a plain run, against the
tree as it stood just before that commit, reports 38 (26 `errcheck`, 12
`staticcheck`). Per this project's own rule, the design spec is a living
reference and may be corrected in place with a visible marker the way
`docs/superpowers/specs/2026-08-17-network-policies-design.md` §2.4 already
does this; the commit message cannot be edited, and stands as the record of
what was believed at the time, corrected here and in
`docs/known-issues.md`'s new "From milestone 6e" section.

**The varying-subset claim about the truncation cap was observed once and
did not reproduce — do not read it as established.** Three runs during this
milestone's design each reported seventeen issues from a plain,
unconfigured `golangci-lint run`, and a note recorded at the time claimed
each of the three named a different set of files. A reviewer later ran the
same command three consecutive times against the same pre-fix tree and got
the *identical* seventeen each time. The cap itself is real and confirmed
twice over — seventeen against a true count of thirty-eight is the same
order of undercount either way — but the specific claim that the sampled
subset itself varies run to run is, as of this milestone, a single
unreplicated observation. `docs/known-issues.md` says exactly that; nothing
this milestone wrote should be read as saying more.

**The grep for `is forbidden:` over the CI job log is not evidence for
anything, and should not be cited as if it were.** An earlier draft of
Task 5's own report offered a zero-match grep of the `e2e` job's raw stdout
as independent corroboration that the new `events.k8s.io/v1` RBAC grant is
sufficient. It is not: `hack/e2e.sh`'s pod-log dump runs only when the
job's own exit status is non-zero, and the check's own log source,
`operatorLog()` (`test/e2e/e2e_test.go:272-310`), reads the operator's
container log through the Kubernetes API in-process and never prints it —
so on a green run, the corpus that grep searches structurally cannot
contain the operator's log at all, and a zero-match result against it says
nothing about the grant either way. The real evidence is narrower and
already stated in full in §1 and in `docs/known-issues.md`: the scenario
ran, ran last, and passed, which is genuine evidence and the *only*
evidence anywhere in this repository that the grant is sufficient — reaching
exactly as far as `theOperatorWasNeverDenied`'s own design allows. That
check excludes any line containing `violates PodSecurity` (milestone 6c's
own narrowing), and two paths can carry a real denial without ever
producing an `is forbidden:` line: a revoked cache-backed read (tried
against pods and networks, watched for close to eight minutes, silent both
times) and `readForwardingSecret` folding a `403` into a condition message
with no `is forbidden:` substring and no log call at all.

**`internal/controller/servergroup_controller.go:437` is an open gap in the
same class the events-note fix closed elsewhere, not a closed one.** Five
sites that build an event note from runtime text now go through
`eventNote()`; this one — `resize.Message`, ultimately a
`PersistentVolumeClaimControllerResizeError`/`NodeResizeError` message an
external CSI driver writes — was found after the fix round and after both
of its reviews, and is not wired to the helper. A single `eventNote()` call
at that site closes it; nobody has made that call yet.

**Nothing new here changes what milestone 6c and 6d already said about
reachability, NetworkPolicy enforcement, or `HostPort` under CIS
`restricted`.** 6e's whole scope is CI wiring, lint, and the events-API
migration it forced; it neither touched nor re-measured any of that. Read
§4 below, and `docs/handover-milestone-6d.md`'s own §2, as still current in
those respects.

## 3. What the next milestone finds in place

- **`.golangci.yml`** enables exactly `errcheck` and `staticcheck` and pins
  two keys under `issues`: `max-issues-per-linter: 0` and
  `max-same-issues: 0`. Both are load-bearing, not belt-and-braces —
  `max-same-issues` is the one that actually mattered here: golangci-lint's
  own default (3) is what turned a real 38 findings into an apparent 17, and
  `max-issues-per-linter`'s own default (50) would not by itself have hidden
  anything at this tree's scale, but is pinned anyway so a future linter
  with many findings sharing few messages cannot reintroduce the same
  failure mode from the other direction. The file's own header comment
  states the varying-subset claim as settled fact ("each named a different
  set of files"); §2 above and `docs/known-issues.md` are the corrected
  version — the comment itself is outside this task's file list and was
  left as written.
- **The one `//nolint` in the tree** is
  `api/v1alpha1/groupversion_info.go:47`,
  `//nolint:staticcheck // deprecated scheme.Builder; see above`. `nolintlint`
  is not enabled, so a bare `//nolint` on that line would have silenced
  everything golangci-lint could ever report there — confirmed by mutation
  during Task 3 (a bare `//nolint` still left `make lint` at `0 issues.`) —
  which is exactly why this one names its linter and carries a reason
  rather than standing bare.
- **`internal/controller/events.go`** is where an `action` value and an
  event note both get decided, by rule rather than by precedent: a
  subordinate-object mutation names `<Verb><Kind>`, everything else is
  `actionSyncStatus`; a note built from runtime text goes through
  `eventNote()` (`:125`), which formats first, truncates to 1024 bytes on a
  rune boundary, and appends a marker pointing at the full text on the
  object's own condition. `action` is invisible to every test in this
  repository — both fake recorders format only `eventtype + reason + note` —
  so none of the nine action constants is covered by an assertion; that is
  by design; see the file's own comment for why.
- **RBAC gained one grant.** `events.k8s.io`/`events`, `create` and `patch`
  (`internal/controller/server_controller.go:109`, cluster-scoped, no
  `namespace=` marker — a ClusterRole rule, distinct from the two existing
  `namespace=spawnery-system` placeholder markers on `internal/certs/store.go:62`
  and `internal/controller/setup.go:77`, which are unaffected). The core
  `events`/`create`/`patch` grant stays for controller-runtime's own leader
  election. `internal/rbacaudit/required.go:42-56`'s own comment states
  plainly what covers this and what does not: role-versus-table drift, yes;
  a wrong table next to a matching wrong role, no — "which is what the
  cluster-level end-to-end test is for."
- **`Makefile`'s `lint` target is unchanged** — `golangci-lint run`, nothing
  else — and `USE_BWRAP: "0"` lives in `.github/workflows/ci.yml`'s `deps`
  job only, as a step-scoped env var, not in the `Makefile`'s `agent-deps`
  target itself; a local `make agent-deps` run is unaffected and still uses
  whatever `USE_BWRAP` default the caller's own environment has.
- **`docs/known-issues.md` gained a "From milestone 6e" section** — the
  stale-cache lesson generalised past `golangci-lint`, the truncation cap,
  the RBAC grant and how far its one piece of evidence reaches, the
  1024-byte note cap and its open sixth site, the now-unexercised
  rootless-podman path, and the two workflow paths that exist only on
  paper. It also closes the milestone-2c `deps.json` guard entry and states
  plainly that the neighbouring toolchain-version-coupling entry is *not*
  closed by this milestone.
- **`release.yml` is not a registered workflow.** `gh api
  repos/spawnery/spawnery/actions/workflows` lists only `CI` and `Nightly` —
  GitHub registers a workflow once something triggers it or it reaches the
  default branch, and `release.yml` has done neither. Its `skopeo login`,
  its real `hack/publish.sh` invocation with `WRITE_DIGEST=1`, and its
  digest-guard step have all run zero times. The `WRITE_DIGEST` branch of
  `hack/publish.sh` itself has never run against a real registry anywhere in
  this project's history — milestone 6d only exercised the substitution
  against scratch copies of `charts/spawnery/values.yaml`.
- **`nightly.yml`'s merge shape has never run.** The commit that removed the
  temporary `pull_request:` trigger (`4155c7d`) is the version on this
  branch; the version that actually produced the one green `image-repro` run
  differs by exactly that trigger line. `workflow_dispatch` and `schedule`
  are both, as of this document, unexercised.

## 4. What the RKE2 rollout owes

Carried forward from `docs/handover-milestone-6d.md` §4 unchanged — nothing
in 6e's own scope touched any of it:

`docs/handover-milestone-6c.md` §4 stands unchanged in what it listed: CIS
`restricted` pod security and `HostPort` cannot both hold in one namespace,
and the runbook — not the code — has to choose between a relaxed-label
namespace for the `HostPort` `ProxyGroup` or dropping the `HostPort` leg of
the rollout. 6d added one item to that list:
**`config/rbac/forwarding-secret-reader.yaml:65` names
`namespace: spawnery-system` in its RoleBinding subject, and the chart
cannot template it (design §9).** The file is applied per game namespace, by
hand, after the chart is installed. An operator installed in any namespace
other than `spawnery-system` must have that line changed first, or the
`Network` in every such game namespace loses forwarding-secret rotation
detection silently — `docs/handover-milestone-6d.md` §2 has the narrower,
code-verified consequence, which is not what the design document states.
`charts/spawnery/README.md` carries this in its installation steps.

6e adds nothing to this list. Its own new gap — the rootless-podman path
being unexercised by anything automatic — is a CI-coverage question, not
something the RKE2 rollout itself has to decide or work around; it is
recorded in `docs/known-issues.md`'s new section instead.

## 5. Every finding this milestone's reviews produced

The SDD ledger (`.superpowers/sdd/2026-08-19-ci/progress.md`) is the only
place this list exists in full; it is restated here with what caught each
one, task by task in the order the ledger records them.

**Task 1 — one job, and the cache decision.**
1. Minor, deferred. `ci.yml`'s timeout comment says 25 minutes is "roughly
   double what it takes on the author's machine," implying about 12.5
   minutes locally — nothing measured that; the 25-minute value itself
   gives about 4.3x margin over the observed 5m49s. Caught by review
   reading; left as written.
2. Minor, deferred, carried forward as an instruction to Tasks 4 and 5. The
   "Warm the dev shell" timing step exists only to produce Task 1's own
   cache measurement; copying it into later jobs would add three useless
   25s steps. Caught by review reading. Heeded: neither Task 4's `lint`/`deps`
   jobs nor Task 5's `e2e` job carry it.
3. Complete, review clean. Reviewer reproduced every figure from `gh api`
   directly and confirmed the post-amend tree (run `32307721048`) was
   itself what ran, not merely a message change over an unverified tree.

**Task 2 — `.golangci.yml`, and the 26 unchecked returns.**
4. FINDING against the plan's own text. The brief's Step 5 mutation cannot
   fire: `errcheck` never flags a direct `os.Stdout`/`os.Stderr` write by
   default. The task substituted a mutation through a function's `stderr
   io.Writer` parameter instead, which did produce a 34th (then, before the
   cold-cache correction) finding; this also explains why the original 26
   findings existed at all — every `cmd/*` binary's `run(args, stdout,
   stderr io.Writer)` shape means every `Fprintf` reaches errcheck through a
   parameter, never through the `os.Stderr` identifier. Caught by mutation
   testing exposing that the brief's own prescribed mutation was inert;
   reviewer reproduced both halves.
5. WORDING, load-bearing for this document. The truncation cap is
   confirmed (plain run 17, explicit flags 33 as then measured), but a
   reviewer's three consecutive pre-fix runs each returned the *same*
   seventeen, not a varying subset — the "different files each run"
   observation is a design-time note that did not reproduce when checked
   again. See §2 above for the corrected statement.
6. Complete, review clean. Reviewer reproduced the pre-fix 26 findings and
   all three of the production `Close()` call-chain analyses independently.

**Task 3 — the seven `staticcheck` findings.**
7. Minor, deferred. The `QF1001` at `proxygroup_controller.go:490`'s De
   Morgan expansion is, per the reviewer's own independent reading, "a
   toss-up leaning slightly toward the negated form" — the implementer's
   choice is defensible and documented, not silently picked. Caught by
   review reading.
8. Minor, deferred, for a later milestone. `nolintlint` is not enabled, so
   a bare `//nolint` would silence everything on its line; confirmed by
   Task 3's own Mutation 2. The one `//nolint` that ships names its linter
   and carries a reason, so the risk is documented rather than live. Caught
   by mutation.
9. Complete, review clean. Reviewer re-derived all four of Step 5's
   citation items, including the `SA1019` docstring pulled from the module
   cache, and reproduced both Step 5 mutations.

**Task 4 — the `lint` and `deps` jobs.**
10. FINDING that invalidated Task 3's own "complete." `make lint` was never
    actually green — see the Status block above and §1's own account. This
    is the headline finding of the milestone. Caught by CI itself: a runner
    with no `golangci-lint` cache to answer from, on its very first `lint`
    run.
11. FINDING against the plan's own text. The brief names
    `agent/paper/deps.json`; the real path has been `agent/deps.json` since
    milestone 6d's `14eee4f`. The task used the current path. Caught by
    checking the brief's claim directly against the repository rather than
    trusting it.
12. The `deps` job needed an undocumented environmental fix, `USE_BWRAP=0`,
    because nixpkgs' `gradle.fetchDeps` update script wraps itself in its
    own independent `bwrap` call, which fails under GitHub's default
    unprivileged-userns restriction. Two earlier attempts (Nix's own
    `sandbox = false`; a single-user Nix install) were confidently wrong
    diagnoses, left visible in `git log` rather than squashed away. Caught
    by iterating against real CI runs and reading the failing derivation's
    own builder script.
13. Task 3 reopened by finding 10; fix round dispatched for the five
    `setup.go` `SA1019` findings.
14. Fix round stopped correctly at the point of discovering `GetEventRecorderFor`
    (`record.EventRecorder`, 3 methods) and `GetEventRecorder`
    (`events.EventRecorder`, 1 method, different shape) are not a same-shape
    rename. Confirmed from a clean cache: 38 before Task 2, 5 remaining at
    that point. Caught by the implementer's own investigation, correctly
    declining to make the migration decision unilaterally.

**Task 3b — the events-API migration (new task, created from finding 10).**
15. New task, not in the original plan — the plan is a historical record
    and this work exists because of a finding, per this project's own rule.
    Surface: 5 `Recorder` field declarations, 23 production call sites (not
    the 24 first estimated), 21 fake-recorder constructions across 8 test
    files.
16. Minor, deferred. The report originally said six `.Event` sites became
    `Eventf`; there were five — the sixth
    (`proxygroup_controller.go:1627`) was already an `Eventf` with a bare
    format string, a pre-existing latent bug fixed in passing rather than a
    consequence of this migration. Caught by review reading.
17. Minor, deferred. The report said both shadowing `events` locals were
    renamed to `rec`; three remain, all in `network_controller_test.go`,
    each shadowing the freshly imported `events` package. Caught by review
    reading.
18. Minor, deferred. `proxygroup_controller.go:1059`'s `ProxyDrainTimeout`
    fires with action `actionDrainProxy` where the stated rule would derive
    `DeleteProxyPod`; the constant's own doc comment redefines `DrainProxy`
    to cover the delete, so the constant is right and the stated rule is
    imprecise. Caught by review reading.
19. Minor, deferred. `internal/controller/events.go` carries no Apache
    licence header, unlike 16 of the package's other 18 production files.
    Caught by review reading.
20. Minor, deferred. `OrphanReconciler.Recorder` (`orphan.go:47`) is a dead
    field — no `.Event`/`.Eventf` call anywhere in `orphan.go` — still fed
    by `setup.go:132`; one of the five original `SA1019` findings was on a
    construction site feeding a field nothing reads. Caught by the
    implementer's own reading, recorded as an observation rather than fixed.
21. FINDING, load-bearing for this document. The report's own "no test
    could catch a missing grant" was narrowed by review:
    `internal/rbacaudit` applies the rendered chart and runs real
    `SubjectAccessReview`s, so it *would* catch role-versus-table drift.
    What nothing catches is table-versus-code, since the table itself is
    hand-maintained. Caught by review reasoning; see §1 and §3 above for
    where this now lives in the code's own comments.
22. Important, fix round dispatched. `events.k8s.io/v1` caps a note at 1024
    bytes where the old core `v1.Event` did not, measured against envtest's
    real API server; five sites built notes from runtime text with nothing
    truncating, so a long refusal was dropped from `kubectl get events`
    entirely. Caught by review reading and reproduction.
23. Fix round 1/5, one finding addressed. Re-reviewer reproduced the
    byte-versus-character measurement, the rune-boundary backoff, the
    uncovered-site claim, and the real-API-server assertion independently.
24. FOR THE FINAL REVIEW, and still open. A sixth site of the same shape,
    missed by the fix round and by both of its reviews:
    `servergroup_controller.go:437`, `resize.Message` from an external CSI
    driver's condition. The re-reviewer verified the other seventeen
    candidate sites are genuinely literal or numeric-substitution-only.
    Caught by a later reading pass; see §2 above — not fixed as of this
    document.
25. Complete, review clean after one fix round. `make lint` 0 from a
    cleared cache, `make test` green, all five original `SA1019` findings
    gone.

**Task 4, continued — for this document specifically.**
26. `docs/known-issues.md:295-299` (pre-6e line numbers) still named
    `agent/paper/deps.json`, stale since milestone 6d's `14eee4f`, and the
    entry needed closing now that the guard exists. Caught by the ledger's
    own note for this task; both are done in §1's Step above.
27. Complete, review clean. Reviewer checked the raw job logs via `gh api`,
    confirmed the guard failed at `git diff --exit-code` and not inside
    `make agent-deps` itself, and confirmed the four superseded fix
    attempts left no residue in the final tree.

**Task 5 — the `e2e` job.**
28. `e2e` green on the first attempt — run `32332616823`, job `96316013283`,
    7m00s, 18/18 scenarios, `theOperatorWasNeverDenied` last and passing.
    `hack/e2e.sh` needed no change to run under a hosted runner's Docker —
    the design's own open assumption, now measured. Caught by driving the
    real run.
29. FINDING, load-bearing for this document. The report first offered a
    grep of the CI job log for `is forbidden:` as independent corroboration
    of the RBAC grant's sufficiency; it is a null result over a corpus that
    structurally cannot contain the thing searched for on a green run (§2).
    Caught by re-reading the report's own claim against `hack/e2e.sh`'s and
    `operatorLog()`'s actual code before the report shipped.
30. Fix round dispatched: correct the report's framing and state how far the
    real evidence reaches, given milestone 6c's `violates PodSecurity`
    exclusion and the two paths that can carry a denial without an `is
    forbidden:` line.
31. Fix round 1/5, one finding addressed, report-only (no code commit).
    Re-reviewer checked the two named uncovered paths against
    `test/e2e/e2e_test.go`'s own doc comment and spot-checked two of the
    report's five swept claims.
32. Complete, review clean after one fix round.

**Task 6 — the nightly, and the release.**
33. Complete, review clean. Nightly run `32333966209` green, 9m21s, with
    four "checking outputs of" lines in the log proving the rebuild
    comparison actually fired rather than short-circuiting; `release.yml`
    confirmed unregistered by GitHub and entirely unexercised (§1, §3).

No whole-branch review distinct from these task-level ones is recorded in
the ledger for this milestone — unlike milestones 6c and 6d, which each ran
one more review pass after all task reviews closed. This document's own
Step 7 verification pass (§7) is the closest thing 6e has to that pass, and
what it caught is recorded there rather than implied by its absence here.

## 6. The environment

Unchanged from 6d's own §6. Every command runs inside `nix develop`, and on
this machine that means the full flag, every time:

```bash
nix --extra-experimental-features 'nix-command flakes' develop -c make test
systemd-run --scope --user --property=Delegate=yes -- \
  nix --extra-experimental-features 'nix-command flakes' develop -c \
  env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" make e2e
```

- `make e2e` is part of neither `make test` nor `make all`, deliberately —
  unchanged by CI, which runs it as its own separate `e2e` job for the same
  reason: it must not queue behind, or be conflated with, the three faster
  checks.
- `kind` runs under rootless Podman here, which needs both
  `KIND_EXPERIMENTAL_PROVIDER=podman` and a systemd scope with
  `Delegate=yes`. CI needs neither: a hosted runner has a real Docker
  daemon, which is `kind`'s default — this is the one place this
  milestone's own workflow YAML and this section now diverge on purpose,
  and §2's podman-gap paragraph in `docs/known-issues.md` is why that
  divergence matters going forward.
- `TMPDIR` matters locally: the default `/tmp` is too small for an image
  archive here.
- The machine has 8 GB and no swap. Run one cluster at a time; `E2E_KEEP=1`
  leaves it standing and prints its `KUBECONFIG`.
- `golangci-lint`'s own cache is local-machine state, not something `nix
  develop` resets. `golangci-lint cache clean` before trusting any lint
  count by hand is now this milestone's own standing lesson, not merely
  advice — see `docs/known-issues.md`'s new section.
- Every image derivation takes the working tree as its source, so editing a
  file under `docs/` changes the operator image's derivation hash and makes
  the next `make e2e` rebuild it — slow, not wrong.

## 7. The Step 7 verification pass

Every file path, line number, run ID, duration and constant in this
document was checked against the repository or against `gh api` before it
shipped, per the brief's own instruction. What that pass caught, and fixed,
before this document reached its current form:

- **Five call-site line numbers had moved since Task 3b's own report was
  written**, because the fix round's edits shifted lines below them.
  `proxygroup_controller.go:295` is now `:297`; `:1129` is now `:1131`;
  `:1601` is now `:1605`; `server_controller.go:286` is now `:287`;
  `network_controller.go:158` is now `:160`. All five were re-grepped
  directly against the current tree (`grep -n "eventNote("`) rather than
  copied from the report, and the corrected numbers are what appears in §1
  and §3 above.
- **The sixth, unfixed site's line number was independently confirmed
  rather than trusted from the ledger**: `servergroup_controller.go:437`
  was re-grepped and matches exactly what the ledger's Task 3b entry claims,
  as does `resizeConditionError`'s definition at `server_controller.go:471`
  and its two call sites feeding `StorageResizeError`.
- **The final green CI run's four job durations were re-queried from `gh
  api` directly** (run `32334756738`) rather than copied from Task 6's own
  report, and matched it exactly: `deps` 1m32s, `lint` 2m4s, `test` 5m19s,
  `e2e` 7m14s, all starting `05:13:04Z`.
- **`release.yml`'s non-registration was re-confirmed directly** with `gh
  api repos/spawnery/spawnery/actions/workflows` rather than taken on Task
  6's report alone — it still lists only `CI` and `Nightly` (plus the
  built-in `Dependency Graph`).
- **`make lint` was re-run from a freshly cleared cache** at the tip of this
  branch, not merely cited from Task 3b's report: `golangci-lint cache
  clean && golangci-lint run` reports `0 issues.` today, confirming the
  claim in §1 is still true of the tree as this document leaves it, not
  only of the tree as Task 3b's own report described it.
- **Two of §5's citations named a stale line, and this pass re-grepped every
  such citation instead of copying it from the source report.**
  `proxygroup_controller.go:1623` (Task 3b's report's own citation for the
  sixth `%`-hazard site) is the `Message:` field assignment; the actual
  `Eventf` call carrying the pre-existing bare-format-string bug is three
  lines later, at `:1627` — corrected in finding 16 above.
  `proxygroup_controller.go:1058` (the ledger's own citation for the
  `ProxyDrainTimeout` finding) is off by one from the current tree; the
  `r.Recorder.Eventf` call is at `:1059` — corrected in finding 18 above.
  Both were re-grepped directly (`grep -n "actionDrainProxy\|ProxyDrainTimeout"`,
  `grep -n "proxyPodsAdmittedMessage"`) rather than trusted from the reports
  they came from, which is what caught the drift.
- **`operatorLog()`'s own line range was off by one.** Task 5's report and
  an earlier draft of §2 above both cite `test/e2e/e2e_test.go:272-309`;
  counting the function's actual closing brace (confirmed with `awk` against
  the current file) puts it at `:310`. Corrected to `272-310` in §2.
- **`nix --extra-experimental-features 'nix-command flakes' develop -c make
  test` was run in full** as the brief's own Step 7 requires, green across
  all packages, `internal/controller` at 101.231s / 90.2% coverage — the
  slowest package, matching the proportion every prior task report in this
  milestone also recorded for it.

Nothing else this pass checked required a correction; the list above is
exactly what it caught.

## 8. Where everything lives

- Design: [`docs/superpowers/specs/2026-08-19-ci-design.md`](superpowers/specs/2026-08-19-ci-design.md).
- Open points: [`docs/known-issues.md`](known-issues.md), "From milestone
  6e", plus the closed and still-open halves of the milestone-2c entries it
  now amends.
- The workflows: `.github/workflows/ci.yml`, `.github/workflows/nightly.yml`,
  `.github/workflows/release.yml`.
- The lint configuration: `.golangci.yml`.
- The events-API migration: `internal/controller/events.go`,
  `internal/rbacaudit/required.go:42-58`.
- The SDD record of how this milestone was built, task by task, including
  every mutation run and its verbatim output:
  [`.superpowers/sdd/2026-08-19-ci/`](../.superpowers/sdd/2026-08-19-ci/)
  (`task-1-report.md` through `task-6-report.md`, `task-3b-report.md`, and
  `progress.md`, the ledger §5 above restates).
- 6d's record, and what 6e started from:
  [`handover-milestone-6d.md`](handover-milestone-6d.md).
