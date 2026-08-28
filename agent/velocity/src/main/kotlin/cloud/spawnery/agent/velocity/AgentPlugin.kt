package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.MirrorApi
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.Spawnery
import cloud.spawnery.agent.BearerCredentials
import cloud.spawnery.agent.OperatorChannel
import cloud.spawnery.agent.SessionLoop
import cloud.spawnery.agent.TokenSource
import cloud.spawnery.agent.pb.OperatorToProxy
import cloud.spawnery.agent.pb.PlayerJoinedServer
import cloud.spawnery.agent.pb.ProxyMessage
import com.google.inject.Inject
import com.velocitypowered.api.event.Subscribe
import com.velocitypowered.api.event.connection.DisconnectEvent
import com.velocitypowered.api.event.player.KickedFromServerEvent
import com.velocitypowered.api.event.player.PlayerChooseInitialServerEvent
import com.velocitypowered.api.event.player.ServerConnectedEvent
import com.velocitypowered.api.event.player.ServerPostConnectEvent
import com.velocitypowered.api.event.proxy.ProxyInitializeEvent
import com.velocitypowered.api.event.proxy.ProxyShutdownEvent
import com.velocitypowered.api.plugin.Plugin
import com.velocitypowered.api.proxy.ProxyServer
import com.velocitypowered.api.scheduler.ScheduledTask
import org.slf4j.Logger
import java.nio.file.Files
import java.nio.file.Path
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit

/**
 * The only class in this plugin that touches the Velocity API.
 *
 * That is not tidiness for its own sake: it is what lets every other unit be
 * tested with JUnit and no proxy. Nothing here decides anything — the
 * decisions live in [ProxyEnvironment], [ReadyGate], [ServerDirectory],
 * [Router], [Drain], [ProxyRole] and `SessionLoop`. This class only wires them
 * to Velocity's events and to its scheduler, which is also why it has no test
 * of its own: what would be under test is Velocity's behaviour.
 *
 * Velocity reads this plugin's identity entirely out of
 * `velocity-plugin.json`; its `main` field is what names this class, and Guice
 * constructs it from there. Nothing at runtime reads the annotation below —
 * measured 2026-08-11 by scanning every class in the proxy jar for a reference
 * to `com/velocitypowered/api/plugin/Plugin`, which found exactly three, all
 * compile-time machinery: the annotation itself, `PluginAnnotationProcessor`
 * and `SerializedPluginDescription`. It has `RUNTIME` retention and no reader.
 *
 * It is kept anyway, for two reasons that are not "Velocity requires it": it
 * puts the plugin's identity in the source rather than only in a resource
 * file, and it is the input the annotation processor would consume if anyone
 * ever took the kapt route this project declined. See [version].
 */
