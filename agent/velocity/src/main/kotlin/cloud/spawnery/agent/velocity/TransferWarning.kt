package cloud.spawnery.agent.velocity

import net.kyori.adventure.text.Component

object TransferWarning {
    const val KEY = "spawnery.transfer.warning"
    const val FALLBACK = "You will be reconnected in %s seconds."
    const val LEAD_MILLIS = 10_000L

    fun message(seconds: Long): Component =
        Component.translatable()
            .key(KEY)
            .fallback(FALLBACK)
            .arguments(Component.text(seconds))
            .build()
}
