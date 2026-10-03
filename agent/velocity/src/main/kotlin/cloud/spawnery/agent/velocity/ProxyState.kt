package cloud.spawnery.agent.velocity

import java.util.concurrent.atomic.AtomicInteger

/**
 * The proxy's player count, written by Velocity's scheduler thread through
 * [sample] and read by the network side, so no Velocity call happens from a
 * gRPC callback thread.
 *
 * Unlike Paper's `ServerState`, no readiness flag: a proxy's readiness is the
 * [ReadyGate] socket the kubelet probes. [slots] is fixed for the pod's life
 * (`ProxyGroup.spec.config.playerLimit`) and a `val`, because the operator
 * discards every report with players above slots.
 */
class ProxyState(val slots: Int) {
    private val playerCount = AtomicInteger(0)

    val players: Int get() = playerCount.get()

    fun sample(players: Int) {
        playerCount.set(players)
    }
}
