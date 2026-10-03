package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.api.NetworkStatus
import cloud.spawnery.agent.api.ProxyInfo
import cloud.spawnery.agent.api.ServerInfo
import cloud.spawnery.agent.api.ServerPhase
import cloud.spawnery.agent.api.SpawneryApi
import com.mojang.brigadier.arguments.IntegerArgumentType
import com.mojang.brigadier.arguments.StringArgumentType
import com.mojang.brigadier.builder.LiteralArgumentBuilder
import com.mojang.brigadier.builder.RequiredArgumentBuilder
import com.mojang.brigadier.suggestion.SuggestionProvider
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter

const val PERMISSION_READ: String = "spawnery.cloud.read"

/** Its own node, not implied by [PERMISSION_READ]: reading is for moderators, retiring changes the fleet. */
const val PERMISSION_RETIRE: String = "spawnery.cloud.retire"

/** Covers both `start` and `stop`, so whoever adds capacity can take it back. */
const val PERMISSION_SCALE: String = "spawnery.cloud.scale"

/** `/cloud status`, which asks the operator and so is not part of reading the mirror. */
const val PERMISSION_STATUS: String = "spawnery.cloud.status"

// PERMISSION_EVENTS lives in Feed.kt, beside its only reader.

/**
 * `list`, `info` and completion read only the local mirror, so they cannot
 * block or fail when the operator is unreachable. The other verbs answer from
 * a gRPC thread, which is safe because [SourceAdapter] can only send to an
 * audience.
 */
