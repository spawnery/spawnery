# Runbook — milestone 6, the RKE2 rollout

**Status: DRIVEN**

Driven against `paulwtf` on 2026-08-20 — three nodes, all `control-plane,etcd`,
RKE2 `v1.34.3+rke2r1`, Cilium `v1.18.4` with `cni-chaining-mode: portmap` and
`kube-proxy-replacement: false`, Longhorn as the default StorageClass, Flux and
Traefik in front. Spawnery `v0.1.0`, operator image
`sha256:e5eb7626cdf2b7ac186e844aad418fd388c5c3d6ab225d09a37c041b5b4414ca`. The design is
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

The `minecraft` namespace enforces `restricted`, and the pods do not merely
pass admission — they run:

```
$ kubectl -n minecraft get pods -o wide
NAME           READY  STATUS   RESTARTS  AGE  IP            NODE
gateway-dnhp   1/1    Running  0         18m  10.42.2.131   server03
gateway-q7qw   1/1    Running  0         18m  10.42.0.65    server01
lobby-3dxq     1/1    Running  0         18m  10.42.1.149   server02

$ kubectl -n minecraft get servergroup lobby -o wide
NAME   TYPE       PHASE  READY  PLAYERS  FREE SLOTS  AGE
lobby  Ephemeral  Ready  1      0        100         18m

$ kubectl -n minecraft get proxygroup gateway -o wide
NAME     PHASE  READY  ADDRESS               PLAYERS  AGE
gateway  Ready  2      45.137.203.199:25565  0        18m

$ kubectl -n minecraft get network production -o jsonpath='{.status.conditions}'
Accepted                  True   "this network owns its namespace"
ForwardingSecretResolved  True   secret "velocity-forwarding-secret" carries a "secret" key
```

**This is the first time real Paper and Velocity images have run in any
cluster.** `test/e2e/manifests/e2e.yaml` names unresolvable tags on purpose, so
every earlier run stopped at `ErrImagePull` by design. Three things follow from
`READY 1` and `FREE SLOTS 100` that no previous run could establish:

- Paper starts under Pod Security `restricted` — `runAsNonRoot`,
  `readOnlyRootFilesystem`, `capabilities: drop: [ALL]` and all — rather than
  merely being admitted under it;
- the agent inside the pod reached the operator on 9443 and reported its slots,
  which is what moves the group to `Ready`;
- and therefore **the NetworkPolicy from 6b, enforced here for the first time
  ever, does not block the agent channel.** Under `kindnet` it was inert. Had
  its selector been wrong, this line would read `READY 0` and the group would
  never have left `Pending`. Scenario 9 measures the other direction — that it
  refuses what it should.

The proxies reached `Ready` too, which needs the Velocity agent to bind its
readiness port: `PHASE Ready, READY 2`.


## Scenario 3 — a real join

**Not driven.** TCP 25565 does not reach this cluster from outside, and the
cause is not in spawnery — see scenario 4. A join could not be attempted
without first changing something in front of the cluster, which is outside
this rollout's scope and the repository owner's to decide.

What that leaves unmeasured, stated plainly rather than inferred from the
green statuses above: nothing here shows a player being routed through the
proxy to a lobby server, and nothing here exercises `syncOccupiedLabels` under
a real occupancy — scenario 7 records what could be established about it
without a join.


## Scenario 4 — the LoadBalancer address

**Assigned, and reported by the operator. Not proven reachable from outside.**

The sharing plan of §5 was abandoned on evidence, not on preference. With both
Services on `externalTrafficPolicy: Local`, Cilium refused:

```
$ kubectl -n minecraft get svc gateway -o jsonpath='{.status.conditions}'
[{"message":"The IP '185.117.3.72' is already allocated to an incompatible service.
   Reason: compatible ExternalTrafficPolicy local but selecting different set of pods",
  "reason":"already_allocated_incompatible_service","status":"False",
  "type":"cilium.io/IPAMRequestSatisfied"}]
```

