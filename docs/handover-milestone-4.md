# Handover to milestone 4

Status: end of milestone 3c, the Velocity agent (2026-08-11). Updated
2026-08-12 with what the evidence run below actually measured against a real
cluster, and again on 2026-08-13 with the manual session that closed the two
criteria that run left open; nothing before that section changed.

This document is not a spec. It says where 3c stopped and what milestone 4 —
scaling and drain — already finds in place. The design decisions live in
`docs/superpowers/specs/2026-08-11-velocity-agent-design.md`; the open points
are in `docs/known-issues.md`, whose "From milestone 3c" section this
document does not repeat in full.

## Where we are

A `Server` reaches phase `Ready` (milestone 2c), and now so does a
`ProxyGroup`: the Velocity agent opens `ProxySession`, mirrors the operator's
server list into Velocity's own registry, binds its readiness port on the
first `FullSync` and not before, routes a joining player by the group's
`fallbackGroups` try-list, and moves players off a backend the operator is
draining. Both halves of milestone 3's success criterion — a player can join,
automated and by hand — are implemented, and both now hold against a real
cluster: the evidence run below proved the automated half, and the manual
session after it proved the other half and the drain.

## The evidence run

`docs/runbook-milestone-3-evidence.md` was run against a real `kind` cluster
on 2026-08-12: kind v0.32.0, Kubernetes v1.36.1, rootless Podman, one
control-plane node, 8 GiB RAM and 8 vCPU, images
`ghcr.io/spawnery/paper:26.2-0.2.0` and `ghcr.io/spawnery/velocity:3.5.1-0.2.0`,
operator run outside the cluster through `go run` with a socat relay on the
kind network. Six defects in the runbook itself stopped the run at various
points and are now corrected there; they are not repeated here.

**Criterion 7 — a player can join, automated. PROVEN.** Clean run, exit 0:

```
$ spawnery-join --host 127.0.0.1 --port 30565 --hold 45s --timeout 75s
{"protocol":776,"username":"spawnery_probe","uuid":"bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f","compressed":true}
```

Velocity's own log (`gateway-auto`) and Paper's (`lobby-q7mv`), the same
second:

```
[06:01:39 INFO]: [server connection] spawnery_probe -> lobby-q7mv has connected
[06:01:39 INFO]: UUID of player spawnery_probe is bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f
```

On an earlier run in the same cluster, `kubectl get proxygroup gateway-auto
-o jsonpath='{.status.connectedPlayers}'` read **`1`** during the hold,
confirming the whole-branch review's prediction that a held connection is
counted.

Routing honoured the try list: `fallbackGroups: [lobby, hub]`, and the player
landed in `lobby`.

**The forwarding chain is proven live**, read directly out of the running
pods rather than inferred. Velocity's `/data/velocity.toml`:

```
bind = '0.0.0.0:25565'
config-version = '2.8'
forwarding-secret-file = '/etc/spawnery/forwarding.secret'
online-mode = false
player-info-forwarding-mode = 'modern'
show-max-players = 100
```

Paper's `/data/config/paper-global.yml`, under `proxies.velocity`, **as
Paper itself wrote it back**:

```
    enabled: true
    online-mode: true
    secret: <redacted>
```

and `server.properties` carries `online-mode=false`.

That `enabled: true` is the milestone's most important single artifact and
deserves to be called out as such: before `494fa47` fixed the rendered key
from `secret-key` to `secret`, Paper's own post-processing set this to
`false` and logged why in every container since milestone 3b (see
`docs/known-issues.md`, "From milestone 3c"). This is the first time the
forwarding chain has been observed working end to end, not merely rendered
correctly on disk.

`spec.config.onlineMode: false` reaching `online-mode = false` in the
rendered TOML is the second artifact worth naming: it is the CRD field added
in `14331b2`, doing exactly what it was added to do.

**Criterion 8 — a player can join manually, with a real Microsoft account —
was not attempted in this run.** It needs a licensed Minecraft client and a
person to drive it, neither available in this session.
`docs/runbook-milestone-3-evidence.md` §10, "The manual proof, for a later
session", was written for whoever ran it next. That session happened the
following day and is recorded under "The manual session" below.

