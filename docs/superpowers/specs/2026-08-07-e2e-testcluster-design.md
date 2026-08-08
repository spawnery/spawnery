# Design: A reproducible E2E test cluster and proof of RBAC

**Date:** 2026-08-07
**Status:** Draft for approval
**Scope:** Test infrastructure for Spawnery on two levels — a permission table
in envtest that runs on every commit, and a NixOS VM test with RKE2 that watches
the operator work under its own ServiceAccount. Each level gets its own
implementation plan; level A first.

## 1. Purpose

Milestone 1 leaves a gap: the controller tests talk to envtest's admin
kubeconfig, so nothing in them ever runs under the operator's ServiceAccount. A
missing verb in the generated ClusterRole goes unnoticed there — it only shows
up the first time the operator runs in a real cluster under its own permissions.
The final review of milestone 1 marked this unprovable and, in the same pass,
found seven verbs granted for no reason that nobody would have caught.

**Correcting an earlier assumption.** This document used to claim envtest runs
with admin rights and can therefore say nothing about permissions. That holds
for the *client* in the controller tests, not for the *authorizer*: envtest
starts the API server with `--authorization-mode=RBAC`, and `SubjectAccessReview`
gives real answers there. Verified empirically — denied without a role, allowed
after binding, a verb that was never granted denied, all in under two seconds.

That is where this document's two-level split comes from.

### 1.1 Two levels

**Level A — the permission table in envtest.** Checking that the ClusterRole
grants everything the code needs and nothing beyond it does not need a cluster.
It runs inside the existing envtest suite on **every commit**, in seconds. A
missing or superfluous verb shows up as it is introduced.

**Level B — the operator in a real cluster.** What envtest cannot do: have an
operator process talk to a real API server under its ServiceAccount, walk its
code paths, and produce no `Forbidden`. That is what the VM is for.

The levels are planned and built separately; level A first, because it pays off
immediately and makes level B smaller.

**Success criterion for level A:** `make test` fails as soon as a verb is
missing from the ClusterRole or one is granted too many.

**Success criterion for level B:** A run of `make e2e` shows the operator
working in the cluster without a single denied request.

### What this split is not

It does not replace the end-to-end scenarios in §10 of the operator design.
Those all assume real Paper and Velocity processes and start with milestone 3.
This infrastructure is the foundation they grow into later.

## 2. Why a VM and not a local cluster

`kind` and `k3d` need a container runtime. The development machine (NixOS) has
none, and setting one up would mean changing the system configuration and
building up state that looks different for the next person.

`pkgs.testers.runNixOSTest` instead boots a VM under QEMU/KVM whose entire
contents are pinned through `flake.lock`. The same invocation produces the same
cluster for every developer and in CI. No daemon, no change to the system
configuration, no mutable state between runs.

**RKE2 rather than k3s**, even though the RBAC proof would be identical on
either: RKE2 is the target platform from the operator design, and the quirks
described there (the CIS profile forcing cluster-wide `restricted` pod security,
the CNI dependency of the HostPort strategy) surface earlier this way instead of
in milestone 6. The price is boot time and memory.

## 3. Parts

Level A needs only the deployment manifests from 3.1 and an ordinary Go test.
Everything else in this section and in section 4 belongs to level B.

Three new flake outputs:

| Output | Contents |
|---|---|
| `packages.operator-image` | `dockerTools.buildLayeredImage` over the existing operator binary. No daemon, bit-reproducible. |
| `packages.e2e-probe` | A Go binary holding the assertions. Imports the same constants the operator does. |
| `checks.e2e-rbac` | `testers.runNixOSTest` — the VM that hands both in and runs them. |

Moved out into `nix/operator-image.nix` and `nix/e2e-rbac.nix` so `flake.nix`
stays readable.

### 3.1 Deployment manifests

Milestone 1 produces the ClusterRole and nothing else. ServiceAccount,
ClusterRoleBinding and Deployment belong to the Helm chart from milestone 6 and
do not exist yet — without them the operator never runs under a ServiceAccount
at all, and RBAC is unprovable in principle.

This split therefore pulls a thin slice out of milestone 6: four manifests under
`config/deploy/` — the namespace `spawnery-system`, the ServiceAccount
`spawnery-operator` inside it, a ClusterRoleBinding onto the generated
ClusterRole, and a Deployment with one replica. No wasted effort — the chart
will template exactly these objects later.

The Deployment sets `--startup-deadline=20s` so the failure path is reachable
within a single test run (see 5.2, scenario 6).

### 3.2 A test manifest, not the example manifest

The test deliberately does **not** apply `config/samples/network.yaml`; it uses
its own manifest under `test/e2e/manifests/`. The reason: scenario 6 needs
`failedRetentionSeconds: 30`, and the example manifest should stay a realistic
starting point for users rather than being bent to fit a test run.

Both use the namespace `minecraft` and the same shape — one network, one
ephemeral group. A separate test case additionally checks that
`config/samples/network.yaml` is accepted by the API server, so the example
cannot rot unnoticed.

## 4. How the VM test runs

The testScript restricts itself to plumbing; the probe makes the claims.

1. Wait for `rke2-server.service`.
2. Wait for a `Ready` node.
3. Import the operator image into the containerd namespace `k8s.io`.
4. Apply the CRDs, the deployment manifests and the test manifest.
5. Wait until the operator deployment reports `Available`.
6. `machine.succeed("/bin/e2e-probe")`.

