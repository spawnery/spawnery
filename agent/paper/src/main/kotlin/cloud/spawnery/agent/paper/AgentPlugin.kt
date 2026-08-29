package cloud.spawnery.agent.paper

import cloud.spawnery.agent.CloudConnector
import cloud.spawnery.agent.cloudCommand
import cloud.spawnery.agent.MirrorApi
import cloud.spawnery.agent.Requests
import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.api.ServerSelf
import cloud.spawnery.agent.api.Spawnery
import cloud.spawnery.agent.BearerCredentials
import cloud.spawnery.agent.Environment
import cloud.spawnery.agent.Feed
import cloud.spawnery.agent.FeedState
import cloud.spawnery.agent.OperatorChannel
import cloud.spawnery.agent.SessionLoop
import cloud.spawnery.agent.TokenSource
import cloud.spawnery.agent.pb.CloudRequest
import cloud.spawnery.agent.pb.EventInterest
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.ServerMessage
import io.papermc.paper.plugin.lifecycle.event.types.LifecycleEvents
import org.bukkit.Bukkit
import org.bukkit.event.EventHandler
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
 * The only class in this plugin that touches Bukkit.
 *
 * That is not tidiness for its own sake: it is what lets every other unit be
 * tested with JUnit and no server. Nothing here decides anything — the
 * decisions live in Environment, ServerState and SessionLoop.
 */
class AgentPlugin : JavaPlugin(), Listener {
    private val state = ServerState()
    private val mirror = NetworkMirror()

    /**
     * How a plugin's connect call reaches the operator.
     *
     * The one lambda is the whole platform seam: it wraps the request in a
     * ServerMessage, which is the only thing about this that a Paper agent
     * knows and a Velocity one does not. It does not know which request it is
     * carrying, and that is why adding a verb touches no platform file.
     */
    private val connector = CloudConnector(
        Requests(timeoutMillis = CloudConnector.TIMEOUT_MILLIS, clock = System::currentTimeMillis),
    ) { request ->
        val loop = this.loop
            ?: throw IllegalStateException("this agent has no session to the operator")
        loop.send(ServerMessage.newBuilder().setCloudRequest(request).build())
    }
    private val feedState = FeedState()
    private val feed = Feed(PaperAudience, feedState, System::currentTimeMillis)
    private val role = ServerRole(state, mirror, connector, feed)

    /**
     * The last EventInterest this agent sent, or null on a stream it has not
     * reported on.
     *
     * Null and not false: the operator's answer for a session it has never
     * seen is "no" and it remembers nothing across a renewal, so a new stream
     * has to be told even when the answer has not moved. Without that a
     * renewal would leave the operator believing "no" while this agent
     * believed it had already said "yes", and the feed would go silent until
     * somebody's permissions happened to change.
     */
    private var lastInterest: Boolean? = null
    private lateinit var scheduler: ScheduledExecutorService
    private var loop: SessionLoop<ServerMessage, OperatorToServer>? = null

