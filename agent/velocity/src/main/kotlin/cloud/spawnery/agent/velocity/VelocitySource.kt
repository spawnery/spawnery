package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.SourceAdapter
import com.velocitypowered.api.command.CommandSource
import com.velocitypowered.api.proxy.Player
import net.kyori.adventure.text.minimessage.MiniMessage
import java.util.UUID

/** Velocity's half; see [cloud.spawnery.agent.paper.PaperSource]. */
object VelocitySource : SourceAdapter<CommandSource> {
    override fun hasPermission(source: CommandSource, permission: String): Boolean =
        source.hasPermission(permission)

    override fun send(source: CommandSource, message: String) {
        source.sendMessage(MiniMessage.miniMessage().deserialize(message))
    }

    // On this platform a Player *is* a CommandSource; see FeedAudience.
    override fun playerId(source: CommandSource): UUID? =
        (source as? Player)?.uniqueId
}
