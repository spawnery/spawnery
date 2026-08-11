package cloud.spawnery.agent

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertInstanceOf
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path

class EnvironmentTest {
    @Test
    fun `is dormant without an endpoint`(@TempDir dir: Path) {
        val result = Environment.from(mapOf<String, String>()::get, dir)
        val dormant = assertInstanceOf(Environment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("SPAWNERY_OPERATOR_ENDPOINT"))
    }

    @Test
    fun `is dormant when the endpoint is set but empty`(@TempDir dir: Path) {
        val result = Environment.from(mapOf("SPAWNERY_OPERATOR_ENDPOINT" to "")::get, dir)
        val dormant = assertInstanceOf(Environment.Dormant::class.java, result)
        // The reason, like its three siblings: dormancy is a normal outcome, so
        // the log line is the only thing that distinguishes "not configured" —
        // which is what make image-test runs — from an agent that cannot read
        // what the operator mounted for it.
        assertTrue(dormant.reason.contains("SPAWNERY_OPERATOR_ENDPOINT"), dormant.reason)
    }

    @Test
    fun `is dormant when the CA bundle is missing`(@TempDir dir: Path) {
        val env = mapOf("SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443")
        val result = Environment.from(env::get, dir)
        val dormant = assertInstanceOf(Environment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("ca.crt"), dormant.reason)
    }

    @Test
    fun `is dormant when the token is unreadable`(@TempDir dir: Path) {
        // The CA is there and the token is not, which is the one dormancy
        // branch nothing covered. It is also the branch a change would break
        // most quietly: an agent that goes dormant here looks exactly like one
        // that was never meant to connect.
        Files.writeString(dir.resolve("ca.crt"), "pem")
        val env = mapOf("SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443")

        val result = Environment.from(env::get, dir)

        val dormant = assertInstanceOf(Environment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("token"), dormant.reason)
    }

    @Test
    fun `is configured when endpoint, CA and token are all present`(@TempDir dir: Path) {
        Files.writeString(dir.resolve("ca.crt"), "pem")
        Files.writeString(dir.resolve("token"), "t")
        val env = mapOf("SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443")

        val result = Environment.from(env::get, dir)

        val configured = assertInstanceOf(Environment.Configured::class.java, result)
        assertEquals("operator:9443", configured.endpoint)
        // The path, not its bytes: the bundle is re-read per connection attempt
        // so a CA rotation reaches a pod that was already running.
        assertEquals(dir.resolve("ca.crt"), configured.caBundlePath)
        assertEquals(dir.resolve("token"), configured.tokenPath)
    }
}
