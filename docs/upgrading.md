# Upgrading an installation across a rename

Three objects changed name during development, and the operator renames
nothing: it writes the new name and leaves whatever the old code wrote sitting
there. None of this applies to an installation created at v0.1.0 or later —
the only one that exists was installed 2026-08-20, milestones after all three
renames, and was checked clean on 2026-08-22. This page is for whoever finds
an older one.

Nothing here is an open defect. `docs/known-issues.md` carries those.

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
