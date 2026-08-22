# A release refuses a commit CI has not passed

## 1. What this is

On 2026-08-22 the v0.2.0 release failed in its publish step on a Nix
fixed-output hash mismatch: asserting a Prometheus gauge had pulled
`client_golang/prometheus/testutil` in, `go mod tidy` added
`kylelemons/godebug` as an indirect requirement, and `flake.nix`'s five copies
of `vendorHash` still named the module set from before.

That failure was not new information. `.github/workflows/ci.yml`'s `e2e` job
runs `make e2e`, which builds `.#operator-image` through Nix, and it had
already failed on exactly this — three times:

| CI run | commit | when |
|---|---|---|
| 32509166321 | `Merge: the CA can be rotated without disconnecting an agent` | 2026-08-21 17:38 |
| 32580425657 | `Merge: a rotation slot no agent could use never reaches one` | 2026-08-22 |
| 32580737899 | `chore(release): 0.2.0` | 2026-08-22 |

Three red runs on `master` went unread, two further pushes went out on top of
them, and the tag was the fourth place the repository said so — 52 seconds into
a release that had already been announced by a pushed tag.

**This design adds one step to the release workflow: it refuses to publish a
commit whose CI run is not green.**

## 2. What this is not, and why it is this small

Two other gaps were proposed alongside this one. One dissolved when the code
was read rather than remembered. The other shrank instead of dissolving — the
first reading of it was itself made from one pass over the code, and a second
pass found the smaller thing that is genuinely there — and what is left of it
is recorded outside this design. Keeping both accounts, and being explicit
about which is which, is what keeps this design one step.

**"The tag guard accepts a match against either version, so a stale image can
ride along."** The preflight at `release.yml:89` does accept a tag that equals
`imageVersion` *or* `operatorVersion`, and that laxity is deliberate — the two
are decoupled, and a release that bumps only one is legitimate. What catches
the case is the next step: the publish loop counts what it actually pushed, and
on a first attempt a count of zero fails with `all three images were already
published at the versions flake.nix names, so <tag> releases nothing. Bump
imageVersion or operatorVersion in flake.nix and tag again.` The hole was in
reading the preflight in isolation, not in the workflow.

**"CI does not build the Nix derivations the release path builds."** It builds
one of the three. The `e2e` job runs `hack/e2e.sh`, which runs `nix build
.#operator-image`, and the table above is the proof that this is what had
already caught the 2026-08-22 `vendorHash` mismatch. It is also the only image
derivation any CI job reaches: `make test` and `make lint` enter Nix only
through `nix develop`, and the release publishes `paper-image` and
`velocity-image` beside the operator's.

So the original claim was too broad and this dissolution of it, as first
written, was too generous. What is true is narrower: the derivation the
incident happened to be about is covered, and the two carrying their own
fixed-output hashes are not. **That leftover gap is real and outlives this
branch, so it is recorded where it will be found rather than restated here:
`docs/known-issues.md`, under "From milestone 6e (CI)".** It is out of scope
for this design for the reason the rest of this section is about — closing it
is machinery, and this design is one step.

The remaining gap this design *does* close is not machinery that is missing.
It is that a signal the repository already produces is only consulted by a
human, and on the one day it mattered it was not.

**What this does not do:** it adds no check beyond the tagged commit. It does
not consult `nightly.yml`, whose signal is not per-commit and would need its
own staleness rule. It does not touch branch protection. And it replaces no
existing guard — the version preflight, the chart-version guard and the
publishes-nothing guard all stay exactly where they are and keep catching what
they catch.

## 3. The check

A step named for what it asserts, first in the `publish` job, before the
version preflight.

It carries `if: github.event_name == 'push'`, the same condition the version
preflight already has. A `workflow_dispatch` of this workflow can only ever
dry-run — the trigger's own comment says publishing under this project's name
is a decision a tag records — so a dispatch has no tag, no commit under test,
and nothing for this step to be about.

That also means **there is no bypass.** Every real release comes from a tag
push, and a tag push is exactly what this step gates. A release that has to go
out over red CI is a release that fixes CI first.

It resolves the `ci.yml` workflow run whose `head_sha` is `github.sha` and
decides four ways.

**Which run, when there is more than one.** A commit can carry two `ci.yml`
runs: one from the `pull_request` trigger while it was a branch head, and one
from the `push` trigger once it reached `master`. They are not
interchangeable — the pull-request run tested the merge preview, the push run
tested what is actually on `master`, and it is the second that a tag publishes.
So the query filters on `event=push` and `branch=master`, and takes the most
recent. A re-run does not multiply this: it adds an attempt to the same run,
and the run's conclusion is the latest attempt's.

For a tag push, `github.sha` is the commit the tag points at, which is what
makes this a check about the release's own contents rather than about whatever
`master` happens to be.

| state | outcome |
|---|---|
| no run exists | **fail** |
| still in progress or queued | **wait**, up to `ciWaitLimit` |
| conclusion `success` | proceed |
| any other conclusion | **fail**, naming the conclusion and the run's URL |

