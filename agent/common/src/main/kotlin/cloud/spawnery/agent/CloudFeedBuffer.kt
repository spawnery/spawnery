package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/**
 * Holds a window of events and hands over the lines when it closes.
 *
 * The clock is a parameter and the tick is a call, so a test asserts the
 * boundary instead of racing it -- the same rule `internal/boost.Live` follows
 * on the operator's side, for the same reason.
 *
 * **The window starts at the first event, not at the last delivery.** A window
 * measured from the last tick would never close under a steady trickle, and
 * the feed would go quiet exactly when something is happening.
 *
 * Not thread-safe by itself. Events arrive on a network callback thread and
 * [tick] runs on the platform's scheduler, so the caller synchronises -- see
 * [Feed], which is the only thing that should be constructing one of these.
 */
class CloudFeedBuffer(
    private val clock: () -> Long,
    private val windowMillis: Long,
    private val deliver: (List<String>) -> Unit,
) {
    private val pending = mutableListOf<CloudEvent>()
    private var openedAt = 0L

    fun add(event: CloudEvent) {
        if (pending.isEmpty()) {
            openedAt = clock()
        }
        pending += event
    }

    fun tick() {
        if (pending.isEmpty()) return
        if (clock() - openedAt < windowMillis) return
        val lines = coalesce(pending.toList())
        // Cleared before delivering, not after: deliver reaches a platform,
        // and an exception from it must not leave this window to be sent again
        // on every tick for the rest of the process.
        pending.clear()
        if (lines.isNotEmpty()) {
            deliver(lines)
        }
    }
}
