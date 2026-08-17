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
// It also checks that config/samples/network.yaml is accepted, so the example
// cannot rot unnoticed -- with client.DryRunAll, because the sample names the
// real Paper and Velocity images. Since milestone 6a publishes those to a
// public registry, actually creating it would make the kubelet pull 724 MB into
// this node and start real servers, which is the run's declared non-goal.
//
// The order of the two applies is load-bearing and not cosmetic. Every object
// in the sample collides by name with one in the run's own manifest -- both
// describe a Network `production`, a ServerGroup `lobby` and a ProxyGroup
// `gateway` in `minecraft` -- and applyManifest tolerates AlreadyExists. Run
// the other way round, the sample check would pass without the API server
// having validated a single one of its objects.
func theTestManifestIsAccepted(t *testing.T) {
	applyManifest(t, "config/samples/network.yaml", client.DryRunAll)
	applyManifest(t, "test/e2e/manifests/e2e.yaml")

	eventually(t, 2*time.Minute, "the lobby group's two Servers", func() (bool, string) {
		servers := nonFailedServersInGroup(t, "lobby")
		return len(servers) == 2, fmt.Sprintf("%d non-Failed Servers", len(servers))
	})

	eventually(t, 2*time.Minute, "a pod per Server", func() (bool, string) {
		var pods corev1.PodList
		if err := k8s.List(ctx, &pods,
			client.InNamespace(testNamespace),
			client.MatchingLabels{podspec.LabelGroup: "lobby"}); err != nil {
			return false, err.Error()
		}
		return len(pods.Items) == 2, fmt.Sprintf("%d pods", len(pods.Items))
	})

	// The pods stay in ErrImagePull, and that is the expected end state:
	// test/e2e/manifests/e2e.yaml names an unresolvable image on purpose. This
	// assertion is what keeps that a decision -- if somebody points the manifest
	// at a real tag, this fails and says why rather than quietly making every
	// run pull 724 MB.
	var pods corev1.PodList
	if err := k8s.List(ctx, &pods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{podspec.LabelGroup: "lobby"}); err != nil {
		t.Fatalf("list lobby pods: %v", err)
	}
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			if c.State.Running != nil {
				t.Errorf("pod %s is running. Milestone 6a loads no game image and its "+
					"manifest names an unresolvable one on purpose (spec §7.4); a running "+
					"server here means the manifest was pointed at a real tag", p.Name)
			}
		}
	}
}

// theGroupScalesUp raises minReplicas and waits for the operator to build the
// difference.
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
// This does not lower minReplicas to make its point. DecideSize has two ways
// to remove a server, and only one of them is reachable from this harness.
// The minReplicas-driven "demand" removal needs a server that was seen empty
// -- an "empty since" stamp that internal/agent/registry.go writes only when
// a connected agent's report crosses into zero players -- held for
// scaleDownStabilizationSeconds, which defaults to 300s. No agent ever
// connects to a Server whose image never resolves (e2e.yaml's images are
// deliberately unresolvable; see its header), so that stamp is never written
// and that branch is not merely slow here, it is unreachable.
//
// A lowered ceiling reaches a different rule, and the path there is worth
// being precise about: every ServerGroup spec patch, this one included, bumps
// metadata.generation, whatever field actually changed. The moment it does,
// decideSize's coldStart sees every existing Server as belonging to a prior
// generation and insists on building one of the current generation before
// anything may retire -- an unconditional create -- but this patch also drops
// maxReplicas to the group's live count, so there is no room left to grant
// it. A refused cold start with no room is answered the same way an ordinary
// shortfall the ceiling refuses is: by shedding the surplus in the same pass,
// through candidates.go's SelectDeletionCandidates, which excludes a server
// only if it may hold players -- and one that never registered never can.
//
// It cannot lower maxReplicas alone. The CRD's own validation
// (spec.scaling.minReplicas must not exceed spec.scaling.maxReplicas) rejects
// a patch that would leave the ceiling below the floor scenario 2 raised, so
// the floor comes down in the same atomic patch, to the same number the
// ceiling lands on -- purely to keep the object legal. That reopens exactly
// the question this scenario means to answer: with the floor lowered too,
// task-5-report.md's Step 4 found that disabling decideSize's ceiling-driven
// removal does not make this assertion fail -- the group still reaches the
// same count, because every Server here fails its own --startup-deadline in
// the end and the lowered floor never asks for a replacement. What changes
// without the real mechanism is only the time it takes: seconds for an
// active removal (nothing here ever held a player, so the drain that follows
// has nothing to wait for) against ~20s, the harness's fixed
// --startup-deadline, for the fallback. The deadline below is chosen inside
// that gap on purpose -- generous over the few seconds real removal needs,
// short of the 20s attrition floor -- so this assertion fails on its own if
// the fast path ever breaks, rather than passing anyway, slowly, on a
// mechanism this scenario does not mean to exercise.
func theCeilingShedsSurplus(t *testing.T) {
	patchScalingBounds(t, "lobby", 2, 2)
	eventuallyStable(t, 15*time.Second, 3*time.Second,
		"the surplus Server to go, and stay gone", func() (bool, string) {
			servers := nonFailedServersInGroup(t, "lobby")
			return len(servers) == 2, fmt.Sprintf("%d non-Failed Servers", len(servers))
		})
}

// serversInGroup lists every Server of one group in the test namespace,
// whatever its phase -- including Failed corpses still held for diagnosis.
// Task 6's Failed-then-pruned scenario needs to see those; callers that want
// the group's live capacity instead should use nonFailedServersInGroup.
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
// This harness's own manifest names an unresolvable image on purpose (see
// e2e.yaml's header), so no Server it ever builds becomes Ready, and every
// one of them eventually hits --startup-deadline and is marked Failed, then
// pruned on failedRetentionSeconds' own clock. That makes the group churn for
// the whole run -- created, Failed at twenty seconds, pruned around thirty,
// replaced -- for reasons that have nothing to do with any scaling decision
// this package makes. A Failed corpse held for diagnosis is not capacity the
// group is providing, so a scenario measuring capacity counts around it:
// counting it in would let the pruner's own clock masquerade as a scale-down,
// or a real one hide behind a corpse still waiting out its retention.
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

// patchScaling edits one group's scaling block through a single atomic patch.
// Shared by patchMinReplicas and patchScalingBounds so neither ever commits a
// change the CRD's own cross-field validation would reject --
// spec.scaling.minReplicas must never exceed spec.scaling.maxReplicas, not
// even transiently between two separate patches.
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

// patchMinReplicas raises or lowers one group's floor and nothing else.
func patchMinReplicas(t *testing.T, group string, n int32) {
	t.Helper()
	patchScaling(t, group, func(s *spawneryv1alpha1.ScalingSpec) { s.MinReplicas = n })
}

// patchScalingBounds sets a group's floor and ceiling together in one patch.
// theCeilingShedsSurplus is the reason it exists rather than a second call to
// patchMinReplicas: the CRD rejects any single patch that would leave the
// ceiling below whatever floor a previous patch left in place, so lowering a
// ceiling below a floor set earlier must move both fields at once.
func patchScalingBounds(t *testing.T, group string, min, max int32) {
	t.Helper()
	patchScaling(t, group, func(s *spawneryv1alpha1.ScalingSpec) {
		s.MinReplicas = min
		s.MaxReplicas = max
	})
}
