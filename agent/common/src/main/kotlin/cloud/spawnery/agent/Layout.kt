package cloud.spawnery.agent

/** Chat is not monospaced, so nothing here pads to line up. */
object Layout {
    const val SEGMENTS = 20

    fun heading(title: String, summary: String = ""): String =
        Style.title(title) + if (summary.isEmpty()) "" else "  $summary"

    fun section(title: String): String = " " + Style.sectionTitle(title)

    fun entry(text: String): String = "   $text"

    fun member(text: String): String = "     $text"

    fun field(label: String, value: String): String = " " + Style.quiet(label) + "  " + value

    fun joined(vararg parts: String): String =
        parts.filter { it.isNotEmpty() }.joinToString(Style.quiet(" · "))

    fun bar(fraction: Double, colour: String): String {
        val filled = if (fraction.isNaN()) 0 else (fraction.coerceIn(0.0, 1.0) * SEGMENTS).toInt()
        return "<$colour>" + "|".repeat(filled) + "</$colour>" +
            "<dark_gray>" + "|".repeat(SEGMENTS - filled) + "</dark_gray>"
    }

    fun fillColour(fraction: Double): String = when {
        fraction < 0.70 -> "green"
        fraction < 0.90 -> "yellow"
        else -> "red"
    }

    fun tpsColour(tps: Double): String = when {
        tps >= 19 -> "green"
        tps >= 15 -> "yellow"
        else -> "red"
    }

    fun count(n: Int, one: String, many: String): String = "$n ${if (n == 1) one else many}"

    fun kindName(kind: cloud.spawnery.agent.api.Group.Kind): String = kind.name.lowercase().replace('_', '-')

    fun ok(text: String): String = "<green>✔</green> $text"

    fun fail(text: String): String = "<red>✘</red> $text"
}
