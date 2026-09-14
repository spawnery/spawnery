# Runbook: detecting a forwarding secret rotation, on a real cluster

**DRIVEN 2026-08-16.** Both acceptance tests passed. Results are recorded
against each section's expectation below; the summary is here.

| § | Expected | Observed |
|---|---|---|
| 3 | the CRD carries `pattern` | `"pattern":"^([a-f0-9]{16})?$"` |
| 4 | the reader Role applies with `-n` supplying the namespace | Role and RoleBinding created in `minecraft`; neither names a namespace in the file |
| 5 | one `Ready` server, two pods, one `Bound` claim | `survival-0` Ready and registered, `gateway-gyfd` Running, `survival-0-data` Bound |
| 6 | recomputed = published = both labels | all three `d4c4ffe9483d0af4` |
| 6 | `True/SecretResolved`, `False/ForwardingSecretInSync` | both, as written |
| 7 | the join lands on `survival-0` | it did; players 1 on group, proxy and network |
| 8 | one `Warning` event, `True/RotationPending`, server named before proxy | `COUNT 1`; `server/survival=1, proxy/gateway=1` |
| 8 | `COUNT` still 1 after ~20 more reconciles | still 1, `firstTimestamp == lastTimestamp` |
| 9 | stamps diverge; message names only the proxy | server `114dbffced4d9d27`, proxy `d4c4ffe9483d0af4`; `proxy/gateway=1` |
| 9 | **the join fails with "Unable to verify player details"** | **it did — verbatim from both layers, below** |
| 10 | both stamps new, `False/ForwardingSecretInSync` | both `114dbffced4d9d27`; "every pod of this network runs on the current forwarding secret" |
| 10 | **the join succeeds** | **it did, on `survival-0`** |
| 11 | `spec.podHash` unchanged | `0dd5900930601f18` before and after |
| 11 | pod UID changed exactly once | `23e30d8f…` → `f4b115da…` |
| 12 | `False/SecretNotFound`, one event, `Accepted` still `True` | all three; the network kept serving its connected player throughout |
| 12 | restoring the secret reports no second rotation | `True/SecretResolved`, `False/ForwardingSecretInSync`, rotation events still 1 |

**§9's verbatim record**, which is the run's most valuable observation. The
backend rejects and the proxy relays the rejection — the direction matters,
because it is the proxy that signed with the stale secret:

```
survival-0 (Paper):
  [20:43:52 INFO]: Disconnecting paul_wtf (/10.244.0.5:36428): Unable to verify player details
  [20:43:52 INFO]: paul_wtf (/10.244.0.5:36428) lost connection: Unable to verify player details

gateway-gyfd (Velocity):
  [20:43:52 ERROR]: [connected player] paul_wtf (/10.244.0.1:55188): disconnected while
    connecting to survival-0: Unable to verify player details
  [20:43:52 INFO]: [connected player] paul_wtf (/10.244.0.1:55188) has disconnected:
    Unable to connect to survival-0: Unable to verify player details
```

**One correction this run made to itself:** §11 as first written expected
`PodCreated` events "only at §5, §9 and §10". `PodCreated` is the Server
controller's event and covers server pods only — the ProxyGroup controller emits
none, so §10's proxy recreation appears as kubelet `Scheduled`/`Created`/`Started`
events instead. §11 below says so now.

**One thing the run confirmed that it was not looking for:** the §9 roll
produced a `PodLost` event, not a `Failed` one. That is what the standing
runbook relies on when it says a rotation roll cannot trip the group's failure
backoff and strand the network half-rotated — derived from the code during the
branch review, and visible in the event log here.

---

This is the evidence run for milestone 5c. Unlike the runs for 5a and 5b, it
does not describe the operations it performs: **it drives
`docs/runbook-milestone-5c-secret-rotation.md`**, the standing procedure the
milestone ships, and records what happened. If that document is wrong, this run
is where it shows.

## What this measures

