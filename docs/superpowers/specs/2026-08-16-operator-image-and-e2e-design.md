# Design: milestone 6a — the operator in the cluster: image, registry, and a driven E2E run

**Date:** 2026-08-16
**Status:** Approved
**Scope:** An OCI image for the operator, a published home for all three images,
and an automated run on `kind` that watches the operator work under its own
ServiceAccount and produce no denied request. No game images, no join, no CI, no
Helm chart, no NetworkPolicies, no new expose strategies.

## 1. What this milestone is

Milestone 6 is one line in the master design's §11 table — *"All three expose
strategies, NetworkPolicies, the Helm chart, RKE2 E2E in CI"* — and four
subsystems. This document is the first of five pieces. §1.3 records the cut.

6a's result: the operator runs **inside** a cluster for the first time, under its
own ServiceAccount, from an image that falls out of the flake, and a single
command shows that it walks its code paths without a single `Forbidden`.

### 1.1 Why level A is not enough

`internal/rbacaudit` is level A of
`docs/superpowers/specs/2026-08-07-e2e-testcluster-design.md`, and it has been in
service since milestone 1. It compares the generated ClusterRole against a
hand-maintained table in both directions, so a marker added without a table entry
— or a table entry without a marker — turns `make test` red.

What it cannot do is stated in that document's own §5.1 and repeated in
`docs/known-issues.md:2480`: it checks that role and table **agree**, not that the
table is **complete**. A permission missing from both leaves the suite green and
the operator still walks into a `Forbidden` the first time it runs for real.
Closing that needs a real process, under a real authorizer, exercising real code
paths. That is level B, and level B was never built: `packages.operator-image`,
`packages.e2e-probe`, `checks.e2e-rbac` and `make e2e` are all absent from this
repository.

### 1.2 The second yield: the operator stops running outside the cluster

Today the operator runs through `go run` from a developer's terminal. Because it
sits outside the cluster, no selector can find it, so every local run has to
hand-build a selector-less `Service` and a matching `Endpoints` object, or a
`Server` never leaves `Starting` (`docs/known-issues.md:305`). Under rootless
Podman that is harder than it sounds — the entry records that the `kind`
network's gateway refuses the connection from inside the rootless namespace, and
that the address which does reach the host is rejected by the API server in both
`Endpoints` and `EndpointSlice`.

The README documents a relay container as the workaround that works. Two
known-issues entries say what the durable answer is, in the same words: run the
operator inside the cluster from its own image, "where the Service is a Service".
The second of them — "Whether the operator runs inside the cluster for the E2E
flow is still open" — records that milestone 3c is where the absence "first has
to be worked around a second time rather than once", and calls the hand-built
objects "workable for one person at a terminal, a wall for milestone 6's CI".

6a is that answer. It is not a side effect of this milestone; it is half its
value.

### 1.3 How milestone 6 is cut

| | Piece | Why there |
|---|---|---|
| **6a** | Operator image, registry, driven E2E on `kind` | The foundation. What follows can be shown to *exist* in envtest; that it *works* needs a cluster. |
| **6b** | NetworkPolicies | The one piece with a security consequence, overdue since 3b. 6a is what shows it works rather than merely exists. |
| **6c** | `LoadBalancer` and `HostPort` | Needs node-address discovery, the `node-external-ip` warning, the pod-security warning, and a reachability test per strategy. |
| **6d** | The Helm chart | Last, because it ships what 6b and 6c produce. Written earlier, it is written twice. |
| **6e** | CI | Its own piece: where it runs is a question no code answers. |

At the end of milestone 6 the whole system is rolled out to a real RKE2 cluster
by hand and driven from a runbook, in the manner of milestones 3, 4c-1, 5a, 5b
and 5c. §12 says what that run owes.

### 1.4 What 6a does not touch

