package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.CloudConnector
import cloud.spawnery.agent.CloudEvents
import cloud.spawnery.agent.ProxyCommands
import cloud.spawnery.agent.cloudCommand
import cloud.spawnery.agent.Feed
import cloud.spawnery.agent.FeedState
import cloud.spawnery.agent.JoinRules
import cloud.spawnery.agent.LuckPermsContexts
import cloud.spawnery.agent.LuckPermsPermissions
import cloud.spawnery.agent.MirrorApi
import cloud.spawnery.agent.TransferView
import cloud.spawnery.agent.Requests
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.api.ProxySelf
import cloud.spawnery.agent.api.Spawnery
import cloud.spawnery.agent.BearerCredentials
import cloud.spawnery.agent.OperatorChannel
import cloud.spawnery.agent.SessionLoop
import cloud.spawnery.agent.TokenSource
import cloud.spawnery.agent.pb.EventInterest
import cloud.spawnery.agent.pb.OperatorToProxy
import cloud.spawnery.agent.pb.PlayerJoinedServer
import cloud.spawnery.agent.pb.ProxyMessage
import com.google.inject.Inject
import com.velocitypowered.api.command.BrigadierCommand
import com.velocitypowered.api.command.CommandSource
import com.velocitypowered.api.event.EventTask
import com.velocitypowered.api.event.Subscribe
import com.velocitypowered.api.event.connection.DisconnectEvent
import com.velocitypowered.api.event.connection.LoginEvent
import com.velocitypowered.api.event.player.CookieReceiveEvent
import com.velocitypowered.api.event.player.KickedFromServerEvent
import com.velocitypowered.api.event.player.PlayerChooseInitialServerEvent
import com.velocitypowered.api.event.player.ServerConnectedEvent
import com.velocitypowered.api.event.player.ServerPostConnectEvent
import com.velocitypowered.api.event.player.ServerPreConnectEvent
import com.velocitypowered.api.event.proxy.ProxyInitializeEvent
import com.velocitypowered.api.event.proxy.ProxyShutdownEvent
import com.velocitypowered.api.network.HandshakeIntent
import com.velocitypowered.api.plugin.Plugin
import com.velocitypowered.api.proxy.Player
import com.velocitypowered.api.proxy.ProxyServer
import com.velocitypowered.proxy.VelocityServer
import com.velocitypowered.api.scheduler.ScheduledTask
import net.kyori.adventure.text.Component
import org.slf4j.Logger
import java.nio.file.Files
import java.nio.file.Path
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.UUID
import java.util.concurrent.TimeUnit

/**
 * The only class in this plugin that touches the Velocity API, so every other
 * unit can be tested with JUnit and no proxy. The decisions live in
 * [ProxyEnvironment], [ReadyGate], [ServerDirectory], [Router], [Drain],
 * [ProxyRole] and `SessionLoop`; this class only wires them to Velocity's
 * events and scheduler.
 *
 * Velocity reads this plugin's identity out of `velocity-plugin.json`; nothing
 * at runtime reads the annotation below.
 */
