package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.RegisteredServer

/**
 * Chooses which server a player goes to, on join and on drain.
 *
 * [groups] -- `fallbackGroups`, as internal/podspec and [ProxyEnvironment]
 * carry it -- is a *try list*, not a search space. [choose] walks it in
 * order and the first group holding at least one candidate (after
 * [excluding], if given) wins outright; later groups are never consulted,
 * even if one of them would have offered an emptier server. That is what
 * lets an operator put a small, always-available lobby group ahead of a
 * bigger, usually-emptier hub group and have it mean something: hunting for
 * the global minimum across every group would silently undo that ordering
 * the first time the lobby got busy.
 */
class Router(private val directory: ServerDirectory) {
    /**
     * @param excluding a server name never returned, compared
     *   case-insensitively -- matching [ServerDirectory]'s own lookup rule.
     *   [Drain] passes the server it is draining players off of here, so
     *   that server is never handed back as its own replacement even while it
     *   is still, briefly, a member of one of [groups].
     */
    fun choose(groups: List<String>, excluding: String? = null): RegisteredServer? {
        for (group in groups) {
            val candidates = directory.inGroup(group)
                .filter { excluding == null || !it.serverInfo.name.equals(excluding, ignoreCase = true) }
            // Emptiness is decided after the exclusion, not before it: a group
            // whose only member is the server being drained holds no candidate
            // and has to fall through to the next group. Checking
            // inGroup(group).isEmpty() first instead would return null there
            // rather than the fallback -- see RouterTest's "a group the
            // exclusion empties falls through to the next group", which is the
            // one test that separates the two orderings.
            if (candidates.isEmpty()) continue

            // Ties break by name so the choice is deterministic rather than
            // dependent on directory.inGroup's iteration order.
            return candidates.minWithOrNull(compareBy({ it.playersConnected.size }, { it.serverInfo.name }))
        }
        return null
    }
}
