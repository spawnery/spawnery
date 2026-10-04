package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.api.ProxyInfo
import cloud.spawnery.agent.api.ServerInfo

internal fun nodeText(node: String): String =
    if (node.isEmpty()) Style.quiet("not scheduled") else Style.name(node)


internal fun listLines(groups: List<Group>, servers: List<ServerInfo>, proxies: List<ProxyInfo>): List<String> {
    val lines = mutableListOf(
        Layout.heading(
            "Network",
            Style.quiet("${Layout.count(groups.size, "group", "groups")} · ${Layout.count(servers.size, "server", "servers")} · ${Layout.count(proxies.size, "proxy", "proxies")}"),
        ),
    )
    if (groups.isEmpty()) {
        lines += Layout.entry(Style.quiet("no groups on this network yet"))
        return lines
    }
    val (proxyGroups, serverGroups) = groups.partition { it.kind() == Group.Kind.PROXY }
    if (serverGroups.isNotEmpty()) {
        lines += Layout.section("Server groups")
        for (g in serverGroups) {
            lines += Layout.entry(
                Layout.joined(
                    Style.name(g.name()) + Style.quiet(" (${Layout.kindName(g.kind())})"),
                    Style.number("${g.readyReplicas()}/${g.replicas()}") + Style.quiet(" ready"),
                    Style.number("${g.onlinePlayers()}/${g.onlinePlayers() + g.freeSlots()}") + Style.quiet(" players"),
                    Style.number(g.freeSlots()) + Style.quiet(" free"),
                ),
            )
        }
    }
    if (proxyGroups.isNotEmpty()) {
        lines += Layout.section("Proxy groups")
        for (g in proxyGroups) {
            lines += Layout.entry(
                Layout.joined(
                    Style.name(g.name()),
                    Style.number("${g.readyReplicas()}/${g.replicas()}") + Style.quiet(" ready"),
                    Style.number(g.onlinePlayers()) + Style.quiet(if (g.onlinePlayers() == 1) " player" else " players"),
                ),
            )
            for (p in proxies.filter { it.group() == g.name() }.sortedBy { it.name() }) {
                lines += Layout.member(proxyLine(p))
            }
        }
    }
    return lines
}

private fun proxyLine(p: ProxyInfo): String =
    Layout.joined(
        Style.name(p.name()),
        if (p.ready()) Style.good("ready") else Style.bad("not ready"),
        if (p.draining()) Style.bad("draining") else "",
        Style.number(p.players()) + Style.quiet(if (p.players() == 1) " player" else " players"),
    )

internal fun serverInfoLines(s: ServerInfo): List<String> {
    val lines = mutableListOf(
        Layout.heading(
            s.name(),
            Layout.joined(
                Style.quiet("server in ") + Style.name(s.group()),
                Style.number(s.phase().toString()),
                if (s.registered()) Style.good("taking joins") else Style.bad("not taking joins"),
            ),
        ),
        Layout.field("Node", nodeText(s.node())),
    )
    val fill = seatsFill(s.players(), s.playableSlots(), s.slots())
    lines += Layout.field(
        "Players",
        (if (s.slots() > 0) Layout.bar(fill, Layout.fillColour(fill)) + "  " else "") +
            Style.number(seatsText(s.players(), s.playableSlots(), s.slots(), spaced = true)),
    )
    if (s.state().isNotEmpty()) lines += Layout.field("Says", Style.name(s.state()))
    if (s.held()) lines += Layout.field("Marked", Style.bad("held"))
    return lines
}

internal fun proxyInfoLines(p: ProxyInfo): List<String> = listOf(
    Layout.heading(
        p.name(),
        Layout.joined(
            Style.quiet("proxy in ") + Style.name(p.group()),
            if (p.ready()) Style.good("ready") else Style.bad("not ready"),
            if (p.draining()) Style.bad("draining") else "",
        ),
    ),
    Layout.field("Node", nodeText(p.node())),
    Layout.field("Players", Style.number(p.players())),
)

internal fun groupInfoLines(g: Group, servers: List<ServerInfo>, proxies: List<ProxyInfo>): List<String> {
    val lines = mutableListOf(
        Layout.heading(
            g.name(),
            Layout.joined(
                Style.quiet("${Layout.kindName(g.kind())} group"),
                Style.number("${g.readyReplicas()}/${g.replicas()}") + Style.quiet(" ready"),
            ),
        ),
    )
    if (g.kind() == Group.Kind.PROXY) {
        lines += Layout.field("Players", Style.number(g.onlinePlayers()))
        lines += Layout.section("Proxies")
        proxies.filter { it.group() == g.name() }.sortedBy { it.name() }.forEach { lines += Layout.entry(proxyLine(it)) }
    } else {
        val capacity = g.onlinePlayers() + g.freeSlots()
        val fill = if (capacity > 0) g.onlinePlayers().toDouble() / capacity else 0.0
        lines += Layout.field(
            "Players",
            (if (capacity > 0) Layout.bar(fill, Layout.fillColour(fill)) + "  " else "") +
                Style.number("${g.onlinePlayers()} / $capacity") + Style.quiet(" · ") +
                Style.number(g.freeSlots()) + Style.quiet(" free"),
        )
        if (g.pinned()) {
            val count = Style.number(g.pinnedReplicas()) + Style.quiet(if (g.pinnedReplicas() == 1) " server" else " servers")
            val until = g.pinnedUntil()
            lines += Layout.field(
                "Pinned",
                if (until == null) count + Style.quiet(", no end")
                else count + Style.quiet(" until ") + Style.number(AT_MINUTE_UTC.format(until)) + Style.quiet(" UTC"),
            )
        }
        val members = servers.filter { it.group() == g.name() }.sortedBy { it.name() }
        if (members.isNotEmpty()) {
            lines += Layout.section("Servers")
            members.forEach {
                lines += Layout.entry(
                    Layout.joined(
                        Style.name(it.name()),
                        Style.number(it.phase().toString()),
                        Style.number(seatsText(it.players(), it.playableSlots(), it.slots(), spaced = false)),
                        nodeText(it.node()),
                        if (it.held()) Style.bad("held") else "",
                    ),
                )
            }
        }
    }
    return lines
}
