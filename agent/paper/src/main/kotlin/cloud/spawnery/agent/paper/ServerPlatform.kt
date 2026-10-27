package cloud.spawnery.agent.paper

import org.bukkit.Server
import org.bukkit.plugin.Plugin
import java.util.concurrent.CompletableFuture

data class TickSample(val tps: Double, val mspt: Double)

/** What differs between Paper and Canvas. Both methods run on the global region thread. */
interface ServerPlatform {
    fun ticks(): TickSample

    fun saveRegions(): List<CompletableFuture<*>>
}

class PaperPlatform(private val server: Server) : ServerPlatform {
    // Paper's own one-minute average; it caps at 20.
    override fun ticks(): TickSample = TickSample(server.tps[0], server.averageTickTime)

    override fun saveRegions(): List<CompletableFuture<*>> = emptyList()
}

object Platforms {
    const val CANVAS_MARKER = "io.canvasmc.canvas.region.WorldRegionizer"
    const val CANVAS_IMPL = "cloud.spawnery.agent.paper.canvas.CanvasPlatform"

    /** [loader] is the server's; the Canvas implementation is loaded from the agent's own. */
    fun detect(plugin: Plugin, loader: ClassLoader): ServerPlatform {
        try {
            Class.forName(CANVAS_MARKER, false, loader)
        } catch (_: ClassNotFoundException) {
            return PaperPlatform(plugin.server)
        }
        return Class.forName(CANVAS_IMPL, true, Platforms::class.java.classLoader)
            .getConstructor(Plugin::class.java)
            .newInstance(plugin) as ServerPlatform
    }
}
