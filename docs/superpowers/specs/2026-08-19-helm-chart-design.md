# Milestone 6d — the Helm chart

## 1. What this milestone is

The operator installs today by `kubectl apply -f config/deploy/`, into a
namespace whose name is written into eight files by hand and into two Go
source comments as a `+kubebuilder:rbac` marker argument. Every handover
since 6a has named that as the single most likely way this project ships
something that works on the author's machine and nowhere else.

6d replaces the flat manifests with a Helm chart, and deletes them. The chart
is not an additional way to install; it is **the** way. `config/deploy/`
ceases to exist.

The milestone is therefore two things at once: a chart, and the removal of
every consumer of the directory it replaces — twenty-one `readManifest` call
sites in `internal/rbacaudit`, four `kubectl apply` lines in `hack/e2e.sh`,
one digest rewrite in `hack/publish.sh`, and one constant in `test/e2e`.

**What it does not establish, stated first because the milestone before this
one earned the habit:** no `helm upgrade` is ever run. The chart puts the
CRDs in `templates/` with `helm.sh/resource-policy: keep` precisely so that
upgrades carry schema changes and uninstalls do not destroy every `Network`
in the cluster — and nothing here observes either behaviour. `make e2e`
builds a fresh cluster on every run and installs once. The upgrade path is
designed and unproven, and every document 6d writes must say so in those
words.

## 2. What falls, and what stays

**Falls entirely:** `config/deploy/` — all seven files
(`clusterrolebinding.yaml`, `deployment.yaml`, `namespace.yaml`,
`networkpolicy.yaml`, `rolebinding.yaml`, `serviceaccount.yaml`,
`service.yaml`).

**Stays, generated:** `config/crd/bases/` and `config/rbac/role.yaml` remain
controller-gen's output. They stop being deploy manifests and become an
intermediate that a second generation step turns into chart templates.

**Stays, hand-written, and deliberately outside the chart:**
`config/rbac/forwarding-secret-reader.yaml`. It is the grant applied *per
game namespace*, not per installation — milestone 5c kept it out of
`config/deploy/` for exactly that reason, and a chart installed once cannot
know the namespaces a user will create later.

That decision has a consequence the chart cannot fix, and §9 records it:
this file's RoleBinding names the operator's ServiceAccount at
`config/rbac/forwarding-secret-reader.yaml:65` with a literal
`namespace: spawnery-system`. An operator installed anywhere else needs that
line changed by hand before any `Network` in that game namespace can be
accepted. Without it the `Network` reports `SecretReadForbidden` — the
condition milestone 5c built for precisely this failure — and every group in
the namespace refuses.

**See §9's correction.** The `SecretReadForbidden` half is right; the rest of
that sentence is not. The `Network` is accepted regardless, and its groups keep
scheduling — what the missing grant costs is forwarding-secret rotation
detection in that namespace, silently.

## 3. The chart

`charts/spawnery/`, containing `Chart.yaml`, `values.yaml`, `README.md`, and
`templates/`:

| Template | From |
|---|---|
| `serviceaccount.yaml` | `config/deploy/serviceaccount.yaml` |
| `clusterrolebinding.yaml` | `config/deploy/clusterrolebinding.yaml` |
| `rolebinding.yaml` | `config/deploy/rolebinding.yaml` |
| `deployment.yaml` | `config/deploy/deployment.yaml` |
| `service.yaml` | `config/deploy/service.yaml` |
| `networkpolicy.yaml` | `config/deploy/networkpolicy.yaml`, gated on a value |
| `rbac.yaml` | **generated** from `config/rbac/role.yaml` |
| `crds.yaml` | **generated** from `config/crd/bases/` |
| `_helpers.tpl` | new |

Every comment in the seven hand-migrated manifests moves with its object.
They are not decoration: `deployment.yaml`'s explains why the strategy is
`Recreate` and instructs the reader not to change it back;
`networkpolicy.yaml`'s explains why its second ingress rule has no `from`;
`rolebinding.yaml`'s explains that a RoleBinding grants only within its own
namespace. A migration that drops them ships the objects and loses the
reasons.

### 3.1 No Namespace object

The release namespace belongs to Helm. `--create-namespace` creates it, and
the chart templates no `Namespace`.