No Paper or Velocity image is loaded into the E2E cluster, so no client ever
joins. No NixOS VM. No CI. No Helm chart. No NetworkPolicy. The two refused
expose strategies stay refused
(`internal/controller/proxygroup_controller.go:214`). No controller behaviour
changes: nothing under `internal/` or `api/` moves except test files, and the
only shipped manifest this milestone edits is `config/deploy/deployment.yaml` —
§5 is why.

## 2. What supersedes the 2026-08-07 E2E design, and what survives

That document chose a NixOS VM under QEMU, and its §2 gives one reason:

> `kind` and `k3d` need a container runtime. The development machine (NixOS) has
> none, and setting one up would mean changing the system configuration and
> building up state that looks different for the next person.

That has not been true since milestone 2b. `kind`, `k3d`, `kubectl` and
`kubernetes-helm` are all in the dev shell (`flake.nix`), the README documents
`kind` under `KIND_EXPERIMENTAL_PROVIDER=podman` wrapped in `systemd-run --scope
--user --property=Delegate=yes`, and milestone 5's two evidence runs were driven
on exactly that — `docs/handover-milestone-5.md` records both as single-node
`kind` clusters. The VM's premise is spent.

The VM keeps a *different* argument, and it is a real one: RKE2 is the target
platform, and the CIS profile's cluster-wide `restricted` pod security together
with the CNI dependency of `HostPort` only surface there. This milestone does not
answer that argument with a simulation. It answers it by rolling the finished
system out to an actual RKE2 cluster at the end of milestone 6 and driving a
runbook against it. The consequence is stated rather than buried: **until that
run, the `HostPort` and pod-security branches of 6c are unproven, not merely
untested.**

Of the old document:

- **§5.1 survives entirely.** Level A is built, in service, and unchanged by this
  milestone.
- **§2 is superseded** — the VM, for the reason above.
- **§3 is superseded** — `packages.e2e-probe` and `checks.e2e-rbac` are not built;
  `packages.operator-image` is, and §3 of this document replaces its one-line
  description.
- **§5.2's scenario list is carried forward and extended.** It was written for
  milestone 1 and shows it: its scenario 1 names `ErrImagePull` as the expected
  end state, which was true then and needs saying out loud now. §7.1 is the
  current list.
- **§5.3 is carried forward and grows.** It named one thing the scenarios cannot
  reach; §7.4 names what the list is today.
- **§7 is superseded** in its mechanism (`make e2e` no longer calls `nix build
  .#checks…`) and kept in its rule: `make e2e` is not part of `make test` or
  `make all`.
- **§8's open points are answered or dropped.** "CI needs KVM" is moot — there is
  no VM. "Pod security" moves to the RKE2 rollout. "The scenarios from spec §10
  grow into this same VM" becomes: they grow into this same harness, and §7.1 is
  cut so they can be placed beside the existing ones.

The old document is **not rewritten**. It gets a status header naming which
sections this one supersedes, in keeping with how this project handles
corrections everywhere else: the wrong version stays legible next to the right
one.

## 3. The operator image

### 3.1 The Go derivation

`cmd/spawnery-operator` has no Nix derivation today — `make build` runs `go build`
into `bin/`. This milestone adds one beside the four that already exist
(`spawnery-slp`, `spawnery-stubop`, `spawnery-join`, `spawnery-config`), in the
same shape: `pkgs.buildGoModule`, `src = ./.`, the shared `vendorHash`,
`subPackages = [ "cmd/spawnery-operator" ]`, `env.CGO_ENABLED = 0`, `ldflags =
[ "-s" "-w" ]`.

Static, because the image carries no libc of its own — the same reason the other
four give.

### 3.2 The image frame, and why it does not reuse `oci-common.layeredImage`

`nix/oci-common.nix` describes itself as "what both Spawnery images need
verbatim". Its `layeredImage` frame creates `/data` and `/tmp`, chmods them, and
sets `WorkingDir = "/data"` — all correct for a game server whose world lives
there, all wrong for an operator that runs with `readOnlyRootFilesystem: true`
and writes nothing to disk.

