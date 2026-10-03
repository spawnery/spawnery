package cloud.spawnery.agent.velocity

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

/** [ProxyState.slots] never moves: the operator discards reports with players above slots. */
class ProxyStateTest {
    @Test
    fun `slots is what it was constructed with and never changes`() {
        val state = ProxyState(slots = 500)
        assertEquals(500, state.slots)

        state.sample(players = 17)
        assertEquals(500, state.slots, "sampling players moved the configured slot count")
    }

    @Test
    fun `players starts at zero and reads back what was sampled`() {
        val state = ProxyState(slots = 500)
        assertEquals(0, state.players)

        state.sample(players = 3)
        assertEquals(3, state.players)

        // A gauge the scheduler overwrites, not a counter.
        state.sample(players = 1)
        assertEquals(1, state.players)
    }

    @Test
    fun `reading the count does not consume it`() {
        val state = ProxyState(slots = 500)
        state.sample(players = 4)

        assertEquals(4, state.players)
        // Read twice with no sample in between, which catches a destructive
        // read: the sampler and the reporting timer run on independent clocks.
        assertEquals(4, state.players, "reading the player count consumed it")
    }
}
