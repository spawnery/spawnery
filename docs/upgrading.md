# Upgrading an installation across a rename

Three objects changed name during development, and the operator renames
nothing: it writes the new name and leaves whatever the old code wrote sitting
there. None of this applies to an installation created at v0.1.0 or later —
the only one that exists was installed 2026-08-20, milestones after all three
renames, and was checked clean on 2026-08-22. This page is for whoever finds
an older one.

Nothing here is an open defect. `docs/known-issues.md` carries those.

## An operator upgrade can roll every proxy in the cluster

Nobody has to edit a spec. A proxy pod is stale when its
`spawnery.cloud/pod-hash` label differs from a digest of the pod the operator
*would* render for its group right now, and that digest is taken over the
rendered pod rather than over a chosen list of spec fields
(`2026-08-14-proxy-rolling-updates-design.md` §3.1). So a change to the
*rendering code* -- a new default in `internal/podspec`, an added environment
variable, a renamed label -- moves the digest for every `ProxyGroup` while
every spec stays byte for byte what it was.

There is a second trigger nobody would guess from the spec: the
`agentEndpoint` handed to the renderer feeds the digest, and it is
`spawnery-operator.<operator-namespace>.svc:9443`. Moving the operator to a
different namespace, or restarting it with a different `--operator-namespace`
or `POD_NAMESPACE`, rolls the whole fleet with no image, no rendering change
and no spec edit involved. The Helm chart is the first thing that makes that a
routine operation.

This is an accepted cost rather than a defect. The alternative is milestone
4b's rule for ServerGroups -- roll on any `metadata.generation` change -- and
for a proxy group that is worse, because `replicas` is the routine edit there
and a generation rule would make every scale-up and scale-down a full
replacement, each pod waiting out an attrition-bound drain.

**The group's status will not tell you.** The surge pod comes up before any
old pod is withdrawn, so `readyReplicas` holds at `replicas` and the phase
reads `Ready` throughout, exactly as when nothing is happening. The pod label
is what says so:

```bash
kubectl get pods -n <ns> -l spawnery.cloud/role=proxy -L spawnery.cloud/pod-hash
```

Two distinct values inside one group means that group is mid-roll; one value
everywhere means done or never started. Every group starts within a reconcile
of the new operator coming up, one pod at a time per group but all groups at
once -- nothing serialises across groups. Each replaced pod runs the ordinary
drain, so players keep playing and are disconnected only if still there when
`spec.drain.timeoutSeconds` elapses, with one `Warning ProxyDrainTimeout` per
pod naming what it cost. A busy fleet upgraded at peak disconnects, per group,
whoever is still on each proxy at each deadline.

**Finding out before you upgrade.** The code trigger now answers for itself:
`internal/podspec/hash_golden_test.go` pins `DesiredProxyHash` and
`DesiredServerHash` over frozen fixtures, so a change to either render path
fails on the pull request that makes it. Comparing two builds after the fact,
the cheap negative filter is `git diff <old>..<new> -- internal/podspec/`; if
nothing in the pod-render path moved, the digest cannot have. That is how
2026-08-22's v0.1.2 to v0.2.0 upgrade was known to be safe in advance, and it
was: both proxies kept `pod-hash 2dd6593373a4ffd2` and 46 hours of uptime,
because the only file that had moved was `netpol.go` and only its comments.

Neither the golden tests nor the diff covers the triggers outside the code --
the group's own namespace and name, the `Network`'s name, and the agent
endpoint above. For those, run the new build against a scratch cluster over
*the same* manifests and compare the `pod-hash` it stamps with what the
running pods carry. Different manifests tell you nothing about your fleet.

## Upgrade the proxy images before the operator

A new operator against proxy images that predate milestone 4c-1's `SetReady`
empties nobody and disconnects everybody at the deadline. What it looks like
first is that nothing happens: `spec.replicas` goes 2 to 1 and the surplus pod
stays `Ready`, stays in the Service's endpoint slice, and goes on receiving
*new* players for the whole drain window. Then the pod is deleted with all of
them on it, and one event is the only record:

```
Warning  ProxyDrainTimeout  proxygroup/gateway  deleting proxy gateway-xxxx after 5m0s with 3 player(s) still connected
```

That is worse than the immediate deletion 4c-1 replaced, which disconnected
the same people without first routing more of them onto a pod it was about to
remove.

