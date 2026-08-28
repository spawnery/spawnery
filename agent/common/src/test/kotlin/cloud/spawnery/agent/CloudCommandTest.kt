package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.SpawneryApi
import cloud.spawnery.agent.pb.CloudRequest
import cloud.spawnery.agent.pb.CloudResponse
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.RequestError
import cloud.spawnery.agent.pb.RetireResult
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.ServerState
import com.mojang.brigadier.CommandDispatcher
import com.mojang.brigadier.exceptions.CommandSyntaxException
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertFalse
import kotlin.test.assertTrue

private fun aNetwork(): NetworkState =
    NetworkState.newBuilder()
        .addGroups(
            GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                .setReplicas(2).setReadyReplicas(1).setOnlinePlayers(12).setFreeSlots(88),
        )
        .addServers(
            ServerState.newBuilder().setName("lobby-a").setGroup("lobby")
                .setPhase("Ready").setPlayers(12).setSlots(100).setRegistered(true),
        )
        .build()

class CloudCommandTest {
    private val sent = mutableListOf<String>()
    private var permissions = setOf(PERMISSION_READ, PERMISSION_RETIRE)
    private val asked = mutableListOf<String>()

    // An Int as the source, which is the cheapest way to say that the tree
    // does not care what a source is.
    private val adapter = object : SourceAdapter<Int> {
        override fun hasPermission(source: Int, permission: String): Boolean {
            asked += permission
            return permission in permissions
        }

        override fun send(source: Int, message: String) {
            sent += message
        }
    }

    // A connector whose wire the test holds: requests land in `requested`,
    // and the test answers them the way the operator would. Nothing here fakes
    // SpawneryApi itself -- the real MirrorApi and the real CloudConnector are
    // under test, and only the socket is replaced.
    private val requested = mutableListOf<CloudRequest>()
    private val requests = Requests(timeoutMillis = 1_000, clock = System::currentTimeMillis)
    private val connector = CloudConnector(requests) { request -> requested += request }

    private fun api(): SpawneryApi = MirrorApi(
        NetworkMirror().also { it.apply(aNetwork()) },
        object : ProxySelf {
            override fun name(): String = "gateway-0"
            override fun group(): String = "gateway"
            override fun network(): String = "production"
        },
        connector,
    )

    private fun run(command: String, api: SpawneryApi = api()): Int {
        val dispatcher = CommandDispatcher<Int>()
        dispatcher.register(cloudCommand(api, adapter))
        return dispatcher.execute(command, 0)
    }

    /** Answers the one request outstanding, as the operator would. */
    private fun answer(build: CloudResponse.Builder.() -> Unit) {
        val id = requested.single().id
        connector.answer(CloudResponse.newBuilder().setId(id).apply(build).build())
    }

    @Test
    fun `list names every group and what it is doing`() {
        run("cloud list")

        assertTrue(sent.any { it.contains("lobby") }, "the output named no group: $sent")
        assertTrue(sent.single().contains("12 players"), "the output did not say what the group is doing: $sent")
    }

    @Test
    fun `a source without the permission cannot see the command at all`() {
        // Brigadier's requires makes the branch invisible rather than refused,
        // which is the platforms' own convention: somebody without it gets
        // "unknown command" and not a lecture.
        permissions = emptySet()

        assertFailsWith<CommandSyntaxException> { run("cloud list") }
        assertTrue(sent.isEmpty(), "an unpermitted source was sent something: $sent")
    }

    @Test
    fun `info about a server names its phase and whether it takes joins`() {
        run("cloud info lobby-a")

        val line = sent.single()
        assertTrue(line.contains("lobby-a") && line.contains("READY"), line)
        // Registered and not the phase: the two disagree during a drain, and
        // this is the one that answers "can I send somebody there".
        assertTrue(line.contains("taking joins"), line)
    }

    @Test
    fun `info about a group works through the same argument`() {
        run("cloud info lobby")

        assertTrue(sent.single().contains("88 free slots"), sent.toString())
    }

    @Test
    fun `info about something absent names what was asked for`() {
        // The failure mode this replaces: an empty line that leaves an admin
        // unsure whether they mistyped or the thing is gone.
        run("cloud info nothing-here")

        assertTrue(sent.single().contains("nothing-here"), "the answer did not name it: $sent")
    }

    @Test
    fun `the tree asks the platform for nothing but the two permissions it declares`() {
        // The structural claim, asserted. A tree that reached for anything
        // else could not have been written against this adapter, so what this
        // really guards is the adapter staying two methods.
        run("cloud list")
        run("cloud info lobby-a")
        run("cloud retire lobby-a")

        assertEquals(setOf(PERMISSION_READ, PERMISSION_RETIRE), asked.toSet())
    }

