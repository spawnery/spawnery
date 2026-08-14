# Milestone 4c-1: the proxy readiness contract, and the first drain that uses it

Status: written 2026-08-14, at the start of the milestone, against `25983b0`
(the merge of 4d).

Companion documents: `docs/superpowers/specs/2026-08-07-minecraft-cloud-operator-design.md`
§6.6 is the requirement; `docs/handover-milestone-4.md`'s "The one contract
change milestone 4 has to make" is what this answers, and this design departs
from it — see §3.1.

## 1. Scope, and why 4c is three milestones

`docs/handover-milestone-4.md` names 4c as "proxy drain and node drain". Read
against the tree, that is three pieces, and only two of them are related:

| | Contents | Depends on |
|---|---|---|
| **4c-1** | The readiness contract, and draining a proxy before deleting it — **this document** | nothing |
| **4c-2** | Proxy rolling updates: surge, one-at-a-time replacement, choosing which proxy goes, and `ProxyGroupReconciler.pods()`'s missing expectations | 4c-1 |
| **4c-3** | Node drain: `unschedulable` nodes start the existing server drain | nothing |

4c-3 is independent of both: it drains *servers*, which has worked since 4b,
and nothing in the operator reads `Node` objects today. It is listed here only
so that splitting 4c does not lose it.

This document is 4c-1. It is the piece that carries the risk, because it is the
first change since milestone 3c to cross into the proto, the Kotlin agent and
the image — `make agent`, `make agent-test`, `make image-test` and
`make image-repro` come back into the path after four operator-only
milestones.

## 2. What this closes

`ProxyGroupReconciler.reconcileReplicas` removes a surplus proxy like this:

```go
for i := len(pods) - 1; i >= int(group.Spec.Replicas); i-- {
	if err := r.Delete(ctx, &pods[i]); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
}
```

There is no drain, no readiness step and no player check. **Lowering a
`ProxyGroup`'s `spec.replicas` disconnects everyone on the removed proxy in
the same instant.** Master design §6.6 describes the opposite: the old proxy
goes `NotReady`, disappears from the endpoints, and existing connections run
out until it is empty or `drain.timeoutSeconds` expires.

`ProxyGroupSpec.Drain` already exists for this, documented as "bounds how long
existing sessions may run out on proxy replacement", and nothing reads it.

## 3. Decisions

### 3.1 The registry is not touched, and the handover is wrong about that

The handover says the fix "needs a way to carry 'ready' separately from
'connected'" in `internal/agent/registry.go`. Measured against the tree, that
is not what proxy drain needs:

- `Snapshot.Ready` has exactly **one** reader in the repository,
  `internal/controller/server_controller.go:381` (`in.AgentReady = snap.Ready`),
  and that is the server state machine.
- `ProxyGroupReconciler` touches the registry only for player counts
  (`proxygroup_controller.go:345`). It decides a proxy's readiness from the
  **pod condition** (`isPodReady`), which the kubelet drives by probing the
  agent's ready-gate port.

So a proxy's readiness already lives where the routing decision is made: the
`Service` drops an endpoint when the pod goes `NotReady`, and established TCP
connections survive that, because Kubernetes does not close connections when
an endpoint is removed. "Connected, but no longer ready" is a state the system
can already express — the operator simply has no way to *ask* for it.

**Adding a second copy of that truth to the registry would be the shape this
repository has already paid for once.** `candidates.go` records at length what
two implementations of one occupancy rule cost when they drifted. The pod
condition is the one the `Service` obeys; a registry bit that disagreed with it
would be a bit nobody could act on.

The handover's conclusion — that this is "a milestone 2a change" spanning the
registry, the agent server and the agent — is therefore narrower than it
looks. What is left is one message and one `when` branch.

### 3.2 The message carries a state, not an event

A new `OperatorToProxy` case, `SetReady { bool ready = 1; }`. The operator
asserts what the proxy's readiness *should be*; the agent maps it onto
`ReadyGate.open()` and `close()`, both of which already exist and are already
guarded against the race between the accept loop and shutdown.