The cause is one line of protobuf: `SetReady` is field 7 of
`OperatorToProxy`'s oneof, added by that milestone. An older agent does what
protobuf requires of an unknown field and ignores it, so `ReadyGate.close()`
is never reached, the kubelet's probe keeps succeeding, and the endpoint never
goes away. The deadline bounds the damage; nothing prevents it.

**The signature is annotation plus `Ready`.** The operator writes
`spawnery.cloud/draining-since` whether or not the agent ever hears the
message, so an un-upgraded proxy carries the annotation while its `Ready`
condition is still `True` and
`kubectl get endpointslice -l kubernetes.io/service-name=<group>` still shows
its address as ready. A correctly drained proxy carries the annotation and is
`NotReady`.

Nothing version-gates the message, which is why the order matters. It could:
`Hello` has carried `string version = 1` since the original gRPC contract on
2026-08-08, the agent fills it from its plugin metadata, and the operator
already logs it at V(1) in `internal/agentserver/server.go`. The wire carries
what a gate would need; nobody acts on it yet.

Rolling the *operator* back on its own is safe. An agent that supports
`SetReady` and never receives one behaves exactly as milestone 3c's did:
`ProxyRole`'s latch starts at `Latch(synced = false, asserted = null)` and its
`FULL_SYNC` branch opens the gate unless a `false` was asserted --
`if (!previous.synced && previous.asserted != false) onFirstSync()`.

## 0.2.2: the images roll both fleets, the operator rolls nothing

0.2.2 is a Velocity agent change and nothing else: `Drain` now moves a player
who lands on a draining server after the drain began, which is the arrival
`DrainPlayers` used to miss. The Paper agent is untouched.

Both images carry the tag anyway, because `flake.nix` holds one
`imageVersion` for both, and a `Network` names both. So a Paper fleet that
gained nothing rolls on this upgrade, one server at a time, drain-aware — the
cost is the rollout, not any player's session. The alternative is two agent
versions to keep straight against one operator, and that trade was made
deliberately; the reasoning sits beside `imageVersion` itself.

**The operator moves to 0.2.2 too, and rolls nothing by itself.** The section
at the top of this page explains that an operator upgrade *can* roll every
proxy in the cluster, because the pod hash is a digest of the rendered pod and
a change in the rendering code moves it. This release does not change that
rendering: `internal/podspec`'s golden hash test
(`internal/podspec/hash_golden_test.go`) still passes unchanged, which is the
check that would fail if either `DesiredServerHash` or `DesiredProxyHash` had
moved. What the operator gained is a startup permission self-check, a refusal
to adopt a ConfigMap it does not own, a report on a duplicated ordinal, a drain
that completes on a node whose group's `Network` is broken, and a reconnect
grace derived from measurement — none of which reaches a pod's rendered shape.

So the only rolling this release causes is the one a `Network` asks for by
naming the new image, and that is the paragraph above.

**No ordering requirement in either direction**, unlike the `SetReady` case
above. Nothing new is on the wire: the change is entirely inside the proxy
agent's own reaction to a `DrainPlayers` message it has understood since
milestone 3c. An old proxy image against any operator behaves as it always
did — it misses the late arrival, which is the defect, not a new failure — and
a 0.2.2 proxy against an older operator behaves correctly. Roll them in
whatever order suits.

What this does *not* close is the operator's half, and
`docs/known-issues.md` carries it: `Occupied()` still reads only the backend's
count, so a `DeletePod` decided between a player's arrival and the agent's
move still lands on someone. The window is much smaller and it is not zero.

## 0.2.3: drains take a report interval longer, on purpose

Nothing to do, and one thing to expect. A drain now waits for a count that
was taken *after* it began, where before any sufficiently recent count would
do — and a count from four seconds ago is perfectly fresh while saying nothing
about a player who joined three seconds ago. That was the oldest form of the
gap this release closes.

So every drain is longer by up to one `spec.agent.reportInterval` (five
seconds by default) plus one more second. The extra second is not slack:
`status.drainStartedAt` is a `metav1.Time` and those are truncated to whole
seconds through the API server, so the stamp read back is up to a second
earlier than the drain really started, and the threshold has to clear that.

