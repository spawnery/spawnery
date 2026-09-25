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

func TestUpdateStrategyAndFloorAccessors(t *testing.T) {
	cases := []struct {
		name      string
		update    *UpdateSpec
		whenEmpty bool
		floor     int32
	}{
		{"no update policy", nil, false, 0},
		{"rolling update without a floor", &UpdateSpec{Strategy: UpdateRollingUpdate}, false, 0},
		{"when empty", &UpdateSpec{Strategy: UpdateWhenEmpty}, true, 0},
		{"a floor", &UpdateSpec{MinAvailable: ptr.To[int32](3)}, false, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &ServerGroup{Spec: ServerGroupSpec{Update: tc.update}}
			if got := g.UpdateWhenEmpty(); got != tc.whenEmpty {
				t.Errorf("UpdateWhenEmpty() = %v, want %v", got, tc.whenEmpty)
			}
			if got := g.UpdateMinAvailable(); got != tc.floor {
				t.Errorf("UpdateMinAvailable() = %d, want %d", got, tc.floor)
			}
		})
	}
}
