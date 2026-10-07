/*
Copyright paul_wtf.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rbacaudit_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/rbacaudit"
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

// renderNamespace is neither the chart default spawnery-system, where a
// hard-coded namespace renders byte-identically, nor hack/e2e.sh's
// platform-system.
const renderNamespace = "audit-system"

// Everything below reads renderChart's objects, so this is what proves the
// chart honours the release namespace.
func TestTheChartRendersIntoTheNamespaceItIsGiven(t *testing.T) {
	rendered := renderChart(t)

	if renderNamespace != "spawnery-system" {
		for key, doc := range rendered {
			if strings.Contains(string(doc), "spawnery-system") {
				t.Errorf("%s carries the literal spawnery-system when rendered into %s:\n%s",
					key, renderNamespace, doc)
			}
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

// A Deployment's selector is immutable, and a drifted one presents as a
// network fault.
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

// renderedObjectKeys is the closed set of objects the chart renders by
// default. The rest of the package audits objects by name, so a new template
// (say, a second ClusterRole) would ship unaudited; adding its key here is
// the moment to decide what audits it.
var renderedObjectKeys = []string{
	"ClusterRole/spawnery-operator",
	"ClusterRoleBinding/spawnery-operator",
	"CustomResourceDefinition/networks.spawnery.cloud",
	"CustomResourceDefinition/proxygroups.spawnery.cloud",
	// Audited like the other CRDs: its grants are rows in required.go.
	"CustomResourceDefinition/scaleboosts.spawnery.cloud",
	"CustomResourceDefinition/servergroups.spawnery.cloud",
	"CustomResourceDefinition/servers.spawnery.cloud",
	"Deployment/spawnery-operator",
	"NetworkPolicy/spawnery-operator-agent",
	"Role/spawnery-operator",
	"RoleBinding/spawnery-operator",
	"Service/spawnery-operator",
	"ServiceAccount/spawnery-operator",
	// Audited by TestTheOperatorMayDeleteOnlyAnOnDemandWorld.
	"ValidatingAdmissionPolicy/spawnery-world-deletion",
	"ValidatingAdmissionPolicyBinding/spawnery-world-deletion",
}

// Default values, as renderChart uses; only the NetworkPolicy is conditional
// (networkPolicy.enabled, default true).
func TestTheChartRendersExactlyTheseObjects(t *testing.T) {
	rendered := renderChart(t)

	got := slices.Sorted(maps.Keys(rendered))
	want := slices.Clone(renderedObjectKeys)
	slices.Sort(want)

	for _, key := range got {
		if !slices.Contains(want, key) {
			t.Errorf("the chart renders %s, which this package audits nowhere. Every "+
				"other test here looks up objects by name, so a new template is invisible "+
				"to all of them -- a ClusterRoleBinding widening the operator's grants "+
				"would ship with every test green. Add the key to renderedObjectKeys, and "+
				"in the same edit decide what audits the object", key)
		}
	}
	for _, key := range want {
		if !slices.Contains(got, key) {
			t.Errorf("renderedObjectKeys lists %s, which the chart does not render. Either "+
				"a template was deleted or its condition is now false by default; whichever "+
				"it is, some test below is reading an object the cluster never sees", key)
		}
	}
}

// renderChart runs helm once per package run (sync.Once, so a failure lands on
// a real *testing.T). helm comes from the flake, so its absence means the
// wrong shell.
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

// splitRendered refuses duplicate keys: keeping one would audit an object the
// cluster never sees.
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

// renderedManifest decodes strictly: sigs.k8s.io/yaml's plain Unmarshal drops
// unknown keys, so a typo would decode to a zero value.
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

// The rendered chart is audited rather than config/rbac/role.yaml, so a broken
// transform in hack/chart-templates.sh is seen too.
const (
	generatedClusterRoleKey = "ClusterRole/spawnery-operator"
	generatedRoleKey        = "Role/spawnery-operator"
)

// readMultiDocManifest leaves everything kind-specific, including refusing a
// second object of a kind, to decode.
func readMultiDocManifest(t *testing.T, rel string, decode func(kind string, doc []byte)) {
	t.Helper()

	f, err := os.Open(testenv.RepoPath(t, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	defer func() { _ = f.Close() }()

	docs := utilyaml.NewYAMLReader(bufio.NewReader(f))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.TrimSpace(string(doc)) == "" {
			continue
		}
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatalf("decode %s: %v", rel, err)
		}
		decode(meta.Kind, doc)
	}
}

// readGeneratedRoles insists on exactly one of each. A missing Role means a
// namespace= qualifier fell off a marker.
func readGeneratedRoles(t *testing.T) (*rbacv1.ClusterRole, *rbacv1.Role) {
	t.Helper()

	rendered := renderChart(t)
	keys := make([]string, 0, len(rendered))
	for k := range rendered {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	renders := strings.Join(keys, ", ")

	if _, ok := rendered[generatedClusterRoleKey]; !ok {
		t.Fatalf("the chart renders no %s; it renders: %s", generatedClusterRoleKey, renders)
	}
	var cluster rbacv1.ClusterRole
	renderedManifest(t, generatedClusterRoleKey, &cluster)

	if _, ok := rendered[generatedRoleKey]; !ok {
		t.Fatalf("the chart renders no %s; it renders: %s — a namespace= "+
			"qualifier fell off a marker, and the operator would hold its Secret "+
			"and Lease rights in every namespace", generatedRoleKey, renders)
	}
	var namespaced rbacv1.Role
	renderedManifest(t, generatedRoleKey, &namespaced)

	return &cluster, &namespaced
}

// apply tolerates AlreadyExists, since cluster-scoped objects outlive a test
// in the shared control plane, but checks that an existing ClusterRole or Role
// carries the rules asked for, so a stale one is never audited. Not
// create-or-update: some objects carry immutable server-assigned fields.
func apply(t *testing.T, objs ...client.Object) {
	t.Helper()
	c, ctx := testenv.Client(t)
	for _, obj := range objs {
		err := c.Create(ctx, obj)
		if err == nil {
			continue
		}
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
		assertSameRules(t, c, ctx, obj)
	}
}

func assertSameRules(t *testing.T, c client.Client, ctx context.Context, want client.Object) {
	t.Helper()
	key := client.ObjectKeyFromObject(want)
	switch desired := want.(type) {
	case *rbacv1.ClusterRole:
		var existing rbacv1.ClusterRole
		if err := c.Get(ctx, key, &existing); err != nil {
			t.Fatalf("get the ClusterRole %s that already exists: %v", key.Name, err)
		}
		diffRules(t, "ClusterRole", key.Name, desired.Rules, existing.Rules)
	case *rbacv1.Role:
		var existing rbacv1.Role
		if err := c.Get(ctx, key, &existing); err != nil {
			t.Fatalf("get the Role %s that already exists: %v", key.Name, err)
		}
		diffRules(t, "Role", key.Name, desired.Rules, existing.Rules)
	}
}

func diffRules(t *testing.T, kind, name string, want, got []rbacv1.PolicyRule) {
	t.Helper()
	if reflect.DeepEqual(want, got) {
		return
	}
	t.Fatalf("%s %s already exists in the shared control plane with different rules than "+
		"this test asked for, so everything below would audit an object nobody wrote.\n"+
		"  in the cluster: %v\n  asked for:      %v\n"+
		"Two tests in this package are rendering different roles under one name; the "+
		"control plane is shared and nothing cleans it between tests, so whichever ran "+
		"first wins and the other silently audits it.", kind, name, got, want)
}

// A binding naming the wrong role, or a Deployment the wrong ServiceAccount,
// looks fine file by file.
func TestDeployManifestsAreAcceptedAndConsistent(t *testing.T) {
	c, ctx := testenv.Client(t)

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: renderNamespace}}
	var sa corev1.ServiceAccount
	var binding rbacv1.ClusterRoleBinding
	var deploy appsv1.Deployment

	renderedManifest(t, "ServiceAccount/spawnery-operator", &sa)
	renderedManifest(t, "ClusterRoleBinding/spawnery-operator", &binding)
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)
	role, _ := readGeneratedRoles(t)

	apply(t, &ns, &sa, role, &binding, &deploy)

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

// Nothing else reads rolebinding.yaml; a mistake there leaves certs.Store
// Forbidden on the operator's own Secret.
func TestTheRoleBindingBindsTheOperatorInItsOwnNamespace(t *testing.T) {
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: renderNamespace}}
	var sa corev1.ServiceAccount
	var binding rbacv1.RoleBinding

	renderedManifest(t, "ServiceAccount/spawnery-operator", &sa)
	renderedManifest(t, "RoleBinding/spawnery-operator", &binding)
	_, role := readGeneratedRoles(t)

	apply(t, &ns, role, &binding)

	// The Role's namespace comes from the markers, the binding's from the
	// manifest.
	if role.Namespace != ns.Name {
		t.Errorf("the generated Role is in %q, but the operator runs in %q — the "+
			"namespace= qualifier on the markers names the wrong namespace",
			role.Namespace, ns.Name)
	}
	if binding.Namespace != role.Namespace {
		t.Errorf("rolebinding namespace = %q, role namespace = %q — a RoleBinding only "+
			"grants in its own namespace", binding.Namespace, role.Namespace)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name {
		t.Errorf("roleRef = %s/%s, want Role/%s",
			binding.RoleRef.Kind, binding.RoleRef.Name, role.Name)
	}
	if len(binding.Subjects) != 1 {
		t.Fatalf("binding has %d subjects, want exactly one — a second subject would "+
			"widen the grant and quietly make TestTheAuthorizerActuallyDenies the only "+
			"test that could still notice", len(binding.Subjects))
	}
	subj := binding.Subjects[0]
	if subj.Kind != "ServiceAccount" || subj.Name != sa.Name || subj.Namespace != sa.Namespace {
		t.Errorf("subject = %s %s/%s, want ServiceAccount %s/%s",
			subj.Kind, subj.Namespace, subj.Name, sa.Namespace, sa.Name)
	}
}

// Every hop of the agents' dial address lives in a different file.
func TestAgentServiceReachesTheOperatorPods(t *testing.T) {
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: renderNamespace}}
	var svc corev1.Service
	var deploy appsv1.Deployment
	renderedManifest(t, "Service/spawnery-operator", &svc)
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)

	apply(t, &ns, &svc)

	// The operator derives the dial address and its certificate SANs from
	// podspec.AgentServiceName and never compares them with the manifest.
	if svc.Name != podspec.AgentServiceName {
		t.Errorf("the Service is named %q but the operator dials and certifies %q — "+
			"every agent would fail its TLS handshake against a name that does not resolve",
			svc.Name, podspec.AgentServiceName)
	}

	if svc.Namespace != deploy.Namespace {
		t.Errorf("service namespace = %q, deployment namespace = %q — a Service only "+
			"selects pods in its own namespace", svc.Namespace, deploy.Namespace)
	}

	podLabels := deploy.Spec.Template.Labels
	for k, v := range svc.Spec.Selector {
		if podLabels[k] != v {
			t.Errorf("service selects %s=%q but the operator pods carry %s=%q — "+
				"the Service would have no endpoints at all", k, v, k, podLabels[k])
		}
	}

	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want exactly one", len(deploy.Spec.Template.Spec.Containers))
	}
	container := deploy.Spec.Template.Spec.Containers[0]
	named := map[string]int32{}
	for _, p := range container.Ports {
		named[p.Name] = p.ContainerPort
	}
	for _, p := range svc.Spec.Ports {
		target := p.TargetPort.StrVal
		if target == "" {
			t.Errorf("service port %q targets a number, not a named port", p.Name)
			continue
		}
		if _, ok := named[target]; !ok {
			t.Errorf("service port %q targets the container port %q, which the operator "+
				"container does not declare", p.Name, target)
		}
	}
	if got := named["agent"]; got != 9443 {
		t.Errorf("the agent container port = %d, want 9443", got)
	}

	// readyz turns green only on the leader, which keeps a standby out of the
	// Service.
	probe := container.ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		t.Fatal("the operator has no HTTP readiness probe; a standby would serve as an endpoint")
	}
	if probe.HTTPGet.Path != "/readyz" {
		t.Errorf("readiness probe path = %q, want /readyz", probe.HTTPGet.Path)
	}
}

// With readiness tied to the leader lock, RollingUpdate (maxSurge 1,
// maxUnavailable 0) deadlocks: the new pod waits for a lease the old pod
// holds. Recreate is the shape for a single-replica leader-elected operator.
func TestTheOperatorIsReplacedRatherThanRolled(t *testing.T) {
	var deploy appsv1.Deployment
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)

	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1 — this test reasons about the single-replica case",
			deploy.Spec.Replicas)
	}
	if got := deploy.Spec.Strategy.Type; got != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("deployment strategy = %q, want %q — a rolling update would wait for a "+
			"readiness the new pod cannot reach until the old one releases the leader lease",
			got, appsv1.RecreateDeploymentStrategyType)
	}
}

func TestOperatorPodIsRestrictedCompliant(t *testing.T) {
	var deploy appsv1.Deployment
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)

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

// sigs.k8s.io/yaml is not strict, so a mistyped flag key disappears silently.
// The --startup-deadline floor: a server needs 24 s to become ready in the
// best case. hack/e2e.sh overrides it by repeating the flag; the last wins.
func TestTheOperatorDeploymentCarriesProductionFlags(t *testing.T) {
	var deploy appsv1.Deployment
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)

	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want exactly one", len(deploy.Spec.Template.Spec.Containers))
	}

	args := map[string]string{}
	for _, a := range deploy.Spec.Template.Spec.Containers[0].Args {
		name, value, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !ok {
			t.Errorf("argument %q is not --flag=value, so this test cannot judge it", a)
			continue
		}
		args[name] = value
	}

	want := []string{
		"leader-elect",
		"startup-deadline",
		"metrics-bind-address",
		"health-probe-bind-address",
		// Off by default, but rendered either way so the value is visible.
		"allow-plugin-volumes",
		"allow-file-volumes",
		"allow-mount-volumes",
		"aot-cache",
		"world-sync",
	}
	for _, name := range want {
		if _, ok := args[name]; !ok {
			t.Errorf("the operator container does not pass --%s", name)
		}
	}
	for name := range args {
		if !slices.Contains(want, name) {
			t.Errorf("the operator container passes --%s, which this test does not know "+
				"about. The flag package rejects nothing it is not given and the YAML "+
				"decoder accepts any string, so a mistyped flag reaches a real cluster "+
				"silently. Add it here deliberately, or fix the typo", name)
		}
	}

	deadline, err := time.ParseDuration(args["startup-deadline"])
	if err != nil {
		t.Fatalf("--startup-deadline=%q does not parse: %v", args["startup-deadline"], err)
	}
	if deadline < 5*time.Minute {
		t.Errorf("--startup-deadline=%s, want at least 5m. Milestone 5a's evidence run "+
			"measured 24 seconds from apply to ReadyGatePassed on an idle single-node "+
			"kind cluster with the image already present; a shorter deadline in the "+
			"manifest a person installs fails healthy servers. The E2E run patches its "+
			"own copy down instead", deadline)
	}
}

// Tags are mutable (design §8); hack/publish.sh writes a digest after a push.
func TestTheOperatorImageIsNotAMutableTag(t *testing.T) {
	var deploy appsv1.Deployment
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)

	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want exactly one", len(deploy.Spec.Template.Spec.Containers))
	}
	ref := deploy.Spec.Template.Spec.Containers[0].Image

	const repo = "ghcr.io/spawnery/spawnery-operator"
	if digest, ok := strings.CutPrefix(ref, repo+"@"); ok {
		if !strings.HasPrefix(digest, "sha256:") {
			t.Errorf("image = %q: the digest does not start with sha256:", ref)
		}
		return
	}
	tag, ok := strings.CutPrefix(ref, repo+":")
	if !ok {
		t.Fatalf("image = %q, want %s with either a tag or a digest", ref, repo)
	}
	switch tag {
	case "", "dev", "latest":
		t.Errorf("image = %q. %q is a tag nothing publishes or a tag that moves; the "+
			"operator would either fail to pull or silently change version between "+
			"restarts", ref, tag)
	}

	// nix/operator-image.nix tags the image with flake.nix's operatorVersion, and
	// `make e2e` patches the image away, so nothing else notices a stale tag.
	if want := operatorVersionFromFlake(t); tag != want {
		t.Errorf("image = %q, but flake.nix's operatorVersion is %q. "+
			"nix/operator-image.nix tags the image with operatorVersion, so this "+
			"manifest names a tag nothing builds or publishes", ref, want)
	}
}

// operatorVersionFromFlake reads flake.nix as text rather than via `nix eval`,
// which would put an evaluation into `make test`; it fails loudly when the
// line's shape changes.
func operatorVersionFromFlake(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(testenv.RepoPath(t, "flake.nix"))
	if err != nil {
		t.Fatalf("read flake.nix: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*operatorVersion\s*=\s*"([^"]+)"\s*;`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no `operatorVersion = \"...\";` line in flake.nix. Either it was " +
			"renamed or its shape moved; this test reads it as text (see the comment " +
			"above) and cannot check the manifest's tag against something it cannot find")
	}
	return string(m[1])
}

// appVersion is the operator release the chart installs by default. Whether
// Chart.yaml's version moved since the last release is a question about two
// commits, which .github/workflows/release.yml asks.
func TestTheChartAgreesWithTheFlakeAboutTheOperatorRelease(t *testing.T) {
	flakeVersion := operatorVersionFromFlake(t)

	chart, err := os.ReadFile(testenv.RepoPath(t, "charts/spawnery/Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	appRe := regexp.MustCompile(`(?m)^appVersion:\s*"?([^"\s]+)"?\s*$`)
	m := appRe.FindSubmatch(chart)
	if m == nil {
		t.Fatal("no appVersion line in charts/spawnery/Chart.yaml; this test reads it " +
			"as text and cannot check what it cannot find")
	}
	if got := string(m[1]); got != flakeVersion {
		t.Errorf("Chart.yaml appVersion = %q, flake.nix operatorVersion = %q. appVersion "+
			"names the operator release this chart installs by default, so a chart that "+
			"disagrees with the flake ships a claim nobody built", got, flakeVersion)
	}

	values, err := os.ReadFile(testenv.RepoPath(t, "charts/spawnery/values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	tagRe := regexp.MustCompile(`(?m)^\s*tag:\s*"([^"]+)"\s*$`)
	m = tagRe.FindSubmatch(values)
	if m == nil {
		t.Fatal("no image tag line in charts/spawnery/values.yaml")
	}
	if got := string(m[1]); got != flakeVersion {
		t.Errorf("values.yaml image.tag = %q, flake.nix operatorVersion = %q. "+
			"TestTheOperatorImageIsNotAMutableTag checks this too, but only while "+
			"image.digest is empty -- it returns early otherwise, which is how the tag "+
			"went stale unnoticed once already", got, flakeVersion)
	}
}

// An OCI install needs --version, and a number in a README goes stale
// silently. Read as text: what is checked is what a person copies.
func TestTheInstallInstructionsNameTheChartVersion(t *testing.T) {
	chart, err := os.ReadFile(testenv.RepoPath(t, "charts/spawnery/Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	versionRe := regexp.MustCompile(`(?m)^version:\s*"?([^"\s]+)"?\s*$`)
	m := versionRe.FindSubmatch(chart)
	if m == nil {
		t.Fatal("no version line in charts/spawnery/Chart.yaml; this test reads it " +
			"as text and cannot check what it cannot find")
	}
	want := string(m[1])

	// Every --version in either README, not just the first.
	flagRe := regexp.MustCompile(`--version\s+(\S+)`)
	for _, doc := range []string{"README.md", "charts/spawnery/README.md"} {
		body, err := os.ReadFile(testenv.RepoPath(t, doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		found := flagRe.FindAllSubmatch(body, -1)
		if len(found) == 0 {
			t.Errorf("%s names no --version. The chart installs from an OCI reference, "+
				"which does not carry a version of its own, so an install line without "+
				"one resolves to whatever is newest rather than to this release", doc)
			continue
		}
		for _, f := range found {
			if got := string(f[1]); got != want {
				t.Errorf("%s says --version %s, Chart.yaml says version: %s. The install "+
					"line is what a reader copies, and the registry has no chart at %s",
					doc, got, want, got)
			}
		}
	}
}

// The Lease right belongs in the namespaced Role; cluster-wide it would let
// the operator lock anything anywhere.
func TestLeaderElectionPermissionIsGranted(t *testing.T) {
	cluster, role := readGeneratedRoles(t)

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
			t.Errorf("the Role in %s does not grant %q on coordination.k8s.io/leases — "+
				"leader election would fail with Forbidden on startup", role.Namespace, verb)
		}
	}

	for _, rule := range cluster.Rules {
		if contains(rule.APIGroups, "coordination.k8s.io") && contains(rule.Resources, "leases") {
			t.Errorf("the ClusterRole still grants %v on coordination.k8s.io/leases — the "+
				"operator could take a leader lock in any namespace", rule.Verbs)
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

// The operator pod deliberately lacks spawnery.cloud/managed-by, so a selector
// copied from a managed pod selects nothing and fails open. The peer needs an
// empty namespaceSelector, since agents dial in from every game namespace.
// Egress in policyTypes would default-deny the operator's own traffic.
func TestTheAgentPolicySelectsTheOperatorAndAdmitsManagedPods(t *testing.T) {
	var policy networkingv1.NetworkPolicy
	var deploy appsv1.Deployment
	renderedManifest(t, "NetworkPolicy/spawnery-operator-agent", &policy)
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)

	if policy.Namespace != deploy.Namespace {
		t.Errorf("policy namespace = %q, deployment namespace = %q — a "+
			"NetworkPolicy only governs pods in its own namespace",
			policy.Namespace, deploy.Namespace)
	}

	podLabels := deploy.Spec.Template.Labels
	for k, v := range policy.Spec.PodSelector.MatchLabels {
		if podLabels[k] != v {
			t.Errorf("the policy selects %s=%q but the operator pod carries "+
				"%s=%q — the policy would select nothing, and a policy that "+
				"selects nothing fails open", k, v, k, podLabels[k])
		}
	}
	if len(policy.Spec.PodSelector.MatchLabels) == 0 {
		t.Error("an empty podSelector selects every pod in the namespace")
	}

	// podspec.OperatorPodLabels() is the third copy of these labels and builds the
	// per-Network egress peer. Subset of the Deployment's labels, but equal to the
	// policy's selector, since a narrowed selector is a widened policy.
	wantOperator := podspec.OperatorPodLabels()
	for k, v := range wantOperator {
		if podLabels[k] != v {
			t.Errorf("podspec.OperatorPodLabels() has %s=%q but the Deployment's "+
				"pod carries %s=%q — the per-Network policy's egress peer is built "+
				"from the former and would select nothing", k, v, k, podLabels[k])
		}
	}
	if len(policy.Spec.PodSelector.MatchLabels) != len(wantOperator) {
		t.Errorf("the policy's podSelector = %v, want exactly %v — a selector "+
			"narrowed to a subset selects more pods, not fewer",
			policy.Spec.PodSelector.MatchLabels, wantOperator)
	}
	for k, v := range wantOperator {
		if policy.Spec.PodSelector.MatchLabels[k] != v {
			t.Errorf("the policy's podSelector[%q] = %q, want %q",
				k, policy.Spec.PodSelector.MatchLabels[k], v)
		}
	}

	// Egress with no egress rules would cut the operator off from the API server.
	if len(policy.Spec.PolicyTypes) != 1 ||
		policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes = %v, want [Ingress] alone — this policy has no "+
			"egress rules, so declaring Egress default-denies the operator's own "+
			"outbound traffic and, wherever a CNI enforces, it cannot reach the "+
			"API server", policy.Spec.PolicyTypes)
	}

	var agentRule, probeRule *networkingv1.NetworkPolicyIngressRule
	for i := range policy.Spec.Ingress {
		rule := &policy.Spec.Ingress[i]
		if len(rule.From) == 0 {
			probeRule = rule
			continue
		}
		agentRule = rule
	}

	if agentRule == nil {
		t.Fatal("no ingress rule with a peer: nothing admits the agents")
	}
	if len(agentRule.From) != 1 {
		t.Fatalf("the agent rule has %d peers, want exactly one", len(agentRule.From))
	}
	peer := agentRule.From[0]
	if peer.NamespaceSelector == nil || len(peer.NamespaceSelector.MatchLabels) != 0 {
		t.Errorf("the agent peer's namespaceSelector = %v, want an empty one — "+
			"every managed pod dials in from its own game namespace, and the "+
			"operator's chart cannot know those names", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil ||
		peer.PodSelector.MatchLabels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		t.Errorf("the agent peer must select %s=%s; got %v",
			podspec.LabelManagedBy, podspec.ManagedByValue, peer.PodSelector)
	}
	if len(agentRule.Ports) != 1 || agentRule.Ports[0].Port.IntValue() != int(podspec.AgentPort) {
		t.Errorf("the agent rule admits %v, want only %d", agentRule.Ports, podspec.AgentPort)
	}

	// Selecting the pod makes it default-deny for ingress, kubelet probes and
	// metrics scrapes included.
	if probeRule == nil {
		t.Fatal("no peerless ingress rule: the kubelet's probe to the health " +
			"port is denied, and the operator goes NotReady")
	}
	// Named and nil ports are refused rather than modelled: IntValue() returns 0
	// for a named port, and a nil port admits everything.
	admitted := map[int]bool{}
	for _, p := range probeRule.Ports {
		switch {
		case p.Port == nil:
			t.Fatalf("the peerless ingress rule has a port entry with no port, which admits "+
				"every port on the operator pod from anywhere; this check models numbered "+
				"ports only: %+v", p)
		case p.Port.Type == intstr.String:
			t.Fatalf("the peerless ingress rule admits the named port %q. That is legal, and "+
				"Kubernetes resolves it against the pod's own port names -- but this check "+
				"compares numbers and does not resolve names, so it would have reported it "+
				"as port 0 rather than measured it", p.Port.StrVal)
		}
		admitted[p.Port.IntValue()] = true
	}
	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("got %d containers, want exactly one", len(deploy.Spec.Template.Spec.Containers))
	}
	// "agent" is already admitted, from a peer, by the rule above.
	nonAgentDeclared := map[int]bool{}
	for _, p := range deploy.Spec.Template.Spec.Containers[0].Ports {
		if p.Name == "agent" {
			continue
		}
		nonAgentDeclared[int(p.ContainerPort)] = true
		if !admitted[int(p.ContainerPort)] {
			t.Errorf("the container declares port %q (%d) and the policy does "+
				"not admit it", p.Name, p.ContainerPort)
		}
	}
	// This rule has no `from`, so it admits any source; a stray port is attack
	// surface, and "agent" here would bypass its peer restriction.
	for port := range admitted {
		if !nonAgentDeclared[port] {
			t.Errorf("the peerless rule admits port %d, which the container "+
				"does not declare (or is the agent port, already admitted "+
				"under a peer by the rule above) — this rule has no `from`, "+
				"so anything it admits is reachable from anywhere", port)
		}
	}
}

var chartClusterScopedKinds = map[string]bool{
	"ClusterRole":                      true,
	"ClusterRoleBinding":               true,
	"CustomResourceDefinition":         true,
	"ValidatingAdmissionPolicy":        true,
	"ValidatingAdmissionPolicyBinding": true,
}

// chartNamespacedObjects is listed, not counted, so a template that stops or
// starts rendering fails here.
var chartNamespacedObjects = []string{
	"Deployment/spawnery-operator",
	"NetworkPolicy/spawnery-operator-agent",
	"PrometheusRule/spawnery-operator",
	"Role/spawnery-operator",
	"RoleBinding/spawnery-operator",
	"Service/spawnery-operator",
	"ServiceAccount/spawnery-operator",
	"ServiceMonitor/spawnery-operator",
}

// Helm resolves a typo like `{{ .Release.Namspace }}` to "", so lint and
// template pass. Rendered with the optional templates on, so the
// ServiceMonitor and PrometheusRule are covered.
func TestEveryRenderedObjectLandsInTheReleaseNamespace(t *testing.T) {
	chart := testenv.RepoPath(t, "charts/spawnery")
	cmd := exec.Command("helm", "template", "spawnery", chart,
		"--namespace", renderNamespace,
		"--set", "metrics.serviceMonitor.enabled=true",
		"--set", "metrics.prometheusRule.enabled=true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template with the optional templates on: %v\n%s", err, stderr.String())
	}
	docs, err := splitRendered(out)
	if err != nil {
		t.Fatalf("split the rendered chart: %v", err)
	}

	seen := map[string]bool{}
	for key, doc := range docs {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			t.Fatalf("%s does not parse: %v", key, err)
		}
		if chartClusterScopedKinds[obj.Kind] {
			if obj.Metadata.Namespace != "" {
				t.Errorf("%s is cluster-scoped but renders with namespace %q",
					key, obj.Metadata.Namespace)
			}
			continue
		}
		seen[key] = true
		if obj.Metadata.Namespace != renderNamespace {
			t.Errorf("%s renders with namespace %q, want %q. An empty one here is what a "+
				"typo'd .Release field produces: helm resolves it to \"\", lint and template "+
				"both exit 0, and the object installs into whatever namespace kubectl "+
				"happens to default to", key, obj.Metadata.Namespace, renderNamespace)
		}
	}

	for _, want := range chartNamespacedObjects {
		if !seen[want] {
			t.Errorf("the chart renders no %s; this test's coverage is the list it checks, "+
				"so an object that stops rendering has to fail here rather than pass by "+
				"being absent", want)
		}
	}
	for key := range seen {
		if !slices.Contains(chartNamespacedObjects, key) {
			t.Errorf("the chart renders %s, which chartNamespacedObjects does not list — "+
				"add it, so the next object to appear is checked rather than assumed", key)
		}
	}
}

// renderChartWith does not cache, since its renders vary by values.
func renderChartWith(t *testing.T, setJSON ...string) map[string][]byte {
	t.Helper()
	args := []string{"template", "spawnery", testenv.RepoPath(t, "charts/spawnery"),
		"--namespace", renderNamespace}
	for _, s := range setJSON {
		args = append(args, "--set-json", s)
	}
	cmd := exec.Command("helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	docs, splitErr := splitRendered(out)
	if splitErr != nil {
		t.Fatalf("split: %v", splitErr)
	}
	return docs
}

func helmRefuses(t *testing.T, setJSON string) string {
	t.Helper()
	cmd := exec.Command("helm", "template", "spawnery", testenv.RepoPath(t, "charts/spawnery"),
		"--namespace", renderNamespace, "--set-json", setJSON)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if _, err := cmd.Output(); err == nil {
		t.Fatalf("helm rendered %s, want a refusal", setJSON)
	}
	return stderr.String()
}

// A chart cannot know game namespaces created later, so those still use
// config/rbac/forwarding-secret-reader.yaml.
func TestNoReaderRoleIsRenderedByDefault(t *testing.T) {
	for name := range renderChart(t) {
		if strings.Contains(name, "forwarding-secret-readers") {
			t.Errorf("%s was rendered with no networkNamespaces set", name)
		}
	}
}

// Rendered from a value, resourceNames cost nothing, unlike in the
// hand-applied file.
func TestAListedNamespaceGetsANarrowedReaderRole(t *testing.T) {
	docs := renderChartWith(t,
		`networkNamespaces=[{"namespace":"minecraft","secrets":["velocity-forwarding-secret"]}]`)

	var role rbacv1.Role
	var binding rbacv1.RoleBinding
	var foundRole, foundBinding bool
	for _, doc := range docs {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			continue
		}
		if meta.Metadata.Name != "spawnery-forwarding-secret-reader" {
			continue
		}
		switch meta.Kind {
		case "Role":
			if err := yaml.Unmarshal(doc, &role); err != nil {
				t.Fatalf("parse the Role: %v", err)
			}
			foundRole = true
		case "RoleBinding":
			if err := yaml.Unmarshal(doc, &binding); err != nil {
				t.Fatalf("parse the RoleBinding: %v", err)
			}
			foundBinding = true
		}
	}
	if !foundRole || !foundBinding {
		t.Fatalf("role=%v binding=%v, want both rendered", foundRole, foundBinding)
	}

	if role.Namespace != "minecraft" {
		t.Errorf("Role namespace = %q, want the listed one", role.Namespace)
	}
	granted, err := rbacaudit.ExpandRules(role.Rules)
	if err != nil {
		t.Fatalf("ExpandRules on the narrowed Role: %v", err)
	}
	want := []rbacaudit.Permission{{
		Group: "", Resource: "secrets", Verb: "get",
		ResourceNames: []string{"velocity-forwarding-secret"},
	}}
	if diff := rbacaudit.Compare(want, granted); len(diff.Missing) > 0 || len(diff.Extra) > 0 {
		t.Errorf("missing=%v extra=%v, want exactly a get on that one secret", diff.Missing, diff.Extra)
	}

	// The hand-applied file hard-codes spawnery-system.
	if len(binding.Subjects) != 1 || binding.Subjects[0].Namespace != renderNamespace {
		t.Errorf("subjects = %+v, want one in %q", binding.Subjects, renderNamespace)
	}
	if binding.Namespace != "minecraft" {
		t.Errorf("RoleBinding namespace = %q, want the listed one", binding.Namespace)
	}
}

// A Role with no resourceNames would grant get on every Secret.
func TestAnEntryWithNoSecretsIsRefused(t *testing.T) {
	said := helmRefuses(t, `networkNamespaces=[{"namespace":"minecraft","secrets":[]}]`)
	if !strings.Contains(said, "secrets") {
		t.Errorf("helm said %q, want it to name what is missing", said)
	}

	said = helmRefuses(t, `networkNamespaces=[{"namespace":"minecraft"}]`)
	if !strings.Contains(said, "secrets") {
		t.Errorf("helm said %q for a missing key, want it to name what is missing", said)
	}
}