Two acceptance tests, from the design's §9
(`docs/superpowers/specs/2026-08-16-secret-rotation-design.md`). The second is
the more interesting one, because it demonstrates *why* the design refuses to
automate rotation rather than only that detection works:

1. **After the full procedure** — rotate, roll the server group, roll the proxy
   group — a player can join and reach a backend.
2. **Mid-window**, with the backend rotated and the proxy not, the join fails
   with *"Unable to verify player details"*.

Around those, four things only a real cluster can show:

- The digest the operator publishes is genuinely `sha256(networkUID ‖ 0x00 ‖
  secret)` truncated to eight bytes — recomputed here by hand from the Network's
  UID and the secret's plaintext, which is the only check that proves the salt
  is what the design says it is.
- Every running pod carries that digest as a label, and the label matches.
- **A rotation moves no pod hash.** The milestone's load-bearing property, and
  the one that would be catastrophic to get wrong: if the digest had reached
  `spec.podHash`, the operator would recreate every ordinal by itself, and this
  run would see servers restarting that nobody asked to restart.
- A deleted secret produces `SecretNotFound` and its event — the diagnostic gap
  that existed before this milestone, where a mistyped `forwardingSecretRef`
  surfaced only as servers that would not start.

## What this run does NOT measure, and why

**The per-namespace reader Role is not exercised.** The operator runs outside
the cluster under the driver's own kubeconfig, which is cluster-admin — the same
arrangement 5a and 5b used. Every `GET` of the Secret therefore succeeds
regardless of RBAC, so neither
`config/rbac/forwarding-secret-reader.yaml`'s necessity nor the
`SecretReadForbidden` condition can appear here. Both are covered by
`internal/rbacaudit`'s envtest, which applies the Role into its own namespace
and probes the authorizer directly. Demonstrating it on this cluster would mean
running the operator under a ServiceAccount token instead, which is a different
run than this one.

§4 applies the reader Role anyway, and confirms only that it applies cleanly.

## 0. Prerequisites

**Read `docs/runbook-milestone-5a-evidence.md` §0 and satisfy it.** Nothing
there is repeated: `x86_64-linux`, rootless Podman, a `TMPDIR` on a real
filesystem, a licensed Minecraft Java Edition client at 26.2 (protocol 776) and
a Microsoft account that owns the game, a person to drive that client, and
network reach from the client's machine to the cluster host's NodePort.

**This run needs less of the machine than 5b's did** — one ordinal, not two, and
no second storage class. It needs the driver at the client for three separate
moments (§7, §9, §10) rather than one, with cluster work between them.

## 1. Build and load the images

Identical to `docs/runbook-milestone-5a-evidence.md` §1.

```bash
cd /path/to/spawnery
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make image-load CONTAINER=podman
nix develop -c env TMPDIR="$HOME/.cache/spawnery-tmp" \
  make velocity-image-load CONTAINER=podman
```

Use whatever tags these two commands actually print if a Paper or Velocity bump
has moved them since this was written.

## 2. Create a single-node `kind` cluster

```bash
cat >/tmp/spawnery-5c-kind.yaml <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: 30567
        hostPort: 30567
EOF

systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind create cluster --name spawnery-5c \
  --config /tmp/spawnery-5c-kind.yaml
```

This run's own cluster name, so it never collides with a 5a or 5b cluster that
happens to still exist. Unlike 5b's §2, nothing here depends on the storage
class's `allowVolumeExpansion`, so its value does not need checking.

## 3. Load the images and apply the CRDs

```bash
nix build .#paper-image --out-link "$HOME/.cache/spawnery-tmp/paper-img"
nix build .#velocity-image --out-link "$HOME/.cache/spawnery-tmp/velocity-img"

for img in paper velocity; do
  systemd-run --scope --user --property=Delegate=yes --quiet \
    env KIND_EXPERIMENTAL_PROVIDER=podman TMPDIR="$HOME/.cache/spawnery-tmp" \
    nix develop -c kind load image-archive \
    "$HOME/.cache/spawnery-tmp/${img}-img" --name spawnery-5c
done

nix develop -c kubectl apply -f config/crd/bases
```

