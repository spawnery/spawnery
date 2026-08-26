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

**A `Forbidden` on the policy write stops the whole namespace, and it names the
wrong thing.** `reconcileNetworkPolicy` is called after the `Accepted`
condition is set on the in-memory object but before any `Status().Update`
(`internal/controller/network_controller.go`), so an error there returns before
the condition is ever persisted. Both group controllers gate on
`Accepted=True`, so a *fresh* `Network` in a cluster where the operator cannot
write NetworkPolicies never becomes usable, and every group in that namespace
refuses with `network "..." has not been accepted yet` — true and misleading in
the same breath, because the network *was* accepted and the acceptance could
not be written down. An existing `Network` keeps its persisted condition and
its groups keep running, so only new ones are affected.

Failing closed is right: an unprotected namespace must not quietly come up. The
ordering was reviewed and left alone, because persisting `Accepted` before
writing the policy would let groups start servers in a namespace with no policy
at all. **What is missing is a report naming the cause**, which was available
and was not shipped; nothing but the operator log says why.

*Driven 2026-08-25.* `networkpolicies: create` was removed from the kubebuilder
marker **and** from `internal/rbacaudit`'s table — the sharpest form of the
case, absent from both, so the audit stays green and only the running operator
knows. The startup self-check does not catch this one either, for the same
reason: it checks the table, and the table no longer asks. What did change is
that the harness now reports it — every `eventually` that gives up reads the
operator's log and names any denial it finds, so the cause lands in the failure
message of the scenario that actually stalled instead of in a scenario twenty
places later that the run never reaches.

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

**The events grants are right and minimal, and that is readable in client-go
rather than inferred from a green e2e run — which is the only reason to
believe it.** Migrating to `events.k8s.io/v1` needed its own RBAC grant, and
the evidence for it being sufficient was one `make e2e` `PASS`, which reaches
about as far as the rest of this entry says. The verbs do not need it. In
client-go v0.36.0, the events broadcaster's `recordEvent`
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
