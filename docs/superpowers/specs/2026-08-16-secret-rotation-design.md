# Design: milestone 5c — detecting forwarding secret rotation

## 1. What this milestone is

Milestone 5 in the master design reads *"Persistent groups with a PVC, ordered
shutdown and recreate updates; detecting secret rotation along with a runbook"*
(`docs/superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`, §11).
5a built the persistent groups, 5b the ordered shutdown and the recreate
updates. 5c is the last clause, and the master design already fixed its shape in
§6.5:

> **Secret rotation is a manual maintenance operation in V1.** The operator
> detects the change (a secret watch with hash comparison) and reports it as the
> condition `ForwardingSecretRotationPending` along with a Kubernetes event; the
> restarts follow a documented runbook: roll all backend groups first, then all
> proxy groups.

So 5c is **detection and reporting**. It rotates nothing, restarts nothing, and
takes no ordinal down. That restraint is the design, not a shortcut, and §6.5
gives the reason: neither Velocity nor Paper accepts two forwarding secrets at
once, so there is necessarily a window in which joins between an already-rotated
and a not-yet-rotated layer fail with *"Unable to verify player details"* — and
in exactly that window an automatic drain would want to move its players onto
fallback servers it can no longer reach. Automatic orchestration would need
registration to become generation-aware. That is deferred, and 5c does not
approach it.

**5c shares no mechanism with 5a or 5b** and could have been built before either
of them (`docs/superpowers/specs/2026-08-15-persistent-groups-design.md`, §1).
It does, however, have to stay out of 5b's way, which §2 is about.

### 1.1 The one rule everything else serves

The new hash **must not enter the pod hash**.

`LabelPodHash` (`internal/podspec/labels.go:46`) is a digest of the pod the
operator would render right now, and a pod whose label differs from the current
digest is stale and gets replaced — by 4c's rolling updates for an ephemeral
group and by 5b's `StaleSpec` candidate class for a persistent one. If a
rotation moved that digest, the operator would immediately recreate every pod of
the network, in no particular order, with proxies and backends interleaved. That
is not the deferred orchestration arriving early; it is the uncoordinated
version of it, and it produces exactly the outage §6.5 exists to avoid.

Hence: `DesiredServerHash` and `DesiredProxyHash` delete the new label before
digesting, alongside the `LabelPodHash` deletion each already performs
(`internal/podspec/hash.go:71` and `:129`) — **but not for the same reason, and
the existing comments' wording must not be copied onto it.** Those two call
themselves belt-and-braces because neither builder sets `LabelPodHash`, so the
deletion never actually removes anything. The forwarding label is the opposite:
the builders do set it, the digest input does contain it, and the deletion is
the only thing standing between a rotation and a fleet-wide recreate. §7's first
test is what proves the line is load-bearing rather than decorative.

### 1.2 What 5c does not touch

The Kotlin Velocity agent, the Paper agent, and the gRPC contract between them
and the operator. 5c is operator-side only. It also introduces no new candidate
class in 5b's takedown rule
(`docs/superpowers/specs/2026-08-16-persistent-updates-design.md`, §2): the
handover asked whether rotation should become a fifth one, and the answer is no,
for the reason in §1 above.

## 2. Reading the secret

### 2.1 Who reads it, and with which client

The **Network controller** is the only reader. It already holds
`spec.forwardingSecretRef` (`api/v1alpha1/network_types.go:24`) and already
requeues at `resyncInterval` (5 seconds,
`internal/controller/server_controller.go:75`), so detection latency is bounded
by a loop that exists.

It reads through an **uncached reader**, for the reason
`cmd/spawnery-operator/main.go:226` already states about the TLS bundle:

> The TLS bundle is read and written directly, never through the cache: a cached
> Secret would mean an informer over every Secret in the cluster, and the
> operator's role deliberately grants no list or watch on them.

Concretely that reader is `mgr.GetAPIReader()`, which
`internal/controller/setup.go` already hands to the `Bootstrapper` for the same
purpose. Nothing new is constructed and nothing is threaded through
`controller.Options`: `SetupAll` sets the field where it already sets
`Client: mgr.GetClient()`, and `cmd/spawnery-operator/main.go` is not touched at
all.

