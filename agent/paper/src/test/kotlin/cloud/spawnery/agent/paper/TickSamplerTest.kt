package cloud.spawnery.agent.paper

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertInstanceOf
import org.junit.jupiter.api.Test

class TickSamplerTest {
    private val warnings = mutableListOf<Throwable>()
    private val samples = mutableListOf<Pair<Double, Double>>()

    private fun sampler(ticks: () -> TickSample) =
        TickSampler(ticks, { tps, mspt -> samples += tps to mspt }) { warnings += it }

    @Test
    fun `a reading is passed on`() {
        sampler { TickSample(19.5, 12.0) }.sample()

        assertEquals(listOf(19.5 to 12.0), samples)
    }

    @Test
    fun `a fork without the tick API is warned about once and does not throw`() {
        val sampler = sampler { throw NoSuchMethodError("org.bukkit.Server.getTPS()") }
        sampler.sample()
        sampler.sample()

        assertEquals(1, warnings.size)
        assertInstanceOf(NoSuchMethodError::class.java, warnings.single())
    }

    @Test
    fun `an exception is warned about once and does not throw`() {
        val sampler = sampler { throw IllegalStateException("not yet") }
        sampler.sample()
        sampler.sample()

        assertEquals(1, warnings.size)
    }
}
