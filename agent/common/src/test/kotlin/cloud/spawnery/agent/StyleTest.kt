package cloud.spawnery.agent

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class StyleTest {
    @Test
    fun `a value carrying a tag cannot inject one`() {
        // The operator's refusal messages reach chat verbatim.
        val hostile = "that group has room for <red>0</red>, not 9"

        val styled = Style.bad(hostile)

        assertTrue(styled.contains("\\<red>"), "the tag was not escaped: $styled")
        assertTrue(styled.startsWith("<red>") && styled.endsWith("</red>"),
            "the style's own tags were escaped away: $styled")
    }

    @Test
    fun `a backslash survives rather than eating the next character`() {
        // Escaping `<` before `\` would turn a value's backslash into an escape.
        assertEquals("<gray>a\\\\b</gray>", Style.quiet("a\\b"))
    }

    @Test
    fun `an ordinary value is wrapped and otherwise untouched`() {
        assertEquals("<aqua>lobby-a3f9</aqua>", Style.name("lobby-a3f9"))
    }

    @Test
    fun `a subject carries its full name on hover and a click that suggests cloud info`() {
        assertEquals(
            "<hover:show_text:'challenge-3f2b1c9a'><click:suggest_command:'/cloud info challenge-3f2b1c9a'>" +
                "<aqua>challenge-3f2b1c</aqua></click></hover>",
            Style.subject("challenge-3f2b1c9a", "challenge-3f2b1c"),
        )
        assertEquals("challenge-3f2b1c", plain(Style.subject("challenge-3f2b1c9a", "challenge-3f2b1c")))
    }

    @Test
    fun `a name a quoted argument cannot carry gets no hover and no click`() {
        assertEquals("<aqua>it's</aqua>", Style.subject("it's"))
        assertEquals("<aqua>Lobby</aqua>", Style.subject("Lobby"))
        assertEquals("<aqua>a\\<b</aqua>", Style.subject("a<b"))
    }
}
