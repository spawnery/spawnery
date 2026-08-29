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

### Choosing a game namespace

A game namespace is one trust domain. Anyone who may create a pod in it has
access to that network, by two routes that do not depend on each other: they
may mount the `Network`'s forwarding secret, because any pod may mount any
Secret in its own namespace, and they may wear the labels the operator's
`NetworkPolicy` admits, because a pod's labels are chosen by whoever creates
it. Both were measured on 2026-08-21 against a real CNI — a pod labelled as a
proxy reached a backend on 25565, and an unlabelled pod read the secret.

The policy the operator writes is therefore not a boundary *inside* the
namespace, and no policy it could write would be: vanilla NetworkPolicy selects
peers by pod labels, namespace or CIDR, and inside one namespace none of those
tells a real proxy from an invented one. What it does defend against is the
co-tenant that cannot create pods — a compromised workload cannot relabel
itself, and that half of the measurement is a timeout.

So: **do not share a game namespace with workloads you would not trust with
that network.** Give each `Network` a namespace of its own, and treat the right
to create pods in it as the right that it is.

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

**For the namespaces you do already know, `networkNamespaces` does it for
you** — and does two things this file cannot. It restricts the grant by
`resourceNames` to that network's own secrets rather than to every secret in
the namespace, and it puts the RoleBinding's subject in the release's own
namespace rather than the hard-coded `spawnery-system` below. Nothing renders
by default. A namespace nobody listed is still a namespace where this manual
step is the answer.

Because forgetting this step fails silently rather than loudly (see below),
`helm install` prints it: `charts/spawnery/templates/NOTES.txt` names the
step, and when the release namespace is not `spawnery-system` it also prints
the exact line the file needs changed to. That is a reminder, not a check —
nothing verifies the grant was ever applied.

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
reader chasing that message looks at the secret, not at this file. Since
2026-08-24 the operator also logs the API server's own refusal once, when it
first sees it, and that line is the one worth reading: it names the
ServiceAccount the operator actually runs as, so a namespace that does not
match line 65 is visible there rather than deducible. The
practical effect is narrower than a first read suggests: `ServerGroup`s and
`ProxyGroup`s in that namespace keep scheduling normally — milestone 5c built
forwarding-secret rotation detection as reporting only, and it stays reporting
only here — but that namespace's `Network` can never detect a rotation of its
forwarding secret, for as long as the RoleBinding names the wrong namespace —
with nothing on any group's status to show for it.

## The values

Every key in `values.yaml`:

| Key | Default | What it does |
|---|---|---|
| `image.repository` | `ghcr.io/spawnery/spawnery-operator` | The operator image. |
| `image.tag` | `"0.2.8"` | Used when `image.digest` is empty. |
| `image.digest` | `""` | Set, this wins over `image.tag` and the image is referenced by digest (`repository@sha256:...`). This is what `hack/publish.sh`'s `WRITE_DIGEST=1` path writes once a real `make publish` has pushed the operator image. **The value checked in at any tag describes the release before it, and structurally cannot describe its own.** The digest comes from `skopeo copy --digestfile`, which exists only after the tag has been published, so the commit writing it back is necessarily behind the tag it names — measured on the RKE2 rollout, where a `HelmRelease` installing the chart at `v0.1.0` ran the *tag* rather than a digest. A deployment that wants an immutable reference pins it where the deployment is described, not here. |
| `image.pullPolicy` | `IfNotPresent` | Passed straight to the container. `hack/e2e.sh` overrides this to `Never` for its own run, so a missing local image fails loudly instead of being fetched. |
| `resources.requests.cpu` | `100m` | Passed straight to the operator container. |
| `resources.requests.memory` | `128Mi` | Passed straight to the operator container. |
| `resources.limits.memory` | `256Mi` | Passed straight to the operator container. There is deliberately no CPU limit. |
| `nodeSelector` | `{}` | Passed through to the operator pod unchanged. |
| `tolerations` | `[]` | Passed through to the operator pod unchanged. On a cluster whose control plane carries a taint, this is often the difference between `Running` and `Pending`. |
| `affinity` | `{}` | Passed through to the operator pod unchanged. |
| `networkPolicy.enabled` | `true` | Gates the `NetworkPolicy` in front of the agent endpoint (milestone 6b). Disabling it is the right answer on a cluster that manages its own policies centrally, and makes no difference at all on a CNI that implements no `NetworkPolicy` controller, which is what 6b measured of the CNI in this repository's own end-to-end harness — see [`docs/network-boundaries.md`](../../docs/network-boundaries.md), which carries what these objects buy and what they do not. |
| `operator.startupDeadline` | `5m` | The production value. `hack/e2e.sh` overrides it to `20s` for its own run. |
| `operator.leaderElect` | `true` | Passed straight to the operator's `--leader-elect` flag. |
| `operator.allowPluginVolumes` | `false` | Passed to `--allow-plugin-volumes`. Lets a group name a `spec.extraPlugins` claim whose contents are copied into every server's plugins directory on start — see [`docs/plugins.md`](../../docs/plugins.md). **It is not a security control.** A `PersistentVolumeClaim` is a namespaced object in the same trust domain as the group naming it, so anybody who can write one can write the other; the switch stops nobody who was not already stopped. What it buys is an operator being able to say "this installation runs no third-party plugins" and have that be a fact rather than a convention. |

