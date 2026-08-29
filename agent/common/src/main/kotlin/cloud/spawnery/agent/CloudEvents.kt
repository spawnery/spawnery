package cloud.spawnery.agent

import cloud.spawnery.agent.api.CloudEventInfo
import cloud.spawnery.agent.api.EventBus
import cloud.spawnery.agent.pb.CloudEvent
import java.util.concurrent.CopyOnWriteArrayList
import java.util.function.Consumer

/**
 * The [EventBus] a plugin subscribes to.
 *
 * Separate from [Feed], and deliberately: the feed is presentation -- it
 * collapses a rolling update into one readable line -- while this hands over
 * the facts one at a time. A plugin that got the feed's summary would be
 * reading somebody else's editorial decision.
 *
 * `CopyOnWriteArrayList` because [publish] runs on a network callback thread
 * and subscribe/close run on whichever thread a plugin enables on. Writes are
 * rare -- once per plugin enable -- and a dispatch must never wait on one.
 */
class CloudEvents : EventBus {
    private val listeners = CopyOnWriteArrayList<Consumer<CloudEventInfo>>()

    override fun subscribe(listener: Consumer<CloudEventInfo>): AutoCloseable {
        listeners += listener
        // Idempotent: a plugin that closes twice, or closes on disable after
        // the agent already stopped, must not be punished for it.
        return AutoCloseable { listeners.remove(listener) }
    }

    /**
     * Hands one event to every listener.
     *
     * A listener that throws is dropped and the rest still run. The
     * alternative is that one plugin's bug costs every other plugin its
     * events, and -- because this runs inside a gRPC callback -- costs the
     * agent its session too.
     */
    fun publish(event: CloudEvent) {
        val info = CloudEventInfo(
            event.kind,
            event.subject,
            event.group,
            event.message,
            event.warning,
        )
        for (listener in listeners) {
            try {
                listener.accept(info)
            } catch (failure: RuntimeException) {
                listeners.remove(listener)
            } catch (failure: Error) {
                // An Error is not this agent's to swallow -- an
                // OutOfMemoryError caught and ignored here would leave the JVM
                // limping with nothing said. Drop the listener and rethrow.
                listeners.remove(listener)
                throw failure
            }
        }
    }

    /** How many listeners are subscribed. For tests. */
    fun size(): Int = listeners.size
}
