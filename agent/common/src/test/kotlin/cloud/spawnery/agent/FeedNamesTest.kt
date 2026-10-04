package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import kotlin.test.Test
import kotlin.test.assertEquals

class FeedNamesTest {
    @Test
    fun `an on-demand member with a long key shows its group and six characters`() {
        assertEquals("challenge-3f2b1c", shortName("challenge-3f2b1c9a0d4e", "challenge", Group.Kind.ON_DEMAND))
    }

    @Test
    fun `a key of six characters or fewer is shown whole`() {
        assertEquals("challenge-3f2b1c", shortName("challenge-3f2b1c", "challenge", Group.Kind.ON_DEMAND))
        assertEquals("challenge-ab", shortName("challenge-ab", "challenge", Group.Kind.ON_DEMAND))
    }

    @Test
    fun `nothing else is shortened`() {
        assertEquals("lobby-x7k2abcdef", shortName("lobby-x7k2abcdef", "lobby", Group.Kind.EPHEMERAL))
        assertEquals("challenge", shortName("challenge", "challenge", Group.Kind.ON_DEMAND))
        assertEquals("other-3f2b1c9a0d", shortName("other-3f2b1c9a0d", "challenge", Group.Kind.ON_DEMAND))
        assertEquals("challenge-3f2b1c9a", shortName("challenge-3f2b1c9a", "challenge", Group.Kind.UNKNOWN))
    }
}
