# Spawnery

A Kubernetes-native cloud system for Minecraft networks.

Spawnery runs Paper game servers behind a Velocity proxy layer on Kubernetes —
dynamically scaling minigame and lobby groups as much as persistent survival
worlds. The target platform is RKE2 on bare metal, without ruling out other
distributions.

Servers are described in groups, not in pods:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
spec:
  networkRef: { name: production }
  type: Ephemeral
  maxPlayers: 100
  scaling:
    minReplicas: 1
    maxReplicas: 10
    spareSlots: 40      # how many free slots Spawnery keeps in reserve
```

Scaling follows free player slots rather than CPU, and a server with players is
never deleted: before it stops, its players are moved onto a fallback through
the proxy.

## Status

Under development. Milestone 1 is done: the four CRDs, the operator with the
Network, ServerGroup and Server controllers, the state machine including
readiness loss, and the orphan sweep.

Milestone 2a is done: the agent channel. A gRPC service inside the operator
process accepts TLS connections from game server pods, identifies them through a
pod-bound ServiceAccount token, and the registry behind it feeds the two-stage
ready gate and `status.players` that milestone 1 had left unwired. The channel
is proven end to end in envtest — a test agent brings a `Server` with green pod
readiness all the way to phase `Ready`.

Milestone 2b is done: the Paper base image. `nix build .#paper-image` produces
a reproducible image holding Paper 26.2, a headless JDK 25 and `spawnery-slp`, the tool
the readiness probe calls to speak a real server list ping. Paper is patched at
build time, so a pod downloads nothing at startup; `make image-test` runs the
image offline to keep that true.

Milestone 2c is done: the Paper agent. `nix build .#paper-agent` produces a
shaded, bit-reproducible Kotlin plugin that is baked into the image; the
entrypoint copies it into the pod's writable plugins directory, and from there
it opens an authenticated `ServerSession` to the operator, reports its
readiness and its player counts, and renews the session with overlap so a
renewal never costs a server its `Ready`.

Both halves of the ready gate now close. Measured in a local kind cluster, a
`Server` reaches phase `Ready` about twenty seconds after its pod is created,
and `status.players` and `status.slots` carry what the agent read off the
running server rather than a placeholder.

Milestone 3a is done: the operator's proxy side. `ProxySession` joins
`ServerSession` in the fan-out instead of answering `Unimplemented`, the
bootstrap creates a `spawnery-proxy` ServiceAccount in every namespace it
touches alongside `spawnery-server`, and the orphan sweep no longer discards a
connected proxy's registry entry. None of it has an agent to drive it yet, so
this closes the operator's half of the contract without anything reaching
phase `Ready` for a proxy.

Milestone 3b is done: the Velocity image and configuration rendering.
`nix build .#velocity-image` produces a reproducible image holding Velocity
3.5.1 on the same JDK 25 and non-root base `nix/oci-common.nix` now shares
with the Paper image. `cmd/spawnery-config`, baked into both images, replaces
what the Paper entrypoint used to rewrite by hand: it resolves the operator's
rendered ConfigMap, a user overlay and a handful of fields neither may move
into `server.properties`, `config/paper-global.yml` and `velocity.toml`, and
refuses to start — naming the file and the key — rather than guess at
anything missing or malformed. `online-mode` is now `false` on the backends
and `true` on the proxy, which is what modern player-info forwarding actually
requires.

Milestone 3c is done: the Velocity agent. `nix build .#agents` produces both
shaded plugin jars — Paper's and Velocity's, sharing one session loop in
`agent/common` since this milestone's Gradle split. The Velocity plugin opens
an authenticated `ProxySession`, mirrors the operator's server list into
Velocity's own registry, binds its readiness port only once a server list has
arrived so a proxy pod cannot turn ready before it can route anyone, routes a
joining player through the group's `fallbackGroups` try-list, and moves
players off a backend the operator is draining onto whatever that try-list
offers next. A `ProxyGroup` now reaches phase `Ready` the way a `Server`
already did, and milestone 3's whole point — a player can join — has code
behind it.

Getting there found the milestone's one Critical defect, and it is worth
stating plainly rather than passed over: the paragraph above about milestone
3b claimed the rendered configuration was correct on disk, and that was not
true. `internal/render` wrote Paper's forwarding secret under the key
`secret-key`; Paper reads `secret`, silently ignores the one nobody asked
for, and disables forwarding in its own log — a line nothing had ever read.
Every backend this operator rendered came up healthy and refused every
forwarded join. It was found by the first real end-to-end join attempt, ten
tasks into this milestone, and no test until then could have caught it: every
render test asserts what the renderer wrote, none of them asked what Paper
read back. Both are now checked. `docs/known-issues.md`'s "From milestone
3c" section has the rest of what this milestone leaves open, in particular
that a NetworkPolicy restricting backends to proxies-only is now overdue
rather than deferred, and that proxy drain needs a readiness
`internal/agent/registry.go` cannot yet lower — milestone 4c-1 below is what
gave it one, and the NetworkPolicy is still owed, by milestone 6b.

