# Docs Shortening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut the six documentation pages phase 3 never turned around from
25,829 words to under 9,700, without losing a measurement or breaking a link.

**Architecture:** One task per page, each ending in its own commit and its own
word count. `guides/upgrading.md` splits rather than shrinks: everything named
after a release moves verbatim to a new archive page. A final task adds
`hack/docs-length.sh` so the sizes hold.

**Tech Stack:** Markdown, MkDocs Material, bash, `make docs` (`mkdocs build
--strict`) as the link checker.

**Spec:** `docs/superpowers/specs/2026-09-17-docs-shortening-design.md`

## Global Constraints

- **Everything in git is English.** Prose, headings, commit messages. The one
  German file this plan touches (`charts/spawnery/Chart.yaml`) keeps its
  German; only a path inside a comment changes.
- **Measurements stay, with their dates and the cluster they were taken on.**
  `CLAUDE.md`: *"Docs and code comments in this repo record measurements
  ('measured on …') rather than assumptions."* What goes is the provenance
  around them — milestone numbers, task numbers, fix rounds, "this used to
  read".
- **Filenames do not change.** Three are pinned from code:
  `internal/controller/forwardingsecret.go:37` (a `const`),
  `internal/podspec/labels.go:73`, `internal/certs/events.go:47`.
- **Commits are Conventional Commits** with a scope, a body wrapped at 72
  columns saying why, and these two trailers:

  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01RhLpABDQDo2jeWRbfNEdtT
  ```

- **Every make target runs in the devshell, with no `cd` in front of it:**

  ```bash
  nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs
  ```

- **`make docs` is the only link checker** and `mkdocs.yml` sets
  `validation.links.anchors: warn`, which `--strict` turns into an error. A
  dead anchor fails the build.
- **`make docs` needs `make docs-assets` once per checkout** — it builds the
  gitignored `docs/assets/mermaid.min.js` and `docs/plugin-api/javadoc/`.
  Run it before the first `make docs` and not again.
- **Word counts are `wc -w` over the Markdown source**, code blocks included.
- **Do not reopen the pages phase 3 rewrote** (`tutorial/`, `guides/{expose-strategies,scaling-and-boosts,updates-and-drain,scheduling,cloud-command}.md`, `plugin-api/`, `explanation/{architecture,agent-trust}.md`) except for the link repairs this plan names.
- **Angle brackets in an outline are a brief, not text to paste.** Where a
  task shows `<the kubectl command; two values means rolling>`, that names
  what the section says and what it must keep; the sentences are the task's
  to write. Headings shown outside angle brackets are literal — several are
  link anchors and their exact wording is load-bearing.

## File Structure

| File | Change |
|---|---|
| `docs/archive/release-notes.md` | **Create.** Lines 114–949 of today's `guides/upgrading.md`, verbatim prose, relative links repointed. |
| `docs/guides/upgrading.md` | Rewrite from lines 1–113. ≤ 1,100 words. |
| `docs/getting-started/index.md` | Rewrite. ≤ 900 words. |
| `docs/guides/rotating-the-forwarding-secret.md` | Rewrite. ≤ 1,600 words. |
| `docs/guides/rotating-the-ca.md` | Rewrite. ≤ 1,250 words. |
| `docs/explanation/network-boundaries.md` | Rewrite. ≤ 3,550 words. |
| `docs/contributing/development.md` | Rewrite. ≤ 2,400 words. |
| `mkdocs.yml` | One nav entry for the archive page. |
| `hack/docs-length.sh` | **Create.** The ceiling table and the check. |
| `hack/docs-length-test.sh` | **Create.** Drives the check past its own failure. |
| `Makefile` | `docs-length-lint` into `test:`'s prerequisites, `docs-length-lint-test` beside it. |

**"Verbatim" has one exception.** The archived text moves from `docs/guides/`
to `docs/archive/`, so its four relative links change depth or target. The
prose does not change. Those four are listed in Task 1, Step 3.

---

### Task 1: Archive the release notes, rewrite the upgrading guide

**Files:**
- Create: `docs/archive/release-notes.md`
- Modify: `docs/guides/upgrading.md` (replace whole file)
- Modify: `mkdocs.yml` (Archive nav)
- Modify: `docs/guides/scheduling.md:152`, `docs/guides/cloud-command.md:99`,
  `docs/reference/known-issues.md:12`, `docs/contributing/development.md:100`,
  `charts/spawnery/Chart.yaml:98`, `hack/image-tag-pins-agree.sh` (header comment)

**Interfaces:**
- Produces: `docs/archive/release-notes.md` with every `##` heading of today's
  lines 114–949 intact, so anchors survive. Later tasks link to it.
