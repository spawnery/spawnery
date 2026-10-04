package cloud.spawnery.agent.paper

import cloud.spawnery.agent.heapNow
import cloud.spawnery.agent.AgentRole
import cloud.spawnery.agent.CloudConnector
import cloud.spawnery.agent.CloudEvents
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.Feed
import cloud.spawnery.agent.pb.AgentServiceGrpc
import cloud.spawnery.agent.pb.ExecuteCommand
import cloud.spawnery.agent.pb.Hello
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.PlayerCount
import cloud.spawnery.agent.pb.Ready
import cloud.spawnery.agent.pb.ServerMessage
import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.StreamObserver

class ServerRole(
    private val state: ServerState,
    private val mirror: NetworkMirror,
    private val connector: CloudConnector,
    private val feed: Feed,
    private val events: CloudEvents,
    private val execute: (ExecuteCommand) -> Unit = {},
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

    override fun playerCount(): ServerMessage {
        val (heapUsed, heapMax) = heapNow()
        return ServerMessage.newBuilder()
            .setPlayerCount(
                PlayerCount.newBuilder()
                    .setPlayers(state.players)
                    .setSlots(state.slots)
                    .setTps(state.tps)
                    .setMspt(state.mspt)
                    .setPlayableSlots(state.playable)
                    .setHeapUsedBytes(heapUsed)
                    .setHeapMaxBytes(heapMax),
            )
            .build()
    }

    /** Copied by hand as `FakeRole.asServerRoleWould`; nothing fails when the two drift. */
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
                mirror.apply(message.networkState)
                Directive.None
            }
            OperatorToServer.MessageCase.CLOUD_RESPONSE -> {
                connector.answer(message.cloudResponse)
                Directive.None
            }
            OperatorToServer.MessageCase.CLOUD_EVENT -> {
                feed.onEvent(message.cloudEvent)
                events.publish(message.cloudEvent)
                Directive.None
            }
            OperatorToServer.MessageCase.EXECUTE_COMMAND -> {
                execute(message.executeCommand)
                Directive.None
            }
            else -> Directive.None
        }

    /** Readiness itself rides on Hello. */
    fun ready(): ServerMessage =
        ServerMessage.newBuilder().setReady(Ready.getDefaultInstance()).build()
}