This deletes rather than ports a hazard `hack/e2e.sh:90-98` documents at
length: `config/rbac/role.yaml` carries a cluster-scoped ClusterRole *and* a
namespaced Role, and a plain `kubectl apply -f config/deploy/` walks the
directory alphabetically, so the Deployment precedes the Namespace. The
first of those was reproduced on the first run of `hack/e2e.sh` —
`namespaces "spawnery-system" not found`, not a hypothetical. 6a's handover
says of this: "Helm has its own answer to install ordering; use it, rather
than porting the script's sequence." This is that.

### 3.2 The selector labels are frozen

Helm's convention appends `app.kubernetes.io/instance: {{ .Release.Name }}`
to selector labels. Here that would be wrong three times over:

- A Deployment's `spec.selector.matchLabels` is immutable after creation, so
  a selector containing the release name cannot survive a rename and cannot
  be corrected in place.
- `config/deploy/networkpolicy.yaml`'s `podSelector` and
  `config/deploy/service.yaml:11-13`'s `selector` both pin exactly the pair
  `app.kubernetes.io/name: spawnery` and
  `app.kubernetes.io/component: operator`. The NetworkPolicy's own comment
  says so and says why: the operator pod does **not** carry
  `spawnery.cloud/managed-by`, so this pair is the only handle on it.
- `test/e2e`'s `operatorPod` lists on the same pair.

So the two-label selector stays exactly as it is in all three places, and
Helm's additional labels appear only under `metadata.labels`, never inside a
selector. A helper in `_helpers.tpl` provides the selector pair, and a
separate helper provides the fuller metadata label set, so that no template
can reach for the wrong one by accident.

### 3.3 Generation

`make manifests` gains a second half. controller-gen writes as it does today
into `config/crd/bases` and `config/rbac`; a render step then produces:

- `charts/spawnery/templates/crds.yaml` — the four CRDs, each stamped with
  `helm.sh/resource-policy: keep`.
- `charts/spawnery/templates/rbac.yaml` — the ClusterRole unchanged, and the
  Role with its `namespace:` replaced by `{{ .Release.Namespace }}`.

Both generated files carry a header naming the target that writes them and
stating that hand edits are lost on the next `make manifests`.

The two `+kubebuilder:rbac` markers that carry `namespace=spawnery-system`
(`internal/certs/store.go:57`, `internal/controller/setup.go:72`) keep the
literal, because controller-gen requires *some* namespace to emit a Role at
all. Each gains a comment saying what the value now is: a placeholder the
render step replaces, and not a statement about where the operator runs. A
marker that quietly means something other than what it says is how this
trap was set in the first place.

## 4. The values

```yaml
image:
  repository: ghcr.io/spawnery/spawnery-operator
  tag: "0.1.0"
  digest: ""
  pullPolicy: IfNotPresent

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    memory: 256Mi

nodeSelector: {}
tolerations: []
affinity: {}

networkPolicy:
  enabled: true

operator:
  startupDeadline: 5m
  leaderElect: true
```

`digest` beats `tag`: set, the template renders `repository@sha256:…`;
unset, `repository:tag`. This is where `hack/publish.sh` writes from now on
(§6), and because the chart is the only installation form it is the only
place a digest means anything.

`operator.startupDeadline` replaces a trick.
`hack/e2e.sh:155` currently appends a **second** `--startup-deadline=20s` to
the container's argument list and relies on Go's flag package taking the
last occurrence. A value is a setting; that was a sleight of hand that
worked.

`networkPolicy.enabled` gates the 6b policy in front of the agent endpoint,
defaulting to on. Its template comment must carry what 6b measured — that a
CNI which does not implement NetworkPolicy makes the object inert either way
— so that a reader neither mistakes disabling it for a loss it is not, nor
for a free action where it is one.

**`replicas` is deliberately absent.** `readyz` hangs off the leader lock, so
a second pod never becomes ready; that is why `config/deploy/deployment.yaml`
carries `strategy: Recreate` and a comment telling the reader not to change
it back. A knob whose only valid setting is 1, and whose second setting
wedges the operator, is a trap rather than a feature. The template writes 1
and the comment moves with it.