`make agent-test` runs both plugins, in the real images, against a real
operator-shaped gRPC server — including, for the proxy, that its ready port
stays closed until a server list arrives and opens once one does. The
cluster-level proof beyond that harness is written down as
[`docs/runbook-milestone-3-evidence.md`](docs/runbook-milestone-3-evidence.md),
and it has now been run once, against a real `kind` cluster (2026-08-12): an
automated join through `cmd/spawnery-join` reached a backend, and Paper's own
log and Velocity's own log confirmed it — the first time the forwarding chain
built in 3b and 3c was observed working end to end rather than merely
rendered correctly on disk. That automated run's drain proof surfaced a real
finding instead of a clean result: deleting a `Server` under a player held
open by the evidence tool disconnected them rather than moving them, traced to
the tool stopping short of the point Paper counts a player as online rather
than to the drain logic itself — full diagnosis in
[`docs/known-issues.md`](docs/known-issues.md), "From the milestone 3c
evidence run", which is to be read as a finding about `cmd/spawnery-join` and
not as an open criterion.

The two things that run could not settle — a join with a real Microsoft
account, and a drain moving a real player rather than the tool's stand-in —
were both proven by hand the next day (2026-08-13), and
[`docs/handover-milestone-4.md`](docs/handover-milestone-4.md) carries the
logs. A licensed Minecraft client joined through the proxy; the artifact is
the UUID, because Mojang minted a version-4 one only after the client proved
its session, where the automated probe could only ever produce the
version-3 offline form. Deleting that player's `Server` while they were in the
game moved them onto a fallback inside the same second, with the proxy's log
and the destination's both timestamping it. So milestone 3's whole point is
proven against a real client, not only against a tool.

Carry-overs and preconditions for later milestones — CA rotation, the
NetworkPolicy restricting backends to proxies-only that `online-mode=false`
now makes a real invariant rather than a deferred one, and what earlier
milestones leave open — are in
[`docs/known-issues.md`](docs/known-issues.md).

The design lives under [`docs/superpowers/specs/`](docs/superpowers/specs/), the
plans under [`docs/superpowers/plans/`](docs/superpowers/plans/).

Milestone 4a is done: slot-based scaling. An ephemeral `ServerGroup` no longer
sits at its floor. It creates servers as soon as its free player slots fall
below `spec.scaling.spareSlots`, bounded by `maxReplicas`, and removes them
again — one per pass, and only while the group's free slots would still cover
the spare — once a server has been empty for `scaleDownStabilizationSeconds`.
That is the rule for a group short of demand; a lowered `maxReplicas` is a
different rule, and a stronger one — it removes the whole surplus in a single
pass and waits for neither the stabilization window nor the spare-slot check,
because a ceiling is an instruction, not a suggestion. The rule is
`DecideSize` in `internal/controller/scaling.go`, a pure function beside
`phase.Decide` and `SelectDeletionCandidates`, and the invariant those already
carried holds unchanged: a server that may be carrying players is never
nominated.

The one thing worth naming is what the scale-up rule reads. A server created
now is not `Ready` for tens of seconds and adds nothing to `status.freeSlots`,
so a scaler reading that figure would see the same shortfall on every
five-second pass and order the same replacement again, until `maxReplicas`
stopped it. It reads a second figure instead, one that credits capacity that
has been ordered and has not arrived. The two are deliberately not the same
number, and the envtest that carries this milestone is the one that keeps
reconciling for ten more passes and asserts the count has not moved — a single
decision cannot show that failure.

Milestone 4b is done: rolling updates of ephemeral groups. A spec edit bumps a
`ServerGroup`'s `metadata.generation`, which makes every server created before
it stale, and 4b is what carries out the changeover instead of a person
deleting pods. `selectRetirement` (`internal/controller/scaling.go`) nominates
one stale server per pass — empty ones first, then the oldest — the group
patches `spec.retire: true` onto it, and `phase.Decide` moves it into the new
phase `Retiring`: deregistered, so it takes no new joins, while the players it
already has finish in their own time. Nobody is kicked.
`spec.update.maxUnavailable` bounds how many may be out at once, and
`spec.update.maxStaleSeconds` is what escalates a server that never empties
into a real drain.

The one thing worth naming is the deadlock this had to break twice. A group
already at `maxReplicas` has no room to build the first server of the new
generation, so the changeover cannot start; the rule that resolves it sheds an
idle stale server first, and when there is nothing to shed the group says which
kind of stuck it is — `ScalingLimited` names the blocked cold start rather than
an ordinary capacity shortfall. What 4b leaves open is that *any* spec change
starts a changeover: retuning `spareSlots` replaces a whole group of
functionally identical servers.

