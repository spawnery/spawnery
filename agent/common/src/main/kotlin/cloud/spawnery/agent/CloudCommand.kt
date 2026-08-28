package cloud.spawnery.agent

import cloud.spawnery.agent.api.ServerInfo
import cloud.spawnery.agent.api.SpawneryApi
import com.mojang.brigadier.arguments.StringArgumentType
import com.mojang.brigadier.builder.LiteralArgumentBuilder
import com.mojang.brigadier.builder.RequiredArgumentBuilder

/** The permission a source needs to read anything. */
const val PERMISSION_READ: String = "spawnery.cloud.read"

/**
 * The `/cloud` command, written once for both platforms.
 *
 * Generic in the source type, and nothing in this file names Paper's
 * `CommandSourceStack` or Velocity's `CommandSource`. What a platform can be
 * asked for is [SourceAdapter]'s two methods and nothing else, which is what
 * makes "the same command answers the same way on both sides" a property of
 * the code rather than of two implementations kept in step.
 *
 * **It reads only the local mirror.** Every branch here is a lookup in memory,
 * so a command cannot block a platform's main thread, time out, or fail
 * because the operator is unreachable -- which is the promise [SpawneryApi]
 * makes and the reason this is safe to run from a chat message.
 */
fun <S> cloudCommand(api: SpawneryApi, adapter: SourceAdapter<S>): LiteralArgumentBuilder<S> =
    LiteralArgumentBuilder.literal<S>("cloud")
        // On the root as well as on each branch: without it, `/cloud` with no
        // arguments would be visible to everybody and answer with usage for
        // subcommands they cannot see.
        .requires { adapter.hasPermission(it, PERMISSION_READ) }
        .then(
            LiteralArgumentBuilder.literal<S>("list")
                .requires { adapter.hasPermission(it, PERMISSION_READ) }
                .executes { ctx ->
                    val groups = api.groups()
                    if (groups.isEmpty()) {
                        // Said rather than answered with silence. An empty
                        // network and an operator this agent has not heard
                        // from look identical otherwise, and the second is the
                        // one somebody needs to act on.
                        adapter.send(ctx.source, "no groups on this network yet")
                    }
                    for (group in groups) {
                        adapter.send(
                            ctx.source,
                            "${group.name()} (${group.kind()}): ${group.readyReplicas()}/${group.replicas()} ready, " +
                                "${group.onlinePlayers()} players, ${group.freeSlots()} free slots",
                        )
                    }
                    groups.size
                },
        )
        .then(
            LiteralArgumentBuilder.literal<S>("info")
                .requires { adapter.hasPermission(it, PERMISSION_READ) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("name", StringArgumentType.word())
                        .executes { ctx ->
                            val name = StringArgumentType.getString(ctx, "name")
                            val server = api.server(name)
                            if (server.isPresent) {
                                adapter.send(ctx.source, describe(server.get()))
                                return@executes 1
                            }
                            val group = api.group(name)
                            if (group.isPresent) {
                                val g = group.get()
                                adapter.send(
                                    ctx.source,
                                    "${g.name()} (${g.kind()}): ${g.readyReplicas()}/${g.replicas()} ready, " +
                                        "${g.onlinePlayers()} players, ${g.freeSlots()} free slots",
                                )
                                return@executes 1
                            }
                            // Names what was asked for. A bare "not found"
                            // leaves an admin unsure whether they mistyped or
                            // the thing is gone, and an empty line leaves them
                            // unsure whether the command works at all.
                            adapter.send(ctx.source, "no server or group called \"$name\" on this network")
                            0
                        },
                ),
        )

private fun describe(server: ServerInfo): String =
    "${server.name()} in ${server.group()}: ${server.phase()}, " +
        "${server.players()}/${server.slots()} players, " +
        // Registered and not the phase, because they disagree during a drain
        // and this is the one that says whether anybody new can reach it.
        (if (server.registered()) "taking joins" else "not taking joins")
