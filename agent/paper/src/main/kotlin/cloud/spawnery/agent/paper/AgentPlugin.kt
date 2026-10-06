package cloud.spawnery.agent.paper

import cloud.spawnery.agent.CloudConnector
import cloud.spawnery.agent.CloudEvents
import cloud.spawnery.agent.cloudCommand
import cloud.spawnery.agent.MirrorApi
import cloud.spawnery.agent.ReadinessGate
import cloud.spawnery.agent.Requests
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.api.ServerSelf
import cloud.spawnery.agent.api.Spawnery
import cloud.spawnery.agent.BearerCredentials
import cloud.spawnery.agent.Environment
import cloud.spawnery.agent.Feed
import cloud.spawnery.agent.FeedLevels
import cloud.spawnery.agent.LuckPermsContexts
import cloud.spawnery.agent.LuckPermsFeedLevels
import cloud.spawnery.agent.OperatorChannel
import cloud.spawnery.agent.SessionLoop
import cloud.spawnery.agent.TokenSource
import cloud.spawnery.agent.pb.CloudRequest
import cloud.spawnery.agent.pb.EventInterest
import cloud.spawnery.agent.pb.ExecuteCommand
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.ServerMessage
import io.papermc.paper.plugin.lifecycle.event.types.LifecycleEvents
import org.bukkit.Bukkit
import org.bukkit.event.EventHandler
import org.bukkit.event.EventPriority
import org.bukkit.event.Listener
import org.bukkit.event.server.ServerLoadEvent
import org.bukkit.plugin.java.JavaPlugin
import java.nio.file.Files
import java.nio.file.Path
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit
import java.util.logging.Level

/**
 * The only class in this plugin that touches Bukkit, so every other unit can
 * be tested without a server.
 */
class AgentPlugin : JavaPlugin(), Listener {
    private val state = ServerState()

    private val readiness = ReadinessGate {
        if (state.markReady()) {
            loop?.send(role.ready())
        }
    }
    private val mirror = NetworkMirror()

    private var loginGate: LoginGateListener? = null

    private val connector = CloudConnector(
        Requests(timeoutMillis = CloudConnector.TIMEOUT_MILLIS, clock = System::currentTimeMillis),
    ) { request ->
        val loop = this.loop
            ?: throw IllegalStateException("this agent has no session to the operator")
        loop.send(ServerMessage.newBuilder().setCloudRequest(request).build())
    }
    private val feedLevels = FeedLevels(LuckPermsFeedLevels.storeIfPresent())
    private val feed = Feed(PaperAudience, feedLevels, System::currentTimeMillis, format = mirror::feedFormat)
    private val events = CloudEvents()
    private val role = ServerRole(state, mirror, connector, feed, events, ::execute)

    private fun execute(command: ExecuteCommand) {
        server.scheduler.runTask(this, Runnable {
            val outcome = runCommand(command) { line, feedback ->
                server.dispatchCommand(server.createCommandSender { feedback(it) }, line)
            }
            loop?.send(ServerMessage.newBuilder().setExecuteOutcome(outcome).build())
        })
    }

    /**
     * Null on a new stream: the operator assumes "no" for a session it has
     * never seen, so a renewal must be told again even if nothing changed.
     */
    private var lastInterest: Boolean? = null
    private lateinit var scheduler: ScheduledExecutorService
    private var loop: SessionLoop<ServerMessage, OperatorToServer>? = null

    private val worldSync: WorldSync? =
        if (System.getenv(WorldSync.ENV_ENABLED) == "1") {
            WorldSync(Path.of(System.getProperty("user.dir"), WorldSync.CONTROL_DIR))
        } else {
            null
        }
    private var snapshotSeq = 0L
    private var snapshotInFlight = false

    private fun startSnapshots() {
        val sync = worldSync ?: return
        snapshotSeq = sync.nextSequence() - 1
        val millis = WorldSync.parseInterval(System.getenv(WorldSync.ENV_INTERVAL)) ?: DEFAULT_SNAPSHOT_MILLIS
        val ticks = millis / 50
        server.scheduler.runTaskTimer(this, Runnable { snapshot(sync) }, ticks, ticks)
    }

    /** Main thread: freeze saving, flush, ask; the wait runs off the main thread. */
    private fun snapshot(sync: WorldSync) {
        if (snapshotInFlight) return
        snapshotInFlight = true
        val autosave = server.worlds.associateWith { it.isAutoSave }
        val restore = Runnable {
            autosave.forEach { (world, on) -> world.isAutoSave = on }
            snapshotInFlight = false
        }
        val seq: Long
        try {
            autosave.keys.forEach { it.isAutoSave = false }
            server.dispatchCommand(server.consoleSender, "save-all flush")
            seq = ++snapshotSeq
            sync.requestSnapshot(seq)
        } catch (t: Throwable) {
            restore.run()
            logger.log(Level.WARNING, "spawnery world sync: could not ask for a snapshot", t)
            return
        }
        server.scheduler.runTaskAsynchronously(this, Runnable {
            val outcome: Any = try {
                sync.awaitSnapshot(seq, SNAPSHOT_WAIT_MILLIS)
            } catch (t: Throwable) {
                t
            }
            server.scheduler.runTask(this, Runnable {
                restore.run()
                if (outcome != WorldSync.Outcome.Ready) {
                    logger.warning("spawnery world sync: snapshot $seq: $outcome")
                }
            })
        })
    }

