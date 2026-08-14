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
     * Sets the pod's readiness gate.
     *
     * Called for every SetReady the operator sends, with one exception: a
     * `true` that arrives before a FullSync has applied is recorded and not
     * passed on, because readiness means routable. See the SET_READY branch.
     *
     * The operator re-sends the value it last asserted on every resync -- one
     * per `proxyreg.DefaultResyncInterval`, 30 seconds -- as well as when that
     * value changes, so this is called with the value it already has, roughly
     * that often, for as long as a drain lasts.
     */
    private val onSetReady: (Boolean) -> Unit,
    private val log: (String, Throwable?) -> Unit,
) : AgentRole<ProxyMessage, OperatorToProxy> {
    /**
     * Whether a `FullSync` has ever been applied, paired with the last
     * readiness the operator asserted (or null if it never has) -- read and
     * written as a pair, never as two independent fields.
     *
     * The FULL_SYNC branch below has to read them together, because a standing
     * not-ready has to survive the first sync; and during a make-before-break
     * renewal two gRPC callback threads really can be inside [apply] at once,
     * one per live stream (see [SessionLoop]'s class comment for why that is a
     * real window and not a hypothetical one). Two independent reads there can
     * straddle a concurrent `SetReady` write, observing a stale `asserted`
     * beside whichever `synced` transition won.
     */
    private data class Latch(val synced: Boolean, val asserted: Boolean?)

    /** Guarded by [readiness]. */
    private var latch = Latch(synced = false, asserted = null)

    /**
     * The monitor the two readiness branches of [apply] hold -- across the
     * [latch] update *and* the [onFirstSync]/[onSetReady] call that update
     * decides, as one step rather than two.
     *
     * An `AtomicReference<Latch>` stood here before, with no lock, and it was
     * not enough. It made the *read* of the pair atomic; the gate call is
     * outside the read, and nothing ordered one thread's gate call against
     * another's. With two live streams that admitted:
     *
     *     FULL_SYNC  reads (synced = false, asserted = null), claims synced
     *     SET_READY  records asserted = false, and closes the gate
     *     FULL_SYNC  acts on the pair it read: opens the gate
     *
     * ending at `asserted = false` with the gate **open** -- the pod Ready, in
     * the Service's endpoints and taking new players, while the operator's
     * drain deadline runs against it. `ProxyRoleTest`'s two-thread case drives
     * exactly that, and measured it landing about once in 570 attempts.
     *
     * Under this monitor a concurrent `SetReady` either runs wholly before the
     * FULL_SYNC block, and is therefore in the pair it reads, or wholly after
     * it, and closes a gate that block had just opened. There is no third
     * ordering, and the end state agrees with `asserted` either way.
     *
     * **Why the lock is safe**, since an earlier version of this comment
     * declined one over a deadlock: [onFirstSync] and [onSetReady] both end in
     * [ReadyGate], whose `open()`/`close()` are `@Synchronized`, so the order
     * taken here is `ProxyRole` then `ReadyGate`. It is one-directional.
     * `ReadyGate.open()` binds a `ServerSocket` and starts the acceptor
     * thread; `close()` closes that socket. Neither calls back into this
     * class, and neither waits on a thread that could: the acceptor holds
     * neither monitor, and the only other call out of either is into the
     * logger Velocity injected, on a bind or accept failure. With no cycle
     * there is nothing to deadlock on. A third callback added to this class
     * owns keeping that true.
     *
     * The rest of [apply] stays outside: `directory.apply` and `drain.run`
     * take [ServerDirectory]'s monitor and can run long, and neither touches
     * the latch or the gate.
     */
    private val readiness = Any()

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
                //
                // The read, the claim and the gate call are one critical
                // section, not three steps: `previous` is the pair as it stood
                // immediately before this attempt won the transition to
                // synced, and no SetReady can land between reading it and
                // acting on it. See the `readiness` doc comment for the
                // interleaving that costs, and for why holding a monitor
                // across onFirstSync cannot deadlock.
                synchronized(readiness) {
                    val previous = latch
                    latch = previous.copy(synced = true)
                    if (!previous.synced && previous.asserted != false) onFirstSync()
                }
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
                //
                // Recorded and applied inside one critical section, so a
                // concurrent FullSync thread sees both or neither -- there is
                // no window here for it to read a stale `asserted` beside a
                // gate this has already moved.
                //
                // The record still goes first within the block, where the
                // order is no longer observable from another thread but is
                // still observable after a throw: `ReadyGate.close()` can
                // raise an IOException, and onMessage swallows it. Recording
                // first leaves a latch saying not-ready over a gate that may
                // not have shut, which the next SetReady or the operator's
                // next resync repeats; recording second would leave a shut
                // gate with `asserted` unset, which the next FullSync would
                // reopen.
                //
                // `ready` reaches the gate only once a FullSync has applied;
                // `!ready` always does. Readiness means routable, and a proxy
                // with no server list routes nobody: it would take players out
                // of the Service and disconnect each with "no available
                // server". Nothing is lost by waiting -- `asserted` is
                // recorded either way, and the FULL_SYNC branch above opens
                // the gate on the first sync that applies, because
                // `asserted != false` holds for a standing true.
                //
                // The one case this changes is a `directory.apply` that keeps
                // throwing while the operator wants this proxy ready: the pod
                // stays NotReady until a sync gets through, where it used to
                // go Ready with an empty routing table. Closing is not gated
                // for the mirror of the same reason: a proxy that cannot apply
                // its directory has no business in the endpoints, and refusing
                // a close is the one direction that leaves a draining pod
                // ready.
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
