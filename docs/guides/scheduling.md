# Where game pods land, and who decides

A group can ask for node selectors, tolerations and affinity — but only as far
as its `Network` lets it. This is the part of the operator that is not ordinary
Kubernetes: the namespace is the boundary a group author is held to, and
scheduling reaches past it, onto a control-plane node, a cordoned one, or
beside somebody else's workload. So the `Network`'s owner has to widen that
boundary first.

That makes the permission two-sided, and both halves have to exist before
anything works. Here they are together — a network that allows one taint key
and one node label, and a group that uses both to keep off the database nodes.
Save this as `scheduling.yaml`:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: production
  namespace: minecraft
spec:
  # The Secret the sample and the tutorial both create; this half widens an
  # existing network rather than replacing it.
  forwardingSecretRef:
    name: velocity-forwarding-secret
  scheduling:
    allowedTolerationKeys:
      - workload
    allowedNodeSelectorKeys:
      - node-role.example.com/games
---
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: Ephemeral
  image: ghcr.io/spawnery/purpur:26.2-0.8.0
  maxPlayers: 50
  scaling:
    minReplicas: 1
    maxReplicas: 5
    spareSlots: 20
  scheduling:
    nodeSelector:
      node-role.example.com/games: "true"
    tolerations:
      - key: workload
        operator: Equal
        value: games
        effect: NoSchedule
```

```bash
kubectl apply -f scheduling.yaml
kubectl get servergroup lobby -n minecraft
```

Apply only the group half and it is refused, which is the single most common
way to meet this feature.

## Reading the refusal

A group whose scheduling the network does not allow gets `Accepted: False` with
reason `SchedulingNotAllowed`, a `Warning` event carrying the same text, and no
servers at all:

```bash
kubectl get servergroup lobby -n minecraft \
  -o jsonpath='{.status.conditions[?(@.type=="Accepted")]}'
```

The message names the key **and** the `Network` field that would allow it,
because the person reading it is usually a group author who can write neither
the taint nor the Network — "not allowed" on its own sends them to look through
the wrong object. The operator assembles the message at the moment it
refuses, so the wording below is exactly what it writes, with this page's own
network and key names substituted in:

```text
network "production" allows no scheduling; its spec.scheduling names what a group may ask of the scheduler, and it is unset
toleration key "workload" is not in network "production"'s spec.scheduling.allowedTolerationKeys
nodeSelector key "node-role.example.com/games" is not in network "production"'s spec.scheduling.allowedNodeSelectorKeys
```

Node affinity is checked against the same list as `nodeSelector`, and says so
with `node affinity key` in place of `nodeSelector key`.

## What the network can allow

Each list is a plain allowlist. An absent or empty list allows **nothing** —
there is no "unset means anything" here, and a `Network` with no
`spec.scheduling` at all refuses every group that asks for scheduling.

| Field | Governs |
|---|---|
| `allowedTolerationKeys` | which taint keys a group may tolerate |
| `allowedNodeSelectorKeys` | which node label keys it may select on, in `nodeSelector` and in node-affinity terms alike |
| `allowedAffinityNamespaces` | which namespaces besides its own a pod-affinity term may name |
| `hostPortRange` | which ports a `HostPort` proxy group may bind |

Two things no network can allow, at any setting:

- **A toleration with no key.** It tolerates every taint, including the ones
  that exist to keep workloads off a node. The refusal says so in those words:
  `a toleration with no key tolerates every taint, which no network allows`.
- **A pod-affinity term with a `namespaceSelector`.** It can name any
  namespace, so allowlisting namespaces would mean nothing beside it.

Both are refused outright rather than made configurable, because an allowlist
with a wildcard in it is not an allowlist.

## `HostPort` is part of this, which is easy to miss

A proxy group exposed by `HostPort` binds a port on the node, so it is a
scheduling question too and it goes through the same gate. Without
`spec.scheduling.hostPortRange` on the Network, **no HostPort group is
accepted**. The same two shapes, with the names filled in:

```text
network "production" allows no host port; its spec.scheduling.hostPortRange is unset
host port 25565 is outside network "production"'s spec.scheduling.hostPortRange 30000-32767
```

```yaml
spec:
  scheduling:
    hostPortRange:
      min: 25565
      max: 25599
```

The other costs of `HostPort` — the node cap on replicas, and Pod Security
refusing it outright in a `baseline` or `restricted` namespace — are in [How
players reach the proxies](expose-strategies.md).

## Inheriting it instead of repeating it

Scheduling can also come from `Network.spec.defaults.scheduling`, which every
group inherits and any group may override. The allowlist is applied to the
*effective* scheduling either way, so a default the Network sets for its own
groups still has to be something the Network's policy permits — the two halves
do not shortcut each other.

## A note for upgrades

This gate arrived in 0.2.33. A group that set `spec.scheduling` before that
version and kept working can stop being accepted on upgrade, if its Network
never gained a matching policy. [Release notes](../archive/release-notes.md#0233-a-groups-scheduling-needs-the-networks-permission)
carries what that upgrade costs and what to do about it; this page is about
the model itself.

Every field named here is in the generated [custom resource
reference](../reference/crds.md#network).
