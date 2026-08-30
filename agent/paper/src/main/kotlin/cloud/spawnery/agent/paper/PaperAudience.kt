package cloud.spawnery.agent.paper

import cloud.spawnery.agent.FeedAudience
import java.util.UUID

/**
 * Paper's audience for the chat feed, and it is deliberately empty.
 *
 * **A backend must not deliver the feed, because every player on it is also on
 * a proxy that delivers the same line.** That is not a guess about how somebody
 * deploys this: the operator's own NetworkPolicy allows ingress to a backend
 * only from pods carrying `spawnery.cloud/role: proxy`, and the operator
 * renders no Service for backends at all. A player cannot reach one except
 * through a proxy.
 *
 * Delivering from both sides is what a player actually saw -- every cloud
 * event twice, once from the proxy holding them and once from the backend they
 * were standing on. It only became visible when LuckPerms put both agents on
 * one database, because before that `op` granted the permission on the backend
 * alone and the proxy never had them.
 *
 * The agent still *receives* every event: [cloud.spawnery.agent.CloudEvents]
 * hands them to any plugin that subscribed through the API, and the plugin's
 * interest report is what keeps them flowing. What stops here is the chat line,
 * and only the chat line.
 *
 * **If a network without a proxy ever has to work**, this is the file to
 * change, and it needs more than deleting the emptyList: the agent has no way
 * to know whether a proxy is in front of it, so the answer would have to come
 * from the operator, which does know.
 */
object PaperAudience : FeedAudience {
    override fun holders(permission: String): List<UUID> = emptyList()

    override fun send(player: UUID, message: String) = Unit
}
