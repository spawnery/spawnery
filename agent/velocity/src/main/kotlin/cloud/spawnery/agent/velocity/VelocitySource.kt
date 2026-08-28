package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.SourceAdapter
import com.velocitypowered.api.command.CommandSource
import net.kyori.adventure.text.Component

/**
 * Velocity's half. See [cloud.spawnery.agent.paper.PaperSource]'s counterpart
 * for why Adventure lives here rather than in the tree -- the two files are
 * the same four lines against two platforms, which is the whole of what
 * differs between them.
 */
object VelocitySource : SourceAdapter<CommandSource> {
    override fun hasPermission(source: CommandSource, permission: String): Boolean =
        source.hasPermission(permission)

    override fun send(source: CommandSource, message: String) {
        source.sendMessage(Component.text(message))
    }
}
