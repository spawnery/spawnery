package cloud.spawnery.agent

/** Heap in use and its maximum, in bytes. */
fun heapNow(runtime: Runtime = Runtime.getRuntime()): Pair<Long, Long> =
    (runtime.totalMemory() - runtime.freeMemory()) to runtime.maxMemory()