**State rather than event is this repository's own rule, stated twice.**
`Hello`'s comment says of readiness: "Readiness is a state, not an event, so it
is repeated on every connect." `FullSync` exists so that "a reconnect during a
drain cannot undo a deregistration". A one-shot `Drain` message would break
both: a proxy that reconnected mid-drain would come back ready, and an operator
that crashed between asking and deleting would leave a pod stuck `NotReady`
with no way back — holding a replica slot forever, because
`reconcileReplicas` counts pods rather than ready pods.

Being a state also makes the drain **reversible**, which is a real requirement
rather than symmetry for its own sake: a scale-down that is cancelled must
leave a working proxy behind, not a corpse. This is deliberately *unlike* the
server side, where `Retiring` has no path back to `Ready` — a `Server` on its
way out is replaced, while a proxy on its way out may simply be kept.

### 3.3 The desired state is derived every pass, not remembered

`reconcileReplicas` already computes which pods are surplus. It asserts "not
ready" for those and "ready" for the rest, on every reconcile. Nothing is
stored, so an operator restart recomputes the same answer, and a cancelled
scale-down corrects itself on the next pass without anything having to be
cleaned up.

`internal/proxyreg` keeps a per-session memo so the same assertion is not
re-sent every five seconds. The memo is per *stream*, so a reconnect clears it
and the state is re-asserted — which is exactly the behaviour §3.2 wants, for
free.

### 3.4 Only the deadline is stored, and it lives on the pod

The wait needs a start time, and that is the one thing that cannot be derived.
It goes in an annotation on the proxy pod — `spawnery.cloud/draining-since`,
an RFC 3339 timestamp — written when the operator first asks that pod to go
not-ready, read to bound the wait, and gone when the pod is.

A `Server` keeps this in `status.drainStartedAt`, but a proxy pod has no CR of
its own; the `ProxyGroup`'s status is per group, not per pod. The annotation is
the only per-pod place that survives an operator restart, which §3.2's crash
case requires.

### 3.5 The wait is for *empty*, not for `NotReady`

`NotReady` stops the inflow; it says nothing about the players already
connected. So the sequence is: assert not-ready → the endpoint disappears →
wait until the proxy reports zero players → delete.

**Nobody is moved, and that is not an omission.** A server drain moves players
to another backend because the client's connection terminates at the *proxy*,
which stays. A proxy drain has no such option: the connection terminates at
the proxy being removed, and there is no elsewhere to put it without
disconnecting the client. Master design §6.6 says the same: "Unlike the server
drain there is no active moving."

That makes the deadline sharper here than on the server side. When
`spec.drain.timeoutSeconds` expires with players still connected, the pod is
deleted and those players are disconnected. It is the only path in this
milestone that disconnects anyone, it is configured rather than accidental, and
it emits a `Warning` event naming how many were affected so it is provable
afterwards.

The player count is already available: `proxygroup_controller.go:345` reads
`r.Agents.Lookup(podUID).Players` for the group status. "Is this proxy empty"
is not new information — only a decision nobody makes with it yet.

### 3.6 The drain timeout defaults to 300 seconds

`ProxyGroupSpec.Drain` is optional, so a default is needed. The server side
uses 60 seconds, but there the players are *actively moved*, which is quick.
Here the operator waits for people to leave on their own, and a minute
practically guarantees that whoever is left gets disconnected.

Five minutes is long enough that a scale-down in a quiet period completes
without kicks, and short enough that a deploy does not hang indefinitely.
**There is no honest default here** — a play session runs to tens of minutes,
so every number short of that disconnects somebody — which is why the
documentation says plainly that an operator who cares about this should set
the field.

### 3.7 Which proxy is removed does not change

The existing selection — from the end of the pod list — stays. Preferring the
emptiest proxy is a good idea and belongs to 4c-2, where the replica count
really moves; here it would widen the diff past the contract this milestone is
about.

Likewise, when several proxies are surplus at once they all drain at once.
That is what the operator asked for by lowering `spec.replicas`. Master design
§6.6's "one at a time" is about *replacement* during a rolling update, which is
4c-2's.

