package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.Target
import cloud.spawnery.agent.api.Self
import cloud.spawnery.agent.api.ServerSelf
import cloud.spawnery.agent.pb.ConnectRequest
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.RosterEntry
import cloud.spawnery.agent.pb.ServerState
import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertSame
import kotlin.test.assertTrue

private val richPlayer: UUID = UUID.fromString("00000000-0000-4000-8000-00000000000a")

private fun aRichState(): NetworkState =
    NetworkState.newBuilder()
        .addGroups(
            GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                .setReplicas(2).setReadyReplicas(2).setOnlinePlayers(1).setFreeSlots(199),
        )
        .addServers(
            ServerState.newBuilder().setName("lobby-a").setGroup("lobby")
                .setPhase("Ready").setPlayers(1).setSlots(100).setRegistered(true),
        )
        .addServers(
            ServerState.newBuilder().setName("lobby-b").setGroup("lobby")
                .setPhase("Ready").setSlots(100).setRegistered(true),
        )
        .addPlayers(
            RosterEntry.newBuilder().setUuid(richPlayer.toString()).setName("alice")
                .setServer("lobby-a"),
        )
        .build()

private fun serverSelf(): ServerSelf = object : ServerSelf {
    override fun name(): String = "lobby-a"
    override fun group(): String = "lobby"
    override fun network(): String = "production"
    override fun slots(): Int = 100
}

private fun proxySelf(): ProxySelf = object : ProxySelf {
    override fun name(): String = "gateway-0"
    override fun group(): String = "gateway"
    override fun network(): String = "production"
}

class MirrorApiTest {
    private val requested = mutableListOf<ConnectRequest>()

    private fun connector() = CloudConnector(
        Requests(timeoutMillis = 1_000, clock = System::currentTimeMillis),
    ) { _, request -> requested += request }

    private fun api(self: Self, state: NetworkState = aRichState()): MirrorApi =
        MirrorApi(NetworkMirror().also { it.apply(state) }, self, connector())

    @Test
    fun `a lookup by name finds what the list holds`() {
        val api = api(serverSelf())

        assertEquals("lobby-a", api.server("lobby-a").orElseThrow().name())
        assertEquals("lobby", api.group("lobby").orElseThrow().name())
        assertEquals("alice", api.player(richPlayer).orElseThrow().name())
    }

    @Test
    fun `a lookup for something absent is empty rather than null`() {
        // Optional and not null, because a plugin that forgot a null check
        // gets an NPE at some later line while an empty Optional refuses at
        // the point of use.
        val api = MirrorApi(NetworkMirror(), serverSelf(), connector())

        assertTrue(api.server("nothing-here").isEmpty)
        assertTrue(api.group("nothing-here").isEmpty)
        assertTrue(api.player(UUID.randomUUID()).isEmpty)
    }

    @Test
    fun `self is whatever the platform supplied`() {
        val self = serverSelf()
        val api = MirrorApi(NetworkMirror(), self, connector())

        assertSame(self, api.self())
        // The type is how a plugin asks which side it is on, so it has to
        // survive the round trip rather than being flattened to Self.
        assertTrue(api.self() is ServerSelf)
    }

    // The spec's symmetry invariant. One implementation makes it structural,
    // so this is what is left to assert: given one state, the answers do not
    // depend on which side the API is running on. It would catch a future
    // `if (self is ProxySelf)` in a read method, which is the only way the two
    // sides could still come apart.
    @Test
    fun `both sides answer every read identically from one state`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        val onServer = MirrorApi(mirror, serverSelf(), connector())
        val onProxy = MirrorApi(mirror, proxySelf(), connector())

        assertEquals(onServer.groups(), onProxy.groups())
        assertEquals(onServer.servers(), onProxy.servers())
        assertEquals(onServer.players(), onProxy.players())
        assertEquals(onServer.server("lobby-a"), onProxy.server("lobby-a"))
        assertEquals(onServer.group("lobby"), onProxy.group("lobby"))
        assertEquals(onServer.player(richPlayer), onProxy.player(richPlayer))
    }

    // connect too, and it is the read the symmetry could most easily lose:
    // a proxy could answer it locally and a backend could not, which is
    // exactly the asymmetry section 3.1 refuses. Both must produce the same
    // request for the same arguments.
    @Test
    fun `both sides build the same request for the same connect`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        MirrorApi(mirror, serverSelf(), connector()).connect(richPlayer, Target.group("lobby"))
        MirrorApi(mirror, proxySelf(), connector()).connect(richPlayer, Target.group("lobby"))

        assertEquals(2, requested.size)
        assertEquals(requested[0], requested[1])
        assertEquals("lobby", requested[0].group)
    }
}