That is a property of `Local`, not a misconfiguration: the load balancer may
only steer to nodes holding a local endpoint, and Traefik's DaemonSet across
three nodes is a different set of nodes from two Velocity pods on two. **The
design's §5 premise was incomplete** — non-overlapping ports are necessary for
sharing but not sufficient. On this cluster the choice was between real player
addresses and a shared address; a fourth address settled it.

`45.137.203.199` was added to the `node-ips` pool. It is routed to `server01`
rather than owned by a node, which the three original addresses are not:

```
$ kubectl get ciliumloadbalancerippools node-ips -o wide
NAME      DISABLED  CONFLICTING  IPS AVAILABLE  AGE
node-ips  false     False        1              143d

$ kubectl -n minecraft get svc gateway -o wide
NAME     TYPE          CLUSTER-IP     EXTERNAL-IP     PORT(S)          AGE
gateway  LoadBalancer  10.43.101.155  45.137.203.199  25565:32458/TCP  18m

$ kubectl -n minecraft get svc gateway -o jsonpath='{.status.conditions}'
[{"lastTransitionTime":"2026-08-20T11:06:09Z","message":"","reason":"satisfied",
  "status":"True","type":"cilium.io/IPAMRequestSatisfied"}]

$ kubectl -n minecraft get proxygroup gateway -o jsonpath='{.status.address}'
45.137.203.199:25565
```

The operator's read-back of a load balancer address is therefore driven for
the first time against a real controller. Until now the E2E wrote
`status.loadBalancer` by hand, and its own manifest says so.

DNS is correct, and the `cloudflare-proxied` annotation is load-bearing:

```
$ dig +short mc.paul.wtf @1.1.1.1
45.137.203.199
```

A Cloudflare-proxied record would answer with a Cloudflare address, and
Cloudflare forwards no TCP on 25565 without Spectrum — the name would resolve
and nothing would connect, with no error on any object. It answers with the
address itself, so external-dns honoured
`external-dns.alpha.kubernetes.io/cloudflare-proxied: "false"` over its own
global `--cloudflare-proxied` default.

**What is not established.** TCP 25565 does not answer from outside:

```
$ timeout 6 bash -c 'cat < /dev/null > /dev/tcp/45.137.203.199/25565'   # closed
$ timeout 6 bash -c 'cat < /dev/null > /dev/tcp/45.137.203.198/32458'   # closed
$ curl -o /dev/null -w '%{http_code}' --resolve immich.paul.wtf:443:45.137.203.198 \
    https://immich.paul.wtf/                                             # 200
```

The second line is the one that locates the cause. `45.137.203.198` is an
ordinary node address that pings, serves HTTPS, and has done so throughout this
run — and *its* NodePort is closed too. Whatever refuses 25565 refuses 32458 on
a known-good address as well, while 443 passes. That points at port filtering
in front of the cluster and not at the new address, at Cilium's IPAM, or at
anything spawnery configures. It was not pursued further: opening a port on the
path into this cluster is the owner's decision and outside this rollout.

So §6's "a `LoadBalancer` address a client can reach" is **half met**: the
address exists, is unique to this network, is reported by the operator and
resolves under a name. That a client reaches it is untested.

### Addendum — what was actually closing the port, in two parts

The filtering was found, and it was where the cluster's own repository said it
would be. `infrastructure/cilium/host-firewall-policy.yaml` is a
`CiliumClusterwideNetworkPolicy` over the host endpoints, and its `world` rule
admitted exactly ICMP echo, 6443, 80, 443, 25, 465, 587, 143, 993 and 5432.
Every observation above follows from that list: `ping` to a node answered
because type 8 is explicitly allowed, 443 served all day, and 25565 and the
NodePort 32458 were dropped without a trace. `25565/TCP` was added to that
rule and the policy object carries it:

```
$ kubectl get ciliumclusterwidenetworkpolicy host-firewall-ingress \
    -o jsonpath='{.spec.ingress[3].toPorts[0].ports}'
['6443','80','443','25','465','587','143','993','5432','25565']
```