Where it shows: a rolling update over ten servers takes roughly a minute
longer than it did. Where it does not: the drain deadline is unchanged, and a
server whose agent has gone still leaves on `spec.drain.timeoutSeconds` rather
than waiting for a report that cannot come.

**The images roll both fleets and the operator rolls nothing**, the same shape
as 0.2.2 above and for the same reasons: one `imageVersion` covers both
images, and `internal/podspec` is untouched, so no pod's rendered hash moves.
No ordering requirement either — the new `BackendPlayers` message only ever
*adds* to what the operator counts as occupied, so a proxy too old to send it
contributes nothing and behaves exactly as it did.

## 0.2.4: the refusal counter gains a `bound` label

Nothing to do in a cluster, one thing to fix in a dashboard.
`spawnery_agent_connections_refused_total` used to be a single series and is
now split by `bound`, which is `peer` or `fleet`. A query that named the metric
bare still works and now sums two series that mean different things, so pin the
label: `{bound="peer"}` is one pod over its own limit, `{bound="fleet"}` is the
whole endpoint over what the operator's pod count can account for. The chart's
PrometheusRule ships an alert for each.

There is also a new gauge, `spawnery_agents_expected`: the pods the operator
manages, which is how many agent connections it ought to be holding. It is the
denominator the old alert text told you to work out by hand, and it is absent
rather than zero until the first count succeeds.

**The bound it feeds can refuse connections, so read this if you run agents the
operator does not manage.** Above four times the pod count in open connections,
every peer's limit drops from 8 to 4; above eight times, connections are
refused regardless of peer. A legitimate fleet holds one connection per pod and
two through a handover — an eighth of the second threshold — so nothing in an
ordinary cluster comes near either. What would is a peer the operator cannot
see in its own caches: a pod in a namespace it does not watch, or an agent
reaching it from outside the pod network. Neither is a supported shape, and now
neither is a free one.

## 0.2.4: the permission self-check repeats

The operator has asked the API server what it may actually do since 0.2.2, at
startup. From 0.2.4 it asks again every ten minutes, so a permission revoked
while it runs is reported instead of turning it into a process that looks
healthy and reconciles nothing on the paths that need the verb.

Nothing to do. `--permission-check-interval` sets the cadence and a negative
value restores exactly what 0.2.3 did, one check at startup. The cost of the
repeat was measured before it was chosen: 73 `SelfSubjectAccessReview`s in
54 ms against a real API server, with the client-side rate limiter off, which
is what controller-runtime v0.24 configures by default. A cluster that sets a
client QPS instead should know that the same 73 take 3.4 seconds at 20 and
13.4 at client-go's default of 5.

What to watch is `spawnery_permissions_missing`, a gauge per scope, absent
until the first check answers and left where it was when one fails. The chart's
PrometheusRule alerts on it. In the log, a denial repeats at every check and a
grant is logged only when it is news — at startup, and again when a denial has
been repaired.

## 0.2.4: the agents ping, and the operator still does not

The agents now send a keepalive ping every 45 seconds and give up on a
connection 20 seconds after one goes unanswered. Before this, a connection that
was up and going nowhere — a node hard-powered off, a network black-holing —
held the agent for as long as TCP's own retransmission took to give up:
measured at over 200 seconds and twice not at all within 213. Measured after:
64 seconds, against a stub told to stop reading and writing without closing
anything.

**Nothing on the operator gained a keepalive, and that is deliberate rather
than pending.** The operator already notices a silent agent through its
reports, within twice the report interval, and a transport keepalive would be
slower than that *and* would replace the state it acts on: a broken stream is
tolerated for `StreamDownGrace` and does not start the drain that moves players
off a backend which will never answer. `MaxConnectionIdle` in
`internal/agentserver` carries the argument in full.

Nothing to do, and nothing to configure. The one thing that could have gone
wrong is an operator refusing the pings: gRPC's *default* enforcement policy is
one ping per five minutes, so an agent pinging every 45 seconds would collect
strikes and be sent a GOAWAY. Every released operator sets
`MinKeepaliveInterval` to 30 seconds instead, from v0.1.0 onwards, so no
supported combination hits it. `hack/agent-test.sh`'s seventh phase asserts
that from both sides against a stub carrying the same policy.

