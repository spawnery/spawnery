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

**`fsGroup` is missing.** For ephemeral groups this does not bite: the kubelet
creates an `emptyDir` world-writable, so uid 10001 writes into `/data` fine. A
PVC in milestone 5 arrives owned by root, and uid 10001 does not. The fix
belongs in `podspec.BuildServerPod`'s `PodSecurityContext` and has to land
before the first persistent server exists.

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
again. Both belong in one milestone.

**Terminating pods count as "process gone".** `isOccupied` treats a pod with a
set `deletionTimestamp` as free of sessions, even though the process is still
running during the grace period and players may still be connected. As a result
`minAvailable` drops by one for the duration of the grace period while the pod's
label still matches the selector. Not reproducible in envtest, because there is
no kubelet there — this needs a real cluster.

**Exponential backoff per group.** Spec 7 requires it along with the condition
`Degraded`/`CrashLoopBackoff` and stopping further attempts. Milestone 1 instead
has only an upper bound of one retained failure per group.

**Nothing bounds the reported `slots` against the group's `maxPlayers`.**
`internal/agent/registry.go` rejects a player count above the reported `slots`,
but checks `slots` itself against nothing — even though the operator knows the
upper bound and handed it to the pod itself as `SPAWNERY_MAX_PLAYERS`. Today
`slots` only ends up in `status.slots` and is therefore cosmetic. From milestone
4 it feeds `FreeSlots` and with it the scaling decision: a compromised pod
reporting `slots: 1000000` at zero players makes the group look permanently
spacious and suppresses every scale-up for all of its servers — exactly the
effect across pod boundaries that milestone 2a otherwise rules out. The bound
belongs in the same change as `FreeSlots`: the registry entry does not yet know
the pod's group, so clamping to `maxPlayers` has to sit where both come
together.

**`ProxyGroupReconciler.pods()` has no expectations tracking, and the blast
radius of that is new.** It lists pods through the manager's plain cached
`List`, with no reservation for a create it just issued. A reconcile the
group's own pod-create event triggers can therefore read a cache that has not
caught up yet, see the count still short, create a second pod, and have the
next reconcile — once the cache catches up — delete the surplus.
`ServerGroupReconciler.pods()` has the identical shape (`servergroup_controller.go`),
so this is a repository-wide pattern, not something 3a introduced. What 3a
changes is the cost of hitting it: milestone 3a ships no proxy drain, so
deleting a surplus proxy pod disconnects every player on it outright, where
deleting a surplus `Server` at least goes through the drain state machine
first. The fix — expectations tracking, à la the original `ReplicaSet`
controller, or deterministic pod names derived from an ordinal instead of a
random suffix — belongs with milestone 4's rolling updates, which touch this
same code for an unrelated reason. This entry is the precondition: the record
that the problem exists and why, so milestone 4 does not have to rediscover
it.

**Orphaned `Server`s without a pod.** The sweep covers "pod without CR" and
"server without group", but not "CR without pod": that is handled by the state
machine through `PodLost`, which only applies once `status.podName` has been
written. A server that never got a pod would stay in `Pending` and occupy its
slot.

## Preconditions for milestone 5 (persistent groups)

If a server's `ServerGroup` is missing, the server controller carries on with
ephemeral fallback timings. For a persistent server those are the wrong
deadlines.

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
- "Keep the oldest failure" does not carry when the `creationTimestamp` is equal
  (second resolution); the tiebreak falls to the random suffix instead of
  `status.failedAt`.
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
