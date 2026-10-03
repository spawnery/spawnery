//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// theTestManifestIsAccepted applies the run's own manifest and waits for the
// group to build what it asks for.
//
// It also dry-runs config/samples/network.yaml, which names real images that
// would pull 724 MB. The sample goes first: its objects collide by name with
// the run's own, and applyManifest tolerates AlreadyExists.
func theTestManifestIsAccepted(t *testing.T) {
	applyManifest(t, "config/samples/network.yaml", client.DryRunAll)
	applyManifest(t, "test/e2e/manifests/e2e.yaml")

	eventually(t, 2*time.Minute, "the lobby group's two Servers", func() (bool, string) {
		servers := nonFailedServersInGroup(t, "lobby")
		return len(servers) == 2, fmt.Sprintf("%d non-Failed Servers", len(servers))
	})

	eventually(t, 2*time.Minute, "a pod for each live Server", func() (bool, string) {
		return podsCoverLiveServers(t, "lobby")
	})

	// The pods stay in ErrImagePull on purpose. Reading the pod spec rather
	// than container states keeps this from passing before any container
	// could have started.
	assertPodsUseTheUnresolvableImage(t, "lobby", unresolvableServerImage)
}

// unresolvableServerImage restates test/e2e/manifests/e2e.yaml; reading it
// back would compare the manifest against itself.
const unresolvableServerImage = "ghcr.io/spawnery/paper:e2e-no-such-tag"

// assertPodsUseTheUnresolvableImage checks that every container of every pod of
// one group asks for want, and that there was at least one pod to check.
func assertPodsUseTheUnresolvableImage(t *testing.T, group, want string) {
	t.Helper()

	var pods corev1.PodList
	if err := k8s.List(ctx, &pods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{podspec.LabelGroup: group}); err != nil {
		t.Fatalf("list %s pods: %v", group, err)
	}
	if len(pods.Items) == 0 {
		t.Fatalf("no pods in group %s to check the image of", group)
	}
	for _, p := range pods.Items {
		for _, c := range p.Spec.Containers {
			if c.Image != want {
				t.Errorf("pod %s container %s asks for image %q, want %q. Milestone 6a "+
					"loads no game image and test/e2e/manifests/e2e.yaml names an "+
					"unresolvable one on purpose (spec §7.4); a resolvable reference here "+
					"means the manifest was pointed at a real tag, and every run of this "+
					"suite would download 724 MB and start real servers",
					p.Name, c.Name, c.Image, want)
			}
		}
	}
}

func theGroupScalesUp(t *testing.T) {
	patchMinReplicas(t, "lobby", 3)
	eventually(t, 2*time.Minute, "a third Server", func() (bool, string) {
		servers := nonFailedServersInGroup(t, "lobby")
		return len(servers) == 3, fmt.Sprintf("%d non-Failed Servers", len(servers))
	})
}

// theCeilingShedsSurplus lowers the group's ceiling below its live count and
// waits for the surplus to go -- and to stay gone, not merely to have been
// observed once.
//
// Lowering minReplicas cannot show it: demand removal needs an agent to have
// reported the server empty, and no agent connects to these unresolvable
// images. The floor comes down in the same patch only to keep the CRD's
// min <= max rule.
//
// Servers here also fail on --startup-deadline, which would bring the count
// down too. So the wait requires one of the original Servers to be deleted
// outright: a Failed one stays for failedRetentionSeconds, longer than this
// window.
func theCeilingShedsSurplus(t *testing.T) {
	before := make(map[string]bool)
	for _, s := range nonFailedServersInGroup(t, "lobby") {
		before[s.Name] = true
	}
	if len(before) != 3 {
		t.Fatalf("the lobby group has %d live Servers, want the 3 scenario 3 left; "+
			"this scenario is about which one of them the ceiling sheds", len(before))
	}

	patchScalingBounds(t, "lobby", 2, 2)
	eventuallyStable(t, 15*time.Second, 3*time.Second,
		"the surplus Server to be deleted, and stay gone", func() (bool, string) {
			all := serversInGroup(t, "lobby")
			still := make(map[string]bool, len(all))
			live := 0
			for _, s := range all {
				still[s.Name] = true
				if s.Status.Phase != string(phase.Failed) {
					live++
				}
			}
			deleted := 0
			for name := range before {
				if !still[name] {
					deleted++
				}
			}
			return live == 2 && deleted >= 1,
				fmt.Sprintf("%d non-Failed Servers, %d of the 3 originals deleted outright",
					live, deleted)
		})
}

