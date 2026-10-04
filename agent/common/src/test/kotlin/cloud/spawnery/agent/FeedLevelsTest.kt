package cloud.spawnery.agent

import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNull
import kotlin.test.assertTrue

class FeedLevelsTest {
    private val admin = UUID.nameUUIDFromBytes("admin".toByteArray())
    private val other = UUID.nameUUIDFromBytes("other".toByteArray())

    @Test
    fun `without a value a player is minimal`() {
        assertEquals(FeedLevel.MINIMAL, FeedLevels(null).level(admin))
        assertEquals(FeedLevel.MINIMAL, FeedLevels(StandInStore()).level(admin))
    }

    @Test
    fun `a value the player inherits counts, until the player sets their own`() {
        val store = StandInStore()
        store.group[admin] = "normal"
        val levels = FeedLevels(store)
        assertEquals(FeedLevel.NORMAL, levels.level(admin))

        levels.set(admin, FeedLevel.VERBOSE)
        assertEquals("verbose", store.user[admin])
        assertEquals(FeedLevel.VERBOSE, levels.level(admin))
    }

    @Test
    fun `a value written by hand is read whatever its case and spacing`() {
        val store = StandInStore()
        val levels = FeedLevels(store)
        store.user[admin] = " Normal\n"
        assertEquals(FeedLevel.NORMAL, levels.level(admin))
        store.user[admin] = "loud"
        assertEquals(FeedLevel.MINIMAL, levels.level(admin))
        store.user[admin] = ""
        assertEquals(FeedLevel.MINIMAL, levels.level(admin))
    }

    @Test
    fun `on is stored as minimal`() {
        val store = StandInStore()
        FeedLevels(store).set(admin, FeedLevel.parse("on")!!)
        assertEquals("minimal", store.user[admin])
    }

    @Test
    fun `a store that is not loaded leaves the level to memory`() {
        val store = StandInStore(loaded = false)
        val levels = FeedLevels(store)

        levels.set(admin, FeedLevel.OFF)

        assertEquals(FeedLevel.OFF, levels.level(admin))
        assertTrue(store.user.isEmpty(), "an unloaded store was written: ${store.user}")
        assertFalse(levels.kept())
        assertTrue(FeedLevels(StandInStore()).kept())
        assertFalse(FeedLevels(null).kept())
    }

    @Test
    fun `a store that throws on read means minimal, not a broken feed`() {
        val store = StandInStore()
        store.user[admin] = "verbose"
        store.failReads = true
        assertEquals(FeedLevel.MINIMAL, FeedLevels(store).level(admin))
    }

    @Test
    fun `one player's level is not another's`() {
        val levels = FeedLevels(null)
        levels.set(admin, FeedLevel.OFF)
        assertEquals(FeedLevel.OFF, levels.level(admin))
        assertEquals(FeedLevel.MINIMAL, levels.level(other))
    }

    @Test
    fun `without LuckPerms on the classpath there is no durable store`() {
        assertNull(LuckPermsFeedLevels.storeIfPresent())
    }
}
