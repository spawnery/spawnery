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
     * Anything else this role sends on the same tick as [playerCount], in
     * order after it.
     *
     * A list with a default rather than a second required method, because only
     * one role has anything to add: the proxy reports which backends its
     * players are attached to, which is a fact a server agent cannot have --
     * it knows about itself and about nobody else. A default of nothing keeps
     * that asymmetry out of the Paper role entirely instead of making it
     * implement an empty method to say so.
     *
     * Called on the reporting timer, so it must not block: whatever it reads
     * has to be readable without waiting on the proxy's own threads, exactly
     * as [playerCount] is.
     */
    fun extraReports(): List<Req> = emptyList()

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
