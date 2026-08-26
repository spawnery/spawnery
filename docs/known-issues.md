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

## From milestone 2b (the base image)

**Following Paper upstream is manual, and needs a decision rather than a
fix.** A new build means new hashes in `nix/paper.nix`, by hand, including the
Mojang hash out of the new jar's `META-INF/download-context`. Automating it is
project 3 in the main design and has never been scheduled; until it is, every
Paper release is a person reading two hashes out of a jar. Nothing here is
broken — the pinning is deliberate and the hash chain is stronger than most —
so this stays open only because somebody has to decide whether project 3 is
worth doing.

## From milestone 2c (the Paper agent)

**`hack/agent-test.sh`'s six phases are near-verbatim copies of one another,
and whether that is worth paying off needs a decision.** Each phase starts its
own stub, its own container and its own volume, waits on its own events and
tears its own scaffolding down, and the differences between them — which flags
the stub gets, which assertions run — have to be found by reading two phases
side by side. There were three when this was first written and there are six
now, so the cost has doubled rather than been paid off.

It is not a defect and nothing it proves is wrong; every assertion in it is
sound and the script is the only thing that proves the agents work at all.
What it is is a standing tax on adding a seventh phase, weighed against the
risk of refactoring the one harness that stands between a broken agent and a
cluster. Parameterising the phases would also hide the differences this entry
complains are hard to see, which is the argument against — so this stays open
as a judgement about where to spend effort rather than as something to fix.

## From milestone 3c (the Velocity agent)

**A proxy that cannot bind its ready port stays `Pending` with the reason
only in its own log, and closing that needs a choice between two answers
neither of which is obviously right.** `ReadyGate.open` logs the bind failure
and carries on; the kubelet's probe on 8081 then fails forever, the pod never
goes `Ready`, and the group sits below its count with nothing saying why.
`kubectl logs` has the reason, which is the ordinary path for a container-level
failure — but the group's own status does not, and a `playerLimit` defect of
exactly this shape was worth fixing in milestone 3b.

The two ways out cost different things. **A fault channel**: the agent's stream
is up even when the gate is not, so it could tell the operator — but
`OperatorToProxy`'s counterpart carries no field for it, so this means a new
proto message and every agent in the fleet rolling before the operator may
rely on it. **Shutting the proxy down**: a proxy whose gate never binds can
never take a player, so `proxy.shutdown()` would turn a silent `Pending` into a
`CrashLoopBackOff`, which the operator already reports on the group with a
reason. That is free and immediate, and it also means one failed bind takes
down a proxy that is otherwise serving nobody yet — which is either exactly
right or exactly wrong depending on how much a transient bind failure is
believed in.

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

**The drain's exit condition still cannot see a player who is arriving, and
no milestone owns the question.** `Occupied()`
(`internal/phase/phase.go`) is `in.PlayersStale || in.PlayersOnline > 0` —
what the *backend* has reported — so a player whose connection to the draining
server is still in flight makes it read empty.

The agent no longer loses that player: `Drain` remembers which servers are
draining and moves whoever lands on one afterwards
(`agent/velocity/.../Drain.kt`, which carries the disassembly the rule rests
on). What is left is the operator's half. Between the arrival and the move the
player *is* on the draining server, and the backend's count has not caught up,
so a `DeletePod` decided in that window still lands on someone. The window is
now the gap between two events on one proxy rather than a whole backend
handshake, and it is bounded by nothing stated anywhere.

Reading the proxy's `playersConnected` instead would not have helped, and
that is worth recording because it is the obvious fix: disassembling velocity
3.5.1 build 615, `VelocityRegisteredServer.addPlayer` is called only from
`BackendPlaySessionHandler.activated()` — the backend's *play* phase — so the
proxy does not count such a player either. Closing this needs the proxy to
report what only it knows, which is
`ConnectedPlayer.getConnectionInFlightOrConnectedServer` per backend, as a
periodic state beside `PlayerCount`. That is a proto change and a deployment
order — every proxy has to carry it before the operator may trust it — and it
is deferred rather than done.

None of this branch's reviews caught the original, and why is the transferable
part. The whole-branch review correctly predicted that a held connection would
be *counted* on the proxy side, in `status.connectedPlayers`. Nobody asked the
complementary question: which side does the drain's own exit condition read?
The two counts live in different structs, are populated by different agents,
and were never checked against each other until an actual delete on an actual
held connection forced it. The prediction was also wrong, which nobody noticed
either: the proxy does not count a held connection.

In production the window is real but small: a real client completes the
configuration phase within the same round trip. It was found because
`spawnery-join --hold` freezes a connection there deliberately, which made a
`kubectl delete` on a held player disconnect them rather than move them —
`internal/mcjoin`'s `holdOpen` carries what a held connection is and is not.

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

**A `-drain-taint` key that is simply absent from the cluster cannot be told
from a typo.** The flag is validated as far as it can be: since 2026-08-24 it
refuses a value that is not a bare qualified name, using
`validation.IsQualifiedName` — the same check the API server validates a taint
key with, so it refuses exactly what the API server would and nothing more.
That catches the mistake to expect, because taints are written
`key=value:Effect` nearly everywhere a person meets them and passing the whole
taint was the slip this operator survived worst: such a key matches no taint
that exists, so the flag was accepted, nothing ever drained, and nothing said
why.

