# RBAC-Audit in envtest (Ebene A) — Implementierungsplan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `make test` schlägt fehl, sobald die ClusterRole des Operators ein Verb nicht gewährt, das sein Code braucht, oder eines gewährt, das er nicht braucht.

**Architecture:** Eine handgepflegte Tabelle in `internal/rbacaudit` sagt unabhängig vom generierten Manifest, was der Code braucht. Reine Funktionen falten die `PolicyRule`s der ClusterRole zu Tripeln auf und vergleichen beide Richtungen. Ein envtest-Test wendet die Deployment-Manifeste an, leitet das zu prüfende Subjekt aus ClusterRoleBinding und Deployment ab und fragt jedes Tripel per `SubjectAccessReview` gegen den echten RBAC-Authorizer ab.

**Tech Stack:** Go 1.26, envtest (kube-apiserver 1.36, gestartet mit `--authorization-mode=RBAC`), `k8s.io/api/rbac/v1`, `k8s.io/api/authorization/v1`, `sigs.k8s.io/yaml`.

## Global Constraints

- Go-Modulpfad `github.com/spawnery/spawnery`. API-Gruppe `spawnery.cloud/v1alpha1`.
- Code, Kommentare, Log- und Condition-Messages auf **Englisch**. Spec, Plan und Commit-Messages auf **Deutsch**, schlicht formuliert, ohne `feat:`-artiges Präfix, **mit echten Umlauten**.
- Jeder Commit endet mit einer Leerzeile und dann:
  ```
  Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Pqicn4xG3Xtvz8F3kfrhTY
  ```
- Alle Kommandos laufen in der Dev-Shell: `nix develop -c <kommando>` (bis zu 600000 ms).
- Apache-2.0-Header aus `hack/boilerplate.go.txt` auf jeder handgeschriebenen `.go`-Datei.
- Keine Abhängigkeitsversion in `go.mod` ändern.
- `internal/phase` und `internal/agent` importieren weiterhin nichts außerhalb der Standardbibliothek.
- Kein `git clean` — es gibt unversionierte Arbeitsdateien.
- Namespace des Operators: `spawnery-system`. ServiceAccount: `spawnery-operator`. ClusterRole und ClusterRoleBinding heißen ebenfalls `spawnery-operator`. Geprüfter Ziel-Namespace für namespaced Ressourcen: `minecraft`.

---

### Task 1: Leases-Recht ergänzen und Deployment-Manifeste

**Files:**
- Modify: `internal/controller/setup.go` (RBAC-Marker ergänzen)
- Modify: `config/rbac/role.yaml` (generiert)
- Create: `config/deploy/namespace.yaml`
- Create: `config/deploy/serviceaccount.yaml`
- Create: `config/deploy/clusterrolebinding.yaml`
- Create: `config/deploy/deployment.yaml`
- Modify: `internal/testenv/testenv.go` (Helfer `RepoPath`)
- Test: `internal/rbacaudit/deploy_envtest_test.go`

**Interfaces:**
- Consumes: `testenv.Client`, `testenv.Namespace`, `testenv.Stop` aus dem bestehenden Paket.
- Produces:
  - `testenv.RepoPath(t *testing.T, rel string) string` — löst einen repo-relativen Pfad auf, indem es vom Arbeitsverzeichnis aufwärts nach `go.mod` sucht.
  - Die vier Manifeste unter `config/deploy/`.
  - Die ClusterRole enthält zusätzlich `coordination.k8s.io/leases` mit `create`, `get`, `update`.

**Warum das Leases-Recht fehlt und warum es zuerst kommt:** Leader-Election ist im Operator standardmäßig aktiv (`--leader-elect=true`) und nutzt einen `Lease` als Sperre. Kein kubebuilder-Marker deklariert dieses Recht, also fehlt es in der generierten ClusterRole. Ein Deployment würde beim Start in ein `Forbidden` laufen, bevor der erste Reconcile passiert. Die Manifeste dieses Tasks wären ohne das Recht von vornherein kaputt.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`internal/rbacaudit/deploy_envtest_test.go`:

