package cloud.spawnery.agent

import cloud.spawnery.agent.api.ServerInfo
import cloud.spawnery.agent.api.SpawneryApi
import com.mojang.brigadier.arguments.StringArgumentType
import com.mojang.brigadier.builder.LiteralArgumentBuilder
import com.mojang.brigadier.builder.RequiredArgumentBuilder

/** The permission a source needs to read anything. */
const val PERMISSION_READ: String = "spawnery.cloud.read"

/**
 * The permission a source needs to retire a server.
 *
 * Its own node rather than a level above [PERMISSION_READ], because the two
 * are not the same kind of thing: reading is what you give a moderator so they
 * can see where people are, and retiring changes the fleet. A permission
 * system that made one imply the other would hand every moderator the second
 * the day somebody granted the first.
 */
const val PERMISSION_RETIRE: String = "spawnery.cloud.retire"

/**
 * The `/cloud` command, written once for both platforms.
 *
 * Generic in the source type, and nothing in this file names Paper's
 * `CommandSourceStack` or Velocity's `CommandSource`. What a platform can be
 * asked for is [SourceAdapter]'s two methods and nothing else, which is what
 * makes "the same command answers the same way on both sides" a property of
 * the code rather than of two implementations kept in step.
 *
 * **The reading branches read only the local mirror.** `list` and `info` are
 * lookups in memory, so they cannot block a platform's main thread, time out,
 * or fail because the operator is unreachable -- which is the promise
 * [SpawneryApi] makes and the reason they are safe to run from a chat message.
 *
 * `retire` is the exception and cannot be otherwise: it changes an object in
 * the cluster. It does not block either -- it hands the request off and
 * answers from the completion, which arrives on a gRPC thread. Both platforms'
 * adapters send to an audience rather than touching the world, which is what
 * makes that safe, and it is the reason [SourceAdapter] has exactly those two
 * methods and no way to ask for anything a main thread would have to own.
 */
fun <S> cloudCommand(api: SpawneryApi, adapter: SourceAdapter<S>): LiteralArgumentBuilder<S> =
    LiteralArgumentBuilder.literal<S>("cloud")
        // On the root as well as on each branch: without it, `/cloud` with no
        // arguments would be visible to everybody and answer with usage for
        // subcommands they cannot see.
        //
        // Either permission opens the root, not just the reading one. A root
        // that demanded PERMISSION_READ would hide the whole tree from
        // somebody granted only PERMISSION_RETIRE -- and hide it in the worst
        // way, by making the command look as though it does not exist. The
        // branches still gate themselves, so this widens what is visible and
        // nothing else.
        .requires {
            adapter.hasPermission(it, PERMISSION_READ) || adapter.hasPermission(it, PERMISSION_RETIRE)
        }
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

        .then(
            LiteralArgumentBuilder.literal<S>("retire")
                // Its own permission, and deliberately not PERMISSION_READ:
                // see PERMISSION_RETIRE.
                .requires { adapter.hasPermission(it, PERMISSION_RETIRE) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("name", StringArgumentType.word())
                        .executes { ctx ->
                            val name = StringArgumentType.getString(ctx, "name")
                            val source = ctx.source
                            api.retire(name).whenComplete { _, failure ->
                                if (failure == null) {
                                    // The second sentence is not decoration.
                                    // "Retire" reads as "stop" to anybody who
                                    // has not read the design, and an admin
                                    // who believes they just disconnected
                                    // forty people does something worse next.
                                    adapter.send(
                                        source,
                                        "$name is retiring. It takes no new joins; the players on it " +
                                            "finish in their own time and nobody is kicked.",
                                    )
                                } else {
                                    // The operator's own words. Every refusal
                                    // it sends is written for a person --
                                    // already retiring, no such server, asked
                                    // too often -- and rewording them here
                                    // would only lose which one it was.
                                    adapter.send(source, "could not retire $name: ${reason(failure)}")
                                }
                            }
                            // One, meaning the request went out -- not that it
                            // worked. Brigadier wants a number before the
                            // answer can exist, so this is the only honest
                            // thing it can be.
                            1
                        },
                ),
        )

// The operator's message, unwrapped from the plumbing a plugin author would
// otherwise read in chat: a CompletionStage reports failures wrapped in
// CompletionException, and "java.util.concurrent.CompletionException:
// java.lang.IllegalStateException: REFUSED: ..." is not a sentence anybody
// wants in a chat line.
private fun reason(failure: Throwable): String {
    val cause = if (failure is java.util.concurrent.CompletionException && failure.cause != null) {
        failure.cause!!
    } else {
        failure
    }
    return cause.message ?: cause.javaClass.simpleName
}

private fun describe(server: ServerInfo): String =
    "${server.name()} in ${server.group()}: ${server.phase()}, " +
        "${server.players()}/${server.slots()} players, " +
        // Registered and not the phase, because they disagree during a drain
        // and this is the one that says whether anybody new can reach it.
        (if (server.registered()) "taking joins" else "not taking joins")
