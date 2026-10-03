package cloud.spawnery.agent

import java.util.UUID

/** Not part of [SourceAdapter]: on Paper nothing turns an online `Player` into a `CommandSourceStack`. */
interface FeedAudience {
    fun holders(permission: String): List<UUID>

    /** A no-op for a player who has left since [holders]. */
    fun send(player: UUID, message: String)
}
