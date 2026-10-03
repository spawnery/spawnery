package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.FeedAudience
import com.velocitypowered.api.proxy.ProxyServer
import net.kyori.adventure.text.minimessage.MiniMessage
import java.util.UUID

/** Velocity's half of the feed's audience; the proxy is injected here. */
class VelocityAudience(private val proxy: ProxyServer) : FeedAudience {
    override fun holders(permission: String): List<UUID> =
        proxy.allPlayers.filter { it.hasPermission(permission) }.map { it.uniqueId }

    override fun send(player: UUID, message: String) {
        proxy.getPlayer(player).ifPresent { it.sendMessage(MiniMessage.miniMessage().deserialize(message)) }
    }
}
