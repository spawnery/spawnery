# Runbook — milestone 6, the RKE2 rollout

**Status: IN PROGRESS**

Driven against `paulwtf` on 2026-08-20. The design is
`docs/superpowers/specs/2026-08-20-rke2-rollout-design.md`; §7 of it lists
these scenarios and §1 says what this run does not establish.

Every section carries the command and its real output. A scenario that could
not be driven says so and says why.

## Scenario 0 — the sharing key

The design's §5 took three Cilium annotation names from documentation. This
project does not let documentation stand in for measurement, and the step that
depends on them — annotating Traefik — is the only one in this rollout that can
take down services unrelated to spawnery. So the names were measured first,
with a Service that asks for the share and cannot carry traffic: no selector
matches anything, and it was deleted immediately.

```
$ kubectl -n default create -f - <<'EOF'
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
service/lbipam-probe created

$ kubectl -n default get svc lbipam-probe -o wide
NAME           TYPE           CLUSTER-IP     EXTERNAL-IP   PORT(S)           AGE   SELECTOR
lbipam-probe   LoadBalancer   10.43.245.40   <pending>     25565:30962/TCP   20s   no-such-app=lbipam-probe

$ kubectl -n default get svc lbipam-probe -o jsonpath='{.status.conditions}'
[{"lastTransitionTime":"2026-08-20T10:27:40Z",
  "message":"The IP '185.117.3.72' is already allocated to an incompatible service. Reason: different sharing key",
  "reason":"already_allocated_incompatible_service",
  "status":"False",
  "type":"cilium.io/IPAMRequestSatisfied"}]

$ kubectl -n default delete svc lbipam-probe
service "lbipam-probe" deleted
$ kubectl -n default get svc lbipam-probe
Error from server (NotFound): services "lbipam-probe" not found
```

**What this establishes.** Two of the three names are confirmed by the
message itself: Cilium resolved `lbipam.cilium.io/ips` to the exact address it
names, and it compared sharing keys — "different sharing key" is a comparison,
which means `lbipam.cilium.io/sharing-key` was read and Traefik's absent key
was the mismatch. The design's §5 premise holds.

**What it does not establish**, and this matters more than the half that
passed: `lbipam.cilium.io/sharing-cross-namespace` was never reached. Cilium
refused on the key before it could consider the namespaces, so a misspelling
in that third name would look exactly like this run. Cilium ignores unknown
annotations rather than rejecting them, so there is no cheap way to spell-check
it in isolation — and no second address to test with, because the pool is
exactly the three node IPs and Traefik holds all three. That name is measured
in scenario 0's second half, on the real pair, or not at all.

**One thing the message does settle about the risk.** The incumbent kept the
address: a newcomer with a mismatched key was refused, and Traefik's allocation
was not disturbed by the attempt. That is the direction of failure for an
arriving Service. It says nothing about the reverse — adding a sharing key to
Traefik changes *Traefik's own* request and causes its allocation to be
re-evaluated, which is the risk the second half carries and which this probe
cannot reduce.

### Second half — annotating Traefik

The known-good commit was recorded first, together with the recovery command,
so that a broken ingress would need nothing composed:

```
known-good: e8ffac20d0a4f03637bf8fb88b5b441ac9a3e4b4
recovery:   cd /home/paul/git/fluxcd && git revert --no-edit HEAD && git push \
              && flux reconcile kustomization infrastructure
```

**The baseline was measured per address, not per hostname.** All three names
resolve to all three addresses in round robin, so a single `curl` can succeed
against a working address while another is dead. `--resolve` pins each one:

```
$ for ip in 45.137.203.198 45.13.227.226 185.117.3.72; do
    curl -sS -o /dev/null -m 15 --resolve "immich.paul.wtf:443:$ip" -w "$ip -> %{http_code}\n" https://immich.paul.wtf/
  done
45.137.203.198 -> 200
45.13.227.226 -> 200
185.117.3.72 -> 200
$ curl -sS -o /dev/null --resolve "webvault.paul.wtf:443:185.117.3.72" -w '%{http_code}\n' https://webvault.paul.wtf/
200
```

