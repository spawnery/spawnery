package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer

/**
 * Chooses which server a player goes to, on join and on drain.
 *
 * [groups] is a *try list*, not a search space: the first group holding a
 * candidate wins, even if a later one has an emptier server, so an operator's
 * ordering means something.
 *
 * It balances by this proxy's own view of `playersConnected`, so several
 * proxies are even per proxy and not necessarily across the network.
 */
class Router(private val directory: ServerDirectory) {
    /**
     * @param excluding server names never returned, compared
     *   case-insensitively. [Drain] passes every draining server, [Rescue]
     *   the whole chain a player was already bounced through.
     */
    fun choose(
        groups: List<String>,
        excluding: Collection<String> = emptySet(),
        mayJoin: (server: String, group: String) -> Boolean = { _, _ -> true },
    ): RegisteredServer? {
        for (group in groups) {
            val candidates = directory.inGroup(group)
                .filter { candidate -> excluding.none { candidate.serverInfo.name.equals(it, ignoreCase = true) } }
                .filter { candidate -> mayJoin(candidate.serverInfo.name, group) }
            // Emptiness after the exclusion and the join rule, so a group either
            // one empties falls through to the next group.
            if (candidates.isEmpty()) continue

            // Ties break by name, for a deterministic choice.
            return candidates.minWithOrNull(compareBy({ it.playersConnected.size }, { it.serverInfo.name }))
        }
        return null
    }
}
