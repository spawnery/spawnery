/*
Copyright paul_wtf.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloud.spawnery.agent.velocity

import cloud.spawnery.agent.NetworkMirror
import cloud.spawnery.agent.Routing
import com.velocitypowered.api.proxy.server.RegisteredServer
import java.time.Duration

internal data class FillCandidate(
    val name: String,
    val players: Int,
    val slots: Int,
    val line: Int,
    val stale: Boolean,
    val number: Int,
)

/**
 * The fullest server below its fill line, in the same order as the
 * operator's `chooseFill`, so every proxy and the operator agree. A
 * [FillCandidate.slots] of 0 is a server that has not reported: room and line
 * without limit.
 */
internal fun chooseFill(candidates: List<FillCandidate>): String? = candidates.minWithOrNull(fillOrder)?.name

private class FillKey(c: FillCandidate) {
    private val players = c.players.toLong()
    private val slots = if (c.slots == 0) Long.MAX_VALUE else c.slots.toLong()
    private val line = if (c.slots == 0) Long.MAX_VALUE else c.line.toLong()
    val room = players < slots
    val belowLine = players < line
    val free = slots - players
    val number = if (c.number == 0) Long.MAX_VALUE else c.number.toLong()
}

private val fillOrder = Comparator<FillCandidate> { a, b ->
    val ka = FillKey(a)
    val kb = FillKey(b)
    when {
        ka.room != kb.room -> if (ka.room) -1 else 1
        a.stale != b.stale -> if (!a.stale) -1 else 1
        ka.belowLine != kb.belowLine -> if (ka.belowLine) -1 else 1
        ka.belowLine && a.players != b.players -> b.players.compareTo(a.players)
        !ka.belowLine && ka.free != kb.free -> kb.free.compareTo(ka.free)
        ka.number != kb.number -> ka.number.compareTo(kb.number)
        else -> a.name.compareTo(b.name)
    }
}

/**
 * The players this proxy has sent to each server that the mirror's count may
 * not include yet. An entry counts until a state arrives [margin] after the
 * send, or until [maxAge] passes without one.
 * [clock] and [stateAt] are [System.nanoTime] readings.
 */
class SentTally(
    private val clock: () -> Long,
    private val stateAt: () -> Long,
    margin: Duration = Duration.ofSeconds(10),
    maxAge: Duration = Duration.ofMinutes(2),
) {
    private val marginNanos = margin.toNanos()
    private val maxAgeNanos = maxAge.toNanos()

    private val sends = HashMap<String, ArrayDeque<Long>>()

    @Synchronized
    fun sent(server: String) {
        expire()
        sends.getOrPut(server.lowercase()) { ArrayDeque() }.addLast(clock())
    }

    @Synchronized
    fun pending(server: String): Int {
        expire()
        return sends[server.lowercase()]?.size ?: 0
    }

    /**
     * Runs [choose] against the tally and records its answer, under one lock,
     * so two threads choosing at once cannot both take a server's last seat.
     */
    @Synchronized
    internal fun reserve(choose: (pending: (String) -> Int) -> String?): String? {
        expire()
        val server = choose { sends[it.lowercase()]?.size ?: 0 } ?: return null
        sent(server)
        return server
    }

    private fun expire() {
        val now = clock()
        val state = stateAt()
        val each = sends.values.iterator()
        while (each.hasNext()) {
            val times = each.next()
            while (times.isNotEmpty() && !counts(times.first(), now, state)) times.removeFirst()
            if (times.isEmpty()) each.remove()
        }
    }

    private fun counts(sentAt: Long, now: Long, state: Long): Boolean =
        state - sentAt < marginNanos && now - sentAt < maxAgeNanos
}

class FillSource(val mirror: NetworkMirror, val tally: SentTally) {
    internal fun fills(group: String): Boolean = mirror.routing(group) == Routing.FILL

    internal fun choose(candidates: List<RegisteredServer>): RegisteredServer? {
        val chosen = tally.reserve { pending ->
            chooseFill(
                candidates.map { server ->
                    val name = server.serverInfo.name
                    val view = mirror.fillView(name)
                    FillCandidate(
                        name = name,
                        players = maxOf((view?.players ?: 0) + pending(name), server.playersConnected.size),
                        slots = view?.slots ?: 0,
                        line = view?.line ?: 0,
                        stale = view?.stale ?: false,
                        number = view?.number ?: 0,
                    )
                },
            )
        }
        return candidates.firstOrNull { it.serverInfo.name == chosen }
    }
}
