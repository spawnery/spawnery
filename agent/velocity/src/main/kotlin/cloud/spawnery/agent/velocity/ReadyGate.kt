package cloud.spawnery.agent.velocity

import java.io.IOException
import java.net.ServerSocket

/**
 * The proxy's readiness signal, as a TCP port that either answers or does not.
 *
 * internal/podspec gives the proxy container a `tcpSocket` readiness probe on
 * [cloud.spawnery.agent.velocity.AgentPlugin] READY_PORT and nothing else. The
 * proxy's own listener on 25565 answers long before it has a server list, and
 * a proxy without one disconnects every player with "no available server".
 *
 * Nothing is ever written to or read from an accepted connection.
 *
 * @param port the port to bind; tests pass 0 and read [boundPort] back.
 * @param onHopeless called when the *first* bind fails, meaning this pod can
 *   never become ready and has never had a player. Declared before [log] so
 *   a positional trailing lambda keeps binding to [log].
 * @param log where a failed bind goes.
 */
class ReadyGate(
    private val port: Int,
    private val onHopeless: () -> Unit = {},
    private val log: (String, Throwable?) -> Unit,
) {
    // Guarded by `this`: open, close and the accept loop run on three threads,
    // and a close racing an open would leak a listening socket.
    private var socket: ServerSocket? = null
    private var acceptor: Thread? = null

    // Never cleared by close(): it answers "could a player have reached this
    // pod".
    private var everOpened = false

    val isOpen: Boolean
        @Synchronized get() = socket != null

    /** The port actually bound, or -1 while closed. */
    val boundPort: Int
        @Synchronized get() = socket?.localPort ?: -1

    /**
     * Binds the port, idempotently: every reconnect's first FullSync calls it.
     *
     * A failed **first** bind calls [onHopeless]: nothing retries it, and the
     * pod would sit unready with the reason only in a container log. A failed
     * **later** bind (after a cancelled drain) is logged and swallowed,
     * because this proxy may have players on it right now.
     *
     * Never thrown: on a gRPC callback thread the stream observer would
     * absorb it.
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
            isDaemon = true
            start()
        }
    }

    /** Idempotent, and safe to call on a gate never opened. */
    @Synchronized
    fun close() {
        // Thread.interrupt() does not unblock a socket accept; closing does.
        socket?.close()
        socket = null
        acceptor = null
    }

    /**
     * Accepts and immediately closes, forever. Without it the accept queue
     * fills after about 51 connections, and further SYNs are dropped rather
     * than refused, so the kubelet's probes start timing out minutes into a
     * pod's life.
     */
    private fun accept(bound: ServerSocket) {
        while (!bound.isClosed) {
            try {
                bound.accept().close()
            } catch (e: IOException) {
                // close() is the expected way out of accept().
                if (bound.isClosed) return
                log("spawnery ready gate failed to accept a probe", e)
            }
        }
    }
}
