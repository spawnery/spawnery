package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
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

// PERMISSION_EVENTS lives in Feed.kt, beside the thing that reads it: the feed
// asks for it once a tick to decide whether this agent wants events at all,
// and a permission split from its only reader is one that drifts.

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
fun <S> cloudCommand(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    feed: FeedState,
    /**
     * The same shape the chat feed uses, from the Network's own spec.
     *
     * One field and not two: a reply to a command and an announcement about
     * the cloud come from the same plugin, and a network that styles one
     * should not have to style the other to match. `$EVENT_MESSAGE` keeps its
     * name because it is what an installation already wrote down; what it
     * stands for is simply "what this line has to say".
     *
     * A lambda for the reason [Feed]'s own is one: the format arrives in the
     * NetworkState and an edit must land at the next resync rather than at the
     * next pod.
     */
    format: () -> String = { Feed.DEFAULT_FORMAT },
): LiteralArgumentBuilder<S> =
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
                adapter.hasPermission(it, PERMISSION_SCALE) ||
                adapter.hasPermission(it, PERMISSION_EVENTS)
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
                        reply(adapter, format, ctx.source, Style.quiet("no groups on this network yet"))
                    }
                    for (group in groups) {
                        reply(adapter, format, 
                            ctx.source,
                            describeGroup(group),
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
                                reply(adapter, format, ctx.source, describe(server.get()))
                                return@executes 1
                            }
                            val group = api.group(name)
                            if (group.isPresent) {
                                val g = group.get()
                                reply(adapter, format, 
                                    ctx.source,
                                    describeGroup(g),
                                )
                                return@executes 1
                            }
                            // Names what was asked for. A bare "not found"
                            // leaves an admin unsure whether they mistyped or
                            // the thing is gone, and an empty line leaves them
                            // unsure whether the command works at all.
                            reply(adapter, format, 
                                ctx.source,
                                Style.bad("no server or group called") + " " + Style.name(name) +
                                    Style.quiet(" on this network"),
                            )
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
                                    reply(adapter, format, 
                                        source,
                                        Style.name(name) + Style.good(" is retiring.") +
                                            Style.quiet(
                                                " It takes no new joins; the players on it finish in " +
                                                    "their own time and nobody is kicked.",
                                            ),
                                    )
                                } else {
                                    // The operator's own words. Every refusal
                                    // it sends is written for a person --
                                    // already retiring, no such server, asked
                                    // too often -- and rewording them here
                                    // would only lose which one it was.
                                    reply(adapter, format, 
                                        source,
                                        Style.bad("could not retire") + " " + Style.name(name) +
                                            Style.quiet(": ") + Style.bad(reason(failure)),
                                    )
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
                        .executes { ctx -> startBoost(api, adapter, format, ctx.source, group(ctx), 1, null) }
                        .then(
                            RequiredArgumentBuilder.argument<S, Int>("count", IntegerArgumentType.integer(1))
                                .executes { ctx ->
                                    startBoost(api, adapter, format, ctx.source, group(ctx), count(ctx), null)
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
                                                    reply(adapter, format, 
                                                        ctx.source,
                                                        Style.bad("could not read") + " " +
                                                            Style.name(text) +
                                                            Style.quiet(" as a length of time. Try ") +
                                                            Style.number("30m") + Style.quiet(", ") +
                                                            Style.number("2h") + Style.quiet(" or ") +
                                                            Style.number("90s") + Style.quiet("."),
                                                    )
                                                    return@executes 0
                                                }
                                                startBoost(api, adapter, format, ctx.source, group(ctx), count(ctx), span)
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
                                        reply(adapter, format, 
                                            source,
                                            Style.bad("could not stop boosts on") + " " + Style.name(name) +
                                                Style.quiet(": ") + Style.bad(reason(failure)),
                                        )
                                    // Zero said plainly rather than dressed up
                                    // as a success. An admin who expected
                                    // boosts has to learn there were none,
                                    // because the next thing they do depends
                                    // on it.
                                    removed == 0 ->
                                        reply(adapter, format, 
                                            source,
                                            Style.name(name) + Style.quiet(" had no boosts running"),
                                        )
                                    else -> reply(adapter, format, 
                                        source,
                                        Style.name(name) + Style.quiet(": removed ") +
                                            Style.number(removed) +
                                            Style.good(" boost${if (removed == 1) "" else "s"}.") +
                                            Style.quiet(" The group returns to its own floor as servers empty."),
                                    )
                                }
                            }
                            1
                        },
                ),
        )

        .then(
            LiteralArgumentBuilder.literal<S>("events")
                .requires { adapter.hasPermission(it, PERMISSION_EVENTS) }
                // Two literals rather than one argument, so tab-completion
                // offers `on` and `off` and a typo is an unknown command
                // instead of a silent no-op.
                .then(
                    LiteralArgumentBuilder.literal<S>("on")
                        .executes { ctx -> setFeed(adapter, format, feed, ctx.source, on = true) },
                )
                .then(
                    LiteralArgumentBuilder.literal<S>("off")
                        .executes { ctx -> setFeed(adapter, format, feed, ctx.source, on = false) },
                ),
        )

