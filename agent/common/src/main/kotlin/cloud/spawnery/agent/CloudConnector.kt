package cloud.spawnery.agent

import cloud.spawnery.agent.api.ConnectResult
import cloud.spawnery.agent.api.Target
import cloud.spawnery.agent.pb.CloudResponse
import cloud.spawnery.agent.pb.ConnectRequest
import cloud.spawnery.agent.pb.RequestError
import java.util.UUID
import java.util.concurrent.CompletableFuture
import java.util.concurrent.CompletionStage

/**
 * Turns a plugin's `connect` call into a request on the wire, and the answer
 * back into what the plugin waits on.
 *
 * **Shared between the two platforms, with exactly one line of platform in
 * it**: [sendRequest], which wraps a [ConnectRequest] in whichever message
 * this side speaks. Everything else -- correlation, the failure mapping, what
 * a renewal does -- lives here once, because two copies would come to disagree
 * about what a plugin sees and that difference is the one thing this API
 * promises does not exist.
 *
 * @param sendRequest wraps the request in a ProxyMessage or a ServerMessage
 *   and hands it to the session loop. The whole platform seam.
 */
class CloudConnector(
    private val requests: Requests,
    private val sendRequest: (Long, ConnectRequest) -> Unit,
) {
    fun connect(player: UUID, to: Target): CompletionStage<ConnectResult> =
        requests.start<ConnectResult> { id ->
            val request = ConnectRequest.newBuilder().setPlayerUuid(player.toString())
            when (to) {
                is Target.Server -> request.setServer(to.name())
                is Target.Group -> request.setGroup(to.name())
            }
            sendRequest(id, request.build())
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
    CloudConnector(Requests(timeoutMillis = 1, clock = { 0L })) { _, _ ->
        throw IllegalStateException("this agent has no session to the operator")
    }
