package cloud.spawnery.agent.paper

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class FakeLevel

object FakeRegionIO {
    val flushed = mutableListOf<FakeLevel>()

    @JvmStatic
    fun flush(level: FakeLevel) {
        flushed += level
    }

    @JvmStatic
    fun flush(server: String) {
        error("the overload for the whole server was called")
    }
}

class RegionWritesTest {
    private val loader = javaClass.classLoader

    @Test
    fun `the chunk system's flush is waited on for each level`() {
        FakeRegionIO.flushed.clear()
        val writes = RegionWrites.locate(loader, FakeRegionIO::class.java.name, FakeLevel::class.java.name)
        val levels = listOf(FakeLevel(), FakeLevel())

        assertTrue(writes.available)
        writes.await(levels)

        assertEquals(levels, FakeRegionIO.flushed)
    }

    @Test
    fun `a server without that chunk system has nothing to wait on`() {
        val writes = RegionWrites.locate(loader, "no.such.RegionFileIO", FakeLevel::class.java.name)

        assertFalse(writes.available)
    }
}
