package cloud.spawnery.agent

import org.bukkit.Bukkit
import org.bukkit.event.EventHandler
import org.bukkit.event.Listener
import org.bukkit.event.server.ServerLoadEvent
import org.bukkit.plugin.java.JavaPlugin
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
    private lateinit var scheduler: ScheduledExecutorService
    private var loop: SessionLoop? = null

    override fun onEnable() {
        when (val env = Environment.from(System::getenv, Path.of(AGENT_DIR))) {
            is Environment.Dormant -> {
                logger.info("spawnery agent dormant: ${env.reason}")
                return
            }

            is Environment.Configured -> {
                scheduler = Executors.newSingleThreadScheduledExecutor { runnable ->
                    Thread(runnable, "spawnery-agent").apply { isDaemon = true }
                }

                val session = SessionLoop(
                    channels = { OperatorChannel.build(env.endpoint, env.caBundle) },
                    credentials = BearerCredentials.of(TokenSource(env.tokenPath)),
                    state = state,
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
                )
                loop = session

                server.pluginManager.registerEvents(this, this)

                // Bukkit.getOnlinePlayers() is not thread-safe. The count is
                // sampled here, on the main thread, and the network side only
                // ever reads what this wrote.
                server.scheduler.runTaskTimer(this, Runnable {
                    state.sample(Bukkit.getOnlinePlayers().size, Bukkit.getMaxPlayers())
                }, 0L, SAMPLE_TICKS)

                session.start()
                logger.info("spawnery agent connecting to ${env.endpoint}")
            }
        }
    }

    override fun onDisable() {
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
            loop?.readyChanged()
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
