# How players reach the proxies

A `ProxyGroup` carries one of four expose strategies, and it is the first real
decision after the tutorial: the tutorial uses `NodePort` because that is what
works on a local `kind` cluster, and a network on a real cluster usually wants
something else.

Here is the common case — a proxy group behind a `LoadBalancer`, which is what
most managed clusters want. It joins the Network of
`config/samples/network.yaml`; apply that first if the namespace has none. Save
this as `gateway.yaml`:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft
spec:
  networkRef:
    name: production
  replicas: 2
  image: ghcr.io/spawnery/velocity:3.5.1-0.3.0
  expose:
    type: LoadBalancer
    loadBalancer: {}
  routing:
    fallbackGroups:
      - lobby
```

```bash
kubectl apply -f gateway.yaml
kubectl get proxygroup gateway -n minecraft
```

The `ADDRESS` column is what a player types. It stays empty until the strategy
has produced one, which is the quickest way to tell a group that is still
starting from one whose strategy cannot work here at all.

## Which of the four

| `type` | What the operator creates | What `ADDRESS` becomes |
|---|---|---|
| `LoadBalancer` | a `Service` of type `LoadBalancer` | the first ingress IP or hostname the controller assigns, with port 25565 |
| `NodePort` | a `Service` of type `NodePort` | the host IP of a ready proxy pod, with the allocated node port |
| `HostPort` | **nothing** — no Service at all | the host IP of a ready pod that binds the port, with your `port` |
| `ClusterIP` | a `Service` of type `ClusterIP` | `spec.expose.clusterIP.address`, echoed exactly as you wrote it |

**`LoadBalancer`** is the default answer where something assigns addresses. On
bare metal that something has to be installed: RKE2 ships no active
LoadBalancer controller, so without MetalLB or kube-vip no address is ever
assigned and `ADDRESS` stays empty forever rather than merely late — a failure
that looks exactly like slowness until you know to expect it.

`externalTrafficPolicy` defaults to `Local`, which is deliberate: it keeps the
player's real IP instead of replacing it with the load balancer's, and bans and
rate limits depend on that. `loadBalancer.annotations` is copied onto the
Service, which is where a MetalLB pool selector goes.

**`NodePort`** needs nothing installed, which is why the tutorial uses it. The
port must lie inside the API server's `service-node-port-range` — the usual
default is 30000–32767, so 25565 is not available to it.

**`HostPort`** binds a fixed port directly on every node running a proxy pod,
so a player can be sent to 25565 itself. Two costs come with it, and both bite
before anything works:

- **Replicas are capped by nodes.** The kube-scheduler will not place two pods
  binding the same host port on one node, so a group of three proxies needs
  three nodes and otherwise sits with pods it cannot schedule.
- **Pod Security forbids it.** Both `baseline` and `restricted` disallow a
  container host port outright, so in a namespace enforcing either, the API
  server refuses every pod of the group. The operator reports that on the
  group's own `Degraded` condition rather than admitting one. Against
  `restricted` the refusal reads:

  ```text
  violates PodSecurity "restricted:latest": hostPort
  (container "velocity" uses hostPort 25577)
  ```

  The remedy is a namespace of its own for the HostPort group, labelled with a
  relaxed policy and separate from the restricted namespaces the rest of the
  network runs in.

And a host port that is admitted, ready and published in `ADDRESS` can still be
unreachable, because whether the port is open to the world is a host-firewall
question rather than a Kubernetes one. [Network
boundaries](../explanation/network-boundaries.md) has that story.

**`ClusterIP`** is for a network something else publishes — an ingress
controller with a TCP entry point, a gateway, a tunnel. The operator creates
the Service that thing routes to, and nothing else. Because it cannot learn the
name players type, you give it:

```yaml
  expose:
    type: ClusterIP
    clusterIP:
      address: mc.example.com
```

`address` is required rather than optional, so that "empty" and "forgotten"
cannot be the same state — closing that gap is the whole reason this strategy
exists. Give a bare hostname and no port unless the entry point really is on
another one: Minecraft clients default to 25565, so `mc.example.com` is the
whole of what a player types.

Nothing checks it. Not that it resolves, not that anything listens, not that it
leads to this group's Service. It is a sign on a door, not a test of the door —
and it is echoed into `ADDRESS` verbatim, so a typo there is a typo players
will meet.

## When the address stays empty

Every strategy returns no address until a proxy pod is actually ready, so an
empty `ADDRESS` on a young group means nothing is wrong yet. It becomes a
diagnosis when it persists:

```bash
kubectl get proxygroup gateway -n minecraft \
  -o jsonpath='{.status.conditions}'
```

- `LoadBalancer` with no address and ready pods: no controller is assigning
  one. This is the bare-metal case above.
- `HostPort` with no address and pods that never schedule: either the node
  count is below `replicas`, or Pod Security is refusing them — the `Degraded`
  condition says which.
- `ClusterIP` publishes its address as soon as a pod is ready, whether or not
  anything outside routes to it. An address here is not evidence that players
  can connect.

Every field of `spec.expose` is in the generated [custom resource
reference](../reference/crds.md#proxygroup), and
[`config/samples/network.yaml`](https://github.com/spawnery/spawnery/blob/master/config/samples/network.yaml)
carries all four written out as commented alternatives to paste over one
another.