So the operator image takes from `oci-common` the parts that are genuinely
shared — `uid`, `gid`, `passwd` and `group`, so that all three images run as the
same numeric user and `runAsNonRoot` has an entry to find — and calls
`dockerTools.buildLayeredImage` itself, with no `/data`, no `/tmp` and
`WorkingDir = "/"`.

Extending the frame with a `workingDir` parameter and a switch for the data
directory was considered and rejected: it would churn the one file both working
images depend on in order to serve a third with different needs. `oci-common.nix`
gains a sentence saying the third image deliberately does not use the frame, so
the next reader does not take its absence for an oversight.

The image is exposed on **`x86_64-linux` only**, guarded exactly as the other two
are. The reason is in `flake.nix` already: `buildLayeredImage` does not
cross-compile but labels its output `amd64` regardless, and restricting the
attribute is what keeps that label honest — elsewhere `nix build .#operator-image`
fails with "does not provide attribute" rather than quietly producing a
mislabelled image.

### 3.3 Its own version number

`imageVersion = "0.2.0"` in `flake.nix` is the **agent** version. Its comment says
so and says why it lives in one place: it reaches the plugin's `paper-plugin.yml`,
which the agent reports to the operator as `Hello.version`, and it reaches the
image tag, so the two can never drift.

The operator has no part in that. Hanging its tag off `imageVersion` would couple
the operator's releases to the agent's, and a bug fix in the reconciler would
claim a new agent version. This milestone adds `operatorVersion` beside it, with
the reason in the comment.

The published tags are therefore:

| Image | Tag |
|---|---|
| `ghcr.io/spawnery/paper` | `<paper version>-<imageVersion>` (unchanged) |
| `ghcr.io/spawnery/velocity` | `<velocity version>-<imageVersion>` (unchanged) |
| `ghcr.io/spawnery/spawnery-operator` | `<operatorVersion>` |

### 3.4 Reproducibility, and the shared `./result` symlink

The operator image joins `make image-repro`. Bit-reproducibility is an acceptance
criterion of the master design's §5.3 for the game images; nothing about the
operator makes it a weaker claim.

`docs/known-issues.md`'s "Small things" already records that `make -j image-test`
can load the wrong image, because `image` and `velocity-image` both run `nix
build` with no `--out-link` and therefore share `./result`, and it names the fix.
A **third** image makes that collision more likely rather than less, and this
milestone is where the fix stops being optional: `--out-link result-paper`,
`result-velocity`, `result-operator`, with the load and publish steps reading
their own link.

## 4. Publishing

### 4.1 What is published and where

All three images, to `ghcr.io/spawnery/`. The registry is public, so nothing
needs an `imagePullSecret` — neither the E2E, nor `config/deploy/`, nor the RKE2
rollout, nor the Helm chart in 6d.

Publishing all three rather than only the operator follows from §12: a rollout to
RKE2 that only finds the operator in a registry still needs Paper and Velocity
imported onto every node by hand — 724 MB and 735 MB as tarballs, by milestone
2b's own measurement. One publish path, three consumers.

`skopeo` joins the dev shell. `hack/publish.sh` copies each freshly built tarball
straight to the registry — `docker-archive:` to `docker://`, with no local
container store in between, so what is published is what `nix build` just
produced. Two properties it must have:

- it refuses to overwrite an existing tag rather than silently replacing it;
- it prints the digest the registry returns for each image.

Authentication comes from the environment (a GitHub token with `write:packages`),
never from a file in the repository.

### 4.2 The image reference in the deployment manifest

`config/deploy/deployment.yaml` names `ghcr.io/spawnery/spawnery-operator:dev`
today — a tag nothing produces. The master design's §8 requires shipped manifests
to pin by digest, tags being mutable.

