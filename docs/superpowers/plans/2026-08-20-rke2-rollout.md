# RKE2 Rollout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Install Spawnery on the `paulwtf` RKE2 cluster through Flux, then
drive the evidence run `docs/handover-milestone-6.md` §6 asks for, and record
it in `docs/runbook-milestone-6-rollout.md`.

**Architecture:** Two Flux Kustomizations in the `fluxcd` repository —
`spawnery-operator` (namespace, `GitRepository` pinned to `v0.1.0`,
`HelmRelease`) and `spawnery-network` (namespace, forwarding secret, per-
namespace RBAC, the `Network`/`ServerGroup`/`ProxyGroup` objects), the second
gated on the first by `dependsOn`. The `ProxyGroup`'s Service shares one of
Traefik's LoadBalancer addresses through Cilium LB-IPAM annotations the
operator copies verbatim from `expose.loadBalancer.annotations`. Every task
ends by reading the outcome back out of the cluster and appending that output
to the runbook.

**Tech Stack:** RKE2 v1.34.3, Cilium v1.18.4 (portmap chaining, kube-proxy
replacement off), Flux (`source.toolkit.fluxcd.io/v1`,
`kustomize.toolkit.fluxcd.io/v1`, `helm.toolkit.fluxcd.io/v2`),
external-secrets `external-secrets.io/v1` against the `vault-store`
`ClusterSecretStore`, external-dns v0.21.0 with the Cloudflare provider,
Longhorn, Helm chart `charts/spawnery` at tag `v0.1.0`.

## Global Constraints

- **This runs against a live production cluster.** `paulwtf` serves the
  repository owner's mail, photos, password vault and file sync. Nothing in
  this plan may delete, patch or annotate an object outside
  `spawnery-system`, `minecraft`, `minecraft-hostport`, and the one Traefik
  Service annotation Task 5 adds.
- **Never run `kubectl delete` against a namespace this plan did not create.**
  The two it creates are `minecraft-hostport` (Task 8, temporary) and any
  probe pod named in a task's own steps.
- **Never push to `master` in either repository without being told to, never
  merge, and never push a tag.** A `v*` tag triggers a real publish under this
  project's name.
- Two repositories: `/home/paul/git/spawnery` (this repo — the runbook and any
  code fix) and `/home/paul/git/fluxcd` (the Flux objects). The `fluxcd`
  repository's own commit convention applies there: Conventional Commits with
  German subjects. This repository's applies here: Conventional Commits with
  English subjects, and every commit ends with the two trailers used
  throughout its history.
- The operator namespace is `spawnery-system` and nothing may rename it:
  `config/rbac/forwarding-secret-reader.yaml` hard-codes that name as its
  RoleBinding subject's namespace, and the chart hard-codes the ServiceAccount
  name `spawnery-operator`.
- The network namespace is `minecraft`. Both namespaces carry
  `pod-security.kubernetes.io/enforce: restricted`.
- Images, exactly: `ghcr.io/spawnery/paper:26.2-0.2.0`,
  `ghcr.io/spawnery/velocity:3.5.1-0.2.0`, and the operator by digest from the
  chart (`sha256:e5eb7626cdf2b7ac186e844aad418fd388c5c3d6ab225d09a37c041b5b4414ca`).
- The shared LoadBalancer address is `185.117.3.72` (`server03`). The sharing
  key is `paulwtf-node-ips`. The hostname is `mc.paul.wtf`.
- The HostPort proof uses port **25566**, never 25565.
- **A guard checks its outcome, not its input.** Every verification step in
  this plan reads the object back out of the cluster. `kubectl apply`
  succeeding is not evidence that what you meant is what is there — a
  RoleBinding naming a ServiceAccount in a namespace that does not exist
  applies cleanly and grants nothing.
- Nothing is marked done from a document. The runbook records commands and
  their real output; a scenario that could not be driven is recorded as not
  driven, with the reason.

---

## File Structure

**In `/home/paul/git/fluxcd`:**

| File | Responsibility |
|---|---|
| `clusters/paulwtf/spawnery-operator.yaml` | Flux `Kustomization` over `./spawnery/operator` |
| `clusters/paulwtf/spawnery-network.yaml` | Flux `Kustomization` over `./spawnery/network`, `dependsOn` the above |
| `spawnery/operator/namespace.yaml` | `spawnery-system`, PSA `restricted` |
| `spawnery/operator/repository.yaml` | `GitRepository` `spawnery` at tag `v0.1.0` |
| `spawnery/operator/release.yaml` | `HelmRelease` installing `./charts/spawnery` |
| `spawnery/operator/kustomization.yaml` | lists the three above |
| `spawnery/network/namespace.yaml` | `minecraft`, PSA `restricted` |
| `spawnery/network/externalsecret.yaml` | `velocity-forwarding-secret` from Vault |
| `spawnery/network/rbac.yaml` | the per-namespace forwarding-secret reader |
| `spawnery/network/network.yaml` | `Network`, `ServerGroup`, `ProxyGroup` |
| `spawnery/network/kustomization.yaml` | lists the four above |
| `infrastructure/traefik/values.yaml` | modified: two sharing annotations |

**In `/home/paul/git/spawnery`:**

| File | Responsibility |
|---|---|
| `docs/runbook-milestone-6-rollout.md` | the evidence, appended to by every task |

---

## Task 1: The runbook's skeleton and scenario 0's first half

**Files:**
- Create: `/home/paul/git/spawnery/docs/runbook-milestone-6-rollout.md`

**Interfaces:**
- Produces: the runbook file with a `## Scenario N` heading per scenario, in
  the order of the design's §7. Later tasks append their output under their own
  heading and change nothing else.

This task also answers the design's §5 open question — the Cilium annotation
names — **before** anything is committed to the `fluxcd` repository, because
the answer decides whether Task 5 is safe to attempt at all.

- [ ] **Step 1: Write the runbook skeleton**

Create `docs/runbook-milestone-6-rollout.md`:

```markdown
# Runbook — milestone 6, the RKE2 rollout

**Status: IN PROGRESS**

Driven against `paulwtf` on 2026-08-20. The design is
`docs/superpowers/specs/2026-08-20-rke2-rollout-design.md`; §7 of it lists
these scenarios and §1 says what this run does not establish.

Every section carries the command and its real output. A scenario that could
not be driven says so and says why.

## Scenario 0 — the sharing key

## Scenario 1 — installation

## Scenario 2 — `restricted` against a game server namespace

## Scenario 3 — a real join

## Scenario 4 — the LoadBalancer address

## Scenario 5 — HostPort under the real CNI

## Scenario 6 — node drain and the PodDisruptionBudget

## Scenario 7 — the three RBAC gaps

## Scenario 8 — the widened denial measurement

## Scenario 9 — the NetworkPolicy, enforced for the first time
```

