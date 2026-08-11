package cloud.spawnery.agent.velocity

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.net.ConnectException
import java.net.InetSocketAddress
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
 *
 * Every connect that is expected to succeed uses an explicit [CONNECT_TIMEOUT]
 * rather than a bare `Socket(host, port)`. That is not defensiveness: a Linux
 * accept queue that has overflowed *drops* the SYN rather than resetting it, so
 * the client retries and a bare connect blocks for something over two minutes
 * before failing. With the timeout, a gate that has stopped accepting fails
 * these tests in three seconds and in the same way the kubelet would see it.
 */
class ReadyGateTest {
    @Test
    fun `a fresh gate is closed and refuses connections`() {
        // A port that is free, obtained the same way every other port here is:
        // ask the kernel for one and hand it straight back. Between the close
        // and the connect below nothing is listening on it, which is the state
        // this test needs and the one a literal port could not guarantee.
        val port = ServerSocket(0).use { it.localPort }

        val gate = ReadyGate(port) { _, _ -> }

        // Construction must not bind. The whole point of the gate is that a
        // proxy is not ready until something decides it is, and a constructor
        // that bound would make the pod ready the moment the plugin loaded.
        assertFalse(gate.isOpen)
        assertEquals(-1, gate.boundPort)
        // And the refusal the name promises, which the two assertions above do
        // not actually attempt. Refused and not merely unanswered: a closed
        // gate has to read as not-ready within milliseconds, where a port that
        // accepted and then hung would read as ready until the probe's
        // timeoutSeconds ran out.
        assertThrows(ConnectException::class.java) {
            Socket("127.0.0.1", port).close()
        }
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
    fun `the gate accepts past the listen backlog, not merely more than once`() {
        val gate = ReadyGate(0) { _, _ -> }
        gate.open()
        try {
            // The count is the entire test, and 8 -- what this was -- asserted
            // nothing at all. Measured 2026-08-11 on this kernel: a bound
            // ServerSocket with no accept() ever called still completes 51
            // connections, because java.net.ServerSocket(int) passes a backlog
            // of 50 and Linux queues one beyond it. A ReadyGate that bound the
            // port and never started its acceptor thread therefore passed the
            // old assertion unchanged, which is the one thing this test exists
            // to catch. 64 is past that queue with room for a kernel that
            // rounds the backlog up.
            //
            // The same measurement is the production failure mode, and it is
            // worse than "the probe succeeds anyway". Connection 52 was not
            // refused; it *timed out*. internal/podspec gives the readiness
            // probe periodSeconds 5 and timeoutSeconds 3, so a missing accept
            // loop surfaces about four minutes into a pod's life as readiness
            // flapping on probe timeouts, with nothing in any log to say why.
            repeat(64) {
                Socket().use { socket ->
                    socket.connect(InetSocketAddress("127.0.0.1", gate.boundPort), CONNECT_TIMEOUT)
                    assertTrue(socket.isConnected)
                }
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

    private companion object {
        // internal/podspec gives the proxy's readiness probe timeoutSeconds 3.
        // Matching it means a gate that has stopped accepting fails here in
        // exactly the time the kubelet would give it, rather than in the two
        // minutes a bare connect spends retrying a dropped SYN.
        const val CONNECT_TIMEOUT = 3_000
    }
}
