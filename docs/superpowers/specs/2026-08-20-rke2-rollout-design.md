# Milestone 6 — the RKE2 rollout

## 1. What this is

Milestone 6 has one thing left. `docs/handover-milestone-6.md` §6 names it and
says why nothing else can stand in for it: a real cluster, several nodes, a CNI
that is not `kindnet`, an API server enforcing Pod Security, images that
resolve, and a player who actually connects. Everything this repository knows
about its own behaviour under those conditions is either reasoned or measured
on a single-node `kind` cluster with deliberately unresolvable image tags.

Three of §12's preconditions closed on 2026-08-20, before this design was
written:

- all three images are pullable from `ghcr.io/spawnery/` without a pull
  secret — measured with `skopeo inspect --no-creds` against each of
  `paper:26.2-0.2.0`, `velocity:3.5.1-0.2.0` and `spawnery-operator:0.1.0`
  after the packages were made public;
- the operator is referenced by digest —
  `sha256:e5eb7626cdf2b7ac186e844aad418fd388c5c3d6ab225d09a37c041b5b4414ca`
  in `charts/spawnery/values.yaml`, written from `skopeo copy --digestfile` on
  release run 32351037208 and confirmed to resolve anonymously;
- `--startup-deadline` is the production value — the chart's default is `5m`
  and `hack/e2e.sh` overrides it only for its own run.

**What this does not establish**, stated first. This is one cluster, one CNI
and one distribution. Cilium with `cni-chaining-mode: portmap` and
`kube-proxy-replacement: false` is what `paulwtf` runs; a HostPort that works
here says nothing about Calico, and a NetworkPolicy enforced here says nothing
about a cluster whose CNI implements none — which is precisely the trap
milestone 6b fell into in the other direction. Every claim this rollout makes
is a claim about `paulwtf` on the day it was driven, and the runbook says so in
those words.

It also does not establish anything about scale. One lobby group and two
proxies with, at most, a handful of players is not load. Nothing here measures
what happens at a hundred.

## 2. The target, measured

Read off the cluster on 2026-08-20, not off documentation:

| | |
|---|---|
| Nodes | `server01`, `server02`, `server03` — all `control-plane,etcd`, **no taints**, so ordinary workloads schedule on all three |
| Kubernetes | `v1.34.3+rke2r1`, containerd `2.1.5-k3s1`, Debian 13 |
| Capacity | 32 GB and 12–16 cores per node |
| CNI | Cilium `v1.18.4`, `cni-chaining-mode: portmap`, `kube-proxy-replacement: false` |
| Storage | Longhorn, `longhorn` is the default StorageClass |
| Ingress | Traefik, one `LoadBalancer` Service holding all three node IPs |
| GitOps | Flux, four Kustomizations: `flux-system`, `infrastructure`, `apps`, `homepage` |
| DNS | external-dns `v0.21.0`, Cloudflare provider, `--cloudflare-proxied` **globally** |

Three facts from that table drive the whole design.

**HostPort works here.** `portmap` chaining is configured and several
workloads already use host ports — `csi-driver-nfs` on 19809, the etcd metrics
forwarder on 42381, the Cilium operator on 9963. The node IPs are public, so a
host port on 25565 is reachable from the internet without anything in front of
it.

**No LoadBalancer address is free.** There is exactly one
`CiliumLoadBalancerIPPool`, `node-ips`, whose blocks are the three node IPs as
`/32`, and Traefik's Service claims all three by name through
`io.cilium/lb-ipam-ips`. `IPS AVAILABLE` reads 0. A `Service` of type
`LoadBalancer` created today stays `<pending>` forever. §5 is how this design
answers that.

**`apps` and `infrastructure` reconcile in parallel.**
`clusters/paulwtf/apps.yaml` carries no `dependsOn`. Anything that places a
custom resource in one Kustomization and the CRD that defines it in another is
relying on Flux's retry, with `wait: true` and a five-minute timeout standing
next to it.

## 3. Where the artefacts live

Two repositories, two roles, and the boundary is not negotiable.

**`fluxcd`** gets the Flux objects and one two-line change to Traefik's values.
Its own commit convention — Conventional Commits, German subjects — applies
there, not this repository's.

**`spawnery`** gets the runbook, `docs/runbook-milestone-6-rollout.md`, in the
form of its five predecessors: driven once, marked `DRIVEN`, carrying the
actual output of the commands rather than a description of it.

Anything the run finds that is a defect in the code becomes an ordinary fix in
this repository — a test that fails first, then the change. It does not become
a paragraph in the runbook explaining a known problem. The runbook records what
happened; the repository records what was done about it.