```go
package rbacaudit_test

import (
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

// readManifest decodes a single-document YAML manifest from the repository.
func readManifest[T any](t *testing.T, rel string, into *T) {
	t.Helper()
	raw, err := os.ReadFile(testenv.RepoPath(t, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", rel, err)
	}
}

// apply creates objects that several tests in this package share. The cluster
// scoped ones — ClusterRole and ClusterRoleBinding — outlive a single test in
// the shared control plane, so creating them twice is normal and not a failure.
func apply(t *testing.T, objs ...client.Object) {
	t.Helper()
	c, ctx := testenv.Client(t)
	for _, obj := range objs {
		if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
	}
}

// TestDeployManifestsAreAcceptedAndConsistent applies the deployment manifests
// to a real API server and checks that they refer to each other correctly.
// A binding that names the wrong role, or a deployment that runs under the
// wrong ServiceAccount, would leave the operator without permissions while
// every manifest on its own still looked fine.
func TestDeployManifestsAreAcceptedAndConsistent(t *testing.T) {
	c, ctx := testenv.Client(t)

	var ns corev1.Namespace
	var sa corev1.ServiceAccount
	var role rbacv1.ClusterRole
	var binding rbacv1.ClusterRoleBinding
	var deploy appsv1.Deployment

	readManifest(t, "config/deploy/namespace.yaml", &ns)
	readManifest(t, "config/deploy/serviceaccount.yaml", &sa)
	readManifest(t, "config/rbac/role.yaml", &role)
	readManifest(t, "config/deploy/clusterrolebinding.yaml", &binding)
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

	apply(t, &ns, &sa, &role, &binding, &deploy)

	if ns.Name != "spawnery-system" {
		t.Errorf("namespace = %q, want spawnery-system", ns.Name)
	}
	if sa.Namespace != ns.Name {
		t.Errorf("serviceAccount namespace = %q, want %q", sa.Namespace, ns.Name)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != role.Name {
		t.Errorf("roleRef = %s/%s, want ClusterRole/%s",
			binding.RoleRef.Kind, binding.RoleRef.Name, role.Name)
	}
	if len(binding.Subjects) != 1 {
		t.Fatalf("binding has %d subjects, want exactly one", len(binding.Subjects))
	}
	subj := binding.Subjects[0]
	if subj.Kind != "ServiceAccount" || subj.Name != sa.Name || subj.Namespace != sa.Namespace {
		t.Errorf("subject = %s %s/%s, want ServiceAccount %s/%s",
			subj.Kind, subj.Namespace, subj.Name, sa.Namespace, sa.Name)
	}
	if deploy.Namespace != ns.Name {
		t.Errorf("deployment namespace = %q, want %q", deploy.Namespace, ns.Name)
	}
	if got := deploy.Spec.Template.Spec.ServiceAccountName; got != sa.Name {
		t.Errorf("deployment serviceAccountName = %q, want %q — the operator would "+
			"run as the namespace default account and have no permissions at all", got, sa.Name)
	}
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
		t.Errorf("replicas = %v, want 1", deploy.Spec.Replicas)
	}

	got := &appsv1.Deployment{}
	key := types.NamespacedName{Name: deploy.Name, Namespace: deploy.Namespace}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
}

// TestOperatorPodIsRestrictedCompliant guards the design decision that the
// operator itself runs under Pod Security "restricted" — the same profile it
// enforces on the game servers it creates.
func TestOperatorPodIsRestrictedCompliant(t *testing.T) {
	var deploy appsv1.Deployment
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

	pod := deploy.Spec.Template.Spec
	if pod.SecurityContext == nil ||
		pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("runAsNonRoot must be true")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccompProfile must be RuntimeDefault")
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("got %d containers, want exactly one", len(pod.Containers))
	}
	sc := pod.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("container security context missing")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must be true")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities = %+v, want drop ALL", sc.Capabilities)
	}
}

// TestLeaderElectionPermissionIsGranted is the regression test for a real gap:
// leader election is on by default and locks on a Lease, but no kubebuilder
// marker declared that permission, so the generated role never granted it and
// the operator would have failed on startup with Forbidden.
func TestLeaderElectionPermissionIsGranted(t *testing.T) {
	var role rbacv1.ClusterRole
	readManifest(t, "config/rbac/role.yaml", &role)

	want := map[string]bool{"create": false, "get": false, "update": false}
	for _, rule := range role.Rules {
		if !contains(rule.APIGroups, "coordination.k8s.io") || !contains(rule.Resources, "leases") {
			continue
		}
		for _, v := range rule.Verbs {
			if _, ok := want[v]; ok {
				want[v] = true
			}
		}
	}
	for verb, found := range want {
		if !found {
			t.Errorf("clusterrole does not grant %q on coordination.k8s.io/leases — "+
				"leader election would fail with Forbidden on startup", verb)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
```

`sigs.k8s.io/yaml` steht bereits als indirekte Abhängigkeit in `go.mod`; `go mod tidy` verschiebt es nach dem ersten Import zu den direkten. Das ist keine Versionsänderung.

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/rbacaudit/... -v`
Expected: FAIL — `no required module provides package .../internal/rbacaudit`, und nach dem Anlegen des Verzeichnisses `undefined: testenv.RepoPath`.

- [ ] **Step 3: `testenv.RepoPath` ergänzen**

In `internal/testenv/testenv.go` hinzufügen (die bestehende `crdPath`-Funktion bleibt unverändert):

```go
// RepoPath resolves a repository-relative path by walking up from the test's
// working directory until it finds go.mod. Tests run with their package
// directory as the working directory, so a plain relative path would break as
// soon as a test moves to another package.
func RepoPath(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}
```

- [ ] **Step 4: RBAC-Marker für Leases ergänzen**

In `internal/controller/setup.go` über `func SetupAll` — **mit einer Leerzeile
zwischen Marker und dem Doc-Kommentar von `SetupAll`**:

```go
// Leader election locks on a Lease in the operator's own namespace. It is not
// tied to any single controller, which is why the marker lives here on the
// wiring rather than on a reconciler.
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=create;get;update

