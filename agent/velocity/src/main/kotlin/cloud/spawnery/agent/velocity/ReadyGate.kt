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
 * @param onHopeless called when the *first* bind fails, meaning this pod can
 *   never become ready and has never had a player. [AgentPlugin] passes
 *   Velocity's own shutdown; a test passes a flag. Default does nothing, so a
 *   gate constructed without one behaves as this class always did.
 *
 *   Declared before [log] rather than after it, and that is not cosmetic: log
 *   is the trailing lambda every existing caller passes positionally, and a
 *   parameter added after it silently rebinds that lambda to the new one.
 *   Kotlin refuses the result here because the types differ, which is luck
 *   rather than protection -- two callbacks of the same shape would have
 *   compiled and swapped meanings.
 * @param log where a failed bind goes. A callback rather than a logger,
 *   because the only logger this plugin has is the one Velocity injects into
 *   [AgentPlugin], and taking it here would make the class untestable without
 *   a proxy.
 */
class ReadyGate(
    private val port: Int,
    private val onHopeless: () -> Unit = {},
    private val log: (String, Throwable?) -> Unit,
) {
    // Guarded by `this`. open() and close() are called from a gRPC callback
    // thread and from Velocity's shutdown thread respectively, and the accept
    // loop reads the socket from a third; without the lock a close racing an
    // open leaks a listening socket, which is the one failure mode that leaves
    // a draining pod ready.
    private var socket: ServerSocket? = null
    private var acceptor: Thread? = null

    // Whether this gate has ever been bound. Guarded by `this` like the rest,
    // and never cleared by close(): what it answers is "could a player have
    // reached this pod", and a pod that was ready once is a pod the Service
    // once carried, whatever it is doing now.
    private var everOpened = false

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
     * # A bind that fails
     *
     * The two cases are not the same and only one of them is survivable.
     *
     * **The first bind.** This pod has never been ready, so it has never been
     * an endpoint of its group's Service, so no player has ever reached it and
     * none can. It also never will be: nothing retries a bind that failed, and
     * the kubelet will probe a port nothing is listening on for as long as the
     * pod lives. Carrying on means a pod stuck in Pending with the reason in a
     * container log and nothing on the group -- which is precisely what
     * docs/known-issues.md recorded. [onHopeless] ends it instead, so the
     * failure becomes a restart and then a CrashLoopBackOff, which the
     * operator does report.
     *
     * **A later bind**, after [close] and a re-open -- which is what a
     * cancelled drain produces. This proxy has been serving and may have
     * players on it right now, and taking the process down would disconnect
     * every one of them to fix a readiness signal. So that case is reported
     * and swallowed, exactly as before: the pod stays NotReady, out of the
     * Service, holding the sessions it has until they end.
     *
     * Either way the failure is not thrown. This runs on a gRPC callback
     * thread, where an exception is absorbed by the stream observer and the
     * proxy carries on with no gate and no explanation.
     */
    @Synchronized
    fun open() {
        if (socket != null) return

        val bound = try {
            ServerSocket(port)
        } catch (e: IOException) {
            if (everOpened) {
                log(
                    "spawnery ready gate could not rebind port $port; this proxy stays out of " +
                        "service and keeps the players it has",
                    e,
                )
            } else {
                log(
                    "spawnery ready gate could not bind port $port on this pod's first sync; " +
                        "it can never become ready, so the proxy is stopping to say so",
                    e,
                )
                onHopeless()
            }
            return
        }

        everOpened = true
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
