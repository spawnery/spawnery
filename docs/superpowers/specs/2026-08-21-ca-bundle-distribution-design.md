# The CA bundle reaches a quiet namespace

## 1. What this is

`docs/known-issues.md` records, under "On the agent channel", that the CA has
no rotation procedure. That entry has two halves, and this design closes the
second one:

> Even then a new CA does not reach every namespace immediately:
> `Bootstrapper.Ensure` only runs before the server controller creates a pod.
> An existing namespace where no new pod is created keeps the old `ca.crt` in
> its ConfigMap until the next pod appears there.

The first half — that nothing ever issues a second CA — is the rotation itself,
and it is deliberately **not** in this design. It is separated because a
rotation cannot be built on a distribution that never reaches a quiet
namespace: its overlap window closes on the question "does every agent have the
new bundle yet?", and today that question is undecidable for any namespace
where nothing happens to be starting.

So this is one change with an independent purpose, and a prerequisite for the
next.

**What it does not do**, stated first: it issues no second CA, changes no
certificate lifetime, and adds no rotation mechanism. After it, a namespace's
`ca.crt` tracks whatever bundle the operator currently holds. Since the
operator holds exactly one CA for ten years, nothing observable changes on a
running cluster today. What changes is that the guarantee exists to build on.

## 2. What is already right, and stays

Three properties of the current code carry decisions worth naming, because the
obvious refactor would break each of them.

**`Bootstrapper.Ensure` is already idempotent and already refreshes.** Its own
doc says it is "safe to call on every reconcile", and `ensureConfigMap` writes
the current CA through `controllerutil.CreateOrUpdate` on every call, so an
out-of-date `ca.crt` is corrected by the next call that happens. The mechanism
is not the problem. **The call site is** — `server_controller.go:304`, on the
path that creates a pod.

**The CA ConfigMap and the ServiceAccounts carry no `OwnerReference`, on
purpose.** From `Ensure`'s doc: "they are meant to outlive the operator, so a
pod restarting during an operator outage still finds a CA to trust and a
ServiceAccount to authenticate with." Making the `Network` own the ConfigMap
would be the tidy-looking change and would delete a running fleet's trust
anchor the moment somebody deleted a `Network`. It is not made here.

**The `Network` already owns its namespace**, and `pickNamespaceOwner` decides
which one when two exist. That rule is what makes a per-`Network` call site
correct rather than merely convenient: the losing `Network` in a two-`Network`
namespace must not write, and the existing acceptance logic already says which
one loses.

## 3. The change

`NetworkReconciler` gains a `Bootstrap *Bootstrapper` field and calls
`Bootstrap.Ensure(ctx, network.Namespace)` once per reconcile, **after** the
`Accepted` condition has been set to true and before
`reconcileNetworkPolicy`.

After acceptance, because a `Network` that lost its namespace to an older one
must write nothing into it — the same reason `reconcileNetworkPolicy` sits
where it does. Before the policy, because the CA is what a pod needs in order
to work at all and the policy governs pods that already exist; the order
matters to nothing else and this is the order that reads correctly.

`NetworkReconciler` is wired with the same `Bootstrapper` instance the
`ServerReconciler` already has — `internal/controller/setup.go` builds one and
hands it out; a second instance would be a second `CA func() []byte` closure
over the same provider and no clearer for it.

`SetupAll` already refuses a nil `Bootstrapper`, and its message says "the
server controller cannot create pods without one"
(`setup.go:85`). After this change that sentence is half true, and the guard
covers a second controller: the message says so. This is a one-line edit and it
is called out because a refusal that names the wrong reason is worse than one
that names none — somebody debugging a nil `Bootstrapper` would go looking at
the wrong controller.

**The Server controller keeps its call.** A namespace reaches its first pod
through `ServerReconciler`, and that path must not depend on a `Network`
reconcile having run first. Two call sites for an idempotent function is the
correct shape here, not duplication: one guarantees the bundle is present
before a pod needs it, the other guarantees it stays current afterwards.

## 4. Failure, and the case that will actually happen

`Ensure` refuses when `CA()` returns an empty bundle — the operator has
started but `certs.Provider` has not published yet. That is a real state, it
lasts seconds, and it is not the `Network`'s fault.

The error is returned rather than swallowed, and the reconcile fails and
requeues. Three reasons: `ServerReconciler` already behaves exactly this way on
the same call, so the two paths do not disagree about what an unavailable CA
means; the failure is visible in the controller's error metric and its log
rather than being invisible; and a swallowed error here would leave a
ConfigMap silently stale, which is the defect this whole design exists to
remove.

What it costs: a `Network` reconciled during those seconds records no status
update for that pass. The next pass is 5 seconds later
(`resyncInterval`), and controller-runtime's backoff is shorter than that on
the first retries.

## 5. What it costs when nothing is wrong

Every `Network` is reconciled every `resyncInterval` — 5 seconds — so `Ensure`
now runs that often per `Network`. Each run is, against the manager's cached
client, one `Get` of a ConfigMap and two of ServiceAccounts, and a write only
when a field differs. The cache is already restricted to objects carrying
`podspec.LabelManagedBy` and `Ensure` labels everything it writes, so all three
reads are served from memory.

This is stated rather than measured because it is arithmetic on operations the
process already performs; §6 measures the thing that is not arithmetic.

## 6. How it is proven

**The test that matters fails today.** In envtest, with a `Network` created and
reconciled and **no pod created anywhere**: change what the provider's `CA()`
returns, reconcile the `Network` again, and read the namespace's
`spawnery-ca` ConfigMap back from the API server. Its `ca.crt` must be the new
bundle. Against the current code the ConfigMap keeps the old one — or does not
exist at all, since nothing created it — and that failure is the point of the
test.

Three more, each for a decision §2 and §3 record:

- **A losing `Network` writes nothing.** Two `Network` objects in one
  namespace: the older one's reconcile creates the ConfigMap; the younger one's
  reconcile must not create or modify it. Asserted by deleting the ConfigMap
  after the first reconcile and confirming the loser's reconcile leaves it
  absent.
- **The ConfigMap still carries no `OwnerReference`.** A direct assertion on
  the stored object, because the tidy-looking refactor this design refuses is
  exactly what a later reader might add.
- **An empty CA fails the reconcile rather than passing quietly.** With the
  provider returning nothing, `Reconcile` returns an error and the ConfigMap is
  not created.

**No end-to-end scenario.** `hack/e2e.sh` creates pods, so its namespaces are
never quiet, and a scenario there would exercise the path that already works.
The claim being made is about a namespace where nothing happens, and envtest is
where that is expressible.

## 7. Acceptance criteria

1. A `Network`'s namespace holds a `spawnery-ca` ConfigMap matching the
   operator's current bundle after a reconcile, with no pod having been
   created in that namespace.
2. Changing the operator's bundle changes that ConfigMap within one
   `resyncInterval`, still with no pod involved.
3. A `Network` that does not own its namespace writes nothing into it.
4. The ConfigMap and the ServiceAccounts still carry no `OwnerReference`.
5. A reconcile that runs before the CA is available fails and requeues rather
   than silently skipping.
6. `ServerReconciler` still calls `Ensure` on the path that creates the first
   pod, and its behaviour is unchanged.
7. `docs/known-issues.md`'s "The CA has no rotation procedure" entry loses its
   second half and keeps its first, which remains true.
