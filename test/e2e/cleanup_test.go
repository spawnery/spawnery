//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// theOrphanSweepRemovesAStrayPod plants a pod that carries the managed labels
// but belongs to no Server object, and waits for the sweep to take it.
//
// This is the one scenario that checks a code path nothing else in the run
// reaches: every other pod here was created by the operator itself.
func theOrphanSweepRemovesAStrayPod(t *testing.T) {
	orphan := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "e2e-orphan",
			Namespace: testNamespace,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelNetwork:   "production",
				podspec.LabelGroup:     "lobby",
				podspec.LabelServer:    "e2e-orphan",
				// Sweep dispatches on this label (orphan.go's "branch on the
				// role explicitly"); without it the List still finds the pod
				// by managed-by, but the switch matches neither RoleServer
				// nor RoleProxy and sweepServerPod is never called. Verified
				// the hard way: the first e2e run of this test timed out with
				// "still there, with no deletion timestamp" even though
				// Sweep's Delete was never touched -- the pod was invisible
				// to the switch, not resistant to the sweep.
				podspec.LabelRole: podspec.RoleServer,
			},
		},
		Spec: corev1.PodSpec{
			// Never pulled: the sweep deletes it long before the kubelet
			// gives up, and a real image would cost this run a download.
			Containers: []corev1.Container{{
				Name:  "orphan",
				Image: "ghcr.io/spawnery/paper:e2e-no-such-tag",
			}},
		},
	}
	if err := k8s.Create(ctx, orphan); err != nil {
		t.Fatalf("create the orphan pod: %v", err)
	}

	eventually(t, 2*time.Minute, "the orphan sweep to delete e2e-orphan", func() (bool, string) {
		var got corev1.Pod
		err := k8s.Get(ctx, client.ObjectKeyFromObject(orphan), &got)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		if !got.DeletionTimestamp.IsZero() {
			return true, ""
		}
		return false, "still there, with no deletion timestamp"
	})
}

// theFinalizerIsReleased deletes a Server by hand and waits for the object to
// go. The Server carries a finalizer, so the object survives its own deletion
// until the controller has taken the pod down and released it -- a stuck
// finalizer is invisible from a diff and shows up only as an object that never
// disappears.
//
// The pick-and-delete step is itself retried, not a single snapshot followed
// by a Fatal. This harness's churn (created, failed at twenty seconds, corpse
// held thirty, pruned, replaced) can empty the group's live list between one
// poll and the next, or retire the very Server just listed before the Delete
// call reaches it -- a NotFound there would read as a test bug rather than as
// the churn this package exists to tolerate everywhere else in it (see
// nonFailedServersInGroup and podsCoverLiveServers for the same discipline).
// eventually keeps listing and attempting the delete until one succeeds, so a
// momentarily empty list or a victim pruned mid-flight is absorbed instead of
// failing the whole scenario on a single unlucky read.
//
// It picks from nonFailedServersInGroup rather than serversInGroup on
// purpose. serversInGroup's Failed entries are corpses already mid-prune, on
// their own countdown to disappearing on failedRetentionSeconds' clock rather
// than on any action of this test's -- deleting one would not distinguish
// "the finalizer-release branch did it" from "the retention pruner would have
// removed it a few seconds later regardless." A live Server's disappearance
// has exactly one explanation: the Delete this function issued, and the
// finalizer-release branch in server_controller.go that has to run for it to
// take effect.
func theFinalizerIsReleased(t *testing.T) {
	var victim spawneryv1alpha1.Server
	eventually(t, 2*time.Minute, "a live Server to pick and delete", func() (bool, string) {
		servers := nonFailedServersInGroup(t, "lobby")
		if len(servers) == 0 {
			return false, "no non-Failed Servers in the lobby group"
		}
		v := servers[0]
		if err := k8s.Delete(ctx, &v); err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Sprintf("%s was already gone by delete time (churn)", v.Name)
			}
			return false, err.Error()
		}
		victim = v
		return true, ""
	})

	eventually(t, 2*time.Minute, "Server "+victim.Name+" to disappear", func() (bool, string) {
		var got spawneryv1alpha1.Server
		err := k8s.Get(ctx, client.ObjectKeyFromObject(&victim), &got)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, fmt.Sprintf("still there, finalizers %v, deletionTimestamp %v",
			got.Finalizers, got.DeletionTimestamp)
	})
}

// theStartupDeadlineFailsAServerAndClearsIt is scenario 6, and it proves two
// things at once.
//
// The first is the failure path itself: a server whose image never resolves
// cannot become Ready, so --startup-deadline is what ends the attempt, and
// failedRetentionSeconds: 30 is what clears the corpse afterwards.
//
// The second is indirect and worth naming. charts/spawnery's production
// default is --startup-deadline=5m (values.yaml); hack/e2e.sh overrides it
// with --set operator.startupDeadline=20s for its own run. If that value did
// not reach the container's args, the deadline would stay 5m and this test
// would simply time out waiting on the 3-minute budget below. That makes this
// the only place the override is checked.
func theStartupDeadlineFailsAServerAndClearsIt(t *testing.T) {
	eventually(t, 3*time.Minute, "a Server to reach phase Failed", func() (bool, string) {
		var seen []string
		for _, s := range serversInGroup(t, "lobby") {
			if s.Status.Phase == string(phase.Failed) {
				return true, ""
			}
			seen = append(seen, fmt.Sprintf("%s=%s", s.Name, s.Status.Phase))
		}
		return false, fmt.Sprintf("phases: %v", seen)
	})

	eventually(t, 3*time.Minute, "the failed Server's corpse to be cleared", func() (bool, string) {
		for _, s := range serversInGroup(t, "lobby") {
			if s.Status.Phase == string(phase.Failed) {
				age := time.Since(s.Status.FailedAt.Time)
				return false, fmt.Sprintf("%s still Failed, %s old", s.Name, age.Round(time.Second))
			}
		}
		return true, ""
	})
}
