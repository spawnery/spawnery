# What the NetworkPolicies buy, and what they do not

Spawnery writes a `NetworkPolicy` per accepted `Network`, into that network's
own namespace, selecting its server pods; one beside the operator, selecting the
operator pod; and one per `ProxyGroup`. This page is what they are worth —
measured scope rather than a list of faults. A security feature whose limits
are not written down is read as covering more than it does.

**The one sentence to read first: whether any of this refuses anything is a
property of the cluster's CNI, and this repository's own end-to-end harness runs
on one that enforces nothing.** Everything below distinguishes what the objects
*say* from what has been observed to happen.

## The harness's CNI enforces nothing, and that was measured

**kindnet, the CNI the end-to-end harness runs on, was measured not to enforce a
NetworkPolicy ingress rule — and measured is the operative word.** With the
peerless kubelet-probe rule deleted from the operator's policy, leaving a policy
that selects the operator pod (so default-deny for ingress) and admits only the
agent peer on 9443 — so the kubelet's probe to the health port is denied
outright by the object in force — `make e2e` stayed green: `deployment
"spawnery-operator" successfully rolled out` on its usual timeline, all twelve
subtests passing.

Two alternative explanations were closed. The denied path was genuinely
exercised: the readiness probe is an `httpGet` to `/readyz` on the health port,
which travels the real network path, and `kubectl rollout status` cannot return
success without one passing. And the mutated policy was genuinely in force
rather than left over: `hack/e2e.sh` creates the cluster afresh on every run,
and that run's apply log read
`networkpolicy.networking.k8s.io/spawnery-operator-agent created` rather than
`unchanged`.

That leaves one explanation: the CNI passed traffic its policy denied. The scope
is **one ingress rule, on one path**. That kindnet implements no NetworkPolicy
controller at all — neither direction, for any pod — is what kindnet's own
documentation says, and a mechanism is not evidence, a CNI's README included.
The practical difference is nil, and every section below leans on it: on this
harness nothing written here has been shown to refuse anything.

## What the policy defends, and what it does not

**A game namespace is one trust domain, and the per-`Network` policy is not a
boundary inside it.** Its ingress peer is a podSelector over labels a pod's own
creator chooses. Measured on 2026-08-21 against Cilium on `paulwtf`:

- a pod carrying `spawnery.cloud/managed-by`,
  `spawnery.cloud/network=production` and `spawnery.cloud/role=proxy` — labels
  anyone creating a pod may write — **connected to a backend on 25565**;
- the same pod without them **timed out**;
- and an ordinary unlabelled pod **mounted `velocity-forwarding-secret`**, 44
  bytes of it, because any pod may mount any Secret in its own namespace.

So `pods: create` in a game namespace is access to that network by two
independent routes, and the label filter's forgeability adds nothing to whoever
holds it. Nor can the operator close it: vanilla NetworkPolicy's peers are
`podSelector`, `namespaceSelector` and `ipBlock`, and inside one namespace the
first is forgeable and the second says nothing. **No policy expressible here
tells a real proxy from an invented one.** Both ways out were considered on
2026-08-21 and neither taken: proxies in a namespace of their own, so a
`namespaceSelector` could discriminate, breaks "a Network owns its namespace";
an admission webhook forbidding foreign pods the `managed-by` label brings
certificates and a failure mode to an operator that has no webhooks.

What the policy does defend, and defends well, is the co-tenant that *cannot*
create pods — a compromised workload cannot relabel itself, and the timeout
above is what that looks like from the inside. Where a CNI enforces, it does
refuse: that timeout, and on 2026-08-25 an egress-deny policy cutting a proxy
agent's stream. The `spawnery.cloud/network` label also keeps a *losing*
Network's pods, in a two-Network namespace, outside the winner's policy. What
all of it is written against is that a Paper server runs `online-mode=false`,
authenticates nobody, and trusts whatever completes the modern-forwarding
handshake with the right secret.
[Installing the operator](../getting-started/index.md) says so where an
administrator chooses a namespace.

