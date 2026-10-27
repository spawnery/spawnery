package cloud.spawnery.agent.paper

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class RegionTicksTest {
    @Test
    fun `one lagging region among thirty idle ones is the report`() {
        val regions = List(29) { TickSample(20.0, 3.0) } + TickSample(8.0, 120.0)

        assertEquals(TickSample(8.0, 120.0), RegionTicks.worst(regions, TickSample(20.0, 1.0)))
    }

    @Test
    fun `tps and mspt may come from different regions`() {
        val regions = listOf(TickSample(19.0, 60.0), TickSample(12.0, 40.0))

        assertEquals(TickSample(12.0, 60.0), RegionTicks.worst(regions, TickSample(20.0, 1.0)))
    }

    @Test
    fun `no region reports the fallback`() {
        assertEquals(TickSample(20.0, 1.5), RegionTicks.worst(emptyList(), TickSample(20.0, 1.5)))
    }
}
