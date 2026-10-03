package cloud.spawnery.agent

/**
 * A styled line with the markup taken back out, for tests about wording.
 *
 * Not MiniMessage's parser: `:common` has no Adventure on its classpath. Only
 * an unescaped tag is a tag, as in MiniMessage.
 */
internal fun plain(styled: String): String =
    Regex("(?<!\\\\)</?[a-z_]+>").replace(styled, "")
        .replace("\\<", "<")
        .replace("\\\\", "\\")
