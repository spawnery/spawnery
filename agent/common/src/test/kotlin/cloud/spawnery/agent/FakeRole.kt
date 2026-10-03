package cloud.spawnery.agent

import cloud.spawnery.agent.pb.AgentServiceGrpc
import cloud.spawnery.agent.pb.Hello
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.PlayerCount
import cloud.spawnery.agent.pb.Ready
import cloud.spawnery.agent.pb.ServerMessage
import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.StreamObserver
import java.util.Collections
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger

/**
 * The role [SessionLoop]'s own tests drive the loop with: the real wire types,
 * a record of what the loop asked for, and a [decide] hook to dictate a
 * [Directive]. Its counters are its own because `ServerState` is in `:paper`.
 */
class FakeRole(
    private val decide: (OperatorToServer) -> Directive = ::asServerRoleWould,
) : AgentRole<ServerMessage, OperatorToServer> {
    private val readyFlag = AtomicBoolean(false)
    private val playerCount = AtomicInteger(0)
    private val slotCount = AtomicInteger(0)

    val hellos: MutableList<ServerMessage> = Collections.synchronizedList(mutableListOf())
    val reports: MutableList<ServerMessage> = Collections.synchronizedList(mutableListOf())

    val directives: MutableList<Directive> = Collections.synchronizedList(mutableListOf())

    val ready: Boolean get() = readyFlag.get()
    val players: Int get() = playerCount.get()
    val slots: Int get() = slotCount.get()

    fun markReady(): Boolean = readyFlag.compareAndSet(false, true)

    fun sample(players: Int, slots: Int) {
        playerCount.set(players)
        slotCount.set(slots)
    }

    override fun open(
        channel: ManagedChannel,
        credentials: CallCredentials,
        observer: StreamObserver<OperatorToServer>,
    ): StreamObserver<ServerMessage> =
        AgentServiceGrpc.newStub(channel).withCallCredentials(credentials).serverSession(observer)

    override fun hello(version: String): ServerMessage =
        ServerMessage.newBuilder()
            .setHello(Hello.newBuilder().setVersion(version).setReady(ready))
            .build()
            .also { hellos.add(it) }

    override fun playerCount(): ServerMessage =
        ServerMessage.newBuilder()
            .setPlayerCount(PlayerCount.newBuilder().setPlayers(players).setSlots(slots))
            .build()
            .also { reports.add(it) }

    override fun onMessage(message: OperatorToServer): Directive =
        decide(message).also { directives.add(it) }

    /** Readiness itself rides on Hello. */
    fun ready(): ServerMessage =
        ServerMessage.newBuilder().setReady(Ready.getDefaultInstance()).build()
}

/**
 * A hand-maintained copy of `ServerRole.onMessage`, which is in `:paper`.
 * Nothing enforces the copy: a case added there and not here fails no test.
 * NETWORK_STATE falls to `else` because ServerRole's branch also returns
 * Directive.None; its effect is on the mirror, which this does not model.
 */
private fun asServerRoleWould(message: OperatorToServer): Directive =
    when (message.messageCase) {
        OperatorToServer.MessageCase.REPORT_INTERVAL ->
            Directive.Report(message.reportInterval.seconds)
        OperatorToServer.MessageCase.SESSION_DEADLINE ->
            Directive.Deadline(
                message.sessionDeadline.renewAfterSeconds,
                message.sessionDeadline.hardDeadlineSeconds,
            )
        else -> Directive.None
    }