## Why no policy selects the proxy pods

**No ingress policy selects proxy pods, because of an asymmetry in how the two
pod classes are probed.** A server's readiness probe is an `exec` of
`spawnery-slp` against `127.0.0.1:25565` (`internal/podspec/server.go`), inside
the container over loopback, which no NetworkPolicy governs; a proxy's is a
`TCPSocket` from the kubelet to `ProxyReadyPort` (`internal/podspec/proxy.go`),
which one might. Selecting proxies would have put the whole fleet's readiness at
the mercy of whether a given CNI subjects kubelet traffic to policy. The price
is stated rather than hidden: **nothing restricts who may open a TCP connection
to a proxy's 25565 from inside the cluster.** The proxy is the public front
door, behind a NodePort with `externalTrafficPolicy: Local`, so a rule there
would have to admit the world on that port anyway — and unlike a backend it
authenticates its players.

## Proxy egress, written per group since 0.2.33

Each `ProxyGroup` owns an egress-only policy (`<group>-proxies`) admitting
cluster DNS, the operator's agent port and the backends of its own network on
25565. A proxy with `onlineMode: true` also has to reach Mojang's session
servers, whose addresses are neither stable nor discoverable and which a
`NetworkPolicy` cannot name by DNS — so it is admitted the internet on 443
through an `ipBlock` over `0.0.0.0/0` that leaves out the private ranges and
link-local, where a cloud's metadata endpoint hands out node credentials. A
proxy with `onlineMode: false` authenticates nobody and gets nothing beyond the
three. Egress only, for the same reason the backend policy never selected
proxies.

Backend egress is *written* against by the per-`Network` policy, whose egress
half **admits** cluster DNS and the operator's agent port and nothing else —
admits being the honest verb, since whether anything is thereby restricted is
the CNI's business. That narrowness is safe because a backend needs Mojang for
nothing: `online-mode=false` by construction, so the Yggdrasil key fetch is
already gone. Paper's own update check to `fill.papermc.io` is the one outbound
call left, and `internal/render/paper.go` turns it off by default for that
reason; turned back on it fails harmlessly anyway, measured with no network
reachable — the server still reaches `Done` and answers a ping.

## Whether a pod-selector egress rule survives Service DNAT

**Settled for Cilium and for no other CNI.** Two of the per-`Network` policy's
egress rules name a pod or namespace selector while what the pod actually dials
is a Service ClusterIP that kube-proxy DNATs: the operator hop
(`podspec.OperatorPodLabels()`, against `spawnery-operator.<ns>.svc`) and the
resolver hop (`kube-system` by namespace selector, against the cluster DNS
Service). A CNI evaluating policy pre-DNAT would drop both, and the rule would
have to be an `ipBlock` over the Service CIDR instead — which the operator
cannot discover from inside the cluster. The pod-selector form is what ships,
and which side any CNI falls on is not asserted.

On Cilium both rules match: `paulwtf` carries `production-backends` in
`minecraft` selecting `role=server`, and a backend under it reaches `Ready`,
which is granted only once that server's agent has connected — so it resolved
the operator's name *and* dialled it through the ClusterIP. Verified again
2026-08-25 on a pod rolled that day.

That is one CNI, not the class, and it cannot be widened where testing would be
cheapest: kindnet enforces nothing, so an egress rule that matches and one that
does not are the same green in `make e2e`. The two failure symptoms diverge and
the misleading one comes first — the operator hop failing looks like agents that
never register; DNS failing looks like nothing resolving at all, the operator's
name included, so agents failing to register is the downstream effect and
checking it first leads away from the cause.

## The peerless rule, and the one test that guards it

