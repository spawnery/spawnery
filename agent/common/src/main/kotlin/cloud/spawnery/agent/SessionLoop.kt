package cloud.spawnery.agent

import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.ClientCallStreamObserver
import io.grpc.stub.StreamObserver
import java.util.concurrent.RejectedExecutionException
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.ScheduledFuture
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference

/**
 * One attempt at a stream to the operator, and everything scheduled on it.
 *
 * Constructed before the stub call and attached afterwards: the in-process
 * transport can deliver the first callback, such as the opening
 * `SessionDeadline`, from inside `stub.serverSession()` before it returns.
 */
private class Session<Req>(val channel: ManagedChannel, replaces: Session<Req>?) {
    private val toOperator = AtomicReference<StreamObserver<Req>?>(null)
    private var reporting: ScheduledFuture<*>? = null
    private var retired = false

    /**
     * Cleared by whichever of [takeOver] and [abandon] runs first. That makes
     * both idempotent and keeps the chain of replaced sessions, each with its
     * channel, from staying reachable from `current` for the life of the JVM.
     */
    private val replaces = AtomicReference(replaces)

    /**
     * Set once a replacement attempt has been opened; from then on the
     * replacement owes the reconnect. See [SessionLoop.streamEnded].
     */
    private val replaced = AtomicBoolean(false)

    /**
     * Claimed exactly once, by whichever ends this attempt first: [close] (no
     * reconnect owed), the stream's terminal callback (owed unless [replaced]),
     * or the answer deadline (owed).
     */
    val ended = AtomicBoolean(false)

    /**
     * The timer lives here so the monitor that retires the session also
     * disarms it; one left armed would be a second claim on [ended].
     */
    private val answered = AtomicBoolean(false)
    private var answerDeadline: ScheduledFuture<*>? = null

    fun attach(observer: StreamObserver<Req>) {
        toOperator.set(observer)
    }

    /**
     * gRPC's request observer is not thread-safe, and the greeting, the
     * readiness push and the reporting timer send from different threads.
     */
    @Synchronized
    fun send(message: Req) {
        toOperator.get()?.onNext(message)
    }

    /**
     * A no-op once retired: a `ReportInterval` racing retirement would
     * otherwise leave a timer firing forever against a dead channel.
     */
    @Synchronized
    fun report(schedule: () -> ScheduledFuture<*>) {
        if (retired) return
        reporting?.cancel(false)
        reporting = schedule()
    }

    /**
     * Retires the stream this one replaced, once. Called only when the operator
     * first answers on this stream; see [SessionLoop].
     */
    fun takeOver() {
        replaces.getAndSet(null)?.close()
    }

    /**
     * Gives up the handover without performing it. The replaced stream keeps
     * running until its own hard deadline.
     */
    fun abandon() {
        replaces.set(null)
    }

    /**
     * Called before the replacement's stream exists, because the operator can
     * end this one the instant it does.
     */
    fun replacementOpened() {
        replaced.set(true)
    }

    fun hasReplacement(): Boolean = replaced.get()

    /**
     * The operator can answer before connect() gets here, synchronously from
     * inside `serverSession()` on the in-process transport.
     */
    @Synchronized
    fun awaitAnswer(arm: () -> ScheduledFuture<*>?) {
        if (retired || answered.get()) return
        answerDeadline = arm()
    }

    fun answerArrived() {
        answered.set(true)
        stopAwaitingAnswer()
    }

    fun wasAnswered(): Boolean = answered.get()

    /**
     * For a stream that ended on its own. [close] would also [takeOver] and so
     * retire the stream this one replaced. Graceful is enough here because the
     * call underneath has already finished.
     */
    fun releaseChannel() {
        channel.shutdown()
    }

    @Synchronized
    fun stopAwaitingAnswer() {
        answerDeadline?.cancel(false)
        answerDeadline = null
    }