- Produces: `guides/upgrading.md` with no section named after a version.

- [ ] **Step 1: Record the starting size**

```bash
wc -w docs/guides/upgrading.md      # expect 7880
```

- [ ] **Step 2: Split the file**

```bash
sed -n '114,949p' docs/guides/upgrading.md > /tmp/release-notes-body.md
sed -n '1,113p'   docs/guides/upgrading.md > /tmp/upgrading-head.md
```

- [ ] **Step 3: Build `docs/archive/release-notes.md`**

Header, then the body from Step 2 unchanged:

```markdown
# Release notes

These were `docs/guides/upgrading.md` until 2026-09-17, one section per
release. They are here rather than deleted because an installation older than
any that exists today would still meet them; what is true of *any* upgrade is
in [Upgrading](../guides/upgrading.md).

Nothing here is an open defect. [`known-issues.md`](../reference/known-issues.md)
carries those.
```

Then repoint exactly four links, because the file moved a directory:

| Was | Becomes |
|---|---|
| `[the chart's README](../getting-started/index.md#the-cloud-permissions)` | `[The /cloud command](../guides/cloud-command.md)` |
| `` [`plugins.md`](plugins-from-a-volume.md) `` | `` [`plugins.md`](../guides/plugins-from-a-volume.md) `` |
| `[mount](mounts-and-files.md)` | `[mount](../guides/mounts-and-files.md)` |
| `` [`docs/guides/plugins-from-a-volume.md`](plugins-from-a-volume.md) `` and `` [`docs/guides/mounts-and-files.md`](mounts-and-files.md) `` | same two with `../guides/` |

The first is a target change, not only a depth change: Task 2 deletes that
anchor, and `guides/cloud-command.md` is where those permissions are
documented.

- [ ] **Step 4: Write the new `docs/guides/upgrading.md`**

≤ 1,100 words, this outline, built from `/tmp/upgrading-head.md`:

```markdown
# Upgrading between releases

<Two sentences: an upgrade costs whatever it rolls, and two things decide
that. Then straight to the command.>

## Is a group mid-roll?

<the `kubectl get pods -L spawnery.cloud/pod-hash` command; two distinct
values inside one group means rolling, one means done or never started. The
group's status will not tell you — the surge pod comes up before any old pod
is withdrawn, so readyReplicas holds and the phase reads Ready throughout.>

## What makes a fleet roll

<The pod hash is a digest of the *rendered pod*, not of chosen spec fields.
So a change in the rendering code moves it while every spec stays byte for
byte what it was. The second trigger: the agent endpoint feeds the digest, so
moving the operator's namespace rolls the whole fleet with nothing else
changing. Every group starts within a reconcile, one pod at a time per group,
all groups at once; each replaced pod runs the ordinary drain, and a player
still there at spec.drain.timeoutSeconds is disconnected with one
`Warning ProxyDrainTimeout` naming what it cost. Why this rather than a
metadata.generation rule: replicas is the routine edit on a proxy group, and
a generation rule would make every scale a full replacement.>

## Finding out before you upgrade

<`internal/podspec/hash_golden_test.go` pins both digests, so a render change
fails on the pull request. Comparing two builds after the fact, the cheap
negative filter is `git diff <old>..<new> -- internal/podspec/`. Keep the
measurement: that is how 2026-08-22's v0.1.2 to v0.2.0 upgrade was known safe
in advance, and both proxies kept `pod-hash 2dd6593373a4ffd2` and 46 hours of
uptime. What neither covers: the group's own namespace and name, the
Network's name, and the agent endpoint.>

## Upgrade the proxy images before the operator

<A new operator against proxy images older than `SetReady` empties nobody and
disconnects everybody at the deadline — and looks at first like nothing
happening, because the surplus pod stays Ready and in the endpoint slice,
receiving new players for the whole drain window. The signature: the
`spawnery.cloud/draining-since` annotation while `Ready` is still True. A
correctly drained proxy carries the annotation and is NotReady. Rolling the
operator back on its own is safe.>

## Older installations

<One paragraph to [Release notes](../archive/release-notes.md): the
release-by-release notes, and the renames an installation created before
v0.1.0 still carries.>
```

