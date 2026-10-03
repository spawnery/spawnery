package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.pb.CloudRequest
import cloud.spawnery.agent.pb.CloudResponse
import cloud.spawnery.agent.pb.DeleteServerResult
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.RequestError
import cloud.spawnery.agent.pb.StartServerResult
import cloud.spawnery.agent.pb.StatusResult
import cloud.spawnery.agent.pb.StopServerResult
import java.time.Duration
import java.util.OptionalDouble
import java.util.concurrent.CompletionException
import java.util.concurrent.ExecutionException
import java.util.concurrent.TimeUnit
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

/**
 * The operator forgets an announcement with the session that made it, so the
 * agent restates it on every new stream.
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
        // Unrestated, the server would be routed again and its pod recorded
        // as Failed rather than Finished.
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
        val connector = connector()
        connector.announce("running", emptyMap())
        requested.clear()

        connector.onStreamChanged()

        assertEquals(1, requested.size)
        assertTrue(requested.none { it.hasAcceptJoins() })
    }

    @Test
    fun `a server that never announced says nothing on a new stream`() {
        val connector = connector()

        connector.onStreamChanged()

        assertTrue(requested.isEmpty())
    }

    @Test
    fun `a cleared description is restated as cleared`() {
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
    fun `a delete is sent with the group and the key`() {
        val connector = connector()

        connector.deleteServer("private-servers", "c0ffee")

        val sent = requested.single().deleteServer
        assertEquals("private-servers", sent.group)
        assertEquals("c0ffee", sent.key)
    }

    @Test
    fun `a delete answer completes with no value`() {
        val connector = connector()
        val future = connector.deleteServer("private-servers", "c0ffee")

        connector.answer(
            CloudResponse.newBuilder()
                .setId(requested.single().id)
                .setDeleteServer(DeleteServerResult.newBuilder().setServer("private-servers-c0ffee").setWorld(true))
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

    // The shape SpawneryApi.startServer's Javadoc describes.
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

    // Tests the JDK rather than this code: it pins the getCause() advice in
    // SpawneryApi.startServer's Javadoc.
    @Test
    fun `a dependent stage sees the exception wrapped in a CompletionException`() {
        val seen = refusedStart()
            .thenApply { it.name() }
            .handle { _, failure -> failure }
            .toCompletableFuture().get(1, TimeUnit.SECONDS)

        assertTrue(seen is CompletionException, "$seen")
        assertTrue(seen.cause is IllegalStateException, "${seen.cause}")
    }

    @Test
    fun `status asks with the target and turns the answer into records`() {
        val connector = connector()
        val future = connector.status("lobby")
        assertEquals("lobby", requested.single().status.target)

        connector.answer(
            CloudResponse.newBuilder().setId(requested.single().id).setStatus(
                StatusResult.newBuilder()
                    .setMetricsAvailable(true)
                    .setTotal(pbUsage(cpuUsed = 400, pods = 2, measured = 1))
                    .addGroups(
                        cloud.spawnery.agent.pb.GroupStatus.newBuilder()
                            .setName("lobby").setKind(GroupState.Kind.EPHEMERAL).setPhase("Ready")
                            .setReplicas(2).setReadyReplicas(2).setPlayers(5).setLowestTps(16.5)
                            .setUsage(pbUsage(cpuUsed = 400, pods = 2, measured = 1)),
                    )
                    .addInstances(
                        cloud.spawnery.agent.pb.InstanceStatus.newBuilder()
                            .setName("lobby-a").setGroup("lobby").setPhase("Ready").setReady(true)
                            .setPlayers(2).setSlots(20).setTps(0.0).setMspt(0.0).setAgeSeconds(5400)
                            .setUsage(pbUsage(cpuUsed = 0, pods = 1, measured = 0)),
                    ),
            ).build(),
        )

        val status = future.toCompletableFuture().get(1, TimeUnit.SECONDS)
        assertTrue(status.metricsAvailable())
        assertEquals(1, status.total().podsMeasured())
        assertFalse(status.total().complete())
        val group = status.groups().single()
        assertEquals(Group.Kind.EPHEMERAL, group.kind())
        assertEquals(OptionalDouble.of(16.5), group.lowestTps())
        val instance = status.instances().single()
        assertEquals(OptionalDouble.empty(), instance.tps(), "0 on the wire is not a TPS of zero")
        assertEquals(OptionalDouble.empty(), instance.mspt())
        assertEquals(Duration.ofSeconds(5400), instance.age())
        assertFalse(instance.usage().measured())
        assertEquals(0, status.other().pods(), "an absent other is an empty usage, not null")
    }

    private fun pbUsage(cpuUsed: Long, pods: Int, measured: Int) =
        cloud.spawnery.agent.pb.ResourceUsage.newBuilder()
            .setCpuUsedMillicores(cpuUsed).setPods(pods).setPodsMeasured(measured).build()
}
