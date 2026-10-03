package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.JoinRule
import cloud.spawnery.agent.JoinRules
import cloud.spawnery.agent.PermissionValue
import com.velocitypowered.api.permission.Tristate
import com.velocitypowered.api.proxy.ProxyServer
import java.util.UUID

fun interface PermissionLookup {
    fun value(player: UUID, node: String, contexts: Map<String, String>): PermissionValue?
}

fun interface JoinAccess {
    fun mayJoin(player: UUID, server: String, group: String): Boolean

    companion object {
        val OPEN = JoinAccess { _, _, _ -> true }
    }
}

class JoinPermissions(
    private val rules: (String) -> JoinRule?,
    private val network: () -> String,
    private val lookups: List<PermissionLookup>,
) : JoinAccess {
    override fun mayJoin(player: UUID, server: String, group: String): Boolean {
        val rule = rules(group) ?: return true
        val contexts = JoinRules.contexts(server, group, network())
        val value = lookups.firstNotNullOfOrNull { lookup ->
            runCatching { lookup.value(player, rule.node, contexts) }.getOrNull()
        } ?: PermissionValue.UNDEFINED
        return JoinRules.mayJoin(rule, value)
    }
}

/** Velocity's own answer knows no contexts; it decides only where LuckPerms cannot. */
class VelocityPermissionLookup(private val proxy: ProxyServer) : PermissionLookup {
    override fun value(player: UUID, node: String, contexts: Map<String, String>): PermissionValue? =
        proxy.getPlayer(player).map {
            when (it.getPermissionValue(node)) {
                Tristate.TRUE -> PermissionValue.TRUE
                Tristate.FALSE -> PermissionValue.FALSE
                else -> PermissionValue.UNDEFINED
            }
        }.orElse(null)
}
