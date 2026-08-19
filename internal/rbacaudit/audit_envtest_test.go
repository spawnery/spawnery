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

// operatorNamespace is where the operator itself runs, and the only namespace
// the namespaced Role grants anything in. It is renderNamespace
// (deploy_envtest_test.go), not the literal spawnery-system: every object this
// file applies now comes from renderChart, which renders into renderNamespace,
// so a Role or Deployment applied here lands there and nowhere else.
const operatorNamespace = renderNamespace

// foreignNamespace is a namespace that is not the operator's own. The
// cluster-wide half of the permissions must answer the same there — the binding
// is cluster-wide, so any namespace would do, and the one from the sample
// manifest keeps the failure messages recognisable. The namespaced half must
// answer denied there, which is what TestTheAuthorizerActuallyDenies checks.
const foreignNamespace = "minecraft"

// applyDeploymentAndDeriveSubject applies the deployment manifests and returns
// the subject the operator actually runs as, derived from the manifests rather
// than restated. That way a binding naming the wrong role, or a deployment
// naming the wrong ServiceAccount, shows up as denied permissions instead of
// passing silently.
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

// allowed asks the real RBAC authorizer whether subject may do attrs.
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

// requireGranted asks the authorizer, one permission at a time, whether the
// operator's ServiceAccount may do it in namespace ns. A denial here means the
// operator would hit Forbidden in a real cluster.
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

// TestEveryRequiredClusterPermissionIsGranted covers the cluster-wide half,
// checked in a namespace that is not the operator's own: what the ClusterRole
// grants must hold everywhere, because the operator manages game servers in
// namespaces it does not know in advance.
func TestEveryRequiredClusterPermissionIsGranted(t *testing.T) {
	subject := applyDeploymentAndDeriveSubject(t)
	requireGranted(t, subject, foreignNamespace, rbacaudit.RequiredCluster)
}

// TestEveryRequiredNamespacedPermissionIsGranted covers the half the operator
// only ever needs where it runs itself. Checked in that namespace only —
// TestTheAuthorizerActuallyDenies is the other side of the same statement.
func TestEveryRequiredNamespacedPermissionIsGranted(t *testing.T) {
	subject := applyDeploymentAndDeriveSubject(t)
	requireGranted(t, subject, operatorNamespace, rbacaudit.RequiredNamespaced)
}

// TestClusterRoleGrantsNothingExtra is the other direction: every verb the
// role grants must appear in the table. An operator that can create pods is
// worth keeping narrow.
func TestClusterRoleGrantsNothingExtra(t *testing.T) {
	role, _ := readGeneratedRoles(t)
	assertNothingExtra(t, "clusterrole", role.Rules, rbacaudit.RequiredCluster)
}

// TestTheNamespacedRoleGrantsNothingExtra is the same direction for the
// namespaced half. Without it a right could be moved out of RequiredCluster
// into a Role and never be compared against a manifest again.
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

// TestTheAuthorizerActuallyDenies is what keeps the SubjectAccessReview
// direction meaningful. Every other check here asserts that something is
// allowed, so a second binding that widened the subject would leave all of them
// green no matter what the roles say, and the file-based direction would be
// carrying the whole promise alone.
//
// The probes are chosen so that a wrong answer names its own cause. Two of them
// are rights the operator genuinely holds — in the other scope: secrets in a
// namespace that is not its own, and a lease there. Those are what prove the
// split actually binds where it claims to, rather than having quietly landed in
// the ClusterRole.
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

// readerProbeNamespace is deliberately not foreignNamespace. The Role applied
// below grants secrets/get, which is exactly what TestTheAuthorizerActuallyDenies
// requires to stay denied in foreignNamespace — applying it there would make
// this suite pass or fail by test order.
const readerProbeNamespace = "spawnery-reader-probe"

// The reader Role is hand-written rather than generated from a marker, because
// the namespace is not known until an administrator applies it. Both directions
// of the audit therefore matter more here, not less: nothing else compares this
// file against anything.
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

// The file has to work when applied, not only when parsed: a RoleBinding whose
// subject names the wrong ServiceAccount parses perfectly and grants nothing.
//
// The subject comes from the reader file's own RoleBinding, not from
// applyDeploymentAndDeriveSubject. forwarding-secret-reader.yaml's subject
// names its ServiceAccount and namespace as a literal in the file (spawnery-
// operator in spawnery-system — see the file's own comments on why: kubectl
// apply -n rewrites the Role's and RoleBinding's own metadata.namespace, never
// a namespace field inside a subject), not wherever this package's render
// happens to install the chart. Deriving the probe subject from the render
// would tie this test to renderNamespace, which the file never promised to
// track, instead of to what the file actually says.
func TestTheForwardingSecretReaderOpensExactlyOneNamespace(t *testing.T) {
	applyForwardingSecretReader(t, readerProbeNamespace)
	_, binding := readForwardingSecretReader(t)
	if len(binding.Subjects) != 1 {
		t.Fatalf("the reader RoleBinding has %d subjects, want exactly one", len(binding.Subjects))
	}
	subj := binding.Subjects[0]
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

// forwardingSecretReaderManifest is the repository-relative path of the
// hand-written Role and RoleBinding an administrator applies per namespace.
// Unlike the objects readGeneratedRoles reads, controller-gen never touches this file — nothing
// else in the build checks it against anything, which is why both directions
// of the audit run against it here.
const forwardingSecretReaderManifest = "config/rbac/forwarding-secret-reader.yaml"

// readForwardingSecretReader decodes both objects in
// forwardingSecretReaderManifest, using the same multi-document split
// readGeneratedRoles uses (readMultiDocManifest, in deploy_envtest_test.go).
// Like readGeneratedRoles it refuses to silently drop a second object of a
// kind it already saw — except this file holds a Role and a RoleBinding
// rather than a ClusterRole and a Role.
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

// applyForwardingSecretReader models what an administrator's
// `kubectl apply -n <namespace>` does: it creates namespace if it does not
// already exist, then creates the decoded Role and RoleBinding with
// metadata.namespace set to it. Neither object in the file carries a
// namespace of its own — kubectl -n supplies it — so proving the file works
// applied, and not only parsed, means setting it here the same way.
func applyForwardingSecretReader(t *testing.T, namespace string) {
	t.Helper()

	role, binding := readForwardingSecretReader(t)
	role.Namespace = namespace
	binding.Namespace = namespace

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	apply(t, ns, role, binding)
}
