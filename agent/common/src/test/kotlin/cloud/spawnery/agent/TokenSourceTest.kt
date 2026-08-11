package cloud.spawnery.agent

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path

class TokenSourceTest {
    @Test
    fun `reads the token from disk every time`(@TempDir dir: Path) {
        val path = dir.resolve("token")
        Files.writeString(path, "first")
        val tokens = TokenSource(path)

        assertEquals("first", tokens.read())

        // The kubelet replaces this file roughly every eight minutes. A token
        // cached at startup carries the first session and no later one, and
        // the failure would read as an authentication problem rather than a
        // caching bug.
        Files.writeString(path, "second")
        assertEquals("second", tokens.read())
    }

    @Test
    fun `strips the trailing newline a file may carry`(@TempDir dir: Path) {
        val path = dir.resolve("token")
        Files.writeString(path, "value\n")
        assertEquals("value", TokenSource(path).read())
    }
}
