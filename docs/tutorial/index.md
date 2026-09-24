# From an empty machine to a player on a server

By the end of this page a `kind` cluster runs the Spawnery operator, a lobby
group of Paper servers, and one Velocity proxy in front of them — and your own
Minecraft client is standing on one of those servers. Every command below, and
the output shown beside most of them, comes from a real run of exactly this
path — not a description of what one would look like.

You will need `kind`, `kubectl`, `helm`, a Minecraft client, and **7-8Gi of
memory free**. Part of that is exact, from the manifest in step 3: a Paper
server pod requests 2Gi, the proxy requests 1Gi, and — as step 4 explains —
you end up with two Paper servers rather than one, so the pods alone request
5Gi. The rest is an allowance, not a measurement: roughly 1Gi for `kind`'s own
control plane, and 1-2Gi for the Minecraft client itself, which runs alongside
the cluster rather than inside it and is easy to forget when counting. Budget
for the client — leaving it out makes 4Gi look like enough right up to the
step where it is not.

## 1. Create the cluster

`docs/tutorial/kind-config.yaml`, in full:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      # Routes the host to the proxy's NodePort (network.yaml's
      # spec.expose.nodePort.port) so a client outside the cluster can reach
      # it. Without this mapping the NodePort only answers inside the kind
      # node's own network namespace.
      - containerPort: 30001
        hostPort: 30001
        protocol: TCP
```

The `extraPortMappings` block, and its own comment, is the one part worth
reading before you copy this: it is how a client running outside the cluster
reaches a proxy running inside it. Save it as `kind-config.yaml` and create
the cluster:

```bash
kind create cluster --name spawnery-tutorial --config kind-config.yaml
```

## 2. Install the operator

```bash
helm install spawnery oci://ghcr.io/spawnery/charts/spawnery \
  --version 0.6.0 \
  --namespace spawnery-system --create-namespace
```

```text
NAME: spawnery
LAST DEPLOYED: Tue Sep 15 14:23:40 2026
NAMESPACE: spawnery-system
STATUS: deployed
REVISION: 1
DESCRIPTION: Install complete
TEST SUITE: None
NOTES:
spawnery 0.6.0 installed as release spawnery in namespace spawnery-system.

  kubectl -n spawnery-system rollout status deployment/spawnery-operator

ONE STEP REMAINS, AND SKIPPING IT FAILS SILENTLY.

Every namespace that will hold a Network needs the forwarding-secret reader
grant. This chart cannot template it: the game namespaces do not exist yet
when the chart is installed, which is why the file stays outside the chart.
Apply it once per game namespace, after that namespace exists:

  kubectl apply -n <game-namespace> -f config/rbac/forwarding-secret-reader.yaml

Nothing refuses a grant that binds a ServiceAccount which does not exist: the
apply succeeds, the Network is still Accepted, and its ServerGroups and
ProxyGroups keep scheduling. What is lost is forwarding-secret rotation
detection in that namespace, reported only on the Network itself:

  kubectl -n <game-namespace> get network <name> \
    -o jsonpath='{.status.conditions[?(@.type=="ForwardingSecretResolved")]}'

Uninstalling leaves the four CRDs standing, and with them every Network,
ServerGroup, ProxyGroup and Server in the cluster. charts/spawnery/README.md
has both procedures in full.
```

That "one step" is about detecting a rotated secret later, which a namespace
this tutorial deletes at the end will never need — skip it here, and see
[Getting started](../getting-started/index.md#the-one-manual-step-this-chart-cannot-make)
for the real procedure on a network you keep. Wait for the rollout instead:

```bash
kubectl -n spawnery-system rollout status deployment/spawnery-operator
```

```text
Waiting for deployment "spawnery-operator" rollout to finish: 0 of 1 updated replicas are available...
deployment "spawnery-operator" successfully rolled out
```

## 3. Apply the network

`docs/tutorial/network.yaml`, in full:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: spawnery-tutorial
---
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: spawnery-tutorial
stringData:
  # This namespace is deleted at the end of the tutorial, so a fixed value is
  # fine here. For a real network, generate one instead:
  # head -c 32 /dev/urandom | base64
  secret: tutorial-forwarding-secret
---
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: tutorial
  namespace: spawnery-tutorial
spec:
  forwardingSecretRef:
    name: velocity-forwarding-secret
  defaults:
    minecraftVersion: "26.2"
    resources:
      requests:
        cpu: "1"
        memory: 2Gi
      limits:
        memory: 2Gi
---
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: spawnery-tutorial
spec:
  networkRef:
    name: tutorial
  type: Ephemeral
  image: ghcr.io/spawnery/purpur:26.2-0.5.0
  maxPlayers: 20
  scaling:
    minReplicas: 1
    maxReplicas: 3
    spareSlots: 5
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: spawnery-tutorial
spec:
  networkRef:
    name: tutorial
  replicas: 1
  image: ghcr.io/spawnery/velocity:3.5.1-0.5.0
  # Velocity does not need a backend's heap; overriding the Network's
  # defaults keeps the proxy off the 2Gi a Paper server needs.
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      memory: 1Gi
  expose:
    type: NodePort
    nodePort:
      # Fixed rather than left to allocate: kind-config.yaml's
      # extraPortMappings maps one host port to this exact number, and a
      # mapping cannot point at a port nobody can predict.
      port: 30001
  routing:
    fallbackGroups:
      - lobby
  config:
    # The custom resource field, not a configOverlay: internal/render
    # reasserts the keys it owns after merging an overlay, so this is the
    # only place online-mode can be moved from. It lets a reader's own
    # licensed client join without a real Mojang session.
    onlineMode: false
    playerLimit: 50
    motd: "Spawnery tutorial"
```

