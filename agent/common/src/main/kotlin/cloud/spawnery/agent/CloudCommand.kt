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

/** Covers pinning and resetting, so whoever holds a group at a size can let it go. */
const val PERMISSION_SCALE: String = "spawnery.cloud.scale"

/** Proxy only. Kills a pod without a drain. */
const val PERMISSION_FORCESTOP: String = "spawnery.cloud.forcestop"

/** Proxy only, and only on a network with spec.commands.execute. */
const val PERMISSION_EXECUTE: String = "spawnery.cloud.execute"

internal const val EXECUTE_MAX_COMMAND: Int = 256

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
    levels: FeedLevels,
    /** A lambda so a Network edit lands at the next resync, not the next pod. */
    format: () -> String = { Feed.DEFAULT_FORMAT },
    proxy: ProxyCommands<S>? = null,
): LiteralArgumentBuilder<S> {
    val root = LiteralArgumentBuilder.literal<S>("cloud")
        // Any branch's permission opens the root; the branches gate themselves.
        .requires { source -> rootNodes(proxy != null).any { adapter.hasPermission(source, it) } }
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
                                    Style.bad("nothing called") + " " + Style.name(name) +
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
                                    replyOk(adapter, format, 
                                        source,
                                        Style.name(name) + Style.good(" is retiring") +
                                            Style.quiet(": no new joins, and nobody is kicked."),
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
                                        Style.name(name) + Style.good(" takes joins again") +
                                            Style.quiet(" and stays until it ends by itself."),
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
            LiteralArgumentBuilder.literal<S>("scale")
                .requires { adapter.hasPermission(it, PERMISSION_SCALE) }
                .then(
                    RequiredArgumentBuilder.argument<S, String>("group", StringArgumentType.word())
                        .suggests(suggesting { api.groups().filter { it.kind() == Group.Kind.EPHEMERAL }.map(Group::name) })
                        .then(
                            LiteralArgumentBuilder.literal<S>("reset")
                                .executes { ctx -> resetScale(api, adapter, format, ctx.source, group(ctx)) },
                        )
                        .then(
                            RequiredArgumentBuilder.argument<S, Int>("count", IntegerArgumentType.integer(0))
                                .executes { ctx -> pin(api, adapter, format, ctx.source, group(ctx), count(ctx), null) }
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
                                                        Style.name(text) +
                                                            Style.bad(" is not a duration") +
                                                            Style.quiet(". Try ") +
                                                            Style.number("30m") + Style.quiet(", ") +
                                                            Style.number("2h") + Style.quiet(" or ") +
                                                            Style.number("3d") + Style.quiet("."),
                                                    )
                                                    return@executes 0
                                                }
                                                pin(api, adapter, format, ctx.source, group(ctx), count(ctx), span)
                                            },
                                        ),
                                ),
                        ),
                ),
        )

        .then(eventsBranch(adapter, format, levels))

    if (proxy != null) {
        root.then(forceStopBranch(api, adapter, format, proxy)).then(executeBranch(api, adapter, format, proxy))
    }
    return root
}

/** Read first, so a reader is asked one question, as before. */
private fun rootNodes(onProxy: Boolean): List<String> =
    listOf(PERMISSION_READ, PERMISSION_RETIRE, PERMISSION_SCALE, PERMISSION_STATUS, PERMISSION_EVENTS) +
        if (onProxy) listOf(PERMISSION_FORCESTOP, PERMISSION_EXECUTE) else emptyList()

private val LEVEL_WORDS = listOf("minimal", "normal", "verbose", "off", "on")