- [ ] **Step 2: Establish what Cilium 1.18.4 actually accepts**

Cilium validates LB-IPAM annotations at assignment time, not at admission, so
the way to learn whether the names are right is to create a Service that asks
for a share and read back what Cilium did. Do it in a namespace this plan
owns nothing in yet — use `default`, and delete the probe immediately after.

```bash
kubectl -n default create -f - <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: lbipam-probe
  annotations:
    lbipam.cilium.io/ips: "185.117.3.72"
    lbipam.cilium.io/sharing-key: "paulwtf-node-ips"
    lbipam.cilium.io/sharing-cross-namespace: "traefik"
spec:
  type: LoadBalancer
  ports:
    - name: mc
      port: 25565
      targetPort: 25565
      protocol: TCP
  selector:
    no-such-app: lbipam-probe
EOF
sleep 15
kubectl -n default get svc lbipam-probe -o wide
kubectl -n default get svc lbipam-probe -o jsonpath='{.status.loadBalancer.ingress}{"\n"}'
kubectl get events -n default --field-selector involvedObject.name=lbipam-probe
kubectl get ciliumloadbalancerippools node-ips -o wide
```

Expected if the names are right and sharing needs both sides: the probe stays
**without** an ingress address, because Traefik does not yet carry the sharing
key. That is the informative outcome — it means Cilium parsed the annotations
and refused the share for the stated reason rather than ignoring them.

Expected if a name is wrong: Cilium ignores the annotation entirely and the
Service reports a conflict or stays pending with a message naming the pool
rather than the sharing key.

Record the exact output either way. This is a measurement, not a formality:
Task 5 touches Traefik, and Task 5 is the only step in this plan that can take
down services unrelated to spawnery.

- [ ] **Step 3: Delete the probe**

```bash
kubectl -n default delete svc lbipam-probe
kubectl -n default get svc lbipam-probe 2>&1 | tail -1
```

Expected: `Error from server (NotFound)`. Read it back — do not assume the
delete took.

- [ ] **Step 4: Write scenario 0's first half into the runbook**

Under `## Scenario 0 — the sharing key`, paste the commands and their real
output, and state in one sentence what it establishes about the annotation
names. If it establishes that a name is wrong, say which and what the right
one is, and stop: Tasks 5 and 6 depend on this and the plan needs correcting
before they run.

- [ ] **Step 5: Commit**

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): the rollout runbook, and what Cilium says about sharing

Scenario 0's first half, driven before anything reaches the fluxcd
repository: the annotation names in the design's §5 came from documentation
and this project does not let documentation stand in for measurement. The
output here is what Cilium 1.18.4 does with them.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 2: The operator's Flux objects

**Files:**
- Create: `/home/paul/git/fluxcd/spawnery/operator/namespace.yaml`
- Create: `/home/paul/git/fluxcd/spawnery/operator/repository.yaml`
- Create: `/home/paul/git/fluxcd/spawnery/operator/release.yaml`
- Create: `/home/paul/git/fluxcd/spawnery/operator/kustomization.yaml`
- Create: `/home/paul/git/fluxcd/clusters/paulwtf/spawnery-operator.yaml`
- Modify: `/home/paul/git/spawnery/docs/runbook-milestone-6-rollout.md`

**Interfaces:**
- Produces: namespace `spawnery-system` holding a `spawnery-operator`
  Deployment and a `spawnery-operator` ServiceAccount. Task 4's RoleBinding
  names both.

- [ ] **Step 1: Write the namespace**

`spawnery/operator/namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: spawnery-system
  labels:
    goldilocks.fairwinds.com/enabled: "true"
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/warn: restricted
```

`restricted` rather than `baseline`: the chart's Deployment already sets
`runAsNonRoot`, `seccompProfile: RuntimeDefault`,
`allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true` and
`capabilities: drop: [ALL]`, so this is the level it already satisfies, and
enforcing it means a regression shows up as a pod that will not start.

- [ ] **Step 2: Write the GitRepository**

`spawnery/operator/repository.yaml`:

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

- [ ] **Step 3: Write the HelmRelease**

`spawnery/operator/release.yaml`:

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: spawnery
  namespace: spawnery-system
spec:
  interval: 1h0m0s
  chart:
    spec:
      chart: ./charts/spawnery
      sourceRef:
        kind: GitRepository
        name: spawnery
        namespace: flux-system
  install:
    crds: Create
  upgrade:
    crds: CreateReplace
```

No `version:` — that field belongs to a `HelmRepository` source; with a
`GitRepository` the tag on the source is the version. `crds: CreateReplace`
on upgrade because the chart ships its CRDs in `templates/` with
`helm.sh/resource-policy: keep`, so Helm will not delete them and a replace is
how a CRD change reaches the cluster.

- [ ] **Step 4: Write the kustomization**

`spawnery/operator/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - namespace.yaml
  - repository.yaml
  - release.yaml
```

- [ ] **Step 5: Write the Flux Kustomization**

`clusters/paulwtf/spawnery-operator.yaml`:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: spawnery-operator
  namespace: flux-system
spec:
  interval: 10m0s
  path: ./spawnery/operator
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  wait: true
  timeout: 5m0s
```

- [ ] **Step 6: Render it locally before pushing anything**

```bash
cd /home/paul/git/fluxcd
kubectl kustomize spawnery/operator
```

Expected: three documents, and the `HelmRelease` carrying
`chart: ./charts/spawnery`. This catches a YAML error without involving the
cluster.

- [ ] **Step 7: Commit and push**

```bash
cd /home/paul/git/fluxcd
git add spawnery/operator clusters/paulwtf/spawnery-operator.yaml
git commit -m "feat(spawnery): Operator per HelmRelease aus dem Chart-Tag v0.1.0

Eigene Top-Level-Kustomization statt apps/ oder infrastructure/: das Netz
kommt in einer zweiten, die per dependsOn dahinter haengt, und ein Fehler
darin soll nicht die apps-Kustomization mitnehmen. Das Chart kommt aus einer
GitRepository auf dem Tag v0.1.0, weil es den Operator-Digest traegt und ein
Branch den Cluster ein Chart fahren liesse, dessen Digest zu keinem Release
gehoert."
git push
```

