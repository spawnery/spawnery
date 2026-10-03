package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer
import java.util.UUID

/**
 * A [Players] fixture: a fixed roster whose [FakePlayer.currentServer] a test
 * can change mid-test. [moves] records every [PlayerRef.moveTo] as (username,
 * target server name), in order.
 */
class FakePlayers(private val roster: List<FakePlayer>) : Players {
    val moves = mutableListOf<Pair<String, String>>()

    override fun all(): List<PlayerRef> = roster.map(::ref)

    /** The same object [all] builds, for a caller that already has a player. */
    fun ref(fake: FakePlayer): PlayerRef = object : PlayerRef {
        override val uuid: UUID get() = fake.uuid
        override val username: String get() = fake.username
        override val currentServer: String? get() = fake.currentServer

        // Defaults to currentServer; only an arriving player differs.
        override val attachedServer: String? get() = fake.attachedServer ?: fake.currentServer

        override fun moveTo(target: RegisteredServer) {
            fake.failWith?.let { throw it }
            moves += fake.username to target.serverInfo.name
        }
    }

    override fun count(): Int = roster.size
}

/**
 * One entry in a [FakePlayers] roster. [failWith], when set, is thrown by
 * [PlayerRef.moveTo] instead of recording a move.
 */
class FakePlayer(
    val username: String,
    var currentServer: String? = null,
    var failWith: Throwable? = null,
    /** Where this player is heading; null means the same as currentServer. */
    var attachedServer: String? = null,
    /** Last, so a positional caller cannot bind a String into it. */
    val uuid: UUID = UUID.nameUUIDFromBytes(username.toByteArray()),
)
