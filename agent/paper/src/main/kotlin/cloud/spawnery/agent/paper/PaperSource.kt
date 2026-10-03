package cloud.spawnery.agent.paper

import cloud.spawnery.agent.SourceAdapter
import io.papermc.paper.command.brigadier.CommandSourceStack
import net.kyori.adventure.text.minimessage.MiniMessage
import org.bukkit.entity.Player
import java.util.UUID

object PaperSource : SourceAdapter<CommandSourceStack> {
    override fun hasPermission(source: CommandSourceStack, permission: String): Boolean =
        source.sender.hasPermission(permission)

    override fun send(source: CommandSourceStack, message: String) {
        source.sender.sendMessage(MiniMessage.miniMessage().deserialize(message))
    }

    // Not getPlayerOrThrow(), which throws for the console.
    override fun playerId(source: CommandSourceStack): UUID? =
        (source.sender as? Player)?.uniqueId
}
