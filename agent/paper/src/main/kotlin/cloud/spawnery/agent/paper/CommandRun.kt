package cloud.spawnery.agent.paper

import cloud.spawnery.agent.pb.ExecuteCommand
import cloud.spawnery.agent.pb.ExecuteOutcome
import net.kyori.adventure.text.Component
import net.kyori.adventure.text.serializer.plain.PlainTextComponentSerializer

internal const val OUTPUT_MAX_LINES = 20
internal const val OUTPUT_MAX_CHARS = 256

/** Synchronised: on Canvas a command may answer from region threads other than the one that ran it. */
internal class CommandOutput(
    private val maxLines: Int = OUTPUT_MAX_LINES,
    private val maxChars: Int = OUTPUT_MAX_CHARS,
) {
    private val lines = mutableListOf<String>()

    fun add(message: Component) = addText(PlainTextComponentSerializer.plainText().serialize(message))

    fun addText(text: String) {
        val cleaned = text.split('\n').map { LEGACY_CODE.replace(it, "").take(maxChars) }
        synchronized(lines) {
            for (line in cleaned) {
                if (lines.size >= maxLines) return
                lines += line
            }
        }
    }

    fun lines(): List<String> = synchronized(lines) { lines.toList() }

    private companion object {
        val LEGACY_CODE = Regex("§[0-9a-fk-orx]", RegexOption.IGNORE_CASE)
    }
}

/**
 * Hands [command] to [schedule] and its outcome to [answer]. A scheduler that
 * refuses, as Paper's does while the plugin disables, is answered at once
 * rather than left to expire on the operator.
 */
internal fun scheduleCommand(
    command: ExecuteCommand,
    schedule: (Runnable) -> Unit,
    run: () -> ExecuteOutcome,
    answer: (ExecuteOutcome) -> Unit,
) {
    try {
        schedule { answer(run()) }
    } catch (refused: Exception) {
        answer(
            ExecuteOutcome.newBuilder()
                .setId(command.id)
                .setOk(false)
                .setError("the server cannot run commands now: ${refused.message ?: refused.javaClass.simpleName}")
                .build(),
        )
    }
}

/** Feedback a command sends after [dispatch] returns is not in the outcome. */
internal fun runCommand(
    command: ExecuteCommand,
    dispatch: (String, (Component) -> Unit) -> Boolean,
): ExecuteOutcome {
    val output = CommandOutput()
    val outcome = ExecuteOutcome.newBuilder().setId(command.id)
    try {
        val known = dispatch(command.command) { output.add(it) }
        outcome.setOk(known)
        if (!known) outcome.setError("unknown command")
    } catch (failure: Throwable) {
        outcome.setOk(false).setError(failure.message ?: failure.javaClass.simpleName)
    }
    return outcome.addAllOutput(output.lines()).build()
}
