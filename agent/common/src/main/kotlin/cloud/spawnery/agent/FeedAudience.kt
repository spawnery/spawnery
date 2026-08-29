package cloud.spawnery.agent

import java.util.UUID

/**
 * Where the feed goes: everybody online who may see it.
 *
 * **Separate from [SourceAdapter], and measured rather than chosen.** On
 * Velocity `Player` extends `CommandSource`, so the two could have been one
 * interface. On Paper they cannot: `CommandSourceStack` is an interface with
 * seven accessors and no factory, and nothing turns a `Player` from
 * `Bukkit.getOnlinePlayers()` into one. A `holders(): List<S>` on
 * [SourceAdapter] would compile on one platform and be unimplementable on the
 * other, which is the asymmetry this design refuses everywhere else.
 *
 * Two methods with a UUID between them rather than one taking a lambda: the
 * caller has to drop whoever turned the feed off, and it needs an identity to
 * do that.
 */
interface FeedAudience {
    /** The ids of everybody online holding [permission]. */
    fun holders(permission: String): List<UUID>

    /**
     * Sends one line to one player. A no-op for a player who has left, which
     * is ordinary rather than exceptional: the list above is a moment old by
     * the time it is used.
     */
    fun send(player: UUID, message: String)
}
