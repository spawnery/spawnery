package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.Player
import com.velocitypowered.proxy.connection.client.ConnectedPlayer
import net.kyori.adventure.key.Key
import java.net.InetAddress
import java.net.InetSocketAddress
import java.util.UUID
import java.util.concurrent.ConcurrentHashMap

interface Traveller {
    val uuid: UUID
    val username: String
    val currentServer: String?
    val virtualHost: InetSocketAddress?
    val address: InetAddress?

    /** Stores [cookie] on the client and sends it to [to]; throws when the client cannot be transferred. */
    fun transfer(cookie: ByteArray, to: InetSocketAddress)
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
    private val clock: () -> Long = System::currentTimeMillis,
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
        if (cooling(player.address, clock())) return false
        if (!policy.onSwitch(picture(), player.uuid)) return false
        return send(player, host, target, "switch")
    }

    fun pass(players: List<Traveller>) {
        val now = clock()
        lastSentFrom.values.removeIf { now - it >= spacingMillis }
        val chosen = mutableSetOf<InetAddress>()
        val movable = players.filter { it.virtualHost != null }.associateBy { it.uuid }
        val occupants = movable.values.map { TransferPolicy.Occupant(it.uuid, it.currentServer) }
        val admit = { id: UUID ->
            val address = movable.getValue(id).address
            address == null || (!cooling(address, now) && chosen.add(address))
        }
        for ((id, target) in policy.forced(picture(), occupants, admit)) {
            val player = movable[id] ?: continue
            send(player, player.virtualHost ?: continue, target, "forced")
        }
    }

    private fun send(player: Traveller, host: InetSocketAddress, target: String, reason: String): Boolean {
        try {
            player.transfer(cookie.write(player.uuid, target), host)
        } catch (e: Exception) {
            warn("spawnery: could not transfer '${player.username}' ($reason) toward '$target'; leaving them here", e)
            return false
        }
        leaving += player.uuid
        player.address?.let { lastSentFrom[it] = clock() }
        info("spawnery: transferred '${player.username}' ($reason) toward '$target'")
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

    private fun cooling(address: InetAddress?, now: Long): Boolean {
        val last = lastSentFrom[address ?: return false] ?: return false
        return now - last < spacingMillis
    }

    private fun refused(username: String, reason: String) {
        warn("spawnery: transfer cookie from '$username' refused: $reason", null)
    }
}

internal val TRANSFER_COOKIE_KEY: Key = Key.key(TransferCookie.KEY)

internal class VelocityTraveller(
    private val player: Player,
    private val warn: (String, Throwable?) -> Unit,
) : Traveller {
    override val uuid: UUID get() = player.uniqueId
    override val username: String get() = player.username
    override val currentServer: String? get() = player.currentServer.map { it.serverInfo.name }.orElse(null)
    override val virtualHost: InetSocketAddress? get() = player.virtualHost.orElse(null)
    override val address: InetAddress? get() = player.remoteAddress?.address

    override fun transfer(cookie: ByteArray, to: InetSocketAddress) {
        player.storeCookie(TRANSFER_COOKIE_KEY, cookie)
        // storeCookie writes its packet from a task on the connection's event
        // loop; queued behind it, the transfer cannot overtake the cookie.
        val loop = (player as? ConnectedPlayer)?.connection?.eventLoop() ?: return player.transferToHost(to)
        loop.execute {
            try {
                player.transferToHost(to)
            } catch (e: Exception) {
                warn("spawnery: could not transfer '${player.username}' toward $to", e)
            }
        }
    }
}
