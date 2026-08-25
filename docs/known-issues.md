# Known issues and carry-overs for later milestones

Status: end of milestone 3c, the Velocity agent (2026-08-11).

This list collects what was deliberately left open during the implementation and
the reviews of milestone 1, milestone 2a, milestone 2b, milestone 2c, milestone
3a, milestone 3b and milestone 3c. It does not replace a spec — the design
decisions live in
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

**The image was large because `jdk25_headless` has a 697 MiB closure** — a
full headless JDK, not a JRE. Measured at 724 MB as a tarball for 26.2-0.1.0
and 735 MB for 26.2-0.2.0 once the agent joined it. *Cut on 2026-08-25: the
Paper image ships a jlink'd runtime of fourteen modules instead, and the
tarball is 372 MB.* Closure 405 MiB against 697; `nix/paper-jre.nix` carries
the list and how it was derived.

Getting there took two steps and only the first was mechanical. `jdeps
--print-module-deps --ignore-missing-deps --multi-release 25` over the whole
classpath — all 105 jars of `.#paper-repo` plus `.#agents`' agent jar — gave
thirteen modules with an empty stderr, so nothing was skipped despite the flag.
Paper then booted on those thirteen and died in `Paperclip.extractFiles` with
`java.nio.file.ProviderNotFoundException`: `FileSystems.newFileSystem` needs
the zip provider, which arrives through `ServiceLoader` rather than through any
reference `jdeps` can follow. `jdk.zipfs` is the fourteenth and no static
analysis could have produced it.

The remaining doubt was the agent's channel, since a boot without an operator
never opens one and a security provider reached by name — `jdk.crypto.ec` being
the candidate — would have failed the same way and only on a connection. `make
image-test` and `make agent-test` both pass on this runtime, the second driving
a real session with a TLS handshake against a rotated CA bundle, so that doubt
is closed rather than carried. Velocity's image is a separate derivation over a
separate classpath and still carries the full JDK; this list is Paper's.

**`k3d` does not work on this machine, and probably not on similar ones.**
`docker` here is a Podman 5.8.4 alias with no `/var/run/docker.sock`, only a
rootless Podman socket. k3d's tools node always bind-mounts the runtime socket
to the fixed in-container path `/var/run/docker.sock`; rootless Podman refuses
to create that mount point (`mkdir /var/run/docker.sock: permission denied`),
regardless of `DOCKER_HOST`. There is no workaround short of a rootful Podman
socket, which this user does not have group access to. `kind` under
`KIND_EXPERIMENTAL_PROVIDER=podman`, wrapped in
`systemd-run --scope --user --property=Delegate=yes`, works against the same
rootless socket and is what the README now documents. Milestone 6 settled the
CI half of this: `ci.yml`'s `e2e` job runs `make e2e` on a GitHub runner,
which has a real Docker daemon and needs none of the above. What the entry
still says is about this machine and machines like it — the local flow needs
kind that way, and the README documents it.

## From milestone 2c (the Paper agent)

**A read-only mount at `/data/plugins` breaks the start.** `image/entrypoint.sh`
copies the agent jar out of the image into `plugins/` under `/data`, because
Paper writes its plugins' data folders inside the plugins directory and a
read-only one takes Paper's own bundled plugins down with it. Mounts below
`/data` are the documented way to add files and `checkMountCollision` allows
them; a ConfigMap or Secret mount is always read-only (`internal/podspec`
sets `ReadOnly: true` on every user mount unconditionally). So a mount at
`/data/plugins` makes the `cp` fail under `set -eu` with a bare `cp:` message
that says nothing about why. This is the same shape as the `/data/config`
collision above and wants the same fix; the entrypoint already points here for
it.

**This entry still stands.** Unlike `/data/config`, milestone 3b did not
resolve it, and could not have with the same move: `/data/config` was
avoided by relocating the operator's *own* mount target to `/etc/spawnery`
(see above), but `/data/plugins` is not the operator's choice to relocate —
it is Paper's own plugins directory, and the agent jar has to land inside it
regardless of what a user mounts there. A user mount that names
`/data/plugins` — permitted, since `checkMountCollision` does not single it
out either — still breaks the start with the same bare `cp:` message.

**The relocation is not proven on the give-up path.** The cast to
`ClientCallStreamObserver` that the cancel needs sits inside a `runCatching`,
which catches `Throwable` — so a `NoClassDefFoundError` from a shading
regression would be swallowed and phase 3 of `make agent-test` would still pass.
Phase 3 being green is evidence that the bound holds, not that the cast resolves
under the shaded names.

**A permanently unreachable operator writes one WARNING every 30 seconds,
forever.** There is no rate limit and no deduplication on the reconnect log. One
line per pod per 30 s is nothing for one server and is a real log bill for a
fleet that loses its operator for a day.

*The fix this entry proposed was considered and refused, deliberately.* Both
`AgentPlugin`s now carry the argument at their `log` callback: `SessionLoop`
never gives up and never escalates on its own, so that callback is the only
place deciding how loud an unreachable operator gets to be. INFO would bury a
30-second cadence among routine startup lines, and by the time anybody looked,
"unmonitored for six hours" would be indistinguishable from "fine"; SEVERE
would misrepresent a condition the loop is already recovering from. Logging
only on a change of cause has the same defect as INFO — a fleet that has been
down all day would say so once, at the start. So the cost stands as written
and is accepted; what changed is that it is now a decision with reasons rather
than an oversight.

**The JRE module list is derived and shipped as of 2026-08-25 — see the
image-size entry above for how, and `nix/paper-jre.nix` for the list.**
The Paper-side classpath stopped moving with this milestone: the agent is the
last thing that joins it, and gRPC and okhttp pull modules Paper alone does not.
So the list can finally be derived from the complete classpath, with `jdeps
--print-module-deps` or `-verbose:module` against a running server. Milestone 3
adds Velocity and faces the same question with the same answer, so derive it
once, there, for both images — see the image-size entry under milestone 2b for
what it buys.

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

**The overlay's "refuse rather than guess" philosophy now covers the nested
documents, and still not `server.properties`.** Both flavours refuse an
overlay that does not parse — bad YAML, bad TOML; `paperGlobal` refuses one
whose `proxies` or `proxies.velocity` key parses to something other than a
mapping, and `velocityToml` refuses the same of `servers` and `forced-hosts`,
"rather than treating either as an absent overlay" (`paperGlobal`'s own doc
comment). Since 2026-08-24 they also refuse a key the receiving program does
not declare, at any depth, measured against that program's own default
configuration (`internal/render/declared.go`, `internal/render/defaults/`).
That was the class that had cost this project two outages, and the trade it
takes is stated where it is taken: a Paper or Velocity bump refuses a
legitimate override for a newly added key until the default file is
regenerated.

What is left is `server.properties`, and it is left by construction rather
than by omission. `parseProperties` accepts any `key=value` line, so a mistyped
key adds an unused one — and there is no fixture to check against, because
Paper's `server.properties` is Minecraft's own and this repository has never
measured it the way it measures `paper-global.yml` and `velocity.toml`. The
four keys the operator relies on there are in the critical layer and no
overlay can move them (`internal/render/paper.go`), so what a typo can reach
is the author's own settings and nothing this operator depends on. Closing it
would mean a third default file and a third regeneration step.

**The rendered ConfigMap's name changed, and nothing migrates the old
one.** `podspec.GroupConfigMapName` used to return the group's own bare
name; it now returns `<group>-<role>-config`. The rename fixed a real
collision — a `ServerGroup` and a `ProxyGroup` sharing a name could fight
over one ConfigMap, and a user's own ConfigMap named after their group
(their `configOverlay` ConfigMap, most plausibly) would have been adopted,
owner-reference stamped, and deleted the moment the group was. What the
rename does not do is carry an already-running cluster across the change: a
group reconciled under the old code has a ConfigMap at the old bare name,
nothing renames or deletes it once the new code takes over, and nothing
warns that it is sitting there orphaned. That was acceptable only for as
long as nothing was deployed against this code, and milestone 6 ended that
condition — but not the way this entry expected. The first cluster to run
this operator was installed at v0.1.0 on 2026-08-20, months of milestones
after the rename, so it never held a ConfigMap at the old bare name and the
migration gap is moot for it. What the gap now describes is a cluster nobody
has: one installed before the rename and upgraded across it. Read it as a
warning for whoever finds such an installation, not as a pending task.

It is also not fully closed on the receiving end: neither
`reconcileConfigMap` (`internal/controller/servergroup_controller.go` and
`internal/controller/proxygroup_controller.go`) checks that a ConfigMap
already sitting at the *new* name is actually owned by this group before
mutating it. `controllerutil.CreateOrUpdate`'s mutate closure sets the
label and `config.yaml` and calls `SetControllerReference` last, and
`SetControllerReference` only refuses when the object already has a
*different* controller owner — an object with no owner at all is silently
given one. A ConfigMap that happens to carry `podspec.LabelManagedBy` (so
the cache sees it) but was created by something other than this group's
reconciler is therefore still adoptable. The rename traded a plausible
collision (bare group name) for an implausible one (the exact rendered
name, pre-labelled); it did not remove the shape.

## From milestone 3c (the Velocity agent)

Design `2026-08-11-velocity-agent-design.md` §11 named five of these before
the milestone started; what follows carries them and adds what building it
actually found. The one that matters most is first.

**`internal/render/paper.go` wrote the Velocity forwarding secret under the
key `secret-key`; Paper reads `secret`.** Paper does not reject the unknown
key — it ignores it, preserves it verbatim on the next save so the file looks
correct, keeps `secret: ''`, and disables forwarding in its own
post-processing while logging *"Velocity is enabled, but no secret key was
specified. A secret key is required. Disabling velocity..."*. That line was
printed in every Paper container from the moment milestone 3b shipped, and
nothing read it. Backend-side forwarding had never worked. It was found only
by the first real end-to-end join, ten tasks into this milestone.

The portable lesson is bigger than the fix: **no test in this repository
measured the receiving program.** The render tests assert that the renderer
writes the string the renderer says it writes, which cannot fail on a key the
receiver ignores; and until this milestone no test had ever put a proxy in
front of a Paper server. Both halves are now closed — a fixture of Paper's
own default config checked against the renderer's key names
(`TestPaperWritesTheKeysPaperItselfReads`, against
`internal/render/defaults/paper-global.default.yml`), and an image test that
reads the file back out of the running container
(`hack/image-test.sh`) — but the class is what the next person needs to
carry forward: a green render test proves what was written, never what was
read.

**And the same two for Velocity**, which the whole-branch review found had
been missed: the lesson had been applied only to the flavour it was learned
on. `TestVelocityWritesTheKeysVelocityItselfReads` checks the renderer's key
names against `internal/render/defaults/velocity.default.toml`, extracted from
the pinned jar's own `default-velocity.toml`, and `hack/velocity-image-test.sh`
now asks the running proxy for a server list ping and reads `show-max-players`
and the motd back out of the answer. That script's readback of
`/data/velocity.toml` had been asserting over the renderer's own bytes:
Velocity never rewrites that file (`.autosave()`, and no migration fires at
`config-version = "2.8"`), which the script's own comment claimed the opposite
of. Its `playerLimit` fixture moved off 500 in the same change, because 500 is
both Velocity's default and `podspec.DefaultPlayerLimit`, so a misspelled
`show-max-players` is invisible against it. The one key still not read back
from Velocity is `forwarding-secret-file`, which nothing but a forwarded join
exercises; it is checked instead by the absence of the `/data/forwarding.secret`
Velocity generates when it cannot find the configured one.

**`online-mode` moved to the CRD.** `ProxyGroup.spec.config.onlineMode`,
defaulting `true`. It could not be set by a `configOverlay` because
`render.Velocity` reasserts the keys it owns after the merge, and the ruling
was that a security property switchable in a YAML file nobody reads is worse
than one visible on the custom resource. Turning it off means the proxy
stops authenticating players.

**Paper 26.2 accepts the forwarding secret from the environment**
(`PAPER_VELOCITY_SECRET`), so the plaintext need not be written into
`/data/config/paper-global.yml` in the writable layer at all. Not done; a
smaller attack surface for whoever next opens the Paper renderer.

