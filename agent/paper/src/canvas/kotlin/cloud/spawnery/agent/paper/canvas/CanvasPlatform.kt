package cloud.spawnery.agent.paper.canvas

import cloud.spawnery.agent.paper.RegionTicks
import cloud.spawnery.agent.paper.ServerPlatform
import cloud.spawnery.agent.paper.TickSample
import io.canvasmc.canvas.region.RegionTickData.Frame
import io.canvasmc.canvas.region.WorldRegionizer.ChunkRegion
import org.bukkit.plugin.Plugin
import java.util.concurrent.CompletableFuture

class CanvasPlatform(private val plugin: Plugin) : ServerPlatform {
    private val server get() = plugin.server

    override fun ticks(): TickSample {
        val regions = mutableListOf<TickSample>()
        forEachLiveRegion { region ->
            val data = region.tickData
            // Canvas answers a region that has not ticked yet with a
            // NullPointerException: it has no tick report to read.
            try {
                regions += TickSample(data.getTPS(Frame._1_MINUTE), data.getMSPT(Frame._5_SECONDS))
            } catch (_: NullPointerException) {
            }
        }
        // On the global region thread these answer for the global tick.
        val worst = RegionTicks.worst(regions, TickSample(server.tps[0], server.averageTickTime))
        return worst.copy(tps = minOf(worst.tps, PAPER_MAX_TPS))
    }

    override fun saveRegions(): List<CompletableFuture<*>> {
        val saves = mutableListOf<CompletableFuture<*>>()
        forEachLiveRegion { region -> saves += CompletableFuture<Void>().also { offer(region, it) } }
        return saves
    }

    // A region holds one save ticket at a time and save-all has just handed
    // each its own, so propagate throws until the region takes that one at
    // the start of a tick it saves in. Ours is taken a tick later.
    private fun offer(region: ChunkRegion, saved: CompletableFuture<Void>) {
        if (saved.isDone) return
        // A dead region's chunks went to another region, which may have saved
        // before they arrived.
        if (region.state == ChunkRegion.State.DEAD) {
            saved.completeExceptionally(IllegalStateException("region ${region.id} merged or split before it saved"))
            return
        }
        try {
            region.tickData.propagateSaveAllTicket(false).whenComplete { _, error ->
                if (error == null) saved.complete(null) else saved.completeExceptionally(error)
            }
        } catch (_: IllegalStateException) {
            server.globalRegionScheduler.run(plugin) { offer(region, saved) }
        } catch (t: Throwable) {
            saved.completeExceptionally(t)
        }
    }

    private fun forEachLiveRegion(action: (ChunkRegion) -> Unit) {
        for (world in server.worlds) {
            world.regionizer.computeForAllChunkRegions { region ->
                if (!region.hasNoAliveSections() && region.state != ChunkRegion.State.DEAD) action(region)
            }
        }
    }

    private companion object {
        // Canvas averages the rate of each tick rather than counting ticks over
        // the window, which jitter pushes past 20: an idle server read 20.01
        // (measured 2026-10-26). Paper's figure stops at 20.
        const val PAPER_MAX_TPS = 20.0
    }
}
