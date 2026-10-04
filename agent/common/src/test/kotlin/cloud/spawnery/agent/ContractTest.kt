package cloud.spawnery.agent

import cloud.spawnery.agent.pb.ExecuteCommand
import cloud.spawnery.agent.pb.ExecuteOutcome
import cloud.spawnery.agent.pb.Hello
import cloud.spawnery.agent.pb.OperatorToServer
import cloud.spawnery.agent.pb.PlayerCount
import cloud.spawnery.agent.pb.ServerMessage
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test

/** Kotlin counterpart of internal/agentpb/contract_test.go: the checked-in stubs match the .proto. */
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

    @Test
    fun `an execute command and its outcome round-trip with their id`() {
        val down = OperatorToServer.newBuilder()
            .setExecuteCommand(ExecuteCommand.newBuilder().setId(7).setCommand("list"))
            .build()
        val downBack = OperatorToServer.parseFrom(down.toByteArray())
        assertEquals(OperatorToServer.MessageCase.EXECUTE_COMMAND, downBack.messageCase)
        assertEquals(7L, downBack.executeCommand.id)

        val up = ServerMessage.newBuilder()
            .setExecuteOutcome(ExecuteOutcome.newBuilder().setId(7).setOk(true).addOutput("There are 0 players"))
            .build()
        val upBack = ServerMessage.parseFrom(up.toByteArray())
        assertEquals(7L, upBack.executeOutcome.id)
        assertEquals(listOf("There are 0 players"), upBack.executeOutcome.outputList)
    }
}
