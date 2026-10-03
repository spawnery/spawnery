package cloud.spawnery.agent

import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.StreamObserver

/** One method for incoming messages rather than a classify/apply pair, so two readers of messageCase cannot drift. */
interface AgentRole<Req, Resp> {
    fun open(
        channel: ManagedChannel,
        credentials: CallCredentials,
        observer: StreamObserver<Resp>,
    ): StreamObserver<Req>

    /** The first message on every stream. */
    fun hello(version: String): Req

    /** The periodic report. */
    fun playerCount(): Req

    /** Sent after [playerCount] on the same tick, on the reporting timer, so it must not block. */
    fun extraReports(): List<Req> = emptyList()

    /** Returns what the loop itself must act on; [Directive.None] also for a message this agent does not recognise. */
    fun onMessage(message: Resp): Directive
}

sealed interface Directive {
    data class Report(val seconds: Int) : Directive
    data class Deadline(val renewAfterSeconds: Int, val hardDeadlineSeconds: Int) : Directive
    data object None : Directive
}
