package cloud.spawnery.agent

import cloud.spawnery.agent.api.CloudPlayer
import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.api.ServerInfo
import cloud.spawnery.agent.api.ServerPhase
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.NetworkState
import java.util.Optional
import java.util.UUID

/**
 * The last picture of the network the operator sent, in the plugin API's own
 * types.
 *
 * **One volatile reference to an immutable snapshot, swapped whole.** Not three
 * fields and not a lock, and both alternatives are worse for the same reason:
 * a reader that took the groups from one state and the players from the next
 * would see a player on a server the same read says does not exist. Swapping
 * one reference makes every read internally consistent without anybody
 * agreeing to hold anything.
 *
 * The threads, the way [cloud.spawnery.agent.AgentRole] documents its own:
 *
 *  - [apply] is called from `SessionLoop`'s gRPC callback thread. Not *one*
 *    such thread either -- a make-before-break renewal keeps two streams alive,
 *    so two callbacks can arrive at once, and the last writer wins, which is
 *    correct: both carry a whole state and the newer one is the answer.
 *  - the readers are called from Bukkit's main thread, from Velocity's event
 *    thread, and from whatever thread a plugin chooses. None of them may block,
 *    which is why there is no lock here at all.
 *
 * Conversion happens on [apply] rather than on read: a plugin that calls
 * `servers()` in a loop should pay for a volatile read, not for rebuilding
 * every record each time.
 */
class NetworkMirror {
    private data class Snapshot(
        val groups: List<Group>,
        val servers: List<ServerInfo>,
        val players: List<CloudPlayer>,
        /**
         * The chat feed's shape, from the Network's own spec. Blank until the
         * first NetworkState arrives, and blank from an operator older than
         * the field -- [Feed] reads both as "use my own default".
         */
        val feedFormat: String,
    )

    @Volatile
    private var snapshot = Snapshot(emptyList(), emptyList(), emptyList(), "")

    /** Replaces everything this mirror holds. */
    fun apply(state: NetworkState) {
        snapshot = Snapshot(
            feedFormat = state.feedFormat,
            groups = state.groupsList.map {
                Group(it.name, kindOf(it.kind), it.replicas, it.readyReplicas, it.onlinePlayers, it.freeSlots)
            },
            servers = state.serversList.map {
                ServerInfo(
                    it.name,
                    it.group,
                    // fromWire and not valueOf: the operator's phase vocabulary
                    // gains values, and an agent older than one must read it as
                    // unknown rather than throw inside a gRPC callback.
                    ServerPhase.fromWire(it.phase),
                    it.players,
                    it.slots,
                    it.registered,
                    it.state,
                    // The proto's own map, copied by ServerInfo rather than
                    // here: a snapshot a plugin holds must not change under it
                    // when the next state arrives.
                    it.attributesMap,
                )
            },
            // An entry whose UUID will not parse is dropped and the rest of the
            // state applies. One malformed player must not cost this agent its
            // whole mirror -- the trade ProxyRole already makes for a FullSync
            // carrying one bad address.
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

    fun players(): List<CloudPlayer> = snapshot.players

    /**
     * The chat feed's shape, as the operator last stated it.
     *
     * Read per delivery rather than captured, so an edit to the Network takes
     * effect at the next resync instead of at the next pod -- see Feed's own
     * `format` parameter for why that matters.
     */
    fun feedFormat(): String = snapshot.feedFormat

    private fun kindOf(kind: GroupState.Kind): Group.Kind =
        when (kind) {
            GroupState.Kind.EPHEMERAL -> Group.Kind.EPHEMERAL
            GroupState.Kind.PERSISTENT -> Group.Kind.PERSISTENT
            GroupState.Kind.PROXY -> Group.Kind.PROXY
            // Both the operator's explicit "I do not know" and what proto3
            // hands an agent older than a value it was sent.
            else -> Group.Kind.UNKNOWN
        }
}
