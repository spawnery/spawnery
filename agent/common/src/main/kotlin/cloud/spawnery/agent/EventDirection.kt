package cloud.spawnery.agent

/**
 * Whether an event kind added capacity to the network, took some away, or did
 * neither.
 *
 * The kinds are the operator's own event reasons, spelled here as strings
 * because the wire carries them as strings -- `internal/cloudevent.Derive`
 * passes the Kubernetes reason through untouched. `internal/cloudevent`'s
 * agreement test reads this file and fails when a name here is not a reason
 * over there, which is the only thing keeping the two lists in step.
 */
internal enum class Direction { ADDED, REMOVED, NEUTRAL }

private val ADDED = setOf(
    "ReadyGatePassed",
    "PodRunning",
    "JoinsOpen",
)

private val REMOVED = setOf(
    "DeletionRequested",
    "Drained",
    "DrainingBeforeCleanup",
    "FinishedRetentionElapsed",
    "MaxStaleElapsed",
    "PodLost",
    "ReadinessLost",
    "RetentionElapsed",
    "Retiring",
    "RoundFinished",
    "Terminating",
)

/**
 * Unknown is [Direction.NEUTRAL] and not a failure.
 *
 * The operator's set of reasons is open, and a release that adds one must not
 * make an agent of the previous release guess. A wrong sign reads as a fact.
 */
internal fun direction(kind: String): Direction = when (kind) {
    in ADDED -> Direction.ADDED
    in REMOVED -> Direction.REMOVED
    else -> Direction.NEUTRAL
}
