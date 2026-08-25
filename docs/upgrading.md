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
