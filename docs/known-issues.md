# Known issues and carry-overs for later milestones

This file carries only problems that still exist. An entry that gets fixed is
deleted, and the account of what it was and how it was found lives in the
commit that removed it — `git log -p docs/known-issues.md` is where to look
for one. A closed entry left standing with a note saying it is closed costs a
reader the same attention as a live one, which is the whole reason for the
rule.

Two things that are not open problems live elsewhere.
[`upgrading.md`](upgrading.md) carries what strands an object or rolls a fleet
when an installation crosses a release — real work for whoever is upgrading
one, and nothing at all for anyone else.
[`ca-rotation.md`](ca-rotation.md) carries the CA rotation procedure, which is
a thing a human drives rather than a thing that is wrong. The design decisions live in
`superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`, in
`superpowers/specs/2026-08-08-agent-channel-design.md`, in
`superpowers/specs/2026-08-09-paper-agent-design.md`, in
`superpowers/specs/2026-08-10-proxy-channel-design.md`, in
`superpowers/specs/2026-08-10-velocity-image-design.md` and in
`superpowers/specs/2026-08-11-velocity-agent-design.md`.

## From milestone 2b (the base image)

**The Darwin machine cannot build the image.** A Linux image needs a Linux
builder, so `nix build .#paper-image`, `make image-test` and the local-cluster
flow only work on `x86_64-linux`. `make test` still runs everywhere, including
the entrypoint and SLP tests. This is the mirror image of the envtest gate that
milestone 2a closed, and it cannot be closed with a checked-in hash.

**Without a memory limit the JVM sizes itself against the whole node.** The
entrypoint passes `-XX:MaxRAMPercentage=75`, which is a share of the container
limit — and of the node when there is no limit. `AlwaysPreTouch` then claims
that share immediately. Neither `ServerGroup` nor `Network` is required to set
`resources`, and no CEL rule demands it; the sample manifest sets 2Gi and
nothing makes anyone else do so.

**Following Paper upstream is manual.** A new build means new hashes in
`nix/paper.nix`, by hand, including the Mojang hash out of the new jar's
`META-INF/download-context`. The automated image pipeline is project 3 in the
main design.

**`cache/mojang_26.2.jar` ships unused**, 61 MB of the image. Paperclip touches
the cache directory before deciding whether it needs to patch, and fails on a
read-only path if it is absent. Removing it would require a writable cache
directory in every pod, which is the worse trade.

**Paper makes two outbound calls on every start that have nothing to do with
artifact provisioning.** `api.minecraftservices.com/publickeys` is the
Yggdrasil key fetch that follows from `online-mode=true`; `fill.papermc.io` is
Paper's own update checker. Both are measured to fail harmlessly with no
network reachable — the server still reaches `Done` and answers a ping, which
is what `make image-test` relies on. Both consequences this entry once named
have since happened: milestone 3 flipped `online-mode` to false, which retired
the Yggdrasil call, and milestone 6b built the egress policy. What remains is
the bare fact — a Paper server still calls `fill.papermc.io` on every start,
and anything that tightens egress further has to decide about it.

## From milestone 2c (the Paper agent)

**A read-only mount at `/data/plugins` breaks the start.** `image/entrypoint.sh`
copies the agent jar out of the image into `plugins/` under `/data`, because
Paper writes its plugins' data folders inside the plugins directory and a
read-only one takes Paper's own bundled plugins down with it. Mounts below
`/data` are the documented way to add files and `checkMountCollision` allows
them; a ConfigMap or Secret mount is always read-only (`internal/podspec`
sets `ReadOnly: true` on every user mount unconditionally). So a mount at
`/data/plugins` makes the `cp` fail under `set -eu` with a bare `cp:` message
that says nothing about why. Unlike `/data/config`, this one cannot be fixed the way that one was:
`/data/config` was avoided by relocating the operator's *own* mount target to
`/etc/spawnery`, but `/data/plugins` is Paper's own plugins directory and the
agent jar has to land inside it regardless of what a user mounts there.
`checkMountCollision` does not single it out, so the mount is permitted and
the start breaks with a bare `cp:` message that says nothing about why.

**The relocation is not proven on the give-up path.** The cast to
`ClientCallStreamObserver` that the cancel needs sits inside a `runCatching`,
which catches `Throwable` — so a `NoClassDefFoundError` from a shading
regression would be swallowed and phase 3 of `make agent-test` would still pass.
Phase 3 being green is evidence that the bound holds, not that the cast resolves
under the shaded names.

**The level-2 harness has rough edges milestone 3 inherits.** `hack/agent-test.sh`
and `cmd/spawnery-stubop` are exactly what a Velocity agent will be tested with,
so what they do not check is worth writing down: stream indices `0` and `1` are
hard-coded in the overlap verdict; `seq` is record order and not arrival order,
which the verdict's wording overstates; two wait loops after `await_event` do not
check that the container is still alive; the phases are near-verbatim copies
of one another rather than one parameterised function, so what each varies has
to be found by eye -- and there are six of them now, not the three this entry
counted, so that cost has doubled rather than been paid off; and the stub's own
Go tests cover neither the never-closes property nor the uniqueness of `seq`.

**The local kind flow needs a `Service` nothing creates.** A pod dials
`spawnery-operator.<ns>.svc:9443`. When the operator runs outside the cluster —
which is the only way the README's local flow runs it — no selector can find it,
so the flow has to create a selector-less `Service` and a hand-written
`Endpoints` by hand, or the `Server` never leaves `Starting`. Under rootless
Podman that is harder than it sounds: the `kind` network's gateway is inside the
rootless network namespace and refuses the connection, and the address that does
reach the host (`host.containers.internal`, a pasta link-local `169.254.x.x`) is
rejected by the API server in both `Endpoints` and `EndpointSlice`. The README
documents the relay container that works. Milestone 6 wires this flow into CI and
will meet the same wall, and the durable answer there is to run the operator
inside the cluster from its own image, where the Service is a Service.

*Closed by milestone 6a for the half that matters, and still true for the
other.* Anything that runs the operator **in** the cluster now gets a Service
carrying an ordinary selector over
`app.kubernetes.io/name=spawnery,app.kubernetes.io/component=operator` and needs
no hand-written `Endpoints` and no relay; `hack/e2e.sh` installs exactly that and
`make e2e` runs on it. *The source moved under milestone 6d*: what was
`config/deploy/service.yaml` is now `charts/spawnery/templates/service.yaml`,
rendered by `helm install` rather than applied directly — the selector itself
is unchanged. The README's local `go run` flow is unchanged and still
needs the whole workaround — the selector-less `Service`, the hand-written
`Endpoints`, and under rootless Podman the relay container — because the process
is still outside the cluster there and no selector can reach it. Read this entry
as scoped to that flow from 6a onward.

## From milestones 3a and 3b (the operator's proxy side, and the Velocity image)

All five of milestone 3's original preconditions were discharged — three by 3a
on 2026-08-10, two by 3b on 2026-08-11 — and were removed on 2026-08-22 once
their stated reason for being kept had expired: they were held so the next
sub-project could inherit the reasoning, and that sub-project, 3c, landed and
shares the code rather than reimplementing it. `git log` has them.

What stands below is what 3a and 3b found while closing them, which is a
different thing and outlived its milestone.

What follows is what 3b discovered while closing its own two preconditions,
and what 3c inherits as a result.

**`server.properties` is the one overlay nothing checks.** Both other
flavours refuse an overlay that does not parse and, since 2026-08-24, one that
names a key the receiving program does not declare, measured against that
program's own defaults (`internal/render/declared.go`). This one is left out
by construction rather than by omission. `parseProperties` accepts any
`key=value` line, so a mistyped key adds an unused one — and there is no
fixture to check against, because
Paper's `server.properties` is Minecraft's own and this repository has never
measured it the way it measures `paper-global.yml` and `velocity.toml`. The
four keys the operator relies on there are in the critical layer and no
overlay can move them (`internal/render/paper.go`), so what a typo can reach
is the author's own settings and nothing this operator depends on. Closing it
would mean a third default file and a third regeneration step.

