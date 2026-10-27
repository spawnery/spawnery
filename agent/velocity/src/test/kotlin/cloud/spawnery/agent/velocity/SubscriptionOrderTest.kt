package cloud.spawnery.agent.velocity

import com.velocitypowered.api.event.Subscribe
import com.velocitypowered.api.event.player.ServerPreConnectEvent
import org.junit.jupiter.api.Test
import kotlin.test.assertEquals

class SubscriptionOrderTest {
    @Test
    fun `the switch decision runs after every other plugin has set its result`() {
        val subscribe = AgentPlugin::class.java
            .getMethod("onServerPreConnect", ServerPreConnectEvent::class.java)
            .getAnnotation(Subscribe::class.java)

        assertEquals(Short.MIN_VALUE, subscribe.priority)
    }

    @Test
    fun `the join rule is checked after every other plugin's redirect, and before the switch decision`() {
        val joinRule = AgentPlugin::class.java
            .getMethod("onServerPreConnectJoinRule", ServerPreConnectEvent::class.java)
            .getAnnotation(Subscribe::class.java)

        assertEquals((Short.MIN_VALUE + 1).toShort(), joinRule.priority)
    }
}