Cut from the head that does not survive: the opening paragraph about three
renames (the renames are archived), the design-document citation
(`2026-08-14-proxy-rolling-updates-design.md` §3.1), the milestone-4b
comparison as an argument rather than a rule, and the protobuf field number
of `SetReady`.

- [ ] **Step 5: Repair the five inbound references**

```
docs/guides/scheduling.md:152
  upgrading.md#0233-a-groups-scheduling-needs-the-networks-permission
  → ../archive/release-notes.md#0233-a-groups-scheduling-needs-the-networks-permission

docs/guides/cloud-command.md:99
  upgrading.md#the-agents-gain-a-cloud-command-granted-to-nobody
  → ../archive/release-notes.md#the-agents-gain-a-cloud-command-granted-to-nobody

docs/reference/known-issues.md:12
  Sentence is now wrong as well as the target: upgrading.md no longer carries
  "what strands an object". Say that the guide carries what rolls a fleet and
  the archive carries what strands an object.

docs/contributing/development.md:100
  Points at upgrading.md for what an installation does about Paper's
  deprecation. That note is archived; retarget it.

charts/spawnery/Chart.yaml:98
  German comment, path only:
  docs/guides/upgrading.md → docs/archive/release-notes.md
```

- [ ] **Step 6: Move the exemption in `hack/image-tag-pins-agree.sh`**

Its header comment reads *"and docs/guides/upgrading.md, whose version notes
name old tags on purpose"*. The version notes are now under `docs/archive/`,
which the same comment already exempts. Correct the sentence; the code does
not change.

- [ ] **Step 7: Add the nav entry**

`mkdocs.yml`, under `Archive:`, between `What this is` and `How it was built`:

```yaml
      - Release notes: archive/release-notes.md
```

- [ ] **Step 8: Verify**

```bash
wc -w docs/guides/upgrading.md      # expect ≤ 1100
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs
```

`make docs` must pass. It is the check that no anchor died.

- [ ] **Step 9: Commit**

```bash
git add docs/archive/release-notes.md docs/guides/upgrading.md mkdocs.yml \
  docs/guides/scheduling.md docs/guides/cloud-command.md \
  docs/reference/known-issues.md docs/contributing/development.md \
  charts/spawnery/Chart.yaml hack/image-tag-pins-agree.sh
git commit    # docs(guides): upgrading keeps the rules, the archive keeps the releases
```

---

### Task 2: Turn getting-started into an install page

**Files:**
- Modify: `docs/getting-started/index.md` (replace whole file)
- Modify: `docs/explanation/network-boundaries.md:458` (one link)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: two anchors later tasks and existing pages depend on —
  `#choosing-a-game-namespace` and
  `#the-one-manual-step-this-chart-cannot-make`. **Both headings keep their
  exact current wording.** `docs/index.md:39` and `docs/tutorial/index.md:92`
  link to them.