    override fun onEnable() {
        when (val env = Environment.from(System::getenv, Path.of(AGENT_DIR))) {
            is Environment.Dormant -> {
                logger.info("spawnery agent dormant: ${env.reason}")
                return
            }

            is Environment.Configured -> {
                // Installed before the loop starts, and after the mirror
                // exists. A plugin whose own enable ran between those two
                // points would hold an API whose mirror is empty with no way
                // to know it is about to fill.
                //
                // Every value comes from what the operator already puts on the
                // pod -- no second reader of the same variables, and nothing
                // guessed: a missing one is empty rather than derived from a
                // hostname.
                // Hoisted, so the line below names the same object the API
                // was built from rather than a second construction that could
                // drift from it.
                val self = object : ServerSelf {
                    override fun name(): String = System.getenv("SPAWNERY_SERVER") ?: ""
                    override fun group(): String = System.getenv("SPAWNERY_GROUP") ?: ""
                    override fun network(): String = System.getenv("SPAWNERY_NETWORK") ?: ""
                    override fun slots(): Int = state.slots
                }
                val api = MirrorApi(mirror, self, connector)
                Spawnery.install(api)
                // Registered inside the COMMANDS lifecycle event because that
                // is the only window Paper accepts a Brigadier node in; a
                // direct call from onEnable throws. The node is built from the
                // same `api` the install above handed out, so a player asking
                // /cloud list and a plugin calling the API read one mirror.
                lifecycleManager.registerEventHandler(LifecycleEvents.COMMANDS) { event ->
                    event.registrar().register(
                        // feedState and not a second one: the command writes
                        // the opt-out that the feed above reads, and two
                        // instances would leave `/cloud events off` looking as
                        // though it worked while the lines kept arriving.
                        cloudCommand(api, PaperSource, feedState).build(),
                        "Spawnery cloud commands",
                    )
                }
                // Info and not fine. This line is how a server owner confirms
                // the API is there for their own plugins, and it is the only
                // outward sign that the install path ran at all: installing
                // leaves no trace on the wire, so nothing upstream can see it.
                logger.info(
                    "spawnery API installed for network ${self.network()} group ${self.group()}",
                )

                scheduler = Executors.newSingleThreadScheduledExecutor { runnable ->
                    Thread(runnable, "spawnery-agent").apply { isDaemon = true }
                }

                val session = SessionLoop(
                    // The bundle is read here, per attempt, rather than once at
                    // enable: see Environment.Configured. A channel is built
                    // per attempt anyway, so this costs one file read on a
                    // path that is already opening a TCP connection.
                    channels = {
                        OperatorChannel.build(env.endpoint, Files.readAllBytes(env.caBundlePath))
                    },
                    credentials = BearerCredentials.of(TokenSource(env.tokenPath)),
                    role = role,
                    scheduler = scheduler,
                    version = pluginMeta.version,
                    // SessionLoop never gives up and never escalates a broken stream
                    // on its own — this callback is the only place that decides how
                    // loud a permanently unreachable operator gets to be. INFO would
                    // bury a 30-second-cadence failure among routine startup lines,
                    // and by the time anyone reads the log looking for it, "the
                    // server has been unmonitored for six hours" is indistinguishable
                    // from "the server has been fine". WARNING keeps every failed
                    // attempt out of the noise without escalating to SEVERE, which
                    // would misrepresent a condition the loop is already recovering
                    // from on its own — the server itself is healthy; only its
                    // connection to the operator is not.
                    log = { message, error -> logger.log(Level.WARNING, message, error) },
                    // Two things are per-stream and nothing else would tell
                    // them a renewal happened. The connector fails what is in
                    // flight rather than resending it, because only the caller
                    // knows whether repeating a request is safe. And the
                    // interest is forgotten, because the operator's answer for
                    // a session it has never seen is "no" -- without this the
                    // agent would believe it had already said "yes" and the
                    // feed would go silent until somebody's permissions
                    // happened to change.
                    onStreamChanged = {
                        connector.onStreamChanged()
                        lastInterest = null
                    },
                )
                loop = session

                server.pluginManager.registerEvents(this, this)

                // Bukkit.getOnlinePlayers() is not thread-safe. The count is
                // sampled here, on the main thread, and the network side only
                // ever reads what this wrote.
                server.scheduler.runTaskTimer(this, Runnable {
                    state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())
                    // The feed's window closes here, on the main thread, and
                    // the interest is recomputed on the same tick: a player
                    // joining, leaving or gaining a permission all change the
                    // answer, and watching for each of those separately is
                    // three subscriptions to get wrong.
                    feed.tick()
                    reportInterest(feed.wanted())
                }, 0L, SAMPLE_TICKS)

                // Once here, before the first stream exists, because the timer
                // above does not run until the next tick and onEnable has to
                // return first. The operator's ReportInterval schedules its
                // first report at delay zero, so without this the first
                // PlayerCount of the process carries the zeroes the counters
                // were constructed with -- and internal/controller/candidates.go
                // reads Slots - Players, so a server that has just gone Ready
                // announces itself as having no free slots for a tick. Both
                // calls are main-thread-safe here, which is the whole reason
                // the sampling is on a Bukkit timer at all.
                state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())

                session.start()
                logger.info("spawnery agent connecting to ${env.endpoint}")
            }
        }
    }

    /**
     * Tells the operator whether anybody is here to read events.
     *
     * Sent only when the answer changes. EventInterest is a state, and one
     * resent every second would be a report the operator has to read in order
     * to learn nothing.
     */
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
        // Uninstall before anything else: install refuses a second
        // implementation, so a plugin that enabled twice without this would
        // throw on the second and take the whole agent down with it.
        Spawnery.uninstall()
        loop?.stop()
        if (::scheduler.isInitialized) {
            scheduler.shutdownNow()
            scheduler.awaitTermination(2, TimeUnit.SECONDS)
        }
    }

    @EventHandler
    fun onServerLoad(event: ServerLoadEvent) {
        if (event.type != ServerLoadEvent.LoadType.STARTUP) return
        state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())
        if (state.markReady()) {
            loop?.send(role.ready())
        }
    }

    private companion object {
        // internal/podspec.AgentMountPath. Hard-coded rather than configurable:
        // the operator creates these pods and mounts exactly here, and a second
        // place to spell it would be a second place to get it wrong.
        const val AGENT_DIR = "/var/run/spawnery"

        // One second at 20 ticks. Fast enough that the reported count is never
        // stale by more than the report interval's own resolution, cheap enough
        // that it is invisible next to a tick.
        const val SAMPLE_TICKS = 20L
    }
}
