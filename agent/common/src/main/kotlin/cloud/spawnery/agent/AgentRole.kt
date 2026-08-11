package cloud.spawnery.agent

import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.StreamObserver

/**
 * Everything about one agent's stream that differs between the two roles.
 *
 * Deliberately one method for incoming messages rather than a classify/apply
 * pair: two readers of the same messageCase in two files is how the two halves
 * drift.
 */
interface AgentRole<Req, Resp> {
    /** Which rpc this role speaks. */
    fun open(
        channel: ManagedChannel,
        credentials: CallCredentials,
        observer: StreamObserver<Resp>,
    ): StreamObserver<Req>

    /** The first message on every stream. */
    fun hello(version: String): Req

    /** The periodic report. */
    fun playerCount(): Req

    /**
     * Applies the role-specific effect of one operator message and returns
     * what the loop itself must act on. Returning [Directive.None] is the
     * normal outcome for a role-specific message and for one this agent does
     * not recognise.
     */
    fun onMessage(message: Resp): Directive
}

sealed interface Directive {
    data class Report(val seconds: Int) : Directive
    data class Deadline(val renewAfterSeconds: Int, val hardDeadlineSeconds: Int) : Directive
    data object None : Directive
}