- [ ] **Step 8: Reconcile and read the outcome back**

```bash
flux reconcile source git flux-system
flux get kustomizations spawnery-operator
flux get helmreleases -n spawnery-system
kubectl -n spawnery-system get deployment spawnery-operator -o wide
kubectl -n spawnery-system get pods
kubectl -n spawnery-system get deployment spawnery-operator \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
kubectl get ns spawnery-system -o jsonpath='{.metadata.labels}{"\n"}'
```

Expected: the Kustomization and the HelmRelease `Ready: True`, the Deployment
`1/1`, the image ending in
`@sha256:e5eb7626cdf2b7ac186e844aad418fd388c5c3d6ab225d09a37c041b5b4414ca`,
and the namespace carrying `pod-security.kubernetes.io/enforce: restricted`.

The image assertion matters on its own: it is the first time in this
project's history that a cluster has resolved the digest reference the chart
ships, and it needs no pull secret anywhere in the chain.

- [ ] **Step 9: Confirm no pull secret is involved**

```bash
kubectl -n spawnery-system get deployment spawnery-operator \
  -o jsonpath='{.spec.template.spec.imagePullSecrets}{"\n"}'
kubectl -n spawnery-system get serviceaccount spawnery-operator \
  -o jsonpath='{.imagePullSecrets}{"\n"}'
```

Expected: both empty. This closes `docs/handover-milestone-6.md` §12's third
precondition in the cluster rather than against a registry from a
workstation.

- [ ] **Step 10: Write scenario 1 into the runbook and commit**

Paste the output of steps 8 and 9 under `## Scenario 1 — installation`.

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): scenario 1 — the operator runs on paulwtf

Flux installs the chart from tag v0.1.0 into spawnery-system under Pod
Security restricted, and the kubelet resolves the digest in
charts/spawnery/values.yaml from a public registry with no pull secret on
either the Deployment or the ServiceAccount. §12's three preconditions,
closed in a cluster rather than against a registry from a workstation.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 3: The forwarding secret in Vault

**Files:** none. This task is a request to the repository owner and a
verification.

**Interfaces:**
- Produces: a Vault entry at `spawnery/forwarding` with the property `secret`.
  Task 4's `ExternalSecret` reads exactly that path and property.

- [ ] **Step 1: Ask the repository owner to create the Vault entry**

The value authenticates the Velocity proxy to the Paper backends. It is a
credential, so it is generated once and never committed:

```
head -c 32 /dev/urandom | base64
```

goes into Vault at path `spawnery/forwarding`, property `secret`.

Do not generate it yourself and do not put it anywhere but Vault. If the
owner is not available, stop here — Task 4 can be written but not verified,
and this plan does not mark unverified work done.

- [ ] **Step 2: Verify the entry is reachable by the operator's store**

There is no way to read Vault from here without the owner's token, so the
verification is Task 4's: the `ExternalSecret` either produces a `Secret` with
a non-empty `secret` key or it reports why not. Note in the runbook that the
value's existence is verified through the `ExternalSecret` and not directly.

---

## Task 4: The network's namespace, secret and RBAC

**Files:**
- Create: `/home/paul/git/fluxcd/spawnery/network/namespace.yaml`
- Create: `/home/paul/git/fluxcd/spawnery/network/externalsecret.yaml`
- Create: `/home/paul/git/fluxcd/spawnery/network/rbac.yaml`
- Create: `/home/paul/git/fluxcd/spawnery/network/kustomization.yaml`
- Create: `/home/paul/git/fluxcd/clusters/paulwtf/spawnery-network.yaml`

**Interfaces:**
- Consumes: `spawnery-system` and the ServiceAccount `spawnery-operator` from
  Task 2; the Vault entry from Task 3.
- Produces: namespace `minecraft` holding `velocity-forwarding-secret` with
  the key `secret`, and a Role and RoleBinding both named
  `spawnery-forwarding-secret-reader`. Task 5 puts the `Network` beside them.

- [ ] **Step 1: Write the namespace**

`spawnery/network/namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: minecraft
  labels:
    goldilocks.fairwinds.com/enabled: "true"
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/warn: restricted
```

- [ ] **Step 2: Write the ExternalSecret**

`spawnery/network/externalsecret.yaml`:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft
spec:
  secretStoreRef:
    name: vault-store
    kind: ClusterSecretStore
  target:
    name: velocity-forwarding-secret
  data:
    - secretKey: secret
      remoteRef:
        key: spawnery/forwarding
        property: secret
```

`secretKey: secret` is not a stylistic choice — `podspec.ForwardingSecretKey`
is the string `"secret"` (`internal/podspec/server.go:121`) and
`readForwardingSecret` reports `SecretKeyMissing` for anything else.

- [ ] **Step 3: Write the RBAC, with the namespace made explicit**

`spawnery/network/rbac.yaml` is `config/rbac/forwarding-secret-reader.yaml`
with `namespace: minecraft` **added to both objects**:

```yaml
# Copied from spawnery's config/rbac/forwarding-secret-reader.yaml, which
# deliberately carries no metadata.namespace because `kubectl apply -n` is
# meant to supply it. Flux does not: a namespaced object with no namespace
# lands in `default`, and a Role granting secret reads in `default` is both
# wrong and silent. The namespace is therefore explicit here.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: spawnery-forwarding-secret-reader
  namespace: minecraft
  labels:
    app.kubernetes.io/name: spawnery
rules:
  - apiGroups:
      - ""
    resources:
      - secrets
    verbs:
      - get
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: spawnery-forwarding-secret-reader
  namespace: minecraft
  labels:
    app.kubernetes.io/name: spawnery
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: spawnery-forwarding-secret-reader
subjects:
  - kind: ServiceAccount
    name: spawnery-operator
    namespace: spawnery-system
```

The subject's `namespace: spawnery-system` is correct unedited because this
installation puts the operator there. `hack/e2e.sh` rewrites it only because
it installs into `platform-system`.

- [ ] **Step 4: Write the kustomization**

`spawnery/network/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - namespace.yaml
  - externalsecret.yaml
  - rbac.yaml
```

`network.yaml` is added to this list in Task 5, not now: this task's
verification is about the secret and the grant, and adding the `Network`
before the grant exists would produce a `SecretReadForbidden` that means
nothing.

- [ ] **Step 5: Write the Flux Kustomization**

`clusters/paulwtf/spawnery-network.yaml`:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: spawnery-network
  namespace: flux-system
spec:
  dependsOn:
    - name: spawnery-operator
  interval: 10m0s
  path: ./spawnery/network
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  wait: true
  timeout: 5m0s
```

