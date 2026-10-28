package cloud.spawnery.agent.paper

/**
 * Runs inside the sampling timer, so nothing it catches may stop the rest of
 * that timer's body. A fork without the tick API fails with a LinkageError
 * such as NoSuchMethodError, which is not an Exception.
 */
class TickSampler(
    private val ticks: () -> TickSample,
    private val sample: (tps: Double, mspt: Double) -> Unit,
    private val warnOnce: (Throwable) -> Unit,
) {
    private var warned = false

    fun sample() {
        try {
            val read = ticks()
            sample(read.tps, read.mspt)
        } catch (e: Exception) {
            warn(e)
        } catch (e: LinkageError) {
            warn(e)
        }
    }

    private fun warn(e: Throwable) {
        if (warned) return
        warned = true
        warnOnce(e)
    }
}
