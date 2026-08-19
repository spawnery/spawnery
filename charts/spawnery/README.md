# spawnery

The Helm chart for the Spawnery operator. Since milestone 6d this is the only
way the operator installs — `config/deploy/`, the flat manifests this chart
replaces, no longer exists in this repository.

## Installing

```bash
helm install spawnery charts/spawnery --namespace spawnery-system --create-namespace
```

`--create-namespace` is not optional in practice. The chart templates no
`Namespace` object of its own (see "No Namespace object" below), so without
the flag `helm install` refuses immediately with `namespaces "spawnery-system"
not found` — the ordering hazard a plain `kubectl apply -f config/deploy/`
used to walk into on its own first run, now turned into a refusal at install
time instead of a Deployment landing before the namespace that would hold it.

### The one manual step this chart cannot make

`config/rbac/forwarding-secret-reader.yaml` grants the operator's
ServiceAccount `secrets: get` in a *game* namespace — the namespace holding a
`Network`, not the namespace the operator itself runs in. It is applied once
per game namespace, by hand, after the chart is installed and after the
namespace exists:

```bash
kubectl apply -n <game-namespace> -f config/rbac/forwarding-secret-reader.yaml
```

This file is deliberately kept outside the chart: a chart installed once
cannot know the game namespaces an operator will create later, and rendering
a namespaced grant for a namespace that does not exist yet is not something
`helm install` can do. Its own header comment explains this in full.

**Its RoleBinding hard-codes the operator's ServiceAccount to this chart's own
documented default namespace, `spawnery-system`, at line 65:**

```yaml
subjects:
- kind: ServiceAccount
  name: spawnery-operator
  namespace: spawnery-system
```

**If this chart is installed into any namespace other than `spawnery-system`,
that line has to be changed to the real namespace before the file is applied
into any game namespace.** Get this wrong and the failure will not say
"namespace": the operator's own read of the forwarding secret comes back
`Forbidden`, and `Network.status` reports
`ForwardingSecretResolved=Unknown/SecretReadForbidden` with a message that
names the *secret* it could not read and the `kubectl apply` line that fixes
it — never the RoleBinding subject at line 65 that is actually wrong. A
reader chasing that message looks at the secret, not at this file. The
practical effect is narrower than a first read suggests: `ServerGroup`s and
`ProxyGroup`s in that namespace keep scheduling normally — milestone 5c built
forwarding-secret rotation detection as reporting only, and it stays reporting
only here — but that namespace's `Network` can never detect a rotation of its
forwarding secret, silently, for as long as the RoleBinding names the wrong
namespace.

## The values

Every key in `values.yaml`:

| Key | Default | What it does |
|---|---|---|
| `image.repository` | `ghcr.io/spawnery/spawnery-operator` | The operator image. |
| `image.tag` | `"0.1.0"` | Used when `image.digest` is empty. |
| `image.digest` | `""` | Set, this wins over `image.tag` and the image is referenced by digest (`repository@sha256:...`). This is what `hack/publish.sh`'s `WRITE_DIGEST=1` path writes once a real `make publish` has pushed the operator image. |
| `image.pullPolicy` | `IfNotPresent` | Passed straight to the container. `hack/e2e.sh` overrides this to `Never` for its own run, so a missing local image fails loudly instead of being fetched. |
| `resources.requests.cpu` | `100m` | Passed straight to the operator container. |
| `resources.requests.memory` | `128Mi` | Passed straight to the operator container. |
| `resources.limits.memory` | `256Mi` | Passed straight to the operator container. There is deliberately no CPU limit. |
| `nodeSelector` | `{}` | Passed through to the operator pod unchanged. |
| `tolerations` | `[]` | Passed through to the operator pod unchanged. On a cluster whose control plane carries a taint, this is often the difference between `Running` and `Pending`. |
| `affinity` | `{}` | Passed through to the operator pod unchanged. |
| `networkPolicy.enabled` | `true` | Gates the `NetworkPolicy` in front of the agent endpoint (milestone 6b). Disabling it is the right answer on a cluster that manages its own policies centrally, and makes no difference at all on a CNI that implements no `NetworkPolicy` controller, which is what 6b measured of the CNI in this repository's own end-to-end harness — see `docs/known-issues.md`, "From milestone 6b". |
| `operator.startupDeadline` | `5m` | The production value. `hack/e2e.sh` overrides it to `20s` for its own run. |
| `operator.leaderElect` | `true` | Passed straight to the operator's `--leader-elect` flag. |

`replicas` and `imagePullSecrets` are deliberately not values. `readyz` hangs
off the leader lock, so a second replica never becomes ready — the Deployment
template hard-codes `1` and `strategy: Recreate`, with a comment explaining
why, and a knob whose only valid setting is 1 is a trap rather than a
feature. `imagePullSecrets` is absent because all three of this project's
images are public and unauthenticated by decision; if that changes, it is a
field and a test to add, not a restructuring.

## Uninstalling

```bash
helm uninstall spawnery --namespace <namespace>
```

removes the Deployment, Service, NetworkPolicy and RBAC objects this chart
manages, but **leaves the four CRDs standing**:

```
networks.spawnery.cloud
proxygroups.spawnery.cloud
servergroups.spawnery.cloud
servers.spawnery.cloud
```

Each carries `helm.sh/resource-policy: keep` in
`charts/spawnery/templates/crds.yaml`, and `helm uninstall` prints exactly
that it kept them. Because the CRDs survive, **every `Network`, `ServerGroup`,
`ProxyGroup` and `Server` object in the cluster survives too** — an uninstall
removes the operator that reconciles them, not the objects it was managing.

A full removal needs a second, manual step:

```bash
kubectl delete crd networks.spawnery.cloud proxygroups.spawnery.cloud \
  servergroups.spawnery.cloud servers.spawnery.cloud
```

Deleting a CRD deletes every object of that kind immediately, with no
finalizer and no drain — the operator that would run either is already gone.
Kubernetes' own owner-reference garbage collection then cascades that into
every Pod, Service and ConfigMap the operator created for those objects.
**Persistent worlds are the deliberate exception.** Milestone 5a gives every
`PersistentVolumeClaim` no owner reference at all, precisely so a world
outlives its `Server`, its group, and an operator who deletes the wrong
object — so this command does not touch a game's stored blocks. Reclaiming an
orphaned claim afterward is the same manual `kubectl` act 5a always required
(`docs/known-issues.md`, "From milestone 5a").

**No `helm upgrade` has ever been run against this chart, anywhere in this
project.** The CRDs live in `templates/` rather than Helm's own `crds/`
directory, and carry `helm.sh/resource-policy: keep`, specifically so that a
future `upgrade` would carry a CRD schema change through — this API has
changed in six consecutive milestones — and so that `uninstall` would not take
every `Network` in the cluster with it. Only the second half of that design
has been exercised: `helm uninstall` leaving the four CRDs standing was
driven and observed against a real cluster (see
`.superpowers/sdd/2026-08-19-helm-chart/task-5-report.md`, "Step 7"). Nothing
here has ever run `helm upgrade`, so the schema-carrying half of the design is
built and unproven.