private fun <S> eventsBranch(
    adapter: SourceAdapter<S>,
    format: () -> String,
    levels: FeedLevels,
): LiteralArgumentBuilder<S> {
    val branch = LiteralArgumentBuilder.literal<S>("events")
        .requires { adapter.hasPermission(it, PERMISSION_EVENTS) }
        .executes { ctx -> showLevel(adapter, format, levels, ctx.source) }
    // Literals rather than one argument, so a typo is an unknown command
    // instead of a silent no-op.
    for (word in LEVEL_WORDS) {
        val level = FeedLevel.parse(word)!!
        branch.then(
            LiteralArgumentBuilder.literal<S>(word)
                .executes { ctx -> setLevel(adapter, format, levels, ctx.source, level) },
        )
    }
    return branch
}

private fun <S> showLevel(adapter: SourceAdapter<S>, format: () -> String, levels: FeedLevels, source: S): Int {
    val player = adapter.playerId(source) ?: return consoleHasNoLevel(adapter, format, source)
    replyOk(
        adapter, format, source,
        Style.quiet("Your feed level is ") + Style.number(levels.level(player).word) +
            Style.quiet(". Change it with ") + Style.number("/cloud events minimal|normal|verbose|off") +
            Style.quiet("."),
    )
    return 1
}

private fun <S> setLevel(
    adapter: SourceAdapter<S>,
    format: () -> String,
    levels: FeedLevels,
    source: S,
    level: FeedLevel,
): Int {
    val player = adapter.playerId(source) ?: return consoleHasNoLevel(adapter, format, source)
    val kept = levels.kept()
    levels.set(player, level).whenComplete { _, failure ->
        if (failure != null) {
            replyFail(
                adapter, format, source,
                Style.bad("could not save your feed level") + Style.quiet(": ") + Style.bad(reason(failure)),
            )
        } else {
            replyOk(
                adapter, format, source,
                Style.good("Feed level: ${level.word}.") +
                    if (kept) "" else Style.quiet(" Kept until the next restart."),
            )
        }
    }
    return 1
}

private fun <S> consoleHasNoLevel(adapter: SourceAdapter<S>, format: () -> String, source: S): Int {
    replyFail(
        adapter, format, source,
        Style.bad("the console has no feed level") + Style.quiet(": the feed goes to players only"),
    )
    return 0
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
            replyFail(adapter, format, source, Style.bad("could not get the status") + Style.quiet(": ") + Style.bad(reason(failure)))
        }
    }
    return 1
}

private fun <S> group(ctx: com.mojang.brigadier.context.CommandContext<S>): String =
    StringArgumentType.getString(ctx, "group")

private fun <S> count(ctx: com.mojang.brigadier.context.CommandContext<S>): Int =
    IntegerArgumentType.getInteger(ctx, "count")

private fun <S> pin(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    format: () -> String,
    source: S,
    group: String,
    replicas: Int,
    forHowLong: java.time.Duration?,
): Int {
    api.scale(group, replicas, forHowLong).whenComplete { result, failure ->
        if (failure != null) {
            replyFail(adapter, format, source,
                Style.bad("could not scale") + " " + Style.name(group) + Style.quiet(": ") + Style.bad(reason(failure)))
            return@whenComplete
        }
        replyOk(adapter, format, source,
            Style.name(group) + Style.quiet(" is pinned to ") + Style.number(result.replicas()) +
                Style.quiet(if (result.replicas() == 1) " server" else " servers") +
                Style.quiet(" until ") + Style.number(AT_MINUTE_UTC.format(result.expiresAt())) + Style.quiet(" UTC"))
        reply(adapter, format, source,
            Style.number("/cloud scale $group reset") + Style.quiet(" ends it early."))
    }
    return 1
}

private fun <S> resetScale(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    format: () -> String,
    source: S,
    group: String,
): Int {
    api.resetScale(group).whenComplete { removed, failure ->
        when {
            failure != null -> replyFail(adapter, format, source,
                Style.bad("could not reset") + " " + Style.name(group) + Style.quiet(": ") + Style.bad(reason(failure)))
            removed == 0 -> replyOk(adapter, format, source, Style.name(group) + Style.quiet(" has no pin or boost"))
            else -> replyOk(adapter, format, source,
                Style.quiet("removed ") + Style.number(removed) +
                    Style.good(if (removed == 1) " pin or boost" else " pins and boosts") +
                    Style.quiet(" from ") + Style.name(group) +
                    Style.quiet("; it is back to its own floor and ceiling"))
        }
    }
    return 1
}