    @Test
    fun `retire asks the operator and says what retiring means`() {
        run("cloud retire lobby-a")

        assertEquals("lobby-a", requested.single().retire.server)
        // Nothing said yet: the answer has not arrived, and the command must
        // not have claimed anything on the strength of having asked.
        assertTrue(sent.isEmpty(), "the command answered before the operator did: $sent")

        answer { setRetire(RetireResult.newBuilder().setServer("lobby-a")) }

        val line = sent.single()
        assertTrue(line.contains("lobby-a") && line.contains("retiring"), line)
        // The sentence that stops an admin thinking they disconnected
        // everybody. Without it "retire" reads as "stop".
        assertTrue(line.contains("nobody is kicked"), "the output did not say what retiring does: $line")
    }

    @Test
    fun `a refusal reaches the source in the operator's own words`() {
        run("cloud retire lobby-a")

        answer {
            setError(
                RequestError.newBuilder()
                    .setReason(RequestError.Reason.REFUSED)
                    .setMessage("that server is already retiring"),
            )
        }

        val line = sent.single()
        assertTrue(line.contains("already retiring"), "the operator's reason was lost: $line")
        // And the plumbing is not in it. A CompletionStage wraps failures, and
        // "java.util.concurrent.CompletionException: ..." in a chat line tells
        // an admin nothing they can act on.
        assertFalse(line.contains("CompletionException"), "the future's wrapper reached chat: $line")
        assertTrue(line.startsWith("could not retire"), "a refusal was worded as a success: $line")
    }

    @Test
    fun `an agent with no session says so instead of throwing at the platform`() {
        // The dormant seam throws when asked to send. Brigadier would turn a
        // throw out of executes into "an internal error occurred", which tells
        // an admin nothing; the stage carries it instead, and the source is
        // told.
        val dormant = MirrorApi(
            NetworkMirror().also { it.apply(aNetwork()) },
            object : ProxySelf {
                override fun name(): String = "gateway-0"
                override fun group(): String = "gateway"
                override fun network(): String = "production"
            },
            dormantConnector(),
        )

        run("cloud retire lobby-a", dormant)

        assertTrue(sent.single().contains("no session"), "the source was not told why: $sent")
    }

    @Test
    fun `a wrapped failure is unwrapped before it reaches chat`() {
        // Not reachable through MirrorApi, which fails its own future and so
        // delivers the cause directly -- but SpawneryApi returns a
        // CompletionStage, and any implementation that derives one (a
        // thenApply, a handle) delivers a CompletionException instead. The
        // command is written against the interface, so this is the case the
        // interface allows and the one implementation happens not to produce.
        val wrapping = object : SpawneryApi by api() {
            override fun retire(server: String): java.util.concurrent.CompletionStage<Void> {
                val failed = java.util.concurrent.CompletableFuture<Void>()
                failed.completeExceptionally(IllegalStateException("REFUSED: that server is already retiring"))
                return failed.thenApply { it }
            }
        }

        run("cloud retire lobby-a", wrapping)

        val line = sent.single()
        assertTrue(line.contains("already retiring"), "the operator's reason was lost: $line")
        assertFalse(line.contains("CompletionException"), "the future's wrapper reached chat: $line")
        assertFalse(line.contains("IllegalStateException"), "the exception class reached chat: $line")
    }

    @Test
    fun `retire is invisible without its own permission even to a reader`() {
        // The split PERMISSION_RETIRE exists for: reading where people are is
        // what a moderator gets, and changing the fleet is not.
        permissions = setOf(PERMISSION_READ)

        assertFailsWith<CommandSyntaxException> { run("cloud retire lobby-a") }
        assertTrue(requested.isEmpty(), "an unpermitted source reached the operator: $requested")
    }

    @Test
    fun `holding only retire still opens the root`() {
        // A root gated on PERMISSION_READ would hide the whole tree from
        // somebody granted only PERMISSION_RETIRE, and hide it in the worst
        // way: the command would look as though it does not exist.
        permissions = setOf(PERMISSION_RETIRE)

        run("cloud retire lobby-a")

        assertEquals("lobby-a", requested.single().retire.server)
    }

    @Test
    fun `holding only retire still cannot read`() {
        // The other half of the same claim: widening the root must not have
        // widened a branch.
        permissions = setOf(PERMISSION_RETIRE)

        assertFailsWith<CommandSyntaxException> { run("cloud list") }
        assertTrue(sent.isEmpty(), "a source without the read permission was told something: $sent")
    }
}
