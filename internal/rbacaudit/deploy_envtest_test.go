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
	"errors"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// readManifest decodes a single-document YAML manifest from the repository.
// Strictly: sigs.k8s.io/yaml's plain Unmarshal drops keys the target type does
// not have, so `serviceAccountNam:` or `readOnlyRootFilesytem:` would decode
// into a zero value and every assertion below would then be checking a field
// nobody set.
func readManifest[T any](t *testing.T, rel string, into *T) {
	t.Helper()
	raw, err := os.ReadFile(testenv.RepoPath(t, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if err := yaml.UnmarshalStrict(raw, into); err != nil {
		t.Fatalf("decode %s: %v", rel, err)
	}
}

// generatedRoles is the repository-relative path of the manifest controller-gen
// writes. It holds every role the markers ask for, in one multi-document file:
// the cluster-wide ClusterRole and the Role scoped to the operator's own
// namespace. Nothing in the build splits them apart, so readManifest — which
// decodes a single document — cannot be used on it.
const generatedRoles = "config/rbac/role.yaml"

// readMultiDocManifest splits a multi-document YAML manifest at rel and hands
// each document's Kind and raw bytes to decode. readManifest cannot be reused
// here because it decodes exactly one document; this is the loop both
// readGeneratedRoles and readForwardingSecretReader (audit_envtest_test.go)
// need, factored out so neither restates it. decode owns everything
// kind-specific — including its own "second one of this kind" refusal and its
// own diagnostics — this helper only owns finding the file, splitting it, and
// skipping blank documents.
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

// readGeneratedRoles decodes both halves of the generated RBAC manifest. It
// insists on finding exactly one of each.
//
// A missing Role means a namespace= qualifier fell off a marker and the operator
// would hold its Secret and Lease rights everywhere. A *second* Role is the
// quieter failure and the reason this refuses rather than takes the last one:
// returning one of two would leave the other unaudited in both directions while
// every test still reported green. controller-gen emits one Role per namespace
// named in a marker, so a second one arrives the moment a second namespace does —
// which the Helm chart makes likely.
func readGeneratedRoles(t *testing.T) (*rbacv1.ClusterRole, *rbacv1.Role) {
	t.Helper()

	var cluster *rbacv1.ClusterRole
	var namespaced *rbacv1.Role
	readMultiDocManifest(t, generatedRoles, func(kind string, doc []byte) {
		switch kind {
		case "ClusterRole":
			decoded := &rbacv1.ClusterRole{}
			if err := yaml.Unmarshal(doc, decoded); err != nil {
				t.Fatalf("decode the ClusterRole in %s: %v", generatedRoles, err)
			}
			if cluster != nil {
				t.Fatalf("%s contains more than one ClusterRole (%q and %q). This audit "+
					"compares exactly one against rbacaudit.RequiredCluster; taking the "+
					"last would leave the other unchecked in both directions without "+
					"saying so. Teach it to handle several before adding one",
					generatedRoles, cluster.Name, decoded.Name)
			}
			cluster = decoded
		case "Role":
			decoded := &rbacv1.Role{}
			if err := yaml.Unmarshal(doc, decoded); err != nil {
				t.Fatalf("decode the Role in %s: %v", generatedRoles, err)
			}
			if namespaced != nil {
				t.Fatalf("%s contains more than one Role (%s/%s and %s/%s). This audit "+
					"compares exactly one against rbacaudit.RequiredNamespaced; taking "+
					"the last would leave the other unchecked in both directions without "+
					"saying so. A second namespace= qualifier needs the namespaced table "+
					"split per namespace first",
					generatedRoles, namespaced.Namespace, namespaced.Name,
					decoded.Namespace, decoded.Name)
			}
			namespaced = decoded
		default:
			t.Fatalf("%s contains an unexpected %s; this audit only models roles", generatedRoles, kind)
		}
	})
	if cluster == nil {
		t.Fatalf("%s contains no ClusterRole", generatedRoles)
	}
	if namespaced == nil {
		t.Fatalf("%s contains no Role — a namespace= qualifier fell off a marker, and "+
			"the operator would hold its Secret and Lease rights in every namespace",
			generatedRoles)
	}
	return cluster, namespaced
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
	var binding rbacv1.ClusterRoleBinding
	var deploy appsv1.Deployment

	readManifest(t, "config/deploy/namespace.yaml", &ns)
	readManifest(t, "config/deploy/serviceaccount.yaml", &sa)
	readManifest(t, "config/deploy/clusterrolebinding.yaml", &binding)
	readManifest(t, "config/deploy/deployment.yaml", &deploy)
	role, _ := readGeneratedRoles(t)

	apply(t, &ns, &sa, role, &binding, &deploy)

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

// TestTheRoleBindingBindsTheOperatorInItsOwnNamespace is the namespaced half of
// TestDeployManifestsAreAcceptedAndConsistent. Nothing else reads
// rolebinding.yaml, so a RoleRef naming a ClusterRole, a subject naming the
// wrong ServiceAccount, or a binding sitting in a namespace other than the one
// controller-gen put the Role in would all leave certs.Store.Ensure with
// Forbidden on the operator's own Secret while every file on its own still
// looked plausible.
func TestTheRoleBindingBindsTheOperatorInItsOwnNamespace(t *testing.T) {
	var ns corev1.Namespace
	var sa corev1.ServiceAccount
	var binding rbacv1.RoleBinding

	readManifest(t, "config/deploy/namespace.yaml", &ns)
	readManifest(t, "config/deploy/serviceaccount.yaml", &sa)
	readManifest(t, "config/deploy/rolebinding.yaml", &binding)
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
	var ns corev1.Namespace
	var svc corev1.Service
	var deploy appsv1.Deployment
	readManifest(t, "config/deploy/namespace.yaml", &ns)
	readManifest(t, "config/deploy/service.yaml", &svc)
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

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
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

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
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

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
	readManifest(t, "config/deploy/networkpolicy.yaml", &policy)
	readManifest(t, "config/deploy/deployment.yaml", &deploy)

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
	// egress rules makes the operator pod default-deny for egress, and it
	// cannot reach the API server at all — every controller stops, at once,
	// from a one-word manifest edit.
	if len(policy.Spec.PolicyTypes) != 1 ||
		policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes = %v, want [Ingress] alone — this policy has no "+
			"egress rules, so declaring Egress default-denies the operator's own "+
			"outbound traffic and it cannot reach the API server",
			policy.Spec.PolicyTypes)
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
	admitted := map[int]bool{}
	for _, p := range probeRule.Ports {
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
