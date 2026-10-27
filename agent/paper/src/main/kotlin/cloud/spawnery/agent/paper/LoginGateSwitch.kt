package cloud.spawnery.agent.paper

import org.bukkit.event.Listener

/**
 * Keeps the login check registered only while its group needs it. Paper turns
 * its reconfiguration API off for each login made while any PlayerLoginEvent
 * listener is registered (HandlerList read at login, Paper 26.3 build 174), so
 * unregistering gives it back to the logins that follow.
 *
 * Not thread-safe: called from the global region thread only.
 */
internal class LoginGateSwitch(
    private val register: () -> Listener,
    private val unregister: (Listener) -> Unit,
) {
    enum class Change { REGISTERED, UNREGISTERED, NONE }

    private var current: Listener? = null

    fun update(wanted: Boolean): Change {
        val registered = current
        return when {
            wanted && registered == null -> {
                current = register()
                Change.REGISTERED
            }
            !wanted && registered != null -> {
                unregister(registered)
                current = null
                Change.UNREGISTERED
            }
            else -> Change.NONE
        }
    }
}