**And the port stayed closed**, which turned out to be a second, independent
cause — the first one had been hiding it.

```
$ ip -4 addr show          # on server01, inside its cilium agent
inet 45.137.203.198/28 brd 45.137.203.207 scope global eth0
```

A **/28**. The subnet is `45.137.203.192/28`, so `45.137.203.199` is not routed
*to* server01 — it is **on-link on the same segment**, and the upstream router
resolves it by ARP. Nothing answers: the address is configured on no interface,
Cilium's `enable-l2-announcements` is unset, and there is no
`CiliumL2AnnouncementPolicy`.

The cluster side is complete, which is what makes the diagnosis certain rather
than likely. From a pod, the same address on the same port answers:

```
$ nc -zv -w6 45.137.203.199 25565
Connection to 45.137.203.199 25565 port [tcp/*] succeeded!
$ nc -zv -w6 gateway.minecraft.svc.cluster.local 25565
Connection to ... (10.43.101.155) 25565 port [tcp/*] succeeded!
```

kube-proxy has programmed the LoadBalancer address, the endpoints are healthy,
and Velocity is listening. Packets from outside simply never arrive.

**What would fix it**, in the order of least surprise:

1. Configure `45.137.203.199/28` on `server01`'s `eth0`. The kernel then answers
   ARP and kube-proxy's existing DNAT rule does the rest. One line, and a
   machine-level change outside this cluster's GitOps.
2. Enable Cilium L2 announcements with a `CiliumL2AnnouncementPolicy`, which is
   the mechanism designed for exactly this — and a larger change to a
   Cilium configuration RKE2 manages.

Option 1 carries a coupling worth writing down: under
`externalTrafficPolicy: Local`, an address answered by `server01` serves traffic
only while a proxy pod runs **on server01**. Two replicas across three nodes do
not guarantee that. Three replicas, a `nodeSelector`, or an affinity rule would;
so would `Cluster`, at the cost of the player addresses the policy exists to
preserve.


## Scenario 5 — HostPort under the real CNI

A temporary namespace with **no** `pod-security.kubernetes.io/enforce` label at
all — `restricted` and `baseline` both forbid host ports, and this scenario is
about the case where they are allowed. The refusal under `baseline` is already
measured by the E2E and is not re-derived here.

The per-namespace grant was copied with `sed` and then read back, because
`sed` exits 0 whether or not it matched and this repository has shipped that
mistake three times:

```
$ sed 's/namespace: minecraft$/namespace: minecraft-hostport/' \
    /home/paul/git/fluxcd/spawnery/network/rbac.yaml | kubectl apply -f -
role.rbac.authorization.k8s.io/spawnery-forwarding-secret-reader created
rolebinding.rbac.authorization.k8s.io/spawnery-forwarding-secret-reader created

$ kubectl -n minecraft-hostport get rolebinding spawnery-forwarding-secret-reader \
    -o jsonpath='eigener-ns={.metadata.namespace} subject-ns={.subjects[0].namespace}'
eigener-ns=minecraft-hostport subject-ns=spawnery-system
```

The `$` anchor is what keeps the subject's `namespace: spawnery-system` out of
the substitution; the read-back is what proves it did.

**The pod, and no Service:**

```
$ kubectl -n minecraft-hostport get pod -l spawnery.cloud/role=proxy \
    -o jsonpath='{.items[0].spec.containers[0].ports}'
[{"containerPort":25565,"hostPort":25566,"name":"minecraft","protocol":"TCP"},
 {"containerPort":8081,"name":"ready","protocol":"TCP"}]

$ kubectl -n minecraft-hostport get svc
No resources found in minecraft-hostport namespace.

$ kubectl -n minecraft-hostport get proxygroup gateway -o wide
NAME     PHASE  READY  ADDRESS               PLAYERS  AGE
gateway  Ready  1      45.137.203.198:25566  0        21s
```

`status.address` naming the node rather than a Service is milestone 6c's
`proxyAddress` running against a real node for the first time.

