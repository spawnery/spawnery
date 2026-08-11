package cloud.spawnery.agent

import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.ReportInterval
import cloud.spawnery.agent.pb.ServerMessage
import cloud.spawnery.agent.pb.SessionDeadline
import io.grpc.CallOptions
import io.grpc.ClientCall
import io.grpc.ForwardingClientCall
import io.grpc.ManagedChannel
import io.grpc.Metadata
import io.grpc.MethodDescriptor
import io.grpc.Status
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path
import java.util.Collections
import java.util.concurrent.Executors
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit

/**
 * A [ManagedChannel] that reports, in program order, exactly when a message is
 * sent on it and when it is shut down.
 *
 * It says what the agent did locally, which is enough to catch an outgoing
 * stream retired for a replacement that never got off the ground. It is not
 * enough to establish make before break: that is a claim about what the
 * operator saw, and the tests below make it by holding the operator's answer
 * back and watching the outgoing stream stay up.
 */
private class TrackingChannel(
    private val delegate: ManagedChannel,
    private val onSend: (() -> Unit)? = null,
    private val onShutdown: (() -> Unit)? = null,
) : ManagedChannel() {
    override fun <ReqT, RespT> newCall(
        methodDescriptor: MethodDescriptor<ReqT, RespT>,
        callOptions: CallOptions,
    ): ClientCall<ReqT, RespT> {
        val call = delegate.newCall(methodDescriptor, callOptions)
        return object : ForwardingClientCall.SimpleForwardingClientCall<ReqT, RespT>(call) {
            override fun sendMessage(message: ReqT) {
                onSend?.invoke()
                super.sendMessage(message)
            }
        }
    }

    override fun shutdown(): ManagedChannel {
        onShutdown?.invoke()
        return delegate.shutdown()
    }

    override fun shutdownNow(): ManagedChannel = delegate.shutdownNow()
    override fun isShutdown(): Boolean = delegate.isShutdown()
    override fun isTerminated(): Boolean = delegate.isTerminated()
    override fun awaitTermination(timeout: Long, unit: TimeUnit): Boolean = delegate.awaitTermination(timeout, unit)
    override fun authority(): String = delegate.authority()
}

/**
 * A channel whose calls fail the instant they are started — synchronously, from
 * inside `stub.serverSession()`, before [SessionLoop] has had the chance to
 * install the session anywhere. It stands in for every way a stream can die
 * before it is established: an operator that is rolling out, a DNS name that
 * does not resolve yet, a rejected token.
 */
private class FailingChannel : ManagedChannel() {
    override fun <ReqT, RespT> newCall(
        methodDescriptor: MethodDescriptor<ReqT, RespT>,
        callOptions: CallOptions,
    ): ClientCall<ReqT, RespT> = object : ClientCall<ReqT, RespT>() {
        override fun start(responseListener: Listener<RespT>, headers: Metadata) {
            responseListener.onClose(
                Status.UNAVAILABLE.withDescription("no operator"),
                Metadata(),
            )
        }

        override fun request(numMessages: Int) = Unit
        override fun cancel(message: String?, cause: Throwable?) = Unit
        override fun halfClose() = Unit
        override fun sendMessage(message: ReqT) = Unit
    }

    override fun shutdown(): ManagedChannel = this
    override fun shutdownNow(): ManagedChannel = this
    override fun isShutdown(): Boolean = true
    override fun isTerminated(): Boolean = true
    override fun awaitTermination(timeout: Long, unit: TimeUnit): Boolean = true
    override fun authority(): String = "failing"
}

class SessionLoopTest {
    private val scheduler: ScheduledExecutorService = Executors.newSingleThreadScheduledExecutor()

    @AfterEach fun shutdown() { scheduler.shutdownNow() }

