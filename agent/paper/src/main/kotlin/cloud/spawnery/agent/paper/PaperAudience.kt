package cloud.spawnery.agent.paper

import cloud.spawnery.agent.FeedAudience
import net.kyori.adventure.text.Component
import org.bukkit.Bukkit
import java.util.UUID

/**
 * Paper's half of the feed's audience.
 *
 * See [cloud.spawnery.agent.FeedAudience] for why this is not [PaperSource]
 * with a third method: a `CommandSourceStack` cannot be built from a `Player`,
 * so the audience and the command source are two different types on this
 * platform and one on the other.
 *
 * An object rather than a class because `Bukkit` is static and this needs
 * nothing injected. Its Velocity counterpart is a class for the opposite
 * reason; they are deliberately not made to match.
 */
object PaperAudience : FeedAudience {
    override fun holders(permission: String): List<UUID> =
        Bukkit.getOnlinePlayers().filter { it.hasPermission(permission) }.map { it.uniqueId }

    override fun send(player: UUID, message: String) {
        // Null for somebody who left between the list and this call, which is
        // ordinary rather than exceptional.
        Bukkit.getPlayer(player)?.sendMessage(Component.text(message))
    }
}
