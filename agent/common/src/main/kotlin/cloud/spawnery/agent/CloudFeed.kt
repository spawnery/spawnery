package cloud.spawnery.agent

import cloud.spawnery.agent.pb.CloudEvent

/**
 * How many names a collapsed line prints before it starts counting.
 *
 * Six, which is a chat line and not a wall. The alternative -- printing all of
 * them -- turns a forty-server scale-up into a message that pushes everything
 * else off the screen, which is a feed people turn off for the same reason ten
 * separate lines is.
 */
private const val NAMES_SHOWN = 6

/**
 * Collapses one window of events into what happened, without deciding how the
 * line looks.
 *
 * The surrounding format is [Feed]'s and comes from the Network's own spec, so
 * an installation can re-word its prefix without this file knowing. What is
 * here is the part that has to be true.
 *
 * **Pure, and that is the whole design.** It takes a list and returns strings:
 * no clock, no platform, no I/O. The window is [CloudFeedBuffer]'s problem,
 * which is what makes every rule here assertable as one table of inputs and
 * outputs.
 *
 * **It runs on the agent and not the operator.** The wire carries one event per
 * transition so that the feed and `kubectl get events` stay the same list of
 * facts; collapsing them into a sentence is presentation, and a plugin
 * subscribing through the API gets the facts rather than somebody else's
 * summary.
 *
 * Grouped by kind and group, never across either: "4 things happened in lobby"
 * is the shape of a summary that says nothing.
 *
 * **Warnings are never collapsed.** A failure hidden inside "3 servers ready"
 * is the one event in this feed somebody actually needs to see, and each keeps
 * the operator's own sentence -- a count and a name is the right shape for ten
 * identical successes and the wrong shape for one failure, because two
 * failures rarely fail for the same reason.
 */
fun coalesce(events: List<CloudEvent>): List<String> {
    val lines = mutableListOf<String>()
    val (warnings, ordinary) = events.partition { it.warning }

    for (w in warnings) {
        // A warning is the one line in this feed somebody has to see, so it
        // is the one that is red rather than merely prefixed. The sign is
        // added to that and does not replace it.
        lines += "$WARNING ${Style.name(w.subject)}${Style.quiet(": ")}${Style.bad(w.message)}"
    }

    // LinkedHashMap, so the order events arrived is the order they are read.
    // A map iteration order that varied would make two identical windows
    // produce two different feeds, which nothing downstream would ever catch.
    val byKindAndGroup = LinkedHashMap<Pair<String, String>, MutableList<CloudEvent>>()
    for (e in ordinary) {
        byKindAndGroup.getOrPut(e.kind to e.group) { mutableListOf() } += e
    }

    for ((key, collapsed) in byKindAndGroup) {
        val (kind, groupName) = key
        val only = collapsed.singleOrNull()
        if (only != null) {
            // One event keeps its sentence. A count of one is not a summary,
            // and "1 ReadyGatePassed in lobby (lobby-a)" says less than the
            // operator already said.
            lines += "${sign(kind)} ${Style.name(only.subject)}${Style.quiet(": ")}" +
                Style.quiet(only.message)
            continue
        }
        val shown = collapsed.take(NAMES_SHOWN).joinToString(Style.quiet(", ")) { Style.name(it.subject) }
        val rest = collapsed.size - minOf(collapsed.size, NAMES_SHOWN)
        val names = if (rest > 0) shown + Style.quiet(" and $rest more") else shown
        // The kind is no longer green. It was green for every kind, which
        // said "something happened" and read as good news above a line about
        // three servers terminating -- and next to the sign it would now
        // contradict it outright.
        lines += "${sign(kind)} ${Style.number(collapsed.size)} ${Style.number(kind)}" +
            "${Style.quiet(" in ")}${Style.name(groupName)}${Style.quiet(" (")}$names${Style.quiet(")")}"
    }
    return lines
}

/**
 * The sign a line opens with, by what the kind did to the network.
 *
 * A neutral event gets one too, dim: without it the column jumps and the signs
 * stop lining up, which is most of what makes them readable at a glance.
 */
private fun sign(kind: String): String = when (direction(kind)) {
    Direction.ADDED -> Style.marker("+", "green")
    Direction.REMOVED -> Style.marker("-", "gold")
    Direction.NEUTRAL -> Style.marker("\u00b7", "dark_gray")
}

private val WARNING = Style.marker("!", "red")
