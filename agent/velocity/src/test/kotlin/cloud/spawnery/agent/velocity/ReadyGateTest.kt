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
    fun `close releases the port`() = retryingOnAStolenPort {
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

    @Test
    fun `a first bind that fails stops the proxy`() {
        // Somebody else already holds the port, which is what a bind failure
        // is. A real pod cannot reach this state through its own netns; what
        // it can reach is the same IOException for a reason nobody predicted,
        // and the consequence is identical either way.
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
        // The cancelled-drain shape: the gate opened, a SetReady(false) closed
        // it, and the re-open cannot get the port back. This proxy has been in
        // its group's Service and may have players on it right now, so taking
        // the process down would disconnect every one of them to fix a
        // readiness signal.
        // A fixed port, not 0. With 0 the re-open binds a *different* free
        // port and succeeds, which is the behaviour this class's own comment
        // warns about and would make this test pass without exercising a
        // failed rebind at all.
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
        // The log line is what an operator reads next, and the two cases send
        // them to different places: one is a pod that will never work, the
        // other is a pod that is working and out of service.
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
     * the port.
     *
     * Three tests here ask the kernel for an ephemeral port, hand it straight
     * back, and then bind it again — to prove the gate released it, or to take
     * it away from the gate on purpose. Nothing reserves it in between, and an
     * ephemeral port is exactly what the kernel hands to the next outbound
     * connection on the machine. Measured 2026-08-27 on a machine kept busy:
     * ten failures in 150 runs of `:velocity:test`, 6.7%, spread over those
     * three tests and no other — every one a `java.net.BindException` on the
     * test's own line. That is almost certainly the flake that turned the
     * 0.2.5 release's CI red and passed on a re-run of the identical
     * derivation.
     *
     * A retry is sound here and would not be everywhere. What these tests
     * catch — a gate that leaked its listening socket, a rebind that reports
     * the wrong thing — is deterministic: it fails on every attempt. A stolen
     * port fails one. So retrying separates them instead of hiding either, and
     * twenty attempts against a one-in-fifteen race is a run that never sees
     * it rather than one that usually does not.
     *
     * A BindException can only come from the test's own sockets. [ReadyGate]
     * catches its own IOException and logs it — that is the behaviour two of
     * these tests are asserting — so it never throws one outward.
     *
     * One test here has the same assumption and is deliberately left alone.
     * `a fresh gate is closed and refuses connections` asks for a port and then
     * asserts a *connect* to it is refused, which needs somebody to be
     * listening rather than merely to have bound — a far rarer thing, measured
     * at none in 20 000 attempts here and none in 300 loaded runs of this
     * suite. Its failure would also be an AssertionError rather than a
     * BindException, so this helper would not catch it. If it ever does fail,
     * this is the shape of the answer.
     */
    private fun retryingOnAStolenPort(body: () -> Unit) {
        val attempts = 20
        repeat(attempts) { attempt ->
            try {
                body()
                return
            } catch (e: BindException) {
                if (attempt == attempts - 1) {
                    // Both readings, in the order they are worth suspecting.
                    // At the measured one-in-fifteen, twenty losses running is
                    // about one run in 10^23; a gate that is holding the port
                    // fails every attempt, always. Reporting only the race
                    // would give a real leak the one diagnosis that sends
                    // somebody to look at their machine instead of at the
                    // gate.
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
