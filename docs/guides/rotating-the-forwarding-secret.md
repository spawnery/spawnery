# Rotating a Network's forwarding secret

You are replacing the value in one Network's forwarding Secret and restarting
every pod that reads it, server groups first and proxy groups last.
Neither Velocity nor Paper takes two secrets at once or re-reads the file, so
until both layers have crossed over a join between them fails with *"Unable to
verify player details"*.

`<ns>` is the namespace, `<net>` the Network, `<secret>` the Secret named by
`spec.forwardingSecretRef.name`.

## Before you start: the reader Role

The operator needs a namespaced grant to read the Secret: the chart renders it
for namespaces listed in `networkNamespaces`, anywhere else by hand
([Installing the operator](../getting-started/index.md)).

```
kubectl get role,rolebinding spawnery-forwarding-secret-reader -n <ns>
kubectl apply -n <ns> -f config/rbac/forwarding-secret-reader.yaml
```

Without it the conditions read `Unknown SecretReadForbidden` and `Unknown
SecretUnresolved`, and nothing below step 3 can confirm.

## The progress command

`spawnery.cloud/forwarding-hash` carries the digest recorded when the pod was
created; written once and never revised, it names the secret that process read
at startup:

```
kubectl get pods -n <ns> -l spawnery.cloud/network=<net> \
  -L spawnery.cloud/role -L spawnery.cloud/group -L spawnery.cloud/forwarding-hash
```

An empty column means no stamp at all — *unknown*, not *stale*.

## Step 1 — write down what is there now

The value is what a rollback writes back; the digest is what step 5 selects
on, and afterwards only the new one is kept.

```
kubectl get network <net> -n <ns> -o jsonpath='{.spec.forwardingSecretRef.name}{"\n"}'
kubectl get secret <secret> -n <ns> -o jsonpath='{.data.secret}' | base64 -d
kubectl get network <net> -n <ns> -o jsonpath='{.status.forwardingSecretHash}{"\n"}'
```

Call the third `<old-hash>`, and list what must be rolled, in order:

```
kubectl get servergroups,proxygroups -n <ns>
```

## Step 2 — rotate the value

```
kubectl patch secret <secret> -n <ns> --type merge \
  -p '{"stringData":{"secret":"<new value>"}}'
```

The bytes are digested untrimmed: a trailing newline is a different secret and
a rotation of its own.

## Step 3 — confirm the operator saw it

The Network controller re-reads the Secret every five seconds. Expect a new
digest, `True SecretResolved`, and `True RotationPending` naming each stale
group as `role/group=count`.

```
kubectl get network <net> -n <ns> -o jsonpath='{.status.forwardingSecretHash}{"\n"}'
kubectl get network <net> -n <ns> -o jsonpath='{range .status.conditions[?(@.type=="ForwardingSecretResolved")]}{.status} {.reason} {.message}{"\n"}{end}'
kubectl get network <net> -n <ns> -o jsonpath='{range .status.conditions[?(@.type=="ForwardingSecretRotationPending")]}{.status} {.reason} {.message}{"\n"}{end}'
```

Where no pod is stamped yet it reads `Unknown PodsPredateTracking` instead,
and the digest is the confirmation. The event fires once on the transition:

```
kubectl get events -n <ns> \
  --field-selector involvedObject.kind=Network,involvedObject.name=<net>,reason=ForwardingSecretRotated
```

`False SecretKeyMissing` means step 2 wrote the wrong key or an empty value.
Fix it before going on: a pod created meanwhile wears the previous digest and
hangs in `ContainerCreating` with no `secret` key to mount.

## Step 4 — roll the server groups, one group at a time

Read the two warnings below first. Proxies first would throw every player out
at once into a network where no backend is reachable; servers first keeps the
proxies up and leaves one hard cut at the end.

```
kubectl get pods -n <ns> \
  -l spawnery.cloud/network=<net>,spawnery.cloud/role=server,spawnery.cloud/group=<group> \
  -L spawnery.cloud/forwarding-hash

kubectl delete pod <pod> -n <ns>
```

Deleting the pod rolls the server: it reaches `Terminating` through `PodLost`
and is replaced — a persistent group's replacement keeping the ordinal and
therefore the claim, so the world comes back.

## Step 5 — verify the group before moving to the next

Select on the old digest, where an empty answer means the group is done, then
check that the replacements carry the new one:

```
kubectl get pods -n <ns> \
  -l spawnery.cloud/network=<net>,spawnery.cloud/role=server,spawnery.cloud/group=<group>,spawnery.cloud/forwarding-hash=<old-hash> \
  -o name

kubectl get pods -n <ns> \
  -l spawnery.cloud/network=<net>,spawnery.cloud/role=server,spawnery.cloud/group=<group> \
  -L spawnery.cloud/forwarding-hash
```

**Both selectors pin `role=server`, and that is not redundant:** the two Kinds
may share a name, so without it a same-named proxy group's pods answer too and
the loop comes back empty only once the proxies are rolled — which is last.

Repeat until no server pod is on `<old-hash>` or unstamped.

## Step 6 — roll the proxy groups

Same shape, `role=proxy`. The hard cut starts at the end of step 5, not at the
first `delete` here: once the server groups have crossed over, a proxy still on
the old secret reaches no backend. Work through them without pausing.

