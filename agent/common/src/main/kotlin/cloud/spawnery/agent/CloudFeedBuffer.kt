package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/**
 * The window starts at the first event, not at the last delivery: measured from
 * the last tick it would never close under a steady trickle.
 *
 * Not thread-safe; [Feed] synchronises.
 */
class CloudFeedBuffer(
    private val clock: () -> Long,
    private val windowMillis: Long,
    private val deliver: (List<CloudEvent>) -> Unit,
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
        val batch = pending.toList()
        // Cleared first, so a throwing deliver does not resend this window on every tick.
        pending.clear()
        deliver(batch)
    }
}
