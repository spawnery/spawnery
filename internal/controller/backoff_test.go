package controller

import (
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// failedAt builds a Failed server that failed at t.
func failedAt(name string, t time.Time) ServerView {
	return ServerView{Name: name, Phase: phase.Failed, FailedAt: t}
}

// readyAt builds a Ready server that became ready at t.
func readyAt(name string, t time.Time) ServerView {
	return ServerView{Name: name, Phase: phase.Ready, ReadySince: t, Slots: 100}
}

func TestCountFailuresCountsANewCorpseOnce(t *testing.T) {
	base := time.Now()
	views := []ServerView{failedAt("a", base)}

	got, newest := CountFailures(views, 0, time.Time{})
	if got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
	if !newest.Equal(base) {
		t.Errorf("newest = %v, want %v", newest, base)
	}

	// The same corpse on the next pass. Without the FailedAt > since test this
	// would climb by one every five-second resync forever, which is the whole
	// reason a counter can survive at all.
	got, _ = CountFailures(views, got, newest)
	if got != 1 {
		t.Errorf("count = %d after re-observing the same corpse, want 1", got)
	}
}

func TestCountFailuresCountsTwoInOnePass(t *testing.T) {
	base := time.Now()
	views := []ServerView{failedAt("a", base), failedAt("b", base.Add(time.Second))}

	got, newest := CountFailures(views, 0, time.Time{})
	if got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
	if !newest.Equal(base.Add(time.Second)) {
		t.Error("newest is not the newer of the two failures")
	}
}

func TestCountFailuresResetsOnASuccessAfterTheLastFailure(t *testing.T) {
	base := time.Now()
	// Three failures already counted, then a server came up.
	views := []ServerView{readyAt("b", base.Add(time.Minute))}

	got, _ := CountFailures(views, 3, base)
	if got != 0 {
		t.Errorf("count = %d, want 0: a success since the last failure breaks the streak", got)
	}
}

func TestCountFailuresIgnoresASuccessOlderThanTheLastFailure(t *testing.T) {
	// The rule that a plausible implementation gets wrong. "Any server is
	// Ready" would hold the counter at zero forever for a group with one
	// healthy server and one that crash-loops — a bad node, or a resource
	// request only some nodes satisfy — and the group would hammer
	// indefinitely. The success has to be *since* the failure.
	base := time.Now()
	views := []ServerView{
		readyAt("healthy", base.Add(-time.Hour)),
		failedAt("broken", base.Add(time.Second)),
	}

	got, _ := CountFailures(views, 3, base)
	if got != 4 {
		t.Errorf("count = %d, want 4: the healthy server predates the streak and does not break it", got)
	}
}

func TestCountFailuresStartsAFreshStreakAfterASuccess(t *testing.T) {
	// A success breaks the old streak, and a failure after that success starts
	// a new one at 1 rather than continuing the old count.
	base := time.Now()
	views := []ServerView{
		readyAt("recovered", base.Add(time.Minute)),
		failedAt("next", base.Add(2*time.Minute)),
	}

	got, newest := CountFailures(views, 3, base)
	if got != 1 {
		t.Errorf("count = %d, want 1: the success ended the old streak and the later failure begins a new one", got)
	}
	if !newest.Equal(base.Add(2 * time.Minute)) {
		t.Error("newest is not the failure that started the new streak")
	}
}

func TestDecideBackoffLetsTheFirstAttemptThrough(t *testing.T) {
	got := DecideBackoff(BackoffInputs{ConsecutiveFailures: 0, Now: time.Now()})
	if !got.MayCreate {
		t.Error("MayCreate = false with no failures; the first attempt has no window")
	}
	if got.GaveUp {
		t.Error("GaveUp = true with no failures")
	}
}

func TestDecideBackoffWaitsAndThenAllows(t *testing.T) {
	failed := time.Now()

	// One failure: a ten-second window.
	got := DecideBackoff(BackoffInputs{
		ConsecutiveFailures: 1, LastFailureAt: failed, Now: failed.Add(9 * time.Second),
	})
	if got.MayCreate {
		t.Error("MayCreate = true nine seconds into a ten-second window")
	}
	if got.RetryAfter != time.Second {
		t.Errorf("RetryAfter = %v, want 1s", got.RetryAfter)
	}

	got = DecideBackoff(BackoffInputs{
		ConsecutiveFailures: 1, LastFailureAt: failed, Now: failed.Add(10 * time.Second),
	})
	if !got.MayCreate {
		t.Error("MayCreate = false exactly at the end of the window")
	}
}

func TestDecideBackoffDoubles(t *testing.T) {
	failed := time.Now()
	for _, tc := range []struct {
		failures int32
		want     time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 80 * time.Second},
		{5, 160 * time.Second},
	} {
		got := DecideBackoff(BackoffInputs{
			ConsecutiveFailures: tc.failures, LastFailureAt: failed, Now: failed,
		})
		if got.RetryAfter != tc.want {
			t.Errorf("after %d failures RetryAfter = %v, want %v", tc.failures, got.RetryAfter, tc.want)
		}
	}
}

func TestDecideBackoffGivesUpAtTheThreshold(t *testing.T) {
	failed := time.Now()
	got := DecideBackoff(BackoffInputs{
		ConsecutiveFailures: backoffGiveUpAt, LastFailureAt: failed, Now: failed.Add(time.Hour),
	})
	if !got.GaveUp {
		t.Errorf("GaveUp = false at %d failures", backoffGiveUpAt)
	}
	if got.MayCreate {
		t.Error("MayCreate = true after giving up; an elapsed window must not resurrect it")
	}
}

func TestBackoffDelayIsCapped(t *testing.T) {
	// The cap is not reached at the shipped threshold — the largest delay
	// before giving up is 160s, well under five minutes. It exists so that
	// raising backoffGiveUpAt, the one of these four numbers somebody might
	// plausibly want larger, cannot turn the doubling into an unbounded wait.
	// So this case has to construct a count past the threshold rather than
	// assert the cap against the default, which would never reach it.
	if got := backoffDelay(20); got != backoffCap {
		t.Errorf("backoffDelay(20) = %v, want the cap %v", got, backoffCap)
	}
}
