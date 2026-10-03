package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.heapNow
import cloud.spawnery.agent.AgentRole
import cloud.spawnery.agent.CloudConnector
import cloud.spawnery.agent.CloudEvents
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.Feed
import cloud.spawnery.agent.pb.AgentServiceGrpc
import cloud.spawnery.agent.pb.BackendPlayers
import cloud.spawnery.agent.pb.PlayerRoster
import cloud.spawnery.agent.pb.RosterEntry
import cloud.spawnery.agent.pb.Hello
import cloud.spawnery.agent.pb.OperatorToProxy
import cloud.spawnery.agent.pb.PlayerCount
import cloud.spawnery.agent.pb.ProxyMessage
import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.StreamObserver
import cloud.spawnery.agent.pb.RegisteredServer as PbServer

/**
 * Velocity's half of the channel: what the proxy says, and what it does with
 * what the operator says back.
 *
 * Everything here runs on a gRPC callback thread, so every effect goes
 * through [ServerDirectory] or [Drain] and the player count is read out of
 * [ProxyState]. Two callback threads can be inside [onMessage] at once during
 * a make-before-break renewal.
 *
 * This agent never sends `Heartbeat`: the stream is its own liveness signal.
 */
class ProxyRole(
    private val state: ProxyState,
    private val directory: ServerDirectory,
    private val drain: Drain,
    /** The same instance [Drain] holds, so the report and the drain agree. */
    private val players: Players,
    /**
     * Reported once on the [hello], in milliseconds. Declared before the
     * lambdas so a positional caller cannot rebind a trailing lambda.
     */
    private val readTimeoutMillis: Int,
    private val onFirstSync: () -> Unit,
    /**
     * Called for every SetReady, except a `true` before the first FullSync,
     * which is recorded and not passed on. The operator re-sends its value on
     * every resync.
     */
    private val onSetReady: (Boolean) -> Unit,
    private val log: (String, Throwable?) -> Unit,
    private val mirror: NetworkMirror,
    private val connector: CloudConnector,
    private val feed: Feed,
    private val events: CloudEvents,
) : AgentRole<ProxyMessage, OperatorToProxy> {
    /**
     * Whether a `FullSync` has ever been applied, paired with the last
     * readiness the operator asserted (or null if it never has), read and
     * written as a pair.
     */
    private data class Latch(val synced: Boolean, val asserted: Boolean?)

    /** Guarded by [readiness]. */
    private var latch = Latch(synced = false, asserted = null)

    /**
     * Held across the [latch] update *and* the [onFirstSync]/[onSetReady] call
     * it decides. With only an atomic latch, two live streams could end with
     * `asserted = false` and the gate open.
     *
     * Lock order is `ProxyRole` then `ReadyGate`, never the reverse: neither
     * gate call reaches back into this class.
     */
    private val readiness = Any()

    override fun open(
        channel: ManagedChannel,
        credentials: CallCredentials,
        observer: StreamObserver<OperatorToProxy>,
    ): StreamObserver<ProxyMessage> =
        AgentServiceGrpc.newStub(channel).withCallCredentials(credentials).proxySession(observer)

    /**
     * `ready` is left unset: a proxy's readiness reaches the operator through
     * the kubelet's probe on [ReadyGate]'s port only.
     */
    override fun hello(version: String): ProxyMessage =
        ProxyMessage.newBuilder()
            .setHello(
                Hello.newBuilder()
                    .setVersion(version)
                    // Only the proxy knows its effective read timeout.
                    .setReadTimeoutMillis(readTimeoutMillis),
            )
            .build()

    /**
     * `slots` is the configured player limit, never zero: the operator
     * discards any report where players exceed slots.
     */
    override fun playerCount(): ProxyMessage {
        val (heapUsed, heapMax) = heapNow()
        return ProxyMessage.newBuilder()
            .setPlayerCount(
                PlayerCount.newBuilder()
                    .setPlayers(state.players)
                    .setSlots(state.slots)
                    .setHeapUsedBytes(heapUsed)
                    .setHeapMaxBytes(heapMax),
            )
            .build()
    }

    /**
     * How many of this proxy's players are on, or on their way to, each
     * backend; only backends with at least one player.
     *
     * [PlayerRef.attachedServer] and not [PlayerRef.currentServer]: a player
     * mid-handshake has no current server and the backend has not counted
     * them either.
     *
     * A live snapshot rather than counting joins and leaves, which would drift.
     * Runs on the reporting timer, so it must not block.
     */
    override fun extraReports(): List<ProxyMessage> {
        // One read of the roster for both messages, so they agree.
        val snapshot = players.all()

        val counts = mutableMapOf<String, Int>()
        val roster = PlayerRoster.newBuilder()
        for (player in snapshot) {
            val attached = player.attachedServer
            if (attached != null) {
                counts[attached] = (counts[attached] ?: 0) + 1
            }
            // Everyone, including a player attached to nothing.
            roster.addPlayers(
                RosterEntry.newBuilder()
                    .setUuid(player.uuid.toString())
                    .setName(player.username)
                    .setServer(attached ?: ""),
            )
        }

        return listOf(
            ProxyMessage.newBuilder()
                .setBackendPlayers(BackendPlayers.newBuilder().putAllPlayers(counts))
                .build(),
            ProxyMessage.newBuilder()
                .setPlayerRoster(roster)
                .build(),
        )
    }

    /**
     * Applies one operator message, and cannot throw: an exception would end
     * the stream, and the reconnect's FullSync would carry the same bad entry.
     */
    override fun onMessage(message: OperatorToProxy): Directive =
        runCatching { apply(message) }.getOrElse { error ->
            log("spawnery: failed to apply a ${message.messageCase} message from the operator", error)
            Directive.None
        }

    private fun apply(message: OperatorToProxy): Directive =
        when (message.messageCase) {
            OperatorToProxy.MessageCase.FULL_SYNC -> {
                directory.apply(message.fullSync.serversList.map(::backend))
                // Behind the apply: a sync that threw half-way says nothing,
                // and keeping the old drain set is the safe direction.
                drain.resynced()
                // After the apply, so a failed sync does not claim the latch.
                // Read, claim and gate call are one critical section; see
                // [readiness].
                synchronized(readiness) {
                    val previous = latch
                    latch = previous.copy(synced = true)
                    if (!previous.synced && previous.asserted != false) onFirstSync()
                }
                Directive.None
            }

            // Not a sync, so it does not open the gate.
            OperatorToProxy.MessageCase.REGISTER_SERVER -> {
                directory.add(backend(message.registerServer.server))
                Directive.None
            }

            OperatorToProxy.MessageCase.UNREGISTER_SERVER -> {
                directory.remove(message.unregisterServer.name)
                Directive.None
            }

            OperatorToProxy.MessageCase.DRAIN_PLAYERS -> {
                drain.run(message.drainPlayers.fromServer, message.drainPlayers.toGroupsList)
                Directive.None
            }

            OperatorToProxy.MessageCase.SET_READY -> {
                // Remembered as well as applied: the first FullSync must not
                // open a gate the operator has already closed.
                //
                // Recorded first, so a throwing `ReadyGate.close()` leaves the
                // latch saying not-ready.
                //
                // `ready` reaches the gate only once a FullSync has applied,
                // because readiness means routable; `!ready` always does.
                val ready = message.setReady.ready
                synchronized(readiness) {
                    val previous = latch
                    latch = previous.copy(asserted = ready)
                    if (!ready || previous.synced) onSetReady(ready)
                }
                Directive.None
            }

            OperatorToProxy.MessageCase.REPORT_INTERVAL ->
                Directive.Report(message.reportInterval.seconds)

            OperatorToProxy.MessageCase.SESSION_DEADLINE ->
                Directive.Deadline(
                    message.sessionDeadline.renewAfterSeconds,
                    message.sessionDeadline.hardDeadlineSeconds,
                )

            OperatorToProxy.MessageCase.CLOUD_RESPONSE -> {
                connector.answer(message.cloudResponse)
                Directive.None
            }
            OperatorToProxy.MessageCase.CLOUD_EVENT -> {
                // Buffered: the window that collapses events closes on the
                // proxy's own timer.
                feed.onEvent(message.cloudEvent)
                events.publish(message.cloudEvent)
                Directive.None
            }
            OperatorToProxy.MessageCase.NETWORK_STATE -> {
                mirror.apply(message.networkState)
                Directive.None
            }
            // Including MESSAGE_NOT_SET, for a newer operator.
            else -> Directive.None
        }

    /**
     * The address is passed through raw; [ServerDirectory] decides what an
     * unparsable one means.
     */
    private fun backend(server: PbServer): Backend =
        Backend(name = server.name, address = server.address, group = server.group)
}
