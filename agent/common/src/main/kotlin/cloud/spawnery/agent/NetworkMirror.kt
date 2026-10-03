package cloud.spawnery.agent

import cloud.spawnery.agent.api.CloudPlayer
import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.api.ProxyInfo
import cloud.spawnery.agent.api.ServerInfo
import cloud.spawnery.agent.api.ServerPhase
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.NetworkState
import java.util.Optional
import java.util.UUID

/** For the agent's login check; not published through the plugin API. */
data class GroupAdmission(val playableSlots: Int, val enforce: Boolean)

class NetworkMirror {
    private data class Snapshot(
        val groups: List<Group>,
        val servers: List<ServerInfo>,
        val proxies: List<ProxyInfo>,
        val players: List<CloudPlayer>,
        /** Blank before the first state and from an older operator; [Feed] then uses its default. */
        val feedFormat: String,
        val admissions: Map<String, GroupAdmission> = emptyMap(),
        val closedDoors: Set<String> = emptySet(),
        val acceptingTransfers: Set<String> = emptySet(),
        val joinRules: Map<String, JoinRule> = emptyMap(),
    )

    // Swapped whole and never locked, so a read never mixes two states and never blocks.
    @Volatile
    private var snapshot = Snapshot(emptyList(), emptyList(), emptyList(), emptyList(), "")

    fun apply(state: NetworkState) {
        snapshot = Snapshot(
            feedFormat = state.feedFormat,
            admissions = state.groupsList.associate { it.name to GroupAdmission(it.playableSlots, it.enforcePlayableSlots) },
            closedDoors = state.serversList.filter { it.joinsClosed }.mapTo(mutableSetOf()) { it.name },
            acceptingTransfers = state.proxiesList.filter { it.acceptsTransfers }.mapTo(mutableSetOf()) { it.name },
            joinRules = state.groupsList
                .filter { it.joinPermission.isNotEmpty() }
                .associate { it.name to JoinRule(it.joinPermission, it.joinPermissionDenyOnly) },
            groups = state.groupsList.map {
                Group(
                    it.name,
                    kindOf(it.kind),
                    it.replicas,
                    it.readyReplicas,
                    it.onlinePlayers,
                    it.freeSlots,
                    it.attributesMap,
                    it.displayName,
                )
            },
            servers = state.serversList.map {
                ServerInfo(
                    it.name,
                    it.group,
                    // Not valueOf: an older agent must read a new phase as unknown, not throw.
                    ServerPhase.fromWire(it.phase),
                    it.players,
                    it.slots,
                    it.registered,
                    it.state,
                    // Copied by ServerInfo.
                    it.attributesMap,
                    it.incarnation,
                    it.number,
                    it.held,
                    it.node,
                    it.playableSlots,
                )
            },
            proxies = state.proxiesList.map {
                ProxyInfo(it.name, it.group, it.ready, it.draining, it.players, it.node)
            },
            players = state.playersList.mapNotNull { entry ->
                val id = runCatching { UUID.fromString(entry.uuid) }.getOrNull() ?: return@mapNotNull null
                CloudPlayer(
                    id,
                    entry.name,
                    if (entry.server.isEmpty()) Optional.empty() else Optional.of(entry.server),
                )
            },
        )
    }

    fun groups(): List<Group> = snapshot.groups

    fun servers(): List<ServerInfo> = snapshot.servers
    fun proxies(): List<ProxyInfo> = snapshot.proxies

    fun players(): List<CloudPlayer> = snapshot.players

    fun feedFormat(): String = snapshot.feedFormat

    fun admission(group: String): GroupAdmission? = snapshot.admissions[group]

    fun closedDoors(): Set<String> = snapshot.closedDoors

    fun acceptingTransfers(): Set<String> = snapshot.acceptingTransfers

    fun joinRule(group: String): JoinRule? = snapshot.joinRules[group]
}

internal fun kindOf(kind: GroupState.Kind): Group.Kind =
    when (kind) {
        GroupState.Kind.EPHEMERAL -> Group.Kind.EPHEMERAL
        GroupState.Kind.PERSISTENT -> Group.Kind.PERSISTENT
        GroupState.Kind.PROXY -> Group.Kind.PROXY
        GroupState.Kind.ON_DEMAND -> Group.Kind.ON_DEMAND
        // Both the operator's explicit "I do not know" and what proto3
        // hands an agent older than a value it was sent.
        else -> Group.Kind.UNKNOWN
    }
