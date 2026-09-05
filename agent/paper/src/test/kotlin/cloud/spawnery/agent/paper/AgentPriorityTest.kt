package cloud.spawnery.agent.paper

import java.nio.file.Path
import kotlin.io.path.readText
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

/**
 * Read from the source rather than from the annotation: loading AgentPlugin
 * pulls in Paper's API, which is compiled for a newer Java than this test JVM,
 * and the class cannot be loaded here at all.
 */
class AgentPriorityTest {
    @Test
    fun `the agent reads the load event after every other handler`() {
        val source = Path.of("src/main/kotlin/cloud/spawnery/agent/paper/AgentPlugin.kt").readText()

        assertTrue(
            source.contains(
                "@EventHandler(priority = EventPriority.MONITOR)\n    fun onServerLoad(",
            ),
            "a plugin that holds readiness from its own ServerLoadEvent handler " +
                "must have spoken before the agent decides; on a bare @EventHandler " +
                "that is plugin registration order",
        )
    }
}
