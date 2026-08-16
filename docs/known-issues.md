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

## Preconditions for milestone 2c (the Kotlin agent) — met

The three obligations below are discharged. They are kept rather than deleted,
because the reasoning behind each is what a second agent — Velocity, in
milestone 3 — has to satisfy all over again, and only the reasoning makes that
legible.

**The Kotlin agent must reconnect with overlap.** On connect the operator
announces with `SessionDeadline{renewAfterSeconds, hardDeadlineSeconds}` when it
will close the stream hard (480/600 seconds today). If the agent does not open a
new stream before `renewAfterSeconds` while the old one is still running, every
server drops out of `Ready` on the rhythm of the hard deadline, deregisters from
the proxies and collects a readiness loss — a home-made flap counter (design,
section 7.1). `internal/agentserver` only supplies the operator half of this
(`Registry.Supersede` carries the readiness of a superseding stream over);
without an agent that actually reconnects before the deadline it has no effect.

*Met* by `SessionLoop`, which opens the replacement stream and only retires
the outgoing one once the operator has answered on the replacement. It lived
in `agent/paper/src/main/kotlin` when this precondition was written; milestone
3c's Gradle split moved it to `agent/common/src/main/kotlin` so the Velocity
agent could run the identical loop rather than a copy of it, and every claim
below about its behaviour still holds of the shared class. That last clause is not decoration: an earlier
version retired on the local `Hello` call, which travels an established
connection and therefore beats a greeting that still needs TCP and TLS, so the
operator saw `leave()` then `enter()` — a `Disconnect` followed by a `Connect`
rather than a `Supersede` — and every server would have dropped out of `Ready`
on every renewal. Phase 1 of `make agent-test` is the standing proof, on the
operator's side of the wire.

**`Hello{ready:false}` cannot lower a readiness that was once set.**
`registry.MarkReady` is only called on `Hello{ready:true}` and on the standalone
`Ready` message; `Hello{ready:false}` is a no-op for the registry. So an agent
cannot report itself as "connected, but no longer ready" without dropping the
connection — only a real break or a supersede lowers readiness. It gets sharper
in a second place: if a stream really breaks before its teardown path runs, and a
new one supersedes it in the meantime, `Supersede` carries the old readiness
forward — possibly that of a process that has since restarted. For the Kotlin
agent that means: "briefly not ready, but still connected" cannot be expressed
with this contract.

*Met* by not needing it: the Paper agent latches readiness once the server is
up and never lowers it, so the gap in the contract is never reached. A Velocity
agent that wants to express "draining, still connected" will reach it.

**The interceptor reads `Bearer ` character for character, and `RoleForMethod`
matches the method on its suffix.** `internal/grpcauth/interceptor.go`
recognizes the prefix `"Bearer "` exactly like that — not `bearer`, not `Bearer`
with two spaces — and the method mapping looks at the end of the full gRPC method
name (`.../ServerSession`). Both fail closed: a mismatched spelling leads to "no
token", an unknown method to no role at all and therefore to `Unimplemented`
before the handler runs. That is not a hole, but the first half is an obligation
on the Kotlin agent's side, exactly like the overlapping reconnect: whoever
assembles the header themselves has to set it character for character, or every
session is rejected without the cause being visible in the error.

*Met* by `BearerCredentials`, and checked twice: once in the plugin's own unit
tests, and once byte for byte by the stub operator in `make agent-test`, which
compares the header it received against the literal string rather than parsing
it.

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

**`fsGroup` was missing — closed in 5a, before the first persistent server
shipped.** For ephemeral groups this never bit: the kubelet creates an
`emptyDir` world-writable, so uid 10001 writes into `/data` fine. A PVC
arrives owned by root, and uid 10001 does not. This entry tracked the gap
from milestone 2b, when only ephemeral groups existed and the risk was
theoretical, through milestone 5a, the first milestone that could create a
persistent server — the fix landed inside 5a rather than after it, so no
persistent server has ever shipped without it.

`BuildServerPod`'s `PodSecurityContext` (`internal/podspec/server.go`) now
sets `FSGroup` to `10001` — `nix/oci-common.nix`'s `uid` and `gid` for the
image, both the same value — and `FSGroupChangePolicy` to
`OnRootMismatch` rather than the kubelet's own default of `Always`: `Always`
walks and `chown`s the entire volume on every pod start, a real cost against
a large Minecraft world, where `OnRootMismatch` pays that cost only when the
volume's top-level directory ownership doesn't already match, which is
precisely the case a freshly bound, root-owned PVC starts in. It is set for
every server pod, ephemeral groups included, rather than only persistent
ones: one `PodSecurityContext` shape for both group types is one fewer thing
to keep in sync, and the kubelet's ownership check costs nothing extra
against an empty, freshly created `emptyDir`.

What this closes: on a storage class that hands back a claim owned by root at
a narrower mode than world-writable — most CSI drivers backing a real cloud
volume — uid 10001 can now write into `/data`, where before this fix it could
not, and Paper would fail to start with nothing on the `Server` object saying
why beyond a generic startup-deadline failure.

