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

## A `spec.mounts` entry under `/data` is a fourth writer into it

`extraFiles` reasons about three things writing into a server's working
directory on a start — the renderer, the `extraFiles` copy and the
`extraPlugins` copy — and makes their paths disjoint by refusing a claim that
carries a path one of the others owns. A claim-backed or `ConfigMap`-backed
`spec.mounts` entry nested under `/data` is a fourth, and no scan knows about
it.

A group with a `ConfigMap` mounted at `/data/mods` and an `extraFiles` claim
carrying a top-level `mods/` dies on the copy, because every mount this
operator renders is read-only:

```
cp: can't create 'mods/pack.jar': Read-only file system
```

Under `set -eu` that ends the start, and the message names neither the mount
nor the claim.

**Documented rather than fixed, because it mirrors an accepted risk this code
already carries.** The `chmod` in `image/entrypoint.sh` narrows itself to the
entries it just copied, rather than running `chmod -R u+w .`, for exactly this
reason: a read-only mount somewhere else under `/data` would make the wider
version die the same way — and since 0.2.34 it stops at filesystem boundaries
(`find -xdev`), so a mount nested *inside* a copied directory no longer kills
the start on the chmod either. What remains is the `cp` itself: the
entrypoint cannot tell a read-only mount from a read-only file without probing
every destination before copying, and the operator cannot know what a claim
holds when it admits the group. What it could do is refuse a `spec.mounts`
path under `/data` when the group also names `extraFiles` — which would refuse
the many groups where the two do not overlap at all, to catch the few where
they do.

The remedy is the ordinary one: a mount and an `extraFiles` claim should not
aim at the same directory. Found by reading the design against the collision
check, not by a failure.

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

The assertion prints what a second occurrence needs and the first did not
have: the `Accepted` condition, every pod in the namespace with its deletion
timestamp and node, and whether the pod under the name is still the
predecessor by UID. A lingering predecessor says the force delete did not
take; a Server carrying `PodNameTerminating` with no such pod present says the
controller decided against a pod that is no longer there; an empty namespace
with a clean condition says something else refused the create. The second
occurrence should be a diagnosis, and this entry leaves with it.