- [ ] **Step 6: Render locally, then commit and push**

```bash
cd /home/paul/git/fluxcd
kubectl kustomize spawnery/network
git add spawnery/network clusters/paulwtf/spawnery-network.yaml
git commit -m "feat(spawnery): Namespace, Forwarding-Secret aus Vault und der Leserechte-Grant

Der Grant liegt in spawnery bewusst ohne metadata.namespace, weil dort
kubectl apply -n ihn liefert. Flux liefert ihn nicht -- ein namespaced
Objekt ohne Namespace landet in default, und ein Role, das dort Secrets
lesen darf, ist falsch und still. Deshalb hier ausgeschrieben."
git push
flux reconcile source git flux-system
```

- [ ] **Step 7: Read back what actually landed**

```bash
flux get kustomizations spawnery-network
kubectl get ns minecraft -o jsonpath='{.metadata.labels}{"\n"}'
kubectl -n minecraft get externalsecret velocity-forwarding-secret
kubectl -n minecraft get secret velocity-forwarding-secret \
  -o jsonpath='{.data.secret}' | wc -c
kubectl -n minecraft get rolebinding spawnery-forwarding-secret-reader \
  -o jsonpath='{.subjects[0].kind}/{.subjects[0].name}/{.subjects[0].namespace}{"\n"}'
kubectl -n default get role spawnery-forwarding-secret-reader 2>&1 | tail -1
```

Expected: the Kustomization `Ready: True`; the namespace enforcing
`restricted`; the `ExternalSecret` `SecretSynced`; a non-zero byte count for
the secret's `secret` key; the RoleBinding subject reading exactly
`ServiceAccount/spawnery-operator/spawnery-system`; and
`Error from server (NotFound)` for the `default` namespace, proving the
namespace was not silently dropped.

The last two are the same check `hack/e2e.sh`'s
`check_forwarding_secret_reader_subject` performs, and for the same reason:
`kubectl apply` does not reject a subject naming a namespace that does not
exist, so the only way to know the grant is real is to read it back.

- [ ] **Step 8: Prove the grant with an access review rather than by reading it**

```bash
kubectl auth can-i get secrets \
  --as=system:serviceaccount:spawnery-system:spawnery-operator \
  -n minecraft
kubectl auth can-i get secrets \
  --as=system:serviceaccount:spawnery-system:spawnery-operator \
  -n default
```

Expected: `yes` then `no`. The second is the one that matters — it proves the
grant is scoped to the namespace that holds a `Network` and not wider.

- [ ] **Step 9: Append to the runbook under scenario 1 and commit**

Add the step 7 and 8 output to `## Scenario 1 — installation`, under a
sub-heading naming it as the network namespace's preparation.

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): the minecraft namespace, its secret and the scoped grant

Read back rather than assumed: the RoleBinding's subject, the absence of a
Role in default, and an access review in both namespaces. kubectl apply
accepts a subject naming a namespace that does not exist, so applying
cleanly proves nothing about what was granted.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 5: Traefik shares its address

**Files:**
- Modify: `/home/paul/git/fluxcd/infrastructure/traefik/values.yaml:18-19`

**Interfaces:**
- Consumes: scenario 0's finding from Task 1 about the annotation names.
- Produces: a Traefik Service carrying `lbipam.cilium.io/sharing-key:
  paulwtf-node-ips` and `lbipam.cilium.io/sharing-cross-namespace: minecraft`.
  Task 6's `ProxyGroup` shares against exactly those.

**This is the only task in this plan that can break something unrelated to
spawnery.** If Cilium refuses the sharing, Traefik loses its three addresses
and every ingress on this cluster goes with them — mail, photos, the password
vault, file sync.

- [ ] **Step 1: Prepare the revert before making the change**

```bash
cd /home/paul/git/fluxcd
git rev-parse HEAD
```

Write that SHA into the runbook now as the known-good state, together with the
recovery command, which is fixed in advance because the change in step 3 will
be the tip of the branch when it lands:

```
cd /home/paul/git/fluxcd && git revert --no-edit HEAD && git push && flux reconcile kustomization infrastructure
```

The point is that if ingress goes down, nobody has to compose anything. Run it
verbatim; do not stop to work out which commit to name.

- [ ] **Step 2: Record what working ingress looks like right now**

```bash
kubectl -n traefik get svc traefik -o wide
curl -sS -o /dev/null -w '%{http_code} %{remote_ip}\n' https://vaultwarden.paul.wtf/
```

Expected: three external IPs, and an HTTP status from a real request. The
`curl` is the point — an address still listed on an object is not an address
still serving traffic, and step 5 compares against this line.

- [ ] **Step 3: Add the two annotations**

In `infrastructure/traefik/values.yaml`, the `service.annotations` block
becomes:

```yaml
service:
  type: LoadBalancer
  annotations:
    io.cilium/lb-ipam-ips: "45.137.203.198,45.13.227.226,185.117.3.72"
    # Geteilt mit dem Minecraft-Netz in Namespace minecraft: Cilium vergibt
    # eine IP an mehrere Services, solange sich die Ports nicht ueberschneiden.
    # Traefik haelt 25, 80, 143, 443, 465, 587, 993 und 5432; Minecraft ist
    # 25565. Ohne diese zwei Zeilen bekaeme der Proxy-Service nie eine
    # Adresse -- der Pool node-ips hat 0 freie IPs.
    lbipam.cilium.io/sharing-key: "paulwtf-node-ips"
    lbipam.cilium.io/sharing-cross-namespace: "minecraft"
```

The existing `io.cilium/lb-ipam-ips` line stays as it is. It is the older
spelling of the same annotation and both prefixes coexist; migrating it is a
second change and does not belong in this one.

- [ ] **Step 4: Commit and push**

```bash
cd /home/paul/git/fluxcd
git add infrastructure/traefik/values.yaml
git commit -m "feat(traefik): Sharing-Key, damit das Minecraft-Netz eine Adresse bekommt

Der Pool node-ips besteht aus genau den drei Node-IPs, und Traefik haelt
alle drei. Cilium kann eine IP zwischen Services teilen, wenn sich die Ports
nicht ueberschneiden; 25565 ueberschneidet sich mit nichts, was Traefik
haelt. Beide Seiten muessen denselben Schluessel tragen, deshalb hier."
git push
flux reconcile kustomization infrastructure
```