// SetupAll registers every controller and the orphan sweep with the manager.
func SetupAll(mgr ctrl.Manager, opts Options) error {
```

Die Leerzeile ist nicht Kosmetik. Ohne sie zieht Gos Parser den Marker in den
`Doc`-Kommentar von `SetupAll` hinein, und controller-gen findet ihn dort nicht
mehr — `make manifests` erzeugt dann **keine Änderung und keine Fehlermeldung**.
Ein stiller Fehlschlag. Dieselbe Trennung durch Leerzeilen findet sich bereits
bei dem Marker in `orphan.go`.

Dann neu generieren:

```bash
nix develop -c make manifests
```

Expected: `config/rbac/role.yaml` enthält jetzt einen Block für `coordination.k8s.io`/`leases` mit `create`, `get`, `update`.

- [ ] **Step 5: Die vier Manifeste schreiben**

`config/deploy/namespace.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: spawnery-system
  labels:
    app.kubernetes.io/name: spawnery
```

`config/deploy/serviceaccount.yaml`:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: spawnery-operator
  namespace: spawnery-system
  labels:
    app.kubernetes.io/name: spawnery
```

`config/deploy/clusterrolebinding.yaml`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: spawnery-operator
  labels:
    app.kubernetes.io/name: spawnery
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: spawnery-operator
subjects:
  - kind: ServiceAccount
    name: spawnery-operator
    namespace: spawnery-system
```

`config/deploy/deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: spawnery-operator
  namespace: spawnery-system
  labels:
    app.kubernetes.io/name: spawnery
    app.kubernetes.io/component: operator
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: spawnery
      app.kubernetes.io/component: operator
  template:
    metadata:
      labels:
        app.kubernetes.io/name: spawnery
        app.kubernetes.io/component: operator
    spec:
      serviceAccountName: spawnery-operator
      terminationGracePeriodSeconds: 10
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: operator
          # Ebene B ersetzt dieses Image durch das lokal gebaute.
          image: ghcr.io/spawnery/spawnery-operator:dev
          args:
            - --leader-elect=true
            - --startup-deadline=20s
            - --metrics-bind-address=:8080
            - --health-probe-bind-address=:8081
          ports:
            - name: metrics
              containerPort: 8080
            - name: health
              containerPort: 8081
          livenessProbe:
            httpGet:
              path: /healthz
              port: health
            initialDelaySeconds: 15
            periodSeconds: 20
          readinessProbe:
            httpGet:
              path: /readyz
              port: health
            initialDelaySeconds: 5
            periodSeconds: 10
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              memory: 256Mi
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop:
                - ALL
```

- [ ] **Step 6: Tests laufen lassen, Erfolg prüfen**

```bash
nix develop -c go test ./internal/rbacaudit/... -v
nix develop -c go test ./... 
```

Expected: PASS für `TestDeployManifestsAreAcceptedAndConsistent`, `TestOperatorPodIsRestrictedCompliant` und `TestLeaderElectionPermissionIsGranted`; die bestehende Suite bleibt grün.

- [ ] **Step 7: Mutationsnachweis**

Jeder der drei Tests muss die Regel wirklich bewachen. Nacheinander mutieren, Test laufen lassen, zurücknehmen:

1. In `deployment.yaml` `serviceAccountName` entfernen → `TestDeployManifestsAreAcceptedAndConsistent` muss fehlschlagen.
2. In `clusterrolebinding.yaml` `roleRef.name` auf `falsch` setzen → derselbe Test muss fehlschlagen.
3. Den Leases-Marker aus `setup.go` entfernen und `make manifests` laufen lassen → `TestLeaderElectionPermissionIsGranted` muss fehlschlagen. Danach Marker zurück und erneut generieren.

Im Bericht festhalten, welche Mutation welchen Test umgeworfen hat.

- [ ] **Step 8: Commit**

```bash
git add internal/controller/setup.go config/rbac config/deploy internal/testenv internal/rbacaudit
git commit -m "Leases-Recht für Leader-Election und Deployment-Manifeste"
```

---

### Task 2: Reine Rechtelogik

**Files:**
- Create: `internal/rbacaudit/permissions.go`
- Test: `internal/rbacaudit/permissions_test.go`

**Interfaces:**
- Consumes: `k8s.io/api/rbac/v1`.
- Produces:
  - `rbacaudit.Permission{Group, Resource, Subresource, Verb, Why string}` mit `Key() string`.
  - `rbacaudit.ExpandRules(rules []rbacv1.PolicyRule) ([]Permission, error)`.
  - `rbacaudit.Diff{Missing, Extra []Permission}` und `rbacaudit.Compare(required, granted []Permission) Diff`.

Dieses Paket kennt kein Kubernetes-Cluster und keinen Client — nur Datenstrukturen. Deshalb sind alle Regeln hier tabellengetrieben prüfbar.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`internal/rbacaudit/permissions_test.go`:

```go
package rbacaudit

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func perm(group, resource, sub, verb string) Permission {
	return Permission{Group: group, Resource: resource, Subresource: sub, Verb: verb}
}

func keys(perms []Permission) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, p.Key())
	}
	return out
}