**No ordering requirement in either direction.** A 0.2.4 agent against an older
operator pings and is answered, which is the paragraph above. An older agent
against a 0.2.4 operator sends no pings and is asked for none — the operator
gained no keepalive and deliberately never will — so it behaves exactly as it
did, which is to say it waits out a partition the way it always has.

## 0.2.4: Paper moves from build 111 to 119

`nix/paper.nix` named Paper 26.2 build 111 and PaperMC had published 119. The
gap is what `.github/workflows/paper-watch.yml` exists to stop happening again;
this release is the first time it was closed by something other than somebody
remembering to look.

Mojang's server jar did not move — its URL and hash are unchanged, so this is
Paper's own patch level and not a Minecraft version. What is in it, from the
API's own changelog: a `ClientboundLoginCompressionPacket` ordering fix (112),
spark bumped twice (115, 119), Leafpile 1.1.0 (116), velocity-natives 4.1.0
(117), a DataConverter sync (118).

Verified on 2026-08-26 before the pin was taken: `make image-test` and
`make agent-test`, both green against build 119 — Velocity forwarding
negotiated, the agent plugin loaded and linked, a full session driven against
a stub operator, and the CA-rotation handshake. That is what a person does for
a Paper bump, and it is deliberately not what CI does: no job in this
repository runs a Paper server, so a green pull request for a pin bump proves
the fixed-output hashes and nothing else.

**Both fleets roll, the operator rolls nothing by itself.** The image tag is
`<upstream>-<imageVersion>` and carries no build number, so build 119 reaches a
cluster as `paper:26.2-0.2.4` — a `Network` moved to that tag rolls its server
fleet, one server at a time, drain-aware. Unlike 0.2.2, where the Paper fleet
rolled for a Velocity change and gained nothing, this time it is the Paper
fleet that gained something and the Velocity fleet that rolls for the version
bump alone. `hack/publish.sh` is what makes the tag bump obligatory rather than
tidy: it refuses to overwrite a tag that already exists, so republishing
`26.2-0.2.3` with different bytes stops the release rather than mutating what a
running cluster pulled.

The operator itself changes nothing a pod renders: `internal/podspec` is
untouched, so no pod's rendered hash moves, and the agent jars are unchanged.

## 0.2.5: the proxies report their read timeout, and a Network says what it means

A `Network` gains a `RescueWindowShort` condition. It answers how long the
operator has to move players off a backend whose node has died before Velocity
disconnects them itself — the proxy's read timeout less twice the agent report
interval — and it can answer at all because the proxies now report that
timeout on their `Hello`.

Before this the operator assumed the value this repository ships. A
`velocity.toml` overlay lowering `advanced.read-timeout` closed that window with
nothing noticing, which is the last entry `docs/known-issues.md` carried. The
agent reads `ProxyServer.getConfiguration().getReadTimeout()`, so what reaches
the operator is what Velocity actually parsed: after the overlay, after
whatever the image ships, after Velocity's own defaults.

Three readings, and the third is not the second:

| Condition | What it means |
|---|---|
| `False/RescueWindowSufficient` | a proxy reported, and the window clears the operator's own resync |
| `True/RescueWindowTooShort` | a proxy reported, and it does not — players on a dying node may be disconnected rather than moved |
| `Unknown/NoProxyReported` | no proxy in this namespace has said, which is not the same as sufficient |

A namespace with several proxies is judged by the shortest of them: whichever
gives up first is the one that kicks the players.

**Nothing to do, and no ordering requirement.** An agent too old to send the
field reports zero, the registry ignores it, and the operator falls back to the
shipped default — exactly the reading it took before. The condition then says
`Unknown` for that namespace rather than inventing an answer.

## 0.2.6: a capacity edit no longer rolls an ephemeral group

Before milestone 7a, an ephemeral `ServerGroup` treated `metadata.generation`
as the definition of staleness, so *any* field of its spec moving replaced
every server it had. Since 7a it compares `podspec.DesiredServerHash`, and only
an edit that changes the rendered pod or the group's config does that. A
`ProxyGroup` and a `Persistent` `ServerGroup` already worked this way; the
ephemeral rule was the last one on generations.

Nothing has to be done for this, and the direction is the safe one: strictly
fewer changeovers than before, never more. Four things are worth knowing.

