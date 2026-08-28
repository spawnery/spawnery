package cloud.spawnery.agent

import cloud.spawnery.agent.api.ServerInfo
import cloud.spawnery.agent.api.SpawneryApi
import com.mojang.brigadier.arguments.IntegerArgumentType
import com.mojang.brigadier.arguments.StringArgumentType
import com.mojang.brigadier.builder.LiteralArgumentBuilder
import com.mojang.brigadier.builder.RequiredArgumentBuilder
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter

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
 * The permission a source needs to change how much capacity a group has.
 *
 * One permission for `start` and `stop` rather than two: somebody trusted to
 * add servers is trusted to take back what they added, and a grant that let a
 * person start boosts without ending them would leave them no way to undo
 * their own mistake.
 */
const val PERMISSION_SCALE: String = "spawnery.cloud.scale"

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
            adapter.hasPermission(it, PERMISSION_READ) ||
                adapter.hasPermission(it, PERMISSION_RETIRE) ||
                adapter.hasPermission(it, PERMISSION_SCALE)
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

        .then(
            LiteralArgumentBuilder.literal<S>("start")
                .requires { adapter.hasPermission(it, PERMISSION_SCALE) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("group", StringArgumentType.word())
                        // Without a count, and the default is one. A person
                        // typing `/cloud start lobby` in a hurry means "one
                        // more", and refusing them for a missing argument
                        // would be pedantry at the exact moment they are busy.
                        .executes { ctx -> startBoost(api, adapter, ctx.source, group(ctx), 1, null) }
                        .then(
                            RequiredArgumentBuilder.argument<S, Int>("count", IntegerArgumentType.integer(1))
                                .executes { ctx ->
                                    startBoost(api, adapter, ctx.source, group(ctx), count(ctx), null)
                                }
                                .then(
                                    LiteralArgumentBuilder.literal<S>("for")
                                        .then(
                                            RequiredArgumentBuilder.argument<S, String>(
                                                "duration",
                                                StringArgumentType.word(),
                                            ).executes { ctx ->
                                                val text = StringArgumentType.getString(ctx, "duration")
                                                val span = parseDuration(text)
                                                if (span == null) {
                                                    adapter.send(
                                                        ctx.source,
                                                        "could not read \"$text\" as a length of time. " +
                                                            "Try 30m, 2h, or 90s.",
                                                    )
                                                    return@executes 0
                                                }
                                                startBoost(api, adapter, ctx.source, group(ctx), count(ctx), span)
                                            },
                                        ),
                                ),
                        ),
                ),
        )
        .then(
            LiteralArgumentBuilder.literal<S>("stop")
                .requires { adapter.hasPermission(it, PERMISSION_SCALE) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("group", StringArgumentType.word())
                        .executes { ctx ->
                            val name = group(ctx)
                            val source = ctx.source
                            api.stopBoosts(name).whenComplete { removed, failure ->
                                when {
                                    failure != null ->
                                        adapter.send(source, "could not stop boosts on $name: ${reason(failure)}")
                                    // Zero said plainly rather than dressed up
                                    // as a success. An admin who expected
                                    // boosts has to learn there were none,
                                    // because the next thing they do depends
                                    // on it.
                                    removed == 0 ->
                                        adapter.send(source, "$name had no boosts running")
                                    else -> adapter.send(
                                        source,
                                        "$name: removed $removed boost${if (removed == 1) "" else "s"}. " +
                                            "The group returns to its own floor as servers empty.",
                                    )
                                }
                            }
                            1
                        },
                ),
        )

private fun <S> group(ctx: com.mojang.brigadier.context.CommandContext<S>): String =
    StringArgumentType.getString(ctx, "group")

private fun <S> count(ctx: com.mojang.brigadier.context.CommandContext<S>): Int =
    IntegerArgumentType.getInteger(ctx, "count")

/**
 * Sends the boost and says the three things section 5.3 requires.
 *
 * The second and third lines are not decoration. The second says it is
 * temporary and how to end it early; the third points at the thing a person
 * should edit when the need is not temporary -- because this command
 * deliberately cannot change desired state, and an admin who does not know
 * that will type it again next week and every week after.
 *
 * A command that permanently changes desired state while looking like a
 * one-shot nudge is the class of surprise this repository avoids everywhere
 * else. That is what these lines buy.
 */
private fun <S> startBoost(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    source: S,
    group: String,
    replicas: Int,
    forHowLong: java.time.Duration?,
): Int {
    api.boost(group, replicas, forHowLong).whenComplete { result, failure ->
        if (failure != null) {
            adapter.send(source, "could not boost $group: ${reason(failure)}")
            return@whenComplete
        }
        adapter.send(
            source,
            "$group: +${result.replicas()} server${if (result.replicas() == 1) "" else "s"} " +
                "until ${AT_MINUTE_UTC.format(result.expiresAt())} UTC",
        )
        adapter.send(
            source,
            "This is a boost, not a spec change. It expires on its own; /cloud stop $group ends it early.",
        )
        adapter.send(source, "For a lasting change, edit the ServerGroup.")
    }
    return 1
}

/** The expiry, to the minute, on the operator's clock. */
private val AT_MINUTE_UTC: DateTimeFormatter =
    DateTimeFormatter.ofPattern("HH:mm").withZone(ZoneOffset.UTC)

/**
 * Reads `30m`, `2h`, `90s` and nothing else.
 *
 * Deliberately not java.time's ISO-8601 parser, which would want `PT30M`.
 * Nobody types that into a chat window, and a command that demanded it would
 * be a command people stop using. Null for anything it cannot read, so the
 * caller can name what it could not read rather than guessing a default --
 * silently treating `2hh` as the default hour is how somebody ends up
 * believing they set a length they did not.
 */
internal fun parseDuration(text: String): java.time.Duration? {
    if (text.length < 2) return null
    val amount = text.dropLast(1).toLongOrNull() ?: return null
    if (amount <= 0) return null
    return when (text.last()) {
        's' -> java.time.Duration.ofSeconds(amount)
        'm' -> java.time.Duration.ofMinutes(amount)
        'h' -> java.time.Duration.ofHours(amount)
        else -> null
    }
}

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