**The measurement, with a control.** A `Ready` pod carrying a `hostPort` field
proves the API server accepted the field, not that the CNI bound the port, so
the port is connected to — from inside the cluster, against the node's own
address, because TCP into this cluster from outside is filtered (scenario 4)
and that would have measured the filter rather than the CNI:

```
$ nc -zv -w5 45.137.203.198 25566
Connection to 45.137.203.198 25566 port [tcp/*] succeeded!     # exit 0
$ nc -zv -w5 45.137.203.198 25567
nc: connect to 45.137.203.198 port 25567 (tcp) failed: Connection refused   # exit 1
```

The second line is the control and it is the reason this counts. 25567 has
nothing bound to it; `Connection refused` means the packets reached the host's
stack and were rejected by it, so the success on 25566 is a port that is
actually bound and not an artefact of something upstream accepting everything.

**HostPort works on Cilium `v1.18.4` with `cni-chaining-mode: portmap` and
`kube-proxy-replacement: false`.** That is a claim about this cluster on this
day; §1 says why it is not a claim about any other CNI.

Torn down, and the teardown read back:

```
$ kubectl delete namespace minecraft-hostport --wait=true
namespace "minecraft-hostport" deleted
$ kubectl get ns minecraft-hostport
Error from server (NotFound): namespaces "minecraft-hostport" not found
```

The production network was untouched throughout — same three pods, same ages.


## Scenario 6 — node drain and the PodDisruptionBudget

**The node drain was not performed.** Two facts, both measured before the fact
rather than argued after it, made it the wrong trade:

```
$ kubectl get pods -A --field-selector spec.nodeName=server03 --no-headers | wc -l
67
```

Sixty-seven pods, among them `hashicorp-vault-0` — the source every
`ExternalSecret` on this cluster reads from, including spawnery's own — and the
CloudNativePG primaries `immich-cluster-1` and `authentik-cluster-1`. A drain
moves all of it, with the Longhorn volumes underneath.

And what the drain could have measured is smaller than it looks:

```
$ kubectl -n minecraft get pdb
NAME               MIN  ALLOWED  CURRENT  DESIRED  SELECTOR
gateway-proxy-pdb  0    0        0        0        ... spawnery.cloud/occupied:true ...
lobby-server-pdb   0    0        0        0        ... spawnery.cloud/occupied:true ...
```

Both budgets select on `occupied: true`, no pod carries it without players, and
`desiredHealthy` is therefore **0**. The budget would not have refused
anything — correctly, and by its own selector's design. Without the join, §6's
"PodDisruptionBudget under a real eviction" cannot be measured at all, and what
remains — a pod evicted and replaced elsewhere — is what 4c-2's rolling updates
already exercise.

**What was driven instead: the eviction API itself, against one proxy pod.**
It touches nothing outside `minecraft` and exercises the same spawnery path:

```
$ kubectl -n minecraft create --raw "/api/v1/namespaces/minecraft/pods/gateway-dnhp/eviction" -f - <<< \
    '{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"gateway-dnhp","namespace":"minecraft"}}'
{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Success","code":201}

$ kubectl -n minecraft get pods -o wide      # 15 seconds later
gateway-3k7f   1/1  Running  0  15s  10.42.2.159  server03
gateway-q7qw   1/1  Running  0  32m  10.42.0.65   server01
lobby-3dxq     1/1  Running  0  32m  10.42.1.149  server02

$ kubectl -n minecraft get proxygroup gateway -o wide
gateway  Ready  2  45.137.203.199:25565  0  32m
```

The eviction was **allowed**, which is the correct answer at `desiredHealthy 0`,
and the operator had a replacement running on the same node within fifteen
seconds.

**An attempt at the protective direction, and why it failed.** Labelling both
proxy pods `spawnery.cloud/occupied=true` by hand moved `currentHealthy` to 2
but left `minAvailable` and `desiredHealthy` at 0, so a second eviction was
allowed as well:

