package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.api.ServerPhase
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.ProxyState
import cloud.spawnery.agent.pb.RosterEntry
import cloud.spawnery.agent.pb.ServerState
import java.util.UUID
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNull
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
    fun `a group's kind is found by name, and a group not in the picture is unknown`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addGroups(GroupState.newBuilder().setName("challenge").setKind(GroupState.Kind.ON_DEMAND))
                .build(),
        )

        assertEquals(Group.Kind.ON_DEMAND, mirror.groupKind("challenge"))
        assertEquals(Group.Kind.UNKNOWN, mirror.groupKind("lobby"))
    }

    @Test
    fun `a group's join rule is mirrored, and a group without one has none`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addGroups(GroupState.newBuilder().setName("vip").setJoinPermission("network.vip"))
                .addGroups(
                    GroupState.newBuilder().setName("hub")
                        .setJoinPermission("network.banned").setJoinPermissionDenyOnly(true),
                )
                .addGroups(GroupState.newBuilder().setName("lobby"))
                .build(),
        )
        assertEquals(JoinRule("network.vip", denyOnly = false), mirror.joinRule("vip"))
        assertEquals(JoinRule("network.banned", denyOnly = true), mirror.joinRule("hub"))
        assertNull(mirror.joinRule("lobby"))
        assertNull(mirror.joinRule("nowhere"))
    }

    @Test
    fun `a group's admission comes from its state`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addGroups(GroupState.newBuilder().setName("duels").setPlayableSlots(12).setEnforcePlayableSlots(true))
                .build(),
        )
        assertEquals(GroupAdmission(playableSlots = 12, enforce = true), mirror.admission("duels"))
    }

    @Test
    fun `absent fields mean not enforced`() {
        val mirror = NetworkMirror()
        mirror.apply(NetworkState.newBuilder().addGroups(GroupState.newBuilder().setName("duels")).build())
        assertEquals(GroupAdmission(playableSlots = 0, enforce = false), mirror.admission("duels"))
        assertEquals(null, mirror.admission("unknown"))
    }

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
        // A persistent server keeps its name across restarts.
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
        // Also every server under an operator that predates the verb.
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
    fun `an on-demand group is named as one`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addGroups(
                GroupState.newBuilder().setName("private-servers").setKind(GroupState.Kind.ON_DEMAND),
            ).build(),
        )

        assertEquals(Group.Kind.ON_DEMAND, mirror.groups().single().kind())
    }

    @Test
    fun `a group kind this jar predates becomes UNKNOWN rather than throwing`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addGroups(
                GroupState.newBuilder().setName("later").setKindValue(999),
            ).build(),
        )

        assertEquals(Group.Kind.UNKNOWN, mirror.groups().single().kind())
    }

    @Test
    fun `a player on no backend carries an empty Optional`() {
        val mirror = NetworkMirror()
        mirror.apply(state(players = listOf(someUuid to "alice"), playerServer = ""))

        assertTrue(mirror.players().single().server().isEmpty)
    }

    @Test
    fun `a player entry with an unparseable uuid is dropped rather than failing the apply`() {
        // The operator relays what a proxy reported; ProxyRole makes the same trade.
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

    @Test
    fun `a server's number, players and slots each land in their own place`() {
        // Distinct values, so a positional swap in the ServerInfo constructor fails.
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addServers(
                ServerState.newBuilder().setName("hub-a").setGroup("hub").setPhase("Ready")
                    .setNumber(3).setPlayers(7).setSlots(20),
            ).build(),
        )

        val server = mirror.servers().single()
        assertEquals(3, server.number())
        assertEquals(7, server.players())
        assertEquals(20, server.slots())
    }

    @Test
    fun `a server's playable figure reaches ServerInfo`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder().addServers(
                ServerState.newBuilder().setName("duels-a").setGroup("duels").setPhase("Ready")
                    .setPlayers(14).setSlots(100).setPlayableSlots(12),
            ).build(),
        )
        assertEquals(12, mirror.servers().single().playableSlots())
    }

    @Test
    fun `a server with its door closed is in closedDoors`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addServers(ServerState.newBuilder().setName("lobby-a").setGroup("lobby").setPhase("Ready").setJoinsClosed(true))
                .addServers(ServerState.newBuilder().setName("lobby-b").setGroup("lobby").setPhase("Ready"))
                .build(),
        )
        assertEquals(setOf("lobby-a"), mirror.closedDoors())
    }

    @Test
    fun `closed doors follow each NetworkState rather than accumulating`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addServers(ServerState.newBuilder().setName("lobby-a").setGroup("lobby").setPhase("Ready").setJoinsClosed(true))
                .build(),
        )
        mirror.apply(
            NetworkState.newBuilder()
                .addServers(ServerState.newBuilder().setName("lobby-a").setGroup("lobby").setPhase("Ready"))
                .build(),
        )
        assertEquals(emptySet(), mirror.closedDoors())
    }

    @Test
    fun `the proxies that accept transfers are named, and follow each NetworkState`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addProxies(ProxyState.newBuilder().setName("edge-1").setGroup("edge").setAcceptsTransfers(true))
                .addProxies(ProxyState.newBuilder().setName("edge-2").setGroup("edge"))
                .build(),
        )
        assertEquals(setOf("edge-1"), mirror.acceptingTransfers())

        mirror.apply(
            NetworkState.newBuilder()
                .addProxies(ProxyState.newBuilder().setName("edge-1").setGroup("edge"))
                .build(),
        )
        assertEquals(emptySet(), mirror.acceptingTransfers())
    }

    @Test
    fun `a pinned group says to what and until when`() {
        val mirror = NetworkMirror()
        mirror.apply(
            NetworkState.newBuilder()
                .addGroups(GroupState.newBuilder().setName("lobby").setKind(GroupState.Kind.EPHEMERAL)
                    .setPinned(true).setPinnedReplicas(0).setPinnedUntilUnix(1_800_000_000))
                .addGroups(GroupState.newBuilder().setName("arena").setKind(GroupState.Kind.EPHEMERAL))
                .build(),
        )
        val lobby = mirror.groups().single { it.name() == "lobby" }
        assertTrue(lobby.pinned())
        assertEquals(0, lobby.pinnedReplicas())
        assertEquals(java.time.Instant.ofEpochSecond(1_800_000_000), lobby.pinnedUntil())
        assertFalse(mirror.groups().single { it.name() == "arena" }.pinned())
    }
}
