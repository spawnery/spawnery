package cloud.spawnery.agent

/**
 * The whole of what the command tree may ask of a platform.
 *
 * Two methods, and the number is the design. Every branch this adapter does
 * not offer is a branch the tree cannot take, so a decision that depends on
 * which side it is running on becomes impossible to write rather than merely
 * discouraged -- which is the argument [MirrorApi] makes by taking a
 * [cloud.spawnery.agent.api.Self] instead of asking.
 *
 * [send] takes a String and not an Adventure Component, though both platforms
 * speak Adventure. A Component in this signature would put a platform type in
 * a shared module, which is the trap the design records for Kotlin and for
 * protobuf one layer down. Each adapter converts at the last moment.
 *
 * @param S the platform's own command source -- Paper's CommandSourceStack,
 *   Velocity's CommandSource. Nothing here names either, and nothing in the
 *   tree may.
 */
interface SourceAdapter<S> {
    /**
     * Whether this source holds a permission node.
     *
     * Brigadier's `requires` makes an unpermitted branch *invisible* rather
     * than refused, so somebody without it sees "unknown command". That is the
     * platforms' own convention and the tree follows it -- which is why every
     * message the tree does send names the node it is talking about, since a
     * person who cannot see a branch has no other way to learn what reveals it.
     */
    fun hasPermission(source: S, permission: String): Boolean

    /** Sends one line to whoever ran the command. */
    fun send(source: S, message: String)
}
