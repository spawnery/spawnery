# Runbook: rotating a Network's forwarding secret

**Status: standing operating procedure.** Unlike
`docs/runbook-milestone-5a-evidence.md`,
`docs/runbook-milestone-5b-evidence.md` and the other evidence runbooks beside
it, this document is not a record of a run. It is the procedure itself, and it
stays true after milestone 5c ships. Milestone 5c is what makes the operator
report a rotation; every restart below is still a human's to perform.

Throughout, `<ns>` is the namespace holding the Network, `<net>` the Network's
name and `<secret>` the Secret named by `spec.forwardingSecretRef.name`.

---

## 1. What this is for, and when it applies

Rotating the Velocity modern forwarding secret of one Network: the value the
proxies present and the backends check when a player is handed from one to the
other. It is the Secret named by `spec.forwardingSecretRef`, under the key
`secret` (`internal/podspec/server.go:121`, `podspec.ForwardingSecretKey`).

**The rotation is manual, and the master design fixed that in
`docs/superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md` §6.5.**
Neither Velocity nor Paper accepts two forwarding secrets at once, and neither
re-reads the file once its process has started. So there is necessarily a
window in which one layer holds the new value and the other still holds the
old, and a join or a transfer across that boundary fails with *"Unable to
verify player details"*. §6.5 also gives the reason automatic orchestration is
deferred rather than merely unbuilt: in exactly that window an automatic drain
would want to move players onto fallback backends it can no longer reach, so
closing the window automatically needs registration to become
generation-aware. Until that exists, the operator detects the change and
reports it; the restarts follow this document.

The operator therefore never restarts anything on its own account here. A
rotation moves no pod hash: `DesiredServerHash` and `DesiredProxyHash` delete
`spawnery.cloud/forwarding-hash` before digesting
(`internal/podspec/hash.go:76` and `:137`), so nothing about a rotation makes a
pod stale in the sense that 4c's rolling updates or 5b's takedown rule act on.

---

## 2. Why the server groups go first

Not an arbitrary order. A proxy holding the old secret and a backend holding
the new one reject each other, so from the first restart until the last the two
layers disagree and every join across the boundary fails. The order decides
what that window costs.

**Roll the proxies first** and every connected player is thrown out in the same
second — a proxy restart drops its connections — *and* lands in a network where
no backend is reachable, because every backend still holds the old secret. The
outage lasts until the last server group has been rolled.

**Roll the server groups first** and the proxies stay up with their players
still connected. Unreachability grows group by group as each group crosses
over, and only the groups already rolled are unreachable. The one hard cut then
stays at the end, where it lasts as long as a proxy restart rather than as long
as the whole rotation.

The condition's own message is written in this order — the stale server groups
before the stale proxy groups (`staleSummary`,
`internal/controller/forwardingsecret.go:197`) — so the message reads in the
order the work is done.

---

## 3. Prerequisite: the reader Role, once per namespace

The operator reads the Secret with a namespaced grant that is not part of
`config/deploy/`. Without it the operator cannot read the Secret at all and
detects nothing:

```
kubectl apply -n <ns> -f config/rbac/forwarding-secret-reader.yaml
```

That manifest holds a `Role` named `spawnery-forwarding-secret-reader` granting
`secrets: get` and nothing else, and a `RoleBinding` of the same name to the
`spawnery-operator` ServiceAccount in `spawnery-system`. Neither object carries
a `metadata.namespace`, so the `-n <ns>` above supplies it.

Check it before rotating:

```
kubectl get role,rolebinding spawnery-forwarding-secret-reader -n <ns>
```

A namespace where this was never applied reports itself rather than staying
silent: `ForwardingSecretResolved` reads `Unknown` with reason
`SecretReadForbidden`, and its message names this manifest and the `kubectl
apply` line to fix it. In that state the operator has no digest to compare
against, so `ForwardingSecretRotationPending` reads `Unknown` with reason
`SecretUnresolved` and this runbook cannot be followed to its end — nothing
below step 3 will ever confirm.

