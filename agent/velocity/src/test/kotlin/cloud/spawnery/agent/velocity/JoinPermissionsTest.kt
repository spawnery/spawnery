package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.JoinRule
import cloud.spawnery.agent.PermissionValue
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.util.UUID

class JoinPermissionsTest {
    private val player = UUID.fromString("00000000-0000-0000-0000-000000000001")
    private val rules = mapOf("vip" to JoinRule("network.vip", denyOnly = false))

    @Test
    fun `a group without a rule is open and nobody is asked`() {
        var asked = 0
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(
            PermissionLookup { _, _, _ ->
                asked++
                null
            },
        ))
        assertTrue(access.mayJoin(player, "lobby-1", "lobby"))
        assertEquals(0, asked)
    }

    @Test
    fun `the lookup is asked in the target server's contexts`() {
        var asked: Map<String, String>? = null
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(
            PermissionLookup { _, node, contexts ->
                asked = contexts
                if (node == "network.vip") PermissionValue.TRUE else PermissionValue.UNDEFINED
            },
        ))
        assertTrue(access.mayJoin(player, "vip-x1", "vip"))
        assertEquals(
            mapOf("server" to "vip-x1", "group" to "vip", "network" to "tutorial", "environment" to "paper"),
            asked,
        )
    }

    @Test
    fun `a lookup that cannot answer hands over to the next`() {
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(
            PermissionLookup { _, _, _ -> null },
            PermissionLookup { _, _, _ -> PermissionValue.FALSE },
        ))
        assertFalse(access.mayJoin(player, "vip-x1", "vip"))
    }

    @Test
    fun `no lookup answering reads as undefined`() {
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(PermissionLookup { _, _, _ -> null }))
        assertFalse(access.mayJoin(player, "vip-x1", "vip"))
    }

    @Test
    fun `a lookup that throws counts as no answer`() {
        val access = JoinPermissions(rules::get, { "tutorial" }, listOf(
            PermissionLookup { _, _, _ -> throw IllegalStateException("LuckPerms is not loaded") },
            PermissionLookup { _, _, _ -> PermissionValue.TRUE },
        ))
        assertTrue(access.mayJoin(player, "vip-x1", "vip"))
    }
}