So the manifest carries a digest reference, and `hack/publish.sh` writes it
forward after a successful push. That keeps the promise §8 makes and removes the
one reference in this repository that currently points at nothing.

### 4.3 What the E2E does instead, and what that leaves unexercised

The E2E run does **not** pull that digest. It loads the image archive `nix build`
just produced into the `kind` node and overrides the Deployment's image with it.
The reason is that the run exists to test the bits that were just built, not the
bits that happen to be sitting in a registry — a run that pulls would go green
against a stale digest while the working tree was broken.

The price is stated rather than hidden: **the digest reference in
`config/deploy/deployment.yaml` is never exercised by `make e2e`.** The first
thing that fetches it is the RKE2 rollout in §12, and that runbook's first step
is therefore to check that the reference resolves.

## 5. The startup deadline is a test value in a production manifest

`cmd/spawnery-operator/main.go:145` defaults `--startup-deadline` to **five
minutes**. `config/deploy/deployment.yaml` overrides it to **20 seconds**, and
§3.1 of the 2026-08-07 E2E design says why: scenario 6 needs the failure path to
be reachable inside a single test run.

That was sound while `config/deploy/` was test-only scaffolding. It is not sound
now that the same five manifests are what gets rolled out to a real cluster in
§12. Twenty seconds is below what a healthy server takes: milestone 5a's evidence
run measured **24 seconds** from apply to `ReadyGatePassed` on an idle
single-node `kind` cluster with the image already present, which is the
favourable case in every dimension that matters — no image pull, no contention,
no world to read.

The manifest therefore drops the override and takes the production default. The
E2E patches its own copy down to 20 seconds before waiting for the Deployment, so
scenario 6 stays reachable. The test value belongs to the test, not to the
artifact a person installs.

## 6. The harness

### 6.1 Cluster lifecycle

`hack/e2e.sh`, in the shape of `hack/image-test.sh` and `hack/agent-test.sh`:

1. Create a single-node `kind` cluster with a fixed name (`spawnery-e2e`).
2. `kind load image-archive` the operator tarball. Not `kind load docker-image`:
   the archive route needs no container CLI at all, which is why `make e2e`
   requires neither `$(CONTAINER)` nor a running daemon.
3. Apply `config/crd/bases/`, `config/rbac/role.yaml` (a ClusterRole and a
   namespace-local Role in one file) and `config/deploy/` (Namespace,
   ServiceAccount, ClusterRoleBinding, RoleBinding, Deployment, Service).
4. Patch the Deployment's image and `--startup-deadline`.
5. Wait for the Deployment to report `Available`.
6. Apply the test manifest from `test/e2e/manifests/`.
7. Run the assertions.
8. On failure: dump the operator log, `kubectl get networks,servergroups,
   proxygroups,servers,pods,pvc -A`, and the events.
9. Delete the cluster, unless `E2E_KEEP=1`.

The script honours `KIND_EXPERIMENTAL_PROVIDER` from the environment rather than
hard-coding a provider, so the rootless-Podman invocation the README documents
keeps working without the script knowing about it.

A single node, as milestone 5's two evidence runs used. Nothing in 6a's scenario list
needs a second one; node drain, which does, is 4c-3's and is not driven here.

### 6.2 A test manifest, not the example manifest

Carried unchanged from the old design's §3.2: the run uses its own manifest under
`test/e2e/manifests/`, because scenario 6 needs `failedRetentionSeconds: 30` and
`config/samples/network.yaml` should stay a realistic starting point rather than
being bent to fit a test. A separate assertion checks that
`config/samples/network.yaml` is accepted by the API server, so the example
cannot rot unnoticed.

**One thing §4.1 changes about that manifest.** Once Paper and Velocity are
published and public, a group naming `ghcr.io/spawnery/paper:<tag>` is a group
whose image the kubelet can actually pull — 724 MB into a fresh `kind` node on
every run, and a server that then really starts. The test manifest therefore
names an image that deliberately cannot resolve, and says so in a comment. This
is the difference between "the pod stays in `ErrImagePull`" as a *decision* and
as an accident of nothing being published yet, and §7.4 turns on it.