What this does not close, and cannot from `internal/podspec` alone:
`envtest` runs no kubelet, so nothing at that layer observes the ownership
change actually happening — `internal/podspec/server_test.go`'s assertions
confirm only that the pod spec asks for it, which is as much as that layer
can ever show. Confirming the chown itself takes a real cluster and a
storage class that does not already hand back a world-writable directory:
`kind`'s default local-path provisioner runs `mkdir -m 0777 -p "$VOL_DIR"`
when it provisions a volume (verified against
`rancher/local-path-provisioner`'s own `local-path-storage.yaml`), which
means a `kind` cluster can never exercise this fix — the directory it hands
back is already writable by any uid regardless of `fsGroup`. That
verification has not happened yet and belongs to whichever run first tries
this against a storage class that does not do that.

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
is what `make image-test` relies on. It matters twice downstream: the egress
NetworkPolicy in milestone 6 has to allow both, and the Yggdrasil call
disappears on its own once milestone 3 flips `online-mode` off.

**The image is large because `jdk25_headless` has a 697 MiB closure** — a full
headless JDK, not a JRE. Measured at 724 MB as a tarball for 26.2-0.1.0, and
735 MB for 26.2-0.2.0 once the agent joined it (a little over a gigabyte
unpacked, as Podman reports it). There is no cheap substitution: this pin's
`temurin-jre-bin` stops at 21, and Paper 26.2 needs 25 or newer, while
`jre25_minimal` is `jlink` with `modules = [ "java.base" ]` by default and
therefore needs a module list nobody has yet. The right time to derive that list
has now arrived — see "The JRE module list is now derivable" below.

**`k3d` does not work on this machine, and probably not on similar ones.**
`docker` here is a Podman 5.8.4 alias with no `/var/run/docker.sock`, only a
rootless Podman socket. k3d's tools node always bind-mounts the runtime socket
to the fixed in-container path `/var/run/docker.sock`; rootless Podman refuses
to create that mount point (`mkdir /var/run/docker.sock: permission denied`),
regardless of `DOCKER_HOST`. There is no workaround short of a rootful Podman
socket, which this user does not have group access to. `kind` under
`KIND_EXPERIMENTAL_PROVIDER=podman`, wrapped in
`systemd-run --scope --user --property=Delegate=yes`, works against the same
rootless socket and is what the README now documents. This matters for
milestone 6, which wires a local-cluster E2E flow into CI: whatever runs CI
needs either a real Docker daemon, a rootful Podman socket, or kind the same
way this was done here.

**No image is published.** The tag `ghcr.io/spawnery/paper:26.2-0.2.0` is
correct but nothing pushes it, so every consumer needs `kind load docker-image`
(or `k3d image import`, where k3d works) or the equivalent. Publishing belongs
with CI in milestone 6.

**`/data/config` collides with Paper's own writable config directory.** The
main design's own §4.3 `ServerGroup` example mounts a ConfigMap at
`mountPath: /data/config` as the documented way to override configuration —
but that is also exactly where Paper itself writes `paper-global.yml` and
`paper-world-defaults.yml` on startup, and a ConfigMap volume is mounted
read-only. That mount is therefore likely to break the server's start, and
nothing in this milestone exercises it: the shipped sample carries no `mounts`
block, so the evidence run never touched the path. Milestone 3 has to reckon
with this collision when design §5.4's configuration rendering lands — a
narrower mount (a `subPath` per file, or a different target directory
entirely) is the likely shape of the fix, since `Mount`
(`api/v1alpha1/common_types.go:89-105`) has no `subPath` today.

*Met* by construction, not by a narrower mount. Design §5.4's rendering lands
the operator's own ConfigMap at `/etc/spawnery`
(`internal/podspec.ConfigMountPath`) — a path neither Paper nor Velocity ever
writes to — rather than at `/data/config`, which was the mount §4.3's old
sketch proposed and which this entry warned against. `spawnery-config` writes
`server.properties` and `config/paper-global.yml` straight into `/data`,
which Paper already owns and writes to itself; nothing renders a ConfigMap
there any more. The `subPath` fix this entry expected was never needed: the
mechanism that would have collided was replaced, not narrowed. A user is
still free to mount something of their own at `/data/config` —
`checkMountCollision` does not single that path out, only the exact `/data`
and `/tmp` roots and the reserved agent and config mounts — but that is now
an arbitrary user choice, not something the design's own documented override
path walks into.

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

**The operator's hard-deadline rescue is armed after its first two `Send`s, and
moving it up is necessary but not sufficient.** `internal/agentserver/server.go`
arms `time.AfterFunc(s.opts.HardDeadline, …)` at `:218`, below the `ReportInterval`
and `SessionDeadline` sends. An operator that stalls *before* its first `Send`
therefore never arms its own rescue, and the stream stays open forever. Moving
the `time.AfterFunc` above both `Send`s is worth doing — and it does not close
the case on its own, because `sessions.cancel` cancels the context *derived*
from the stream's (`streams.go:104-113`), not `stream.Context()`, so a handler
blocked inside `Send` never observes it. The agent no longer depends on either:
since milestone 2c it bounds an unanswered session itself, with the operator's
own `hardDeadlineSeconds` once it knows it and a finite fallback until then.
This is milestone 2a code and was deliberately left untouched by 2c.

**An operator that answers once and then stalls leaves a session with no
future.** The agent's answer bound is discharged by *any* first message, but
only a `SessionDeadline` gives the session a renewal. An operator that completes
the `ReportInterval` send (`server.go:200`) and then stalls before the
`SessionDeadline` send (`:207`) leaves the agent with an answered session, no
renewal scheduled, `hardDeadlineMillis` still zero — and, per the entry above,
its own `AfterFunc` never armed either. Narrow, but it is the one hole the
milestone's three-phase harness does not cover, because the stub either answers
fully or not at all.

**The relocation is not proven on the give-up path.** The cast to
`ClientCallStreamObserver` that the cancel needs sits inside a `runCatching`,
which catches `Throwable` — so a `NoClassDefFoundError` from a shading
regression would be swallowed and phase 3 of `make agent-test` would still pass.
Phase 3 being green is evidence that the bound holds, not that the cast resolves
under the shaded names.

**A permanently unreachable operator writes one WARNING every 30 seconds,
forever.** There is no rate limit and no deduplication on the reconnect log. One
line per pod per 30 s is nothing for one server and is a real log bill for a
fleet that loses its operator for a day. `SessionLoop` owns the cadence; the
cheap fix is to log the first failure and then only on a change of cause.

**The JRE module list is now derivable, and milestone 3 is where to cut it.**
The Paper-side classpath stopped moving with this milestone: the agent is the
last thing that joins it, and gRPC and okhttp pull modules Paper alone does not.
So the list can finally be derived from the complete classpath, with `jdeps
--print-module-deps` or `-verbose:module` against a running server. Milestone 3
adds Velocity and faces the same question with the same answer, so derive it
once, there, for both images — see the image-size entry under milestone 2b for
what it buys.

**`deps.json` has no CI guard.** Nothing fails if `agent/paper/deps.json` drifts
from `agent/paper/build.gradle.kts`; the drift only shows up when someone runs
`make agent-deps` and gets a diff. The target cannot run inside a Nix build,
because it reaches Maven Central, so the check belongs with CI in milestone 6:
run `make agent-deps` and fail on a non-empty `git diff`.

**Two toolchain versions are pinned twice each, and a nixpkgs bump moves only
one half.** `protoc-gen-grpc-java` comes from nixpkgs (1.83.1 at this pin) while
the grpc-java runtime artifacts come from `deps.json`; `protoc` comes from
nixpkgs while `protobuf-java` (4.35.1, tracking protoc 35.1 one for one) comes
from `deps.json`. In both cases a nixpkgs bump moves the generator without the
runtime, and the symptom is a generated stub that does not match the library it
runs against. The failure is loud — `compileProtoJava` fails with "cannot find
symbol" — but it appears nowhere near the pin that caused it. `flake.nix` now
names the coupling at both edit sites, which is the cheapest half; the standing
check still does not exist, and belongs with the `deps.json` guard above.

**The level-2 harness has rough edges milestone 3 inherits.** `hack/agent-test.sh`
and `cmd/spawnery-stubop` are exactly what a Velocity agent will be tested with,
so what they do not check is worth writing down: stream indices `0` and `1` are
hard-coded in the overlap verdict; `seq` is record order and not arrival order,
which the verdict's wording overstates; two wait loops after `await_event` do not
check that the container is still alive; the three phases are a near-verbatim
copy of one another rather than one parameterised function, so what each varies
has to be found by eye; and the stub's own Go tests cover neither the
never-closes property nor the uniqueness of `seq`.

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

## Preconditions for milestone 3 (proxy integration) — fully discharged

All five original preconditions below are discharged: three by milestone 3a
(the operator's proxy side, 2026-08-10), and the remaining two — the
image-layer items — by milestone 3b (the Velocity image, 2026-08-11). They are
kept rather than deleted for the same reason milestone 2c's closed
preconditions are: the reasoning is what the next sub-project inherits, and
only the reasoning makes it legible. That next sub-project was 3c, the
Velocity agent, and it has now also landed (2026-08-11) — its own discoveries
are their own section, "From milestone 3c", below, in the same shape as "From
milestone 2c" above rather than folded into this one, because unlike 3a and
3b it is not itself a precondition of anything later. What 3a itself
discovered while closing its three follows after them, and what 3b discovered
while closing its own two follows after that.

**Factor the shared image Nix while there is still exactly one consumer.**
`nix/paper-image.nix` holds four things the Velocity image will need verbatim:
the `passwd`/`group` pair for the numeric user, the entrypoint's shebang
rewrite through `substitute --replace-fail`, the copy into `/usr/local/bin` at
the literal path a pod spec names, and the layered-image configuration around
them. The Velocity image also needs its own readiness check and the same
non-root story. Extracting a small `nix/oci-common.nix` before the second image
exists is much cheaper than reconciling two copies after they have drifted, and
the drift is the kind nobody notices — an image that starts fine while its user
or its paths quietly differ from the other one's.

*Met* by `nix/oci-common.nix`, extracted before `nix/velocity-image.nix`
existed at all. Both `nix/paper-image.nix` and `nix/velocity-image.nix` now
build on its `passwd`, `group`, `entrypointFrom`, `binIn` and `layeredImage`.
The store-path proof this entry originally expected turned out to be
structurally impossible in this repository — `spawnery-slp` is built with
`src = ./.`, so every tracked-file change moves every derivation's input
hash, including the extraction's own edits, and an identical path would have
been the surprise rather than the proof. What stands as evidence instead: the
pre- and post-extraction Paper image tarballs list identically, and the four
files `oci-common.nix` took over — `usr/local/bin/spawnery-slp`,
`usr/local/bin/spawnery-entrypoint`, `etc/passwd` and `etc/group` — have
matching `sha256sum` values across both images.

**Do not extend `set_property` for the forwarding mode.** It is a
`.properties` helper and it does not generalise: the forwarding secret and
`online-mode` live in `config/paper-global.yml`, which is YAML, and design §5.4
already commits the entrypoint to merging rendered configuration into that
file. Editing YAML from shell is the wrong tool. A small Go program baked into
the image is the right one — it reuses the `buildGoModule` path `spawnery-slp`
already establishes, it is testable the way `internal/slp` is, and it is the
natural home for §5.4's per-group ConfigMap rendering when that arrives. It is
also where the `/data/config` collision above has to be resolved, since that is
the directory the rendered configuration would land in.

*Met* by `cmd/spawnery-config`, a Go program baked into both images with its
logic in `internal/render`, tested the way `internal/slp` is. It resolves the
operator's rendered ConfigMap, a user overlay and the fields neither may move
into `server.properties` and `config/paper-global.yml` on the Paper side and
`velocity.toml` on the Velocity side, and refuses to start rather than guess
at a missing secret, a missing `maxPlayers` or an overlay that fails to
parse — naming the file and the key in every case. `set_property` and the
`mv`-based rewrite it used are deleted from `image/entrypoint.sh` outright,
not extended.

**The orphan sweep discarded proxy agents — met.** `OrphanReconciler.Sweep`
used to list pods with `spawnery.cloud/role=server` and then forget every
registry entry not in that list, so the first Velocity agent to open a session
would have been removed from the registry within one sweep interval.

*Met* by widening the filter: `Sweep` now lists by
`spawnery.cloud/managed-by` and restricts the server-existence check to
`role=server` (`internal/controller/orphan.go`), so a connected proxy's
registry entry survives a sweep the same way a connected server's does.

**`ProxySession` answered `Unimplemented`, and no bootstrap created the
`spawnery-proxy` ServiceAccount — met.** The contract from milestone 2a covers
both sessions completely (design, section 5), but through milestone 2c only
`ServerSession` was implemented and authenticated, and
`internal/controller.Bootstrapper` knew only the `spawnery-server`
ServiceAccount.

*Met*: `ProxySession` joins the fan-out and streams it back
(`internal/agentserver/server.go`), and `Bootstrapper.ensureServiceAccounts`
now creates both `spawnery-server` and `spawnery-proxy` in every namespace it
touches (`internal/controller/bootstrap.go`).

**`Register` was sent before `WasRegistered` was persisted — met.**
`applyDecision` called the registrar and only afterwards wrote
`status.wasRegistered = true`. If the status write were lost while players
were already joining, a deletion in that window would take the branch "never
registered → terminate immediately, no drain" — harmless while the registrar
was a no-op, real from milestone 3a on.

*Met* by reordering: `applyDecision` now persists `WasRegistered` with its own
`Status().Update` before calling `Registrar.Register`
(`internal/controller/server_controller.go`), so a crash between the two
finds the intent already durable.

What follows is what 3a discovered while closing the items above, and what
3b and 3c inherit as a result (design, §7 and §11).

**No lowerable readiness in the agent registry, and proxy drain will need
it.** `internal/agent/registry.go`'s contract cannot express "connected, but
no longer ready" — `Hello{ready:false}` is a no-op once readiness has latched
(see the milestone 2c precondition above). 3a lives inside that limit by
making a proxy's readiness startup-only: once ready, a proxy stays ready even
if its stream later breaks (design §3, §6.6). Proxy drain — stop accepting new
players while still serving the ones already connected — needs to lower a
readiness that was already reported, and the registry cannot do that today.
Milestone 4, which owns proxy drain, is where this becomes a change to the
milestone 2a contract rather than something worked around.

**A NetworkPolicy restricting backends to proxies-only is now overdue, not
merely deferred.** Design §7 left it out of 3a on purpose: built then, before
`online-mode` was off anywhere, the policy would have guarded an invariant
nothing yet relied on, and a green NetworkPolicy test would have looked like
proof of an isolation guarantee the servers did not actually have — they
still trusted whatever connected to them directly.

**As of this milestone that is no longer true.** `online-mode` is `false` on
the backend and `true` on the proxy (`internal/render.Paper` and
`internal/render.Velocity`, both asserted in one test per the design). The
invariant the policy would guard is real now: a Paper server authenticates no
one itself, so it trusts whatever opens a connection and completes the
modern-forwarding handshake with the right secret — and nothing today
restricts *who* can attempt that handshake to begin with. The only thing
still keeping the NetworkPolicy out is that milestone 6 owns NetworkPolicies
as a group (see the availability precondition below), not any remaining
reason to wait on `online-mode`. This is the entry in this file most likely
to be read as a formality — it is not. Until the NetworkPolicy lands, a
backend pod accepts a connection attempt from any pod in the cluster that can
reach port 25565, proxy or not.

**A proxy must report its configured player limit as `slots`, not zero.**
`Registry.ReportPlayers` rejects any report where `players > slots`; the
original proto comment said proxies leave `slots` at zero, which means a
proxy with even one player online would have every report silently discarded
— visible only as a `RejectedReports` counter — and
`ProxyGroup.status.connectedPlayers` would sit at zero forever while players
were connected. Design §8 corrects the comment: a proxy reports
`spec.config.playerLimit` as `slots`. The wire format did not move, only the
agreement about what goes on it, but 3c's Velocity agent has to honor the
corrected comment rather than the original one — 3a's own stub client already
reports the corrected way, and is the only thing today that would catch a
Velocity agent that did not.

**Whether the Velocity and Paper agents share a Gradle subproject was open;
it is now decided: yes.** `agent/common` will hold the session loop, the
token source, the channel construction, the credentials and the TLS-1.3
`ConnectionSpec` override that milestone 2c built for Paper. The cost is real:
the two agents can no longer be versioned apart, and 3c is where that
constraint has to be lived with rather than reopened.

**Where the forwarding secret reaches the backend is decided: a mounted
file, merged by a small Go program, not an extended `set_property`.** Velocity
points `forwarding-secret-file` directly at the mount; on the Paper side, a
Go program baked into the image merges `online-mode` and the secret into
`config/paper-global.yml`, reusing the `buildGoModule` path `spawnery-slp`
already establishes. This is the concrete answer to "do not extend
`set_property`" above, and 3b built it: `cmd/spawnery-config` is the Go
program, `internal/render` is where its logic lives, and the `/data/config`
collision above is resolved by moving the rendered ConfigMap's mount target
to `/etc/spawnery` rather than by narrowing the old one.

**Whether the operator runs inside the cluster for the E2E flow is still
open, and 3c's evidence run is where it starts to cost.** Today it runs
outside through `go run`, and the local kind flow hand-builds the `Service`
and `Endpoints` its own pods dial (see "The local kind flow needs a `Service`
nothing creates" above) — workable for one person at a terminal, a wall for
milestone 6's CI. An operator image is out of scope for all of milestone 3,
but 3c is where its absence first has to be worked around a second time
rather than once.

What follows is what 3b discovered while closing its own two preconditions,
and what 3c inherits as a result.

**The overlay's "refuse rather than guess" philosophy covers a parse failure
and a couple of named shapes, not the whole surface.** Both flavours refuse
an overlay that does not parse — bad YAML, bad TOML — and `paperGlobal`
refuses one whose `proxies` or `proxies.velocity` key parses to something
other than a mapping, "rather than treating either as an absent overlay" (its
own doc comment). Two shapes still slip through silently:

- A `paper-global.yml` overlay with `proxies` present but misspelled, or any
  other unrecognized top-level key, is valid YAML, matches nothing in
  `paperGlobal`, and does nothing. `server.properties` has the symmetric gap
  by construction: `parseProperties` accepts any `key=value` line, so a
  mistyped key just adds an unused one instead of being refused.
- A `velocity.toml` overlay whose `servers` key parses to something other
  than a table fails the `doc["servers"].(map[string]any)` type assertion in
  `velocityToml` and silently skips the `try` re-defaulting, rather than
  refusing the way `paperGlobal` refuses an equivalent wrong-shaped
  `proxies.velocity`.

In both cases the operator believes the override applied and the rendered
file does not reflect it — the exact failure this design's refusal exists to
rule out, just not ruled out everywhere it could be. No agent-side code in 3c
depends on this, so nothing forces a fix now; whoever next touches
`internal/render`'s overlay contract should close it with a real key check
rather than assume the existing refusals already cover it.

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
warns that it is sitting there orphaned. That is acceptable only for as
long as nothing is deployed against this code yet, which is true today and
is exactly the condition milestone 6 (Helm, the first thing that makes a
running upgrade real) will end.

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

**The adoption comment in `internal/podspec/labels.go` overstates how the
collision actually presents.** `GroupConfigMapName`'s doc comment says that
without the role suffix "the operator would silently adopt the user's
ConfigMap, inject config.yaml into it, and delete it on the group's own
deletion." That is only true when the user's ConfigMap already carries
`podspec.LabelManagedBy`: `cmd/spawnery-operator/main.go` narrows the
manager's ConfigMap cache to that label specifically so it does not have to
hold every ConfigMap in the cluster, so an *unlabelled* ConfigMap of the
colliding name is invisible to `CreateOrUpdate`'s `Get` — the reconciler
instead attempts a plain `Create`, which fails with `AlreadyExists`, and the
reconcile loops on that error rather than adopting anything. Nothing is
silent about it; it just does not say why on the CR. The comment's other
half, about `AlreadyOwnedError` on a cross-kind name collision (a
`ServerGroup` and a `ProxyGroup` sharing a bare name), is accurate — the
operator's own sibling controller writes the label, so the cache does see
that object. Whoever next edits the comment should scope the "silently
adopt" clause to the labelled case instead of stating it as universal.

**The proxy player-limit default is still spelled out twice.**
`podspec.BuildProxyPod` (`internal/podspec/proxy.go`) and
`proxyConfigValues` (`internal/controller/proxygroup_controller.go`) each
write the same guard, `cfg != nil && cfg.PlayerLimit > 0`, before falling
back to `podspec.DefaultPlayerLimit`. They share the constant but not the
predicate, and nothing asserts the two decisions agree. That gap is worth
recording because the disagreement between exactly these two code paths
*was* this milestone's one Critical finding: the controller used to leave
the ConfigMap's `playerLimit` nil whenever `spec.config` was nil, while the
pod's own `SPAWNERY_PLAYER_LIMIT` environment variable already defaulted to
500 — so a `ProxyGroup` with no `spec.config` came up Accepted, with its
Service, and every pod crash-looped forever reading `config.yaml:
playerLimit is not set`, with nothing on the CR explaining why. That
specific disagreement is fixed and pinned by
`TestProxyGroupWithNoConfigStillStartsAProxy`, which carries the
reconciler's own ConfigMap through `render.Velocity` the way
`spawnery-config` actually reads it. What is not fixed is the shape: the
default is still decided in two places, by two copies of the same
condition, with no test that would fail if a future edit changed one
without the other.

**`TestConfigPathsAgreeWithRender` pins three of the four path constants
`internal/podspec` and `internal/render` each name independently, not all
four.** The independence is deliberate — `podspec` must stay free of a
dependency on `internal/render` so building a pod spec never touches the
filesystem — and the test exists precisely because the two sides can only
agree by construction, not by sharing code. It asserts `configOverlayDir ==
render.OverlayDir`, `configSecretFile == render.SecretFile` and
`ConfigMountPath == render.ConfigDir`, but never `podspec.ConfigValuesKey
== render.ValuesFile`; the test's own doc comment still says "the three
places" where there are four literal pairs today. Both constants happen to
be `"config.yaml"` right now, so nothing is broken. A divergence here would
fail loudly — `spawnery-config` would refuse to start reading
`/etc/spawnery` with `config.yaml: not found` — rather than silently, which
is why this is a gap and not a hole; it is the same class of duplication as
`configOverlayDir`, whose divergence the test's own comment already
documents as silent. Closing it is a one-line addition to the existing
test.

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
`internal/render/testdata/paper-global.default.yml`), and an image test that
reads the file back out of the running container
(`hack/image-test.sh`) — but the class is what the next person needs to
carry forward: a green render test proves what was written, never what was
read.

**And the same two for Velocity**, which the whole-branch review found had
been missed: the lesson had been applied only to the flavour it was learned
on. `TestVelocityWritesTheKeysVelocityItselfReads` checks the renderer's key
names against `internal/render/testdata/velocity.default.toml`, extracted from
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

**A `configOverlay` can still inject unknown keys** into `proxies.velocity`.
`TestPaperWritesTheKeysPaperItselfReads` renders with a nil overlay, so the
"keys Paper reads" invariant is asserted for the base render only. The three
critical keys (`enabled`, `online-mode`, `secret`) are reasserted over the
overlay, so it is defensible — but it is the one path a `secret-key`-shaped
key can still take, and nothing would catch a second one arriving the same
way this one did.

**The ready port is spelled in two languages** —
`internal/podspec.ProxyReadyPort` and a Kotlin constant in
`agent/velocity/src/main/kotlin/cloud/spawnery/agent/velocity/AgentPlugin.kt`
— with no test that can compare them. Only the level-2 harness
(`hack/agent-test.sh`, phase four) catches a divergence, and only when it
runs.

**A proxy that cannot bind its ready port is silent on the CR.** It stays
`Pending` with the reason only in the container log
(`ReadyGate.open`'s own `log(...)` call). This is the same shape as the
`playerLimit` defect milestone 3b found and fixed, in a place where the
operator has nothing to write to.

**`SPAWNERY_FALLBACK_GROUPS` is a third spelling of the fallback list**,
after the CRD field `ProxyGroup.spec.routing.fallbackGroups` and
`DrainPlayers.toGroups`. `internal/podspec.BuildProxyPod` builds the
environment variable from the same CRD field the operator's own
`DrainPlayers` sender reads, so it cannot disagree today; nothing pins that
it will not.

**Per-proxy load balancing.** With several proxies, placement is even per
proxy and not necessarily across the network — `Router` counts only the
players Velocity itself can see, not what any other proxy in the same
`ProxyGroup` is carrying.

**Proxy drain still needs a lowerable readiness** in
`internal/agent/registry.go`. That is a milestone 2a contract change and
belongs to milestone 4, which owns proxy drain — see
`docs/handover-milestone-4.md` for what the change has to do.

**The NetworkPolicy is overdue, not deferred.** With `online-mode=false` on
the backends and forwarding now actually working, a Paper server
authenticates no one and trusts whatever completes the handshake with the
right secret — and nothing restricts who may attempt it. Milestone 6 owns
NetworkPolicies as a group. This entry is the one most likely to be read as a
formality; it is not.

**Smaller ones**, each worth a sentence: phase 5 of `hack/agent-test.sh`
reuses phase 2's window constants declared 400 lines
earlier, both derived from a hard-coded renewal interval; `streams_opened`
counts what the operator saw, so a proxy leaking a gRPC channel per reconnect
is still measured nowhere — the standing blind spot inherited from milestone
2c; `ServerDirectory`'s stale-removal path (`unregisterTracked`) logs nothing
at the point of removal, unlike every other mutation in the same class. Two
entries that stood here are closed: phase 1's empty-token comparison now
carries the same guard phase 4 does, and `Router.choose`'s fall-through when
the exclusion empties the first group is covered both by a unit test and by
the second fallback group `docs/runbook-milestone-3-evidence.md` §8a drains
into. Separately: `cmd/spawnery-join` asks a
server for its protocol version by announcing an unsupported one
(`announceUnsupported = -1`) and trusts that the proxy's newest supported
version and the backend's actual version agree — true of every pinned pair
this repository ships and not guaranteed generally; `internal/mcjoin`'s own
package comment names the failure mode (a loud "Outdated client!" naming the
version to fix it to), so it fails loud rather than silent, but the runbook
that depends on this tool inherits the same assumption. And
`config/samples/network.yaml`'s own top comment still describes a `ProxyGroup`
whose pod never turns `Ready` because "milestone 3c's Velocity agent" does not
exist — false as of this milestone; nobody has updated the sample's comment
to match.

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
- The drain's own exit condition is `internal/phase/phase.go:224`, inside the
  `Draining` case: `if !in.Occupied() { ... Reason: ReasonDrained, Message:
  "no players left" ... }`. `Occupied()` (`internal/phase/phase.go:146`) is
  `in.PlayersStale || in.PlayersOnline > 0` — and with a stale-held client
  Paper never counted, `PlayersOnline` is exactly zero.
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
   Paper starts counting it — the whole-branch review already on file
   established that this is two packet-id constants and one `case` in
   `holdOpen`, not a rewrite of the tool. Until that lands, criterion 9 can
   only be proven manually, with a real client — see
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

**None of this branch's many reviews caught this**, and it is worth saying
plainly why not, rather than filing it away as bad luck. The whole-branch
review correctly predicted that a held connection would be *counted* — on
the proxy side, in `status.connectedPlayers`, which is exactly right and is
what §6 of the runbook now proves. Nobody in any of those reviews asked the
complementary question: which side does the *drain's own* exit condition
read? The two counts live in different structs, are populated by different
agents, and were never checked against each other until an
actual `kubectl delete` on an actual held connection forced the question.

## Preconditions for milestone 4 (scaling and drain)

**The PodDisruptionBudget has no counterpart.** Milestone 1 delivers the
protection of occupied pods, but not the detection of nodes that have become
`unschedulable` along with a proactive drain, as foreseen in spec 5.1 and 7.
Until then the operator can block a node drain without being able to release it
again. Both belong in one milestone: 4c, which owns node drain.

**Terminating pods count as "process gone".** `isOccupied` treats a pod with a
set `deletionTimestamp` as free of sessions, even though the process is still
running during the grace period and players may still be connected. As a result
`minAvailable` drops by one for the duration of the grace period while the pod's
label still matches the selector. Not reproducible in envtest, because there is
no kubelet there — this needs a real cluster. Belongs to 4c, alongside node
drain, which needs the same real cluster to prove.

**Exponential backoff per group.** Spec 7 requires it along with the condition
`Degraded`/`CrashLoopBackoff` and stopping further attempts. Milestone 1 instead
has only an upper bound of one retained failure per group. Belongs to 4b,
alongside its per-group backoff on rolling-update failures.

*Met* by milestone 4d's `CountFailures` and `DecideBackoff`
(`internal/controller/backoff.go`): a counter on `ServerGroupStatus`
(`consecutiveFailures`, `lastFailureAt`) tracks the streak, `ConditionBackingOff`
reports the wait with the count and the remaining time in its message, and at
six consecutive failures the group sets `Degraded` with reason
`CrashLoopBackoff` and creates nothing further until `metadata.generation`
moves. It shipped as its own sub-milestone rather than folded into 4b — cut out
during 4b's own brainstorm on the measurement that it shares no code with the
rolling update — but it is the same backoff this entry pointed at. The bound
this entry named, "an upper bound of one retained failure per group," is
`maxRetainedFailures = 1` and it stays: it still caps the footprint of a
failure, not the rate of retrying after one, which is now bounded separately.
See `docs/superpowers/specs/2026-08-13-per-group-backoff-design.md`.

**`ProxyGroupReconciler.pods()` has no expectations tracking — half of this is
now closed.** The `ServerGroup` side is: `internal/controller/expectations.go`
gives `ServerGroupReconciler` create and delete reservations keyed by name, so
a reconcile its own create event triggers no longer reads a stale cache, sees
the group still short, and creates a second server. `ProxyGroupReconciler.pods()`
is untouched — it still lists pods through the manager's plain cached `List`,
with no reservation for a create it just issued, so a reconcile the group's own
pod-create event triggers can still create a second pod and have the next
reconcile delete the surplus once the cache catches up. This now belongs to
4c, the milestone that makes proxy replica counts move: the mechanism to copy
already exists in `expectations.go`, so 4c does not have to design it again.

*Met* by milestone 4c-2, which copied it rather than designing it again, as
this entry expected. `ProxyGroupReconciler` now carries an `Expectations`
field, `pods()` observes the group's live pod list through
`expectations.observePods` — a second, narrow method beside `observe`, because
a proxy has no per-pod CR and therefore no retire reservation to model — and
`reconcileReplicas` subtracts what is already reserved from `DecideRollout`'s
create count and reserves each create and delete it carries out. The
subtraction is arithmetic rather than a bare gate, matching what
`ServerGroupReconciler.size` does through `DecideSize`: capping at zero the
moment anything is pending would also block a create the group still
legitimately needs beyond the one already reserved. 4c-2 is also what made the
race ordinary rather than rare, since a rollout creates a pod per replacement
instead of only at scale-up.

**Orphaned `Server`s without a pod.** The sweep covers "pod without CR" and
"server without group", but not "CR without pod": that is handled by the state
machine through `PodLost`, which only applies once `status.podName` has been
written. A server that never got a pod would stay in `Pending` and occupy its
slot. Belongs to 4c.

## Closed by milestone 4a

**Nothing bounded the reported `slots` against the group's `maxPlayers`.**
`internal/agent/registry.go` rejected a player count above the reported
`slots`, but checked `slots` itself against nothing, even though the operator
knows the upper bound and handed it to the pod itself as `SPAWNERY_MAX_PLAYERS`.
Once `slots` started feeding a scaling decision this milestone, a compromised
pod reporting `slots: 1000000` at zero players could have made its whole group
look permanently spacious and suppressed every scale-up for all of its
servers — exactly the effect across pod boundaries that milestone 2a otherwise
rules out. Closed by `clampReport` in `internal/controller/candidates.go`: the
reported `slots` is clamped to the group's `maxPlayers`, and the reported
`players` is clamped to the (possibly lower) result, before either reaches
`DecideSize`.

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

**`provisionalCapacity` cannot tell "has never reported" from "the pod
vanished"** — both present as `Slots == 0`, because `Registry.Lookup` on an
unknown pod returns a zero snapshot. A server whose pod is gone is therefore
credited a full `maxPlayers` of capacity it does not have, for the seconds
until the Server controller fails it. The blast radius is under-creation for a
resync or two, and no invariant is touched. The obvious fix — testing `Stale`
before `Slots == 0` — would be a regression: a server that is genuinely
starting up is also stale and has never reported, and crediting it zero
reintroduces exactly the runaway `provisionalCapacity` exists to prevent. The
right signal is already on the view and unused here: `ServerView.SessionsGone`,
one line at the top of the function. It belongs with 4b's own work on
`scaling.go`.

*Met* by `54a2ef2`: `provisionalCapacity` now tests `ServerView.SessionsGone`
before the `Slots == 0` credit, exactly the one line this entry named, so a
server whose pod has vanished is no longer credited a full `maxPlayers` it
does not have. The obvious fix this entry warned against — testing `Stale`
instead — stays wrong for the reason given above, and is now pinned rather
than merely argued: `TestProvisionalCapacityStillCreditsAStartingServer` is
the regression guard that fails if `Stale` is swapped in for `SessionsGone`,
because a genuinely starting server is stale too and would then be credited
zero, reintroducing the runaway this rule exists to prevent.

What `SessionsGone` does not resolve: `servergroup_controller.go`'s
`SessionsGone: srv.Status.PodName != "" && (!podFound || podTerminal(pod))`
can still read true for a single resync for a server that has genuinely just
started, if the informer's cache has not yet shown a pod the API server
already created. A server caught in that window is credited zero for one
pass instead of the full `maxPlayers`, so `provisionalCapacity`'s sum reads
lower than the truth and `wanted` reads higher — over-creation for a resync
or two, against the pre-fix entry's own under-creation above, and the safer
direction of the two: a group with a server too many costs money, a group
with a server too few costs joins. It is the same shape of lag this flag
already carried before this fix gave it a second reader: `isOccupied` has
relied on it for the same reason since before 4b. It is worth naming here
because 4b's cold start is the first place this risk feeds a scaling
decision rather than only the
occupied-pod protection.

**`derivePhase` still measures readiness against `DesiredReplicas()`, and that
field's meaning changed under it this milestone.** Before 4a,
`DesiredReplicas()` in `api/v1alpha1/servergroup_types.go` was the size the
group ran at, so "ready replicas have reached it" meant the group was fully
up. Now it is only the group's floor: `DecideSize` can and does run the group
above it to cover `spareSlots`. `derivePhase` in
`internal/controller/servergroup_controller.go` never changed its comparison,
so a group scaled to five for spare slots with one server up and four still
starting publishes `status.phase: Ready` off that one. Defensible — the group
is serving — but it is no longer what the field used to mean, and the change
happened silently. 4b's rolling update, which needs to say "the new generation
is up" as something other than "one server somewhere is," will want the
distinction this milestone left unmade.

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

**The cold-start loop is only half-closed.** A broken new image fails, drops
out of `countsTowardSize`, and would be re-created every five-second pass
forever; 4b suppresses the cold start while a `Failed` server of the current
generation is retained, so the interval becomes `failedRetentionSeconds` (an
hour by default) instead of five seconds. Two things have to agree for that to
hold, and the second is easy to lose: the retention cap keeps one failure
(`maxRetainedFailures = 1`), so it has to keep *that* one.
`selectFailedForPruning` therefore prefers the newest generation, then the
oldest failure within it — an oldest-first rule alone always kept an inherited
older corpse and pruned the current generation's, which is the only one doing
the suppressing, on the same reconcile that observed it, and the loop ran at
time-to-fail. Anyone changing that ordering is changing this interval from an
hour back to a minute.

The suppression is a guard on 4b's own door, not backoff — it does nothing for
the floor rule, which has the identical loop and keeps it.
`maxRetainedFailures = 1` still caps only the footprint of the failure, not the
rate at which the group tries again. Real per-group exponential backoff with
the `Degraded` condition (master design §7) belongs to its own spec, which is
next.

*Met* by milestone 4d's per-group backoff, which subsumes this suppression
rather than fixing its ordering further. `CountFailures` and `DecideBackoff`
(`internal/controller/backoff.go`) count consecutive failures on
`ServerGroupStatus` and gate the create call site in
`ServerGroupReconciler.size()` with a window that starts at ten seconds and
doubles, rather than this suppression's flat `failedRetentionSeconds` hour
after any single failure. The `coldStart` branch this entry describes — the
one counting a retained current-generation `Failed` server — is removed,
along with the tests that pinned it, and the loop it closed is now closed by
the backoff instead: `TestGroupWithABrokenNewImageDoesNotRebuildEveryPass`
holds a group with a permanently broken new generation to at most 3 servers
across 10 five-second passes, and fails without the backoff gate (20
servers built against that bound) — the mutation was run, not assumed.
Restoring the removed suppression itself was also tried, and fails only the
rewritten tail of `TestOccupiedServerSurvivesAContinuousScaleDown`, so that
test is what now pins the removal.
`maxRetainedFailures = 1` stays, and still bounds only the footprint — one
corpse retained for diagnosis — not the rate; the rate is what the backoff
now bounds. `selectFailedForPruning`'s newest-generation-first ordering,
which this entry's own fix made load-bearing for the suppression, stays too,
for the reason it always had independent of the suppression: the first
failure after a generation bump is the one that says what broke, and the
previous generation's corpse says nothing about the new image. See
`docs/superpowers/specs/2026-08-13-per-group-backoff-design.md`.

## From milestone 4d

**A group whose `minReplicas` is 6 or more gives up after a single round of
failures, with no retry at all.** The backoff's threshold
(`backoffGiveUpAt = 6`, `internal/controller/backoff.go`) counts failed
*servers*, not failed rounds. `size()` creates the whole shortfall in one pass
— for a group starting from nothing the floor term `minReplicas - alive` is
the entire floor at once — and `CountFailures` then counts every `Failed` view
in a single pass. So the budget of six is spent in `⌈6 / minReplicas⌉` rounds:
three attempts at `minReplicas 2`, two at 3, and exactly one at 6 or above.
Verified rather than reasoned about:
`DecideSize({MinReplicas: 6, MaxReplicas: 10, SpareSlots: 10, MaxPlayers: 100})`
returns `Create: 6`, `CountFailures` over those six same-instant `Failed` views
returns 6, and `DecideBackoff` turns that straight into
`GaveUp: true, MayCreate: false`.

The operational consequence is what matters here, because giving up is
terminal: a transient scheduler, registry or quota problem that fails a whole
floor's worth of servers at once takes a large group straight to
`Degraded`/`CrashLoopBackoff`, and design §3.5 makes the way back a spec edit
by a human — the group will not recover on its own however long the problem
lasted or how quickly it cleared. Nothing is lost and no player is
disconnected by this on its own; the group simply stays at zero new servers
until somebody touches it. If a group with a floor above one is found
`Degraded` shortly after a cluster-wide hiccup, this is very likely why, and
any spec change clears it (a `metadata.generation` bump is all the reconciler
looks at).

Design §3.6 and §5 are both narrated at `minReplicas 1`, where the schedule is
the intended one free attempt plus five growing waits. §3.6 now says so
explicitly and §11 carries the open design question — whether the schedule
should count rounds, or the threshold should scale with the floor. Neither was
decided in 4d, and changing it is a behaviour change, not a fix.

**Which message an operator sees first when a group has both given up and a
dead `Network` is unpinned.** The `BackingOff`/`Degraded` switch in
`ServerGroupReconciler.Reconcile` tests `!sized` before `backoff.GaveUp`, so a
group whose `Network` died while it was also six failures deep gets "backoff
is not being decided: the group's network is not usable" rather than "not
retrying: change the group's spec to try again" — even though the failure
count that produced `GaveUp` is computed from the views before `sized` is
known, and does not depend on the `Network` at all. Moving the `!sized` case
after `GaveUp` in the switch leaves the whole suite green: both messages are
true of a group in that state, which is likely why nothing distinguishes
them, but which one an operator should see first is a real question and
nothing checked in answers it. Worth a deliberate ruling, and a test that
pins whichever order is chosen, rather than leaving the switch's current
order as an accident of how it was written.

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
version-gates the message.** Nothing in the session says which agent build is
on the other end — `Hello` carries no version — so the operator cannot detect
this case, and there is no condition and no event that tells "old agent" apart
from "busy proxy". The other order is the safe one, and so is rolling the
operator back on its own: an agent that supports `SetReady` and never receives
one behaves exactly as 3c's did, because `ProxyRole`'s latch starts at
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
same shape on `derivePhase` and `DesiredReplicas()`, and that one is still
open. Both were found by reading a comment or description against what the
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

**Smaller ones**, each carried out of this milestone's task reviews rather than
fixed in them:

- The drain-deadline event reports the *last known* player count, which is a
  floor and not a measurement. `Registry.Lookup` hands back a stale snapshot's
  numbers, so a proxy whose agent died with seven players on it is announced
  with whatever it last reported, and one whose agent never connected at all is
  announced as `0 player(s)` inside a `Warning` about disconnecting people.
  Nothing reads that number today, which is what keeps it cosmetic; the first
  reader — a metric, a condition, a decision — turns it into a wrong input.
  "up to N", or no number at all, are the two rewrites on offer.
- `drainingSince`'s parse error aborts the whole `Reconcile`, on every pass,
  over one pod's corrupt annotation, while every neighbour in the same loop
  tolerates a single pod's bad state: `Create` tolerates `AlreadyExists`,
  `Delete` and `markDraining`'s patch tolerate `NotFound`, and `Fleet.SetReady`
  returns nil for a pod it has no session for. Nobody writes that annotation
  but the operator, so reaching it takes a hand edit; the objection is the
  blast radius, not the likelihood.
- The deadline event is emitted before the `Delete`, so a failed delete
  re-announces it on the next pass, and a `NotFound` announces lost sessions
  for a pod that was already gone.
- Two assertions in `hack/agent-test.sh` are argued rather than demonstrated:
  the control probe on 25565 that follows the closed-gate assertion, which
  needs the container to die mid-phase to be shown failing, and the post-loop
  arm of phase 4's withdrawal guard, which needs `port_open` to answer while
  `set_ready_sent` is already non-zero — a sub-second window on a correct
  agent.

**Two facts about the machine this milestone was built on, measured
2026-08-14.** `docker` here is a symlink into `podman-docker-compat`, so the
`Makefile`'s `CONTAINER ?= docker` default already runs Podman and
`CONTAINER=podman` changes nothing; and `/tmp` is not a tmpfs (part of root,
649 G free), so the `TMPDIR` prerequisite a small tmpfs would make necessary
does not apply here. Both are stated conditionally where they matter —
`docs/runbook-milestone-3-evidence.md` §0 always did, and
`docs/handover-milestone-4b.md` was corrected during this milestone — because a
machine where either is false still needs the override. Neither is a property
of the repository, which is the reason to date them.

**An unchecked question about milestone 3's runbook, which must not be repeated
as a finding.** Measured 2026-08-14 against Go 1.26.5 in this repository's
devshell: `go run` does not forward `SIGTERM` to the binary it compiled, so
`pkill -f` on the wrapper leaves the compiled child running and reparented, and
the operator goes on reconciling.
`docs/runbook-milestone-4c1-evidence.md` says so and kills the child.
`docs/runbook-milestone-3-evidence.md` cleans up the same `go run` with
`kill %1`, and whether that has the same problem is **not known**: `kill %n`
addresses the *job*, which in some shells means the job's whole process group,
and signalling the group would take the compiled child down along with the
wrapper. Two possibilities, one of them benign, and nobody has run it in the
shell milestone 3 was actually run in. The 4c-1 measurement is about `pkill -f`,
which signals only the process it matched, and it does not settle the other
case — evidence consistent with a conclusion is not evidence that establishes
it, which is a lesson this milestone learned three separate times.

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

**A deferred structural fix to `readinessDivergence`, recorded so that it is a
decision rather than an omission.** An entry in that map measures how long a
pod has been diverging *while something was watching*, so a pass that does not
call `observe` for a group must not leave a first-seen timestamp behind to fire
the moment observation resumes. Three of `Reconcile`'s steady-state early
returns handle that by calling `forget` explicitly — `NetworkNotFound`,
`NetworkNotAccepted` and `ExposeNotImplemented`, each of which returns before
`reconcileReplicas` runs while the group itself still exists. Every error
return above `reconcileReplicas` leaves the identical shape and does not
forget: a `ProxyGroup` read that failed for any reason other than the object
being gone, a failed `Network` read, the status write, `Bootstrap.Ensure`, the
ConfigMap, the `Service`, and the first `pods()` call — seven at the time of
writing, which is the point rather than a figure worth maintaining. Those are
transient by nature and the cost is bounded the same way — a report delayed
or, in the other
direction, a stretch of unwatched time counted as if it had been watched, up to
one grace period. The better fix is structural and was deliberately not made
mid-milestone: have the entry carry when it was *last observed* rather than
only when it first diverged, and treat an entry unobserved on the previous pass
as void. The property is then enforced by the type instead of by remembering to
call `forget` at each new exit, and the next person to add an early return to
`ProxyGroupReconciler.Reconcile` does not have to know this rule exists.

## From milestone 4c-3 (node drain)

**An operator running cluster-autoscaler must pass `-drain-taint
ToBeDeletedByClusterAutoscaler`, or a scale-in is invisible to this operator
until something else cordons the node.** `IsDeparting` (`internal/controller/nodes.go`)
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
`Accepted`, and an `expose.type` this milestone refuses — and each of them
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
after a real outage, and nobody has measured it; 15 seconds is a number
chosen for being finite, not one derived from the agents' own behaviour. It
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
`PreferNoSchedule` taint does not stop the scheduler putting a replacement
pod straight back on the same node, so matching on it would condemn a pod,
rebuild it in place, and condemn it again next pass. That correctness comes
at a cost this operator never reports: a key configured with an effect it
ignores — a real taint on a real node, `PreferNoSchedule` or any future
effect Kubernetes adds — simply never matches, silently, with nothing on any
group's conditions or events distinguishing "this taint does not apply" from
"there is no such taint at all". Nor is the key itself checked against
anything — a typo in `-drain-taint` is indistinguishable from a taint key
that legitimately does not exist in this cluster. An operator relying on a
taint to drain a node should confirm independently, with `kubectl describe
node`, that the taint is present with an effect this operator honours; there
is no warning if it is not.

## Preconditions for milestone 5 (persistent groups)

If a server's `ServerGroup` is missing, the server controller carries on with
ephemeral fallback timings. For a persistent server those are the wrong
deadlines. 5a did not touch `fallbackGroup` (`internal/controller/server_controller.go`),
which still stamps `Type: ServerGroupEphemeral` unconditionally; this
precondition is therefore still open.

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
the volume rather than failing cleanly. This is the mirror of the squatter
entry below — there an object holds the *name* without the ordinal, here it
holds the *ordinal* without being reachable. The tell is the same shape:

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

`ReasonNotImplemented` (`api/v1alpha1/common_types.go`) has no user left in the
codebase after this milestone: grepping `.go` and `.yaml` finds the identifier
only at its own definition, and the string `NotImplementedInThisVersion` only
in a test comment describing the block that was removed and in
`docs/superpowers/plans/`, which is a historical record rather than live code.
The constant is kept rather than deleted: it costs a line, and it is the exact
string an operator meets on the stale condition above, so removing it would
make that string unsearchable in the repository it came from.

**A `Persistent` group with `replicas: 0` reports `Pending` forever.**
`derivePhase` (`internal/controller/servergroup_controller.go`) returns `Ready`
only when `readyReplicas >= DesiredReplicas()` **and** `readyReplicas > 0`, so a
group that has been asked for zero servers, and correctly has zero, never
reaches `Ready`. The shape predates this milestone — it is the same arithmetic
an ephemeral group with `minReplicas: 0` meets — but 5a is what makes zero a
deliberate operator action: `spec.replicas: 0` is the accepted way to park a
persistent group and keep its claims, since deleting the group would leave the
claims behind but take the ordinals' `Server` objects with it. Such a group
publishes `Accepted: True`, `Degraded: False`, `replicas: 0`,
`readyReplicas: 0` and `phase: Pending`, which is the truth about every field
except the phase. Nothing else is affected: no condition depends on the phase,
and the group is sized, PDB'd and reconciled as normal.

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
else, because no pod means no `status.startedAt`, so the startup deadline never
arms, the server never reaches `Failed`, and the per-group backoff never counts
it. The phase stays `Pending`.

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

**A squatter can stall an ordinal silently.** `DecidePersistentSize` decides
which ordinal is missing by reading `ServerView.Ordinal`, sourced from
`spec.ordinal` — never by parsing the candidate name. If some other object
already holds a persistent ordinal's exact name (`<group>-<ordinal>`) without
carrying `spec.ordinal` — created by hand, or left over from something else —
that object never appears in `DecidePersistentSize`'s `held` map, so the group
goes on believing the ordinal is missing forever. `createPersistentServer`
(`internal/controller/servergroup_controller.go`) then retries the `Create`
every pass, gets `AlreadyExists`, and returns success without an error,
without an event ("No event: nothing was created here", by its own comment)
and without a log line. The reservation this creates
(`Expectations.expectCreated`) does not save the next pass either: `observe`
clears a create reservation the instant the name is `present` in the group's
own view list, and the squatter is present from the very first pass onward —
so the reservation never survives past the pass that made it, and the group
retries at its ordinary five-second resync cadence, forever. Nothing on the
group's conditions, events or logs distinguishes this from an ordinary
transient collision. The tell, today, is `kubectl get server <group>-<ordinal>
-o jsonpath='{.spec.ordinal}'` returning empty on an object that exists.

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
  `internal/controller/server_controller.go:348` and releases its finalizer at
  `:340`, after which the object is genuinely gone.
- A reconcile that fetched the object before that point — the reconciler reads
  through the manager's informer cache, which can hand back an object the API
  server has already removed — then writes to it and gets `NotFound` back.
- Whichever write it was, the error escapes unwrapped and `Reconcile` returns
  it. controller-runtime logs it at `error` with a stacktrace and requeues; the
  requeued pass fetches the `Server` at `:116`, where
  `client.IgnoreNotFound(err)` treats the absence as ordinary, and returns
  cleanly. Nothing retries into a loop and no object ends up wrong — the
  2026-08-16 run's `Server` was recreated within a second of the delete and
  reached `Ready` 22 seconds later, with the error line in between.

**Which write produced it is not determined by the log, and this entry does not
guess.** Three writes on this path can land on a vanished object, and **none of
the three tolerates `NotFound`**: the `Status().Update` after pod creation
(`:319`), the `Update` that releases the finalizer (`:340`), and
`applyDecision`'s closing `Status().Update` (`:685`, returned through `:333`).
The last is the likeliest, being the write most reconciles of a live server
reach — `:319` returns early on the pass that creates a pod, so it is not on
this path at all unless a pod was just created — but the log carries no line
number and nothing here distinguishes them. Anyone fixing this should reproduce it with a line-level log or a
breakpoint first rather than trusting this paragraph's ordering.

**The asymmetry worth noticing is between the deletes and the writes.** Both
`Delete` calls on this path carry an explicit `&& !apierrors.IsNotFound(err)`
— the pod delete at `:671` and the `Server` delete at `:348` — and none of the
three writes carries the equivalent. That reads as an oversight rather than a
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

**A permanently broken ordinal stalls the group's whole update.** §2's
invariant — at most one ordinal down at a time — is held by waiting for every
required ordinal to be `Ready` (Gate B) before a stale or resize-pending
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
count returns to 1, the corpse itself, rather than to 0. Defensible — a spec
edit does not heal a broken ordinal, and an operator watching for a stall
should not read a `1` as "nothing happened" — but it means the reset an
ephemeral group gets in full, a persistent one gets only most of.

**Fixed: a persistent group's failure counter used to freeze on any spec
edit.** `CountFailures`'s call site (`internal/controller/servergroup_controller.go:231-238`)
used to filter every group's views through `ofGeneration`, which keeps only
servers whose `spec.groupGeneration` equals the group's current
`metadata.generation`. `Server.spec.groupGeneration` is stamped once at
creation and never updated, and `DecidePersistentSize` is generation-blind by
design, so any spec edit on a persistent group bumped `metadata.generation`
and the filter then discarded every one of that group's servers —
`CountFailures` saw an empty slice, counted nothing, and
`status.consecutiveFailures` froze wherever it stood. A pre-existing defect
inherited from 5a, invisible until 5b gave persistent failures somewhere to
accumulate toward. Fixed by reading `ofGeneration` for an ephemeral group only
and passing the unfiltered views for a persistent one, and pinned by
`TestAPersistentGroupCountsAFailureAfterItsGenerationMoves`.

**Fixed: a squatter used to be able to stall a whole group's takedowns, not
only its own ordinal.** Gate A (`takedownInFlight`,
`internal/controller/persistent.go:222`) now skips any view whose `Ordinal` is
nil, the same as every other pass over `in.Views` in `persistent.go`. Without
that skip, an object squatting on a persistent ordinal's name without carrying
`spec.ordinal` — already recorded above, under "From milestone 5a", as
stalling *its own* ordinal — would also have held Gate A open forever, since
`leaving()` on such an object never resolves. That would have blocked every
takedown for the whole group, updates and scale-downs alike, indefinitely and
silently, rather than only the one ordinal the squatter occupies.

## Preconditions for milestone 6 (Helm, RBAC, E2E)

**`spawnery-system` is hard-wired into the RBAC markers.** The
`+kubebuilder:rbac` markers for the TLS secret (`internal/certs/store.go`) and
for the leases (`internal/controller/setup.go`) carry `namespace=spawnery-system`
as a literal. If the operator runs in another namespace, `controller-gen` still
produces a `Role` that binds in `spawnery-system` — the actual namespace stays
without secret and lease permissions, and the operator fails at its first
`certs.Ensure` or during leader election, without RBAC itself ever reporting
where the problem is. The Helm chart has to parameterize the namespace here, not
only in the object names.

**Completeness of the permission table.** The audit in `internal/rbacaudit`
catches drift between table and role. If a permission is missing from both, it
stays green — only the operator running under its ServiceAccount in a real
cluster proves that (level B of the E2E design).

**No `--leader-election-namespace`.** With the default flags, a local run
outside the cluster fails; `--leader-elect=false` is required.

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

## On the agent channel (`internal/certs`, `internal/agentserver`)

**The CA has no rotation procedure.** The bundle format of the CA ConfigMap is
deliberately open to several concatenated PEMs (design, section 6.2) so a later
rotation can run old and new with overlap — but today exactly one is ever
written, and the overlap path itself does not exist. If the CA expires in ten
years, or has to be replaced after a compromise, the only recipe is "delete the
secret, restart all pods". Even then a new CA does not reach every namespace
immediately: `Bootstrapper.Ensure` only runs before the server controller
creates a pod. An existing namespace where no new pod is created keeps the old
`ca.crt` in its ConfigMap until the next pod appears there.

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

## On the RBAC audit (`internal/rbacaudit`)

The audit checks the ClusterRole and the namespace-local Role in two directions:
one file based against the hand-maintained table, one through
`SubjectAccessReview` against the real authorizer in envtest. The redundancy is
intentional — the following points each concern only one of the two halves.

- **`apply()` obscures sources of error.** It tolerates `AlreadyExists` so the
  tests can share the cluster-wide objects. That makes the call in the manifest
  test effectively a no-op, because the permission test runs first and creates
  the objects. Anyone applying a *changed* ClusterRole silently gets the old
  one.
- **`ExpandRules` ignores `rule.ResourceNames`.** A name-restricted rule folds
  into an unrestricted permission. controller-gen never produces such a thing,
  and the SAR direction would catch it — hence deliberately left open.
- **The flags in the Deployment are unchecked.** `sigs.k8s.io/yaml` is not
  strict, so a mistyped key disappears silently. The spec requires
  `--startup-deadline=20s` for level B; no test guards that so far.
- **Nothing enforces that `Why` is filled in and `Required` is free of
  duplicates.** `Compare` collects duplicates, and the last one wins.
- **The `configmaps` grant's `Why` no longer names everything that uses it.**
  `internal/rbacaudit/required.go` documents `get`/`list`/`watch`/`create`/
  `update` on `configmaps` as `Bootstrapper.Ensure`'s CA ConfigMap alone.
  Since milestone 3b, `ServerGroupReconciler` and `ProxyGroupReconciler`
  create and update the same kind of object under the same grant. The
  permission itself is correct — the group ConfigMaps and the CA ConfigMap
  really do share one verb set — only the documentation trailed behind the
  second consumer.

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
  token the agent reads its identity from. It still does not check for two user
  mounts sharing a name — the API server catches that, but with a generic
  message instead of a clear operator error.
- "Keep the oldest failure of the newest generation" does not carry when two
  failures of one generation share a `creationTimestamp` (second resolution);
  the tiebreak falls to the random suffix instead of `status.failedAt`.
- The status of a rejected `Network` freezes and keeps reporting old numbers.
- After deleting the winning `Network`, recovery takes up to roughly 90 seconds,
  because the loser retries every minute and the group every 30 seconds. A watch
  mapping `ServerGroup → Network` would solve both.
- `NetworkReconciler.Recorder` and `Clock` are unused; a rejection produces no
  Kubernetes event, only a condition.
- The `deletionTimestamp` skip in `Sweep` is covered by no test; it concerns only
  an already-deleting orphaned pod, where a second `Delete` is harmless.
- **`make -j image-test` can load the wrong image.** `image` and
  `velocity-image` (`Makefile`) both run `nix build` with no `--out-link`, so
  both land in the same default `./result` symlink, and `image-load` /
  `velocity-image-load` each read `< result` right after their own build.
  Plain `make image-test` is safe because `make` orders the two prerequisites
  left to right, but `image-test: image-load velocity-image-load` is what
  makes both reachable from one command, and a parallel `make -jN` is free to
  run the two `nix build` invocations at the same time — the two `load`
  steps that follow can then each read whichever build most recently swapped
  the shared symlink, up to a half-written one mid-swap. `nix build
  --out-link` with two distinct names (e.g. `result-paper` and
  `result-velocity`) closes it; nothing does that today.
- **`record.FakeRecorder`'s buffered channel blocks its writer once full.**
  Almost every fixture in `internal/controller` builds its recorder with
  `record.NewFakeRecorder(100)`, but the buffer is per call site and not a
  package-wide constant: `server_controller_test.go` builds one with 20. The
  channel holds exactly as many events as its own call site asked for,
  and a reconciler that emits one more blocks inside the `Send` call instead of
  dropping it or erroring. Budget against the buffer the test in front of you
  actually built, not against 100. A test that walks enough lifecycle to cross that
  line does not fail — it hangs, and the only symptom is the package's
  ten-minute `go test` timeout, with nothing in the output naming the
  recorder or the channel; a mutant that should take a second to disprove can
  look like a wedge instead. Milestone 4d hit this once, in
  `TestGroupWithABrokenNewImageDoesNotRebuildEveryPass`
  (`internal/controller/servergroup_controller_test.go`), which fails a
  server on every one of ten passes and, unguarded, produces more than 100
  events across the two recorders it shares. The fix there is local: a
  `drainRecorder` helper next to the test empties both recorders once per
  pass, a workaround for that one test, not of the fixture. Nothing stops the
  next event-heavy test from hitting the same wall unwarned — milestone 4c's
  proxy-drain and node-drain suites are the likeliest next hit, being the
  same shape, many servers walked through many lifecycle events in one test.
  The real fix belongs in the fixture itself, a recorder that grows or drops
  past its buffer instead of blocking, the next time someone touches
  `record.NewFakeRecorder`'s call sites in this package rather than adding a
  second local drain.
