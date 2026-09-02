package cloud.spawnery.agent

import cloud.spawnery.agent.api.ServerPhase
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.RosterEntry
import cloud.spawnery.agent.pb.ServerState
import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

private val someUuid: String = "00000000-0000-4000-8000-00000000000a"

private fun state(
    groups: List<String> = emptyList(),
    servers: List<String> = emptyList(),
    phase: String = "Ready",
    players: List<Pair<String, String>> = emptyList(),
    playerServer: String = "lobby-a",
): NetworkState {
    val b = NetworkState.newBuilder()
    for (g in groups) {
        b.addGroups(GroupState.newBuilder().setName(g).setKind(GroupState.Kind.EPHEMERAL))
    }
    for (s in servers) {
        b.addServers(
            ServerState.newBuilder().setName(s).setGroup("lobby").setPhase(phase).setSlots(100),
        )
    }
    for ((uuid, name) in players) {
        b.addPlayers(
            RosterEntry.newBuilder().setUuid(uuid).setName(name).setServer(playerServer),
        )
    }
    return b.build()
}

class NetworkMirrorTest {
    @Test
    fun `a mirror that has been told nothing answers empty rather than null`() {
        val mirror = NetworkMirror()
        assertEquals(emptyList(), mirror.groups())
        assertEquals(emptyList(), mirror.servers())
        assertEquals(emptyList(), mirror.players())
    }

    @Test
    fun `applying a state replaces what came before rather than merging`() {
        val mirror = NetworkMirror()
        mirror.apply(state(servers = listOf("lobby-a", "lobby-b")))
        mirror.apply(state(servers = listOf("lobby-b")))

        assertEquals(listOf("lobby-b"), mirror.servers().map { it.name() })
    }

    @Test
    fun `a group carries what somebody wrote down about it`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addGroups(
                GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                    .putAttributes("permission", "task.build"),
            ).build(),
        )

        assertEquals("task.build", mirror.groups().single().attributes()["permission"])
    }

    @Test
    fun `a group nobody described carries an empty map rather than null`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addGroups(
                GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL),
            ).build(),
        )

        assertTrue(mirror.groups().single().attributes().isEmpty())
    }

    @Test
    fun `a group carries the name a person gave it`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addGroups(
                GroupState.newBuilder().setName("bingo-team").setKind(GroupState.Kind.EPHEMERAL)
                    .setDisplayName("Bingo-Team"),
            ).build(),
        )

        assertEquals("Bingo-Team", mirror.groups().single().displayName())
    }

    @Test
    fun `a group nobody named is displayed by its own name`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addGroups(
                GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL),
            ).build(),
        )

        assertEquals("lobby", mirror.groups().single().displayName())
    }

    @Test
    fun `a server carries what it said about itself`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addServers(
                ServerState.newBuilder().setName("lobby-a").setGroup("lobby")
                    .setPhase("Ready").setState("running")
                    .putAttributes("map", "arena"),
            ).build(),
        )

        val server = mirror.servers().single()
        assertEquals("running", server.state())
        assertEquals("arena", server.attributes()["map"])
    }

    @Test
    fun `a server says which run of it this is`() {
        // The name cannot say it: a persistent server keeps its name across
        // every restart, because that name is the identity of its world.
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addServers(
                ServerState.newBuilder().setName("survival-0").setGroup("survival")
                    .setPhase("Ready").setIncarnation("pod-7c3f"),
            ).build(),
        )

        assertEquals("pod-7c3f", mirror.servers().single().incarnation())
    }

    @Test
    fun `a server the operator has not placed yet carries an empty incarnation`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addServers(
                ServerState.newBuilder().setName("lobby-a").setGroup("lobby").setPhase("Pending"),
            ).build(),
        )

        assertEquals("", mirror.servers().single().incarnation())
    }

    @Test
    fun `a server that said nothing carries an empty description rather than null`() {
        // Which is also every server on a network whose operator predates the
        // verb. A plugin asking what a server is doing should not have to
        // write a null check to be told that it is doing nothing in
        // particular.
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addServers(
                ServerState.newBuilder().setName("lobby-a").setGroup("lobby").setPhase("Ready"),
            ).build(),
        )

        val server = mirror.servers().single()
        assertEquals("", server.state())
        assertTrue(server.attributes().isEmpty())
    }

    @Test
    fun `a phase this jar predates becomes UNKNOWN rather than throwing`() {
        val mirror = NetworkMirror()
        mirror.apply(state(servers = listOf("lobby-a"), phase = "SomethingLaterInvented"))

        assertEquals(ServerPhase.UNKNOWN, mirror.servers().single().phase())
    }

    @Test
    fun `a player on no backend carries an empty Optional`() {
        val mirror = NetworkMirror()
        mirror.apply(state(players = listOf(someUuid to "alice"), playerServer = ""))

        assertTrue(mirror.players().single().server().isEmpty)
    }

    @Test
    fun `a player entry with an unparseable uuid is dropped rather than failing the apply`() {
        // The operator sends what a proxy reported. One malformed entry must
        // not cost this agent its whole mirror -- ProxyRole's own guard makes
        // the same trade, and it is why a FullSync skips a bad address rather
        // than discarding the sync.
        val mirror = NetworkMirror()
        mirror.apply(state(players = listOf("not-a-uuid" to "mallory", someUuid to "alice")))

        assertEquals(listOf("alice"), mirror.players().map { it.name() })
        assertEquals(UUID.fromString(someUuid), mirror.players().single().id())
    }

    @Test
    fun `groups carry the kind the operator sent`() {
        val mirror = NetworkMirror()
        mirror.apply(state(groups = listOf("lobby")))

        assertEquals("lobby", mirror.groups().single().name())
    }
}
