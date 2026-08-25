# Upgrading an installation across a rename

Three objects changed name during development, and the operator renames
nothing: it writes the new name and leaves whatever the old code wrote sitting
there. None of this applies to an installation created at v0.1.0 or later —
the only one that exists was installed 2026-08-20, milestones after all three
renames, and was checked clean on 2026-08-22. This page is for whoever finds
an older one.

Nothing here is an open defect. `docs/known-issues.md` carries those.

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
