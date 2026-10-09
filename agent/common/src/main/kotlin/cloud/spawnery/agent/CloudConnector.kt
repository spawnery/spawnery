package cloud.spawnery.agent

import cloud.spawnery.agent.api.BoostResult
import cloud.spawnery.agent.api.ConnectResult
import cloud.spawnery.agent.api.StartedServer
import cloud.spawnery.agent.api.Target
import cloud.spawnery.agent.api.NetworkStatus
import cloud.spawnery.agent.api.RestorePoint
import cloud.spawnery.agent.api.RestoredWorld
import cloud.spawnery.agent.api.ScaleResult
import cloud.spawnery.agent.pb.AcceptJoinsRequest
import cloud.spawnery.agent.pb.AnnounceRequest
import cloud.spawnery.agent.pb.BoostRequest
import cloud.spawnery.agent.pb.CloudRequest
import cloud.spawnery.agent.pb.CloudResponse
import cloud.spawnery.agent.pb.ConnectRequest
import cloud.spawnery.agent.pb.DeleteServerRequest
import cloud.spawnery.agent.pb.ExecuteRequest
import cloud.spawnery.agent.pb.ForceStopRequest
import cloud.spawnery.agent.pb.ListRestorePointsRequest
import cloud.spawnery.agent.pb.RequestError
import cloud.spawnery.agent.pb.RestoreWorldRequest
import cloud.spawnery.agent.pb.RetireRequest
import cloud.spawnery.agent.pb.ScaleRequest
import cloud.spawnery.agent.pb.StartServerRequest
import cloud.spawnery.agent.pb.StatusRequest
import cloud.spawnery.agent.pb.StopBoostRequest
import cloud.spawnery.agent.pb.StopServerRequest
import cloud.spawnery.agent.pb.UnretireRequest
import java.time.Duration
import java.time.Instant
import java.util.UUID
import java.util.concurrent.CompletableFuture
import java.util.concurrent.atomic.AtomicReference
import java.util.concurrent.CompletionStage

/**
 * Shared between both platforms; [sendRequest] is the whole platform seam. It
 * takes a whole [CloudRequest] so the platforms never need to know which verbs
 * exist.
 */
