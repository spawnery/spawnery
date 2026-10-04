package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertTrue

private fun event(kind: String, warning: Boolean = false): CloudEvent =
    CloudEvent.newBuilder().setKind(kind).setSubject("lobby-a").setGroup("lobby").setWarning(warning).build()

class EventLevelsTest {
    @Test
    fun `a server's start, ready gate, leaving and end each have a row`() {
        assertEquals(Row.CREATED, row("PodCreated"))
        assertEquals(Row.READY, row("ReadyGatePassed"))
        assertEquals(Row.LEAVING, row("Retiring"))
        assertEquals(Row.LEAVING, row("DeletionRequested"))
        assertEquals(Row.LEAVING, row("RoundFinished"))
        assertEquals(Row.GONE, row("ServerStopped"))
    }

    @Test
    fun `a proxy's start, leaving and end each have a row`() {
        assertEquals(Row.ARRIVED, row("ProxyStarted"))
        assertEquals(Row.LEAVING, row("ProxyRetiring"))
        assertEquals(Row.GONE, row("ProxyStopped"))
    }

    @Test
    fun `a group's own events and kinds this agent does not know are other`() {
        assertEquals(Row.OTHER, row("ServerCreated"))
        assertEquals(Row.OTHER, row("PodPending"))
        assertEquals(Row.OTHER, row("SomethingAddedInALaterRelease"))
    }

    @Test
    fun `a failure the operator records as Normal is still a warning`() {
        assertTrue(isWarning(event("StartupTimeout")))
        assertTrue(isWarning(event("PodLost")))
        assertTrue(isWarning(event("AnythingElse", warning = true)))
        assertFalse(isWarning(event("PodCreated")))
    }

    @Test
    fun `a known warning reads from the table`() {
        assertEquals("did not start in time", shortReason("StartupTimeout"))
        assertEquals("pod refused", shortReason("ServerPodRejected"))
        assertEquals("killed", shortReason("PodKilled"))
    }

    @Test
    fun `an unknown warning is read from its name, so none is dropped`() {
        assertEquals("pod name conflict", shortReason("PodNameConflict"))
        assertEquals("tls handshake failed", shortReason("TLSHandshakeFailed"))
        assertEquals("warning", shortReason(""))
    }
}
