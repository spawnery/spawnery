package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/**
 * The kinds are the operator's Kubernetes event reasons, passed through as strings;
 * `internal/cloudevent`'s agreement test keeps these names in step with them.
 */
internal enum class Row { CREATED, ARRIVED, READY, LEAVING, GONE, OTHER }

private val CREATED = setOf(
    "PodCreated",
)

/** A proxy records nothing before it first takes connections. */
private val ARRIVED = setOf(
    "ProxyStarted",
)

private val READY = setOf(
    "ReadyGatePassed",
)

private val LEAVING = setOf(
    "DeletionRequested",
    "DrainingBeforeCleanup",
    "ForceStopped",
    "ProxyRetiring",
    "Retiring",
    "RoundFinished",
)

private val GONE = setOf(
    "ProxyStopped",
    "ServerStopped",
)

/** Phase changes the operator records as Normal although they are failures. */
private val FAILURES = setOf(
    "DrainTimeout",
    "Flapping",
    "PodLost",
    "PodNeverCreated",
    "PodTerminal",
    "StartupTimeout",
)

private val SHORT = mapOf(
    "DrainTimeout" to "drain deadline, players cut off",
    "Flapping" to "keeps losing readiness",
    "ForceStopped" to "killed",
    "NamespaceNotBootstrapped" to "namespace not ready",
    "PodKilled" to "killed",
    "PodLost" to "pod vanished",
    "PodNeverCreated" to "pod never appeared",
    "PodTerminal" to "pod exited",
    "ProxyDrainTimeout" to "drain deadline, players cut off",
    "ProxyPodBlocked" to "proxy pod blocked",
    "ReadinessLost" to "lost readiness",
    "ServerClaimRejected" to "volume claim refused",
    "ServerPodRejected" to "pod refused",
    "StartupTimeout" to "did not start in time",
)

/** An unknown kind is [Row.OTHER], so a reason a newer operator adds shows only in verbose. */
internal fun row(kind: String): Row = when (kind) {
    in CREATED -> Row.CREATED
    in ARRIVED -> Row.ARRIVED
    in READY -> Row.READY
    in LEAVING -> Row.LEAVING
    in GONE -> Row.GONE
    else -> Row.OTHER
}

internal fun isWarning(event: CloudEvent): Boolean = event.warning || event.kind in FAILURES

internal fun shortReason(kind: String): String = SHORT[kind] ?: words(kind)

private val WORD_BREAK = Regex("(?<=[a-z0-9])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])")

internal fun words(kind: String): String =
    if (kind.isBlank()) "warning" else kind.replace(WORD_BREAK, " ").lowercase()
