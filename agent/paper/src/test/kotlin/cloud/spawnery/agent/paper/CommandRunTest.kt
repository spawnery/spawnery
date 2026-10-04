package cloud.spawnery.agent.paper

import cloud.spawnery.agent.pb.ExecuteCommand
import net.kyori.adventure.text.Component
import net.kyori.adventure.text.format.NamedTextColor
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class CommandRunTest {
    private fun command(text: String) = ExecuteCommand.newBuilder().setId(42).setCommand(text).build()

    @Test
    fun `feedback arrives as plain text`() {
        val output = CommandOutput()
        output.add(Component.text("There are ").append(Component.text("0", NamedTextColor.RED)).append(Component.text(" players")))
        assertEquals(listOf("There are 0 players"), output.lines())
    }

    @Test
    fun `legacy colour codes are stripped and tags are left as text`() {
        val output = CommandOutput()
        output.addText("§aGreen §LBold §x§f§f§0§0§0§0hex")
        output.addText("<click:run_command:/op mallory>press</click>")
        assertEquals(listOf("Green Bold hex", "<click:run_command:/op mallory>press</click>"), output.lines())
    }

    @Test
    fun `output is bounded in lines and in characters`() {
        val output = CommandOutput()
        output.addText((1..25).joinToString("\n") { "line $it" })
        output.addText("x".repeat(300))
        assertEquals(20, output.lines().size)
        assertEquals("line 20", output.lines().last())

        val long = CommandOutput()
        long.addText("x".repeat(300))
        assertEquals(256, long.lines().single().length)
    }

    @Test
    fun `a known command is ok and carries its feedback and id`() {
        val outcome = runCommand(command("list")) { line, feedback ->
            assertEquals("list", line)
            feedback(Component.text("There are 0 of a max of 20 players online"))
            true
        }
        assertTrue(outcome.ok)
        assertEquals(42L, outcome.id)
        assertEquals(listOf("There are 0 of a max of 20 players online"), outcome.outputList)
    }

    @Test
    fun `an unknown command is not ok`() {
        val outcome = runCommand(command("nonsense")) { _, _ -> false }
        assertFalse(outcome.ok)
        assertEquals("unknown command", outcome.error)
    }

    @Test
    fun `a command that throws is not ok and says why`() {
        val outcome = runCommand(command("boom")) { _, feedback ->
            feedback(Component.text("before the throw"))
            throw IllegalStateException("plugin broke")
        }
        assertFalse(outcome.ok)
        assertEquals("plugin broke", outcome.error)
        assertEquals(listOf("before the throw"), outcome.outputList)
    }

    @Test
    fun `a linkage error from a plugin is an outcome too`() {
        val outcome = runCommand(command("boom")) { _, _ -> throw NoClassDefFoundError("gone") }
        assertFalse(outcome.ok)
        assertEquals("gone", outcome.error)
    }
}
