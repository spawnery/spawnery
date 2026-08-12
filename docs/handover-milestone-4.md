# Handover to milestone 4

Status: end of milestone 3c, the Velocity agent (2026-08-11). Updated
2026-08-12 with what the evidence run below actually measured against a real
cluster; nothing before that section changed.

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
automated and by hand — are implemented; whether they hold against a real
cluster is what the evidence run below measures.

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
is NOT proven.** It needs a licensed Minecraft client and a person to drive
it, neither available in this session. `docs/runbook-milestone-3-evidence.md`
§10, "The manual proof, for a later session", is written for whoever runs it
next, from a different machine, starting from an empty cluster.

**Criterion 9 — deleting a `Server` moves a connected player rather than
disconnecting them — is NOT proven, and the reason is the most important
finding of this run.** Deleting a `Server` with a `spawnery-join --hold`
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

The manual session that still owes criterion 8 and a manual re-run of
criterion 9 (`docs/runbook-milestone-3-evidence.md` §10) should record its
own two log lines per side, the player's real UUID, and the drain's move
line under this same heading when it runs.

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