**Criterion 9 — deleting a `Server` moves a connected player rather than
disconnecting them — could not be proven by this run, and the reason is its
most important finding.** Deleting a `Server` with a `spawnery-join --hold`
player on it disconnected the player instead of moving them. The defect is
in the evidence tool's fit for this criterion, not in the drain logic: a
held join never reaches the point where Paper counts it as an online player,
so `Server.status.players` reads zero for a connection the proxy is still
holding, and the drain's own exit condition
(`internal/phase/phase.go:224`, `if !in.Occupied()`) reads that zero and
deletes the pod. Full diagnosis, the measured Kubernetes events, and why
prior reviews missed it are in `docs/known-issues.md`, "From the milestone 3c
evidence run (2026-08-12)". Two things follow from it, kept
separate there: criterion 9 can only be proven manually until
`cmd/spawnery-join` plays the configuration phase through, and a narrower
product finding — a player connected at the proxy but not yet counted by the
backend sits outside the drain's protection today — that belongs to
milestone 4's own design work on drain, not to this evidence tool.

## The manual session

`docs/runbook-milestone-3-evidence.md` §10 was run on 2026-08-13, on a
different machine from the day before (NixOS, 93 GiB RAM, rootless Podman
5.8.4, kind v0.32.0, Kubernetes v1.36.1), against a fresh `spawnery-evidence`
cluster built from §0 upward exactly as §10 instructs. The runbook needed no
correction this time: every section ran as written, and all four pods reached
`Ready` 21 seconds after `kubectl apply`. Log timestamps below are the
containers' own clock (UTC); the host ran CEST, two hours ahead.

**Criterion 7 re-confirmed first, before spending the account's login** — §10
asks for this so that an environment problem cannot be mistaken for a product
one:

```
$ spawnery-join --host 127.0.0.1 --port 30565 --hold 60s --timeout 90s
{"protocol":776,"username":"spawnery_probe","uuid":"bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f","compressed":true}
exit=0
```

`gateway-auto.status.connectedPlayers` read `1` six seconds into the hold —
faster than the runbook's own "not in the first ten seconds" caution
suggests, so that caution is a floor and not a measurement. Both log lines
appeared as on 2026-08-12, this time naming `lobby-6yw2`.

**That `online-mode` was really on for the manual proof was measured, not
assumed.** `gateway-manual`'s `/data/velocity.toml`, read out of the running
pod, carried `online-mode = true` and `player-info-forwarding-mode =
'modern'`; and `spawnery-join` pointed at 30566 was refused exactly where it
should be:

```
spawnery-join: the server is in online mode and asked for encryption, which this client cannot answer
```

That refusal does double duty — it proves the NodePort is reachable from the
host and that a real Mojang session is genuinely being demanded there. It is
worth running before the manual join for that reason.

**Criterion 8 — a player can join manually, with a real Microsoft account.
PROVEN.** A licensed Minecraft Java 26.2 client on the cluster host joined
`127.0.0.1:30566`. Velocity's log (`gateway-manual`) and Paper's (`lobby-6yw2`):

```
[15:04:49 INFO]: [connected player] paul_wtf (/10.244.0.1:50113) has connected
[15:04:49 INFO]: [server connection] paul_wtf -> lobby-6yw2 has connected
[15:04:49 INFO]: UUID of player paul_wtf is 836fe395-9e8b-4985-b8c9-cc93afe43995
[15:04:50 INFO]: paul_wtf joined the game
[15:04:50 INFO]: paul_wtf[/10.244.0.1:35400] logged in with entity id 16 at ([minecraft:overworld]-21.5, 71.0, 40.5)
```

**The UUID is the artifact, and it reads as one against the probe's.**
`836fe395-9e8b-4985-b8c9-cc93afe43995` is version 4 — the `4` leading the
third group — a UUID Mojang minted and handed back only after the client
proved its session. `spawnery_probe`'s `bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f`
is version 3, the name-derived offline form, which proves nothing about who
connected. The two sit side by side in the same cluster's logs, an hour
apart, and the difference between them is the whole of what
`online-mode: true` buys. `paul_wtf joined the game` is the second half of
it: unlike the held probe, this client completed the configuration phase, so
Paper counted it and `server/lobby-6yw2` showed `PLAYERS 1`.

**Criterion 9 — deleting a `Server` moves a connected player rather than
disconnecting them. PROVEN, manually, on that same live player**, which is
the only way it could be proven at all (see the finding above). `kubectl
delete server lobby-6yw2` while the account was in the game:

```
15:05:25  DeletionRequested  server/lobby-6yw2  phase Ready -> Draining: deletion requested, moving players off
15:05:25  [gateway-manual]   [server connection] paul_wtf -> hub-tmdd has connected
15:05:25  [gateway-manual]   [server connection] paul_wtf -> lobby-6yw2 has disconnected
15:05:26  [hub-tmdd]         UUID of player paul_wtf is 836fe395-9e8b-4985-b8c9-cc93afe43995
15:05:26  [hub-tmdd]         paul_wtf joined the game
15:05:26  [hub-tmdd]         paul_wtf[/10.244.0.1:49170] logged in with entity id 29 at ([minecraft:overworld]-92.5, 73.0, -180.5)
15:05:30  PodDeleted         server/lobby-6yw2  deleted pod lobby-6yw2: no players left
15:05:30  Drained            server/lobby-6yw2  phase Draining -> Terminating: no players left
```

**Three things in that sequence carry the proof, and each is worth naming.**

1. **The new connection precedes the old one's close**, in Velocity's own
   log and in that order. That is a move, not a reconnect after a drop.
2. **`no players left` arrives *after* the move, not during it.** The
   2026-08-12 failure logged the identical message while the player was still
   attached — same words, opposite meaning. Here the drain waited, because
   `Server.status.players` actually held the player this time, which is
   precisely the count the held probe could never produce.
3. **The player saw no disconnect screen**, reported by the person driving
   the client. The logs prove what the proxy did; only they could attest to
   what the game showed, and it showed an uninterrupted session that woke up
   in a different world.

The move landed in `hub`, not in another `lobby` server — the fall-through
§8a describes: `lobby` held exactly one server, `Router.choose`'s exclusion
emptied that group, and the try list went on to the second one rather than
giving up. `agent/velocity/.../Drain.kt` logged no `spawnery:` line at all,
which is its documented silence on success. The `ServerGroup` then brought
`lobby` back to `minReplicas` on its own as `lobby-svq7`.

**Milestone 3's acceptance is therefore closed in full**: criteria 7, 8 and 9
are all proven against a real cluster. What is *not* closed by this session is
finding 2 above — a player connected at the proxy but not yet counted by the
backend still sits outside `Occupied()`'s protection. A real client crosses
that window in a single round trip, which is why this session succeeded where
the held probe failed; the window is narrow, not absent, and deciding what to
do about it remains milestone 4's.

## The one contract change milestone 4 has to make

`internal/agent/registry.go` cannot express "connected, but no longer
ready." `Registry.MarkReady` is only ever called on `Hello{ready:true}` or
the standalone `Ready` message; `Hello{ready:false}` is a no-op once
readiness has latched (`docs/known-issues.md`, the milestone 2c precondition
this repeats because milestone 4 is where it stops being avoidable). Milestone
2c's Paper agent never needed to lower readiness — a server latches ready and
stays that way even if its stream later breaks — and 3a built the proxy's
readiness the same way on purpose: "a proxy's readiness startup-only: once
ready, a proxy stays ready even if its stream later breaks" (design §3, §6.6).
3c inherited that and did not change it: `ReadyGate.open()` is reachable only
from the first `FullSync`, `ReadyGate.close()` only from `onShutdown`, and
nothing in `ProxyRole` ever asks the gate to close while the proxy is still
running.

That is exactly backwards from what proxy drain needs. Draining a proxy means:
stop sending it new players while it still serves the ones already
connected — which is "connected, but no longer ready" stated plainly, the
same shape `Hello{ready:false}` cannot express for a server agent and the
same reason it was left unfixed there. Milestone 4 cannot work around this
the way 3a and 3c did by simply not needing it; a `ProxyGroup` that scales
down or rolls an update has no way today to take a proxy out of a Service's
endpoints without disconnecting everyone on it in the same step; see
"`ProxyGroupReconciler.pods()` has no expectations tracking" in
`docs/known-issues.md` for the concrete failure this produces today.

The shape of the fix is a milestone 2a change, not a milestone 4-local one:
`internal/agent/registry.go`'s entry needs a way to carry "ready" separately
from "connected" so a proxy can lower the former without dropping the
latter, `internal/agentserver` needs a message or a field that lets an agent
say it, and the Velocity agent needs to call `ReadyGate.close()` from
somewhere other than shutdown — on receipt of that message, most plausibly a
new `OperatorToProxy` case sent when the operator decides a `ProxyGroup` is
draining a specific pod. None of that exists yet; all of it is milestone 4's
to design.

## What 3c leaves open, briefly

`docs/known-issues.md`'s "From milestone 3c" section is the full list; the
entries most relevant to this milestone's own scope, restated in one line
each:

- **Per-proxy load balancing.** With several proxies, placement is even per
  proxy and not necessarily across the network — `Router` only ever sees the
  players Velocity itself can see. Worth revisiting once milestone 4 makes
  proxy replica counts move.
- **The NetworkPolicy restricting backends to proxies-only is overdue, not
  deferred**, now that `online-mode=false` on the backends and forwarding
  actually working make the invariant it would guard real. Milestone 6 owns
  it, but a scaling milestone that adds and removes pods more often is where
  the exposure gets exercised more, not less.
- **The ready port is spelled in two languages** —
  `internal/podspec.ProxyReadyPort` and a Kotlin constant in
  `agent/velocity` — with nothing that fails if they diverge except the
  level-2 harness, and only when it runs.
- **A proxy that cannot bind its ready port is silent on the CR**; `Pending`
  with the reason only in the container log. Anyone building milestone 4's
  drain signalling on top of `ProxyGroup.status` should notice this gap
  rather than assume the status already carries every failure mode a proxy
  pod can hit.

## What 3c built that milestone 4 gets almost for free

**Backend drain already routes through the proxy correctly**, and milestone
4 does not have to touch it to add proxy drain on top. `agent/velocity/.../Drain.kt`
receives `DrainPlayers{fromServer, toGroups}` on every repeated send — the
operator resends it alongside `FullSync` roughly every 30 seconds for as long
as a `Server` keeps draining — and re-reads each player's current server on
every call rather than trusting a cached list, which is what makes a dropped
message or an operator restart mid-drain safe: a repeat that finds nobody
still on `fromServer` moves nobody. `Router.choose` is the same code path a
join uses, so a drain target is chosen by the identical rule a join would
have used, not a separate policy that can silently disagree.

**`ServerDirectory` and `ProxyRegistry` are the seam a proxy-drain signal
would arrive through.** Both already exist as the mechanism that keeps
Velocity's own server registry in step with the operator's, driven entirely
from the gRPC callback thread `SessionLoop` runs on. A new `OperatorToProxy`
case telling a proxy to lower its own readiness would be one more branch in
`ProxyRole.apply`, in the same shape `DRAIN_PLAYERS` already is — not a new
subsystem.

**`ReadyGate` already has the primitive milestone 4 needs on the proxy
side**, `close()`, and it is already correct: idempotent, safe to call on a
gate never opened, and synchronized against the accept loop so a close racing
an open cannot leak a bound socket. What is missing is only ever calling it
from somewhere other than shutdown — see "The one contract change" above.

## The environment

```bash
nix develop        # Go, controller-gen, protoc, envtest assets, kubectl, kind, k3d, JDK 21, Gradle
make test           # Go only; must be green before anything is touched
make agent          # both agent Gradle subprojects and their JUnit suites
make agent-test     # both agents against the stub operator, in the real images
make image-test     # both images offline, under the pod spec's constraints
make image-repro    # both images, rebuilt and compared byte for byte
```

**A container runtime is required** for every target above except `make
test`, and the image targets only work on `x86_64-linux`. `docs/known-issues.md`
records the Podman-under-`kind` story in full; nothing about it changed in
3c.

**`agent/common`, `agent/paper` and `agent/velocity` are versioned
together, not apart** — the decision recorded in `docs/handover-milestone-3.md`
under "Questions worth settling before code" and made permanent by 3c's
Gradle split. A change to `agent/common`'s session loop is a change both
agents ship on their next build, whether or not the other agent's own code
moved.

## Questions worth settling before code

- **What message carries a proxy's own drain signal, and who decides to send
  it?** `DrainPlayers` already exists for backends and is the wrong shape
  reused: a backend drains because a `Server` is being deleted; a proxy
  drains because its own pod is being removed by a scale-down or a rolling
  update, which is a `ProxyGroupReconciler` decision, not a `Server`
  controller one. Whether this is a new message, a new field on an existing
  one, or a repurposing of a field `ProxyMessage.Hello` already reserves is
  open.
- **Does a draining proxy still receive `FullSync` and `RegisterServer`?**
  Its own players still need to be moved off it — the same `Router.choose`
  and `Drain` machinery a backend drain already uses — which means the
  proxy's own server list has to stay current for exactly as long as it is
  still routing anyone. A drain that also stops the server-list stream would
  strand whoever it has not yet moved.
- **What does `ProxyGroup.status` show while a proxy is draining, given the
  ready-port bind failure is already silent on the CR today?** Milestone 4
  is a natural place to close both gaps in the same change rather than adding
  a second kind of silence next to the first.