A note on the host names, because it nearly produced a false alarm: the plan
named `vaultwarden.paul.wtf`, which does not exist — the service is at
`webvault.paul.wtf`. Its `curl` returned `000` before any change was made. Had
that gone unexamined, the same `000` after the change would have read as an
outage caused by it.

**A change to `values.yaml` is not a change to the Service.**
`infrastructure/traefik/kustomization.yaml` generates the values ConfigMap with
`disableNameSuffixHash: true`, so editing the file changes the ConfigMap's
*content* while its name stays `traefik-values`. Flux applied the new content
within seconds; the Service still carried only the old annotation, because
nothing had asked Helm to re-render. The upgrade happens on the HelmRelease's
own interval — one hour — or when told:

```
$ kubectl -n traefik get svc traefik -o jsonpath='{.metadata.annotations}'
{"io.cilium/lb-ipam-ips":"...","meta.helm.sh/release-name":"traefik", ...}      # no sharing key

$ flux reconcile helmrelease traefik -n traefik --force
✔ applied revision 41.2.0
```

**After the upgrade:**

```
$ kubectl -n traefik get svc traefik -o jsonpath='{.metadata.annotations}'
{"io.cilium/lb-ipam-ips":"45.137.203.198,45.13.227.226,185.117.3.72",
 "lbipam.cilium.io/sharing-cross-namespace":"minecraft",
 "lbipam.cilium.io/sharing-key":"paulwtf-node-ips",
 "meta.helm.sh/release-name":"traefik","meta.helm.sh/release-namespace":"traefik"}

$ kubectl -n traefik get svc traefik -o jsonpath='{.status.loadBalancer.ingress}'
[{"ip":"45.137.203.198","ipMode":"VIP"},{"ip":"45.13.227.226","ipMode":"VIP"},{"ip":"185.117.3.72","ipMode":"VIP"}]

$ kubectl -n traefik get svc traefik -o jsonpath='{.status.conditions}'
[{"lastTransitionTime":"2026-08-12T07:56:35Z","message":"","reason":"satisfied",
  "status":"True","type":"cilium.io/IPAMRequestSatisfied"}]

$ for ip in 45.137.203.198 45.13.227.226 185.117.3.72; do ... done
45.137.203.198 -> 200
45.13.227.226 -> 200
185.117.3.72 -> 200
```

The `lastTransitionTime` is the part worth reading closely: **2026-08-12**,
eight days before this run. The condition did not transition. Cilium did not
re-evaluate Traefik's allocation into a new state and then satisfy it again —
adding a sharing key to a Service that already holds its addresses left the
allocation untouched. That is a stronger result than the three `200`s, which
would also have been produced by a brief outage that had already healed.

`lbipam.cilium.io/sharing-cross-namespace` is still unverified at this point:
Traefik shares with nobody yet, so nothing has crossed a namespace. Scenario 4
is where it is decided.


## Scenario 1 — installation

Two Flux Kustomizations, `spawnery-operator` gating `spawnery-network` by
`dependsOn`. This section covers the first; §4 of the design says why they are
two and why neither lives under `apps/` or `infrastructure/`.

```
$ flux get kustomizations spawnery-operator
NAME              REVISION            SUSPENDED  READY  MESSAGE
spawnery-operator main@sha1:10aaa206  False      True   Applied revision: main@sha1:10aaa206

$ flux get helmreleases -n spawnery-system
NAME      REVISION  SUSPENDED  READY  MESSAGE
spawnery  0.1.0     False      True   Helm install succeeded for release spawnery-system/spawnery.v1 with chart spawnery@0.1.0

$ kubectl get crd | grep spawnery
networks.spawnery.cloud       2026-08-20T10:30:27Z
proxygroups.spawnery.cloud    2026-08-20T10:30:27Z
servergroups.spawnery.cloud   2026-08-20T10:30:27Z
servers.spawnery.cloud        2026-08-20T10:30:27Z
```

