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
import cloud.spawnery.agent.pb.GroupState
import cloud.spawnery.agent.pb.NetworkState
import cloud.spawnery.agent.pb.ServerState
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import java.time.Duration

class FillTest {
    private fun c(name: String, players: Int, slots: Int, line: Int, stale: Boolean, number: Int) =
        FillCandidate(name, players, slots, line, stale, number)

    @Test
    fun `chooseFill follows the shared table`() {
        val cases = mapOf(
            "A" to (listOf(c("a", 5, 25, 20, false, 1), c("b", 0, 25, 20, false, 2)) to "a"),
            "B" to (listOf(c("a", 20, 25, 20, false, 1), c("b", 3, 25, 20, false, 2)) to "b"),
            "C" to (listOf(c("a", 20, 25, 20, false, 1), c("b", 22, 25, 20, false, 2)) to "a"),
            "D" to (listOf(c("a", 4, 25, 20, false, 1), c("b", 4, 25, 20, false, 2)) to "a"),
            "E" to (listOf(c("a", 4, 25, 20, false, 0), c("b", 4, 25, 20, false, 2)) to "b"),
            "F" to (listOf(c("a", 10, 25, 20, true, 1), c("b", 2, 25, 20, false, 2)) to "b"),
            "G" to (
                listOf(c("a", 10, 25, 20, false, 1), c("b", 25, 25, 20, false, 2), c("c", 3, 25, 20, true, 3)) to "a"
                ),
            "H" to (listOf(c("a", 25, 25, 20, false, 1), c("c", 3, 25, 20, true, 3)) to "c"),
            "I" to (listOf(c("a", 0, 0, 0, false, 1), c("b", 3, 25, 20, false, 2)) to "b"),
        )
        for ((case, input) in cases) {
            val (candidates, winner) = input
            assertEquals(winner, chooseFill(candidates), "case $case")
            assertEquals(winner, chooseFill(candidates.reversed()), "case $case, reversed")
        }
        assertNull(chooseFill(emptyList()))
    }

    @Test
    fun `a sent player counts until a state arrives a margin later`() {
        var now = 1_000L
        var stateAt = 900L
        val tally = SentTally({ now }, { stateAt }, margin = Duration.ofNanos(10), maxAge = Duration.ofNanos(1_000))

        tally.sent("lobby-1")
        assertEquals(1, tally.pending("lobby-1"))
        assertEquals(1, tally.pending("LOBBY-1"), "server names compare case-insensitively")
        assertEquals(0, tally.pending("lobby-2"))

        now = 1_050L
        stateAt = 1_009L
        assertEquals(1, tally.pending("lobby-1"), "a state less than a margin after the send may not count the player yet")

        stateAt = 1_010L
        assertEquals(0, tally.pending("lobby-1"), "a state a margin after the send counts the player itself")
    }

    @Test
    fun `a sent player stops counting after maxAge without any state`() {
        var now = 1_000L
        val tally = SentTally({ now }, { 0L }, margin = Duration.ofNanos(10), maxAge = Duration.ofNanos(1_000))

        tally.sent("lobby-1")
        now = 1_999L
        assertEquals(1, tally.pending("lobby-1"))

        now = 2_000L
        assertEquals(0, tally.pending("lobby-1"))
    }
}

/** One server as the mirror reports it; [line] 0 leaves playable slots unset. */
internal data class Reported(
    val name: String,
    val players: Int,
    val slots: Int,
    val line: Int = 0,
    val number: Int = 0,
    val stale: Boolean = false,
)

internal fun mirrorOf(fillGroups: Set<String>, vararg servers: Reported): NetworkMirror {
    val state = NetworkState.newBuilder()
    for (group in fillGroups) {
        state.addGroups(GroupState.newBuilder().setName(group).setRoutingStrategy(GroupState.RoutingStrategy.ROUTING_FILL))
    }
    for (s in servers) {
        state.addServers(
            ServerState.newBuilder()
                .setName(s.name)
                .setPlayers(s.players)
                .setSlots(s.slots)
                .setPlayableSlots(s.line)
                .setNumber(s.number)
                .setStale(s.stale),
        )
    }
    return NetworkMirror { 1L }.apply { apply(state.build()) }
}

/** A tally whose clock never moves, so nothing it records expires during a test. */
internal fun frozenTally(mirror: NetworkMirror): SentTally = SentTally({ 1L }, mirror::appliedAtNanos)