Milestone 4d is done: per-group backoff and the `Degraded` condition. It
appears here, out of alphabetical order, because it shipped here: it was cut
out of 4b during that milestone's design, on the measurement that it shares no
code with the rolling update, and 4c was already named. A group whose servers
cannot start no longer rebuilds them as fast as it can notice them dying.
`CountFailures` and `DecideBackoff`
(`internal/controller/backoff.go`) turn a running failure streak — kept on the
CR as `status.consecutiveFailures` and `status.lastFailureAt` — into permission
to create, waiting ten seconds and doubling to a five-minute cap, and giving up
after six. Waiting publishes `BackingOff`; giving up publishes `Degraded` with
reason `CrashLoopBackoff`, and the phase follows. The one thing worth naming is
what breaks the streak: a success *since the last counted failure*, not any
server being `Ready`. The weaker rule reads well and is wrong — a group with
one healthy server and one that crash-loops would hold its count at zero
forever and hammer indefinitely. It leaves open that the threshold counts
failed servers rather than failed rounds, so a group with `minReplicas` of six
or more can give up on its very first pass, and giving up is terminal until
somebody edits the spec.

Milestone 4c-1 is done: the proxy readiness contract, and the first drain that
uses it. Before a proxy pod is removed, the operator tells its agent to stop
being ready — a `SetReady` message on the proxy channel, which the Velocity
plugin's `ReadyGate` answers by closing the port the kubelet's readiness probe
completes against — so the endpoint disappears and no new player is routed
there. It then waits for the pod to empty, bounded by
`spec.drain.timeoutSeconds`. The one thing worth naming is that the wait is for
*empty*, not for `NotReady`, and that nobody is moved: a proxy drain has no
elsewhere to put a connection that terminates at the proxy being removed. The
deadline is therefore the only path in the milestone that disconnects anyone,
and it says so out loud, with a `Warning` event naming how many people it just
disconnected. What it leaves open matters on upgrade: a proxy image predating
`SetReady` ignores the message, never lowers its readiness, and keeps taking
*new* players for the whole drain window. Upgrade proxy images before the
operator. Its two cluster claims were driven twice against a real cluster with
a licensed client on 2026-08-14
([`docs/runbook-milestone-4c1-evidence.md`](docs/runbook-milestone-4c1-evidence.md)).

Milestone 4c-2 is done: proxy rolling updates. A proxy pod is stale when its
`spawnery.cloud/pod-hash` label differs from a digest of the pod the operator
would render for that group right now; `DecideRollout`
(`internal/controller/rollout.go`) surges by one, takes one pod at a time, and
hands each marked pod to 4c-1's drain unchanged rather than inventing a second
deadline. The one thing worth naming is what hashing the whole rendered pod
costs, rather than a chosen list of spec fields: upgrading the operator can
roll every proxy in the cluster with nobody having edited anything — a new
default in `internal/podspec`, an added environment variable, even a different
`--operator-namespace`, all move the digest. The group's own status hides it,
because the surge pod arrives before any withdrawal; two distinct `pod-hash`
values in one group is the tell. Its own cluster claims are §11 of the same
runbook, added for this milestone and driven the same night against merged
`master`, with a real client
([`docs/runbook-milestone-4c1-evidence.md`](docs/runbook-milestone-4c1-evidence.md)).

Milestone 4c-3 is done: node drain. `IsDeparting`
(`internal/controller/nodes.go`) has two ways in — `spec.unschedulable`,
hardwired, and any taint whose key was passed to the repeatable `-drain-taint`
flag and whose effect is `NoSchedule` or `NoExecute` — and from there the two
group kinds answer differently on purpose. A
condemned server attaches to `DecideSize`'s decision *outside* the capacity,
ceiling and demand rules, because a node drain answers to none of the three:
the node is leaving with or without the group's consent. A proxy on a departing
node is simply a second kind of staleness, so 4c-2's rollout drains it. Both
kinds get a PodDisruptionBudget sized from the `spawnery.cloud/occupied` label.
The one thing worth naming is what this milestone's own closing review found
and fixed: the live budget selector carried no role term, so in a namespace
where a `ProxyGroup` and a `ServerGroup` share a name, ready occupied
*proxies* inflated `currentHealthy` against a `desiredHealthy` counted only
from occupied *servers* — and the eviction API could have spent the difference
disconnecting players. Adding the role term closed it in the reconciler, but
one copy is out of reach: a `ServerGroup` last reconciled by pre-4c-3 code
left a budget at its own bare name, which nothing renames or deletes, carrying
a frozen `minAvailable` and a frozen copy of the broken selector. Delete it by
hand — `docs/known-issues.md` has the `kubectl` to find it. It also leaves
open that an operator running cluster-autoscaler must pass
`-drain-taint ToBeDeletedByClusterAutoscaler` by hand: that autoscaler taints
without cordoning, and an unset flag looks exactly like a quiet node.

