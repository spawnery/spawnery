package cloud.spawnery.agent

/**
 * The whole of what the command tree may ask of a platform.
 *
 * Three methods, and the number is the design. Every branch this adapter does
 * not offer is a branch the tree cannot take, so a decision that depends on
 * which side it is running on becomes impossible to write rather than merely
 * discouraged -- which is the argument [MirrorApi] makes by taking a
 * [cloud.spawnery.agent.api.Self] instead of asking.
 *
 * It said two until `/cloud events` needed to know who was typing. Growing it
 * is meant to cost an argument each time, which is why the number is written
 * down rather than left to be counted.
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

    /**
     * The id of the player behind this source, or null when there is not one.
     *
     * It earns its place because `/cloud events off` is a setting *for
     * somebody*, and the tree has no other way to ask who is typing. It is
     * deliberately not a way to reach the player: [send] is still the only
     * thing that talks, so a branch cannot start doing something to a player
     * merely because it can now name one.
     *
     * Null for the console, and that is a real answer rather than a gap. The
     * console is not a player, has no chat the feed would reach, and already
     * has every one of these lines in its own log.
     *
     * Note that this is *not* the type [FeedAudience] hands back ids for --
     * they are the same UUIDs, but the audience is a separate interface
     * because a Paper `CommandSourceStack` cannot be built from a player.
     */
    fun playerId(source: S): java.util.UUID?
}
