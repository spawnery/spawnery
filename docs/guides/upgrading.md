# Upgrading between releases

An upgrade costs whatever it rolls, and two things decide that: whether the
new operator renders proxy pods differently than the one it replaces, and
whether the agent images in the fleet are older than the operator that will
drain them. Before either, find out whether anything is rolling already.

## Is a group mid-roll?

```bash
kubectl get pods -n <ns> -l spawnery.cloud/role=proxy -L spawnery.cloud/pod-hash
```

Two distinct values inside one group means that group is mid-roll; one value
everywhere means done or never started.

**The group's status will not tell you.** The replacement pods come up before
any old pod is withdrawn, so `readyReplicas` holds at `replicas` and the phase
reads `Ready` throughout, as at rest.

## What an upgrade rolls, with no spec edited

A proxy pod is stale when its `spawnery.cloud/pod-hash` label differs from a
digest of the pod the operator *would* render for its group right now, and
that digest is taken over the rendered pod rather than over a chosen list of
spec fields. So a change to the *rendering code* (a new default in
`internal/podspec`, an added environment variable, a renamed label) moves
the digest for every `ProxyGroup` while every spec stays byte for byte what it
was.

A second trigger is invisible in the spec: the `agentEndpoint` handed to the
renderer feeds the digest, and it is
`spawnery-operator.<operator-namespace>.svc:9443`. Moving the operator to a
different namespace, or restarting it with a different `--operator-namespace`
or `POD_NAMESPACE`, rolls the whole fleet with nothing else changed.

Every group starts as soon as the new operator is up: a server group one pod
at a time, a proxy group blue/green across every stale pod at once, and every
group at once unless the Network caps concurrent changeovers or the groups set
changeover stages. Each replaced pod takes no new connections and is stopped
once its players have left; nobody is disconnected unless the group sets
`spec.update.maxStaleSeconds`, the pod's node is leaving, or its player count
cannot be read for `spec.drain.timeoutSeconds`. See [Rolling a
group](updates-and-drain.md#proxies-wait-for-their-players-too).

From a release without changeover stages, the first reconcile takes a proxy
group mid-roll from one extra pod to one per stale pod, and a `RollingUpdate`
group whose stale servers are all leaving may release its budget place.

Rolling on the rendered pod rather than on any `metadata.generation` change is
deliberate: `replicas` is the routine edit on a proxy group, and a generation
rule would make every scale-up and scale-down a full replacement, each pod
waiting out an attrition-bound drain.

## Finding out before you upgrade

`internal/podspec/hash_golden_test.go` pins `DesiredProxyHash` and
`DesiredServerHash` over frozen fixtures, so a change to either render path
fails on the pull request that makes it. Comparing two builds after the fact,
the cheap negative filter is `git diff <old>..<new> -- internal/podspec/`; if
nothing in the pod-render path moved, the digest cannot have. That is how
2026-08-22's v0.1.2 to v0.2.0 upgrade was known to be safe in advance, and it
was: both proxies kept `pod-hash 2dd6593373a4ffd2` and 46 hours of uptime,
because the only file that had moved was `netpol.go` and only its comments.

Neither the golden tests nor the diff covers the triggers outside the code:
the group's own namespace and name, the `Network`'s name, and the agent
endpoint above. For those, run the new build against a scratch cluster over
*the same* manifests and compare the `pod-hash` it stamps with what the
running pods carry. Different manifests tell you nothing about your fleet.

## Upgrade the proxy images before the operator

A new operator against proxy images older than `SetReady` empties nobody and
disconnects everybody at the deadline. What it looks like first is that
nothing happens: `spec.replicas` goes 2 to 1 and the surplus pod stays
`Ready`, stays in the Service's endpoint slice, and goes on receiving *new*
players for the whole drain window. Then the pod is deleted with all of them
on it, and one event is the only record:

```
Warning  ProxyDrainTimeout  proxygroup/gateway  deleting proxy gateway-xxxx after 5m0s with 3 player(s) still connected
```

An older agent does what protobuf requires of an unknown field and ignores
`SetReady`, so `ReadyGate.close()` is never reached, the kubelet's probe keeps
succeeding, and the endpoint never goes away. The deadline bounds the damage;
nothing prevents it. Nothing version-gates the message, which is why the order
matters.

**The signature is annotation plus `Ready`.** The operator writes
`spawnery.cloud/draining-since` whether or not the agent ever hears the
message, so an un-upgraded proxy carries the annotation while its `Ready`
condition is still `True` and
`kubectl get endpointslice -l kubernetes.io/service-name=<group>` still shows
its address as ready. A correctly drained proxy carries the annotation and is
`NotReady`.

Rolling the *operator* back on its own is safe. An agent that supports
`SetReady` and never receives one opens its gate on the first full sync
unless a `false` was asserted.

## A new Minecraft version

A game image's tag is `<minecraft>-<release>`. When a release moves to a new
Minecraft version, the previous version's Paper and Purpur images are
published beside it for a short transition, so the operator and the proxies
can move first and the worlds later: `purpur:26.2-<release>` next to
`purpur:26.3-<release>`. The one Velocity image serves both. Worlds only go
forward, so move a persistent group to the new version once, and not back.

## `/cloud start` and `/cloud stop` are gone

Since 0.19.0, `/cloud scale <group> <count> [for <duration>]` replaces
`start`. Where `start` raised a group's floor, `scale` holds the group at
exactly that many servers, so it can also take a group below `minReplicas`.
`/cloud scale <group> reset` replaces `stop` and removes pins and boosts
alike. The node is still `spawnery.cloud.scale`. A plugin calling `boost` or
`stopBoosts` keeps working; both are deprecated in favour of `scale` and
`resetScale`.

## Events about private servers stay on the proxies

Since 0.20.0 an event about a member of an on-demand group, or about the
group itself, reaches proxies only. A backend plugin subscribed to `EventBus`
no longer hears of private servers, which matches `servers()`, where they
never appeared.

## Older installations

[Release notes](../archive/release-notes.md) carries the notes release by
release: what each one rolled, and what it left stranded. The operator renames
nothing. It writes the new name and leaves whatever the old code wrote
sitting there, so an installation created before v0.1.0 still carries a
PodDisruptionBudget and a ConfigMap under their old names, and a `Ready: False`
condition nothing republishes.
