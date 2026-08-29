package cloud.spawnery.agent

/**
 * A styled line as a person reads it, with the markup taken back out.
 *
 * Tests about wording assert through this rather than against the raw string.
 * Colour splits a sentence into a dozen tagged fragments, so `contains("3
 * servers")` stops matching the moment anything is coloured -- and loosening
 * those assertions to match fragments would be testing the markup instead of
 * the message.
 *
 * Deliberately not MiniMessage's own parser: `:common` has no Adventure on its
 * classpath, which is the constraint [SourceAdapter] states and the build
 * enforces. This is a test-only inverse of [Style.escape] and the tag shapes
 * that file writes, and nothing more.
 *
 * **The lookbehind is the whole of it.** Stripping every `<...>` would take an
 * *escaped* one with it -- `\<red>` would lose its tag and leave a stray
 * backslash -- and the un-escaping below would then find nothing to undo. So
 * only an unescaped tag is a tag, which is the same rule MiniMessage itself
 * applies. The first version of this file got that wrong and a test caught it.
 */
internal fun plain(styled: String): String =
    Regex("(?<!\\\\)</?[a-z_]+>").replace(styled, "")
        .replace("\\<", "<")
        .replace("\\\\", "\\")
