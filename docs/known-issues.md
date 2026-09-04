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
version die the same way. The entrypoint cannot tell a read-only mount from a
read-only file without probing every destination before copying, and the
operator cannot know what a claim holds when it admits the group. What it
could do is refuse a `spec.mounts` path under `/data` when the group also
names `extraFiles` — which would refuse the many groups where the two do not
overlap at all, to catch the few where they do.

The remedy is the ordinary one: a mount and an `extraFiles` claim should not
aim at the same directory. Found by reading the design against the collision
check, not by a failure.

## A capacity edit still clears a group's failure streak

`ofGeneration` (`internal/controller/servergroup_controller.go`) narrows an
ephemeral group's views to the current `metadata.generation` before
`CountFailures` runs, so raising `minReplicas` resets
`status.consecutiveFailures` and clears a `Degraded` condition. Scaling a group
up is not a fix for whatever its servers were failing on.

Milestone 7a moved every other staleness comparison onto
`podspec.DesiredServerHash` and deliberately did not move this one. Failure
counting runs unconditionally and early, before the hash is computed, and that
ordering is itself a decision: the hash is gated on the group's Network being
usable, and a group whose Network was deleted is exactly the one that piles
failures up. Replacing a value that is always present with one that is
sometimes empty, on that path, is the wrong trade.

**This entry once said the `/cloud` milestone would inherit the hazard, and it
will not.** The claim was that `/cloud start lobby` would be a spec edit an
admin types, and so would clear a `CrashLoopBackoff` and start hammering a
broken image from a fresh window. That command turned out not to edit the
group at all: the operator has no write access to a `ServerGroup`'s spec, and
the one on this project's own cluster is Flux-managed, so a `minReplicas` it
wrote would be reverted. Extra capacity became its own object instead
(`ScaleBoost`), which does not move `metadata.generation` and therefore cannot
touch the streak.

So what remains is the plain fact above and no consequence beyond it: a
**person** editing `minReplicas` still clears their group's failure count.
Whether that is worth fixing is a smaller question than it looked, and nothing
currently forces it.

Found by reading, not by a failure: writing 7a's plan against the code showed
the ordering, and the task that would have changed this was removed from the
plan rather than left to fail.

## A server is joinable before its plugins have finished

Readiness is one server list ping. `cmd/spawnery-slp` performs it and turns
the result into an exit code; `internal/podspec` wires it as the exec probe of
the Paper image and passes nothing but host and port. A Minecraft server
answers that ping as soon as its listener is up, which is before it has
finished enabling plugins — measured 2026-09-04 on `hub-dvjk`, where
`Starting Minecraft server on *:25565` and `Done (35.266s)` are twenty seconds
apart.

A plugin whose initialisation continues past that point leaves a window in
which the server is Ready, the proxy will route a player to it, and the player
is refused. ViaVersion is the one that shows it: it loads its protocol
mappings on its own executor, and on two starts of the same group it finished
once before `Done` and once after it. A client connecting inside that window
meets the unmodified version check and is told

```
Outdated client! Please use 26.2
```

which is exactly wrong — the client is fine, the translation layer is not up
yet. Waiting a few seconds and connecting again works.

**The probe is not the thing to fix.** A server list ping is what a Minecraft
server offers before anything else answers at all; asking it to mean "every
plugin has finished" is asking the wrong party, and no timeout added to it
would be more than a guess at how long somebody's plugin takes. What knows
the answer is the server itself: the agent runs inside it and could report
readiness when the server has finished enabling rather than when its socket
opens. Whether that is worth the coupling has not been decided, and this entry
does not decide it.

Reported from play on the `paulwtf` installation and confirmed by reading two
pod logs, not by a failing test.