- [ ] **Step 5: Verify Traefik still serves traffic**

```bash
kubectl -n traefik get svc traefik -o wide
kubectl -n traefik get svc traefik -o jsonpath='{.metadata.annotations}{"\n"}'
curl -sS -o /dev/null -w '%{http_code} %{remote_ip}\n' https://vaultwarden.paul.wtf/
```

Expected: the same three addresses as in step 2, the two new annotations
present, and the same HTTP status from the same real request.

If the addresses are gone or the request fails, run the revert from step 1
immediately, then stop and report. Do not attempt a fix-forward on a cluster
whose ingress is down.

- [ ] **Step 6: Write scenario 0's second half into the runbook and commit**

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): scenario 0 — Traefik carries the sharing key, and still serves

A sharing key that shares with nobody should change nothing, so this step is
expected to be invisible; an invisible step is one whose failure is cheap.
Verified with a real HTTPS request rather than by reading EXTERNAL-IP, because
an address still listed on an object is not an address still serving traffic.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 6: The network

**Files:**
- Create: `/home/paul/git/fluxcd/spawnery/network/network.yaml`
- Modify: `/home/paul/git/fluxcd/spawnery/network/kustomization.yaml`

**Interfaces:**
- Consumes: the namespace, secret and grant from Task 4; Traefik's sharing key
  from Task 5.
- Produces: `Network production`, `ServerGroup lobby`, `ProxyGroup gateway`,
  all in `minecraft`. Tasks 8 through 12 measure against these.

- [ ] **Step 1: Write the network objects**

`spawnery/network/network.yaml`:

```yaml
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: production
  namespace: minecraft
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
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: Ephemeral
  image: ghcr.io/spawnery/paper:26.2-0.2.0
  maxPlayers: 100
  drain:
    timeoutSeconds: 60
  scaling:
    minReplicas: 1
    maxReplicas: 4
    spareSlots: 40
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft
spec:
  networkRef:
    name: production
  replicas: 2
  image: ghcr.io/spawnery/velocity:3.5.1-0.2.0
  expose:
    type: LoadBalancer
    loadBalancer:
      annotations:
        lbipam.cilium.io/ips: "185.117.3.72"
        lbipam.cilium.io/sharing-key: "paulwtf-node-ips"
        lbipam.cilium.io/sharing-cross-namespace: "traefik"
        external-dns.alpha.kubernetes.io/hostname: "mc.paul.wtf"
        external-dns.alpha.kubernetes.io/cloudflare-proxied: "false"
  routing:
    fallbackGroups:
      - lobby
  config:
    playerLimit: 100
    motd: "spawnery"
    onlineMode: true
```

`cloudflare-proxied: "false"` is load-bearing: external-dns runs with
`--cloudflare-proxied` as a global default, and a proxied record points at
Cloudflare, which forwards no TCP on 25565 without Spectrum. The name would
resolve and nothing would connect, with no error anywhere.

If Task 1's scenario 0 found different annotation names, use those and note
the correction in the runbook.

- [ ] **Step 2: Add it to the kustomization**

`spawnery/network/kustomization.yaml` becomes:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - namespace.yaml
  - externalsecret.yaml
  - rbac.yaml
  - network.yaml
```

- [ ] **Step 3: Commit, push, reconcile**

```bash
cd /home/paul/git/fluxcd
kubectl kustomize spawnery/network
git add spawnery/network
git commit -m "feat(spawnery): das Netz -- eine Lobby und zwei Proxies

Zwei Proxy-Replicas, weil das PodDisruptionBudget aus 4c-3 bei einer nichts
zu schuetzen hat. cloudflare-proxied: false ist nicht Kosmetik: external-dns
laeuft global mit --cloudflare-proxied, und ein proxied A-Eintrag zeigt auf
Cloudflare, das ohne Spectrum kein TCP auf 25565 weiterleitet -- der Name
loeste auf und nichts verbaende sich, ohne Fehler irgendwo."
git push
flux reconcile source git flux-system
```

- [ ] **Step 4: Read back the network's state**

```bash
kubectl -n minecraft get networks,servergroups,proxygroups
kubectl -n minecraft get network production -o jsonpath='{.status.conditions}{"\n"}' | python3 -m json.tool
kubectl -n minecraft get pods -o wide
```

Expected: the `Network` reporting `ForwardingSecretResolved=True/SecretResolved`
and `Accepted=True`; lobby pods and proxy pods present; the proxy pods spread
over more than one node.

- [ ] **Step 5: Wait for the lobby to be ready and record it**

```bash
kubectl -n minecraft wait --for=condition=Ready pod -l spawnery.cloud/role=server --timeout=10m
kubectl -n minecraft get servergroup lobby -o jsonpath='{.status}{"\n"}' | python3 -m json.tool
kubectl -n minecraft get pods -o wide
```

Expected: at least one lobby pod `Ready`, and the group's status reporting it.
This is the first time real Paper images have run in any cluster — the E2E
names unresolvable tags on purpose — so a failure here is a finding, not a
setback. Capture the pod's logs either way:

```bash
kubectl -n minecraft logs -l spawnery.cloud/role=server --tail=80
```

- [ ] **Step 6: Write scenario 2 into the runbook and commit**

Under `## Scenario 2 — restricted against a game server namespace`, record the
namespace's enforce label, the pods, the group status and the server log tail.
State plainly that this is admission **and** execution: the pods started under
`restricted`, they did not merely pass admission.

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): scenario 2 — Paper runs under Pod Security restricted

Admission and execution, not admission alone. This is the first time the
Paper image has run in any cluster: test/e2e/manifests/e2e.yaml names
unresolvable tags on purpose, so every earlier run stopped at ErrImagePull by
design.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 7: The address, the name, and a real join

**Files:**
- Modify: `/home/paul/git/spawnery/docs/runbook-milestone-6-rollout.md`

**Interfaces:**
- Consumes: the `ProxyGroup` from Task 6.

- [ ] **Step 1: Read the assigned address**

```bash
kubectl -n minecraft get svc -o wide
kubectl -n minecraft get proxygroup gateway -o jsonpath='{.status.address}{"\n"}'
kubectl -n traefik get svc traefik -o jsonpath='{.status.loadBalancer.ingress}{"\n"}'
```

Expected: the proxy Service holding `185.117.3.72`, the `ProxyGroup`'s
`status.address` carrying it, and Traefik still holding all three. Both
Services on the same address at the same time is the whole point of Task 5.