## 4. Components

### 4.1 `proto/spawnery/agent/v1alpha1/agent.proto`

```protobuf
// SetReady is the operator asserting whether this proxy should be taking new
// connections. It is a state and not an event: the operator re-sends it
// whenever it syncs, so a reconnect cannot leave a proxy stuck in the wrong
// one, and a cancelled drain simply reverts.
message SetReady {
  bool ready = 1;
}
```

plus `SetReady set_ready = 7;` in `OperatorToProxy`'s oneof. Purely additive,
so no runtime version has to move — but `make proto` regenerates the Go and the
Java stubs, which changes the agent jar and therefore the image.

### 4.2 `internal/proxyreg/fleet.go`

`func (f *Fleet) SetReady(ctx context.Context, podUID string, ready bool) error`
— one session, keyed by pod UID. `f.sessions` is already keyed that way; only
the entry point is missing.

**It must not be called `Drain`**: that name is taken by the *server* drain,
which moves players off a backend, and reusing it would put two unrelated
meanings on one verb in one file. `SetReady` also matches the proto message,
so the wire name and the call site read the same.

Each session carries the last readiness it was sent, so the assertion is not
repeated every five seconds. The memo belongs to the session, so a new stream
starts without one and the state is re-asserted on reconnect.

### 4.3 `agent/velocity/.../ProxyRole.kt`

One more branch in the `when (message.messageCase)` at `:126`, beside
`fullSync`, `registerServer`, `unregisterServer` and `drainPlayers`, mapping
onto `gate.open()` / `gate.close()`. `ReadyGate` itself does not change.

### 4.4 `internal/controller/proxygroup_controller.go`

`reconcileReplicas` stops deleting surplus pods outright:

- assert the desired readiness for every pod, surplus or not (§3.3);
- for a surplus pod without the annotation, write it with the current time;
- delete a surplus pod when its player count is zero, or when
  `spec.drain.timeoutSeconds` has elapsed since the annotation — the latter
  with a `Warning` event naming the players lost;
- for a pod that is no longer surplus, remove the annotation.

A `ProxyGroup.DrainTimeout()` accessor mirrors `ServerGroup`'s, defaulting to
300 seconds when `spec.drain` is absent (§3.6).

### 4.5 Not in this milestone

`ProxyGroupReconciler.pods()`'s missing expectations. `docs/known-issues.md`
assigns them to 4c, and they belong to 4c-2: they concern the *create* path
racing the informer cache, which this change neither causes nor worsens.

## 5. Data flow

A `ProxyGroup` at `replicas: 2`. Someone sets it to 1. Three players are on
proxy `B`, which is the one the existing selection picks.

| Pass | What happens | State |
|---|---|---|
| 1 | `B` is surplus | the operator asserts not-ready, writes the annotation, **does not delete** |
| — | the agent closes its gate | the readiness probe starts failing |
| n | the pod condition turns `NotReady` | the `Service` drops the endpoint — **no new connections reach `B`** |
| … | the three keep playing | their TCP connections are established and Kubernetes does not close them |
| … | they leave one by one | the reported player count falls |
| at 0 | `B` is empty | **now** it is deleted |

If the deadline arrives first, `B` is deleted anyway and its remaining players
are disconnected, with a `Warning` event naming how many.

## 6. Error handling

- **An agent that predates the message.** An unknown `oneof` field is ignored,
  so the gate never closes, the pod stays `Ready`, and the deadline fires with
  players still connected. This is the milestone's one upgrade-ordering hazard:
  a new operator against an old image. The deadline bounds it and the event
  names it, and `known-issues.md` records that images are upgraded before the
  operator.
- **The proxy crashes mid-drain.** The pod goes `NotReady` regardless, its
  players are gone with it, the count reads zero and the deletion proceeds
  normally.
- **The operator restarts mid-drain.** The desired state is re-derived (§3.3)
  and the deadline is on the pod (§3.4), so the drain continues where it was.
- **The scale-down is cancelled.** The pod stops being surplus, the operator
  asserts ready, the gate reopens and the annotation is removed. This is what
  §3.2's reversibility is for.