Milestone 5a is done: persistent groups exist. A `ServerGroup` of type
`Persistent` used to accept `spec.replicas` and `spec.storage` and build
nothing. Now `DecidePersistentSize` (`internal/controller/persistent.go`) sizes
it by ordinal — `<group>-0`, `<group>-1`, created lowest-first and removed
highest-first — `BuildDataClaim` (`internal/podspec/claim.go`) renders one
`PersistentVolumeClaim` per ordinal from `spec.storage`, and the Server
controller creates the claim before the pod that mounts it. The same ordinal
always addresses the same claim, so an ordinal that comes back finds its world
where it left it. The one thing worth naming is what the claim deliberately
lacks: no owner reference, so a world outlives its server, its group, and an
operator who deletes the wrong object — and the ClusterRole grants neither
`delete` nor `update` on claims, with the omission enforced by
`internal/rbacaudit`'s table rather than merely written down, so a `delete`
marker added anywhere later turns `make test` red before it can ship. The
consequence is the open item: claims accumulate, and reclaiming a world is a
deliberate human act with `kubectl`. The acceptance test was driven against a
real `kind` cluster on 2026-08-16 — blocks placed, the pod deleted, the client
rejoined, the blocks still there
([`docs/runbook-milestone-5a-evidence.md`](docs/runbook-milestone-5a-evidence.md)).

Milestone 5b is done: ordered shutdown, `Recreate` updates and storage growth.
An image change now moves a persistent server, a lowered `replicas` takes one
ordinal down at a time, and `spec.storage.size` growth reaches the claim
(`growClaim`, with `patch` and deliberately not `update`, so one field moves
rather than the whole object). It all lands in the same
`DecidePersistentSize`, which nominates missing, surplus, stale-spec and
resize-pending ordinals in that priority order behind two gates: at most one
takedown in flight, and no stale ordinal touched until every required ordinal
is `Ready`. The one thing worth naming is not the feature but the 5a defect it
uncovered: a persistent group's failure counter froze on any spec edit, because
the counting call site filtered every view through a generation stamped once at
creation while `DecidePersistentSize` is generation-blind by design — invisible
until 5b gave persistent failures somewhere to accumulate toward. It leaves
open that a permanently broken ordinal stalls the whole group's update, with
nothing timing that wait out. Driven on 2026-08-16: two worlds survived an
update that recreated both, one ordinal at a time
([`docs/runbook-milestone-5b-evidence.md`](docs/runbook-milestone-5b-evidence.md)).
The positive half of storage growth was deliberately not driven there — `kind`'s
local-path storage class cannot expand a volume at all.

Milestone 5c is done: detecting forwarding secret rotation. The `Network`
controller reads the forwarding secret on each resync, records a salted
eight-byte digest of it in `status.forwardingSecretHash`, stamps every pod it
creates with that digest in `spawnery.cloud/forwarding-hash`, and reports the
comparison as two conditions and two events. It is detection and reporting
only: it restarts nothing and takes no ordinal down, because the restart order
is a decision for a person with a maintenance window, working through
[`docs/runbook-milestone-5c-secret-rotation.md`](docs/runbook-milestone-5c-secret-rotation.md).
The one thing worth naming is the negative that makes it safe: a rotation must
move no pod hash, so both `DesiredServerHash` and `DesiredProxyHash` strip the
forwarding label before digesting. Had that digest reached `spec.podHash`,
rotating a secret would have restarted every pod on that network by itself,
with nobody having asked for one: 5b's takedown rule would have walked every
ordinal of every persistent group, and 4b's retirement rule every server of
every ephemeral one. It leaves open that the stamp records the
last digest the operator *read*, not the bytes the pod actually mounted — under
a refused or failed read a pod can be running the new secret while being
reported stale. Driven on 2026-08-16, against the standing procedure rather
than around it
([`docs/runbook-milestone-5c-evidence.md`](docs/runbook-milestone-5c-evidence.md)).

Milestone 6a is done: the operator runs inside a cluster. `nix build
.#operator-image` produces a reproducible image for the operator itself,
`hack/publish.sh` (`make publish`) is one path that copies all three images
from their Nix archives to `ghcr.io/spawnery/`, and `make e2e` builds a `kind`
cluster, installs `config/deploy/`, and drives the operator through twelve
ordered scenarios under its own ServiceAccount — scaling, the ceiling, the
orphan sweep, the finalizer, the startup deadline, a world outliving its
server, the proxy's Service, the permission table against the real authorizer —
before reading the operator's whole log and failing on `is forbidden:`. That
last check is the point of the exercise: `internal/rbacaudit` compares the
generated ClusterRole against a hand-maintained table in both directions, so it
catches drift, but a permission missing from *both* leaves `make test` green
while the operator walks into a denial the first time it runs for real.

