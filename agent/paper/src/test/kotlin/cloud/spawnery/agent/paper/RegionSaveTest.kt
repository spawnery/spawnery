package cloud.spawnery.agent.paper

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.util.concurrent.CompletableFuture
import java.util.concurrent.ExecutionException

class RegionSaveTest {
    private val retries = ArrayDeque<() -> Unit>()

    private fun RegionSave.offerWith(dead: () -> Boolean = { false }, propagate: () -> CompletableFuture<*>) =
        offer(dead, propagate) { retries.addLast(it) }

    private fun causeOf(save: RegionSave): Throwable =
        runCatching { save.get() }.exceptionOrNull().let { (it as ExecutionException).cause!! }

    @Test
    fun `a held ticket is offered again until the region takes it`() {
        val taken = CompletableFuture<Void>()
        var attempts = 0
        val save = RegionSave("4")
        save.offerWith {
            attempts++
            if (attempts < 3) throw IllegalStateException(RegionSave.TICKET_HELD)
            taken
        }
        while (retries.isNotEmpty()) retries.removeFirst()()

        assertEquals(3, attempts)
        assertFalse(save.isDone)
        taken.complete(null)
        assertTrue(save.isDone && !save.isCompletedExceptionally)
    }

    @Test
    fun `another IllegalStateException fails the save with that cause`() {
        val refused = IllegalStateException("not on the owning thread")
        val save = RegionSave("4")
        save.offerWith { throw refused }

        assertTrue(retries.isEmpty(), "retried")
        assertSame(refused, causeOf(save))
        assertNull(save.refusal)
    }

    @Test
    fun `any other exception fails the save with that cause`() {
        val broken = UnsupportedOperationException("no tickets here")
        val save = RegionSave("4")
        save.offerWith { throw broken }

        assertTrue(retries.isEmpty(), "retried")
        assertSame(broken, causeOf(save))
    }

    @Test
    fun `a dead region fails the save`() {
        val save = RegionSave("9")
        save.offerWith(dead = { true }) { error("never propagated") }

        assertTrue(causeOf(save).message!!.contains("region 9 merged or split"))
    }

    @Test
    fun `a held ticket is remembered as the refusal`() {
        val save = RegionSave("4")
        save.offerWith { throw IllegalStateException(RegionSave.TICKET_HELD) }

        assertEquals("region 4: ${RegionSave.TICKET_HELD}", save.refusal)
        assertEquals(1, retries.size)
    }

    @Test
    fun `a cancelled save is not offered again`() {
        var attempts = 0
        val save = RegionSave("4")
        save.offerWith {
            attempts++
            throw IllegalStateException(RegionSave.TICKET_HELD)
        }
        save.cancel(false)
        while (retries.isNotEmpty()) retries.removeFirst()()

        assertEquals(1, attempts)
    }
}
