package cloud.spawnery.agent.paper

import cloud.spawnery.agent.JoinRules
import cloud.spawnery.agent.PermissionValue
import net.kyori.adventure.text.Component

object LoginGate {
    const val KEY = "spawnery.join.full"
    private const val FALLBACK = "This round is full."

    fun permission(group: String): String = "$KEY.$group"

    /**
     * [seated] counts online players without the bypass permission, [pending]
     * those admitted and not yet joined; a bypass player takes no seat.
     */
    fun admits(enforce: Boolean, bypass: Boolean, seated: Int, pending: Int, playable: Int): Boolean =
        !enforce || bypass || seated + pending < playable

    /** The operator's order: the plugin's figure, the group's, the server's. */
    fun effectivePlayable(pluginValue: Int, groupValue: Int, maxPlayers: Int): Int {
        val chosen = when {
            pluginValue > 0 -> pluginValue
            groupValue > 0 -> groupValue
            else -> maxPlayers
        }
        return chosen.coerceIn(1, maxOf(1, maxPlayers))
    }

    /** Translatable, so a network with its own translations can render it. */
    fun refusal(groupDisplayName: String): Component =
        Component.translatable()
            .key(KEY)
            .fallback(FALLBACK)
            .arguments(Component.text(groupDisplayName))
            .build()

    fun permissionValue(has: Boolean, isSet: Boolean): PermissionValue = when {
        has -> PermissionValue.TRUE
        isSet -> PermissionValue.FALSE
        else -> PermissionValue.UNDEFINED
    }

    fun denied(groupDisplayName: String): Component =
        Component.translatable()
            .key(JoinRules.DENIED_KEY)
            .fallback(JoinRules.DENIED_FALLBACK)
            .arguments(Component.text(groupDisplayName))
            .build()

    fun displayName(group: String, displayName: String?): String =
        displayName?.takeIf { it.isNotBlank() } ?: group
}