- [ ] **Step 2: Verify the DNS record**

```bash
dig +short mc.paul.wtf
dig +short mc.paul.wtf @1.1.1.1
```

Expected: exactly `185.117.3.72`, and **not** a Cloudflare address
(Cloudflare's proxy ranges begin `104.`, `172.6x.`, `188.114.`, `162.159.`).
A Cloudflare address means `cloudflare-proxied` did not take, and the name is
dead even though every object involved reports success.

- [ ] **Step 3: Prove the port answers**

```bash
timeout 5 bash -c 'cat < /dev/null > /dev/tcp/mc.paul.wtf/25565' && echo "open" || echo "closed"
```

Expected: `open`. This is reachability from this machine; the join in step 4
is reachability for a player.

- [ ] **Step 4: The real join**

Ask the repository owner to connect a Minecraft client to `mc.paul.wtf` and
report what happens: whether the server list ping shows the MOTD, whether the
join lands on a lobby server, and the player name that arrives. Then read the
network's side of it:

```bash
kubectl -n minecraft get servergroup lobby -o jsonpath='{.status}{"\n"}' | python3 -m json.tool
kubectl -n minecraft get pods -l spawnery.cloud/role=server \
  -o custom-columns='NAME:.metadata.name,OCCUPIED:.metadata.labels.spawnery\.cloud/occupied'
kubectl -n minecraft logs -l spawnery.cloud/role=proxy --tail=60
```

The `occupied` label is worth its own line: `syncOccupiedLabels` is the path
Task 10 asks about, and a join is what moves it.

If the owner is not available, record scenario 3 as **not driven** with that
as the reason, and carry on with Task 8. Do not simulate a join and describe
it as one.

- [ ] **Step 5: Write scenarios 3 and 4 into the runbook and commit**

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): scenarios 3 and 4 — a shared address, a name, and a player

Both Services hold 185.117.3.72 at once, which is what the Traefik edit in
scenario 0 bought. The DNS check reads the resolved address rather than
trusting the annotation: a proxied Cloudflare record would resolve and refuse
TCP on 25565 with no error anywhere.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 8: HostPort under the real CNI

**Files:**
- Modify: `/home/paul/git/spawnery/docs/runbook-milestone-6-rollout.md`

This task creates and then removes a namespace with `kubectl`, not through
Flux: it is evidence, not installation, and it must not survive the run.

- [ ] **Step 1: Create the hostile-free namespace and its grant**

```bash
kubectl create namespace minecraft-hostport
kubectl get ns minecraft-hostport -o jsonpath='{.metadata.labels}{"\n"}'
```

No `pod-security.kubernetes.io/enforce` label at all: `restricted` and
`baseline` both forbid host ports, and this scenario is about the case where
they are allowed. The refusal under `baseline` is already measured by the E2E
and is not re-derived here.

```bash
kubectl -n minecraft-hostport apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft-hostport
stringData:
  secret: hostport-scenario-only
EOF
sed 's/namespace: minecraft/namespace: minecraft-hostport/' \
  /home/paul/git/fluxcd/spawnery/network/rbac.yaml | kubectl apply -f -
kubectl -n minecraft-hostport get rolebinding spawnery-forwarding-secret-reader \
  -o jsonpath='{.metadata.namespace}/{.subjects[0].namespace}{"\n"}'
```

Expected: `minecraft-hostport/spawnery-system`. The `sed` rewrites both
objects' own namespace and must not touch the subject's — read it back, because
`sed` exits 0 whether or not it matched, and this repository has shipped that
mistake three times.

- [ ] **Step 2: Create a HostPort ProxyGroup**

```bash
kubectl apply -f - <<'EOF'
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: hostport
  namespace: minecraft-hostport
spec:
  forwardingSecretRef:
    name: velocity-forwarding-secret
  defaults:
    minecraftVersion: "26.2"
---
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: minecraft-hostport
spec:
  networkRef:
    name: hostport
  type: Ephemeral
  image: ghcr.io/spawnery/paper:26.2-0.2.0
  maxPlayers: 20
  scaling:
    minReplicas: 1
    maxReplicas: 1
    spareSlots: 10
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft-hostport
spec:
  networkRef:
    name: hostport
  replicas: 1
  image: ghcr.io/spawnery/velocity:3.5.1-0.2.0
  expose:
    type: HostPort
    hostPort:
      port: 25566
  routing:
    fallbackGroups:
      - lobby
EOF
```

- [ ] **Step 3: Verify the pod runs and the port answers on its node**

```bash
kubectl -n minecraft-hostport wait --for=condition=Ready pod -l spawnery.cloud/role=proxy --timeout=10m
kubectl -n minecraft-hostport get pods -o wide
node="$(kubectl -n minecraft-hostport get pods -l spawnery.cloud/role=proxy -o jsonpath='{.items[0].spec.nodeName}')"
ip="$(kubectl get node "$node" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
echo "$node $ip"
timeout 5 bash -c "cat < /dev/null > /dev/tcp/$ip/25566" && echo "open" || echo "closed"
kubectl -n minecraft-hostport get proxygroup gateway -o jsonpath='{.status}{"\n"}' | python3 -m json.tool
```

Expected: the pod `Ready`, and `open` on the node's own address. The TCP probe
is the measurement — a pod that is `Ready` with a `hostPort` field proves the
API server accepted the field, not that the CNI bound the port.

- [ ] **Step 4: Tear the namespace down and confirm it is gone**

```bash
kubectl delete namespace minecraft-hostport --wait=true
kubectl get ns minecraft-hostport 2>&1 | tail -1
```

Expected: `Error from server (NotFound)`.

- [ ] **Step 5: Write scenario 5 into the runbook and commit**

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): scenario 5 — HostPort binds under Cilium's portmap chaining

Measured with a TCP connect to the node's own address, not by observing that
the pod is Ready: a Ready pod carrying a hostPort field proves the API server
accepted the field, not that the CNI bound the port. The refusal under Pod
Security baseline is already measured by the E2E and is not re-derived here.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 9: Node drain and the PodDisruptionBudget

**Files:**
- Modify: `/home/paul/git/spawnery/docs/runbook-milestone-6-rollout.md`

- [ ] **Step 1: Record the starting state**

```bash
kubectl -n minecraft get pods -o wide
kubectl -n minecraft get pdb
kubectl -n minecraft get pdb -o jsonpath='{range .items[*]}{.metadata.name} {.status.currentHealthy}/{.status.desiredHealthy} {.spec.selector.matchLabels}{"\n"}{end}'
```