**Every ephemeral server that predates the upgrade is adopted, not replaced.**
Servers created before `spec.podHash` had a reader on this side carry an empty
hash, and the first reconcile after the upgrade stamps them with the group's
current one rather than nominating them. So the upgrade itself rolls nothing.
The cost is a bounded one-time window: a spec edit landing inside that same
reconcile is adopted along with the old pod instead of triggering a rebuild. It
closes for good the first time the group reconciles.

**`status.freeSlots` no longer drops to zero on a capacity edit.** It counted
only servers of the current generation, so before 7a every spec edit briefly
published a healthy group as having no free capacity at all. It follows the
render hash now.

**The `Progressing` condition no longer announces a replacement that is not
happening.** It counted servers "of an earlier generation" the same way, so a
capacity edit made it report `N server(s) of an earlier generation are still
being replaced` while nothing was. Its messages now say *spec* rather than
*generation*, because the generation is no longer what they are about.

**A capacity edit still resets the group's failure streak**, and that is the
one thing 7a did not change. `docs/known-issues.md` carries why.

## A chart upgrade brings a fifth CRD, and moves nothing

`ScaleBoost` is installed by the chart from this release on. **Nothing uses it
until somebody creates one**, so the upgrade changes no running group — worth
saying plainly, because "a new CRD" reads as "something is about to move".

What it is: extra capacity for a group, for a while, as an object rather than
as an edit to the group's spec.

```bash
kubectl apply -f - <<'EOF'
apiVersion: spawnery.cloud/v1alpha1
kind: ScaleBoost
metadata:
  generateName: lobby-
  namespace: minecraft
spec:
  groupRef: {name: lobby}
  replicas: 2
  expiresAt: "2026-08-28T20:00:00Z"
EOF
```

It **adds to the group's floor and never to its ceiling**: `maxReplicas` still
binds. Two boosts on one group add up. One with no `expiresAt` never expires,
which is a real need and the known way to end up with four servers in March
and nobody who remembers why.

`kubectl get servergroups` gains a `BOOSTED` column, and the group's
`status.boostedReplicas` says how much of its current floor is not its own
spec. That column is the answer to "why is this group bigger than its
minReplicas", which is otherwise a question with no visible answer.

**It exists because the operator cannot edit a group's spec, and should not.**
Its ClusterRole grants `get, list, watch` on `servergroups` and no write; and
on a GitOps-managed cluster that spec belongs to a file, so a floor the
operator raised would be reverted at the next reconciliation. A boost is the
operator's own object and nothing outside the cluster claims it.

**For a lasting change, edit the `ServerGroup`.** A group that needs four
servers every Saturday needs that in the file a person reviews, not a boost
somebody creates every Saturday.

## A cluster still on `v0.1.1`'s chart has the old CRDs

`v0.1.1` added a fourth `expose` strategy to the `ProxyGroup` CRD's enum. The
upgrade ran and the operator's image moved, but the cluster's CRD never
learned the new value: Flux names a packaged chart after `Chart.yaml`'s
`version`, that number had stayed at `0.1.0`, so the artifact counted as
unchanged and the HelmRelease kept serving the previous chart's templates. The
image moved anyway because the deployment pins its digest in values, which is
exactly what made the failure look like a success.

A tag cannot be moved, so `v0.1.1` is permanently a release whose chart no
cluster can receive. `v0.1.2` moved `Chart.yaml`'s version with the release
and the enum arrived. If you find a cluster that took `v0.1.1`, check the CRD
rather than the operator version:

```bash
kubectl get crd proxygroups.spawnery.cloud \
  -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.expose.properties.type.enum}'
```

Four values means the chart arrived. Upgrading to any later release fixes it,
because every one since has moved `Chart.yaml`'s version.

## The ServerGroup's PodDisruptionBudget — delete it promptly

Before milestone 4c-3 the budget was named after the bare group name; it is
now `podspec.GroupPDBName(group, role)`, so `<group>-server-pdb` or
`<group>-proxy-pdb`. The rename fixed a collision between a `ServerGroup` and
a `ProxyGroup` sharing a name.

