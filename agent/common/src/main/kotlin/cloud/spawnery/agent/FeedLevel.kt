package cloud.spawnery.agent

enum class FeedLevel {
    OFF,
    MINIMAL,
    NORMAL,
    VERBOSE,
    ;

    val word: String get() = name.lowercase()

    companion object {
        /** `on` is what the command took before there were levels. */
        fun parse(value: String?): FeedLevel? = when (value?.trim()?.lowercase()) {
            "off" -> OFF
            "minimal", "on" -> MINIMAL
            "normal" -> NORMAL
            "verbose" -> VERBOSE
            else -> null
        }

        fun of(value: String?): FeedLevel = parse(value) ?: MINIMAL
    }
}
