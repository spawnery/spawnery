# What the NetworkPolicies buy, and what they do not

Spawnery writes two `NetworkPolicy` objects: one per accepted `Network`, into
that network's own namespace, selecting its server pods; and one beside the
operator, selecting the operator pod.

This page is what they are worth. It is not in
[`known-issues.md`](known-issues.md) because none of it is a defect — every
line is a measured statement of scope, with the alternative considered and, in
each case, declined for a reason. A security feature whose limits are not
written down is read as covering more than it does, which is the failure this
page exists to prevent.

**The one sentence to read first: whether any of this refuses anything is a
property of the cluster's CNI, and this repository's own end-to-end harness
runs on one that enforces nothing.** Everything below distinguishes what the
objects *say* from what has been observed to happen.

Paths written `config/deploy/` are where a file was in milestone 6b. Milestone
6d moved that directory into `charts/spawnery/templates/`; the old paths are
left where they date a measurement.

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


## How many agents may reach the operator

**The NetworkPolicy beside the operator says who may open a connection to the
agent endpoint. It says nothing about how many, and it cannot.** Its ingress
peer is a `podSelector` over `spawnery.cloud/managed-by` — the same forgeable
label as everywhere above — and vanilla NetworkPolicy has no concept of a
count. Whatever bounds the number has to live in the operator, and does:
`internal/agentserver`'s `PeerLimiter`, which refuses at `Accept`, before the
TLS handshake that is the expensive half of a connection.

There are two bounds and they answer different questions.

`MaxConnectionsPerPeer` bounds one peer address, which on an un-NAT'd pod
network is one pod. A legitimate agent's peak is 2, measured over roughly
seventy renewals across four paths; the bound is 8, and the slack is deliberate
because being too low costs a working agent its session.

The fleet bound is that slack's answer. Eight per pod is a factor nobody would
grant a fleet in aggregate, and until 2026-08-26 nothing did anything about
it — a set of compromised pods was simply a multiple of the one-pod bound.
What closes it is a number the operator already had and had never compared to
anything: the count of pods it manages, exported as `spawnery_agents_expected`.
Above four times that many connections open in total, every peer's bound drops
to `FleetConnectionsPerAgent` (4, twice the measured peak, so no working agent
is refused anything it would have asked for); above eight times, connections
are refused whatever peer they came from.

**Both are derived from the fleet's own size, and that is the whole design.** A
fixed ceiling would be a number legitimate growth eventually reaches, and the
agent it refused on that day would be whoever asked next — one namespace's
traffic becoming another namespace's outage, which is the harm the agent
channel's isolation promise is about, moved rather than removed. A ceiling that
is a multiple of the pod count counts every legitimate agent in the number that
bounds it, so growth raises it in lockstep and only connections that are not
agents doing their job can reach it.

**What is still not bounded is the number of peers.** A pod that is not in the
operator's caches at all still gets its own allowance until the fleet ceiling
binds, because what admits it is the policy above, which passes any labelled
pod and counts nothing. Deriving the bound from the pod count narrows what the
fleet may hold; it does not decide who is in the fleet, and nothing at `Accept`
can — identity arrives with the bearer token, two round trips later. When the
fleet ceiling does bind, the connection refused is whichever arrived next, and
it may belong to an agent that has done nothing wrong. That residual is paid
only in a cluster already holding eight times the connections its pods can
account for, and `SpawneryAgentFleetOverItsBound` in the chart's PrometheusRule
is what says so out loud.

## `HostPort`, Pod Security, and the host firewall

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


## A shared LoadBalancer address, and when Cilium refuses one

**Cilium will not share a LoadBalancer address between two `Local` Services
that select different pods.** Non-overlapping ports are necessary and not
sufficient. Measured:
`"compatible ExternalTrafficPolicy local but selecting different set of pods"`.
This is a property of `externalTrafficPolicy: Local` — the announcement would
be wrong for whichever Service lacks an endpoint on a given node — and it means
a cluster whose address pool is exhausted must choose between real client
addresses and a shared address.

