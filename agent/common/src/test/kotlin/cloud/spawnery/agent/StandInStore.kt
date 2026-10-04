package cloud.spawnery.agent

import java.util.UUID
import java.util.concurrent.CompletableFuture
import java.util.concurrent.CompletionStage

/** Stands in for LuckPerms: a value on the player wins over the one its group carries. */
internal class StandInStore(var loaded: Boolean = true) : LevelStore {
    val user = mutableMapOf<UUID, String>()
    val group = mutableMapOf<UUID, String>()
    var failWrites = false
    var failReads = false

    override fun available(): Boolean = loaded

    override fun read(player: UUID): String? {
        if (failReads) throw IllegalStateException("LuckPerms is reloading")
        return user[player] ?: group[player]
    }

    override fun write(player: UUID, value: String): CompletionStage<*> {
        if (failWrites) return CompletableFuture.failedFuture<Unit>(IllegalStateException("storage is read-only"))
        user[player] = value
        return CompletableFuture.completedFuture(Unit)
    }
}
