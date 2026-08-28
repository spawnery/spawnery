package cloud.spawnery.agent.paper

import cloud.spawnery.agent.AgentRole
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.pb.AgentServiceGrpc
import cloud.spawnery.agent.pb.Hello
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.PlayerCount
import cloud.spawnery.agent.pb.Ready
import cloud.spawnery.agent.pb.ServerMessage
import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.StreamObserver

/** Paper's half of the channel. */
class ServerRole(
    private val state: ServerState,
    /**
     * Where the operator's picture of the network is kept for the plugin API
     * to read.
     *
     * Last, and that position is deliberate: every call site in this package
     * passes positionally, so a parameter added above would rebind them --
     * silently, wherever the types happen to match.
     */
    private val mirror: NetworkMirror,
) : AgentRole<ServerMessage, OperatorToServer> {
    override fun open(
        channel: ManagedChannel,
        credentials: CallCredentials,
        observer: StreamObserver<OperatorToServer>,
    ): StreamObserver<ServerMessage> =
        AgentServiceGrpc.newStub(channel).withCallCredentials(credentials).serverSession(observer)

    override fun hello(version: String): ServerMessage =
        ServerMessage.newBuilder()
            .setHello(Hello.newBuilder().setVersion(version).setReady(state.ready))
            .build()

    override fun playerCount(): ServerMessage =
        ServerMessage.newBuilder()
            .setPlayerCount(
                PlayerCount.newBuilder().setPlayers(state.players).setSlots(state.slots),
            )
            .build()

    /**
     * `:common`'s test double copies this `when` by hand, as
     * `FakeRole.asServerRoleWould`, because `SessionLoopTest` drives the loop
     * from `:common` and cannot see this class. Nothing enforces the copy, so a
     * case added here has to be added there too. Nothing fails if it is not:
     * those tests assert what the two existing branches do, never which
     * branches exist, so they go on passing against a mapping that no longer
     * matches production.
     */
    override fun onMessage(message: OperatorToServer): Directive =
        when (message.messageCase) {
            OperatorToServer.MessageCase.REPORT_INTERVAL ->
                Directive.Report(message.reportInterval.seconds)
            OperatorToServer.MessageCase.SESSION_DEADLINE ->
                Directive.Deadline(
                    message.sessionDeadline.renewAfterSeconds,
                    message.sessionDeadline.hardDeadlineSeconds,
                )
            OperatorToServer.MessageCase.NETWORK_STATE -> {
                // The whole effect is the side effect. The loop is told
                // nothing, which is why FakeRole's hand copy of this mapping
                // needs no branch for it: `else -> Directive.None` is already
                // the right answer to the only question that copy models.
                mirror.apply(message.networkState)
                Directive.None
            }
            else -> Directive.None
        }

    /** The immediate readiness notification. Readiness itself rides on Hello. */
    fun ready(): ServerMessage =
        ServerMessage.newBuilder().setReady(Ready.getDefaultInstance()).build()
}
