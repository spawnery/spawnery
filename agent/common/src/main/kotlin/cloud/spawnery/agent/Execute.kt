package cloud.spawnery.agent

/** One server's answer to `/cloud execute`. */
data class ExecuteLine(val server: String, val ok: Boolean, val output: List<String>, val error: String)

/** Paper builds the tree without it, so neither `forcestop` nor `execute` exists there. */
class ProxyCommands<S>(
    val connector: CloudConnector,
    /** For the operator's record: a player's name, or "console". */
    val issuer: (S) -> String,
)

/** For one server its output; for a group a line per server and a total, no output. */
internal fun executeLines(target: String, outcomes: List<ExecuteLine>): List<String> {
    val single = outcomes.singleOrNull()
    if (single != null && single.server == target) {
        if (!single.ok) {
            return listOf(Layout.fail(Style.name(single.server) + Style.quiet(": ") + Style.bad(single.error)))
        }
        return listOf(Layout.ok(Style.name(single.server) + Style.good(" ran it"))) +
            single.output.map { Layout.entry(Style.quiet(it)) }
    }
    val ran = outcomes.count { it.ok }
    val lines = outcomes.sortedBy { it.server }.map {
        Layout.entry(Layout.joined(Style.name(it.server), if (it.ok) Style.good("ok") else Style.bad(it.error)))
    }
    val total = Style.number(ran) + Style.quiet(" of ") + Style.number(outcomes.size) + Style.quiet(" servers ran it")
    return lines + if (ran == outcomes.size) Layout.ok(total) else Layout.fail(total)
}