private fun <S> forceStopBranch(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    format: () -> String,
    proxy: ProxyCommands<S>,
): LiteralArgumentBuilder<S> =
    LiteralArgumentBuilder.literal<S>("forcestop")
        .requires { adapter.hasPermission(it, PERMISSION_FORCESTOP) }
        .then(
            RequiredArgumentBuilder.argument<S, String>("server", StringArgumentType.word())
                .suggests(suggesting { api.servers().map(ServerInfo::name) })
                .executes { ctx ->
                    val name = StringArgumentType.getString(ctx, "server")
                    val source = ctx.source
                    proxy.connector.forceStop(name, proxy.issuer(source)).whenComplete { _, failure ->
                        if (failure == null) {
                            replyOk(adapter, format, source,
                                Style.name(name) + Style.bad(" is being killed.") +
                                    Style.quiet(" /cloud info shows when it is gone."))
                        } else {
                            replyFail(adapter, format, source,
                                Style.bad("could not force-stop") + " " + Style.name(name) +
                                    Style.quiet(": ") + Style.bad(reason(failure)))
                        }
                    }
                    1
                },
        )

private fun <S> executeBranch(
    api: SpawneryApi,
    adapter: SourceAdapter<S>,
    format: () -> String,
    proxy: ProxyCommands<S>,
): LiteralArgumentBuilder<S> =
    LiteralArgumentBuilder.literal<S>("execute")
        .requires { adapter.hasPermission(it, PERMISSION_EXECUTE) }
        .then(
            RequiredArgumentBuilder.argument<S, String>("target", StringArgumentType.word())
                .suggests(suggesting {
                    api.servers().map(ServerInfo::name) +
                        api.groups().filter { it.kind() != Group.Kind.PROXY }.map(Group::name)
                })
                .then(
                    RequiredArgumentBuilder.argument<S, String>("command", StringArgumentType.greedyString())
                        .executes { ctx ->
                            val target = StringArgumentType.getString(ctx, "target")
                            val command = StringArgumentType.getString(ctx, "command").trim().removePrefix("/")
                            val source = ctx.source
                            if (command.length > EXECUTE_MAX_COMMAND) {
                                replyFail(adapter, format, source,
                                    Style.bad("that command is ") + Style.number(command.length) +
                                        Style.bad(" characters long; the limit is ") +
                                        Style.number(EXECUTE_MAX_COMMAND))
                                return@executes 0
                            }
                            proxy.connector.execute(target, command, proxy.issuer(source)).whenComplete { outcomes, failure ->
                                if (failure != null) {
                                    replyFail(adapter, format, source,
                                        Style.bad("could not run it on") + " " + Style.name(target) +
                                            Style.quiet(": ") + Style.bad(reason(failure)))
                                } else {
                                    executeLines(target, outcomes).forEach { reply(adapter, format, source, it) }
                                }
                            }
                            1
                        },
                ),
        )

internal val AT_MINUTE_UTC: DateTimeFormatter =
    DateTimeFormatter.ofPattern("HH:mm").withZone(ZoneOffset.UTC)

/** Not java.time's ISO-8601 parser: nobody types `PT30M` into a chat window. */
internal fun parseDuration(text: String): java.time.Duration? {
    if (text.length < 2) return null
    val amount = text.dropLast(1).toLongOrNull() ?: return null
    if (amount <= 0) return null
    return try {
        when (text.last()) {
            's' -> java.time.Duration.ofSeconds(amount)
            'm' -> java.time.Duration.ofMinutes(amount)
            'h' -> java.time.Duration.ofHours(amount)
            'd' -> java.time.Duration.ofDays(amount)
            else -> null
        }
    } catch (_: ArithmeticException) {
        null
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
