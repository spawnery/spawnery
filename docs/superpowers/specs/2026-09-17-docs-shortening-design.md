# Shortening the six pages phase 3 never turned around

The "show before explain" work rewrote the guides it created and left the
pages it did not touch as they were. Measured on 2026-09-17, those pages are
the whole of what is left of the wall of text.

## What was measured

Words per page, and lines of code block per line of prose:

| Page | Words | Code/prose |
|---|---:|---:|
| `guides/upgrading.md` | 7,880 | 0.12 |
| `explanation/network-boundaries.md` | 4,534 | 0.00 |
| `guides/rotating-the-forwarding-secret.md` | 3,857 | 0.22 |
| `contributing/development.md` | 3,859 | 0.25 |
| `getting-started/index.md` | 2,862 | 0.17 |
| `guides/rotating-the-ca.md` | 2,837 | 0.09 |
| — the pages phase 3 wrote | 772–1,440 | 0.27–3.09 |

The split is clean: every page phase 3 rewrote sits under 1,500 words and
shows something; every page it skipped is two to five times that and mostly
does not.

## The diagnosis

These pages are written as a record of the work that produced the software,
not as an instruction to somebody running it. They carry milestone numbers
("since 6b", "before 4c-3", "7b-3 wrote this section"), task numbers, fix
rounds, what the page used to say, and which alternative was declined. A
reader who was not there cannot use any of it, and it is the majority of the
words.

Three consequences are worth naming on their own, because each is a defect
rather than verbosity:

**`getting-started/index.md` is the chart's README under a different name.**
Its hand-written values table (858 words) duplicates the *generated*
`reference/chart-values.md`, and the two have already drifted: the hand copy
names `image.tag` default `"0.2.12"`, the generated page `0.2.33`. A further
629 words restate `guides/cloud-command.md`, which phase 3 wrote for exactly
that. Half the page is a stale second copy.

**`rotating-the-forwarding-secret.md` explains the same thing twice.** §8's
"One thing the stamp does not say" (272 words) and §9's "When the stamp lies,
and for how long" (642 words) are the same argument in two drafts, in places
sentence for sentence.

**`upgrading.md` is a changelog filed as a guide**, 32 sections ordered by
release. Its own first paragraph says it applies to nobody: *"None of this
applies to an installation created at v0.1.0 or later — the only one that
exists was installed 2026-08-20."* What is true of every upgrade — that an
operator upgrade can roll the whole proxy fleet, and that proxy images go
before the operator — is 870 of its 7,880 words.

## What does not get cut

`CLAUDE.md` binds this: *"Docs and code comments in this repo record
measurements ('measured on …') rather than assumptions. Keep that when adding
to them."*

So the rule is not "remove the measurements". It is:

- **Keep** the measured fact, its date, and the cluster or harness it was
  measured on. "Measured 2026-08-21 on Cilium: a pod wearing the proxy labels
  reached a backend on 25565, the same pod without them timed out" stays as
  it is.
- **Cut** the provenance around it — which milestone, which task, which fix
  round, what the page said before, which alternative was considered and
  declined. That belongs to the commit that made the change, where `git
  blame` finds it from the line itself.

The exception to the second half: an alternative stays when the reader would
otherwise try it. "Do not reach for a drain mid-rotation" is an instruction,
not a history.

## The three cutting rules

1. **The reader operates a cluster; they did not watch it being built.**
   Milestone and task numbers, handover references, "this used to read", and
   paths that no longer exist go. A date stays.
2. **No page explains what another page explains.** The duplicate goes and a
   link replaces it. Where the other page is generated, the hand copy always
   loses.
3. **A guide opens with what to run.** Same rule phase 3 followed. An
   explanation page may open with prose; a guide may not.

## Per page

Budgets are ceilings, not targets. Each is roughly 15% above what the cut
should land on, so that a later paragraph with something to say fits without
a fight.

### `guides/upgrading.md` — 7,880 → ≤ 1,000

Keeps only what is true of any upgrade between any two releases:

