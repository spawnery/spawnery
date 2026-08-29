package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.FeedAudience
import com.velocitypowered.api.proxy.ProxyServer
import net.kyori.adventure.text.minimessage.MiniMessage
import java.util.UUID

/**
 * Velocity's half of the feed's audience.
 *
 * A class and not an object: the proxy is injected here, where Paper's
 * counterpart reaches a static. See that one for why the audience is a
 * separate interface from the command source at all -- on this platform a
 * `Player` *is* a `CommandSource`, and on Paper it is not.
 */
class VelocityAudience(private val proxy: ProxyServer) : FeedAudience {
    override fun holders(permission: String): List<UUID> =
        proxy.allPlayers.filter { it.hasPermission(permission) }.map { it.uniqueId }

    override fun send(player: UUID, message: String) {
        proxy.getPlayer(player).ifPresent { it.sendMessage(MiniMessage.miniMessage().deserialize(message)) }
    }
}
