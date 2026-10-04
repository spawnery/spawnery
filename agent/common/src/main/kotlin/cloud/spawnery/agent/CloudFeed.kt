package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.pb.CloudEvent

/** Six names fit a chat line; a forty-server scale-up printed in full is a wall. */
private const val NAMES_SHOWN = 6

/**
 * Collapsed here and not on the operator, so the wire stays one event per
 * transition, the same facts as `kubectl get events`.
 *
 * Warnings are never collapsed: two failures rarely fail for the same reason.
 */
internal fun coalesce(
    events: List<CloudEvent>,
    level: FeedLevel,
    groupKind: (String) -> Group.Kind = { Group.Kind.UNKNOWN },
): List<String> {
    if (level == FeedLevel.OFF) return emptyList()
    val name = { e: CloudEvent -> Style.subject(e.subject, shortName(e.subject, e.group, groupKind(e.group))) }
    val lines = mutableListOf<String>()
    val (warnings, ordinary) = events.partition(::isWarning)

    for (w in warnings) {
        lines += if (level == FeedLevel.VERBOSE) {
            "$WARNING ${name(w)}${Style.quiet(": ")}${Style.bad(w.message.ifBlank { shortReason(w.kind) })}"
        } else {
            "$WARNING ${name(w)} ${Style.bad(shortReason(w.kind))}"
        }
    }

    val verbose = level == FeedLevel.VERBOSE
    val byKey = LinkedHashMap<Pair<String, String>, MutableList<CloudEvent>>()
    for (e in ordinary) {
        if (word(row(e.kind), level) == null) continue
        val key = (if (verbose) e.kind else row(e.kind).name) to e.group
        byKey.getOrPut(key) { mutableListOf() } += e
    }

    for ((key, events) in byKey) {
        val collapsed = events.distinctBy { it.subject }
        val group = key.second
        val r = row(collapsed.first().kind)
        val sign = sign(r, level)
        val only = collapsed.singleOrNull()
        lines += when {
            verbose && only != null ->
                "$sign ${name(only)}${Style.quiet(": ")}${Style.quiet(only.message.ifBlank { words(only.kind) })}"
            verbose -> {
                val shown = collapsed.take(NAMES_SHOWN).joinToString(Style.quiet(", ")) { name(it) }
                val rest = collapsed.size - minOf(collapsed.size, NAMES_SHOWN)
                val names = if (rest > 0) shown + Style.quiet(" and $rest more") else shown
                "$sign ${Style.number(collapsed.size)} ${Style.number(key.first)}${Style.quiet(" in ")}" +
                    "${Style.subject(group)}${Style.quiet(" (")}$names${Style.quiet(")")}"
            }
            only != null -> "$sign ${name(only)}" + suffix(word(r, level))
            else -> "$sign ${Style.number(collapsed.size)} ${Style.subject(group)}" + suffix(word(r, level))
        }
    }
    return lines
}

/** Null hides the row at this level; empty shows the name alone. */
private fun word(row: Row, level: FeedLevel): String? = when (level) {
    FeedLevel.OFF -> null
    FeedLevel.MINIMAL -> when (row) {
        Row.CREATED, Row.ARRIVED, Row.GONE -> ""
        else -> null
    }
    FeedLevel.NORMAL -> when (row) {
        Row.CREATED -> "starting"
        Row.ARRIVED, Row.READY -> "ready"
        Row.LEAVING -> "leaving"
        Row.GONE -> "stopped"
        Row.OTHER -> null
    }
    FeedLevel.VERBOSE -> ""
}

private fun suffix(word: String?): String = if (word.isNullOrEmpty()) "" else " " + Style.quiet(word)

/** At minimal a proxy's ready gate is its arrival, so it takes the arrival's sign. */
private fun sign(row: Row, level: FeedLevel): String = when (row) {
    Row.CREATED -> Style.marker("+", "green")
    Row.ARRIVED -> if (level == FeedLevel.MINIMAL) Style.marker("+", "green") else Style.marker("✓", "green")
    Row.READY -> Style.marker("✓", "green")
    Row.LEAVING, Row.GONE -> Style.marker("-", "gold")
    Row.OTHER -> Style.marker("·", "dark_gray")
}

private val WARNING = Style.marker("!", "red")
