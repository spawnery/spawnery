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
 * Velocity reads this plugin's identity entirely out of
 * `velocity-plugin.json`; its `main` field is what names this class, and Guice
 * constructs it from there. Nothing at runtime reads the annotation below —
 * measured 2026-08-11 by scanning every class in the proxy jar for a reference
 * to `com/velocitypowered/api/plugin/Plugin`, which found exactly three, all
 * compile-time machinery: the annotation itself, `PluginAnnotationProcessor`
 * and `SerializedPluginDescription`. It has `RUNTIME` retention and no reader.
 *
 * It is kept anyway, for two reasons that are not "Velocity requires it": it
 * puts the plugin's identity in the source rather than only in a resource
 * file, and it is the input the annotation processor would consume if anyone
 * ever took the kapt route this project declined. See [version].
 */
@Plugin(
    id = "spawnery",
    name = "Spawnery Agent",
    // Never read by anything. The descriptor in velocity-plugin.json carries
    // the real version, expanded from -PagentVersion by processResources, and
    // that is the one Velocity reports and the agent sends as Hello.version.
    // A literal here rather than a build-time constant, because an annotation
    // argument must be a compile-time constant.
    //
    // The trap, for whoever adds kapt: Velocity's annotation processor
    // generates velocity-plugin.json to this same path from these same
    // arguments. Adding kapt *alongside* the hand-written resource would ship
    // a descriptor carrying this literal 0.0.0, no description and no authors
    // -- and it would not fail anything. Both descriptor guards in
    // hack/agent-jar-check.sh still pass: a "version": key is present, and no
    // ${ placeholder remains to be caught. The operator would simply record
    // every proxy in the fleet as running version 0.0.0. Taking that route
    // means deleting src/main/resources/velocity-plugin.json and making this
    // argument the real version, never doing one without the other.
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
