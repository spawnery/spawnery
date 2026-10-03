package cloud.spawnery.agent

import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

class JoinRulesTest {
    private val required = JoinRule("network.vip", denyOnly = false)
    private val denyOnly = JoinRule("network.banned", denyOnly = true)

    @Test
    fun `without a rule everybody may join`() {
        for (value in PermissionValue.entries) {
            assertEquals(true, JoinRules.mayJoin(null, value), "$value")
        }
    }

    @Test
    fun `required admits only a granted node`() {
        assertEquals(true, JoinRules.mayJoin(required, PermissionValue.TRUE))
        assertEquals(false, JoinRules.mayJoin(required, PermissionValue.FALSE))
        assertEquals(false, JoinRules.mayJoin(required, PermissionValue.UNDEFINED))
    }

    @Test
    fun `deny only refuses only an explicit false`() {
        assertEquals(true, JoinRules.mayJoin(denyOnly, PermissionValue.TRUE))
        assertEquals(false, JoinRules.mayJoin(denyOnly, PermissionValue.FALSE))
        assertEquals(true, JoinRules.mayJoin(denyOnly, PermissionValue.UNDEFINED))
    }

    @Test
    fun `the contexts are the target server's, as a backend registers them`() {
        assertEquals(
            mapOf("server" to "vip-x1", "group" to "vip", "network" to "tutorial", "environment" to "paper"),
            JoinRules.contexts("vip-x1", "vip", "tutorial"),
        )
    }

    @Test
    fun `a blank context is left out rather than matched as empty`() {
        assertEquals(
            mapOf("server" to "vip-x1", "group" to "vip", "environment" to "paper"),
            JoinRules.contexts("vip-x1", "vip", ""),
        )
    }
}
