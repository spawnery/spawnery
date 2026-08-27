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

## From the 0.2.5 release

**A Velocity unit test failed once in CI and nothing kept its name.** On
2026-08-27 the `images` job went red on `:velocity:test` — "There were failing
tests" — and a re-run of the *identical* derivation
(`nmxzj8ff3bc740035zy07azdhp7skphv-spawnery-agents-0.2.5.drv`, byte-for-byte
the one this machine had already built) passed. So it is a flake and not a
disagreement between two trees.

Which test it was is not known. Gradle logs each result where it happens, in
the middle of a build log that Nix then reports as its last ten lines, and the
failure's own name was outside that window. The nix store holding the full log
belonged to a runner that is gone.

*What was done about the diagnosis, not about the flake.* The agent build now
throws the failed test names as the Gradle failure text, so they land in the
"What went wrong" block; and the workflows set `log-lines = 40`, because
Gradle's own "Try: run with --stacktrace / --info / --scan" block is eight
lines and pushed even that outside a ten-line window. Both were driven by
mutating a test to fail and reading the result. **Neither of them makes the
test less flaky** — the next occurrence will be diagnosable, which is all.

The likeliest candidate is `ProxyRoleTest`'s 20 000-trial race over
`onFirstSync` and `onSetReady`, which is the only test in these modules that
asserts an interleaving; it is written to catch a regression that landed about
one trial in 570 when the code was wrong, so a failure now would mean a second
and rarer race the current code still has. That is a guess and is written here
as one. Nothing has been measured about it.
