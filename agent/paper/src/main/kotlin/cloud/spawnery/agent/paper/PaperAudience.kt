package cloud.spawnery.agent.paper

import cloud.spawnery.agent.FeedAudience
import java.util.UUID

/**
 * Empty: every player on a backend is also on a proxy that delivers the same
 * line, since the operator's NetworkPolicy admits only proxies. Plugins still
 * get every event through [cloud.spawnery.agent.CloudEvents].
 */
object PaperAudience : FeedAudience {
    override fun holders(permission: String): List<UUID> = emptyList()

    override fun send(player: UUID, message: String) = Unit
}
