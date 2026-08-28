package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.SpawneryApi
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.ServerState
import com.mojang.brigadier.CommandDispatcher
import com.mojang.brigadier.exceptions.CommandSyntaxException
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
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
    private var permissions = setOf(PERMISSION_READ)
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

    private fun api(): SpawneryApi = MirrorApi(
        NetworkMirror().also { it.apply(aNetwork()) },
        object : ProxySelf {
            override fun name(): String = "gateway-0"
            override fun group(): String = "gateway"
            override fun network(): String = "production"
        },
        dormantConnector(),
    )

    private fun run(command: String): Int {
        val dispatcher = CommandDispatcher<Int>()
        dispatcher.register(cloudCommand(api(), adapter))
        return dispatcher.execute(command, 0)
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
    fun `the tree asks the platform for nothing but the read permission`() {
        // The structural claim, asserted. A tree that reached for anything
        // else could not have been written against this adapter, so what this
        // really guards is the adapter staying two methods.
        run("cloud list")
        run("cloud info lobby-a")

        assertEquals(setOf(PERMISSION_READ), asked.toSet())
    }
}
