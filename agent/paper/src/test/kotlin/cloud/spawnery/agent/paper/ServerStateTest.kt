package cloud.spawnery.agent.paper

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class ServerStateTest {
    @Test
    fun `starts not ready with no players`() {
        val state = ServerState()
        assertFalse(state.ready)
        assertEquals(0, state.players)
        assertEquals(0, state.slots)
    }

    @Test
    fun `markReady reports whether this was the transition`() {
        val state = ServerState()
        assertTrue(state.markReady(), "the first markReady is the transition")
        assertFalse(state.markReady(), "a repeated markReady is not")
        assertTrue(state.ready)
    }

    @Test
    fun `sample overwrites the last measurement`() {
        val state = ServerState()
        state.sample(players = 3, slots = 100)
        assertEquals(3, state.players)
        assertEquals(100, state.slots)
        state.sample(players = 7, slots = 100)
        assertEquals(7, state.players)
    }
}
