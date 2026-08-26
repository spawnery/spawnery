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

    override fun all(): List<PlayerRef> = roster.map(::ref)

    /**
     * One roster entry as a [PlayerRef], for the caller that has a player
     * already rather than a roster to scan -- `Drain.landed` takes exactly
     * one, the way `AgentPlugin` hands it the player an arrival event named.
     * The same object [all] builds, so a test that mixes the two is not
     * comparing two different fakes.
     */
    fun ref(fake: FakePlayer): PlayerRef = object : PlayerRef {
        override val username: String get() = fake.username
        override val currentServer: String? get() = fake.currentServer

        // Defaults to currentServer, so every existing fixture keeps meaning
        // what it meant. A test about the arriving player sets it apart.
        override val attachedServer: String? get() = fake.attachedServer ?: fake.currentServer

        override fun moveTo(target: RegisteredServer) {
            fake.failWith?.let { throw it }
            moves += fake.username to target.serverInfo.name
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
    /**
     * Where this player is heading, when that differs from where they are.
     *
     * Null means "the same as currentServer", which is true of every player
     * who is not mid-handshake and keeps every fixture written before this
     * existed saying what it said. The case worth setting it for is the one
     * the operator could not see: currentServer null, attachedServer named --
     * a player whose connection is in flight and whom neither the backend nor
     * the proxy's own player list counts.
     */
    var attachedServer: String? = null,
)
