package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer

/**
 * A [Players] fixture for [DrainTest]: a fixed roster of [FakePlayer]s built up
 * front, each with a [FakePlayer.currentServer] a test can set -- and change
 * mid-test, which is what "a second identical drain moves nobody" needs to
 * simulate a reconnect completing between two `DrainPlayers` deliveries --
 * independently of anything [ServerDirectory]/[FakeRegistry] know about.
 *
 * [moves] is the one thing a test asserts against: every [PlayerRef.moveTo]
 * call, recorded as (username, target server name) in the order they
 * happened. That is enough to check who moved, where, how many times, and in
 * what order, without depending on how a [RegisteredServer] compares for
 * equality.
 */
class FakePlayers(private val roster: List<FakePlayer>) : Players {
    val moves = mutableListOf<Pair<String, String>>()

    override fun all(): List<PlayerRef> = roster.map { fake ->
        object : PlayerRef {
            override val username: String get() = fake.username
            override val currentServer: String? get() = fake.currentServer

            override fun moveTo(target: RegisteredServer) {
                fake.failWith?.let { throw it }
                moves += fake.username to target.serverInfo.name
            }
        }
    }

    override fun count(): Int = roster.size
}

/**
 * One entry in a [FakePlayers] roster.
 *
 * [currentServer] is a `var`, not fixed at construction, for the same reason
 * [FakeServer.players] is: a test drives it mid-scenario rather than
 * rebuilding the fixture.
 *
 * [failWith], when set, is thrown by this player's [PlayerRef.moveTo] instead
 * of recording a move -- the fixture [DrainTest] uses to show that an
 * exception moving one player is caught and logged, and does not stop the
 * rest of the drain.
 */
class FakePlayer(
    val username: String,
    var currentServer: String? = null,
    var failWith: Throwable? = null,
)