- Produces: `#the-cloud-permissions` no longer exists.

- [ ] **Step 1: Record the starting size**

```bash
wc -w docs/getting-started/index.md    # expect 2862
```

- [ ] **Step 2: Rewrite the page**

≤ 900 words, this outline:

```markdown
# Installing the operator

<The chart is the only installation form. One sentence.>

## Installing

<The OCI `helm install` line. That `--version` is the chart's own number and
not the release tag, in one sentence — each GitHub Release body prints the
line with the right one in it. Then the from-a-checkout line, and why
`--create-namespace` is not optional: the chart templates no Namespace of its
own, so without it helm refuses with `namespaces "spawnery-system" not
found`.>

### Choosing a game namespace

<Keep as it is, tightened. The measurement stays: measured 2026-08-21
against a real CNI, a pod labelled as a proxy reached a backend on 25565 and
an unlabelled pod read the secret. Keep the conclusion in bold — do not share
a game namespace with workloads you would not trust with that network.>

### The one manual step this chart cannot make

<The `kubectl apply -n <game-namespace> -f config/rbac/forwarding-secret-reader.yaml`
line and why it is outside the chart. That `networkNamespaces` does it for
the namespaces you already know. The RoleBinding hard-codes
`spawnery-system` at line 65 and has to be changed for any other release
namespace — keep the bold warning and the sentence that the failure names the
secret rather than the RoleBinding. Cut the paragraph tracing what
`Network.status` reports through which condition; one sentence covers it.>

## The values

<Three sentences, no table: every key is in
[Chart values](../reference/chart-values.md), generated from
`charts/spawnery/values.schema.json` by `make manifests`. Keep the one fact
the generated page does not carry: `replicas` and `imagePullSecrets` are
deliberately not values, because `readyz` hangs off the leader lock so a
second replica never becomes ready.>

## The `/cloud` permissions

<Two sentences and a link to [The /cloud command](../guides/cloud-command.md).
Keep only the one thing an installer meets immediately: nobody holds any of
them by default, so right after installing the command answers "unknown
command" to every player, and that is the safe state rather than a broken
install.>

## Uninstalling

<The `helm uninstall` line, that it leaves the four CRDs standing with
`helm.sh/resource-policy: keep`, and therefore every Network, ServerGroup,
ProxyGroup and Server with them. The `kubectl delete crd` line for a full
removal, and that persistent claims carry no owner reference and survive it.
Cut the paragraph about no `helm upgrade` ever having been run.>
```

