package cloud.spawnery.agent.paper

import cloud.spawnery.agent.NetworkMirror
import com.destroystokyo.paper.event.player.PlayerConnectionCloseEvent
import org.bukkit.Bukkit
import org.bukkit.event.EventHandler
import org.bukkit.event.EventPriority
import org.bukkit.event.Listener
import org.bukkit.event.player.PlayerJoinEvent
import org.bukkit.event.player.PlayerLoginEvent
import org.bukkit.event.player.PlayerQuitEvent

/**
 * Registered only once the group enforces: a PlayerLoginEvent listener makes
 * Paper refuse its reconfiguration API server-wide. PlayerLoginEvent is
 * deprecated, but the only login event that has the player's permissions.
 */
@Suppress("DEPRECATION")
class LoginGateListener(
    private val group: String,
    private val mirror: NetworkMirror,
    private val state: ServerState,
    private val pending: PendingSeats = PendingSeats(System::currentTimeMillis),
) : Listener {
    @EventHandler(priority = EventPriority.HIGH)
    fun onLogin(event: PlayerLoginEvent) {
        if (event.result != PlayerLoginEvent.Result.ALLOWED) return
        val admission = mirror.admission(group) ?: return
        if (!admission.enforce) return
        val permission = LoginGate.permission(group)
        val bypass = event.player.hasPermission(permission)
        val playable = LoginGate.effectivePlayable(state.playable, admission.playableSlots, Bukkit.getMaxPlayers())
        val seated = Bukkit.getOnlinePlayers().count { !it.hasPermission(permission) }
        if (!LoginGate.admits(true, bypass, seated, pending.count(), playable)) {
            val name = mirror.groups().firstOrNull { it.name() == group }?.displayName()?.takeIf { it.isNotBlank() } ?: group
            event.disallow(PlayerLoginEvent.Result.KICK_FULL, LoginGate.refusal(name))
            return
        }
        if (!bypass) pending.admit(event.player.uniqueId)
    }

    @EventHandler(priority = EventPriority.MONITOR)
    fun onLoginSettled(event: PlayerLoginEvent) {
        if (event.result != PlayerLoginEvent.Result.ALLOWED) pending.release(event.player.uniqueId)
    }

    @EventHandler(priority = EventPriority.MONITOR)
    fun onJoin(event: PlayerJoinEvent) = pending.release(event.player.uniqueId)

    @EventHandler(priority = EventPriority.MONITOR)
    fun onQuit(event: PlayerQuitEvent) = pending.release(event.player.uniqueId)

    @EventHandler(priority = EventPriority.MONITOR)
    fun onClose(event: PlayerConnectionCloseEvent) = pending.release(event.playerUniqueId)
}
