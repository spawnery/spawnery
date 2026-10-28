package cloud.spawnery.agent

import java.io.File
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class ClocksTest {
    /**
     * The three left are instants another process reads: a cookie's expiry, the proxies' shared transfer
     * windows, and the deadline of a snapshot request, which the node agent checks.
     */
    @Test
    fun `no agent measures a duration on the wall clock`() {
        val sources = File("..").listFiles()!!
            .flatMap { project -> listOf("main", "canvas").map { File(project, "src/$it/kotlin") } }
            .filter { it.isDirectory }
            .flatMap { dir -> dir.walkTopDown().filter { it.extension == "kt" }.toList() }
        assertTrue(sources.size > 20, "found only ${sources.size} sources from ${File("..").absolutePath}")

        val wallClock = sources.flatMap { file ->
            file.readLines().withIndex()
                .filter { (_, line) -> "currentTimeMillis" in line }
                .map { (i, line) -> "${file.path}:${i + 1}: ${line.trim()}" }
        }
        val instants = wallClock.filter { "TransferCookie(" in it || "turnClock" in it || "wallClock" in it }

        assertEquals(emptyList(), wallClock - instants.toSet())
        assertEquals(3, instants.size, "the three instants moved: $instants")
    }
}
