package cloud.spawnery.agent.velocity

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

/**
 * Six lines of production code, two tests, and both of them earn their place.
 *
 * [ProxyState.slots] never moving is what the operator's registry depends on:
 * `internal/agent` discards any report whose players exceed its slots, so a
 * slot count that could drift down below a live player count would silently
 * throw every subsequent report away. That is worth one assertion even though
 * nothing in this class could plausibly change it today.
 */
class ProxyStateTest {
    @Test
    fun `slots is what it was constructed with and never changes`() {
        val state = ProxyState(slots = 500)
        assertEquals(500, state.slots)

        // The only mutator this class has. A proxy's capacity comes from
        // ProxyGroup.spec.config.playerLimit by way of the pod's environment,
        // and nothing inside the JVM may move it -- unlike Paper, where
        // Bukkit.getMaxPlayers() is sampled alongside the player count.
        state.sample(players = 17)
        assertEquals(500, state.slots, "sampling players moved the configured slot count")
    }

    @Test
    fun `players starts at zero and reads back what was sampled`() {
        val state = ProxyState(slots = 500)
        assertEquals(0, state.players)

        state.sample(players = 3)
        assertEquals(3, state.players)

        // Every sample replaces the last: this is a gauge the scheduler
        // overwrites, not a counter anything accumulates.
        state.sample(players = 1)
        assertEquals(1, state.players)
    }
}