**The widest-open thing here is an ingress rule with no `from` at all.** The
operator's own `NetworkPolicy` (`charts/spawnery/templates/networkpolicy.yaml`)
admits 8081 and 8080 from anywhere in the cluster, because the kubelet's source
is a node rather than a pod and no selector names it. That is the only
formulation correct on every CNI, and also the rule where a mistake is worst: an
extra port there admits that port from every source in the cluster. Since the
harness enforces nothing, the manifest test in
`internal/rbacaudit/deploy_envtest_test.go`, which reads the rendered chart, is
the only thing in this repository standing behind it. It checks both directions
— while it checked one, adding port 9999 to the peerless rule left it green —
and it matches the container port *named* `agent` rather than the number 9443,
so it survives a port change. The most dangerous mutation of all, adding 9443 to
the peerless rule, is caught.

## How many agents may reach the operator

**The NetworkPolicy beside the operator says who may open a connection to the
agent endpoint. It says nothing about how many, and it cannot.** Its ingress
peer is a `podSelector` over `spawnery.cloud/managed-by` — the same forgeable
label as everywhere above — and vanilla NetworkPolicy has no concept of a count.
The bound has to live in the operator, and does: `internal/agentserver`'s
`PeerLimiter`, which refuses at `Accept`, before the TLS handshake that is the
expensive half of a connection.

`MaxConnectionsPerPeer` bounds one peer address, which on an un-NAT'd pod
network is one pod. A legitimate agent's peak is 2, measured over roughly
seventy renewals across four paths; the bound is 8, and the slack is deliberate
because being too low costs a working agent its session.

The fleet bound is that slack's answer, closed since 2026-08-26: eight per pod
is a factor nobody would grant a fleet in aggregate. What closes it is a number
the operator already had: the count of pods it manages, exported as
`spawnery_agents_expected`. Above four times that many
connections open in total, every peer's bound drops to
`FleetConnectionsPerAgent` (4, twice the measured peak, so no working agent is
refused anything it would have asked for); above eight times, connections are
refused whatever peer they came from. Both are multiples of the fleet's own size
rather than fixed numbers: a fixed ceiling is one legitimate growth eventually
reaches, and the agent it refused that day would be whoever asked next.

**What is still not bounded is the number of peers.** A pod in none of the
operator's caches still gets its own allowance until the fleet ceiling binds,
because what admits it is the policy above, which passes any labelled pod and
counts nothing. Nothing at `Accept` can decide who is in the fleet — identity
arrives with the bearer token, two round trips later. When the fleet ceiling
does bind, the connection refused is whichever arrived next, and it may belong
to an agent that has done nothing wrong. That residual is paid only in a cluster
already holding eight times the connections its pods can account for, and
`SpawneryAgentFleetOverItsBound` in the chart's PrometheusRule is what says so
out loud.

## `HostPort`, Pod Security, and the host firewall

**`HostPort` and CIS `restricted` cannot both hold in one namespace.** Pod
Security `baseline` — which `restricted` inherits, per the Kubernetes Pod
Security Standards rather than anything measured here — disallows a container
`hostPort` outright, so a namespace enforcing either refuses every `HostPort`
pod's create, and `ProxyGroupReconciler` reports the refusal on the group's own
`Degraded` condition (`ReasonProxyPodRejected`) rather than ever admitting one.
`baseline` is the one thing here observed being enforced by a real API server,
in envtest (`internal/controller/expose_test.go`,
`TestARejectedProxyPodIsReportedOnTheGroup`) and in `make e2e`
(`test/e2e/expose_test.go`, `aForbiddenHostPortIsReportedOnTheGroup`).

`restricted` itself is measured too, on `paulwtf` on 2026-08-25: a throwaway
namespace labelled `enforce: restricted`, a `Network`, and one `ProxyGroup` at
`expose.type: HostPort` port 25577. The group went
`Degraded=True`/`ProxyPodRejected` and quoted the API server verbatim —
`violates PodSecurity "restricted:latest": hostPort (container "velocity" uses
hostPort 25577)` — with no pod in the namespace at any point. Driven against the
deployed `v0.2.0` operator rather than a working tree, which makes it a
statement about what ships.