The one thing worth naming is what that check turned out to cover, because it
was measured rather than assumed and the answer is narrower than it looks.
Removing a **write** verb makes it fire: taking `create` on `pods` out of the
markers produced a quoted `is forbidden: ... cannot create resource "pods"` on
the first attempt. Removing a **cache-backed `list`** does not. With `list` on
`pods` revoked and confirmed revoked at the API server, seven and three-quarter
minutes of continuous watching produced no denial in the log, no `403` in the
operator's own client metrics, no restart and no drop in `Available`; `list` on
`networks` behaved the same way. Those two lists are what was measured, and
reads as a class are not: no uncached read was ever revoked and watched. The
hypothesis that would license the wider claim — that such a read goes through
the manager's cache, whose initial sync is a watch rather than a list, so the
revoked verb never reaches a request anyone could deny — is a hypothesis, not
something this milestone established. `docs/known-issues.md` carries both
halves of the measurement, the anomaly the hypothesis does not explain, and a
second and unrelated way a denied read escapes the check.

What 6a leaves open is recorded as open. No real `make publish` has been
driven — it needs a token nobody in that milestone had — so the digest
reference in `config/deploy/deployment.yaml` has never been resolved by
anything, and until someone pushes, every consumer still loads the images into
their cluster by hand. The run is single-node, so node drain, `HostPort` and
CIS pod security wait for the RKE2 rollout at the end of milestone 6.

Milestone 6b is done: the traffic rules, and the agent channel's availability
half. An accepted `Network` now writes a `NetworkPolicy` into its own
namespace, owned by that `Network` so the garbage collector takes it away
again, selecting the network's own server pods: ingress on 25565 from that same
network's proxies in that same namespace, egress to cluster DNS and to the
operator's agent port, and nothing else either way. `config/deploy/` gains a
second policy, which selects the operator pod and admits 9443 only from pods
carrying `spawnery.cloud/managed-by` in any namespace — cross-namespace by
construction, because every managed pod in every game namespace dials the one
operator. Behind it the agent channel finally got the bounds it never had
(`MaxConcurrentStreams`, `ConnectionTimeout`, an idle reaper and a keepalive
enforcement policy), a `TokenReview` cache that deliberately does not cache the
pod lookup — so deleting a pod, the revocation an operator actually performs,
still takes effect on the next connection attempt — and a per-peer token bucket
consulted only when that cache misses.

The one thing worth naming is the asymmetry: the policy selects backends and
not proxies. A server's readiness probe is an `exec` inside its own container
against `127.0.0.1`, which no NetworkPolicy governs; a proxy's is a dial from
the kubelet, which one might, and whether kubelet traffic is subject to policy
at all depends on the CNI. The invariant at stake is entirely a backend's — a
Paper server runs `online-mode=false`, authenticates nobody, and trusts
whatever completes the modern-forwarding handshake with the right secret — so
6b selects backends and puts no game pod's readiness at a CNI's mercy. The one
place it could not avoid the question is the operator's own policy, which
selects the operator pod and therefore has to admit the kubelet's probe from a
source no selector can name; that rule has no `from` at all, which is the only
formulation correct on every CNI. The price of leaving proxies alone is stated
rather than hidden: nothing restricts who may open a connection to a proxy's
25565 from inside the cluster, and the proxy is the front door that has to
accept the world anyway.

What it leaves open is why none of the above should be read as protection:
**6b has not observed a single connection being refused, anywhere.** kindnet,
the CNI `make e2e` runs on, was measured to enforce nothing: with the operator
policy's kubelet-probe rule deleted, so that the object in force denied the
probe outright, the run stayed green and the rollout succeeded on its usual
timeline. Both alternative explanations were closed, because the readiness
probe is an `httpGet` over the real network path that `kubectl rollout status`
cannot succeed without, and `hack/e2e.sh` recreates the cluster every run with
the apply log reading `created` rather than `unchanged`. What that measures is
one ingress rule; that kindnet implements no NetworkPolicy controller at all,
in either direction, is its documentation rather than anything observed here.
So 6b ships objects, asserts them as objects, and says so in the tests' own
comments. Whether they enforce anything is a property of a cluster's CNI that
no run here has tested on any CNI — including the design's one portability
trap, whether a pod-selector egress rule still matches after kube-proxy has
DNATed a Service ClusterIP to a pod IP. The RKE2 rollout at the
end of milestone 6 is the first thing that can turn these objects into a
guarantee.

Milestone 6 continues with 6d, the Helm chart, and 6e, CI. It ends with the
whole system rolled out to a real RKE2 cluster and driven from a runbook.