The values table and the permissions table are deleted, not moved: both exist
elsewhere, and the values table has already drifted (it names `image.tag`
`"0.2.12"` against the generated page's `0.2.33`).

- [ ] **Step 3: Retarget the one inbound anchor**

`docs/explanation/network-boundaries.md:458` links
`../getting-started/index.md#the-cloud-permissions`. That anchor is gone.
Point it at `../guides/cloud-command.md`.

- [ ] **Step 4: Verify**

```bash
wc -w docs/getting-started/index.md    # expect ≤ 900
grep -n 'choosing-a-game-namespace\|the-one-manual-step' docs/index.md docs/tutorial/index.md
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs
```

- [ ] **Step 5: Commit**

```bash
git add docs/getting-started/index.md docs/explanation/network-boundaries.md
git commit    # docs(getting-started): an install page instead of the chart README
```

---

### Task 3: Shorten the forwarding-secret rotation guide

**Files:**
- Modify: `docs/guides/rotating-the-forwarding-secret.md` (replace whole file)

**Interfaces:**
- The filename is pinned by `internal/controller/forwardingsecret.go:37` and
  named in `internal/podspec/labels.go:73`. It does not change.

- [ ] **Step 1: Record the starting size**

```bash
wc -w docs/guides/rotating-the-forwarding-secret.md    # expect 3857
```

- [ ] **Step 2: Rewrite**

≤ 1,600 words. Keep the procedure and lead with it:

- Drop the `# Runbook:` prefix from the title and the whole
  `**Status: standing operating procedure**` block. It explains this
  document's relationship to the archive, which is not a question a reader
  rotating a secret has.
- §1 (the reader Role), §2 (the progress command) and §3 (steps 1–7) stay,
  tightened. Every `kubectl` line stays. Every condition name and reason
  string stays.
- §5 ("why the server groups go first") becomes two sentences inside step 4:
  proxies first throws every player out at once into a network where no
  backend is reachable; servers first keeps the proxies up and leaves one
  hard cut at the end. Delete the rest.
- §6's two warnings stay as they are. Both are things a reader would
  otherwise get wrong: `kubectl delete pod` bypasses the PDB, and the
  one-ordinal budget does not bind a human.
- §7 (rollback) stays.
- §8: **keep both condition tables unchanged.** Delete the prose around them
  and the `One thing the stamp does not say` subsection.
- §9: **delete.** It is §8's argument in a second draft. One fact in it
  appears nowhere else — that a pod created between the secret changing and
  the operator recording the new digest is stamped with the old one, and
  reads as stale for as long as it lives. That becomes two sentences under
  the `PodsPredateTracking` row.

Every `internal/...:NNN` source citation may go; they date faster than the
prose and `git grep` finds the symbol. Keep the ones inside a table cell
where they identify which of two similar fields is meant.

- [ ] **Step 3: Verify**

```bash
wc -w docs/guides/rotating-the-forwarding-secret.md    # expect ≤ 1600
grep -c 'kubectl' docs/guides/rotating-the-forwarding-secret.md   # no command lost
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs
```

- [ ] **Step 4: Commit**

```bash
git add docs/guides/rotating-the-forwarding-secret.md
git commit    # docs(guides): the forwarding-secret rotation, once instead of twice
```

---

### Task 4: Shorten the CA rotation guide

**Files:**
- Modify: `docs/guides/rotating-the-ca.md` (replace whole file)

**Interfaces:**
- Filename named by `internal/certs/events.go:47`. It does not change.
- `docs/explanation/architecture.md:78` and
  `docs/reference/known-issues.md:15` link to the file with no anchor.

- [ ] **Step 1: Record the starting size**

```bash
wc -w docs/guides/rotating-the-ca.md    # expect 2837
```

- [ ] **Step 2: Rewrite**

≤ 1,250 words. "The sequence" already leads with the commands and stays
nearly as it is. "How the rotation works" is 2,388 words in one section and
becomes four:

```markdown
## The sequence                  <as today>
## What the operator waits for
## What blocks the gate
## When a request is refused
## Do not hand-edit the secret
## Nothing starts this on its own
```

- **What the operator waits for.** The two waits, both with their numbers:
  `start` can sit up to an hour before a tick picks it up (the loop runs at
  `RenewCheckInterval` while nothing is rotating), and restarting the
  operator pod picks it up at once. Then the 30-second cadence, the wait for
  every namespace to show the new CA, and the further wait covering the
  kubelet's projection delay plus `--agent-session-deadline` — roughly a
  quarter of an hour.
- **What blocks the gate.** That the gate stops running once
  `ca-rotation-since` is stamped, and that a namespace with leftover agent
  pods and no `Network` blocks it. That `RotationBlocked` fires only on a
  change and expires within the hour, so the durable signals are the
  `ca-rotation-blocked-on` annotation and the
  `spawnery_ca_rotation_blocked_namespaces` gauge — alert on the gauge.
- **When a request is refused.** The annotation is consumed either way and a
  `RotationRequestRefused` warning is the only trace; an unrecognised value
  is never consumed and fires every 30 seconds until corrected.
- **Do not hand-edit the secret.** ~150 words replacing ~900. The page itself
  says the procedure cannot produce this state: *"Only a hand-edited or
  truncated secret reaches this; nothing the procedure itself does can."*
  Keep: the operator repairs or discards a damaged slot on its own, before it
  looks at `rotate-ca` at all; a damaged `ca-next.crt` while `distributing`
  abandons the rotation; a damaged `ca-previous.crt` while `switched`
  **completes the irreversible drop within one 30-second tick**;
  `spawnery.cloud/ca-rotation-discarded` is the durable record.
  `internal/certs/rotation.go` carries the rest.
- **Nothing starts this on its own.** `CALifetime` is ten years and nothing
  schedules a rotation — deliberate, because how many days remaining should
  worry somebody is a fact about a cluster. What is not left to memory is the
  clock: the two expiry gauges and the chart's optional `PrometheusRule` with
  `caExpiryWarningDays`. Keep the closing sentence about starting with time
  in hand. Keep that a compromised CA key is a different emergency.

- [ ] **Step 3: Verify**

```bash
wc -w docs/guides/rotating-the-ca.md    # expect ≤ 1250
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs
```

- [ ] **Step 4: Commit**

```bash
git add docs/guides/rotating-the-ca.md
git commit    # docs(guides): the CA rotation without the slot forensics
```

---

### Task 5: Shorten network-boundaries

**Files:**
- Modify: `docs/explanation/network-boundaries.md` (replace whole file)

**Interfaces:**
- Consumes: Task 2 already retargeted line 458's link to
  `../guides/cloud-command.md`. **Do not reintroduce a link to
  `getting-started/index.md#the-cloud-permissions`** — that anchor is gone.
- Produces: `#how-many-agents-may-reach-the-operator` must survive.
  `docs/explanation/agent-trust.md:70` links to it.
- Five source files name this page in comments
  (`internal/{netstate,render,agentserver}`, `internal/controller/scheduling_test.go`,
  `charts/spawnery/templates/crds.yaml`). None uses an anchor.

- [ ] **Step 1: Record the starting size**

```bash
wc -w docs/explanation/network-boundaries.md    # expect 4534
```

- [ ] **Step 2: Rewrite**

≤ 3,550 words. This stays the longest of the six — it is an explanation page
and it is allowed to be. What goes is provenance, not substance:

- **Milestone and task numbers throughout.** "since 6b", "before 4c-3",
  "Task 3's fix round", "6a's handover §6 listed…", "this entry used to
  read". Roughly 40 occurrences. The date attached to each measurement stays
  and is what dates the claim.
- **The `config/deploy/` note in the lead** and the paths kept "where they
  date a measurement". The date does that.
- **The repetition.** That the harness's CNI enforces nothing is stated in
  five sections. It stays once, in the opening, and the later sections lean
  on it in a clause.
- **"What the operator knows about a person"** — 1,118 words across four
  "And since 7b-N" paragraphs, each widening the last. It becomes one
  statement: what the operator holds for every player today (UUID, username,
  backend), that it is in memory and reaches no CR, no etcd, no log at
  default verbosity and no metric label; that every agent in a namespace
  receives it; that an agent may ask for a player to be moved and nothing
  else, structurally bounded to its own namespace because the request carries
  no namespace field; that a server may describe itself within stated size
  bounds; and the one thing not bounded — anyone who can reach the operator
  process reads the roster.
- **The plugin-permission argument** (a gate would have to be invented and
  would be worth nothing) compresses to three sentences ending where it
  already ends: the boundary is who may install a plugin, which is who may
  create a pod.

Keep every heading a section still earns, and keep
`## How many agents may reach the operator` by that exact wording.

Keep every measurement: the 2026-08-21 Cilium labels test, the unlabelled
pod mounting 44 bytes of the forwarding secret, the 2026-08-25 egress-deny
cutting a proxy agent's stream, the `restricted` HostPort refusal quoted
verbatim from the API server, the 2026-08-25 HostPort admission and
`status.address: 45.137.203.198:25577`, the host-firewall port list, and
Cilium's refusal to share a LoadBalancer address.

- [ ] **Step 3: Verify**

```bash
wc -w docs/explanation/network-boundaries.md    # expect ≤ 3550
grep -c 'measured\|Measured' docs/explanation/network-boundaries.md
grep -n 'how-many-agents' docs/explanation/agent-trust.md
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs
```

- [ ] **Step 4: Commit**

```bash
git add docs/explanation/network-boundaries.md
git commit    # docs(explanation): the boundaries without the milestone numbers
```

---

### Task 6: Shorten development.md

**Files:**
- Modify: `docs/contributing/development.md` (replace whole file)

**Interfaces:**
- Consumes: Task 1 already retargeted line 100's Paper-deprecation link. Do
  not point it back at `guides/upgrading.md`.

- [ ] **Step 1: Record the starting size**

```bash
wc -w docs/contributing/development.md    # expect 3859
```

- [ ] **Step 2: Rewrite**

≤ 2,400 words.

- **The targets table stays whole.** It is the page's reference value.
- **"Publishing" is 1,060 words and becomes about 400.** Keep: what each of
  the five scripts does, that three reach the network and are part of no
  other target, `DRY_RUN=1` / `FORCE=1` / `WRITE_DIGEST=1`, that
  `publish-chart` packages from `git archive HEAD` and why, that the Maven
  Central step needs two secrets nobody can grant from a workflow so
  `release.yml` skips rather than fails it, and that a gap in a version
  sequence is a release that built nothing on that side. Delete: the recital
  of which release moved which number (`v0.2.6`, `v0.2.8`, `v0.2.11`
  bumped `operatorVersion` alone …), and the story of `v0.2.14`'s first
  attempt failing its own preflight. Both are commit messages.
- **"Trying it locally against kind" keeps its commands and its traps** —
  the rootless-Podman relay, the `systemd-run --scope` wrapper,
  `--operator-namespace`, the selector-less Service and hand-written
  Endpoints, why the link-local address is rejected. The narrative of
  discovering k3d cannot work under rootless Podman becomes one sentence.
  Keep the measured expectations block and the 2026-08-10 renewal
  measurement.
- **"Reproducibility"** keeps why the plain build in front of `--rebuild` is
  not redundant, and loses "until milestone 6a's final fix wave".
- **"The operator's image"** loses the 2026-08-17 four-way `--rebuild` run,
  which `make image-repro` re-establishes on demand.

- [ ] **Step 3: Verify**

```bash
wc -w docs/contributing/development.md    # expect ≤ 2400
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs
```

- [ ] **Step 4: Commit**

```bash
git add docs/contributing/development.md
git commit    # docs(contributing): publishing without the release recital
```

---

### Task 7: The length check

**Files:**
- Create: `hack/docs-length.sh`
- Create: `hack/docs-length-test.sh`
- Modify: `Makefile`

**Interfaces:**
- Consumes: all six pages are at or under their ceilings. This task runs last
  because the check fails until they are.
- Follows the shape `hack/toolchain-pins-agree.sh` and
  `hack/image-tag-pins-agree.sh` already set: a script, a `-test.sh` beside
  it that drives it past its own failure, a `<name>-lint` target in `test:`'s
  prerequisites, and a `<name>-lint-test` target that is **not** part of
  `test`.

- [ ] **Step 1: Write `hack/docs-length.sh`**

```bash
#!/usr/bin/env bash
# Refuses a documentation page that has grown past the size its rewrite
# landed on.
#
# The six pages below were 25,829 words on 2026-09-17 and 10,740 after
# docs/superpowers/specs/2026-09-17-docs-shortening-design.md. They had grown
# there once already, which is why a check exists at all rather than a note
# saying to keep them short.
#
# A ceiling is not a judgement about a page: it is roughly 15% above what its
# rewrite landed on, so a paragraph with something to say fits without a
# fight. Raising one is a line here and a sentence in the commit saying what
# the page gained. Generated pages are deliberately absent -- their length is
# their sources' business.
#
# Usage: hack/docs-length.sh [--page FILE:CEILING]...
#
# With no --page, checks the table below. --page is for
# hack/docs-length-test.sh, which has to drive a failure this tree does not
# carry.
set -euo pipefail

PAGES=(
  "docs/guides/upgrading.md:950"
  "docs/getting-started/index.md:1000"
  "docs/guides/rotating-the-forwarding-secret.md:1800"
  "docs/guides/rotating-the-ca.md:1400"
  "docs/explanation/network-boundaries.md:3900"
  "docs/contributing/development.md:3000"
)
```

Then: parse `--page` into `PAGES` when given, `wc -w` each entry, collect
every failure rather than stopping at the first, and exit 1 printing
`<file>: <count> words, ceiling <ceiling>` for each. A missing file is a
failure naming the file, not a silent pass — the table is the only thing that
knows the page should exist.

- [ ] **Step 2: Run it against the tree and watch it pass**

```bash
hack/docs-length.sh ; echo "exit $?"    # expect exit 0
```

- [ ] **Step 3: Write `hack/docs-length-test.sh`**

Four cases, each driving the real script through `--page`, none touching the
tree's own pages:

1. a file under its ceiling → exit 0
2. a file over its ceiling → exit 1, and the message names the file, the
   count and the ceiling
3. a file exactly at its ceiling → exit 0 (the ceiling is inclusive)
4. a named file that does not exist → exit 1 naming it

Build the fixtures in a `mktemp -d` the test removes on exit.

- [ ] **Step 4: Prove the check fails when it should**

Not in the working tree — a mutation there is indistinguishable from the
defect it imitates if something interrupts:

```bash
git worktree add --detach /tmp/docs-length-proof
# append 400 words to docs/guides/rotating-the-ca.md there
/tmp/docs-length-proof/hack/docs-length.sh ; echo "exit $?"   # expect exit 1
git worktree remove --force /tmp/docs-length-proof
```

Record the real failure output in the commit body, and the pass beside it.

- [ ] **Step 5: Wire it into the Makefile**

Add `docs-length-lint` to `test:`'s prerequisite list, beside
`image-tag-lint`:

```make
.PHONY: docs-length-lint
docs-length-lint:
	hack/docs-length.sh

.PHONY: docs-length-lint-test
docs-length-lint-test:
	hack/docs-length-test.sh
```

`docs-length-lint-test` is separate from `test`, the way
`toolchain-lint-test` and `image-tag-pins-agree-test` are.

- [ ] **Step 6: Verify**

```bash
chmod +x hack/docs-length.sh hack/docs-length-test.sh
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs-length-lint
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs-length-lint-test
```

On `dev`, do not run the full `make test` — it starts an envtest control
plane per package. `make docs-length-lint` is what this task needs.

- [ ] **Step 7: Commit**

```bash
git add hack/docs-length.sh hack/docs-length-test.sh Makefile
git commit    # chore(hack): a ceiling per documentation page
```

---

## Final verification

After Task 7, with the whole branch in place:

```bash
for f in docs/guides/upgrading.md docs/getting-started/index.md \
         docs/guides/rotating-the-forwarding-secret.md docs/guides/rotating-the-ca.md \
         docs/explanation/network-boundaries.md docs/contributing/development.md; do
  printf "%6d  %s\n" "$(wc -w < "$f")" "$f"
done
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs
nix --extra-experimental-features 'nix-command flakes' develop /home/paul/git/spawnery -c make docs-length-lint
```

Expected: six numbers under their ceilings summing to under 9,700, a green
`mkdocs build --strict` — which is the proof no anchor died — and a green
length check.

One reading pass per page that no command covers: does the page still answer
the question its nav entry asks, and does every measurement that was in it
still stand with its date?