`restricted` against a game namespace otherwise holds. Measured on `paulwtf` on
2026-08-22: the `minecraft` namespace enforces `restricted` (`enforce` and
`warn` both), and the one `ProxyGroup` in it exposes `ClusterIP` with two ready
replicas beside a `Ready` `Server`, all of it up for over two days.

`HostPort` under the real CNI works, and that is not the interesting half.
Measured on `paulwtf` on 2026-08-25: a namespace of its own with no Pod Security
label and one `HostPort` `ProxyGroup` at port 25577 — the pod was admitted, went
`Ready`, and the group published `status.address: 45.137.203.198:25577`, having
withheld it until a ready pod actually declared that `hostPort`. So the CNI
implements `hostPort`: `cilium-config` runs `cni-chaining-mode = portmap` with
`kube-proxy-replacement = false`, the portmap plugin's job rather than Cilium's
eBPF.

What a player would meet is a different object entirely. From outside, 25577
times out while 25565 and 443 on the same node IP connect instantly — the
difference is `paulwtf`'s own `CiliumClusterwideNetworkPolicy`
`host-firewall-ingress`, which admits from `world` exactly 6443, 80, 443, 25,
465, 587, 143, 993, 5432 and 25565, plus ICMP echo, and drops the rest. **So
`HostPort` on this cluster is a host-firewall question, not a CNI one**, and the
remedy is one port in that policy rather than anything in this operator.

The Pod Security half stays a trap, because the code cannot make the two
compatible. Its remedy is the runbook's: give the namespace running the
`HostPort` `ProxyGroup` a relaxed Pod Security label, or a namespace of its own,
separate from the `restricted` namespaces the rest of the network runs in — and
read the host firewall beside it, because that namespace is necessary and not
sufficient.

## A shared LoadBalancer address, and when Cilium refuses one

**Cilium will not share a LoadBalancer address between two `Local` Services that
select different pods.** Non-overlapping ports are necessary and not sufficient.
Measured on `paulwtf`: `"compatible ExternalTrafficPolicy local but selecting
different set of pods"`. This is a property of `externalTrafficPolicy: Local` —
the announcement would be wrong for whichever Service lacks an endpoint on a
given node — and it means a cluster whose address pool is exhausted must choose
between real client addresses and a shared address.

## What a group may ask of the scheduler

`spec.scheduling` on a group — tolerations, `nodeSelector`, affinity — and
`expose.hostPort.port` on a proxy group reach past the namespace: onto a
control-plane node, a node cordoned for maintenance, or beside a workload in
another namespace, and onto one host port per node. Until 0.2.33 they were
copied into the pod as written, so whoever could write a group could place a pod
running an image of their choosing anywhere in the cluster. Since then the
Network decides, and absent a `Network.spec.scheduling` allowlist nothing is
allowed — the fields, the two shapes no network can ever allow, and the refusals
are in [Where game pods land, and who
decides](../guides/scheduling.md).

It is a check on the group, not on the pod: a `pods: create` grant in the
namespace is still equivalent to placing a pod, as the section above says.

## What the operator may delete

Since `ScaleBoost` exists, the operator deletes an object a person may have
created by hand. It is bounded twice and both bounds are in the grant rather
than in code: the verb list carries `delete` on `scaleboosts` and on nothing
else it does not already own, and the sweep removes only boosts whose own
`expiresAt` has passed. It has `create` as well, since `/cloud boost` — the
caller that justifies the grant — and no `update` or `patch`: a boost is made
whole and expires, never edited.

## What the operator knows about a person

The operator holds, for every player on a network, their Minecraft UUID, their
username, and the backend they are on.