```
$ kubectl -n minecraft label pods -l spawnery.cloud/role=proxy spawnery.cloud/occupied=true --overwrite
$ kubectl -n minecraft get pdb gateway-proxy-pdb
MIN  ALLOWED  CURRENT  DESIRED
0    2        2        0
```

The operator sizes `minAvailable` from **its own** occupancy tally, not from
the labels on the pods, so a hand-set label cannot make the budget protective —
it only changes who the budget counts as healthy. That is a sound design
(the label follows the tally, not the reverse) and it means **the budget's
protective behaviour cannot be simulated. Only real players can drive it**, so
§6's item stays open, blocked by the same port filtering as scenario 3.

The hand-set labels did produce something, though: the operator removed them
within five seconds, which is the measurement scenario 7 needed for
`pods: patch`.


## Scenario 7 — the three RBAC gaps

`docs/handover-milestone-6.md` §6 names three, each open for a different
reason. Two are now settled and one is not, and the difference matters.

### `tokenreviews: create` — reasoned in §6, still reasoned here

```
$ kubectl -n minecraft get servergroup lobby -o wide
NAME   TYPE       PHASE  READY  PLAYERS  FREE SLOTS  AGE
lobby  Ephemeral  Ready  1      0        100         28m
```

`FREE SLOTS 100` is reported by the agent inside the Paper pod over the agent
channel, and that channel authenticates the pod's projected token with a
`TokenReview` (`podspec.AgentTokenAudience`). A failed review means no `Hello`,
no slots, and a group that never leaves `Pending`. So the review succeeded.

**That is an inference, not a measurement, and it is the same inference §6
called reasoning.** What is new is only that a real agent now exists to reason
about: the client metrics carry no per-resource label, and nothing in the
operator's log names a `TokenReview`. A direct measurement would need either
API-server audit logs or a deliberate revocation, and the second belongs to
scenario 8's method rather than this one.

### `persistentvolumeclaims: patch` — measured, and it was absent before

§6 lists this as measured-absent: nothing in the harness grows a claim. The
operator's own client metrics settle it, because they count HTTP verbs.

**Before**, after 40 minutes of ordinary reconciliation with a lobby and two
proxies running — no `PATCH` line exists at all:

```
rest_client_requests_total{code="200",method="DELETE"} 1
rest_client_requests_total{code="200",method="GET"}    397
rest_client_requests_total{code="200",method="PUT"}    3316
rest_client_requests_total{code="201",method="POST"}   49
```

A persistent group was then created on Longhorn and grown:

```
$ kubectl -n minecraft get pvc
NAME             PHASE  REQ  CAP  SC        OWNER
survival-0-data  Bound  1Gi  1Gi  longhorn  <none>

$ kubectl -n minecraft patch servergroup survival --type=merge \
    -p '{"spec":{"storage":{"size":"2Gi"}}}'
servergroup.spawnery.cloud/survival patched

$ kubectl -n minecraft get pvc survival-0-data -o jsonpath='{.status.conditions}'
[{"lastTransitionTime":"2026-08-20T11:15:08Z","status":"True","type":"Resizing"}]
```

**After** — exactly one `PATCH`, and it succeeded:

```
rest_client_requests_total{code="200",method="PATCH"} 1
```

and the claim reached its new size:

```
$ kubectl -n minecraft get pvc survival-0-data
survival-0-data  Bound  2Gi  2Gi
```

The claim carried **no owner reference**, as designed, so it outlived its
group and had to be removed by hand:

```
$ kubectl -n minecraft delete servergroup survival
$ kubectl -n minecraft get pvc
survival-0-data  Bound  ...  2Gi  longhorn      # still there
$ kubectl -n minecraft delete pvc survival-0-data
```

### `pods: patch` — the call site exists, and it was *not* exercised

§6 asks whether `syncOccupiedLabel`, the call site `required.go` names for
`pods: patch`, works at all. Half of that is answered by reading:

