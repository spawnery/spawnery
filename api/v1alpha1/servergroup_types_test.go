package v1alpha1

import (
	"testing"
	"time"

	"k8s.io/utils/ptr"
)

func TestFinishedRetentionIsSecondsNotTheFailedOne(t *testing.T) {
	g := &ServerGroup{}
	g.Spec.FailedRetentionSeconds = 3600
	g.Spec.FinishedRetentionSeconds = 300

	if got, want := g.FinishedRetention(), 5*time.Minute; got != want {
		t.Errorf("FinishedRetention() = %v, want %v", got, want)
	}
	if got, want := g.FailedRetention(), time.Hour; got != want {
		t.Errorf("FailedRetention() = %v, want %v", got, want)
	}
}

func TestDesiredReplicasIsZeroForOnDemand(t *testing.T) {
	g := &ServerGroup{
		Spec: ServerGroupSpec{Type: ServerGroupOnDemand, Replicas: ptr.To[int32](3)},
	}
	if got := g.DesiredReplicas(); got != 0 {
		t.Fatalf("DesiredReplicas() = %d, want 0", got)
	}
	if !g.IsOnDemand() {
		t.Fatal("IsOnDemand() = false for a group of type OnDemand")
	}
	if g.IsEphemeral() {
		t.Fatal("IsEphemeral() = true for a group of type OnDemand")
	}
}
