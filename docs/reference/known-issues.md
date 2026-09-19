# Known issues and carry-overs for later milestones

This file carries only problems that still exist. An entry that gets fixed is
deleted, and the account of what it was and how it was found lives in the
commit that removed it — `git log -p docs/reference/known-issues.md` is where to look
for one. A closed entry left standing with a note saying it is closed costs a
reader the same attention as a live one, which is the whole reason for the
rule.

Things that are not open problems live elsewhere, and on 2026-08-27 four of
them moved out of this file to where they belong.
[`upgrading.md`](../guides/upgrading.md) carries what rolls a fleet when an installation
crosses a release, and [`release-notes.md`](../archive/release-notes.md) carries what
strands an object — real work for whoever is upgrading one, and nothing at all
for anyone else.
[`ca-rotation.md`](../guides/rotating-the-ca.md) carries the CA rotation procedure, which is
a thing a human drives rather than a thing that is wrong — including that
nothing schedules one, which is a decision and not an omission, and where the
clock is published so that nobody has to remember.
[`persistent-storage.md`](../guides/persistent-worlds.md) carries what an operator owns
about a persistent group's claims — that this operator never deletes one, that
deleting one deletes a world, and how long a group whose storage is broken
takes to say so.
[`network-boundaries.md`](../explanation/network-boundaries.md) carries what the
`NetworkPolicy` objects buy and what they do not, and what bounds the number of
agents that may reach the operator — measured scope rather than a list of
faults.
[Installing the operator](../getting-started/index.md) carries the manual grant
a chart cannot make for a namespace that does not exist yet, and [Chart
values](chart-values.md) why the digest checked in at any tag describes the
release before it.

**Two of the things this file used to carry were facts about one cluster rather
than about this code, and they now live where that cluster is described** — the
GitOps repository, beside the `HelmRelease` whose arguments they are about.
Anything here should be a claim about this repository; a claim about `paulwtf`
belongs to `paulwtf`.

Older documents in `docs/` name sections of this file that no longer exist.
That is the rule above working, not rot: a handover or a runbook records what
was open at its milestone, and rewriting one to match today would falsify a
record. `git log -p docs/reference/known-issues.md` is where a named section went.

The design decisions live in
`superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`, in
`superpowers/specs/2026-08-08-agent-channel-design.md`, in
`superpowers/specs/2026-08-09-paper-agent-design.md`, in
`superpowers/specs/2026-08-10-proxy-channel-design.md`, in
`superpowers/specs/2026-08-10-velocity-image-design.md` and in
`superpowers/specs/2026-08-11-velocity-agent-design.md`.

## `TestARecreatedOrdinalCreatesItsPodOnceThePredecessorIsGone` failed once and was never reproduced

The test (`internal/controller/server_controller_test.go`) recreates an
ordinal over a pod that is still terminating, lets the pod finish the way a
kubelet would, and expects the next pass to create the successor. It failed
one `make test` with `status.podName` empty, passed in isolation and on a
full rerun, and nothing was captured.

One thing is ruled out rather than assumed: it is not cache lag.
`internal/testenv`'s client is `client.New`, a direct client with no informer
behind it, so the hypothesis anyone reaches for first with envtest cannot be
the mechanism.

Measured 2026-09-19 on paul-desktop, with 16 to 24 test binaries running the
test in parallel beside full package runs: it did not recur in about 26,000
runs. The same runs found a sibling failure in the fixture, about once in
2,700: the API server decided the predecessor's delete on the pod as it was
before its binding and removed it outright, shown by an audit log of the
failing run. The fixture now holds the pod with a finalizer instead. If this
entry's failure is the same stale read in the other direction, the
controller reading the predecessor after the force delete, that would explain
an empty `status.podName` with `PodNameTerminating`; it is not shown.

The assertion prints what a second occurrence needs and the first did not
have: the `Accepted` condition, every pod in the namespace with its deletion
timestamp and node, and whether the pod under the name is still the
predecessor by UID. A lingering predecessor says the force delete did not
take; a Server carrying `PodNameTerminating` with no such pod present says the
controller decided against a pod that is no longer there; an empty namespace
with a clean condition says something else refused the create. The second
occurrence should be a diagnosis, and this entry leaves with it.
