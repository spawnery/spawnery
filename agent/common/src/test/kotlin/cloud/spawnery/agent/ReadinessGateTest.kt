package cloud.spawnery.agent

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class ReadinessGateTest {
    private fun counting(): Pair<ReadinessGate, () -> Int> {
        var opened = 0
        val gate = ReadinessGate { opened++ }
        return gate to { opened }
    }

    @Test
    fun `the load event alone opens the gate`() {
        val (gate, opened) = counting()
        gate.serverLoaded()
        assertEquals(1, opened())
    }

    @Test
    fun `a hold keeps the gate shut past the load event`() {
        val (gate, opened) = counting()
        val hold = gate.hold("mappings")
        gate.serverLoaded()
        assertEquals(0, opened(), "the hold is still open")
        hold.close()
        assertEquals(1, opened())
    }

    @Test
    fun `the last hold opens it, not the first`() {
        val (gate, opened) = counting()
        val one = gate.hold("mappings")
        val two = gate.hold("database")
        gate.serverLoaded()
        one.close()
        assertEquals(0, opened())
        two.close()
        assertEquals(1, opened())
    }

    @Test
    fun `a hold released before the load event does not open it early`() {
        val (gate, opened) = counting()
        gate.hold("mappings").close()
        assertEquals(0, opened(), "the server has not finished enabling yet")
        gate.serverLoaded()
        assertEquals(1, opened())
    }

    @Test
    fun `the gate opens once`() {
        val (gate, opened) = counting()
        gate.serverLoaded()
        gate.serverLoaded()
        assertEquals(1, opened())
    }

    @Test
    fun `a hold taken after the gate opened changes nothing`() {
        // Readiness is a one-way latch: ServerState.markReady cannot be
        // cleared, so a late hold must not pretend it can.
        val (gate, opened) = counting()
        gate.serverLoaded()
        gate.hold("too late").close()
        assertEquals(1, opened())
        assertTrue(gate.openReasons().isEmpty())
    }

    @Test
    fun `closing a hold twice releases once`() {
        val (gate, opened) = counting()
        val one = gate.hold("mappings")
        val two = gate.hold("database")
        gate.serverLoaded()
        one.close()
        one.close()
        assertEquals(0, opened(), "the second close must not stand in for two")
        two.close()
        assertEquals(1, opened())
    }

    @Test
    fun `open reasons name what is still awaited`() {
        val (gate, _) = counting()
        gate.hold("mappings")
        gate.hold("database")
        assertEquals(listOf("mappings", "database"), gate.openReasons())
    }
}