Anyone starting milestone 6d begins at
[`docs/handover-milestone-6c.md`](docs/handover-milestone-6c.md): it says where
6c stopped, what it proved and what it only wrote down, what 6d finds in place,
and what the RKE2 rollout now owes — which has grown by everything 6c could not
prove. It is written to be read by someone with no memory of how any of this
was built.
[`docs/handover-milestone-6b.md`](docs/handover-milestone-6b.md), written for 6c
and kept because its §2 and §3 are the record of what 6c started from and had
to decide,
[`docs/handover-milestone-6.md`](docs/handover-milestone-6.md), written for 6b
and kept because its §2 and §3 are the record of what 6b started from and had
to decide,
[`docs/handover-milestone-5.md`](docs/handover-milestone-5.md),
[`docs/handover-milestone-4b.md`](docs/handover-milestone-4b.md),
[`docs/handover-milestone-4.md`](docs/handover-milestone-4.md),
[`docs/handover-milestone-3.md`](docs/handover-milestone-3.md),
[`docs/handover-milestone-2c.md`](docs/handover-milestone-2c.md) and
[`docs/handover-milestone-2b.md`](docs/handover-milestone-2b.md) are its
predecessors, kept as the record of what those milestones started from.

## Development

```bash
nix develop            # Go, controller-gen, envtest assets, kubectl, kind, k3d,
                       # protoc with its Go and Java plugins, JDK 21, Gradle
make test              # unit and envtest tests
make build             # bin/spawnery-operator
make agent             # both agent plugins (Paper and Velocity) and their JUnit suites
make e2e               # the driven run: the operator in a real kind cluster
```

`make proto` regenerates the Go code under `internal/agentpb` from
`proto/spawnery/agent/v1alpha1/agent.proto`. The generated code is checked in
like `zz_generated.deepcopy.go` — after a change to the `.proto`, run `make
proto` and commit the diff with it; `make test` does not regenerate it on its
own.

`make agent` builds both agent plugins (`nix build .#agents`) — Paper's and
Velocity's, sharing the session loop, token source and channel construction in
`agent/common` since milestone 3c's Gradle split — and runs both JUnit suites
as the derivations' check phases; it is the target to reach for after any
change under `agent/`. `make agent-deps` regenerates `agent/deps.json`, the
checked-in lockfile that pins every Maven artifact by hash across all three
Gradle subprojects; it is needed only when a `build.gradle.kts` under `agent/`
changes a dependency, and it is deliberately part of no other target, not even
`make all`, because it reaches Maven Central — a dependency change is an
explicit act and a Nix build must never depend on the network. `make
agent-test` runs both real images against
the Go stub operator in `cmd/spawnery-stubop` and checks the handshake, the
authorization header, the player reports, the overlapping renewal and the
bound on a session the operator never answers — and, for the Velocity image,
that its readiness port stays closed until a server list has arrived and
opens once one does; it is the target to run after any change to either
agent's session handling, and like the image targets below it needs a
container runtime and only works on `x86_64-linux`.

`make image` builds the Paper base image, `make image-load` hands it to the
local container runtime, and `make image-test` runs both the Paper and the
Velocity images offline under the same constraints the podspec imposes —
loading both first, so the target needs no separate `make velocity-image`
step of its own. All three need Docker or Podman and only work on
`x86_64-linux`. Pass `CONTAINER=podman` if `docker` is not your runtime.
`make velocity-image`, `make velocity-image-load` and `make
velocity-image-test` are the same three steps scoped to only the Velocity
image, for when a change is known to touch nothing on the Paper side.

`make image-repro` builds each image and then rebuilds it with `nix build
--rebuild`, and fails if the two builds do not produce the same bytes. Design
§5.3 makes that reproducibility an acceptance criterion, not a one-off claim, so
this is the standing check for it — worth running again after any change to
`nix/paper.nix` or `nix/paper-image.nix`. Like `image-test`, it is not part of
`make test` or `make all`: it needs a build's worth of time and only runs on
`x86_64-linux`.

The plain build in front of each `--rebuild` is not redundant. `--rebuild`
compares a fresh build against the output already in the store, and with
nothing there it does not fail the check, it declines to run it — "some outputs
… are not valid, so checking is not possible". All three image derivations take
the working tree as their source: appending one line to a file in `docs/` was
measured to change the derivation hash of `paper-image`, `velocity-image` and
`operator-image` alike (`agents` was unaffected). So an edit almost anywhere
empties the store of them, and until milestone 6a's final fix wave this target
had nothing to check against on a tree anybody had touched.

`make operator-image` builds the operator's own image, `make operator-image-load`
hands it to the local container runtime, and `make operator-image-test` runs it
under the constraints `config/deploy/deployment.yaml` imposes — non-root and a
read-only root filesystem — rather than more comfortable ones, plus
`--network none`, which is the script's own choice and not the Deployment's,
and cheap here because the run only asks the binary to print its usage.
Since milestone 6a the operator is a container like the other two, and
`make image-repro` covers all three of them plus the agent jars. The whole
target was driven on 2026-08-17, after the milestone merged: all four
`--rebuild` comparisons — `paper.tar.gz`, `velocity.tar.gz`,
`spawnery-operator.tar.gz` and `spawnery-agents` — came back clean, exit 0.

