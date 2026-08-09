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
import java.util.concurrent.RejectedExecutionException
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicReference

/**
 * One attempt at a stream to the operator, and everything scheduled on it.
 *
 * The session is constructed before the stub call rather than after it: the
 * in-process transport can hand the response observer its first callback — a
 * message, or a terminal one — synchronously from inside `stub.serverSession()`,
 * before that call has returned a value anything could have captured. Attaching
 * the outbound observer afterwards means no callback can arrive with nowhere to
 * route it. That matters most for the operator's opening `SessionDeadline`:
 * dropping it would leave the agent with no renewal scheduled at all, which is
 * the one failure this whole class exists to prevent.
 *
 * The scheduler is injected rather than created here so tests drive time
 * instead of sleeping through it, and so the renewal and backoff timers share
 * the same clock as reporting.
 */
private class Session(val channel: ManagedChannel) {
    private val toOperator = AtomicReference<StreamObserver<ServerMessage>?>(null)
    private var reporting: ScheduledFuture<*>? = null
    private var retired = false

    /**
     * Claimed exactly once, by whichever of the two ends this attempt first:
     * [close], after which nobody owes this stream a reconnect because a
     * replacement already exists, or the stream's own terminal callback, which
     * does owe it one.
     */
    val ended = AtomicBoolean(false)

    fun attach(observer: StreamObserver<ServerMessage>) {
        toOperator.set(observer)
    }

    /**
     * Serialized against [close] and against every other send: gRPC's request
     * observer is not thread-safe, and the greeting, the readiness push and the
     * reporting timer now reach it from three different threads.
     */
    @Synchronized
    fun send(message: ServerMessage) {
        toOperator.get()?.onNext(message)
    }

    /**
     * Installs the reporting timer, unless this session has already been
     * retired — a `ReportInterval` racing with retirement would otherwise leave
     * a timer firing forever against a channel nobody will shut down again.
     *
     * `reporting` is written from a gRPC callback thread and read from whichever
     * thread retires the session, and since renewal those are genuinely
     * different threads; the monitor is what makes the write visible to the
     * reader.
     */
    @Synchronized
    fun report(schedule: () -> ScheduledFuture<*>) {
        if (retired) return
        reporting?.cancel(false)
        reporting = schedule()
    }

    @Synchronized
    fun close() {
        if (retired) return
        retired = true
        // Set before anything below can provoke a terminal callback: this
        // closing is deliberate, so it must not be mistaken for a stream that
        // broke and owes the agent a reconnect.
        ended.set(true)
        reporting?.cancel(false)
        reporting = null
        runCatching { toOperator.get()?.onCompleted() }
        channel.shutdown()
    }
}

/**
 * Connects to the operator, greets it, reports player counts on the interval
 * the operator dictates, and renews the session before the operator's deadline
 * expires.
 *
 * Renewal is make-before-break, and that is the point of the class rather than
 * a detail of it: the operator carries readiness across a handover only if the
 * replacement stream has greeted before the outgoing one goes away. Break
 * before make would drop every server out of `Ready` on the rhythm of the hard
 * deadline, deregister it from its proxies and book a readiness loss — a
 * self-inflicted flap counter, every ten minutes, forever.
 *
 * A broken stream is always retried, with backoff and without a give-up point.
 * There is nothing useful for the agent to do other than keep trying: the pod
 * stays up either way, and an operator that is rolling out or briefly
 * unreachable must not cost the agent its session permanently.
 */