**`imagePullSecrets` is deliberately absent.** Milestone 6a decided the three
images are public and unauthenticated. If that changes it is a field and a
test, not a restructuring.

## 5. What proves it

### 5.1 `internal/rbacaudit` audits the rendered chart

Today `internal/rbacaudit` reads files: twenty-one `readManifest` calls
across two test files — five in `audit_envtest_test.go` (`:59-63`) and
sixteen in `deploy_envtest_test.go` — plus
`deploy_envtest_test.go:72`'s `generatedRoles = "config/rbac/role.yaml"`.

All of them move behind one helper that runs `helm template` once per
package run and parses the result into typed objects, keyed by kind and
name. The package's tests then read from the rendered set. The `Namespace`
those tests read today no longer exists as an object anywhere, so the tests
that need one construct it themselves.

**The helper renders with a namespace that is not the chart's default**, for
the same reason §5.2 gives: rendered at `spawnery-system`, a template that
forgot `{{ .Release.Namespace }}` and hard-coded the default produces
byte-identical output, and the audit would confirm a chart that cannot move.
The rendered namespace is the helper's own constant and is not
`platform-system` either — the two checks should not be able to pass for the
same wrong reason.

The gain is not tidiness. The audit currently compares its table against an
intermediate; afterwards it compares against exactly what a user installs,
which is the first time anything in this repository does.

The subprocess is a real dependency and worth naming: the package's tests
will require `helm` on `PATH`. That is not a new class of requirement —
envtest already takes its API server binaries from the flake, and nothing in
this repository runs outside `nix develop` — but it is one more thing that
can be absent, and the helper must fail with a message that says so plainly
rather than with a parse error on empty input.

### 5.2 `make e2e` installs somewhere else on purpose

`hack/e2e.sh` replaces its four apply lines (`:99-102`) with a single
`helm install --create-namespace --namespace platform-system`.
`test/e2e/e2e_test.go:48`'s `operatorNamespace` constant follows it.

The name is chosen to share nothing with the default. A near-miss like
`spawnery-operators` would still fail a leaked literal, but it reads as a
variant of the default and invites somebody to "tidy" it back later;
`platform-system` is visibly a third party's choice, which is the case the
chart has to survive.

This is the milestone's central assertion, and the pleasing part is who
enforces it. If any `spawnery-system` survives the templating, the Role
lands in a namespace holding no operator, and the operator fails at its
first `certs.Store.Ensure` with `Forbidden` — which
`theOperatorWasNeverDenied`, the last scenario of every run, exists to
catch. The check this E2E package has had since 6a becomes, without being
modified at all, the guard over this milestone's principal risk.

**Corrected after the milestone's final review: that holds for one of the two
ways the literal can leak, and the other was never measured.** Task 4 put a
`spawnery-system` literal on the chart's own RoleBinding `metadata.namespace`
and expected `theOperatorWasNeverDenied` to go red. It never executed.
Kubernetes validates a RoleBinding's own `metadata.namespace` for existence at
admission, so `helm install` refused first with `namespaces "spawnery-system"
not found` and `hack/e2e.sh` aborted under `set -e` before `go test` started.
The hazard splits: a literal surviving in the chart's **own-namespace** RBAC
fields — a RoleBinding's own `metadata.namespace`, or the generated Role's
`namespace:` — is caught at install time by Kubernetes, not by this check. A
literal surviving in a **subject** namespace is not validated by the API
server at all; it applies cleanly, binds a ServiceAccount that exists
nowhere, and is by this design's own reasoning the path
`theOperatorWasNeverDenied` catches, once the resulting denial lands on a write
verb. **That second path was never mutated by this milestone**, so for it the
claim is reasoning rather than measurement. The corrected statement lives in
`docs/handover-milestone-6d.md` §2 and in
`docs/known-issues.md`'s "From milestone 6d" section;
`hack/e2e.sh`'s `OPERATOR_NAMESPACE` comment and
`test/e2e/e2e_test.go`'s `operatorNamespace` comment now carry the same split.