    /**
     * Retires this attempt. Graceful by default: the agent half-closes, the
     * operator finishes the call, and `shutdown()` can terminate.
     *
     * [cancel] is for a call the operator never answered. Its handler is
     * blocked before it reads, so it never sees the half-close, and a graceful
     * shutdown would wait on that call forever.
     */
    @Synchronized
    fun close(cancel: Boolean = false) {
        if (retired) return
        retired = true
        // Before anything below can provoke a terminal callback, so a
        // deliberate close is not taken for a broken stream owing a reconnect.
        ended.set(true)
        reporting?.cancel(false)
        reporting = null
        stopAwaitingAnswer()
        val observer = toOperator.get()
        if (cancel) {
            // Cancelling through the call's observer puts RST_STREAM on the
            // wire; shutdownNow() covers the cast failing, and this channel
            // carries no other call.
            runCatching {
                @Suppress("UNCHECKED_CAST")
                (observer as? ClientCallStreamObserver<Req>)
                    ?.cancel("the operator never answered this stream", null)
            }
            channel.shutdownNow()
        } else {
            runCatching { observer?.onCompleted() }
            channel.shutdown()
        }
        // stop() closes only the current session, so a handover still in
        // flight must not leave the outgoing one running. Graceful even after a
        // give-up, because the replaced stream was answered; that rests on the
        // argument in [SessionLoop.answerOverdue].
        takeOver()
    }
}

/**
 * Connects to the operator, greets it, reports player counts on the interval
 * the operator dictates, and renews the session before the operator's deadline
 * expires.
 *
 * Renewal is make-before-break: the operator carries readiness across a
 * handover only if the replacement stream has reached it before the outgoing
 * one goes away. Break before make would drop every server out of `Ready` on
 * every renewal.
 *
 * "Reached it" means the operator's first message on the new stream. Retiring
 * the outgoing stream right after handing the Hello to the transport loses that
 * race every time on a real network, because the replacement still needs a TCP
 * and TLS handshake. The in-process transport the unit tests use hides this.
 *
 * A broken stream is retried with backoff and never given up on. A stream that
 * is accepted and never answered is bounded by [awaitAnswer]. A stream that was
 * answered and then goes silent is bounded only by
 * [OperatorChannel.KEEPALIVE_SECONDS].
 */
