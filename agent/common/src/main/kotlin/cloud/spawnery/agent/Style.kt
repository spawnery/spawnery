package cloud.spawnery.agent

/**
 * MiniMessage tags in plain Strings: `:common` has no Adventure on its compile
 * classpath. Not legacy `§` codes, which cannot be escaped.
 */
internal object Style {
    fun name(value: String): String = "<aqua>${escape(value)}</aqua>"

    fun good(value: String): String = "<green>${escape(value)}</green>"

    fun bad(value: String): String = "<red>${escape(value)}</red>"

    fun title(value: String): String = "<white><bold>${escape(value)}</bold></white>"

    fun sectionTitle(value: String): String = "<gray><bold>${escape(value)}</bold></gray>"

    fun warn(value: String): String = "<yellow>${escape(value)}</yellow>"

    fun quiet(value: String): String = "<gray>${escape(value)}</gray>"

    fun number(value: Any): String = "<white>${escape(value.toString())}</white>"

    /** Only the sign carries colour, so a warning's sentence stays the loudest thing on the line. */
    fun marker(sign: String, colour: String): String =
        "<dark_gray>[</dark_gray><$colour>${escape(sign)}</$colour><dark_gray>]</dark_gray>"

    /** The operator's refusal messages reach chat verbatim and may hold a `<`. */
    fun escape(value: String): String =
        value.replace("\\", "\\\\").replace("<", "\\<")
}