The stranded object is worse than a frozen `minAvailable`, and this is why it
is first on this page. It carries the pre-4c-3 *selector* —
`managed-by`, `group`, `occupied`, with no `role` term — and 4c-3 is also the
milestone that put `spawnery.cloud/occupied` on proxy pods. So in a namespace
holding a `ProxyGroup` of the same name, that selector matches occupied
*proxies* too, while its `minAvailable` was only ever counted from occupied
*servers*. `currentHealthy` counts ready pods across everything the selector
matches, proxies included; `desiredHealthy` is the frozen server-only figure;
`disruptionsAllowed` is the difference, and the ready proxies push it up. The
eviction API can then spend those disruptions on occupied server pods,
disconnecting the players on them.

It is a frozen copy of a selector that was fixed in the live reconciler and
that no fix can reach.

```bash
kubectl get pdb -n <namespace>        # look for one named exactly the group
kubectl get pdb <name> -n <namespace> -o jsonpath='{.metadata.ownerReferences[0].name}'
kubectl delete pdb <name> -n <namespace>
```

Protection continues uninterrupted through the new-named object, which
`reconcilePDB` has been maintaining all along.

## The rendered ConfigMap — orphaned, harmless, unannounced

`podspec.GroupConfigMapName` used to return the group's bare name and now
returns `<group>-<role>-config`, for the same collision reason. A group
reconciled under the old code leaves a ConfigMap at the old bare name; nothing
renames it, deletes it, or warns that it is there. Delete it once you have
confirmed the group is serving from the new one.

## A `Persistent` group's stale `Ready: False`

Before milestone 5a the ServerGroup controller published
`Ready: False / NotImplementedInThisVersion` on every persistent group,
unconditionally. 5a removed that block, and it was the only thing that ever
set `ConditionReady` on a `ServerGroup` of either kind — readiness is
`status.phase`. Nothing removes a condition an older operator wrote, so such a
group carries `Ready: False` beside pods that are up and players who are
online.

Nothing in the operator reads it. It misleads a person, and any alert written
on `.status.conditions[?(@.type=="Ready")]`.

```bash
kubectl get servergroup <name> -n <namespace> \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\n"}{end}' | grep -n Ready

kubectl patch servergroup <name> -n <namespace> --subresource=status --type=json \
  -p '[{"op":"test","path":"/status/conditions/<index>/type","value":"Ready"},
       {"op":"remove","path":"/status/conditions/<index>"}]'
```

The `test` operation makes the patch fail loudly rather than remove a
neighbouring condition if the index moved between the read and the write. It
is safe precisely because nothing republishes the condition.

`ReasonNotImplemented` in `api/v1alpha1/common_types.go` has no user left in
the codebase and is kept anyway: it is the exact string an operator meets on
that stale condition, and deleting it would make the string unsearchable in
the repository it came from.

## The agents gain a `/cloud` command, granted to nobody

Every Paper server and every Velocity proxy running the new agent registers
`/cloud`. **No permission is granted to anybody by default**, so immediately
after the upgrade the command answers "unknown command" to every player on the
network.

That is the safe state, and it is worth saying plainly because it looks like a
bug. Brigadier hides a branch a source may not use rather than refusing it —
which is the platforms' own convention, and better than a lecture — so an
ungranted player cannot tell "you may not" from "there is no such command".

