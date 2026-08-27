# Known issues and carry-overs for later milestones

This file carries only problems that still exist. An entry that gets fixed is
deleted, and the account of what it was and how it was found lives in the
commit that removed it — `git log -p docs/known-issues.md` is where to look
for one. A closed entry left standing with a note saying it is closed costs a
reader the same attention as a live one, which is the whole reason for the
rule.

Things that are not open problems live elsewhere, and on 2026-08-27 four of
them moved out of this file to where they belong.
[`upgrading.md`](upgrading.md) carries what strands an object or rolls a fleet
when an installation crosses a release — real work for whoever is upgrading
one, and nothing at all for anyone else.
[`ca-rotation.md`](ca-rotation.md) carries the CA rotation procedure, which is
a thing a human drives rather than a thing that is wrong — including that
nothing schedules one, which is a decision and not an omission, and where the
clock is published so that nobody has to remember.
[`persistent-storage.md`](persistent-storage.md) carries what an operator owns
about a persistent group's claims — that this operator never deletes one, that
deleting one deletes a world, and how long a group whose storage is broken
takes to say so.
[`network-boundaries.md`](network-boundaries.md) carries what the
`NetworkPolicy` objects buy and what they do not, and what bounds the number of
agents that may reach the operator — measured scope rather than a list of
faults.
[`charts/spawnery/README.md`](../charts/spawnery/README.md) carries the manual
grant a chart cannot make for a namespace that does not exist yet, and why the
digest checked in at any tag describes the release before it.

**Two of the things this file used to carry were facts about one cluster rather
than about this code, and they now live where that cluster is described** — the
GitOps repository, beside the `HelmRelease` whose arguments they are about.
Anything here should be a claim about this repository; a claim about `paulwtf`
belongs to `paulwtf`.

Older documents in `docs/` name sections of this file that no longer exist.
That is the rule above working, not rot: a handover or a runbook records what
was open at its milestone, and rewriting one to match today would falsify a
record. `git log -p docs/known-issues.md` is where a named section went.

The design decisions live in
`superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`, in
`superpowers/specs/2026-08-08-agent-channel-design.md`, in
`superpowers/specs/2026-08-09-paper-agent-design.md`, in
`superpowers/specs/2026-08-10-proxy-channel-design.md`, in
`superpowers/specs/2026-08-10-velocity-image-design.md` and in
`superpowers/specs/2026-08-11-velocity-agent-design.md`.

## Nothing is open

There is no entry in this file today. That is a statement about 2026-08-27 and
not a claim about the code: the last one was closed by asking the proxy for the
number the operator had been assuming, and the next defect anybody finds
belongs here the moment they find it.

What the file is for, and the rule it keeps, is above.
