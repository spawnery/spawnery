package cloud.spawnery.agent.velocity

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertInstanceOf
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.io.TempDir
import java.nio.file.Files
import java.nio.file.Path

class ProxyEnvironmentTest {
    /** The two files the kubelet projects; only their readability matters. */
    private fun mount(dir: Path) {
        Files.writeString(dir.resolve("ca.crt"), "pem")
        Files.writeString(dir.resolve("token"), "t")
    }

    private fun env(vararg pairs: Pair<String, String>) = mapOf(*pairs)::get

    @Test
    fun `a complete environment is configured`(@TempDir dir: Path) {
        mount(dir)

        val result = ProxyEnvironment.from(
            env(
                "SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443",
                "SPAWNERY_PLAYER_LIMIT" to "500",
                "SPAWNERY_FALLBACK_GROUPS" to "lobby,hub",
            ),
            dir,
        )

        val configured = assertInstanceOf(ProxyEnvironment.Configured::class.java, result)
        assertEquals("operator:9443", configured.base.endpoint)
        assertEquals(dir.resolve("ca.crt"), configured.base.caBundlePath)
        assertEquals(dir.resolve("token"), configured.base.tokenPath)
        assertEquals(500, configured.playerLimit)
        // Order is preserved: the router tries them in order.
        assertEquals(listOf("lobby", "hub"), configured.fallbackGroups)
    }

    @Test
    fun `a missing endpoint is dormant without mentioning the proxy variables`(@TempDir dir: Path) {
        mount(dir)

        val result = ProxyEnvironment.from(
            env(
                "SPAWNERY_PLAYER_LIMIT" to "500",
                "SPAWNERY_FALLBACK_GROUPS" to "lobby",
            ),
            dir,
        )

        val dormant = assertInstanceOf(ProxyEnvironment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("SPAWNERY_OPERATOR_ENDPOINT"), dormant.reason)
        // The check order is what this asserts: outside a cluster the reason
        // must be "not connecting to an operator", which the image test greps
        // for.
        assertFalse(dormant.reason.contains("SPAWNERY_PLAYER_LIMIT"), dormant.reason)
        assertFalse(dormant.reason.contains("SPAWNERY_FALLBACK_GROUPS"), dormant.reason)
    }

    @Test
    fun `a missing player limit is dormant and names SPAWNERY_PLAYER_LIMIT`(@TempDir dir: Path) {
        mount(dir)

        val result = ProxyEnvironment.from(
            env(
                "SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443",
                "SPAWNERY_FALLBACK_GROUPS" to "lobby",
            ),
            dir,
        )

        val dormant = assertInstanceOf(ProxyEnvironment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("SPAWNERY_PLAYER_LIMIT"), dormant.reason)
    }

    @Test
    fun `a non-numeric player limit is dormant and names SPAWNERY_PLAYER_LIMIT`(@TempDir dir: Path) {
        mount(dir)

        val result = ProxyEnvironment.from(
            env(
                "SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443",
                "SPAWNERY_PLAYER_LIMIT" to "many",
                "SPAWNERY_FALLBACK_GROUPS" to "lobby",
            ),
            dir,
        )

        val dormant = assertInstanceOf(ProxyEnvironment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("SPAWNERY_PLAYER_LIMIT"), dormant.reason)
    }

    @Test
    fun `a zero player limit is dormant and names SPAWNERY_PLAYER_LIMIT`(@TempDir dir: Path) {
        mount(dir)

        // A limit of zero would have the registry discard every count.
        val result = ProxyEnvironment.from(
            env(
                "SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443",
                "SPAWNERY_PLAYER_LIMIT" to "0",
                "SPAWNERY_FALLBACK_GROUPS" to "lobby",
            ),
            dir,
        )

        val dormant = assertInstanceOf(ProxyEnvironment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("SPAWNERY_PLAYER_LIMIT"), dormant.reason)
    }

    @Test
    fun `a missing fallback list is dormant and names SPAWNERY_FALLBACK_GROUPS`(@TempDir dir: Path) {
        mount(dir)

        val result = ProxyEnvironment.from(
            env(
                "SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443",
                "SPAWNERY_PLAYER_LIMIT" to "500",
            ),
            dir,
        )

        val dormant = assertInstanceOf(ProxyEnvironment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("SPAWNERY_FALLBACK_GROUPS"), dormant.reason)
    }

    @Test
    fun `an empty fallback list is dormant and names SPAWNERY_FALLBACK_GROUPS`(@TempDir dir: Path) {
        mount(dir)

        // Coming up without a fallback would pass the ready gate and then
        // disconnect every player with "no available server".
        val result = ProxyEnvironment.from(
            env(
                "SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443",
                "SPAWNERY_PLAYER_LIMIT" to "500",
                "SPAWNERY_FALLBACK_GROUPS" to "",
            ),
            dir,
        )

        val dormant = assertInstanceOf(ProxyEnvironment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("SPAWNERY_FALLBACK_GROUPS"), dormant.reason)
    }