**No run is a failure, not a pass.** A tag on a commit CI never saw is exactly
as unverified as a tag on a red one, and treating absence as permission is the
shape of guard this repository has already been bitten by once — the version
preflight's own comment records a check that "measured a number that had quietly
stopped being the one it named". The cost is that a commit which was never on
`master` can no longer be tagged, which for this project is not a cost.

**Waiting is what makes it usable.** `ci.yml` runs on pushes to `master`, and
the ordinary release is `git push origin master` followed by a tag — measured,
CI takes four to seven minutes, so the tag will normally arrive while it is
still running. Failing on "in progress" would forbid the sequence that actually
happens. `ciWaitLimit` is twenty minutes: roughly three times the longest run
observed, which leaves room for a queued runner without letting a wedged run
hold a release job open indefinitely. It polls every fifteen seconds — the
thing being waited on takes minutes, so anything finer only spends API calls. Hitting the limit fails, and says it hit
the limit rather than pretending CI was red.

`ci.yml`'s concurrency group sets `cancel-in-progress` to false for `master`, so
a run this step is waiting on will not be cancelled by a later push. A run that
*is* cancelled — by hand, or by a cancelled workflow — has a conclusion of
`cancelled` and lands in the fail row, which is right: nobody knows what it
would have found.

## 4. The run, not the jobs

The check reads the workflow run's conclusion, not any individual job's.

`ci.yml` has four jobs — `test`, `lint`, `deps` and `e2e` — and a run is red if
any of them is. Keying on a job name would mean a job added later is silently
outside the gate, and the incident this design exists for failed in `e2e`, the
last of the four and the one most recently added. A rule that had named `test`
would have passed it.

## 5. What it costs

Nothing on the ordinary path but an API call, and on the racing path some idle
minutes of a runner that would otherwise have spent them building images it was
going to throw away. The failed release of 2026-08-22 spent 52 seconds before
dying; a release that waits four minutes and then does not start is cheaper than
one that starts and cannot finish.

The workflow's `permissions` block gains `actions: read`, which is what reading
another workflow's runs requires. It keeps `contents: write` and
`packages: write`.

**And it makes an existing exemption more reachable, which is the one cost
that is not paid in runner minutes.** `release.yml`'s publishes-nothing guard
disarms itself when `GITHUB_RUN_ATTEMPT > 1`, and its comment explains why: on
a re-run, "all three images are already on the registry" is the normal state
rather than the mistake, and failing there would stop a recovery attempt
before it reached the digest and Release steps it was re-run for.

This design adds a *designed-in* first-attempt failure whose documented remedy
is a re-run — CI slower than `ciWaitLimit`, the gate times out, the owner
clicks Re-run. On that second attempt the publishes-nothing guard is disarmed,
so a tag that bumps nothing in `flake.nix` gets a `::notice::` and a GitHub
Release where on a first attempt it would have got the loud refusal and "Bump
imageVersion or operatorVersion in flake.nix and tag again".

Nothing about that is new — any transient failure on attempt 1 has always had
the same effect — but this is the first change that raises how often attempt 2
happens, and it raises it on purpose. **No code change is proposed here.**
Narrowing the exemption, say by keying it on whether an earlier attempt
actually published something rather than on the attempt number alone, is a
change to a guard §2 promises to leave exactly where it is, and making it as a
side effect of adding a wait is the scope creep §2 exists to refuse. What this
paragraph is for is that the two are now coupled: whoever next touches
`ciWaitLimit` or that exemption should read this and the comment on
`release.yml`'s publish loop together, because neither one's reasoning is
complete without the other's.

## 6. How it is proven

This is a workflow step, so the levels available to it are reading and running
it, not a unit test.

- **A dry-run dispatch still works.** `workflow_dispatch` must reach the steps
  it exists to exercise, and the `if` must keep this step out of its way. Run
  one and confirm the step is skipped and the run proceeds.
- **A tag on a commit with a green CI run publishes.** This is the next real
  release, and it is the case that must not regress.
- **A tag on a commit with a red CI run refuses.** The obvious way to
  demonstrate it without breaking a release is a scratch tag on a branch whose
  CI is red — but `ci.yml` runs on `master` and on pull requests only, and a
  `v*` tag on a non-master commit now fails for the no-run reason instead,
  which tests a different row. **State plainly in the report which of the four
  rows were exercised on a runner and which were only read**, rather than
  claiming a matrix that was not run.

The four rows are small enough that reading the step against the API's
documented `status` and `conclusion` values is honest evidence for the ones a
runner cannot reach cheaply. What must not happen is a report that implies
otherwise.

## 7. Acceptance criteria

1. A tag push whose commit has a successful `ci.yml` run publishes as before.
2. A tag push whose commit has a failed, cancelled or timed-out `ci.yml` run
   fails before anything is built, naming the conclusion and the run's URL.
3. A tag push whose commit has no `ci.yml` run fails, and says that is why.
4. A tag push whose commit has a run still in progress waits for it, up to
   `ciWaitLimit`, and fails on the limit with a message that says the limit was
   reached rather than that CI was red.
5. A `workflow_dispatch` run skips the step entirely and still dry-runs.
6. The check reads the workflow run's conclusion, not a named job's.
7. The version preflight, the chart-version guard and the publishes-nothing
   guard are unchanged.