```
kubectl get pods -n <ns> \
  -l spawnery.cloud/network=<net>,spawnery.cloud/role=proxy,spawnery.cloud/group=<group> \
  -L spawnery.cloud/forwarding-hash

kubectl delete pod <pod> -n <ns>
```

## Step 7 — confirm the rotation is complete

```
kubectl get network <net> -n <ns> -o jsonpath='{range .status.conditions[?(@.type=="ForwardingSecretRotationPending")]}{.status} {.reason} {.message}{"\n"}{end}'
```

Expected: `False ForwardingSecretInSync every pod of this network runs on the
current forwarding secret`.

**`Unknown PodsPredateTracking` here means the rotation is not finished.** An
unstamped pod was never rolled, and step 5 cannot see it: that check selects
on `forwarding-hash=<old-hash>`, which an unlabelled pod never matches. Find
them by their empty column, and roll them server pods first:

```
kubectl get pods -n <ns> -l spawnery.cloud/network=<net> \
  -L spawnery.cloud/role -L spawnery.cloud/group -L spawnery.cloud/forwarding-hash
```

## Two warnings

**`kubectl delete pod` bypasses the PodDisruptionBudget, and the players on
that pod are disconnected.** The PDB refuses the *eviction API* for a pod
labelled `spawnery.cloud/occupied`, but a direct delete is not an eviction,
and there is no drain on this path. **A rotation is a maintenance window:**
announce it, and do not reach for a drain to soften it — mid-rotation a drain
moves players onto fallback backends the proxy, still on the old secret,
cannot reach.

**The one-ordinal budget does not bind a human.** *At most one ordinal of a
persistent group is down at a time* constrains the **operator's** takedowns,
not what a person may delete: deleting every pod of such a group takes every
world offline at once. Delete one ordinal's pod, wait for its replacement to
be `Ready` **and** carrying the new digest, then the next.

## Rollback

Write the old value back, exactly as recorded in step 1, then roll back what
has already been rolled.

```
kubectl patch secret <secret> -n <ns> --type merge \
  -p '{"stringData":{"secret":"<the value from step 1>"}}'
```

The same bytes restore the same digest, so the pods already rolled become the
stale ones, named the same way and rolled back in the same order; the rest
need no action.

## `ForwardingSecretRotationPending`

Negative polarity: `True` is the problem; precedence runs down the table,
unreadable before stale before unstamped.

| Status | Reason | What it means | What to do |
|---|---|---|---|
| `True` | `RotationPending` | at least one pod runs on a digest other than the current one; the message names each as `role/group=count` | run this guide: server groups first (step 4), then proxy groups (step 6) |
| `False` | `ForwardingSecretInSync` | every pod carries the current digest and none is unstamped. A network with no pods at all reads this too, vacuously | nothing |
| `Unknown` | `PodsPredateTracking` | no pod is stale, but at least one carries no stamp — see below | **outside a rotation:** nothing, it clears as pods turn over. **During one (step 7):** the unstamped pods have not been rolled — find them by the empty column of the progress command and roll them |
| `Unknown` | `SecretUnresolved` | the secret could not be read, so no comparison is possible; the message carries the `ForwardingSecretResolved` message inside it | fix the read first — the table below |

## `ForwardingSecretResolved`

Positive polarity: `True` is healthy.

| Status | Reason | What it means | What to do |
|---|---|---|---|
| `True` | `SecretResolved` | the Secret exists and its `secret` key holds a non-empty value | nothing |
| `False` | `SecretNotFound` | the `GET` returned NotFound: `spec.forwardingSecretRef` names a Secret that does not exist in this namespace | fix the name, or create the Secret. Pods of this network hang in `ContainerCreating` until it exists, because the projected volume cannot mount |
| `False` | `SecretKeyMissing` | the Secret exists but has no `secret` key, or an empty one | put the forwarding secret under the key `secret` |
| `Unknown` | `SecretReadForbidden` | the `GET` was denied: the reader Role was never applied to this namespace. Forbidden arrives before the operator can learn whether the Secret exists, so it may be present and pods may still start, or it may be missing and they hang in `ContainerCreating` — see the note below before rotating in this state | `kubectl apply -n <ns> -f config/rbac/forwarding-secret-reader.yaml` |
| `Unknown` | `SecretReadFailed` | any other error — the API server was unreachable, for instance. Nobody's typo and nothing to edit. Same caveat as the row above, and the kubelet projects the Secret through that same API server, so an unreachable one stops pods starting for its own reason | look at the message and at the operator's logs; it clears when the read succeeds |

**Where the stamp misleads — and fix the read first.** The stamp is the
operator's last successful read, never revised, while the kubelet projects
whatever the Secret holds. A pod created between the Secret changing and the
digest being recorded — or under the last two reasons, where it starts
normally — runs the new value under the old stamp and reads stale although it
is current; rolling it before the read recovers only mis-stamps it again. A
container restarting after a rotation likewise comes up on the refreshed file
(`RestartPolicy` is `Always`, the secret projected without a `subPath`) under
a pod keeping its first stamp.

`SecretNotFound` is also an event:

```
kubectl get events -n <ns> \
  --field-selector involvedObject.kind=Network,involvedObject.name=<net>,reason=ForwardingSecretNotFound
```
