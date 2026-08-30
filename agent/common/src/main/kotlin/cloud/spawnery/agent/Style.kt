package cloud.spawnery.agent

/**
 * The palette the `/cloud` tree writes in, and the escaping that makes it safe.
 *
 * **MiniMessage tags in a plain String, and no Adventure type anywhere here.**
 * `:common` has no Adventure on its compile classpath at all -- see
 * agent/common/build.gradle.kts -- so this file could not import one if it
 * wanted to, which is the constraint [SourceAdapter] states enforced by the
 * build rather than by discipline. Each platform adapter deserialises at the
 * last moment, exactly as it already converted a plain String before.
 *
 * MiniMessage and not the legacy section-sign codes: those are deprecated by
 * Adventure, and `§` in a value would inject formatting where `<` can be
 * escaped. Both platforms carry it -- measured 2026-08-29, Paper's
 * paper-repo/libraries ships adventure-text-minimessage 5.2.0 and the pinned
 * velocity jar bundles its own copy.
 *
 * **Colour carries meaning here or it is not used.** The one field that earns
 * it most is whether a server takes joins: that is the question somebody is
 * actually asking, and it disagrees with the phase during a drain.
 */
internal object Style {
    /** A server or group name, so a line can be scanned for the one you want. */
    fun name(value: String): String = "<aqua>${escape(value)}</aqua>"

    /** Something that went right. */
    fun good(value: String): String = "<green>${escape(value)}</green>"

    /** Something somebody should look at. */
    fun bad(value: String): String = "<red>${escape(value)}</red>"

    /** Context rather than news: the sentences that explain a result. */
    fun quiet(value: String): String = "<gray>${escape(value)}</gray>"

    /** A figure worth the eye landing on. */
    fun number(value: Any): String = "<white>${escape(value.toString())}</white>"

    /**
     * Plain text that must not be read as markup.
     *
     * Every value this file interpolates goes through it. Server and group
     * names are Kubernetes object names and can hold neither `<` nor `\`, but
     * the operator's own refusal messages reach chat verbatim -- see
     * CloudCommand's `reason` -- and one containing a `<` would otherwise be
     * eaten by the parser or make it throw inside a network callback.
     *
     * The backslash goes first. Escaping `<` first would then have its own
     * backslash escaped again, turning `\<` into `\\<` and printing the
     * backslash.
     */
    fun escape(value: String): String =
        value.replace("\\", "\\\\").replace("<", "\\<")
}
