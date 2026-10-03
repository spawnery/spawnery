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
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/spawnery/spawnery/internal/rbacaudit"
	"github.com/spawnery/spawnery/internal/testenv"
)

// operatorNamespace is renderNamespace because every object here comes from
// renderChart.
const operatorNamespace = renderNamespace

// foreignNamespace is not the operator's own: cluster-wide grants must hold
// there, namespaced ones must be denied (TestTheAuthorizerActuallyDenies).
const foreignNamespace = "minecraft"

// applyDeploymentAndDeriveSubject derives the subject from the manifests, so a
// binding or ServiceAccount naming the wrong thing shows up as denials.
func applyDeploymentAndDeriveSubject(t *testing.T) string {
	t.Helper()

	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: renderNamespace}}
	var sa corev1.ServiceAccount
	var clusterBinding rbacv1.ClusterRoleBinding
	var binding rbacv1.RoleBinding
	var deploy appsv1.Deployment

	renderedManifest(t, "ServiceAccount/spawnery-operator", &sa)
	renderedManifest(t, "ClusterRoleBinding/spawnery-operator", &clusterBinding)
	renderedManifest(t, "RoleBinding/spawnery-operator", &binding)
	renderedManifest(t, "Deployment/spawnery-operator", &deploy)
	clusterRole, role := readGeneratedRoles(t)

	apply(t, &ns, &sa, clusterRole, role, &clusterBinding, &binding, &deploy)

	return fmt.Sprintf("system:serviceaccount:%s:%s",
		deploy.Namespace, deploy.Spec.Template.Spec.ServiceAccountName)
}

func allowed(t *testing.T, subject string, attrs authzv1.ResourceAttributes) (bool, string) {
	t.Helper()
	c, ctx := testenv.Client(t)

	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:               subject,
			ResourceAttributes: &attrs,
		},
	}
	if err := c.Create(ctx, sar); err != nil {
		t.Fatalf("SubjectAccessReview: %v", err)
	}
	return sar.Status.Allowed, sar.Status.Reason
}

func requireGranted(t *testing.T, subject, ns string, table []rbacaudit.Permission) {
	t.Helper()
	if len(table) == 0 {
		t.Fatal("the table is empty; this test would pass without checking anything")
	}
	for _, p := range table {
		p := p
		t.Run(p.Key(), func(t *testing.T) {
			ok, reason := allowed(t, subject, authzv1.ResourceAttributes{
				Namespace:   ns,
				Group:       p.Group,
				Resource:    p.Resource,
				Subresource: p.Subresource,
				Verb:        p.Verb,
			})
			if !ok {
				t.Errorf("%s is denied for %s in namespace %q — reason: %q",
					p, subject, ns, reason)
			}
		})
	}
}

func TestEveryRequiredClusterPermissionIsGranted(t *testing.T) {
	subject := applyDeploymentAndDeriveSubject(t)
	requireGranted(t, subject, foreignNamespace, rbacaudit.RequiredCluster)
}

func TestEveryRequiredNamespacedPermissionIsGranted(t *testing.T) {
	subject := applyDeploymentAndDeriveSubject(t)
	requireGranted(t, subject, operatorNamespace, rbacaudit.RequiredNamespaced)
}

func TestClusterRoleGrantsNothingExtra(t *testing.T) {
	role, _ := readGeneratedRoles(t)
	assertNothingExtra(t, "clusterrole", role.Rules, rbacaudit.RequiredCluster)
}

// Without it a right moved into a Role would never be compared again.
func TestTheNamespacedRoleGrantsNothingExtra(t *testing.T) {
	_, role := readGeneratedRoles(t)
	assertNothingExtra(t, "role", role.Rules, rbacaudit.RequiredNamespaced)
}

func assertNothingExtra(t *testing.T, kind string, rules []rbacv1.PolicyRule, table []rbacaudit.Permission) {
	t.Helper()

	granted, err := rbacaudit.ExpandRules(rules)
	if err != nil {
		t.Fatalf("expand rules: %v", err)
	}

	diff := rbacaudit.Compare(table, granted)
	for _, p := range diff.Extra {
		t.Errorf("the %s grants %s, which no entry in the matching rbacaudit table claims", kind, p.Key())
	}
	for _, p := range diff.Missing {
		t.Errorf("the rbacaudit table lists %s, which the %s never mentions", p, kind)
	}
}