class SessionLoop(
    private val channels: () -> ManagedChannel,
    private val credentials: CallCredentials,
    private val state: ServerState,
    private val scheduler: ScheduledExecutorService,
    private val version: String,
    private val log: (String, Throwable?) -> Unit,
    private val jitter: (Long) -> Long = { base ->
        // ±10 %, so the pods of one group neither renew nor reconnect in the
        // same instant. An operator restart breaks every agent's stream at once,
        // so the reconnect delay needs this as much as the renewal delay does.
        base + (Math.random() * 0.2 * base).toLong() - (0.1 * base).toLong()
    },
) : AutoCloseable {
    private val current = AtomicReference<Session?>(null)
    private val attempt = AtomicInteger(0)
    private val stopped = AtomicBoolean(false)

    companion object {
        /** 1 s doubling to a 30 s cap. Never gives up: see the class comment. */
        fun backoffMillis(attempt: Int): Long =
            minOf(30_000L, 1_000L shl minOf(attempt, 20))
    }

    fun start() {
        stopped.set(false)
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
        // Set first, so a reconnect or renewal already sitting on the scheduler
        // cannot open a stream the plugin has no way left to close.
        stopped.set(true)
        current.getAndSet(null)?.close()
    }

    override fun close() = stop()

    private fun connect() {
        if (stopped.get()) return
        val channel = channels()
        val session = Session(channel)
        val stub = AgentServiceGrpc.newStub(channel).withCallCredentials(credentials)

        val fromOperator = object : StreamObserver<OperatorToServer> {
            override fun onNext(value: OperatorToServer) {
                // The operator answering is what proves the stream reached it;
                // a Hello handed to the transport proves nothing. Resetting the
                // backoff at the end of connect() instead would turn a stream
                // that always dies just after it opens into a reconnect every
                // second, for as long as the operator stays broken.
                attempt.set(0)
                when (value.messageCase) {
                    OperatorToServer.MessageCase.REPORT_INTERVAL ->
                        startReporting(session, value.reportInterval.seconds)
                    OperatorToServer.MessageCase.SESSION_DEADLINE ->
                        scheduleRenewal(session, value.sessionDeadline.renewAfterSeconds)
                    else -> Unit
                }
            }

            override fun onError(t: Throwable) {
                log("the operator stream failed", t)
                streamEnded(session)
            }

            override fun onCompleted() {
                log("the operator closed the stream", null)
                streamEnded(session)
            }
        }

        session.attach(stub.serverSession(fromOperator))

        send(
            session,
            ServerMessage.newBuilder()
                .setHello(Hello.newBuilder().setVersion(version).setReady(state.ready))
                .build(),
        )

        // Retire the outgoing session only now, after the new one has greeted:
        // make-before-break. Closing it at the point `session` was created —
        // before the Hello went out — would drop the server out of Ready for
        // however long the new stream takes to come up. This is also why the
        // renewal path below has no close() of its own.
        current.getAndSet(session)?.close()

        // stop() may have run while this attempt was in flight, in which case it
        // found nothing in `current` to retire and this session would outlive
        // the plugin.
        if (stopped.get()) stop()
    }

    /**
     * A stream ended without the agent having retired it, so it owes itself a
     * reconnect — including when it ended so early that `current` never held it.
     *
     * Guarding on `current` instead, which reads as the obvious thing to do,
     * would skip the reconnect in exactly that case: connect() installs the
     * session only after the Hello has gone out, so a failure arriving before
     * that leaves `current` pointing elsewhere and the compare-and-set failing.
     * The agent would then sit silent forever, which is worse than reconnecting
     * too eagerly. A per-attempt flag says what `current` cannot: whether this
     * particular stream was replaced on purpose.
     */
    private fun streamEnded(session: Session) {
        if (session.ended.compareAndSet(false, true)) reconnectLater()
    }

    /**
     * Make before break. connect() opens and greets the new stream and only
     * then retires the old one, so there is deliberately no close() here — the
     * old session is closed by connect(), strictly after the replacement has
     * greeted.
     */
    private fun scheduleRenewal(session: Session, renewAfterSeconds: Int) {
        if (renewAfterSeconds <= 0) return
        val delay = jitter(TimeUnit.SECONDS.toMillis(renewAfterSeconds.toLong()))
        schedule(delay, "the renewal could not be scheduled") {
            // Superseded or already broken in the meantime: whoever did that
            // owns what happens next.
            if (current.get() !== session || session.ended.get()) return@schedule
            try {
                connect()
            } catch (e: Exception) {
                // The old stream is untouched and lives until the hard deadline,
                // so there is still time — but only if something tries again.
                log("the renewal failed; retrying before the hard deadline", e)
                reconnectLater()
            }
        }
    }

    private fun reconnectLater() {
        if (stopped.get()) return
        val delay = jitter(backoffMillis(attempt.getAndIncrement()))
        schedule(delay, "the reconnect could not be scheduled") {
            try {
                connect()
            } catch (e: Exception) {
                // connect() can fail before anything is registered anywhere —
                // an unreadable token, a channel that will not build. Nothing
                // else would ever retry, so this has to.
                log("could not reconnect to the operator", e)
                reconnectLater()
            }
        }
    }

    private fun schedule(delayMillis: Long, whenRejected: String, task: () -> Unit) {
        try {
            scheduler.schedule(Runnable { task() }, delayMillis, TimeUnit.MILLISECONDS)
        } catch (e: RejectedExecutionException) {
            // The scheduler is gone, which happens on shutdown. Throwing here
            // would surface on a gRPC callback thread instead.
            log(whenRejected, e)
        }
    }

    private fun startReporting(session: Session, seconds: Int) {
        if (seconds <= 0) return
        session.report {
            scheduler.scheduleAtFixedRate(
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
    }

    private fun send(session: Session, message: ServerMessage) {
        try {
            session.send(message)
        } catch (e: Exception) {
            log("could not send to the operator", e)
        }
    }
}
