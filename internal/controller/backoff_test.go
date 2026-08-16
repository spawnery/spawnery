package controller

import (
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
	"k8s.io/utils/ptr"
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

	got, newest := CountFailures(views, 0, time.Time{}, 0)
	if got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
	if !newest.Equal(base) {
		t.Errorf("newest = %v, want %v", newest, base)
	}

	// The same corpse on the next pass. Without the FailedAt > since test this
	// would climb by one every five-second resync forever, which is the whole
	// reason a counter can survive at all.
	got, _ = CountFailures(views, got, newest, 0)
	if got != 1 {
		t.Errorf("count = %d after re-observing the same corpse, want 1", got)
	}
}

func TestCountFailuresCountsTwoInOnePass(t *testing.T) {
	base := time.Now()
	views := []ServerView{failedAt("a", base), failedAt("b", base.Add(time.Second))}

	got, newest := CountFailures(views, 0, time.Time{}, 0)
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

	got, _ := CountFailures(views, 3, base, 0)
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

	got, _ := CountFailures(views, 3, base, 0)
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

	got, newest := CountFailures(views, 3, base, 0)
	if got != 1 {
		t.Errorf("count = %d, want 1: the success ended the old streak and the later failure begins a new one", got)
	}
	if !newest.Equal(base.Add(2 * time.Minute)) {
		t.Error("newest is not the failure that started the new streak")
	}
}

// TestCountFailuresTakesASuccessFromAnyPhaseAndWhyThatIsSafe writes down the
// half of CountFailures' stated safety property that lives in another file.
//
// backoff.go says: "A Failed server carries no ReadySince (the Server
// controller clears it on the way out of Ready), so a corpse can never look
// like the success that ends its own streak." That is load-bearing — without
// it a corpse whose own readySince post-dates the watermark would reset the
// streak it belongs to, and the count could never climb past 1 for the
// Ready -> Failed class — and until now nothing checked it: the whole-branch
// review mutated `case phase.Failed: srv.Status.ReadySince = nil` out of
// server_controller.go and the entire suite stayed green, because every
// fixture reaches Failed through Starting, which clears it anyway.
//
// This test pins the *dependency*, and does so honestly: CountFailures' search
// for the newest success reads ReadySince off every view regardless of phase,
// so handed a Failed view carrying one it resets the streak. That is the
// behaviour, and asserting it is what makes the invariant's location explicit
// — the guarantee is not in this function, it is upstream, in the Server
// controller's clearing of readySince on entry to Failed. Its direct pin is
// TestServerFailedStraightFromReadyClearsReadySince in
// server_controller_test.go, over the one transition that reaches Failed
// without passing through Starting; a reader who breaks that one should land
// here to see what it costs.
func TestCountFailuresTakesASuccessFromAnyPhaseAndWhyThatIsSafe(t *testing.T) {
	base := time.Now()
	// The state the Server controller must never produce: Failed, and carrying
	// a readySince newer than the watermark the streak is counted from.
	corpse := ServerView{
		Name:       "broken",
		Phase:      phase.Failed,
		FailedAt:   base.Add(2 * time.Second),
		ReadySince: base.Add(time.Second),
	}

	got, _ := CountFailures([]ServerView{corpse}, 3, base, 0)
	if got != 1 {
		t.Errorf("count = %d, want 1: CountFailures reads readySince off every view whatever its "+
			"phase, so a corpse carrying one ends its own streak and the new failure starts a fresh one", got)
	}

	// The same corpse as the Server controller actually stamps it — readySince
	// cleared on the way into Failed — continues the streak, which is what the
	// group's six-failure budget depends on.
	corpse.ReadySince = time.Time{}
	if got, _ := CountFailures([]ServerView{corpse}, 3, base, 0); got != 4 {
		t.Errorf("count = %d, want 4: with readySince cleared the corpse is a failure and nothing else", got)
	}
}

// A broken ordinal must reach the give-up threshold however often a healthy
// sibling flaps. The old rule took the maximum ReadySince across all views, so
// a neighbour that regained readiness faster than failures arrived reset the
// count more often than it incremented and six was never reached.
//
// One ordinal cannot show this, which is why the existing
// TestAPersistentGroupSaysItIsBackingOffAndThenGivesUp could not.
func TestAFlappingSiblingDoesNotClearABrokenOrdinalsStreak(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	count := int32(0)
	since := time.Time{}
	for i := 0; i < 6; i++ {
		failedAt := base.Add(time.Duration(i) * time.Hour)
		// g-1 blips ready halfway through every interval -- more often than
		// g-0 fails, which is the rate comparison that broke the old rule.
		siblingReady := failedAt.Add(30 * time.Minute)

		views := []ServerView{
			{Name: "g-0", Ordinal: ptr.To(int32(0)), Phase: phase.Failed, FailedAt: failedAt},
			{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Ready, ReadySince: siblingReady},
		}
		count, since = CountFailures(views, count, since, 2)
	}

	if count < 6 {
		t.Fatalf("counted %d failures; a flapping sibling is still clearing the streak", count)
	}
}

// The ephemeral rule must not move: interchangeable servers are exactly the
// case the maximum is right for.
func TestAnEphemeralGroupKeepsTheMaximumRule(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	views := []ServerView{
		{Name: "a", Phase: phase.Failed, FailedAt: base},
		{Name: "b", Phase: phase.Ready, ReadySince: base.Add(time.Minute)},
	}
	count, _ := CountFailures(views, 3, time.Time{}, 0)
	if count != 0 {
		t.Fatalf("count = %d; a ready sibling must still break an ephemeral streak", count)
	}
}

// The group is not recovered while an ordinal is missing entirely, so the
// streak must not reset then either.
func TestAMissingOrdinalDoesNotCountAsRecovered(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	views := []ServerView{
		{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Ready, ReadySince: base.Add(time.Hour)},
	}
	count, _ := CountFailures(views, 4, base, 2)
	if count != 4 {
		t.Fatalf("count = %d; ordinal 0 has no ready server, so the group has not recovered", count)
	}
}

// TestAGroupHasNotRecoveredUntilItsSlowestRequiredOrdinalHas is what pins the
// minimum itself, as opposed to the missing-ordinal shortcut that produces
// the same zero-time answer above it. Both required ordinals are Ready here,
// so there is no missing entry to fall back on: the choice between the
// earlier ReadySince and the later one is the whole content of "minimum, not
// maximum," and this is the only case in this file where that choice is
// actually made.
//
// g-1 alone would end the streak under the old maximum rule (its ReadySince
// is after the last counted failure); g-0 alone would not (its ReadySince is
// before it). The minimum takes g-0's answer over g-1's, so the streak must
// not reset.
func TestAGroupHasNotRecoveredUntilItsSlowestRequiredOrdinalHas(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	since := base.Add(time.Hour)
	views := []ServerView{
		{Name: "g-0", Ordinal: ptr.To(int32(0)), Phase: phase.Ready, ReadySince: base.Add(30 * time.Minute)},
		{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Ready, ReadySince: base.Add(90 * time.Minute)},
	}

	count, _ := CountFailures(views, 4, since, 2)
	if count != 4 {
		t.Fatalf("count = %d, want 4: g-0's ReadySince predates the last counted failure, and the "+
			"minimum over both required ordinals must be taken from it rather than from g-1's later one", count)
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