**Measured 2026-08-25: the second path is caught, and by two checks rather
than one.** `config/rbac/forwarding-secret-reader.yaml` was applied unedited
into both game namespaces of a real `make e2e` run — the mistake this chart's
README warns an administrator against — so its RoleBinding bound
`system:serviceaccount:spawnery-system:spawnery-operator` while the operator
ran as `platform-system:spawnery-operator`. Kubernetes accepted it, as this
section says it would. `theOperatorWasNeverDenied` went red naming both
denials, and `the table holds against the real authorizer` went red beside it.

Two things the reasoning above had wrong, both in the same direction. The
denial lands on a **read**, not a write: it is the forwarding-secret `get`, and
it reaches the log only because `readForwardingSecret` was made to carry the
API server's own error out on 2026-08-24 — before that it folded the 403 into a
condition message with no `is forbidden:` substring and this run would have
stayed green. And the check is not alone: the `SubjectAccessReview` scenario
sees the same break without needing the operator to have attempted anything.

The chart keeps `spawnery-system` as its documented default, so the README
and every document stay true for the ordinary case.

### 5.3 The rest

- `make chart-lint` runs `helm lint`, wired into `make test`.
- `hack/publish.sh`'s `WRITE_DIGEST` path writes `charts/spawnery/values.yaml`
  instead of `config/deploy/deployment.yaml:157`.

### 5.4 One check that happens once and does not stay

Before `config/deploy/` is deleted, `helm template` with default values must
be compared object by object against the seven manifests it replaces, and
the result recorded in the implementer's report: every object present, every
field equal or deliberately different with the reason. Afterwards there is
nothing left to compare against, so this is a piece of evidence rather than
a test. Skipped, any silent divergence introduced during the migration is
unobservable from that moment on.

## 6. Facts this design asserts about the code already here

Each is a claim an implementer can check before trusting anything above.

1. `config/deploy/` contains exactly seven files, listed in §2.
2. Eight files hard-code `namespace: spawnery-system`: all of
   `config/deploy/` except `namespace.yaml` (which *is* it), plus
   `config/rbac/role.yaml:137` and
   `config/rbac/forwarding-secret-reader.yaml:65`.
3. `internal/rbacaudit` calls `readManifest` twenty-one times — five in
   `audit_envtest_test.go` at `:59-63`, sixteen in `deploy_envtest_test.go`
   — and names `config/rbac/role.yaml` once at `deploy_envtest_test.go:72`.
4. `hack/e2e.sh` applies four things at `:99-102`: `config/crd/bases/`,
   `config/deploy/namespace.yaml`, `config/rbac/role.yaml`, then
   `config/deploy/`. It separately creates two game namespaces and applies
   `config/rbac/forwarding-secret-reader.yaml` into each (`:127-128`,
   `:135-137`).
5. `hack/e2e.sh:155` appends a second `--startup-deadline=20s` as a JSON
   patch to the container's args.
6. `hack/publish.sh:157` sets `manifest="config/deploy/deployment.yaml"` on
   the `WRITE_DIGEST` path.
7. `test/e2e/e2e_test.go:48` holds `operatorNamespace = "spawnery-system"`;
   `operatorPod` lists on `app.kubernetes.io/name: spawnery` and
   `app.kubernetes.io/component: operator`.
8. `config/deploy/service.yaml:11-13` and `config/deploy/networkpolicy.yaml`'s
   `podSelector` both use that same pair.
9. `config/rbac/role.yaml` is two documents: a ClusterRole (line 3) with no
   namespace, and a Role (line 134) whose namespace is on line 137.
10. `+kubebuilder:rbac` markers carrying `namespace=spawnery-system` exist at
    `internal/certs/store.go:57` and `internal/controller/setup.go:72`, and
    nowhere else.
11. `flake.nix:62` provides `kubernetes-helm`; the shell resolves it to Helm
    **v4.2.3**.
12. `config/deploy/deployment.yaml` sets `strategy: Recreate` with a comment
    forbidding a change back to `RollingUpdate`, and its image line is a tag
    (`ghcr.io/spawnery/spawnery-operator:0.1.0`) because no real
    `make publish` has run.

## 7. What 6d does not do

- **No `helm upgrade` is exercised.** See §1. The design chooses
  `templates/` plus `resource-policy: keep` over Helm's `crds/` directory
  because `crds/` is never touched by an upgrade, and a `v1alpha1` API that
  has changed in six consecutive milestones needs its schema to move with
  the chart. Nothing here proves that it does.
