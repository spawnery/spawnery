# Milestone 6d — Helm Chart Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `config/deploy/` with a Helm chart that installs into any
namespace, and delete every consumer of the directory it replaces.

**Architecture:** `charts/spawnery/` becomes the only installation form. Six
templates are migrated by hand from `config/deploy/`; two are generated from
controller-gen's output by a new half of `make manifests`. `internal/rbacaudit`
stops reading files and starts auditing `helm template`'s output. `make e2e`
installs into a namespace that shares nothing with the chart's default, which
makes every existing E2E scenario a guard over the templating.

**Tech Stack:** Helm v4.2.3 (from the flake), Go 1.24, controller-runtime,
envtest, kind under rootless podman, Nix flakes.

**Spec:** `docs/superpowers/specs/2026-08-19-helm-chart-design.md`

## Global Constraints

- **Every command runs inside the dev shell.** This machine needs the
  experimental flags: prefix with
  `nix --extra-experimental-features 'nix-command flakes' develop -c`. This
  plan writes the short form `nix develop -c <cmd>`; expand it every time.
  Nothing — `go`, `helm`, `kubectl`, `kind`, envtest — is on `PATH` without
  it. See `docs/known-issues.md`.
- **`make test` runs with `-race`.** Green without it and red with it is red.
- **Commit messages use Conventional Commits** — `feat(6d):`, `fix(6d):`,
  `test(6d):`, `docs(6d):`, `refactor(6d):` — deliberately overriding this
  repository's older sentence-style history. Every commit ends with exactly
  these two trailers:
  ```
  Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_014xc1DzU4wwe2h1bV5V7zpn
  ```
- **The selector label pair is frozen and appears in four places.** Exactly
  `app.kubernetes.io/name: spawnery` and
  `app.kubernetes.io/component: operator`, in the Deployment's
  `spec.selector.matchLabels` and pod template, the Service's `spec.selector`,
  the NetworkPolicy's `spec.podSelector`, and `test/e2e`'s `operatorPod`.
  **Helm's `app.kubernetes.io/instance` must never enter a selector.** A
  Deployment's selector is immutable after creation; the Service and
  NetworkPolicy would stop matching the pod; the E2E would stop finding it.
- **No `Namespace` object in the chart.** The release namespace is Helm's, and
  `--create-namespace` creates it.
- **`replicas` is not a value and the template writes `1`.** `readyz` hangs off
  the leader lock, so a second pod never becomes ready.
- **The chart's default namespace stays `spawnery-system`**, so every document
  and the README remain true for the ordinary case. Only the tests install
  elsewhere.
- **Three namespaces, none of which may be the same as another:**
  `spawnery-system` is the chart's default; `platform-system` is where
  `make e2e` installs; `rbacaudit`'s render helper uses a third of its own.
  Two checks that could pass for the same wrong reason are one check.
- **Every comment in a migrated manifest moves with its object.** They carry
  reasons — why the strategy is `Recreate`, why the second ingress rule has no
  `from`, why the RoleBinding's namespace must match the Role's. A migration
  that drops them ships the objects and loses the reasons.
- **A test is not done until a mutation proves it can fail.** Every task names
  the mutation, the command, and the failure to expect. Report the actual
  output. This project's record across milestones 5, 6b and 6c is fifteen
  findings, every one produced by a mutation and none by reading, and every
  one sitting in test code a plan had specified verbatim.
- **No claim of reachability, and no claim about upgrades.** Nothing in this
  repository demonstrates that a client reaches a proxy, and **no `helm
  upgrade` is ever run in this milestone.** The CRD lifecycle the chart is
  built around is designed and unobserved; say so in those words wherever it
  comes up.

---

## File Structure

**Created:**

- `charts/spawnery/Chart.yaml` — chart metadata and the Kubernetes floor.
- `charts/spawnery/values.yaml` — the whole configurable surface, §4 of the spec.
- `charts/spawnery/README.md` — install steps, including the one manual edit
  the chart cannot make (`config/rbac/forwarding-secret-reader.yaml`).
- `charts/spawnery/templates/_helpers.tpl` — the frozen selector pair, the
  fuller metadata label set, and the image reference.
- `charts/spawnery/templates/{serviceaccount,clusterrolebinding,rolebinding,deployment,service,networkpolicy}.yaml`
  — migrated by hand from `config/deploy/`.
- `charts/spawnery/templates/{rbac,crds}.yaml` — **generated**, never hand-edited.
- `hack/chart-templates.sh` — the generation step `make manifests` calls.
- `docs/handover-milestone-6d.md` — the cold-start entry point for 6e.

**Modified:**

- `Makefile` — `manifests` gains its second half; new `chart-lint` target,
  wired into `test`.
- `internal/certs/store.go:57`, `internal/controller/setup.go:72` — a comment
  each, saying the marker's namespace is now a placeholder.
- `internal/rbacaudit/deploy_envtest_test.go`, `audit_envtest_test.go` — the
  render helper and twenty-one call sites.
- `hack/e2e.sh` — `helm install` replaces four `kubectl apply` lines; the
  startup-deadline patch goes.
- `hack/publish.sh:157` — the digest lands in the chart's values.
- `test/e2e/e2e_test.go:48` — the operator namespace.
- `README.md`, `docs/known-issues.md`.

**Deleted:**

- `config/deploy/` — all seven files, in Task 5, after every reader has moved.

**Unchanged, and worth knowing why:**

- `config/crd/bases/` and `config/rbac/role.yaml` stay as controller-gen's
  output. They stop being deploy manifests and become the input to the
  generation step. `internal/testenv` loads CRDs from `config/crd/bases`, so
  envtest is unaffected by any of this.
- `config/rbac/forwarding-secret-reader.yaml` stays outside the chart. It is
  applied per game namespace, and a chart installed once cannot know the
  namespaces a user creates later.

---

### Task 1: The chart, by hand

**Files:**
- Create: `charts/spawnery/Chart.yaml`, `values.yaml`, `templates/_helpers.tpl`,
  `templates/serviceaccount.yaml`, `templates/clusterrolebinding.yaml`,
  `templates/rolebinding.yaml`, `templates/deployment.yaml`,
  `templates/service.yaml`, `templates/networkpolicy.yaml`
- Read (do not modify): all of `config/deploy/`

**Interfaces:**
- Consumes: nothing.
- Produces, for later tasks:
  - The chart renders with `helm template spawnery charts/spawnery --namespace <ns>`.
  - Template helpers: `spawnery.selectorLabels` (the frozen pair, two lines,
    no trailing newline issues), `spawnery.labels` (the pair plus Helm's
    metadata labels), `spawnery.image` (digest when set, else tag).
  - Values consumed by later tasks: `.Values.image.digest` (Task 5 writes it),
    `.Values.operator.startupDeadline` (Task 4 sets it), and
    `.Values.networkPolicy.enabled`.

