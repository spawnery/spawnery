package cloud.spawnery.agent.velocity.voice

import cloud.spawnery.agent.pb.ProxyMessage
import cloud.spawnery.agent.pb.VoiceConfig
import cloud.spawnery.agent.pb.VoiceRelayState
import java.net.InetAddress
import java.util.UUID
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicBoolean
import cloud.spawnery.agent.pb.VoiceCounters as PbVoiceCounters

/**
 * The relay's lifecycle and the secret rewrite. [apply] runs on the gRPC
 * thread, [secretFor], [recordVersion] and [forget] on Velocity's network
 * threads.
 */
class VoiceControl(
    private val startRelay: (Int) -> AutoCloseable,
    private val hostIp: String?,
    private val counters: VoiceCounters,
    private val log: (String, Throwable?) -> Unit,
    private val gameAddressOf: (UUID) -> InetAddress? = { null },
) {
    @Volatile private var config: VoiceConfig? = null
    @Volatile private var everEnabled = false
    private var relay: AutoCloseable? = null
    @Volatile private var relayState = VoiceRelayState.VOICE_RELAY_STATE_STOPPED
    private val versions = ConcurrentHashMap<UUID, Int>()
    private val warnedVersion = ConcurrentHashMap.newKeySet<UUID>()
    private val warnedHostIp = AtomicBoolean(false)

    @Synchronized
    fun apply(config: VoiceConfig) {
        this.config = config
        if (config.enabled) {
            everEnabled = true
            if (relay == null) relay = start()
            relayState = if (relay != null) {
                VoiceRelayState.VOICE_RELAY_STATE_RUNNING
            } else {
                VoiceRelayState.VOICE_RELAY_STATE_BIND_FAILED
            }
        } else {
            relayState = VoiceRelayState.VOICE_RELAY_STATE_STOPPED
            val running = relay ?: return
            relay = null
            try {
                running.close()
            } catch (e: Exception) {
                log("spawnery: closing the voice relay failed", e)
            }
        }
    }

    private fun start(): AutoCloseable? =
        try {
            startRelay(RELAY_PORT)
        } catch (e: Exception) {
            log("spawnery: the voice relay cannot bind UDP port $RELAY_PORT; the next resync retries", e)
            null
        }

    fun secretFor(player: UUID, packet: ByteArray): ByteArray? {
        val config = config ?: return null
        if (!config.enabled || config.announcePort == 0) return null
        val host = if (config.announceOwnNode) {
            hostIp?.takeIf { it.isNotBlank() } ?: run {
                if (warnedHostIp.compareAndSet(false, true)) {
                    log("spawnery: voice is announced on this pod's node, but SPAWNERY_HOST_IP is unset; secrets pass through", null)
                }
                return null
            }
        } else {
            config.announceHost
        }
        val patched = patchSecret(packet, host, config.announcePort) as? SecretPatch.Patched
        if (patched == null || versions[player] != VoiceWire.PATCHED_VERSION) unknownVersion(player)
        return patched?.bytes
    }

    private fun unknownVersion(player: UUID) {
        counters.inc(VoiceCounters.UNKNOWN_VERSION)
        if (warnedVersion.add(player)) {
            log(
                "spawnery: player $player's voice chat is not on compatibility version " +
                    "${VoiceWire.PATCHED_VERSION}, or its secret has another layout; it is rewritten " +
                    "only where version ${VoiceWire.PATCHED_VERSION}'s layout parses",
                null,
            )
        }
    }

    fun recordVersion(player: UUID, request: ByteArray) {
        compatibilityVersion(request)?.let { versions[player] = it }
    }

    fun forget(player: UUID) {
        versions.remove(player)
        warnedVersion.remove(player)
    }

    fun gameAddress(player: UUID): InetAddress? =
        if (config?.clientIpPreserved == true) gameAddressOf(player) else null

    fun backendPort(): Int = config?.backendPort?.takeIf { it > 0 } ?: DEFAULT_BACKEND_PORT

    fun report(): ProxyMessage? {
        if (!everEnabled) return null
        return ProxyMessage.newBuilder()
            .setVoiceCounters(
                PbVoiceCounters.newBuilder().putAllTotals(counters.totals()).setRelayState(relayState),
            )
            .build()
    }

    private companion object {
        const val RELAY_PORT = 25565
        const val DEFAULT_BACKEND_PORT = 24454
    }
}