The three permissions and what each one costs are in
[the chart's README](../charts/spawnery/README.md#the-cloud-permissions). The
short version: `spawnery.cloud.read` changes nothing, `spawnery.cloud.retire`
takes a server out of rotation without moving anybody, and
`spawnery.cloud.scale` spends money.

**The console holds all three without being granted anything**, by default on
both platforms. **On a pod rendered by v0.2.7 you cannot reach it**, and on
anything later you can: v0.2.7 set neither `stdin` nor `tty` on the container,
so `kubectl attach` connected and the keystrokes went nowhere — measured on a
live 0.2.7 lobby, where `cloud list` produced no line in the log at all. The
release after it sets `stdin`, so

```bash
kubectl attach -i lobby-a3f9 -n minecraft -c minecraft
```

reaches the console and `cloud list` answers. That is how to check an upgrade
worked without granting a permission to anybody first — and on a network being
brought up, there is nobody to grant one to.

**That fix rolls every group once.** The container spec is part of the pod
hash, so the operator upgrade that brings it replaces every proxy and every
server: players moved off proxies, worlds stopped and restarted. It is a
one-time cost and it is recorded in `internal/podspec/hash_golden_test.go`
rather than discovered.

If you are on v0.2.7 and cannot upgrade yet, granting a permission to a player
is the only route in.

On a Velocity proxy the console's permissions come from a
`PermissionFunction` that a permissions plugin is free to replace; the default
is `ALWAYS_TRUE`. On Paper the console answers every permission itself.

**Nothing about this upgrade moves a running server.** The command is
registered at plugin enable, which happens on a pod that is starting anyway.
Whether the agents roll at all is decided by the image change, exactly as it
was before this feature existed.

## Cloud events reach chat, and are silent until somebody is granted them

An administrator holding `spawnery.cloud.events` now sees things happening in
the cloud as chat lines — a server becoming ready, retiring, failing to be
scheduled. Nobody holds it by default, so **the feed is silent immediately
after the upgrade**, exactly as `/cloud` itself is.

**They are the events `kubectl get events` already shows.** The operator
records through one recorder, and the chat copy is derived from that same call
rather than computed beside it. Two independent derivations of one fact
eventually disagree, and the one in the chat is the one nobody can audit — so
there is only one. If a line appears in chat, `kubectl get events` has it, with
the same sentence.

**The feed collapses.** A rolling update of a ten-server group produces ten
`Ready` transitions in a few seconds, and arrives as one line:

```
[cloud] 3 ReadyGatePassed in lobby (lobby-a3f9, lobby-b71c, lobby-c02e)
```

Warnings are never folded into such a line and each keeps the operator's own
sentence — a failure hidden inside "3 servers ready" is the one event somebody
actually needs to see.

**`/cloud events off` lasts for the session.** Paper could persist it per
player and Velocity has no equivalent, so symmetry won and the command says so
in its own output. The feed is back after a rejoin.

**Nothing is sent to a server nobody is watching.** Each agent tells the
operator whether anybody holding the permission is online, and the operator
sends events only to those that said yes. On a network that grants `.events` to
nobody, this feature costs no traffic at all.

Plugins can subscribe too, through `SpawneryApi.events()`. They receive the
events one at a time rather than the collapsed summary — see
[`agent/api/README.md`](../agent/api/README.md). It is a feed and not a ledger:
an agent that was disconnected missed what happened while it was gone, and the
network picture it re-syncs on reconnect is the correction.

## Plugins can come from a volume, and nothing moves until you ask

`ServerGroup` and `ProxyGroup` gain `spec.extraPlugins.claimName`: a
`ReadWriteMany` claim whose contents are copied into every server's plugins
directory on start. It exists so a plugin change costs a restart rather than an
image rebuild and a release. [`plugins.md`](plugins.md) is the whole of it.

**This upgrade moves no pod.** A group that names no claim renders exactly the
pod it rendered before, so both golden pod digests in
`internal/podspec/hash_golden_test.go` are unchanged — checked, not assumed.
Worth saying plainly, because "a new field" reads as "something is about to
change".

**It is inert twice over.** The operator refuses a group naming a claim unless
it was started with `--allow-plugin-volumes`, which the chart renders as
`false`; and nothing happens without the field. An installation that wants
neither has nothing to do.

Turning it on and adding the field to a group *does* roll that group, because
the rendered pod really is different — one group, when you edit it, not the
fleet on upgrade.

The two refusals both land on the group as `Accepted=False` with an event on
the transition: `PluginVolumesDisabled` names the operator flag, and
`PluginVolumeUnusable` names the claim and its access modes. Neither creates
servers — a group whose volume cannot be mounted would otherwise fill with pods
sitting `Pending` on a claim that will not attach, and look like a scheduling
problem rather than a spec one.

## 0.2.15: Purpur is the backend image, and Paper is deprecated

`ghcr.io/spawnery/purpur` is published from this release. It is
[Purpur](https://purpurmc.org), a fork of Paper, and it is what the network
this operator was built for actually runs.

**Nothing moves on its own.** A `ServerGroup` names its own `spec.image`, so no
installation changes backend until somebody edits one. `ghcr.io/spawnery/paper`
is still built, still tested and still published at every release.

### What the two images share

Everything except the server jar: the same `image/entrypoint.sh`, the same
agent plugin, the same `spawnery-slp` and `spawnery-config`, and the same
jlink'd Java runtime. That last one is a measurement rather than an assumption —
`nix/paper-jre.nix`'s module list was re-derived with `jdeps` over Purpur's own
classpath (109 jars against Paper's 105) and came out identical.

Both are tagged the same way, so the version you are on is the version you
move to:

```yaml
-  image: ghcr.io/spawnery/paper:26.2-0.2.15
+  image: ghcr.io/spawnery/purpur:26.2-0.2.15
```

That edit changes the group's pod hash, so it rolls the group exactly the way
any image bump does — through `maxUnavailable` and the cold start, with drains
moving players rather than dropping them.

### What you get, and what it costs

Purpur adds its own configuration file, `purpur.yml`, on top of Paper's. Nothing
renders it: it is a [mount](mounts.md) like any other file, and `subPath` is how
a single file lands beside the ones the server writes itself.

The cost is an upstream more: Purpur tracks Paper, so a Purpur build lags a
Paper build by however long that takes. `hack/purpur-pin.sh` is the sibling of
`hack/paper-pin.sh` and the same `make purpur-pin-check` says whether the pin is
behind.

### Why Paper is not simply removed

Every `ServerGroup` in every installation carries a `spec.image`, so deleting
the derivation would strand each of them on a tag that stops being rebuilt.
It goes when there is a release note saying it is going, and not before.

## 0.2.18: a server can describe itself, and nothing acts on what it says

A backend publishes a short state and up to sixteen key/value attributes
through the plugin API, and every agent in the namespace reads them back out of
the network picture it already receives:

```java
api.announce("running", Map.of("map", "arena"));

api.servers().stream().filter(s -> s.state().equals("waiting"))
```

**The operator carries it and reads none of it.** No decision it makes looks at
a word: not scheduling, not routing, not scaling. That is the whole reason a
free-form description is safe to carry here, and it is also the limit — a
server that announces `ending` goes on taking joins until something
[retires](../agent/api/README.md#changing-the-fleet) it.

It is deliberately not the phase. `ServerInfo.phase()` is the operator's
account of a server's lifecycle and no plugin can write it; this is the
server's account of itself, and the two are meant to disagree. A server is
`READY` from the moment it can take players until it stops, and what is
happening inside that window is a question only the thing running there can
answer. `/cloud info` prints both, and prints the server's after the word
`says`.

**Nothing changes for an installation that ignores it.** No CRD field, no
status field, no object at all: an announcement lives in the operator's memory
for as long as that pod has a session. A server that has announced nothing and
a server whose agent predates the verb are the same server in the picture —
both carry an empty description rather than a missing one.

### A server can close its own door, and it is not a retire

`acceptJoins(false)` stops the proxies routing new players to one server;
`acceptJoins(true)` undoes it. Nobody already there is moved, and the phase
does not change — a closed server is `Ready` and not registered, which is a
state this operator always had.

It exists as its own verb rather than as a use of `retire` because the two mean
different things and one of them is permanent. Retiring says a server is
finished and ends it once it is empty; a round that has started is not a server
on its way out, and an operator looking at a `Retiring` server afterwards would
read a decommissioning that nobody meant.

The group notices: a closed server's empty seats stop counting toward
`status.freeSlots`, so a group sized by `spec.scaling.spareSlots` builds a
replacement instead of sitting at its floor while every server in it has shut
its door. **That rule tightened one case that predates any door**: between the
pass that makes a server `Ready` and the one that registers it, its seats used
to count as reachable.

Nothing changes for an installation whose plugins never call it. A server that
has never asked is open, and so is one whose agent this operator has never
heard from.

### The counterpart: what a person writes down about a group

`ServerGroup` and `ProxyGroup` gain `spec.attributes`, which is the same idea
from the other side. A server says what it is doing right now and changes its
mind every round; this is written by a person in the group's own definition and
changes when somebody edits it:

```yaml
spec:
  attributes:
    permission: task.build
    game: bingo
```

A plugin reads it as `Group.attributes()`. The operator reads none of it
either, and unlike `spec.env` it shapes no pod — editing it replaces nothing
and restarts nothing, because the next network picture simply carries the new
value.

Both sides stop at the same bounds, which the API server enforces for the group
half and the operator for the server half: sixteen entries, names of at most 64
characters, values of at most 256. Refused rather than trimmed. They are small
because this is copied into every agent's picture on every resync, so what it
costs is paid by every pod for as long as the network runs.
