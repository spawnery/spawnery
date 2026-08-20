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


## Scenario 1 — installation

## Scenario 2 — `restricted` against a game server namespace

## Scenario 3 — a real join

## Scenario 4 — the LoadBalancer address

## Scenario 5 — HostPort under the real CNI

## Scenario 6 — node drain and the PodDisruptionBudget

## Scenario 7 — the three RBAC gaps

## Scenario 8 — the widened denial measurement

## Scenario 9 — the NetworkPolicy, enforced for the first time
