# Show before explain

What the remaining documentation phases become, after the site went up and its
first reader said it reads like a thesis.

This supersedes phases 3 to 5 of
`2026-09-14-documentation-site-design.md`. Phases 1 and 2 stand as built: the
site, and the reference that generates itself. Nothing here revisits them.

## The finding

Paul, 2026-09-15, after skimming `docs.spawnery.cloud`:

> die statische doku sieht mehr wie eine doktorarbeit aus als nach einer doku
> … einfach eine wand aus text ohne anwendungsbeispielen bzw beispielen aus
> der praxis. jemand der die doku tatsächlich braucht … wird sich das dann
> erst recht nicht durchlesen.

Measured, and it holds:

| Page | Words | Code blocks | Words per example |
|---|---:|---:|---:|
| `explanation/network-boundaries.md` | 4,470 | **0** | — |
| `guides/rotating-the-ca.md` | 2,394 | 1 | 2,394 |
| `guides/upgrading.md` | 7,880 | 15 | 525 |
| `guides/persistent-worlds.md` | 2,096 | 4 (no YAML) | 524 |
| `index.md` | 423 | 2 | 211 |

About 20,000 words of prose across the guides. `network-boundaries.md` runs
184 lines before its first subheading.

**But the quantity is not the finding. The absence is.** There is no run
through. "Getting started" is 2,862 words of installation reference; nothing
anywhere says *here is a network from nothing to a player standing in it, and
here is what you see at each step*. `config/samples/network.yaml` exists and is
linked, never shown.

### Why it happened, because the cause decides the fix

This repository writes well, and it writes for a particular reader: somebody
**maintaining** the system. Explain why a thing is the way it is, name the
measurement, record the alternative that was rejected. In commit messages and
doc comments that is the right register and it is the reason this codebase is
navigable at all.

Documentation for somebody **adopting** the system needs the other order. Show
the thing working, then explain it to whoever stays. Phase 1 deliberately moved
the existing pages without touching their prose — the right call for a
restructuring phase, and it lifted that register onto a public site unchanged,
where it reads as a thesis.

So the fix is not "write more documentation" and not "delete the prose". The
prose is good and it stays. It goes *after* the thing a reader can copy.

## What changes

### The tutorial, which does not exist and is the whole point

A new top-level section, before Guides: **one** tutorial that takes a reader
from an empty machine to a Minecraft client standing on a server the operator
created. Not a tour of features — one path, no branches, no "you could also".

It has to be followable by somebody who has none of Paul's infrastructure, so
it runs on `kind`. That is also the only environment this project can drive
itself, which matters for the next section.

Roughly: create the cluster, install the chart, apply a `Network` and one
ephemeral `ServerGroup` and one `ProxyGroup`, watch `kubectl get servergroups`
fill in, connect a client, watch the group scale when you join. Each step shows
the command, then what comes back — the actual output, not a description of
it.

Where a step has a reason worth knowing, the tutorial gives one sentence and a
link. It does not explain the drain protocol.

### The tutorial is executed, not proofread

Phase 2's thesis was that a generated page cannot go stale. The equivalent for
a tutorial is that an **executed** one cannot: its manifests live in the
repository, and the end-to-end suite applies them.

This is not new machinery. `test/e2e/lifecycle_test.go:34` already
dry-run-applies `config/samples/network.yaml` so that the sample in the docs is
checked. The tutorial's manifests extend that habit from a dry run to a real
one, in the suite that already builds a kind cluster and already installs the
chart.

**The decision, and the one to check first:** the manifests the tutorial page
shows are the ones CI applies. They live in the repository under the tutorial,
and the e2e suite reads them from there — it does not keep a lookalike beside
`test/e2e/manifests/e2e.yaml`, which stays what it is: the driven run's own
fixture, deliberately not the sample.

A tutorial that drifts from what the operator accepts is worse than no
tutorial, because it fails for a reader in their own cluster and they have no
way to tell whether the fault is theirs.

