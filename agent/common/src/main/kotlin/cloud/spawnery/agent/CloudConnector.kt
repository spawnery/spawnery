package cloud.spawnery.agent

import cloud.spawnery.agent.api.BoostResult
import cloud.spawnery.agent.api.ConnectResult
import cloud.spawnery.agent.api.Target
import cloud.spawnery.agent.pb.CloudRequest
import cloud.spawnery.agent.pb.CloudResponse
import cloud.spawnery.agent.pb.ConnectRequest
import cloud.spawnery.agent.pb.BoostRequest
import cloud.spawnery.agent.pb.RetireRequest
import cloud.spawnery.agent.pb.StopBoostRequest
import cloud.spawnery.agent.pb.RequestError
import java.time.Duration
import java.time.Instant
import java.util.UUID
import java.util.concurrent.CompletableFuture
import java.util.concurrent.CompletionStage

/**
 * Turns a plugin's `connect` call into a request on the wire, and the answer
 * back into what the plugin waits on.
 *
 * **Shared between the two platforms, with exactly one line of platform in
 * it**: [sendRequest], which wraps a finished [CloudRequest] in whichever
 * message this side speaks. Everything else -- correlation, the failure
 * mapping, what a renewal does -- lives here once, because two copies would
 * come to disagree about what a plugin sees and that difference is the one
 * thing this API promises does not exist.
 *
 * The seam takes a whole CloudRequest rather than one verb's request, and that
 * is what keeps it one line as verbs are added: the platforms know how to put
 * a request on their own wire and nothing about which requests exist. The
 * first draft passed a ConnectRequest and made both plugins spell `setId` and
 * `setConnect` themselves, so retire would have had to be added in three
 * places, two of them platform files that have no business knowing the verb.
 *
 * @param sendRequest wraps the request in a ProxyMessage or a ServerMessage
 *   and hands it to the session loop. The whole platform seam.
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

    /**
     * Asks that one server stop taking joins and empty out.
     *
     * The future carries no value because the operator's answer carries none
     * worth passing on: it echoes the name the caller already has. What the
     * caller learns is which of the two things happened -- it completed, or it
     * failed with the operator's reason, and "that server is already retiring"
     * is a failure rather than a quiet success on purpose. See the operator's
     * own RetireResult for why.
     */
    fun retire(server: String): CompletionStage<Void> =
        requests.start<Void> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setRetire(RetireRequest.newBuilder().setServer(server))
                    .build(),
            )
        }

    /**
     * Asks for extra capacity on a group, for a while.
     *
     * A duration on the wire and never an instant: the two sides do not share
     * a clock, and an expiry computed here would be wrong by this pod's clock
     * error with nothing on either side able to see it. Null means the
     * operator's default, which is why zero is what goes on the wire -- the
     * proto documents zero as "the operator decides", and a duration this
     * agent invented would take that choice away from the side that owns it.
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

    /** Ends every boost on a group and reports how many there were. */
    fun stopBoosts(group: String): CompletionStage<Int> =
        requests.start<Int> { id ->
            sendRequest(
                CloudRequest.newBuilder()
                    .setId(id)
                    .setStopBoost(StopBoostRequest.newBuilder().setGroup(group))
                    .build(),
            )
        }

    /**
     * Routes an answer to whoever is waiting on it.
     *
     * An answer for an id nobody holds is dropped by [Requests], which is what
     * makes calling this from a gRPC callback safe: a late answer to a request
     * that already reached its deadline is ordinary, and throwing on one would
     * end the session and cost every other request outstanding.
     */
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
            response.hasBoost() -> requests.complete(
                response.id,
                BoostResult(
                    response.boost.replicas,
                    Instant.ofEpochSecond(response.boost.expiresAtUnix),
                ),
            )
            response.hasStopBoost() -> requests.complete(response.id, response.stopBoost.removed)
            // A result kind this agent does not know. Failed rather than
            // ignored: a plugin holding a future to its deadline learns
            // nothing, where a failure names the version skew.
            else -> requests.fail(
                response.id,
                IllegalStateException("the operator answered with a result this agent does not know"),
            )
        }
    }

    /** Fails everything outstanding. Called when this agent's stream changes. */
    fun onStreamChanged() {
        requests.failAll(IllegalStateException("the session was renewed while this request was in flight"))
    }

    /** Fails everything past its deadline. Called from the reporting timer. */
    fun expire() = requests.expire()

    private fun asException(error: RequestError): Throwable =
        // The reason and the message both, because a caller branches on one
        // and a person reads the other -- and a plugin author with only the
        // enum has nothing to put in a log.
        IllegalStateException("${error.reason}: ${error.message}")

    companion object {
        /** How long an unanswered request lives. */
        const val TIMEOUT_MILLIS: Long = 10_000
    }
}

/** A connector that refuses, for an agent with no session. */
fun dormantConnector(): CloudConnector =
    CloudConnector(Requests(timeoutMillis = 1, clock = { 0L })) { _ ->
        throw IllegalStateException("this agent has no session to the operator")
    }