`make publish` (`hack/publish.sh`) copies all three images from their Nix
archives straight to `ghcr.io/spawnery/` with `skopeo`, so what reaches the
registry is what the flake describes rather than what a previous
`podman load` left in a local store. It is part of no other target, because it
contacts a registry and needs a GitHub token with `write:packages`. `DRY_RUN=1`
still builds every image it was asked for — on a machine without them cached
that is the expensive part — and then prints what it would copy where instead
of copying it, so nothing reaches the registry and no credential is needed;
`FORCE=1` overwrites a tag that already exists, which it otherwise refuses to
do; `WRITE_DIGEST=1` rewrites the operator's image reference in
`config/deploy/deployment.yaml` to the digest `skopeo copy` reported for the
push it just made.

`make publish IMAGES=operator-image` publishes one image rather than all three,
and that is the ordinary case rather than an escape hatch: `flake.nix` keeps
`operatorVersion` apart from `imageVersion` on purpose, so after a reconciler
fix exactly one of the three tags is new. Asking for all three then stops,
correctly, at the first tag that is already published, and never reaches the
one that changed — and `FORCE=1` would get past that only by re-pushing about
1.4 GB over tags that were already right.

**Nothing has been pushed yet** — the first real run needs that token, and
until it happens every consumer still needs `kind load docker-image` or the
equivalent.

`make e2e` (`hack/e2e.sh`) is the driven end-to-end run: it builds the operator
image, creates a `kind` cluster, loads the image into it, installs the CRDs and
`config/deploy/`, and then runs a Go test package that drives the operator
through twelve ordered scenarios under its own ServiceAccount before reading its
whole log and failing on `is forbidden:`. The operator runs *in* the cluster
here, from its own image, so nothing hand-builds a `Service` — which is the
difference between this and the local flow below. It is part of neither
`make test` nor `make all`: it builds a cluster and takes minutes, and the
commit loop stays where it is. Like the image targets it needs a container
runtime and only works on `x86_64-linux`. On a machine where `kind` runs under
rootless Podman, the invocation is:

```bash
systemd-run --scope --user --property=Delegate=yes -- \
  nix develop -c env KIND_EXPERIMENTAL_PROVIDER=podman make e2e
```

`E2E_KEEP=1` leaves the cluster standing afterwards and prints its
`KUBECONFIG`; a failed run dumps the operator log, the objects and the events
before tearing down.