Note which node carries which proxy. The budget's selector is worth reading:
4c-3's closing review found a live budget whose selector carried no role term,
so ready occupied proxies inflated `currentHealthy` against a `desiredHealthy`
counted only from occupied servers.

- [ ] **Step 2: Cordon and drain the node carrying a proxy**

```bash
node="$(kubectl -n minecraft get pods -l spawnery.cloud/role=proxy -o jsonpath='{.items[0].spec.nodeName}')"
echo "$node"
kubectl cordon "$node"
kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data --timeout=10m
```

This evicts every workload on that node, not only spawnery's. That is what a
drain is, and it is why this runs once with the owner present. Watch the other
namespaces while it runs:

```bash
kubectl get pods -A -o wide | grep -v Running | head -40
```

- [ ] **Step 3: Record what the drain did to the group**

```bash
kubectl -n minecraft get pods -o wide
kubectl -n minecraft get proxygroup gateway -o jsonpath='{.status}{"\n"}' | python3 -m json.tool
kubectl -n minecraft get pdb -o wide
kubectl -n minecraft get events --sort-by=.lastTimestamp | tail -30
```

Expected: the proxy on the drained node replaced on another, the group's ready
count recovering, and the budget never having permitted an eviction that took
it below `desiredHealthy`.

- [ ] **Step 4: Uncordon and confirm the cluster is whole**

```bash
kubectl uncordon "$node"
kubectl get nodes
kubectl get pods -A -o wide | grep -v Running | head -40
```

Expected: three `Ready` nodes with no `SchedulingDisabled`, and nothing left
pending. Do not leave this task with a cordoned node.

- [ ] **Step 5: Write scenario 6 into the runbook and commit**

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): scenario 6 — a real eviction against the PodDisruptionBudget

4c-1's drain and 4c-3's budget under the eviction API, which a single-node
kind cluster cannot approach. The budget's selector is recorded verbatim:
4c-3's closing review found a live budget whose selector carried no role term.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 10: The three RBAC gaps

**Files:**
- Modify: `/home/paul/git/spawnery/docs/runbook-milestone-6-rollout.md`

`docs/handover-milestone-6.md` §6 names three, each with a different reason
for being open.

- [ ] **Step 1: `tokenreviews: create`, reasoned but never measured**

§6 records this as reasoned rather than measured, because no agent process
ever ran in the harness. One runs now.

```bash
kubectl -n spawnery-system logs deployment/spawnery-operator --tail=400 \
  | grep -i -E 'tokenreview|authenticat' | head -20
kubectl -n minecraft get pods -l spawnery.cloud/role=server \
  -o jsonpath='{range .items[*]}{.metadata.name} {.status.phase}{"\n"}{end}'
```

A server pod reaching `Ready` means its agent authenticated, which means the
`TokenReview` succeeded. Record both the log lines and the reasoning that
connects them — and say which half is measurement and which is inference.

- [ ] **Step 2: `persistentvolumeclaims: patch`, measured absent**

Nothing in the harness grows a claim. Longhorn is here, so drive it:

```bash
kubectl apply -f - <<'EOF'
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: survival
  namespace: minecraft
spec:
  networkRef:
    name: production
  type: Persistent
  image: ghcr.io/spawnery/paper:26.2-0.2.0
  maxPlayers: 20
  replicas: 1
  drain:
    timeoutSeconds: 60
  storage:
    size: 1Gi
EOF
sleep 90
kubectl -n minecraft get pvc
kubectl -n minecraft get pvc -o jsonpath='{range .items[*]}{.metadata.name} {.status.phase} {.spec.resources.requests.storage}{"\n"}{end}'
```

Then grow it, which is the patch:

```bash
kubectl -n minecraft patch servergroup survival --type=merge \
  -p '{"spec":{"storage":{"size":"2Gi"}}}'
sleep 60
kubectl -n minecraft get pvc -o jsonpath='{range .items[*]}{.metadata.name} {.spec.resources.requests.storage} {.status.capacity.storage}{"\n"}{end}'
kubectl -n spawnery-system logs deployment/spawnery-operator --tail=200 | grep -i -E 'pvc|claim' | head
```

Expected: the claim's `spec.resources.requests.storage` reaching `2Gi`,
written by the operator. `longhorn` has `allowVolumeExpansion: true`, so the
expansion is real rather than merely requested.

Remove the group afterwards — §6's permanent scope is the lobby only:

```bash
kubectl -n minecraft delete servergroup survival
sleep 30
kubectl -n minecraft get pvc
```

The claims outliving the group is expected: they carry no owner reference by
design. Delete them explicitly and record that they needed it.

- [ ] **Step 3: `pods: patch` and the call site `required.go` names**

`internal/rbacaudit/required.go`'s `Why` for `pods: patch` names
`syncOccupiedLabel`, a call site nothing in the harness reaches, while
`ProxyGroupReconciler.syncOccupiedLabels` exercises the grant on every run.

```bash
grep -rn 'syncOccupiedLabel' /home/paul/git/spawnery/internal/ | head
kubectl -n minecraft get pods -l spawnery.cloud/role=server \
  -o custom-columns='NAME:.metadata.name,OCCUPIED:.metadata.labels.spawnery\.cloud/occupied'
kubectl -n minecraft get pods -l spawnery.cloud/role=proxy \
  -o custom-columns='NAME:.metadata.name,OCCUPIED:.metadata.labels.spawnery\.cloud/occupied'
```

Record whether the label is present on server pods, on proxy pods, or on both,
and whether the named call site exists in the source at all. If the `Why`
field names something that does not run, that is a finding for
`docs/known-issues.md` and a fix in this repository — not a paragraph here.

- [ ] **Step 4: Write scenario 7 into the runbook and commit**

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): scenario 7 — the three RBAC gaps §6 left open

tokenreviews: create, measured through a real agent rather than reasoned
about. persistentvolumeclaims: patch, driven against Longhorn by growing a
claim — §6 lists it measured-absent, and leaving it absent once a real
storage class exists would be a choice not to look. And whether the call site
required.go names for pods: patch runs at all.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 11: The widened denial measurement and the NetworkPolicy

**Files:**
- Modify: `/home/paul/git/spawnery/docs/runbook-milestone-6-rollout.md`

Both scenarios break something on purpose. Both are reversible, and both come
after the join.