**A ConfigMap already sitting at the rendered name is adopted without an
ownership check.** Neither `reconcileConfigMap`
(`internal/controller/servergroup_controller.go` and
`internal/controller/proxygroup_controller.go`) checks that a ConfigMap at
`<group>-<role>-config` belongs to this group before mutating it.
`controllerutil.CreateOrUpdate`'s mutate closure sets the
label and `config.yaml` and calls `SetControllerReference` last, and
`SetControllerReference` only refuses when the object already has a
*different* controller owner — an object with no owner at all is silently
given one. A ConfigMap that happens to carry `podspec.LabelManagedBy` (so
the cache sees it) but was created by something other than this group's
reconciler is therefore still adoptable. The `<group>-<role>-config` rename
traded a plausible collision (a user's own ConfigMap at the bare group name)
for an implausible one (the exact rendered name, pre-labelled); it did not
remove the shape.

## From milestone 3c (the Velocity agent)

**Paper 26.2 accepts the forwarding secret from the environment**
(`PAPER_VELOCITY_SECRET`), so the plaintext need not be written into
`/data/config/paper-global.yml` in the writable layer at all. Not done; a
smaller attack surface for whoever next opens the Paper renderer.

**A proxy that cannot bind its ready port is silent on the CR.** It stays
`Pending` with the reason only in the container log
(`ReadyGate.open`'s own `log(...)` call). This is the same shape as the
`playerLimit` defect milestone 3b found and fixed, in a place where the
operator has nothing to write to.

**Smaller ones**, each worth a sentence. Phase 5 of `hack/agent-test.sh`
reuses phase 2's window constants, declared 400 lines earlier and both derived
from a hard-coded renewal interval. `streams_opened` counts what the operator
saw, so a proxy leaking a gRPC channel per reconnect is measured nowhere — the
standing blind spot inherited from milestone 2c. And `cmd/spawnery-join` asks
a server for its protocol version by announcing an unsupported one
(`announceUnsupported = -1`), trusting that the proxy's newest supported
version and the backend's actual version agree — true of every pinned pair
this repository ships and not guaranteed generally. `internal/mcjoin`'s
package comment names the failure mode, a loud "Outdated client!" naming the
version to fix it to, so it fails loud rather than silent; the runbook that
depends on the tool inherits the same assumption.

**A backend that goes silent without closing its socket still disconnects its
players, and no plugin can stop it.** `Rescue` catches a player whose server
drops them and redirects them onto `fallbackGroups`, which is what Velocity's
own failover cannot do here — it walks `try`, and internal/render renders
`try = []` because the server list is dynamic. It does not cover every case.
Disassembling velocity 3.5.1 build 615, `handleConnectionException` returns
*before* firing `KickedFromServerEvent` when its `safe` argument is false, and
`BackendPlaySessionHandler.exception(cause)` passes
`safe = !(cause instanceof ReadTimeoutException)`. So a hard-powered-off node
or a partitioned network surfaces as a read timeout and the player is
disconnected before any plugin is consulted. Closing it would mean the
operator noticing the dead server and sending `DrainPlayers` inside Velocity's
read timeout, which is a different mechanism than this one.

## From the milestone 3c evidence run (2026-08-12)

`docs/runbook-milestone-3-evidence.md` was finally run against a real `kind`
cluster. Criterion 7 (a player can join, automated) is now proven — see
`docs/handover-milestone-4.md`. Criterion 9 (deleting a `Server` moves its
player rather than disconnecting them) was not, and the reason why is the most
important finding of this run.

**A player connected at the proxy but not yet counted by the backend sits
outside the drain's protection, and no milestone owns the question.**
`Occupied()` (`internal/phase/phase.go`) is `in.PlayersStale ||
in.PlayersOnline > 0` — what the *backend* has reported — and the proxy's own
count is not consulted at all. Checked again 2026-08-22: nothing in 4a through
4d changed it, so the window is exactly as open as when it was found and it no
longer has a milestone assigned. Whoever next touches drain inherits it rather
than finding it owned.

In production the window is real but small: a real client completes the
configuration phase within the same round trip. It was found because
`spawnery-join --hold` freezes a connection there deliberately, which made a
`kubectl delete` on a held player disconnect them rather than move them —
`internal/mcjoin`'s `holdOpen` now carries what a held connection is and is
not, and what closing that gap would take.

None of this branch's reviews caught it, and why is the transferable part. The
whole-branch review correctly predicted that a held connection would be
*counted* on the proxy side, in `status.connectedPlayers`. Nobody asked the
complementary question: which side does the drain's own exit condition read?
The two counts live in different structs, are populated by different agents,
and were never checked against each other until an actual delete on an actual
held connection forced it.

## From milestone 4a

**`status.freeSlots` and the scaler's own figure are two numbers.**
`AggregateGroup` computes free slots over `Ready` servers of the current
generation; `provisionalCapacity` in `internal/controller/scaling.go` computes
a second figure that also credits servers whose capacity is ordered and has
not arrived. Anyone reading the code for the first time will want to unify
them. They must not be: the first is what `status.freeSlots` documents and
what 4b's rolling update needs; the second is what stops the scaler ordering
the same replacement on every five-second pass. Both files say so; this entry
is the third place, so a search finds it.

**`provisionalCapacity` credits a server the informer has not caught up with,
for one pass.** The original defect here — a server whose pod had vanished
credited a full `maxPlayers` — was closed by testing `ServerView.SessionsGone`
before the `Slots == 0` credit, and both the reason and the wrong fix are
pinned where they belong: `internal/controller/scaling.go` says in its own
comment why testing `Stale` there would be a regression, and
`TestProvisionalCapacityStillCreditsAStartingServer` fails if anybody tries.

What is recorded here because it is recorded nowhere else: `SessionsGone` is
`srv.Status.PodName != "" && (!podFound || podTerminal(pod))`, and that reads
true for one resync for a server that has genuinely just started, if the
informer's cache has not yet shown a pod the API server already created. Such
a server is credited zero instead of its full `maxPlayers`, so the sum reads
low and `wanted` reads high — over-creation for a pass or two. That is the
safer of the two directions: a group with a server too many costs money, a
group with a server too few costs joins. `isOccupied` has carried the same lag
for the same reason since before 4b; what 4b changed is that a scaling
decision now reads it too.

## From milestone 4b

**A group at its ceiling with nothing to shed cannot start a changeover, and
one that is holding a stuck retiree does not say why.** The cold start (design
§3.3) is a create like any other, so a group whose `maxReplicas` equals its
current size cannot simply build the first server of the new generation. It
first tries to make its own room: a refused cold start that was the only thing
the pass wanted falls through to the demand rule, which sheds an idle stale
server if there is one, and the next pass then starts the changeover. Only when
there is genuinely nothing to shed does the group stall with its old generation
serving. That stall is correct — a lowered ceiling is an instruction, not a
suggestion — and it is not silent: `DecideSize` sets `Limited` and, in this
specific case, `ColdStartBlocked`, so the `ScalingLimited` condition carries a
message naming the cold start specifically rather than an ordinary capacity
shortfall. Raising `maxReplicas` by one is the way out.

There is a second way a changeover stops, and this one *is* silent. A server
the group has just patched `spec.retire: true` onto, which loses readiness
before the `Server` controller next reconciles, goes to `Starting` rather than
`Retiring` (`phase.Decide` tests the readiness loss first, deliberately) while
`spec.retire` stays true. If it recovers to `Ready` it retires normally. If it
never does, `StartupDeadlineReached` fails it — and a `Failed` server carrying
`spec.retire` holds the whole `maxUnavailable` budget for its retention window,
an hour by default, with no condition, no event and nothing telling an operator
why no further server is retiring. This matches design §3.8 as written ("a
server counts against the budget while its `spec.retire` is true"); the spec
did not consider a retiree that never retires, and the behaviour errs
conservative — fewer retirements, never a disconnection — so it is carried
rather than changed. `kubectl get servers -o custom-columns` showing
`spec.retire` alongside the phase is what answers it today. Both of these
present the same way from outside — the changeover stopped — and the difference
is that the first one names itself on the group's conditions and the second
does not.

## From milestone 4c-1 (the proxy readiness contract)

**`nix build` filters the source tree through the git index, so an untracked
file does not exist for a sandboxed build.** This is not a 4c-1 discovery —
it cost time in milestone 2c as well and was simply never written down. It
presents as a compile failure naming a symbol that is plainly there in the file
in front of you: this milestone's was 35 copies of
`package cloud.spawnery.agent.pb.SetReady does not exist` from `make agent`,
immediately after `make proto` had generated the Java stubs, which looks
exactly like the `protoc`/runtime version drift the milestone 2c entry above
warns about and is not. The agents derivation builds from `src = ../agent`
(`nix/agents.nix:33`), the four Go binaries from `src = ./.` in `flake.nix`;
either way the source is the git tree. `git add` before the build, not just
before the commit — staging is enough, nothing has to be committed.

**Two assertions in `hack/agent-test.sh` are argued rather than
demonstrated.** The control probe on 25565 that follows the closed-gate
assertion needs the container to die mid-phase to be shown failing, and the
post-loop arm of phase 4's withdrawal guard needs `port_open` to answer while
`set_ready_sent` is already non-zero — a sub-second window on a correct agent.
Neither has been made to fail on purpose, so neither is known to be able to.

## From milestone 4c-2 (proxy rolling updates)

**Which edits roll a proxy group is decided by what reaches the pod, and that
is not the shape of the CRD.** The digest covers `podspec.BuildProxyPod`'s
output, so an edit rolls the group exactly when it changes that pod. Read off
`internal/podspec/proxy.go`: `image`, `resources`, `scheduling`,
`config.playerLimit`, `routing.fallbackGroups`, `configOverlay` (the
ConfigMap's *name*, in a volume) and `drain.timeoutSeconds` all roll it, as do
the `Network` fields the proxy pod inherits — `defaults.resources`,
`defaults.scheduling`, `defaults.imagePullSecrets` and
`forwardingSecretRef.name`. `replicas` does not, which is the point of the
design's §3.1. Neither do `config.motd` and `config.onlineMode`, nor the
*contents* of a `configOverlay` ConfigMap or of the forwarding secret, because
the pod names all of those rather than carrying them — those two fields now
say so themselves, and `kubectl explain` says it with them.

The one worth knowing before an incident: **`spec.drain.timeoutSeconds` rolls
the group**, because it reaches the pod as `terminationGracePeriodSeconds`.
Tuning a drain timeout is something an operator does in the middle of an
incident, and the edit adds a surge pod and a full replacement cycle on top of
whatever prompted it. Raising it while a drain is already in flight otherwise
behaves — the marked pod keeps its mark, being now stale as well as draining,
and the deadline is read from the current spec on every pass.
`docs/runbook-milestone-4c1-evidence.md` §9 recommends exactly this edit for a
drain you want to give more room to; since 4c-2 it is not free.

## From milestone 4c-3 (node drain)

**An operator running cluster-autoscaler must pass `-drain-taint
ToBeDeletedByClusterAutoscaler`, or a scale-in is invisible to this operator
until something else cordons the node.** *Measured on `paulwtf` 2026-08-25: it
passes none.* Its `Deployment`'s args are `--leader-elect`,
`--startup-deadline`, `--metrics-bind-address` and
`--health-probe-bind-address`, and nothing else — so the taint branch cannot
fire on that cluster at all. Harmless there, and worth writing down rather than
fixing: three fixed bare-metal nodes and no autoscaler, so the branch has
nothing to react to. It stops being harmless the day one appears, and this is
the sentence that will be looked for then. `IsDeparting` (`internal/controller/nodes.go`)
has two ways in: `spec.unschedulable`, which is hardwired, and a taint whose
key appears in the operator's `-drain-taint` list — repeatable, and empty by
default. An earlier draft of the design that produced this milestone claimed
cluster-autoscaler cordons a node in addition to tainting it, so that the
empty default would still see a scale-in a moment later; that claim did not
survive the milestone's own review and was corrected in place
(`bc4122a`, "cluster-autoscaler does not cordon, so say what stays true").
What is actually true: cluster-autoscaler taints
`ToBeDeletedByClusterAutoscaler:NoSchedule` and deletes the node without
touching `spec.unschedulable` unless `--cordon-node-before-terminating` is
turned on, and that flag defaults to off. Karpenter was not re-checked and is
not claimed here either way. The default stays empty regardless — a default
that reacted to another project's taint key would couple this operator to a
vocabulary that project is free to rename, which is exactly the coupling a
configurable list exists to avoid — so this is a configuration step every
cluster-autoscaler user has to take themselves, and nothing in the operator
will tell them they missed it: an unset flag and a genuinely quiet node look
identical from here.

**A group in create-backoff, or one with a broken Network, condemns without
replacing.** `size()` (`internal/controller/servergroup_controller.go`) gates
only the create loop behind `backoff.MayCreate` — `condemn()`, which runs
`decision.Condemn` through `deleteServer` with event reason `NodeDraining`,
is not gated, and runs on every pass regardless of the group's backoff state,
the same as the ordinary delete and retire loops beside it. So a group whose creates are
failing for a reason that has nothing to do with node drain — a broken image,
a quota limit, anything `CountFailures` is counting — still condemns every
server on a departing node while it is in backoff, and does not replace them
until the backoff window next permits a create. This was a deliberate ruling
during the milestone's implementation, not an oversight: the alternative is
holding players on a node that is going away, and they get evicted from it
regardless of what the group's backoff thinks — moving them onto a fallback
group beats being kicked off the node with nowhere chosen for them at all.
The group runs below capacity for the length of whatever backoff window it
was already in; nothing about node drain makes that window longer or
shorter, and once it lifts the group rebuilds to its normal size the way it
would after any other backoff.

The same holds, for the same reason and by the same ruling, when a group's
`Network` has been deleted or has lost the one-per-namespace contest.
`Reconcile` calls `size()` once, on every pass, and passes it a `mayResize`
flag that is false whenever the `Network` is unusable; the branch is inside
`size()` itself, not at the call site. When `mayResize` is false, `size()`
skips straight to condemning and returns — the sizing arithmetic and the
creates, deletes and retirements it would otherwise produce wait for a usable
`Network`, and the condemnation does not. A group in that state condemns the
servers on a departing node and cannot build replacements at all until the
`Network` is fixed — a longer wait than a backoff window, and an unbounded
one. It is still the better half of the trade: those players are evicted off
that node whatever the group does, and the group was already unable to build
anything before the node started leaving. What the earlier shape did instead
was worse in a way that is easy to miss — the group published
`NodeDraining: True` naming the node, condemned nothing, and left `kubectl
drain` hanging on an occupied pod indefinitely, which is the exact failure
this milestone exists to end.

**A `ProxyGroup` whose `Network` is broken cannot be drained at all.** Its
budget still protects players, but nothing moves them off. `Reconcile`
(`internal/controller/proxygroup_controller.go`) gives up before
`reconcileReplicas` on three paths — a missing `Network`, one that is not
`Accepted`, and an `expose.type` this operator has no branch for, the last of
which milestone 6c made unreachable while the CRD's enum and
`exposeImplemented` agree, and kept only as the fail-safe for an enum value
added without a branch to serve it — and each of them
calls `protectPlayersOnly` instead, which re-derives the
`spawnery.cloud/occupied` labels, re-sizes the budget from them, and
republishes the `NodeDraining` condition. What `protectPlayersOnly` does not
do is anything `reconcileReplicas` owns: `markDraining`, the readiness
withdrawal (`Proxies.SetReady`) that stops new connections from arriving, and
the drain-deadline deletion that finally removes an empty or timed-out pod
all live inside `reconcileReplicas` and run nowhere else. So such a group
publishes `NodeDraining: True` naming the node, sizes `minAvailable` to cover
every occupied proxy — which now makes the eviction API refuse every attempt
to take the occupied proxy on the departing node — and never marks that
proxy draining, never starts its removal deadline, and never replaces it.
`kubectl drain` cannot complete against a `ProxyGroup` in this state until
its `Network` is fixed; the budget refuses the disruption, it does not act
on it.

That is a change from before this milestone, and it is worth being precise
about what moved and what did not. In the case that matters most, the same
group already hung: a proxy that already had a player was already counted
(its frozen `minAvailable` was at least 1 against a pod the kubelet still
called Ready), so `disruptionsAllowed` was already 0 and the eviction API
already refused it, node drain or not. The one case that moved is the proxy
that was empty at the group's last good pass and picked up a player
afterwards: before, nothing on these paths re-derived the label or the
budget, so that pod sat at `minAvailable: 0` with no label and the eviction
API could take it — a disconnect. Now `protectPlayersOnly` counts it and the
eviction API refuses it too — a hang instead of a disconnect. That trade is
deliberate, for the same reason the create-backoff case above accepts a
longer wait: a hung eviction recovers once the `Network` is fixed, and a
disconnected player does not.

The cadence cost the previous version of this entry described still applies
on top: those three early-return paths requeue at `networkRetryInterval`
(30 s) rather than the ordinary five-second `resyncInterval`, so even the
label and budget `protectPlayersOnly` does maintain lag behind a healthy
group's by up to 6×. Nothing watches the agent registry — occupancy is
in-process state, not an API object — so no event corresponds to a player
joining; the group's other watches fire on `Pod`, `Node`, `Service` and
`ConfigMap` changes, and a reconcile any of those happens to trigger brings
the budget forward as a side effect rather than because anybody asked it to.

**The 15-second occupancy grace is not derived from a measured reconnect
distribution, and a proxy that is genuinely reconnecting can lose its
protection before it answers.** `proxyOccupiedForBudget`
(`internal/controller/proxygroup_controller.go`) argues the bound in full
where it lives: it sits between reporting a live, player-carrying fleet as
empty the instant the operator restarts, and letting a proxy whose agent never
arrives wedge its group's evictions forever. What the comment cannot say is
that 15 seconds was chosen to sit between those two rather than derived from
anything, and `SessionLoop`'s backoff cap is 30 seconds — so a proxy still
dialling back in can pass the fifteenth second without having had a chance to
report, and after that nothing tells its group's budget apart from the pod
that never will.

One observation stands unexplained beside it: a reconnect measured at 85 s,
more than the 30 s cap plus a resync accounts for. The likeliest reading is
`SessionLoop`'s own class comment — the channel has no keepalive, no idle
timeout and no call deadlines, so a partitioned agent learns its stream is
dead only when a send fails, which for a Paper agent is its next player-count
report, and its backoff clock starts well after the operator's did. That is
reasoning, not measurement. Whoever narrows the bound should isolate the two
distributions first: the grace is sized against the second one.

**The taint list is trusted, not validated.** `-drain-taint` accepts any
string, and `IsDeparting` matches it only against a taint whose effect is
`NoSchedule` or `NoExecute` — deliberately, per §3.1's own reasoning: a
`PreferNoSchedule` taint does not stop the scheduler putting a replacement pod
straight back on the same node, so matching on it would condemn a pod, rebuild
it in place, and condemn it again next pass. That correctness comes at a cost
this operator never reports: a key configured with an effect it ignores — a
real taint on a real node, `PreferNoSchedule` or any future effect Kubernetes
adds — simply never matches, silently, with nothing on any group's conditions
or events distinguishing "this taint does not apply" from "there is no such
taint at all". *The key is checked as of 2026-08-24, for the half of that
which is checkable.* `-drain-taint` now refuses a value that is not a bare
qualified name, using `validation.IsQualifiedName` — the same check Kubernetes
validates a taint key with, so it refuses exactly what the API server would
and nothing more.

The mistake this catches is the one to expect. Taints are written
`key=value:Effect` nearly everywhere a person meets them — `kubectl taint`,
node manifests, every tutorial — so passing the whole taint is the likely slip,
and it was the one this operator survived worst: such a key matches no taint
that exists, so the flag was accepted, nothing ever drained, and nothing said
why. It is now a refusal at startup whose message shows what the flag takes.

A well-formed key that is simply absent from the cluster still cannot be told
from a typo. Nothing can tell those apart, and this does not pretend to. An
operator relying on a taint to drain a node should confirm independently, with
`kubectl describe node`, that the taint is present with an effect this
operator honours; there is no warning if it is not.

## From milestone 5a (persistent groups exist)

5a gives a `Persistent` group ordinals, a `PersistentVolumeClaim` per ordinal
and both directions of `spec.replicas`. What follows is what that leaves for
an operator to know, and for 5b and 5c to find in place — the fuller record is
`docs/handover-milestone-5.md`.

**Claims accumulate, and this operator can never remove one.** Deleting a
`Server` — by scaling down, by hand, or through the failed-retention path
below — never deletes the `PersistentVolumeClaim` it mounted:
`podspec.BuildDataClaim` stamps no owner reference, and nothing in this
operator calls `Delete` on a claim anywhere. That is not merely the observed
behaviour, it is enforced structurally: the ClusterRole
(`config/rbac/role.yaml`) grants `persistentvolumeclaims:
create;get;list;watch` and nothing else, and `internal/rbacaudit/required.go`
documents exactly those four verbs with a comment explaining why `delete` and
`update` are absent on purpose. `internal/rbacaudit`'s tests compare the
generated role against that table in both directions — extra grants as well as
missing ones — so a future `delete` marker added anywhere in the codebase
turns the audit red before it can ship. A lowered `spec.replicas`, a group
deleted outright, or an ordinal simply never brought back all leave their
claims standing, by design: §3.3 of the persistent-groups design settles that
a mistake here should cost a stray object, never a world.

To find what a namespace has accumulated:

```bash
kubectl get pvc -l spawnery.cloud/managed-by=spawnery-operator -n <namespace>
```

Every claim this operator ever created carries that label
(`podspec.LabelManagedBy`), and it is the one — the only one — that restricts
the manager's own cache over claims (`cmd/spawnery-operator/main.go`).
`podspec.BuildDataClaim` puts three more on every claim it renders,
`spawnery.cloud/network`, `spawnery.cloud/group` and `spawnery.cloud/server`;
none of those narrows anything the operator does, and they are there for
whoever is reading claims by hand. To tell a claim still in
service from an orphan, compare each claim's `spawnery.cloud/server` label
against the `Server` objects that currently exist for that group: a claim
named `<group>-<ordinal>-data` whose `spawnery.cloud/server` names a `Server`
that is gone (scaled away, or the group itself deleted) is an orphan.
**Deleting a claim deletes a world** — there is no undelete, and no
confirmation this operator can offer, because it never performs the deletion
itself. Removing one is a deliberate human act with `kubectl delete pvc`,
outside this operator entirely, and belongs on the runbook that grows up
around this operator's use rather than in its own code.

**A claim that never binds ends in a stall, and the stall is deliberate.**
`docs/superpowers/specs/2026-08-15-persistent-groups-design.md` §3.5 is on its
third version for exactly this mechanism — the first two were wrong, and its
own top-of-section note says so — so what follows is checked against the code
as it stands rather than repeated from memory:

- A pod that never becomes playable fails its server's startup deadline the
  same way an ephemeral one would; `phase.Decide`'s `Failed` case is
  type-blind.
- Nothing on the *group's* side ever removes a persistent server for having
  failed. `pruneFailed` only runs `if group.IsEphemeral()`
  (`internal/controller/servergroup_controller.go`), and
  `DecidePersistentSize` holds an ordinal for as long as any server carries
  it, in any phase — so a `Failed` corpse keeps its ordinal.
- What does eventually move it is `phase.Decide`'s own retention clock: once
  `now - status.failedAt >= spec.failedRetentionSeconds` (3600 seconds at the
  CRD default), the `Failed` case returns `Terminating`, and the **Server**
  controller deletes the object once its pod is gone
  (`internal/controller/server_controller.go`, the `decision.Next ==
  phase.Terminating && !podFound` branch). The ordinal is free the moment that
  delete lands.
- The group's very next pass sees the ordinal missing and creates it again,
  under the identical deterministic name — `podspec.DataClaimName` derives the
  claim name from the server name, and the server name is `<group>-<ordinal>`
  — so the new server's claim-create call gets `AlreadyExists` and mounts the
  same, still-broken volume. `DecideBackoff`'s create gate
  (`backoff.MayCreate`, gating `CreateOrdinals` the same way it gates the
  ephemeral count in `internal/controller/servergroup_controller.go`'s
  `size()`) is what turns this from an unbounded loop into a bounded one: six
  counted failures and the group gives up.
- So the period of the retry loop is `spec.failedRetentionSeconds`, not the
  backoff window — the backoff window is at most 160 seconds (10s doubling to
  160s across five gaps before the sixth failure), which at the CRD's
  3600-second default never actually delays an attempt. What the backoff
  contributes here is only the give-up.
- After the give-up the group waits indefinitely — but not, at first, with no
  `Server` object for that ordinal. At the moment the count reaches the
  threshold the sixth corpse is still standing and still holding its ordinal,
  and it stays for one more `failedRetentionSeconds` before the Server
  controller takes it away. The empty-ordinal state is where this settles,
  roughly an hour later at the CRD default, not where it begins. The claim and
  the world on it are untouched throughout — nothing in this operator can
  update or delete a claim, per the RBAC point above — and a spec change (any
  edit that moves `metadata.generation`) resets the counter and brings the
  ordinal back.

Stalling is the right outcome rather than a tolerated one. A persistent world
lives on one claim and nothing else can serve it, so a rebuild only ever meets
the same broken volume — sequentially, never concurrently, since the corpse's
pod is deleted before its replacement is created. After six attempts roughly
an hour apart, what is broken is the storage and not the server, and only a
human can fix a storage class, a quota, or a stuck `WaitForFirstConsumer`
binding.

**`Degraded` is late, and that is worth knowing before it fires.** At the
default `failedRetentionSeconds` of 3600 the group is visibly backing off
(`BackingOff: True`) for only ten to a hundred and sixty seconds of each
roughly hourly cycle. For the rest of each cycle it publishes `BackingOff:
False` with the reason "no server has failed to start recently" — true in the
narrow sense the field means, and easy to read as "nothing is wrong" while a
`Failed` corpse is sitting right there holding the ordinal. **Six counted
failures span five gaps, not six**, and each gap is longer than the
retention window alone: the corpse's `failedRetentionSeconds` (3600s) has to
elapse before the `Server` object is removed and a replacement created, and
that replacement then runs its own `--startup-deadline` (300s by default)
before it can fail in turn and be counted as the next failure. Each gap is
therefore close to `3600 + 300` = 3900 seconds, about sixty-five minutes, not
an even hour. `Degraded` therefore does not turn true until roughly **five
and a half hours** after the first failure — five gaps of about sixty-five
minutes each — not six. **That figure holds at `replicas: 1`, and is a floor
with no ceiling above it; the next entry is why.** An operator watching for a
stall in that window should not wait for `Degraded` or for `BackingOff: True`: both
`status.consecutiveFailures` and `status.lastFailureAt` are written from the
very first counted failure, for a group of either type — that counting is
unconditional in `Reconcile`, not behind `if group.IsEphemeral()` the way the
two conditions used to be before this milestone's own review lifted them out.
`kubectl get servergroup <name> -o jsonpath='{.status.consecutiveFailures}
{.status.lastFailureAt}'` says something true from the first failure onward,
hours before either condition would — at `replicas: 1`. At two or more the two
fields part company, and the next entry says which of them still tells the
truth.

**A healthy sibling resets a broken ordinal's failure streak, so with two or
more ordinals `Degraded` may never arrive.** `CountFailures`
(`internal/controller/backoff.go`) takes the newest `ReadySince` across the
group's servers of the current generation — the set `ofGeneration` narrows the
views to before handing them over — and, when it is newer than the last counted
failure, restarts the count at zero. For an ephemeral group that is the right rule, and its own
doc comment says why: those servers are interchangeable, so one of them
becoming ready is evidence about the group. A persistent group's are not
interchangeable — the world is on one claim and no other server can serve it.

So with `replicas: 2`: `survival-0`'s claim never binds and its server fails
roughly hourly, while `survival-1` runs normally and loses its readiness probe
once — a container restart, a slow tick, a node blip. That one recovery stamps
a `ReadySince` newer than `survival-0`'s last counted failure; the streak goes
back to zero, the six-failure give-up starts over, and the condition that
exists to name this stall need never turn true. There is no bound on how long
that can go on, because there is no bound on how often a healthy server may
blip.

Recorded rather than fixed in 5a, and the reasoning is on the record: a
per-ordinal streak changes what `BackoffInputs` means for a group of either
kind and deserves a design of its own rather than being appended to a milestone
whose scope is "persistent groups exist", and what it costs is a late or absent
condition rather than lost data or a disconnected player. 5b takes it, as
either a per-ordinal streak or a reset restricted to the ordinal that failed.

Until then the two status fields answer differently, and the difference is
worth knowing before you read either. `status.consecutiveFailures` **is** what
the sibling resets: `CountFailures` sets the count to zero when any view's
`ReadySince` is newer than the last counted failure, so for a multi-ordinal
group it can read 0 or 1 while an ordinal has been stalled for a day.
`status.lastFailureAt` survives the reset and keeps advancing —
`CountFailures` returns its watermark unchanged on that path, and the write is
guarded against zeroing it, deliberately: the comment beside it says clearing
it on a reset "would be the opposite of durable". So a `lastFailureAt` far in
the past beside a low `consecutiveFailures` is itself the signature of this
issue rather than a sign that nothing is wrong.

What does not lie at all is the `Server` objects:

```bash
kubectl get server -n <namespace> -l spawnery.cloud/group=<group> \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,FAILED:.status.failedAt
```

A persistent ordinal sitting in `Failed`, or one that keeps reappearing with a
newer `.status.failedAt` and never reaching `Ready`, is what `Degraded` would
have told you about. The one test that exercises the give-up
(`TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp`) runs a single ordinal,
which is the one configuration in which none of this can show.

**Lowering `replicas` nominates the top ordinal whoever is on it.** The two
sizing rules do not agree about this, and they share one delete path.
`SelectDeletionCandidates` (`internal/controller/candidates.go`) skips any
server that `mayHavePlayers()`, so an ephemeral group shrinks around its
players and takes an empty server instead. `DecidePersistentSize`
(`internal/controller/persistent.go`) has no such guard in its surplus loop: it
sorts the ordinals at or above the new `replicas` and names them, highest
first. Lowering `replicas` from 3 to 2 therefore asks for `survival-2` with
players still on it.

What protects them from there is the ordinary drain, and only that: the Server
controller moves them through the proxies and waits
`spec.drain.timeoutSeconds` (60 by the CRD default), and anyone still connected
when that deadline passes is disconnected with the pod. Design §7's acceptance
criterion 3 now carries that qualifier; it previously read "without
disconnecting a player on it", unconditionally, which is true only of a drain
that finishes in time.

The alternative is worth naming rather than assuming: a `mayHavePlayers()`
guard here would mean a lowered `replicas` is not honoured at all while anyone
is online, because no other server can take ordinal N's place — an ephemeral
group has a different server to delete instead, and a persistent group does
not. Neither direction is free. If you need the players off first, empty the
ordinal before lowering `replicas`, or raise `spec.drain.timeoutSeconds` on the
group beforehand so the drain has time to finish.

**Two servers carrying the same `spec.ordinal` are invisible in both
directions.** `DecidePersistentSize` builds its `held` map as
`held[*v.Ordinal] = v.Name`, which is last-write-wins over the list order. If
two `Server` objects of the group carry the same `spec.ordinal` — hand-created,
restored from a backup, or copied — one of them wins the map entry and the
other exists as far as the rule is concerned only in that it is never named.
It is never surplus, because the surplus loop walks `held` and the loser is not
in it; it is never recreated, because the ordinal is taken; and it goes on
running its own pod, mounting the claim named after *its own* name. If that
name is not `<group>-<ordinal>` the claim is a second world nobody is looking
at; if it is, two pods contend for one `ReadWriteOnce` volume, which hangs on
the volume rather than failing cleanly. It is the mirror of the case
`ConditionOrdinalBlocked` reports — there an object holds the *name* without
the ordinal; here it holds the *ordinal* without being reachable, which nothing
reports. The tell is the same shape:

```bash
kubectl get server -n <namespace> -l spawnery.cloud/group=<group> \
  -o custom-columns=NAME:.metadata.name,ORDINAL:.spec.ordinal
```

Two rows with one ordinal between them is the state; nothing on the group's
conditions, events or logs says so.

**An ordinal waits, visibly, for a pod that a dead node will never finish
terminating.** As of the branch review closing this milestone, the Server
controller refuses to create a pod while a pod of the same name still exists,
terminating or not (`internal/controller/server_controller.go`). That is what
it must do — creating into the name gets `AlreadyExists`, and the controller
would then adopt a pod it did not create and delete its own `Server` one pass
later — but it means the wait inherits whatever bound the termination has. For
an ordinary pod deletion that is `spec.terminationGracePeriodSeconds`. For a
pod on a node that has gone `NotReady`, there is none: the API server keeps the
object until a kubelet confirms the kill, and there is no kubelet to confirm
it.

The `Server` says so rather than sitting silent — `Accepted: False` with reason
`PodNameTerminating` and the pod's name in the message — and it says nothing
else: the server never reaches `Failed`, the per-group backoff never counts it,
and the phase stays `Pending` for as long as the wait lasts.

That used to be an accident and is a decision since 2026-08-24.
`status.startedAt` is now stamped when the operator accepts a Server rather than
beside its pod, so a Server with no pod does have a clock — and the deadline
that clock drives is deliberately not run while the pod's *name* is held by
another pod. Failing here would make the situation worse than the wait: the
replacement is derived from the same ordinal name and meets the same pod, a
`Failed` server holds its ordinal in `DecidePersistentSize`'s held map, and
`pruneFailed` does not run for a persistent group — so the object would stay for
its full `failedRetentionSeconds`, an hour by default, **including after
somebody force-deletes the stuck pod below.** The wait, by contrast, ends the
moment the name comes free.

```bash
kubectl get server <group>-<ordinal> -n <namespace> \
  -o jsonpath='{range .status.conditions[?(@.type=="Accepted")]}{.reason}: {.message}{"\n"}{end}'
```

The remedy is the same one a `StatefulSet` needs in this situation, and it
carries the same warning: force-deleting the pod object tells the API server
the container is gone without anything having verified that it is. On a node
that is merely unreachable rather than dead, the process may still be running
and still holding the volume, and the replacement will then contend for a
`ReadWriteOnce` claim the old pod has not released — which hangs on the volume.
Confirm the node is really gone first.

```bash
kubectl delete pod <group>-<ordinal> -n <namespace> --force --grace-period=0
```

## From the milestone 5a evidence run (2026-08-16)

**A clean recreate still logs twelve optimistic-concurrency conflicts, and
they are not a symptom.** The 2026-08-16 run — driven against a single-node
`kind` cluster at `f3c6fc1`, acceptance passed, blocks still there after the
pod was deleted and the client rejoined — logged twelve `the object has been
modified; please apply your changes to the latest version` errors across all
three controllers, one at essentially every state transition: the apply, first
readiness, the join, the leave, the delete, the replacement's readiness, the
rejoin. They are controller-runtime's ordinary retry path and they self-heal;
one of them is what produced the `PodAdopted` event the runbook's §8 explains.
Recorded so that somebody counting error lines in an operator log does not
read a healthy recreate as a fault.

## From milestone 5b (ordered shutdown, `Recreate` updates, storage growth)

5b closes the five gaps 5a's own handover named: an image change now moves a
persistent server, a lowered `replicas` takes one ordinal down at a time
rather than every surplus one at once, `spec.storage.size` growth reaches the
claim, a broken ordinal's failure streak survives a healthy sibling, and a
changed `motd` reaches a running proxy. What follows is what an operator finds
still open, checked against the code as it stands.

**The one-ordinal budget belongs to the nomination rule, not to the group: a
node drain takes down as many ordinals as the node held.** §2's invariant is
written "at most one ordinal of a persistent group is down at a time, whatever
the reason", and the last three words claim more than the code does.
`decision.Condemn` is attached for a persistent group like any other
(`internal/controller/servergroup_controller.go:711`) and `condemn()` executes
it ungated (`:779`), removing every server on a departing node in one pass — so
a node holding two ordinals takes both down. Gate A is not bypassed here: a
condemned view reads as `leaving()`, so the nomination rule declines to name
anything on that pass. What it cannot do is throttle the drain, and an ordinal
it nominated on an earlier pass can still be draining while the drain lands, so
three ordinals of one group can be out at once.

The behaviour is not the part to fix. "From milestone 4c-3" above records why
condemnation is unthrottled — draining one server at a time makes `kubectl
drain` wait out `drain.timeoutSeconds` once per occupied server rather than
once for the node — and a node that is leaving takes its pods with it whether
or not this operator moves their players first. The group's
`PodDisruptionBudget` is a bound on a *different* thing and is worth not
confusing with this one: sized to the occupied pods
(`servergroup_controller.go:1226`), it refuses the eviction API an occupied
pod, so somebody else's drain cannot disconnect players out from under the
condemnation. It does not bound this operator's own deletes, which never go
through eviction.

**A permanently broken ordinal stalls the group's whole update.** §2's
invariant — at most one ordinal taken down at a time — is held by waiting for
every required ordinal to be `Ready` (Gate B) before a stale or resize-pending
takedown proceeds. An ordinal that can never become `Ready` therefore holds
the whole group at its current spec forever; nothing times this wait out.
Inherited from `StatefulSet`'s shape knowingly, and tolerable only because the
stall is reported: `ConditionDegraded` publishes for a persistent group since
5a, and 5b's failure-streak fix (below) is what makes it actually arrive
rather than being reset forever by a healthy sibling.

**A spec edit made during the upgrade window can be missed on an ordinal that
is adopted rather than replaced.** Every server that predates 5b carries an
empty `Server.spec.podHash`, and `adoptServers`
(`internal/controller/servergroup_controller.go:1136`) stamps the current hash
onto such a server without ordering a takedown, rather than nominating it as
stale — the alternative would restart every persistent world in the
installation on the first reconcile after the upgrade. The cost is that a spec
edit landing inside that same reconcile can be adopted along with the old pod
rather than triggering a rebuild; it is bounded by the next edit, which will
compute a hash that no longer matches.

**The positive half of storage growth cannot be shown on `kind`'s default
storage class.** `kind`'s `local-path` provisioner reports
`allowVolumeExpansion: false`
(`docs/runbook-milestone-5a-evidence.md` §2's own `kubectl get storageclass`
output), so raising `storage.size` against the default cluster this
repository's other runbooks use can only ever exercise the rejection path —
`ConditionStorageResize` turning `False` with the class named. Confirming that
a claim actually grows, and that a driver's `FileSystemResizePending`
condition restarts exactly the ordinal that needs it, requires a driver that
supports expansion (`csi-driver-host-path`), which is extra cluster setup
`docs/runbook-milestone-5b-evidence.md` §4 keeps separate for exactly this
reason.

**The synchronous resize rejection cannot be diagnosed from the error.**
`growClaim`'s patch (`internal/controller/server_controller.go:420-453`) can
be refused by the API server for at least two different reasons carrying the
identical shape: `allowVolumeExpansion: false` on the storage class, and a
claim that is not dynamically provisioned at all (verified empirically:
`reason="Forbidden" code=403
message="only dynamically provisioned pvc can be resized and the storageclass
that provisions the pvc must support resize" Causes:nil` — the same response
for both). `status.storageResizeError` therefore names `allowVolumeExpansion`
as the first thing an operator should check, not as the established cause,
and the message says so explicitly.

**The generation-change reset is now partly undone for a persistent group
whose stale-generation corpse is still present.** The reset at
`internal/controller/servergroup_controller.go:208-210` zeroes
`status.consecutiveFailures` and `status.lastFailureAt` whenever
`metadata.generation` moves, on the reasoning that a spec edit is the
operator's answer to whatever broke. For a persistent group the very same pass
now counts failures over the *unfiltered* view list (`ofGeneration` is
ephemeral-only as of 5b — see below), so a stale-generation `Failed` corpse
still holding its ordinal is counted right back in on the same reconcile: the
count returns to 1 rather than to 0.

*It returned to the number of corpses until 2026-08-24*, when the count became
rounds rather than servers (see milestone 4d). Four held ordinals used to put
the group four-sixths of the way to a terminal give-up on the very pass the
operator's spec edit was meant to answer for them. They are one round however
many of them there are, so it is one now, pinned by
`TestAGenerationResetLeavesOneRoundNotOnePerCorpse`.
`pruneFailed` does not run for a persistent group, and each corpse keeps its
own ordinal until its failed retention elapses, so with `replicas > 1` that is
one per ordinal that has one, bounded by `spec.replicas`
(`internal/controller/servergroup_controller.go:251-258`). Defensible — a spec
edit does not heal a broken ordinal, and an operator watching for a stall
should not read a non-zero count as "nothing happened" — but it means the reset
an ephemeral group gets in full, a persistent one gets all but one round of.

## From milestone 5c (detecting forwarding secret rotation)

5c is detection and reporting only: the Network controller reads the forwarding
secret each resync, records a salted digest of it in
`status.forwardingSecretHash`, stamps every pod it creates with that digest in
`spawnery.cloud/forwarding-hash`, and reports the comparison as two conditions
and two events. It restarts nothing and takes no ordinal down — automatically
orchestrated rotation stays deferred, unchanged and for the reason the master
design's §6.5 gives, that it needs registration to become generation-aware. The
restarts follow `docs/runbook-milestone-5c-secret-rotation.md`. What follows is
what an operator finds still open, checked against the code as it stands.

**The salted short hash does not defeat a targeted dictionary attack** on a
weakly chosen forwarding secret. `podspec.ForwardingHash`
(`internal/podspec/hash.go:168`) is eight bytes of
`sha256(network.UID ‖ 0x00 ‖ value)`, and the result is a pod label, which is
readable by anyone holding pod read access in the namespace — a far commoner
grant than read access to the Secret itself. The salt does the one thing it was
chosen for: it forces the work to be redone per network and makes precomputed
tables worthless across installations. It does nothing against a guess aimed at
one particular network, which is an offline test per candidate against a
sixteen-character digest. A forwarding secret chosen the way a password is
chosen is guessable this way; one generated at random is not.

**The stamp says what a pod loaded at start**, not what it would load now. That
is the point — the kubelet refreshes the projected file underneath a running
pod and neither Velocity nor Paper reads it a second time, so the creation-time
value is the only one that describes the running process
(`internal/podspec/labels.go:56-74`). The cost is at the other end: **the stamp
is the last digest the operator read, not necessarily the bytes the pod
mounted.** `status.forwardingSecretHash` keeps its previous value on *any*
failed read (`api/v1alpha1/network_types.go:75`, guarded at
`internal/controller/network_controller.go:125-134`), and both builders stamp
it whenever it is non-empty (`internal/podspec/server.go:441-443`,
`internal/podspec/proxy.go:300-302`). The kubelet meanwhile projects whatever
the Secret currently holds, which is independent of whether the operator may
read it. The five resolved reasons therefore split two ways:

- Under `SecretNotFound` and `SecretKeyMissing` there is nothing to project, so
  a pod created in that state never starts — it sits in `ContainerCreating`,
  and its label describes an intention rather than a fact. Here the stamp
  misreports a pod that is not running.
- Under `SecretReadForbidden` and `SecretReadFailed` the Secret may be
  perfectly present, so such a pod **starts normally**. If the value is rotated
  inside that window — the reader Role removed, or a transient API failure —
  the pod loads the new bytes and carries the old digest, and once the read
  recovers the operator reports it stale while it is in fact current. The read
  recovering stops *further* pods being mis-stamped; it does not correct the
  ones already created, whose labels stay wrong until they are rolled. So "the
  stamp misreports only a pending pod" is not true of these two reasons, and a
  rotation performed while the operator cannot read the secret produces a
  network the condition describes incorrectly for as long as those pods live.

**Rotation detection is off until an install step is performed per namespace.**
The operator's ClusterRole grants no access to Secrets outside its own
namespace, by design: `config/rbac/forwarding-secret-reader.yaml` has to be
applied into each namespace holding a Network, and it is deliberately not part
of `config/deploy/`, for the reasons the manifest itself gives
(`config/rbac/forwarding-secret-reader.yaml:5-14`) — an administrator opens
exactly the namespaces that hold a Network, and the operator never creates
these itself, because one that may write RBAC makes every other restriction on
it advisory. Until it is applied, the `GET` is denied, the Network reports
`ForwardingSecretResolved=Unknown/SecretReadForbidden` with a message naming
the manifest and the `kubectl apply` line
(`internal/controller/forwardingsecret.go:71-78`), and
`ForwardingSecretRotationPending` reads `Unknown/SecretUnresolved` because
there is no digest to compare against. So the gap announces itself rather than
hiding — but it is a gap, and a namespace nobody opened has rotation detection
that reports nothing about rotations.

*Not closed by milestone 6d, on purpose — the design changed rather than the
gap.* This entry expected the Helm chart to render this Role for each
configured network namespace; 6d's design
(`docs/superpowers/specs/2026-08-19-helm-chart-design.md` §2) decided the
opposite: a chart installed once cannot know the game namespaces a user will
create later, so `config/rbac/forwarding-secret-reader.yaml` stays a
hand-applied file, exactly as it was. What 6d changed is elsewhere — an
operator installed outside the chart's default namespace now needs a manual
edit to this file's RoleBinding subject before applying it anywhere, and
`charts/spawnery/README.md` carries that edit in its installation steps. The
consequence of skipping it is narrower than a first read of the design
suggests: `ServerGroup`s and `ProxyGroup`s in the namespace keep scheduling,
and only rotation detection stays blind — see "From milestone 6d" below.

*Observed working, 2026-08-22, which nothing before this had.* The step was
taken on the one real installation during the RKE2 rollout — `kubectl get role
-n minecraft` shows `spawnery-forwarding-secret-reader` created 2026-08-20 —
and the whole chain reports healthy on that cluster:
`ForwardingSecretResolved=True/SecretResolved` naming the secret and its key,
and `ForwardingSecretRotationPending=False/ForwardingSecretInSync`. So the
`Unknown/SecretReadForbidden` path this entry describes is what a namespace
nobody opened reports, and the opened case now has a witness rather than only
tests.

**The reader Role does not carry `resourceNames`, and the master design asks
that it should.** §8 of
`docs/superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md:776-779`
says the secrets grant is to be "restricted through `resourceNames` to the
secrets referenced in networks". The rotation design's §2.2 rejects
`resourceNames` for a *watch*, correctly — the clause does not restrict `list`
or `watch` — but the grant that was kept is `get`-only
(`config/rbac/forwarding-secret-reader.yaml`), and `resourceNames` restricts
`get`. So the narrowing is available and is not taken. Deliberately: the
manifest applies with no editing at all today, and `resourceNames` would make
every administrator hand-edit it per namespace and edit it again whenever a
Network's secret is renamed; and the operator holds `pods: create` cluster-wide
(`internal/rbacaudit/required.go:52`), so it can mount any Secret in those
namespaces into a pod it creates, which makes a name-scoped `get` defence in
depth rather than a boundary. Milestone 6's Helm chart, where a per-namespace
value renders the list, is where it becomes cheap. **The audit that guards this
manifest refuses rather than models it.** `rbacaudit.Permission` still carries
no name, so a rule restricted to particular objects cannot be represented — but
since 2026-08-24 `ExpandRules` fails on such a rule instead of expanding it
into one that reads as unrestricted, which is the direction that matters: the
silent version had `Compare` reporting the permission satisfied for every
object when it was satisfied for one. Whoever takes the narrowing up gets an
error naming the names.

**The mis-stamp window is not confined to failed reads.** The two entries above
document what a *failed* read does to the stamp; the successful path has a
narrower version of the same gap, and nothing in the code or the runbook says
so. Between the Secret changing and the Network controller persisting the new
digest, a pod created by a group controller mounts the new value and is stamped
with the old one. That interval is up to one `resyncInterval`
(`internal/controller/network_controller.go:153`,
`internal/controller/server_controller.go:75` — five seconds) for the Network
controller to notice, plus informer lag before the group controllers see the
new `status.forwardingSecretHash`: both builders copy it out of the Network
object their reconciler holds from the cache
(`internal/podspec/server.go:441-443`, `internal/podspec/proxy.go:300-302`).
The stamp is written at creation and never revised, so that pod reads as stale
for as long as it lives. The ordinary runbook roll sweeps it up, because it
happens after the digest is recorded — but a pod created that way *after* its
own group was already rolled, by a scale-up or a replacement, stays falsely
stale until something rolls it again. `RotationPending` then keeps naming a
group whose work is done. A third way needs no window and no failed read at
all: the premise the stamp rests on — the process reads the file once
(`internal/podspec/labels.go:56-60`) — holds for the pod and not for the
container, because `RestartPolicy` is `Always`
(`internal/podspec/server.go:387`, `internal/podspec/proxy.go:277`) and the
forwarding secret is projected without a `subPath`
(`internal/podspec/server.go:166-191`, `:291`), so a container that restarts
after a rotation starts on whatever the kubelet has since refreshed onto that
file, and a pod that crash-looped and then recovered — leaving `podTerminal`,
and counted again — is reported stale while its process runs the new secret.

## Preconditions for milestone 6 (Helm, RBAC, E2E)

**Milestone 2a's isolation promise does not cover availability.** The promise
of the agent channel reads: a compromised game server pod cannot harm any
other. For identity and confidentiality it holds — the token is
audience-bound and accepted nowhere else, the `spawnery-server`
ServiceAccount has no RoleBinding anywhere, pods run with
`automountServiceAccountToken: false`, the private CA key never leaves the
operator secret, and identity comes exclusively from the token and never from
what the agent claims about itself.

For availability it does not, and 6b narrowed it without closing it.
`internal/agentserver` now sets `MaxConcurrentStreams`, `ConnectionTimeout`,
`MaxConnectionIdle` and a keepalive policy; `internal/grpcauth` caches
TokenReviews and rate-limits cache misses per peer; the chart ships a
NetworkPolicy admitting 9443 only from pods labelled
`spawnery.cloud/managed-by`. Read what each bounds rather than the group:
**none of them bounds how many connections one pod may open**, which is the
attack. `MaxConcurrentStreams` bounds streams on one connection and an agent
opens one; a connection carrying a live stream is never idle, so the reaper
does not touch it; the rate limit is unreachable by a pod replaying one valid
token, because that pod hits the cache; and a *compromised managed* pod
carries the label the policy admits.

What 6b did remove is the API-server amplification, and the cache is what does
it — observable from inside the operator in
`spawnery_agent_token_review_cache_hits_total` beside its misses and
`spawnery_agent_rate_limited_total`. Anyone quoting milestone 2a's promise has
to quote this entry with it.

## From the milestone 6a Task 4 measurement round (2026-08-17)

**A denied permission on a type reached only through the manager's cache
produces no signal at all — not a late one, an absent one.** Measured by
removing `pods:list` from every marker that grants it, confirming the
revocation at the API server, and then watching the operator continuously for
seven minutes and forty-five seconds, well past `Controller`'s two-minute
`CacheSyncTimeout`: the log gained not one line past the initial
`"Starting workers"` burst, `rest_client_requests_total` recorded `200`,
`201`, `404` and `429` across 24 samples and never `403`, and the restart
count stayed `0`. So `theOperatorWasNeverDenied` cannot see it, and neither
can a person reading the log.

The class is now largely covered from the other side. Since
`testenv.RestrictedClient`, removing `pods:list` turns **231** controller
tests red, because those tests call through to a live List rather than only
registering a watch. What is left uncovered is a permission reached
*exclusively* through `Owns()`, `For()` or one of the restricted caches in
`cmd/spawnery-operator/main.go`, on a path no controller test drives — and
for that class a green `make e2e` still proves nothing, because the failure
mode above is silence.

## From milestone 6a (the operator in a cluster)

6a gives the operator its own OCI image, one publish path for all three images,
and `make e2e`: a `kind` cluster in which the operator runs as a Deployment
under its own ServiceAccount while a Go test package drives it through twelve
ordered subtests — design §7.1's nine scenarios, plus the operator's own
health, plus §7.3's permission table against the real authorizer, plus §7.2's
last one, which reads the operator's whole log for `is forbidden:`. The section
above — Task 4's
measurement round — belongs to this milestone too and is not repeated here; the
first item below is what answered its open question.

**The denial check fires on a write verb, and the shape of what it misses is
narrower than "reads".** Removing `create` on `pods` from the markers produced
a quoted `... is forbidden: ... cannot create resource "pods" ...` on the first
attempt, with the operator still healthy and `theOperatorWasNeverDenied` still
able to read it — the combination Task 4 tried four ways and could not produce.

Be exact about the other side, because it is easy to over-read. What was
revoked and watched is **two cache-backed `list` verbs**: `pods: list`, for
seven and three-quarter minutes continuously, and `networks: list`. Neither
produced anything. **No uncached read was ever revoked and watched**, so
"the check misses read verbs" is not a measured statement — it is the
conclusion of a hypothesis, and the hypothesis is the one the section above
declines to assert: that such a read goes through the manager's cache, whose
initial sync is a watch rather than a `list`, so a revoked verb never reaches a
request the API server could deny, while a write goes to the API server
directly and does. That hypothesis would also have to account for the anomaly
the section above records and does not explain. What is established is the
asymmetry between a revoked write and those two revoked lists, and no more.

**And a denied read can escape this check for a reason that has nothing to do
with the cache.** `readForwardingSecret`
(`internal/controller/forwardingsecret.go`) deliberately uses the *uncached*
reader, so a missing `secrets: get` in a `Network`'s namespace really is a 403
from the API server — and the function folds it into a `forwardingRead` whose
message reads "the operator may not read secret …", with no `is forbidden:`
substring anywhere in it. The read sits after `network_controller.go`'s
`Accepted` branch has already returned, so no scenario fails either, and that
controller makes no logger call at all. The check would stay green through it.
`hack/e2e.sh` claimed the opposite until this was checked; its comment now says
so. The general lesson for anyone extending the check: it can only see what
something logs, so an error the code handles well is invisible to it by the
same mechanism that makes the handling good.

**A verb is only proven by a test that runs under the operator's own identity
*and* takes the path needing it, and `make e2e` cannot do the second.**
`selectRetirement` only nominates when the group holds a Ready server of the
current generation, and `test/e2e/manifests/e2e.yaml` names an unresolvable
image on purpose, so nothing in that harness is ever Ready — an image change
there produces no nomination at all. Since 2026-08-25 the controller tests
carry the first half instead: `testenv.RestrictedClient` gives every
reconciler the operator's ServiceAccount and exactly the generated
ClusterRole, so any path those tests already take proves its own verbs. What
remains uncovered is a path no controller test takes, and `make e2e` is still
the only place the operator's real identity meets a real cluster.

**The E2E cluster is a single node, so a whole class of behaviour is
untouched.** `hack/e2e.sh` creates one `kind` cluster with its default
single-node topology. Nothing in the run can reach node drain and its taint
handling (4c-3), a PodDisruptionBudget's effect on a real eviction, `HostPort`
and its CNI dependency, a `LoadBalancer` address, or CIS `restricted` pod
security. Those belong to the RKE2 rollout at the end of milestone 6 (design
§12).

*Three of the five are driven on `paulwtf` as of 2026-08-25, against the
deployed `v0.2.0` operator.* CIS `restricted` and `HostPort` are under
milestone 6c above. **Node departure is driven for the cordon leg**: `kubectl
cordon server03`, which carried one of the two `gateway` proxies. The operator
replaced it without being asked twice and the timestamps are the interesting
part — the replacement was scheduled to `server02` and started **eleven seconds
before** the condemned pod was stopped, which is make-before-break doing what
it is for on a real scheduler. `NodeDraining` went `True` and back to `False`
inside one twenty-second polling window, so the group read `Ready` with two
replicas throughout and `mc.paul.wtf` never had fewer than one proxy behind it.

The **taint** leg — `IsDeparting`'s second branch, the one an autoscaler
drives — a cordon never reaches, since it sets `spec.unschedulable` instead. It
is driven now, but in envtest rather than here:
`TestATaintedNodeCondemnsOnlyWhenItsKeyIsConfigured` taints a real `Node` the
API server holds and reads it back through `nodeDeparting` on the path a
condemnation takes. Before that, both halves were covered only where they cost
least — `IsDeparting` over a `Node` built in memory, and
`TestSetupAllThreadsTheDrainTaintKeys` over the option that reaches the
reconciler. It was not forced on `paulwtf`, and the reason is the measurement
under milestone 4c-3 below: that operator passes no `-drain-taint` at all, so
the branch cannot fire there, and giving it one means editing a Flux-managed
`Deployment` on a live cluster to exercise a branch envtest already drives.

A **PodDisruptionBudget's effect on a real eviction** was driven on 2026-08-20
and is recorded under the RKE2 rollout below, not here — with a real player,
both budgets at `minAvailable 1` and `disruptionsAllowed 0`, and the eviction
API answering `TooManyRequests`. The sentence this paragraph replaced said it
was still open, which was a misreading of the line above: the *harness* cannot
reach it, and the rollout did. What remains true for an idle cluster is only
that it cannot be re-driven on demand — both PDBs sit at `minAvailable: 0`
precisely because no pod is occupied, which the rollout also measured and found
correct.

**No image in the run resolves, by decision, so no game or proxy process ever
starts.** Every pod sits in `ErrImagePull`/`ImagePullBackOff` for the whole
run. Out of reach in consequence: the second stage of the ready gate, which
needs a connected agent; `ServerReconciler.syncOccupiedLabel` and the PDB
upkeep, which need a server that has been `Ready` once; growing a claim; and a
join. There is no cheap stand-in either — the server pod's readiness probe
execs `/usr/local/bin/spawnery-slp` against `127.0.0.1:25565`, and both that
binary and something answering a server-list ping exist only in the real Paper
image.

**A claim binds in this harness even though no container ever runs.** kind's
default class is `rancher.io/local-path` with
`volumeBindingMode: WaitForFirstConsumer`, which binds on the first pod
*scheduled* to consume the claim, not the first pod that actually runs. The
pods here are scheduled — a single control-plane node has nothing stopping
that — and only then get stuck pulling. An earlier version of the manifest's
own comment asserted `Pending` from the binding mode alone, without checking;
the measured answer is `Bound`.

**A group's count of live servers briefly touched zero under sustained
churn** before recovering — observed during Task 5, not investigated, and
recorded here for whoever next looks at backoff and replacement timing.

## From milestone 6b (NetworkPolicies, and the channel's availability half)

6b writes two `NetworkPolicy` objects — one per accepted `Network`, into that
network's own namespace, selecting its server pods; and one in `config/deploy/`
selecting the operator pod — and closes the availability half of the agent
channel with gRPC bounds, a `TokenReview` cache and a per-peer rate limit. The
first entry below is the one that governs how every other sentence in this
section, in the README, and in the handover has to be read.

Every `config/deploy/` path in this section is where the file was in 6b.
Milestone 6d moved that directory into `charts/spawnery/templates/`, and its
own section below opens by saying so; the paths here are left as they were
written because they date what was measured.

**kindnet, the CNI the end-to-end harness runs on, was measured not to enforce
a NetworkPolicy ingress rule — and measured is the operative word.** Task 3
deleted the peerless kubelet-probe rule from `config/deploy/networkpolicy.yaml`,
leaving a policy that selects the operator pod (which makes it default-deny for
ingress) and admits only the agent peer on 9443 — so the kubelet's probe to the
health port is denied outright by the object in force. `make e2e` then stayed
green: the rollout succeeded on its usual timeline
(`deployment "spawnery-operator" successfully rolled out`) and all twelve
subtests passed. Two alternative explanations were closed rather than waved at:

- the operator's readiness probe is an `httpGet` to `/readyz` on the health
  port (`config/deploy/deployment.yaml`), which travels the real network path,
  and `kubectl rollout status` cannot return success without one passing — so
  the denied path was genuinely exercised inside the window; and
- `hack/e2e.sh` creates the cluster afresh on every run and the apply log for
  that run reads `networkpolicy.networking.k8s.io/spawnery-operator-agent
  created` rather than `unchanged` — so the mutated policy was genuinely in
  force, not left over or skipped.

That leaves one explanation: the CNI passed traffic its policy denied. Be
precise about the scope of that, because the wider claim is the one everything
downstream leans on: what was measured is **one ingress rule, on one path**.
That kindnet implements no NetworkPolicy controller at all — no ingress rule
and no egress rule, for any pod — is what kindnet's own documentation says, and
this project's rule is that a mechanism is not evidence, which applies to a
CNI's README as much as to a shell script. The measurement and the
documentation agree, and neither of them has been extended to egress here. The
practical difference is nil: on this harness nothing 6b writes has been shown
to refuse anything, in either direction.

**The policy defends against a co-tenant that cannot create pods, and against
nothing else.** Its ingress peer is a podSelector over labels a pod's own
creator chooses, so anyone who may create a pod in a game namespace can wear
its colours. Closing that would buy little: the same privilege reads
`velocity-forwarding-secret` outright, measured 2026-08-21 by mounting it from
an unlabelled pod. The boundary is the namespace, not this policy.

It does refuse connections where a CNI enforces it, which took a real cluster
to establish and is recorded at `internal/podspec/netpol.go`: on 2026-08-21 a
pod carrying the managed-by, network and role=proxy labels reached a backend
on 25565 while the same pod without labels timed out, and on 2026-08-25 an
egress-deny policy cut a proxy agent's stream (scenario 12 of the rollout
runbook). What it is written against is the invariant open since 3b — a Paper
server runs `online-mode=false`, authenticates nobody, and trusts whatever
completes the modern-forwarding handshake with the right secret.

**Proxy pods are selected by no policy 6b writes, and the reason is an
asymmetry in how the two pod classes are probed.** A server's readiness probe
is an `exec` of `spawnery-slp` against `127.0.0.1:25565`
(`internal/podspec/server.go`), which runs inside the container over loopback
and which no NetworkPolicy governs; a proxy's is a `TCPSocket` from the kubelet
to `ProxyReadyPort` (`internal/podspec/proxy.go`), which one might. Selecting
proxies would therefore have put the whole fleet's readiness at the mercy of
whether a given CNI subjects kubelet traffic to policy — the risk the milestone
6 handover made an acceptance criterion. 6b removes that risk instead of
testing it, and the price is stated rather than hidden: **nothing restricts who
may open a TCP connection to a proxy's 25565 from inside the cluster.** The
proxy is the public front door — it sits behind a NodePort with
`externalTrafficPolicy: Local` — so a rule there would have to admit the world
on that port anyway, and unlike a backend it authenticates its players.

**A game namespace is one trust domain, and the per-`Network` policy is not a
boundary inside it.** This entry used to read "an unlabelled pod in a game
namespace is unrestricted", which is true and is the less interesting half.
Measured on 2026-08-21 against Cilium on `paulwtf`:

- a pod carrying `spawnery.cloud/managed-by`, `spawnery.cloud/network=production`
  and `spawnery.cloud/role=proxy` — labels anyone creating a pod may write —
  **connected to a backend on 25565**;
- the same pod without them **timed out**;
- and an ordinary unlabelled pod **mounted `velocity-forwarding-secret`**, 44
  bytes of it, because any pod may mount any Secret in its own namespace.

So `pods: create` in a game namespace is equivalent to access to that network,
by two independent routes, and the label filter's forgeability adds nothing to
whoever holds it. Nor can the operator close it: vanilla NetworkPolicy's peers
are `podSelector`, `namespaceSelector` and `ipBlock`, and inside one namespace
the first is forgeable and the second says nothing. **No policy expressible
here tells a real proxy from an invented one.**

What the policy does defend, and defends well, is the co-tenant that *cannot*
create pods — a compromised workload cannot relabel itself, and the second
measurement above is what that looks like from the inside. The
`spawnery.cloud/network` label also keeps the pods of a *losing* Network in a
two-Network namespace outside the winner's policy, unchanged from before.

Closing it properly would mean moving proxies to their own namespace, so a
`namespaceSelector` could discriminate, or an admission webhook forbidding
foreign pods the `managed-by` label. Both were considered on 2026-08-21 and
neither was taken: the first breaks "a Network owns its namespace" and the
second brings certificates and a failure mode to an operator that has no
webhooks. The boundary is the namespace, and `charts/spawnery/README.md` now
says so where an administrator chooses one.

**Proxy egress is unrestricted, and vanilla NetworkPolicy is the reason.** A
proxy configured `onlineMode: true` has to reach Mojang's session servers,
whose addresses are neither stable nor discoverable, and a `NetworkPolicy`
cannot name a destination by DNS name — only by pod, namespace or CIDR. An
egress rule for it would have to be an `ipBlock` over addresses nobody can
pin, so 6b writes none. A cluster that wants it needs a CNI with FQDN policies
(Cilium, for one), which is not a portable assumption and therefore not
something the operator can render. Backend egress is *written* against by the
per-`Network` policy, whose egress half **admits** cluster DNS and the
operator's agent port and nothing else — admits being the honest verb in this
section, since whether anything is thereby restricted is the CNI's business and
no run here has watched one refuse. It is safe to write that narrowly because a
backend needs Mojang for nothing: it is
`online-mode=false` by construction, so the Yggdrasil key fetch this file's
milestone 2b section records is already gone. The one outbound call that would
stop working where a CNI enforces is Paper's own update check to
`fill.papermc.io`, and 2b measured that one to fail harmlessly with no network
reachable — the server still reaches `Done` and answers a ping. Nothing has
observed it failing *this* way, through a policy, because nothing here enforces
one.

**Whether a pod-selector egress rule survives Service DNAT is settled for
Cilium and for no other CNI.** Two of the per-`Network` policy's egress rules
name a pod or namespace selector while what the pod actually dials is a
Service ClusterIP that kube-proxy DNATs: the operator hop
(`podspec.OperatorPodLabels()`, against `spawnery-operator.<ns>.svc`) and the
resolver hop (`kube-system` by namespace selector, against the cluster DNS
Service). A CNI evaluating policy pre-DNAT would drop both, and the rule would
have to be an `ipBlock` over the Service CIDR instead — which the operator
cannot discover from inside the cluster. The design (§6) declined to assert
which side any CNI falls on, and the pod-selector form is what ships.

On Cilium both rules match: `paulwtf` carries `production-backends` in
`minecraft` selecting `role=server`, and a backend under it reaches `Ready`,
which the design only grants once that server's agent has connected — so it
resolved the operator's name *and* dialled it through the ClusterIP. Verified
again 2026-08-25 on a pod rolled that day.

That is one CNI, not the class, and it cannot be widened where it would be
cheapest to test: kindnet enforces nothing, so an egress rule that matches and
one that does not are the same green in `make e2e`. The two failure symptoms
diverge and the misleading one comes first — the operator hop failing looks
like agents that never register, DNS failing looks like nothing resolving at
all *including* the operator's name, so agents failing to register is the
downstream effect and checking that first leads away from the cause.

**The peerless rule is the widest-open thing 6b writes, and one unit test is
all that stands behind it.** The operator's own `NetworkPolicy` (`config/deploy/networkpolicy.yaml`
at 6b, now `charts/spawnery/templates/networkpolicy.yaml`)'s second ingress
rule has no `from` at all — it admits 8081 and 8080 from anywhere in
the cluster — because the kubelet's source is a node rather than a pod and no
selector names it. That is the only formulation correct on every CNI, and it is
also the rule where a mistake is worst: an extra port there admits that port
from every source in the cluster. Since the harness enforces nothing, the
manifest test in `internal/rbacaudit/deploy_envtest_test.go` is the only thing
in this repository standing behind it — since milestone 6d that test reads the
rendered chart rather than the file directly, and the claim is otherwise
unchanged. Task 3's fix round made that check bidirectional — it had been
one-directional, and adding port 9999 to the peerless rule left it green — and
it now matches the container port *named* `agent` rather than the number
9443, so it survives a port change. The most dangerous mutation of all,
adding 9443 to the peerless rule, is caught.

**A `Forbidden` on the policy write stops the whole namespace, and the design
did not predict that.** `reconcileNetworkPolicy` is called after the `Accepted`
condition is set on the in-memory object but before any `Status().Update`
(`internal/controller/network_controller.go`), so an error there returns before
the condition is ever persisted. The design's §2.4 argued the failure needs no new
report because it is a fact about the installation rather than about the
`Network`. The final review ruled that argument wrong and the code right, and
§2.4 now carries the correction: **"no new report" was never on offer.** The
shape produces one anyway, on every group in the namespace, and it names the
wrong thing. `ServerGroupReconciler` and `ProxyGroupReconciler` both gate on the
`Network`'s `Accepted=True`, so a *fresh* `Network` in a cluster where the
operator cannot write NetworkPolicies never becomes usable, and every group in
that namespace refuses with `ReasonNetworkNotAccepted` and the message
`network "..." has not been accepted yet`. That message is true and misleading
in the same breath: the network was accepted, and the acceptance could not be
written down. An *existing* `Network` keeps its persisted condition and its
groups keep running, so only new ones are affected. Failing closed is the right
direction — an unprotected namespace does not quietly come up — but it is a
consequence, not a decision, and nothing but the operator log names the cause.
`test/e2e`'s `theOperatorWasNeverDenied` would catch it, since it is a denied
*write* and 6a measured that those are the kind that get logged.

*Driven 2026-08-25, and it does not — for a reason neither half of that
sentence covers.* `networkpolicies: create` was removed from the kubebuilder
marker **and** from `internal/rbacaudit`'s table, which is the sharpest form of
the case: absent from both, so the audit is green and only the running operator
knows. The consequence this entry predicts arrived exactly as written — no
`Network` ever reached `Accepted`, so no group ever sized, and the run's first
scenario reported `0 non-Failed Servers`.

That is what starves the check. Every scenario after it waits out its own
two-minute timeout on state that will never arrive, and
`theOperatorWasNeverDenied` is the *last* of twenty
(`test/e2e/e2e_test.go:129`). The package's own `go test -timeout 20m` fired
first: `panic: test timed out after 20m0s`, six scenarios short of the one that
would have read the log. The denial was in that log the whole time and nothing
looked.

So the claim is not wrong about what the check can see — it is wrong about the
check getting to look. A permission whose absence stops the fleet dead is
precisely the permission whose denial this scenario cannot report, because the
scenarios in front of it spend the budget waiting. Ordering it earlier, or
giving the harness a per-scenario deadline that fails fast rather than waiting
out state that cannot arrive, is what would close that — neither is done here.

The ordering was reviewed and left alone. Persisting `Accepted` before writing
the policy would let groups start servers in a namespace with no policy at all,
which is the one thing this milestone exists to prevent — the review weighed
the alternatives and judged each of them worse than the behaviour above. A
report naming the cause was available and was not shipped.

**Smaller ones, each worth a sentence:**

- No test asserts the per-`Network` policy's own `spawnery.cloud/network` and
  `managed-by` metadata labels. Both are there for a human reading `kubectl`
  output and nothing selects on them, so a wrong value costs nothing today —
  but nothing would catch one either.
- `TestADeletedPolicyComesBack` does not compare a UID before and after; it
  relies on envtest's synchronous delete plus the recorded mutation to
  distinguish "recreated" from "never removed". The mutation discharges it, the
  test's own text does not.
- The e2e scenario's owner-reference check asserts only `len(...) == 1`, not
  the referenced Kind, Name or UID. The unit test asserts all of them; the
  cluster-level one only counts.
- The token-review cache does not coalesce concurrent misses on the same token:
  two goroutines both call `reviewToken` and both store. Benign, and it means
  the cache does not itself deduplicate a hot token.
- `evictFullLocked` is not a hard cap: with `maxBuckets` peers all
  simultaneously active nothing is evictable and the map grows past it. That is
  the many-compromised-pods case the design ruled out of scope, recorded so the
  absence reads as a decision.
- **The rate limit lives inside `Authenticate`, not in the interceptor**, where
  the design's §5.3 sketched it. It has to: "consulted only when the cache
  misses" cannot be decided by a caller that has not yet looked in the cache.
  Worth knowing before reading the design and expecting it a layer up, and
  worth knowing what the placement costs — it forces the peer to be recovered
  from a `context.Context` rather than passed as a parameter, which is how a
  limiter keyed on the connection instead of the pod survived a whole
  milestone. The seam has to be tested deliberately.
- `newAuthFixture` (`internal/grpcauth/auth_envtest_test.go`) wires neither the
  cache nor the limiter, which is legal because both types' methods are
  nil-safe — and it means the package's existing envtest suite exercises the
  uncached, unlimited path. That is why a mutation to the cache broke nothing
  there and needed a test written for it.
- A `Why` field in `required.go` goes stale the first time a second call site
  appears, and nothing in the audit can catch it — `configmaps` and
  `pods: patch` both did. Checked 2026-08-25: 22 of the 23 identifiers those
  fields name still resolve to methods in this repository, and the 23rd is
  `Recorder.Eventf`, which is controller-runtime's.

## From milestone 6c (the LoadBalancer and HostPort expose strategies)

**`HostPort` and CIS `restricted` cannot both hold in one namespace.** Pod
Security `baseline` — which `restricted` inherits, per the Kubernetes Pod
Security Standards rather than anything measured here — disallows a container
`hostPort` outright, so a namespace enforcing either policy refuses every
`HostPort` pod's create, and `ProxyGroupReconciler` reports the refusal on the
group's own `Degraded` condition (`ReasonProxyPodRejected`) rather than ever
admitting one. This refusal is the one thing 6c observed being enforced:
`baseline`, not `restricted`, against a real API server, in both envtest
(`internal/controller/expose_test.go`,
`TestARejectedProxyPodIsReportedOnTheGroup`) and `make e2e`
(`test/e2e/expose_test.go`, `aForbiddenHostPortIsReportedOnTheGroup`).

*`restricted` itself is now measured too, on `paulwtf` on 2026-08-25.* A
throwaway namespace labelled `enforce: restricted`, a `Network`, and one
`ProxyGroup` at `expose.type: HostPort` port 25577. The group went
`Degraded=True`/`ProxyPodRejected` and quoted the API server verbatim —
`violates PodSecurity "restricted:latest": hostPort (container "velocity" uses
hostPort 25577)` — with no pod in the namespace at any point. So the sentence
this entry opens with is no longer inherited from the Pod Security Standards
for `restricted`; it is a thing this cluster did. Driven against the deployed
`v0.2.0` operator rather than a working tree, which is what makes it a
statement about what ships.

6a's handover §6 listed CIS `restricted` pod security and `HostPort` under the
cluster's real CNI among what the RKE2 rollout owed
(`docs/handover-milestone-6.md`), and this entry was written to say the two
could not both be honoured in one namespace. That rollout has since happened,
and it did not have to choose. Measured on `paulwtf` on 2026-08-22: the
`minecraft` namespace enforces `restricted` (`enforce` and `warn` both), and
the one `ProxyGroup` in it exposes `ClusterIP` with two ready replicas beside a
`Ready` `Server`, all of it up for over two days. So `restricted` against a
game server namespace is driven and holds; `HostPort` under the real CNI was
the leg that went undriven rather than the leg that conflicted, and nothing in
the cluster is standing in this incompatibility today.

*That leg is driven too, on 2026-08-25, and the answer is not the one the
phrase "under the real CNI" implies.* A namespace of its own with no Pod
Security label — the remedy this entry recommends two paragraphs down — and one
`HostPort` `ProxyGroup` at port 25577. The pod was admitted, went `Ready`, and
the group published `status.address: 45.137.203.198:25577`, having withheld it
until a ready pod actually declared that `hostPort`. So the CNI implements
`hostPort`: `cilium-config` runs `cni-chaining-mode = portmap` with
`kube-proxy-replacement = false`, which is the portmap plugin's job rather than
Cilium's eBPF.

What a player would meet is a different object entirely. From outside, 25577
times out while 25565 and 443 on the same node IP connect instantly — the
difference is `paulwtf`'s own `CiliumClusterwideNetworkPolicy`
`host-firewall-ingress`, which admits from `world` exactly 6443, 80, 443, 25,
465, 587, 143, 993, 5432 and 25565, plus ICMP echo, and drops the rest. Its own
description says so. **So `HostPort` on this cluster is a host-firewall
question, not a CNI one**, and the remedy is one port in that policy rather
than anything in this operator. Anyone reading the paragraph below about giving
the `HostPort` group a namespace of its own should read this beside it: the
namespace is necessary and not sufficient.

It stays recorded because the code cannot make the two compatible and the trap
is waiting for whoever picks `HostPort` later. The remedy is the runbook's to
take: give the namespace running the `HostPort` `ProxyGroup` a relaxed Pod
Security label, or a namespace of its own, separate from the `restricted`
namespaces the rest of the network runs in.

## From milestone 6d (the Helm chart)

`config/deploy/` no longer exists. The operator installs by
`helm install charts/spawnery`, and `internal/rbacaudit` now audits what
`helm template` actually renders rather than an intermediate on disk. Full
account: `docs/handover-milestone-6d.md`.

**`make chart-lint` cannot catch a chart that renders with an empty
namespace, and that is a property of Helm rather than of the target.** The
plan justified `chart-lint`'s `helm template` line by a chart that lints but
does not render. Measured directly with a typo'd `{{ .Release.Namspace }}` in
a template, and measured again on 2026-08-24 against the same Helm v4.2.3 the
flake pins: an unresolved `.Release` field renders as the empty string rather
than erroring, so both `helm lint` and `helm template` exit 0. Nothing at the
lint step can see it; `chart-lint` still catches a template that fails to
render at all, which is a different class.

What used to catch it was `TestAgentServiceReachesTheOperatorPods`
(`internal/rbacaudit/deploy_envtest_test.go`), incidentally, because it
applies the rendered Service into envtest's real API server and that refuses
a `Service` with an empty `namespace` — so the one object that test happens to
apply was covered and the other eight were not.
`TestTheChartRendersIntoTheNamespaceItIsGiven` did not close the gap either:
its literal scan looks for `spawnery-system`, and an empty namespace contains
no literal to find, and it reads the namespace of two objects out of nine.

Since 2026-08-24 `TestEveryRenderedObjectLandsInTheReleaseNamespace` reads
every object instead, with the optional templates switched on so the
ServiceMonitor and the PrometheusRule — both off by default, so the ordinary
render never sees them — are covered too. It carries the list of namespaced
objects it expects rather than a count, so a template that stops rendering
fails rather than passing by being absent, and one that is added fails until
somebody lists it.

**`TestARecreatedOrdinalCreatesItsPodOnceThePredecessorIsGone` flaked once and
has never done it again.** `internal/controller/server_controller_test.go`. It
failed during milestone 6d's Task 6 `make test`, passed in isolation and on a
full rerun, and 6d changed nothing it touches. Nothing was captured.

One thing is ruled out rather than assumed: **it is not cache lag.**
`internal/testenv`'s `Client` is `client.New`, a direct client with no
informer behind it, so the hypothesis everyone reaches for first with envtest
cannot be the mechanism. Sixty runs in isolation and five full-package runs on
2026-08-23 did not reproduce it.

The evidence against it has grown without anyone doing anything: `ci.yml`'s
`test` job runs `go test -race ./...` on every push and pull request, and over
86 runs since 2026-08-20T08:35Z it has **never** concluded `failure` — the ten
red runs are five `e2e`, five `lint` and three `deps`, none of them this
suite. So this is one occurrence standing against roughly ninety executions.

It stays because an unrecorded flake is rediagnosed from scratch. The
assertion now prints what it saw — the `Accepted` condition, every pod with
its `deletionTimestamp` and node, and whether the pod under the name is still
the predecessor *by UID*, since the name is reused across generations and the
first draft of that diagnostic reported "predecessor still present: true"
about a healthy successor. A second occurrence should be a diagnosis rather
than another data point.

## From milestone 6e (CI)

GitHub Actions now runs three workflows: `.github/workflows/ci.yml` blocks
four jobs — `test`, `lint`, `deps`, `e2e` — on every pull request and on
push to `master`; `.github/workflows/nightly.yml` runs `make image-repro` on
a schedule plus `workflow_dispatch`; `.github/workflows/release.yml` runs
`hack/publish.sh` on a `v*` tag. Full account:
`docs/handover-milestone-6e.md`.

**The verb set on both grants is exactly right and exactly minimal, and
this is readable in client-go rather than inferred from a green e2e run.**
The paragraph above leans on one `make e2e` `PASS` for the claim that the
grant is sufficient, and the next paragraph but one says how far that
reaches — not far. The verbs, at least, do not need it. In client-go
v0.36.0, the events broadcaster's `recordEvent`
(`tools/events/event_broadcaster.go:230-273`) calls exactly two methods on
its sink: `sink.Patch` at `:240`, when the event is a series, and
`sink.Create` at `:246` otherwise or when the patch found nothing to patch.
`EventSink.Update` is declared in the same package
(`tools/events/interfaces.go:71`) and called from nowhere in it. The
deprecated recorder's own `recordEvent` (`tools/record/event.go:330-341`)
has the identical shape, `sink.Patch` then `sink.Create` and no `Update`.
So `create;patch` on `events.k8s.io/events` and on `""/events` is neither
short a verb the library will reach for nor carrying one it will not, and
that is a statement about the library's source, not about a run.
`internal/rbacaudit` checks the rendered chart's RBAC against a
hand-maintained table in both directions, so it catches the table and the
role disagreeing. It once could not catch both agreeing while both were
wrong against what the code needs; `testenv.RestrictedClient` closes that for
any path a controller test takes. The only thing that has exercised
the operator's real ServiceAccount against a real API server since the
grant changed is one `make e2e` run, and that run's `PASS` reaches exactly
as far as the check it drives allows, no further:
`theOperatorWasNeverDenied` excludes any log line containing `violates
PodSecurity` (milestone 6c's own narrowing, so a deliberate Pod Security
refusal in an unrelated scenario in the same run cannot fail this one), and
two paths can carry a real denial without ever producing the `is forbidden:`
line the check looks for — a revoked cache-backed read (tried against pods
and against networks, watched for close to eight minutes, no log line and
no `403` in the operator's own client metrics either time) and
`readForwardingSecret` folding a real `403` into a condition message with no
`is forbidden:` substring and no log call at all. A grep of the CI job's own
stdout for `is forbidden:` was tried as independent corroboration of the
grant and does not stand as any: `hack/e2e.sh` prints the operator's pod log
only when the job's own exit status is non-zero, and the check's log source
reads the same log through the Kubernetes API in-process, never printing it
— so on a green run the corpus that grep searches structurally cannot
contain the thing being searched for, and a zero-match result against it is
not evidence about the grant one way or the other.

**What is still not covered anywhere, and is now at least recorded: the
`action` the events API takes at every one of this package's `Eventf` call
sites.**
`events.FakeRecorder` renders an event as `eventtype + " " + reason + " " +
note` (client-go v0.36.0, `tools/events/fake.go:36-38`) and drops `action`
entirely, so no assertion reading a fake recorder in this repository can
say anything about it — four of the action constants were replaced with
garbage during the final review and `go test ./internal/controller/` stayed
green in 87.7s. `go vet` gives no cover either: it cannot see through the
`events.EventRecorder` interface to know `Eventf`'s note is a format
string, and a deliberately broken format at the `PodCreated` site in
`internal/controller/server_controller.go` produced no diagnostic. The consequence of a call site passing `""` is a
silent loss — `events.k8s.io/v1` refuses the event, the broadcaster
classifies the `*errors.StatusError` as non-retryable and abandons it with
a `klog` line, and unit tests, envtest and e2e all stay green. Two guards
now stand in for what the fake cannot see, in
`internal/controller/events_test.go`:
`TestTheRealAPIServerRefusesAnEventWithNoAction` measures the premise
against envtest's real API server, and
`TestEveryEventfCallSitePassesAKnownAction` walks this package's own
non-test sources and requires the fifth argument of every `Eventf` call to
be one of `events.go`'s action constants.

The second is a source-level check, and two of its three assertions exist
because the obvious one alone was weaker than it looked — both found by the
re-review of the fix that added it. It matches the action argument by
*identifier name* and resolves no types, so `actionCreatePod := ""`
declared above a call site passed; it now also requires that no local
anywhere in the package shadows one of the constant names, which is
what makes matching by name mean anything (a package-level redeclaration is
a compile error, so the two together pin the identifier to the constant
without a type checker). And it logged the number of call sites rather than
asserting it, so deleting a call site outright left it green at 22, and a
controller moved into a subpackage would have dropped out of a
non-recursive scan the same way; the walk is recursive now and the count is
asserted against `wantEventfSites`.

What the check still cannot say — stated in its own comment as well — is
whether the constant a call site chose is the *right* one for that call
site. `actionSyncStatus` where `actionCreatePod` was meant passes every
assertion. Nothing observes the action end to end, and nothing will until a
test reads `Event.Action` back off a real API server for an event a
controller actually emitted.

**The rootless-podman path is now unexercised by anything automatic.**
Before this milestone there was exactly one way `hack/e2e.sh` ran: by hand,
on the author's machine, under rootless Podman with `KIND_EXPERIMENTAL_PROVIDER=podman`
and a `systemd-run --scope --user --property=Delegate=yes` wrapper — so
there was no gap between what was proven and what anyone relied on.
`ci.yml`'s `e2e` job now runs the same unmodified script on a hosted
runner's Docker daemon, which is a genuine second, independent execution of
it — eighteen scenarios green — but from this point on nothing automatic
ever exercises the podman path again. CI proves Docker; the author's machine
proves podman; neither proves the other, and a change that only breaks under
one container runtime can now sit green in CI indefinitely.

**The `if: failure()` step that opens a `nightly-red` issue has never run.**
`9a25874` gave `nightly.yml` that step, an `if: success()` step that closes
the issue, and `hack/require-no-red-nightly.sh`, which refuses a release while
one stands. The only red night this project has had was 2026-08-22, the day
before the step landed, so it has never written anything.

The two pieces downstream of it were driven on 2026-08-25 by standing in for
the failure: an issue opened by hand with the `nightly-red` label made the
gate refuse (exit 1, live rather than against a fixture, and that is the
branch whose silence is permission), and a dispatched green run closed it and
returned the gate to exit 0. Driving the writing step itself needs a
genuinely red nightly and nothing stands in for that.

**Only the nightly catches a hash that goes stale from outside.** `ci.yml`
builds `.#operator-image` unconditionally, through `hack/e2e.sh`, and reaches
no other image derivation on its own: `make test` and `make lint` enter Nix
only through `nix develop`, and the `deps` job builds
`.#agents.mitmCache.updateScript` and nothing else. Since 2026-08-23 an
`images` job closes most of that — `hack/image-derivations-changed.sh` decides
from a `git diff` whether anything defining `.#paper-image` or
`.#velocity-image` moved, and the two `nix build` steps run only when it did,
so a hash this repository breaks is caught on the pull request that breaks it
and every other push spends one diff.

What no diff can see is a fixed-output hash breaking because the bytes at a
URL changed, with no line here moving. `nightly.yml` builds all four
derivations unconditionally and is the only thing watching for that, which is
why its verdict is wired into the release rather than left in a run nobody
opens: it labels a `nightly-red` issue on failure, closes it on a later pass,
and `hack/require-no-red-nightly.sh` refuses to publish while one stands.

## From the RKE2 rollout (milestone 6, driven 2026-08-20)

Driven against `paulwtf`; the evidence is `docs/runbook-milestone-6-rollout.md`
and every claim here is a claim about that cluster on that day.

**No git tag can carry its own operator digest.** `hack/publish.sh` takes the
digest from `skopeo copy`'s `--digestfile`, which exists only after the tag has
been published, so the commit that writes it back into
`charts/spawnery/values.yaml` is necessarily behind the tag it describes. A
`HelmRelease` installing the chart at tag `v0.1.0` therefore runs the *tag*
`ghcr.io/spawnery/spawnery-operator:0.1.0`, not the digest — measured, the
install came up that way. The value in the chart is documentation of the
previous release, and a deployment that wants a digest pins it where the
deployment is described. The design's §4 and its acceptance criterion 2 both
assumed the opposite; both were wrong, and this is structural rather than an
oversight anyone could have avoided.

**Cilium will not share a LoadBalancer address between two `Local` Services
that select different pods.** Non-overlapping ports are necessary and not
sufficient. Measured:
`"compatible ExternalTrafficPolicy local but selecting different set of pods"`.
This is a property of `externalTrafficPolicy: Local` — the announcement would
be wrong for whichever Service lacks an endpoint on a given node — and it means
a cluster whose address pool is exhausted must choose between real client
addresses and a shared address.

## On the agent channel (`internal/certs`, `internal/agentserver`)

**A CA rotation is asked for, never scheduled — but its clock is now
visible.** (The procedure itself is [`ca-rotation.md`](ca-rotation.md).) `CALifetime` (`internal/certs/bundle.go`) is still ten years, and
nothing in the operator *starts* a rotation on its own. What changed on
2026-08-23 is that the remaining life stopped being invisible:
`spawnery_ca_expiry_timestamp_seconds` and
`spawnery_serving_cert_expiry_timestamp_seconds` are published from
`Provider.Set`, so every path that changes what the operator serves updates
them. The chart ships an optional `PrometheusRule`
(`metrics.prometheusRule.enabled`) whose `SpawneryCAExpiringSoon` fires at 90
days by default. The operator still
holds no threshold of its own: how many days should worry somebody is a fact
about a cluster, not about this code.

## Small things

- Since milestone 2a, `BuildServerPod` rejects a user mount that hits `/data` or
  `/tmp` exactly, that reuses one of the operator's reserved volume **names**,
  and that overlaps the agent mount path in either direction: the same path,
  nested underneath, **or an ancestor of it** (`checkMountCollision`). The
  asymmetry is intentional — mounting under `/data` is the documented way to add
  extra files, whereas mounting under or above the agent mount would shadow the
  token the agent reads its identity from. **Since 2026-08-24 it also refuses
  two user mounts that collide with each other**, by name or by path —
  including the same path spelled two ways, since the loop cleans before
  comparing exactly as `checkMountCollision` does. That function sees one
  mount at a time, so a collision *between* two of them is structurally
  invisible to it and the check belongs to the loop. The API server catches both, but as a rejected pod
  create that reaches a user as a `Degraded` condition quoting an apimachinery
  message about an index in an array.
