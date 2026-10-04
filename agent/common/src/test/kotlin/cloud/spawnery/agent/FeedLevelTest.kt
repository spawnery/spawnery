package cloud.spawnery.agent

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNull

class FeedLevelTest {
    @Test
    fun `the four words and on are read whatever their case and spacing`() {
        assertEquals(FeedLevel.NORMAL, FeedLevel.parse(" Normal"))
        assertEquals(FeedLevel.VERBOSE, FeedLevel.parse("VERBOSE"))
        assertEquals(FeedLevel.OFF, FeedLevel.parse("off\n"))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.parse("minimal"))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.parse("on"))
    }

    @Test
    fun `no value and an unreadable one both mean minimal`() {
        assertNull(FeedLevel.parse("loud"))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.of(null))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.of(""))
        assertEquals(FeedLevel.MINIMAL, FeedLevel.of("loud"))
    }

    @Test
    fun `a level's word is what parse reads back`() {
        for (level in FeedLevel.entries) assertEquals(level, FeedLevel.parse(level.word))
    }
}
