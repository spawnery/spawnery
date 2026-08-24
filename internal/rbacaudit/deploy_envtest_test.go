/*
Copyright The Spawnery Authors.

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
	"github.com/spawnery/spawnery/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	_ = testenv.Stop()
	os.Exit(code)
}

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

	// Only meaningful away from the chart's default: rendered at
	// spawnery-system itself, a template that correctly substitutes
	// .Release.Namespace and one that forgot it and hard-coded the default
	// produce byte-identical output, so scanning for the literal here would
	// report every object as broken even when nothing is. That is the whole
	// reason renderNamespace is not spawnery-system in the first place.
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

// renderedObjectKeys is every object the chart renders with default values,
// keyed the way splitRendered keys them. It is a closed set on purpose.
//
// The rest of this package audits objects it names one at a time:
// readGeneratedRoles looks up two keys, applyDeploymentAndDeriveSubject
// applies six, and splitRendered refuses only a duplicate kind and name. A
// template added to the chart is therefore invisible to all of them. Add
// charts/spawnery/templates/metrics-rbac.yaml rendering a second ClusterRole
// and a ClusterRoleBinding granting the operator's ServiceAccount
// secrets: list cluster-wide, and nothing above would say a word:
// TestClusterRoleGrantsNothingExtra reads only ClusterRole/spawnery-operator,
// and TestTheAuthorizerActuallyDenies would notice the widened subject only if
// the new binding were among the objects it applies -- which it is not,
// because that list is fixed. An unaudited cluster-wide secrets grant would
// ship green.
//
// So the list below is the gate. A new template fails this test until someone
// adds its key here, and adding the key is the moment to decide what audits
// it: a new binding belongs in applyDeploymentAndDeriveSubject, a new role in
// readGeneratedRoles and the rbacaudit tables, or the object needs a reason
// recorded here for why neither applies to it.
var renderedObjectKeys = []string{
	"ClusterRole/spawnery-operator",
	"ClusterRoleBinding/spawnery-operator",
	"CustomResourceDefinition/networks.spawnery.cloud",
	"CustomResourceDefinition/proxygroups.spawnery.cloud",
	"CustomResourceDefinition/servergroups.spawnery.cloud",
	"CustomResourceDefinition/servers.spawnery.cloud",
	"Deployment/spawnery-operator",
	"NetworkPolicy/spawnery-operator-agent",
	"Role/spawnery-operator",
	"RoleBinding/spawnery-operator",
	"Service/spawnery-operator",
	"ServiceAccount/spawnery-operator",
}

// TestTheChartRendersExactlyTheseObjects is the audit's own completeness
// check: not "does every object this package names render", which every other
// test here already answers, but "does the chart render anything this package
// has never looked at".
//
// Default values, because that is what renderChart uses. The one object under
// a condition is the NetworkPolicy (.Values.networkPolicy.enabled, default
// true); a value that switched something else off would show up here as a
// missing key rather than silently reducing what the audit covers.
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

// renderedManifest decodes one rendered object, strictly. sigs.k8s.io/yaml's
// plain Unmarshal drops keys the target type does not have, so a typo like
// `serviceAccountNam:` or `readOnlyRootFilesytem:` would decode into a zero
// value and every assertion below would then be checking a field nobody set.
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

// generatedClusterRoleKey and generatedRoleKey are the rendered chart's keys
// (see renderChart, splitRendered) for the two roles the controller-gen
// markers ask for: the cluster-wide ClusterRole and the Role scoped to the
// operator's own namespace. readGeneratedRoles below uses these same
// constants both to look the objects up and to name them in a "the chart
// renders no ..." failure, so the two strings cannot drift apart the way a
// separately hand-written diagnostic could.
//
// controller-gen still writes config/rbac/role.yaml, and
// hack/chart-templates.sh (run by `make manifests`) transforms it into
// charts/spawnery/templates/rbac.yaml before anything is installed. Auditing
// config/rbac/role.yaml would never go near that sed, so a transform that
// stopped applying — writing a Role whose namespace is still the literal
// spawnery-system — would leave every assertion here green. Auditing the
// rendered chart is what sees it. The script now carries its own
// postcondition over the file it writes as well, which fails at `make
// manifests` time rather than here; both are wanted, because only one of them
// runs when somebody edits the chart by hand.
const (
	generatedClusterRoleKey = "ClusterRole/spawnery-operator"
	generatedRoleKey        = "Role/spawnery-operator"
)

// readMultiDocManifest splits a multi-document YAML manifest at rel and hands
// each document's Kind and raw bytes to decode. Its only caller today is
// readForwardingSecretReader (audit_envtest_test.go), for
// config/rbac/forwarding-secret-reader.yaml — a Role and a RoleBinding in one
// file, so a single-document decode cannot read it. readGeneratedRoles used to
// be a second caller, over config/rbac/role.yaml, before the chart became the
// source of truth for the generated roles; it now reads the rendered chart's
// ClusterRole and Role directly through renderedManifest instead. decode owns
// everything kind-specific — including its own "second one of this kind"
// refusal and its own diagnostics — this helper only owns finding the file,
// splitting it, and skipping blank documents.
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

// readGeneratedRoles decodes both halves of the generated RBAC manifest from
// the rendered chart. It insists on finding exactly one of each.
//
// A missing Role means a namespace= qualifier fell off a marker and the
// operator would hold its Secret and Lease rights everywhere. A *second*
// object of either kind used to be this function's own refusal; it is now
// splitRendered's, enforced for every kind and name the chart renders, not
// just these two — returning one of two would leave the other unaudited in
// both directions while every test still reported green, whichever helper
// catches it first.
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

// apply creates objects that several tests in this package share. The cluster
// scoped ones — ClusterRole and ClusterRoleBinding — outlive a single test in
// the shared control plane, so creating them twice is normal and not a failure.
//
// Tolerating AlreadyExists is what makes the sharing work and is also what used
// to hide a real hazard: a second caller asking for a *different* object under
// a name already taken got the first one, silently, and then audited it. Every
// test in this package exists to check a rendered ClusterRole against a table,
// so auditing a stale one is the failure mode that matters most here and would
// have looked exactly like success.
//
// So the object that is already there is now checked against the one being
// asked for, and only for the two types where the difference changes what an
// audit means. Deliberately not a create-or-update: several of the objects
// here carry fields the API server assigns and forbids changing — a Service's
// clusterIP among them — so updating generically would trade a silent staleness
// for a noisy failure that has nothing to do with what is being tested.
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

// assertSameRules fails when a ClusterRole or Role already in the cluster
// carries different rules from the one a test just tried to create. Anything
// else is left alone: a namespace, a ServiceAccount and a binding carry no
// rules, and a Deployment or Service that already exists is scenery for these
// tests rather than their subject.
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

// TestDeployManifestsAreAcceptedAndConsistent applies the deployment manifests
// to a real API server and checks that they refer to each other correctly.
// A binding that names the wrong role, or a deployment that runs under the
// wrong ServiceAccount, would leave the operator without permissions while
// every manifest on its own still looked fine.
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

	// ns.Name is renderNamespace by construction, not read from anywhere
	// independent, so there is nothing left to check it against here;
	// TestTheChartRendersIntoTheNamespaceItIsGiven is what proves the chart
	// actually renders into the namespace it is given. What still matters is
	// that every other rendered object agrees with the namespace this test
	// chose to apply into.
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

// TestTheRoleBindingBindsTheOperatorInItsOwnNamespace is the namespaced half of
// TestDeployManifestsAreAcceptedAndConsistent. Nothing else reads
// rolebinding.yaml, so a RoleRef naming a ClusterRole, a subject naming the
// wrong ServiceAccount, or a binding sitting in a namespace other than the one
// controller-gen put the Role in would all leave certs.Store.Ensure with
// Forbidden on the operator's own Secret while every file on its own still
// looked plausible.
func TestTheRoleBindingBindsTheOperatorInItsOwnNamespace(t *testing.T) {
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: renderNamespace}}
	var sa corev1.ServiceAccount
	var binding rbacv1.RoleBinding

	renderedManifest(t, "ServiceAccount/spawnery-operator", &sa)
	renderedManifest(t, "RoleBinding/spawnery-operator", &binding)
	_, role := readGeneratedRoles(t)

	apply(t, &ns, role, &binding)

	// The Role's namespace comes from the markers, the binding's from the
	// manifest. If they ever drift the binding grants nothing at all.
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

// TestAgentServiceReachesTheOperatorPods checks the one path nothing else
// covers: a game server pod dials spawnery-operator.<ns>.svc:9443, and every
// hop of that address lives in a different file. A selector that matches
// nothing, or a targetPort naming a container port that does not exist, leaves
// the agents with a connection refused and the operator looking healthy.
func TestAgentServiceReachesTheOperatorPods(t *testing.T) {
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: renderNamespace}}
	var svc corev1.Service
	var deploy appsv1.Deployment
	renderedManifest(t, "Service/spawnery-operator", &svc)
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)

	apply(t, &ns, &svc)

	// The name is the first hop, and the only one no other test touches: the
	// operator builds both the agents' dial address and its own certificate
	// SANs from podspec.AgentServiceName, and never compares either against
	// the manifest. Renaming the Service here alone would leave every agent
	// dialling a name that resolves to nothing, and the suite fully green.
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

	// The readiness probe is what keeps a standby out of the Service: readyz
	// only turns green once this replica holds the leader lock, and an unready
	// pod is not an endpoint. A probe pointing anywhere else would silently
	// undo that.
	probe := container.ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		t.Fatal("the operator has no HTTP readiness probe; a standby would serve as an endpoint")
	}
	if probe.HTTPGet.Path != "/readyz" {
		t.Errorf("readiness probe path = %q, want /readyz", probe.HTTPGet.Path)
	}
}

// The other end of tying readiness to the leader lock, and a deadlock rather
// than a slowdown: with one replica the default RollingUpdate resolves to
// maxSurge 1 and maxUnavailable 0, so the new pod has to report Ready before
// the old one is removed. Readiness now waits for the lease, and the lease is
// held by the pod that is waiting to be removed. The rollout stops there until
// someone deletes the old pod by hand. Recreate is the honest shape for a
// single-replica leader-elected operator, and this test is what stops it from
// being "improved" back.
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

// TestOperatorPodIsRestrictedCompliant guards the design decision that the
// operator itself runs under Pod Security "restricted" — the same profile it
// enforces on the game servers it creates.
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

// TestTheOperatorDeploymentCarriesProductionFlags is the guard
// docs/known-issues.md asked for under "The flags in the Deployment are
// unchecked": sigs.k8s.io/yaml is not strict, so a mistyped key disappears
// silently, and until now nothing looked at the container's arguments at all.
//
// The floor on --startup-deadline is the point. These manifests are what gets
// installed, not test scaffolding: milestone 5a's evidence run measured 24
// seconds from apply to ReadyGatePassed on an idle single-node kind cluster
// with the image already present -- the favourable case in every dimension
// that matters, since there was no image pull, no contention and no world to
// read. A manifest carrying 20s would fail every server on a real cluster.
// hack/e2e.sh gets its short deadline by appending a second occurrence of the
// flag, which Go's flag package resolves to the last one.
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

// TestTheOperatorImageIsNotAMutableTag guards what the manifest points at. It
// named ghcr.io/spawnery/spawnery-operator:dev until milestone 6a -- a tag
// nothing produced, so the manifest referenced nothing at all. The master
// design's §8 asks for digest references in shipped manifests because tags are
// mutable; hack/publish.sh writes one in after a push, and until it has run the
// version tag is what resolves.
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

	// And the tag has to be one that gets published. nix/operator-image.nix
	// takes the image's tag straight from flake.nix's operatorVersion, so
	// bumping that version and forgetting this line leaves the manifest
	// pointing at a tag hack/publish.sh will never push again. Nothing else in
	// the tree would notice: `make e2e` patches the image away before the
	// cluster ever pulls it, and the whole point of milestone 6a's §3.3 is
	// that operatorVersion moves on its own schedule, so this will happen
	// while imageVersion sits still. Once acceptance criterion 7 closes and a
	// real digest lands here the CutPrefix above returns first, and this stops
	// applying -- until then it is the only thing keeping the tag honest.
	if want := operatorVersionFromFlake(t); tag != want {
		t.Errorf("image = %q, but flake.nix's operatorVersion is %q. "+
			"nix/operator-image.nix tags the image with operatorVersion, so this "+
			"manifest names a tag nothing builds or publishes", ref, want)
	}
}

// operatorVersionFromFlake reads the one line of flake.nix that sets
// operatorVersion.
//
// Text and a regexp, and not `nix eval`: shelling out to nix from `make test`
// would put a several-second evaluation, a network-capable tool and a
// dependency on nix being installed at all into the commit loop, for one
// string. The cost of reading it as text is that the regexp is coupled to the
// line's shape -- so it fails loudly when the shape moves rather than
// returning an empty string and passing. That is the failure mode that matters
// here: this whole assertion exists because a value can go stale unnoticed.
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

// TestTheChartAgreesWithTheFlakeAboutTheOperatorRelease pins the three places
// that name the operator's version to each other.
//
// Chart.yaml's own comment says version and appVersion move independently, and
// that is right: a chart fix touching no image is a chart bump alone. But
// appVersion is not free -- it is the operator release the chart installs by
// default, so it has to agree with flake.nix's operatorVersion and with the
// tag values.yaml renders. On 2026-08-20 all three drifted apart in one
// release and nothing noticed: operatorVersion went to 0.1.1 while Chart.yaml
// stayed at 0.1.0, and the cluster kept serving the previous chart.
//
// What this does NOT catch is that failure's actual cause, and saying so is
// the point: Chart.yaml's `version` is what Flux packages the artifact under,
// so leaving it still means a changed chart never reaches a cluster. No unit
// test can know whether the chart changed since the last release -- that is a
// question about two commits, and .github/workflows/release.yml asks it.
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

// TestLeaderElectionPermissionIsGranted is the regression test for a real gap:
// leader election is on by default and locks on a Lease, but no kubebuilder
// marker declared that permission, so the generated role never granted it and
// the operator would have failed on startup with Forbidden.
//
// The right now lives in the namespaced Role, where it belongs — the lock is
// taken in the operator's own namespace, and granting it cluster-wide would let
// the operator lock anything anywhere. This test therefore also insists the
// ClusterRole stays out of it.
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

// TestTheAgentPolicySelectsTheOperatorAndAdmitsManagedPods checks the one
// shipped NetworkPolicy, and every hop of it lives in a different file.
//
// The mistakes it exists to catch. A podSelector copied from a managed pod's
// labels selects nothing here — the operator pod deliberately does not carry
// spawnery.cloud/managed-by — and a policy that selects nothing fails open,
// which looks exactly like one that works. A peer without an empty
// namespaceSelector would admit only pods in spawnery-system, while every
// managed pod in the cluster dials in from its own game namespace. A
// policyTypes line that declares Egress on a policy with no egress rules
// default-denies the operator's own outbound traffic. And these two labels
// exist in three places — this manifest, the Deployment, and
// podspec.OperatorPodLabels() — of which the third builds the per-Network
// policy's egress peer, so a drift between any two of them breaks a policy
// nothing else would notice.
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

	// podspec.OperatorPodLabels() is the third copy of these two labels, and
	// until now nothing tied it to either of the other two. The per-Network
	// policy's egress peer is built from it, so renaming the operator's pod
	// labels in the Deployment would leave that peer selecting nothing —
	// backends unable to reach the operator on an enforcing CNI — with every
	// test in the repository green.
	//
	// Subset rather than equality against the Deployment's template labels: a
	// pod may legitimately gain labels neither selector names, and a rename is
	// caught either way. Equality against the policy's own selector, though,
	// including the length: the loop above validates only the keys the policy
	// declares, so a selector narrowed to one of the two labels passed it, and
	// a narrowed selector is a widened policy.
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

	// policyTypes is not decoration and the mistake here is the mirror of the
	// one internal/podspec's TestBuildNetworkPolicyDeclaresBothPolicyTypes
	// guards. This policy carries ingress rules only; adding Egress with no
	// egress rules makes the operator pod default-deny for egress, so wherever
	// a CNI enforces it cannot reach the API server — every controller stops,
	// at once, from a one-word manifest edit.
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

	// Selecting the pod at all makes it default-deny for ingress, which covers
	// the kubelet's probes and any metrics scrape. Both have to be admitted
	// explicitly or the operator goes NotReady the moment this policy lands.
	if probeRule == nil {
		t.Fatal("no peerless ingress rule: the kubelet's probe to the health " +
			"port is denied, and the operator goes NotReady")
	}
	// Refused rather than modelled, for the same reason ExpandRules refuses a
	// rule it cannot represent. IntValue() discards its Atoi error and returns
	// 0 for a named port, so `port: metrics` here used to report as "admits
	// port 0" -- safe, because 0 is never a declared container port and the
	// reverse check below rejects it either way, but a message about a port
	// nobody wrote. A named port is legal in a NetworkPolicy and Kubernetes
	// resolves it against the pod's own port names; comparing it here would
	// mean resolving it the same way, which this check does not do. A nil port
	// is the other unmodellable shape: it admits every port on the pod, and an
	// empty admitted set would then pass the reverse direction by having
	// nothing to check.
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
	// nonAgentDeclared is every container port except "agent": that one is
	// already admitted, from a peer, by the rule above. Anything the peerless
	// rule admits has to be one of these and nothing else.
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
	// The reverse direction matters here in a way it does not for the agent
	// rule: this rule has no `from`, so it admits from any source in any
	// namespace, and a stray port line here is real attack surface rather
	// than a typo the agent rule's peer would already contain. Every port it
	// admits must be a container port that is not "agent" -- admitting that
	// one here too would bypass the agent rule's peer restriction entirely.
	for port := range admitted {
		if !nonAgentDeclared[port] {
			t.Errorf("the peerless rule admits port %d, which the container "+
				"does not declare (or is the agent port, already admitted "+
				"under a peer by the rule above) — this rule has no `from`, "+
				"so anything it admits is reachable from anywhere", port)
		}
	}
}

// chartClusterScopedKinds are the kinds this chart renders that carry no
// namespace at all. Everything else it renders is namespaced, and an object of
// a namespaced kind with an empty namespace is exactly the failure below.
var chartClusterScopedKinds = map[string]bool{
	"ClusterRole":              true,
	"ClusterRoleBinding":       true,
	"CustomResourceDefinition": true,
}

// chartNamespacedObjects is every namespaced object the chart renders with the
// optional templates switched on. Listed rather than counted so that a
// template which stops rendering fails here instead of passing vacuously; a
// template that is *added* fails too, which is the point — a new object with a
// namespace nobody checked is what this test exists to stop.
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

// TestEveryRenderedObjectLandsInTheReleaseNamespace closes the entry in
// docs/known-issues.md: "`make chart-lint` does not catch a chart that renders
// with an empty namespace."
//
// A typo'd `{{ .Release.Namspace }}` is not a render failure. Helm resolves an
// unknown `.Release` field to the empty string, so `helm lint` and `helm
// template` both exit 0 and `make chart-lint` sees nothing. The literal scan in
// TestTheChartRendersIntoTheNamespaceItIsGiven does not see it either — an
// empty namespace contains no "spawnery-system" to find — and that test reads
// the namespace of two objects out of nine. What caught it was
// TestAgentServiceReachesTheOperatorPods, incidentally, because envtest's API
// server refuses to create a Service with an empty namespace; nothing caught it
// for the six objects that test does not apply.
//
// This reads every object instead. It renders with the optional templates
// enabled so the ServiceMonitor and the PrometheusRule are covered too: both
// are off by default, so the ordinary render never sees them at all.
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
