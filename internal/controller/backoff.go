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
// consecutive failed *rounds*, and returns the newest failure timestamp it
// counted.
//
// Rounds rather than servers: see the comment on the counting loop below for
// why, and for what it does not change.
//
// A round counts once, identified by at least one server's status.failedAt
// being newer than the newest one already counted. That test is what makes the count
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
		// Which ordinals have a ready server, and each one's newest ReadySince.
		// Presence is what the gate below reads; the timestamps are what it
		// takes the maximum of.
		ready := make(map[int32]time.Time, len(views))
		for _, v := range views {
			if v.Ordinal == nil || v.Phase != phase.Ready {
				continue
			}
			if cur, ok := ready[*v.Ordinal]; !ok || v.ReadySince.After(cur) {
				ready[*v.Ordinal] = v.ReadySince
			}
		}
		// Two questions with different answers. *Whether* the group recovered:
		// every required ordinal has a ready server, which is what stops a
		// healthy sibling standing in for a broken one. *When* it recovered:
		// the latest of their ReadySince values, because that is when the last
		// of them came back. The earliest would be wrong -- an ordinal that
		// stays ready through the whole episode never advances its ReadySince,
		// so it would pin this to a time before the failure and no recovery
		// could ever break the streak.
		for ordinal := int32(0); ordinal < requiredOrdinals; ordinal++ {
			at, ok := ready[ordinal]
			if !ok {
				// Broken, being rebuilt, or not created yet: not recovered.
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
		// Failures older than that success belong to the streak it ended, not
		// to a new one, so the count restarts and only failures after it are
		// counted below.
		count, from = 0, lastSuccess
	}

	// One per *round*, not one per corpse, and that is the whole of what
	// milestone 4d left undecided.
	//
	// Counting servers spent the budget in ceil(backoffGiveUpAt / floor)
	// rounds, because size() creates the whole shortfall in one pass: three
	// attempts at minReplicas 2, two at 3, and exactly one at 6 or above. A
	// transient scheduler, registry or quota problem that failed a whole
	// floor's worth of servers at once therefore took a large group straight
	// to a terminal give-up that only a spec edit clears, however briefly the
	// problem lasted.
	//
	// At minReplicas 1 nothing changes -- one server fails per round, so
	// rounds and servers are the same number -- which is the schedule design
	// §3.6 and §5 narrate: one free attempt and five growing waits.
	//
	// A pass counts at most one round however many corpses it sees. Two passes
	// that each see new failures are two rounds, which is the conservative
	// reading when one creation round fails in two batches: the operator
	// observed twice, and spending two of six is far from the six it used to
	// spend.
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
