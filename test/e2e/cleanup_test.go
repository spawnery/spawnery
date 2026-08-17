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
func theFinalizerIsReleased(t *testing.T) {
	servers := serversInGroup(t, "lobby")
	if len(servers) == 0 {
		t.Fatal("no Servers in the lobby group to delete")
	}
	victim := servers[0]

	if err := k8s.Delete(ctx, &victim); err != nil {
		t.Fatalf("delete Server %s: %v", victim.Name, err)
	}

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
// The second is indirect and worth naming. config/deploy/deployment.yaml
// carries --startup-deadline=5m and hack/e2e.sh appends a second occurrence of
// the flag rather than rewriting the list. If Go's flag package did not resolve
// a repeated flag to the last one, nothing would fail loudly -- this test would
// simply time out. That makes this the only place the append is checked.
func theStartupDeadlineFailsAServerAndClearsIt(t *testing.T) {
	eventually(t, 3*time.Minute, "a Server to reach phase Failed", func() (bool, string) {
		var seen []string
		for _, s := range serversInGroup(t, "lobby") {
			if s.Status.Phase == "Failed" {
				return true, ""
			}
			seen = append(seen, fmt.Sprintf("%s=%s", s.Name, s.Status.Phase))
		}
		return false, fmt.Sprintf("phases: %v", seen)
	})

	eventually(t, 3*time.Minute, "the failed Server's corpse to be cleared", func() (bool, string) {
		for _, s := range serversInGroup(t, "lobby") {
			if s.Status.Phase == "Failed" {
				age := time.Since(s.Status.FailedAt.Time)
				return false, fmt.Sprintf("%s still Failed, %s old", s.Name, age.Round(time.Second))
			}
		}
		return true, ""
	})
}