Confirm the CRD carries this milestone's field **and its pattern** — the pattern
is what stops a malformed digest from reaching a pod label and failing every
pod `Create` for the network:

```bash
nix develop -c kubectl get crd networks.spawnery.cloud -o jsonpath\
='{.spec.versions[0].schema.openAPIV3Schema.properties.status.properties.forwardingSecretHash}'
echo
```

**Expect a JSON object containing `"pattern":"^([a-f0-9]{16})?$"`.** An empty
result means the CRDs were applied from a stale tree.

## 4. Run the operator outside the cluster, and hand-build what its pods dial

Identical to `docs/runbook-milestone-5a-evidence.md` §4.

```bash
nix develop -c kubectl create namespace minecraft
nix develop -c go run ./cmd/spawnery-operator \
  --leader-elect=false --operator-namespace minecraft &

podman run -d --name spawnery-5c-relay --network kind \
  -v /nix/store:/nix/store:ro \
  --entrypoint "$(nix build --no-link --print-out-paths nixpkgs#socat)/bin/socat" \
  ghcr.io/spawnery/paper:26.2-0.2.0 \
  TCP-LISTEN:9443,fork,reuseaddr TCP:host.containers.internal:9443
RELAY_IP=$(podman inspect spawnery-5c-relay \
  --format '{{.NetworkSettings.Networks.kind.IPAddress}}')

nix develop -c kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: spawnery-operator
  namespace: minecraft
spec:
  ports:
    - name: agent
      port: 9443
      targetPort: 9443
      protocol: TCP
---
apiVersion: v1
kind: Endpoints
metadata:
  name: spawnery-operator
  namespace: minecraft
subsets:
  - addresses:
      - ip: $RELAY_IP
    ports:
      - name: agent
        port: 9443
        protocol: TCP
EOF
```

