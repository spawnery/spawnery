package cloud.spawnery.agent.paper.canvas

import cloud.spawnery.agent.paper.RegionTicks
import cloud.spawnery.agent.paper.ServerPlatform
import cloud.spawnery.agent.paper.TickSample
import io.canvasmc.canvas.region.RegionTickData.Frame
import io.canvasmc.canvas.region.WorldRegionizer.ChunkRegion
import org.bukkit.Server
import java.util.concurrent.CompletableFuture

class CanvasPlatform(private val server: Server) : ServerPlatform {
    override fun ticks(): TickSample {
        val regions = mutableListOf<TickSample>()
        for (world in server.worlds) {
            world.regionizer.computeForAllChunkRegions { region ->
                if (region.hasNoAliveSections() || region.state == ChunkRegion.State.DEAD) {
                    return@computeForAllChunkRegions
                }
                val data = region.tickData
                // Canvas answers a region that has not ticked yet with a
                // NullPointerException: it has no tick report to read.
                val sample = try {
                    TickSample(data.getTPS(Frame._1_MINUTE), data.getMSPT(Frame._5_SECONDS))
                } catch (_: NullPointerException) {
                    return@computeForAllChunkRegions
                }
                regions += sample
            }
        }
        // On the global region thread these answer for the global tick.
        val worst = RegionTicks.worst(regions, TickSample(server.tps[0], server.averageTickTime))
        return worst.copy(tps = minOf(worst.tps, PAPER_MAX_TPS))
    }

    override fun saveRegions(): List<CompletableFuture<*>> = emptyList()

    private companion object {
        // Canvas averages the rate of each tick rather than counting ticks over
        // the window, which jitter pushes past 20: an idle server read 20.01
        // (measured 2026-10-26). Paper's figure stops at 20.
        const val PAPER_MAX_TPS = 20.0
    }
}
