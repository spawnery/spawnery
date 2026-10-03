package cloud.spawnery.agent

/**
 * The whole of what the command tree may ask of a platform, kept small so a
 * platform-dependent branch cannot be written. [send] takes a String, not an
 * Adventure Component, to keep platform types out of `:common`.
 *
 * @param S Paper's CommandSourceStack or Velocity's CommandSource.
 */
interface SourceAdapter<S> {
    /**
     * Brigadier's `requires` hides an unpermitted branch, so every message the
     * tree sends names the node it is about.
     */
    fun hasPermission(source: S, permission: String): Boolean

    fun send(source: S, message: String)

    /** Null for the console. An id, not a handle: [send] stays the only thing that talks. */
    fun playerId(source: S): java.util.UUID?
}
