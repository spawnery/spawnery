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

	"github.com/spawnery/spawnery/internal/rbacaudit"
	"github.com/spawnery/spawnery/internal/testenv"
)

// operatorNamespace is where the operator itself runs, and the only namespace
// the namespaced Role grants anything in.
const operatorNamespace = "spawnery-system"

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

	var ns corev1.Namespace
	var sa corev1.ServiceAccount
	var clusterBinding rbacv1.ClusterRoleBinding
	var binding rbacv1.RoleBinding
	var deploy appsv1.Deployment

	readManifest(t, "config/deploy/namespace.yaml", &ns)
	readManifest(t, "config/deploy/serviceaccount.yaml", &sa)
	readManifest(t, "config/deploy/clusterrolebinding.yaml", &clusterBinding)
	readManifest(t, "config/deploy/rolebinding.yaml", &binding)
	readManifest(t, "config/deploy/deployment.yaml", &deploy)
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
			why:   "secrets are granted by the namespaced Role, and only in " + operatorNamespace,
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
