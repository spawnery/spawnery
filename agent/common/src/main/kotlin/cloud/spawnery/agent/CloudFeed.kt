package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/** Six names fit a chat line; a forty-server scale-up printed in full is a wall. */
private const val NAMES_SHOWN = 6

/**
 * Collapsed here and not on the operator, so the wire stays one event per
 * transition, the same facts as `kubectl get events`.
 *
 * Warnings are never collapsed: two failures rarely fail for the same reason.
 */
fun coalesce(events: List<CloudEvent>): List<String> {
    val lines = mutableListOf<String>()
    val (warnings, ordinary) = events.partition { it.warning }

    for (w in warnings) {
        lines += "$WARNING ${Style.name(w.subject)}${Style.quiet(": ")}${Style.bad(w.message)}"
    }

    val byKindAndGroup = LinkedHashMap<Pair<String, String>, MutableList<CloudEvent>>()
    for (e in ordinary) {
        byKindAndGroup.getOrPut(e.kind to e.group) { mutableListOf() } += e
    }

    for ((key, collapsed) in byKindAndGroup) {
        val (kind, groupName) = key
        val only = collapsed.singleOrNull()
        if (only != null) {
            lines += "${sign(kind)} ${Style.name(only.subject)}${Style.quiet(": ")}" +
                Style.quiet(only.message)
            continue
        }
        val shown = collapsed.take(NAMES_SHOWN).joinToString(Style.quiet(", ")) { Style.name(it.subject) }
        val rest = collapsed.size - minOf(collapsed.size, NAMES_SHOWN)
        val names = if (rest > 0) shown + Style.quiet(" and $rest more") else shown
        lines += "${sign(kind)} ${Style.number(collapsed.size)} ${Style.number(kind)}" +
            "${Style.quiet(" in ")}${Style.name(groupName)}${Style.quiet(" (")}$names${Style.quiet(")")}"
    }
    return lines
}

/** A neutral event gets a dim sign too, so the signs line up. */
private fun sign(kind: String): String = when (row(kind)) {
    Row.CREATED, Row.ARRIVED, Row.READY -> Style.marker("+", "green")
    Row.LEAVING, Row.GONE -> Style.marker("-", "gold")
    Row.OTHER -> Style.marker("\u00b7", "dark_gray")
}

private val WARNING = Style.marker("!", "red")