func TestKeyIgnoresWhy(t *testing.T) {
	a := Permission{Group: "", Resource: "pods", Verb: "get", Why: "eine Stelle"}
	b := Permission{Group: "", Resource: "pods", Verb: "get", Why: "eine andere"}
	if a.Key() != b.Key() {
		t.Errorf("Key differs on Why alone: %q vs %q", a.Key(), b.Key())
	}
}

func TestKeyDistinguishesSubresource(t *testing.T) {
	bare := perm("spawnery.cloud", "servers", "", "update")
	status := perm("spawnery.cloud", "servers", "status", "update")
	if bare.Key() == status.Key() {
		t.Fatalf("bare and subresource share a key: %q", bare.Key())
	}
}

func TestExpandRules(t *testing.T) {
	cases := []struct {
		name  string
		rules []rbacv1.PolicyRule
		want  []string
	}{
		{
			name: "cross product of groups, resources and verbs",
			rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"spawnery.cloud"},
				Resources: []string{"networks", "servergroups"},
				Verbs:     []string{"get", "list"},
			}},
			want: []string{
				"spawnery.cloud/networks:get",
				"spawnery.cloud/networks:list",
				"spawnery.cloud/servergroups:get",
				"spawnery.cloud/servergroups:list",
			},
		},
		{
			name: "the core group is the empty string",
			rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"delete"},
			}},
			want: []string{"/pods:delete"},
		},
		{
			name: "a subresource is split off the resource",
			rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"spawnery.cloud"},
				Resources: []string{"servers/status"},
				Verbs:     []string{"update"},
			}},
			want: []string{"spawnery.cloud/servers/status:update"},
		},
		{
			name:  "no rules yield no permissions",
			rules: nil,
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpandRules(tc.rules)
			if err != nil {
				t.Fatalf("ExpandRules: %v", err)
			}
			gotKeys := keys(got)
			if len(gotKeys) != len(tc.want) {
				t.Fatalf("got %v, want %v", gotKeys, tc.want)
			}
			for i := range gotKeys {
				if gotKeys[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", gotKeys, tc.want)
				}
			}
		})
	}
}

// TestExpandRulesRejectsWildcards is the point of this function: a wildcard
// grants everything in its position, so it can never be matched against a
// finite table. Treating it as an over-grant is the only honest answer.
func TestExpandRulesRejectsWildcards(t *testing.T) {
	cases := []struct {
		name string
		rule rbacv1.PolicyRule
	}{
		{"wildcard group", rbacv1.PolicyRule{
			APIGroups: []string{"*"}, Resources: []string{"pods"}, Verbs: []string{"get"}}},
		{"wildcard resource", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"*"}, Verbs: []string{"get"}}},
		{"wildcard verb", rbacv1.PolicyRule{
			APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"*"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExpandRules([]rbacv1.PolicyRule{tc.rule}); err == nil {
				t.Fatal("wildcard accepted, want an error")
			}
		})
	}
}

func TestCompare(t *testing.T) {
	required := []Permission{
		perm("", "pods", "", "get"),
		perm("", "pods", "", "delete"),
		perm("spawnery.cloud", "servers", "status", "update"),
	}

	t.Run("exact match yields nothing", func(t *testing.T) {
		d := Compare(required, required)
		if len(d.Missing) != 0 || len(d.Extra) != 0 {
			t.Errorf("diff = %+v, want empty", d)
		}
	})

	t.Run("a granted verb the table does not list is extra", func(t *testing.T) {
		granted := append(append([]Permission{}, required...), perm("", "pods", "", "update"))
		d := Compare(required, granted)
		if len(d.Extra) != 1 || d.Extra[0].Key() != "/pods:update" {
			t.Errorf("extra = %v, want exactly /pods:update", keys(d.Extra))
		}
		if len(d.Missing) != 0 {
			t.Errorf("missing = %v, want none", keys(d.Missing))
		}
	})

	t.Run("a required verb the role does not grant is missing", func(t *testing.T) {
		granted := required[:2]
		d := Compare(required, granted)
		if len(d.Missing) != 1 || d.Missing[0].Key() != "spawnery.cloud/servers/status:update" {
			t.Errorf("missing = %v, want exactly spawnery.cloud/servers/status:update", keys(d.Missing))
		}
		if len(d.Extra) != 0 {
			t.Errorf("extra = %v, want none", keys(d.Extra))
		}
	})

	t.Run("both directions at once", func(t *testing.T) {
		granted := []Permission{
			perm("", "pods", "", "get"),
			perm("", "secrets", "", "list"),
		}
		d := Compare(required, granted)
		if len(d.Missing) != 2 {
			t.Errorf("missing = %v, want two entries", keys(d.Missing))
		}
		if len(d.Extra) != 1 || d.Extra[0].Key() != "/secrets:list" {
			t.Errorf("extra = %v, want exactly /secrets:list", keys(d.Extra))
		}
	})

	t.Run("a duplicate in the table is not reported as extra", func(t *testing.T) {
		granted := []Permission{perm("", "pods", "", "get"), perm("", "pods", "", "get")}
		d := Compare([]Permission{perm("", "pods", "", "get")}, granted)
		if len(d.Extra) != 0 {
			t.Errorf("extra = %v, want none", keys(d.Extra))
		}
	})
}
```

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/rbacaudit/... -run 'TestKey|TestExpand|TestCompare' -v`
Expected: FAIL — `undefined: Permission`, `undefined: ExpandRules`, `undefined: Compare`.