/**
 * Turns the feed on or off for whoever typed it.
 *
 * The `off` line says the setting lasts for the session, and section 5.5 asks
 * for that sentence rather than leaving it out. Paper could persist this in a
 * player's PersistentDataContainer and Velocity has no equivalent, so symmetry
 * won -- and a setting that quietly comes back after a rejoin is a setting
 * people report as a bug.
 */
private fun <S> setFeed(
    adapter: SourceAdapter<S>,
    format: () -> String,
    feed: FeedState,
    source: S,
    on: Boolean,
): Int {
    val player = adapter.playerId(source)
    if (player == null) {
        // Said rather than silently doing nothing. A console whose command
        // appeared to work and changed nothing is the worst of the three
        // possible behaviours here.
        reply(adapter, format, 
            source,
            Style.bad("the console cannot turn the cloud feed off") +
                Style.quiet(": it is not a player, and these lines are already in its log"),
        )
        return 0
    }
    if (on) {
        feed.optIn(player)
        reply(adapter, format, source, Style.good("The cloud feed is on for you."))
    } else {
        feed.optOut(player)
        reply(adapter, format, 
            source,
            Style.good("The cloud feed is off for you.") +
                Style.quiet(
                    " It comes back when you rejoin -- this setting lives for the session, on " +
                        "purpose: the proxy has nowhere to keep it, and one platform remembering " +
                        "while the other forgets would be worse than neither.",
                ),
        )
    }
    return 1
}

/**
 * One line of command output, wrapped in the network's own format.
 *
 * Every reply in this file goes through it rather than calling
 * [SourceAdapter.send] directly, and that is the point: eighteen call sites are
 * eighteen chances to forget the format at one of them, and the one that
 * forgot would look like a bug in the command rather than a missing wrapper.
 *
 * A blank format falls back to the built-in default, for the reason [Feed]
 * does the same: blank is what an operator older than the field sends, and
 * reading it as "print nothing" would silence the command on exactly the
 * upgrade that introduces it.
 */
private fun <S> reply(adapter: SourceAdapter<S>, format: () -> String, source: S, message: String) {
    adapter.send(source, format().ifBlank { Feed.DEFAULT_FORMAT }.replace(Feed.MESSAGE_TOKEN, message))
}

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
    format: () -> String,
    source: S,
    group: String,
    replicas: Int,
    forHowLong: java.time.Duration?,
): Int {
    api.boost(group, replicas, forHowLong).whenComplete { result, failure ->
        if (failure != null) {
            reply(adapter, format, 
                source,
                Style.bad("could not boost") + " " + Style.name(group) +
                    Style.quiet(": ") + Style.bad(reason(failure)),
            )
            return@whenComplete
        }
        reply(adapter, format, 
            source,
            Style.name(group) + Style.quiet(": ") +
                Style.good("+${result.replicas()} server${if (result.replicas() == 1) "" else "s"}") +
                Style.quiet(" until ") + Style.number(AT_MINUTE_UTC.format(result.expiresAt())) +
                Style.quiet(" UTC"),
        )
        reply(adapter, format, 
            source,
            Style.quiet("This is a boost, not a spec change. It expires on its own; ") +
                Style.number("/cloud stop $group") + Style.quiet(" ends it early."),
        )
        reply(adapter, format, source, Style.quiet("For a lasting change, edit the ServerGroup."))
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
    Style.name(server.name()) + Style.quiet(" in ") + Style.name(server.group()) +
        Style.quiet(": ") + Style.number(server.phase()) + Style.quiet(", ") +
        Style.number("${server.players()}/${server.slots()}") + Style.quiet(" players, ") +
        // Registered and not the phase, because they disagree during a drain
        // and this is the one that says whether anybody new can reach it.
        //
        // The one field in this whole tree where colour earns its place rather
        // than decorating: "can I send somebody there" is the question being
        // asked, and green against red answers it before the words are read.
        (if (server.registered()) Style.good("taking joins") else Style.bad("not taking joins")) +
        // What the server says it is doing, and only when it says something.
        // Last and introduced by "says", because everything before it is the
        // operator's account and this one is the server's -- an admin reading
        // a line where the two disagree has to be able to tell which is which.
        // A server that has announced nothing gets no fragment at all rather
        // than an empty one, which would read as a server that had gone quiet.
        (if (server.state().isEmpty()) "" else Style.quiet(", says ") + Style.name(server.state()))

/**
 * One group as a line, written once because `list` and `info` both print it.
 *
 * They were two copies of the same interpolation until colour made each of
 * them four times as long -- at which point two copies would have been two
 * palettes the day somebody improved one.
 */
private fun describeGroup(group: Group): String =
    Style.name(group.name()) + Style.quiet(" (") + Style.number(group.kind()) + Style.quiet("): ") +
        Style.number("${group.readyReplicas()}/${group.replicas()}") + Style.quiet(" ready, ") +
        Style.number(group.onlinePlayers()) + Style.quiet(" players, ") +
        Style.number(group.freeSlots()) + Style.quiet(" free slots")
