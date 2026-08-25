package cloud.spawnery.agent.velocity

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.util.UUID

/**
 * Like [RouterTest], every test here builds a real [ServerDirectory] over a
 * [FakeRegistry] and a real [Router] on top of it -- the composition that runs
 * in production. [Rescue] takes a player as a [UUID] rather than a
 * `Player`, which is the whole reason this class needs no Velocity object at
 * all: the only thing it ever does with a player is use them as a map key.
 */
class RescueTest {
    private val logs = mutableListOf<String>()

    private fun rescueOver(vararg backends: Backend): Rescue {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(backends.toList())
        return Rescue(Router(directory)) { message, _ -> logs += message }
    }

    @Test
    fun `a player dropped by their server is sent to another in the group`() {
        val rescue = rescueOver(
            Backend("lobby-1", "10.0.0.1:25565", "lobby"),
            Backend("lobby-2", "10.0.0.2:25565", "lobby"),
        )

        val target = rescue.target(UUID.randomUUID(), "lobby-1", false, listOf("lobby"))

        // The measured production failure in one line: before this class
        // existed, Velocity disconnected the player here because its own
        // failover walks `try`, which internal/render renders empty.
        assertEquals("lobby-2", target?.serverInfo?.name)
    }

    @Test
    fun `a player who still has a working server is left where they are`() {
        val rescue = rescueOver(
            Backend("lobby-1", "10.0.0.1:25565", "lobby"),
            Backend("lobby-2", "10.0.0.2:25565", "lobby"),
        )

        // kickedDuringServerConnect: the player was refused by some *other*
        // server and is still sitting on the one they were on. Velocity's own
        // result is Notify. A target here would move somebody off a server
        // that is working.
        val target = rescue.target(UUID.randomUUID(), "lobby-1", true, listOf("lobby"))

        assertNull(target)
        assertTrue(logs.isEmpty(), "a player who kept their server is not an incident: $logs")
    }

    @Test
    fun `a chain never returns a server that already dropped the same player`() {
        // The ping-pong this guard exists for. Both backends are dead but
        // still registered -- the seconds between a node going and the
        // operator noticing -- and Router prefers the emptiest, which a dead
        // server always is. Excluding only the server just left would answer
        // lobby-1 here and bounce the player between the two.
        val rescue = rescueOver(
            Backend("lobby-1", "10.0.0.1:25565", "lobby"),
            Backend("lobby-2", "10.0.0.2:25565", "lobby"),
        )
        val player = UUID.randomUUID()

        assertEquals("lobby-2", rescue.target(player, "lobby-1", false, listOf("lobby"))?.serverInfo?.name)

        assertNull(rescue.target(player, "lobby-2", false, listOf("lobby")))
        assertEquals(1, logs.size, "the exhausted chain is worth exactly one line: $logs")
        assertTrue(logs.single().contains("lobby-1") && logs.single().contains("lobby-2"))
    }

    @Test
    fun `arriving somewhere ends the chain`() {
        val rescue = rescueOver(
            Backend("lobby-1", "10.0.0.1:25565", "lobby"),
            Backend("lobby-2", "10.0.0.2:25565", "lobby"),
        )
        val player = UUID.randomUUID()
        rescue.target(player, "lobby-1", false, listOf("lobby"))

        // The player landed on lobby-2 and played there for an hour. When
        // lobby-2 later dies, lobby-1 -- long since replaced by a healthy pod
        // under the same name -- has to be a candidate again. A chain that
        // outlived the incident it was avoiding would strand them.
        rescue.forget(player)

        val target = rescue.target(player, "lobby-2", false, listOf("lobby"))

        assertEquals("lobby-1", target?.serverInfo?.name)
    }

    @Test
    fun `one player's chain does not narrow another's`() {
        val rescue = rescueOver(
            Backend("lobby-1", "10.0.0.1:25565", "lobby"),
            Backend("lobby-2", "10.0.0.2:25565", "lobby"),
        )
        rescue.target(UUID.randomUUID(), "lobby-1", false, listOf("lobby"))

        // A second player losing the same server gets the same answer. The
        // obvious wrong implementation -- one shared tried-set, which is the
        // cheaper thing to write -- would have excluded lobby-2 by now.
        val target = rescue.target(UUID.randomUUID(), "lobby-1", false, listOf("lobby"))

        assertEquals("lobby-2", target?.serverInfo?.name)
    }

    @Test
    fun `an empty fallback list leaves velocity's own decision in place`() {
        val rescue = rescueOver(Backend("lobby-1", "10.0.0.1:25565", "lobby"))

        // Null is not "do nothing": it is "Velocity's result stands", which
        // here is the DisconnectPlayer it built before firing the event. The
        // log line is the only record of which groups were searched.
        val target = rescue.target(UUID.randomUUID(), "lobby-1", false, emptyList())

        assertNull(target)
        assertEquals(1, logs.size)
    }
}
