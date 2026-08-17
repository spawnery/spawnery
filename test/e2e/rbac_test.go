//go:build e2e

package e2e

import (
	"testing"

	authzv1 "k8s.io/api/authorization/v1"

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
	const subject = "system:serviceaccount:" + operatorNamespace + ":spawnery-operator"

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
				subject, p, where, review.Status.Reason, p.Why)
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
