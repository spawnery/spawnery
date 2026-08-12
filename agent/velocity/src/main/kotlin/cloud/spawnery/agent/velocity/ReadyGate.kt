package cloud.spawnery.agent.velocity

import java.io.IOException
import java.net.ServerSocket

/**
 * The proxy's readiness signal, as a TCP port that either answers or does not.
 *
 * internal/podspec gives the proxy container a `tcpSocket` readiness probe on
 * [cloud.spawnery.agent.velocity.AgentPlugin] READY_PORT and nothing else, so
 * "ready" means exactly "this port accepts a connection". The proxy's own
 * listener on 25565 answers long before it has a server list, which is why the
 * probe does not point at it: a proxy that got traffic without a list would
 * disconnect every player with "no available server". The gate is opened
 * elsewhere, once there is a list.
 *
 * Nothing is ever written to an accepted connection and nothing is read from
 * it. A `tcpSocket` probe completes the handshake and closes; a protocol here
 * would be a protocol nobody speaks.
 *
 * @param port the port to bind. Production passes 8081; the tests pass 0 and
 *   read [boundPort] back, which is why this is a parameter at all.
 * @param log where a failed bind goes. A callback rather than a logger,
 *   because the only logger this plugin has is the one Velocity injects into
 *   [AgentPlugin], and taking it here would make the class untestable without
 *   a proxy.
 */
class ReadyGate(private val port: Int, private val log: (String, Throwable?) -> Unit) {
    // Guarded by `this`. open() and close() are called from a gRPC callback
    // thread and from Velocity's shutdown thread respectively, and the accept
    // loop reads the socket from a third; without the lock a close racing an
    // open leaks a listening socket, which is the one failure mode that leaves
    // a draining pod ready.
    private var socket: ServerSocket? = null
    private var acceptor: Thread? = null

    /** Whether the gate is bound, and therefore whether the pod is ready. */
    val isOpen: Boolean
        @Synchronized get() = socket != null

    /**
     * The port actually bound, or -1 while closed.
     *
     * -1 and not [port]: with port 0 the two are different numbers, and a
     * caller that read back 0 would have no way to tell "not open" from "open
     * on a port I cannot name".
     */
    val boundPort: Int
        @Synchronized get() = socket?.localPort ?: -1

    /**
     * Binds the port, idempotently.
     *
     * Idempotence is the ordinary path and not an edge case: the session
     * reconnects, and every reconnect's first FullSync would otherwise rebind.
     * On port 0 that would produce a *different* port each time while the
     * kubelet went on probing the one in the podspec.
     *
     * A bind failure is reported and swallowed. This runs on a gRPC callback
     * thread, where a thrown exception is absorbed by the stream observer and
     * the proxy carries on with no gate and no explanation; a logged failure
     * plus a pod that never turns ready is the same outcome with a cause
     * attached.
     */
    @Synchronized
    fun open() {
        if (socket != null) return

        val bound = try {
            ServerSocket(port)
        } catch (e: IOException) {
            log("spawnery ready gate could not bind port $port; this proxy will not become ready", e)
            return
        }

        socket = bound
        acceptor = Thread({ accept(bound) }, "spawnery-ready-gate").apply {
            // A daemon thread, so a failure to close it can never be what
            // holds the JVM open past a drain.
            isDaemon = true
            start()
        }
    }

    /** Releases the port. Idempotent, and safe to call on a gate never opened. */
    @Synchronized
    fun close() {
        // Closing the ServerSocket is what stops the loop: it makes the
        // blocking accept() throw, which is the only way to interrupt it —
        // Thread.interrupt() does not unblock a socket accept.
        socket?.close()
        socket = null
        acceptor = null
    }

    /**
     * Accepts and immediately closes, forever.
     *
     * The loop is not an optimisation, and the numbers are worth having.
     * Measured 2026-08-11: a bound socket with nothing accepting still
     * completes **51** connections — `java.net.ServerSocket(int)` passes a
     * backlog of 50 and Linux queues one beyond it — and the 52nd is not
     * refused, it *times out*, because an overflowed accept queue drops the
     * SYN rather than resetting it.
     *
     * So a gate without this loop passes any test that probes it a handful of
     * times, comes up green in an image test, and then, about four minutes into
     * a pod's life, starts failing the kubelet's five-second probes on their
     * three-second timeout — readiness flapping, with nothing in any log and
     * nothing that ever logged an error. ReadyGateTest's connection count is
     * chosen against these numbers for exactly that reason.
     */
    private fun accept(bound: ServerSocket) {
        while (!bound.isClosed) {
            try {
                bound.accept().close()
            } catch (e: IOException) {
                // close() is the expected way out of accept(), and reporting
                // it would put a warning in the log of every clean shutdown.
                // Anything else is worth one line: the gate is still bound, so
                // the pod stays ready, and the only evidence is here.
                if (bound.isClosed) return
                log("spawnery ready gate failed to accept a probe", e)
            }
        }
    }
}
