package cloud.spawnery.agent

/**
 * The kinds are the operator's Kubernetes event reasons, passed through as strings;
 * `internal/cloudevent`'s agreement test keeps these names in step with them.
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

/** Unknown is [Direction.NEUTRAL]: a newer operator adds reasons, and a wrong sign reads as a fact. */
internal fun direction(kind: String): Direction = when (kind) {
    in ADDED -> Direction.ADDED
    in REMOVED -> Direction.REMOVED
    else -> Direction.NEUTRAL
}