### 2.2 Why a poll and not a watch

§6.5 says "a secret watch", and §8 of the master design says the secret grant
should be "restricted through `resourceNames` to the secrets referenced in
networks". **Those two sentences cannot both be honoured**: `resourceNames` does
not restrict `list` or `watch` in Kubernetes RBAC — it applies to requests that
name a single object. A real watch therefore requires `list` and `watch` over
every Secret in the operator's scope.

Between the two, this design keeps the narrower grant and gives up the watch.
The cost is one `GET` per network per resync — twelve requests per minute for a
single-network cluster — and a detection latency of at most one resync.

### 2.2.1 Where the permission lives, and why not in the ClusterRole

The obvious move is `secrets: get` in the ClusterRole. It is one line, and it is
rejected here, because it would make the operator's ServiceAccount a reader of
**every Secret in the cluster**. The mitigation that suggests itself — `get`
without `list` means an attacker must know a name — carries less than it sounds
like: Secret names are not secret, and they are visible in the very pod specs
this operator is already allowed to list.

It would also break a test that is right to break. `TestTheAuthorizerActuallyDenies`
(`internal/rbacaudit/audit_envtest_test.go:182`) probes `secrets: get` in a
foreign namespace and requires a denial, on the grounds that "secrets are
granted by the namespaced Role, and only in spawnery-system". Deleting that
probe to make room for the grant would be the wrong instinct exactly.

So the grant is **namespace-scoped, one namespace at a time**:

- **The ClusterRole gains nothing.** `rbacaudit.RequiredCluster` is unchanged,
  and the denial probe above stays as it is and stays true.
- **`config/rbac/forwarding-secret-reader.yaml`** ships a `Role` granting
  `secrets: get` and a `RoleBinding` to the operator's ServiceAccount. Neither
  object carries a `metadata.namespace`, so the namespace comes from the apply:

  ```
  kubectl apply -n <network-namespace> -f config/rbac/forwarding-secret-reader.yaml
  ```

  It is deliberately not part of `config/deploy/`, which is what keeps the
  denial probe meaningful: a cluster where nobody applied it grants the operator
  no cross-namespace secret access at all.
- **The operator never creates it.** Granting an operator the right to write
  RBAC makes every other restriction on it advisory; the same test denies
  `clusterroles: create` for that reason.
- **No `list`, no `watch`, anywhere.** `internal/rbacaudit/required.go:171`
  states that those verbs are deliberately absent. That statement stays true.

**The failure mode is deliberate and visible.** A Network in a namespace where
nobody applied the Role produces a `Forbidden` on the `GET`, which surfaces as
`ForwardingSecretResolved=Unknown / SecretReadForbidden` with a message naming
the manifest to apply (§4.1). An install step that was skipped reports itself
instead of silently disabling rotation detection.

Milestone 6 owns the Helm chart, which is where rendering this Role for each
configured network namespace belongs.

An alternative was considered and rejected: a label-restricted informer over
Secrets, matching the pattern `main.go:192` already uses for ConfigMaps,
ServiceAccounts and PVCs. It would need the user to label a Secret they created
themselves, which makes an unlabelled secret invisible to the operator — a new
and silent failure mode — on top of the RBAC widening.

### 2.3 The hash

```
forwardingHash = hex(sha256(network.UID ‖ 0x00 ‖ secret.Data["secret"])[:8])
```

Sixteen hex characters, the same shape and truncation as `DesiredServerHash`.

**The salt is not decoration.** This value becomes a pod label, and read access
to pods is granted far more freely than read access to Secrets. An unsalted
truncated SHA-256 of a weakly chosen secret would turn "no access to the Secret"
into an off-the-shelf dictionary attack, with precomputation shared across every
Spawnery cluster in the world. Salting with the Network's UID forces the attack
to be redone per network and makes precomputed tables worthless. It does not
defeat a targeted dictionary attack against a weak secret; §8 records that rather
than dressing it up.

The key is `secret`, per `NetworkSpec.ForwardingSecretRef`'s documented contract
and `podspec.ForwardingSecretKey` (`internal/podspec/server.go:121`).