### 6.3 A Go test package, not a probe binary

The old §3 specified `packages.e2e-probe`, a standalone Go binary. That shape was
an answer to the VM: something had to be copied into a machine and run there.
With `kind`, the checker runs on the host against the kubeconfig, and then a Go
**test package** is the better tool — subtests, ordinary failure output, `-run`
to pick out one scenario while debugging it.

So: `test/e2e/`, behind a build tag so `go test ./...` and `make test` never pick
it up, driven by `make e2e`. It imports `internal/rbacaudit` and the operator's
own constants directly, which is what the old design meant by "imports the same
constants the operator does".

### 6.4 No fixed waits

Carried from the old §4, unchanged and non-negotiable: every waiting point hangs
off a condition with a deadline. A run built on `sleep` turns flaky under load,
and a flaky E2E test is ignored within weeks.

## 7. What the run asserts

### 7.1 The nine scenarios

The first six are the old §5.2, brought up to date. The last three could not have
existed when that document was written.

1. Apply the test manifest → the `Network` is accepted, the `ServerGroup` is
   accepted, `Server` objects and pods appear. The pods stay in `ErrImagePull`;
   §7.4 is why that is the expected end state rather than a failure.
2. Raise `minReplicas` → further `Server`s and pods appear.
3. Lower `minReplicas` → the surplus `Server`s disappear.
4. Slip in a foreign pod carrying the managed labels with no `Server` object →
   the orphan sweep deletes it.
5. Delete a `Server` → the finalizer is released and the object disappears.
6. With `--startup-deadline=20s` and `failedRetentionSeconds: 30`, a server runs
   from `Failed` to `Terminating` inside a minute. This is also what makes the
   operator *deleting* a pod reachable.
7. **New.** A `Persistent` group → one `PersistentVolumeClaim` per ordinal, each
   with **no owner reference**; lowering `replicas` removes the top ordinal and
   **the claim outlives it**. That is milestone 5a's load-bearing property, so far
   proven only in envtest and by hand in two evidence runs.
8. **New.** A `ProxyGroup` → the NodePort `Service` is created and carries the
   port the group asked for.
9. **New.** `certs.Ensure` has written its TLS secret in `spawnery-system`, and
   leader election holds its lease. Both are driven by startup alone, and both run
   through the markers that carry `namespace=spawnery-system` as a literal
   (`docs/known-issues.md:2470`) — the permissions most likely to be wrong the
   first time the operator runs anywhere but here.

The list is cut so that further scenarios can be placed beside these without
touching them, which is what the old §8 asks for and what 6b, 6c and the join
scenarios of the master design's §10 will need.

### 7.2 Completeness: no `forbidden`

After the scenarios, read the operator's log through the API and fail on every
occurrence of `forbidden`, quoting the line verbatim.

This is the assertion level A structurally cannot make, and it is the reason this
milestone exists. It is also why the scenarios matter more than their individual
claims: each one is a set of API calls under the operator's own ServiceAccount,
and a missing permission announces itself there.

### 7.3 The table against a real authorizer

One `SubjectAccessReview` per entry in `rbacaudit.RequiredCluster`,
`RequiredNamespaced` and `RequiredNetworkNamespace`, against
`system:serviceaccount:spawnery-system:spawnery-operator` — namespaced resources
in `minecraft`, cluster-scoped ones without a namespace.

Level A already does this in envtest. Doing it again here is cheap and covers a
different failure: that the role as it lands in a real cluster is not the role the
envtest suite applied. As in level A, the subject is derived from the
ClusterRoleBinding and the Deployment rather than restated, so the run also covers
that the binding names the right role and the Deployment the right ServiceAccount.