| `networkNamespaces` | `[]` | Renders a narrowed `secrets: get` Role and its RoleBinding into each namespace named here, restricted by `resourceNames` to that entry's own secrets, with the binding's subject in the release's own namespace. It is the manual step above, done by the chart for the namespaces you already know — and it renders nothing by default, because a chart installed once cannot know the game namespaces somebody creates later. An entry naming no secrets is refused rather than rendered: a Role with no `resourceNames` grants `get` on *every* secret in that namespace, which is what naming them is meant to avoid. |

`replicas` and `imagePullSecrets` are deliberately not values. `readyz` hangs
off the leader lock, so a second replica never becomes ready — the Deployment
template hard-codes `1` and `strategy: Recreate`, with a comment explaining
why, and a knob whose only valid setting is 1 is a trap rather than a
feature. `imagePullSecrets` is absent because all three of this project's
images are public and unauthenticated by decision; if that changes, it is a
field and a test to add, not a restructuring.

## The `/cloud` permissions

The agents register a `/cloud` command on every Paper server and every Velocity
proxy. **Nobody holds any of these permissions by default**, so on a fresh
upgrade the command answers "unknown command" to every player. That is the safe
state, and it looks exactly like a broken install — grant one of these in
whatever permissions plugin the network runs, and it appears.

The console holds all four by default — on Paper that is how the console sender
answers every permission, and on Velocity the console starts with
`PermissionFunction.ALWAYS_TRUE`, checked in the pinned
`velocity-3.5.1-615.jar`. A Velocity permissions plugin that handles
`PermissionsSetupEvent` may replace that, which is where to look if `/cloud` is
invisible from a proxy console.

**On v0.2.7 that gave you no way in**: those pods were rendered without
`stdin`, so `kubectl attach` connected and delivered nothing — measured against
a live 0.2.7 lobby. From the next release the container keeps stdin open, so

```bash
kubectl attach -i <server-pod> -n <namespace> -c minecraft
```

reaches the console, and `cloud list` answers without anybody being granted
anything. The change rolls every group once, because the container spec is part
of the pod hash.

| Permission | What somebody holding it can do | What a player would notice |
|---|---|---|
| `spawnery.cloud.read` | `/cloud list`, `/cloud info <name>` — read the network as this agent last saw it. | Nothing. It changes nothing and costs nothing; the figures are the same ones `kubectl get servergroups` shows. |
| `spawnery.cloud.retire` | `/cloud retire <server>` — ask one server to stop taking joins and empty out. | Anyone already on that server stays, finishes, and is not kicked; the server disappears once it is empty. Nobody new lands there. Reversible only by the server's own lifetime running out — there is no un-retire. |
| `spawnery.cloud.scale` | `/cloud start <group> [n] [for <duration>]` and `/cloud stop <group>` — add capacity for a while, or end it early. | More servers, and **a larger bill**: each one is a pod with the group's own CPU and memory requests. Bounded by the group's `maxReplicas`, which this cannot lift, and by a twelve-hour ceiling on how long one boost may run. |
| `spawnery.cloud.events` | See cloud events in chat as they happen, and `/cloud events on\|off` to choose. | Chat lines, for the holder alone — servers becoming ready, retiring, failing. **On as soon as it is granted**; `/cloud events off` lasts for the session and the feed is back after a rejoin. It changes nothing and costs nothing. |

Give `.read` freely; it is a read of a picture the agent already holds. Treat
`.retire` as a moderator power — it moves nobody, but it takes a server out of
rotation and only the cluster can put one back. Treat `.scale` as a spending
power.

`.events` is the one to grant to whoever is on call. The lines are the same
transitions `kubectl get events` shows, derived from the same call rather than
computed twice, so somebody watching chat and somebody watching the cluster
never disagree about what happened. A rolling update arrives as one line —
`[cloud] 3 ReadyGatePassed in lobby (…)` — because ten lines is a feed people
turn off. **Warnings are never collapsed into it**, and each keeps the
operator's own sentence.

Nothing is delivered to an agent whose server has nobody holding `.events`
online, so a network that grants it to nobody carries no extra traffic at all.

**None of these can change what a group is.** `/cloud start` creates a
`ScaleBoost`, which expires; it never edits the `ServerGroup`, and the
operator's own ClusterRole has no write on `servergroups` at all. A group that
needs to be permanently bigger needs its file edited by a person.

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
