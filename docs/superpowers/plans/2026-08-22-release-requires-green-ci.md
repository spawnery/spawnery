# Release Requires Green CI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The release workflow refuses to publish a commit whose CI run is not
green.

**Architecture:** The decision lives in `hack/require-green-ci.sh`, which
`.github/workflows/release.yml` calls as its first step. Putting it in a script
rather than inline in YAML is what makes it testable: this repository has no
workflow linter and no shell linter, and an implementer cannot trigger a
runner — but a script can be run against the repository's own history, which
already contains commits with green CI, commits with red CI, and commits CI
never saw. `hack/publish.sh` is the existing precedent for the workflow
shelling out.

**Tech Stack:** bash, `gh` CLI, the GitHub Actions API.

## Global Constraints

- The spec is `docs/superpowers/specs/2026-08-22-release-requires-green-ci-design.md`.
  Where this plan and the spec disagree, stop and ask — do not pick one.
- Every command runs inside the dev shell:
  `nix --extra-experimental-features 'nix-command flakes' develop -c <cmd>`.
- **Do not run `make e2e`.** This machine has 8 GB of RAM.
- **Never run `git config` in any form.** A worktree shares `.git/config` with
  the main repository; a previous agent set an identity there and rewrote the
  author name on real commits.
- **Never push, never merge, and never create a tag.** This one is load-bearing
  here: a `v*` tag triggers the release workflow, and this branch is about that
  workflow. Creating one would fire a real release.
- Conventional Commits with English subjects. Every commit ends with exactly:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **The check must be positive, never negative.** `--jq '.workflow_runs[0].conclusion'`
  over an empty list yields the string `null`, so `[ "$c" != failure ]` would
  treat "CI never ran" as permission to publish. Assert `conclusion = success`
  and let everything else fall through to a refusal. This is the single
  mistake that would make the whole step worse than useless.
- Comments explain why, not what. `release.yml` and `hack/publish.sh` set the
  voice: they explain the failure the code is standing in front of.
- **A test that passes the moment it is written has proven nothing.**
- **Do not claim a verification that did not run.** The spec's §6 says so
  explicitly, and the report contract below repeats it.

---

## File Structure

| File | Change |
|---|---|
| `hack/require-green-ci.sh` | new — resolves the run, waits, decides |
| `hack/require-green-ci-test.sh` | new — drives the decision against real history and fixtures |
| `.github/workflows/release.yml` | the first step in `publish`, and `actions: read` |

---

## Task 1: The script that decides

**Files:**
- Create: `hack/require-green-ci.sh`
- Create: `hack/require-green-ci-test.sh`

**Interfaces:**
- Produces, for Task 2:
  `hack/require-green-ci.sh <owner/repo> <sha>` — exits 0 when the `ci.yml`
  run for that commit concluded `success`, and non-zero with a message on
  stderr in every other case.
- Reads `GH_TOKEN` from the environment, as `gh` does.
- **The seam:** the command that fetches runs is taken from
  `${CI_RUNS_CMD:-}`, defaulting to the real `gh api` call when unset. This
  exists so the test can feed fixtures without a network, and it follows
  `hack/e2e.sh`, which "deliberately hard-codes neither the scope nor the
  provider; it reads them from the environment". Document it in the script's
  header as a seam, not as configuration.

- [ ] **Step 1: Write the script**

Four outcomes, from the spec's table:

| state | outcome |
|---|---|
| no run exists | exit non-zero, saying CI never ran on this commit |
| status not `completed` | wait, up to `CI_WAIT_LIMIT` (default 1200s), polling every 15s |
| conclusion `success` | exit 0 |
| any other conclusion | exit non-zero, naming the conclusion and the run's URL |

The query filters on `event=push` and `branch=master` and takes the most
recent, for the reason the spec's §3 gives: a commit can carry a
`pull_request` run and a `push` run, they tested different trees, and it is
the second that a tag publishes.

```bash
gh api "/repos/${repo}/actions/workflows/ci.yml/runs?head_sha=${sha}&event=push&branch=master&per_page=1"
```

**It reads the run's conclusion, never a job's.** `ci.yml` has four jobs —
`test`, `lint`, `deps` and `e2e` — and a run is red if any is. Keying on a job
name would leave a job added later silently outside the gate, and the incident
this exists for failed in `e2e`, the last of the four; a rule naming `test`
would have waved it through. Querying `/actions/workflows/ci.yml/runs` is what
gets this for free, so do not reach into `/jobs` to refine it.

The decision, with the shape that matters:

```bash
runs="$(${CI_RUNS_CMD:-gh api "/repos/${repo}/actions/workflows/ci.yml/runs?head_sha=${sha}&event=push&branch=master&per_page=1"})"
count="$(printf '%s' "${runs}" | jq '.workflow_runs | length')"
if [ "${count}" -eq 0 ]; then
    # Absence is not permission. A tag on a commit CI never saw is exactly as
    # unverified as a tag on a red one.
    echo "no ci.yml run for ${sha} on master ..." >&2
    exit 1
fi
status="$(printf '%s' "${runs}" | jq -r '.workflow_runs[0].status')"
conclusion="$(printf '%s' "${runs}" | jq -r '.workflow_runs[0].conclusion')"
url="$(printf '%s' "${runs}" | jq -r '.workflow_runs[0].html_url')"

if [ "${status}" != completed ]; then
    ... wait, re-fetch, and on exceeding the limit say the limit was reached ...
fi

# Positive, never negative: `!= failure` would let the string "null" through.
if [ "${conclusion}" = success ]; then
    exit 0
fi
echo "ci.yml on ${sha} concluded ${conclusion}, not success: ${url}" >&2
exit 1
```

