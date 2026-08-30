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
    /**
     * What the network's own spec says a feed line should look like, read
     * fresh on every delivery.
     *
     * A lambda and not a value, because the format arrives in the NetworkState
     * the operator resends on every resync: a value captured at construction
     * would hold whatever the first message said, and an edit would take
     * effect at the next pod rather than at the next resync -- which is
     * exactly the rolling this design put the format on the wire to avoid.
     */
    private val format: () -> String = { DEFAULT_FORMAT },
) {
    private val lock = Any()
    private val buffer = CloudFeedBuffer(clock, windowMillis) { lines -> deliver(lines) }

    /** Called from the network callback. */
    fun onEvent(event: CloudEvent) = synchronized(lock) { buffer.add(event) }

    /** Called from the platform's scheduler. */
    fun tick() = synchronized(lock) { buffer.tick() }

    /**
     * Whether anybody is here to read this -- in chat or in a plugin.
     *
     * **The subscriber count is not decoration.** A backend's audience is
     * empty by design (see the Paper agent's own FeedAudience: every player on
     * it is behind a proxy that delivers the same line), so without this a
     * backend would report no interest, the operator would stop sending it
     * events, and a plugin that subscribed through the API would receive
     * nothing at all -- silently, with every object in the cluster looking
     * correct.
     *
     * Recomputed rather than tracked, because a player joining, leaving,
     * gaining a permission, or typing the command all change the answer, and
     * three of those four are events this agent would otherwise have to watch
     * for. One pass over the online list per tick is cheaper than being wrong.
     */
    fun wanted(subscribers: Int): Boolean =
        subscribers > 0 || audience.holders(PERMISSION_EVENTS).any(state::wants)

    private fun deliver(lines: List<String>) {
        // The list is read once and reused for every line. A player who left
        // between two lines gets a no-op send, which FeedAudience documents --
        // and re-reading it per line would give one recipient a partial batch.
        val recipients = audience.holders(PERMISSION_EVENTS).filter(state::wants)
        if (recipients.isEmpty()) return
        // Read once per delivery, not once per recipient: every line of one
        // window is the same shape, and a resync landing mid-loop would
        // otherwise give two players different-looking lines for one event.
        val shape = format().ifBlank { DEFAULT_FORMAT }
        for (who in recipients) {
            for (line in lines) {
                audience.send(who, shape.replace(MESSAGE_TOKEN, line))
            }
        }
    }

    companion object {
        /** Section 5.4's window. */
        const val WINDOW_MILLIS: Long = 1_000

        /**
         * What the format replaces with the event.
         *
         * A plain token and not a printf verb or a MiniMessage placeholder: a
         * format is written by a person in a YAML file, and the two things
         * that could go wrong there -- a stray `%` and a tag the parser would
         * eat -- are exactly what those would introduce.
         */
        const val MESSAGE_TOKEN: String = "\$EVENT_MESSAGE"

        /**
         * The format an agent uses when the operator sends none.
         *
         * Kept in step by hand with the +kubebuilder:default on
         * Defaults.FeedFormat. They differ only for an agent newer than its
         * operator, which is the one case the CRD's default cannot cover.
         */
        const val DEFAULT_FORMAT: String =
            "<gray>»</gray> <gradient:aqua:green>Spawnery</gradient> " +
                "<dark_gray>|</dark_gray> <gray>\$EVENT_MESSAGE"
    }
}
