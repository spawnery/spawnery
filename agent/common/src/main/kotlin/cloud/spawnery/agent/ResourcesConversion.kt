package cloud.spawnery.agent

import cloud.spawnery.agent.api.ServerResources
import cloud.spawnery.agent.pb.StartResources
import java.util.Optional

internal fun toServerResources(pb: StartResources): ServerResources =
    ServerResources(
        optional(pb.memoryRequest),
        optional(pb.memoryLimit),
        optional(pb.cpuRequest),
        optional(pb.cpuLimit),
        optional(pb.heap),
    )

/** Null when nothing is set, so the request carries no `resources` message at all. */
internal fun toStartResources(resources: ServerResources): StartResources? {
    if (resources == ServerResources.NONE) return null
    return StartResources.newBuilder().apply {
        resources.memoryRequest().ifPresent { memoryRequest = it }
        resources.memoryLimit().ifPresent { memoryLimit = it }
        resources.cpuRequest().ifPresent { cpuRequest = it }
        resources.cpuLimit().ifPresent { cpuLimit = it }
        resources.heap().ifPresent { heap = it }
    }.build()
}

private fun optional(value: String): Optional<String> =
    if (value.isEmpty()) Optional.empty() else Optional.of(value)
