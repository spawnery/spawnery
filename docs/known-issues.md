# Known issues and carry-overs for later milestones

Status: end of milestone 3a, the operator's proxy side (2026-08-10).

This list collects what was deliberately left open during the implementation and
the reviews of milestone 1, milestone 2a, milestone 2b, milestone 2c and
milestone 3a. It does not replace a spec — the design decisions live in
`superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`, in
`superpowers/specs/2026-08-08-agent-channel-design.md`, in
`superpowers/specs/2026-08-09-paper-agent-design.md` and in
`superpowers/specs/2026-08-10-proxy-channel-design.md`.

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

*Met* by `SessionLoop` in `agent/paper/src/main/kotlin`, which opens the
replacement stream and only retires the outgoing one once the operator has
answered on the replacement. That last clause is not decoration: an earlier
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

**`set_property` in `image/entrypoint.sh` assumes `server.properties` is
writable.** It rewrites the file via `grep -v ... >server.properties.tmp` and
`mv server.properties.tmp server.properties`. If a mount ever makes
`server.properties` (or its directory) read-only, that `mv` fails under
`set -eu` with a bare `mv:` message that says nothing about why. Design §8
claims the entrypoint survives "a user mount overwrites `server.properties` →
the entrypoint rewrites the three enforced fields afterwards" — it does not,
once the mount is read-only, and nothing today exercises that case.

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

## Preconditions for milestone 3 (proxy integration)

Three of the five original preconditions below are discharged by milestone 3a
(the operator's proxy side, 2026-08-10). They are kept rather than deleted for
the same reason milestone 2c's closed preconditions are: the reasoning is what
the next sub-project — 3b, the Velocity image — inherits, and only the
reasoning makes it legible. The two image-layer items stay open; they belong
to 3b. What 3a itself discovered while closing the other three follows after
them.

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

**A NetworkPolicy restricting backends to proxies-only is deferred until
`online-mode` is actually off.** Design §7 leaves it out of 3a on purpose:
built now, before 3b flips `online-mode=false`, the policy would guard an
invariant nothing yet relies on, and a green NetworkPolicy test would look
like proof of an isolation guarantee the servers do not actually have — they
still trust connections directly. Milestone 6 owns NetworkPolicies generally
(see the availability precondition below); 3b is where pairing this one with
`online-mode` first becomes checkable, and that is the point at which leaving
it out stops being safe.

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
`set_property`" above — 3b is where it has to be built, since that is also
where the `/data/config` collision has to be resolved.

**Whether the operator runs inside the cluster for the E2E flow is still
open, and 3c's evidence run is where it starts to cost.** Today it runs
outside through `go run`, and the local kind flow hand-builds the `Service`
and `Endpoints` its own pods dial (see "The local kind flow needs a `Service`
nothing creates" above) — workable for one person at a terminal, a wall for
milestone 6's CI. An operator image is out of scope for all of milestone 3,
but 3c is where its absence first has to be worked around a second time
rather than once.

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