class CloudConnector(
    private val requests: Requests,
    private val sendRequest: (CloudRequest) -> Unit,
) {
    fun connect(player: UUID, to: Target): CompletionStage<ConnectResult> =
        requests.start<ConnectResult> { id ->
            val request = ConnectRequest.newBuilder().setPlayerUuid(player.toString())
            when (to) {
                is Target.Server -> request.setServer(to.name())
                is Target.Group -> request.setGroup(to.name())
            }
            sendRequest(CloudRequest.newBuilder().setId(id).setConnect(request).build())
        }

    /** "Already retiring" fails rather than succeeding quietly; see the operator's RetireResult. */
    fun retire(server: String): CompletionStage<Void> =
        requests.start<Void> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setRetire(RetireRequest.newBuilder().setServer(server))
                    .build(),
            )
        }

    fun unretire(server: String): CompletionStage<Void> =
        requests.start<Void> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setUnretire(UnretireRequest.newBuilder().setServer(server))
                    .build(),
            )
        }

    fun status(target: String): CompletionStage<NetworkStatus> =
        requests.start<NetworkStatus> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setStatus(StatusRequest.newBuilder().setTarget(target))
                    .build(),
            )
        }

    /**
     * A duration, never an instant: the two sides do not share a clock. Null
     * sends zero, which the proto defines as "the operator decides".
     */
    fun boost(group: String, replicas: Int, forHowLong: Duration?): CompletionStage<BoostResult> =
        requests.start<BoostResult> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setBoost(
                        BoostRequest.newBuilder()
                            .setGroup(group)
                            .setReplicas(replicas)
                            .setDurationSeconds(forHowLong?.seconds ?: 0L),
                    )
                    .build(),
            )
        }

    /** The same duration rule as [boost]. */
    fun scale(group: String, replicas: Int, forHowLong: Duration?): CompletionStage<ScaleResult> =
        requests.start<ScaleResult> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setScale(
                        ScaleRequest.newBuilder()
                            .setGroup(group)
                            .setReplicas(replicas)
                            .setDurationSeconds(forHowLong?.seconds ?: 0L),
                    )
                    .build(),
            )
        }

    /** The operator's StopBoostRequest removes pins and boosts alike. */
    fun resetScale(group: String): CompletionStage<Int> = stopBoosts(group)

    fun forceStop(server: String, issuer: String): CompletionStage<String> =
        requests.start<String> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setForceStop(ForceStopRequest.newBuilder().setServer(server).setIssuer(issuer))
                    .build(),
            )
        }

    fun execute(target: String, command: String, issuer: String): CompletionStage<List<ExecuteLine>> =
        requests.start<List<ExecuteLine>> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setExecute(ExecuteRequest.newBuilder().setTarget(target).setCommand(command).setIssuer(issuer))
                    .build(),
            )
        }

    fun startServer(group: String, key: String): CompletionStage<StartedServer> =
        requests.start<StartedServer> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setStartServer(
                        StartServerRequest.newBuilder()
                            .setGroup(group)
                            .setKey(key),
                    )
                    .build(),
            )
        }

    /** Its world stays. */
    fun stopServer(server: String): CompletionStage<Void> =
        requests.start<Void> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setStopServer(StopServerRequest.newBuilder().setServer(server))
                    .build(),
            )
        }

    fun deleteServer(group: String, key: String): CompletionStage<Void> =
        requests.start<Void> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setDeleteServer(DeleteServerRequest.newBuilder().setGroup(group).setKey(key))
                    .build(),
            )
        }

    fun listRestorePoints(group: String, key: String): CompletionStage<List<RestorePoint>> =
        requests.start<List<RestorePoint>> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setListRestorePoints(ListRestorePointsRequest.newBuilder().setGroup(group).setKey(key))
                    .build(),
            )
        }

    fun restoreWorld(group: String, key: String, generation: Long): CompletionStage<RestoredWorld> =
        requests.start<RestoredWorld> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setRestoreWorld(
                        RestoreWorldRequest.newBuilder().setGroup(group).setKey(key).setGeneration(generation),
                    )
                    .build(),
            )
        }

    /**
     * Re-sent on every new stream: the operator forgets an announcement with
     * the session that made it. Null until something announces.
     */
    private val lastAnnouncement = AtomicReference<AnnounceRequest?>(null)

    /** The operator replaces rather than merges, so anything left out is taken back. */
    fun announce(state: String, attributes: Map<String, String>): CompletionStage<Void> {
        val announcement = AnnounceRequest.newBuilder()
            .setState(state)
            .putAllAttributes(attributes)
            .build()
        // Remembered before sending: a failed send is still this server's intent.
        lastAnnouncement.set(announcement)
        return send(announcement)
    }

    /**
     * Held as the built message, not two flags, so a restatement cannot
     * recombine `accept` and `round_ended` into a pair never sent. Null means
     * never asked, which matches the operator's defaults for a new session.
     */
    private val lastAcceptJoins = AtomicReference<AcceptJoinsRequest?>(null)

    /** Closing is not retiring: nobody is moved and it can be taken back. */
    fun acceptJoins(accept: Boolean): CompletionStage<Void> {
        val request = AcceptJoinsRequest.newBuilder().setAccept(accept).build()
        lastAcceptJoins.set(request)
        return send(request)
    }

    /**
     * A closed door and an ended round in one message: the operator ends the
     * round from the same field it deregisters on. See [SpawneryApi.endRound].
     */
    fun endRound(): CompletionStage<Void> {
        val request = AcceptJoinsRequest.newBuilder().setAccept(false).setRoundEnded(true).build()
        lastAcceptJoins.set(request)
        return send(request)
    }

    private fun send(request: AcceptJoinsRequest): CompletionStage<Void> =
        requests.start<Void> { id ->
            sendRequest(
                CloudRequest.newBuilder().setId(id).setAcceptJoins(request).build(),
            )
        }

    private fun send(announcement: AnnounceRequest): CompletionStage<Void> =
        requests.start<Void> { id ->
            sendRequest(CloudRequest.newBuilder().setId(id).setAnnounce(announcement).build())
        }

    fun stopBoosts(group: String): CompletionStage<Int> =
        requests.start<Int> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setStopBoost(StopBoostRequest.newBuilder().setGroup(group))
                    .build(),
            )
        }

    /** An answer for an id nobody holds is dropped by [Requests]; a late answer is ordinary. */
    fun answer(response: CloudResponse) {
        when {
            response.hasError() -> requests.fail(response.id, asException(response.error))
            response.hasConnect() -> requests.complete(
                response.id,
                ConnectResult(
                    response.connect.ordered,
                    response.connect.alreadyThere,
                    response.connect.target,
                ),
            )
            response.hasRetire() -> requests.complete(response.id, null)
            response.hasUnretire() -> requests.complete(response.id, null)
            response.hasStatus() -> requests.complete(response.id, toNetworkStatus(response.status))
            response.hasBoost() -> requests.complete(
                response.id,
                BoostResult(
                    response.boost.replicas,
                    Instant.ofEpochSecond(response.boost.expiresAtUnix),
                ),
            )
            response.hasStartServer() -> requests.complete(
                response.id,
                StartedServer(
                    response.startServer.server,
                    response.startServer.alreadyRunning,
                ),
            )
            response.hasStopServer() -> requests.complete(response.id, null)
            response.hasDeleteServer() -> requests.complete(response.id, null)
            response.hasListRestorePoints() -> requests.complete(
                response.id,
                response.listRestorePoints.pointsList.map {
                    RestorePoint(it.generation, Instant.ofEpochMilli(it.takenUnixMillis), it.current)
                },
            )
            response.hasRestoreWorld() -> requests.complete(
                response.id,
                RestoredWorld(response.restoreWorld.generation, response.restoreWorld.restoredFrom),
            )
            response.hasStopBoost() -> requests.complete(response.id, response.stopBoost.removed)
            response.hasAnnounce() -> requests.complete(response.id, null)
            response.hasAcceptJoins() -> requests.complete(response.id, null)
            response.hasScale() -> requests.complete(
                response.id,
                ScaleResult(response.scale.replicas, Instant.ofEpochSecond(response.scale.expiresAtUnix)),
            )
            response.hasForceStop() -> requests.complete(response.id, response.forceStop.server)
            response.hasExecute() -> requests.complete(
                response.id,
                response.execute.outcomesList.map { ExecuteLine(it.server, it.ok, it.outputList, it.error) },
            )
            // Failed rather than ignored, so the version skew has a name.
            else -> requests.fail(
                response.id,
                IllegalStateException("the operator answered with a result this agent does not know"),
            )
        }
    }

    fun onStreamChanged() {
        requests.failAll(IllegalStateException("the session was renewed while this request was in flight"))
        // Restated on the new stream: the operator forgets a session's
        // announcement and defaults to door open, round not ended.
        lastAnnouncement.get()?.let { send(it) }
        lastAcceptJoins.get()?.let { send(it) }
    }

    fun expire() = requests.expire()

    private fun asException(error: RequestError): Throwable =
        IllegalStateException("${error.reason}: ${error.message}")

    companion object {
        const val TIMEOUT_MILLIS: Long = 10_000
    }
}

fun dormantConnector(): CloudConnector =
    CloudConnector(Requests(timeoutMillis = 1, clock = { 0L })) { _ ->
        throw IllegalStateException("this agent has no session to the operator")
    }