@Plugin(
    id = "spawnery",
    name = "Spawnery Agent",
    // Never read. The real version is in velocity-plugin.json. Adding kapt
    // would generate that file from these arguments and silently ship 0.0.0;
    // that route means deleting the resource and making this the real version.
    version = "0.0.0",
)
class AgentPlugin @Inject constructor(
    private val proxy: ProxyServer,
    private val logger: Logger,
) {
    private var gate: ReadyGate? = null

    private val mirror = NetworkMirror()

    private val connector = CloudConnector(
        Requests(timeoutMillis = CloudConnector.TIMEOUT_MILLIS, clock = System::currentTimeMillis),
    ) { request ->
        val loop = this.loop
            ?: throw IllegalStateException("this agent has no session to the operator")
        loop.send(ProxyMessage.newBuilder().setCloudRequest(request).build())
    }

    /**
     * Null while the agent is dormant. Velocity registers this instance's
     * `@Subscribe` methods at plugin load (and `register(this, this)` throws),
     * so a dormant agent still receives the player events and can only ignore
     * them.
     */
    private var loop: SessionLoop<ProxyMessage, OperatorToProxy>? = null
    private var router: Router? = null
    private var directory: ServerDirectory? = null
    private var joinAccess: JoinAccess = JoinAccess.OPEN
    private var rescue: Rescue? = null
    private var drain: Drain? = null
    private var transfers: Transfers? = null
    private var transferPass: ScheduledTask? = null

    /** Built here rather than in start(), because it outlives a reconnect. */
    private val feedState = FeedState()
    private var feed: Feed? = null

    /**
     * A plugin's own listeners. Built here rather than in start(), so a plugin
     * that subscribed before this agent connected keeps its subscription.
     */
    private val events = CloudEvents()

    /**
     * The last EventInterest this agent sent, or null on a stream it has not
     * reported on: the operator forgets interest across a renewal.
     */
    private var lastInterest: Boolean? = null

    /** Sent only when the answer changes. */
    private fun reportInterest(wanted: Boolean) {
        if (lastInterest == wanted) return
        val loop = this.loop ?: return
        lastInterest = wanted
        loop.send(
            ProxyMessage.newBuilder()
                .setEventInterest(EventInterest.newBuilder().setWanted(wanted))
                .build(),
        )
    }
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

    private fun start(env: ProxyEnvironment.Configured) {
        // Not ready until it has a server list. ProxyRole opens the gate on the
        // first FullSync and holds back a SetReady(true) that arrives before it,
        // so neither callback can open it with an empty routing table.
        //
        // onHopeless fires only on a first bind that fails (see ReadyGate.open):
        // stopping turns a never-ready pod into a visible CrashLoopBackOff.
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
        this.directory = directory
        this.router = router
        this.fallbackGroups = env.fallbackGroups
        val state = ProxyState(env.playerLimit)

        // Installed before the loop starts and after the mirror exists, so no
        // plugin holds an API whose mirror is empty with no way to know it fills.
        val self = object : ProxySelf {
            override fun name(): String = System.getenv("SPAWNERY_PROXY") ?: ""
            override fun group(): String = System.getenv("SPAWNERY_GROUP") ?: ""
            override fun network(): String = System.getenv("SPAWNERY_NETWORK") ?: ""
        }
        val access = JoinPermissions(
            rules = mirror::joinRule,
            network = self::network,
            log = ::warn,
            lookups = listOfNotNull(
                LuckPermsPermissions.lookupIfPresent()?.let { f -> PermissionLookup { p, n, c -> f(p, n, c) } },
                VelocityPermissionLookup(proxy),
            ),
        )
        this.joinAccess = access
        this.rescue = Rescue(router, ::warn, access)
        val feed = Feed(VelocityAudience(proxy), feedState, System::currentTimeMillis, format = mirror::feedFormat)
        this.feed = feed
        // No ReadinessGate: a proxy has no readiness flag to hold. See ProxyState.
        val api = MirrorApi(
            mirror, self, connector, events,
            transfers = object : TransferView {
                override fun arrived(player: UUID) = transfers?.arrivedByTransfer(player) ?: false
                override fun leaving(player: UUID) = transfers?.leavingByTransfer(player) ?: false
            },
        )
        Spawnery.install(api)
        LuckPermsContexts.registerIfPresent(self, logger::info)
        // Not the deprecated one-argument register(), which files the command
        // under no plugin.
        val proxyCommands = ProxyCommands<CommandSource>(connector) { source -> (source as? Player)?.username ?: "console" }
        val command = BrigadierCommand(
            cloudCommand(api, VelocitySource, feedState, mirror::feedFormat, proxyCommands).build(),
        )
        proxy.commandManager.register(
            proxy.commandManager.metaBuilder(command).plugin(this).build(),
            command,
        )
        logger.info(
            "spawnery API installed for network {} group {}",
            self.network(),
            self.group(),
        )
        startTransfers(env, self)
        val drain = Drain(players, router, ::warn, access)
        this.drain = drain
        val role = ProxyRole(
            state = state,
            directory = directory,
            drain = drain,
            players = players,
            // The effective value Velocity parsed, which the operator cannot see.
            readTimeoutMillis = proxy.configuration.readTimeout,
            onFirstSync = gate::open,
            onSetReady = { ready -> if (ready) gate.open() else gate.close() },
            log = ::warn,
            mirror = mirror,
            connector = connector,
            feed = feed,
            events = events,
        )

        val scheduler = Executors.newSingleThreadScheduledExecutor { runnable ->
            Thread(runnable, "spawnery-agent").apply { isDaemon = true }
        }
        this.scheduler = scheduler

        // Sampled on Velocity's scheduler and read from the reporting timer as
        // an atomic, so the gRPC side never calls the Velocity API.
        sampling = proxy.scheduler
            .buildTask(
                this,
                Runnable {
                    state.sample(players.count())
                    this.feed?.let {
                        it.tick()
                        reportInterest(it.wanted(events.size()))
                    }
                },
            )
            .repeat(SAMPLE_SECONDS, TimeUnit.SECONDS)
            .schedule()

        // Once here, before the first stream: the operator's first report comes
        // at delay zero, before the timer above has fired.
        state.sample(players.count())

        val session = SessionLoop(
            // Read per attempt: the kubelet replaces the bundle in place.
            channels = {
                OperatorChannel.build(env.base.endpoint, Files.readAllBytes(env.base.caBundlePath))
            },
            credentials = BearerCredentials.of(TokenSource(env.base.tokenPath)),
            role = role,
            scheduler = scheduler,
            version = version(),
            // The only place that decides how loud an unreachable operator gets.
            // A proxy that never syncs never turns ready, so these lines say why.
            log = ::warn,
            // The operator retires displaced streams on a schedule; not a warning.
            note = logger::info,
            // The connector fails what is in flight rather than resending it.
            onStreamChanged = {
                connector.onStreamChanged()
                lastInterest = null
            },
        )
        loop = session
        session.start()

        logger.info("spawnery agent connecting to ${env.base.endpoint}")
    }

    private fun startTransfers(env: ProxyEnvironment.Configured, self: ProxySelf) {
        env.transferOff?.let { logger.warn("spawnery: transfers off, this proxy drains without them: $it") }
        val transfer = env.transfer ?: return
        val transfers = Transfers(
            cookie = TransferCookie(transfer.secret) { System.currentTimeMillis() / 1000 },
            policy = TransferPolicy(transfer.forceAfterSeconds * 1000, System::currentTimeMillis),
            picture = { TransferPolicy.Picture(self.name(), self.group(), mirror.proxies(), mirror.closedDoors(), mirror.acceptingTransfers()) },
            registered = { name ->
                proxy.getServer(name).isPresent && mirror.servers().any { it.name() == name && it.registered() }
            },
            info = logger::info,
            warn = ::warn,
            spacingMillis = loginRatelimitMillis().let { if (it > 0) it + TRANSFER_SPACING_MARGIN_MILLIS else 0 },
        )
        this.transfers = transfers
        transferPass = proxy.scheduler
            .buildTask(this, Runnable { transfers.pass(proxy.allPlayers.map { VelocityTraveller(it, ::warn) }) })
            .repeat(TRANSFER_PASS_SECONDS, TimeUnit.SECONDS)
            .schedule()
    }

    @Subscribe
    fun onShutdown(event: ProxyShutdownEvent) {
        // First: install refuses a second implementation.
        Spawnery.uninstall()
        loop?.stop()
        gate?.close()
        sampling?.cancel()
        transferPass?.cancel()
        scheduler?.let {
            it.shutdownNow()
            it.awaitTermination(2, TimeUnit.SECONDS)
        }
    }

    /**
     * Picks the server a joining player lands on. A null choice sets nothing,
     * so Velocity disconnects the player with its own "no available server"
     * message; the log line names the groups that were searched.
     */
    @Subscribe
    fun onChooseInitialServer(event: PlayerChooseInitialServerEvent) {
        val player = event.player.uniqueId
        val mayJoin = { server: String, group: String -> joinAccess.mayJoin(player, server, group) }
        val arrival = transfers?.landing(player, event.player.username)
            ?.let { proxy.getServer(it).orElse(null) }
            ?.takeIf { server ->
                val group = directory?.groupOf(server.serverInfo.name)
                group == null || mayJoin(server.serverInfo.name, group)
            }
        val target = arrival ?: router?.choose(fallbackGroups, mayJoin = mayJoin) ?: run {
            if (router != null) {
                logger.warn(
                    "spawnery: no server '${event.player.username}' may join in $fallbackGroups " +
                        "(empty, or closed to them by a join permission); letting the proxy refuse the connection",
                )
            }
            return
        }
        event.setInitialServer(target)
    }

    /**
     * Moves a player whose server dropped them, rather than letting the proxy
     * disconnect them. A null from [Rescue] leaves Velocity's own result.
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

    @Subscribe
    fun onDisconnect(event: DisconnectEvent) {
        rescue?.forget(event.player.uniqueId)
    }

    /** Last, so every other plugin's listener can still ask leavingByTransfer. */
    @Subscribe(priority = Short.MIN_VALUE)
    fun onDisconnectSettled(event: DisconnectEvent) {
        transfers?.forget(event.player.uniqueId)
    }

    private fun loginRatelimitMillis(): Long =
        try {
            (proxy as? VelocityServer)?.configuration?.loginRatelimit?.toLong() ?: DEFAULT_LOGIN_RATELIMIT_MILLIS
        } catch (e: LinkageError) {
            DEFAULT_LOGIN_RATELIMIT_MILLIS
        }

    /**
     * LoginEvent, the earliest event with a player to ask: Velocity takes a
     * cookie answer in the login phase and holds the login on this event's
     * task, so PlayerChooseInitialServerEvent comes after the answer.
     */
    @Subscribe
    fun onLogin(event: LoginEvent): EventTask? {
        val transfers = transfers ?: return null
        val scheduler = scheduler ?: return null
        val player = event.player
        if (!event.result.isAllowed || player.handshakeIntent != HandshakeIntent.TRANSFER) return null
        return EventTask.withContinuation { continuation ->
            transfers.expecting(player.uniqueId, continuation::resume)
            scheduler.schedule(
                { transfers.gaveUp(player.uniqueId, player.username, "no answer within $COOKIE_WAIT_MILLIS ms") },
                COOKIE_WAIT_MILLIS,
                TimeUnit.MILLISECONDS,
            )
            try {
                player.requestCookie(TRANSFER_COOKIE_KEY)
            } catch (e: Exception) {
                warn("spawnery: could not ask '${player.username}' for a transfer cookie", e)
                transfers.gaveUp(player.uniqueId, player.username, "not asked")
            }
        }
    }

    @Subscribe
    fun onCookieReceive(event: CookieReceiveEvent) {
        val transfers = transfers ?: return
        if (event.originalKey != TRANSFER_COOKIE_KEY) return
        if (transfers.received(event.player.uniqueId, event.player.username, event.originalData)) {
            event.result = CookieReceiveEvent.ForwardResult.handled()
        }
    }

    @Subscribe
    fun onServerPreConnectJoinRule(event: ServerPreConnectEvent) {
        if (!event.result.isAllowed) return
        val target = event.result.server.orElse(event.originalServer)
        val name = target.serverInfo.name
        val group = directory?.groupOf(name) ?: return
        val player = event.player.uniqueId
        val mayJoin = { server: String, g: String -> joinAccess.mayJoin(player, server, g) }
        when (
            val decision = decidePreConnect(
                allowed = mayJoin(name, group),
                onServer = event.player.currentServer.isPresent,
                alternative = { router?.choose(fallbackGroups, excluding = setOf(name), mayJoin = mayJoin) },
            )
        ) {
            PreConnectDecision.Keep -> return
            PreConnectDecision.Deny -> {
                event.result = ServerPreConnectEvent.ServerResult.denied()
                event.player.sendMessage(joinDenied(group))
            }
            is PreConnectDecision.Redirect -> {
                event.result = ServerPreConnectEvent.ServerResult.allowed(decision.server)
            }
            PreConnectDecision.Disconnect -> {
                event.result = ServerPreConnectEvent.ServerResult.denied()
                event.player.disconnect(joinDenied(group))
            }
        }
    }

    private fun joinDenied(group: String): Component {
        val shown = mirror.groups().firstOrNull { it.name() == group }?.displayName()?.takeIf { it.isNotBlank() } ?: group
        return Component.translatable()
            .key(JoinRules.DENIED_KEY)
            .fallback(JoinRules.DENIED_FALLBACK)
            .arguments(Component.text(shown))
            .build()
    }

    @Subscribe(priority = Short.MIN_VALUE)
    fun onServerPreConnect(event: ServerPreConnectEvent) {
        val transfers = transfers ?: return
        if (!event.result.isAllowed || event.player.currentServer.isEmpty) return
        val target = event.result.server.orElse(event.originalServer)
        if (transfers.onSwitch(VelocityTraveller(event.player, ::warn), target.serverInfo.name)) {
            event.result = ServerPreConnectEvent.ServerResult.denied()
        }
    }

    /**
     * Moves a player who has just landed on a server the operator is draining:
     * the drain's late half, see [Drain].
     *
     * `ServerPostConnectEvent` and not `ServerConnectedEvent`: the latter fires
     * mid-transition, and a fresh connection request then races the switch.
     */
    @Subscribe
    fun onServerPostConnect(event: ServerPostConnectEvent) {
        drain?.landed(VelocityPlayer(event.player))
    }

    /** The operator accepts and ignores this report today. */
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
     * The log sink handed to every unit, as a callback so none of them needs
     * Velocity's injected logger.
     */
    private fun warn(message: String, error: Throwable?) {
        logger.warn(message, error)
    }

    /**
     * The version Velocity read out of `velocity-plugin.json`, not the
     * `@Plugin` literal.
     */
    private fun version(): String =
        proxy.pluginManager.fromInstance(this)
            .flatMap { it.description.version }
            .orElse("unknown")

    private companion object {
        // internal/podspec.AgentMountPath.
        const val AGENT_DIR = "/var/run/spawnery"

        // internal/podspec.ProxyReadyPort.
        const val READY_PORT = 8081

        // Matching Paper's 20-tick sampling period.
        const val SAMPLE_SECONDS = 1L

        const val TRANSFER_PASS_SECONDS = 1L
        const val COOKIE_WAIT_MILLIS = 2_000L

        /** Velocity's own default for login-ratelimit. */
        const val DEFAULT_LOGIN_RATELIMIT_MILLIS = 3_000L
        const val TRANSFER_SPACING_MARGIN_MILLIS = 500L
    }
}
