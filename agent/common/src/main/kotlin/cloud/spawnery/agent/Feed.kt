package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/** The permission a source needs to see the feed. */
const val PERMISSION_EVENTS: String = "spawnery.cloud.events"

/**
 * The feed, from an arriving event to a line in somebody's chat.
 *
 * Written once for both platforms, because the only thing that differs is who
 * is online and how a line reaches them -- which is exactly [FeedAudience]'s
 * two methods and nothing else.
 *
 * **The lock separates two threads that exist on both platforms**: events
 * arrive on a network callback thread and [tick] runs on the platform's own
 * scheduler. Without it the buffer's list would be mutated during its own
 * iteration, and that fails as a ConcurrentModificationException on a network
 * callback -- where a throw costs the session, not just the line.
 */
class Feed(
    private val audience: FeedAudience,
    private val state: FeedState,
    clock: () -> Long,
    windowMillis: Long = WINDOW_MILLIS,
) {
    private val lock = Any()
    private val buffer = CloudFeedBuffer(clock, windowMillis) { lines -> deliver(lines) }

    /** Called from the network callback. */
    fun onEvent(event: CloudEvent) = synchronized(lock) { buffer.add(event) }

    /** Called from the platform's scheduler. */
    fun tick() = synchronized(lock) { buffer.tick() }

    /**
     * Whether anybody is here to read this.
     *
     * Recomputed rather than tracked, because a player joining, leaving,
     * gaining a permission, or typing the command all change the answer, and
     * three of those four are events this agent would otherwise have to watch
     * for. One pass over the online list per tick is cheaper than being wrong.
     */
    fun wanted(): Boolean = audience.holders(PERMISSION_EVENTS).any(state::wants)

    private fun deliver(lines: List<String>) {
        // The list is read once and reused for every line. A player who left
        // between two lines gets a no-op send, which FeedAudience documents --
        // and re-reading it per line would give one recipient a partial batch.
        val recipients = audience.holders(PERMISSION_EVENTS).filter(state::wants)
        for (who in recipients) {
            for (line in lines) {
                audience.send(who, line)
            }
        }
    }

    companion object {
        /** Section 5.4's window. */
        const val WINDOW_MILLIS: Long = 1_000
    }
}