### The chart cannot carry the digest of its own tag

The install came up running **the tag**, not the digest:

```
$ kubectl -n spawnery-system get deployment spawnery-operator \
    -o jsonpath='{.spec.template.spec.containers[0].image}'
ghcr.io/spawnery/spawnery-operator:0.1.0
```

The design's §4 asserts that the chart at `v0.1.0` carries the operator's
digest, and its acceptance criterion 2 requires the operator to run "from the
digest in `charts/spawnery/values.yaml`". Both were wrong, and not by an
oversight anybody could have avoided:

```
$ git rev-parse v0.1.0 && git log --oneline -1 v0.1.0
0742b884cd4e3744de0b1bcd7bd21434ed2ab7c7
017ee94 feat(6e): CI, and the count it proved wrong on its first run

$ git show v0.1.0:charts/spawnery/values.yaml | sed -n '1,9p'
image:
  repository: ghcr.io/spawnery/spawnery-operator
  tag: "0.1.0"
  digest: ""
```

`hack/publish.sh` takes the digest from `skopeo copy`'s own `--digestfile`, so
it exists only **after** the tag has been published. The commit that writes it
back (`e7877f8`) is therefore necessarily behind the tag it describes. **No tag
can contain its own digest**, and the value in `charts/spawnery/values.yaml` is
structurally always one release behind — which makes it documentation of the
previous release rather than a pin for this one.

The fix belongs where the deployment is described, not in the chart. The
`HelmRelease` sets the value:

```yaml
  values:
    image:
      digest: sha256:e5eb7626cdf2b7ac186e844aad418fd388c5c3d6ab225d09a37c041b5b4414ca
```

which is what the master design's §8 actually asks for: a digest in the
manifest a cluster runs. The chart stays general.

### After the pin

Read at two levels, because the Deployment's spec is what was asked for and
the container status is what the kubelet did:

```
$ kubectl -n spawnery-system get deployment spawnery-operator \
    -o jsonpath='{.spec.template.spec.containers[0].image}'
ghcr.io/spawnery/spawnery-operator@sha256:e5eb7626cdf2b7ac186e844aad418fd388c5c3d6ab225d09a37c041b5b4414ca

$ kubectl -n spawnery-system get pod \
    -o jsonpath='{.items[0].status.containerStatuses[0].imageID}'
ghcr.io/spawnery/spawnery-operator@sha256:e5eb7626cdf2b7ac186e844aad418fd388c5c3d6ab225d09a37c041b5b4414ca

$ kubectl -n spawnery-system get pods -o wide
NAME                                 READY  STATUS   RESTARTS  AGE  IP           NODE
spawnery-operator-6cbf4b8457-sztmp   1/1    Running  0         23s  10.42.1.153  server02
```

No pull secret anywhere in the chain — the images were made public earlier the
same day, and this is the cluster-side confirmation of it:

```
$ kubectl -n spawnery-system get deployment spawnery-operator -o jsonpath='{.spec.template.spec.imagePullSecrets}'
$ kubectl -n spawnery-system get sa spawnery-operator -o jsonpath='{.imagePullSecrets}'
```

Both empty.

### Pod Security

```
$ kubectl get ns spawnery-system -o jsonpath='{.metadata.labels}'
{"goldilocks.fairwinds.com/enabled":"true","kubernetes.io/metadata.name":"spawnery-system",
 "kustomize.toolkit.fluxcd.io/name":"spawnery-operator",
 "kustomize.toolkit.fluxcd.io/namespace":"flux-system",
 "pod-security.kubernetes.io/enforce":"restricted",
 "pod-security.kubernetes.io/warn":"restricted"}

$ kubectl -n spawnery-system get pod -o jsonpath='{.items[0].spec.securityContext}'
{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}
$ kubectl -n spawnery-system get pod -o jsonpath='{.items[0].spec.containers[0].securityContext}'
{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}
```