- what makes a fleet roll (the pod hash is a digest of the rendered pod, so
  rendering code and the agent endpoint move it while every spec stays
  byte-identical), and how to find out before upgrading rather than after;
- why proxy images go before the operator;
- the commands that say whether a group is mid-roll.

Everything named after a version, and every pre-v0.1.0 migration, moves
verbatim to `docs/archive/release-notes.md`. Verbatim is deliberate: the
archive is a record, and rewriting a record costs effort to make it less
true.

### `getting-started/index.md` — 2,862 → ≤ 800

Becomes the Install page its nav entry already calls it. Keeps: the install
command, choosing a game namespace, the one manual RBAC step, uninstalling.

Deletes the values table — `reference/chart-values.md` is generated from the
schema by `make manifests` and cannot drift — and the `/cloud` permissions
section, which `guides/cloud-command.md` carries.

The page title becomes `Installing the operator`. It currently reads
`spawnery`, which is the chart's name and tells a reader nothing.

### `guides/rotating-the-forwarding-secret.md` — 3,857 → ≤ 1,300

The procedure (§1–§3) stays and leads, tightened. §5's argument for rolling
server groups first compresses into the step that needs it. §6's two warnings
stay — both are things a reader would otherwise get wrong. §7 rollback stays.

§8's two condition tables stay; the prose around them goes. §9 goes entirely,
except for the one fact §8 does not carry — that a pod created between the
secret changing and the operator recording the new digest is stamped with the
old one — which becomes a short note under the `PodsPredateTracking` row.

The `Status: standing operating procedure` block at the top goes: it explains
this document's relationship to the archive, which is a question no reader
has.

The filename does not change. `internal/controller/forwardingsecret.go:37`
pins it as a constant, and `internal/podspec/labels.go:73` names it in a
comment.

### `guides/rotating-the-ca.md` — 2,837 → ≤ 1,000

"The sequence" (359 words) already does the right thing and stays nearly as
it is. "How the rotation works" is 2,388 words in one section and becomes
several, keeping: the two waits and what they are for, the up-to-an-hour
pickup and the way to skip it, what blocks the gate, how a refusal is
recorded, and the expiry metrics with the reason nothing rotates on its own.

The forensics of a hand-edited rotation slot — roughly 900 words on
`RotationSlotTruncated` and `RotationSlotDiscarded` and what each does per
phase — compress to a warning of about 150 words. The page itself says the
procedure cannot produce that state: *"Only a hand-edited or truncated secret
reaches this; nothing the procedure itself does can."* What a reader needs is
that a damaged `ca-previous.crt` at `switched` completes the irreversible
drop within one 30-second tick, and that the secret is not to be hand-edited.
`internal/certs/rotation.go` carries the rest.

Filename unchanged; `internal/certs/events.go:47` names it.

### `explanation/network-boundaries.md` — 4,534 → ≤ 2,300

An explanation page is allowed to be the longest of the six, and this one
stays the longest. What goes is provenance, not substance: the milestone
numbers throughout, the `config/deploy/` paths kept "where they date a
measurement" (the date is what dates it), the paragraph about what 6a's
handover owed, and "this entry used to read".

That the harness's CNI enforces nothing is stated five times. It stays once,
in the opening, where every later section can lean on it.

"What the operator knows about a person" is 1,118 words across four "And
since 7b-N" paragraphs that each add a widening to the last. It becomes one
statement of what the operator holds today, what bounds it, and the one thing
that is not bounded.

Every measurement and every date stays.

### `contributing/development.md` — 3,859 → ≤ 2,100

"Publishing" is 1,060 words and becomes about 400. The recital of which
release moved which version number, and the story of `v0.2.14`'s first
attempt failing its own preflight, are commit messages. What stays: what each
script does, that they reach the network and are part of no other target,
`DRY_RUN=1`, and that a gap in a version sequence is a release that built
nothing on that side.