---

## 4. The progress command

A pod the operator creates while it has a digest recorded carries that digest
in the label `spawnery.cloud/forwarding-hash`
(`internal/podspec/labels.go:74`), and the label is written at creation and
never touched again — so it says which secret that process read at startup. It
is a label rather than an annotation so that a single `kubectl get` shows the
whole fleet's progress, and so that the old value can be selected on:

```
kubectl get pods -n <ns> -l spawnery.cloud/network=<net> \
  -L spawnery.cloud/role -L spawnery.cloud/group -L spawnery.cloud/forwarding-hash
```

Sixteen hex characters per pod, or an empty column for a pod that carries no
stamp at all. An empty column means *unknown*, not *stale* — see §8.

---

## 5. The steps

### Step 1 — write down what is there now

Three readings, before anything changes. The first resolves `<secret>`. The
secret's value is what a rollback writes back (§7), and the current digest is
what the per-group checks in step 5 select on — after the rotation, the
operator records only the *new* digest and the old one is nowhere else.

```
kubectl get network <net> -n <ns> -o jsonpath='{.spec.forwardingSecretRef.name}{"\n"}'
kubectl get secret <secret> -n <ns> -o jsonpath='{.data.secret}' | base64 -d
kubectl get network <net> -n <ns> -o jsonpath='{.status.forwardingSecretHash}{"\n"}'
```

Call the third value `<old-hash>` below.

Also list what has to be rolled, in order — the server groups, then the proxy
groups:

```
kubectl get servergroups,proxygroups -n <ns>
```

### Step 2 — rotate the value

```
kubectl patch secret <secret> -n <ns> --type merge \
  -p '{"stringData":{"secret":"<new value>"}}'
```

The bytes are digested exactly as they are stored, untrimmed
(`podspec.ForwardingHash`, `internal/podspec/hash.go:168`), so a trailing
newline is a different secret and is reported as a rotation of its own.

### Step 3 — confirm the operator saw it

The Network controller re-reads the Secret on every reconcile and requeues
itself at `resyncInterval`, five seconds
(`internal/controller/network_controller.go:153`, the constant at
`internal/controller/server_controller.go:75`), so this answers within about
that.

The recorded digest, which should now differ from `<old-hash>`:

```
kubectl get network <net> -n <ns> -o jsonpath='{.status.forwardingSecretHash}{"\n"}'
```

The two conditions:

```
kubectl get network <net> -n <ns> -o jsonpath='{range .status.conditions[?(@.type=="ForwardingSecretResolved")]}{.status} {.reason} {.message}{"\n"}{end}'
kubectl get network <net> -n <ns> -o jsonpath='{range .status.conditions[?(@.type=="ForwardingSecretRotationPending")]}{.status} {.reason} {.message}{"\n"}{end}'
```

Expected: `True SecretResolved ...` and `True RotationPending ...`, the second
naming each stale group as `role/group=count` with the server groups first.

`True RotationPending` needs at least one *stamped* pod to compare. On a
network whose pods all predate this operator version, the second command reads
`Unknown PodsPredateTracking` instead while the rotation is genuinely pending —
that is not a sign the rotation did not land, and the first command plus the
digest above are what confirm it did. Roll everything anyway; the empty column
of the §4 command is what tells you which pods are still to go.

The event, emitted once on the transition rather than once per resync:

```
kubectl get events -n <ns> \
  --field-selector involvedObject.kind=Network,involvedObject.name=<net>,reason=ForwardingSecretRotated
```

If `ForwardingSecretResolved` reads `False SecretKeyMissing`, the patch in step
2 wrote the wrong key or an empty value. Fix that before going further. On a
failed read the recorded digest keeps its previous value
(`internal/controller/network_controller.go:125-134`), so a pod created in this
state is stamped with the old digest while its projected volume has no `secret`
key to mount — it stays in `ContainerCreating` carrying a stamp that describes
nothing it ever loaded.

