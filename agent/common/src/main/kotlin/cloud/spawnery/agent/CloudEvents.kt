package cloud.spawnery.agent

import cloud.spawnery.agent.api.CloudEventInfo
import cloud.spawnery.agent.api.EventBus
import cloud.spawnery.agent.pb.CloudEvent
import java.util.concurrent.CopyOnWriteArrayList
import java.util.function.Consumer

/** [publish] runs on a network callback thread and must never wait on a subscribe. */
class CloudEvents : EventBus {
    private val listeners = CopyOnWriteArrayList<Consumer<CloudEventInfo>>()

    override fun subscribe(listener: Consumer<CloudEventInfo>): AutoCloseable {
        listeners += listener
        return AutoCloseable { listeners.remove(listener) }
    }

    /** A listener that throws is dropped: inside a gRPC callback a throw would cost the session. */
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
                listeners.remove(listener)
                throw failure
            }
        }
    }

    /** For tests. */
    fun size(): Int = listeners.size
}
