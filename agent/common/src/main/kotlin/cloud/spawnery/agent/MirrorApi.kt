package cloud.spawnery.agent

import cloud.spawnery.agent.api.CloudPlayer
import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.api.BoostResult
import cloud.spawnery.agent.api.ConnectResult
import cloud.spawnery.agent.api.EventBus
import cloud.spawnery.agent.api.Self
import cloud.spawnery.agent.api.Target
import cloud.spawnery.agent.api.ServerInfo
import cloud.spawnery.agent.api.SpawneryApi
import java.util.Optional
import java.time.Duration
import java.util.UUID
import java.util.concurrent.CompletionStage

/**
 * The one implementation of [SpawneryApi], for both platforms.
 *
 * **This is where the design's central promise stops being a claim.** A plugin
 * author moving between a Paper backend and a Velocity proxy gets the same
 * answers because it is the same code -- not because two implementations were
 * written to agree and a test checks that they still do. The only
 * platform-specific input is [self], and it is a constructor argument rather
 * than a branch: nothing in this file may ask which side it is on, and
 * `MirrorApiTest` asserts that every read answers identically whichever [Self]
 * it was built with.
 *
 * The reads never block and never fail. [NetworkMirror] hands back an
 * immutable snapshot, so a caller on Bukkit's main thread pays one volatile
 * read -- which is what lets [SpawneryApi] promise no timeout and no
 * exception, and what makes it safe to call from an event handler.
 *
 * A lookup returns [Optional] and not null for the reason the interface's own
 * javadoc gives: a plugin that forgets a null check meets an NPE at some later
 * line, where an empty Optional refuses at the point of use.
 */
class MirrorApi(
    private val mirror: NetworkMirror,
    private val self: Self,
    /**
     * How a request reaches the operator. Last, for the reason every other
     * added parameter in these packages is.
     */
    private val connector: CloudConnector,
    /**
     * Where a plugin's own listeners live. Last, as ever -- a parameter added
     * above would rebind every positional caller, silently wherever the types
     * happen to match.
     */
    private val events: CloudEvents,
) : SpawneryApi {
    override fun self(): Self = self

    override fun groups(): List<Group> = mirror.groups()

    override fun group(name: String): Optional<Group> =
        Optional.ofNullable(mirror.groups().firstOrNull { it.name() == name })

    override fun servers(): List<ServerInfo> = mirror.servers()

    override fun server(name: String): Optional<ServerInfo> =
        Optional.ofNullable(mirror.servers().firstOrNull { it.name() == name })

    override fun players(): List<CloudPlayer> = mirror.players()

    override fun player(id: UUID): Optional<CloudPlayer> =
        Optional.ofNullable(mirror.players().firstOrNull { it.id() == id })

    // Delegated whole. Nothing here asks which side it is on, which is the
    // invariant this class exists to hold.
    override fun connect(player: UUID, to: Target): CompletionStage<ConnectResult> =
        connector.connect(player, to)

    override fun retire(server: String): CompletionStage<Void> =
        connector.retire(server)

    override fun boost(group: String, replicas: Int, forHowLong: Duration?): CompletionStage<BoostResult> =
        connector.boost(group, replicas, forHowLong)

    override fun stopBoosts(group: String): CompletionStage<Int> =
        connector.stopBoosts(group)

    override fun events(): EventBus = events
}
