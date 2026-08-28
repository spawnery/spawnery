package cloud.spawnery.agent.paper

import cloud.spawnery.agent.SourceAdapter
import io.papermc.paper.command.brigadier.CommandSourceStack
import net.kyori.adventure.text.Component

/**
 * Paper's half of the two things the command tree may ask of a platform.
 *
 * Adventure appears here and in the Velocity counterpart and nowhere else: the
 * tree hands out Strings, and each side converts at the last moment. A
 * `Component` in the shared signature would put a platform type in a shared
 * module, which is the trap the design records for Kotlin and for protobuf.
 */
object PaperSource : SourceAdapter<CommandSourceStack> {
    override fun hasPermission(source: CommandSourceStack, permission: String): Boolean =
        source.sender.hasPermission(permission)

    override fun send(source: CommandSourceStack, message: String) {
        source.sender.sendMessage(Component.text(message))
    }
}