    @Test
    fun `blank entries in the fallback list are dropped, not kept`(@TempDir dir: Path) {
        mount(dir)

        // A group named "" or " hub" matches nothing in the router.
        for (raw in listOf("lobby,,hub", "lobby, hub", " lobby , hub ", ",lobby,hub,")) {
            val result = ProxyEnvironment.from(
                env(
                    "SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443",
                    "SPAWNERY_PLAYER_LIMIT" to "500",
                    "SPAWNERY_FALLBACK_GROUPS" to raw,
                ),
                dir,
            )

            val configured = assertInstanceOf(ProxyEnvironment.Configured::class.java, result, raw)
            assertEquals(listOf("lobby", "hub"), configured.fallbackGroups, raw)
        }
    }

    @Test
    fun `an unreadable agent directory is dormant before the proxy variables are read`(@TempDir dir: Path) {
        // No ca.crt, no token: Environment's own check still comes first.
        val result = ProxyEnvironment.from(
            env(
                "SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443",
                "SPAWNERY_PLAYER_LIMIT" to "500",
                "SPAWNERY_FALLBACK_GROUPS" to "lobby",
            ),
            dir,
        )

        val dormant = assertInstanceOf(ProxyEnvironment.Dormant::class.java, result)
        assertTrue(dormant.reason.contains("ca.crt"), dormant.reason)
    }

    private fun configured(dir: Path, vararg extra: Pair<String, String>): ProxyEnvironment.Configured {
        mount(dir)
        val result = ProxyEnvironment.from(
            env(
                "SPAWNERY_OPERATOR_ENDPOINT" to "operator:9443",
                "SPAWNERY_PLAYER_LIMIT" to "500",
                "SPAWNERY_FALLBACK_GROUPS" to "lobby",
                *extra,
            ),
            dir,
        )
        return assertInstanceOf(ProxyEnvironment.Configured::class.java, result)
    }

    @Test
    fun `without the transfer variables there is no transfer and nothing to report`(@TempDir dir: Path) {
        val configured = configured(dir)

        assertNull(configured.transfer)
        assertNull(configured.transferOff)
    }

    @Test
    fun `both transfer variables and a readable secret configure transfers`(@TempDir dir: Path) {
        val secret = dir.resolve("forwarding.secret")
        Files.writeString(secret, "s3cret\n")

        val configured = configured(
            dir,
            "SPAWNERY_TRANSFER_FORCE_AFTER_SECONDS" to "90",
            "SPAWNERY_FORWARDING_SECRET_FILE" to secret.toString(),
        )

        val transfer = assertNotNull(configured.transfer)
        assertEquals(90L, transfer.forceAfterSeconds)
        assertEquals("s3cret", String(transfer.secret))
        assertNull(configured.transferOff)
    }

    @Test
    fun `only one transfer variable leaves the proxy serving with transfers off`(@TempDir dir: Path) {
        val configured = configured(dir, "SPAWNERY_TRANSFER_FORCE_AFTER_SECONDS" to "90")

        assertNull(configured.transfer)
        val reason = assertNotNull(configured.transferOff)
        assertTrue(reason.contains("SPAWNERY_FORWARDING_SECRET_FILE"), reason)
    }

    @Test
    fun `a bad deadline leaves the proxy serving with transfers off`(@TempDir dir: Path) {
        val secret = dir.resolve("forwarding.secret")
        Files.writeString(secret, "s3cret")

        for (raw in listOf("soon", "-1", "")) {
            val configured = configured(
                dir,
                "SPAWNERY_TRANSFER_FORCE_AFTER_SECONDS" to raw,
                "SPAWNERY_FORWARDING_SECRET_FILE" to secret.toString(),
            )

            assertNull(configured.transfer, raw)
            val reason = assertNotNull(configured.transferOff, raw)
            assertTrue(reason.contains("SPAWNERY_TRANSFER_FORCE_AFTER_SECONDS"), reason)
        }
    }

    @Test
    fun `an unreadable or empty secret leaves the proxy serving with transfers off`(@TempDir dir: Path) {
        val empty = dir.resolve("empty.secret")
        Files.writeString(empty, "\n")

        for (path in listOf(dir.resolve("missing.secret"), empty)) {
            val configured = configured(
                dir,
                "SPAWNERY_TRANSFER_FORCE_AFTER_SECONDS" to "90",
                "SPAWNERY_FORWARDING_SECRET_FILE" to path.toString(),
            )

            assertNull(configured.transfer, path.toString())
            val reason = assertNotNull(configured.transferOff, path.toString())
            assertTrue(reason.contains(path.toString()), reason)
        }
    }
}