// Every other check asserts something is allowed, so a widened subject would
// leave them all green. Two probes are rights the operator holds in the
// other scope, proving the split binds where it claims to.
func TestTheAuthorizerActuallyDenies(t *testing.T) {
	subject := applyDeploymentAndDeriveSubject(t)

	denied := []struct {
		why   string
		attrs authzv1.ResourceAttributes
	}{
		{
			why: "secrets are granted by namespaced Roles only — the operator's own in " + operatorNamespace +
				", and the forwarding-secret reader in whichever namespaces an administrator applied it to, " +
				"which this one is not",
			attrs: authzv1.ResourceAttributes{Resource: "secrets", Verb: "get", Namespace: foreignNamespace},
		},
		{
			why:   "the leader lock is taken in " + operatorNamespace + " and nowhere else",
			attrs: authzv1.ResourceAttributes{Group: "coordination.k8s.io", Resource: "leases", Verb: "create", Namespace: foreignNamespace},
		},
		{
			why: "certs.Store runs on an uncached client on purpose; a cached Secret would " +
				"need an informer over every Secret in the namespace",
			attrs: authzv1.ResourceAttributes{Resource: "secrets", Verb: "list", Namespace: operatorNamespace},
		},
		{
			why:   "nothing in the operator touches nodes",
			attrs: authzv1.ResourceAttributes{Resource: "nodes", Verb: "delete"},
		},
		{
			why:   "an operator that may write RBAC can grant itself everything else",
			attrs: authzv1.ResourceAttributes{Group: "rbac.authorization.k8s.io", Resource: "clusterroles", Verb: "create"},
		},
	}

	for _, probe := range denied {
		probe := probe
		name := probe.attrs.Resource + "/" + probe.attrs.Verb
		if probe.attrs.Namespace != "" {
			name += "@" + probe.attrs.Namespace
		}
		t.Run(name, func(t *testing.T) {
			if ok, _ := allowed(t, subject, probe.attrs); ok {
				t.Errorf("the authorizer allows %s for %s, but %s — either a role is too "+
					"wide, or a second binding made the SubjectAccessReview direction of "+
					"this audit meaningless", name, subject, probe.why)
			}
		})
	}
}

// readerProbeNamespace is not foreignNamespace, where secrets/get must stay
// denied; sharing it would make the suite depend on test order.
const readerProbeNamespace = "spawnery-reader-probe"

// The reader Role is hand-written, so nothing else compares it to anything.
func TestTheForwardingSecretReaderGrantsNothingExtra(t *testing.T) {
	role, _ := readForwardingSecretReader(t)
	assertNothingExtra(t, "forwarding-secret-reader role", role.Rules, rbacaudit.RequiredNetworkNamespace)
}

func TestTheForwardingSecretReaderGrantsEverythingRequired(t *testing.T) {
	role, _ := readForwardingSecretReader(t)
	granted, err := rbacaudit.ExpandRules(role.Rules)
	if err != nil {
		t.Fatalf("expand rules: %v", err)
	}
	if diff := rbacaudit.Compare(rbacaudit.RequiredNetworkNamespace, granted); len(diff.Missing) > 0 {
		t.Errorf("the reader role is missing %v — the operator cannot read a forwarding secret "+
			"even where an administrator applied it", diff.Missing)
	}
}

// chartDefaultNamespace is the chart's documented default, which the
// hand-applied reader file promises to follow, not renderNamespace.
const chartDefaultNamespace = "spawnery-system"

