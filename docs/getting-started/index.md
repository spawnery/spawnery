# Installing the operator

The Helm chart is the only way the operator installs.

## Installing

```bash
helm install spawnery oci://ghcr.io/spawnery/charts/spawnery \
  --version 0.5.0 --namespace spawnery-system --create-namespace
```

The chart is an OCI artefact, so there is no `helm repo add`: an OCI
reference is the whole address.

`--version` is the chart's own number and not the release tag; the two move
apart. Each GitHub Release body prints the install line with the right one
already in it.

From a checkout, which is what `hack/e2e.sh` and every local install do:

```bash
helm install spawnery charts/spawnery --namespace spawnery-system --create-namespace
```

`--create-namespace` is not optional in practice. The chart templates no
`Namespace` object of its own, so without the flag `helm install` refuses
immediately with `namespaces "spawnery-system" not found`.

### Choosing a game namespace

A game namespace is one trust domain. Anyone who may create a pod in it has
access to that network, by two routes that do not depend on each other: they
may mount the `Network`'s forwarding secret, because any pod may mount any
Secret in its own namespace, and they may wear the labels the operator's
`NetworkPolicy` admits, because a pod's labels are chosen by whoever creates
it. Both were measured on 2026-08-21 against Cilium — a pod labelled as a
proxy reached a backend on 25565, and an unlabelled pod read the secret.

No policy the operator could write would close that, and [Network
boundaries](../explanation/network-boundaries.md#what-the-policy-defends-and-what-it-does-not)
carries why, along with what the policy does defend.

So: **do not share a game namespace with workloads you would not trust with
that network.** Give each `Network` a namespace of its own, and treat the right
to create pods in it as the right that it is.

### The one manual step this chart cannot make

`config/rbac/forwarding-secret-reader.yaml` grants the operator's
ServiceAccount `secrets: get` in a *game* namespace — the namespace holding a
`Network`, not the namespace the operator runs in. Apply it once per game
namespace, once the chart is installed and the namespace exists:

```bash
kubectl apply -n <game-namespace> -f config/rbac/forwarding-secret-reader.yaml
```

It sits outside the chart because a chart installed once cannot know the game
namespaces an operator will create later.

**For the namespaces you do already know, `networkNamespaces` does it for
you** — and does two things this file cannot: it restricts the grant by
`resourceNames` to that network's own secrets, and it puts the RoleBinding's
subject in the release's own namespace. It renders nothing by default, so a
namespace nobody listed still needs the manual step. `helm install` prints
that reminder from `NOTES.txt`; nothing checks the grant was ever applied.

**The RoleBinding in `config/rbac/forwarding-secret-reader.yaml` hard-codes
this chart's documented default namespace, `spawnery-system`, as its subject:**

```yaml
subjects:
- kind: ServiceAccount
  name: spawnery-operator
  namespace: spawnery-system
```

**Installed into any namespace other than `spawnery-system`, that `namespace`
has to be changed to the real one before the file is applied.** Get it wrong and the
failure will not say "namespace": the `Network` reports that it could not read
its forwarding secret and names the *secret* and the `kubectl apply` line
above, never the RoleBinding subject that is wrong. The operator logs the API
server's refusal once, naming the ServiceAccount it really runs as, and that
line is where the mismatch is visible rather than deducible. Until it is
fixed, that namespace's `Network` can never detect a rotation of its
forwarding secret, and its groups keep scheduling normally with nothing on
their status to show for it.

## The values

Every key is in [Chart values](../reference/chart-values.md), generated from
`charts/spawnery/values.schema.json` by `make manifests`, so it cannot drift.

One thing that page does not carry: `replicas` and `imagePullSecrets` are
deliberately not values. `readyz` hangs off the leader lock, so a second
replica never becomes ready — the Deployment hard-codes `1` and `strategy:
Recreate`, and a knob whose only valid setting is 1 is a trap. There is no
`imagePullSecrets` because all three images are public.

## The `/cloud` permissions

The agents register a `/cloud` command on every Paper server and Velocity
proxy, and **nobody holds any of its permissions by default** — so right after
installing it answers "unknown command" to every player. That is the safe
state rather than a broken install; [The `/cloud`
command](../guides/cloud-command.md) covers the four nodes and what each
opens.

## Uninstalling

```bash
helm uninstall spawnery --namespace <namespace>
```

removes the Deployment, Service, NetworkPolicy and RBAC objects this chart
manages, but **leaves the four CRDs standing** — each carries
`helm.sh/resource-policy: keep` — and therefore **every `Network`,
`ServerGroup`, `ProxyGroup` and `Server` object with them**. An uninstall
removes the operator that reconciles them, not the objects it managed.

A full removal needs a second, manual step:

```bash
kubectl delete crd networks.spawnery.cloud proxygroups.spawnery.cloud \
  servergroups.spawnery.cloud servers.spawnery.cloud
```

Deleting a CRD deletes every object of that kind immediately, with no
finalizer and no drain, and owner-reference garbage collection cascades into
every Pod, Service and ConfigMap it created. **Persistent worlds are the deliberate exception**:
every `PersistentVolumeClaim` carries no owner reference at all, precisely so
a world outlives its `Server`, its group, and an operator who deletes the
wrong object. Reclaiming an orphaned claim afterwards is a manual `kubectl`
act — see [Known issues](../reference/known-issues.md).
