package cloud.spawnery.agent

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class ExecuteLinesTest {
    @Test
    fun `one server shows its output under a line saying it ran`() {
        val lines = executeLines("lobby-a", listOf(ExecuteLine("lobby-a", true, listOf("There are 0 players"), "")))
        assertEquals(2, lines.size)
        assertTrue(lines[0].startsWith("<green>✔</green>") && plain(lines[0]).contains("lobby-a ran it"), lines[0])
        assertTrue(plain(lines[1]).contains("There are 0 players"), lines[1])
    }

    @Test
    fun `output reaches chat as text, never as markup`() {
        val lines = executeLines("lobby-a", listOf(ExecuteLine("lobby-a", true, listOf("<click:run_command:/op mallory>press"), "")))
        assertTrue(lines[1].contains("\\<click:run_command"), "a tag in output was left live: ${lines[1]}")
    }

    @Test
    fun `one server that failed says why`() {
        val lines = executeLines("lobby-a", listOf(ExecuteLine("lobby-a", false, emptyList(), "unknown command")))
        assertTrue(lines.single().startsWith("<red>✘</red>") && plain(lines.single()).contains("unknown command"), lines.single())
    }

    @Test
    fun `a group gets a line per server without output and a total`() {
        val lines = executeLines(
            "lobby",
            listOf(
                ExecuteLine("lobby-b", false, emptyList(), "no answer within 8s"),
                ExecuteLine("lobby-a", true, listOf("hidden"), ""),
            ),
        )
        assertEquals(3, lines.size)
        assertTrue(plain(lines[0]).contains("lobby-a") && plain(lines[0]).contains("ok"), lines[0])
        assertTrue(plain(lines[1]).contains("lobby-b") && plain(lines[1]).contains("no answer within 8s"), lines[1])
        assertTrue(lines.none { it.contains("hidden") }, "a group answer showed output: $lines")
        assertTrue(lines[2].startsWith("<red>✘</red>") && plain(lines[2]).contains("1 of 2 servers ran it"), lines[2])
    }
}