    override fun onEnable() {
        when (val env = Environment.from(System::getenv, Path.of(AGENT_DIR))) {
            is Environment.Dormant -> {
                logger.info("spawnery agent dormant: ${env.reason}")
                startSnapshots()
                return
            }

            is Environment.Configured -> {
                // Installed after the mirror exists and before the loop starts,
                // so no plugin holds an API whose mirror fills unannounced.
                val self = object : ServerSelf {
                    override fun name(): String = System.getenv("SPAWNERY_SERVER") ?: ""
                    override fun group(): String = System.getenv("SPAWNERY_GROUP") ?: ""
                    override fun network(): String = System.getenv("SPAWNERY_NETWORK") ?: ""
                    override fun slots(): Int = state.slots
                }
                val api = MirrorApi(mirror, self, connector, events, readiness, state::setPlayable)
                Spawnery.install(api)
                // Paper accepts a Brigadier node only inside the COMMANDS
                // lifecycle event; a direct call from onEnable throws.
                lifecycleManager.registerEventHandler(LifecycleEvents.COMMANDS) { event ->
                    event.registrar().register(
                        cloudCommand(api, PaperSource, feedLevels, mirror::feedFormat).build(),
                        "Spawnery cloud commands",
                    )
                }
                // The only outward sign that the install ran; it leaves no
                // trace on the wire.
                logger.info(
                    "spawnery API installed for network ${self.network()} group ${self.group()}",
                )
                LuckPermsContexts.registerIfPresent(self, logger::info)

                scheduler = Executors.newSingleThreadScheduledExecutor { runnable ->
                    Thread(runnable, "spawnery-agent").apply { isDaemon = true }
                }

                val session = SessionLoop(
                    // Read per attempt rather than once: see Environment.Configured.
                    channels = {
                        OperatorChannel.build(env.endpoint, Files.readAllBytes(env.caBundlePath))
                    },
                    credentials = BearerCredentials.of(TokenSource(env.tokenPath)),
                    role = role,
                    scheduler = scheduler,
                    version = pluginMeta.version,
                    // SessionLoop retries forever and never escalates; WARNING keeps
                    // an unreachable operator visible without calling the server
                    // itself unhealthy.
                    log = { message, error -> logger.log(Level.WARNING, message, error) },
                    // The operator retires a displaced stream on its own
                    // schedule, so a renewal is routine.
                    note = { message -> logger.log(Level.INFO, message) },
                    // The connector fails what is in flight rather than resending,
                    // since only the caller knows whether a repeat is safe; the
                    // interest is forgotten because the operator assumes "no" for
                    // a new session.
                    onStreamChanged = {
                        connector.onStreamChanged()
                        lastInterest = null
                    },
                )
                loop = session

                server.pluginManager.registerEvents(this, this)

                // Bukkit.getOnlinePlayers() is not thread-safe, so the count is
                // sampled on the main thread.
                server.scheduler.runTaskTimer(this, Runnable {
                    state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())
                    registerLoginGateOnce()
                    // Paper's own one-minute average; it caps at 20.
                    state.sampleTicks(Bukkit.getTPS()[0], Bukkit.getAverageTickTime())
                    // Recomputed every tick rather than subscribing to joins,
                    // leaves and permission changes separately.
                    feed.tick()
                    reportInterest(feed.wanted(events.size()))
                }, 0L, SAMPLE_TICKS)

                // The timer's first run comes after onEnable returns, and the
                // operator asks for a report at once; unsampled, a fresh server
                // would announce zero free slots.
                state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())

                session.start()
                logger.info("spawnery agent connecting to ${env.endpoint}")
                startSnapshots()
            }
        }
    }

    /** Sent only when the answer changes: EventInterest is a state. */
    private fun reportInterest(wanted: Boolean) {
        if (lastInterest == wanted) return
        val loop = this.loop ?: return
        lastInterest = wanted
        loop.send(
            ServerMessage.newBuilder()
                .setEventInterest(EventInterest.newBuilder().setWanted(wanted))
                .build(),
        )
    }

    override fun onDisable() {
        // First: install refuses a second implementation, so a re-enable
        // would throw and take the agent down.
        Spawnery.uninstall()
        loop?.stop()
        if (::scheduler.isInitialized) {
            scheduler.shutdownNow()
            scheduler.awaitTermination(2, TimeUnit.SECONDS)
        }
    }

    private fun registerLoginGateOnce() {
        if (loginGate != null) return
        val group = System.getenv("SPAWNERY_GROUP") ?: return
        if (mirror.admission(group)?.enforce != true && mirror.joinRule(group) == null) return
        val gate = LoginGateListener(group, mirror, state, log = logger::info)
        server.pluginManager.registerEvents(gate, this)
        loginGate = gate
        logger.info("group $group enforces playable slots or a join permission; login check registered")
    }

    // MONITOR so every other plugin's ServerLoadEvent handler has run first.
    @EventHandler(priority = EventPriority.MONITOR)
    fun onServerLoad(event: ServerLoadEvent) {
        if (event.type != ServerLoadEvent.LoadType.STARTUP) return
        state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())
        // The operator never learns a hold's reason, so this log line is the
        // only place a plugin that never finishes is named.
        val waiting = readiness.openReasons()
        if (waiting.isNotEmpty()) {
            logger.info("not ready yet, waiting for: ${waiting.joinToString(", ")}")
        }
        readiness.serverLoaded()
    }

    private companion object {
        // internal/podspec.AgentMountPath.
        const val AGENT_DIR = "/var/run/spawnery"

        // One second at 20 ticks.
        const val SAMPLE_TICKS = 20L

        const val SNAPSHOT_WAIT_MILLIS = 60 * 1000L
        const val DEFAULT_SNAPSHOT_MILLIS = 5 * 60 * 1000L
    }
}