```
$ grep -rn 'syncOccupiedLabel' internal/ --include=*.go | grep -v _test
internal/rbacaudit/required.go:69:    ... Why: "syncOccupiedLabel patches the occupied label"
internal/controller/server_controller.go:846:  if err := r.syncOccupiedLabel(ctx, srv, pod, snap); err != nil {
internal/controller/server_controller.go:900:  func (r *ServerReconciler) syncOccupiedLabel(
```

The name is real and the Server controller calls it, so `required.go`'s `Why`
does not name a function that does not exist. But the label is absent on every
pod:

```
$ kubectl -n minecraft get pods -l spawnery.cloud/role=server \
    -o custom-columns='NAME:...,OCCUPIED:.metadata.labels.spawnery\.cloud/occupied'
lobby-3dxq   <none>
```

and "absent" is indistinguishable from "never written" by inspection alone.
At this point in the run the metrics agreed with the pessimistic reading: the
only `PATCH` the operator had ever issued was the claim expansion above.

**Scenario 6 then settled it**, by provoking the write instead of waiting for a
player. `syncOccupiedLabel` reconciles the label *to* reality in both
directions, so setting it by hand on a pod that carries no players makes the
controller remove it — and removal is the same `r.Patch(...)` call
(`server_controller.go:923`) that adding it would be. The label went away in
under five seconds and the counter moved:

```
PATCH 1  →  3   after hand-labelling the two proxy pods    (syncOccupiedLabels, plural)
PATCH 3  →  4   after hand-labelling the lobby server pod  (syncOccupiedLabel,  singular)
```

The split matters, and it is exactly the distinction `docs/known-issues.md`
draws: `required.go`'s `Why` names the **singular** — the Server controller's
`syncOccupiedLabel` — while `ProxyGroupReconciler.syncOccupiedLabels` is what
runs on every ordinary pass. The first two patches came from the plural, the
third from the singular. **So the call site `required.go` names does run, and
it does issue the patch the grant is for.**

What this is not: a measurement of the label under real occupancy. The write
was provoked by a hand-set label, not by a player joining, and only the
removal path was observed — the addition path still has nothing that drives it.


## Scenario 8 — the widened denial measurement

§6 is careful about what milestone 6a established and what it did not:
`theOperatorWasNeverDenied` caught a revoked **write** immediately, two
cache-backed **lists** produced nothing at all over eight minutes, and *reads
as a class were never measured* — the explanation that would generalise the
list result is called "a hypothesis nothing established".

`readForwardingSecret` is the right instrument: an uncached read
(`internal/controller/forwardingsecret.go:57-59` says why it must be uncached),
on a path known to fold a 403 into a condition message carrying no
`is forbidden:` substring, with nothing on that path logging.

**Before**, and the log count is over the operator's whole lifetime:

```
$ kubectl -n minecraft get network production \
    -o jsonpath='{.status.conditions[?(@.type=="ForwardingSecretResolved")]}'
{"reason":"SecretResolved","status":"True",
 "message":"secret \"velocity-forwarding-secret\" carries a \"secret\" key"}

$ kubectl -n spawnery-system logs deployment/spawnery-operator --tail=3000 | grep -c 'is forbidden'
24
```

