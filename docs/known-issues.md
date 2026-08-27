# Known issues and carry-overs for later milestones

This file carries only problems that still exist. An entry that gets fixed is
deleted, and the account of what it was and how it was found lives in the
commit that removed it — `git log -p docs/known-issues.md` is where to look
for one. A closed entry left standing with a note saying it is closed costs a
reader the same attention as a live one, which is the whole reason for the
rule.

Four things that are not open problems live elsewhere.
[`upgrading.md`](upgrading.md) carries what strands an object or rolls a fleet
when an installation crosses a release — real work for whoever is upgrading
one, and nothing at all for anyone else.
[`ca-rotation.md`](ca-rotation.md) carries the CA rotation procedure, which is
a thing a human drives rather than a thing that is wrong.
[`persistent-storage.md`](persistent-storage.md) carries what an operator owns
about a persistent group's claims — that this operator never deletes one, that
deleting one deletes a world, and how long a group whose storage is broken
takes to say so.
[`network-boundaries.md`](network-boundaries.md) carries what the
`NetworkPolicy` objects buy and what they do not, which is measured scope
rather than a list of faults. The design decisions live in
`superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`, in
`superpowers/specs/2026-08-08-agent-channel-design.md`, in
`superpowers/specs/2026-08-09-paper-agent-design.md`, in
`superpowers/specs/2026-08-10-proxy-channel-design.md`, in
`superpowers/specs/2026-08-10-velocity-image-design.md` and in
`superpowers/specs/2026-08-11-velocity-agent-design.md`.

## From milestone 3c (the Velocity agent)

**A backend whose node dies with players on it now has about twenty seconds
of margin, and that margin is not guaranteed.** Velocity disconnects such
players outright rather than firing an event: disassembling velocity 3.5.1
build 615, `ConnectedPlayer.handleConnectionException` falls straight through
to `disconnect()` when its `safe` argument is false, and
`BackendPlaySessionHandler` passes false for exactly a `ReadTimeoutException`.
So no `KickedFromServerEvent` fires, the agent's own `Rescue` never sees them,
and no plugin can intervene. That is unchanged and unchangeable from this side.

What changed is that the operator now moves them first. A stream that is *up
and quiet* is the signature of a peer that is gone without TCP having noticed —
measured through a freezable relay at over 200 seconds before the socket
reacts — and it is distinguishable from an operator restart, which breaks
every stream at once and leaves `AgentConnected` false. On that signature the
server loses readiness, is deregistered so nobody else is sent to it, and is
drained.

The margin is arithmetic and `phase.RescueWindow` is now that arithmetic:
Velocity's `read-timeout` less twice the agent report interval, which is twenty
seconds at the operator's defaults and zero at a report interval of fifteen.
**Half of it is checked and half of it cannot be.** The operator warns at
startup when its own `--report-interval` leaves a window shorter than the
`ResyncInterval` it could act within — that is the half somebody sets by hand,
and the read timeout it is compared against is pinned to the value
`internal/render/defaults/velocity.default.toml` ships by a test, so the two
cannot drift apart silently.

What stays open is the other half: **a `velocity.toml` overlay that lowers
`read-timeout` closes the gap without anything noticing.** The overlay is the
user's own ConfigMap, mounted into the pod by name and rendered there by
`spawnery-config`; the operator never reads its contents, and its ConfigMap
cache is deliberately restricted to objects carrying the managed-by label, so
seeing it would mean an uncached read of somebody else's object on every
reconcile. A cluster that replaces the whole file rather than overlaying it
lands on Velocity's own 30-second default by luck rather than by agreement, and
nothing here can tell the two apart.

## From milestone 4c-1 (the proxy readiness contract)

**One assertion in `hack/agent-test.sh` is still argued rather than
demonstrated.** The post-loop arm of phase 4's withdrawal guard — the 25565
probe that runs after the gate has closed — would only be seen failing if the
proxy's own listener went down with the gate, which no correct agent does and
no fault this harness can inject produces. It is a control that has never been
observed controlling anything.

The probe underneath it is no longer the unknown: `port_open` is now shown to
answer true for a bound port (25565, before the gate probe) and false for one
nothing binds, so it discriminates rather than merely agreeing. What is left is
whether this particular assertion can fail, and answering that means making a
proxy shut its own listener on a SetReady — a fault injection nothing here
needs for any other purpose.

## From milestone 4c-3 (node drain)

**`paulwtf` passes no `-drain-taint`, so the day an autoscaler appears there
somebody has to set it.** *Measured 2026-08-25:* the operator `Deployment`'s
args are `--leader-elect`, `--startup-deadline`, `--metrics-bind-address` and
`--health-probe-bind-address`, and nothing else. Harmless today — three fixed
bare-metal nodes and no autoscaler, so the taint branch has nothing to react to
— and this is the sentence that will be looked for the day one appears.

`IsDeparting` (`internal/controller/nodes.go`) has two ways in:
`spec.unschedulable`, which is hardwired, and a taint whose key appears in the
`-drain-taint` list, which is repeatable and empty by default. That default
stays empty: reacting to another project's taint key by default would couple
this operator to a vocabulary that project is free to rename, which is the
coupling a configurable list exists to avoid.

What is no longer true is the rest of the old entry, which said nothing in the
operator would tell an operator they had missed the flag. It does now: a node
carrying a well-known drain taint the operator was not configured for produces
one log line per node naming the project and the flag. Noticing is not
reacting, and only the second was ever the coupling worth avoiding.

For the record, since a draft of the design that produced this milestone got it
wrong and was corrected in place (`bc4122a`): cluster-autoscaler taints
`ToBeDeletedByClusterAutoscaler:NoSchedule` and deletes the node **without**
touching `spec.unschedulable`, unless `--cordon-node-before-terminating` is on,
and that flag defaults to off. Karpenter was never re-checked and is not
claimed here either way.

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

*Narrowed since 2026-08-26, for the namespaces somebody names.*
`charts/spawnery/values.yaml`'s `networkNamespaces` renders the Role and
RoleBinding per listed namespace, with `resourceNames` restricted to that
Network's secret — the narrowing the master design's §8 asks for and the
hand-applied file cannot afford — and with the RoleBinding's subject in the
release's own namespace rather than a hard-coded `spawnery-system`. What
remains is the gap below: a namespace nobody listed is a namespace nobody
opened.

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

## From milestone 6e (CI)

GitHub Actions runs four workflows: `.github/workflows/ci.yml` blocks
four jobs — `test`, `lint`, `deps`, `e2e` — on every pull request and on
push to `master`; `.github/workflows/nightly.yml` runs `make image-repro` on
a schedule plus `workflow_dispatch`; `.github/workflows/release.yml` runs
`hack/publish.sh` on a `v*` tag; and `.github/workflows/paper-watch.yml`,
added after that milestone, asks daily whether PaperMC has published a build
newer than `nix/paper.nix` names. Full account of the first three:
`docs/handover-milestone-6e.md`.

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