- [ ] **Step 3: Implementieren**

`internal/rbacaudit/permissions.go`:

```go
// Package rbacaudit states, independently of the generated manifests, which
// Kubernetes permissions the operator actually needs — and checks the
// generated ClusterRole against that statement in both directions.
//
// The table is maintained by hand on purpose. Deriving it from the kubebuilder
// markers would only prove that the role grants what the role grants.
package rbacaudit

import (
	"fmt"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
)

// Permission is one thing the operator may do: a verb on a resource, possibly
// on one of its subresources.
type Permission struct {
	// Group is the API group. The core group is the empty string.
	Group string
	// Resource is the plural resource name, without any subresource.
	Resource string
	// Subresource is empty for the resource itself.
	Subresource string
	// Verb is the RBAC verb.
	Verb string
	// Why names the call site that needs this. It is documentation, not
	// identity — Key ignores it — and it is what makes an obsolete entry
	// noticeable when its call site disappears.
	Why string
}

// Key is the identity of a permission, ignoring Why.
func (p Permission) Key() string {
	resource := p.Resource
	if p.Subresource != "" {
		resource += "/" + p.Subresource
	}
	return fmt.Sprintf("%s/%s:%s", p.Group, resource, p.Verb)
}

// String renders a permission for a failure message, including its reason.
func (p Permission) String() string {
	if p.Why == "" {
		return p.Key()
	}
	return p.Key() + " (" + p.Why + ")"
}

// ExpandRules flattens PolicyRules into individual permissions.
//
// A wildcard in any position is an error rather than an expansion: it grants
// everything in that position, so it can never be reconciled against a finite
// table, and an operator that needs a wildcard has outgrown this audit.
func ExpandRules(rules []rbacv1.PolicyRule) ([]Permission, error) {
	var out []Permission
	for i, rule := range rules {
		for _, group := range rule.APIGroups {
			if group == rbacv1.APIGroupAll {
				return nil, fmt.Errorf("rule %d grants every API group", i)
			}
			for _, resource := range rule.Resources {
				if resource == rbacv1.ResourceAll {
					return nil, fmt.Errorf("rule %d grants every resource in group %q", i, group)
				}
				name, sub, _ := strings.Cut(resource, "/")
				for _, verb := range rule.Verbs {
					if verb == rbacv1.VerbAll {
						return nil, fmt.Errorf("rule %d grants every verb on %q", i, resource)
					}
					out = append(out, Permission{
						Group:       group,
						Resource:    name,
						Subresource: sub,
						Verb:        verb,
					})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// Diff is the two-way comparison between what the code needs and what the
// role grants.
type Diff struct {
	// Missing is required but not granted: the operator will hit Forbidden.
	Missing []Permission
	// Extra is granted but not required: the role is wider than it needs to be.
	Extra []Permission
}

// Compare reports both directions. Duplicates on either side are collapsed.
func Compare(required, granted []Permission) Diff {
	requiredByKey := make(map[string]Permission, len(required))
	for _, p := range required {
		requiredByKey[p.Key()] = p
	}
	grantedByKey := make(map[string]Permission, len(granted))
	for _, p := range granted {
		grantedByKey[p.Key()] = p
	}

	var d Diff
	for key, p := range requiredByKey {
		if _, ok := grantedByKey[key]; !ok {
			d.Missing = append(d.Missing, p)
		}
	}
	for key, p := range grantedByKey {
		if _, ok := requiredByKey[key]; !ok {
			d.Extra = append(d.Extra, p)
		}
	}
	sort.Slice(d.Missing, func(i, j int) bool { return d.Missing[i].Key() < d.Missing[j].Key() })
	sort.Slice(d.Extra, func(i, j int) bool { return d.Extra[i].Key() < d.Extra[j].Key() })
	return d
}
```

- [ ] **Step 4: Test laufen lassen, Erfolg prüfen**

Run: `nix develop -c go test ./internal/rbacaudit/... -run 'TestKey|TestExpand|TestCompare' -v`
Expected: PASS für alle Unterfälle.

- [ ] **Step 5: Commit**

```bash
git add internal/rbacaudit/permissions.go internal/rbacaudit/permissions_test.go
git commit -m "Reine Logik für den Rechteabgleich"
```

---

### Task 3: Rechtetabelle und Audit gegen den echten Authorizer

**Files:**
- Create: `internal/rbacaudit/required.go`
- Test: `internal/rbacaudit/audit_envtest_test.go`
- Modify: `docs/bekannte-punkte.md`

