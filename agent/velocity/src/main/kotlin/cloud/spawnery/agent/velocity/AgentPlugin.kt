package cloud.spawnery.agent.velocity

import com.google.inject.Inject
import com.velocitypowered.api.event.Subscribe
import com.velocitypowered.api.event.proxy.ProxyInitializeEvent
import com.velocitypowered.api.event.proxy.ProxyShutdownEvent
import com.velocitypowered.api.plugin.Plugin
import com.velocitypowered.api.proxy.ProxyServer
import org.slf4j.Logger
import java.nio.file.Path

/**
 * The only class in this plugin that touches the Velocity API.
 *
 * That is not tidiness for its own sake: it is what lets every other unit be
 * tested with JUnit and no proxy. Nothing here decides anything — the
 * decisions live in [ProxyEnvironment], [ReadyGate] and, from task 7, the
 * session.
 *
 * The annotation is still required even though Velocity reads this plugin's
 * identity out of `velocity-plugin.json`: the descriptor is what makes the jar
 * a plugin, and the annotation is what marks the class Guice instantiates.
 */
@Plugin(
    id = "spawnery",
    name = "Spawnery Agent",
    // The descriptor in velocity-plugin.json carries the real version,
    // expanded from -PagentVersion by processResources; Velocity reads it from
    // there and this value is never consulted. A literal here rather than a
    // build-time constant, because an annotation argument must be a compile
    // time constant and a generated one would be a second version to keep in
    // step with the first.
    version = "0.0.0",
)
class AgentPlugin @Inject constructor(
    private val proxy: ProxyServer,
    private val logger: Logger,
) {
    private var gate: ReadyGate? = null

    @Subscribe
    fun onInitialize(event: ProxyInitializeEvent) {
        when (val env = ProxyEnvironment.from(System::getenv, Path.of(AGENT_DIR))) {
            is ProxyEnvironment.Dormant -> {
                logger.info("spawnery agent dormant: ${env.reason}")
                return
            }

            is ProxyEnvironment.Configured -> {
                // Constructed, not opened. The gate is what makes this pod
                // ready, and a proxy is not ready until it has a server list;
                // task 7 opens it on the first FullSync. Until then this
                // plugin loads, logs and does nothing, which is exactly the
                // claim hack/velocity-image-test.sh makes.
                gate = ReadyGate(READY_PORT) { message, error -> logger.warn(message, error) }
                logger.info("spawnery agent connecting to ${env.base.endpoint}")
            }
        }
    }

    @Subscribe
    fun onShutdown(event: ProxyShutdownEvent) {
        gate?.close()
    }

    private companion object {
        // internal/podspec.AgentMountPath. Hard-coded rather than configurable:
        // the operator creates these pods and mounts exactly here, and a second
        // place to spell it would be a second place to get it wrong.
        const val AGENT_DIR = "/var/run/spawnery"

        // internal/podspec.ProxyReadyPort, for the same reason.
        const val READY_PORT = 8081
    }
}
