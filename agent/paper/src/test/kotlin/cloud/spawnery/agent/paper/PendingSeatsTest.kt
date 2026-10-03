package cloud.spawnery.agent.paper

import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals

class PendingSeatsTest {
    private var now = 0L
    private val seats = PendingSeats(clock = { now }, ttlMillis = 1_000)
    private val player = UUID.randomUUID()

    @Test
    fun `an admitted player holds a seat`() {
        seats.admit(player)
        assertEquals(1, seats.count())
    }

    // A connection can close in the configuration phase, before any join or quit.
    @Test
    fun `a released player holds none`() {
        seats.admit(player)
        seats.release(player)
        assertEquals(0, seats.count())
    }

    @Test
    fun `a seat nobody released expires`() {
        seats.admit(player)
        now = 1_001
        assertEquals(0, seats.count())
    }

    @Test
    fun `admitting twice holds one seat`() {
        seats.admit(player)
        seats.admit(player)
        assertEquals(1, seats.count())
    }
}