@Plugin(
    id = "spawnery",
    name = "Spawnery Agent",
    // Never read by anything. The descriptor in velocity-plugin.json carries
    // the real version, expanded from -PagentVersion by processResources, and
    // that is the one Velocity reports and the agent sends as Hello.version.
    // A literal here rather than a build-time constant, because an annotation
    // argument must be a compile-time constant.
    //
    // The trap, for whoever adds kapt: Velocity's annotation processor
    // generates velocity-plugin.json to this same path from these same
    // arguments. Adding kapt *alongside* the hand-written resource would ship
    // a descriptor carrying this literal 0.0.0, no description and no authors
    // -- and it would not fail anything. Both descriptor guards in
    // hack/agent-jar-check.sh still pass: a "version": key is present, and no
    // ${ placeholder remains to be caught. The operator would simply record
    // every proxy in the fleet as running version 0.0.0. Taking that route
    // means deleting src/main/resources/velocity-plugin.json and making this
    // argument the real version, never doing one without the other.
    version = "0.0.0",
)
class AgentPlugin @Inject constructor(
    private val proxy: ProxyServer,
    private val logger: Logger,
) {
    private var gate: ReadyGate? = null

    /** What the plugin API reads from. See [MirrorApi]. */
    private val mirror = NetworkMirror()

    /**
     * Everything below is null while the agent is dormant, and that is what
     * makes the player events inert in that state rather than merely
     * harmless.
     *
     * The brief for this task said to register those events with
     * `proxy.eventManager.register(this, this)`. That call throws:
     * `VelocityEventManager.register` compares its two arguments and answers
     * `IllegalArgumentException("The plugin main instance is automatically
     * registered.")` when they are the same object. Measured 2026-08-11
     * against velocity 3.5.1 build 615 by disassembling
     * `com.velocitypowered.proxy.event.VelocityEventManager.register` and
     * `com.velocitypowered.proxy.VelocityServer`, which calls
     * `registerInternally` for every loaded plugin's own instance -- the same
     * mechanism that already delivers [onInitialize] here with no registration
     * anywhere. So a `@Subscribe` method on this class is registered from
     * plugin load, before [onInitialize] runs and whatever it decides, and a
     * dormant agent cannot decline to receive these events. It can only
     * decline to do anything with them, which is what the null checks below
     * are.
     */
    private var loop: SessionLoop<ProxyMessage, OperatorToProxy>? = null
    private var router: Router? = null
    private var rescue: Rescue? = null
    private var drain: Drain? = null
    private var fallbackGroups: List<String> = emptyList()
    private var sampling: ScheduledTask? = null
    private var scheduler: ScheduledExecutorService? = null

    @Subscribe
    fun onInitialize(event: ProxyInitializeEvent) {
        when (val env = ProxyEnvironment.from(System::getenv, Path.of(AGENT_DIR))) {
            is ProxyEnvironment.Dormant -> {
                logger.info("spawnery agent dormant: ${env.reason}")
                return
            }

            is ProxyEnvironment.Configured -> start(env)
        }
    }

    /**
     * Builds the agent and connects it. Split out of [onInitialize] only
     * because the `when` above reads better without twenty lines inside one of
     * its branches; nothing here is conditional.
     */
    private fun start(env: ProxyEnvironment.Configured) {
        // Constructed, not opened: a proxy is not ready until it has a server
        // list, and the gate is what makes this pod ready.
        //
        // Two of the callbacks below open it, not one. ProxyRole opens it on
        // the first FullSync it applies without throwing, and a SetReady(true)
        // opens it directly -- which is how a cancelled drain puts a proxy
        // back into the Service's endpoints without waiting for the next sync.
        // Both are conditional on there being a server list: ProxyRole holds
        // back a SetReady(true) that arrives before its first successful sync,
        // records it, and lets that sync open the gate instead. So neither of
        // these two lambdas can be reached with an empty routing table, and
        // the rule sits in ProxyRole rather than here because it needs the
        // latch to state it. Anyone adding a third caller owns that argument.
        //
        // A dormant agent never gets here at all, which is the claim
        // hack/velocity-image-test.sh makes by probing 8081 and requiring a
        // refusal.
        // onHopeless is Velocity's own shutdown, and it fires only on a first
        // bind that fails -- see ReadyGate.open. Such a pod has never been an
        // endpoint of its group's Service, so nobody is on it and nobody can
        // be; what stopping buys is a restart and then a CrashLoopBackOff,
        // which the operator reports on the group. Carrying on buys a pod
        // stuck in Pending with the reason in a container log and nothing
        // anywhere else.
        val gate = ReadyGate(
            READY_PORT,
            onHopeless = {
                logger.error(
                    "spawnery: this proxy cannot serve its readiness probe and never will; stopping " +
                        "so the operator sees a restart rather than a pod that is silently never ready",
                )
                proxy.shutdown()
            },
            log = ::warn,
        )
        this.gate = gate

        val directory = ServerDirectory(VelocityRegistry(proxy), ::warn)
        val players = VelocityPlayers(proxy)
        val router = Router(directory)
        this.router = router
        this.rescue = Rescue(router, ::warn)
        this.fallbackGroups = env.fallbackGroups
        val state = ProxyState(env.playerLimit)

        // Installed before the loop starts, and after the mirror exists, for
        // the reason the Paper plugin gives at the same point: a plugin
        // enabling between those two would hold an API whose mirror is empty
        // with no way to know it is about to fill.
        //
        // Every value comes from what the operator already puts on the pod --
        // no second reader of the same variables, and nothing guessed.
        // Hoisted for the reason the Paper plugin gives at the same point.
        val self = object : ProxySelf {
            override fun name(): String = System.getenv("SPAWNERY_PROXY") ?: ""
            override fun group(): String = System.getenv("SPAWNERY_GROUP") ?: ""
            override fun network(): String = System.getenv("SPAWNERY_NETWORK") ?: ""
        }
        Spawnery.install(MirrorApi(mirror, self))
        logger.info(
            "spawnery API installed for network {} group {}",
            self.network(),
            self.group(),
        )
        val drain = Drain(players, router, ::warn)
        this.drain = drain
        val role = ProxyRole(
            state = state,
            directory = directory,
            drain = drain,
            players = players,
            // The effective value, not the file's: getConfiguration() is what
            // Velocity parsed, after the image's own velocity.toml, after any
            // configOverlay the user mounted, after Velocity's defaults. That
            // is the whole reason this is read here rather than by the
            // operator, which can see none of those.
            readTimeoutMillis = proxy.configuration.readTimeout,
            onFirstSync = gate::open,
            onSetReady = { ready -> if (ready) gate.open() else gate.close() },
            log = ::warn,
            mirror = mirror,
        )

        val scheduler = Executors.newSingleThreadScheduledExecutor { runnable ->
            Thread(runnable, "spawnery-agent").apply { isDaemon = true }
        }
        this.scheduler = scheduler

        // Sampled on Velocity's own scheduler, and read from the reporting
        // timer as nothing but an atomic. Velocity's API is widely held to be
        // thread-safe where Bukkit's is not, so calling proxy.playerCount
        // straight from the gRPC side would very probably be fine -- and
        // "probably fine, by reputation" is precisely the kind of claim this
        // milestone has already been caught out by twice. The hop costs
        // nothing and removes the question.
        //
        // Through the Players seam rather than proxy.playerCount directly, so
        // the sampling path is the one Router and Drain are tested against.
        sampling = proxy.scheduler
            .buildTask(this, Runnable { state.sample(players.count()) })
            .repeat(SAMPLE_SECONDS, TimeUnit.SECONDS)
            .schedule()

        // Once here, before the first stream exists, because the timer above
        // does not fire until the scheduler picks it up and this method has to
        // return first. The operator's ReportInterval schedules its first
        // report at delay zero, so without this the first PlayerCount of the
        // process carries the zero the counter was constructed with. Paper's
        // AgentPlugin carries the same call for the same reason.
        state.sample(players.count())

        val session = SessionLoop(
            // The bundle is read here, per attempt, rather than once at
            // startup: see Environment.Configured. The kubelet replaces both
            // files in place, and a channel is built per attempt anyway, so
            // this costs one file read on a path that is already opening a TCP
            // connection.
            channels = {
                OperatorChannel.build(env.base.endpoint, Files.readAllBytes(env.base.caBundlePath))
            },
            credentials = BearerCredentials.of(TokenSource(env.base.tokenPath)),
            role = role,
            scheduler = scheduler,
            version = version(),
            // SessionLoop never gives up and never escalates a broken stream
            // on its own -- this callback is the only place that decides how
            // loud a permanently unreachable operator gets to be. See Paper's
            // AgentPlugin for the argument; it applies here with one addition.
            // A proxy that never reaches the operator never receives a
            // FullSync, so its gate never opens and its pod never turns ready:
            // the failure is already visible in `kubectl get pods`, and these
            // lines are what say why.
            log = ::warn,
        )
        loop = session
        session.start()

        logger.info("spawnery agent connecting to ${env.base.endpoint}")
    }

    @Subscribe
    fun onShutdown(event: ProxyShutdownEvent) {
        // First: install refuses a second implementation, so a proxy that
        // enabled twice without this would throw on the second.
        Spawnery.uninstall()
        loop?.stop()
        gate?.close()
        sampling?.cancel()
        scheduler?.let {
            it.shutdownNow()
            it.awaitTermination(2, TimeUnit.SECONDS)
        }
    }

    /**
     * Picks the server a joining player lands on.
     *
     * A null choice sets nothing, so Velocity disconnects the player with its
     * own "no available server" message rather than this plugin inventing one.
     * That is the honest outcome: there is genuinely nowhere to send them, and
     * the log line here is what names the groups that were searched -- without
     * it the only evidence would be Velocity's message, which names nothing.
     */
    @Subscribe
    fun onChooseInitialServer(event: PlayerChooseInitialServerEvent) {
        val target = router?.choose(fallbackGroups) ?: run {
            if (router != null) {
                logger.warn(
                    "spawnery: no server available in $fallbackGroups for " +
                        "'${event.player.username}'; letting the proxy refuse the connection",
                )
            }
            return
        }
        event.setInitialServer(target)
    }

    /**
     * Moves a player whose server dropped them, rather than letting the proxy
     * disconnect them.
     *
     * The decision is [Rescue]'s -- including when *not* to decide, which is
     * what a null means here: Velocity's own result stands, and that is
     * deliberately the outcome both when the player still has a working
     * server and when there is genuinely nowhere left to send them. See
     * [Rescue] for which backend failures reach this at all; one of them does
     * not.
     */
    @Subscribe
    fun onKickedFromServer(event: KickedFromServerEvent) {
        val target = rescue?.target(
            player = event.player.uniqueId,
            from = event.server.serverInfo.name,
            stillConnectedElsewhere = event.kickedDuringServerConnect(),
            toGroups = fallbackGroups,
        ) ?: return
        event.result = KickedFromServerEvent.RedirectPlayer.create(target)
    }

    /**
     * Ends a player's rescue chain when they leave.
     *
     * Without this the map in [Rescue] would be the one structure in this
     * plugin that only ever grows: a proxy that has rescued somebody keeps
     * their entry for as long as the process lives.
     */
    @Subscribe
    fun onDisconnect(event: DisconnectEvent) {
        rescue?.forget(event.player.uniqueId)
    }

    /**
     * Tells the operator where a player ended up, and ends their rescue chain.
     *
     * The report is accepted and ignored by the operator today -- player
     * counts come from the servers themselves -- and on the wire for
     * project 4's dashboard. [SessionLoop.send] drops it silently when there
     * is no stream, which is right: this is a notification about a moment, and
     * a moment that has passed by the time a reconnect completes is not worth
     * replaying.
     *
     * The [Rescue.forget] is not a detail of the report. Arriving anywhere is
     * what makes an earlier bounce history rather than an ongoing incident,
     * and this is the only event that says a player arrived.
     */
    /**
     * Moves a player who has just landed on a server the operator is draining.
     *
     * This is the drain's late half, and [Drain] carries the measurement it
     * rests on. The short version: a player whose connection was already in
     * flight when the drain began is counted by neither the backend nor the
     * proxy, so `DrainPlayers` arrives and moves everyone except them, and the
     * operator then reads an empty server and deletes the pod under them.
     *
     * `ServerPostConnectEvent` and not `ServerConnectedEvent`, although the
     * agent already subscribes to the latter and it fires sooner. Velocity
     * fires both from `TransitionSessionHandler`, but the connected one is
     * fired mid-transition -- there is a second `setConnectedServer` after it
     * -- and issuing a fresh connection request into that is a race against
     * the switch still under way. The post event is on the far side of it,
     * and the few milliseconds bought by the earlier one are not worth
     * racing Velocity for.
     */
    @Subscribe
    fun onServerPostConnect(event: ServerPostConnectEvent) {
        drain?.landed(VelocityPlayer(event.player))
    }

    @Subscribe
    fun onServerConnected(event: ServerConnectedEvent) {
        rescue?.forget(event.player.uniqueId)
        loop?.send(
            ProxyMessage.newBuilder()
                .setPlayerJoinedServer(
                    PlayerJoinedServer.newBuilder()
                        .setPlayer(event.player.username)
                        .setServer(event.server.serverInfo.name),
                )
                .build(),
        )
    }

    /**
     * The one log sink handed to every unit that takes one. They all take a
     * callback rather than a logger because the only logger this plugin has is
     * the one Velocity injects here, and taking it directly would make each of
     * them untestable without a proxy.
     */
    private fun warn(message: String, error: Throwable?) {
        logger.warn(message, error)
    }

    /**
     * What the agent reports as `Hello.version`: the version Velocity read out
     * of `velocity-plugin.json`, which `processResources` expanded from
     * `-PagentVersion`. Not the `@Plugin` annotation's literal, which is never
     * read by anything -- see [version] on that annotation for why reading it
     * would be worse than useless.
     *
     * `fromInstance` cannot fail here: Velocity only calls into a plugin
     * through a container it already holds. The fallback is a string rather
     * than a throw because a version this agent cannot name is not a reason to
     * leave the proxy unmanaged -- the operator only logs it.
     */
    private fun version(): String =
        proxy.pluginManager.fromInstance(this)
            .flatMap { it.description.version }
            .orElse("unknown")

    private companion object {
        // internal/podspec.AgentMountPath. Hard-coded rather than configurable:
        // the operator creates these pods and mounts exactly here, and a second
        // place to spell it would be a second place to get it wrong.
        const val AGENT_DIR = "/var/run/spawnery"

        // internal/podspec.ProxyReadyPort, for the same reason.
        const val READY_PORT = 8081

        // Matching Paper's 20-tick sampling period. Fast enough that the
        // reported count is never stale by more than the report interval's own
        // resolution, cheap enough to be invisible next to anything else the
        // scheduler runs.
        const val SAMPLE_SECONDS = 1L
    }
}