## 4. The Flux wiring

A new top-level directory in the `fluxcd` repository:

```
spawnery/
  operator/     namespace.yaml  repository.yaml  release.yaml  kustomization.yaml
  network/      namespace.yaml  externalsecret.yaml  rbac.yaml
                network.yaml  kustomization.yaml
```

and two Kustomizations in `clusters/paulwtf/`:

```yaml
# spawnery-operator.yaml
spec:
  interval: 10m0s
  path: ./spawnery/operator
  prune: true
  wait: true
  timeout: 5m0s
  sourceRef: {kind: GitRepository, name: flux-system}
---
# spawnery-network.yaml
spec:
  dependsOn:
    - name: spawnery-operator
  interval: 10m0s
  path: ./spawnery/network
  prune: true
  wait: true
  timeout: 5m0s
  sourceRef: {kind: GitRepository, name: flux-system}
```

Two Kustomizations rather than one, and top-level rather than folded into
`apps/` or `infrastructure/`, for the reason §2 ends on: `dependsOn` makes the
CRDs land before the resources that need them, and keeping both out of the two
collecting paths means a broken `Network` cannot take the `apps` Kustomization
to `NotReady` alongside eleven unrelated applications. The precedent for a
Kustomization of its own is `homepage`, which already has one.

The chart comes from a `GitRepository` pinned to a tag:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: spawnery
  namespace: flux-system
spec:
  interval: 1h
  url: https://github.com/spawnery/spawnery.git
  ref:
    tag: v0.1.0
```

and the `HelmRelease` names `./charts/spawnery` inside it, installing into
`spawnery-system`. A tag rather than a branch: the chart carries the operator's
digest, and a branch would let the cluster run a chart whose digest belongs to
a commit nobody released. Upgrading is one line in this file.

An OCI chart pushed to `ghcr.io` would be the better shape once anyone but the
author consumes this chart. It is not that yet, and this session has twice paid
for machinery that had never run.

**Both namespaces carry `pod-security.kubernetes.io/enforce: restricted`.**
This is not aspiration. The chart's Deployment sets `runAsNonRoot`,
`seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false`,
`readOnlyRootFilesystem: true` and `capabilities: drop: [ALL]`, and
`internal/podspec` gives server and proxy pods the identical shape. §6's "CIS
`restricted` against the operator's own security context and against a game
server namespace" is therefore the installation's normal state rather than an
extra step — which also means a regression in either security context shows up
as pods that will not start, not as a check nobody runs.

The chart's `networkPolicy.enabled` stays at its default, `true`. Under
`kindnet` that policy is inert — 6b measured exactly that, and
`charts/spawnery/README.md` says so. Cilium enforces it. This is the first
enforcement the policy has ever had, which makes it a thing to verify (§7,
scenario 9) rather than a thing to assume.

`config/rbac/forwarding-secret-reader.yaml` is copied into
`spawnery/network/rbac.yaml` rather than referenced. It hard-codes its
RoleBinding subject's namespace to `spawnery-system`, which is exactly where
this installation puts the operator, so the manual edit its header describes —
and which `hack/e2e.sh` performs programmatically for its own non-default
namespace — does not arise here. Copying rather than linking means Flux owns
the object and `prune` reclaims it.

## 5. The shared LoadBalancer address

Cilium can assign one IP to several Services when their ports do not collide.
Traefik holds ports 25, 80, 143, 443, 465, 587, 993 and 5432. Minecraft is
25565. Nothing collides.

The proxy Service is the operator's to create, and `ExposeSpec.LoadBalancer`
already carries an `Annotations` map whose documented purpose is this — "copied
onto the Service, e.g. for MetalLB pool selection". So the ProxyGroup asks for
it directly, and no operator change is needed:

```yaml
expose:
  type: LoadBalancer
  loadBalancer:
    annotations:
      lbipam.cilium.io/ips: "185.117.3.72"
      lbipam.cilium.io/sharing-key: "paulwtf-node-ips"
      lbipam.cilium.io/sharing-cross-namespace: "traefik"
      external-dns.alpha.kubernetes.io/hostname: "mc.paul.wtf"
      external-dns.alpha.kubernetes.io/cloudflare-proxied: "false"
```

Sharing is symmetric: Traefik's Service must carry the same sharing key and
permit the `minecraft` namespace, so `infrastructure/traefik/values.yaml` gains

```yaml
    lbipam.cilium.io/sharing-key: "paulwtf-node-ips"
    lbipam.cilium.io/sharing-cross-namespace: "minecraft"
