package cloud.spawnery.agent

enum class PermissionValue { TRUE, FALSE, UNDEFINED }

data class JoinRule(val node: String, val denyOnly: Boolean)

object JoinRules {
    const val DENIED_KEY = "spawnery.join.denied"
    const val DENIED_FALLBACK = "You may not join %s."

    fun mayJoin(rule: JoinRule?, value: PermissionValue): Boolean = when {
        rule == null -> true
        rule.denyOnly -> value != PermissionValue.FALSE
        else -> value == PermissionValue.TRUE
    }
}
