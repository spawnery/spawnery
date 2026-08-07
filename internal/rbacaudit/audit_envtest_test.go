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

// targetNamespace is where namespaced permissions are checked. The binding is
// cluster-wide, so any namespace answers the same; using the one from the
// sample manifest keeps the failure messages recognisable.
const targetNamespace = "minecraft"

// applyDeploymentAndDeriveSubject applies the deployment manifests and returns
// the subject the operator actually runs as, derived from the manifests rather
// than restated. That way a binding naming the wrong role, or a deployment
// naming the wrong ServiceAccount, shows up as denied permissions instead of
// passing silently.
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
