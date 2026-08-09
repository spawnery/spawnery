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
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference

/**
 * One live stream to the operator, and everything scheduled on it.
 *
 * The scheduler is injected rather than created here so tests drive time
 * instead of sleeping through it, and so Task 6's renewal and backoff timers
 * share the same clock as reporting.
 */
private class Session(
    val channel: ManagedChannel,
    val toOperator: StreamObserver<ServerMessage>,
) {
    var reporting: ScheduledFuture<*>? = null

    fun close() {
        reporting?.cancel(false)
        runCatching { toOperator.onCompleted() }
        channel.shutdown()
    }
}

/**
 * Connects to the operator, greets it, and reports player counts on the
 * interval the operator dictates.
 *
 * `connect()` is written to be callable more than once and a `SessionDeadline`
 * arriving on the stream is ignored rather than crashing, because Task 6 adds
 * renewal-on-deadline and reconnect-with-backoff to this same class.
 */
class SessionLoop(
    private val channels: () -> ManagedChannel,
    private val credentials: CallCredentials,
    private val state: ServerState,
    private val scheduler: ScheduledExecutorService,
    private val version: String,
    private val log: (String, Throwable?) -> Unit,
) : AutoCloseable {
    private val current = AtomicReference<Session?>(null)

    fun start() {
        connect()
    }

    /**
     * Called when the server finished loading. Readiness is a state, not an
     * event: every Hello carries it, and this only adds the immediate
     * notification so the operator does not wait for the next connect.
     */
    fun readyChanged() {
        val session = current.get() ?: return
        if (!state.ready) return
        send(session, ServerMessage.newBuilder().setReady(Ready.getDefaultInstance()).build())
    }

    fun stop() {
        current.getAndSet(null)?.close()
    }

    override fun close() = stop()

    private fun connect() {
        val channel = channels()
        val stub = AgentServiceGrpc.newStub(channel).withCallCredentials(credentials)

        // Assigned before the stub call returns, because the in-process
        // transport can hand the response observer its first callback
        // synchronously from within stub.serverSession() — before the `session`
        // local below has a value the callback closure could capture. Routing
        // through this holder resolves that forward reference; `current` is
        // set from the same value immediately after.
        val holder = AtomicReference<Session?>(null)

        val fromOperator = object : StreamObserver<OperatorToServer> {
            override fun onNext(value: OperatorToServer) {
                val session = holder.get() ?: return
                when (value.messageCase) {
                    OperatorToServer.MessageCase.REPORT_INTERVAL ->
                        startReporting(session, value.reportInterval.seconds)
                    // SessionDeadline is Task 6's concern (renewal). Ignoring it
                    // here — rather than failing on an unhandled case — is what
                    // lets that be added without touching this branch.
                    else -> Unit
                }
            }

            override fun onError(t: Throwable) {
                log("the operator stream failed", t)
            }

            override fun onCompleted() {
                log("the operator closed the stream", null)
            }
        }

        val toOperator = stub.serverSession(fromOperator)
        val session = Session(channel, toOperator)
        holder.set(session)
        current.set(session)

        send(
            session,
            ServerMessage.newBuilder()
                .setHello(Hello.newBuilder().setVersion(version).setReady(state.ready))
                .build(),
        )
    }

    private fun startReporting(session: Session, seconds: Int) {
        if (seconds <= 0) return
        session.reporting?.cancel(false)
        session.reporting = scheduler.scheduleAtFixedRate(
            {
                send(
                    session,
                    ServerMessage.newBuilder()
                        .setPlayerCount(
                            PlayerCount.newBuilder()
                                .setPlayers(state.players)
                                .setSlots(state.slots),
                        )
                        .build(),
                )
            },
            0,
            seconds.toLong(),
            TimeUnit.SECONDS,
        )
    }

    private fun send(session: Session, message: ServerMessage) {
        try {
            synchronized(session) { session.toOperator.onNext(message) }
        } catch (e: Exception) {
            log("could not send to the operator", e)
        }
    }
}
