package cloud.spawnery.agent

import kotlin.test.Test
import kotlin.test.assertEquals

class EventDirectionTest {
    @Test
    fun `a server arriving reads as added`() {
        assertEquals(Direction.ADDED, direction("ReadyGatePassed"))
        assertEquals(Direction.ADDED, direction("PodRunning"))
        assertEquals(Direction.ADDED, direction("JoinsOpen"))
    }

    @Test
    fun `a server leaving reads as removed`() {
        assertEquals(Direction.REMOVED, direction("Retiring"))
        assertEquals(Direction.REMOVED, direction("Terminating"))
        assertEquals(Direction.REMOVED, direction("JoinsClosed"))
        assertEquals(Direction.REMOVED, direction("ReadinessLost"))
    }

    @Test
    fun `the two doors face opposite ways`() {
        // JoinsOpen and JoinsClosed are the one pair a table like this is most
        // likely to get wrong, because they differ by one word.
        assertEquals(Direction.ADDED, direction("JoinsOpen"))
        assertEquals(Direction.REMOVED, direction("JoinsClosed"))
    }

    @Test
    fun `a kind nobody classified is neutral rather than a guess`() {
        assertEquals(Direction.NEUTRAL, direction("PodPending"))
        assertEquals(Direction.NEUTRAL, direction("RotationStarted"))
        // The case that matters: an operator newer than this agent.
        assertEquals(Direction.NEUTRAL, direction("SomethingAddedInALaterRelease"))
    }
}
