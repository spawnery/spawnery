package cloud.spawnery.agent.velocity

import java.util.concurrent.atomic.AtomicInteger

/**
 * The one fact the proxy agent reports, held where both Velocity's scheduler
 * thread and the network thread can reach it.
 *
 * The scheduler thread writes through [sample]; the network side only reads.
 * No Velocity call happens from a gRPC callback thread. Velocity's API is
 * widely held to be thread-safe where Bukkit's is not, and reading
 * `proxy.playerCount` straight from the reporting timer would very probably be
 * fine -- but "probably fine, by reputation" is exactly the class of claim this
 * agent has been caught out by before, and the scheduler hop costs nothing.
 *
 * Two deliberate differences from Paper's `ServerState`:
 *
 * There is no readiness flag and no `markReady`. A proxy's readiness is a
 * bound socket, not a bit on a message: `ProxyMessage` carries none, and
 * internal/agentserver's `handleProxy` says why -- the kubelet probes
 * [ReadyGate]'s port and has already written the answer where the ProxyGroup
 * controller reads it. Somewhere to *store* a readiness here would only invite
 * code that tried to send one.
 *
 * [slots] is a constructor argument rather than something sampled. Paper reads
 * `Bukkit.getMaxPlayers()` on every tick because a plugin can move it; a
 * proxy's capacity is `ProxyGroup.spec.config.playerLimit`, delivered once in
 * the pod's environment (see [ProxyEnvironment]) and unchangeable without a
 * restart. It is a `val` so that no future caller can lower it below a live
 * player count, which the operator's registry would answer by discarding every
 * report from this pod for as long as the two stayed crossed.
 */
class ProxyState(val slots: Int) {
    private val playerCount = AtomicInteger(0)

    val players: Int get() = playerCount.get()

    fun sample(players: Int) {
        playerCount.set(players)
    }
}