    private fun loopAgainst(
        operator: FakeOperator,
        state: ServerState,
        dir: Path,
        channels: () -> ManagedChannel = { operator.newChannel() },
        // Identity, so a test's delays are the delays it wrote down. The
        // production default spreads them by ±10 %, which would make every
        // assertion about timing approximate.
        jitter: (Long) -> Long = { it },
        // The production value is five minutes, which is the point of it — see
        // SessionLoop.FALLBACK_ANSWER_BOUND_MILLIS. Only the test that is about
        // that bound overrides it, and every other test here is answered long
        // before it could matter.
        fallbackAnswerBoundMillis: Long = SessionLoop.FALLBACK_ANSWER_BOUND_MILLIS,
    ): SessionLoop {
        val token = dir.resolve("token")
        Files.writeString(token, "test-token")
        return SessionLoop(
            channels = channels,
            credentials = BearerCredentials.of(TokenSource(token)),
            state = state,
            scheduler = scheduler,
            version = "26.2-0.2.0",
            log = { _, _ -> },
            jitter = jitter,
            fallbackAnswerBoundMillis = fallbackAnswerBoundMillis,
        )
    }

    @Test
    fun `greets with the version and the current readiness`(@TempDir dir: Path) {
        FakeOperator("greets").use { operator ->
            val state = ServerState().apply { markReady() }
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()

                val stream = operator.awaitStream(0)
                val hello = stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                assertEquals("26.2-0.2.0", hello.hello.version)
                assertTrue(hello.hello.ready)
                assertEquals("Bearer test-token", stream.authorization)
            }
        }
    }

    @Test
    fun `sends Ready when readiness arrives after the greeting`(@TempDir dir: Path) {
        FakeOperator("ready-later").use { operator ->
            val state = ServerState()
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                val hello = stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                assertEquals(false, hello.hello.ready)

                state.markReady()
                loop.readyChanged()

                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.READY }
            }
        }
    }

    @Test
    fun `reports the player count at the interval the operator dictates`(@TempDir dir: Path) {
        FakeOperator("reports").use { operator ->
            val state = ServerState().apply { sample(players = 3, slots = 100) }
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                stream.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )

                val report = stream.awaitMessage {
                    it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT
                }
                assertEquals(3, report.playerCount.players)
                assertEquals(100, report.playerCount.slots)
            }
        }
    }

    @Test
    fun `does not report before the operator has dictated an interval`(@TempDir dir: Path) {
        FakeOperator("no-interval").use { operator ->
            val state = ServerState().apply { sample(players = 3, slots = 100) }
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                Thread.sleep(500)

                assertTrue(
                    stream.received.none { it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT },
                    "the interval is the operator's to set; both sides derive the staleness " +
                        "threshold from it, so guessing one locally would break that",
                )
            }
        }
    }

    @Test
    fun `retires the previous session only after the operator has answered the new one`(
        @TempDir dir: Path,
    ) {
        FakeOperator("renews").use { operator ->
            val state = ServerState()

            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // Get the first session reporting, so a leaked session (the
                // bug) keeps firing on the shared scheduler after a second
                // connect() -- proof that retirement actually happened, not
                // just that a message was sent.
                first.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT }

                // Reconnect while the first session is still live -- no stop()
                // in between, exactly the case the leak was missed on.
                loop.start()
                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // Half 1: make before break. The operator has the new stream's
                // Hello and has not answered it, and until it does the outgoing
                // stream has to stay up. Waiting on the latch rather than
                // sleeping keeps the failure fast: a regression closes the
                // stream at once and this returns immediately.
                assertFalse(
                    first.closed.await(1, TimeUnit.SECONDS),
                    "the previous session was retired before the operator answered the new one",
                )

                // Half 2: once it does answer, the previous session is actually
                // retired -- its stream closes and its reporting future stops.
                second.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )
                assertTrue(
                    first.closed.await(5, TimeUnit.SECONDS),
                    "the previous session's stream was never closed",
                )
                val reportsAtRetirement = first.received.count {
                    it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT
                }
                Thread.sleep(2000)
                assertEquals(
                    reportsAtRetirement,
                    first.received.count { it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT },
                    "the previous session's reporting future kept firing after retirement",
                )
            }
        }
    }

    @Test
    fun `renews before the deadline and keeps the old stream open until the operator answers`(
        @TempDir dir: Path,
    ) {
        FakeOperator("renew").use { operator ->
            val state = ServerState().apply { markReady() }

            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setSessionDeadline(
                            SessionDeadline.newBuilder()
                                .setRenewAfterSeconds(1)
                                .setHardDeadlineSeconds(3),
                        )
                        .build(),
                )

                // Nothing else opens a stream: arriving at all is the assertion
                // that the deadline was acted on before it ran out.
                val second = operator.awaitStream(1)
                val hello = second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // Readiness is repeated on every connect, so the operator's
                // Supersede has something to carry across the handover.
                assertTrue(hello.hello.ready, "the renewed stream greeted as not ready")

                // Make before break, stated as what the operator sees rather
                // than as what the agent does: the renewed stream is greeted
                // and unanswered, and for as long as that is true the outgoing
                // stream must still be there. An agent that retires it on its
                // own Hello passes a local-ordering check and still hands the
                // operator a disconnect followed by a connect, because the
                // replacement's TLS handshake takes longer than the outgoing
                // stream's close.
                assertFalse(
                    first.closed.await(1, TimeUnit.SECONDS),
                    "break before make: the outgoing stream was retired before the " +
                        "operator had answered the renewed one, so the operator sees a " +
                        "disconnect and the server drops out of Ready",
                )

                second.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )
                assertTrue(
                    first.closed.await(5, TimeUnit.SECONDS),
                    "the outgoing stream was never closed",
                )

                // Retiring the outgoing stream is not a breakage, so it must not
                // start a reconnect of its own. Bounded wait: the shortest
                // backoff is a second.
                Thread.sleep(2000)
                assertEquals(
                    2,
                    operator.streams.size,
                    "the handover was mistaken for a broken stream and reconnected",
                )
            }
        }
    }

    @Test
    fun `books no reconnect when the operator retires the outgoing stream before answering the replacement`(
        @TempDir dir: Path,
    ) {
        FakeOperator("supersedes").use { operator ->
            val state = ServerState().apply { markReady() }

            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setSessionDeadline(
                            SessionDeadline.newBuilder()
                                .setRenewAfterSeconds(1)
                                .setHardDeadlineSeconds(3),
                        )
                        .build(),
                )

                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // The real operator's order, and the whole point of this test.
                // internal/agentserver cancels the displaced stream's context
                // inside sessions.enter(), at the handler entry of the
                // replacement -- before Supersede and before either Send -- and
                // the cancelled handler answers Unavailable. So the outgoing
                // stream fails while the replacement is still unanswered, and
                // it is not the agent's stream to mourn: the replacement is
                // already on its way and owes the agent whatever comes next.
                //
                // An agent that books a reconnect here books one on every
                // renewal, and because the replacement's first message resets
                // the backoff to its 1 s floor, that reconnect supersedes the
                // replacement a second later and the whole thing repeats,
                // forever.
                first.toAgent.onError(
                    Status.UNAVAILABLE
                        .withDescription("session ended, reconnect with a fresh token")
                        .asRuntimeException(),
                )
                second.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )

                // Two and a half seconds is two whole backoff floors: a booked
                // reconnect has fired well before this returns.
                Thread.sleep(2500)
                assertEquals(
                    2,
                    operator.streams.size,
                    "the operator retiring the displaced stream was mistaken for a " +
                        "breakage the agent owes a reconnect, so every renewal opens a " +
                        "spare stream and the spare supersedes the replacement a second later",
                )
            }
        }
    }

    @Test
    fun `books one reconnect when the replacement dies after the operator retired the outgoing stream`(
        @TempDir dir: Path,
    ) {
        FakeOperator("replacement-dies-late").use { operator ->
            val state = ServerState().apply { markReady() }

            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // A hard deadline far past the end of this test: what is under
                // test here is the terminal callbacks, and an answer deadline
                // firing in the middle of it would be a second reconnect from
                // somewhere else.
                first.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setSessionDeadline(
                            SessionDeadline.newBuilder()
                                .setRenewAfterSeconds(1)
                                .setHardDeadlineSeconds(30),
                        )
                        .build(),
                )

                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // The operator cancels the displaced stream at the handler entry
                // of the replacement, so the agent hears about the outgoing
                // stream first and skips the reconnect: the replacement owes it.
                first.toAgent.onError(
                    Status.UNAVAILABLE
                        .withDescription("session ended, reconnect with a fresh token")
                        .asRuntimeException(),
                )
                // And then the replacement dies too, still unanswered, which is
                // where the obligation now lives. This differs from `keeps the
                // outgoing stream when the renewal's replacement dies at once`
                // in the two ways that matter: the outgoing stream was already
                // cancelled by the operator, and the replacement dies
                // asynchronously rather than from inside serverSession().
                second.toAgent.onError(
                    Status.UNAVAILABLE.withDescription("connection reset").asRuntimeException(),
                )

                // Exactly one reconnect. None at all would be permanent silence
                // - both streams are gone and nothing else is owed - and two
                // would be the storm the skip exists to prevent.
                val third = operator.awaitStream(2)
                third.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                Thread.sleep(2500)
                assertEquals(
                    3,
                    operator.streams.size,
                    "the reconnect the dead replacement owed was booked more than once",
                )
            }
        }
    }

    @Test
    fun `gives up on a replacement the operator accepts and never answers`(
        @TempDir dir: Path,
    ) {
        FakeOperator("mute-operator").use { operator ->
            val state = ServerState().apply { markReady() }

            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // The operator's own numbers, and the hard deadline is the one
                // the agent has to bound the wait below with.
                first.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setSessionDeadline(
                            SessionDeadline.newBuilder()
                                .setRenewAfterSeconds(1)
                                .setHardDeadlineSeconds(2),
                        )
                        .build(),
                )

                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // The operator retires the displaced stream where it always
                // does, at the replacement's handler entry - and then says
                // nothing at all on the replacement. That is an operator
                // goroutine blocked in Agents.Supersede, which sits between the
                // cancel and the first Send, and its own hard-deadline rescue is
                // armed after both Sends, so it never arms.
                first.toAgent.onError(
                    Status.UNAVAILABLE
                        .withDescription("session ended, reconnect with a fresh token")
                        .asRuntimeException(),
                )

                // Everything the agent could still be waiting for is now gone:
                // the outgoing stream was cancelled and skipped because a
                // replacement exists, and the replacement is accepted, mute, and
                // holds an obligation nothing will ever discharge. Without a
                // bound of its own the agent is silent for the life of the TCP
                // connection - no renewal, no reports, no reconnect.
                val third = operator.awaitStream(2)
                third.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                assertTrue(
                    second.closed.await(1, TimeUnit.SECONDS),
                    "the attempt the agent gave up on was left open",
                )
            }
        }
    }

    /**
     * The give-up has to end the call, not merely stop talking on it.
     *
     * `close()` half-closes and shuts the channel down gracefully, which is
     * right everywhere else: the operator finishes the call and the channel
     * terminates behind it. On this one path the operator is by definition not
     * answering — in production its handler is blocked before it starts
     * receiving — so it never finishes anything, and a graceful shutdown waits
     * for that forever. The channel stays in SHUTDOWN holding a connection and
     * a reader thread, once per give-up, for as long as the operator stalls.
     *
     * What this proves: the operator observes a cancellation rather than a
     * half-close, and the channel actually reaches TERMINATED against an
     * operator that answers neither.
     *
     * What it does not prove: that a socket and an OkHttp reader thread are
     * released. The in-process transport has neither. Termination is the
     * property those hang off, so this is the closest a unit test gets, and the
     * `closed` latch on its own is no pin at all — it counts down on a
     * half-close and on a cancellation alike, which is why the existing
     * give-up test above passes either way.
     */
    @Test
    fun `cancels the attempt it gives up on instead of waiting for an answer that is not coming`(
        @TempDir dir: Path,
    ) {
        FakeOperator("mute-parks-transport").use { operator ->
            val state = ServerState().apply { markReady() }
            val opened = Collections.synchronizedList(mutableListOf<ManagedChannel>())

            loopAgainst(
                operator,
                state,
                dir,
                channels = { operator.newChannel().also { opened.add(it) } },
            ).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                first.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setSessionDeadline(
                            SessionDeadline.newBuilder()
                                .setRenewAfterSeconds(1)
                                .setHardDeadlineSeconds(2),
                        )
                        .build(),
                )

                // The renewal's attempt, which the operator accepts and never
                // answers, on a channel of its own that the test holds.
                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                first.toAgent.onError(
                    Status.UNAVAILABLE
                        .withDescription("session ended, reconnect with a fresh token")
                        .asRuntimeException(),
                )

                // The give-up, and the stream it books.
                operator.awaitStream(2)

                assertTrue(
                    second.closed.await(5, TimeUnit.SECONDS),
                    "the attempt the agent gave up on was left open",
                )
                assertEquals(
                    "cancelled",
                    second.terminal.get(),
                    "the agent half-closed the stream it gave up on; an operator that is not " +
                        "answering does not answer a half-close either, so the call stays open",
                )
                assertTrue(
                    opened[1].awaitTermination(5, TimeUnit.SECONDS),
                    "the channel behind the abandoned attempt never terminated: a graceful " +
                        "shutdown waits for a call the operator will never finish, and every " +
                        "give-up parks another one",
                )
            }
        }
    }

    /**
     * The same bound, on the attempt that has no operator number to use.
     *
     * `hardDeadlineMillis` is zero until the operator has sent a
     * `SessionDeadline`, so before this the first attempt of a fresh process
     * was bounded by nothing. That is the worse half of the case, not a corner
     * of it: `internal/agentserver` calls `Agents.Connect` before both `Send`s
     * and before its receive goroutine, so any operator that accepts a stream
     * and stalls there hangs every pod that starts during the stall — with no
     * reports, no readiness, and the Hello unread, so the pod is invisible to
     * the control plane rather than merely quiet.
     *
     * What this proves: with no `SessionDeadline` ever sent, the agent still
     * gives up and opens another stream. What it does not prove: anything about
     * the production value of the bound, which is five minutes and is what this
     * test overrides. The test above and `gives up on a replacement the
     * operator accepts and never answers` are what pin the operator's own
     * number taking over as soon as there is one.
     */
    @Test
    fun `bounds a first attempt the operator has not given it a deadline for`(
        @TempDir dir: Path,
    ) {
        FakeOperator("mute-from-the-start").use { operator ->
            val state = ServerState().apply { markReady() }

            loopAgainst(operator, state, dir, fallbackAnswerBoundMillis = 500).use { loop ->
                loop.start()

                // Accepted, greeted, and never answered — no ReportInterval and
                // no SessionDeadline, so the agent has no number of the
                // operator's to bound the wait with.
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                assertTrue(
                    first.closed.await(1, TimeUnit.SECONDS),
                    "the first attempt was left open after the agent gave up on it",
                )
                assertEquals(
                    "cancelled",
                    first.terminal.get(),
                    "the first attempt was half-closed rather than cancelled",
                )
            }
        }
    }

    @Test
    fun `keeps the outgoing stream when the renewal's replacement dies at once`(
        @TempDir dir: Path,
    ) {
        FakeOperator("renew-fails").use { operator ->
            val state = ServerState().apply { markReady() }
            val order = Collections.synchronizedList(mutableListOf<String>())
            var connectCount = 0

            // The renewal's attempt dies from inside stub.serverSession(), the
            // way a rejected token or a briefly unreachable operator does. The
            // outgoing stream is untouched by that and still has the gap between
            // renewAfterSeconds and hardDeadlineSeconds left to live, so
            // retiring it for a replacement that never existed would be break
            // before make -- the readiness loss the overlap exists to prevent,
            // reached by a different route.
            val loop = loopAgainst(
                operator,
                state,
                dir,
                channels = {
                    when (connectCount++) {
                        0 -> TrackingChannel(operator.newChannel(), onShutdown = { order.add("outgoing-closed") })
                        1 -> FailingChannel()
                        else -> TrackingChannel(operator.newChannel(), onSend = { order.add("replacement-greeted") })
                    }
                },
            )

            loop.use {
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setSessionDeadline(
                            SessionDeadline.newBuilder()
                                .setRenewAfterSeconds(1)
                                .setHardDeadlineSeconds(3),
                        )
                        .build(),
                )

                // The failed renewal owes itself a reconnect, and that one
                // succeeds: arriving at all is the assertion that a renewal
                // failing is not the end of the agent's session.
                val replacement = operator.awaitStream(1)
                replacement.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // The outgoing stream survived the failed attempt: it is still
                // open here, one whole renewal later, and only the operator
                // answering the replacement retires it.
                assertFalse(
                    first.closed.await(1, TimeUnit.SECONDS),
                    "the outgoing stream was retired for a replacement that had already died",
                )
                replacement.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )
                assertTrue(
                    first.closed.await(5, TimeUnit.SECONDS),
                    "the outgoing stream was never closed",
                )

                assertTrue(order.contains("replacement-greeted"), "nothing ever greeted; saw $order")
                assertTrue(order.contains("outgoing-closed"), "the outgoing channel was never shut down; saw $order")
                assertTrue(
                    order.indexOf("replacement-greeted") < order.indexOf("outgoing-closed"),
                    "the outgoing stream was retired for a replacement that had already died: $order",
                )
            }
        }
    }

    @Test
    fun `reconnects with backoff after the stream breaks`(@TempDir dir: Path) {
        FakeOperator("reconnect").use { operator ->
            val state = ServerState()
            loopAgainst(operator, state, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onError(Status.UNAVAILABLE.asRuntimeException())

                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
            }
        }
    }

    /**
     * The quantity nothing else on either side of the wire measures: how many
     * channels the agent leaves behind.
     *
     * Every other reconnect test here counts `operator.streams.size`, and
     * hack/agent-test.sh counts `stream_opened` — both taken on the operator's
     * side, where a channel the agent never shut down is invisible. A channel
     * is built per attempt, so an operator that is unreachable for the length
     * of a rolling update used to leave one behind per attempt: strongly
     * reachable through the `replaces` chain, so not collectable, and still
     * running gRPC's own reconnect loop underneath, aimed at the operator that
     * is trying to come back.
     *
     * The stream count below is asserted too, and deliberately: it is what
     * makes "every channel terminated" mean "every channel behind a stream
     * that broke" rather than "no channel was ever built".
     *
     * What this does not prove: that a socket and a reader thread are released.
     * The in-process transport has neither, and termination is the property
     * those hang off — the same limit `cancels the attempt it gives up on`
     * records.
     */
    @Test
    fun `shuts down the channel behind every stream that breaks`(@TempDir dir: Path) {
        FakeOperator("channels-released").use { operator ->
            val state = ServerState()
            val opened = Collections.synchronizedList(mutableListOf<ManagedChannel>())

            loopAgainst(
                operator,
                state,
                dir,
                channels = { operator.newChannel().also { opened.add(it) } },
                // The backoff sequence is asserted by the test below; here it
                // only has to be short enough that three failures fit in one
                // test, and it must not be zero, or the reconnects would race
                // the assertions rather than follow them.
                jitter = { 50L },
            ).use { loop ->
                loop.start()

                // Three breakages in a row, each on a channel of its own, and
                // never answered — so nothing takes the handover path and every
                // channel here is one only the reconnect path can release.
                repeat(3) { attempt ->
                    val stream = operator.awaitStream(attempt)
                    stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                    stream.toAgent.onError(Status.UNAVAILABLE.asRuntimeException())
                }
                operator.awaitStream(3).awaitMessage {
                    it.messageCase == ServerMessage.MessageCase.HELLO
                }

                assertEquals(4, opened.size, "the agent did not build one channel per attempt")
                opened.take(3).forEachIndexed { index, channel ->
                    assertTrue(
                        channel.awaitTermination(5, TimeUnit.SECONDS),
                        "the channel behind broken stream $index was never shut down; every " +
                            "failed attempt retains one for the length of the outage, each " +
                            "still retrying underneath",
                    )
                }
            }
        }
    }

    /**
     * The one assertion design section 9 names and nothing had: that the delay
     * between reconnects is `backoffMillis(attempt)` and that `attempt` moves.
     *
     * `backoff grows and is capped` tests the pure function and never runs the
     * loop; `reconnects with backoff after the stream breaks` runs the loop and
     * makes no timing assertion. Between them, pinning `attempt` at zero passed
     * everything — and a permanently unreachable operator dialled once a second
     * per pod, forever, which is the 1 Hz churn by another route.
     *
     * Both directions are here, because each is a separate defect. Growth is
     * the first three bases; the reset on the operator's answer — and not on a
     * Hello merely handed to the transport — is the fourth.
     */
    @Test
    fun `grows the backoff with every failed attempt and starts over once the operator answers`(
        @TempDir dir: Path,
    ) {
        FakeOperator("backoff-sequence").use { operator ->
            val state = ServerState()
            // The base handed to jitter is backoffMillis(attempt) exactly, so
            // recording it reads the sequence without waiting it out. The
            // return value collapses the wait: what is under test is the number
            // the loop computed, not the scheduler's ability to sleep on it.
            val bases = LinkedBlockingQueue<Long>()
            var connectCount = 0

            loopAgainst(
                operator,
                state,
                dir,
                channels = { if (connectCount++ < 3) FailingChannel() else operator.newChannel() },
                jitter = { base -> bases.add(base); 1L },
            ).use { loop ->
                loop.start()

                assertEquals(
                    listOf(1_000L, 2_000L, 4_000L),
                    listOf(nextBase(bases), nextBase(bases), nextBase(bases)),
                    "the reconnect delay did not grow with the number of failed attempts, so a " +
                        "permanently unreachable operator is dialled at the floor rate forever",
                )

                // The fourth attempt reaches the operator, and the operator
                // answers it. Waiting for the report rather than for the send
                // is what makes this the operator's answer having been
                // processed, not merely dispatched.
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                stream.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT }

                stream.toAgent.onError(Status.UNAVAILABLE.asRuntimeException())
                assertEquals(
                    1_000L,
                    nextBase(bases),
                    "the backoff was not reset by the operator answering, so a stream that is " +
                        "established and later breaks is retried as though the operator had " +
                        "never been reachable",
                )
            }
        }
    }

    private fun nextBase(bases: LinkedBlockingQueue<Long>): Long =
        bases.poll(5, TimeUnit.SECONDS)
            ?: throw AssertionError("the loop scheduled no further reconnect within 5s")

    @Test
    fun `reconnects when the stream fails before the session is established`(@TempDir dir: Path) {
        FakeOperator("early-failure").use { operator ->
            val state = ServerState()
            var connectCount = 0

            // The first attempt fails from inside stub.serverSession(), which is
            // before connect() reaches the point where the session becomes the
            // current one. A reconnect guarded on `current` would skip this case
            // and the agent would sit silent forever -- the one outcome worse
            // than reconnecting too eagerly.
            val loop = loopAgainst(
                operator,
                state,
                dir,
                channels = { if (connectCount++ == 0) FailingChannel() else operator.newChannel() },
            )

            loop.use {
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
            }
        }
    }

    @Test
    fun `retries when the very first connect throws`(@TempDir dir: Path) {
        FakeOperator("first-throws").use { operator ->
            val state = ServerState()
            var connectCount = 0

            // Every other call site of connect() is a scheduled one that
            // reschedules itself. start() is not, so a first attempt that throws
            // before the stream exists -- an endpoint that will not parse, a CA
            // bundle that will not load -- has to be caught here or the agent
            // never opens a session at all and never says why.
            loopAgainst(
                operator,
                state,
                dir,
                channels = {
                    if (connectCount++ == 0) throw IllegalStateException("the channel could not be built")
                    operator.newChannel()
                },
            ).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
            }
        }
    }

    @Test
    fun `does not reconnect after stop`(@TempDir dir: Path) {
        FakeOperator("stopped").use { operator ->
            val state = ServerState()
            // The delay has to be long enough that stop() lands before the
            // reconnect fires, and short enough that the wait below outlives it:
            // a window that ends before the reconnect was due would pass whether
            // or not stop() suppressed anything. Half a second of headroom
            // against the microseconds stop() needs, and a full second of
            // observation after the reconnect should have opened a stream.
            loopAgainst(operator, state, dir, jitter = { 500L }).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onError(Status.UNAVAILABLE.asRuntimeException())
                loop.stop()

                Thread.sleep(1500)
                assertEquals(
                    1,
                    operator.streams.size,
                    "a reconnect outlived the plugin that asked to be stopped",
                )
            }
        }
    }

    /**
     * The assertion is on the channel and not on the operator's view of the
     * stream, and the difference is the whole of what `stop()` now does.
     *
     * An attempt the operator has never answered is one it will never finish,
     * so `stop()` cancels it rather than half-closing it — the same choice
     * `answerOverdue` makes, for the same reason. A cancelled call that had not
     * yet been handed a transport never reaches the operator at all, so
     * `awaitStream(1)` is no longer something this test may wait for. That is
     * not a weaker statement: a stream nobody opened cannot outlive the plugin,
     * and termination covers the case where it did open as well.
     *
     * It is also strictly stronger than the close latch it replaces. Under a
     * graceful shutdown the channel waits for a call this operator never
     * finishes, so it would sit in SHUTDOWN forever while the latch — which
     * counts down on a half-close just as happily — reported success.
     */
    @Test
    fun `leaves no channel running when stop lands mid-connect`(@TempDir dir: Path) {
        FakeOperator("stopped-mid-connect").use { operator ->
            val state = ServerState()
            val started = java.util.concurrent.atomic.AtomicReference<SessionLoop?>(null)
            val opened = Collections.synchronizedList(mutableListOf<ManagedChannel>())
            var connectCount = 0

            // stop() runs after connect() has passed its entry check but before
            // the session is installed, so stop() finds nothing in `current` to
            // retire. Without connect()'s re-check at the end, the attempt would
            // outlive the plugin with nothing left holding a reference to close
            // it -- and its channel would still be running underneath.
            val loop = loopAgainst(
                operator,
                state,
                dir,
                channels = {
                    val channel = operator.newChannel().also { opened.add(it) }
                    if (connectCount++ == 1) started.get()!!.stop()
                    channel
                },
            )
            started.set(loop)

            loop.use {
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onError(Status.UNAVAILABLE.asRuntimeException())

                val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5)
                while (opened.size < 2 && System.nanoTime() < deadline) Thread.sleep(10)
                assertEquals(2, opened.size, "the reconnect stop() was meant to land in never ran")

                opened.forEachIndexed { index, channel ->
                    assertTrue(
                        channel.awaitTermination(5, TimeUnit.SECONDS),
                        "the channel behind attempt $index was still running after stop()",
                    )
                }
            }
        }
    }

    @Test
    fun `backoff grows and is capped`() {
        val delays = (0..10).map { SessionLoop.backoffMillis(it) }
        assertEquals(1_000L, delays[0])
        assertEquals(2_000L, delays[1])
        assertEquals(4_000L, delays[2])
        assertTrue(delays.all { it <= 30_000L }, "capped at 30s: $delays")
        assertEquals(30_000L, delays.last())
    }
}
