package cloud.spawnery.agent.velocity.voice

import cloud.spawnery.agent.pb.VoiceConfig
import org.junit.jupiter.api.Assertions.assertArrayEquals
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import java.util.UUID

class VoiceControlTest {
    private val player = UUID.randomUUID()
    private var started = 0
    private var closed = 0
    private val counters = VoiceCounters()
    private fun control(hostIp: String? = "192.0.2.5") = VoiceControl(
        startRelay = { port -> assertEquals(25565, port); started++; AutoCloseable { closed++ } },
        hostIp = hostIp, counters = counters, log = { _, _ -> },
    )
    private fun config(enabled: Boolean = true, host: String = "", port: Int = 25565, ownNode: Boolean = false) =
        VoiceConfig.newBuilder().setEnabled(enabled).setBackendPort(24454)
            .setAnnounceHost(host).setAnnouncePort(port).setAnnounceOwnNode(ownNode).build()

    @Test
    fun `enabled starts the relay once, disabled stops it`() {
        val c = control()
        c.apply(config()); c.apply(config())
        assertEquals(1, started)
        c.apply(config(enabled = false))
        assertEquals(1, closed)
    }

    @Test
    fun `a version 20 client's secret points at the announced port`() {
        val c = control()
        c.apply(config(port = 30765))
        c.recordVersion(player, byteArrayOf(0, 0, 0, 20))
        val patched = c.secretFor(player, secretV20(24454, "x"))!!
        assertArrayEquals(secretV20(30765, ""), patched)
    }

    @Test
    fun `own node announces this pod's node`() {
        val c = control(hostIp = "192.0.2.5")
        c.apply(config(port = 25577, ownNode = true))
        c.recordVersion(player, byteArrayOf(0, 0, 0, 20))
        assertArrayEquals(secretV20(25577, "192.0.2.5"), c.secretFor(player, secretV20(24454, ""))!!)
    }

    @Test
    fun `an unknown endpoint forwards the secret unchanged`() {
        val c = control()
        c.apply(config(port = 0))
        assertNull(c.secretFor(player, secretV20(24454, "")))
    }

    @Test
    fun `an unknown version that parses is patched and counted`() {
        val c = control()
        c.apply(config())
        c.recordVersion(player, byteArrayOf(0, 0, 0, 21))
        assertNotNull(c.secretFor(player, secretV20(24454, "")))
        assertEquals(1L, counters.totals()[VoiceCounters.UNKNOWN_VERSION])
    }

    @Test
    fun `a failed bind leaves the relay stopped and the next config retries`() {
        var attempts = 0
        val c = VoiceControl(
            startRelay = { attempts++; if (attempts == 1) throw java.net.BindException("in use") else AutoCloseable { } },
            hostIp = null, counters = counters, log = { _, _ -> },
        )
        c.apply(config())
        c.apply(config())
        assertEquals(2, attempts)
        c.apply(config())
        assertEquals(2, attempts)
    }

    @Test
    fun `own node without a node address forwards the secret unchanged`() {
        val c = control(hostIp = null)
        c.apply(config(ownNode = true))
        c.recordVersion(player, byteArrayOf(0, 0, 0, 20))
        assertNull(c.secretFor(player, secretV20(24454, "")))
    }

    @Test
    fun `counters are reported only once a config has said enabled`() {
        val c = control()
        c.apply(config(enabled = false))
        assertNull(c.report())
        c.apply(config())
        c.apply(config(enabled = false))
        assertNotNull(c.report()!!.voiceCounters)
    }
}