`SubjectAccessReview` and not `SelfSubjectAccessReview`: the checker asks about a
third party's permissions, which lets it keep its own admin rights and still read
logs and events.

### 7.4 What the scenarios do not reach, and why that is a choice

It is worth being exact about the reason, because 6a changes it.

Before this milestone, a full run was *impossible* locally: the agent inside a
game server dials `spawnery-operator.<ns>.svc:9443`, and with the operator
outside the cluster no selector could resolve that. **After this milestone it is
possible** — the operator runs in the cluster, `config/deploy/service.yaml` is a
Service with a selector, and an agent in a pulled Paper image would reach it. The
stack could go all the way to a join, unattended.

6a declines to, on scope. It publishes the game images (§4.1) but does not load
them, and its test manifest deliberately names an unresolvable one (§6.2), so the
pods stay in `ErrImagePull` by decision. The join scenarios of the master design's
§10 are the natural next tenants of this harness, and §7.1 is cut so they can be
placed beside the nine without touching them.

A second constraint sits behind that choice and would bite anyone tempted to
substitute a small stand-in image: the server pod's readiness probe is an `exec`
of `/usr/local/bin/spawnery-slp` against `127.0.0.1:25565`
(`internal/podspec/server.go:342`, binary path at `:56`). Both the tool and
something answering the server-list ping on that port exist only inside the real
Paper image, so a stand-in would have to carry a pinger and a fake protocol
responder to buy pod readiness — and the `Server` would still stall at the ready
gate's second stage for want of an agent. There is no cheap middle tier; it is
the real images or none.

Out of reach in 6a, in consequence:

- the second stage of the ready gate, which needs a connected agent;
- `syncOccupiedLabel` and the PDB upkeep, which need a server that has been
  `Ready` once — the old §5.3 named exactly this for milestone 1, and it is still
  true;
- growing a claim (`persistentvolumeclaims: patch`), which needs a running
  persistent server;
- everything that is only RKE2: CIS `restricted`, `HostPort` and its CNI
  dependency, several nodes, node drain.

For the permission table this means the verbs behind those paths are covered by
§7.3's direction and by no driven scenario. The spec does not guess which ones
those are: the implementation measures it on the first green run and writes the
list into `docs/known-issues.md`, because a guessed list is exactly the kind of
plausible-sounding claim this project keeps catching in its own documents.

## 8. Tests

Three layers, and only the first runs on every commit.

**Unit, in `make test`.** One addition, and it is `internal/rbacaudit`'s.
`docs/known-issues.md` already records that **the flags in the Deployment are
unchecked**: `sigs.k8s.io/yaml` is not strict, so a mistyped key disappears
silently, and no test guards `--startup-deadline` at all. That was a tolerable
gap while `config/deploy/` was scaffolding. §5 makes it the manifest a person
installs, so this milestone closes it — the same test that already derives the
subject from the ClusterRoleBinding and the Deployment gains an assertion on the
container's flags.

`hack/publish.sh` gets no Go unit test; it is shell, and pretending otherwise
would produce a test of a wrapper rather than of the push. It is covered by
acceptance criteria 7 and 8 and by a dry-run mode that prints what it would copy
where without contacting the registry.

**The driven run, `make e2e`.** Not part of `make test` or `make all`. The commit
loop stays where it is.

**Mutation, and it is the acceptance criterion rather than a green run.** The old
§6 says it in words this project has earned three times over. For 6a:

- remove a verb from the kubebuilder markers, `make manifests`, `make e2e` → red,
  with the triple named;
- point the ClusterRoleBinding at the wrong ServiceAccount → red, because the
  derived subject is allowed nothing;
- break the orphan sweep → scenario 4 falls over;
- revert §5's change so the Deployment carries `--startup-deadline=20s` again →
  the unit check that guards the flag goes red.

