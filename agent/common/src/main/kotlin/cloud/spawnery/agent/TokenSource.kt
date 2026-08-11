package cloud.spawnery.agent

import java.nio.file.Files
import java.nio.file.Path

/**
 * Reads the projected ServiceAccount token, every time it is asked.
 *
 * The token lives 600 seconds and the kubelet replaces the file in place. It is
 * deliberately never cached: a value read once at startup carries the first
 * session and none after it, and the resulting failure presents as an
 * authentication problem rather than as a caching bug.
 */
class TokenSource(private val path: Path) {
    fun read(): String = Files.readString(path).trim()
}
