package cloud.spawnery.agent.velocity

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.net.ServerSocket
import java.net.Socket

/**
 * Port 0 everywhere, never a literal. Production binds
 * internal/podspec.ProxyReadyPort (8081); a test that bound it too would fail
 * whenever a developer happened to have a proxy running, and would be a race
 * against any other test doing the same. Asking the kernel for a free port and
 * reading it back off [ReadyGate.boundPort] is what makes these tests
 * independent of the machine they run on -- and is the reason boundPort exists
 * at all.
 */
class ReadyGateTest {
    @Test
    fun `a fresh gate is closed and refuses connections`() {
        val gate = ReadyGate(0) { _, _ -> }

        // Construction must not bind. The whole point of the gate is that a
        // proxy is not ready until something decides it is, and a constructor
        // that bound would make the pod ready the moment the plugin loaded.
        assertFalse(gate.isOpen)
        assertEquals(-1, gate.boundPort)
    }

    @Test
    fun `open binds and accepts`() {
        val gate = ReadyGate(0) { _, _ -> }
        gate.open()
        try {
            assertTrue(gate.isOpen)
            Socket("127.0.0.1", gate.boundPort).use { assertTrue(it.isConnected) }
        } finally {
            gate.close()
        }
    }

    @Test
    fun `open is idempotent and keeps the same port`() {
        val gate = ReadyGate(0) { _, _ -> }
        gate.open()
        try {
            val first = gate.boundPort
            gate.open()
            // Not merely "still open": a second bind would take a *different*
            // ephemeral port, and the kubelet probes the one in the podspec.
            // Re-opening happens on every reconnect in task 7, so this is the
            // ordinary path and not an edge case.
            assertEquals(first, gate.boundPort)
        } finally {
            gate.close()
        }
    }

    @Test
    fun `the gate accepts more than one connection`() {
        val gate = ReadyGate(0) { _, _ -> }
        gate.open()
        try {
            // The kubelet probes every 5 s forever. A gate that served one
            // probe and stopped would turn a pod ready and then not-ready
            // again -- and it would not fail loudly: a bound socket with
            // nothing accepting still completes handshakes until the backlog
            // fills, so the symptom would appear minutes later, under load,
            // as flapping readiness.
            repeat(8) {
                Socket("127.0.0.1", gate.boundPort).use { assertTrue(it.isConnected) }
            }
        } finally {
            gate.close()
        }
    }

    @Test
    fun `close releases the port`() {
        val gate = ReadyGate(0) { _, _ -> }
        gate.open()
        val port = gate.boundPort
        gate.close()

        assertFalse(gate.isOpen)
        // A gate that closed its accept loop but leaked the listening socket
        // would keep the pod ready forever, including through the drain the
        // proxy is supposed to signal by going not-ready.
        ServerSocket(port).use { assertEquals(port, it.localPort) }
    }

    @Test
    fun `a port already in use is reported and leaves the gate closed`() {
        ServerSocket(0).use { held ->
            var message: String? = null
            val gate = ReadyGate(held.localPort) { text, _ -> message = text }

            // It must not throw. In task 7 open() is called from a gRPC
            // callback thread; an exception there is swallowed by the stream
            // observer and the proxy would go on running with no gate and no
            // explanation.
            gate.open()

            assertFalse(gate.isOpen)
            assertEquals(-1, gate.boundPort)
            val logged = message
            assertNotNull(logged, "a failed bind must be logged, not silent")
            // The port is in the message because the reason a bind fails is
            // almost always "something else has it", and the log line is the
            // only place that names which port to go looking for.
            assertTrue(logged.orEmpty().contains(held.localPort.toString()), logged.orEmpty())
        }
    }
}
