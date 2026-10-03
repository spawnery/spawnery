package cloud.spawnery.agent

import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.Target
import cloud.spawnery.agent.api.Self
import cloud.spawnery.agent.api.ServerSelf
import cloud.spawnery.agent.pb.CloudRequest
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.ProxyState
import cloud.spawnery.agent.pb.RosterEntry
import cloud.spawnery.agent.pb.ServerState
import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
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
    private val requested = mutableListOf<CloudRequest>()

    private fun connector() = CloudConnector(
        Requests(timeoutMillis = 1_000, clock = System::currentTimeMillis),
    ) { request -> requested += request }

    private fun api(self: Self, state: NetworkState = aRichState()): MirrorApi =
        MirrorApi(NetworkMirror().also { it.apply(state) }, self, connector(), CloudEvents())

    @Test
    fun `a lookup by name finds what the list holds`() {
        val api = api(serverSelf())

        assertEquals("lobby-a", api.server("lobby-a").orElseThrow().name())
        assertEquals("lobby", api.group("lobby").orElseThrow().name())
        assertEquals("alice", api.player(richPlayer).orElseThrow().name())
    }

    @Test
    fun `a lookup for something absent is empty rather than null`() {
        val api = MirrorApi(NetworkMirror(), serverSelf(), connector(), CloudEvents())

        assertTrue(api.server("nothing-here").isEmpty)
        assertTrue(api.group("nothing-here").isEmpty)
        assertTrue(api.player(UUID.randomUUID()).isEmpty)
    }

    @Test
    fun `self is whatever the platform supplied`() {
        val self = serverSelf()
        val api = MirrorApi(NetworkMirror(), self, connector(), CloudEvents())

        assertSame(self, api.self())
        // The type is how a plugin asks which side it is on.
        assertTrue(api.self() is ServerSelf)
    }

    // Given one state, the answers do not depend on which side the API runs on.
    @Test
    fun `both sides answer every read identically from one state`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        val onServer = MirrorApi(mirror, serverSelf(), connector(), CloudEvents())
        val onProxy = MirrorApi(mirror, proxySelf(), connector(), CloudEvents())

        assertEquals(onServer.groups(), onProxy.groups())
        assertEquals(onServer.servers(), onProxy.servers())
        assertEquals(onServer.players(), onProxy.players())
        assertEquals(onServer.server("lobby-a"), onProxy.server("lobby-a"))
        assertEquals(onServer.group("lobby"), onProxy.group("lobby"))
        assertEquals(onServer.player(richPlayer), onProxy.player(richPlayer))
    }

    // A proxy could answer connect locally and a backend could not.
    @Test
    fun `both sides build the same request for the same connect`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        MirrorApi(mirror, serverSelf(), connector(), CloudEvents()).connect(richPlayer, Target.group("lobby"))
        MirrorApi(mirror, proxySelf(), connector(), CloudEvents()).connect(richPlayer, Target.group("lobby"))

        assertEquals(2, requested.size)
        // The envelope's correlation id is per-connector state.
        assertEquals(requested[0].connect, requested[1].connect)
        assertEquals("lobby", requested[0].connect.group)
    }

    @Test
    fun `both sides build the same request for the same retire`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        MirrorApi(mirror, serverSelf(), connector(), CloudEvents()).retire("lobby-a")
        MirrorApi(mirror, proxySelf(), connector(), CloudEvents()).retire("lobby-a")

        assertEquals(2, requested.size)
        assertEquals(requested[0].retire, requested[1].retire)
        assertEquals("lobby-a", requested[0].retire.server)
    }

    @Test
    fun `both sides build the same request for the same unretire`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        MirrorApi(mirror, serverSelf(), connector(), CloudEvents()).unretire("lobby-a")
        MirrorApi(mirror, proxySelf(), connector(), CloudEvents()).unretire("lobby-a")

        assertEquals(2, requested.size)
        assertEquals(requested[0].unretire, requested[1].unretire)
        assertEquals("lobby-a", requested[0].unretire.server)
    }

    @Test
    fun `both sides build the same request for the same start and stop`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        val onServer = MirrorApi(mirror, serverSelf(), connector(), CloudEvents())
        val onProxy = MirrorApi(mirror, proxySelf(), connector(), CloudEvents())
        onServer.startServer("private-servers", "c0ffee")
        onProxy.startServer("private-servers", "c0ffee")
        onServer.stopServer("private-servers-c0ffee")
        onProxy.stopServer("private-servers-c0ffee")

        assertEquals(4, requested.size)
        assertEquals(requested[0].startServer, requested[1].startServer)
        assertEquals("c0ffee", requested[0].startServer.key)
        assertEquals(requested[2].stopServer, requested[3].stopServer)
        assertEquals("private-servers-c0ffee", requested[2].stopServer.server)
    }

    // Only one side can announce, but which one is the operator's rule, not this client's.
    @Test
    fun `both sides build the same request for the same announcement`() {
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        MirrorApi(mirror, serverSelf(), connector(), CloudEvents())
            .announce("running", mapOf("map" to "arena"))
        MirrorApi(mirror, proxySelf(), connector(), CloudEvents())
            .announce("running", mapOf("map" to "arena"))

        assertEquals(2, requested.size)
        assertEquals(requested[0].announce, requested[1].announce)
        assertEquals("running", requested[0].announce.state)
        assertEquals("arena", requested[0].announce.attributesMap["map"])
    }

    @Test
    fun `both sides build the same request for the same door`() {
        // Which side may close a door is the operator's rule.
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        MirrorApi(mirror, serverSelf(), connector(), CloudEvents()).acceptJoins(false)
        MirrorApi(mirror, proxySelf(), connector(), CloudEvents()).acceptJoins(false)

        assertEquals(2, requested.size)
        assertEquals(requested[0].acceptJoins, requested[1].acceptJoins)
        assertEquals(false, requested[0].acceptJoins.accept)
    }

    @Test
    fun `both sides build the same request for the same round end`() {
        // Which side may end a round is the operator's rule.
        val mirror = NetworkMirror().also { it.apply(aRichState()) }
        MirrorApi(mirror, serverSelf(), connector(), CloudEvents()).endRound()
        MirrorApi(mirror, proxySelf(), connector(), CloudEvents()).endRound()

        assertEquals(2, requested.size)
        assertEquals(requested[0].acceptJoins, requested[1].acceptJoins)
        assertEquals(false, requested[0].acceptJoins.accept)
        assertTrue(requested[0].acceptJoins.roundEnded)
    }

    @Test
    fun `holdReadiness reaches the gate on a server`() {
        val gate = ReadinessGate {}
        val api = MirrorApi(
            NetworkMirror(), serverSelf(), connector(), CloudEvents(), gate,
        )

        api.holdReadiness("mappings")

        assertEquals(listOf("mappings"), gate.openReasons())
    }

    @Test
    fun `holdReadiness refuses on a proxy`() {
        // Never leaves the process, so there is no stage to fail.
        val api = MirrorApi(NetworkMirror(), proxySelf(), connector(), CloudEvents())

        assertFailsWith<UnsupportedOperationException> { api.holdReadiness("mappings") }
    }

    @Test
    fun `an announcement with nothing in it is what clears a description`() {
        // An empty announcement clears the previous one.
        MirrorApi(NetworkMirror(), serverSelf(), connector(), CloudEvents())
            .announce("", emptyMap())

        assertEquals(1, requested.size)
        assertEquals("", requested[0].announce.state)
        assertTrue(requested[0].announce.attributesMap.isEmpty())
    }

    @Test
    fun `servers and proxies carry the node they run on`() {
        val api = api(
            proxySelf(),
            NetworkState.newBuilder()
                .addServers(ServerState.newBuilder().setName("lobby-a").setGroup("lobby").setPhase("Ready").setNode("node-2"))
                .addProxies(ProxyState.newBuilder().setName("gateway-a").setGroup("gateway").setNode("node-3"))
                .build(),
        )
        assertEquals("node-2", api.server("lobby-a").get().node())
        assertEquals("node-3", api.proxy("gateway-a").get().node())
    }

    @Test
    fun `playableSlots refuses on a proxy`() {
        val api = MirrorApi(NetworkMirror(), proxySelf(), connector(), CloudEvents())

        assertFailsWith<UnsupportedOperationException> { api.playableSlots(12) }
    }

    @Test
    fun `playableSlots refuses a negative figure and hands the rest to the server`() {
        val set = mutableListOf<Int>()
        val api = MirrorApi(NetworkMirror(), serverSelf(), connector(), CloudEvents(), playable = { set += it })

        assertFailsWith<IllegalArgumentException> { api.playableSlots(-1) }
        api.playableSlots(12)
        api.playableSlots(0)
        assertEquals(listOf(12, 0), set)
    }
}
