package cloud.spawnery.agent

import cloud.spawnery.agent.api.Group
import cloud.spawnery.agent.api.GroupStatus
import cloud.spawnery.agent.api.InstanceStatus
import cloud.spawnery.agent.api.NetworkStatus
import cloud.spawnery.agent.api.ResourceUsage
import java.time.Duration
import java.util.Locale
import java.util.OptionalDouble

internal fun statusLines(status: NetworkStatus, target: String): List<String> {
    val metrics = status.metricsAvailable()
    val lines = mutableListOf<String>()
    when {
        target.isEmpty() -> {
            lines += Layout.heading(
                "Network status",
                Style.quiet(
                    Layout.count(status.players(), "player", "players") + " · " +
                        Layout.count(status.servers(), "server", "servers") + " · " +
                        Layout.count(status.proxies(), "proxy", "proxies"),
                ),
            )
            lines += Layout.section("Resources")
            lines += Layout.entry(cpuLine(status.total(), metrics))
            lines += Layout.entry(ramLine(status.total(), metrics))
            note(status.total(), metrics)?.let { lines += Layout.entry(it) }
            val (proxyGroups, serverGroups) = status.groups().partition { it.kind() == Group.Kind.PROXY }
            if (serverGroups.isNotEmpty()) {
                lines += Layout.section("Server groups")
                serverGroups.forEach { lines += Layout.entry(groupLine(it)) }
            }
            if (proxyGroups.isNotEmpty()) {
                lines += Layout.section("Proxy groups")
                proxyGroups.forEach { lines += Layout.entry(groupLine(it)) }
            }
            if (status.other().pods() > 0) {
                lines += Layout.section("Other pods")
                lines += Layout.entry(cpuRam(status.other()))
            }
        }
        status.groups().isNotEmpty() -> {
            val g = status.groups().single()
            lines += Layout.heading(g.name(), groupLine(g, withName = false))
            lines += Layout.section(if (g.kind() == Group.Kind.PROXY) "Proxies" else "Servers")
            status.instances().forEach { lines += Layout.entry(memberLine(it)) }
        }
        else -> lines += instanceLines(status.instances().single(), metrics)
    }
    return lines
}

private fun cores(milli: Long) = String.format(Locale.ROOT, "%.1f", milli / 1000.0)
private fun gib(bytes: Long) = String.format(Locale.ROOT, "%.1f", bytes / (1L shl 30).toDouble())
private fun one(v: Double) = String.format(Locale.ROOT, "%.1f", v)

private fun resource(
    label: String?, used: Long, requested: Long, limit: Long, unlimited: Boolean,
    measured: Boolean, unit: String, show: (Long) -> String,
): String {
    val against = if (!unlimited && limit > 0) limit else requested
    val barPart = if (measured && against > 0) {
        val f = used.toDouble() / against
        Layout.bar(f, Layout.fillColour(f)) + "  "
    } else ""
    val usedText = if (measured) show(used) else "–"
    val limitText = when {
        !unlimited -> Style.number(usedText) + Style.quiet(" of ") + Style.number(show(limit)) + Style.quiet(" $unit")
        limit == 0L -> Style.number(usedText) + Style.quiet(" $unit · no limit")
        else -> Style.number(usedText) + Style.quiet(" $unit · limit ≥ ") + Style.number(show(limit))
    }
    return (if (label == null) "" else Style.quiet("$label  ")) + barPart + limitText + Style.quiet(" · ") +
        Style.number(show(requested)) + Style.quiet(" requested")
}

private fun cpuLine(u: ResourceUsage, metrics: Boolean, labelled: Boolean = true) = resource(
    if (labelled) "CPU" else null, u.cpuUsedMillicores(), u.cpuRequestedMillicores(), u.cpuLimitMillicores(), u.cpuUnlimited(),
    metrics && u.measured(), "cores", ::cores,
)

private fun ramLine(u: ResourceUsage, metrics: Boolean, labelled: Boolean = true) = resource(
    if (labelled) "RAM" else null, u.memoryUsedBytes(), u.memoryRequestedBytes(), u.memoryLimitBytes(), u.memoryUnlimited(),
    metrics && u.measured(), "GiB", ::gib,
)

private fun note(u: ResourceUsage, metrics: Boolean): String? = when {
    !metrics -> Style.bad("usage unavailable (metrics API not answering)")
    u.measured() && !u.complete() -> Style.quiet("usage of ${u.podsMeasured()} of ${u.pods()} pods")
    else -> null
}

