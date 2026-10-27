package cloud.spawnery.agent.paper

import java.util.concurrent.CompletableFuture

/**
 * One region's save for a snapshot, offered again until the region takes it.
 * [refusal] is why the latest offer was turned down, so a wait that gives up
 * can say what it was still waiting on.
 */
class RegionSave(private val region: String) : CompletableFuture<Void>() {
    @Volatile
    var refusal: String? = null
        private set

    /**
     * [propagate] hands the region its save ticket; [again] runs the next
     * offer a tick later.
     */
    fun offer(dead: () -> Boolean, propagate: () -> CompletableFuture<*>, again: (() -> Unit) -> Unit) {
        if (isDone) return
        // A dead region's chunks went to another region, which may have saved
        // before they arrived.
        if (dead()) {
            completeExceptionally(IllegalStateException("region $region merged or split before it saved"))
            return
        }
        try {
            propagate().whenComplete { _, error ->
                if (error == null) complete(null) else completeExceptionally(error)
            }
        } catch (e: IllegalStateException) {
            if (e.message != TICKET_HELD) {
                completeExceptionally(e)
                return
            }
            refusal = "region $region: ${e.message}"
            again { offer(dead, propagate, again) }
        } catch (t: Throwable) {
            completeExceptionally(t)
        }
    }

    companion object {
        // Canvas's TicketHolder.propagate while the region still holds a ticket
        // (read from Canvas 26.3 build 991).
        const val TICKET_HELD = "Ticket already propagated"
    }
}
