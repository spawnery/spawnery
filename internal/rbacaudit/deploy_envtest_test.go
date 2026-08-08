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

// TestAgentServiceReachesTheOperatorPods checks the one path nothing else
// covers: a game server pod dials spawnery-operator.<ns>.svc:9443, and every
// hop of that address lives in a different file. A selector that matches
// nothing, or a targetPort naming a container port that does not exist, leaves
// the agents with a connection refused and the operator looking healthy.
func TestAgentServiceReachesTheOperatorPods(t *testing.T) {
	var ns corev1.Namespace
	var svc corev1.Service
	var deploy appsv1.Deployment
	readManifest(t, "config/deploy/namespace.yaml", &ns)
	readManifest(t, "config/deploy/service.yaml", &svc)
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

	apply(t, &ns, &svc)

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
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

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
