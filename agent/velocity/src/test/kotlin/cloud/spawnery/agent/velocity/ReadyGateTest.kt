package cloud.spawnery.agent.velocity

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.fail
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.net.BindException
import java.net.ConnectException
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.Socket

/**
 * Port 0 everywhere, read back off [ReadyGate.boundPort], never 8081.
 *
 * Connects that should succeed use [CONNECT_TIMEOUT]: an overflowed accept
 * queue drops the SYN, and a bare connect then blocks for over two minutes.
 */
class ReadyGateTest {
    @Test
    fun `a fresh gate is closed and refuses connections`() {
        // Free and with nothing listening between the close and the connect.
        val port = ServerSocket(0).use { it.localPort }

        val gate = ReadyGate(port) { _, _ -> }

        assertFalse(gate.isOpen)
        assertEquals(-1, gate.boundPort)
        // Refused, not merely unanswered: a closed gate must read as not-ready
        // at once.
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
            // A second bind would take a *different* ephemeral port, and the
            // kubelet probes the one in the podspec.
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
            // Past the accept queue: a bound socket with no accept() still
            // completes 51 connections (backlog 50, plus one), so a gate
            // without its acceptor passes any smaller count.
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
    fun `close releases the port`() = retryingOnAStolenPort {
        val gate = ReadyGate(0) { _, _ -> }
        gate.open()
        val port = gate.boundPort
        gate.close()

        assertFalse(gate.isOpen)
        ServerSocket(port).use { assertEquals(port, it.localPort) }
    }

    @Test
    fun `a port already in use is reported and leaves the gate closed`() {
        ServerSocket(0).use { held ->
            var message: String? = null
            val gate = ReadyGate(held.localPort) { text, _ -> message = text }

            // It must not throw: on a gRPC callback thread the stream observer
            // would swallow it.
            gate.open()

            assertFalse(gate.isOpen)
            assertEquals(-1, gate.boundPort)
            val logged = message
            assertNotNull(logged, "a failed bind must be logged, not silent")
            assertTrue(logged.orEmpty().contains(held.localPort.toString()), logged.orEmpty())
        }
    }

    private companion object {
        // The readiness probe's timeoutSeconds.
        const val CONNECT_TIMEOUT = 3_000
    }

    @Test
    fun `a first bind that fails stops the proxy`() {
        ServerSocket(0).use { taken ->
            var stopped = false
            val gate = ReadyGate(taken.localPort, onHopeless = { stopped = true }) { _, _ -> }

            gate.open()

            assertFalse(gate.isOpen, "the gate reports open on a port it did not get")
            assertTrue(
                stopped,
                "a proxy that can never serve its readiness probe carried on regardless, " +
                    "which leaves a pod stuck in Pending with the reason only in its own log",
            )
        }
    }

    @Test
    fun `a later bind that fails leaves the proxy alone`() = retryingOnAStolenPort {
        // The cancelled-drain shape: open, closed, and the re-open cannot get
        // the port back. A fixed port, not 0, or the re-open would bind a
        // different one and succeed.
        val port = ServerSocket(0).use { it.localPort }
        val gate = ReadyGate(port, onHopeless = { fail("the proxy was stopped with players possibly on it") }) { _, _ -> }
        gate.open()
        assertTrue(gate.isOpen, "the gate never opened, so there is no re-open to test")
        gate.close()

        ServerSocket(port).use {
            gate.open()
            assertFalse(gate.isOpen, "the gate reports open on a port somebody else holds")
        }
    }

    @Test
    fun `the two failures say different things`() = retryingOnAStolenPort {
        // The two cases send whoever reads the log to different places.
        val first = mutableListOf<String>()
        ServerSocket(0).use { taken ->
            ReadyGate(taken.localPort, onHopeless = {}) { m, _ -> first += m }.open()
        }
        assertTrue(first.any { it.contains("never become ready") }, "first bind: $first")

        val later = mutableListOf<String>()
        val port = ServerSocket(0).use { it.localPort }
        val gate = ReadyGate(port, onHopeless = {}) { m, _ -> later += m }
        gate.open()
        gate.close()
        ServerSocket(port).use { gate.open() }
        assertTrue(later.any { it.contains("keeps the players it has") }, "later bind: $later")
    }

    /**
     * Runs [body], and runs it again if the *test's own* bind lost a race for
     * the port: these tests hand an ephemeral port back and bind it again, and
     * the kernel may give it to another connection in between. The defects
     * they catch fail every attempt; a stolen port fails one. [ReadyGate]
     * never throws a BindException outward.
     */
    private fun retryingOnAStolenPort(body: () -> Unit) {
        val attempts = 20
        repeat(attempts) { attempt ->
            try {
                body()
                return
            } catch (e: BindException) {
                if (attempt == attempts - 1) {
                    // Both readings: a gate that holds the port fails every
                    // attempt.
                    throw AssertionError(
                        "could not bind the port on any of $attempts attempts. Either the gate " +
                            "is still holding it -- which is what this test exists to catch -- " +
                            "or this machine lost the race for an ephemeral port $attempts " +
                            "times running, which at the rate measured on 2026-08-27 is about " +
                            "one run in 10^23",
                        e,
                    )
                }
            }
        }
    }
}
