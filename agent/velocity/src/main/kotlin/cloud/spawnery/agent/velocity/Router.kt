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
 *
 * What it balances is this proxy's own view. `playersConnected` is what
 * Velocity itself can see, so with several proxies in one group the placement
 * is even per proxy and not necessarily across the network -- two proxies
 * choosing at the same moment both see the same emptiest backend and neither
 * knows what the other is carrying. Nothing here can fix that; it would take
 * a count the operator holds and hands back.
 */
class Router(private val directory: ServerDirectory) {
    /**
     * @param excluding server names never returned, compared
     *   case-insensitively -- matching [ServerDirectory]'s own lookup rule.
     *   [Drain] passes every server the operator is draining, so none of
     *   them is handed back as a replacement even while it is still, briefly,
     *   a member of one of [groups] -- neither the one being left, nor
     *   another that would only have to be left again. [Rescue] passes a
     *   whole chain: a player being bounced from one dead server to the next
     *   has to exclude every server that already refused them, not just the
     *   last one.
     */
    fun choose(groups: List<String>, excluding: Collection<String> = emptySet()): RegisteredServer? {
        for (group in groups) {
            val candidates = directory.inGroup(group)
                .filter { candidate -> excluding.none { candidate.serverInfo.name.equals(it, ignoreCase = true) } }
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