Then the standing procedure's own prerequisite, which this run performs even
though its effect cannot be observed here (see "What this run does NOT
measure"):

```bash
nix develop -c kubectl apply -n minecraft \
  -f config/rbac/forwarding-secret-reader.yaml
```

**Expect `role.rbac.authorization.k8s.io/spawnery-forwarding-secret-reader
created` and the matching `rolebinding`.** Neither object names a namespace, so
this is also the check that `-n` really supplies it.

**Leave the operator's log visible for the whole run.** `docs/known-issues.md`'s
"From the milestone 5a evidence run" section records that the recreate path logs
a benign `level=error` line with a stacktrace on every ordinary recreate —
expect one at each roll below, and read past it.

## 5. Apply the network

One persistent ordinal and one proxy: the minimum that has both layers of the
forwarding handshake, which is what a rotation breaks.

```bash
nix develop -c kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft
stringData:
  secret: 5c-evidence-run-secret-before
---
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: evidence
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
  name: survival
  namespace: minecraft
spec:
  networkRef:
    name: evidence
  type: Persistent
  replicas: 1
  image: ghcr.io/spawnery/paper:26.2-0.2.0
  maxPlayers: 20
  storage:
    size: 1Gi
  drain:
    timeoutSeconds: 60
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft
spec:
  networkRef:
    name: evidence
  replicas: 1
  image: ghcr.io/spawnery/velocity:3.5.1-0.2.0
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      memory: 1Gi
  expose:
    type: NodePort
    nodePort:
      port: 30567
  routing:
    fallbackGroups:
      - survival
  config:
    playerLimit: 20
    motd: "5c evidence run"
EOF

sleep 75
nix develop -c kubectl get network,servergroup,proxygroup,servers,pods,pvc -n minecraft
```

**Expect one `Ready` server `survival-0`, two `1/1 Running` pods, and one
`Bound` claim `survival-0-data`.**

## 6. The baseline, and the digest recomputed by hand

This is the section that proves the digest is what the design says, rather than
merely that some digest exists.

```bash
NETUID=$(nix develop -c kubectl get network evidence -n minecraft \
  -o jsonpath='{.metadata.uid}')
SECRET=$(nix develop -c kubectl get secret velocity-forwarding-secret -n minecraft \
  -o jsonpath='{.data.secret}' | base64 -d)
EXPECT=$({ printf '%s' "$NETUID"; head -c1 /dev/zero; printf '%s' "$SECRET"; } \
  | sha256sum | cut -c1-16)

echo "recomputed: $EXPECT"
nix develop -c kubectl get network evidence -n minecraft \
  -o jsonpath='published:   {.status.forwardingSecretHash}{"\n"}'
nix develop -c kubectl get pods -n minecraft -l spawnery.cloud/network=evidence \
  -L spawnery.cloud/role -L spawnery.cloud/group -L spawnery.cloud/forwarding-hash
```

`head -c1 /dev/zero` rather than `printf '\0'`: bash reads `\0` as the start of
an octal escape, and what it does with no digits after it is not worth relying
on for the one byte that separates the salt from the value.

**Expect the recomputed value, the published value and both pods' labels to be
the same sixteen hex characters.** That equality is the whole chain: the
operator salted with the Network's UID, published the result, and stamped both
layers with it.

Now the two conditions, and the pod hash to compare against later:

```bash
nix develop -c kubectl get network evidence -n minecraft -o jsonpath\
='{range .status.conditions[?(@.type=="ForwardingSecretResolved")]}Resolved: {.status}/{.reason}{"\n"}{end}'
nix develop -c kubectl get network evidence -n minecraft -o jsonpath\
='{range .status.conditions[?(@.type=="ForwardingSecretRotationPending")]}Pending:  {.status}/{.reason}{"\n"}{end}'

nix develop -c kubectl get server survival-0 -n minecraft \
  -o jsonpath='{.spec.podHash}' | tee /tmp/spawnery-5c-podhash-before
echo
nix develop -c kubectl get pod survival-0 -n minecraft \
  -o jsonpath='{.metadata.uid}' | tee /tmp/spawnery-5c-poduid-before
echo
```

**Expect `Resolved: True/SecretResolved` and `Pending: False/ForwardingSecretInSync`.**

The two values go to files rather than shell variables so that §11 can compare
them from a different shell than this one — a run driven across several
sessions, or by an agent whose shell state does not survive between commands,
otherwise silently compares against an empty string and passes.

## 7. Join, and confirm the baseline works

Point the licensed client at `127.0.0.1:30567` (or the tunnelled port from §0)
and log in.

```bash
nix develop -c kubectl logs -n minecraft -l spawnery.cloud/role=server \
  --prefix=true --tail=-1 --timestamps | grep 'joined the game'
```

**Expect the join to succeed and land on `survival-0`.** This is the control:
everything §9 shows is only meaningful against a network that was working a
minute earlier.

Leave the game after confirming.

## 8. Rotate, and watch the operator notice

From here the driver follows
`docs/runbook-milestone-5c-secret-rotation.md`. Its §5 step 1 says to write the
current value down first; this run's is `5c-evidence-run-secret-before`, and it
is written here.

```bash
nix develop -c kubectl patch secret velocity-forwarding-secret -n minecraft \
  --type merge -p '{"stringData":{"secret":"5c-evidence-run-secret-after"}}'

sleep 10

nix develop -c kubectl get events -n minecraft \
  --field-selector reason=ForwardingSecretRotated \
  -o custom-columns=COUNT:.count,TYPE:.type,REASON:.reason,MESSAGE:.message
nix develop -c kubectl get network evidence -n minecraft -o jsonpath\
='{range .status.conditions[?(@.type=="ForwardingSecretRotationPending")]}{.status}/{.reason}{"\n"}{.message}{"\n"}{end}'
```

**Expect exactly one `Warning` `ForwardingSecretRotated` event with `COUNT` 1,**
and the condition at `True/RotationPending` with a message naming both layers,
**server first**:

```
still on the previous forwarding secret: server/survival=1, proxy/gateway=1; …
```

The ordering is not cosmetic — it is the order the procedure is to be executed
in, rendered by `staleSummary`.

Wait a further thirty seconds and re-read the event's `COUNT`. **Expect it still
to be 1.** The requeue is five seconds, so a per-resync event would be visibly
climbing by now; this is the check that the suppression works on a real cluster
and not only in envtest.

## 9. Acceptance test 2 — the mid-window failure

Roll the server group only, per the standing runbook's §5 step 4. One ordinal,
so its pacing warning has nothing to pace here.

```bash
nix develop -c kubectl delete pod -n minecraft \
  -l spawnery.cloud/network=evidence,spawnery.cloud/role=server,spawnery.cloud/group=survival

sleep 60
nix develop -c kubectl get pods -n minecraft -l spawnery.cloud/network=evidence \
  -L spawnery.cloud/role -L spawnery.cloud/forwarding-hash
```

**Expect the server pod's stamp to be the new digest and the proxy pod's to be
the old one.** Recompute the new digest the §6 way if you want the comparison
spelled out rather than eyeballed.

```bash
nix develop -c kubectl get network evidence -n minecraft -o jsonpath\
='{range .status.conditions[?(@.type=="ForwardingSecretRotationPending")]}{.status}/{.reason}{"\n"}{.message}{"\n"}{end}'
```

**Expect `True/RotationPending` still, with the message now naming only
`proxy/gateway=1`** — the server group has dropped out of the work list.

**Now the acceptance test.** Point the client at the same address and try to
join.

**Expect the join to fail with *"Unable to verify player details"*.** Record the
message verbatim, including any surrounding text the client shows — this is the
run's most valuable observation, because it is the failure mode the whole design
is arranged around. If it fails with something else, that is a finding, not a
detail: write down what appeared.

## 10. Acceptance test 1 — finish the procedure

Roll the proxy group, per the standing runbook's §5 step 6.

```bash
nix develop -c kubectl delete pod -n minecraft \
  -l spawnery.cloud/network=evidence,spawnery.cloud/role=proxy

sleep 45
nix develop -c kubectl get pods -n minecraft -l spawnery.cloud/network=evidence \
  -L spawnery.cloud/role -L spawnery.cloud/forwarding-hash
nix develop -c kubectl get network evidence -n minecraft -o jsonpath\
='{range .status.conditions[?(@.type=="ForwardingSecretRotationPending")]}{.status}/{.reason}{"\n"}{.message}{"\n"}{end}'
```

**Expect both stamps to be the new digest, and the condition to read
`False/ForwardingSecretInSync`** with the message "every pod of this network
runs on the current forwarding secret". That is the standing runbook's §5 step 7
completion criterion, reached.

**Now the acceptance test.** Join again.

**Expect the join to succeed and land on `survival-0`.** Confirm from the log
the §7 way.

## 11. The invariant, on a real cluster

```bash
echo "podHash before: $(cat /tmp/spawnery-5c-podhash-before)"
nix develop -c kubectl get server survival-0 -n minecraft \
  -o jsonpath='podHash after:  {.spec.podHash}{"\n"}'

echo "pod uid before: $(cat /tmp/spawnery-5c-poduid-before)"
nix develop -c kubectl get pod survival-0 -n minecraft \
  -o jsonpath='pod uid after:  {.metadata.uid}{"\n"}'
```

**Expect `spec.podHash` to be unchanged across the whole rotation.** Had the
forwarding digest reached it, the operator would have judged every ordinal stale
the moment the secret changed and recreated them itself — the uncoordinated
rollout the master design defers.

**Expect the pod UID to have changed exactly once**, by our own `kubectl delete`
in §9. Two changes would mean the operator recreated the ordinal on its own
after we did; that is the same defect seen from the other side.

Also confirm nothing was recreated between §8 and §9 — the window in which the
operator knew about the rotation and had not been told to do anything:

```bash
nix develop -c kubectl get events -n minecraft \
  --field-selector reason=PodCreated \
  -o custom-columns=COUNT:.count,MESSAGE:.message,FIRST:.firstTimestamp
```

**Expect exactly two `created pod survival-0` events: one at §5 and one at §9,
with nothing between the rotation's timestamp and §9's `kubectl delete`.**

`PodCreated` is the Server controller's own event, so it covers server pods and
not proxy pods — the ProxyGroup controller emits none, and §10's proxy
recreation shows up as the kubelet's `Scheduled`/`Created`/`Started` instead.
That is fine for what this check is for: the ordinal is the thing whose
unrequested recreation would mean the digest had reached the pod hash. Read the
full event list if you want the proxy's side of it:

```bash
nix develop -c kubectl get events -n minecraft \
  -o custom-columns=REASON:.reason,COUNT:.count,OBJ:.involvedObject.name \
  --sort-by=.metadata.creationTimestamp
```

Worth noticing there: §9's roll produces `PodLost`, not `Failed`. That is what
the standing runbook depends on when it says a rotation roll cannot trip the
group's failure backoff — a `kubectl delete` drives `PodLost → Terminating`, and
`CountFailures` counts only `phase.Failed`.

## 12. The diagnostic gap 5c closes

Short, and worth doing because it is the case that had no operator-side name
before this milestone: a `forwardingSecretRef` pointing at nothing.

```bash
nix develop -c kubectl delete secret velocity-forwarding-secret -n minecraft
sleep 10

nix develop -c kubectl get network evidence -n minecraft -o jsonpath\
='{range .status.conditions[?(@.type=="ForwardingSecretResolved")]}{.status}/{.reason}{"\n"}{.message}{"\n"}{end}'
nix develop -c kubectl get events -n minecraft \
  --field-selector reason=ForwardingSecretNotFound \
  -o custom-columns=COUNT:.count,TYPE:.type,MESSAGE:.message
nix develop -c kubectl get network evidence -n minecraft -o jsonpath\
='{range .status.conditions[?(@.type=="Accepted")]}Accepted: {.status}/{.reason}{"\n"}{end}'
```

**Expect `False/SecretNotFound` naming the secret and the namespace, exactly one
`Warning` `ForwardingSecretNotFound` event, and `Accepted` still `True`.** The
last is the one worth pausing on: an unreadable secret must not reach `Accepted`,
because since 5b that condition gates all sizing for the network — a typo in one
field would otherwise stop the network from scheduling anything.

Put it back and confirm the operator recovers:

```bash
nix develop -c kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft
stringData:
  secret: 5c-evidence-run-secret-after
EOF

sleep 10
nix develop -c kubectl get network evidence -n minecraft -o jsonpath\
='{range .status.conditions[?(@.type=="ForwardingSecretResolved")]}{.status}/{.reason}{"\n"}{end}'
```

**Expect `True/SecretResolved`.** The recreated secret carries the post-rotation
value, so the digest is unchanged and no second rotation is reported.

## 13. Clean up

```bash
kill %1                          # the operator
podman rm -f spawnery-5c-relay
systemd-run --scope --user --property=Delegate=yes \
  env KIND_EXPERIMENTAL_PROVIDER=podman \
  nix develop -c kind delete cluster --name spawnery-5c
rm -f /tmp/spawnery-5c-kind.yaml
```

## Where this goes

Mark this document **DRIVEN &lt;date&gt;** at the top when the run completes, with
each section's observed result recorded against its expectation — including,
verbatim, whatever the client actually said in §9. Anything that surprised the
run goes to `docs/known-issues.md`; anything that was wrong in
`docs/runbook-milestone-5c-secret-rotation.md` gets fixed there, since that
document is the one that outlives this run.
