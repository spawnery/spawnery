package cloud.spawnery.agent.paper

import java.lang.reflect.Method
import java.lang.reflect.Modifier

/**
 * Waits until the chunk system has written what a save queued. Paper has no
 * API for this. Moonrise's `flush(ServerLevel)` only polls its write queue and
 * checks no thread (read from Purpur 26.3 build 2647), so it may run off the
 * main thread; `save-all flush` runs the same wait and an fsync per region
 * file on the main thread.
 */
class RegionWrites private constructor(private val flush: Method?) {
    val available: Boolean get() = flush != null

    /** Off the main thread. [levels] are the worlds' `ServerLevel`s. */
    fun await(levels: List<Any>) {
        val flush = checkNotNull(flush) { "no chunk system flush on this server" }
        for (level in levels) flush.invoke(null, level)
    }

    companion object {
        const val IO_CLASS = "ca.spottedleaf.moonrise.patches.chunk_system.io.MoonriseRegionFileIO"
        const val LEVEL_CLASS = "net.minecraft.server.level.ServerLevel"

        fun locate(loader: ClassLoader, ioClass: String = IO_CLASS, levelClass: String = LEVEL_CLASS): RegionWrites =
            RegionWrites(
                runCatching {
                    Class.forName(ioClass, false, loader).methods.single {
                        it.name == "flush" &&
                            Modifier.isStatic(it.modifiers) &&
                            it.parameterTypes.singleOrNull()?.name == levelClass
                    }
                }.getOrNull(),
            )
    }
}
