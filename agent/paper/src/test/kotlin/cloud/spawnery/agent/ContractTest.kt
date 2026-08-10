package cloud.spawnery.agent

import cloud.spawnery.agent.pb.Hello
import cloud.spawnery.agent.pb.PlayerCount
import cloud.spawnery.agent.pb.ServerMessage
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

/**
 * The Kotlin counterpart of internal/agentpb/contract_test.go. It does not test
 * protobuf; it tests that the checked-in stubs were generated from the .proto
 * this repository holds, which is the thing that silently rots.
 */
class ContractTest {
    @Test
    fun `a server message round-trips through the wire format`() {
        val sent = ServerMessage.newBuilder()
            .setHello(Hello.newBuilder().setVersion("26.2-0.2.0").setReady(true))
            .build()

        val back = ServerMessage.parseFrom(sent.toByteArray())

        assertEquals(ServerMessage.MessageCase.HELLO, back.messageCase)
        assertEquals("26.2-0.2.0", back.hello.version)
        assertEquals(true, back.hello.ready)
    }

    @Test
    fun `player count carries both numbers`() {
        val sent = ServerMessage.newBuilder()
            .setPlayerCount(PlayerCount.newBuilder().setPlayers(3).setSlots(100))
            .build()

        val back = ServerMessage.parseFrom(sent.toByteArray())

        assertEquals(3, back.playerCount.players)
        assertEquals(100, back.playerCount.slots)
    }
}
