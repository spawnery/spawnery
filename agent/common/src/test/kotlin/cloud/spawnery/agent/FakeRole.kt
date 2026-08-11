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
 * The role [SessionLoop]'s own tests drive the loop with.
 *
 * It speaks the real `ServerMessage`/`OperatorToServer` types and the real
 * `serverSession` rpc, so [FakeOperator] and every assertion about wire content
 * mean what they meant before the loop became generic. What it adds over the
 * production `ServerRole` is a record of what the loop asked it for, and a
 * [decide] hook so a test can dictate a [Directive] independently of which
 * message produced it — the seam the Velocity role will rely on.
 *
 * Its counters are its own rather than a `ServerState`. `ServerState` lives in
 * `:paper` now, and `:paper` depends on `:common`; reaching for it here would
 * reintroduce the project cycle the move exists to avoid.
 */
class FakeRole(
    private val decide: (OperatorToServer) -> Directive = ::asServerRoleWould,
) : AgentRole<ServerMessage, OperatorToServer> {
    private val readyFlag = AtomicBoolean(false)
    private val playerCount = AtomicInteger(0)
    private val slotCount = AtomicInteger(0)

    /** Every message this role built, in the order the loop asked for it. */
    val hellos: MutableList<ServerMessage> = Collections.synchronizedList(mutableListOf())
    val reports: MutableList<ServerMessage> = Collections.synchronizedList(mutableListOf())

    /** Every directive this role returned, including [Directive.None]. */
    val directives: MutableList<Directive> = Collections.synchronizedList(mutableListOf())

    val ready: Boolean get() = readyFlag.get()
    val players: Int get() = playerCount.get()
    val slots: Int get() = slotCount.get()

    /** Returns true only for the call that made the transition. */
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

    /** The immediate readiness notification. Readiness itself rides on Hello. */
    fun ready(): ServerMessage =
        ServerMessage.newBuilder().setReady(Ready.getDefaultInstance()).build()
}

/**
 * A hand-maintained copy of `ServerRole.onMessage`, repeated here because
 * `ServerRole` is in `:paper` and out of this project's reach.
 *
 * Nothing enforces the copy. `ServerRoleTest` pins `ServerRole` to an
 * expectation table of its own and never sees this function, so the two are
 * coupled only by whoever remembers. A case added to `ServerRole.onMessage` and
 * not to this one therefore fails no test anywhere: the [SessionLoopTest] cases
 * that drive a report interval or a session deadline execute the two branches
 * below and assert nothing about which branches exist, so they would go on
 * passing against a mapping production had already left behind -- and go on
 * reading as though they were facts about `ReportInterval` and
 * `SessionDeadline` rather than about this copy of them.
 *
 * Which is exactly what [AgentRole]'s own KDoc warns about -- "two readers of
 * the same messageCase in two files is how the two halves drift" -- now true of
 * the test double rather than of the loop. It is the price of keeping the loop's
 * tests in the project the loop lives in; see the plan's Step 5 for the
 * alternative that was rejected. `ServerRole.onMessage` carries a note pointing
 * back here.
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
