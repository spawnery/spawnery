package cloud.spawnery.agent.velocity

/**
 * Moves every player off a server the operator is draining, onto whatever
 * [Router] picks from [toGroups].
 *
 * The operator repeats `DrainPlayers` after every periodic `FullSync` --
 * roughly every 30 seconds, for as long as the server keeps draining -- and
 * that repetition is what makes a dropped reconnect or an operator restart
 * mid-drain safe rather than a move-storm. It is free, and not a re-move of
 * everyone still nominally "draining", only because [run] re-reads
 * [PlayerRef.currentServer] on every call: a player whose earlier move
 * already landed is no longer on [fromServer], so a second identical call
 * moves nobody. [Drain] itself remembers nothing between calls -- the
 * proxy's own connection state is the only memory this needs.
 */
class Drain(
    private val players: Players,
    private val router: Router,
    private val log: (String, Throwable?) -> Unit,
) {
    fun run(fromServer: String, toGroups: List<String>) {
        val draining = players.all().filter {
            it.currentServer?.equals(fromServer, ignoreCase = true) == true
        }
        if (draining.isEmpty()) return

        // True once a null choice has been logged, so ten players stranded by
        // the same empty toGroups produce one log line, not ten.
        var loggedNoTarget = false

        for (player in draining) {
            // Asked fresh for every player, not once for the whole drain: a
            // single cached target would pile every drained player onto
            // whichever server was emptiest at the moment this message
            // arrived, instead of spreading them across toGroups the way
            // repeated per-player choices do.
            val target = router.choose(toGroups, excluding = setOf(fromServer))
            if (target == null) {
                if (!loggedNoTarget) {
                    log(
                        "spawnery: no target available in $toGroups to drain '$fromServer'; " +
                            "${draining.size} player(s) left in place",
                        null,
                    )
                    loggedNoTarget = true
                }
                continue
            }

            try {
                player.moveTo(target)
            } catch (e: Exception) {
                log("spawnery: failed to move '${player.username}' off draining server '$fromServer'", e)
            }
        }
    }
}
