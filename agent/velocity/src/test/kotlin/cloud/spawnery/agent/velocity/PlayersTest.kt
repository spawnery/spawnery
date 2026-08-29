package cloud.spawnery.agent.velocity

import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals

class PlayersTest {
    @Test
    fun `a player ref carries the identity the operator has no other source for`() {
        val id = UUID.fromString("00000000-0000-4000-8000-000000000001")
        val players = FakePlayers(listOf(FakePlayer(username = "somebody", uuid = id)))

        val ref = players.all().single()

        assertEquals(id, ref.uuid)
        assertEquals("somebody", ref.username)
    }

    // Every fixture written before uuid existed passes none, and none of them
    // is about identity. A default derived from the username keeps them
    // compiling and keeps two different fakes from colliding on one UUID.
    @Test
    fun `a fake without an explicit uuid still has a distinct one`() {
        val players = FakePlayers(listOf(FakePlayer("alice"), FakePlayer("bob")))
        val ids = players.all().map { it.uuid }.toSet()
        assertEquals(2, ids.size)
    }
}
