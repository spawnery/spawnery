//go:build e2e

package e2e

import (
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
// SubjectAccessReview and not SelfSubjectAccessReview: the question is about a
// third party's permissions, which lets this test keep its own admin rights and
// still read logs and events.
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
	for _, p := range rbacaudit.RequiredNetworkNamespace {
		check(p, testNamespace)
	}

	// Printed, not merely counted: a loop over an empty slice passes without
	// asking the cluster anything, and PASS would look identical. The Fatalf
	// above turns that specific failure mode into a hard failure rather than
	// relying on a human to notice a zero in a log line nobody reads.
	t.Logf("checked %d cluster, %d namespaced and %d per-network permissions",
		len(rbacaudit.RequiredCluster), len(rbacaudit.RequiredNamespaced),
		len(rbacaudit.RequiredNetworkNamespace))
}

// operatorSubject derives the user name every SubjectAccessReview above asks
// about, from the objects that actually decide it in the cluster: the
// ClusterRoleBinding says which ServiceAccount the operator's ClusterRole is
// bound to, and the Deployment says which ServiceAccount the operator's pod
// runs as.
//
// Design §7.3 asks for it derived rather than restated, and the reason is a
// mutation in §8: point the ClusterRoleBinding at the wrong ServiceAccount. A
// restated literal survives that untouched -- the account it names is still
// allowed everything, so every review comes back allowed and the run stays
// green while the operator in the pod is bound to nothing. Reading the subject
// out of the binding instead makes the reviews ask about whatever the binding
// actually grants, and the cross-check below makes the Deployment agree that
// this is the account the process runs as. Both halves have to be right for
// this subtest to mean what its name says.
//
// This reads the live objects rather than the files under config/. Level A
// (internal/rbacaudit/deploy_envtest_test.go) already checks the manifests
// against one another; what only a cluster can add is that the objects that
// were actually installed say the same thing.
func operatorSubject(t *testing.T) string {
	t.Helper()

	var binding rbacv1.ClusterRoleBinding
	if err := k8s.Get(ctx, client.ObjectKey{Name: "spawnery-operator"}, &binding); err != nil {
		t.Fatalf("get ClusterRoleBinding spawnery-operator: %v", err)
	}
	if binding.RoleRef.Kind != "ClusterRole" {
		t.Fatalf("the binding's roleRef is a %s, not a ClusterRole", binding.RoleRef.Kind)
	}

	// The role it names has to exist. A RoleRef pointing at nothing is legal to
	// create and grants nothing at all, and every review below would come back
	// denied without saying why.
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