**Interfaces:**
- Consumes: `Permission`, `ExpandRules`, `Compare` aus Task 2; `testenv.Client`, `testenv.RepoPath` aus Task 1.
- Produces: `rbacaudit.Required []Permission` — die handgepflegte Aussage, was der Code braucht.

**Der entscheidende Punkt dieses Tasks:** `SubjectAccessReview` fragt nach den Rechten eines *fremden* Subjekts. Der Test läuft mit dem Admin-Client von envtest und fragt für `system:serviceaccount:spawnery-system:spawnery-operator`. `SelfSubjectAccessReview` wäre falsch — das prüft die Rechte des Aufrufers, also des Admins.

Der API-Server von envtest startet mit `--authorization-mode=RBAC`; das wurde empirisch bestätigt (ohne Rolle verweigert, nach Bindung erlaubt, nicht gewährtes Verb verweigert). Der Authorizer wertet also echte Rollen aus.

- [ ] **Step 1: Den fehlschlagenden Test schreiben**

`internal/rbacaudit/audit_envtest_test.go`:

```go
package rbacaudit_test

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"github.com/spawnery/spawnery/internal/rbacaudit"
	"github.com/spawnery/spawnery/internal/testenv"
)

// targetNamespace is where namespaced permissions are checked. The binding is
// cluster-wide, so any namespace answers the same; using the one from the
// sample manifest keeps the failure messages recognisable.
const targetNamespace = "minecraft"

// applyDeployment applies the deployment manifests and returns the subject the
// operator actually runs as, derived from the manifests rather than restated.
// That way a binding naming the wrong role, or a deployment naming the wrong
// ServiceAccount, shows up as denied permissions instead of passing silently.
func applyDeploymentAndDeriveSubject(t *testing.T) string {
	t.Helper()

	var ns corev1.Namespace
	var sa corev1.ServiceAccount
	var role rbacv1.ClusterRole
	var binding rbacv1.ClusterRoleBinding
	var deploy appsv1.Deployment

	readManifest(t, "config/deploy/namespace.yaml", &ns)
	readManifest(t, "config/deploy/serviceaccount.yaml", &sa)
	readManifest(t, "config/rbac/role.yaml", &role)
	readManifest(t, "config/deploy/clusterrolebinding.yaml", &binding)
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

	apply(t, &ns, &sa, &role, &binding, &deploy)

	return fmt.Sprintf("system:serviceaccount:%s:%s",
		deploy.Namespace, deploy.Spec.Template.Spec.ServiceAccountName)
}

// TestEveryRequiredPermissionIsGranted asks the real RBAC authorizer, one
// permission at a time, whether the operator's ServiceAccount may do it.
// A denial here means the operator would hit Forbidden in a real cluster.
func TestEveryRequiredPermissionIsGranted(t *testing.T) {
	c, ctx := testenv.Client(t)
	subject := applyDeploymentAndDeriveSubject(t)

	for _, p := range rbacaudit.Required {
		p := p
		t.Run(p.Key(), func(t *testing.T) {
			sar := &authzv1.SubjectAccessReview{
				Spec: authzv1.SubjectAccessReviewSpec{
					User: subject,
					ResourceAttributes: &authzv1.ResourceAttributes{
						Namespace:   targetNamespace,
						Group:       p.Group,
						Resource:    p.Resource,
						Subresource: p.Subresource,
						Verb:        p.Verb,
					},
				},
			}
			if err := c.Create(ctx, sar); err != nil {
				t.Fatalf("SubjectAccessReview: %v", err)
			}
			if !sar.Status.Allowed {
				t.Errorf("%s is denied for %s — reason: %q",
					p, subject, sar.Status.Reason)
			}
		})
	}
}

// TestClusterRoleGrantsNothingExtra is the other direction: every verb the
// role grants must appear in the table. An operator that can create pods is
// worth keeping narrow.
func TestClusterRoleGrantsNothingExtra(t *testing.T) {
	var role rbacv1.ClusterRole
	readManifest(t, "config/rbac/role.yaml", &role)

	granted, err := rbacaudit.ExpandRules(role.Rules)
	if err != nil {
		t.Fatalf("expand rules: %v", err)
	}

	diff := rbacaudit.Compare(rbacaudit.Required, granted)
	for _, p := range diff.Extra {
		t.Errorf("clusterrole grants %s, which no entry in rbacaudit.Required claims", p.Key())
	}
	for _, p := range diff.Missing {
		t.Errorf("rbacaudit.Required lists %s, which the clusterrole never mentions", p)
	}
}
```

Die Helfer `readManifest`, `apply` und `TestMain` stehen bereits in `deploy_envtest_test.go` im selben Testpaket — nicht doppelt anlegen. `applyDeploymentAndDeriveSubject` braucht deshalb selbst keinen Client mehr; die Variable `c` und `ctx` entfallen dort.

- [ ] **Step 2: Test laufen lassen, Fehlschlag prüfen**

Run: `nix develop -c go test ./internal/rbacaudit/... -run 'TestEvery|TestClusterRole' -v`
Expected: FAIL — `undefined: rbacaudit.Required`.