```

beside its existing `io.cilium/lb-ipam-ips` line. The two annotation prefixes
coexist; `io.cilium/lb-ipam-*` is the older spelling of the same thing and
Traefik's existing line is left alone rather than migrated, because migrating
it is a second change riding along with the one that matters.

**That Traefik edit is the only step in this rollout that can break something
unrelated to spawnery.** If Cilium refuses the sharing, Traefik loses its
addresses and every ingress on this cluster goes with them. Three things
follow, and they are requirements, not advice:

1. **The annotation names are unverified.** Nothing in this design has been
   tested against Cilium 1.18.4. `lbipam.cilium.io/sharing-key` and
   `lbipam.cilium.io/sharing-cross-namespace` are what the documentation gives;
   this project's standing rule is that documentation is not measurement. The
   runbook's first scenario measures it, before anything is committed.
2. **Traefik is annotated alone first.** A sharing key that shares with nobody
   changes nothing, so this step is expected to be invisible — and an
   invisible step is one whose failure is cheap. It is verified by an actual
   HTTPS request to one of the existing services, not by reading
   `EXTERNAL-IP` out of `kubectl get svc`. An IP still listed on an object is
   not an IP still serving traffic.
3. **The revert commit exists before the annotating commit is pushed.** If
   ingress goes down, the fix must not require composing anything.

Only then does the ProxyGroup's Service join.

`externalTrafficPolicy` stays at the CRD's default, `Local`, so the player's
address survives to the proxy — bans and rate limits depend on it.

## 6. The network

Namespace `minecraft`, holding what `config/samples/network.yaml` describes as
a realistic starting point, with the image tags that now exist:

| Object | Shape |
|---|---|
| `Network production` | `minecraftVersion: "26.2"`, defaults `requests: {cpu: 1, memory: 2Gi}`, `limits: {memory: 2Gi}` |
| `ServerGroup lobby` | `Ephemeral`, `ghcr.io/spawnery/paper:26.2-0.2.0`, `maxPlayers: 100`, `scaling: {minReplicas: 1, maxReplicas: 4, spareSlots: 40}`, `drain.timeoutSeconds: 60` |
| `ProxyGroup gateway` | `replicas: 2`, `ghcr.io/spawnery/velocity:3.5.1-0.2.0`, `expose` per §5, `routing.fallbackGroups: [lobby]`, `config: {playerLimit: 100, onlineMode: true}` |

Two proxy replicas rather than one, because the PodDisruptionBudget from 4c-3
has nothing to protect at one, and §6 wants node drain under a real eviction.
Three untainted nodes make two replicas ordinary rather than tight.

**The forwarding secret comes from Vault**, in the pattern the rest of the
`fluxcd` repository uses: an `ExternalSecret` against the `vault-store`
`ClusterSecretStore` produces `velocity-forwarding-secret` carrying the key
`secret`, which is the key `readForwardingSecret` looks for
(`podspec.ForwardingSecretKey`). The value is a one-time manual step —
`head -c 32 /dev/urandom | base64` into Vault — and it authenticates the proxy
to the backends, so it is a credential and not a setting. Until it exists the
`Network` reports `ForwardingSecretResolved=False/SecretNotFound`, which is a
described state rather than a failure.

## 7. The runbook

`docs/runbook-milestone-6-rollout.md`, driven once, in order. Every scenario
records the command and its actual output. A scenario that cannot be driven is
recorded as not driven, with the reason — this project's convention is that a
mechanism is not evidence.

**0. The sharing key.** §5's unverified claim, measured before the Traefik
commit is pushed: annotate Traefik, verify its three addresses still serve real
HTTPS traffic, then bring up the proxy Service and confirm both Services hold
185.117.3.72 simultaneously. If Cilium refuses, this scenario ends the
LoadBalancer half of the rollout and §6's LoadBalancer criterion is recorded as
unmet with the refusal quoted.

**1. Installation.** Flux reconciles both Kustomizations. The operator is
`Available` in `spawnery-system` under `restricted`, running the digest from
`values.yaml`, pulled from a public registry with no pull secret. Closes §12's
three preconditions as a set, in the cluster rather than on a workstation.

**2. `restricted` against a game server namespace.** Not admission alone: the
lobby's pods start, their agent reports in over the agent channel, and the
group reaches `Ready`. This is the first time real Paper and Velocity images
have run in any cluster — `test/e2e/manifests/e2e.yaml` names unresolvable tags
on purpose, so every previous run stopped at `ErrImagePull` by design.

**3. A real join.** A Minecraft client connects to `mc.paul.wtf` and lands on a
lobby server through the proxy. §6's "a real join", and the only scenario that
needs the repository owner at a keyboard rather than the agent.

**4. The LoadBalancer address.** `ProxyGroup.status.address` carries the shared
address, and a client reaches it. The operator's read-back path has been tested
against a status written by hand in the E2E; this is the first time a load
balancer wrote it.

**5. HostPort under the real CNI.** A second namespace, temporary and without
the `restricted` label, holding a `ProxyGroup` with `expose.type: HostPort` on
**25566** — not 25565, so it cannot collide with the LoadBalancer path on the
same node. The refusal under `baseline` is already measured and is not
re-derived here; what kind could never show is the working case.

**6. Node drain and the PodDisruptionBudget.** One of the three nodes, cordoned
and drained while a proxy runs on it. 4c-1's drain and 4c-3's budget under a
real eviction API, which a single-node cluster cannot approach.

**7. The three RBAC gaps §6 names.** `tokenreviews: create` falls out of the
first real agent authenticating. `persistentvolumeclaims: patch` needs a
persistent group, so one is created on Longhorn for this scenario and removed
after — §6 lists it as measured-absent, and leaving it absent when a real
storage class is finally available would be a choice not to look. And whether
`syncOccupiedLabel`, the call site `internal/rbacaudit/required.go` *names* for
`pods: patch`, runs at all — the second entry of its shape in
`docs/known-issues.md`.

**8. The widened denial measurement.** §6 records that reads *as a class* were
never measured and that the explanation which would generalise the result is a
hypothesis nothing established. The forwarding-secret grant is an uncached read
with a known-quiet failure path: `readForwardingSecret` folds a 403 into a
condition message carrying no `is forbidden:`, and nothing on that path logs.
Revoking it and watching what surfaces decides the question. It breaks the
network while it runs, so it comes after the join.

**9. The NetworkPolicy, enforced for the first time.** A pod in an unrelated
namespace attempts port 9443 on the operator. Inert under `kindnet`; not under
Cilium.

Scenarios 7 and 8 modify a live installation. Both undo themselves, and both
are why this is driven once, in one sitting, rather than piecemeal.

## 8. Failure, rollback and limits

**Removal is asymmetric, deliberately.** Dropping `spawnery/network/` from git
prunes the `Network`, `ServerGroup` and `ProxyGroup` objects. Dropping the
`HelmRelease` removes the control plane but **leaves the CRDs**: milestone 6d
gave them `helm.sh/resource-policy: keep` so that an uninstall does not take
every custom resource in the cluster with it. Uninstalling therefore removes
the operator, not the data. Removing the CRDs is a manual act, and it takes
everything.

**The failure modes, and what each one means:**

| Failure | What it means |
|---|---|
| Traefik loses its addresses | The only failure here that touches something unrelated. §5's three requirements exist for it. |
| `SecretReadForbidden` on the Network | The operator cannot read the forwarding secret. The network stalls; nothing crashes; no data is lost. Scenario 8 causes it on purpose. |
| A pod refused by Pod Security | 6c surfaces this on the ProxyGroup instead of leaving it in an event stream. Expected in scenario 5, a defect anywhere else. |
| An image will not pull | Measured against all three on 2026-08-20, including the digest reference the cluster actually uses. |

**Out of scope, and each its own piece of work:** a permanent persistent group,
the manual secret rotation from 5c, monitoring and alerting, and more than one
network.

## 9. Acceptance criteria

1. Both Flux Kustomizations reconcile to `Ready` on a cluster that has never
   had spawnery installed, with `spawnery-network` gated on
   `spawnery-operator` by `dependsOn`.
2. The operator runs in `spawnery-system` under
   `pod-security.kubernetes.io/enforce: restricted`, from the digest in
   `charts/spawnery/values.yaml`, with no pull secret anywhere in the chain.
3. The `minecraft` namespace enforces `restricted` and the lobby's pods reach
   `Ready` inside it.
4. `ProxyGroup gateway` reports an address, and a Minecraft client reaches a
   lobby server through it.
5. A `HostPort` ProxyGroup gets a running pod on this cluster's CNI.
6. A node drain evicts a proxy under the PodDisruptionBudget without taking the
   group below its budget.
7. Scenarios 7, 8 and 9 each produce a recorded result — including "no effect
   observed", which is a result and the one §6 most expects for part of 8.
8. `docs/runbook-milestone-6-rollout.md` is marked `DRIVEN` and carries the
   commands' real output.

Criterion 4 depends on scenario 0. If Cilium refuses the sharing, criterion 4
is recorded unmet, with the refusal quoted and the remaining criteria driven
regardless — a `NodePort` on 30000–32767 would let a client connect, but it is
not what §6 asked for and this design does not let it stand in.