**The bytes are not trimmed.** A trailing newline produces a different hash and
is reported as a rotation. The digest covers exactly the bytes the pod mounts;
whether Velocity and Paper make the same thing of them is their business and not
something this operator asserts.

### 2.4 What gets recorded

`NetworkStatus` gains one field:

```go
// ForwardingSecretHash is the salted digest of the forwarding secret as the
// operator last read it, per design §2.3. Written only after a successful
// read: a transient failure must not leave newly created pods unstamped.
// +optional
ForwardingSecretHash string `json:"forwardingSecretHash,omitempty"`
```

On a failed read the previous value stays. That matters because it is what the
pod builders stamp from (§3), and a five-second API hiccup must not produce a
cohort of pods the operator can say nothing about afterwards.

## 3. Stamping the pod

`BuildServerPod` and `BuildProxyPod` both already receive the `Network`. They
read `net.Status.ForwardingSecretHash` and set:

```
spawnery.cloud/forwarding-hash: <16 hex chars>
```

No signature changes, and nothing is threaded through the group controllers:
they copy a string out of an object they already read, so the single reader
established in §2.1 stays a single reader.

**An empty hash means no label.** A pod without the label is *unknown*, not
*stale* — the same rule 5b's `podHash` follows, stated out loud this time
because §4 gives it a visible consequence.

**The label describes what the process loaded.** A pod mounts the secret through
a projected volume source that names it (`internal/podspec/server.go:285`,
`internal/podspec/proxy.go:189`), so every newly created pod receives the current
contents. The label is written at creation and never touched again, which makes
it say precisely the thing that matters: *which secret this process read when it
started*. That the kubelet later refreshes the file inside a running pod changes
nothing, because neither Velocity nor Paper reads it a second time.

## 4. The comparison and the two conditions

Each reconcile of an accepted Network lists the pods carrying both
`spawnery.cloud/managed-by=spawnery-operator` and
`spawnery.cloud/network=<name>`, **skips those with a `DeletionTimestamp`** — a
pod on its way out must not hold the report open — and sorts the rest into three
buckets by their `spawnery.cloud/forwarding-hash`: *current*, *stale*,
*untracked*.

Two boundary cases, so that neither is guessed at:

- **A network with no pods** has nothing stale and nothing untracked, so it
  reports `False / ForwardingSecretInSync` — vacuously in sync.
- **A Network that does not own its namespace** returns before reading any
  secret (§6), so neither condition is set on it at all. The absence is the
  answer: a Network that manages nothing reports nothing about a secret.

### 4.1 `ForwardingSecretResolved`

New, positive polarity, and the reason `Accepted` is left alone (§4.3).

| Status | Reason | When |
|---|---|---|
| `True` | `SecretResolved` | the Secret exists and its `secret` key is present and non-empty |
| `False` | `SecretNotFound` | the `GET` returned NotFound — the typo in `forwardingSecretRef` |
| `False` | `SecretKeyMissing` | it exists, but has no `secret` key, or an empty one |
| `Unknown` | `SecretReadForbidden` | the `GET` was denied — the reader Role of §2.2.1 was never applied to this namespace |
| `Unknown` | `SecretReadFailed` | any other error — the API server is unreachable |

Three distinctions worth keeping apart, because each has a different remedy:
`SecretNotFound` is a name the user can fix, `SecretReadForbidden` is an install
step that was skipped and whose message names the manifest to apply, and
`SecretReadFailed` is neither — nobody's typo, and nothing to edit.

### 4.2 `ForwardingSecretRotationPending`

The name §6.5 gives it. Negative polarity, matching `BackingOff`,
`NodeDraining` and `ReadinessDiverged` in the same repository
(`api/v1alpha1/common_types.go`).

| Status | Reason | When |
|---|---|---|
| `True` | `RotationPending` | at least one pod is stale |
| `False` | `ForwardingSecretInSync` | every pod is current, and none is untracked |
| `Unknown` | `PodsPredateTracking` | none is stale, but at least one is untracked |
| `Unknown` | `SecretUnresolved` | the secret could not be read, so no judgement is possible |

