package cloud.spawnery.agent

import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.ServerMessage
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit

/**
 * The seam between [SessionLoop] and [AgentRole], independent of which message
 * produced which directive: each test dictates the directive and sends a
 * message the production mapping does not recognise.
 */
class AgentRoleSeamTest {
    private val scheduler: ScheduledExecutorService = Executors.newSingleThreadScheduledExecutor()

    @AfterEach fun shutdown() { scheduler.shutdownNow() }

    private fun loopAgainst(
        operator: FakeOperator,
        role: FakeRole,
        dir: Path,
        fallbackAnswerBoundMillis: Long = SessionLoop.FALLBACK_ANSWER_BOUND_MILLIS,
    ): SessionLoop<ServerMessage, OperatorToServer> {
        val token = dir.resolve("token")
        Files.writeString(token, "test-token")
        return SessionLoop(
            channels = { operator.newChannel() },
            credentials = BearerCredentials.of(TokenSource(token)),
            role = role,
            scheduler = scheduler,
            version = "26.2-0.2.0",
            log = { _, _ -> },
            note = { },
            jitter = { it },
            fallbackAnswerBoundMillis = fallbackAnswerBoundMillis,
            expireRequests = {},
        )
    }

    /** The production mapping answers [Directive.None] for this. */
    private fun unrecognised(): OperatorToServer = OperatorToServer.getDefaultInstance()

    @Test
    fun `a report directive starts the reporting timer`(@TempDir dir: Path) {
        FakeOperator("seam-report").use { operator ->
            val role = FakeRole { Directive.Report(1) }.apply { sample(players = 2, slots = 20) }
            loopAgainst(operator, role, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                stream.toAgent.onNext(unrecognised())

                val report = stream.awaitMessage {
                    it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT
                }
                assertEquals(2, report.playerCount.players)
                assertEquals(20, report.playerCount.slots)
                assertEquals(
                    listOf<Directive>(Directive.Report(1)),
                    role.directives.toList(),
                    "the loop read the message itself instead of asking the role",
                )
            }
        }
    }

    @Test
    fun `a deadline directive schedules a renewal and sets the answer bound`(@TempDir dir: Path) {
        FakeOperator("seam-deadline").use { operator ->
            val role = FakeRole { Directive.Deadline(1, 1) }
            // A minute, so a give-up within five seconds can only be hardDeadlineSeconds.
            loopAgainst(operator, role, dir, fallbackAnswerBoundMillis = 60_000).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onNext(unrecognised())

                // Nothing but renewAfterSeconds opens a second stream.
                val second = operator.awaitStream(1)
                second.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                assertTrue(
                    second.closed.await(5, TimeUnit.SECONDS),
                    "the renewed attempt was never given up on, so the directive's " +
                        "hardDeadlineSeconds never became the answer bound",
                )
                assertEquals(
                    "cancelled",
                    second.terminal.get(),
                    "the attempt the agent gave up on was half-closed rather than cancelled",
                )
                assertEquals(
                    listOf<Directive>(Directive.Deadline(1, 1)),
                    role.directives.toList(),
                    "the loop read the message itself instead of asking the role",
                )
            }
        }
    }

    @Test
    fun `an unrecognised message produces no directive and no scheduling`(@TempDir dir: Path) {
        FakeOperator("seam-none").use { operator ->
            val role = FakeRole().apply { sample(players = 2, slots = 20) }
            loopAgainst(operator, role, dir).use { loop ->
                loop.start()
                val first = operator.awaitStream(0)
                first.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                first.toAgent.onNext(unrecognised())

                // A report and a renewal are each at least one second.
                Thread.sleep(1500)
                assertEquals(
                    listOf<Directive>(Directive.None),
                    role.directives.toList(),
                    "a message the role does not recognise is None and nothing else",
                )
                assertTrue(
                    first.received.none {
                        it.messageCase == ServerMessage.MessageCase.PLAYER_COUNT
                    },
                    "a message the role does not recognise started the reporting timer",
                )
                assertEquals(
                    1,
                    operator.streams.size,
                    "a message the role does not recognise scheduled a renewal",
                )
            }
        }
    }

    @Test
    fun `the hello the role builds is the first message on the wire`(@TempDir dir: Path) {
        FakeOperator("seam-hello").use { operator ->
            val role = FakeRole().apply { markReady() }
            loopAgainst(operator, role, dir).use { loop ->
                loop.start()
                val stream = operator.awaitStream(0)
                stream.awaitMessage { it.messageCase == ServerMessage.MessageCase.HELLO }

                assertEquals(1, role.hellos.size, "the loop greeted more than once, or not at all")
                assertEquals(
                    "26.2-0.2.0",
                    role.hellos[0].hello.version,
                    "the loop did not hand the role the version it was constructed with",
                )
                assertEquals(
                    role.hellos[0],
                    stream.received.first(),
                    "the first message on the wire was not the one the role built",
                )
            }
        }
    }
}
