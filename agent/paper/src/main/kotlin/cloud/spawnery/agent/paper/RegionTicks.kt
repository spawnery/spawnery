package cloud.spawnery.agent.paper

object RegionTicks {
    fun worst(regions: List<TickSample>, fallback: TickSample): TickSample =
        if (regions.isEmpty()) {
            fallback
        } else {
            TickSample(regions.minOf { it.tps }, regions.maxOf { it.mspt })
        }
}