**It is in memory and nowhere else.** `agent.Registry` keeps it per proxy
session; it reaches no custom resource, no etcd, no log line at default
verbosity and no metric label. The last is deliberate twice over: a metric
labelled by player name would multiply every series by the player base, and it
would turn a live figure into whatever the monitoring stack's retention is. The
one log line that can mention a roster is the refusal at `V(1)`, carrying the
reason and no player. It expires on its own, too: a roster older than twice the
report interval is skipped, and a proxy whose stream is gone contributes
nothing, so an operator that stops hearing from a proxy serves no frozen list.

**Every agent in the namespace receives it** — every Velocity proxy and every
Paper backend gets every player in that namespace, by name and UUID, on connect
and on each resync, as part of the `NetworkState` the plugin API is built on. So
a compromised game server pod learns who is on the whole network. The namespace
is the horizon, because the state is built from a List scoped to the pod's own
authenticated namespace.

**An agent may ask that a player be moved, and that is the whole of it.** No
verb creates, deletes or resizes anything, and it cannot reach another network —
structurally rather than by a check: a request names a player and a target and
carries no namespace, and the operator resolves both inside the namespace the
pod's own ServiceAccount token authenticated. There is no field to put another
network's name in, so it is not a guard a later edit can drop. A rate bounds the
rest: eight requests back to back per pod, one token back per second, counted
per pod so that a noisy one cannot spend another's budget.
`spawnery_agent_requests_refused_total` publishes the refusals by reason, and a
rising `RATE_LIMITED` is the shape a misbehaving or compromised plugin has.

**A server may describe itself to the rest of its network, within stated
bounds.** A backend agent may publish a short state and a handful of key/value
attributes, which the operator carries into the `NetworkState` every other agent
in the namespace receives and reads none of: no scheduling, routing or scaling
decision looks at a word, which is why free-form text is acceptable here and
nowhere else on this channel. So a compromised pod can put text of its choosing
in front of every plugin in its own network, and a plugin that treats what
another server said as an instruction has built a path from one pod to another
that no policy here bounds. Bounded are the size, the reach and the name: a
state of at most 64 characters and at most 16 attributes of at most 64 and 256,
refused rather than trimmed; the namespace, by the same scoped List; and the
name, which is the pod's own from its token, since an `AnnounceRequest` has no
field for one. It reaches no custom resource and no etcd: it lives in the
operator's memory for as long as that pod has a session, and is gone with it.

**The one thing not bounded is who may read it inside the operator.** Anyone who
can reach that process — a debugger, a core dump, a memory-reading exploit —
reads the roster, and no `NetworkPolicy` in this repository is about that. What
does bound it is the channel's own authenticity, in [What a compromised game
server can do](agent-trust.md): a pod-bound token over a connection pinned to
the operator's CA, and a namespace taken from that token rather than from the
message, so a proxy can assert a roster for its own network and for no other.

## A plugin needs no permission to read it, and cannot be given one

Bukkit permissions attach to a `CommandSender` and Velocity's to a
`CommandSource`; both are about a player or the console, and neither platform
has a `Plugin.hasPermission`. A plugin calling `Spawnery.api()` presents no
identity there is anything to check, so a gate would have to be invented — a
list of trusted plugin ids, say — and it would be worth nothing, because any
plugin on the server already reads the platform's own player list, loads
classes, and calls whatever the JVM exposes. So the boundary is the one that
already exists: **who may install a plugin**, which is who may create a pod in
that namespace, which [Installing the
operator](../getting-started/index.md) tells an administrator to treat as one
trust domain.

The `/cloud` command is the different case and does gate. A command has a
source, so a permission is expressible there. It carries three —
`spawnery.cloud.read`, `.retire` and `.scale`, listed with what each costs in
[The `/cloud` command](../guides/cloud-command.md) — and the split is not
cosmetic: reading the network is what a moderator gets, and adding servers
spends money.

**That gate binds a person, not a pod.** A permission decides who may ask; the
operator's own bounds — a ceiling a boost cannot lift, a duration it cannot
exceed, a namespace it structurally cannot leave — decide what may be asked
for, and they hold underneath the command as well as behind it.