Precedence runs down the table: a known problem outranks an unknown one.

The `True` message names the groups and counts, split by role and **listed
backends first**, so that the message reads in the order the runbook is to be
executed in. Example:

```
2 server pods in group survival and 3 in group lobby, and 2 proxy pods in
group edge, still carry the previous forwarding secret; roll the server
groups first, then the proxy groups — see the rotation runbook
```

**Why untracked pods produce `Unknown` and not `True`.** After an operator
upgrade, no running pod carries the label. `True` would mean every upgrade
instructs every user to perform a rotation runbook they do not need — a false
alarm that costs the condition its credibility within two upgrades. `False`
would be the silent form of the same error: a genuine rotation shortly after an
upgrade would go unreported. `Unknown` is the only honest answer, it costs
nothing, and it resolves itself as pods turn over for any reason at all.

### 4.3 Why not `Accepted`

Because `Accepted=False` on a Network stops the network.
`internal/controller/servergroup_controller.go:132` derives `networkUsable` from
it, `internal/controller/proxygroup_controller.go:203` gates on it, and since 5b
`mayResize == networkUsable`. Reporting an unreadable secret there would freeze
all sizing and the whole persistent machinery — and would do so on a transient
read error, converting a five-second API hiccup into a self-inflicted outage.
`Accepted` keeps its meaning: this Network owns its namespace.

### 4.4 Events

Both `Warning`, both emitted **on transition only**, never once per five-second
resync:

- **`ForwardingSecretRotated`**, when `status.forwardingSecretHash` moves from a
  **non-empty** old value to a different one. Empty → value is adoption and
  emits nothing, the same rule 5b applies to `podHash`.
- **`ForwardingSecretNotFound`**, on entry into `SecretNotFound`. This is the
  loud channel for a misconfiguration that is today reported under the wrong
  name: a Network pointing at a secret that does not exist produces pods that
  hang in `ContainerCreating`, and the only operator-side account of it arrives
  after `--startup-deadline` as a counted startup failure, then as `BackingOff`
  and eventually `Degraded` — a group whose servers will not start, which is
  what a bad image looks like too. The fault is visible; what is missing is its
  name.

### 4.5 No per-group condition

Which group is still stale is in the Network condition's message. That is the
information the runbook needs, and it needs it in one place; a condition on
`ServerGroup` and `ProxyGroup` would add status surface on two more CRDs to say
the same thing.

## 5. The runbook

`docs/runbook-milestone-5c-secret-rotation.md`. Unlike the milestone evidence
runbooks that precede it, this one is a standing operating procedure rather than
a record of a run — it is a deliverable of the milestone per §11 of the master
design.

**Why backends first, stated as a reason and not only as an order.** A proxy
holding the old secret and a backend holding the new one reject each other. Roll
the proxies first and you throw every connected player out in the same second
*and* drop them into a network where no backend is reachable. Roll the backends
first and the proxies stay up, players stay connected, and unreachability grows
group by group. The one hard cut stays at the end and lasts as long as a proxy
restart, not as long as the whole rotation.

Progress is visible in a single command — which is the real payoff of the label
in §3:

```
kubectl get pods -n <ns> -l spawnery.cloud/network=<net> \
  -L spawnery.cloud/role -L spawnery.cloud/group -L spawnery.cloud/forwarding-hash
```

The steps, in order: confirm the namespace has the reader Role of §2.2.1, since
without it the operator reports `SecretReadForbidden` and detects nothing; write
down the current secret value; rotate it; confirm the operator saw it (the
condition and the event); roll each server group; verify no pod of that group
carries the old hash; roll each proxy group; confirm the condition reads
`False / ForwardingSecretInSync`. The rollback is to write the old value back
and roll whatever has already been rolled, which is why the value is written
down before anything changes.

Two warnings the document has to carry:

- **`kubectl delete pod` bypasses the PodDisruptionBudget.** The PDB protects an
  occupied pod against the eviction API; a direct deletion is not an eviction.
  Deleting an occupied pod disconnects the players on it. Rotation is a
  maintenance window.
