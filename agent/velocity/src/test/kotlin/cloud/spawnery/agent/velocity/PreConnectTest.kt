package cloud.spawnery.agent.velocity

import com.velocitypowered.api.proxy.server.ServerInfo
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Test
import java.net.InetSocketAddress

class PreConnectTest {
    private val alternative = FakeRegistry().register(ServerInfo("lobby-1", InetSocketAddress("10.0.0.2", 25565)))

    @Test
    fun `an allowed connect is kept and nobody is asked for an alternative`() {
        val decision = decidePreConnect(allowed = true, onServer = false) {
            error("an allowed connect needs no alternative")
        }

        assertEquals(PreConnectDecision.Keep, decision)
    }

    @Test
    fun `a denied switch from a server is denied so the player stays where they are`() {
        val decision = decidePreConnect(allowed = false, onServer = true) {
            error("a player on a server is not moved")
        }

        assertEquals(PreConnectDecision.Deny, decision)
    }

    @Test
    fun `a denied initial connect is redirected to an alternative`() {
        val decision = decidePreConnect(allowed = false, onServer = false) { alternative }

        assertSame(alternative, (decision as PreConnectDecision.Redirect).server)
    }

    @Test
    fun `a denied initial connect with nothing left is disconnected`() {
        val decision = decidePreConnect(allowed = false, onServer = false) { null }

        assertEquals(PreConnectDecision.Disconnect, decision)
    }
}