### Step 4 — roll the server groups, one group at a time

Read §6 first: this disconnects the players on each pod it takes down, and for
a persistent group it is paced by hand.

For each `ServerGroup` in the namespace, list its pods and delete them:

```
kubectl get pods -n <ns> \
  -l spawnery.cloud/network=<net>,spawnery.cloud/role=server,spawnery.cloud/group=<group> \
  -L spawnery.cloud/forwarding-hash

kubectl delete pod <pod> -n <ns>
```

Deleting the pod is what rolls the server: with the pod gone, the Server
controller's state machine reads `PodLost` and moves the Server to
`Terminating` (`internal/phase/phase.go:316`), and a Server that reaches
`Terminating` without a deletion having been requested is removed so the group
creates a replacement (`internal/controller/server_controller.go:358-363`). For
a persistent group the replacement carries the same ordinal, and therefore the
same claim name — `PersistentServerName` is `<group>-<ordinal>`
(`internal/controller/persistent.go:36`) and `DataClaimName` derives from the
Server's name (`internal/podspec/server.go:157`) — so the world comes back with
it. Nothing in this operator deletes a claim (`growClaim`'s own comment,
`internal/controller/server_controller.go:373`).

### Step 5 — verify the group before moving to the next

Select on the old digest. An empty answer means no pod of that group is left on
it:

```
kubectl get pods -n <ns> \
  -l spawnery.cloud/network=<net>,spawnery.cloud/role=server,spawnery.cloud/group=<group>,spawnery.cloud/forwarding-hash=<old-hash> \
  -o name
```

And positively, that the replacements are up and carry the new one:

```
kubectl get pods -n <ns> \
  -l spawnery.cloud/network=<net>,spawnery.cloud/role=server,spawnery.cloud/group=<group> \
  -L spawnery.cloud/forwarding-hash
```

**Both selectors pin `role=server`, and that is not redundant.** A `ServerGroup`
and a `ProxyGroup` are different Kinds, so Kubernetes lets them share a name in
one namespace — a collision this repository designs around by name rather than
forbids (`podspec.GroupConfigMapName` and `podspec.GroupPDBName`,
`internal/podspec/labels.go:117-162`). Without the role term, a same-named
proxy group's pods answer this query too, and the step 4/5 loop never comes
back empty until the proxies are rolled — which this runbook says to do last.

Repeat steps 4 and 5 until no server group has a pod on `<old-hash>`, and until
no server pod carries an empty forwarding-hash column.

### Step 6 — roll the proxy groups

Same shape, `role=proxy`. This is the hard cut, and it starts at the end of
step 5 rather than at the first `delete` here: once every server group holds the
new secret, a proxy that still holds the old one can reach no backend at all,
so no player can join through it and any left connected are already stranded.
Restarting a proxy then disconnects the players on it, and the group is
whole again only when the last proxy is back. Work through the proxy groups
without pausing between them.

```
kubectl get pods -n <ns> \
  -l spawnery.cloud/network=<net>,spawnery.cloud/role=proxy,spawnery.cloud/group=<group> \
  -L spawnery.cloud/forwarding-hash

kubectl delete pod <pod> -n <ns>
```

The `ProxyGroup` controller creates the replacement pod to bring the group back
to its replica count.

### Step 7 — confirm the rotation is complete

```
kubectl get network <net> -n <ns> -o jsonpath='{range .status.conditions[?(@.type=="ForwardingSecretRotationPending")]}{.status} {.reason} {.message}{"\n"}{end}'
```

Expected: `False ForwardingSecretInSync every pod of this network runs on the
current forwarding secret`.