- **5b's budget does not apply here.** *At most one ordinal of a persistent
  group down at a time* binds the takedowns the operator nominates. A human
  deleting every pod of a persistent group takes every world offline at once.
  The pacing is the operator's own to keep: delete one ordinal, wait for it to
  become `Ready` and to carry the new label, then the next.

## 6. Error handling and edge cases

- **Secret deleted while pods run.** `Resolved=False/SecretNotFound`,
  `RotationPending=Unknown/SecretUnresolved`, and the status hash keeps its last
  value (§2.4). Newly created pods are therefore stamped with a hash they never
  loaded — but they never start either, because the projected volume cannot
  mount. The label misreports a pending pod, never a running one. §8 records it.
- **A re-applied identical value** produces the same hash: no event, no
  condition, nothing. Correct.
- **A lost status field is recomputed harmlessly.** A content hash over a stable
  salt yields the same value again, so wiping `status` produces no spurious
  rotation event. A `resourceVersion` would not have had that property, which is
  the reason it is not one.
- **The second, non-accepted Network in a namespace** returns before reading any
  secret. It does not own the namespace, so it passes no judgement on its
  secret.
- **Operator restart.** Nothing to rebuild: the recorded hash is in `status` and
  the per-pod truth is on the pods.

## 7. Tests

The load-bearing one first.

1. **A rotation does not move `LabelPodHash`**, for both server and proxy pods.
   If this test fails, the operator rolls the whole network uncoordinated, which
   is the failure §1.1 exists to prevent.
2. **A rotation raises the condition**, names groups and roles in the message,
   and emits **exactly one** event; the next reconcile emits no second one.
3. **First observation emits no event** — an empty recorded hash is adoption.
4. **Untracked pods yield `Unknown/PodsPredateTracking`**, not `True`.
5. **A missing secret and a missing key** yield their own distinct reasons, and
   `Accepted` stays `True` throughout — so sizing keeps running (§4.3).
6. **The hash is salt-sensitive**: the same secret value in two Networks yields
   two different hashes.
7. **The reader Role grants exactly `secrets: get` and nothing else**, checked
   against `config/rbac/forwarding-secret-reader.yaml` in both directions the
   way `required.go` already checks the other two roles — and applied into a
   foreign namespace in envtest, it allows `get` there while `list`, `watch`,
   `create`, `update` and `delete` stay denied.
8. **`TestTheAuthorizerActuallyDenies` is untouched** and still passes: without
   the reader Role applied, `secrets: get` in a foreign namespace is denied.
9. **Terminating pods are skipped**: a stale pod with a `DeletionTimestamp` does
   not hold `RotationPending` at `True`.

## 8. What 5c leaves open

Recorded in `docs/known-issues.md` when the milestone lands:

- **Automatically orchestrated rotation remains deferred**, unchanged and for
  the reason §6.5 gives: it needs generation-aware registration. 5c changes
  nothing about that; it only makes visible when it would be wanted.
- **The salted short hash does not defeat a targeted dictionary attack** against
  a weakly chosen forwarding secret. Anyone with pod read access in the
  namespace can test guesses offline against the label.
- **The label says what a pod loaded at start**, not what it would load now.
  That is the point (§3), but it means the label of a pod that never started —
  because its secret is missing — describes an intention rather than a fact
  (§6).
- **No per-group condition** (§4.5).
- **Rotation detection is off until an install step is performed per
  namespace.** `config/rbac/forwarding-secret-reader.yaml` has to be applied
  into every namespace holding a Network (§2.2.1). Until it is, the Network
  reports `SecretReadForbidden` and names the manifest — so the gap announces
  itself rather than hiding — but it is a gap, and closing it for good belongs
  to milestone 6's Helm chart.

## 9. Evidence run

Two acceptance tests, the second being the more interesting one because it
demonstrates *why* the design is cautious rather than only that it works:

1. After the full runbook — rotate, roll backends, roll proxies — a player can
   join and reach a backend.
2. Mid-window, with backends rotated and proxies not, the join fails with
   *"Unable to verify player details"*.

The run is driven on the local kind cluster with a real licensed client, the way
the 5a and 5b runs were, and its record joins the others in `docs/`.
