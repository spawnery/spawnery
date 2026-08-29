package cloud.spawnery.agent

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class StyleTest {
    @Test
    fun `a value carrying a tag cannot inject one`() {
        // The operator's own refusal messages reach chat verbatim, and one
        // containing a `<` would otherwise be eaten by the parser or make it
        // throw inside a network callback.
        val hostile = "that group has room for <red>0</red>, not 9"

        val styled = Style.bad(hostile)

        assertTrue(styled.contains("\\<red>"), "the tag was not escaped: $styled")
        assertTrue(styled.startsWith("<red>") && styled.endsWith("</red>"),
            "the style's own tags were escaped away: $styled")
    }

    @Test
    fun `a backslash survives rather than eating the next character`() {
        // Escaping `<` before `\` would turn a value's own backslash into an
        // escape for the tag marker, and print the backslash.
        assertEquals("<gray>a\\\\b</gray>", Style.quiet("a\\b"))
    }

    @Test
    fun `an ordinary value is wrapped and otherwise untouched`() {
        assertEquals("<aqua>lobby-a3f9</aqua>", Style.name("lobby-a3f9"))
    }
}