// serversInGroup lists every Server of one group in the test namespace,
// whatever its phase, Failed ones included.
func serversInGroup(t *testing.T, group string) []spawneryv1alpha1.Server {
	t.Helper()
	var list spawneryv1alpha1.ServerList
	if err := k8s.List(ctx, &list,
		client.InNamespace(testNamespace),
		client.MatchingLabels{podspec.LabelGroup: group}); err != nil {
		t.Fatalf("list Servers of group %s: %v", group, err)
	}
	return list.Items
}

// nonFailedServersInGroup lists a group's Servers, excluding any in phase
// Failed.
//
// No Server here ever becomes Ready, so the group churns all run: Failed at
// the startup deadline, pruned at failedRetentionSeconds, replaced. A Failed
// Server is not capacity.
func nonFailedServersInGroup(t *testing.T, group string) []spawneryv1alpha1.Server {
	t.Helper()
	all := serversInGroup(t, group)
	live := make([]spawneryv1alpha1.Server, 0, len(all))
	for _, s := range all {
		if s.Status.Phase != string(phase.Failed) {
			live = append(live, s)
		}
	}
	return live
}

// podsCoverLiveServers reports whether every non-Failed Server of a group has
// a pod, checked by name against the live set rather than by counting pod
// objects.
//
// A Failed Server keeps its pod for failedRetentionSeconds, so a pod count
// would oscillate during churn.
func podsCoverLiveServers(t *testing.T, group string) (bool, string) {
	t.Helper()
	servers := nonFailedServersInGroup(t, group)

	var pods corev1.PodList
	if err := k8s.List(ctx, &pods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{podspec.LabelGroup: group}); err != nil {
		return false, err.Error()
	}
	havePod := make(map[string]bool, len(pods.Items))
	for _, p := range pods.Items {
		havePod[p.Labels[podspec.LabelServer]] = true
	}

	missing := 0
	for _, s := range servers {
		if !havePod[s.Name] {
			missing++
		}
	}
	return missing == 0, fmt.Sprintf("%d of %d live Servers missing a pod (%d pods total in group)",
		missing, len(servers), len(pods.Items))
}

// patchScaling edits one group's scaling block through a single atomic patch,
// so min <= max holds at every step.
func patchScaling(t *testing.T, group string, mutate func(*spawneryv1alpha1.ScalingSpec)) {
	t.Helper()
	var g spawneryv1alpha1.ServerGroup
	key := client.ObjectKey{Namespace: testNamespace, Name: group}
	if err := k8s.Get(ctx, key, &g); err != nil {
		t.Fatalf("get ServerGroup %s: %v", group, err)
	}
	patch := client.MergeFrom(g.DeepCopy())
	if g.Spec.Scaling == nil {
		t.Fatalf("ServerGroup %s has no scaling block; this test edits it", group)
	}
	mutate(g.Spec.Scaling)
	if err := k8s.Patch(ctx, &g, patch); err != nil {
		t.Fatalf("patch ServerGroup %s scaling: %v", group, err)
	}
}

func patchMinReplicas(t *testing.T, group string, n int32) {
	t.Helper()
	patchScaling(t, group, func(s *spawneryv1alpha1.ScalingSpec) { s.MinReplicas = n })
}

func patchScalingBounds(t *testing.T, group string, min, max int32) {
	t.Helper()
	patchScaling(t, group, func(s *spawneryv1alpha1.ScalingSpec) {
		s.MinReplicas = min
		s.MaxReplicas = max
	})
}