- [ ] **Step 3: Die Rechtetabelle schreiben**

`internal/rbacaudit/required.go`:

```go
package rbacaudit

// Required is the hand-maintained statement of what the operator's code
// actually does against the Kubernetes API. It is deliberately not derived
// from the kubebuilder markers: a derived table would only prove that the
// generated role grants what the generated role grants.
//
// Adding a marker without adding an entry here turns the audit red. That is
// the point — it forces a moment of thought about whether the new permission
// is really needed.
//
// Note the limit of this table: it catches drift between role and table, not
// a permission missing from both. Only the operator actually running under
// this ServiceAccount can prove completeness, which is what the cluster-level
// end-to-end test is for.
var Required = []Permission{
	// Events — the recorder writes them for every phase change and every
	// warning, and patches them when it aggregates repeats.
	{Group: "", Resource: "events", Verb: "create", Why: "Recorder.Eventf in allen Controllern"},
	{Group: "", Resource: "events", Verb: "patch", Why: "Event-Aggregation des Recorders"},

	// Pods — the Server controller owns their whole life cycle.
	{Group: "", Resource: "pods", Verb: "get", Why: "ServerReconciler.fetchPod"},
	{Group: "", Resource: "pods", Verb: "list", Why: "OrphanReconciler.Sweep"},
	{Group: "", Resource: "pods", Verb: "watch", Why: "ServerReconciler Owns(&corev1.Pod{})"},
	{Group: "", Resource: "pods", Verb: "create", Why: "ServerReconciler erzeugt den Pod aus podspec"},
	{Group: "", Resource: "pods", Verb: "delete", Why: "Terminating-Entscheidung und Verwaisten-Abgleich"},
	{Group: "", Resource: "pods", Verb: "patch", Why: "syncOccupiedLabel patcht das Occupied-Label"},

	// PodDisruptionBudgets — one per group, kept in step with the occupied count.
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "get", Why: "CreateOrUpdate in reconcilePDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "list", Why: "ServerGroupReconciler Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "watch", Why: "ServerGroupReconciler Owns(&policyv1.PodDisruptionBudget{})"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "create", Why: "CreateOrUpdate in reconcilePDB"},
	{Group: "policy", Resource: "poddisruptionbudgets", Verb: "update", Why: "CreateOrUpdate in reconcilePDB"},

	// Leader election locks on a Lease in the operator's own namespace.
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "create", Why: "Leader-Election beim Start"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "get", Why: "Leader-Election erneuert die Sperre"},
	{Group: "coordination.k8s.io", Resource: "leases", Verb: "update", Why: "Leader-Election erneuert die Sperre"},

	// The operator's own resources.
	{Group: "spawnery.cloud", Resource: "networks", Verb: "get", Why: "Auflösen von networkRef"},
	{Group: "spawnery.cloud", Resource: "networks", Verb: "list", Why: "NetworkReconciler.namespaceOwner"},
	{Group: "spawnery.cloud", Resource: "networks", Verb: "watch", Why: "NetworkReconciler For(&Network{})"},
	{Group: "spawnery.cloud", Resource: "networks", Subresource: "status", Verb: "get", Why: "Status().Update liest vorher"},
	{Group: "spawnery.cloud", Resource: "networks", Subresource: "status", Verb: "update", Why: "NetworkReconciler schreibt Conditions und Zähler"},

	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "get", Why: "Auflösen von groupRef"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "list", Why: "NetworkReconciler zählt Gruppen"},
	{Group: "spawnery.cloud", Resource: "servergroups", Verb: "watch", Why: "ServerGroupReconciler For(&ServerGroup{})"},
	{Group: "spawnery.cloud", Resource: "servergroups", Subresource: "status", Verb: "get", Why: "Status().Update liest vorher"},
	{Group: "spawnery.cloud", Resource: "servergroups", Subresource: "status", Verb: "update", Why: "ServerGroupReconciler schreibt Aggregation und Conditions"},

	{Group: "spawnery.cloud", Resource: "servers", Verb: "get", Why: "ServerReconciler.Reconcile"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "list", Why: "ServerGroupReconciler.collectViews und Verwaisten-Abgleich"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "watch", Why: "ServerReconciler For(&Server{})"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "create", Why: "ServerGroupReconciler erzeugt Server bis zur Untergrenze"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "delete", Why: "Verkleinern, Kappen aufbewahrter Fehlschläge, Verwaisten-Abgleich"},
	{Group: "spawnery.cloud", Resource: "servers", Verb: "update", Why: "Finalizer setzen und entfernen"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "status", Verb: "get", Why: "Status().Update liest vorher"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "status", Verb: "update", Why: "ServerReconciler schreibt Phase, Zeitstempel und Conditions"},
	{Group: "spawnery.cloud", Resource: "servers", Subresource: "finalizers", Verb: "update", Why: "blockOwnerDeletion auf den OwnerReferences der Pods"},

	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "get", Why: "NetworkReconciler zählt Proxy-Gruppen"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "list", Why: "NetworkReconciler zählt Proxy-Gruppen"},
	{Group: "spawnery.cloud", Resource: "proxygroups", Verb: "watch", Why: "Cache des Managers"},
}
```

