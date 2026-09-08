# Scheduling within the Network

## What this corrects

`docs/network-boundaries.md` says the boundary a group author cannot cross is
the namespace. Three fields cross it today, and nothing in the operator looks
at them: `spec.scheduling.tolerations`, `spec.scheduling.nodeSelector` and
`spec.scheduling.affinity` are copied into the pod verbatim (`podspec/server.go`,
`podspec/proxy.go`), and `expose.hostPort.port` is bounded only by
`1..65535`. Whoever may write a ServerGroup may therefore put a pod running an
image of their choosing on a control-plane node (`tolerations: [{operator:
Exists}]` plus a `nodeSelector` on `node-role.kubernetes.io/control-plane`), on
a node cordoned for maintenance, or beside a named workload in another
namespace through `podAffinity` with `namespaceSelector`; and a ProxyGroup with
`replicas` at the node count can claim one arbitrary host port on every node.

Found by the deep review of 2026-09-07 (P1). No installation is affected: on
`paulwtf` no group sets either field.

## The rule

**The Network decides what its groups may ask of the scheduler.** A new
optional field on the Network, `spec.scheduling`, names what is allowed:

```yaml
spec:
  scheduling:
    allowedTolerationKeys: ["spawnery.cloud/game"]
    allowedNodeSelectorKeys: ["topology.kubernetes.io/zone", "spawnery.cloud/pool"]
    allowedAffinityNamespaces: ["minecraft-shared"]
    hostPortRange: {min: 30000, max: 30100}
```

- A toleration's key must be in `allowedTolerationKeys`. A toleration with
  no key matches every taint and is never allowed.
- A `nodeSelector` key, and every key in a node-affinity term's
  `matchExpressions` and `matchFields`, must be in `allowedNodeSelectorKeys`.
- A pod-affinity or pod-anti-affinity term may name the group's own
  namespace, or one in `allowedAffinityNamespaces`. A `namespaceSelector` can
  name any namespace and is never allowed.
- A HostPort proxy's port must lie in `hostPortRange`; without the range no
  HostPort group is accepted.

**Absent means nothing is allowed.** A Network without `spec.scheduling`
accepts no group that sets any of the three scheduling fields, and no HostPort
group. That is the boundary the documentation already claims, made true, and
it is the safe direction: an installation that never used these fields
notices nothing, and one that did is told exactly which key to allow.

The check runs on the **effective** scheduling, the group's own or the
Network's `defaults.scheduling` where the group sets none, resolved exactly as
`podspec` resolves it for the pod. A Network default outside the Network's own
policy therefore refuses every group that inherits it, with a message naming
the default, which is honest rather than surprising: the two fields sit on the
same object and the same author wrote both.

## Where it runs

A pure function in `internal/podspec`, beside the builders that copy the
fields: `SchedulingRefusal(policy, effective, namespace)` and
`HostPortRefusal(policy, port)`, each returning a message and a verdict.
`podspec` already owns the resolution (`EffectiveScheduling` is extracted from
the two builders so the check and the pod cannot disagree).

Both group reconcilers call it in their acceptance chain, after the volume
checks and before `Accepted=True`, with two new reasons on the `Accepted`
condition: `SchedulingNotAllowed` and `HostPortNotAllowed`. The message names
the offending key, namespace or port and the Network field that would allow
it, the way the claim switches name their flag. A refused group creates no
server or proxy, exactly as a group whose claim cannot be served.

## What it does not do

- It does not filter or rewrite: a group outside the policy is refused whole,
  not rendered with the offending entries dropped. A silently narrowed pod is
  a pod nobody asked for.
- It does not validate the Network's own defaults at admission. The refusal
  surfaces on the groups, where the effect is.
- It does not touch `resources` or `imagePullSecrets`; neither reaches beyond
  the namespace.

## Testing

Table tests on the two pure functions cover every rule above and the absent
policy. One reconcile test per group kind: a group with a toleration is
refused with `SchedulingNotAllowed` naming the key, and accepted once the
Network allows that key. The CRD and chart are regenerated; the hash goldens
do not move, because no rendered pod changes.

## Upgrade

A Network that has groups using scheduling or HostPort refuses them after the
upgrade until `spec.scheduling` allows what they use. `docs/upgrading.md`
carries the note; `paulwtf` has nothing to write.