A well-formed key nobody uses is a different matter and nothing can tell it
from a working one — except in the one case that now warns, a node carrying a
*well-known* drain taint this operator was not configured for. For a key of
somebody's own choosing there is no such list to check against. Confirm with
`kubectl describe node` that the taint is present with an effect this operator
honours (`NoSchedule` or `NoExecute`; `PreferNoSchedule` is ignored
deliberately, since it does not stop the scheduler putting a replacement back
on the same node).

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

## Preconditions for milestone 6 (Helm, RBAC, E2E)

**Milestone 2a's isolation promise holds against one compromised pod and not
against a set of them.** The promise of the agent channel reads: a compromised
game server pod cannot harm any other. For identity and confidentiality it
holds — the token is audience-bound and accepted nowhere else, the
`spawnery-server` ServiceAccount has no RoleBinding anywhere, pods run with
`automountServiceAccountToken: false`, the private CA key never leaves the
operator secret, and identity comes exclusively from the token and never from
what the agent claims about itself.

For availability it now holds for one pod, and stops exactly there. Every axis
a single pod controls is bounded: `MaxConnectionsPerPeer` bounds its
connections, `MaxConcurrentStreams` the streams on each, and `internal/grpcauth`
the TokenReviews it can drive. **Nothing bounds the sum across peers.** Ten
compromised pods are ten times the one-pod bound; the NetworkPolicy admits
every pod carrying `spawnery.cloud/managed-by` and says nothing about how many
there may be; and the operator holds no global ceiling — deliberately, because
a global cap reached by one busy namespace would refuse another namespace's
agents, which is the harm the promise is about, moved rather than removed.

Closing it needs a number the per-peer bound cannot use: how many agents the
operator *ought* to be serving, which is the pod count of the groups it owns.
It has that number in its own caches. Nothing yet compares it to
`spawnery_agent_open_connections`, and a bound derived from it would be the
first one in this channel that is a statement about the fleet rather than
about a peer.

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

**The denial check reads a log, so an error the code handles well is
invisible to it — and the self-check that answers the wider question runs
once.** `hack/e2e.sh`'s §7.2 scenario greps the operator's whole log for
`is forbidden:`. A revoked *write* verb produces exactly that on the first
attempt, measured. A revoked cache-backed *list* produces nothing at all,
measured over seven and three-quarter minutes with `pods: list` gone: no log
line, no `403` in `rest_client_requests_total` across 24 samples, no restart.
And `readForwardingSecret` deliberately folds a real 403 into a message
reading "the operator may not read secret …", with no `is forbidden:` anywhere
in it — an error handled well is invisible to a check that can only see what
something logs.

Since 2026-08-26 the operator asks the authorizer directly instead of waiting
to be refused: a `SelfSubjectAccessReview` per entry of
`rbacaudit.RequiredCluster` and `RequiredNamespaced` at startup, which sees a
cache-backed verb exactly as well as any other and names the call site of
every one it lacks. **That runs once.** A permission revoked while the
operator is running is still invisible — the same silence, arriving later —
and closing that would mean 74 reviews on every resync for a state that
changes when a person changes it. Whether that trade is worth taking is the
open question; nothing else here is.

**A group's count of live servers briefly touched zero under sustained
churn** before recovering — observed during Task 5, not investigated, and
recorded here for whoever next looks at backoff and replacement timing.

## From milestone 6b (NetworkPolicies)

What the two `NetworkPolicy` objects buy and what they do not is
[`network-boundaries.md`](network-boundaries.md): measured scope, not defects.
One thing from that milestone is a defect and stays here.

**A `Forbidden` on the policy write is reported, and the audit still cannot
see the case that produced it.** `reconcileNetworkPolicy` runs before anything
else the Network reconcile does, and a failure there is fail-closed by design:
the namespace must not quietly come up unprotected, so every group in it
refuses. Since 2026-08-26 it is also *named* — `Accepted=False` with reason
`NetworkPolicyNotWritten` and the API server's own refusal in the message —
where before the condition was never persisted at all and every group quoted
`network "..." has not been accepted yet`, which is true and misleading in the
same breath.

What is still open is upstream of that. *Driven 2026-08-25:*
`networkpolicies: create` was removed from the kubebuilder marker **and** from
`internal/rbacaudit`'s table. Absent from both, so the audit is green, and the
startup self-check does not catch it either — it checks the table, and the
table no longer asks. **Nothing in this repository compares the required table
against what the code actually calls.** A verb dropped from both places is
invisible until a cluster refuses it, and the only reason that case is now
survivable rather than mysterious is that the refusal reports itself when it
arrives.

## From milestone 6e (CI)

GitHub Actions now runs three workflows: `.github/workflows/ci.yml` blocks
four jobs — `test`, `lint`, `deps`, `e2e` — on every pull request and on
push to `master`; `.github/workflows/nightly.yml` runs `make image-repro` on
a schedule plus `workflow_dispatch`; `.github/workflows/release.yml` runs
`hack/publish.sh` on a `v*` tag. Full account:
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
