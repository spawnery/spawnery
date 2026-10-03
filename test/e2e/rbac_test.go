//go:build e2e

package e2e

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authzv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/rbacaudit"
)

// theTableHoldsAgainstTheRealAuthorizer asks the cluster, one permission at a
// time, whether the operator's ServiceAccount may do what the table says it
// needs.
//
// SubjectAccessReview, so the test keeps its own admin rights.
func theTableHoldsAgainstTheRealAuthorizer(t *testing.T) {
	subject := operatorSubject(t)

	check := func(p rbacaudit.Permission, namespace string) {
		review := &authzv1.SubjectAccessReview{
			Spec: authzv1.SubjectAccessReviewSpec{
				User: subject,
				ResourceAttributes: &authzv1.ResourceAttributes{
					Namespace:   namespace,
					Group:       p.Group,
					Resource:    p.Resource,
					Subresource: p.Subresource,
					Verb:        p.Verb,
				},
			},
		}
		if err := k8s.Create(ctx, review); err != nil {
			t.Fatalf("SubjectAccessReview for %s: %v", p, err)
		}
		if !review.Status.Allowed {
			where := "cluster-wide"
			if namespace != "" {
				where = "in namespace " + namespace
			}
			t.Errorf("%s is denied %s %s: %s. The table says the code needs it (%s)",
				subject, p.Key(), where, review.Status.Reason, p.Why)
		}
	}

	if len(rbacaudit.RequiredCluster) == 0 || len(rbacaudit.RequiredNamespaced) == 0 ||
		len(rbacaudit.RequiredNetworkNamespace) == 0 {
		t.Fatalf("a required-permissions table is empty (cluster=%d, namespaced=%d, "+
			"network-namespace=%d): a loop over it would pass without asking the cluster "+
			"anything, which is not this test's purpose",
			len(rbacaudit.RequiredCluster), len(rbacaudit.RequiredNamespaced),
			len(rbacaudit.RequiredNetworkNamespace))
	}

	for _, p := range rbacaudit.RequiredCluster {
		check(p, "")
	}
	for _, p := range rbacaudit.RequiredNamespaced {
		check(p, operatorNamespace)
	}
	// Swapping in the operator's own namespace stays green: `secrets: get` is
	// granted there too, for certs.Store.Ensure.
	for _, p := range rbacaudit.RequiredNetworkNamespace {
		check(p, testNamespace)
	}

	t.Logf("checked %d cluster, %d namespaced and %d per-network permissions",
		len(rbacaudit.RequiredCluster), len(rbacaudit.RequiredNamespaced),
		len(rbacaudit.RequiredNetworkNamespace))
}

// operatorSubject derives the user name every SubjectAccessReview above asks
// about from the live ClusterRoleBinding, and checks that the Deployment runs
// as that ServiceAccount. A restated literal would stay green with the binding
// pointing elsewhere.
func operatorSubject(t *testing.T) string {
	t.Helper()

	var binding rbacv1.ClusterRoleBinding
	if err := k8s.Get(ctx, client.ObjectKey{Name: "spawnery-operator"}, &binding); err != nil {
		t.Fatalf("get ClusterRoleBinding spawnery-operator: %v", err)
	}
	if binding.RoleRef.Kind != "ClusterRole" {
		t.Fatalf("the binding's roleRef is a %s, not a ClusterRole", binding.RoleRef.Kind)
	}

	// A RoleRef pointing at nothing is legal and grants nothing.
	var role rbacv1.ClusterRole
	if err := k8s.Get(ctx, client.ObjectKey{Name: binding.RoleRef.Name}, &role); err != nil {
		t.Fatalf("the binding names ClusterRole %q, which this cluster does not have: %v",
			binding.RoleRef.Name, err)
	}

	if len(binding.Subjects) != 1 {
		t.Fatalf("the binding has %d subjects, want exactly one; a second subject would "+
			"widen the grant and leave this subtest unable to say which account it "+
			"measured", len(binding.Subjects))
	}
	subj := binding.Subjects[0]
	if subj.Kind != "ServiceAccount" {
		t.Fatalf("the binding's subject is a %s, not a ServiceAccount", subj.Kind)
	}

	var deploy appsv1.Deployment
	key := client.ObjectKey{Namespace: operatorNamespace, Name: "spawnery-operator"}
	if err := k8s.Get(ctx, key, &deploy); err != nil {
		t.Fatalf("get Deployment %s: %v", key, err)
	}
	if got := deploy.Spec.Template.Spec.ServiceAccountName; got != subj.Name ||
		deploy.Namespace != subj.Namespace {
		t.Fatalf("the ClusterRoleBinding grants %s/%s, but the operator pod runs as "+
			"%s/%s. The permissions checked below would be somebody else's",
			subj.Namespace, subj.Name, deploy.Namespace, got)
	}

	return "system:serviceaccount:" + subj.Namespace + ":" + subj.Name
}

// theOperatorCheckedItsOwnPermissions reads the operator's verdict on itself:
// that rbacaudit.Checker is wired in, runs, and reports. Unlike
// theOperatorWasNeverDenied it also covers cached reads.
func theOperatorCheckedItsOwnPermissions(t *testing.T) {
	log, _ := operatorLog(t, operatorNamespace)

	if strings.Contains(log, "is missing permissions it needs") {
		t.Errorf("the operator reports permissions it does not have. Its own lines say which:\n%s",
			strings.Join(linesContaining(log, "is missing permissions it needs"), "\n"))
	}

	granted := linesContaining(log, "every permission the operator needs is granted")
	if len(granted) < len(rbacaudit.DefaultScopes(operatorNamespace)) {
		t.Errorf("the operator reported on %d permission scopes, want %d. Either the "+
			"self-check is not wired in or it did not finish; without it a permission "+
			"revoked while the operator runs is invisible to everything here",
			len(granted), len(rbacaudit.DefaultScopes(operatorNamespace)))
	}
}

func linesContaining(log, want string) []string {
	var found []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, want) {
			found = append(found, line)
		}
	}
	return found
}
