package cloud.spawnery.agent.paper

import cloud.spawnery.agent.pb.ExecuteCommand
import cloud.spawnery.agent.pb.ExecuteOutcome
import net.kyori.adventure.text.Component
import net.kyori.adventure.text.serializer.plain.PlainTextComponentSerializer

internal const val OUTPUT_MAX_LINES = 20
internal const val OUTPUT_MAX_CHARS = 256

internal class CommandOutput(
    private val maxLines: Int = OUTPUT_MAX_LINES,
    private val maxChars: Int = OUTPUT_MAX_CHARS,
) {
    private val lines = mutableListOf<String>()

    fun add(message: Component) = addText(PlainTextComponentSerializer.plainText().serialize(message))

    fun addText(text: String) {
        for (line in text.split('\n')) {
            if (lines.size >= maxLines) return
            lines += LEGACY_CODE.replace(line, "").take(maxChars)
        }
    }

    fun lines(): List<String> = lines.toList()

    private companion object {
        val LEGACY_CODE = Regex("§[0-9a-fk-orx]", RegexOption.IGNORE_CASE)
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