Hitting the wait limit must say it hit the limit — not that CI was red. They
are different facts and a release owner acts differently on each.

- [ ] **Step 2: Write the test, and run it against real history**

`hack/require-green-ci-test.sh`. Four cases. Three of them use commits that
exist in this repository right now, so they are not fixtures of the test's own
making — verify each commit's actual CI state with
`gh run list --workflow=ci.yml` before relying on it, because these are claims
about a live repository and this plan was written from one reading of it:

| case | how |
|---|---|
| green | a commit whose `ci.yml` push run concluded `success` — `1a9293b` was one when this plan was written |
| red | a commit whose run concluded `failure` — `2ee9ce7` was one |
| no run | any commit that was never pushed to `master`; a fresh empty commit on your working branch will do |
| in progress | a fixture through `CI_RUNS_CMD`, since a live in-progress run cannot be summoned |

The in-progress case must assert two things, not one: that the script waits,
and that it stops. Give it a short `CI_WAIT_LIMIT` so the test does not take
twenty minutes, and assert both that it exceeded the limit and that its message
says so rather than saying CI failed.

- [ ] **Step 3: Prove the test can fail**

Three mutations, each run against only the case it should break:

| mutation | expected |
|---|---|
| accept `null` as success — the empty-list trap from the constraints | the *no run* case passes when it must fail; test fails |
| drop `&event=push&branch=master` from the query | on a commit with both run kinds this can resolve the wrong one; if no such commit exists in the repository today, say so in the report and note the case could not be demonstrated |
| report the wait limit as a CI failure | the *in progress* case fails on the message |

Record the verbatim output of each, revert, and confirm with `git diff --stat hack/`.

- [ ] **Step 4: Commit**

---

## Task 2: Wiring it into the workflow

**Files:**
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `hack/require-green-ci.sh` from Task 1.

- [ ] **Step 1: Add the permission**

`permissions:` at `release.yml:14` gains `actions: read`, which is what reading
another workflow's runs requires. `contents: write` and `packages: write` stay.

- [ ] **Step 2: Add the step**

**First in the `publish` job — before `actions/checkout@v4`.** It needs neither
the tree nor Nix, and a refusal there costs seconds rather than the two minutes
`cachix/install-nix-action` takes.

That ordering has one consequence to handle: without a checkout there is no
`hack/require-green-ci.sh` on disk. Decide between putting the step after
checkout but before Install Nix, or fetching the script another way, and say in
your report which you chose and why. Checkout is cheap; Nix is not.

```yaml
      - name: CI, on the commit this tag points at
        if: github.event_name == 'push'
        env:
          GH_TOKEN: ${{ github.token }}
        run: hack/require-green-ci.sh "${GITHUB_REPOSITORY}" "${GITHUB_SHA}"
```

The `if` is the same condition the version preflight at `release.yml:89`
already carries. A `workflow_dispatch` can only ever dry-run — the trigger's
own comment says publishing under this project's name is a decision a tag
records — so a dispatch has no tag and nothing for this step to be about.

Write the comment above the step in the file's own voice: what it is standing
in front of. The concrete history is that `ci.yml`'s `e2e` job had already
failed on the same Nix hash mismatch three times — on both merges and on the
release commit itself — before the tag became the fourth place the repository
said so.

- [ ] **Step 3: Confirm the guards behind it are untouched**

The version preflight, the chart-version guard and the publishes-nothing guard
all stay exactly where they are and keep catching what they catch — the spec
says so, and this step adds to them rather than replacing any. Confirm with
`git diff` that the only changes to `release.yml` are the permission line and
the new step, and say so in the report.

- [ ] **Step 4: Read the whole job once, in order**

There is no workflow linter and no shell linter in this dev shell, and you
cannot trigger a runner. So the only check available at this level is reading.
Read the `publish` job top to bottom and confirm: the new step is first among
those that can fail, its `if` matches the preflight's, nothing later depends on
something it skips, and the YAML is valid — `python3 -c "import yaml,sys;
yaml.safe_load(open('.github/workflows/release.yml'))"` is available and is
worth running.

- [ ] **Step 5: Commit**

---

## What this plan does not cover, and what it cannot

**Three of the four decision rows cannot be exercised on a runner from this
branch**, and the report must say so rather than implying a matrix that was
run:

- The **green** row is the next real release. It is the case that must not
  regress and it will be observed then, not now.
- The **red** row would need a commit that both carries this step and has red
  CI. `on: push: tags` runs the workflow file *from the tagged commit*, so
  tagging an existing red commit would run the old workflow without the step —
  which would publish. Demonstrating it means deliberately breaking CI on a
  commit that has the step, and that is not worth doing.
- The **no run** row is demonstrable with a scratch `v*` tag on an off-master
  commit that carries the step: the gate refuses, and if it did not, the
  version preflight behind it would. That is a real experiment, but it creates
  a tag, which this branch's implementers must not do. **It belongs to whoever
  merges this**, not to a task here.

What Task 1 *does* exercise, against the live repository rather than against
invented data, is the decision itself in all four states. That is the honest
level for this work, and the difference between "the script decides correctly"
and "the workflow behaves correctly on a runner" is the thing the report must
keep straight.