- **No chart publishing.** No OCI push, no chart repository, no
  `helm package` in CI. That belongs to 6e or to the repository owner.
- **No templating of game namespaces.** `forwarding-secret-reader.yaml` stays
  a file applied per namespace, and §2 records the manual step that leaves.
- **No claim about reachability, enforcement, or a running game.** Nothing in
  this milestone changes what any earlier one measured.

## 8. Acceptance

1. `make test` green, including the race detector, with `internal/rbacaudit`
   auditing the rendered chart rather than files.
2. `make e2e` green with eighteen scenarios, installed by `helm install`
   into a namespace that is **not** `spawnery-system`, with
   `theOperatorWasNeverDenied` still last and still passing.
3. `helm lint` clean, via `make chart-lint`.
4. The one-time equivalence check of §5.4 recorded in a task report.
5. No `spawnery-system` literal survives outside: the chart's default value,
   the two `+kubebuilder:rbac` placeholders with their new comments,
   `config/rbac/forwarding-secret-reader.yaml` with its documented manual
   step, and prose in documents.
6. `internal/rbacaudit` goes red in both directions when a marker and the
   table disagree, as it does today, now against the rendered chart.
7. `hack/publish.sh`'s digest path writes `charts/spawnery/values.yaml`, and
   a chart rendered with a digest set produces an image reference containing
   `@sha256:`.
8. `helm uninstall` leaves the four CRDs standing. This one *is* observable
   here, unlike upgrade, and the chart's README states the manual deletion
   it implies.

## 9. What the RKE2 rollout inherits from 6d

Unchanged from 6c's handover in everything it listed, plus one manual step
this milestone creates rather than removes:

**`config/rbac/forwarding-secret-reader.yaml:65` names
`namespace: spawnery-system` in its RoleBinding subject, and the chart
cannot template it.** The file is applied per game namespace, by hand, after
the chart is installed. An operator installed in any other namespace must
have that line changed first, or every `Network` in every game namespace
reports `SecretReadForbidden` and every group in it refuses with
`NetworkNotAccepted`. The chart's README must say this in its installation
steps rather than in a footnote, because the failure it produces names the
secret and not the namespace, and a reader will look in the wrong place.

**Corrected after the milestone's final review: the second half of that
consequence is false, and the truth is quieter.** Read against
`internal/controller/network_controller.go`, nothing on this path can produce
`NetworkNotAccepted`: `ConditionAccepted` is set `True` at `:93-97`, the
forwarding secret is read afterwards at `:142`, and everything persists
together at `:171`, so a `Network` whose reader grant is missing or points at
the wrong operator namespace stays `Accepted=True`. Both group controllers
gate on that condition, so every `ServerGroup` and `ProxyGroup` in the
namespace keeps scheduling normally. `ReasonNetworkNotAccepted` exists
(`internal/controller/servergroup_controller.go:152`,
`internal/controller/proxygroup_controller.go:217`) but nothing in this code
path ever sets it. What actually breaks is narrower and silent: the read's
outcome only ever reaches `ConditionForwardingSecretResolved` and
`ConditionForwardingSecretRotationPending`, so that namespace loses
forwarding-secret rotation detection and reports it nowhere a group's status
would show. Everything the paragraph above says about the README stands, and
`charts/spawnery/README.md` states this narrower consequence. The corrected
statement lives in `docs/handover-milestone-6d.md`
§2 and §4 and in `docs/known-issues.md`'s "From
milestone 6d" section.

**Amended 2026-08-24: it is no longer silent.** `readForwardingSecret` now
carries the API server's own error out beside the condition message, and
`network_controller.go` logs it and records a `Warning` on the `Network` on
entering the state. The condition message is written for a person and quotes
no API server, so it carries no `is forbidden:` substring — which is exactly
what `test/e2e`'s `theOperatorWasNeverDenied` greps the operator's log for,
and why a broken grant escaped the one check written to catch a denial the
RBAC audit cannot. Everything else above stands: `Accepted` is untouched, the
groups keep scheduling, and what breaks is still only rotation detection for
that namespace.