class SessionLoop<Req, Resp>(
    private val channels: () -> ManagedChannel,
    private val credentials: CallCredentials,
    private val role: AgentRole<Req, Resp>,
    private val scheduler: ScheduledExecutorService,
    private val version: String,
    private val log: (String, Throwable?) -> Unit,
    /**
     * Routine events, as opposed to [log]'s problems. A separate channel
     * because `log(..., null)` already carries non-routine events that have no
     * throwable.
     */
    private val note: (String) -> Unit,
    private val jitter: (Long) -> Long = { base ->
        // ±10 %, so the pods of one group neither renew nor reconnect in the
        // same instant; an operator restart breaks every stream at once.
        base + (Math.random() * 0.2 * base).toLong() - (0.1 * base).toLong()
    },
    /** Injectable only so tests need not wait out [FALLBACK_ANSWER_BOUND_MILLIS]. */
    private val fallbackAnswerBoundMillis: Long = FALLBACK_ANSWER_BOUND_MILLIS,
    /**
     * Called once for every stream that becomes current, on the installing
     * thread before anything is sent on it. Per-stream state hangs off it: the
     * requests in flight, which [CloudConnector.onStreamChanged] fails rather
     * than resends because only the caller knows whether that is safe, and
     * state the operator forgets across a changeover, such as EventInterest.
     */
    private val onStreamChanged: () -> Unit = {},
) : AutoCloseable {
    private val current = AtomicReference<Session<Req>?>(null)
    private val attempt = AtomicInteger(0)
    private val stopped = AtomicBoolean(false)

    /**
     * The operator's last `hardDeadlineSeconds`, in milliseconds. Zero until it
     * has sent one, and then [awaitAnswer] uses [FALLBACK_ANSWER_BOUND_MILLIS].
     */
    private val hardDeadlineMillis = AtomicLong(0)

    companion object {
        /** 1 s doubling to a 30 s cap. Never gives up: see the class comment. */
        fun backoffMillis(attempt: Int): Long =
            minOf(30_000L, 1_000L shl minOf(attempt, 20))

        /**
         * The answer bound before the operator has stated a deadline, so at
         * most once per process. Deliberately loose: a healthy operator answers
         * in microseconds, and this is only a finite number in place of none.
         */
        const val FALLBACK_ANSWER_BOUND_MILLIS = 5L * 60 * 1000
    }

    fun start() {
        stopped.set(false)
        attempt.set(0)
        try {
            connect()
        } catch (e: Exception) {
            // Every other connect() caller reschedules itself on failure; without
            // this a first attempt that throws, say on an unparseable endpoint,
            // would end the agent before it started.
            log("the first connect to the operator failed", e)
            reconnectLater()
        }
    }

    /**
     * Does nothing if there is no stream. This is the immediate notification,
     * not the state: every Hello carries the state anyway.
     */
    fun send(message: Req) {
        val session = current.get() ?: return
        send(session, message)
    }

    fun stop() {
        // Set first, so a reconnect or renewal already sitting on the scheduler
        // cannot open a stream the plugin has no way left to close.
        stopped.set(true)
        val session = current.getAndSet(null) ?: return
        // The same choice as [answerOverdue]: an unanswered call never finishes,
        // and onDisable() gives the scheduler only two seconds, so a parked
        // channel would outlive the plugin inside a running JVM.
        session.close(cancel = !session.wasAnswered())
    }

    override fun close() = stop()

    private fun connect() {
        if (stopped.get()) return
        val channel = channels()
        // Captured before the stub call: the operator's first message can
        // arrive from inside serverSession(), and the handover has to know its
        // predecessor by then.
        val outgoing = current.get()
        val session = Session(channel, outgoing)
        // Before the stream exists: the operator ends the displaced stream at
        // the entry of the replacement's handler, so its terminal callback can
        // fire while the call below is still returning. See streamEnded.
        outgoing?.replacementOpened()

        val fromOperator = object : StreamObserver<Resp> {
            override fun onNext(value: Resp) {
                // The operator's answer, not a Hello handed to the transport, is
                // what proves the stream reached it. The answer bound, the
                // backoff and the handover all wait for it.
                session.answerArrived()
                // Resetting at the end of connect() instead would turn a stream
                // that always dies just after opening into a reconnect every
                // second.
                attempt.set(0)
                session.takeOver()
                when (val directive = role.onMessage(value)) {
                    is Directive.Report -> startReporting(session, directive.seconds)
                    is Directive.Deadline -> {
                        hardDeadlineMillis.set(
                            TimeUnit.SECONDS.toMillis(directive.hardDeadlineSeconds.toLong()),
                        )
                        scheduleRenewal(session, directive.renewAfterSeconds)
                    }
                    Directive.None -> Unit
                }
            }

            override fun onError(t: Throwable) {
                // Every renewal ends the displaced stream with Unavailable, by
                // design, so that is a note rather than a failure with a stack
                // trace. A stream nothing replaced keeps its cause.
                if (session.hasReplacement()) {
                    note("the operator retired this stream for a renewal")
                } else {
                    log("the operator stream failed", t)
                }
                streamEnded(session)
            }

            override fun onCompleted() {
                log("the operator closed the stream", null)
                streamEnded(session)
            }
        }

        val toOperator = try {
            role.open(channel, credentials, fromOperator)
        } catch (e: Exception) {
            channel.shutdown()
            throw e
        }
        session.attach(toOperator)

        send(session, role.hello(version))

        // The replacement died before it could take over: synchronously on the
        // in-process transport, in production on a rejected token or an
        // unreachable operator. The outgoing session still lives until its hard
        // deadline, and the terminal callback has already booked a reconnect.
        if (session.ended.get()) {
            session.abandon()
            session.close()
            return
        }

        // Retiring the outgoing stream is deliberately not done here but in
        // onNext, once the operator has answered.
        current.set(session)
        // Before anything is sent on it: a report that raced this would be
        // sent on the new stream and then immediately undone by a reset.
        onStreamChanged()

        // This attempt now owes the agent its next stream, so an operator that
        // never answers needs a bound.
        awaitAnswer(session)

        // stop() may have run meanwhile and found nothing in `current` to retire.
        if (stopped.get()) stop()
    }

    /**
     * A stream ended without the agent retiring it, so it owes a reconnect,
     * even if it ended before `current` ever held it. That is why this guards
     * on a per-attempt flag and not on `current`.
     *
     * Unless a replacement is under way: the operator cancels the displaced
     * stream at the replacement's handler entry, so on every renewal the
     * outgoing stream fails first. Reconnecting here would supersede the
     * replacement a second later, and again, at roughly 1 Hz. A replacement
     * that dies books its own reconnect.
     */
    private fun streamEnded(session: Session<Req>) {
        if (!session.ended.compareAndSet(false, true)) return
        session.stopAwaitingAnswer()
        // Before the skip: a superseded stream's channel is as dead as a broken
        // one's.
        session.releaseChannel()
        if (session.hasReplacement()) return
        reconnectLater()
    }

    /**
     * Bounds how long an attempt may sit accepted and unanswered. Once the
     * replacement holds the reconnect obligation nothing else ends that wait:
     * the call has no deadline, and the operator arms its own hard deadline
     * only after its first `Send`.
     *
     * The bound is the operator's `hardDeadlineSeconds`, so by the time it
     * fires the outgoing stream is past its own deadline and [answerOverdue]
     * may retire it too. Until the operator has stated one, it is
     * [FALLBACK_ANSWER_BOUND_MILLIS].
     */
    private fun awaitAnswer(session: Session<Req>) {
        val bound = hardDeadlineMillis.get().takeIf { it > 0 } ?: fallbackAnswerBoundMillis
        if (bound <= 0) return
        session.awaitAnswer {
            schedule(bound, "the answer deadline could not be scheduled") { answerOverdue(session) }
        }
    }

    /**
     * The operator accepted this attempt and never answered it. Claiming
     * `ended` makes the one reconnect exactly-once against the terminal
     * callback.
     *
     * Unlike [streamEnded] there is no `hasReplacement()` skip, because an
     * unanswered session cannot have been replaced: a renewal is armed only by
     * a `SessionDeadline`, which is an answer. Should the operator ever send
     * two `SessionDeadline`s on one stream, the guard is needed here, or both
     * sessions book a reconnect.
     *
     * The close is the forceful one; see [Session.close].
     */
    private fun answerOverdue(session: Session<Req>) {
        if (!session.ended.compareAndSet(false, true)) return
        log("the operator accepted the stream and never answered it; opening another", null)
        session.close(cancel = true)
        reconnectLater()
    }

    /**
     * No close() here: the old session is retired once the operator answers
     * the replacement.
     */
    private fun scheduleRenewal(session: Session<Req>, renewAfterSeconds: Int) {
        if (renewAfterSeconds <= 0) return
        val delay = jitter(TimeUnit.SECONDS.toMillis(renewAfterSeconds.toLong()))
        schedule(delay, "the renewal could not be scheduled") {
            // Superseded or already broken in the meantime: whoever did that
            // owns what happens next.
            if (current.get() !== session || session.ended.get()) return@schedule
            try {
                connect()
            } catch (e: Exception) {
                // The old stream lives until the hard deadline, so there is
                // still time, but only if something tries again.
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
                // connect() can fail before anything is registered anywhere,
                // so nothing else would ever retry.
                log("could not reconnect to the operator", e)
                reconnectLater()
            }
        }
    }

    /** Null if the scheduler refused the task. */
    private fun schedule(delayMillis: Long, whenRejected: String, task: () -> Unit): ScheduledFuture<*>? {
        return try {
            scheduler.schedule(Runnable { task() }, delayMillis, TimeUnit.MILLISECONDS)
        } catch (e: RejectedExecutionException) {
            // The scheduler is gone on shutdown. Throwing here would surface on
            // a gRPC callback thread instead.
            log(whenRejected, e)
            null
        }
    }

    private fun startReporting(session: Session<Req>, seconds: Int) {
        if (seconds <= 0) return
        try {
            session.report {
                scheduler.scheduleAtFixedRate(
                    {
                        send(session, role.playerCount())
                        // On the same tick as the count, so both describe the
                        // same instant; each sent on its own, so one that cannot
                        // go does not hold back the others.
                        for (extra in role.extraReports()) {
                            send(session, extra)
                        }
                    },
                    0,
                    seconds.toLong(),
                    TimeUnit.SECONDS,
                )
            }
        } catch (e: RejectedExecutionException) {
            // The same shutdown case as [schedule]: this runs on a gRPC
            // callback thread.
            log("the reporting timer could not be scheduled", e)
        }
    }

    private fun send(session: Session<Req>, message: Req) {
        try {
            session.send(message)
        } catch (e: Exception) {
            log("could not send to the operator", e)
        }
    }
}