// A RoleBinding naming the wrong ServiceAccount parses and grants nothing.
// The probe subject comes from the binding itself, so the probe can only
// prove Role and binding fit; the subject's name and namespace are checked
// separately against the chart's ServiceAccount and chartDefaultNamespace.
func TestTheForwardingSecretReaderOpensExactlyOneNamespace(t *testing.T) {
	applyForwardingSecretReader(t, readerProbeNamespace)
	_, binding := readForwardingSecretReader(t)
	if len(binding.Subjects) != 1 {
		t.Fatalf("the reader RoleBinding has %d subjects, want exactly one", len(binding.Subjects))
	}
	subj := binding.Subjects[0]

	var sa corev1.ServiceAccount
	renderedManifest(t, "ServiceAccount/spawnery-operator", &sa)
	if subj.Kind != "ServiceAccount" {
		t.Errorf("the reader RoleBinding's subject is a %q, want a ServiceAccount", subj.Kind)
	}
	if subj.Name != sa.Name {
		t.Errorf("%s names the ServiceAccount %q, but the chart renders %q — the binding "+
			"would apply cleanly and grant the operator nothing, and the only symptom is a "+
			"Network's ForwardingSecretResolved going SecretReadForbidden at runtime",
			forwardingSecretReaderManifest, subj.Name, sa.Name)
	}
	if subj.Namespace != chartDefaultNamespace {
		t.Errorf("%s names the namespace %q, but the chart's documented default is %q — "+
			"the file's own header says it tracks that default, and an administrator who "+
			"installed elsewhere edits this line by hand (charts/spawnery/README.md)",
			forwardingSecretReaderManifest, subj.Namespace, chartDefaultNamespace)
	}

	subject := fmt.Sprintf("system:serviceaccount:%s:%s", subj.Namespace, subj.Name)

	if ok, reason := allowed(t, subject, authzv1.ResourceAttributes{
		Namespace: readerProbeNamespace, Resource: "secrets", Verb: "get",
	}); !ok {
		t.Errorf("secrets/get is denied in %s after applying the reader role — reason: %q",
			readerProbeNamespace, reason)
	}

	for _, verb := range []string{"list", "watch", "create", "update", "delete"} {
		t.Run(verb, func(t *testing.T) {
			if ok, _ := allowed(t, subject, authzv1.ResourceAttributes{
				Namespace: readerProbeNamespace, Resource: "secrets", Verb: verb,
			}); ok {
				t.Errorf("the reader role allows secrets/%s in %s; it exists to grant get and "+
					"nothing else", verb, readerProbeNamespace)
			}
		})
	}
}

// forwardingSecretReaderManifest is hand-written and applied per namespace;
// controller-gen never touches it.
const forwardingSecretReaderManifest = "config/rbac/forwarding-secret-reader.yaml"

// readForwardingSecretReader reads the file off disk, since the chart never
// templates it, and refuses a second object of a kind it already saw.
func readForwardingSecretReader(t *testing.T) (*rbacv1.Role, *rbacv1.RoleBinding) {
	t.Helper()

	var role *rbacv1.Role
	var binding *rbacv1.RoleBinding
	readMultiDocManifest(t, forwardingSecretReaderManifest, func(kind string, doc []byte) {
		switch kind {
		case "Role":
			decoded := &rbacv1.Role{}
			if err := yaml.Unmarshal(doc, decoded); err != nil {
				t.Fatalf("decode the Role in %s: %v", forwardingSecretReaderManifest, err)
			}
			if role != nil {
				t.Fatalf("%s contains more than one Role (%q and %q). This audit compares "+
					"exactly one against rbacaudit.RequiredNetworkNamespace; taking the last "+
					"would leave the other unchecked in both directions without saying so",
					forwardingSecretReaderManifest, role.Name, decoded.Name)
			}
			role = decoded
		case "RoleBinding":
			decoded := &rbacv1.RoleBinding{}
			if err := yaml.Unmarshal(doc, decoded); err != nil {
				t.Fatalf("decode the RoleBinding in %s: %v", forwardingSecretReaderManifest, err)
			}
			if binding != nil {
				t.Fatalf("%s contains more than one RoleBinding (%q and %q)",
					forwardingSecretReaderManifest, binding.Name, decoded.Name)
			}
			binding = decoded
		default:
			t.Fatalf("%s contains an unexpected %s; this audit only models a Role and a RoleBinding",
				forwardingSecretReaderManifest, kind)
		}
	})
	if role == nil {
		t.Fatalf("%s contains no Role", forwardingSecretReaderManifest)
	}
	if binding == nil {
		t.Fatalf("%s contains no RoleBinding", forwardingSecretReaderManifest)
	}
	return role, binding
}

// applyForwardingSecretReader models `kubectl apply -n <namespace>`: neither
// object carries a namespace of its own.
func applyForwardingSecretReader(t *testing.T, namespace string) {
	t.Helper()

	role, binding := readForwardingSecretReader(t)
	role.Namespace = namespace
	binding.Namespace = namespace

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	apply(t, ns, role, binding)
}