- [ ] **Step 4: Tests laufen lassen und die Übergewährungen abtragen**

```bash
nix develop -c go test ./internal/rbacaudit/... -run 'TestEvery|TestClusterRole' -v
```

`TestClusterRoleGrantsNothingExtra` wird jetzt fehlschlagen und die Verben nennen, die die Schlussdurchsicht von Meilenstein 1 bereits als ungenutzt gefunden hatte. Erwartet werden mindestens:

`/pods:update`, `spawnery.cloud/networks:patch`, `spawnery.cloud/networks:update`, `spawnery.cloud/servergroups:patch`, `spawnery.cloud/servergroups:update`, `spawnery.cloud/servers:patch`, `policy/poddisruptionbudgets:delete`, `policy/poddisruptionbudgets:patch`, `spawnery.cloud/networks/status:patch`, `spawnery.cloud/servergroups/status:patch`, `spawnery.cloud/servers/status:patch`.

Für jedes gemeldete Verb den zugehörigen kubebuilder-Marker in `internal/controller/*.go` entsprechend kürzen — **nicht** die Tabelle erweitern. Danach `nix develop -c make manifests` und den Test erneut laufen lassen, bis beide Richtungen leer sind.

Meldet der Test ein Verb, das der Code doch braucht, ist die Tabelle unvollständig: dann die Tabelle ergänzen und im Bericht begründen, welche Aufrufstelle es benutzt.

- [ ] **Step 5: Volle Suite laufen lassen**

```bash
nix develop -c make all
```

Expected: alles grün. Die gekürzten Marker dürfen keinen bestehenden Test beeinflussen — envtest arbeitet mit Adminrechten, die Controller-Tests sind von der Rolle unberührt.

- [ ] **Step 6: Mutationsnachweis**

Drei Mutationen, jeweils zurücknehmen:

1. Ein Verb aus einem Marker entfernen, `make manifests`, Test laufen lassen → `TestEveryRequiredPermissionIsGranted` muss genau bei diesem Tripel fehlschlagen, und `TestClusterRoleGrantsNothingExtra` muss es als `Missing` melden.
2. Einen Marker um ein Verb erweitern, `make manifests` → `TestClusterRoleGrantsNothingExtra` muss es als `Extra` melden.
3. In `config/deploy/clusterrolebinding.yaml` den Subject-Namespace auf `default` ändern → `TestEveryRequiredPermissionIsGranted` muss für **jedes** Tripel fehlschlagen, weil das abgeleitete Subjekt dann nichts mehr darf.

Im Bericht festhalten, welche Mutation welchen Test umgeworfen hat.

- [ ] **Step 7: Bekannte Punkte aktualisieren**

In `docs/bekannte-punkte.md` den Abschnitt zu Meilenstein 6 anpassen: Der Eintrag „Die generierte ClusterRole ist zu weit" ist erledigt und wird ersetzt durch:

```markdown
**Leases gehören in eine namespaced Role.** Das Recht auf
`coordination.k8s.io/leases` wird derzeit clusterweit gewährt, obwohl
Leader-Election nur im eigenen Namespace des Operators sperrt. Die Helm-Chart
sollte es in eine `Role` in `spawnery-system` verschieben und aus der
ClusterRole entfernen; die Rechtetabelle in `internal/rbacaudit` ist dann
entsprechend aufzuteilen.

**Vollständigkeit der Rechtetabelle.** Der Audit in `internal/rbacaudit` fängt
Abweichungen zwischen Tabelle und Rolle. Fehlt ein Recht in beiden, bleibt er
grün — das beweist erst der Operator, der unter seinem ServiceAccount in einem
echten Cluster läuft (Ebene B des E2E-Entwurfs).
```

- [ ] **Step 8: Commit**

```bash
git add internal/rbacaudit config/rbac internal/controller docs/bekannte-punkte.md
git commit -m "Rechtetabelle und Audit gegen den echten Authorizer"
```

---

## Abnahme

- [ ] `nix develop -c make all` ist grün.
- [ ] `internal/rbacaudit` meldet weder fehlende noch überflüssige Rechte.
- [ ] Die ClusterRole gewährt `coordination.k8s.io/leases` mit `create`, `get`, `update`.
- [ ] Die elf von der Schlussdurchsicht gefundenen Übergewährungen sind aus den Markern entfernt.
- [ ] `config/deploy/` enthält Namespace, ServiceAccount, ClusterRoleBinding und Deployment, und der Test prüft, dass sie aufeinander zeigen.
- [ ] Alle drei Mutationen aus Task 3, Step 6 werfen den erwarteten Test um.

## Was dieser Plan bewusst offenlässt

Alles aus Ebene B des Entwurfs: das Operator-Image, der NixOS-VM-Test mit RKE2,
die getriebenen Szenarien im Cluster und die Prüfung auf `Forbidden` im
Operator-Log. Dafür entsteht ein eigener Plan, sobald dieser abgeschlossen ist.
