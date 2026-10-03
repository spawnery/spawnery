package cloud.spawnery.agent

import java.nio.file.Files
import java.nio.file.Path

/** Never cached: the token lives 600 seconds and the kubelet replaces the file in place. */
class TokenSource(private val path: Path) {
    fun read(): String = Files.readString(path).trim()
}