Those 24 are scenario 5's teardown — `unable to create new content in namespace
minecraft-hostport because it is being terminated` — and they are worth keeping
in view, because they are the **loud** kind: every one produced a log line
carrying `is forbidden:`, which is exactly what a grep-based denial check
detects.

**The revocation:**

```
$ kubectl -n minecraft delete rolebinding spawnery-forwarding-secret-reader
$ kubectl auth can-i get secrets --as=system:serviceaccount:spawnery-system:spawnery-operator -n minecraft
no
$ kubectl -n minecraft annotate network production rollout-probe=... --overwrite
```

**After 90 seconds:**

```
$ kubectl -n minecraft get network production \
    -o jsonpath='{.status.conditions[?(@.type=="ForwardingSecretResolved")]}'
{"reason":"SecretReadForbidden","status":"Unknown",
 "message":"the operator may not read secret \"velocity-forwarding-secret\" in namespace
            \"minecraft\"; grant it with kubectl apply -n minecraft -f
            config/rbac/forwarding-secret-reader.yaml"}

$ kubectl -n spawnery-system logs deployment/spawnery-operator --tail=3000 | grep -c 'is forbidden'
24                                    # unchanged

$ kubectl -n spawnery-system logs deployment/spawnery-operator --tail=300 \
    | grep -iE 'secret|forbidden' | grep -v 'being terminated'
                                      # nothing
```

**Twenty denied reads, and not one line of log.** The metric is where they are:

```
rest_client_requests_total{code="403",host="10.43.0.1:443",method="GET"}   20
rest_client_requests_total{code="403",host="10.43.0.1:443",method="POST"}  24     # scenario 5, unchanged
```

### What this establishes

**A denied uncached read is silent in the operator's log.** This is the
measurement §6 asked for and did not have. A check that greps the log for
`is forbidden:` — which is what `theOperatorWasNeverDenied` does — cannot see
this denial, and the reason is not the manager's cache: the read reached the
API server, was refused, and the refusal was handled rather than surfaced.

It does **not** establish the wider claim that reads as a class are invisible.
This is one read on one path whose quietness was already known by inspection;
what was missing was the measurement, and what it adds is that the quietness is
real at runtime and not merely apparent in the source.

**And it establishes where such a denial *is* visible**, which §6 did not ask
for and is the more useful half: `rest_client_requests_total` counts it. The
counter has no per-resource label, so it cannot say *what* was denied — but
`code="403"` on a client that should never be denied anything is a signal that
needs no code change to obtain, and it caught this one where the log did not.

### Recovery

Flux restored the RoleBinding, which also demonstrates that the GitOps loop
repairs a hand-deleted object:

```
$ flux reconcile kustomization spawnery-network
$ kubectl -n minecraft get rolebinding spawnery-forwarding-secret-reader \
    -o jsonpath='{.subjects[0].namespace}'
spawnery-system
$ kubectl auth can-i get secrets --as=system:serviceaccount:spawnery-system:spawnery-operator -n minecraft
yes
$ kubectl -n minecraft get network production \
    -o jsonpath='{.status.conditions[?(@.type=="ForwardingSecretResolved")]}'
{"reason":"SecretResolved","status":"True", ...}
```

No pod restarted and no server was lost while the network was in
`SecretReadForbidden`: the condition is a report, not an outage.


## Scenario 9 — the NetworkPolicy, enforced for the first time

Milestone 6b wrote this policy; nothing has ever enforced it. `kindnet`
implements no NetworkPolicy controller, which 6b measured and
`charts/spawnery/README.md` records. Cilium does.

The policy selects the operator by its own two labels and admits 9443 from
pods carrying `spawnery.cloud/managed-by: spawnery-operator` under a
`namespaceSelector: {}` — **every** namespace, deliberately, because
`agentEndpoint` builds its dial name from the operator's own namespace and the
chart cannot know the game namespaces' names:

```
$ kubectl -n spawnery-system get networkpolicy spawnery-operator-agent -o jsonpath='{.spec}'
{"podSelector":{"matchLabels":{"app.kubernetes.io/component":"operator",
                               "app.kubernetes.io/name":"spawnery"}},
 "policyTypes":["Ingress"],
 "ingress":[{"from":[{"namespaceSelector":{},
                      "podSelector":{"matchLabels":{"spawnery.cloud/managed-by":"spawnery-operator"}}}],
             "ports":[{"port":9443,"protocol":"TCP"}]},
            {"ports":[{"port":8081,"protocol":"TCP"},{"port":8080,"protocol":"TCP"}]}]}
```

So the discriminator is the pod label, not the namespace. Both probes therefore
run **from the same namespace**, from the same image, with the same command,
differing in one label — probing from two namespaces would have measured
something the policy does not select on:

```
# no spawnery.cloud/managed-by label
$ nc -zv -w6 spawnery-operator.spawnery-system.svc.cluster.local 9443
nc: connect to ... (10.43.251.87) port 9443 (tcp) timed out: Operation in progress
exit=1

# identical pod, plus spawnery.cloud/managed-by: spawnery-operator
$ nc -zv -w6 spawnery-operator.spawnery-system.svc.cluster.local 9443
Connection to ... (10.43.251.87) 9443 port [tcp/*] succeeded!
exit=0
```

**The policy refuses what it should and admits what it must**, and the pair is
what makes that a result. A refusal alone would be equally consistent with a
misspelled Service name, a policy that denies everything, or an operator not
listening; the success on the labelled pod rules out all three, and the two
runs differ in nothing else.

The admitting half was already established indirectly in scenario 2 — the
lobby reached `Ready`, which requires the agent to have reached 9443 — but that
was one direction only. This is the direction 6b was written for.


## Acceptance criteria

Against the design's §9, one line each, naming the scenario that decided it.

| # | Criterion | Result |
|---|---|---|
| 1 | Both Kustomizations `Ready`, `spawnery-network` gated by `dependsOn` | **met** — scenario 1 |
| 2 | Operator in `spawnery-system` under `restricted`, from the digest, no pull secret | **met** — scenario 1, after the digest was pinned in the `HelmRelease` |
| 3 | `minecraft` enforces `restricted` and the lobby reaches `Ready` | **met** — scenario 2 |
| 4 | The `ProxyGroup` reports an address and a client reaches a lobby through it | **half met** — scenario 4: the address exists, is unique, resolves under `mc.paul.wtf` and is reported by the operator; no client reached it |
| 5 | A `HostPort` `ProxyGroup` gets a running pod on this CNI | **met** — scenario 5 |
| 6 | A node drain evicts a proxy under the budget without going below it | **not met** — scenario 6: the drain was declined on measured grounds, the eviction API was driven instead, and the budget's protective direction cannot be reached without players |
| 7 | Scenarios 7, 8 and 9 each produce a recorded result | **met** — including two results that are "no effect observed", which §6 expected |
| 8 | The runbook is `DRIVEN` and carries real output | **met** |

Criterion 4 depended on scenario 0, and scenario 0 succeeded: the sharing
question was answered, just not the way §5 expected. What criterion 4 finally
turned on was not Cilium at all but TCP filtering in front of the cluster,
which also took criterion 6 and half of scenario 7 with it. A `NodePort` was
not substituted; §6 did not ask for one.

## What this run changed in the repository

Three defects, each found by the cluster rather than by reading:

1. **No tag can carry its own digest.** `hack/publish.sh` takes the digest from
   `skopeo copy --digestfile`, so it cannot be written until the tag is
   published. `charts/spawnery/values.yaml`'s `image.digest` is therefore
   always one release behind, and the design's §4 and criterion 2 both assumed
   otherwise. The `HelmRelease` pins it instead.
2. **`service.externalTrafficPolicy` was silently ignored** in Traefik's
   values. The chart accepts free-form Service spec fields only under
   `service.spec`, and a key in the wrong place changes nothing and reports
   nothing.
3. **A change to a values file is not a change to a Service.** With
   `disableNameSuffixHash: true` the generated ConfigMap keeps its name, so
   Helm has no trigger and the upgrade waits for the release's own interval.

And two claims in the design were wrong:

- **§5's sharing premise.** Non-overlapping ports are necessary for two
  Services to share an address, not sufficient: under `Local` they must also
  select the same pods.
- **§7 scenario 9's method**, corrected before it ran: the policy's
  `namespaceSelector` is empty, so probing from two namespaces would have
  measured nothing it selects on.

## What this run did not establish

Beyond §1's standing limits — one cluster, one CNI, one distribution, nothing
about scale — the port filtering in front of this cluster left three things
unmeasured, and they are the same three:

- a player reaching the network (scenario 3);
- the PodDisruptionBudget refusing an eviction, which needs a pod the operator
  itself counts as occupied (scenario 6);
- `syncOccupiedLabel` adding the label rather than removing it (scenario 7).

All three unblock together the moment TCP 25565 reaches this cluster.
