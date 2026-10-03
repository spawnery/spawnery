package controller

import (
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// Constants rather than CRD fields: adding a field later is cheap, removing
// one is not.
const (
	backoffBase   = 10 * time.Second
	backoffFactor = 2
	// Not reached at backoffGiveUpAt (the largest wait is 160s); it keeps a
	// raised backoffGiveUpAt from producing an unbounded wait.
	backoffCap = 5 * time.Minute
	// One free attempt and five retries over about five minutes of waiting.
	backoffGiveUpAt int32 = 6
)

// CountFailures counts consecutive failed rounds (not servers) and returns the
// newest failure timestamp counted. The window runs from failedAt, not now, or
// every pass would extend it.
//
// The streak breaks on a success since the last counted failure, not on any
// server being Ready; otherwise one healthy server would mask a crash-looping
// sibling forever. requiredOrdinals is 0 for ephemeral groups (any success
// counts) and spec.replicas for persistent ones (every ordinal must be ready).
func CountFailures(views []ServerView, prev int32, since time.Time, requiredOrdinals int32) (int32, time.Time) {
	var lastSuccess time.Time
	if requiredOrdinals == 0 {
		for _, v := range views {
			if v.ReadySince.After(lastSuccess) {
				lastSuccess = v.ReadySince
			}
		}
	} else {
		ready := make(map[int32]time.Time, len(views))
		for _, v := range views {
			if v.Ordinal == nil || v.Phase != phase.Ready {
				continue
			}
			if cur, ok := ready[*v.Ordinal]; !ok || v.ReadySince.After(cur) {
				ready[*v.Ordinal] = v.ReadySince
			}
		}
		// The latest ReadySince, not the earliest: an ordinal that stayed ready
		// throughout would pin the earliest before the failure.
		for ordinal := int32(0); ordinal < requiredOrdinals; ordinal++ {
			at, ok := ready[ordinal]
			if !ok {
				lastSuccess = time.Time{}
				break
			}
			if at.After(lastSuccess) {
				lastSuccess = at
			}
		}
	}

	count, from := prev, since
	if lastSuccess.After(since) {
		count, from = 0, lastSuccess
	}

	// One per round, not per corpse: size() creates the whole shortfall at once,
	// so counting servers would let one transient failure exhaust a large group.
	newest := since
	sawNewFailure := false
	for _, v := range views {
		if v.Phase != phase.Failed || !v.FailedAt.After(from) {
			continue
		}
		sawNewFailure = true
		if v.FailedAt.After(newest) {
			newest = v.FailedAt
		}
	}
	if sawNewFailure {
		count++
	}
	return count, newest
}

type BackoffInputs struct {
	ConsecutiveFailures int32
	LastFailureAt       time.Time
	Now                 time.Time
}

type BackoffDecision struct {
	// Deletions, retirements and drains are never gated by it.
	MayCreate bool
	// Cleared only when what the servers start with changes (see attemptKey).
	GaveUp     bool
	RetryAfter time.Duration
}

func DecideBackoff(in BackoffInputs) BackoffDecision {
	if in.ConsecutiveFailures >= backoffGiveUpAt {
		// Before the window check, so an elapsed window cannot resurrect it.
		return BackoffDecision{GaveUp: true}
	}
	if in.ConsecutiveFailures == 0 {
		return BackoffDecision{MayCreate: true}
	}
	ready := in.LastFailureAt.Add(backoffDelay(in.ConsecutiveFailures))
	if !in.Now.Before(ready) {
		return BackoffDecision{MayCreate: true}
	}
	return BackoffDecision{RetryAfter: ready.Sub(in.Now)}
}

// A loop rather than base * factor^(n-1), which overflows for large n.
func backoffDelay(n int32) time.Duration {
	d := backoffBase
	for i := int32(1); i < n; i++ {
		d *= backoffFactor
		if d >= backoffCap {
			return backoffCap
		}
	}
	return d
}
