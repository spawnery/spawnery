package cloud.spawnery.agent.paper

import org.bukkit.event.Listener
import org.junit.jupiter.api.Test
import kotlin.test.assertEquals

class LoginGateSwitchTest {
    private val live = mutableListOf<Listener>()
    private val gate = LoginGateSwitch(
        register = { object : Listener {}.also { live += it } },
        unregister = { live -= it },
    )

    @Test
    fun `the check is registered once while wanted and removed when the rule goes`() {
        assertEquals(LoginGateSwitch.Change.NONE, gate.update(false))
        assertEquals(LoginGateSwitch.Change.REGISTERED, gate.update(true))
        assertEquals(LoginGateSwitch.Change.NONE, gate.update(true))
        assertEquals(1, live.size)

        assertEquals(LoginGateSwitch.Change.UNREGISTERED, gate.update(false))
        assertEquals(emptyList(), live)
    }

    @Test
    fun `a rule that comes back registers the check again`() {
        gate.update(true)
        gate.update(false)

        assertEquals(LoginGateSwitch.Change.REGISTERED, gate.update(true))
        assertEquals(1, live.size)
    }
}
