package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudRequest
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/**
 * What the connector does about a description outliving the session that
 * carried it.
 *
 * The operator remembers an announcement for as long as it has the session
 * that made it. An operator that restarts has therefore forgotten every
 * description on the network while every game that published one is still
 * running and has no reason to publish it again -- so the agent restates it,
 * exactly as readiness and event interest are restated, and for the same
 * reason.
 */
class CloudConnectorTest {
    private val requested = mutableListOf<CloudRequest>()

    private fun connector() = CloudConnector(
        Requests(timeoutMillis = 1_000, clock = System::currentTimeMillis),
    ) { request -> requested += request }

    @Test
    fun `a new stream is told again what this server said it was`() {
        val connector = connector()
        connector.announce("running", mapOf("map" to "arena"))
        requested.clear()

        connector.onStreamChanged()

        assertEquals(1, requested.size)
        assertEquals("running", requested[0].announce.state)
        assertEquals("arena", requested[0].announce.attributesMap["map"])
    }

    @Test
    fun `the restated announcement is the newest one and not every one`() {
        val connector = connector()
        connector.announce("waiting", emptyMap())
        connector.announce("running", emptyMap())
        requested.clear()

        connector.onStreamChanged()

        assertEquals(1, requested.size)
        assertEquals("running", requested[0].announce.state)
    }

    @Test
    fun `a new stream is told again that this server's door is shut`() {
        // Sharper than the description: the operator's default for a session
        // it has never seen is open, so a closed door that went unrestated
        // would put players into a round that had already started.
        val connector = connector()
        connector.acceptJoins(false)
        requested.clear()

        connector.onStreamChanged()

        assertEquals(1, requested.size)
        assertEquals(false, requested[0].acceptJoins.accept)
    }

    @Test
    fun `a door that was opened again is restated as open`() {
        val connector = connector()
        connector.acceptJoins(false)
        connector.acceptJoins(true)
        requested.clear()

        connector.onStreamChanged()

        assertEquals(1, requested.size)
        assertEquals(true, requested[0].acceptJoins.accept)
    }

    @Test
    fun `a server that never spoke about its door says nothing about it`() {
        // Never having spoken is not the same as having said "open": there is
        // nothing to restate, and the operator's own default already agrees.
        val connector = connector()
        connector.announce("running", emptyMap())
        requested.clear()

        connector.onStreamChanged()

        assertEquals(1, requested.size)
        assertTrue(requested.none { it.hasAcceptJoins() })
    }

    @Test
    fun `a server that never announced says nothing on a new stream`() {
        // Never having described itself is not the same as having described
        // itself as nothing, and only the second is worth a message.
        val connector = connector()

        connector.onStreamChanged()

        assertTrue(requested.isEmpty())
    }

    @Test
    fun `a cleared description is restated as cleared`() {
        // The empty announcement is a description like any other: a game that
        // finished and said so must not come back, after a reconnect, still
        // claiming to be running.
        val connector = connector()
        connector.announce("running", mapOf("map" to "arena"))
        connector.announce("", emptyMap())
        requested.clear()

        connector.onStreamChanged()

        assertEquals(1, requested.size)
        assertEquals("", requested[0].announce.state)
        assertTrue(requested[0].announce.attributesMap.isEmpty())
    }

    @Test
    fun `an announcement that could not be sent is still what the next stream carries`() {
        // Remembered before it is sent, so a send that failed because there
        // was no session is exactly the one whose replacement should carry it.
        val failing = CloudConnector(
            Requests(timeoutMillis = 1_000, clock = System::currentTimeMillis),
        ) { throw IllegalStateException("this agent has no session to the operator") }
        // The failure reaches the caller's stage rather than this call.
        failing.announce("running", emptyMap())

        val sent = mutableListOf<CloudRequest>()
        val reconnected = CloudConnector(
            Requests(timeoutMillis = 1_000, clock = System::currentTimeMillis),
        ) { request -> sent += request }
        reconnected.announce("running", emptyMap())
        sent.clear()
        reconnected.onStreamChanged()

        assertEquals(1, sent.size)
    }
}