One `Network`, one ephemeral `ServerGroup` of Paper backends, one `ProxyGroup`
in front of it — the manifest creates its own namespace, so save it as
`network.yaml` and apply it directly:

```bash
kubectl apply -f network.yaml
```

## 4. Watch it come up

```bash
kubectl get servergroups -n spawnery-tutorial
```

```text
NAME    TYPE        PHASE   READY   REPLICAS   PLAYERS   FREE SLOTS   BOOSTED   AGE
lobby   Ephemeral   Ready   2       2          0         40           0         76s
```

Two servers, not the one `minReplicas: 1` asked for, and not something a join
caused — nobody has joined yet: a server this new hasn't shown its pod to the
operator's cache for one pass, so the group briefly builds a spare rather than
risk coming up short, and sheds it again a few minutes later, well past where
this tutorial ends. See [`ServerGroup`](../reference/crds.md#servergroup) for
the field (`scaleDownStabilizationSeconds`) that governs the timing.

```bash
kubectl get servers -n spawnery-tutorial
kubectl get pods -n spawnery-tutorial
```

```text
NAME         GROUP   PHASE   PLAYERS   SLOTS   REGISTERED   AGE
lobby-d2hf   lobby   Ready   0         20      true         76s
lobby-nd79   lobby   Ready   0         20      true         54s

NAME           READY   STATUS    RESTARTS   AGE
gateway-jgkq   1/1     Running   0          77s
lobby-d2hf     1/1     Running   0          77s
lobby-nd79     1/1     Running   0          55s
```

Three pods, matching the three pod requests the memory figure above was built
from: two servers at 2Gi and one proxy at 1Gi.

## 5. Join with your own client

```bash
kubectl get proxygroups -n spawnery-tutorial
kubectl get svc -n spawnery-tutorial
```

```text
NAME      PHASE   READY   ADDRESS            PLAYERS   AGE
gateway   Ready   1       10.89.0.41:30001   0         76s

NAME      TYPE       CLUSTER-IP     EXTERNAL-IP   PORT(S)           AGE
gateway   NodePort   10.96.56.131   <none>        25565:30001/TCP   77s
```

`ADDRESS` is the proxy's address inside the cluster's own network — not
reachable from your desktop. The `30001` in both lines is the same `NodePort`
`kind-config.yaml` mapped to a host port in step 1, so point your client at
**`localhost:30001`** instead.

Open your Minecraft client, add `localhost:30001` as a server, and connect.
The proxy runs with `spec.config.onlineMode: false` because this is a local
cluster on your own machine with no Mojang session to check — the same field
that lets this project drive this exact path in its own CI — so a licensed
client connects to it exactly as it would to any other server, nothing
withheld.

If you want confirmation before you open a client: this project's own
automated check joins the same address the same way, with a test-only tool
that logs in and prints what it saw instead of rendering a world —

```json
{"protocol":776,"username":"spawnery_probe","uuid":"bcc1dc19-a5eb-33a1-aa1b-4e3907d5e22f","compressed":true}
```

## 6. Watch the group notice you

```bash
kubectl get proxygroups -n spawnery-tutorial
```

```text
NAME      PHASE   READY   ADDRESS            PLAYERS   AGE
gateway   Ready   1       10.89.0.44:30001   1         104s
```

`PLAYERS` moves the moment your client is registered by the proxy. The lobby
`ServerGroup`'s own `onlinePlayers` counts you a step later: a backend counts
a player from the moment they are standing in the world, not from login. That
second counter is not shown here, because the tool from step 5 stops at Login
Acknowledged and so is counted by the proxy and never by the backend, while
your own client walks into the world and is counted by both.

## 7. Tear it down

```bash
kind delete cluster --name spawnery-tutorial
```

One command: the cluster is the whole footprint, so nothing else is left to
uninstall or clean up once it is gone.