The namespace enforces `restricted` and the pod runs, which is admission and
execution rather than admission alone. This is the first half of §6's "CIS
`restricted` against the operator's own security context"; the game server
namespace is scenario 2.

The NetworkPolicy from 6b is installed and selects the operator by
`app.kubernetes.io/component=operator,app.kubernetes.io/name=spawnery`. Whether
it is *enforced* is scenario 9 — it has never been, under any CNI this project
has run on.

### The network namespace, its secret and the scoped grant

`spawnery-network` is gated on `spawnery-operator` by `dependsOn`, so the CRDs
exist before anything references them. It landed without the `Network` on
purpose: a `SecretReadForbidden` produced by a grant that was never applied
measures nothing.

```
$ flux get kustomizations spawnery-network
NAME             REVISION            SUSPENDED  READY  MESSAGE
spawnery-network main@sha1:e8ffac20  False      True   Applied revision: main@sha1:e8ffac20

$ kubectl -n minecraft get externalsecret velocity-forwarding-secret
NAME                        STORETYPE           STORE        REFRESH INTERVAL  STATUS        READY  LAST SYNC
velocity-forwarding-secret  ClusterSecretStore  vault-store  1h0m0s            SecretSynced  True   7s

$ kubectl -n minecraft get secret velocity-forwarding-secret -o jsonpath='{.data.secret}' | wc -c
60
```

The byte count rather than the value: `readForwardingSecret` rejects an empty
`secret` key with `SecretKeyMissing`, so what has to be established is that the
key exists and is non-empty. The value is a credential and stays in Vault.

The grant is read back rather than assumed, because `kubectl apply` accepts a
RoleBinding whose subject names a namespace that does not exist — it applies
cleanly and grants nothing. This is the same check `hack/e2e.sh`'s
`check_forwarding_secret_reader_subject` performs, for the same reason:

```
$ kubectl -n minecraft get rolebinding spawnery-forwarding-secret-reader \
    -o jsonpath='{.subjects[0].kind}/{.subjects[0].name}/{.subjects[0].namespace}'
ServiceAccount/spawnery-operator/spawnery-system

$ kubectl -n default get role spawnery-forwarding-secret-reader
Error from server (NotFound): roles.rbac.authorization.k8s.io "spawnery-forwarding-secret-reader" not found
```

The second command is the one that matters. `config/rbac/forwarding-secret-reader.yaml`
carries no `metadata.namespace` — by design, so that `kubectl apply -n` supplies
it — and Flux supplies nothing, so an unedited copy would have put both objects
in `default`. Proving they are *not* there is the only way to know the copy in
`spawnery/network/rbac.yaml` writes its namespace out.

And the grant's width, measured with an access review rather than read off the
Role:

```
$ kubectl auth can-i get secrets --as=system:serviceaccount:spawnery-system:spawnery-operator -n minecraft
yes
$ kubectl auth can-i get secrets --as=system:serviceaccount:spawnery-system:spawnery-operator -n default
no
```

`no` is the informative half: the ClusterRole grants no access to secrets
outside the operator's own namespace, and an administrator opens exactly the
namespaces that hold a `Network`. This is the first time that claim has been
checked against a real API server's authorizer rather than against the YAML.


## Scenario 2 — `restricted` against a game server namespace

## Scenario 3 — a real join

## Scenario 4 — the LoadBalancer address

## Scenario 5 — HostPort under the real CNI

## Scenario 6 — node drain and the PodDisruptionBudget

## Scenario 7 — the three RBAC gaps

## Scenario 8 — the widened denial measurement

## Scenario 9 — the NetworkPolicy, enforced for the first time