**If it reads `Unknown PodsPredateTracking` instead, the rotation is not
finished — do not stop here.** Outside a rotation that reading is benign and
clears on its own (§8). During one it means the opposite: a pod carrying no
stamp is a pod that was never rolled, it predates this operator's tracking, and
it is quite possibly still running the previous secret. Step 5 cannot see it
either — that check selects on `forwarding-hash=<old-hash>`, and a pod with no
such label matches no value of it.

Find them by the empty column, and roll them the same way, server pods before
proxy pods:

```
kubectl get pods -n <ns> -l spawnery.cloud/network=<net> \
  -L spawnery.cloud/role -L spawnery.cloud/group -L spawnery.cloud/forwarding-hash
```

With a digest recorded — which step 3 confirmed — every pod the operator
creates is stamped (`internal/podspec/server.go:441-443`,
`internal/podspec/proxy.go:300-302`), so the condition reaches `False
ForwardingSecretInSync` once the last unstamped pod has been replaced.

---

## 6. Two warnings

**`kubectl delete pod` bypasses the PodDisruptionBudget, and the players on
that pod are disconnected.** Each group's PDB is sized to its occupied pods and
selects on `spawnery.cloud/occupied`, which refuses the *eviction API* an
occupied pod (`internal/podspec/labels.go:34-45`). A direct `kubectl delete pod`
is not an eviction and the budget does not see it. Nor is there a drain on this
path: the state machine reaches `Terminating` through `PodLost`
(`internal/phase/phase.go:316`), not through the `Draining` branch that a
requested deletion takes. **A rotation is a maintenance window.** Announce it,
and do not reach for a drain to avoid the disconnection — §6.5 of the master
design is explicit that a drain mid-rotation would move players onto fallback
backends that the proxy, still holding the old secret, can no longer reach.

**5b's one-ordinal budget does not bind a human.** *At most one ordinal of a
persistent group is down at a time* constrains the takedowns the **operator**
nominates; it is a rule inside the nomination logic, not a lock on the objects.
A human deleting every pod of a persistent group takes every world in it
offline at once. Pace it by hand:

1. delete one ordinal's pod;
2. wait for its replacement to be `Ready` **and** to carry the new digest —
   both columns of the step 5 command;
3. only then the next ordinal.

---

## 7. Rollback

Write the old value back, exactly as it was recorded in step 1, and then roll
whatever has already been rolled — which is why the value is written down
before anything changes.

```
kubectl patch secret <secret> -n <ns> --type merge \
  -p '{"stringData":{"secret":"<the value from step 1>"}}'
```

The digest is a content hash over a stable salt, so restoring the same bytes
restores the same digest, and `status.forwardingSecretHash` returns to
`<old-hash>`. Pods that were never rolled are current again and need no action;
the pods rolled onto the new value are now the stale ones, and
`ForwardingSecretRotationPending` names them in the same `role/group=count`
form. Roll those back in the same order this runbook uses: server groups first,
then proxy groups.

---

## 8. What each condition state means

Both conditions live on the Network. They are deliberately separate from
`Accepted`, which keeps its own meaning — this Network owns its namespace — so
that an unreadable secret never stops the network's sizing
(`api/v1alpha1/common_types.go:61-69`).

### `ForwardingSecretRotationPending`

Negative polarity: `True` is the problem. Precedence runs down this table — an
unreadable secret is decided before a stale pod, which is decided before an
unstamped one.

| Status | Reason | What it means | What to do |
|---|---|---|---|
| `True` | `RotationPending` | at least one pod runs on a digest other than the current one; the message names each as `role/group=count` | run this runbook: server groups first (§5 step 4), then proxy groups (§5 step 6) |
| `False` | `ForwardingSecretInSync` | every pod carries the current digest and none is unstamped. A network with no pods at all reads this too, vacuously | nothing |
| `Unknown` | `PodsPredateTracking` | no pod is stale, but at least one carries no stamp — see below | **outside a rotation:** nothing, it clears as pods turn over. **During one (§5 step 7):** the unstamped pods have not been rolled — find them by the empty column of the §4 command and roll them |
| `Unknown` | `SecretUnresolved` | the secret could not be read, so no comparison is possible; the message carries the `ForwardingSecretResolved` message inside it | fix the read first — the table below |

