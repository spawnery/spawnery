package cloud.spawnery.agent.velocity

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.util.UUID

/** Over a real [ServerDirectory]/[FakeRegistry] and [Router]. */
class RescueTest {
    private val logs = mutableListOf<String>()

    private fun rescueOver(vararg backends: Backend): Rescue {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(backends.toList())
        return Rescue(Router(directory), { message, _ -> logs += message })
    }

    @Test
    fun `a player dropped by their server is sent to another in the group`() {
        val rescue = rescueOver(
            Backend("lobby-1", "10.0.0.1:25565", "lobby"),
            Backend("lobby-2", "10.0.0.2:25565", "lobby"),
        )

        val target = rescue.target(UUID.randomUUID(), "lobby-1", false, listOf("lobby"))

        // Velocity's own failover walks `try`, which internal/render renders empty.
        assertEquals("lobby-2", target?.serverInfo?.name)
    }

    @Test
    fun `a player who still has a working server is left where they are`() {
        val rescue = rescueOver(
            Backend("lobby-1", "10.0.0.1:25565", "lobby"),
            Backend("lobby-2", "10.0.0.2:25565", "lobby"),
        )

        // The player is still on a working server; Velocity's result is Notify.
        val target = rescue.target(UUID.randomUUID(), "lobby-1", true, listOf("lobby"))

        assertNull(target)
        assertTrue(logs.isEmpty(), "a player who kept their server is not an incident: $logs")
    }

    @Test
    fun `a chain never returns a server that already dropped the same player`() {
        // Both backends dead but registered, and Router prefers the emptiest:
        // excluding only the server just left would bounce the player.
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

        // After the player landed, lobby-1 is a candidate again.
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

        // Per-player chains, not one shared set.
        val target = rescue.target(UUID.randomUUID(), "lobby-1", false, listOf("lobby"))

        assertEquals("lobby-2", target?.serverInfo?.name)
    }

    @Test
    fun `an empty fallback list leaves velocity's own decision in place`() {
        val rescue = rescueOver(Backend("lobby-1", "10.0.0.1:25565", "lobby"))

        // Null means Velocity's own DisconnectPlayer stands.
        val target = rescue.target(UUID.randomUUID(), "lobby-1", false, emptyList())

        assertNull(target)
        assertEquals(1, logs.size)
    }

    @Test
    fun `a rescued player is not sent into a group they may not join`() {
        val registry = FakeRegistry()
        val directory = ServerDirectory(registry) { _, _ -> }
        directory.apply(
            listOf(
                Backend("vip-1", "10.0.0.1:25565", "vip"),
                Backend("lobby-1", "10.0.0.2:25565", "lobby"),
            ),
        )
        val rescue = Rescue(Router(directory), { _, _ -> }, JoinAccess { _, _, group -> group != "vip" })

        val target = rescue.target(UUID.randomUUID(), "hub-1", false, listOf("vip", "lobby"))

        assertEquals("lobby-1", target?.serverInfo?.name)
    }
}