"Trying it locally against kind" keeps its commands and its real traps — the
rootless-Podman relay, the `systemd-run` scope, `--operator-namespace` — and
loses the narrative of discovering that k3d cannot work, which becomes a
sentence.

## The archive page

`docs/archive/release-notes.md`, added to the Archive nav under "What this
is" and above "How it was built". A short header says what it is: the
release-by-release notes that were `guides/upgrading.md` until 2026-09-17,
kept because they describe upgrades an installation older than this one would
still meet.

`hack/image-tag-pins-agree.sh` exempts `docs/guides/upgrading.md` by name in
its header comment, "whose version notes name old tags on purpose". The
exemption moves to the new page, and `docs/archive/` is already exempt, so
the script needs only its comment corrected.

## Links that break

`mkdocs build --strict` with `validation.links.anchors: warn` is the check —
a warning is an error under `--strict`, so every one of these fails `make
docs` if it is left wrong.

| Link | In | Becomes |
|---|---|---|
| `getting-started/index.md#the-cloud-permissions` | `explanation/network-boundaries.md:458` | `../guides/cloud-command.md` |
| `getting-started/index.md#choosing-a-game-namespace` | `docs/index.md:39` | anchor must survive the rewrite |
| `getting-started/index.md#the-one-manual-step-this-chart-cannot-make` | `tutorial/index.md:92` | anchor must survive, or both sides move together |
| `upgrading.md#0233-a-groups-scheduling-needs-the-networks-permission` | `guides/scheduling.md:152` | the archive page's anchor |
| `upgrading.md#the-agents-gain-a-cloud-command-granted-to-nobody` | `guides/cloud-command.md:99` | the archive page's anchor |
| `network-boundaries.md#how-many-agents-may-reach-the-operator` | `explanation/agent-trust.md:70` | anchor must survive the rewrite |

Two links stay valid as paths and stop being true as sentences, because what
they describe moves to the archive. Both get their sentence corrected rather
than only their target:

- `reference/known-issues.md:12` says `upgrading.md` "carries what strands an
  object or rolls a fleet when an installation crosses a release". After the
  cut it carries the second half only; what strands an object is in the
  archive.
- `contributing/development.md:100` sends a reader to `upgrading.md` for what
  an installation does about Paper's deprecation, which is a version-numbered
  note and moves.

Also to repair, outside `docs/`: `charts/spawnery/Chart.yaml:98` points a
German comment at `docs/guides/upgrading.md` for the 0.2.33 scheduling note,
which moves to the archive. The path changes; the German stays German.

## The guard

`hack/docs-length.sh` holds the table of page → ceiling above and fails when
a page exceeds its own. `make test` runs it, beside
`hack/image-tag-pins-agree.sh` and `hack/toolchain-pins-agree.sh`.

This is a decision rather than a requirement, and its cost is real: a word
count is a crude proxy for whether a page is any good, and a genuinely needed
paragraph will one day hit a ceiling and have to be argued past by editing
the table. It is in because this is the second time these pages have been
measured and found long, and because the alternative — noticing again in six
months — is what actually happened. Raising a ceiling is one line and a
sentence in the commit.

Counted the way the measurements above were: words in the Markdown source,
code blocks included, which is what `wc -w` gives.

## Verification

- `make docs` — `mkdocs build --strict` is the project's only link checker,
  and it fails on a dead anchor. Every page's inbound links survive or are
  repaired.
- `make test` — `hack/docs-length.sh` passes at the new sizes, and the
  mutation that proves it is a page pushed over its ceiling in a throwaway
  worktree.
- A reading pass per page: does it still answer the question its nav entry
  asks, and does every measurement that was in it still stand?
- No fact is deleted that appears nowhere else. Where a cut removes the last
  copy of something, it moves rather than goes.

## Out of scope

`reference/crds.md`, `reference/chart-values.md`,
`reference/metrics-and-alerts.md` and `plugin-api/javadoc/` are generated —
their length is their sources' business. `docs/archive/` and
`docs/superpowers/` are records and stay as they are. The pages phase 3
rewrote are not reopened.