- **`spec.drain` is absent.** 300 seconds, per §3.6.
- **Several proxies surplus at once.** They all drain at once (§3.7).

## 7. What this milestone deliberately does not do

- **It does not touch `internal/agent/registry.go`** (§3.1).
- **It does not choose which proxy to remove** (§3.7) — 4c-2's.
- **It does not add expectations to `pods()`** (§4.5) — 4c-2's.
- **It does not do node drain** — 4c-3's, and independent of all of this.
- **It does not move players anywhere**, because there is nowhere to move them
  (§3.5).

## 8. Facts this design asserts about the code already here

Each was read in the tree at `25983b0` rather than remembered:

- `Snapshot.Ready`'s only reader is `server_controller.go:381`.
- `ProxyGroupReconciler` reads the registry only at `:345`, for players, and
  decides readiness from the pod condition via `isPodReady`.
- `reconcileReplicas` deletes surplus pods immediately, with no drain and no
  player check, and counts pods rather than ready pods.
- `ProxyGroupSpec.Drain *DrainSpec` exists, is documented for exactly this, and
  is read by nothing.
- `f.sessions` in `internal/proxyreg/fleet.go` is keyed by pod UID; `Fleet` has
  `Register`, `Deregister` and `Drain`, all broadcasts, and `Drain` means the
  server drain.
- `ProxyRole.kt:126` dispatches `OperatorToProxy` in a `when (message.messageCase)`.
- `ReadyGate` has `open()` and `close()`, both `@Synchronized`, and `close()` is
  reachable today only from `onShutdown`.
- `ReadyGate` takes its port as a parameter so tests can pass 0 and read the
  bound port back.

## 9. Test strategy

- **envtest** carries the operator logic: the assertion, the annotation, the
  wait, the deletion at zero, and the deletion at the deadline with its event.
  Player counts come from the registry, which these tests already populate.
- **`internal/proxyreg`** unit tests: sending to one session, the memo
  suppressing repeats, and — the one that matters — a new stream re-asserting
  the state without being asked.
- **The Velocity agent**: a test that the new branch opens and closes the gate.
  `ReadyGate`'s port parameter exists so this can be done without a proxy.
- **`make agent-test`** is where the contract first crosses the wire. The stub
  operator in `cmd/spawnery-stubop` has to learn to send the message; that is
  real work and it is the honest place for it.
- **`make image-test` and `make image-repro`** return to the path because the
  jar changes.

**Two of the acceptance criteria cannot be proven by any of that.** envtest has
no kubelet, no probes and no kube-proxy, so "the endpoint disappears" and "the
established connection survives" are claims about a real cluster. Milestone 3
met the same wall and answered it with `docs/runbook-milestone-3-evidence.md`
plus a driven session; this milestone owes the same. The runbook is written as
part of the milestone and the run is part of its acceptance.

## 10. Acceptance criteria

1. Scaling a `ProxyGroup` from 2 to 1 with a player connected to the removed
   proxy **does not disconnect them**; the pod is deleted only once empty.
2. The removed proxy leaves the `Service` endpoints **before** it is deleted.
3. After a reconnect the desired readiness is re-asserted without the operator
   being told to.
4. A cancelled scale-down reopens the gate and removes the annotation.
5. The deadline deletes with a `Warning` event naming the players lost.
6. `make agent-test`, `make image-test` and `make image-repro` are green — the
   agent and image path back in service after four operator-only milestones.
7. `make manifests` produces no diff: this milestone adds no CRD field.
8. Criteria 1 and 2 are measured against a real cluster, per §9.

## 11. What this leaves open

- **4c-2**: proxy rolling updates — surge, one-at-a-time replacement, choosing
  the emptiest proxy, and `pods()`'s missing expectations.
- **4c-3**: node drain, which needs none of this.
- **The upgrade ordering** (§6): images before the operator, until something
  version-gates the message.
- **Milestone 4d's own carry-overs**, unchanged: the backoff counts failed
  servers rather than failed attempts, and the `!sized`/`GaveUp` message
  precedence is undecided.
