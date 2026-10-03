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
        assertEquals(Direction.REMOVED, direction("RoundFinished"))
        assertEquals(Direction.REMOVED, direction("ReadinessLost"))
        assertEquals(Direction.REMOVED, direction("RetentionElapsed"))
        assertEquals(Direction.REMOVED, direction("FinishedRetentionElapsed"))
    }

    @Test
    fun `the two doors face opposite ways`() {
        // Direction is reachability, not the join door: JoinsOpen registers,
        // RoundFinished deregisters, JoinsClosed does neither.
        assertEquals(Direction.ADDED, direction("JoinsOpen"))
        assertEquals(Direction.REMOVED, direction("RoundFinished"))
    }

    @Test
    fun `a kind nobody classified is neutral rather than a guess`() {
        assertEquals(Direction.NEUTRAL, direction("PodPending"))
        assertEquals(Direction.NEUTRAL, direction("RotationStarted"))
        // An operator newer than this agent.
        assertEquals(Direction.NEUTRAL, direction("SomethingAddedInALaterRelease"))
    }
}