- [ ] **Step 1: Revoke the forwarding-secret grant**

`theOperatorWasNeverDenied` catches a missing **write**. §6 records that reads
as a class were never measured, and that the explanation which would
generalise the one measurement taken is a hypothesis nothing established.
`readForwardingSecret` is an uncached read whose failure path is known to be
quiet: it folds a 403 into a condition message carrying no `is forbidden:`,
and nothing on that path logs.

```bash
kubectl -n minecraft delete rolebinding spawnery-forwarding-secret-reader
kubectl auth can-i get secrets \
  --as=system:serviceaccount:spawnery-system:spawnery-operator -n minecraft
kubectl -n minecraft annotate network production rollout-probe="$(date +%s)" --overwrite
sleep 90
kubectl -n minecraft get network production -o jsonpath='{.status.conditions}{"\n"}' | python3 -m json.tool
kubectl -n spawnery-system logs deployment/spawnery-operator --tail=200 | grep -i -E 'forbidden|secret' | head -20
kubectl -n spawnery-system logs deployment/spawnery-operator --tail=400 | grep -c 'is forbidden:'
```

Expected, and the point of the measurement: the `Network` reports
`ForwardingSecretResolved=Unknown/SecretReadForbidden`, and the operator's log
carries **no** `is forbidden:` line — which is what makes the denial invisible
to the check that exists. Record the exact condition message and the grep
count. A count of zero is a result, not a failure of the scenario.

- [ ] **Step 2: Restore the grant and confirm recovery**

```bash
cd /home/paul/git/fluxcd
flux reconcile kustomization spawnery-network
kubectl -n minecraft get rolebinding spawnery-forwarding-secret-reader \
  -o jsonpath='{.subjects[0].namespace}{"\n"}'
sleep 60
kubectl -n minecraft get network production -o jsonpath='{.status.conditions}{"\n"}' | python3 -m json.tool
```

Expected: the RoleBinding restored by Flux — which also demonstrates that the
GitOps loop repairs a hand-deleted object — and the condition back to
`True/SecretResolved`.

- [ ] **Step 3: Probe the NetworkPolicy**

The chart's policy has never been enforced: `kindnet` implements no
NetworkPolicy controller, which 6b measured and
`charts/spawnery/README.md` records. Cilium enforces it.

```bash
kubectl -n spawnery-system get networkpolicy -o yaml | head -60
kubectl -n default run netpol-probe --rm -i --restart=Never \
  --image=ghcr.io/nicolaka/netshoot:latest --command -- \
  timeout 8 nc -zv spawnery-operator.spawnery-system.svc 9443
```

Expected: the connection refused or timing out from a namespace the policy
does not admit. Then prove the policy is what refused it, rather than
something else:

```bash
kubectl -n minecraft run netpol-probe --rm -i --restart=Never \
  --image=ghcr.io/nicolaka/netshoot:latest --command -- \
  timeout 8 nc -zv spawnery-operator.spawnery-system.svc 9443
```

Expected: this one succeeds, because the agent channel runs over exactly that
port from exactly those pods. One probe alone proves nothing — a refusal with
no matching success could just as easily be a wrong Service name.

If `minecraft` enforces `restricted`, the probe pod needs a compliant security
context; add `--overrides` with `runAsNonRoot`, `seccompProfile` and
`capabilities: drop: [ALL]` rather than relaxing the namespace.

- [ ] **Step 4: Write scenarios 8 and 9 into the runbook and commit**

```bash
cd /home/paul/git/spawnery
git add docs/runbook-milestone-6-rollout.md
git commit -m "$(cat <<'EOF'
docs(6): scenarios 8 and 9 — the quiet denial, and a policy with teeth

§6 records that reads as a class were never measured and that the
explanation which would generalise the one measurement is a hypothesis
nothing established. Revoking an uncached read decides it. The policy probe
runs twice, from a namespace it refuses and one it admits: a refusal with no
matching success proves nothing about the policy.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## Task 12: Close the runbook and the milestone

**Files:**
- Modify: `/home/paul/git/spawnery/docs/runbook-milestone-6-rollout.md`
- Modify: `/home/paul/git/spawnery/docs/known-issues.md`
- Modify: `/home/paul/git/spawnery/README.md`

- [ ] **Step 1: Check every acceptance criterion against what was recorded**

Walk the design's §9, one at a time, and for each write `met` or `unmet` with
the scenario that decided it. Criterion 4 depends on scenario 0; if Cilium
refused the sharing, it is unmet and a `NodePort` does not stand in for it.

- [ ] **Step 2: Mark the runbook DRIVEN**

Change `**Status: IN PROGRESS**` to `**Status: DRIVEN**` and add, directly
beneath it, the date and the cluster's identity — RKE2 `v1.34.3+rke2r1`,
Cilium `v1.18.4`, three nodes — so a later reader knows what the evidence is
evidence about.

- [ ] **Step 3: Record what the run found in `docs/known-issues.md`**

Add a `## From the RKE2 rollout` section carrying anything the run
established that the code does not yet reflect: the outcome of the widened
denial measurement, the `syncOccupiedLabel` finding, and any claim the run
disproved. Each entry says what was measured and what was inferred.

- [ ] **Step 4: Update the README's milestone status**

Find the paragraph describing milestone 6 and state that the rollout has been
driven, naming the runbook. Keep the existing voice: what it does, what it
cost, and what it did not establish.

- [ ] **Step 5: Run the test suite, because the tree was touched**

```bash
cd /home/paul/git/spawnery
nix --extra-experimental-features 'nix-command flakes' develop -c make test
nix --extra-experimental-features 'nix-command flakes' develop -c make lint
```

Expected: both green. Documentation-only changes still get the suite: this
plan may have produced code fixes in earlier tasks.

- [ ] **Step 6: Commit**

```bash
git add docs/runbook-milestone-6-rollout.md docs/known-issues.md README.md
git commit -m "$(cat <<'EOF'
docs(6): the rollout is DRIVEN, and milestone 6 is closed

Every scenario of the design's §7 carries its command and its real output,
and every acceptance criterion of §9 carries the scenario that decided it —
including the ones recorded unmet. known-issues.md gains what the run
established that the code does not yet reflect.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
EOF
)"
```

---

## What this plan does not cover

- A permanent persistent group. Task 10 creates one and removes it.
- The manual secret rotation from 5c.
- Monitoring and alerting integration.
- More than one network.
- Publishing the chart as an OCI artefact. The design says why it is the right
  shape later and the wrong shape now.
