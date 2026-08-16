package controller

import (
	"time"

	"github.com/spawnery/spawnery/internal/phase"
)

// The backoff's four numbers. They are constants rather than CRD fields
// because the master design does not ask for configurability, nobody has
// asked for it, and a knob nobody turns is a knob somebody turns wrongly.
// Adding a field later is cheap; removing one is not.
const (
	// backoffBase is the wait after the first failure. Short enough that a
	// single unlucky start costs the group seconds, not minutes.
	backoffBase = 10 * time.Second
	// backoffFactor is how much each further failure multiplies the wait.
	backoffFactor = 2
	// backoffCap bounds the doubling. It is NOT reached at backoffGiveUpAt:
	// the largest wait before the group gives up is 160s. It exists so that
	// raising backoffGiveUpAt — the one of these numbers somebody might
	// plausibly want larger — cannot produce an unbounded wait.
	backoffCap = 5 * time.Minute
	// backoffGiveUpAt is how many consecutive failures end the attempts. Six
	// gives one free attempt and five retries over roughly five minutes of
	// waiting; against a container that takes about ninety seconds to exhaust
	// its restarts, the whole run is about fourteen minutes. Long enough to
	// ride out a transient cluster problem, short enough not to spend an hour
	// confirming a broken image.
	backoffGiveUpAt int32 = 6
)

// CountFailures folds this pass's views into the group's running count of
// consecutive failures, and returns the newest failure timestamp it counted.
//
// A failure counts once, identified by its own status.failedAt being newer
// than the newest one already counted. That test is what makes the count
// idempotent: without it a five-second resync would re-count the same corpse
// forever. The window also runs from failedAt rather than from now, because
// stamping the observation would extend the window on every pass and the
// backoff would never expire.
//
// The streak breaks on a success *since* the last counted failure, not on any
// server being Ready. The weaker rule reads well and is wrong: a group with
// one healthy server and one that crash-loops — a bad node, a resource request
// only some nodes satisfy — would hold its count at zero forever and hammer
// indefinitely. A Failed server carries no ReadySince (the Server controller
// clears it on the way out of Ready), so a corpse can never look like the
// success that ends its own streak.
//
// requiredOrdinals selects which group that last rule is stated for. Zero
// means ephemeral, where servers are interchangeable and any success ends the
// streak. Above zero it is spec.replicas of a persistent group, where each
// ordinal owns a world of its own: the streak breaks only when every required
// ordinal has a ready server, so a healthy sibling can no longer stand in for
// a broken one and clear its count.
func CountFailures(views []ServerView, prev int32, since time.Time, requiredOrdinals int32) (int32, time.Time) {
	var lastSuccess time.Time
	if requiredOrdinals == 0 {
		for _, v := range views {
			if v.ReadySince.After(lastSuccess) {
				lastSuccess = v.ReadySince
			}
		}
	} else {
		// Each ordinal's own newest ReadySince, so a flapping sibling's older
		// blips can't stand in for a more recent one.
		ready := make(map[int32]time.Time, len(views))
		for _, v := range views {
			if v.Ordinal == nil || v.Phase != phase.Ready {
				continue
			}
			if cur, ok := ready[*v.Ordinal]; !ok || v.ReadySince.After(cur) {
				ready[*v.Ordinal] = v.ReadySince
			}
		}
		// The group recovered only once every required ordinal has, so the
		// earliest of them is what the streak breaks on -- an ordinal missing
		// from the map pulls this to the zero time, same as one still broken.
		for ordinal := int32(0); ordinal < requiredOrdinals; ordinal++ {
			at, ok := ready[ordinal]
			if !ok {
				lastSuccess = time.Time{}
				break
			}
			if lastSuccess.IsZero() || at.Before(lastSuccess) {
				lastSuccess = at
			}
		}
	}

	count, from := prev, since
	if lastSuccess.After(since) {
		// Failures older than that success belong to the streak it ended, not
		// to a new one, so the count restarts and only failures after it are
		// counted below.
		count, from = 0, lastSuccess
	}

	newest := since
	for _, v := range views {
		if v.Phase != phase.Failed || !v.FailedAt.After(from) {
			continue
		}
		count++
		if v.FailedAt.After(newest) {
			newest = v.FailedAt
		}
	}
	return count, newest
}

// BackoffInputs is what the retry decision needs.
type BackoffInputs struct {
	// ConsecutiveFailures is the count CountFailures produced.
	ConsecutiveFailures int32
	// LastFailureAt is the newest counted failure. The window runs from here.
	LastFailureAt time.Time
	// Now is the reconciler's clock.
	Now time.Time
}

// BackoffDecision is what the group may do about creating this pass.
type BackoffDecision struct {
	// MayCreate is false while a window is open and false once the group has
	// given up. Deletions, retirements and drains are never gated by it.
	MayCreate bool
	// GaveUp is true past the threshold. Nothing is created until the group's
	// spec changes.
	GaveUp bool
	// RetryAfter is how long until the window closes, for the condition's
	// message. Zero when MayCreate or GaveUp.
	RetryAfter time.Duration
}

// DecideBackoff turns the count into permission to create.
func DecideBackoff(in BackoffInputs) BackoffDecision {
	if in.ConsecutiveFailures >= backoffGiveUpAt {
		// Terminal until the spec changes. An elapsed window must not
		// resurrect it, which is why this is tested before the window below.
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

// backoffDelay is the window after n consecutive failures.
//
// Multiplied in a loop with the cap checked each time rather than computed as
// base * factor^(n-1): a large n would overflow the exponent long before it
// reached anything meaningful, and the cap makes every step past it identical
// anyway.
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
