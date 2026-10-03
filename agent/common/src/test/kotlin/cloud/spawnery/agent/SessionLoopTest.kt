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
 * A [ManagedChannel] that reports, in program order, when a message is sent on
 * it and when it is shut down. That is what the agent did locally, not what the
 * operator saw, so it cannot establish make before break on its own.
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
 * A channel whose calls fail synchronously from inside `stub.serverSession()`,
 * before [SessionLoop] has installed the session: any stream that dies before
 * it is established.
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
        role: FakeRole,
        dir: Path,
        channels: () -> ManagedChannel = { operator.newChannel() },
        // Identity, so a test's delays are exactly the ones it wrote down.
        jitter: (Long) -> Long = { it },
        fallbackAnswerBoundMillis: Long = SessionLoop.FALLBACK_ANSWER_BOUND_MILLIS,
        onStreamChanged: () -> Unit = {},
        log: (String, Throwable?) -> Unit = { _, _ -> },
        note: (String) -> Unit = { },
    ): SessionLoop<ServerMessage, OperatorToServer> {
        val token = dir.resolve("token")
        Files.writeString(token, "test-token")
        return SessionLoop(
            channels = channels,
            credentials = BearerCredentials.of(TokenSource(token)),
            role = role,
            scheduler = scheduler,
            version = "26.2-0.2.0",
            log = log,
            note = note,
            jitter = jitter,
            fallbackAnswerBoundMillis = fallbackAnswerBoundMillis,
            onStreamChanged = onStreamChanged,
        )
    }

    @Test
    fun `greets with the version and the current readiness`(@TempDir dir: Path) {
        FakeOperator("greets").use { operator ->
            val role = FakeRole().apply { markReady() }
            loopAgainst(operator, role, dir).use { loop ->
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
            val role = FakeRole()
            loopAgainst(operator, role, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                val hello = stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                assertEquals(false, hello.hello.ready)

                role.markReady()
                loop.send(role.ready())

                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.READY }
            }
        }
    }

    @Test
    fun `reports the player count at the interval the operator dictates`(@TempDir dir: Path) {
        FakeOperator("reports").use { operator ->
            val role = FakeRole().apply { sample(players = 3, slots = 100) }
            loopAgainst(operator, role, dir).use { loop ->
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
            val role = FakeRole().apply { sample(players = 3, slots = 100) }
            loopAgainst(operator, role, dir).use { loop ->
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
            val role = FakeRole()

            loopAgainst(operator, role, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // Get the first session reporting, so a leaked session would
                // keep firing on the shared scheduler after the second connect().
                first.toAgent.onNext(
                    OperatorToServer.newBuilder()
                        .setReportInterval(ReportInterval.newBuilder().setSeconds(1))
                        .build(),
                )
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT }

                // Reconnect while the first session is still live, no stop()
                // in between.
                loop.start()
                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // Make before break: until the operator answers the new
                // stream, the outgoing one stays up.
                assertFalse(
                    first.closed.await(1, TimeUnit.SECONDS),
                    "the previous session was retired before the operator answered the new one",
                )

                // Once it answers, the previous stream closes and its reporting
                // stops.
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
            val role = FakeRole().apply { markReady() }

            loopAgainst(operator, role, dir).use { loop ->
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

                // The shortest backoff is a second.
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
            val role = FakeRole().apply { markReady() }

            loopAgainst(operator, role, dir).use { loop ->
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

                // The real operator's order: it cancels the displaced stream at
                // the replacement's handler entry, so the outgoing stream fails
                // with Unavailable before the replacement is answered.
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
    fun `does not report a stream the operator retired for a renewal as a failure`(
        @TempDir dir: Path,
    ) {
        val warned = Collections.synchronizedList(mutableListOf<Pair<String, Throwable?>>())
        val noted = Collections.synchronizedList(mutableListOf<String>())

        FakeOperator("renewal-is-not-a-failure").use { operator ->
            val role = FakeRole().apply { markReady() }

            loopAgainst(
                operator,
                role,
                dir,
                log = { message, cause -> warned += message to cause },
                note = { message -> noted += message },
            ).use { loop ->
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

                // The operator ends the displaced stream with Unavailable on
                // every renewal, by design.
                first.toAgent.onError(
                    Status.UNAVAILABLE
                        .withDescription("session ended, reconnect with a fresh token")
                        .asRuntimeException(),
                )

                Thread.sleep(500)

                assertTrue(
                    warned.isEmpty(),
                    "a renewal was reported on the channel for things going wrong: $warned",
                )
                assertTrue(
                    noted.any { it.contains("renewal") },
                    "the retirement was not reported at all: $noted",
                )
            }
        }
    }

    @Test
    fun `reports a stream that broke on its own with what broke it`(@TempDir dir: Path) {
        val logged = Collections.synchronizedList(mutableListOf<Pair<String, Throwable?>>())

        FakeOperator("breakage-keeps-its-cause").use { operator ->
            val role = FakeRole().apply { markReady() }

            loopAgainst(operator, role, dir, log = { message, cause ->
                logged += message to cause
            }).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // No replacement was ever opened.
                first.toAgent.onError(
                    Status.UNAVAILABLE.withDescription("connection reset").asRuntimeException(),
                )

                Thread.sleep(500)

                assertTrue(
                    logged.any { it.second != null },
                    "a stream that broke on its own lost its cause: $logged",
                )
            }
        }
    }

    @Test
    fun `books one reconnect when the replacement dies after the operator retired the outgoing stream`(
        @TempDir dir: Path,
    ) {
        FakeOperator("replacement-dies-late").use { operator ->
            val role = FakeRole().apply { markReady() }

            loopAgainst(operator, role, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // A hard deadline past the end of this test, so no answer
                // deadline adds a reconnect of its own.
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

                // The outgoing stream fails first and skips the reconnect: the
                // replacement owes it.
                first.toAgent.onError(
                    Status.UNAVAILABLE
                        .withDescription("session ended, reconnect with a fresh token")
                        .asRuntimeException(),
                )
                // Then the replacement dies too, asynchronously and still
                // unanswered.
                second.toAgent.onError(
                    Status.UNAVAILABLE.withDescription("connection reset").asRuntimeException(),
                )

                // Exactly one reconnect: none would be permanent silence, two
                // the storm the skip exists to prevent.
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
            val role = FakeRole().apply { markReady() }

            loopAgainst(operator, role, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                // The hard deadline is what bounds the wait below.
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

                // The operator retires the displaced stream and then says
                // nothing on the replacement, as when it blocks between the
                // cancel and its first Send.
                first.toAgent.onError(
                    Status.UNAVAILABLE
                        .withDescription("session ended, reconnect with a fresh token")
                        .asRuntimeException(),
                )

                // Only the agent's own answer bound can open the next stream.
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
     * The give-up has to cancel the call, not half-close it: an operator that is
     * not answering never finishes the call, so a graceful shutdown would leave
     * the channel in SHUTDOWN forever. The `closed` latch counts down on both,
     * so this checks the terminal state and that the channel terminates. The
     * in-process transport has no socket or reader thread to check directly.
     */
    @Test
    fun `cancels the attempt it gives up on instead of waiting for an answer that is not coming`(
        @TempDir dir: Path,
    ) {
        FakeOperator("mute-parks-transport").use { operator ->
            val role = FakeRole().apply { markReady() }
            val opened = Collections.synchronizedList(mutableListOf<ManagedChannel>())

            loopAgainst(
                operator,
                role,
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
     * The same bound before the operator has sent any `SessionDeadline`, which
     * is the fallback; this test shortens it from its production five minutes.
     */
    @Test
    fun `bounds a first attempt the operator has not given it a deadline for`(
        @TempDir dir: Path,
    ) {
        FakeOperator("mute-from-the-start").use { operator ->
            val role = FakeRole().apply { markReady() }

            loopAgainst(operator, role, dir, fallbackAnswerBoundMillis = 500).use { loop ->
                loop.start()

                // Accepted, greeted, and never answered.
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
            val role = FakeRole().apply { markReady() }
            val order = Collections.synchronizedList(mutableListOf<String>())
            var connectCount = 0

            // The renewal's attempt dies from inside stub.serverSession(). The
            // outgoing stream still lives until its hard deadline, so retiring
            // it for that replacement would be break before make.
            val loop = loopAgainst(
                operator,
                role,
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

                // The failed renewal books a reconnect, and that one succeeds.
                val replacement = operator.awaitStream(1)
                replacement.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

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
            val role = FakeRole()
            loopAgainst(operator, role, dir).use { loop ->
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
     * Counts the channels the agent leaves behind, which the operator's side
     * cannot see. The channel count makes "every channel terminated" mean
     * "every channel behind a broken stream", not "no channel was built".
     */
    @Test
    fun `shuts down the channel behind every stream that breaks`(@TempDir dir: Path) {
        FakeOperator("channels-released").use { operator ->
            val role = FakeRole()
            val opened = Collections.synchronizedList(mutableListOf<ManagedChannel>())

            loopAgainst(
                operator,
                role,
                dir,
                channels = { operator.newChannel().also { opened.add(it) } },
                // Not zero, or the reconnects would race the assertions rather
                // than follow them.
                jitter = { 50L },
            ).use { loop ->
                loop.start()

                // Never answered, so only the reconnect path can release these
                // channels.
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
     * The loop itself, not just `backoffMillis`: `attempt` grows with every
     * failure and only the operator's answer resets it.
     */
    @Test
    fun `grows the backoff with every failed attempt and starts over once the operator answers`(
        @TempDir dir: Path,
    ) {
        FakeOperator("backoff-sequence").use { operator ->
            val role = FakeRole()
            // jitter receives backoffMillis(attempt) exactly; returning 1 ms
            // reads the sequence without waiting it out.
            val bases = LinkedBlockingQueue<Long>()
            var connectCount = 0

            loopAgainst(
                operator,
                role,
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

                // Waiting for the report means the answer was processed, not
                // merely sent.
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
            val role = FakeRole()
            var connectCount = 0

            // The first attempt fails before connect() installs the session, so
            // a reconnect guarded on `current` would never fire.
            val loop = loopAgainst(
                operator,
                role,
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
            val role = FakeRole()
            var connectCount = 0

            loopAgainst(
                operator,
                role,
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
            val role = FakeRole()
            // Long enough for stop() to land first, short enough that the wait
            // below outlives it; otherwise this passes whether or not stop()
            // suppressed anything.
            loopAgainst(operator, role, dir, jitter = { 500L }).use { loop ->
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
     * Asserts on the channels, not on the operator's view: `stop()` cancels an
     * unanswered attempt, and a call cancelled before it had a transport never
     * reaches the operator, so `awaitStream(1)` cannot be waited for.
     */
    @Test
    fun `leaves no channel running when stop lands mid-connect`(@TempDir dir: Path) {
        FakeOperator("stopped-mid-connect").use { operator ->
            val role = FakeRole()
            val started =
                java.util.concurrent.atomic.AtomicReference<SessionLoop<ServerMessage, OperatorToServer>?>(null)
            val opened = Collections.synchronizedList(mutableListOf<ManagedChannel>())
            var connectCount = 0

            // stop() runs after connect()'s entry check but before the session
            // is installed, so only connect()'s re-check at the end retires it.
            val loop = loopAgainst(
                operator,
                role,
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

    @Test
    fun `every stream that becomes current tells the agent it changed`(@TempDir dir: Path) {
        val changes = java.util.concurrent.atomic.AtomicInteger(0)
        FakeOperator("stream-changed").use { operator ->
            val role = FakeRole().apply { markReady() }

            loopAgainst(operator, role, dir, onStreamChanged = { changes.incrementAndGet() }).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }
                assertEquals(1, changes.get(), "the first stream did not report a change")

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
                assertEquals(2, changes.get(), "the renewal's stream did not report a change")
            }
        }
    }
}