fun <S> cloudCommand(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    feed: FeedState,
    /** A lambda so a Network edit lands at the next resync, not the next pod. */
    format: () -> String = { Feed.DEFAULT_FORMAT },
): LiteralArgumentBuilder<S> =
    LiteralArgumentBuilder.literal<S>("cloud")
        // Any branch's permission opens the root; the branches gate themselves.
        .requires {
            adapter.hasPermission(it, PERMISSION_READ) ||
                adapter.hasPermission(it, PERMISSION_RETIRE) ||
                adapter.hasPermission(it, PERMISSION_SCALE) ||
                adapter.hasPermission(it, PERMISSION_STATUS) ||
                adapter.hasPermission(it, PERMISSION_EVENTS)
        }
        .then(
            LiteralArgumentBuilder.literal<S>("list")
                .requires { adapter.hasPermission(it, PERMISSION_READ) }
                .executes { ctx ->
                    listLines(api.groups(), api.servers(), api.proxies()).forEach {
                        reply(adapter, format, ctx.source, it)
                    }
                    api.groups().size
                },
        )
        .then(
            LiteralArgumentBuilder.literal<S>("info")
                .requires { adapter.hasPermission(it, PERMISSION_READ) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("name", StringArgumentType.word())
                        .suggests(suggesting {
                            api.servers().map(ServerInfo::name) + api.proxies().map(ProxyInfo::name) +
                                api.groups().map(Group::name)
                        })
                        .executes { ctx ->
                            val name = StringArgumentType.getString(ctx, "name")
                            val server = api.server(name)
                            if (server.isPresent) {
                                serverInfoLines(server.get()).forEach { reply(adapter, format, ctx.source, it) }
                                return@executes 1
                            }
                            val proxy = api.proxy(name)
                            if (proxy.isPresent) {
                                proxyInfoLines(proxy.get()).forEach { reply(adapter, format, ctx.source, it) }
                                return@executes 1
                            }
                            val group = api.group(name)
                            if (group.isPresent) {
                                groupInfoLines(group.get(), api.servers(), api.proxies()).forEach {
                                    reply(adapter, format, ctx.source, it)
                                }
                                return@executes 1
                            }
                            reply(adapter, format, 
                                ctx.source,
                                Layout.fail(
                                    Style.bad("no server, proxy or group called") + " " + Style.name(name) +
                                        Style.quiet(" on this network"),
                                ),
                            )
                            0
                        },
                ),
        )
        .then(
            LiteralArgumentBuilder.literal<S>("status")
                .requires { adapter.hasPermission(it, PERMISSION_STATUS) }
                .executes { ctx -> askStatus(api.status(), adapter, format, ctx.source, "") }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("name", StringArgumentType.word())
                        .suggests(suggesting {
                            api.groups().map(Group::name) + api.servers().map(ServerInfo::name) +
                                api.proxies().map(ProxyInfo::name)
                        })
                        .executes { ctx ->
                            val name = StringArgumentType.getString(ctx, "name")
                            askStatus(api.status(name), adapter, format, ctx.source, name)
                        },
                ),
        )

        .then(
            LiteralArgumentBuilder.literal<S>("retire")
                .requires { adapter.hasPermission(it, PERMISSION_RETIRE) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("name", StringArgumentType.word())
                        .suggests(suggesting { api.servers().map(ServerInfo::name) + api.proxies().map(ProxyInfo::name) })
                        .executes { ctx ->
                            val name = StringArgumentType.getString(ctx, "name")
                            val source = ctx.source
                            api.retire(name).whenComplete { _, failure ->
                                if (failure == null) {
                                    // "Retire" reads as "stop" to anybody who has not read the design.
                                    replyOk(adapter, format, 
                                        source,
                                        Style.name(name) + Style.good(" is retiring.") +
                                            Style.quiet(
                                                " It takes no new joins; the players on it finish in " +
                                                    "their own time and nobody is kicked.",
                                            ),
                                    )
                                } else {
                                    // The operator's refusals are written for a person.
                                    replyFail(adapter, format, 
                                        source,
                                        Style.bad("could not retire") + " " + Style.name(name) +
                                            Style.quiet(": ") + Style.bad(reason(failure)),
                                    )
                                }
                            }
                            // The request went out, not that it worked.
                            1
                        },
                ),
        )

        .then(
            LiteralArgumentBuilder.literal<S>("unretire")
                .requires { adapter.hasPermission(it, PERMISSION_RETIRE) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("name", StringArgumentType.word())
                        .suggests(suggesting { api.servers().filter { it.phase() == ServerPhase.RETIRING }.map(ServerInfo::name) })
                        .executes { ctx ->
                            val name = StringArgumentType.getString(ctx, "name")
                            val source = ctx.source
                            api.unretire(name).whenComplete { _, failure ->
                                if (failure == null) {
                                    replyOk(adapter, format, 
                                        source,
                                        Style.name(name) + Style.good(" takes joins again.") +
                                            Style.quiet(" Nothing automatic removes it now; it stays until it ends by itself."),
                                    )
                                } else {
                                    replyFail(adapter, format, 
                                        source,
                                        Style.bad("could not unretire") + " " + Style.name(name) +
                                            Style.quiet(": ") + Style.bad(reason(failure)),
                                    )
                                }
                            }
                            1
                        },
                ),
        )
        .then(
            LiteralArgumentBuilder.literal<S>("start")
                .requires { adapter.hasPermission(it, PERMISSION_SCALE) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("group", StringArgumentType.word())
                        .suggests(suggesting { api.groups().map(Group::name) })
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
                                                    replyFail(adapter, format, 
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
                        .suggests(suggesting { api.groups().map(Group::name) })
                        .executes { ctx ->
                            val name = group(ctx)
                            val source = ctx.source
                            api.stopBoosts(name).whenComplete { removed, failure ->
                                when {
                                    failure != null ->
                                        replyFail(adapter, format, 
                                            source,
                                            Style.bad("could not stop boosts on") + " " + Style.name(name) +
                                                Style.quiet(": ") + Style.bad(reason(failure)),
                                        )
                                    removed == 0 ->
                                        replyOk(adapter, format, 
                                            source,
                                            Style.name(name) + Style.quiet(" had no boosts running"),
                                        )
                                    else -> replyOk(adapter, format, 
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
                // Two literals rather than one argument, so a typo is an
                // unknown command instead of a silent no-op.
                .then(
                    LiteralArgumentBuilder.literal<S>("on")
                        .executes { ctx -> setFeed(adapter, format, feed, ctx.source, on = true) },
                )
                .then(
                    LiteralArgumentBuilder.literal<S>("off")
                        .executes { ctx -> setFeed(adapter, format, feed, ctx.source, on = false) },
                ),
        )

private fun <S> setFeed(
    adapter: SourceAdapter<S>,
    format: () -> String,
    feed: FeedState,
    source: S,
    on: Boolean,
): Int {
    val player = adapter.playerId(source)
    if (player == null) {
        replyFail(adapter, format, 
            source,
            Style.bad("the console cannot turn the cloud feed off") +
                Style.quiet(": it is not a player, and these lines are already in its log"),
        )
        return 0
    }
    if (on) {
        feed.optIn(player)
        replyOk(adapter, format, source, Style.good("The cloud feed is on for you."))
    } else {
        feed.optOut(player)
        replyOk(adapter, format, 
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

/** A blank format is what an operator older than the field sends. */
private fun <S> reply(adapter: SourceAdapter<S>, format: () -> String, source: S, message: String) {
    adapter.send(source, format().ifBlank { Feed.DEFAULT_FORMAT }.replace(Feed.MESSAGE_TOKEN, message))
}

private fun <S> replyOk(adapter: SourceAdapter<S>, format: () -> String, source: S, message: String) =
    reply(adapter, format, source, Layout.ok(message))

private fun <S> replyFail(adapter: SourceAdapter<S>, format: () -> String, source: S, message: String) =
    reply(adapter, format, source, Layout.fail(message))

private fun <S> askStatus(
    answer: java.util.concurrent.CompletionStage<NetworkStatus>,
    adapter: SourceAdapter<S>,
    format: () -> String,
    source: S,
    target: String,
): Int {
    answer.whenComplete { status, failure ->
        if (failure == null) {
            for (line in statusLines(status, target)) reply(adapter, format, source, line)
        } else {
            replyFail(adapter, format, source, Style.bad("no status") + Style.quiet(": ") + Style.bad(reason(failure)))
        }
    }
    return 1
}

private fun <S> group(ctx: com.mojang.brigadier.context.CommandContext<S>): String =
    StringArgumentType.getString(ctx, "group")

private fun <S> count(ctx: com.mojang.brigadier.context.CommandContext<S>): Int =
    IntegerArgumentType.getInteger(ctx, "count")

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
            replyFail(adapter, format, 
                source,
                Style.bad("could not boost") + " " + Style.name(group) +
                    Style.quiet(": ") + Style.bad(reason(failure)),
            )
            return@whenComplete
        }
        replyOk(adapter, format, 
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

private val AT_MINUTE_UTC: DateTimeFormatter =
    DateTimeFormatter.ofPattern("HH:mm").withZone(ZoneOffset.UTC)

/** Not java.time's ISO-8601 parser: nobody types `PT30M` into a chat window. */
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

private fun reason(failure: Throwable): String {
    val cause = if (failure is java.util.concurrent.CompletionException && failure.cause != null) {
        failure.cause!!
    } else {
        failure
    }
    return cause.message ?: cause.javaClass.simpleName
}

/** Out of the local mirror, so a keystroke never waits on the operator. */
private fun <S> suggesting(names: () -> List<String>): SuggestionProvider<S> =
    SuggestionProvider { _, builder ->
        val typed = builder.remaining.lowercase()
        for (name in names()) {
            if (name.lowercase().startsWith(typed)) builder.suggest(name)
        }
        builder.buildFuture()
    }