What this cannot check is the prose between the commands, or the output the
page claims each one prints. That stays a human's job and the spec does not
pretend otherwise.

*Cost if wrong:* the e2e job grows by the tutorial's path, on a suite already
taking 7m42s in CI. If that proves too much, the fallback is a dry-run check of
the tutorial's manifests alone — cheaper, still better than nothing, and
strictly weaker.

### Every guide opens with something to copy

The nine existing guides and the five new ones take one shape:

1. **What you are doing**, one or two sentences.
2. **The YAML or the command**, complete and runnable — not a fragment with an
   ellipsis.
3. **What you see**, the real output.
4. **Then the reasoning**, which for the existing pages is the prose already
   there, moved rather than rewritten.

Point 4 is the constraint that keeps this from becoming a rewrite. The existing
reasoning is good and it is not being replaced — it is being put after the
thing a reader came for. A reviewer should be able to check that no paragraph
was lost, only relocated.

Where a guide genuinely has no example — rotating the CA is a procedure, not a
manifest — it opens with the command sequence instead. `rotating-the-ca.md` at
2,394 words and one code block is the page this rule exists for.

### The five missing guides, written in that shape from the start

Expose strategies, scaling and boosts, updates and drain, scheduling, and the
`/cloud` command — the same list as before, but each opens with a working
example rather than ending with one.

### Explanation stays explanation

`explanation/network-boundaries.md` has no code blocks and that is **correct**.
Explanation is not a tutorial and does not become one by adding YAML to it.

What is wrong there is navigation, not register: 184 lines before the first
subheading. It gains subheadings and an opening paragraph that says what the
page will and will not tell you. Its content is not touched.

The two explanation pages the earlier spec still owes — the architecture, and
the agent channel's trust model — are written to that same standard.

### `history.md`, unchanged in intent

Still two entries against 94 `feat` commits since the rollout. Still told as
six or seven chapters rather than listed. Nothing about the finding changes
this; it is in the archive, where a reader who wants the story goes on purpose.

## What this does not do

- **It does not delete prose.** Every word currently on the site that is
  accurate stays somewhere. The complaint was about order, not truth.
- **It does not restructure the nav again.** Phase 1's eight sections stand,
  with Tutorial added before Guides.
- **It does not touch the reference pages.** They are generated, they are
  tables, and a table is already the shape a reader scans.
- **It does not add a second tutorial.** One path. A second is how a tutorial
  section becomes a guide section with worse names.

## Phasing

| | | |
|---|---|---|
| 3a | The tutorial, its manifests, and the e2e path that runs them | A reader can get to a joined player |
| 3b | The nine existing guides turned around | Every guide opens with something to copy |
| 3c | The five missing guides, in the new shape | The feature surface is documented |
| 4 | The plugin API's prose pages; `agent/api/README.md` cut to its Maven Central role | Its Javadoc already shipped with phase 2 |
| 5 | The two explanation pages, the archive frame, `history.md` brought forward | |

3a is the one worth doing first and alone: it is the missing thing, it is the
only part with a build dependency, and until it exists the site has no front
door. 3b and 3c are prose and need no plan document — the shape above is the
spec, and `mkdocs build --strict` plus somebody reading them is the test.

**3a gets an implementation plan.** Nothing else here does.

## Open question for the author

The tutorial needs a Minecraft client to reach a `kind` cluster for its last
step, and that is the one thing CI cannot do — the end-to-end suite has never
had a licensed client in it, and the runbooks record every join as
hand-driven.

So the tutorial's final step is either (a) written from a hand-driven run,
recorded once, with the page saying plainly that the join was driven by a
person on a named date; or (b) stopped one step earlier, at `kubectl get
servers` showing a `Ready` backend and the proxy's address, leaving the join to
the reader.

This spec assumes **(a)**, because a tutorial that stops before the payoff is a
tutorial nobody finishes, and because this project already records driven runs
that way. It costs one hand-driven session, and the date on it will age.
