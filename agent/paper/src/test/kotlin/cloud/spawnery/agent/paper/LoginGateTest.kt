package cloud.spawnery.agent.paper

import cloud.spawnery.agent.PermissionValue
import net.kyori.adventure.text.Component
import net.kyori.adventure.text.TranslatableComponent
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class LoginGateTest {
    private fun admits(enforce: Boolean = true, bypass: Boolean = false, seated: Int, pending: Int = 0, playable: Int) =
        LoginGate.admits(enforce = enforce, bypass = bypass, seated = seated, pending = pending, playable = playable)

    @Test
    fun `not enforced admits a full server`() = assertTrue(admits(enforce = false, seated = 12, playable = 12))

    @Test
    fun `a free seat admits`() = assertTrue(admits(seated = 11, playable = 12))

    @Test
    fun `a full server refuses`() = assertFalse(admits(seated = 12, playable = 12))

    @Test
    fun `a bypass player gets onto a full server`() = assertTrue(admits(bypass = true, seated = 12, playable = 12))

    @Test
    fun `a player admitted but not yet joined takes a seat`() =
        assertFalse(admits(seated = 11, pending = 1, playable = 12))

    @Test
    fun `the plugin's value beats the group's`() = assertEquals(8, LoginGate.effectivePlayable(8, 12, 100))

    @Test
    fun `the group's value stands without the plugin's`() = assertEquals(12, LoginGate.effectivePlayable(0, 12, 100))

    @Test
    fun `without either the playable slots are max players`() =
        assertEquals(100, LoginGate.effectivePlayable(0, 0, 100))

    @Test
    fun `a value above max players is max players`() = assertEquals(100, LoginGate.effectivePlayable(500, 0, 100))

    @Test
    fun `the refusal is the translatable key with the group and a fallback`() {
        val c = LoginGate.refusal("Duels") as TranslatableComponent
        assertEquals("spawnery.join.full", c.key())
        assertEquals("This round is full.", c.fallback())
        assertEquals(Component.text("Duels"), c.arguments().single().asComponent())
    }

    @Test
    fun `the permission names the group`() = assertEquals("spawnery.join.full.duels", LoginGate.permission("duels"))

    @Test
    fun `a granted node reads as true, whether or not it is set`() {
        assertEquals(PermissionValue.TRUE, LoginGate.permissionValue(has = true, isSet = true))
        // An op on a node nobody set: Bukkit's default for ops is true.
        assertEquals(PermissionValue.TRUE, LoginGate.permissionValue(has = true, isSet = false))
    }

    @Test
    fun `an explicit false reads as false and an unset node as undefined`() {
        assertEquals(PermissionValue.FALSE, LoginGate.permissionValue(has = false, isSet = true))
        assertEquals(PermissionValue.UNDEFINED, LoginGate.permissionValue(has = false, isSet = false))
    }

    @Test
    fun `the refusal is translatable and names the group`() {
        val refusal = LoginGate.denied("VIP Lobby") as TranslatableComponent
        assertEquals("spawnery.join.denied", refusal.key())
        assertEquals("You may not join %s.", refusal.fallback())
        assertEquals(Component.text("VIP Lobby"), refusal.arguments().single().asComponent())
    }

    @Test
    fun `a group without a display name is named by its name`() {
        assertEquals("vip", LoginGate.displayName("vip", ""))
        assertEquals("vip", LoginGate.displayName("vip", null))
        assertEquals("VIP Lobby", LoginGate.displayName("vip", "VIP Lobby"))
    }
}