Every one of these must be **performed**, not reasoned about, and its output
recorded. A mutation kills only the lines a test actually executes; a single
passing mutation says nothing about a branch the run never enters. That lesson is
milestone 5's, from `TestDeletingAPersistentServerLeavesItsClaim`, and it applies
here with more force because a driven run has many more branches it might quietly
skip.

## 9. Acceptance criteria

1. `nix build .#operator-image` produces a tarball, on `x86_64-linux` only.
2. `make image-repro` rebuilds all three images bit-identically.
3. `make e2e` creates the cluster, the operator Deployment reaches `Available`,
   all nine scenarios pass, the cluster is torn down, exit 0.
4. The operator log contains no `forbidden` across the whole run.
5. Every entry of the three permission tables is allowed by the real authorizer.
6. Removing any one verb from the markers turns `make e2e` red, and the failure
   names the triple.
7. `config/deploy/deployment.yaml` carries the production `--startup-deadline`
   and a digest reference that resolves.
8. All three images are pullable from `ghcr.io/spawnery/` without a pull secret.
9. `make test` is unchanged in what it runs and still green; `make e2e` belongs to
   neither `make test` nor `make all`.

## 10. Documentation this milestone owes

- **A status header on
  `docs/superpowers/specs/2026-08-07-e2e-testcluster-design.md`**, naming §2, §3
  and §7 as superseded by this document and §5.1 as in force. The old text stays.
- **`README.md` is five sub-milestones behind.** Its last commit is `d7aefb0`,
  the milestone 4b handover; 4b, 4c, 4d, 5a, 5b and 5c appear in it nowhere.
  6a brings it up to date — reconstructed from the handovers, `docs/known-issues.md`
  and the specs rather than from memory — and adds what 6a itself changes: `make
  e2e`, the publish path, and that the operator now runs in the cluster.
- **`docs/known-issues.md`** gains a "From milestone 6a" section, and loses or
  amends the entries 6a closes: "No image is published", "The local kind flow
  needs a `Service` nothing creates", "`make -j image-test` can load the wrong
  image", "Whether the operator runs inside the cluster for the E2E flow is
  still open", and "The flags in the Deployment are unchecked" — the last of
  which also states the *wrong* required value once §5 lands, since it names
  `--startup-deadline=20s` as what level B requires.
- **A handover**, `docs/handover-milestone-6.md`, written to be picked up cold.

## 11. What 6a leaves open

- The permissions no driven scenario reaches (§7.4), listed after measurement.
- The digest reference in `config/deploy/deployment.yaml` is unexercised until
  §12's rollout.
- Publishing is a hand-driven step. Automating it is 6e's, along with the
  `deps.json` drift guard that `docs/known-issues.md` has been parking under
  "belongs with CI in milestone 6" since milestone 2c.
- `spawnery-system` stays hard-wired in the RBAC markers
  (`docs/known-issues.md:2470`). 6a runs in that namespace, so it does not bite
  here; parameterizing it is 6d's, and the entry says the chart has to do it in
  the markers and not only in the object names.
- The agent channel's availability gap — no `MaxConcurrentStreams`, no keepalive
  policy, no rate limit in front of `Authenticator.Authenticate`, no NetworkPolicy
  on port 9443 — is untouched. It belongs to 6b and 6d.

## 12. The RKE2 rollout at the end of milestone 6

Not part of 6a, recorded here because 6a's decisions are what it inherits: all
three images come from `ghcr.io/spawnery/` without a pull secret, the operator
runs from the digest in `config/deploy/deployment.yaml` (by then rendered by the
chart), and `--startup-deadline` is the production value.

What that run owes, and 6a cannot: CIS `restricted` against the operator's own
pod security context and against a game server namespace, `HostPort` under the
cluster's actual CNI, a `LoadBalancer` address that a client can reach, several
nodes, and a real join. It is a runbook, driven once, marked `DRIVEN`, in the
manner of `docs/runbook-milestone-3-evidence.md` and its four successors.
