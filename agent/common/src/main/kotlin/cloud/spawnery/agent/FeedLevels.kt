package cloud.spawnery.agent

import java.util.UUID
import java.util.concurrent.CompletableFuture
import java.util.concurrent.CompletionStage
import java.util.concurrent.ConcurrentHashMap

interface LevelStore {
    /** False while the store's own plugin is not loaded; [FeedLevels] then uses its memory. */
    fun available(): Boolean

    fun read(player: UUID): String?

    fun write(player: UUID, value: String): CompletionStage<*>
}

/**
 * Nothing removes a player who logs out; the map is bounded by the
 * administrators who type the command.
 */
class MemoryLevels : LevelStore {
    private val values = ConcurrentHashMap<UUID, String>()

    override fun available(): Boolean = true

    override fun read(player: UUID): String? = values[player]

    override fun write(player: UUID, value: String): CompletionStage<*> {
        values[player] = value
        return CompletableFuture.completedFuture(Unit)
    }
}

class FeedLevels(private val durable: LevelStore?, private val memory: LevelStore = MemoryLevels()) {
    private fun store(): LevelStore = durable?.takeIf { it.available() } ?: memory

    /** Read on the feed's tick, where a throw would cost every other player their lines. */
    fun level(player: UUID): FeedLevel =
        FeedLevel.of(
            try {
                store().read(player)
            } catch (_: RuntimeException) {
                null
            },
        )

    fun set(player: UUID, level: FeedLevel): CompletionStage<*> =
        try {
            store().write(player, level.word)
        } catch (e: RuntimeException) {
            CompletableFuture.failedFuture<Unit>(e)
        }

    /** Whether a level set now outlives this process. */
    fun kept(): Boolean = store() !== memory
}