**`PodsPredateTracking` needs its own paragraph, because it looks like a
failure and is not one.** The operator cannot tell whether an unstamped pod
runs on the current secret: the stamp is written when the pod is created and
never afterwards, so a pod created before this operator version existed carries
none. That is the ordinary state of every running pod immediately after an
operator upgrade, and it clears as pods turn over for any reason at all. It is
`Unknown` rather than `True` because `True` would send every user through this
runbook after every upgrade, and rather than `False` because a genuine rotation
shortly after an upgrade would then go unreported.

### `ForwardingSecretResolved`

Positive polarity: `True` is healthy.

| Status | Reason | What it means | What to do |
|---|---|---|---|
| `True` | `SecretResolved` | the Secret exists and its `secret` key holds a non-empty value | nothing |
| `False` | `SecretNotFound` | the `GET` returned NotFound: `spec.forwardingSecretRef` names a Secret that does not exist in this namespace | fix the name, or create the Secret. Pods of this network hang in `ContainerCreating` until it exists, because the projected volume cannot mount |
| `False` | `SecretKeyMissing` | the Secret exists but has no `secret` key, or an empty one | put the forwarding secret under the key `secret` |
| `Unknown` | `SecretReadForbidden` | the `GET` was denied: the reader Role of §3 was never applied to this namespace. Forbidden arrives before the operator can learn whether the Secret exists, so it may be present and pods may still start, or it may be missing and they hang in `ContainerCreating` — see the note below before rotating in this state | `kubectl apply -n <ns> -f config/rbac/forwarding-secret-reader.yaml` |
| `Unknown` | `SecretReadFailed` | any other error — the API server was unreachable, for instance. Nobody's typo and nothing to edit. Same caveat as the row above, and the kubelet projects the Secret through that same API server, so an unreachable one stops pods starting for its own reason | look at the message and at the operator's logs; it clears when the read succeeds |

`SecretNotFound` is also the one reported as an event,
`ForwardingSecretNotFound`, on entry into that state:

```
kubectl get events -n <ns> \
  --field-selector involvedObject.kind=Network,involvedObject.name=<net>,reason=ForwardingSecretNotFound
```

### One thing the stamp does not say

**The stamp is the last digest the operator read, not necessarily the bytes the
pod mounted.** On any failed read the recorded digest keeps its previous value
(`internal/controller/network_controller.go:125-134`) and the pod builders
stamp it whenever it is non-empty (`internal/podspec/server.go:441-443`,
`internal/podspec/proxy.go:300-302`). The kubelet, meanwhile, projects whatever
the Secret holds, which has nothing to do with whether the *operator* may read
it. So a pod created while a read is failing carries a claim the operator could
not check, and the two failure families differ:

- **`SecretNotFound` and `SecretKeyMissing`.** There is nothing to project, so
  such a pod never starts — it sits in `ContainerCreating`. Its label describes
  an intention rather than a fact, and the stamp misreports a pod that is not
  running.
- **`SecretReadForbidden` and `SecretReadFailed`.** The Secret itself may be
  perfectly present, so such a pod **starts normally**. If the secret was
  rotated inside that window — the reader Role removed, or the API server
  briefly unreachable — the pod loads the new value and is stamped with the old
  digest. When the read recovers, the operator reports that pod stale although
  it is current, and `RotationPending` stays `True` until it is rolled.

Rolling the pod resolves it either way, but only once the read succeeds again:
a pod rolled while the read is still failing is stamped from the same retained
digest and is mis-stamped a second time. Fix the read first — the remedy
column above says how for each reason — and then roll, because a pod created
against a readable secret is stamped with the digest of the bytes it is about
to mount. Recorded in `docs/known-issues.md`.
