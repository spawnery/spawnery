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
				// Sweep dispatches on this label; without it the pod is
				// never swept.
				podspec.LabelRole: podspec.RoleServer,
			},
		},
		Spec: corev1.PodSpec{
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
// go, which it does only once the controller releases its finalizer.
//
// Pick-and-delete is retried, since churn can remove the chosen Server first.
// It picks a non-Failed Server, because a Failed one would be pruned anyway.
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

// theStartupDeadlineFailsAServerAndClearsIt: --startup-deadline fails a server
// whose image never resolves, and failedRetentionSeconds clears it. It also
// checks that hack/e2e.sh's operator.startupDeadline=20s reaches the
// container; the chart default of 5m would outlast the budget below.
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
