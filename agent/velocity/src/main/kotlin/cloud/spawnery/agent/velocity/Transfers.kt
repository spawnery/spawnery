package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.monotonicMillis
import com.velocitypowered.api.proxy.Player
import com.velocitypowered.proxy.connection.client.ConnectedPlayer
import net.kyori.adventure.key.Key
import net.kyori.adventure.text.Component
import java.net.InetAddress
import java.net.InetSocketAddress
import java.util.UUID
import java.util.concurrent.CompletableFuture
import java.util.concurrent.ConcurrentHashMap

interface Traveller {
    val uuid: UUID
    val username: String
    val currentServer: String?
    val virtualHost: InetSocketAddress?
    val address: InetAddress?

    fun tell(message: Component)

    /**
     * Stores [cookie] on the client and sends it to [to]. Throws when the
     * transfer cannot be queued; the future fails when it was queued and then
     * could not be sent.
     */
    fun transfer(cookie: ByteArray, to: InetSocketAddress): CompletableFuture<Unit>
}

class Transfers(
    private val cookie: TransferCookie,
    private val policy: TransferPolicy,
    private val picture: () -> TransferPolicy.Picture,
    private val registered: (String) -> Boolean,
    private val info: (String) -> Unit,
    private val warn: (String, Throwable?) -> Unit,
    /** The receiving proxy refuses a second login from one address within its login rate limit. */
    private val spacingMillis: Long = 0,
    private val clock: () -> Long = ::monotonicMillis,
    /** The wall clock: it is the only one the proxies of a group share, and [myTurn]'s windows must line up across them. */
    private val turnClock: () -> Long = System::currentTimeMillis,
) {
    private val waiting = ConcurrentHashMap<UUID, () -> Unit>()
    private val asked = ConcurrentHashMap.newKeySet<UUID>()
    private val arrivals = ConcurrentHashMap<UUID, String>()
    private val arrived = ConcurrentHashMap.newKeySet<UUID>()
    private val leaving = ConcurrentHashMap.newKeySet<UUID>()
    private val lastSentFrom = ConcurrentHashMap<InetAddress, Long>()

    fun arrivedByTransfer(player: UUID): Boolean = player in arrived

    fun leavingByTransfer(player: UUID): Boolean = player in leaving

    /** True when the switch was replaced by a transfer and must be denied. */
    fun onSwitch(player: Traveller, target: String): Boolean {
        val host = player.virtualHost ?: return false
        val now = clock()
        val picture = picture()
        if (cooling(player.address, now) || !myTurn(picture)) return false
        if (!policy.onSwitch(picture, player.uuid)) return false
        return send(player, host, target, "switch")
    }

    fun pass(players: List<Traveller>) {
        val now = clock()
        lastSentFrom.values.removeIf { now - it >= spacingMillis }
        val picture = picture()
        val movable = players.filter { it.virtualHost != null }.associateBy { it.uuid }
        val occupants = movable.values.map { TransferPolicy.Occupant(it.uuid, it.currentServer) }
        for (warning in policy.warnings(picture, occupants, TransferWarning.LEAD_MILLIS)) {
            val player = movable[warning.id] ?: continue
            try {
                player.tell(TransferWarning.message(warning.seconds))
            } catch (e: Exception) {
                warn("spawnery: could not warn '${player.username}' of the coming transfer", e)
            }
        }
        if (!myTurn(picture)) return
        val chosen = mutableSetOf<InetAddress>()
        val admit = { id: UUID ->
            val address = movable.getValue(id).address
            address == null || (!cooling(address, now) && chosen.add(address))
        }
        for ((id, target) in policy.forced(picture, occupants, admit)) {
            val player = movable[id] ?: continue
            send(player, player.virtualHost ?: continue, target, "forced")
        }
    }

    private fun send(player: Traveller, host: InetSocketAddress, target: String, reason: String): Boolean {
        val what = "'${player.username}' ($reason) toward '$target'"
        val sent = try {
            player.transfer(cookie.write(player.uuid, target), host)
        } catch (e: Exception) {
            warn("spawnery: could not transfer $what; leaving them here", e)
            return false
        }
        leaving += player.uuid
        player.address?.let { lastSentFrom[it] = clock() }
        if (!sent.isDone) info("spawnery: queued the transfer of $what")
        sent.whenComplete { _, failure ->
            if (failure == null) {
                info("spawnery: transferred $what")
            } else {
                leaving -= player.uuid
                warn("spawnery: could not transfer $what after queueing it; leaving them here", failure)
            }
        }
        return true
    }

    fun expecting(player: UUID, resume: () -> Unit) {
        asked += player
        waiting[player] = resume
    }

    /** True when this agent asked for the cookie, so nobody else should see the answer. */
    fun received(player: UUID, username: String, payload: ByteArray?): Boolean {
        if (!asked.remove(player)) return false
        val resume = waiting.remove(player) ?: return true
        when (val result = if (payload == null) TransferCookie.Result.Refused("no cookie") else cookie.read(player, payload)) {
            is TransferCookie.Result.Valid -> {
                arrivals[player] = result.target
                arrived += player
            }
            is TransferCookie.Result.Refused -> refused(username, result.reason)
        }
        resume()
        return true
    }

    fun gaveUp(player: UUID, username: String, reason: String) {
        val resume = waiting.remove(player) ?: return
        refused(username, reason)
        resume()
    }

    fun landing(player: UUID, username: String): String? {
        val target = arrivals.remove(player) ?: return null
        if (!registered(target)) {
            refused(username, "'$target' is not a registered server")
            return null
        }
        return target
    }

    fun forget(player: UUID) {
        waiting.remove(player)?.invoke()
        asked.remove(player)
        arrivals.remove(player)
        arrived.remove(player)
        leaving.remove(player)
        policy.forget(picture(), player)
    }

    /**
     * Proxies of one group that leave together share the receiving proxies'
     * rate limit, and none of them knows the others' addresses. Each sends
     * only in its own window, with an empty window between any two.
     */
    private fun myTurn(picture: TransferPolicy.Picture): Boolean {
        if (spacingMillis <= 0) return true
        val leaving = picture.proxies.filter { it.group() == picture.group && it.draining() }.map { it.name() }.sorted()
        val index = leaving.indexOf(picture.self)
        if (leaving.size <= 1 || index < 0) return true
        return (turnClock() / spacingMillis) % (2L * leaving.size) == 2L * index
    }

    private fun cooling(address: InetAddress?, now: Long): Boolean {
        val last = lastSentFrom[address ?: return false] ?: return false
        return now - last < spacingMillis
    }

    private fun refused(username: String, reason: String) {
        warn("spawnery: transfer cookie from '$username' refused: $reason", null)
    }
}

internal val TRANSFER_COOKIE_KEY: Key = Key.key(TransferCookie.KEY)

internal class VelocityTraveller(private val player: Player) : Traveller {
    override val uuid: UUID get() = player.uniqueId
    override val username: String get() = player.username
    override val currentServer: String? get() = player.currentServer.map { it.serverInfo.name }.orElse(null)
    override val virtualHost: InetSocketAddress? get() = player.virtualHost.orElse(null)
    override val address: InetAddress? get() = player.remoteAddress?.address

    override fun tell(message: Component) = player.sendMessage(message)

    override fun transfer(cookie: ByteArray, to: InetSocketAddress): CompletableFuture<Unit> {
        player.storeCookie(TRANSFER_COOKIE_KEY, cookie)
        // storeCookie writes its packet from a task on the connection's event
        // loop; queued behind it, the transfer cannot overtake the cookie.
        val loop = (player as? ConnectedPlayer)?.connection?.eventLoop()
            ?: return CompletableFuture.completedFuture(player.transferToHost(to))
        val sent = CompletableFuture<Unit>()
        loop.execute {
            try {
                player.transferToHost(to)
                sent.complete(Unit)
            } catch (e: Exception) {
                sent.completeExceptionally(e)
            }
        }
        return sent
    }
}