**A proxy that cannot bind its ready port is silent on the CR.** It stays
`Pending` with the reason only in the container log
(`ReadyGate.open`'s own `log(...)` call). This is the same shape as the
`playerLimit` defect milestone 3b found and fixed, in a place where the
operator has nothing to write to.

**Per-proxy load balancing.** With several proxies, placement is even per
proxy and not necessarily across the network — `Router` counts only the
players Velocity itself can see, not what any other proxy in the same
`ProxyGroup` is carrying.

**The NetworkPolicy is overdue, not deferred.** With `online-mode=false` on
the backends and forwarding now actually working, a Paper server
authenticates no one and trusts whatever completes the handshake with the
right secret — and nothing restricts who may attempt it. Milestone 6 owns
NetworkPolicies as a group. This entry is the one most likely to be read as a
formality; it is not.

*Milestone 6b wrote the policy, and it has since been watched doing its
work.* Measured on 2026-08-21 against a real CNI and recorded at
`internal/podspec/netpol.go`: a pod carrying the managed-by, network and
role=proxy labels reached a backend on 25565, while the same pod without
labels timed out. What that measurement also established is the limit — the
ingress peer is a podSelector over labels any pod's creator chooses, so the
policy defends against a co-tenant that cannot create pods in the namespace
and against nothing else. The same day, mounting `velocity-forwarding-secret`
from an unlabelled pod showed why closing that would buy little. The boundary
is the namespace, not this policy, and the longer form is under "From
milestone 6b" below.

**Smaller ones**, each worth a sentence: phase 5 of `hack/agent-test.sh`
reuses phase 2's window constants declared 400 lines
earlier, both derived from a hard-coded renewal interval; and `streams_opened`
counts what the operator saw, so a proxy leaking a gRPC channel per reconnect
is still measured nowhere — the standing blind spot inherited from milestone
2c. Three entries that stood here are closed: phase 1's empty-token comparison now
carries the same guard phase 4 does; `Router.choose`'s fall-through when
the exclusion empties the first group is covered both by a unit test and by
the second fallback group `docs/runbook-milestone-3-evidence.md` §8a drains
into; and `ServerDirectory`'s stale-removal path logs the backend it drops. Separately: `cmd/spawnery-join` asks a
server for its protocol version by announcing an unsupported one
(`announceUnsupported = -1`) and trusts that the proxy's newest supported
version and the backend's actual version agree — true of every pinned pair
this repository ships and not guaranteed generally; `internal/mcjoin`'s own
package comment names the failure mode (a loud "Outdated client!" naming the
version to fix it to), so it fails loud rather than silent, but the runbook
that depends on this tool inherits the same assumption.

**A backend that goes silent without closing its socket still disconnects its
players, and no plugin can stop it.** `Rescue` (added 2026-08-25) catches a
player whose server drops them and redirects them onto `fallbackGroups`, which
closes the gap the design's §6.2 assumed was already closed — measured that
day on `paulwtf` against a two-backend `lobby` group, where force-deleting the
pod under a joined player disconnected them while a ready, registered, empty
peer stood unused beside it. Re-run the same way against `0.2.1` the same
day, the player was moved instead: the proxy logged the kick from
`lobby-97eq` and `-> lobby-gzvz has connected` in the same second, and the
client stayed until its own hold expired. It does not close every case. Disassembling
velocity 3.5.1 build 615, `ConnectedPlayer.handleConnectionException(server,
reason, friendly, safe)` returns *before* firing `KickedFromServerEvent` when
`safe` is false, and `BackendPlaySessionHandler.exception(cause)` passes
`safe = !(cause instanceof ReadTimeoutException)`. So a hard-powered-off node
or a partitioned network — where the backend stops answering but never closes
the connection — surfaces as a read timeout, and Velocity disconnects the
player before any plugin is consulted. The hole is Velocity's own and cannot
be closed from the agent; closing it would mean the operator noticing the
dead server and sending `DrainPlayers` inside Velocity's read timeout, which
is a different mechanism than this one.

## From the milestone 3c evidence run (2026-08-12)

`docs/runbook-milestone-3-evidence.md` was finally run against a real `kind`
cluster. Criterion 7 (a player can join, automated) is now proven — see
`docs/handover-milestone-4.md`. Criterion 9 (deleting a `Server` moves its
player rather than disconnecting them) was not, and the reason why is the most
important finding of this run.

**Read the rest of this entry as a finding about `spawnery-join`, not as an
open criterion.** Criterion 9 was proven manually the next day, 2026-08-13,
with a real client on a live account — recorded in
`docs/handover-milestone-4.md` under "The manual session". Of the two
findings below, the first is closed for the milestone's purposes and stays
here because it still describes the tool's limits; the second is open and
belongs to milestone 4.

**Deleting a `Server` with a `spawnery-join --hold` player on it disconnected
the player instead of moving them.** The diagnosis is measured end to end,
and the defect sits in the evidence tool's fit for this criterion, not in the
drain logic itself:

- `--hold` stops the join one packet after `Login Acknowledged`, in the
  configuration state — `internal/mcjoin`'s own package comment documents
  this as deliberate, because that is as far as Velocity itself needs before
  it dials a backend.
- Paper's `getOnlinePlayers()` never contains such a client, because it never
  finishes the configuration phase Paper is waiting for. So the Paper agent
  reports zero players, and `Server.status.players` reads zero for a
  connection the proxy is actively holding open.
- The drain's own exit condition is inside the `Draining` case:
  `if !in.Occupied() { ... Reason: ReasonDrained, Message: "no players left"
  ... }` (`internal/phase/phase.go:247` and `:282` as of 2026-08-22).
  `Occupied()` (`:166`) is `in.PlayersStale || in.PlayersOnline > 0` — and
  with a stale-held client Paper never counted, `PlayersOnline` is exactly
  zero.
- Measured directly, in one `kubectl get` against the running cluster:
  `proxygroup/gateway-auto` showed `PLAYERS 1` in the same instant
  `server/lobby-bsvg` showed `PLAYERS 0`. Same player, same second, two
  different counts, and the drain reads the one that says nobody is there.
- The Kubernetes events, all in the same second, show the operator acting on
  the wrong count rather than hanging or erroring:

  ```
  DeletionRequested  server/lobby-5wv2  phase Ready -> Draining: deletion requested, moving players off
  Killing            pod/lobby-5wv2     Stopping container minecraft
  PodDeleted         server/lobby-5wv2  deleted pod lobby-5wv2: no players left
  Drained            server/lobby-5wv2  phase Draining -> Terminating: no players left
  ```

- Velocity then reported `disconnected while connecting to lobby-5wv2: An
  internal server connection error occurred.` — the player lost the
  connection Velocity itself was still holding, because the pod it was about
  to be moved off was already gone.

So: the operator concluded the server was empty and deleted the pod out from
under a player the proxy was still counting.

**This is two separable findings, and they should stay separate:**

1. **Criterion 9 is not provable with `spawnery-join` as it stands.** Closing
   this needs the client to play the configuration phase through to the point
   Paper starts counting it. The whole-branch review scoped that as "two
   packet-id constants and one `case` in `holdOpen`, not a rewrite".

   *Measured on the wire 2026-08-25, against a local Paper 26.2 (protocol
   776), and the scoping was wrong in the way that matters: the client has to
   **drive**, not react.* After Login Acknowledged the server sends Plugin
   Message `0x01` (`minecraft:brand`, "Paper"), Feature Flags `0x0c`
   (`minecraft:vanilla`) and Select Known Packs `0x0e` (`minecraft:core
   26.2`) — and then nothing but Keep Alive `0x04`, for as long as the client
   waits. It never sends Finish Configuration unprompted, so a `case` that
   answers one packet would wait for a packet that never comes.

   What actually moves it, each step confirmed by the server's own next move
   rather than by a remembered constant:

   - serverbound **Known Packs `0x07`** with an empty list — the server
     answers with 29 Registry Data `0x07` packets and Update Tags `0x0d`,
     35 KB of them, rather than assuming the client already has the registry;
   - clientbound **Finish Configuration `0x03`**, empty payload, once that is
     done;
   - serverbound **Acknowledge Finish Configuration `0x03`**, empty — after
     which the server logs `probeplayer joined the game` and counts the
     player, which is the whole point;
   - and the hold then sits in the play state, where Keep Alive is `0x2c`
     carrying a millisecond timestamp, not the configuration state's `0x04`.

   So it is four constants and a small state machine that leads the exchange,
   not two constants and a `case`. Until that lands, criterion 9 can only be
   proven manually, with a real client — see
   `docs/runbook-milestone-3-evidence.md` §10 for that session — or not at
   all.
2. **A narrower product finding, and this half belongs to milestone 4, not
   to the evidence tool.** A player who is connected at the proxy but not yet
   counted by the backend sits outside the drain's protection: `Occupied()`
   only sees what the backend has reported, and the proxy's own count is not
   consulted at all. In production this window is real but small — a real
   client completes the configuration phase within the same round trip, well
   under the second `--hold` freezes it at deliberately to make the gap
   visible. Small is not the same as absent, and milestone 4 owns drain, so
   this is the milestone to decide whether `Occupied()` should ever
   incorporate what the proxy side reports.

   *Milestone 4 finished without deciding it.* Checked on 2026-08-22:
   `Occupied()` is still `in.PlayersStale || in.PlayersOnline > 0`, and
   nothing in 4a through 4d consults the proxy side. The window is therefore
   exactly as open as this entry described it, and it no longer has a
   milestone assigned — whoever next touches drain inherits the question
   rather than finding it already owned.

**None of this branch's many reviews caught this**, and it is worth saying
plainly why not, rather than filing it away as bad luck. The whole-branch
review correctly predicted that a held connection would be *counted* — on
the proxy side, in `status.connectedPlayers`, which is exactly right and is
what §6 of the runbook now proves. Nobody in any of those reviews asked the
complementary question: which side does the *drain's own* exit condition
read? The two counts live in different structs, are populated by different
agents, and were never checked against each other until an
actual `kubectl delete` on an actual held connection forced the question.

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

**`derivePhase` measures readiness against `DesiredReplicas()`, which is only
the floor — and that is now a decision rather than a leftover.** Before 4a,
`DesiredReplicas()` in `api/v1alpha1/servergroup_types.go` was the size the
group ran at, so "ready replicas have reached it" meant the group was fully up.
Since 4a it is the group's floor: `DecideSize` can and does run an ephemeral
group above it to cover `spareSlots`. `derivePhase` never changed its
comparison, so a group scaled to five for spare slots with one server up and
four still starting publishes `status.phase: Ready` off that one. 4b's rolling
update, which needs to say "the new generation is up" as something other than
"one server somewhere is," found its own answer rather than changing this.

*Ruled 2026-08-24: the phase keeps its meaning and the missing question got a
field of its own.* `Ready` there means the group is serving, which is true as
soon as one server is up and is the useful thing for a printed column to say;
redefining it would have traded a true statement for a different true statement
and made the column flicker on every scale-up. `ConditionProgressing`
(`reportProgressing`, `internal/controller/servergroup_controller.go`) carries
the other half: true while a server of the current generation is still coming
up, or while one of an earlier generation is still there. That is the split a
Deployment makes between `Available` and `Progressing`, with one difference
taken on purpose — `True` here means "has not arrived" and nothing else, so a
group that has given up stays `True` and `Degraded`/`GaveUp` beside it says it
has stopped trying.

**One field further along, `GroupTotals.ReadyReplicas` counts servers of every
generation — and that is a decision too, taken the same day after it was first
mistaken for an oversight.** `AggregateGroup`
(`internal/controller/candidates.go`) filters `FreeSlots` on the current
generation, with a comment saying why, and does not filter `ReadyReplicas` two
lines above it. The asymmetry is the point: `replicas`, `readyReplicas` and
`onlinePlayers` are the "what is there" trio and answer how much of the group
is serving right now, which is what a printed column should answer — and during
a changeover the servers being replaced *are* still serving. `freeSlots` is the
odd one because it is the scaler's own input that happens to be published, and
the scaler is asking a different question. What the trio cannot say during a
changeover, `ConditionProgressing` now says beside it, rather than one number
being made to mean two things.

## From milestone 4b

**Any spec change begins a changeover.** `metadata.generation` moves on every
edit, so tuning `minReplicas`, `spareSlots` or `maxReplicas` marks every
running server stale and replaces a whole group of functionally identical
servers. The master design's §4.4 specifies exactly this — "when the group
spec changes, its generation goes up" — and 4b implements it as written. The
behaviour was latent before: `AggregateGroup` already filtered
`status.freeSlots` by generation, but nothing acted on it, so a generation
bump changed a published number and nothing else. It is safe — nobody is
kicked, and `maxUnavailable` and the cold start govern the changeover exactly
as they would for a real image bump — but it costs churn, and the likeliest
moment anyone changes a scaling knob is a player spike, which is the worst
time to be replacing servers one at a time. Narrowing staleness to the fields
that actually shape a pod is a real design change with its own pitfalls —
which fields count, and what happens to a server whose pod-affecting fields
never moved but whose scaling knobs did — and was deliberately not made
mid-milestone. `TestGroupShrinksOnceTheStabilizationWindowElapses`
(`internal/controller/servergroup_controller_test.go`) documents the
behaviour rather than hiding it: dropping `minReplicas` from 3 to 1 still
produces a fourth server, of the new generation, before it produces a shrink,
because the spec edit that lowered the floor is the same edit that staled the
three servers already running.

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

## From milestone 4d

**Which message an operator sees first when a group has both given up and a
dead `Network` is unpinned.** The `BackingOff`/`Degraded` switch in
`ServerGroupReconciler.Reconcile` tests `!sized` before `backoff.GaveUp`, so a
group whose `Network` died while it was also six failures deep gets "backoff
is not being decided: the group's network is not usable" rather than "not
retrying: change the group's spec to try again" — even though the failure
count that produced `GaveUp` is computed from the views before `sized` is
known, and does not depend on the `Network` at all.

**Ruled and pinned 2026-08-24: `GaveUp` goes first.**

Both messages are true, which is why nothing distinguished them. They are not
equally useful. The Network's unusability is transient and *already* has a
condition of its own — `Accepted: False` — which the `!sized` case's own
comment observes; repeating it on `BackingOff` and `Degraded` spends the only
two conditions that can carry the give-up on a fact reported elsewhere anyway.
And giving up is terminal: it needs a spec edit, so an operator who reads only
"the group's network is not usable", fixes the Network and walks away has been
told the truth and left with a group that still creates nothing.

`TestAGroupThatGaveUpSaysSoEvenWhileItsNetworkIsDead` pins it, and restoring
the old order fails it. The test also asserts `Accepted` is still `False`, so
the ruling cannot quietly turn into hiding the Network rather than declining to
repeat it.

**Writing that test surfaced something the entry did not know, and it is worth
more than the ruling.** The obvious construction — point the group's
`networkRef` at something missing — cannot reach this state at all, because
editing the group is a spec change and a spec change deliberately clears the
failure streak ("the operator's answer to whatever broke",
`servergroup_controller.go`). So the scenario is only reachable when the
`Network` becomes unusable *without* the group being touched: the object
deleted, or made unaccepted by a rival. Any future test about a group that has
given up while its `Network` is broken has to break the `Network`, never the
group.

## From milestone 4c-1 (the proxy readiness contract)

**A new operator against proxy images that predate `SetReady` empties nobody
and disconnects everybody five minutes later.** What an operator sees first is
that nothing happens: `spec.replicas` goes from 2 to 1 and the surplus pod
stays `Ready`, stays in the `Service`'s endpoint slice, and goes on receiving
*new* players for the whole of the drain window. Then, at the deadline, the pod
is deleted with all of them on it, and one event is the only thing that says
so:

```
Warning  ProxyDrainTimeout  proxygroup/gateway  deleting proxy gateway-xxxx after 5m0s with 3 player(s) still connected
```

That is worse than the immediate deletion 4c-1 replaced, which disconnected the
same people without first routing more of them onto a pod it was about to
remove.

The tell that separates this from a proxy that is genuinely still occupied is
on the pod. The operator writes `spawnery.cloud/draining-since` whether or not
the agent ever hears the message, so an un-upgraded proxy carries the
annotation while its `Ready` condition is still `True` and
`kubectl get endpointslice -l kubernetes.io/service-name=<group>` still shows
its address as ready. A correctly drained proxy carries the annotation and is
`NotReady`. Annotation plus `Ready` is the signature.

The cause is one line of protobuf: `SetReady` is field 7 of
`OperatorToProxy`'s oneof (`proto/spawnery/agent/v1alpha1/agent.proto`), added
by this milestone. An older agent does what protobuf requires of an unknown
field and ignores it, so `ReadyGate.close()` is never reached, the kubelet's
probe keeps succeeding, and the endpoint never goes away. The deadline bounds
the damage and the event names it; nothing prevents it.

**So: upgrade the proxy images before the operator, until something
version-gates the message.** There is no condition and no event that tells
"old agent" apart from "busy proxy", so nothing an operator can see
distinguishes them, and the other order is the safe one.

> **Corrected 2026-08-22, and the correction matters more than the entry.**
> This paragraph used to say the operator *cannot* detect the case, because
> "`Hello` carries no version". That is false, and was when it was written:
> `Hello` has carried `string version = 1` since the original gRPC contract on
> 2026-08-08, the agent fills it from its plugin metadata, and the operator
> already reads it — `internal/agentserver/server.go:454` logs `"proxy
> connected", "version", m.Hello.GetVersion()` at V(1). So the wire carries
> exactly what a version gate would need and nothing has to be added to the
> protocol; what is missing is only that nobody acts on it. The advice above
> stands, because today nothing does act on it. The task it defers to is much
> smaller than this entry implied.

Rolling the operator back on its own is safe too: an agent that supports
`SetReady` and never receives one behaves exactly as 3c's did, because
`ProxyRole`'s latch starts at
`asserted = null` and its first `FullSync` opens the gate unless a `false` was
asserted — `ProxyRole.kt`'s `Latch(synced = false, asserted = null)` and, in
its `FULL_SYNC` branch, `if (!previous.synced && previous.asserted != false)
onFirstSync()`. Quoted rather than cited by line: this file's citations into
that one have gone stale twice already.

**`status.connectedPlayers` briefly meant something other than what it says,
because this milestone changed the field's meaning without editing the line
that computes it — found and fixed inside the same milestone.** `setStatus`
skipped any pod that was not `Ready` before adding its player count, so after
a scale-down the group printed `READY 1 PLAYERS 0` with a person visibly in
the game. The evidence run of 2026-08-14 saw exactly that.

The line did not need editing to become wrong. Before 4c-1, `NotReady` meant
starting up or broken, and skipping those pods was a fair reading of "players
in this group". 4c-1 makes `NotReady` also a deliberate, healthy, populated
state — a proxy serving people precisely because it is being emptied — so the
same code answered a different question than it used to, while the CRD's own
description still promised "the sum of players across all proxies".

The whole-branch review ruled it a defect rather than a naming question, and
the sum now runs above the readiness guard while `ready++` stays inside it.
Four things decided it: the CRD description was false in a state the operator
deliberately creates, and it is a printed column; it was wrong during the one
operation where it is the only observable, since nothing logs a readiness
withdrawal; the milestone had shipped a runbook paragraph apologising for it,
which is evidence the output was wrong rather than the reader; and no test
anywhere asserted the field, so the fix broke nothing — which also means
nothing would have caught it.

**Kept because the trap is general, not because the bug survives.** A guard
can go on compiling, passing its tests and reading sensibly while the meaning
of the state it filters on moves underneath it. The 4a entry above records the
same shape on `derivePhase` and `DesiredReplicas()`, which stood open until
2026-08-24 and was then ruled rather than repaired. Both were found by reading a comment or description against what the
code had come to do — not by any test.

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

**One smaller thing**, carried out of this milestone's task reviews rather than
fixed in them. Three others stood here and are closed:

- Two assertions in `hack/agent-test.sh` are argued rather than demonstrated:
  the control probe on 25565 that follows the closed-gate assertion, which
  needs the container to die mid-phase to be shown failing, and the post-loop
  arm of phase 4's withdrawal guard, which needs `port_open` to answer while
  `set_ready_sent` is already non-zero — a sub-second window on a correct
  agent.

**Three facts about the machine this milestone was built on — two of them have
since flipped and the third was never measured at all, which is the point.** Measured 2026-08-14: `docker` was a symlink
into `podman-docker-compat`, so `CONTAINER ?= docker` already ran Podman and
`CONTAINER=podman` changed nothing; and `/tmp` was not a tmpfs, so the
`TMPDIR` prerequisite did not apply.

Re-measured 2026-08-22, and both are now the opposite. There is no `docker` in
the PATH at all, so `CONTAINER=podman` is required rather than inert; and
`/tmp` is a 3.9 G tmpfs, so `TMPDIR` on a disk-backed path is required for
anything that extracts an image archive. Both overrides are what this
repository's own image and agent runs use today.

The entry was right to date these and right to say they are not properties of
the repository. What it shows is that dating is necessary and not sufficient:
nothing re-reads a dated fact, and a stale one about the machine is acted on
rather than merely read. `docs/runbook-milestone-3-evidence.md` §0 states both
conditionally, which is why nothing downstream broke while this paragraph was
wrong.

**The third was worse than stale: it was never measured.** "This machine has
8 GB, so `make e2e` does not run here" stood as a working rule for weeks and
was acted on — it is why several entries in this file said "unproven until
driven" rather than saying what a run had found. Measured 2026-08-25 on the
same machine, 7.9 GiB total with no swap: a full `make e2e` peaks at **962 MiB
over baseline** with a warm nix store, and 1.1 GiB on a run whose scenarios
fail. A bare single-node `kind` cluster is 675 MiB of that; `nix build
.#operator-image` costs about 1 GiB on a cold store and does not overlap the
cluster, since `hack/e2e.sh` builds at line 95 and creates at line 112. So the
peak is the larger of the two, not their sum.

The reason it is so small is a decision this file already records: no image in
the run resolves, so no Paper or Velocity process ever starts. What runs is a
Kubernetes control plane and one operator pod with a 256 MiB limit. An
estimate that assumed game servers would have been several times too high —
which is what the rule was, and nothing had checked it.

**An unchecked question about milestone 3's runbook, which must not be repeated
as a finding.** Measured 2026-08-14 against Go 1.26.5 in this repository's
devshell: `go run` does not forward `SIGTERM` to the binary it compiled, so
`pkill -f` on the wrapper leaves the compiled child running and reparented, and
the operator goes on reconciling.
`docs/runbook-milestone-4c1-evidence.md` says so and kills the child.
`docs/runbook-milestone-3-evidence.md` cleans up the same `go run` with
`kill %1`. Whether that has the same problem was recorded here as **not
known**, on the reasoning that `kill %n` addresses the *job*, which in some
shells means the job's whole process group, and signalling the group would take
the compiled child down along with the wrapper. Two possibilities, one of them
benign.

*Measured 2026-08-25 in bash, and it is the other one.* `go run` in the
background, `kill %1`, three seconds, then `kill -0` on both pids: the wrapper
is gone and the compiled child is alive, reparented and still running. Bash's
`kill %1` resolves the job to one pid and signals that process; `kill -- -PGID`
is what signals a group, and no runbook writes that. So milestone 3's cleanup
leaves the operator reconciling exactly the way `pkill -f` does, and both
runbooks need the second step the 4c-1 one already has: kill the compiled child
by name.

The reasoning that made this "not known" was sound and the possibility it
leaned towards was the wrong one — the third time in this file that evidence
consistent with a conclusion turned out not to establish it. The measurement
cost thirty seconds.

## From milestone 4c-2 (proxy rolling updates)

**Upgrading the operator can roll every proxy in the cluster, with nobody
having edited a spec.** This is the accepted cost of the decision
`docs/superpowers/specs/2026-08-14-proxy-rolling-updates-design.md` §3.1 made:
a proxy pod is stale when its `spawnery.cloud/pod-hash` label differs
from a digest of the pod the operator *would* render for its group right now
(`podspec.DesiredProxyHash`), and that digest is taken over the rendered pod
rather than over a chosen list of spec fields. So a change to the **rendering
code** — a new default in `internal/podspec`, an added environment variable, a
renamed label — moves the digest for every `ProxyGroup` in the cluster while
every spec stays byte for byte what it was. The next operator upgrade then
finds the whole fleet stale and rolls it.

**What that looks like from outside.** Every group starts replacing its proxies
within a reconcile of the new operator coming up, each group one pod at a time,
all groups at once — nothing serialises a roll across groups. The group's own
status is the wrong place to look for it: the surge pod comes up before any old
pod is withdrawn, so `readyReplicas` holds at `replicas` and the phase reads
`Ready` throughout, exactly as it does when nothing is happening. What says so
is the pod label:

```bash
kubectl get pods -n <ns> -l spawnery.cloud/role=proxy -L spawnery.cloud/pod-hash
```

`-L` prints the label's value as a column of its own. Two distinct values
inside one group means that group is mid-roll; one value everywhere means it is
done or never started. Each replaced pod then runs 4c-1's drain unchanged —
the players on it keep playing and are
disconnected only if they are still there when `spec.drain.timeoutSeconds`
elapses (300 seconds by default), with the same one `Warning ProxyDrainTimeout`
event per pod naming what it cost. A busy fleet upgraded at peak therefore
disconnects, per group, whoever is still on each proxy at each deadline.

**What to do about it.** Two things, and the first is the one worth planning
around:

- **Find out in advance whether the digest moved**, because the label is
  computable before the upgrade rather than only observable during it. Run the
  new build against a scratch cluster over the same manifests and compare the
  `pod-hash` it stamps on a fresh pod with the value the running pods carry. It
  has to be the *same* manifests to mean anything: the digest covers the
  rendered pod, so the group's namespace and name, the `Network`'s name, and
  the operator's own `--operator-namespace` (see the next bullet) all reach it,
  and a comparison run under different ones tells you nothing about your fleet.

  There is a cheap *negative* filter to run first, and 2026-08-22's upgrade is
  what showed it is worth having. The operator went from v0.1.2 to v0.2.0 on a
  live cluster and nothing rolled: both proxies kept `pod-hash
  2dd6593373a4ffd2` and 46 hours of uptime. `git diff v0.1.2..v0.2.0 --
  internal/podspec/` explains why — one file moved, `netpol.go`, and only its
  comments; that file renders a NetworkPolicy, not a pod. So if nothing in the
  pod-render path changed between two builds, the digest cannot have moved and
  no scratch cluster is needed. Note what it does not cover: the digest also
  takes the group's namespace and name, the `Network`'s name and the agent
  endpoint, so a clean diff rules out the code trigger only, never the
  namespace one in the entry below.

  *The code trigger now answers for itself, since 2026-08-23.*
  `internal/podspec/hash_golden_test.go` pins `DesiredProxyHash` and
  `DesiredServerHash` over frozen fixtures, so a change to either render path
  fails on the pull request that makes it rather than waiting to be noticed
  during a release — and it cannot be fooled the way the diff above can,
  because it does not guess which files belong to the render path, it runs the
  render. The manual diff is still the cheaper first look when comparing two
  builds after the fact; the golden tests are what stops a change reaching a
  build unremarked. Both remain silent about the two triggers that live outside
  the code: the group's own names, and the agent endpoint in the entry below.

  A failure there is not a defect, and the test says so. Rolling the fleet is
  sometimes what a change is for. What it must not be is a discovery made by
  players — so the test asks for the constant to be updated *and* for the
  commit to say it, because the release notes need the sentence more than the
  code does.
- **The group now says it is happening, since 2026-08-23.** Everything above
  is about finding out *before*; this is about the objects saying so *while*.
  `ConditionChangingOver` is True on a `ProxyGroup` holding pods whose rendered
  shape the operator no longer produces, and its message carries the count and
  the sentence that distinguishes the two causes: **if every group in the
  cluster says it at once, an operator upgrade changed the render rather than
  anyone editing a spec.** Until it existed the only outward sign was pods
  churning and `readyReplicas` dipping, which is what a dozen unrelated faults
  look like.

  It reports the hash half of staleness and not the node-draining half, which
  `ConditionNodeDraining` already has. Folding them together would lose exactly
  the distinction a reader needs most — one is local and expected, the other
  arrived with a release — and a test drives the merged version and fails.

  No event accompanies it, for the reason `reportNodeDraining` gives about its
  own condition: an event tied to a transition under-reports, because a group
  already True that has more pods fall stale produces no second transition to
  fire on. The condition carries the count, so it says the second thing where
  an event could not.

- **`spec.drain.timeoutSeconds` is the knob that bounds the damage**, and it is
  read from the current spec on every pass, so raising it on a group already
  rolling extends the drains still in flight. Raising it does not prevent a
  disconnection, it buys the people on each proxy more time to leave on their
  own; lowering it during a roll expires the drain in flight and disconnects
  them sooner.

**A second trigger for the same hazard, which nobody would guess from the
spec.** The `agentEndpoint` handed to the renderer feeds the digest, and it is
derived from the operator's own namespace —
`spawnery-operator.<operator-namespace>.svc:9443`, from `agentEndpoint()` in
`cmd/spawnery-operator/main.go`. Moving the operator to a different namespace,
or restarting it with a different `--operator-namespace` (or `POD_NAMESPACE`),
therefore moves the digest for every group in the cluster and rolls the whole
fleet, with no image, no rendering change and no spec edit involved. Milestone
6's Helm chart is the first thing that makes that a routine operation.

**Why 4c-2 took this trade rather than 4b's.** The entry under "From milestone
4b" above records the other side of it: `metadata.generation` moves on every
edit, so for a `ServerGroup` "any spec change begins a changeover" and tuning a
scaling knob replaces a whole group of functionally identical servers. For a
`ProxyGroup` that rule would be worse, because changing `replicas` is the
routine operation on a proxy group and a generation rule would make every
scale-up and every scale-down a full replacement — every pod waiting out an
attrition-bound drain, and the deadline disconnecting whoever is left. The two
milestones bought opposite things: 4b's rule rolls on edits that change no pod,
4c-2's rolls on changes nobody edited. Scaling a proxy group stays scaling, and
the price is that upgrading the operator is not free.

**Which edits roll a group, and which do not, is decided by what reaches the
pod — and that is not the shape of the CRD.** The digest covers
`podspec.BuildProxyPod`'s output, so an edit rolls the group exactly when it
changes that pod. Read off `internal/podspec/proxy.go` as it stands: `image`,
`resources`, `scheduling`, `config.playerLimit` (it is
`SPAWNERY_PLAYER_LIMIT`), `routing.fallbackGroups` (`SPAWNERY_FALLBACK_GROUPS`),
`configOverlay` (the ConfigMap's *name*, in a volume) and `drain.timeoutSeconds`
all roll it, as do the `Network` fields the proxy pod inherits —
`defaults.resources`, `defaults.scheduling`, `defaults.imagePullSecrets` and
`forwardingSecretRef.name`. `replicas` does not, which is the whole point of that
design's §3.1. Two others do not either, and those are the ones worth knowing
in advance:

- **`spec.config.motd` and `spec.config.onlineMode` do not roll a group, so
  editing them changes nothing about a proxy that is already running.** Both
  are rendered into the group's ConfigMap by `proxyConfigValues`, and the pod
  references that ConfigMap by name; `spawnery-config` reads it at container
  start and writes `velocity.toml` from it. So the new value applies to the
  *next* proxy pod and to no existing one. For `motd` that is cosmetic. For
  `onlineMode` it is not: turning it off, or back on, decides whether the proxy
  authenticates players at all, and the change takes effect on a pod's next
  restart rather than on the edit. Nothing on the CR says so — the group's
  `status.observedGeneration` advances and the phase stays `Ready` — so every
  signal the API offers says the change is applied while the proxies go on
  authenticating players the old way. That is exactly the state 4c-2 was built
  to end, now narrowed to the fields that land in the ConfigMap instead of
  covering the whole spec. The same holds for the *contents* of a user's own
  `configOverlay` ConfigMap and of the forwarding secret: the pod names them,
  so editing what is inside them rolls nothing.

  **After changing `onlineMode`, delete the group's proxy pods, or edit a
  field that does roll — `spec.config.playerLimit` is the cheapest.** Nothing
  on the CR will tell you it has not been applied, and doing nothing is worse
  than it looks: a pod that restarts later for an unrelated reason picks the
  new value up on its own, so a group left alone drifts into running both
  settings at once, with which proxy authenticates depending on which happened
  to restart. That drift is not random, and knowing where it starts is the
  point: a crashlooping proxy is by definition a pod that restarts, so it takes
  the new value first while any sibling that stays up keeps the old — the
  broken proxy is the one that diverges, and it diverges soonest. The mechanism,
  if you need to confirm it on a cluster: the group ConfigMap reaches the pod
  as a projected volume with no `subPath`, so the kubelet updates the file in
  place, and `image/velocity-entrypoint.sh` re-runs `spawnery-config` on every
  container start — including an in-place restart under `RestartPolicy:
  Always` — while the pod carries a readiness probe and no liveness probe, so a
  restart means the process exited rather than a probe having killed it.

  Note where the boundary actually falls, because it is not where the CRD
  suggests: `spec.config` has three fields and two behaviours. `playerLimit`
  rolls the group, because it reaches the pod as `SPAWNERY_PLAYER_LIMIT`.
  `motd` and `onlineMode` are siblings of it under the same stanza and do not,
  because they reach only the ConfigMap. Nothing about the stanza distinguishes
  them; only `internal/podspec/proxy.go` does.
- **`spec.drain.timeoutSeconds` does roll the group**, because it reaches the
  pod as `terminationGracePeriodSeconds`. Tuning a drain timeout is something
  an operator does in the middle of an incident, and under this rule it also
  replaces every proxy in the group. **Expect the edit itself to add a surge
  pod and a full replacement cycle on top of whatever incident prompted it**;
  that is the operationally relevant part and it applies whether or not a drain
  is under way. Raising it while a drain is already in flight does otherwise
  behave — the marked pod keeps its mark, since it is now stale as well as
  draining, and the deadline it is measured against is read from the current
  spec on every pass.
  `docs/runbook-milestone-4c1-evidence.md` §9 recommends exactly this edit for
  a drain you want to give more room to; after 4c-2 it is no longer free.

Neither of these is a defect in the digest — it covers what it says it covers —
and both are arguments for a later milestone to hash the *rendered
configuration* alongside the rendered pod, rather than for hand-picking fields,
which is the trade that design already ruled on.

**`ProxyGroupReconciler.pods()` is read once per `Reconcile` and is not
refreshed by the creates that follow it.** `Reconcile` lists the pods, hands
that slice to `reconcileReplicas`, and that function may create pods below —
but the slice it is iterating is the one from before its own creates, so a pod
created on this pass is first seen by the per-pod logic (the readiness
assertion, the divergence check riding along with it, the deletion loop) on the
pass *after* this one. The call site says so, and this entry exists because the
property has caught test construction twice in this milestone: a test that
creates a pod and asserts something about it within the same reconcile is
asserting against a snapshot that predates it. Note that the status at the end
of `Reconcile` does not have this property — it re-lists deliberately, so the
published counts describe what is there rather than what was there when the
pass began.

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

**The ServerGroup's PodDisruptionBudget was renamed this milestone, and
upgrading an already-running cluster strands the old object.** Before 4c-3,
`reconcilePDB` named the budget after the bare group name; this milestone's
own review caught that a `ProxyGroup` sharing that name would collide with it
— exactly the incident `podspec.GroupConfigMapName`'s doc comment already
narrates for the ConfigMap, reproduced one object type over — so both group
kinds' budgets now go through `podspec.GroupPDBName(group, role)`, which
appends `-server-pdb` or `-proxy-pdb`. `reconcilePDB` and
`reconcileProxyPDB` only ever `CreateOrUpdate` the new name; nothing renames
or deletes the old one. A `ServerGroup` reconciled under pre-4c-3 code
therefore leaves a `PodDisruptionBudget` sitting at its own bare name, with
`minAvailable` frozen at whatever the last old-code reconcile wrote — and
nothing updates it again, because the reconciler has moved on to writing the
new-named object exclusively. **Delete it promptly.** The frozen count is the
smaller of the two problems it causes: the stranded object also carries the
pre-4c-3 *selector*
(`spawnery.cloud/managed-by`, `spawnery.cloud/group`,
`spawnery.cloud/occupied`), which has no `spawnery.cloud/role` term, and this
milestone is the one that put `spawnery.cloud/occupied` on proxy pods as well
as server pods. So in a namespace holding a `ProxyGroup` of the same name,
that selector matches the occupied *proxies* too, while its `minAvailable`
was only ever counted from occupied *servers*. `currentHealthy` counts the
ready pods among everything the selector matches, proxies included;
`desiredHealthy` is the frozen server-only figure; `disruptionsAllowed` is
the difference, and the ready occupied proxies push it up. The eviction API
can then spend those disruptions on occupied server pods — disconnecting the
players on them. That is the exact
defect this milestone's own final review found in the live `reconcilePDB`
selector and fixed there by adding the role term; the stranded object is a
frozen copy of the broken selector that no fix can reach. To find it:
`kubectl get pdb -n <namespace>` and look for one named exactly the
`ServerGroup`'s own name, rather than `<group>-server-pdb` — `kubectl get pdb
<name> -n <namespace> -o
jsonpath='{.metadata.ownerReferences[0].name}'` confirms it is owned by that
group. `kubectl delete pdb <name> -n <namespace>` removes it; the group's
protection continues uninterrupted through the new-named object, which
`reconcilePDB` has been maintaining all along.

*Checked against the only installation there is, 2026-08-22.* `kubectl get pdb
-n minecraft` returns `gateway-proxy-pdb` and `lobby-server-pdb` and nothing
at a bare group name: that cluster was installed at v0.1.0 on 2026-08-20,
milestones after this rename, so it never wrote the old object. Like the
`GroupConfigMapName` rename recorded under milestones 3a and 3b, this hazard
now describes a cluster nobody has — one installed before 4c-3 and upgraded
across it. Keep it for whoever finds such an installation; the selector
analysis above is what makes it worth finding, and it is not a pending task
for anyone here.

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

**A proxy the registry has not yet heard from is not evictable for up to 15
seconds after the operator starts — and that window can matter well past the
fifteenth second.** `proxyOccupiedForBudget`
(`internal/controller/proxygroup_controller.go`) is what sizes a
`ProxyGroup`'s budget and writes the `spawnery.cloud/occupied` label, and it
treats a pod the registry has never heard of as occupied while
`Snapshot.StreamDownFor` is still under `phase.StreamDownGrace` — which, for
an unknown pod, `Registry.Lookup` reports as the time since the operator
process came up. The agent registry is in-process state and does not survive
a restart, so during that grace every proxy whose agent has not yet dialled
back in reads as unknown, gets the label, and is counted into its group's
`minAvailable`; where that covers the whole group, `currentHealthy` equals
`desiredHealthy` and the eviction API refuses everything. A `kubectl drain` running across an operator restart therefore
stalls for up to 15 seconds beyond whatever it was already waiting for. That
is the deliberate direction: the alternative reads a fleet of Ready proxies
full of players as empty, at exactly the moment a drain is retrying
evictions in a loop — and an operator evicted off the node being drained is
an ordinary way to arrive there. A proxy whose agent reconnects inside the
grace and reports, say, zero players on a live stream is judged on that
report immediately: it carries no label and is evictable well before the
fifteenth second.

The 15-second figure is a floor on the stall, not a ceiling on how late an
agent can still be un-dialled, and that gap is worth naming rather than
leaving as an implicit risk. The agents' own reconnect backoff
(`SessionLoop.backoffMillis`,
`agent/common/src/main/kotlin/cloud/spawnery/agent/SessionLoop.kt`) is
`min(30 s, 1 s << attempt)` with jitter and no give-up point, reset only when
the operator actually answers a stream. After an operator outage long enough
to push agents up against that 30-second cap, a meaningful share of the
fleet can still be un-dialled at second 15 — not because those agents ever
stop trying, but because they are mid-backoff — and every one of them drops
out of `minAvailable` at the same fifteenth second regardless of how much
longer its own reconnect is going to take. The grace does not distinguish a
proxy whose agent never arrives from one whose agent arrives at, say, second
22: both are treated identically until the moment either one actually
reports. That identical treatment is what keeps a `CrashLoopBackOff` proxy
from wedging its group's evictions permanently — at the cost of the same
30-second tail applying to every other proxy still reconnecting.

This is recorded as a decision, not left as an unweighed risk. A tighter
bound needs the distribution of how long agents actually take to reconnect
after a real outage; 15 seconds is a number chosen for being finite, not one
derived from the agents' own behaviour.

*Measured 2026-08-25 on `paulwtf`, and the shape matters more than the
numbers.* A throwaway `ProxyGroup`, its agent's stream cut with a
`NetworkPolicy` denying the proxy's egress, then restored — timed from
deleting the policy to the group's budget relaxing again:

| outage held for | budget relaxed after |
|---|---|
| 10 s | 12 s |
| 150 s | 85 s |

The agent's own reconnect delay is `1 s` doubling to a `30 s` cap with ±10%
jitter (`SessionLoop.backoffMillis`), so a short outage is answered almost at
once and a long one waits out whatever sleep it is in. Both figures include the
operator's five-second resync and whatever the CNI takes to drop the policy, so
they are upper bounds on the reconnect rather than the reconnect itself.

**85 s is more than the 30 s cap plus a resync accounts for, and the
decomposition was not isolated.** The likeliest explanation is in
`SessionLoop`'s own class comment: the channel has no keepalive and no idle
timeout, and the calls have no deadline, so a partitioned agent does not learn
its stream is dead when the packets stop — it learns when a send fails, which
for a Paper agent is its next player-count report. Its backoff clock would then
start well after the operator's did. That is reasoning, not measurement, and
whoever tightens this bound should isolate it first: the two are different
distributions, and the grace period is sized against the second. It
sits between two failures neither of which it is allowed to become: no grace
at all, and a grace that never ends. The alternative that was considered and
rejected was the first of those — qualifying occupancy on `Known` alone, with
no grace period — which would report every un-dialled proxy as unoccupied the
instant the operator starts, collapsing every group's budget to
`minAvailable: 0` at second zero and exposing a live, player-carrying fleet to
eviction at exactly the moment an operator restart is likeliest to coincide
with a drain; fifteen seconds of blanket protection is safer than none. The
second failure is the one this section's own reasoning names for the grace
existing at all: an unbounded grace would let a proxy whose agent never
arrives — `CrashLoopBackOff`, or simply gone — wedge its group's evictions
permanently, which is the exact failure this mechanism exists to end. Fifteen
seconds sits between those two without being derived from either, and the
30-second backoff cap is why it does not fully close the gap: a proxy that is
genuinely still reconnecting, not dead, can lose its protection at the
fifteenth second before it has had a chance to answer, and nothing after that
point tells its group's budget the difference from the pod that never will.
That residual is not eliminated by the choice of a bound, only left bounded
by one. A number derived from a measured reconnect distribution would narrow
it further; that is work for a later milestone.

**A node holding a whole group empties it at once**, so its players go to the
fallback groups rather than to the group's own replacements, which are not
ready yet. `DecideSize`'s `Condemn` rule names every server on a departing
node in the same pass, unconditionally and all at once — described in
`docs/superpowers/specs/2026-08-15-node-drain-design.md` §3.3, along with the
reason it is not throttled: draining one server at a time would make `kubectl
drain` wait out `drain.timeoutSeconds` once per occupied server on the node
rather than once for the whole node, turning ten occupied servers at the
default 60-second deadline into ten minutes of exactly the hanging this
milestone exists to end. So a `ServerGroup` whose every live server happens
to sit on one node — a small group, or an unlucky scheduling run — condemns
its entire population in one pass. The replacement servers are ordered in the
same pass, but they take a cold start to come up, and in the meantime every
player who was on that node has nowhere to land but a `fallbackGroups` entry.
This is the nature of losing the node those servers were on, not a choice
this design makes differently than it could have; an operator meeting it
should recognise it rather than read it as a fallback-routing defect.

**A `Persistent` server on a node-pinned RWO volume may not be schedulable
anywhere else.** `Condemn` names a server whose pod sits on a departing node
regardless of what kind of server it is or what volume it carries — the node
is leaving either way, and the alternative is leaving the server's players
to whatever eviction the node's own departure eventually forces. Its
replacement, once ordered, then sits `Pending` if the storage class backing
its `PersistentVolumeClaim` is bound to the node that is going away: a local
or node-pinned RWO volume does not follow the pod to a different node, and
nothing in this milestone — or in the storage class itself — can move it.
This is out of scope by the design's own §4 rather than an oversight
discovered afterwards, and it stays a limit of the storage class a `Persistent`
group is configured against, not something node drain can be taught to work
around.

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

**A persistent server on a node-pinned volume cannot follow a node drain.**
`docs/known-issues.md`'s own "From milestone 4c-3" section recorded this
before 5a existed to make it real: `Condemn` names a server whose pod sits on
a departing node regardless of what kind of server it is, and its replacement
then sits `Pending` if the storage class backing its claim is bound to the
node that is leaving — a local or node-pinned `ReadWriteOnce` volume does not
follow a pod to a different node, and nothing in this operator or in the
storage class itself can move it. That entry is not repeated here in full; 5a
is what turns it from a recorded limit into one an operator can actually run
into, because before 5a no persistent server existed to condemn.

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

**This is correct, not merely tolerated.** A persistent world lives on one
claim and nothing else can serve it, so a rebuild only ever meets the same
broken volume — sequentially, never concurrently, since the corpse's pod is
deleted before its replacement is created. Waiting for a human after six
attempts roughly an hour apart is the right call: at that point the thing
that is broken is the storage, not the server, and only a human can fix a
storage class, a quota, or a stuck `WaitForFirstConsumer` binding.

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

**A `Persistent` group that existed before this upgrade keeps a stale `Ready:
False` forever.** Before 5a the ServerGroup controller published
`Ready: False` with reason `NotImplementedInThisVersion` and the message
"persistent groups arrive in milestone 5" on every persistent group,
unconditionally. 5a removed that block, and it was the only thing that ever set
`ConditionReady` on a `ServerGroup` of either type — readiness is
`status.phase`, which `derivePhase` computes from `readyReplicas` against
`DesiredReplicas()`. Nothing removes the condition an older operator wrote, so
such a group carries `Ready: False / NotImplementedInThisVersion` beside pods
that are up and players who are online, while `status.phase` next to it reports
whatever those pods actually support — `Ready` among them. Nothing in the operator reads the condition, so nothing behaves
differently; it misleads a person, and any alert written on
`.status.conditions[?(@.type=="Ready")]` for a ServerGroup.

Clearing it is one command per affected group, and it is safe precisely because
nothing republishes it:

```bash
kubectl patch servergroup <name> -n <namespace> --subresource=status --type=json \
  -p '[{"op":"test","path":"/status/conditions/<index>/type","value":"Ready"},
       {"op":"remove","path":"/status/conditions/<index>"}]'
```

`<index>` is the position of the `Ready` entry in `.status.conditions`; the
`test` operation is there so the patch fails loudly rather than removing a
neighbouring condition if the index has moved between the read and the write.
Find it with:

```bash
kubectl get servergroup <name> -n <namespace> \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\n"}{end}' | grep -n Ready
```

*Checked against the only installation there is, 2026-08-22.* It runs one
`ServerGroup`, `minecraft/lobby`, of type `Ephemeral`, whose conditions are
`Accepted`, `NodeDraining`, `ScalingLimited`, `BackingOff` and `Degraded` —
no `Ready` among them — and it holds no `Persistent` group at all. Installed
at v0.1.0 on 2026-08-20, milestones after 5a, so no older operator ever wrote
the condition here. This is the third hazard in this file about upgrading
across a rename or a removal, beside `GroupConfigMapName` under 3a/3b and the
PodDisruptionBudget under 4c-3, and all three describe the same installation
nobody has: one that predates the change and was carried across it. Keep them
for whoever finds one; none is a task for anybody here.

`ReasonNotImplemented` (`api/v1alpha1/common_types.go`) has no user left in the
codebase after this milestone: grepping `.go` and `.yaml` finds the identifier
only at its own definition, and the string `NotImplementedInThisVersion` only
in a test comment describing the block that was removed and in
`docs/superpowers/plans/`, which is a historical record rather than live code.
The constant is kept rather than deleted: it costs a line, and it is the exact
string an operator meets on the stale condition above, so removing it would
make that string unsearchable in the repository it came from.

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

**`spec.replicas` is now required for `Persistent`.** A CEL rule —
`self.type != 'Persistent' || has(self.replicas)`
(`api/v1alpha1/servergroup_types.go`, `config/crd/bases/spawnery.cloud_servergroups.yaml`)
— rejects a `Persistent` group with no `spec.replicas` on its next apply. Before
this milestone such a group was accepted, sized nothing (`DecidePersistentSize`
did not exist, and nothing called it), and reported `Ready` while running zero
servers. In practice no such group can already be running: nothing in this
repository could create a persistent server before this milestone, so there is
nothing for the new rule to reject that was not already useless.

## From the milestone 5a evidence run (2026-08-16)

`docs/runbook-milestone-5a-evidence.md` was driven against a single-node `kind`
cluster on master at `f3c6fc1`, and its acceptance test passed: blocks placed
at -74 / -10, the pod deleted, the client rejoined, the blocks still there.
Every claim the milestone makes held. What follows is the one thing the run
found that the runbook could not have predicted from reading the code, because
it is about what the code *prints* rather than what it does. It is corrected in
no document but this one; the runbook points here.

**The recreate path logs `level=error` with a full stacktrace every time it
runs, on the happy path.** Deleting a persistent server's pod produces, in the
operator's own log, exactly one line of the shape:

```
{"level":"error","controller":"server","Server":{"name":"survival-0",...},
 "error":"servers.spawnery.cloud \"survival-0\" not found","stacktrace":"..."}
```

It is benign, and that is the problem. The mechanism, as far as the evidence
actually reaches:

- The recreate path deletes the `Server` object at
  `internal/controller/server_controller.go:389` and releases its finalizer at
  `:381`, after which the object is genuinely gone. (Line numbers re-anchored
  2026-08-22; they were `:348` and `:340` when this was written.)
- A reconcile that fetched the object before that point — the reconciler reads
  through the manager's informer cache, which can hand back an object the API
  server has already removed — then writes to it and gets `NotFound` back.
- Whichever write it was, the error escapes unwrapped and `Reconcile` returns
  it. controller-runtime logs it at `error` with a stacktrace and requeues; the
  requeued pass fetches the `Server` at `:144`, where
  `client.IgnoreNotFound(err)` treats the absence as ordinary, and returns
  cleanly. Nothing retries into a loop and no object ends up wrong — the
  2026-08-16 run's `Server` was recreated within a second of the delete and
  reached `Ready` 22 seconds later, with the error line in between.

**Which write produced it is not determined by the log, and this entry does not
guess.** **Four** writes on this path can land on a vanished object, and **none
of them tolerates `NotFound`**: the `Status().Update` after pod creation
(`:360`), the `Update` that releases the finalizer (`:381`), the
`Status().Update` that persists the registration intent (`:767`), and
`applyDecision`'s closing `Status().Update` (`:871`). The last is the
likeliest, being the write most reconciles of a live server reach — `:360`
returns early on the pass that creates a pod, so it is not on this path at all
unless a pod was just created — but the log carries no line number and nothing
here distinguishes them.

*It was three when this entry was written, and the fourth arrived by a route
worth noticing.* `:767` is milestone 3a's own fix: `WasRegistered` is
persisted with its own `Status().Update` before `Registrar.Register`, so a
crash between the two finds the intent durable. That change was correct and
carefully argued, and it added a write to this path without anybody connecting
it to this entry — which is how the count in a document goes stale while every
individual change is right. `ensureFinalizer`'s `Update` (`:563`) is a fifth
write in this file and is **not** on this path; it runs on the way in, not on
the way out. Anyone fixing this should reproduce it with a line-level log or a
breakpoint first rather than trusting this paragraph's ordering.

**The asymmetry worth noticing is between the deletes and the writes.** Both
`Delete` calls on this path carry an explicit `&& !apierrors.IsNotFound(err)`
— the pod delete at `:857` and the `Server` delete at `:389` — and none of
the four writes carries the equivalent. That reads as an oversight rather than a
decision; nothing in the surrounding comments argues for the difference.

**Why this is worth an entry rather than a shrug.**
`docs/runbook-milestone-5a-evidence.md` §4 tells the person driving it to leave
the operator log visible, because "a reconcile that is erroring — against the
CRDs, the claim, the Service — says so there and nowhere else." That
instruction is correct and this line defeats it: the single most important
transition in milestone 5a announces itself as an error, so an operator who has
learned to read past it will read past a real failure on the same path. The
cost is not a broken cluster, it is a log an operator stops trusting.

The narrow fix is to tolerate `NotFound` on the write that produces it, the way
`:671` and `:348` already do, since a write to an object that is deliberately
gone is not a failure. It belongs to 5b rather than to a documentation pass,
for two reasons beyond the identification above being unfinished.
`applyDecision`'s return is also the path a *genuine* lost status write takes —
`:319`'s own recovery, the one that produces `PodAdopted`, depends on such a
write failing loudly — so swallowing `NotFound` there narrows a real signal and
deserves a code review rather than a line slipped in beside a runbook edit. And
the test obligation is the interesting half: an envtest that deletes the
`Server` and asserts the reconcile returns no error would pass today against a
fixture whose cache is not stale, since it is the cache-staleness window that
produces the error at all. Reproducing it deterministically is the work, and
this milestone's own review already recorded what a test that cannot reach its
branch is worth.

**Twelve optimistic-concurrency conflicts are separate, expected, and not part
of this.** The same run logged twelve `the object has been modified; please
apply your changes to the latest version` errors across all three controllers,
one at essentially every state transition — the apply, first readiness, the
join, the leave, the delete, the replacement's readiness, the rejoin. These are
controller-runtime's ordinary retry path and they self-heal; one of them is
what produced the `PodAdopted` event the runbook's §8 now explains. They are
noted here only so that a reader counting error lines in an operator log does
not attribute all of them to the paragraph above.

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

**Widening the proxy hash rolls every proxy group once on upgrade.**
`DesiredProxyHash` (`internal/podspec/hash.go:57`) now digests the rendered
config values as well as the pod, closing the `motd` gap 5a's handover
recorded — but unlike the server-side adoption above, there is no
adopt-on-empty escape here: the label `LabelPodHash` is present on every
existing proxy pod already, merely computed under the narrower, pod-only hash,
and nothing can tell "the hash widened" apart from "the image really changed"
from the value alone. The first reconcile after upgrading therefore rolls
every `ProxyGroup` once, through the ordinary surge-1, one-at-a-time path.
Expected, not a defect — but worth knowing before it is read as one, and
before it is read as evidence that the `motd` fix itself is broken.

**`DesiredProxyHash` takes the agent endpoint and `DesiredServerHash` does
not.** `DesiredServerHash`'s own doc comment (`internal/podspec/hash.go:104`)
names the asymmetry directly: the endpoint comes from an operator flag, not
from any spec, so `DesiredServerHash` renders with a fixed sentinel address
regardless of the real flag value, while `DesiredProxyHash` still takes it as
a parameter and folds the real value in (`internal/podspec/hash.go:57`). An
operator restarted with a different `--operator-namespace` therefore rolls
every proxy group in the installation, while a persistent server group is
unaffected. This is a real asymmetry between the two sibling functions rather
than an oversight in one of them — a
rolled proxy loses no world, so the argument that forces the exclusion on the
server side does not reach the proxy side — but it is worth knowing before
someone reads the difference as a bug and "fixes" it into consistency.

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

**Completeness of the permission table.** The audit in `internal/rbacaudit`
catches drift between table and role. If a permission is missing from both, it
stays green — only the operator running under its ServiceAccount in a real
cluster proves that (level B of the E2E design).

*That run has happened, twice over, and the blind spot is now bounded rather
than open.* `make e2e` runs the operator as a Deployment under
`spawnery-operator` in a kind cluster, and a real installation has run under
the same ServiceAccount on RKE2 since 2026-08-20 — through a CA rotation,
among other things. What those two prove is exactly the paths they exercise:
a permission missing from both table and role on a path neither touches is
still green here and still absent there. So the entry is not closed; it is
narrowed to the code the driven runs do not reach.

**Milestone 2a's isolation promise does not cover availability.** The promise of
the agent channel reads: a compromised game server pod cannot harm any other.
For identity and confidentiality it holds — the token is audience-bound and not
accepted anywhere else, the `spawnery-server` ServiceAccount has no RoleBinding
anywhere, the pods run with `automountServiceAccountToken: false`, the private CA
key never leaves the operator secret, `ProxySession` does not exist, and the
identity comes exclusively from the token, never from what the agent claims about
itself. For availability it does not hold:

- `grpc.NewServer` in `internal/agentserver` sets neither
  `MaxConcurrentStreams` nor `ConnectionTimeout` nor a keepalive policy — the
  number of open streams and half-open connections is unbounded;
- there is no rate limit in front of `Authenticator.Authenticate`, so a sender
  needs no valid token to cause work;
- there is no NetworkPolicy in `config/`, so port 9443 is reachable from every
  pod in the cluster, not only from the managed ones;
- and every connection costs a `TokenReview` against the API server, with no
  cache for positive answers.

Together that means: a single pod in a connection loop generates load on the API
server and thereby hits the whole cluster, not only itself. It is triggered by a
compromised or simply faulty reconnecting agent — the failure case from the
Kotlin agent, already listed above under "reconnect with overlap", is the same
path without malice. The remedy belongs to this milestone's Helm chart: a
NetworkPolicy allowing ingress on 9443 only from pods carrying
`spawnery.cloud/managed-by`, plus an upper bound on concurrent streams. Whoever
quotes the promise from milestone 2a has to quote this point along with it — it
holds for identity and confidentiality, not for availability.

*Narrowed by milestone 6b, and narrowed is the right word — the entry stays,
with three of its four bullets amended and its conclusion still standing.*

- **The gRPC bounds exist now.** `internal/agentserver/server.go` constructs
  the server with `MaxConcurrentStreams` (8), `ConnectionTimeout` (30s), a
  `MaxConnectionIdle` of five minutes and a keepalive enforcement policy
  (`MinTime` 30s, `PermitWithoutStream: false`). Read what each one bounds
  rather than the group: `MaxConcurrentStreams` bounds streams on **one**
  connection, and an agent opens one stream, so the headroom is deliberate.
  **None of them bounds how many connections one pod may open**, which is
  precisely the attack this entry describes. A connection carrying a live
  stream is never idle, so the idle reaper does not touch it either.
- **The `TokenReview` is cached, the pod lookup is not.**
  `internal/grpcauth/cache.go` keys on a SHA-256 of the token, holds an
  accepted review for 60 seconds and a refusal for 10, and never stores the
  third answer — "the API server could not say" — because caching an outage
  would extend it past its end. The line is the design: deleting a pod —
  the revocation an operator actually performs — takes effect on the next
  connection attempt whatever the cache holds, because the pod lookup runs
  every time. What the cache can delay is a token revoked while its pod still
  runs, which in Kubernetes means deleting the ServiceAccount.
- **There is a rate limit, on cache misses only.**
  `internal/grpcauth/limiter.go` is a token bucket per peer address: five
  misses of burst, one refilled per ten seconds, refused attempts returning
  `codes.ResourceExhausted`. It is deliberately unreachable by a pod replaying
  one valid token, because that pod hits the cache — which is the same
  sentence as "a pod in a connection loop with one good token is not rate
  limited". What it bounds is the API-server load such a pod can generate, not
  the connections it can open.
- **The NetworkPolicy exists** (`config/deploy/` at 6b, now
  `charts/spawnery/templates/networkpolicy.yaml`, gated on
  `values.networkPolicy.enabled` since milestone 6d and on by default),
  admitting 9443 on the operator pod only from pods labelled
  `spawnery.cloud/managed-by`, in any namespace. Whether it removes the
  anonymous half of the attack depends entirely on the cluster's CNI, and
  nothing in this repository has observed a connection to 9443 being
  refused — see "From milestone 6b" below.

**So the conclusion of this entry survives 6b intact.** A single pod in a
connection loop can still open connections without bound, and a *compromised
managed* pod carries the label the policy admits, so the policy would not stop
it even where a CNI enforces. What 6b removes is the API-server amplification
— the cache is what does that, and it is the one part of this whose effect is
observable from inside the operator, in
`spawnery_agent_token_review_cache_hits_total` beside its misses and
`spawnery_agent_rate_limited_total`. Anyone quoting milestone 2a's promise
still has to quote this entry with it.

## From the milestone 6a Task 4 measurement round (2026-08-17)

The "Completeness of the permission table" item above names
`test/e2e` (`TestSpawneryUnderItsOwnServiceAccount`) as the only thing
that can prove a permission missing from both the ClusterRole and
`internal/rbacaudit/required.go` at once. This entry narrows what that
proof actually covers, found while verifying the check itself rather
than while using it.

**A denied `list` on a watched-but-idle type (`pods`, and separately
`networks`) is invisible to the check, not merely late.** The check
reads the operator's log once, moments after rollout; the first
hypothesis was that this was simply too early — `OrphanReconciler`'s
periodic sweep, one of the two call sites `required.go` cites for
`pods:list`, only ticks once a minute, and no custom resource exists
yet in a Task-4-only run to drive the other reconcilers at all.
That hypothesis was tested and rejected. `pods:list` was removed from
every marker that grants it (`controller-gen` unions verbs per
resource across markers, so a partial removal is not a real
mutation), the revocation was confirmed at the API server
(`kubectl auth can-i list pods --as=system:serviceaccount:spawnery-
system:spawnery-operator` → `no`), and the operator was then watched
continuously — log followed, not sampled; `rest_client_requests_total`
scraped every 15 seconds, every sample kept; pod phase and restart
count watched — for seven minutes and forty-five seconds, well past
`Controller`'s default two-minute `CacheSyncTimeout`. Across the whole
window: the log gained not one line past the initial
`"Starting workers"` burst (32 lines at second 0, 32 lines at 7m45s);
`rest_client_requests_total` recorded only codes `200`, `201`, `404`
and `429`, never `403`, across 24 fifteen-second samples; the pod's
restart count stayed `0`; `kubectl logs --previous` confirms there is
no previous container to have hidden anything in (`"previous
terminated container \"operator\" ... not found"`); and the
Deployment's `Available` condition never moved off `True` from
`04:05:46Z` onward. The two-minute boundary passed with no observable
effect of any kind — the process did not exit, and nothing in the
vendored source's own documented failure mode
(`sigs.k8s.io/controller-runtime/pkg/internal/source/kind.go:58-59,82`,
"`cache.GetInformer` will block ... and log ... `failed to get informer
from cache`") was ever seen to fire, even though the same log line
*does* appear, promptly, when a startup-critical uncached call is
denied instead (see below).

**The contrast is what makes this a coverage gap and not a
measurement artifact.** Denying a direct, uncached call that gates a
leader-bound `manager.Runnable` — `create` on the TLS secret
(`internal/certs/store.go`), or `update` on the leader-election lease
(`internal/controller/setup.go`) — produces a real, immediate, and
repeated `is forbidden:` line every time
(`"ensure the TLS bundle: create spawnery-agent-tls: secrets is
forbidden: ..."`; `"Failed to update lease" err="...is forbidden: ...
cannot update resource \"leases\"..."` on a ~3-4 second retry loop).
Those two mutations sit at the opposite failure mode: the denial is
loud, but the pod never reaches `Available`, so `hack/e2e.sh`'s
rollout wait times out and `test/e2e` never runs at all. Between the
two failure modes — cache-backed reads that are silent and safe,
direct calls that are loud and fatal to readiness — no permission
removal tried during Task 4 both fires a logged denial *and* leaves
the operator healthy enough for the check to read it.

**What this means for anyone relying on a green `test/e2e` run.** A
pass proves the operator was not denied anything it actually asked
the API server for while the check was watching. It does not prove
every permission the ClusterRole grants is exercised, and — this
entry's finding — it specifically does not prove a *watched* resource
type (anything reached only through the manager's cache: `Owns()`,
`For()`, the restricted caches in `cmd/spawnery-operator/main.go`) is
correctly permissioned, because a missing permission there produces no
signal this check — or, on this evidence, the pod's log at all —
ever surfaces. Proving that class needs either a second signal this
check does not currently read (the metrics endpoint;
`kubectl logs --previous` after a restart, if the informer failure
ever does become fatal on some other code path) or CR-driven traffic
that forces a controller to actually call through to a live List
rather than only register a watch for one. Milestone 6a's Tasks 5
through 8, which apply `test/e2e/manifests/e2e.yaml` and drive
reconciles, narrow this for the reconciler-triggered call sites but do
not by themselves explain why the *informer's own* initial sync — which
the vendored source's comment says should retry and log on its own,
unconditionally, without any reconcile — produced nothing observable
here. That mechanism was not chased further.

*The raw output is gone, 2026-08-22.* This entry cited
`.superpowers/sdd/2026-08-16-operator-image-and-e2e/task-4-report.md` for the
full transcript of both mutations. That directory is git-ignored scratch, and
the subagent-driven workflow that creates it deletes it on completion by
design, so nothing there survives and nothing is in git either. What remains
is the account above, which is why it states its numbers — 32 log lines at
second 0 and at 7m45s, 24 samples carrying only 200, 201, 404 and 429, restart
count 0 — rather than deferring them. **Nothing in this file should cite that
directory**: two other entries did, both under milestone 6d, and both are
corrected the same way.

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

**And a third way past it: the verb was never exercised while anybody was
watching.** `ServerGroupReconciler.retireServer` nominates a server for a
rolling update with a `client.MergeFrom` patch, and the `servers` marker
granted `get;list;watch;create;update;delete` -- no `patch`, which is its own
verb to the API server. So the operator could create and delete servers but
never retire one: a rolling update brought the new generation up and left the
old one running beside it, the reconcile erroring every few seconds with
`is forbidden`, and `status.freeSlots` frozen at whatever the failed pass
never got to write. Fixed 2026-08-25, found by changing a `ServerGroup`'s
image on a live cluster rather than by any test.

Three guards were in place and none could see it. `required.go` is checked
against the generated role in both directions, so table and ClusterRole can
never drift -- but the table is written from the markers rather than from the
call sites, so both sides agreed on the same wrong answer. The envtest
controller tests do drive `retireServer`, and envtest enforces no RBAC against
their admin client, so a missing verb is invisible there by construction. And
the `is forbidden:` sweep above would have caught the exact string -- except
no e2e scenario ever changes a `ServerGroup`'s image, so the one path that
needs the verb is never taken during a run.

The intersection is the hole: a verb is only proven by a test that both runs
under the operator's own identity *and* takes the path that needs it. Driving
an image change in `make e2e` would close it for this verb. Nothing closes it
for the next one, because no machinery here derives the required table from
the code.


**"The whole log" is only as whole as the container is old, and the check now
says so itself.** Until the milestone's final fix wave, `operatorLog` passed an
empty `PodLogOptions`, which returns the *current* container's log and nothing
else, and the restart count was read once — by the first subtest, before any
scenario had driven a single call. An operator OOM-killed mid-run on a
memory-tight host would have left the last subtest reading a log that began after the
interesting part, and a replacement process making no denied call of its own
would have reported PASS over a run it had covered a fraction of.
`theOperatorWasNeverDenied` now re-reads the pod, prepends the previous
container's log where the kubelet still has it, and fails on any restart at
all. Kubernetes keeps exactly one container back, so from the second restart on
there is a stretch nothing can read — which is why the check errors rather than
patching over it.

**Two permissions in the table that no driven scenario exercises,** measured on
a held cluster at the end of Task 8 rather than guessed:

- `persistentvolumeclaims: patch` — **measured.** `growClaim`
  (`internal/controller/server_controller.go`) patches only when
  `spec.storage.size` has grown past what the claim already requests. No
  scenario changes a group's `storage.size` after its claim exists —
  `test/e2e/manifests/e2e.yaml` fixes `survival` at `1Gi` and every patch the
  harness makes goes to `replicas` or the scaling bounds — so the guard
  short-circuits before the `Patch`. Confirmed at the end of the run: both
  `survival-*-data` claims still request and hold their original `1Gi`.
- `tokenreviews: create` — **reasoned, not measured.** No image in the run
  resolves, so no agent process exists anywhere to open a stream, and
  `grpcauth.Authenticator.Authenticate` is never called. This one could not be
  confirmed the way the other was: this client-go build's
  `rest_client_requests_total` carries only `method`, `code` and `host`, with
  no `resource` or `verb` label, so a TokenReview `POST` is indistinguishable
  in that counter from the run's ninety-odd other creates. Disaggregating it
  would need API server audit logging, which this harness's cluster does not
  enable.

Both are absence-of-agent gaps in what the harness proves rather than defects:
a deployment with a resolvable image exercises both. `pods: patch` is
deliberately **not** on this list, and why is under "On the RBAC audit" below.

**Both were measured by the RKE2 rollout on 2026-08-20**, and the harness
still exercises neither — the two statements are compatible and worth keeping
apart. `persistentvolumeclaims: patch`: a persistent group was created on
Longhorn and grown from `1Gi` to `2Gi`, and the operator's own client metric
moved from no `PATCH` line at all to exactly one. `tokenreviews: create`, which
this entry rightly called reasoned rather than measured:
`spawnery_agent_token_review_cache_misses_total` — `internal/grpcauth/metrics.go`,
"Token checks that required a TokenReview" — went 7 to 8 when a single lobby pod
was deleted and its replacement's agent connected. Neither the client metrics
nor the API server's own `apiserver_request_total` could have answered that
second one: the first has no per-resource label, and the second stood at 38,607
across every client on the cluster. See `docs/runbook-milestone-6-rollout.md`,
scenario 7.

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

**Any patch to a `ServerGroup`'s spec bumps `metadata.generation`,** and
therefore starts a rolling update beside whatever the patch was for. Task 5's
scaling scenario was written against `minReplicas`, passed, and passed for the
wrong reason: what it observed was churn from the run's own 20-second startup
deadline rather than a scaling decision, and the rewrite that fixed it walked
into the same trap once more — a cold start refused by the ceiling instead of
the plain over-ceiling branch. Anyone adding a scenario that patches a spec
should assume a generation change rides along and say which branch the
assertion is actually pinning. (`docs/known-issues.md`'s "From milestone 4b"
section carries the same property as a product concern.)

**A group's count of live servers briefly touched zero under sustained
churn** before recovering — observed during Task 5, not investigated, and
recorded here for whoever next looks at backoff and replacement timing.

**The orphan sweep dispatches on `podspec.LabelRole`.** A fixture pod built
without that label is never routed to `sweepServerPod`
(`internal/controller/orphan.go`), so a scenario built on one tests nothing
while looking like it does. Task 6's brief shipped with exactly that defect and
it was caught by reading the switch rather than by the run.

**Smaller things this milestone leaves, none of them shipping behaviour:**

- Swapping the namespace in `test/e2e/rbac_test.go`'s third loop to the
  operator's own namespace would leave the scenario green, because
  `secrets: get` is granted there too for `certs.Store.Ensure`. The loop is
  correct as written; the mutation is simply not catchable by it.

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

**The consequence, stated once so nothing downstream has to re-derive it: 6b
has not observed a single connection being refused, anywhere.** Every test it
ships asserts an object — the rendered policy in `internal/podspec`, the
manifest in `internal/rbacaudit`, the created object and the operator's
continued readiness in `test/e2e`. On this harness a perfect policy and a
wholly broken one produce the same green, and the e2e scenario
`theOperatorStaysReadyBehindItsOwnPolicy` says so in its own doc comment
rather than leaving a reader to infer it. The invariant open since 3b — a
Paper server runs `online-mode=false`, authenticates nobody, and trusts
whatever completes the modern-forwarding handshake with the right secret — now
has a policy written against it.

*The RKE2 rollout turned that into an observation, on 2026-08-21.* Against
that cluster's CNI, recorded at `internal/podspec/netpol.go`: a pod carrying
the managed-by, network and role=proxy labels reached a backend on 25565, and
the same pod without labels timed out. So the policy does refuse connections
where the CNI enforces it — and the same measurement established what it
cannot do, which is the part worth carrying forward. The ingress peer is a
podSelector over labels a pod's own creator chooses, so anyone who may create
a pod in a game namespace can wear the policy's colours; and closing that
would buy little, since the same privilege reads `velocity-forwarding-secret`
outright, measured the same day by mounting it from an unlabelled pod. The
boundary is the namespace, not this policy. Everything above about kindnet
still stands unchanged: on the harness `make e2e` runs, nothing is enforced
and the object changes nothing.

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

**The Service ClusterIP and the DNAT question, which the RKE2 rollout has to
settle.** The per-`Network` policy's egress rule to the operator names the
operator's pod by selector (`podspec.OperatorPodLabels()`, intersected with the
operator's namespace in a single peer). What the agent actually dials is
`spawnery-operator.<ns>.svc`, which resolves to a Service ClusterIP that
kube-proxy DNATs to that pod's IP. Whether a pod-selector egress rule matches
depends on whether the CNI evaluates policy **before or after** that
translation: after, and the selector matches; before, and the rule would have
to be an `ipBlock` covering the Service CIDR instead — which the operator
cannot discover from inside the cluster. The design (§6) declined to assert
which side any particular CNI falls on, exactly because that is the class of
claim this project keeps catching itself making from memory. The pod-selector
form is what ships. It cannot be tested where the CNI enforces nothing: on
kindnet an egress rule that matches and one that does not are the same green.
If backends stop reaching the operator on a real CNI, this is the first thing
to check, and the symptom would be every agent failing to connect at once
while the objects all look correct.

*Settled for one CNI, 2026-08-22.* The RKE2 cluster runs Cilium, carries
`production-backends` in `minecraft` — the per-`Network` policy, selecting
`role=server` — and its backend `lobby-hktx` reports `Ready`, which the design
only grants once that server's agent has connected to the operator. So on
Cilium the pod-selector egress rule does match traffic addressed to the
Service ClusterIP: policy is evaluated on a side where the selector still
resolves. That is one CNI, not the class. The operator still cannot discover
which side any other CNI falls on, and the `ipBlock`-over-Service-CIDR
fallback this entry names is still what a CNI on the other side would need.

**The DNS rule has the same exposure, and its symptom looks nothing like the
one above.** A backend resolves through the cluster DNS `Service`, whose
ClusterIP kube-proxy DNATs to a CoreDNS pod exactly as it does the operator's,
and the per-`Network` policy's first egress rule names `kube-system` by
namespace selector — so a CNI evaluating policy pre-DNAT would drop the
resolver query too. The RKE2 rollout's obligation is therefore both rules, not
just the operator hop: check that a backend can resolve as well as that it can
dial. The symptoms diverge, which is why this is worth naming separately — the
operator hop failing looks like agents that never register, while DNS failing
looks like nothing resolving at all, including the operator's own name, so the
agent failure is a downstream effect and the first thing to check is the wrong
one.

*Settled by the same observation, 2026-08-22, and by the same limit.* A
backend that reaches `Ready` on Cilium has resolved the operator's name to
dial it, so the `kube-system` namespace-selector rule admits the resolver
query there too. Both rules therefore work on that one CNI, and neither is
established for any other.

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

**Six defects in this milestone's own plan test code, each an assertion that
could not fail or that would have failed for the wrong reason.** This is the
seventh milestone in a row where mutation found what reading did not; milestone
5's handover records six of its own across five shapes. What is worth carrying
is the shapes, not the count:

1. **An assertion that verifies only non-nilness.** Task 1's egress-peer test
   checked that the operator peer had a `PodSelector`, never its content:
   replacing `OperatorPodLabels()` with a wrong map left all five subtests
   green — the exact trap the builder's own doc comment warns about.
2. **Unasserted fields on an object whose whole point is those fields.** Task
   1's owner-reference test left `APIVersion` and `BlockOwnerDeletion`
   unchecked; a wrong `APIVersion` means the owner never resolves and the
   policy outlives its `Network`, which is the single failure the owner
   reference exists to prevent. Both are now compared against
   `spawneryv1alpha1.GroupVersion.String()` rather than a literal, so they
   cannot drift from the scheme.
3. **A one-directional check.** Task 3's admitted-ports assertion checked that
   every required port was present and never that no other one was: adding port
   9999 to the peerless rule left it green. See the peerless-rule entry above.
4. **A test that passes down a branch that accepts anything.** Task 4's
   over-limit stream test was documented as blocking in `Recv()`; grpc-go
   v1.83.0 actually blocks in `NewStream` on the mirrored
   `SETTINGS_MAX_CONCURRENT_STREAMS` quota (`internal/transport/http2_client.go`),
   so the branch a passing run really takes was `if err != nil { return }`,
   which accepts **any** error. Widening the accepted set to
   `{DeadlineExceeded, Unavailable, ResourceExhausted}` made it worse, because
   two of those are codes this codebase produces for unrelated reasons and both
   are reachable only *after* the bound has already failed. It is now
   `DeadlineExceeded` alone.
5. **A name that claims more than the test proves.**
   `TestARepeatedTokenNeverReachesTheLimiter` passed with the limiter **removed
   entirely**, not merely moved: replaying one token means only the first call
   misses the cache, so `reviews.calls != 1` was satisfied by the cache alone.
   It proved ordering while its name claimed ordering *and* wiring;
   `TestDistinctTokensFromOnePeerAreRateLimited` now covers the second half.
6. **A prescribed mutation that could not produce a failure.** Task 7's brief
   said to rename `podspec.NetworkPolicyName` and watch the e2e scenario time
   out. It passed, all fourteen subtests: the test calls that same exported
   function to build its own expectation, so both sides move together and a
   broken build is indistinguishable from a correct one. The real mutation —
   hardcoding a suffix onto what `BuildNetworkPolicy` writes, leaving the name
   function alone — produced the predicted red. **A mutation that shares a
   function with the code under test is not a mutation**, and it looks exactly
   like one on the page.

**What Task 4 established about grpc-go, and the part of it that is
fixture-specific.** `MaxConcurrentStreams` is mirrored to the client through
SETTINGS, and the client blocks in `NewStream` when the quota is exhausted —
which is why the over-limit test's runtime is dominated by its 5s deadline
rather than by anything the server does. The test now accepts
`DeadlineExceeded` alone, and that narrowness is safe **in this fixture only**:
the two real paths to `Unavailable` during the quota wait are a GOAWAY and the
transport context closing, and neither can fire here because the connection is
never idle (eight live streams), `MaxConnectionAge` is unset, the test's client
sets no keepalive so `ENHANCE_YOUR_CALM` cannot fire, and the transport context
is cancelled only by `t.Cleanup`. If that test ever shares a connection across
subtests or adds client keepalive, the narrowing has to be revisited.

**`make agent-test` was run for this milestone and stayed green**, from a
pruned podman store, with no `ENHANCE_YOUR_CALM`: the new keepalive enforcement
policy regresses neither real agent. That matters because the agents send no
keepalive at all — `agent/common`'s `SessionLoop` says so in its own comment —
so the enforcement policy has nothing legitimate to throttle. Nothing under
`agent/` or `proto/` changed on the branch.

**The per-peer rate limit was per *connection*, and the attack it was written
against reset it on every reconnect.** Found by the whole-branch review, fixed
in 6b's final fix wave, and recorded here because the shape is more useful than
the fix. `peerAddr` returned `peer.FromContext(ctx).Addr.String()`, and that
`Addr` is a `*net.TCPAddr` whose `String()` is `IP:ephemeral-port` — measured
from a real gRPC server as `127.0.0.1:42662` on one connection and
`127.0.0.1:42674` on the next. So the bucket key named a connection, every TCP
connection started with a fresh `PeerBurst`, and the pod-in-a-connection-loop
this file documents could spend five real `TokenReview` calls, close, reconnect
and spend five more, bounded only by how fast it completes TLS handshakes. It
failed **open**, which is why nothing broke and nothing said anything.
`net.SplitHostPort` makes the bucket one per pod IP, which is what the design's
mass-reconnect safety argument had assumed all along.

The reason it survived is the part worth carrying, and it was measured
rather than reasoned: **no test in the tree exercised the limiter's key.**
Replacing `peerAddr`'s body with a constant left `go test ./...` entirely green
before the fix, because no test carried a `peer.Peer` in its context — the unit
tests pass `context.Background()` and the envtest fixture passes `testenv`'s
context, so the key was the constant `"unknown"` in every one of them — while
`TestPeerLimiterBucketsArePerPeer` exercised the limiter's own keying with bare
IP strings the production path never produced.
Two tests, each individually reasonable, and between them a seam nothing
crossed. `TestTheRateLimitKeysOnThePodRatherThanTheConnection` now installs the
`peer.Peer` a real server installs and closes both directions.

**The `TokenReview` cache's bound was not a bound, and its test could not see
that.** `evictExpiredLocked` deleted only expired entries, so with
`maxCacheEntries` live entries it deleted nothing and the map grew for as long
as distinct tokens arrived. The test stored 1025 entries at one frozen instant —
already past the bound — then advanced the clock past `PositiveTTL` before its
final store, so its length assertion ran against a map the expiry sweep had just
emptied. Green for a cache that bounded nothing, under a comment claiming "this
bounds how large it gets in between". 6b's final wave made the bound real rather
than correcting the claim: `store` sweeps expired entries first and drops the
entry closest to expiry only when that frees nothing. The replacement test's
clock never moves, so nothing can expire and the bound can only hold by
dropping a live entry.

**Smaller ones, each worth a sentence:**

- The per-`Network` policy's own `spawnery.cloud/network` metadata label is
  unasserted. It is there for a human reading `kubectl` output rather than for
  a mechanism — and so, it turns out, is `managed-by` beside it. Four places in
  6b said that label existed so the operator's restricted cache could see the
  object: `internal/podspec/netpol.go`, a *failure message* in
  `netpol_test.go`, the design's §3.2, and this file. There is no such
  restriction. `cmd/spawnery-operator`'s `Cache.ByObject` has no
  `NetworkPolicy` entry, so `Owns(&NetworkPolicy{})` starts an unrestricted
  informer, and all four have been corrected to say what the label is. The
  claim was deleted rather than the restriction added: NetworkPolicies are a
  handful per cluster, unlike the ConfigMaps and claims those entries exist to
  bound, and adding the restriction would be a regression — `CreateOrUpdate`
  reads through that cache, so a pre-existing *unlabelled* object at
  `<network>-backends` would be invisible to its `Get` and every pass would
  `Create` and take `AlreadyExists`, forever.
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
- The refusal split reorders one message. A token that authenticates, belongs
  to a valid ServiceAccount, asks for the wrong role **and** lacks pod-binding
  claims now reports "token is not bound to a pod" rather than the role
  message. No refusal was dropped and no error type changed, and `TokenRequest`
  cannot mint that combination.
- `evictFullLocked` is not a hard cap: with `maxBuckets` peers all
  simultaneously active nothing is evictable and the map grows past it. That is
  the many-compromised-pods case the design ruled out of scope, recorded so the
  absence reads as a decision.
- **The ingress peer is a label selector, so it admits whoever wears the
  labels.** A pod in the game namespace carrying `managed-by`, that network's
  `spawnery.cloud/network` and `role=proxy` is admitted to the backends on
  25565 wherever a CNI enforces, whoever created it. Creating one takes pod
  `create` in that namespace, which is authority over the namespace anyway, so
  this is the ordinary shape of a pod-selector policy rather than a defect —
  but "only this network's proxies" means "only pods labelled as this
  network's proxies", and the two are the same sentence only for as long as
  nobody else can write those labels.
- **The rate limit lives inside `Authenticate`, not in the interceptor.** The
  design's §5.3 sketched it "in the interceptor before `Authenticate`"; it is
  at `internal/grpcauth/identity.go`'s cache-miss branch instead, and it has to
  be, because "consulted only when the cache misses" cannot be decided by a
  caller that has not yet looked in the cache. Worth knowing before reading the
  design and expecting to find it a layer up. **It is also the proximate cause
  of the milestone's one critical defect, and that is worth recording beside
  the ruling that it is right.** Living inside `Authenticate` forced the peer
  to be recovered from a `context.Context` — and no test in the package put a
  peer in one: the tests passed `context.Background()`, so the key was always
  `"unknown"`; nothing exercised the real key; and a key naming a connection
  rather than a pod therefore survived the whole milestone. The interceptor
  placement would have made the peer an explicit parameter and the bug visible
  at the call site. The placement stands, because the alternative is a limiter
  consulted on every request; what it costs is that the seam has to be tested
  deliberately, which
  `TestTheRateLimitKeysOnThePodRatherThanTheConnection` now does.
- `newAuthFixture` (`internal/grpcauth/auth_envtest_test.go`) wires neither the
  cache nor the limiter, which is legal because both types' methods are
  nil-safe — and it means the package's existing envtest suite exercises the
  uncached, unlimited path. That is why a mutation to the cache broke nothing
  there and needed a test written for it.
- The new RBAC entries' `Why` fields will go stale the first time a second call
  site appears, exactly as `configmaps`' and `pods: patch`'s did. Nothing in
  the audit can catch it; both precedents are under "On the RBAC audit" below.

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

**`status.address` can go on advertising a Service that has been deleted, and
6c made that reachable through its own headline path.** `Reconcile` returned
early on any error, and `setStatus` — the only writer of `status.address` —
was after every one of those returns, so a group that failed partway through
a pass kept whatever address the last successful pass had published. The
shape predated 6c. What 6c added was a way to sit in it indefinitely:

> A `NodePort` group publishing `10.0.0.7:30765` is switched to `HostPort` in
> a namespace enforcing Pod Security `baseline`. `reconcileService` deletes
> the Service; `reconcileReplicas` is then refused by the API server, so
> `Reconcile` returns before `setStatus`. `status.address` keeps naming the
> node port of a Service that no longer exists — and because the create can
> never succeed while the namespace's label stands, no later pass corrects
> it. The group does say `Degraded=True`/`ReasonProxyPodRejected` with the
> API server's own message, so the failure is legible; the address beside it
> is not.

Recorded rather than fixed at the time, deliberately, and the reasoning is
worth keeping because the obvious fix was worse than it looked. Recomputing
the address on the error path with `proxyAddress(group, pods, svc)` does
clear the deleted Service's node port — but in this exact scenario the
group's old `NodePort` pods are still `Ready` while their replacements are
refused, so the `HostPort` branch would publish `hostIP:25565` for a port no
pod in existence binds. That trades a stale address for a fabricated one.
Clearing the address outright on the error path is worse again: the same
error return covers a group that stays `NodePort` and hits an unrelated
quota or RBAC refusal while its live Service and ready pods keep serving
players, and blanking that group's address would be a regression caused by
this entry rather than a fix.

*Closed, in two changes taken in that order, by
`docs/superpowers/specs/2026-08-23-proxygroup-status-on-every-path-design.md`.*
The fabrication this entry predicted as a *cost* of the obvious fix turned
out to already be live *behaviour*, independent of any fix, and it had to
close first: `proxyAddress`'s `HostPort` branch took the `HostIP` of any
ready pod and appended `spec.expose.hostPort.port` without checking that the
pod it found was one whose container actually declared that port. In the
entry's own scenario, had `setStatus` ever been reached with the old
`NodePort` pods still present and the new `HostPort` spec in force, it would
already have published `hostIP:25565` for a port nothing was listening on —
the early return only kept this from firing, it never fixed it. So
`proxyAddress` was changed first to require, per strategy, an observation
that actually supports the address: `HostPort` now needs a ready pod whose
container declares that `hostPort` (`internal/podspec/proxy.go` sets it only
under that strategy, so a pod from any other generation carries `0` and
cannot match), `NodePort` now reads the port the API server allocated off the
Service rather than echoing the spec, and `ClusterIP` now requires the
Service to exist. With fabrication no longer possible, `Reconcile` was then
split: everything from the namespace bootstrap through the second,
post-create pod read now lives in `reconcileObserved`, which returns what it
saw — the pods, the Service, and an `observed` flag — alongside its usual
result and error. `Reconcile` finalises once, after that call returns: if the
pass observed the pods and the Service, it calls `setStatus` and writes the
status on every one of that call's return paths, success or error alike; if
the pass never got far enough to look — a missing or unaccepted `Network`, or
an unimplemented `expose.type` — the address is left exactly as it was,
because nothing about the serving world changed on those paths.
`TestAGroupSwitchedIntoARefusedStrategyStopsAdvertisingTheOldAddress`
(`internal/controller/expose_test.go`) drives this entry's own scenario end
to end; `TestAFailureInsideReconcileObservedLeavesTheAddressAlone` beside it
is the guard against the fix becoming the regression the paragraph above
warned about — a group that stays `NodePort` and hits an unrelated failure
while its live Service and ready pods keep serving must keep its address.
`TestABrokenNetworkLeavesAWorkingAddressAlone` shows a different half of the
same rule: a path that returns before `reconcileObserved` is ever
called — here, a missing `Network` — leaves the address alone because the
pass never looked at the pods or the Service at all.

Both are envtest, and that is the limit worth stating plainly: `HostPort` is
exercised in envtest and unit tests only, never against a real kubelet.
`paulwtf` runs one `ProxyGroup`, on `ClusterIP`, so no cluster has driven the
`HostPort` or `NodePort` rows this work changed most.

A staleness the design's §5 does not cover, because that section justifies
only the second `pods` read: if `reconcileReplicas` deletes a pod and then
fails, `reconcileObserved` returns without ever attempting that second read,
so `obs.pods` is still the read taken *before* `reconcileReplicas` ran — the
deleted pod included. If that pod was `Ready`, `setStatus` can publish its
`HostIP` for the one pass in which the failure happened. It is bounded to a
single pass, since the next pass's first `pods` read no longer sees it, and
it is no worse than the address the code published before this design — but
it is a case the design does not name.

**A side effect worth naming rather than hiding: `status.observedGeneration`
now advances on a pass that failed, not only one that succeeded.**
`setStatus` writes it alongside the address, and `setStatus` is now reached
on every path that observed the pods and the Service, so a group
permanently refused by Pod Security reports `observedGeneration ==
generation` for as long as the refusal stands. That agrees with the field's
own definition (`api/v1alpha1/proxygroup_types.go`: "ObservedGeneration is
the spec generation this status was computed from") — the status genuinely
was computed from that generation. A reader who instead uses the looser,
common convention — `observedGeneration == generation` meaning "the
controller has caught up with the spec" — would be misled into reading a
permanently-refused group as settled. `Degraded=True` standing beside the
same generation is the correction: read together, they say the controller
did catch up and what it found was a refusal.

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

**`hack/chart-templates.sh` now checks its outcomes as well as its inputs.**
Its two original guards (`grep -q` on `config/rbac/role.yaml` and on each
file under `config/crd/bases/`) confirm only that the *source* still has the
shape the following `sed` is anchored to, which is exactly what a broken
`sed` leaves untouched: an intact input and a substitution that never fired
were indistinguishable to them, and both exited 0 while writing a corrupted
`rbac.yaml` (a surviving `spawnery-system` literal) or `crds.yaml` (no
`helm.sh/resource-policy: keep` annotation). The whole-branch review measured
that the CRD half was uncovered by anything else in the repository — a broken
CRD anchor gave `make manifests` exit 0, `make chart-lint` exit 0 and
`go test ./internal/rbacaudit/` fully green, with zero `keep` annotations in
`crds.yaml` — while the rbac half had become covered by
`TestTheChartRendersIntoTheNamespaceItIsGiven` when the audit moved onto the
rendered chart. The script now also asserts, over the files it wrote, that
`crds.yaml` carries the `keep` annotation once per CRD file processed
(counted against the files the run walked, so a fifth CRD cannot pass on the
other four's annotations) and that `rbac.yaml` carries exactly one
`{{ .Release.Namespace }}` and no `spawnery-system`. Closed; kept here
because the shape of the mistake — a guard that checks its input rather than
its outcome — recurred three times in this milestone.

**The design's claim about the forwarding-secret grant is wrong, and the real
consequence is quieter.** §9 of
`docs/superpowers/specs/2026-08-19-helm-chart-design.md`
and milestone 6d's own Task 6 brief both state that a misconfigured grant
leaves "every group in the namespace refuses with `NetworkNotAccepted`." It
does not. `internal/controller/network_controller.go`'s `Reconcile` sets
`ConditionAccepted` `True` before the forwarding secret is read, and nothing on
the read's path can clear it: the read's outcome only ever reaches
`ConditionForwardingSecretResolved` and
`ConditionForwardingSecretRotationPending`. Since 2026-08-24 that holds more
firmly than it did, because a failure below the condition now writes the status
before it requeues rather than discarding it. The one path that does return
before the write is the NetworkPolicy, recorded under milestone 6b above. So
`ServerGroup`s and `ProxyGroup`s in the affected namespace keep scheduling
normally, and only forwarding-secret rotation detection breaks, for that
namespace. `charts/spawnery/README.md` states the narrower, grep-verified
consequence.

**It used to break silently, and that half is closed.** `readForwardingSecret`
folded the `403` into a condition message written for a person — "the operator
may not read secret X; grant it with `kubectl apply` …" — which quotes no API
server and therefore carries no `is forbidden:` substring, and the controller
made no logger call at all. That substring is exactly what `test/e2e`'s
`theOperatorWasNeverDenied` greps the operator's log for, so a broken
`config/rbac/forwarding-secret-reader.yaml` grant was invisible to the one
check in this repository written to catch a denial the RBAC audit cannot — not
through the cache, the way that check's other blind spot works, but through an
error the code handled instead of surfacing. The read now carries the API
server's error out beside the message, and the controller logs it and records a
`Warning` on the Network. Both are gated on entering the state, like the
forwarding-secret events beside them: at `resyncInterval` an ungated report is
twelve a minute per Network, forever. The residue that gate leaves is worth
knowing — an operator restarted into an already-refused state finds the
condition already set and says nothing more, so the log line reports the
transition and the condition is the durable record.

**`v0.1.1` is a release whose chart no cluster can receive, and a tag cannot
be moved.** The four CRDs sit in `charts/spawnery/templates/` with
`helm.sh/resource-policy: keep`, so an upgrade carries a CRD schema change
through and an `uninstall` does not destroy every custom resource in the
cluster. The uninstall half was the half that had been observed; the upgrade
half failed the first time it mattered, and that failure is the useful one.

`v0.1.1` added a fourth `expose` strategy to the
`ProxyGroup` CRD's enum. The upgrade ran, the operator's image moved — and the
cluster's CRD never learned the new value. Flux names a packaged chart after
`Chart.yaml`'s `version`, that number had stayed at `0.1.0`, so the artifact
counted as unchanged and the HelmRelease kept serving the previous chart's
templates. The image moved only because the deployment pins its digest in
values, which is exactly what made the failure look like a success.

`v0.1.2` moved `Chart.yaml`'s version with the release and the enum arrived:
`["LoadBalancer","NodePort","HostPort","ClusterIP"]`.

Two guards were added rather than a note: `internal/rbacaudit`'s
`TestTheChartAgreesWithTheFlakeAboutTheOperatorRelease` pins `appVersion` and
`values.yaml`'s tag to `flake.nix`'s `operatorVersion`, and `release.yml`
refuses a tag whose `charts/` differ from the previous tag's while
`Chart.yaml`'s version does not — simulated against `v0.1.1` in a worktree,
where it exits 1. Neither reaches back: the tag stands as it was published.

**`TestARecreatedOrdinalCreatesItsPodOnceThePredecessorIsGone` flaked once.**
`internal/controller/server_controller_test.go`. It failed once during
milestone 6d's Task 6 `make test`, then passed both in isolation and on a full
rerun, and 6d changed nothing it touches — the test is envtest-backed and the
suspicion is timing around the predecessor pod's disappearance, but that is a
guess and nothing has reproduced it since. Recorded because an unrecorded
flake is rediscovered from scratch by whoever meets it next; if it recurs,
this entry is the second data point rather than the first.

**Hunted on 2026-08-23, not caught — and the failure is now made to explain
itself, which is the part that will matter.** What was tried: sixty runs of the
test in isolation, and five full-package runs, all green. What was ruled out
rather than assumed: **it is not cache lag.** `internal/testenv`'s `Client` is
`client.New`, a direct client with no informer behind it, so "the controller's
cache still shows the terminating pod" — the first hypothesis anyone reaches
for with envtest — cannot be the mechanism here.

What is left is genuinely unknown, and one occurrence with no captured output
is close to no information at all. So the assertion that failed now prints what
it saw: the `Accepted` condition, every pod in the namespace with its
`deletionTimestamp` and node, and whether the pod holding the name is still the
predecessor's. Those three separate the candidates — a lingering predecessor
means the force delete did not take; `PodNameTerminating` with no such pod
present means the controller decided against a pod that was already gone; an
empty namespace with a clean condition means something else refused the create
entirely.

**The UID, not the name**, and finding out why is the reason to write this
down: the ordinal's pod name is reused across generations, so "is a pod called
`survival-0` present" is true both while the predecessor lingers and once the
successor exists — the two states the failure has to tell apart. The first
draft asked by name and reported "predecessor still present: true" about a
perfectly healthy successor. It was caught only because the message was made to
fire on purpose and read, which is worth doing to any diagnostic before
trusting it.

**What milestone 6e adds to this entry: frequency, not a diagnosis.**
`go test -race ./...` now runs on every pull request through `ci.yml`'s
`test` job, which means this suite runs on the rhythm of how often somebody
pushes rather than the rhythm of how often somebody happens to run
`make test` by hand — far more often than a person does. A test with a real,
low-probability timing flake that nobody has diagnosed will therefore surface
more often simply because it is asked more often, with nothing about the
underlying flake having changed. A red `test` job in CI's first weeks, on
this test specifically, is a measurement of how often the flake already
happened to occur, not a regression CI introduced.

Two days of that rhythm have now run, and the flake has not taken any of the
chances. Measured 2026-08-22 over every `ci.yml` run the API lists — 61 runs
since 2026-08-20T08:35Z, 47 from pushes to `master` and 14 from pull
requests — **the `test` job has never concluded `failure`.** The ten red runs
are all elsewhere: five `e2e` (the Nix `vendorHash` mismatch of 2026-08-22 and
its predecessors) and five `lint`/`deps` on milestone 6e's own branch. So this
entry is still one data point, but it is one data point against roughly sixty
further executions of the suite rather than against nothing, and the next
person to meet it should weight it accordingly.

## From milestone 6e (CI)

GitHub Actions now runs three workflows: `.github/workflows/ci.yml` blocks
four jobs — `test`, `lint`, `deps`, `e2e` — on every pull request and on
push to `master`; `.github/workflows/nightly.yml` runs `make image-repro` on
a schedule plus `workflow_dispatch`; `.github/workflows/release.yml` runs
`hack/publish.sh` on a `v*` tag. Full account:
`docs/handover-milestone-6e.md`.

**`make lint` was never actually green, and a runner's cold cache is what
proved it.** Three local runs and two reviewers all reported `make lint`
exiting 0 during this milestone's own Task 3. `lint`'s first CI run found
five real `SA1019` findings in `internal/controller/setup.go`
(`mgr.GetEventRecorderFor`, deprecated) that none of them had seen. The
cause was `golangci-lint`'s own disk cache: every local run had been
answering from a cache warmed before those five findings existed, and a
hosted runner starts with none. `golangci-lint cache clean` followed by
`golangci-lint run` reproduced the five findings locally, and reproduced a
second thing: the milestone's own accepted count of "33" findings — written
into the design spec and into Task 2's commit message — was measured
against the same stale cache and was wrong for the same reason. The true
count, from a cleared cache, was **38** (26 `errcheck`, 12 `staticcheck`);
the missing five are exactly the `setup.go` findings the cache had been
hiding. This is the clearest demonstration in this milestone of what CI
actually bought: a check that had been "clean" on three different local runs
and two independent reviews was not clean, and only a machine with no
opinion about what it had already seen could say so.

**The lesson generalises past `golangci-lint`.** Any tool that caches its
own answers — a linter, a formatter, a build system's incremental state —
can report a tree as clean when the cache, not the tree, is what is clean.
The fix is not a smarter cache; it is to clear the cache before trusting a
count from it, the same way this entry's own correction (33 → 38) only
became visible once someone did.

**Plain `golangci-lint run` with no config reports at most three findings
sharing a message per linter, and the concurrent package walk means the
subset shown is not guaranteed to be the same one twice.** Three consecutive
runs during this milestone's design each reported seventeen issues, and the
note recorded alongside them at the time claimed each of the three named a
different set of files — read together, that would mean the cap was hiding
a genuinely unstable sample. That second half did not hold up: a reviewer's
three consecutive runs later, against the same pre-fix tree, each returned
the identical seventeen. So this entry says only what was actually
confirmed twice — the cap is real, seventeen against a true (cleared-cache)
count of thirty-eight is not close — and says the varying-subset claim was
observed once, during design, and did not reproduce when it was checked
again, rather than asserting it as established behaviour. `.golangci.yml`
now pins `max-issues-per-linter: 0` and `max-same-issues: 0`, so
`golangci-lint run` reports the true count with no explicit flags needed.
Worth an entry rather than only a commit message because the failure mode
generalises the same way the cache one does: any tool with a default output
cap reports a sample, and a sample that looks like a total is how a count
gets trusted that should not be.

**The events-API migration this milestone's lint fix forced, and how far
the one piece of evidence for its RBAC grant actually reaches.** Fixing the
five `SA1019` findings above meant migrating `internal/controller`'s five
`Recorder` fields from the deprecated `record.EventRecorder` to
`events.EventRecorder` — twenty-three production call sites, twenty-one
fake-recorder constructions across eight test files. The new recorder's
sink is the `events.k8s.io/v1` API, not the core group controller-runtime
used before, so it needed its own RBAC grant
(`config/rbac/role.yaml`, `charts/spawnery/templates/rbac.yaml`); the old
core-group grant stays, unrelated to this migration, because
controller-runtime's own leader-election lock still calls the deprecated
`GetEventRecorderFor` internally, and leader election is on by default here.

**Corrected after the milestone's final review, on the core grant's width.**
The sentence above is right that the core grant stays and right about why,
and wrong by omission about how wide it should be. It was cluster-wide,
which was correct while every controller wrote core events — and stopped
being correct the moment they stopped. Its one remaining consumer is a
leader-election lock on a Lease in the operator's own namespace, so the
events it writes regard an object in that namespace and nowhere else. The
marker at `internal/controller/server_controller.go` now carries
`namespace=spawnery-system` (the same placeholder, rewritten by
`hack/chart-templates.sh`, as the lease grant at
`internal/controller/setup.go`), the generated Role carries `""/events`
instead of the ClusterRole, and both entries moved to
`internal/rbacaudit/required.go`'s `RequiredNamespaced`. `events.k8s.io`
stays cluster-wide, because those events really do regard objects in
namespaces the operator does not know in advance.

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
role disagreeing — but the table itself is hand-maintained, and nothing
catches the table and the role agreeing while both are wrong against what
the code actually needs. envtest cannot close that gap either: its test
client is granted everything, so a missing grant is invisible to every
envtest-backed test in this repository. The only thing that has exercised
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

**`events.k8s.io/v1` caps a note at 1024 bytes — a limit the core `v1.Event`
this operator used before never had — and the check is `len()` on bytes,
not on characters.** A note over the limit is refused outright by the API
server, and the client library abandons the refused event with a `klog`
line; nothing retries it and nothing on the reconciled object says an event
was lost. Measured directly against envtest's real API server: 512
em-dashes is 512 *characters* and 1536 *bytes*, and is refused, despite the
API server's own error text saying "characters" — a rune-counting helper
would have been the wrong fix. Six sites build their note from text this
operator did not write — an admission refusal, a scheduler explanation, a
divergence list, a bootstrap error, a secret-resolution message and a CSI
driver's resize error — and the
sharpest case is the one milestone 6c built on purpose: a PodSecurity
`restricted` refusal is exactly the kind of API server text that runs past 1
KB, and before this fix it would have been dropped from `kubectl get
events` entirely while the `Degraded` condition (which allows 32 KB) kept
the full text. `internal/controller/events.go`'s `eventNote` helper now
formats first, truncates on a rune boundary, and appends a marker pointing
at the condition for the untruncated text; applied at all six.

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

**The nightly's red path has never been driven.** Two workflow paths once
existed only on paper here, and both have since run in the shape that ships —
including the `schedule` trigger, which this entry carried as a residue for
four days and which has now fired on its own on four consecutive nights,
2026-08-21 through 2026-08-24, on `master`. What has not run is the issue a
failure opens.

`9a25874` gave `nightly.yml` an `if: failure()` step that opens or edits a
`nightly-red` issue, an `if: success()` step that closes it, and
`hack/require-no-red-nightly.sh`, which refuses a release while one stands.
The one red nightly this project has had was run 32550359170 on 2026-08-22 —
the day before that step landed. So the opening path has never executed, the
closing path has only ever run against a repository with no such issue, and
the release gate has only ever measured an absence. That gate is the one where
absence means *permission* rather than refusal — so the branch nothing has
exercised is the branch that would stop a release.

`nightly.yml` was driven once before the merge, but not in its merged shape: it
needed a temporary `pull_request:` trigger, because GitHub will not run
`workflow_dispatch` on a workflow that has never reached the default branch,
and that trigger was removed before merging. Run 32350280041 is the merged
file, dispatched from `master`: `image-repro: success`.

`release.yml` has now run four times: once dispatched with `DRY_RUN=1` before
any tag existed, which is what that trigger is for, and three times on real
tags — `v0.1.0`, `v0.1.1`, `v0.1.2`. `skopeo login` works on a hosted runner
with `REGISTRY_AUTH_FILE`, the `WRITE_DIGEST` branch of `hack/publish.sh` has
run against a real registry, the digest guard has fired, and the per-image loop
has exercised both of its outcomes — publishing, and skipping an image already
at its version with exit 3, which `v0.1.1` and `v0.1.2` did for Paper and
Velocity.

**Corrected after the milestone's final review, on `release.yml`'s first
tag specifically: as the file shipped, that tag would have published
nothing.** Two defects, found by reading rather than by running, because
nothing can run this file until it is on `master`. First, the `skopeo
login` step relied on `XDG_RUNTIME_DIR`, which GitHub's hosted Ubuntu
runners do not set; with it unset, containers/image falls back to
`/run/containers/$UID/auth.json` and cannot create it as a non-root user
(`mkdir /run/containers: permission denied`, reproduced in this
repository's own dev shell, and containers/skopeo#1654 reports the same at
`/run/containers/1001/auth.json` on a runner). Worse than a failed step
would be a merely absent credential: `hack/publish.sh`'s guard would then
inspect ghcr.io anonymously, which answers `403 Forbidden`, and 403 is not
the `manifest unknown` the guard reads as permission to proceed — so the
run would stop at "cannot tell whether … already exists". The workflow now
names the credential file with `REGISTRY_AUTH_FILE` under `$RUNNER_TEMP`,
which also reaches the `skopeo inspect` and `skopeo copy` inside the
script. Second, the workflow ran `hack/publish.sh` with no arguments, which
the script's own header says publishes all three images and "refuses at the
first image whose tag is already there — correctly — and never reaches the
one that changed": a `v0.1.1` that bumps only `operatorVersion` would have
stopped on Paper's existing tag and never published the operator. The
workflow now invokes the script once per image and the script's refusal
carries exit status 3, distinct from 1, so "already there" is separable
from "I could not tell" without a second copy of the guard in YAML. A third
defect in the same file was found by the re-review of that fix and closed
the same way: with the loop above, a re-run of a tag that already published
finds all three tags present, and
the "this tag releases nothing" guard would have failed the job before it
reached the digest and Release steps — which are exactly what a re-run
after a transient failure there is for, and whose remedy text ("bump and
tag again") would have been actively wrong. The guard now exempts
`github.run_attempt > 1`, keeping its teeth on the attempt where a tag
pushed without a version bump is actually possible.

When this was written, none of these fixes had run, and what was verified
about the first tag was only what the review could check from outside:
authenticated, GHCR answers `manifest unknown` for a repository that does
not exist, so the guard proceeds rather than falsely aborting; and `gh api
/orgs/spawnery/packages?package_type=container` returned `[]`.

They have since run on a runner, repeatedly. Measured 2026-08-22:
`release.yml` has five successful runs from tag pushes (`v0.1.0` through
`v0.2.0`, the last of them the retag after the `vendorHash` fix), and that
same API call now returns `paper`, `velocity` and `spawnery-operator`. So the
`REGISTRY_AUTH_FILE` path authenticates on a hosted runner, and the
once-per-image invocation publishes a release that bumps only one of the three.
The clean-slate premise is spent; every future tag meets tags that are already
there, which is the case the exit-status-3 separation was written for.

**`release.yml` can now be dry-run without a tag, which it could not
before.** It has a `workflow_dispatch` trigger that runs the job with
`DRY_RUN=1` unconditionally and takes no input — the images are built and
what would be copied where is printed, and nothing can reach the registry,
so the button cannot become an accidental publish. This is the mechanism
the design's §5 already claimed existed ("`DRY_RUN=1` … is how the workflow
gets exercised before a real tag exists") and did not; §5 now says so. Like
`nightly.yml`'s, the dispatch cannot fire until the file is on the default
branch, so it too was a one-time post-merge check for whoever owns this
repository: `gh workflow run release.yml`, once, and read what it says it
would push. **Done — 2026-08-20T08:45Z**, the one `workflow_dispatch` run in
this workflow's history, concluded `success`.

**CI builds one of the three image derivations the release publishes, so a
stale Paper or Velocity hash is green all the way to the tag.** Recorded
while writing the 2026-08-22 green-CI gate, whose design had claimed the
opposite. `hack/e2e.sh` runs `nix build .#operator-image`, and that is the
only image derivation any job in `ci.yml` reaches: `make test` and `make
lint` enter Nix at all only through `nix develop`, and the `deps` job's
`make agent-deps` builds `.#agents.mitmCache.updateScript` and nothing else.
`make image-test`, `make velocity-image-test` and `make image-repro` — among
the targets that do build `.#paper-image` and `.#velocity-image` — are in no
CI job. Only `image-repro` and `agent-test` say as much in their own comments,
and what they say is that they are not part of `test` or `all`, which is a
narrower claim than this one: CI runs `make test`, `lint`, `agent-deps` and
`e2e`, and never `make all`. Verified by
`make -n` on all four CI targets in the dev shell, and by `nix path-info
--derivation -r .#operator-image`, whose 1431-derivation closure contains
`spawnery-operator` and no `paper`, `velocity` or `agents` derivation at
all.

What that leaves uncovered is not hypothetical: `nix/paper.nix` carries two
fixed-output `hash =` values (the paperclip launcher and Mojang's server
jar) and `nix/velocity.nix` a third, and nothing outside a release ever
fetches against them. `flake.nix`'s five `vendorHash` copies are covered,
but only accidentally — they all hold the same value, and one of the five is
`spawnery-operator`'s, which is why the `e2e` job caught the 2026-08-22
mismatch. Move a Paper or Velocity download instead and the first thing to
notice is `hack/publish.sh` on a runner, after a `v*` tag has been pushed
and the release has therefore already been announced. That is the 2026-08-22
failure exactly, one derivation over, and the green-CI gate does not close
it: the gate asks whether CI passed, and CI passing is the premise of this
entry rather than a defence against it.

One thing does build all three, on a slower clock: `nightly.yml` runs `make
image-repro` at 03:17 UTC, and that target builds `.#paper-image`,
`.#velocity-image`, `.#operator-image` and `.#agents`, each twice. So a
stale Paper or Velocity hash would go red there within a day. But its own
comment says it "does not block a pull request", nothing consults it at tag
time — the green-CI gate deliberately does not, because a nightly's signal
is not per-commit and would need its own staleness rule — and a nightly is
precisely the kind of standing red this repository has already let run three
times unread. Whether a red nightly gets noticed was the open question, and
it is the same question, not a different one.

**It has been answered, in the worst way available, by this entry's own
incident.** `nightly.yml` ran at 2026-08-22T03:56Z and concluded `failure`,
its `make image-repro` step being the one that failed. That is eleven hours
before the `v0.2.0` release died at 15:07Z on the stale `vendorHash`, and
`image-repro` builds `.#operator-image`, so the nightly was standing red on
the release's own cause while the tag was being prepared. Nobody read it — it
was found on 2026-08-22 only because this entry was being triaged. The run's
log is no longer retrievable through the API, so the cause is inferred from
what `master` carried at that hour rather than read from the output; the
conclusion and the failing step are read directly.

The green-CI gate would not have helped here either, for the reason above:
`ci.yml`'s `e2e` job was red on the same commit, and the gate catches that.
What the nightly adds is the two derivations `e2e` does not build — and this
run is the demonstration that adding a signal is not the same as adding a
reader.

Closing the gap properly means building all three per push: minutes of
runner time on every commit for derivations that change a handful of times a
year. Nobody had made that trade, which is why this was an entry and not a
commit.

**The trade nobody had written down, taken 2026-08-23.** There is a third
option between "every push" and "never": build them when the files that define
them move, which is exactly when a hash in this repository can move, and spend
one `git diff` on every other push. `ci.yml` gained an `images` job whose first
step is `hack/image-derivations-changed.sh`, and the two `nix build` steps
behind it carry `if: steps.changed.outputs.build == 'true'`.

Three things about its shape are deliberate and are the reasons it is worth
more than it looks:

- **It is a job in `ci.yml`, not a workflow of its own.**
  `hack/require-green-ci.sh` reads this workflow's *run* conclusion and never a
  named job's, precisely so a job added later is inside the release gate for
  free. A separate `images.yml` would have been outside it, and a red one would
  not have stopped a tag.
- **Every uncertainty builds.** An all-zeros base on a branch's first push, an
  empty base, a base the clone does not contain, a `git diff` that fails —
  each answers `true`. The wrong answer that costs runner minutes is always
  preferred to the wrong answer that costs coverage, and skipping is the exact
  failure the job exists to prevent. Its test drives all three not-knowing
  cases, because none of them is reachable in the ordinary run anybody would
  check by hand.
- **The path list is `nix/` entire**, plus `flake.nix`, `flake.lock` and the
  two files that decide this. Naming only the four files that carry hashes
  would be a list somebody has to remember to extend.

**It does not replace the nightly, and the entry above is still true of the
part it cannot reach.** A fixed-output hash breaks when the bytes at a URL
change, and upstream can do that without a line of this repository moving. This
job narrows the window between a bad commit and 03:17 the next morning; only
`nightly.yml`, which builds all four derivations unconditionally, watches for
the breakage that arrives from outside.

**What was made instead, on 2026-08-23: the nightly got a reader and the
release got a gate.** The gap above is unchanged — `ci.yml` still builds one
of the three, and a stale Paper or Velocity hash still goes green through
every pull request. What changed is that the nightly's verdict no longer stops
at a run nobody opens. `nightly.yml` opens an issue labelled `nightly-red` when
`make image-repro` fails and closes it when a later nightly passes, and
`release.yml`'s publish job refuses to start while such an issue is open
(`hack/require-no-red-nightly.sh`, beside the green-CI gate).

The rule is deliberately a person rather than a duration, and the reason is
this entry's own incident read the other way round. A nightly's verdict is
about the night it ran: "the last one was green" says nothing about a commit
pushed this morning, and "the last one was red" stands until 03:17 the next
morning even after the cause is fixed at noon. A gate reading the run would
therefore have refused the 2026-08-22 retag that fixed the `vendorHash` — it
would have blocked its own remedy. A gate reading an issue refuses until
somebody closes it, and closing it is a claim that the cause is fixed, with the
next nightly free to contradict them. **The override is the act of reading,
which is precisely what was missing.**

Two things this does not do. It does not make CI build the two derivations, so
the window between a bad merge and 03:17 the next morning is as open as it
was. And it cannot help a repository where the nightly never runs at all — a
disabled schedule leaves no issue to find, and the gate reads that as
permission, by the same rule that makes "no issue" the ordinary state.
`docs/superpowers/specs/2026-08-22-release-requires-green-ci-design.md` §2
carries the correction to the design that had refused this.

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

**`ProxyGroup.spec.expose` needed a fourth strategy for this, and got one
afterwards.** What was measured on `paulwtf` is the gap: the rollout put a
network behind Traefik's TCP entryPoint and had to use `NodePort` as a
stand-in, which left a node port allocated that nobody dialled and made
`status.address` report `<node>:<nodePort>` — an address nobody plays on.

The rest of this entry is not a claim about that cluster on that day.
`type: ClusterIP` with a required `clusterIP.address` was built after the
rollout, and when this was written it had been driven against no cluster at
all — only envtest and the E2E kind cluster, where no proxy image resolves and
no player has ever connected. What the operator does is narrow and unchanged:
it creates the Service the fronting thing routes to, publishes the address it
was given — once a proxy pod of the group is ready, and nothing before then,
the same gate every other strategy's address is behind — and creates no
routing object and verifies no address. See
`docs/superpowers/specs/2026-08-20-clusterip-expose-design.md` §4 for why each
of those refusals is a refusal rather than an omission.

**It has since been driven, and players have played on it.** Measured on
`paulwtf` on 2026-08-22, with the group `minecraft/gateway` `Ready` since
2026-08-20T10:47:30Z:

- `spec.expose` is `{"type":"ClusterIP","clusterIP":{"address":"mc.paul.wtf"}}`
  and `status.address` is `mc.paul.wtf`, two ready replicas.
- The routing object the operator refuses to create exists, written by hand:
  `IngressRouteTCP/spawnery-gateway` in `minecraft`, entryPoint `minecraft`,
  matching `HostSNI(*)`, to `Service/gateway` port 25565 with
  `proxyProtocol.version: 2`.
- `mc.paul.wtf` resolves to Traefik's three LoadBalancer addresses, whose
  Service publishes `25565:30561/TCP`, and `Service/gateway`'s EndpointSlice
  carries both proxy pod IPs `ready`.
- Both proxy pods have served real joins. Three distinct names appear in the
  logs the pods still hold — `WildesDomi`, `anweisen`, `DomiIRL` — each
  reaching `lobby-hktx` through the proxy and disconnecting cleanly; the most
  recent connect is 2026-08-22T16:57:35+02:00. That is what the pods' current
  logs carry, not necessarily every join since the group came up.

So the answer to "whether Traefik actually routes to that Service" is yes, for
this one fronting proxy in this one configuration. Two things ride along with
it. The client addresses in those logs are public ones
(`/95.89.220.159`, `/79.198.252.14`), not pod IPs, which means the PROXY v2
header Traefik sends is being honoured — the entry below about
`haproxy-protocol` under `[advanced]` is what makes that work, and this is its
confirmation from the other end. And the operator's refusal to verify the
address is unchanged by any of this: nothing in the chain above was checked by
the operator, and every link of it is the cluster owner's to keep.

**A `configOverlay` key in the wrong TOML table used to be silently ignored,
and looked right in the rendered file.** Velocity's `haproxy-protocol` lives under
`[advanced]`. Set at the top level it reaches the rendered
`/data/velocity.toml` — where it reads exactly as intended — and Velocity acts
as though it were `false`: no PROXY header is required, and a connection
carrying one is dropped without a log line. Measured on 2026-08-20 against a
scratch ProxyGroup with a hand-built PROXY v1 header sent straight to the pod,
no reverse proxy involved:

| key placed | no header | with header |
|---|---|---|
| top level | status response | silence |
| under `[advanced]` | silence | status response |

The overlay mechanism itself is sound and this is the first end-to-end
confirmation that it works: `internal/render/velocity.go` assigns whole
top-level keys from the fragment, the operator writes no `[advanced]` table of
its own, and Velocity fills the rest of that table from its defaults. Half a
day was spent on this one, most of it suspecting the reverse proxy, and the
reason it could be spent that way was that nothing in the operator knew
Velocity's schema — a misplaced key was indistinguishable from a correct one
until something downstream behaved strangely.

Since 2026-08-24 it does know, and this exact overlay is what the check was
built around. `internal/render/declared.go` measures an overlay against
Velocity's own `default-velocity.toml`, refuses a key it does not declare, and
says where the key is actually declared when it is declared somewhere —
`haproxy-protocol` at the top level names `advanced.haproxy-protocol` in the
error. The measurement above is kept because it is the evidence the check
rests on: it is what establishes that Velocity ignores the misplaced key
rather than failing on it, which is the whole reason a render-time refusal is
worth its cost.

**`syncOccupiedLabel` runs.** The "On the RBAC audit" list used to record that
`required.go`'s `Why` for `pods: patch` named `syncOccupiedLabel` — the Server
controller's, singular — while `ProxyGroupReconciler.syncOccupiedLabels` is
what runs on every pass. That entry is gone: the `Why` now names both. Both were
driven by hand-labelling pods `spawnery.cloud/occupied=true` and watching the
operator remove the label, which is the same `Patch` call the grant exists for:
`rest_client_requests_total{method="PATCH"}` moved 1→3 for the two proxy pods
and 3→4 for the lobby's server pod. The named call site does run and does issue
the patch. Both directions were then driven by a real join: with one player online the
labels appeared on their own — `true` on the occupied proxy pod and on the
lobby's server pod — the PATCH counter moved from 8 to 12, and both went away
again on disconnect.

**A PodDisruptionBudget's protective behaviour cannot be simulated.** Both
budgets select on `spawnery.cloud/occupied: true` and the operator sizes
`minAvailable` from its own occupancy tally rather than from the labels on the
pods. Hand-labelling pods occupied moved `currentHealthy` to 2 and left
`desiredHealthy` at 0, so the eviction API still allowed the eviction — which
is correct: the operator sizes from its own tally and a label nobody counted
changes nothing.

*"Only real players can make the budget refuse anything" is what this used to
say next, and it is wrong.* What the budget needs is an **occupied** pod, and
`proxyOccupied` is `Players != 0 || PlayersStale || !Connected` — a proxy whose
agent has gone silent counts, not because anyone is on it but because the
operator cannot know, and the conservative answer is the one it takes. Driven
on `paulwtf` on 2026-08-25 with nobody playing, in a namespace of its own: one
`ProxyGroup` at one replica, evicted successfully while healthy (`201`), then a
`NetworkPolicy` selecting the proxy for `Egress` with no allow rules — the
first policy to select a proxy at all, per the 6b entry above — to cut the
agent's stream. Eighteen seconds later `spawnery.cloud/occupied: true` was on
the pod, the budget read `minAvailable 1 / currentHealthy 1 / disruptionsAllowed
0`, and the eviction API answered `TooManyRequests: Cannot evict pod as it
would violate the pod's disruption budget` — the same sentence the player
produced. Deleting the policy cleared it within 24 seconds, so the protection
lifts on its own rather than sticking.

That run measured something else on the way past, which nothing had: **Cilium
enforces a NetworkPolicy on `paulwtf`.** The egress deny is what cut the
stream, so it was enforced rather than ignored — the opposite of what 6b
measured for kindnet on the e2e harness, and the first time the production CNI
has been asked.

A real player did, later the same day: both budgets went to `minAvailable 1`,
`disruptionsAllowed 0`, and the eviction API answered
`TooManyRequests: Cannot evict pod as it would violate the pod's disruption
budget` for the occupied proxy and for the lobby server carrying the session.
§6's "PodDisruptionBudget under a real eviction" is met.

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
visible.** `CALifetime` (`internal/certs/bundle.go`) is still ten years, and
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

**And the gauges could not be scraped at all until the same day, which made
this file's own advice unfollowable.** The operator has served metrics on
`:8080` since it existed, and the NetworkPolicy has admitted that port from
anywhere with a comment naming "a metrics scrape" — but
`templates/service.yaml` exposed only the agent port, so nothing could route to
it, and no `ServiceMonitor` existed to ask. The entry below tells a reader to
"alert on the gauge, not on the event" because the events expire out of the API
within the hour; as installed, there was no address at which those gauges could
be reached. The Service now carries the port the Deployment had always
declared, and `metrics.serviceMonitor.enabled` renders the object that scrapes
it. Both monitoring templates default to off, because their kinds come from the
Prometheus Operator's CRDs and a chart that renders them unasked fails `helm
install` on every cluster without them.

Nothing in the operator watches a certificate's remaining lifetime. The
procedure below exists and works, but it only runs because a human annotates
the operator's own TLS secret, `spawnery-agent-tls`; nothing triggers it on its
own. Design: `docs/superpowers/specs/2026-08-21-ca-rotation-design.md`, built
on the distribution guarantee
`docs/superpowers/specs/2026-08-21-ca-bundle-distribution-design.md`
established — that a new CA reaches every namespace holding a `Network`
without waiting for a pod restart, because `NetworkReconciler` calls
`Bootstrapper.Ensure` on every reconcile.

The whole interface is annotations on that secret
(`internal/certs/rotation.go`):

- `spawnery.cloud/rotate-ca=start` mints a second CA and publishes it beside
  the one that is still signing — the serving certificate keeps chaining to
  the old CA throughout (`Store.applyRequest`'s `RequestStart` case,
  `Bundle.WithNextCA`). `spawnery.cloud/ca-rotation-phase` becomes
  `distributing`.
- **`start` can sit unnoticed for up to an hour.** The annotation is only read
  on a tick of `Provider.Start`'s loop, and while nothing is rotating that loop
  ticks at `RenewCheckInterval` — one hour (`checkInterval(false)`). On an idle
  cluster the worst case after annotating is sixty minutes in which nothing at
  all happens: no phase, no event, no change to either gauge. The 30-second
  cadence only engages once a phase is set, which is to say after the tick that
  picked the request up. If that wait is not acceptable, restart the operator
  pod: `Provider.Start` runs `Store.Ensure` and `AdvanceRotation` once
  immediately, before it arms its first timer, so the new leader picks the
  request up as it comes up. `start` is the only request this affects: it is the
  only one sent while no rotation is in flight. `drivePhase` reports both
  `distributing` and `switched` as in flight, so from `start` onwards — across
  restarts, since the phase is re-read from the secret — the loop stays on the
  30-second cadence and a `drop-old` or `rollback` is picked up within it.
- From there the operator drives itself, checking every 30 seconds
  (`RotationCheckInterval`). It waits for two things in order. First, every
  namespace where an agent could be running to show the new CA in its own
  `spawnery-ca` ConfigMap: the union of the namespaces holding a `Network` and
  the namespaces holding a managed pod that is not in a terminal phase, read
  from the cluster on each check until the gate passes (`namespacesMissingCA`).
  Until that is true, `spawnery.cloud/ca-rotation-blocked-on` names the
  namespaces still missing it (truncated past ten). Second, once every
  namespace has caught up — `spawnery.cloud/ca-rotation-since` is stamped at
  that moment, not at `start` — a further wait covering the kubelet's
  projection delay plus the operator's own `--agent-session-deadline`
  (`drivePhase`; `projectionMargin` (2 minutes) + `Store.AgentSessionDeadline`,
  which defaults to 10 minutes — roughly a quarter of an hour in total, longer
  if that flag is raised), so that every agent stream open when the ConfigMap
  changed has had a chance to close and reopen at least once.
- **The gate stops running the moment `since` is stamped**, and is never
  re-evaluated for that rotation — neither half of the union. That is
  deliberate (design, section 5: a cluster where networks are created regularly
  would otherwise push the switch out forever), and it is the fact to reason
  from when reasoning about safety: what the switch is safe against is the set
  of namespaces as it stood at the instant the gate passed, plus the argument
  that anything created afterwards receives the two-CA bundle on its first
  reconcile and has never held anything else.
- **A `RotationBlocked` event does not repeat.** It fires only when the list of
  blocked namespaces *changes* (`drivePhase` compares the new note against
  `ca-rotation-blocked-on` before writing either), so a gate blocked on the
  same namespace for days fires once and then goes quiet — and Kubernetes
  expires the event out of the API within the hour, after which nothing in
  `kubectl describe secret spawnery-agent-tls` mentions it. The durable signals
  are the `ca-rotation-blocked-on` annotation and the
  `spawnery_ca_rotation_blocked_namespaces` gauge; alert on the gauge, not on
  the event.
- **A namespace with leftover agent pods and no `Network` blocks the
  rotation**, by design, and is named in `ca-rotation-blocked-on` like any
  other. `ServerGroup` and `ProxyGroup` carry no `OwnerReference` to the
  `Network`, so deleting a `Network` leaves the groups and their pods running,
  and nothing refreshes `spawnery-ca` in that namespace afterwards — switching
  would strand those agents at their next handshake. The remedy is to delete or
  drain the leftover groups; the gate clears within one 30-second check once
  their pods are gone.
- Once both conditions hold, the operator switches on its own: the serving
  certificate is re-signed under the new CA,
  `spawnery.cloud/ca-rotation-phase` becomes `switched`, and
  `spawnery.cloud/ca-rotation-since` is restamped — from here it reads as how
  long the outgoing CA has been waiting for a human. The operator then
  **holds**: the old CA stays published and trusted, and nothing moves
  further without a second annotation.
- `spawnery.cloud/rotate-ca=drop-old`, sent once `switched`, drops the
  outgoing CA; `ca.crt` is the only CA published afterwards, and all three
  rotation annotations are cleared.
- `spawnery.cloud/rotate-ca=rollback` **abandons** the rotation rather than
  pausing it, and works from either phase: out of `distributing` it discards
  the unused incoming CA, and out of `switched` it re-signs the serving
  certificate back under the old one (`Bundle.RestorePrevious`). Either way
  nothing is left that would advance on its own.

The operator clears `rotate-ca` once it has acted on whatever value it held —
a refusal consumes the annotation exactly as an accepted request does. A
`start` while a rotation is already open, a `drop-old` outside `switched`, and
a `rollback` with nothing in progress are all refused this way, each with a
`RotationRequestRefused` warning naming the phase that refused it. That event
is the only trace a refusal leaves: within one tick the annotation itself is
gone. Asking again means setting the annotation again.

A value that is none of `start`, `drop-old` or `rollback` is the one
exception: it is left in place and does not halt whatever step was already due
— the sequence keeps advancing on its own schedule regardless
(`AdvanceRotation`'s default case). Because it is deliberately never consumed,
its `RotationRequestUnrecognised` event fires on **every** tick for as long as
the value sits on the secret — every 30 seconds while a rotation is in flight.
Correcting or deleting the annotation is what stops it.

**All of the above has been driven once, end to end, on `paulwtf`, on
2026-08-22.** There is no `docs/runbook-…-evidence.md` for it; this paragraph
is the record. `start` → `distributing` with both CAs published while the
serving certificate still chained to the old one → the gate passed → the
operator switched on its own → the hold → `drop-old` by hand. The end state,
re-read from the cluster the same day:

- `spawnery-agent-tls` carries exactly four keys — `ca.crt`, `ca.key`,
  `tls.crt`, `tls.key` — and **no annotations at all**, so `drop-old` cleared
  the slots and all three rotation annotations as described.
- `ca.crt` is the CA minted at `start`: `notBefore 2026-08-22T15:19:55Z`,
  subject `CN=spawnery-agent-ca`, ten-year lifetime.
- The serving certificate was re-signed at the switch —
  `notBefore 2026-08-22T15:32:31Z` — and its Authority Key Identifier equals
  the new CA's Subject Key Identifier byte for byte
  (`27:A3:51:…:D1:E1`), which is the check that distinguishes a real switch
  from a re-published old certificate.
- `minecraft`'s `spawnery-ca` ConfigMap holds exactly one certificate, so the
  overlap is closed everywhere the bundle reaches.
- **The three agent pods in `minecraft` have `restartCount` 0 and start times
  of 2026-08-20** — older than the rotation by two days and unrestarted
  through all of it. That is the whole point of the overlap, measured rather
  than argued: `gateway-mcv4`, `gateway-pmmy` and `lobby-hktx` re-read `ca.crt`
  from their projected volume across session deadlines and never noticed.

One rotation is one rotation. What it establishes is that the sequence
completes and that agents survive it; it establishes nothing about a fleet
larger than three pods, about a gate that actually blocks, or about any of the
refusal and slot-repair paths above, none of which were exercised here.

**The operator repairs or throws away a hand-edited rotation slot, and it can
end the rotation while doing so.** On every tick, *before* it looks at
`rotate-ca` at all, `AdvanceRotation` re-reads `ca-next.crt` and
`ca-previous.crt` out of the secret and checks that each is exactly the PEM
encoding of one certificate — those bytes are published verbatim into every
namespace's `spawnery-ca` ConfigMap, and an agent that cannot parse the bundle
loses its entire trust store, not merely the slot. A slot that fails the check
is dealt with there and then, and that is the tick's one step: a `rotate-ca`
set at the same moment is **not** consumed, and is picked up on the next tick
against the state the cleanup left. Only a hand-edited or truncated secret
reaches this; nothing the procedure itself does can.

Which of the two happens turns on one question — **is the slot's first PEM
block a certificate?**

- **Yes: the slot is repaired.** It is truncated to that first block, with a
  `RotationSlotTruncated` warning. This is what a pasted chain, or a stray line
  before or after the certificate, produces. The operator was already signing
  with that first block, so nothing usable is lost, no phase moves, and the
  rotation carries on. It is not cosmetic: a surplus block that happens to be a
  valid certificate is loaded by every agent as another CA it will accept the
  operator's identity from, and nothing else would ever say so.

  It also fires on a difference you would not call damage. The test is
  byte-exact, so a certificate that is merely re-wrapped to a different column,
  saved with CRLF line endings, or left with a blank line at the end is
  repaired too — same warning, same record, and while `distributing` the same
  restarted window. One rule covers stray bytes and a surplus certificate
  because separating them would mean trusting that stray bytes never happen to
  contain a PEM header, which is not a property worth resting a fleet on. The
  operator's own writes are always canonical, so this only ever follows a hand
  edit, and it happens once: the repaired slot is canonical and is not touched
  again.
- **No — not PEM at all, or a PEM envelope around something that is not a
  certificate: the slot is cleared**, with a `RotationSlotDiscarded` warning.
  The bytes are gone; they are kept nowhere. What becomes of the rotation then
  depends on the phase:
  - **`ca-next.crt` while `distributing` — the rotation is abandoned.** The end
    state is the one `rollback` out of `distributing` produces: no slot, no
    phase, `ca.crt` published alone. Nothing usable had been distributed, and
    every agent trusted `ca.crt` throughout. Start again when ready.
  - **`ca-previous.crt` while `switched` — the drop is completed.** This is the
    procedure's one irreversible step, performed without anyone asking, and it
    is the behaviour worth having read *before* meeting it. The hold at
    `switched` exists for exactly one purpose — that a `rollback` stays
    possible — and a rollback signs with the previous CA's bytes
    (`RestorePrevious` → `Reissue` → `parseCA`). A slot only reaches this
    branch when those bytes will not parse, so the rollback was already
    impossible; clearing the slot records that rather than causing it. Nobody
    is stranded: the serving certificate chains to the new CA, which every
    agent came to trust during the overlap. But the bytes are gone once the
    operator has acted, and while a rotation is in flight it acts within 30
    seconds — so a `ca-previous.crt` damaged by a slipped paste leaves about
    one tick in which to put it back. After that the old CA can only come from
    a backup of the secret, not from the operator.
  - **Anything else** — a `ca-previous` sitting on a `distributing` secret, or
    either slot on an idle one — is cleared and reported, and a rotation that
    did not depend on it carries on untouched.

**Repairing `ca-next.crt` during `distributing` also deletes
`spawnery.cloud/ca-rotation-since` and re-runs the gate.** The wait starts
over, and `ca-rotation-blocked-on` may reappear naming namespaces that had
already caught up: the gate had passed against bytes that are no longer in the
slot, and if what was pasted in front of the certificate is a different CA,
switching to it would strand every agent in the fleet. Expect to lose up to the
quarter of an hour again. A `ca-previous` repair at `switched` does not do
this — there is no window there, and `since` means the age of the hold.

**`spawnery.cloud/ca-rotation-discarded` is the durable record of all of the
above**, and it is where to look once the events have expired. It carries the
slot, the parse error, whether the slot was cleared or truncated, and the time
— everything except the bytes, which are the one thing that must stop existing.
It is one annotation for both outcomes, and two slots touched in one step are
two entries in one value, `;`-separated, with a single timestamp at the end:

```
spawnery.cloud/ca-rotation-discarded: ca-next.crt: not PEM (2026-08-21T14:02:11Z)
spawnery.cloud/ca-rotation-discarded: ca-next.crt: more than its first PEM block; truncated to that block; ca-previous.crt: parse certificate: x509: malformed certificate (2026-08-21T14:02:11Z)
```

**It is removed only by the next accepted `start`.** Neither `drop-old` nor
`rollback` touches it, and neither does a refusal; a later repair or clearance
overwrites it with that step's own entries. So a record on a secret with no
phase set describes the last thing that happened to a slot, which is not
necessarily anything to do with the rotation that has just finished — check its
timestamp before reading it as news.

Eight reasons appear as events on the secret — `RotationStarted`,
`RotationBlocked`, `RotationSwitched`, `RotationCompleted`,
`RotationRequestUnrecognised`, `RotationRequestRefused`,
`RotationSlotDiscarded` and `RotationSlotTruncated`
(`internal/certs/events.go`); `rollback` alone ends a rotation with no event of
its own, since the human-written annotation already says what happened. All of
them expire out of the API within the hour, which is why the last two have
`ca-rotation-discarded` behind them. Two gauges track it from outside
(`internal/certs/metrics.go`): `spawnery_ca_rotation_phase`, 1 for the current
phase and 0 for the others so "is anything rotating" is one query, and
`spawnery_ca_rotation_blocked_namespaces`, the count
`ca-rotation-blocked-on` is built from.

**A compromised CA key is still a different emergency, not this procedure.**
The overlap above is orderly precisely because it keeps trusting the outgoing
CA for the width of that wait — on the order of a quarter of an hour. That is
exactly what a compromise cannot afford to do. "Delete the secret, restart all
pods" stays the answer to that case.

**`controller-gen` silently ignores a `+kubebuilder:rbac` marker inside a doc
comment — no rule, no error.** The marker has to sit immediately before the
declaration it applies to; if it sits further up as part of that declaration's
comment block, no line appears in `config/rbac/role.yaml` at all. Task 10 walked
into this twice — the first attempt to add permissions for `secrets` and
`tokenreviews` produced no rule whatsoever, without comment. Anyone adding a new
marker should diff `config/rbac/role.yaml` afterwards rather than just watch
`make manifests` go green.

**On Darwin the envtest binaries come from the controller-tools releases, not
from nixpkgs**, with one hash per version checked into `flake.nix` (design,
section 3). A new Kubernetes version in the nixpkgs channel — the Linux path —
does not bring that hash along; it has to be updated separately for Darwin, or
the two development environments run different `kube-apiserver` versions against
the same suite with nothing indicating it.

## Small things

- `ObjectRef` is a non-pointer struct without `omitempty`, so a `required`
  marker on fields of that type never applies; the rejection effectively comes
  from `MinLength=1` on the name.
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