**No fixed wait anywhere.** Every waiting point is tied to a condition with a
deadline. A VM test built on `sleep` turns flaky under load, and a flaky E2E
test is ignored within weeks.

If the probe fails, the testScript dumps the operator logs,
`kubectl get networks,servergroups,servers,pods -A` and the events.

## 5. The checks

Both levels ask about the permissions of a *third-party* subject — the
operator's ServiceAccount — through `SubjectAccessReview`. That way the checker
needs no ServiceAccount token and can still read logs and events, which it could
not do with the operator's own permissions. (`SelfSubjectAccessReview` checks
the caller's permissions and would be the wrong tool here.)

### 5.1 Level A: the permission table, in both directions

Runs in the envtest suite, not in the VM. The test applies the generated
ClusterRole and the manifests from `config/deploy/` into envtest and derives the
subject to check **from the ClusterRoleBinding and the Deployment** instead of
restating it. Level A therefore also covers that the binding points at the right
role and that the Deployment uses the right ServiceAccount — three sources of
error instead of one.

```go
type Permission struct {
    Group, Resource, Subresource, Verb string
    Why string   // which place in the code needs it
}
```

The table is **maintained by hand** and not derived from the kubebuilder
markers. A derived table would only check that the role grants what the role
grants. Maintained by hand, it is the independent statement "this is what the
code needs".

The `Why` field names the call site, so that removing a code path makes it
obvious that the entry goes with it.

**Nothing missing.** One `SubjectAccessReview` per entry against
`system:serviceaccount:spawnery-system:spawnery-operator`. Namespaced resources
are checked in the namespace `minecraft`, cluster-scoped ones without a
namespace. Every denial is a failure that names the triple in plain text.

**Nothing extra.** Read the ClusterRole from the cluster, expand its rules into
triples, and check that each one appears in the table. A `*` in group, resource
or verb counts as over-granting by definition and fails.

Add a marker without maintaining the table and you get a red test. That is
intended.

**What level A cannot do.** It checks that role and table agree — not that the
table is complete. If a permission is missing from *both*, the test stays green
and the operator still walks into a `Forbidden`. That is precisely why level B
remains necessary: there a real process talks under its ServiceAccount, and
every gap announces itself. Level A prevents drift, level B proves
completeness.

### 5.2 Level B: driven scenarios in the cluster

Reachable without a Paper image:

1. Apply the test manifest → network accepted, group accepted, one `Server`, one
   pod. The pod stays in `ErrImagePull` — that is the expected end state of
   milestone 1, not a failure.
2. Raise `minReplicas` → further `Server`s and pods appear.
3. Lower `minReplicas` → the surplus `Server`s disappear.
4. Slip in a foreign pod carrying the managed labels but with no `Server` object
   → the orphan sweep deletes it.
5. Delete the `Server` → the finalizer is released, the object disappears.
6. With `--startup-deadline=20s` on the Deployment and `failedRetentionSeconds:
   30` in the manifest, a server runs from `Failed` to `Terminating` within a
   minute. That also makes the operator deleting a pod reachable.

Afterwards read the operator logs through the API and fail on every `forbidden`,
quoting the line verbatim.

### 5.3 What stays unproven even then

Patching the occupied label. It requires a server to have been `Ready` once, and
that needs an image with the SLP health tool from milestone 2. The table check
covers the verb, the scenario does not.

## 6. The checker is itself checked

Expanding the ClusterRole rules and comparing them against the table are pure
functions and get ordinary Go unit tests without a cluster.

**The acceptance criterion is mutation, not a green run.** For level A:

- remove a verb from the markers → failure naming exactly that triple,
- add a superfluous verb → failure in the opposite direction,
- point the ClusterRoleBinding at the wrong ServiceAccount → failure, because
  the derived subject is no longer allowed anything.

For level B: break the orphan sweep → scenario 4 falls over.

In this project, a test that is merely green has proven nothing three times
over.

## 7. Where it fits

Level A runs as an ordinary Go test inside `make test` — it costs seconds.

Level B: `make e2e` calls `nix build .#checks.x86_64-linux.e2e-rbac -L`.
Explicitly **not** part of `make test` or `make all`: the commit loop stays at
around 25 seconds. Wiring it into CI belongs to milestone 6; this split only
takes care not to make that harder.

**Cost:** The first build downloads several gigabytes (NixOS image, RKE2,
containerd). Every run boots a VM with roughly 4 GB of RAM; expect a few
minutes. That is the price of the run being the same for every developer and in
CI.

## 8. Open points for later

- **CI needs KVM.** Without `/dev/kvm` the test runs under QEMU emulation and
  becomes many times slower. Before wiring it into CI in milestone 6, check
  whether the chosen runner offers nested virtualization.
- **The scenarios from spec §10** grow into this same VM from milestone 3
  onwards. The test should be cut so that further checks can be placed beside
  the existing ones without touching them.
- **Pod security.** RKE2 with the CIS profile enforces `restricted`
  cluster-wide. This spec does not enable the profile; as soon as milestone 6
  checks the HostPort strategy, the test has to switch it on and check the
  namespace exception described there along with it.