private fun cpuRam(u: ResourceUsage): String =
    if (!u.measured()) {
        Layout.joined(Style.quiet("CPU ") + Style.number("–"), Style.quiet("RAM ") + Style.number("–"))
    } else {
        Layout.joined(
            Style.quiet("CPU ") + Style.number(cores(u.cpuUsedMillicores())),
            Style.quiet("RAM ") + Style.number(gib(u.memoryUsedBytes()) + " GiB"),
        )
    }

private fun tpsText(value: OptionalDouble): String =
    if (value.isEmpty) Style.quiet("TPS ") + Style.number("–")
    else Style.quiet("TPS ") + "<${Layout.tpsColour(value.asDouble)}>" + Style.escape(one(value.asDouble)) +
        "</${Layout.tpsColour(value.asDouble)}>"

private fun groupLine(g: GroupStatus, withName: Boolean = true): String {
    val proxy = g.kind() == Group.Kind.PROXY
    return Layout.joined(
        if (withName) Style.name(g.name()) else "",
        Style.number(g.phase()),
        Style.number("${g.readyReplicas()}/${g.replicas()}"),
        Style.number(g.players()) + Style.quiet(if (g.players() == 1) " player" else " players"),
        if (proxy) "" else tpsText(g.lowestTps()),
        cpuRam(g.usage()),
    )
}

private fun age(d: Duration): String {
    val s = d.seconds
    return when {
        s < 60 -> "${s}s"
        s < 3600 -> "${s / 60}m"
        s < 86400 -> "${s / 3600}h${(s % 3600) / 60}m"
        else -> "${s / 86400}d${(s % 86400) / 3600}h"
    }
}

private fun markers(i: InstanceStatus): String = listOfNotNull(
    if (i.retiring()) "retiring" else null,
    if (i.held()) "held" else null,
    if (i.draining()) "draining" else null,
).joinToString(Style.quiet(" · ")) { Style.bad(it) }

private fun memberLine(i: InstanceStatus): String = Layout.joined(
    Style.name(i.name()),
    if (i.proxy()) (if (i.ready()) Style.good("ready") else Style.bad("not ready")) else Style.number(i.phase()),
    Style.number(seatsText(i.players(), i.playableSlots(), i.slots(), spaced = false)),
    if (i.proxy()) "" else tpsText(i.tps()),
    if (i.proxy()) "" else Style.quiet("MSPT ") + Style.number(if (i.mspt().isEmpty) "–" else one(i.mspt().asDouble)),
    cpuRam(i.usage()),
    nodeText(i.node()),
    Style.quiet("up ") + Style.number(age(i.age())),
    markers(i),
)

private fun instanceLines(i: InstanceStatus, metrics: Boolean): List<String> {
    val lines = mutableListOf(
        Layout.heading(
            i.name(),
            Layout.joined(
                Style.quiet(if (i.proxy()) "proxy in " else "server in ") + Style.name(i.group()),
                if (i.proxy()) (if (i.ready()) Style.good("ready") else Style.bad("not ready")) else Style.number(i.phase()),
                Style.quiet("up ") + Style.number(age(i.age())),
            ),
        ),
        Layout.field("Node", nodeText(i.node())),
    )
    val fill = seatsFill(i.players(), i.playableSlots(), i.slots())
    lines += Layout.field(
        "Players",
        (if (i.slots() > 0) Layout.bar(fill, Layout.fillColour(fill)) + "  " else "") +
            Style.number(seatsText(i.players(), i.playableSlots(), i.slots(), spaced = true)),
    )
    if (!i.proxy()) {
        val tps = i.tps()
        val mspt = if (i.mspt().isEmpty) "–" else one(i.mspt().asDouble)
        lines += Layout.field(
            "TPS",
            if (tps.isEmpty) Style.number("–") + Style.quiet(" · MSPT ") + Style.number(mspt)
            else Layout.bar(tps.asDouble / 20, Layout.tpsColour(tps.asDouble)) + "  " +
                "<${Layout.tpsColour(tps.asDouble)}>" + Style.escape(one(tps.asDouble)) + "</${Layout.tpsColour(tps.asDouble)}>" +
                Style.quiet(" · MSPT ") + Style.number(mspt),
        )
    }
    if (i.usage().pods() > 0) {
        lines += Layout.field("CPU", cpuLine(i.usage(), metrics, labelled = false))
        lines += Layout.field("RAM", ramLine(i.usage(), metrics, labelled = false))
    }
    if (!metrics) lines += Layout.field("Usage", Style.bad("unavailable (metrics API not answering)"))
    markers(i).takeIf { it.isNotEmpty() }?.let { lines += Layout.field("Marked", it) }
    return lines
}
