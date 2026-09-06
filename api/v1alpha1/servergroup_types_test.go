package v1alpha1

import (
	"testing"
	"time"
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
