package controller

import (
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/phase"
	"k8s.io/utils/ptr"
)

func failedAt(name string, t time.Time) ServerView {
	return ServerView{Name: name, Phase: phase.Failed, FailedAt: t}
}

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

	// Without the FailedAt > since watermark this would climb on every resync.
	got, _ = CountFailures(views, got, newest, 0)
	if got != 1 {
		t.Errorf("count = %d after re-observing the same corpse, want 1", got)
	}
}

// A transient problem that fails a whole round must not spend the budget per corpse.
func TestCountFailuresCountsOneRoundHoweverManyFailInIt(t *testing.T) {
	base := time.Now()
	views := []ServerView{failedAt("a", base), failedAt("b", base.Add(time.Second))}

	got, newest := CountFailures(views, 0, time.Time{}, 0)
	if got != 1 {
		t.Errorf("count = %d for two servers failing in one round, want 1", got)
	}
	if !newest.Equal(base.Add(time.Second)) {
		t.Error("newest is not the newer of the two failures")
	}

	later := base.Add(time.Minute)
	views = append(views, failedAt("c", later), failedAt("d", later.Add(time.Second)))
	got, _ = CountFailures(views, got, newest, 0)
	if got != 2 {
		t.Errorf("count = %d after a second round of two, want 2", got)
	}
}

func TestCountFailuresResetsOnASuccessAfterTheLastFailure(t *testing.T) {
	base := time.Now()
	views := []ServerView{readyAt("b", base.Add(time.Minute))}

	got, _ := CountFailures(views, 3, base, 0)
	if got != 0 {
		t.Errorf("count = %d, want 0: a success since the last failure breaks the streak", got)
	}
}

func TestCountFailuresIgnoresASuccessOlderThanTheLastFailure(t *testing.T) {
	// "Any server is Ready" would let a group with one crash-looping server retry forever.
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

// The guarantee that a corpse never ends its own streak is upstream: the Server
// controller clears readySince on entry to Failed.
func TestCountFailuresTakesASuccessFromAnyPhaseAndWhyThatIsSafe(t *testing.T) {
	base := time.Now()
	// The state the Server controller must never produce.
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

	// The corpse as the Server controller stamps it, readySince cleared.
	corpse.ReadySince = time.Time{}
	if got, _ := CountFailures([]ServerView{corpse}, 3, base, 0); got != 4 {
		t.Errorf("count = %d, want 4: with readySince cleared the corpse is a failure and nothing else", got)
	}
}

func TestAFlappingSiblingDoesNotClearABrokenOrdinalsStreak(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	count := int32(0)
	since := time.Time{}
	for i := 0; i < 6; i++ {
		failedAt := base.Add(time.Duration(i) * time.Hour)
		// g-1 blips ready more often than g-0 fails.
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

// Interchangeable servers are exactly the case the maximum is right for.
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

// g-0's ReadySince predates the last failure; taking it over g-1's would hold
// the count for as long as g-0 stayed up.
func TestTheGroupRecoveredWhenItsLastRequiredOrdinalDid(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	since := base.Add(time.Hour)
	views := []ServerView{
		{Name: "g-0", Ordinal: ptr.To(int32(0)), Phase: phase.Ready, ReadySince: base.Add(30 * time.Minute)},
		{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Ready, ReadySince: base.Add(90 * time.Minute)},
	}

	count, _ := CountFailures(views, 4, since, 2)
	if count != 0 {
		t.Fatalf("count = %d, want 0: every required ordinal has a ready server and the last of them "+
			"became ready after the last counted failure, so the group has recovered", count)
	}
}

// g-0 stays up throughout, so only the latest ReadySince over the required
// ordinals can break each streak.
func TestRecoveredFailuresDoNotAccumulateAcrossAStableSibling(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	stable := base // g-0 started here and never restarted.

	count := int32(0)
	since := time.Time{}
	for i := 0; i < 6; i++ {
		failedAt := base.Add(time.Duration(i+1) * time.Hour)

		// g-1 holds its ordinal while Failed, so the group is not recovered yet.
		count, since = CountFailures([]ServerView{
			{Name: "g-0", Ordinal: ptr.To(int32(0)), Phase: phase.Ready, ReadySince: stable},
			{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Failed, FailedAt: failedAt},
		}, count, since, 2)
		if count != 1 {
			t.Fatalf("failure %d: count = %d, want 1; each of these is a streak of its own", i+1, count)
		}

		count, since = CountFailures([]ServerView{
			{Name: "g-0", Ordinal: ptr.To(int32(0)), Phase: phase.Ready, ReadySince: stable},
			{Name: "g-1", Ordinal: ptr.To(int32(1)), Phase: phase.Ready, ReadySince: failedAt.Add(10 * time.Minute)},
		}, count, since, 2)
		if count != 0 {
			t.Fatalf("failure %d: count = %d, want 0; both required ordinals are ready and g-1 came "+
				"back after the failure, so the group has recovered", i+1, count)
		}
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
	// The cap is unreachable at the shipped threshold, so the count is built past it.
	if got := backoffDelay(20); got != backoffCap {
		t.Errorf("backoffDelay(20) = %v, want the cap %v", got, backoffCap)
	}
}

// Several unseen corpses in one pass, e.g. after an operator restart, are one round.
func TestCorpsesFirstSeenTogetherAreOneRound(t *testing.T) {
	base := time.Now()
	corpses := []ServerView{
		failedAt("lobby-0", base),
		failedAt("lobby-1", base.Add(time.Second)),
		failedAt("lobby-2", base.Add(2*time.Second)),
		failedAt("lobby-3", base.Add(3*time.Second)),
	}

	got, _ := CountFailures(corpses, 0, time.Time{}, 4)
	if got != 1 {
		t.Errorf("count = %d for four corpses first seen in one pass, want 1", got)
	}
}

func TestAFinishedRoundSpendsNoneOfTheBackoffBudget(t *testing.T) {
	// CountFailures reads the phase, so Finished must stay a phase of its own.
	ended := time.Unix(2000, 0)
	views := []ServerView{
		{Name: "a", Phase: phase.Finished, FailedAt: ended},
		{Name: "b", Phase: phase.Finished, FailedAt: ended},
	}

	count, _ := CountFailures(views, 0, time.Unix(1000, 0), 0)
	if count != 0 {
		t.Errorf("consecutiveFailures = %d, want 0 — finished rounds are not faults", count)
	}
}
