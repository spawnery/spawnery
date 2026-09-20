package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudRequest
import cloud.spawnery.agent.pb.CloudResponse
import cloud.spawnery.agent.pb.RequestError
import cloud.spawnery.agent.pb.StartServerResult
import cloud.spawnery.agent.pb.StopServerResult
import java.util.concurrent.CompletionException
import java.util.concurrent.ExecutionException
import java.util.concurrent.TimeUnit
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
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
    fun `endRound closes the door and says the round is over`() {
        val connector = connector()

        connector.endRound()

        assertEquals(1, requested.size)
        assertEquals(false, requested[0].acceptJoins.accept)
        assertTrue(requested[0].acceptJoins.roundEnded)
    }

    @Test
    fun `a new stream is told again that this server's round has ended`() {
        // The sharper of the two defaults: the operator's default for a
        // session it has never seen is that the round has not ended, so an
        // ended round that went unrestated would put the server back in the
        // routing table and record its pod as Failed rather than Finished.
        val connector = connector()
        connector.endRound()
        requested.clear()

        connector.onStreamChanged()

        assertEquals(1, requested.size)
        assertEquals(false, requested[0].acceptJoins.accept)
        assertTrue(requested[0].acceptJoins.roundEnded)
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

    @Test
    fun `a start is sent with the group and the key`() {
        val connector = connector()

        connector.startServer("private-servers", "c0ffee")

        assertEquals("private-servers", requested.single().startServer.group)
        assertEquals("c0ffee", requested.single().startServer.key)
    }

    @Test
    fun `a start answer completes with the composed name`() {
        val connector = connector()
        val future = connector.startServer("private-servers", "c0ffee")

        connector.answer(
            CloudResponse.newBuilder()
                .setId(requested.single().id)
                .setStartServer(
                    StartServerResult.newBuilder()
                        .setServer("private-servers-c0ffee")
                        .setAlreadyRunning(true),
                )
                .build(),
        )

        val started = future.toCompletableFuture().get(1, TimeUnit.SECONDS)
        assertEquals("private-servers-c0ffee", started.name())
        assertTrue(started.alreadyRunning())
    }

    @Test
    fun `a start that made the server says it was not already running`() {
        val connector = connector()
        val future = connector.startServer("private-servers", "c0ffee")

        connector.answer(
            CloudResponse.newBuilder()
                .setId(requested.single().id)
                .setStartServer(StartServerResult.newBuilder().setServer("private-servers-c0ffee"))
                .build(),
        )

        assertEquals(false, future.toCompletableFuture().get(1, TimeUnit.SECONDS).alreadyRunning())
    }

    @Test
    fun `a stop is sent with the server's name`() {
        val connector = connector()

        connector.stopServer("private-servers-c0ffee")

        assertEquals("private-servers-c0ffee", requested.single().stopServer.server)
    }

    @Test
    fun `a stop answer completes with no value`() {
        val connector = connector()
        val future = connector.stopServer("private-servers-c0ffee")

        connector.answer(
            CloudResponse.newBuilder()
                .setId(requested.single().id)
                .setStopServer(StopServerResult.newBuilder().setServer("private-servers-c0ffee"))
                .build(),
        )

        assertEquals(null, future.toCompletableFuture().get(1, TimeUnit.SECONDS))
    }

    @Test
    fun `a refused start fails the stage with the reason and the operator's words`() {
        val connector = connector()
        val future = connector.startServer("lobby", "c0ffee")

        connector.answer(
            CloudResponse.newBuilder()
                .setId(requested.single().id)
                .setError(
                    RequestError.newBuilder()
                        .setReason(RequestError.Reason.REFUSED)
                        .setMessage("that group is at spec.maxInstances"),
                )
                .build(),
        )

        val failure = assertFailsWith<ExecutionException> { future.toCompletableFuture().get(1, TimeUnit.SECONDS) }
        assertTrue(failure.cause is IllegalStateException, "${failure.cause}")
        assertEquals("REFUSED: that group is at spec.maxInstances", failure.cause!!.message)
    }

    @Test
    fun `a refused stop fails the stage with the reason and the operator's words`() {
        val connector = connector()
        val future = connector.stopServer("lobby-a")

        connector.answer(
            CloudResponse.newBuilder()
                .setId(requested.single().id)
                .setError(
                    RequestError.newBuilder()
                        .setReason(RequestError.Reason.REFUSED)
                        .setMessage("that server is not a member of an on-demand group"),
                )
                .build(),
        )

        val failure = assertFailsWith<ExecutionException> { future.toCompletableFuture().get(1, TimeUnit.SECONDS) }
        assertTrue(failure.cause is IllegalStateException, "${failure.cause}")
        assertEquals(
            "REFUSED: that server is not a member of an on-demand group",
            failure.cause!!.message,
        )
    }

    // The Javadoc on startServer tells plugin authors when to unwrap, so the
    // shape it describes is pinned here rather than inferred.
    private fun refusedStart(): java.util.concurrent.CompletionStage<cloud.spawnery.agent.api.StartedServer> {
        val connector = connector()
        val stage = connector.startServer("lobby", "c0ffee")
        connector.answer(
            CloudResponse.newBuilder()
                .setId(requested.last().id)
                .setError(
                    RequestError.newBuilder()
                        .setReason(RequestError.Reason.REFUSED)
                        .setMessage("that group is at spec.maxInstances"),
                )
                .build(),
        )
        return stage
    }

    @Test
    fun `handle, exceptionally and whenComplete on the stage itself see the bare exception`() {
        var byHandle: Throwable? = null
        var byExceptionally: Throwable? = null
        var byWhenComplete: Throwable? = null
        refusedStart().handle { _, failure -> byHandle = failure }
        refusedStart().exceptionally { failure -> byExceptionally = failure; null }
        refusedStart().whenComplete { _, failure -> byWhenComplete = failure }

        for (seen in listOf(byHandle, byExceptionally, byWhenComplete)) {
            assertTrue(seen is IllegalStateException, "$seen")
            assertEquals(null, seen.cause)
        }
    }

    @Test
    fun `a dependent stage sees the exception wrapped in a CompletionException`() {
        val seen = refusedStart()
            .thenApply { it.name() }
            .handle { _, failure -> failure }
            .toCompletableFuture().get(1, TimeUnit.SECONDS)

        assertTrue(seen is CompletionException, "$seen")
        assertTrue(seen.cause is IllegalStateException, "${seen.cause}")
    }
}
