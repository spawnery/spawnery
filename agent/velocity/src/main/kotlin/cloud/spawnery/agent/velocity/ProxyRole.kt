package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.AgentRole
import cloud.spawnery.agent.Directive
import cloud.spawnery.agent.pb.AgentServiceGrpc
import cloud.spawnery.agent.pb.Hello
import cloud.spawnery.agent.pb.OperatorToProxy
import cloud.spawnery.agent.pb.PlayerCount
import cloud.spawnery.agent.pb.ProxyMessage
import io.grpc.CallCredentials
import io.grpc.ManagedChannel
import io.grpc.stub.StreamObserver
import java.util.concurrent.atomic.AtomicBoolean
import cloud.spawnery.agent.pb.RegisteredServer as PbServer

/**
 * Velocity's half of the channel: what the proxy says, and what it does with
 * what the operator says back.
 *
 * Everything here runs on a gRPC callback thread. Nothing on that thread may
 * touch Velocity's own state directly, which is why every effect goes through
 * [ServerDirectory] or [Drain] -- both of which were built to be driven from
 * exactly here -- and why the player count is read out of [ProxyState] rather
 * than off the proxy.
 *
 * **What that does not mean is that the state those two carry is single-
 * threaded.** [ServerDirectory] is `@Synchronized` throughout, and the reason
 * is on its side of the seam rather than this one: this class is its only
 * writer, but its reader is [Router.choose], which `AgentPlugin` reaches from
 * **Velocity's event thread** every time a player joins. A `FullSync` applied
 * here while a player is being routed there is two threads on one map. (Nor
 * is the writer itself reliably one thread: `SessionLoop`'s make-before-break
 * renewal runs two streams on two `ManagedChannel`s at once, so two callback
 * threads can be inside [onMessage] while the displaced one drains.)
 *
 * **This agent never sends `Heartbeat`.** The message exists in `ProxyMessage`
 * and internal/agentserver has a branch for it that deliberately does nothing:
 * the stream is its own liveness signal, and the registry's staleness rule
 * already derives from `ReportInterval`. A second liveness path would be a
 * second truth about the same fact. Its absence below is a decision, not an
 * oversight.
 *
 * @param onFirstSync opens the pod's readiness gate. A lambda rather than the
 *   [ReadyGate] itself, so `ProxyRoleTest` can count how often this role
 *   decides the gate should open -- [ReadyGate.open] is idempotent, so a role
 *   that asked on every sync would be indistinguishable from this one through
 *   the gate alone.
 */
class ProxyRole(
    private val state: ProxyState,
    private val directory: ServerDirectory,
    private val drain: Drain,
    private val onFirstSync: () -> Unit,
    /**
     * Sets the pod's readiness gate. Called for every SetReady the operator
     * sends, which it re-asserts whenever it syncs rather than only on a
     * change -- so this may be called with the value it already has.
     */
    private val onSetReady: (Boolean) -> Unit,
    private val log: (String, Throwable?) -> Unit,
) : AgentRole<ProxyMessage, OperatorToProxy> {
    /**
     * Whether a `FullSync` has ever been applied. Atomic rather than a plain
     * `Boolean`, because during a make-before-break renewal two gRPC callback
     * threads can be applying a `FullSync` at the same time -- one per live
     * stream -- and a lost transition would open the gate twice or, worse,
     * never.
     */
    private val synced = AtomicBoolean(false)

    /**
     * The last readiness the operator asserted, or null if it never has.
     * Read by the FullSync branch so a standing instruction wins over the
     * gate-opening that a first sync would otherwise do.
     */
    @Volatile
    private var asserted: Boolean? = null

    override fun open(
        channel: ManagedChannel,
        credentials: CallCredentials,
        observer: StreamObserver<OperatorToProxy>,
    ): StreamObserver<ProxyMessage> =
        AgentServiceGrpc.newStub(channel).withCallCredentials(credentials).proxySession(observer)

    /**
     * `ready` is left unset, and that is the point rather than an omission.
     *
     * `Hello.ready` is meaningful for server agents only: a proxy's readiness
     * reaches the operator through the kubelet's probe on [ReadyGate]'s port
     * and nowhere else, which is what internal/agentserver's `handleProxy`
     * states in its own comment. Setting it here would put a second, and
     * eventually contradicting, source under a fact the kubelet owns -- one
     * that would read as authoritative to anyone who found it.
     */
    override fun hello(version: String): ProxyMessage =
        ProxyMessage.newBuilder()
            .setHello(Hello.newBuilder().setVersion(version))
            .build()

    /**
     * The proxy reports its configured player limit as `slots`, never zero:
     * the operator's registry discards any report where players exceed slots,
     * so a proxy that reported no capacity would have every report with a
     * player online thrown away -- visible only as a counter, while its
     * recorded player count sat at zero. See the `PlayerCount` comment in
     * proto/spawnery/agent/v1alpha1/agent.proto.
     */
    override fun playerCount(): ProxyMessage =
        ProxyMessage.newBuilder()
            .setPlayerCount(
                PlayerCount.newBuilder().setPlayers(state.players).setSlots(state.slots),
            )
            .build()

    /**
     * Applies one operator message, and cannot throw.
     *
     * `SessionLoop` calls this from a gRPC callback thread with no guard of
     * its own, and an exception escaping here would end the stream: one
     * malformed `RegisteredServer` in one `FullSync` would cost this proxy its
     * session -- and, since a reconnect starts with a fresh `FullSync`
     * carrying the same entry, cost it every session after that too, on the
     * reconnect backoff, forever. Skipping the message and logging it leaves
     * the proxy on a slightly stale server list, which is the strictly better
     * of the two.
     *
     * The guard wraps [apply] whole rather than sitting inside any branch, so
     * it covers the branches that do work -- which is all of them except the
     * two that only build a directive -- and cannot be narrowed by adding a
     * case.
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
                // After the apply and not before it: a sync that threw
                // half-way has not given this proxy a server list, and the
                // operator repeats FullSync roughly every 30 seconds. Claiming
                // the latch first would spend the pod's one chance to become
                // ready on the attempt that failed.
                if (synced.compareAndSet(false, true) && asserted != false) onFirstSync()
                Directive.None
            }

            // Not a sync: an incremental register says one backend appeared,
            // not that this is the list. A proxy that turned ready on it would
            // be routing against whatever fragment happened to arrive first.
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
                // open a gate the operator has already closed. A pod that goes
                // surplus while it is still starting gets the instruction
                // before its first sync, and opening on that sync would put a
                // draining proxy back into the Service's endpoints.
                asserted = message.setReady.ready
                onSetReady(message.setReady.ready)
                Directive.None
            }

            OperatorToProxy.MessageCase.REPORT_INTERVAL ->
                Directive.Report(message.reportInterval.seconds)

            OperatorToProxy.MessageCase.SESSION_DEADLINE ->
                Directive.Deadline(
                    message.sessionDeadline.renewAfterSeconds,
                    message.sessionDeadline.hardDeadlineSeconds,
                )

            // Including MESSAGE_NOT_SET. A newer operator against an older
            // agent has to keep working, exactly as handleProxy's own unknown
            // branch does in the other direction.
            else -> Directive.None
        }

    /**
     * The one place the proto's `RegisteredServer` becomes a [Backend], shared
     * by both branches that carry one. The address is passed through as the
     * raw string it arrived as: deciding that an unparsable one is a
     * skip-and-log rather than a throw belongs to [ServerDirectory], and
     * splitting that decision across two files is how the two halves drift.
     */
    private fun backend(server: PbServer): Backend =
        Backend(name = server.name, address = server.address, group = server.group)
}
