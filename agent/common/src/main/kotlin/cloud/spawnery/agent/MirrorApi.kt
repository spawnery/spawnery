package cloud.spawnery.agent

import cloud.spawnery.agent.api.CloudPlayer
import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.api.BoostResult
import cloud.spawnery.agent.api.ConnectResult
import cloud.spawnery.agent.api.ReadinessHold
import cloud.spawnery.agent.api.EventBus
import cloud.spawnery.agent.api.Self
import cloud.spawnery.agent.api.Target
import cloud.spawnery.agent.api.NetworkStatus
import cloud.spawnery.agent.api.ProxyInfo
import cloud.spawnery.agent.api.ServerInfo
import cloud.spawnery.agent.api.StartedServer
import cloud.spawnery.agent.api.SpawneryApi
import java.util.Optional
import java.time.Duration
import java.util.UUID
import java.util.concurrent.CompletionStage

/** Shared by both platforms: nothing here may ask which side it is on. */
class MirrorApi(
    private val mirror: NetworkMirror,
    private val self: Self,
    private val connector: CloudConnector,
    private val events: CloudEvents,
    /** Null on a proxy. */
    private val readiness: ReadinessGate? = null,
    /** Null on a proxy. */
    private val playable: ((Int) -> Unit)? = null,
    /** Null on a server. */
    private val transfers: TransferView? = null,
) : SpawneryApi {
    override fun self(): Self = self

    override fun groups(): List<Group> = mirror.groups()

    override fun group(name: String): Optional<Group> =
        Optional.ofNullable(mirror.groups().firstOrNull { it.name() == name })

    override fun servers(): List<ServerInfo> = mirror.servers()

    override fun server(name: String): Optional<ServerInfo> =
        Optional.ofNullable(mirror.servers().firstOrNull { it.name() == name })

    override fun proxies(): List<ProxyInfo> = mirror.proxies()

    override fun proxy(name: String): Optional<ProxyInfo> =
        Optional.ofNullable(mirror.proxies().firstOrNull { it.name() == name })

    override fun players(): List<CloudPlayer> = mirror.players()

    override fun player(id: UUID): Optional<CloudPlayer> =
        Optional.ofNullable(mirror.players().firstOrNull { it.id() == id })

    override fun arrivedByTransfer(player: UUID): Boolean = transfers?.arrived(player) ?: false

    override fun leavingByTransfer(player: UUID): Boolean = transfers?.leaving(player) ?: false

    override fun connect(player: UUID, to: Target): CompletionStage<ConnectResult> =
        connector.connect(player, to)

    override fun retire(server: String): CompletionStage<Void> =
        connector.retire(server)

    override fun unretire(server: String): CompletionStage<Void> =
        connector.unretire(server)

    override fun status(): CompletionStage<NetworkStatus> = connector.status("")

    override fun status(target: String): CompletionStage<NetworkStatus> = connector.status(target)

    override fun boost(group: String, replicas: Int, forHowLong: Duration?): CompletionStage<BoostResult> =
        connector.boost(group, replicas, forHowLong)

    override fun startServer(group: String, key: String): CompletionStage<StartedServer> =
        connector.startServer(group, key)

    override fun stopServer(server: String): CompletionStage<Void> =
        connector.stopServer(server)

    override fun deleteServer(group: String, key: String): CompletionStage<Void> =
        connector.deleteServer(group, key)

    override fun announce(state: String, attributes: Map<String, String>): CompletionStage<Void> =
        connector.announce(state, attributes)

    override fun acceptJoins(accept: Boolean): CompletionStage<Void> =
        connector.acceptJoins(accept)

    override fun endRound(): CompletionStage<Void> =
        connector.endRound()

    override fun holdReadiness(reason: String): ReadinessHold {
        val gate = readiness ?: throw UnsupportedOperationException(
            "this is a proxy; a proxy has no readiness to hold",
        )
        return gate.hold(reason)
    }

    override fun playableSlots(slots: Int) {
        val sink = playable ?: throw UnsupportedOperationException(
            "this is a proxy; a proxy has no seats a group is sized by",
        )
        require(slots >= 0) { "playable slots must not be negative, got $slots" }
        sink(slots)
    }

    override fun stopBoosts(group: String): CompletionStage<Int> =
        connector.stopBoosts(group)

    override fun events(): EventBus = events
}

interface TransferView {
    fun arrived(player: UUID): Boolean

    fun leaving(player: UUID): Boolean
}
