package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.pb.CloudEvent

const val PERMISSION_EVENTS: String = "spawnery.cloud.events"

class Feed(
    private val audience: FeedAudience,
    private val levels: FeedLevels,
    clock: () -> Long,
    windowMillis: Long = WINDOW_MILLIS,
    /** A lambda, because the format arrives with every resync and an edit must not wait for the next pod. */
    private val format: () -> String = { DEFAULT_FORMAT },
    /** On-demand members get short names; a backend's picture has no on-demand groups, so it never shortens. */
    private val groupKind: (String) -> Group.Kind = { Group.Kind.UNKNOWN },
) {
    private val lock = Any()
    private val buffer = CloudFeedBuffer(clock, windowMillis) { events -> deliver(events) }

    /** Called from the network callback. */
    fun onEvent(event: CloudEvent) = synchronized(lock) { buffer.add(event) }

    /** Called from the platform's scheduler. */
    fun tick() = synchronized(lock) { buffer.tick() }

    /**
     * A backend's audience is empty by design, so without [subscribers] the
     * operator would stop sending events its plugins subscribed to.
     */
    fun wanted(subscribers: Int): Boolean =
        subscribers > 0 || audience.holders(PERMISSION_EVENTS).any { levels.level(it) != FeedLevel.OFF }

    private fun deliver(events: List<CloudEvent>) {
        // Read once, not per line, so nobody gets a partial batch.
        val byLevel = audience.holders(PERMISSION_EVENTS).groupBy(levels::level)
        if (byLevel.keys.all { it == FeedLevel.OFF }) return
        // Read once, so a resync mid-loop cannot give two players different shapes.
        val shape = format().ifBlank { DEFAULT_FORMAT }
        for ((level, players) in byLevel) {
            val lines = coalesce(events, level, groupKind)
            for (who in players) {
                for (line in lines) {
                    audience.send(who, shape.replace(MESSAGE_TOKEN, line))
                }
            }
        }
    }

    companion object {
        const val WINDOW_MILLIS: Long = 1_000

        /** Not a printf verb or MiniMessage placeholder: a hand-written format must survive a stray `%`. */
        const val MESSAGE_TOKEN: String = "\$EVENT_MESSAGE"

        /** Kept in step by hand with the CRD default on Defaults.FeedFormat, for an agent newer than its operator. */
        const val DEFAULT_FORMAT: String =
            "<gray>»</gray> <gradient:aqua:green>Spawnery</gradient> " +
                "<dark_gray>|</dark_gray> <gray>\$EVENT_MESSAGE"
    }
}
