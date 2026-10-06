package cloud.spawnery.agent.paper

import io.papermc.paper.plugin.bootstrap.BootstrapContext
import io.papermc.paper.plugin.bootstrap.PluginBootstrap
import java.nio.file.Path

/**
 * Runs before the server opens its world, which happens before any plugin's
 * onLoad: waiting later would let Paper create fresh world data over a world
 * that is still downloading.
 */
class WorldSyncBootstrap : PluginBootstrap {
    override fun bootstrap(context: BootstrapContext) {
        if (System.getenv(WorldSync.ENV_ENABLED) != "1") return
        val sync = WorldSync(Path.of(System.getProperty("user.dir"), WorldSync.CONTROL_DIR))
        val started = System.currentTimeMillis()
        when (val outcome = sync.awaitReady(WAIT_MILLIS)) {
            WorldSync.Outcome.Ready ->
                context.logger.info("spawnery world sync: world ready after {} ms", System.currentTimeMillis() - started)
            is WorldSync.Outcome.Failed -> abort(context, "the world could not be downloaded: ${outcome.reason}")
            WorldSync.Outcome.TimedOut -> abort(context, "no world after ${WAIT_MILLIS / 1000} s")
        }
    }

    // halt, not exit: no shutdown hook may save a half-downloaded world. It
    // also skips log4j's shutdown, so the reason goes to stderr directly.
    private fun abort(context: BootstrapContext, reason: String) {
        context.logger.error("spawnery world sync: {}", reason)
        System.err.println("spawnery world sync: $reason")
        System.err.flush()
        Runtime.getRuntime().halt(1)
    }

    private companion object {
        const val WAIT_MILLIS = 10 * 60 * 1000L
    }
}