- [ ] **Step 1: Read every file you are about to migrate**

```
nix develop -c cat config/deploy/clusterrolebinding.yaml config/deploy/deployment.yaml config/deploy/rolebinding.yaml config/deploy/serviceaccount.yaml config/deploy/service.yaml config/deploy/networkpolicy.yaml
```

Read the comments, not only the fields. Three of them are load-bearing and
must survive into the templates:

- `deployment.yaml` explains why `strategy: Recreate` and instructs the reader
  not to change it back to `RollingUpdate`: `readyz` hangs off the leader lock,
  a new pod only turns ready once it holds that lock, and the old pod holds it
  for as long as it runs.
- `networkpolicy.yaml` explains why its second ingress rule has no `from` (the
  kubelet's source is the node, not a pod), and why the pod is selected by its
  two labels rather than by `spawnery.cloud/managed-by` (the operator pod does
  not carry that label).
- `rolebinding.yaml` explains that a RoleBinding grants only within its own
  namespace, and that misplacing it makes the operator fail at its first
  `certs.Store.Ensure` with `Forbidden`.

- [ ] **Step 2: Write `Chart.yaml`**

```yaml
apiVersion: v2
name: spawnery
description: A Kubernetes-native control plane for Minecraft server networks
type: application

# The chart's own version, and the operator release it installs by default.
# They move independently: a chart fix that changes no image is a chart
# version bump alone.
version: 0.1.0
appVersion: "0.1.0"

# The floor is the CRDs', not the operator's. api/v1alpha1 carries
# +kubebuilder:validation:XValidation rules -- five of them on ExposeSpec
# alone -- and CEL validation reaches beta and on-by-default in 1.25.
# Declared rather than measured: nothing in this repository installs the
# chart on an old cluster to find out.
kubeVersion: ">=1.25.0-0"
```

- [ ] **Step 3: Write `values.yaml`**

Copy the resource figures from `config/deploy/deployment.yaml` exactly; do not
round them.

```yaml
image:
  repository: ghcr.io/spawnery/spawnery-operator
  # A tag until hack/publish.sh has run once against a real registry, then the
  # digest it writes below. The master design's section 8 asks for digests in
  # shipped manifests because a tag can move under a running cluster.
  tag: "0.1.0"
  # Set, this wins over tag and the image is referenced by digest.
  digest: ""
  pullPolicy: IfNotPresent

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    memory: 256Mi

# Passed through to the operator pod unchanged. On a cluster whose control
# plane carries a taint, tolerations are often the difference between Running
# and Pending.
nodeSelector: {}
tolerations: []
affinity: {}

networkPolicy:
  # The ingress rule in front of the agent endpoint, milestone 6b's
  # network-independent half. Disabling it is the right answer on a cluster
  # that manages its own policies centrally -- and makes no difference at all
  # on a CNI that implements no NetworkPolicy controller, which is what 6b
  # measured of the CNI in this repository's own end-to-end harness.
  enabled: true

operator:
  # The production value. hack/e2e.sh overrides it for its own run.
  startupDeadline: 5m
  leaderElect: true
```

There is deliberately **no `replicas`** and **no `imagePullSecrets`**. If you
find yourself wanting either, re-read the plan's Global Constraints and the
spec's §4 before adding one.

- [ ] **Step 4: Write `templates/_helpers.tpl`**

```
{{/*
The selector pair, and nothing else, ever.

Four things pin exactly these two labels: the Deployment's own
spec.selector.matchLabels, the Service's spec.selector, the NetworkPolicy's
spec.podSelector, and test/e2e's operatorPod. Helm's convention would add
app.kubernetes.io/instance here; it must not. A Deployment's selector is
immutable after creation, so a selector carrying the release name cannot be
corrected in place -- and the Service and NetworkPolicy would silently stop
matching the pod, which looks like a network fault rather than a label one.
*/}}
{{- define "spawnery.selectorLabels" -}}
app.kubernetes.io/name: spawnery
app.kubernetes.io/component: operator
{{- end }}

{{/*
Metadata labels: the selector pair plus what Helm expects to find on objects
it manages. Never used in a selector -- see above.
*/}}
{{- define "spawnery.labels" -}}
{{ include "spawnery.selectorLabels" . }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{/*
The operator image. A digest beats a tag because a tag can move under a
running cluster; hack/publish.sh writes .Values.image.digest after a real
publish.
*/}}
{{- define "spawnery.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}
{{- end }}
```

- [ ] **Step 5: Write the six migrated templates**

Each keeps its original object's fields exactly, replaces the literal
`spawnery-system` with `{{ .Release.Namespace }}`, and takes its labels from
the helpers. Names stay literal (`spawnery-operator`) — this chart installs one
operator per namespace and a release-name prefix would break the
ClusterRoleBinding's reference to the generated ClusterRole, whose name
controller-gen fixes at `spawnery-operator`.

`templates/serviceaccount.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: spawnery-operator
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "spawnery.labels" . | nindent 4 }}
```

`templates/clusterrolebinding.yaml`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: spawnery-operator
  labels:
    {{- include "spawnery.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: spawnery-operator
subjects:
  - kind: ServiceAccount
    name: spawnery-operator
    namespace: {{ .Release.Namespace }}
```

`templates/rolebinding.yaml` — keep the original comment, adjusted to name the
template rather than the file:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: spawnery-operator
  # Must be the namespace of the Role in templates/rbac.yaml: a RoleBinding
  # grants within its own namespace and nowhere else. Put it elsewhere and it
  # grants nothing at all, and the operator fails at its first
  # certs.Store.Ensure with Forbidden. Both come from .Release.Namespace, so
  # they cannot disagree.
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "spawnery.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: spawnery-operator
subjects:
  - kind: ServiceAccount
    name: spawnery-operator
    namespace: {{ .Release.Namespace }}
```

`templates/service.yaml` — copy the original's fields and comments verbatim,
substituting the namespace and taking `spec.selector` from
`spawnery.selectorLabels`.

`templates/networkpolicy.yaml` — wrap the whole document in
`{{- if .Values.networkPolicy.enabled }}` … `{{- end }}`, keep every comment,
substitute the namespace, and take `spec.podSelector.matchLabels` from
`spawnery.selectorLabels`.

`templates/deployment.yaml` — the largest. Keep `strategy: Recreate` and its
whole comment. Take `spec.selector.matchLabels` **and** the pod template's
labels from `spawnery.selectorLabels`. The image comes from
`{{ include "spawnery.image" . }}`. The args become:

```yaml
          args:
            - --leader-elect={{ .Values.operator.leaderElect }}
            - --startup-deadline={{ .Values.operator.startupDeadline }}
            - --metrics-bind-address=:8080
            - --health-probe-bind-address=:8081
```

and the four scheduling fields are appended to the pod spec with `with` blocks:

```yaml
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
```

`resources` comes from `{{- toYaml .Values.resources | nindent 12 }}`.
Everything else — probes, ports, securityContext, the `POD_NAMESPACE` downward
API env var and its comment, `terminationGracePeriodSeconds` — is copied
unchanged.

- [ ] **Step 6: Render it and read the output**

```
nix develop -c helm template spawnery charts/spawnery --namespace platform-system
```

Expected: six objects, no errors. Read the rendered YAML. Check by eye that no
`spawnery-system` appears anywhere in it, and that no selector carries
`app.kubernetes.io/instance`.

- [ ] **Step 7: Lint**

```
nix develop -c helm lint charts/spawnery
```

Expected: `1 chart(s) linted, 0 chart(s) failed`. A missing `icon` warning is
acceptable and expected.

- [ ] **Step 8: The one-time equivalence check — this is evidence, not a test**

`config/deploy/` still exists at this point and is deleted in Task 5. This is
the only moment the two can be compared, so do it now and record it.

```
mkdir -p /tmp/6d
nix develop -c helm template spawnery charts/spawnery --namespace spawnery-system > /tmp/6d/rendered.yaml
```

Then, object by object — ServiceAccount, ClusterRoleBinding, RoleBinding,
Deployment, Service, NetworkPolicy — compare the rendered object against the
file in `config/deploy/` it came from. For each object record in your report:

- every field present in the original and present in the rendering
- every field that differs, **with the reason** (the added
  `helm.sh/chart`, `app.kubernetes.io/managed-by` and
  `app.kubernetes.io/version` metadata labels are expected; anything else is
  a finding)
- every comment that survived

`config/deploy/namespace.yaml` has no counterpart by design — the chart
templates no `Namespace`.

**If any field differs for a reason you cannot state, stop and report it
rather than resolving it silently.** After Task 5 there is nothing left to
compare against, and a divergence introduced here becomes permanently
invisible.

- [ ] **Step 9: Commit**

```
git add charts/
git commit
```

Subject: `feat(6d): the chart, hand-migrated and rendering`.
The body should name the three load-bearing comments that moved with their
objects, and state that the selector pair is frozen and why.

---

### Task 2: The generated templates

**Files:**
- Create: `hack/chart-templates.sh`
- Create (by running it): `charts/spawnery/templates/rbac.yaml`,
  `charts/spawnery/templates/crds.yaml`
- Modify: `Makefile:15-18` (the `manifests` target)
- Modify: `internal/certs/store.go:57`, `internal/controller/setup.go:72`
  (one comment each)

**Interfaces:**
- Consumes: the chart from Task 1.
- Produces:
  - `charts/spawnery/templates/rbac.yaml` — the ClusterRole named
    `spawnery-operator` unchanged, and the Role named `spawnery-operator` whose
    `namespace:` is `{{ .Release.Namespace }}`.
  - `charts/spawnery/templates/crds.yaml` — the four CRDs, each carrying the
    annotation `helm.sh/resource-policy: keep`.
  - `make manifests` regenerates both. Task 3 audits their rendered form.

- [ ] **Step 1: Read what you are transforming**

```
nix develop -c head -20 config/rbac/role.yaml
nix develop -c grep -n "^kind:\|^  name:\|^  namespace:\|^---" config/rbac/role.yaml
nix develop -c ls config/crd/bases/
nix develop -c grep -n "manifests:" -A 5 Makefile
```

`config/rbac/role.yaml` is exactly two documents: a ClusterRole (no namespace)
and a Role whose `namespace: spawnery-system` sits on its own line.
`config/crd/bases/` holds four files.

- [ ] **Step 2: Write `hack/chart-templates.sh`**

Follow the style of the other scripts in `hack/` — read `hack/e2e.sh`'s header
first. `set -euo pipefail`, a header comment saying what it does and why, and
loud failure rather than partial output.

```bash
#!/usr/bin/env bash
# Turns controller-gen's output into the two chart templates that must not be
# written by hand.
#
# Run by `make manifests`, immediately after controller-gen. Two transforms,
# both small and both load-bearing:
#
#   role.yaml  -> templates/rbac.yaml, with the Role's namespace replaced by
#                 Helm's release namespace. The +kubebuilder:rbac markers must
#                 name *some* namespace for controller-gen to emit a Role at
#                 all, so the literal in the markers is a placeholder and this
#                 is where it stops being one.
#   crd/bases/ -> templates/crds.yaml, with helm.sh/resource-policy: keep on
#                 every CRD. The CRDs live in templates/ rather than Helm's
#                 crds/ directory so that `helm upgrade` carries schema
#                 changes -- an API that has moved in six consecutive
#                 milestones needs that -- and the annotation is what stops
#                 `helm uninstall` from taking every Network, ServerGroup,
#                 ProxyGroup and Server in the cluster with it.
#
# Neither output is ever edited by hand. Both carry a header saying so.
set -euo pipefail

cd "$(dirname "$0")/.."

roles_in="config/rbac/role.yaml"
crds_in="config/crd/bases"
out_dir="charts/spawnery/templates"

header='# Generated by hack/chart-templates.sh via `make manifests`. Do not edit:
# your changes are lost the next time controller-gen runs.'

# --- rbac.yaml ------------------------------------------------------------
#
# The substitution is anchored to the exact line controller-gen emits, and the
# script refuses if it is not there: a silent no-op would ship a Role pinned to
# spawnery-system, which installs cleanly in any namespace and then denies the
# operator its own Secret -- the failure this whole milestone exists to remove.
if ! grep -q '^  namespace: spawnery-system$' "$roles_in"; then
	echo "hack/chart-templates.sh: $roles_in has no '  namespace: spawnery-system' line." >&2
	echo "controller-gen's output changed shape, or a marker lost its namespace= argument." >&2
	exit 1
fi

{
	echo "$header"
	sed 's|^  namespace: spawnery-system$|  namespace: {{ .Release.Namespace }}|' "$roles_in"
} > "$out_dir/rbac.yaml"

# --- crds.yaml ------------------------------------------------------------
#
# Each CRD gets the keep annotation. controller-gen emits `metadata:` followed
# by `annotations:` on the next line, so the insertion point is that
# annotations block; the script refuses per file if it is absent rather than
# emitting a CRD Helm will delete on uninstall.
{
	echo "$header"
	for f in "$crds_in"/*.yaml; do
		if ! grep -q '^  annotations:$' "$f"; then
			echo "hack/chart-templates.sh: $f has no top-level annotations block." >&2
			exit 1
		fi
		echo "---"
		sed '0,/^  annotations:$/s|^  annotations:$|  annotations:\n    helm.sh/resource-policy: keep|' "$f"
	done
} > "$out_dir/crds.yaml"

echo "wrote $out_dir/rbac.yaml and $out_dir/crds.yaml"
```

**Verify the two `grep -q` guards against the real files before you trust
them.** If `config/crd/bases/*.yaml` does not carry `  annotations:` at that
indentation, find what it does carry and anchor to that instead — and say so in
your report. A guard that never fires is worse than no guard, because it reads
as protection.

- [ ] **Step 3: Wire it into `make manifests`**

```make
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=spawnery-operator paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac
	./hack/chart-templates.sh
```

- [ ] **Step 4: Run it**

```
nix develop -c make manifests
nix develop -c head -5 charts/spawnery/templates/rbac.yaml
nix develop -c grep -n "namespace: {{ .Release.Namespace }}" charts/spawnery/templates/rbac.yaml
nix develop -c grep -c "helm.sh/resource-policy: keep" charts/spawnery/templates/crds.yaml
```

Expected: the header on both files; exactly one `{{ .Release.Namespace }}` in
`rbac.yaml`; exactly `4` in the `crds.yaml` count.

- [ ] **Step 5: Render and lint the whole chart**

```
nix develop -c helm template spawnery charts/spawnery --namespace platform-system > /tmp/6d/full.yaml
nix develop -c grep -c "^kind: CustomResourceDefinition" /tmp/6d/full.yaml
nix develop -c grep -n "spawnery-system" /tmp/6d/full.yaml
nix develop -c helm lint charts/spawnery
```

Expected: `4` CRDs; **no output at all** from the `spawnery-system` grep;
lint clean.

- [ ] **Step 6: Comment the two markers**

`internal/certs/store.go:57` and `internal/controller/setup.go:72` each carry a
`+kubebuilder:rbac` marker with `namespace=spawnery-system`. Add a comment
above each — worded for its own site, not copy-pasted — saying that the
namespace here is a placeholder that `hack/chart-templates.sh` replaces with
Helm's release namespace, that controller-gen needs *some* namespace to emit a
namespaced Role at all, and that it is therefore not a statement about where
the operator runs. Do not change the marker itself: it is what makes
controller-gen emit a `Role` rather than folding these rules into the
ClusterRole, and the operator's Secret and Lease rights are deliberately not
cluster-wide.

- [ ] **Step 7: Mutate, and report what happened**

For each, apply, run, record the verbatim output, revert.

1. Delete the `grep -q '^  namespace: spawnery-system$'` guard **and** break
   the `sed` expression (change `spawnery-system` to `spawnery-systm` in the
   pattern), then run `make manifests` and render with
   `--namespace platform-system`. Expected: the render still succeeds and
   `grep -n spawnery-system` on the output now finds the Role's namespace.
   **This is the failure the guard exists for**; confirm the guard catches it
   when restored.
2. Remove the annotation insertion from the CRD loop, run `make manifests`,
   and count `helm.sh/resource-policy: keep` in `crds.yaml`. Expected: `0`.

Note that neither mutation is caught by any Go test at this point — Task 3 is
what closes that. Say so in your report rather than implying the guards are
tested.

- [ ] **Step 8: Full suite and commit**

```
nix develop -c make test
git add -A
git commit
```

Subject: `feat(6d): generate the chart's RBAC and CRDs from controller-gen`.
The body should say why the CRDs are in `templates/` with the keep annotation
rather than in Helm's `crds/`, and that **no `helm upgrade` is exercised
anywhere in this milestone**, so the upgrade path is designed and unobserved.

---

### Task 3: `internal/rbacaudit` audits the rendered chart

**Files:**
- Modify: `internal/rbacaudit/deploy_envtest_test.go` — `generatedRoles` at
  `:72`, sixteen `readManifest` call sites
- Modify: `internal/rbacaudit/audit_envtest_test.go` — five `readManifest` call
  sites at `:59-63`

**Interfaces:**
- Consumes: the complete chart from Tasks 1 and 2.
- Produces:
  - `func renderChart(t *testing.T) map[string][]byte` — the chart rendered
    once per package run, keyed `"<Kind>/<name>"`, memoised.
  - `func renderedManifest[T any](t *testing.T, key string, into *T)` — the
    same strict decode `readManifest` does, from the rendered set.
  - `const renderNamespace` — the namespace the helper renders with.
  - `readManifest` **stays**, because
    `config/rbac/forwarding-secret-reader.yaml` is still a file on disk.

- [ ] **Step 1: Read the package first**

```
nix develop -c sed -n '40,110p' internal/rbacaudit/deploy_envtest_test.go
nix develop -c grep -n "readManifest(" internal/rbacaudit/*_test.go
nix develop -c grep -n "generatedRoles\|readMultiDocManifest\|readGeneratedRoles" internal/rbacaudit/*_test.go
```

Note three things. `readManifest` uses `yaml.UnmarshalStrict`, deliberately —
its comment explains that a plain `Unmarshal` drops unknown keys, so a typo
like `readOnlyRootFilesytem:` would decode to a zero value and every assertion
would then check a field nobody set. Your rendered decode must be strict for
the same reason. `readMultiDocManifest` already splits multi-document YAML and
is used by both `readGeneratedRoles` and `readForwardingSecretReader`. And
`testenv.RepoPath(t, rel)` is how this package finds the repository root.

- [ ] **Step 2: Write the failing test**

Add to `internal/rbacaudit/deploy_envtest_test.go`, above the helpers:

```go
// renderNamespace is the namespace renderChart renders with, and it is
// deliberately none of the two namespaces this project otherwise uses.
//
// Not spawnery-system, the chart's default: a template that forgot
// {{ .Release.Namespace }} and hard-coded the default renders byte-identically
// there, and this whole audit would then confirm a chart that cannot move.
// Not platform-system either, which is where hack/e2e.sh installs -- two
// checks that can pass for the same wrong reason are one check.
const renderNamespace = "audit-system"

// TestTheChartRendersIntoTheNamespaceItIsGiven is the assertion the rest of
// this file rests on. Everything below reads objects out of renderChart, so if
// the chart ignored the release namespace, every one of those assertions would
// still pass while the chart installed nowhere but its default.
func TestTheChartRendersIntoTheNamespaceItIsGiven(t *testing.T) {
	rendered := renderChart(t)

	for key, doc := range rendered {
		if strings.Contains(string(doc), "spawnery-system") {
			t.Errorf("%s carries the literal spawnery-system when rendered into %s:\n%s",
				key, renderNamespace, doc)
		}
	}

	var deploy appsv1.Deployment
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)
	if deploy.Namespace != renderNamespace {
		t.Errorf("the Deployment renders into %q, want %q", deploy.Namespace, renderNamespace)
	}

	var role rbacv1.Role
	renderedManifest(t, "Role/spawnery-operator", &role)
	if role.Namespace != renderNamespace {
		t.Errorf("the Role renders into %q, want %q. A Role in the wrong namespace "+
			"installs cleanly and then denies the operator its own Secret at the "+
			"first certs.Store.Ensure -- which is the failure this milestone exists "+
			"to remove", role.Namespace, renderNamespace)
	}
}

// TestTheSelectorsCarryOnlyTheFrozenPair guards the one label mistake that
// cannot be corrected in place. A Deployment's spec.selector is immutable
// after creation, and the Service and NetworkPolicy would stop matching the
// pod -- which presents as a network fault rather than as a label one.
func TestTheSelectorsCarryOnlyTheFrozenPair(t *testing.T) {
	want := map[string]string{
		"app.kubernetes.io/name":      "spawnery",
		"app.kubernetes.io/component": "operator",
	}

	var deploy appsv1.Deployment
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)
	if !maps.Equal(deploy.Spec.Selector.MatchLabels, want) {
		t.Errorf("the Deployment's selector = %v, want exactly %v",
			deploy.Spec.Selector.MatchLabels, want)
	}
	for k, v := range want {
		if deploy.Spec.Template.Labels[k] != v {
			t.Errorf("the pod template is missing %s=%s; the selector would match nothing", k, v)
		}
	}

	var svc corev1.Service
	renderedManifest(t, "Service/spawnery-operator", &svc)
	if !maps.Equal(svc.Spec.Selector, want) {
		t.Errorf("the Service's selector = %v, want exactly %v", svc.Spec.Selector, want)
	}

	var policy networkingv1.NetworkPolicy
	renderedManifest(t, "NetworkPolicy/spawnery-operator-agent", &policy)
	if !maps.Equal(policy.Spec.PodSelector.MatchLabels, want) {
		t.Errorf("the NetworkPolicy's podSelector = %v, want exactly %v",
			policy.Spec.PodSelector.MatchLabels, want)
	}
}
```

Add `"maps"` to the file's imports.

**Check the Service's and NetworkPolicy's rendered names before you rely on
them** — the keys above assume `Service/spawnery-operator` and
`NetworkPolicy/spawnery-operator-agent`, which is what `config/deploy/` used.
Read the rendered output and correct the keys if they differ.

- [ ] **Step 3: Run to verify it fails**

```
nix develop -c go test ./internal/rbacaudit/ -run TheChartRenders -v
```

Expected: compile failure — `renderChart` and `renderedManifest` are undefined.

- [ ] **Step 4: Write the helpers**

In `internal/rbacaudit/deploy_envtest_test.go`, beside `readManifest`:

```go
// renderChart runs the chart through helm once per package run and returns its
// objects keyed "<Kind>/<name>".
//
// This package used to read config/deploy/ off disk. That directory no longer
// exists: the chart is the only installation form, so the only honest thing to
// audit is what helm produces. The subprocess is the price. helm comes from
// the flake (flake.nix's kubernetes-helm), and nothing in this repository runs
// outside `nix develop`, so its absence means the shell is wrong rather than
// the tree -- which is why the failure below says that rather than reporting a
// parse error on empty input.
//
// Memoised because rendering is a process spawn and eleven tests in this
// package want the same objects. sync.Once rather than a package-level
// initialiser so that a failure is reported against a real *testing.T.
var (
	renderOnce sync.Once
	renderDocs map[string][]byte
	renderErr  error
)

func renderChart(t *testing.T) map[string][]byte {
	t.Helper()
	renderOnce.Do(func() {
		chart := testenv.RepoPath(t, "charts/spawnery")
		cmd := exec.Command("helm", "template", "spawnery", chart,
			"--namespace", renderNamespace)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				renderErr = fmt.Errorf("helm is not on PATH; run this through `nix develop`: %w", err)
				return
			}
			renderErr = fmt.Errorf("helm template: %w\n%s", err, stderr.String())
			return
		}
		renderDocs, renderErr = splitRendered(out)
	})
	if renderErr != nil {
		t.Fatalf("render %s: %v", "charts/spawnery", renderErr)
	}
	return renderDocs
}

// splitRendered indexes helm's multi-document output by Kind and name. A
// duplicate key is an error rather than a last-one-wins: two objects of the
// same kind and name cannot both be installed, and silently keeping one would
// audit an object the cluster never sees.
func splitRendered(out []byte) (map[string][]byte, error) {
	docs := map[string][]byte{}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(out)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			if len(docs) == 0 {
				return nil, errors.New("helm produced no objects at all")
			}
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(doc)) == "" {
			continue
		}
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			return nil, err
		}
		if meta.Kind == "" {
			continue
		}
		key := meta.Kind + "/" + meta.Metadata.Name
		if _, dup := docs[key]; dup {
			return nil, fmt.Errorf("the chart renders %s twice", key)
		}
		docs[key] = doc
	}
}

// renderedManifest decodes one rendered object, strictly, for the reason
// readManifest's own comment gives: a plain Unmarshal drops keys the target
// type does not have, so a misspelled field would decode to a zero value and
// every assertion below would then be checking something nobody set.
func renderedManifest[T any](t *testing.T, key string, into *T) {
	t.Helper()
	doc, ok := renderChart(t)[key]
	if !ok {
		keys := make([]string, 0, len(renderDocs))
		for k := range renderDocs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("the chart renders no %s; it renders: %s", key, strings.Join(keys, ", "))
	}
	if err := yaml.UnmarshalStrict(doc, into); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
}
```

Add `bytes`, `fmt`, `os/exec`, `sort` and `sync` to the imports.

- [ ] **Step 5: Move the twenty-one call sites**

Replace every `readManifest(t, "config/deploy/…", &x)` with the equivalent
`renderedManifest(t, "<Kind>/<name>", &x)`. There are five in
`audit_envtest_test.go` (`:59-63`) and sixteen in `deploy_envtest_test.go`.
The mapping:

| Old path | New key |
|---|---|
| `config/deploy/serviceaccount.yaml` | `ServiceAccount/spawnery-operator` |
| `config/deploy/clusterrolebinding.yaml` | `ClusterRoleBinding/spawnery-operator` |
| `config/deploy/rolebinding.yaml` | `RoleBinding/spawnery-operator` |
| `config/deploy/deployment.yaml` | `Deployment/spawnery-operator` |
| `config/deploy/service.yaml` | `Service/spawnery-operator` |
| `config/deploy/networkpolicy.yaml` | `NetworkPolicy/spawnery-operator-agent` |

`config/deploy/namespace.yaml` has no counterpart. Every test that reads it
constructs the object instead:

```go
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: renderNamespace}}
```

and any assertion that compared another object's namespace against `ns.Name`
keeps working unchanged.

`generatedRoles` and `readGeneratedRoles` (`:72`, `:126`) move too: the roles
now come from the rendered chart. Replace the `readMultiDocManifest` loop over
`config/rbac/role.yaml` with two `renderedManifest` calls,
`ClusterRole/spawnery-operator` and `Role/spawnery-operator`, and keep every
diagnostic message the old loop produced — including its refusal of a second
object of the same kind, which `splitRendered` now enforces globally. Update
the `generatedRoles` doc comment, which describes a file, to describe the
rendering.

`readForwardingSecretReader` (`audit_envtest_test.go:294`) and
`readMultiDocManifest` **stay as they are**:
`config/rbac/forwarding-secret-reader.yaml` is still a file, applied per game
namespace, outside the chart.

- [ ] **Step 6: Run the package**

```
nix develop -c go test ./internal/rbacaudit/ -v
```

Expected: PASS, including the two new tests and every pre-existing one. If a
pre-existing test needed a change beyond the mechanical substitution above,
say exactly what and why in your report.

- [ ] **Step 7: Mutate, and report what happened**

For each: apply, run `nix develop -c go test ./internal/rbacaudit/`, record the
verbatim failure, revert.

1. In `charts/spawnery/templates/rbac.yaml`, replace
   `{{ .Release.Namespace }}` with the literal `spawnery-system`. Expected:
   `TestTheChartRendersIntoTheNamespaceItIsGiven` fails twice — on the literal
   scan and on the Role's namespace. **This is the milestone's central
   defect and the test that must catch it.**
2. In `_helpers.tpl`, add `app.kubernetes.io/instance: {{ .Release.Name }}` to
   `spawnery.selectorLabels`. Expected:
   `TestTheSelectorsCarryOnlyTheFrozenPair` fails on all three selectors.
3. In `values.yaml`, set `networkPolicy.enabled: false`. Expected:
   `TestTheSelectorsCarryOnlyTheFrozenPair` fails in `renderedManifest` with
   `the chart renders no NetworkPolicy/spawnery-operator-agent; it renders: …`.
   **Judge this one:** it means the audit's default rendering assumes the
   policy is on. That is correct — the chart's default is on and the audit
   audits the default — but say in your report whether any assertion would
   silently weaken if somebody flipped that default, and if so, whether the
   value belongs in the render invocation explicitly.
4. In `deploy_envtest_test.go`, change `renderNamespace` to
   `"spawnery-system"`. Expected: `TestTheChartRendersIntoTheNamespaceItIsGiven`
   still passes — because rendered at the default, a hard-coded literal and a
   correct template are indistinguishable. **Report this as the reason the
   constant is what it is**, and confirm the comment on it says so.

- [ ] **Step 8: Full suite and commit**

```
nix develop -c make test
git add -A
git commit
```

Subject: `refactor(6d): audit the chart, not the files it replaced`.
The body should say that the audit now reads what a user installs for the
first time, name the new `helm`-on-`PATH` requirement, and name the render
namespace and why it is neither of the other two.

---

### Task 4: `make e2e` installs somewhere else

**Files:**
- Modify: `hack/e2e.sh:99-102` (the four apply lines), `:155` (the
  startup-deadline patch)
- Modify: `test/e2e/e2e_test.go:48` (`operatorNamespace`)

**Interfaces:**
- Consumes: the complete chart from Tasks 1 and 2.
- Produces: an E2E run installed by `helm install` into `platform-system`.
  Nothing later consumes anything from this task.

- [ ] **Step 1: Read the script's install block and the patch**

```
nix develop -c sed -n '85,160p' hack/e2e.sh
```

Read the comment above the apply lines in full. It documents a real failure
reproduced on this script's first run — `namespaces "spawnery-system" not
found`, because `kubectl apply -f config/deploy/` walks the directory
alphabetically and the Deployment precedes the Namespace. Your change deletes
that hazard rather than porting it, and the comment should be replaced by one
that says so, not merely removed. 6a's handover put it exactly this way: "Helm
has its own answer to install ordering; use it, rather than porting the
script's sequence."

Read `:145-158` too. Three edits are applied to the manifest there; the
`--startup-deadline=20s` one becomes a Helm value, and the other two may not.
Establish what each of the three is before you change any of them.

- [ ] **Step 2: Replace the install**

The four lines at `:99-102` become one `helm install`, with the image and the
deadline passed as values:

```bash
helm install spawnery charts/spawnery \
	--namespace "$OPERATOR_NAMESPACE" \
	--create-namespace \
	--set image.repository="$image_repo" \
	--set image.tag="$image_tag" \
	--set image.pullPolicy=Never \
	--set operator.startupDeadline=20s \
	--wait --timeout 5m
```

with, near the top of the script beside the other configuration:

```bash
# Deliberately not spawnery-system, the chart's own default. Every one of this
# run's scenarios is thereby a guard over the chart's templating: a literal
# that survived puts the Role in a namespace holding no operator, the operator
# fails at its first certs.Store.Ensure with Forbidden, and
# theOperatorWasNeverDenied -- the last scenario, unchanged since 6a -- is what
# catches it. The name shares nothing with the default on purpose: a near-miss
# like spawnery-operators reads as a variant and invites somebody to tidy it
# back.
OPERATOR_NAMESPACE=platform-system
```

The two image variables come from the nix evals the script already runs at
`:72`, which today build one `repo:tag` string:

```bash
image="$(nix eval --raw '.#operator-image.imageName'):$(nix eval --raw '.#operator-image.imageTag')"
```

Split it into the two halves Helm wants, from the same two evals — do not
parse the combined string apart, and do not add a third source:

```bash
image_repo="$(nix eval --raw '.#operator-image.imageName')"
image_tag="$(nix eval --raw '.#operator-image.imageTag')"
```

**Drop `--wait`.** The script already waits, at `:161`:
`kubectl -n spawnery-system rollout status deployment/spawnery-operator --timeout="${DEADLINE}s"`.
Keep that line, point it at `$OPERATOR_NAMESPACE`, and leave `--wait` off the
install so there is one place that decides how long the run waits.

- [ ] **Step 3: Remove the whole JSON patch**

The `kubectl patch` block at `:150-159` makes exactly three edits, and **all
three are now chart values**, so the entire patch — and the `kubectl patch`
call around it — goes:

| Patch operation | Becomes |
|---|---|
| `replace` `/spec/template/spec/containers/0/image` with `$image` | `--set image.repository=$image_repo --set image.tag=$image_tag` |
| `add` `imagePullPolicy: Never` | `--set image.pullPolicy=Never` |
| `add` `--startup-deadline=20s` to args | `--set operator.startupDeadline=20s` |

The comment above the patch explains two of these and is worth keeping in
substance: the image is set so the run tests the bits just built rather than
whatever a registry holds, and `imagePullPolicy: Never` makes that a guarantee
rather than a hope — a missing local image then fails loudly instead of being
fetched. Move that reasoning to the `helm install` invocation.

The comment's third paragraph, about appending a second `--startup-deadline`
so "the manifest stays the single place the flags are written" and Go's flag
package resolving a repeated flag to the last one, describes a mechanism that
no longer exists. Delete it rather than adapting it: there is now one
occurrence of the flag, set from one value.

- [ ] **Step 4: Point the tests at the new namespace**

`test/e2e/e2e_test.go:48`:

```go
	// operatorNamespace is where hack/e2e.sh installs the chart, and it is
	// deliberately not the chart's own default. See the comment on
	// OPERATOR_NAMESPACE in hack/e2e.sh: every scenario in this package is a
	// guard over the chart's templating precisely because this is not
	// spawnery-system.
	operatorNamespace = "platform-system"
```

- [ ] **Step 5: Run it**

```
E2E_KEEP=1 nix develop -c make e2e
```

Run it in the **foreground**. Expected: eighteen scenarios, all passing,
`theOperatorWasNeverDenied` last. Report the scenario count and the wall-clock
time.

If it fails, inspect the kept cluster before changing anything:

```
nix develop -c kubectl -n platform-system get pods,roles,rolebindings
nix develop -c kubectl -n platform-system logs deploy/spawnery-operator | tail -50
nix develop -c helm -n platform-system get manifest spawnery | head -40
```

A `Forbidden` on a Secret in the operator's log means a literal survived the
templating — which is the defect this task exists to expose, not a reason to
change the namespace back.

- [ ] **Step 6: Mutate, and report what happened**

Each needs its own `make e2e` run in the foreground. Budget the time.

1. In `charts/spawnery/templates/rolebinding.yaml`, replace
   `namespace: {{ .Release.Namespace }}` on the RoleBinding's own metadata
   with the literal `spawnery-system`. Expected: the install succeeds and the
   operator then fails. **Report exactly how it presents** — which scenario
   fails first, what the operator's log says, and whether
   `theOperatorWasNeverDenied` is among them. That answer is the evidence for
   this milestone's central claim and belongs verbatim in your report.
2. Revert 1, then set `operator.startupDeadline` back to `5m` in the
   `helm install` invocation (i.e. drop the `--set`). Expected: the
   startup-deadline scenario
   (`theStartupDeadlineFailsAServerAndClearsIt`) times out, because it depends
   on the short deadline the script used to patch in. This confirms the value
   actually reaches the container's args.

- [ ] **Step 7: Commit**

```
nix develop -c make test
git add -A
git commit
```

Subject: `test(6d): install the chart into a namespace it does not default to`.
The body should name what the run proved and, precisely, what mutation 1
demonstrated about who catches a leaked literal.

---

### Task 5: The last consumers, and the deletion

**Files:**
- Modify: `hack/publish.sh:157` and the block around it
- Modify: `Makefile` — new `chart-lint` target, wired into `test`
- Delete: `config/deploy/` — all seven files

**Interfaces:**
- Consumes: everything from Tasks 1–4. Nothing may still read `config/deploy/`
  when this task ends.
- Produces: `make chart-lint`.

- [ ] **Step 1: Prove nothing still reads the directory**

```
nix develop -c grep -rn "config/deploy" --include=*.go --include=*.sh --include=Makefile --include=*.yaml . | grep -v "^./docs"
```

Expected at this point: only `hack/publish.sh`. If anything else appears, it
was missed by an earlier task — fix it here and say which task missed it.

- [ ] **Step 2: Move the digest**

Read `hack/publish.sh:145-175` first, including the comment explaining why a
digest rather than a tag. The rewrite target becomes
`charts/spawnery/values.yaml`, and the shape of the edit changes: the old code
replaced an `image:` line carrying `name:tag` with `name@digest`; the new code
sets the `digest:` key under `image:` and leaves `repository` and `tag` alone,
because the chart's `spawnery.image` helper already prefers the digest when it
is non-empty.

Keep the script's existing guard style — it fails closed elsewhere, and a
silent no-op here would publish an image and then install a different one.
Anchor the substitution to the exact line and refuse if it is absent.

- [ ] **Step 3: Add `make chart-lint`**

```make
.PHONY: chart-lint
chart-lint:
	helm lint charts/spawnery
	helm template spawnery charts/spawnery --namespace chart-lint-check >/dev/null
```

and add `chart-lint` to whatever `test` already depends on. The second line is
not redundant: `helm lint` accepts templates that fail to render with a real
namespace, and a chart that lints but does not template is a chart nobody can
install.

- [ ] **Step 4: Delete `config/deploy/`**

```
git rm -r config/deploy/
```

- [ ] **Step 5: Run everything**

```
nix develop -c make test
nix develop -c make e2e
```

Both in the foreground. Expected: `make test` green including the new
`chart-lint`; `make e2e` green with eighteen scenarios.

- [ ] **Step 6: Mutate, and report what happened**

1. Break a template so it lints but does not render — in
   `templates/service.yaml`, change `{{ .Release.Namespace }}` to
   `{{ .Release.Namspace }}`. Run `nix develop -c make chart-lint`. Expected:
   `helm lint` alone may pass; the `helm template` line fails. **Report which
   of the two caught it**, because that is the justification for having both.
2. In `hack/publish.sh`, break the anchor its digest substitution greps for.
   Expected: the script refuses loudly. Confirm the refusal reaches an exit
   code rather than only a log line. Since a real `make publish` needs a
   registry token this repository does not have, exercise the substitution
   path directly with `WRITE_DIGEST=1` and a fabricated digest if the script
   allows it, and say plainly what you could and could not drive.

- [ ] **Step 7: Prove `helm uninstall` leaves the CRDs standing**

Unlike upgrade, this one is observable here, and the spec's acceptance
criterion 8 asks for it. Against a cluster kept from Step 5:

```
E2E_KEEP=1 nix develop -c make e2e
nix develop -c kubectl get crd | grep spawnery
nix develop -c helm uninstall spawnery --namespace platform-system
nix develop -c kubectl get crd | grep spawnery
nix develop -c kubectl -n platform-system get deploy,svc,networkpolicy
```

Expected: four CRDs before, the **same four** after, and the Deployment,
Service and NetworkPolicy gone. Record both `kubectl get crd` outputs verbatim.

This is a piece of evidence recorded in your report, not a test — an E2E
scenario that uninstalled the operator would break every scenario ordered
after it. Delete the cluster afterwards.

If the CRDs do **not** survive, the `helm.sh/resource-policy: keep` annotation
is not reaching them; check `charts/spawnery/templates/crds.yaml` and report it
rather than working around it. That annotation is the only thing standing
between `helm uninstall` and every `Network`, `ServerGroup`, `ProxyGroup` and
`Server` in the cluster.

- [ ] **Step 8: Commit**

```
git add -A
git commit
```

Subject: `feat(6d): the chart is the only way in`.
The body should list what was deleted, confirm that the grep in Step 1 is now
empty, and say which parts of `hack/publish.sh` were exercised and which were
not.

---

### Task 6: The record

**Files:**
- Create: `charts/spawnery/README.md`, `docs/handover-milestone-6d.md`
- Modify: `README.md`, `docs/known-issues.md`

**Interfaces:**
- Consumes: everything. Written last because it describes what the run did,
  not what the plan expected.

- [ ] **Step 1: Write the chart's README**

`charts/spawnery/README.md` is what a stranger reads before installing. It must
carry, in this order:

1. The install command, with `--create-namespace`.
2. **The one manual step the chart cannot make**, given the weight §9 of the
   spec gives it: `config/rbac/forwarding-secret-reader.yaml` is applied *per
   game namespace*, and its RoleBinding subject names
   `namespace: spawnery-system` on line 65. An operator installed elsewhere
   must have that line changed before any `Network` in that game namespace can
   be accepted. Without it the `Network` reports `SecretReadForbidden` and
   every group in the namespace refuses with `NetworkNotAccepted`. Put this in
   the installation steps, not in a footnote — the failure names the secret,
   not the namespace, and a reader will look in the wrong place.
3. The values table: every key in `values.yaml`, its default, and what it does.
4. **What uninstalling does and does not remove.** The four CRDs carry
   `helm.sh/resource-policy: keep`, so `helm uninstall` leaves them — and
   therefore leaves every `Network`, `ServerGroup`, `ProxyGroup` and `Server`
   in the cluster. State the manual `kubectl delete crd` that a full removal
   needs, and state what deleting them destroys.
5. **That no `helm upgrade` has ever been run.** The CRDs are in `templates/`
   so that upgrades carry schema changes, and nothing in this repository
   observes an upgrade. Say it in those words.

- [ ] **Step 2: Correct what the deletion falsified**

```
nix develop -c grep -rn "config/deploy" README.md docs/known-issues.md docs/handover-milestone-*.md
```

Every hit is a document describing a directory that no longer exists. Rewrite
each to the present tense of what is now true. Read two neighbouring entries in
`docs/known-issues.md` before adding or editing one, and follow its format.

`README.md`'s roadmap paragraph names 6d as upcoming; it becomes 6e, CI, plus
the RKE2 rollout, and points a 6e reader at `docs/handover-milestone-6d.md`.
Add the "Milestone 6d is done:" narrative paragraph in the same place in the
sequence and the same voice as the 6a, 6b and 6c paragraphs that precede it —
each leads with what the milestone made true and then says what it deliberately
did not establish. For 6d the second half is that no upgrade was ever run.

**Do not edit anything under `docs/superpowers/plans/`.** Plan documents are a
historical record of what was planned; specs and `docs/known-issues.md` are the
living references. That is this project's own rule, from milestone 6b.

- [ ] **Step 3: Add the superseded header to 6c's handover**

`docs/handover-milestone-6c.md` becomes the previous document.
`docs/handover-milestone-6b.md` carries the pattern: a bolded paragraph after
the Status block naming the successor with a relative link, saying the document
is kept unedited, and naming which of its claims are now false.

Read 6c's §3 ("What 6d finds in place") and check each claim against the tree
as 6d leaves it. Its first claim — that `spawnery-system` is hard-wired in
three places — is exactly what this milestone changed, and the *count* of
claims must be the real one. Count them yourself; 6c's own Task 7 shipped a
wrong count twice and had to be corrected in a review round.

- [ ] **Step 4: Write the handover**

`docs/handover-milestone-6d.md`, in the form of its two predecessors, for
someone with no memory of how any of this was built, starting 6e. It must
cover:

- **What was driven and what only exists.** The chart is installed by every
  `make e2e` run and audited by `internal/rbacaudit` against a third
  namespace. No `helm upgrade` was run; the CRD lifecycle is designed and
  unobserved. No chart was published anywhere.
- **What 6e finds in place** — read off the code, not off this plan. In
  particular: what `make manifests` now does, that
  `charts/spawnery/templates/{rbac,crds}.yaml` are generated and carry a
  header saying so, that `internal/rbacaudit`'s tests now require `helm` on
  `PATH`, and the three distinct namespaces with why each is what it is.
- **What the RKE2 rollout owes**, carried forward from 6c's §4 unchanged, plus
  the manual `forwarding-secret-reader.yaml` edit from §9 of the spec.
- **Every finding this milestone's reviews produced**, with what caught each
  one. The SDD ledger is the only place that list exists in full.

- [ ] **Step 5: Verify every citation**

For every file path, line number, function name, constant and command you wrote
into the handover, the chart README and the known-issues entries, grep for it.
A handover is read by someone who cannot check your work cheaply, and a wrong
line number costs them more than a missing one. Report anything this pass
caught before you fixed it — that report is the evidence the pass happened.

```
nix develop -c make test
```

- [ ] **Step 6: Commit**

```
git add -A
git commit
```

Subject: `docs(6d): the chart's own README, and what 6e inherits`.

---

## Notes for the executor

**On the review loop.** This project's record across milestones 5, 6b and 6c is
fifteen findings, every one produced by a mutation and none by reading, and
every one sitting in test code the plan specified verbatim — including this
plan's author's. Treat every block above as a proposal that has not been run.
When a mutation you were told to expect to fail does not fail, that is the
finding, and it outranks finishing the task on time. Milestone 6c's worst
defect — a reconcile hot loop the milestone had measured at 3,940 occurrences
and written down as a normal cost — was found by the final whole-branch review
after seven task reviews had passed over it.

**On `helm template` versus a cluster.** Rendering proves the chart produces
objects; it does not prove the API server accepts them, that RBAC suffices, or
that the operator starts. Task 4 is the only thing in this milestone that
proves any of those, and it proves them only for the one namespace it installs
into. Do not let a document claim that a render established more.

**On the three namespaces.** `spawnery-system` (chart default),
`platform-system` (`make e2e`), and the render helper's own. They exist as
three so that two checks cannot pass for the same wrong reason. If any task
finds itself wanting to collapse two of them, that is a finding to report, not
a simplification to make.