Running this image accepts
[Mojang's EULA](https://www.minecraft.net/eula) on your behalf: the entrypoint
writes `eula=true`, because Paper does not start otherwise.

### Trying it locally against kind

This section is the hand-driven flow, and it runs the operator **outside** the
cluster through `go run`. If what you want is the operator running inside a
cluster, `make e2e` above does the whole thing automatically and needs none of
the workarounds below — the `Service` there has a selector, because there is a
pod for it to select. What this flow still gives you that `make e2e` does not
is a real Paper image, a server that reaches `Ready`, and an agent that reports
players.

These steps need a container runtime — Docker or Podman — for a local
Kubernetes cluster. On the machine this was last run on, `docker` is a Podman
5.8.4 alias with no `/var/run/docker.sock`, and only a rootless Podman socket
is available. Under that setup `k3d` cannot bring up a cluster at all: its
tools node always bind-mounts the runtime socket to `/var/run/docker.sock`
inside itself, and rootless Podman refuses to create that mount point
(`mkdir /var/run/docker.sock: permission denied`) — no `DOCKER_HOST` value
fixes it, since the failure is in the tools node's own container creation, not
in the client reaching the socket. `kind` under
`KIND_EXPERIMENTAL_PROVIDER=podman` does work against the same rootless
socket, and is what the flow below uses. Anyone with a real Docker daemon (or
a rootful Podman socket) can use `k3d` the same way instead — the manifests
and the operator invocation are identical either way.

kind additionally needs cgroup delegation to run under systemd as a regular
user, hence the `systemd-run --scope --user --property=Delegate=yes` wrapper
around every kind command below: without it, kind refuses with a
`Delegate=yes` error even when that property is already set on the user's
systemd service — the scope is what its check actually looks for.

The operator runs here through `go run` outside the cluster, so without
`POD_NAMESPACE` from the downward API. `--operator-namespace` therefore has to
be set explicitly — without the flag the process refuses to start (see
`validateAgentFlags`), because the serving certificate would otherwise carry the
wrong SANs.

That leaves one gap this flow has to close by hand, and since milestone 2c it
is the difference between a `Server` that reaches `Ready` and one that does
not. The pod dials `spawnery-operator.<ns>.svc:9443`, and nothing creates that
Service: the operator is not in the cluster, so no selector could find it. A
selector-less `Service` with a hand-written `Endpoints` pointing at the host
closes it — the serving certificate already carries that DNS name, so TLS
verifies against the CA the pod was given.

Which address goes into those `Endpoints` depends on the runtime. With a real
Docker daemon it is the bridge gateway, `172.17.0.1`, and nothing further is
needed. Under rootless Podman it is none of the obvious candidates: the gateway
of the `kind` network (`10.89.0.1` here) lives inside the rootless network
namespace, where the operator is not listening and a connection is refused, and
the one address that does reach the host — the pasta link-local
`169.254.1.2`, which Podman also publishes as `host.containers.internal` — is
rejected by the API server in both `Endpoints` and `EndpointSlice` with
`may not be in the link-local range`. What works, measured, is one more
container on the same Podman network relaying to the host: it gets a routable
address on that network, and pods reach it.

```bash
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind create cluster --name spawnery-dev
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind load docker-image ghcr.io/spawnery/paper:26.2-0.2.0 --name spawnery-dev
nix develop -c kubectl apply -f config/crd/bases
nix develop -c kubectl apply -f config/samples/network.yaml
nix develop -c go run ./cmd/spawnery-operator --leader-elect=false --operator-namespace minecraft &

# Rootless Podman only. With a real Docker daemon, skip this and use
# 172.17.0.1 as the endpoint address below.
podman run -d --name spawnery-relay --network kind \
  -v /nix/store:/nix/store:ro \
  --entrypoint "$(nix build --no-link --print-out-paths nixpkgs#socat)/bin/socat" \
  ghcr.io/spawnery/paper:26.2-0.2.0 \
  TCP-LISTEN:9443,fork,reuseaddr TCP:host.containers.internal:9443
RELAY_IP=$(podman inspect spawnery-relay \
  --format '{{.NetworkSettings.Networks.kind.IPAddress}}')

nix develop -c kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: spawnery-operator
  namespace: minecraft
spec:
  ports:
    - name: agent
      port: 9443
      targetPort: 9443
      protocol: TCP
---
apiVersion: v1
kind: Endpoints
metadata:
  name: spawnery-operator
  namespace: minecraft
subsets:
  - addresses:
      - ip: $RELAY_IP
    ports:
      - name: agent
        port: 9443
        protocol: TCP
EOF

sleep 90
nix develop -c kubectl get networks,servergroups,servers,pods -n minecraft
```

The image only needs a rootfs for the relay, which is why the Paper image
stands in for one; `socat` itself comes out of the mounted Nix store.

The first server can take a good half minute to appear: if the ServerGroup
meets its network before the Network controller has accepted it, it tries again
only after `networkRetryInterval` (30 seconds). The 90 seconds above also cover
Paper's own start — about seven seconds to a first answered ping — and the
agent's handshake after it. Loading the image into the cluster beforehand is
its own wait: at 26.2-0.2.0 it is 735 MB as a tarball, a little over a gigabyte
unpacked.

Expected, as measured on 2026-08-10 against `kind` v1.36.1 under rootless
Podman:

- `network production` with `Accepted=True` and `SERVER GROUPS 1`,
- `servergroup lobby` in phase `Ready` with `READY 1` and `FREE SLOTS 100` —
  `READY` is `status.readyReplicas`, and since 2c a server does reach `Ready`,
  so unlike after 2b this is no longer `Pending 0`,
- a pod `lobby-xxxx` in `Running` with `READY 1/1` — the readiness probe spoke
  a real server list ping to a real Paper process,
- a `server lobby-xxxx` in phase `Ready` with `SLOTS 100`, `PLAYERS 0` and
  `REGISTERED true`. `SLOTS` is what the agent reported from
  `SPAWNERY_MAX_PLAYERS`, `PLAYERS` what it counted on the running server —
  zero, because nobody can join yet.

If the `Server` stops in `Starting` instead, the agent cannot reach the
operator: `kubectl logs` on the pod shows the reason, and it has so far always
been the `Service`/`Endpoints` pair above, not the agent.

Leaving it running for a quarter of an hour shows the other half of what the
agent is for. The session renews after eight minutes
(`--agent-session-renew-after`), and if the replacement stream did not overlap
the outgoing one, the server would drop out of `Ready` on that rhythm. Measured
over thirteen minutes:

```bash
nix develop -c kubectl get server lobby-xxxx -n minecraft \
  -o jsonpath='{.status.readinessLosses} {.status.readySince} {.status.playersUpdatedAt}'
# 0 2026-08-09T22:37:36Z 2026-08-09T22:49:57Z
```

`readinessLosses` still zero and `readySince` still the original timestamp,
while `playersUpdatedAt` keeps moving — the renewal happened and cost the
server nothing.

Afterwards, clean up:

```bash
kill %1
podman rm -f spawnery-relay
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind delete cluster --name spawnery-dev
```
